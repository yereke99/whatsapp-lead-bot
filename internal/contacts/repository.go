// Package contacts owns the contact record: creation from inbound messages,
// consent state, activity counters and the queries behind the contact list and
// the chat console.
package contacts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/pkg/phone"
)

const contactColumns = `
	c.id, c.phone, c.chat_id, c.name, c.push_name, c.source,
	c.first_trigger_keyword, c.first_campaign_id, c.status, c.opted_out,
	c.opted_out_at, c.blocked_at, c.first_contact_at, c.last_incoming_at,
	c.last_outgoing_at, c.last_activity_at, c.incoming_count, c.outgoing_count,
	c.notes, c.avatar_url, c.avatar_source_url, c.avatar_checked_at,
	c.unread_count, c.last_message_preview, c.last_message_type,
	c.last_message_direction, c.created_at, c.updated_at`

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

// UpsertFromInbound finds or creates the contact behind an incoming message.
//
// first_contact_at is the platform's consent anchor and is set exactly once,
// on the first inbound message. Nothing may be sent to a contact whose anchor
// is still NULL, which is what makes "never message first" structural rather
// than a convention.
// Whether the contact was created is answered by which of the two statements
// produced the row. Postgres could fold this into one upsert and read xmax to
// tell an insert from an update; SQLite exposes no such marker, and deriving it
// from the stored timestamps would misreport a second message that carries the
// same provider timestamp as the first.
func (r *Repository) UpsertFromInbound(ctx context.Context, q sqlite.Querier, chatID, phoneDigits, pushName string, at time.Time) (*domain.Contact, bool, error) {
	querier := r.querier(q)
	columns := strings.ReplaceAll(contactColumns, "c.", "")

	const insertQuery = `
		INSERT INTO contacts (phone, chat_id, push_name, first_contact_at, last_incoming_at, last_activity_at, status)
		VALUES ($1, $2, $3, $4, $4, $4, 'NEW')
		ON CONFLICT (phone) DO NOTHING
		RETURNING `

	var contact domain.Contact
	err := scanContactWith(
		querier.QueryRow(ctx, insertQuery+columns, phoneDigits, chatID, pushName, at), &contact)
	switch {
	case err == nil:
		contact.PhoneDisplay = phone.Display(contact.Phone)
		return &contact, true, nil
	case !sqlite.IsNoRows(err):
		return nil, false, fmt.Errorf("upsert contact: %w", err)
	}

	// The insert conflicted, so the contact already existed. Refresh only what
	// an inbound message is authoritative about, and leave the consent anchor
	// alone once it is set.
	const updateQuery = `
		UPDATE contacts SET
			chat_id          = $2,
			push_name        = CASE WHEN $3 <> '' THEN $3 ELSE push_name END,
			first_contact_at = COALESCE(first_contact_at, $4),
			updated_at       = now()
		WHERE phone = $1
		RETURNING `

	if err := scanContactWith(
		querier.QueryRow(ctx, updateQuery+columns, phoneDigits, chatID, pushName, at), &contact); err != nil {
		return nil, false, fmt.Errorf("upsert contact: %w", err)
	}

	contact.PhoneDisplay = phone.Display(contact.Phone)
	return &contact, false, nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Contact, error) {
	query := `SELECT ` + contactColumns + `, COALESCE(camp.name, '')
		FROM contacts c
		LEFT JOIN campaigns camp ON camp.id = c.first_campaign_id
		WHERE c.id = $1`

	row := r.db.QueryRow(ctx, query, id)
	var contact domain.Contact
	if err := scanContactWith(row, &contact, &contact.CampaignName); err != nil {
		if sqlite.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	contact.PhoneDisplay = phone.Display(contact.Phone)
	return &contact, nil
}

func (r *Repository) GetByPhone(ctx context.Context, q sqlite.Querier, phoneDigits string) (*domain.Contact, error) {
	query := `SELECT ` + contactColumns + ` FROM contacts c WHERE c.phone = $1`

	row := r.querier(q).QueryRow(ctx, query, phoneDigits)
	var contact domain.Contact
	if err := scanContactWith(row, &contact); err != nil {
		if sqlite.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	contact.PhoneDisplay = phone.Display(contact.Phone)
	return &contact, nil
}

// Filter describes the contact list query.
type Filter struct {
	Search      string
	Status      string
	CampaignID  *uuid.UUID
	Trigger     string
	OptedOut    *bool
	TagID       *uuid.UUID
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	Sort        string
	Limit       int
	Offset      int
}

// buildWhere renders the shared predicate for list, count and export.
// Every value goes through a placeholder; nothing is interpolated.
func (f Filter) buildWhere(args *[]any) string {
	var clauses []string

	add := func(clause string, value any) {
		*args = append(*args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(*args)))
	}

	if s := strings.TrimSpace(f.Search); s != "" {
		*args = append(*args, "%"+strings.ToLower(s)+"%")
		n := len(*args)
		clauses = append(clauses, fmt.Sprintf(
			"(lower(c.name) LIKE $%d OR lower(c.push_name) LIKE $%d OR c.phone LIKE $%d)", n, n, n))
	}
	if f.Status != "" {
		add("c.status = $%d", f.Status)
	}
	if f.CampaignID != nil {
		*args = append(*args, *f.CampaignID)
		n := len(*args)
		clauses = append(clauses, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM campaign_contacts cc WHERE cc.contact_id = c.id AND cc.campaign_id = $%d)", n))
	}
	if t := strings.TrimSpace(f.Trigger); t != "" {
		add("c.first_trigger_keyword = $%d", t)
	}
	if f.OptedOut != nil {
		add("c.opted_out = $%d", *f.OptedOut)
	}
	if f.TagID != nil {
		*args = append(*args, *f.TagID)
		n := len(*args)
		clauses = append(clauses, fmt.Sprintf(
			"EXISTS (SELECT 1 FROM contact_tags ct WHERE ct.contact_id = c.id AND ct.tag_id = $%d)", n))
	}
	if f.CreatedFrom != nil {
		add("c.created_at >= $%d", *f.CreatedFrom)
	}
	if f.CreatedTo != nil {
		add("c.created_at < $%d", *f.CreatedTo)
	}

	if len(clauses) == 0 {
		return "TRUE"
	}
	return strings.Join(clauses, " AND ")
}

// orderBy maps a client sort key onto a fixed SQL fragment. The mapping is a
// closed set, so the sort parameter can never reach the query as free text.
func orderBy(sort string) string {
	switch sort {
	case "created_asc":
		return "c.created_at ASC"
	case "activity_desc":
		return "c.last_activity_at DESC NULLS LAST"
	case "activity_asc":
		return "c.last_activity_at ASC NULLS LAST"
	case "name_asc":
		return "lower(c.name) ASC, c.phone ASC"
	default:
		return "c.created_at DESC"
	}
}

func (r *Repository) List(ctx context.Context, f Filter) ([]domain.Contact, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 25
	}

	var args []any
	where := f.buildWhere(&args)

	var total int
	countQuery := `SELECT count(*) FROM contacts c WHERE ` + where
	if err := r.db.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count contacts: %w", err)
	}

	args = append(args, f.Limit, f.Offset)
	query := `SELECT ` + contactColumns + `, COALESCE(camp.name, ''),
			COALESCE((SELECT count(*) FROM campaign_contacts cc
				WHERE cc.contact_id = c.id AND cc.status = 'ACTIVE'), 0) AS active_enrollments
		FROM contacts c
		LEFT JOIN campaigns camp ON camp.id = c.first_campaign_id
		WHERE ` + where + `
		ORDER BY ` + orderBy(f.Sort) + `
		LIMIT $` + fmt.Sprint(len(args)-1) + ` OFFSET $` + fmt.Sprint(len(args))

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list contacts: %w", err)
	}
	defer rows.Close()

	var out []domain.Contact
	for rows.Next() {
		var c domain.Contact
		var activeEnrollments int
		if err := scanContactWith(rows, &c, &c.CampaignName, &activeEnrollments); err != nil {
			return nil, 0, err
		}
		c.PhoneDisplay = phone.Display(c.Phone)
		if activeEnrollments > 0 && c.Status == domain.ContactNew {
			c.Status = domain.ContactActive
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// ChatListItem is one row of the inbox sidebar.
type ChatListItem struct {
	domain.Contact
	ActiveCampaign string `json:"active_campaign,omitempty"`
}

// ChatList returns conversations ordered by most recent activity.
func (r *Repository) ChatList(ctx context.Context, search string, unreadOnly bool, limit, offset int) ([]ChatListItem, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 40
	}

	args := []any{}
	clauses := []string{"c.first_contact_at IS NOT NULL"}

	if s := strings.TrimSpace(search); s != "" {
		args = append(args, "%"+strings.ToLower(s)+"%")
		n := len(args)
		clauses = append(clauses, fmt.Sprintf(
			"(lower(c.name) LIKE $%d OR lower(c.push_name) LIKE $%d OR c.phone LIKE $%d)", n, n, n))
	}
	if unreadOnly {
		clauses = append(clauses, "c.unread_count > 0")
	}
	where := strings.Join(clauses, " AND ")

	var total int
	if err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM contacts c WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count chats: %w", err)
	}

	args = append(args, limit, offset)
	query := `SELECT ` + contactColumns + `,
			COALESCE((SELECT camp.name FROM campaign_contacts cc
				JOIN campaigns camp ON camp.id = cc.campaign_id
				WHERE cc.contact_id = c.id AND cc.status = 'ACTIVE'
				ORDER BY cc.enrolled_at DESC LIMIT 1), '') AS active_campaign
		FROM contacts c
		WHERE ` + where + `
		ORDER BY c.last_activity_at DESC NULLS LAST, c.created_at DESC
		LIMIT $` + fmt.Sprint(len(args)-1) + ` OFFSET $` + fmt.Sprint(len(args))

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list chats: %w", err)
	}
	defer rows.Close()

	var out []ChatListItem
	for rows.Next() {
		var item ChatListItem
		if err := scanContactWith(rows, &item.Contact, &item.ActiveCampaign); err != nil {
			return nil, 0, err
		}
		item.PhoneDisplay = phone.Display(item.Phone)
		out = append(out, item)
	}
	return out, total, rows.Err()
}

