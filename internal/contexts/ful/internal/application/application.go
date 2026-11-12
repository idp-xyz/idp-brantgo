// Package application runs FUL's commands and queries on the command ledger. The shipment rules
// live in domain/shipment; SQL lives in adapters/postgres.
//
// FUL's upstream contexts are TRD (Context Map R27: ActiveOrderRef v1 before the transaction,
// OrderGuard v1 inside it), QLT (R26: QualityReleaseRef v1, QualityReleaseGuard v1), PUR (R59:
// supplier delivery lines and SupplierDeliveryGuard v1, through FUL's port Deliveries), PTY (R44:
// PartyScreeningGuard v1), CNX (R56: TradeComplianceGuard v1, ExportLicenseGuard v1), STL (R45:
// PaymentConditionGuard v1) and EVD (R74: SourceRef v1 for evidence). A command reads them before
// its transaction (the ledger's Preparer step); inside it, it calls their guard functions with FUL's
// own executor, through the ports below. FUL never calls another context's transaction owner. It
// publishes ShipmentFacts v1 to TRD (R70).
//
// Lock order inside FUL's transactions (ADR-BR-011; tactical lockOrder "FUL 为出运根与订单履约根";
// S5 decision D-3): the shipment its ID names FOR UPDATE; for shipment.prepare, shipment.confirm and
// the release commands then the order's fulfilment root FOR UPDATE (prepare creates it with the
// order's first shipment); for shipment.confirm and the release commands then the shipment's
// release of the scope FOR UPDATE (FUL's own roots before the authority guard); then the company
// authority guard FOR SHARE (the ledger); then, in shipment.confirm and the release commands, the
// upstream guard rows FOR SHARE in context code order (judge.go) — CNX's trade compliance check and
// export licence, PTY's screening per party in party ID order, PUR's delivery per delivery line in
// (delivery, line) order, QLT's release per release in ID order, STL's order account, TRD's order.
// shipment.prepare calls TRD's OrderGuard only. packing.plan, packing.record_actual and
// shipment.cancel lock the shipment alone: they change nothing an order's other shipments are
// judged on.
//
// S5-03b: milestone.plan and milestone.record_actual lock the plan FOR UPDATE, then the authority
// guard; they call no upstream guard (an order subject is read from TRD before the transaction).
// export_lot.register reads its lot ID, locks the shipment FOR SHARE — it changes nothing of it, and
// shipment.confirm and shipment.cancel, which lock it FOR UPDATE, commit before or after it — then
// the authority guard. ShipmentLineRef v1 (R30) and ExportLotRef v1 (R36) are lookups in a FUL
// transaction of their own, before the caller's.
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
// shipment.confirm, commercial_release.grant_exception and document_release.record: an AI or system
// actor is PERMISSION_DENIED. shipment.prepare, packing.plan, packing.record_actual and
// shipment.cancel are human or AI (SM-FUL-SHIPMENT: cancel "human|ai");
// commercial_release.evaluate is human, AI or system (SM-FUL-RELEASE). The permission of a command
// is its name (role templates merchandiser, documentation, and credit_manager, which holds
// commercial_release.grant_exception exclusively). Reads need ful.read, the audit trail ful.audit.
func Rules() []authorization.Rule {
	return append([]authorization.Rule{
		{Command: api.CommandPrepareShipment, Permission: api.CommandPrepareShipment, ActorKinds: humanOrAgent},
		{Command: api.CommandPlanPacking, Permission: api.CommandPlanPacking, ActorKinds: humanOrAgent},
		{Command: api.CommandRecordPacking, Permission: api.CommandRecordPacking, ActorKinds: humanOrAgent},
		{Command: api.CommandCancelShipment, Permission: api.CommandCancelShipment, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandConfirmShipment, api.CommandConfirmShipment),
		{Command: api.CommandEvaluateRelease, Permission: api.CommandEvaluateRelease, ActorKinds: anyActor},
		authorization.HumanOnly(api.CommandGrantReleaseException, api.CommandGrantReleaseException),
		authorization.HumanOnly(api.CommandRecordDocumentRelease, api.CommandRecordDocumentRelease),
		{Command: api.CommandGetShipment, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetFulfilment, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetRelease, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandListShipmentReleases, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandAudit, Permission: api.PermissionAudit, ActorKinds: anyActor},
		// S5-03b: milestone.plan, milestone.record_actual and export_lot.register are human or AI
		// (none is in humanOnlyCommands); an internal verification or an accepted fact is a
		// person's, which the plan's rules decide.
		{Command: api.CommandPlanMilestones, Permission: api.CommandPlanMilestones, ActorKinds: humanOrAgent},
		{Command: api.CommandRecordMilestoneActual, Permission: api.CommandRecordMilestoneActual, ActorKinds: humanOrAgent},
		{Command: api.CommandRegisterExportLot, Permission: api.CommandRegisterExportLot, ActorKinds: humanOrAgent},
		{Command: api.CommandGetMilestonePlan, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetExportLot, Permission: api.PermissionRead, ActorKinds: anyActor},
	}, documentRules()...) // S5-08: bookings and shipment documents (documents.go)
}

