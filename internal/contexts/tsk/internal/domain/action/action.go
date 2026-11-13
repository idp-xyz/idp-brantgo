// Package action is TSK's ActionExecution (tactical model TSK-AGG-ActionExecution, TSK-INV-003,
// X-INV-001; Context Map R18 and R66–R69, ActionReconciliation v1; owner decision S4 D-11; baseline
// 17.1 and 17.2).
//
// A task step that writes another context is one action of one business intent. The action is
// registered before its command is sent, with the target context, the command, the idempotency key
// the command carries — the action's own ID, derived from the business intent (ID), so a new plan
// cannot give the same intent a new key — and the request hash the target's ledger records. Each
// send is an attempt; attempts are appended and never rewritten (Step). What became of an attempt is
// learnt from the target's ledger by key and hash (Answer): committed — the target's stored receipt,
// the original result and never a new qualification —, payload_conflict — the key holds another
// request: stop —, or not_committed — nothing under the key, so the next attempt sends the same key
// again and the target's ledger executes it once. The executor's own observation of an attempt
// (rejected with the target's code, or unknown when it lost the answer) is recorded only while the
// target holds nothing under the key: the target's ledger is the truth.
//
// An action whose attempt is submitted or unknown is open: a task's stop barrier, verification,
// release and ends wait until it is reconciled (TSK-INV-003; the count is the store's).
package action

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// State is an action's state, the state its last log row records.
type State string

const (
	// Submitted: an attempt is recorded before its command is sent; its answer is pending.
	Submitted State = "submitted"
	// Unknown: the executor lost the answer of the open attempt (a commit it cannot confirm, a
	// broken connection, a restart); the target held nothing under the key when it was recorded.
	Unknown State = "unknown"
	// Rejected: the target refused the attempt with a code and holds nothing under the key.
	Rejected State = "rejected"
	// NotCommitted: a reconciliation found nothing under the key.
	NotCommitted State = "not_committed"
	// Committed: the target's ledger holds the request under the key; final.
	Committed State = "committed"
	// Conflict: the target's ledger holds another request under the key; final, nothing is sent
	// again.
	Conflict State = "conflict"
)

// States are an action's states.
var States = []State{Submitted, Unknown, Rejected, NotCommitted, Committed, Conflict}

// Open reports whether s has an attempt whose outcome is not known.
func (s State) Open() bool { return s == Submitted || s == Unknown }

// Final reports whether s never changes again.
func (s State) Final() bool { return s == Committed || s == Conflict }

// The commands that change an action.
const (
	CmdRecordAttempt = "action.record_attempt"
	CmdReconcile     = "action.reconcile"
)

// Contexts are the contexts a task step writes and whose ActionReconciliation v1 TSK asks (Context
// Map R18 TRD, R66 EVD, R67 INQ, R68 SUP, R69 CST).
var Contexts = []string{"evd", "inq", "sup", "cst", "trd"}

// KnownContext reports whether c is one of Contexts.
func KnownContext(c string) bool {
	for _, k := range Contexts {
		if k == c {
			return true
		}
	}
	return false
}

// idPrefix starts every action ID; the rest is a digest of the business intent.
const idPrefix = "tska."

// ID returns the ID of the action of business intent intent, which is also the idempotency key its
// command carries: the same for every attempt and every plan that pursues the intent (baseline
// 17.1: an action's key is the application's, fixed before the side effect, never the model's).
func ID(intent string) string {
	sum := sha256.Sum256([]byte("brant.tsk.action\x1f" + intent))
	return idPrefix + hex.EncodeToString(sum[:20])
}

// Target is the command an action sends: the context, the command name and the request hash, as
// the context's RequestOf returns them.
type Target struct {
	Context     string
	Command     string
	RequestHash string
}

// Result is the target's stored receipt of a committed command.
type Result struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
}

// Answer is what the target's ActionReconciliation v1 answered for the action's key and hash.
type Answer struct {
	State  kernel.ReconcileState
	Result *Result // committed only
}

func (a Answer) valid() bool {
	switch a.State {
	case kernel.ReconcileCommitted:
		return a.Result != nil
	case kernel.ReconcilePayloadConflict, kernel.ReconcileNotCommitted:
		return a.Result == nil
	}
	return false
}

