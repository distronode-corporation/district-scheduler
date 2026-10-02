package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtime"
	"github.com/calnode/calnode/internal/i18n"
	"github.com/calnode/calnode/internal/mailer"
	"github.com/disintegration/imaging"
)

// logoServePath is the served path for the workspace logo. The stored value carries a
// cache-busting ?v=<content hash> so re-uploads aren't masked by email/browser caching.
const logoServePath = "/branding/logo"

// bannerServePath is the served path for the instance banner image.
const bannerServePath = "/branding/banner"

// brandingSettings is the instance-wide brand identity used in emails and on the
// public booking/manage pages.
type brandingSettings struct {
	BusinessName  string
	LogoURL       string // served path (relative), e.g. "/branding/logo?v=123"; empty = no logo
	LogoHeight    int    // email logo height in px (pages scale up); see pageLogoHeight
	LogoOpacity   int    // 20–100; CSS opacity for a subtle logo. 100 = fully opaque
	BannerURL     string // served path (relative), e.g. "/branding/banner?v=123"; empty = no banner
	BannerOpacity int    // 20–100; CSS opacity for the banner. 100 = fully opaque
	PrivacyURL    string // operator's Privacy Policy URL (absolute http[s]); "" = hidden
	TermsURL      string // operator's Terms URL (absolute http[s]); "" = hidden
	// FallbackLocale is what a visitor sees when their browser doesn't ask for any
	// locale Calnode supports — defaults to "en". See i18n.ResolveWithFallback.
	FallbackLocale string
}

// loadBranding reads the brand identity from the singleton settings row.
func (h *Handler) loadBranding(ctx context.Context) brandingSettings {
	var b brandingSettings
	// The two image URLs are read through db.LogoURLSQL and db.BannerURLSQL, so one that
	// names an image this workspace does not have reads as unset rather than as a broken
	// <img>.
	const query = `
		SELECT COALESCE(business_name,''), ` + db.LogoURLSQL + `,
		       COALESCE(logo_height,28), COALESCE(logo_opacity,100),
		       ` + db.BannerURLSQL + `, COALESCE(banner_opacity,100),
		       COALESCE(privacy_url,''), COALESCE(terms_url,''),
		       COALESCE(fallback_locale,'en')
		FROM server_settings WHERE id = 1`
	_ = h.db.QueryRowContext(ctx, query).Scan(&b.BusinessName, &b.LogoURL, &b.LogoHeight, &b.LogoOpacity,
		&b.BannerURL, &b.BannerOpacity, &b.PrivacyURL, &b.TermsURL, &b.FallbackLocale)
	if b.LogoHeight <= 0 {
		b.LogoHeight = 28
	}
	if b.LogoOpacity <= 0 || b.LogoOpacity > 100 {
		b.LogoOpacity = 100
	}
	if b.BannerOpacity <= 0 || b.BannerOpacity > 100 {
		b.BannerOpacity = 100
	}
	if i18n.Get(b.FallbackLocale) == nil {
		b.FallbackLocale = i18n.DefaultCode
	}
	return b
}

// pageLogoHeight scales the email logo height up ~1.3× for the roomier public
// booking/manage page headers.
func pageLogoHeight(emailPx int) int {
	if emailPx <= 0 {
		emailPx = 28
	}
	return (emailPx*13 + 5) / 10
}

// validatedLegalURL trims and validates an operator-supplied legal link. Empty is
// allowed (the link is hidden). Non-empty must be an absolute http(s) URL with a host.
func validatedLegalURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	if len(raw) > 500 {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return raw, true
}

// opacityCSS turns a 20–100 percentage into a CSS opacity value ("1", "0.6", …).
func opacityCSS(pct int) string {
	if pct <= 0 || pct > 100 {
		pct = 100
	}
	return strconv.FormatFloat(float64(pct)/100, 'f', -1, 64)
}

// applyBranding stamps the instance brand identity onto booking email data so
// every outbound email carries the configured wordmark/logo. The logo is stored
// as a relative path; emails need an absolute URL, so it's prefixed with the
// public base URL here. Cheap single-row read; called once per send batch.
func (h *Handler) applyBranding(ctx context.Context, d *mailer.BookingData) {
	b := h.loadBranding(ctx)
	d.BrandName = b.BusinessName
	d.LogoHeight = b.LogoHeight
	d.LogoOpacity = b.LogoOpacity
	if strings.HasPrefix(b.LogoURL, "/") {
		d.LogoURL = h.publicURL() + b.LogoURL
	} else {
		d.LogoURL = b.LogoURL
	}
	d.BannerOpacity = b.BannerOpacity
	if strings.HasPrefix(b.BannerURL, "/") {
		d.BannerURL = h.publicURL() + b.BannerURL
	} else {
		d.BannerURL = b.BannerURL
	}
}