// Upstream are the contexts FUL reads, through its ports.
type Upstream struct {
	Orders     Orders         // TRD ActiveOrderRef v1 and OrderGuard v1 (R27)
	Releases   Releases       // QLT QualityReleaseRef v1 and QualityReleaseGuard v1 (R26)
	Deliveries Deliveries     // PUR supplier delivery lines and SupplierDeliveryGuard v1 (R59), FUL's port
	Screening  Screening      // PTY PartyScreeningGuard v1 (R44)
	Sources    evdapi.Queries // EVD SourceRef v1 (R74)
	Compliance Compliance     // CNX TradeComplianceGuard v1 and ExportLicenseGuard v1 (R56)
	Payments   Payments       // STL PaymentConditionGuard v1 (R45)
	// Documents are the upstreams of bookings and shipment documents (S5-08, document_ports.go).
	Documents DocumentUpstream
}

// Complete reports whether every upstream is wired.
func (u Upstream) Complete() bool {
	return u.Orders != nil && u.Releases != nil && u.Deliveries != nil && u.Screening != nil && u.Sources != nil && u.Compliance != nil &&
		u.Payments != nil
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

// Visible reports whether d's actor sees the object a stored result of command names: a milestone
// plan, an export lot, a shipment, or the shipment of the release a release command's result names.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	if ok, handled, err := documentVisible(ctx, exec, d, command, id); handled {
		return ok, err
	}
	switch command {
	case api.CommandPlanMilestones, api.CommandRecordMilestoneActual:
		p, err := postgres.LoadPlan(ctx, exec, scope(ctx), id, postgres.NoLock)
		return err == nil && p != nil && d.Sees(p.OwnerActor, p.OwnerTeam), err
	case api.CommandRegisterExportLot:
		l, err := postgres.LoadLot(ctx, exec, scope(ctx), id)
		return err == nil && l != nil && d.Sees(l.OwnerActor, l.OwnerTeam), err
	case api.CommandEvaluateRelease, api.CommandGrantReleaseException, api.CommandRecordDocumentRelease:
		r, err := postgres.LoadReleaseByID(ctx, exec, scope(ctx), id)
		if err != nil || r == nil {
			return false, err
		}
		id = r.ShipmentID
	}
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
	case api.EvaluateRelease:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandEvaluateRelease, key, c
	case api.GrantReleaseException:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandGrantReleaseException, key, c
	case api.RecordDocumentRelease:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRecordDocumentRelease, key, c
	case api.PlanMilestones:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandPlanMilestones, key, c
	case api.RecordMilestoneActual:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRecordMilestoneActual, key, c
	case api.RegisterExportLot:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandRegisterExportLot, key, c
	}
	return documentRequest(cmd)
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
