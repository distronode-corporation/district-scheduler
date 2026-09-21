-- +goose Up
-- One-time, short-lived password-reset tokens emailed to a user. We store only the
-- SHA-256 of the token (never the raw value); single-use is enforced by used_at.
-- Mirrors magic_link_tokens (00040) with a longer TTL: a reset email sits in an
-- inbox longer than a just-requested login link.
--
-- Upstream Calnode's 00058, renumbered: this fork's 00058-00065 were already taken
-- when it arrived (merge of upstream/main, 2026-09-21). workspace_id is declared
-- here rather than by a later ALTER, for the same reason and with the same
-- SQLite-side shape as every tenant table in 00060 (no REFERENCES, see there).
CREATE TABLE password_reset_tokens (
    token_hash   TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at   TEXT NOT NULL,
    used_at      TEXT,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    workspace_id TEXT NOT NULL DEFAULT 'default'
);

-- +goose Down
DROP TABLE password_reset_tokens;
