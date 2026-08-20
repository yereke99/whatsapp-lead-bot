package campaigns

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
)

// The daily webinar sequence, as arithmetic.
//
// These are the pure half: which steps make up the sequence, when a contact
// counts as having been through it, and what the planner does about it. The
// database half — enrolment, restarts, reconciliation — lives in
// internal/integration/daily_webinar_test.go.

func dailyStep(offset int, daily bool) domain.CampaignStep {
	return domain.CampaignStep{
		ID:                    uuid.New(),
		Name:                  "step",
		OffsetSeconds:         offset,
		Enabled:               true,
		ScheduleKind:          domain.ScheduleRelativeToEvent,
		IncludeInDailyWebinar: daily,
	}
}

func recurringFor(t *testing.T) *domain.Campaign {
	t.Helper()
	return &domain.Campaign{
		ID:               uuid.New(),
		Status:           domain.CampaignActive,
		Timezone:         "Asia/Almaty",
		IsDailyRecurring: true,
		RecurrenceTime:   "21:00",
	}
}

// TestDailySequenceIsWhateverTheOperatorMarked. The customer's funnel has seven
// reminders today and may have five or ten tomorrow. Nothing in this system
// counts them.
func TestDailySequenceIsWhateverTheOperatorMarked(t *testing.T) {
	for _, want := range []int{0, 1, 5, 7, 10} {
		steps := make([]domain.CampaignStep, 0, want+3)
		for i := 0; i < want; i++ {
			steps = append(steps, dailyStep(-(i+1)*600, true))
		}
		// Three that are not part of the sequence: a greeting, an unrelated
		// note, a follow-up after the event.
		steps = append(steps, dailyStep(0, false), dailyStep(-7200, false), dailyStep(3600, false))

		if got := len(DailySequence(steps)); got != want {
			t.Errorf("daily sequence of %d marked steps = %d", want, got)
		}
	}
}

// TestDailySequenceIsNotConsumedOnAFirstRun is the guard against the dangerous
// misreading of this feature. A contact halfway through their own first
// sequence has messages sent and messages still to come; nothing here may
// retire the ones still to come.
func TestDailySequenceIsNotConsumedOnAFirstRun(t *testing.T) {
	campaign := recurringFor(t)
	steps := []domain.CampaignStep{dailyStep(-18000, true), dailyStep(-10800, true), dailyStep(0, true)}

	jobs := []scheduler.ExistingJob{
		{StepID: steps[0].ID, RunNumber: 1, Status: domain.JobSent},
		{StepID: steps[1].ID, RunNumber: 1, Status: domain.JobSent},
		{StepID: steps[2].ID, RunNumber: 1, Status: domain.JobPending},
	}

	if DailySequenceConsumed(campaign, steps, jobs, 1) {
		t.Fatal("a contact mid-sequence was treated as having finished it")
	}
}

// TestDailySequenceConsumedAfterARestart is the reported bug, reduced. The
// contact went through the sequence yesterday; the campaign is configured to
// restart on a repeat trigger; today's run must not hand them the sequence
// again.
func TestDailySequenceConsumedAfterARestart(t *testing.T) {
	campaign := recurringFor(t)
	steps := []domain.CampaignStep{dailyStep(-18000, true), dailyStep(0, true)}

	jobs := []scheduler.ExistingJob{
		{StepID: steps[0].ID, RunNumber: 1, Status: domain.JobSent},
		{StepID: steps[1].ID, RunNumber: 1, Status: domain.JobSent},
	}

	if !DailySequenceConsumed(campaign, steps, jobs, 2) {
		t.Fatal("a returning participant was treated as a new one")
	}
}

