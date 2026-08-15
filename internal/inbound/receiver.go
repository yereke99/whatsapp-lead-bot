package inbound

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/whatsapp/greenapi"
)

// notificationClient is the slice of the provider the receiver needs.
//
// Depending on this rather than on the concrete client keeps the polling loop
// testable without HTTP, and keeps provider requests behind one abstraction.
type notificationClient interface {
	ReceiveNotification(ctx context.Context, receiveTimeout time.Duration) (*greenapi.Notification, error)
	DeleteNotification(ctx context.Context, receiptID int64) error
}

// ingester is the half of Processor the receiver drives.
type ingester interface {
	Ingest(ctx context.Context, body []byte) (bool, error)
}

// Receiver drains the provider's notification queue.
//
// It replaces the webhook endpoint the platform used to expose: instead of the
// provider pushing events at a public URL, this pulls them. The installation
// therefore needs no inbound connectivity, no TLS certificate and no public
// hostname to receive WhatsApp messages.
type Receiver struct {
	client    notificationClient
	processor ingester
	cfg       config.GreenAPI
	log       *slog.Logger

	// backoffFloor is the first delay after a provider failure. Tests shorten
	// it; nothing else changes it.
	backoffFloor time.Duration
}

func NewReceiver(client notificationClient, processor ingester, cfg config.GreenAPI, log *slog.Logger) *Receiver {
	return &Receiver{
		client:       client,
		processor:    processor,
		cfg:          cfg,
		log:          log.With(slog.String("component", "greenapi_receiver")),
		backoffFloor: minBackoff,
	}
}

// Backoff bounds for provider failures. The ceiling keeps a long outage from
// stretching recovery out to minutes once the provider returns.
const (
	minBackoff = 1 * time.Second
	maxBackoff = 30 * time.Second
)

// Run polls until the context is cancelled.
//
// Each pass takes at most one notification, hands it to the processor and
// acknowledges it. The provider's queue is strictly ordered and hands out one
// entry at a time, so this loop is intentionally sequential: concurrency lives
// behind Ingest, where the worker pool drains the durable queue.
func (r *Receiver) Run(ctx context.Context) {
	r.log.Info("[GREENAPI] polling started",
		slog.Duration("poll_interval", r.cfg.PollInterval),
		slog.Duration("receive_timeout", r.cfg.ReceiveTimeout))

	backoff := r.floor()

	for {
		if ctx.Err() != nil {
			r.log.Info("[GREENAPI] polling stopped")
			return
		}

		notification, err := r.client.ReceiveNotification(ctx, r.cfg.ReceiveTimeout)
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// Shutdown, not a provider fault.
			if ctx.Err() != nil {
				r.log.Info("[GREENAPI] polling stopped")
				return
			}
			continue

		case err != nil:
			// A provider outage must not spin the CPU or crash the service.
			r.log.Error("[GREENAPI] receive failed",
				slog.String("error", err.Error()),
				slog.Duration("retry_in", backoff))
			if !sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		backoff = r.floor()

		if notification == nil {
			// An empty queue is the normal idle state. The receive call already
			// blocked server-side for receive_timeout, so this is a short pause
			// rather than a busy loop.
			if !sleep(ctx, r.cfg.PollInterval) {
				return
			}
			continue
		}

		r.handle(ctx, notification)
	}
}

// handle ingests one notification and acknowledges it.
//
// Acknowledgement happens once the event is durably stored, not once the
// campaign work has finished. The queue is FIFO and redelivers the same entry
// until it is deleted, so waiting for business processing would let a single
// failing message block every message behind it. Once Ingest returns, the event
// is committed to the local queue, where the worker pool retries it and stale
// locks are recovered after a crash — deleting is safe, and nothing is lost.
func (r *Receiver) handle(ctx context.Context, notification *greenapi.Notification) {
	log := r.log.With(slog.Int64("receipt_id", notification.ReceiptID))
	log.Debug("[GREENAPI] notification received")

	accepted, err := r.processor.Ingest(ctx, notification.Body)
	switch {
	case err != nil && isMalformed(err):
		// A payload we cannot parse will never become parseable. Leaving it in
		// place would wedge the queue permanently, so drop it deliberately and
		// loudly.
		log.Error("[GREENAPI] discarding unparseable notification",
			slog.String("error", err.Error()))

	case err != nil:
		// Storage failed: keep the notification queued and try again.
		log.Error("[GREENAPI] ingest failed; notification kept for retry",
			slog.String("error", err.Error()))
		return

	case accepted:
		log.Info("[GREENAPI] notification queued")

	default:
		log.Info("[GREENAPI] duplicate notification ignored")
	}

	// Deleting uses a context detached from shutdown: the event is already
	// stored, and skipping the delete would replay it on the next start.
	deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	if err := r.client.DeleteNotification(deleteCtx, notification.ReceiptID); err != nil {
		// Harmless: the entry reappears and deduplication rejects it.
		log.Warn("[GREENAPI] delete failed; notification will be redelivered",
			slog.String("error", err.Error()))
		return
	}
	log.Debug("[GREENAPI] notification deleted")
}

// isMalformed reports whether an ingest error is a permanent parse failure
// rather than a transient storage problem.
func isMalformed(err error) bool {
	var parseErr *greenapi.ParseError
	return errors.As(err, &parseErr)
}

// floor is the delay the first retry after a failure uses.
func (r *Receiver) floor() time.Duration {
	if r.backoffFloor > 0 {
		return r.backoffFloor
	}
	return minBackoff
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}

// sleep waits for d, reporting false if the context ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
