//go:build integration

// Package integration exercises the platform against a real SQLite database:
// migrations, concurrent job claiming, the anti-duplicate constraints and the
// end-to-end trigger flow.
//
// The database is a scratch file created per run, so there is nothing to
// provision. Run with:
//
//	go test -tags=integration ./internal/integration/...
//
// Set TEST_DATABASE_PATH to point at a specific file instead.
package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/conversations"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/internal/templates"
	"github.com/ayran/whatsapp-automation/migrations"
)

var testDB *sqlite.DB

// discardLogger keeps test output readable.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMain(m *testing.M) {
	path := os.Getenv("TEST_DATABASE_PATH")
	if path == "" {
		dir, err := os.MkdirTemp("", "whatsapp-integration-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "create temp dir: %v\n", err)
			os.Exit(1)
		}
		defer os.RemoveAll(dir)
		path = filepath.Join(dir, "test.db")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db, err := sqlite.Connect(ctx, config.Database{
		Path:     path,
		MaxConns: 10,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect to test database: %v\n", err)
		os.Exit(1)
	}

	if err := db.Migrate(ctx, migrations.FS, discardLogger()); err != nil {
		fmt.Fprintf(os.Stderr, "migrate test database: %v\n", err)
		os.Exit(1)
	}

	testDB = db
	code := m.Run()

	db.Close()
	os.Exit(code)
}

// resetTables lists every table child-first, so deleting in order never trips a
// foreign key even though the schema also cascades.
var resetTables = []string{
	"scheduled_messages", "messages", "campaign_contacts", "campaign_steps",
	"campaign_triggers", "campaigns", "message_template_versions", "message_templates",
	"contact_tags", "tags", "contacts", "media_files", "webhook_events", "audit_logs",
	"admin_sessions", "login_attempts", "admins",
}

// reset clears every table between tests so cases stay independent.
//
// SQLite has no TRUNCATE; a plain DELETE with the autoincrement counters reset
// is the equivalent.
func reset(t *testing.T) {
	t.Helper()

	ctx := context.Background()
	for _, table := range resetTables {
		if _, err := testDB.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}
	if _, err := testDB.Exec(ctx, "DELETE FROM sqlite_sequence"); err != nil {
		t.Fatalf("reset identity counters: %v", err)
	}
}

// ---------------------------------------------------------------- fixtures --

type fixture struct {
	campaignRepo *campaigns.Repository
	campaignSvc  *campaigns.Service
	contactRepo  *contacts.Repository
	messageRepo  *conversations.Repository
	templateRepo *templates.Repository
	jobRepo      *scheduler.Repository
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	reset(t)

	log := discardLogger()
	campaignRepo := campaigns.NewRepository(testDB)
	contactRepo := contacts.NewRepository(testDB)
	jobRepo := scheduler.NewRepository(testDB)

	return &fixture{
		campaignRepo: campaignRepo,
		campaignSvc:  campaigns.NewService(campaignRepo, jobRepo, contactRepo, log),
		contactRepo:  contactRepo,
		messageRepo:  conversations.NewRepository(testDB),
		templateRepo: templates.NewRepository(testDB),
		jobRepo:      jobRepo,
	}
}

// createTemplate inserts a minimal text template.
func (f *fixture) createTemplate(t *testing.T, name, body string) uuid.UUID {
	t.Helper()

	tpl := &domain.MessageTemplate{
		Name:        name,
		Type:        domain.TemplateText,
		Body:        body,
		LinkPreview: true,
	}
	if err := f.templateRepo.Create(context.Background(), tpl); err != nil {
		t.Fatalf("create template: %v", err)
	}
	return tpl.ID
}

// createCampaign builds an active campaign anchored at the given event time.
func (f *fixture) createCampaign(t *testing.T, name string, eventStart time.Time, offsets []int) *domain.Campaign {
	t.Helper()
	ctx := context.Background()

	campaign := &domain.Campaign{
		Name:                    name,
		EventType:               "WEBINAR",
		EventStartAt:            &eventStart,
		Timezone:                "Asia/Almaty",
		WebinarLink:             "https://example.com/live",
		Status:                  domain.CampaignDraft,
		ExistingContactBehavior: domain.BehaviorIgnore,
		UnsubscribeKeywords:     []string{"STOP", "ТОҚТАТУ"},
		CatchUpMissedSteps:      true,
		MaxSendAttempts:         5,
	}
	if err := f.campaignRepo.Create(ctx, nil, campaign); err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	for i, offset := range offsets {
		templateID := f.createTemplate(t, fmt.Sprintf("%s tpl %d", name, i), fmt.Sprintf("body %d", i))
		step := &domain.CampaignStep{
			CampaignID:    campaign.ID,
			Name:          fmt.Sprintf("step %d", i),
			OffsetSeconds: offset,
			TemplateID:    templateID,
			Enabled:       true,
			OrderIndex:    i + 1,
			ScheduleKind:  domain.ScheduleRelativeToEvent,
		}
		if err := f.campaignRepo.CreateStep(ctx, nil, step); err != nil {
			t.Fatalf("create step: %v", err)
		}
	}

	if err := f.campaignRepo.SetStatus(ctx, nil, campaign.ID, domain.CampaignActive); err != nil {
		t.Fatalf("activate campaign: %v", err)
	}

	full, err := f.campaignRepo.GetFull(ctx, campaign.ID)
	if err != nil {
		t.Fatalf("reload campaign: %v", err)
	}
	return full
}

func (f *fixture) addTrigger(t *testing.T, campaignID uuid.UUID, keyword string) {
	t.Helper()

	if _, err := f.campaignSvc.AddTrigger(context.Background(), campaignID, keyword, "EXACT", true); err != nil {
		t.Fatalf("add trigger: %v", err)
	}
}

// createContact inserts a contact that has already written to us, which is the
// consent precondition for any outbound message.
func (f *fixture) createContact(t *testing.T, phone string) *domain.Contact {
	t.Helper()

	contact, _, err := f.contactRepo.UpsertFromInbound(
		context.Background(), nil, phone+"@c.us", phone, "Test User", time.Now().UTC())
	if err != nil {
		t.Fatalf("create contact: %v", err)
	}
	return contact
}
