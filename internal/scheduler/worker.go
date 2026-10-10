package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/media"
	"github.com/ayran/whatsapp-automation/internal/messaging"
	"github.com/ayran/whatsapp-automation/internal/templates"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
	"github.com/ayran/whatsapp-automation/pkg/backoff"
	"github.com/ayran/whatsapp-automation/pkg/phone"
	"github.com/ayran/whatsapp-automation/pkg/render"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

// Dispatcher delivers one message. In production it is the outbound gate,
// which bounds concurrency and paces sending; the interface keeps the
// scheduler independent of that policy.
type Dispatcher interface {
	Send(ctx context.Context, contact *domain.Contact, out messaging.Outbound) (*domain.Message, error)
}

// Worker drains the persistent job queue.
type Worker struct {
	cfg        config.Scheduler
	repo       *Repository
	templates  *templates.Repository
	contacts   *contacts.Repository
	sender     Dispatcher
	mediaStore *media.Store
	log        *slog.Logger

	backoff  backoff.Policy
	workerID string

	wg   sync.WaitGroup
	once sync.Once
}

func NewWorker(
	cfg config.Scheduler,
	repo *Repository,
	templateRepo *templates.Repository,
	contactRepo *contacts.Repository,
	sender Dispatcher,
	mediaStore *media.Store,
	log *slog.Logger,
) *Worker {
	host, _ := os.Hostname()
	if host == "" {
		host = "worker"
	}

	return &Worker{
		cfg:        cfg,
		repo:       repo,
		templates:  templateRepo,
		contacts:   contactRepo,
		sender:     sender,
		mediaStore: mediaStore,
		log:        log.With(slog.String("component", "scheduler")),
		backoff:    backoff.Default(cfg.RetryBaseDelay, cfg.RetryMaxDelay),
		workerID:   fmt.Sprintf("%s/%s", host, uuid.NewString()[:8]),
	}
}

// Start launches the polling loops and returns immediately.
func (w *Worker) Start(ctx context.Context) {
	w.once.Do(func() {
		// One recovery sweep before serving: any job left PROCESSING by a
		// previous process is orphaned and must go back in the queue.
		if released, err := w.repo.ReleaseStale(ctx, w.cfg.LockTimeout); err != nil {
			w.log.Error("startup recovery failed", slog.String("error", err.Error()))
		} else if released > 0 {
			w.log.Warn("JOB_RECOVERED",
				slog.Int64("count", released),
				slog.String("reason", "orphaned by a previous run"))
		}

		for i := 0; i < w.cfg.Workers; i++ {
			w.wg.Add(1)
			go w.pollLoop(ctx, i)
		}

		w.wg.Add(1)
		go w.maintenanceLoop(ctx)

		w.log.Info("scheduler started",
			slog.Int("workers", w.cfg.Workers),
			slog.Duration("poll_interval", w.cfg.PollInterval),
			slog.String("worker_id", w.workerID))
	})
}

// Wait blocks until every loop has exited.
func (w *Worker) Wait() { w.wg.Wait() }

func (w *Worker) pollLoop(ctx context.Context, index int) {
	defer w.wg.Done()

	// Stagger the loops so replicas do not all hit the database on the same
	// tick.
	jitter := time.Duration(index) * (w.cfg.PollInterval / time.Duration(max(w.cfg.Workers, 1)))
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.drain(ctx)
		}
	}
}

// drain processes up to one configured batch, renewing each active lease.
func (w *Worker) drain(ctx context.Context) {
	limit := w.cfg.BatchSize
	if limit <= 0 {
		limit = 25
	}
	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			return
		}

		now := time.Now().UTC()
		// A lease older than this belongs to a worker that is gone. Such a row
		// must not hold up the rest of its contact's queue while it waits for
		// the recovery sweep.
		// Claim only the job this worker can immediately execute. Reserving a
		// batch behind paced sends lets the tail expire before it even starts.
		jobs, err := w.repo.claim(ctx, w.workerID, 1, now, now.Add(-w.cfg.LockTimeout), i%2 == 0)
		if err != nil {
			w.log.Error("claiming jobs failed", slog.String("error", err.Error()))
			return
		}
		if len(jobs) == 0 {
			jobs, err = w.repo.Claim(ctx, w.workerID, 1, now, now.Add(-w.cfg.LockTimeout))
			if err != nil || len(jobs) == 0 {
				return
			}
		}

		for i := range jobs {
			if ctx.Err() != nil {
				// Shutting down: leave the remaining claims for the reaper to
				// release rather than sending on a cancelled context.
				return
			}
			w.process(ctx, jobs[i])
		}
	}
}

