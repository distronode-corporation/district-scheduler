package db

import (
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"
)

// Constraint violations are the one class of database error Calnode routinely acts
// on rather than just reporting: a duplicate slug is a 409, an out-of-range value is
// a 400, a dangling reference is a 404. Deciding which is which used to be a
// substring match on SQLite's English message, which is invisible to every gate and
// degraded silently to a 500 on PostgreSQL.
//
// Both engines are matched on their error codes. Codes rather than text because the
// text is not a contract: PostgreSQL localises its messages by the server's
// lc_messages, so a server running in German defeats any text match no matter how
// carefully written.
const (
	pgUniqueViolation     = "23505" // unique_violation, and PostgreSQL's code for a primary-key collision too
	pgCheckViolation      = "23514" // check_violation
	pgForeignKeyViolation = "23503" // foreign_key_violation
)

// SQLite's extended result codes, as reported by (*sqlite.Error).Code().
//
// ⛔ SQLITE_CONSTRAINT_PRIMARYKEY is a SEPARATE code from
// SQLITE_CONSTRAINT_UNIQUE even though both carry the message "UNIQUE constraint
// failed". Matching only 2067 would silently stop recognising primary-key
// collisions, and Calnode has one that matters: idempotency_keys.idempotency_key is
// a bare PRIMARY KEY, so every idempotent replay arrives as 1555. Both belong to
// IsUniqueViolation. PostgreSQL has no such split — a primary-key collision is
// 23505 like any other unique violation — which is why the trap only exists on one
// side.
const (
	sqliteConstraintCheck      = 275  // SQLITE_CONSTRAINT_CHECK
	sqliteConstraintForeignKey = 787  // SQLITE_CONSTRAINT_FOREIGNKEY
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE

	// sqliteConstraintForeignKeyKey is SQLITE_CONSTRAINT_FOREIGNKEYKEY, the extended
	// code SQLite reports INSTEAD of 787 when the violated constraint is declared
	// ON DELETE RESTRICT. Verified against the driver: NO ACTION and an unqualified
	// REFERENCES both report 787, RESTRICT reports 1811, and the message is identical
	// in every case. Calnode declares bookings.event_type_id ON DELETE RESTRICT
	// (explicitly, to stop event-type deletion), so the 787-only check classified
	// every such delete as unrecognised and the handler answered 500 "internal error"
	// where it means 409. Matching only 787 was a silent hole in exactly the case
	// this classifier exists to catch.
	sqliteConstraintForeignKeyKey = 1811 // SQLITE_CONSTRAINT_FOREIGNKEYKEY
)

// SQLite's message fragments, used only as a fallback — see violates.
const (
	sqliteUniqueText     = "UNIQUE constraint failed"
	sqliteCheckText      = "CHECK constraint failed"
	sqliteForeignKeyText = "FOREIGN KEY constraint failed"
)

// IsUniqueViolation reports whether err is a unique-constraint violation — a
// duplicate slug, a replayed idempotency key, a second booking at one host's exact
// start time. A primary-key collision counts, on both engines.
func IsUniqueViolation(err error) bool {
	return violates(err, pgUniqueViolation, sqliteUniqueText,
		sqliteConstraintUnique, sqliteConstraintPrimaryKey)
}

// IsCheckViolation reports whether err is a CHECK-constraint violation, i.e. a value
// outside the set the column allows. Callers turn this into a 400, since the only way
// to reach it is a request carrying a value the handler did not validate.
func IsCheckViolation(err error) bool {
	return violates(err, pgCheckViolation, sqliteCheckText, sqliteConstraintCheck)
}

// IsForeignKeyViolation reports whether err is a foreign-key violation — a reference
// to a row that does not exist, or a delete that would orphan one. ON DELETE RESTRICT
// reports a different extended code from the default NO ACTION, so both are accepted;
// see sqliteConstraintForeignKeyKey.
func IsForeignKeyViolation(err error) bool {
	return violates(err, pgForeignKeyViolation, sqliteForeignKeyText, sqliteConstraintForeignKey, sqliteConstraintForeignKeyKey)
}

// violates classifies err: the driver's own error code when one is available, the
// message only when it is not.
//
// A driver error is a DEFINITE answer in both directions. A *pgconn.PgError or a
// *sqlite.Error whose code does not match returns false and does not fall through to
// the text comparison — falling through would classify an error by whether its
// message happened to contain an English phrase, which is the fragility being
// removed. It would also reintroduce the primary-key trap in reverse: a 1555 error
// excluded by code would be readmitted by its "UNIQUE constraint failed" message.
//
// The text fallback is deliberate rather than vestigial. It covers an error that
// reaches here without the concrete driver type still attached — a driver release
// that changes its error type, a layer that reformats an error into a plain one
// instead of wrapping it. In that case the message is the only signal left, and
// answering from it beats answering "not a constraint violation" and returning a 500.
func violates(err error, sqlstate, sqliteText string, sqliteCodes ...int) bool {
	if err == nil {
		return false
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == sqlstate
	}

	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return slices.Contains(sqliteCodes, sqliteErr.Code())
	}

	return strings.Contains(err.Error(), sqliteText)
}