// RecordIncoming updates activity counters and the denormalized chat preview.
func (r *Repository) RecordIncoming(ctx context.Context, q sqlite.Querier, contactID uuid.UUID, preview, msgType string, at time.Time) error {
	const query = `
		UPDATE contacts SET
			last_incoming_at       = max(COALESCE(last_incoming_at, $2), $2),
			last_activity_at       = max(COALESCE(last_activity_at, $2), $2),
			incoming_count         = incoming_count + 1,
			unread_count           = unread_count + 1,
			last_message_preview   = $3,
			last_message_type      = $4,
			last_message_direction = 'INCOMING',
			updated_at             = now()
		WHERE id = $1`

	_, err := r.querier(q).Exec(ctx, query, contactID, at, truncatePreview(preview), msgType)
	return err
}

// RecordOutgoing mirrors RecordIncoming for messages the platform sent.
func (r *Repository) RecordOutgoing(ctx context.Context, q sqlite.Querier, contactID uuid.UUID, preview, msgType string, at time.Time) error {
	const query = `
		UPDATE contacts SET
			last_outgoing_at       = max(COALESCE(last_outgoing_at, $2), $2),
			last_activity_at       = max(COALESCE(last_activity_at, $2), $2),
			outgoing_count         = outgoing_count + 1,
			last_message_preview   = $3,
			last_message_type      = $4,
			last_message_direction = 'OUTGOING',
			status                 = CASE WHEN status = 'NEW' THEN 'ACTIVE' ELSE status END,
			updated_at             = now()
		WHERE id = $1`

	_, err := r.querier(q).Exec(ctx, query, contactID, at, truncatePreview(preview), msgType)
	return err
}

