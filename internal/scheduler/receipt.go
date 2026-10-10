package scheduler

import (
	"context"

	"github.com/google/uuid"
)

// ReconcileReceipt closes the local crash gap between messages.MarkSent and
// scheduled_messages.MarkSent without calling the provider again.
func (r *Repository) ReconcileReceipt(ctx context.Context, jobID uuid.UUID) (bool, error) {
	tag, err := r.db.Exec(ctx, `UPDATE scheduled_messages SET status = 'SENT',
	  message_id = (SELECT m.id FROM messages m WHERE m.scheduled_message_id = $1
	    AND (m.sent_at IS NOT NULL OR m.status IN ('SENT','DELIVERED','READ')) ORDER BY m.created_at LIMIT 1),
	  sent_at = (SELECT COALESCE(m.sent_at, m.created_at) FROM messages m WHERE m.scheduled_message_id = $1
	    AND (m.sent_at IS NOT NULL OR m.status IN ('SENT','DELIVERED','READ')) ORDER BY m.created_at LIMIT 1),
	  locked_by = NULL, locked_at = NULL, last_error = '', updated_at = now()
	  WHERE id = $1 AND status = 'PROCESSING'
	    AND EXISTS (SELECT 1 FROM messages m WHERE m.scheduled_message_id = $1
	      AND (m.sent_at IS NOT NULL OR m.status IN ('SENT','DELIVERED','READ')))`, jobID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
