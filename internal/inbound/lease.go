package inbound

import (
	"context"
	"log/slog"
	"time"
)

func (p *Processor) keepLease(parent context.Context, e StoredEvent) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tag, err := p.repo.db.Exec(ctx, `UPDATE webhook_events SET updated_at = now()
				  WHERE id = $1 AND status = 'PROCESSING' AND attempts = $2`, e.ID, e.Attempts)
				if err != nil || tag.RowsAffected() == 0 {
					if err != nil {
						p.log.Error("renewing incoming lease failed", slog.String("error", err.Error()))
					}
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(); <-done }
}
