-- +goose Up
-- Uploaded images: the workspace logo, the workspace banner, and each member's avatar.
-- The bytes live here instead of under DATA_DIR. The PostgreSQL file carries the full
-- reasoning; this is the same table in SQLite's spelling, with workspace_id declared
-- here and no REFERENCES workspaces, the same shape as every tenant table in 00060 (see
-- there). BLOB for BYTEA, and length() on a BLOB counts bytes.
--
-- Nothing is imported from disk, and nothing is cleared: a stored URL that points at a
-- row that does not exist reads as unset. So a single-tenant instance that kept its
-- files on a persistent volume shows no logo, banner or avatar after this upgrade until
-- they are uploaded again, rather than showing a broken image.
CREATE TABLE workspace_assets (
    kind         TEXT NOT NULL CHECK (kind IN ('logo', 'banner', 'avatar')),
    owner_id     TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL CHECK (content_type IN ('image/jpeg', 'image/png', 'image/gif', 'image/webp')),
    data         BLOB NOT NULL CHECK (length(data) BETWEEN 1 AND 5242880),
    sha256       TEXT NOT NULL CHECK (length(sha256) = 64),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
    workspace_id TEXT NOT NULL DEFAULT 'default',
    PRIMARY KEY (workspace_id, kind, owner_id),
    CHECK ((kind = 'avatar') = (owner_id <> ''))
);

-- +goose Down
DROP TABLE workspace_assets;
