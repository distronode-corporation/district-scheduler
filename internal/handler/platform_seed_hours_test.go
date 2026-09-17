package handler_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
)

// The seeded WORKING HOURS (D12): defaults.event_type.availability becomes the owner's
// global availability rules, the ones the District dashboard edits.
//
// ⛔ The failure this pins was measured in production. Provisioning wrote those rules with
// event_type_id set to the seeded event type. Slot generation offers the UNION of a host's
// global rules and the event type's own, and the dashboard's Working Hours editor (and its
// overview) reads and writes global rules only. So the seeded Monday to Friday 09:00-17:00
// was invisible on the page that claims to show the tenant's hours, and nothing the tenant
// did there could take it away: a tenancy that set 09:00-13:00 kept selling afternoons.
// Every read an operator makes looked right, which is why the assertion that matters is
// the last one: remove a day the way the dashboard does, and the day stops being sold.
//
// ⛔ Postgres-only, for the reason platform_seed_host_test.go states at length:
// multi-tenant IS row-level security, db.OpenPair refuses a non-postgres DSN, and SQLite's
// migration 00060 keeps the single-tenant uniques (server_settings' id = 1 among them), so
// the platform API cannot provision a workspace there at all.

// newPlatformHoursAPI registers the create route plus the three routes this story crosses,
// as server.go registers them: the public slots read (Host-scoped), and the rules list and
// delete the dashboard calls (RequireAuth, then Scoped on the credential). Registered on a
// mux so {slug} and {id} arrive as real path values.
func newPlatformHoursAPI(t *testing.T) (create http.HandlerFunc, mux http.Handler, app, platform *db.DB) {
	t.Helper()
	app, platform = dbtest.RequireTenantPair(t)

	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://cal.example.test")
	h.SetPlatformToken(platformToken)
	h.SetEncKey(platformTestEncKey)

	m := http.NewServeMux()
	m.HandleFunc("GET /v1/event-types/{slug}/slots",
		h.Scoped(handler.HostWorkspace, (*handler.Handler).GetSlots))
	m.HandleFunc("GET /v1/availability-rules",
		h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*handler.Handler).ListAvailabilityRules)))
	m.HandleFunc("DELETE /v1/availability-rules/{id}",
		h.RequireAuth(h.Scoped(handler.CredentialWorkspace, (*handler.Handler).DeleteAvailabilityRule)))

	return h.Platform((*handler.Handler).CreateWorkspace), m, app, platform
}

// weekdayHoursBody is the settled create body with the hours the website sends by default:
// Monday to Friday, 09:00-17:00. The owner is in UTC so the grid below is plain arithmetic;
// the rules are local HH:MM in the owner's zone, and zone handling is not what this is about.
func weekdayHoursBody(id, host string) map[string]any {
	body := platformCreateBody(id, host)
	body["owner_timezone"] = "UTC"

	availability := make([]map[string]any, 0, 5)
	for dow := 1; dow <= 5; dow++ {
		availability = append(availability, map[string]any{
			"day_of_week": dow, "start_time": "09:00", "end_time": "17:00",
		})
	}
	et := body["defaults"].(map[string]any)["event_type"].(map[string]any)
	et["availability"] = availability
	return body
}

// slotGrid asks the public slots route for the seeded event type over days, and returns
// each of those dates' slots as "HH:MM-HH:MM" in UTC. The caller starts days tomorrow so
// the 60-minute notice period can never touch the grid; the request runs one day past the
// last date so whether `to` is inclusive cannot decide what that date shows.
func slotGrid(t *testing.T, mux http.Handler, days []time.Time) map[string][]string {
	t.Helper()
	from := days[0].Format("2006-01-02")
	to := days[len(days)-1].AddDate(0, 0, 1).Format("2006-01-02")
	req := httptest.NewRequest(http.MethodGet,
		"/v1/event-types/intro/slots?from="+from+"&to="+to+"&tz=UTC", nil)
	req.Host = "book.acme.example"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("slots: status = %d; want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Slots []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"slots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode slots %s: %v", rec.Body.String(), err)
	}

	grid := make(map[string][]string, len(days))
	for _, d := range days {
		grid[d.Format("2006-01-02")] = []string{}
	}
	for _, s := range out.Slots {
		start, err := time.Parse(time.RFC3339, s.Start)
		if err != nil {
			t.Fatalf("slot start %q: %v", s.Start, err)
		}
		end, err := time.Parse(time.RFC3339, s.End)
		if err != nil {
			t.Fatalf("slot end %q: %v", s.End, err)
		}
		key := start.UTC().Format("2006-01-02")
		if _, inWindow := grid[key]; !inWindow {
			continue
		}
		grid[key] = append(grid[key], start.UTC().Format("15:04")+"-"+end.UTC().Format("15:04"))
	}
	return grid
}

