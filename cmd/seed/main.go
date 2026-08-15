// Command seed loads the initial Turkish Ayran / Kaymak webinar campaign.
//
// Everything it writes is ordinary data: templates, a campaign, a trigger and
// automation steps, all editable from the admin panel afterwards. Nothing here
// is referenced by the runtime code.
//
// Media-bearing steps are seeded as TEXT templates carrying the exact copy, so
// the campaign is sendable immediately. The intended final type is recorded in
// each template's description; upload the audio or image in the admin panel
// and switch the type to VOICE or IMAGE_WITH_CAPTION when the assets are
// ready. Seeding placeholder media instead would risk sending a placeholder to
// a real customer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/logging"
	"github.com/ayran/whatsapp-automation/internal/media"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/internal/templates"
)

// seedStep pairs an automation offset with the template that fills it.
type seedStep struct {
	name         string
	offsetSec    int
	scheduleKind domain.ScheduleKind
	templateName string
	intendedType string
	body         string
}

const campaignName = "Түрік айран / Қаймақ тегін сабағы"

// triggerPhrase is the exact message a lead must send to enter the funnel.
const triggerPhrase = "Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді"

func seedSteps() []seedStep {
	return []seedStep{
		{
			name:         "Триггер: танысу",
			offsetSec:    0,
			scheduleKind: domain.ScheduleOnTrigger,
			templateName: "01. Триггер — таныстыру",
			intendedType: "VOICE",
			body: "Сәлеметсіз бе! Есімім Әлішер, Айран/Қаймақ кәсібін өте аз суммамен бастап, " +
				"күніне кемі 40 мың теңге таза табуға болатынын бүгін сағат {{webinar_time}}-де өтетін " +
				"тегін сабағымызда түсіндіретін боламыз! Станок, рецепт, шикізаттарға басыңызды " +
				"қатырмасаңыз болады! Барлығын өзіміз береміз, ал сабаққа қалай қатысамын десеңіз, " +
				"сабақ жақындағанда осы чатқа сілтеме жіберемін, сол сілтеме арқылы сабаққа қатысасыз. " +
				"Орындар саны шектеулі, сілтеме жібергенде кіріп орын алып қалуға асығыңыз!",
		},
		{
			name:         "5 сағат бұрын",
			offsetSec:    -5 * 3600,
			scheduleKind: domain.ScheduleRelativeToEvent,
			templateName: "02. 5 сағат бұрын",
			intendedType: "IMAGE_WITH_CAPTION",
			body: "Осы жылдың соңғы тегін сабағы! Бұдан кейін тегін сабақ өткізбейміз!\n\n" +
				"Бүгін сағат {{webinar_time}}-де өтетін соңғы мүмкіндікті жіберіп алмаңыз!\n\n" +
				"Түрік айраны мен қаймақ бизнесі арқылы күніне кемінде 40 мың теңге табудың жолын " +
				"меңгеріп, өткен қаржылық қиындықтарды артта қалдырып, жаңа кәсіппен және жаңа " +
				"табыспен алға қадам жасаңыз.\n\n" +
				"Сабақтың басталуына 5 сағат уақыт қалды.\n\n" +
				"Ал сабаққа қалай қатысамын десеңіз, сабақ жақындағанда осы чатқа сілтеме жіберемін, " +
				"сол сілтеме арқылы сабаққа қатысасыз.\n\n" +
				"Орындар саны шектеулі, сілтеме жібергенде кіріп орын алып қалуға асығыңыз!",
		},
		{
			name:         "3 сағат бұрын",
			offsetSec:    -3 * 3600,
			scheduleKind: domain.ScheduleRelativeToEvent,
			templateName: "03. 3 сағат бұрын",
			intendedType: "VOICE",
			body: "Күніне кемінде 40 мың пайда алып келетін Түрік айраны мен Қаймақ кәсібін " +
				"ақтарып-төңкеріп түсіндіріп береміз. Ешқандай риск жоқ. Аз ақшамен бастап кетесіз. " +
				"Сабақтың басталуына 3 сағат уақыт қалды. Ал сабаққа қалай қатысамын десеңіз, сабақ " +
				"жақындағанда осы чатқа сілтеме жіберемін, сол сілтеме арқылы сабаққа қатысасыз. " +
				"Орындар саны шектеулі, сілтеме жібергенде кіріп орын алып қалуға асығыңыз!",
		},
		{
			name:         "2 сағат бұрын",
			offsetSec:    -2 * 3600,
			scheduleKind: domain.ScheduleRelativeToEvent,
			templateName: "04. 2 сағат бұрын",
			intendedType: "IMAGE_WITH_CAPTION",
			body: "Қаржылық жағдайыңыз тұрақсыз болып жүрсе, Түрік айраны мен Қаймақ кәсібі оны " +
				"тұрақты табыс көзіне айналдыруға мүмкіндік береді!\n\n" +
				"Сабақтың басталуына небәрі 2 сағат қалды!\n\n" +
				"Ал сабаққа қалай қатысамын десеңіз, сабақ жақындағанда осы чатқа сілтеме жіберемін, " +
				"сол сілтеме арқылы сабаққа қатысасыз.\n\n" +
				"Орындар саны шектеулі, сілтеме жібергенде кіріп орын алып қалуға асығыңыз!",
		},
		{
			name:         "1 сағат бұрын",
			offsetSec:    -3600,
			scheduleKind: domain.ScheduleRelativeToEvent,
			templateName: "05. 1 сағат бұрын",
			intendedType: "IMAGE_WITH_CAPTION",
			body: "Бүгін сабақта Түрік айраны мен Қаймақ бизнесін нөлден бастап қалай бастауға " +
				"болатынын көрсетеміз.\n\n" +
				"Бұл сабақ сіздің қаржылық еркіндікке жасаған алғашқы қадамыңыз болуы мүмкін!\n\n" +
				"Сабақтың басталуына бар-жоғы 1 сағат уақыт қалды.\n\n" +
				"Ал сабаққа қалай қатысамын десеңіз, сабақ жақындағанда осы чатқа сілтеме жіберемін, " +
				"сол сілтеме арқылы сабаққа қатысасыз.\n\n" +
				"Орындар саны шектеулі, сілтеме жібергенде кіріп орын алып қалуға асығыңыз!",
		},
		{
			name:         "30 минут бұрын",
			offsetSec:    -30 * 60,
			scheduleKind: domain.ScheduleRelativeToEvent,
			templateName: "06. 30 минут бұрын",
			intendedType: "VOICE",
			body: "Бүгінгі сабақ — жай әңгіме емес. 30 минуттан кейін сабаққа қатысып, Айран Қаймақ " +
				"кәсібін қалай аз ақшамен бастауға болатынын, не керек, қайдан бастау керек екенін " +
				"нақты түсінесіз. Сабаққа қатысқаннан ештеңе жоғалтпайсыз! Керісінше, табысқа бір " +
				"қадам жақындайсыз. Осы чатқа 15 минут қалғанда сілтеме жіберемін, сол сілтеме арқылы " +
				"сабаққа қатысасыз. Орындар саны шектеулі, сілтеме жібергенде кіріп орын алып қалуға " +
				"асығыңыз!",
		},
		{
			name:         "15 минут бұрын",
			offsetSec:    -15 * 60,
			scheduleKind: domain.ScheduleRelativeToEvent,
			templateName: "07. 15 минут бұрын — сілтеме",
			intendedType: "IMAGE_WITH_CAPTION",
			body: "15 минуттан кейін сабағымызды бастаймыз!\n\n" +
				"Тегін сабаққа қатысамын деген шешіміңіз сізді «Мен бизнес жасай алмаймын» деген " +
				"ойдан құтқаруы мүмкін.\n\n" +
				"Басқалар күмәнданады. Біз дәлелдейміз.\n\n" +
				"Түрік айран-қаймақ кәсібі арқылы күніне кемі 40 мың пайда табуды үйрететін тегін " +
				"сабағымыз 15 минутта басталады.\n\n" +
				"Тегін сабаққа сілтеме:\n\n{{webinar_link}}",
		},
		{
			// 7.5 minutes is 450 seconds: the reason offsets are stored in
			// seconds rather than whole minutes.
			name:         "7.5 минут бұрын",
			offsetSec:    -450,
			scheduleKind: domain.ScheduleRelativeToEvent,
			templateName: "08. 7.5 минут бұрын — сілтеме",
			intendedType: "IMAGE_WITH_CAPTION",
			body: "Болашақ кәсіпкерлер жиналып жатыр, сізді күтіп отырмыз!\n\n" +
				"— Кәсіп бастауға ақшаңыз жоқ па?\n" +
				"— Қайда сатам, кім алады дейсіз бе?\n" +
				"— Қолымнан келе ме деп уайымдайсыз ба?\n\n" +
				"Бәріне нақты жауап осы сабақта болады! 🔥\n\n" +
				"⏰ Турік айран бизнесі арқылы күніне кемі 40 мың теңге табуды үйрететін тегін " +
				"сабақтың басталуына 5 минут қалды 🙌🏻\n\n" +
				"🍾 Тегін сабаққа сілтеме:\n\n{{webinar_link}}",
		},
		{
			name:         "Сабақ басталды",
			offsetSec:    0,
			scheduleKind: domain.ScheduleRelativeToEvent,
			templateName: "09. Сабақ басталды",
			intendedType: "IMAGE_WITH_CAPTION",
			body: "Таяқ жерге тасталды, көптен күткен тегін сабағымыз басталды 🥳\n\n" +
				"⏰ Түрік айран мен Қаймақ бизнесі арқылы күніне кемі 40 мың теңге табуды үйрететін " +
				"тегін сабағымыз басталды! 🥳🔥\n\n" +
				"🍾 Тегін сабаққа сілтеме:\n\n{{webinar_link}}",
		},
	}
}

