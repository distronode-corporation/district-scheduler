-- +goose Up
-- RSVPs for invites Calnode sends itself (invite_delivery = 'calnode', migration 00076).
--
-- A booker's Yes/No/Maybe is an iTIP REPLY that their mail client emails to the invite's
-- ORGANIZER. With Resend's inbound email configured, that organizer is a per-booking reply
-- address (rsvp+<random token>@<rsvp domain>); Resend receives the reply, calls
-- POST /v1/email/inbound/resend, and Calnode records the answer on the booking.
--
-- Upstream Calnode's 00069, renumbered (see 00076). rsvp_at is COLLATE "C" for the reason
-- 00059 gives; collation_test.go audits it by name.

-- The inbound address RSVPs are routed through (e.g. rsvp@reply.example.com). Empty = RSVP
-- tracking off: Calnode-sent invites are organized by the plain email sender, as before.
ALTER TABLE server_settings ADD COLUMN rsvp_address TEXT NOT NULL DEFAULT '';
-- Signing secret of the Resend webhook ("whsec_..."), encrypted like the other email secrets.
ALTER TABLE server_settings ADD COLUMN resend_webhook_secret_enc TEXT NOT NULL DEFAULT '';

-- The ORGANIZER address a booking's invite went out with, fixed at the first send. Calendar
-- clients match a later reschedule or cancellation by UID and organizer, so it must never
-- change mid-lifecycle - not when the sender address is edited, and not when RSVP tracking
-- is switched on after the booking was made. A per-booking reply address is also how an
-- incoming RSVP finds its booking. Empty for bookings whose invite Calnode never sent.
ALTER TABLE bookings ADD COLUMN invite_organizer TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_bookings_invite_organizer ON bookings (invite_organizer)
    WHERE invite_organizer <> '';
-- Upstream's 00069 also backfills invite_organizer for bookings already invited by Calnode
-- (invite_delivery = 'calnode') from the singleton server_settings row. It is left out here,
-- deliberately: on this fork 00076 and 00077 ship together, so no booking can have been
-- created with invite_delivery = 'calnode' before this runs and the UPDATE would match
-- nothing. It would also be wrong as written: server_settings holds one id = 1 row PER
-- WORKSPACE since 00060, so the uncorrelated subquery matches several rows: an error on
-- PostgreSQL, and an arbitrary workspace's sender on SQLite.

-- The booker's latest answer, iCalendar PARTSTAT values in lower case.
ALTER TABLE booking_attendees ADD COLUMN rsvp_status TEXT NOT NULL DEFAULT 'needs-action'
    CHECK (rsvp_status IN ('needs-action', 'accepted', 'declined', 'tentative'));
ALTER TABLE booking_attendees ADD COLUMN rsvp_at TEXT COLLATE "C";

-- +goose Down
DROP INDEX IF EXISTS idx_bookings_invite_organizer;
ALTER TABLE booking_attendees DROP COLUMN rsvp_at;
ALTER TABLE booking_attendees DROP COLUMN rsvp_status;
ALTER TABLE bookings DROP COLUMN invite_organizer;
ALTER TABLE server_settings DROP COLUMN resend_webhook_secret_enc;
ALTER TABLE server_settings DROP COLUMN rsvp_address;