func (r *Repository) MarkRead(ctx context.Context, contactID uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`UPDATE contacts SET unread_count = 0, updated_at = now() WHERE id = $1`, contactID)
	return err
}

func (r *Repository) UpdateStatus(ctx context.Context, q sqlite.Querier, contactID uuid.UUID, status domain.ContactStatus) error {
	_, err := r.querier(q).Exec(ctx,
		`UPDATE contacts SET status = $2, updated_at = now() WHERE id = $1`, contactID, status)
	return err
}

// SetOptOut records an unsubscribe. It is idempotent: repeating STOP does not
// move the timestamp.
func (r *Repository) SetOptOut(ctx context.Context, q sqlite.Querier, contactID uuid.UUID, optedOut bool) error {
	const query = `
		UPDATE contacts SET
			opted_out    = $2,
			opted_out_at = CASE WHEN $2 THEN COALESCE(opted_out_at, now()) ELSE NULL END,
			status       = CASE WHEN $2 THEN 'UNSUBSCRIBED'
			                    WHEN status = 'UNSUBSCRIBED' THEN 'ACTIVE'
			                    ELSE status END,
			updated_at   = now()
		WHERE id = $1`

	_, err := r.querier(q).Exec(ctx, query, contactID, optedOut)
	return err
}

func (r *Repository) SetBlocked(ctx context.Context, contactID uuid.UUID, blocked bool) error {
	const query = `
		UPDATE contacts SET
			blocked_at = CASE WHEN $2 THEN COALESCE(blocked_at, now()) ELSE NULL END,
			status     = CASE WHEN $2 THEN 'BLOCKED'
			                  WHEN status = 'BLOCKED' THEN 'ACTIVE'
			                  ELSE status END,
			updated_at = now()
		WHERE id = $1`

	_, err := r.db.Exec(ctx, query, contactID, blocked)
	return err
}

