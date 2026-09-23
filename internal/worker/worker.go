package worker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/i18n"
	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/netutil"
	"github.com/calnode/calnode/internal/webhook"
)

// webhookDeliveryRetention is how long a finished webhook delivery is kept before the
// purge sweeps it. Long enough to still be useful when someone investigates a failure
// days later, short enough that the table cannot grow without bound. The deliveries UI
// only ever shows the 50 most recent, so this is not what limits what anyone can see.
const webhookDeliveryRetention = 30 * 24 * time.Hour

// TenantDeps is everything processing a job needs that differs per workspace: the
// bound database handle, and the two services built from that workspace's own
// server_settings row.
//
// ⛔ It exists because the claim loop and the work are on opposite sides of the
// tenancy boundary. Claiming has to see every workspace's jobs — the queue is one
// queue, ordered by run_at globally — so it runs on the PLATFORM handle. Doing the
// work has to see exactly one workspace, so it runs on ForWorkspace(job's id) with
// that workspace's mailer and webhook secret. Running the work on the platform
// handle would read across tenants; running the claim on a bound handle would claim
// only one tenant's jobs and starve the rest.
type TenantDeps struct {
	DB      *db.DB
	Mailer  mailer.Mailer
	Webhook *webhook.Service
}

// Worker polls the jobs table and processes pending jobs (webhooks, reminders).
type Worker struct {
	// db is the PLATFORM handle: the claim loop, the reaper and the housekeeping
	// sweeps all run across every workspace through it.
	db         *db.DB
	svc        *webhook.Service
	mailer     mailer.Mailer
	logger     *slog.Logger
	httpClient *http.Client
	handlers   map[string]func(context.Context, string, string) error // custom job types; (workspaceID, payload)
	tenant     func(workspaceID string) TenantDeps
	done       chan struct{}
}

// RegisterHandler registers a processor for a custom job type whose logic lives outside this
// package (e.g. the notetaker jobs in the handler package, which need LLM/S3/encKey). Call before
// Run; processJob falls back to these for any type it doesn't handle natively.
//
// The handler receives the claimed job's workspace id as well as its payload, so
// it can bind its own database handle and per-tenant clients. In single-tenant mode
// the id is "default".
func (w *Worker) RegisterHandler(typ string, fn func(ctx context.Context, workspaceID, payload string) error) {
	w.handlers[typ] = fn
}

// WithHTTPClient overrides the default SSRF-safe HTTP client. Intended for testing only.
func WithHTTPClient(c *http.Client) func(*Worker) {
	return func(w *Worker) { w.httpClient = c }
}

// WithMailer configures the mailer used to send reminder emails. In multi-tenant
// mode it is only the fallback: WithTenantResolver's mailer wins.
func WithMailer(m mailer.Mailer) func(*Worker) {
	return func(w *Worker) { w.mailer = m }
}

// WithTenantResolver supplies the per-workspace dependencies a claimed job is
// processed with. Unset — single-tenant mode — every job is processed with the one
// handle and the one mailer the Worker was built with, exactly as before.
func WithTenantResolver(fn func(workspaceID string) TenantDeps) func(*Worker) {
	return func(w *Worker) { w.tenant = fn }
}

func New(db *db.DB, svc *webhook.Service, logger *slog.Logger, opts ...func(*Worker)) *Worker {
	w := &Worker{
		db:       db,
		svc:      svc,
		mailer:   &mailer.Noop{},
		logger:   logger,
		handlers: map[string]func(context.Context, string, string) error{},
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: netutil.SafeTransport(logger, "worker: webhook SSRF block"),
		},
		done: make(chan struct{}),
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Run polls for pending jobs every 5 seconds until ctx is cancelled.
// When ctx is cancelled the current Poll cycle (if any) runs to completion
// before Run returns, so in-progress jobs are not abandoned mid-delivery.
// Call Wait to block until Run has exited.
func (w *Worker) Run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Poll uses a background context so that a shutdown signal does not
			// cancel an in-progress webhook delivery or reminder email mid-flight.
			w.Poll(context.Background())
		}
	}
}

// Wait blocks until Run has returned. It returns immediately if Run was never
// started or has already exited.
func (w *Worker) Wait() {
	<-w.done
}

