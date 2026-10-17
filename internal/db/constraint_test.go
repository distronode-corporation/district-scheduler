package db_test

import (
	"errors"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/uid"
)

// TestConstraintPredicates provokes a real violation of each class against the real
// schema, on whichever engine dbtest is configured for, and asserts the predicate
// recognises it.
//
// Provoked rather than constructed: a hand-built error would only prove the
// predicate agrees with whatever the test author believed the driver returns, and
// the whole reason these helpers exist is that that belief was wrong on PostgreSQL.
// Run it twice — once bare, once with CALNODE_TEST_POSTGRES_DSN — and both engines
// are covered.
func TestConstraintPredicates(t *testing.T) {
	h := dbtest.Open(t)

	// A user and an event type to hang the booking constraints off.
	userID := uid.New()
	if _, err := h.Exec(
		`INSERT INTO users (id, email, name, iana_timezone) VALUES (?, ?, 'Host', 'UTC')`,
		userID, userID+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	etID := uid.New()
	if _, err := h.Exec(
		`INSERT INTO event_types (id, user_id, slug, name, duration_minutes)
		 VALUES (?, ?, ?, 'Call', 30)`, etID, userID, etID); err != nil {
		t.Fatalf("seed event type: %v", err)
	}

	t.Run("unique", func(t *testing.T) {
		// users.email is UNIQUE in both migration sets.
		_, err := h.Exec(
			`INSERT INTO users (id, email, name, iana_timezone) VALUES (?, ?, 'Clash', 'UTC')`,
			uid.New(), userID+"@example.com")
		assertOnly(t, err, "unique", db.IsUniqueViolation)
	})

	// ⛔ The case that distinguishes a correct implementation from a plausible one.
	//
	// SQLite reports a PRIMARY KEY collision as SQLITE_CONSTRAINT_PRIMARYKEY (1555),
	// NOT as SQLITE_CONSTRAINT_UNIQUE (2067) — while still saying "UNIQUE constraint
	// failed" in the message. So a predicate matching only 2067 passes the subtest
	// above and fails here, and the old text match passed both by accident.
	//
	// This is not a theoretical shape: idempotency_keys.idempotency_key is a bare
	// PRIMARY KEY, so claimIdempotencyKey's entire replay path depends on 1555 being
	// classified as a unique violation. PostgreSQL reports 23505 for both, so this
	// subtest is redundant there — and it runs there anyway, because "redundant on
	// one engine" is exactly the assumption worth re-checking after a driver bump.
	t.Run("unique via primary key", func(t *testing.T) {
		key := uid.New()
		if _, err := h.Exec(
			`INSERT INTO idempotency_keys (idempotency_key, request_hash, created_at) VALUES (?, 'h', ?)`,
			key, "2026-06-01T00:00:00Z"); err != nil {
			t.Fatalf("seed idempotency key: %v", err)
		}
		_, err := h.Exec(
			`INSERT INTO idempotency_keys (idempotency_key, request_hash, created_at) VALUES (?, 'h', ?)`,
			key, "2026-06-01T00:00:00Z")
		assertOnly(t, err, "primary key", db.IsUniqueViolation)
	})

	t.Run("check", func(t *testing.T) {
		// bookings.status has CHECK (status IN ('confirmed','cancelled')).
		_, err := h.Exec(`
			INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status, created_at, updated_at)
			VALUES (?, ?, ?, '2026-06-15T09:00:00Z', '2026-06-15T09:30:00Z', 'not-a-status', ?, ?)`,
			uid.New(), etID, userID, "2026-06-01T00:00:00Z", "2026-06-01T00:00:00Z")
		assertOnly(t, err, "check", db.IsCheckViolation)
	})

	t.Run("foreign key", func(t *testing.T) {
		// event_type_id references event_types(id). SQLite needs foreign_keys=ON,
		// which OpenDB sets.
		_, err := h.Exec(`
			INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status, created_at, updated_at)
			VALUES (?, 'no-such-event-type', ?, '2026-06-15T10:00:00Z', '2026-06-15T10:30:00Z', 'confirmed', ?, ?)`,
			uid.New(), userID, "2026-06-01T00:00:00Z", "2026-06-01T00:00:00Z")
		assertOnly(t, err, "foreign key", db.IsForeignKeyViolation)
	})

	// The foreign-key case above inserts a booking pointing at a row that does not
	// exist, which SQLite reports as SQLITE_CONSTRAINT_FOREIGNKEY (787). Deleting an
	// event type that a booking already references is a different statement against a
	// different clause — bookings.event_type_id is declared ON DELETE RESTRICT — and
	// SQLite reports THAT as SQLITE_CONSTRAINT_FOREIGNKEYKEY (1811), with an identical
	// message. A classifier checking only 787 passes the subtest above and fails here,
	// which is exactly how DeleteEventType answered 500 "internal error" instead of
	// 409 when an operator tried to delete an event type with bookings in history.
	t.Run("foreign key on delete restrict", func(t *testing.T) {
		const etID = "et-restrict"
		if _, err := h.Exec(
			`INSERT INTO event_types (id, user_id, slug, name, duration_minutes, slot_interval_minutes, location_type, is_active, is_public, archived_at)
			VALUES (?, ?, 'restrict-et', 'Restrict ET', 30, 30, 'in_person', 1, 1, NULL)`,
			etID, userID); err != nil {
			t.Fatalf("seed event type: %v", err)
		}
		if _, err := h.Exec(
			`INSERT INTO event_type_hosts (id, event_type_id, user_id, role, priority) VALUES (?, ?, ?, 'required', 0)`,
			uid.New(), etID, userID); err != nil {
			t.Fatalf("seed host: %v", err)
		}
		if _, err := h.Exec(
			`INSERT INTO bookings (id, event_type_id, host_id, start_at, end_at, status, created_at, updated_at)
			VALUES (?, ?, ?, '2026-06-15T11:00:00Z', '2026-06-15T11:30:00Z', 'confirmed', ?, ?)`,
			"b-restrict", etID, userID, "2026-06-01T00:00:00Z", "2026-06-01T00:00:00Z"); err != nil {
			t.Fatalf("seed booking: %v", err)
		}

		_, err := h.Exec(`DELETE FROM event_types WHERE id = ?`, etID)
		assertOnly(t, err, "foreign key (delete restrict)", db.IsForeignKeyViolation)
	})

	t.Run("unrelated error", func(t *testing.T) {
		// A predicate that answered true for everything would satisfy every caller
		// above and be badly wrong, so the negative cases carry as much weight.
		_, err := h.Exec(`SELECT * FROM a_table_that_does_not_exist`)
		if err == nil {
			t.Fatal("expected an error from a missing table")
		}
		assertNone(t, err, "missing table")
		assertNone(t, errors.New("some unrelated failure"), "plain error")
		assertNone(t, nil, "nil")
	})

	t.Run("unhandled constraint class", func(t *testing.T) {
		// A NOT NULL violation is a constraint violation of a class nothing here
		// classifies (SQLite 1299, PostgreSQL 23502). It must match none of the
		// three, so a caller cannot turn it into a 409 by accident.
		_, err := h.Exec(
			`INSERT INTO users (id, email, name, iana_timezone) VALUES (?, ?, NULL, 'UTC')`,
			uid.New(), uid.New()+"@example.com")
		if err == nil {
			t.Fatal("expected a NOT NULL violation")
		}
		assertNone(t, err, "not-null violation")
	})
}

// TestConstraintTextFallback covers the branch no live engine reaches: an error that
// arrives without its driver type still attached.
//
// It is engine-independent, so it needs no database. The branch exists for a driver
// release that changes its error type, or a layer that reformats an error into a
// plain one rather than wrapping it — in which case the message is the only signal
// left, and answering from it beats returning a 500. Without this test the fallback
// would be unexecuted code that reads like an accident.
func TestConstraintTextFallback(t *testing.T) {
	cases := []struct {
		text string
		want func(error) bool
		name string
	}{
		{"boom: UNIQUE constraint failed: t.a", db.IsUniqueViolation, "unique"},
		{"boom: CHECK constraint failed: n > 0", db.IsCheckViolation, "check"},
		{"boom: FOREIGN KEY constraint failed", db.IsForeignKeyViolation, "foreign key"},
	}
	for _, c := range cases {
		err := errors.New(c.text)
		if !c.want(err) {
			t.Errorf("%s: fallback did not recognise %q", c.name, c.text)
		}
	}
	// And the fallback is still discriminating, not a catch-all.
	assertNone(t, errors.New("NOT NULL constraint failed: t.a"), "not-null text")
}

// assertOnly checks that want recognises err and the other two predicates do not:
// the classes have to be distinguishable, or a CHECK violation becomes a 409.
func assertOnly(t *testing.T, err error, class string, want func(error) bool) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a constraint violation, got nil", class)
	}
	if !want(err) {
		t.Errorf("%s: predicate did not recognise %v", class, err)
	}
	matches := 0
	for _, p := range []func(error) bool{db.IsUniqueViolation, db.IsCheckViolation, db.IsForeignKeyViolation} {
		if p(err) {
			matches++
		}
	}
	if matches != 1 {
		t.Errorf("%s: %d of 3 predicates matched %v; want exactly 1", class, matches, err)
	}
}

func assertNone(t *testing.T, err error, what string) {
	t.Helper()
	for name, p := range map[string]func(error) bool{
		"IsUniqueViolation":     db.IsUniqueViolation,
		"IsCheckViolation":      db.IsCheckViolation,
		"IsForeignKeyViolation": db.IsForeignKeyViolation,
	} {
		if p(err) {
			t.Errorf("%s returned true for %s (%v)", name, what, err)
		}
	}
}
