// Package api is TSK's published contract (business tasks and their control, WBS S4-01; tactical
// model TSK-AGG-BusinessTask, SM-TSK-TASK, TSK-INV-001, TSK-INV-002, TSK-INV-004; Context Map TSK,
// R17 TSK→TRD ObjectControl commands v1, R18 TRD→TSK GoalReadiness v1; owner decisions S4 D-1, D-2,
// D-4, D-5; baseline 8.5 and 16.1).
//
// A BusinessTask pursues one goal — quote_ready or order_active (GOAL_UNSUPPORTED otherwise) — for
// a company, driven by an AI execution identity (control mode ai) or by people (manual). Its
// lifecycle has the 13 states of SM-TSK-TASK and moves only through the registered commands below,
// each on the transitions SM-TSK-TASK lists for it, by the actor kinds the transition names; any
// other move is INVALID_STATE, an actor kind outside the transition's rule PERMISSION_DENIED. There
// is no command that sets a state (Q01 V0.1
// tests/test_execution.py::test_generic_transition_cannot_set_completed), and no command an AI actor
// may issue: model output never changes a task (AI-INV-004). Every command names the task's version it saw (expected_version, VERSION_CONFLICT
// otherwise) and moves it by one. (ActionExecution's commands, action.go, record the side effects of
// a task's steps; their executor may be the task's AI agent.)
//
//	task.create             human or system: a task in created, its goal and goal version, control
//	                        mode, AI agent (mode ai), owner, target, source, deadline, budget and
//	                        execution bundle.
//	task.scope              created → scoping (system); scoping → running (human or system) settles
//	                        the scope.
//	task.wait               system: scoping, running or verifying → waiting_input; running or
//	                        verifying → waiting_approval; running → waiting_external; with the
//	                        reason, the responsible person and what the task waits for.
//	task.resume             waiting_* → running (system, on what arrived); paused → the waiting state
//	                        it was paused in, else running (human).
//	task.verify             system: running → verifying, with no submitted or unknown action.
//	goal.verify             system: verifying → completed when every required assertion of the goal
//	                        is satisfied (TSK-INV-001), else waiting_input (unsatisfied) or
//	                        waiting_external (unknown or error), through TRD's GoalReadiness v1.
//	task.pause              human or system: created, scoping, running, waiting_* or verifying →
//	                        paused, with the cause, reason and resume condition.
//	task.take_control       human only: any state but stopping, terminal and paused-in-manual →
//	                        stopping. A bound root is stopped at TRD first (object_control.stop,
//	                        which moves the control epoch and fences the old executor at once); the
//	                        task then records TRD's epoch (TSK-INV-002).
//	task.handoff            human only: a manual task in running, waiting_* or paused → stopping, to
//	                        hand it back to its AI agent.
//	task.cancel             human only: any non-terminal state but stopping → stopping.
//	task.finish_stop        system: stopping → paused (take control: control mode manual, TRD
//	                        object_control.enter_manual; hand back: mode ai, TRD
//	                        object_control.handoff) or → cancelled (cancel), once no action is
//	                        submitted or unknown (stop barrier).
//	task.close_unfulfilled  human or system: running, waiting_*, verifying or paused →
//	                        closed_unfulfilled; never counted as success.
//	task.fail               human only: running, waiting_*, verifying or paused → failed, with the
//	                        person responsible for what follows.
//	task.bind_object        system: running → running; binds the quote root the task's action
//	                        created through TRD object_control.bind and records the control epoch
//	                        TRD returns. TSK records the epoch; TRD is the fence (MD-31).
//	task.release            human only: a terminal task, or a paused manual one, gives its root back
//	                        through TRD object_control.release; the lifecycle does not change.
//
// Request members in snake_case: task.create {task_id, goal_code, control_mode, agent_id, owner_id,
// target {rfq_id, quote_id, customer_id}, source {kind, id}, deadline, budget {max_model_calls,
// max_retries, max_cost {currency, minor_units, amount}}, execution_bundle {bundle_id, sha256}};
// task.scope {task_id, expected_version, note}; task.wait {task_id, expected_version, wait, reason,
// responsible_id, items, awaiting {kind, id, version}, due_at}; task.verify, goal.verify and
// task.finish_stop {task_id, expected_version}; task.pause {task_id, expected_version, cause,
// reason, responsible_id, resume_condition}; task.resume {task_id, expected_version, reason,
// arrival {kind, id, version}}; task.take_control, task.handoff, task.cancel and task.release
// {task_id, expected_version, reason}; task.close_unfulfilled {task_id, expected_version, cause,
// reason}; task.fail {task_id, expected_version, reason, responsible_id}; task.bind_object {task_id,
// expected_version, quote_id}.
//
// The claim query (task.claim, Queries.Claim) is how internal/ai/execution obtains the control a
// task step carries into TRD (D-2, TSK-INV-004): in control mode ai the task's AI agent (while the
// task is scoping or running) or a human, in manual mode humans only, and nobody while the task is
// stopping or has ended.
package api

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Source is TSK's event source.
const Source = "idp-brant.tsk"

