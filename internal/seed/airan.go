// Package seed installs the default Airan campaign on a fresh installation.
//
// It runs once, at startup, and only when the database contains no campaigns at
// all. A database that already has campaigns in it is never touched: not to add
// a missing step, not to fix a trigger, not to update a template. An operator's
// database belongs to the operator, and a process that rewrites it on every
// boot is a process nobody can trust with production data.
//
// That restraint is the whole design. "Seed an empty database" is a decision
// that can be verified once and then reasoned about forever; "reconcile the
// campaign towards a definition baked into the binary" would mean every deploy
// could silently change what customers receive.
//
// # Media, and why every template is seeded as TEXT
//
// Six of the nine steps are meant to go out as voice notes or captioned
// images. None of them can be created that way here, and the schema is explicit
// about why:
//
//	message_templates_payload_check   a non-TEXT template must reference a
//	                                  media file; NULL is rejected
//	message_templates_caption_check   a VOICE template must have an empty body
//
// So a VOICE template cannot be created without an audio file, and it could not
// carry this copy even if it could — the text of a voice step is the script to
// record, not a caption, and WhatsApp voice notes have no caption.
//
// Inventing a placeholder audio file to satisfy the constraint would mean a
// real customer eventually receives the placeholder. Instead each template is
// created as TEXT holding the exact copy, with its intended final type recorded
// in the description. The campaign is therefore complete and sendable from the
// first boot, and the operator uploads the real assets and switches the type in
// the admin panel when they are ready.
package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/templates"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

// CampaignName identifies the seeded campaign. It is also the idempotency key
// for the CLI seeder, so it must not change once shipped.
const CampaignName = "Airan"

// TriggerPhrase is the exact message a lead sends to enter the funnel.
//
// It is a full sentence rather than a word because an opt-in should be
// unambiguous. "АЙРАН" — what this used to be — is what somebody types when
// they are talking about ayran, not when they are asking to join a webinar;
// this sentence is what the ads and landing page tell people to send, and
// nobody writes it by accident.
//
// Matching is EXACT against the normalized form, and normalization case-folds
// and collapses whitespace, so the sentence still matches whatever mixture of
// capitals and stray spaces the contact's keyboard produces. Nothing fuzzy is
// involved: they either sent this sentence or they did not.
//
// Migration 0003 moves existing databases onto the same phrase, so the two must
// stay in step.
const TriggerPhrase = "Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді"

// WebinarLink is the default room. It lives on the campaign rather than inside
// the copy, and the templates reach it through {{webinar_link}}, so changing
// the room is one edit instead of nine.
const WebinarLink = "https://bizon365.online/room/209010/ayrankaimaq"

// WebinarTime is the daily start, interpreted in the campaign's timezone.
const WebinarTime = "21:00"

// Step is one entry of the funnel: when it fires, and the copy it sends.
type Step struct {
	Name         string
	OffsetSec    int
	ScheduleKind domain.ScheduleKind
	TemplateName string
	// IntendedType is the type this template should end up as once its media is
	// uploaded. It is recorded in the template description rather than applied,
	// because the schema will not accept a media type without a media file.
	IntendedType string
	// DailyWebinar marks the step as part of the daily webinar sequence: the
	// messages a contact receives once, for the webinar they signed up for.
	//
	// Every reminder anchored to the event is in it; the greeting is not,
	// because it answers the contact's own message rather than the schedule.
	// The campaign is seeded as a one-time webinar, so this changes nothing
	// until an operator turns on "repeats every day" — at which point the
	// sequence is already described correctly instead of being empty.
	DailyWebinar bool
	Body         string
}

