package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/webhook"
)

// errNoRows is sql.ErrNoRows under a shorter name, because the two workspace
// reads below both branch on it and the line is long enough already.
var errNoRows = sql.ErrNoRows

// Workspace is a tenant. In single-tenant mode there is exactly one, and it is
// DefaultWorkspace.
type Workspace struct {
	ID         string
	Slug       string
	PublicHost string
	Region     string
	Status     string
}

// Suspended reports whether this workspace's public and admin surfaces should
// answer 503 (D12).
func (w *Workspace) Suspended() bool { return w != nil && w.Status == "suspended" }

// DefaultWorkspace is the tenant every request runs as when MULTI_TENANT is
// unset. Its id is the literal migration 00060 seeds and the SQLite column
// default, so a single-tenant database and a single-tenant handler agree without
// either having to ask.
//
// Its PublicHost is deliberately empty: publicURL falls back to
// PUBLIC_BASE_URL/BASE_URL for it, which is exactly the behaviour every existing
// deployment has.
var DefaultWorkspace = &Workspace{
	ID:     db.DefaultWorkspaceID,
	Slug:   db.DefaultWorkspaceID,
	Status: "active",
}

// The four ways resolution fails, each with its own response. They are values
// rather than status codes at the call site so a resolver can say what happened
// and one place decides what the client sees.
var (
	// errUnknownHost — the request's Host names no workspace. 404, and
	// deliberately NOT a fallback to the default workspace: on a multi-tenant
	// instance that would serve one tenant's booking page on an unrecognised
	// domain (D10).
	errUnknownHost = errors.New("handler: no workspace for this host")

	// errWorkspaceSuspended — 503 on public and admin surfaces.
	errWorkspaceSuspended = errors.New("handler: workspace suspended")

	// errWorkspaceMismatch — the credential resolved workspace A and the Host
	// names workspace B. 403 {"error":"workspace mismatch"} (D10).
	errWorkspaceMismatch = errors.New("handler: workspace mismatch")

	// errNoWorkspace — a resolver returned nothing and no reason. A bug, and a
	// 500: an unscoped read on a multi-tenant database is the one outcome that
	// must never be reachable by accident.
	errNoWorkspace = errors.New("handler: no workspace resolved")
)

// SetMultiTenant turns host and credential resolution on. Unset — the default —
// every request runs on DefaultWorkspace and nothing about the handler changes.
func (h *Handler) SetMultiTenant(v bool) { h.multiTenant = v }

// MultiTenant reports whether this handler resolves a tenant per request.
func (h *Handler) MultiTenant() bool { return h.multiTenant }

// Workspace returns the workspace this handler is scoped to. Never nil.
func (h *Handler) Workspace() *Workspace {
	if h.ws == nil {
		return DefaultWorkspace
	}
	return h.ws
}

// platformDB is the handle for reads that must cross workspaces, or that happen
// before a workspace is known: resolving a host, resolving a credential,
// enumerating tenants. In single-tenant mode it is the same handle as h.db.
//
// ⛔ Every credential lookup has to go through this. An api_keys or sessions read
// on the application handle runs bound to the workspace of the request, and the
// workspace of the request is what the credential is being read to DISCOVER — so
// it would find nothing, and the caller would see "invalid API key" for a
// perfectly good key.
func (h *Handler) platformDB() *db.DB { return h.db.Platform() }

// forWorkspace returns a copy of h scoped to ws.
//
// A value copy, so it costs an allocation and no locks: *shared is a pointer that
// every copy shares, and db.ForWorkspace pins no connection. That is what makes
// it safe for a handler to start a goroutine and let the request return —
// Calnode does that on every booking (notify hosts, enqueue the webhook, enqueue
// reminders).
func (h *Handler) forWorkspace(ws *Workspace) *Handler {
	if ws == nil {
		ws = DefaultWorkspace
	}
	scoped := *h
	scoped.ws = ws
	if !h.multiTenant {
		// Nothing to bind: one workspace, and db.ForWorkspace is the identity
		// function on a single handle anyway. Returning early keeps the
		// single-tenant path allocation-for-allocation what it was.
		return &scoped
	}
	// ⛔ From the APPLICATION handle, not from h.db. On a Platform-wrapped route h.db is the
	// platform handle, whose role bypasses the policies — binding a workspace onto that gives
	// a handle that names a tenant and is not confined to it. See shared.appDB.
	scoped.db = h.appBase().ForWorkspace(ws.ID)
	scoped.bookingSvc = h.bookingSvc.ForDB(scoped.db)
	scoped.webhookSvc = h.webhookSvc.ForDB(scoped.db)
	return &scoped
}

