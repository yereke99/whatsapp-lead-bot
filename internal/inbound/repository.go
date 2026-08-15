// Package inbound ingests provider notifications, guarantees each one is
// processed exactly once, and drives the inbound side of the automation:
// contact creation, conversation history, trigger detection and unsubscribes.
package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
)

type Repository struct {
	db *sqlite.DB
}

func NewRepository(db *sqlite.DB) *Repository { return &Repository{db: db} }

// StoredEvent is a queued notification awaiting processing.
type StoredEvent struct {
	ID        uuid.UUID
	Provider  string
	EventType string
	DedupeKey string
	Payload   json.RawMessage
	Attempts  int
}

// Insert records an event, returning false when it is a replay.
//
// The unique index on (provider, dedupe_key) is what makes webhook processing
// idempotent: Green API retries deliveries, and a retried delivery must not
// produce a second contact, a second message or a second campaign enrollment.
func (r *Repository) Insert(ctx context.Context, provider, eventType, dedupeKey, externalID string, payload []byte, receivedAt time.Time) (uuid.UUID, bool, error) {
	const query = `
		INSERT INTO webhook_events (provider, event_type, dedupe_key, external_id, payload, received_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (provider, dedupe_key) DO NOTHING
		RETURNING id`

	var id uuid.UUID
	err := r.db.QueryRow(ctx, query,
		provider, eventType, dedupeKey, externalID, payload, receivedAt).Scan(&id)

	if err != nil {
		if sqlite.IsNoRows(err) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("store webhook event: %w", err)
	}
	return id, true, nil
}

// Claim moves up to limit queued events into PROCESSING.
func (r *Repository) Claim(ctx context.Context, limit int) ([]StoredEvent, error) {
	if limit <= 0 {
		limit = 25
	}

	// RETURNING names the updated table's own columns; SQLite rejects an alias
	// prefix there even though the UPDATE itself is aliased.
	const query = `
		UPDATE webhook_events AS we
		SET status = 'PROCESSING', attempts = we.attempts + 1, updated_at = now()
		FROM (
			SELECT id FROM webhook_events
			WHERE status = 'RECEIVED'
			ORDER BY received_at
			LIMIT $1
		) queued
		WHERE we.id = queued.id
		RETURNING id, provider, event_type, dedupe_key, payload, attempts`

	rows, err := r.db.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("claim webhook events: %w", err)
	}
	defer rows.Close()

	var out []StoredEvent
	for rows.Next() {
		var e StoredEvent
		if err := rows.Scan(&e.ID, &e.Provider, &e.EventType, &e.DedupeKey, &e.Payload, &e.Attempts); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *Repository) MarkProcessed(ctx context.Context, id uuid.UUID, status domain.WebhookEventStatus, errText string) error {
	const query = `
		UPDATE webhook_events
		SET status = $2, error = $3, processed_at = now(), updated_at = now()
		WHERE id = $1`

	_, err := r.db.Exec(ctx, query, id, status, truncate(errText, 2000))
	return err
}

// Requeue returns a failed event to the queue for another attempt.
func (r *Repository) Requeue(ctx context.Context, id uuid.UUID, errText string) error {
	const query = `
		UPDATE webhook_events
		SET status = 'RECEIVED', error = $2, updated_at = now()
		WHERE id = $1`

	_, err := r.db.Exec(ctx, query, id, truncate(errText, 2000))
	return err
}

// ReleaseStale re-queues events abandoned by a crashed worker.
func (r *Repository) ReleaseStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	const query = `
		UPDATE webhook_events
		SET status = 'RECEIVED', updated_at = now()
		WHERE status = 'PROCESSING' AND updated_at < $1`

	tag, err := r.db.Exec(ctx, query, time.Now().UTC().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// List powers the diagnostics page.
func (r *Repository) List(ctx context.Context, status string, limit, offset int) ([]domain.WebhookEvent, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var total int
	if err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM webhook_events WHERE ($1 = '' OR status = $1)`, status).Scan(&total); err != nil {
		return nil, 0, err
	}

	const query = `
		SELECT id, provider, event_type, dedupe_key, external_id, status, attempts,
		       error, received_at, processed_at
		FROM webhook_events
		WHERE ($1 = '' OR status = $1)
		ORDER BY received_at DESC
		LIMIT $2 OFFSET $3`

	rows, err := r.db.Query(ctx, query, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []domain.WebhookEvent
	for rows.Next() {
		var e domain.WebhookEvent
		if err := rows.Scan(&e.ID, &e.Provider, &e.EventType, &e.DedupeKey, &e.ExternalID,
			&e.Status, &e.Attempts, &e.Error, &e.ReceivedAt, &e.ProcessedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// Prune deletes processed events older than the retention window, keeping the
// table from growing without bound.
func (r *Repository) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	const query = `
		DELETE FROM webhook_events
		WHERE status IN ('PROCESSED', 'IGNORED') AND received_at < $1`

	tag, err := r.db.Exec(ctx, query, time.Now().UTC().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
