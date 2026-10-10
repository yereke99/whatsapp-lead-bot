package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/messaging"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
	"github.com/ayran/whatsapp-automation/internal/whatsapp/greenapi"
	"github.com/ayran/whatsapp-automation/pkg/phone"
)

type RecoveryItem struct {
	EventID           uuid.UUID `json:"event_id"`
	InstanceID        int64     `json:"instance_id"`
	ExternalMessageID string    `json:"external_message_id"`
	ReceivedAt        time.Time `json:"received_at"`
}

type RecoveryPreview struct {
	Found          int            `json:"incoming_events_found"`
	AlreadyReplied int            `json:"already_replied"`
	NotMatching    int            `json:"not_matching_active_campaign"`
	Waiting        int            `json:"already_queued"`
	Ineligible     int            `json:"ineligible"`
	Ambiguous      int            `json:"ambiguous_legacy_events"`
	Eligible       int            `json:"eligible_for_recovery"`
	BatchSize      int            `json:"batch_size"`
	Items          []RecoveryItem `json:"items"`
	ReviewItems    []RecoveryItem `json:"manual_review_items"`
}

const storedEventColumns = `id, COALESCE(whatsapp_instance_id, 1), provider,
 event_type, dedupe_key, payload, attempts, external_id, received_at,
 trigger_result, status, error`

func scanStored(row interface{ Scan(...any) error }, e *StoredEvent) error {
	return row.Scan(&e.ID, &e.WhatsAppInstanceID, &e.Provider, &e.EventType,
		&e.DedupeKey, &e.Payload, &e.Attempts, &e.ExternalID, &e.ReceivedAt,
		&e.TriggerResult, &e.Status, &e.Error)
}