// Contract names TSK's command contract, the envelope's contract member on the command routes
// POST /api/brant/v1/tsk/commands/<command>.
const Contract = "brant.task/0.1.0"

// Commands, reads, permissions and events (Context Map TSK, tactical model TSK-AGG-BusinessTask).
const (
	CommandCreate           = "task.create"
	CommandScope            = "task.scope"
	CommandWait             = "task.wait"
	CommandVerify           = "task.verify"
	CommandVerifyGoal       = "goal.verify"
	CommandPause            = "task.pause"
	CommandResume           = "task.resume"
	CommandTakeControl      = "task.take_control"
	CommandHandoff          = "task.handoff"
	CommandCancel           = "task.cancel"
	CommandFinishStop       = "task.finish_stop"
	CommandCloseUnfulfilled = "task.close_unfulfilled"
	CommandFail             = "task.fail"
	CommandBindObject       = "task.bind_object"
	CommandRelease          = "task.release"

	// Reads: a task with its history, the claim of a task step, the audit trail.
	CommandGetTask  = "task.get"
	CommandClaim    = "task.claim"
	CommandAudit    = "tsk.audit"
	PermissionRead  = "tsk.read"
	PermissionAudit = "tsk.audit"

	EventTaskCreated           = "idp-brant.tsk.task_created.v1"
	EventTaskControlChanged    = "idp-brant.tsk.task_control_changed.v1"
	EventTaskCompleted         = "idp-brant.tsk.task_completed.v1"
	EventTaskClosedUnfulfilled = "idp-brant.tsk.task_closed_unfulfilled.v1"
)

// Goals P0 supports (baseline 15.4; the assertions of each are GoalReadiness v1's).
const (
	GoalQuoteReady  = "quote_ready"
	GoalOrderActive = "order_active"
)

var goalVersions = map[string]int64{GoalQuoteReady: 1, GoalOrderActive: 1}

// GoalVersion returns the current version of goal code, which a task records when it is created.
func GoalVersion(code string) (int64, bool) {
	v, ok := goalVersions[code]
	return v, ok
}

// Lifecycle states (SM-TSK-TASK, baseline 16.1).
const (
	StateCreated           = "created"
	StateScoping           = "scoping"
	StateRunning           = "running"
	StateWaitingInput      = "waiting_input"
	StateWaitingApproval   = "waiting_approval"
	StateWaitingExternal   = "waiting_external"
	StateVerifying         = "verifying"
	StatePaused            = "paused"
	StateStopping          = "stopping"
	StateCompleted         = "completed"
	StateClosedUnfulfilled = "closed_unfulfilled"
	StateCancelled         = "cancelled"
	StateFailed            = "failed"
)

// Control modes and completion actors (baseline 16.1). P0 never completes ai_unassisted: its goals
// include a required human review.
const (
	ModeAI     = "ai"
	ModeManual = "manual"

	CompletionAIWithRequiredReview = "ai_with_required_review"
	CompletionAIUnassisted         = "ai_unassisted"
	CompletionManual               = "manual"
)

