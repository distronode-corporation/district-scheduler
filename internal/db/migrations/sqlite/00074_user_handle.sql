-- +goose Up
-- Public booking handle for per-person pages (/u/{handle}, upstream #94). Nullable: only
-- users who set one get a page; NULLs are distinct under UNIQUE, so any number of
-- handle-less users coexist; setting an in-use handle 409s at the API.
--
-- Upstream Calnode's 00067, renumbered (see 00069). ⛔ Unique PER WORKSPACE, not per
-- instance as upstream has it: /u/{handle} is served on a workspace's own host, and a
-- global index would let one tenant's "sean" refuse every other tenant's.
ALTER TABLE users ADD COLUMN handle TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_handle ON users (workspace_id, handle);

-- +goose Down
DROP INDEX IF EXISTS idx_users_handle;
ALTER TABLE users DROP COLUMN handle;