// weekdayGrid is Monday to Friday 09:00-17:00 as 30-minute meetings starting every 30
// minutes (the seeded duration, and the interval an event type defaults to), with the
// dates in skip left empty.
func weekdayGrid(days []time.Time, skip ...time.Weekday) map[string][]string {
	var workday []string
	for m := 9 * 60; m+30 <= 17*60; m += 30 {
		workday = append(workday, fmt.Sprintf("%02d:%02d-%02d:%02d", m/60, m%60, (m+30)/60, (m+30)%60))
	}
	grid := make(map[string][]string, len(days))
	for _, d := range days {
		key := d.Format("2006-01-02")
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday || slices.Contains(skip, d.Weekday()) {
			grid[key] = []string{}
			continue
		}
		grid[key] = workday
	}
	return grid
}

func assertGrid(t *testing.T, got, want map[string][]string, days []time.Time, when string) {
	t.Helper()
	for _, d := range days {
		key := d.Format("2006-01-02")
		if !slices.Equal(got[key], want[key]) {
			t.Errorf("%s: %s (%s) offers %v; want %v", when, key, d.Weekday(), got[key], want[key])
		}
	}
}

func TestPlatform_seedsTheOwnersWorkingHoursAsGlobalRules(t *testing.T) {
	create, mux, app, platform := newPlatformHoursAPI(t)

	rec := doPlatform(t, create, http.MethodPost, "/v1/platform/workspaces",
		weekdayHoursBody("acme", "book.acme.example"), platformToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d; want 201: %s", rec.Code, rec.Body.String())
	}
	var provisioned struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &provisioned); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	var ownerID, etID string
	if err := platform.QueryRow(
		`SELECT id FROM users WHERE workspace_id = 'acme' AND is_owner = 1`).Scan(&ownerID); err != nil {
		t.Fatalf("owner id: %v", err)
	}
	if err := platform.QueryRow(
		`SELECT id FROM event_types WHERE workspace_id = 'acme' AND slug = 'intro'`).Scan(&etID); err != nil {
		t.Fatalf("seeded event type id: %v", err)
	}

	// 1. The rows: five, all the owner's, none attached to the event type.
	//
	// Read across the boundary with the platform handle, so a rule attached to the event
	// type or to another user is counted rather than filtered out of sight.
	var total, global, ownersGlobal int
	if err := platform.QueryRow(`
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE event_type_id IS NULL),
		       COUNT(*) FILTER (WHERE event_type_id IS NULL AND user_id = ?)
		FROM availability_rules WHERE workspace_id = 'acme'`, ownerID).
		Scan(&total, &global, &ownersGlobal); err != nil {
		t.Fatalf("count seeded rules: %v", err)
	}
	if total != 5 || global != 5 || ownersGlobal != 5 {
		var attached int
		if err := platform.QueryRow(
			`SELECT COUNT(*) FROM availability_rules WHERE workspace_id = 'acme' AND event_type_id = ?`,
			etID).Scan(&attached); err != nil {
			t.Fatalf("count rules attached to the seeded event type: %v", err)
		}
		t.Errorf("seeded rules: %d total, %d global, %d global and the owner's (%d attached to the "+
			"seeded event type); want 5/5/5/0. A rule carrying event_type_id is invisible to the "+
			"dashboard's Working Hours editor and is still unioned into every slot of that event "+
			"type, so the tenant cannot see or remove those hours", total, global, ownersGlobal, attached)
	}

	// And the tenancy's own bound handle sees them: the rows are in acme, not merely somewhere.
	var visible int
	if err := app.ForWorkspace("acme").QueryRow(
		`SELECT COUNT(*) FROM availability_rules WHERE event_type_id IS NULL`).Scan(&visible); err != nil {
		t.Fatalf("count rules through acme's handle: %v", err)
	}
	if visible != 5 {
		t.Errorf("global rules visible to acme's own handle = %d; want 5", visible)
	}

	// 2. The dashboard's read. It calls this as the owner and keeps the rules whose
	// event_type_id is null; those are the tenant's working hours as far as it knows.
	listReq := authReq(http.MethodGet, "/v1/availability-rules", "", provisioned.APIKey)
	listReq.Host = "cal.example.test"
	listRec := httptest.NewRecorder()
	mux.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list rules: status = %d; want 200: %s", listRec.Code, listRec.Body.String())
	}
	var listed struct {
		Items []struct {
			ID          string  `json:"id"`
			EventTypeID *string `json:"event_type_id"`
			DayOfWeek   int     `json:"day_of_week"`
			StartTime   string  `json:"start_time"`
			EndTime     string  `json:"end_time"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode rules %s: %v", listRec.Body.String(), err)
	}
	var dashboardDays []int
	var wednesdayRuleIDs []string
	for _, it := range listed.Items {
		if it.EventTypeID != nil {
			t.Errorf("GET /v1/availability-rules lists rule %s (day %d) with event_type_id %q; want "+
				"null. The dashboard shows only global rules, so this one is hours the owner is "+
				"selling and cannot see", it.ID, it.DayOfWeek, *it.EventTypeID)
			continue
		}
		if it.StartTime != "09:00" || it.EndTime != "17:00" {
			t.Errorf("global rule for day %d = %s-%s; want 09:00-17:00", it.DayOfWeek, it.StartTime, it.EndTime)
		}
		dashboardDays = append(dashboardDays, it.DayOfWeek)
		if it.DayOfWeek == int(time.Wednesday) {
			wednesdayRuleIDs = append(wednesdayRuleIDs, it.ID)
		}
	}
	if !slices.Equal(dashboardDays, []int{1, 2, 3, 4, 5}) {
		t.Errorf("days the dashboard shows as working hours = %v; want [1 2 3 4 5] (Monday to Friday)",
			dashboardDays)
	}

	// 3. Booking is unchanged: the same Monday to Friday 09:00-17:00 grid the event-type
	// rules produced. Moving the rows must not move a single slot.
	tomorrow := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	days := make([]time.Time, 7)
	for i := range days {
		days[i] = tomorrow.AddDate(0, 0, i)
	}
	assertGrid(t, slotGrid(t, mux, days), weekdayGrid(days), days, "before any edit")

	// 4. The bug. The owner takes Wednesday out of their working hours on the dashboard,
	// which deletes that day's global rules. Wednesday must stop being sold, and nothing
	// else may change.
	if len(wednesdayRuleIDs) != 1 {
		t.Errorf("global Wednesday rules the dashboard can delete = %d; want 1", len(wednesdayRuleIDs))
	}
	for _, id := range wednesdayRuleIDs {
		delReq := authReq(http.MethodDelete, "/v1/availability-rules/"+id, "", provisioned.APIKey)
		delReq.Host = "cal.example.test"
		delRec := httptest.NewRecorder()
		mux.ServeHTTP(delRec, delReq)
		if delRec.Code != http.StatusNoContent {
			t.Fatalf("delete Wednesday rule %s: status = %d; want 204: %s", id, delRec.Code, delRec.Body.String())
		}
	}
	assertGrid(t, slotGrid(t, mux, days), weekdayGrid(days, time.Wednesday), days,
		"after the dashboard removed Wednesday (a Wednesday still offering slots is the production "+
			"symptom: hours the owner removed, still sold by rules the dashboard cannot see)")
}
