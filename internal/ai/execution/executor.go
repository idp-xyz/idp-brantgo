// Package execution is the single trusted path of task-bound commands (plan S4-03 execution; owner
// decision D-2; TSK-INV-003, TSK-INV-004, AI-INV-003, AI-INV-004; baseline 17.1–17.4). Every step of
// a task that writes EVD, INQ, SUP, CST or TRD — the AI runner's tool calls and the steps people take
// on a task alike — goes through Executor.Run:
//
//  1. the task's claim from TSK (task.claim: D-2's rules — the task's AI agent while scoping or
//     running in control mode ai, people in either mode, nobody while stopping or ended);
//  2. the command's ledger identity from its own context (ActionReconciliation v1 RequestOf);
//  3. the attempt recorded in TSK before anything is sent (action.record_attempt submitted: one
//     business intent is one action, and its ID is the command's idempotency key, baseline 17.1);
//     an intent that committed before answers with its result and nothing is sent (CT11);
//  4. for an AI step, the gateway's admission with that key (delegation re-checked, approval grant
//     used);
//  5. the command sent with the key and, on a bound quote root, the task's control claim
//     (trdapi.WithControl), so TRD fences it (TRD-INV-015);
//  6. the outcome recorded: TSK reconciles the action with the target's ledger (committed), or the
//     executor records the target's refusal (rejected) or the lost answer (unknown), which a later
//     reconciliation settles.
//
// The receipt says how far a step got (UL AI-Receipt): ACCEPTED — the intent is recorded and the
// step admitted, nothing is known to have happened yet; COMMITTED — the owning context answered with
// its receipt; VERIFIED — TSK recorded the action as committed from the target's own ledger with
// that receipt. ACCEPTED is not VERIFIED, and no receipt completes a task: only goal.verify does.
package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Stage is how far a step got (UL AI-Receipt).
type Stage string

const (
	// Accepted: the intent is recorded in TSK and the step admitted; nothing is known to have been
	// committed.
	Accepted Stage = "ACCEPTED"
	// Committed: the owning context answered the command with its receipt.
	Committed Stage = "COMMITTED"
	// Verified: TSK recorded the action as committed from the owning context's ledger.
	Verified Stage = "VERIFIED"
)

// Tasks is what the executor needs of TSK: the claim, the task, and ActionExecution.
type Tasks interface {
	tskapi.Queries
	tskapi.ActionCommands
	tskapi.ActionQueries
}

// Requests gives the ledger identity (command, request hash) of an api command at its context:
// each context's ActionReconciliation v1 RequestOf, which executes and reads nothing.
type Requests struct {
	EVD interface {
		RequestOf(evdapi.Command) (kernel.ActionRequest, error)
	}
	INQ interface {
		RequestOf(inqapi.Command) (kernel.ActionRequest, error)
	}
	SUP interface {
		RequestOf(supapi.CommandRequest) (kernel.ActionRequest, error)
	}
	CST interface {
		RequestOf(cstapi.Command) (kernel.ActionRequest, error)
	}
	TRD interface {
		RequestOf(trdapi.Command) (kernel.ActionRequest, error)
	}
}

// ControlCommandPrefix starts TRD's object control commands (object_control.bind, .stop, .enter_manual,
// .handoff, .release), which only TSK sends (R17, decision D-4): the executor refuses them.
const ControlCommandPrefix = "object_control."

// ErrNotACommand is a step whose command is not an api command of its context.
var ErrNotACommand = errors.New("execution: not a command of the step's context")

// RequestOf returns the ledger identity of cmd at context (tskapi.Target*).
func (r Requests) RequestOf(context string, cmd any) (kernel.ActionRequest, error) {
	bad := fmt.Errorf("%w: %s %T", ErrNotACommand, context, cmd)
	switch context {
	case tskapi.TargetEVD:
		if c, ok := cmd.(evdapi.Command); ok && r.EVD != nil {
			return r.EVD.RequestOf(c)
		}
	case tskapi.TargetINQ:
		if c, ok := cmd.(inqapi.Command); ok && r.INQ != nil {
			return r.INQ.RequestOf(c)
		}
	case tskapi.TargetSUP:
		if c, ok := cmd.(supapi.CommandRequest); ok && r.SUP != nil {
			return r.SUP.RequestOf(c)
		}
	case tskapi.TargetCST:
		if c, ok := cmd.(cstapi.Command); ok && r.CST != nil {
			return r.CST.RequestOf(c)
		}
	case tskapi.TargetTRD:
		if c, ok := cmd.(trdapi.Command); ok && r.TRD != nil {
			return r.TRD.RequestOf(c)
		}
	}
	return kernel.ActionRequest{}, bad
}

