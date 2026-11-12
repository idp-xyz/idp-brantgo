package application

import (
	"context"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// prepareCommand runs shipment.prepare. Before the transaction it reads the order from TRD, the
// releases from QLT and the delivery lines from PUR; inside it, it locks the shipment its ID names,
// or else opens and locks the order's fulfilment root, reads what the order's confirmed shipments
// consumed, decides, re-checks the order with OrderGuard v1 and writes the shipment. Nothing is
// reserved: shipment.confirm decides again.
type prepareCommand struct {
	s          Service
	cmd        api.PrepareShipment
	req        shipment.Request
	order      *shipment.OrderFact
	releases   map[string]shipment.ReleaseFact
	deliveries map[shipment.DeliveryRef]shipment.DeliveryFact
	existing   *shipment.Shipment
	consumed   shipment.Consumed
	replay     bool
}

func (h *prepareCommand) Prepare(ctx context.Context) error {
	var taken bool
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		s, err := postgres.LoadShipment(ctx, exec, scope(ctx), h.req.ID, postgres.NoLock)
		taken = s != nil
		return err
	})
	if err != nil || taken || h.req.Route != shipment.RouteSupplierDirect {
		return err
	}
	orders, err := h.s.Upstream.Orders.ResolveActiveOrders(ctx, []string{h.req.OrderID})
	if err != nil {
		return err
	}
	if f, ok := orders[h.req.OrderID]; ok {
		h.order = orderFact(f)
	}
	var ids []string
	var refs []api.DeliveryLineRef
	for _, l := range h.cmd.Lines {
		if !slices.Contains(ids, l.QualityReleaseID) {
			ids = append(ids, l.QualityReleaseID)
		}
		if !slices.Contains(refs, l.SupplierDelivery) {
			refs = append(refs, l.SupplierDelivery)
		}
	}
	releases, err := h.s.Upstream.Releases.ResolveReleases(ctx, ids, h.s.Clock.Now())
	if err != nil {
		return err
	}
	h.releases = map[string]shipment.ReleaseFact{}
	for _, r := range releases {
		h.releases[r.Ref.ReleaseID] = releaseFact(r)
	}
	lines, err := h.s.Upstream.Deliveries.ResolveDeliveryLines(ctx, refs)
	if err != nil {
		return err
	}
	// A delivery line keeps the version its first shipment line names; the domain refuses another.
	h.deliveries = map[shipment.DeliveryRef]shipment.DeliveryFact{}
	for _, l := range h.cmd.Lines {
		ref := shipment.DeliveryRef{DeliveryID: l.SupplierDelivery.DeliveryID, LineID: l.SupplierDelivery.LineID}
		if _, seen := h.deliveries[ref]; seen {
			continue
		}
		if got, ok := lines[l.SupplierDelivery]; ok {
			h.deliveries[ref] = deliveryFact(got)
		}
	}
	return nil
}

func (h *prepareCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.consumed, h.replay = nil, shipment.Consumed{}, false
	sc := scope(ctx)
	s, err := postgres.LoadShipment(ctx, exec, sc, h.req.ID, postgres.ForUpdate)
	if err != nil || s != nil {
		h.existing = s
		return err
	}
	p, err := identity.FromContext(ctx)
	if err != nil {
		return err
	}
	if err = postgres.OpenFulfilment(ctx, exec, sc, h.req.OrderID, p.Actor, h.s.Clock.Now()); err != nil {
		return err
	}
	var refs []shipment.DeliveryRef
	var ids []string
	for _, l := range h.req.Lines {
		refs, ids = append(refs, l.Delivery), append(ids, l.ReleaseID)
	}
	h.consumed, err = postgres.LoadConsumed(ctx, exec, sc, h.req.OrderID, refs, ids)
	return err
}

