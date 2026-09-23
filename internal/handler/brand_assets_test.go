package handler_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/uid"
)

// Uploaded images are rows in workspace_assets (migration 00069), not files under
// DATA_DIR. These run on whichever engine the environment selects (dbtest.Open), so the
// SQLite and PostgreSQL lanes both prove the round trip. The multi-tenant half, two
// workspaces on two hosts through the real mux, is in internal/server's tenancy tests,
// because only a NOBYPASSRLS role proves isolation.

// solidPNG encodes a w×h PNG of one colour, so two uploads can be told apart by pixel.
func solidPNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// uploadForm builds a multipart request carrying one file field.
func uploadForm(t *testing.T, path, field string, data []byte, apiKey string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(field, "upload.png")
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("multipart write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, path, &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("X-API-Key", apiKey)
	return r
}

func serve(t *testing.T, fn http.HandlerFunc, path string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	fn(rec, r)
	return rec
}

func getBrandingURLs(t *testing.T, h *handler.Handler, apiKey string) (logo, banner string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RequireAuth(h.GetBranding)(rec, authReq(http.MethodGet, "/v1/settings/branding", "", apiKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET branding: %d — %s", rec.Code, rec.Body.String())
	}
	var body struct {
		LogoURL   string `json:"logo_url"`
		BannerURL string `json:"banner_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode branding: %v", err)
	}
	return body.LogoURL, body.BannerURL
}

func countAssets(t *testing.T, database *db.DB) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM workspace_assets`).Scan(&n); err != nil {
		t.Fatalf("count assets: %v", err)
	}
	return n
}

var versionedURL = regexp.MustCompile(`\?v=[0-9a-f]{16}$`)

// The whole life of a workspace image, for both kinds: upload, serve with the headers a
// cache needs, a conditional GET, a re-upload that changes the URL, remove, 404.
func TestBrandingImages_roundTrip(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		upload func(*handler.Handler) http.HandlerFunc
		remove func(*handler.Handler) http.HandlerFunc
		serve  func(*handler.Handler) http.HandlerFunc
		path   string
		pick   func(logo, banner string) string
	}{
		{"logo", func(h *handler.Handler) http.HandlerFunc { return h.RequireAuth(h.UploadBrandingLogo) },
			func(h *handler.Handler) http.HandlerFunc { return h.RequireAuth(h.DeleteBrandingLogo) },
			func(h *handler.Handler) http.HandlerFunc { return h.ServeBrandingLogo },
			"/branding/logo", func(l, _ string) string { return l }},
		{"banner", func(h *handler.Handler) http.HandlerFunc { return h.RequireAuth(h.UploadBrandingBanner) },
			func(h *handler.Handler) http.HandlerFunc { return h.RequireAuth(h.DeleteBrandingBanner) },
			func(h *handler.Handler) http.HandlerFunc { return h.ServeBrandingBanner },
			"/branding/banner", func(_, b string) string { return b }},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			h, database, key, _ := setupWorkspaceWithDB(t)

			if rec := serve(t, tc.serve(h), tc.path, nil); rec.Code != http.StatusNotFound {
				t.Fatalf("serve before any upload: %d; want 404", rec.Code)
			}

			rec := httptest.NewRecorder()
			tc.upload(h)(rec, uploadForm(t, "/v1/settings/branding/"+tc.kind, tc.kind,
				solidPNG(t, 40, 20, color.NRGBA{R: 200, A: 255}), key))
			if rec.Code != http.StatusOK {
				t.Fatalf("upload: %d — %s", rec.Code, rec.Body.String())
			}
			var up map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
				t.Fatalf("decode upload: %v", err)
			}
			url := up[tc.kind+"_url"]
			if !strings.HasPrefix(url, tc.path+"?v=") || !versionedURL.MatchString(url) {
				t.Fatalf("%s_url = %q; want %s?v=<16 hex>", tc.kind, url, tc.path)
			}
			if got := tc.pick(getBrandingURLs(t, h, key)); got != url {
				t.Errorf("GET branding %s_url = %q; want the uploaded %q", tc.kind, got, url)
			}

			got := serve(t, tc.serve(h), url, nil)
			if got.Code != http.StatusOK {
				t.Fatalf("serve: %d — %s", got.Code, got.Body.String())
			}
			if ct := got.Header().Get("Content-Type"); ct != "image/png" {
				t.Errorf("Content-Type = %q; want image/png", ct)
			}
			if v := got.Header().Get("X-Content-Type-Options"); v != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q; want nosniff", v)
			}
			if cc := got.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
				t.Errorf("Cache-Control = %q", cc)
			}
			served := got.Body.Bytes()
			sum := sha256.Sum256(served)
			wantETag := `"` + hex.EncodeToString(sum[:]) + `"`
			if et := got.Header().Get("ETag"); et != wantETag {
				t.Errorf("ETag = %q; want the served bytes' SHA-256 %s", et, wantETag)
			}
			if !strings.HasSuffix(url, hex.EncodeToString(sum[:])[:16]) {
				t.Errorf("URL %q does not carry the served bytes' hash prefix", url)
			}
			img, format, err := image.Decode(bytes.NewReader(served))
			if err != nil || format != "png" {
				t.Fatalf("served bytes are not a PNG (%s): %v", format, err)
			}
			if r, _, _, _ := img.At(1, 1).RGBA(); r>>8 != 200 {
				t.Errorf("served pixel red = %d; want the uploaded 200", r>>8)
			}

			if nm := serve(t, tc.serve(h), url, map[string]string{"If-None-Match": wantETag}); nm.Code != http.StatusNotModified {
				t.Errorf("If-None-Match with the current ETag: %d; want 304", nm.Code)
			} else if nm.Body.Len() != 0 {
				t.Errorf("304 carried %d body bytes", nm.Body.Len())
			}
			if stale := serve(t, tc.serve(h), url, map[string]string{"If-None-Match": `"stale"`}); stale.Code != http.StatusOK {
				t.Errorf("If-None-Match with another ETag: %d; want 200", stale.Code)
			}

			// A re-upload of different bytes replaces the row and changes the URL.
			rec = httptest.NewRecorder()
			tc.upload(h)(rec, uploadForm(t, "/v1/settings/branding/"+tc.kind, tc.kind,
				solidPNG(t, 40, 20, color.NRGBA{B: 200, A: 255}), key))
			if rec.Code != http.StatusOK {
				t.Fatalf("re-upload: %d — %s", rec.Code, rec.Body.String())
			}
			var again map[string]string
			_ = json.Unmarshal(rec.Body.Bytes(), &again)
			if again[tc.kind+"_url"] == url {
				t.Errorf("re-upload of different bytes kept the URL %q; caches would keep the old image", url)
			}
			if n := countAssets(t, database); n != 1 {
				t.Errorf("%d asset rows after a re-upload; want 1 (replaced, not added)", n)
			}

			rec = httptest.NewRecorder()
			tc.remove(h)(rec, authReq(http.MethodDelete, "/v1/settings/branding/"+tc.kind, "", key))
			if rec.Code != http.StatusNoContent {
				t.Fatalf("remove: %d — %s", rec.Code, rec.Body.String())
			}
			if rec := serve(t, tc.serve(h), tc.path, nil); rec.Code != http.StatusNotFound {
				t.Errorf("serve after remove: %d; want 404", rec.Code)
			}
			if got := tc.pick(getBrandingURLs(t, h, key)); got != "" {
				t.Errorf("GET branding %s_url after remove = %q; want empty", tc.kind, got)
			}
			if n := countAssets(t, database); n != 0 {
				t.Errorf("%d asset rows after remove; want 0", n)
			}
		})
	}
}

