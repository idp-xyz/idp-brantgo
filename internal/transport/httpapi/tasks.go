package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/execution"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/foundation/strictjson"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TSK is what the transport needs from the task context (WBS S4-01f): the BusinessTask commands and
// reads, ActionExecution's, the goal verifications, reconciliation and the audit trail.
type TSK interface {
	tskapi.Commands
	tskapi.Queries
	tskapi.ActionCommands
	tskapi.ActionQueries
	tskapi.VerificationQueries
	tskapi.Reconciler
	tskapi.Auditor
}

// tskCommands routes every command the Context Map lists for TSK: the 15 commands of SM-TSK-TASK's
// transitions and ActionExecution's two. TSK's commands name their objects (task_id, action_id), so
// no member is renamed to id; the members follow the reflective mapping (task.create {task_id,
// goal_code, control_mode, agent_id, owner_id, target {rfq_id, quote_id, customer_id}, source {kind,
// id}, deadline, budget {max_model_calls, max_retries, max_cost {currency, minor_units, amount}},
// execution_bundle {bundle_id, sha256}}, ...; tskapi's package comment lists them). The commands
// SM-TSK-TASK gives the system (task.wait, task.verify, goal.verify, task.finish_stop,
// task.bind_object) are routed too, for a system client; TSK's rules refuse them to people and AI
// agents (PERMISSION_DENIED), and every task command to an AI agent (AI-INV-004). The answer is the
// closed receipt: object_id is the task (the action for action.*), row_version its version, status
// its lifecycle state (the action's state); the control a task records is read with GET
// /api/brant/v1/tsk/tasks/<id>.
func tskCommands(c TSK) *commandSet {
	return &commandSet{bc: "tsk", contract: tskapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			tskapi.CommandCreate:           reflected("", "", receiptOf[tskapi.Receipt](c.CreateTask)),
			tskapi.CommandScope:            reflected("", "", receiptOf[tskapi.Receipt](c.ScopeTask)),
			tskapi.CommandWait:             reflected("", "", receiptOf[tskapi.Receipt](c.WaitTask)),
			tskapi.CommandVerify:           reflected("", "", receiptOf[tskapi.Receipt](c.VerifyTask)),
			tskapi.CommandVerifyGoal:       reflected("", "", receiptOf[tskapi.Receipt](c.VerifyGoal)),
			tskapi.CommandPause:            reflected("", "", receiptOf[tskapi.Receipt](c.PauseTask)),
			tskapi.CommandResume:           reflected("", "", receiptOf[tskapi.Receipt](c.ResumeTask)),
			tskapi.CommandTakeControl:      reflected("", "", receiptOf[tskapi.Receipt](c.TakeControl)),
			tskapi.CommandHandoff:          reflected("", "", receiptOf[tskapi.Receipt](c.Handoff)),
			tskapi.CommandCancel:           reflected("", "", receiptOf[tskapi.Receipt](c.CancelTask)),
			tskapi.CommandFinishStop:       reflected("", "", receiptOf[tskapi.Receipt](c.FinishStop)),
			tskapi.CommandCloseUnfulfilled: reflected("", "", receiptOf[tskapi.Receipt](c.CloseUnfulfilled)),
			tskapi.CommandFail:             reflected("", "", receiptOf[tskapi.Receipt](c.FailTask)),
			tskapi.CommandBindObject:       reflected("", "", receiptOf[tskapi.Receipt](c.BindObject)),
			tskapi.CommandRelease:          reflected("", "", receiptOf[tskapi.Receipt](c.ReleaseObject)),
			tskapi.CommandRecordAttempt:    reflected("", "", receiptOf[tskapi.Receipt](c.RecordAttempt)),
			tskapi.CommandReconcileAction:  reflected("", "", receiptOf[tskapi.Receipt](c.ReconcileAction)),
		}}
}

