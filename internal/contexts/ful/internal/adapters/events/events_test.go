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
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/application"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The Inbox consumer ful.shipment_review (WBS S5-09b, FUL part; event storming PL-09; WF-06 step 5;
// Context Map R27). Python V0.50 has no fulfilment context; the tests cite design IDs.

// reviews records what the consumer asks of the application and answers err.
type reviews struct {
	prepared, reviewed []application.OrderChange
	bases              []application.ReviewBasis
	err, prepareErr    error
}

func (r *reviews) PrepareReview(_ context.Context, e application.OrderChange) (application.ReviewBasis, error) {
	r.prepared = append(r.prepared, e)
	return application.ReviewBasis{Known: true, Order: application.OrderFact{Revision: e.Revision, Hash: e.Hash}}, r.prepareErr
}

func (r *reviews) ReviewOrderChange(_ context.Context, _ bentopg.Executor, e application.OrderChange, b application.ReviewBasis) (string, error) {
	r.reviewed, r.bases = append(r.reviewed, e), append(r.bases, b)
	return "review_opened", r.err
}

var orderHashA = strings.Repeat("a", 64)

func payload(fields map[string]any) json.RawMessage {
	p := map[string]any{"tenant_id": "tenant.1", "company_id": "company.A", "aggregate_type": "SalesOrder", "aggregate_id": "order.1",
		"aggregate_version": 6, "actor_kind": "human", "actor_id": "reviewer", "authority_epoch": 3, "action_id": "act-1",
		"order_id": "order.1", "quote_id": "quote.1", "business_revision": 1, "business_hash": strings.Repeat("b", 64), "order_revision": 2,
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

func orderEvent(typ eventing.EventType, fields map[string]any) messaging.Event {
	at := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	return messaging.Event{Consumer: ShipmentReview, Envelope: eventing.Envelope{SpecVersion: eventing.SpecVersion,
		ID: "evt-0190a1b2-c3d4-7e5f-8a6b-0c1d2e3f4a5b", Source: trdapi.Source, Type: typ, Version: 1, Scope: "tenant.1/company.A",
		Subject: "order.1", PartitionKey: "tenant.1/company.A/SalesOrder:order.1", OccurredAt: at, RecordedAt: at,
		ContentType: eventing.JSONContentType, Payload: payload(fields)}, Prepared: application.ReviewBasis{}}
}

func ended(typ eventing.EventType) messaging.Event {
	return orderEvent(typ, map[string]any{"ended_from": "active", "order_status": "closed", "conversion_slot": "slot.1",
		"lines": []any{map[string]any{"line_id": "L1", "unit": "PCS", "ordered": "120", "shipped": "116", "remainder": "4"}}})
}

// FUL declares one consumer, ful.shipment_review, on TRD's source: OrderAmended, OrderCancelled and
// OrderRemainderClosed (PL-09; WF-06 step 5). Its system actor system:inbox.ful.shipment_review holds
// shipment_review.open (SM-FUL-SHIPMENT-REVIEW: the system opens) and ful.read — what brant-admin
// inbox grants it.
func TestShipmentReviewSubscription(t *testing.T) {
	subs := Subscriptions(nil, &reviews{})
	if len(subs) != 1 {
		t.Fatalf("%d subscriptions", len(subs))
	}
	s := subs[0]
	want := []eventing.EventType{trdapi.EventOrderAmended, trdapi.EventOrderCancelled, trdapi.EventOrderRemainderClosed}
	if s.Consumer != "ful.shipment_review" || s.Source != trdapi.Source || !slices.Equal(s.EventTypes, want) || s.Prepare == nil || s.Handler == nil {
		t.Fatalf("subscription %+v", s)
	}
	grants, err := messaging.InboxGrants(subs)
	if err != nil || len(grants) != 1 || grants[0].Actor != (kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.ful.shipment_review"}) ||
		!slices.Equal(grants[0].Permissions, []string{"ful.read", "shipment_review.open"}) {
		t.Fatalf("grants %+v %v", grants, err)
	}
}

// Prepare and the handler hand the application the order change: the binding, TRD's row version of
// the order and the cause. What Prepare read reaches the handler's call.
func TestOrderChangesReachTheApplication(t *testing.T) {
	r := &reviews{}
	sub := Subscriptions(nil, r)[0]
	e := orderEvent(trdapi.EventOrderAmended, map[string]any{"from_order_revision": 1})
	basis, err := sub.Prepare(context.Background(), e.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	e.Prepared = basis
	if err = sub.Handler(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	want := application.OrderChange{EventID: string(e.Envelope.ID), Cause: api.CauseOrderAmended, OrderID: "order.1", Revision: 2, Hash: orderHashA,
		Version: 6}
	if len(r.prepared) != 1 || r.prepared[0] != want || len(r.reviewed) != 1 || r.reviewed[0] != want || !r.bases[0].Known ||
		r.bases[0].Order.Revision != 2 {
		t.Fatalf("prepared %+v reviewed %+v bases %+v", r.prepared, r.reviewed, r.bases)
	}
	for typ, cause := range map[eventing.EventType]string{trdapi.EventOrderCancelled: api.CauseOrderCancelled,
		trdapi.EventOrderRemainderClosed: api.CauseOrderRemainderClosed} {
		if err := sub.Handler(context.Background(), ended(typ)); err != nil {
			t.Fatal(err)
		}
		if got := r.reviewed[len(r.reviewed)-1]; got.Cause != cause || got.Revision != 2 {
			t.Fatalf("%s: %+v", typ, got)
		}
	}
	// A draft order's cancel carries no ended_from.
	if err := sub.Handler(context.Background(), orderEvent(trdapi.EventOrderCancelled, map[string]any{"order_status": "cancelled"})); err != nil {
		t.Fatalf("a draft's cancel: %v", err)
	}
}

// A payload that is not a SalesOrder event's is refused for good with ful.order_event_invalid, in
// Prepare and in the handler, before the application sees it.
func TestMalformedOrderChangesAreRejected(t *testing.T) {
	r := &reviews{}
	sub := Subscriptions(nil, r)[0]
	cases := map[string]messaging.Event{
		"another aggregate":        orderEvent(trdapi.EventOrderAmended, map[string]any{"aggregate_type": "QuoteRoot"}),
		"order is not the subject": orderEvent(trdapi.EventOrderAmended, map[string]any{"order_id": "order.2"}),
		"no version":               orderEvent(trdapi.EventOrderAmended, map[string]any{"aggregate_version": 0}),
		"no revision":              orderEvent(trdapi.EventOrderAmended, map[string]any{"order_revision": nil}),
		"no hash":                  orderEvent(trdapi.EventOrderAmended, map[string]any{"order_hash": "abc"}),
		"another event":            orderEvent(trdapi.EventOrderActivated, nil),
		"ended from a draft":       orderEvent(trdapi.EventOrderCancelled, map[string]any{"ended_from": "draft"}),
	}
	badID := orderEvent(trdapi.EventOrderAmended, nil)
	badID.Envelope.ID = "evt 1"
	cases["an event ID that is no identifier"] = badID
	notJSON := orderEvent(trdapi.EventOrderAmended, nil)
	notJSON.Envelope.Payload = json.RawMessage(`[1]`)
	cases["not an object"] = notJSON
	for name, e := range cases {
		for step, err := range map[string]error{"handler": sub.Handler(context.Background(), e), "prepare": func() error {
			_, err := sub.Prepare(context.Background(), e.Envelope)
			return err
		}()} {
			if !errors.Is(err, messaging.ErrEventRejected) || !strings.Contains(err.Error(), string(FailureOrderEventInvalid)) {
				t.Errorf("%s (%s): %v", name, step, err)
			}
		}
	}
	if len(r.prepared) != 0 || len(r.reviewed) != 0 {
		t.Fatalf("the application saw a malformed event: %+v %+v", r.prepared, r.reviewed)
	}
}

// A change FUL cannot review for good (application.ReviewMismatch) refuses the event under its code;
// a missing prepared basis, a failed read and every other failure are retried.
func TestReviewFailures(t *testing.T) {
	r := &reviews{err: &application.ReviewMismatch{Code: application.FailureReviewNotOpenable, Detail: "TRD shows no order order.1"}}
	sub := Subscriptions(nil, r)[0]
	err := sub.Handler(context.Background(), orderEvent(trdapi.EventOrderAmended, nil))
	if !errors.Is(err, messaging.ErrEventRejected) || !strings.Contains(err.Error(), application.FailureReviewNotOpenable) {
		t.Fatalf("mismatch: %v", err)
	}
	r.err = errors.New("database gone")
	if err = sub.Handler(context.Background(), orderEvent(trdapi.EventOrderAmended, nil)); errors.Is(err, messaging.ErrEventRejected) || err == nil {
		t.Fatalf("retry: %v", err)
	}
	r.err = nil
	e := orderEvent(trdapi.EventOrderAmended, nil)
	e.Prepared = nil
	if err = sub.Handler(context.Background(), e); err == nil || errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("without its basis: %v", err)
	}
	r.prepareErr = errors.New("TRD unreachable")
	if _, err = sub.Prepare(context.Background(), e.Envelope); err == nil || errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("a failed read: %v", err)
	}
}
