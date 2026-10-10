package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/conversations"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/media"
	"github.com/ayran/whatsapp-automation/internal/messaging"
	"github.com/ayran/whatsapp-automation/internal/outbound"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/internal/templates"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
	"github.com/ayran/whatsapp-automation/migrations"
)

type recoveryProvider struct {
	whatsapp.Provider
	mu       sync.Mutex
	calls    int
	failures int
	status   int
	accepted int
	times    []time.Time
}

func (p *recoveryProvider) Name() string { return "fake" }
func (p *recoveryProvider) SendText(_ context.Context, _ whatsapp.TextMessage) (*whatsapp.SendResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.times = append(p.times, time.Now())
	if p.failures > 0 {
		p.failures--
		return nil, &whatsapp.Error{Provider: "fake", Op: "send", StatusCode: p.status, Retryable: p.status >= 500}
	}
	p.accepted++
	return &whatsapp.SendResult{ExternalID: fmt.Sprintf("sent-%d", p.calls)}, nil
}
func (p *recoveryProvider) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.accepted
}

type recoveryRegistry struct{ provider *recoveryProvider }

func (r recoveryRegistry) ProviderFor(int64) (whatsapp.Provider, error) { return r.provider, nil }

type recoveryFixture struct {
	db       *sqlite.DB
	p        *Processor
	provider *recoveryProvider
	jobs     *scheduler.Repository
	campaign *domain.Campaign
	cfg      config.Scheduler
	gate     *outbound.Router
	store    *media.Store
}

