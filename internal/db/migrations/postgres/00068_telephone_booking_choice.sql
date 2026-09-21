-- +goose Up
-- An optional phone call offered beside an online location, and the location the
-- booker actually chose. Upstream Calnode's 00061, renumbered (see 00066).
-- SMALLINT for the flag, as every flag in this schema is on PostgreSQL (see 00063).
ALTER TABLE event_types ADD COLUMN allow_phone_call SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE bookings ADD COLUMN location_type TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE bookings DROP COLUMN location_type;
ALTER TABLE event_types DROP COLUMN allow_phone_call;