// GetBranding handles GET /v1/settings/branding (admin).
func (h *Handler) GetBranding(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	b := h.loadBranding(r.Context())
	h.writeJSON(w, http.StatusOK, map[string]any{
		"business_name":     b.BusinessName,
		"logo_url":          b.LogoURL,
		"logo_height":       b.LogoHeight,
		"logo_opacity":      b.LogoOpacity,
		"banner_url":        b.BannerURL,
		"banner_opacity":    b.BannerOpacity,
		"privacy_url":       b.PrivacyURL,
		"terms_url":         b.TermsURL,
		"fallback_locale":   b.FallbackLocale,
		"supported_locales": i18n.SupportedLocales(),
	})
}

// PatchBranding handles PATCH /v1/settings/branding (admin). Business name only;
// the logo is managed via the upload/delete endpoints.
func (h *Handler) PatchBranding(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		BusinessName   string `json:"business_name"`
		LogoHeight     int    `json:"logo_height"`
		LogoOpacity    int    `json:"logo_opacity"`
		BannerOpacity  int    `json:"banner_opacity"`
		PrivacyURL     string `json:"privacy_url"`
		TermsURL       string `json:"terms_url"`
		FallbackLocale string `json:"fallback_locale"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.BusinessName = strings.TrimSpace(req.BusinessName)
	if len(req.BusinessName) > 200 {
		h.writeError(w, http.StatusBadRequest, "business name is too long")
		return
	}
	privacyURL, ok := validatedLegalURL(req.PrivacyURL)
	if !ok {
		h.writeError(w, http.StatusBadRequest, "privacy policy must be a full http(s) URL")
		return
	}
	termsURL, ok := validatedLegalURL(req.TermsURL)
	if !ok {
		h.writeError(w, http.StatusBadRequest, "terms link must be a full http(s) URL")
		return
	}
	req.FallbackLocale = strings.TrimSpace(req.FallbackLocale)
	if req.FallbackLocale == "" {
		req.FallbackLocale = i18n.DefaultCode
	}
	if i18n.Get(req.FallbackLocale) == nil {
		h.writeError(w, http.StatusBadRequest, "fallback_locale is not a supported language code")
		return
	}
	// Clamp logo height to a sane range; 0/omitted falls back to the small default.
	if req.LogoHeight <= 0 {
		req.LogoHeight = 28
	}
	if req.LogoHeight < 16 {
		req.LogoHeight = 16
	}
	if req.LogoHeight > 64 {
		req.LogoHeight = 64
	}
	// Clamp opacity to 20–100; 0/omitted falls back to fully opaque.
	if req.LogoOpacity <= 0 {
		req.LogoOpacity = 100
	}
	if req.LogoOpacity < 20 {
		req.LogoOpacity = 20
	}
	if req.LogoOpacity > 100 {
		req.LogoOpacity = 100
	}
	// Clamp banner opacity to 20–100; 0/omitted falls back to fully opaque.
	if req.BannerOpacity <= 0 {
		req.BannerOpacity = 100
	}
	if req.BannerOpacity < 20 {
		req.BannerOpacity = 20
	}
	if req.BannerOpacity > 100 {
		req.BannerOpacity = 100
	}
	if _, err := h.db.ExecContext(r.Context(), `
		UPDATE server_settings SET business_name = ?, logo_height = ?, logo_opacity = ?,
		       banner_opacity = ?, privacy_url = ?, terms_url = ?, fallback_locale = ?, updated_at = ?
		WHERE id = 1`, req.BusinessName, req.LogoHeight, req.LogoOpacity, req.BannerOpacity, privacyURL, termsURL, req.FallbackLocale, dbtime.Now()); err != nil {
		h.logger.ErrorContext(r.Context(), "branding settings: update", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.GetBranding(w, r)
}

// UploadBrandingLogo handles POST /v1/settings/branding/logo (admin).
// Accepts multipart/form-data with a "logo" file field (JPEG/PNG/GIF/WebP, ≤5 MB).
// Resized to fit 600×160 preserving aspect ratio, re-encoded as PNG (keeps
// transparency), and stored in workspace_assets for the caller's workspace.
func (h *Handler) UploadBrandingLogo(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20+1024)
	if err := r.ParseMultipartForm(5 << 20); err != nil { // #nosec G120 -- bounded by the MaxBytesReader above; the body can't exceed ~5MB
		h.writeError(w, http.StatusBadRequest, "logo must be ≤5 MB")
		return
	}
	file, _, err := r.FormFile("logo")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "logo field required")
		return
	}
	defer file.Close()

	img, userMsg, err := decodeUploadedImage(file, "logo")
	if err != nil {
		h.logger.ErrorContext(r.Context(), "logo: read body", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if userMsg != "" {
		h.writeError(w, http.StatusBadRequest, userMsg)
		return
	}

	// Fit within 600×160, preserving aspect ratio; never upscale. Logos are not
	// square, so we don't crop server-side (the client does any cropping) — the
	// display height-constrains it to ~30px, and ~160px tall covers retina/larger
	// page display while keeping the file small. BestCompression trims size further.
	resized := imaging.Fit(img, 600, 160, imaging.Lanczos)
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&out, resized); err != nil {
		h.logger.ErrorContext(r.Context(), "logo: encode png", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	logoURL, ok := h.storeBrandingImage(w, r, assetLogo, "logo_url", logoServePath, out.Bytes())
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]string{"logo_url": logoURL})
}

// DeleteBrandingLogo handles DELETE /v1/settings/branding/logo (admin).
func (h *Handler) DeleteBrandingLogo(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if !h.removeBrandingImage(w, r, assetLogo, "logo_url") {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ServeBrandingLogo handles GET /branding/logo. Public — the logo is embedded in
// public pages and emails. Serves the logo of the workspace whose public host was
// asked for.
func (h *Handler) ServeBrandingLogo(w http.ResponseWriter, r *http.Request) {
	h.serveAsset(w, r, assetLogo, "", "public, max-age=86400")
}

// UploadBrandingBanner handles POST /v1/settings/branding/banner (admin).
// Accepts multipart/form-data with a "banner" file field (JPEG/PNG/GIF/WebP, ≤5 MB).
// Resized to fit 1600×800 preserving aspect ratio, re-encoded as PNG, and stored in
// workspace_assets for the caller's workspace. Unlike the logo, the banner is always
// displayed at 100% width, so it's resized larger to stay sharp at full-container width.
func (h *Handler) UploadBrandingBanner(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20+1024)
	if err := r.ParseMultipartForm(5 << 20); err != nil { // #nosec G120 -- bounded by the MaxBytesReader above; the body can't exceed ~5MB
		h.writeError(w, http.StatusBadRequest, "banner must be ≤5 MB")
		return
	}
	file, _, err := r.FormFile("banner")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "banner field required")
		return
	}
	defer file.Close()

	img, userMsg, err := decodeUploadedImage(file, "banner")
	if err != nil {
		h.logger.ErrorContext(r.Context(), "banner: read body", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if userMsg != "" {
		h.writeError(w, http.StatusBadRequest, userMsg)
		return
	}

	resized := imaging.Fit(img, 1600, 800, imaging.Lanczos)
	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&out, resized); err != nil {
		h.logger.ErrorContext(r.Context(), "banner: encode png", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	bannerURL, ok := h.storeBrandingImage(w, r, assetBanner, "banner_url", bannerServePath, out.Bytes())
	if !ok {
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]string{"banner_url": bannerURL})
}

// DeleteBrandingBanner handles DELETE /v1/settings/branding/banner (admin).
func (h *Handler) DeleteBrandingBanner(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if !h.removeBrandingImage(w, r, assetBanner, "banner_url") {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ServeBrandingBanner handles GET /branding/banner. Public — the banner is
// embedded in public pages and emails. Serves the banner of the workspace whose public
// host was asked for.
func (h *Handler) ServeBrandingBanner(w http.ResponseWriter, r *http.Request) {
	h.serveAsset(w, r, assetBanner, "", "public, max-age=86400")
}

// storeBrandingImage writes an encoded PNG as the workspace's logo or banner and points
// the settings column at it, in one transaction, so neither can exist without the other.
// It answers the request itself on failure and reports whether it succeeded.
//
// column is "logo_url" or "banner_url", a literal from the two callers above.
func (h *Handler) storeBrandingImage(w http.ResponseWriter, r *http.Request, kind, column, servePath string, png []byte) (string, bool) {
	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), kind+": begin tx", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return "", false
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit

	etag, err := putAsset(r.Context(), tx, kind, "", "image/png", png)
	if err != nil {
		h.logger.ErrorContext(r.Context(), kind+": store", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return "", false
	}
	url := assetURL(servePath, etag)
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE server_settings SET `+brandingColumn(column)+` = ?, updated_at = ? WHERE id = 1`, // #nosec G202 -- brandingColumn admits only the two literal column names
		url, dbtime.Now()); err != nil {
		h.logger.ErrorContext(r.Context(), kind+": update db", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return "", false
	}
	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(r.Context(), kind+": commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return "", false
	}
	return url, true
}

// removeBrandingImage deletes the workspace's logo or banner and clears the column that
// names it, in one transaction. It answers the request itself on failure.
func (h *Handler) removeBrandingImage(w http.ResponseWriter, r *http.Request, kind, column string) bool {
	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), kind+": delete begin tx", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit

	if err := deleteAsset(r.Context(), tx, kind, ""); err != nil {
		h.logger.ErrorContext(r.Context(), kind+": delete asset", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE server_settings SET `+brandingColumn(column)+` = '', updated_at = ? WHERE id = 1`, // #nosec G202 -- brandingColumn admits only the two literal column names
		dbtime.Now()); err != nil {
		h.logger.ErrorContext(r.Context(), kind+": delete db", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(r.Context(), kind+": delete commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}
	return true
}

// brandingColumn is the allowlist for the one identifier storeBrandingImage and
// removeBrandingImage interpolate. Anything else is a programming error.
func brandingColumn(column string) string {
	switch column {
	case "logo_url", "banner_url":
		return column
	}
	panic("handler: not a branding image column: " + column)
}