func (r *Repository) Get(ctx context.Context, id uuid.UUID) (*StoredEvent, error) {
	var e StoredEvent
	if err := scanStored(r.db.QueryRow(ctx, `SELECT `+storedEventColumns+` FROM webhook_events WHERE id = $1`, id), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// PreviewRecovery only reads. Keyset pages bound memory even with a large
// history; the response contains at most one configured batch of event IDs.
// Requeue rechecks every ID because this preview can race with live workers.
func (p *Processor) PreviewRecovery(ctx context.Context, instanceID int64) (*RecoveryPreview, error) {
	out := &RecoveryPreview{BatchSize: p.recoveryBatchSize(), Items: []RecoveryItem{}, ReviewItems: []RecoveryItem{}}
	var cursorID string
	var cursorAt time.Time
	for {
		rows, err := p.repo.db.Query(ctx, `SELECT `+storedEventColumns+` FROM webhook_events
		  WHERE event_type = 'incomingMessageReceived' AND ($1 = 0 OR COALESCE(whatsapp_instance_id, 1) = $1)
		    AND ($2 = '' OR received_at > $3 OR (received_at = $3 AND id > $2))
		  ORDER BY received_at, id LIMIT $4`, instanceID, cursorID, cursorAt, p.recoveryBatchSize())
		if err != nil {
			return nil, err
		}
		var batch []StoredEvent
		for rows.Next() {
			var e StoredEvent
			if err := scanStored(rows, &e); err != nil {
				rows.Close()
				return nil, err
			}
			batch = append(batch, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return out, nil
		}
		for _, e := range batch {
			kind, _, err := p.recoveryEligibility(ctx, e)
			if err != nil {
				return nil, err
			}
			out.Found++
			switch kind {
			case "replied":
				out.AlreadyReplied++
			case "not_matching":
				out.NotMatching++
			case "waiting":
				out.Waiting++
			case "ineligible":
				out.Ineligible++
			case "ambiguous":
				out.Ambiguous++
				if len(out.ReviewItems) < out.BatchSize {
					out.ReviewItems = append(out.ReviewItems, RecoveryItem{e.ID, e.WhatsAppInstanceID, e.ExternalID, e.ReceivedAt})
				}
			case "eligible":
				out.Eligible++
				if len(out.Items) < out.BatchSize {
					out.Items = append(out.Items, RecoveryItem{e.ID, e.WhatsAppInstanceID, e.ExternalID, e.ReceivedAt})
				}
			}
		}
		last := batch[len(batch)-1]
		cursorID, cursorAt = last.ID.String(), last.ReceivedAt
	}
}

// recoveryEligibility is intentionally conservative about legacy courtesy
// replies: without an event correlation the old records cannot prove which
// repeat-trigger reply was sent. Such records require operator investigation.
func (p *Processor) recoveryEligibility(ctx context.Context, e StoredEvent) (string, *uuid.UUID, error) {
	if strings.Contains(e.Error, "persistSendReceipt") {
		return "ambiguous", nil, nil
	}
	event, err := greenapi.ParseWebhook(e.Payload)
	if err != nil || event.Kind != whatsapp.EventIncomingMessage || event.Message == nil || event.Message.Type != whatsapp.TypeText || phone.IsGroupChatID(event.ChatID) {
		return "not_matching", nil, nil
	}
	if e.ExternalID == "" {
		return "ambiguous", nil, nil
	}
	var canonical uuid.UUID
	if err := p.repo.db.QueryRow(ctx, `SELECT id FROM webhook_events
	  WHERE provider = $1 AND COALESCE(whatsapp_instance_id, 1) = $2
	    AND external_id = $3 AND event_type = 'incomingMessageReceived'
	  ORDER BY created_at, id LIMIT 1`, e.Provider, e.WhatsAppInstanceID, e.ExternalID).Scan(&canonical); err != nil {
		return "", nil, err
	}
	if canonical != e.ID {
		return "ineligible", nil, nil
	}
	match, err := p.campaigns.MatchTrigger(ctx, nil, e.WhatsAppInstanceID, event.Message.Text)
	if err != nil {
		return "", nil, err
	}
	if match == nil {
		return "not_matching", nil, nil
	}
	digits, err := phone.FromChatID(event.ChatID)
	if err != nil {
		return "ineligible", nil, nil
	}
	contact, err := p.contacts.GetByPhone(ctx, nil, digits)
	if err != nil {
		return "", nil, err
	}
	var initialJobID *uuid.UUID
	if e.TriggerResult != nil {
		var result campaigns.EnrollResult
		if err := json.Unmarshal([]byte(*e.TriggerResult), &result); err != nil {
			return "ambiguous", nil, nil
		}
		if result.Campaign == nil || result.Campaign.ID != match.CampaignID {
			return "ineligible", nil, nil
		}
		initialJobID = result.InitialJobID
		if result.Action == campaigns.ActionSpecialMessage {
			sent, err := p.messages.SuccessfulOutbound(ctx, nil, &e.ID)
			if err != nil {
				return "", nil, err
			}
			if sent != nil {
				return "replied", nil, nil
			}
		} else if initialJobID == nil {
			return "ineligible", nil, nil
		}
	} else if contact != nil {
		enrollment, err := p.campaigns.Repo().FindEnrollment(ctx, nil, match.CampaignID, contact.ID)
		if err != nil {
			return "", nil, err
		}
		if enrollment != nil {
			// A welcome already sent on any run must never be recreated during
			// legacy recovery, even if the campaign now says RESTART.
			var greeted bool
			err := p.repo.db.QueryRow(ctx, `SELECT EXISTS (
			  SELECT 1 FROM scheduled_messages sm JOIN campaign_steps cs ON cs.id = sm.campaign_step_id
			  WHERE sm.enrollment_id = $1 AND cs.schedule_kind = 'ON_TRIGGER'
			    AND (sm.status = 'SENT' OR EXISTS (SELECT 1 FROM messages m WHERE m.scheduled_message_id = sm.id
			      AND (m.sent_at IS NOT NULL OR m.status IN ('SENT','DELIVERED','READ')))))`, enrollment.ID).Scan(&greeted)
			if err != nil {
				return "", nil, err
			}
			if greeted {
				return "replied", nil, nil
			}
			initialJobID, err = p.campaigns.InitialReplyJob(ctx, nil, enrollment)
			if err != nil {
				return "", nil, err
			}
			if initialJobID == nil {
				return "ambiguous", nil, nil
			}
			if enrollment.Status != domain.EnrollmentActive && enrollment.Status != domain.EnrollmentCompleted {
				return "ineligible", nil, nil
			}
		}
	}
	if initialJobID != nil {
		outcome := p.repo.InitialReplyOutcome(ctx, *initialJobID)
		if outcome == nil {
			return "replied", initialJobID, nil
		}
		if !errors.Is(outcome, errAwaitingReply) {
			var status, reason, lastError string
			if err := p.repo.db.QueryRow(ctx, `SELECT status, cancel_reason, last_error FROM scheduled_messages WHERE id = $1`, *initialJobID).Scan(&status, &reason, &lastError); err != nil {
				if sqlite.IsNoRows(err) {
					return "ineligible", nil, nil
				}
				return "", nil, err
			}
			if strings.Contains(lastError, "persistSendReceipt") {
				return "ambiguous", nil, nil
			}
			if status == "CANCELLED" && reason != "send window expired" {
				return "ineligible", nil, nil
			}
		}
	}
	uncertain, err := p.messages.UncertainOutbound(ctx, initialJobID, &e.ID)
	if err != nil {
		return "", nil, err
	}
	if uncertain {
		return "ambiguous", nil, nil
	}
	if contact != nil {
		optedOut, err := p.contacts.IsOptedOutOn(ctx, nil, e.WhatsAppInstanceID, contact.ID)
		if err != nil {
			return "", nil, err
		}
		stop, err := p.campaigns.IsUnsubscribe(ctx, nil, contact.ID, e.WhatsAppInstanceID, event.Message.Text)
		if err != nil {
			return "", nil, err
		}
		if !contact.CanBeContacted() || optedOut || stop {
			return "ineligible", nil, nil
		}
	}
	contactID := uuid.Nil
	if contact != nil {
		contactID = contact.ID
	}
	// Historical replay requires more than the absence of a linked welcome
	// receipt: phone replies and unrelated successful outgoing messages count.
	if e.Status != domain.WebhookReceived && e.Status != domain.WebhookProcessing {
		state, err := p.history.Check(ctx, e.WhatsAppInstanceID, contactID, event.ChatID, e.ExternalID, event.Timestamp)
		if err != nil {
			return "", nil, err
		}
		if state != messaging.ReplyUnanswered {
			return state, nil, nil
		}
	}
	// PENDING or leased work is already handled by normal startup recovery.
	// Historical PROCESSED records still need an explicit requeue/checkpoint.
	if e.Status == domain.WebhookReceived || e.Status == domain.WebhookProcessing {
		return "waiting", nil, nil
	}
	return "eligible", initialJobID, nil
}

type RecoveryResult struct {
	Requested int `json:"requested"`
	Requeued  int `json:"requeued"`
	Skipped   int `json:"skipped"`
}

func (p *Processor) RequeueRecovery(ctx context.Context, ids []uuid.UUID) (*RecoveryResult, error) {
	if len(ids) == 0 || len(ids) > p.recoveryBatchSize() {
		return nil, fmt.Errorf("provide 1 to %d previewed event IDs", p.recoveryBatchSize())
	}
	out := &RecoveryResult{Requested: len(ids)}
	p.log.Info("recovery_batch_started", slog.Int("count", len(ids)))
	for _, id := range ids {
		e, err := p.repo.Get(ctx, id)
		if sqlite.IsNoRows(err) {
			out.Skipped++
			continue
		}
		if err != nil {
			return nil, err
		}
		kind, jobID, err := p.recoveryEligibility(ctx, *e)
		if err != nil {
			return nil, err
		}
		if kind != "eligible" {
			out.Skipped++
			continue
		}
		requeued := false
		err = p.repo.db.InTx(ctx, func(tx sqlite.Querier) error {
			// Compare-and-set and receipt checks make a stale preview harmless.
			tag, err := tx.Exec(ctx, `UPDATE webhook_events SET status = 'RECEIVED',
			  attempts = 0, next_attempt_at = NULL, processed_at = NULL,
			  error = 'manual trigger recovery', updated_at = now()
			  WHERE id = $1 AND status IN ('PROCESSED','FAILED','IGNORED')
			    AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.direction = 'OUTGOING'
			      AND (m.incoming_event_id = $1 OR ($2 IS NOT NULL AND m.scheduled_message_id = $2))
			      AND (m.sent_at IS NOT NULL OR m.status IN ('SENT','DELIVERED','READ')))
			    AND NOT EXISTS (SELECT 1 FROM scheduled_messages WHERE id = $2 AND status = 'SENT')
			    AND NOT EXISTS (SELECT 1 FROM messages m JOIN contacts c ON c.id = m.contact_id
			      WHERE c.chat_id = json_extract(webhook_events.payload, '$.senderData.chatId')
			        AND m.direction = 'OUTGOING' AND COALESCE(m.whatsapp_instance_id, 1) = COALESCE(webhook_events.whatsapp_instance_id, 1)
			        AND COALESCE(m.sent_at, m.created_at) >= webhook_events.received_at
			        AND (m.sent_at IS NOT NULL OR m.status IN ('SENT','DELIVERED','READ')))`, id, jobID)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			if jobID != nil && e.TriggerResult != nil {
				if err := p.campaigns.RecoverInitialReply(ctx, tx, *jobID); err != nil {
					return err
				}
			}
			requeued = true
			return nil
		})
		if err != nil {
			return nil, err
		}
		if requeued {
			out.Requeued++
			p.log.Info("incoming_event_recovered", slog.Int64("instance_id", e.WhatsAppInstanceID),
				slog.String("external_message_id", e.ExternalID), slog.String("event_id", id.String()),
				slog.String("recovery_reason", "manual preview and requeue"))
		} else {
			out.Skipped++
		}
	}
	p.log.Info("recovery_batch_completed", slog.Int("requeued", out.Requeued), slog.Int("skipped", out.Skipped))
	return out, nil
}