// The upload limits hold, and a refused upload writes nothing.
func TestBrandingImages_refusals(t *testing.T) {
	h, database, key, _ := setupWorkspaceWithDB(t)

	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"a truncated PNG", solidPNG(t, 4, 4, color.White)[:40], "could not decode image"},
		{"text", []byte(strings.Repeat("hello ", 200)), "must be JPEG, PNG, GIF, or WebP"},
		{"over 5 MB", append(solidPNG(t, 4, 4, color.White), make([]byte, 5<<20+2048)...), "≤5 MB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.RequireAuth(h.UploadBrandingLogo)(rec, uploadForm(t, "/v1/settings/branding/logo", "logo", tc.data, key))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("upload: %d; want 400 — %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body = %s; want it to say %q", rec.Body.String(), tc.want)
			}
		})
	}
	if n := countAssets(t, database); n != 0 {
		t.Errorf("%d asset rows after refused uploads; want 0", n)
	}
	if logo, _ := getBrandingURLs(t, h, key); logo != "" {
		t.Errorf("logo_url = %q after refused uploads; want empty", logo)
	}
}

// The migration's CHECKs are the second line: a write that got past the handler's limits
// is still refused by the database, on both engines.
func TestWorkspaceAssets_schemaRefusesWhatTheUploadRefuses(t *testing.T) {
	_, database, _, userID := setupWorkspaceWithDB(t)
	hash := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name               string
		kind, owner, ctype string
		data               []byte
	}{
		{"over 5 MiB", "logo", "", "image/png", make([]byte, 5<<20+1)},
		{"empty", "logo", "", "image/png", []byte{}},
		{"svg", "logo", "", "image/svg+xml", []byte("<svg/>")},
		{"unknown kind", "favicon", "", "image/png", []byte{1}},
		{"avatar without an owner", "avatar", "", "image/jpeg", []byte{1}},
		{"logo with an owner", "logo", userID, "image/png", []byte{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := database.Exec(
				`INSERT INTO workspace_assets (kind, owner_id, content_type, data, sha256) VALUES (?, ?, ?, ?, ?)`,
				tc.kind, tc.owner, tc.ctype, tc.data, hash); err == nil {
				t.Errorf("the schema accepted it")
			} else if !db.IsCheckViolation(err) {
				t.Errorf("refused, but not by a CHECK: %v", err)
			}
		})
	}
	// Control: the shape the handlers write is accepted, so the refusals above are the
	// CHECKs and not a broken INSERT.
	if _, err := database.Exec(
		`INSERT INTO workspace_assets (kind, owner_id, content_type, data, sha256) VALUES ('avatar', ?, 'image/jpeg', ?, ?)`,
		userID, []byte{0xff, 0xd8, 0xff}, hash); err != nil {
		t.Fatalf("control insert: %v", err)
	}
}

