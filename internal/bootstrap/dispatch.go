package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// subscriber is a module that declares event subscriptions (repository structure §07).
type subscriber interface {
	Subscriptions() []messaging.Subscription
}

// Subscriptions returns the event subscriptions of the composed contexts: those of every module in
// c that has a Subscriptions method, in field order. A context added to Contexts is collected
// without a change here.
func (c *Contexts) Subscriptions() []messaging.Subscription { return collectSubscriptions(c) }

// collectSubscriptions gathers the subscriptions of the non-nil fields of the struct modules
// points to.
func collectSubscriptions(modules any) []messaging.Subscription {
	v := reflect.ValueOf(modules).Elem()
	var out []messaging.Subscription
	for i := range v.NumField() {
		f := v.Field(i)
		if !f.CanInterface() || ((f.Kind() == reflect.Pointer || f.Kind() == reflect.Interface) && f.IsNil()) {
			continue
		}
		if s, ok := f.Interface().(subscriber); ok {
			out = append(out, s.Subscriptions()...)
		}
	}
	return out
}

// Dispatcher returns in-process dispatch to the subscriptions of c (messaging.Dispatcher): each
// event reaches every subscribed consumer through the Inbox on its context's own transaction owner.
func (c *Contexts) Dispatcher() (*messaging.Dispatcher, error) {
	return messaging.NewDispatcher(c.Subscriptions()...)
}

// Dispatcher wires every context (Contexts) and returns the dispatch to their subscriptions, the
// publisher of brant-worker -dispatch. Call it at most once per process, instead of Contexts.
func (p *Platform) Dispatcher() (*messaging.Dispatcher, error) {
	c, err := p.Contexts()
	if err != nil {
		return nil, err
	}
	return c.Dispatcher()
}

// publisherFor returns the HTTP sink at sinkURL or publisher, whichever is set; exactly one must be.
func publisherFor(sinkURL string, publisher eventing.Publisher) (eventing.Publisher, error) {
	switch {
	case sinkURL != "" && publisher != nil:
		return nil, errors.New("bootstrap: events go to a sink URL or a publisher, not both")
	case publisher != nil:
		return publisher, nil
	case sinkURL != "":
		return messaging.Webhook{URL: sinkURL}, nil
	}
	return nil, errors.New("bootstrap: events need a sink URL or a publisher")
}

// ProvisionInbox gives the system actor of every consumer in subs, system:inbox.<consumer>
// (messaging.InboxActor), exactly the permissions its subscriptions declare and data scope
// company in scope, creating the scope's authority row when it is missing. It is an operator's
// change like Provision: run it for every company the consumers serve, and again when a consumer's
// permissions change (brant-admin inbox). A consumer's handler runs the context's commands as this
// actor, so without it every event the consumer must authorize is retried until it is
// dead-lettered. It returns the scope's authority epoch after the change, 0 without consumers.
func (p *Platform) ProvisionInbox(ctx context.Context, scope kernel.Scope, subs []messaging.Subscription) (int64, error) {
	grants, err := messaging.InboxGrants(subs)
	if err != nil || len(grants) == 0 {
		return 0, err
	}
	return p.inAuthority(ctx, scope, func(ctx context.Context, exec bentopg.Executor, g authorization.Grants, now time.Time) (int64, error) {
		if err := g.Provision(ctx, exec, scope); err != nil {
			return 0, err
		}
		var epoch int64
		for _, grant := range grants {
			var err error
			if _, err = g.Put(ctx, exec, scope, grant.Actor, grant.Permissions, now); err != nil {
				return 0, err
			}
			if epoch, err = g.SetDataScope(ctx, exec, scope, grant.Actor.ID, authorization.DataScopeCompany, "", now); err != nil {
				return 0, err
			}
		}
		return epoch, nil
	})
}

// ProvisionInboxFor is ProvisionInbox for every consumer the composed contexts subscribe, from
// plain operator input, for cmd/brant-admin. It returns each consumer's actor and permissions.
func (p *Platform) ProvisionInboxFor(ctx context.Context, tenant, company string) ([]messaging.InboxGrant, int64, error) {
	scope, err := kernel.NewScope(tenant, company)
	if err != nil {
		return nil, 0, err
	}
	c, err := p.Contexts()
	if err != nil {
		return nil, 0, err
	}
	subs := c.Subscriptions()
	grants, err := messaging.InboxGrants(subs)
	if err != nil {
		return nil, 0, err
	}
	epoch, err := p.ProvisionInbox(ctx, scope, subs)
	return grants, epoch, err
}
