-- +goose Up
-- Deleting a user with booking history failed with a foreign-key violation
-- (surfaced as a 500): booking_hosts.user_id referenced users(id) with no
-- ON DELETE action, and the DeleteUser guard only checked bookings.host_id, so
-- even a past group attendee blocked the delete. The MCP OAuth tables never
-- referenced users at all, so a deleted user's bearer tokens stayed valid.
-- SQLite can't ALTER a constraint, so recreate the tables with
-- ON DELETE CASCADE (same approach as 00014).
--
-- Upstream Calnode's 00063, renumbered (see 00069). ⛔ The rebuilt tables carry this
-- fork's workspace_id (00060) as well as every upstream column: upstream's column
-- list alone would drop it and put every row in no workspace. Tokens of users that no
-- longer exist are deleted first, since the new foreign key would refuse to copy them
-- and they are exactly the tokens this migration exists to kill.
DELETE FROM oauth_auth_codes WHERE user_id NOT IN (SELECT id FROM users);
DELETE FROM oauth_access_tokens WHERE user_id NOT IN (SELECT id FROM users);

CREATE TABLE booking_hosts_new (
    id                   TEXT PRIMARY KEY,
    booking_id           TEXT NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
    user_id              TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    is_primary           INTEGER NOT NULL DEFAULT 0,
    external_event_id    TEXT,
    needs_sync           INTEGER NOT NULL DEFAULT 0,
    external_calendar_id TEXT,
    workspace_id         TEXT NOT NULL DEFAULT 'default',
    external_provider    TEXT NOT NULL DEFAULT '',
    UNIQUE(booking_id, user_id)
);
INSERT INTO booking_hosts_new
    (id, booking_id, user_id, is_primary, external_event_id, needs_sync,
     external_calendar_id, workspace_id, external_provider)
    SELECT id, booking_id, user_id, is_primary, external_event_id, needs_sync,
           external_calendar_id, workspace_id, external_provider
    FROM booking_hosts;
DROP TABLE booking_hosts;
ALTER TABLE booking_hosts_new RENAME TO booking_hosts;
CREATE INDEX idx_booking_hosts_booking ON booking_hosts(booking_id);
CREATE INDEX idx_booking_hosts_user ON booking_hosts(user_id);

CREATE TABLE oauth_auth_codes_new (
    code_hash      TEXT PRIMARY KEY,
    client_id      TEXT NOT NULL,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    redirect_uri   TEXT NOT NULL,
    code_challenge TEXT NOT NULL,
    scope          TEXT NOT NULL DEFAULT '',
    resource       TEXT NOT NULL DEFAULT '',
    expires_at     TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    workspace_id   TEXT NOT NULL DEFAULT 'default'
);
INSERT INTO oauth_auth_codes_new
    (code_hash, client_id, user_id, redirect_uri, code_challenge, scope,
     resource, expires_at, created_at, workspace_id)
    SELECT code_hash, client_id, user_id, redirect_uri, code_challenge, scope,
           resource, expires_at, created_at, workspace_id
    FROM oauth_auth_codes;
DROP TABLE oauth_auth_codes;
ALTER TABLE oauth_auth_codes_new RENAME TO oauth_auth_codes;

CREATE TABLE oauth_access_tokens_new (
    id           TEXT PRIMARY KEY,
    token_hash   TEXT NOT NULL UNIQUE,
    refresh_hash TEXT UNIQUE,
    client_id    TEXT NOT NULL,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    scope        TEXT NOT NULL DEFAULT '',
    resource     TEXT NOT NULL DEFAULT '',
    expires_at   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    last_used_at TEXT,
    workspace_id TEXT NOT NULL DEFAULT 'default'
);
INSERT INTO oauth_access_tokens_new
    (id, token_hash, refresh_hash, client_id, user_id, scope, resource,
     expires_at, created_at, last_used_at, workspace_id)
    SELECT id, token_hash, refresh_hash, client_id, user_id, scope, resource,
           expires_at, created_at, last_used_at, workspace_id
    FROM oauth_access_tokens;
DROP TABLE oauth_access_tokens;
ALTER TABLE oauth_access_tokens_new RENAME TO oauth_access_tokens;
CREATE INDEX idx_oauth_tokens_user ON oauth_access_tokens(user_id);
CREATE INDEX idx_oauth_tokens_refresh ON oauth_access_tokens(refresh_hash);

-- +goose Down
-- Rebuild without the CASCADE rules (data-preserving; orphans stay orphaned).
CREATE TABLE booking_hosts_old (
    id                   TEXT PRIMARY KEY,
    booking_id           TEXT NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
    user_id              TEXT NOT NULL REFERENCES users(id),
    is_primary           INTEGER NOT NULL DEFAULT 0,
    external_event_id    TEXT,
    needs_sync           INTEGER NOT NULL DEFAULT 0,
    external_calendar_id TEXT,
    workspace_id         TEXT NOT NULL DEFAULT 'default',
    external_provider    TEXT NOT NULL DEFAULT '',
    UNIQUE(booking_id, user_id)
);
INSERT INTO booking_hosts_old
    (id, booking_id, user_id, is_primary, external_event_id, needs_sync,
     external_calendar_id, workspace_id, external_provider)
    SELECT id, booking_id, user_id, is_primary, external_event_id, needs_sync,
           external_calendar_id, workspace_id, external_provider
    FROM booking_hosts;
DROP TABLE booking_hosts;
ALTER TABLE booking_hosts_old RENAME TO booking_hosts;
CREATE INDEX idx_booking_hosts_booking ON booking_hosts(booking_id);
CREATE INDEX idx_booking_hosts_user ON booking_hosts(user_id);

CREATE TABLE oauth_auth_codes_old (
    code_hash      TEXT PRIMARY KEY,
    client_id      TEXT NOT NULL,
    user_id        TEXT NOT NULL,
    redirect_uri   TEXT NOT NULL,
    code_challenge TEXT NOT NULL,
    scope          TEXT NOT NULL DEFAULT '',
    resource       TEXT NOT NULL DEFAULT '',
    expires_at     TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    workspace_id   TEXT NOT NULL DEFAULT 'default'
);
INSERT INTO oauth_auth_codes_old SELECT * FROM oauth_auth_codes;
DROP TABLE oauth_auth_codes;
ALTER TABLE oauth_auth_codes_old RENAME TO oauth_auth_codes;

CREATE TABLE oauth_access_tokens_old (
    id           TEXT PRIMARY KEY,
    token_hash   TEXT NOT NULL UNIQUE,
    refresh_hash TEXT UNIQUE,
    client_id    TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    scope        TEXT NOT NULL DEFAULT '',
    resource     TEXT NOT NULL DEFAULT '',
    expires_at   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    last_used_at TEXT,
    workspace_id TEXT NOT NULL DEFAULT 'default'
);
INSERT INTO oauth_access_tokens_old SELECT * FROM oauth_access_tokens;
DROP TABLE oauth_access_tokens;
ALTER TABLE oauth_access_tokens_old RENAME TO oauth_access_tokens;
CREATE INDEX idx_oauth_tokens_user ON oauth_access_tokens(user_id);
CREATE INDEX idx_oauth_tokens_refresh ON oauth_access_tokens(refresh_hash);
