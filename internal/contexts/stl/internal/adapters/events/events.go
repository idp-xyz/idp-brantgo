// Package events holds STL's Inbox consumers: the inbound adapter that turns the integration events
// of STL's upstream contexts into calls on STL's application (repository structure §07). The
// Dispatcher runs each handler in a transaction on STL's own transaction owner, as the consumer's
// system actor, once per event ID (foundation/messaging).
//
// stl.order_credit consumes TRD's order events (Context Map R32 TRD→STL, "OrderCommercialSnapshot v1
// 与订单事件"; WBS S5-12c): OrderActivated has the order revision's credit reservation consumed (event
// storming PL-12), and the end of an order — OrderCancelled, OrderClosedUnfulfilled,
// OrderRemainderClosed, OrderClosed — has every reservation the order still holds released (PL-13;
// WF-08 step 5; SM-STL-CREDIT-RESERVATION). OrderAmended belongs to the payment plan review (PL-09).
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/application"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// OrderCredit is the consumer of TRD's order events for customer credit. Its handler runs as
// system:inbox.stl.order_credit, which brant-admin inbox grants OrderCreditPermissions in every
// company it serves.
const OrderCredit eventing.InboxConsumer = "stl.order_credit"

// FailureOrderEventInvalid is the failure code of an order event whose payload does not carry what
// TRD's SalesOrder events carry: the event is refused for good.
const FailureOrderEventInvalid eventing.FailureCode = "stl.order_event_invalid"

// salesOrder is the aggregate type of TRD's order events.
const salesOrder = "SalesOrder"

// OrderCreditPermissions are what the consumer's commands need: credit.consume and credit.release,
// and stl.read, which the command ledger asks for before it answers a command from its stored result.
func OrderCreditPermissions() []string {
	return []string{api.CommandConsume, api.CommandRelease, api.PermissionRead}
}

// activationEvents and endEvents are the event types of TRD the consumer handles.
var (
	activationEvents = []eventing.EventType{trdapi.EventOrderActivated}
	endEvents        = []eventing.EventType{trdapi.EventOrderCancelled, trdapi.EventOrderClosedUnfulfilled, trdapi.EventOrderRemainderClosed,
		trdapi.EventOrderClosed}
)

// Credit is what the consumer asks of STL's application; application.Service implements it.
type Credit interface {
	ConsumeOnActivation(ctx context.Context, exec bentopg.Executor, e application.OrderEvent) (application.CreditEffect, error)
	ReleaseOnOrderEnd(ctx context.Context, exec bentopg.Executor, e application.OrderEvent) (application.CreditEffect, error)
}

// Subscriptions returns STL's subscriptions on db, STL's own transaction owner.
func Subscriptions(db *bentopg.DB, credit Credit) []messaging.Subscription {
	return []messaging.Subscription{{
		Consumer:    OrderCredit,
		Source:      trdapi.Source,
		EventTypes:  slices.Concat(activationEvents, endEvents),
		DB:          db,
		Permissions: OrderCreditPermissions(),
		Handler:     OrderCreditHandler(credit),
	}}
}

// OrderCreditHandler applies one of TRD's order events to the order's credit reservations.
//
// The event is refused for good (messaging.RejectEvent: dead letter and alert at once, ADR-BR-015)
// when its payload is not a SalesOrder event's, and when STL's reservations contradict it
// (application.CreditMismatch). Every other failure — the consumer's actor not provisioned, a
// version conflict, the database — rolls the transaction back and is delivered again.
//
// The order of an order's events matters in one place: an OrderActivated that arrives after the
// order's end (a dead-letter replay) must consume nothing. The end released every reservation the
// order held, reserved ones too, and a released reservation is never consumed, so the late
// activation finds nothing to consume and is settled without effect.
func OrderCreditHandler(credit Credit) messaging.EventFunc {
	return func(ctx context.Context, e messaging.Event) error {
		event, err := orderEvent(e.Envelope)
		if err != nil {
			return messaging.RejectEvent(FailureOrderEventInvalid, err)
		}
		switch {
		case slices.Contains(activationEvents, e.Envelope.Type):
			_, err = credit.ConsumeOnActivation(ctx, e.Exec, event)
		case slices.Contains(endEvents, e.Envelope.Type):
			_, err = credit.ReleaseOnOrderEnd(ctx, e.Exec, event)
		default:
			// The Dispatcher delivers subscribed types only.
			return messaging.RejectEvent(FailureOrderEventInvalid, fmt.Errorf("stl: %s is not an order event %s handles", e.Envelope.Type, OrderCredit))
		}
		var mismatch *application.CreditMismatch
		if errors.As(err, &mismatch) {
			return messaging.RejectEvent(eventing.FailureCode(mismatch.Code), mismatch)
		}
		return err
	}
}

// orderPayload is what the consumer reads of a SalesOrder event's payload (contract C4: identifiers
// and versions): the aggregate the command ledger names and the order binding TRD adds to every
// order event, with the activation instant of OrderActivated.
type orderPayload struct {
	AggregateType    string `json:"aggregate_type"`
	AggregateID      string `json:"aggregate_id"`
	AggregateVersion int64  `json:"aggregate_version"`
	OrderID          string `json:"order_id"`
	OrderRevision    int64  `json:"order_revision"`
	OrderHash        string `json:"order_hash"`
	ActivatedAt      string `json:"activated_at"`
}

var orderHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// orderEvent reads envelope as an order event, or says what its payload lacks.
func orderEvent(envelope eventing.Envelope) (application.OrderEvent, error) {
	var p orderPayload
	if err := json.Unmarshal(envelope.Payload, &p); err != nil {
		return application.OrderEvent{}, fmt.Errorf("stl: order event payload: %w", err)
	}
	fail := func(what string) (application.OrderEvent, error) {
		return application.OrderEvent{}, fmt.Errorf("stl: order event %s of type %s: %s", envelope.ID, envelope.Type, what)
	}
	switch {
	case p.AggregateType != salesOrder:
		return fail("aggregate_type is not " + salesOrder)
	case !kernel.ValidIdentifier(p.OrderID) || p.OrderID != p.AggregateID:
		return fail("order_id is not the aggregate's identifier")
	case p.AggregateVersion < 1:
		return fail("aggregate_version is not positive")
	case p.OrderRevision < 1:
		return fail("order_revision is not positive")
	case !orderHash.MatchString(p.OrderHash):
		return fail("order_hash is not a SHA-256")
	}
	out := application.OrderEvent{EventID: string(envelope.ID), EventType: string(envelope.Type), OrderID: p.OrderID,
		OrderRevision: p.OrderRevision, OrderHash: p.OrderHash, OrderVersion: p.AggregateVersion}
	if slices.Contains(activationEvents, envelope.Type) {
		at, err := kernel.ParseTimestamp(p.ActivatedAt)
		if err != nil {
			return fail("activated_at is not a timestamp")
		}
		out.ActivatedAt = at
	}
	return out, nil
}
