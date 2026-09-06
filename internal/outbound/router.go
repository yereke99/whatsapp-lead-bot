package outbound

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/messaging"
)

// InstanceSpec is one account's identity and sending pace, as the router needs
// it. It is a plain value rather than a reference to the account registry so
// that this package stays independent of how accounts are discovered.
type InstanceSpec struct {
	ID       int64
	Key      string
	Outbound config.Outbound
}

// Router is the outbound side of account isolation: one queue, one
// concurrency budget and one pacing timeline per WhatsApp number.
//
// The reason this is not a single shared gate is throughput fairness, and it
// matters more than it sounds. A gate paces *every* message that passes
// through it, so with one gate a hundred messages due on the second number at
// 20:45 would each take their turn on the same timeline as the first number's,
// and the first number's customers -- the ones already in production -- would
// receive their reminders minutes late because of a campaign they have nothing
// to do with. Green API meters per instance, so there is no reason to make
// them share: each account sends at its own configured pace, and neither can
// slow the other down.
//
// What is *not* duplicated is anything above the pacing. There is one sender,
// one scheduler, one campaign engine and one template engine; this type only
// decides which pacing timeline a message waits on before it reaches the one
// sender.
type Router struct {
	gates map[int64]*Gate
	log   *slog.Logger
}

// NewRouter builds one gate per account.
//
// Every known account gets a gate, including one whose credentials are not
// configured. That is deliberate: a message for an unconfigured account should
// fail in the sender, with an error naming the missing credentials, rather
// than vanish here with a routing error that tells the operator nothing.
func NewRouter(specs []InstanceSpec, sender Sender, log *slog.Logger) *Router {
	r := &Router{
		gates: make(map[int64]*Gate, len(specs)),
		log:   log.With(slog.String("component", "outbound_router")),
	}

	for _, spec := range specs {
		r.gates[spec.ID] = New(spec.Outbound, sender,
			log.With(slog.Int64("whatsapp_instance_id", spec.ID), slog.String("instance", spec.Key)))
	}

	return r
}

// Send paces the message on its own account's timeline and delivers it.
//
// An unknown account is a permanent failure, never a fallback to another
// number: delivering a second number's message through the first one is the
// single worst thing this subsystem could do, and it is worth failing loudly
// to keep it impossible.
func (r *Router) Send(ctx context.Context, contact *domain.Contact, out messaging.Outbound) (*domain.Message, error) {
	instanceID := domain.InstanceIDOrPrimary(&out.WhatsAppInstanceID)

	gate, ok := r.gates[instanceID]
	if !ok {
		return nil, fmt.Errorf("no outbound queue for whatsapp account %d: the account is not served by this process", instanceID)
	}
	return gate.Send(ctx, contact, out)
}

// Gate exposes one account's queue, for tests and diagnostics.
func (r *Router) Gate(instanceID int64) (*Gate, bool) {
	gate, ok := r.gates[instanceID]
	return gate, ok
}