// tskRoutes are TSK's audited reads (tsk.read; NOT_FOUND for a task the caller does not see): a task
// with its control and history (task.get), its goal verifications (task.verifications), its actions
// with their logs (action.list), an action (action.get). The claim of a task step (task.claim) has no
// route: an HTTP client cannot carry it into a command — a person's step on a task goes through the
// task-step route, which takes the claim itself.
func (s *server) tskRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/brant/v1/tsk/tasks/{id}", s.authenticated(read(s, s.cfg.TSK.GetTask, okView[tskapi.TaskView])))
	mux.Handle("GET /api/brant/v1/tsk/tasks/{id}/verifications", s.authenticated(listRead(s, s.cfg.TSK.GoalVerifications, "task_id", "verifications")))
	mux.Handle("GET /api/brant/v1/tsk/tasks/{id}/actions", s.authenticated(listRead(s, s.cfg.TSK.TaskActions, "task_id", "actions")))
	mux.Handle("GET /api/brant/v1/tsk/actions/{id}", s.authenticated(read(s, s.cfg.TSK.GetAction, okView[tskapi.ActionView])))
}

// tskReads are TSK's read routes and the task-step route in the OpenAPI document.
var tskReads = []readRoute{
	{"/api/brant/v1/tsk/tasks/{id}", "tsk.task.get", "tsk",
		"A business task: goal and version, lifecycle, control mode, the control epoch recorded from TRD and the binding of its quote root, owner, agent, target, source, budget, execution bundle, what it waits for or why it is paused or stopping, how it ended, and its append-only history (task.get)", []string{"id"}, false},
	{"/api/brant/v1/tsk/tasks/{id}/verifications", "tsk.task.verification.list", "tsk",
		"The goal verifications of a task, oldest first: who judged, the outcome and its code, the unmet assertions, every assertion with its checks and evidence, the records judged and TRD's control of a bound root (task.verifications)", []string{"id"}, false},
	{"/api/brant/v1/tsk/tasks/{id}/actions", "tsk.action.list", "tsk",
		"The actions of a task, oldest first, each with its target command and request hash, its state, the target's receipt when committed and its append-only attempt log (action.list)", []string{"id"}, false},
	{"/api/brant/v1/tsk/actions/{id}", "tsk.action.get", "tsk",
		"An action of a task with its attempt log (action.get)", []string{"id"}, false},
}

