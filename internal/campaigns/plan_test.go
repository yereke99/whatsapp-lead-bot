package campaigns

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

// almaty is the campaign timezone used throughout these tests.
func almaty(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatalf("load Asia/Almaty: %v", err)
	}
	return loc
}

func step(name string, offsetSeconds, order int, enabled bool) domain.CampaignStep {
	return domain.CampaignStep{
		ID:            uuid.New(),
		Name:          name,
		OffsetSeconds: offsetSeconds,
		Enabled:       enabled,
		OrderIndex:    order,
		ScheduleKind:  domain.ScheduleRelativeToEvent,
		TemplateID:    uuid.New(),
	}
}

// webinarSteps mirrors the reference campaign from the specification.
func webinarSteps() []domain.CampaignStep {
	return []domain.CampaignStep{
		{
			ID: uuid.New(), Name: "Триггер", OffsetSeconds: 0, Enabled: true,
			OrderIndex: 1, ScheduleKind: domain.ScheduleOnTrigger, TemplateID: uuid.New(),
		},
		step("5 сағат бұрын", -5*3600, 2, true),
		step("3 сағат бұрын", -3*3600, 3, true),
		step("2 сағат бұрын", -2*3600, 4, true),
		step("1 сағат бұрын", -3600, 5, true),
		step("30 минут бұрын", -30*60, 6, true),
		step("15 минут бұрын", -15*60, 7, true),
		step("7.5 минут бұрын", -450, 8, true),
		step("Басталды", 0, 9, true),
	}
}

// TestBuildPlanMatchesSpecifiedSchedule is the canonical case: a 21:00 webinar
// must produce exactly the times listed in the requirements, including the
// fractional 7.5-minute step at 20:52:30.
func TestBuildPlanMatchesSpecifiedSchedule(t *testing.T) {
	loc := almaty(t)
	eventStart := time.Date(2026, 8, 16, 21, 0, 0, 0, loc).UTC()

	// Enrol well before the first step so nothing is treated as missed.
	now := time.Date(2026, 8, 16, 9, 0, 0, 0, loc).UTC()

	plan := BuildPlan(&eventStart, webinarSteps(), PlanOptions{
		EnrolledAt: now, Now: now, CatchUp: true,
	})

	want := []string{
		"", // ON_TRIGGER, checked separately
		"16:00:00",
		"18:00:00",
		"19:00:00",
		"20:00:00",
		"20:30:00",
		"20:45:00",
		"20:52:30",
		"21:00:00",
	}

	if len(plan) != len(want) {
		t.Fatalf("plan has %d entries, want %d", len(plan), len(want))
	}

	for i, entry := range plan {
		if entry.Skipped {
			t.Errorf("step %q was skipped unexpectedly: %s", entry.Step.Name, entry.Reason)
			continue
		}

		if i == 0 {
			if !entry.RunAt.Equal(now) {
				t.Errorf("ON_TRIGGER step should run at enrollment time %v, got %v", now, entry.RunAt)
			}
			continue
		}

		got := entry.RunAt.In(loc).Format("15:04:05")
		if got != want[i] {
			t.Errorf("step %q scheduled at %s, want %s", entry.Step.Name, got, want[i])
		}
	}
}

// TestBuildPlanEventTimeShift verifies that moving the webinar moves every
// relative step with it.
func TestBuildPlanEventTimeShift(t *testing.T) {
	loc := almaty(t)
	now := time.Date(2026, 8, 16, 9, 0, 0, 0, loc).UTC()

	at21 := time.Date(2026, 8, 16, 21, 0, 0, 0, loc).UTC()
	at20 := time.Date(2026, 8, 16, 20, 0, 0, 0, loc).UTC()

	steps := webinarSteps()
	planA := Scheduled(BuildPlan(&at21, steps, PlanOptions{EnrolledAt: now, Now: now}))
	planB := Scheduled(BuildPlan(&at20, steps, PlanOptions{EnrolledAt: now, Now: now}))

	if len(planA) != len(planB) {
		t.Fatalf("plans differ in length: %d vs %d", len(planA), len(planB))
	}

	for i := range planA {
		if planA[i].Step.ScheduleKind == domain.ScheduleOnTrigger {
			continue
		}
		diff := planA[i].RunAt.Sub(planB[i].RunAt)
		if diff != time.Hour {
			t.Errorf("step %q shifted by %v, want 1h", planA[i].Step.Name, diff)
		}
	}
}

func TestBuildPlanSkipsDisabledSteps(t *testing.T) {
	loc := almaty(t)
	eventStart := time.Date(2026, 8, 16, 21, 0, 0, 0, loc).UTC()
	now := time.Date(2026, 8, 16, 9, 0, 0, 0, loc).UTC()

	steps := []domain.CampaignStep{
		step("enabled", -3600, 1, true),
		step("disabled", -1800, 2, false),
	}

	plan := BuildPlan(&eventStart, steps, PlanOptions{EnrolledAt: now, Now: now})

	if plan[0].Skipped {
		t.Error("enabled step should be scheduled")
	}
	if !plan[1].Skipped {
		t.Error("disabled step should be skipped")
	}
	if len(Scheduled(plan)) != 1 {
		t.Errorf("expected 1 scheduled entry, got %d", len(Scheduled(plan)))
	}
}

