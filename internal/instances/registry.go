package instances

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
	"github.com/ayran/whatsapp-automation/internal/whatsapp/greenapi"
)

// ErrUnknownInstance is returned when something asks to send through an
// account this process does not serve.
//
// It is a permanent failure on purpose. A job whose account is missing must
// not fall back to another number -- that is precisely the cross-send this
// whole design exists to prevent -- so it fails, stays visible in the panel,
// and waits for the operator to supply the credentials.
var ErrUnknownInstance = errors.New("whatsapp account is not configured on this server")

// Instance is one WhatsApp account as the running process sees it: the stored
// identity, plus the provider client bound to its own credentials.
//
// The client is created once and never mutated. Two accounts never share one,
// and nothing swaps credentials on a live client between requests, so there is
// no window in which a message could leave through the wrong number.
type Instance struct {
	ID          int64
	Key         string
	Name        string
	PhoneLabel  string
	Description string
	Enabled     bool

	// Client is nil when no credentials are configured for this account.
	Client *greenapi.Client
	// Poll carries the account's own receive timeout and poll interval, so one
	// number can long-poll differently from another.
	Poll config.GreenAPI
	// Outbound carries the account's own pacing. It is applied by the outbound
	// router, which keeps one queue per account.
	Outbound config.Outbound
}

// Configured reports whether credentials were supplied for this account.
func (i *Instance) Configured() bool { return i != nil && i.Client != nil && i.Client.Configured() }

// Active reports whether this account should poll and send.
func (i *Instance) Active() bool { return i != nil && i.Enabled && i.Configured() }

// Label is what logs and the panel call this account.
func (i *Instance) Label() string {
	if i == nil {
		return "unknown"
	}
	if strings.TrimSpace(i.Name) != "" {
		return i.Name
	}
	return i.Key
}

// LogAttrs is the identity every log line about this account should carry.
// Credentials are never among them.
func (i *Instance) LogAttrs() []any {
	if i == nil {
		return []any{slog.String("instance", "unknown")}
	}
	return []any{
		slog.Int64("whatsapp_instance_id", i.ID),
		slog.String("instance", i.Key),
	}
}

// Registry is the process's view of every WhatsApp account.
//
// It is built once at startup and never written to afterwards, so every
// lookup is a read of immutable data and needs no locking.
type Registry struct {
	byID    map[int64]*Instance
	byKey   map[string]*Instance
	ordered []*Instance
}

// Load reconciles the accounts stored in the database with the credentials
// found in the environment.
//
// The database is authoritative about which accounts exist and what they own;
// the environment is authoritative about which of them this process can
// actually reach. An account present in one and not the other is not an error:
//
//   - stored but not configured -- the row and everything it owns stays, the
//     account simply does not poll or send, and the panel shows it as not
//     connected. This is the normal state of Secondary before its number is
//     bought.
//   - configured but not stored -- refused, because a campaign could never
//     point at it. Migration 0007 creates the rows, so this only happens if
//     somebody adds a fourth block of credentials without a matching row.
func Load(ctx context.Context, repo *Repository, cfg *config.Config, log *slog.Logger) (*Registry, error) {
	stored, err := repo.List(ctx, nil)
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return nil, errors.New("no whatsapp accounts found: migration 0007 should have created them")
	}

	configured := map[int64]config.WhatsAppInstance{}
	for _, c := range cfg.Instances {
		configured[c.ID] = c
	}
	for id := range configured {
		found := false
		for _, row := range stored {
			if row.ID == id {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf(
				"green api credentials are configured for whatsapp account %d, but no such account exists in the database; "+
					"add a row to whatsapp_instances before configuring it", id)
		}
	}

	reg := &Registry{byID: map[int64]*Instance{}, byKey: map[string]*Instance{}}

	for _, row := range stored {
		inst := &Instance{
			ID:          row.ID,
			Key:         row.Key,
			Name:        row.Name,
			PhoneLabel:  row.PhoneLabel,
			Description: row.Description,
			Enabled:     row.Enabled,
			Poll:        cfg.GreenAPI,
			Outbound:    cfg.Outbound,
		}

		if c, ok := configured[row.ID]; ok {
			inst.Poll = c.GreenAPI
			inst.Outbound = c.Outbound
			// Enabled has to hold in both places: an operator can switch an
			// account off from the environment without touching the database,
			// and from the database without touching the environment.
			inst.Enabled = row.Enabled && c.Enabled
			inst.Client = greenapi.New(c.GreenAPI, log.With(
				slog.String("instance", row.Key),
				slog.Int64("whatsapp_instance_id", row.ID)))

			// Labels configured in the environment win, so an operator can name
			// the numbers where the credentials live. Silent when unset.
			if err := repo.SyncLabels(ctx, row.ID, c.Name, c.PhoneLabel); err != nil {
				log.Warn("could not sync whatsapp account labels",
					slog.Int64("whatsapp_instance_id", row.ID),
					slog.String("error", err.Error()))
			} else {
				if strings.TrimSpace(c.Name) != "" {
					inst.Name = c.Name
				}
				if strings.TrimSpace(c.PhoneLabel) != "" {
					inst.PhoneLabel = c.PhoneLabel
				}
			}
		}

		reg.byID[inst.ID] = inst
		reg.byKey[strings.ToLower(strings.TrimSpace(inst.Key))] = inst
		reg.ordered = append(reg.ordered, inst)
	}

	sort.Slice(reg.ordered, func(i, j int) bool { return reg.ordered[i].ID < reg.ordered[j].ID })

	for _, inst := range reg.ordered {
		switch {
		case !inst.Enabled:
			log.Warn("[WHATSAPP] account is disabled: it will not poll or send; its data is untouched",
				slog.Int64("whatsapp_instance_id", inst.ID),
				slog.String("instance", inst.Key))
		case !inst.Configured():
			log.Warn("[WHATSAPP] account has no green api credentials: it will not poll or send",
				slog.Int64("whatsapp_instance_id", inst.ID),
				slog.String("instance", inst.Key),
				slog.String("expected_env", "GREEN_API_INSTANCE_ID"+envSuffix(inst.ID)+" / GREEN_API_TOKEN"+envSuffix(inst.ID)))
		default:
			log.Info("[WHATSAPP] account ready",
				slog.Int64("whatsapp_instance_id", inst.ID),
				slog.String("instance", inst.Key),
				slog.String("name", inst.Name),
				slog.Duration("send_min_delay", inst.Outbound.MinDelay),
				slog.Duration("send_max_delay", inst.Outbound.MaxDelay))
		}
	}

	return reg, nil
}

