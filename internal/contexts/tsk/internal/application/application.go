// Package application runs TSK's commands and queries on the command ledger. The lifecycle rules
// live in domain/task (SM-TSK-TASK); SQL lives in adapters/postgres.
//
// TSK's upstream contexts are TRD (Context Map R18: GoalReadiness v1, TRD integration events v1;
// and the object control commands TSK issues under R17), and EVD, INQ, SUP and CST
// (R66–R69, ActionReconciliation v1, from unit B1 on). None of these relationships is a guard edge:
// a command that needs TRD calls it through TSK's ports (ObjectControl, Readiness) before TSK's
// transaction begins (the ledger's Preparer step) and records what TRD answered inside it. TSK
// never calls another context's transaction owner and holds no lock of another context.
//
// The object control of a bound quote root is TRD's: TRD is the only writer of the root's control
// state and epoch and the fence every task step meets (MD-31). TSK records the epoch TRD returned
// (TSK-INV-002) and issues the claims task steps carry (D-2, TSK-INV-004). A take-over is two steps:
// TRD's stop first — the fence holds from then on — and TSK's record second; when the second step
// fails, the root stays fenced and the take-over is sent again: the TRD call of a task at a recorded
// control epoch has one idempotency key, so it replays what TRD did instead of acting again. When the
// task moved on before the retry (goal verification, a pause, a close) and another control change
// meets TRD's epoch instead, the port learns TRD's epoch for the task's own binding (adapters/trd).
// The stop barrier (task.finish_stop) reconciles the task's open actions with their targets itself
// before it changes the root's control (barrierCommand).
//
// Lock order inside TSK's transactions (ADR-BR-011): the task row FOR UPDATE, then the company's
// authority guard FOR SHARE (the ledger), then the grant rows of the actors a command names (owner,
// agent, responsible person: authorization.Qualify) FOR SHARE.
package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules is TSK's authorization policy. Each command's permission is its name (role templates
// V0.1); its rule admits the actor kinds of its SM-TSK-TASK transitions (task.create: human or
// system), and the domain checks the kind of the transition it takes. The tactical model's
// humanOnlyCommands name task.take_control, task.release, task.handoff, task.cancel and task.fail:
// authorization.HumanOnly. No command of a task's lifecycle admits an AI actor (AI-INV-004); the
// commands of ActionExecution, which record and reconcile a task step's side effect, admit its
// executor, human or the task's AI agent (actionRules). Reads need tsk.read — the claim of a task
// step is issued to humans and AI actors only —, the audit trail tsk.audit.
func Rules() []authorization.Rule {
	var rules []authorization.Rule
	for _, c := range task.Commands() {
		kinds := task.ActorKinds(c)
		if len(kinds) == 1 && kinds[0] == kernel.ActorHuman {
			rules = append(rules, authorization.HumanOnly(string(c), string(c)))
			continue
		}
		rules = append(rules, authorization.Rule{Command: string(c), Permission: string(c), ActorKinds: kinds})
	}
	return append(append(rules,
		authorization.Rule{Command: api.CommandGetTask, Permission: api.PermissionRead, ActorKinds: anyActor},
		authorization.Rule{Command: api.CommandClaim, Permission: api.PermissionRead, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		authorization.Rule{Command: api.CommandAudit, Permission: api.PermissionAudit, ActorKinds: anyActor},
		authorization.Rule{Command: api.CommandVerifications, Permission: api.PermissionRead, ActorKinds: anyActor},
	), actionRules()...)
}

// ControlRequest is one object control command TSK sends TRD for a task (R17): Key is its
// idempotency key, the same for every attempt of the task at Epoch (DeriveKey); Epoch is the
// control epoch the task recorded, which TRD compares and which the task's claim carries. Initiator
// and Agent name the task's delegation for a bind or a hand-back (S4 D-5: the binding holds while
// their grants are unchanged): the task's owner, the human it acts for, and its AI agent.
type ControlRequest struct {
	Key       string
	TaskID    string
	QuoteID   string
	Epoch     int64
	Reason    string
	Initiator string
	Agent     string
}

// ObjectControl is TSK's port to TRD's object control (R17 ObjectControl commands v1). Each call
// runs as the principal of ctx — the human who takes over, hands back, cancels or releases, the
// system actor that binds or completes the barrier — and returns the control epoch TRD answered
// with. A refusal is TRD's *kernel.Rejection, passed on unchanged.
type ObjectControl interface {
	Bind(ctx context.Context, r ControlRequest) (int64, error)
	Stop(ctx context.Context, r ControlRequest) (int64, error)
	EnterManual(ctx context.Context, r ControlRequest) (int64, error)
	Handoff(ctx context.Context, r ControlRequest) (int64, error)
	Release(ctx context.Context, r ControlRequest) (int64, error)
}

// Goal is what goal verification asks TRD about: the task's goal at its version, its target (the
// quote root judged and the customer and RFQ it must bind) and Bound, the quote root bound to the
// task now ("" when none), whose control TRD shows with the assessment.
type Goal struct {
	TaskID  string
	Code    string
	Version int64
	Target  task.Target
	Bound   string
}

// Verdict is what GoalReadiness v1 found for a goal (baseline 15.4): the records judged, every
// required assertion satisfied, unsatisfied, unknown or error with its checks and evidence, the
// outcome and the instant judged, and for a bound root TRD's control of it (assessor
// trd.goal_readiness.v1).
type Verdict = task.Assessment

// Readiness is TSK's port to TRD's GoalReadiness v1 (R18): the assessment of a task's goal against
// TRD's authoritative records (baseline 15.4) and, when Goal.Bound names a root, TRD's control of it
// read after the assessment. A refusal is TRD's *kernel.Rejection, passed on unchanged.
type Readiness interface {
	Assess(ctx context.Context, g Goal) (Verdict, error)
}

// Actions counts a task's submitted and unknown actions in TSK's transaction (TSK-INV-003): the stop
// barrier, verification, completion, release and the ends of a task wait for them. StoredActions
// counts ActionExecution's (S4 unit B1).
type Actions interface {
	Unsettled(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, taskID string) (int, error)
}

// NoActions is Actions while TSK has no ActionExecution aggregate (S4 unit B1): a task has no
// action, so none is submitted or unknown.
type NoActions struct{}

// Unsettled reports 0.
func (NoActions) Unsettled(context.Context, bentopg.Executor, kernel.Scope, string) (int, error) {
	return 0, nil
}

// ReadinessNotWired is Readiness before TRD publishes GoalReadiness v1 (S4 unit A3, bound by B2):
// goal.verify answers DEPENDENCY_NOT_SUPPORTED.
type ReadinessNotWired struct{}

// Assess refuses.
func (ReadinessNotWired) Assess(context.Context, Goal) (Verdict, error) {
	return Verdict{}, api.Reject(api.CodeDependencyNotSupported, "/", "TRD's GoalReadiness v1 is not wired")
}

// Upstream are the ports TSK calls TRD through, and the ActionReconciliation v1 ports of the
// contexts a task step writes (R18, R66–R69).
type Upstream struct {
	Control     ObjectControl
	Readiness   Readiness
	Reconcilers Reconcilers
}

// Complete reports whether every port is wired.
func (u Upstream) Complete() bool {
	return u.Control != nil && u.Readiness != nil && u.Reconcilers.Complete()
}

// Service runs TSK's use cases.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Upstream Upstream
	Actions  Actions
}

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// Visible reports whether d's actor sees the task a stored result names: the task itself, or for
// an ActionExecution command the action's task.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command string, id string) (bool, error) {
	if command == api.CommandRecordAttempt || command == api.CommandReconcileAction {
		e, err := postgres.LoadAction(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil || e == nil {
			return false, err
		}
		id = e.TaskID
	}
	t, err := postgres.LoadTask(ctx, exec, scope(ctx), id, postgres.NoLock)
	return err == nil && t != nil && t.Visible(d.Actor, d.Sees), err
}

// DeriveKey returns the idempotency key of TRD command for task at the control epoch the task
// recorded: every attempt of a TSK command that needs it — the first, and the ones after a failure
// between TRD's step and TSK's — sends TRD the same key, so TRD replays what it did once.
func DeriveKey(taskID, trdCommand string, epoch int64) string {
	sum := sha256.Sum256([]byte(taskID + "\x1f" + trdCommand + "\x1f" + strconv.FormatInt(epoch, 10)))
	return "tsk." + hex.EncodeToString(sum[:20])
}

// ControlReason is the reason TSK gives TRD for a control change of task at epoch; the person's own
// reason is in the task's history. It is fixed per key, so a retry sends the same request.
func ControlReason(taskID string, epoch int64) string {
	return "TSK task " + taskID + " at control epoch " + strconv.FormatInt(epoch, 10)
}

func (s Service) run(ctx context.Context, cmd action.Command, req api.Command, validation error, h action.Handler) (api.Receipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.Receipt{}, errors.Join(validation, err)
			}
		}
		return api.Receipt{}, validation
	}
	_, _, args := request(req)
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Receipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	return api.Receipt(res), err
}