// addMember inserts a second member with an API key of their own, the way the tenancy
// fixtures do, and returns (userID, apiKey).
func addMember(t *testing.T, database *db.DB, name string) (string, string) {
	t.Helper()
	id := uid.New()
	if _, err := database.Exec(
		`INSERT INTO users (id, email, name, is_admin, is_owner) VALUES (?, ?, ?, 0, 0)`,
		id, strings.ToLower(name)+"@example.com", name); err != nil {
		t.Fatalf("insert member: %v", err)
	}
	key := "cno_" + strings.ReplaceAll(uid.New(), "-", "")
	sum := sha256.Sum256([]byte(key))
	if _, err := database.Exec(
		`INSERT INTO api_keys (id, user_id, name, key_hash) VALUES (?, ?, 'test', ?)`,
		uid.New(), id, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("insert key: %v", err)
	}
	return id, key
}

func serveAvatar(t *testing.T, h *handler.Handler, userID string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/avatars/"+userID, nil)
	r.SetPathValue("userID", userID)
	rec := httptest.NewRecorder()
	h.ServeAvatar(rec, r)
	return rec
}

func uploadAvatar(t *testing.T, h *handler.Handler, key string, c color.Color) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RequireAuth(h.UploadAvatar)(rec, uploadForm(t, "/v1/users/me/avatar", "avatar", solidPNG(t, 30, 30, c), key))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload avatar: %d — %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body["avatar_url"]
}

