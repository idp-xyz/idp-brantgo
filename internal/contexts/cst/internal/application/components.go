package application

import (
	"context"
	"encoding/json"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/scenario"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// boundKey names an input an evaluation bound: its context, kind, ID and revision.
type boundKey struct {
	context, kind, id string
	revision          int64
}

// frozenResult is a snapshot the caller sees, with its evaluation's result and bound inputs.
type frozenResult struct {
	ref    api.CostSnapshotRef
	result api.EvaluationResult
	bound  []scenario.BoundInput
}

// ResolveSnapshotComponents answers ComponentLookups (Context Map R78, CostSnapshotRef v1 by cost
// category; S5 decision D-13): for each snapshot the caller sees, its payable components with
// their categories. CST reads its snapshots and evaluations in a read transaction of its own, then
// asks SUP for the logistics offer revisions the charges came from and POL for their fees'
// fee_category revisions — each checked against the content hash the evaluation bound. A charge
// whose offer, fee or fee category CST cannot read now, or whose revision's content hash is not the
// bound one, has no category and is listed as unresolved.
func (s Service) ResolveSnapshotComponents(ctx context.Context, refs []api.CostSnapshotRef) (map[api.CostSnapshotRef]api.SnapshotComponents, error) {
	if len(refs) > api.MaxSnapshotLookup {
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/refs", "")
	}
	for i, r := range refs {
		if err := r.Validate(index("/refs", i)); err != nil {
			return nil, err
		}
	}
	var frozen []frozenResult
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		for _, r := range refs {
			snap, err := postgres.LoadSnapshot(ctx, exec, sc, r.SnapshotID, postgres.NoLock)
			if err != nil {
				return err
			}
			if snap == nil || !d.Sees(snap.OwnerActor, snap.OwnerTeam) || (r.ResultHash != "" && r.ResultHash != snap.ResultHash) {
				continue
			}
			e, err := postgres.LoadEvaluation(ctx, exec, sc, snap.EvaluationID)
			if err != nil {
				return err
			}
			f := frozenResult{ref: r, bound: e.Inputs}
			if err = json.Unmarshal(e.Document, &f.result); err != nil {
				return err
			}
			frozen = append(frozen, f)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	now := s.Clock.Now()
	var offers []scenario.Ref
	for _, f := range frozen {
		for _, c := range f.result.Components {
			if c.Kind == api.ComponentCharge && c.Source.Kind == scenario.KindLogisticsOffer {
				offers = append(offers, scenario.Ref{Kind: c.Source.Kind, ID: c.Source.ID, Revision: c.Source.Revision})
			}
		}
	}
	supply, err := s.resolveSupply(ctx, offers, now)
	if err != nil {
		return nil, err
	}
	var feeTypes []scenario.Ref
	for _, sp := range supply {
		if sp.Logistics == nil {
			continue
		}
		for _, fee := range sp.Logistics.Fees {
			if fee.FeeType != nil {
				feeTypes = append(feeTypes, *fee.FeeType)
			}
		}
	}
	configs, err := s.resolveConfigs(ctx, feeTypes, now)
	if err != nil {
		return nil, err
	}
	out := make(map[api.CostSnapshotRef]api.SnapshotComponents, len(frozen))
	for _, f := range frozen {
		out[f.ref] = categorize(f.ref, f.result, f.bound, supply, configs)
	}
	return out, nil
}

// categorize gives every payable component of result its cost category: goods for a purchase; for
// a charge from a logistics offer the category of its fee's POL fee_category, when SUP's offer
// revision and POL's fee category revision are what the evaluation bound (same content hash);
// anything else stays unresolved.
func categorize(ref api.CostSnapshotRef, result api.EvaluationResult, bound []scenario.BoundInput, supply map[scenario.Ref]scenario.Supply,
	configs map[scenario.Ref]scenario.Config) api.SnapshotComponents {
	hashes := map[boundKey]string{}
	for _, b := range bound {
		hashes[boundKey{b.Context, b.Kind, b.ID, b.Revision}] = b.ContentHash
	}
	out := api.SnapshotComponents{Ref: ref, Currency: result.Currency, MinorUnits: result.MinorUnits, TotalMinor: result.TotalMinor,
		Components: []api.CategorizedComponent{}, Unresolved: []string{}}
	for _, c := range result.Components {
		cc := api.CategorizedComponent{ComponentID: c.ComponentID, Kind: c.Kind, Source: api.SupplyRef{Kind: c.Source.Kind, ID: c.Source.ID,
			Revision: c.Source.Revision}, SourceLineID: c.SourceLineID, Currency: c.Currency, CostMinor: c.CostMinor, AmountMinor: c.AmountMinor}
		switch c.Kind {
		case api.ComponentPurchase:
			cc.Category = api.CategoryGoods
		case api.ComponentCharge:
			if ft, category, ok := feeCategory(c, hashes, supply, configs); ok {
				r := api.ConfigRef{Kind: ft.Kind, ID: ft.ID, Revision: ft.Revision}
				cc.FeeCategory, cc.Category = &r, category
			}
		}
		if cc.Category == "" {
			out.Unresolved = append(out.Unresolved, c.ComponentID)
		}
		out.Components = append(out.Components, cc)
	}
	slices.Sort(out.Unresolved)
	return out
}

// feeCategory returns the fee_category revision and category of charge c, read from the logistics
// offer revision and fee category revision the evaluation bound.
func feeCategory(c api.CostComponent, hashes map[boundKey]string, supply map[scenario.Ref]scenario.Supply,
	configs map[scenario.Ref]scenario.Config) (scenario.Ref, string, bool) {
	offer := scenario.Ref{Kind: c.Source.Kind, ID: c.Source.ID, Revision: c.Source.Revision}
	sp, ok := supply[offer]
	if !ok || sp.Logistics == nil || sp.ContentHash == "" || hashes[boundKey{api.InputFromSUP, offer.Kind, offer.ID, offer.Revision}] != sp.ContentHash {
		return scenario.Ref{}, "", false
	}
	for _, fee := range sp.Logistics.Fees {
		if fee.ID != c.SourceLineID || fee.FeeType == nil {
			continue
		}
		ft := *fee.FeeType
		cfg, ok := configs[ft]
		if !ok || cfg.FeeCategory == nil || cfg.FeeCategory.Category == "" || cfg.ContentHash == "" ||
			hashes[boundKey{api.InputFromPOL, ft.Kind, ft.ID, ft.Revision}] != cfg.ContentHash {
			return scenario.Ref{}, "", false
		}
		return ft, cfg.FeeCategory.Category, true
	}
	return scenario.Ref{}, "", false
}
