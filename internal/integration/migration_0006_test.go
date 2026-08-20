//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/migrations"
)

// Migration safety for the daily-webinar-sequence schema change.
//
// The production database holds real campaigns, real contacts and a real queue.
// "The migration is additive" is a claim about SQL that is easy to make and
// easy to get wrong, so it is tested the way it will actually happen: a
// database is built on the previous schema, filled with data, and upgraded.
//
// What must hold afterwards:
//
//   - every row is still there, with its values unchanged;
//   - every existing step defaults to include_in_daily_webinar = 0, so no
//     campaign silently acquires a daily sequence and nothing changes for
//     anybody already in the funnel;
//   - the enrolment uniqueness the feature depends on is the one that has been
//     in the schema since 0001, so no constraint is added over live data and
//     no duplicate has to be cleaned up to deploy this;
//   - running the migration again does nothing, because a restart must not
//     re-apply what is already applied.

func TestMigration0006PreservesExistingData(t *testing.T) {
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "production.db")
	db, err := sqlite.Connect(ctx, config.Database{Path: path, MaxConns: 4})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer db.Close()

	// ---- the database as it exists in production today ----------------------
	before0006, pending := schemaBefore(t, "0006")
	if err := db.Migrate(ctx, os.DirFS(before0006), discardLogger()); err != nil {
		t.Fatalf("migrate to the pre-feature schema: %v", err)
	}
	if pending != 1 {
		t.Fatalf("migrations newer than 0005 = %d, want exactly the one under test", pending)
	}

	eventStart := time.Now().UTC().Add(6 * time.Hour)
	if _, err := db.Exec(ctx, `
		INSERT INTO campaigns (id, name, event_start_at, timezone, webinar_link, status,
		                       is_daily_recurring, recurrence_time)
		VALUES ('c0000000-0000-0000-0000-000000000006', 'Airan', $1, 'Asia/Almaty',
		        'https://bizon365.online/room/209010/ayrankaimaq', 'ACTIVE', 1, '21:00')`,
		eventStart); err != nil {
		t.Fatalf("seed campaign: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO message_templates (id, name, type, body)
		VALUES ('t0000000-0000-0000-0000-000000000006', 'tpl', 'TEXT', 'body')`); err != nil {
		t.Fatalf("seed template: %v", err)
	}
	for i, offset := range []int{-18000, -10800, -1800, 0} {
		if _, err := db.Exec(ctx, `
			INSERT INTO campaign_steps (campaign_id, offset_seconds, message_template_id, order_index)
			VALUES ('c0000000-0000-0000-0000-000000000006', $1,
			        't0000000-0000-0000-0000-000000000006', $2)`, offset, i+1); err != nil {
			t.Fatalf("seed step: %v", err)
		}
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO contacts (id, phone, chat_id, first_contact_at)
		VALUES ('a0000000-0000-0000-0000-000000000006', '77010000006',
		        '77010000006@c.us', now())`); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO campaign_contacts (id, campaign_id, contact_id, occurrence_at)
		VALUES ('e0000000-0000-0000-0000-000000000006',
		        'c0000000-0000-0000-0000-000000000006',
		        'a0000000-0000-0000-0000-000000000006', $1)`, eventStart); err != nil {
		t.Fatalf("seed enrolment: %v", err)
	}

	countAll := func() map[string]int {
		t.Helper()
		out := map[string]int{}
		for _, table := range []string{
			"campaigns", "campaign_steps", "campaign_contacts",
			"scheduled_messages", "contacts", "message_templates",
		} {
			var n int
			if err := db.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
			out[table] = n
		}
		return out
	}
	before := countAll()

	// ---- git pull, make run -------------------------------------------------
	if err := db.Migrate(ctx, migrations.FS, discardLogger()); err != nil {
		t.Fatalf("apply the new migration: %v", err)
	}

	for table, want := range countAll() {
		if before[table] != want {
			t.Errorf("%s holds %d rows after the migration, want %d — no data may be lost",
				table, want, before[table])
		}
	}

	// Not one existing step joins the daily sequence on its own. This is what
	// makes deploying the feature a no-op until an operator ticks a box, and it
	// is the difference between a safe migration and a mailing to everybody.
	var marked int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM campaign_steps WHERE include_in_daily_webinar <> 0`).Scan(&marked); err != nil {
		t.Fatalf("read migrated steps: %v", err)
	}
	if marked != 0 {
		t.Errorf("%d existing steps became part of a daily sequence; the feature must be opt-in", marked)
	}

	// The enrolment pinned before the upgrade still points at the same webinar,
	// so nothing queued against it changes anchor.
	var occurrence time.Time
	if err := db.QueryRow(ctx,
		`SELECT occurrence_at FROM campaign_contacts
		 WHERE id = 'e0000000-0000-0000-0000-000000000006'`).Scan(&occurrence); err != nil {
		t.Fatalf("read migrated enrolment: %v", err)
	}
	if !occurrence.Equal(eventStart.Truncate(time.Nanosecond)) && occurrence.Unix() != eventStart.Unix() {
		t.Errorf("occurrence moved across the migration: %s, want %s", occurrence, eventStart)
	}

	// One enrolment per contact per campaign is enforced by the schema, not by
	// application code — and by the constraint that has been there since 0001,
	// so this feature adds no constraint over live data and needs no duplicate
	// cleanup to deploy.
	_, err = db.Exec(ctx, `
		INSERT INTO campaign_contacts (campaign_id, contact_id)
		VALUES ('c0000000-0000-0000-0000-000000000006',
		        'a0000000-0000-0000-0000-000000000006')`)
	if err == nil {
		t.Fatal("a second enrolment for the same contact and campaign was accepted")
	}

	// A restart must not re-apply what is already applied.
	if err := db.Migrate(ctx, migrations.FS, discardLogger()); err != nil {
		t.Fatalf("second migration run: %v", err)
	}
	for table, want := range countAll() {
		if before[table] != want {
			t.Errorf("%s changed on the second migration run: %d, want %d", table, want, before[table])
		}
	}
}
