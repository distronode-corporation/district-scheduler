package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	<-done

	var n int
	database.QueryRow(`SELECT COUNT(*) FROM users WHERE id = 'u3'`).Scan(&n)
	if n != 1 {
		t.Fatalf("the new owner was deleted (status %d): the workspace has no owner", rec.Code)
	}
	if rec.Code == http.StatusOK {
		t.Errorf("got 200 for a delete that removed nothing; want a refusal")
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
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	<-done

	var isOwner int
	database.QueryRow(`SELECT is_owner FROM users WHERE id = ?`, ownerID).Scan(&isOwner)
	if isOwner != 1 {
		t.Errorf("owner was demoted although the transfer target is gone (status %d)", rec.Code)
	}
	if rec.Code == http.StatusOK {
		t.Errorf("got 200 for a transfer to a user who no longer exists")
	}
}
