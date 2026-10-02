package handler_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
)

// Export, import and attendee erasure (D12). Real pair throughout: the round trip is only
// meaningful if the rows it moves are ones the policies would otherwise hide.

// newPlatformDataAPI returns the seven platform routes plus both handles.
func newPlatformDataAPI(t *testing.T) (map[string]http.HandlerFunc, *db.DB, *db.DB) {
	t.Helper()
	app, platform := dbtest.RequireTenantPair(t)

	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://cal.example.test")
	h.SetPlatformToken(platformToken)
	h.SetEncKey(platformTestEncKey)

	return map[string]http.HandlerFunc{
		"create":       h.Platform((*handler.Handler).CreateWorkspace),
		"export":       h.Platform((*handler.Handler).ExportWorkspace),
		"import":       h.Platform((*handler.Handler).ImportWorkspace),
		"erase":        h.Platform((*handler.Handler).EraseAttendee),
		"delete":       h.Platform((*handler.Handler).DeleteWorkspace),
		"getWorkspace": h.Platform((*handler.Handler).GetWorkspace),
	}, app, platform
}

// seedTenantData gives a workspace one booking with two attendees and an answer, a note,
// and (from provisioning) a webhook, an API key, an owner and an event type — so the round
// trip has a parent-child chain, a credential, a secret and a free-text row to compare.
func seedTenantData(t *testing.T, platform *db.DB, wsID string) {
	t.Helper()
	var ownerID, etID string
	if err := platform.QueryRow(
		`SELECT id FROM users WHERE workspace_id = ? AND is_owner = 1`, wsID).Scan(&ownerID); err != nil {
		t.Fatalf("read owner of %s: %v", wsID, err)
	}
	if err := platform.QueryRow(
		`SELECT id FROM event_types WHERE workspace_id = ?`, wsID).Scan(&etID); err != nil {
		t.Fatalf("read event type of %s: %v", wsID, err)
	}

	stmts := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO bookings (id, workspace_id, event_type_id, host_id, start_at, end_at, status, created_at)
		  VALUES (?, ?, ?, ?, '2026-10-01T10:00:00Z', '2026-10-01T10:30:00Z', 'confirmed', '2026-09-01T09:00:00Z')`,
			[]any{wsID + "-booking", wsID, etID, ownerID}},
		{`INSERT INTO booking_attendees (id, workspace_id, booking_id, name, email, iana_timezone, is_organizer)
		  VALUES (?, ?, ?, 'Ada', 'ada@example.test', 'UTC', 0)`,
			[]any{wsID + "-att-ada", wsID, wsID + "-booking"}},
		{`INSERT INTO event_type_questions (id, workspace_id, event_type_id, label, type, position)
		  VALUES (?, ?, ?, 'Why?', 'text', 0)`,
			[]any{wsID + "-q", wsID, etID}},
		{`INSERT INTO booking_answers (id, workspace_id, booking_id, question_id, value)
		  VALUES (?, ?, ?, ?, 'because')`,
			[]any{wsID + "-ans", wsID, wsID + "-booking", wsID + "-q"}},
		{`INSERT INTO notes (id, workspace_id, booking_id, content, status, created_at, updated_at)
		  VALUES (?, ?, ?, 'a private note', 'ready', '2026-09-01T09:05:00Z', '2026-09-01T09:05:00Z')`,
			[]any{wsID + "-note", wsID, wsID + "-booking"}},
	}
	for _, s := range stmts {
		if _, err := platform.Exec(s.query, s.args...); err != nil {
			t.Fatalf("seed %s: %v", wsID, err)
		}
	}
}

func provisionForData(t *testing.T, routes map[string]http.HandlerFunc, id, host string) {
	t.Helper()
	rec := doPlatform(t, routes["create"], http.MethodPost, "/v1/platform/workspaces",
		platformCreateBody(id, host), platformToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("provision %s: %d — %s", id, rec.Code, rec.Body.String())
	}
}

// doPlatformSub drives a /{id}/<sub> route, which doPlatform's 5-segment path-value rule
// does not cover.
func doPlatformSub(t *testing.T, route http.HandlerFunc, method, target, id string, body []byte, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.SetPathValue("id", id)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	route(rec, req)
	return rec
}

// The round trip: export, delete the workspace, re-create it empty, import, export again,
// and compare the two documents.
//
// ⛔ The second export is what makes this a real assertion. Comparing row counts would pass
// with every value mangled; comparing the documents byte for byte (minus the two fields
// that are timestamps of the export itself) says the data that came back is the data that
// left, including the API-key hash and the encrypted webhook secret.
func TestPlatformData_exportDeleteImportRoundTrip(t *testing.T) {
	routes, _, platform := newPlatformDataAPI(t)
	provisionForData(t, routes, "acme", "book.acme.example")
	seedTenantData(t, platform, "acme")

	first := doPlatformSub(t, routes["export"], http.MethodPost,
		"/v1/platform/workspaces/acme/export", "acme", nil, platformToken)
	if first.Code != http.StatusOK {
		t.Fatalf("export: %d — %s", first.Code, first.Body.String())
	}
	document := first.Body.Bytes()

	// Every table the schema says is a tenant table is present in the document.
	var doc struct {
		Tables []struct {
			Table string           `json:"table"`
			Rows  []map[string]any `json:"rows"`
		} `json:"tables"`
		RowCounts      map[string]int `json:"row_counts"`
		DEKFingerprint string         `json:"dek_fingerprint"`
	}
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	present := map[string]bool{}
	for _, tbl := range doc.Tables {
		present[tbl.Table] = true
	}
	for _, want := range db.TenantTables {
		if !present[want] {
			t.Errorf("export omits the tenant table %q — a workspace's backup would be incomplete", want)
		}
	}
	for table, min := range map[string]int{
		"users": 1, "event_types": 1, "availability_rules": 2, "api_keys": 1,
		"webhooks": 1, "bookings": 1, "booking_attendees": 1, "booking_answers": 1,
		"notes": 1, "server_settings": 1,
	} {
		if doc.RowCounts[table] < min {
			t.Errorf("export has %d rows in %s; want at least %d", doc.RowCounts[table], table, min)
		}
	}

	// Delete, then re-create the same workspace empty.
	if rec := doPlatform(t, routes["delete"], http.MethodDelete,
		"/v1/platform/workspaces/acme", nil, platformToken); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d — %s", rec.Code, rec.Body.String())
	}
	var left int
	if err := platform.QueryRow(`SELECT COUNT(*) FROM users WHERE workspace_id = 'acme'`).Scan(&left); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if left != 0 {
		t.Fatalf("delete left %d users behind", left)
	}
	if _, err := platform.Exec(`
		INSERT INTO workspaces (id, slug, public_host, region, status, created_at, updated_at)
		VALUES ('acme', 'acme', 'book.acme.example', 'us', 'active', '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z')`); err != nil {
		t.Fatalf("re-create workspace: %v", err)
	}

	imp := doPlatformSub(t, routes["import"], http.MethodPost,
		"/v1/platform/workspaces/acme/import", "acme", document, platformToken)
	if imp.Code != http.StatusOK {
		t.Fatalf("import: %d — %s", imp.Code, imp.Body.String())
	}

	second := doPlatformSub(t, routes["export"], http.MethodPost,
		"/v1/platform/workspaces/acme/export", "acme", nil, platformToken)
	if second.Code != http.StatusOK {
		t.Fatalf("second export: %d — %s", second.Code, second.Body.String())
	}

	if a, b := normaliseExport(t, document), normaliseExport(t, second.Body.Bytes()); a != b {
		t.Errorf("the round trip is not byte-identical: %s", firstDifference(a, b))
	}
}

// normaliseExport drops the fields that describe the export rather than the workspace: the
// timestamp of the export itself, and the workspace row's own updated_at, which the
// re-creation above legitimately rewrites.
func normaliseExport(t *testing.T, raw []byte) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode for comparison: %v", err)
	}
	delete(doc, "exported_at")
	delete(doc, "workspace")
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-encode for comparison: %v", err)
	}
	return string(out)
}

// firstDifference reports where two documents diverge, because a diff of two 50 KB JSON
// strings is unreadable in a test log.
func firstDifference(a, b string) string {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			lo := i - 80
			if lo < 0 {
				lo = 0
			}
			hiA, hiB := i+80, i+80
			if hiA > len(a) {
				hiA = len(a)
			}
			if hiB > len(b) {
				hiB = len(b)
			}
			return "at byte " + strconv.Itoa(i) + "\n…" + a[lo:hiA] + "\n…" + b[lo:hiB]
		}
	}
	if len(a) != len(b) {
		return "lengths differ: " + strconv.Itoa(len(a)) + " vs " + strconv.Itoa(len(b))
	}
	return "identical"
}

// Import refuses a workspace that already holds rows, and leaves it exactly as it was.
func TestPlatformData_importIntoAPopulatedWorkspaceIs409(t *testing.T) {
	routes, _, platform := newPlatformDataAPI(t)
	provisionForData(t, routes, "acme", "book.acme.example")
	seedTenantData(t, platform, "acme")

	export := doPlatformSub(t, routes["export"], http.MethodPost,
		"/v1/platform/workspaces/acme/export", "acme", nil, platformToken)
	if export.Code != http.StatusOK {
		t.Fatalf("export: %d — %s", export.Code, export.Body.String())
	}

	before := tenantRowCounts(t, platform, "acme")

	rec := doPlatformSub(t, routes["import"], http.MethodPost,
		"/v1/platform/workspaces/acme/import", "acme", export.Body.Bytes(), platformToken)
	if rec.Code != http.StatusConflict {
		t.Fatalf("import into a populated workspace: %d; want 409 — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already has") {
		t.Errorf("409 body = %s; want it to name the table that is not empty", rec.Body.String())
	}

	after := tenantRowCounts(t, platform, "acme")
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s went from %d to %d rows; a refused import must change nothing",
				table, n, after[table])
		}
	}
}

// ⛔ A document exported from B, imported into A, must produce A's rows — not B's. The
// endpoint is authorised by the platform token, so trusting the document's own workspace_id
// would make an export of any workspace a way to write into any other.
func TestPlatformData_importForcesTheTargetWorkspace(t *testing.T) {
	routes, app, platform := newPlatformDataAPI(t)
	provisionForData(t, routes, "globex", "book.globex.example")
	seedTenantData(t, platform, "globex")

	export := doPlatformSub(t, routes["export"], http.MethodPost,
		"/v1/platform/workspaces/globex/export", "globex", nil, platformToken)
	if export.Code != http.StatusOK {
		t.Fatalf("export globex: %d — %s", export.Code, export.Body.String())
	}
	document := export.Body.Bytes()
	if !bytes.Contains(document, []byte(`"globex"`)) {
		t.Fatal("the document does not mention globex, so this test would prove nothing")
	}

	// ⚠️ The source workspace is deleted first, and that is not tidying: ids are GLOBAL
	// primary keys, so importing a document into a second workspace while the first still
	// holds its rows collides on users_pkey. The supported operation is a MOVE — export,
	// delete, import, usually into another region's instance where the rows do not exist —
	// and this test performs it inside one database because that is where the workspace_id
	// question can be asked.
	if rec := doPlatform(t, routes["delete"], http.MethodDelete,
		"/v1/platform/workspaces/globex", nil, platformToken); rec.Code != http.StatusOK {
		t.Fatalf("delete globex: %d — %s", rec.Code, rec.Body.String())
	}

	// An empty target workspace, with a different id from the one the document names.
	if _, err := platform.Exec(`
		INSERT INTO workspaces (id, slug, public_host, region, status, created_at, updated_at)
		VALUES ('acme', 'acme', 'book.acme.example', 'us', 'active', '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed acme: %v", err)
	}

	if rec := doPlatformSub(t, routes["import"], http.MethodPost,
		"/v1/platform/workspaces/acme/import", "acme", document, platformToken); rec.Code != http.StatusOK {
		t.Fatalf("import into acme: %d — %s", rec.Code, rec.Body.String())
	}

	// Every imported row belongs to acme, and globex still has exactly its own.
	var acmeBookings, globexBookings int
	if err := platform.QueryRow(
		`SELECT COUNT(*) FROM bookings WHERE workspace_id = 'acme'`).Scan(&acmeBookings); err != nil {
		t.Fatalf("count acme bookings: %v", err)
	}
	if err := platform.QueryRow(
		`SELECT COUNT(*) FROM bookings WHERE workspace_id = 'globex'`).Scan(&globexBookings); err != nil {
		t.Fatalf("count globex bookings: %v", err)
	}
	if acmeBookings != 1 || globexBookings != 0 {
		t.Errorf("bookings: acme %d, globex %d; want 1 and 0 — every row in a document that "+
			"says \"globex\" must land in the workspace named by the URL", acmeBookings, globexBookings)
	}

	// And acme's own bound handle can see them, which is the whole point of forcing the id:
	// a row carrying globex's workspace_id would be invisible to acme under the policies.
	var visible int
	if err := app.ForWorkspace("acme").QueryRow(`SELECT COUNT(*) FROM bookings`).Scan(&visible); err != nil {
		t.Fatalf("count as acme: %v", err)
	}
	if visible != 1 {
		t.Errorf("acme's own handle sees %d bookings; want 1", visible)
	}
}

