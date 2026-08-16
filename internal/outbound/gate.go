// Package outbound is the single controlled path from the platform to
// WhatsApp.
//
// Everything the scheduler sends passes through Gate, which decides how many
// messages may be in flight and how far apart they go out. Without it each
// scheduler worker would call the provider the moment it claimed a job, and a
// hundred contacts due at 21:00 would become a hundred near-simultaneous API
// calls from one WhatsApp account.
//
// The goal is a calm, predictable sending rate — not evading anything. There
// is no account rotation, no fingerprinting and no attempt to disguise
// automated traffic; the queue simply refuses to go faster than the configured
// pace, and holds work rather than dropping it when a limit is reached.
//
// Message ordering per contact is not enforced here. It is enforced one level
// down, in the claim query, which never hands out a second job for a contact
// while one is in flight. Gate therefore only has to care about global pace.
package outbound

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/messaging"
)

// Sender is the delivery mechanism the gate wraps.
type Sender interface {
	Send(ctx context.Context, contact *domain.Contact, out messaging.Outbound) (*domain.Message, error)
}

type Gate struct {
	sender Sender
	log    *slog.Logger

	// slots bounds how many sends may be in flight at once.
	slots chan struct{}

	mu       sync.Mutex
	nextFree time.Time
	minDelay time.Duration
	maxDelay time.Duration
}

func New(cfg config.Outbound, sender Sender, log *slog.Logger) *Gate {
	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}
	minDelay := cfg.MinDelay
	if minDelay < 0 {
		minDelay = 0
	}
	maxDelay := cfg.MaxDelay
	if maxDelay < minDelay {
		maxDelay = minDelay
	}

	g := &Gate{
		sender:   sender,
		log:      log.With(slog.String("component", "outbound")),
		slots:    make(chan struct{}, workers),
		minDelay: minDelay,
		maxDelay: maxDelay,
	}
	for i := 0; i < workers; i++ {
		g.slots <- struct{}{}
	}

	g.log.Info("outbound queue configured",
		slog.Int("workers", workers),
		slog.Duration("min_delay", minDelay),
		slog.Duration("max_delay", maxDelay))

	return g
}

// Send waits for a free slot and for the pacing interval, then delivers.
//
// The wait is a blocked goroutine on a timer, not a busy loop, and it is
// always cancellable: a shutdown mid-pause returns the context error instead
// of sending late.
func (g *Gate) Send(ctx context.Context, contact *domain.Contact, out messaging.Outbound) (*domain.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.slots:
	}
	defer func() { g.slots <- struct{}{} }()

	if err := g.pace(ctx); err != nil {
		return nil, err
	}

	return g.sender.Send(ctx, contact, out)
}

// pace reserves the next sending slot on the shared timeline and sleeps until
// it arrives.
//
// The reservation is taken under the lock and the wait happens outside it, so
// concurrent callers queue up behind each other in the order they arrived
// instead of all waking at the same instant.
func (g *Gate) pace(ctx context.Context) error {
	now := time.Now()

	g.mu.Lock()
	sendAt := now
	if g.nextFree.After(sendAt) {
		sendAt = g.nextFree
	}
	g.nextFree = sendAt.Add(g.interval())
	g.mu.Unlock()

	wait := time.Until(sendAt)
	if wait <= 0 {
		return nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// interval draws the gap before the next send. Spreading it across the
// configured range keeps a long queue from going out on an exact metronome,
// which is neither necessary nor realistic for a human-paced account.
func (g *Gate) interval() time.Duration {
	if g.maxDelay <= g.minDelay {
		return g.minDelay
	}
	spread := g.maxDelay - g.minDelay
	return g.minDelay + time.Duration(rand.Int63n(int64(spread)+1))
}
