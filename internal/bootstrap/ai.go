package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai"
	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/adapters/model/openaicompat"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/execution"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// newAI assembles the AI layer (internal/ai; ADR-BR-003: not a bounded context) on db, a
// transaction owner of its own: the Q01 capabilities over the contexts' api (decision D-7), and the
// execution entry of task-bound steps over TSK and the request identities of the contexts a task
// step writes (S4 unit C2; decision D-2).
func newAI(db *bentopg.DB, c *Contexts) (*ai.Layer, error) {
	reg, err := capability.Q01Registry(capability.Q01Targets{EVD: c.EVD, INQ: c.INQ, CST: c.CST, CSTScenarios: c.CST, TRD: c.TRD,
		TRDAcceptance: c.TRD, TRDOrders: c.TRD})
	if err != nil {
		return nil, err
	}
	return ai.New(db, reg, ai.WithTasks(c.TSK, execution.Requests{EVD: c.EVD, INQ: c.INQ, SUP: c.SUP, CST: c.CST, TRD: c.TRD}))
}

// AgentConfig configures the agent runner of brant-worker -agent.
type AgentConfig struct {
	// Scopes and Agents: the runner acts as each AI actor of Agents in each scope, on the tasks the
	// actor holds delegations for.
	Scopes []kernel.Scope
	Agents []string
	// Model is the model port: the scripted model by default, the OpenAI-compatible adapter for
	// live runs on the owner's machine (decision E-4); Profile describes it.
	Model   model.Port
	Profile executionevidence.ModelProfile
	// Accepts lists the SHA-256s of earlier ExecutionBundles the runner still runs (baseline 17.3).
	Accepts  []string
	MaxSteps int
	Logger   *slog.Logger
}

// Agent assembles the agent runner over c: the Q01 bundle with cfg's model profile and the default
// budget of decision E-4, the runner reading a task's EVD source as its agent.
func (c *Contexts) Agent(cfg AgentConfig) (*execution.Runner, error) {
	if c.AI == nil {
		return nil, errors.New("bootstrap: the AI layer is not wired")
	}
	return c.AI.Runner(ai.RunnerConfig{Scopes: cfg.Scopes, Agents: cfg.Agents, Model: cfg.Model, Profile: cfg.Profile,
		Budget: execution.Q01DefaultBudget(), Accepts: cfg.Accepts, Sources: c.EVD, MaxSteps: cfg.MaxSteps, Logger: cfg.Logger})
}

// Agent wires every context and returns the agent runner over them. Call it at most once per
// process, instead of Contexts.
func (p *Platform) Agent(cfg AgentConfig) (*execution.Runner, error) {
	c, err := p.Contexts()
	if err != nil {
		return nil, err
	}
	return c.Agent(cfg)
}

// ProvisionAgentRunner gives the agent runner's system actor (execution.RunnerActor,
// system:ai.runner) exactly execution.RunnerPermissions with data scope company in scope, creating
// the scope's authority row when it is missing (brant-admin agent). It is an operator's change like
// ProvisionInbox; the AI agents themselves get their role template (ai_q01_agent) with brant-admin
// assign. It returns the scope's authority epoch after the change.
func (p *Platform) ProvisionAgentRunner(ctx context.Context, scope kernel.Scope) (int64, error) {
	actor, err := kernel.NewActorRef(kernel.ActorSystem, execution.RunnerActor)
	if err != nil {
		return 0, err
	}
	return p.inAuthority(ctx, scope, func(ctx context.Context, exec bentopg.Executor, g authorization.Grants, now time.Time) (int64, error) {
		if err := g.Provision(ctx, exec, scope); err != nil {
			return 0, err
		}
		if _, err := g.Put(ctx, exec, scope, actor, execution.RunnerPermissions, now); err != nil {
			return 0, err
		}
		return g.SetDataScope(ctx, exec, scope, actor.ID, authorization.DataScopeCompany, "", now)
	})
}

// ProvisionAgentRunnerFor is ProvisionAgentRunner from plain operator input, for cmd/brant-admin.
func (p *Platform) ProvisionAgentRunnerFor(ctx context.Context, tenant, company string) (int64, error) {
	scope, err := kernel.NewScope(tenant, company)
	if err != nil {
		return 0, err
	}
	return p.ProvisionAgentRunner(ctx, scope)
}

// ModelConfig chooses the agent runner's model (brant-worker -model): "scripted" (the default and
// the only one in CI) answers the steps of Script, a script file's JSON (model.ParseScript);
// "openaicompat" calls an OpenAI-compatible service at BaseURL (decision E-4: the local service at
// http://127.0.0.1:8080) with the optional bearer APIKey, which the caller reads from the
// environment and which is never logged.
type ModelConfig struct {
	Kind    string
	Name    string
	Script  []byte
	BaseURL string
	APIKey  string
	Timeout time.Duration
}

// Model kinds of ModelConfig.
const (
	ModelScripted     = "scripted"
	ModelOpenAICompat = "openaicompat"
)

// NewModel returns the model port of cfg and its profile (cost 0 for both kinds).
func NewModel(cfg ModelConfig) (model.Port, executionevidence.ModelProfile, error) {
	switch cfg.Kind {
	case ModelScripted, "":
		if cfg.Name == "" {
			cfg.Name = "scripted-q01-v1"
		}
		steps, err := model.ParseScript(cfg.Script)
		if err != nil {
			return nil, executionevidence.ModelProfile{}, err
		}
		return model.NewScripted(cfg.Name, steps...), execution.ScriptedProfile(cfg.Name), nil
	case ModelOpenAICompat:
		if cfg.Name == "" {
			cfg.Name = "qwen3.8-flash-next-iq3_xxs"
		}
		c, err := openaicompat.New(openaicompat.Config{BaseURL: cfg.BaseURL, Model: cfg.Name, APIKey: cfg.APIKey, Timeout: cfg.Timeout})
		if err != nil {
			return nil, executionevidence.ModelProfile{}, err
		}
		return c, execution.StrataProfile(cfg.Name), nil
	}
	return nil, executionevidence.ModelProfile{}, fmt.Errorf("bootstrap: model kind %q is not scripted or openaicompat", cfg.Kind)
}

// ParseScopes reads "tenant/company" pairs separated by commas.
func ParseScopes(list string) ([]kernel.Scope, error) {
	var out []kernel.Scope
	for _, item := range strings.Split(list, ",") {
		tenant, company, ok := strings.Cut(strings.TrimSpace(item), "/")
		if !ok {
			return nil, fmt.Errorf("bootstrap: scope %q is not tenant/company", item)
		}
		s, err := kernel.NewScope(tenant, company)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