func recoverySetup(t *testing.T, behavior domain.ExistingContactBehavior) *recoveryFixture {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Connect(ctx, config.Database{Path: filepath.Join(t.TempDir(), "test.db"), MaxConns: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx, migrations.FS, discardLogger()); err != nil {
		t.Fatal(err)
	}
	contactRepo := contacts.NewRepository(db)
	messageRepo := conversations.NewRepository(db)
	jobRepo := scheduler.NewRepository(db)
	campaignRepo := campaigns.NewRepository(db)
	campaignSvc := campaigns.NewService(campaignRepo, jobRepo, contactRepo, discardLogger())
	templateRepo := templates.NewRepository(db)
	tpl := &domain.MessageTemplate{Name: "welcome", Type: domain.TemplateText, Body: "Hello"}
	if err := templateRepo.Create(ctx, tpl); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(24 * time.Hour)
	campaign := &domain.Campaign{Name: "recovery", Status: domain.CampaignDraft, EventType: "CUSTOM", EventStartAt: &at,
		Timezone: "Asia/Almaty", ExistingContactBehavior: behavior, UnsubscribeKeywords: []string{"STOP"}, MaxSendAttempts: 2,
		ResumePolicy: domain.ResumeSkipExpired, ExistingContactTemplate: &tpl.ID}
	if err := campaignRepo.Create(ctx, nil, campaign); err != nil {
		t.Fatal(err)
	}
	step := &domain.CampaignStep{CampaignID: campaign.ID, Name: "greeting", TemplateID: tpl.ID, Enabled: true, OrderIndex: 1, ScheduleKind: domain.ScheduleOnTrigger}
	if err := campaignRepo.CreateStep(ctx, nil, step); err != nil {
		t.Fatal(err)
	}
	if _, err := campaignSvc.AddTrigger(ctx, campaign.ID, "JOIN", "EXACT", true); err != nil {
		t.Fatal(err)
	}
	if err := campaignRepo.SetStatus(ctx, nil, campaign.ID, domain.CampaignActive); err != nil {
		t.Fatal(err)
	}
	store, err := media.NewStore(t.TempDir(), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	provider := &recoveryProvider{status: 503}
	sender := messaging.NewSender(recoveryRegistry{provider}, messageRepo, contactRepo, store, discardLogger())
	gate := outbound.NewRouter([]outbound.InstanceSpec{{ID: 1, Key: "primary", Outbound: config.Outbound{Workers: 1, MinDelay: 2 * time.Millisecond, MaxDelay: 2 * time.Millisecond}},
		{ID: 2, Key: "secondary", Outbound: config.Outbound{Workers: 1}}}, sender, discardLogger())
	cfg := config.Scheduler{Workers: 2, PollInterval: 5 * time.Millisecond, BatchSize: 25, NotificationWorkers: 1,
		NotificationBatchSize: 150, RecoveryBatchSize: 150, RetryBaseDelay: 5 * time.Millisecond, RetryMaxDelay: 30 * time.Millisecond,
		LockTimeout: time.Minute, StaleJobTTL: 2 * time.Hour, MaxAttempts: 2}
	p := NewProcessor(cfg, NewRepository(db), contactRepo, messageRepo, campaignSvc, templateRepo, gate, store, discardLogger())
	return &recoveryFixture{db, p, provider, jobRepo, campaign, cfg, gate, store}
}

func (f *recoveryFixture) sql(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}
func (f *recoveryFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func (f *recoveryFixture) ingest(t *testing.T, instance int64, id, digits string, at time.Time) uuid.UUID {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"typeWebhook": "incomingMessageReceived", "idMessage": id, "timestamp": at.Unix(),
		"senderData":  map[string]any{"chatId": digits + "@c.us", "senderName": "Lead"},
		"messageData": map[string]any{"typeMessage": "textMessage", "textMessageData": map[string]any{"textMessage": "JOIN"}}})
	key := "primary"
	if instance == 2 {
		key = "secondary"
	}
	if _, err := f.p.Ingest(context.Background(), instance, key, body); err != nil {
		t.Fatal(err)
	}
	var eventID uuid.UUID
	if err := f.db.QueryRow(context.Background(), `SELECT id FROM webhook_events WHERE external_id = $1 AND whatsapp_instance_id = $2`, id, instance).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	return eventID
}
func (f *recoveryFixture) process(t *testing.T) {
	t.Helper()
	f.sql(t, `UPDATE webhook_events SET next_attempt_at = NULL WHERE status = 'RECEIVED'`)
	events, err := f.p.repo.Claim(context.Background(), 1)
	if err != nil || len(events) != 1 {
		t.Fatalf("claim = %d: %v", len(events), err)
	}
	f.p.handle(context.Background(), events[0])
}
func (f *recoveryFixture) startWorker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := scheduler.NewWorker(f.cfg, f.jobs, f.p.templates, f.p.contacts, f.gate, f.store, discardLogger())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Wait() })
}
func awaitRecovery(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("recovery condition timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func (f *recoveryFixture) sendGreeting(t *testing.T) {
	t.Helper()
	f.sql(t, `UPDATE scheduled_messages SET scheduled_at = now() WHERE status = 'PENDING'`)
	f.startWorker(t)
	awaitRecovery(t, func() bool { return f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'SENT'`) == 1 })
}

func TestDurableTriggerNormalAndRepeatedRestart(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorRestart)
	f.ingest(t, 1, "lead-1", "77011234567", time.Now().UTC())
	f.process(t)
	if got := f.count(t, `SELECT count(*) FROM webhook_events WHERE status = 'PROCESSED'`); got != 0 {
		t.Fatal("marked successful before send")
	}
	f.sendGreeting(t)
	for i := 0; i < 5; i++ {
		f.sql(t, `UPDATE webhook_events SET status = 'PROCESSING', updated_at = ts_add(now(), -600)`)
		if _, err := f.p.repo.ReleaseStale(context.Background(), 5*time.Minute); err != nil {
			t.Fatal(err)
		}
		f.process(t)
	}
	if calls, accepted := f.provider.counts(); calls != 1 || accepted != 1 {
		t.Fatalf("calls=%d accepted=%d", calls, accepted)
	}
	if n := f.count(t, `SELECT count(*) FROM campaign_contacts WHERE run_number = 1`); n != 1 {
		t.Fatal("replay restarted enrollment")
	}
	if n := f.count(t, `SELECT count(*) FROM scheduled_messages`); n != 1 {
		t.Fatal("duplicate schedule")
	}
	if n := f.count(t, `SELECT unread_count FROM contacts`); n != 1 {
		t.Fatalf("replay incremented unread counter: %d", n)
	}
}

func TestCrashAfterHistoryBeforeTriggerIsRecovered(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorRestart)
	ctx := context.Background()
	eventID := f.ingest(t, 1, "history-only", "77011234567", time.Now().UTC().Add(-72*time.Hour))
	contact, _, err := f.p.contacts.UpsertFromInbound(ctx, nil, "77011234567@c.us", "77011234567", "Lead", time.Now().UTC().Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	id := "history-only"
	if err := f.p.messages.Create(ctx, nil, &domain.Message{ContactID: contact.ID, ExternalID: &id, Direction: domain.DirectionIncoming, Type: "TEXT", Text: "JOIN", Status: domain.MessageStatusReceived}); err != nil {
		t.Fatal(err)
	}
	f.sql(t, `UPDATE webhook_events SET status = 'PROCESSING', updated_at = ts_add(now(), -600) WHERE id = $1`, eventID)
	if _, err := f.p.repo.ReleaseStale(ctx, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	f.process(t)
	f.sendGreeting(t)
	f.process(t)
	if n := f.count(t, `SELECT count(*) FROM webhook_events WHERE status = 'PROCESSED'`); n != 1 {
		t.Fatalf("completed events = %d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM messages WHERE direction = 'INCOMING'`); n != 1 {
		t.Fatal("duplicated history")
	}
}

func TestCrashBeforeProcessingAndAfterSendReceipt(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorIgnore)
	// A persisted event is enough; no in-memory enqueue is required.
	f.ingest(t, 1, "pending", "77011234567", time.Now().UTC())
	f.process(t)
	jobs, err := f.jobs.Claim(context.Background(), "dead-worker", 1, time.Now().UTC().Add(time.Hour), time.Now().UTC().Add(-time.Minute))
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v", err)
	}
	contact, err := f.p.contacts.GetByID(context.Background(), jobs[0].ContactID)
	if err != nil {
		t.Fatal(err)
	}
	// Crash after messages.MarkSent, before scheduled_messages.MarkSent.
	if _, err := f.gate.Send(context.Background(), contact, messaging.Outbound{WhatsAppInstanceID: 1, Type: domain.TemplateText, Text: "Hello", ScheduledMessageID: &jobs[0].ID}); err != nil {
		t.Fatal(err)
	}
	f.sql(t, `UPDATE scheduled_messages SET locked_at = ts_add(now(), -600), scheduled_at = now()`)
	f.startWorker(t)
	awaitRecovery(t, func() bool { return f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'SENT'`) == 1 })
	f.process(t)
	if calls, _ := f.provider.counts(); calls != 1 {
		t.Fatalf("receipt was resent: %d", calls)
	}
}

func TestFailedWelcomeRemainsRetryableThroughLongProviderOutage(t *testing.T) {
	for _, status := range []int{503, 401, 466} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := recoverySetup(t, domain.BehaviorIgnore)
			t.Cleanup(func() {
				if t.Failed() {
					calls, accepted := f.provider.counts()
					t.Logf("calls=%d accepted=%d sent=%d pending=%d processing=%d failed=%d cancelled=%d", calls, accepted,
						f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'SENT'`),
						f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'PENDING'`),
						f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'PROCESSING'`),
						f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'FAILED'`),
						f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'CANCELLED'`))
				}
			})
			f.provider.failures, f.provider.status = 4, status // exceeds campaign budget
			f.ingest(t, 1, "failed", "77011234567", time.Now().UTC())
			f.process(t)
			f.sendGreeting(t)
			if calls, accepted := f.provider.counts(); calls != 5 || accepted != 1 {
				t.Fatalf("calls=%d successes=%d", calls, accepted)
			}
			f.process(t)
			if n := f.count(t, `SELECT count(*) FROM webhook_events WHERE status = 'PROCESSED'`); n != 1 {
				t.Fatal("event did not complete after recovery")
			}
		})
	}
}

