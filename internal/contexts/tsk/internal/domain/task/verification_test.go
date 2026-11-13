package task

import (
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Goal verification in the domain (WBS S4-01d; SM-TSK-TASK's goal.verify rows; TSK-INV-001,
// TSK-INV-003; baseline 15.4; BC-AT25, AN11, CT13).

var (
	hashA = strings.Repeat("a", 64)
	hashB = strings.Repeat("b", 64)
)

// verifying returns a task of goal in verifying, with quote.1 as its target (bound at epoch 1 in
// mode ai when bound).
func verifying(t *testing.T, goal string, mode Mode, bound bool) *Task {
	t.Helper()
	tk := running(t, mode)
	tk.Goal = goal
	sys := Act{By: tskSys, Epoch: 1, At: t0}
	tk.Target.QuoteID = "quote.1"
	if bound {
		ok(t)(tk.BindObject("quote.1", false, 1, sys))
	}
	ok(t)(tk.Verify(0, sys))
	return tk
}

// assessed is TRD's assessment of goal on quote.1 with every required assertion satisfied, its
// evidence naming root version 3, revision 1 at hashA and, for order_active, order.1 at revision 2
// and hashB; answers sets assertions to (answer, code) pairs.
func assessed(goal string, answers map[string][2]string) Assessment {
	a := Assessment{Assessor: AssessorGoalReadiness, Goal: goal, GoalVersion: 1, QuoteID: "quote.1", RootVersion: 3, BusinessRevision: 1,
		BusinessHash: hashA, CheckedAt: t0}
	if goal == "order_active" {
		a.OrderID, a.OrderRevision, a.OrderHash = "order.1", 2, hashB
	}
	for _, name := range RequiredAssertions(goal, 1) {
		c := Check{Check: "c", Answer: Satisfied}
		if x, ok := answers[name]; ok {
			c.Answer, c.Code, c.Path = x[0], x[1], "/data"
		}
		var ev []Evidence
		switch name {
		case AssertionScope:
			ev = []Evidence{{Kind: EvidenceQuoteRoot, ID: "quote.1", Version: 3}}
		case AssertionQuote:
			ev = []Evidence{{Kind: EvidenceQuoteRevision, ID: "quote.1", Version: 1, Hash: hashA}, {Kind: "commit_evidence", ID: "k", Version: 1}}
		case AssertionDocument:
			ev = []Evidence{{Kind: EvidenceDocument, ID: "QT-1", Version: 1, Hash: hashB}}
		case AssertionOrder:
			ev = []Evidence{{Kind: "conversion_slot", ID: "order.1", Version: 1, Hash: hashA}, {Kind: EvidenceOrder, ID: "order.1", Version: 2, Hash: hashB}}
		}
		a.Assertions = append(a.Assertions, newAssertion(name, ev, c))
	}
	answersOf := make([]string, len(a.Assertions))
	for i, x := range a.Assertions {
		answersOf[i] = x.Answer
	}
	a.Outcome, a.Code = Outcome(answersOf...)
	return a
}

func sysAct() Act { return Act{By: tskSys, Epoch: 1, At: t0} }

func names(v Verification) []string {
	var out []string
	for _, x := range v.Assertions {
		out = append(out, x.Assertion+"="+x.Answer)
	}
	return out
}

// Outcome is GoalReadiness v1's combination: satisfied only when every answer is and there is one;
// unsatisfied before error before unknown.
func TestOutcomeCombination(t *testing.T) {
	for _, c := range []struct {
		answers       []string
		outcome, code string
	}{
		{nil, Unknown, CodeRequiredCheckUnknown},
		{[]string{Satisfied}, Satisfied, ""},
		{[]string{Satisfied, Unknown}, Unknown, CodeRequiredCheckUnknown},
		{[]string{Unknown, Error}, Error, CodeRequiredCheckUnknown},
		{[]string{Error, Unsatisfied, Unknown}, Unsatisfied, CodeRequiredCheckUnsatisfied},
	} {
		if o, code := Outcome(c.answers...); o != c.outcome || code != c.code {
			t.Errorf("%v: %s %s", c.answers, o, code)
		}
	}
	if got := RequiredAssertions("order_active", 1); len(got) != 8 || got[7] != AssertionOrder {
		t.Fatalf("%v", got)
	}
	if RequiredAssertions("quote_ready", 2) != nil || RequiredAssertions("quote_sent", 1) != nil {
		t.Fatal("an unknown goal or version has required assertions")
	}
}

// TSK-INV-001, Q01 V0.1 tests/test_execution.py::test_local_quote_to_active_order_and_truthful_completion:
// every required assertion satisfied and no unsettled action completes the task, with
// ai_with_required_review in mode ai and manual in manual mode; the verification keeps TRD's
// assertions, Q01.execution last (unsettled_actions and snapshot satisfied), the records judged and
// the state reached.
func TestASatisfiedGoalCompletesTheTask(t *testing.T) {
	for _, mode := range []Mode{ModeAI, ModeManual} {
		tk := verifying(t, "order_active", mode, false)
		ch, v, err := tk.RecordVerification(assessed("order_active", nil), 0, sysAct())
		must(t, ch, err)
		want := map[Mode]string{ModeAI: CompletionAIWithRequiredReview, ModeManual: CompletionManual}[mode]
		if tk.State != Completed || tk.CompletionActor != want || ch.To != Completed || ch.Command != CmdVerifyGoal || tk.WaitCond != nil {
			t.Fatalf("%s: %+v", mode, tk)
		}
		x := v.Assertions[len(v.Assertions)-1]
		if v.Outcome != Satisfied || v.Code != "" || len(v.Unmet) != 0 || v.To != Completed || v.TaskVersion != tk.Version ||
			len(v.Assertions) != 9 || x.Assertion != AssertionExecution || x.Answer != Satisfied || len(x.Checks) != 2 ||
			v.OrderID != "order.1" || v.OrderRevision != 2 || v.BusinessRevision != 1 || v.RecordedAt != t0 || v.TaskID != "task.1" {
			t.Fatalf("%s: %+v", mode, v)
		}
	}
}

// SM-TSK-TASK goal.verify rows and AN11, CT13: an unsatisfied assertion sends the task to
// waiting_input, an unknown or failed one to waiting_external; unsatisfied comes first. The unmet
// assertions are the wait's items; its reason names each with its answer and code and how the task
// goes on (CT13 展示具体原因及恢复条件); the owner is responsible.
func TestAnUnmetGoalSendsTheTaskBackToWait(t *testing.T) {
	for _, c := range []struct {
		name    string
		answers map[string][2]string
		to      State
		unmet   []string
		reason  []string
	}{
		{"AN11 a quote without the order", map[string][2]string{AssertionOrder: {Unsatisfied, "ORDER_MISSING"},
			AssertionAcceptance: {Unsatisfied, "ORDER_MISSING"}}, WaitingInput, []string{AssertionAcceptance, AssertionOrder},
			[]string{"not met", "Q01.order unsatisfied ORDER_MISSING", "The owner deals with them"}},
		{"CT13 industry unknown", map[string][2]string{AssertionIndustry: {Unknown, "DEPENDENCY_NOT_SUPPORTED"}}, WaitingExternal,
			[]string{AssertionIndustry}, []string{"not decided", "Q01.industry unknown DEPENDENCY_NOT_SUPPORTED", "once the checks can answer"}},
		{"CT13 industry error", map[string][2]string{AssertionIndustry: {Error, "FORMAL_DOCUMENT_INTEGRITY_FAILURE"}}, WaitingExternal,
			[]string{AssertionIndustry}, []string{"Q01.industry error FORMAL_DOCUMENT_INTEGRITY_FAILURE"}},
		{"unsatisfied before error", map[string][2]string{AssertionDocument: {Error, "X"}, AssertionOrder: {Unsatisfied, "ORDER_NOT_ACTIVE"}},
			WaitingInput, []string{AssertionDocument, AssertionOrder}, []string{"Q01.document error X; Q01.order unsatisfied ORDER_NOT_ACTIVE"}},
	} {
		tk := verifying(t, "order_active", ModeAI, false)
		ch, v, err := tk.RecordVerification(assessed("order_active", c.answers), 0, sysAct())
		must(t, ch, err)
		if tk.State != c.to || tk.CompletionActor != "" || !slices.Equal(tk.WaitCond.Items, c.unmet) || !slices.Equal(v.Unmet, c.unmet) ||
			tk.WaitCond.Responsible != "alice" || tk.WaitCond.Reason != ch.Reason || v.To != c.to {
			t.Fatalf("%s: %+v %+v", c.name, tk, v)
		}
		for _, s := range c.reason {
			if !strings.Contains(ch.Reason, s) {
				t.Errorf("%s: reason %q lacks %q", c.name, ch.Reason, s)
			}
		}
		if want := map[State]string{WaitingInput: CodeRequiredCheckUnsatisfied, WaitingExternal: CodeRequiredCheckUnknown}[c.to]; v.Code != want {
			t.Errorf("%s: code %s", c.name, v.Code)
		}
	}
}

// TSK-INV-003 and baseline 15.4 Q01.execution: TRD's satisfied goal does not complete a task with an
// action still submitted or unknown; Q01.execution is unknown (EXTERNAL_RESULT_UNKNOWN) and the task
// waits for the external result.
func TestAnUnsettledActionKeepsTheGoalUndecided(t *testing.T) {
	tk := verifying(t, "quote_ready", ModeAI, false)
	ch, v, err := tk.RecordVerification(assessed("quote_ready", nil), 2, sysAct())
	must(t, ch, err)
	x := v.Assertions[len(v.Assertions)-1]
	if tk.State != WaitingExternal || v.Outcome != Unknown || !slices.Equal(v.Unmet, []string{AssertionExecution}) || v.Unsettled != 2 ||
		x.Code != CodeExternalResultUnknown || x.Checks[0].Answer != Unknown || !strings.Contains(ch.Reason, "Q01.execution unknown EXTERNAL_RESULT_UNKNOWN") {
		t.Fatalf("%+v %+v", tk, v)
	}
}

// SM-TSK-TASK "快照版本仍匹配": the evidence of TRD's assessment must name the records it judged — an
// order, revision, document or root at another version or hash makes Q01.execution's snapshot check
// an error, and the goal is not decided; other evidence kinds (the conversion slot, the commit
// evidence) carry versions of their own.
func TestEvidenceOfAnotherSnapshotIsNoPass(t *testing.T) {
	for name, change := range map[string]func(*Assessment){
		"order revision":  func(a *Assessment) { a.OrderRevision = 3 },
		"order hash":      func(a *Assessment) { a.OrderHash = hashA },
		"revision hash":   func(a *Assessment) { a.BusinessHash = hashB },
		"document":        func(a *Assessment) { a.Assertions[4].Evidence[0].Version = 2 },
		"root version":    func(a *Assessment) { a.RootVersion = 4 },
		"revision number": func(a *Assessment) { a.BusinessRevision = 2 },
	} {
		tk := verifying(t, "order_active", ModeAI, false)
		a := assessed("order_active", nil)
		change(&a)
		ch, v, err := tk.RecordVerification(a, 0, sysAct())
		must(t, ch, err)
		x := v.Assertions[len(v.Assertions)-1]
		if tk.State != WaitingExternal || v.Outcome != Error || x.Answer != Error || x.Checks[1].Check != CheckSnapshot ||
			x.Checks[1].Code != kernel.CodeVersionConflict || !strings.HasPrefix(x.Checks[1].Path, "/assertions/") {
			t.Errorf("%s: %+v %+v", name, tk.State, x)
		}
	}
}

// An answer that is not an assessment of this task's goal is no verification: TRD's assessment of
// another goal, goal version or quote, without a required assertion or with one the goal does not
// have, with answers its checks do not give, or an outcome its assertions do not give; TSK's own
// assessment never satisfied. Each is an error, and the task is unchanged.
func TestAnAssessmentOfAnotherGoalIsRefused(t *testing.T) {
	for name, change := range map[string]func(*Assessment){
		"another goal":      func(a *Assessment) { a.Goal = "quote_ready" },
		"another version":   func(a *Assessment) { a.GoalVersion = 2 },
		"another quote":     func(a *Assessment) { a.QuoteID = "quote.2" },
		"no instant":        func(a *Assessment) { a.CheckedAt = time.Time{} },
		"quote_ready's set": func(a *Assessment) { a.Assertions = a.Assertions[:6] },
		"Q01.execution by TRD": func(a *Assessment) {
			a.Assertions = append(a.Assertions, newAssertion(AssertionExecution, nil, Check{Answer: Satisfied}))
		},
		"assertion vs checks": func(a *Assessment) { a.Assertions[7].Answer = Unknown },
		"outcome vs assertions": func(a *Assessment) {
			a.Outcome, a.Code = Unknown, CodeRequiredCheckUnknown
		},
		"unknown answer":   func(a *Assessment) { a.Assertions[0].Checks[0].Answer = "pass" },
		"unknown assessor": func(a *Assessment) { a.Assessor = "model" },
		"TSK satisfied":    func(a *Assessment) { a.Assessor = AssessorTSK },
	} {
		tk := verifying(t, "order_active", ModeAI, false)
		before := *tk
		a := assessed("order_active", nil)
		change(&a)
		_, _, err := tk.RecordVerification(a, 0, sysAct())
		if _, refused := kernel.AsRejection(err); err == nil || refused || tk.State != Verifying || tk.Version != before.Version {
			t.Errorf("%s: %v %+v", name, err, tk.State)
		}
	}
}

// TSK judges a goal itself only to find it not met: a task that names no quote root (Q01.quote) and
// a root TRD does not show the verifying actor (Q01.scope with TRD's refusal) go to waiting_input.
func TestLocalAssessmentsNeverPass(t *testing.T) {
	tk := running(t, ModeManual)
	ok(t)(tk.Verify(0, sysAct()))
	ch, v, err := tk.RecordVerification(tk.LocalAssessment("", t0), 0, sysAct())
	must(t, ch, err)
	if tk.State != WaitingInput || v.Assessor != AssessorTSK || !slices.Equal(v.Unmet, []string{AssertionQuote}) ||
		v.Assertions[0].Checks[0].Code != kernel.CodeReferenceInvalid || v.Assertions[0].Checks[0].Path != "/target/quote_id" ||
		len(v.Assertions[1].Checks) != 1 || v.QuoteID != "" {
		t.Fatalf("%+v %+v", tk, v)
	}
	for _, refusal := range []string{kernel.CodeNotFound, kernel.CodePermissionDenied} {
		tk := verifying(t, "quote_ready", ModeAI, true)
		ch, v, err := tk.RecordVerification(tk.LocalAssessment(refusal, t0), 0, sysAct())
		must(t, ch, err)
		if tk.State != WaitingInput || !slices.Equal(names(v), []string{"Q01.scope=unsatisfied", "Q01.execution=satisfied"}) ||
			v.Assertions[0].Code != refusal || v.QuoteID != "quote.1" || !strings.Contains(ch.Reason, "Q01.scope unsatisfied "+refusal) {
			t.Fatalf("%s: %+v %+v", refusal, tk, v)
		}
	}
}

// SM-TSK-TASK "快照版本仍匹配且无冲突", TSK-INV-002: for a task with a bound root, TRD's control of the
// root must be the task's at the epoch it recorded, bound (mode ai) or manual; a stop TRD committed
// that the task has not recorded (a take-over under way), another task, or no control shown refuses
// goal.verify with CONTROL_EPOCH_STALE and leaves the task as it was, so the take-over is recorded
// first. A task without a bound root needs no control.
func TestABoundRootMustBeTheTasksAtItsEpoch(t *testing.T) {
	for name, c := range map[string]*RootControl{
		"no control":      nil,
		"stopped":         {State: "stopping", TaskID: "task.1", Epoch: 2},
		"a later epoch":   {State: RootBound, TaskID: "task.1", Epoch: 2},
		"another task":    {State: RootBound, TaskID: "task.2", Epoch: 1},
		"unbound":         {State: "unbound", TaskID: "", Epoch: 1},
		"stopping at one": {State: "stopping", TaskID: "task.1", Epoch: 1},
	} {
		tk := verifying(t, "quote_ready", ModeAI, true)
		a := assessed("quote_ready", nil)
		a.Control = c
		if _, _, err := tk.RecordVerification(a, 0, sysAct()); code(err) != CodeControlEpochStale || tk.State != Verifying || tk.Version != 5 {
			t.Errorf("%s: %s %s v%d", name, code(err), tk.State, tk.Version)
		}
	}
	tk := verifying(t, "quote_ready", ModeAI, true)
	a := assessed("quote_ready", nil)
	a.Control = &RootControl{State: RootBound, TaskID: "task.1", Epoch: 1}
	ch, v, err := tk.RecordVerification(a, 0, sysAct())
	must(t, ch, err)
	if tk.State != Completed || v.Control == nil || v.Control.Epoch != 1 {
		t.Fatalf("%+v %+v", tk, v)
	}
	// Under manual control after a take-over (TRD manual at the epoch of enter_manual).
	m := verifying(t, "quote_ready", ModeAI, true)
	m.Mode, m.ControlEpoch, m.Binding.Epoch = ModeManual, 3, 3
	a.Control = &RootControl{State: RootManual, TaskID: "task.1", Epoch: 3}
	if _, _, err = m.RecordVerification(a, 0, sysAct()); err != nil || m.State != Completed || m.CompletionActor != CompletionManual {
		t.Fatalf("%v %+v", err, m)
	}
	// Unbound: TRD's control is not consulted.
	u := verifying(t, "quote_ready", ModeManual, false)
	if _, _, err = u.RecordVerification(assessed("quote_ready", nil), 0, sysAct()); err != nil || u.State != Completed {
		t.Fatalf("%v %+v", err, u)
	}
}

// goal.verify applies to a verifying task, by a system actor only; a terminal task takes none (Q01
// V0.1 tests/test_execution.py::test_terminal_task_rejects_new_forward_action, BC-AT25).
func TestRecordVerificationFollowsSMTSKTASK(t *testing.T) {
	tk := running(t, ModeAI)
	tk.Target.QuoteID = "quote.1"
	if _, _, err := tk.RecordVerification(assessed("order_active", nil), 0, sysAct()); code(err) != kernel.CodeInvalidState {
		t.Fatalf("running: %s", code(err))
	}
	tk = verifying(t, "order_active", ModeAI, false)
	if _, _, err := tk.RecordVerification(assessed("order_active", nil), 0, Act{By: alice, Epoch: 1, At: t0}); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("a human: %s", code(err))
	}
	ok(t)(func() (Change, error) {
		ch, _, err := tk.RecordVerification(assessed("order_active", nil), 0, sysAct())
		return ch, err
	}())
	if _, _, err := tk.RecordVerification(assessed("order_active", nil), 0, sysAct()); code(err) != kernel.CodeInvalidState || tk.State != Completed {
		t.Fatalf("completed again: %s", code(err))
	}
}

// The wait's reason fits brant_tsk's 1000 characters, however many assertions and codes it names.
func TestVerificationReasonIsBounded(t *testing.T) {
	var as []Assertion
	for i := 0; i < 40; i++ {
		as = append(as, newAssertion("Q01.x"+strings.Repeat("y", 30), nil, Check{Answer: Unknown, Code: strings.Repeat("C", 60)}))
	}
	r := verificationReason("order_active", Unknown, as)
	if n := utf8.RuneCountInString(r); n != 1000 || !strings.HasSuffix(r, "…") {
		t.Fatalf("%d characters", n)
	}
}
