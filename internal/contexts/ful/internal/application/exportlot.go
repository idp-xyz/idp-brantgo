package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/exportlot"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// lotCommand runs export_lot.register (FUL-AGG-ExportLot; UL FUL-ExportLot; CN-AT02). Inside its
// transaction it reads the lot its ID names (a taken ID is answered from it or refused), then locks
// its shipment FOR SHARE — never FOR UPDATE: the lot changes nothing of the shipment — and reads the
// shipment's lines, their prices and the lots that hold them. shipment.confirm and shipment.cancel
// lock the shipment FOR UPDATE, so a registration and a confirmation or cancellation of the same
// shipment commit one after the other: a lot never records a state its shipment has left. Two lots
// that take the same shipment line meet on its unique key; the second is refused when the ledger
// runs it again.
type lotCommand struct {
	s        Service
	cmd      api.RegisterExportLot
	req      exportlot.Request
	existing *exportlot.Lot
	ship     *shipment.Shipment
	prices   map[string]shipment.Price
	taken    map[string]string
	replay   bool
}

func (h *lotCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.ship, h.prices, h.taken, h.replay = nil, nil, nil, nil, false
	sc := scope(ctx)
	l, err := postgres.LoadLot(ctx, exec, sc, h.req.ID)
	if err != nil || l != nil {
		h.existing = l
		return err
	}
	if h.ship, err = postgres.LoadShipment(ctx, exec, sc, h.req.ShipmentID, postgres.ForShare); err != nil || h.ship == nil {
		return err
	}
	if h.prices, err = postgres.LoadPrices(ctx, exec, sc, h.ship.ID); err != nil {
		return err
	}
	h.taken, err = postgres.TakenLines(ctx, exec, sc, h.ship.ID)
	return err
}

func (h *lotCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if l := h.existing; l != nil {
		if !d.Sees(l.OwnerActor, l.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/export_lot_id", "")
		}
		if err := l.Answer(h.req.RequestHash, d.Actor); err != nil {
			return action.Outcome{}, err
		}
		h.replay = true
		return action.Outcome{ObjectID: l.ID, RowVersion: l.RowVersion, Status: l.Status}, nil
	}
	ship := h.ship
	if ship != nil && !d.Sees(ship.OwnerActor, ship.OwnerTeam) {
		ship = nil
	}
	l, err := exportlot.Register(h.req, exportlot.Inputs{Shipment: ship, Prices: h.prices, Taken: h.taken,
		Act: exportlot.Act{By: d.Actor, Epoch: d.Epoch, At: h.s.Clock.Now()}, Team: d.Team})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertLot(ctx, exec, scope(ctx), l); err != nil {
		return action.Outcome{}, err
	}
	lines := make([]map[string]any, 0, len(l.Lines))
	for _, x := range l.Lines {
		lines = append(lines, map[string]any{"line_id": x.ID, "shipment_line_id": x.ShipmentLineID, "order_line_id": x.OrderLineID,
			"quantity": postgres.Text(x.Quantity), "unit_id": x.UnitID, "unit_revision": x.UnitRevision, "net_minor": postgres.MinorText(x.Amounts.Net)})
	}
	event := action.Event{Type: api.EventExportLotRegistered, AggregateType: "ExportLot", AggregateID: l.ID, AggregateVersion: l.RowVersion,
		Fields: map[string]any{"export_lot_id": l.ID, "shipment_id": l.ShipmentID, "shipment_status": l.ShipmentStatus,
			"shipment_version": l.ShipmentVersion, "order_id": l.Order.OrderID, "order_revision": l.Order.OrderRevision, "order_hash": l.Order.OrderHash,
			"currency_id": l.CurrencyID, "currency_revision": l.CurrencyRevision, "lines": lines, "net_minor": postgres.MinorText(l.Totals.Net),
			"registered_at": kernel.FormatTimestamp(l.Registered.At)}}
	return action.Outcome{ObjectID: l.ID, RowVersion: l.RowVersion, Status: l.Status, Events: []action.Event{event}}, nil
}

