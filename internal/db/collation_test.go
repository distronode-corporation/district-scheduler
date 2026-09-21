package db_test

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// Calnode stores timestamps as TEXT and compares them lexicographically: the
// worker claims with `run_at <= ?`, sessions and tokens expire on
// `expires_at > ?`, the consent window brackets `decided_at`, booking overlap is
// `start_at`/`end_at` against bound strings, and several lists are
// `ORDER BY created_at`. On SQLite that is memcmp, always. On PostgreSQL it is a
// comparison under the column's collation, so migration 00059 pins every one of
// those columns to COLLATE "C".
//
// These tests hold that pin. They are Postgres-only because SQLite has no
// collation to get wrong.

// timestampColumnPredicate selects the columns migration 00059 covers, by name.
//
// `_at`/`_until` catches every timestamp column in the schema. The three
// remaining names are the availability columns, which hold 'HH:MM' and
// 'YYYY-MM-DD' and are ordered as times too (`ORDER BY day_of_week, start_time`
// in internal/handler/availability.go, `ORDER BY date` in override.go). Matching
// by name rather than by a committed list is the point: a timestamp column added
// by a later migration is caught here without anyone remembering to add it.
const timestampColumnPredicate = `(c.column_name ~ '_(at|until)$'
	  OR c.column_name IN ('date', 'start_time', 'end_time'))`

// wantTimestampColumns is the number of columns 00059 altered, plus the two
// workspaces timestamps 00060 added, sso_nonces.expires_at from 00061 and the three
// password_reset_tokens timestamps from 00066. A
// floor, not an equality: the assertion that matters is "every match is C", and a
// query that silently stopped matching anything would satisfy that vacuously.
const wantTimestampColumns = 60

