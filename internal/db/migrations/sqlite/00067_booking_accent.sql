-- +goose Up
-- Per-user booking accent colour. Upstream Calnode's 00060, renumbered (see 00066).
ALTER TABLE users ADD COLUMN booking_accent TEXT NOT NULL DEFAULT '#111827';

-- +goose Down
ALTER TABLE users DROP COLUMN booking_accent;
