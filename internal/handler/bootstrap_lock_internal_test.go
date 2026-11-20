package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/uid"
)

// The first-user decision must be serialised on PostgreSQL. A transaction that holds
// the bootstrap lock and has inserted (but not yet committed) a first user stands in
// for a concurrent Setup or Claim; the handler under test must wait for it and then
// refuse, not count zero users and insert a second administrator alongside it.
func TestBootstrapIsSerialisedOnPostgres(t *testing.T) {
	cases := []struct {
		name string
		call func(h *Handler, w http.ResponseWriter, r *http.Request)
		path string
		body string
	}{
		{"claim", (*Handler).Claim, "/v1/auth/claim",
			`{"name":"Second","email":"second@example.com","password":"correct horse battery staple"}`},
		{"setup", (*Handler).Setup, "/v1/setup",
			`{"name":"Second","email":"second@example.com","timezone":"UTC"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := dbtest.RequirePostgres(t)
			t.Cleanup(func() { database.Close() })
			h := New(database, slog.Default())
			ctx := context.Background()

			tx, err := database.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback() //nolint:errcheck
			if err := lockBootstrap(ctx, tx, h.ws.ID); err != nil {
				t.Fatalf("lock: %v", err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO users (id, email, name, iana_timezone, is_admin, is_owner) VALUES (?, ?, ?, 'UTC', 1, 1)`,
				uid.New(), "first@example.com", "First"); err != nil {
				t.Fatalf("insert first user: %v", err)
			}

			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				tc.call(h, rec, req)
			}()

			// Give the handler ample time to reach its user count. With the lock it is
			// still waiting here; without it, it has already counted zero and inserted.
			select {
			case <-done:
			case <-time.After(1500 * time.Millisecond):
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("handler did not finish after the competing transaction committed")
			}

			if rec.Code != http.StatusConflict {
				t.Errorf("got %d (%s); want 409 once another first user has committed", rec.Code, rec.Body.String())
			}
			var n int
			if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != 1 {
				t.Errorf("users = %d; want exactly 1", n)
			}
		})
	}
}
