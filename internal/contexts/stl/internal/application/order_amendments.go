package application

import (
	"context"
	"fmt"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What TRD's OrderAmended does in STL (unit C4b-STL, WBS S5-09b; event storming PL-09; WF-06 step 6;
// S5 plan D-4, D-9, D-18, D-21). Two consumers handle it, each in its own transaction (D-3: one
// modified aggregate per transaction), as for OrderActivated:
//
//   - stl.order_account opens the payment plan review: the order account moves to TRD's current
//     revision of the amended order with its payment stages (payment_plan_review.open as the
//     consumer's system actor, under the event ID as idempotency key). TRD and POL are read in the
//     subscription's Prepare step; the transaction locks the account FOR UPDATE.
//   - stl.order_credit moves the order's credit to the new revision when TRD's apply asked
//     CreditGuard for it (the event names the reservation, credit_reservation_id): that reservation
//     is consumed at the amendment's applied_at, and every reservation the order holds for an
//     earlier revision is released, naming the event. Reserving the new revision already replaced
//     the earlier one in the customer's exposure (D-4 counts an order's latest revision), so the
//     exposure does not change here; the release ends the earlier hold for good. When the event
//     names no reservation — the new revision asks no more credit (not_increased) or none at all
//     (not_required) — the revision stands on what the order holds and nothing moves: a smaller
//     need keeps the larger earlier reservation until the order ends (PL-13), and the review lists
//     the changed credit portion for treasury.
//
// Replays are harmless: the Inbox settles each event once; a review opened, an account at the event's
// revision or a later one, a reservation consumed or released by this consumer each leave a late or
// repeated delivery without effect.

// What an OrderAmended did to the order's account.
const (
	// EffectReviewOpened: the account moved to TRD's current revision (or was opened there) and the
	// payment plan review of that revision is open.
	EffectReviewOpened = "review_opened"
	// EffectAccountCurrent: the account holds the amended revision or a later one already.
	EffectAccountCurrent = "account_current"
)

// FailureReviewNotOpenable is the failure code of an OrderAmended whose review STL cannot open for
// good: TRD shows no such order, its revision is not intact, or payment_plan_review.open refused it
// in a way no retry changes.
const FailureReviewNotOpenable = "stl.payment_plan_review_not_openable"

// What an OrderAmended did to the order's reservations, besides EffectConsumed, EffectAlreadyConsumed
// and EffectSuperseded.
const (
	// EffectStandsOnEarlier: the amended revision asks no more credit than the one it amends, or
	// none; it stands on what the order holds, and nothing moves.
	EffectStandsOnEarlier = "stands_on_earlier"
)

// PrepareReview reads, outside any transaction, what OrderAmended e's review needs: TRD's order now
// and, when TRD shows it active and intact at e's revision or a later one, the POL revision of its
// currency. A failure is retried with the event.
func (s Service) PrepareReview(ctx context.Context, e OrderEvent) (AccountBasis, error) {
	orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{e.OrderID})
	if err != nil {
		return AccountBasis{}, err
	}
	o, ok := orders[e.OrderID]
	if !ok {
		return AccountBasis{}, nil
	}
	b := AccountBasis{Order: &o}
	if o.Code != "" || o.Status != OrderActive || o.Binding.OrderRevision < e.OrderRevision {
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

// OpenReviewOnAmendment applies TRD's OrderAmended e to the order's account with b, read before the
// transaction: unless the account holds e's revision or a later one, payment_plan_review.open moves
// it — or opens it, when the order has none — to TRD's current revision (e's, or a later one when TRD
// amended again since), under the event ID as idempotency key. An order that ended since is left
// as it is. An order TRD does not show intact, or a review the command refuses for good, is an
// *AccountMismatch.
func (s Service) OpenReviewOnAmendment(ctx context.Context, exec bentopg.Executor, e OrderEvent, b AccountBasis) (string, error) {
	a, err := postgres.LoadAccount(ctx, exec, scope(ctx), e.OrderID, true)
	if err != nil {
		return "", err
	}
	if a != nil && a.OrderRevision >= e.OrderRevision {
		return EffectAccountCurrent, nil
	}
	o := b.Order
	switch {
	case o == nil:
		return "", &AccountMismatch{Code: FailureReviewNotOpenable, Detail: fmt.Sprintf("TRD shows no order %s", e.OrderID)}
	case o.Code != "":
		return "", &AccountMismatch{Code: FailureReviewNotOpenable, Detail: fmt.Sprintf("TRD's order %s revision %d is not intact (%s)",
			e.OrderID, o.Binding.OrderRevision, o.Code)}
	case o.Status != OrderActive:
		return EffectOrderEnded, nil
	case o.Binding.OrderRevision < e.OrderRevision:
		return "", fmt.Errorf("stl: TRD shows order %s at revision %d, before the amended revision %d", e.OrderID, o.Binding.OrderRevision,
			e.OrderRevision)
	}
	cmd := api.OpenPaymentPlanReview{IdempotencyKey: CommandKey(e.EventID, ""), OrderID: e.OrderID, OrderRevision: o.Binding.OrderRevision,
		OrderHash: o.Binding.OrderHash}
	h := &openReview{s: s, cmd: cmd, order: o, currency: b.Currency}
	_, err = s.run(ctx, action.Command{Name: api.CommandOpenReview, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID}, cmd,
		cmd.Validate(), prepared{h})
	if rej, ok := kernel.AsRejection(err); ok && !retried(rej.Code) {
		return "", &AccountMismatch{Code: FailureReviewNotOpenable, Detail: fmt.Sprintf("payment_plan_review.open of order %s revision %d: %s",
			e.OrderID, cmd.OrderRevision, rej.Error())}
	}
	if err != nil {
		return "", err
	}
	return EffectReviewOpened, nil
}

// supersededReason is the reason credit.release records for a reservation an amendment superseded.
func supersededReason(e OrderEvent, by string) string {
	return fmt.Sprintf("Order %s amended to revision %d: superseded by reservation %s (%s %s at order version %d)", e.OrderID, e.OrderRevision, by,
		e.EventType, e.EventID, e.OrderVersion)
}

// ConsumeOnAmendment applies TRD's OrderAmended e to the order's credit reservations (D-9, WF-06
// "信用部分增加时须新的占用"): when e names the reservation that covered the new revision at apply,
// that reservation is consumed with the amendment's applied_at under the event ID as idempotency
// key (or found consumed already), and every reservation the order still holds for an earlier
// revision is released as the system. When e names none, nothing moves (EffectStandsOnEarlier). A
// named reservation this consumer released since (the order ended, or a later amendment superseded
// it, before this event arrived) leaves the event without effect; one a person released, or one
// that cannot be consumed for e, is a *CreditMismatch.
func (s Service) ConsumeOnAmendment(ctx context.Context, exec bentopg.Executor, e OrderEvent) (CreditEffect, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return CreditEffect{}, err
	}
	held, err := s.lockHeld(ctx, exec, e.OrderID)
	if err != nil {
		return CreditEffect{}, err
	}
	if e.CreditReservationID == "" {
		return CreditEffect{Outcome: EffectStandsOnEarlier}, nil
	}
	i := -1
	for j := range held {
		if held[j].ID == e.CreditReservationID {
			i = j
		}
	}
	if i < 0 {
		released, err := postgres.LoadReleasedReservations(ctx, exec, scope(ctx), e.OrderID, e.OrderRevision)
		if err != nil {
			return CreditEffect{}, err
		}
		for _, r := range released {
			if r.ID != e.CreditReservationID {
				continue
			}
			if r.Released != nil && r.Released.By == p.Actor {
				return CreditEffect{Outcome: EffectSuperseded}, nil
			}
			return unheld([]cc.Reservation{r}, e, p.Actor)
		}
		return CreditEffect{}, &CreditMismatch{Code: FailureReservationNotConsumable, Detail: fmt.Sprintf(
			"order %s revision %d names reservation %s, which STL does not hold for that revision", e.OrderID, e.OrderRevision, e.CreditReservationID)}
	}
	r := held[i]
	out := CreditEffect{Outcome: EffectAlreadyConsumed, ReservationIDs: []string{r.ID}}
	switch {
	case r.Binding.OrderRevision != e.OrderRevision:
		return CreditEffect{}, &CreditMismatch{Code: FailureReservationNotConsumable, Detail: fmt.Sprintf(
			"reservation %s is for order %s revision %d, not the amended revision %d", r.ID, e.OrderID, r.Binding.OrderRevision, e.OrderRevision)}
	case r.Status == cc.ReservationConsumed && r.Binding.OrderHash != e.OrderHash:
		return CreditEffect{}, &CreditMismatch{Code: FailureReservationNotConsumable, Detail: fmt.Sprintf(
			"reservation %s of order %s revision %d is consumed for another order hash", r.ID, e.OrderID, e.OrderRevision)}
	case r.Status != cc.ReservationConsumed:
		at := e
		at.ActivatedAt = e.AppliedAt
		if err = consumable(r, at, p.Actor, kernel.Instant(s.Clock.Now())); err != nil {
			return CreditEffect{}, err
		}
		if _, err = s.ConsumeCredit(ctx, api.ConsumeCredit{IdempotencyKey: CommandKey(e.EventID, ""), ReservationID: r.ID, OrderID: e.OrderID,
			OrderRevision: e.OrderRevision, OrderHash: e.OrderHash, ActivatedAt: kernel.FormatTimestamp(e.AppliedAt)}); err != nil {
			return CreditEffect{}, err
		}
		out.Outcome = EffectConsumed
	}
	for _, x := range held {
		if x.Binding.OrderRevision >= e.OrderRevision {
			continue
		}
		if _, err = s.ReleaseCredit(ctx, api.ReleaseCredit{IdempotencyKey: CommandKey(e.EventID, x.ID), ReservationID: x.ID,
			ExpectedVersion: x.Version, Reason: supersededReason(e, r.ID)}); err != nil {
			return CreditEffect{}, err
		}
		out.Released = append(out.Released, x.ID)
	}
	return out, nil
}