func (r *Repository) UpdateProfile(ctx context.Context, contactID uuid.UUID, name, notes string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE contacts SET name = $2, notes = $3, updated_at = now() WHERE id = $1`,
		contactID, name, notes)
	return err
}

func (r *Repository) UpdateAvatar(ctx context.Context, contactID uuid.UUID, localURL, sourceURL string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE contacts SET avatar_url = $2, avatar_source_url = $3, avatar_checked_at = now(), updated_at = now()
		 WHERE id = $1`, contactID, localURL, sourceURL)
	return err
}

// SetFirstCampaign records the campaign a contact originally entered through.
// It only fills an empty slot, so the attribution never changes later.
func (r *Repository) SetFirstCampaign(ctx context.Context, q sqlite.Querier, contactID, campaignID uuid.UUID, keyword string) error {
	const query = `
		UPDATE contacts SET
			first_campaign_id     = COALESCE(first_campaign_id, $2),
			first_trigger_keyword = CASE WHEN first_trigger_keyword = '' THEN $3 ELSE first_trigger_keyword END,
			status                = CASE WHEN status = 'NEW' THEN 'ACTIVE' ELSE status END,
			updated_at            = now()
		WHERE id = $1`

	_, err := r.querier(q).Exec(ctx, query, contactID, campaignID, keyword)
	return err
}

