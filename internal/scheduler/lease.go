package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// One heartbeat per active worker, including time spent in outbound pacing.
// The lease is not granted to a batch of work waiting in memory.
func (w *Worker) keepLease(parent context.Context, id uuid.UUID) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	interval := w.cfg.LockTimeout / 3
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tag, err := w.repo.db.Exec(ctx, `UPDATE scheduled_messages SET locked_at = now()
				  WHERE id = $1 AND status = 'PROCESSING' AND locked_by = $2`, id, w.workerID)
				if err != nil || tag.RowsAffected() == 0 {
					if err != nil {
						w.log.Error("renewing job lease failed", slog.String("error", err.Error()))
					}
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(); <-done }
}
