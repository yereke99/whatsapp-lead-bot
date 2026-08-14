// Package domain holds the platform's entity types and the vocabularies they
// share. It has no dependencies on storage or transport, so every other
// package can reference these types without creating an import cycle.
package domain

import (
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------- accounts --

type AdminRole string

const (
	RoleOwner  AdminRole = "OWNER"
	RoleAdmin  AdminRole = "ADMIN"
	RoleViewer AdminRole = "VIEWER"
)

// CanWrite reports whether the role may mutate data. Viewers are read-only.
func (r AdminRole) CanWrite() bool { return r == RoleOwner || r == RoleAdmin }

// CanManageAdmins reports whether the role may create or remove operators.
func (r AdminRole) CanManageAdmins() bool { return r == RoleOwner }

type Admin struct {
	ID           uuid.UUID  `json:"id"`
	Email        string     `json:"email"`
	Name         string     `json:"name"`
	PasswordHash string     `json:"-"`
	Role         AdminRole  `json:"role"`
	IsActive     bool       `json:"is_active"`
	LastLoginAt  *time.Time `json:"last_login_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type Session struct {
	ID         uuid.UUID
	AdminID    uuid.UUID
	TokenHash  string
	CSRFToken  string
	IPAddress  string
	UserAgent  string
	ExpiresAt  time.Time
	LastSeenAt time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

// ---------------------------------------------------------------- contacts --

type ContactStatus string

const (
	ContactNew          ContactStatus = "NEW"
	ContactActive       ContactStatus = "ACTIVE"
	ContactCompleted    ContactStatus = "COMPLETED"
	ContactUnsubscribed ContactStatus = "UNSUBSCRIBED"
	ContactBlocked      ContactStatus = "BLOCKED"
	ContactError        ContactStatus = "ERROR"
)

func ValidContactStatus(s string) bool {
	switch ContactStatus(s) {
	case ContactNew, ContactActive, ContactCompleted, ContactUnsubscribed, ContactBlocked, ContactError:
		return true
	}
	return false
}

type Contact struct {
	ID                  uuid.UUID     `json:"id"`
	Phone               string        `json:"phone"`
	PhoneDisplay        string        `json:"phone_display"`
	ChatID              string        `json:"chat_id"`
	Name                string        `json:"name"`
	PushName            string        `json:"push_name"`
	Source              string        `json:"source"`
	FirstTriggerKeyword string        `json:"first_trigger_keyword"`
	FirstCampaignID     *uuid.UUID    `json:"first_campaign_id"`
	Status              ContactStatus `json:"status"`
	OptedOut            bool          `json:"opted_out"`
	OptedOutAt          *time.Time    `json:"opted_out_at"`
	BlockedAt           *time.Time    `json:"blocked_at"`
	FirstContactAt      *time.Time    `json:"first_contact_at"`
	LastIncomingAt      *time.Time    `json:"last_incoming_at"`
	LastOutgoingAt      *time.Time    `json:"last_outgoing_at"`
	LastActivityAt      *time.Time    `json:"last_activity_at"`
	IncomingCount       int           `json:"incoming_count"`
	OutgoingCount       int           `json:"outgoing_count"`
	Notes               string        `json:"notes"`
	AvatarURL           string        `json:"avatar_url"`
	AvatarSourceURL     string        `json:"-"`
	AvatarCheckedAt     *time.Time    `json:"-"`
	UnreadCount         int           `json:"unread_count"`
	LastMessagePreview  string        `json:"last_message_preview"`
	LastMessageType     string        `json:"last_message_type"`
	LastMessageDir      string        `json:"last_message_direction"`
	CreatedAt           time.Time     `json:"created_at"`
	UpdatedAt           time.Time     `json:"updated_at"`

	// Populated by list/detail queries, not stored on the row.
	CampaignName string `json:"campaign_name,omitempty"`
	Tags         []Tag  `json:"tags,omitempty"`
}

// DisplayName is what the admin UI shows for the contact.
func (c *Contact) DisplayName() string {
	if c.Name != "" {
		return c.Name
	}
	if c.PushName != "" {
		return c.PushName
	}
	return c.PhoneDisplay
}

// CanReceiveMessages reports whether the platform is permitted to send to this
// contact. Consent requires an inbound message: the platform never initiates.
func (c *Contact) CanReceiveMessages() bool {
	return c.FirstContactAt != nil &&
		!c.OptedOut &&
		c.BlockedAt == nil &&
		c.Status != ContactBlocked &&
		c.Status != ContactUnsubscribed
}

type Tag struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Color string    `json:"color"`
}

// --------------------------------------------------------------- campaigns --

type CampaignStatus string

const (
	CampaignDraft     CampaignStatus = "DRAFT"
	CampaignActive    CampaignStatus = "ACTIVE"
	CampaignPaused    CampaignStatus = "PAUSED"
	CampaignCompleted CampaignStatus = "COMPLETED"
	CampaignArchived  CampaignStatus = "ARCHIVED"
)

func ValidCampaignStatus(s string) bool {
	switch CampaignStatus(s) {
	case CampaignDraft, CampaignActive, CampaignPaused, CampaignCompleted, CampaignArchived:
		return true
	}
	return false
}

// ExistingContactBehavior decides what happens when an already-enrolled
// contact sends the trigger again.
type ExistingContactBehavior string

const (
	// BehaviorIgnore silently does nothing. Safe default: it cannot produce
	// duplicate messages.
	BehaviorIgnore ExistingContactBehavior = "IGNORE"
	// BehaviorRestart cancels pending jobs and re-enrolls from scratch.
	BehaviorRestart ExistingContactBehavior = "RESTART"
	// BehaviorContinue leaves the existing schedule untouched.
	BehaviorContinue ExistingContactBehavior = "CONTINUE"
	// BehaviorSpecialMessage sends one configured reply without rescheduling.
	BehaviorSpecialMessage ExistingContactBehavior = "SPECIAL_MESSAGE"
)

func ValidExistingBehavior(s string) bool {
	switch ExistingContactBehavior(s) {
	case BehaviorIgnore, BehaviorRestart, BehaviorContinue, BehaviorSpecialMessage:
		return true
	}
	return false
}

type Campaign struct {
	ID                      uuid.UUID               `json:"id"`
	Name                    string                  `json:"name"`
	Description             string                  `json:"description"`
	EventType               string                  `json:"event_type"`
	EventStartAt            *time.Time              `json:"event_start_at"`
	Timezone                string                  `json:"timezone"`
	WebinarLink             string                  `json:"webinar_link"`
	Status                  CampaignStatus          `json:"status"`
	ExistingContactBehavior ExistingContactBehavior `json:"existing_contact_behavior"`
	ExistingContactTemplate *uuid.UUID              `json:"existing_contact_template_id"`
	UnsubscribeKeywords     []string                `json:"unsubscribe_keywords"`
	CatchUpMissedSteps      bool                    `json:"catch_up_missed_steps"`
	MaxSendAttempts         int                     `json:"max_send_attempts"`
	ArchivedAt              *time.Time              `json:"archived_at"`
	CreatedBy               *uuid.UUID              `json:"created_by"`
	CreatedAt               time.Time               `json:"created_at"`
	UpdatedAt               time.Time               `json:"updated_at"`

	// Aggregates filled in by list queries.
	Triggers     []CampaignTrigger `json:"triggers,omitempty"`
	Steps        []CampaignStep    `json:"steps,omitempty"`
	ContactCount int               `json:"contact_count"`
	PendingJobs  int               `json:"pending_jobs"`
	SentCount    int               `json:"sent_count"`
}

// AcceptsEnrollments reports whether new contacts may join right now.
func (c *Campaign) AcceptsEnrollments() bool {
	return c.Status == CampaignActive && c.ArchivedAt == nil && c.EventStartAt != nil
}

type CampaignTrigger struct {
	ID          uuid.UUID `json:"id"`
	CampaignID  uuid.UUID `json:"campaign_id"`
	Keyword     string    `json:"keyword"`
	Normalized  string    `json:"normalized_keyword"`
	MatchMode   string    `json:"match_mode"`
	IsActive    bool      `json:"is_active"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	CampaignRef string    `json:"campaign_name,omitempty"`
}

type ScheduleKind string

const (
	ScheduleRelativeToEvent ScheduleKind = "RELATIVE_TO_EVENT"
	ScheduleOnTrigger       ScheduleKind = "ON_TRIGGER"
)

type CampaignStep struct {
	ID              uuid.UUID    `json:"id"`
	CampaignID      uuid.UUID    `json:"campaign_id"`
	Name            string       `json:"name"`
	OffsetSeconds   int          `json:"offset_seconds"`
	TemplateID      uuid.UUID    `json:"message_template_id"`
	Enabled         bool         `json:"enabled"`
	OrderIndex      int          `json:"order_index"`
	ScheduleKind    ScheduleKind `json:"schedule_kind"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
	TemplateName    string       `json:"template_name,omitempty"`
	TemplateType    TemplateType `json:"template_type,omitempty"`
	TemplatePreview string       `json:"template_preview,omitempty"`
}

// --------------------------------------------------------------- templates --

type TemplateType string

const (
	TemplateText             TemplateType = "TEXT"
	TemplateImage            TemplateType = "IMAGE"
	TemplateImageWithCaption TemplateType = "IMAGE_WITH_CAPTION"
	TemplateVideo            TemplateType = "VIDEO"
	TemplateVideoWithCaption TemplateType = "VIDEO_WITH_CAPTION"
	TemplateAudio            TemplateType = "AUDIO"
	TemplateVoice            TemplateType = "VOICE"
	TemplateDocument         TemplateType = "DOCUMENT"
)

func ValidTemplateType(s string) bool {
	switch TemplateType(s) {
	case TemplateText, TemplateImage, TemplateImageWithCaption, TemplateVideo,
		TemplateVideoWithCaption, TemplateAudio, TemplateVoice, TemplateDocument:
		return true
	}
	return false
}

// RequiresMedia reports whether the type must reference a media file.
func (t TemplateType) RequiresMedia() bool { return t != TemplateText }

// AllowsCaption reports whether the type carries body text alongside media.
func (t TemplateType) AllowsCaption() bool {
	switch t {
	case TemplateText, TemplateImageWithCaption, TemplateVideoWithCaption, TemplateDocument:
		return true
	}
	return false
}

// MediaKind maps a template type onto the kind of file it needs.
func (t TemplateType) MediaKind() MediaKind {
	switch t {
	case TemplateImage, TemplateImageWithCaption:
		return MediaImage
	case TemplateVideo, TemplateVideoWithCaption:
		return MediaVideo
	case TemplateAudio:
		return MediaAudio
	case TemplateVoice:
		return MediaVoice
	case TemplateDocument:
		return MediaDocument
	default:
		return ""
	}
}

type MessageTemplate struct {
	ID          uuid.UUID    `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Type        TemplateType `json:"type"`
	Body        string       `json:"body"`
	MediaFileID *uuid.UUID   `json:"media_file_id"`
	FileName    string       `json:"file_name"`
	LinkPreview bool         `json:"link_preview"`
	Version     int          `json:"version"`
	ArchivedAt  *time.Time   `json:"archived_at"`
	CreatedBy   *uuid.UUID   `json:"created_by"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`

	Media  *MediaFile `json:"media,omitempty"`
	UsedBy int        `json:"used_by_steps"`
}

// ------------------------------------------------------------------- media --

type MediaKind string

const (
	MediaImage    MediaKind = "IMAGE"
	MediaVideo    MediaKind = "VIDEO"
	MediaAudio    MediaKind = "AUDIO"
	MediaVoice    MediaKind = "VOICE"
	MediaDocument MediaKind = "DOCUMENT"
)

func ValidMediaKind(s string) bool {
	switch MediaKind(s) {
	case MediaImage, MediaVideo, MediaAudio, MediaVoice, MediaDocument:
		return true
	}
	return false
}

type MediaFile struct {
	ID           uuid.UUID  `json:"id"`
	OriginalName string     `json:"original_name"`
	StoredName   string     `json:"stored_name"`
	RelativePath string     `json:"-"`
	MimeType     string     `json:"mime_type"`
	SizeBytes    int64      `json:"size_bytes"`
	Kind         MediaKind  `json:"kind"`
	Checksum     string     `json:"-"`
	DurationMS   *int       `json:"duration_ms"`
	Width        *int       `json:"width"`
	Height       *int       `json:"height"`
	SourceFileID *uuid.UUID `json:"source_media_file_id"`
	UploadedBy   *uuid.UUID `json:"uploaded_by"`
	CreatedAt    time.Time  `json:"created_at"`
	URL          string     `json:"url"`
}

// ---------------------------------------------------------------- messages --

type Direction string

const (
	DirectionIncoming Direction = "INCOMING"
	DirectionOutgoing Direction = "OUTGOING"
)

type MessageStatus string

const (
	MessageStatusPending   MessageStatus = "PENDING"
	MessageStatusSent      MessageStatus = "SENT"
	MessageStatusDelivered MessageStatus = "DELIVERED"
	MessageStatusRead      MessageStatus = "READ"
	MessageStatusFailed    MessageStatus = "FAILED"
	MessageStatusReceived  MessageStatus = "RECEIVED"
)

type Message struct {
	ID                 uuid.UUID     `json:"id"`
	ContactID          uuid.UUID     `json:"contact_id"`
	CampaignID         *uuid.UUID    `json:"campaign_id"`
	EnrollmentID       *uuid.UUID    `json:"enrollment_id"`
	CampaignStepID     *uuid.UUID    `json:"campaign_step_id"`
	ScheduledMessageID *uuid.UUID    `json:"scheduled_message_id"`
	Direction          Direction     `json:"direction"`
	Type               string        `json:"type"`
	Text               string        `json:"text"`
	MediaFileID        *uuid.UUID    `json:"media_file_id"`
	MediaURL           string        `json:"media_url"`
	FileName           string        `json:"file_name"`
	MimeType           string        `json:"mime_type"`
	ExternalID         *string       `json:"external_id"`
	Status             MessageStatus `json:"status"`
	Error              string        `json:"error"`
	IsManual           bool          `json:"is_manual"`
	SentByAdminID      *uuid.UUID    `json:"sent_by_admin_id"`
	TemplateID         *uuid.UUID    `json:"template_id"`
	TemplateVersion    *int          `json:"template_version"`
	MediaDownloadState string        `json:"media_download_status"`
	SentAt             *time.Time    `json:"sent_at"`
	DeliveredAt        *time.Time    `json:"delivered_at"`
	ReadAt             *time.Time    `json:"read_at"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`

	StepName    string `json:"step_name,omitempty"`
	AdminName   string `json:"admin_name,omitempty"`
	MediaAccess string `json:"media_access_url,omitempty"`
}

// ------------------------------------------------------------- enrollments --

type EnrollmentStatus string

const (
	EnrollmentActive       EnrollmentStatus = "ACTIVE"
	EnrollmentCompleted    EnrollmentStatus = "COMPLETED"
	EnrollmentCancelled    EnrollmentStatus = "CANCELLED"
	EnrollmentUnsubscribed EnrollmentStatus = "UNSUBSCRIBED"
)

type Enrollment struct {
	ID             uuid.UUID        `json:"id"`
	CampaignID     uuid.UUID        `json:"campaign_id"`
	ContactID      uuid.UUID        `json:"contact_id"`
	TriggerID      *uuid.UUID       `json:"trigger_id"`
	TriggerKeyword string           `json:"trigger_keyword"`
	Status         EnrollmentStatus `json:"status"`
	RunNumber      int              `json:"run_number"`
	RestartCount   int              `json:"restart_count"`
	EnrolledAt     time.Time        `json:"enrolled_at"`
	CompletedAt    *time.Time       `json:"completed_at"`
	CancelledAt    *time.Time       `json:"cancelled_at"`
	CancelReason   string           `json:"cancel_reason"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`

	CampaignName string `json:"campaign_name,omitempty"`
	PendingJobs  int    `json:"pending_jobs"`
	SentJobs     int    `json:"sent_jobs"`
}

// ------------------------------------------------------- scheduled messages --

type JobStatus string

const (
	JobPending    JobStatus = "PENDING"
	JobProcessing JobStatus = "PROCESSING"
	JobSent       JobStatus = "SENT"
	JobFailed     JobStatus = "FAILED"
	JobCancelled  JobStatus = "CANCELLED"
)

type ScheduledMessage struct {
	ID            uuid.UUID  `json:"id"`
	CampaignID    uuid.UUID  `json:"campaign_id"`
	ContactID     uuid.UUID  `json:"contact_id"`
	EnrollmentID  uuid.UUID  `json:"enrollment_id"`
	StepID        uuid.UUID  `json:"campaign_step_id"`
	RunNumber     int        `json:"run_number"`
	ScheduledAt   time.Time  `json:"scheduled_at"`
	Status        JobStatus  `json:"status"`
	AttemptCount  int        `json:"attempt_count"`
	NextAttemptAt *time.Time `json:"next_attempt_at"`
	LockedBy      *string    `json:"-"`
	LockedAt      *time.Time `json:"-"`
	SentAt        *time.Time `json:"sent_at"`
	CancelledAt   *time.Time `json:"cancelled_at"`
	CancelReason  string     `json:"cancel_reason"`
	LastError     string     `json:"last_error"`
	MessageID     *uuid.UUID `json:"message_id"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`

	CampaignName string `json:"campaign_name,omitempty"`
	StepName     string `json:"step_name,omitempty"`
	ContactName  string `json:"contact_name,omitempty"`
	ContactPhone string `json:"contact_phone,omitempty"`
}

// ------------------------------------------------------------------ system --

type WebhookEventStatus string

const (
	WebhookReceived   WebhookEventStatus = "RECEIVED"
	WebhookProcessing WebhookEventStatus = "PROCESSING"
	WebhookProcessed  WebhookEventStatus = "PROCESSED"
	WebhookFailed     WebhookEventStatus = "FAILED"
	WebhookIgnored    WebhookEventStatus = "IGNORED"
)

type WebhookEvent struct {
	ID          uuid.UUID          `json:"id"`
	Provider    string             `json:"provider"`
	EventType   string             `json:"event_type"`
	DedupeKey   string             `json:"dedupe_key"`
	ExternalID  string             `json:"external_id"`
	Payload     []byte             `json:"-"`
	Status      WebhookEventStatus `json:"status"`
	Attempts    int                `json:"attempts"`
	Error       string             `json:"error"`
	ReceivedAt  time.Time          `json:"received_at"`
	ProcessedAt *time.Time         `json:"processed_at"`
}

type AuditLog struct {
	ID         int64      `json:"id"`
	AdminID    *uuid.UUID `json:"admin_id"`
	AdminEmail string     `json:"admin_email"`
	Action     string     `json:"action"`
	EntityType string     `json:"entity_type"`
	EntityID   string     `json:"entity_id"`
	Summary    string     `json:"summary"`
	OldValues  []byte     `json:"old_values,omitempty"`
	NewValues  []byte     `json:"new_values,omitempty"`
	IPAddress  string     `json:"ip_address"`
	UserAgent  string     `json:"user_agent"`
	CreatedAt  time.Time  `json:"created_at"`
}