// StaleAvatars lists contacts whose avatar has never been fetched or is older
// than ttl, so a background job can refresh them.
func (r *Repository) StaleAvatars(ctx context.Context, ttl time.Duration, limit int) ([]domain.Contact, error) {
	const query = `
		SELECT c.id, c.chat_id, c.avatar_source_url
		FROM contacts c
		WHERE c.first_contact_at IS NOT NULL
		  AND (c.avatar_checked_at IS NULL OR c.avatar_checked_at < $1)
		ORDER BY c.last_activity_at DESC NULLS LAST
		LIMIT $2`

	rows, err := r.db.Query(ctx, query, time.Now().UTC().Add(-ttl), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Contact
	for rows.Next() {
		var c domain.Contact
		if err := rows.Scan(&c.ID, &c.ChatID, &c.AvatarSourceURL); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// BulkSetStatus applies a status change to many contacts at once.
func (r *Repository) BulkSetStatus(ctx context.Context, ids []uuid.UUID, status domain.ContactStatus) (int64, error) {
	tag, err := r.db.Exec(ctx,
		`UPDATE contacts SET status = $2, updated_at = now() WHERE id IN (SELECT value FROM json_each($1))`, ids, status)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) BulkSetOptOut(ctx context.Context, ids []uuid.UUID, optedOut bool) (int64, error) {
	const query = `
		UPDATE contacts SET
			opted_out    = $2,
			opted_out_at = CASE WHEN $2 THEN COALESCE(opted_out_at, now()) ELSE NULL END,
			status       = CASE WHEN $2 THEN 'UNSUBSCRIBED'
			                    WHEN status = 'UNSUBSCRIBED' THEN 'ACTIVE'
			                    ELSE status END,
			updated_at   = now()
		WHERE id IN (SELECT value FROM json_each($1))`

	tag, err := r.db.Exec(ctx, query, ids, optedOut)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) BulkSetBlocked(ctx context.Context, ids []uuid.UUID, blocked bool) (int64, error) {
	const query = `
		UPDATE contacts SET
			blocked_at = CASE WHEN $2 THEN COALESCE(blocked_at, now()) ELSE NULL END,
			status     = CASE WHEN $2 THEN 'BLOCKED'
			                  WHEN status = 'BLOCKED' THEN 'ACTIVE'
			                  ELSE status END,
			updated_at = now()
		WHERE id IN (SELECT value FROM json_each($1))`

	tag, err := r.db.Exec(ctx, query, ids, blocked)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Exec(ctx, `DELETE FROM contacts WHERE id = $1`, id)
	return err
}

// ---------------------------------------------------------------- tagging --

func (r *Repository) ListTags(ctx context.Context) ([]domain.Tag, error) {
	rows, err := r.db.Query(ctx, `SELECT id, name, color FROM tags ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Tag
	for rows.Next() {
		var t domain.Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Color); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// EnsureTag returns the existing tag with this name or creates it.
func (r *Repository) EnsureTag(ctx context.Context, name, color string) (*domain.Tag, error) {
	const query = `
		INSERT INTO tags (name, color) VALUES ($1, $2)
		ON CONFLICT (lower(trim(name))) DO UPDATE SET name = tags.name
		RETURNING id, name, color`

	var t domain.Tag
	if err := r.db.QueryRow(ctx, query, strings.TrimSpace(name), color).
		Scan(&t.ID, &t.Name, &t.Color); err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *Repository) AttachTag(ctx context.Context, contactIDs []uuid.UUID, tagID uuid.UUID) (int64, error) {
	const query = `
		INSERT INTO contact_tags (contact_id, tag_id)
		SELECT id, $2 FROM contacts WHERE id IN (SELECT value FROM json_each($1))
		ON CONFLICT DO NOTHING`

	tag, err := r.db.Exec(ctx, query, contactIDs, tagID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) TagsFor(ctx context.Context, contactID uuid.UUID) ([]domain.Tag, error) {
	const query = `
		SELECT t.id, t.name, t.color
		FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id
		WHERE ct.contact_id = $1
		ORDER BY lower(t.name)`

	rows, err := r.db.Query(ctx, query, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Tag
	for rows.Next() {
		var t domain.Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Color); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- scanning --

type scanner interface {
	Scan(dest ...any) error
}

// scanContactWith reads the standard contact projection plus any extra
// trailing columns the caller selected.
func scanContactWith(row scanner, c *domain.Contact, extra ...any) error {
	dest := []any{
		&c.ID, &c.Phone, &c.ChatID, &c.Name, &c.PushName, &c.Source,
		&c.FirstTriggerKeyword, &c.FirstCampaignID, &c.Status, &c.OptedOut,
		&c.OptedOutAt, &c.BlockedAt, &c.FirstContactAt, &c.LastIncomingAt,
		&c.LastOutgoingAt, &c.LastActivityAt, &c.IncomingCount, &c.OutgoingCount,
		&c.Notes, &c.AvatarURL, &c.AvatarSourceURL, &c.AvatarCheckedAt,
		&c.UnreadCount, &c.LastMessagePreview, &c.LastMessageType,
		&c.LastMessageDir, &c.CreatedAt, &c.UpdatedAt,
	}
	dest = append(dest, extra...)
	return row.Scan(dest...)
}

func truncatePreview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 160
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// ExportRows streams the filtered contact set for CSV export.
func (r *Repository) ExportRows(ctx context.Context, f Filter) (*sqlite.Rows, error) {
	var args []any
	where := f.buildWhere(&args)

	query := `SELECT c.phone, c.name, c.push_name, COALESCE(camp.name, ''),
			c.first_trigger_keyword, c.status, c.opted_out, c.first_contact_at,
			c.last_activity_at, c.incoming_count, c.outgoing_count, c.created_at
		FROM contacts c
		LEFT JOIN campaigns camp ON camp.id = c.first_campaign_id
		WHERE ` + where + `
		ORDER BY ` + orderBy(f.Sort)

	return r.db.Query(ctx, query, args...)
}
