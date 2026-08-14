// Package audit records who changed what, when, and from where.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/storage/postgres"
)

// Action names are stable identifiers; the UI maps them to Kazakh labels.
const (
	ActionLogin           = "auth.login"
	ActionLogout          = "auth.logout"
	ActionLoginFailed     = "auth.login_failed"
	ActionPasswordChanged = "auth.password_changed"

	ActionAdminCreated = "admin.created"
	ActionAdminUpdated = "admin.updated"
	ActionAdminDeleted = "admin.deleted"

	ActionCampaignCreated    = "campaign.created"
	ActionCampaignUpdated    = "campaign.updated"
	ActionCampaignActivated  = "campaign.activated"
	ActionCampaignPaused     = "campaign.paused"
	ActionCampaignArchived   = "campaign.archived"
	ActionCampaignDeleted    = "campaign.deleted"
	ActionCampaignDuplicated = "campaign.duplicated"
	ActionEventTimeChanged   = "campaign.event_time_changed"

	ActionStepCreated   = "step.created"
	ActionStepUpdated   = "step.updated"
	ActionStepDeleted   = "step.deleted"
	ActionStepReordered = "step.reordered"

	ActionTriggerCreated = "trigger.created"
	ActionTriggerUpdated = "trigger.updated"
	ActionTriggerDeleted = "trigger.deleted"

	ActionTemplateCreated    = "template.created"
	ActionTemplateUpdated    = "template.updated"
	ActionTemplateDeleted    = "template.deleted"
	ActionTemplateDuplicated = "template.duplicated"

	ActionContactUpdated      = "contact.updated"
	ActionContactBlocked      = "contact.blocked"
	ActionContactUnblocked    = "contact.unblocked"
	ActionContactUnsubscribed = "contact.unsubscribed"
	ActionContactResubscribed = "contact.resubscribed"
	ActionContactDeleted      = "contact.deleted"

	ActionManualMessage = "message.manual_sent"
	ActionBulkAction    = "contacts.bulk_action"
	ActionExport        = "contacts.exported"
	ActionMediaUploaded = "media.uploaded"
	ActionMediaDeleted  = "media.deleted"
	ActionJobRequeued   = "job.requeued"
	ActionJobCancelled  = "job.cancelled"
)

// Actor identifies who performed an action.
type Actor struct {
	ID        *uuid.UUID
	Email     string
	IPAddress string
	UserAgent string
}

// Entry is one audit record to write.
type Entry struct {
	Action     string
	EntityType string
	EntityID   string
	Summary    string
	Old        any
	New        any
}

type Logger struct {
	db  *postgres.DB
	log *slog.Logger
}

func NewLogger(db *postgres.DB, log *slog.Logger) *Logger {
	return &Logger{db: db, log: log.With(slog.String("component", "audit"))}
}

// Record writes an audit entry.
//
// Failures are logged but never propagated: an audit write must not turn a
// successful administrative action into an error response. The structured log
// line is the backstop if the database write is lost.
func (l *Logger) Record(ctx context.Context, actor Actor, entry Entry) {
	oldJSON := marshalOrNil(entry.Old)
	newJSON := marshalOrNil(entry.New)

	const query = `
		INSERT INTO audit_logs (
			admin_id, admin_email, action, entity_type, entity_id,
			summary, old_values, new_values, ip_address, user_agent
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

	_, err := l.db.Pool.Exec(ctx, query,
		actor.ID, actor.Email, entry.Action, entry.EntityType, entry.EntityID,
		truncate(entry.Summary, 1000), oldJSON, newJSON,
		actor.IPAddress, truncate(actor.UserAgent, 500))

	if err != nil {
		l.log.Error("writing audit entry failed",
			slog.String("action", entry.Action),
			slog.String("entity", entry.EntityType+":"+entry.EntityID),
			slog.String("error", err.Error()))
		return
	}

	l.log.Info("admin action",
		slog.String("action", entry.Action),
		slog.String("admin", actor.Email),
		slog.String("entity_type", entry.EntityType),
		slog.String("entity_id", entry.EntityID),
		slog.String("summary", entry.Summary))
}

// ListFilter drives the audit log page.
type ListFilter struct {
	Action     string
	EntityType string
	EntityID   string
	AdminID    *uuid.UUID
	Search     string
	Limit      int
	Offset     int
}

func (l *Logger) List(ctx context.Context, f ListFilter) ([]domain.AuditLog, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}

	var args []any
	var clauses []string

	if f.Action != "" {
		args = append(args, f.Action)
		clauses = append(clauses, fmt.Sprintf("action = $%d", len(args)))
	}
	if f.EntityType != "" {
		args = append(args, f.EntityType)
		clauses = append(clauses, fmt.Sprintf("entity_type = $%d", len(args)))
	}
	if f.EntityID != "" {
		args = append(args, f.EntityID)
		clauses = append(clauses, fmt.Sprintf("entity_id = $%d", len(args)))
	}
	if f.AdminID != nil {
		args = append(args, *f.AdminID)
		clauses = append(clauses, fmt.Sprintf("admin_id = $%d", len(args)))
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		args = append(args, "%"+strings.ToLower(s)+"%")
		n := len(args)
		clauses = append(clauses, fmt.Sprintf("(lower(summary) LIKE $%d OR lower(admin_email) LIKE $%d)", n, n))
	}

	where := "TRUE"
	if len(clauses) > 0 {
		where = strings.Join(clauses, " AND ")
	}

	var total int
	if err := l.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	args = append(args, f.Limit, f.Offset)
	query := `
		SELECT id, admin_id, admin_email, action, entity_type, entity_id, summary,
		       old_values, new_values, ip_address, user_agent, created_at
		FROM audit_logs
		WHERE ` + where + `
		ORDER BY created_at DESC
		LIMIT $` + fmt.Sprint(len(args)-1) + ` OFFSET $` + fmt.Sprint(len(args))

	rows, err := l.db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []domain.AuditLog
	for rows.Next() {
		var e domain.AuditLog
		if err := rows.Scan(&e.ID, &e.AdminID, &e.AdminEmail, &e.Action, &e.EntityType,
			&e.EntityID, &e.Summary, &e.OldValues, &e.NewValues, &e.IPAddress,
			&e.UserAgent, &e.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

func marshalOrNil(v any) []byte {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