func main() {
	var (
		eventDate = flag.String("date", time.Now().AddDate(0, 0, 1).Format("2006-01-02"),
			"webinar date, YYYY-MM-DD")
		eventTime = flag.String("time", "21:00", "webinar start time, HH:MM")
		link      = flag.String("link", "", "webinar join link")
		force     = flag.Bool("force", false, "recreate the campaign if it already exists")
	)
	flag.Parse()

	if err := run(*eventDate, *eventTime, *link, *force); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run(eventDate, eventTime, link string, force bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logging.New(cfg.App.LogLevel, "text")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db, err := sqlite.Connect(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer db.Close()

	mediaStore, err := media.NewStore(cfg.Media.StoragePath, cfg.Media.MaxUploadMB, cfg.Media.PublicURL)
	if err != nil {
		return err
	}
	transcoder := media.NewTranscoder(cfg.Media.FFmpegPath, cfg.Media.FFprobePath, cfg.Media.VoiceBitrate)
	mediaSvc := media.NewService(mediaStore, media.NewRepository(db), transcoder, log)

	templateRepo := templates.NewRepository(db)
	templateSvc := templates.NewService(templateRepo, mediaSvc)
	campaignRepo := campaigns.NewRepository(db)

	existing, err := campaignRepo.List(ctx, campaigns.ListFilter{Search: campaignName, IncludeArchived: true})
	if err != nil {
		return err
	}
	if len(existing) > 0 && !force {
		log.Info("campaign already exists; nothing to do",
			"campaign", existing[0].Name, "id", existing[0].ID)
		return nil
	}

	steps := seedSteps()

	// Templates first: steps reference them.
	templateIDs := make(map[string]uuid.UUID, len(steps))
	for _, step := range steps {
		tpl, err := templateSvc.Create(ctx, templates.Input{
			Name: step.templateName,
			Description: fmt.Sprintf(
				"Жоспарланған түрі: %s. Медиа файлды жүктеп, шаблон түрін өзгертіңіз.",
				step.intendedType),
			Type:        string(domain.TemplateText),
			Body:        step.body,
			LinkPreview: true,
		}, nil)

		if err != nil {
			if errors.Is(err, templates.ErrNameTaken) {
				log.Info("template already exists, reusing", "name", step.templateName)
				list, listErr := templateSvc.List(ctx, step.templateName, "", false)
				if listErr != nil || len(list) == 0 {
					return fmt.Errorf("locate existing template %q: %w", step.templateName, listErr)
				}
				templateIDs[step.templateName] = list[0].ID
				continue
			}
			return fmt.Errorf("create template %q: %w", step.templateName, err)
		}

		templateIDs[step.templateName] = tpl.ID
		log.Info("template created", "name", tpl.Name, "id", tpl.ID)
	}

	campaignSvc := campaigns.NewService(campaignRepo, scheduler.NewRepository(db), contacts.NewRepository(db), log)

	campaign, err := campaignSvc.Create(ctx, campaigns.SaveInput{
		Name:                    campaignName,
		Description:             "Түрік айраны мен қаймақ бизнесі бойынша тегін вебинар",
		EventType:               "WEBINAR",
		EventDate:               eventDate,
		EventTime:               eventTime,
		Timezone:                cfg.App.DefaultTimezone,
		WebinarLink:             link,
		ExistingContactBehavior: string(domain.BehaviorIgnore),
		UnsubscribeKeywords:     []string{"STOP", "СТОП", "ТОҚТАТУ", "ТОКТАТУ"},
		CatchUpMissedSteps:      true,
		MaxSendAttempts:         5,
	}, nil)
	if err != nil {
		return fmt.Errorf("create campaign: %w", err)
	}
	log.Info("campaign created", "name", campaign.Name, "id", campaign.ID)

	trigger, err := campaignSvc.AddTrigger(ctx, campaign.ID, triggerPhrase, "EXACT", true)
	if err != nil {
		return fmt.Errorf("create trigger: %w", err)
	}
	log.Info("trigger created", "keyword", trigger.Keyword, "mode", trigger.MatchMode)

	for _, step := range steps {
		created, err := campaignSvc.AddStep(ctx, campaign.ID, campaigns.StepInput{
			Name:          step.name,
			OffsetSeconds: step.offsetSec,
			TemplateID:    templateIDs[step.templateName],
			Enabled:       true,
			ScheduleKind:  string(step.scheduleKind),
		})
		if err != nil {
			return fmt.Errorf("create step %q: %w", step.name, err)
		}
		log.Info("step created", "name", created.Name, "offset_seconds", created.OffsetSeconds)
	}

	fmt.Println()
	fmt.Println("Seed complete.")
	fmt.Printf("  Campaign: %s\n", campaign.Name)
	fmt.Printf("  Event:    %s %s (%s)\n", eventDate, eventTime, cfg.App.DefaultTimezone)
	fmt.Printf("  Trigger:  %s\n", triggerPhrase)
	fmt.Println()
	fmt.Println("Next steps in the admin panel:")
	fmt.Println("  1. Upload the voice recordings and images under Шаблондар.")
	fmt.Println("  2. Switch each template to its intended type (VOICE / IMAGE_WITH_CAPTION).")
	fmt.Println("  3. Set the webinar link on the campaign.")
	fmt.Println("  4. Review the timeline under Автоматтандыру, then activate the campaign.")
	fmt.Println()
	fmt.Println("The campaign is created as a DRAFT and sends nothing until activated.")

	return nil
}
