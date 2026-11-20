package task

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Goal verification (goal.verify; SM-TSK-TASK verifying → completed | waiting_input |
// waiting_external; TSK-INV-001; tactical model BusinessTask entity OutcomeEvidence; baseline 8.5
// OutcomeEvidence and 15.4 完成断言; WBS S4-01d).
//
// TRD judges the assertions of a goal on its authoritative records (GoalReadiness v1, Context Map
// R18) and TSK records the judgement with the task: every goal.verify leaves one Verification —
// the records judged (the quote root at its version, the business revision at its hash, the order
// at its revision and hash), each assertion with its checks and evidence, the instant TRD judged
// and the state the task moved to. TSK adds the assertion it owns, Q01.execution (baseline 15.4:
// no unsettled side effect and no conflicting action): no action of the task is submitted or
// unknown (TSK-INV-003), and the evidence of the assessment names the records it says it judged
// (the snapshot). The task completes only when every required assertion of its goal, Q01.execution
// included, is satisfied; an unsatisfied one sends it to waiting_input, an unknown or failed one to
// waiting_external, with the unmet assertions as the items and a reason naming each with its code
// and how the task goes on. completed is the fact that the goal was achieved when it was judged
// (baseline 15.4); what happens to the quote or order afterwards does not undo it (BC-AT25).
//
// A verification judges the task as it is (task_version CAS between TRD's read and TSK's record)
// and, for a task with a bound root, TRD's control of that root as TRD showed it: unless it is the
// task's at the control epoch the task recorded, a control change is under way (a take-over whose
// TRD step committed before the task recorded it), and goal.verify is refused with
// CONTROL_EPOCH_STALE so the take-over is recorded first (SM-TSK-TASK "快照版本仍匹配且无冲突").

// Who judged a goal: TRD's GoalReadiness v1, or TSK itself when TRD could not be asked (the task
// names no quote root) or refused the read.
const (
	AssessorGoalReadiness = "trd.goal_readiness.v1"
	AssessorTSK           = "tsk"
)

// Assertions TSK names itself (baseline 15.4). The others are GoalReadiness v1's.
const (
	AssertionScope      = "Q01.scope"
	AssertionQuote      = "Q01.quote"
	AssertionLines      = "Q01.lines"
	AssertionApproval   = "Q01.approval"
	AssertionDocument   = "Q01.document"
	AssertionIndustry   = "Q01.industry"
	AssertionAcceptance = "Q01.acceptance"
	AssertionOrder      = "Q01.order"
	AssertionExecution  = "Q01.execution"
)

// Checks TSK makes: Q01.execution's (unsettled_actions, snapshot), and the one it records when TRD
// was not asked (quote_root) or refused the read (read).
const (
	CheckUnsettled = "unsettled_actions"
	CheckSnapshot  = "snapshot"
	CheckQuoteRoot = "quote_root"
	CheckRead      = "read"
)

// Kinds of evidence whose version and hash name the records an assessment judged (GoalReadiness v1).
const (
	EvidenceQuoteRoot     = "quote_root"
	EvidenceQuoteRevision = "quote_revision"
	EvidenceDocument      = "formal_document"
	EvidenceOrder         = "order"
)

// The control state of a root TRD shows for a task that holds it (SM-TRD-CONTROL).
const (
	RootBound  = "bound"
	RootManual = "manual"
)

// Codes of a verification's outcome (error catalog 任务与 AI, contexts TSK／TRD): the goal is not
// met (an assertion unsatisfied) or not decided (unknown or error); never a pass.
const (
	CodeRequiredCheckUnsatisfied = "REQUIRED_CHECK_UNSATISFIED"
	CodeRequiredCheckUnknown     = "REQUIRED_CHECK_UNKNOWN"
)

// required are the assertions GoalReadiness v1 answers for each goal at version 1, in its order
// (baseline 15.4; GoalSpec 完成断言). Q01.execution, TSK's own, is required of both.
var required = map[string][]string{
	"quote_ready":  {AssertionScope, AssertionQuote, AssertionLines, AssertionApproval, AssertionDocument, AssertionIndustry},
	"order_active": {AssertionScope, AssertionQuote, AssertionLines, AssertionApproval, AssertionDocument, AssertionIndustry, AssertionAcceptance, AssertionOrder},
}

// RequiredAssertions returns the assertions GoalReadiness v1 answers for goal at version, in order,
// or nil for a goal TSK does not know.
func RequiredAssertions(goal string, version int64) []string {
	if version != 1 {
		return nil
	}
	return slices.Clone(required[goal])
}

// Evidence is a record an assertion rests on: its kind, ID, version and hash.
type Evidence struct {
	Kind    string
	ID      string
	Version int64
	Hash    string
}

// Check is one check of an assertion: its answer and, when not satisfied, the code and the path it
// points at.
type Check struct {
	Check  string
	Answer string
	Code   string
	Path   string
}

// Assertion is the answer for one assertion: its checks combined (Outcome), the code of its first
// check giving that answer, the checks in the order made and the evidence judged.
type Assertion struct {
	Assertion string
	Answer    string
	Code      string
	Checks    []Check
	Evidence  []Evidence
}

// RootControl is TRD's object control of the task's bound root as TRD showed it after judging the
// goal: its state, the task holding it and its control epoch.
type RootControl struct {
	State  string
	TaskID string
	Epoch  int64
}

// Assessment is a goal judged: who judged it (Assessor*), the goal and its version, the records
// judged (zero where there is none), the outcome over the assertions with its code, the assertions
// in order, the instant judged, and for a task with a bound root, TRD's control of that root.
type Assessment struct {
	Assessor         string
	Goal             string
	GoalVersion      int64
	QuoteID          string
	RootVersion      int64
	BusinessRevision int64
	BusinessHash     string
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	Outcome          string
	Code             string
	Assertions       []Assertion
	CheckedAt        time.Time
	Control          *RootControl
}

// Verification is one goal verification as the task records it (OutcomeEvidence): the assessment
// with Q01.execution appended and its outcome and code taken over every assertion, the unmet
// assertions, the actions that were submitted or unknown, the state the task moved to and its
// version then, and when TSK recorded it.
type Verification struct {
	TaskID      string
	TaskVersion int64
	Assessment
	Unmet      []string
	Unsettled  int
	To         State
	RecordedAt time.Time
}

// Outcome combines answers as GoalReadiness v1 does: satisfied when every one is (and there is at
// least one); else unsatisfied when one is — someone has to act —, else error when one is, else
// unknown; with the goal's code for it ("" when satisfied).
func Outcome(answers ...string) (outcome, code string) {
	seen := map[string]bool{}
	for _, a := range answers {
		seen[a] = true
	}
	switch {
	case len(answers) == 0:
		return Unknown, CodeRequiredCheckUnknown
	case seen[Unsatisfied]:
		return Unsatisfied, CodeRequiredCheckUnsatisfied
	case seen[Error]:
		return Error, CodeRequiredCheckUnknown
	case seen[Unknown]:
		return Unknown, CodeRequiredCheckUnknown
	}
	return Satisfied, ""
}

func answer(s string) bool { return s == Satisfied || s == Unsatisfied || s == Unknown || s == Error }

// newAssertion builds an assertion from its checks and evidence: its answer is the checks' Outcome,
// its code the first check's that gave it.
func newAssertion(name string, evidence []Evidence, checks ...Check) Assertion {
	a := Assertion{Assertion: name, Checks: checks, Evidence: evidence}
	answers := make([]string, len(checks))
	for i, c := range checks {
		answers[i] = c.Answer
	}
	a.Answer, _ = Outcome(answers...)
	for _, c := range checks {
		if c.Answer == a.Answer && a.Answer != Satisfied {
			a.Code = c.Code
			break
		}
	}
	return a
}

// LocalAssessment is the assessment TSK makes when TRD does not judge the goal: the task names no
// quote root (refusal ""; Q01.quote unsatisfied, REFERENCE_INVALID at /target/quote_id), or TRD
// refused to show it the root with refusal — NOT_FOUND or PERMISSION_DENIED: Q01.scope unsatisfied
// (baseline 15.4 Q01.scope: the records belong to the task's scope and the executing identity may
// read them). TSK never finds a goal achieved by itself.
func (t *Task) LocalAssessment(refusal string, at time.Time) Assessment {
	a := Assessment{Assessor: AssessorTSK, Goal: t.Goal, GoalVersion: t.GoalVersion, QuoteID: t.Target.QuoteID, CheckedAt: kernel.Instant(at)}
	if refusal == "" {
		a.Assertions = []Assertion{newAssertion(AssertionQuote, nil,
			Check{Check: CheckQuoteRoot, Answer: Unsatisfied, Code: kernel.CodeReferenceInvalid, Path: "/target/quote_id"})}
	} else {
		a.Assertions = []Assertion{newAssertion(AssertionScope, []Evidence{{Kind: EvidenceQuoteRoot, ID: t.Target.QuoteID}},
			Check{Check: CheckRead, Answer: Unsatisfied, Code: refusal, Path: "/target/quote_id"})}
	}
	a.Outcome, a.Code = Unsatisfied, CodeRequiredCheckUnsatisfied
	return a
}

// judges checks that a is an assessment of the task's goal TSK can record: TRD's names the task's
// goal, goal version and quote root and answers exactly the goal's required assertions, each
// combined from its checks, with the outcome they give; TSK's own never satisfies a goal. A
// different answer is not a verification of this task: an error, and nothing is recorded.
func (t *Task) judges(a Assessment) error {
	bad := func(reason string) error { return errors.New("tsk: goal assessment " + reason) }
	if a.Goal != t.Goal || a.GoalVersion != t.GoalVersion || a.QuoteID != t.Target.QuoteID || a.CheckedAt.IsZero() || len(a.Assertions) == 0 {
		return bad("for another goal or quote, or without assertions")
	}
	names := make([]string, len(a.Assertions))
	answers := make([]string, len(a.Assertions))
	for i, x := range a.Assertions {
		checks := make([]string, len(x.Checks))
		for j, c := range x.Checks {
			if !answer(c.Answer) {
				return bad("check answer " + c.Answer)
			}
			checks[j] = c.Answer
		}
		if got, _ := Outcome(checks...); got != x.Answer {
			return bad(x.Assertion + " answers " + x.Answer + ", its checks " + got)
		}
		names[i], answers[i] = x.Assertion, x.Answer
	}
	outcome, code := Outcome(answers...)
	if outcome != a.Outcome || code != a.Code {
		return bad("outcome " + a.Outcome + ", its assertions " + outcome)
	}
	switch a.Assessor {
	case AssessorGoalReadiness:
		if t.Target.QuoteID == "" || !slices.Equal(names, required[t.Goal]) || t.GoalVersion != 1 {
			return bad("without the required assertions of " + t.Goal)
		}
	case AssessorTSK:
		if outcome == Satisfied || slices.Contains(names, AssertionExecution) {
			return bad("by TSK satisfied")
		}
	default:
		return bad("by " + a.Assessor)
	}
	return nil
}

// snapshot checks that the evidence of TRD's assessment names the records it judged: the quote root
// at the judged root version, the revision at the judged business revision and hash, the formal
// document of that revision, the order at the judged order revision and hash.
func (a Assessment) snapshot() Check {
	c := Check{Check: CheckSnapshot, Answer: Satisfied}
	for i, x := range a.Assertions {
		for j, e := range x.Evidence {
			var ok bool
			switch e.Kind {
			case EvidenceQuoteRoot:
				ok = e.ID == a.QuoteID && e.Version == a.RootVersion
			case EvidenceQuoteRevision:
				ok = e.ID == a.QuoteID && e.Version == a.BusinessRevision && e.Hash == a.BusinessHash
			case EvidenceDocument:
				ok = e.Version == a.BusinessRevision
			case EvidenceOrder:
				ok = e.ID == a.OrderID && e.Version == a.OrderRevision && e.Hash == a.OrderHash
			default:
				continue
			}
			if !ok {
				return Check{Check: CheckSnapshot, Answer: Error, Code: kernel.CodeVersionConflict,
					Path: "/assertions/" + strconv.Itoa(i) + "/evidence/" + strconv.Itoa(j)}
			}
		}
	}
	return c
}

// execution is Q01.execution, TSK's own assertion: no action of the task is submitted or unknown
// (unknown, EXTERNAL_RESULT_UNKNOWN), and TRD's assessment names one snapshot (error,
// VERSION_CONFLICT otherwise).
func (a Assessment) execution(unsettled int) Assertion {
	actions := Check{Check: CheckUnsettled, Answer: Satisfied}
	if unsettled > 0 {
		actions = Check{Check: CheckUnsettled, Answer: Unknown, Code: CodeExternalResultUnknown, Path: "/args/task_id"}
	}
	checks := []Check{actions}
	if a.Assessor == AssessorGoalReadiness {
		checks = append(checks, a.snapshot())
	}
	return newAssertion(AssertionExecution, nil, checks...)
}

// controls checks that TRD's control of the task's bound root is the task's at the epoch the task
// recorded, bound or under manual control.
func (t *Task) controls(c *RootControl) error {
	if c != nil && c.TaskID == t.ID && c.Epoch == t.ControlEpoch && (c.State == RootBound || c.State == RootManual) {
		return nil
	}
	return fail(CodeControlEpochStale, "/args/task_id",
		"TRD's control of the bound quote root is not the task's at the epoch it recorded: a control change is under way")
}

// maxReason is the longest reason a task records (characters; brant_tsk checks it).
const maxReason = 1000

// verificationReason tells why the goal is not met and how the task goes on: each unmet assertion
// with its answer and code (CT13 展示具体原因及恢复条件).
func verificationReason(goal, outcome string, assertions []Assertion) string {
	var b strings.Builder
	if outcome == Unsatisfied {
		b.WriteString("Goal " + goal + " not met: ")
	} else {
		b.WriteString("Goal " + goal + " not decided: ")
	}
	first := true
	for _, x := range assertions {
		if x.Answer == Satisfied {
			continue
		}
		if !first {
			b.WriteString("; ")
		}
		first = false
		b.WriteString(x.Assertion + " " + x.Answer)
		if x.Code != "" {
			b.WriteString(" " + x.Code)
		}
	}
	if outcome == Unsatisfied {
		b.WriteString(". The owner deals with them; goal.verify judges the goal again once the task resumes.")
	} else {
		b.WriteString(". goal.verify judges the goal again once the checks can answer and every action is reconciled.")
	}
	s := b.String()
	if utf8.RuneCountInString(s) > maxReason {
		r := []rune(s)
		s = string(r[:maxReason-1]) + "…"
	}
	return s
}

// RecordVerification ends goal verification with assessment a (goal.verify; system; verifying →
// completed, waiting_input or waiting_external) and returns the history entry and the verification
// the task records. a must judge the task's goal (judges); for a task with a bound root, TRD's
// control of the root must be the task's at its recorded epoch (CONTROL_EPOCH_STALE otherwise, the
// task unchanged). Q01.execution is appended — unsettled actions of the task (TSK-INV-003) and the
// snapshot TRD's evidence names — and the outcome taken over every assertion decides: satisfied
// completes the task (TSK-INV-001; completion actor manual or ai_with_required_review), unsatisfied
// waits for input, unknown and error wait for an external answer, with the unmet assertions as items.
func (t *Task) RecordVerification(a Assessment, unsettled int, act Act) (Change, Verification, error) {
	if err := t.CheckVerifyGoal(act); err != nil {
		return Change{}, Verification{}, err
	}
	if err := t.judges(a); err != nil {
		return Change{}, Verification{}, err
	}
	if a.Assessor == AssessorGoalReadiness && t.Bound() {
		if err := t.controls(a.Control); err != nil {
			return Change{}, Verification{}, err
		}
	}
	v := Verification{TaskID: t.ID, Assessment: a, Unsettled: unsettled}
	v.Assertions = append(slices.Clone(a.Assertions), a.execution(unsettled))
	answers := make([]string, len(v.Assertions))
	for i, x := range v.Assertions {
		answers[i] = x.Answer
		if x.Answer != Satisfied {
			v.Unmet = append(v.Unmet, x.Assertion)
		}
	}
	v.Outcome, v.Code = Outcome(answers...)
	verdict := Verdict{Outcome: v.Outcome, Unmet: slices.Clone(v.Unmet)}
	if v.Outcome != Satisfied {
		verdict.Reason = verificationReason(t.Goal, v.Outcome, v.Assertions)
	}
	ch, err := t.VerifyGoal(verdict, unsettled, act)
	if err != nil {
		return Change{}, Verification{}, err
	}
	v.TaskVersion, v.To, v.RecordedAt = t.Version, t.State, kernel.Instant(act.At)
	return ch, v, nil
}
