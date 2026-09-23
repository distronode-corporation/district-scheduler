package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/calnode/calnode/internal/dbtime"
)

// Uploaded images (the workspace logo and banner, and each member's avatar) are stored
// in workspace_assets, one row per (workspace, kind, owner), and served from it.
// Migration 00069 has the history: they used to be files under DATA_DIR, which on a
// multi-tenant instance were shared by every tenant and on the fleet lived on an
// emptyDir that every rollout wiped.
//
// The table is a tenant table and every route that touches it is Scoped, so nothing here
// names a workspace: the upload and delete routes are CredentialWorkspace-scoped and
// write the caller's workspace, the serve routes are HostWorkspace-scoped and read the
// workspace whose public host was asked for. On PostgreSQL in multi-tenant mode that is
// enforced by the row-level-security policy, not by a WHERE clause here.

const (
	assetLogo   = "logo"
	assetBanner = "banner"
	assetAvatar = "avatar"

	// maxAssetBytes bounds what is STORED, and restates the upload limit: every image
	// is decoded and re-encoded before it is stored, and the largest encoding the
	// handlers can produce (a 1600x800 RGBA PNG of noise, ~5.12 MB) fits under it. The
	// migration's CHECK holds the same number, so a write that got past this check
	// would still be refused.
	maxAssetBytes = 5 << 20
)

// assetContentTypes are the types an asset may be stored and served as: the four the
// upload path accepts. The migration's CHECK holds the same list.
var assetContentTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// errInvalidAsset is returned by putAsset for an asset that breaks one of the limits.
// Every caller has already enforced them on the upload, so reaching it is a bug, and
// callers report it as one (500) rather than blaming the uploader.
var errInvalidAsset = errors.New("handler: invalid asset")

// putAsset stores (or replaces) one image and returns its SHA-256 in hex, which is both
// the ETag it is served with and, shortened, the cache-buster in the URL that names it.
// ex is a *db.Tx in every caller (see execer in email_settings.go), so the image and the
// URL that names it are written together.
func putAsset(ctx context.Context, ex execer, kind, ownerID, contentType string, data []byte) (string, error) {
	switch {
	case kind != assetLogo && kind != assetBanner && kind != assetAvatar:
		return "", fmt.Errorf("%w: kind %q", errInvalidAsset, kind)
	case (kind == assetAvatar) != (ownerID != ""):
		return "", fmt.Errorf("%w: kind %q with owner %q", errInvalidAsset, kind, ownerID)
	case !assetContentTypes[contentType]:
		return "", fmt.Errorf("%w: content type %q", errInvalidAsset, contentType)
	case len(data) == 0 || len(data) > maxAssetBytes:
		return "", fmt.Errorf("%w: %d bytes", errInvalidAsset, len(data))
	}
	sum := sha256.Sum256(data)
	etag := hex.EncodeToString(sum[:])
	if _, err := ex.ExecContext(ctx, `
		INSERT INTO workspace_assets (kind, owner_id, content_type, data, sha256, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (workspace_id, kind, owner_id) DO UPDATE SET
		    content_type = excluded.content_type,
		    data         = excluded.data,
		    sha256       = excluded.sha256,
		    updated_at   = excluded.updated_at`,
		kind, ownerID, contentType, data, etag, dbtime.Now()); err != nil {
		return "", fmt.Errorf("store %s asset: %w", kind, err)
	}
	return etag, nil
}

// deleteAsset removes one image. Deleting one that does not exist is not an error: the
// remove routes are idempotent, as they were when they removed a file.
func deleteAsset(ctx context.Context, ex execer, kind, ownerID string) error {
	if _, err := ex.ExecContext(ctx,
		`DELETE FROM workspace_assets WHERE kind = ? AND owner_id = ?`, kind, ownerID); err != nil {
		return fmt.Errorf("delete %s asset: %w", kind, err)
	}
	return nil
}

// assetURL is the relative URL an asset is served at, with a cache-buster taken from its
// content: a re-upload of different bytes is a different URL, so neither a browser nor
// an email client's image proxy keeps showing the old one. The path is what matters to
// the router; the query is ignored by the serve handlers.
func assetURL(path, etag string) string {
	return path + "?v=" + etag[:16]
}

// serveAsset writes one stored image, or 404 when this workspace has none.
//
// ETag is the content's SHA-256, so a conditional GET is answered 304 by ServeContent
// without sending the bytes again. nosniff because the Content-Type is ours and must be
// believed: the bytes were re-encoded by this server and are exactly that type.
//
// An avatar is served only while its user still exists in this workspace. Member removal
// deletes the row as well, so this is the second of two guards: a removed member's
// photo is not public just because a row outlived them.
func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request, kind, ownerID, cacheControl string) {
	var contentType, etag string
	var data []byte
	err := h.db.QueryRowContext(r.Context(), `
		SELECT a.content_type, a.data, a.sha256
		FROM workspace_assets a
		WHERE a.kind = ? AND a.owner_id = ?
		  AND (a.owner_id = '' OR EXISTS (
		        SELECT 1 FROM users u WHERE u.id = a.owner_id AND u.workspace_id = a.workspace_id))`,
		kind, ownerID).Scan(&contentType, &data, &etag)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "serve asset: read", "kind", kind, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A zero modtime: ServeContent then sets no Last-Modified and answers conditionals
	// from the ETag alone, which is the validator that actually tracks the content.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}