// What a task waits for (task.wait).
const (
	WaitInput    = "input"
	WaitApproval = "approval"
	WaitExternal = "external"
)

// Causes of a pause (task.pause; baseline 16.1 "预算、模型／工具故障、冲突").
const (
	PauseBudgetExhausted  = "budget_exhausted"
	PauseModelUnavailable = "model_unavailable"
	PauseToolUnavailable  = "tool_unavailable"
	PauseConflict         = "conflict"
	PauseOperator         = "operator"
)

// Causes of an unfulfilled close (task.close_unfulfilled; baseline 16.1 "拒绝、到期且不再推进").
const (
	CloseCustomerDeclined = "customer_declined"
	CloseQuoteExpired     = "quote_expired"
	CloseSuperseded       = "superseded"
	CloseNotPursued       = "not_pursued"
)

// Why a task is stopping, and the control changes a task records (ControlTransition).
const (
	StopTakeControl = "take_control"
	StopHandoff     = "handoff"
	StopCancel      = "cancel"

	ChangeBind        = "bind"
	ChangeTakeControl = "take_control"
	ChangeHandoff     = "handoff"
	ChangeCancel      = "cancel"
	ChangeFinishStop  = "finish_stop"
	ChangeRelease     = "release"

	BindingBound    = "bound"
	BindingReleased = "released"
)

// Kinds of the source a task serves (来源防重).
const (
	SourceEVD = "evd_source" // an EVD source, such as a registered inquiry message
	SourceRFQ = "rfq"        // an INQ customer RFQ
)

// Limits of the contract.
const (
	MaxReason        = 1000
	MaxItems         = 20
	MaxItem          = 200
	MaxModelCalls    = 100000
	MaxRetries       = 100
	MaxMinorUnits    = 4
	maxVersion       = 2147483647
	maxKind          = 40
	currencyCodeSize = 3
)

// Target is what the task works on: the customer RFQ, the quote root and the customer, each
// optional. A task with a quote root binds only that root.
type Target struct {
	RfqID      string
	QuoteID    string
	CustomerID string
}

// SourceRef is the source a task serves; one open task serves a goal for a source.
type SourceRef struct {
	Kind string
	ID   string
}

// CostCap is a budget's cost limit: Amount in Currency with MinorUnits decimals (kernel.Money).
type CostCap struct {
	Currency   string
	MinorUnits int
	Amount     string
}

// Budget limits a task's model calls, retries and cost; internal/ai's execution entry enforces it
// before every model or tool call (baseline 17.4, AI-INV-003).
type Budget struct {
	MaxModelCalls int
	MaxRetries    int
	MaxCost       *CostCap
}

// BundleRef names the ExecutionBundle a task runs with and its SHA-256 (baseline 17.3).
type BundleRef struct {
	BundleID string
	SHA256   string
}

// ObjectRef names what a task waits for or what arrived: an object of Kind (a lower-case name such
// as quote_review or customer_response) with ID, at Version when it has one.
type ObjectRef struct {
	Kind    string
	ID      string
	Version int64
}

// CreateTask creates task TaskID for GoalCode in ControlMode. AgentID is the AI execution identity
// the task's delegation names (control mode ai; none in manual). OwnerID is the human responsible
// for the task; empty means the initiator, who must then be human. Deadline is a UTC timestamp.
// Human or system.
type CreateTask struct {
	IdempotencyKey  string
	TaskID          string
	GoalCode        string
	ControlMode     string
	AgentID         string
	OwnerID         string
	Target          Target
	Source          *SourceRef
	Deadline        string
	Budget          *Budget
	ExecutionBundle *BundleRef
}

// ScopeTask moves a created task to scoping (system) and a scoping task to running (human or
// system), which settles its scope; Note is recorded with the transition.
type ScopeTask struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
	Note            string
}

