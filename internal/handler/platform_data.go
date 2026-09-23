package handler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/db"
)

// Export, import and attendee erasure (D12).
//
// Everything here is Platform-wrapped, so h.db is the platform handle: unscoped, policy
// bypassing, and binding ''. Two rules follow, and they are the same two the rest of
// platform.go obeys — every read carries its own workspace_id predicate, and every INSERT
// names workspace_id. Import goes further and names the workspace from the URL rather than
// from the document, so an import into A cannot write a row for B even if the document
// says otherwise.

// exportTableOrder is the replay order: parents before children.
//
// ⛔ It is a list rather than db.TenantTables' alphabetical order because
// alphabetical is a foreign-key violation waiting to happen — booking_answers before
// bookings, event_type_hosts before event_types. Import replays the document in the order
// the document carries, so this order IS the contract.
//
// ⚠️ It is checked against db.TenantTables at export time (exportCoversEveryTenantTable),
// so a table added by a later migration fails the export loudly instead of being quietly
// left out of every tenant's backup. That check is the reason this list can be trusted.
var exportTableOrder = []string{
	// The tenant's own settings and people first: almost everything references a user.
	"server_settings",
	"users",
	"teams",
	"team_members",
	// Event types and what hangs off them.
	"event_types",
	"event_type_hosts",
	"event_type_questions",
	"event_type_reminders",
	"availability_rules",
	"availability_overrides",
	// Bookings and their children.
	"bookings",
	"booking_hosts",
	"booking_attendees",
	"booking_answers",
	"booking_manage_tokens",
	// Meeting artefacts.
	"notes",
	"transcripts",
	"recordings",
	"meeting_consents",
	// Integrations.
	"calendar_connections",
	"connection_calendars",
	"zoom_connections",
	// Credentials and sessions. Included verbatim, ciphertext and hashes alike: the
	// destination has to accept the same API keys and manage links, or a migrated tenant's
	// integrations all break at once.
	"api_keys",
	"sessions",
	"magic_link_tokens",
	"password_reset_tokens",
	"invite_tokens",
	"oauth_access_tokens",
	"oauth_auth_codes",
	// Uploaded images. After users: an avatar row names its user.
	"workspace_assets",
	// Subscriptions and their delivery history.
	"webhooks",
	"webhook_deliveries",
	// Queue and replay state.
	"jobs",
	"idempotency_keys",
}

// workspaceExport is the document. Tables are an ordered array rather than a map because
// the replay order is part of the data, and Go map iteration would shuffle it.
type workspaceExport struct {
	FormatVersion  int             `json:"format_version"`
	ExportedAt     string          `json:"exported_at"`
	Workspace      map[string]any  `json:"workspace"`
	DEKFingerprint string          `json:"dek_fingerprint"`
	Tables         []exportedTable `json:"tables"`
	RowCounts      map[string]int  `json:"row_counts"`
}

type exportedTable struct {
	Table string           `json:"table"`
	Rows  []map[string]any `json:"rows"`
}

