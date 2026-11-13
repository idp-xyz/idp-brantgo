package application

import (
	"context"
	"fmt"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What TRD's OrderActivated does to the order's account (unit B6b; S5 plan D-4, D-18; tactical model
// STL-AGG-OrderAccount; Context Map R32, R45): the Inbox consumer stl.order_account opens the account
// of the activated order at the activated revision with that revision's payment stages, through
// order_account.open as its system actor, so that PaymentConditionGuard v1 judges every active order
// by its stages — also one with nothing due before shipment — instead of answering no_account.
//
// TRD and POL are read before the consumer's transaction (PrepareAccount, the subscription's Prepare
// step); inside it STL locks the order's account FOR UPDATE when it exists, decides on STL's own rows
// and on what was read, and runs order_account.open without its own upstream reads. An order's end
// leaves its account as it is: the account has no end state, and applications made remain facts.
// Moving the account to an amended revision is the payment plan review (payment_plan_review.open,
// SM-STL-PAYMENT-PLAN-REVIEW, unit C4b), never this consumer.

// What an OrderActivated did to the order's account.
const (
	// EffectAccountOpened: the account is opened now at the activated revision.
	EffectAccountOpened = "account_opened"
	// EffectAccountOpen: it was open at the activated revision already (opened by a person, or
	// before a lost answer).
	EffectAccountOpen = "account_open"
	// EffectAccountElsewhere: the account holds another revision (the payment plan review moved it);
	// a replayed activation never moves it back.
	EffectAccountElsewhere = "account_elsewhere"
	// EffectOrderEnded: TRD's order is no longer active (it ended before the event was handled); no
	// account is opened for it.
	EffectOrderEnded = "order_ended"
	// EffectOrderAmended: TRD's order is active at a later revision than the activated one; the
	// account of that revision is the payment plan review's to open.
	EffectOrderAmended = "order_amended"
)

// FailureAccountNotOpenable is the failure code of an activation whose account STL cannot open for
// good: TRD shows no such order, its revision is not intact, or order_account.open refused it in a
// way no retry changes (the payment plan, the currency).
const FailureAccountNotOpenable = "stl.order_account_not_openable"

// AccountMismatch is an OrderActivated whose account STL cannot open for good: the consumer refuses
// the event and the relay dead-letters it with an alert (ADR-BR-015). Nothing is changed.
type AccountMismatch struct {
	Code   string
	Detail string
}

func (m *AccountMismatch) Error() string { return "stl: " + m.Code + ": " + m.Detail }

// AccountBasis is what opening an activated order's account needs from STL's upstreams, read before
// the consumer's transaction: TRD's order now (nil when TRD shows none) and, when it is active at the
// activated revision, its currency's POL revision.
type AccountBasis struct {
	Order    *Order
	Currency polapi.ResolvedConfig
}

// opens reports whether b's order is TRD's active order at e's binding, intact.
func (b AccountBasis) opens(e OrderEvent) bool {
	o := b.Order
	return o != nil && o.Code == "" && o.Status == OrderActive && o.Binding == OrderBinding{OrderID: e.OrderID, OrderRevision: e.OrderRevision,
		OrderHash: e.OrderHash}
}

// PrepareAccount reads, outside any transaction, what OrderActivated e's account needs: TRD's order
// (OrderCommercialSnapshot v1, R32) and, if it is still active at e's revision, the POL revision of
// its currency (R33). A failure is retried with the event.
func (s Service) PrepareAccount(ctx context.Context, e OrderEvent) (AccountBasis, error) {
	orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{e.OrderID})
	if err != nil {
		return AccountBasis{}, err
	}
	o, ok := orders[e.OrderID]
	if !ok {
		return AccountBasis{}, nil
	}
	b := AccountBasis{Order: &o}
	if !b.opens(e) {
		return b, nil
	}
	configs, err := s.resolveConfigs(ctx, s.Clock.Now(), map[string]polapi.ConfigRef{"/args/order_id": {Kind: polapi.KindCurrency,
		ConfigID: o.Currency.ID, Revision: o.Currency.Revision}})
	if err != nil {
		return AccountBasis{}, err
	}
	b.Currency = configs["/args/order_id"]
	return b, nil
}

// OpenOnActivation applies TRD's OrderActivated e to the order's account with b, read before the
// transaction: it opens the account at the activated revision through order_account.open, under
// the event ID as idempotency key, unless the account is open already (at that revision or, moved
// by the payment plan review, at another) or TRD's order has ended or moved to a later revision
// since. An order TRD does not show intact, or an open the command refuses for good, is an
// *AccountMismatch.
func (s Service) OpenOnActivation(ctx context.Context, exec bentopg.Executor, e OrderEvent, b AccountBasis) (string, error) {
	a, err := postgres.LoadAccount(ctx, exec, scope(ctx), e.OrderID, true)
	if err != nil {
		return "", err
	}
	if a != nil {
		if a.OrderRevision == e.OrderRevision && a.OrderHash == e.OrderHash {
			return EffectAccountOpen, nil
		}
		return EffectAccountElsewhere, nil
	}
	o := b.Order
	switch {
	case o == nil:
		return "", &AccountMismatch{Code: FailureAccountNotOpenable, Detail: fmt.Sprintf("TRD shows no order %s", e.OrderID)}
	case o.Code != "":
		return "", &AccountMismatch{Code: FailureAccountNotOpenable, Detail: fmt.Sprintf("TRD's order %s revision %d is not intact (%s)",
			e.OrderID, o.Binding.OrderRevision, o.Code)}
	case o.Status != OrderActive:
		return EffectOrderEnded, nil
	case !b.opens(e):
		return EffectOrderAmended, nil
	}
	cmd := api.OpenOrderAccount{IdempotencyKey: CommandKey(e.EventID, ""), OrderID: e.OrderID, OrderRevision: e.OrderRevision, OrderHash: e.OrderHash}
	h := &openAccount{s: s, cmd: cmd, order: o, currency: b.Currency}
	_, err = s.run(ctx, action.Command{Name: api.CommandOpenOrderAccount, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID}, cmd,
		cmd.Validate(), prepared{h})
	if rej, ok := kernel.AsRejection(err); ok && !retried(rej.Code) {
		return "", &AccountMismatch{Code: FailureAccountNotOpenable, Detail: fmt.Sprintf("order_account.open of order %s revision %d: %s",
			e.OrderID, e.OrderRevision, rej.Error())}
	}
	if err != nil {
		return "", err
	}
	return EffectAccountOpened, nil
}

// retried reports whether a rejection of order_account.open can pass on a later delivery: the
// consumer's actor not provisioned yet, a concurrent change, or a POL revision that changed since it
// was read before the transaction.
func retried(code string) bool {
	switch code {
	case kernel.CodePermissionDenied, kernel.CodeVersionConflict, api.CodeReferenceStale:
		return true
	}
	return false
}

// prepared is a command handler whose upstream reads were made before the transaction it runs in:
// it hides the handler's Prepare from the ledger, so nothing reads another context inside STL's
// transaction.
type prepared struct{ h action.Handler }

func (p prepared) Lock(ctx context.Context, exec bentopg.Executor) error { return p.h.Lock(ctx, exec) }

func (p prepared) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return p.h.Apply(ctx, exec, d)
}