func (h *prepareCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if s := h.existing; s != nil {
		if !d.Sees(s.OwnerActor, s.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/shipment_id", "")
		}
		if err := s.Answer(h.req.RequestHash, d.Actor); err != nil {
			return action.Outcome{}, err
		}
		h.replay = true
		return action.Outcome{ObjectID: s.ID, RowVersion: s.RowVersion, Status: s.Status}, nil
	}
	s, err := shipment.Prepare(h.req, shipment.Inputs{Order: h.order, Releases: h.releases, Deliveries: h.deliveries, Consumed: h.consumed,
		Preparer: d.Actor, Epoch: d.Epoch, Team: d.Team, Now: h.s.Clock.Now()})
	if err != nil {
		return action.Outcome{}, err
	}
	v, err := h.s.Upstream.Orders.GuardOrder(ctx, exec, binding(s.Order))
	if err != nil {
		return action.Outcome{}, err
	}
	if _, err = shipment.CheckOrder(s.Order, orderVerdict(v), "/args/order_id"); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertShipment(ctx, exec, scope(ctx), s); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: s.ID, RowVersion: s.RowVersion, Status: s.Status}, nil
}

// PrepareShipment prepares a shipment of an active order (shipment.prepare; SM-FUL-SHIPMENT, R27,
// R26, R59, D-10, BT11).
func (s Service) PrepareShipment(ctx context.Context, cmd api.PrepareShipment) (api.ShipmentReceipt, error) {
	h := &prepareCommand{s: s, cmd: cmd}
	err := cmd.Validate()
	if err == nil {
		h.req = shipment.Request{ID: cmd.ShipmentID, OrderID: cmd.OrderID, Route: cmd.Route, RequestHash: requestHash(cmd)}
		for _, l := range cmd.Lines {
			q, _ := api.Quantity(l.Quantity)
			h.req.Lines = append(h.req.Lines, shipment.RequestLine{ID: l.LineID, OrderLineID: l.OrderLineID, Quantity: q,
				Delivery: shipment.DeliveryRef{DeliveryID: l.SupplierDelivery.DeliveryID, LineID: l.SupplierDelivery.LineID},
				Version:  l.SupplierDelivery.Version, ReleaseID: l.QualityReleaseID})
		}
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandPrepareShipment, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ShipmentID}, cmd, err, h)
	if err != nil {
		return api.ShipmentReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return s.shipmentReceipt(ctx, res)
}

// packCommand runs packing.plan and packing.record_actual: it locks the shipment and records the
// packing (UL FUL-ActualPackage; BT11 "箱至订单数量一致").
type packCommand struct {
	s        Service
	kind     string
	id       string
	expected int64
	packages []shipment.Package
	evidence []string
	sources  map[string]bool
	existing *shipment.Shipment
}

func (h *packCommand) Prepare(ctx context.Context) error {
	var err error
	h.sources, err = h.s.sources(ctx, h.evidence)
	return err
}

func (h *packCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.existing, err = postgres.LoadShipment(ctx, exec, scope(ctx), h.id, postgres.ForUpdate)
	return err
}

