package inbound

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
)

func (r *Repository) InitialReplyOutcome(ctx context.Context, jobID uuid.UUID) error {
	var status, reason, lastError string
	var sent bool
	err := r.db.QueryRow(ctx, `SELECT status, cancel_reason, last_error,
	  EXISTS (SELECT 1 FROM messages m WHERE m.scheduled_message_id = sm.id
	    AND (m.sent_at IS NOT NULL OR m.status IN ('SENT','DELIVERED','READ')))
	  FROM scheduled_messages sm WHERE id = $1`, jobID).Scan(&status, &reason, &lastError, &sent)
	if err != nil {
		if sqlite.IsNoRows(err) {
			return &whatsapp.Error{Op: "triggerReply", Message: "initial reply job no longer exists"}
		}
		return fmt.Errorf("read initial reply state: %w", err)
	}
	if sent || status == "SENT" || (status == "CANCELLED" && reason == "trigger already answered") {
		return nil
	}
	if status == "PENDING" || status == "PROCESSING" {
		return errAwaitingReply
	}
	return &whatsapp.Error{Op: "triggerReply", Message: "reply " + status + ": " + reason + lastError}
}

func (p *Processor) recoveryBatchSize() int {
	if p.cfg.RecoveryBatchSize > 0 {
		return p.cfg.RecoveryBatchSize
	}
	return 150
}

// Exhausted transient event failures are retained and retried in bounded
// sweeps. Historical PROCESSED records are only recovered by explicit preview
// and requeue, because their old business decisions were not checkpointed.
func (p *Processor) recoverRetryable(ctx context.Context) {
	delay := p.cfg.RetryMaxDelay
	if delay <= 0 {
		delay = 30 * time.Minute
	}
	tag, err := p.repo.db.Exec(ctx, `UPDATE webhook_events SET status = 'RECEIVED',
	  attempts = 0, next_attempt_at = NULL, processed_at = NULL, updated_at = now()
	  WHERE id IN (SELECT id FROM webhook_events WHERE status = 'FAILED'
	    AND event_type = 'incomingMessageReceived'
	    AND error NOT LIKE 'permanent failure:%' AND updated_at <= $1
	    AND error NOT LIKE '%persistSendReceipt%'
	    ORDER BY received_at, id LIMIT $2)`, time.Now().UTC().Add(-delay), p.recoveryBatchSize())
	if err != nil {
		p.log.Error("incoming recovery failed", slog.String("error", err.Error()))
		return
	}
	if tag.RowsAffected() > 0 {
		p.log.Info("incoming_event_recovered", slog.Int64("count", tag.RowsAffected()), slog.String("recovery_reason", "retryable failure after backoff"))
	}
}
