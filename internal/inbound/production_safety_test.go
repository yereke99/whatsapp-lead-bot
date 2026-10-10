package inbound

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
	"github.com/google/uuid"
)

func recordPhoneReply(t *testing.T, f *recoveryFixture, digits string, instance int64, at time.Time) {
	t.Helper()
	contact, _, err := f.p.contacts.UpsertFromInbound(context.Background(), nil, digits+"@c.us", digits, "Lead", at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	external := uuid.NewString()
	m := &domain.Message{ContactID: contact.ID, WhatsAppInstanceID: &instance, Direction: domain.DirectionOutgoing,
		Type: "TEXT", Text: "Operator reply", ExternalID: &external, Status: domain.MessageStatusSent, SentAt: &at}
	if err := f.p.messages.Create(context.Background(), nil, m); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalTriggerAlreadyAnsweredFromPhoneIsExcluded(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorRestart)
	old := time.Now().UTC().Add(-72 * time.Hour)
	id := f.ingest(t, 1, "historical-manual", "77011234567", old)
	recordPhoneReply(t, f, "77011234567", 1, old.Add(time.Minute))
	f.sql(t, `UPDATE webhook_events SET status = 'PROCESSED' WHERE id = $1`, id)
	preview, err := f.p.PreviewRecovery(context.Background(), 0)
	if err != nil || preview.AlreadyReplied != 1 || preview.Eligible != 0 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	result, err := f.p.RequeueRecovery(context.Background(), []uuid.UUID{id})
	if err != nil || result.Requeued != 0 {
		t.Fatalf("requeue=%+v err=%v", result, err)
	}
	// A durable notification delivered after an outage must obey the same rule.
	f.sql(t, `UPDATE webhook_events SET status = 'RECEIVED' WHERE id = $1`, id)
	f.process(t)
	if n := f.count(t, `SELECT count(*) FROM campaign_contacts`); n != 0 {
		t.Fatal("answered contact was enrolled")
	}
	if calls, _ := f.provider.counts(); calls != 0 {
		t.Fatal("duplicate reply sent")
	}
}

func TestHistoricalTriggerWithUnverifiablePhoneHistoryRequiresReview(t *testing.T) {
	for _, kind := range []string{"unavailable", "phone capture disabled", "missing trigger", "pending phone reply"} {
		t.Run(kind, func(t *testing.T) {
			f := recoverySetup(t, domain.BehaviorIgnore)
			old := time.Now().UTC().Add(-time.Hour)
			id := f.ingest(t, 1, "uncertain", "77011234567", old)
			f.provider.history = func(context.Context, string) (*whatsapp.ReplyHistory, error) {
				if kind == "unavailable" {
					return nil, fmt.Errorf("provider offline")
				}
				h := &whatsapp.ReplyHistory{PhoneRepliesRecorded: kind != "phone capture disabled"}
				if kind != "missing trigger" {
					h.Messages = append(h.Messages, whatsapp.HistoryMessage{Type: "incoming", ExternalID: "uncertain", Timestamp: old.Unix(), ChatID: "77011234567@c.us"})
				}
				if kind == "pending phone reply" {
					h.Messages = append(h.Messages, whatsapp.HistoryMessage{Type: "outgoing", Timestamp: old.Add(time.Minute).Unix(), ChatID: "77011234567@c.us", Status: "pending"})
				}
				return h, nil
			}
			f.sql(t, `UPDATE webhook_events SET status = 'PROCESSED' WHERE id = $1`, id)
			preview, err := f.p.PreviewRecovery(context.Background(), 0)
			if err != nil || preview.Ambiguous != 1 || preview.Eligible != 0 {
				t.Fatalf("preview=%+v err=%v", preview, err)
			}
			result, err := f.p.RequeueRecovery(context.Background(), []uuid.UUID{id})
			if err != nil || result.Requeued != 0 {
				t.Fatalf("requeue=%+v err=%v", result, err)
			}
			f.sql(t, `UPDATE webhook_events SET status = 'RECEIVED' WHERE id = $1`, id)
			f.process(t)
			if n := f.count(t, `SELECT count(*) FROM webhook_events WHERE status = 'FAILED' AND error LIKE '%manualReview%'`); n != 1 {
				t.Fatal("missing durable manual-review outcome")
			}
			if n := f.count(t, `SELECT count(*) FROM scheduled_messages`); n != 0 {
				t.Fatal("uncertain event created jobs")
			}
		})
	}
}

func TestRecoveryDoesNotCatchUpWebinarStepsFromPreviousDays(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorRestart)
	old := time.Now().UTC().Add(-72 * time.Hour)
	f.sql(t, `UPDATE campaigns SET event_start_at = $1, catch_up_missed_steps = 1 WHERE id = $2`, old.Add(time.Hour), f.campaign.ID)
	steps, err := f.p.campaigns.Repo().ListSteps(context.Background(), nil, f.campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	step := &domain.CampaignStep{CampaignID: f.campaign.ID, Name: "expired webinar", TemplateID: steps[0].TemplateID, Enabled: true, OrderIndex: 2, ScheduleKind: domain.ScheduleRelativeToEvent, IncludeInDailyWebinar: true}
	if err := f.p.campaigns.Repo().CreateStep(context.Background(), nil, step); err != nil {
		t.Fatal(err)
	}
	f.ingest(t, 1, "old-join", "77011234567", old)
	f.process(t)
	if n := f.count(t, `SELECT count(*) FROM scheduled_messages WHERE campaign_step_id = $1 AND status = 'CANCELLED' AND cancel_reason = 'skip:step_expired'`, step.ID); n != 1 {
		t.Fatal("historical catch-up revived webinar")
	}
	f.sendGreeting(t)
	if calls, accepted := f.provider.counts(); calls != 1 || accepted != 1 {
		t.Fatalf("calls=%d accepted=%d", calls, accepted)
	}
}

func TestQueuedWelcomeRechecksPhoneRepliesBeforeSending(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorIgnore)
	id := f.ingest(t, 1, "queued-welcome", "77011234567", time.Now().UTC())
	f.process(t)
	old := time.Now().UTC().Add(-time.Hour)
	f.sql(t, `UPDATE webhook_events SET received_at = $1, payload = json_set(payload, '$.timestamp', $2) WHERE id = $3`, old, old.Unix(), id)
	recordPhoneReply(t, f, "77011234567", 1, old.Add(time.Minute))
	f.sql(t, `UPDATE scheduled_messages SET scheduled_at = now()`)
	f.startWorker(t)
	awaitRecovery(t, func() bool {
		return f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'CANCELLED' AND cancel_reason = 'trigger already answered'`) == 1
	})
	f.process(t)
	if calls, _ := f.provider.counts(); calls != 0 {
		t.Fatal("queued duplicate welcome sent")
	}
	if n := f.count(t, `SELECT count(*) FROM webhook_events WHERE status = 'PROCESSED'`); n != 1 {
		t.Fatal("manual answer did not resolve trigger")
	}
	if n := f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'SENT'`); n != 0 {
		t.Fatal("manual reply falsely marked welcome SENT")
	}
}

func TestOrphanedUnconfirmedSubmissionIsNotResent(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorIgnore)
	f.ingest(t, 1, "inflight", "77011234567", time.Now().UTC())
	f.process(t)
	var job, contact uuid.UUID
	if err := f.db.QueryRow(context.Background(), `SELECT id, contact_id FROM scheduled_messages`).Scan(&job, &contact); err != nil {
		t.Fatal(err)
	}
	m := &domain.Message{ContactID: contact, Direction: domain.DirectionOutgoing, Type: "TEXT", Text: "possibly accepted", Status: domain.MessageStatusPending, ScheduledMessageID: &job}
	if err := f.p.messages.Create(context.Background(), nil, m); err != nil {
		t.Fatal(err)
	}
	f.sql(t, `UPDATE scheduled_messages SET scheduled_at = now()`)
	f.startWorker(t)
	awaitRecovery(t, func() bool {
		return f.count(t, `SELECT count(*) FROM scheduled_messages WHERE status = 'FAILED' AND last_error LIKE '%manualReview%'`) == 1
	})
	if calls, _ := f.provider.counts(); calls != 0 {
		t.Fatal("uncertain submission resent")
	}
}