// appBase is the application handle every scoped copy is built from. It falls back to h.db for
// a handler constructed without one (older tests), where there is no pair and the two are the
// same handle anyway.
func (h *Handler) appBase() *db.DB {
	if h.appDB != nil {
		return h.appDB
	}
	return h.db
}

// workspaceForJob returns a handler scoped to a workspace named by a background
// job rather than by a request. The id comes from the jobs row (migration 00060),
// so it has already been through validation on the way in; an id that no longer
// names a workspace still yields a bound handle, which under the policies matches
// no row — the safe answer for a job whose tenant was deleted.
//
// Identity in single-tenant mode, where there is one workspace and nothing to bind.
func (h *Handler) workspaceForJob(workspaceID string) *Handler {
	if !h.multiTenant || workspaceID == "" {
		return h
	}
	return h.forWorkspace(&Workspace{ID: workspaceID, Status: "active"})
}

// TenantRuntime returns the per-workspace dependencies a background job needs: the
// bound database handle, and the mailer and webhook service built from THAT
// workspace's settings row.
//
// It is exported for internal/server to adapt into worker.TenantDeps. The handler
// deliberately does not import the worker package: the worker owns the queue, the
// handler owns the per-tenant state, and one closure in server.New joins them.
func (h *Handler) TenantRuntime(workspaceID string) (*db.DB, mailer.Mailer, *webhook.Service) {
	scoped := h.workspaceForJob(workspaceID)
	return scoped.db, scoped.getMailer(), scoped.webhookSvc
}

// JobPublicURL is publicURL for a workspace named by a background job: the
// booker-facing base URL its emails link to and load their branding images from.
//
// workspaceForJob carries only the id, so on a multi-tenant instance the public
// host is read here from the workspaces row. A workspace that cannot be read (or
// has no public host) answers "", never the identity host, whose /branding/ paths
// are no tenant's: the caller then leaves relative images out instead of pointing
// a mail client at another host. Single-tenant, it is PUBLIC_BASE_URL or BASE_URL.
func (h *Handler) JobPublicURL(ctx context.Context, workspaceID string) string {
	if !h.multiTenant || workspaceID == "" {
		return h.publicURL()
	}
	ws, err := h.workspaceByID(ctx, workspaceID)
	if err != nil {
		h.logger.WarnContext(ctx, "job: resolve workspace public host", "workspace", workspaceID, "error", err)
		return ""
	}
	if ws.PublicHost == "" {
		return ""
	}
	return h.forWorkspace(ws).publicURL()
}

// Resolver decides which workspace a request belongs to. The two that exist are
// HostWorkspace and CredentialWorkspace; a route picks one at registration.
type Resolver func(*Handler, *http.Request) (*Workspace, error)

// Scoped is the ONLY way a tenant route reaches a bound handle.
//
// It resolves the workspace, builds the per-request copy, and calls method on it.
// A request that resolves no workspace never reaches the method: it gets 404, 503,
// 403 or 500, because the alternative — running the method on an unscoped handle —
// is a cross-tenant read on a multi-tenant database.
//
// method is a method EXPRESSION, e.g. h.Scoped(HostWorkspace, (*Handler).BookPage),
// not a bound method value. That is deliberate and the compiler enforces it: a
// bound value would capture the unscoped receiver, which is precisely the bug this
// wrapper exists to make impossible.
func (h *Handler) Scoped(resolve Resolver, method func(*Handler, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, err := resolve(h, r)
		switch {
		case err != nil:
			h.writeResolveError(w, r, err)
			return
		case ws == nil:
			h.writeResolveError(w, r, errNoWorkspace)
			return
		}
		method(h.forWorkspace(ws), w, r)
	}
}