// WaitTask makes the task wait for input, an approval or an external result, for Reason, with
// ResponsibleID the person to act: input names the missing Items; approval names the approval
// request it awaits (Awaiting); external names the operation or source it awaits and when the
// wait ends (DueAt). System.
type WaitTask struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
	Wait            string
	Reason          string
	ResponsibleID   string
	Items           []string
	Awaiting        *ObjectRef
	DueAt           string
}

// VerifyTask moves a running task to verifying (task.verify). System.
type VerifyTask struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
}

// VerifyGoal verifies a verifying task's goal against the authoritative records (goal.verify).
// System.
type VerifyGoal struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
}

// PauseTask pauses the task for Cause and Reason until ResumeCondition holds; ResponsibleID (the
// owner when empty) acts on it. Human or system.
type PauseTask struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
	Cause           string
	Reason          string
	ResponsibleID   string
	ResumeCondition string
}

// ResumeTask resumes a waiting task on Arrival (system), or a paused one once its cause is dealt
// with, for Reason (human).
type ResumeTask struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
	Reason          string
	Arrival         *ObjectRef
}

// TakeControl takes the task over for people, for Reason. Human only.
type TakeControl struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
	Reason          string
}

// Handoff hands a manual task back to its AI agent, for Reason. Human only.
type Handoff struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
	Reason          string
}

// CancelTask cancels the task, for Reason. Human only.
type CancelTask struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
	Reason          string
}

// FinishStop completes the stop barrier of a stopping task. System.
type FinishStop struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
}

// CloseUnfulfilled closes the task without reaching its goal, for Cause and Reason. Human or
// system.
type CloseUnfulfilled struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
	Cause           string
	Reason          string
}

// FailTask ends the task as failed for Reason; ResponsibleID is the person responsible for what
// follows. Human only.
type FailTask struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
	Reason          string
	ResponsibleID   string
}

// BindObject binds quote root QuoteID to the task through TRD. System.
type BindObject struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
	QuoteID         string
}

// ReleaseObject gives the task's bound root back through TRD, for Reason. Human only.
type ReleaseObject struct {
	IdempotencyKey  string
	TaskID          string
	ExpectedVersion int64
	Reason          string
}

// Command is one of TSK's commands.
type Command interface{ tskCommand() }

func (CreateTask) tskCommand()       {}
func (ScopeTask) tskCommand()        {}
func (WaitTask) tskCommand()         {}
func (VerifyTask) tskCommand()       {}
func (VerifyGoal) tskCommand()       {}
func (PauseTask) tskCommand()        {}
func (ResumeTask) tskCommand()       {}
func (TakeControl) tskCommand()      {}
func (Handoff) tskCommand()          {}
func (CancelTask) tskCommand()       {}
func (FinishStop) tskCommand()       {}
func (CloseUnfulfilled) tskCommand() {}
func (FailTask) tskCommand()         {}
func (BindObject) tskCommand()       {}
func (ReleaseObject) tskCommand()    {}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

// Reason reports whether s is a reason: 1 to MaxReason characters of valid UTF-8, not only white
// space, without control characters.
func Reason(s string) bool { return text(s, MaxReason) }

func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s) && strings.TrimSpace(s) != "" && strings.IndexFunc(s, unicode.IsControl) < 0
}

// Kind reports whether s is a kind name: a lower-case letter, then up to 39 lower-case letters,
// digits and underscores.
func Kind(s string) bool {
	if len(s) == 0 || len(s) > maxKind || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func head(key, task string, expected int64) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(task):
		return schema("/args/task_id")
	case expected < 1 || expected > maxVersion:
		return schema("/args/expected_version")
	}
	return nil
}

func reasoned(key, task string, expected int64, reason string) error {
	if err := head(key, task, expected); err != nil {
		return err
	}
	if !Reason(reason) {
		return schema("/args/reason")
	}
	return nil
}

func (r ObjectRef) validate(path string) error {
	switch {
	case !Kind(r.Kind):
		return schema(path + "/kind")
	case !kernel.ValidIdentifier(r.ID):
		return schema(path + "/id")
	case r.Version < 0 || r.Version > maxVersion:
		return schema(path + "/version")
	}
	return nil
}

