package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// These are internal tests because the pieces under test are: hostOnly,
// forWorkspace, and the four resolution errors.
//
// They run against ONE handle with multiTenant set, which is enough for
// everything here: resolution reads the workspaces table through platformDB, and
// on a single handle that is the handle itself. What they deliberately do not
// re-prove is the row-level-security binding — that needs a NOBYPASSRLS role and
// is already proven in internal/db (rls_proof_test.go, pair_test.go). Splitting it
// that way keeps this file about the resolver's decisions.

func newResolverHandler(t *testing.T, multiTenant bool) *Handler {
	t.Helper()
	database := dbtest.Open(t)
	h := New(database, slog.Default())
	h.SetMultiTenant(multiTenant)
	h.SetBaseURL("https://app.calnode.test")
	return h
}

func seedWorkspace(t *testing.T, h *Handler, id, host, status string) {
	t.Helper()
	if _, err := h.db.Exec(
		`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', ?)`,
		id, id, host, status); err != nil {
		t.Fatalf("seed workspace %s: %v", id, err)
	}
}

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"book.acme.com":               "book.acme.com",
		"book.acme.com:8443":          "book.acme.com",
		"https://book.acme.com":       "book.acme.com",
		"https://book.acme.com/":      "book.acme.com",
		"https://book.acme.com/admin": "book.acme.com",
		"BOOK.ACME.COM":               "book.acme.com",
		"  book.acme.com  ":           "book.acme.com",
		"localhost:3000":              "localhost",
		"[::1]:8080":                  "[::1]",
		"":                            "",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestHostWorkspace_singleTenantAlwaysDefault is the byte-identical promise at the
// resolver layer: with MULTI_TENANT unset nothing is read and nothing can fail.
func TestHostWorkspace_singleTenantAlwaysDefault(t *testing.T) {
	h := newResolverHandler(t, false)
	for _, host := range []string{"book.acme.com", "nowhere.example", ""} {
		r := httptest.NewRequest(http.MethodGet, "/book/intro", nil)
		r.Host = host
		ws, err := HostWorkspace(h, r)
		if err != nil {
			t.Fatalf("HostWorkspace(%q) = %v", host, err)
		}
		if ws != DefaultWorkspace {
			t.Errorf("HostWorkspace(%q) = %+v; want DefaultWorkspace", host, ws)
		}
	}
}

func TestHostWorkspace_multiTenant(t *testing.T) {
	h := newResolverHandler(t, true)
	seedWorkspace(t, h, "acme", "book.acme.com", "active")
	seedWorkspace(t, h, "globex", "book.globex.com", "suspended")

	t.Run("known host", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/book/intro", nil)
		r.Host = "book.acme.com:8443"
		ws, err := HostWorkspace(h, r)
		if err != nil {
			t.Fatalf("HostWorkspace: %v", err)
		}
		if ws.ID != "acme" {
			t.Errorf("resolved %q; want acme", ws.ID)
		}
	})

	t.Run("unknown host does not fall back to default", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/book/intro", nil)
		r.Host = "nobody.example"
		ws, err := HostWorkspace(h, r)
		if err == nil {
			t.Fatalf("resolved %+v; want an error", ws)
		}
		// ⛔ The important half: not DefaultWorkspace. Falling back would serve
		// one tenant's booking page on any domain pointed at the instance.
		if !isErr(err, errUnknownHost) {
			t.Errorf("err = %v; want errUnknownHost", err)
		}
	})

	t.Run("the default workspace is unreachable by host", func(t *testing.T) {
		// Migration 00060 seeds it with an empty public_host precisely so that no
		// request can land on it, since no HTTP request carries an empty Host.
		r := httptest.NewRequest(http.MethodGet, "/book/intro", nil)
		r.Host = ""
		if _, err := HostWorkspace(h, r); !isErr(err, errUnknownHost) {
			t.Errorf("an empty Host resolved; err = %v", err)
		}
	})

	t.Run("suspended", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/book/intro", nil)
		r.Host = "book.globex.com"
		if _, err := HostWorkspace(h, r); !isErr(err, errWorkspaceSuspended) {
			t.Errorf("err = %v; want errWorkspaceSuspended", err)
		}
	})
}

