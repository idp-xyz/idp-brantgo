package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/approvalgrant"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	"go.idp.xyz/idp-brant/internal/ai/gateway"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/ai/runtime"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// RunnerActor is the system actor the agent runner moves tasks with (task.pause, task.wait,
// task.resume, task.verify, goal.verify, task.bind_object): system:ai.runner, provisioned like the
// inbox actors (bootstrap ProvisionAgentRunner, brant-admin agent).
const RunnerActor = "system:ai.runner"

// RunnerPermissions are the permissions of RunnerActor: the system rows of SM-TSK-TASK the runner
// takes, the reads they need (goal.verify reads TRD's GoalReadiness, task.bind_object TRD's quote)
// and TRD's bind.
//
// task.finish_stop completes the stop barrier of a stopping task: TSK reconciles its open actions as
// the caller, so the actor reads every context a task step writes, and it moves the root's control
// at TRD (enter_manual on a take-over, handoff on a hand-back) as the system.
var RunnerPermissions = []string{
	tskapi.CommandPause, tskapi.CommandWait, tskapi.CommandResume, tskapi.CommandVerify, tskapi.CommandVerifyGoal,
	tskapi.CommandBindObject, tskapi.CommandFinishStop, tskapi.PermissionRead,
	"evd.read", "inq.read", "sup.read", "cst.read", "trd.read",
	trdapi.CommandBindControl, trdapi.CommandEnterManualControl, trdapi.CommandHandoffControl,
}

// ArrivalApprovalGrant is the kind of what a task waiting for an AI approval grant awaits (task.wait
// approval) and of the arrival its resume records: the approval grant a person issued for the
// agent's proposed call (AI-INV-002).
const ArrivalApprovalGrant = "ai_approval_grant"

// RunnerTasks is what the runner needs of TSK.
type RunnerTasks interface {
	Tasks
	tskapi.Commands
}

// Sources reads a task's EVD source as the agent (EVD re-checks the agent's access now, CT16): the
// source an evd_source task names, or the source a committed source.register of an evd_file task
// registered from the task's file.
type Sources interface {
	GetSource(ctx context.Context, sourceID string) (evdapi.SourceView, error)
}

// Runner is the agent runner (baseline 17.4: a single controlled execution flow; brant-worker
// -agent). For each scope and AI agent it serves, it works on the tasks the agent holds delegations
// for, one turn per task and pass:
//
//   - a running task in control mode ai whose agent it serves: the ExecutionBundle is checked
//     (EXECUTION_BUNDLE_INCOMPATIBLE: paused, CT14), every open action reconciled first (恢复前核查已有副作用),
//     then the model is asked — with the tools the task's delegations allow and the task's facts —
//     and each tool call runs through the gateway and the Executor; budget and retries are
//     reserved in brant_ai before every model and tool call (AI-INV-003: BUDGET_EXHAUSTED pauses
//     the task; an unavailable model or tool is retried within max_retries, then the task pauses,
//     BC-AT26). A call needing an approval grant without one makes the task wait for approval; a
//     quote root the task's draft created is bound to the task; when the model answers without a
//     tool call the task goes to verification (task.verify, goal.verify), never to completed on the
//     model's word (AI-INV-004);
//   - a task waiting for an approval grant of this runner's proposal resumes once a person issued
//     the grant;
//   - a verifying task is verified (goal.verify);
//   - a stopping task of its agent (a take-over, a hand-back, a cancellation) has its stop barrier
//     completed: open actions reconciled, then task.finish_stop;
//   - a running task without an active delegation is paused for its owner (AN04).
//
// A task that starts from an uploaded file (source kind evd_file, S4 G2) is told its file; once a
// committed source.register of the task registered that file's extract, the facts name the source
// and carry its bounded text as they do for an evd_source task. When that happens inside a turn the
// runner rebuilds the facts and the model goes on from them (a turn is a replan over what happened,
// decision E-6), so the model reads the inquiry in the call after it registered it.
//
// With an external runtime (Runtime, e.g. EER; WBS S4-05), the stop barrier of a task also waits
// until the runtime's operation of the task (runtime.OperationOf) has ended: the runner asks the
// runtime to cancel it and finishes the stop only once the runtime shows it ended or never had it
// (runtime.Barrier; IG-10).
type Runner struct {
	Executor    Executor
	Tasks       RunnerTasks
	Gateway     gateway.Gateway
	DB          *bentopg.DB // brant_ai: delegations and approval grants
	Evidence    executionevidence.Ledger
	Model       model.Port
	Bundle      executionevidence.Bundle
	Prompt      string    // the system prompt template; its SHA-256 is the bundle's
	Accepts     []string  // SHA-256s of earlier bundles this runtime still runs (baseline 17.3)
	Sources     Sources   // optional: the text of the task's source, or of its file's registered extract, goes into the facts
	Trace       *TraceDir // optional, off by default: the local diagnostic trace of every turn (trace.go)
	Scopes      []kernel.Scope
	Agents      []string     // AI actor IDs the runner acts as
	MaxSteps    int          // model turns per task and pass (default 8)
	Runtime     runtime.Port // optional external runtime (EER) holding operations of the tasks; nil: none (S4)
	Clock       application.Clock
	Logger      *slog.Logger
	ref         executionevidence.Ref
	defaults    executionevidence.Limits
	mu          sync.Mutex
	ended       map[string]bool
	initialized bool
}

// TurnResult reports what one turn did to a task.
type TurnResult struct {
	TaskID     string
	Lifecycle  string // after the turn
	ModelCalls int
	ToolCalls  int
	Receipts   []Receipt
	Refusals   []string // "<tool> <code>" for calls the gateway or a context refused
	Paused     string   // the pause cause when the turn paused the task
	Waiting    string   // the wait when the turn made the task wait
	Verified   bool     // goal.verify ran
	Stopped    bool     // task.finish_stop completed the stop barrier
	Skipped    string   // why nothing was done
}

