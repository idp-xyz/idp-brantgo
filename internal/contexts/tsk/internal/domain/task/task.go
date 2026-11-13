// Package task is TSK's BusinessTask (tactical model TSK-AGG-BusinessTask, SM-TSK-TASK,
// TSK-INV-001, TSK-INV-002, TSK-INV-004; baseline 8.5 and 16.1; owner decisions S4 D-1, D-2, D-4).
//
// A task's lifecycle moves only through the transitions of SM-TSK-TASK (transitions.go), each by
// the actor kinds its row names: a command from a state it has no row for is INVALID_STATE (Q01
// V0.1 TRANSITION_NOT_ALLOWED), an actor kind its row does not name PERMISSION_DENIED. Nothing sets
// a state directly. Every change moves the task version by one; the caller compares it first
// (task_version CAS).
//
// Control mode (ai or manual) is apart from the lifecycle. The control epoch is TRD's: TSK records
// the epoch TRD returned for the task's last object control command and never invents one (TRD is
// the fence authority, MD-31). A take-over, a hand-back and a cancellation pass through stopping;
// task.finish_stop completes the stop barrier once no action is submitted or unknown.
package task

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Mode is a task's control mode.
type Mode string

const (
	ModeAI     Mode = "ai"
	ModeManual Mode = "manual"
)

// Completion actors (baseline 16.1). P0's goals include a required human review, so a task never
// completes ai_unassisted.
const (
	CompletionAIWithRequiredReview = "ai_with_required_review"
	CompletionManual               = "manual"
)

// Wait kinds, pause causes and stop purposes.
const (
	WaitInput    = "input"
	WaitApproval = "approval"
	WaitExternal = "external"

	StopTakeControl = "take_control"
	StopHandoff     = "handoff"
	StopCancel      = "cancel"

	// Causes a termination records besides close_unfulfilled's own.
	EndCancelled = "cancelled"
	EndFailed    = "failed"
)

// Control changes a transition records (ControlTransition).
const (
	ChangeBind        = "bind"
	ChangeTakeControl = "take_control"
	ChangeHandoff     = "handoff"
	ChangeCancel      = "cancel"
	ChangeFinishStop  = "finish_stop"
	ChangeRelease     = "release"
)

// Binding states.
const (
	BindingBound    = "bound"
	BindingReleased = "released"
)

// Codes the domain gives besides the shared kernel codes; package api declares them with the same
// categories.
const (
	CodeControlModeMismatch   = "CONTROL_MODE_MISMATCH"
	CodeControlEpochStale     = "CONTROL_EPOCH_STALE"
	CodeExternalResultUnknown = "EXTERNAL_RESULT_UNKNOWN"
)

var codes = map[string]kernel.Category{
	CodeControlModeMismatch:   kernel.CategoryConflict,
	CodeControlEpochStale:     kernel.CategoryConflict,
	CodeExternalResultUnknown: kernel.CategoryConflict,
}

// Codes returns the codes the domain gives with their categories.
func Codes() map[string]kernel.Category {
	out := make(map[string]kernel.Category, len(codes))
	for k, v := range codes {
		out[k] = v
	}
	return out
}

func fail(code, path, reason string) error { return kernel.RejectAs(code, codes[code], path, reason) }

func invalidState(reason string) error {
	return kernel.Reject(kernel.CodeInvalidState, "/args/task_id", reason)
}

// Target is what the task works on.
type Target struct {
	RfqID      string
	QuoteID    string
	CustomerID string
}

// Source is the source a task serves.
type Source struct {
	Kind string
	ID   string
}

// Budget limits a task's model calls, retries and cost.
type Budget struct {
	MaxModelCalls int
	MaxRetries    int
	MaxCost       *kernel.Money
}

// Bundle is the ExecutionBundle a task runs with.
type Bundle struct {
	ID     string
	SHA256 string
}

// Ref names an object a task waits for or that arrived.
type Ref struct {
	Kind    string
	ID      string
	Version int64
}

