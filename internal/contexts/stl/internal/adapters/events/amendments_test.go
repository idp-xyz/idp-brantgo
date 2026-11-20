package events

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/application"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Unit C4b-STL (WBS S5-09b; PL-09, WF-06, D-9, D-21): TRD's OrderAmended reaches both consumers.

func amended(fields map[string]any) messaging.Event {
	f := map[string]any{"order_revision": 2, "order_hash": strings.Repeat("c", 64), "from_order_revision": 1, "from_order_hash": orderHashA,
		"applied_at": "2026-11-12T09:00:00.5Z", "credit_guard": "covered", "credit_reservation_id": "res.2", "aggregate_version": 9}
	for k, v := range fields {
		f[k] = v
	}
	return orderEnvelope(trdapi.EventOrderAmended, f)
}

// prepared runs the subscription's Prepare step on e, as the dispatcher does before the handler.
func prepared(t *testing.T, sub messaging.Subscription, e messaging.Event) messaging.Event {
	t.Helper()
	basis, err := sub.Prepare(context.Background(), e.Envelope)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	e.Prepared = basis
	return e
}

// stl.order_credit hands ConsumeOnAmendment the new binding, the apply instant, the guard's value
// and the reservation the event names (none for not_increased / not_required), with the credit basis
// its Prepare step read (S5R-D08: PrepareCredit; the other order events prepare nothing).
func TestOrderAmendedReachesTheCreditConsumer(t *testing.T) {
	c := &credit{}
	sub := Subscriptions(nil, c)[0]
	h := sub.Handler
	if err := h(context.Background(), prepared(t, sub, amended(nil))); err != nil {
		t.Fatal(err)
	}
	if len(c.creditPrepared) != 1 || len(c.creditBases) != 1 || c.creditBases[0].Order.Binding.OrderRevision != 7 {
		t.Fatalf("prepared %+v %+v", c.creditPrepared, c.creditBases)
	}
	if err := h(context.Background(), amended(nil)); err == nil || errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("an amendment without its prepared basis is retried: %v", err)
	}
	if basis, err := sub.Prepare(context.Background(), activated(nil).Envelope); basis != nil || err != nil || len(c.creditPrepared) != 1 {
		t.Fatalf("an activation prepares nothing: %v %v", basis, err)
	}
	if len(c.amended) != 1 || len(c.consumed) != 0 || len(c.released) != 0 {
		t.Fatalf("routed %+v %+v %+v", c.amended, c.consumed, c.released)
	}
	got := c.amended[0]
	if got.EventType != trdapi.EventOrderAmended || got.OrderRevision != 2 || got.OrderHash != strings.Repeat("c", 64) || got.OrderVersion != 9 ||
		!got.AppliedAt.Equal(time.Date(2026, 11, 12, 9, 0, 0, 500000000, time.UTC)) || got.CreditGuard != "covered" ||
		got.CreditReservationID != "res.2" || !got.ActivatedAt.IsZero() {
		t.Fatalf("event %+v", got)
	}
	if err := h(context.Background(), prepared(t, sub, amended(map[string]any{"credit_guard": "not_increased", "credit_reservation_id": ""}))); err != nil ||
		c.amended[1].CreditReservationID != "" || c.amended[1].CreditGuard != "not_increased" {
		t.Fatalf("not increased: %+v %v", c.amended, err)
	}
	for name, e := range map[string]messaging.Event{
		"no apply instant":          amended(map[string]any{"applied_at": nil}),
		"local apply instant":       amended(map[string]any{"applied_at": "2026-11-12T17:00:00+08:00"}),
		"reservation not an id":     amended(map[string]any{"credit_reservation_id": "res 2"}),
		"another aggregate amended": amended(map[string]any{"aggregate_type": "QuoteRoot"}),
	} {
		if err := h(context.Background(), e); !errors.Is(err, messaging.ErrEventRejected) || !strings.Contains(err.Error(), string(FailureOrderEventInvalid)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(c.amended) != 2 {
		t.Fatalf("the application saw a malformed amendment: %+v", c.amended)
	}
	if _, err := sub.Prepare(context.Background(), amended(map[string]any{"applied_at": "yesterday"}).Envelope); !errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("malformed amendment in prepare: %v", err)
	}
	ready := amended(nil)
	ready.Prepared = application.CreditBasis{}
	mismatch := &credit{err: &application.CreditMismatch{Code: application.FailureReservationReleased, Detail: "res.2"}}
	if err := OrderCreditHandler(mismatch)(context.Background(), ready); !errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("mismatch: %v", err)
	}
	denied := kernel.Reject(kernel.CodePermissionDenied, "/", "")
	if err := OrderCreditHandler(&credit{err: denied})(context.Background(), ready); !errors.Is(err, denied) ||
		errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("denied: %v", err)
	}
	cause := errors.New("TRD unreachable")
	if _, err := Subscriptions(nil, &credit{prepareErr: cause})[0].Prepare(context.Background(), amended(nil).Envelope); !errors.Is(err, cause) {
		t.Fatalf("a failed read is retried: %v", err)
	}
}

// stl.order_account reads TRD and POL for the review in Prepare (PrepareReview, not PrepareAccount)
// and opens the review in the handler (OpenReviewOnAmendment); a review STL cannot open for good
// refuses the event under its code; other failures are retried.
func TestOrderAmendedReachesTheAccountConsumer(t *testing.T) {
	c := &credit{}
	sub := Subscriptions(nil, c)[1]
	e := amended(nil)
	basis, err := sub.Prepare(context.Background(), e.Envelope)
	if err != nil || len(c.reviewPrepared) != 1 || len(c.prepared) != 0 || c.reviewPrepared[0].OrderRevision != 2 {
		t.Fatalf("prepare %+v %+v, %v", c.reviewPrepared, c.prepared, err)
	}
	e.Prepared = basis
	if err = sub.Handler(context.Background(), e); err != nil || len(c.reviewed) != 1 || len(c.opened) != 0 || c.reviewed[0] != c.reviewPrepared[0] ||
		c.bases[0].Order.Binding.OrderRevision != 9 {
		t.Fatalf("handler %+v %+v, %v", c.reviewed, c.opened, err)
	}
	refused := &credit{err: &application.AccountMismatch{Code: application.FailureReviewNotOpenable, Detail: "order.1"}}
	if err = Subscriptions(nil, refused)[1].Handler(context.Background(), e); !errors.Is(err, messaging.ErrEventRejected) ||
		!strings.Contains(err.Error(), "("+application.FailureReviewNotOpenable+")") {
		t.Fatalf("review not openable: %v", err)
	}
	cause := errors.New("connection reset")
	if err = Subscriptions(nil, &credit{err: cause})[1].Handler(context.Background(), e); !errors.Is(err, cause) {
		t.Fatalf("retry: %v", err)
	}
	if _, err = sub.Prepare(context.Background(), amended(map[string]any{"applied_at": "yesterday"}).Envelope); !errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("malformed amendment in prepare: %v", err)
	}
}
