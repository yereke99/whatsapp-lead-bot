//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/seed"
	"github.com/ayran/whatsapp-automation/pkg/textnorm"
)

// Migration 0003 swaps the Airan campaign's trigger from the single word
// "АЙРАН" to the full opt-in sentence. These tests cover the four states a
// production database can be in when it runs, and the guarantee that everything
// other than campaign_triggers is left alone.

const (
	oldTriggerKeyword = "АЙРАН"
	newTriggerKeyword = "Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді"

	// The literal written into migration 0003. It has to equal what the Go
	// normalizer produces, or the trigger silently never fires.
	migrationNormalized = "айран/қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді"
)

// migration0003 is the statement sequence from
// migrations/0003_airan_trigger_phrase.sql.
//
// The migration itself is applied once per test binary, against a database that
// has no campaigns in it, so re-running the real file is how these tests reach
// the states a production database would be in. The SQL is kept identical to
// the migration on purpose: if the two drift, these tests stop testing it.
func migration0003(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	statements := []string{
		`UPDATE campaign_triggers SET
			keyword            = '` + newTriggerKeyword + `',
			normalized_keyword = '` + migrationNormalized + `',
			match_mode         = 'EXACT',
			is_active          = 1
		WHERE id = (
			SELECT t.id FROM campaign_triggers t
			JOIN campaigns c ON c.id = t.campaign_id
			WHERE lower(trim(c.name)) = 'airan' AND c.archived_at IS NULL
			  AND t.normalized_keyword = 'айран' AND t.is_active
			ORDER BY t.created_at LIMIT 1
		)
		AND NOT EXISTS (
			SELECT 1 FROM campaign_triggers t2
			JOIN campaigns c2 ON c2.id = t2.campaign_id
			WHERE lower(trim(c2.name)) = 'airan' AND c2.archived_at IS NULL
			  AND t2.normalized_keyword = '` + migrationNormalized + `'
		)`,

		`INSERT INTO campaign_triggers (campaign_id, keyword, normalized_keyword, match_mode, is_active)
		SELECT c.id, '` + newTriggerKeyword + `', '` + migrationNormalized + `', 'EXACT', 1
		FROM campaigns c
		WHERE lower(trim(c.name)) = 'airan' AND c.archived_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM campaign_triggers t
			WHERE t.campaign_id = c.id
			  AND t.normalized_keyword = '` + migrationNormalized + `'
		  )`,

		`UPDATE campaign_triggers SET is_active = 0
		WHERE normalized_keyword = 'айран' AND is_active
		  AND campaign_id IN (
			SELECT c.id FROM campaigns c
			WHERE lower(trim(c.name)) = 'airan' AND c.archived_at IS NULL
		  )`,
	}

	for i, stmt := range statements {
		if _, err := testDB.Exec(ctx, stmt); err != nil {
			t.Fatalf("migration 0003 statement %d: %v", i+1, err)
		}
	}
}

// airanTriggers lists the campaign's triggers, newest state first.
func airanTriggers(t *testing.T, campaignID uuid.UUID) []domain.CampaignTrigger {
	t.Helper()

	rows, err := testDB.Query(context.Background(), `
		SELECT id, keyword, normalized_keyword, match_mode, is_active
		FROM campaign_triggers WHERE campaign_id = $1 ORDER BY created_at`, campaignID)
	if err != nil {
		t.Fatalf("list triggers: %v", err)
	}
	defer rows.Close()

	var out []domain.CampaignTrigger
	for rows.Next() {
		var tr domain.CampaignTrigger
		if err := rows.Scan(&tr.ID, &tr.Keyword, &tr.Normalized, &tr.MatchMode, &tr.IsActive); err != nil {
			t.Fatalf("scan trigger: %v", err)
		}
		out = append(out, tr)
	}
	return out
}

func activeTriggers(list []domain.CampaignTrigger) []domain.CampaignTrigger {
	var out []domain.CampaignTrigger
	for _, tr := range list {
		if tr.IsActive {
			out = append(out, tr)
		}
	}
	return out
}