func TestFailedSpecialReplyAndReceiptReplay(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorSpecialMessage)
	f.ingest(t, 1, "first", "77011234567", time.Now().UTC())
	f.process(t)
	f.sendGreeting(t)
	f.process(t)
	second := f.ingest(t, 1, "second", "77011234567", time.Now().UTC())
	f.provider.mu.Lock()
	f.provider.failures = 1
	f.provider.mu.Unlock()
	f.process(t)
	if n := f.count(t, `SELECT count(*) FROM webhook_events WHERE id = $1 AND status = 'PROCESSED'`, second); n != 0 {
		t.Fatal("failed special reply marked completed")
	}
	f.process(t)
	// Force replay after successful reply, as if finishing the event had failed.
	f.sql(t, `UPDATE webhook_events SET status = 'PROCESSING', updated_at = ts_add(now(), -600) WHERE id = $1`, second)
	if _, err := f.p.repo.ReleaseStale(context.Background(), time.Minute); err != nil {
		t.Fatal(err)
	}
	f.process(t)
	if calls, accepted := f.provider.counts(); calls != 3 || accepted != 2 {
		t.Fatalf("calls=%d successes=%d", calls, accepted)
	}
	// Same phone and text, different provider ID is a legitimate new event.
	f.ingest(t, 1, "third", "77011234567", time.Now().UTC())
	f.process(t)
	if _, accepted := f.provider.counts(); accepted != 3 {
		t.Fatal("deduped legitimate repeated text")
	}
}