func hex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Cost returns the cost cap as money: SCHEMA_INVALID for a currency that is not three upper-case
// letters, minor units outside 0..MaxMinorUnits or an amount that is not a non-negative decimal
// with at most that many decimals.
func (c CostCap) Cost() (kernel.Money, error) {
	if len(c.Currency) != currencyCodeSize || strings.IndexFunc(c.Currency, func(r rune) bool { return r < 'A' || r > 'Z' }) >= 0 {
		return kernel.Money{}, schema("/args/budget/max_cost/currency")
	}
	if c.MinorUnits < 0 || c.MinorUnits > MaxMinorUnits {
		return kernel.Money{}, schema("/args/budget/max_cost/minor_units")
	}
	cur, err := kernel.NewCurrency(c.Currency, c.MinorUnits)
	if err != nil {
		return kernel.Money{}, schema("/args/budget/max_cost/currency")
	}
	m, err := kernel.ParseAmount(cur, c.Amount)
	if err != nil || m.Sign() < 0 {
		return kernel.Money{}, schema("/args/budget/max_cost/amount")
	}
	return m, nil
}

// Validate checks the closed contract: a registered goal (GOAL_UNSUPPORTED otherwise), a control
// mode with an agent exactly in mode ai, identifiers, a source of a known kind, a deadline, a budget
// of positive limits and a bundle with its SHA-256.
func (c CreateTask) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.TaskID):
		return schema("/args/task_id")
	case c.GoalCode == "":
		return schema("/args/goal_code")
	}
	if _, ok := GoalVersion(c.GoalCode); !ok {
		return Reject(CodeGoalUnsupported, "/args/goal_code", "P0 supports the goals quote_ready and order_active")
	}
	switch c.ControlMode {
	case ModeAI:
		if !kernel.ValidIdentifier(c.AgentID) {
			return schema("/args/agent_id")
		}
	case ModeManual:
		if c.AgentID != "" && !kernel.ValidIdentifier(c.AgentID) {
			return schema("/args/agent_id")
		}
	default:
		return schema("/args/control_mode")
	}
	if c.OwnerID != "" && !kernel.ValidIdentifier(c.OwnerID) {
		return schema("/args/owner_id")
	}
	for _, f := range [...]struct{ path, id string }{{"rfq_id", c.Target.RfqID}, {"quote_id", c.Target.QuoteID}, {"customer_id", c.Target.CustomerID}} {
		if f.id != "" && !kernel.ValidIdentifier(f.id) {
			return schema("/args/target/" + f.path)
		}
	}
	if s := c.Source; s != nil {
		if s.Kind != SourceEVD && s.Kind != SourceRFQ {
			return schema("/args/source/kind")
		}
		if !kernel.ValidIdentifier(s.ID) {
			return schema("/args/source/id")
		}
	}
	if c.Deadline != "" {
		if _, err := kernel.ParseTimestamp(c.Deadline); err != nil {
			return schema("/args/deadline")
		}
	}
	if b := c.Budget; b != nil {
		switch {
		case b.MaxModelCalls < 1 || b.MaxModelCalls > MaxModelCalls:
			return schema("/args/budget/max_model_calls")
		case b.MaxRetries < 0 || b.MaxRetries > MaxRetries:
			return schema("/args/budget/max_retries")
		}
		if b.MaxCost != nil {
			if _, err := b.MaxCost.Cost(); err != nil {
				return err
			}
		}
	}
	if e := c.ExecutionBundle; e != nil {
		if !kernel.ValidIdentifier(e.BundleID) {
			return schema("/args/execution_bundle/bundle_id")
		}
		if !hex64(e.SHA256) {
			return schema("/args/execution_bundle/sha256")
		}
	}
	return nil
}

// Validate checks the closed contract.
func (c ScopeTask) Validate() error {
	if err := head(c.IdempotencyKey, c.TaskID, c.ExpectedVersion); err != nil {
		return err
	}
	if c.Note != "" && !Reason(c.Note) {
		return schema("/args/note")
	}
	return nil
}