// Step is one task-bound command.
type Step struct {
	TaskID string
	// BusinessIntentID names the business action (baseline 17.1): the same intent is the same
	// action and the same idempotency key in every attempt and every plan. The AI runner derives it
	// from the request (IntentOf); a person's client names it.
	BusinessIntentID string
	// Context is the owning context of the command (tskapi.TargetEVD, ...).
	Context string
	// Command is the api command value; the executor sets its IdempotencyKey to the action's.
	Command any
	// RequestHash, when set, is the hash a caller already bound (the gateway's, which an approval
	// grant binds); the owning context's must equal it.
	RequestHash string
	// DelegationID and GrantID are recorded with the submission of an AI step (never authority).
	DelegationID string
	GrantID      string
	// Admit, when set, clears the step with the action's key just before it is sent (the gateway's
	// Admit for an AI step: the delegation re-checked and the approval grant used). Its refusal is
	// recorded as the attempt's rejection; nothing is sent.
	Admit func(ctx context.Context, key string) error
	// Send executes the command, which carries the key, at its context with the Principal and claims
	// of ctx.
	Send func(ctx context.Context, cmd any) (capability.Result, error)
}

// Receipt is the executor's answer for a step (UL AI-Receipt).
type Receipt struct {
	Stage            Stage
	TaskID           string
	BusinessIntentID string
	ActionID         string // TSK's action, also the command's idempotency key
	Attempt          int    // the attempt this run opened or found settled; 0 when none
	Target           tskapi.TargetCommand
	ControlEpoch     int64              // the epoch of the claim the step carried (0: no bound root)
	Result           *capability.Result // the owning context's answer to this run's send
	Stored           *tskapi.TargetReceipt
	// Replayed: the intent had committed before; the stored result is returned and nothing was sent
	// (CT11).
	Replayed bool
}

// Executor runs task-bound steps.
type Executor struct {
	Tasks    Tasks
	Requests Requests
}

// Run runs step s as the Principal of ctx. Refusals are the owning context's request identity
// (SCHEMA_INVALID), an object control command (PERMISSION_DENIED: only TSK changes a root's
// control), the claim's (TSK), TSK's record (EXTERNAL_RESULT_UNKNOWN only after a
// reconciliation did not settle it, CONTROL_EPOCH_STALE, IDEMPOTENCY_PAYLOAD_CONFLICT for an intent
// that names another request or whose key holds another request at the target), the admission's
// and the owning context's. An answer lost on the way back is model.ErrUnavailable (TOOL_UNAVAILABLE)
// once a reconciliation finds nothing committed; the next run of the same intent sends the same key.
// The receipt is returned with every error, at the stage the step reached.
func (e Executor) Run(ctx context.Context, s Step) (Receipt, error) {
	if err := s.check(); err != nil {
		return Receipt{}, err
	}
	r := Receipt{TaskID: s.TaskID, BusinessIntentID: s.BusinessIntentID, ActionID: tskapi.ActionIDOf(s.BusinessIntentID)}
	req, err := e.Requests.RequestOf(s.Context, s.Command)
	if err != nil {
		return r, err
	}
	if strings.HasPrefix(req.Command, ControlCommandPrefix) {
		// Only TSK changes a root's control (R17, D-4): never a task step.
		return r, kernel.Reject(kernel.CodePermissionDenied, "/command", "object control changes only through TSK")
	}
	if s.RequestHash != "" && s.RequestHash != req.RequestHash {
		return r, fmt.Errorf("execution: %s hashes to %s at %s, the step bound %s", req.Command, req.RequestHash, s.Context, s.RequestHash)
	}
	claim, err := e.Tasks.Claim(ctx, s.TaskID)
	if err != nil {
		return r, err
	}
	r.ControlEpoch = claim.ControlEpoch
	r.Target = tskapi.TargetCommand{Context: s.Context, Command: req.Command, RequestHash: req.RequestHash}
	rec, err := e.submit(ctx, s, claim, r.Target, r.ActionID)
	if err != nil {
		return r, err
	}
	r.Attempt = rec.Attempt
	switch rec.State {
	case tskapi.ActionCommitted:
		r.Stage, r.Stored, r.Replayed = Verified, rec.Result, true
		return r, nil
	case tskapi.ActionConflict:
		return r, kernel.Reject(kernel.CodeIdempotencyPayloadConflict, "/business_intent_id", "the action's key holds another request")
	}
	r.Stage = Accepted
	key, version := rec.IdempotencyKey, rec.RowVersion
	if s.Admit != nil {
		if err = s.Admit(ctx, key); err != nil {
			return e.refused(ctx, s, r, err)
		}
	}
	cmd, err := WithKey(s.Command, key)
	if err != nil {
		return r, err
	}
	sendCtx := ctx
	if claim.QuoteID != "" && claim.ControlEpoch > 0 {
		if sendCtx, err = trdapi.WithControl(ctx, trdapi.ControlClaim{TaskID: claim.TaskID, ControlEpoch: claim.ControlEpoch}); err != nil {
			return r, err
		}
	}
	res, err := s.Send(sendCtx, cmd)
	if err != nil {
		if _, ok := kernel.AsRejection(err); ok {
			return e.refused(ctx, s, r, err)
		}
		return e.lost(ctx, s, r, err)
	}
	r.Stage, r.Result = Committed, &res
	return e.verify(ctx, s, r, version), nil
}

