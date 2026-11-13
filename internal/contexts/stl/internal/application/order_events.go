package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What TRD's order events do to an order's credit reservations (WBS S5-12c, unit B6; event storming
// PL-12 and PL-13; WF-08 step 5; SM-STL-CREDIT-RESERVATION). The Inbox consumer in adapters/events
// calls these inside its transaction on STL's own transaction owner, as its system actor: they
// decide on STL's own rows under the customer's credit root lock and change them only through
// credit.consume and credit.release on the command ledger, which joins that transaction, so the
// reservation, the action result, the audit entry, the Outbox event and the inbox record commit
// together or not at all. Nothing here reads TRD: the event carries the order binding.
//
// A command the ledger rejects marks the joined transaction for rollback, so every decision is
// made before a command runs and a command's error is always the event's error.

// OrderEvent is one of TRD's SalesOrder events as far as customer credit reads it (Context Map R32):
// identifiers and versions only.
type OrderEvent struct {
	EventID       string // the envelope's ID: the Inbox deduplicates by it and the commands' keys derive from it
	EventType     string
	OrderID       string
	OrderRevision int64
	OrderHash     string
	OrderVersion  int64     // the order's row version after the command that published the event
	ActivatedAt   time.Time // OrderActivated only: the instant TRD activated the order
	// OrderAmended only: the instant TRD applied the amendment, what its CreditGuard call recorded for
	// the new revision (covered, documents_secured, not_increased, not_required) and the reservation
	// that covered it ("" when none was asked).
	AppliedAt           time.Time
	CreditGuard         string
	CreditReservationID string
}

// What an order event did to the order's reservations.
const (
	// EffectConsumed: the reservation of the activated order revision is consumed now.
	EffectConsumed = "consumed"
	// EffectAlreadyConsumed: it was consumed before, for this order binding.
	EffectAlreadyConsumed = "already_consumed"
	// EffectNoReservation: STL never reserved credit for the activated order revision. TRD-INV-022
	// asks for none when every payment is due before shipment; the consumer does not make one up.
	EffectNoReservation = "no_reservation"
	// EffectSuperseded: this consumer released the revision's reservation for a later event of the
	// order, its end, before this activation arrived (a dead-letter replay): nothing is consumed.
	EffectSuperseded = "superseded"
	// EffectReleased: every reservation the ended order still held is released now.
	EffectReleased = "released"
	// EffectNothingHeld: the ended order held no reservation.
	EffectNothingHeld = "nothing_held"
)

// CreditEffect is the outcome of one order event and the reservations it changed; Released are the
// earlier revisions' reservations an OrderAmended released.
type CreditEffect struct {
	Outcome        string
	ReservationIDs []string
	Released       []string
}

// Failure codes of an activation STL cannot honour, as the Inbox and the dead letter record them.
const (
	// FailureReservationReleased: the activated order revision's reservation was released by a
	// person or an agent and nothing replaced it — the order is active without the credit hold
	// TRD-INV-022 requires (a release that passed its check of TRD's order before the activation
	// committed, or an activation not yet guarded by CreditGuard).
	FailureReservationReleased = "stl.credit_reservation_released"
	// FailureReservationNotConsumable: the revision's reservation is reserved but SM-STL-CREDIT-
	// RESERVATION forbids consuming it for this activation: another order hash, expired when the
	// order was activated, or reserved only after it.
	FailureReservationNotConsumable = "stl.credit_reservation_not_consumable"
)

// CreditMismatch is an order event that contradicts STL's reservations for good: no retry and no
// later event repairs it, so the consumer refuses the event and the relay dead-letters it with an
// alert (ADR-BR-015). Nothing is changed.
type CreditMismatch struct {
	Code   string
	Detail string
}

func (m *CreditMismatch) Error() string { return "stl: " + m.Code + ": " + m.Detail }

// CommandKey is the idempotency key of the command an order event causes: the event ID itself for
// its one credit.consume (reservationID ""), and the event ID with a digest of the reservation ID
// for each of its credit.release commands, one per held reservation. An event ID that does not fit
// the identifier rule that way gives the digest of both instead. The key is a second line behind
// the Inbox, which deduplicates by event ID in the same transaction.
func CommandKey(eventID, reservationID string) string {
	key := eventID
	if reservationID != "" {
		sum := sha256.Sum256([]byte(reservationID))
		key += ":" + hex.EncodeToString(sum[:8])
	}
	if kernel.ValidIdentifier(key) {
		return key
	}
	sum := sha256.Sum256([]byte(eventID + "\x00" + reservationID))
	return hex.EncodeToString(sum[:])
}

// lockHeld locks FOR UPDATE the credit root of every customer that holds a reservation of the order,
// in the order of their IDs (tactical lockOrder: own roots first), and returns the order's reserved
// and consumed reservations as read under those locks. An order without one locks nothing.
func (s Service) lockHeld(ctx context.Context, exec bentopg.Executor, orderID string) ([]cc.Reservation, error) {
	sc := scope(ctx)
	seen, err := postgres.LoadHeldReservations(ctx, exec, sc, orderID)
	if err != nil || len(seen) == 0 {
		return nil, err
	}
	var parties []string
	for _, r := range seen {
		parties = append(parties, r.PartyID)
	}
	slices.Sort(parties)
	parties = slices.Compact(parties)
	for _, p := range parties {
		if _, err = postgres.LoadRoot(ctx, exec, sc, p, true); err != nil {
			return nil, err
		}
	}
	held, err := postgres.LoadHeldReservations(ctx, exec, sc, orderID)
	if err != nil {
		return nil, err
	}
	for _, r := range held {
		if !slices.Contains(parties, r.PartyID) {
			// Reserved for another customer between the two reads: the next delivery locks it too.
			return nil, fmt.Errorf("stl: order %s gained a reservation of customer %s while its credit roots were locked", orderID, r.PartyID)
		}
	}
	return held, nil
}

// consumable reports whether reserved reservation r may be consumed for activation e, judged now
// by actor: nil, a *CreditMismatch when the state machine forbids it for good, or an error to retry
// when the activation instant is still ahead of this process's clock.
func consumable(r cc.Reservation, e OrderEvent, actor kernel.ActorRef, now time.Time) error {
	if e.ActivatedAt.After(now) {
		return fmt.Errorf("stl: order %s was activated at %s, after this consumer's clock %s", e.OrderID,
			kernel.FormatTimestamp(e.ActivatedAt), kernel.FormatTimestamp(now))
	}
	err := r.Consume(cc.Binding{OrderID: e.OrderID, OrderRevision: e.OrderRevision, OrderHash: e.OrderHash}, e.ActivatedAt,
		cc.Act{By: actor, At: now})
	if rej, ok := kernel.AsRejection(err); ok {
		return &CreditMismatch{Code: FailureReservationNotConsumable, Detail: fmt.Sprintf("reservation %s of order %s revision %d: %s (%s)",
			r.ID, e.OrderID, e.OrderRevision, rej.Code, rejectionReason(rej))}
	}
	return err
}

func rejectionReason(rej *kernel.Rejection) string {
	if len(rej.Fields) == 0 {
		return ""
	}
	return rej.Fields[0].Path + ": " + rej.Fields[0].Reason
}

// unheld is the outcome of activation e when its order revision holds no reservation, from the
// revision's released reservations: none at all means STL never reserved for it; one this consumer
// released itself means it applied the order's end already; otherwise people released what the
// activation needed.
func unheld(released []cc.Reservation, e OrderEvent, consumer kernel.ActorRef) (CreditEffect, error) {
	if len(released) == 0 {
		return CreditEffect{Outcome: EffectNoReservation}, nil
	}
	for _, r := range released {
		if r.Released != nil && r.Released.By == consumer {
			return CreditEffect{Outcome: EffectSuperseded}, nil
		}
	}
	r := released[0]
	by := kernel.ActorRef{}
	if r.Released != nil {
		by = r.Released.By
	}
	return CreditEffect{}, &CreditMismatch{Code: FailureReservationReleased, Detail: fmt.Sprintf(
		"order %s is active at revision %d, but its reservation %s was released by %s:%s and no other holds its credit",
		e.OrderID, e.OrderRevision, r.ID, by.Kind, by.ID)}
}

// ConsumeOnActivation applies TRD's OrderActivated (PL-12): the reservation of exactly the activated
// order revision, reserved for the order hash and not expired when the order was activated, is
// consumed by credit.consume with the activation instant, under the event ID as idempotency key.
// A revision without any reservation is left as it is (EffectNoReservation); a reservation consumed
// before, or released by this consumer for the order's end, is left too. A reservation that cannot
// be consumed, or that people released, is a *CreditMismatch.
func (s Service) ConsumeOnActivation(ctx context.Context, exec bentopg.Executor, e OrderEvent) (CreditEffect, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return CreditEffect{}, err
	}
	held, err := s.lockHeld(ctx, exec, e.OrderID)
	if err != nil {
		return CreditEffect{}, err
	}
	i := slices.IndexFunc(held, func(r cc.Reservation) bool { return r.Binding.OrderRevision == e.OrderRevision })
	if i < 0 {
		released, err := postgres.LoadReleasedReservations(ctx, exec, scope(ctx), e.OrderID, e.OrderRevision)
		if err != nil {
			return CreditEffect{}, err
		}
		return unheld(released, e, p.Actor)
	}
	r := held[i]
	if r.Status == cc.ReservationConsumed {
		if r.Binding.OrderHash != e.OrderHash {
			return CreditEffect{}, &CreditMismatch{Code: FailureReservationNotConsumable, Detail: fmt.Sprintf(
				"reservation %s of order %s revision %d is consumed for another order hash", r.ID, e.OrderID, e.OrderRevision)}
		}
		return CreditEffect{Outcome: EffectAlreadyConsumed, ReservationIDs: []string{r.ID}}, nil
	}
	if err = consumable(r, e, p.Actor, kernel.Instant(s.Clock.Now())); err != nil {
		return CreditEffect{}, err
	}
	_, err = s.ConsumeCredit(ctx, api.ConsumeCredit{IdempotencyKey: CommandKey(e.EventID, ""), ReservationID: r.ID, OrderID: e.OrderID,
		OrderRevision: e.OrderRevision, OrderHash: e.OrderHash, ActivatedAt: kernel.FormatTimestamp(e.ActivatedAt)})
	if err != nil {
		return CreditEffect{}, err
	}
	return CreditEffect{Outcome: EffectConsumed, ReservationIDs: []string{r.ID}}, nil
}

