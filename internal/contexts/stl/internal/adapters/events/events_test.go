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

// credit records what the consumer asks of the application and answers err.
type credit struct {
	consumed, released []application.OrderEvent
	err                error
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

// STL declares one consumer, stl.order_credit, on TRD's source: OrderActivated (PL-12) and the four
// ends of an order (PL-13; OrderClosedUnfulfilled ends a draft, whose reservation is still
// reserved), not OrderAmended (PL-09). Its system actor is system:inbox.stl.order_credit with
// credit.consume, credit.release and stl.read.
func TestOrderCreditSubscription(t *testing.T) {
	subs := Subscriptions(nil, &credit{})
	if len(subs) != 1 {
		t.Fatalf("%d subscriptions", len(subs))
	}
	s := subs[0]
	want := []eventing.EventType{trdapi.EventOrderActivated, trdapi.EventOrderCancelled, trdapi.EventOrderClosedUnfulfilled,
		trdapi.EventOrderRemainderClosed, trdapi.EventOrderClosed}
	if s.Consumer != "stl.order_credit" || s.Source != trdapi.Source || !slices.Equal(s.EventTypes, want) || s.Prepare != nil || s.Handler == nil ||
		slices.Contains(s.EventTypes, eventing.EventType(trdapi.EventOrderAmended)) {
		t.Fatalf("subscription %+v", s)
	}
	grants, err := messaging.InboxGrants(subs)
	if err != nil || len(grants) != 1 || grants[0].Actor != (kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.stl.order_credit"}) ||
		!slices.Equal(grants[0].Permissions, []string{"credit.consume", "credit.release", "stl.read"}) {
		t.Fatalf("grants %+v %v", grants, err)
	}
	// The declared types are not the handler's own slices: a caller cannot change what it routes.
	s.EventTypes[0] = trdapi.EventOrderAmended
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
		"another event":            orderEnvelope(trdapi.EventOrderAmended, nil),
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
