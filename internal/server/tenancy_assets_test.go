package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/uid"
)

// Uploaded images on a multi-tenant instance, through the real mux and a NOBYPASSRLS
// application role (newTenancyFixture skips loudly without one).
//
// ⛔ The bug this replaces: images were files under one DATA_DIR shared by every tenant,
// so A's logo upload overwrote B's file, B's booking pages and emails served A's logo, and
// A's "Remove logo" deleted B's. Every assertion below is made in both directions, and
// no handler in the tree carries a workspace predicate for workspace_assets: what is
// being proven is the row-level-security policy, via the host the request names.

// tenantPNG is a small PNG of one colour; the colour is how a test tells whose image it
// was served.
func tenantPNG(t *testing.T, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 24, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 24; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// upload posts one file field through the mux, on host, as apiKey.
func (f *tenancyFixture) upload(t *testing.T, host, path, field, apiKey string, data []byte) map[string]string {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(field, "image.png")
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("multipart write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	r := newRequest(t, http.MethodPost, host, path, &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("X-API-Key", apiKey)
	rec := f.serve(r)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s on %s: %d — %s", path, host, rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	return out
}

// redOf fetches path on host and returns the red channel of its first pixel, or -1 with
// the status when the response is not a 200 image.
func (f *tenancyFixture) redOf(t *testing.T, host, path string) (int, int) {
	t.Helper()
	rec := f.do(t, http.MethodGet, host, path, "", "")
	if rec.Code != http.StatusOK {
		return -1, rec.Code
	}
	img, _, err := image.Decode(rec.Body)
	if err != nil {
		t.Fatalf("GET %s on %s: not an image: %v", path, host, err)
	}
	r, _, _, _ := img.At(0, 0).RGBA()
	return int(r >> 8), rec.Code
}

func TestTenancy_brandingImagesArePerWorkspace(t *testing.T) {
	f := newTenancyFixture(t)
	red := color.NRGBA{R: 220, A: 255}
	blue := color.NRGBA{B: 220, A: 255}

	for _, kind := range []string{"logo", "banner"} {
		t.Run(kind, func(t *testing.T) {
			path := "/branding/" + kind
			f.upload(t, f.a.host, "/v1/settings/branding/"+kind, kind, f.a.apiKey, tenantPNG(t, red))
			f.upload(t, f.b.host, "/v1/settings/branding/"+kind, kind, f.b.apiKey, tenantPNG(t, blue))

			if r, code := f.redOf(t, f.a.host, path); r != 220 {
				t.Errorf("A's host serves %s red=%d (status %d); want A's own image (220)", kind, r, code)
			}
			if r, code := f.redOf(t, f.b.host, path); r != 0 {
				t.Errorf("B's host serves %s red=%d (status %d); want B's own image (0) — A's upload reached B", kind, r, code)
			}

			// Each workspace's settings name its own image.
			var aURL, bURL string
			for _, tn := range []struct {
				host, key string
				dst       *string
			}{{f.a.host, f.a.apiKey, &aURL}, {f.b.host, f.b.apiKey, &bURL}} {
				rec := f.do(t, http.MethodGet, tn.host, "/v1/settings/branding", tn.key, "")
				var body map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &body)
				*tn.dst, _ = body[kind+"_url"].(string)
			}
			if aURL == "" || bURL == "" || aURL == bURL {
				t.Errorf("%s_url A=%q B=%q; want two different, non-empty URLs", kind, aURL, bURL)
			}

			// A removes theirs; B's is untouched.
			if rec := f.do(t, http.MethodDelete, f.a.host, "/v1/settings/branding/"+kind, f.a.apiKey, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("A removes its %s: %d — %s", kind, rec.Code, rec.Body.String())
			}
			if _, code := f.redOf(t, f.a.host, path); code != http.StatusNotFound {
				t.Errorf("A's %s after removal: status %d; want 404", kind, code)
			}
			if r, code := f.redOf(t, f.b.host, path); r != 0 || code != http.StatusOK {
				t.Errorf("B's %s after A removed theirs: red=%d status %d; want B's image, 200", kind, r, code)
			}
		})
	}

	// The platform handle sees one row per workspace per kind that is still present,
	// each with its own workspace_id: the rows landed where the host said, not where a
	// column default would have put them.
	rows, err := f.plat.QueryContext(context.Background(),
		`SELECT workspace_id, kind FROM workspace_assets ORDER BY workspace_id, kind`)
	if err != nil {
		t.Fatalf("read assets: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var ws, kind string
		if err := rows.Scan(&ws, &kind); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, ws+"/"+kind)
	}
	if want := "globex/banner,globex/logo"; strings.Join(got, ",") != want {
		t.Errorf("assets on the platform handle = %v; want %s", got, want)
	}
}

// addHexMember gives a tenant a second member whose id has the UUID shape /avatars/
// accepts (the fixture's own users are "acme-user"), with an API key of their own.
func (f *tenancyFixture) addHexMember(t *testing.T, workspace string) (id, key string) {
	t.Helper()
	h := f.app.ForWorkspace(workspace)
	id = uid.New()
	if _, err := h.ExecContext(context.Background(),
		`INSERT INTO users (id, email, name, is_admin, is_owner) VALUES (?, ?, 'Member', 0, 0)`,
		id, workspace+"-member@example.com"); err != nil {
		t.Fatalf("member for %s: %v", workspace, err)
	}
	key = "cno_" + workspace + "_member_key"
	sum := sha256.Sum256([]byte(key))
	if _, err := h.ExecContext(context.Background(),
		`INSERT INTO api_keys (id, user_id, name, key_hash) VALUES (?, ?, 'test', ?)`,
		workspace+"-member-key", id, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("member key for %s: %v", workspace, err)
	}
	return id, key
}

func TestTenancy_avatarsArePerWorkspace(t *testing.T) {
	f := newTenancyFixture(t)
	aID, aKey := f.addHexMember(t, f.a.id)
	bID, bKey := f.addHexMember(t, f.b.id)

	aURL := f.upload(t, f.a.host, "/v1/users/me/avatar", "avatar", aKey, tenantPNG(t, color.NRGBA{R: 255, A: 255}))["avatar_url"]
	f.upload(t, f.b.host, "/v1/users/me/avatar", "avatar", bKey, tenantPNG(t, color.NRGBA{B: 255, A: 255}))
	if !strings.HasPrefix(aURL, "/avatars/"+aID+"?v=") {
		t.Fatalf("A's avatar_url = %q", aURL)
	}

	if _, code := f.redOf(t, f.a.host, "/avatars/"+aID); code != http.StatusOK {
		t.Errorf("A's member avatar on A's host: %d; want 200", code)
	}
	if _, code := f.redOf(t, f.b.host, "/avatars/"+bID); code != http.StatusOK {
		t.Errorf("B's member avatar on B's host: %d; want 200", code)
	}
	// Each on the other's host: the row exists, and the host's workspace cannot see it.
	if _, code := f.redOf(t, f.b.host, "/avatars/"+aID); code != http.StatusNotFound {
		t.Errorf("A's member avatar on B's host: %d; want 404", code)
	}
	if _, code := f.redOf(t, f.a.host, "/avatars/"+bID); code != http.StatusNotFound {
		t.Errorf("B's member avatar on A's host: %d; want 404", code)
	}
}
