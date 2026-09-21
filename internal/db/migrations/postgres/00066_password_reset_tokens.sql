-- +goose Up
-- One-time, short-lived password-reset tokens emailed to a user. We store only the
-- SHA-256 of the token (never the raw value); single-use is enforced by used_at.
-- Mirrors magic_link_tokens (00040) with a longer TTL: a reset email sits in an
-- inbox longer than a just-requested login link.
--
-- Upstream Calnode's 00058, renumbered: this fork's 00058-00065 were already taken
-- when it arrived (merge of upstream/main, 2026-09-21).
--
-- ⛔ A TENANT table, classified in db.TenantTables, exactly like magic_link_tokens:
-- workspace_id with the 00060 default, the foreign key to workspaces, and the
-- per-workspace policy. EnableRLS turns the policy on at boot in multi-tenant mode.
-- A reset token is only ever looked up on a host-resolved request (the forgot and
-- reset routes are HostWorkspace-scoped), so, unlike a bearer credential, nothing
-- needs to find it before the workspace is known.
--
-- The three timestamps are COLLATE "C" for the reason 00059 gives; collation_test.go
-- audits them by column name.
CREATE TABLE password_reset_tokens (
    token_hash   TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at   TEXT COLLATE "C" NOT NULL,
    used_at      TEXT COLLATE "C",
    created_at   TEXT COLLATE "C" NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS')),
    workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE
);

CREATE POLICY password_reset_tokens_tenant ON password_reset_tokens USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));

-- +goose Down
DROP POLICY IF EXISTS password_reset_tokens_tenant ON password_reset_tokens;
DROP TABLE password_reset_tokens;
