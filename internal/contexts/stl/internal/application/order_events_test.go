package application

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Unit B6 (WBS S5-12c): what an order event may do to a reservation, decided before any command
// runs (PL-12, PL-13, SM-STL-CREDIT-RESERVATION, TRD-INV-022). Python V0.50 has no STL; the tests
// cite design IDs.

var (
	consumerActor = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.stl.order_credit"}
	salesClerk    = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "sales.1"}
	hashA         = strings.Repeat("a", 64)
	hashB         = strings.Repeat("b", 64)
)

func reservedAt(t0 time.Time) cc.Reservation {
	return cc.Reservation{ID: "res.1", PartyID: "customer.1", Binding: cc.Binding{OrderID: "order.1", OrderRevision: 1, OrderHash: hashA},
		Status: cc.ReservationReserved, Version: 1, ExpiresAt: t0.Add(30 * 24 * time.Hour), Reserved: cc.Act{By: salesClerk, Epoch: 1, At: t0}}
}

func activation(at time.Time) OrderEvent {
	return OrderEvent{EventID: "evt-0190a1b2-c3d4-7e5f-8a6b-0c1d2e3f4a5b", EventType: "idp-brant.trd.order_activated.v1", OrderID: "order.1",
		OrderRevision: 1, OrderHash: hashA, OrderVersion: 4, ActivatedAt: at}
}

func mismatch(t *testing.T, err error, code string) {
	t.Helper()
	var m *CreditMismatch
	if !errors.As(err, &m) || m.Code != code || m.Detail == "" {
		t.Fatalf("want a mismatch %s, got %v", code, err)
	}
}

// One credit.consume per event carries the event ID as its key; each credit.release of an order's
// end carries the event ID and its reservation, so several releases of one event never share a key.
// Keys are identifiers, the same for the same event and reservation.
func TestCommandKeyDerivesFromTheEvent(t *testing.T) {
	const event = "evt-0190a1b2-c3d4-7e5f-8a6b-0c1d2e3f4a5b"
	if got := CommandKey(event, ""); got != event {
		t.Fatalf("consume key %q", got)
	}
	a, b := CommandKey(event, "res.a"), CommandKey(event, "res.b")
	if a == b || a != CommandKey(event, "res.a") || !strings.HasPrefix(a, event+":") || len(a) != len(event)+17 ||
		!kernel.ValidIdentifier(a) || !kernel.ValidIdentifier(b) {
		t.Fatalf("release keys %q %q", a, b)
	}
	// Event IDs are up to 128 characters of any kind (eventing.MaxEventIDLength); keys are identifiers.
	for _, id := range []string{strings.Repeat("e", 100), "evt/with spaces", strings.Repeat("e", 60)} {
		for _, res := range []string{"", "res.a", "res.b"} {
			k := CommandKey(id, res)
			if !kernel.ValidIdentifier(k) || k != CommandKey(id, res) {
				t.Errorf("key of %q %q: %q", id, res, k)
			}
		}
		if CommandKey(id, "res.a") == CommandKey(id, "res.b") || (len(id) > 64 && CommandKey(id, "") == CommandKey(id, "res.a")) {
			t.Errorf("keys of %q collide", id)
		}
	}
	if err := (api.ReleaseCredit{IdempotencyKey: a, ReservationID: "res.a", ExpectedVersion: 2, Reason: endReason(activation(time.Now()))}).Validate(); err != nil {
		t.Fatalf("the end's release command: %v", err)
	}
}

