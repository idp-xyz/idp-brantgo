package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.idp.xyz/idp-brant/internal/ai"
	"go.idp.xyz/idp-brant/internal/ai/approvalgrant"
	"go.idp.xyz/idp-brant/internal/ai/execution"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/ai/proposal"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// AI is what the transport needs from the AI layer (internal/ai; ADR-BR-003: not a bounded context;
// Context Map platformCommands AI, S4 units A5, C2): the four human-only commands, the audited reads
// (ai.read) of delegations, approval grants, a task's model and tool calls and the ExecutionBundles,
// the audit trail (ai.audit), and the execution entry of a person's step on a task.
type AI interface {
	GrantDelegation(ctx context.Context, cmd policy.GrantDelegation) (policy.Receipt, error)
	RevokeDelegation(ctx context.Context, cmd policy.RevokeDelegation) (policy.Receipt, error)
	IssueApprovalGrant(ctx context.Context, cmd approvalgrant.IssueGrant) (approvalgrant.Receipt, error)
	RevokeApprovalGrant(ctx context.Context, cmd approvalgrant.RevokeGrant) (approvalgrant.Receipt, error)
	GetDelegation(ctx context.Context, id string) (policy.Delegation, error)
	GetApprovalGrant(ctx context.Context, id string) (approvalgrant.Grant, error)
	GetProposal(ctx context.Context, id string) (proposal.Proposal, error)
	ApprovalGrantsOf(ctx context.Context, delegationID string) ([]approvalgrant.Grant, error)
	ModelCalls(ctx context.Context, taskID string) ([]executionevidence.Call, error)
	ExecutionBundle(ctx context.Context, ref executionevidence.Ref) (executionevidence.Bundle, error)
	RunStep(ctx context.Context, s execution.Step) (execution.Receipt, error)
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// expiry parses a command's expires_at, a UTC timestamp of the contracts (kernel.ParseTimestamp);
// an absent one stays zero and the command refuses it.
func expiry(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := kernel.ParseTimestamp(s)
	if err != nil {
		return time.Time{}, schemaError{kernel.Reject(kernel.CodeSchemaInvalid, "/args/expires_at", "UTC timestamp")}
	}
	return t, nil
}

// aiCommands are the AI layer's commands (Context Map platformCommands AI; owner decision D-7), all
// human-only (authorization.HumanOnly: an AI or system actor is PERMISSION_DENIED) with their own
// permissions:
//
//	ai.delegation.grant      {id, agent_id, task_id, capabilities, autonomy, expires_at}
//	ai.delegation.revoke     {id, expected_version, reason}
//	ai.approval_grant.issue  {id, delegation_id, command, request_hash, object_id, object_version, expires_at}
//	ai.approval_grant.revoke {id, expected_version, reason}
//
// expires_at is a UTC timestamp; object_version is required (0: a new or unversioned object), as the
// grant binds it. The answer is the closed receipt: object_id the delegation or grant, row_version
// its version, status active, revoked, issued or used.
func aiCommands(c AI) *commandSet {
	type grantDelegation struct {
		ID           string   `json:"id"`
		AgentID      string   `json:"agent_id"`
		TaskID       string   `json:"task_id"`
		Capabilities []string `json:"capabilities"`
		Autonomy     string   `json:"autonomy"`
		ExpiresAt    string   `json:"expires_at"`
	}
	type issueGrant struct {
		ID            string `json:"id"`
		DelegationID  string `json:"delegation_id"`
		Command       string `json:"command"`
		RequestHash   string `json:"request_hash"`
		ObjectID      string `json:"object_id"`
		ObjectVersion *int64 `json:"object_version"`
		ExpiresAt     string `json:"expires_at"`
	}
	return &commandSet{bc: "ai", contract: ai.Contract, auditor: c, audit: c.Audit, commands: map[string]commandFunc{
		policy.CommandGrant: handle(func(key string, a grantDelegation) (policy.GrantDelegation, error) {
			at, err := expiry(a.ExpiresAt)
			return policy.GrantDelegation{IdempotencyKey: key, DelegationID: a.ID, AgentID: a.AgentID, TaskID: a.TaskID,
				Capabilities: a.Capabilities, Autonomy: a.Autonomy, ExpiresAt: at}, err
		}, c.GrantDelegation),
		policy.CommandRevoke: handle(func(key string, a reasoned) (policy.RevokeDelegation, error) {
			v, err := version(a.ExpectedVersion)
			return policy.RevokeDelegation{IdempotencyKey: key, DelegationID: a.ID, ExpectedVersion: v, Reason: a.Reason}, err
		}, c.RevokeDelegation),
		approvalgrant.CommandIssue: handle(func(key string, a issueGrant) (approvalgrant.IssueGrant, error) {
			if a.ObjectVersion == nil {
				return approvalgrant.IssueGrant{}, schemaError{kernel.Reject(kernel.CodeSchemaInvalid, "/args/object_version", "required")}
			}
			at, err := expiry(a.ExpiresAt)
			return approvalgrant.IssueGrant{IdempotencyKey: key, GrantID: a.ID, DelegationID: a.DelegationID, Command: a.Command,
				RequestHash: a.RequestHash, ObjectID: a.ObjectID, ObjectVersion: *a.ObjectVersion, ExpiresAt: at}, err
		}, c.IssueApprovalGrant),
		approvalgrant.CommandRevoke: handle(func(key string, a reasoned) (approvalgrant.RevokeGrant, error) {
			v, err := version(a.ExpectedVersion)
			return approvalgrant.RevokeGrant{IdempotencyKey: key, GrantID: a.ID, ExpectedVersion: v, Reason: a.Reason}, err
		}, c.RevokeApprovalGrant),
	}}
}

// aiRoutes are the AI layer's audited reads (ai.read): a delegation and its approval grants (seen by
// its agent or a reader whose data scope covers its initiator), an approval grant, a task's model and
// tool calls (for a reader who sees the task in TSK), a recorded ExecutionBundle; and the task-step
// route, a person's step on a task through the execution entry.
func (s *server) aiRoutes(mux routeMux) {
	mux.Handle("GET /api/brant/v1/ai/delegations/{id}", s.authenticated(read(s, s.cfg.AI.GetDelegation, okView[policy.Delegation])))
	mux.Handle("GET /api/brant/v1/ai/delegations/{id}/approval_grants", s.authenticated(listRead(s, s.cfg.AI.ApprovalGrantsOf, "delegation_id",
		"approval_grants")))
	mux.Handle("GET /api/brant/v1/ai/approval_grants/{id}", s.authenticated(read(s, s.cfg.AI.GetApprovalGrant, okView[approvalgrant.Grant])))
	mux.Handle("GET /api/brant/v1/ai/proposals/{id}", s.authenticated(read(s, s.cfg.AI.GetProposal, proposalOf)))
	mux.Handle("GET /api/brant/v1/ai/tasks/{id}/model_calls", s.authenticated(s.modelCalls))
	mux.Handle("GET /api/brant/v1/ai/execution_bundles/{id}/{sha256}", s.authenticated(s.executionBundle))
	mux.Handle("POST "+StepPath, s.authenticated(s.taskStep))
}

// aiReads are the AI layer's read routes in the OpenAPI document.
var aiReads = []readRoute{
	{"/api/brant/v1/ai/delegations/{id}", "ai.delegation.get", "ai",
		"A delegation: initiator, AI agent, task, capabilities, autonomy, expiry, status and revocation (ai.delegation.get)", []string{"id"}, false},
	{"/api/brant/v1/ai/delegations/{id}/approval_grants", "ai.approval_grant.list", "ai",
		"The approval grants issued under a delegation, each with the command, request hash, object and version it binds, its expiry and its use (ai.approval_grant.get)", []string{"id"}, false},
	{"/api/brant/v1/ai/approval_grants/{id}", "ai.approval_grant.get", "ai",
		"An approval grant: the call it binds (command, request hash, object and version), its expiry, issuer, status and the key of the call that used it (ai.approval_grant.get)", []string{"id"}, false},
	{"/api/brant/v1/ai/proposals/{id}", "ai.proposal.get", "ai",
		"The request an AI agent proposed and a person is asked to approve with an approval grant: the command, the arguments of the request as the agent made them (without the idempotency key), its hash, the object and version a grant binds, the delegation, the status (pending, sending, sent, refused, void) and, once settled, the refusal's code and path or the TSK action that committed it (ai.proposal.get)", []string{"id"}, false},
	{"/api/brant/v1/ai/tasks/{id}/model_calls", "ai.model_call.list", "ai",
		"The model and tool calls of a task in order: reservation, outcome, the model ID as returned, prompt/input/output hashes, tokens, latency, cost, the action a tool call ran and, for a refused tool call, the code, the request path and the refusing context's static reason (ai.model_call.list; no prompt or output content)", []string{"id"}, false},
	{"/api/brant/v1/ai/execution_bundles/{id}/{sha256}", "ai.execution_bundle.get", "ai",
		"A recorded ExecutionBundle by its ID and the SHA-256 of its canonical JSON, with that canonical document (ai.execution_bundle.get)", []string{"id", "sha256"}, false},
}

// proposalView is a proposal as the read shows it. arguments is the request as the agent's tool call
// carried it, in the names of the tool's schema and without the idempotency key; a person approves
// what it shows (S4 G7b).
type proposalView struct {
	OK            bool            `json:"ok"`
	ProposalID    string          `json:"proposal_id"`
	TaskID        string          `json:"task_id"`
	DelegationID  string          `json:"delegation_id"`
	AgentID       string          `json:"agent_id"`
	InitiatorID   string          `json:"initiator_id"`
	Command       string          `json:"command"`
	RequestHash   string          `json:"request_hash"`
	ObjectType    string          `json:"object_type"`
	ObjectID      string          `json:"object_id"`
	ObjectVersion int64           `json:"object_version"`
	Arguments     json.RawMessage `json:"arguments"`
	Status        string          `json:"status"`
	RowVersion    int64           `json:"row_version"`
	Attempts      int             `json:"attempts"`
	Sends         int             `json:"sends"`
	ProposedAt    string          `json:"proposed_at"`
	ClaimedAt     string          `json:"claimed_at,omitempty"`
	GrantID       string          `json:"grant_id,omitempty"`
	SettledAt     string          `json:"settled_at,omitempty"`
	OutcomeCode   string          `json:"outcome_code,omitempty"`
	OutcomePath   string          `json:"outcome_path,omitempty"`
	ActionID      string          `json:"action_id,omitempty"`
}

func proposalOf(p proposal.Proposal) proposalView {
	v := proposalView{OK: true, ProposalID: p.ProposalID, TaskID: p.TaskID, DelegationID: p.DelegationID, AgentID: p.AgentID,
		InitiatorID: p.InitiatorID, Command: p.Command, RequestHash: p.RequestHash, ObjectType: p.ObjectType, ObjectID: p.ObjectID,
		ObjectVersion: p.ObjectVersion, Arguments: p.Arguments, Status: p.Status, RowVersion: p.RowVersion, Attempts: p.Attempts,
		Sends: p.Sends, ProposedAt: timestamp(p.ProposedAt), GrantID: p.GrantID, OutcomeCode: p.OutcomeCode, OutcomePath: p.OutcomePath,
		ActionID: p.ActionID}
	if !p.ClaimedAt.IsZero() {
		v.ClaimedAt = timestamp(p.ClaimedAt)
	}
	if !p.SettledAt.IsZero() {
		v.SettledAt = timestamp(p.SettledAt)
	}
	return v
}

// moneyView is an amount as the reads show it: currency, its minor units and the decimal amount.
type moneyView struct {
	Currency   string `json:"currency"`
	MinorUnits int    `json:"minor_units"`
	Amount     string `json:"amount"`
}

func moneyOf(m kernel.Money) moneyView {
	return moneyView{Currency: m.Currency().Code(), MinorUnits: m.Currency().MinorUnits(), Amount: m.Amount()}
}

// callView is one model or tool call of a task (executionevidence.Call).
type callView struct {
	CallID         string     `json:"call_id"`
	Seq            int        `json:"seq"`
	Kind           string     `json:"kind"`
	Retry          bool       `json:"retry"`
	State          string     `json:"state"`
	ActorID        string     `json:"actor_id"`
	TaskVersion    int64      `json:"task_version"`
	BundleID       string     `json:"bundle_id"`
	BundleSHA256   string     `json:"bundle_sha256"`
	ProfileID      string     `json:"profile_id,omitempty"`
	Provider       string     `json:"provider,omitempty"`
	ModelRequested string     `json:"model_requested,omitempty"`
	PromptSHA256   string     `json:"prompt_sha256,omitempty"`
	ToolCommand    string     `json:"tool_command,omitempty"`
	Reserved       moneyView  `json:"reserved"`
	ReservedAt     string     `json:"reserved_at"`
	ModelReturned  string     `json:"model_returned,omitempty"`
	InputSHA256    string     `json:"input_sha256,omitempty"`
	OutputSHA256   string     `json:"output_sha256,omitempty"`
	InputTokens    *int64     `json:"input_tokens,omitempty"`
	OutputTokens   *int64     `json:"output_tokens,omitempty"`
	LatencyMS      *int64     `json:"latency_ms,omitempty"`
	FinishReason   string     `json:"finish_reason,omitempty"`
	ActionID       string     `json:"action_id,omitempty"`
	Code           string     `json:"code,omitempty"`
	RefusalPath    string     `json:"refusal_path,omitempty"`
	RefusalReason  string     `json:"refusal_reason,omitempty"`
	Cost           *moneyView `json:"cost,omitempty"`
	SettledAt      string     `json:"settled_at,omitempty"`
}

// modelCalls answers GET /api/brant/v1/ai/tasks/<task_id>/model_calls.
func (s *server) modelCalls(w http.ResponseWriter, r *http.Request) {
	calls, err := s.cfg.AI.ModelCalls(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]callView, len(calls))
	for i, c := range calls {
		v := callView{CallID: c.CallID, Seq: c.Seq, Kind: c.Kind, Retry: c.Retry, State: c.State, ActorID: c.ActorID, TaskVersion: c.TaskVersion,
			BundleID: c.Bundle.BundleID, BundleSHA256: c.Bundle.SHA256, ProfileID: c.ProfileID, Provider: c.Provider,
			ModelRequested: c.ModelRequested, PromptSHA256: c.PromptSHA256, ToolCommand: c.ToolCommand, Reserved: moneyOf(c.Reserved),
			ReservedAt: timestamp(c.ReservedAt), ModelReturned: c.ModelReturned, InputSHA256: c.InputSHA256, OutputSHA256: c.OutputSHA256,
			InputTokens: c.InputTokens, OutputTokens: c.OutputTokens, LatencyMS: c.LatencyMS, FinishReason: c.FinishReason,
			ActionID: c.ActionID, Code: c.Code, RefusalPath: c.RefusalPath, RefusalReason: c.RefusalReason}
		if c.Cost != nil {
			m := moneyOf(*c.Cost)
			v.Cost = &m
		}
		if !c.SettledAt.IsZero() {
			v.SettledAt = timestamp(c.SettledAt)
		}
		out[i] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task_id": r.PathValue("id"), "calls": out})
}

// executionBundle answers GET /api/brant/v1/ai/execution_bundles/<bundle_id>/<sha256>: the bundle's
// canonical JSON, whose SHA-256 is the one in the path.
func (s *server) executionBundle(w http.ResponseWriter, r *http.Request) {
	b, err := s.cfg.AI.ExecutionBundle(r.Context(), executionevidence.Ref{BundleID: r.PathValue("id"), SHA256: r.PathValue("sha256")})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	doc, err := b.Canonical()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bundle_id": b.BundleID, "sha256": executionevidence.SHA256Hex(doc),
		"bundle": json.RawMessage(doc)})
}
