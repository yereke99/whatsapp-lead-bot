package campaigns

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

// Problem is one reason a campaign is not ready, or one thing worth a second
// look before it goes live.
//
// Blocking problems stop activation. Warnings do not: a campaign can be
// perfectly valid and still be worth questioning, and an operator who knows
// what they are doing should not be argued with.
type Problem struct {
	Field    string `json:"field"`
	Message  string `json:"message"`
	Blocking bool   `json:"blocking"`
	StepID   string `json:"step_id,omitempty"`
}

// ValidationError carries the blocking problems back to the API layer so the
// operator sees every one of them at once rather than one per attempt.
type ValidationError struct {
	Problems []Problem
}

func (e ValidationError) Error() string {
	messages := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		messages = append(messages, p.Message)
	}
	return strings.Join(messages, "; ")
}

// Blocking returns only the problems that prevent activation.
func Blocking(problems []Problem) []Problem {
	out := make([]Problem, 0, len(problems))
	for _, p := range problems {
		if p.Blocking {
			out = append(out, p)
		}
	}
	return out
}

// Validate checks a fully loaded campaign — steps and triggers included —
// against everything that must hold before it can start sending.
//
// It is pure so the same answers back both the "can this activate" gate and
// the checklist the panel shows while the campaign is still a draft.
func Validate(campaign *domain.Campaign) []Problem {
	var problems []Problem

	add := func(field, message string, blocking bool) {
		problems = append(problems, Problem{Field: field, Message: message, Blocking: blocking})
	}

	if _, err := timex.Location(campaign.Timezone); err != nil {
		add("timezone", fmt.Sprintf("Уақыт белдеуі жарамсыз: %s", campaign.Timezone), true)
	}

	activeTriggers := 0
	for _, t := range campaign.Triggers {
		if t.IsActive {
			activeTriggers++
		}
	}
	if activeTriggers == 0 {
		add("triggers", "Кампанияда белсенді триггер жоқ — клиенттер воронкаға кіре алмайды", true)
	}

	enabled := make([]domain.CampaignStep, 0, len(campaign.Steps))
	for _, step := range campaign.Steps {
		if step.Enabled {
			enabled = append(enabled, step)
		}
	}
	if len(enabled) == 0 {
		add("steps", "Кампанияда кемінде бір қосулы хабарлама болуы керек", true)
	}

	needsEvent := false
	for _, step := range enabled {
		label := stepLabel(step)

		if step.TemplateID == uuid.Nil {
			problems = append(problems, Problem{
				Field: "steps", StepID: step.ID.String(), Blocking: true,
				Message: fmt.Sprintf("«%s» қадамында шаблон таңдалмаған", label),
			})
		}
		if step.TemplateType != "" && step.TemplateType.RequiresMedia() && !step.TemplateHasMedia {
			problems = append(problems, Problem{
				Field: "steps", StepID: step.ID.String(), Blocking: true,
				Message: fmt.Sprintf("«%s» қадамының шаблонына медиафайл тіркелмеген", label),
			})
		}
		if step.TemplateArchived {
			problems = append(problems, Problem{
				Field: "steps", StepID: step.ID.String(), Blocking: true,
				Message: fmt.Sprintf("«%s» қадамы мұрағатталған шаблонды пайдаланады", label),
			})
		}

		switch step.ScheduleKind {
		case domain.ScheduleOnTrigger:
			if step.OffsetSeconds < 0 {
				problems = append(problems, Problem{
					Field: "steps", StepID: step.ID.String(), Blocking: true,
					Message: fmt.Sprintf("«%s»: триггерден кейінгі кідіріс теріс болмауы керек", label),
				})
			}
		default:
			needsEvent = true
		}
	}

	if needsEvent && campaign.EventStartAt == nil {
		add("event_start_at",
			"Іс-шара күні мен уақыты белгіленбеген — нақты уақытқа байланған қадамдарды жоспарлау мүмкін емес", true)
	}

	problems = append(problems, scheduleConflicts(campaign, enabled)...)

	if campaign.ExistingContactBehavior == domain.BehaviorSpecialMessage &&
		campaign.ExistingContactTemplate == nil {
		add("existing_contact_template_id",
			"«Арнайы жауап» режимі таңдалған, бірақ жауап шаблоны көрсетілмеген", true)
	}

	if campaign.EventStartAt != nil && campaign.EventStartAt.Before(time.Now().UTC()) {
		add("event_start_at", "Іс-шара уақыты өтіп кеткен", false)
	}
	if campaign.WebinarLink == "" && usesWebinarLink(campaign.Steps) {
		add("webinar_link",
			"Шаблондарда {{webinar_link}} қолданылған, бірақ кампанияда сілтеме енгізілмеген", false)
	}

	return problems
}

// scheduleConflicts reports steps that would arrive at the same moment, and
// steps whose configured order contradicts the order they will actually be
// sent in.
//
// Neither blocks activation: the queue is well defined either way. They are
// reported because a queue that reads 20:00, 19:00, 21:00 in the panel is
// almost always a mistake, and the operator should see it before customers do.
func scheduleConflicts(campaign *domain.Campaign, enabled []domain.CampaignStep) []Problem {
	if campaign.EventStartAt == nil {
		return nil
	}

	type placed struct {
		step  domain.CampaignStep
		runAt time.Time
	}

	timed := make([]placed, 0, len(enabled))
	for _, step := range enabled {
		if step.ScheduleKind == domain.ScheduleOnTrigger {
			continue
		}
		timed = append(timed, placed{step: step, runAt: timex.Offset(*campaign.EventStartAt, step.OffsetSeconds)})
	}

	var problems []Problem

	byOrder := make([]placed, len(timed))
	copy(byOrder, timed)
	sort.SliceStable(byOrder, func(i, j int) bool { return byOrder[i].step.OrderIndex < byOrder[j].step.OrderIndex })

	for i := 1; i < len(byOrder); i++ {
		if byOrder[i].runAt.Before(byOrder[i-1].runAt) {
			problems = append(problems, Problem{
				Field: "steps", StepID: byOrder[i].step.ID.String(), Blocking: false,
				Message: fmt.Sprintf("«%s» тізімде «%s» қадамынан кейін тұр, бірақ одан бұрын жіберіледі",
					stepLabel(byOrder[i].step), stepLabel(byOrder[i-1].step)),
			})
		}
	}

	sort.SliceStable(timed, func(i, j int) bool { return timed[i].runAt.Before(timed[j].runAt) })
	for i := 1; i < len(timed); i++ {
		if timed[i].runAt.Equal(timed[i-1].runAt) {
			problems = append(problems, Problem{
				Field: "steps", StepID: timed[i].step.ID.String(), Blocking: false,
				Message: fmt.Sprintf("«%s» және «%s» бір уақытта жіберіледі (%s)",
					stepLabel(timed[i-1].step), stepLabel(timed[i].step),
					timex.FormatIn(timed[i].runAt, campaign.Timezone, "02.01 15:04:05")),
			})
		}
	}

	return problems
}

func usesWebinarLink(steps []domain.CampaignStep) bool {
	for _, step := range steps {
		if strings.Contains(step.TemplatePreview, "{{webinar_link}}") {
			return true
		}
	}
	return false
}

func stepLabel(step domain.CampaignStep) string {
	if name := strings.TrimSpace(step.Name); name != "" {
		return name
	}
	if step.TemplateName != "" {
		return step.TemplateName
	}
	return timex.HumanOffset(step.OffsetSeconds)
}
