package outbound

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/messaging"
)

// blockingSender holds one account's send open until it is released, and
// records which account every send belonged to.
type blockingSender struct {
	mu      sync.Mutex
	seen    []int64
	blockOn int64
	release chan struct{}
}

func (b *blockingSender) Send(ctx context.Context, contact *domain.Contact, out messaging.Outbound) (*domain.Message, error) {
	b.mu.Lock()
	b.seen = append(b.seen, out.WhatsAppInstanceID)
	blocked := out.WhatsAppInstanceID == b.blockOn
	release := b.release
	b.mu.Unlock()

	if blocked && release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &domain.Message{}, nil
}

func (b *blockingSender) accounts() []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int64(nil), b.seen...)
}

func fastPace() config.Outbound {
	return config.Outbound{Workers: 1, MinDelay: 0, MaxDelay: 0}
}

func testRouter(sender Sender) *Router {
	return NewRouter([]InstanceSpec{
		{ID: 1, Key: "primary", Outbound: fastPace()},
		{ID: 2, Key: "secondary", Outbound: fastPace()},
	}, sender, discardLogger())
}

// TestRouterKeepsOneQueuePerAccount is the reason the router exists.
//
// With a single shared gate, a message stuck in flight on one number would
// hold the only worker slot and every other number's messages would wait
// behind it. The first number's customers -- the ones already in production --
// would receive their reminders late because of a campaign they have nothing
// to do with. Here the second account's send is held open indefinitely and the
// first account must still get through.
func TestRouterKeepsOneQueuePerAccount(t *testing.T) {
	sender := &blockingSender{blockOn: 2, release: make(chan struct{})}
	router := testRouter(sender)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stuck := make(chan struct{})
	go func() {
		defer close(stuck)
		_, _ = router.Send(ctx, &domain.Contact{}, messaging.Outbound{WhatsAppInstanceID: 2})
	}()

	// Give the blocked send time to occupy its own queue's only slot.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(sender.accounts()) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the secondary send never started")
		}
		time.Sleep(time.Millisecond)
	}

	done := make(chan error, 1)
	go func() {
		_, err := router.Send(ctx, &domain.Contact{}, messaging.Outbound{WhatsAppInstanceID: 1})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("primary send failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stuck send on one account blocked the other account's queue")
	}

	close(sender.release)
	<-stuck
}

// TestRouterRefusesAnUnknownAccount pins down the failure mode that matters
// most: no silent fallback.
//
// Delivering a message meant for one number through another is the worst
// outcome this subsystem could produce, so an account the process does not
// serve is an error rather than a default.
func TestRouterRefusesAnUnknownAccount(t *testing.T) {
	sender := &blockingSender{}
	router := testRouter(sender)

	_, err := router.Send(context.Background(), &domain.Contact{},
		messaging.Outbound{WhatsAppInstanceID: 99})
	if err == nil {
		t.Fatal("sending through an account this process does not serve must fail")
	}
	if len(sender.accounts()) != 0 {
		t.Fatalf("nothing should have reached the sender, got %v", sender.accounts())
	}
}

// TestRouterTreatsAZeroAccountAsPrimary covers rows written before migration
// 0007, and any caller that predates the field. They must stay deliverable
// rather than piling up in the queue with an unroutable owner.
func TestRouterTreatsAZeroAccountAsPrimary(t *testing.T) {
	sender := &blockingSender{}
	router := testRouter(sender)

	if _, err := router.Send(context.Background(), &domain.Contact{},
		messaging.Outbound{}); err != nil {
		t.Fatalf("an unset account must fall back to primary: %v", err)
	}

	seen := sender.accounts()
	if len(seen) != 1 || seen[0] != 0 {
		t.Fatalf("expected exactly one send, got %v", seen)
	}
}

// TestRouterGatesAreDistinct is the structural assertion behind the isolation:
// two accounts must not be handed the same pacing timeline.
func TestRouterGatesAreDistinct(t *testing.T) {
	router := testRouter(&blockingSender{})

	primary, ok := router.Gate(1)
	if !ok {
		t.Fatal("no queue for the primary account")
	}
	secondary, ok := router.Gate(2)
	if !ok {
		t.Fatal("no queue for the secondary account")
	}
	if primary == secondary {
		t.Fatal("the two accounts share one queue; a burst on one would delay the other")
	}
	if _, ok := router.Gate(99); ok {
		t.Fatal("an unknown account must not have a queue")
	}
}
