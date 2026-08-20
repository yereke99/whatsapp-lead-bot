package campaigns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/pkg/textnorm"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

// defaultTriggerDelay keeps the first reply from landing in the same instant
// as the customer's own message when no delay is configured.
const defaultTriggerDelay = 2 * time.Second

type Service struct {
	repo     *Repository
	jobs     *scheduler.Repository
	contacts *contacts.Repository
	log      *slog.Logger

	// triggerDelay is the floor for steps anchored to the trigger.
	triggerDelay time.Duration
}

func NewService(repo *Repository, jobs *scheduler.Repository, contactRepo *contacts.Repository, log *slog.Logger) *Service {
	return &Service{
		repo:         repo,
		jobs:         jobs,
		contacts:     contactRepo,
		log:          log.With(slog.String("component", "campaigns")),
		triggerDelay: defaultTriggerDelay,
	}
}

// WithTriggerDelay sets the minimum gap between a trigger and the first
// message it schedules. Values outside 1–5 seconds are ignored: an instant
// reply reads as a bot, and a long one reads as a broken funnel.
func (s *Service) WithTriggerDelay(d time.Duration) *Service {
	if d >= time.Second && d <= 5*time.Second {
		s.triggerDelay = d
	}
	return s
}

func (s *Service) Repo() *Repository { return s.repo }

// ------------------------------------------------------- trigger routing --

// MatchTrigger finds the campaign a message should enroll into.
//
// Only triggers belonging to ACTIVE campaigns are considered, so pausing a
// campaign immediately stops new entries. Ordering from the query makes the
// result deterministic when several keywords could match.
func (s *Service) MatchTrigger(ctx context.Context, q sqlite.Querier, messageText string) (*TriggerMatch, error) {
	normalized := textnorm.Normalize(messageText)
	if normalized == "" {
		return nil, nil
	}

	triggers, err := s.repo.ActiveTriggers(ctx, q)
	if err != nil {
		return nil, err
	}

	for i := range triggers {
		t := triggers[i]
		if textnorm.Matches(normalized, t.Normalized, textnorm.MatchMode(t.MatchMode)) {
			return &t, nil
		}
	}
	return nil, nil
}

// IsUnsubscribe reports whether a message is a stop word for this contact.
func (s *Service) IsUnsubscribe(ctx context.Context, q sqlite.Querier, contactID uuid.UUID, messageText string) (bool, error) {
	normalized := textnorm.Normalize(messageText)
	if normalized == "" {
		return false, nil
	}

	keywords, err := s.repo.UnsubscribeKeywordsFor(ctx, q, contactID)
	if err != nil {
		return false, err
	}

	for _, kw := range keywords {
		if textnorm.Matches(normalized, textnorm.Normalize(kw), textnorm.MatchExact) {
			return true, nil
		}
	}
	return false, nil
}

// --------------------------------------------------------------- enroll --

// EnrollAction records what the engine decided to do about a trigger.
type EnrollAction string

const (
	ActionEnrolled       EnrollAction = "ENROLLED"
	ActionRestarted      EnrollAction = "RESTARTED"
	ActionContinued      EnrollAction = "CONTINUED"
	ActionIgnored        EnrollAction = "IGNORED"
	ActionSpecialMessage EnrollAction = "SPECIAL_MESSAGE"
	ActionBlocked        EnrollAction = "BLOCKED"
)

type EnrollResult struct {
	Action      EnrollAction
	Campaign    *domain.Campaign
	Enrollment  *domain.Enrollment
	JobsCreated int
	// SpecialTemplateID is set for the SPECIAL_MESSAGE outcome: one reply is
	// sent and no new schedule is created.
	SpecialTemplateID *uuid.UUID
	Reason            string
}

// HandleTrigger enrolls a contact into the matched campaign, or applies the
// configured behaviour when they are already enrolled.
//
// The whole decision runs in one transaction, so two webhook deliveries racing
// on the same trigger cannot both create a schedule.
func (s *Service) HandleTrigger(ctx context.Context, contact *domain.Contact, match *TriggerMatch, at time.Time) (*EnrollResult, error) {
	if contact == nil || match == nil {
		return &EnrollResult{Action: ActionIgnored, Reason: "no trigger match"}, nil
	}
	if !contact.CanReceiveMessages() {
		return &EnrollResult{
			Action: ActionBlocked,
			Reason: "contact has opted out or is blocked",
		}, nil
	}

	result := &EnrollResult{}

	err := s.repo.DB().InTx(ctx, func(tx sqlite.Querier) error {
		campaign, err := s.repo.GetByID(ctx, tx, match.CampaignID)
		if err != nil {
			return err
		}
		if campaign == nil {
			return ErrNotFound
		}
		if !campaign.AcceptsEnrollments() {
			result.Action = ActionIgnored
			result.Reason = "campaign is not accepting enrollments"
			return nil
		}
		result.Campaign = campaign

		existing, err := s.repo.FindEnrollment(ctx, tx, campaign.ID, contact.ID)
		if err != nil {
			return err
		}

		if existing != nil {
			return s.applyExistingBehavior(ctx, tx, campaign, contact, existing, match, at, result)
		}

		// An optional cap on how many contacts one campaign may be running at
		// once. New entries stop; contacts already in the funnel finish.
		if campaign.MaxActiveContacts != nil {
			active, err := s.repo.ActiveEnrollmentCount(ctx, tx, campaign.ID)
			if err != nil {
				return err
			}
			if active >= *campaign.MaxActiveContacts {
				result.Action = ActionIgnored
				result.Reason = fmt.Sprintf("белсенді клиент шегі толды (%d)", *campaign.MaxActiveContacts)
				s.log.Warn("campaign contact limit reached; enrollment refused",
					slog.String("campaign_id", campaign.ID.String()),
					slog.Int("active", active),
					slog.Int("limit", *campaign.MaxActiveContacts))
				return nil
			}
		}

		enrollment := &domain.Enrollment{
			CampaignID:     campaign.ID,
			ContactID:      contact.ID,
			TriggerID:      &match.TriggerID,
			TriggerKeyword: match.Keyword,
		}

		// A recurring campaign pins the webinar this contact is joining before
		// the enrolment row exists, so the anchor is written once, atomically,
		// and every later read — planner, reconciler, worker — sees the same
		// answer. For a one-time campaign this is nil and nothing changes.
		occurrence, err := s.occurrenceFor(ctx, tx, campaign, at)
		if err != nil {
			return err
		}
		enrollment.OccurrenceAt = occurrence

		if err := s.repo.CreateEnrollment(ctx, tx, enrollment); err != nil {
			if errors.Is(err, ErrAlreadyEnrolled) {
				// Lost a race with a concurrent delivery of the same trigger.
				// The other transaction has done the work.
				result.Action = ActionIgnored
				result.Reason = "duplicate trigger delivery"
				return nil
			}
			return err
		}
		result.Enrollment = enrollment
		result.Action = ActionEnrolled

		if err := s.contacts.SetFirstCampaign(ctx, tx, contact.ID, campaign.ID, match.Keyword); err != nil {
			return err
		}

		created, err := s.scheduleEnrollment(ctx, tx, campaign, enrollment, at)
		if err != nil {
			return err
		}
		result.JobsCreated = created
		return nil
	})

	if err != nil {
		return nil, err
	}

	s.log.Info("trigger handled",
		slog.String("action", string(result.Action)),
		slog.String("contact_id", contact.ID.String()),
		slog.String("campaign", match.CampaignName),
		slog.String("keyword", match.Keyword),
		slog.Int("jobs_created", result.JobsCreated))

	return result, nil
}

