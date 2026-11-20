package application

import (
	"context"
	"fmt"
	"time"

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
//   - stl.order_credit moves the order's credit to the new revision (S5R-D08). When TRD's apply
//     asked CreditGuard for it (the event names the reservation, credit_reservation_id), that
//     reservation is consumed at the amendment's applied_at; reserving the new revision already
//     replaced the earlier one in the customer's exposure (D-4 counts an order's latest revision).
//     When the event names no reservation — the new revision asks no more credit (not_increased)
//     or none at all (not_required) — the new revision's own reservation is consumed if one was
//     made, else the earlier revision's reservation is carried over at the new revision's credit
//     portion (carry.go), so the exposure falls with the order. Either way every reservation the
//     order holds for an earlier revision is released with the cause superseded, naming the event.
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

// supersededReason is the reason credit.release records for a reservation an amendment superseded:
// by reservation by, or ("") by the amended revision itself (it needs no credit, or an earlier
// reservation is carried over in its place).
func supersededReason(e OrderEvent, by string) string {
	what := "superseded by reservation " + by
	if by == "" {
		what = "superseded by the amended revision"
	}
	return fmt.Sprintf("Order %s amended to revision %d: %s (%s %s at order version %d)", e.OrderID, e.OrderRevision, what, e.EventType, e.EventID,
		e.OrderVersion)
}

// PrepareCredit reads, outside any transaction, what OrderAmended e's credit needs when e names no
// reservation (S5R-D08): TRD's order now and the POL revisions of the order's held reservations
// (CreditBasis). An amendment that names its reservation needs nothing. A failure is retried with the
// event.
func (s Service) PrepareCredit(ctx context.Context, e OrderEvent) (CreditBasis, error) {
	if e.CreditReservationID != "" {
		return CreditBasis{}, nil
	}
	return s.creditBasis(ctx, e.OrderID)
}

// releaseSuperseded releases, as the system and with the cause superseded, each reservation of rs
// that is still held (S5R-D08), naming by; it returns their IDs.
func (s Service) releaseSuperseded(ctx context.Context, e OrderEvent, by string, rs []cc.Reservation) ([]string, error) {
	var out []string
	for _, x := range rs {
		if x.Status == cc.ReservationReleased {
			continue
		}
		if _, err := s.ReleaseCredit(ctx, api.ReleaseCredit{IdempotencyKey: CommandKey(e.EventID, x.ID), ReservationID: x.ID,
			ExpectedVersion: x.Version, Reason: supersededReason(e, by), Cause: cc.CauseSuperseded}); err != nil {
			return out, err
		}
		out = append(out, x.ID)
	}
	return out, nil
}

// What an OrderAmended did to the order's reservations when it named none (S5R-D08).
const (
	// EffectCarriedOver: the earlier revision's reservation is carried over to the amended revision at
	// its credit portion and released as superseded.
	EffectCarriedOver = "carried_over"
	// EffectNotRequired: the amended revision has nothing due on credit; the earlier revisions'
	// reservations are released as superseded.
	EffectNotRequired = "not_required"
	// EffectOrderMoved: TRD's order is no longer active at the amended revision (amended again or
	// ended since); the order's later event decides.
	EffectOrderMoved = "order_moved"
)

// ConsumeOnAmendment applies TRD's OrderAmended e to the order's credit reservations under the credit
// roots' lock, so that the amended revision holds exactly one consumed reservation of its credit
// portion and the customer's exposure follows the order (S5R-D08, STL-INV-004; D-9, WF-06).
//
// When e names the reservation that covered the new revision at apply (its need grew), that
// reservation is consumed with the amendment's applied_at under the event ID as idempotency key (or
// found consumed already), and every reservation the order still holds for an earlier revision is
// released as superseded. A named reservation this consumer released since (the order ended, or a
// later amendment superseded it, before this event arrived) leaves the event without effect; one a
// person released, or one that cannot be consumed for e, is a *CreditMismatch.
//
// When e names none (not_increased, not_required), b — read before the transaction — gives the new
// revision's payment stages:
//   - a reservation of a later revision already consumed (the order's later amendment came first), or
//     TRD's order no longer active at e's revision: nothing moves (EffectSuperseded, EffectOrderMoved);
//   - the new revision's own reservation, reserved although apply asked none (a stray one, or one made
//     before apply), is consumed when it holds exactly the revision's credit portion and was still
//     valid when the amendment took effect (or later, when it was reserved); otherwise it is released
//     as superseded;
//   - else the latest earlier revision's reservation is carried over at the new revision's credit
//     portion, no larger (credit.consume with carried_from, EffectCarriedOver), or — nothing due on
//     credit any more — nothing is carried (EffectNotRequired);
//   - the earlier revisions' reservations are released as superseded.
func (s Service) ConsumeOnAmendment(ctx context.Context, exec bentopg.Executor, e OrderEvent, b CreditBasis) (CreditEffect, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return CreditEffect{}, err
	}
	held, err := s.lockHeld(ctx, exec, e.OrderID)
	if err != nil {
		return CreditEffect{}, err
	}
	if e.CreditReservationID == "" {
		return s.followAmendment(ctx, e, b, held)
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
	out.Released, err = s.releaseSuperseded(ctx, e, r.ID, earlier(held, e.OrderRevision))
	return out, err
}

// earlier returns the reservations of held for an order revision before revision.
func earlier(held []cc.Reservation, revision int64) []cc.Reservation {
	var out []cc.Reservation
	for _, x := range held {
		if x.Binding.OrderRevision < revision {
			out = append(out, x)
		}
	}
	return out
}

// followAmendment is ConsumeOnAmendment for an amendment that named no reservation (S5R-D08), with the
// order's held reservations read under the credit roots' lock.
func (s Service) followAmendment(ctx context.Context, e OrderEvent, b CreditBasis, held []cc.Reservation) (CreditEffect, error) {
	var current, stray, from *cc.Reservation
	for i := range held {
		x := &held[i]
		switch {
		case x.Binding.OrderRevision > e.OrderRevision && x.Status == cc.ReservationConsumed:
			return CreditEffect{Outcome: EffectSuperseded}, nil
		case x.Binding.OrderRevision == e.OrderRevision && x.Status == cc.ReservationConsumed:
			current = x
		case x.Binding.OrderRevision == e.OrderRevision:
			stray = x
		case x.Binding.OrderRevision < e.OrderRevision && (from == nil || x.Binding.OrderRevision > from.Binding.OrderRevision):
			from = x
		}
	}
	prior := earlier(held, e.OrderRevision)
	if current != nil {
		released, err := s.releaseSuperseded(ctx, e, current.ID, prior)
		return CreditEffect{Outcome: EffectAlreadyConsumed, ReservationIDs: []string{current.ID}, Released: released}, err
	}
	if stray == nil && from == nil {
		return CreditEffect{Outcome: EffectNoReservation}, nil
	}
	binding := cc.Binding{OrderID: e.OrderID, OrderRevision: e.OrderRevision, OrderHash: e.OrderHash}
	now := kernel.Instant(s.Clock.Now())
	out := CreditEffect{}
	if stray != nil {
		consume, err := s.strayFits(*stray, e, b, binding, from != nil, now)
		if err != nil {
			return CreditEffect{}, err
		}
		if consume {
			at := e.AppliedAt
			if at.Before(stray.Reserved.At) {
				at = stray.Reserved.At // reserved after the amendment took effect: it holds credit from then on
			}
			if _, err = s.ConsumeCredit(ctx, api.ConsumeCredit{IdempotencyKey: CommandKey(e.EventID, ""), ReservationID: stray.ID, OrderID: e.OrderID,
				OrderRevision: e.OrderRevision, OrderHash: e.OrderHash, ActivatedAt: kernel.FormatTimestamp(at)}); err != nil {
				return CreditEffect{}, err
			}
			out.Released, err = s.releaseSuperseded(ctx, e, stray.ID, prior)
			return CreditEffect{Outcome: EffectConsumed, ReservationIDs: []string{stray.ID}, Released: out.Released}, err
		}
		released, err := s.releaseSuperseded(ctx, e, "", []cc.Reservation{*stray})
		if err != nil {
			return CreditEffect{}, err
		}
		out.Released = released
		if from == nil {
			out.Outcome = EffectNoReservation
			return out, nil
		}
	}
	portion, secured, _, known, err := b.need(from, binding)
	if err != nil {
		return CreditEffect{}, err
	}
	if !known {
		if err = orderMoved(e, b); err != nil {
			return CreditEffect{}, err
		}
		out.Outcome = EffectOrderMoved
		return out, nil
	}
	if portion.Sign() == 0 && secured.Sign() == 0 {
		released, err := s.releaseSuperseded(ctx, e, "", prior)
		out.Outcome, out.Released = EffectNotRequired, append(out.Released, released...)
		return out, err
	}
	if x, _ := portion.Cmp(from.Amount); x > 0 {
		return CreditEffect{}, &CreditMismatch{Code: FailureReservationNotConsumable, Detail: fmt.Sprintf(
			"order %s revision %d needs %s on credit, more than reservation %s holds, and TRD's apply asked for no reservation (%s)",
			e.OrderID, e.OrderRevision, portion, from.ID, e.CreditGuard)}
	}
	id := CarriedID(e.EventID)
	cmd := api.ConsumeCredit{IdempotencyKey: CommandKey(e.EventID, ""), ReservationID: id, OrderID: e.OrderID, OrderRevision: e.OrderRevision,
		OrderHash: e.OrderHash, ActivatedAt: kernel.FormatTimestamp(e.AppliedAt), CarriedFrom: from.ID}
	h := &carryCredit{s: s, cmd: cmd, basis: b}
	res, err := s.run(ctx, action.Command{Name: api.CommandConsume, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ReservationID}, cmd,
		cmd.Validate(), prepared{h})
	if err != nil {
		return CreditEffect{}, err
	}
	released, err := s.releaseSuperseded(ctx, e, res.ObjectID, prior)
	out.Outcome, out.ReservationIDs, out.Released = EffectCarriedOver, []string{res.ObjectID}, append(out.Released, released...)
	return out, err
}

// strayFits reports whether the amended revision's own reserved reservation r is consumed for e: it is
// for e's order hash, still valid when the amendment took effect (or when it was reserved, if later),
// and — when an earlier reservation could be carried over instead (carryable) — holds exactly the
// credit portion the revision needs under r's policy.
func (s Service) strayFits(r cc.Reservation, e OrderEvent, b CreditBasis, binding cc.Binding, carryable bool, now time.Time) (bool, error) {
	if e.AppliedAt.After(now) {
		return false, fmt.Errorf("stl: order %s was amended at %s, after this consumer's clock %s", e.OrderID,
			kernel.FormatTimestamp(e.AppliedAt), kernel.FormatTimestamp(now))
	}
	at := e.AppliedAt
	if at.Before(r.Reserved.At) {
		at = r.Reserved.At
	}
	if r.Binding != binding || !at.Before(r.ExpiresAt) || at.After(now) {
		return false, nil
	}
	if !carryable {
		return true, nil
	}
	portion, secured, _, known, err := b.need(&r, binding)
	if err != nil || !known {
		return !known && err == nil, err
	}
	return portion.Equal(r.Amount) && secured.Equal(r.Secured), nil
}

// orderMoved checks an amendment whose revision TRD no longer shows active (amended again, or ended,
// since): nothing moves, the order's later event decides (nil). TRD showing no such order, or a
// revision that is not intact, is a *CreditMismatch.
func orderMoved(e OrderEvent, b CreditBasis) error {
	o := b.Order
	switch {
	case o == nil:
		return &CreditMismatch{Code: FailureReservationNotConsumable, Detail: fmt.Sprintf("TRD shows no order %s", e.OrderID)}
	case o.Code != "":
		return &CreditMismatch{Code: FailureReservationNotConsumable, Detail: fmt.Sprintf("TRD's order %s revision %d is not intact (%s)",
			e.OrderID, o.Binding.OrderRevision, o.Code)}
	}
	return nil
}