func (h *packCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	s := h.existing
	if s == nil || !d.Sees(s.OwnerActor, s.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/shipment_id", "")
	}
	from := s.RowVersion
	rec, err := s.Pack(h.kind, h.expected, h.packages, h.evidence, h.sources, shipment.Act{By: d.Actor, Epoch: d.Epoch, At: h.s.Clock.Now()})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertPacking(ctx, exec, scope(ctx), s, rec, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	return action.Outcome{ObjectID: s.ID, RowVersion: s.RowVersion, Status: s.Status}, nil
}

func packages(ps []api.Package) []shipment.Package {
	out := make([]shipment.Package, 0, len(ps))
	measure := func(s string) *kernel.Decimal {
		if s == "" {
			return nil
		}
		d, _ := api.Quantity(s)
		return &d
	}
	for _, p := range ps {
		k := shipment.Package{Cartons: p.Cartons, CartonFrom: p.CartonFrom, Gross: measure(p.GrossWeightKg), Net: measure(p.NetWeightKg),
			Volume: measure(p.VolumeCbm)}
		for _, c := range p.Contents {
			q, _ := api.Quantity(c.QuantityPerCarton)
			k.Contents = append(k.Contents, shipment.Content{LineID: c.LineID, PerCarton: q})
		}
		out = append(out, k)
	}
	return out
}

func (s Service) pack(ctx context.Context, name, kind string, cmd api.Command, key, id string, expected int64, ps []api.Package, evidence []string,
	validation error) (api.ShipmentReceipt, error) {
	h := &packCommand{s: s, kind: kind, id: id, expected: expected, evidence: slices.Clone(evidence)}
	if validation == nil {
		h.packages = packages(ps)
	}
	res, err := s.run(ctx, action.Command{Name: name, IdempotencyKey: key, ObjectID: id}, cmd, validation, h)
	if err != nil {
		return api.ShipmentReceipt{}, err
	}
	return s.shipmentReceipt(ctx, res)
}

// PlanPacking records the planned packing of a prepared shipment (packing.plan).
func (s Service) PlanPacking(ctx context.Context, cmd api.PlanPacking) (api.ShipmentReceipt, error) {
	return s.pack(ctx, api.CommandPlanPacking, shipment.KindPlan, cmd, cmd.IdempotencyKey, cmd.ShipmentID, cmd.ExpectedVersion, cmd.Packages,
		cmd.EvidenceIDs, cmd.Validate())
}

// RecordPacking records the actual packing of a prepared shipment (packing.record_actual; UL
// FUL-ActualPackage, R74).
func (s Service) RecordPacking(ctx context.Context, cmd api.RecordPacking) (api.ShipmentReceipt, error) {
	return s.pack(ctx, api.CommandRecordPacking, shipment.KindActual, cmd, cmd.IdempotencyKey, cmd.ShipmentID, cmd.ExpectedVersion, cmd.Packages,
		cmd.EvidenceIDs, cmd.Validate())
}

// cancelCommand runs shipment.cancel: it locks the shipment and cancels it (SM-FUL-SHIPMENT
// prepared → cancelled).
type cancelCommand struct {
	s        Service
	cmd      api.CancelShipment
	existing *shipment.Shipment
}

func (h *cancelCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.existing, err = postgres.LoadShipment(ctx, exec, scope(ctx), h.cmd.ShipmentID, postgres.ForUpdate)
	return err
}

func (h *cancelCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	s := h.existing
	if s == nil || !d.Sees(s.OwnerActor, s.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/shipment_id", "")
	}
	from := s.RowVersion
	if err := s.Cancel(h.cmd.ExpectedVersion, h.cmd.Reason, shipment.Act{By: d.Actor, Epoch: d.Epoch, At: h.s.Clock.Now()}); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.CancelShipment(ctx, exec, scope(ctx), s, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	return action.Outcome{ObjectID: s.ID, RowVersion: s.RowVersion, Status: s.Status}, nil
}

// CancelShipment cancels a prepared shipment (shipment.cancel).
func (s Service) CancelShipment(ctx context.Context, cmd api.CancelShipment) (api.ShipmentReceipt, error) {
	h := &cancelCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandCancelShipment, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ShipmentID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.ShipmentReceipt{}, err
	}
	return s.shipmentReceipt(ctx, res)
}

// shipmentReceipt completes res with the order the stored shipment binds and, for a confirmed one,
// the extension points its confirmation recorded as not wired.
func (s Service) shipmentReceipt(ctx context.Context, res api.Receipt) (api.ShipmentReceipt, error) {
	out := api.ShipmentReceipt{Receipt: res, UncheckedGuards: []string{}}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sh, err := postgres.LoadShipment(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || sh == nil {
			return err
		}
		out.Order = api.OrderBinding{OrderID: sh.Order.OrderID, OrderRevision: sh.Order.OrderRevision, OrderHash: sh.Order.OrderHash}
		if sh.Confirmation != nil {
			out.UncheckedGuards = slices.Clone(sh.Confirmation.Unchecked)
		}
		return nil
	})
	return out, err
}

func releaseRef(id string) qltapi.QualityReleaseRef { return qltapi.QualityReleaseRef{ReleaseID: id} }