// TestBuildPlanCatchUpSendsOnlyTheLatestMissedStep checks the late-joiner rule:
// a contact who enrols two hours before the webinar must not receive the
// "5 hours left" and "3 hours left" messages at once.
func TestBuildPlanCatchUpSendsOnlyTheLatestMissedStep(t *testing.T) {
	loc := almaty(t)
	eventStart := time.Date(2026, 8, 16, 21, 0, 0, 0, loc).UTC()
	now := time.Date(2026, 8, 16, 19, 30, 0, 0, loc).UTC() // between the 2h and 1h steps

	plan := BuildPlan(&eventStart, webinarSteps(), PlanOptions{
		EnrolledAt: now, Now: now, CatchUp: true,
	})

	var caughtUp []string
	var skipped []string
	for _, entry := range plan {
		if entry.Step.ScheduleKind == domain.ScheduleOnTrigger {
			continue
		}
		switch {
		case entry.Skipped:
			skipped = append(skipped, entry.Step.Name)
		case entry.RunAt.Equal(now):
			caughtUp = append(caughtUp, entry.Step.Name)
		}
	}

	if len(caughtUp) != 1 {
		t.Fatalf("expected exactly one caught-up step, got %v", caughtUp)
	}
	if caughtUp[0] != "2 сағат бұрын" {
		t.Errorf("caught-up step is %q, want the most recent missed one", caughtUp[0])
	}

	wantSkipped := []string{"5 сағат бұрын", "3 сағат бұрын"}
	if len(skipped) != len(wantSkipped) {
		t.Errorf("skipped %v, want %v", skipped, wantSkipped)
	}
}

func TestBuildPlanWithoutCatchUpSkipsAllPastSteps(t *testing.T) {
	loc := almaty(t)
	eventStart := time.Date(2026, 8, 16, 21, 0, 0, 0, loc).UTC()
	now := time.Date(2026, 8, 16, 19, 30, 0, 0, loc).UTC()

	plan := BuildPlan(&eventStart, webinarSteps(), PlanOptions{
		EnrolledAt: now, Now: now, CatchUp: false,
	})

	for _, entry := range plan {
		if entry.Step.ScheduleKind == domain.ScheduleOnTrigger {
			continue
		}
		if !entry.Skipped && entry.RunAt.Before(now.Add(-time.Minute)) {
			t.Errorf("step %q kept a past send time %v", entry.Step.Name, entry.RunAt)
		}
	}

	scheduled := Scheduled(plan)
	// ON_TRIGGER plus the five steps still in the future.
	if len(scheduled) != 6 {
		t.Errorf("expected 6 scheduled entries, got %d", len(scheduled))
	}
}

func TestBuildPlanWithoutEventStart(t *testing.T) {
	now := time.Now().UTC()

	plan := BuildPlan(nil, webinarSteps(), PlanOptions{EnrolledAt: now, Now: now})

	for _, entry := range plan {
		if entry.Step.ScheduleKind == domain.ScheduleOnTrigger {
			if entry.Skipped {
				t.Error("trigger-time step should still be schedulable without an event anchor")
			}
			continue
		}
		if !entry.Skipped {
			t.Errorf("step %q should be skipped when the event has no start time", entry.Step.Name)
		}
	}
}

// TestBuildPlanGraceWindow ensures a step whose time passed a few seconds ago
// is still sent rather than dropped, absorbing scheduler and clock jitter.
func TestBuildPlanGraceWindow(t *testing.T) {
	loc := almaty(t)
	eventStart := time.Date(2026, 8, 16, 21, 0, 0, 0, loc).UTC()

	// Ten seconds after the "1 hour before" step was due.
	now := time.Date(2026, 8, 16, 20, 0, 10, 0, loc).UTC()

	steps := []domain.CampaignStep{step("1 сағат бұрын", -3600, 1, true)}
	plan := BuildPlan(&eventStart, steps, PlanOptions{
		EnrolledAt: now, Now: now, Grace: time.Minute,
	})

	if plan[0].Skipped {
		t.Error("a step due ten seconds ago should still be scheduled within the grace window")
	}
}

// TestBuildPlanAcrossDSTBoundary confirms the offsets are wall-clock stable in
// a zone that observes daylight saving. Asia/Almaty does not, so a European
// zone is used to exercise the arithmetic.
func TestBuildPlanAcrossDSTBoundary(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("Europe/Berlin unavailable: %v", err)
	}

	// 2026-03-29 02:00 local is when Berlin springs forward.
	eventStart := time.Date(2026, 3, 29, 12, 0, 0, 0, loc).UTC()
	now := time.Date(2026, 3, 28, 12, 0, 0, 0, loc).UTC()

	steps := []domain.CampaignStep{step("12 сағат бұрын", -12*3600, 1, true)}
	plan := BuildPlan(&eventStart, steps, PlanOptions{EnrolledAt: now, Now: now})

	// The offset is an absolute duration, so the instant is exactly 12h before.
	want := eventStart.Add(-12 * time.Hour)
	if !plan[0].RunAt.Equal(want) {
		t.Errorf("run at %v, want %v", plan[0].RunAt, want)
	}
}

func TestHumanOffsetFormatting(t *testing.T) {
	cases := map[int]string{
		0:         "0",
		-300:      "-5m",
		-3600:     "-1h",
		-450:      "-7m30s",
		-9000:     "-2h30m",
		3600:      "+1h",
		-5 * 3600: "-5h",
	}

	for seconds, want := range cases {
		if got := timex.HumanOffset(seconds); got != want {
			t.Errorf("HumanOffset(%d) = %q, want %q", seconds, got, want)
		}
	}
}