// seedWithOldTrigger installs Airan the way it existed before this change: the
// current seeder carries the new phrase, so the trigger is rewritten back to
// the old word to reproduce a pre-migration database.
func seedWithOldTrigger(t *testing.T, f *fixture) *seed.Result {
	t.Helper()
	ctx := context.Background()

	result, err := seed.EnsureDefaultCampaign(ctx, seedDeps(t, f), seed.Options{Activate: true})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := testDB.Exec(ctx, `
		UPDATE campaign_triggers SET keyword = $2, normalized_keyword = 'айран'
		WHERE campaign_id = $1`, result.CampaignID, oldTriggerKeyword); err != nil {
		t.Fatalf("restore the old trigger: %v", err)
	}
	return result
}

// TestMigrationLiteralMatchesNormalizer is the pin that stops the stored
// keyword drifting away from the matcher.
//
// The migration cannot call textnorm.Normalize — it is SQL — so it hard-codes
// the normalized form. If the normalizer ever changes how it folds case,
// punctuation or whitespace, the stored value stops matching and the campaign
// quietly accepts nobody. This test turns that into a build failure.
func TestMigrationLiteralMatchesNormalizer(t *testing.T) {
	if got := textnorm.Normalize(newTriggerKeyword); got != migrationNormalized {
		t.Fatalf("the normalizer produces %q but migration 0003 stores %q;\n"+
			"update the literal in migrations/0003_airan_trigger_phrase.sql", got, migrationNormalized)
	}
	if got := textnorm.Normalize(oldTriggerKeyword); got != "айран" {
		t.Fatalf("the old trigger normalizes to %q, not \"айран\"; "+
			"migration 0003 would not find it", got)
	}
	// The seeder and the migration must agree, or a fresh install and a
	// migrated one would accept different messages.
	if seed.TriggerPhrase != newTriggerKeyword {
		t.Errorf("seed.TriggerPhrase = %q, want %q", seed.TriggerPhrase, newTriggerKeyword)
	}
}

// TestMigrationReplacesOldTrigger is scenario 1: the campaign exists with the
// old word, and the migration promotes it in place.
func TestMigrationReplacesOldTrigger(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	result := seedWithOldTrigger(t, f)
	before := airanTriggers(t, result.CampaignID)
	if len(before) != 1 {
		t.Fatalf("%d triggers before the migration, want 1", len(before))
	}
	originalID := before[0].ID

	migration0003(t)

	after := airanTriggers(t, result.CampaignID)
	active := activeTriggers(after)
	if len(active) != 1 {
		t.Fatalf("%d active triggers after the migration, want exactly 1", len(active))
	}

	// The row is promoted, not replaced: campaign_contacts.trigger_id points at
	// it, so a new id would rewrite how every enrolled contact arrived.
	if active[0].ID != originalID {
		t.Errorf("trigger id changed from %s to %s; the row must be updated in place",
			originalID, active[0].ID)
	}
	if active[0].Keyword != newTriggerKeyword {
		t.Errorf("keyword = %q, want %q", active[0].Keyword, newTriggerKeyword)
	}
	if active[0].Normalized != migrationNormalized {
		t.Errorf("normalized_keyword = %q, want %q", active[0].Normalized, migrationNormalized)
	}
	if active[0].MatchMode != "EXACT" {
		t.Errorf("match_mode = %q, want EXACT", active[0].MatchMode)
	}

	// The new phrase matches; the old word no longer does.
	match, err := f.campaignSvc.MatchTrigger(ctx, nil, newTriggerKeyword)
	if err != nil {
		t.Fatalf("MatchTrigger: %v", err)
	}
	if match == nil {
		t.Fatal("the new trigger phrase does not match")
	}
	if match.CampaignID != result.CampaignID {
		t.Errorf("the phrase matched campaign %s, want Airan (%s)", match.CampaignID, result.CampaignID)
	}

	for _, text := range []string{oldTriggerKeyword, "айран", "Айран"} {
		match, err := f.campaignSvc.MatchTrigger(ctx, nil, text)
		if err != nil {
			t.Fatalf("MatchTrigger(%q): %v", text, err)
		}
		if match != nil {
			t.Errorf("%q still matches after the migration", text)
		}
	}

	// Unrelated messages, including ones that mention the word, stay out.
	for _, text := range []string{"Сәлем", "айран ішкім келеді", "қаймақ"} {
		match, err := f.campaignSvc.MatchTrigger(ctx, nil, text)
		if err != nil {
			t.Fatalf("MatchTrigger(%q): %v", text, err)
		}
		if match != nil {
			t.Errorf("%q matched but should not have", text)
		}
	}
}

