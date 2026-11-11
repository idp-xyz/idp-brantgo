// Package application runs PUR's commands and queries on the command ledger. The purchase draft
// rules live in domain/draft, the demand allocation in domain/allocation; SQL lives in
// adapters/postgres.
//
// PUR's upstream contexts are TRD (Context Map R19, ActiveOrderRef v1, through PUR's own port
// Orders), SUP (R20, SupplyConditionRef v1 and SupplyConditionGuard v1), CST (R21, CostSnapshotRef
// v1 and AdoptabilityGuard v1, for the costing comparison), PTY (R22, PartyRef v1 and PartyGuard
// v1), POL (R48, ConfigRef v1 and ConfigGuard v1) and CAT (R52, SkuRef v1 and SkuGuard v1). A
// command reads them through their api packages before PUR's transaction begins (the ledger's
// Preparer step) and the domain judges what they said; inside the transaction, after PUR locked its
// own roots FOR UPDATE, the owners' guard functions re-check every adopted reference in context code
// order (CAT, CST, POL, PTY, SUP, TRD), each locking its row FOR SHARE until PUR commits
// (ADR-BR-004 option B, ADR-BR-011). PUR never calls another context's transaction owner.
package application

import (
	"context"
	"errors"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules is PUR's authorization policy. The tactical model's humanOnlyCommands name
// purchase.draft.prepare and purchase.draft.void. Reads need pur.read, the audit trail pur.audit.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		authorization.HumanOnly(api.CommandPrepareDraft, api.CommandPrepareDraft),
		authorization.HumanOnly(api.CommandVoidDraft, api.CommandVoidDraft),
		{Command: api.CommandGetDraft, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetAllocations, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandAudit, Permission: api.PermissionAudit, ActorKinds: anyActor},
	}
}

// Upstream are the contexts PUR reads references from.
type Upstream struct {
	Orders    Orders                 // TRD ActiveOrderRef v1 (R19), PUR's port
	Supply    supapi.Queries         // SUP SupplyConditionRef v1 (R20)
	Snapshots cstapi.SnapshotLookups // CST CostSnapshotRef v1 (R21)
	Parties   ptyapi.Queries         // PTY PartyRef v1 (R22)
	Configs   polapi.Queries         // POL ConfigRef v1 (R48)
	Skus      catapi.Queries         // CAT SkuRef v1 (R52)
}

// Complete reports whether every upstream is wired.
func (u Upstream) Complete() bool {
	return u.Orders != nil && u.Supply != nil && u.Snapshots != nil && u.Parties != nil && u.Configs != nil && u.Skus != nil
}

// Service runs PUR's use cases.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Upstream Upstream
}

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// Visible reports whether d's actor sees the draft a stored result names; a draft whose record is
// not intact is REFERENCE_INVALID, never replayed (Python _load on a write replay).
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, _ string, id string) (bool, error) {
	sc := scope(ctx)
	dr, err := postgres.LoadDraft(ctx, exec, sc, id, postgres.NoLock)
	if err != nil || dr == nil || !d.Sees(dr.OwnerActor, dr.OwnerTeam) {
		return false, err
	}
	return true, dr.Verify(sc, "/args/draft_id")
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
// request hash covers: everything but the key and the expected version.
func request(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.PrepareDraft:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandPrepareDraft, key, c
	case api.VoidDraft:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandVoidDraft, key, c
	}
	return "", "", nil
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

// Audit returns PUR's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, api.CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}