// Erasure: exactly that email, in exactly that workspace, cancelling nothing.
func TestPlatformData_eraseAttendee(t *testing.T) {
	routes, _, platform := newPlatformDataAPI(t)
	provisionForData(t, routes, "acme", "book.acme.example")
	provisionForData(t, routes, "globex", "book.globex.example")
	seedTenantData(t, platform, "acme")
	seedTenantData(t, platform, "globex") // the same address, in another workspace

	// A second attendee on acme's booking, so the answer must SURVIVE: with someone else
	// still on the booking, the answers cannot be attributed to the erased person.
	if _, err := platform.Exec(`
		INSERT INTO booking_attendees (id, workspace_id, booking_id, name, email, iana_timezone, is_organizer)
		VALUES ('acme-att-bob', 'acme', 'acme-booking', 'Bob', 'bob@example.test', 'UTC', 0)`); err != nil {
		t.Fatalf("seed second attendee: %v", err)
	}

	rec := doPlatformSub(t, routes["erase"], http.MethodDelete,
		"/v1/platform/workspaces/acme/attendees?email=ada@example.test", "acme", nil, platformToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("erase: %d — %s", rec.Code, rec.Body.String())
	}
	var counts struct {
		Attendees int `json:"booking_attendees"`
		Answers   int `json:"booking_answers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &counts); err != nil {
		t.Fatalf("decode counts: %v", err)
	}
	if counts.Attendees != 1 {
		t.Errorf("booking_attendees erased = %d; want 1", counts.Attendees)
	}
	if counts.Answers != 0 {
		t.Errorf("booking_answers erased = %d; want 0 — another attendee is still on that "+
			"booking, so the answers are not unambiguously the erased person's", counts.Answers)
	}

	// Ada is gone from acme, Bob is not, and the booking is untouched.
	var ada, bob, bookings, status int
	if err := platform.QueryRow(
		`SELECT COUNT(*) FROM booking_attendees WHERE workspace_id = 'acme' AND email = 'ada@example.test'`).Scan(&ada); err != nil {
		t.Fatalf("count ada: %v", err)
	}
	if err := platform.QueryRow(
		`SELECT COUNT(*) FROM booking_attendees WHERE workspace_id = 'acme' AND email = 'bob@example.test'`).Scan(&bob); err != nil {
		t.Fatalf("count bob: %v", err)
	}
	if err := platform.QueryRow(
		`SELECT COUNT(*) FROM bookings WHERE workspace_id = 'acme'`).Scan(&bookings); err != nil {
		t.Fatalf("count bookings: %v", err)
	}
	if err := platform.QueryRow(
		`SELECT COUNT(*) FROM bookings WHERE workspace_id = 'acme' AND status = 'confirmed'`).Scan(&status); err != nil {
		t.Fatalf("count confirmed: %v", err)
	}
	if ada != 0 || bob != 1 {
		t.Errorf("after erasure acme has %d ada and %d bob rows; want 0 and 1", ada, bob)
	}
	if bookings != 1 || status != 1 {
		t.Errorf("bookings = %d (%d confirmed); erasure cancels nothing", bookings, status)
	}

	// ⛔ And globex's Ada is untouched. The erasure names a workspace, and an erasure
	// request from one tenant is not consent to delete another tenant's records.
	var globexAda int
	if err := platform.QueryRow(
		`SELECT COUNT(*) FROM booking_attendees WHERE workspace_id = 'globex' AND email = 'ada@example.test'`).Scan(&globexAda); err != nil {
		t.Fatalf("count globex ada: %v", err)
	}
	if globexAda != 1 {
		t.Errorf("globex's ada rows = %d; want 1 — erasure must not cross the workspace boundary", globexAda)
	}
}

// The only-attendee case: with nobody else on the booking, the answers go too.
func TestPlatformData_eraseTakesAnswersWhenNobodyElseIsOnTheBooking(t *testing.T) {
	routes, _, platform := newPlatformDataAPI(t)
	provisionForData(t, routes, "acme", "book.acme.example")
	seedTenantData(t, platform, "acme")

	rec := doPlatformSub(t, routes["erase"], http.MethodDelete,
		"/v1/platform/workspaces/acme/attendees?email=ada@example.test", "acme", nil, platformToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("erase: %d — %s", rec.Code, rec.Body.String())
	}
	var counts struct {
		Attendees int `json:"booking_attendees"`
		Answers   int `json:"booking_answers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &counts); err != nil {
		t.Fatalf("decode counts: %v", err)
	}
	if counts.Attendees != 1 || counts.Answers != 1 {
		t.Errorf("erased %d attendees and %d answers; want 1 and 1 — she was the only "+
			"attendee, so the answers were hers", counts.Attendees, counts.Answers)
	}
}