func (s Step) check() error {
	switch {
	case !kernel.ValidIdentifier(s.TaskID):
		return errors.New("execution: a step names its task")
	case !kernel.ValidIdentifier(s.BusinessIntentID):
		return kernel.Reject(kernel.CodeSchemaInvalid, "/business_intent_id", "")
	case s.Command == nil || s.Send == nil:
		return errors.New("execution: a step has a command and a way to send it")
	}
	return nil
}

// submit records the attempt before the send. An open attempt of the intent (a run that lost its
// answer or its process) is reconciled first, once.
func (e Executor) submit(ctx context.Context, s Step, claim tskapi.Claim, target tskapi.TargetCommand, actionID string) (tskapi.ActionReceipt, error) {
	for try := 0; ; try++ {
		attempts, version := 0, int64(0)
		if a, err := e.Tasks.GetAction(ctx, actionID); err == nil {
			attempts, version = a.Attempts, a.Version
		} else if rej, ok := kernel.AsRejection(err); !ok || rej.Code != kernel.CodeNotFound {
			return tskapi.ActionReceipt{}, err
		}
		rec, err := e.Tasks.RecordAttempt(ctx, tskapi.RecordAttempt{IdempotencyKey: recordKey(actionID, attempts+1, tskapi.ActionSubmitted),
			TaskID: s.TaskID, BusinessIntentID: s.BusinessIntentID, Target: target, ControlEpoch: claim.ControlEpoch,
			Outcome: tskapi.ActionSubmitted, DelegationID: s.DelegationID, GrantID: s.GrantID})
		if rej, ok := kernel.AsRejection(err); ok && rej.Code == tskapi.CodeExternalResultUnknown && try == 0 {
			if _, err = e.reconcile(ctx, s.TaskID, actionID, version); err != nil {
				return tskapi.ActionReceipt{}, err
			}
			continue
		}
		return rec, err
	}
}

// refused records the refusal of an attempt (by the admission or the owning context) and returns
// it. When TSK finds the target committed after all, the step is verified.
func (e Executor) refused(ctx context.Context, s Step, r Receipt, refusal error) (Receipt, error) {
	rej, _ := kernel.AsRejection(refusal)
	rec, err := e.Tasks.RecordAttempt(ctx, tskapi.RecordAttempt{IdempotencyKey: recordKey(r.ActionID, r.Attempt, tskapi.ActionRejected),
		TaskID: s.TaskID, BusinessIntentID: s.BusinessIntentID, Target: r.Target, ControlEpoch: r.ControlEpoch,
		Outcome: tskapi.ActionRejected, Code: rej.Code})
	if err != nil {
		return r, errors.Join(refusal, err)
	}
	if rec.State == tskapi.ActionCommitted {
		r.Stage, r.Stored = Verified, rec.Result
		return r, nil
	}
	return r, refusal
}