func (s *Service) applyExistingBehavior(
	ctx context.Context, tx sqlite.Querier,
	campaign *domain.Campaign, contact *domain.Contact,
	existing *domain.Enrollment, match *TriggerMatch,
	at time.Time, result *EnrollResult,
) error {
	result.Enrollment = existing

	switch campaign.ExistingContactBehavior {
	case domain.BehaviorRestart:
		if _, err := s.jobs.CancelPendingForEnrollment(ctx, tx, existing.ID, "campaign restarted by trigger"); err != nil {
			return err
		}
		// A restart is a new run, so it gets the webinar that is next for this
		// contact now rather than inheriting the one the previous run was
		// waiting for. nil for a non-recurring campaign, which restores exactly
		// the previous behaviour.
		occurrence, err := s.occurrenceFor(ctx, tx, campaign, at)
		if err != nil {
			return err
		}
		restarted, err := s.repo.RestartEnrollment(ctx, tx, existing.ID, match.Keyword, occurrence)
		if err != nil {
			return err
		}
		result.Enrollment = restarted
		result.Action = ActionRestarted

		created, err := s.scheduleEnrollment(ctx, tx, campaign, restarted, at)
		if err != nil {
			return err
		}
		result.JobsCreated = created
		return nil

	case domain.BehaviorContinue:
		// Reactivate a cancelled enrollment but leave the existing schedule
		// alone; anything already sent stays sent.
		if existing.Status != domain.EnrollmentActive {
			if err := s.repo.SetEnrollmentStatus(ctx, tx, existing.ID, domain.EnrollmentActive, ""); err != nil {
				return err
			}
		}
		// A contact enrolled before recurrence was switched on has no webinar
		// pinned, so they are still anchored to the series start — a date in the
		// past, whose every step is expired. Giving them the next occurrence is
		// what CONTINUE means for a daily webinar. An enrolment that already has
		// an occurrence keeps it: CONTINUE's contract is not to disturb a
		// schedule that exists.
		if campaign.IsDailyRecurring && existing.OccurrenceAt == nil {
			occurrence, err := s.occurrenceFor(ctx, tx, campaign, at)
			if err != nil {
				return err
			}
			if occurrence != nil {
				if err := s.repo.SetEnrollmentOccurrence(ctx, tx, existing.ID, occurrence); err != nil {
					return err
				}
				existing.OccurrenceAt = occurrence
			}
		}
		created, err := s.scheduleEnrollment(ctx, tx, campaign, existing, at)
		if err != nil {
			return err
		}
		result.JobsCreated = created
		result.Action = ActionContinued
		return nil

	case domain.BehaviorSpecialMessage:
		result.Action = ActionSpecialMessage
		result.SpecialTemplateID = campaign.ExistingContactTemplate
		if result.SpecialTemplateID == nil {
			result.Action = ActionIgnored
			result.Reason = "no reply template configured for repeat triggers"
		}
		return nil

	default:
		result.Action = ActionIgnored
		result.Reason = "repeat trigger ignored by campaign configuration"
		return nil
	}
}

// occurrenceFor decides which webinar a contact entering at `at` belongs to.
//
// It returns nil for every campaign that is not recurring, which is what keeps
// the feature opt-in all the way down: a nil occurrence means the enrolment is
// anchored to campaign.event_start_at, exactly as every enrolment is today.
//
// The steps are read only for a recurring campaign, and only to ask how long an
// occurrence keeps claiming arrivals — see Recurrence.OccurrenceFor. A campaign
// with the toggle off pays nothing for this.
//
// A misconfigured recurrence is not allowed to break enrolment. The trigger has
// already arrived and the contact is already in the funnel; refusing to enrol
// them because an operator typed a bad time would lose the lead. The campaign
// falls back to its one-time anchor and the fault is logged loudly instead.
func (s *Service) occurrenceFor(ctx context.Context, tx sqlite.Querier, campaign *domain.Campaign, at time.Time) (*time.Time, error) {
	if !campaign.IsDailyRecurring {
		return nil, nil
	}

	steps, err := s.repo.ListSteps(ctx, tx, campaign.ID)
	if err != nil {
		return nil, err
	}

	recurrence := RecurrenceOf(campaign)
	occurrence, err := recurrence.OccurrenceFor(at, StepTail(steps))
	if err != nil {
		s.log.Error("recurring webinar is misconfigured; falling back to the campaign event time",
			slog.String("campaign_id", campaign.ID.String()),
			slog.String("campaign_name", campaign.Name),
			slog.String("webinar_time", recurrence.Time),
			slog.String("timezone", recurrence.Zone),
			slog.String("error", err.Error()))
		return nil, nil
	}

	s.log.Info("RECURRING_OCCURRENCE_ASSIGNED",
		slog.String("campaign_id", campaign.ID.String()),
		slog.String("campaign_name", campaign.Name),
		slog.String("occurrence_date", timex.FormatIn(occurrence, campaign.Timezone, timex.DateLayout)),
		slog.Time("webinar_datetime", occurrence),
		slog.String("timezone", campaign.Timezone))

	return &occurrence, nil
}

