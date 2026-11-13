// Package orders implements STL's port to TRD (application.Orders) on TRD's published contract
// (Context Map R32 TRD→STL, OrderCommercialSnapshot v1, guardLock false): TRD's
// OrderSnapshotLookups before STL's transaction. STL never locks a TRD row and never calls TRD's
// transaction owner; TRD calls STL's CreditGuard v1 instead (R57).
package orders

import (
	"context"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/application"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
)

// TRD answers STL's port from TRD's api.
type TRD struct {
	Lookups trdapi.OrderSnapshotLookups
}

var (
	_ application.Orders         = TRD{}
	_ application.OrderRevisions = TRD{}
)

// ResolveOrderIncoterms asks TRD's OrderRevisionLookups (OrderRevisionRef v1, an active grant, no lock) for the
// revisions bindings name and returns the Incoterm of each intact one whose hash is the binding's (S5R-D24); nothing
// when TRD's lookups do not answer revisions.
func (t TRD) ResolveOrderIncoterms(ctx context.Context, bindings []application.OrderBinding) (map[application.OrderBinding]string, error) {
	out := map[application.OrderBinding]string{}
	revisions, ok := t.Lookups.(trdapi.OrderRevisionLookups)
	if !ok || len(bindings) == 0 {
		return out, nil
	}
	for start := 0; start < len(bindings); start += trdapi.MaxOrderRefs {
		batch := bindings[start:min(start+trdapi.MaxOrderRefs, len(bindings))]
		keys := make([]trdapi.OrderRevisionKey, 0, len(batch))
		for _, b := range batch {
			keys = append(keys, trdapi.OrderRevisionKey{OrderID: b.OrderID, OrderRevision: b.OrderRevision})
		}
		if err := trdapi.ValidateOrderRevisionKeys(keys); err != nil {
			return nil, err
		}
		facts, err := revisions.ResolveOrderRevisions(ctx, keys)
		if err != nil {
			return nil, err
		}
		for _, b := range batch {
			f, ok := facts[trdapi.OrderRevisionKey{OrderID: b.OrderID, OrderRevision: b.OrderRevision}]
			if ok && f.Code == "" && f.Binding.OrderHash == b.OrderHash {
				out[b] = f.TradeTerms.Incoterm
			}
		}
	}
	return out, nil
}

// ResolveOrders asks TRD's OrderSnapshotLookups (an active grant, TRD's data scope) and returns
// what TRD says of each visible order at its current revision, draft or active or ended.
func (t TRD) ResolveOrders(ctx context.Context, ids []string) (map[string]application.Order, error) {
	out := map[string]application.Order{}
	if err := trdapi.ValidateOrderIDs(ids); err != nil || len(ids) == 0 {
		return out, err
	}
	facts, err := t.Lookups.ResolveOrderSnapshots(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, f := range facts {
		out[id] = orderOf(f)
	}
	return out, nil
}

// orderOf is TRD's OrderSnapshotFact as STL's port carries it.
func orderOf(f trdapi.OrderSnapshotFact) application.Order {
	o := application.Order{Binding: application.OrderBinding{OrderID: f.Binding.OrderID, OrderRevision: f.Binding.OrderRevision,
		OrderHash: f.Binding.OrderHash}, Status: f.Status, Code: f.Code, CustomerID: f.CustomerRef.PartyID,
		Currency: application.CurrencyRef{ID: f.CurrencyRef.ID, Revision: f.CurrencyRef.Revision}, CreditPortionMinor: f.CreditPortionMinor,
		Ended: f.Ended, NetMinor: f.NetMinor, Incoterm: f.TradeTerms.Incoterm}
	o.Stages = stagesOf(f.Stages)
	if a := f.AnalysisFX; a.OrderCurrency != "" {
		o.Analysis = &application.OrderAnalysis{OrderCurrency: a.OrderCurrency, Currency: a.ComparisonCurrencyRef.ID, Rate: a.Rate,
			CurrencyRevision: a.ComparisonCurrencyRef.Revision}
	}
	for _, p := range f.Proposed {
		if p.Code != "" {
			continue
		}
		o.Proposed = append(o.Proposed, application.ProposedRevision{AmendmentID: p.AmendmentID, Binding: application.OrderBinding{
			OrderID: p.Binding.OrderID, OrderRevision: p.Binding.OrderRevision, OrderHash: p.Binding.OrderHash}, Stages: stagesOf(p.Stages),
			CreditPortionMinor: p.CreditPortionMinor})
	}
	return o
}

func stagesOf(stages []trdapi.PaymentStage) []application.OrderStage {
	var out []application.OrderStage
	for _, s := range stages {
		out = append(out, application.OrderStage{StageID: s.StageID, Trigger: s.Trigger, Days: s.Days, Percent: s.Percent,
			AmountMinor: s.AmountMinor, ReleaseControl: s.ReleaseControl, OnCredit: s.OnCredit})
	}
	return out
}