// envSuffix renders the environment-variable suffix an account's credentials
// are read from, for error messages only.
func envSuffix(id int64) string {
	if id <= 1 {
		return ""
	}
	return fmt.Sprintf("_%d", id)
}

// All returns every known account, Primary first.
func (r *Registry) All() []*Instance {
	if r == nil {
		return nil
	}
	return r.ordered
}

// Active returns the accounts that should poll and send.
func (r *Registry) Active() []*Instance {
	if r == nil {
		return nil
	}
	var out []*Instance
	for _, inst := range r.ordered {
		if inst.Active() {
			out = append(out, inst)
		}
	}
	return out
}

// Get resolves an account id. An id of zero, or one carried by a row written
// before migration 0007, resolves to Primary.
func (r *Registry) Get(id int64) (*Instance, error) {
	if r == nil {
		return nil, ErrUnknownInstance
	}
	if id <= 0 {
		id = domain.PrimaryInstanceID
	}
	inst, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: id %d", ErrUnknownInstance, id)
	}
	return inst, nil
}

// ByKey resolves an account by its stable key, for tests and for logs.
func (r *Registry) ByKey(key string) (*Instance, bool) {
	if r == nil {
		return nil, false
	}
	inst, ok := r.byKey[strings.ToLower(strings.TrimSpace(key))]
	return inst, ok
}

// ProviderFor resolves the transport a message must leave through.
//
// This is the single point where "which campaign is this?" becomes "which
// WhatsApp number does this leave through?", and it is why no send path
// anywhere else in the codebase names an account. A disabled or unconfigured
// account returns an error rather than any other account's client.
func (r *Registry) ProviderFor(instanceID int64) (whatsapp.Provider, error) {
	inst, err := r.Get(instanceID)
	if err != nil {
		return nil, err
	}
	if !inst.Enabled {
		return nil, fmt.Errorf("%w: account %q is disabled", ErrUnknownInstance, inst.Key)
	}
	if !inst.Configured() {
		return nil, fmt.Errorf("%w: account %q has no green api credentials", ErrUnknownInstance, inst.Key)
	}
	return inst.Client, nil
}

// Describe returns the panel-safe view of every account: identity, whether it
// is connected, and how many campaigns it owns. Credentials are not part of
// the type it returns and cannot leak through it.
func (r *Registry) Describe(stored []domain.WhatsAppInstance) []domain.WhatsAppInstance {
	out := make([]domain.WhatsAppInstance, 0, len(stored))
	for _, row := range stored {
		if inst, ok := r.byID[row.ID]; ok {
			row.Configured = inst.Configured()
			row.Enabled = inst.Enabled
			if strings.TrimSpace(inst.Name) != "" {
				row.Name = inst.Name
			}
			if strings.TrimSpace(inst.PhoneLabel) != "" {
				row.PhoneLabel = inst.PhoneLabel
			}
		}
		out = append(out, row)
	}
	return out
}
