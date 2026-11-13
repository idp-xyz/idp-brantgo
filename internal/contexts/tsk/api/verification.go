package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Goal verification (goal.verify; SM-TSK-TASK verifying → completed | waiting_input |
// waiting_external; TSK-INV-001; tactical model BusinessTask entity OutcomeEvidence; Context Map
// TSK truth 目标核验结论, R18 GoalReadiness v1; baseline 8.5 OutcomeEvidence, 15.4 完成断言; WBS
// S4-01d).
//
// goal.verify asks TRD's GoalReadiness v1 for the task's goal on its quote root (and its target
// customer and RFQ) before TSK's transaction, and records one GoalVerification with the task:
// the records judged, every assertion of the goal with its checks and evidence, TSK's own
// Q01.execution — no action of the task submitted or unknown (TSK-INV-003), and the assessment's
// evidence naming the records it judged (snapshot) —, the instant TRD judged and the state the task
// moved to. The outcome over all assertions decides: satisfied → completed (TaskCompleted with the
// business revision and the order judged), unsatisfied → waiting_input, unknown or error →
// waiting_external, the unmet assertions being the wait's items and its reason naming each with its
// code. A task that names no quote root, or whose root TRD does not show the verifying actor
// (NOT_FOUND, PERMISSION_DENIED), is not met (Q01.quote, Q01.scope; assessor "tsk"). The verification
// judges the task as it was read (VERSION_CONFLICT when it changed in between); for a task with a
// bound root, TRD's control of the root must be the task's at its recorded epoch, else
// CONTROL_EPOCH_STALE and nothing changes — a take-over whose TRD step committed is recorded first.
// Any other refusal of TRD (SCHEMA_INVALID, VERSION_CONFLICT, DEPENDENCY_NOT_SUPPORTED) refuses
// goal.verify unchanged. completed is the fact that the goal was achieved when TRD judged it; later
// changes of the quote or the order do not undo it (BC-AT25).

// CommandVerifications is the audited read of a task's goal verifications (tsk.read).
const CommandVerifications = "task.verifications"

// Who judged a goal.
const (
	AssessorGoalReadiness = "trd.goal_readiness.v1" // TRD's GoalReadiness v1
	AssessorTSK           = "tsk"                   // TSK, when TRD was not asked or refused the read
)

// AssertionExecution is the assertion TSK judges itself (baseline 15.4 Q01.execution).
const AssertionExecution = "Q01.execution"

// Answers of a check, an assertion and a verification (baseline 15.4).
const (
	AnswerSatisfied   = "satisfied"
	AnswerUnsatisfied = "unsatisfied"
	AnswerUnknown     = "unknown"
	AnswerError       = "error"
)

// Codes of a verification that does not complete the task (error catalog 任务与 AI, contexts
// TSK／TRD; 422): they are recorded outcomes, not refusals of goal.verify.
const (
	CodeRequiredCheckUnsatisfied = "REQUIRED_CHECK_UNSATISFIED"
	CodeRequiredCheckUnknown     = "REQUIRED_CHECK_UNKNOWN"
)

// GoalEvidence is a record an assertion rests on (GoalReadiness v1's evidence kinds).
type GoalEvidence struct {
	Kind    string
	ID      string
	Version int64
	Hash    string
}

// GoalCheck is one check of an assertion.
type GoalCheck struct {
	Check  string
	Answer string
	Code   string
	Path   string
}

// GoalAssertion is the answer for one assertion with its checks and evidence.
type GoalAssertion struct {
	Assertion string
	Answer    string
	Code      string
	Checks    []GoalCheck
	Evidence  []GoalEvidence
}

// RootControlView is TRD's control of the task's bound root as TRD showed it to the verification.
type RootControlView struct {
	State        string
	TaskID       string
	ControlEpoch int64
}

// GoalVerification is one goal.verify of a task (OutcomeEvidence): TaskVersion is the task version
// it produced (its history entry), Lifecycle the state it moved the task to; the goal; who judged
// it; the outcome over every assertion and its code; the unmet assertions; the records judged (zero
// where there is none); TRD's control of a bound root; the actions that were submitted or unknown;
// the assertions in order, Q01.execution last; when TRD judged and when TSK recorded.
type GoalVerification struct {
	TaskID           string
	TaskVersion      int64
	Lifecycle        string
	GoalCode         string
	GoalVersion      int64
	Assessor         string
	Outcome          string
	Code             string
	Unmet            []string
	QuoteID          string
	RootVersion      int64
	BusinessRevision int64
	BusinessHash     string
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	Control          *RootControlView
	UnsettledActions int
	Assertions       []GoalAssertion
	CheckedAt        time.Time
	RecordedAt       time.Time
	Actor            kernel.ActorRef
}

// VerificationQueries reads the goal verifications of a task.
type VerificationQueries interface {
	// GoalVerifications returns the verifications of a task the caller sees, oldest first, audited
	// as task.verifications (tsk.read): NOT_FOUND otherwise.
	GoalVerifications(ctx context.Context, taskID string) ([]GoalVerification, error)
}
