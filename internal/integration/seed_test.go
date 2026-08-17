//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/media"
	"github.com/ayran/whatsapp-automation/internal/seed"
	"github.com/ayran/whatsapp-automation/internal/templates"
)

// seedDeps builds the seeder against the shared test database.
func seedDeps(t *testing.T, f *fixture) seed.Deps {
	t.Helper()

	store, err := media.NewStore(t.TempDir(), 32, "")
	if err != nil {
		t.Fatalf("media store: %v", err)
	}
	log := discardLogger()
	mediaSvc := media.NewService(store, media.NewRepository(testDB),
		media.NewTranscoder("", "", ""), log)

	return seed.Deps{
		Campaigns: f.campaignSvc,
		Templates: templates.NewService(f.templateRepo, mediaSvc),
		Log:       log,
	}
}

func countRows(t *testing.T, table string) int {
	t.Helper()

	var n int
	if err := testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestSeedInstallsAiranOnEmptyDatabase is the primary case: a fresh
// installation gets the whole campaign.
func TestSeedInstallsAiranOnEmptyDatabase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	deps := seedDeps(t, f)

	result, err := seed.EnsureDefaultCampaign(ctx, deps, seed.Options{})
	if err != nil {
		t.Fatalf("EnsureDefaultCampaign: %v", err)
	}
	if result.Skipped {
		t.Fatalf("seeding was skipped on an empty database: %s", result.SkipReason)
	}

	if result.CampaignName != "Airan" {
		t.Errorf("campaign name = %q, want Airan", result.CampaignName)
	}
	if got := countRows(t, "campaigns"); got != 1 {
		t.Errorf("%d campaigns, want 1", got)
	}
	if got := countRows(t, "campaign_triggers"); got != 1 {
		t.Errorf("%d triggers, want 1", got)
	}
	if got := countRows(t, "campaign_steps"); got != 9 {
		t.Errorf("%d steps, want 9", got)
	}
	if got := countRows(t, "message_templates"); got != 9 {
		t.Errorf("%d templates, want 9", got)
	}
	// Every template must carry a version row, or a pinned campaign could not
	// resolve what it sent.
	if got := countRows(t, "message_template_versions"); got != 9 {
		t.Errorf("%d template versions, want 9", got)
	}

	campaign, err := f.campaignRepo.GetFull(ctx, result.CampaignID)
	if err != nil {
		t.Fatalf("GetFull: %v", err)
	}

	// A seeded campaign must not send on its own: the media steps are still
	// text placeholders until an operator uploads the real assets.
	if campaign.Status != domain.CampaignDraft {
		t.Errorf("campaign status = %s, want DRAFT", campaign.Status)
	}
	if campaign.WebinarLink != seed.WebinarLink {
		t.Errorf("webinar link = %q, want %q", campaign.WebinarLink, seed.WebinarLink)
	}
	if campaign.Timezone != "Asia/Almaty" {
		t.Errorf("timezone = %q, want Asia/Almaty", campaign.Timezone)
	}
	if !campaign.CatchUpMissedSteps {
		t.Error("catch-up must be on so a late lead still gets context")
	}

	// The event must land on 21:00 local time, whichever day was chosen.
	if campaign.EventStartAt == nil {
		t.Fatal("campaign has no event start")
	}
	loc, _ := time.LoadLocation("Asia/Almaty")
	local := campaign.EventStartAt.In(loc)
	if local.Hour() != 21 || local.Minute() != 0 {
		t.Errorf("event starts at %s local, want 21:00", local.Format("15:04"))
	}
	if campaign.EventStartAt.Before(time.Now().UTC()) {
		t.Error("the seeded webinar is in the past")
	}

	// The offsets from the brief, in send order.
	wantOffsets := []int{0, -18000, -10800, -7200, -3600, -1800, -900, -300, 0}
	if len(campaign.Steps) != len(wantOffsets) {
		t.Fatalf("%d steps, want %d", len(campaign.Steps), len(wantOffsets))
	}
	for i, step := range campaign.Steps {
		if step.OffsetSeconds != wantOffsets[i] {
			t.Errorf("step %d offset = %d, want %d", i+1, step.OffsetSeconds, wantOffsets[i])
		}
		if !step.Enabled {
			t.Errorf("step %d is disabled", i+1)
		}
	}

	// Only the greeting is anchored to the contact; everything else to the
	// event, so all contacts share one wall-clock schedule.
	if campaign.Steps[0].ScheduleKind != domain.ScheduleOnTrigger {
		t.Errorf("step 1 kind = %s, want ON_TRIGGER", campaign.Steps[0].ScheduleKind)
	}
	for i, step := range campaign.Steps[1:] {
		if step.ScheduleKind != domain.ScheduleRelativeToEvent {
			t.Errorf("step %d kind = %s, want RELATIVE_TO_EVENT", i+2, step.ScheduleKind)
		}
	}
}