// Steps is the Airan funnel, in send order.
//
// The offsets are signed seconds from the webinar start, which is what lets one
// definition serve a webinar at any hour: -18000 is "five hours before", not
// "16:00". Only the greeting is anchored to the contact instead of the event,
// because it answers their message rather than the schedule.
func Steps() []Step {
	return []Step{
		{
			Name:         "Триггер: танысу",
			OffsetSec:    0,
			ScheduleKind: domain.ScheduleOnTrigger,
			TemplateName: "01. Airan — триггер, таныстыру",
			IntendedType: "VOICE",
			Body: "Сәлеметсіз бе! Есімім Әлішер, Айран/Қаймақ кәсібін өте аз суммамен бастап, " +
				"күніне кемі 40 мың теңге таза табуға болатынын бүгін сағат {{webinar_time}}-де өтетін " +
				"тегін сабағымызда түсіндіретін боламыз! Станок, рецепт, шикі заттарға басыңызды " +
				"қатырмасаңыз болады! Барлығын өзіміз береміз. Ал сабаққа қалай қатысамын десеңіз, " +
				"сабақ жақындағанда осы чатқа сілтеме жіберемін, сол сілтеме арқылы сабаққа қатысасыз. " +
				"Орындар саны шектеулі, сілтеме жібергенде кіріп орын алып қалуға асығыңыз!",
		},
		{
			Name:         "5 сағат бұрын",
			OffsetSec:    -5 * 3600,
			ScheduleKind: domain.ScheduleRelativeToEvent,
			DailyWebinar: true,
			TemplateName: "02. Airan — 5 сағат бұрын",
			IntendedType: "IMAGE_WITH_CAPTION",
			Body: "Осы жылдың соңғы тегін сабағы! Бұдан кейін тегін сабақ өткізбейміз! " +
				"Бүгін сағат {{webinar_time}}-де өтетін соңғы мүмкіндікті жіберіп алмаңыз!\n\n" +
				"Түрік айраны мен қаймақ бизнесі арқылы күніне кемінде 40 мың теңге табудың жолын " +
				"меңгеріп, өткен қаржылық қиындықтарды артта қалдырып, жаңа кәсіппен және жаңа " +
				"табыспен алға қадам жасаңыз.\n\n" +
				"Сабақтың басталуына 5 сағат уақыт қалды.\n\n" +
				"Ал сабаққа қалай қатысамын десеңіз, сабақ жақындағанда осы чатқа сілтеме жіберемін, " +
				"сол сілтеме арқылы сабаққа қатысасыз.\n\n" +
				"Орындар саны шектеулі, сілтеме жібергенде кіріп орын алып қалуға асығыңыз!\n\n" +
				"{{webinar_link}}",
		},
		{
			Name:         "3 сағат бұрын",
			OffsetSec:    -3 * 3600,
			ScheduleKind: domain.ScheduleRelativeToEvent,
			DailyWebinar: true,
			TemplateName: "03. Airan — 3 сағат бұрын",
			IntendedType: "VOICE",
			Body: "Күніне кемінде 40 мың пайда алып келетін Түрік айраны мен Қаймақ кәсібін " +
				"ақтарып-төңкеріп түсіндіріп береміз. Ешқандай риск жоқ. Аз ақшамен бастап кетесіз. " +
				"Сабақтың басталуына 3 сағат уақыт қалды.\n\n" +
				"Ал сабаққа қалай қатысамын десеңіз, сабақ жақындағанда осы чатқа сілтеме жіберемін, " +
				"сол сілтеме арқылы сабаққа қатысасыз.\n\n" +
				"Орындар саны шектеулі, сілтеме жібергенде кіріп орын алып қалуға асығыңыз!",
		},
		{
			Name:         "2 сағат бұрын",
			OffsetSec:    -2 * 3600,
			ScheduleKind: domain.ScheduleRelativeToEvent,
			DailyWebinar: true,
			TemplateName: "04. Airan — 2 сағат бұрын",
			IntendedType: "IMAGE_WITH_CAPTION",
			Body: "Қаржылық жағдайыңыз тұрақсыз болып жүрсе, Түрік айраны мен Қаймақ кәсібі оны " +
				"тұрақты табыс көзіне айналдыруға мүмкіндік береді!\n\n" +
				"Сабақтың басталуына небәрі 2 сағат қалды!\n\n" +
				"Ал сабаққа қалай қатысамын десеңіз, сабақ жақындағанда осы чатқа сілтеме жіберемін, " +
				"сол сілтеме арқылы сабаққа қатысасыз.\n\n" +
				"Орындар саны шектеулі, сілтеме жібергенде кіріп орын алып қалуға асығыңыз!\n\n" +
				"{{webinar_link}}",
		},
		{
			Name:         "1 сағат бұрын",
			OffsetSec:    -3600,
			ScheduleKind: domain.ScheduleRelativeToEvent,
			DailyWebinar: true,
			TemplateName: "05. Airan — 1 сағат бұрын",
			IntendedType: "IMAGE_WITH_CAPTION",
			Body: "Бүгін сабақта Түрік айраны мен Қаймақ бизнесін нөлден бастап қалай бастауға " +
				"болатынын көрсетеміз.\n\n" +
				"Бұл сабақ сіздің қаржылық еркіндікке жасаған алғашқы қадамыңыз болуы мүмкін!\n\n" +
				"Сабақтың басталуына бар болғаны 1 сағат уақыт қалды.\n\n" +
				"Ал сабаққа қалай қатысамын десеңіз, сабақ жақындағанда осы чатқа сілтеме жіберемін, " +
				"сол сілтеме арқылы сабаққа қатысасыз.\n\n" +
				"Орындар саны шектеулі, сілтеме жібергенде кіріп орын алып қалуға асығыңыз!\n\n" +
				"{{webinar_link}}",
		},
		{
			Name:         "30 минут бұрын",
			OffsetSec:    -30 * 60,
			ScheduleKind: domain.ScheduleRelativeToEvent,
			DailyWebinar: true,
			TemplateName: "06. Airan — 30 минут бұрын",
			IntendedType: "VOICE",
			Body: "Бүгінгі сабақ — жай әңгіме емес.\n\n" +
				"30 минуттан кейін сабаққа қатысып, Айран Қаймақ кәсібін қалай аз ақшамен бастауға " +
				"болатынын, не керек, қайдан бастау керек екенін нақты түсінесіз.\n\n" +
				"Сабаққа қатысқаннан ештеңе жоғалтпайсыз! Керісінше, табысқа бір қадам жақындайсыз.\n\n" +
				"Сабаққа қатысу сілтемесін 15 минут қалғанда осы чатқа жіберемін, сол сілтеме арқылы " +
				"сабаққа қатысасыз.\n\n" +
				"Орындар саны шектеулі, сілтеме жібергенде кіріп орын алып қалуға асығыңыз!",
		},
		{
			Name:         "15 минут бұрын",
			OffsetSec:    -15 * 60,
			ScheduleKind: domain.ScheduleRelativeToEvent,
			DailyWebinar: true,
			TemplateName: "07. Airan — 15 минут бұрын, сілтеме",
			IntendedType: "IMAGE_WITH_CAPTION",
			Body: "15 минуттан кейін сабағымызды бастаймыз!\n\n" +
				"Тегін сабаққа қатысамын деген шешіміңіз — сізді «Мен бизнес жасай алмаймын» деген " +
				"ойдан құтқаруы мүмкін.\n\n" +
				"Басқалар күмәнданады. Біз дәлелдейміз.\n\n" +
				"Турецкий айран және қаймақ кәсібі арқылы күніне кемі 40 мың теңге табуды үйрететін " +
				"тегін сабағымыз 15 минутта басталады.\n\n" +
				"Тегін сабаққа сілтеме:\n\n{{webinar_link}}",
		},
		{
			Name:         "5 минут бұрын",
			OffsetSec:    -5 * 60,
			ScheduleKind: domain.ScheduleRelativeToEvent,
			DailyWebinar: true,
			TemplateName: "08. Airan — 5 минут бұрын, сілтеме",
			IntendedType: "IMAGE_WITH_CAPTION",
			Body: "Болашақ кәсіпкерлер жиналып жатыр, сізді күтіп отырмыз!\n\n" +
				"— Кәсіп бастауға ақшаңыз жоқ па?\n" +
				"— Қайда сатам, кім алады дейсіз бе?\n" +
				"— Қолымнан келе ме деп уайымдайсыз ба?\n\n" +
				"Бәріне нақты жауап осы сабақта болады! 🔥\n\n" +
				"⏰ Турецкий айран бизнесі арқылы күніне кемі 40 мың теңге табуды үйрететін тегін " +
				"сабақтың басталуына 5 минут қалды! 🙌🏻\n\n" +
				"🍾 Тегін сабаққа сілтеме:\n\n{{webinar_link}}",
		},
		{
			Name:         "Сабақ басталды",
			OffsetSec:    0,
			ScheduleKind: domain.ScheduleRelativeToEvent,
			DailyWebinar: true,
			TemplateName: "09. Airan — сабақ басталды",
			IntendedType: "IMAGE_WITH_CAPTION",
			Body: "Таяқ жерге тасталды, көптен күткен тегін сабағымыз басталды! 🥳\n\n" +
				"⏰ Турецкий айран мен Қаймақ бизнесі арқылы күніне кемі 40 мың теңге табуды үйрететін " +
				"тегін сабағымыз басталды! 🥳🔥\n\n" +
				"🍾 Тегін сабаққа сілтеме:\n\n{{webinar_link}}",
		},
	}
}