// Validate checks the closed contract and what each wait records: a reason and a responsible
// person always; input names 1 to MaxItems missing items; approval names the request it awaits;
// external names what it awaits and the instant the wait ends.
func (c WaitTask) Validate() error {
	if err := reasoned(c.IdempotencyKey, c.TaskID, c.ExpectedVersion, c.Reason); err != nil {
		return err
	}
	if !kernel.ValidIdentifier(c.ResponsibleID) {
		return schema("/args/responsible_id")
	}
	if len(c.Items) > MaxItems {
		return schema("/args/items")
	}
	for i, it := range c.Items {
		if !text(it, MaxItem) {
			return schema("/args/items/" + strconv.Itoa(i))
		}
	}
	if c.Awaiting != nil {
		if err := c.Awaiting.validate("/args/awaiting"); err != nil {
			return err
		}
	}
	if c.DueAt != "" {
		if _, err := kernel.ParseTimestamp(c.DueAt); err != nil {
			return schema("/args/due_at")
		}
	}
	switch c.Wait {
	case WaitInput:
		if len(c.Items) == 0 {
			return schema("/args/items")
		}
	case WaitApproval:
		if c.Awaiting == nil {
			return schema("/args/awaiting")
		}
	case WaitExternal:
		if c.Awaiting == nil {
			return schema("/args/awaiting")
		}
		if c.DueAt == "" {
			return schema("/args/due_at")
		}
	default:
		return schema("/args/wait")
	}
	return nil
}

// Validate checks the closed contract.
func (c VerifyTask) Validate() error { return head(c.IdempotencyKey, c.TaskID, c.ExpectedVersion) }

// Validate checks the closed contract.
func (c VerifyGoal) Validate() error { return head(c.IdempotencyKey, c.TaskID, c.ExpectedVersion) }

// Validate checks the closed contract: a known cause, a reason and a resume condition.
func (c PauseTask) Validate() error {
	if err := reasoned(c.IdempotencyKey, c.TaskID, c.ExpectedVersion, c.Reason); err != nil {
		return err
	}
	switch c.Cause {
	case PauseBudgetExhausted, PauseModelUnavailable, PauseToolUnavailable, PauseConflict, PauseOperator:
	default:
		return schema("/args/cause")
	}
	if c.ResponsibleID != "" && !kernel.ValidIdentifier(c.ResponsibleID) {
		return schema("/args/responsible_id")
	}
	if !Reason(c.ResumeCondition) {
		return schema("/args/resume_condition")
	}
	return nil
}

// Validate checks the closed contract; which of Reason and Arrival a resume needs depends on the
// state it leaves (the domain decides).
func (c ResumeTask) Validate() error {
	if err := head(c.IdempotencyKey, c.TaskID, c.ExpectedVersion); err != nil {
		return err
	}
	if c.Reason != "" && !Reason(c.Reason) {
		return schema("/args/reason")
	}
	if c.Arrival != nil {
		return c.Arrival.validate("/args/arrival")
	}
	return nil
}

// Validate checks the closed contract.
func (c TakeControl) Validate() error {
	return reasoned(c.IdempotencyKey, c.TaskID, c.ExpectedVersion, c.Reason)
}

// Validate checks the closed contract.
func (c Handoff) Validate() error {
	return reasoned(c.IdempotencyKey, c.TaskID, c.ExpectedVersion, c.Reason)
}

// Validate checks the closed contract.
func (c CancelTask) Validate() error {
	return reasoned(c.IdempotencyKey, c.TaskID, c.ExpectedVersion, c.Reason)
}

// Validate checks the closed contract.
func (c FinishStop) Validate() error { return head(c.IdempotencyKey, c.TaskID, c.ExpectedVersion) }

// Validate checks the closed contract: a known cause and a reason.
func (c CloseUnfulfilled) Validate() error {
	if err := reasoned(c.IdempotencyKey, c.TaskID, c.ExpectedVersion, c.Reason); err != nil {
		return err
	}
	switch c.Cause {
	case CloseCustomerDeclined, CloseQuoteExpired, CloseSuperseded, CloseNotPursued:
		return nil
	}
	return schema("/args/cause")
}