// SM-STL-CREDIT-RESERVATION reserved → consumed: 占用未过期，订单、修订与金额一致. The reservation of
// the activated revision is consumable when it is reserved for the order hash, was made before the
// activation and had not expired at the activation instant — judged at that instant, not now, so a
// late delivery still consumes. Anything else never becomes consumable (a mismatch); an activation
// instant ahead of the consumer's clock is retried.
func TestReservationConsumableForTheActivation(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	r := reservedAt(t0)
	now := t0.Add(48 * time.Hour)
	if err := consumable(r, activation(t0.Add(time.Hour)), consumerActor, now); err != nil {
		t.Fatal(err)
	}
	if r.Status != cc.ReservationReserved || r.Consumed != nil || r.Version != 1 {
		t.Fatalf("the check changed the reservation %+v", r)
	}
	// Delivered after the reservation's expiry, activated before it.
	if err := consumable(r, activation(t0.Add(time.Hour)), consumerActor, t0.Add(90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Activated at the instant of the reservation.
	if err := consumable(r, activation(t0), consumerActor, now); err != nil {
		t.Fatal(err)
	}

	other := activation(t0.Add(time.Hour))
	other.OrderHash = hashB
	mismatch(t, consumable(r, other, consumerActor, now), FailureReservationNotConsumable)
	mismatch(t, consumable(r, activation(r.ExpiresAt), consumerActor, r.ExpiresAt.Add(time.Hour)), FailureReservationNotConsumable)
	mismatch(t, consumable(r, activation(t0.Add(-time.Second)), consumerActor, now), FailureReservationNotConsumable)

	err := consumable(r, activation(now.Add(time.Second)), consumerActor, now)
	var m *CreditMismatch
	if err == nil || errors.As(err, &m) {
		t.Fatalf("an activation ahead of the clock is retried, got %v", err)
	}
}

// An activated order revision that holds no reservation: none ever made is TRD-INV-022's order paid
// before shipment (nothing is invented); one this consumer released is the order's end applied
// before a replayed activation (nothing is consumed); one released by people is the order active
// without its credit hold.
func TestActivationOfARevisionThatHoldsNoReservation(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	e := activation(t0.Add(time.Hour))
	released := func(id string, by kernel.ActorRef) cc.Reservation {
		r := reservedAt(t0)
		r.ID, r.Status, r.Version = id, cc.ReservationReleased, 2
		r.Released = &cc.Act{By: by, Epoch: 1, At: t0.Add(2 * time.Hour), Reason: "r"}
		return r
	}
	if got, err := unheld(nil, e, consumerActor); err != nil || got.Outcome != EffectNoReservation || len(got.ReservationIDs) != 0 {
		t.Fatalf("no reservation: %+v %v", got, err)
	}
	if got, err := unheld([]cc.Reservation{released("res.1", consumerActor)}, e, consumerActor); err != nil || got.Outcome != EffectSuperseded {
		t.Fatalf("released for the order's end: %+v %v", got, err)
	}
	// Abandoned once by the clerk, reserved again, then released for the order's end.
	if got, err := unheld([]cc.Reservation{released("res.2", salesClerk), released("res.1", consumerActor)}, e, consumerActor); err != nil ||
		got.Outcome != EffectSuperseded {
		t.Fatalf("released by both: %+v %v", got, err)
	}
	_, err := unheld([]cc.Reservation{released("res.1", salesClerk)}, e, consumerActor)
	mismatch(t, err, FailureReservationReleased)
	// Another system actor's release is not this consumer's: it does not prove the order ended.
	_, err = unheld([]cc.Reservation{released("res.1", kernel.ActorRef{Kind: kernel.ActorSystem, ID: "job.expiry"})}, e, consumerActor)
	mismatch(t, err, FailureReservationReleased)
}

// The release of an order's end names the event, its type and the order's version, within the
// contract's reason.
func TestEndReasonNamesTheEvent(t *testing.T) {
	e := OrderEvent{EventID: strings.Repeat("e", 128), EventType: "idp-brant.trd.order_remainder_closed.v1", OrderID: strings.Repeat("o", 64),
		OrderVersion: 9}
	reason := endReason(e)
	if !api.Reason(reason) || !strings.Contains(reason, e.EventID) || !strings.Contains(reason, e.EventType) || !strings.Contains(reason, "version 9") {
		t.Fatalf("reason %q", reason)
	}
}