// TestSeedIsIdempotent is the requirement that the migration may run any number
// of times. The server calls it on every boot.
func TestSeedIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	deps := seedDeps(t, f)

	if _, err := seed.EnsureDefaultCampaign(ctx, deps, seed.Options{}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	before := map[string]int{
		"campaigns":                 countRows(t, "campaigns"),
		"campaign_triggers":         countRows(t, "campaign_triggers"),
		"campaign_steps":            countRows(t, "campaign_steps"),
		"message_templates":         countRows(t, "message_templates"),
		"message_template_versions": countRows(t, "message_template_versions"),
	}

	for i := 0; i < 5; i++ {
		result, err := seed.EnsureDefaultCampaign(ctx, deps, seed.Options{})
		if err != nil {
			t.Fatalf("run %d: %v", i+2, err)
		}
		if !result.Skipped {
			t.Errorf("run %d installed the campaign again instead of skipping", i+2)
		}
	}

	for table, want := range before {
		if got := countRows(t, table); got != want {
			t.Errorf("%s went from %d to %d rows across repeated runs", table, want, got)
		}
	}
}

// TestSeedLeavesExistingDatabaseAlone is the guarantee the operator asked for:
// a database with campaigns in it is not touched at all.
func TestSeedLeavesExistingDatabaseAlone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// An operator's own campaign, nothing to do with Airan.
	existing := f.createCampaign(t, "Менің науқаным", time.Now().UTC().Add(6*time.Hour),
		[]int{-3600, 0})

	before := map[string]int{
		"campaigns":         countRows(t, "campaigns"),
		"campaign_steps":    countRows(t, "campaign_steps"),
		"message_templates": countRows(t, "message_templates"),
		"campaign_triggers": countRows(t, "campaign_triggers"),
	}

	result, err := seed.EnsureDefaultCampaign(ctx, seedDeps(t, f), seed.Options{})
	if err != nil {
		t.Fatalf("EnsureDefaultCampaign: %v", err)
	}
	if !result.Skipped {
		t.Fatal("seeding ran against a database that already had a campaign")
	}

	for table, want := range before {
		if got := countRows(t, table); got != want {
			t.Errorf("%s changed from %d to %d rows; a non-empty database must not be touched",
				table, want, got)
		}
	}

	// And the operator's campaign is untouched.
	reloaded, err := f.campaignRepo.GetFull(ctx, existing.ID)
	if err != nil {
		t.Fatalf("GetFull: %v", err)
	}
	if reloaded.Name != "Менің науқаным" || len(reloaded.Steps) != 2 {
		t.Errorf("the existing campaign was modified: %q with %d steps",
			reloaded.Name, len(reloaded.Steps))
	}
}