// Init checks the configuration, computes the bundle's reference and records the bundle in every
// scope (as the first agent). Run and Pass call it.
func (r *Runner) Init(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.initialized {
		return nil
	}
	switch {
	case r.Tasks == nil || r.Model == nil || r.DB == nil || r.Clock == nil || len(r.Scopes) == 0 || len(r.Agents) == 0:
		return errors.New("execution: the runner needs TSK, a model, brant_ai, a clock, scopes and agents")
	case executionevidence.SHA256Hex([]byte(r.Prompt)) != r.Bundle.PromptSHA256:
		return errors.New("execution: the prompt is not the bundle's prompt template")
	}
	ref, err := r.Bundle.Ref()
	if err != nil {
		return err
	}
	if r.defaults, err = r.Bundle.DefaultBudget.Limits("runner"); err != nil {
		return err
	}
	for _, scope := range r.Scopes {
		actx, err := withPrincipal(ctx, kernel.ActorAI, r.Agents[0], scope)
		if err != nil {
			return err
		}
		if _, err = r.Evidence.PutBundle(actx, r.Bundle); err != nil {
			return fmt.Errorf("execution: recording bundle %s in %s: %w", ref.BundleID, scope, err)
		}
	}
	if r.MaxSteps <= 0 {
		r.MaxSteps = 8
	}
	if r.Logger == nil {
		r.Logger = slog.New(slog.DiscardHandler)
	}
	r.ref, r.ended, r.initialized = ref, map[string]bool{}, true
	return nil
}

// Ref returns the reference of the runner's bundle (after Init): what an AI task it runs names as
// its execution_bundle.
func (r *Runner) Ref() executionevidence.Ref { return r.ref }

