package events

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/application"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Unit B6 (WBS S5-12c): the Inbox consumer stl.order_credit (event storming PL-12, PL-13; WF-08 step
// 5; Context Map R32). Python V0.50 has no STL; the tests cite design IDs.

// credit records what the consumers ask of the application and answers err.
type credit struct {
	consumed, released []application.OrderEvent
	prepared, opened   []application.OrderEvent
	bases              []application.AccountBasis
	amended            []application.OrderEvent // ConsumeOnAmendment
	reviewPrepared     []application.OrderEvent // PrepareReview
	reviewed           []application.OrderEvent // OpenReviewOnAmendment
	err, prepareErr    error
}

func (c *credit) ConsumeOnAmendment(_ context.Context, _ bentopg.Executor, e application.OrderEvent) (application.CreditEffect, error) {
	c.amended = append(c.amended, e)
	return application.CreditEffect{Outcome: application.EffectConsumed}, c.err
}

func (c *credit) PrepareReview(_ context.Context, e application.OrderEvent) (application.AccountBasis, error) {
	c.reviewPrepared = append(c.reviewPrepared, e)
	return application.AccountBasis{Order: &application.Order{Binding: application.OrderBinding{OrderID: e.OrderID, OrderRevision: 9}}}, c.prepareErr
}

func (c *credit) OpenReviewOnAmendment(_ context.Context, _ bentopg.Executor, e application.OrderEvent, b application.AccountBasis) (string, error) {
	c.reviewed, c.bases = append(c.reviewed, e), append(c.bases, b)
	return application.EffectReviewOpened, c.err
}

func (c *credit) PrepareAccount(_ context.Context, e application.OrderEvent) (application.AccountBasis, error) {
	c.prepared = append(c.prepared, e)
	return application.AccountBasis{Order: &application.Order{Binding: application.OrderBinding{OrderID: e.OrderID}}}, c.prepareErr
}

func (c *credit) OpenOnActivation(_ context.Context, _ bentopg.Executor, e application.OrderEvent, b application.AccountBasis) (string, error) {
	c.opened, c.bases = append(c.opened, e), append(c.bases, b)
	return application.EffectAccountOpened, c.err
}

func (c *credit) ConsumeOnActivation(_ context.Context, _ bentopg.Executor, e application.OrderEvent) (application.CreditEffect, error) {
	c.consumed = append(c.consumed, e)
	return application.CreditEffect{Outcome: application.EffectConsumed}, c.err
}

func (c *credit) ReleaseOnOrderEnd(_ context.Context, _ bentopg.Executor, e application.OrderEvent) (application.CreditEffect, error) {
	c.released = append(c.released, e)
	return application.CreditEffect{Outcome: application.EffectReleased}, c.err
}

var orderHashA = strings.Repeat("a", 64)

