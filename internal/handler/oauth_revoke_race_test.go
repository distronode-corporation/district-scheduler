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

	"github.com/calnode/calnode/internal/dbtest"
)

// "Sign out everywhere" that overlaps an authorization-code exchange must not leave the
// exchange's token behind. The test holds the code's row, queues the exchange on it and
// then the revocation behind the exchange, and releases the row: the exchange redeems the
// code first, so the revocation has to remove the token it issued. Interleaving needs a
// pool with more than one connection, so this runs on PostgreSQL only.
func TestRevokeAllSessions_removesATokenIssuedByAnOverlappingExchange(t *testing.T) {
	if dbtest.PostgresDSN() == "" {
		t.Skipf("%s is not set; this interleaving needs PostgreSQL", dbtest.DSNEnv)
	}
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)
	const verifier = "race-verifier-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	now := time.Now().UTC()
	if _, err := database.Exec(`
		INSERT INTO oauth_auth_codes (code_hash, client_id, user_id, redirect_uri, code_challenge, scope, resource, expires_at, created_at)
		VALUES (?, 'client-x', ?, 'https://app.example.test/cb', ?, 'mcp', '', ?, ?)`,
		sha256HexForTest("code-x"), ownerID, challenge,
		now.Add(5*time.Minute).Format(time.RFC3339), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed code: %v", err)
	}

	ctx := context.Background()
	hold, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer hold.Rollback() //nolint:errcheck
	var held string
	if err := hold.QueryRowContext(ctx,
		`SELECT code_hash FROM oauth_auth_codes WHERE code_hash = ? FOR UPDATE`, sha256HexForTest("code-x")).Scan(&held); err != nil {
		t.Fatalf("hold the code: %v", err)
	}

	exchanged := make(chan struct{})
	go func() {
		defer close(exchanged)
		form := url.Values{
			"grant_type": {"authorization_code"}, "code": {"code-x"}, "client_id": {"client-x"},
			"redirect_uri": {"https://app.example.test/cb"}, "code_verifier": {verifier},
		}
		req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.TokenMCP(httptest.NewRecorder(), req)
	}()
	waitForBlockedWrite(t, database, exchanged, `DELETE FROM oauth_auth_codes WHERE code_hash = `)

	revoked := make(chan struct{})
	var revokeStatus int
	go func() {
		defer close(revoked)
		revokeStatus = revokeAll(h, "", ownerKey, "").Code
	}()
	waitForBlockedWrite(t, database, revoked, `DELETE FROM oauth_auth_codes WHERE user_id = `)

	if err := hold.Rollback(); err != nil {
		t.Fatalf("release the code: %v", err)
	}
	<-exchanged
	<-revoked

	if revokeStatus != http.StatusOK {
		t.Fatalf("revoke status %d; want 200", revokeStatus)
	}
	if n := countMCPTokens(t, database, ownerID); n != 0 {
		t.Errorf("mcp tokens after the revocation = %d; want 0: the overlapping exchange's token survived it", n)
	}
}