// Run passes over the runner's tasks every poll until ctx ends.
func (r *Runner) Run(ctx context.Context, poll time.Duration) error {
	if err := r.Init(ctx); err != nil {
		return err
	}
	for {
		if _, err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			r.Logger.Error("agent pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Pass takes one turn on every task of every scope and agent; a failing task does not stop the
// others (its error is logged and returned joined).
func (r *Runner) Pass(ctx context.Context) ([]TurnResult, error) {
	if err := r.Init(ctx); err != nil {
		return nil, err
	}
	var out []TurnResult
	var errs []error
	for _, scope := range r.Scopes {
		for _, agent := range r.Agents {
			tasks, err := r.tasksOf(ctx, scope, agent)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, task := range tasks {
				if r.isEnded(scope, task) {
					continue
				}
				res, err := r.Turn(ctx, scope, agent, task)
				if err != nil {
					r.Logger.Warn("agent turn failed", "task", task, "error", errorCode(err))
					errs = append(errs, fmt.Errorf("task %s: %w", task, err))
				}
				out = append(out, res)
			}
		}
	}
	return out, errors.Join(errs...)
}

func (r *Runner) isEnded(scope kernel.Scope, task string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ended[string(scope.Tenant)+"/"+string(scope.Company)+"/"+task]
}

func (r *Runner) setEnded(scope kernel.Scope, task string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended[string(scope.Tenant)+"/"+string(scope.Company)+"/"+task] = true
}

// tasksOf lists the tasks agent holds delegations for in scope (active, or ended within a
// delegation's lifetime), in order.
func (r *Runner) tasksOf(ctx context.Context, scope kernel.Scope, agent string) ([]string, error) {
	var dels []policy.Delegation
	err := policy.WithinScope(ctx, r.DB, scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		dels, err = policy.OfAgent(ctx, exec, scope, agent, r.Clock.Now().Add(-policy.MaxLifetime))
		return err
	})
	var out []string
	for _, d := range dels {
		if !slices.Contains(out, d.TaskID) {
			out = append(out, d.TaskID)
		}
	}
	return out, err
}

// delegations returns the delegations of agent for task in scope that allow something now.
func (r *Runner) delegations(ctx context.Context, scope kernel.Scope, agent, task string) ([]policy.Delegation, error) {
	var out []policy.Delegation
	now := r.Clock.Now()
	err := policy.WithinScope(ctx, r.DB, scope, func(ctx context.Context, exec bentopg.Executor) error {
		dels, err := policy.OfAgent(ctx, exec, scope, agent, now)
		for _, d := range dels {
			if d.TaskID == task && d.Status == policy.StatusActive && !d.Expired(now) {
				out = append(out, d)
			}
		}
		return err
	})
	return out, err
}

// grantFor returns an issued, unexpired approval grant of d for exactly call, or nil.
func (r *Runner) grantFor(ctx context.Context, scope kernel.Scope, d policy.Delegation, call gateway.Call) (*approvalgrant.Grant, error) {
	var out *approvalgrant.Grant
	now := r.Clock.Now()
	err := policy.WithinScope(ctx, r.DB, scope, func(ctx context.Context, exec bentopg.Executor) error {
		grants, err := approvalgrant.OfDelegation(ctx, exec, scope, d.DelegationID)
		for _, g := range grants {
			if g.Status == approvalgrant.StatusIssued && now.Before(g.ExpiresAt) && g.Command == call.Capability.Command &&
				g.RequestHash == call.RequestHash && g.ObjectID == call.Object.ID && g.ObjectVersion == call.Object.Version {
				g := g
				out = &g
				return err
			}
		}
		return err
	})
	return out, err
}

// issuedGrants returns the issued, unexpired approval grants of the delegations.
func (r *Runner) issuedGrants(ctx context.Context, scope kernel.Scope, dels []policy.Delegation) ([]approvalgrant.Grant, error) {
	var out []approvalgrant.Grant
	now := r.Clock.Now()
	err := policy.WithinScope(ctx, r.DB, scope, func(ctx context.Context, exec bentopg.Executor) error {
		for _, d := range dels {
			grants, err := approvalgrant.OfDelegation(ctx, exec, scope, d.DelegationID)
			if err != nil {
				return err
			}
			for _, g := range grants {
				if g.Status == approvalgrant.StatusIssued && now.Before(g.ExpiresAt) {
					out = append(out, g)
				}
			}
		}
		return nil
	})
	return out, err
}

// turn is the state of one turn on one task.
type turn struct {
	r      *Runner
	scope  kernel.Scope
	agent  context.Context // the AI agent's Principal
	system context.Context // RunnerActor's Principal
	view   tskapi.TaskView
	limits executionevidence.Limits
	dels   []policy.Delegation
	res    TurnResult
	// source is the source registered from the file of an evd_file task, once the facts found it;
	// others are the task's committed source.register actions found to be something else.
	source string
	others map[string]bool
	trace  *turnTrace
}

// Turn takes one turn on task as agent in scope.
func (r *Runner) Turn(ctx context.Context, scope kernel.Scope, agent, task string) (TurnResult, error) {
	if err := r.Init(ctx); err != nil {
		return TurnResult{}, err
	}
	t := &turn{r: r, scope: scope, res: TurnResult{TaskID: task}, others: map[string]bool{}}
	var err error
	if t.agent, err = withPrincipal(ctx, kernel.ActorAI, agent, scope); err != nil {
		return t.res, err
	}
	if t.system, err = withPrincipal(ctx, kernel.ActorSystem, RunnerActor, scope); err != nil {
		return t.res, err
	}
	if err = t.refresh(); err != nil {
		if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
			t.res.Skipped = "the agent does not see the task"
			return t.res, nil
		}
		return t.res, err
	}
	err = t.run()
	t.res.Lifecycle = t.view.Lifecycle
	switch t.view.Lifecycle {
	case tskapi.StateCompleted, tskapi.StateClosedUnfulfilled, tskapi.StateCancelled, tskapi.StateFailed:
		r.setEnded(scope, task)
	}
	if traceErr := t.trace.end(r.Clock.Now(), t.res, err); traceErr != nil {
		// The trace is a diagnostic: losing it does not fail the turn, and it is said.
		r.Logger.Warn("agent trace not written", "task", task, "error", traceErr)
	}
	return t.res, err
}

func (t *turn) refresh() error {
	v, err := t.r.Tasks.GetTask(t.agent, t.res.TaskID)
	if err == nil {
		t.view = v
	}
	return err
}

func (t *turn) run() error {
	v := t.view
	if v.Lifecycle == tskapi.StateStopping && v.AgentID == actorID(t.agent) {
		return t.finishStop()
	}
	if v.ControlMode != tskapi.ModeAI || v.AgentID != actorID(t.agent) {
		t.res.Skipped = "not an AI task of this agent"
		return nil
	}
	switch v.Lifecycle {
	case tskapi.StateVerifying:
		return t.verifyGoal()
	case tskapi.StateWaitingApproval:
		if v.Wait == nil || v.Wait.Awaiting == nil || v.Wait.Awaiting.Kind != ArrivalApprovalGrant {
			t.res.Skipped = "waiting for an approval the runner did not ask for"
			return nil
		}
	case tskapi.StateRunning:
	default:
		t.res.Skipped = "the task is " + v.Lifecycle
		return nil
	}
	if err := t.r.CheckBundle(v); err != nil {
		return t.pause(tskapi.PauseConflict, reasonOf(err), "A runner whose ExecutionBundle is compatible resumes the task, or the owner takes control (baseline 17.3).")
	}
	var err error
	if t.dels, err = t.r.delegations(t.agent, t.scope, actorID(t.agent), v.TaskID); err != nil {
		return err
	}
	// The owner's delegations first: TSK names the task's owner as the initiator at TRD (D-5), so a
	// tool the owner's delegation lists runs under it; another person's covers the rest (the costing
	// drafts under the costing person's).
	slices.SortStableFunc(t.dels, func(a, b policy.Delegation) int {
		return boolRank(a.InitiatorID != v.OwnerID) - boolRank(b.InitiatorID != v.OwnerID)
	})
	if len(t.dels) == 0 {
		return t.pause(tskapi.PauseConflict, kernel.CodePermissionDenied+": the agent holds no active delegation for the task",
			"The owner delegates the task to its agent again and resumes it, or takes control (AN04).")
	}
	if v.Lifecycle == tskapi.StateWaitingApproval {
		resumed, err := t.resumeOnGrant()
		if err != nil || !resumed {
			return err
		}
	}
	if t.limits, err = executionevidence.LimitsOf(v.Budget, t.r.defaults); err != nil {
		return err
	}
	// 恢复前核查已有副作用: what an earlier turn left in flight is settled before the model is asked.
	actions, err := t.r.Executor.Recover(t.agent, v.TaskID)
	if err != nil {
		return err
	}
	if err = t.bindCreatedRoot(actions); err != nil {
		return err
	}
	return t.converse(actions)
}

// CheckBundle checks that the runner runs a task's ExecutionBundle: the same bundle, or one whose
// SHA-256 the runner lists as compatible; a task without a bundle is not run (Q01 V0.1
// tests/test_execution.py::test_incompatible_persisted_bundle_stops_actions; CT14).
func (r *Runner) CheckBundle(v tskapi.TaskView) error {
	b := v.ExecutionBundle
	switch {
	case b == nil:
		return codes.Reject(codes.ExecutionBundleIncompatible, "/execution_bundle")
	case b.BundleID == r.ref.BundleID && b.SHA256 == r.ref.SHA256, slices.Contains(r.Accepts, b.SHA256):
		return nil
	}
	return codes.Reject(codes.ExecutionBundleIncompatible, "/execution_bundle/sha256")
}

// resumeOnGrant resumes a task waiting for this runner's proposed approval once a person issued a
// grant for it.
func (t *turn) resumeOnGrant() (bool, error) {
	grants, err := t.r.issuedGrants(t.agent, t.scope, t.dels)
	if err != nil {
		return false, err
	}
	awaited := t.view.Wait.Awaiting
	for _, g := range grants {
		if ProposalID(t.view.TaskID, g.Command, g.RequestHash) != awaited.ID {
			continue
		}
		_, err = t.r.Tasks.ResumeTask(t.system, tskapi.ResumeTask{IdempotencyKey: commandKey(t.view, "resume"), TaskID: t.view.TaskID,
			ExpectedVersion: t.view.TaskVersion, Arrival: &tskapi.ObjectRef{Kind: ArrivalApprovalGrant, ID: g.GrantID, Version: g.ObjectVersion}})
		if err != nil {
			return false, err
		}
		return true, t.refresh()
	}
	t.res.Skipped = "still waiting for the approval grant"
	return false, nil
}

// bindCreatedRoot binds the quote root the task's own committed quote.save_draft created, when the
// task has no binding yet (task.bind_object; TSK records the epoch TRD returns, MD-31).
func (t *turn) bindCreatedRoot(actions []tskapi.ActionView) error {
	if t.view.Binding != nil {
		return nil
	}
	for _, a := range actions {
		if a.Target.Context != tskapi.TargetTRD || a.Target.Command != trdapi.CommandSaveDraft || a.State != tskapi.ActionCommitted ||
			a.Result == nil || (t.view.Target.QuoteID != "" && a.Result.ObjectID != t.view.Target.QuoteID) {
			continue
		}
		_, err := t.r.Tasks.BindObject(t.system, tskapi.BindObject{IdempotencyKey: commandKey(t.view, "bind"), TaskID: t.view.TaskID,
			ExpectedVersion: t.view.TaskVersion, QuoteID: a.Result.ObjectID})
		if err != nil {
			return err
		}
		return t.refresh()
	}
	return nil
}

// converse asks the model and runs its tool calls until it answers without one, the task leaves
// running, or the turn's steps are used.
func (t *turn) converse(actions []tskapi.ActionView) error {
	tools, toolDel, err := t.tools()
	if err != nil {
		return err
	}
	facts, err := t.facts(actions)
	if err != nil {
		return err
	}
	t.trace = t.r.Trace.begin(t.scope, actorID(t.agent), t.view, traceBundle{BundleID: t.r.ref.BundleID, SHA256: t.r.ref.SHA256,
		Profile: t.r.Bundle.Model.ProfileID, Model: t.r.Bundle.Model.Model, PromptSHA256: t.r.Bundle.PromptSHA256}, t.r.Prompt, tools, t.r.Clock.Now())
	t.trace.facts(t.r.Clock.Now(), facts, false)
	messages := []model.Message{{Role: model.RoleSystem, Content: t.r.Prompt}, {Role: model.RoleUser, Content: facts}}
	for step := 0; step < t.r.MaxSteps; step++ {
		resp, stop, err := t.ask(messages, tools)
		if err != nil || stop {
			return err
		}
		if len(resp.Message.ToolCalls) == 0 {
			return t.finish()
		}
		messages = append(messages, resp.Message)
		for _, call := range resp.Message.ToolCalls {
			answer, stop, err := t.toolCall(call, toolDel)
			if err != nil || stop {
				return err
			}
			messages = append(messages, model.Message{Role: model.RoleTool, ToolCallID: call.ID, Content: answer})
		}
		if err = t.refresh(); err != nil {
			return err
		}
		if t.view.Lifecycle != tskapi.StateRunning {
			return nil
		}
		if actions, err = t.r.Executor.Tasks.TaskActions(t.agent, t.view.TaskID); err != nil {
			return err
		}
		if err = t.bindCreatedRoot(actions); err != nil {
			return err
		}
		if t.awaitsExtract(actions) {
			// The task's file may have been registered in this step: the facts are rebuilt, and when
			// they now carry the source and its text the model goes on from them, not from a
			// conversation that never showed it the inquiry (decision E-6: a replan over what happened).
			if facts, err = t.facts(actions); err != nil {
				return err
			}
			if t.source != "" {
				messages = []model.Message{{Role: model.RoleSystem, Content: t.r.Prompt}, {Role: model.RoleUser, Content: facts}}
				t.trace.facts(t.r.Clock.Now(), facts, true)
			}
		}
	}
	return nil
}

// awaitsExtract reports whether the task serves an uploaded file whose registered extract the
// facts have not found yet, while a committed source.register of the task has not been looked at.
func (t *turn) awaitsExtract(actions []tskapi.ActionView) bool {
	s := t.view.Source
	if s == nil || s.Kind != tskapi.SourceFile || t.source != "" || t.r.Sources == nil {
		return false
	}
	for _, a := range actions {
		if registersSource(a) && !t.others[a.ActionID] {
			return true
		}
	}
	return false
}

// registersSource reports whether a is a committed source.register with the target's receipt.
func registersSource(a tskapi.ActionView) bool {
	return a.Target.Context == tskapi.TargetEVD && a.Target.Command == evdapi.CommandRegisterSource && a.State == tskapi.ActionCommitted &&
		a.Result != nil
}

// readSource reads a source as the agent; a source the agent may not read (or that is gone) is
// nil, not an error.
func (t *turn) readSource(id string) (*evdapi.SourceView, error) {
	src, err := t.r.Sources.GetSource(t.agent, id)
	if err == nil {
		return &src, nil
	}
	if _, ok := kernel.AsRejection(err); ok {
		return nil, nil
	}
	return nil, err
}

// registeredExtract returns the source of the task's file: the object of the task's oldest
// committed source.register whose source EVD shows, to the agent now, as an extract of that file
// (EVD's record, not the model's word: the action names the source, EVD names the file it was
// read from). Nil until there is one.
func (t *turn) registeredExtract(actions []tskapi.ActionView) (*evdapi.SourceView, error) {
	for _, a := range actions {
		if !registersSource(a) || t.others[a.ActionID] {
			continue
		}
		src, err := t.readSource(a.Result.ObjectID)
		if err != nil {
			return nil, err
		}
		if src != nil && src.Extract != nil && src.Extract.FileID == t.view.Source.ID {
			t.source = src.SourceID
			return src, nil
		}
		t.others[a.ActionID] = true
	}
	return nil, nil
}

// tools returns the tools the task's delegations allow, and for each the delegation that allows it
// (lead decision 2026-11-13: a task may carry several delegations; the step runs under the one
// that covers it).
func (t *turn) tools() ([]model.Tool, map[string]policy.Delegation, error) {
	var out []model.Tool
	by := map[string]policy.Delegation{}
	for _, d := range t.dels {
		tools, err := t.r.Gateway.Tools(t.agent, d)
		if err != nil {
			return nil, nil, err
		}
		for _, tool := range tools {
			if _, seen := by[tool.Name]; !seen {
				by[tool.Name] = d
				out = append(out, tool)
			}
		}
	}
	slices.SortFunc(out, func(a, b model.Tool) int { return strings.Compare(a.Name, b.Name) })
	return out, by, nil
}

// ask makes one model call within the budget: reserved first (BUDGET_EXHAUSTED pauses the task),
// settled with what the provider returned; an unavailable model is retried within max_retries,
// then the task pauses (BC-AT26).
func (t *turn) ask(messages []model.Message, tools []model.Tool) (model.Response, bool, error) {
	req := model.Request{Profile: t.r.Bundle.Model.ProfileID, Messages: messages, Tools: tools, MaxOutputTokens: t.r.Bundle.Model.MaxOutputTokens}
	profile := t.r.Bundle.Model
	for retry := false; ; retry = true {
		call, err := t.r.Evidence.Reserve(t.agent, t.limits, executionevidence.Reservation{TaskID: t.view.TaskID, TaskVersion: t.view.TaskVersion,
			Kind: executionevidence.KindModel, Retry: retry, Bundle: t.r.ref, Profile: &profile, PromptSHA: t.r.Bundle.PromptSHA256})
		if err != nil {
			return model.Response{}, true, t.exhausted(err, retry, tskapi.PauseModelUnavailable)
		}
		t.res.ModelCalls++
		start := t.r.Clock.Now()
		resp, callErr := t.r.Model.Complete(t.agent, req)
		latency := t.r.Clock.Now().Sub(start)
		if resp.Latency > 0 {
			latency = resp.Latency
		}
		switch {
		case callErr == nil:
			t.trace.reply(t.r.Clock.Now(), resp, latency)
		case errors.Is(callErr, context.DeadlineExceeded):
			t.trace.modelError(t.r.Clock.Now(), "timeout", retry)
		case errors.Is(callErr, model.ErrUnavailable):
			t.trace.modelError(t.r.Clock.Now(), "unavailable", retry)
		default:
			t.trace.modelError(t.r.Clock.Now(), "refused", retry)
		}
		out, err := t.settleModel(call, req, resp, callErr, latency)
		if err != nil {
			return model.Response{}, true, err
		}
		switch {
		case callErr == nil:
			return resp, false, nil
		case errors.Is(callErr, model.ErrUnavailable):
			t.r.Logger.Info("model unavailable", "task", t.view.TaskID, "call", out.CallID)
			continue
		case t.agent.Err() != nil:
			return model.Response{}, true, t.agent.Err()
		default:
			// A refusal of the request or a broken answer: not retried; the task pauses for people.
			return model.Response{}, true, t.pause(tskapi.PauseModelUnavailable, codes.ToolUnavailable+": the model refused or broke the request",
				"The model service answers the task's requests again and the owner resumes the task, or takes control (BC-AT26).")
		}
	}
}

func (t *turn) settleModel(call executionevidence.Call, req model.Request, resp model.Response, callErr error, latency time.Duration) (executionevidence.Call, error) {
	zero, _ := kernel.NewMoney(t.limits.Currency, 0)
	o := executionevidence.Outcome{Latency: latency, InputSHA256: hashJSON(req)}
	switch {
	case callErr == nil:
		in, out := resp.Usage.InputTokens, resp.Usage.OutputTokens
		cost, err := t.r.Bundle.Model.Cost(t.limits.Currency, in, out)
		if err != nil {
			return call, err
		}
		o.State, o.ModelReturned, o.OutputSHA256, o.FinishReason, o.Cost = executionevidence.StateSucceeded, bounded(resp.Model, 200),
			hashJSON(resp.Message), bounded(resp.FinishReason, 40), &cost
		o.InputTokens, o.OutputTokens = &in, &out
	case errors.Is(callErr, context.DeadlineExceeded):
		// The provider may have worked and charged: the reservation stays counted until reconciled.
		o.State, o.Code = executionevidence.StateUnknown, codes.ToolUnavailable
	case errors.Is(callErr, model.ErrUnavailable):
		o.State, o.Code, o.Cost = executionevidence.StateUnavailable, codes.ToolUnavailable, &zero
	default:
		o.State, o.Code, o.Cost = executionevidence.StateFailed, codes.ToolUnavailable, &zero
	}
	return t.r.Evidence.Settle(t.agent, call.CallID, o)
}

// toolCall runs one tool call of the model: reserved, decoded and admitted by the gateway under the
// delegation that allows it, run through the Executor; it returns the answer the model sees.
func (t *turn) toolCall(call model.ToolCall, toolDel map[string]policy.Delegation) (string, bool, error) {
	name := auditName(call.Name)
	for retry := false; ; retry = true {
		reserved, err := t.r.Evidence.Reserve(t.agent, t.limits, executionevidence.Reservation{TaskID: t.view.TaskID,
			TaskVersion: t.view.TaskVersion, Kind: executionevidence.KindTool, Retry: retry, Bundle: t.r.ref, ToolCommand: name})
		if err != nil {
			return "", true, t.exhausted(err, retry, tskapi.PauseToolUnavailable)
		}
		t.res.ToolCalls++
		answer, rcpt, runErr := t.runTool(call, toolDel)
		zero, _ := kernel.NewMoney(t.limits.Currency, 0)
		o := executionevidence.Outcome{State: executionevidence.StateSucceeded, Cost: &zero, ActionID: rcpt.ActionID}
		switch {
		case runErr == nil:
		case errors.Is(runErr, errWaiting):
			// A wait is not a refusal (D1 finding 5): the call did not run, and it is not GRANT_INVALID.
			o.State, o.Code = executionevidence.StateWaiting, executionevidence.CodeWaitingApproval
		case errors.Is(runErr, model.ErrUnavailable):
			o.State, o.Code = executionevidence.StateUnavailable, codes.ToolUnavailable
		default:
			o.State, o.Code = executionevidence.StateFailed, errorCode(runErr)
			if _, ok := kernel.AsRejection(runErr); ok {
				// A refusal: the model is told where and why, and the ledger keeps it for the facts of
				// the next turn (S4 G3; refusal.go).
				answer, o.RefusalPath, o.RefusalReason = t.refused(call, runErr)
			}
		}
		if t.isRead(call.Name) {
			// A read is recorded as hashes, never as the records it returned (E-6).
			o.InputSHA256 = executionevidence.SHA256Hex(call.Arguments)
			o.OutputSHA256 = executionevidence.SHA256Hex([]byte(answer))
		}
		if o.ActionID != "" && !kernel.ValidIdentifier(o.ActionID) {
			o.ActionID = ""
		}
		if _, err = t.r.Evidence.Settle(t.agent, reserved.CallID, o); err != nil {
			return "", true, err
		}
		switch {
		case runErr == nil:
			t.trace.toolResult(t.r.Clock.Now(), call, answer, "")
			return answer, false, nil
		case errors.Is(runErr, errWaiting):
			t.trace.toolEnded(t.r.Clock.Now(), "tool_wait", call, "")
			return "", true, nil
		case errors.Is(runErr, model.ErrUnavailable):
			t.trace.toolEnded(t.r.Clock.Now(), "tool_error", call, "unavailable")
			continue
		}
		if _, ok := kernel.AsRejection(runErr); !ok {
			t.trace.toolEnded(t.r.Clock.Now(), "tool_error", call, "internal")
			return "", true, runErr
		}
		t.res.Refusals = append(t.res.Refusals, name+" "+errorCode(runErr))
		t.trace.toolResult(t.r.Clock.Now(), call, answer, errorCode(runErr))
		return answer, false, nil
	}
}

// errWaiting ends a turn whose task now waits for a person.
var errWaiting = errors.New("execution: the task waits")

// isRead reports whether name is a registered read. An unregistered name is not.
func (t *turn) isRead(name string) bool {
	if t.r.Gateway.Registry == nil {
		return false
	}
	c, err := t.r.Gateway.Registry.Lookup(t.scope.Tenant, name, "/command")
	return err == nil && c.SideEffect == capability.Read
}

// readCall admits and runs a read. The answer is the bounded projection; the ledger keeps its
// hashes. There is no TSK action and no approval grant.
func (t *turn) readCall(b gateway.Binding, c gateway.Call) (string, Receipt, error) {
	admitted, err := t.r.Gateway.Admit(t.agent, b, c, "rd.1")
	if err != nil {
		return "", Receipt{}, err
	}
	res, err := admitted.Run(t.agent)
	if err != nil {
		return "", Receipt{}, err
	}
	body := map[string]any{"ok": true, "read": c.Capability.Command, "result": res.Detail}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", Receipt{}, err
	}
	if len(raw) > capability.MaxReadBytes {
		body["result"] = map[string]any{"object_id": res.ObjectID, "truncated": true,
			"omitted": "the result exceeded 16384 bytes and was left out"}
		raw, err = json.Marshal(body)
		if err != nil {
			return "", Receipt{}, err
		}
	}
	return string(raw), Receipt{}, nil
}

