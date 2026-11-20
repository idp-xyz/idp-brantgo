package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// ActionExecution (tactical model TSK-AGG-ActionExecution, TSK-INV-003, X-INV-001, X-INV-003;
// Context Map TSK action.record_attempt, action.reconcile, ActionReconciled, R18 and R66–R69
// ActionReconciliation v1; owner decision S4 D-11; baseline 17.1 and 17.2; WBS S4-02).
//
// Every task step that writes another context — EVD, INQ, SUP, CST or TRD — is one action of one
// business intent, recorded before its command is sent:
//
//	action.record_attempt  human or AI (the task's agent): {task_id, business_intent_id, target
//	                       {context, command, request_hash}, control_epoch, outcome, code,
//	                       delegation_id, grant_id}. outcome "submitted" before the command is
//	                       sent: registers the intent's action on its first attempt and opens the
//	                       next one; the actor must hold the task's claim (Claim's rules) at
//	                       control_epoch, the epoch of that claim (CONTROL_EPOCH_STALE otherwise).
//	                       outcome "rejected" (code: the target's) or "unknown" (the answer was
//	                       lost; code optional) after it, for the submitted attempt.
//	action.reconcile       human or AI (the task's agent): {task_id, action_id}. Asks the target
//	                       what became of the action's key and hash and records it; publishes
//	                       ActionReconciled.
//
// The target context's ActionReconciliation v1 is asked by (command, key, hash) before TSK's
// transaction, on every record and every reconciliation, as the caller (X-INV-003: the caller's
// read permission and data scope at the target now). Its answer is the truth TSK records:
// committed — the target's stored receipt, the original result and never a new qualification
// (CT11: recording another attempt of an intent that committed answers with that result and opens
// none; the command is not sent again) —; payload_conflict — the key holds another request: the
// action is final, nothing is sent again —; not_committed — nothing under the key: a submission
// opens the attempt, the executor's rejection or lost answer is recorded, a reconciliation records
// not_committed and the next attempt sends the same key, which the target's ledger executes once.
//
// The idempotency key of the target command is the action's ID, which TSK derives from the business
// intent (ActionIDOf): one intent is one action and one key (TSK-INV-003; the same intent for
// another task, context, command or request is IDEMPOTENCY_PAYLOAD_CONFLICT). An action with a
// submitted or unknown attempt blocks a new attempt of its intent (EXTERNAL_RESULT_UNKNOWN) and the
// task's stop barrier, verification, release and ends.
const (
	CommandRecordAttempt   = "action.record_attempt"
	CommandReconcileAction = "action.reconcile"

	// Reads: an action with its log, the actions of a task.
	CommandGetAction   = "action.get"
	CommandTaskActions = "action.list"

	// ReconcileAudit is the name a reconciliation of TSK's own commands (Reconciler) has in TSK's
	// audit trail, apart from the command action.reconcile.
	ReconcileAudit = "tsk.reconcile"

	EventActionReconciled = "idp-brant.tsk.action_reconciled.v1"
)

// The contexts a task step writes (Context Map R18, R66–R69).
const (
	TargetEVD = "evd"
	TargetINQ = "inq"
	TargetSUP = "sup"
	TargetCST = "cst"
	TargetTRD = "trd"
)

// Action states; the outcomes action.record_attempt records are the first three.
const (
	ActionSubmitted    = "submitted"
	ActionRejected     = "rejected"
	ActionUnknown      = "unknown"
	ActionNotCommitted = "not_committed"
	ActionCommitted    = "committed"
	ActionConflict     = "conflict"
)

// MaxTargetCommand is the longest command name a target names.
const MaxTargetCommand = 100

// ActionIDOf returns the ID of the action of a business intent, which is also the idempotency key
// its command carries (baseline 17.1: fixed before the side effect, the same for every attempt and
// every plan that pursues the intent, never chosen by a model).
func ActionIDOf(businessIntentID string) string {
	sum := sha256.Sum256([]byte("brant.tsk.action\x1f" + businessIntentID))
	return "tska." + hex.EncodeToString(sum[:20])
}

// TargetCommand is the command an action sends, as the target context's RequestOf returns it
// (kernel.ActionRequest without the key, which is the action's).
type TargetCommand struct {
	Context     string
	Command     string
	RequestHash string
}

// RecordAttempt records an attempt of the action of BusinessIntentID: Outcome "submitted" before
// its command is sent, "rejected" with the target's Code or "unknown" after. ControlEpoch is the
// control epoch of the task's claim the step carries (0 for a task without a bound root).
// DelegationID and GrantID name the AI delegation and the approval grant a submission uses; they are
// recorded, never authority (baseline 17.1). Human or AI.
type RecordAttempt struct {
	IdempotencyKey   string
	TaskID           string
	BusinessIntentID string
	Target           TargetCommand
	ControlEpoch     int64
	Outcome          string
	Code             string
	DelegationID     string
	GrantID          string
}

// ReconcileAction asks the target what became of action ActionID of task TaskID and records it.
// Human or AI.
type ReconcileAction struct {
	IdempotencyKey string
	TaskID         string
	ActionID       string
}

func (RecordAttempt) tskCommand()   {}
func (ReconcileAction) tskCommand() {}

