// Package events holds TSK's Inbox consumer: the inbound adapter that turns the integration events
// of TSK's upstream contexts into calls on TSK's application (repository structure §07). The
// Dispatcher runs the handler in a transaction on TSK's own transaction owner, as the consumer's
// system actor, once per event ID (foundation/messaging).
//
// tsk.task_progress consumes TRD's integration events v1 (Context Map R18 TRD→TSK; event storming
// PL-04 QuoteActivated → quote_ready 可核验, PL-06 OrderActivated → order_active 可核验; WBS S4-06b):
// a waiting task resumes when what it awaits arrives, when its goal becomes verifiable or when an
// approval of its quote is revoked, and a running task whose goal became verifiable starts its
// verification — as SM-TSK-TASK allows (task.resume, task.verify; domain/task wake.go). The
// arrival kinds are tskapi's Arrival* constants.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/application"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TaskProgress is the consumer of TRD's facts for tasks. Its handler runs as
// system:inbox.tsk.task_progress, which brant-admin inbox grants TaskProgressPermissions in every
// company it serves.
const TaskProgress eventing.InboxConsumer = "tsk.task_progress"

// FailureFactInvalid is the failure code of a TRD event whose payload does not carry what TRD's
// events of its type carry: the event is refused for good (dead letter and alert, ADR-BR-015).
const FailureFactInvalid eventing.FailureCode = "tsk.trd_fact_invalid"

// TaskProgressPermissions are what the consumer's commands need — task.resume and task.verify — and
// tsk.read, which the command ledger asks for before it answers a command from its stored result.
func TaskProgressPermissions() []string {
	return []string{api.CommandResume, api.CommandVerify, api.PermissionRead}
}

// Facts are the TRD event types the consumer reads (TRD integration events v1 as TSK uses them).
var Facts = []eventing.EventType{trdapi.EventQuoteReviewDecided, trdapi.EventQuoteActivated, trdapi.EventQuoteReplaced,
	trdapi.EventQuoteApprovalRevoked, trdapi.EventCustomerResponseRecorded, trdapi.EventAcceptanceVerified,
	trdapi.EventOrderReviewDecided, trdapi.EventOrderActivated, trdapi.EventOrderApprovalRevoked}

// Tasks is what tsk.task_progress asks of TSK's application; application.Service implements it.
type Tasks interface {
	ProgressOn(ctx context.Context, exec bentopg.Executor, consumer, eventID string, f task.Fact, origin string) (application.Progress, error)
}

// Subscriptions returns TSK's subscriptions on db, TSK's own transaction owner.
func Subscriptions(db *bentopg.DB, app Tasks) []messaging.Subscription {
	return []messaging.Subscription{{
		Consumer:    TaskProgress,
		Source:      trdapi.Source,
		EventTypes:  Facts,
		DB:          db,
		Permissions: TaskProgressPermissions(),
		Handler:     ProgressHandler(app),
	}}
}

// ProgressHandler applies one TRD event to the tasks it concerns. A payload that is not what TRD's
// events of its type carry is refused for good; every other failure — the consumer's actor not
// provisioned, a conflict, the database — rolls the transaction back and the event is delivered
// again. Nothing is read from TRD: the event names the objects, and the tasks are TSK's.
func ProgressHandler(app Tasks) messaging.EventFunc {
	return func(ctx context.Context, e messaging.Event) error {
		f, err := FactOf(e.Envelope)
		if err != nil {
			return messaging.RejectEvent(FailureFactInvalid, err)
		}
		_, err = app.ProgressOn(ctx, e.Exec, string(TaskProgress), string(e.Envelope.ID), f, Origin(e.Envelope))
		return err
	}
}

// Origin names the TRD event a resume records as the arrival's origin.
func Origin(envelope eventing.Envelope) string {
	return "TRD " + strings.TrimPrefix(string(envelope.Type), trdapi.Source+".") + " event " + string(envelope.ID)
}

// factPayload is what the consumer reads of a TRD event's payload (contract C4: identifiers and
// versions; TRD's command ledger writes the aggregate members, the command the rest).
type factPayload struct {
	AggregateType      string `json:"aggregate_type"`
	AggregateID        string `json:"aggregate_id"`
	QuoteID            string `json:"quote_id"`
	BusinessRevision   int64  `json:"business_revision"`
	ReviewRequestID    string `json:"review_request_id"`
	ApprovalID         string `json:"approval_id"`
	EffectivityVersion int64  `json:"effectivity_version"`
	ResponseID         string `json:"response_id"`
	AcceptanceID       string `json:"acceptance_id"`
	OrderID            string `json:"order_id"`
	OrderRevision      int64  `json:"order_revision"`
	ReviewID           string `json:"review_id"`
}

