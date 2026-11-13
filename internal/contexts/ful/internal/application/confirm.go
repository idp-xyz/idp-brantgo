package application

import (
	"context"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/release"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// confirmCommand runs shipment.confirm (human only). Before the transaction it asks EVD about the
// evidence. Inside it, it locks the shipment FOR UPDATE, the order's fulfilment root FOR UPDATE and
// the shipment's shipment release FOR UPDATE, reads what the order's confirmed shipments consumed,
// and — after the ledger's authority guard — re-checks every guard in context code order with
// FUL's executor (judge: CNX, PTY, PUR, QLT, STL for scope shipment, TRD, FUL-INV-001), each keeping
// its rows FOR SHARE until FUL commits. It then needs the release granted (D-11) and consumes it
// with the confirmation; a refusal leaves both as they were. The confirmation records each guard's
// answer as evidence with its SHA-256 and publishes ShipmentConfirmed.
type confirmCommand struct {
	s        Service
	cmd      api.ConfirmShipment
	sources  map[string]bool
	existing *shipment.Shipment
	consumed shipment.Consumed
	release  *release.Release
}

func (h *confirmCommand) Prepare(ctx context.Context) error {
	var err error
	h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *confirmCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.consumed, h.release = nil, shipment.Consumed{}, nil
	s, err := postgres.LoadShipment(ctx, exec, scope(ctx), h.cmd.ShipmentID, postgres.ForUpdate)
	h.existing = s
	if err != nil || s == nil || s.Status != shipment.StatusPrepared {
		return err
	}
	h.consumed, h.release, err = releaseRoots(ctx, exec, s, release.ScopeShipment)
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
	j, err := h.s.judge(ctx, exec, s, h.consumed, release.ScopeShipment, h.release, now)
	if err != nil {
		return action.Outcome{}, err
	}
	if j.failure != nil {
		return action.Outcome{}, j.failure
	}
	rel := h.release
	if err = rel.Consumable(release.ScopeShipment, "/args/shipment_id"); err != nil {
		return action.Outcome{}, err
	}
	entries := append(j.entries, rel.Entry(j.exception))
	act := shipment.Act{By: d.Actor, Epoch: d.Epoch, At: now}
	from, relFrom := s.RowVersion, rel.RowVersion
	if err = s.Confirm(slices.Clone(h.cmd.EvidenceIDs), entries, nil, act); err != nil {
		return action.Outcome{}, err
	}
	if err = rel.Consume("", slices.Clone(h.cmd.EvidenceIDs), entries, act); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.ConfirmShipment(ctx, exec, scope(ctx), s, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	if err = postgres.ConsumeRelease(ctx, exec, scope(ctx), rel, relFrom); err != nil {
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
			"route": s.Route, "lines": lines, "guards_sha256": c.EntriesSHA256, "unchecked_guards": c.Unchecked, "release_id": rel.ID,
			"confirmed_at": kernel.FormatTimestamp(c.Confirmed.At)}}
	return action.Outcome{ObjectID: s.ID, RowVersion: s.RowVersion, Status: s.Status, Events: []action.Event{event}}, nil
}

// ConfirmShipment confirms a prepared shipment (shipment.confirm, human only; SM-FUL-SHIPMENT,
// SM-FUL-RELEASE granted → consumed, FUL-INV-001, FUL-INV-002, FUL-INV-003, R26, R27, R44, R45, R56,
// R59, D-1, D-11).
func (s Service) ConfirmShipment(ctx context.Context, cmd api.ConfirmShipment) (api.ShipmentReceipt, error) {
	h := &confirmCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandConfirmShipment, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ShipmentID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.ShipmentReceipt{}, err
	}
	return s.shipmentReceipt(ctx, res)
}
