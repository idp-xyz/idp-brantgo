package task

import (
	"testing"
	"time"
)

// R18 TRD integration events v1, PL-04, PL-06, SM-TSK-TASK (waiting_* → running by task.resume on
// what arrived; running → verifying by task.verify), BC-AT25 (an ended task takes no forward action),
// unit B3: what a fact of TRD asks of a task, in every lifecycle state. Only running and waiting
// tasks move; a waiting task resumes on what it awaits (kind, identifier, the awaited version or a
// later one), on its goal becoming verifiable or on a revocation of an approval of its quote; a
// running or resumed task whose goal became verifiable starts its verification; a fact of another
// quote that answers nothing the task awaits does not concern it.
func TestWhatATRDFactAsksOfATask(t *testing.T) {
	quote := Ref{Kind: "quote", ID: "quote.1", Version: 2}
	review := Ref{Kind: "quote_review", ID: "review.1", Version: 2}
	order := Ref{Kind: "order", ID: "order.1", Version: 1}
	activated := Fact{QuoteID: "quote.1", Answers: []Ref{quote}, Goal: "quote_ready"}
	decided := Fact{QuoteID: "quote.1", Answers: []Ref{review, quote}}
	orderActive := Fact{QuoteID: "quote.1", Answers: []Ref{order, quote}, Goal: "order_active"}
	revoked := Fact{QuoteID: "quote.1", Answers: []Ref{{Kind: "quote_approval", ID: "approval.1", Version: 2}, quote}, Revocation: true}
	elsewhere := Fact{QuoteID: "quote.9", Answers: []Ref{{Kind: "order", ID: "order.9", Version: 1}, {Kind: "quote", ID: "quote.9", Version: 1}},
		Goal: "order_active"}

	task := func(goal string, state State, awaiting *Ref) *Task {
		t := &Task{ID: "task.1", Goal: goal, State: state, Target: Target{QuoteID: "quote.1"}}
		if state.Waiting() {
			t.WaitCond = &WaitCondition{Kind: WaitExternal, Awaiting: awaiting, Since: t0, DueAt: t0.Add(time.Hour)}
		}
		return t
	}
	for name, c := range map[string]struct {
		t    *Task
		f    Fact
		want Wake
	}{
		"running, its goal verifiable (PL-04)":   {task("quote_ready", Running, nil), activated, Wake{Verify: true}},
		"running, another goal's fact":           {task("order_active", Running, nil), activated, Wake{}},
		"running, an order goal (PL-06)":         {task("order_active", Running, nil), orderActive, Wake{Verify: true}},
		"running, a review decided":              {task("quote_ready", Running, nil), decided, Wake{}},
		"waiting for input, its goal verifiable": {task("quote_ready", WaitingInput, nil), activated, Wake{Resume: true, Arrival: quote, Verify: true}},
		"waiting for the review it awaits": {task("quote_ready", WaitingApproval, &Ref{Kind: "quote_review", ID: "review.1"}), decided,
			Wake{Resume: true, Arrival: review}},
		"waiting for a later version of the review": {task("quote_ready", WaitingApproval, &Ref{Kind: "quote_review", ID: "review.1", Version: 3}),
			decided, Wake{}},
		"waiting for the review at its version": {task("quote_ready", WaitingApproval, &Ref{Kind: "quote_review", ID: "review.1", Version: 2}),
			decided, Wake{Resume: true, Arrival: review}},
		"waiting for another review":   {task("quote_ready", WaitingApproval, &Ref{Kind: "quote_review", ID: "review.2"}), decided, Wake{}},
		"waiting for the quote itself": {task("order_active", WaitingExternal, &Ref{Kind: "quote", ID: "quote.1"}), decided, Wake{Resume: true, Arrival: quote}},
		"waiting for the order, its goal verifiable": {task("order_active", WaitingExternal, &Ref{Kind: "order", ID: "order.1"}), orderActive,
			Wake{Resume: true, Arrival: order, Verify: true}},
		"waiting, an approval of its quote revoked": {task("quote_ready", WaitingExternal, &Ref{Kind: "customer_response", ID: "r.1"}), revoked,
			Wake{Resume: true, Arrival: revoked.Answers[0]}},
		"waiting, another quote's order": {task("order_active", WaitingExternal, &Ref{Kind: "order", ID: "order.1"}), elsewhere, Wake{}},
		"waiting on another quote's order it awaits": {&Task{ID: "t", Goal: "order_active", State: WaitingExternal, Target: Target{QuoteID: "quote.1"},
			WaitCond: &WaitCondition{Kind: WaitExternal, Awaiting: &Ref{Kind: "order", ID: "order.9"}}}, elsewhere,
			Wake{Resume: true, Arrival: Ref{Kind: "order", ID: "order.9", Version: 1}}},
		"a fact that answers nothing": {task("quote_ready", WaitingInput, nil), Fact{QuoteID: "quote.1", Goal: "quote_ready"}, Wake{}},
	} {
		if got := c.t.WakeOn(c.f); got != c.want {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}

	// No other state moves, whatever the fact (BC-AT25 for the ended ones).
	for _, s := range []State{Created, Scoping, Verifying, Paused, Stopping, Completed, ClosedUnfulfilled, Cancelled, Failed} {
		tk := task("quote_ready", s, nil)
		tk.WaitCond = &WaitCondition{Kind: WaitApproval, Awaiting: &Ref{Kind: "quote_review", ID: "review.1"}}
		for _, f := range []Fact{activated, decided, revoked} {
			if got := tk.WakeOn(f); got != (Wake{}) {
				t.Errorf("%s: %+v on %+v", s, got, f)
			}
		}
	}

	// Concerns: the target quote, or an awaited object.
	other := &Task{Target: Target{QuoteID: "quote.2"}, State: WaitingExternal,
		WaitCond: &WaitCondition{Kind: WaitExternal, Awaiting: &Ref{Kind: "quote_review", ID: "review.1"}}}
	if !task("quote_ready", Running, nil).Concerns(activated) || !other.Concerns(decided) || other.Concerns(activated) ||
		(&Task{State: Running}).Concerns(activated) {
		t.Fatal("Concerns")
	}
}
