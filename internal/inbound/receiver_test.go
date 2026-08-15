package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/whatsapp/greenapi"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeClient scripts a sequence of receive results and records deletions.
type fakeClient struct {
	mu       sync.Mutex
	queue    []*greenapi.Notification
	errs     []error
	deleted  []int64
	receives int
	delErr   error
}

func (f *fakeClient) ReceiveNotification(ctx context.Context, _ time.Duration) (*greenapi.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.receives++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, err
	}
	if len(f.queue) == 0 {
		return nil, nil
	}
	next := f.queue[0]
	f.queue = f.queue[1:]
	return next, nil
}

func (f *fakeClient) DeleteNotification(ctx context.Context, receiptID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, receiptID)
	return nil
}

func (f *fakeClient) deletedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.deleted...)
}

// fakeIngester records bodies and can be told to fail.
type fakeIngester struct {
	mu       sync.Mutex
	bodies   [][]byte
	err      error
	accepted bool
}

func (f *fakeIngester) Ingest(ctx context.Context, body []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.bodies = append(f.bodies, body)
	return f.accepted, f.err
}

func (f *fakeIngester) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func testConfig() config.GreenAPI {
	return config.GreenAPI{
		InstanceID:     "1",
		Token:          "t",
		ReceiveTimeout: 5 * time.Second,
		PollInterval:   time.Millisecond,
	}
}

func notification(id int64, text string) *greenapi.Notification {
	body, _ := json.Marshal(map[string]any{
		"typeWebhook": "incomingMessageReceived",
		"idMessage":   text,
		"senderData":  map[string]string{"chatId": "77011234567@c.us"},
	})
	return &greenapi.Notification{ReceiptID: id, Body: body}
}

// runUntil drives the receiver until cond holds or the deadline passes.
func runUntil(t *testing.T, r *Receiver, cond func() bool) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("condition not met before the deadline")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not stop on context cancellation")
	}
}

// The happy path: a notification is stored, then acknowledged.
func TestReceiverDeletesAfterSuccessfulIngest(t *testing.T) {
	client := &fakeClient{queue: []*greenapi.Notification{notification(11, "A"), notification(12, "B")}}
	processor := &fakeIngester{accepted: true}

	receiver := NewReceiver(client, processor, testConfig(), discardLogger())
	runUntil(t, receiver, func() bool { return len(client.deletedIDs()) == 2 })

	if got := client.deletedIDs(); got[0] != 11 || got[1] != 12 {
		t.Errorf("deleted receipts: got %v, want [11 12]", got)
	}
	if processor.count() != 2 {
		t.Errorf("ingested %d notifications, want 2", processor.count())
	}
}

// The reliability rule: if the event could not be stored, it must stay in the
// provider's queue so the next poll sees it again. Deleting here would lose a
// customer's trigger permanently.
func TestReceiverKeepsNotificationWhenIngestFails(t *testing.T) {
	client := &fakeClient{queue: []*greenapi.Notification{notification(21, "A")}}
	processor := &fakeIngester{err: errors.New("database is locked")}

	receiver := NewReceiver(client, processor, testConfig(), discardLogger())
	runUntil(t, receiver, func() bool { return processor.count() >= 1 })

	if deleted := client.deletedIDs(); len(deleted) != 0 {
		t.Errorf("a failed ingest must not be acknowledged, but deleted %v", deleted)
	}
}

// A duplicate is already stored from an earlier delivery, so acknowledging it
// is correct — otherwise the same entry would be redelivered forever.
func TestReceiverDeletesDuplicates(t *testing.T) {
	client := &fakeClient{queue: []*greenapi.Notification{notification(31, "A")}}
	processor := &fakeIngester{accepted: false} // already seen

	receiver := NewReceiver(client, processor, testConfig(), discardLogger())
	runUntil(t, receiver, func() bool { return len(client.deletedIDs()) == 1 })
}

// A payload that cannot be parsed will never parse. Leaving it queued would
// block every message behind it, so it is dropped deliberately.
func TestReceiverDropsUnparseableNotification(t *testing.T) {
	client := &fakeClient{queue: []*greenapi.Notification{{ReceiptID: 41, Body: []byte(`{`)}}}
	processor := &fakeIngester{err: &greenapi.ParseError{Err: errors.New("bad json")}}

	receiver := NewReceiver(client, processor, testConfig(), discardLogger())
	runUntil(t, receiver, func() bool { return len(client.deletedIDs()) == 1 })

	if got := client.deletedIDs(); got[0] != 41 {
		t.Errorf("deleted %v, want [41]", got)
	}
}

// A provider outage must not crash or spin; polling continues once it recovers.
func TestReceiverRecoversFromProviderErrors(t *testing.T) {
	client := &fakeClient{
		errs:  []error{errors.New("connection refused")},
		queue: []*greenapi.Notification{notification(51, "A")},
	}
	processor := &fakeIngester{accepted: true}

	cfg := testConfig()
	receiver := NewReceiver(client, processor, cfg, discardLogger())
	receiver.backoffFloor = time.Millisecond // keep the test quick

	runUntil(t, receiver, func() bool { return len(client.deletedIDs()) == 1 })
}

// An idle queue is normal: no work, no error, no busy loop.
func TestReceiverHandlesEmptyQueue(t *testing.T) {
	client := &fakeClient{}
	processor := &fakeIngester{accepted: true}

	receiver := NewReceiver(client, processor, testConfig(), discardLogger())
	runUntil(t, receiver, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.receives >= 3
	})

	if processor.count() != 0 {
		t.Errorf("an empty queue must not produce work, got %d", processor.count())
	}
}

func TestReceiverStopsOnContextCancellation(t *testing.T) {
	client := &fakeClient{}
	receiver := NewReceiver(client, &fakeIngester{}, testConfig(), discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		receiver.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not honour context cancellation")
	}
}
