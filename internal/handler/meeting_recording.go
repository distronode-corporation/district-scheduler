package handler

// MEETING_RECORDING (config.MeetingRecordingEnabled) switches off everything that exists
// only because a meeting was recorded. This file is the handler half of that switch; the
// routes it removes are left unregistered in internal/server, and the checks below cover
// the paths that stay reachable with the routes gone:
//
//   - the room's join payload (recording_available), so the Record button never shows;
//   - the LiveKit webhook, which LiveKit keeps calling for every event in the project;
//     it still verifies and 200-ACKs, and acts on nothing;
//   - the notetaker jobs, so a job queued before the switch completes as a no-op;
//   - the MCP server, which does not offer get_meeting_notes or get_transcript;
//   - the workspace delete, which no longer reports recording object keys;
//   - GET /v1/auth/status, which tells the admin console to hide the surfaces.
//
// The tables (recordings, meeting_consents, transcripts, notes) are left in place, so
// export and erasure still see any row written before the switch was turned off.

// SetMeetingRecording records whether meeting recording, the notetaker and stored
// notes and transcripts are available. Call once at boot, before serving.
func (h *Handler) SetMeetingRecording(on bool) { h.meetingRecordingOff = !on }

// MeetingRecording reports whether meeting recording is available on this instance.
func (h *Handler) MeetingRecording() bool { return !h.meetingRecordingOff }