// scheduleEnrollment turns the campaign's steps into persisted jobs.
//
// This is the whole of "send the campaign to this contact": the plan is
// computed once, written to the queue, and from then on the database is the
// only thing that decides when anything is sent. Nothing is delivered inline,
// and nothing depends on this process staying alive.
//
// Every step produces a row — a live job, or a recorded skip saying why not.
// Writing the skips down is what lets reconciliation tell "this step was
// considered and declined" apart from "this step has no job", and the second of
// those is now always a fault worth repairing.
//
// This is also the only place catch-up applies. Deciding to send a contact the
// most recent message they missed is a judgement about someone who has just
// arrived; later repairs must never make it again, or a step added late would
// fire at everyone already in the funnel at once.
func (s *Service) scheduleEnrollment(ctx context.Context, tx sqlite.Querier, campaign *domain.Campaign, enrollment *domain.Enrollment, at time.Time) (int, error) {
	steps, err := s.repo.ListSteps(ctx, tx, campaign.ID)
	if err != nil {
		return 0, err
	}

	// A returning participant of a daily webinar is not a new lead. The queue's
	// own unique constraint already stops the same run being scheduled twice;
	// this is the case it cannot see, where a restart has deliberately opened a
	// new run and the daily sequence would be recreated on a later webinar.
	//
	// The lookup only happens on a restart of a recurring campaign. A first
	// enrolment — every contact who has just arrived — has no earlier run to
	// examine and pays nothing for this.
	dailyConsumed := false
	if campaign.IsDailyRecurring && enrollment.RunNumber > 1 {
		previous, err := s.jobs.JobsForEnrollment(ctx, tx, enrollment.ID)
		if err != nil {
			return 0, err
		}
		dailyConsumed = DailySequenceConsumed(campaign, steps, previous, enrollment.RunNumber)
		if dailyConsumed {
			s.log.Info("DAILY_SEQUENCE_SKIPPED",
				slog.String("campaign_id", campaign.ID.String()),
				slog.String("campaign_name", campaign.Name),
				slog.String("enrollment_id", enrollment.ID.String()),
				slog.String("contact_id", enrollment.ContactID.String()),
				slog.Int("run_number", enrollment.RunNumber),
				slog.Int("daily_steps", len(DailySequence(steps))),
				slog.String("reason", "contact has already received this campaign's daily webinar sequence"))
		}
	}

	now := time.Now().UTC()
	plan := BuildPlan(EventAnchor(campaign, enrollment), steps, PlanOptions{
		EnrolledAt:            at,
		Now:                   now,
		CatchUp:               campaign.CatchUpMissedSteps,
		TriggerDelay:          s.triggerDelay,
		DailySequenceConsumed: dailyConsumed,
	})

	jobs := make([]scheduler.NewJob, 0, len(plan))
	live := 0
	for _, entry := range plan {
		runAt := entry.RunAt
		if entry.Skipped && runAt.IsZero() {
			// A step with no derivable time still needs one on the row: the
			// column is NOT NULL and the panel sorts the queue by it.
			runAt = now
		}
		if !entry.Skipped {
			live++
		}
		jobs = append(jobs, scheduler.NewJob{
			CampaignID:   campaign.ID,
			ContactID:    enrollment.ContactID,
			EnrollmentID: enrollment.ID,
			StepID:       entry.Step.ID,
			RunNumber:    enrollment.RunNumber,
			ScheduledAt:  runAt,
			SkipReason:   entry.SkipCode,
		})
	}

	if _, err := s.jobs.Enqueue(ctx, tx, jobs); err != nil {
		return 0, err
	}

	for _, entry := range plan {
		if entry.Skipped {
			continue
		}
		s.log.Info("JOB_CREATED",
			slog.String("campaign_id", campaign.ID.String()),
			slog.String("enrollment_id", enrollment.ID.String()),
			slog.String("campaign_step_id", entry.Step.ID.String()),
			slog.String("contact_id", enrollment.ContactID.String()),
			slog.Time("scheduled_at", entry.RunAt),
			slog.String("source", "enrollment"))
	}

	// Report the jobs that will actually be delivered. Skips are recorded, not
	// scheduled, and counting them would tell the operator a contact is getting
	// messages that were explicitly declined.
	return live, nil
}

// -------------------------------------------------------------- lifecycle --

// SaveInput is the validated payload for creating or updating a campaign.
type SaveInput struct {
	Name        string
	Description string
	EventType   string
	EventDate   string
	EventTime   string
	Timezone    string
	WebinarLink string
	// IsDailyRecurring turns the single webinar into a daily one. EventDate
	// then means the day the series starts, and RecurrenceTime the hour it
	// happens at.
	IsDailyRecurring bool
	// RecurrenceTime is "HH:MM" local to Timezone. Empty falls back to
	// EventTime, so an operator who only ticks the box gets the hour they had
	// already chosen.
	RecurrenceTime string
	// RecurrenceStartDate is "YYYY-MM-DD" local to Timezone. Empty falls back
	// to EventDate.
	RecurrenceStartDate     string
	ExistingContactBehavior string
	ExistingContactTemplate *uuid.UUID
	UnsubscribeKeywords     []string
	CatchUpMissedSteps      bool
	MaxSendAttempts         int
	ResumePolicy            string
	PinTemplateVersion      bool
	MaxMessagesPerHour      *int
	MaxMessagesPerDay       *int
	MaxActiveContacts       *int
}