// Execution is an action.
type Execution struct {
	ID           string
	Intent       string
	TaskID       string
	Target       Target
	State        State
	Version      int64
	Attempts     int
	Result       *Result
	RegisteredBy kernel.ActorRef
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Key returns the idempotency key the action's command carries.
func (e *Execution) Key() string { return e.ID }

// Act is who changes an action, under which authority epoch, at which control epoch of its task,
// and when.
type Act struct {
	By           kernel.ActorRef
	Epoch        int64
	ControlEpoch int64
	At           time.Time
}

// Step is one row of an action's append-only log: the change that produced Version.
type Step struct {
	Version      int64
	Attempt      int
	Command      string
	State        State
	Answer       kernel.ReconcileState
	Code         string
	ControlEpoch int64
	DelegationID string
	GrantID      string
	Result       *Result
	By           kernel.ActorRef
	Epoch        int64
	At           time.Time
}

// Request is an action.record_attempt: the business intent, its task and target, what the
// executor records — Submitted before it sends, Rejected (with the target's Code) or Unknown after
// — and, for a submission, the delegation and approval grant the attempt names.
type Request struct {
	Intent       string
	TaskID       string
	Target       Target
	Outcome      State
	Code         string
	DelegationID string
	GrantID      string
}

func payloadConflict() error {
	return kernel.Reject(kernel.CodeIdempotencyPayloadConflict, "/args/business_intent_id", "The business intent is registered for another task or command")
}

func invalidState(path, reason string) error {
	return kernel.Reject(kernel.CodeInvalidState, path, reason)
}

// CodeExternalResultUnknown: an attempt of the action is submitted or unknown; it is reconciled
// before anything else is sent for the intent (catalog 任务与 AI).
const CodeExternalResultUnknown = "EXTERNAL_RESULT_UNKNOWN"

func unknownResult() error {
	return kernel.RejectAs(CodeExternalResultUnknown, kernel.CategoryConflict, "/args/business_intent_id",
		"An attempt of the action is submitted or unknown: reconcile it first")
}

// Precheck decides what it can of r before the target is asked (e is the intent's action, nil when
// it has none): the intent belongs to this task and target (IDEMPOTENCY_PAYLOAD_CONFLICT
// otherwise, TSK-INV-003), no attempt is open for a submission (EXTERNAL_RESULT_UNKNOWN), a
// submitted attempt exists for an observation (INVALID_STATE). It reports whether the target's
// answer is needed: not for an action that is final, which answers with what it holds.
func Precheck(e *Execution, r Request) (ask bool, err error) {
	if e != nil {
		if e.TaskID != r.TaskID || e.Target != r.Target {
			return false, payloadConflict()
		}
		if e.State.Final() {
			return false, nil
		}
	}
	switch r.Outcome {
	case Submitted:
		if e != nil && e.State.Open() {
			return false, unknownResult()
		}
	case Rejected, Unknown:
		if e == nil || e.State != Submitted {
			return false, invalidState("/args/outcome", "No attempt of the intent is submitted")
		}
	default:
		return false, kernel.Reject(kernel.CodeSchemaInvalid, "/args/outcome", "")
	}
	return true, nil
}

// Record applies action.record_attempt to e, the intent's action (nil when it has none), with the
// target's answer for the action's key and hash. It returns the action and the step it appended;
// no step when the action is final, which answers with what it holds (CT11: an intent that
// succeeded answers with its original result and is not sent again).
//
// committed and payload_conflict settle the action whatever the executor recorded; with
// not_committed, a submission opens the next attempt (the first registers the action, TSK-INV-003),
// a rejection or a lost answer settles the open attempt as the executor saw it.
func Record(e *Execution, r Request, answer Answer, act Act) (*Execution, *Step, error) {
	ask, err := Precheck(e, r)
	if err != nil || !ask {
		return e, nil, err
	}
	if !answer.valid() {
		return nil, nil, kernel.Reject(kernel.CodeSchemaInvalid, "/answer", "The target's answer is incomplete")
	}
	at := kernel.Instant(act.At)
	if e == nil {
		e = &Execution{ID: ID(r.Intent), Intent: r.Intent, TaskID: r.TaskID, Target: r.Target, RegisteredBy: act.By, CreatedAt: at}
	}
	s := Step{Command: CmdRecordAttempt, Answer: answer.State, ControlEpoch: act.ControlEpoch, By: act.By, Epoch: act.Epoch, At: at}
	switch {
	case answer.State != kernel.ReconcileNotCommitted:
		e.settle(answer, &s)
	case r.Outcome == Submitted:
		e.State, e.Attempts = Submitted, e.Attempts+1
		s.State, s.DelegationID, s.GrantID = Submitted, r.DelegationID, r.GrantID
	default:
		e.State, s.State, s.Code = r.Outcome, r.Outcome, r.Code
	}
	e.Version++
	e.UpdatedAt = at
	s.Version, s.Attempt = e.Version, e.Attempts
	return e, &s, nil
}

// settle records a committed or conflicting answer in e and s.
func (e *Execution) settle(answer Answer, s *Step) {
	if answer.State == kernel.ReconcileCommitted {
		r := *answer.Result
		e.State, e.Result, s.State, s.Result = Committed, &r, Committed, &r
		return
	}
	e.State, s.State = Conflict, Conflict
}

// CheckReconcile decides whether action.reconcile may ask the target: a final action has nothing
// to reconcile (INVALID_STATE).
func (e *Execution) CheckReconcile() error {
	if e.State.Final() {
		return invalidState("/args/action_id", "The action is "+string(e.State))
	}
	return nil
}

// Reconcile applies action.reconcile with the target's answer: committed and payload_conflict
// settle the action; not_committed settles its open attempt as not committed — the next attempt
// sends the same key — or records that a settled attempt still did not commit.
func (e *Execution) Reconcile(answer Answer, act Act) (Step, error) {
	if err := e.CheckReconcile(); err != nil {
		return Step{}, err
	}
	if !answer.valid() {
		return Step{}, kernel.Reject(kernel.CodeSchemaInvalid, "/answer", "The target's answer is incomplete")
	}
	at := kernel.Instant(act.At)
	s := Step{Command: CmdReconcile, Answer: answer.State, ControlEpoch: act.ControlEpoch, By: act.By, Epoch: act.Epoch, At: at}
	if answer.State == kernel.ReconcileNotCommitted {
		e.State, s.State = NotCommitted, NotCommitted
	} else {
		e.settle(answer, &s)
	}
	e.Version++
	e.UpdatedAt = at
	s.Version, s.Attempt = e.Version, e.Attempts
	return s, nil
}

// CreatedRoot reports whether one of list, the actions of a task, created the root object at
// context with command: the action is committed and the target's stored receipt names object at row
// version 1, the version a root has when the save that creates it commits (SM-TSK-TASK
// task.bind_object "本任务的动作已创建报价根": quote.save_draft at TRD).
func CreatedRoot(list []*Execution, context, command, object string) bool {
	for _, e := range list {
		if e.State == Committed && e.Target.Context == context && e.Target.Command == command && e.Result != nil &&
			e.Result.ObjectID == object && e.Result.RowVersion == 1 {
			return true
		}
	}
	return false
}
