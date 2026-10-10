// Command server runs the WhatsApp campaign automation platform: the REST
// API, the admin dashboard, the provider notification poller and the
// background workers.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ayran/whatsapp-automation/internal/analytics"
	"github.com/ayran/whatsapp-automation/internal/api"
	"github.com/ayran/whatsapp-automation/internal/audit"
	"github.com/ayran/whatsapp-automation/internal/auth"
	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/conversations"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/exports"
	"github.com/ayran/whatsapp-automation/internal/inbound"
	"github.com/ayran/whatsapp-automation/internal/instances"
	"github.com/ayran/whatsapp-automation/internal/logging"
	"github.com/ayran/whatsapp-automation/internal/media"
	"github.com/ayran/whatsapp-automation/internal/messaging"
	"github.com/ayran/whatsapp-automation/internal/outbound"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
	"github.com/ayran/whatsapp-automation/internal/seed"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/internal/templates"
	"github.com/ayran/whatsapp-automation/internal/whatsapp/greenapi"
	"github.com/ayran/whatsapp-automation/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logging.New(cfg.App.LogLevel, cfg.App.LogFormat)
	slog.SetDefault(log)

	log.Info("starting whatsapp automation platform",
		slog.String("env", cfg.App.Env),
		slog.Int("port", cfg.HTTP.Port),
		slog.String("timezone", cfg.App.DefaultTimezone))

	// Signals cancel the root context, which every worker observes.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqlite.Connect(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer db.Close()
	log.Info("database connected")

	if cfg.Database.AutoMigrate {
		if err := db.Migrate(ctx, migrations.FS, log); err != nil {
			return fmt.Errorf("run migrations: %w", err)
		}
		log.Info("database schema up to date")
	}

	// ---- infrastructure ---------------------------------------------------

	mediaStore, err := media.NewStore(cfg.Media.StoragePath, cfg.Media.MaxUploadMB, cfg.Media.PublicURL)
	if err != nil {
		return fmt.Errorf("initialise media storage: %w", err)
	}

	transcoder := media.NewTranscoder(cfg.Media.FFmpegPath, cfg.Media.FFprobePath, cfg.Media.VoiceBitrate)
	if !transcoder.Available() {
		log.Warn("ffmpeg not found: voice messages require an OGG/Opus upload",
			slog.String("configured_path", cfg.Media.FFmpegPath))
	}

	// One WhatsApp account, one immutable client. The registry is built once,
	// after migrations have guaranteed the account rows exist, and is never
	// written to afterwards -- so no request path can swap a live client's
	// credentials, and two accounts sending at the same moment cannot reach
	// each other's.
	instanceRepo := instances.NewRepository(db)
	registry, err := instances.Load(ctx, instanceRepo, cfg, log)
	if err != nil {
		return fmt.Errorf("load whatsapp accounts: %w", err)
	}

	primary, err := registry.Get(domain.PrimaryInstanceID)
	if err != nil {
		return fmt.Errorf("resolve the primary whatsapp account: %w", err)
	}
	provider := primary.Client
	if provider == nil || !provider.Configured() {
		provider = greenapi.New(cfg.GreenAPI, log)
		log.Warn("green api credentials are not set for the primary account: " +
			"inbound and outbound messaging is disabled for it")
	}

	// ---- repositories -----------------------------------------------------

	authRepo := auth.NewRepository(db)
	contactRepo := contacts.NewRepository(db)
	messageRepo := conversations.NewRepository(db)
	templateRepo := templates.NewRepository(db)
	campaignRepo := campaigns.NewRepository(db)
	jobRepo := scheduler.NewRepository(db)
	mediaRepo := media.NewRepository(db)
	notificationRepo := inbound.NewRepository(db)

	// ---- services ---------------------------------------------------------

	auditLog := audit.NewLogger(db, log)
	authSvc := auth.NewService(cfg.Auth, authRepo, log)
	mediaSvc := media.NewService(mediaStore, mediaRepo, transcoder, log)
	templateSvc := templates.NewService(templateRepo, mediaSvc)
	campaignSvc := campaigns.NewService(campaignRepo, jobRepo, contactRepo, log).
		WithTriggerDelay(cfg.Scheduler.TriggerDelay)
	// The sender resolves a client per message from the account the message
	// carries, so there is no such thing as "the" provider on the send path.
	sender := messaging.NewSender(registry, messageRepo, contactRepo, mediaStore, log)
	analyticsSvc := analytics.NewService(db)
	exportSvc := exports.NewService(contactRepo)

	// Every automated message leaves through this router, which holds one
	// queue per account: each bounds how many of that account's sends run at
	// once and how closely together they go out. Scheduler workers hand
	// messages to it rather than calling a provider themselves, and a burst on
	// one number cannot delay the other's reminders.
	outboundSpecs := make([]outbound.InstanceSpec, 0, len(registry.All()))
	for _, inst := range registry.All() {
		outboundSpecs = append(outboundSpecs, outbound.InstanceSpec{
			ID:       inst.ID,
			Key:      inst.Key,
			Outbound: inst.Outbound,
		})
	}
	outboundGate := outbound.NewRouter(outboundSpecs, sender, log)

	notifications := inbound.NewProcessor(cfg.Scheduler, notificationRepo, contactRepo,
		messageRepo, campaignSvc, templateRepo, outboundGate, mediaStore, log)

	worker := scheduler.NewWorker(cfg.Scheduler, jobRepo, templateRepo, contactRepo,
		outboundGate, mediaStore, log)

	if err := authSvc.EnsureBootstrapAdmin(ctx); err != nil {
		return fmt.Errorf("bootstrap administrator: %w", err)
	}

	// A fresh installation gets the default campaign so the panel opens on
	// something working rather than an empty page. A database that already has
	// campaigns in it is left completely alone — see internal/seed.
	if cfg.App.SeedDefaultCampaign {
		result, err := seed.EnsureDefaultCampaign(ctx, seed.Deps{
			Campaigns: campaignSvc,
			Templates: templateSvc,
			Log:       log,
		}, seed.Options{
			Timezone: cfg.App.DefaultTimezone,
			Activate: cfg.App.SeedCampaignActive,
		})
		if err != nil {
			// Seeding is a convenience, not a prerequisite. A failure here must
			// not stop an otherwise healthy system from starting.
			log.Error("installing the default campaign failed", slog.String("error", err.Error()))
		} else if result.Skipped {
			log.Info("default campaign not installed", slog.String("reason", result.SkipReason))
		} else {
			log.Info("default campaign installed",
				slog.String("campaign", result.CampaignName),
				slog.String("campaign_id", result.CampaignID.String()),
				slog.String("status", string(result.CampaignStatus)),
				slog.String("trigger", result.TriggerKeyword),
				slog.Time("event_start_at", result.EventStartAt),
				slog.Int("templates", result.Templates),
				slog.Int("steps", result.Steps))
			if result.CampaignStatus == domain.CampaignDraft {
				log.Warn("the default campaign is a DRAFT and sends nothing yet: " +
					"upload the voice and image assets, switch each template to its intended " +
					"type, then activate it in the panel")
			}
		}
	}

	// ---- background work --------------------------------------------------

	var receiversDone []chan struct{}

	authSvc.StartMaintenance(ctx)
	notifications.Start(ctx)

	// One poller per account, each in its own goroutine with its own client
	// and its own backoff state.
	//
	// The number of goroutines is bounded by the number of configured
	// accounts, which is bounded by the environment -- there is no per-message
	// or per-notification goroutine here. Concurrency for the work itself
	// lives behind Ingest, in the notification worker pool, exactly as it did
	// with one account.
	//
	// Failure isolation falls out of the structure rather than being handled:
	// a second number whose subscription lapsed retries on its own timer, in
	// its own loop, and the first number's poller never observes it. Neither
	// can crash the other, and neither can stop the scheduler or the panel.
	active := registry.Active()
	if len(active) == 0 {
		log.Warn("[WHATSAPP] no account is both enabled and configured: inbound polling is disabled")
	}
	for _, inst := range active {
		done := make(chan struct{})
		receiversDone = append(receiversDone, done)

		receiver := inbound.NewReceiver(inst.ID, inst.Key, inst.Client, notifications, inst.Poll, log)
		go func(r *inbound.Receiver, done chan struct{}) {
			defer close(done)
			r.Run(ctx)
		}(receiver, done)

		log.Info("[WHATSAPP] inbound poller started",
			slog.Int64("whatsapp_instance_id", inst.ID),
			slog.String("instance", inst.Key),
			slog.String("name", inst.Name))
	}

	if cfg.Scheduler.Enabled {
		worker.Start(ctx)
	} else {
		log.Warn("scheduler is disabled: scheduled messages will not be sent")
	}

	go reconcileLoop(ctx, campaignSvc, cfg.Scheduler.ReconcileInterval, log)

	// ---- http -------------------------------------------------------------

	server := api.NewServer(api.Dependencies{
		Config:           cfg,
		Log:              log,
		DB:               db,
		Auth:             authSvc,
		Audit:            auditLog,
		Contacts:         contactRepo,
		Messages:         messageRepo,
		Campaigns:        campaignSvc,
		Templates:        templateSvc,
		TemplateRepo:     templateRepo,
		Media:            mediaSvc,
		Jobs:             jobRepo,
		Sender:           sender,
		Notifications:    notifications,
		NotificationRepo: notificationRepo,
		Analytics:        analyticsSvc,
		Exports:          exportSvc,
		Provider:         provider,
		Instances:        instanceRepo,
		Registry:         registry,
	})

	httpServer := &http.Server{
		Addr:        fmt.Sprintf(":%d", cfg.HTTP.Port),
		Handler:     server.Handler(),
		ReadTimeout: cfg.HTTP.ReadTimeout,
		IdleTimeout: cfg.HTTP.IdleTimeout,
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		BaseContext: func(net.Listener) context.Context { return ctx },
		// WriteTimeout stays unset: media downloads stream large files and a
		// blanket deadline would truncate them. Per-handler timeouts bound the
		// ordinary requests instead.
		ReadHeaderTimeout: 15 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", slog.String("addr", httpServer.Addr))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Stop accepting new work, then give in-flight requests a chance to
	// finish before the workers' contexts are already cancelled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", slog.String("error", err.Error()))
		_ = httpServer.Close()
	}

	// Workers observe the cancelled root context; wait for them to drain.
	done := make(chan struct{})
	go func() {
		worker.Wait()
		notifications.Wait()
		// Every poller observes the same cancelled root context, so they stop
		// in parallel; this only waits for the last one to finish the
		// notification it was holding.
		for _, receiverDone := range receiversDone {
			<-receiverDone
		}
		close(done)
	}()

	select {
	case <-done:
		log.Info("background workers stopped")
	case <-time.After(cfg.HTTP.ShutdownTimeout):
		log.Warn("background workers did not stop in time; exiting anyway")
	}

	log.Info("shutdown complete")
	return nil
}

