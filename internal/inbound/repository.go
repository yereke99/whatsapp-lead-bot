// Package inbound durably ingests and retries provider notifications and
// drives the inbound side of the automation:
// contact creation, conversation history, trigger detection and unsubscribes.
package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	ID uuid.UUID
	// WhatsAppInstanceID is the account whose queue this notification came out
	// of. It travels with the event all the way to trigger matching, so a
	// message received on one number can never be answered as though it had
	// arrived on the other -- including after a restart, when the event is
	// picked up from the database by a worker that never saw the poller.
	WhatsAppInstanceID int64
	InstanceKey        string
	Provider           string
	EventType          string
	DedupeKey          string
	Payload            json.RawMessage
	Attempts           int
	ExternalID         string
	ReceivedAt         time.Time
	TriggerResult      *string
	Status             domain.WebhookEventStatus
	Error              string
}

// Insert records an event, returning false when it is a replay.
//
// The unique index on (provider, dedupe_key) is what makes webhook processing
// idempotent: Green API retries deliveries, and a retried delivery must not
// produce a second contact, a second message or a second campaign enrollment.
//
// With more than one account that index also has to be per-account, because a
// Green API receipt id is only unique inside its own instance and because the
// fallback key for an event without a message id is a hash of the payload --
// and two accounts can genuinely produce byte-identical payloads, a state
// change being the obvious one. Rather than rebuild the table to widen the
// constraint, the caller namespaces dedupeKey with the account key before it
// gets here, which makes the existing index per-account without touching a
// single stored row. This column carries the same fact in queryable form.
func (r *Repository) Insert(ctx context.Context, instanceID int64, provider, eventType, dedupeKey, externalID string, payload []byte, receivedAt time.Time) (uuid.UUID, bool, error) {
	const query = `
		INSERT INTO webhook_events (provider, event_type, dedupe_key, external_id, payload, received_at, whatsapp_instance_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (provider, dedupe_key) DO NOTHING
		RETURNING id`

	var id uuid.UUID
	inserted := false
	err := r.db.InTx(ctx, func(tx sqlite.Querier) error {
		// Old deployments used unscoped keys. Reuse their record by the actual
		// provider identity, without modifying historical dedupe keys.
		if eventType == "incomingMessageReceived" && externalID != "" {
			var existing uuid.UUID
			err := tx.QueryRow(ctx, `SELECT id FROM webhook_events
			  WHERE provider = $1 AND COALESCE(whatsapp_instance_id, 1) = $2
			    AND external_id = $3 AND event_type = $4 ORDER BY created_at, id LIMIT 1`,
				provider, instanceID, externalID, eventType).Scan(&existing)
			if err == nil {
				id = existing
				return nil
			}
			if !sqlite.IsNoRows(err) {
				return err
			}
		}
		err := tx.QueryRow(ctx, query, provider, eventType, dedupeKey, externalID, payload, receivedAt, instanceID).Scan(&id)
		if sqlite.IsNoRows(err) {
			return nil
		}
		inserted = err == nil
		return err
	})
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("store webhook event: %w", err)
	}
	return id, inserted, nil
}

// Claim moves up to limit queued events into PROCESSING.
func (r *Repository) Claim(ctx context.Context, limit int) ([]StoredEvent, error) {
	return r.claim(ctx, limit, claimOldest)
}

type claimLane int

const (
	claimOldest claimLane = iota
	claimLive
	claimReady
)

func (r *Repository) claim(ctx context.Context, limit int, lane claimLane) ([]StoredEvent, error) {
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
			  AND (next_attempt_at IS NULL OR next_attempt_at <= now())
			  AND ($2 != 1 OR (attempts = 0 AND trigger_result IS NULL AND received_at >= $3))
			  AND ($2 != 2 OR error != $4)
			ORDER BY received_at, id
			LIMIT $1
		) queued
		WHERE we.id = queued.id
		RETURNING id, provider, event_type, dedupe_key, payload, attempts,
		          COALESCE(whatsapp_instance_id, 1), external_id, received_at, trigger_result, status, error`

	rows, err := r.db.Query(ctx, query, limit, lane, time.Now().UTC().Add(-time.Minute), errAwaitingReply.Error())
	if err != nil {
		return nil, fmt.Errorf("claim webhook events: %w", err)
	}
	defer rows.Close()

	var out []StoredEvent
	for rows.Next() {
		var e StoredEvent
		if err := rows.Scan(&e.ID, &e.Provider, &e.EventType, &e.DedupeKey, &e.Payload,
			&e.Attempts, &e.WhatsAppInstanceID, &e.ExternalID, &e.ReceivedAt, &e.TriggerResult, &e.Status, &e.Error); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	// SQLite's RETURNING order is unspecified, even with an ordered subquery.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ReceivedAt.Equal(out[j].ReceivedAt) {
			return out[i].ID.String() < out[j].ID.String()
		}
		return out[i].ReceivedAt.Before(out[j].ReceivedAt)
	})
	return out, rows.Err()
}

func (r *Repository) MarkProcessed(ctx context.Context, id uuid.UUID, status domain.WebhookEventStatus, errText string) error {
	const query = `
		UPDATE webhook_events
		SET status = $2, error = $3, processed_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'PROCESSING'`

	_, err := r.db.Exec(ctx, query, id, status, truncate(errText, 2000))
	return err
}

// Requeue returns a failed event to the queue for another attempt.
func (r *Repository) Requeue(ctx context.Context, id uuid.UUID, errText string) error {
	return r.Defer(ctx, id, time.Now().UTC().Add(10*time.Second), errText, false)
}

func (r *Repository) Defer(ctx context.Context, id uuid.UUID, until time.Time, errText string, waiting bool) error {
	const query = `
		UPDATE webhook_events
		SET status = 'RECEIVED', error = $2, next_attempt_at = $3,
		    attempts = max(0, attempts - $4), updated_at = now()
		WHERE id = $1 AND status = 'PROCESSING'`

	_, err := r.db.Exec(ctx, query, id, truncate(errText, 2000), until, waiting)
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
		WHERE status IN ('PROCESSED', 'IGNORED') AND received_at < $1
		  AND event_type != 'incomingMessageReceived'
		  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.incoming_event_id = webhook_events.id)`

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
