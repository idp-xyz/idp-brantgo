// Package application runs CLM's commands and queries on the command ledger. The claim rules live
// in domain/claim; SQL lives in adapters/postgres.
//
// CLM reads FUL's ShipmentLineRef v1 (Context Map R34, guardLock false) before its transaction and
// does not lock the shipment. Evidence sources and the shipment currency's minor units are read
// the same way, through ports the composition root implements. A claim transaction locks its own
// claim root FOR UPDATE, then the authority guard FOR SHARE, and for a return or a remedy the
// shipment line's lock row FOR UPDATE. It never calls FUL, EVD, POL or STL.
package application

import (
	"context"
	"errors"
	"fmt"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/clm/api"
	"go.idp.xyz/idp-brant/internal/contexts/clm/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules is CLM's authorization policy. SM-CLM-CLAIM names claim.open and return.register as
// human or AI. remedy.decide and claim.close are human only (humanOnlyCommands, CLM-INV-003).
// Reads need clm.read, the audit trail clm.audit.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandOpenClaim, Permission: api.CommandOpenClaim, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		{Command: api.CommandRegisterReturn, Permission: api.CommandRegisterReturn, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		authorization.HumanOnly(api.CommandDecideRemedy, api.CommandDecideRemedy),
		authorization.HumanOnly(api.CommandCloseClaim, api.CommandCloseClaim),
		{Command: api.CommandGetClaim, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandAudit, Permission: api.PermissionAudit, ActorKinds: anyActor},
	}
}

// Upstream are the facts CLM reads before its transaction.
type Upstream struct {
	Lines    ShipmentLines
	Evidence Evidence
	Currency Currencies
}

// Complete reports whether every upstream is wired.
func (u Upstream) Complete() bool {
	return u.Lines != nil && u.Evidence != nil && u.Currency != nil
}

// Service runs CLM's use cases.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Upstream Upstream
}

func scopeOf(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// Visible reports whether d's actor sees the claim a stored result names.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, _, id string) (bool, error) {
	c, err := postgres.LoadCase(ctx, exec, scopeOf(ctx), id, postgres.NoLock)
	return err == nil && c != nil && d.Sees(c.OwnerActor, c.OwnerTeam), err
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
	case api.OpenClaim:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandOpenClaim, key, c
	case api.RegisterReturn:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandRegisterReturn, key, c
	case api.DecideRemedy:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandDecideRemedy, key, c
	case api.CloseClaim:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandCloseClaim, key, c
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

// Audit returns CLM's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, api.CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

func (s Service) evidence(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	got, err := s.Upstream.Evidence.Resolve(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		if _, ok := got[id]; !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, fmt.Sprintf("/args/evidence_ids/%d", i), "Evidence is not accessible")
		}
	}
	return got, nil
}

func conflict(err error) error {
	if errors.Is(err, postgres.ErrConflict) {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return err
}

func seen(d authorization.Decision, owner, team string) error {
	if !d.Sees(owner, team) {
		return kernel.Reject(kernel.CodeNotFound, "/args/claim_id", "")
	}
	return nil
}
