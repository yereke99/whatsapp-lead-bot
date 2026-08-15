package campaigns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

type Service struct {
	repo     *Repository
	jobs     *scheduler.Repository
	contacts *contacts.Repository
	log      *slog.Logger
}

func NewService(repo *Repository, jobs *scheduler.Repository, contactRepo *contacts.Repository, log *slog.Logger) *Service {
	return &Service{
		repo:     repo,
		jobs:     jobs,
		contacts: contactRepo,
		log:      log.With(slog.String("component", "campaigns")),
	}
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

		enrollment := &domain.Enrollment{
			CampaignID:     campaign.ID,
			ContactID:      contact.ID,
			TriggerID:      &match.TriggerID,
			TriggerKeyword: match.Keyword,
		}
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
		restarted, err := s.repo.RestartEnrollment(ctx, tx, existing.ID, match.Keyword)
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

// scheduleEnrollment turns the campaign's steps into persisted jobs.
func (s *Service) scheduleEnrollment(ctx context.Context, tx sqlite.Querier, campaign *domain.Campaign, enrollment *domain.Enrollment, at time.Time) (int, error) {
	steps, err := s.repo.ListSteps(ctx, tx, campaign.ID)
	if err != nil {
		return 0, err
	}

	plan := BuildPlan(campaign.EventStartAt, steps, PlanOptions{
		EnrolledAt: at,
		Now:        at,
		CatchUp:    campaign.CatchUpMissedSteps,
	})

	jobs := make([]scheduler.NewJob, 0, len(plan))
	for _, entry := range Scheduled(plan) {
		jobs = append(jobs, scheduler.NewJob{
			CampaignID:   campaign.ID,
			ContactID:    enrollment.ContactID,
			EnrollmentID: enrollment.ID,
			StepID:       entry.Step.ID,
			RunNumber:    enrollment.RunNumber,
			ScheduledAt:  entry.RunAt,
		})
	}

	return s.jobs.Enqueue(ctx, tx, jobs)
}

// -------------------------------------------------------------- lifecycle --

// SaveInput is the validated payload for creating or updating a campaign.
type SaveInput struct {
	Name                    string
	Description             string
	EventType               string
	EventDate               string
	EventTime               string
	Timezone                string
	WebinarLink             string
	ExistingContactBehavior string
	ExistingContactTemplate *uuid.UUID
	UnsubscribeKeywords     []string
	CatchUpMissedSteps      bool
	MaxSendAttempts         int
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
	}

	if !domain.ValidExistingBehavior(string(c.ExistingContactBehavior)) {
		return nil, fmt.Errorf("қайталанған триггер режимі жарамсыз: %s", c.ExistingContactBehavior)
	}
	if c.MaxSendAttempts < 1 || c.MaxSendAttempts > 20 {
		c.MaxSendAttempts = 5
	}

	if strings.TrimSpace(in.EventDate) != "" {
		start, err := timex.ParseInLocation(in.EventDate, in.EventTime, tz)
		if err != nil {
			return nil, err
		}
		c.EventStartAt = &start
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

		if !sameInstant(existing.EventStartAt, updated.EventStartAt) {
			result.TimeChanged = true
			if updated.EventStartAt != nil {
				n, err := s.jobs.RescheduleCampaign(ctx, tx, id, *updated.EventStartAt)
				if err != nil {
					return err
				}
				result.Rescheduled = n
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

	campaign, err := s.repo.GetFull(ctx, id)
	if err != nil {
		return nil, err
	}
	result.Campaign = campaign
	return result, nil
}

// Activate validates that a campaign is fit to go live before flipping it on.
func (s *Service) Activate(ctx context.Context, id uuid.UUID) (*domain.Campaign, error) {
	campaign, err := s.repo.GetFull(ctx, id)
	if err != nil {
		return nil, err
	}
	if campaign == nil {
		return nil, ErrNotFound
	}
	if campaign.EventStartAt == nil {
		return nil, ErrNoEventStart
	}

	enabled := 0
	for _, step := range campaign.Steps {
		if step.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		return nil, ErrNoEnabledSteps
	}
	if len(campaign.Triggers) == 0 {
		return nil, errors.New("кампанияда белсенді триггер жоқ")
	}

	if err := s.repo.SetStatus(ctx, nil, id, domain.CampaignActive); err != nil {
		return nil, err
	}
	return s.repo.GetFull(ctx, id)
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
}

func (in StepInput) validate() error {
	if in.TemplateID == uuid.Nil {
		return errors.New("шаблон таңдалуы керек")
	}
	kind := domain.ScheduleKind(defaultIfEmpty(in.ScheduleKind, string(domain.ScheduleRelativeToEvent)))
	if kind != domain.ScheduleRelativeToEvent && kind != domain.ScheduleOnTrigger {
		return fmt.Errorf("қадам түрі жарамсыз: %s", kind)
	}
	// One year either side of the event covers every realistic drip campaign
	// and matches the database constraint.
	if in.OffsetSeconds < -31536000 || in.OffsetSeconds > 31536000 {
		return errors.New("уақыт ығысуы шектен тыс")
	}
	return nil
}

func (s *Service) AddStep(ctx context.Context, campaignID uuid.UUID, in StepInput) (*domain.CampaignStep, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}

	step := &domain.CampaignStep{
		CampaignID:    campaignID,
		Name:          strings.TrimSpace(in.Name),
		OffsetSeconds: in.OffsetSeconds,
		TemplateID:    in.TemplateID,
		Enabled:       in.Enabled,
		ScheduleKind:  domain.ScheduleKind(defaultIfEmpty(in.ScheduleKind, string(domain.ScheduleRelativeToEvent))),
	}

	err := s.repo.DB().InTx(ctx, func(tx sqlite.Querier) error {
		if err := s.repo.CreateStep(ctx, tx, step); err != nil {
			return err
		}
		// Back-fill the new step for contacts already moving through the
		// campaign, so a step added mid-flight is not silently missed.
		if step.Enabled {
			return s.backfillStep(ctx, tx, campaignID, step)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return step, nil
}

// UpdateStep saves a step and reconciles the jobs already queued for it.
func (s *Service) UpdateStep(ctx context.Context, stepID uuid.UUID, in StepInput) (*domain.CampaignStep, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}

	step := &domain.CampaignStep{
		ID:            stepID,
		Name:          strings.TrimSpace(in.Name),
		OffsetSeconds: in.OffsetSeconds,
		TemplateID:    in.TemplateID,
		Enabled:       in.Enabled,
		ScheduleKind:  domain.ScheduleKind(defaultIfEmpty(in.ScheduleKind, string(domain.ScheduleRelativeToEvent))),
	}

	err := s.repo.DB().InTx(ctx, func(tx sqlite.Querier) error {
		previous, err := s.repo.GetStep(ctx, stepID)
		if err != nil {
			return err
		}
		if previous == nil {
			return ErrStepNotFound
		}

		if err := s.repo.UpdateStep(ctx, tx, step); err != nil {
			return err
		}

		switch {
		case previous.Enabled && !step.Enabled:
			_, err = s.jobs.CancelPendingForStep(ctx, tx, stepID, "step disabled")
			return err

		case !previous.Enabled && step.Enabled:
			return s.backfillStep(ctx, tx, step.CampaignID, step)

		case previous.OffsetSeconds != step.OffsetSeconds:
			_, err = s.jobs.RescheduleStep(ctx, tx, stepID)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return step, nil
}

// backfillStep enqueues a newly enabled step for every active enrollment.
func (s *Service) backfillStep(ctx context.Context, tx sqlite.Querier, campaignID uuid.UUID, step *domain.CampaignStep) error {
	campaign, err := s.repo.GetByID(ctx, tx, campaignID)
	if err != nil {
		return err
	}
	if campaign == nil || campaign.EventStartAt == nil {
		return nil
	}

	enrollments, err := s.repo.ActiveEnrollmentIDs(ctx, tx, campaignID)
	if err != nil {
		return err
	}
	if len(enrollments) == 0 {
		return nil
	}

	now := time.Now().UTC()
	runAt := campaign.EventStartAt.Add(time.Duration(step.OffsetSeconds) * time.Second)
	if step.ScheduleKind == domain.ScheduleOnTrigger {
		// A trigger-time step added later has no meaningful past anchor, so it
		// is not back-filled at all.
		return nil
	}
	if runAt.Before(now) {
		// Do not resurrect a moment that has already passed for existing
		// contacts; the plan builder handles catch-up only at enrollment.
		return nil
	}

	jobs := make([]scheduler.NewJob, 0, len(enrollments))
	for _, e := range enrollments {
		jobs = append(jobs, scheduler.NewJob{
			CampaignID:   campaignID,
			ContactID:    e.ContactID,
			EnrollmentID: e.ID,
			StepID:       step.ID,
			RunNumber:    e.RunNumber,
			ScheduledAt:  runAt,
		})
	}

	created, err := s.jobs.Enqueue(ctx, tx, jobs)
	if err != nil {
		return err
	}
	s.log.Info("step back-filled for active contacts",
		slog.String("step_id", step.ID.String()),
		slog.Int("jobs_created", created))
	return nil
}

func (s *Service) DeleteStep(ctx context.Context, stepID uuid.UUID) error {
	return s.repo.DB().InTx(ctx, func(tx sqlite.Querier) error {
		if _, err := s.jobs.CancelPendingForStep(ctx, tx, stepID, "step deleted"); err != nil {
			return err
		}
		return s.repo.DeleteStep(ctx, stepID)
	})
}

func (s *Service) ReorderSteps(ctx context.Context, campaignID uuid.UUID, ordered []uuid.UUID) error {
	return s.repo.ReorderSteps(ctx, campaignID, ordered)
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
func (s *Service) Preview(ctx context.Context, id uuid.UUID) ([]PreviewEntry, error) {
	campaign, err := s.repo.GetFull(ctx, id)
	if err != nil {
		return nil, err
	}
	if campaign == nil {
		return nil, ErrNotFound
	}

	entries := make([]PreviewEntry, 0, len(campaign.Steps))
	for _, step := range campaign.Steps {
		entry := PreviewEntry{
			StepID:       step.ID.String(),
			Name:         step.Name,
			Offset:       step.OffsetSeconds,
			OffsetLabel:  timex.HumanOffset(step.OffsetSeconds),
			TemplateName: step.TemplateName,
			TemplateType: string(step.TemplateType),
			Enabled:      step.Enabled,
		}

		switch {
		case step.ScheduleKind == domain.ScheduleOnTrigger:
			entry.LocalTime = "триггер кезінде"
			entry.OffsetLabel = "бірден"
		case campaign.EventStartAt == nil:
			entry.Warning = "іс-шара уақыты белгіленбеген"
		default:
			runAt := campaign.EventStartAt.Add(time.Duration(step.OffsetSeconds) * time.Second)
			entry.LocalTime = timex.FormatIn(runAt, campaign.Timezone, "2006-01-02 15:04:05")
			entry.UTCTime = runAt.UTC().Format(time.RFC3339)
		}

		entries = append(entries, entry)
	}
	return entries, nil
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

// CompleteFinishedEnrollments closes enrollments whose jobs have all resolved.
func (s *Service) CompleteFinishedEnrollments(ctx context.Context) (int64, error) {
	const query = `
		UPDATE campaign_contacts cc SET
			status = 'COMPLETED', completed_at = now()
		WHERE cc.status = 'ACTIVE'
		  AND EXISTS (SELECT 1 FROM scheduled_messages sm WHERE sm.enrollment_id = cc.id)
		  AND NOT EXISTS (
			SELECT 1 FROM scheduled_messages sm
			WHERE sm.enrollment_id = cc.id AND sm.status IN ('PENDING', 'PROCESSING')
		  )`

	tag, err := s.repo.DB().Exec(ctx, query)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
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