// payload is a SalesOrder event's payload as TRD's command ledger writes it: the ledger's members,
// the order binding of every order event, and fields.
func payload(fields map[string]any) json.RawMessage {
	p := map[string]any{"tenant_id": "tenant.1", "company_id": "company.A", "aggregate_type": "SalesOrder", "aggregate_id": "order.1",
		"aggregate_version": 4, "actor_kind": "human", "actor_id": "author", "authority_epoch": 3, "action_id": "act-1",
		"order_id": "order.1", "quote_id": "quote.1", "business_revision": 1, "business_hash": strings.Repeat("b", 64), "order_revision": 1,
		"order_hash": orderHashA, "order_status": "active"}
	for k, v := range fields {
		if v == nil {
			delete(p, k)
		} else {
			p[k] = v
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return b
}

func orderEnvelope(typ eventing.EventType, fields map[string]any) messaging.Event {
	at := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	return messaging.Event{Consumer: OrderCredit, Envelope: eventing.Envelope{SpecVersion: eventing.SpecVersion,
		ID: "evt-0190a1b2-c3d4-7e5f-8a6b-0c1d2e3f4a5b", Source: trdapi.Source, Type: typ, Version: 1, Scope: "tenant.1/company.A",
		Subject: "order.1", PartitionKey: "tenant.1/company.A/SalesOrder:order.1", OccurredAt: at, RecordedAt: at,
		ContentType: eventing.JSONContentType, Payload: payload(fields)}}
}

func activated(fields map[string]any) messaging.Event {
	f := map[string]any{"activated_at": "2026-11-12T07:59:59.123456Z"}
	for k, v := range fields {
		f[k] = v
	}
	return orderEnvelope(trdapi.EventOrderActivated, f)
}

// STL declares one consumer, stl.order_credit, on TRD's source: OrderActivated (PL-12), the four
// ends of an order (PL-13; OrderClosedUnfulfilled ends a draft, whose reservation is still
// reserved) and OrderAmended (D-9: the new revision's reservation; unit C4b). Its system actor is
// system:inbox.stl.order_credit with credit.consume, credit.release and stl.read. The second
// consumer, stl.order_account (D-4, D-18, units B6b and C4b), takes OrderActivated and OrderAmended
// (PL-09, the payment plan review), reads TRD and POL in its Prepare step, and its actor
// system:inbox.stl.order_account holds order_account.open (S5R-D02), payment_plan_review.open and
// stl.read.
func TestOrderCreditSubscription(t *testing.T) {
	subs := Subscriptions(nil, &credit{})
	if len(subs) != 2 {
		t.Fatalf("%d subscriptions", len(subs))
	}
	s := subs[0]
	want := []eventing.EventType{trdapi.EventOrderActivated, trdapi.EventOrderCancelled, trdapi.EventOrderClosedUnfulfilled,
		trdapi.EventOrderRemainderClosed, trdapi.EventOrderClosed, trdapi.EventOrderAmended}
	if s.Consumer != "stl.order_credit" || s.Source != trdapi.Source || !slices.Equal(s.EventTypes, want) || s.Prepare != nil || s.Handler == nil {
		t.Fatalf("subscription %+v", s)
	}
	a := subs[1]
	if a.Consumer != "stl.order_account" || a.Source != trdapi.Source ||
		!slices.Equal(a.EventTypes, []eventing.EventType{trdapi.EventOrderActivated, trdapi.EventOrderAmended}) || a.Prepare == nil || a.Handler == nil {
		t.Fatalf("account subscription %+v", a)
	}
	grants, err := messaging.InboxGrants(subs)
	if err != nil || len(grants) != 2 || grants[0].Actor != (kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.stl.order_credit"}) ||
		!slices.Equal(grants[0].Permissions, []string{"credit.consume", "credit.release", "stl.read"}) ||
		grants[1].Actor != (kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.stl.order_account"}) ||
		!slices.Equal(grants[1].Permissions, []string{"order_account.open", "payment_plan_review.open", "stl.read"}) {
		t.Fatalf("grants %+v %v", grants, err)
	}
	// The declared types are not the handler's own slices: a caller cannot change what it routes.
	s.EventTypes[0] = trdapi.EventOrderReviewDecided
	if Subscriptions(nil, &credit{})[0].EventTypes[0] != trdapi.EventOrderActivated {
		t.Fatal("the subscription shares the handler's event types")
	}
}

// OrderActivated reaches ConsumeOnActivation with the order binding, the order's version and the
// activation instant of the payload; each end of an order reaches ReleaseOnOrderEnd.
func TestOrderEventsReachTheApplication(t *testing.T) {
	c := &credit{}
	h := OrderCreditHandler(c)
	if err := h(context.Background(), activated(nil)); err != nil {
		t.Fatal(err)
	}
	want := application.OrderEvent{EventID: "evt-0190a1b2-c3d4-7e5f-8a6b-0c1d2e3f4a5b", EventType: trdapi.EventOrderActivated, OrderID: "order.1",
		OrderRevision: 1, OrderHash: orderHashA, OrderVersion: 4, ActivatedAt: time.Date(2026, 11, 12, 7, 59, 59, 123456000, time.UTC)}
	if len(c.consumed) != 1 || c.consumed[0] != want || len(c.released) != 0 {
		t.Fatalf("consumed %+v, released %+v", c.consumed, c.released)
	}
	for i, typ := range []eventing.EventType{trdapi.EventOrderCancelled, trdapi.EventOrderClosedUnfulfilled, trdapi.EventOrderRemainderClosed,
		trdapi.EventOrderClosed} {
		if err := h(context.Background(), orderEnvelope(typ, map[string]any{"aggregate_version": 7, "order_revision": 2, "order_status": "closed"})); err != nil {
			t.Fatal(err)
		}
		got := c.released[i]
		if len(c.released) != i+1 || len(c.consumed) != 1 || got.EventType != string(typ) || got.OrderID != "order.1" || got.OrderRevision != 2 ||
			got.OrderVersion != 7 || got.OrderHash != orderHashA || !got.ActivatedAt.IsZero() {
			t.Fatalf("%s: released %+v", typ, c.released)
		}
	}
}

// A payload that is not a SalesOrder event's is refused for good with stl.order_event_invalid,
// before the application sees it; an end needs no activation instant.
func TestMalformedOrderEventsAreRejected(t *testing.T) {
	c := &credit{}
	h := OrderCreditHandler(c)
	cases := map[string]messaging.Event{
		"another aggregate":        activated(map[string]any{"aggregate_type": "QuoteRoot"}),
		"no order":                 activated(map[string]any{"order_id": nil}),
		"order is not the subject": activated(map[string]any{"order_id": "order.2"}),
		"invalid order id":         activated(map[string]any{"order_id": "order 1", "aggregate_id": "order 1"}),
		"no version":               activated(map[string]any{"aggregate_version": 0}),
		"no revision":              activated(map[string]any{"order_revision": nil}),
		"no hash":                  activated(map[string]any{"order_hash": "abc"}),
		"no activation instant":    activated(map[string]any{"activated_at": nil}),
		"local activation instant": activated(map[string]any{"activated_at": "2026-11-12T07:59:59+08:00"}),
		"revision as text":         activated(map[string]any{"order_revision": "1"}),
		"another event":            orderEnvelope(trdapi.EventOrderReviewDecided, nil),
	}
	notJSON := activated(nil)
	notJSON.Envelope.Payload = json.RawMessage(`[1]`)
	cases["not an object"] = notJSON
	for name, e := range cases {
		err := h(context.Background(), e)
		if !errors.Is(err, messaging.ErrEventRejected) || !strings.Contains(err.Error(), string(FailureOrderEventInvalid)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(c.consumed) != 0 || len(c.released) != 0 {
		t.Fatalf("the application saw a malformed event: %+v %+v", c.consumed, c.released)
	}
	if err := h(context.Background(), orderEnvelope(trdapi.EventOrderCancelled, nil)); err != nil || len(c.released) != 1 {
		t.Fatalf("an end without activated_at: %v", err)
	}
}

// stl.order_account: Prepare reads the activation's order binding for the application before the
// transaction; the handler hands the application the event and what Prepare read. A malformed
// payload is refused for good in either step, an account STL cannot open for good refuses the event
// under its code, and every other failure is retried.
func TestOrderAccountConsumer(t *testing.T) {
	c := &credit{}
	sub := Subscriptions(nil, c)[1]
	e := activated(nil)
	basis, err := sub.Prepare(context.Background(), e.Envelope)
	if err != nil || len(c.prepared) != 1 || c.prepared[0].OrderRevision != 1 || c.prepared[0].OrderHash != orderHashA ||
		c.prepared[0].ActivatedAt.IsZero() {
		t.Fatalf("prepare %+v, %v", c.prepared, err)
	}
	e.Prepared = basis
	if err = sub.Handler(context.Background(), e); err != nil || len(c.opened) != 1 || c.opened[0] != c.prepared[0] ||
		c.bases[0].Order == nil || c.bases[0].Order.Binding.OrderID != "order.1" || len(c.consumed) != 0 {
		t.Fatalf("handler %+v, %v", c.opened, err)
	}
	if err = sub.Handler(context.Background(), activated(nil)); err == nil || errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("without the prepared basis: %v; want a retry", err)
	}

	bad := activated(map[string]any{"order_hash": "abc"})
	if _, err = sub.Prepare(context.Background(), bad.Envelope); !errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("prepare of a malformed payload: %v", err)
	}
	if err = sub.Handler(context.Background(), bad); !errors.Is(err, messaging.ErrEventRejected) || len(c.opened) != 1 {
		t.Fatalf("handler of a malformed payload: %v", err)
	}
	failing := &credit{prepareErr: errors.New("TRD unavailable")}
	if _, err = Subscriptions(nil, failing)[1].Prepare(context.Background(), e.Envelope); err == nil || errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("a failed read: %v; want a retry", err)
	}

	refused := &credit{err: &application.AccountMismatch{Code: application.FailureAccountNotOpenable, Detail: "order.1"}}
	err = Subscriptions(nil, refused)[1].Handler(context.Background(), e)
	if !errors.Is(err, messaging.ErrEventRejected) || !strings.Contains(err.Error(), "("+application.FailureAccountNotOpenable+")") {
		t.Fatalf("account not openable: %v", err)
	}
	for _, cause := range []error{kernel.Reject(kernel.CodePermissionDenied, "/", ""), errors.New("connection reset")} {
		err = Subscriptions(nil, &credit{err: cause})[1].Handler(context.Background(), e)
		if !errors.Is(err, cause) || errors.Is(err, messaging.ErrEventRejected) {
			t.Errorf("%v: %v", cause, err)
		}
	}
}

// A mismatch between the event and STL's reservations refuses the event for good under its own
// failure code; a command rejection the operator can cure (the actor's grant) and every other
// failure are returned as they are, so the event is delivered again.
func TestMismatchRejectsAndOtherFailuresRetry(t *testing.T) {
	for _, code := range []string{application.FailureReservationReleased, application.FailureReservationNotConsumable} {
		c := &credit{err: &application.CreditMismatch{Code: code, Detail: "order.1 revision 1"}}
		err := OrderCreditHandler(c)(context.Background(), activated(nil))
		var m *application.CreditMismatch
		if !errors.Is(err, messaging.ErrEventRejected) || !errors.As(err, &m) || !strings.Contains(err.Error(), "("+code+")") {
			t.Errorf("%s: %v", code, err)
		}
	}
	denied := kernel.Reject(kernel.CodePermissionDenied, "/", "")
	for _, cause := range []error{denied, errors.New("connection reset"), kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")} {
		c := &credit{err: cause}
		for _, e := range []messaging.Event{activated(nil), orderEnvelope(trdapi.EventOrderClosed, nil)} {
			if err := OrderCreditHandler(c)(context.Background(), e); !errors.Is(err, cause) || errors.Is(err, messaging.ErrEventRejected) {
				t.Errorf("%v on %s: %v", cause, e.Envelope.Type, err)
			}
		}
	}
}
