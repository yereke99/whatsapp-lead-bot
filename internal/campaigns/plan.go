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
	// SkipCode is the stable, machine-readable form of Reason. Reason is the
	// operator's sentence and may be reworded; SkipCode is stored on the job
	// row, so it must not change. Empty when the entry is not skipped.
	SkipCode string `json:"skip_code,omitempty"`
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
	// TriggerDelay is the shortest gap allowed between the customer's trigger
	// and a step anchored to it. It stops the greeting landing in the same
	// instant as the message that asked for it.
	TriggerDelay time.Duration
}

// BuildPlan resolves a campaign's steps into absolute send times.
//
// Two anchors are in play, and they answer different questions:
//
//   - RELATIVE_TO_EVENT measures from the campaign's event start, so every
//     contact receives the step at the same wall-clock moment. A webinar at
//     21:00 with an offset of -3h is the 18:00 message, for everyone.
//   - ON_TRIGGER measures from the moment this contact entered the campaign,
//     so the offset is a personal delay. A contact who triggers at 17:30 and
//     one who triggers at 18:10 each get their own timetable.
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

	// The trigger anchor never runs behind the current time: the provider
	// timestamp on the inbound message can be seconds old by the time it is
	// processed, and a delay measured from a stale instant would collapse to
	// nothing.
	triggerAnchor := opts.EnrolledAt
	if triggerAnchor.Before(opts.Now) {
		triggerAnchor = opts.Now
	}

	// First pass: resolve times and mark everything that is disabled or
	// impossible to schedule.
	for _, step := range steps {
		entry := PlanEntry{Step: step}

		switch {
		case !step.Enabled:
			entry.Skipped = true
			entry.Reason = "қадам өшірілген"
			entry.SkipCode = domain.SkipStepDisabled

		case step.ScheduleKind == domain.ScheduleOnTrigger:
			delay := time.Duration(step.OffsetSeconds) * time.Second
			if delay < opts.TriggerDelay {
				delay = opts.TriggerDelay
			}
			entry.RunAt = triggerAnchor.Add(delay)

		case eventStart == nil:
			entry.Skipped = true
			entry.Reason = "іс-шара уақыты белгіленбеген"
			entry.SkipCode = domain.SkipNoEventAnchor

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
			// Send it now rather than at its original, already-past time —
			// but never ahead of the greeting. The queue is drained in
			// schedule order, so a catch-up placed at "now" would reach a
			// contact who has just written in before the reply that
			// acknowledges them, which reads as the funnel talking over
			// itself.
			e.RunAt = triggerAnchor.Add(opts.TriggerDelay + time.Second)
			if e.RunAt.Before(opts.Now) {
				e.RunAt = opts.Now
			}
			e.Reason = "уақыты өтіп кеткен, бірден жіберіледі"
			continue
		}

		e.Skipped = true
		e.Reason = "уақыты өтіп кеткен"
		e.SkipCode = domain.SkipStepExpired
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
//
// It carries both the stored configuration (offset, schedule kind) and the
// resolved wall-clock time in the campaign's own timezone, because the
// operator reasons in "18:00" while the database reasons in offsets.
type PreviewEntry struct {
	StepID       string `json:"step_id"`
	OrderIndex   int    `json:"order_index"`
	Name         string `json:"name"`
	ScheduleKind string `json:"schedule_kind"`
	OffsetLabel  string `json:"offset_label"`
	Offset       int    `json:"offset_seconds"`
	LocalDate    string `json:"local_date"`
	LocalTime    string `json:"local_time"`
	UTCTime      string `json:"utc_time"`
	TemplateID   string `json:"message_template_id"`
	TemplateName string `json:"template_name"`
	TemplateType string `json:"template_type"`
	Enabled      bool   `json:"enabled"`
	Warning      string `json:"warning,omitempty"`
}
