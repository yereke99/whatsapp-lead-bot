// Package instances owns the platform's WhatsApp accounts: the rows that
// record which account owns what, and the runtime registry that binds each of
// them to its own provider client.
//
// The split between the two halves is deliberate. The database knows about
// *ownership* -- which campaigns, triggers and messages belong to which
// account -- and nothing else; it never sees a credential. The environment
// knows about *credentials* and nothing else; it never decides who owns what.
// The registry is the one place the two meet, and it meets them once, at
// startup, so no request path ever has to ask which number it is talking to.
package instances

import (
	"context"
	"fmt"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
)

type Repository struct {
	db *sqlite.DB
}

func NewRepository(db *sqlite.DB) *Repository { return &Repository{db: db} }

func (r *Repository) querier(q sqlite.Querier) sqlite.Querier {
	if q != nil {
		return q
	}
	return r.db
}

const instanceColumns = `
	i.id, i.instance_key, i.name, i.phone_label, i.description, i.enabled,
	i.created_at, i.updated_at`

// List returns every account, Primary first, with the number of campaigns each
// one owns so the panel can warn before an account is switched off.
func (r *Repository) List(ctx context.Context, q sqlite.Querier) ([]domain.WhatsAppInstance, error) {
	const query = `
		SELECT ` + instanceColumns + `,
			COALESCE((
				SELECT count(*) FROM campaigns c
				WHERE COALESCE(c.whatsapp_instance_id, 1) = i.id
				  AND c.archived_at IS NULL
			), 0)
		FROM whatsapp_instances i
		ORDER BY i.id`

	rows, err := r.querier(q).Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list whatsapp instances: %w", err)
	}
	defer rows.Close()

	var out []domain.WhatsAppInstance
	for rows.Next() {
		var in domain.WhatsAppInstance
		if err := rows.Scan(&in.ID, &in.Key, &in.Name, &in.PhoneLabel, &in.Description,
			&in.Enabled, &in.CreatedAt, &in.UpdatedAt, &in.CampaignCount); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// GetByID loads one account.
func (r *Repository) GetByID(ctx context.Context, q sqlite.Querier, id int64) (*domain.WhatsAppInstance, error) {
	const query = `SELECT ` + instanceColumns + `, 0 FROM whatsapp_instances i WHERE i.id = $1`

	var in domain.WhatsAppInstance
	err := r.querier(q).QueryRow(ctx, query, id).Scan(&in.ID, &in.Key, &in.Name, &in.PhoneLabel,
		&in.Description, &in.Enabled, &in.CreatedAt, &in.UpdatedAt, &in.CampaignCount)
	if err != nil {
		if sqlite.IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get whatsapp instance: %w", err)
	}
	return &in, nil
}

// Exists reports whether an account id is one the database knows about. The
// API uses it to reject a campaign pointed at an account that does not exist,
// which is the one way a campaign could otherwise become unroutable.
func (r *Repository) Exists(ctx context.Context, q sqlite.Querier, id int64) (bool, error) {
	var n int
	err := r.querier(q).QueryRow(ctx,
		`SELECT count(*) FROM whatsapp_instances WHERE id = $1 AND enabled`, id).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check whatsapp instance: %w", err)
	}
	return n > 0, nil
}

// SyncLabels writes the display name and phone label configured in the
// environment onto the account rows.
//
// It touches nothing else -- not enabled, not ownership, not a single row
// belonging to a campaign -- and it never inserts: the rows are created by
// migration 0007, so a name arriving from the environment can only ever update
// a row that is already there. An account whose name is not configured keeps
// the name it has, so an operator who renamed it in the database is not
// overruled on every restart.
func (r *Repository) SyncLabels(ctx context.Context, id int64, name, phoneLabel string) error {
	const query = `
		UPDATE whatsapp_instances SET
			name        = CASE WHEN length(trim($2)) > 0 THEN $2 ELSE name END,
			phone_label = CASE WHEN length(trim($3)) > 0 THEN $3 ELSE phone_label END
		WHERE id = $1`

	_, err := r.db.Exec(ctx, query, id, name, phoneLabel)
	if err != nil {
		return fmt.Errorf("sync whatsapp instance labels: %w", err)
	}
	return nil
}
