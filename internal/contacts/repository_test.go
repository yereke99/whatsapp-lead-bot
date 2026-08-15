package contacts

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/migrations"
)

// newTestRepo builds a repository over a scratch database with the real schema.
func newTestRepo(t *testing.T) *Repository {
	t.Helper()

	db, err := sqlite.Connect(context.Background(), config.Database{
		Path:     filepath.Join(t.TempDir(), "contacts.db"),
		MaxConns: 4,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	if err := db.Migrate(context.Background(), migrations.FS, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewRepository(db)
}

// The activity counters are updated by SQL that never returns a row, so a
// broken function name there fails as a logged warning rather than a visible
// error. Exercising the statements is the only way to catch that.
func TestRecordActivityUpdatesCounters(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	first := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	contact, created, err := repo.UpsertFromInbound(ctx, nil, "77011234567@c.us", "77011234567", "Aigerim", first)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !created {
		t.Fatal("expected the contact to be created")
	}

	later := first.Add(time.Hour)
	if err := repo.RecordIncoming(ctx, nil, contact.ID, "Айран", "TEXT", later); err != nil {
		t.Fatalf("RecordIncoming: %v", err)
	}
	if err := repo.RecordOutgoing(ctx, nil, contact.ID, "Сәлеметсіз бе", "TEXT", later.Add(time.Minute)); err != nil {
		t.Fatalf("RecordOutgoing: %v", err)
	}

	got, err := repo.GetByID(ctx, contact.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if got.IncomingCount != 1 || got.OutgoingCount != 1 {
		t.Errorf("counters: incoming=%d outgoing=%d, want 1 and 1", got.IncomingCount, got.OutgoingCount)
	}
	if got.LastIncomingAt == nil || !got.LastIncomingAt.Equal(later) {
		t.Errorf("last_incoming_at: got %v, want %s", got.LastIncomingAt, later)
	}
	if got.LastActivityAt == nil || !got.LastActivityAt.Equal(later.Add(time.Minute)) {
		t.Errorf("last_activity_at: got %v, want %s", got.LastActivityAt, later.Add(time.Minute))
	}
	if got.LastMessagePreview != "Сәлеметсіз бе" {
		t.Errorf("preview: got %q", got.LastMessagePreview)
	}
	// An outgoing message promotes a NEW contact to ACTIVE.
	if string(got.Status) != "ACTIVE" {
		t.Errorf("status: got %q, want ACTIVE", got.Status)
	}
}

// Activity timestamps must never move backwards: provider notifications can
// arrive out of order, and an older one must not rewrite a newer high-water
// mark. That is what the max() around the stored value is for.
func TestRecordActivityNeverMovesTimestampsBackwards(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	contact, _, err := repo.UpsertFromInbound(ctx, nil, "77019999999@c.us", "77019999999", "", base)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	newest := base.Add(2 * time.Hour)
	if err := repo.RecordIncoming(ctx, nil, contact.ID, "second", "TEXT", newest); err != nil {
		t.Fatalf("RecordIncoming (newest): %v", err)
	}
	// A straggler that the provider delivered late.
	if err := repo.RecordIncoming(ctx, nil, contact.ID, "first", "TEXT", base.Add(time.Hour)); err != nil {
		t.Fatalf("RecordIncoming (late): %v", err)
	}

	got, err := repo.GetByID(ctx, contact.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if got.LastIncomingAt == nil || !got.LastIncomingAt.Equal(newest) {
		t.Errorf("a late notification moved last_incoming_at backwards: got %v, want %s",
			got.LastIncomingAt, newest)
	}
	if got.LastActivityAt == nil || !got.LastActivityAt.Equal(newest) {
		t.Errorf("a late notification moved last_activity_at backwards: got %v, want %s",
			got.LastActivityAt, newest)
	}
	// Both messages still count.
	if got.IncomingCount != 2 {
		t.Errorf("incoming_count: got %d, want 2", got.IncomingCount)
	}
}