// TestSeededTriggerEnrollsAndSchedulesEverything walks the funnel the way a real
// lead does, and checks the whole schedule is persisted at once rather than one
// step at a time.
func TestSeededTriggerEnrollsAndSchedulesEverything(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	result, err := seed.EnsureDefaultCampaign(ctx, seedDeps(t, f), seed.Options{
		// Activate so the trigger is live: MatchTrigger only considers ACTIVE
		// campaigns, which is what stops a draft funnel from firing.
		Activate: true,
	})
	if err != nil {
		t.Fatalf("EnsureDefaultCampaign: %v", err)
	}

	// Case-folded matching: the lead does not have to shout.
	for _, text := range []string{"АЙРАН", "айран", "Айран", "  Айран  "} {
		match, err := f.campaignSvc.MatchTrigger(ctx, nil, text)
		if err != nil {
			t.Fatalf("MatchTrigger(%q): %v", text, err)
		}
		if match == nil {
			t.Errorf("%q did not match the trigger", text)
		}
	}

	// An unrelated message must not enrol anyone. EXACT means exactly this word.
	for _, text := range []string{"айран ішкім келеді", "сәлем", "айрандар"} {
		match, err := f.campaignSvc.MatchTrigger(ctx, nil, text)
		if err != nil {
			t.Fatalf("MatchTrigger(%q): %v", text, err)
		}
		if match != nil {
			t.Errorf("%q matched the trigger but should not have", text)
		}
	}

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "АЙРАН")
	enroll, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC())
	if err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	if enroll.Action != campaignsActionEnrolled {
		t.Fatalf("action = %v, want ENROLLED", enroll.Action)
	}

	enrollment := enrollmentOf(t, f, result.CampaignID, contact.ID)

	// Every one of the nine steps must be accounted for immediately, not
	// scheduled one at a time as the funnel advances.
	rows := allJobs(t, f, enrollment.ID)
	if len(rows) != 9 {
		t.Errorf("%d job rows after enrolment, want 9 — the whole schedule must "+
			"be persisted up front", len(rows))
	}

	// A second trigger from the same contact changes nothing.
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("second HandleTrigger: %v", err)
	}
	if got := len(allJobs(t, f, enrollment.ID)); got != 9 {
		t.Errorf("%d job rows after a repeat trigger, want 9", got)
	}
	if got := countRows(t, "campaign_contacts"); got != 1 {
		t.Errorf("%d enrolments after a repeat trigger, want 1", got)
	}
}

// TestSeededCampaignSkipsPastStepsButKeepsFutureOnes is the late-arrival rule
// from the brief: a lead who triggers at 20:40 must not receive the 16:00
// message, and must still receive everything that has not happened yet.
func TestSeededCampaignSkipsPastStepsButKeepsFutureOnes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	result, err := seed.EnsureDefaultCampaign(ctx, seedDeps(t, f), seed.Options{Activate: true})
	if err != nil {
		t.Fatalf("EnsureDefaultCampaign: %v", err)
	}

	// Move the webinar to twenty minutes away, which puts the lead in the same
	// position as one arriving at 20:40 for a 21:00 start: the -5h, -3h, -2h,
	// -1h and -30m steps are gone; -15m, -5m and the start are still ahead.
	eventStart := time.Now().UTC().Add(20 * time.Minute)
	if _, err := testDB.Exec(ctx,
		`UPDATE campaigns SET event_start_at = $2 WHERE id = $1`,
		result.CampaignID, eventStart); err != nil {
		t.Fatalf("move event: %v", err)
	}

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "АЙРАН")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	enrollment := enrollmentOf(t, f, result.CampaignID, contact.ID)

	steps, err := f.campaignRepo.ListSteps(ctx, nil, result.CampaignID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}

	byStep := map[string]domain.CampaignStep{}
	for _, step := range steps {
		byStep[step.ID.String()] = step
	}

	var futureLive, pastLive, greetingLive int
	for _, job := range allJobs(t, f, enrollment.ID) {
		step := byStep[job.StepID.String()]
		offset := step.OffsetSeconds
		live := job.Status == domain.JobPending

		// The greeting is anchored to the contact, not the event, so it is
		// always due shortly after they write in and has no bearing on which
		// event-anchored steps are still ahead.
		if step.ScheduleKind == domain.ScheduleOnTrigger {
			if live {
				greetingLive++
			}
			continue
		}

		// -900, -300 and 0 are still ahead of a webinar twenty minutes away.
		if offset >= -900 {
			if live {
				futureLive++
			} else {
				t.Errorf("step at offset %ds is still in the future but its job is %s",
					offset, job.Status)
			}
			continue
		}
		if live {
			pastLive++
		}
	}

	if futureLive != 3 {
		t.Errorf("%d future steps queued, want 3 (-15m, -5m, start)", futureLive)
	}
	if greetingLive != 1 {
		t.Errorf("%d greeting jobs queued, want 1 — a lead must always be answered", greetingLive)
	}
	// Catch-up keeps at most one overdue message for context; the rest of the
	// past must not be replayed.
	if pastLive > 1 {
		t.Errorf("%d past steps queued, want at most 1 (the catch-up message)", pastLive)
	}

	// And nothing was lost: every step has a row either way.
	if got := len(allJobs(t, f, enrollment.ID)); got != len(steps) {
		t.Errorf("%d job rows for %d steps", got, len(steps))
	}
}