func (t *turn) runTool(call model.ToolCall, toolDel map[string]policy.Delegation) (string, Receipt, error) {
	d, ok := toolDel[call.Name]
	if !ok {
		d = t.dels[0] // the gateway refuses the call and audits it under this delegation's agent
	}
	b := gateway.Binding{TaskID: t.view.TaskID, DelegationID: d.DelegationID}
	c, err := t.r.Gateway.Decode(t.agent, b, call)
	if err != nil {
		return "", Receipt{}, err
	}
	if c.Capability.SideEffect == capability.Read {
		// A read changes nothing, so it is not an ActionExecution (TSK-INV-003, D-2). The gateway
		// still admits it: the delegation and both grants, and never an approval grant.
		return t.readCall(b, c)
	}
	if c.NeedsGrant() {
		// Settle the intent with its target before asking for a grant (D1 finding 2, AN05). A
		// committed intent replays from TSK's record of the target's ledger; nothing is sent and no
		// new grant is used. An intent the target has not committed still needs its grant.
		intent := IntentOf(t.view.TaskID, c.Capability.Command, c.Object.ID, c.RequestHash)
		if rcpt, ok, err := t.r.Executor.Settled(t.agent, t.view.TaskID, intent); err != nil {
			return "", Receipt{}, err
		} else if ok {
			t.res.Receipts = append(t.res.Receipts, rcpt)
			return answerOf(rcpt), rcpt, nil
		}
		g, err := t.r.grantFor(t.agent, t.scope, d, c)
		if err != nil {
			return "", Receipt{}, err
		}
		if g == nil {
			return "", Receipt{}, t.waitForGrant(c)
		}
		b.GrantID = g.GrantID
	}
	var admitted gateway.Admitted
	step := Step{TaskID: t.view.TaskID, BusinessIntentID: IntentOf(t.view.TaskID, c.Capability.Command, c.Object.ID, c.RequestHash),
		Context: c.Capability.Context, Command: c.Command, RequestHash: c.RequestHash, DelegationID: b.DelegationID, GrantID: b.GrantID,
		Admit: func(ctx context.Context, key string) error {
			var err error
			admitted, err = t.r.Gateway.Admit(ctx, b, c, key)
			return err
		},
		Send: func(ctx context.Context, _ any) (capability.Result, error) { return admitted.Run(ctx) }}
	rcpt, err := t.r.Executor.Run(t.agent, step)
	if err != nil {
		return "", rcpt, err
	}
	t.res.Receipts = append(t.res.Receipts, rcpt)
	return answerOf(rcpt), rcpt, nil
}

