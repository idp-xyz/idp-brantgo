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

var _ application.Orders = TRD{}

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
		Currency: application.CurrencyRef{ID: f.CurrencyRef.ID, Revision: f.CurrencyRef.Revision}, CreditPortionMinor: f.CreditPortionMinor}
	for _, s := range f.Stages {
		o.Stages = append(o.Stages, application.OrderStage{StageID: s.StageID, Trigger: s.Trigger, Days: s.Days, Percent: s.Percent,
			AmountMinor: s.AmountMinor, ReleaseControl: s.ReleaseControl, OnCredit: s.OnCredit})
	}
	return o
}
