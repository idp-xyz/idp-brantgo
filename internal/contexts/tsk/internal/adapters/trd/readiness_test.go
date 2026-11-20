package trd

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/application"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TSK's port to TRD's GoalReadiness v1 (Context Map R18; WBS S4-01d) against a fake TRD api.

type fakeGoals struct {
	asked   []trdapi.GoalQuery
	quotes  []string
	answer  trdapi.GoalAssessment
	refuse  error
	view    trdapi.QuoteView
	refuseQ error
}

func (f *fakeGoals) GoalReadiness(_ context.Context, q trdapi.GoalQuery) (trdapi.GoalAssessment, error) {
	f.asked = append(f.asked, q)
	return f.answer, f.refuse
}

func (f *fakeGoals) GetQuote(_ context.Context, id string) (trdapi.QuoteView, error) {
	f.quotes = append(f.quotes, id)
	return f.view, f.refuseQ
}

var at = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

func trdAssessment() trdapi.GoalAssessment {
	return trdapi.GoalAssessment{Goal: trdapi.GoalOrderActive, GoalVersion: 1, QuoteID: "quote.1", RootVersion: 3, BusinessRevision: 1,
		BusinessHash: "h1", OrderID: "order.1", OrderRevision: 2, OrderHash: "h2", Outcome: trdapi.AnswerUnsatisfied,
		Code: trdapi.CodeRequiredCheckUnsatisfied, CheckedAt: at, Assertions: []trdapi.Assertion{
			{Assertion: trdapi.AssertionScope, Answer: trdapi.AnswerSatisfied, Checks: []trdapi.GoalCheck{{Check: "customer", Answer: trdapi.AnswerSatisfied}},
				Evidence: []trdapi.EvidenceRef{{Kind: trdapi.EvidenceQuoteRoot, ID: "quote.1", Version: 3}}},
			{Assertion: trdapi.AssertionOrder, Answer: trdapi.AnswerUnsatisfied, Code: trdapi.CodeOrderNotActive,
				Checks:   []trdapi.GoalCheck{{Check: trdapi.CheckOrder, Answer: trdapi.AnswerUnsatisfied, Code: trdapi.CodeOrderNotActive, Path: "/order/status"}},
				Evidence: []trdapi.EvidenceRef{{Kind: trdapi.EvidenceOrder, ID: "order.1", Version: 2, Hash: "h2"}}},
		}}
}

