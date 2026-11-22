package handler_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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

// The token endpoint applies the same resource rule as authorization: a request naming a
// resource other than this server's MCP endpoint is refused with invalid_target, and the
// refusal does not consume the authorization code.
func TestMCP_TokenRequestResourceIsBoundToThisServer(t *testing.T) {
	h, database, _, userID := setupWorkspaceWithDB(t)
	h.SetBaseURL("https://sched.example.test")
	const verifier = "resource-verifier-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	now := time.Now().UTC()
	if _, err := database.Exec(`
		INSERT INTO oauth_auth_codes (code_hash, client_id, user_id, redirect_uri, code_challenge, scope, resource, expires_at, created_at)
		VALUES (?, 'client-t', ?, 'https://app.example.test/cb', ?, 'mcp', '', ?, ?)`,
		sha256HexForTest("code-t"), userID, challenge,
		now.Add(5*time.Minute).Format(time.RFC3339), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed code: %v", err)
	}
	exchange := func(resource string) *httptest.ResponseRecorder {
		form := url.Values{
			"grant_type": {"authorization_code"}, "code": {"code-t"}, "client_id": {"client-t"},
			"redirect_uri": {"https://app.example.test/cb"}, "code_verifier": {verifier},
		}
		if resource != "" {
			form.Set("resource", resource)
		}
		req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.TokenMCP(rec, req)
		return rec
	}

	rec := exchange("https://other.example.test/mcp")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_target"`) {
		t.Fatalf("foreign resource: status %d, body %s; want 400 invalid_target", rec.Code, rec.Body.String())
	}
	if rec := exchange("https://sched.example.test/mcp"); rec.Code != http.StatusOK {
		t.Errorf("this server's resource after a refusal: status %d, body %s; want 200 (the code was not consumed)",
			rec.Code, rec.Body.String())
	}
}

// The code exchange stores its token through the transaction that consumed the code, and
// takes the token's workspace from that code row. On a pool of one connection (SQLite
// always; Postgres at DB_MAX_OPEN_CONNS=1) any lookup on another handle while the
// transaction is open waits for the connection the exchange itself holds, so before the
// fix this request hung until its deadline and answered 500.
func TestMCP_CodeExchangeOnAOneConnectionPoolDoesNotWaitOnItself(t *testing.T) {
	h, database, _, userID := setupWorkspaceWithDB(t)
	h.SetMultiTenant(true)
	database.SetMaxOpenConns(1)
	const verifier = "pool-verifier-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	now := time.Now().UTC()
	var workspaceID string
	if err := database.QueryRow(`SELECT workspace_id FROM users WHERE id = ?`, userID).Scan(&workspaceID); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO oauth_auth_codes (workspace_id, code_hash, client_id, user_id, redirect_uri, code_challenge, scope, resource, expires_at, created_at)
		VALUES (?, ?, 'client-p', ?, 'https://app.example.test/cb', ?, 'mcp', '', ?, ?)`,
		workspaceID, sha256HexForTest("code-p"), userID, challenge,
		now.Add(5*time.Minute).Format(time.RFC3339), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed code: %v", err)
	}

	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {"code-p"}, "client_id": {"client-p"},
		"redirect_uri": {"https://app.example.test/cb"}, "code_verifier": {verifier},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.TokenMCP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("exchange: status %d, body %s; want 200", rec.Code, rec.Body.String())
	}
	var stored string
	if err := database.QueryRow(`SELECT workspace_id FROM oauth_access_tokens WHERE user_id = ?`, userID).Scan(&stored); err != nil {
		t.Fatalf("read token: %v", err)
	}
	if stored != workspaceID {
		t.Errorf("token workspace %q; want the code's %q", stored, workspaceID)
	}
}