// request returns the name, the idempotency key and the business arguments of cmd, which its
// request hash covers: everything but the key and the expected task version.
func request(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.CreateTask:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandCreate, key, c
	case api.ScopeTask:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandScope, key, c
	case api.WaitTask:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandWait, key, c
	case api.VerifyTask:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandVerify, key, c
	case api.VerifyGoal:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandVerifyGoal, key, c
	case api.PauseTask:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandPause, key, c
	case api.ResumeTask:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandResume, key, c
	case api.TakeControl:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandTakeControl, key, c
	case api.Handoff:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandHandoff, key, c
	case api.CancelTask:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandCancel, key, c
	case api.FinishStop:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandFinishStop, key, c
	case api.CloseUnfulfilled:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandCloseUnfulfilled, key, c
	case api.FailTask:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandFail, key, c
	case api.BindObject:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandBindObject, key, c
	case api.ReleaseObject:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRelease, key, c
	}
	return actionRequest(cmd)
}

func requestHash(cmd api.Command) string {
	_, _, args := request(cmd)
	h, _ := action.RequestHash(args)
	return h
}

// Reconcile reports what became of cmd, sent earlier with its idempotency key, without executing
// it (api.Reconciler; contract 06, 动作对账).
func (s Service) Reconcile(ctx context.Context, cmd api.Command) (api.Reconciliation, error) {
	name, key, args := request(cmd)
	if name == "" {
		return api.Reconciliation{}, kernel.Reject(kernel.CodeSchemaInvalid, "/command", "")
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Reconciliation{}, err
	}
	r, err := s.Ledger.Reconcile(ctx, name, key, hash)
	if err != nil {
		return api.Reconciliation{}, err
	}
	out := api.Reconciliation{State: r.State, ActionID: r.ActionID, ObjectID: r.ObjectID}
	if r.Result != nil {
		receipt := api.Receipt(*r.Result)
		out.Receipt = &receipt
	}
	return out, nil
}

// Audit returns TSK's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, api.CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

// qualify checks that actorID has an active grant of kind in the company, in TSK's transaction
// (authorization.Qualify; FOR SHARE after the authority guard): REFERENCE_INVALID at path
// otherwise. It returns the grant's team.
func qualify(ctx context.Context, exec bentopg.Executor, actorID string, kind kernel.ActorKind, path string) (string, error) {
	h, err := authorization.Authorizer{}.Qualify(ctx, exec, actorID)
	if err != nil {
		return "", err
	}
	if !h.Active || h.Actor.Kind != kind {
		return "", kernel.Reject(kernel.CodeReferenceInvalid, path, "No active "+string(kind)+" actor "+actorID+" in the company")
	}
	return h.Team, nil
}

// receipt completes res with the task's control as it is stored now.
func (s Service) receipt(ctx context.Context, res api.Receipt) (api.TaskReceipt, error) {
	out := api.TaskReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		t, err := postgres.LoadTask(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || t == nil {
			return err
		}
		out.ControlMode, out.ControlEpoch, out.BoundQuoteID = string(t.Mode), t.ControlEpoch, t.BoundQuote()
		return nil
	})
	return out, err
}
