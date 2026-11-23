// Package application runs MFG's commands and queries on the command ledger. Plan rules live in
// domain/plan and subcontract rules in domain/subcontract; SQL lives in adapters/postgres.
//
// PUR's SubcontractDemand v1 (Context Map R23, guardLock false) is read in Prepare, before the
// subcontract transaction, and only when the issue cites a commitment. The processing quantity is
// not written back as a sales demand. Material handover (R42) is performed by the composition
// root's Handovers port, which calls STK in STK's own transaction before MFG records the
// association. MFG does not import STK: the Context Map edge lets STK import MFG, not the reverse.
// operation.record_progress locks the subcontract only and calls neither QLT nor STK (MFG-INV-001).
//
// Lock order: the command's own root FOR UPDATE (the plan, or the subcontract), then the authority
// guard FOR SHARE. A handover locks the subcontract and no stock row. Children of a plan are not
// locked on their own.
package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/api"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/adapters/postgres"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	anyActor    = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}
	humanAI     = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
	humanSystem = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorSystem}
)

// Rules is MFG's authorization policy. mfg.module.configure and subcontract_order.issue are human
// only. collaboration_plan.publish and operation.record_progress are human or AI (SM-MFG-PLAN,
// SM-MFG-SUBCONTRACT). material.handover is in process, on the purchaser's issue permission.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		authorization.HumanOnly(api.CommandConfigure, api.CommandConfigure),
		{Command: api.CommandPublish, Permission: api.CommandPublish, ActorKinds: humanAI},
		authorization.HumanOnly(api.CommandIssue, api.CommandIssue),
		{Command: api.CommandProgress, Permission: api.CommandProgress, ActorKinds: humanAI},
		{Command: api.CommandHandover, Permission: api.CommandIssue, ActorKinds: humanSystem},
		{Command: api.CommandGetSetting, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetPlan, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetSubcontract, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandAudit, Permission: api.PermissionAudit, ActorKinds: anyActor},
	}
}

// Service runs MFG's use cases. Demands is PUR's SubcontractDemand v1; nil refuses an issue that
// cites a commitment. Handovers is the composition root's port to STK's MaterialHandover v1 (R42);
// nil refuses a handover. MFG does not import STK.
type Service struct {
	Ledger    *action.Ledger
	Clock     application.Clock
	Demands   purapi.SubcontractDemands
	Handovers api.Handovers
}

func scopeOf(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// Visible reports whether d's actor sees the object a stored result names.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	sc := scopeOf(ctx)
	switch command {
	case api.CommandConfigure:
		st, err := postgres.LoadSetting(ctx, exec, sc, postgres.NoLock)
		return err == nil && st != nil && d.Sees(st.ActorID, ""), err
	case api.CommandHandover:
		h, err := postgres.LoadHandover(ctx, exec, sc, id)
		return err == nil && h != nil && d.Sees(h.OwnerActor, h.OwnerTeam), err
	case api.CommandIssue, api.CommandProgress, api.CommandGetSubcontract:
		s, err := postgres.LoadSubcontract(ctx, exec, sc, id, postgres.NoLock)
		return err == nil && s != nil && d.Sees(s.Order.OwnerActor, s.OwnerTeam), err
	default:
		p, err := postgres.LoadPlan(ctx, exec, sc, id, postgres.NoLock)
		return err == nil && p != nil && d.Sees(p.OwnerActor, p.OwnerTeam), err
	}
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
	_, _, args := requestOf(req)
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Receipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	return api.Receipt(res), err
}

// requestOf returns the name, the idempotency key and the business arguments of cmd. The request
// hash covers everything but the key and the expected version.
func requestOf(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.ConfigureModule:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandConfigure, key, c
	case api.PublishPlan:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandPublish, key, c
	case api.IssueSubcontract:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandIssue, key, c
	case api.RecordProgress:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandProgress, key, c
	case api.RecordMaterialHandover:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandHandover, key, c
	}
	return "", "", nil
}

func requestHash(cmd api.Command) string {
	_, _, args := requestOf(cmd)
	h, _ := action.RequestHash(args)
	return h
}

// Reconcile reports what became of cmd without executing it.
func (s Service) Reconcile(ctx context.Context, cmd api.Command) (api.Reconciliation, error) {
	name, key, args := requestOf(cmd)
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

// Audit returns MFG's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, api.CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

func (s Service) moduleOn(ctx context.Context) (bool, error) {
	var on bool
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		st, err := postgres.LoadSetting(ctx, exec, scopeOf(ctx), postgres.NoLock)
		if st != nil {
			on = st.Enabled
		}
		return err
	})
	return on, err
}

func enabled(ctx context.Context, exec bentopg.Executor) error {
	st, err := postgres.LoadSetting(ctx, exec, scopeOf(ctx), postgres.NoLock)
	if err != nil {
		return err
	}
	if st == nil || !st.Enabled {
		return api.Reject(api.CodeDependencyNotSupported, "/", "Manufacturing is not enabled")
	}
	return nil
}

func conflict(err error) error {
	if errors.Is(err, postgres.ErrConflict) {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return err
}

func seen(d authorization.Decision, owner, team, path string) error {
	if !d.Sees(owner, team) {
		return kernel.Reject(kernel.CodeNotFound, path, "")
	}
	return nil
}

func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
