package handler

import (
	"context"
	"log/slog"
	"testing"

	"github.com/calnode/calnode/internal/dbtest"
)

// enqueueJob's upsert has to name the jobs uniqueness index exactly, predicate
// included, or both engines refuse the statement outright. Its target said
// (type, payload) from migration 00060 on, when the index became
// (workspace_id, type, payload), so every notetaker enqueue failed; 00072 then made
// the index partial. This runs on whichever engine the suite is pointed at.
func TestEnqueueJob_upsertsLiveJobAndReenqueuesAfterFinish(t *testing.T) {
	database := dbtest.Open(t)
	h := New(database, slog.Default())
	ctx := context.Background()
	payload := map[string]string{"recording_id": "rec-1"}

	count := func(status string) int {
		t.Helper()
		var n int
		if err := database.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM jobs WHERE type = 'notetaker.transcribe' AND status = ?`, status).Scan(&n); err != nil {
			t.Fatalf("count %s jobs: %v", status, err)
		}
		return n
	}

	if err := h.enqueueJob(ctx, "notetaker.transcribe", payload); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if err := h.enqueueJob(ctx, "notetaker.transcribe", payload); err != nil {
		t.Fatalf("second enqueue of the same payload: %v", err)
	}
	if got := count("pending"); got != 1 {
		t.Fatalf("pending jobs after two enqueues = %d, want 1 (the second must upsert, not insert)", got)
	}

	// A finished job no longer blocks the same payload (00072): the retry is a new row.
	if _, err := database.ExecContext(ctx,
		`UPDATE jobs SET status = 'done' WHERE type = 'notetaker.transcribe'`); err != nil {
		t.Fatalf("finish the job: %v", err)
	}
	if err := h.enqueueJob(ctx, "notetaker.transcribe", payload); err != nil {
		t.Fatalf("enqueue after the job finished: %v", err)
	}
	if got, done := count("pending"), count("done"); got != 1 || done != 1 {
		t.Fatalf("after re-enqueue: pending=%d done=%d, want 1 and 1", got, done)
	}
}
