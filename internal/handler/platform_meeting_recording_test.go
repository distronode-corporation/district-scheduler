package handler_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"

	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
)

// With MEETING_RECORDING=off the workspace delete carries no recording_object_keys. The
// switch-on half of this is TestPlatform_getPatchDelete, which asserts the seeded key IS
// reported; the platform's client reads an absent list as an empty one.
func TestPlatform_deleteReportsNoRecordingKeysWithRecordingOff(t *testing.T) {
	app, platform := dbtest.RequireTenantPair(t)

	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://cal.example.test")
	h.SetPlatformToken(platformToken)
	h.SetEncKey(platformTestEncKey)
	h.SetMeetingRecording(false)
	create := h.Platform((*handler.Handler).CreateWorkspace)
	del := h.Platform((*handler.Handler).DeleteWorkspace)

	if rec := doPlatform(t, create, http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody("quiet", "book.quiet.example"), platformToken); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d — %s", rec.Code, rec.Body.String())
	}
	// A row written before the switch went off. It still cascades with the workspace.
	if _, err := platform.Exec(`
		INSERT INTO recordings (id, workspace_id, room, egress_id, status, object_key)
		VALUES ('rec-q', 'quiet', 'booking-q', 'eg-q', 'complete', 'recordings/quiet/rec-q.mp4')`); err != nil {
		t.Fatalf("seed recording: %v", err)
	}

	rec := doPlatform(t, del, http.MethodDelete, "/v1/platform/workspaces/quiet", nil, platformToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d — %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode delete: %v", err)
	}
	if _, present := out["recording_object_keys"]; present {
		t.Errorf("delete response = %s; want no recording_object_keys with MEETING_RECORDING=off", rec.Body.String())
	}

	var n int
	if err := platform.QueryRow(`SELECT COUNT(*) FROM recordings WHERE workspace_id = 'quiet'`).Scan(&n); err != nil {
		t.Fatalf("count recordings: %v", err)
	}
	if n != 0 {
		t.Errorf("%d recordings rows survived the workspace delete; the cascade must still remove them", n)
	}
}