// Options controls one seeding run.
type Options struct {
	// EventDate is the webinar date, YYYY-MM-DD. Empty means the next
	// occurrence of WebinarTime.
	EventDate string
	// EventTime is the webinar start, HH:MM. Empty means WebinarTime.
	EventTime string
	// Timezone the event time is written in. Empty means Asia/Almaty.
	Timezone string
	// Link overrides the default room.
	Link string
	// Activate creates the campaign live rather than as a draft.
	//
	// The default is off, and deliberately so. Every media step is seeded as
	// text until its asset is uploaded, so a campaign activated on first boot
	// would send the script of a voice note to real leads as a written message.
	// Going live is a decision with an audience on the other end of it, and it
	// belongs to the operator.
	Activate bool
}

// Result reports what a seeding run did, for the log line and the CLI.
type Result struct {
	Skipped        bool
	SkipReason     string
	CampaignID     uuid.UUID
	CampaignName   string
	CampaignStatus domain.CampaignStatus
	EventStartAt   time.Time
	Templates      int
	Steps          int
	TriggerKeyword string
}

// Deps is what seeding needs from the application.
type Deps struct {
	Campaigns *campaigns.Service
	Templates *templates.Service
	Log       *slog.Logger
}

// EnsureDefaultCampaign installs Airan when, and only when, the database holds
// no campaigns.
//
// The emptiness check is the safety property, so it is a count over campaigns
// rather than a lookup for "Airan": a database with any campaign in it is one
// somebody has already set up, and an installer that adds to it is an installer
// that can surprise them. An operator who deletes Airan is telling us they do
// not want it, and the next restart must respect that — which it does, as long
// as any other campaign exists. On a database whose only campaign was Airan,
// deleting it does invite a fresh copy on the next boot; disable seeding once
// the system is configured if that matters.
func EnsureDefaultCampaign(ctx context.Context, deps Deps, opts Options) (*Result, error) {
	count, err := deps.Campaigns.Repo().CountAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("count campaigns: %w", err)
	}
	if count > 0 {
		return &Result{
			Skipped:    true,
			SkipReason: fmt.Sprintf("database already holds %d campaign(s); nothing was changed", count),
		}, nil
	}

	deps.Log.Info("empty database detected; installing the default campaign",
		slog.String("campaign", CampaignName))

	return install(ctx, deps, opts)
}