// Each member's avatar is their own row: two members, two images, and removing one
// leaves the other.
func TestAvatars_perUser(t *testing.T) {
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)
	memberID, memberKey := addMember(t, database, "Member")

	ownerURL := uploadAvatar(t, h, ownerKey, color.NRGBA{R: 255, A: 255})
	memberURL := uploadAvatar(t, h, memberKey, color.NRGBA{G: 255, A: 255})
	if !strings.HasPrefix(ownerURL, "/avatars/"+ownerID+"?v=") || !versionedURL.MatchString(ownerURL) {
		t.Errorf("owner avatar_url = %q; want /avatars/<id>?v=<16 hex>", ownerURL)
	}

	for _, tc := range []struct {
		who, id string
		red     bool
	}{{"owner", ownerID, true}, {"member", memberID, false}} {
		rec := serveAvatar(t, h, tc.id)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s avatar: %d", tc.who, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
			t.Errorf("%s avatar Content-Type = %q; want image/jpeg", tc.who, ct)
		}
		img, err := jpeg.Decode(rec.Body)
		if err != nil {
			t.Fatalf("%s avatar is not a JPEG: %v", tc.who, err)
		}
		r, g, _, _ := img.At(5, 5).RGBA()
		if isRed := r > g; isRed != tc.red {
			t.Errorf("%s avatar is the other member's image (r=%d g=%d)", tc.who, r>>8, g>>8)
		}
	}

	rec := httptest.NewRecorder()
	h.RequireAuth(h.DeleteAvatar)(rec, authReq(http.MethodDelete, "/v1/users/me/avatar", "", memberKey))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete member avatar: %d — %s", rec.Code, rec.Body.String())
	}
	if rec := serveAvatar(t, h, memberID); rec.Code != http.StatusNotFound {
		t.Errorf("member avatar after delete: %d; want 404", rec.Code)
	}
	if rec := serveAvatar(t, h, ownerID); rec.Code != http.StatusOK {
		t.Errorf("owner avatar after the member deleted theirs: %d; want 200", rec.Code)
	}
	var memberAvatar *string
	if err := database.QueryRow(`SELECT avatar_url FROM users WHERE id = ?`, memberID).Scan(&memberAvatar); err != nil {
		t.Fatalf("read member avatar_url: %v", err)
	}
	if memberAvatar != nil {
		t.Errorf("member avatar_url = %q after delete; want NULL", *memberAvatar)
	}
	_ = memberURL

	// Removing the member takes their avatar row with them.
	uploadAvatar(t, h, memberKey, color.NRGBA{G: 255, A: 255})
	del := authReq(http.MethodDelete, "/v1/users/"+memberID, "", ownerKey)
	del.SetPathValue("id", memberID)
	rec = httptest.NewRecorder()
	h.RequireAuth(h.DeleteUser)(rec, del)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove member: %d — %s", rec.Code, rec.Body.String())
	}
	var left int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM workspace_assets WHERE kind = 'avatar' AND owner_id = ?`, memberID).Scan(&left); err != nil {
		t.Fatalf("count member avatar rows: %v", err)
	}
	if left != 0 {
		t.Errorf("%d avatar rows left for a removed member; want 0", left)
	}
	if rec := serveAvatar(t, h, memberID); rec.Code != http.StatusNotFound {
		t.Errorf("removed member's avatar: %d; want 404", rec.Code)
	}
}

func TestServeAvatar_unknownAndMalformedAre404(t *testing.T) {
	h, _, _, _ := setupWorkspaceWithDB(t)
	for _, id := range []string{uid.New(), "../../etc/passwd", "NOT-HEX", strings.Repeat("a", 65)} {
		if rec := serveAvatar(t, h, id); rec.Code != http.StatusNotFound {
			t.Errorf("GET /avatars/%s: %d; want 404", id, rec.Code)
		}
	}
}

// ⛔ The state every instance is in after upgrading: URLs written when the images were
// files, with no row behind them. They must read as unset everywhere a URL is shown,
// or the booking emails switch into the tenant-branded layout with a dead image and
// every page shows a broken <img>.
func TestDanglingImageURLs_readAsUnset(t *testing.T) {
	h, database, key, userID := setupWorkspaceWithDB(t)
	slug, _ := seedEventTypeHTTP(t, h, key)

	if _, err := database.Exec(
		`UPDATE server_settings SET logo_url = '/branding/logo?v=1726000000', banner_url = '/branding/banner?v=1726000000' WHERE id = 1`); err != nil {
		t.Fatalf("seed legacy branding urls: %v", err)
	}
	if _, err := database.Exec(`UPDATE users SET avatar_url = ? WHERE id = ?`, "/avatars/"+userID, userID); err != nil {
		t.Fatalf("seed legacy avatar url: %v", err)
	}

	if logo, banner := getBrandingURLs(t, h, key); logo != "" || banner != "" {
		t.Errorf("GET branding = %q / %q; want both empty while no image is stored", logo, banner)
	}
	pub := publicEventType(t, h, slug)
	if pub.LogoURL != "" || pub.BannerURL != "" {
		t.Errorf("public event type logo/banner = %q / %q; want empty", pub.LogoURL, pub.BannerURL)
	}
	for _, host := range pub.Hosts {
		if host.AvatarURL != "" {
			t.Errorf("public host avatar_url = %q; want empty", host.AvatarURL)
		}
	}
	if got := listedAvatar(t, h, key, userID); got != "" {
		t.Errorf("GET /v1/users avatar_url = %q; want empty", got)
	}

	// The column is untouched: this is a read-time rule, so nothing was destroyed and an
	// upload brings the image back under a new URL.
	var stored string
	if err := database.QueryRow(`SELECT logo_url FROM server_settings WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatalf("read stored logo_url: %v", err)
	}
	if stored != "/branding/logo?v=1726000000" {
		t.Errorf("stored logo_url = %q; the read-time rule must not rewrite the column", stored)
	}
	url := uploadAvatar(t, h, key, color.NRGBA{R: 255, A: 255})
	if got := listedAvatar(t, h, key, userID); got != url {
		t.Errorf("GET /v1/users avatar_url after upload = %q; want %q", got, url)
	}
	if pub := publicEventType(t, h, slug); len(pub.Hosts) == 0 || !strings.HasSuffix(pub.Hosts[0].AvatarURL, url) {
		t.Errorf("public host avatar after upload = %+v; want it to end in %q", pub.Hosts, url)
	}
}