// Binding is the quote root bound to the task as TSK recorded it.
type Binding struct {
	QuoteID    string
	State      string // bound or released
	Epoch      int64  // the control epoch TRD returned for the last control change of the root
	BoundAt    time.Time
	ReleasedAt time.Time
}

// WaitCondition is what a waiting task waits for: why, who acts, the missing items, the object it
// awaits and when the wait ends. It stays while the task is paused or stopping from the waiting
// state, so the task resumes to that wait (SM-TSK-TASK paused → waiting_*: "回到暂停前的等待状态").
type WaitCondition struct {
	Kind        string // input, approval, external
	Reason      string
	Responsible string
	Items       []string
	Awaiting    *Ref
	DueAt       time.Time
	Since       time.Time
}

// PauseCondition is why a task was paused, who acts and when it may resume.
type PauseCondition struct {
	Cause           string
	Reason          string
	Responsible     string
	ResumeCondition string
	Since           time.Time
}

// Stop is the request that put the task in stopping.
type Stop struct {
	Purpose string // take_control, handoff, cancel
	From    State  // where the task resumes after a take-over or hand-back: the state it stopped from, or the state a paused task was paused in
	Reason  string
	By      kernel.ActorRef
	At      time.Time
}

// Termination is how a task ended (closed_unfulfilled, cancelled or failed).
type Termination struct {
	Cause       string
	Reason      string
	Responsible string
	By          kernel.ActorRef
	At          time.Time
}