// listRead answers GET of a list owned by the object in the path: {"ok": true, <owner>: id, <member>:
// [...]}, an empty list as [].
func listRead[V any](s *server, get func(context.Context, string) ([]V, error), owner, member string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := get(r.Context(), r.PathValue("id"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if list == nil {
			list = []V{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, owner: r.PathValue("id"), member: wire(list)})
	}
}

// stepTargets are the contexts a task step writes (Context Map R18, R66–R69).
var stepTargets = []string{tskapi.TargetEVD, tskapi.TargetINQ, tskapi.TargetSUP, tskapi.TargetCST, tskapi.TargetTRD}

// StepPath is the task-step route: a person's step on a task, a command of a context the task writes.
const StepPath = "/api/brant/v1/tsk/tasks/{id}/steps/{bc}/{command}"

// stepEnvelope is the request of a task step: the owning context's contract and the command, the
// business intent the step pursues and the command's arguments exactly as POST
// /api/brant/v1/<bc>/commands/<command> takes them. It has no idempotency key: the command's key is
// the ID of the intent's action, which TSK derives from the intent and keeps for every attempt
// (baseline 17.1). Presence is checked member by member.
type stepEnvelope struct {
	Contract         *string         `json:"contract"`
	Command          *string         `json:"command"`
	BusinessIntentID *string         `json:"business_intent_id"`
	Args             json.RawMessage `json:"args"`
}

// stepTarget is the command a step's action sends, as its owning context identifies it.
type stepTarget struct {
	Context     string `json:"context"`
	Command     string `json:"command"`
	RequestHash string `json:"request_hash"`
}

// stepResult is the owning context's answer to this run's send.
type stepResult struct {
	ActionID   string `json:"action_id"`
	ObjectID   string `json:"object_id"`
	RowVersion int64  `json:"row_version"`
	Status     string `json:"status"`
	Replayed   bool   `json:"replayed"`
}

// storedResult is TSK's record of the owning context's receipt of the committed action.
type storedResult struct {
	ActionID   string `json:"action_id"`
	ObjectID   string `json:"object_id"`
	RowVersion int64  `json:"row_version"`
	Status     string `json:"status"`
}

// stepReceipt answers a task step (UL AI-Receipt; execution.Receipt): stage ACCEPTED (the attempt is
// recorded, nothing is known to be committed), COMMITTED (the owning context answered with result) or
// VERIFIED (TSK recorded the action committed from the owning context's ledger: stored); action_id is
// TSK's action and the command's idempotency key; attempt the attempt this run opened or found
// settled; control_epoch the epoch of the task's claim the step carried (0: no bound root); replayed:
// the intent had committed before and nothing was sent (CT11).
type stepReceipt struct {
	OK               bool          `json:"ok"`
	Stage            string        `json:"stage"`
	TaskID           string        `json:"task_id"`
	BusinessIntentID string        `json:"business_intent_id"`
	ActionID         string        `json:"action_id"`
	Attempt          int           `json:"attempt"`
	Target           stepTarget    `json:"target"`
	ControlEpoch     int64         `json:"control_epoch"`
	Result           *stepResult   `json:"result"`
	Stored           *storedResult `json:"stored"`
	Replayed         bool          `json:"replayed"`
}

func stepReceiptOf(r execution.Receipt) stepReceipt {
	out := stepReceipt{OK: true, Stage: string(r.Stage), TaskID: r.TaskID, BusinessIntentID: r.BusinessIntentID, ActionID: r.ActionID,
		Attempt: r.Attempt, Target: stepTarget(r.Target), ControlEpoch: r.ControlEpoch, Replayed: r.Replayed}
	if res := r.Result; res != nil {
		v := stepResult(res.Receipt)
		out.Result = &v
	}
	if st := r.Stored; st != nil {
		out.Stored = &storedResult{ActionID: st.ActionID, ObjectID: st.ObjectID, RowVersion: st.RowVersion, Status: st.Status}
	}
	return out
}

// taskStep answers POST /api/brant/v1/tsk/tasks/<task_id>/steps/<bc>/<command>: a person's step on
// a task (owner decision D-2; TSK-INV-003, TSK-INV-004, TRD-INV-015). The command is one of the
// owning context's (evd, inq, sup, cst or trd), decoded exactly as its own command route decodes it,
// and runs through the AI layer's execution entry (internal/ai/execution, Layer.RunStep), never the
// plain command route: the task's claim from TSK (task.claim), the attempt recorded in TSK before
// the command is sent (action.record_attempt), the command sent with the action's key and, on the
// task's bound quote root, the task's control claim, the outcome recorded and reconciled with the
// owning context's ledger. The same business intent sent again is the same action and key: an
// attempt whose answer was lost is reconciled first, an intent that committed answers with its
// result and sends nothing.
//
// Only people take steps here (PERMISSION_DENIED otherwise): an AI agent's steps go through the AI
// gateway, which checks its delegation and uses its approval grants. TRD's object control commands
// (object_control.*) are refused (PERMISSION_DENIED at /command): only TSK changes a root's control
// (R17, D-4). A refusal before the step reaches the execution entry is audited in the owning context
// under the command's name, as the command route audits it; the execution entry's refusals are
// audited where they happen (TSK's claim and records, the owning context's command).
func (s *server) taskStep(w http.ResponseWriter, r *http.Request) {
	bc, name := r.PathValue("bc"), r.PathValue("command")
	set, ok := s.contexts[bc]
	if !ok || !slices.Contains(stepTargets, bc) {
		s.reject(w, r, kernel.Reject(kernel.CodeNotFound, "/", "no such route"))
		return
	}
	run, known := set.commands[name]
	audited := name
	if !known {
		audited = "invalid"
	}
	res, err := s.runStep(r, set, name, run)
	if err != nil {
		if rej, ok := kernel.AsRejection(err); ok && rej.Code != kernel.CodeUnauthenticated && !executed(err) {
			if auditErr := set.auditor.AuditRejected(r.Context(), audited, "", rej.Code); auditErr != nil {
				s.fail(w, r, errors.Join(rej, auditErr))
				return
			}
		}
		s.fail(w, r, unwrapExecuted(err))
		return
	}
	writeJSON(w, http.StatusOK, stepReceiptOf(res))
}

func (s *server) runStep(r *http.Request, set *commandSet, name string, run commandFunc) (execution.Receipt, error) {
	if strings.HasPrefix(name, execution.ControlCommandPrefix) {
		return execution.Receipt{}, kernel.Reject(kernel.CodePermissionDenied, "/command", "object control changes only through TSK")
	}
	raw, limit, values, err := s.readBody(r, set, name)
	if err != nil {
		return execution.Receipt{}, err
	}
	var env stepEnvelope
	if err = strictjson.Decode(bytes.NewReader(raw), &env, strictjson.Limits{MaxBytes: limit, MaxValues: values}); err != nil {
		return execution.Receipt{}, err
	}
	switch {
	case env.Contract == nil || *env.Contract != set.contract:
		return execution.Receipt{}, kernel.Reject(kernel.CodeUnsupportedContract, "/contract", "")
	case env.Command == nil || *env.Command != name || run.build == nil:
		return execution.Receipt{}, kernel.Reject(kernel.CodeUnsupportedContract, "/command", "")
	case env.BusinessIntentID == nil || !kernel.ValidIdentifier(*env.BusinessIntentID):
		return execution.Receipt{}, kernel.Reject(kernel.CodeSchemaInvalid, "/business_intent_id", "required identifier")
	case len(env.Args) == 0 || env.Args[0] != '{':
		return execution.Receipt{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args", "required object")
	}
	if p := emptyString(raw); p != "" {
		return execution.Receipt{}, kernel.Reject(kernel.CodeSchemaInvalid, p, "empty string")
	}
	intent := *env.BusinessIntentID
	cmd, err := run.build(tskapi.ActionIDOf(intent), env.Args, values)
	if err != nil {
		return execution.Receipt{}, contextError(err)
	}
	if p, err := identity.FromContext(r.Context()); err != nil || p.Actor.Kind != kernel.ActorHuman {
		return execution.Receipt{}, kernel.Reject(kernel.CodePermissionDenied, "/", "task steps here are people's; an AI agent's go through the AI gateway")
	}
	task := r.PathValue("id")
	if !kernel.ValidIdentifier(task) {
		return execution.Receipt{}, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	res, err := s.cfg.AI.RunStep(r.Context(), execution.Step{TaskID: task, BusinessIntentID: intent, Context: set.bc, Command: cmd,
		Send: func(ctx context.Context, cmd any) (capability.Result, error) {
			rc, err := run.run(ctx, cmd)
			return capability.Result{Receipt: capability.Receipt{ActionID: rc.ActionID, ObjectID: rc.ObjectID, RowVersion: rc.RowVersion,
				Status: rc.Status, Replayed: rc.Replayed}}, err
		}})
	if err != nil {
		return res, executedError{err}
	}
	return res, nil
}

// stepOperation is the task-step route in the OpenAPI document.
func stepOperation() map[string]any {
	params := []any{
		map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}, "description": "The task"},
		map[string]any{"name": "bc", "in": "path", "required": true, "schema": map[string]any{"type": "string", "enum": stepTargets},
			"description": "The context that owns the command"},
		map[string]any{"name": "command", "in": "path", "required": true, "schema": map[string]any{"type": "string"},
			"description": "A command of that context other than object_control.*"},
	}
	body := map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"contract", "command", "business_intent_id", "args"},
		"properties": map[string]any{
			"contract":           map[string]any{"type": "string", "description": "The owning context's contract"},
			"command":            map[string]any{"type": "string", "description": "The command of the path"},
			"business_intent_id": map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$"},
			"args": map[string]any{"type": "object",
				"description": "The arguments exactly as POST /api/brant/v1/{bc}/commands/{command} takes them; no idempotency key (the action's ID is the command's key)"},
		}}}}}
	responses := commandResponses()
	responses["200"] = map[string]any{"description": "ACCEPTED, COMMITTED or VERIFIED (UL AI-Receipt); replayed: the intent had committed and nothing was sent.",
		"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/StepReceipt"}}}}
	return map[string]any{"post": map[string]any{
		"operationId": "tsk.task.step",
		"tags":        []string{"tsk"},
		"summary":     "A person's step on a task through the execution entry (owner decision D-2)",
		"description": "The command runs through internal/ai/execution: the task's claim from TSK, the attempt recorded in TSK before the command is sent, the command sent with the key of the business intent's action and, on the task's bound quote root, the task's control claim, the outcome reconciled with the owning context's ledger. The same business_intent_id is the same action and key in every attempt. People only (an AI agent's steps go through the AI gateway); object_control.* is PERMISSION_DENIED. Request bodies follow the owning command's ingress limits.",
		"parameters":  params,
		"requestBody": body,
		"responses":   responses,
	}}
}