func TestExpiredJobsAreStoppedBeforeMaintenanceSweep(t *testing.T) {
	for _, kind := range []string{"expired TTL", "previous local day"} {
		t.Run(kind, func(t *testing.T) {
			f := recoverySetup(t, domain.BehaviorIgnore)
			steps, err := f.p.campaigns.Repo().ListSteps(context.Background(), nil, f.campaign.ID)
			if err != nil {
				t.Fatal(err)
			}
			step := &domain.CampaignStep{CampaignID: f.campaign.ID, Name: "webinar", TemplateID: steps[0].TemplateID, Enabled: true, OrderIndex: 2, ScheduleKind: domain.ScheduleRelativeToEvent, IncludeInDailyWebinar: kind == "previous local day"}
			if err := f.p.campaigns.Repo().CreateStep(context.Background(), nil, step); err != nil {
				t.Fatal(err)
			}
			f.ingest(t, 1, "expire", "77011234567", time.Now().UTC())
			f.process(t)
			f.sql(t, `UPDATE scheduled_messages SET status = 'CANCELLED', cancel_reason = 'cancelled by administrator' WHERE campaign_step_id != $1`, step.ID)
			at := time.Now().UTC().Add(-3 * time.Hour)
			if kind == "previous local day" {
				f.cfg.StaleJobTTL = 48 * time.Hour
				at = time.Now().UTC().Add(-24 * time.Hour)
			}
			f.sql(t, `UPDATE scheduled_messages SET scheduled_at = $1 WHERE campaign_step_id = $2`, at, step.ID)
			f.startWorker(t)
			awaitRecovery(t, func() bool {
				return f.count(t, `SELECT count(*) FROM scheduled_messages WHERE campaign_step_id = $1 AND status = 'CANCELLED' AND cancel_reason = 'send window expired'`, step.ID) == 1
			})
			if calls, _ := f.provider.counts(); calls != 0 {
				t.Fatal("expired webinar sent before reaper")
			}
		})
	}
}

