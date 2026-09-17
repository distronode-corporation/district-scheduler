package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestTenancy_archivedMemberLosesMCPBearerAndRefresh is upstream #49 on the fork's own
// path: the multi-tenant shape, through the real mux, on a real OpenPair.
//
// Upstream's TestMCP_OAuthBearerRejectedWhenArchived drives the handler directly on one
// handle. Here the two reads it fixes run where they actually run in production:
//
//   - VerifyMCPBearer on the PLATFORM handle, because /mcp is on the identity host and a
//     bearer has to resolve before any workspace is known;
//   - the refresh grant on POST /oauth/token, which is wrapped in Platform, so its h.db is
//     the platform handle too.
//
// Neither has a row-level-security policy behind it, so the archived_at predicate in the
// statement is the whole of the check. Workspace B's member is left live throughout, which
// is what shows the join did not reach across the boundary in the other direction.
func TestTenancy_archivedMemberLosesMCPBearerAndRefresh(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	const identityHost = "app.calnode.example"

	hash := func(raw string) string {
		sum := sha256.Sum256([]byte(raw))
		return hex.EncodeToString(sum[:])
	}
	// Written through each tenant's BOUND handle with no workspace_id named, which is the
	// shape a real grant has (tenancy_sweep_test.go pins that it lands in its owner's
	// workspace).
	seedGrant := func(workspace, userID, access, refresh string) {
		t.Helper()
		now := time.Now().UTC()
		if _, err := f.app.ForWorkspace(workspace).ExecContext(ctx, `
			INSERT INTO oauth_access_tokens (id, token_hash, refresh_hash, client_id, user_id, expires_at, created_at)
			VALUES (?, ?, ?, 'client-archive-test', ?, ?, ?)`,
			workspace+"-grant", hash(access), hash(refresh), userID,
			now.Add(time.Hour).Format(time.RFC3339), now.Format(time.RFC3339Nano)); err != nil {
			t.Fatalf("seed grant for %s: %v", workspace, err)
		}
	}
	seedGrant(f.a.id, f.a.userID, "mcat_acme_archive", "mcrt_acme_archive")
	seedGrant(f.b.id, f.b.userID, "mcat_globex_archive", "mcrt_globex_archive")

	// An MCP initialize through the real /mcp mount. The bearer guard runs first, so a
	// refused token is a 401 whatever the body says, and an accepted one is answered 200
	// by the MCP server behind it: asserting the 200 rather than "not 401" is what stops a
	// broken fixture (a 400 from a malformed initialize) reading as an accepted bearer.
	mcpStatus := func(t *testing.T, bearer string) int {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
			`"protocolVersion":"2025-06-18","capabilities":{},` +
			`"clientInfo":{"name":"archive-test","version":"0"}}}`
		r := newRequest(t, http.MethodPost, identityHost, "/mcp", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Authorization", "Bearer "+bearer)
		return f.serve(r).Code
	}
	refresh := func(t *testing.T, raw string) *httptest.ResponseRecorder {
		t.Helper()
		return f.postForm(t, identityHost, "/oauth/token", url.Values{
			"grant_type": {"refresh_token"}, "refresh_token": {raw},
		})
	}

	// Before archiving: A's bearer authenticates and its refresh rotates. Keep the
	// ROTATED pair, which is the agent's current credential; asserting on the original
	// values after the archive would pass vacuously, because rotation already replaced
	// them.
	if code := mcpStatus(t, "mcat_acme_archive"); code != http.StatusOK {
		t.Fatalf("A's bearer before archive: %d; want 200", code)
	}
	rec := refresh(t, "mcrt_acme_archive")
	if rec.Code != http.StatusOK {
		t.Fatalf("A's refresh before archive: %d; want 200 - %s", rec.Code, rec.Body.String())
	}
	var rotated struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil || rotated.AccessToken == "" || rotated.RefreshToken == "" {
		t.Fatalf("rotated pair: %v - %s", err, rec.Body.String())
	}
	if code := mcpStatus(t, rotated.AccessToken); code != http.StatusOK {
		t.Fatalf("A's rotated bearer before archive: %d; want 200", code)
	}

	// Archive A's member through A's own bound handle: the row a workspace admin's
	// archive writes.
	if _, err := f.app.ForWorkspace(f.a.id).ExecContext(ctx,
		`UPDATE users SET archived_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), f.a.userID); err != nil {
		t.Fatalf("archive A's member: %v", err)
	}

	if code := mcpStatus(t, rotated.AccessToken); code != http.StatusUnauthorized {
		t.Errorf("A's bearer after archive: %d; want 401 - an archived member's agent must not keep its access", code)
	}
	if rec := refresh(t, rotated.RefreshToken); rec.Code == http.StatusOK {
		t.Errorf("A's refresh after archive issued tokens to an archived member: %s", rec.Body.String())
	}

	// Upstream deliberately deletes nothing on archive: the grant row is still there,
	// it just no longer authenticates.
	var rows int
	if err := f.plat.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM oauth_access_tokens WHERE user_id = ?`, f.a.userID).Scan(&rows); err != nil {
		t.Fatalf("count A's grants: %v", err)
	}
	if rows != 1 {
		t.Errorf("A's grant rows after archive = %d; want 1 (archive refuses, it does not delete)", rows)
	}

	// B was never archived, and nothing about A's archive may reach B's credentials.
	if code := mcpStatus(t, "mcat_globex_archive"); code != http.StatusOK {
		t.Errorf("B's bearer after A's archive: %d; want 200", code)
	}
	if rec := refresh(t, "mcrt_globex_archive"); rec.Code != http.StatusOK {
		t.Errorf("B's refresh after A's archive: %d; want 200 - %s", rec.Code, rec.Body.String())
	}
}