const maxVersion = 2147483647

// FactOf reads envelope as one of TRD's facts the consumer subscribes to, or says what its payload
// lacks.
func FactOf(envelope eventing.Envelope) (task.Fact, error) {
	var p factPayload
	if err := json.Unmarshal(envelope.Payload, &p); err != nil {
		return task.Fact{}, fmt.Errorf("tsk: TRD event payload: %w", err)
	}
	fail := func(what string) (task.Fact, error) {
		return task.Fact{}, fmt.Errorf("tsk: TRD event %s of type %s: %s", envelope.ID, envelope.Type, what)
	}
	if envelope.ID == "" {
		return fail("no event ID")
	}
	quote := task.Ref{Kind: api.ArrivalQuote, ID: p.QuoteID, Version: p.BusinessRevision}
	if !valid(quote) || p.BusinessRevision < 1 {
		return fail("quote_id and business_revision do not name a quote revision")
	}
	f := task.Fact{QuoteID: p.QuoteID}
	var aggregate, aggregateID string
	var answers []task.Ref
	add := func(kind, id string, version int64) {
		answers = append(answers, task.Ref{Kind: kind, ID: id, Version: version})
	}
	switch envelope.Type {
	case trdapi.EventQuoteReviewDecided:
		aggregate, aggregateID = "QuoteRoot", p.QuoteID
		add(api.ArrivalQuoteReview, p.ReviewRequestID, p.BusinessRevision)
	case trdapi.EventQuoteActivated, trdapi.EventQuoteReplaced:
		aggregate, aggregateID, f.Goal = "QuoteRoot", p.QuoteID, api.GoalQuoteReady
	case trdapi.EventQuoteApprovalRevoked:
		aggregate, aggregateID, f.Revocation = "QuoteRoot", p.QuoteID, true
		add(api.ArrivalQuoteApproval, p.ApprovalID, p.EffectivityVersion)
	case trdapi.EventCustomerResponseRecorded:
		aggregate, aggregateID = "CustomerResponse", p.ResponseID
		add(api.ArrivalCustomerResponse, p.ResponseID, 0)
	case trdapi.EventAcceptanceVerified:
		aggregate, aggregateID = "Acceptance", p.AcceptanceID
		add(api.ArrivalAcceptance, p.AcceptanceID, p.EffectivityVersion)
		add(api.ArrivalCustomerResponse, p.ResponseID, 0)
	case trdapi.EventOrderReviewDecided, trdapi.EventOrderApprovalRevoked:
		aggregate, aggregateID, f.Revocation = "SalesOrder", p.OrderID, envelope.Type == trdapi.EventOrderApprovalRevoked
		add(api.ArrivalOrderReview, p.ReviewID, p.OrderRevision)
		add(api.ArrivalOrder, p.OrderID, p.OrderRevision)
	case trdapi.EventOrderActivated:
		aggregate, aggregateID, f.Goal = "SalesOrder", p.OrderID, api.GoalOrderActive
		add(api.ArrivalOrder, p.OrderID, p.OrderRevision)
	default:
		return fail("not a TRD fact " + string(TaskProgress) + " reads")
	}
	if p.AggregateType != aggregate || p.AggregateID != aggregateID {
		return fail("aggregate_type and aggregate_id are not the " + aggregate + " the event names")
	}
	for _, r := range answers {
		if !valid(r) {
			return fail(r.Kind + " is not an identifier at a version")
		}
	}
	if (aggregate == "SalesOrder" && p.OrderRevision < 1) || (envelope.Type == trdapi.EventQuoteApprovalRevoked && p.EffectivityVersion < 1) {
		return fail("a version the event carries is not positive")
	}
	f.Answers = append(answers, quote)
	return f, nil
}

// valid reports whether r names an object: an identifier at a version from 0.
func valid(r task.Ref) bool {
	return kernel.ValidIdentifier(r.ID) && r.Version >= 0 && r.Version <= maxVersion
}
