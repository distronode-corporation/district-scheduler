package config_test

import (
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/config"
)

// MEETING_RECORDING switches meeting recording, the notetaker and the stored notes and
// transcripts off. Unset, it follows the mode: a single-tenant instance keeps upstream's
// behaviour, and a multi-tenant one has none of it.

func TestMeetingRecordingEnabled_defaultFollowsTheMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		multiTenant string
		want        bool
	}{
		{"single-tenant keeps upstream's recording", "", true},
		{"multi-tenant is off by default", "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEETING_RECORDING", "")
			t.Setenv("MULTI_TENANT", tc.multiTenant)
			cfg := config.Load()
			if got := cfg.MeetingRecordingEnabled(); got != tc.want {
				t.Errorf("MeetingRecordingEnabled() = %v with MULTI_TENANT=%q and MEETING_RECORDING unset; want %v",
					got, tc.multiTenant, tc.want)
			}
		})
	}
}

func TestMeetingRecordingEnabled_anExplicitValueWinsInEitherMode(t *testing.T) {
	for _, tc := range []struct {
		value       string
		multiTenant string
		want        bool
	}{
		{"off", "", false},
		{" OFF ", "", false}, // trimmed and case-insensitive, like ADMIN_SPA
		{"on", "", true},
		{"on", "1", true},
		{"off", "1", false},
	} {
		t.Run(tc.value+"/mt="+tc.multiTenant, func(t *testing.T) {
			t.Setenv("MEETING_RECORDING", tc.value)
			t.Setenv("MULTI_TENANT", tc.multiTenant)
			cfg := config.Load()
			if got := cfg.MeetingRecordingEnabled(); got != tc.want {
				t.Errorf("MeetingRecordingEnabled() = %v for MEETING_RECORDING=%q; want %v", got, tc.value, tc.want)
			}
		})
	}
}

// ⛔ Anything that is neither on nor off refuses the boot. The fallback is the per-mode
// default, so on a single-tenant instance an unreadable value would keep recording on
// for an operator who wrote the variable to turn it off.
func TestValidate_rejectsAnUnreadableMeetingRecording(t *testing.T) {
	for _, value := range []string{"maybe", "true", "false", "1", "0", "disabled"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("MULTI_TENANT", "")
			t.Setenv("MEETING_RECORDING", value)
			err := config.Load().Validate()
			if err == nil {
				t.Fatalf("Validate() = nil for MEETING_RECORDING=%q; want a refusal", value)
			}
			if !strings.Contains(err.Error(), "MEETING_RECORDING") {
				t.Errorf("Validate() = %v; the message must name the variable", err)
			}
		})
	}
}
