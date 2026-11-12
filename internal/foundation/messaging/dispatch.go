package messaging

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Dispatcher delivers events in process to the subscriptions of the contexts a process composes
// (brant-worker -dispatch). It is the eventing.Publisher the Relay and the Replayer publish to, in
// place of an HTTP sink, so dead letters and their replay work as they do with a sink.
//
// Publish hands the envelope to every subscription of its source and type, in the order of the
// subscriptions, each through a Consumer on the subscribing context's own DB: one transaction per
// consumer, bound to the event's scope, deduplicated by event ID. Delivery is at least once: a
// consumer that processed the event before sees a duplicate and does not run again, so a
// redelivery, a second worker and a dead-letter replay run each handler to completion once.
//
// Publish settles the delivery for the relay by the consumers' outcomes:
//   - every consumer processed it, now or before (or nobody subscribes to it): nil, and the relay
//     marks it published;
//   - a consumer must retry (its handler failed, its commit is uncertain, another worker holds its
//     inbox entry): an error the relay retries with backoff and dead-letters once its budget is
//     spent; the consumers that processed it see a duplicate on the next attempt;
//   - otherwise, a consumer rejected it, now or before (RejectEvent): eventing.ErrPublishRejected,
//     so the relay dead-letters it at once and alerts (ADR-BR-015), and a replay is refused like
//     the sink refuses a rejected event.
//
// The relay claims one delivery per partition at a time and Publish returns only when every
// consumer settled, so the events of one aggregate reach each consumer in their order. A consumer
// that keeps failing holds up its partition until the delivery is dead-lettered.
//
// The relay's publish timeout bounds one Publish, all consumers together.
type Dispatcher struct {
	subs   []Subscription
	routes map[routeKey][]route
}

var _ eventing.Publisher = (*Dispatcher)(nil)

// errNoDB is NewDispatcher's refusal of a subscription without its context's DB.
var errNoDB = errors.New("messaging: subscription needs its consuming context's DB")

type routeKey struct {
	source    string
	eventType eventing.EventType
}

// route is one subscription with the consumer that runs it.
type route struct {
	sub      Subscription
	consumer Consumer // Handler is set per delivery
	actor    kernel.ActorRef
	executor func(context.Context) (bentopg.Executor, error)
}

// NewDispatcher returns the dispatcher of subs, normally every subscription the composed contexts
// declare. It refuses an invalid subscription (Subscription), a subscription without its DB, a
// consumer on two DBs and a consumer subscribed twice to an event type. Without subscriptions it
// delivers every event to nobody.
func NewDispatcher(subs ...Subscription) (*Dispatcher, error) {
	return newDispatcher(subs, func(s Subscription) (route, error) {
		if s.DB == nil {
			return route{}, fmt.Errorf("%w: %s", errNoDB, s.Consumer)
		}
		// The handler is replaced per delivery; this one only satisfies the constructor.
		c, err := NewConsumer(s.DB, s.Consumer, EventHandlerFunc(func(context.Context, eventing.Envelope) error {
			return errors.New("messaging: dispatcher route without a delivery")
		}))
		if err != nil {
			return route{}, err
		}
		return route{sub: s, consumer: c, executor: s.DB.RequireExecutor}, nil
	})
}

func newDispatcher(subs []Subscription, build func(Subscription) (route, error)) (*Dispatcher, error) {
	if err := validateSubscriptions(subs); err != nil {
		return nil, err
	}
	d := &Dispatcher{subs: slices.Clone(subs), routes: map[routeKey][]route{}}
	for i, s := range d.subs {
		s.EventTypes = slices.Clone(s.EventTypes)
		s.Permissions = slices.Clone(s.Permissions)
		d.subs[i] = s
		r, err := build(s)
		if err != nil {
			return nil, err
		}
		if r.actor, err = InboxActor(s.Consumer); err != nil {
			return nil, err
		}
		for _, t := range s.EventTypes {
			k := routeKey{s.Source, t}
			d.routes[k] = append(d.routes[k], r)
		}
	}
	return d, nil
}

// Subscriptions returns the subscriptions d delivers to, in order.
func (d *Dispatcher) Subscriptions() []Subscription { return slices.Clone(d.subs) }

// Consumers returns the names of the consumers of an event of source and type, in delivery order.
func (d *Dispatcher) Consumers(source string, eventType eventing.EventType) []eventing.InboxConsumer {
	var out []eventing.InboxConsumer
	for _, r := range d.routes[routeKey{source, eventType}] {
		out = append(out, r.sub.Consumer)
	}
	return out
}

// Publish delivers envelope to every subscribed consumer; see Dispatcher.
func (d *Dispatcher) Publish(ctx context.Context, envelope eventing.Envelope) error {
	if err := envelope.Validate(); err != nil {
		return fmt.Errorf("%w: %w", eventing.ErrPublishRejected, err)
	}
	routes := d.routes[routeKey{envelope.Source, envelope.Type}]
	var retry, rejected []error
	for _, r := range routes {
		if err := ctx.Err(); err != nil {
			retry = append(retry, err)
			break
		}
		outcome, err := r.deliver(ctx, envelope)
		switch outcome {
		case InboxProcessed, InboxDuplicate:
		case InboxRejected:
			rejected = append(rejected, fmt.Errorf("consumer %s: %w", r.sub.Consumer, err))
		default:
			retry = append(retry, fmt.Errorf("consumer %s: %w", r.sub.Consumer, err))
		}
	}
	switch {
	case len(retry) > 0:
		return fmt.Errorf("messaging: dispatch %s/%s: %w", envelope.Source, envelope.ID, errors.Join(append(retry, rejected...)...))
	case len(rejected) > 0:
		return fmt.Errorf("%w: dispatch %s/%s: %w", eventing.ErrPublishRejected, envelope.Source, envelope.ID, errors.Join(rejected...))
	}
	return nil
}

// deliver runs the subscription's Prepare and then its consumer with the handler of this delivery.
func (r route) deliver(ctx context.Context, envelope eventing.Envelope) (InboxOutcome, error) {
	var prepared any
	var prepareErr error
	if r.sub.Prepare != nil {
		// An envelope whose scope does not parse is rejected by Consume; Prepare never sees it.
		if scope, err := kernel.ParseScope(envelope.Scope); err == nil {
			pctx, err := asConsumer(ctx, r.actor, scope, envelope)
			if err != nil {
				return InboxRetry, err
			}
			prepared, prepareErr = r.sub.Prepare(pctx, envelope.Clone())
		}
	}
	c := r.consumer
	c.Handler = EventHandlerFunc(func(txCtx context.Context, e eventing.Envelope) error {
		if prepareErr != nil {
			return prepareErr // after Start, so an event settled before stays settled
		}
		exec, err := r.executor(txCtx)
		if err != nil {
			return err
		}
		p, err := identity.FromContext(txCtx)
		if err != nil {
			return err
		}
		return r.sub.Handler(txCtx, Event{Consumer: r.sub.Consumer, Envelope: e, Principal: p, Exec: exec, Prepared: prepared})
	})
	return c.Consume(ctx, envelope)
}