// Platform wraps a route that runs on the platform handle and belongs to no
// workspace: the identity host's OAuth endpoints, /.well-known/*, /healthz,
// /readyz, /version, /metrics and the platform API (D11).
//
// It exists so that "which routes are unscoped, and on purpose" is a question the
// registration answers out loud rather than by omission.
func (h *Handler) Platform(method func(*Handler, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform := *h
		platform.ws = DefaultWorkspace
		if h.multiTenant {
			platform.db = h.db.Platform()
			platform.bookingSvc = h.bookingSvc.ForDB(platform.db)
			platform.webhookSvc = h.webhookSvc.ForDB(platform.db)
		}
		method(&platform, w, r)
	}
}

// writeResolveError turns a resolution failure into a response.
//
// The unknown-host case is an HTML 404 rather than JSON because the surfaces that
// resolve by host are pages a person is looking at — a booking page, a manage
// link, the admin UI. Everything else is JSON, since a mismatch or a suspension
// is something a client has to read.
func (h *Handler) writeResolveError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errUnknownHost):
		h.logger.WarnContext(r.Context(), "no workspace for host", "host", r.Host, "path", r.URL.Path)
		http.NotFound(w, r)
	case errors.Is(err, errWorkspaceSuspended):
		w.Header().Set("Retry-After", "3600")
		h.writeError(w, http.StatusServiceUnavailable, "workspace suspended")
	case errors.Is(err, errWorkspaceMismatch):
		h.writeError(w, http.StatusForbidden, "workspace mismatch")
	default:
		h.logger.ErrorContext(r.Context(), "resolve workspace", "error", err,
			"host", r.Host, "path", r.URL.Path)
		h.writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// HostWorkspace resolves the tenant from the request Host. It is the resolver for
// every public and admin surface: /book/{slug}, /v1/event-types/{slug}/*,
// POST /v1/bookings, /embed.js, /manage/{token}, /room/{room}, /admin/*, and the
// branding files (D10).
//
// An unrecognised host is a 404. There is no default-tenant fallback, because on
// a multi-tenant instance that would answer one tenant's booking page on a domain
// nobody registered.
func HostWorkspace(h *Handler, r *http.Request) (*Workspace, error) {
	if !h.multiTenant {
		return DefaultWorkspace, nil
	}
	ws, err := h.workspaceByHost(r.Context(), r.Host)
	if err != nil {
		return nil, err
	}
	if ws.Suspended() {
		return nil, fmt.Errorf("%w: %s", errWorkspaceSuspended, ws.ID)
	}
	return ws, nil
}