func TestCredentialWorkspace(t *testing.T) {
	h := newResolverHandler(t, true)
	seedWorkspace(t, h, "acme", "book.acme.com", "active")
	seedWorkspace(t, h, "globex", "book.globex.com", "active")

	withUser := func(r *http.Request, wsID string) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), ctxKeyUser,
			AuthUser{ID: "u1", WorkspaceID: wsID}))
	}

	t.Run("credential decides", func(t *testing.T) {
		r := withUser(httptest.NewRequest(http.MethodGet, "/v1/bookings", nil), "acme")
		r.Host = "app.calnode.test" // the identity host: names no workspace
		ws, err := CredentialWorkspace(h, r)
		if err != nil {
			t.Fatalf("CredentialWorkspace: %v", err)
		}
		if ws.ID != "acme" {
			t.Errorf("resolved %q; want acme", ws.ID)
		}
	})

	t.Run("host agreeing is fine", func(t *testing.T) {
		r := withUser(httptest.NewRequest(http.MethodGet, "/v1/bookings", nil), "acme")
		r.Host = "book.acme.com"
		if _, err := CredentialWorkspace(h, r); err != nil {
			t.Errorf("CredentialWorkspace: %v", err)
		}
	})

	t.Run("host naming another workspace is a mismatch", func(t *testing.T) {
		r := withUser(httptest.NewRequest(http.MethodGet, "/v1/bookings", nil), "acme")
		r.Host = "book.globex.com"
		if _, err := CredentialWorkspace(h, r); !isErr(err, errWorkspaceMismatch) {
			t.Errorf("err = %v; want errWorkspaceMismatch", err)
		}
	})

	t.Run("host naming no workspace at all is fine", func(t *testing.T) {
		// API and MCP callers legitimately arrive on hosts that are not any
		// tenant's public host.
		r := withUser(httptest.NewRequest(http.MethodGet, "/v1/bookings", nil), "acme")
		r.Host = "some-proxy.internal"
		if _, err := CredentialWorkspace(h, r); err != nil {
			t.Errorf("CredentialWorkspace: %v", err)
		}
	})

	t.Run("no caller in context is a bug, not a client error", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/v1/bookings", nil)
		if _, err := CredentialWorkspace(h, r); !isErr(err, errNoWorkspace) {
			t.Errorf("err = %v; want errNoWorkspace", err)
		}
	})

	t.Run("a credential naming a deleted workspace", func(t *testing.T) {
		r := withUser(httptest.NewRequest(http.MethodGet, "/v1/bookings", nil), "gone")
		if _, err := CredentialWorkspace(h, r); !isErr(err, errNoWorkspace) {
			t.Errorf("err = %v; want errNoWorkspace", err)
		}
	})
}

// TestScoped_refusalsNeverReachTheMethod is the load-bearing assertion of the
// whole boundary: a request that resolves no tenant must not run the handler at
// all. Running it would be an unscoped read on a multi-tenant database.
func TestScoped_refusalsNeverReachTheMethod(t *testing.T) {
	h := newResolverHandler(t, true)
	seedWorkspace(t, h, "acme", "book.acme.com", "active")
	seedWorkspace(t, h, "globex", "book.globex.com", "suspended")

	var reached int
	method := func(scoped *Handler, w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"workspace": scoped.Workspace().ID})
	}

	cases := []struct {
		name       string
		host       string
		wantStatus int
		wantBody   string // "" = do not check
		wantReach  bool
	}{
		{name: "known host", host: "book.acme.com", wantStatus: 200, wantReach: true},
		{name: "unknown host is 404", host: "nobody.example", wantStatus: 404},
		{name: "suspended is 503", host: "book.globex.com", wantStatus: 503, wantBody: "workspace suspended"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = 0
			r := httptest.NewRequest(http.MethodGet, "/book/intro", nil)
			r.Host = tc.host
			rec := httptest.NewRecorder()

			h.Scoped(HostWorkspace, method)(rec, r)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d; want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if (reached == 1) != tc.wantReach {
				t.Errorf("the method ran %d times; wantReach=%v", reached, tc.wantReach)
			}
			if tc.wantBody != "" {
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body %q: %v", rec.Body.String(), err)
				}
				if body["error"] != tc.wantBody {
					t.Errorf("error = %q; want %q", body["error"], tc.wantBody)
				}
			}
		})
	}
}

func TestScoped_mismatchIs403WithTheDocumentedBody(t *testing.T) {
	h := newResolverHandler(t, true)
	seedWorkspace(t, h, "acme", "book.acme.com", "active")
	seedWorkspace(t, h, "globex", "book.globex.com", "active")

	var reached int
	method := func(*Handler, http.ResponseWriter, *http.Request) { reached++ }

	r := httptest.NewRequest(http.MethodGet, "/v1/bookings", nil)
	r.Host = "book.globex.com"
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyUser, AuthUser{ID: "u1", WorkspaceID: "acme"}))
	rec := httptest.NewRecorder()

	h.Scoped(CredentialWorkspace, method)(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	// The body is part of the contract (D10).
	if body["error"] != "workspace mismatch" {
		t.Errorf("error = %q; want \"workspace mismatch\"", body["error"])
	}
	if reached != 0 {
		t.Error("the method ran on a mismatched request")
	}
}

