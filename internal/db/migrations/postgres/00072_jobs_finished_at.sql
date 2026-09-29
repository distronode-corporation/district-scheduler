-- +goose Up
-- Two job-table fixes:
-- 1. finished_at records when a job reached a terminal state, so completed rows
--    can be purged (nothing deleted done/failed jobs; the table grew for the life
--    of the database).
-- 2. The (type, payload) uniqueness guard covered ALL statuses, so a done/failed
--    row permanently blocked re-enqueue of the same payload (e.g. retrying
--    reminders after a failure was a silent no-op). Scope it to live rows only.
--
-- Upstream Calnode's 00065, renumbered (see 00069). The guard keeps this fork's
-- leading workspace_id (00060): two workspaces may hold the same payload. finished_at
-- is COLLATE "C" for the reason 00059 gives; collation_test.go audits it by name.
ALTER TABLE jobs ADD COLUMN finished_at TEXT COLLATE "C";

DROP INDEX IF EXISTS ux_jobs_type_payload;
CREATE UNIQUE INDEX IF NOT EXISTS ux_jobs_type_payload_live
    ON jobs (workspace_id, type, payload) WHERE status IN ('pending', 'running');

-- +goose Down
DROP INDEX IF EXISTS ux_jobs_type_payload_live;
CREATE UNIQUE INDEX IF NOT EXISTS ux_jobs_type_payload
    ON jobs (workspace_id, type, payload);
ALTER TABLE jobs DROP COLUMN finished_at;
