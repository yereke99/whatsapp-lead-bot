//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/migrations"
)

// Migration safety for the recurring-webinar schema change.
//
// The production database holds real campaigns, real contacts and a real queue.
// "The migration is additive" is a claim about SQL that is easy to make and easy
// to get wrong, so it is tested the way it will actually happen: a database is
// built on the previous schema, filled with data, and then upgraded.
//
// What must hold afterwards:
//
//   - every row is still there, with its values unchanged;
//   - existing campaigns default to is_daily_recurring = 0, so none of them
//     silently becomes recurring;
//   - existing enrolments have occurrence_at NULL, so none of their queued
//     messages changes anchor;
//   - running the migration again does nothing, because a restart must not
//     re-apply what is already applied.

// schemaBeforeRecurrence copies migrations 0001-0004 into a temp directory, so
// a database can be built on the schema exactly as it stands in production
// before this feature is deployed.
func schemaBeforeRecurrence(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		if name >= "0005" {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestMigration0005PreservesExistingData(t *testing.T) {
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "production.db")
	db, err := sqlite.Connect(ctx, config.Database{Path: path, MaxConns: 4})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()

	// ---- the database as it exists in production today ----------------------
	if err := db.Migrate(ctx, os.DirFS(schemaBeforeRecurrence(t)), discardLogger()); err != nil {
		t.Fatalf("migrate to the pre-feature schema: %v", err)
	}

	eventStart := time.Now().UTC().Add(6 * time.Hour)
	if _, err := db.Exec(ctx, `
		INSERT INTO campaigns (id, name, event_start_at, timezone, webinar_link, status)
		VALUES ('c0000000-0000-0000-0000-000000000001', 'Airan', $1, 'Asia/Almaty',
		        'https://bizon365.online/room/209010/ayrankaimaq', 'ACTIVE')`, eventStart); err != nil {
		t.Fatalf("seed campaign: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO message_templates (id, name, type, body)
		VALUES ('t0000000-0000-0000-0000-000000000001', 'tpl', 'TEXT', 'body')`); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO campaign_steps (id, campaign_id, offset_seconds, message_template_id, order_index)
		VALUES ('s0000000-0000-0000-0000-000000000001',
		        'c0000000-0000-0000-0000-000000000001', -1800,
		        't0000000-0000-0000-0000-000000000001', 1)`); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO contacts (id, phone, chat_id, first_contact_at)
		VALUES ('a0000000-0000-0000-0000-000000000001', '77011112233', '77011112233@c.us', now())`); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO campaign_contacts (id, campaign_id, contact_id)
		VALUES ('e0000000-0000-0000-0000-000000000001',
		        'c0000000-0000-0000-0000-000000000001',
		        'a0000000-0000-0000-0000-000000000001')`); err != nil {
		t.Fatalf("seed enrolment: %v", err)
	}

	jobAt := eventStart.Add(-30 * time.Minute)
	if _, err := db.Exec(ctx, `
		INSERT INTO scheduled_messages (id, campaign_id, contact_id, enrollment_id, campaign_step_id, scheduled_at)
		VALUES ('j0000000-0000-0000-0000-000000000001',
		        'c0000000-0000-0000-0000-000000000001',
		        'a0000000-0000-0000-0000-000000000001',
		        'e0000000-0000-0000-0000-000000000001',
		        's0000000-0000-0000-0000-000000000001', $1)`, jobAt); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	countAll := func(label string) map[string]int {
		t.Helper()
		out := map[string]int{}
		for _, table := range []string{
			"campaigns", "campaign_steps", "campaign_contacts",
			"scheduled_messages", "contacts", "message_templates", "schema_migrations",
		} {
			var n int
			if err := db.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
				t.Fatalf("%s count %s: %v", label, table, err)
			}
			out[table] = n
		}
		return out
	}
	before := countAll("before")

	// ---- git pull, make run -------------------------------------------------
	if err := db.Migrate(ctx, migrations.FS, discardLogger()); err != nil {
		t.Fatalf("apply the new migration: %v", err)
	}

	after := countAll("after")
	for table, want := range before {
		if table == "schema_migrations" {
			// One new row: the migration that was just applied.
			if after[table] != want+1 {
				t.Errorf("schema_migrations = %d, want %d (exactly one new migration)", after[table], want+1)
			}
			continue
		}
		if after[table] != want {
			t.Errorf("%s holds %d rows after the migration, want %d — no data may be lost",
				table, after[table], want)
		}
	}

	// Existing campaigns are not recurring, and carry no recurrence settings.
	var recurring int
	var clock, startDate *string
	if err := db.QueryRow(ctx, `
		SELECT is_daily_recurring, recurrence_time, recurrence_start_date
		FROM campaigns WHERE id = 'c0000000-0000-0000-0000-000000000001'`).
		Scan(&recurring, &clock, &startDate); err != nil {
		t.Fatalf("read migrated campaign: %v", err)
	}
	if recurring != 0 {
		t.Error("an existing campaign became recurring; the feature must be opt-in")
	}
	if clock != nil || startDate != nil {
		t.Errorf("recurrence settings were invented: time=%v start=%v", clock, startDate)
	}

	// Existing enrolments pin no occurrence, so their queue keeps its anchor.
	var occurrence *time.Time
	if err := db.QueryRow(ctx,
		`SELECT occurrence_at FROM campaign_contacts WHERE id = 'e0000000-0000-0000-0000-000000000001'`).
		Scan(&occurrence); err != nil {
		t.Fatalf("read migrated enrolment: %v", err)
	}
	if occurrence != nil {
		t.Errorf("occurrence_at = %v, want NULL for an enrolment that predates the feature", occurrence)
	}

	// The queued message is exactly where it was.
	var scheduledAt time.Time
	var status string
	if err := db.QueryRow(ctx,
		`SELECT scheduled_at, status FROM scheduled_messages WHERE id = 'j0000000-0000-0000-0000-000000000001'`).
		Scan(&scheduledAt, &status); err != nil {
		t.Fatalf("read migrated job: %v", err)
	}
	if !scheduledAt.Equal(jobAt) || status != "PENDING" {
		t.Errorf("queued job moved: %s/%s, want %s/PENDING", scheduledAt, status, jobAt)
	}

	// ---- make run-restart, twice more --------------------------------------
	for i := 0; i < 2; i++ {
		if err := db.Migrate(ctx, migrations.FS, discardLogger()); err != nil {
			t.Fatalf("restart %d re-ran the migration and failed: %v", i, err)
		}
	}
	final := countAll("final")
	for table, want := range after {
		if final[table] != want {
			t.Errorf("%s changed on restart: %d, want %d", table, final[table], want)
		}
	}
}

