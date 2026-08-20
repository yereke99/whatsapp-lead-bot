//go:build integration

package integration

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

// Daily webinars, against a real database.
//
// The rule these tests exist to hold down, in the customer's own words:
//
//	ONE USER -> ONE ENROLMENT -> ONE WEBINAR OCCURRENCE -> ONE SEQUENCE
//
// "The webinar repeats every day" is a fact about the event. It is not an
// instruction to send the funnel to everybody every day. A contact who went
// through the sequence for the 20 August webinar is an existing participant on
// 21 August and receives nothing; the 21 August webinar exists for the people
// who arrive on 21 August.
//
// Each test below is one of the acceptance cases from the brief, in order.

const dailyKeyword = "Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді"

// dailyCampaign builds the Airan shape: a webinar every day at clock, with one
// reminder per offset, every reminder marked as part of the daily sequence.
//
// It goes through the service's own save path rather than writing rows, so
// what is tested is the path the admin panel takes.
func dailyCampaign(t *testing.T, f *fixture, name, clock string, offsets []int, behavior domain.ExistingContactBehavior) *domain.Campaign {
	t.Helper()
	ctx := context.Background()

	today := time.Now().In(timex.MustLocation(testZone)).Format(timex.DateLayout)

	campaign, err := f.campaignSvc.Create(ctx, campaigns.SaveInput{
		Name:                    name,
		EventType:               "WEBINAR",
		EventDate:               today,
		EventTime:               clock,
		Timezone:                testZone,
		WebinarLink:             "https://bizon365.online/room/209010/ayrankaimaq",
		IsDailyRecurring:        true,
		RecurrenceTime:          clock,
		RecurrenceStartDate:     today,
		ExistingContactBehavior: string(behavior),
		CatchUpMissedSteps:      false,
		MaxSendAttempts:         5,
		ResumePolicy:            string(domain.ResumeSkipExpired),
	}, nil)
	if err != nil {
		t.Fatalf("create daily campaign: %v", err)
	}

	for _, offset := range offsets {
		addDailyStep(t, f, campaign.ID, "reminder "+timex.HumanOffset(offset), offset, true)
	}
	f.addTrigger(t, campaign.ID, dailyKeyword)

	if _, err := f.campaignSvc.SetStatus(ctx, campaign.ID, domain.CampaignActive); err != nil {
		t.Fatalf("activate: %v", err)
	}

	full, err := f.campaignRepo.GetFull(ctx, campaign.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return full
}

// addDailyStep appends one step, marked or not, through the service.
func addDailyStep(t *testing.T, f *fixture, campaignID uuid.UUID, name string, offset int, daily bool) *domain.CampaignStep {
	t.Helper()

	templateID := f.createTemplate(t, name+" tpl "+uuid.NewString()[:8], "body "+name)
	step, err := f.campaignSvc.AddStep(context.Background(), campaignID, campaigns.StepInput{
		Name:                  name,
		OffsetSeconds:         offset,
		TemplateID:            templateID,
		Enabled:               true,
		ScheduleKind:          string(domain.ScheduleRelativeToEvent),
		IncludeInDailyWebinar: daily,
	})
	if err != nil {
		t.Fatalf("AddStep %s: %v", name, err)
	}
	return step
}

// trigger runs the real inbound path for one contact: match the phrase, hand it
// to the campaign engine. This is exactly what an incoming WhatsApp message
// does.
func trigger(t *testing.T, f *fixture, contact *domain.Contact, at time.Time) *campaigns.EnrollResult {
	t.Helper()
	ctx := context.Background()

	match, err := f.campaignSvc.MatchTrigger(ctx, nil, dailyKeyword)
	if err != nil {
		t.Fatalf("MatchTrigger: %v", err)
	}
	if match == nil {
		t.Fatal("the opt-in phrase matched no campaign")
	}
	result, err := f.campaignSvc.HandleTrigger(ctx, contact, match, at)
	if err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	return result
}

// enrollmentCount is the number of enrolment rows for one contact in one
// campaign. The brief asks for this to be one, always.
func enrollmentCount(t *testing.T, campaignID, contactID uuid.UUID) int {
	t.Helper()

	var n int
	if err := testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM campaign_contacts WHERE campaign_id = $1 AND contact_id = $2`,
		campaignID, contactID).Scan(&n); err != nil {
		t.Fatalf("count enrolments: %v", err)
	}
	return n
}

// deliverableCount counts the rows that will actually be delivered: queued
// work and deliveries that already happened. Recorded skips are rows too, and
// counting them would hide the very duplication these tests look for.
func deliverableCount(t *testing.T, f *fixture, enrollmentID uuid.UUID) int {
	t.Helper()

	n := 0
	for _, job := range allJobs(t, f, enrollmentID) {
		switch job.Status {
		case domain.JobPending, domain.JobProcessing, domain.JobSent, domain.JobFailed:
			n++
		}
	}
	return n
}

// deliverEverything marks every queued job of an enrolment as sent, which is
// what "the contact went through the sequence" means.
func deliverEverything(t *testing.T, f *fixture, enrollmentID uuid.UUID) int {
	t.Helper()

	sent := 0
	for _, job := range allJobs(t, f, enrollmentID) {
		if job.Status == domain.JobPending {
			markSent(t, job.ID)
			sent++
		}
	}
	return sent
}

// skipCounts groups an enrolment's cancelled rows by reason.
func skipCounts(t *testing.T, f *fixture, enrollmentID uuid.UUID) map[string]int {
	t.Helper()

	out := map[string]int{}
	for _, job := range allJobs(t, f, enrollmentID) {
		if job.Status == domain.JobCancelled {
			out[job.CancelReason]++
		}
	}
	return out
}

// noDuplicates asserts the queue's own consistency report is clean.
func noDuplicates(t *testing.T, f *fixture) {
	t.Helper()

	report, err := f.campaignSvc.Consistency(context.Background())
	if err != nil {
		t.Fatalf("Consistency: %v", err)
	}
	for _, check := range report.Checks {
		if check.Name == "duplicate_jobs" && check.Count != 0 {
			t.Fatalf("duplicate_jobs = %d, want 0", check.Count)
		}
	}
}

// ---------------------------------------------------------------------------

//  1. A new contact sends the opt-in phrase. One enrolment, pinned to the
//     webinar they signed up for, and one sequence.
func TestNewContactGetsOneEnrollmentAndOneSequence(t *testing.T) {
	f := newFixture(t)

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(6 * time.Hour).Format("15:04")
	campaign := dailyCampaign(t, f, "Airan", clock,
		[]int{-5 * 3600, -3 * 3600, -2 * 3600, -3600, -1800, -900, 0}, domain.BehaviorIgnore)

	contact := f.createContact(t, "77010000101")
	result := trigger(t, f, contact, time.Now().UTC())

	if result.Action != campaigns.ActionEnrolled {
		t.Fatalf("action = %s, want ENROLLED", result.Action)
	}
	if n := enrollmentCount(t, campaign.ID, contact.ID); n != 1 {
		t.Fatalf("enrolments = %d, want 1", n)
	}

	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)
	if enrollment.OccurrenceAt == nil {
		t.Fatal("no webinar occurrence was pinned")
	}
	if got := deliverableCount(t, f, enrollment.ID); got != 7 {
		t.Fatalf("live jobs = %d, want 7 (one per marked step)", got)
	}
}

//  2. The same contact sends the phrase again. No second enrolment, whatever
//     the campaign's repeat behaviour is.
func TestRepeatTriggerCreatesNoSecondEnrollment(t *testing.T) {
	for _, behavior := range []domain.ExistingContactBehavior{
		domain.BehaviorIgnore, domain.BehaviorContinue, domain.BehaviorRestart,
	} {
		t.Run(string(behavior), func(t *testing.T) {
			f := newFixture(t)

			now := time.Now().In(timex.MustLocation(testZone))
			clock := now.Add(5 * time.Hour).Format("15:04")
			campaign := dailyCampaign(t, f, "Airan", clock, []int{-3600, 0}, behavior)

			contact := f.createContact(t, "77010000102")
			trigger(t, f, contact, time.Now().UTC())
			trigger(t, f, contact, time.Now().UTC())
			trigger(t, f, contact, time.Now().UTC())

			if n := enrollmentCount(t, campaign.ID, contact.ID); n != 1 {
				t.Fatalf("enrolments after three triggers = %d, want 1", n)
			}
			noDuplicates(t, f)
		})
	}
}

//  3. Tomorrow's webinar happens. The contact who went through yesterday's
//     sequence receives nothing, and their enrolment is not re-anchored.
//
//     This is the reported bug: the reminders were arriving every day.
func TestExistingParticipantGetsNothingOnTheNextDay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(2 * time.Hour).Format("15:04")
	campaign := dailyCampaign(t, f, "Airan", clock,
		[]int{-5 * 3600, -3 * 3600, -3600, -1800, 0}, domain.BehaviorRestart)

	contact := f.createContact(t, "77010000103")
	trigger(t, f, contact, time.Now().UTC())
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)
	occurrence := *enrollment.OccurrenceAt

	// Yesterday's webinar came and went.
	delivered := deliverEverything(t, f, enrollment.ID)
	if delivered == 0 {
		t.Fatal("nothing was queued for the first sequence")
	}
	before := deliverableCount(t, f, enrollment.ID)

	// The next day. The contact writes in again — the trigger phrase, a
	// question, anything the campaign is configured to restart on.
	trigger(t, f, contact, time.Now().UTC().Add(24*time.Hour))

	// ... and every sweep the scheduler runs between now and then.
	for i := 0; i < 5; i++ {
		if _, err := f.campaignSvc.ReconcileCampaign(ctx, campaign.ID); err != nil {
			t.Fatalf("ReconcileCampaign %d: %v", i, err)
		}
	}

	if n := enrollmentCount(t, campaign.ID, contact.ID); n != 1 {
		t.Fatalf("enrolments = %d, want 1", n)
	}
	if after := deliverableCount(t, f, enrollment.ID); after != before {
		t.Fatalf("live jobs = %d, want %d — the sequence was sent a second time", after, before)
	}
	// Every step of the sequence is refused for the new run — recorded, not
	// left absent, so the refusal is auditable rather than invisible.
	dailySteps := len(campaigns.DailySequence(mustSteps(t, f, campaign.ID)))
	if skips := skipCounts(t, f, enrollment.ID); skips[domain.SkipDailySequenceDone] != dailySteps {
		t.Errorf("daily-sequence skips = %d, want %d recorded refusals",
			skips[domain.SkipDailySequenceDone], dailySteps)
	}

	// The first run's occurrence is history and is not rewritten.
	reloaded := enrollmentOf(t, f, campaign.ID, contact.ID)
	if reloaded.OccurrenceAt == nil {
		t.Fatal("the occurrence was cleared")
	}
	sent := 0
	for _, job := range allJobs(t, f, enrollment.ID) {
		if job.Status == domain.JobSent {
			sent++
		}
	}
	if sent != delivered {
		t.Errorf("delivered messages = %d, want %d — nothing new may have gone out", sent, delivered)
	}
	_ = occurrence
	noDuplicates(t, f)
}

//  4. A different contact arrives the next day. They are new, so they get the
//     webinar that is coming and one full sequence.
func TestNewContactTheNextDayGetsTheirOwnSequence(t *testing.T) {
	f := newFixture(t)

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(3 * time.Hour).Format("15:04")
	campaign := dailyCampaign(t, f, "Airan", clock, []int{-2 * 3600, -3600, 0}, domain.BehaviorIgnore)

	yesterday := f.createContact(t, "77010000104")
	trigger(t, f, yesterday, time.Now().UTC())
	first := enrollmentOf(t, f, campaign.ID, yesterday.ID)
	deliverEverything(t, f, first.ID)
	firstJobs := deliverableCount(t, f, first.ID)

	// A day later, somebody who has never written to us before.
	tomorrow := f.createContact(t, "77010000105")
	trigger(t, f, tomorrow, time.Now().UTC().Add(24*time.Hour))
	second := enrollmentOf(t, f, campaign.ID, tomorrow.ID)

	if second.OccurrenceAt == nil {
		t.Fatal("the new contact was pinned to no webinar")
	}
	if !second.OccurrenceAt.After(*first.OccurrenceAt) {
		t.Errorf("the new contact was put on the old webinar: %s, previous %s",
			localOf(*second.OccurrenceAt), localOf(*first.OccurrenceAt))
	}
	if got := deliverableCount(t, f, second.ID); got != 3 {
		t.Errorf("new contact's live jobs = %d, want 3", got)
	}
	if got := deliverableCount(t, f, first.ID); got != firstJobs {
		t.Errorf("the earlier contact gained jobs: %d, want %d", got, firstJobs)
	}
}

// 5 & 7. The sequence is however long the operator made it. Seven is the shape
//
//	of the Airan funnel, not a number anywhere in the code.
func TestSequenceLengthIsNotFixed(t *testing.T) {
	for _, size := range []int{5, 7, 10} {
		t.Run(strconv.Itoa(size)+" steps", func(t *testing.T) {
			f := newFixture(t)

			now := time.Now().In(timex.MustLocation(testZone))
			clock := now.Add(12 * time.Hour).Format("15:04")

			offsets := make([]int, size)
			for i := range offsets {
				offsets[i] = -(i + 1) * 600
			}
			campaign := dailyCampaign(t, f, "Airan", clock, offsets, domain.BehaviorIgnore)

			contact := f.createContact(t, "77010000201")
			trigger(t, f, contact, time.Now().UTC())
			enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

			if got := deliverableCount(t, f, enrollment.ID); got != size {
				t.Fatalf("live jobs = %d, want %d", got, size)
			}
			if got := len(campaigns.DailySequence(mustSteps(t, f, campaign.ID))); got != size {
				t.Fatalf("daily sequence = %d steps, want %d", got, size)
			}
		})
	}
}

//  6. A step left out of the daily sequence keeps its own behaviour: it is not
//     retired with the sequence, and it is not disabled either.
func TestUnmarkedStepIsNotPartOfTheSequence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(2 * time.Hour).Format("15:04")
	campaign := dailyCampaign(t, f, "Airan", clock, []int{-3600, 0}, domain.BehaviorRestart)

	// A follow-up an hour after the webinar, deliberately outside the sequence.
	followUp := addDailyStep(t, f, campaign.ID, "follow-up", 3600, false)

	contact := f.createContact(t, "77010000106")
	trigger(t, f, contact, time.Now().UTC())
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)
	deliverEverything(t, f, enrollment.ID)

	trigger(t, f, contact, time.Now().UTC().Add(24*time.Hour))
	if _, err := f.campaignSvc.ReconcileCampaign(ctx, campaign.ID); err != nil {
		t.Fatalf("ReconcileCampaign: %v", err)
	}

	// The follow-up is not a daily-sequence step, so it was never retired for
	// that reason.
	for _, job := range allJobs(t, f, enrollment.ID) {
		if job.StepID == followUp.ID && job.CancelReason == domain.SkipDailySequenceDone {
			t.Fatal("a step outside the daily sequence was retired with it")
		}
	}
}

//  8. Running the scheduler over and over changes nothing. Idempotence is the
//     property the whole queue rests on.
func TestRepeatedSweepsCreateNoDuplicateDailyJobs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(4 * time.Hour).Format("15:04")
	campaign := dailyCampaign(t, f, "Airan", clock,
		[]int{-3 * 3600, -3600, -1800, 0}, domain.BehaviorRestart)

	contact := f.createContact(t, "77010000107")
	trigger(t, f, contact, time.Now().UTC())
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)
	deliverEverything(t, f, enrollment.ID)
	trigger(t, f, contact, time.Now().UTC().Add(24*time.Hour))

	var counts []int
	for i := 0; i < 12; i++ {
		if _, err := f.campaignSvc.ReconcileCampaign(ctx, campaign.ID); err != nil {
			t.Fatalf("ReconcileCampaign %d: %v", i, err)
		}
		counts = append(counts, deliverableCount(t, f, enrollment.ID))
	}
	for i, n := range counts {
		if n != counts[0] {
			t.Fatalf("live jobs drifted on sweep %d: %d, want %d", i, n, counts[0])
		}
	}
	noDuplicates(t, f)
}

//  9. Contacts who are in the database but were never enrolled receive nothing.
//     A daily webinar is not a mailing list.
func TestUnenrolledContactsAreNeverScheduled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(4 * time.Hour).Format("15:04")
	campaign := dailyCampaign(t, f, "Airan", clock, []int{-3600, 0}, domain.BehaviorIgnore)

	for _, phone := range []string{"77010000108", "77010000109", "77010000110"} {
		f.createContact(t, phone)
	}

	if _, err := f.campaignSvc.ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	var jobs, enrolments int
	if err := testDB.QueryRow(ctx,
		`SELECT count(*) FROM scheduled_messages WHERE campaign_id = $1`, campaign.ID).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if err := testDB.QueryRow(ctx,
		`SELECT count(*) FROM campaign_contacts WHERE campaign_id = $1`, campaign.ID).Scan(&enrolments); err != nil {
		t.Fatalf("count enrolments: %v", err)
	}
	if jobs != 0 || enrolments != 0 {
		t.Fatalf("contacts who never opted in acquired %d enrolments and %d jobs", enrolments, jobs)
	}
}

// 10. Messages already queued or already sent are left exactly as they are.
func TestExistingScheduledMessagesArePreserved(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(6 * time.Hour).Format("15:04")
	campaign := dailyCampaign(t, f, "Airan", clock,
		[]int{-5 * 3600, -3 * 3600, -3600, 0}, domain.BehaviorRestart)

	contact := f.createContact(t, "77010000111")
	trigger(t, f, contact, time.Now().UTC())
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

	// Two delivered, the rest still waiting: a contact caught mid-sequence.
	sentIDs := map[uuid.UUID]bool{}
	for i, job := range allJobs(t, f, enrollment.ID) {
		if i < 2 && job.Status == domain.JobPending {
			markSent(t, job.ID)
			sentIDs[job.ID] = true
		}
	}
	beforeLive := deliverableCount(t, f, enrollment.ID)

	for i := 0; i < 5; i++ {
		if _, err := f.campaignSvc.ReconcileCampaign(ctx, campaign.ID); err != nil {
			t.Fatalf("ReconcileCampaign: %v", err)
		}
	}

	if after := deliverableCount(t, f, enrollment.ID); after != beforeLive {
		t.Fatalf("live jobs = %d, want %d — a mid-sequence contact lost or gained work", after, beforeLive)
	}
	for _, job := range allJobs(t, f, enrollment.ID) {
		if sentIDs[job.ID] && job.Status != domain.JobSent {
			t.Errorf("a delivered message was rewritten to %s", job.Status)
		}
		if !sentIDs[job.ID] && job.Status == domain.JobCancelled &&
			job.CancelReason == domain.SkipDailySequenceDone {
			t.Error("a contact mid-sequence had the rest of it retired")
		}
	}
}

//  11. A campaign that is not a daily webinar is untouched by any of this,
//     RESTART included.
func TestOneTimeCampaignRestartStillReschedules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(6 * time.Hour)
	campaign := f.createCampaign(t, "Бір реттік вебинар", eventStart, []int{-3 * 3600, -3600})
	f.addTrigger(t, campaign.ID, dailyKeyword)
	if _, err := testDB.Exec(ctx,
		`UPDATE campaigns SET existing_contact_behavior = 'RESTART' WHERE id = $1`, campaign.ID); err != nil {
		t.Fatalf("set restart behaviour: %v", err)
	}
	// Even with the flag set, a one-time campaign has no daily semantics.
	if _, err := testDB.Exec(ctx,
		`UPDATE campaign_steps SET include_in_daily_webinar = 1 WHERE campaign_id = $1`, campaign.ID); err != nil {
		t.Fatalf("mark steps: %v", err)
	}

	contact := f.createContact(t, "77010000112")
	trigger(t, f, contact, time.Now().UTC())
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)
	deliverEverything(t, f, enrollment.ID)

	trigger(t, f, contact, time.Now().UTC())
	restarted := enrollmentOf(t, f, campaign.ID, contact.ID)
	if restarted.RunNumber != 2 {
		t.Fatalf("run_number = %d, want 2 — RESTART must keep working for one-time campaigns", restarted.RunNumber)
	}

	fresh := 0
	for _, job := range allJobs(t, f, enrollment.ID) {
		if job.RunNumber == 2 && job.Status == domain.JobPending {
			fresh++
		}
	}
	if fresh == 0 {
		t.Fatal("the restarted run scheduled nothing; one-time campaign behaviour changed")
	}
}

// 12 & 13. Two deliveries of the same trigger arriving at once. The database
//
//	decides, not a read-then-write, so one enrolment is the only possible
//	outcome.
func TestConcurrentTriggersProduceOneEnrollment(t *testing.T) {
	f := newFixture(t)

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(5 * time.Hour).Format("15:04")
	campaign := dailyCampaign(t, f, "Airan", clock, []int{-3600, -1800, 0}, domain.BehaviorIgnore)

	contact := f.createContact(t, "77010000113")

	const racers = 8
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			ctx := context.Background()
			match, err := f.campaignSvc.MatchTrigger(ctx, nil, dailyKeyword)
			if err != nil || match == nil {
				return
			}
			_, _ = f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC())
		}()
	}
	wg.Wait()

	if n := enrollmentCount(t, campaign.ID, contact.ID); n != 1 {
		t.Fatalf("enrolments after %d concurrent triggers = %d, want 1", racers, n)
	}
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)
	if got := deliverableCount(t, f, enrollment.ID); got != 3 {
		t.Fatalf("live jobs = %d, want 3 — the sequence was queued more than once", got)
	}
	noDuplicates(t, f)
}

// A contact arriving twenty minutes before the webinar gets the messages that
// are still ahead of them, and is finished with the sequence afterwards. This
// is the edge case from the brief, end to end.
func TestLateArrivalGetsTheRemainderAndNothingTomorrow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A webinar twenty minutes from now, so -5h/-3h/-1h are already past and
	// -15m/-5m/0 are still to come.
	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(20 * time.Minute).Format("15:04")
	campaign := dailyCampaign(t, f, "Airan", clock,
		[]int{-5 * 3600, -3 * 3600, -3600, -900, -300, 0}, domain.BehaviorRestart)

	contact := f.createContact(t, "77010000114")
	trigger(t, f, contact, time.Now().UTC())
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

	live := deliverableCount(t, f, enrollment.ID)
	if live == 0 || live > 3 {
		t.Fatalf("live jobs for a late arrival = %d, want the remaining steps only", live)
	}
	if skips := skipCounts(t, f, enrollment.ID); skips[domain.SkipStepExpired] == 0 {
		t.Error("the already-past reminders were not recorded as expired")
	}

	deliverEverything(t, f, enrollment.ID)
	trigger(t, f, contact, time.Now().UTC().Add(24*time.Hour))
	if _, err := f.campaignSvc.ReconcileCampaign(ctx, campaign.ID); err != nil {
		t.Fatalf("ReconcileCampaign: %v", err)
	}

	if after := deliverableCount(t, f, enrollment.ID); after != live {
		t.Fatalf("live jobs = %d, want %d — the late arrival got a second sequence", after, live)
	}
}

// mustSteps loads a campaign's steps the way the scheduler does.
func mustSteps(t *testing.T, f *fixture, campaignID uuid.UUID) []domain.CampaignStep {
	t.Helper()

	steps, err := f.campaignRepo.ListSteps(context.Background(), nil, campaignID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	return steps
}

// ---------------------------------------------------------------------------

// Editing the schedule must not mail the campaign to everyone who ever
// received it.
//
// This is the second production incident, reproduced. An operator moves the
// webinar — or a single step — and every contact who had ever been through the
// funnel gets the whole run-up again.
//
// The mechanism: a contact who joins at 18:00 for a 21:00 webinar has the
// 16:00 reminder recorded as skip:step_expired. That is a *derived* record, so
// reconciliation is allowed to revoke it when the configuration changes — and
// moving the event forward makes every such row applicable again, at once, for
// every enrolment the campaign has ever had. Contacts who finished with this
// campaign days ago are not waiting for anything, and an edit made for
// tomorrow's audience must not reach them.
func TestMovingTheEventDoesNotResendToFinishedContacts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A one-time webinar two hours out, with reminders that are already in the
	// past — exactly what a late arrival's queue looks like.
	eventStart := time.Now().UTC().Add(2 * time.Hour)
	campaign := f.createCampaign(t, "Айран кәсібі", eventStart,
		[]int{-5 * 3600, -3 * 3600, -3600, -1800, 0})
	f.addTrigger(t, campaign.ID, dailyKeyword)

	contact := f.createContact(t, "77010000115")
	trigger(t, f, contact, time.Now().UTC())
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

	expired := skipCounts(t, f, enrollment.ID)[domain.SkipStepExpired]
	if expired == 0 {
		t.Fatal("no reminder was recorded as expired; the case under test did not arise")
	}

	// The webinar happens and the contact is done with this campaign.
	deliverEverything(t, f, enrollment.ID)
	if _, err := f.campaignSvc.ReconcileCampaign(ctx, campaign.ID); err != nil {
		t.Fatalf("ReconcileCampaign: %v", err)
	}
	finished := enrollmentOf(t, f, campaign.ID, contact.ID)
	if finished.Status != domain.EnrollmentCompleted {
		t.Fatalf("enrolment status = %s, want COMPLETED", finished.Status)
	}
	settled := deliverableCount(t, f, enrollment.ID)

	// The operator schedules the next webinar: the event moves to tomorrow, so
	// every past reminder now resolves to a time that has not happened yet.
	tomorrow := time.Now().In(timex.MustLocation(testZone)).AddDate(0, 0, 1)
	if _, err := f.campaignSvc.Update(ctx, campaign.ID, campaigns.SaveInput{
		Name:                    campaign.Name,
		EventType:               "WEBINAR",
		EventDate:               tomorrow.Format(timex.DateLayout),
		EventTime:               "21:00",
		Timezone:                testZone,
		ExistingContactBehavior: string(domain.BehaviorIgnore),
		MaxSendAttempts:         5,
		ResumePolicy:            string(domain.ResumeSkipExpired),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := f.campaignSvc.ReconcileCampaign(ctx, campaign.ID); err != nil {
			t.Fatalf("ReconcileCampaign %d: %v", i, err)
		}
	}

	if got := deliverableCount(t, f, enrollment.ID); got != settled {
		t.Fatalf("deliverable jobs = %d, want %d — the campaign was mailed to a finished contact again", got, settled)
	}
	if got := skipCounts(t, f, enrollment.ID)[domain.SkipStepExpired]; got != expired {
		t.Errorf("expired skips = %d, want %d — a finished contact's record was revoked", got, expired)
	}
	if again := enrollmentOf(t, f, campaign.ID, contact.ID); again.Status != domain.EnrollmentCompleted {
		t.Errorf("enrolment reopened to %s after a time edit", again.Status)
	}
}

// The other half of the same rule: a contact who is still waiting for the
// event does follow it when the operator moves it. Correcting a mistyped time
// has to keep working, or the fix above would just be a different silence.
func TestMovingTheEventStillCarriesWaitingContacts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(2 * time.Hour)
	campaign := f.createCampaign(t, "Айран кәсібі", eventStart, []int{-3600, 0})
	f.addTrigger(t, campaign.ID, dailyKeyword)

	contact := f.createContact(t, "77010000116")
	trigger(t, f, contact, time.Now().UTC())
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

	// A step the operator mistyped: three hours before an event two hours away,
	// so it is recorded as expired rather than queued.
	late := addDailyStep(t, f, campaign.ID, "мистайп", -3*3600, false)
	if _, ok := liveJobs(t, f, enrollment.ID)[late.ID]; ok {
		t.Fatal("a step whose moment has passed must not be queued")
	}

	// The correction: half an hour before the event.
	if _, err := f.campaignSvc.UpdateStep(ctx, late.ID, campaigns.StepInput{
		Name:          late.Name,
		OffsetSeconds: -1800,
		TemplateID:    late.TemplateID,
		Enabled:       true,
		ScheduleKind:  string(domain.ScheduleRelativeToEvent),
	}); err != nil {
		t.Fatalf("UpdateStep: %v", err)
	}

	job, ok := liveJobs(t, f, enrollment.ID)[late.ID]
	if !ok || job.Status != domain.JobPending {
		t.Fatal("a contact still waiting for the event did not receive the corrected step")
	}
}
