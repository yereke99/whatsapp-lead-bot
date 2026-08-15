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

// Worker drains the persistent job queue.
type Worker struct {
	cfg        config.Scheduler
	repo       *Repository
	templates  *templates.Repository
	contacts   *contacts.Repository
	sender     *messaging.Sender
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
	sender *messaging.Sender,
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
			w.log.Warn("requeued jobs orphaned by a previous run", slog.Int64("count", released))
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

// drain claims and processes batches until nothing is due.
func (w *Worker) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		jobs, err := w.repo.Claim(ctx, w.workerID, w.cfg.BatchSize, time.Now().UTC())
		if err != nil {
			w.log.Error("claiming jobs failed", slog.String("error", err.Error()))
			return
		}
		if len(jobs) == 0 {
			return
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
				w.log.Warn("released stale job locks", slog.Int64("count", released))
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
func (w *Worker) process(ctx context.Context, job domain.ScheduledMessage) {
	log := w.log.With(
		slog.String("job_id", job.ID.String()),
		slog.Int("attempt", job.AttemptCount+1))

	jobCtx, err := w.repo.LoadContext(ctx, job.ID)
	if err != nil {
		log.Error("loading job context failed", slog.String("error", err.Error()))
		w.retryOrFail(ctx, job, 5, fmt.Errorf("load context: %w", err), log)
		return
	}
	if jobCtx == nil {
		// The campaign, step or contact was deleted after the job was queued.
		if err := w.repo.Cancel(ctx, job.ID, "related records no longer exist"); err != nil {
			log.Error("cancelling orphaned job failed", slog.String("error", err.Error()))
		}
		return
	}

	if reason, skip := shouldSkip(jobCtx); skip {
		log.Info("job cancelled before sending", slog.String("reason", reason))
		if err := w.repo.Cancel(ctx, job.ID, reason); err != nil {
			log.Error("cancelling job failed", slog.String("error", err.Error()))
		}
		return
	}

	// A paused campaign holds its schedule rather than losing it: the job goes
	// back to PENDING and is retried once the operator resumes.
	if jobCtx.CampaignStatus == domain.CampaignPaused {
		next := time.Now().UTC().Add(2 * time.Minute)
		if err := w.repo.Retry(ctx, job.ID, next, "campaign is paused"); err != nil {
			log.Error("deferring paused job failed", slog.String("error", err.Error()))
		}
		return
	}

	spec, err := w.templates.ResolveForSend(ctx, nil, jobCtx.TemplateID)
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
		log.Error("marking job sent failed", slog.String("error", err.Error()))
	}

	log.Info("scheduled message delivered",
		slog.String("campaign", jobCtx.CampaignName),
		slog.String("step", jobCtx.StepName),
		slog.String("contact_id", jobCtx.Job.ContactID.String()))

}

// shouldSkip reports configuration or consent reasons to drop a job outright.
func shouldSkip(jc *JobContext) (string, bool) {
	switch {
	case !jc.StepEnabled:
		return "step is disabled", true
	case jc.EnrollmentStatus != domain.EnrollmentActive:
		return "enrollment is no longer active", true
	case jc.ContactOptedOut:
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

func (w *Worker) buildOutbound(jc *JobContext, spec *templates.SendSpec, job domain.ScheduledMessage) (messaging.Outbound, error) {
	ctxValues := w.renderValues(jc)
	body := render.Render(spec.Body, render.NewContext(ctxValues))

	out := messaging.Outbound{
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

	if jc.CampaignEventStart != nil {
		values["webinar_date"] = timex.FormatIn(*jc.CampaignEventStart, jc.CampaignTimezone, "02.01.2006")
		values["webinar_time"] = timex.FormatIn(*jc.CampaignEventStart, jc.CampaignTimezone, "15:04")
		values["webinar_datetime"] = timex.FormatIn(*jc.CampaignEventStart, jc.CampaignTimezone, "02.01.2006 15:04")
		values["remaining_time"] = timex.RemainingLabel(time.Now().UTC(), *jc.CampaignEventStart)
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

	if !retryable || attempts >= maxAttempts {
		reason := cause.Error()
		if !retryable {
			reason = "permanent failure: " + reason
		}
		if err := w.repo.Fail(ctx, job.ID, reason); err != nil {
			log.Error("marking job failed", slog.String("error", err.Error()))
		}
		log.Error("job failed permanently",
			slog.Int("attempts", attempts),
			slog.Bool("retryable", retryable),
			slog.String("error", cause.Error()))
		return
	}

	next := w.backoff.NextAttemptAt(time.Now().UTC(), attempts)
	if err := w.repo.Retry(ctx, job.ID, next, cause.Error()); err != nil {
		log.Error("scheduling retry failed", slog.String("error", err.Error()))
		return
	}

	log.Warn("job will be retried",
		slog.Int("attempts", attempts),
		slog.Time("next_attempt", next),
		slog.String("error", cause.Error()))
}

func contactFromJob(jc *JobContext) *domain.Contact {
	var blockedAt *time.Time
	if jc.ContactBlocked {
		now := time.Now().UTC()
		blockedAt = &now
	}

	return &domain.Contact{
		ID:             jc.Job.ContactID,
		Phone:          jc.ContactPhone,
		PhoneDisplay:   phone.Display(jc.ContactPhone),
		ChatID:         jc.ContactChatID,
		Name:           jc.ContactName,
		PushName:       jc.ContactPushName,
		Status:         jc.ContactStatus,
		OptedOut:       jc.ContactOptedOut,
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
