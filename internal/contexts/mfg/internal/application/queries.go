package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/api"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/domain/plan"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/domain/subcontract"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// GetSetting returns the company switch. A company that was never configured is disabled at version 0.
func (s Service) GetSetting(ctx context.Context) (api.SettingView, error) {
	var view api.SettingView
	err := s.Ledger.Read(ctx, api.CommandGetSetting, "module", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		st, err := postgres.LoadSetting(ctx, exec, scopeOf(ctx), postgres.NoLock)
		if err != nil || st == nil {
			return err
		}
		view = api.SettingView{Enabled: st.Enabled, RowVersion: st.RowVersion, UpdatedAt: kernel.FormatTimestamp(st.UpdatedAt)}
		return nil
	})
	return view, err
}

// GetPlan returns one plan the actor can see.
func (s Service) GetPlan(ctx context.Context, planID string) (api.PlanView, error) {
	if !kernel.ValidIdentifier(planID) {
		return api.PlanView{}, kernel.Reject(kernel.CodeSchemaInvalid, "/plan_id", "")
	}
	var view api.PlanView
	err := s.Ledger.Read(ctx, api.CommandGetPlan, planID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		p, err := postgres.LoadPlan(ctx, exec, scopeOf(ctx), planID, postgres.NoLock)
		if err != nil {
			return err
		}
		if p == nil || !d.Sees(p.OwnerActor, p.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/plan_id", "")
		}
		view = planView(p)
		return nil
	})
	return view, err
}

// GetSubcontract returns one subcontract the actor can see.
func (s Service) GetSubcontract(ctx context.Context, id string) (api.SubcontractView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.SubcontractView{}, kernel.Reject(kernel.CodeSchemaInvalid, "/subcontract_id", "")
	}
	var view api.SubcontractView
	err := s.Ledger.Read(ctx, api.CommandGetSubcontract, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		row, err := postgres.LoadSubcontract(ctx, exec, scopeOf(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if row == nil || !d.Sees(row.Order.OwnerActor, row.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/subcontract_id", "")
		}
		view = subcontractView(row)
		return nil
	})
	return view, err
}

// ResolveInspectionSubject answers InspectionSubject v1 for kind mfg_subcontract_operation (R25).
// A missing subcontract is NOT_FOUND. A cancelled subcontract is returned with Code INVALID_STATE:
// the subject does not serve. A progress claim is not itself this answer.
func (s Service) ResolveInspectionSubject(ctx context.Context, subcontractID, operationID string) (api.InspectionSubject, error) {
	if !kernel.ValidIdentifier(subcontractID) || !kernel.ValidIdentifier(operationID) {
		return api.InspectionSubject{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/subject", "")
	}
	var view api.InspectionSubject
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		on, err := settingOn(ctx, exec)
		if err != nil {
			return err
		}
		if !on {
			return api.Reject(api.CodeDependencyNotSupported, "/args/subject", "Manufacturing is not enabled")
		}
		row, err := postgres.LoadSubcontract(ctx, exec, scopeOf(ctx), subcontractID, postgres.NoLock)
		if err != nil {
			return err
		}
		if row == nil || row.Order.OperationID != operationID || !d.Sees(row.Order.OwnerActor, row.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/args/subject", "")
		}
		view = api.InspectionSubject{SubcontractID: row.Order.ID, OperationID: row.Order.OperationID, PlanID: row.Order.PlanID,
			FactoryID: row.FactoryID, Status: row.Order.Status, Revision: row.Order.Version, Hash: row.SubjectHash,
			OrderID: row.OrderID, OrderRevision: row.OrderRevision, OrderHash: row.OrderHash, OrderLineID: row.OrderLineID,
			SkuID: row.SkuID, SpecRevision: row.SpecRevision, UnitID: row.Order.UnitID, UnitRevision: row.UnitRevision,
			Quantity: postgres.Text(row.Order.Quantity)}
		if row.Order.Status == subcontract.StatusCancelled {
			view.Code = kernel.CodeInvalidState
		}
		return nil
	})
	return view, err
}

func settingOn(ctx context.Context, exec bentopg.Executor) (bool, error) {
	st, err := postgres.LoadSetting(ctx, exec, scopeOf(ctx), postgres.NoLock)
	if err != nil || st == nil {
		return false, err
	}
	return st.Enabled, nil
}

func planView(p *plan.Plan) api.PlanView {
	out := api.PlanView{PlanID: p.ID, Status: p.Status, RowVersion: p.Version, FactoryPartyID: p.FactoryID, Note: p.Note}
	for _, op := range p.Operations {
		out.Operations = append(out.Operations, api.OperationView{OperationID: op.ID, Name: op.Name, Sequence: op.Sequence,
			SkuID: op.SkuID, SpecRevision: op.SpecRevision, Quantity: postgres.Text(op.Quantity), UnitID: op.UnitID, UnitRevision: op.UnitRevision})
	}
	for _, m := range p.Materials {
		out.Materials = append(out.Materials, api.MaterialView{MaterialID: m.ID, Kind: m.Kind, SkuID: m.SkuID,
			Quantity: postgres.Text(m.Quantity), UnitID: m.UnitID, UnitRevision: m.UnitRevision, LossBasis: m.LossBasis})
	}
	if out.Operations == nil {
		out.Operations = []api.OperationView{}
	}
	if out.Materials == nil {
		out.Materials = []api.MaterialView{}
	}
	return out
}

func subcontractView(row *postgres.Subcontract) api.SubcontractView {
	return api.SubcontractView{SubcontractID: row.Order.ID, Status: row.Order.Status, RowVersion: row.Order.Version,
		PlanID: row.Order.PlanID, OperationID: row.Order.OperationID, Quantity: postgres.Text(row.Order.Quantity),
		UnitID: row.Order.UnitID, UnitRevision: row.UnitRevision, Currency: row.Currency, MinorUnits: row.MinorUnits,
		ProcessingFeeMinor: row.FeeMinor, CommitmentID: row.CommitmentID, OrderID: row.OrderID, OrderLineID: row.OrderLineID}
}
