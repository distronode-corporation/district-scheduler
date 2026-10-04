import { writable } from 'svelte/store';
import type { User } from './api';

export const currentUser = writable<User | null>(null);

export type AuthStatus = {
	demo_mode: boolean;
	next_reset_at?: string; // RFC3339; only present when demo_mode is true
	// False when the server runs with MEETING_RECORDING=off: no recording, notetaker, or
	// stored notes and transcripts. Absent (an older server) means on.
	meeting_recording?: boolean;
};

/** Whether meeting recording and everything built on it exists on this server. */
export function meetingRecordingOn(status: AuthStatus): boolean {
	return status.meeting_recording !== false;
}

export const authStatus = writable<AuthStatus>({ demo_mode: false });