// CredentialWorkspace resolves the tenant from the credential the request
// carries, which the auth middleware has already verified and put in context.
//
// Credential first, then the Host as a CHECK rather than a source (D10): an API
// key belongs to a user, a user belongs to a workspace, and that is the
// authoritative answer. If the Host also names a workspace and the two disagree,
// the request is refused 403 rather than either one silently winning.
func CredentialWorkspace(h *Handler, r *http.Request) (*Workspace, error) {
	if !h.multiTenant {
		return DefaultWorkspace, nil
	}
	user, ok := userFromContext(r.Context())
	if !ok {
		// Scoped ran before the auth middleware, or the route is registered with
		// the wrong resolver. Either way it is a bug, not a client error.
		return nil, fmt.Errorf("%w: no authenticated caller in context", errNoWorkspace)
	}
	if user.WorkspaceID == "" {
		return nil, fmt.Errorf("%w: authenticated caller has no workspace", errNoWorkspace)
	}
	ws, err := h.workspaceByID(r.Context(), user.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if ws.Suspended() {
		return nil, fmt.Errorf("%w: %s", errWorkspaceSuspended, ws.ID)
	}
	if err := h.hostAgrees(r, ws); err != nil {
		return nil, err
	}
	return ws, nil
}

// hostAgrees refuses a request whose Host names a DIFFERENT workspace from the
// one its credential resolved. A Host that names no workspace at all is fine:
// that is the identity host, which is where API and MCP callers legitimately
// arrive.
func (h *Handler) hostAgrees(r *http.Request, ws *Workspace) error {
	host := hostOnly(r.Host)
	if host == "" || host == hostOnly(h.baseURL) {
		return nil
	}
	byHost, err := h.workspaceByHost(r.Context(), r.Host)
	if errors.Is(err, errUnknownHost) {
		return nil
	}
	if err != nil {
		return err
	}
	if byHost.ID != ws.ID {
		return fmt.Errorf("%w: credential is %s, host %s is %s", errWorkspaceMismatch, ws.ID, host, byHost.ID)
	}
	return nil
}

// workspaceByHost reads a workspace by its public host, through the platform
// handle: the application handle is bound to the workspace of the request, and
// this read is what determines that.
func (h *Handler) workspaceByHost(ctx context.Context, requestHost string) (*Workspace, error) {
	host := hostOnly(requestHost)
	if host == "" {
		return nil, fmt.Errorf("%w: empty Host header", errUnknownHost)
	}
	var ws Workspace
	err := h.platformDB().QueryRowContext(ctx,
		`SELECT id, slug, public_host, region, status FROM workspaces WHERE public_host = ?`, host).
		Scan(&ws.ID, &ws.Slug, &ws.PublicHost, &ws.Region, &ws.Status)
	if err != nil {
		if errors.Is(err, errNoRows) {
			return nil, fmt.Errorf("%w: %s", errUnknownHost, host)
		}
		return nil, fmt.Errorf("read workspace by host %s: %w", host, err)
	}
	return &ws, nil
}

// activeWorkspaceIDs lists every workspace a background loop should act for, on the
// platform handle.
//
// Suspended workspaces are excluded here rather than at each call site, so a new
// periodic loop cannot forget. In single-tenant mode it answers with the one
// workspace, so a caller needs no branch — though reconcileTargets short-circuits
// before reaching it, to avoid a query per sweep on an instance that has one tenant.
func (h *Handler) activeWorkspaceIDs(ctx context.Context) ([]string, error) {
	if !h.multiTenant {
		return []string{db.DefaultWorkspaceID}, nil
	}
	rows, err := h.platformDB().QueryContext(ctx,
		`SELECT id FROM workspaces WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list active workspaces: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan workspace id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspaces: %w", err)
	}
	return ids, nil
}

// workspaceOfUser resolves the workspace a user belongs to, through the platform
// handle.
//
// ⛔ It exists for the INSERTs on Platform-wrapped routes. The platform handle
// binds ”, and the workspace_id column defaults to
// COALESCE(current_setting('app.workspace_id', true), 'default') — so a write that
// omits the column lands in the DEFAULT workspace, silently, rather than failing.
// A Platform route that writes a tenant row therefore has to name workspace_id,
// and this is where it comes from.
func (h *Handler) workspaceOfUser(ctx context.Context, userID string) (string, error) {
	if !h.multiTenant {
		return db.DefaultWorkspaceID, nil
	}
	var ws string
	if err := h.platformDB().QueryRowContext(ctx,
		`SELECT workspace_id FROM users WHERE id = ?`, userID).Scan(&ws); err != nil {
		return "", fmt.Errorf("resolve the workspace of user %s: %w", userID, err)
	}
	return ws, nil
}

// workspaceByID reads a workspace by id, through the platform handle.
func (h *Handler) workspaceByID(ctx context.Context, id string) (*Workspace, error) {
	var ws Workspace
	err := h.platformDB().QueryRowContext(ctx,
		`SELECT id, slug, public_host, region, status FROM workspaces WHERE id = ?`, id).
		Scan(&ws.ID, &ws.Slug, &ws.PublicHost, &ws.Region, &ws.Status)
	if err != nil {
		if errors.Is(err, errNoRows) {
			// The credential names a workspace that no longer exists. Not the
			// client's fault and not an unknown host either: a deleted workspace
			// whose sessions have outlived it.
			return nil, fmt.Errorf("%w: credential names workspace %q, which does not exist", errNoWorkspace, id)
		}
		return nil, fmt.Errorf("read workspace %s: %w", id, err)
	}
	return &ws, nil
}

// hostOnly strips a port, a scheme and any trailing slash, so a request Host
// ("book.acme.com:8443") and a configured URL ("https://book.acme.com/") compare
// equal to the public_host column, which stores the bare hostname.
//
// Lower-cased because DNS is case-insensitive and a browser will happily send
// whatever the user typed.
func hostOnly(host string) string {
	h := strings.TrimSpace(host)
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	h = strings.TrimSuffix(h, "/")
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	// IPv6 literals arrive as [::1]:8080; the brackets are part of the host.
	if strings.HasPrefix(h, "[") {
		if end := strings.IndexByte(h, ']'); end >= 0 {
			return strings.ToLower(h[:end+1])
		}
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i+1:], ".") {
		h = h[:i]
	}
	return strings.ToLower(h)
}