// TestScoped_aResolverReturningNothingIs500: an unscoped read must not be
// reachable even through a buggy resolver.
func TestScoped_aResolverReturningNothingIs500(t *testing.T) {
	h := newResolverHandler(t, true)
	var reached int
	rec := httptest.NewRecorder()

	h.Scoped(
		func(*Handler, *http.Request) (*Workspace, error) { return nil, nil },
		func(*Handler, http.ResponseWriter, *http.Request) { reached++ },
	)(rec, httptest.NewRequest(http.MethodGet, "/anything", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
	if reached != 0 {
		t.Error("the method ran with no workspace")
	}
}

// TestForWorkspace_isACheapValue. The per-request copy shares *shared, so a
// setting applied at boot is visible through every copy and a mutex is never
// copied (which is what go vet copylocks would otherwise catch).
func TestForWorkspace_isACheapValue(t *testing.T) {
	h := newResolverHandler(t, true)
	ws := &Workspace{ID: "acme", Slug: "acme", PublicHost: "book.acme.com", Status: "active"}

	scoped := h.forWorkspace(ws)

	if scoped == h {
		t.Fatal("forWorkspace returned the same handler in multi-tenant mode")
	}
	if scoped.shared != h.shared {
		t.Error("the copy does not share process state")
	}
	if scoped.Workspace().ID != "acme" {
		t.Errorf("scoped workspace = %q", scoped.Workspace().ID)
	}
	if h.Workspace().ID != db.DefaultWorkspaceID {
		t.Errorf("the base handler was mutated: %q", h.Workspace().ID)
	}
	if scoped.bookingSvc == h.bookingSvc {
		t.Error("the booking service was not rebound to the scoped handle")
	}
	if scoped.webhookSvc == h.webhookSvc {
		t.Error("the webhook service was not rebound to the scoped handle")
	}
	// A setting applied to the base handler after the copy is still visible,
	// because *shared is shared.
	h.SetBaseURL("https://later.example.test")
	if scoped.baseURL != "https://later.example.test" {
		t.Error("the copy does not see later changes to process state")
	}
}

// TestForWorkspace_singleTenantKeepsTheSameHandle: nothing is rebound and nothing
// is bound, so an existing deployment runs the statements it always ran.
func TestForWorkspace_singleTenantKeepsTheSameHandle(t *testing.T) {
	h := newResolverHandler(t, false)
	scoped := h.forWorkspace(&Workspace{ID: "acme"})

	if scoped.db != h.db {
		t.Error("the database handle was replaced in single-tenant mode")
	}
	if scoped.bookingSvc != h.bookingSvc {
		t.Error("the booking service was rebuilt in single-tenant mode")
	}
	if scoped.webhookSvc != h.webhookSvc {
		t.Error("the webhook service was rebuilt in single-tenant mode")
	}
}

func TestPublicURL(t *testing.T) {
	t.Run("single tenant keeps PUBLIC_BASE_URL", func(t *testing.T) {
		h := newResolverHandler(t, false)
		h.SetPublicBaseURL("https://book.acme.com")
		if got := h.publicURL(); got != "https://book.acme.com" {
			t.Errorf("publicURL() = %q", got)
		}
	})

	t.Run("single tenant falls back to BASE_URL", func(t *testing.T) {
		h := newResolverHandler(t, false)
		if got := h.publicURL(); got != "https://app.calnode.test" {
			t.Errorf("publicURL() = %q", got)
		}
	})

	t.Run("multi tenant uses the workspace's host", func(t *testing.T) {
		h := newResolverHandler(t, true)
		h.SetPublicBaseURL("https://ignored.example")
		scoped := h.forWorkspace(&Workspace{ID: "acme", PublicHost: "book.acme.com"})
		if got := scoped.publicURL(); got != "https://book.acme.com" {
			t.Errorf("publicURL() = %q; want the workspace's public host", got)
		}
	})

	t.Run("multi tenant with no public host falls back", func(t *testing.T) {
		// The default workspace, which exists but is unreachable by host.
		h := newResolverHandler(t, true)
		h.SetPublicBaseURL("https://fallback.example")
		if got := h.publicURL(); got != "https://fallback.example" {
			t.Errorf("publicURL() = %q", got)
		}
	})
}

// TestPlatform_runsOnThePlatformHandle: the identity-host routes are unscoped on
// purpose, and say so at registration rather than by omission.
func TestPlatform_runsOnThePlatformHandle(t *testing.T) {
	h := newResolverHandler(t, true)
	var seen string
	rec := httptest.NewRecorder()

	h.Platform(func(p *Handler, w http.ResponseWriter, r *http.Request) {
		seen = p.Workspace().ID
	})(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if seen != db.DefaultWorkspaceID {
		t.Errorf("a platform route ran as workspace %q", seen)
	}
}

// isErr is errors.Is without the import, kept local because the four resolution
// errors are wrapped with %w and compared by identity everywhere else.
func isErr(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
