package handler_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/handler"
	"github.com/modelcontextprotocol/go-sdk/auth"
)

func TestArchiveUser_archivesAndBlocksLogin(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	memberKey := "member-archive-key"
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','m@example.com','Member','UTC',0)`)
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k2','u2','t',?,'2024-01-01')`, sha256HexForTest(memberKey))
	database.Exec(`INSERT INTO event_types (id,user_id,slug,name,duration_minutes,is_active) VALUES ('et1','u2','et-slug','E',30,1)`)

	// Archive u2.
	req := authReq(http.MethodPost, "/v1/users/u2/archive", "", ownerKey)
	req.SetPathValue("id", "u2")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ArchiveUser)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive: got %d — %s", rec.Code, rec.Body.String())
	}

	// archived_at set; event types deactivated.
	var archivedAt *string
	database.QueryRow(`SELECT archived_at FROM users WHERE id='u2'`).Scan(&archivedAt)
	if archivedAt == nil {
		t.Error("archived_at should be set")
	}
	var active int
	database.QueryRow(`SELECT is_active FROM event_types WHERE id='et1'`).Scan(&active)
	if active != 0 {
		t.Error("archived member's event types should be deactivated")
	}

	// The archived member's API key no longer authenticates.
	req = authReq(http.MethodGet, "/v1/users/me", "", memberKey)
	rec = httptest.NewRecorder()
	h.RequireAuth(h.GetMe)(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("archived member auth: got %d; want 401", rec.Code)
	}
}

func TestArchiveUser_hiddenFromDefaultListShownWithFlag(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,archived_at) VALUES ('u2','m@example.com','Member','UTC',0,'2026-01-01T00:00:00Z')`)

	// Default list excludes archived.
	req := authReq(http.MethodGet, "/v1/users", "", ownerKey)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ListUsers)(rec, req)
	if rec.Body.String() == "" || rec.Code != http.StatusOK {
		t.Fatalf("list: got %d — %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); strings.Contains(got, "u2") {
		t.Error("archived member should be hidden from default list")
	}

	// With include_archived=true they appear.
	req = authReq(http.MethodGet, "/v1/users?include_archived=true", "", ownerKey)
	rec = httptest.NewRecorder()
	h.RequireAuth(h.ListUsers)(rec, req)
	if got := rec.Body.String(); !strings.Contains(got, "u2") {
		t.Error("archived member should appear with include_archived=true")
	}
}

func TestArchiveUser_blockedByUpcomingBookings(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','m@example.com','Member','UTC',0)`)
	database.Exec(`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('et1','u2','et-slug','E',30)`)
	database.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b1','et1','u2','2099-01-01T10:00:00Z','2099-01-01T10:30:00Z','confirmed')`)

	req := authReq(http.MethodPost, "/v1/users/u2/archive", "", ownerKey)
	req.SetPathValue("id", "u2")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ArchiveUser)(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("got %d; want 409 (upcoming bookings) — %s", rec.Code, rec.Body.String())
	}
}

func TestArchiveUser_cannotArchiveOwner(t *testing.T) {
	h, _, ownerKey, ownerID := setupWorkspaceWithDB(t)
	req := authReq(http.MethodPost, "/v1/users/"+ownerID+"/archive", "", ownerKey)
	req.SetPathValue("id", ownerID)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ArchiveUser)(rec, req)
	// Owner archiving self → self guard (400). Also owner-guard covers others.
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d; want 400", rec.Code)
	}
}

