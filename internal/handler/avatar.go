package handler

import (
	"bytes"
	"image/jpeg"
	"net/http"
	"regexp"

	"github.com/disintegration/imaging"
)

// validUserID bounds the path segment ServeAvatar looks up. User ids are lowercase hex
// UUIDs. The id is a bind parameter, so this is no longer a path-traversal guard (it was
// one while avatars were files); it keeps an arbitrary string from reaching the query.
var validUserID = regexp.MustCompile(`^[0-9a-f-]{1,64}$`)

// avatarServePath is the served path for a member's avatar; the user id follows it.
const avatarServePath = "/avatars/"

// UploadAvatar handles POST /v1/users/me/avatar.
// Accepts multipart/form-data with an "avatar" file field (JPEG/PNG/GIF/WebP, ≤5 MB).
// The image is decoded, resized to at most 400×400, re-encoded as JPEG 88%, and stored
// in workspace_assets for the caller's workspace.
func (h *Handler) UploadAvatar(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20+1024)

	if err := r.ParseMultipartForm(5 << 20); err != nil { // #nosec G120 -- bounded by the MaxBytesReader above; the body can't exceed ~5MB
		h.writeError(w, http.StatusBadRequest, "avatar must be ≤5 MB")
		return
	}

	file, _, err := r.FormFile("avatar")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "avatar field required")
		return
	}
	defer file.Close()

	// Shared with the branding uploaders, which is the point: this endpoint is the one
	// that most needs the dimension guard, because it is the only image upload NOT
	// restricted to admins - any authenticated member can reach it.
	img, userMsg, err := decodeUploadedImage(file, "avatar")
	if err != nil {
		h.logger.ErrorContext(r.Context(), "avatar: read body", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if userMsg != "" {
		h.writeError(w, http.StatusBadRequest, userMsg)
		return
	}

	// Fit within 400×400, preserving aspect ratio; never upscale.
	resized := imaging.Fit(img, 400, 400, imaging.Lanczos)

	// Encode to JPEG.
	var out bytes.Buffer
	if err := jpeg.Encode(&out, resized, &jpeg.Options{Quality: 88}); err != nil {
		h.logger.ErrorContext(r.Context(), "avatar: encode jpeg", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// The image and the URL that names it are written together, so neither can exist
	// without the other.
	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "avatar: begin tx", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit

	etag, err := putAsset(r.Context(), tx, assetAvatar, user.ID, "image/jpeg", out.Bytes())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "avatar: store", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	avatarURL := assetURL(avatarServePath+user.ID, etag)
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE users SET avatar_url = ? WHERE id = ?`, avatarURL, user.ID); err != nil {
		h.logger.ErrorContext(r.Context(), "avatar: update db", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(r.Context(), "avatar: commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.writeJSON(w, http.StatusOK, map[string]string{"avatar_url": avatarURL})
}

// DeleteAvatar handles DELETE /v1/users/me/avatar.
func (h *Handler) DeleteAvatar(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r.Context())
	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "avatar: delete begin tx", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit

	if err := deleteAsset(r.Context(), tx, assetAvatar, user.ID); err != nil {
		h.logger.ErrorContext(r.Context(), "avatar: delete asset", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE users SET avatar_url = NULL WHERE id = ?`, user.ID); err != nil {
		h.logger.ErrorContext(r.Context(), "avatar: delete db", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(r.Context(), "avatar: delete commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ServeAvatar handles GET /avatars/{userID}. Public — no auth required; avatars are shown
// on the public booking pages. Reads the avatar of that user in the workspace whose
// public host was asked for.
func (h *Handler) ServeAvatar(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("userID")
	if !validUserID.MatchString(userID) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.serveAsset(w, r, assetAvatar, userID, "private, max-age=86400")
}
