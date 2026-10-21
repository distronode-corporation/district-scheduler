package crashreport

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/calnode/calnode/internal/buildinfo"
)

// withMockTransport starts the SDK with a transport that records instead of
// sending, and stops it again when the test ends. The DSN is never dialled.
func withMockTransport(t *testing.T) *sentry.MockTransport {
	t.Helper()
	tr := &sentry.MockTransport{}
	if err := start(sentry.ClientOptions{
		Dsn:       "https://public@example.invalid/1",
		Transport: tr,
		Release:   "test",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		enabled.Store(false)
		sentry.CurrentHub().BindClient(nil)
	})
	return tr
}

func TestInit_withoutDSNStartsNothing(t *testing.T) {
	t.Setenv("SENTRY_DSN", "")
	if Init(slog.New(slog.DiscardHandler)) {
		t.Fatal("Init reported enabled with no SENTRY_DSN")
	}
	if Enabled() {
		t.Fatal("Enabled() is true with no SENTRY_DSN")
	}
	if sentry.CurrentHub().Client().IsEnabled() {
		t.Fatal("an enabled Sentry client was bound with no SENTRY_DSN")
	}
	// And reporting is a no-op, not a panic.
	Recovered(errors.New("boom"), nil)
	Flush()
}

func TestInit_malformedDSNLeavesReportingOff(t *testing.T) {
	t.Setenv("SENTRY_DSN", "not a dsn")
	if Init(slog.New(slog.DiscardHandler)) {
		t.Fatal("Init reported enabled for a malformed DSN")
	}
	if Enabled() {
		t.Fatal("Enabled() is true after a failed init")
	}
}

func panicsWith(v any) { panic(v) }

func TestRecovered_sendsTheStackAndTagsButNoRequestData(t *testing.T) {
	tr := withMockTransport(t)

	func() {
		defer func() {
			Recovered(recover(), map[string]string{"path": "/manage/[redacted]", "method": "GET"})
		}()
		panicsWith(errors.New("index out of range"))
	}()

	events := tr.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Level != sentry.LevelFatal {
		t.Errorf("level = %q, want fatal", ev.Level)
	}
	if ev.Tags["path"] != "/manage/[redacted]" || ev.Tags["method"] != "GET" {
		t.Errorf("tags = %v, want the caller's path and method", ev.Tags)
	}
	if ev.Request != nil {
		t.Errorf("event carries request data: %+v", ev.Request)
	}
	if ev.User.IPAddress != "" {
		t.Errorf("event carries an IP address: %q", ev.User.IPAddress)
	}
	if len(ev.Exception) == 0 || ev.Exception[len(ev.Exception)-1].Stacktrace == nil {
		t.Fatalf("event has no exception stack: %+v", ev.Exception)
	}
	var frames []string
	for _, f := range ev.Exception[len(ev.Exception)-1].Stacktrace.Frames {
		frames = append(frames, f.Function)
	}
	if !strings.Contains(strings.Join(frames, " "), "panicsWith") {
		t.Errorf("stack does not reach the panic site; frames: %v", frames)
	}
}

func TestRecovered_aNonErrorValueStillCarriesAStack(t *testing.T) {
	tr := withMockTransport(t)

	func() {
		defer func() { Recovered(recover(), nil) }()
		panicsWith("plain string")
	}()

	events := tr.Events()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Message != "plain string" {
		t.Errorf("message = %q", events[0].Message)
	}
	if len(events[0].Threads) == 0 || events[0].Threads[0].Stacktrace == nil {
		t.Error("message event has no stack; AttachStacktrace is off")
	}
}

func TestRecovered_ignoresErrAbortHandler(t *testing.T) {
	tr := withMockTransport(t)
	Recovered(http.ErrAbortHandler, nil)
	if n := len(tr.Events()); n != 0 {
		t.Fatalf("got %d events for http.ErrAbortHandler, want 0", n)
	}
}

func TestRecoverAndRepanic_reportsThenPanicsAgain(t *testing.T) {
	tr := withMockTransport(t)

	var again any
	func() {
		defer func() { again = recover() }()
		func() {
			defer RecoverAndRepanic()
			panicsWith("fatal")
		}()
	}()

	if again != "fatal" {
		t.Errorf("re-panicked with %v, want the original value", again)
	}
	if n := len(tr.Events()); n != 1 {
		t.Errorf("got %d events, want 1", n)
	}
}

func TestRelease(t *testing.T) {
	if got := Release(buildinfo.Info{Version: "edge", Commit: "9580c40abc"}); got != "9580c40abc" {
		t.Errorf("with a commit: %q, want the commit", got)
	}
	if got := Release(buildinfo.Info{Version: "dev", Commit: "unknown"}); got != "district-scheduler@dev" {
		t.Errorf("without a commit: %q", got)
	}
}

func TestAsError(t *testing.T) {
	base := errors.New("boom")
	if err := AsError(base); !errors.Is(err, base) || err.Error() != "panic: boom" {
		t.Errorf("AsError(error) = %v", err)
	}
	if err := AsError(42); err.Error() != "panic: 42" {
		t.Errorf("AsError(42) = %v", err)
	}
}

// TestInit_realTransportDeliversAnEnvelope runs the SDK's own HTTP transport
// against a local ingest stand-in, so the DSN parsing, the envelope and the
// flush are the real ones, and checks what actually goes over the wire.
func TestInit_realTransportDeliversAnEnvelope(t *testing.T) {
	bodies := make(chan string, 4)
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- r.URL.Path + "\n" + string(b)
	}))
	defer ingest.Close()
	u, _ := url.Parse(ingest.URL)

	t.Setenv("SENTRY_DSN", "http://public@"+u.Host+"/42")
	t.Setenv("SENTRY_ENVIRONMENT", "")
	t.Setenv("SENTRY_RELEASE", "")
	t.Setenv("DISTRONODE_REGION", "ca")
	if !Init(slog.New(slog.DiscardHandler)) {
		t.Fatal("Init did not start with a DSN")
	}
	t.Cleanup(func() {
		enabled.Store(false)
		sentry.CurrentHub().BindClient(nil)
	})

	func() {
		defer func() { Recovered(recover(), map[string]string{"request_id": "abc"}) }()
		panicsWith(errors.New("over the wire"))
	}()
	Flush()

	var got string
	select {
	case got = <-bodies:
	case <-time.After(5 * time.Second):
		t.Fatal("no envelope reached the ingest")
	}
	if !strings.HasPrefix(got, "/api/42/envelope/") {
		t.Errorf("posted to %q, want the project's envelope endpoint", strings.SplitN(got, "\n", 2)[0])
	}
	for _, want := range []string{`"over the wire"`, `"environment":"production"`, `"region":"ca"`, `"request_id":"abc"`} {
		if !strings.Contains(got, want) {
			t.Errorf("envelope lacks %s", want)
		}
	}
	for _, never := range []string{`"ip_address"`, `"request":`, `"cookies"`, `"headers"`} {
		if strings.Contains(got, never) {
			t.Errorf("envelope carries %s", never)
		}
	}
}
