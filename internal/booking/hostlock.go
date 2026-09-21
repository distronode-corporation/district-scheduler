package booking

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"

	"github.com/calnode/calnode/internal/db"
)

// hostLockDomain prefixes every id fed to the hash below, so a future advisory
// lock on some other entity cannot collide with a host's key by hashing the same
// raw string.
const hostLockDomain = "calnode:booking:host:"

// lockHosts serialises the check-then-write window for a set of hosts, for the
// remainder of tx.
//
// Create, Reschedule and ReassignHost all read "is this host busy at this time?"
// and then write on the answer. On SQLite that is free of TOCTOU races without any
// locking, because the pool is a single connection (db.SetMaxOpenConns(1), see
// internal/db/db.go) and transactions therefore cannot interleave at all. A
// Postgres pool has many connections: two overlapping bookings can both read
// "free" and both insert, and the partial unique index
// idx_bookings_no_double(host_id, start_at) only catches an identical start time,
// not a partial overlap. That is exactly the gap the app-level check was never
// asked to close on its own.
//
// pg_advisory_xact_lock is held until the transaction commits or rolls back, so
// there is no unlock call to forget and no leak when a caller returns early — which
// every guard in these functions does. Locking per host rather than once globally
// keeps bookings for different hosts concurrent, which is the whole point of moving
// off the single connection.
//
// On SQLite this is a no-op and the existing guarantee stands unchanged.
func lockHosts(ctx context.Context, tx *db.Tx, hostIDs ...string) error {
	if tx.Dialect() != db.DialectPostgres {
		return nil
	}

	var hosts []string
	for _, id := range hostIDs {
		if id != "" {
			hosts = append(hosts, id)
		}
	}
	if len(hosts) == 0 {
		return nil
	}

	// One key per host, plus one per calendar any of these hosts checks for conflicts or
	// books into. hostBusy (since upstream's shared-calendar check) also counts another
	// host's booking when that host's destination calendar is one this host checks, so
	// two hosts sharing a calendar race on that calendar, not on either host: locking the
	// hosts alone let both read "free" and both insert (TestSharedCalendarAtomicConflict).
	// Every side of such a pair names the shared calendar here, whichever role it plays
	// for them, so both take its key.
	keys := make([]int64, 0, len(hosts))
	for _, id := range hosts {
		keys = append(keys, hostLockKey(id))
	}
	calKeys, err := calendarLockKeys(ctx, tx, hosts)
	if err != nil {
		return err
	}
	keys = append(keys, calKeys...)

	// Sorted and deduplicated, by KEY. Two transactions that needed the same two keys in
	// opposite orders would otherwise deadlock, and Postgres resolves a deadlock by
	// killing one of them: a 500 on a booking that should have been a 409 or a success.
	// Any one total order works as long as every caller uses it, and every caller comes
	// through here. Sorting is done on a copy: Create's HostIDs arrive in round-robin
	// priority order and that order decides who gets the booking.
	slices.Sort(keys)
	keys = slices.Compact(keys)

	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, key); err != nil {
			return fmt.Errorf("booking: lock hosts %v: %w", hosts, err)
		}
	}
	return nil
}

// calendarLockKeys returns one advisory-lock key per distinct calendar (provider,
// account email, calendar id: the identity hostBusy compares) that any of hostIDs has
// selected for conflict checks or as its destination.
func calendarLockKeys(ctx context.Context, tx *db.Tx, hostIDs []string) ([]int64, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(hostIDs)), ",")
	args := make([]any, len(hostIDs))
	for i, id := range hostIDs {
		args[i] = id
	}
	// #nosec G202 -- only "?" placeholders are concatenated; every value is bound.
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT provider, COALESCE(account_email, ''), calendar_id
		FROM connection_calendars
		WHERE user_id IN (`+placeholders+`) AND (check_conflicts = 1 OR is_destination = 1)`, args...)
	if err != nil {
		return nil, fmt.Errorf("booking: load calendars to lock: %w", err)
	}
	defer rows.Close() // #nosec G307 -- read-only cursor; rows.Err below reports iteration failures
	var keys []int64
	for rows.Next() {
		var provider, account, calendarID string
		if err := rows.Scan(&provider, &account, &calendarID); err != nil {
			return nil, fmt.Errorf("booking: scan calendar to lock: %w", err)
		}
		keys = append(keys, calendarLockKey(provider, account, calendarID))
	}
	return keys, rows.Err()
}

// calendarLockDomain keeps calendar keys disjoint from host keys (hostLockDomain).
const calendarLockDomain = "calnode:booking:calendar:"

// calendarLockKey derives a calendar's advisory-lock key the way hostLockKey derives a
// host's, from the three fields hostBusy matches on, NUL-separated so no two distinct
// triples concatenate to the same string.
func calendarLockKey(provider, account, calendarID string) int64 {
	sum := sha256.Sum256([]byte(calendarLockDomain + provider + "\x00" + account + "\x00" + calendarID))
	// #nosec G115 -- same reinterpretation as hostLockKey: uint64 -> int64 keeps all 64 bits.
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// hostLockKey maps a host id onto the single int64 key pg_advisory_xact_lock takes.
//
// SHA-256 of the domain-separated id, with the first eight bytes read big-endian as
// a signed integer. Deriving it in Go rather than with the engine's hashtext() keeps
// the derivation readable and testable from Go, and means the value arrives as an
// ordinary bound parameter.
//
// Two distinct hosts landing on the same key would cost an unnecessary
// serialisation between two unrelated bookings, never a wrong answer, so what this
// needs is stability and a good spread rather than cryptographic strength. SHA-256
// is used because this package already imports it for manage tokens.
func hostLockKey(hostID string) int64 {
	sum := sha256.Sum256([]byte(hostLockDomain + hostID))
	// #nosec G115 -- not a narrowing conversion: uint64 -> int64 is the same 64 bits read
	// two's-complement, and pg_advisory_xact_lock's parameter is a bigint whose domain is
	// exactly that signed range, negatives included. Every one of the 2^64 hash values maps
	// to a distinct, valid key, so there is no input for which this loses information or
	// produces a wrong lock. Masking the top bit to satisfy the rule numerically would
	// change the derivation, which TestHostLockKey pins precisely because two instances
	// sharing one Postgres must agree on the key or they serialise against nothing.
	return int64(binary.BigEndian.Uint64(sum[:8]))
}
