// Package crashreport sends recovered panics to Sentry. It is fork-only code
// (upstream Calnode has no crash reporting) and is kept to this one package plus
// a line at each call site, so an upstream merge rarely has to look at it.
//
// It is OFF unless SENTRY_DSN is set. With no DSN, Init never calls sentry.Init,
// so a dev box, `go test` and CI start no SDK and send nothing. District AI sets
// the DSN in each region's district-scheduler-env Secret.
//
// What is sent: the panic value, its stack, and a few tags (HTTP method, the
// redacted path, the request id, the job type, the workspace id). What is not:
// the request itself. The http.Request is never handed to the SDK, so no headers,
// cookies, query strings, bodies or client IP addresses leave the process, and
// paths that carry a bearer token are redacted before they become a tag.
package crashreport

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/calnode/calnode/internal/buildinfo"
)

// flushTimeout bounds how long a shutdown or a crashing main waits for queued
// events. Two seconds is enough for a handful of events to one ingest host and
// short against the Deployment's termination grace period.
const flushTimeout = 2 * time.Second

var enabled atomic.Bool

// Init starts the SDK from the environment and reports whether it did:
//
//	SENTRY_DSN          the project DSN; empty or unset disables reporting
//	SENTRY_ENVIRONMENT  default "production"
//	SENTRY_RELEASE      default the build's commit (see Release)
//
// A malformed DSN is logged and leaves reporting off; it never stops the server.
func Init(logger *slog.Logger) bool {
	dsn := os.Getenv("SENTRY_DSN")
	if dsn == "" {
		return false
	}
	environment := os.Getenv("SENTRY_ENVIRONMENT")
	if environment == "" {
		environment = "production"
	}
	release := os.Getenv("SENTRY_RELEASE")
	if release == "" {
		release = Release(buildinfo.Get())
	}
	if err := start(sentry.ClientOptions{
		Dsn:         dsn,
		Environment: environment,
		Release:     release,
	}); err != nil {
		logger.Error("crash reporting disabled: sentry init failed", "error", err)
		return false
	}
	logger.Info("crash reporting enabled", "environment", environment, "release", release)
	return true
}

// start applies the options every caller gets, whatever the environment says.
// Split out so tests can install a mock transport.
func start(opts sentry.ClientOptions) error {
	// No request data, no user data, no IP: see the package comment. False is
	// already the SDK's default and is stated so a reader need not know that.
	// (The SDK now prefers DataCollection, which governs only its HTTP
	// integrations, and this package uses none.) Release is always set by the
	// caller, so the SDK never shells out to `git describe` looking for one.
	opts.SendDefaultPII = false
	// Panic values that are not errors (a string, a struct) become message
	// events, and those carry a stack only with this on.
	opts.AttachStacktrace = true
	if region := os.Getenv("DISTRONODE_REGION"); region != "" {
		opts.Tags = map[string]string{"region": region}
	}
	if err := sentry.Init(opts); err != nil {
		return err
	}
	enabled.Store(true)
	return nil
}

// Release is the release name events carry: the full commit SHA when the build
// knows it, which is what the Docker build stamps and what lets a release be
// matched to its commit, else "district-scheduler@<version>".
func Release(bi buildinfo.Info) string {
	if bi.Commit != "" && bi.Commit != "unknown" {
		return bi.Commit
	}
	return "district-scheduler@" + bi.Version
}

// Enabled reports whether Init started the SDK.
func Enabled() bool { return enabled.Load() }

// Recovered reports a panic value the caller has already recovered. tags are
// attached to this event only. A no-op when reporting is off, and for
// http.ErrAbortHandler, which net/http and httputil.ReverseProxy panic with on
// purpose to abort a response (a client that went away is not a crash).
func Recovered(v any, tags map[string]string) {
	if !enabled.Load() || v == nil {
		return
	}
	if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		return
	}
	hub := sentry.CurrentHub().Clone()
	hub.WithScope(func(scope *sentry.Scope) {
		scope.SetTags(tags)
		hub.Recover(v)
	})
}

// RecoverAndRepanic is for a defer at the top of a goroutine that should still
// crash on a panic: it reports, flushes so the event is not lost with the
// process, and panics again with the same value.
func RecoverAndRepanic() {
	if v := recover(); v != nil {
		Recovered(v, nil)
		Flush()
		panic(v)
	}
}

// Flush waits up to flushTimeout for queued events. Call it before exit.
func Flush() {
	if enabled.Load() {
		sentry.Flush(flushTimeout)
	}
}

// AsError turns a recovered panic value into an error, for a caller that keeps
// running and records the failure the way it records any other.
func AsError(v any) error {
	if err, ok := v.(error); ok {
		return fmt.Errorf("panic: %w", err)
	}
	return fmt.Errorf("panic: %v", v)
}
