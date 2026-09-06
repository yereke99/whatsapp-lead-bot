package contacts

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
)

// Per-account unsubscribe state.
//
// A contact is one person, and stays one row: the same phone number writing to
// two of the installation's WhatsApp numbers is the same human, and splitting
// them would fragment their history for no benefit. What is *not* shared is
// the instruction they gave. "STOP" sent to the second number is a statement
// about the second number's campaigns; the reminders they are waiting for from
// the first number must keep arriving.
//
// The stored shape is deliberately minimal: a row in contact_instance_optouts
// means opted out on that account, and no row means not. There is no boolean
// to get out of step with itself, and resubscribing is a delete.
//
// contacts.opted_out, contacts.opted_out_at and contacts.status keep their
// existing meaning -- "this person asked us to stop somewhere" -- and keep
// being written exactly as before, so the panel, its filters, the exports and
// the analytics see no change at all. This table is what the send path asks.

// IsOptedOutOn reports whether the contact has unsubscribed from this account.
func (r *Repository) IsOptedOutOn(ctx context.Context, q sqlite.Querier, instanceID int64, contactID uuid.UUID) (bool, error) {
	const query = `
		SELECT count(*) FROM contact_instance_optouts
		WHERE whatsapp_instance_id = $1 AND contact_id = $2`

	var n int
	if err := r.querier(q).QueryRow(ctx, query, instanceID, contactID).Scan(&n); err != nil {
		return false, fmt.Errorf("read per-account opt-out: %w", err)
	}
	return n > 0, nil
}

// OptOutOn records an unsubscribe for one account only.
//
// Idempotent: repeating STOP does not move the timestamp, matching how
// SetOptOut has always behaved for the whole-person flag.
func (r *Repository) OptOutOn(ctx context.Context, q sqlite.Querier, instanceID int64, contactID uuid.UUID, reason string) error {
	const query = `
		INSERT INTO contact_instance_optouts (whatsapp_instance_id, contact_id, reason)
		VALUES ($1, $2, $3)
		ON CONFLICT (whatsapp_instance_id, contact_id) DO NOTHING`

	if _, err := r.querier(q).Exec(ctx, query, instanceID, contactID, reason); err != nil {
		return fmt.Errorf("record per-account opt-out: %w", err)
	}
	return nil
}

// OptOutEverywhere records an unsubscribe on every account, and is what an
// administrator's action in the panel means.
//
// An operator unsubscribing somebody is making a statement about the person,
// not about one of the installation's numbers, so it must reach all of them.
// It is also what keeps the whole-person flag and the per-account rows in
// agreement for any installation that only ever runs one number.
func (r *Repository) OptOutEverywhere(ctx context.Context, q sqlite.Querier, contactID uuid.UUID, reason string) error {
	const query = `
		INSERT INTO contact_instance_optouts (whatsapp_instance_id, contact_id, reason)
		SELECT i.id, $1, $2 FROM whatsapp_instances i
		WHERE NOT EXISTS (
			SELECT 1 FROM contact_instance_optouts o
			WHERE o.whatsapp_instance_id = i.id AND o.contact_id = $1
		)`

	if _, err := r.querier(q).Exec(ctx, query, contactID, reason); err != nil {
		return fmt.Errorf("record opt-out on every account: %w", err)
	}
	return nil
}

// ResubscribeEverywhere clears the unsubscribe on every account. It is the
// inverse of OptOutEverywhere and is likewise an administrator action.
func (r *Repository) ResubscribeEverywhere(ctx context.Context, q sqlite.Querier, contactID uuid.UUID) error {
	if _, err := r.querier(q).Exec(ctx,
		`DELETE FROM contact_instance_optouts WHERE contact_id = $1`, contactID); err != nil {
		return fmt.Errorf("clear opt-out on every account: %w", err)
	}
	return nil
}

// BulkSetOptOutEverywhere applies the panel's bulk unsubscribe or resubscribe
// to the per-account rows, alongside the whole-person flag BulkSetOptOut
// writes.
func (r *Repository) BulkSetOptOutEverywhere(ctx context.Context, ids []uuid.UUID, optedOut bool) error {
	if len(ids) == 0 {
		return nil
	}

	if !optedOut {
		_, err := r.db.Exec(ctx,
			`DELETE FROM contact_instance_optouts WHERE contact_id IN (SELECT value FROM json_each($1))`, ids)
		if err != nil {
			return fmt.Errorf("bulk clear opt-out: %w", err)
		}
		return nil
	}

	const query = `
		INSERT INTO contact_instance_optouts (whatsapp_instance_id, contact_id, reason)
		SELECT i.id, target.value, 'unsubscribed by an administrator'
		FROM whatsapp_instances i, json_each($1) AS target
		WHERE NOT EXISTS (
			SELECT 1 FROM contact_instance_optouts o
			WHERE o.whatsapp_instance_id = i.id AND o.contact_id = target.value
		)`

	if _, err := r.db.Exec(ctx, query, ids); err != nil {
		return fmt.Errorf("bulk record opt-out: %w", err)
	}
	return nil
}

// OptedOutInstanceIDs lists the accounts a contact has unsubscribed from, for
// the contact detail page.
func (r *Repository) OptedOutInstanceIDs(ctx context.Context, contactID uuid.UUID) ([]int64, error) {
	rows, err := r.db.Query(ctx,
		`SELECT whatsapp_instance_id FROM contact_instance_optouts WHERE contact_id = $1 ORDER BY whatsapp_instance_id`,
		contactID)
	if err != nil {
		return nil, fmt.Errorf("list per-account opt-outs: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
