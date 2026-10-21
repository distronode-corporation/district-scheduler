package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/calnode/calnode/internal/crashreport"
)

// Bearer credentials in URL paths must reach neither the request log nor a crash
// report. The routes are DERIVED, not listed, so a new token route fails this test
// until redactTokenPaths covers it:
//
//   - the Go routes come from server.go through the classification gate's source scan
//     (http.ServeMux cannot enumerate its patterns; see routes_classified_test.go), and
//     every pattern with a {token} or {room} wildcard is one;
//   - the admin SPA's routes come from its route tree, where a [token] directory is a
//     client-side page whose URL (/admin/<dir>/<token>) the server still receives and
//     logs as an /admin/ request. The invite email links to one of these.
//
// Each route is then served through the real Logging and Recover middlewares, with
// crash reporting on and pointed at a local ingest, by a handler that panics, which is
// the one path where the path tag goes to Sentry.

// tokenWildcards are the route wildcard names whose value is a bearer credential.
var tokenWildcards = map[string]bool{"token": true, "room": true}

var (
	muxWildcardRe = regexp.MustCompile(`\{([A-Za-z_]+)(\.\.\.)?\}`)
	spaParamRe    = regexp.MustCompile(`^\[([A-Za-z_]+)\]$`)
)

type tokenRoute struct {
	pattern string // what the test mux registers
	path    string // a concrete request path carrying the sentinel
	method  string
	token   string
}

// goTokenRoutes returns every server.go route with a token wildcard, with a concrete
// path in which each token wildcard holds the sentinel.
func goTokenRoutes(t *testing.T, sentinel func() string) []tokenRoute {
	t.Helper()
	src, err := ReadServerSource()
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	var out []tokenRoute
	for _, r := range ScanClassifiedRoutes(src) {
		method, path := "GET", r.Pattern
		if m, p, ok := strings.Cut(r.Pattern, " "); ok {
			method, path = m, p
		}
		hasToken := false
		for _, m := range muxWildcardRe.FindAllStringSubmatch(path, -1) {
			if tokenWildcards[m[1]] {
				hasToken = true
			}
		}
		if !hasToken {
			continue
		}
		tok := sentinel()
		concrete := muxWildcardRe.ReplaceAllStringFunc(path, func(w string) string {
			if tokenWildcards[muxWildcardRe.FindStringSubmatch(w)[1]] {
				return tok
			}
			return "x"
		})
		out = append(out, tokenRoute{pattern: r.Pattern, path: concrete, method: method, token: tok})
	}
	return out
}

// spaTokenRoutes returns every admin SPA page with a token segment, as the /admin/
// path the server receives for it.
func spaTokenRoutes(t *testing.T, sentinel func() string) []tokenRoute {
	t.Helper()
	root := filepath.Join("..", "..", "frontend", "src", "routes")
	var out []tokenRoute
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == root {
			return err
		}
		m := spaParamRe.FindStringSubmatch(d.Name())
		if m == nil || !tokenWildcards[m[1]] {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		tok := sentinel()
		var segs []string
		for _, s := range strings.Split(filepath.ToSlash(rel), "/") {
			switch {
			case strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")"):
				continue // a SvelteKit route group adds no URL segment
			case spaParamRe.MatchString(s):
				if tokenWildcards[spaParamRe.FindStringSubmatch(s)[1]] {
					segs = append(segs, tok)
				} else {
					segs = append(segs, "x")
				}
			default:
				segs = append(segs, s)
			}
		}
		out = append(out, tokenRoute{pattern: "/admin/", path: "/admin/" + strings.Join(segs, "/"), method: "GET", token: tok})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// envelopeTags returns the tags of the event in a Sentry envelope body.
func envelopeTags(t *testing.T, body string) map[string]string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		var ev struct {
			Tags map[string]string `json:"tags"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Tags != nil {
			return ev.Tags
		}
	}
	t.Fatalf("no event with tags in envelope:\n%s", body)
	return nil
}

func TestTokenRoutesNeverReachLogsOrCrashReports(t *testing.T) {
	n := 0
	sentinel := func() string { n++; return fmt.Sprintf("SENTINELTOKEN%03d", n) }
	routes := append(goTokenRoutes(t, sentinel), spaTokenRoutes(t, sentinel)...)

	// The derivation must find the routes this test exists for; an empty scan would
	// pass everything below.
	want := map[string]bool{
		"GET /v1/invites/{token}":        false,
		"POST /v1/invites/{token}/claim": false,
		"GET /manage/{token}":            false,
		"GET /room/{room}":               false,
		"/admin/invite/":                 false,
	}
	for _, r := range routes {
		key := r.pattern
		if r.pattern == "/admin/" {
			key = "/admin/" + strings.Split(strings.TrimPrefix(r.path, "/admin/"), "/")[0] + "/"
		}
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("token route %q was not derived; the scan of server.go or of the SPA route tree is broken", k)
		}
	}

	// Crash reporting on, pointed at a local ingest.
	bodies := make(chan string, 8)
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- string(b)
	}))
	defer ingest.Close()
	u, _ := url.Parse(ingest.URL)
	t.Setenv("SENTRY_DSN", "http://public@"+u.Host+"/1")
	t.Setenv("SENTRY_ENVIRONMENT", "test")
	t.Setenv("SENTRY_RELEASE", "test")
	if !crashreport.Init(slog.New(slog.DiscardHandler)) {
		t.Fatal("crash reporting did not start")
	}
	// crashreport's own switch is not reachable from this package; with no client
	// bound, Recovered sends nothing, so later tests are unaffected.
	t.Cleanup(func() { sentry.CurrentHub().BindClient(nil) })

	// The real middlewares, around a mux holding the real patterns, every one of
	// which panics.
	panics := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("handler failed") })
	mux := http.NewServeMux()
	registered := map[string]bool{}
	for _, r := range routes {
		if !registered[r.pattern] {
			mux.Handle(r.pattern, panics)
			registered[r.pattern] = true
		}
	}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	srv := RequestID(Logging(logger, Recover(logger, mux)))

	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			logBuf.Reset()
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(r.method, r.path, nil))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 from the panicking handler (pattern %q did not match)", rec.Code, r.pattern)
			}
			crashreport.Flush()

			var body string
			select {
			case body = <-bodies:
			case <-time.After(5 * time.Second):
				t.Fatal("no crash report reached the ingest")
			}
			if tag := envelopeTags(t, body)["path"]; !strings.HasSuffix(tag, "[redacted]") {
				t.Errorf("Sentry path tag = %q, want it redacted", tag)
			}
			if strings.Contains(body, r.token) {
				t.Errorf("the token reached the crash report")
			}

			logs := logBuf.String()
			if !strings.Contains(logs, "[redacted]") {
				t.Errorf("no redacted path in the log lines:\n%s", logs)
			}
			if strings.Contains(logs, r.token) {
				t.Errorf("the token reached the log lines:\n%s", logs)
			}
		})
	}
}
