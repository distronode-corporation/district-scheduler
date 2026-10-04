package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/config"
)

// MEETING_RECORDING=off leaves every recording, notetaker, notes and transcript route
// unregistered, and keeps the LiveKit webhook, which LiveKit calls for every event.
//
// Through the real mux, for the reason adminspa_test.go gives: what is under test is the
// registration, and a handler-level test cannot tell a 404 from the mux apart from one a
// handler wrote. Each path is asked with the switch on first, where the mux must NOT be what answers.

// recordingRoutes are the routes the switch removes, as an unauthenticated client asks
// them. With the switch on each is answered by its own handler (400, 401, or a JSON 404).
var recordingRoutes = []struct{ method, path string }{
	{http.MethodGet, "/v1/settings/storage"},
	{http.MethodPatch, "/v1/settings/storage"},
	{http.MethodGet, "/v1/settings/notetaker"},
	{http.MethodPatch, "/v1/settings/notetaker"},
	{http.MethodGet, "/v1/bookings/b1/notes"},
	{http.MethodPost, "/v1/bookings/b1/notes/regenerate"},
	{http.MethodGet, "/v1/bookings/b1/transcript"},
	{http.MethodPost, "/v1/livekit/record/start"},
	{http.MethodPost, "/v1/livekit/record/stop"},
	{http.MethodPost, "/v1/livekit/consent"},
	{http.MethodGet, "/v1/recordings"},
	{http.MethodDelete, "/v1/recordings"},
	{http.MethodGet, "/v1/recordings/r1/consent"},
	{http.MethodGet, "/v1/recordings/r1/download"},
	{http.MethodDelete, "/v1/recordings/r1"},
}

func recordingMux(t *testing.T, value string) http.Handler {
	t.Helper()
	t.Setenv("MULTI_TENANT", "")
	t.Setenv("MEETING_RECORDING", value)
	cfg := config.Load()
	cfg.BaseURL = "https://cal.example.test"
	cfg.PublicBaseURL = "https://cal.example.test"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return newAdminMux(t, cfg)
}

func serveRecordingRoute(mux http.Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// fromTheMux reports whether the answer is the ServeMux's own "no such route", as opposed
// to a 404 a handler wrote (record/start answers 404 itself when video is unconfigured,
// with a JSON body). The mux writes plain text.
func fromTheMux(rec *httptest.ResponseRecorder) bool {
	switch rec.Code {
	case http.StatusNotFound:
		return strings.HasPrefix(rec.Body.String(), "404 page not found")
	case http.StatusMethodNotAllowed:
		return true
	}
	return false
}

func TestMeetingRecording_onRegistersTheRoutes(t *testing.T) {
	mux := recordingMux(t, "") // single-tenant default: on, as upstream
	for _, rt := range recordingRoutes {
		if rec := serveRecordingRoute(mux, rt.method, rt.path); fromTheMux(rec) {
			t.Errorf("%s %s = %d %q with recording on; want the route's own answer", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

func TestMeetingRecording_offLeavesTheRoutesUnregistered(t *testing.T) {
	mux := recordingMux(t, "off")
	for _, rt := range recordingRoutes {
		rec := serveRecordingRoute(mux, rt.method, rt.path)
		if !fromTheMux(rec) {
			t.Errorf("%s %s = %d %q with MEETING_RECORDING=off; want the mux's 404", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}

	// The webhook stays: LiveKit posts every project event to it, and with recording off
	// it acknowledges them all and acts on none.
	for _, path := range []string{"/v1/livekit/webhook", "/v1/livekit/egress-webhook"} {
		if rec := serveRecordingRoute(mux, http.MethodPost, path); rec.Code != http.StatusOK {
			t.Errorf("POST %s = %d with MEETING_RECORDING=off; want 200", path, rec.Code)
		}
	}
}
