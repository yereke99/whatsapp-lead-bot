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

// ResumePolicy decides what happens to jobs whose time passed while the
// campaign was paused.
type ResumePolicy string

const (
	// ResumeSkipExpired cancels everything already overdue. Resuming at 20:00
	// after a pause at 18:00 does not dump both missed messages on the contact.
	ResumeSkipExpired ResumePolicy = "SKIP_EXPIRED"
	// ResumeSendNextValid keeps the most recent overdue step per contact and
	// sends it immediately, dropping the older ones.
	ResumeSendNextValid ResumePolicy = "SEND_NEXT_VALID"
)

func ValidResumePolicy(s string) bool {
	switch ResumePolicy(s) {
	case ResumeSkipExpired, ResumeSendNextValid:
		return true
	}
	return false
}

type Campaign struct {
	ID           uuid.UUID  `json:"id"`
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	EventType    string     `json:"event_type"`
	EventStartAt *time.Time `json:"event_start_at"`
	Timezone     string     `json:"timezone"`
	WebinarLink  string     `json:"webinar_link"`
	// IsDailyRecurring makes the webinar happen every day at RecurrenceTime in
	// Timezone, instead of once at EventStartAt. It changes one thing only:
	// which instant a contact's RELATIVE_TO_EVENT steps are measured from. Off
	// by default, and a campaign with it off behaves exactly as it always has.
	IsDailyRecurring bool `json:"is_daily_recurring"`
	// RecurrenceTime is the daily start as "HH:MM" wall-clock in Timezone.
	// Empty when recurrence is off.
	RecurrenceTime string `json:"recurrence_time"`
	// RecurrenceStartDate is the first calendar day of the series as
	// "YYYY-MM-DD" in Timezone. Empty means the series starts on EventStartAt's
	// own day, which is what the existing date picker already collects.
	RecurrenceStartDate     string                  `json:"recurrence_start_date"`
	Status                  CampaignStatus          `json:"status"`
	ExistingContactBehavior ExistingContactBehavior `json:"existing_contact_behavior"`
	ExistingContactTemplate *uuid.UUID              `json:"existing_contact_template_id"`
	UnsubscribeKeywords     []string                `json:"unsubscribe_keywords"`
	CatchUpMissedSteps      bool                    `json:"catch_up_missed_steps"`
	MaxSendAttempts         int                     `json:"max_send_attempts"`
	ResumePolicy            ResumePolicy            `json:"resume_policy"`
	// PinTemplateVersion freezes queued jobs to the template revision that was
	// current when they were queued. Off by default, which keeps the
	// long-standing behaviour of unsent messages picking up template edits.
	PinTemplateVersion bool       `json:"pin_template_version"`
	MaxMessagesPerHour *int       `json:"max_messages_per_hour"`
	MaxMessagesPerDay  *int       `json:"max_messages_per_day"`
	MaxActiveContacts  *int       `json:"max_active_contacts"`
	ArchivedAt         *time.Time `json:"archived_at"`
	CreatedBy          *uuid.UUID `json:"created_by"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`

	// Aggregates filled in by list queries.
	Triggers     []CampaignTrigger `json:"triggers,omitempty"`
	Steps        []CampaignStep    `json:"steps,omitempty"`
	ContactCount int               `json:"contact_count"`
	PendingJobs  int               `json:"pending_jobs"`
	SentCount    int               `json:"sent_count"`
	SentLastHour int               `json:"sent_last_hour"`
	SentLastDay  int               `json:"sent_last_day"`

	// NextOccurrenceAt is the upcoming webinar of a recurring series, derived
	// from the recurrence settings rather than stored. It is nil for a
	// one-time campaign, whose upcoming webinar is EventStartAt itself.
	NextOccurrenceAt *time.Time `json:"next_occurrence_at,omitempty"`
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

// ScheduleKind selects which anchor a step's offset is measured from.
type ScheduleKind string

const (
	// ScheduleRelativeToEvent anchors the step to campaign.event_start_at, so
	// every enrolled contact receives it at the same wall-clock moment. This is
	// how an exact campaign time such as "18:00 on 16.08.2026" is expressed:
	// the admin picks a date and time, and the panel stores the resulting
	// signed offset from the event. Moving the event then moves the whole
	// queue coherently.
	ScheduleRelativeToEvent ScheduleKind = "RELATIVE_TO_EVENT"
	// ScheduleOnTrigger anchors the step to the moment the contact entered the
	// campaign, so the offset is a per-contact delay: +2s, +10m, +2h. Each
	// contact therefore gets their own timetable.
	ScheduleOnTrigger ScheduleKind = "ON_TRIGGER"
)

func ValidScheduleKind(s string) bool {
	switch ScheduleKind(s) {
	case ScheduleRelativeToEvent, ScheduleOnTrigger:
		return true
	}
	return false
}

type CampaignStep struct {
	ID            uuid.UUID    `json:"id"`
	CampaignID    uuid.UUID    `json:"campaign_id"`
	Name          string       `json:"name"`
	OffsetSeconds int          `json:"offset_seconds"`
	TemplateID    uuid.UUID    `json:"message_template_id"`
	Enabled       bool         `json:"enabled"`
	OrderIndex    int          `json:"order_index"`
	ScheduleKind  ScheduleKind `json:"schedule_kind"`
	// AudienceFilterEnabled restricts this one step to contacts who entered the
	// campaign at or after AudienceMinJoinedAt. Off by default, and evaluated
	// per step: a campaign can send its 20:00 reminder to everyone and its
	// 20:55 welcome only to people who arrived in the last few minutes.
	AudienceFilterEnabled bool `json:"audience_filter_enabled"`
	// AudienceMinJoinedAt is the inclusive cutoff, in UTC. A contact who
	// enrolled exactly at this instant is eligible.
	AudienceMinJoinedAt *time.Time `json:"audience_min_joined_at"`
	// IncludeInDailyWebinar marks this step as part of the campaign's daily
	// webinar sequence.
	//
	// It answers a question only the operator can: a campaign holds the
	// webinar reminders *and* the greeting, the follow-ups and the
	// administrative notes, and "repeats every day" applies to the reminders
	// alone. Marked steps are delivered to a contact once — for the occurrence
	// their enrolment is pinned to — and are never re-armed by a later webinar,
	// a repeat trigger or a restart. Unmarked steps keep the behaviour they
	// already have, so the flag never disables anything.
	//
	// Off by default, which makes the daily sequence empty until an operator
	// fills it in and keeps every existing campaign behaving exactly as before.
	IncludeInDailyWebinar bool      `json:"include_in_daily_webinar"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`

	// Template details joined in by list queries, so the queue can be rendered
	// and validated without a second round trip per row.
	TemplateName     string       `json:"template_name,omitempty"`
	TemplateType     TemplateType `json:"template_type,omitempty"`
	TemplatePreview  string       `json:"template_preview,omitempty"`
	TemplateVersion  int          `json:"template_version,omitempty"`
	TemplateHasMedia bool         `json:"template_has_media"`
	TemplateArchived bool         `json:"template_archived"`
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
	// OccurrenceAt is the webinar occurrence this run belongs to, for a
	// campaign with a daily recurring webinar. It is the anchor every
	// RELATIVE_TO_EVENT step of this enrolment is measured from.
	//
	// nil means no occurrence is pinned — every enrolment of a one-time
	// campaign, and every row that predates the feature. Those are anchored to
	// the campaign's own event_start_at, exactly as before.
	OccurrenceAt *time.Time `json:"occurrence_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at"`
	CancelledAt  *time.Time `json:"cancelled_at"`
	CancelReason string     `json:"cancel_reason"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`

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

// IsTerminal reports whether a job has finished moving.
//
// This is the predicate an enrollment's completion is decided by: a step counts
// as resolved once its job can no longer change on its own. PENDING and
// PROCESSING are the only states the queue will still act on — a FAILED job has
// exhausted its attempts, and CANCELLED is a deliberate end.
func (s JobStatus) IsTerminal() bool {
	switch s {
	case JobSent, JobFailed, JobCancelled:
		return true
	}
	return false
}

// Skip reasons.
//
// A step that was never eligible for an enrollment is recorded as a CANCELLED
// job carrying one of these codes, rather than left absent. That distinction is
// the whole point: an absent row is indistinguishable from a lost one, which is
// exactly how missing steps used to hide. A row with a skip reason is a
// decision the system can show, count and audit.
//
// The codes are stored, so they are machine-readable and must stay stable. The
// panel renders them as "SKIPPED"; a CANCELLED job without one of these codes
// was revoked by an operator or by the send path.
const (
	// SkipStepExpired: the step's moment had already passed when the job would
	// have been created, and campaign policy does not catch up.
	SkipStepExpired = "skip:step_expired"
	// SkipStepDisabled: the step is switched off.
	SkipStepDisabled = "skip:step_disabled"
	// SkipNoEventAnchor: a RELATIVE_TO_EVENT step in a campaign with no event
	// start, so no absolute time can be derived.
	SkipNoEventAnchor = "skip:no_event_anchor"
	// SkipCampaignClosed: the campaign is finished or archived, so no step of
	// it will run again.
	SkipCampaignClosed = "skip:campaign_closed"
	// SkipNotEligible: the step carries an audience cutoff and this contact
	// entered the campaign before it. Unlike the other reasons this one can
	// never stop being true — an enrolment's entry time does not move — so the
	// row is written once and never reconsidered.
	SkipNotEligible = "skip:recipient_not_eligible"
	// SkipDailySequenceDone: the step belongs to a daily webinar sequence this
	// contact has already been through. The webinar repeats; the sequence does
	// not. This is what stops an existing participant receiving the same seven
	// reminders again tomorrow, and the day after.
	SkipDailySequenceDone = "skip:daily_sequence_done"
)

// IsSkipReason reports whether a cancel reason marks a deliberate skip rather
// than a revocation.
func IsSkipReason(reason string) bool {
	switch reason {
	case SkipStepExpired, SkipStepDisabled, SkipNoEventAnchor, SkipCampaignClosed,
		SkipNotEligible, SkipDailySequenceDone:
		return true
	}
	return false
}

// EligibleFor reports whether a contact who entered the campaign at enrolledAt
// may receive this step.
//
// The boundary is inclusive: a contact who arrived at exactly the cutoff is in.
// This is the single definition of eligibility in the system — the planner, the
// reconciler and the send path all call it, so the queue and the worker can
// never disagree about who a message is for.
//
// A step with the filter switched off is open to everyone, which is what keeps
// the feature opt-in: an unconfigured step behaves as it always has.
func (s *CampaignStep) EligibleFor(enrolledAt time.Time) bool {
	if !s.AudienceFilterEnabled || s.AudienceMinJoinedAt == nil {
		return true
	}
	return !enrolledAt.Before(*s.AudienceMinJoinedAt)
}

type ScheduledMessage struct {
	ID           uuid.UUID `json:"id"`
	CampaignID   uuid.UUID `json:"campaign_id"`
	ContactID    uuid.UUID `json:"contact_id"`
	EnrollmentID uuid.UUID `json:"enrollment_id"`
	StepID       uuid.UUID `json:"campaign_step_id"`
	RunNumber    int       `json:"run_number"`
	ScheduledAt  time.Time `json:"scheduled_at"`
	Status       JobStatus `json:"status"`
	// TemplateVersion is the revision that was current when the job was queued.
	// The campaign decides whether it is used for rendering or only reported.
	TemplateVersion *int       `json:"template_version"`
	AttemptCount    int        `json:"attempt_count"`
	NextAttemptAt   *time.Time `json:"next_attempt_at"`
	LockedBy        *string    `json:"-"`
	LockedAt        *time.Time `json:"-"`
	SentAt          *time.Time `json:"sent_at"`
	CancelledAt     *time.Time `json:"cancelled_at"`
	CancelReason    string     `json:"cancel_reason"`
	LastError       string     `json:"last_error"`
	MessageID       *uuid.UUID `json:"message_id"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`

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
