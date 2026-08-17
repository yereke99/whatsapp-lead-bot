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
	"github.com/ayran/whatsapp-automation/internal/exports"
	"github.com/ayran/whatsapp-automation/internal/inbound"
	"github.com/ayran/whatsapp-automation/internal/logging"
	"github.com/ayran/whatsapp-automation/internal/media"
	"github.com/ayran/whatsapp-automation/internal/messaging"
	"github.com/ayran/whatsapp-automation/internal/outbound"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
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

	provider := greenapi.New(cfg.GreenAPI, log)
	if !provider.Configured() {
		log.Warn("green api credentials are not set: inbound and outbound messaging is disabled")
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
	sender := messaging.NewSender(provider, messageRepo, contactRepo, mediaStore, log)
	analyticsSvc := analytics.NewService(db)
	exportSvc := exports.NewService(contactRepo)

	// Every automated message leaves through this one gate, which bounds how
	// many sends run at once and how closely together they go out. Scheduler
	// workers hand messages to it rather than calling the provider themselves.
	outboundGate := outbound.New(cfg.Outbound, sender, log)

	notifications := inbound.NewProcessor(cfg.Scheduler, notificationRepo, contactRepo,
		messageRepo, campaignSvc, templateRepo, sender, mediaStore, log)
	receiver := inbound.NewReceiver(provider, notifications, cfg.GreenAPI, log)

	worker := scheduler.NewWorker(cfg.Scheduler, jobRepo, templateRepo, contactRepo,
		outboundGate, mediaStore, log)

	if err := authSvc.EnsureBootstrapAdmin(ctx); err != nil {
		return fmt.Errorf("bootstrap administrator: %w", err)
	}

	// ---- background work --------------------------------------------------

	var receiverDone chan struct{}

	authSvc.StartMaintenance(ctx)
	notifications.Start(ctx)

	// Inbound messages are pulled from the provider's queue; without
	// credentials there is nothing to poll, and the panel stays usable.
	if cfg.WhatsAppConfigured() {
		receiverDone = make(chan struct{})
		go func() {
			defer close(receiverDone)
			receiver.Run(ctx)
		}()
	} else {
		log.Warn("green api is not configured: inbound polling is disabled")
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
		if receiverDone != nil {
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
