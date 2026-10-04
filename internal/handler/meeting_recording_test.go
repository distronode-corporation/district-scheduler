package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// MEETING_RECORDING=off, as a client sees it: the MCP server and the auth status.

func TestMeetingRecordingOff_mcpOmitsTheNotesAndTranscriptTools(t *testing.T) {
	h, _, _ := setupWorkspace(t)

	names := func() []string {
		t.Helper()
		res, err := connectMCP(t, h).ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		var out []string
		for _, tool := range res.Tools {
			out = append(out, tool.Name)
		}
		return out
	}

	on := names()
	for _, tool := range []string{"get_meeting_notes", "get_transcript", "list_bookings"} {
		if !slices.Contains(on, tool) {
			t.Fatalf("with the switch on, %s is missing from %v; the positive control failed", tool, on)
		}
	}

	h.SetMeetingRecording(false)
	off := names()
	for _, tool := range []string{"get_meeting_notes", "get_transcript"} {
		if slices.Contains(off, tool) {
			t.Errorf("with MEETING_RECORDING=off the MCP server still offers %s", tool)
		}
	}
	if !slices.Contains(off, "list_bookings") {
		t.Errorf("with MEETING_RECORDING=off the booking tools went too: %v", off)
	}
}

func TestMeetingRecordingOff_authStatusSaysSo(t *testing.T) {
	h, _, _ := setupWorkspace(t)

	read := func() map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		h.AuthStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/auth/status", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("auth status = %d — %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	if got := read()["meeting_recording"]; got != true {
		t.Errorf("meeting_recording = %v with the switch on; want true", got)
	}
	h.SetMeetingRecording(false)
	if got := read()["meeting_recording"]; got != false {
		t.Errorf("meeting_recording = %v with MEETING_RECORDING=off; want false", got)
	}
}