// lost records an attempt whose answer did not arrive, then asks the target at once: committed
// verifies the step; otherwise the step is TOOL_UNAVAILABLE and the next run sends the same key.
func (e Executor) lost(ctx context.Context, s Step, r Receipt, cause error) (Receipt, error) {
	rec, err := e.Tasks.RecordAttempt(ctx, tskapi.RecordAttempt{IdempotencyKey: recordKey(r.ActionID, r.Attempt, tskapi.ActionUnknown),
		TaskID: s.TaskID, BusinessIntentID: s.BusinessIntentID, Target: r.Target, ControlEpoch: r.ControlEpoch,
		Outcome: tskapi.ActionUnknown, Code: codeOf(cause)})
	if err != nil {
		return r, errors.Join(model.ErrUnavailable, cause, err)
	}
	if rec.State == tskapi.ActionCommitted {
		r.Stage, r.Stored = Verified, rec.Result
		return r, nil
	}
	done, err := e.reconcile(ctx, s.TaskID, r.ActionID, rec.RowVersion)
	if err == nil && done.State == tskapi.ActionCommitted {
		r.Stage, r.Stored = Verified, done.Result
		return r, nil
	}
	return r, errors.Join(model.ErrUnavailable, cause, err)
}

// verify has TSK reconcile a committed send with the target's ledger. A reconciliation that fails
// leaves the step COMMITTED; the action stays open in TSK until the next reconciliation.
func (e Executor) verify(ctx context.Context, s Step, r Receipt, version int64) Receipt {
	done, err := e.reconcile(ctx, s.TaskID, r.ActionID, version)
	if err == nil && done.State == tskapi.ActionCommitted && done.Result != nil && r.Result != nil &&
		done.Result.ActionID == r.Result.ActionID && done.Result.ObjectID == r.Result.ObjectID {
		r.Stage, r.Stored = Verified, done.Result
	}
	return r
}

// reconcile has TSK reconcile an action at version (its version as the executor last saw it): the
// key is stable for that version, so a reconciliation whose answer was lost replays, and a later one
// asks the target anew.
func (e Executor) reconcile(ctx context.Context, task, actionID string, version int64) (tskapi.ActionReceipt, error) {
	return e.Tasks.ReconcileAction(ctx, tskapi.ReconcileAction{IdempotencyKey: recordKey(actionID, int(version), "reconcile"),
		TaskID: task, ActionID: actionID})
}

// Recover reconciles every submitted or unknown action of a task with its target (baseline 17.2:
// a restarted executor first learns what became of what was in flight; AI-INV-003 恢复前核查已有副作用)
// and returns the actions as they are afterwards.
func (e Executor) Recover(ctx context.Context, taskID string) ([]tskapi.ActionView, error) {
	actions, err := e.Tasks.TaskActions(ctx, taskID)
	if err != nil {
		return nil, err
	}
	reconciled := false
	for _, a := range actions {
		if a.State == tskapi.ActionSubmitted || a.State == tskapi.ActionUnknown {
			if _, err = e.reconcile(ctx, taskID, a.ActionID, a.Version); err != nil {
				return nil, err
			}
			reconciled = true
		}
	}
	if !reconciled {
		return actions, nil
	}
	return e.Tasks.TaskActions(ctx, taskID)
}

// recordKey is TSK's idempotency key of the executor's record of outcome for attempt of an action:
// stable, so a record whose answer was lost replays (B1's contract).
func recordKey(actionID string, attempt int, outcome string) string {
	sum := sha256.Sum256([]byte("brant.ai.exec\x1f" + actionID + "\x1f" + strconv.Itoa(attempt) + "\x1f" + outcome))
	return "ex." + hex.EncodeToString(sum[:20])
}

// codeOf is the code an unknown outcome records: the rejection's code when there is one, else
// COMMIT_UNCERTAIN.
func codeOf(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code
	}
	return "COMMIT_UNCERTAIN"
}

// KeyField is the api command field that carries the idempotency key.
const KeyField = capability.KeyField

// WithKey returns cmd, a struct value of an api command, with its IdempotencyKey set to key.
func WithKey(cmd any, key string) (any, error) {
	v := reflect.ValueOf(cmd)
	if v.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: %T", ErrNotACommand, cmd)
	}
	c := reflect.New(v.Type()).Elem()
	c.Set(v)
	f := c.FieldByName(KeyField)
	if !f.IsValid() || f.Kind() != reflect.String {
		return nil, fmt.Errorf("%w: %T has no %s", ErrNotACommand, cmd, KeyField)
	}
	f.SetString(key)
	return c.Interface(), nil
}

// IntentOf returns the business intent of an AI step: one per task, command, object and request,
// so a model asking again for the same request — in the same turn, a later one or after a restart —
// asks for the same action and gets its result (CT11), and a changed request is a new intent.
func IntentOf(taskID, command, objectID, requestHash string) string {
	sum := sha256.Sum256([]byte("brant.ai.intent\x1f" + taskID + "\x1f" + command + "\x1f" + objectID + "\x1f" + requestHash))
	return "ai." + hex.EncodeToString(sum[:20])
}