// Validate checks the closed contract: a reason and the person responsible for what follows.
func (c FailTask) Validate() error {
	if err := reasoned(c.IdempotencyKey, c.TaskID, c.ExpectedVersion, c.Reason); err != nil {
		return err
	}
	if !kernel.ValidIdentifier(c.ResponsibleID) {
		return schema("/args/responsible_id")
	}
	return nil
}

// Validate checks the closed contract.
func (c BindObject) Validate() error {
	if err := head(c.IdempotencyKey, c.TaskID, c.ExpectedVersion); err != nil {
		return err
	}
	if !kernel.ValidIdentifier(c.QuoteID) {
		return schema("/args/quote_id")
	}
	return nil
}

// Validate checks the closed contract.
func (c ReleaseObject) Validate() error {
	return reasoned(c.IdempotencyKey, c.TaskID, c.ExpectedVersion, c.Reason)
}

// Receipt is the closed success answer of a command: ObjectID the task, RowVersion its task
// version and Status its lifecycle state after the command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// TaskReceipt is the receipt with the task's control as it is stored now: its control mode, the
// control epoch it recorded from TRD and the quote root it holds bound (empty when none).
type TaskReceipt struct {
	Receipt
	ControlMode  string
	ControlEpoch int64
	BoundQuoteID string
}

// BindingView is the task's binding of a quote root as TSK recorded it: State bound or released,
// ControlEpoch the epoch TRD returned for the last control change (TRD is the fence).
type BindingView struct {
	QuoteID      string
	State        string
	ControlEpoch int64
	BoundAt      time.Time
	ReleasedAt   time.Time
}

// WaitView is what a waiting task waits for (baseline 8.5 "每个等待、停止或暂停状态记录原因、责任人和恢复
// 条件"): Wait input, approval or external, why, who acts, the missing items, the object awaited and
// when the wait ends. A task paused or stopping from a wait keeps it and resumes to it.
type WaitView struct {
	Wait          string
	Reason        string
	ResponsibleID string
	Items         []string
	Awaiting      *ObjectRef
	DueAt         time.Time
	Since         time.Time
}

// PauseView is why a task was paused (task.pause), who acts and when it may resume.
type PauseView struct {
	Cause           string
	Reason          string
	ResponsibleID   string
	ResumeCondition string
	Since           time.Time
}

// StopView is a stopping task's stop request, kept until the next stop.
type StopView struct {
	Purpose     string // take_control, handoff or cancel
	From        string // the state the task stopped from
	Reason      string
	RequestedBy kernel.ActorRef
	RequestedAt time.Time
}

// TerminationView is how a task ended: its cause and reason, the person responsible for what
// follows a failure, who ended it and when.
type TerminationView struct {
	Cause         string
	Reason        string
	ResponsibleID string
	By            kernel.ActorRef
	At            time.Time
}

// TransitionView is one entry of a task's append-only history: a command, the states and control
// it moved between, who issued it under which authority epoch, and ControlChange for a control
// transition (bind, take_control, handoff, cancel, finish_stop, release; empty otherwise).
type TransitionView struct {
	Seq            int64
	Command        string
	From           string
	To             string
	ControlMode    string
	ControlEpoch   int64
	ControlChange  string
	TaskVersion    int64
	Actor          kernel.ActorRef
	AuthorityEpoch int64
	At             time.Time
	Reason         string
}

// TaskView is a task as the reads show it.
type TaskView struct {
	TaskID          string
	GoalCode        string
	GoalVersion     int64
	Lifecycle       string
	ControlMode     string
	ControlEpoch    int64
	TaskVersion     int64
	Initiator       kernel.ActorRef
	OwnerID         string
	AgentID         string
	Target          Target
	Source          *SourceRef
	Deadline        time.Time
	Budget          *Budget
	ExecutionBundle *BundleRef
	Binding         *BindingView
	Wait            *WaitView
	Pause           *PauseView
	PausedFrom      string
	Stop            *StopView
	ScopeSettledAt  time.Time
	CompletionActor string
	Termination     *TerminationView
	CreatedAt       time.Time
	UpdatedAt       time.Time
	History         []TransitionView
}