// TestCampaignRowsFromBeforeTheMigrationStillLoad is the regression test for a
// production outage this feature caused on its first deploy.
//
// The new recurrence columns are nullable, so every campaign that predates
// migration 0005 holds NULL in them. The model carries them as plain strings,
// and scanning NULL into a string fails — which took out the campaign list, the
// campaign detail page and the reconciliation sweep the moment the binary met a
// real database.
//
// The original tests missed it from both sides: the migration test read those
// columns as *string, and every other test created its campaigns through the
// service, which writes ” rather than NULL. Nothing ever loaded a NULL row
// through the repository, which is the only path the panel uses.
//
// So this test writes the row the way an existing database holds it — raw SQL,
// columns left unset — and reads it back the way the panel does.
func TestCampaignRowsFromBeforeTheMigrationStillLoad(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(3 * time.Hour)
	if _, err := testDB.Exec(ctx, `
		INSERT INTO campaigns (id, name, event_start_at, timezone, status,
		                       is_daily_recurring, recurrence_time, recurrence_start_date)
		VALUES ('d0000000-0000-0000-0000-000000000001', 'Legacy Airan', $1, 'Asia/Almaty',
		        'ACTIVE', 0, NULL, NULL)`, eventStart); err != nil {
		t.Fatalf("insert a pre-migration campaign row: %v", err)
	}
	id := uuid.MustParse("d0000000-0000-0000-0000-000000000001")

	// GET /api/campaigns/{id}
	campaign, err := f.campaignRepo.GetByID(ctx, nil, id)
	if err != nil {
		t.Fatalf("GetByID on a NULL-recurrence row: %v", err)
	}
	if campaign == nil {
		t.Fatal("GetByID returned no campaign")
	}
	if campaign.IsDailyRecurring {
		t.Error("a row with is_daily_recurring = 0 loaded as recurring")
	}
	if campaign.RecurrenceTime != "" || campaign.RecurrenceStartDate != "" {
		t.Errorf("NULL recurrence columns loaded as %q/%q, want empty strings",
			campaign.RecurrenceTime, campaign.RecurrenceStartDate)
	}
	if campaign.NextOccurrenceAt != nil {
		t.Errorf("NextOccurrenceAt = %v, want nil", campaign.NextOccurrenceAt)
	}

	// GET /api/campaigns
	list, err := f.campaignRepo.List(ctx, campaigns.ListFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("List with a NULL-recurrence row present: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("List returned nothing")
	}

	// GET /api/campaigns/{id} with steps and triggers, which the detail page and
	// the validation endpoint both use.
	if _, err := f.campaignRepo.GetFull(ctx, id); err != nil {
		t.Fatalf("GetFull on a NULL-recurrence row: %v", err)
	}

	// And the sweep, which failed every thirty seconds during the outage.
	if _, err := f.campaignSvc.ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll with a NULL-recurrence row present: %v", err)
	}
}
