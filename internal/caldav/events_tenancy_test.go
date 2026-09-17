package caldav

import (
	"context"
	"testing"

	"github.com/calnode/calnode/internal/dbtest"
)

// eventConn chooses whose app password an update or cancel sends, from calendar_connections
// and connection_calendars, which are tenant tables. Like loadConn it names no workspace_id:
// the provider reads through the handle calendar.Service.ForDB bound to the request's
// workspace, and row-level security is what confines it. This pins that for the new reads.
//
// Workspace globex's host has a CalDAV account holding an event. A client bound to acme,
// asked to cancel or move that event for globex's user, must find no account and send nothing;
// the same call bound to globex reaches the server as globex's account. Postgres only: SQLite
// has no row-level security to exercise.
func TestEventConn_boundToItsWorkspace(t *testing.T) {
	app, platform := dbtest.RequireTenantPair(t)
	ctx := context.Background()

	for _, ws := range []string{"acme", "globex"} {
		if _, err := platform.ExecContext(ctx,
			`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', 'active')`,
			ws, ws, ws+".example.com"); err != nil {
			t.Fatalf("workspace %s: %v", ws, err)
		}
	}

	base, err := New(app, testKeyHex)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	base.hc.Transport = nil // a local httptest server; see newTestClient
	acme := base.ForDB(app.ForWorkspace("acme")).(*Client)
	globex := base.ForDB(app.ForWorkspace("globex")).(*Client)

	if _, err := globex.db.ExecContext(ctx,
		`INSERT INTO users (id, email, name) VALUES ('globex-host', 'host@globex.test', 'Globex Host')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	srv := newDAVServer(t, "g@globex.test", "pw-globex")
	collection := srv.URL + "/calendars/g/home/"
	if err := globex.saveConnection(ctx, "globex-host", "g@globex.test", "pw-globex", collection); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	eventID := collection + "ev1.ics"

	// Positive control on the data: the row exists, so an empty result below is the policy.
	var n int
	if err := platform.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM calendar_connections WHERE workspace_id = 'globex' AND provider = 'caldav'`).Scan(&n); err != nil {
		t.Fatalf("count connections: %v", err)
	}
	if n != 1 {
		t.Fatalf("fixture left %d globex CalDAV connections; want 1", n)
	}

	if err := acme.CancelEvent(ctx, "globex-host", collection, eventID); err != nil {
		t.Errorf("CancelEvent bound to acme = %v, want nil (no CalDAV account visible to act as)", err)
	}
	if err := acme.UpdateEvent(ctx, "globex-host", collection, eventID, moveStart, moveEnd); err != nil {
		t.Errorf("UpdateEvent bound to acme = %v, want nil (no CalDAV account visible to act as)", err)
	}
	srv.noRequests(t, "globex's server, from acme")

	// Positive control on the path: bound to its own workspace the same call does reach the
	// server, as that account, so the silence above is not a broken fixture.
	if err := globex.CancelEvent(ctx, "globex-host", collection, eventID); err != nil {
		t.Errorf("CancelEvent bound to globex: %v", err)
	}
	srv.onlyOwnCredentials(t, "globex's server")
	if got := srv.requests(); len(got) != 1 || got[0].Method != "DELETE" {
		t.Errorf("globex's server saw %+v, want one DELETE", got)
	}
}
