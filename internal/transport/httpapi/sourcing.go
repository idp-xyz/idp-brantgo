package httpapi

import (
	"context"

	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
)

// SUP and CST are what the transport needs from the sourcing and costing contexts.
type (
	SUP interface {
		supapi.Commands
		supapi.Queries
		supapi.Reconciler
		supapi.ActionReconciler
		supapi.Auditor
	}
	CST interface {
		cstapi.Commands
		cstapi.Queries
		cstapi.ScenarioCommands
		cstapi.ScenarioQueries
		cstapi.SnapshotCommands
		cstapi.SnapshotQueries
		cstapi.Reconciler
		cstapi.ActionReconciler
		cstapi.Auditor
	}
)

// supConditionKinds are SUP's supply-condition kinds with their read route, the action that seals
// and the action that ends a revision of each.
var supConditionKinds = []struct{ kind, route, seal, end string }{
	{supapi.KindSupplierOffer, "supplier_offers", "verify", "withdraw"},
	{supapi.KindPurchaseTerms, "purchase_terms", "publish", "retire"},
	{supapi.KindLogisticsOffer, "logistics_offers", "verify", "withdraw"},
}

func supCommands(c SUP) *commandSet {
	cmds := map[string]commandFunc{
		supapi.CommandSaveSupplierRfq:    reflected("SupplierRfqID", "", c.SaveSupplierRfq),
		supapi.CommandIssueSupplierRfq:   reflected("SupplierRfqID", "", c.RecordSupplierRfqIssued),
		supapi.CommandCloseSupplierRfq:   reflected("SupplierRfqID", "", c.CloseSupplierRfq),
		supapi.CommandSaveSupplierOffer:  reflected("OfferID", "", c.SaveSupplierOffer),
		supapi.CommandSavePurchaseTerms:  reflected("TermsID", "", c.SavePurchaseTerms),
		supapi.CommandSaveLogisticsOffer: reflected("OfferID", "", c.SaveLogisticsOffer),
	}
	for _, k := range supConditionKinds {
		cmds[supapi.Command(k.kind, "revise")] = reflected("ID", k.kind, c.Revise)
		cmds[supapi.Command(k.kind, "discard_draft")] = reflected("ID", k.kind, c.DiscardDraft)
		cmds[supapi.Command(k.kind, k.seal)] = reflected("ID", k.kind, c.Seal)
		cmds[supapi.Command(k.kind, k.end)] = reflected("ID", k.kind, c.End)
	}
	return &commandSet{bc: "sup", contract: supapi.Contract, auditor: c, audit: c.Audit, reconcile: hashReconciler(c.Reconcile, c.ReconcileHash),
		commands: cmds}
}

func cstCommands(c CST) *commandSet {
	// The HTTP receipt is the closed command receipt; the evaluation's hashes and totals are read
	// through GET cst/cost_evaluations/{id} (its ID is the receipt's object_id).
	evaluate := func(ctx context.Context, cmd cstapi.EvaluateScenario) (cstapi.Receipt, error) {
		r, err := c.EvaluateScenario(ctx, cmd)
		return r.Receipt, err
	}
	// The freeze answers the closed receipt too; the snapshot is read by its ID (the object_id).
	freeze := func(ctx context.Context, cmd cstapi.FreezeSnapshot) (cstapi.Receipt, error) {
		r, err := c.FreezeSnapshot(ctx, cmd)
		return r.Receipt, err
	}
	return &commandSet{bc: "cst", contract: cstapi.Contract, auditor: c, audit: c.Audit, reconcile: hashReconciler(c.Reconcile, c.ReconcileHash), commands: map[string]commandFunc{
		cstapi.CommandSaveContext:          reflected("ContextID", "", c.SaveContext),
		cstapi.CommandReviseContext:        reflected("ContextID", "", c.ReviseContext),
		cstapi.CommandDiscardContextDraft:  reflected("ContextID", "", c.DiscardContextDraft),
		cstapi.CommandVerifyContext:        reflected("ContextID", "", c.VerifyContext),
		cstapi.CommandWithdrawContext:      reflected("ContextID", "", c.WithdrawContext),
		cstapi.CommandRecordMeasurement:    reflected("MeasurementID", "", c.RecordMeasurement),
		cstapi.CommandSupersedeMeasurement: reflected("MeasurementID", "", c.SupersedeMeasurement),
		cstapi.CommandWithdrawMeasurement:  reflected("MeasurementID", "", c.WithdrawMeasurement),
		cstapi.CommandSaveScenario:         reflected("ScenarioID", "", c.SaveScenario),
		cstapi.CommandEvaluateScenario:     reflected("ScenarioID", "", evaluate),
		// Python cost_snapshot.freeze names the scenario scenario_id (the object made is the snapshot).
		cstapi.CommandFreezeSnapshot: reflected("", "", freeze),
		cstapi.CommandAttestSource:   reflected("AttestationID", "", c.AttestSource),
	}}
}
