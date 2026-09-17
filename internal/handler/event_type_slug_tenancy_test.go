package handler_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/handler"
)

// A booking link is never renamed on a multi-tenant deployment.
//
// Upstream lets PATCH /v1/event-types/{slug} rename an event type until its first booking.
// On this fork's multi-tenant deployment that window does not exist: the platform in front
// of it (the District website, and the voice agent booking over the phone) addresses each
// tenant's event types BY SLUG and builds booking URLs from them, so a rename breaks voice
// booking the moment it lands, with no booking row anywhere to have warned about it.
//
// The refusal keys on the rename, not on the field. The admin editor submits every field
// on every save, slug included, so refusing on mention would make every event type
// uneditable from the console in this mode.

func TestPatchEventType_renameAnswersByMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		multiTenant bool
		wantCode    int
	}{
		{name: "single-tenant renames while unbooked", multiTenant: false, wantCode: http.StatusOK},
		{name: "multi-tenant refuses even while unbooked", multiTenant: true, wantCode: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, apiKey, _ := setupWorkspace(t)
			slug, _ := seedEventTypeHTTP(t, h, apiKey)
			h.SetMultiTenant(tc.multiTenant)

			code, body := patchSlug(t, h, apiKey, slug, "phone-consultation")
			if code != tc.wantCode {
				t.Fatalf("rename: %d; want %d - %v", code, tc.wantCode, body)
			}

			// Whichever answer, the slug that resolves afterwards has to agree with it.
			want, gone := "phone-consultation", slug
			if tc.multiTenant {
				want, gone = slug, "phone-consultation"
				msg, _ := body["error"].(string)
				if !strings.Contains(msg, "not available on this deployment") {
					t.Errorf("error = %q; want it to say renaming is not available on this deployment", msg)
				}
			}
			if code := patchName(t, h, apiKey, want, "Still reachable"); code != http.StatusOK {
				t.Errorf("PATCH through %q after the rename answer: %d; want 200", want, code)
			}
			if code := patchName(t, h, apiKey, gone, "Should not exist"); code != http.StatusNotFound {
				t.Errorf("PATCH through %q after the rename answer: %d; want 404", gone, code)
			}
		})
	}
}

// The editor's whole-form save: slug unchanged, another field changed. Must succeed in
// multi-tenant mode, or the console could not save anything at all.
func TestPatchEventType_multiTenantUnchangedSlugIsANoOp(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, apiKey)
	h.SetMultiTenant(true)

	body := fmt.Sprintf(`{"slug": %q, "name": "Phone consultation"}`, slug)
	req := authReq(http.MethodPatch, "/v1/event-types/"+slug, body, apiKey)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchEventType)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("whole-form save with the slug unchanged: %d; want 200 - %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"Phone consultation"`) {
		t.Errorf("the other field was not saved: %s", rec.Body.String())
	}

	// Upstream treats a value that slugifies to the stored slug as unchanged, and so does
	// this mode: it is the same link, however the form spelled it.
	if code, body := patchSlug(t, h, apiKey, slug, "  "+strings.ToUpper(slug)+"  "); code != http.StatusOK {
		t.Errorf("resubmitting the slug in another spelling of itself: %d; want 200 - %v", code, body)
	} else if got := body["slug"]; got != slug {
		t.Errorf("slug = %v; want %q untouched", got, slug)
	}
}

// A stored slug that slugify would rewrite is still "unchanged" when it is sent back as
// it is. Create does not normalise slugs (nor does platform provisioning), so a row can
// legitimately hold one, and comparing only the slugified value would turn every save of
// it into a refused rename: an event type the console can never save again.
func TestPatchEventType_multiTenantNonCanonicalStoredSlugStaysEditable(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	const stored = "Phone_Consultation"
	req := authReq(http.MethodPost, "/v1/event-types",
		fmt.Sprintf(`{"slug":%q,"name":"Phone consultation","duration_minutes":30}`, stored), apiKey)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.CreateEventType)(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with a non-canonical slug: %d - %s", rec.Code, rec.Body.String())
	}
	h.SetMultiTenant(true)

	if code, body := patchSlug(t, h, apiKey, stored, stored); code != http.StatusOK {
		t.Fatalf("resubmitting the stored non-canonical slug: %d; want 200 - %v", code, body)
	} else if got := body["slug"]; got != stored {
		t.Errorf("slug = %v; want %q untouched", got, stored)
	}
	// The canonical spelling of it, on the other hand, IS a different link
	// (/book/phone-consultation is not /book/Phone_Consultation), so it is refused.
	if code, body := patchSlug(t, h, apiKey, stored, "phone-consultation"); code != http.StatusConflict {
		t.Errorf("normalising the stored slug to its canonical spelling: %d; want 409 - %v", code, body)
	}
}

// patchName patches an unrelated field through slug, which is a cheap way to ask whether
// that slug resolves to the caller's event type.
func patchName(t *testing.T, h *handler.Handler, apiKey, slug, name string) int {
	t.Helper()
	req := authReq(http.MethodPatch, "/v1/event-types/"+slug, fmt.Sprintf(`{"name": %q}`, name), apiKey)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchEventType)(rec, req)
	return rec.Code
}
