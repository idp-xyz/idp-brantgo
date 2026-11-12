package messaging

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SourcePrefix starts every Brant event source: idp-brant.<bc> (contract C4).
const SourcePrefix = "idp-brant."

var (
	sourcePattern    = regexp.MustCompile(`^idp-brant\.[a-z]{3}$`)
	eventTypePattern = regexp.MustCompile(`^idp-brant\.[a-z]{3}\.[a-z0-9_]+\.v[0-9]+$`) // contract C3, the envelope schema
	consumerPattern  = regexp.MustCompile(`^[a-z]{3}\.[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
)

// Subscription is one consumer's interest in the events of one upstream context. A context declares
// its subscriptions in its module (repository structure §07):
//
//	func (m *Module) Subscriptions() []messaging.Subscription
//
// and the composition root collects them from every composed context for the Dispatcher.
//
// Context Map rule D1: a context subscribes only to the events of its registered upstreams. The
// consumer name starts with the consuming context's code and Source names the upstream context, so
// the composition root's tests check every subscription against the allowlist generated from the
// Context Map, as tools/dependency-check checks imports.
type Subscription struct {
	// Consumer is "<bc>.<purpose>", <bc> being the consuming context, such as "stl.order_credit".
	// The Inbox deduplicates per consumer, source and event ID, and the handler runs as the
	// consumer's system actor system:inbox.<consumer> (InboxActor). One consumer may subscribe to
	// several sources; all its subscriptions use the same DB.
	Consumer eventing.InboxConsumer
	// Source is the upstream context's event source, idp-brant.<bc> (its api.Source).
	Source string
	// EventTypes are the types of Source the consumer handles, with their major version
	// (idp-brant.<bc>.<name>.v<N>, contract C3; the upstream api's Event constants). No other event
	// reaches the handler.
	EventTypes []eventing.EventType
	// DB is the consuming context's own transaction owner. The inbox entry, the handler's effect
	// and whatever the handler runs through the context's own command ledger commit together in one
	// transaction on it, bound to the event's scope.
	DB *bentopg.DB
	// Permissions are what the handler's commands need. The consumer's system actor is granted
	// exactly the permissions of all its subscriptions, with data scope company, in every company it
	// serves (bootstrap Platform.ProvisionInbox, brant-admin inbox).
	Permissions []string
	// Prepare, when set, runs for each delivery before the consumer's transaction begins, outside
	// any transaction: the place for reads of upstream contexts (rule T3), which the handler cannot
	// make inside its transaction. It runs as the consumer's system actor in the event's scope, also
	// for an event the consumer processed before (only reads belong here), and what it returns
	// reaches Handler as Event.Prepared. An error is the handler's error: RejectEvent refuses the
	// event for good, any other error retries it, unless the event was already settled.
	Prepare func(ctx context.Context, envelope eventing.Envelope) (any, error)
	// Handler applies one event; see EventFunc.
	Handler EventFunc
}

// Event is one integration event as a subscription's handler receives it, inside the consumer's
// transaction.
type Event struct {
	Consumer eventing.InboxConsumer
	// Envelope is a copy of the delivered envelope. Its payload carries identifiers and versions
	// only (contract C4), among them aggregate_type, aggregate_id and aggregate_version.
	Envelope eventing.Envelope
	// Principal is the consumer's system actor in the event's scope. The actor who caused the event
	// is named in the payload for the record only; events are never processed as that actor.
	Principal identity.Principal
	// Exec is the transaction on the consumer's DB, bound to Principal.Scope under FORCE RLS.
	Exec bentopg.Executor
	// Prepared is what the subscription's Prepare returned; nil without Prepare.
	Prepared any
}

// EventFunc applies one event for a subscription.
//
// ctx carries the consumer's transaction on its context's DB, the event's scope, the consumer's
// Principal and the event's correlation ID: the context's repositories and its own action.Ledger,
// called with ctx, join the transaction, so a command the handler executes commits its result,
// audit entry and Outbox events together with the inbox record. A call into another context's
// transaction owner fails with application.ErrTransactionConflict; upstream state comes from
// Prepare or from a guard function the upstream context publishes, called with e.Exec.
//
// A nil error commits the effect and marks the event processed. RejectEvent refuses the event for
// good: everything rolls back, the inbox records the rejection and the relay dead-letters the
// delivery with an alert (ADR-BR-015). Any other error rolls everything back, including a
// rejection audit the ledger wrote, and the event is delivered again later.
//
// Delivery is at least once and deduplicated by event ID, so the handler runs to completion once
// per consumer and event. The relay delivers the events of one aggregate in their order, but a
// dead-letter replay arrives after the later events of its aggregate: a handler whose effect
// depends on order compares the payload's aggregate_version with what it applied.
type EventFunc func(ctx context.Context, e Event) error

// SourceContext returns the context code of an event source idp-brant.<bc>, or "" for a source of
// another form.
func SourceContext(source string) string {
	if !sourcePattern.MatchString(source) {
		return ""
	}
	return strings.TrimPrefix(source, SourcePrefix)
}

// ConsumerContext returns the code of the context a consumer "<bc>.<purpose>" belongs to, or ""
// for a name of another form.
func ConsumerContext(consumer eventing.InboxConsumer) string {
	if !consumerPattern.MatchString(string(consumer)) {
		return ""
	}
	bc, _, _ := strings.Cut(string(consumer), ".")
	return bc
}

// validate checks one subscription on its own.
func (s Subscription) validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("messaging: subscription %s to %s: %s", s.Consumer, s.Source, fmt.Sprintf(format, args...))
	}
	consumer, source := ConsumerContext(s.Consumer), SourceContext(s.Source)
	switch {
	case consumer == "":
		return fail("consumer is not <bc>.<purpose>")
	case source == "":
		return fail("source is not idp-brant.<bc>")
	case consumer == source:
		// D1: only the events of upstream contexts; a context's own follow-up belongs in its own
		// transaction.
		return fail("a context does not subscribe to its own events")
	case len(s.EventTypes) == 0:
		return fail("no event types")
	case s.Handler == nil:
		return fail("no handler")
	}
	if _, err := InboxActor(s.Consumer); err != nil {
		return fail("%v", err)
	}
	for i, t := range s.EventTypes {
		if !eventTypePattern.MatchString(string(t)) || !strings.HasPrefix(string(t), s.Source+".") {
			return fail("event type %q is not %s.<name>.v<N>", t, s.Source)
		}
		if slices.Contains(s.EventTypes[:i], t) {
			return fail("event type %s listed twice", t)
		}
	}
	for _, p := range s.Permissions {
		if !kernel.ValidIdentifier(p) {
			return fail("permission %q", p)
		}
	}
	return nil
}

// validateSubscriptions checks subs together: each on its own, one consumer on one DB, and no
// consumer subscribed twice to an event type (its second handler would never run: the inbox
// deduplicates per consumer).
func validateSubscriptions(subs []Subscription) error {
	type key struct {
		consumer eventing.InboxConsumer
		source   string
		typ      eventing.EventType
	}
	seen := map[key]bool{}
	dbs := map[eventing.InboxConsumer]*bentopg.DB{}
	for _, s := range subs {
		if err := s.validate(); err != nil {
			return err
		}
		if db, ok := dbs[s.Consumer]; ok && db != s.DB {
			return fmt.Errorf("messaging: consumer %s subscribes on two DBs", s.Consumer)
		}
		dbs[s.Consumer] = s.DB
		for _, t := range s.EventTypes {
			k := key{s.Consumer, s.Source, t}
			if seen[k] {
				return fmt.Errorf("messaging: consumer %s subscribes to %s twice", s.Consumer, t)
			}
			seen[k] = true
		}
	}
	return nil
}

// InboxGrant is what one consumer's system actor needs in every company it serves.
type InboxGrant struct {
	Consumer    eventing.InboxConsumer
	Actor       kernel.ActorRef // system:inbox.<consumer>
	Permissions []string        // the union of its subscriptions' Permissions, sorted
}

// InboxGrants returns the grant of each consumer of subs, in the order consumers first appear.
func InboxGrants(subs []Subscription) ([]InboxGrant, error) {
	if err := validateSubscriptions(subs); err != nil {
		return nil, err
	}
	var out []InboxGrant
	at := map[eventing.InboxConsumer]int{}
	for _, s := range subs {
		i, ok := at[s.Consumer]
		if !ok {
			actor, err := InboxActor(s.Consumer)
			if err != nil {
				return nil, err
			}
			i = len(out)
			at[s.Consumer] = i
			out = append(out, InboxGrant{Consumer: s.Consumer, Actor: actor, Permissions: []string{}})
		}
		out[i].Permissions = append(out[i].Permissions, s.Permissions...)
	}
	for i := range out {
		slices.Sort(out[i].Permissions)
		out[i].Permissions = slices.Compact(out[i].Permissions)
	}
	return out, nil
}