func TestOutgoingEchoesAndStatusesAreIsolatedByInstance(t *testing.T) {
	f := recoverySetup(t, domain.BehaviorIgnore)
	now := time.Now().UTC()
	f.ingest(t, 1, "contact1", "77011234567", now)
	f.process(t)
	f.ingest(t, 2, "contact2", "77011234568", now)
	f.p.drain(context.Background())
	for instance := int64(1); instance <= 2; instance++ {
		event := &whatsapp.Event{ExternalID: "shared-outgoing", ChatID: fmt.Sprintf("7701123456%d@c.us", instance+6), Timestamp: now, Message: &whatsapp.InboundMessage{Type: whatsapp.TypeText, Text: "phone reply"}}
		if err := f.p.handleOutgoingEcho(context.Background(), instance, event); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM messages WHERE direction = 'OUTGOING'`); n != 2 {
		t.Fatal("second account echo suppressed")
	}
	event := &whatsapp.Event{Timestamp: now, Status: &whatsapp.StatusUpdate{ExternalID: "shared-outgoing", Status: whatsapp.StatusRead}}
	if err := f.p.handleStatus(context.Background(), 2, event); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM messages WHERE direction = 'OUTGOING' AND whatsapp_instance_id = 1 AND status = 'SENT'`); n != 1 {
		t.Fatal("secondary status changed primary message")
	}
	if n := f.count(t, `SELECT count(*) FROM messages WHERE direction = 'OUTGOING' AND whatsapp_instance_id = 2 AND status = 'READ'`); n != 1 {
		t.Fatal("secondary scoped receipt did not advance")
	}
	// Provider acknowledgements with equal IDs must also retain both receipts.
	contact, err := f.p.contacts.GetByPhone(context.Background(), nil, "77011234568")
	if err != nil {
		t.Fatal(err)
	}
	second := int64(2)
	m := &domain.Message{ContactID: contact.ID, WhatsAppInstanceID: &second, Direction: domain.DirectionOutgoing, Type: "TEXT", Text: "API", Status: domain.MessageStatusPending}
	if err := f.p.messages.Create(context.Background(), nil, m); err != nil {
		t.Fatal(err)
	}
	if err := f.p.messages.MarkSent(context.Background(), nil, m.ID, "primary-api", now); err != nil {
		t.Fatal(err)
	}
	first := int64(1)
	m2 := &domain.Message{ContactID: contact.ID, WhatsAppInstanceID: &first, Direction: domain.DirectionOutgoing, Type: "TEXT", Text: "API", Status: domain.MessageStatusPending}
	if err := f.p.messages.Create(context.Background(), nil, m2); err != nil {
		t.Fatal(err)
	}
	if err := f.p.messages.MarkSent(context.Background(), nil, m2.ID, "primary-api", now); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM messages WHERE external_id LIKE '%primary-api' AND sent_at IS NOT NULL`); n != 2 {
		t.Fatal("receipt collision lost a submission")
	}
}
