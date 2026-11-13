package ai

import (
	"context"
	"errors"
	"log/slog"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/execution"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Reads of the execution evidence (permission ai.read; not Context Map commands, like the other
// audited reads of the AI layer).
const (
	// CommandModelCalls reads the model and tool calls of a task the reader sees in TSK.
	CommandModelCalls = "ai.model_call.list"
	// CommandBundle reads a recorded ExecutionBundle.
	CommandBundle = "ai.execution_bundle.get"
)

func evidenceRules() []authorization.Rule {
	any := []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}
	return []authorization.Rule{
		{Command: CommandModelCalls, Permission: "ai.read", ActorKinds: any},
		{Command: CommandBundle, Permission: "ai.read", ActorKinds: any},
	}
}

// WithTasks wires the execution entry (internal/ai/execution) to TSK and to the owning contexts'
// request identities: Layer.RunStep, Layer.Runner and Layer.ModelCalls need it.
func WithTasks(tasks execution.RunnerTasks, requests execution.Requests) Option {
	return func(l *Layer) {
		l.tasks = tasks
		l.Execution = execution.Executor{Tasks: tasks, Requests: requests}
	}
}

// ErrNoTasks is a Layer built without WithTasks asked to execute.
var ErrNoTasks = errors.New("ai: the execution entry is not wired to TSK")

// RunStep runs one task-bound step as the Principal of ctx through the execution entry — the path
// of a person's step on a task (unit C3's task-step route) as of the AI runner's (decision D-2):
// claim, intent recorded before the send, the command sent with the action's key and the task's
// control claim, the outcome recorded and reconciled. See execution.Executor.Run.
func (l *Layer) RunStep(ctx context.Context, s execution.Step) (execution.Receipt, error) {
	if l.tasks == nil {
		return execution.Receipt{}, ErrNoTasks
	}
	return l.Execution.Run(ctx, s)
}

// RunnerConfig configures the agent runner (brant-worker -agent).
type RunnerConfig struct {
	Scopes   []kernel.Scope
	Agents   []string
	Model    model.Port
	Profile  executionevidence.ModelProfile
	Budget   executionevidence.BudgetSpec // the default budget of tasks that name none
	Accepts  []string                     // SHA-256s of earlier bundles still run
	Sources  execution.Sources            // optional
	MaxSteps int
	Logger   *slog.Logger
}

// Bundle returns the ExecutionBundle the runner of cfg runs with: the Q01 task profile over the
// layer's registry, cfg's model profile and default budget, and the Q01 prompt.
func (l *Layer) Bundle(cfg RunnerConfig) (executionevidence.Bundle, error) {
	return execution.Q01Bundle(l.Registry, cfg.Profile, cfg.Budget)
}

// Runner assembles the agent runner over the layer.
func (l *Layer) Runner(cfg RunnerConfig) (*execution.Runner, error) {
	if l.tasks == nil {
		return nil, ErrNoTasks
	}
	bundle, err := l.Bundle(cfg)
	if err != nil {
		return nil, err
	}
	return &execution.Runner{Executor: l.Execution, Tasks: l.tasks, Gateway: l.Gateway, DB: l.ledger.DB, Evidence: l.Evidence,
		Model: cfg.Model, Bundle: bundle, Prompt: execution.Q01Prompt, Accepts: cfg.Accepts, Sources: cfg.Sources, Scopes: cfg.Scopes,
		Agents: cfg.Agents, MaxSteps: cfg.MaxSteps, Clock: l.Evidence.Clock, Logger: cfg.Logger}, nil
}

// ModelCalls returns the model and tool calls of a task (ai.read), audited as ai.model_call.list,
// for a reader who sees the task in TSK (NOT_FOUND otherwise).
func (l *Layer) ModelCalls(ctx context.Context, taskID string) ([]executionevidence.Call, error) {
	if l.tasks == nil {
		return nil, ErrNoTasks
	}
	if _, err := l.tasks.GetTask(ctx, taskID); err != nil {
		return nil, err
	}
	var out []executionevidence.Call
	err := l.ledger.Read(ctx, CommandModelCalls, taskID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		out, err = executionevidence.LoadCalls(ctx, exec, scopeOf(ctx), taskID)
		return err
	})
	return out, err
}

// ExecutionBundle returns a recorded ExecutionBundle (ai.read), audited as ai.execution_bundle.get;
// NOT_FOUND when the scope has none under ref.
func (l *Layer) ExecutionBundle(ctx context.Context, ref executionevidence.Ref) (executionevidence.Bundle, error) {
	var out executionevidence.Bundle
	err := l.ledger.Read(ctx, CommandBundle, ref.BundleID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		b, err := executionevidence.LoadBundle(ctx, exec, scopeOf(ctx), ref)
		if err != nil {
			return err
		}
		if b == nil {
			return kernel.Reject(kernel.CodeNotFound, "/bundle_id", "")
		}
		out = *b
		return nil
	})
	return out, err
}

func scopeOf(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}