// TestMigrationIsIdempotent runs the statements repeatedly, which is what a
// re-applied migration or a rerun deploy would do.
func TestMigrationIsIdempotent(t *testing.T) {
	f := newFixture(t)

	result := seedWithOldTrigger(t, f)
	migration0003(t)

	after := airanTriggers(t, result.CampaignID)
	for i := 0; i < 5; i++ {
		migration0003(t)
		again := airanTriggers(t, result.CampaignID)
		if len(again) != len(after) {
			t.Fatalf("run %d changed the trigger count from %d to %d",
				i+2, len(after), len(again))
		}
	}

	if got := len(activeTriggers(after)); got != 1 {
		t.Errorf("%d active triggers, want 1", got)
	}
}

// TestMigrationOnAlreadyMigratedDatabase is scenario 2: the new phrase is
// already there, so nothing should change. This is the state of every database
// seeded after the change.
func TestMigrationOnAlreadyMigratedDatabase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	result, err := seed.EnsureDefaultCampaign(ctx, seedDeps(t, f), seed.Options{Activate: true})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	before := airanTriggers(t, result.CampaignID)
	migration0003(t)
	after := airanTriggers(t, result.CampaignID)

	if len(before) != len(after) {
		t.Fatalf("trigger count changed from %d to %d on an already-migrated database",
			len(before), len(after))
	}
	if before[0].ID != after[0].ID || before[0].Keyword != after[0].Keyword {
		t.Error("the existing trigger was rewritten on an already-migrated database")
	}
}

// TestMigrationRecreatesMissingTrigger is scenario 3: the campaign is there but
// its trigger was deleted by hand.
func TestMigrationRecreatesMissingTrigger(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	result, err := seed.EnsureDefaultCampaign(ctx, seedDeps(t, f), seed.Options{Activate: true})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := testDB.Exec(ctx,
		`DELETE FROM campaign_triggers WHERE campaign_id = $1`, result.CampaignID); err != nil {
		t.Fatalf("delete trigger: %v", err)
	}

	migration0003(t)

	active := activeTriggers(airanTriggers(t, result.CampaignID))
	if len(active) != 1 {
		t.Fatalf("%d active triggers, want 1 to have been created", len(active))
	}
	if active[0].Keyword != newTriggerKeyword {
		t.Errorf("keyword = %q, want %q", active[0].Keyword, newTriggerKeyword)
	}
}

// TestMigrationDeactivatesDuplicateOldTrigger is the duplicate-protection case:
// both phrases exist, so the old one is retired rather than a third row added.
func TestMigrationDeactivatesDuplicateOldTrigger(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	result, err := seed.EnsureDefaultCampaign(ctx, seedDeps(t, f), seed.Options{Activate: true})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The operator had added the old word alongside the new phrase.
	if _, err := f.campaignSvc.AddTrigger(ctx, result.CampaignID, oldTriggerKeyword, "EXACT", true); err != nil {
		t.Fatalf("add the old trigger: %v", err)
	}
	if got := len(activeTriggers(airanTriggers(t, result.CampaignID))); got != 2 {
		t.Fatalf("%d active triggers before the migration, want 2", got)
	}

	migration0003(t)

	all := airanTriggers(t, result.CampaignID)
	active := activeTriggers(all)
	if len(active) != 1 {
		t.Fatalf("%d active triggers after the migration, want exactly 1", len(active))
	}
	if active[0].Normalized != migrationNormalized {
		t.Errorf("the surviving trigger is %q, want the new phrase", active[0].Normalized)
	}
	// Retired, not deleted: enrolments reference it.
	if len(all) != 2 {
		t.Errorf("%d trigger rows, want 2 — the old one is deactivated, not deleted", len(all))
	}
	if _, err := f.campaignSvc.MatchTrigger(ctx, nil, oldTriggerKeyword); err != nil {
		t.Fatalf("MatchTrigger: %v", err)
	}
}