func (in SaveInput) toCampaign() (*domain.Campaign, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, errors.New("кампания атауы міндетті")
	}

	tz := strings.TrimSpace(in.Timezone)
	if tz == "" {
		tz = "Asia/Almaty"
	}
	if _, err := timex.Location(tz); err != nil {
		return nil, fmt.Errorf("уақыт белдеуі жарамсыз: %s", tz)
	}

	c := &domain.Campaign{
		Name:                    name,
		Description:             strings.TrimSpace(in.Description),
		EventType:               defaultIfEmpty(in.EventType, "WEBINAR"),
		Timezone:                tz,
		WebinarLink:             strings.TrimSpace(in.WebinarLink),
		ExistingContactBehavior: domain.ExistingContactBehavior(defaultIfEmpty(in.ExistingContactBehavior, string(domain.BehaviorIgnore))),
		ExistingContactTemplate: in.ExistingContactTemplate,
		CatchUpMissedSteps:      in.CatchUpMissedSteps,
		MaxSendAttempts:         in.MaxSendAttempts,
		ResumePolicy:            domain.ResumePolicy(defaultIfEmpty(in.ResumePolicy, string(domain.ResumeSkipExpired))),
		PinTemplateVersion:      in.PinTemplateVersion,
		MaxMessagesPerHour:      in.MaxMessagesPerHour,
		MaxMessagesPerDay:       in.MaxMessagesPerDay,
		MaxActiveContacts:       in.MaxActiveContacts,
	}

	if !domain.ValidExistingBehavior(string(c.ExistingContactBehavior)) {
		return nil, fmt.Errorf("қайталанған триггер режимі жарамсыз: %s", c.ExistingContactBehavior)
	}
	if !domain.ValidResumePolicy(string(c.ResumePolicy)) {
		return nil, fmt.Errorf("жалғастыру саясаты жарамсыз: %s", c.ResumePolicy)
	}
	if c.MaxSendAttempts < 1 || c.MaxSendAttempts > 20 {
		c.MaxSendAttempts = 5
	}
	for _, limit := range []struct {
		value *int
		name  string
	}{
		{in.MaxMessagesPerHour, "сағаттық шек"},
		{in.MaxMessagesPerDay, "тәуліктік шек"},
		{in.MaxActiveContacts, "белсенді клиент шегі"},
	} {
		if limit.value != nil && *limit.value < 1 {
			return nil, fmt.Errorf("%s кемінде 1 болуы керек", limit.name)
		}
	}

	if strings.TrimSpace(in.EventDate) != "" {
		start, err := timex.ParseInLocation(in.EventDate, in.EventTime, tz)
		if err != nil {
			return nil, err
		}
		c.EventStartAt = &start
	}

	if err := applyRecurrence(c, in); err != nil {
		return nil, err
	}

	keywords := make([]string, 0, len(in.UnsubscribeKeywords))
	seen := map[string]bool{}
	for _, kw := range in.UnsubscribeKeywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		key := textnorm.Normalize(kw)
		if seen[key] {
			continue
		}
		seen[key] = true
		keywords = append(keywords, kw)
	}
	if len(keywords) == 0 {
		keywords = []string{"STOP", "ТОҚТАТУ", "СТОП"}
	}
	c.UnsubscribeKeywords = keywords

	return c, nil
}

// applyRecurrence resolves the daily-webinar settings onto the campaign.
//
// Two things matter here and both are about not breaking what exists.
//
// The date field keeps working. For a one-time campaign it is still the event
// date, untouched. For a recurring one it becomes the day the series starts,
// which is the same field answering the same question — "when does this
// webinar happen?" — one calendar step out. Nothing is removed from the form
// and no existing campaign's date changes meaning.
//
// event_start_at stays the series' own first occurrence rather than being
// advanced daily. A column that moved on its own would drag every pending job
// of every enrolment that has no occurrence pinned along with it, at midnight,
// unattended. The upcoming webinar is derived for display instead — see
// NextOccurrence — so the stored configuration can stay still while the day it
// resolves to moves.
func applyRecurrence(c *domain.Campaign, in SaveInput) error {
	if !in.IsDailyRecurring {
		// Switching recurrence off clears the settings but never touches the
		// occurrences already pinned on enrolments: those are messages people
		// are waiting for, and the operator asked to stop the series, not to
		// cancel the webinar that is already on its way.
		c.IsDailyRecurring = false
		c.RecurrenceTime = ""
		c.RecurrenceStartDate = ""
		return nil
	}

	clock := strings.TrimSpace(in.RecurrenceTime)
	if clock == "" {
		clock = strings.TrimSpace(in.EventTime)
	}
	startDate := strings.TrimSpace(in.RecurrenceStartDate)
	if startDate == "" {
		startDate = strings.TrimSpace(in.EventDate)
	}
	if startDate == "" && c.EventStartAt != nil {
		startDate = timex.FormatIn(*c.EventStartAt, c.Timezone, timex.DateLayout)
	}
	if startDate == "" {
		startDate = time.Now().In(timex.MustLocation(c.Timezone)).Format(timex.DateLayout)
	}

	c.IsDailyRecurring = true
	c.RecurrenceTime = clock
	c.RecurrenceStartDate = startDate

	// Validated on the server, always. The panel checks the same things, but a
	// request does not have to come from the panel.
	if err := (Recurrence{Enabled: true, Time: clock, StartDate: startDate, Zone: c.Timezone}).Validate(); err != nil {
		return err
	}

	// The event anchor becomes the series' first occurrence, so a recurring
	// campaign still satisfies the schema's "an ACTIVE campaign has an event"
	// rule and still has a sensible answer for an enrolment that predates the
	// toggle.
	start, err := timex.ParseInLocation(startDate, clock, c.Timezone)
	if err != nil {
		return err
	}
	c.EventStartAt = &start
	return nil
}

func (s *Service) Create(ctx context.Context, in SaveInput, adminID *uuid.UUID) (*domain.Campaign, error) {
	campaign, err := in.toCampaign()
	if err != nil {
		return nil, err
	}
	campaign.Status = domain.CampaignDraft
	campaign.CreatedBy = adminID

	if err := s.repo.Create(ctx, nil, campaign); err != nil {
		return nil, err
	}
	return s.repo.GetFull(ctx, campaign.ID)
}

// UpdateResult reports side effects the operator should be told about.
type UpdateResult struct {
	Campaign    *domain.Campaign `json:"campaign"`
	Rescheduled int64            `json:"rescheduled_jobs"`
	TimeChanged bool             `json:"event_time_changed"`
	// Rebased counts the enrolments whose upcoming webinar was moved because
	// the recurrence settings changed. Enrolments whose webinar has already
	// happened are never among them.
	Rebased int64 `json:"rebased_enrollments"`
}