func (w *Worker) maintenanceLoop(ctx context.Context) {
	defer w.wg.Done()

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if released, err := w.repo.ReleaseStale(ctx, w.cfg.LockTimeout); err != nil {
				w.log.Error("releasing stale locks failed", slog.String("error", err.Error()))
			} else if released > 0 {
				w.log.Warn("JOB_RECOVERED",
					slog.Int64("count", released),
					slog.String("reason", "worker lease expired"))
			}

			if cancelled, err := w.repo.CancelStale(ctx, w.cfg.StaleJobTTL); err != nil {
				w.log.Error("cancelling expired jobs failed", slog.String("error", err.Error()))
			} else if cancelled > 0 {
				w.log.Warn("cancelled jobs past their send window", slog.Int64("count", cancelled))
			}
		}
	}
}

// process handles one claimed job end to end.
//
// Every transition a job makes is logged under a stable event name —
// JOB_CLAIMED, JOB_SENT, JOB_RETRY, JOB_FAILED, JOB_CANCELLED, JOB_DEFERRED —
// carrying the identifiers needed to follow one message from campaign step to
// delivery. A message that does not arrive should be explainable from the log
// alone, without reading the database.
func (w *Worker) process(ctx context.Context, job domain.ScheduledMessage) {
	ctx, release := w.keepLease(ctx, job.ID)
	defer release()
	log := w.log.With(
		slog.String("scheduled_message_id", job.ID.String()),
		slog.String("campaign_id", job.CampaignID.String()),
		slog.String("enrollment_id", job.EnrollmentID.String()),
		slog.String("campaign_step_id", job.StepID.String()),
		slog.String("contact_id", job.ContactID.String()),
		slog.Time("scheduled_at", job.ScheduledAt),
		slog.Int("attempt", job.AttemptCount+1))

	log.Info("JOB_CLAIMED", slog.String("worker_id", w.workerID))
	if reconciled, err := w.repo.ReconcileReceipt(ctx, job.ID); err != nil {
		log.Error("reading prior send receipt failed", slog.String("error", err.Error()))
		return
	} else if reconciled {
		log.Info("trigger_reply_already_sent")
		return
	}

	jobCtx, err := w.repo.LoadContext(ctx, job.ID)
	if err != nil {
		log.Error("loading job context failed", slog.String("error", err.Error()))
		w.retryOrFail(ctx, job, 5, fmt.Errorf("load context: %w", err), log)
		return
	}
	if jobCtx != nil {
		// From here on every line about this job says which number it belongs
		// to, so a message that did not arrive can be traced to an account
		// from the log alone.
		log = log.With(
			slog.Int64("whatsapp_instance_id", jobCtx.CampaignInstanceID),
			slog.String("instance", jobCtx.CampaignInstanceKey))
	}
	if jobCtx == nil {
		// The campaign, step or contact was deleted after the job was queued.
		if err := w.repo.Cancel(ctx, job.ID, "related records no longer exist"); err != nil {
			log.Error("cancelling orphaned job failed", slog.String("error", err.Error()))
		}
		return
	}

	if reason, skip := shouldSkip(jobCtx); skip {
		log.Info("JOB_CANCELLED", slog.String("reason", reason))
		if err := w.repo.Cancel(ctx, job.ID, reason); err != nil {
			log.Error("cancelling job failed", slog.String("error", err.Error()))
		}
		return
	}

	// A paused campaign holds its schedule rather than losing it. This is a
	// hold, not a failure, so it must not consume an attempt: a campaign paused
	// for an afternoon would otherwise come back with its whole queue marked
	// FAILED. What happens to jobs that expire during the pause is decided by
	// the campaign's resume policy when the operator resumes it.
	if jobCtx.CampaignStatus == domain.CampaignPaused {
		next := time.Now().UTC().Add(2 * time.Minute)
		if err := w.repo.Defer(ctx, job.ID, next, "кампания кідіртілген"); err != nil {
			log.Error("deferring paused job failed", slog.String("error", err.Error()))
		}
		log.Info("JOB_DEFERRED",
			slog.String("reason", "campaign paused"),
			slog.Time("next_attempt", next))
		return
	}

	// Sending caps hold the queue rather than dropping messages, so an
	// operator who set a conservative limit finds work waiting, not lost.
	if held, until, reason := w.overSendingLimit(ctx, jobCtx); held {
		if err := w.repo.Defer(ctx, job.ID, until, reason); err != nil {
			log.Error("deferring rate-limited job failed", slog.String("error", err.Error()))
		}
		log.Warn("JOB_DEFERRED",
			slog.String("campaign", jobCtx.CampaignName),
			slog.String("reason", reason),
			slog.Time("next_attempt", until))
		return
	}

	spec, err := w.resolveTemplate(ctx, jobCtx, job)
	if err != nil {
		// A deleted template is a configuration fault, not a transient one.
		if errors.Is(err, templates.ErrNotFound) {
			if cancelErr := w.repo.Cancel(ctx, job.ID, "message template no longer exists"); cancelErr != nil {
				log.Error("cancelling job failed", slog.String("error", cancelErr.Error()))
			}
			return
		}
		w.retryOrFail(ctx, job, jobCtx.CampaignMaxAttempts, err, log)
		return
	}

	out, err := w.buildOutbound(jobCtx, spec, job)
	if err != nil {
		if failErr := w.repo.Fail(ctx, job.ID, err.Error()); failErr != nil {
			log.Error("marking job failed", slog.String("error", failErr.Error()))
		}
		log.Error("job cannot be built", slog.String("error", err.Error()))
		return
	}

	contact := contactFromJob(jobCtx)
	message, sendErr := w.sender.Send(ctx, contact, out)
	if sendErr != nil {
		// A consent failure can never succeed on retry.
		if errors.Is(sendErr, messaging.ErrNoConsent) || errors.Is(sendErr, messaging.ErrOptedOut) {
			if cancelErr := w.repo.Cancel(ctx, job.ID, sendErr.Error()); cancelErr != nil {
				log.Error("cancelling job failed", slog.String("error", cancelErr.Error()))
			}
			return
		}
		w.retryOrFail(ctx, job, jobCtx.CampaignMaxAttempts, sendErr, log)
		return
	}

	sentAt := time.Now().UTC()
	if message.SentAt != nil {
		sentAt = *message.SentAt
	}
	if err := w.repo.MarkSent(ctx, nil, job.ID, message.ID, sentAt); err != nil {
		// messages already holds the provider receipt. A recovered worker
		// reconciles that receipt before sending, so this gap cannot resend.
		log.Error("JOB_SENT_UNRECORDED",
			slog.String("message_id", message.ID.String()),
			slog.String("error", err.Error()))
		return
	}

	log.Info("JOB_SENT",
		slog.String("campaign", jobCtx.CampaignName),
		slog.String("step", jobCtx.StepName),
		slog.String("message_id", message.ID.String()),
		slog.Time("sent_at", sentAt))
	if jobCtx.InitialReply {
		log.Info("trigger_reply_sent", slog.String("message_id", message.ID.String()))
	}
}

