package campaigns

import (
	"time"

	"github.com/ayran/whatsapp-automation/internal/domain"
)

// PlanEntry is one step resolved to an absolute send time.
type PlanEntry struct {
	Step    domain.CampaignStep `json:"step"`
	RunAt   time.Time           `json:"run_at"`
	Skipped bool                `json:"skipped"`
	Reason  string              `json:"reason,omitempty"`
}

// PlanOptions controls how a plan is built for one contact.
type PlanOptions struct {
	// EnrolledAt anchors ON_TRIGGER steps.
	EnrolledAt time.Time
	// Now is the reference for deciding which steps are already in the past.
	Now time.Time
	// CatchUp sends the most recent already-passed step immediately instead of
	// skipping it, so a contact who joins two hours before the webinar still
	// receives context rather than silence.
	CatchUp bool
	// Grace treats a step scheduled slightly in the past as still on time,
	// which absorbs clock skew and short scheduler delays.
	Grace time.Duration
}

// BuildPlan resolves a campaign's steps into absolute send times.
//
// This function is deliberately pure: no database, no clock of its own. All
// scheduling behaviour is therefore directly testable, and the same logic
// serves both the admin's campaign preview and real enrollment.
func BuildPlan(eventStart *time.Time, steps []domain.CampaignStep, opts PlanOptions) []PlanEntry {
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if opts.EnrolledAt.IsZero() {
		opts.EnrolledAt = opts.Now
	}
	if opts.Grace <= 0 {
		opts.Grace = time.Minute
	}

	entries := make([]PlanEntry, 0, len(steps))
	cutoff := opts.Now.Add(-opts.Grace)

	// First pass: resolve times and mark everything that is disabled or
	// impossible to schedule.
	for _, step := range steps {
		entry := PlanEntry{Step: step}

		switch {
		case !step.Enabled:
			entry.Skipped = true
			entry.Reason = "қадам өшірілген"

		case step.ScheduleKind == domain.ScheduleOnTrigger:
			entry.RunAt = opts.EnrolledAt

		case eventStart == nil:
			entry.Skipped = true
			entry.Reason = "іс-шара уақыты белгіленбеген"

		default:
			entry.RunAt = eventStart.Add(time.Duration(step.OffsetSeconds) * time.Second)
		}

		entries = append(entries, entry)
	}

	// Second pass: decide what to do with steps whose time already passed.
	// Only the latest missed step is worth sending; the earlier ones would
	// arrive out of order and contradict each other.
	lastMissed := -1
	for i, e := range entries {
		if e.Skipped || e.Step.ScheduleKind == domain.ScheduleOnTrigger {
			continue
		}
		if e.RunAt.Before(cutoff) {
			lastMissed = i
		}
	}

	for i := range entries {
		e := &entries[i]
		if e.Skipped || e.Step.ScheduleKind == domain.ScheduleOnTrigger {
			continue
		}
		if !e.RunAt.Before(cutoff) {
			continue
		}

		if opts.CatchUp && i == lastMissed {
			// Send it now rather than at its original, already-past time.
			e.RunAt = opts.Now
			e.Reason = "уақыты өтіп кеткен, бірден жіберіледі"
			continue
		}

		e.Skipped = true
		e.Reason = "уақыты өтіп кеткен"
	}

	return entries
}

// Scheduled returns only the entries that will actually be enqueued.
func Scheduled(entries []PlanEntry) []PlanEntry {
	out := make([]PlanEntry, 0, len(entries))
	for _, e := range entries {
		if !e.Skipped {
			out = append(out, e)
		}
	}
	return out
}

// PreviewEntry is one row of the campaign timeline shown before activation.
type PreviewEntry struct {
	StepID       string `json:"step_id"`
	Name         string `json:"name"`
	OffsetLabel  string `json:"offset_label"`
	Offset       int    `json:"offset_seconds"`
	LocalTime    string `json:"local_time"`
	UTCTime      string `json:"utc_time"`
	TemplateName string `json:"template_name"`
	TemplateType string `json:"template_type"`
	Enabled      bool   `json:"enabled"`
	Warning      string `json:"warning,omitempty"`
}
