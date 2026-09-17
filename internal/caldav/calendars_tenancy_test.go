package caldav

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/dbtest"
)

// Listing a CalDAV account's calendars now loads its stored app password and talks to its
// server, on every picker load and every saved selection. The read is calendar_connections,
// a tenant table, through the workspace-bound handle, like every other CalDAV read. This pins
// that a client bound to one workspace cannot list, or validate a selection against, another
// workspace's account, and so never sends that account's password anywhere. Postgres only:
// SQLite has no row-level security to exercise.
func TestListCalendars_boundToItsWorkspace(t *testing.T) {
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
	fs := newMultiCalServer(t)
	if _, _, err := globex.Connect(ctx, "globex-host", fs.srv.URL, multiCalAccount, "app-pw"); err != nil {
		t.Fatalf("Connect bound to globex: %v", err)
	}
	personal := fs.url("/calendars/user/personal/")
	fs.reset()

	// From acme: no account to list, and nothing sent.
	cals, err := acme.ListCalendars(ctx, "globex-host", multiCalAccount)
	if err != nil || cals != nil {
		t.Errorf("ListCalendars bound to acme = (%+v, %v), want (nil, nil): no such account in this workspace", cals, err)
	}
	if err := acme.ValidateSelection(ctx, "globex-host", multiCalAccount, []string{personal}); err == nil {
		t.Error("ValidateSelection bound to acme accepted globex's calendar")
	}
	acmeSvc := calendar.NewService(app.ForWorkspace("acme"))
	acmeSvc.Register(acme)
	if err := acmeSvc.SetAccountCalendars(ctx, "globex-host", "caldav", multiCalAccount,
		[]calendar.CalendarSelection{sel(personal, true, true)}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SetAccountCalendars bound to acme = %v, want sql.ErrNoRows (the handler's 404)", err)
	}
	if p := fs.paths("PROPFIND"); len(p) != 0 {
		t.Errorf("globex's server received PROPFINDs %v from a client bound to acme", p)
	}

	// Positive control: bound to globex, the same account lists its calendars.
	cals, err = globex.ListCalendars(ctx, "globex-host", multiCalAccount)
	if err != nil || len(cals) != 3 || cals[0].ID != personal {
		t.Errorf("ListCalendars bound to globex = (%+v, %v), want the account's three event calendars", cals, err)
	}
}