// A URL this server does not serve is not second-guessed: the rule is only about its own
// /branding/ and /avatars/ paths.
func TestForeignImageURLs_passThrough(t *testing.T) {
	h, database, key, userID := setupWorkspaceWithDB(t)
	const logo = "https://cdn.example.test/logo.png"
	if _, err := database.Exec(`UPDATE server_settings SET logo_url = ? WHERE id = 1`, logo); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const avatar = "https://images.example.test/me.jpg"
	if _, err := database.Exec(`UPDATE users SET avatar_url = ? WHERE id = ?`, avatar, userID); err != nil {
		t.Fatalf("seed avatar: %v", err)
	}
	if got, _ := getBrandingURLs(t, h, key); got != logo {
		t.Errorf("logo_url = %q; want %q unchanged", got, logo)
	}
	if got := listedAvatar(t, h, key, userID); got != avatar {
		t.Errorf("avatar_url = %q; want %q unchanged", got, avatar)
	}
}

type publicET struct {
	LogoURL   string `json:"logo_url"`
	BannerURL string `json:"banner_url"`
	Hosts     []struct {
		AvatarURL string `json:"avatar_url"`
	} `json:"hosts"`
}

func publicEventType(t *testing.T, h *handler.Handler, slug string) publicET {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/event-types/"+slug+"/public", nil)
	r.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	h.PublicEventType(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("public event type: %d — %s", rec.Code, rec.Body.String())
	}
	var out publicET
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode public event type: %v", err)
	}
	return out
}

func listedAvatar(t *testing.T, h *handler.Handler, key, userID string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RequireAuth(h.ListUsers)(rec, authReq(http.MethodGet, "/v1/users", "", key))
	if rec.Code != http.StatusOK {
		t.Fatalf("list users: %d — %s", rec.Code, rec.Body.String())
	}
	var users []struct {
		ID        string `json:"id"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("decode users: %v — %s", err, rec.Body.String())
	}
	for _, u := range users {
		if u.ID == userID {
			return u.AvatarURL
		}
	}
	t.Fatalf("user %s not listed", userID)
	return ""
}
