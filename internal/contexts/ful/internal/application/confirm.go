package application

import (
	"context"
	"errors"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// confirmCommand runs shipment.confirm (human only). Before the transaction it asks EVD about the
// evidence. Inside it, it locks the shipment FOR UPDATE and then the order's fulfilment root FOR
// UPDATE, reads what the order's confirmed shipments consumed, and — after the ledger's authority
// guard — re-checks every guard in context code order with FUL's executor, each keeping its rows
// FOR SHARE until FUL commits: the extension points CNX trade compliance and export licence (not
// wired until S5 unit C1), PTY's PartyScreeningGuard on every party (R44), PUR's
// SupplierDeliveryGuard on every delivery line (R59, FUL-INV-003), QLT's QualityReleaseGuard on
// every release (R26), the extension point STL payment condition, TRD's OrderGuard (R27, D-1: the
// order row TRD's order end and amendment lock FOR UPDATE), FUL's commercial release extension
// point; then FUL-INV-001 on every order line. The confirmation records each guard's answer as
// evidence with its SHA-256 and publishes ShipmentConfirmed.
type confirmCommand struct {
	s        Service
	cmd      api.ConfirmShipment
	sources  map[string]bool
	existing *shipment.Shipment
	consumed shipment.Consumed
}

func (h *confirmCommand) Prepare(ctx context.Context) error {
	var err error
	h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *confirmCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.consumed = nil, shipment.Consumed{}
	sc := scope(ctx)
	s, err := postgres.LoadShipment(ctx, exec, sc, h.cmd.ShipmentID, postgres.ForUpdate)
	if err != nil || s == nil || s.Status != shipment.StatusPrepared {
		h.existing = s
		return err
	}
	if err = postgres.LockFulfilment(ctx, exec, sc, s.Order.OrderID); err != nil {
		return err
	}
	var refs []shipment.DeliveryRef
	var ids []string
	for _, u := range s.Deliveries() {
		refs = append(refs, u.Ref)
	}
	for _, u := range s.Releases() {
		ids = append(ids, u.ID)
	}
	h.consumed, err = postgres.LoadConsumed(ctx, exec, sc, s.Order.OrderID, refs, ids)
	h.existing = s
	return err
}

func (h *confirmCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	s := h.existing
	if s == nil || !d.Sees(s.OwnerActor, s.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/shipment_id", "")
	}
	if err := s.Confirmable(h.cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	if err := shipment.Readable(h.cmd.EvidenceIDs, h.sources, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	entries := []shipment.Entry{{Name: "guards_checked_at", Value: kernel.FormatTimestamp(now)}}
	var unchecked []string
	ext := h.s.Upstream.Extensions
	call := ExtensionCall{ShipmentID: s.ID, Order: binding(s.Order), At: now}
	extension := func(name string, guard Extension) error {
		if guard == nil {
			entries, unchecked = append(entries, shipment.Entry{Name: name, Value: shipment.NotWired}), append(unchecked, name)
			return nil
		}
		v, err := guard(ctx, exec, call)
		entries = append(entries, shipment.Entry{Name: name, Value: v})
		return err
	}
	if err := extension(shipment.GuardTradeCompliance, ext.TradeCompliance); err != nil {
		return action.Outcome{}, err
	}
	if err := extension(shipment.GuardExportLicense, ext.ExportLicense); err != nil {
		return action.Outcome{}, err
	}
	for _, id := range s.ScreenedParties() {
		g, err := h.s.Upstream.Screening.GuardScreening(ctx, exec, id, now)
		if err != nil {
			return action.Outcome{}, err
		}
		e, err := s.CheckScreening(id, shipment.ScreeningVerdict{Clear: g.Clear, Status: g.Status, Reason: g.Reason, ScreeningID: g.ScreeningID,
			PartyRevision: g.PartyRevision})
		entries = append(entries, e)
		if err != nil {
			return action.Outcome{}, err
		}
	}
	for _, u := range s.Deliveries() {
		l, err := h.s.Upstream.Deliveries.GuardDeliveryLine(ctx, exec, api.DeliveryLineRef{DeliveryID: u.Ref.DeliveryID, LineID: u.Ref.LineID,
			Version: u.Version})
		if errors.Is(err, ErrDeliveriesNotWired) {
			return action.Outcome{}, deliveriesNotWired(shipment.StoredLinePath(u.Line, "supplier_delivery"))
		}
		if err != nil {
			return action.Outcome{}, err
		}
		e, err := s.CheckDelivery(u, deliveryVerdict(l), h.consumed.Deliveries[u.Ref])
		entries = append(entries, e)
		if err != nil {
			return action.Outcome{}, err
		}
	}
	for _, u := range s.Releases() {
		v, err := h.s.Upstream.Releases.GuardRelease(ctx, exec, releaseRef(u.ID), now)
		if err != nil {
			return action.Outcome{}, err
		}
		e, err := s.CheckRelease(u, releaseVerdict(v), h.consumed.Releases[u.ID])
		entries = append(entries, e)
		if err != nil {
			return action.Outcome{}, err
		}
	}
	if err := extension(shipment.GuardPaymentCondition, ext.PaymentCondition); err != nil {
		return action.Outcome{}, err
	}
	v, err := h.s.Upstream.Orders.GuardOrder(ctx, exec, binding(s.Order))
	if err != nil {
		return action.Outcome{}, err
	}
	e, err := shipment.CheckOrder(s.Order, orderVerdict(v), "/args/shipment_id")
	entries = append(entries, e)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = extension(shipment.GuardCommercialRelease, ext.CommercialRelease); err != nil {
		return action.Outcome{}, err
	}
	if err = s.CheckOrderLines(h.consumed.OrderLines); err != nil {
		return action.Outcome{}, err
	}
	from := s.RowVersion
	if err = s.Confirm(slices.Clone(h.cmd.EvidenceIDs), entries, unchecked, shipment.Act{By: d.Actor, Epoch: d.Epoch, At: now}); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.ConfirmShipment(ctx, exec, scope(ctx), s, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	lines := make([]map[string]any, 0, len(s.Lines))
	for _, l := range s.Lines {
		lines = append(lines, map[string]any{"line_id": l.ID, "order_line_id": l.OrderLineID, "quantity": postgres.Text(l.Quantity), "unit_id": l.UnitID,
			"unit_revision": l.UnitRevision, "delivery_id": l.Delivery.DeliveryID, "delivery_line_id": l.Delivery.LineID,
			"delivery_version": l.DeliveryVersion, "quality_release_id": l.ReleaseID})
	}
	c := s.Confirmation
	event := action.Event{Type: api.EventShipmentConfirmed, AggregateType: "Shipment", AggregateID: s.ID, AggregateVersion: s.RowVersion,
		Fields: map[string]any{"shipment_id": s.ID, "order_id": s.Order.OrderID, "order_revision": s.Order.OrderRevision, "order_hash": s.Order.OrderHash,
			"route": s.Route, "lines": lines, "guards_sha256": c.EntriesSHA256, "unchecked_guards": c.Unchecked,
			"confirmed_at": kernel.FormatTimestamp(c.Confirmed.At)}}
	return action.Outcome{ObjectID: s.ID, RowVersion: s.RowVersion, Status: s.Status, Events: []action.Event{event}}, nil
}

// ConfirmShipment confirms a prepared shipment (shipment.confirm, human only; SM-FUL-SHIPMENT,
// FUL-INV-001, FUL-INV-003, R26, R27, R44, R59, D-1, D-11).
func (s Service) ConfirmShipment(ctx context.Context, cmd api.ConfirmShipment) (api.ShipmentReceipt, error) {
	h := &confirmCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandConfirmShipment, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ShipmentID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.ShipmentReceipt{}, err
	}
	return s.shipmentReceipt(ctx, res)
}