// Poll processes one batch of pending jobs. Exported for testing.
func (w *Worker) Poll(ctx context.Context) {
	now := time.Now().UTC().Format(time.RFC3339)

	// Purge expired manage tokens and sessions to keep tables small.
	if _, err := w.db.ExecContext(ctx,
		`DELETE FROM booking_manage_tokens WHERE expires_at < ?`, now); err != nil {
		w.logger.Error("worker: purge expired tokens", "error", err)
	}
	if _, err := w.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at < ?`, now); err != nil {
		w.logger.Error("worker: purge expired sessions", "error", err)
	}
	// Magic-link tokens are single-use + short-lived; sweep expired/consumed ones.
	if _, err := w.db.ExecContext(ctx,
		`DELETE FROM magic_link_tokens WHERE expires_at < ? OR used_at IS NOT NULL`, now); err != nil {
		w.logger.Error("worker: purge magic link tokens", "error", err)
	}
	// SSO hand-off nonces exist only to refuse a replay inside the token's own validity
	// window (at most ~60s), so a row past its expires_at can never reject anything and
	// is pure growth. Nothing reads them back except the INSERT that claims one.
	if _, err := w.db.ExecContext(ctx,
		`DELETE FROM sso_nonces WHERE expires_at < ?`, now); err != nil {
		w.logger.Error("worker: purge sso nonces", "error", err)
	}
	// Idempotency keys are only useful for the retry window of the original
	// request; purge them 24h after creation so the table stays small.
	idemCutoff := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	if _, err := w.db.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE created_at < ?`, idemCutoff); err != nil {
		w.logger.Error("worker: purge idempotency keys", "error", err)
	}
	// Expired MCP OAuth authorization codes are single-use and short-lived; sweep the
	// abandoned ones so the table doesn't accumulate dead rows.
	if _, err := w.db.ExecContext(ctx,
		`DELETE FROM oauth_auth_codes WHERE expires_at < ?`, now); err != nil {
		w.logger.Error("worker: purge oauth auth codes", "error", err)
	}
	// Webhook deliveries are a log: the UI shows the 50 most recent and nothing reads
	// them back for state. Nothing purged them, so on a busy instance they accumulated
	// for its entire life, inside the SQLite file Litestream replicates offsite.
	//
	// Only terminal rows are swept, and only ones with a recorded attempt time. A
	// pending delivery still has a jobs row pointing at it by id, and deleting one out
	// from under its job would turn a deliverable webhook into a permanent failure.
	// Reaching 'success' or 'failed' means the job already ran to completion.
	deliveryCutoff := time.Now().UTC().Add(-webhookDeliveryRetention).Format(time.RFC3339)
	if _, err := w.db.ExecContext(ctx,
		`DELETE FROM webhook_deliveries
		 WHERE status IN ('success', 'failed')
		   AND last_attempted_at IS NOT NULL
		   AND last_attempted_at < ?`, deliveryCutoff); err != nil {
		w.logger.Error("worker: purge webhook deliveries", "error", err)
	}
	// Backstop for the Stripe checkout.session.expired webhook: release any payment hold
	// still pending well past the 31-min checkout window, freeing the slot. The webhook
	// normally does this promptly; this catches missed/late deliveries.
	holdCutoff := time.Now().UTC().Add(-45 * time.Minute).Format(time.RFC3339)
	if _, err := w.db.ExecContext(ctx,
		`UPDATE bookings SET status = 'cancelled', cancellation_reason = 'payment not completed'
		 WHERE status = 'confirmed' AND payment_status = 'pending' AND created_at < ?`, holdCutoff); err != nil {
		w.logger.Error("worker: release expired payment holds", "error", err)
	}

	// Reaper: handle running jobs whose lock has expired (process crashed mid-job).
	// Jobs with retries remaining are reset to pending with a 1-minute delay so
	// they do not immediately re-enter this Poll cycle. Jobs that have already
	// exhausted max_attempts are marked failed directly.
	reaperRunAt := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	if _, err := w.db.ExecContext(ctx, `
		UPDATE jobs SET status = 'pending', run_at = ?, last_error = 'recovered after crash'
		WHERE status = 'running' AND locked_until < ? AND attempts < max_attempts`,
		reaperRunAt, now); err != nil {
		w.logger.Error("worker: reaper: reset", "error", err)
	}
	if _, err := w.db.ExecContext(ctx, `
		UPDATE jobs SET status = 'failed', last_error = 'max attempts exceeded after crash'
		WHERE status = 'running' AND locked_until < ? AND attempts >= max_attempts`, now); err != nil {
		w.logger.Error("worker: reaper: fail exhausted", "error", err)
	}

	// ⛔ No workspace predicate, deliberately. This is ONE queue ordered by run_at
	// across every tenant, which is why it runs on the platform handle and why
	// migration 00060 keeps a workspace-free idx_jobs_pending_global alongside the
	// workspace-leading one. A bound handle here would serve one tenant and starve
	// the rest.
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, workspace_id, type, payload, attempts, max_attempts
		FROM jobs
		WHERE status = 'pending' AND run_at <= ?
		LIMIT 10`, now)
	if err != nil {
		w.logger.Error("worker: poll", "error", err)
		return
	}

	type job struct {
		id, workspaceID, typ, payload string
		attempts, maxAttempts         int
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.workspaceID, &j.typ, &j.payload, &j.attempts, &j.maxAttempts); err != nil {
			w.logger.Error("worker: scan job", "error", err)
			continue
		}
		jobs = append(jobs, j)
	}
	rows.Close() // #nosec G104 -- rows already fully consumed above; nothing actionable on close error

	for _, j := range jobs {
		lockedUntil := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339)
		res, err := w.db.ExecContext(ctx,
			`UPDATE jobs SET status = 'running', attempts = attempts + 1, locked_until = ?
			 WHERE id = ? AND status = 'pending'`,
			lockedUntil, j.id)
		if err != nil {
			w.logger.Error("worker: claim job", "error", err, "job_id", j.id)
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // claimed by another worker
		}
		j.attempts++

		if err := w.processJob(ctx, j.workspaceID, j.typ, j.payload); err != nil {
			w.logger.Error("worker: process job", "error", err, "job_id", j.id, "type", j.typ, "workspace", j.workspaceID)
			if j.attempts >= j.maxAttempts {
				if _, uerr := w.db.ExecContext(ctx,
					`UPDATE jobs SET status = 'failed', last_error = ? WHERE id = ?`,
					err.Error(), j.id); uerr != nil {
					w.logger.Error("worker: mark job failed", "error", uerr, "job_id", j.id)
				}
			} else {
				runAt := time.Now().UTC().Add(backoff(j.attempts)).Format(time.RFC3339)
				if _, uerr := w.db.ExecContext(ctx,
					`UPDATE jobs SET status = 'pending', last_error = ?, run_at = ? WHERE id = ?`,
					err.Error(), runAt, j.id); uerr != nil {
					w.logger.Error("worker: requeue job", "error", uerr, "job_id", j.id)
				}
			}
		} else {
			if _, uerr := w.db.ExecContext(ctx, `UPDATE jobs SET status = 'done' WHERE id = ?`, j.id); uerr != nil {
				w.logger.Error("worker: mark job done", "error", uerr, "job_id", j.id)
			}
		}
	}
}

func backoff(attempt int) time.Duration {
	if attempt == 1 {
		return 60 * time.Second
	}
	return 5 * time.Minute
}

// depsFor returns the dependencies a job of workspaceID is processed with. Without
// a resolver — single-tenant mode — it is the Worker's own handle and mailer, so
// nothing changes.
func (w *Worker) depsFor(workspaceID string) TenantDeps {
	if w.tenant == nil {
		return TenantDeps{DB: w.db, Mailer: w.mailer, Webhook: w.svc}
	}
	d := w.tenant(workspaceID)
	if d.DB == nil {
		d.DB = w.db
	}
	if d.Mailer == nil {
		d.Mailer = w.mailer
	}
	if d.Webhook == nil {
		d.Webhook = w.svc
	}
	return d
}

func (w *Worker) processJob(ctx context.Context, workspaceID, typ, payload string) error {
	if fn, ok := w.handlers[typ]; ok {
		return fn(ctx, workspaceID, payload)
	}
	deps := w.depsFor(workspaceID)
	switch typ {
	case "webhook.deliver":
		return w.deliverWebhook(ctx, deps, payload)
	case "reminder.send":
		return w.sendReminder(ctx, deps, payload)
	default:
		return fmt.Errorf("worker: unknown job type %q", typ)
	}
}

func (w *Worker) sendReminder(ctx context.Context, deps TenantDeps, payload string) error {
	var p struct {
		BookingID   string `json:"booking_id"`
		HoursBefore int    `json:"hours_before"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return fmt.Errorf("worker: reminder: parse payload: %w", err)
	}

	// One query: join bookings → event_types, and users on the booking's own host.
	// The host is bookings.host_id (the primary host), not event_types.user_id: on a
	// round-robin or multi-host event type the owner may not be attending at all.
	// Also load that host's notify_reminder pref and the msg_reminder custom note.
	// Skip if booking is deleted or no longer confirmed.
	var d mailer.BookingData
	d.BookingID = p.BookingID
	var startAt, endAt, status string
	var locVal, msgReminder, subjReminder sql.NullString
	var notifyReminder int
	// hostID and createdAt are for the booking.reminder webhook below, not the email:
	// Enqueue selects a subscriber's webhooks by the BOOKING's host, which is not
	// necessarily the event type's owner (a rotation or a reassignment moves it).
	var hostID, createdAt string
	// deps.DB, not w.db: the reminder job runs bound to its own workspace (B5), and
	// this read plus the booking.reminder Enqueue below must both stay inside it.
	err := deps.DB.QueryRowContext(ctx, `
		SELECT b.status, b.start_at, b.end_at, b.location_value, b.host_id, b.created_at,
		       et.name, et.slug, et.msg_reminder, et.subj_reminder,
		       u.name, u.email, COALESCE(u.notify_reminder, 1)
		FROM bookings b
		JOIN event_types et ON et.id = b.event_type_id
		JOIN users u ON u.id = b.host_id
		WHERE b.id = ?`, p.BookingID).
		Scan(&status, &startAt, &endAt, &locVal, &hostID, &createdAt,
			&d.EventTypeName, &d.EventTypeSlug, &msgReminder, &subjReminder,
			&d.HostName, &d.HostEmail, &notifyReminder)
	if err == sql.ErrNoRows {
		return nil // booking deleted; skip silently
	}
	if err != nil {
		return fmt.Errorf("worker: reminder: load booking: %w", err)
	}
	if status != "confirmed" {
		return nil // cancelled or otherwise; skip silently
	}
	if notifyReminder == 0 {
		return nil // host has disabled reminder emails
	}

	var parseErr error
	if d.StartAt, parseErr = time.Parse(time.RFC3339, startAt); parseErr != nil {
		return fmt.Errorf("worker: reminder: parse start_at %q: %w", startAt, parseErr)
	}
	if d.EndAt, parseErr = time.Parse(time.RFC3339, endAt); parseErr != nil {
		return fmt.Errorf("worker: reminder: parse end_at %q: %w", endAt, parseErr)
	}
	if locVal.Valid {
		d.LocationValue = locVal.String
	}
	if msgReminder.Valid {
		d.CustomNote = msgReminder.String
	}
	if subjReminder.Valid {
		d.SubjectOverride = subjReminder.String
	}

	// Load organizer attendee.
	var locale string
	orgErr := deps.DB.QueryRowContext(ctx, `
		SELECT name, email, iana_timezone, locale
		FROM booking_attendees WHERE booking_id = ? AND is_organizer = 1`, p.BookingID).
		Scan(&d.OrganizerName, &d.OrganizerEmail, &d.OrganizerTimezone, &locale)
	if orgErr == sql.ErrNoRows {
		return nil // no organizer attendee (data integrity gap); skip silently
	}
	if orgErr != nil {
		return fmt.Errorf("worker: reminder: load organizer: %w", orgErr)
	}
	d.Locale = i18n.Get(locale)

	// Brand the reminder email with the instance wordmark/logo. db.LogoURLSQL reads a logo
	// URL with no stored image behind it as unset, as the handler's loadBranding does.
	_ = deps.DB.QueryRowContext(ctx, `
		SELECT COALESCE(business_name,''), `+db.LogoURLSQL+`
		FROM server_settings WHERE id = 1`).Scan(&d.BrandName, &d.LogoURL)

	if err := mailer.SendReminder(ctx, deps.Mailer, d); err != nil {
		return fmt.Errorf("worker: reminder: send: %w", err)
	}

	// booking.reminder fires AFTER the email and only on success, because the event means
	// "this booking's attendee has been reminded". Emitting it beside a failed send would
	// tell a subscriber something untrue, and the job retries, which would then deliver it
	// twice for one reminder.
	//
	// Conversely, an enqueue failure here must NOT fail the job: the email has already
	// gone, and a retry would send a second one. Logged and dropped, the same trade every
	// other webhook enqueue in the tree makes after a committed side effect.
	//
	// The early returns above are all "no reminder happened" — booking deleted, no longer
	// confirmed, host has reminder emails off — so none of them fires this either.
	// deps.Webhook, not w.svc: the subscriber lookup and the delivery row it writes
	// are this workspace's, and the secret it will be signed with is this
	// workspace's too (B5, deliverable 2).
	if deps.Webhook != nil {
		if err := deps.Webhook.Enqueue(ctx, "booking.reminder", webhook.BookingPayload{
			ID:            p.BookingID,
			EventTypeSlug: d.EventTypeSlug,
			HostID:        hostID,
			StartAt:       d.StartAt.UTC().Format(time.RFC3339),
			EndAt:         d.EndAt.UTC().Format(time.RFC3339),
			Status:        status,
			LocationValue: d.LocationValue,
			CreatedAt:     createdAt,
			HoursBefore:   p.HoursBefore,
		}); err != nil {
			w.logger.Error("worker: reminder: enqueue booking.reminder webhook",
				"error", err, "booking_id", p.BookingID)
		}
	}
	return nil
}

