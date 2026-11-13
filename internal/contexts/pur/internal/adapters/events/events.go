// Package events holds PUR's Inbox consumer: the inbound adapter that turns TRD's order events into
// calls on PUR's application (repository structure §07). The Dispatcher runs the handler in a
// transaction on PUR's own transaction owner, as the consumer's system actor, once per event ID
// (foundation/messaging).
//
// pur.allocation_review follows TRD's order changes (Context Map R19 TRD→PUR; event storming PL-09,
// ES-09 step 5; WF-06 step 4; S5 decisions D-18, D-21; WBS S5-09b): OrderAmended, OrderCancelled and
// OrderRemainderClosed each open, when they touch what PUR holds of the order, one allocation review
// for the order revision and change (allocation_review.open, SM-PUR-ALLOCATION-REVIEW), which a
// purchaser settles by reference to the dispositions made.
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
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/application"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// AllocationReview is the consumer of TRD's order changes. Its handler runs as
// system:inbox.pur.allocation_review, which brant-admin inbox grants AllocationReviewPermissions in
// every company it serves.
const AllocationReview eventing.InboxConsumer = "pur.allocation_review"

// FailureOrderEventInvalid is the failure code of an order event whose payload does not carry what
// TRD's SalesOrder events carry: the event is refused for good.
const FailureOrderEventInvalid eventing.FailureCode = "pur.order_event_invalid"

// salesOrder is the aggregate type of TRD's order events.
const salesOrder = "SalesOrder"

// AllocationReviewPermissions are what the consumer's command needs: allocation_review.open, and
// pur.read, which the command ledger asks for before it answers a command from its stored result.
func AllocationReviewPermissions() []string {
	return []string{api.CommandOpenAllocationReview, api.PermissionRead}
}

// causes are the event types the consumer handles, with the cause each opens a review for.
var causes = map[eventing.EventType]string{
	trdapi.EventOrderAmended:         api.CauseOrderAmended,
	trdapi.EventOrderCancelled:       api.CauseOrderCancelled,
	trdapi.EventOrderRemainderClosed: api.CauseOrderRemainderClosed,
}

// Reviews is what pur.allocation_review asks of PUR's application; application.Service implements it.
type Reviews interface {
	PrepareReview(ctx context.Context, e application.OrderChange) (application.ReviewBasis, error)
	ReviewOrderChange(ctx context.Context, exec bentopg.Executor, e application.OrderChange, b application.ReviewBasis) (string, error)
}

// Subscriptions returns PUR's subscriptions on db, PUR's own transaction owner.
func Subscriptions(db *bentopg.DB, app Reviews) []messaging.Subscription {
	types := []eventing.EventType{trdapi.EventOrderAmended, trdapi.EventOrderCancelled, trdapi.EventOrderRemainderClosed}
	return []messaging.Subscription{{
		Consumer:    AllocationReview,
		Source:      trdapi.Source,
		EventTypes:  types,
		DB:          db,
		Permissions: AllocationReviewPermissions(),
		Prepare:     ReviewPrepare(app),
		Handler:     ReviewHandler(app),
	}}
}

// ReviewPrepare reads, before the consumer's transaction, what the order change needs from TRD
// (application.PrepareReview). A payload that is not a SalesOrder event's is refused for good; a
// failed read is retried.
func ReviewPrepare(reviews Reviews) func(ctx context.Context, envelope eventing.Envelope) (any, error) {
	return func(ctx context.Context, envelope eventing.Envelope) (any, error) {
		change, err := orderChange(envelope)
		if err != nil {
			return nil, messaging.RejectEvent(FailureOrderEventInvalid, err)
		}
		return reviews.PrepareReview(ctx, change)
	}
}

// ReviewHandler applies the order change to PUR's allocation reviews with what ReviewPrepare read
// (application.ReviewOrderChange). A change PUR cannot review for good (application.ReviewMismatch)
// refuses the event; every other failure — the consumer's actor not provisioned, a version conflict,
// the database — rolls the transaction back and is delivered again.
//
// Order matters: a review is kept per (order, revision, cause) and follows TRD's row version of the
// order (aggregate_version), so a redelivered event, or one replayed after a later change of the
// order was reviewed, opens nothing and reopens nothing settled.
func ReviewHandler(reviews Reviews) messaging.EventFunc {
	return func(ctx context.Context, e messaging.Event) error {
		change, err := orderChange(e.Envelope)
		if err != nil {
			return messaging.RejectEvent(FailureOrderEventInvalid, err)
		}
		basis, ok := e.Prepared.(application.ReviewBasis)
		if !ok {
			return fmt.Errorf("pur: %s delivered without its prepared basis", AllocationReview)
		}
		_, err = reviews.ReviewOrderChange(ctx, e.Exec, change, basis)
		var mismatch *application.ReviewMismatch
		if errors.As(err, &mismatch) {
			return messaging.RejectEvent(eventing.FailureCode(mismatch.Code), mismatch)
		}
		return err
	}
}

// orderPayload is what the consumer reads of a SalesOrder event's payload (contract C4: identifiers
// and versions): the aggregate the command ledger names, the order binding TRD adds to every order
// event and, for an active order's end, its lines with what each shipped.
type orderPayload struct {
	AggregateType    string `json:"aggregate_type"`
	AggregateID      string `json:"aggregate_id"`
	AggregateVersion int64  `json:"aggregate_version"`
	OrderID          string `json:"order_id"`
	OrderRevision    int64  `json:"order_revision"`
	OrderHash        string `json:"order_hash"`
	EndedFrom        string `json:"ended_from"`
	Lines            []struct {
		LineID  string `json:"line_id"`
		Shipped string `json:"shipped"`
	} `json:"lines"`
}

var orderHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// orderChange reads envelope as an order change, or says what its payload lacks.
func orderChange(envelope eventing.Envelope) (application.OrderChange, error) {
	var p orderPayload
	if err := json.Unmarshal(envelope.Payload, &p); err != nil {
		return application.OrderChange{}, fmt.Errorf("pur: order event payload: %w", err)
	}
	fail := func(what string) (application.OrderChange, error) {
		return application.OrderChange{}, fmt.Errorf("pur: order event %s of type %s: %s", envelope.ID, envelope.Type, what)
	}
	cause, ok := causes[envelope.Type]
	switch {
	case !ok:
		return fail("not an order change " + string(AllocationReview) + " handles")
	case !kernel.ValidIdentifier(string(envelope.ID)):
		return fail("the event ID is not an identifier")
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
	out := application.OrderChange{EventID: string(envelope.ID), Cause: cause, OrderID: p.OrderID, Revision: p.OrderRevision, Hash: p.OrderHash,
		Version: p.AggregateVersion}
	if cause != api.CauseOrderAmended {
		for _, l := range p.Lines {
			if !kernel.ValidIdentifier(l.LineID) || slices.ContainsFunc(out.Lines, func(x application.EndedLine) bool { return x.LineID == l.LineID }) {
				return fail("a line is not identified once")
			}
			out.Lines = append(out.Lines, application.EndedLine{LineID: l.LineID, Shipped: l.Shipped})
		}
		if p.EndedFrom != "" && p.EndedFrom != "active" {
			return fail("ended_from is not active")
		}
	}
	return out, nil
}