// TestMigrationWithoutAiranCampaignIsHarmless is scenario 4.
//
// It is deliberately a no-op rather than an error: migrations run before the
// default campaign is installed, so on every fresh database this legitimately
// finds nothing. Failing there would stop the service from starting at all.
func TestMigrationWithoutAiranCampaignIsHarmless(t *testing.T) {
	f := newFixture(t)

	other := f.createCampaign(t, "Басқа науқан", time.Now().UTC().Add(6*time.Hour), []int{-3600})
	f.addTrigger(t, other.ID, "БАСҚА")

	before := countRows(t, "campaign_triggers")

	migration0003(t) // must not panic, error, or touch anything

	if got := countRows(t, "campaign_triggers"); got != before {
		t.Errorf("trigger count changed from %d to %d with no Airan campaign present",
			before, got)
	}
	if got := len(activeTriggers(airanTriggers(t, other.ID))); got != 1 {
		t.Errorf("the unrelated campaign's trigger was disturbed (%d active)", got)
	}
}

// TestMigrationPreservesEverythingElse is the blast-radius check: the migration
// touches campaign_triggers and nothing else.
func TestMigrationPreservesEverythingElse(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	result := seedWithOldTrigger(t, f)

	// A contact goes through the funnel first, so there is real history to
	// preserve: an enrolment, and a queue of scheduled messages.
	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, oldTriggerKeyword)
	if match == nil {
		t.Fatal("the old trigger does not match before the migration")
	}
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	campaignBefore, err := f.campaignRepo.GetFull(ctx, result.CampaignID)
	if err != nil {
		t.Fatalf("GetFull: %v", err)
	}
	enrollmentBefore := enrollmentOf(t, f, result.CampaignID, contact.ID)
	jobsBefore := allJobs(t, f, enrollmentBefore.ID)

	counts := func() map[string]int {
		return map[string]int{
			"campaigns":                 countRows(t, "campaigns"),
			"campaign_steps":            countRows(t, "campaign_steps"),
			"message_templates":         countRows(t, "message_templates"),
			"message_template_versions": countRows(t, "message_template_versions"),
			"scheduled_messages":        countRows(t, "scheduled_messages"),
			"campaign_contacts":         countRows(t, "campaign_contacts"),
			"contacts":                  countRows(t, "contacts"),
		}
	}
	before := counts()

	migration0003(t)

	for table, want := range before {
		if got := counts()[table]; got != want {
			t.Errorf("%s changed from %d to %d rows", table, want, got)
		}
	}

	campaignAfter, err := f.campaignRepo.GetFull(ctx, result.CampaignID)
	if err != nil {
		t.Fatalf("GetFull: %v", err)
	}
	switch {
	case campaignAfter.ID != campaignBefore.ID:
		t.Error("the campaign id changed")
	case campaignAfter.Name != campaignBefore.Name:
		t.Error("the campaign name changed")
	case campaignAfter.Status != campaignBefore.Status:
		t.Error("the campaign status changed")
	case campaignAfter.WebinarLink != campaignBefore.WebinarLink:
		t.Errorf("the webinar link changed to %q", campaignAfter.WebinarLink)
	case !campaignAfter.EventStartAt.Equal(*campaignBefore.EventStartAt):
		t.Error("the event time changed")
	case len(campaignAfter.Steps) != len(campaignBefore.Steps):
		t.Error("the step count changed")
	}

	for i, step := range campaignAfter.Steps {
		if step.OffsetSeconds != campaignBefore.Steps[i].OffsetSeconds ||
			step.TemplateID != campaignBefore.Steps[i].TemplateID {
			t.Errorf("step %d was modified", i+1)
		}
	}

	// The enrolment still points at its trigger, and its queue is intact.
	enrollmentAfter := enrollmentOf(t, f, result.CampaignID, contact.ID)
	if enrollmentAfter.ID != enrollmentBefore.ID {
		t.Error("the enrolment was replaced")
	}
	if len(allJobs(t, f, enrollmentAfter.ID)) != len(jobsBefore) {
		t.Error("the scheduled messages changed")
	}

	var orphaned int
	if err := testDB.QueryRow(ctx, `
		SELECT count(*) FROM campaign_contacts cc
		WHERE cc.trigger_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM campaign_triggers t WHERE t.id = cc.trigger_id)`).
		Scan(&orphaned); err != nil {
		t.Fatalf("check trigger references: %v", err)
	}
	if orphaned != 0 {
		t.Errorf("%d enrolments now point at a trigger that no longer exists", orphaned)
	}
}