func TestRecoveryBacklogIsBoundedAndKeepsLiveSlot(t *testing.T) {
	for _, size := range []int{200, 500} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := recoverySetup(t, domain.BehaviorIgnore)
			t.Cleanup(func() {
				if t.Failed() {
					calls, accepted := f.provider.counts()
					t.Logf("calls=%d accepted=%d sent=%d pending=%d processing=%d failed=%d cancelled=%d", calls, accepted,
						f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'SENT'`),
						f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'PENDING'`),
						f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'PROCESSING'`),
						f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'FAILED'`),
						f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'CANCELLED'`))
				}
			})
			for i := 0; i < size; i++ {
				f.ingest(t, 1, fmt.Sprintf("old-%04d", i), fmt.Sprintf("7701000%04d", i), time.Now().UTC().Add(-72*time.Hour).Add(time.Duration(i)*time.Second))
			}
			live := f.ingest(t, 1, "live", "77019999999", time.Now().UTC())
			f.p.drain(context.Background())
			// A slow drain may also recheck replies whose deferral became due.
			// The claim budget bounds total work, not only new history rows.
			if n := f.count(t, `SELECT count(*) FROM messages WHERE direction = 'INCOMING'`); n < 75 || n > 150 {
				t.Fatalf("new events in batch = %d, want 75..150", n)
			}
			if n := f.count(t, `SELECT count(*) FROM webhook_events`); n != size+1 {
				t.Fatalf("lost backlog: %d", n)
			}
			if n := f.count(t, `SELECT count(*) FROM webhook_events WHERE id = $1 AND trigger_result IS NOT NULL`, live); n != 1 {
				t.Fatal("backlog blocked live trigger")
			}
			if calls, _ := f.provider.counts(); calls != 0 {
				t.Fatal("inbound drain bypassed outbound queue")
			}
			for f.count(t, `SELECT count(*) FROM webhook_events WHERE trigger_result IS NULL`) > 0 {
				f.p.drain(context.Background())
			}
			f.sql(t, `UPDATE scheduled_messages SET scheduled_at = now()`)
			f.startWorker(t)
			awaitRecovery(t, func() bool { _, accepted := f.provider.counts(); return accepted == size+1 })
			f.provider.mu.Lock()
			if elapsed := f.provider.times[len(f.provider.times)-1].Sub(f.provider.times[0]); elapsed < time.Duration(size)*time.Millisecond {
				t.Errorf("recovery bypassed pacing: %d sends in %s", size+1, elapsed)
			}
			f.provider.mu.Unlock()
			if n := f.count(t, `SELECT count(*) FROM webhook_events`); n != size+1 {
				t.Fatal("events deleted after sending")
			}
		})
	}
}

func TestAwaitingRepliesDoNotStarveFreshTriggers(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorIgnore)
	old := time.Now().UTC().Add(-72 * time.Hour)
	for i := 0; i < 150; i++ {
		f.ingest(t, 1, fmt.Sprintf("waiting-%04d", i), fmt.Sprintf("7701000%04d", i), old.Add(time.Duration(i)*time.Second))
	}
	for f.count(t, `SELECT count(*) FROM webhook_events WHERE trigger_result IS NULL`) > 0 {
		f.p.drain(context.Background())
	}
	// All greetings are still queued, and all completion checks are due.
	f.sql(t, `UPDATE webhook_events SET next_attempt_at = NULL`)
	for i := 0; i < 80; i++ {
		f.ingest(t, 1, fmt.Sprintf("fresh-%04d", i), fmt.Sprintf("7702000%04d", i), old.Add(time.Hour).Add(time.Duration(i)*time.Second))
	}
	live := f.ingest(t, 1, "live", "77029999999", time.Now().UTC())
	f.p.drain(context.Background())
	if n := f.count(t, `SELECT count(*) FROM webhook_events WHERE external_id LIKE 'fresh-%' AND trigger_result IS NOT NULL`); n != 80 {
		t.Fatalf("completion checks starved fresh backlog: processed %d of 80", n)
	}
	if n := f.count(t, `SELECT count(*) FROM webhook_events WHERE id = $1 AND trigger_result IS NOT NULL`, live); n != 1 {
		t.Fatal("completion checks starved live traffic")
	}
	if n := f.count(t, `SELECT count(*) FROM webhook_events WHERE status = 'PROCESSED'`); n != 0 {
		t.Fatal("queued replies were falsely completed")
	}
	if calls, _ := f.provider.counts(); calls != 0 {
		t.Fatal("completion checks bypassed outbound queue")
	}
}

func TestManualRecoveryPreservesEnrollmentAndExpiredWebinarSteps(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorRestart)
	ctx := context.Background()
	old := time.Now().UTC().Add(-72 * time.Hour)
	f.sql(t, `UPDATE campaigns SET event_start_at = $1, catch_up_missed_steps = 0 WHERE id = $2`, old.Add(time.Hour), f.campaign.ID)
	step := &domain.CampaignStep{CampaignID: f.campaign.ID, Name: "obsolete webinar reminder", Enabled: true, OrderIndex: 2, ScheduleKind: domain.ScheduleRelativeToEvent, OffsetSeconds: -1800}
	// Read the existing template without assuming any campaign-specific ID.
	steps, err := f.p.campaigns.Repo().ListSteps(ctx, nil, f.campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	step.TemplateID = steps[0].TemplateID
	if err := f.p.campaigns.Repo().CreateStep(ctx, nil, step); err != nil {
		t.Fatal(err)
	}
	id := f.ingest(t, 1, "legacy", "77011234567", old)
	f.process(t)
	// State left by the previous version: falsely PROCESSED, greeting expired.
	f.sql(t, `UPDATE webhook_events SET status = 'PROCESSED', trigger_result = NULL WHERE id = $1`, id)
	f.sql(t, `UPDATE scheduled_messages SET status = 'CANCELLED', cancel_reason = 'send window expired', scheduled_at = $1 WHERE campaign_step_id != $2`, old, step.ID)
	f.sql(t, `UPDATE campaign_contacts SET status = 'COMPLETED'`)
	preview, err := f.p.PreviewRecovery(ctx, 0)
	if err != nil || preview.Eligible != 1 || len(preview.Items) != 1 {
		t.Fatalf("preview=%+v error=%v", preview, err)
	}
	result, err := f.p.RequeueRecovery(ctx, []uuid.UUID{id})
	if err != nil || result.Requeued != 1 {
		t.Fatalf("requeue=%+v err=%v", result, err)
	}
	f.process(t)
	f.sendGreeting(t)
	f.process(t)
	if n := f.count(t, `SELECT count(*) FROM campaign_contacts WHERE run_number = 1 AND status = 'COMPLETED'`); n != 1 {
		t.Fatal("recovery restarted enrollment")
	}
	if n := f.count(t, `SELECT count(*) FROM scheduled_messages`); n != 2 {
		t.Fatal("recovery recreated schedule")
	}
	if n := f.count(t, `SELECT count(*) FROM scheduled_messages WHERE campaign_step_id = $1 AND status = 'CANCELLED'`, step.ID); n != 1 {
		t.Fatal("expired webinar step revived")
	}
	preview, err = f.p.PreviewRecovery(ctx, 0)
	if err != nil || preview.AlreadyReplied != 1 || preview.Eligible != 0 {
		t.Fatalf("preview after send: %+v %v", preview, err)
	}
	result, err = f.p.RequeueRecovery(ctx, []uuid.UUID{id})
	if err != nil || result.Requeued != 0 {
		t.Fatal("stale preview requeued successful reply")
	}
}

func TestIncomingIdentityScopedAndLegacyDedupePreserved(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorIgnore)
	ctx := context.Background()
	id := f.ingest(t, 1, "same-provider-id", "77011234567", time.Now().UTC())
	f.sql(t, `UPDATE webhook_events SET dedupe_key = 'incoming:same-provider-id' WHERE id = $1`, id)
	f.ingest(t, 1, "same-provider-id", "77011234567", time.Now().UTC())
	f.ingest(t, 2, "same-provider-id", "77011234568", time.Now().UTC())
	if n := f.count(t, `SELECT count(*) FROM webhook_events`); n != 2 {
		t.Fatal("instance identity or legacy dedupe broken")
	}
	f.p.drain(ctx)
	if n := f.count(t, `SELECT count(*) FROM messages WHERE direction = 'INCOMING'`); n != 2 {
		t.Fatal("global legacy index suppressed second account history")
	}
	// Incoming identity and business checkpoints must survive retention sweeps.
	f.sql(t, `UPDATE webhook_events SET received_at = ts_add(now(), -2000000), status = 'PROCESSED'`)
	if n, err := f.p.repo.Prune(ctx, 14*24*time.Hour); err != nil || n != 0 {
		t.Fatalf("incoming history pruned: %d %v", n, err)
	}
}

func TestStartupRecoversDurableEventsWithoutPoller(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorIgnore)
	id := f.ingest(t, 1, "orphaned", "77011234567", time.Now().UTC())
	f.sql(t, `UPDATE webhook_events SET status = 'PROCESSING', updated_at = ts_add(now(), -600) WHERE id = $1`, id)
	p := NewProcessor(f.cfg, f.p.repo, f.p.contacts, f.p.messages, f.p.campaigns, f.p.templates, f.gate, f.store, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	t.Cleanup(func() { cancel(); p.Wait() })
	awaitRecovery(t, func() bool { return f.count(t, `SELECT count(*) FROM scheduled_messages`) == 1 })
	if n := f.count(t, `SELECT count(*) FROM webhook_events WHERE status = 'PROCESSED'`); n != 0 {
		t.Fatal("startup completed a trigger before sending its reply")
	}
}

func TestManualPreviewBatchDoesNotSendOrDropRemainingEvents(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorIgnore)
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		f.ingest(t, 1, fmt.Sprintf("history-%04d", i), fmt.Sprintf("7701333%04d", i), time.Now().UTC().Add(-72*time.Hour).Add(time.Duration(i)*time.Second))
	}
	f.sql(t, `UPDATE webhook_events SET status = 'PROCESSED'`)
	preview, err := f.p.PreviewRecovery(ctx, 0)
	if err != nil || preview.Eligible != 500 || len(preview.Items) != 150 {
		t.Fatalf("preview=%+v error=%v", preview, err)
	}
	ids := make([]uuid.UUID, len(preview.Items))
	for i, item := range preview.Items {
		ids[i] = item.EventID
	}
	result, err := f.p.RequeueRecovery(ctx, ids)
	if err != nil || result.Requeued != 150 {
		t.Fatalf("requeue=%+v error=%v", result, err)
	}
	if n := f.count(t, `SELECT count(*) FROM webhook_events WHERE status = 'PROCESSED'`); n != 350 {
		t.Fatalf("remaining history=%d", n)
	}
	preview, err = f.p.PreviewRecovery(ctx, 0)
	if err != nil || preview.Eligible != 350 || preview.Waiting != 150 {
		t.Fatalf("remaining preview=%+v %v", preview, err)
	}
	result, err = f.p.RequeueRecovery(ctx, ids)
	if err != nil || result.Requeued != 0 {
		t.Fatal("repeat requeue was not idempotent")
	}
	if _, err := f.p.RequeueRecovery(ctx, append(ids, uuid.New())); err == nil {
		t.Fatal("oversized batch accepted")
	}
	if calls, _ := f.provider.counts(); calls != 0 {
		t.Fatal("manual tool sent directly")
	}
	if n := f.count(t, `SELECT count(*) FROM webhook_events`); n != 500 {
		t.Fatal("manual recovery deleted history")
	}
}
