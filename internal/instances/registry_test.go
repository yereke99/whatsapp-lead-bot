package instances

import (
	"errors"
	"testing"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/whatsapp/greenapi"
)

// newTestRegistry assembles a registry directly, without a database. Load is
// exercised by the integration suite; this file is about the lookup rules,
// which are what stand between a campaign and the wrong phone.
func newTestRegistry(t *testing.T, entries ...*Instance) *Registry {
	t.Helper()

	reg := &Registry{byID: map[int64]*Instance{}, byKey: map[string]*Instance{}}
	for _, inst := range entries {
		reg.byID[inst.ID] = inst
		reg.byKey[inst.Key] = inst
		reg.ordered = append(reg.ordered, inst)
	}
	return reg
}

func configuredClient(t *testing.T) *greenapi.Client {
	t.Helper()
	return greenapi.New(config.GreenAPI{InstanceID: "1101", Token: "secret"}, discardLogger(t))
}

// TestProviderForNeverFallsBack is the single most important rule in this
// package.
//
// Every other kind of failure here is recoverable: a message that fails to
// send stays in the queue and is retried, and an operator sees it in the
// panel. Sending it through a different WhatsApp number is not recoverable --
// the customer has already received a message from a business they did not
// write to, and no amount of correction takes it back. So every path that
// cannot produce *this account's* client must produce an error instead.
func TestProviderForNeverFallsBack(t *testing.T) {
	primary := &Instance{ID: 1, Key: "primary", Enabled: true, Client: configuredClient(t)}
	disabled := &Instance{ID: 2, Key: "secondary", Enabled: false, Client: configuredClient(t)}
	unconfigured := &Instance{ID: 3, Key: "third", Enabled: true}

	reg := newTestRegistry(t, primary, disabled, unconfigured)

	t.Run("a healthy account resolves to its own client", func(t *testing.T) {
		got, err := reg.ProviderFor(1)
		if err != nil {
			t.Fatalf("ProviderFor(1): %v", err)
		}
		if got != primary.Client {
			t.Fatal("resolved a different account's client")
		}
	})

	for _, tc := range []struct {
		name string
		id   int64
	}{
		{"a disabled account", 2},
		{"an account with no credentials", 3},
		{"an account this process does not serve", 99},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			got, err := reg.ProviderFor(tc.id)
			if err == nil {
				t.Fatal("expected an error rather than another account's client")
			}
			if got != nil {
				t.Fatal("a provider was returned alongside the error")
			}
			if !errors.Is(err, ErrUnknownInstance) {
				t.Fatalf("error should wrap ErrUnknownInstance, got %v", err)
			}
		})
	}
}

// TestGetTreatsAnUnsetAccountAsPrimary covers rows written before migration
// 0007, and any caller that predates the field: they resolve to the account
// they have always belonged to rather than becoming unroutable.
func TestGetTreatsAnUnsetAccountAsPrimary(t *testing.T) {
	primary := &Instance{ID: 1, Key: "primary", Enabled: true, Client: configuredClient(t)}
	reg := newTestRegistry(t, primary)

	for _, id := range []int64{0, -1} {
		got, err := reg.Get(id)
		if err != nil {
			t.Fatalf("Get(%d): %v", id, err)
		}
		if got != primary {
			t.Fatalf("Get(%d) resolved %q, want the primary account", id, got.Key)
		}
	}
}

// TestActiveOnlyListsAccountsThatCanWork is what decides how many pollers
// start. An account that is switched off, or has no credentials, must not get
// one -- and must keep everything it owns.
func TestActiveOnlyListsAccountsThatCanWork(t *testing.T) {
	reg := newTestRegistry(t,
		&Instance{ID: 1, Key: "primary", Enabled: true, Client: configuredClient(t)},
		&Instance{ID: 2, Key: "secondary", Enabled: false, Client: configuredClient(t)},
		&Instance{ID: 3, Key: "third", Enabled: true},
	)

	active := reg.Active()
	if len(active) != 1 || active[0].Key != "primary" {
		keys := make([]string, 0, len(active))
		for _, a := range active {
			keys = append(keys, a.Key)
		}
		t.Fatalf("Active() = %v, want only the primary account", keys)
	}
	if len(reg.All()) != 3 {
		t.Fatalf("All() must still list every account, got %d", len(reg.All()))
	}
}
