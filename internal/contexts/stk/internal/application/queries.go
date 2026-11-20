package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stk/api"
	"go.idp.xyz/idp-brant/internal/contexts/stk/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stk/internal/domain/ledger"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// GetSetting returns the company switch. A company that was never configured is disabled at
// version 0 (STK-INV-004).
func (s Service) GetSetting(ctx context.Context) (api.SettingView, error) {
	var view api.SettingView
	err := s.Ledger.Read(ctx, api.CommandGetSetting, "module", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		st, err := postgres.LoadSetting(ctx, exec, scope(ctx), postgres.NoLock)
		if err != nil || st == nil {
			return err
		}
		view = api.SettingView{Enabled: st.Enabled, RowVersion: st.RowVersion, UpdatedAt: kernel.FormatTimestamp(st.UpdatedAt)}
		return nil
	})
	return view, err
}

// GetBalance returns one balance the actor can see.
func (s Service) GetBalance(ctx context.Context, key api.BalanceKey) (api.BalanceView, error) {
	if err := key.Validate(); err != nil {
		return api.BalanceView{}, err
	}
	var view api.BalanceView
	err := s.Ledger.Read(ctx, api.CommandGetBalance, key.SkuID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		b, err := postgres.LoadBalance(ctx, exec, scope(ctx), keyOf(key.SkuID, key.LocationID, key.BatchID, key.CustodyKind), postgres.NoLock)
		if err != nil {
			return err
		}
		if b == nil || !d.Sees(b.OwnerActor, b.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/sku_id", "")
		}
		view = balanceView(b)
		return nil
	})
	return view, err
}

// GetReservation returns one hold the actor can see.
func (s Service) GetReservation(ctx context.Context, id string) (api.ReservationView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.ReservationView{}, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	var view api.ReservationView
	err := s.Ledger.Read(ctx, api.CommandGetReservation, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		r, err := postgres.LoadReservation(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if r == nil || !d.Sees(r.OwnerActor, r.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		view = api.ReservationView{ReservationID: r.ID, SkuID: r.SKU, LocationID: r.Location, BatchID: r.Batch, CustodyKind: r.Custody,
			OrderID: r.OrderID, OrderLineID: r.OrderLineID, Quantity: textDec(r.Quantity), Remaining: textDec(r.Remaining),
			Status: r.Status, UnitID: r.UnitID, UnitRevision: r.UnitRevision, RowVersion: r.RowVersion}
		return nil
	})
	return view, err
}

// ListMovements returns the movements of a balance the actor can see, oldest first.
func (s Service) ListMovements(ctx context.Context, key api.BalanceKey) ([]api.MovementView, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	var view []api.MovementView
	err := s.Ledger.Read(ctx, api.CommandListMovements, key.SkuID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		k := keyOf(key.SkuID, key.LocationID, key.BatchID, key.CustodyKind)
		b, err := postgres.LoadBalance(ctx, exec, scope(ctx), k, postgres.NoLock)
		if err != nil {
			return err
		}
		if b == nil || !d.Sees(b.OwnerActor, b.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/sku_id", "")
		}
		rows, err := postgres.ListMovements(ctx, exec, scope(ctx), k)
		if err != nil {
			return err
		}
		view = make([]api.MovementView, len(rows))
		for i, m := range rows {
			view[i] = api.MovementView{MovementID: m.ID, Kind: m.Kind, SkuID: m.Key.SKU, LocationID: m.Key.Location, BatchID: m.Key.Batch,
				CustodyKind: m.Key.Custody, Quantity: textDec(m.Quantity), UnitID: m.UnitID, UnitRevision: m.UnitRev,
				SourceKind: m.SourceKind, SourceID: m.SourceID, SourceLineID: m.SourceLine, Reason: m.Reason,
				At: kernel.FormatTimestamp(m.At)}
		}
		return nil
	})
	return view, err
}

// ResolveAvailability is the read-only half of StockAvailability v1. It does not write and it does
// not apply own/team visibility: FUL asks before shipment.confirm. Status module_disabled is the
// signal that the company has not enabled STK.
func (s Service) ResolveAvailability(ctx context.Context, q api.AvailabilityQuery) (api.Availability, error) {
	if err := q.Validate(); err != nil {
		return api.Availability{}, err
	}
	var view api.Availability
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		st, err := postgres.LoadSetting(ctx, exec, scope(ctx), postgres.NoLock)
		if err != nil {
			return err
		}
		enabled := st != nil && st.Enabled
		b, err := postgres.LoadBalance(ctx, exec, scope(ctx), keyOf(q.SkuID, q.LocationID, q.BatchID, q.CustodyKind), postgres.NoLock)
		if err != nil {
			return err
		}
		view.Enabled = enabled
		if !enabled {
			view.Status = api.AvailabilityDisabled
			return nil
		}
		if b == nil {
			view.Status = api.AvailabilityInsufficient
			view.OnHand, view.Reserved, view.Available = "0", "0", "0"
			return nil
		}
		view = balanceAvailability(b, enabled)
		if q.Quantity == "" {
			view.Status = api.AvailabilityOK
			return nil
		}
		need, err := qtyOf(q.Quantity)
		if err != nil {
			return err
		}
		if b.Available().Cmp(need) >= 0 {
			view.Status = api.AvailabilityOK
		} else {
			view.Status = api.AvailabilityInsufficient
		}
		return nil
	})
	return view, err
}

// ResolveShipmentStockCost is CostLayerRef v1 (R41). It does not lock and it does not require the
// module to be enabled: issues posted earlier stay readable.
func (s Service) ResolveShipmentStockCost(ctx context.Context, shipmentIDs []string) ([]api.ShipmentCost, error) {
	if len(shipmentIDs) > 200 {
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/shipment_ids", "")
	}
	for _, id := range shipmentIDs {
		if !kernel.ValidIdentifier(id) {
			return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/shipment_ids", "")
		}
	}
	var view []api.ShipmentCost
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		rows, err := postgres.ShipmentCosts(ctx, exec, scope(ctx), shipmentIDs)
		if err != nil {
			return err
		}
		view = make([]api.ShipmentCost, len(rows))
		for i, row := range rows {
			item := api.ShipmentCost{ShipmentID: row.ShipmentID, ShipmentLineID: row.ShipmentLineID, MovementID: row.MovementID,
				Currency: row.Currency, TotalCostMinor: row.Total, Allocations: make([]api.CostAllocation, len(row.Layers))}
			for j, layer := range row.Layers {
				item.Allocations[j] = api.CostAllocation{LayerID: layer.LayerID, Quantity: textDec(layer.Quantity), AmountMinor: layer.AmountMinor, Currency: layer.Currency}
			}
			view[i] = item
		}
		return nil
	})
	return view, err
}

func balanceView(b *ledger.Balance) api.BalanceView {
	return api.BalanceView{SkuID: b.SKU, LocationID: b.Location, BatchID: b.Batch, CustodyKind: b.Custody,
		OnHand: textDec(b.OnHand), Reserved: textDec(b.Reserved), Available: textDec(b.Available()),
		UnitID: b.UnitID, UnitRevision: b.UnitRevision, Currency: b.Currency, MinorUnits: b.MinorUnits, RowVersion: b.RowVersion}
}

func balanceAvailability(b *ledger.Balance, enabled bool) api.Availability {
	return api.Availability{Enabled: enabled, OnHand: textDec(b.OnHand), Reserved: textDec(b.Reserved), Available: textDec(b.Available()),
		UnitID: b.UnitID, UnitRevision: b.UnitRevision, Currency: b.Currency, MinorUnits: b.MinorUnits}
}