// ExportWorkspace handles POST /v1/platform/workspaces/{id}/export.
//
// One JSON document holding every tenant-table row for the workspace, in replay order,
// with workspace_id on every row and secrets included verbatim.
//
// ⛔ Secrets in the clear-as-ciphertext: the _enc columns and the API-key hashes travel as
// they are stored, because the destination must accept the same credentials — a tenant
// whose keys and manage links stopped working on migration has not been migrated. The
// consequence is that this document is as sensitive as the database, and the reason
// dek_fingerprint exists (see below).
func (h *Handler) ExportWorkspace(w http.ResponseWriter, r *http.Request) {
	if !h.platformAuthorized(w, r) {
		return
	}
	id := r.PathValue("id")

	ws, err := h.readPlatformWorkspace(r.Context(), id)
	if err != nil {
		h.writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if err := exportCoversEveryTenantTable(); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: export table list is stale", "error", err)
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	fingerprint, err := h.dekFingerprint(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: dek fingerprint", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	out := workspaceExport{
		FormatVersion:  1,
		ExportedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Workspace:      ws,
		DEKFingerprint: fingerprint,
		RowCounts:      map[string]int{},
	}
	for _, table := range exportTableOrder {
		rows, err := h.exportTable(r.Context(), table, id)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "platform: export table", "table", table, "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		out.Tables = append(out.Tables, exportedTable{Table: table, Rows: rows})
		out.RowCounts[table] = len(rows)
	}

	h.logger.InfoContext(r.Context(), "platform: workspace exported", "workspace_id", id)
	h.writeJSON(w, http.StatusOK, out)
}

// exportTable reads one table's rows for a workspace, generically.
//
// SELECT * and rows.Columns() rather than 32 hand-written column lists: a migration that
// adds a column would silently stop exporting it otherwise, and the failure would only
// show up as data missing after an import. ORDER BY 1 makes two exports of the same
// workspace byte-identical, which is what the round-trip test compares.
func (h *Handler) exportTable(ctx context.Context, table, workspaceID string) ([]map[string]any, error) {
	// table comes from exportTableOrder, a package-level literal checked against
	// db.TenantTables — never from the request.
	rows, err := h.db.QueryContext(ctx,
		`SELECT * FROM `+table+` WHERE workspace_id = ? ORDER BY 1`, workspaceID) // #nosec G202
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", table, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns of %s: %w", table, err)
	}

	out := []map[string]any{}
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(any)
		}
		if err := rows.Scan(cells...); err != nil {
			return nil, fmt.Errorf("scan %s: %w", table, err)
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			v := *(cells[i].(*any))
			// TEXT arrives as []byte from one driver and string from the other; both are
			// text as far as this schema is concerned, and normalising here is what makes
			// an export from PostgreSQL replayable and comparable.
			//
			// ⛔ Except a binary column, which is base64 in the document. Its bytes are not
			// text, and a Go string holding them is re-encoded by encoding/json with every
			// invalid UTF-8 sequence replaced by U+FFFD: the image would come back as a
			// different, undecodable file (or, holding a NUL, be refused by PostgreSQL on
			// import), and a second export of the damaged rows would still match the first
			// byte for byte.
			if b, ok := v.([]byte); ok {
				if isBinaryColumn(table, col) {
					v = base64.StdEncoding.EncodeToString(b)
				} else {
					v = string(b)
				}
			}
			row[col] = v
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ImportWorkspace handles POST /v1/platform/workspaces/{id}/import.
//
// The inverse of export, in one transaction, refusing a workspace that already holds rows.
func (h *Handler) ImportWorkspace(w http.ResponseWriter, r *http.Request) {
	if !h.platformAuthorized(w, r) {
		return
	}
	id := r.PathValue("id")

	if _, err := h.readPlatformWorkspace(r.Context(), id); err != nil {
		h.writeError(w, http.StatusNotFound, "workspace not found")
		return
	}

	// 64 MB: an export of a busy workspace carries its whole delivery history.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	dec := json.NewDecoder(r.Body)
	// ⛔ UseNumber, so 9007199254740993 survives. Without it every numeric column round
	// trips through float64 and large ids lose their low bits silently.
	dec.UseNumber()
	var doc workspaceExport
	if err := dec.Decode(&doc); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if doc.FormatVersion != 1 {
		h.writeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported format_version %d", doc.FormatVersion))
		return
	}

	// ⛔ The DEK check, and it is a refusal rather than a warning. Every _enc column in the
	// document is ciphertext under the SOURCE instance's data key. If the destination's key
	// differs, the rows import perfectly and then every secret in them — SMTP password,
	// LLM key, LiveKit secret, calendar tokens — fails to decrypt at first use, one
	// integration at a time, long after anyone is watching this response. A mismatch here
	// is the only moment the two keys can be compared.
	fingerprint, err := h.dekFingerprint(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: dek fingerprint", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if doc.DEKFingerprint != "" && doc.DEKFingerprint != fingerprint {
		h.writeError(w, http.StatusConflict,
			"dek_fingerprint does not match this instance: every encrypted column in the "+
				"document would be undecryptable here. Move CALNODE_ENCRYPTION_KEY with the data, "+
				"or re-key the source before exporting")
		return
	}

	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: import begin tx", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck

	// Refuse a workspace that holds anything. Checked INSIDE the transaction so the check
	// and the inserts cannot be interleaved by a second import.
	for _, table := range exportTableOrder {
		var n int
		if err := tx.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM `+table+` WHERE workspace_id = ?`, id).Scan(&n); err != nil { // #nosec G202
			h.logger.ErrorContext(r.Context(), "platform: import precheck", "table", table, "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if n > 0 {
			h.writeError(w, http.StatusConflict,
				fmt.Sprintf("workspace %s already has %d rows in %s: import only into an empty workspace", id, n, table))
			return
		}
	}

	imported := map[string]int{}
	for _, t := range doc.Tables {
		if !isExportableTable(t.Table) {
			h.writeError(w, http.StatusBadRequest, "unknown table in document: "+t.Table)
			return
		}
		for _, row := range t.Rows {
			if err := importRow(r.Context(), tx, t.Table, id, row); err != nil {
				h.logger.ErrorContext(r.Context(), "platform: import row",
					"table", t.Table, "error", err)
				h.writeError(w, http.StatusBadRequest,
					fmt.Sprintf("import %s: %v", t.Table, err))
				return
			}
		}
		imported[t.Table] = len(t.Rows)
	}

	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: import commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.logger.InfoContext(r.Context(), "platform: workspace imported", "workspace_id", id)
	h.writeJSON(w, http.StatusOK, map[string]any{"imported": imported})
}

// importRow inserts one exported row, forcing workspace_id to the target workspace.
//
// ⛔ Forcing it is the safety property, not a convenience: a document exported from
// workspace B carries workspace_id = "b" on every row, and replaying it into A must produce
// A's rows or fail. Trusting the document would make this endpoint a way to write into any
// tenant, authorised only by holding an export of another one.
func importRow(ctx context.Context, tx *db.Tx, table, workspaceID string, row map[string]any) error {
	cols := make([]string, 0, len(row)+1)
	args := make([]any, 0, len(row)+1)
	for col, v := range row {
		if col == "workspace_id" {
			continue
		}
		if !validColumnName(col) {
			return fmt.Errorf("invalid column name %q", col)
		}
		value := importValue(v)
		if isBinaryColumn(table, col) {
			encoded, ok := v.(string)
			if !ok {
				return fmt.Errorf("column %s.%s must be a base64 string", table, col)
			}
			raw, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return fmt.Errorf("column %s.%s is not valid base64: %w", table, col, err)
			}
			value = raw
		}
		cols = append(cols, col)
		args = append(args, value)
	}
	cols = append(cols, "workspace_id")
	args = append(args, workspaceID)

	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")
	_, err := tx.ExecContext(ctx,
		`INSERT INTO `+table+` (`+strings.Join(cols, ", ")+`) VALUES (`+placeholders+`)`, args...) // #nosec G202 -- table is allowlisted against db.TenantTables and every column name is validated; all values are bound
	return err
}

// importValue converts a decoded JSON value into something both drivers accept.
//
// json.Number is carried as an int64 when it is one, so an INTEGER column receives an
// integer rather than a float or a string — the two engines disagree about what they do
// with the other two, and neither disagreement is visible until a later read.
func importValue(v any) any {
	n, ok := v.(json.Number)
	if !ok {
		return v
	}
	if i, err := n.Int64(); err == nil {
		return i
	}
	if f, err := n.Float64(); err == nil {
		return f
	}
	return n.String()
}

// validColumnName guards the one part of an import statement that is not a bind parameter.
// Column names come from a document a platform operator supplied, so they are checked
// rather than trusted; a name that is not [a-z0-9_] cannot be a column in this schema.
func validColumnName(col string) bool {
	if col == "" || len(col) > 64 {
		return false
	}
	for i := 0; i < len(col); i++ {
		c := col[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// binaryColumns are the columns export writes as base64 and import decodes from it, by
// table. Every other column is text or a number and travels as itself.
var binaryColumns = map[string]map[string]bool{
	"workspace_assets": {"data": true},
}

func isBinaryColumn(table, col string) bool { return binaryColumns[table][col] }

func isExportableTable(table string) bool {
	for _, t := range exportTableOrder {
		if t == table {
			return true
		}
	}
	return false
}

// exportCoversEveryTenantTable reports a tenant table that export would leave out, or an
// entry in the order that is not a tenant table at all.
//
// ⛔ This is what makes a backup trustworthy. Migration 00060's list of tenant tables is
// already guarded (TestTenancy_tableListsCoverTheSchema fails when a new table is in
// neither the tenant nor the exempt list), so checking THIS list against THAT one means a
// table added by a later migration cannot be silently absent from every export. It is
// checked at request time, not just in a test, because the consequence of being wrong is
// data that was never backed up.
func exportCoversEveryTenantTable() error {
	inOrder := map[string]bool{}
	for _, t := range exportTableOrder {
		inOrder[t] = true
	}
	var missing []string
	for _, t := range db.TenantTables {
		if !inOrder[t] {
			missing = append(missing, t)
		}
	}
	tenant := map[string]bool{}
	for _, t := range db.TenantTables {
		tenant[t] = true
	}
	var unknown []string
	for _, t := range exportTableOrder {
		if !tenant[t] {
			unknown = append(unknown, t)
		}
	}
	if len(missing) > 0 || len(unknown) > 0 {
		return fmt.Errorf("export table order is stale: missing %v, not tenant tables %v", missing, unknown)
	}
	return nil
}

// dekFingerprint identifies the data key this instance is using, without revealing it.
//
// ⛔ The DEK ITSELF DOES NOT TRAVEL, and that is a decision worth stating. crypto_keystore
// is exempt from tenancy (D2) and holds one wrapped DEK per PROCESS (D3) — there is no
// per-workspace row to carry, and manufacturing one would be worse than useless: an export
// of a single tenant would then contain the key that decrypts EVERY tenant's secrets on
// that instance, which is the opposite of what the isolation is for.
//
// What travels instead is a fingerprint: SHA-256 over the wrapped DEK, which is already a
// ciphertext and cannot be reversed to the key. Equal fingerprints mean the two instances
// share a data key, so the document's _enc columns will decrypt. Different ones mean they
// will not, and import refuses.
//
// A per-tenant DEK would change this (and D3, and the schema). Until then, moving a
// workspace between instances means moving CALNODE_ENCRYPTION_KEY with it.
func (h *Handler) dekFingerprint(ctx context.Context) (string, error) {
	var wrapped []byte
	err := h.db.QueryRowContext(ctx,
		`SELECT wrapped_dek FROM crypto_keystore WHERE label = 'primary'`).Scan(&wrapped)
	if err != nil {
		// No keystore row at all is a legitimate state (an instance that has never
		// encrypted anything), and it fingerprints as empty so an export from one such
		// instance imports into another without a spurious conflict.
		if strings.Contains(err.Error(), "no rows") {
			return "", nil
		}
		return "", fmt.Errorf("read keystore: %w", err)
	}
	sum := sha256.Sum256(wrapped)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// EraseAttendee handles DELETE /v1/platform/workspaces/{id}/attendees?email=
//
// An erasure request for one person in one workspace. It cancels nothing: the bookings
// stay, because the host's calendar and the other attendees' records are not the erased
// person's data to remove, and a cancellation would notify people about someone else's
// deletion request.
func (h *Handler) EraseAttendee(w http.ResponseWriter, r *http.Request) {
	if !h.platformAuthorized(w, r) {
		return
	}
	id := r.PathValue("id")
	email := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
	if email == "" || !strings.Contains(email, "@") {
		h.writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	if _, err := h.readPlatformWorkspace(r.Context(), id); err != nil {
		h.writeError(w, http.StatusNotFound, "workspace not found")
		return
	}

	tx, err := h.db.BeginTx(r.Context(), nil)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: erase begin tx", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback() //nolint:errcheck

	// ⚠️ Answers are keyed on (booking_id, question_id) and carry no attendee, so "their
	// answers" has to be derived. They are erased only for bookings where this person was
	// the ONLY attendee — with anyone else still on the booking, the answers cannot be
	// attributed to the erased person, and deleting them would erase a third party's data
	// to satisfy someone else's request.
	answers, err := tx.ExecContext(r.Context(), `
		DELETE FROM booking_answers
		 WHERE workspace_id = ?
		   AND booking_id IN (
		       SELECT booking_id FROM booking_attendees
		        WHERE workspace_id = ? AND LOWER(email) = ?)
		   AND booking_id NOT IN (
		       SELECT booking_id FROM booking_attendees
		        WHERE workspace_id = ? AND LOWER(email) <> ?)`,
		id, id, email, id, email)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: erase answers", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	attendees, err := tx.ExecContext(r.Context(),
		`DELETE FROM booking_attendees WHERE workspace_id = ? AND LOWER(email) = ?`, id, email)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "platform: erase attendees", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := tx.Commit(); err != nil {
		h.logger.ErrorContext(r.Context(), "platform: erase commit", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	answerCount, _ := answers.RowsAffected()
	attendeeCount, _ := attendees.RowsAffected()
	h.logger.InfoContext(r.Context(), "platform: attendee erased",
		"workspace_id", id, "attendees", attendeeCount, "answers", answerCount)
	h.writeJSON(w, http.StatusOK, map[string]any{
		"booking_attendees": attendeeCount,
		"booking_answers":   answerCount,
	})
}
