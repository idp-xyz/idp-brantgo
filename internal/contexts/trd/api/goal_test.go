package api

import (
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// GoalReadiness v1's query is closed: the two goals of baseline 15.4 at version 1, identifiers where
// identifiers are named.
func TestGoalQueryValidate(t *testing.T) {
	ok := GoalQuery{Goal: GoalOrderActive, GoalVersion: 1, QuoteID: "quote.1", BusinessRevision: 2, CustomerID: "customer.1", RfqID: "rfq.1"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for path, change := range map[string]func(q *GoalQuery){
		"/goal":              func(q *GoalQuery) { q.Goal = "quote_sent" },
		"/goal_version":      func(q *GoalQuery) { q.GoalVersion = 0 },
		"/quote_id":          func(q *GoalQuery) { q.QuoteID = "" },
		"/business_revision": func(q *GoalQuery) { q.BusinessRevision = -1 },
		"/customer_id":       func(q *GoalQuery) { q.CustomerID = "bad id" },
		"/rfq_id":            func(q *GoalQuery) { q.RfqID = "-rfq" },
	} {
		q := ok
		change(&q)
		rej, refused := kernel.AsRejection(q.Validate())
		if !refused || rej.Code != kernel.CodeSchemaInvalid || rej.Fields[0].Path != path {
			t.Errorf("%s: %v", path, q.Validate())
		}
	}
	none := GoalQuery{Goal: GoalQuoteReady, GoalVersion: GoalVersion, QuoteID: "quote.1"}
	if err := none.Validate(); err != nil {
		t.Fatalf("no revision and no target: %v", err)
	}
}

// Baseline 15.4: only every required assertion satisfied achieves a goal; missing data and an
// unavailable check are never a pass. Someone has to act on an unsatisfied assertion, so it decides
// the outcome over an error or an unknown one (TSK: waiting_input rather than waiting_external).
func TestOutcomeCombinesAnswers(t *testing.T) {
	s, u, k, e := AnswerSatisfied, AnswerUnsatisfied, AnswerUnknown, AnswerError
	for _, c := range []struct {
		answers       []string
		outcome, code string
	}{
		{[]string{s, s}, s, ""},
		{nil, k, CodeRequiredCheckUnknown},
		{[]string{s, k}, k, CodeRequiredCheckUnknown},
		{[]string{s, e, k}, e, CodeRequiredCheckUnknown},
		{[]string{k, u, e}, u, CodeRequiredCheckUnsatisfied},
		{[]string{u}, u, CodeRequiredCheckUnsatisfied},
	} {
		if o, code := Outcome(c.answers...); o != c.outcome || code != c.code {
			t.Errorf("%v: %s %s, want %s %s", c.answers, o, code, c.outcome, c.code)
		}
	}
	a := GoalAssessment{Assertions: []Assertion{{Assertion: AssertionScope, Answer: s}, {Assertion: AssertionOrder, Answer: u, Code: CodeOrderMissing},
		{Assertion: AssertionDocument, Answer: e}}}
	if got := a.Unmet(); !slices.Equal(got, []string{AssertionOrder, AssertionDocument}) {
		t.Fatalf("unmet %v", got)
	}
	if x, ok := a.Assertion(AssertionOrder); !ok || x.Code != CodeOrderMissing {
		t.Fatalf("Q01.order %+v %v", x, ok)
	}
	if _, ok := a.Assertion(AssertionAcceptance); ok {
		t.Fatal("an assertion the goal does not have")
	}
}

// The codes GoalReadiness answers are TRD's, with the error catalog's categories (ORDER_MISSING,
// REQUIRED_CHECK_UNSATISFIED and REQUIRED_CHECK_UNKNOWN: 422; CONVERSION_SLOT_CONFLICT: 409).
func TestGoalCodesAreDeclared(t *testing.T) {
	for code, want := range map[string]kernel.Category{CodeOrderMissing: kernel.CategoryInvalid, CodeRequiredCheckUnsatisfied: kernel.CategoryInvalid,
		CodeRequiredCheckUnknown: kernel.CategoryInvalid, CodeOrderNotActive: kernel.CategoryInvalid, CodeConversionSlotConflict: kernel.CategoryConflict} {
		if got, ok := Category(code); !ok || got != want {
			t.Errorf("%s: %v %v", code, got, ok)
		}
	}
}