// Update saves campaign settings and, when the event moves, recalculates every
// pending job.
//
// Already-sent messages are never touched: only PENDING rows shift. That is
// what makes moving a webinar from 21:00 to 20:00 safe an hour before it
// starts.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in SaveInput) (*UpdateResult, error) {
	updated, err := in.toCampaign()
	if err != nil {
		return nil, err
	}
	updated.ID = id

	result := &UpdateResult{}

	err = s.repo.DB().InTx(ctx, func(tx sqlite.Querier) error {
		existing, err := s.repo.GetByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if existing == nil {
			return ErrNotFound
		}

		if err := s.repo.Update(ctx, tx, updated); err != nil {
			return err
		}

		recurrenceMoved := recurrenceChanged(existing, updated)

		if !sameInstant(existing.EventStartAt, updated.EventStartAt) {
			result.TimeChanged = true
			// A campaign-wide reschedule assumes every job shares one anchor,
			// which is true of a one-time campaign and false of a recurring one:
			// there, each enrolment is anchored to its own webinar and a blanket
			// UPDATE would collapse them all onto the same day. Recurring
			// campaigns are brought in line by reconciliation instead, which
			// re-derives each job from that enrolment's own occurrence.
			if updated.EventStartAt != nil && !existing.IsDailyRecurring && !updated.IsDailyRecurring {
				n, err := s.jobs.RescheduleCampaign(ctx, tx, id, *updated.EventStartAt)
				if err != nil {
					return err
				}
				result.Rescheduled = n
			}
		}

		// The webinar time, zone or start date moved on a live series. Contacts
		// still waiting for their webinar follow it; contacts whose webinar has
		// already happened are history and are left exactly as they are.
		if updated.IsDailyRecurring && recurrenceMoved {
			now := time.Now().UTC()
			steps, err := s.repo.ListSteps(ctx, tx, id)
			if err != nil {
				return err
			}
			next, err := RecurrenceOf(updated).OccurrenceFor(now, StepTail(steps))
			if err != nil {
				return err
			}
			moved, err := s.repo.RebaseFutureOccurrences(ctx, tx, id, next, now)
			if err != nil {
				return err
			}
			result.Rebased = moved
			if moved > 0 {
				s.log.Info("RECURRING_OCCURRENCE_REBASED",
					slog.String("campaign_id", id.String()),
					slog.String("campaign_name", updated.Name),
					slog.String("occurrence_date", timex.FormatIn(next, updated.Timezone, timex.DateLayout)),
					slog.Time("webinar_datetime", next),
					slog.String("timezone", updated.Timezone),
					slog.Int64("enrollments_rebased", moved))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if result.TimeChanged {
		s.log.Info("campaign event time changed; pending jobs recalculated",
			slog.String("campaign_id", id.String()),
			slog.Int64("rescheduled", result.Rescheduled))
	}

	// RescheduleCampaign moves the jobs that exist. Reconciling afterwards
	// covers the ones that do not: moving an event later makes steps that were
	// skipped as expired viable again, and only a re-derivation from the steps
	// can notice that. For a recurring campaign it is also what actually moves
	// the queue, since the blanket reschedule above is deliberately skipped.
	stats := s.reconcileAfterChange(ctx, id, "campaign updated")
	if result.Rescheduled == 0 {
		result.Rescheduled = int64(stats.JobsMoved)
	}

	campaign, err := s.repo.GetFull(ctx, id)
	if err != nil {
		return nil, err
	}
	result.Campaign = campaign
	return result, nil
}

// Activate validates that a campaign is fit to go live before flipping it on.
//
// Resuming from PAUSED also settles the backlog that built up while it was
// off, according to the campaign's resume policy: a campaign paused at 18:00
// and resumed at 20:00 must not release the 18:00 and 19:00 messages at once.
func (s *Service) Activate(ctx context.Context, id uuid.UUID) (*domain.Campaign, error) {
	campaign, err := s.repo.GetFull(ctx, id)
	if err != nil {
		return nil, err
	}
	if campaign == nil {
		return nil, ErrNotFound
	}

	// Warnings are reported to the panel but never block: an operator who
	// wants to activate a campaign whose event time has passed is allowed to.
	if blocking := Blocking(Validate(campaign)); len(blocking) > 0 {
		return nil, ValidationError{Problems: blocking}
	}

	wasPaused := campaign.Status == domain.CampaignPaused

	err = s.repo.DB().InTx(ctx, func(tx sqlite.Querier) error {
		if err := s.repo.SetStatus(ctx, tx, id, domain.CampaignActive); err != nil {
			return err
		}
		if !wasPaused {
			return nil
		}
		return s.applyResumePolicy(ctx, tx, campaign)
	})
	if err != nil {
		return nil, err
	}

	// Going live is the moment the queue has to be right. A campaign activated
	// after its steps were built, or resumed after an edit during the pause,
	// gets its enrollments brought in line before any of them come due.
	s.reconcileAfterChange(ctx, id, "campaign activated")

	return s.repo.GetFull(ctx, id)
}

// applyResumePolicy decides what to do with jobs whose moment passed during a
// pause.
//
// The grace window matches the planner's, so a job that came due seconds
// before the operator hit resume is still treated as on time.
func (s *Service) applyResumePolicy(ctx context.Context, tx sqlite.Querier, campaign *domain.Campaign) error {
	now := time.Now().UTC()
	cutoff := now.Add(-time.Minute)

	var forwarded int64
	if campaign.ResumePolicy == domain.ResumeSendNextValid {
		// Keep one message of context per contact: their most recent overdue
		// step is moved to now, and the cancel below clears the rest.
		n, err := s.jobs.PullForwardLatestExpired(ctx, tx, campaign.ID, cutoff, now)
		if err != nil {
			return err
		}
		forwarded = n
	}

	skipped, err := s.jobs.ExpirePendingForCampaign(ctx, tx, campaign.ID, cutoff,
		"кампания кідіртілген кезде уақыты өтіп кетті")
	if err != nil {
		return err
	}

	if skipped > 0 || forwarded > 0 {
		s.log.Info("resume policy applied",
			slog.String("campaign_id", campaign.ID.String()),
			slog.String("policy", string(campaign.ResumePolicy)),
			slog.Int64("expired_cancelled", skipped),
			slog.Int64("pulled_forward", forwarded))
	}
	return nil
}

// SetStatus applies a lifecycle transition other than activation.
func (s *Service) SetStatus(ctx context.Context, id uuid.UUID, status domain.CampaignStatus) (*domain.Campaign, error) {
	if status == domain.CampaignActive {
		return s.Activate(ctx, id)
	}
	if !domain.ValidCampaignStatus(string(status)) {
		return nil, fmt.Errorf("мәртебе жарамсыз: %s", status)
	}
	if err := s.repo.SetStatus(ctx, nil, id, status); err != nil {
		return nil, err
	}
	return s.repo.GetFull(ctx, id)
}

// Duplicate clones a campaign with its steps and a fresh DRAFT status.
// Triggers are not copied: a keyword may only route to one campaign, so the
// operator picks new ones deliberately.
func (s *Service) Duplicate(ctx context.Context, id uuid.UUID, adminID *uuid.UUID) (*domain.Campaign, error) {
	src, err := s.repo.GetFull(ctx, id)
	if err != nil {
		return nil, err
	}
	if src == nil {
		return nil, ErrNotFound
	}

	var created *domain.Campaign
	err = s.repo.DB().InTx(ctx, func(tx sqlite.Querier) error {
		clone := *src
		clone.ID = uuid.Nil
		clone.Status = domain.CampaignDraft
		clone.CreatedBy = adminID
		clone.ArchivedAt = nil

		base := src.Name + " (көшірме)"
		clone.Name = base
		for attempt := 2; ; attempt++ {
			err := s.repo.Create(ctx, tx, &clone)
			if err == nil {
				break
			}
			if !errors.Is(err, ErrNameTaken) || attempt > 50 {
				return err
			}
			clone.Name = fmt.Sprintf("%s %d", base, attempt)
		}

		for _, step := range src.Steps {
			newStep := domain.CampaignStep{
				CampaignID:    clone.ID,
				Name:          step.Name,
				OffsetSeconds: step.OffsetSeconds,
				TemplateID:    step.TemplateID,
				Enabled:       step.Enabled,
				OrderIndex:    step.OrderIndex,
				ScheduleKind:  step.ScheduleKind,
				// Which steps form the daily webinar sequence is part of the
				// campaign's shape, not of one contact's history, so a copy is
				// the same campaign and keeps it.
				IncludeInDailyWebinar: step.IncludeInDailyWebinar,
			}
			if err := s.repo.CreateStep(ctx, tx, &newStep); err != nil {
				return err
			}
		}

		created = &clone
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.repo.GetFull(ctx, created.ID)
}

func (s *Service) Archive(ctx context.Context, id uuid.UUID) error { return s.repo.Archive(ctx, id) }

func (s *Service) Delete(ctx context.Context, id uuid.UUID) error { return s.repo.Delete(ctx, id) }

// ---------------------------------------------------------------- steps --

// StepInput is the validated payload for a campaign step.
type StepInput struct {
	Name          string
	OffsetSeconds int
	TemplateID    uuid.UUID
	Enabled       bool
	ScheduleKind  string
	// AudienceFilterEnabled limits this step to contacts who entered the
	// campaign at or after AudienceMinJoinedAt.
	AudienceFilterEnabled bool
	// AudienceMinJoinedAt is the cutoff in UTC. The handler converts it from
	// the operator's local date, time and zone before it gets here.
	AudienceMinJoinedAt *time.Time
	// IncludeInDailyWebinar marks this step as part of the campaign's daily
	// webinar sequence, which a contact receives exactly once.
	IncludeInDailyWebinar bool
}

func (in StepInput) validate() error {
	if in.TemplateID == uuid.Nil {
		return errors.New("шаблон таңдалуы керек")
	}
	// A cutoff that is switched on but never set would silently send to
	// everybody, which is the opposite of what the operator asked for. Refuse
	// it rather than guess.
	if in.AudienceFilterEnabled && in.AudienceMinJoinedAt == nil {
		return errors.New("аудитория шектеуі қосылған кезде қосылу уақытын көрсету міндетті")
	}
	kind := domain.ScheduleKind(defaultIfEmpty(in.ScheduleKind, string(domain.ScheduleRelativeToEvent)))
	if !domain.ValidScheduleKind(string(kind)) {
		return fmt.Errorf("қадам түрі жарамсыз: %s", kind)
	}
	// One year either side of the event covers every realistic drip campaign
	// and matches the database constraint.
	if in.OffsetSeconds < -31536000 || in.OffsetSeconds > 31536000 {
		return errors.New("уақыт ығысуы шектен тыс")
	}
	// A trigger-anchored offset is a delay after the customer wrote to us.
	// Time does not run backwards from that anchor, so a negative value is a
	// configuration mistake rather than a schedule.
	if kind == domain.ScheduleOnTrigger && in.OffsetSeconds < 0 {
		return errors.New("триггерден кейінгі кідіріс теріс болмауы керек")
	}
	return nil
}

func (s *Service) AddStep(ctx context.Context, campaignID uuid.UUID, in StepInput) (*domain.CampaignStep, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}

	step := &domain.CampaignStep{
		CampaignID:            campaignID,
		Name:                  strings.TrimSpace(in.Name),
		OffsetSeconds:         in.OffsetSeconds,
		TemplateID:            in.TemplateID,
		Enabled:               in.Enabled,
		ScheduleKind:          domain.ScheduleKind(defaultIfEmpty(in.ScheduleKind, string(domain.ScheduleRelativeToEvent))),
		AudienceFilterEnabled: in.AudienceFilterEnabled,
		AudienceMinJoinedAt:   in.AudienceMinJoinedAt,
		IncludeInDailyWebinar: in.IncludeInDailyWebinar,
	}

	err := s.repo.DB().InTx(ctx, func(tx sqlite.Querier) error {
		return s.repo.CreateStep(ctx, tx, step)
	})
	if err != nil {
		return nil, err
	}

	// Every contact already moving through this campaign gets the new step,
	// whether they are mid-funnel or were closed out before it existed. This is
	// the case the platform used to lose: a step added after enrollment reached
	// nobody, and nothing ever noticed.
	s.reconcileAfterChange(ctx, campaignID, "step added")
	return step, nil
}

// reconcileAfterChange brings every enrollment in line after the campaign's
// configuration changed.
//
// It runs after the change has committed, not inside its transaction. The
// reconciler takes one transaction per enrollment so a large campaign does not
// hold SQLite's single write lock for the whole sweep, and nesting that inside
// the caller's transaction would deadlock. The cost of committing first is that
// a crash in between leaves the steps updated and the queue briefly stale —
// which the periodic sweep repairs on its next pass. That is the whole reason
// the periodic sweep exists: correctness does not depend on this call
// succeeding.
func (s *Service) reconcileAfterChange(ctx context.Context, campaignID uuid.UUID, cause string) ReconcileStats {
	stats, err := s.ReconcileCampaign(ctx, campaignID)
	if err != nil {
		s.log.Error("reconciling campaign after change failed",
			slog.String("campaign_id", campaignID.String()),
			slog.String("cause", cause),
			slog.String("error", err.Error()))
		return stats
	}
	if stats.Changed() {
		s.log.Info("campaign reconciled",
			slog.String("campaign_id", campaignID.String()),
			slog.String("cause", cause),
			slog.Int("enrollments", stats.EnrollmentsChecked),
			slog.Int("jobs_created", stats.JobsCreated),
			slog.Int("jobs_moved", stats.JobsMoved),
			slog.Int("jobs_cancelled", stats.JobsCancelled),
			slog.Int("skips_recorded", stats.SkipsRecorded),
			slog.Int("enrollments_reopened", stats.EnrollmentsReopen))
	}
	return stats
}

// recurrenceChanged reports whether an edit moved a recurring series: the
// toggle, the hour, the zone or the day the series starts. A change to any
// other campaign setting must not disturb the webinars already assigned.
func recurrenceChanged(before, after *domain.Campaign) bool {
	return before.IsDailyRecurring != after.IsDailyRecurring ||
		before.RecurrenceTime != after.RecurrenceTime ||
		before.RecurrenceStartDate != after.RecurrenceStartDate ||
		before.Timezone != after.Timezone
}

// UpdateStep saves a step and reconciles the jobs already queued for it.
func (s *Service) UpdateStep(ctx context.Context, stepID uuid.UUID, in StepInput) (*domain.CampaignStep, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}

	step := &domain.CampaignStep{
		ID:                    stepID,
		Name:                  strings.TrimSpace(in.Name),
		OffsetSeconds:         in.OffsetSeconds,
		TemplateID:            in.TemplateID,
		Enabled:               in.Enabled,
		ScheduleKind:          domain.ScheduleKind(defaultIfEmpty(in.ScheduleKind, string(domain.ScheduleRelativeToEvent))),
		AudienceFilterEnabled: in.AudienceFilterEnabled,
		AudienceMinJoinedAt:   in.AudienceMinJoinedAt,
		IncludeInDailyWebinar: in.IncludeInDailyWebinar,
	}

	err := s.repo.DB().InTx(ctx, func(tx sqlite.Querier) error {
		previous, err := s.repo.GetStep(ctx, stepID)
		if err != nil {
			return err
		}
		if previous == nil {
			return ErrStepNotFound
		}
		step.CampaignID = previous.CampaignID
		return s.repo.UpdateStep(ctx, tx, step)
	})
	if err != nil {
		return nil, err
	}

	// One path for every kind of edit. The old code had a branch per case —
	// enable, disable, offset moved, anchor changed — and each branch could only
	// UPDATE rows that already existed. A step that had no job (because it was
	// added while its time was past, or because the contact had been closed out)
	// could therefore be edited into the future and still never acquire one.
	// Reconciling instead of patching removes that whole class: the queue is
	// re-derived from the step as it now stands, so a missing job is created and
	// an existing one is moved, without the caller having to know which.
	s.reconcileAfterChange(ctx, step.CampaignID, "step updated")
	return step, nil
}

func (s *Service) DeleteStep(ctx context.Context, stepID uuid.UUID) error {
	var campaignID uuid.UUID

	err := s.repo.DB().InTx(ctx, func(tx sqlite.Querier) error {
		step, err := s.repo.GetStep(ctx, stepID)
		if err != nil {
			return err
		}
		if step == nil {
			return ErrStepNotFound
		}
		campaignID = step.CampaignID

		if _, err := s.jobs.CancelPendingForStep(ctx, tx, stepID, "step deleted"); err != nil {
			return err
		}
		return s.repo.DeleteStep(ctx, stepID)
	})
	if err != nil {
		return err
	}

	// Deleting a step can be what finishes an enrollment: if it was the last
	// thing outstanding, the contact is now done and should be closed out.
	s.reconcileAfterChange(ctx, campaignID, "step deleted")
	return nil
}

// ReorderSteps changes presentation order only.
//
// order_index does not enter any scheduling arithmetic — a step's time comes
// from its offset and its anchor — so reordering cannot move a job. It still
// reconciles, because it is cheap and because "an admin touched this campaign"
// is exactly when a latent gap is worth catching.
func (s *Service) ReorderSteps(ctx context.Context, campaignID uuid.UUID, ordered []uuid.UUID) error {
	if err := s.repo.ReorderSteps(ctx, campaignID, ordered); err != nil {
		return err
	}
	s.reconcileAfterChange(ctx, campaignID, "steps reordered")
	return nil
}

// -------------------------------------------------------------- triggers --

func (s *Service) AddTrigger(ctx context.Context, campaignID uuid.UUID, keyword, matchMode string, active bool) (*domain.CampaignTrigger, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, errors.New("триггер мәтіні бос болмауы керек")
	}
	if matchMode == "" {
		matchMode = string(textnorm.MatchExact)
	}
	if !textnorm.ValidMatchMode(matchMode) {
		return nil, fmt.Errorf("сәйкестендіру режимі жарамсыз: %s", matchMode)
	}

	normalized := textnorm.Normalize(keyword)
	if normalized == "" {
		return nil, errors.New("триггер мәтіні жарамсыз")
	}
	// A very short CONTAINS keyword would fire on unrelated conversation.
	if textnorm.MatchMode(matchMode) == textnorm.MatchContains && len([]rune(normalized)) < 4 {
		return nil, errors.New("«құрамында» режимі үшін триггер кемінде 4 таңба болуы керек")
	}

	trigger := &domain.CampaignTrigger{
		CampaignID: campaignID,
		Keyword:    keyword,
		Normalized: normalized,
		MatchMode:  matchMode,
		IsActive:   active,
	}
	if err := s.repo.CreateTrigger(ctx, nil, trigger); err != nil {
		return nil, err
	}
	return trigger, nil
}

func (s *Service) UpdateTrigger(ctx context.Context, id uuid.UUID, keyword, matchMode string, active bool) (*domain.CampaignTrigger, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, errors.New("триггер мәтіні бос болмауы керек")
	}
	if !textnorm.ValidMatchMode(matchMode) {
		return nil, fmt.Errorf("сәйкестендіру режимі жарамсыз: %s", matchMode)
	}

	trigger := &domain.CampaignTrigger{
		ID:         id,
		Keyword:    keyword,
		Normalized: textnorm.Normalize(keyword),
		MatchMode:  matchMode,
		IsActive:   active,
	}
	if err := s.repo.UpdateTrigger(ctx, trigger); err != nil {
		return nil, err
	}
	return trigger, nil
}

func (s *Service) DeleteTrigger(ctx context.Context, id uuid.UUID) error {
	return s.repo.DeleteTrigger(ctx, id)
}

// --------------------------------------------------------------- preview --

// Preview renders the full automation timeline in the campaign's own timezone
// so the operator can verify it before activating.
//
// Rows come back in send order, which is what the operator is reasoning about.
// Trigger-anchored steps have no shared wall-clock time — each contact gets
// their own — so they are described by their delay and listed first.
func (s *Service) Preview(ctx context.Context, id uuid.UUID) ([]PreviewEntry, error) {
	campaign, err := s.repo.GetFull(ctx, id)
	if err != nil {
		return nil, err
	}
	if campaign == nil {
		return nil, ErrNotFound
	}

	warnings := map[string]string{}
	for _, problem := range Validate(campaign) {
		if problem.StepID != "" {
			if _, seen := warnings[problem.StepID]; !seen {
				warnings[problem.StepID] = problem.Message
			}
		}
	}

	// A recurring campaign is previewed against the webinar that is coming,
	// not against the day the series began. The operator is asking "what goes
	// out, and when?", and for a daily webinar the honest answer is today's or
	// tomorrow's occurrence.
	anchor := PreviewAnchor(campaign, time.Now().UTC())

	entries := make([]PreviewEntry, 0, len(campaign.Steps))
	for _, step := range campaign.Steps {
		entry := PreviewEntry{
			StepID:       step.ID.String(),
			OrderIndex:   step.OrderIndex,
			Name:         step.Name,
			ScheduleKind: string(step.ScheduleKind),
			Offset:       step.OffsetSeconds,
			OffsetLabel:  timex.HumanOffset(step.OffsetSeconds),
			TemplateID:   step.TemplateID.String(),
			TemplateName: step.TemplateName,
			TemplateType: string(step.TemplateType),
			Enabled:      step.Enabled,
			Warning:      warnings[step.ID.String()],
		}

		switch {
		case step.ScheduleKind == domain.ScheduleOnTrigger:
			delay := time.Duration(step.OffsetSeconds) * time.Second
			if delay < s.triggerDelay {
				delay = s.triggerDelay
			}
			entry.LocalTime = "триггерден кейін " + timex.HumanDuration(delay)
			entry.OffsetLabel = "+" + timex.HumanDuration(delay)
		case anchor == nil:
			if entry.Warning == "" {
				entry.Warning = "іс-шара уақыты белгіленбеген"
			}
		default:
			runAt := timex.Offset(*anchor, step.OffsetSeconds)
			entry.LocalDate = timex.FormatIn(runAt, campaign.Timezone, "02.01.2006")
			entry.LocalTime = timex.FormatIn(runAt, campaign.Timezone, "15:04:05")
			entry.UTCTime = runAt.UTC().Format(time.RFC3339)
		}

		entries = append(entries, entry)
	}

	sortPreview(entries)
	return entries, nil
}

// sortPreview orders the timeline the way it will actually run: trigger-time
// steps first, then everything anchored to the event in chronological order.
// Steps that cannot be placed keep their configured position at the end.
func sortPreview(entries []PreviewEntry) {
	rank := func(e PreviewEntry) int {
		switch {
		case e.ScheduleKind == string(domain.ScheduleOnTrigger):
			return 0
		case e.UTCTime != "":
			return 1
		default:
			return 2
		}
	}

	sort.SliceStable(entries, func(i, j int) bool {
		ri, rj := rank(entries[i]), rank(entries[j])
		if ri != rj {
			return ri < rj
		}
		switch ri {
		case 0:
			return entries[i].Offset < entries[j].Offset
		case 1:
			return entries[i].UTCTime < entries[j].UTCTime
		default:
			return entries[i].OrderIndex < entries[j].OrderIndex
		}
	})
}

// Validate reports everything standing between a campaign and activation.
func (s *Service) Validate(ctx context.Context, id uuid.UUID) (*domain.Campaign, []Problem, error) {
	campaign, err := s.repo.GetFull(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if campaign == nil {
		return nil, nil, ErrNotFound
	}
	return campaign, Validate(campaign), nil
}

// StopForContact ends every active enrollment and cancels pending jobs, used
// on unsubscribe and on block.
func (s *Service) StopForContact(ctx context.Context, contactID uuid.UUID, status domain.EnrollmentStatus, reason string) error {
	return s.repo.DB().InTx(ctx, func(tx sqlite.Querier) error {
		if err := s.repo.StopAllForContact(ctx, tx, contactID, status, reason); err != nil {
			return err
		}
		_, err := s.jobs.CancelPendingForContact(ctx, tx, contactID, reason)
		return err
	})
}

// CompleteFinishedEnrollments closes enrollments whose steps have all resolved.
//
// The previous implementation asked only "are there any PENDING or PROCESSING
// jobs left?" and closed the enrollment when there were none. That question has
// two very different answers behind one result: a contact who received
// everything, and a contact whose jobs were never created. It could not tell
// them apart, because it never looked at campaign_steps.
//
// In production it closed a contact at 10:42 who had received 2 of what became
// 5 steps — and because the back-fill for new steps only considered ACTIVE
// enrollments, being COMPLETED then hid that contact from the very mechanism
// that would have given them the missing three.
//
// Completion is now derived from the steps, which is the only place it can
// honestly come from, and it is derived by the same pass that repairs the
// queue: an enrollment cannot be closed while a step is unaccounted for,
// because reconciliation creates the missing job first and then finds it
// unfinished.
func (s *Service) CompleteFinishedEnrollments(ctx context.Context) (int64, error) {
	stats, err := s.ReconcileAll(ctx)
	if err != nil {
		return 0, err
	}
	return int64(stats.EnrollmentsDone), nil
}

func sameInstant(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}

func defaultIfEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.TrimSpace(v)
}