func TestRestoreUser_adminOnlyRestoresOwnArchives(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	adminAKey := "adminA-restore-key"
	adminBKey := "adminB-restore-key"
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,is_owner) VALUES ('a','aa@example.com','AdminA','UTC',1,0)`)
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('ka','a','t',?,'2024-01-01')`, sha256HexForTest(adminAKey))
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,is_owner) VALUES ('b','bb@example.com','AdminB','UTC',1,0)`)
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('kb','b','t',?,'2024-01-01')`, sha256HexForTest(adminBKey))
	// Member archived by AdminA.
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,archived_at,archived_by) VALUES ('m','m@example.com','Member','UTC',0,'2026-01-01T00:00:00Z','a')`)

	// AdminB cannot restore AdminA's archive.
	req := authReq(http.MethodPost, "/v1/users/m/restore", "", adminBKey)
	req.SetPathValue("id", "m")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.RestoreUser)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("adminB restoring adminA's archive: got %d; want 403 — %s", rec.Code, rec.Body.String())
	}

	// AdminA (the archiver) can.
	req = authReq(http.MethodPost, "/v1/users/m/restore", "", adminAKey)
	req.SetPathValue("id", "m")
	rec = httptest.NewRecorder()
	h.RequireAuth(h.RestoreUser)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("adminA restoring own archive: got %d; want 200 — %s", rec.Code, rec.Body.String())
	}

	// Re-archive (by AdminA) and confirm the owner can always restore.
	database.Exec(`UPDATE users SET archived_at='2026-01-01T00:00:00Z', archived_by='a' WHERE id='m'`)
	req = authReq(http.MethodPost, "/v1/users/m/restore", "", ownerKey)
	req.SetPathValue("id", "m")
	rec = httptest.NewRecorder()
	h.RequireAuth(h.RestoreUser)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner restoring adminA's archive: got %d; want 200 — %s", rec.Code, rec.Body.String())
	}
}

func TestRestoreUser_reenablesLogin(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	memberKey := "member-restore-key"
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,archived_at) VALUES ('u2','m@example.com','Member','UTC',0,'2026-01-01T00:00:00Z')`)
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k2','u2','t',?,'2024-01-01')`, sha256HexForTest(memberKey))

	// Archived member can't auth yet.
	req := authReq(http.MethodGet, "/v1/users/me", "", memberKey)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.GetMe)(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("pre-restore auth: got %d; want 401", rec.Code)
	}

	// Restore.
	req = authReq(http.MethodPost, "/v1/users/u2/restore", "", ownerKey)
	req.SetPathValue("id", "u2")
	rec = httptest.NewRecorder()
	h.RequireAuth(h.RestoreUser)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: got %d — %s", rec.Code, rec.Body.String())
	}

	// Now the member can auth again.
	req = authReq(http.MethodGet, "/v1/users/me", "", memberKey)
	rec = httptest.NewRecorder()
	h.RequireAuth(h.GetMe)(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("post-restore auth: got %d; want 200", rec.Code)
	}
}

// seedMemberCredentials gives userID one of every credential archive has to consider:
// a browser session, an MCP OAuth token row (access + refresh), a pending OAuth
// authorization code, and an API key. The raw values embed userID so two members'
// credentials never collide. Everything is inserted directly, unexpired.
func seedMemberCredentials(t *testing.T, database *sql.DB, userID string) (session, access, refresh, apiKey string) {
	t.Helper()
	session, access, refresh, apiKey = "sess-"+userID, "mcat_"+userID, "mcrt_"+userID, "cno_"+userID
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO sessions (id, user_id, expires_at) VALUES (?, ?, ?)`, []any{session, userID, future}},
		{`INSERT INTO oauth_access_tokens (id, token_hash, refresh_hash, client_id, user_id, expires_at, created_at)
		  VALUES (?, ?, ?, 'client-1', ?, ?, '2026-01-01T00:00:00Z')`,
			[]any{"tok-" + userID, sha256HexForTest(access), sha256HexForTest(refresh), userID, future}},
		{`INSERT INTO oauth_auth_codes (code_hash, client_id, user_id, redirect_uri, code_challenge, expires_at, created_at)
		  VALUES (?, 'client-1', ?, 'https://client.example/cb', 'challenge', ?, '2026-01-01T00:00:00Z')`,
			[]any{sha256HexForTest("mcac_" + userID), userID, future}},
		{`INSERT INTO api_keys (id, user_id, name, key_hash, created_at) VALUES (?, ?, 't', ?, '2024-01-01')`,
			[]any{"key-" + userID, userID, sha256HexForTest(apiKey)}},
	} {
		if _, err := database.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("seed credentials for %s: %v", userID, err)
		}
	}
	return session, access, refresh, apiKey
}

// credentialCounts reports how many sessions, OAuth token rows, OAuth codes and API keys
// userID holds.
func credentialCounts(t *testing.T, database *sql.DB, userID string) [4]int {
	t.Helper()
	var c [4]int
	for i, table := range []string{"sessions", "oauth_access_tokens", "oauth_auth_codes", "api_keys"} {
		if err := database.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE user_id = ?`, userID).Scan(&c[i]); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
	}
	return c
}

func archiveOrRestore(t *testing.T, h *handler.Handler, ownerKey, action, userID string) {
	t.Helper()
	req := authReq(http.MethodPost, "/v1/users/"+userID+"/"+action, "", ownerKey)
	req.SetPathValue("id", userID)
	rec := httptest.NewRecorder()
	fn := h.ArchiveUser
	if action == "restore" {
		fn = h.RestoreUser
	}
	h.RequireAuth(fn)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s: got %d — %s", action, userID, rec.Code, rec.Body.String())
	}
}

