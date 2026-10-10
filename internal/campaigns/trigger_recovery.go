package campaigns

import (
	"context"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
)

// InitialReplyJob identifies the first configured ON_TRIGGER step, using the
// existing run/step identity. Later trigger-delayed and webinar steps keep all
// their existing scheduling and expiry rules.
func (s *Service) InitialReplyJob(ctx context.Context, q sqlite.Querier, e *domain.Enrollment) (*uuid.UUID, error) {
	if q == nil {
		q = s.repo.DB()
	}
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT sm.id FROM scheduled_messages sm
		JOIN campaign_steps cs ON cs.id = sm.campaign_step_id
		WHERE sm.enrollment_id = $1 AND sm.run_number = $2
		  AND cs.schedule_kind = 'ON_TRIGGER'
		ORDER BY cs.offset_seconds, cs.order_index, cs.id LIMIT 1`, e.ID, e.RunNumber).Scan(&id)
	if sqlite.IsNoRows(err) {
		return nil, nil
	}
	return &id, err
}

// Only a missed greeting cancelled by the generic send-window reaper, or a
// failed send, can be revived. Consent, audience and operator cancellations
// stay cancelled. Successful message receipts always win over queue state.
func (s *Service) RecoverInitialReply(ctx context.Context, q sqlite.Querier, jobID uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE scheduled_messages SET status = 'PENDING', scheduled_at = now(),
		  next_attempt_at = NULL, locked_at = NULL, locked_by = NULL,
		  last_error = 'missing trigger reply recovered'
		WHERE id = $1 AND (status = 'FAILED' OR
		  (status = 'CANCELLED' AND cancel_reason = 'send window expired'))
		  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.scheduled_message_id = $1
		    AND (m.sent_at IS NOT NULL OR m.status IN ('SENT','DELIVERED','READ')))`, jobID)
	return err
}
