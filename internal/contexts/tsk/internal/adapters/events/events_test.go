package events

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/application"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
)

// Unit B3 (WBS S4-06b): the Inbox consumer tsk.task_progress (Context Map R18 TRD integration events
// v1; PL-04, PL-06). Python V0.50 and Q01 V0.1 have no event consumer; the tests cite design IDs.

type tasks struct {
	facts   []task.Fact
	origins []string
	ids     []string
	err     error
}

func (x *tasks) ProgressOn(_ context.Context, _ bentopg.Executor, consumer, eventID string, f task.Fact, origin string) (application.Progress, error) {
	if consumer != string(TaskProgress) {
		return application.Progress{}, errors.New("consumer " + consumer)
	}
	x.facts, x.origins, x.ids = append(x.facts, f), append(x.origins, origin), append(x.ids, eventID)
	return application.Progress{}, x.err
}

// fact is TRD's event of type typ as its command ledger writes it: aggregate, the members every event
// of the aggregate carries, and fields (nil deletes one).
func fact(typ eventing.EventType, aggregate, id string, fields map[string]any) messaging.Event {
	p := map[string]any{"tenant_id": "tenant.1", "company_id": "company.A", "aggregate_type": aggregate, "aggregate_id": id,
		"aggregate_version": 4, "actor_kind": "human", "actor_id": "author", "authority_epoch": 3, "action_id": "act-1",
		"quote_id": "quote.1", "business_revision": 2, "business_hash": strings.Repeat("b", 64)}
	for k, v := range fields {
		if v == nil {
			delete(p, k)
			continue
		}
		p[k] = v
	}
	body, _ := json.Marshal(p)
	return messaging.Event{Consumer: TaskProgress, Envelope: eventing.Envelope{ID: "evt-1", Source: trdapi.Source, Type: typ, Payload: body}}
}

var order = map[string]any{"order_id": "order.1", "order_revision": 1, "order_hash": strings.Repeat("a", 64), "order_status": "active"}

