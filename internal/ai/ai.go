// Package ai assembles the AI layer (ADR-BR-003: not a bounded context; repository structure §13,
// layer "inbound"): the capability registry, the delegations and approval grants in brant_ai on a
// transaction owner of their own, and the gateway that turns a model's tool calls into registered
// api commands. It imports only context api packages, the kernel and foundation (dependency-check
// D6).
//
// What other units call:
//
//   - the execution entry and agent runner (unit C2): Layer.Gateway — Decode a model.ToolCall
//     under a gateway.Binding (task and delegation from TSK, grant from the human), persist the
//     action intent, Admit with its idempotency key, Run with the task's claim in the context;
//     Gateway.Tools offers the model only what the delegation allows; model.Port is the model;
//   - the HTTP routes (unit C3): GrantDelegation, RevokeDelegation, IssueApprovalGrant,
//     RevokeApprovalGrant (human-only), GetDelegation, GetApprovalGrant, ApprovalGrantsOf
//     (ai.read), Audit (ai.audit), AuditRejected;
//   - bootstrap: New with its own *bentopg.DB and capability.Q01Registry over the composed
//     contexts.
package ai

import (
	"context"
	"strings"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/ai/approvalgrant"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/gateway"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// Source identifies the AI layer in the ledger; it publishes no integration events.
	Source = "idp-brant.ai"
	// CommandAudit reads the AI layer's audit trail (permission ai.audit).
	CommandAudit = "ai.audit"
	// PermissionAudit reads the AI layer's audit trail (role templates compliance, administrator).
	PermissionAudit = "ai.audit"
)

// Rules are the authorization rules of the AI layer's commands and reads: the four human-only
// commands of platformCommands AI, the reads (ai.read) and the audit (ai.audit).
func Rules() []authorization.Rule {
	rules := append(policy.Rules(), approvalgrant.Rules()...)
	return append(rules, authorization.Rule{Command: CommandAudit, Permission: PermissionAudit,
		ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}})
}

// Layer is the AI layer of a process.
type Layer struct {
	Registry    *capability.Registry
	Delegations policy.Service
	Grants      approvalgrant.Service
	Gateway     gateway.Gateway
	ledger      *action.Ledger
}

// Option changes the assembly; the tests use it.
type Option func(*Layer)

// WithClock replaces the system clock.
func WithClock(c application.Clock) Option {
	return func(l *Layer) {
		l.ledger.Clock, l.Delegations.Clock, l.Grants.Clock = c, c, c
		l.Gateway.Policy, l.Gateway.Grants = l.Delegations, l.Grants
	}
}

// New wires the AI layer to db, a transaction owner that belongs to it alone, with registry as the
// capabilities AI actors may call (nil: none, everything is refused).
func New(db *bentopg.DB, registry *capability.Registry, opts ...Option) (*Layer, error) {
	if registry == nil {
		var err error
		if registry, err = capability.NewRegistry(); err != nil {
			return nil, err
		}
	}
	rules, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		return nil, err
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	clock := bento.SystemClock{}
	ledger := &action.Ledger{DB: db, Schema: policy.Schema, Source: Source, Authorizer: authorization.Authorizer{Policy: rules},
		Outbox: outbox, Clock: clock, ReplayRead: policy.CommandGet, Visible: visible}
	l := &Layer{Registry: registry, ledger: ledger,
		Delegations: policy.Service{Ledger: ledger, Clock: clock, Registry: registry},
		Grants:      approvalgrant.Service{Ledger: ledger, Clock: clock, Registry: registry}}
	l.Gateway = gateway.Gateway{Registry: registry, Policy: l.Delegations, Grants: l.Grants, Auditor: ledger}
	for _, o := range opts {
		o(l)
	}
	return l, nil
}

// visible dispatches the ledger's replay visibility to the command's records.
func visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	if strings.HasPrefix(command, "ai.approval_grant.") {
		return approvalgrant.Visible(ctx, exec, d, id)
	}
	return policy.Visible(ctx, exec, d, id)
}

// GrantDelegation grants a delegation (human-only; policy.Service.GrantDelegation).
func (l *Layer) GrantDelegation(ctx context.Context, cmd policy.GrantDelegation) (policy.Receipt, error) {
	return l.Delegations.GrantDelegation(ctx, cmd)
}

// RevokeDelegation revokes a delegation (human-only).
func (l *Layer) RevokeDelegation(ctx context.Context, cmd policy.RevokeDelegation) (policy.Receipt, error) {
	return l.Delegations.RevokeDelegation(ctx, cmd)
}

// IssueApprovalGrant issues an approval grant (human-only; approvalgrant.Service.IssueGrant).
func (l *Layer) IssueApprovalGrant(ctx context.Context, cmd approvalgrant.IssueGrant) (approvalgrant.Receipt, error) {
	return l.Grants.IssueGrant(ctx, cmd)
}

// RevokeApprovalGrant revokes an unused approval grant (human-only).
func (l *Layer) RevokeApprovalGrant(ctx context.Context, cmd approvalgrant.RevokeGrant) (approvalgrant.Receipt, error) {
	return l.Grants.RevokeGrant(ctx, cmd)
}

// GetDelegation reads a delegation (ai.read).
func (l *Layer) GetDelegation(ctx context.Context, id string) (policy.Delegation, error) {
	return l.Delegations.GetDelegation(ctx, id)
}

// GetApprovalGrant reads an approval grant (ai.read).
func (l *Layer) GetApprovalGrant(ctx context.Context, id string) (approvalgrant.Grant, error) {
	return l.Grants.GetGrant(ctx, id)
}

// ApprovalGrantsOf reads the approval grants of a delegation (ai.read).
func (l *Layer) ApprovalGrantsOf(ctx context.Context, delegationID string) ([]approvalgrant.Grant, error) {
	return l.Grants.GrantsOf(ctx, delegationID)
}

// Audit returns the AI layer's audit trail of the caller's scope (ai.audit).
func (l *Layer) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return l.ledger.Audit(ctx, CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it reached the AI layer.
func (l *Layer) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return l.ledger.AuditRejected(ctx, command, objectID, code)
}
