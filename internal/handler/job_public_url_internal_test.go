package handler

import (
	"context"
	"log/slog"
	"testing"

	"github.com/calnode/calnode/internal/dbtest"
)

// JobPublicURL is the host a reminder email loads its branding images from (upstream
// a2bdcf1, ported per workspace). A background job carries only a workspace id, so
// on a multi-tenant instance the host has to be read back from the workspaces row;
// the failure that matters is answering with the identity host, whose /branding/
// paths are no tenant's, so every unresolvable case must answer "".
func TestJobPublicURL(t *testing.T) {
	ctx := context.Background()

	t.Run("single-tenant uses PUBLIC_BASE_URL, then BASE_URL", func(t *testing.T) {
		database := dbtest.Open(t)
		t.Cleanup(func() { database.Close() })
		h := New(database, slog.New(slog.DiscardHandler))
		h.SetBaseURL("https://cal.example.test")
		if got := h.JobPublicURL(ctx, "default"); got != "https://cal.example.test" {
			t.Errorf("no PUBLIC_BASE_URL: got %q; want the BASE_URL", got)
		}
		h.SetPublicBaseURL("https://book.example.test")
		if got := h.JobPublicURL(ctx, "default"); got != "https://book.example.test" {
			t.Errorf("with PUBLIC_BASE_URL: got %q; want it", got)
		}
	})

	t.Run("multi-tenant answers the job's own workspace host", func(t *testing.T) {
		database := dbtest.Open(t)
		t.Cleanup(func() { database.Close() })
		h := New(database, slog.New(slog.DiscardHandler))
		h.SetMultiTenant(true)
		h.SetBaseURL("https://app.calnode.example")
		h.SetPublicBaseURL("https://book.example.test")
		for _, ws := range []struct{ id, host string }{
			{"ws-acme", "book.acme.example"},
			{"ws-globex", "book.globex.example"},
		} {
			if _, err := database.Platform().ExecContext(ctx,
				`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', 'active')`,
				ws.id, ws.id, ws.host); err != nil {
				t.Fatalf("workspace %s: %v", ws.id, err)
			}
		}

		for _, tc := range []struct{ workspace, want string }{
			{"ws-acme", "https://book.acme.example"},
			{"ws-globex", "https://book.globex.example"},
			// The seeded default workspace has an empty public_host by design
			// (migration 00060): no host to brand from, and not the identity host.
			{"default", ""},
			// A job whose workspace has since been deleted.
			{"ws-gone", ""},
		} {
			if got := h.JobPublicURL(ctx, tc.workspace); got != tc.want {
				t.Errorf("JobPublicURL(%q) = %q; want %q", tc.workspace, got, tc.want)
			}
		}
	})
}