func (w *Worker) deliverWebhook(ctx context.Context, deps TenantDeps, jobPayload string) error {
	var p struct {
		WebhookDeliveryID string `json:"webhook_delivery_id"`
	}
	if err := json.Unmarshal([]byte(jobPayload), &p); err != nil {
		return fmt.Errorf("worker: parse job payload: %w", err)
	}

	var (
		deliveryPayload string
		event           string
		webhookURL      string
		secretEnc       string
	)
	err := deps.DB.QueryRowContext(ctx, `
		SELECT d.payload, d.event, wh.url, wh.secret_enc
		FROM webhook_deliveries d
		JOIN webhooks wh ON wh.id = d.webhook_id
		WHERE d.id = ?`, p.WebhookDeliveryID).
		Scan(&deliveryPayload, &event, &webhookURL, &secretEnc)
	if err == sql.ErrNoRows {
		return nil // delivery or webhook deleted; skip silently
	}
	if err != nil {
		return fmt.Errorf("worker: fetch delivery: %w", err)
	}

	secret, err := deps.Webhook.DecryptSecret(secretEnc)
	if err != nil {
		return fmt.Errorf("worker: decrypt secret: %w", err)
	}

	payloadBytes := []byte(deliveryPayload)
	sig := webhook.Sign(secret, payloadBytes)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL,
		bytes.NewReader(payloadBytes))
	if err != nil {
		return fmt.Errorf("worker: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Calnode-Signature", sig)
	req.Header.Set("X-Calnode-Event", event)
	req.Header.Set("X-Calnode-Delivery", p.WebhookDeliveryID)

	resp, err := w.httpClient.Do(req)
	now := time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		if _, uerr := deps.DB.ExecContext(ctx, `
			UPDATE webhook_deliveries
			SET status = 'failed', attempt_count = attempt_count + 1, last_attempted_at = ?
			WHERE id = ?`, now, p.WebhookDeliveryID); uerr != nil {
			w.logger.Error("worker: mark webhook delivery failed", "error", uerr, "delivery_id", p.WebhookDeliveryID)
		}
		return fmt.Errorf("worker: http post: %w", err)
	}
	defer func() {
		// Draining/closing a response body we're done with — no action possible on either error.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) // #nosec G104
		resp.Body.Close()                                            // #nosec G104
	}()

	status := "success"
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status = "failed"
	}
	if _, uerr := deps.DB.ExecContext(ctx, `
		UPDATE webhook_deliveries
		SET status = ?, response_status = ?, attempt_count = attempt_count + 1, last_attempted_at = ?
		WHERE id = ?`, status, resp.StatusCode, now, p.WebhookDeliveryID); uerr != nil {
		w.logger.Error("worker: record webhook delivery result", "error", uerr, "delivery_id", p.WebhookDeliveryID)
	}

	if status == "failed" {
		return fmt.Errorf("worker: endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}
