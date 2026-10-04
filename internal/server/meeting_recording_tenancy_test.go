package server_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/config"
)

// A multi-tenant instance with MEETING_RECORDING unset has no meeting recording: the routes
// are the mux's 404 and the MCP server does not offer the notes tools. The fixture's default
// sets it "on" (so the isolation proofs keep covering that code); this case clears it, and
// the "on" fixture is the positive control.
func TestMultiTenant_meetingRecordingIsOffByDefault(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting string
		wantOff bool
	}{
		{"explicit on", "on", false},
		{"unset", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTenancyFixtureWith(t, func(c *config.Config) { c.MeetingRecording = tc.setting })

			for _, path := range []string{"/v1/recordings", "/v1/settings/notetaker", "/v1/bookings/" + f.a.bookingID + "/notes"} {
				rec := f.do(t, http.MethodGet, "app.calnode.example", path, f.a.apiKey, "")
				muxNotFound := rec.Code == http.StatusNotFound && strings.HasPrefix(rec.Body.String(), "404 page not found")
				if muxNotFound != tc.wantOff {
					t.Errorf("GET %s = %d %q; want the mux's 404: %v", path, rec.Code, firstLine(rec.Body.String()), tc.wantOff)
				}
			}

			tools, _ := f.mcpSession(t, f.a.apiKey)
			if got := slices.Contains(tools, "get_meeting_notes"); got == tc.wantOff {
				t.Errorf("MCP offers get_meeting_notes = %v; want %v (tools: %v)", got, !tc.wantOff, tools)
			}
		})
	}
}