func targetContext(c string) bool {
	switch c {
	case TargetEVD, TargetINQ, TargetSUP, TargetCST, TargetTRD:
		return true
	}
	return false
}

// CommandName reports whether s has the form of a command name: lower-case words joined by dots,
// at most MaxTargetCommand characters (the action ledger's form).
func CommandName(s string) bool {
	if len(s) > MaxTargetCommand {
		return false
	}
	dot := strings.IndexByte(s, '.')
	if dot < 1 || dot == len(s)-1 {
		return false
	}
	word := func(w string, dots bool) bool {
		if w[0] < 'a' || w[0] > 'z' {
			return false
		}
		for i := 1; i < len(w); i++ {
			if c := w[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && (!dots || c != '.') {
				return false
			}
		}
		return true
	}
	return word(s[:dot], false) && word(s[dot+1:], true)
}

// ErrorCode reports whether s has the form of a rejection code: an upper-case letter, then 1 to 63
// upper-case letters, digits and underscores.
func ErrorCode(s string) bool {
	if len(s) < 2 || len(s) > 64 || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// Validate checks the closed contract: a known context, a command name and a request hash; a known
// outcome; a code for a rejection, none for a submission; references only on a submission.
func (c RecordAttempt) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.TaskID):
		return schema("/args/task_id")
	case !kernel.ValidIdentifier(c.BusinessIntentID):
		return schema("/args/business_intent_id")
	case !targetContext(c.Target.Context):
		return schema("/args/target/context")
	case !CommandName(c.Target.Command):
		return schema("/args/target/command")
	case !kernel.ValidRequestHash(c.Target.RequestHash):
		return schema("/args/target/request_hash")
	case c.ControlEpoch < 0:
		return schema("/args/control_epoch")
	case c.Code != "" && !ErrorCode(c.Code):
		return schema("/args/code")
	case c.DelegationID != "" && !kernel.ValidIdentifier(c.DelegationID):
		return schema("/args/delegation_id")
	case c.GrantID != "" && !kernel.ValidIdentifier(c.GrantID):
		return schema("/args/grant_id")
	}
	switch c.Outcome {
	case ActionSubmitted:
		if c.Code != "" {
			return schema("/args/code")
		}
	case ActionRejected, ActionUnknown:
		if c.Outcome == ActionRejected && c.Code == "" {
			return schema("/args/code")
		}
		if c.DelegationID != "" {
			return schema("/args/delegation_id")
		}
		if c.GrantID != "" {
			return schema("/args/grant_id")
		}
	default:
		return schema("/args/outcome")
	}
	return nil
}

// Validate checks the closed contract.
func (c ReconcileAction) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.TaskID):
		return schema("/args/task_id")
	case !kernel.ValidIdentifier(c.ActionID):
		return schema("/args/action_id")
	}
	return nil
}

// TargetReceipt is the target context's stored receipt of a committed command: its own action ID,
// the object, its row version and status.
type TargetReceipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
}

// ActionReceipt answers action.record_attempt and action.reconcile. Receipt is TSK's: ObjectID the
// action's ID, RowVersion the action's version and Status its state after the command (stored for
// a replay). IdempotencyKey is the key the target command carries; Attempt the attempt the command
// opened or settled (0 when none). State and Result are the action as stored now: its state and,
// committed, the target's receipt — what the executor uses instead of sending again.
type ActionReceipt struct {
	Receipt
	IdempotencyKey string
	Attempt        int
	State          string
	Result         *TargetReceipt
}

// AttemptView is one row of an action's append-only log: the change that produced Version, the
// attempt it concerns, the command, the state after it, the target's answer (committed,
// payload_conflict or not_committed), the executor's code, the control epoch of the task's claim,
// the delegation and approval grant a submission named, the target's receipt (committed), who
// recorded it under which authority epoch, and when.
type AttemptView struct {
	Version        int64
	Attempt        int
	Command        string
	State          string
	Answer         string
	Code           string
	ControlEpoch   int64
	DelegationID   string
	GrantID        string
	Result         *TargetReceipt
	Actor          kernel.ActorRef
	AuthorityEpoch int64
	At             time.Time
}

// ActionView is an action as the reads show it, with its log.
type ActionView struct {
	ActionID         string
	BusinessIntentID string
	TaskID           string
	Target           TargetCommand
	IdempotencyKey   string
	State            string
	Version          int64
	Attempts         int
	Result           *TargetReceipt
	RegisteredBy     kernel.ActorRef
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Log              []AttemptView
}

// ActionCommands is TSK's command entry point for ActionExecution (errors as Commands').
type ActionCommands interface {
	RecordAttempt(ctx context.Context, cmd RecordAttempt) (ActionReceipt, error)
	ReconcileAction(ctx context.Context, cmd ReconcileAction) (ActionReceipt, error)
}

// ActionQueries reads actions, audited, with tsk.read: an action or the actions of a task the
// caller sees (the task's visibility; NOT_FOUND otherwise).
type ActionQueries interface {
	// GetAction returns an action with its log (action.get).
	GetAction(ctx context.Context, actionID string) (ActionView, error)
	// TaskActions returns the actions of a task with their logs, oldest first (action.list): a
	// recovering executor finds the open ones there.
	TaskActions(ctx context.Context, taskID string) ([]ActionView, error)
}