// The query is the task's goal at its version on its quote root with the target customer and RFQ
// (never a business revision: the goal judges the active one or the order's); the assessment is
// translated field for field; a bound root's control is read after it; refusals pass unchanged.
func TestReadinessAsksTRDForTheTasksGoal(t *testing.T) {
	f := &fakeGoals{answer: trdAssessment(), view: trdapi.QuoteView{Control: trdapi.ControlView{State: trdapi.ControlBound, TaskID: "task.1",
		ControlEpoch: 4}}}
	g := application.Goal{TaskID: "task.1", Code: "order_active", Version: 1, Target: task.Target{QuoteID: "quote.1", RfqID: "rfq.1",
		CustomerID: "customer.1"}}
	v, err := Readiness{TRD: f}.Assess(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	want := trdapi.GoalQuery{Goal: "order_active", GoalVersion: 1, QuoteID: "quote.1", CustomerID: "customer.1", RfqID: "rfq.1"}
	if len(f.asked) != 1 || f.asked[0] != want || len(f.quotes) != 0 || v.Control != nil {
		t.Fatalf("asked %+v, quotes %v, control %+v", f.asked, f.quotes, v.Control)
	}
	if v.Assessor != task.AssessorGoalReadiness || v.Goal != "order_active" || v.QuoteID != "quote.1" || v.RootVersion != 3 ||
		v.BusinessRevision != 1 || v.BusinessHash != "h1" || v.OrderID != "order.1" || v.OrderRevision != 2 || v.OrderHash != "h2" ||
		v.Outcome != task.Unsatisfied || v.Code != task.CodeRequiredCheckUnsatisfied || !v.CheckedAt.Equal(at) || len(v.Assertions) != 2 {
		t.Fatalf("%+v", v)
	}
	o := v.Assertions[1]
	if o.Assertion != task.AssertionOrder || o.Answer != task.Unsatisfied || o.Code != trdapi.CodeOrderNotActive ||
		o.Checks[0] != (task.Check{Check: trdapi.CheckOrder, Answer: task.Unsatisfied, Code: trdapi.CodeOrderNotActive, Path: "/order/status"}) ||
		o.Evidence[0] != (task.Evidence{Kind: task.EvidenceOrder, ID: "order.1", Version: 2, Hash: "h2"}) {
		t.Fatalf("%+v", o)
	}

	g.Bound = "quote.1"
	if v, err = (Readiness{TRD: f}).Assess(context.Background(), g); err != nil || !slices.Equal(f.quotes, []string{"quote.1"}) ||
		v.Control == nil || *v.Control != (task.RootControl{State: task.RootBound, TaskID: "task.1", Epoch: 4}) {
		t.Fatalf("%+v %v %v", v.Control, f.quotes, err)
	}
	for _, refusal := range []error{kernel.Reject(kernel.CodeNotFound, "/quote_id", ""), errors.New("connection reset")} {
		f := &fakeGoals{refuse: refusal}
		if _, err = (Readiness{TRD: f}).Assess(context.Background(), g); !errors.Is(err, refusal) || len(f.quotes) != 0 {
			t.Fatalf("%v", err)
		}
		f = &fakeGoals{answer: trdAssessment(), refuseQ: refusal}
		if _, err = (Readiness{TRD: f}).Assess(context.Background(), g); !errors.Is(err, refusal) {
			t.Fatalf("%v", err)
		}
	}
}

// TSK's terms are GoalReadiness v1's: the assertions each goal requires, the answers and their
// combination, the evidence kinds that name the records judged, the control states of a root that
// a task holds, and the outcome codes.
func TestTSKSpeaksGoalReadinessV1(t *testing.T) {
	answers := []string{trdapi.AnswerSatisfied, trdapi.AnswerUnsatisfied, trdapi.AnswerUnknown, trdapi.AnswerError}
	if !slices.Equal(answers, []string{task.Satisfied, task.Unsatisfied, task.Unknown, task.Error}) {
		t.Fatal("answers differ")
	}
	var combos [][]string
	var walk func(prefix []string, n int)
	walk = func(prefix []string, n int) {
		combos = append(combos, slices.Clone(prefix))
		if n == 0 {
			return
		}
		for _, a := range answers {
			walk(append(prefix, a), n-1)
		}
	}
	walk(nil, 3)
	for _, c := range combos {
		o1, c1 := trdapi.Outcome(c...)
		o2, c2 := task.Outcome(c...)
		if o1 != o2 || c1 != c2 {
			t.Errorf("%v: TRD %s %s, TSK %s %s", c, o1, c1, o2, c2)
		}
	}
	quoteReady := []string{trdapi.AssertionScope, trdapi.AssertionQuote, trdapi.AssertionLines, trdapi.AssertionApproval,
		trdapi.AssertionDocument, trdapi.AssertionIndustry}
	if !slices.Equal(task.RequiredAssertions(trdapi.GoalQuoteReady, trdapi.GoalVersion), quoteReady) ||
		!slices.Equal(task.RequiredAssertions(trdapi.GoalOrderActive, trdapi.GoalVersion), append(quoteReady, trdapi.AssertionAcceptance,
			trdapi.AssertionOrder)) {
		t.Fatal("required assertions differ")
	}
	if task.EvidenceQuoteRoot != trdapi.EvidenceQuoteRoot || task.EvidenceQuoteRevision != trdapi.EvidenceQuoteRevision ||
		task.EvidenceDocument != trdapi.EvidenceDocument || task.EvidenceOrder != trdapi.EvidenceOrder ||
		task.RootBound != trdapi.ControlBound || task.RootManual != trdapi.ControlManual ||
		task.CodeRequiredCheckUnsatisfied != trdapi.CodeRequiredCheckUnsatisfied || task.CodeRequiredCheckUnknown != trdapi.CodeRequiredCheckUnknown {
		t.Fatal("evidence kinds, control states or codes differ")
	}
}