// Claim is the control a task step carries (D-2, TSK-INV-004): the task, its version and state, its
// control mode, the control epoch TSK recorded from TRD and the quote root bound to it (empty and 0
// when none is bound: a write on an unbound root carries no claim), issued to Actor at IssuedAt.
// internal/ai/execution puts TaskID and ControlEpoch into the TRD command's context
// (trdapi.WithControl); TRD compares them with its own control (TRD is the fence).
type Claim struct {
	TaskID       string
	TaskVersion  int64
	Lifecycle    string
	ControlMode  string
	ControlEpoch int64
	QuoteID      string
	Actor        kernel.ActorRef
	IssuedAt     time.Time
}

// Commands is TSK's command entry point. Errors are *kernel.Rejection for refusals. An error that
// is application.ErrCommitUncertain from IDP-Bento-Go means the outcome is unknown: the caller sends
// the same command with the same idempotency key again, or reconciles it.
type Commands interface {
	CreateTask(ctx context.Context, cmd CreateTask) (TaskReceipt, error)
	ScopeTask(ctx context.Context, cmd ScopeTask) (TaskReceipt, error)
	WaitTask(ctx context.Context, cmd WaitTask) (TaskReceipt, error)
	VerifyTask(ctx context.Context, cmd VerifyTask) (TaskReceipt, error)
	VerifyGoal(ctx context.Context, cmd VerifyGoal) (TaskReceipt, error)
	PauseTask(ctx context.Context, cmd PauseTask) (TaskReceipt, error)
	ResumeTask(ctx context.Context, cmd ResumeTask) (TaskReceipt, error)
	TakeControl(ctx context.Context, cmd TakeControl) (TaskReceipt, error)
	Handoff(ctx context.Context, cmd Handoff) (TaskReceipt, error)
	CancelTask(ctx context.Context, cmd CancelTask) (TaskReceipt, error)
	FinishStop(ctx context.Context, cmd FinishStop) (TaskReceipt, error)
	CloseUnfulfilled(ctx context.Context, cmd CloseUnfulfilled) (TaskReceipt, error)
	FailTask(ctx context.Context, cmd FailTask) (TaskReceipt, error)
	BindObject(ctx context.Context, cmd BindObject) (TaskReceipt, error)
	ReleaseObject(ctx context.Context, cmd ReleaseObject) (TaskReceipt, error)
}

// Queries is TSK's audited read side.
type Queries interface {
	// GetTask returns a task the caller sees with its history, audited as task.get (tsk.read):
	// NOT_FOUND otherwise. The owner and the owner's team (by data scope), the initiator and the
	// task's AI agent see it.
	GetTask(ctx context.Context, taskID string) (TaskView, error)
	// Claim issues the claim of a task step to the caller, audited as task.claim (tsk.read; human
	// or AI): NOT_FOUND for a task the caller does not see; INVALID_STATE while the task is stopping
	// or has ended (Q01 V0.1 tests/test_execution.py::test_terminal_task_rejects_new_forward_action);
	// CONTROL_MODE_MISMATCH for an AI caller of a manual task; PERMISSION_DENIED for an AI caller
	// that is not the task's agent; INVALID_STATE for the agent while the task is neither scoping nor
	// running.
	Claim(ctx context.Context, taskID string) (Claim, error)
	// Audit returns TSK's audit trail (tsk.audit).
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

// Auditor records a request an inbound adapter refused before it became a command.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Reconciliation is what became of a command whose answer the caller lost.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string
	ObjectID string
	Receipt  *Receipt
}

// Reconciler recovers from an unknown outcome without executing anything (contract 06, 动作对账).
// Reconciling needs tsk.read, not the command's own permission.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}
