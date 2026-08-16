package outbound

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/messaging"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recorder stands in for the provider, noting when each send happened and how
// many were running at the same moment.
type recorder struct {
	mu       sync.Mutex
	times    []time.Time
	inFlight int
	peak     int

	work time.Duration
}

func (r *recorder) Send(ctx context.Context, contact *domain.Contact, out messaging.Outbound) (*domain.Message, error) {
	r.mu.Lock()
	r.times = append(r.times, time.Now())
	r.inFlight++
	if r.inFlight > r.peak {
		r.peak = r.inFlight
	}
	r.mu.Unlock()

	if r.work > 0 {
		time.Sleep(r.work)
	}

	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()

	return &domain.Message{}, nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.times)
}

// gaps returns the intervals between consecutive sends.
func (r *recorder) gaps() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]time.Duration, 0, len(r.times))
	for i := 1; i < len(r.times); i++ {
		out = append(out, r.times[i].Sub(r.times[i-1]))
	}
	return out
}

// TestGateSpacesSendsOut is the burst requirement: a hundred contacts due at
// the same second must not turn into a hundred simultaneous API calls.
func TestGateSpacesSendsOut(t *testing.T) {
	rec := &recorder{}
	gate := New(config.Outbound{
		Workers:  1,
		MinDelay: 20 * time.Millisecond,
		MaxDelay: 40 * time.Millisecond,
	}, rec, discardLogger())

	ctx := context.Background()
	contact := &domain.Contact{}

	// Fire them all at once, the way a batch of due jobs arrives.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := gate.Send(ctx, contact, messaging.Outbound{}); err != nil {
				t.Errorf("send: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := rec.count(); got != 10 {
		t.Fatalf("delivered %d messages, want 10", got)
	}
	if rec.peak > 1 {
		t.Errorf("%d sends ran at once, want at most 1", rec.peak)
	}

	// Timers are not exact; the assertion is that the pacing happened at all,
	// not that it hit the millisecond.
	const tolerance = 5 * time.Millisecond
	for i, gap := range rec.gaps() {
		if gap < 20*time.Millisecond-tolerance {
			t.Errorf("gap %d was %v, want at least the configured minimum", i, gap)
		}
	}
}

// TestGateBoundsConcurrency checks the worker limit independently of pacing.
func TestGateBoundsConcurrency(t *testing.T) {
	rec := &recorder{work: 20 * time.Millisecond}
	gate := New(config.Outbound{Workers: 2, MinDelay: 0, MaxDelay: 0}, rec, discardLogger())

	ctx := context.Background()
	contact := &domain.Contact{}

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := gate.Send(ctx, contact, messaging.Outbound{}); err != nil {
				t.Errorf("send: %v", err)
			}
		}()
	}
	wg.Wait()

	if rec.peak > 2 {
		t.Errorf("%d sends ran at once, want at most 2", rec.peak)
	}
	if got := rec.count(); got != 12 {
		t.Errorf("delivered %d messages, want 12", got)
	}
}

// TestGateStopsOnShutdown makes sure a queued send waiting out its pause gives
// up when the process is going down, rather than sending late.
func TestGateStopsOnShutdown(t *testing.T) {
	rec := &recorder{}
	gate := New(config.Outbound{
		Workers:  1,
		MinDelay: 2 * time.Second,
		MaxDelay: 2 * time.Second,
	}, rec, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	contact := &domain.Contact{}

	// The first send goes straight through and reserves the next slot.
	if _, err := gate.Send(ctx, contact, messaging.Outbound{}); err != nil {
		t.Fatalf("first send: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := gate.Send(ctx, contact, messaging.Outbound{})
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("the pending send completed after shutdown, want a cancellation error")
		}
	case <-time.After(time.Second):
		t.Error("the pending send did not return after the context was cancelled")
	}

	if got := rec.count(); got != 1 {
		t.Errorf("delivered %d messages, want 1", got)
	}
}