// resolveTemplate loads the content this job should render.
//
// A campaign that pins template versions renders the revision recorded when
// the job was queued, so an edit never rewrites what a contact already in the
// funnel is about to receive. Left off — the default, and the behaviour the
// platform has always had — the live template is read at send time, so an edit
// reaches every message not yet sent.
func (w *Worker) resolveTemplate(ctx context.Context, jc *JobContext, job domain.ScheduledMessage) (*templates.SendSpec, error) {
	if jc.CampaignPinVersion && job.TemplateVersion != nil {
		spec, err := w.templates.ResolveVersionForSend(ctx, nil, jc.TemplateID, *job.TemplateVersion)
		if err == nil {
			return spec, nil
		}
		// A pinned revision that is missing is not worth failing a send over;
		// the live template is the honest fallback.
		if !errors.Is(err, templates.ErrNotFound) {
			return nil, err
		}
		w.log.Warn("pinned template version is missing; sending the current one",
			slog.String("template_id", jc.TemplateID.String()),
			slog.Int("version", *job.TemplateVersion))
	}
	return w.templates.ResolveForSend(ctx, nil, jc.TemplateID)
}

// overSendingLimit checks the campaign's optional hourly and daily caps.
//
// A cap that is reached holds the queue: the job is deferred to the point the
// window rolls over, which is the earliest moment it could legitimately go
// out. Nothing is discarded and nothing is sent silently past the limit.
func (w *Worker) overSendingLimit(ctx context.Context, jc *JobContext) (bool, time.Time, string) {
	if jc.CampaignMaxPerHour == nil && jc.CampaignMaxPerDay == nil {
		return false, time.Time{}, ""
	}

	now := time.Now().UTC()
	lastHour, lastDay, err := w.repo.CampaignSendCounts(ctx, jc.Job.CampaignID, now)
	if err != nil {
		// Counting failed: let the send proceed rather than stalling a campaign
		// on a transient read error.
		w.log.Error("reading campaign send counters failed", slog.String("error", err.Error()))
		return false, time.Time{}, ""
	}

	if jc.CampaignMaxPerHour != nil && lastHour >= *jc.CampaignMaxPerHour {
		return true, now.Add(5 * time.Minute),
			fmt.Sprintf("сағаттық шек толды (%d/%d)", lastHour, *jc.CampaignMaxPerHour)
	}
	if jc.CampaignMaxPerDay != nil && lastDay >= *jc.CampaignMaxPerDay {
		return true, now.Add(15 * time.Minute),
			fmt.Sprintf("тәуліктік шек толды (%d/%d)", lastDay, *jc.CampaignMaxPerDay)
	}
	return false, time.Time{}, ""
}

