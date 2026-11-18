package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func registerTestClient(t *testing.T, h interface {
	RegisterOAuthClient(http.ResponseWriter, *http.Request)
}, redirectURI string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RegisterOAuthClient(rec, httptest.NewRequest(http.MethodPost, "/oauth/register",
		strings.NewReader(`{"client_name":"Client","redirect_uris":["`+redirectURI+`"]}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d — %s", rec.Code, rec.Body.String())
	}
	i := strings.Index(rec.Body.String(), `"client_id":"`)
	if i < 0 {
		t.Fatalf("register: no client_id in %s", rec.Body.String())
	}
	rest := rec.Body.String()[i+len(`"client_id":"`):]
	return rest[:strings.Index(rest, `"`)]
}

// The redirect URI must match a registered one exactly: path and query are
// case-sensitive, so a case variant is a different resource.
func TestMCP_AuthorizeRedirectURIIsExactMatch(t *testing.T) {
	h, database, _, userID := setupWorkspaceWithDB(t)
	h.SetBaseURL("https://sched.example.test")
	database.Exec(`INSERT INTO sessions (id, user_id, expires_at) VALUES ('s-exact', ?, ?)`,
		userID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	clientID := registerTestClient(t, h, "https://app.example.test/oauth/cb")

	for _, tc := range []struct {
		redirect string
		ok       bool
	}{
		{"https://app.example.test/oauth/cb", true},
		{"https://app.example.test/OAUTH/CB", false},
		{"https://app.example.test/oauth/Cb", false},
	} {
		q := url.Values{
			"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {tc.redirect},
			"code_challenge": {"abc"}, "code_challenge_method": {"S256"}, "state": {"s"},
		}
		req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
		req.AddCookie(&http.Cookie{Name: "calnode_session", Value: "s-exact"})
		rec := httptest.NewRecorder()
		h.AuthorizeMCP(rec, req)
		rendered := rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "Allow")
		if rendered != tc.ok {
			t.Errorf("redirect_uri %q: consent rendered = %v (status %d); want %v", tc.redirect, rendered, rec.Code, tc.ok)
		}
	}
}

// A resource indicator naming anything other than this server's MCP endpoint is
// refused at authorization, and a token bound to one is not accepted at /mcp.
func TestMCP_ResourceIndicatorIsBoundToThisServer(t *testing.T) {
	h, database, _, userID := setupWorkspaceWithDB(t)
	h.SetBaseURL("https://sched.example.test")
	database.Exec(`INSERT INTO sessions (id, user_id, expires_at) VALUES ('s-res', ?, ?)`,
		userID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	const redirectURI = "https://app.example.test/oauth/cb"
	clientID := registerTestClient(t, h, redirectURI)

	for _, tc := range []struct {
		resource string
		ok       bool
	}{
		{"", true},
		{"https://sched.example.test/mcp", true},
		{"https://SCHED.example.test/mcp/", true},
		{"https://sched.example.test", true},
		{"https://other.example.test/mcp", false},
		{"http://sched.example.test/mcp", false},
		{"https://sched.example.test/elsewhere", false},
		{"https://sched.example.test/mcp?x=1", false},
	} {
		q := url.Values{
			"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI},
			"code_challenge": {"abc"}, "code_challenge_method": {"S256"}, "state": {"s"},
		}
		if tc.resource != "" {
			q.Set("resource", tc.resource)
		}
		req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
		req.AddCookie(&http.Cookie{Name: "calnode_session", Value: "s-res"})
		rec := httptest.NewRecorder()
		h.AuthorizeMCP(rec, req)
		rendered := rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "Allow")
		if rendered != tc.ok {
			t.Errorf("resource %q: consent rendered = %v (status %d); want %v", tc.resource, rendered, rec.Code, tc.ok)
		}
	}

	now := time.Now().UTC()
	for _, tc := range []struct {
		raw, resource string
		ok            bool
	}{
		{"tok-unbound", "", true},
		{"tok-ours", "https://sched.example.test/mcp", true},
		{"tok-foreign", "https://other.example.test/mcp", false},
	} {
		if _, err := database.Exec(`
			INSERT INTO oauth_access_tokens (id, token_hash, refresh_hash, client_id, user_id, scope, resource, expires_at, created_at)
			VALUES (?, ?, ?, ?, ?, 'mcp', ?, ?, ?)`,
			tc.raw, sha256HexForTest(tc.raw), sha256HexForTest(tc.raw+"-r"), clientID, userID, tc.resource,
			now.Add(time.Hour).Format(time.RFC3339), now.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("seed %s: %v", tc.raw, err)
		}
		_, err := h.VerifyMCPBearer(context.Background(), tc.raw, nil)
		if (err == nil) != tc.ok {
			t.Errorf("%s (resource %q): err = %v; want accepted = %v", tc.raw, tc.resource, err, tc.ok)
		}
	}
}