func TestPlatformData_eraseRequiresAnEmail(t *testing.T) {
	routes, _, _ := newPlatformDataAPI(t)
	provisionForData(t, routes, "acme", "book.acme.example")

	for name, query := range map[string]string{
		"missing":      "",
		"not an email": "?email=nobody",
		"empty":        "?email=",
	} {
		t.Run(name, func(t *testing.T) {
			rec := doPlatformSub(t, routes["erase"], http.MethodDelete,
				"/v1/platform/workspaces/acme/attendees"+query, "acme", nil, platformToken)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d; want 400 — %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// All three routes are behind the same token gate as the rest of the platform API.
func TestPlatformData_routesRefuseWithoutTheToken(t *testing.T) {
	routes, _, _ := newPlatformDataAPI(t)
	provisionForData(t, routes, "acme", "book.acme.example")

	for _, c := range []struct {
		name, method, target, route string
	}{
		{"export", http.MethodPost, "/v1/platform/workspaces/acme/export", "export"},
		{"import", http.MethodPost, "/v1/platform/workspaces/acme/import", "import"},
		{"erase", http.MethodDelete, "/v1/platform/workspaces/acme/attendees?email=a@b.test", "erase"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := doPlatformSub(t, routes[c.route], c.method, c.target, "acme", []byte(`{}`), "")
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d; want 401 without a token", rec.Code)
			}
		})
	}
}

// A document whose dek_fingerprint names another instance's data key is refused, because
// every _enc column in it would be undecryptable here — one integration at a time, long
// after anyone is reading this response.
func TestPlatformData_importRefusesAForeignDEK(t *testing.T) {
	routes, _, platform := newPlatformDataAPI(t)
	provisionForData(t, routes, "acme", "book.acme.example")

	export := doPlatformSub(t, routes["export"], http.MethodPost,
		"/v1/platform/workspaces/acme/export", "acme", nil, platformToken)
	if export.Code != http.StatusOK {
		t.Fatalf("export: %d — %s", export.Code, export.Body.String())
	}

	var doc map[string]any
	if err := json.Unmarshal(export.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	doc["dek_fingerprint"] = "sha256:" + strings.Repeat("ab", 32)
	foreign, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	if rec := doPlatform(t, routes["delete"], http.MethodDelete,
		"/v1/platform/workspaces/acme", nil, platformToken); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if _, err := platform.Exec(`
		INSERT INTO workspaces (id, slug, public_host, region, status, created_at, updated_at)
		VALUES ('acme', 'acme', 'book.acme.example', 'us', 'active', '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z')`); err != nil {
		t.Fatalf("re-create: %v", err)
	}

	rec := doPlatformSub(t, routes["import"], http.MethodPost,
		"/v1/platform/workspaces/acme/import", "acme", foreign, platformToken)
	if rec.Code != http.StatusConflict {
		t.Fatalf("import with a foreign dek_fingerprint: %d; want 409 — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "CALNODE_ENCRYPTION_KEY") {
		t.Errorf("409 body = %s; want it to name what the operator has to move", rec.Body.String())
	}
}

// tenantRowCounts counts every tenant table for a workspace, through the platform handle.
func tenantRowCounts(t *testing.T, platform *db.DB, wsID string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range db.TenantTables {
		var n int
		if err := platform.QueryRow(
			`SELECT COUNT(*) FROM `+table+` WHERE workspace_id = ?`, wsID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

// ⛔ Image bytes survive export and import exactly. The round-trip test above cannot show
// this on its own: if the bytes were exported as a Go string, encoding/json would replace
// every invalid UTF-8 sequence with U+FFFD, the import would store the damaged bytes, and
// a second export of them would match the first byte for byte. So the bytes are compared
// against the ones that went in, and the document is checked to carry them as base64.
func TestPlatformData_assetBytesSurviveTheRoundTrip(t *testing.T) {
	routes, _, platform := newPlatformDataAPI(t)
	provisionForData(t, routes, "acme", "book.acme.example")

	var ownerID string
	if err := platform.QueryRow(
		`SELECT id FROM users WHERE workspace_id = 'acme' AND is_owner = 1`).Scan(&ownerID); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	// A PNG signature and bytes that are not UTF-8 in any arrangement.
	logo := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe, 0x00, 0xc3, 0x28, 0x80}
	avatar := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0xa0, 0xa1}
	for _, a := range []struct {
		kind, owner, ctype string
		data               []byte
	}{{"logo", "", "image/png", logo}, {"avatar", ownerID, "image/jpeg", avatar}} {
		if _, err := platform.Exec(
			`INSERT INTO workspace_assets (workspace_id, kind, owner_id, content_type, data, sha256)
			 VALUES ('acme', ?, ?, ?, ?, ?)`,
			a.kind, a.owner, a.ctype, a.data, strings.Repeat("0", 64)); err != nil {
			t.Fatalf("seed %s: %v", a.kind, err)
		}
	}

	exp := doPlatformSub(t, routes["export"], http.MethodPost,
		"/v1/platform/workspaces/acme/export", "acme", nil, platformToken)
	if exp.Code != http.StatusOK {
		t.Fatalf("export: %d — %s", exp.Code, exp.Body.String())
	}
	document := exp.Body.Bytes()
	if !bytes.Contains(document, []byte(`"data":"`+base64.StdEncoding.EncodeToString(logo)+`"`)) {
		t.Errorf("the export does not carry the logo as base64")
	}

	if rec := doPlatform(t, routes["delete"], http.MethodDelete,
		"/v1/platform/workspaces/acme", nil, platformToken); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d — %s", rec.Code, rec.Body.String())
	}
	var left int
	if err := platform.QueryRow(`SELECT COUNT(*) FROM workspace_assets WHERE workspace_id = 'acme'`).Scan(&left); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if left != 0 {
		t.Fatalf("deleting the workspace left %d asset rows; the foreign key should cascade", left)
	}
	if _, err := platform.Exec(`
		INSERT INTO workspaces (id, slug, public_host, region, status)
		VALUES ('acme', 'acme', 'book.acme.example', 'us', 'active')`); err != nil {
		t.Fatalf("re-create workspace: %v", err)
	}
	if imp := doPlatformSub(t, routes["import"], http.MethodPost,
		"/v1/platform/workspaces/acme/import", "acme", document, platformToken); imp.Code != http.StatusOK {
		t.Fatalf("import: %d — %s", imp.Code, imp.Body.String())
	}

	for kind, want := range map[string][]byte{"logo": logo, "avatar": avatar} {
		var got []byte
		if err := platform.QueryRow(
			`SELECT data FROM workspace_assets WHERE workspace_id = 'acme' AND kind = ?`, kind).Scan(&got); err != nil {
			t.Fatalf("read imported %s: %v", kind, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("imported %s = %x; want %x", kind, got, want)
		}
	}
}
