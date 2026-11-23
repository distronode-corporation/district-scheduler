package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// These two tests interleave a handler with a concurrent transaction by holding a row
// lock on the target: the handler reads the committed state, then its write waits on the
// lock, and the competing transaction commits underneath it. That needs a pool with more
// than one connection, so they run on PostgreSQL only (CI's postgres job).

// An admin removing a member must not remove them if, meanwhile, the member was made
// the workspace owner.
func TestDeleteUser_doesNotRemoveAMemberPromotedToOwnerMeanwhile(t *testing.T) {
	if dbtest.PostgresDSN() == "" {
		t.Skipf("%s is not set; this interleaving needs PostgreSQL", dbtest.DSNEnv)
	}
	h, database, _, ownerID := setupWorkspaceWithDB(t)
	adminKey := "race-admin-key"
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,is_owner) VALUES ('u2','a@example.com','Admin','UTC',1,0)`)
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k2','u2','t',?,'2024-01-01')`, sha256HexForTest(adminKey))
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','m@example.com','Member','UTC',0)`)

	ctx := context.Background()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `UPDATE users SET is_owner = 0 WHERE id = ?`, ownerID); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET is_owner = 1, is_admin = 1 WHERE id = 'u3'`); err != nil {
		t.Fatalf("promote: %v", err)
	}

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := authReq(http.MethodDelete, "/v1/users/u3", "", adminKey)
		req.SetPathValue("id", "u3")
		h.RequireAuth(h.DeleteUser)(rec, req)
	}()
	waitForBlockedWrite(t, database, done, `DELETE FROM users WHERE id = `)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	<-done

	var n int
	database.QueryRow(`SELECT COUNT(*) FROM users WHERE id = 'u3'`).Scan(&n)
	if n != 1 {
		t.Fatalf("the new owner was deleted (status %d): the workspace has no owner", rec.Code)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status %d; want 409 for a delete refused because the member changed", rec.Code)
	}
}

// Transferring ownership to a member who is removed meanwhile must not commit the
// demotion of the current owner: the workspace would be left with no owner.
func TestTransferOwnership_rollsBackWhenTheTargetVanishesMeanwhile(t *testing.T) {
	if dbtest.PostgresDSN() == "" {
		t.Skipf("%s is not set; this interleaving needs PostgreSQL", dbtest.DSNEnv)
	}
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','m@example.com','Member','UTC',0)`)

	ctx := context.Background()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = 'u3'`); err != nil {
		t.Fatalf("delete: %v", err)
	}

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := authReq(http.MethodPost, "/v1/users/u3/transfer-ownership", "", ownerKey)
		req.SetPathValue("id", "u3")
		h.RequireAuth(h.TransferOwnership)(rec, req)
	}()
	waitForBlockedWrite(t, database, done, `UPDATE users SET is_owner = 1, is_admin = 1 WHERE id = `)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	<-done

	var isOwner int
	database.QueryRow(`SELECT is_owner FROM users WHERE id = ?`, ownerID).Scan(&isOwner)
	if isOwner != 1 {
		t.Errorf("owner was demoted although the transfer target is gone (status %d)", rec.Code)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d; want 404 for a transfer to a user who no longer exists", rec.Code)
	}
}

// waitForBlockedWrite returns once a backend is running the handler's contested write,
// named by the start of its statement text. A backend waiting on the held row lock is
// also `active`, so this covers both "about to block" and "blocked". It fails the test if
// the handler finishes first, or if the write is never seen in 30s: either way the
// interleaving under test was not reached, and the assertions after the commit would
// pass on ordinary precondition checks alone.
func waitForBlockedWrite(t *testing.T, database *db.DB, done <-chan struct{}, statementPrefix string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatal("the handler finished before its write reached the held row, so the race was not exercised")
		default:
		}
		var running int
		if err := database.QueryRow(`
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND state = 'active' AND query LIKE ?`,
			statementPrefix+"%").Scan(&running); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if running > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("in 30s no backend ran %q, so the race was not exercised", statementPrefix)
}
