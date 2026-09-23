-- +goose Up
-- Uploaded images: the workspace logo, the workspace banner, and each member's avatar.
-- The bytes live here, per workspace, instead of under DATA_DIR on the pod's disk.
--
-- Why a table: the files used to be written to <DATA_DIR>/branding/logo.png,
-- banner.png and avatars/<user id>.jpg. Two things were wrong with that.
--   1. The directory was not workspace-scoped, so on a multi-tenant instance tenant A's
--      upload overwrote tenant B's file, B's booking pages and emails served A's logo,
--      and A's "Remove logo" deleted B's.
--   2. The fleet runs this image with /data on an emptyDir, one replica, Recreate: every
--      restart or rollout deleted the files while server_settings.logo_url and
--      users.avatar_url still pointed at them.
-- A row under the tenant policy fixes both, and it travels with the workspace's export.
--
-- The row is keyed per (workspace, kind, owner): owner_id is '' for the two
-- workspace-wide images and the user id for an avatar, which the last CHECK pins so a
-- row can be neither an owned logo nor an ownerless avatar. It is '' rather than NULL
-- because a primary-key column cannot be NULL, and the primary key is what the upload's
-- ON CONFLICT targets. owner_id carries no foreign key for the same reason; the member
-- removal route deletes a user's avatar row itself, and the serve path refuses a row
-- whose user no longer exists.
--
-- The limits restate the upload path's (image_upload.go and the handlers): 5 MiB and
-- the four accepted types. Every stored image is re-encoded before it gets here (PNG
-- for the logo and banner, JPEG for an avatar), so today only two of the four types
-- can occur; the CHECK names all four so a later decision to store an original does
-- not need a migration. The size bound is on the ENCODED bytes, and the largest thing
-- the handlers can produce (a 1600x800 RGBA banner of pure noise) is ~5.12 MB, under
-- 5 MiB.
--
-- ⛔ A TENANT table, classified in db.TenantTables and in the export order, exactly like
-- password_reset_tokens (00066): workspace_id with the 00060 default, the foreign key to
-- workspaces, and the per-workspace policy that db.EnableRLS turns on at boot in
-- multi-tenant mode. The serve routes are HostWorkspace-scoped, so a request for
-- /branding/logo reads the row of the workspace whose public host was asked, and no
-- query in the tree names workspace_id for it.
--
-- updated_at is COLLATE "C" for the reason 00059 gives; collation_test.go audits it by
-- column name.
--
-- Nothing is imported from disk. The files this replaces are already gone on the fleet,
-- and a URL that points at a row that does not exist reads as unset (db.LogoURLSQL,
-- db.BannerURLSQL, db.AvatarURLSQL) rather than being cleared here, so this migration
-- destroys nothing and its Down is exact.
CREATE TABLE workspace_assets (
    kind         TEXT NOT NULL CHECK (kind IN ('logo', 'banner', 'avatar')),
    owner_id     TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL CHECK (content_type IN ('image/jpeg', 'image/png', 'image/gif', 'image/webp')),
    data         BYTEA NOT NULL CHECK (octet_length(data) BETWEEN 1 AND 5242880),
    sha256       TEXT NOT NULL CHECK (length(sha256) = 64),
    updated_at   TEXT COLLATE "C" NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS')),
    workspace_id TEXT NOT NULL DEFAULT COALESCE(current_setting('app.workspace_id', true), 'default') REFERENCES workspaces(id) ON DELETE CASCADE,
    PRIMARY KEY (workspace_id, kind, owner_id),
    CHECK ((kind = 'avatar') = (owner_id <> ''))
);

CREATE POLICY workspace_assets_tenant ON workspace_assets USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (workspace_id = current_setting('app.workspace_id', true));

-- +goose Down
DROP POLICY IF EXISTS workspace_assets_tenant ON workspace_assets;
DROP TABLE workspace_assets;