// endReason is the reason credit.release records for an order's end.
func endReason(e OrderEvent) string {
	return fmt.Sprintf("Order %s ended: %s %s at order version %d", e.OrderID, e.EventType, e.EventID, e.OrderVersion)
}

// ReleaseOnOrderEnd applies the end of an order — TRD's OrderCancelled, OrderClosedUnfulfilled,
// OrderRemainderClosed or OrderClosed (PL-13): every reservation the order still holds, consumed or
// reserved and of whichever order revision, is released by credit.release as the system, naming the
// event. An order that holds none is left as it is.
func (s Service) ReleaseOnOrderEnd(ctx context.Context, exec bentopg.Executor, e OrderEvent) (CreditEffect, error) {
	held, err := s.lockHeld(ctx, exec, e.OrderID)
	if err != nil {
		return CreditEffect{}, err
	}
	out := CreditEffect{Outcome: EffectNothingHeld}
	for _, r := range held {
		if _, err = s.ReleaseCredit(ctx, api.ReleaseCredit{IdempotencyKey: CommandKey(e.EventID, r.ID), ReservationID: r.ID,
			ExpectedVersion: r.Version, Reason: endReason(e)}); err != nil {
			return CreditEffect{}, err
		}
		out.Outcome, out.ReservationIDs = EffectReleased, append(out.ReservationIDs, r.ID)
	}
	return out, nil
}