// An archived member's MCP OAuth bearer must be refused even if the token row is still
// there, as their API key already is. The token is flipped by setting archived_at
// directly, NOT through ArchiveUser, so this holds the check in VerifyMCPBearer on its
// own and cannot pass because archive happened to delete the row.
func TestVerifyMCPBearer_refusesArchivedMembersOAuthToken(t *testing.T) {
	h, database, _, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','m@example.com','Member','UTC',0)`)
	_, access, _, _ := seedMemberCredentials(t, database, "u2")
	ctx := context.Background()

	// Control: the seeded token is valid, so a refusal below is the archive, not the seed.
	if info, err := h.VerifyMCPBearer(ctx, access, nil); err != nil || info.UserID != "u2" {
		t.Fatalf("active member's OAuth token: got %+v, %v; want it accepted as u2", info, err)
	}

	database.Exec(`UPDATE users SET archived_at = '2026-01-01T00:00:00Z' WHERE id = 'u2'`)
	if info, err := h.VerifyMCPBearer(ctx, access, nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("archived member's unexpired OAuth token: got %+v, %v; want ErrInvalidToken", info, err)
	}
	if n := credentialCounts(t, database, "u2")[1]; n != 1 {
		t.Fatalf("test setup: the token row should still exist (%d rows), or this is not testing the check", n)
	}
}

// Archive ends the member's sessions, OAuth tokens and OAuth codes, and only theirs.
func TestArchiveUser_endsSessionsAndAgentAccess(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','m@example.com','Member','UTC',0)`)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','n@example.com','Bystander','UTC',0)`)
	seedMemberCredentials(t, database, "u2")
	_, bystanderAccess, _, _ := seedMemberCredentials(t, database, "u3")

	archiveOrRestore(t, h, ownerKey, "archive", "u2")

	if got, want := credentialCounts(t, database, "u2"), [4]int{0, 0, 0, 1}; got != want {
		t.Errorf("archived member's [sessions, oauth tokens, oauth codes, api keys] = %v; want %v", got, want)
	}
	if got, want := credentialCounts(t, database, "u3"), [4]int{1, 1, 1, 1}; got != want {
		t.Errorf("another member's credentials = %v after archiving u2; want %v untouched", got, want)
	}
	if info, err := h.VerifyMCPBearer(context.Background(), bystanderAccess, nil); err != nil || info.UserID != "u3" {
		t.Errorf("another member's OAuth token after archiving u2: got %+v, %v; want it still accepted", info, err)
	}
}

// Restore brings back the member and their API keys, and nothing archive ended: not a
// signed-in browser, not an agent's access token, and not its refresh token.
func TestRestoreUser_doesNotResurrectSessionsOrAgentAccess(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','m@example.com','Member','UTC',0)`)
	session, access, refresh, apiKey := seedMemberCredentials(t, database, "u2")

	archiveOrRestore(t, h, ownerKey, "archive", "u2")
	archiveOrRestore(t, h, ownerKey, "restore", "u2")

	if got, want := credentialCounts(t, database, "u2"), [4]int{0, 0, 0, 1}; got != want {
		t.Errorf("restored member's [sessions, oauth tokens, oauth codes, api keys] = %v; want %v", got, want)
	}

	if info, err := h.VerifyMCPBearer(context.Background(), access, nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("pre-archive OAuth access token after restore: got %+v, %v; want ErrInvalidToken", info, err)
	}

	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {"client-1"}}
	tokReq := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokRec := httptest.NewRecorder()
	h.TokenMCP(tokRec, tokReq)
	if tokRec.Code != http.StatusBadRequest || !strings.Contains(tokRec.Body.String(), "invalid_grant") {
		t.Errorf("pre-archive refresh token after restore: got %d — %s; want 400 invalid_grant", tokRec.Code, tokRec.Body.String())
	}

	sessReq := httptest.NewRequest(http.MethodGet, "/v1/users/me", nil)
	sessReq.AddCookie(&http.Cookie{Name: "calnode_session", Value: session})
	sessRec := httptest.NewRecorder()
	h.RequireAuth(h.GetMe)(sessRec, sessReq)
	if sessRec.Code != http.StatusUnauthorized {
		t.Errorf("pre-archive session cookie after restore: got %d; want 401", sessRec.Code)
	}

	keyRec := httptest.NewRecorder()
	h.RequireAuth(h.GetMe)(keyRec, authReq(http.MethodGet, "/v1/users/me", "", apiKey))
	if keyRec.Code != http.StatusOK {
		t.Errorf("API key after restore: got %d; want 200 (archive keeps API keys) — %s", keyRec.Code, keyRec.Body.String())
	}
}
