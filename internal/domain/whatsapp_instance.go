package domain

import "time"

// PrimaryInstanceID is the account every row in this database belonged to
// before multi-account support existed.
//
// It is referenced in exactly two situations: when a row's
// whatsapp_instance_id is NULL, which can only happen for a row written by
// hand outside the application, and when a legacy API request omits the field.
// Everything else resolves the account from the campaign it belongs to. Code
// that needs "the account this thing sends through" must never reach for this
// constant as a shortcut -- that is how cross-sending starts.
const PrimaryInstanceID int64 = 1

// Instance keys are the stable identifiers configuration is matched against.
// The display name is editable; these are not.
const (
	InstanceKeyPrimary   = "primary"
	InstanceKeySecondary = "secondary"
)

// WhatsAppInstance is one logical WhatsApp account: a number the installation
// owns, and the campaigns, triggers and schedules that belong to it.
//
// It carries no credentials, by design. The Green API instance id and token
// that back an account live in the process environment, are held only by the
// provider client, and are never stored, never serialised to the admin panel
// and never logged. What is stored here is ownership.
type WhatsAppInstance struct {
	ID  int64  `json:"id"`
	Key string `json:"instance_key"`
	// Name and PhoneLabel are what the panel shows. Both are safe to display.
	Name        string    `json:"name"`
	PhoneLabel  string    `json:"phone_label"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Configured reports whether credentials for this account are present in
	// the environment. Derived at read time, never stored: an account row can
	// legitimately exist before anybody has connected a number to it.
	Configured bool `json:"configured"`
	// CampaignCount is filled in by the list query so the panel can warn
	// before an account is disabled.
	CampaignCount int `json:"campaign_count"`
}

// Active reports whether this account should poll for messages and send.
//
// Both halves have to hold: an operator can switch an account off in the
// database, and an account whose credentials were never supplied has nothing
// to talk to. Neither state affects the data already stored against it.
func (i *WhatsAppInstance) Active() bool {
	return i != nil && i.Enabled && i.Configured
}

// InstanceIDOrPrimary resolves a possibly-absent owner to a usable account id.
//
// Rows written before migration 0007, or by hand afterwards, carry NULL here.
// Reading them as Primary is what keeps such a row routable instead of
// stranding its messages in the queue forever.
func InstanceIDOrPrimary(id *int64) int64 {
	if id == nil || *id <= 0 {
		return PrimaryInstanceID
	}
	return *id
}
