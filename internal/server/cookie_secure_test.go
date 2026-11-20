package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/config"
	"github.com/calnode/calnode/internal/dbtest"
)

// COOKIE_SECURE applies to the session cookie whatever sign-in providers are
// configured. With neither Google nor Microsoft set up, the first-user claim (a
// password sign-in) used to issue its session cookie without Secure.
func TestSessionCookieSecureWithoutOAuthProviders(t *testing.T) {
	for _, secure := range []bool{true, false} {
		database := dbtest.Open(t)
		t.Cleanup(func() { database.Close() })
		ctx, cancel := context.WithCancel(context.Background())
		h, drain := BuildHandler(ctx, &config.Config{
			BaseURL:      "https://calnode.localhost",
			CookieSecure: secure,
		}, database, slog.New(slog.DiscardHandler))

		req := httptest.NewRequest(http.MethodPost, "/v1/auth/claim", strings.NewReader(
			`{"name":"First","email":"first@example.com","password":"correct horse battery staple"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.Claim(rec, req)
		cancel()
		drain()
		if rec.Code != http.StatusCreated {
			t.Fatalf("claim: %d — %s", rec.Code, rec.Body.String())
		}
		var found bool
		for _, c := range rec.Result().Cookies() {
			if c.Name == "calnode_session" {
				found = true
				if c.Secure != secure {
					t.Errorf("CookieSecure=%v: session cookie Secure = %v", secure, c.Secure)
				}
			}
		}
		if !found {
			t.Fatalf("CookieSecure=%v: no session cookie issued", secure)
		}
	}
}
