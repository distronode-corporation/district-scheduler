package handler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/calnode/calnode/internal/db"
)

// bootstrapLockDomain keeps the first-user lock's keys disjoint from every other
// advisory lock taken on the same database (booking's host and calendar locks).
const bootstrapLockDomain = "calnode:bootstrap:workspace:"

// lockBootstrap serialises the first-user decision for one workspace for the rest
// of tx.
//
// Setup and Claim both read "are there any users?" and then insert the first
// (administrator) user on the answer. On SQLite the single-connection pool makes
// that check-then-insert atomic. A PostgreSQL pool does not: at READ COMMITTED two
// overlapping transactions can each count zero users and each insert one. Both
// handlers take this lock before counting, so whichever commits first wins and the
// other then sees a user and answers 409.
//
// On SQLite this is a no-op and the existing guarantee stands unchanged.
func lockBootstrap(ctx context.Context, tx *db.Tx, workspaceID string) error {
	if tx.Dialect() != db.DialectPostgres {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, bootstrapLockKey(workspaceID)); err != nil {
		return fmt.Errorf("lock bootstrap: %w", err)
	}
	return nil
}

// bootstrapLockKey maps a workspace id onto pg_advisory_xact_lock's bigint key, the
// same derivation booking.hostLockKey uses for hosts.
func bootstrapLockKey(workspaceID string) int64 {
	sum := sha256.Sum256([]byte(bootstrapLockDomain + workspaceID))
	// #nosec G115 -- uint64 -> int64 keeps all 64 bits; the lock's key domain is the full signed range.
	return int64(binary.BigEndian.Uint64(sum[:8]))
}