// waitForGrant makes the task wait for a person's approval grant for exactly call (task.wait
// approval; AI-INV-002, D-7): the wait names the proposal and its items the command, request hash
// and object the grant must bind.
func (t *turn) waitForGrant(c gateway.Call) error {
	proposal := ProposalID(t.view.TaskID, c.Capability.Command, c.RequestHash)
	items := []string{"command " + c.Capability.Command, "request_hash " + c.RequestHash,
		fmt.Sprintf("object %s %s version %d", c.Object.Type, c.Object.ID, c.Object.Version)}
	_, err := t.r.Tasks.WaitTask(t.system, tskapi.WaitTask{IdempotencyKey: commandKey(t.view, "wait."+proposal), TaskID: t.view.TaskID,
		ExpectedVersion: t.view.TaskVersion, Wait: tskapi.WaitApproval, ResponsibleID: t.view.OwnerID, Items: items,
		Reason: bounded("The agent proposes "+c.Capability.Command+" on "+c.Object.ID+"; it needs a person's approval grant (ai.approval_grant.issue) "+
			"for exactly this request before it runs.", tskapi.MaxReason),
		Awaiting: &tskapi.ObjectRef{Kind: ArrivalApprovalGrant, ID: proposal, Version: c.Object.Version}})
	if err != nil {
		return err
	}
	t.res.Waiting = tskapi.WaitApproval
	if err = t.refresh(); err != nil {
		return err
	}
	return errWaiting
}

