package handler_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/uid"
)

// The reminder-replacement race, forced rather than waited for.
//
// Two detached goroutines write the same reminder payload: the booking CREATE path
// (enqueueBookingReminders) and the RESCHEDULE path (replaceReminderJobs). jobs carries a
// unique on (workspace_id, type, payload), so exactly one row can exist per
// (booking, hours_before) — and the losing interleaving is:
//
//	reschedule: DELETE reminder rows for this booking   (create's row not yet visible)
//	create:     INSERT the reminder row at the OLD time
//	reschedule: INSERT the reminder row at the NEW time -> conflict
//
// With ON CONFLICT DO NOTHING the last step is a silent no-op and the reminder stays
// pinned to the original time forever. With DO UPDATE it wins, which is what this holds.
//
// ⛔ The interleaving is forced by the unique index itself, not by a sleep: the
// create-side row is inserted inside an UNCOMMITTED transaction, so it is invisible to
// the DELETE and yet already owns the key — which makes the reschedule's INSERT BLOCK
// until that transaction commits. The test waits for a backend to actually be
// lock-waiting before committing, so a run where the interleaving was never reached fails
// loudly instead of passing on the easy path.
func TestReplaceReminderJobs_upsertSurvivesTheLosingInterleaving(t *testing.T) {
	h, database, key, _ := setupWorkspaceWithDB(t)
	ctx := context.Background()

	if database.Dialect() != db.DialectPostgres {
		t.Skip("LOUD SKIP: this needs two concurrent connections to hold one uncommitted " +
			"row while another transaction blocks on its key. SQLite runs on a single " +
			"connection by design (it is what serialises write transactions there), so " +
			"the interleaving cannot be constructed and the race cannot occur either.")
	}

	slug, etID := seedEventTypeHTTP(t, h, key)
	bookStart := futureAt(10, 10, 0)
	newStart := futureAt(13, 9, 0)
	bookingID := createBookingViaHTTP(t, h, slug, bookStart.Format(time.RFC3339))

	// Clean slate: drop whatever the create path enqueued, so the only rows in play are
	// the ones this test writes.
	if _, err := database.ExecContext(ctx,
		`DELETE FROM jobs WHERE type = 'reminder.send'`); err != nil {
		t.Fatalf("clear reminder jobs: %v", err)
	}

	oldRunAt := bookStart.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	wantRunAt := newStart.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	payload := fmt.Sprintf(`{"booking_id":%q,"hours_before":24}`, bookingID)

	// The create side's insert, held open. Identical payload, OLD run_at.
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin create-side tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jobs (id, type, payload, run_at, status, attempts, max_attempts)
		VALUES (?, 'reminder.send', ?, ?, 'pending', 0, 3)
		ON CONFLICT (workspace_id, type, payload) WHERE status IN ('pending', 'running') DO NOTHING`,
		uid.New(), payload, oldRunAt); err != nil {
		t.Fatalf("create-side insert: %v", err)
	}

	// The reschedule, which will delete nothing (the row above is invisible) and then
	// block on its key.
	replaced := make(chan error, 1)
	go func() { replaced <- handler.ReplaceReminderJobsForTest(h, ctx, bookingID, etID, newStart) }()

	waitForLockWaiter(t, database)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit create-side tx: %v", err)
	}
	if err := <-replaced; err != nil {
		t.Fatalf("replaceReminderJobs: %v", err)
	}

	var rows int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE type = 'reminder.send'`).Scan(&rows); err != nil {
		t.Fatalf("count reminder jobs: %v", err)
	}
	if rows != 1 {
		t.Errorf("reminder rows = %d; want 1 — the unique on (workspace_id, type, payload) "+
			"admits exactly one row per (booking, hours_before)", rows)
	}

	var gotRunAt, status string
	if err := database.QueryRowContext(ctx,
		`SELECT run_at, status FROM jobs WHERE type = 'reminder.send'`).Scan(&gotRunAt, &status); err != nil {
		t.Fatalf("read surviving reminder job: %v", err)
	}
	if gotRunAt != wantRunAt {
		t.Errorf("surviving reminder run_at = %s; want %s (the rescheduled time). "+
			"%s is the ORIGINAL time, which is what ON CONFLICT DO NOTHING leaves behind",
			gotRunAt, wantRunAt, oldRunAt)
	}
	if status != "pending" {
		t.Errorf("surviving reminder status = %q; want pending", status)
	}
}

// waitForLockWaiter blocks until the reschedule's INSERT is demonstrably queued behind the
// uncommitted key. It fails rather than returning if that never happens: without the block,
// the test would be exercising the ordinary interleaving and proving nothing.
//
// ⛔ Two signals, because `wait_event_type = 'Lock'` ALONE IS NOT ENOUGH — a `-race
// -count=30` run on a box with two other workers on it failed here 2 times in 30, always at
// this control and never at the run_at assertion. The fix was holding; the detection was not.
//
// The reason is that the Lock wait is a STATE THIS POLL HAS TO CATCH THE BACKEND IN, and
// `pg_stat_activity` is a sampled view of it. Under `-race` (several times slower on the Go
// side) plus CPU contention, the goroutine issuing the INSERT can still be inside the driver,
// or between statements of its transaction, for the whole window this loop looks at — so "not
// blocked yet" and "never going to block" are indistinguishable from that one column.
//
// ⚠️ It is NOT driver-side queueing: the PostgreSQL pool defaults to 10 open / 5 idle
// (config.PoolFromEnv) and this test uses two connections, so nothing is waiting for one. It
// is `-race` plus load, which is why the answer is a better signal rather than a bigger pool.
//
// So a backend running the reschedule's own INSERT counts too, identified by its query text.
// An `active` backend running that statement is either about to block on the key or already
// past it; either way the interleaving has been reached, which is all this control claims. 30s
// rather than 10 for the same reason: the deadline has to outlast a slow, loaded box, and it is
// only ever paid when something is genuinely wrong.
func waitForLockWaiter(t *testing.T, database *db.DB) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var waiters int
		if err := database.QueryRow(`
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND (wait_event_type = 'Lock'
			       OR (state = 'active' AND query LIKE '%ON CONFLICT (workspace_id, type, payload) WHERE status IN (''pending'', ''running'') DO UPDATE%'))`).
			Scan(&waiters); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiters > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("in 30s no backend ever blocked on a lock and none was running the reschedule's " +
		"upsert, so the losing interleaving was never reached and this test would pass " +
		"whatever ON CONFLICT does")
}