// shouldSkip reports configuration or consent reasons to drop a job outright.
//
// The audience check here is deliberately redundant. Queue creation already
// refuses to make a live job for a contact outside the step's audience, so in
// normal operation this never fires. It exists because the row can outlive the
// decision that created it: a step can gain a cutoff after its jobs were
// queued, a deploy can land mid-schedule, a row can be edited by hand. The
// consequence of getting it wrong is a message reaching someone it was
// explicitly meant to spare, which is not recoverable once WhatsApp has
// delivered it — so it is re-derived from the enrolment one last time, against
// the same rule the planner used.
func shouldSkip(jc *JobContext) (string, bool) {
	switch {
	case !jc.StepEnabled:
		return "step is disabled", true

	case !audienceEligible(jc):
		return domain.SkipNotEligible, true
	case jc.EnrollmentStatus != domain.EnrollmentActive && !(jc.InitialReply && jc.EnrollmentStatus == domain.EnrollmentCompleted):
		return "enrollment is no longer active", true
	// The unsubscribe that counts is the one given to *this job's* number.
	// Reading the whole-person flag here would let a customer who left the
	// other number's funnel silently cancel the messages they are still
	// waiting for from this one.
	case jc.ContactOptedOutHere:
		return "contact unsubscribed", true
	case jc.ContactBlocked:
		return "contact is blocked", true
	case jc.ContactConsentAt == nil:
		return "contact has no inbound consent", true
	case jc.CampaignStatus == domain.CampaignArchived || jc.CampaignStatus == domain.CampaignCompleted:
		return "campaign is closed", true
	}
	return "", false
}

// audienceEligible re-applies the step's audience cutoff to this job's
// enrolment, using the same inclusive rule the planner uses so the two can
// never disagree about who a message is for.
func audienceEligible(jc *JobContext) bool {
	step := domain.CampaignStep{
		AudienceFilterEnabled: jc.StepAudienceFilter,
		AudienceMinJoinedAt:   jc.StepAudienceMinJoinedAt,
	}
	return step.EligibleFor(jc.EnrollmentJoinedAt)
}

func (w *Worker) buildOutbound(jc *JobContext, spec *templates.SendSpec, job domain.ScheduledMessage) (messaging.Outbound, error) {
	ctxValues := w.renderValues(jc)
	body := render.Render(spec.Body, render.NewContext(ctxValues))

	out := messaging.Outbound{
		// Resolved from the campaign this job belongs to, on every attempt.
		// This is what makes a retry -- including one picked up by a different
		// process after a restart -- go out through the same number the first
		// attempt would have used. There is no default sender to fall back to.
		WhatsAppInstanceID: jc.CampaignInstanceID,
		Type:               spec.Type,
		Text:               body,
		LinkPreview:        spec.LinkPreview,
		MediaFileID:        spec.MediaID,
		MediaMIME:          spec.MediaMIME,
		FileName:           firstNonEmpty(spec.FileName, spec.MediaName),
		CampaignID:         &jc.Job.CampaignID,
		EnrollmentID:       &jc.Job.EnrollmentID,
		CampaignStepID:     &jc.Job.StepID,
		ScheduledMessageID: &job.ID,
		TemplateID:         &spec.TemplateID,
		TemplateVersion:    &spec.Version,
	}

	if spec.Type.RequiresMedia() {
		if spec.MediaPath == "" {
			return out, fmt.Errorf("template %q has no media file attached", spec.Name)
		}
		abs, err := w.mediaStore.AbsPath(spec.MediaPath)
		if err != nil {
			return out, fmt.Errorf("resolve media path: %w", err)
		}
		out.MediaPath = abs
	}

	return out, nil
}