// RegisterExportLot registers an export lot of a shipment's lines (export_lot.register; UL
// FUL-ExportLot, CN-05, CN-AT02). It publishes ExportLotRegistered.
func (s Service) RegisterExportLot(ctx context.Context, cmd api.RegisterExportLot) (api.Receipt, error) {
	h := &lotCommand{s: s, cmd: cmd}
	err := cmd.Validate()
	if err == nil {
		h.req = exportlot.Request{ID: cmd.ExportLotID, ShipmentID: cmd.ShipmentID, RequestHash: requestHash(cmd)}
		for _, l := range cmd.Lines {
			q, _ := api.Quantity(l.Quantity)
			h.req.Lines = append(h.req.Lines, exportlot.RequestLine{ID: l.LineID, ShipmentLineID: l.ShipmentLineID, Quantity: q})
		}
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandRegisterExportLot, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ExportLotID}, cmd,
		err, h)
	res.Replayed = res.Replayed || (err == nil && h.replay)
	return res, err
}

// GetExportLot returns a lot the caller sees, audited as export_lot.get (ful.read), with how it
// stands against its shipment now: NOT_FOUND otherwise.
func (s Service) GetExportLot(ctx context.Context, id string) (api.ExportLotView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.ExportLotView{}, invalidID(s, ctx, api.CommandGetExportLot)
	}
	var v api.ExportLotView
	err := s.Ledger.Read(ctx, api.CommandGetExportLot, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		l, err := postgres.LoadLot(ctx, exec, scope(ctx), id)
		if err != nil {
			return err
		}
		if l == nil || !d.Sees(l.OwnerActor, l.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		sh, err := postgres.LoadShipment(ctx, exec, scope(ctx), l.ShipmentID, postgres.NoLock)
		if err != nil {
			return err
		}
		v = lotRef(l, sh)
		return nil
	})
	return v, err
}

func amounts(a shipment.Amounts) api.Amounts {
	return api.Amounts{NetMinor: postgres.MinorText(a.Net), TaxMinor: postgres.MinorText(a.Tax), PayableMinor: postgres.MinorText(a.Payable)}
}

// lotRef is ExportLotRef v1 of lot l, whose shipment is sh as it is now.
func lotRef(l *exportlot.Lot, sh *shipment.Shipment) api.ExportLotRef {
	st := l.Against(sh)
	v := api.ExportLotRef{ExportLotID: l.ID, Status: l.Status, RowVersion: l.RowVersion, ShipmentID: l.ShipmentID, ShipmentStatus: sh.Status,
		ShipmentVersion: sh.RowVersion, ConfirmedAt: st.ConfirmedAt, ShipmentStatusAtRegistration: l.ShipmentStatus,
		ShipmentVersionAtRegistration: l.ShipmentVersion, Order: api.OrderBinding{OrderID: l.Order.OrderID, OrderRevision: l.Order.OrderRevision,
			OrderHash: l.Order.OrderHash}, CustomerID: l.CustomerID, Currency: api.CurrencyRef{ID: l.CurrencyID, Revision: l.CurrencyRevision},
		Lines: []api.ExportLotRefLine{}, Totals: amounts(l.Totals), Reconciliation: st.State, Differences: []api.ExportLotDifference{},
		RegisteredBy: l.Registered.By, RegisteredAt: l.Registered.At, AuthorityEpoch: l.Registered.Epoch}
	for _, x := range l.Lines {
		line := api.ExportLotRefLine{LineID: x.ID, ShipmentLineID: x.ShipmentLineID, OrderLineID: x.OrderLineID, SkuID: x.SkuID,
			SpecRevision: x.SpecRevision, UnitID: x.UnitID, UnitRevision: x.UnitRevision, Quantity: postgres.Text(x.Quantity),
			ShipmentLineQuantity: postgres.Text(x.ShipmentLineQuantity), UnitPrice: postgres.Text(x.UnitPrice), Amounts: amounts(x.Amounts)}
		if c, ok := st.Confirmed[x.ID]; ok {
			line.ConfirmedQuantity = postgres.Text(c)
		}
		v.Lines = append(v.Lines, line)
	}
	for _, d := range st.Differences {
		v.Differences = append(v.Differences, api.ExportLotDifference{LineID: d.LineID, ShipmentLineID: d.ShipmentLineID,
			LotQuantity: postgres.Text(d.Lot), ConfirmedQuantity: postgres.Text(d.Confirmed)})
	}
	return v
}
