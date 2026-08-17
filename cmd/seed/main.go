// Command seed installs the default Airan campaign by hand.
//
// The server installs it automatically on a database with no campaigns in it,
// so this command exists for the cases that are not that: seeding a database
// that already holds other campaigns, pinning a specific webinar date, or
// pointing the funnel at a different room.
//
// The campaign definition lives in internal/seed and is shared with the server,
// so the two can never drift apart.
//
// Media-bearing steps are seeded as TEXT templates carrying the exact copy. The
// schema requires a media file for every non-TEXT type and requires VOICE
// templates to have an empty body, so a voice step cannot be created before its
// recording exists — and seeding a placeholder file would risk sending the
// placeholder to a real customer. Upload the assets in the admin panel and
// switch each template to the type named in its description.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/logging"
	"github.com/ayran/whatsapp-automation/internal/media"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
	"github.com/ayran/whatsapp-automation/internal/seed"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/internal/templates"
)

func main() {
	var (
		eventDate = flag.String("date", "",
			"webinar date, YYYY-MM-DD (default: the next "+seed.WebinarTime+" that has not passed)")
		eventTime = flag.String("time", seed.WebinarTime, "webinar start time, HH:MM")
		link      = flag.String("link", seed.WebinarLink, "webinar join link")
		activate  = flag.Bool("activate", false,
			"activate the campaign immediately; only do this once the media assets are uploaded")
		force = flag.Bool("force", false,
			"install even when the database already holds campaigns")
	)
	flag.Parse()

	if err := run(*eventDate, *eventTime, *link, *activate, *force); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run(eventDate, eventTime, link string, activate, force bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logging.New(cfg.App.LogLevel, "text")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db, err := sqlite.Connect(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer db.Close()

	mediaStore, err := media.NewStore(cfg.Media.StoragePath, cfg.Media.MaxUploadMB, cfg.Media.PublicURL)
	if err != nil {
		return err
	}
	transcoder := media.NewTranscoder(cfg.Media.FFmpegPath, cfg.Media.FFprobePath, cfg.Media.VoiceBitrate)
	mediaSvc := media.NewService(mediaStore, media.NewRepository(db), transcoder, log)

	campaignRepo := campaigns.NewRepository(db)
	deps := seed.Deps{
		Campaigns: campaigns.NewService(campaignRepo, scheduler.NewRepository(db),
			contacts.NewRepository(db), log),
		Templates: templates.NewService(templates.NewRepository(db), mediaSvc),
		Log:       log,
	}

	opts := seed.Options{
		EventDate: eventDate,
		EventTime: eventTime,
		Timezone:  cfg.App.DefaultTimezone,
		Link:      link,
		Activate:  activate,
	}

	install := seed.EnsureDefaultCampaign
	if force {
		install = seed.Install
	}

	result, err := install(ctx, deps, opts)
	if err != nil {
		return err
	}
	if result.Skipped {
		fmt.Println(result.SkipReason)
		fmt.Println("Pass -force to install anyway.")
		return nil
	}

	fmt.Println()
	fmt.Println("Seed complete.")
	fmt.Printf("  Campaign:  %s (%s)\n", result.CampaignName, result.CampaignStatus)
	fmt.Printf("  Event:     %s\n", result.EventStartAt.In(location(cfg.App.DefaultTimezone)).Format("2006-01-02 15:04 MST"))
	fmt.Printf("  Trigger:   %s\n", result.TriggerKeyword)
	fmt.Printf("  Templates: %d\n", result.Templates)
	fmt.Printf("  Steps:     %d\n", result.Steps)
	fmt.Println()
	fmt.Println("Next steps in the admin panel:")
	fmt.Println("  1. Upload the voice recordings and images under Шаблондар.")
	fmt.Println("  2. Switch each template to the type named in its description.")
	fmt.Println("  3. Review the timeline under Автоматтандыру.")
	if result.CampaignStatus != "ACTIVE" {
		fmt.Println("  4. Activate the campaign. It sends nothing until you do.")
	}
	fmt.Println()

	return nil
}

func location(tz string) *time.Location {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}