// renderValues assembles the template variables for one recipient.
func (w *Worker) renderValues(jc *JobContext) map[string]string {
	name := strings.TrimSpace(jc.ContactName)
	if name == "" {
		name = strings.TrimSpace(jc.ContactPushName)
	}

	values := map[string]string{
		"contact_name":  name,
		"first_name":    firstWord(name),
		"phone":         phone.Display(jc.ContactPhone),
		"campaign_name": jc.CampaignName,
		"webinar_link":  jc.CampaignLink,
		"timezone":      jc.CampaignTimezone,
	}

	// The webinar this recipient is actually waiting for. A daily recurring
	// campaign pins one occurrence per enrolment, so the same template sent on
	// the 18th and the 19th resolves to the 18th and the 19th — the date is
	// never baked into the copy. A one-time campaign has no occurrence and
	// falls back to the campaign's event start, exactly as before.
	webinar := jc.CampaignEventStart
	if jc.EnrollmentOccurrenceAt != nil {
		webinar = jc.EnrollmentOccurrenceAt
	}

	if webinar != nil {
		values["webinar_date"] = timex.FormatIn(*webinar, jc.CampaignTimezone, "02.01.2006")
		values["webinar_time"] = timex.FormatIn(*webinar, jc.CampaignTimezone, "15:04")
		values["webinar_datetime"] = timex.FormatIn(*webinar, jc.CampaignTimezone, "02.01.2006 15:04")
		values["remaining_time"] = timex.RemainingLabel(time.Now().UTC(), *webinar)
	}

	return values
}

// retryOrFail applies the backoff policy, or gives up once the campaign's
// attempt budget is exhausted.
func (w *Worker) retryOrFail(ctx context.Context, job domain.ScheduledMessage, maxAttempts int, cause error, log *slog.Logger) {
	if maxAttempts <= 0 {
		maxAttempts = w.cfg.MaxAttempts
	}

	attempts := job.AttemptCount + 1
	retryable := whatsapp.IsRetryable(cause)
	jc, contextErr := w.repo.LoadContext(ctx, job.ID)
	if contextErr != nil {
		log.Error("reading trigger reply retry policy failed", slog.String("error", contextErr.Error()))
		return
	}
	owedReply := jc != nil && jc.InitialReply
	if owedReply && whatsapp.IsAvailabilityFailure(cause) {
		retryable = true
	}

	if !retryable || (attempts >= maxAttempts && !owedReply) {
		reason := cause.Error()
		if !retryable {
			reason = "permanent failure: " + reason
		}
		if err := w.repo.Fail(ctx, job.ID, reason); err != nil {
			log.Error("marking job failed", slog.String("error", err.Error()))
		}
		// FAILED is a resting place, not a deletion: the row keeps its error,
		// its attempt count and its schedule, stays visible in the panel and
		// can be requeued by hand.
		attrs := []any{
			slog.Int("attempts", attempts),
			slog.Int("max_attempts", maxAttempts),
			slog.Bool("retryable", retryable),
			slog.String("error", cause.Error()),
		}
		log.Error("JOB_FAILED", append(attrs, scheduledProviderErrorAttrs(cause)...)...)
		return
	}

	next := w.backoff.NextAttemptAt(time.Now().UTC(), attempts)
	if err := w.repo.Retry(ctx, job.ID, next, cause.Error()); err != nil {
		log.Error("scheduling retry failed", slog.String("error", err.Error()))
		return
	}

	attrs := []any{
		slog.Int("attempts", attempts),
		slog.Int("max_attempts", maxAttempts),
		slog.Time("next_attempt", next),
		slog.String("error", cause.Error()),
	}
	log.Warn("JOB_RETRY", append(attrs, scheduledProviderErrorAttrs(cause)...)...)
}

func scheduledProviderErrorAttrs(err error) []any {
	var providerErr *whatsapp.Error
	if !errors.As(err, &providerErr) {
		return nil
	}
	return []any{
		slog.String("provider_op", providerErr.Op),
		slog.Int("http_status", providerErr.StatusCode),
	}
}

func contactFromJob(jc *JobContext) *domain.Contact {
	var blockedAt *time.Time
	if jc.ContactBlocked {
		now := time.Now().UTC()
		blockedAt = &now
	}

	return &domain.Contact{
		ID:           jc.Job.ContactID,
		Phone:        jc.ContactPhone,
		PhoneDisplay: phone.Display(jc.ContactPhone),
		ChatID:       jc.ContactChatID,
		Name:         jc.ContactName,
		PushName:     jc.ContactPushName,
		Status:       jc.ContactStatus,
		// The account-scoped answer, not the whole-person flag. This object
		// exists to send one message through one number, so the unsubscribe it
		// should carry is the one given to that number. Handing over the
		// whole-person flag would make the object read as "opted out" for
		// somebody who only ever left the other number's funnel.
		OptedOut:       jc.ContactOptedOutHere,
		BlockedAt:      blockedAt,
		FirstContactAt: jc.ContactConsentAt,
	}
}

func firstWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
