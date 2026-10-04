package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/livekit"
)

// MEETING_RECORDING=off, at the handler: the paths that stay reachable once the routes are
// gone. Every case runs the same input with the switch on first, as the positive control: a
// check that passes with the switch off but would also pass with it on proves nothing.

const recordingTestSecret = "recording-test-secret"

// newRecordingHandler is a single-tenant handler with a LiveKit client whose webhook
// secret the test knows, so a signed event verifies.
func newRecordingHandler(t *testing.T) (*Handler, *db.DB) {
	t.Helper()
	database := dbtest.Open(t)
	h := New(database, slog.New(slog.DiscardHandler))
	// Before any recording row is seeded: SetLiveKit sweeps 'active' rows to 'complete'.
	h.SetLiveKit(livekit.New("https://lk.example.test", "lk-key", recordingTestSecret, [32]byte{}))
	return h, database
}

// signLiveKitWebhook mints the Authorization value LiveKit sends: an HS256 JWT over the
// body's SHA-256, with a fresh expiry. Mirrors mintWebhookToken in internal/livekit.
func signLiveKitWebhook(body []byte) string {
	sum := sha256.Sum256(body)
	now := time.Now().Unix()
	claims, _ := json.Marshal(map[string]any{
		"sha256": base64.StdEncoding.EncodeToString(sum[:]),
		"iat":    now,
		"exp":    now + 300,
	})
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	enc := base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, []byte(recordingTestSecret))
	mac.Write([]byte(head + "." + enc))
	return head + "." + enc + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func recordingStatus(t *testing.T, database *db.DB, id string) string {
	t.Helper()
	var status string
	if err := database.QueryRow(`SELECT status FROM recordings WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("read recording %s: %v", id, err)
	}
	return status
}

// A signed egress_ended finalizes the recording with the switch on, and with it off the
// same signed event is acknowledged and changes nothing.
func TestMeetingRecordingOff_livekitWebhookAcksAndActsOnNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		on         bool
		wantStatus string
	}{
		{"on finalizes", true, "complete"},
		{"off ignores", false, "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, database := newRecordingHandler(t)
			h.SetMeetingRecording(tc.on)
			if _, err := database.Exec(`
				INSERT INTO recordings (id, room, egress_id, status, object_key)
				VALUES ('rec-1', 'booking-x', 'eg-1', 'active', 'recordings/booking-x/a.mp4')`); err != nil {
				t.Fatalf("seed recording: %v", err)
			}

			body, _ := json.Marshal(map[string]any{
				"event": "egress_ended",
				"egressInfo": map[string]any{
					"egressId": "eg-1",
					"roomName": "booking-x",
					"status":   "EGRESS_COMPLETE",
					"fileResults": []map[string]any{
						{"filename": "recordings/booking-x/a.mp4", "duration": "60000000000"},
					},
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/livekit/webhook", bytes.NewReader(body))
			req.Header.Set("Authorization", signLiveKitWebhook(body))
			rec := httptest.NewRecorder()
			h.LiveKitWebhook(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200 either way — %s", rec.Code, rec.Body.String())
			}
			if got := recordingStatus(t, database, "rec-1"); got != tc.wantStatus {
				t.Errorf("recording status = %q; want %q", got, tc.wantStatus)
			}
		})
	}
}

// The room's join payload: with recording enabled for the workspace and storage configured,
// the Record button shows only while the instance switch is on.
func TestMeetingRecordingOff_roomNeverOffersRecord(t *testing.T) {
	t.Setenv("LITESTREAM_REPLICA_URL", "s3://bucket/db")
	t.Setenv("LITESTREAM_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("LITESTREAM_SECRET_ACCESS_KEY", "secret")

	h, database := newRecordingHandler(t)
	if _, err := database.Exec(`UPDATE server_settings SET recordings_enabled = 1 WHERE id = 1`); err != nil {
		t.Fatalf("enable recordings: %v", err)
	}
	ctx := context.Background()

	if !h.recordingAvailable(ctx) {
		t.Fatal("recordingAvailable = false with the switch on, recordings enabled and storage set; " +
			"the positive control failed, so the off case below would prove nothing")
	}
	h.SetMeetingRecording(false)
	if h.recordingAvailable(ctx) {
		t.Error("recordingAvailable = true with MEETING_RECORDING=off")
	}
}

// A notetaker job queued before the switch went off completes as a no-op. A malformed
// payload is the probe: on, it is an error the worker retries; off, nothing is read.
func TestMeetingRecordingOff_notetakerJobsAreNoOps(t *testing.T) {
	h, _ := newRecordingHandler(t)
	ctx := context.Background()

	if err := h.JobNotetakerTranscribe(ctx, "default", "not json"); err == nil {
		t.Fatal("transcribe with the switch on accepted a malformed payload; the positive control failed")
	}
	if err := h.JobNotetakerSummarize(ctx, "default", "not json"); err == nil {
		t.Fatal("summarize with the switch on accepted a malformed payload; the positive control failed")
	}

	h.SetMeetingRecording(false)
	if err := h.JobNotetakerTranscribe(ctx, "default", "not json"); err != nil {
		t.Errorf("transcribe with MEETING_RECORDING=off = %v; want nil (dropped)", err)
	}
	if err := h.JobNotetakerSummarize(ctx, "default", "not json"); err != nil {
		t.Errorf("summarize with MEETING_RECORDING=off = %v; want nil (dropped)", err)
	}
}