func TestPostgres_timestampColumnsCollateC(t *testing.T) {
	handle := dbtest.RequirePostgres(t)

	rows, err := handle.Query(`
		SELECT c.table_name, c.column_name, COALESCE(c.collation_name, '<database default>')
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema = current_schema()
		  AND t.table_type = 'BASE TABLE'
		  AND c.data_type = 'text'
		  AND ` + timestampColumnPredicate + `
		ORDER BY c.table_name, c.column_name`)
	if err != nil {
		t.Fatalf("read information_schema.columns: %v", err)
	}
	defer rows.Close()

	var checked int
	var offenders []string
	for rows.Next() {
		var table, column, collation string
		if err := rows.Scan(&table, &column, &collation); err != nil {
			t.Fatalf("scan column row: %v", err)
		}
		checked++
		if collation != "C" {
			offenders = append(offenders, table+"."+column+" = "+collation)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}

	if checked < wantTimestampColumns {
		t.Errorf("audited %d timestamp columns; want at least %d — did the predicate stop matching?",
			checked, wantTimestampColumns)
	}
	if len(offenders) > 0 {
		t.Errorf("%d of %d timestamp columns are not COLLATE \"C\":\n\t%s",
			len(offenders), checked, strings.Join(offenders, "\n\t"))
	}
	t.Logf("audited %d TEXT timestamp columns, %d not C", checked, len(offenders))
}

// collationProbeValues are timestamp-shaped strings whose byte order and
// linguistic order disagree.
//
// The first two are the shapes the schema really stores (internal/dbtime: a
// space-separated `datetime('now')` and an RFC 3339 `strftime`). ⚠️ Measured on
// the PostgreSQL 17 this branch is developed against, those two do NOT flip
// under the database's en_US.utf8 default, nor under any of the other 878
// collations installed on that server: glibc ignores the space at the primary
// level but still sorts a digit before 'T', which happens to agree with
// memcmp. So they cannot carry the control on their own — a test built only on
// them would pass with or without migration 00059.
//
// The third is the same instant with RFC 3339's lower-case 't' and 'z', which
// §5.6 of the RFC explicitly permits and which an importer or a third-party API
// can therefore hand us. Case is a tertiary-level difference: en_US.utf8 puts
// lower case first, memcmp puts upper case first ('T' is 0x54, 't' is 0x74).
// That is the pair that makes this control able to fail.
var collationProbeValues = []string{
	"2026-01-01 10:00:00",
	"2026-01-01T10:00:00.000Z",
	"2026-01-01t10:00:00z",
	"2026-01-01T10:00:00Z",
}

// TestPostgres_collationControl is the positive control: it proves the audit
// above is testing something, by showing that a plain TEXT column on this server
// really does order these values differently from a COLLATE "C" one.
//
// If the server's own default already behaves byte-wise (a C or C.UTF-8
// database), there is nothing to control against and the test SKIPS naming the
// collation, rather than passing vacuously — a green run on such a server says
// nothing about a deployment on a linguistic one.
func TestPostgres_collationControl(t *testing.T) {
	handle := dbtest.RequirePostgres(t)

	// The server's collation, for the skip message. lc_collate stopped being a
	// GUC in PostgreSQL 16 (SHOW lc_collate errors with "unrecognized
	// configuration parameter"), so it is read from the catalog, which is also
	// where the per-database value has always actually lived.
	var collate, ctype, provider string
	if err := handle.QueryRow(`
		SELECT datcollate, datctype, datlocprovider
		FROM pg_database WHERE datname = current_database()`).Scan(&collate, &ctype, &provider); err != nil {
		t.Fatalf("read pg_database locale: %v", err)
	}
	serverCollation := collate + " (ctype " + ctype + ", provider " + provider + ")"

	if _, err := handle.Exec(`
		CREATE TABLE collation_control (
			plain TEXT NOT NULL,
			cee   TEXT COLLATE "C" NOT NULL
		)`); err != nil {
		t.Fatalf("create control table: %v", err)
	}

	for _, v := range collationProbeValues {
		if _, err := handle.Exec(`INSERT INTO collation_control (plain, cee) VALUES (?, ?)`, v, v); err != nil {
			t.Fatalf("insert %q: %v", v, err)
		}
	}

	plainOrder := orderedColumn(t, handle, "plain")
	ceeOrder := orderedColumn(t, handle, "cee")

	wantBytes := slices.Clone(collationProbeValues)
	sort.Strings(wantBytes) // Go's sort on strings is byte-wise, i.e. what SQLite does

	if !slices.Equal(ceeOrder, wantBytes) {
		t.Errorf("COLLATE \"C\" column ordered\n\t%v\nwant byte order\n\t%v", ceeOrder, wantBytes)
	}

	if slices.Equal(plainOrder, ceeOrder) {
		t.Skipf("server default collation %s orders these values byte-wise too, "+
			"so this control cannot distinguish a collated column from a C one; "+
			"the audit is unproven on this server", serverCollation)
	}

	t.Logf("control fired: server default collation is %s\n\tplain: %v\n\tC    : %v",
		serverCollation, plainOrder, ceeOrder)
}

func orderedColumn(t *testing.T, handle *db.DB, column string) []string {
	t.Helper()

	// column is one of two literals above, never input.
	rows, err := handle.Query(`SELECT ` + column + ` FROM collation_control ORDER BY ` + column)
	if err != nil {
		t.Fatalf("order by %s: %v", column, err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan %s: %v", column, err)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", column, err)
	}
	return got
}

// TestPostgres_jobsRunAtOrdering holds the ordering on the column the worker
// actually polls.
//
// Two things are asserted, and they are not the same thing:
//
//  1. `ORDER BY run_at` matches Go's byte-wise sort of the same values. This is
//     the discriminating half: the lower-case RFC 3339 value in the set orders
//     differently under a linguistic collation, so this fails without 00059.
//  2. The real claim predicate — `WHERE status = 'pending' AND run_at <= ?` from
//     internal/worker/worker.go, with an RFC 3339 `now` — sees the
//     space-separated shape as due and a future T-shape as not. internal/handler/
//     notetaker.go depends on exactly that: it writes `datetime('now')`'s space
//     form *because* it sorts before any T-separated stamp, which is what makes a
//     notetaker job due immediately. Both collations happen to agree here, so
//     this half is a regression pin rather than a discriminator.
func TestPostgres_jobsRunAtOrdering(t *testing.T) {
	handle := dbtest.RequirePostgres(t)

	// Same instant in the two shapes the tree writes, plus the lower-case RFC
	// 3339 spelling, all in the past relative to `now` below.
	runAts := []string{
		"2026-01-01 10:00:00",
		"2026-01-01T10:00:00.000Z",
		"2026-01-01t10:00:00z",
		"2026-01-01T10:00:00Z",
	}
	for i, runAt := range runAts {
		insertJob(t, handle, "job-past-"+string(rune('a'+i)), runAt)
	}
	insertJob(t, handle, "job-future", "2027-01-01T00:00:00Z")

	rows, err := handle.Query(`SELECT run_at FROM jobs ORDER BY run_at`)
	if err != nil {
		t.Fatalf("order by run_at: %v", err)
	}
	var order []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			t.Fatalf("scan run_at: %v", err)
		}
		order = append(order, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate run_at: %v", err)
	}

	want := append(slices.Clone(runAts), "2027-01-01T00:00:00Z")
	sort.Strings(want)
	if !slices.Equal(order, want) {
		t.Errorf("ORDER BY run_at =\n\t%v\nwant byte order\n\t%v", order, want)
	}

	// The worker's own predicate, verbatim from internal/worker/worker.go, with
	// the RFC 3339 `now` it binds.
	const now = "2026-06-01T12:00:00Z"
	claimed, err := handle.Query(`
		SELECT id, type, payload, attempts, max_attempts
		FROM jobs
		WHERE status = 'pending' AND run_at <= ?
		LIMIT 10`, now)
	if err != nil {
		t.Fatalf("claim query: %v", err)
	}
	defer claimed.Close()

	var ids []string
	for claimed.Next() {
		var id, typ, payload string
		var attempts, maxAttempts int
		if err := claimed.Scan(&id, &typ, &payload, &attempts, &maxAttempts); err != nil {
			t.Fatalf("scan claimed job: %v", err)
		}
		ids = append(ids, id)
	}
	if err := claimed.Err(); err != nil {
		t.Fatalf("iterate claimed jobs: %v", err)
	}
	sort.Strings(ids)

	wantIDs := []string{"job-past-a", "job-past-b", "job-past-c", "job-past-d"}
	if !slices.Equal(ids, wantIDs) {
		t.Errorf("claimed %v; want %v (the four past shapes, not the future one)", ids, wantIDs)
	}
}

func insertJob(t *testing.T, handle *db.DB, id, runAt string) {
	t.Helper()
	if _, err := handle.Exec(`
		INSERT INTO jobs (id, type, payload, run_at, status, attempts, max_attempts)
		VALUES (?, 'reminder.send', ?, ?, 'pending', 0, 3)`, id, `{"id":"`+id+`"}`, runAt); err != nil {
		t.Fatalf("insert job %s: %v", id, err)
	}
}