// reconcileLoop keeps the queue in agreement with the campaigns that define it.
//
// Every running enrollment is compared against its campaign's steps: a step
// with no job gets one, a step whose time moved has its pending job moved, and
// an enrollment whose steps have all resolved is closed out. It is the same
// operation the step editor performs, run on a timer.
//
// The point of running it periodically is that it does not depend on anything
// having noticed. Every event-driven path can fail — a handler that misses a
// case, a process killed between two commits, a step written by hand — and each
// failure used to mean a contact silently stopped receiving messages, with
// nothing in the system able to detect it. Here the queue converges on the
// campaign definition regardless of how it drifted.
//
// It runs immediately on start rather than waiting out the first tick, so a
// restart repairs anything that was lost while the process was down.
func reconcileLoop(ctx context.Context, svc *campaigns.Service, interval time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = 30 * time.Second
	}

	run := func() {
		stats, err := svc.ReconcileAll(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("reconciliation sweep failed", slog.String("error", err.Error()))
			}
			return
		}
		// A correct system reconciles silently. Anything logged here is a gap
		// that was found and repaired, which is worth seeing.
		if stats.Changed() {
			log.Info("reconciliation repaired the queue",
				slog.Int("enrollments_checked", stats.EnrollmentsChecked),
				slog.Int("jobs_created", stats.JobsCreated),
				slog.Int("jobs_moved", stats.JobsMoved),
				slog.Int("jobs_cancelled", stats.JobsCancelled),
				slog.Int("skips_recorded", stats.SkipsRecorded),
				slog.Int("enrollments_reopened", stats.EnrollmentsReopen),
				slog.Int("enrollments_completed", stats.EnrollmentsDone))
		}
	}

	run()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
