package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestTenancy_bookingLinkIsNotRenamed is the multi-tenant slug refusal through the real
// mux, on a tenant host, against a real OpenPair.
//
// The event type it renames is created here, UNBOOKED, on purpose. The fixture's own
// event type already has a booking, and upstream refuses a rename once one exists, so
// asserting against it would pass with the multi-tenant refusal deleted. An unbooked
// event type is the case upstream allows and this deployment must not.
func TestTenancy_bookingLinkIsNotRenamed(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()

	const slug = "phone-consultation"
	created := f.do(t, http.MethodPost, f.a.host, "/v1/event-types", f.a.apiKey,
		fmt.Sprintf(`{"slug":%q,"name":"Phone consultation","duration_minutes":30}`, slug))
	if created.Code != http.StatusCreated {
		t.Fatalf("create the unbooked event type: %d - %s", created.Code, created.Body.String())
	}

	rename := f.do(t, http.MethodPatch, f.a.host, "/v1/event-types/"+slug, f.a.apiKey,
		`{"slug":"phone-call"}`)
	if rename.Code != http.StatusConflict {
		t.Fatalf("rename on a multi-tenant host: %d; want 409 - %s", rename.Code, rename.Body.String())
	}
	var refusal struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rename.Body.Bytes(), &refusal); err != nil ||
		!strings.Contains(refusal.Error, "not available on this deployment") {
		t.Errorf("refusal body = %s; want an error saying renaming is not available on this deployment", rename.Body.String())
	}

	// The editor's whole-form save: the slug it already has, and a change elsewhere.
	save := f.do(t, http.MethodPatch, f.a.host, "/v1/event-types/"+slug, f.a.apiKey,
		fmt.Sprintf(`{"slug":%q,"name":"Phone consultation (15 min)"}`, slug))
	if save.Code != http.StatusOK {
		t.Fatalf("whole-form save with the slug unchanged: %d; want 200 - %s", save.Code, save.Body.String())
	}

	// Where the row IS, read on the platform handle: still under the slug the platform
	// addresses, with the other field saved.
	var gotSlug, gotName string
	if err := f.plat.QueryRowContext(ctx,
		`SELECT slug, name FROM event_types WHERE workspace_id = ? AND name LIKE 'Phone consultation%'`,
		f.a.id).Scan(&gotSlug, &gotName); err != nil {
		t.Fatalf("read the event type back: %v", err)
	}
	if gotSlug != slug {
		t.Errorf("slug after a refused rename = %q; want %q", gotSlug, slug)
	}
	if gotName != "Phone consultation (15 min)" {
		t.Errorf("name after the whole-form save = %q; want it saved", gotName)
	}

	// And the public booking surface still resolves it on the tenant's host.
	if rec := f.do(t, http.MethodGet, f.a.host, "/v1/event-types/"+slug+"/slots", "", ""); rec.Code != http.StatusOK {
		t.Errorf("slots under the original slug: %d; want 200 - %s", rec.Code, rec.Body.String())
	}
}
