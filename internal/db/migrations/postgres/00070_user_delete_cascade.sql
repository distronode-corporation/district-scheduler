-- +goose Up
-- Deleting a user with booking history failed with a foreign-key violation
-- (surfaced as a 500): booking_hosts.user_id referenced users(id) with no
-- ON DELETE action, and the DeleteUser guard only checked bookings.host_id, so
-- even a past group attendee blocked the delete. The MCP OAuth tables never
-- referenced users at all, so a deleted user's bearer tokens stayed valid.
--
-- Upstream Calnode's 00063, renumbered (see 00069). PostgreSQL alters the constraints
-- in place instead of rebuilding the tables, so workspace_id, the per-workspace
-- policies and the 00059 collations are untouched.
--
-- ⛔ Tokens of users that no longer exist are deleted first: ADD CONSTRAINT validates
-- every row, and foreign-key validation BYPASSES row-level security, so one orphan in
-- any workspace would fail the migration in every region. They are exactly the tokens
-- this migration exists to kill. The DELETE sees every workspace because migrations run
-- on the platform handle, which carries BYPASSRLS (see 00065).
DELETE FROM oauth_auth_codes WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = oauth_auth_codes.user_id);
DELETE FROM oauth_access_tokens WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = oauth_access_tokens.user_id);

ALTER TABLE booking_hosts DROP CONSTRAINT booking_hosts_user_id_fkey;
ALTER TABLE booking_hosts ADD CONSTRAINT booking_hosts_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE oauth_auth_codes ADD CONSTRAINT oauth_auth_codes_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE oauth_access_tokens ADD CONSTRAINT oauth_access_tokens_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE oauth_access_tokens DROP CONSTRAINT oauth_access_tokens_user_id_fkey;
ALTER TABLE oauth_auth_codes DROP CONSTRAINT oauth_auth_codes_user_id_fkey;
ALTER TABLE booking_hosts DROP CONSTRAINT booking_hosts_user_id_fkey;
ALTER TABLE booking_hosts ADD CONSTRAINT booking_hosts_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id);