// TestRestartBeforeAnythingWentOutStillGetsTheSequence. Restarting ten minutes
// after entering, before a single message has gone out, is not "already
// received it". Only a delivery retires the sequence.
func TestRestartBeforeAnythingWentOutStillGetsTheSequence(t *testing.T) {
	campaign := recurringFor(t)
	steps := []domain.CampaignStep{dailyStep(-18000, true), dailyStep(0, true)}

	jobs := []scheduler.ExistingJob{
		{StepID: steps[0].ID, RunNumber: 1, Status: domain.JobCancelled, CancelReason: "campaign restarted by trigger"},
		{StepID: steps[1].ID, RunNumber: 1, Status: domain.JobCancelled, CancelReason: domain.SkipStepExpired},
	}

	if DailySequenceConsumed(campaign, steps, jobs, 2) {
		t.Fatal("a contact who received nothing was refused the sequence")
	}
}

// TestOneTimeCampaignsAreUntouched. RESTART on a campaign that is not a daily
// webinar keeps meaning exactly what it always meant.
func TestOneTimeCampaignsAreUntouched(t *testing.T) {
	campaign := recurringFor(t)
	campaign.IsDailyRecurring = false
	steps := []domain.CampaignStep{dailyStep(-18000, true)}
	jobs := []scheduler.ExistingJob{{StepID: steps[0].ID, RunNumber: 1, Status: domain.JobSent}}

	if DailySequenceConsumed(campaign, steps, jobs, 2) {
		t.Fatal("a one-time campaign was given daily-webinar semantics")
	}
}

// TestUnmarkedStepsNeverRetire. The flag is what puts a step in the sequence,
// so a campaign where nobody ticked anything behaves exactly as it does today.
func TestUnmarkedStepsNeverRetire(t *testing.T) {
	campaign := recurringFor(t)
	steps := []domain.CampaignStep{dailyStep(-18000, false), dailyStep(0, false)}
	jobs := []scheduler.ExistingJob{
		{StepID: steps[0].ID, RunNumber: 1, Status: domain.JobSent},
		{StepID: steps[1].ID, RunNumber: 1, Status: domain.JobSent},
	}

	if DailySequenceConsumed(campaign, steps, jobs, 2) {
		t.Fatal("unmarked steps formed a daily sequence on their own")
	}
}

// TestPlanSkipsOnlyTheDailySteps. A retired sequence must not take the
// greeting or the follow-up with it: those are separate messages with separate
// reasons to exist.
func TestPlanSkipsOnlyTheDailySteps(t *testing.T) {
	event := time.Date(2026, 8, 21, 16, 0, 0, 0, time.UTC) // 21:00 Almaty
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)

	greeting := dailyStep(5, false)
	greeting.ScheduleKind = domain.ScheduleOnTrigger
	reminder := dailyStep(-10800, true)
	start := dailyStep(0, true)
	followUp := dailyStep(3600, false)

	plan := BuildPlan(&event, []domain.CampaignStep{greeting, reminder, start, followUp}, PlanOptions{
		EnrolledAt:            now,
		Now:                   now,
		CatchUp:               true,
		TriggerDelay:          2 * time.Second,
		DailySequenceConsumed: true,
	})

	byID := map[uuid.UUID]PlanEntry{}
	for _, e := range plan {
		byID[e.Step.ID] = e
	}

	for _, id := range []uuid.UUID{reminder.ID, start.ID} {
		e := byID[id]
		if !e.Skipped || e.SkipCode != domain.SkipDailySequenceDone {
			t.Errorf("daily step scheduled again: skipped=%v code=%q", e.Skipped, e.SkipCode)
		}
	}
	for _, id := range []uuid.UUID{greeting.ID, followUp.ID} {
		if byID[id].Skipped {
			t.Errorf("a step outside the daily sequence was retired with it: %v", byID[id].Reason)
		}
	}
}

// TestPlanIsUnchangedWhenTheSequenceIsNotConsumed pins the default. Everything
// above is inert until DailySequenceConsumed says otherwise.
func TestPlanIsUnchangedWhenTheSequenceIsNotConsumed(t *testing.T) {
	event := time.Date(2026, 8, 21, 16, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	steps := []domain.CampaignStep{dailyStep(-10800, true), dailyStep(0, true)}

	plan := BuildPlan(&event, steps, PlanOptions{EnrolledAt: now, Now: now})
	for _, e := range plan {
		if e.Skipped {
			t.Fatalf("step skipped with no consumed sequence: %s", e.Reason)
		}
	}
}