// finish moves a running task whose model answered without a tool call to verification and has its
// goal verified against the authoritative records (TSK-INV-001; the model's word completes
// nothing).
func (t *turn) finish() error {
	if t.view.Lifecycle != tskapi.StateRunning {
		return nil
	}
	_, err := t.r.Tasks.VerifyTask(t.system, tskapi.VerifyTask{IdempotencyKey: commandKey(t.view, "verify"), TaskID: t.view.TaskID,
		ExpectedVersion: t.view.TaskVersion})
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == tskapi.CodeExternalResultUnknown {
		t.res.Skipped = "an action is still open; the next turn reconciles it"
		return nil
	}
	if err != nil {
		return err
	}
	if err = t.refresh(); err != nil {
		return err
	}
	return t.verifyGoal()
}

// RuntimeStopReason is the reason the runner gives an external runtime when it cancels the
// operation of a stopping task (no task ID, scope or person: the operation ID correlates).
const RuntimeStopReason = "IDP Brant stop barrier: the task is stopping (take-over, hand-back or cancellation); " +
	"the runtime takes no further step of this operation"

// finishStop completes the stop barrier of a stopping task of the runner's agent — a take-over, a
// hand-back or a cancellation (SM-TSK-TASK task.finish_stop, system): the agent reconciles the task's
// open actions with their targets first (action.reconcile admits the task's agent in any state),
// then RunnerActor finishes the stop, which TSK refuses (EXTERNAL_RESULT_UNKNOWN) while an action is
// still open; the next pass tries again, or a person reconciles.
func (t *turn) finishStop() error {
	if t.r.Runtime != nil {
		// The runtime half of the barrier first: the runtime takes no further step of the task.
		st, err := runtime.Barrier(t.system, t.r.Runtime, runtime.OperationOf(t.scope, t.view.TaskID), RuntimeStopReason)
		if err != nil {
			return fmt.Errorf("the stop barrier cannot settle the runtime's operation: %w", err)
		}
		if !st.Settled {
			t.res.Skipped = "the stop barrier waits for the runtime's operation to end (" + string(st.Operation.State) + ")"
			return nil
		}
	}
	if _, err := t.r.Executor.Recover(t.agent, t.view.TaskID); err != nil {
		if _, ok := kernel.AsRejection(err); !ok {
			return err
		}
	}
	_, err := t.r.Tasks.FinishStop(t.system, tskapi.FinishStop{IdempotencyKey: commandKey(t.view, "finish_stop"), TaskID: t.view.TaskID,
		ExpectedVersion: t.view.TaskVersion})
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == tskapi.CodeExternalResultUnknown {
		t.res.Skipped = "the stop barrier waits for an open action"
		return nil
	}
	if err != nil {
		return err
	}
	t.res.Stopped = true
	return t.refresh()
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (t *turn) verifyGoal() error {
	_, err := t.r.Tasks.VerifyGoal(t.system, tskapi.VerifyGoal{IdempotencyKey: commandKey(t.view, "goal"), TaskID: t.view.TaskID,
		ExpectedVersion: t.view.TaskVersion})
	if err != nil {
		return err
	}
	t.res.Verified = true
	return t.refresh()
}

// exhausted pauses the task for a refused reservation: BUDGET_EXHAUSTED for calls or cost; an
// exhausted retry budget after an unavailable model or tool is TOOL_UNAVAILABLE (cause).
func (t *turn) exhausted(err error, retry bool, cause string) error {
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != codes.BudgetExhausted {
		return err
	}
	if retry && rej.Fields[0].Path == "/budget/max_retries" {
		return t.pause(cause, codes.ToolUnavailable+": still unavailable when the retries of the budget were used ("+reasonOf(err)+")",
			"The model service or tool answers again; the owner resumes the task or takes control (AI-INV-003, BC-AT26).")
	}
	return t.pause(tskapi.PauseBudgetExhausted, reasonOf(err),
		"The owner reviews the spend and takes control or closes the task; the budget of a task is fixed at task.create (AI-INV-003).")
}

// pause pauses the task as RunnerActor for cause, naming the code, the spend and the condition to
// resume (AI-INV-003: 暂停并记录原因、成本与恢复条件).
func (t *turn) pause(cause, why, condition string) error {
	reason := "paused by the agent runner: " + why
	if spend, ok, err := t.r.Evidence.Spend(t.agent, t.view.TaskID); err == nil && ok {
		reason += fmt.Sprintf("; spent %d model calls, %d tool calls, %d retries, %d tokens, cost %s settled and %s open",
			spend.ModelCalls, spend.ToolCalls, spend.Retries, spend.Tokens, spend.Settled, spend.Open)
		if spend.Limits.MaxCost != nil {
			reason += " of " + spend.Limits.MaxCost.String()
		}
		reason += fmt.Sprintf(" (limits %d model calls, %d retries, %s budget)", spend.Limits.MaxModelCalls, spend.Limits.MaxRetries, spend.Limits.Source)
	}
	_, err := t.r.Tasks.PauseTask(t.system, tskapi.PauseTask{IdempotencyKey: commandKey(t.view, "pause."+cause), TaskID: t.view.TaskID,
		ExpectedVersion: t.view.TaskVersion, Cause: cause, Reason: bounded(reason, tskapi.MaxReason), ResponsibleID: t.view.OwnerID,
		ResumeCondition: bounded(condition, tskapi.MaxReason)})
	if err != nil {
		return err
	}
	t.res.Paused = cause
	return t.refresh()
}

// facts is the user message of a turn: the task as TSK and the owning contexts record it now — not
// a stored conversation (decision E-6; baseline 17.3: a turn is a replan over what happened).
//
// The source: a task that names an EVD source (evd_source) gets that source's text, read as the
// agent and cut at 2000 characters (source_text; source_truncated and source_sha256 when cut). A
// task that names an uploaded file (evd_file, S4 G2) is told the file (source_file_id); it has no
// text until the task registered the file's extract — then registered_source_id names the source a
// committed source.register of the task created and source_text carries its text the same way.
//
// What a turn cannot see in the records it is told here (S4 G3):
//
//   - now, the platform's clock, so an instant a draft states (a quote's ExpiresAt) is one after it;
//   - arrivals, what the task was resumed with, oldest first, as TSK recorded each on the history
//     row of its task.resume (kind, id, version): the ready RFQ, the source of the customer's
//     reply, the verified acceptance. The last maxArrivalFacts, without the runner's own resumes
//     on an approval grant;
//   - refusals, the task's refused tool calls a new turn would otherwise repeat (refusalFacts):
//     tool, code, path, the member of the tool's arguments, what it needs and the values it takes,
//     the refusing context's static reason, and how often the refusal was met. They come from the
//     ledger of the task's calls (brant_ai.model_call), which has every refused tool call — the
//     gateway's and a read's too, which are no actions in TSK. A rejected action is therefore not
//     listed among the actions: its refusal says more.
func (t *turn) facts(actions []tskapi.ActionView) (string, error) {
	v := t.view
	type actionFact struct {
		Command    string `json:"command"`
		State      string `json:"state"`
		ObjectID   string `json:"object_id,omitempty"`
		RowVersion int64  `json:"row_version,omitempty"`
		Status     string `json:"status,omitempty"`
	}
	type grantFact struct {
		Command  string `json:"command"`
		ObjectID string `json:"object_id"`
		Version  int64  `json:"object_version"`
	}
	type arrivalFact struct {
		Kind    string `json:"kind"`
		ID      string `json:"id"`
		Version int64  `json:"version,omitempty"`
	}
	f := struct {
		TaskID             string            `json:"task_id"`
		Goal               string            `json:"goal"`
		Now                string            `json:"now"`
		Target             tskapi.Target     `json:"target"`
		Source             *tskapi.SourceRef `json:"source,omitempty"`
		SourceFileID       string            `json:"source_file_id,omitempty"`
		RegisteredSourceID string            `json:"registered_source_id,omitempty"`
		SourceText         string            `json:"source_text,omitempty"`
		SourceTruncated    bool              `json:"source_truncated,omitempty"`
		SourceSHA256       string            `json:"source_sha256,omitempty"`
		BoundQuote         string            `json:"bound_quote,omitempty"`
		Arrivals           []arrivalFact     `json:"arrivals,omitempty"`
		Actions            []actionFact      `json:"actions"`
		Refusals           []refusalNote     `json:"refusals,omitempty"`
		ApprovalGrants     []grantFact       `json:"approval_grants,omitempty"`
		Rules              string            `json:"rules"`
	}{TaskID: v.TaskID, Goal: v.GoalCode, Now: kernel.FormatTimestamp(t.r.Clock.Now()), Target: v.Target, Source: v.Source,
		Rules: "Use only the tools offered; their arguments are the members the tool's schema names, an unknown one left out. " +
			"Scope, actor, task, epoch and grants are the platform's. " +
			"Drafts are reviewed and made effective by people. Answer without a tool call when nothing is left for you to do; the platform then verifies the goal."}
	if v.Binding != nil && v.Binding.State == tskapi.BindingBound {
		f.BoundQuote = v.Binding.QuoteID
	}
	for _, h := range v.History {
		// The runner's own resume on an approval grant is not news to the agent: the grant is
		// listed among the approval grants while it is issued.
		if h.Command == tskapi.CommandResume && h.Arrival != nil && h.Arrival.Kind != ArrivalApprovalGrant {
			f.Arrivals = append(f.Arrivals, arrivalFact{Kind: h.Arrival.Kind, ID: h.Arrival.ID, Version: h.Arrival.Version})
		}
	}
	if len(f.Arrivals) > maxArrivalFacts {
		f.Arrivals = f.Arrivals[len(f.Arrivals)-maxArrivalFacts:]
	}
	for _, a := range actions {
		if a.State == tskapi.ActionRejected {
			continue // told with its code, path and reason among the refusals
		}
		af := actionFact{Command: a.Target.Command, State: a.State}
		if a.Result != nil {
			af.ObjectID, af.RowVersion, af.Status = a.Result.ObjectID, a.Result.RowVersion, a.Result.Status
		}
		f.Actions = append(f.Actions, af)
	}
	calls, err := t.r.Evidence.Calls(t.agent, v.TaskID)
	if err != nil {
		return "", err
	}
	f.Refusals = t.refusalFacts(calls)
	grants, err := t.r.issuedGrants(t.agent, t.scope, t.dels)
	if err != nil {
		return "", err
	}
	for _, g := range grants {
		f.ApprovalGrants = append(f.ApprovalGrants, grantFact{Command: g.Command, ObjectID: g.ObjectID, Version: g.ObjectVersion})
	}
	if v.Source != nil && v.Source.Kind == tskapi.SourceFile {
		f.SourceFileID = v.Source.ID
	}
	if t.r.Sources != nil && v.Source != nil {
		var src *evdapi.SourceView
		switch v.Source.Kind {
		case tskapi.SourceEVD:
			src, err = t.readSource(v.Source.ID)
		case tskapi.SourceFile:
			if src, err = t.registeredExtract(actions); src != nil {
				f.RegisteredSourceID = src.SourceID
			}
		}
		if err != nil {
			return "", err
		}
		if src != nil {
			// The full extract is source.get. The fact carries a bounded excerpt so a turn stays
			// within what one model call can hold (S4 G1).
			text, cut := clipRunes(src.ContentText, 2000)
			f.SourceText = text
			if cut {
				f.SourceTruncated = true
				f.SourceSHA256 = src.ContentSHA256
			}
		}
	}
	raw, err := json.Marshal(f)
	return string(raw), err
}

// maxArrivalFacts bounds the arrivals a turn's facts carry: the last five. A Q01 task is resumed
// on the ready RFQ, the activated quote, the customer's reply and the verified acceptance; five
// keeps the reply and the acceptance in sight until the order is prepared.
const maxArrivalFacts = 5

// ProposalID names the approval a task waits for: one per task, command and request.
func ProposalID(taskID, command, requestHash string) string {
	return "agp." + IntentOf(taskID, command, "", requestHash)[3:]
}

// commandKey is the runner's idempotency key of a TSK command on a task at the version it saw.
func commandKey(v tskapi.TaskView, what string) string {
	return "ar." + IntentOf(v.TaskID, what, "", fmt.Sprint(v.TaskVersion))[3:]
}

func withPrincipal(ctx context.Context, kind kernel.ActorKind, id string, scope kernel.Scope) (context.Context, error) {
	actor, err := kernel.NewActorRef(kind, id)
	if err != nil {
		return nil, err
	}
	return identity.WithPrincipal(ctx, identity.Principal{Actor: actor, Scope: scope})
}

func actorID(ctx context.Context) string {
	p, _ := identity.FromContext(ctx)
	return p.Actor.ID
}

// answerOf is what the model sees of a step's receipt.
func answerOf(r Receipt) string {
	a := map[string]any{"ok": true, "receipt": r.Stage, "replayed": r.Replayed}
	switch {
	case r.Stored != nil:
		a["object_id"], a["row_version"], a["status"] = r.Stored.ObjectID, r.Stored.RowVersion, r.Stored.Status
	case r.Result != nil:
		a["object_id"], a["row_version"], a["status"] = r.Result.ObjectID, r.Result.RowVersion, r.Result.Status
	}
	raw, _ := json.Marshal(a)
	return string(raw)
}

func errorCode(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code
	}
	if errors.Is(err, model.ErrUnavailable) {
		return codes.ToolUnavailable
	}
	return "INTERNAL"
}

func reasonOf(err error) string {
	code := errorCode(err)
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 {
		return code + " at " + rej.Fields[0].Path
	}
	return code
}

func auditName(name string) string {
	if len(name) <= 64 && tskapi.CommandName(name) {
		return name
	}
	return "invalid"
}

func hashJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return executionevidence.SHA256Hex(raw)
}

func bounded(s string, max int) string {
	text, _ := clipRunes(s, max)
	return text
}

func clipRunes(s string, max int) (string, bool) {
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	return string([]rune(s)[:max]), true
}
