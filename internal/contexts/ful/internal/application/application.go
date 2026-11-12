// Package application runs FUL's commands and queries on the command ledger. The shipment rules
// live in domain/shipment; SQL lives in adapters/postgres.
//
// FUL's upstream contexts are TRD (Context Map R27: ActiveOrderRef v1 before the transaction,
// OrderGuard v1 inside it), QLT (R26: QualityReleaseRef v1, QualityReleaseGuard v1), PUR (R59:
// supplier delivery lines and SupplierDeliveryGuard v1, through FUL's port Deliveries), PTY (R44:
// PartyScreeningGuard v1) and EVD (R74: SourceRef v1 for evidence). A command reads them before its
// transaction (the ledger's Preparer step); inside it, it calls their guard functions with FUL's own
// executor, through the ports below. FUL never calls another context's transaction owner. It
// publishes ShipmentFacts v1 to TRD (R70).
//
// Lock order inside FUL's transactions (ADR-BR-011; tactical lockOrder "FUL 为出运根与订单履约根";
// S5 decision D-3): the shipment its ID names FOR UPDATE; for shipment.prepare and shipment.confirm
// then the order's fulfilment root FOR UPDATE (prepare creates it with the order's first shipment);
// then the company authority guard FOR SHARE (the ledger); then, in shipment.confirm, the upstream
// guard rows FOR SHARE in context code order — CNX (extension points until S5 unit C1), PTY's
// screening per party in party ID order, PUR's delivery per delivery line in (delivery, line) order,
// QLT's release per release in ID order, STL (extension point), TRD's order — and FUL's commercial
// release extension point. shipment.prepare calls TRD's OrderGuard only. packing.plan,
// packing.record_actual and shipment.cancel lock the shipment alone: they change nothing an order's
// other shipments are judged on.
package application

import (
	"context"
	"errors"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	anyActor     = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}
	humanOrAgent = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
)

// Rules is FUL's authorization policy. The tactical model's humanOnlyCommands name
// shipment.confirm: an AI or system actor is PERMISSION_DENIED. shipment.prepare, packing.plan,
// packing.record_actual and shipment.cancel are human or AI (SM-FUL-SHIPMENT: cancel "human|ai").
// The permission of a command is its name (role template merchandiser). Reads need ful.read, the
// audit trail ful.audit.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandPrepareShipment, Permission: api.CommandPrepareShipment, ActorKinds: humanOrAgent},
		{Command: api.CommandPlanPacking, Permission: api.CommandPlanPacking, ActorKinds: humanOrAgent},
		{Command: api.CommandRecordPacking, Permission: api.CommandRecordPacking, ActorKinds: humanOrAgent},
		{Command: api.CommandCancelShipment, Permission: api.CommandCancelShipment, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandConfirmShipment, api.CommandConfirmShipment),
		{Command: api.CommandGetShipment, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetFulfilment, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandAudit, Permission: api.PermissionAudit, ActorKinds: anyActor},
	}
}

// Upstream are the contexts FUL reads, through its ports.
type Upstream struct {
	Orders     Orders         // TRD ActiveOrderRef v1 and OrderGuard v1 (R27)
	Releases   Releases       // QLT QualityReleaseRef v1 and QualityReleaseGuard v1 (R26)
	Deliveries Deliveries     // PUR supplier delivery lines and SupplierDeliveryGuard v1 (R59), FUL's port
	Screening  Screening      // PTY PartyScreeningGuard v1 (R44)
	Sources    evdapi.Queries // EVD SourceRef v1 (R74)
	// Extensions are the guards shipment.confirm names before S5 unit C1 wires them; nil ones are
	// recorded as not wired.
	Extensions Extensions
}

// Complete reports whether every upstream is wired.
func (u Upstream) Complete() bool {
	return u.Orders != nil && u.Releases != nil && u.Deliveries != nil && u.Screening != nil && u.Sources != nil
}

// Service runs FUL's use cases.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Upstream Upstream
}

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// Visible reports whether d's actor sees the shipment a stored result names.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, _, id string) (bool, error) {
	s, err := postgres.LoadShipment(ctx, exec, scope(ctx), id, postgres.NoLock)
	return err == nil && s != nil && d.Sees(s.OwnerActor, s.OwnerTeam), err
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
	case api.PrepareShipment:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandPrepareShipment, key, c
	case api.PlanPacking:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandPlanPacking, key, c
	case api.RecordPacking:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRecordPacking, key, c
	case api.CancelShipment:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandCancelShipment, key, c
	case api.ConfirmShipment:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandConfirmShipment, key, c
	}
	return "", "", nil
}

// requestHash is the hash the ledger records for cmd; a taken shipment ID is answered from the
// stored shipment only for the same hash.
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

// Audit returns FUL's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, api.CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

// sources asks EVD which of ids the caller reads. A command that cites no source does not ask and
// needs no EVD permission.
func (s Service) sources(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	refs, err := s.Upstream.Sources.ResolveSources(ctx, ids)
	for id := range refs {
		out[id] = true
	}
	return out, err
}

// conflict turns a compare-and-set miss into VERSION_CONFLICT.
func conflict(err error) error {
	if errors.Is(err, postgres.ErrConflict) {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return err
}