// Install writes the campaign unconditionally. It backs the CLI seeder, where
// the operator has asked for it explicitly; the startup path goes through
// EnsureDefaultCampaign instead.
func Install(ctx context.Context, deps Deps, opts Options) (*Result, error) {
	return install(ctx, deps, opts)
}

func install(ctx context.Context, deps Deps, opts Options) (*Result, error) {
	tz := opts.Timezone
	if tz == "" {
		tz = "Asia/Almaty"
	}
	eventTime := opts.EventTime
	if eventTime == "" {
		eventTime = WebinarTime
	}
	eventDate := opts.EventDate
	if eventDate == "" {
		eventDate = nextOccurrence(tz, eventTime)
	}
	link := opts.Link
	if link == "" {
		link = WebinarLink
	}

	steps := Steps()
	result := &Result{CampaignName: CampaignName, TriggerKeyword: TriggerPhrase}

	// Templates first: steps reference them, and a step cannot be created
	// without one.
	templateIDs := make(map[string]uuid.UUID, len(steps))
	for _, step := range steps {
		id, created, err := ensureTemplate(ctx, deps, step)
		if err != nil {
			return nil, err
		}
		templateIDs[step.TemplateName] = id
		if created {
			result.Templates++
		}
	}

	campaign, err := deps.Campaigns.Create(ctx, campaigns.SaveInput{
		Name:        CampaignName,
		Description: "Turkish Ayran and Kaymak business free webinar campaign.",
		EventType:   "WEBINAR",
		EventDate:   eventDate,
		EventTime:   eventTime,
		Timezone:    tz,
		WebinarLink: link,
		// A repeat trigger from a contact already in the funnel is ignored,
		// which is the only behaviour that cannot produce a duplicate message.
		ExistingContactBehavior: string(domain.BehaviorIgnore),
		UnsubscribeKeywords:     []string{"STOP", "СТОП", "ТОҚТАТУ", "ТОКТАТУ"},
		// A lead who arrives after some steps have passed gets the most recent
		// one immediately for context; the rest of the past is skipped and
		// every future step is still scheduled.
		CatchUpMissedSteps: true,
		MaxSendAttempts:    5,
		ResumePolicy:       string(domain.ResumeSkipExpired),
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("create campaign: %w", err)
	}
	result.CampaignID = campaign.ID
	if campaign.EventStartAt != nil {
		result.EventStartAt = *campaign.EventStartAt
	}

	if _, err := deps.Campaigns.AddTrigger(ctx, campaign.ID, TriggerPhrase, "EXACT", true); err != nil {
		return nil, fmt.Errorf("create trigger: %w", err)
	}

	for _, step := range steps {
		if _, err := deps.Campaigns.AddStep(ctx, campaign.ID, campaigns.StepInput{
			Name:          step.Name,
			OffsetSeconds: step.OffsetSec,
			TemplateID:    templateIDs[step.TemplateName],
			Enabled:       true,
			ScheduleKind:  string(step.ScheduleKind),
			// Recorded on the step so that switching the campaign to a daily
			// webinar later needs no second pass over the funnel.
			IncludeInDailyWebinar: step.DailyWebinar,
		}); err != nil {
			return nil, fmt.Errorf("create step %q: %w", step.Name, err)
		}
		result.Steps++
	}

	result.CampaignStatus = domain.CampaignDraft
	if opts.Activate {
		activated, err := deps.Campaigns.Activate(ctx, campaign.ID)
		if err != nil {
			return nil, fmt.Errorf("activate campaign: %w", err)
		}
		result.CampaignStatus = activated.Status
	}

	return result, nil
}

// ensureTemplate creates one template, reusing an existing one of the same name
// rather than failing. Template names are unique, so a half-finished previous
// run leaves rows this pass must adopt instead of colliding with.
func ensureTemplate(ctx context.Context, deps Deps, step Step) (uuid.UUID, bool, error) {
	tpl, err := deps.Templates.Create(ctx, templates.Input{
		Name: step.TemplateName,
		Description: fmt.Sprintf(
			"Жоспарланған түрі: %s. Медиа файлды жүктеп, шаблон түрін өзгертіңіз.",
			step.IntendedType),
		Type:        string(domain.TemplateText),
		Body:        step.Body,
		LinkPreview: true,
	}, nil)
	if err == nil {
		return tpl.ID, true, nil
	}

	if !errors.Is(err, templates.ErrNameTaken) {
		return uuid.Nil, false, fmt.Errorf("create template %q: %w", step.TemplateName, err)
	}

	existing, listErr := deps.Templates.List(ctx, step.TemplateName, "", false)
	if listErr != nil {
		return uuid.Nil, false, fmt.Errorf("locate existing template %q: %w", step.TemplateName, listErr)
	}
	for _, candidate := range existing {
		if candidate.Name == step.TemplateName {
			return candidate.ID, false, nil
		}
	}
	return uuid.Nil, false, fmt.Errorf("template %q exists but could not be loaded", step.TemplateName)
}

// nextOccurrence returns the date of the next time hh:mm comes round in tz,
// which is today when it has not happened yet and tomorrow when it has.
func nextOccurrence(tz, hhmm string) string {
	loc, err := timex.Location(tz)
	if err != nil {
		loc = time.UTC
	}

	now := time.Now().In(loc)
	today := now.Format("2006-01-02")

	start, err := timex.ParseInLocation(today, hhmm, tz)
	if err != nil {
		return today
	}
	if start.After(now.UTC()) {
		return today
	}
	return now.AddDate(0, 0, 1).Format("2006-01-02")
}