func with(base map[string]any, more map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

func TestSubscription(t *testing.T) {
	subs := Subscriptions(nil, &tasks{})
	if len(subs) != 1 {
		t.Fatalf("%d subscriptions", len(subs))
	}
	s := subs[0]
	want := []eventing.EventType{trdapi.EventQuoteReviewDecided, trdapi.EventQuoteActivated, trdapi.EventQuoteReplaced,
		trdapi.EventQuoteApprovalRevoked, trdapi.EventCustomerResponseRecorded, trdapi.EventAcceptanceVerified, trdapi.EventOrderReviewDecided,
		trdapi.EventOrderActivated, trdapi.EventOrderApprovalRevoked}
	if s.Consumer != "tsk.task_progress" || s.Source != "idp-brant.trd" || !slices.Equal(s.EventTypes, want) || s.Prepare != nil ||
		!slices.Equal(s.Permissions, []string{"task.resume", "task.verify", "tsk.read"}) {
		t.Fatalf("%+v", s)
	}
}

// Each TRD fact as TSK reads it: the quote, the objects it answers (most specific first, the quote
// last), the goal it makes verifiable (PL-04 QuoteActivated and QuoteReplaced: quote_ready; PL-06
// OrderActivated: order_active) and whether it revokes an approval.
func TestTRDFactsAsTSKReadsThem(t *testing.T) {
	quote := task.Ref{Kind: api.ArrivalQuote, ID: "quote.1", Version: 2}
	for name, c := range map[string]struct {
		e    messaging.Event
		want task.Fact
	}{
		"QuoteReviewDecided": {fact(trdapi.EventQuoteReviewDecided, "QuoteRoot", "quote.1", map[string]any{"review_request_id": "review.1"}),
			task.Fact{QuoteID: "quote.1", Answers: []task.Ref{{Kind: "quote_review", ID: "review.1", Version: 2}, quote}}},
		"QuoteActivated": {fact(trdapi.EventQuoteActivated, "QuoteRoot", "quote.1", nil),
			task.Fact{QuoteID: "quote.1", Answers: []task.Ref{quote}, Goal: "quote_ready"}},
		"QuoteReplaced": {fact(trdapi.EventQuoteReplaced, "QuoteRoot", "quote.1", map[string]any{"replaced_revision": 1}),
			task.Fact{QuoteID: "quote.1", Answers: []task.Ref{quote}, Goal: "quote_ready"}},
		"QuoteApprovalRevoked": {fact(trdapi.EventQuoteApprovalRevoked, "QuoteRoot", "quote.1", map[string]any{"approval_id": "approval.1",
			"effectivity_version": 2}), task.Fact{QuoteID: "quote.1", Answers: []task.Ref{{Kind: "quote_approval", ID: "approval.1", Version: 2}, quote},
			Revocation: true}},
		"CustomerResponseRecorded": {fact(trdapi.EventCustomerResponseRecorded, "CustomerResponse", "response.1", map[string]any{"response_id": "response.1"}),
			task.Fact{QuoteID: "quote.1", Answers: []task.Ref{{Kind: "customer_response", ID: "response.1"}, quote}}},
		"AcceptanceVerified": {fact(trdapi.EventAcceptanceVerified, "Acceptance", "acceptance.1", map[string]any{"acceptance_id": "acceptance.1",
			"response_id": "response.1", "effectivity_version": 1}), task.Fact{QuoteID: "quote.1", Answers: []task.Ref{
			{Kind: "acceptance", ID: "acceptance.1", Version: 1}, {Kind: "customer_response", ID: "response.1"}, quote}}},
		"OrderReviewDecided": {fact(trdapi.EventOrderReviewDecided, "SalesOrder", "order.1", with(order, map[string]any{"review_id": "review.o"})),
			task.Fact{QuoteID: "quote.1", Answers: []task.Ref{{Kind: "order_review", ID: "review.o", Version: 1}, {Kind: "order", ID: "order.1",
				Version: 1}, quote}}},
		"OrderActivated": {fact(trdapi.EventOrderActivated, "SalesOrder", "order.1", order),
			task.Fact{QuoteID: "quote.1", Answers: []task.Ref{{Kind: "order", ID: "order.1", Version: 1}, quote}, Goal: "order_active"}},
		"OrderApprovalRevoked": {fact(trdapi.EventOrderApprovalRevoked, "SalesOrder", "order.1", with(order, map[string]any{"review_id": "review.o"})),
			task.Fact{QuoteID: "quote.1", Answers: []task.Ref{{Kind: "order_review", ID: "review.o", Version: 1}, {Kind: "order", ID: "order.1",
				Version: 1}, quote}, Revocation: true}},
	} {
		got, err := FactOf(c.e.Envelope)
		if err != nil || got.QuoteID != c.want.QuoteID || got.Goal != c.want.Goal || got.Revocation != c.want.Revocation ||
			!slices.Equal(got.Answers, c.want.Answers) {
			t.Errorf("%s: %+v %v, want %+v", name, got, err, c.want)
		}
	}
}

// The handler passes the fact on with the event's ID and origin; a payload that is not what TRD's
// events of its type carry is refused for good without asking the application; the application's
// failures are delivered again.
func TestFactsReachTheApplication(t *testing.T) {
	x := &tasks{}
	h := ProgressHandler(x)
	if err := h(context.Background(), fact(trdapi.EventQuoteActivated, "QuoteRoot", "quote.1", nil)); err != nil {
		t.Fatal(err)
	}
	if len(x.facts) != 1 || x.ids[0] != "evt-1" || x.origins[0] != "TRD quote_activated.v1 event evt-1" {
		t.Fatalf("%+v %v %v", x.facts, x.ids, x.origins)
	}
	if !api.Reason(application.ArrivalReason(x.facts[0].Answers[0], x.origins[0])) {
		t.Fatal("the resume reason is not a reason TSK's contract takes")
	}
	for name, e := range map[string]messaging.Event{
		"another aggregate":            fact(trdapi.EventQuoteActivated, "SalesOrder", "quote.1", nil),
		"another aggregate ID":         fact(trdapi.EventQuoteActivated, "QuoteRoot", "quote.2", nil),
		"no quote":                     fact(trdapi.EventQuoteActivated, "QuoteRoot", "quote.1", map[string]any{"quote_id": nil}),
		"no business revision":         fact(trdapi.EventQuoteActivated, "QuoteRoot", "quote.1", map[string]any{"business_revision": nil}),
		"a review not an identifier":   fact(trdapi.EventQuoteReviewDecided, "QuoteRoot", "quote.1", map[string]any{"review_request_id": "r 1"}),
		"a revocation without version": fact(trdapi.EventQuoteApprovalRevoked, "QuoteRoot", "quote.1", map[string]any{"approval_id": "a.1"}),
		"an order without revision": fact(trdapi.EventOrderActivated, "SalesOrder", "order.1",
			with(order, map[string]any{"order_revision": nil})),
		"an acceptance without its response": fact(trdapi.EventAcceptanceVerified, "Acceptance", "acceptance.1",
			map[string]any{"acceptance_id": "acceptance.1", "effectivity_version": 1}),
		"a payload that is not an object": {Consumer: TaskProgress, Envelope: eventing.Envelope{ID: "evt-1", Type: trdapi.EventQuoteActivated,
			Payload: json.RawMessage(`[1]`)}},
		"an event type not subscribed": fact(trdapi.EventQuoteFrozen, "QuoteRoot", "quote.1", nil),
	} {
		before := len(x.facts)
		err := h(context.Background(), e)
		if !errors.Is(err, messaging.ErrEventRejected) || !strings.Contains(err.Error(), string(FailureFactInvalid)) || len(x.facts) != before {
			t.Errorf("%s: %v", name, err)
		}
	}
	cause := errors.New("database down")
	if err := ProgressHandler(&tasks{err: cause})(context.Background(), fact(trdapi.EventQuoteActivated, "QuoteRoot", "quote.1", nil)); !errors.Is(err, cause) ||
		errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("an application failure: %v", err)
	}
}