// Task is a BusinessTask.
type Task struct {
	ID              string
	Goal            string
	GoalVersion     int64
	State           State
	Mode            Mode
	ControlEpoch    int64 // the control epoch TRD returned last; 0 until the first bind
	Version         int64 // task_version
	Initiator       kernel.ActorRef
	Owner           string
	OwnerTeam       string
	Agent           string // the AI execution identity of the task's delegation; empty for a manual task without one
	Target          Target
	Source          *Source
	Deadline        time.Time
	Budget          *Budget
	Bundle          *Bundle
	Binding         *Binding
	WaitCond        *WaitCondition
	PauseCond       *PauseCondition
	PausedFrom      State
	Stop            *Stop
	ScopeSettledAt  time.Time
	CompletionActor string
	Termination     *Termination
	RequestHash     string // the request hash of task.create
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Act is who changes a task, under which authority epoch, and when.
type Act struct {
	By    kernel.ActorRef
	Epoch int64
	At    time.Time
}

// Change is one entry of a task's append-only history (ControlTransition for a control change).
type Change struct {
	Command       Command
	From          State // empty for task.create
	To            State
	Mode          Mode
	ControlEpoch  int64
	ControlChange string
	Version       int64
	By            kernel.ActorRef
	Epoch         int64
	At            time.Time
	Reason        string
}

// CreateRequest is a validated task.create.
type CreateRequest struct {
	ID          string
	Goal        string
	GoalVersion int64
	Mode        Mode
	Agent       string
	Owner       string
	OwnerTeam   string
	Target      Target
	Source      *Source
	Deadline    time.Time
	Budget      *Budget
	Bundle      *Bundle
	RequestHash string
}

// Create creates a task in created (task.create): the initiator is a human or system actor; the
// owner is set (the application qualifies it); a deadline lies after now.
func Create(r CreateRequest, act Act) (*Task, Change, error) {
	if act.By.Kind != kernel.ActorHuman && act.By.Kind != kernel.ActorSystem {
		return nil, Change{}, kernel.Reject(kernel.CodePermissionDenied, "/", "Only a human or system actor creates a task")
	}
	if r.Owner == "" {
		return nil, Change{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/owner_id", "A task created by a system actor names its owner")
	}
	if r.Mode == ModeAI && r.Agent == "" {
		return nil, Change{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/agent_id", "")
	}
	now := kernel.Instant(act.At)
	if !r.Deadline.IsZero() && !kernel.Instant(r.Deadline).After(now) {
		return nil, Change{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/deadline", "The deadline has passed")
	}
	t := &Task{ID: r.ID, Goal: r.Goal, GoalVersion: r.GoalVersion, State: Created, Mode: r.Mode, Version: 1, Initiator: act.By,
		Owner: r.Owner, OwnerTeam: r.OwnerTeam, Agent: r.Agent, Target: r.Target, Source: r.Source, Budget: r.Budget, Bundle: r.Bundle,
		RequestHash: r.RequestHash, CreatedAt: now, UpdatedAt: now}
	if !r.Deadline.IsZero() {
		t.Deadline = kernel.Instant(r.Deadline)
	}
	return t, Change{Command: CmdCreate, To: Created, Mode: t.Mode, Version: 1, By: act.By, Epoch: act.Epoch, At: now}, nil
}

// Expect compares the version the caller saw (task_version CAS).
func (t *Task) Expect(version int64) error {
	if t.Version != version {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Visible reports whether actor sees the task: the owner and, by data scope, the owner's team
// (sees), the initiator and the task's AI agent.
func (t *Task) Visible(actor kernel.ActorRef, sees func(owner, team string) bool) bool {
	return sees(t.Owner, t.OwnerTeam) || actor.ID == t.Initiator.ID || (t.Agent != "" && actor.ID == t.Agent)
}

// Bound reports whether a quote root is bound to the task now.
func (t *Task) Bound() bool { return t.Binding != nil && t.Binding.State == BindingBound }

// BoundQuote returns the quote root bound to the task now, or "".
func (t *Task) BoundQuote() string {
	if t.Bound() {
		return t.Binding.QuoteID
	}
	return ""
}

// admit checks that command moves the task from its state to to, and that act's actor kind is one
// the transition names.
func (t *Task) admit(command Command, to State, act Act) error {
	tr, ok := lookup(t.State, command, to)
	if !ok {
		return invalidState("SM-TSK-TASK has no transition " + string(t.State) + " → " + string(to) + " for " + string(command))
	}
	if !slices.Contains(tr.Actors, act.By.Kind) {
		return kernel.Reject(kernel.CodePermissionDenied, "/", string(command)+" from "+string(t.State)+" is not for a "+string(act.By.Kind)+" actor")
	}
	return nil
}

// first refuses command when the task's state has no transition for it at all, before the
// command's own checks.
func (t *Task) first(command Command) error {
	if !allows(t.State, command) {
		return invalidState(string(command) + " does not apply to a task in " + string(t.State))
	}
	return nil
}

// move changes the state after admit and returns the history entry.
func (t *Task) move(command Command, to State, act Act, reason, change string) Change {
	from := t.State
	now := kernel.Instant(act.At)
	t.State, t.Version, t.UpdatedAt = to, t.Version+1, now
	return Change{Command: command, From: from, To: to, Mode: t.Mode, ControlEpoch: t.ControlEpoch, ControlChange: change,
		Version: t.Version, By: act.By, Epoch: act.Epoch, At: now, Reason: reason}
}

// Scope moves a created task to scoping and a scoping task to running, which settles its scope
// (task.scope; SM-TSK-TASK created→scoping system, scoping→running human or system).
func (t *Task) Scope(note string, act Act) (Change, error) {
	to := Scoping
	if t.State == Scoping {
		to = Running
	}
	if err := t.admit(CmdScope, to, act); err != nil {
		return Change{}, err
	}
	if to == Running {
		t.ScopeSettledAt = kernel.Instant(act.At)
	}
	return t.move(CmdScope, to, act, note, ""), nil
}

// WaitRequest is a validated task.wait.
type WaitRequest struct {
	Wait        string
	Reason      string
	Responsible string
	Items       []string
	Awaiting    *Ref
	DueAt       time.Time
}

// Wait makes the task wait (task.wait; SM-TSK-TASK: scoping, running, verifying → waiting_input;
// running, verifying → waiting_approval; running → waiting_external; system). The wait records its
// reason, the responsible person and what it waits for; its end lies after now.
func (t *Task) Wait(r WaitRequest, act Act) (Change, error) {
	var to State
	switch r.Wait {
	case WaitInput:
		to = WaitingInput
	case WaitApproval:
		to = WaitingApproval
	case WaitExternal:
		to = WaitingExternal
	default:
		return Change{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/wait", "")
	}
	if err := t.first(CmdWait); err != nil {
		return Change{}, err
	}
	if err := t.admit(CmdWait, to, act); err != nil {
		return Change{}, err
	}
	now := kernel.Instant(act.At)
	if !r.DueAt.IsZero() && !kernel.Instant(r.DueAt).After(now) {
		return Change{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/due_at", "The wait would end before it begins")
	}
	c := &WaitCondition{Kind: r.Wait, Reason: r.Reason, Responsible: r.Responsible, Items: slices.Clone(r.Items), Since: now}
	if r.Awaiting != nil {
		a := *r.Awaiting
		c.Awaiting = &a
	}
	if !r.DueAt.IsZero() {
		c.DueAt = kernel.Instant(r.DueAt)
	}
	t.WaitCond, t.PauseCond, t.PausedFrom = c, nil, ""
	return t.move(CmdWait, to, act, r.Reason, ""), nil
}

// Resume resumes the task (task.resume). A waiting task resumes to running when what it waited for
// arrives (system; the arrival is recorded). A paused task resumes to the waiting state it was
// paused in, or else to running (human, with a reason), once no action is submitted or unknown
// (SM-TSK-TASK "已有副作用已对账"). A task that reaches running by a resume before its scope was
// settled — scoping → waiting_input → running when the missing items arrive, or paused before scoping
// ended and resumed by a human — has its scope settled by that resume (baseline 16.1: scoping→running
// is the user's or a restricted flow's scope confirmation once the critical items are cleared).
func (t *Task) Resume(reason string, arrival *Ref, unsettled int, act Act) (Change, error) {
	if err := t.first(CmdResume); err != nil {
		return Change{}, err
	}
	to := Running
	if t.State == Paused && t.PausedFrom.Waiting() {
		to = t.PausedFrom
	}
	if err := t.admit(CmdResume, to, act); err != nil {
		return Change{}, err
	}
	if t.State.Waiting() {
		if arrival == nil {
			return Change{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/arrival", "A waiting task resumes on what arrived")
		}
		if reason == "" {
			reason = "arrived: " + arrival.Kind + " " + arrival.ID
		}
	} else {
		if reason == "" {
			return Change{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/reason", "A paused task resumes once its cause is dealt with")
		}
		if unsettled > 0 {
			return Change{}, fail(CodeExternalResultUnknown, "/args/task_id", "An action of the task is submitted or unknown")
		}
	}
	if to == Running {
		t.WaitCond = nil
		if t.ScopeSettledAt.IsZero() {
			t.ScopeSettledAt = kernel.Instant(act.At)
		}
	}
	t.PauseCond, t.PausedFrom = nil, ""
	return t.move(CmdResume, to, act, reason, ""), nil
}

// Verify moves a running task to verifying (task.verify; system): its required actions have
// reliable results, so no action may be submitted or unknown (TSK-INV-003).
func (t *Task) Verify(unsettled int, act Act) (Change, error) {
	if err := t.admit(CmdVerify, Verifying, act); err != nil {
		return Change{}, err
	}
	if unsettled > 0 {
		return Change{}, fail(CodeExternalResultUnknown, "/args/task_id", "An action of the task is submitted or unknown")
	}
	return t.move(CmdVerify, Verifying, act, "", ""), nil
}

// Verdict is what goal verification found: Outcome satisfied, unsatisfied, unknown or error, and
// the codes of the required assertions that are not satisfied; Reason, when given, is the reason
// the task records for waiting (RecordVerification names each unmet assertion with its code).
type Verdict struct {
	Outcome string
	Unmet   []string
	Reason  string
}

// Verdict outcomes (GoalReadiness v1, baseline 15.4).
const (
	Satisfied   = "satisfied"
	Unsatisfied = "unsatisfied"
	Unknown     = "unknown"
	Error       = "error"
)

// CheckVerifyGoal decides whether goal verification may start, before TRD's GoalReadiness v1 is
// asked: the task is verifying and the actor a system actor (goal.verify).
func (t *Task) CheckVerifyGoal(act Act) error {
	if err := t.first(CmdVerifyGoal); err != nil {
		return err
	}
	return t.admit(CmdVerifyGoal, Completed, act)
}

// VerifyGoal ends goal verification (goal.verify; system). The task completes only when every
// required assertion is satisfied and no action is submitted or unknown (TSK-INV-001, TSK-INV-003);
// completion records the completion actor: manual for a manual task, else ai_with_required_review —
// never ai_unassisted in P0. An unsatisfied assertion sends it back to waiting_input, an unknown or
// failed one (or an unsettled action) to waiting_external, with the unmet assertions as the items
// the owner acts on.
func (t *Task) VerifyGoal(v Verdict, unsettled int, act Act) (Change, error) {
	if err := t.first(CmdVerifyGoal); err != nil {
		return Change{}, err
	}
	var to State
	reason := "goal assertions " + v.Outcome
	switch {
	case v.Outcome == Satisfied && len(v.Unmet) == 0 && unsettled == 0:
		to = Completed
	case v.Outcome == Satisfied && len(v.Unmet) == 0:
		to, reason = WaitingExternal, "an action of the task is submitted or unknown"
	case v.Outcome == Unsatisfied:
		to = WaitingInput
	case v.Outcome == Unknown || v.Outcome == Error || v.Outcome == Satisfied:
		to = WaitingExternal
	default:
		return Change{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/", "Unknown verification outcome "+v.Outcome)
	}
	if v.Reason != "" && to != Completed {
		reason = v.Reason
	}
	if err := t.admit(CmdVerifyGoal, to, act); err != nil {
		return Change{}, err
	}
	now := kernel.Instant(act.At)
	if to == Completed {
		t.CompletionActor = CompletionAIWithRequiredReview
		if t.Mode == ModeManual {
			t.CompletionActor = CompletionManual
		}
		t.WaitCond = nil
	} else {
		wait := WaitInput
		if to == WaitingExternal {
			wait = WaitExternal
		}
		t.WaitCond = &WaitCondition{Kind: wait, Reason: reason, Responsible: t.Owner, Items: slices.Clone(v.Unmet), Since: now}
	}
	return t.move(CmdVerifyGoal, to, act, reason, ""), nil
}

// PauseRequest is a validated task.pause.
type PauseRequest struct {
	Cause           string
	Reason          string
	Responsible     string // empty: the owner
	ResumeCondition string
}

// Pause pauses the task (task.pause; human or system), recording the cause, the reason, who acts
// and the resume condition, and the state it was paused in.
func (t *Task) Pause(r PauseRequest, act Act) (Change, error) {
	if err := t.admit(CmdPause, Paused, act); err != nil {
		return Change{}, err
	}
	who := r.Responsible
	if who == "" {
		who = t.Owner
	}
	t.PausedFrom = t.State
	t.PauseCond = &PauseCondition{Cause: r.Cause, Reason: r.Reason, Responsible: who, ResumeCondition: r.ResumeCondition, Since: kernel.Instant(act.At)}
	return t.move(CmdPause, Paused, act, r.Reason, ""), nil
}

// recordEpoch records the control epoch TRD returned for a control change of the task's root. TRD
// moves the epoch with every change, so an epoch not above the recorded one is stale.
func (t *Task) recordEpoch(epoch int64) error {
	if epoch <= t.ControlEpoch {
		return fail(CodeControlEpochStale, "/args/task_id", "TRD returned a control epoch the task recorded before")
	}
	t.ControlEpoch = epoch
	if t.Binding != nil {
		t.Binding.Epoch = epoch
	}
	return nil
}

// CheckStop decides whether command (task.take_control, task.handoff or task.cancel) may stop the
// task, before TRD is asked to stop a bound root: take_control applies to an AI task (a manual one
// is people's already), handoff to a manual task with an AI agent to hand it back to (SM-TSK-TASK
// "control_mode=manual"); all three are human only.
func (t *Task) CheckStop(command Command, act Act) error {
	if err := t.first(command); err != nil {
		return err
	}
	switch command {
	case CmdTakeControl:
		if t.Mode == ModeManual {
			return invalidState("The task is under manual control already")
		}
	case CmdHandoff:
		switch {
		case t.Mode != ModeManual:
			return invalidState("Only a manual task is handed back")
		case t.Agent == "":
			return invalidState("The task has no AI agent to hand it back to")
		}
	case CmdCancel:
	default:
		return invalidState(string(command) + " does not stop a task")
	}
	return t.admit(command, Stopping, act)
}

// stop moves the task to stopping for command. A task with a bound root records epoch, the control
// epoch TRD returned for object_control.stop, which fenced the old executor before this change
// (TSK-INV-002); a task without one does not call TRD and records only the control change (epoch 0).
func (t *Task) stop(command Command, purpose, reason string, epoch int64, act Act) (Change, error) {
	if err := t.CheckStop(command, act); err != nil {
		return Change{}, err
	}
	switch {
	case t.Bound():
		if err := t.recordEpoch(epoch); err != nil {
			return Change{}, err
		}
	case epoch != 0:
		return Change{}, invalidState("No quote root is bound to the task")
	}
	from := t.State
	if from == Paused {
		from = t.PausedFrom
	}
	t.Stop = &Stop{Purpose: purpose, From: from, Reason: reason, By: act.By, At: kernel.Instant(act.At)}
	t.PauseCond, t.PausedFrom = nil, ""
	if !from.Waiting() {
		t.WaitCond = nil
	}
	return t.move(command, Stopping, act, reason, purpose), nil
}

// TakeControl takes an AI task over for people (task.take_control; human only; any state but
// stopping and the terminal ones).
func (t *Task) TakeControl(reason string, epoch int64, act Act) (Change, error) {
	return t.stop(CmdTakeControl, StopTakeControl, reason, epoch, act)
}

// Handoff hands a manual task back to its AI agent (task.handoff; human only).
func (t *Task) Handoff(reason string, epoch int64, act Act) (Change, error) {
	return t.stop(CmdHandoff, StopHandoff, reason, epoch, act)
}

// Cancel requests the task's cancellation (task.cancel; human only): it stops new actions;
// quotes and orders in force stay with their own controlled commands.
func (t *Task) Cancel(reason string, epoch int64, act Act) (Change, error) {
	return t.stop(CmdCancel, StopCancel, reason, epoch, act)
}

// CheckFinish decides whether the stop barrier may complete (task.finish_stop; system), before TRD
// is asked to change a bound root's control: the task is stopping with a stop request, and no
// action is submitted or unknown (TSK-INV-003, EXTERNAL_RESULT_UNKNOWN otherwise). It returns the
// stop request.
func (t *Task) CheckFinish(unsettled int, act Act) (Stop, error) {
	if err := t.first(CmdFinishStop); err != nil {
		return Stop{}, err
	}
	s := t.Stop
	if s == nil {
		return Stop{}, invalidState("The task has no stop request")
	}
	to := Paused
	if s.Purpose == StopCancel {
		to = Cancelled
	}
	if err := t.admit(CmdFinishStop, to, act); err != nil {
		return Stop{}, err
	}
	if unsettled > 0 {
		return Stop{}, fail(CodeExternalResultUnknown, "/args/task_id", "An action of the task is submitted or unknown")
	}
	return *s, nil
}

// FinishStop completes the stop barrier (task.finish_stop; system): no action may be submitted or
// unknown (TSK-INV-003, EXTERNAL_RESULT_UNKNOWN otherwise). A take-over pauses the task in control
// mode manual, a hand-back in mode ai; a bound root records epoch, the control epoch TRD returned
// for object_control.enter_manual or object_control.handoff. A cancellation ends the task
// cancelled; TRD is not called (its root stays stopping until task.release).
func (t *Task) FinishStop(unsettled int, epoch int64, act Act) (Change, error) {
	s, err := t.CheckFinish(unsettled, act)
	if err != nil {
		return Change{}, err
	}
	to := Paused
	if s.Purpose == StopCancel {
		to = Cancelled
	}
	switch {
	case to == Cancelled:
		if epoch != 0 {
			return Change{}, invalidState("A cancellation leaves the root's control to task.release")
		}
		t.Termination = &Termination{Cause: EndCancelled, Reason: s.Reason, By: s.By, At: kernel.Instant(act.At)}
		t.WaitCond = nil
	case t.Bound():
		if err := t.recordEpoch(epoch); err != nil {
			return Change{}, err
		}
	case epoch != 0:
		return Change{}, invalidState("No quote root is bound to the task")
	}
	switch s.Purpose {
	case StopTakeControl:
		t.Mode = ModeManual
	case StopHandoff:
		t.Mode = ModeAI
	}
	if to == Paused {
		t.PausedFrom = s.From
	}
	return t.move(CmdFinishStop, to, act, s.Reason, ChangeFinishStop), nil
}

// CloseUnfulfilled closes the task without its goal (task.close_unfulfilled; human or system): the
// customer declined, the quote expired, it was superseded or is no longer pursued. No conflict may
// be open: no action submitted or unknown. It is never counted as success (TSK-INV-001).
func (t *Task) CloseUnfulfilled(cause, reason string, unsettled int, act Act) (Change, error) {
	if err := t.admit(CmdCloseUnfulfilled, ClosedUnfulfilled, act); err != nil {
		return Change{}, err
	}
	if unsettled > 0 {
		return Change{}, fail(CodeExternalResultUnknown, "/args/task_id", "An action of the task is submitted or unknown")
	}
	t.Termination = &Termination{Cause: cause, Reason: reason, By: act.By, At: kernel.Instant(act.At)}
	t.WaitCond, t.PauseCond, t.PausedFrom = nil, nil, ""
	return t.move(CmdCloseUnfulfilled, ClosedUnfulfilled, act, reason, ""), nil
}

// Fail ends the task as failed (task.fail; human only): its side effects are reconciled (no action
// submitted or unknown) and the person responsible for what follows is named.
func (t *Task) Fail(reason, responsible string, unsettled int, act Act) (Change, error) {
	if err := t.admit(CmdFail, Failed, act); err != nil {
		return Change{}, err
	}
	if unsettled > 0 {
		return Change{}, fail(CodeExternalResultUnknown, "/args/task_id", "An action of the task is submitted or unknown")
	}
	t.Termination = &Termination{Cause: EndFailed, Reason: reason, Responsible: responsible, By: act.By, At: kernel.Instant(act.At)}
	t.WaitCond, t.PauseCond, t.PausedFrom = nil, nil, ""
	return t.move(CmdFail, Failed, act, reason, ""), nil
}

// CheckBind decides whether the task may bind quote root quote before TRD is asked: a running AI
// task without a bound root. The root is the one the task's target names or, when the target names
// none, one an action of the task created — created reports that a committed action of the task
// created it (SM-TSK-TASK task.bind_object "本任务的动作已创建报价根"; S4 C1, B1-D09): a task never binds a
// root that neither the person who set its target nor its own steps brought to it
// (REFERENCE_INVALID).
func (t *Task) CheckBind(quote string, created bool, act Act) error {
	if err := t.admit(CmdBindObject, Running, act); err != nil {
		return err
	}
	switch {
	case t.Mode != ModeAI:
		return invalidState("Only an AI task binds a quote root")
	case t.Bound():
		return invalidState("A quote root is bound to the task already")
	case t.Target.QuoteID != "" && t.Target.QuoteID != quote:
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/quote_id", "The task targets another quote root")
	case t.Target.QuoteID == "" && !created:
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/quote_id", "No action of the task created the quote root")
	}
	return nil
}

// BindObject records the binding of quote root quote at epoch, the control epoch TRD's
// object_control.bind returned (task.bind_object; system; running → running); created as for
// CheckBind.
func (t *Task) BindObject(quote string, created bool, epoch int64, act Act) (Change, error) {
	if err := t.CheckBind(quote, created, act); err != nil {
		return Change{}, err
	}
	if err := t.recordEpoch(epoch); err != nil {
		return Change{}, err
	}
	t.Binding = &Binding{QuoteID: quote, State: BindingBound, Epoch: epoch, BoundAt: kernel.Instant(act.At)}
	t.Target.QuoteID = quote
	return t.move(CmdBindObject, Running, act, "", ChangeBind), nil
}

// CheckRelease decides whether the task may give its root back before TRD is asked (task.release;
// human only): the task ended, or is paused in manual mode; a root is bound; no action is submitted
// or unknown.
func (t *Task) CheckRelease(unsettled int, act Act) error {
	if err := t.admit(CmdRelease, t.State, act); err != nil {
		return err
	}
	switch {
	case t.State == Paused && t.Mode != ModeManual:
		return invalidState("Only a manual task gives its root back while paused")
	case !t.Bound():
		return invalidState("No quote root is bound to the task")
	case unsettled > 0:
		return fail(CodeExternalResultUnknown, "/args/task_id", "An action of the task is submitted or unknown")
	}
	return nil
}

// Release records that the task's root was given back at epoch, the control epoch TRD's
// object_control.release returned. The lifecycle does not change.
func (t *Task) Release(reason string, epoch int64, unsettled int, act Act) (Change, error) {
	if err := t.CheckRelease(unsettled, act); err != nil {
		return Change{}, err
	}
	if err := t.recordEpoch(epoch); err != nil {
		return Change{}, err
	}
	t.Binding.State, t.Binding.ReleasedAt = BindingReleased, kernel.Instant(act.At)
	return t.move(CmdRelease, t.State, act, reason, ChangeRelease), nil
}

// Claim decides whether actor receives the claim of a task step (D-2, TSK-INV-004): nobody while
// the task is stopping or has ended (Q01 TASK_NOT_RUNNING); a human in either mode; the task's AI
// agent in mode ai while the task is scoping or running (CONTROL_MODE_MISMATCH in manual mode;
// waiting_approval forbids a side effect before the approval, paused stops the AI, verifying is the
// system's); no other actor.
func (t *Task) Claim(actor kernel.ActorRef) error {
	switch {
	case t.State.Terminal():
		return invalidState("The task has ended")
	case t.State == Stopping:
		return invalidState("The task is stopping")
	}
	switch actor.Kind {
	case kernel.ActorHuman:
		return nil
	case kernel.ActorAI:
		switch {
		case t.Mode != ModeAI:
			return fail(CodeControlModeMismatch, "/", "A manual task issues claims to people only")
		case actor.ID != t.Agent:
			return kernel.Reject(kernel.CodePermissionDenied, "/", "The actor is not the task's AI agent")
		case t.State != Scoping && t.State != Running:
			return invalidState("The task's AI agent acts while the task is scoping or running")
		}
		return nil
	}
	return kernel.Reject(kernel.CodePermissionDenied, "/", "Claims are issued to people and the task's AI agent")
}
