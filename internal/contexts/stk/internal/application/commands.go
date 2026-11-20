package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/stk/api"
	"go.idp.xyz/idp-brant/internal/contexts/stk/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stk/internal/domain/batchlineage"
	"go.idp.xyz/idp-brant/internal/contexts/stk/internal/domain/ledger"
	"go.idp.xyz/idp-brant/internal/contexts/stk/internal/domain/reservation"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func (s Service) ConfigureModule(ctx context.Context, cmd api.ConfigureModule) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandConfigure, IdempotencyKey: cmd.IdempotencyKey, ObjectID: "module"}, cmd, cmd.Validate(), &configureCmd{s: s, cmd: cmd})
}

func (s Service) ReceiveStock(ctx context.Context, cmd api.ReceiveStock) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandReceive, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.MovementID}, cmd, cmd.Validate(), &receiveCmd{s: s, cmd: cmd})
}

func (s Service) IssueStock(ctx context.Context, cmd api.IssueStock) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandIssue, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.MovementID}, cmd, cmd.Validate(), &issueCmd{s: s, cmd: cmd})
}

func (s Service) AdjustStock(ctx context.Context, cmd api.AdjustStock) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandAdjust, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.MovementID}, cmd, cmd.Validate(), &adjustCmd{s: s, cmd: cmd})
}

func (s Service) PlaceReservation(ctx context.Context, cmd api.PlaceReservation) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandPlace, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ReservationID}, cmd, cmd.Validate(), &placeCmd{s: s, cmd: cmd})
}

func (s Service) ReleaseReservation(ctx context.Context, cmd api.ReleaseReservation) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandRelease, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ReservationID}, cmd, cmd.Validate(), &releaseCmd{s: s, cmd: cmd})
}

func (s Service) RecordHandover(ctx context.Context, cmd api.RecordHandover) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandHandover, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.MovementID}, cmd, cmd.Validate(), &handoverCmd{s: s, cmd: cmd})
}

type configureCmd struct {
	s   Service
	cmd api.ConfigureModule
	cur *postgres.Setting
}

func (h *configureCmd) Lock(ctx context.Context, exec bentopg.Executor) error {
	st, err := postgres.LoadSetting(ctx, exec, scope(ctx), postgres.ForUpdate)
	h.cur = st
	return err
}

func (h *configureCmd) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	kind, id, _, epoch := actorOf(d)
	now := h.s.Clock.Now()
	next := postgres.Setting{Enabled: h.cmd.Enabled, ActorKind: kind, ActorID: id, Epoch: epoch, UpdatedAt: now}
	switch {
	case h.cmd.ExpectedVersion == 0 && h.cur == nil:
		next.RowVersion = 1
		if err := postgres.InsertSetting(ctx, exec, scope(ctx), next); err != nil {
			return action.Outcome{}, err
		}
	case h.cur == nil || h.cur.RowVersion != h.cmd.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	default:
		next.RowVersion = h.cur.RowVersion + 1
		if err := postgres.SaveSetting(ctx, exec, scope(ctx), next, h.cur.RowVersion); err != nil {
			return action.Outcome{}, stockErr(err)
		}
	}
	status := api.StatusDisabled
	if next.Enabled {
		status = api.StatusEnabled
	}
	return action.Outcome{ObjectID: "module", RowVersion: next.RowVersion, Status: status}, nil
}

type receiveCmd struct {
	s    Service
	cmd  api.ReceiveStock
	bal  *ledger.Balance
	fact *purapi.ReceiptRequest
}

func (h *receiveCmd) Prepare(ctx context.Context) error {
	kind := h.cmd.SourceKind
	if kind != api.SourceCommitment && kind != api.SourceHoldForReuse {
		return stockErr(ledger.ReceiptSource(kind, false, false))
	}
	on, err := h.s.moduleEnabled(ctx)
	if err != nil {
		return err
	}
	if !on {
		return api.Reject(api.CodeDependencyNotSupported, "/", "")
	}
	if h.s.Receipts == nil {
		return api.Reject(api.CodeDependencyNotSupported, "/args/source_id", "")
	}
	fact, err := h.s.Receipts.ResolveReceiptRequest(ctx, h.cmd.SourceID, h.cmd.SourceLineID)
	if err != nil {
		if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/source_id", "")
		}
		return err
	}
	if fact.Code != "" {
		return kernel.Reject(kernel.CodeInvalidState, "/args/source_id", "")
	}
	if fact.SkuID != h.cmd.SkuID || fact.UnitID != h.cmd.UnitID || fact.UnitRevision != h.cmd.UnitRevision ||
		fact.Currency != h.cmd.Currency || fact.MinorUnits != h.cmd.MinorUnits {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/sku_id", "")
	}
	h.fact = &fact
	return stockErr(ledger.ReceiptSource(kind, fact.DirectShip, fact.HoldForReuse))
}

func (h *receiveCmd) Lock(ctx context.Context, exec bentopg.Executor) error {
	b, err := postgres.LoadBalance(ctx, exec, scope(ctx), keyOf(h.cmd.SkuID, h.cmd.LocationID, h.cmd.BatchID, h.cmd.CustodyKind), postgres.ForUpdate)
	h.bal = b
	return err
}

func (h *receiveCmd) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := h.s.requireEnabled(ctx, exec); err != nil {
		return action.Outcome{}, err
	}
	if taken, err := postgres.LoadMovement(ctx, exec, scope(ctx), h.cmd.MovementID); err != nil || taken != nil {
		if err != nil {
			return action.Outcome{}, err
		}
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/movement_id", "")
	}
	qty, err := qtyOf(h.cmd.Quantity)
	if err != nil {
		return action.Outcome{}, err
	}
	if h.fact != nil {
		capQty := h.fact.OrderedQuantity
		if h.cmd.SourceKind == api.SourceHoldForReuse {
			capQty = h.fact.HoldForReuseQuantity
		}
		limit, err := qtyOf(capQty)
		if err != nil || qty.Cmp(limit) > 0 {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/quantity", "")
		}
	}
	return h.s.postIn(ctx, exec, d, inbound{
		id: h.cmd.MovementID, key: keyOf(h.cmd.SkuID, h.cmd.LocationID, h.cmd.BatchID, h.cmd.CustodyKind),
		kind: ledger.KindReceive, qty: qty, unit: h.cmd.UnitID, revision: h.cmd.UnitRevision,
		currency: h.cmd.Currency, minor: h.cmd.MinorUnits, cost: h.cmd.UnitCostMinor, basis: h.cmd.PriceBasisQty,
		sourceKind: h.cmd.SourceKind, sourceID: h.cmd.SourceID, sourceLine: h.cmd.SourceLineID, reason: h.cmd.Reason,
		at: h.cmd.ReceivedAt, links: h.cmd.Lineage, bal: h.bal, event: api.EventStockReceived,
	})
}

type issueCmd struct {
	s   Service
	cmd api.IssueStock
	bal *ledger.Balance
	res *reservation.Reservation
}

func (h *issueCmd) Lock(ctx context.Context, exec bentopg.Executor) error {
	b, err := postgres.LoadBalance(ctx, exec, scope(ctx), keyOf(h.cmd.SkuID, h.cmd.LocationID, h.cmd.BatchID, h.cmd.CustodyKind), postgres.ForUpdate)
	if err != nil {
		return err
	}
	h.bal = b
	if h.cmd.ReservationID == "" {
		return nil
	}
	h.res, err = postgres.LoadReservation(ctx, exec, scope(ctx), h.cmd.ReservationID, postgres.ForUpdate)
	return err
}

func (h *issueCmd) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := h.s.requireEnabled(ctx, exec); err != nil {
		return action.Outcome{}, err
	}
	if err := stockErr(ledger.IssueSource(h.cmd.SourceKind)); err != nil {
		return action.Outcome{}, err
	}
	qty, err := qtyOf(h.cmd.Quantity)
	if err != nil {
		return action.Outcome{}, err
	}
	return h.s.postOut(ctx, exec, d, outbound{
		id: h.cmd.MovementID, key: keyOf(h.cmd.SkuID, h.cmd.LocationID, h.cmd.BatchID, h.cmd.CustodyKind),
		kind: ledger.KindIssue, qty: qty, unit: h.cmd.UnitID, revision: h.cmd.UnitRevision,
		sourceKind: h.cmd.SourceKind, sourceID: h.cmd.SourceID, sourceLine: h.cmd.SourceLineID,
		reason: h.cmd.Reason, at: h.cmd.IssuedAt, bal: h.bal, res: h.res, reservationID: h.cmd.ReservationID,
		event: api.EventStockIssued,
	})
}

type adjustCmd struct {
	s   Service
	cmd api.AdjustStock
	bal *ledger.Balance
}

func (h *adjustCmd) Lock(ctx context.Context, exec bentopg.Executor) error {
	b, err := postgres.LoadBalance(ctx, exec, scope(ctx), keyOf(h.cmd.SkuID, h.cmd.LocationID, h.cmd.BatchID, h.cmd.CustodyKind), postgres.ForUpdate)
	h.bal = b
	return err
}

func (h *adjustCmd) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := h.s.requireEnabled(ctx, exec); err != nil {
		return action.Outcome{}, err
	}
	qty, err := qtyOf(h.cmd.Quantity)
	if err != nil {
		return action.Outcome{}, err
	}
	key := keyOf(h.cmd.SkuID, h.cmd.LocationID, h.cmd.BatchID, h.cmd.CustodyKind)
	if h.cmd.Direction == api.AdjustIn {
		out, err := h.s.postIn(ctx, exec, d, inbound{
			id: h.cmd.MovementID, key: key, kind: ledger.KindAdjustIn, qty: qty, unit: h.cmd.UnitID, revision: h.cmd.UnitRevision,
			currency: h.cmd.Currency, minor: h.cmd.MinorUnits, cost: h.cmd.UnitCostMinor, basis: h.cmd.PriceBasisQty,
			reason: h.cmd.Reason, at: h.cmd.AdjustedAt, bal: h.bal, event: api.EventStockAdjusted,
		})
		return out, err
	}
	return h.s.postOut(ctx, exec, d, outbound{
		id: h.cmd.MovementID, key: key, kind: ledger.KindAdjustOut, qty: qty, unit: h.cmd.UnitID, revision: h.cmd.UnitRevision,
		reason: h.cmd.Reason, at: h.cmd.AdjustedAt, bal: h.bal, event: api.EventStockAdjusted,
	})
}

type placeCmd struct {
	s   Service
	cmd api.PlaceReservation
	bal *ledger.Balance
}

func (h *placeCmd) Lock(ctx context.Context, exec bentopg.Executor) error {
	b, err := postgres.LoadBalance(ctx, exec, scope(ctx), keyOf(h.cmd.SkuID, h.cmd.LocationID, h.cmd.BatchID, h.cmd.CustodyKind), postgres.ForUpdate)
	h.bal = b
	return err
}

func (h *placeCmd) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := h.s.requireEnabled(ctx, exec); err != nil {
		return action.Outcome{}, err
	}
	if taken, err := postgres.LoadReservation(ctx, exec, scope(ctx), h.cmd.ReservationID, postgres.NoLock); err != nil || taken != nil {
		if err != nil {
			return action.Outcome{}, err
		}
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/reservation_id", "")
	}
	if h.bal == nil {
		return action.Outcome{}, api.Reject(api.CodeStockInsufficient, "/args/quantity", "available "+textDec(kernel.Decimal{}))
	}
	qty, err := qtyOf(h.cmd.Quantity)
	if err != nil {
		return action.Outcome{}, err
	}
	if h.bal.UnitID != h.cmd.UnitID || h.bal.UnitRevision != h.cmd.UnitRevision {
		return action.Outcome{}, stockErr(ledger.ErrUnit)
	}
	next, err := ledger.Place(h.bal.OnHand, h.bal.Reserved, qty)
	if err != nil {
		return action.Outcome{}, api.Reject(api.CodeStockInsufficient, "/args/quantity", "available "+textDec(h.bal.Available()))
	}
	expected := h.bal.RowVersion
	h.bal.Reserved = next
	h.bal.RowVersion++
	if err = postgres.SaveBalance(ctx, exec, scope(ctx), *h.bal, expected); err != nil {
		return action.Outcome{}, stockErr(err)
	}
	kind, id, team, _ := actorOf(d)
	res := reservation.Reservation{ID: h.cmd.ReservationID, SKU: h.cmd.SkuID, Location: h.cmd.LocationID, Batch: h.cmd.BatchID,
		Custody: h.cmd.CustodyKind, OrderID: h.cmd.OrderID, OrderLineID: h.cmd.OrderLineID, Quantity: qty, Remaining: qty,
		Status: reservation.StatusActive, UnitID: h.cmd.UnitID, UnitRevision: h.cmd.UnitRevision, RowVersion: 1,
		OwnerActor: id, OwnerTeam: team}
	if err = postgres.InsertReservation(ctx, exec, scope(ctx), res); err != nil {
		return action.Outcome{}, err
	}
	_ = kind
	return action.Outcome{ObjectID: res.ID, RowVersion: 1, Status: api.StatusActive, Events: []action.Event{{
		Type: api.EventReservationPlaced, AggregateType: "Reservation", AggregateID: res.ID, AggregateVersion: 1,
		Fields: map[string]any{
			"reservation_id": res.ID, "sku_id": res.SKU, "location_id": res.Location, "batch_id": res.Batch,
			"quantity": textDec(qty), "unit_id": res.UnitID, "order_id": res.OrderID, "order_line_id": res.OrderLineID,
		},
	}}}, nil
}

type releaseCmd struct {
	s   Service
	cmd api.ReleaseReservation
	bal *ledger.Balance
	res *reservation.Reservation
}

func (h *releaseCmd) Lock(ctx context.Context, exec bentopg.Executor) error {
	// The balance key is read with no lock. The balance is then locked before the reservation,
	// the same order as reservation.place, stock.issue and StockAvailability v1.
	r, err := postgres.LoadReservation(ctx, exec, scope(ctx), h.cmd.ReservationID, postgres.NoLock)
	if err != nil || r == nil {
		h.res = r
		return err
	}
	b, err := postgres.LoadBalance(ctx, exec, scope(ctx), keyOf(r.SKU, r.Location, r.Batch, r.Custody), postgres.ForUpdate)
	if err != nil {
		return err
	}
	h.bal = b
	h.res, err = postgres.LoadReservation(ctx, exec, scope(ctx), h.cmd.ReservationID, postgres.ForUpdate)
	return err
}

func (h *releaseCmd) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := h.s.requireEnabled(ctx, exec); err != nil {
		return action.Outcome{}, err
	}
	if h.res == nil || h.bal == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/reservation_id", "")
	}
	if h.res.RowVersion != h.cmd.ExpectedVersion {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	expectedRes := h.res.RowVersion
	back, ok := h.res.Release()
	if !ok {
		return action.Outcome{}, api.Reject(api.CodeStockReservationConflict, "/args/reservation_id", "")
	}
	if h.bal.Reserved.Cmp(back) < 0 {
		return action.Outcome{}, api.Reject(api.CodeStockReservationConflict, "/args/reservation_id", "")
	}
	expectedBal := h.bal.RowVersion
	h.bal.Reserved = h.bal.Reserved.Sub(back)
	h.bal.RowVersion++
	if err := postgres.SaveBalance(ctx, exec, scope(ctx), *h.bal, expectedBal); err != nil {
		return action.Outcome{}, stockErr(err)
	}
	if err := postgres.SaveReservation(ctx, exec, scope(ctx), *h.res, expectedRes); err != nil {
		return action.Outcome{}, stockErr(err)
	}
	_ = d
	return action.Outcome{ObjectID: h.res.ID, RowVersion: h.res.RowVersion, Status: api.StatusReleased, Events: []action.Event{{
		Type: api.EventReservationReleased, AggregateType: "Reservation", AggregateID: h.res.ID, AggregateVersion: h.res.RowVersion,
		Fields: map[string]any{
			"reservation_id": h.res.ID, "sku_id": h.res.SKU, "quantity": textDec(back), "unit_id": h.res.UnitID,
			"released_at": kernel.FormatTimestamp(h.s.Clock.Now()),
		},
	}}}, nil
}

type handoverCmd struct {
	s    Service
	cmd  api.RecordHandover
	from *ledger.Balance
	to   *ledger.Balance
}

func (h *handoverCmd) Lock(ctx context.Context, exec bentopg.Executor) error {
	from := keyOf(h.cmd.SkuID, h.cmd.LocationID, h.cmd.BatchID, h.cmd.CustodyKind)
	to := keyOf(h.cmd.SkuID, h.cmd.ToLocationID, h.cmd.ToBatchID, h.cmd.ToCustodyKind)
	keys := []ledger.Key{from}
	if h.cmd.Kind == api.HandoverTransfer || h.cmd.Kind == api.HandoverReturn {
		if to.Less(from) {
			keys = []ledger.Key{to, from}
		} else {
			keys = []ledger.Key{from, to}
		}
	}
	for _, k := range keys {
		b, err := postgres.LoadBalance(ctx, exec, scope(ctx), k, postgres.ForUpdate)
		if err != nil {
			return err
		}
		if k.Same(from) {
			h.from = b
		} else {
			h.to = b
		}
	}
	return nil
}

func (h *handoverCmd) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := h.s.requireEnabled(ctx, exec); err != nil {
		return action.Outcome{}, err
	}
	out, in, err := ledger.HandoverMoves(h.cmd.Kind)
	if err != nil {
		return action.Outcome{}, stockErr(err)
	}
	qty, err := qtyOf(h.cmd.Quantity)
	if err != nil {
		return action.Outcome{}, err
	}
	from := keyOf(h.cmd.SkuID, h.cmd.LocationID, h.cmd.BatchID, h.cmd.CustodyKind)
	var total int64
	posted, err := h.s.postOut(ctx, exec, d, outbound{
		id: h.cmd.MovementID, key: from, kind: h.cmd.Kind, qty: qty, unit: h.cmd.UnitID, revision: h.cmd.UnitRevision,
		sourceKind: ledger.SourceHandover, sourceID: h.cmd.MovementID, sourceLine: "out", reason: h.cmd.Reason,
		bal: h.from, event: api.EventStockIssued, total: &total,
	})
	if err != nil || !in {
		_ = out
		return posted, err
	}
	// The inbound layer is priced so issuing the whole quantity returns the same minor total.
	// Both movements are one transaction. The receipt names the outbound movement and carries
	// both facts, so a transfer or a return is not only the departure.
	if h.from == nil {
		return action.Outcome{}, api.Reject(api.CodeStockInsufficient, "/args/quantity", "available "+textDec(kernel.Decimal{}))
	}
	arrived, err := h.s.postIn(ctx, exec, d, inbound{
		id: h.cmd.CounterMovementID, key: keyOf(h.cmd.SkuID, h.cmd.ToLocationID, h.cmd.ToBatchID, h.cmd.ToCustodyKind),
		kind: h.cmd.Kind, qty: qty, unit: h.cmd.UnitID, revision: h.cmd.UnitRevision,
		currency: h.from.Currency, minor: h.from.MinorUnits, minorUnits: h.from.MinorUnits,
		costMinor: total, basisQty: qty,
		sourceKind: ledger.SourceHandover, sourceID: h.cmd.MovementID, sourceLine: "in", reason: h.cmd.Reason,
		bal: h.to, event: api.EventStockReceived,
	})
	if err != nil {
		return action.Outcome{}, err
	}
	posted.Events = append(posted.Events, arrived.Events...)
	return posted, nil
}

type inbound struct {
	id, kind, unit, currency, cost, basis, sourceKind, sourceID, sourceLine, reason, at, event string
	revision                                                                                   int64
	minor, minorUnits                                                                          int
	qty, basisQty                                                                              kernel.Decimal
	costMinor                                                                                  int64
	key                                                                                        ledger.Key
	links                                                                                      []api.LineageLink
	bal                                                                                        *ledger.Balance
}

type outbound struct {
	id, kind, unit, sourceKind, sourceID, sourceLine, reason, at, event, reservationID string
	revision                                                                           int64
	qty                                                                                kernel.Decimal
	key                                                                                ledger.Key
	bal                                                                                *ledger.Balance
	res                                                                                *reservation.Reservation
	total                                                                              *int64
}

func keyOf(sku, location, batch, custody string) ledger.Key {
	return ledger.Key{SKU: sku, Location: location, Batch: batch, Custody: custody}
}

func (s Service) postIn(ctx context.Context, exec bentopg.Executor, d authorization.Decision, in inbound) (action.Outcome, error) {
	if taken, err := postgres.LoadMovement(ctx, exec, scope(ctx), in.id); err != nil || taken != nil {
		if err != nil {
			return action.Outcome{}, err
		}
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/movement_id", "")
	}
	when, err := instant(s.Clock, in.at)
	if err != nil {
		return action.Outcome{}, err
	}
	var cost int64
	var basis kernel.Decimal
	if in.basisQty.Sign() > 0 {
		cost, basis = in.costMinor, in.basisQty
	} else {
		cost, err = minorOf(in.currency, in.minor, in.cost)
		if err != nil {
			return action.Outcome{}, err
		}
		basis, err = qtyOf(in.basis)
		if err != nil {
			return action.Outcome{}, err
		}
		in.minorUnits = in.minor
	}
	kind, actor, team, epoch := actorOf(d)
	bal := in.bal
	if bal == nil {
		bal = &ledger.Balance{Key: in.key, UnitID: in.unit, UnitRevision: in.revision, Currency: in.currency, MinorUnits: in.minorUnits, OwnerActor: actor, OwnerTeam: team}
	}
	if err = ledger.Receive(bal, in.qty, in.unit, in.revision, in.currency, in.minorUnits); err != nil {
		return action.Outcome{}, stockErr(err)
	}
	expected := bal.RowVersion
	if expected == 0 {
		bal.RowVersion = 1
		if err = postgres.InsertBalance(ctx, exec, scope(ctx), *bal); err != nil {
			return action.Outcome{}, err
		}
	} else {
		bal.RowVersion++
		if err = postgres.SaveBalance(ctx, exec, scope(ctx), *bal, expected); err != nil {
			return action.Outcome{}, stockErr(err)
		}
	}
	if err = postgres.InsertMovement(ctx, exec, scope(ctx), postgres.Movement{
		ID: in.id, Kind: in.kind, Key: in.key, Quantity: in.qty, UnitID: in.unit, UnitRev: in.revision,
		SourceKind: in.sourceKind, SourceID: in.sourceID, SourceLine: in.sourceLine, Reason: in.reason, At: when,
		ActorKind: kind, ActorID: actor, Epoch: epoch, OwnerActor: actor, OwnerTeam: team,
	}); err != nil {
		return action.Outcome{}, err
	}
	layerID := action.NewID("lay", when)
	if err = postgres.InsertLayer(ctx, exec, scope(ctx), ledger.Layer{
		ID: layerID, MovementID: in.id, Key: in.key, Currency: in.currency, MinorUnits: in.minorUnits, UnitCostMinor: cost,
		Basis: basis, Original: in.qty, Remaining: in.qty, ReceivedAt: when,
	}); err != nil {
		return action.Outcome{}, err
	}
	if err = writeLineage(ctx, exec, in.id, in.links, in.unit, in.revision, when); err != nil {
		return action.Outcome{}, err
	}
	fields := map[string]any{
		"movement_id": in.id, "sku_id": in.key.SKU, "location_id": in.key.Location, "batch_id": in.key.Batch,
		"custody_kind": in.key.Custody, "quantity": textDec(in.qty), "unit_id": in.unit, "unit_cost_minor": cost,
		"currency": in.currency, "layer_id": layerID, "received_at": kernel.FormatTimestamp(when),
		"source_kind": in.sourceKind, "source_id": in.sourceID,
	}
	if in.event == api.EventStockAdjusted {
		fields = map[string]any{
			"movement_id": in.id, "sku_id": in.key.SKU, "location_id": in.key.Location, "batch_id": in.key.Batch,
			"custody_kind": in.key.Custody, "quantity_delta": textDec(in.qty), "unit_id": in.unit,
			"reason": in.reason, "adjusted_at": kernel.FormatTimestamp(when),
		}
	}
	return action.Outcome{ObjectID: in.id, RowVersion: bal.RowVersion, Status: statusOf(in.kind), Events: []action.Event{{
		Type: in.event, AggregateType: "StockLedger", AggregateID: in.id, AggregateVersion: bal.RowVersion, Fields: fields,
	}}}, nil
}

func (s Service) postOut(ctx context.Context, exec bentopg.Executor, d authorization.Decision, out outbound) (action.Outcome, error) {
	if taken, err := postgres.LoadMovement(ctx, exec, scope(ctx), out.id); err != nil || taken != nil {
		if err != nil {
			return action.Outcome{}, err
		}
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/movement_id", "")
	}
	if out.bal == nil {
		return action.Outcome{}, api.Reject(api.CodeStockInsufficient, "/args/quantity", "available "+textDec(kernel.Decimal{}))
	}
	if out.bal.UnitID != out.unit || out.bal.UnitRevision != out.revision {
		return action.Outcome{}, stockErr(ledger.ErrUnit)
	}
	when, err := instant(s.Clock, out.at)
	if err != nil {
		return action.Outcome{}, err
	}
	var resExpected int64
	if out.reservationID != "" {
		matched := out.res != nil && out.res.Status == reservation.StatusActive && out.res.SKU == out.key.SKU &&
			out.res.Location == out.key.Location && out.res.Batch == out.key.Batch && out.res.Custody == out.key.Custody
		if !matched {
			return action.Outcome{}, api.Reject(api.CodeStockReservationConflict, "/args/reservation_id", "")
		}
		if err = stockErr(ledger.IssueReservation(out.res.OrderID)); err != nil {
			return action.Outcome{}, err
		}
		if out.res.Remaining.Cmp(out.qty) < 0 {
			return action.Outcome{}, api.Reject(api.CodeStockReservationConflict, "/args/reservation_id", "")
		}
		resExpected = out.res.RowVersion
		on, reserved, _, err := ledger.ReservedIssue(out.bal.OnHand, out.bal.Reserved, out.res.Remaining, out.qty)
		if err != nil {
			return action.Outcome{}, api.Reject(api.CodeStockInsufficient, "/args/quantity", "available "+textDec(out.bal.Available()))
		}
		if !out.res.Consume(out.qty) {
			return action.Outcome{}, api.Reject(api.CodeStockReservationConflict, "/args/reservation_id", "")
		}
		out.bal.OnHand, out.bal.Reserved = on, reserved
	} else {
		on, err := ledger.UnreservedIssue(out.bal.OnHand, out.bal.Reserved, out.qty)
		if err != nil {
			return action.Outcome{}, api.Reject(api.CodeStockInsufficient, "/args/quantity", "available "+textDec(out.bal.Available()))
		}
		out.bal.OnHand = on
	}
	layers, err := postgres.LoadLayers(ctx, exec, scope(ctx), out.key, postgres.ForUpdate)
	if err != nil {
		return action.Outcome{}, err
	}
	takes, err := ledger.AllocateFIFO(layers, out.qty)
	if err != nil {
		return action.Outcome{}, stockErr(err)
	}
	if out.total != nil {
		*out.total = ledger.TotalMinor(takes)
	}
	expected := out.bal.RowVersion
	out.bal.RowVersion++
	if err = postgres.SaveBalance(ctx, exec, scope(ctx), *out.bal, expected); err != nil {
		return action.Outcome{}, stockErr(err)
	}
	if out.res != nil && out.reservationID != "" {
		if err = postgres.SaveReservation(ctx, exec, scope(ctx), *out.res, resExpected); err != nil {
			return action.Outcome{}, stockErr(err)
		}
	}
	for _, take := range takes {
		for i := range layers {
			if layers[i].ID == take.LayerID {
				if err = postgres.SaveLayerRemaining(ctx, exec, scope(ctx), layers[i].ID, layers[i].Remaining.Sub(take.Quantity)); err != nil {
					return action.Outcome{}, err
				}
			}
		}
	}
	kind, actor, team, epoch := actorOf(d)
	if err = postgres.InsertMovement(ctx, exec, scope(ctx), postgres.Movement{
		ID: out.id, Kind: out.kind, Key: out.key, Quantity: out.qty, UnitID: out.unit, UnitRev: out.revision,
		SourceKind: out.sourceKind, SourceID: out.sourceID, SourceLine: out.sourceLine, Reason: out.reason, At: when,
		ActorKind: kind, ActorID: actor, Epoch: epoch, OwnerActor: actor, OwnerTeam: team,
	}); err != nil {
		return action.Outcome{}, err
	}
	currency := out.bal.Currency
	for _, take := range takes {
		cur := currency
		for _, layer := range layers {
			if layer.ID == take.LayerID {
				cur = layer.Currency
			}
		}
		if err = postgres.InsertAllocation(ctx, exec, scope(ctx), postgres.Allocation{
			ID: action.NewID("alc", when), MovementID: out.id, LayerID: take.LayerID, Quantity: take.Quantity,
			AmountMinor: take.AmountMinor, Currency: cur,
		}); err != nil {
			return action.Outcome{}, err
		}
	}
	fields := map[string]any{
		"movement_id": out.id, "sku_id": out.key.SKU, "location_id": out.key.Location, "batch_id": out.key.Batch,
		"custody_kind": out.key.Custody, "quantity": textDec(out.qty), "unit_id": out.unit,
		"reservation_id": out.reservationID, "shipment_id": "", "shipment_line_id": "",
	}
	if out.event == api.EventStockAdjusted {
		fields = map[string]any{
			"movement_id": out.id, "sku_id": out.key.SKU, "location_id": out.key.Location, "batch_id": out.key.Batch,
			"custody_kind": out.key.Custody, "quantity_delta": "-" + textDec(out.qty), "unit_id": out.unit,
			"reason": out.reason, "adjusted_at": kernel.FormatTimestamp(when),
		}
	}
	return action.Outcome{ObjectID: out.id, RowVersion: out.bal.RowVersion, Status: statusOf(out.kind), Events: []action.Event{{
		Type: out.event, AggregateType: "StockLedger", AggregateID: out.id, AggregateVersion: out.bal.RowVersion, Fields: fields,
	}}}, nil
}

func writeLineage(ctx context.Context, exec bentopg.Executor, movementID string, links []api.LineageLink, unit string, revision int64, at time.Time) error {
	for _, link := range links {
		norm, err := batchlineage.Normalize(link.InputBatchID, link.OutputBatchID, link.ShipmentID, link.ShipmentLineID, link.Quantity, unit, revision)
		if err != nil {
			return kernel.Reject(kernel.CodeSchemaInvalid, "/args/lineage", "")
		}
		if err = postgres.InsertLineage(ctx, exec, scope(ctx), postgres.Lineage{
			ID: action.NewID("lin", at), MovementID: movementID, InputBatchID: norm.InputBatchID, OutputKind: norm.OutputKind,
			OutputBatchID: norm.OutputBatchID, OutputID: norm.OutputID, OutputLineID: norm.OutputLineID,
			Quantity: norm.Quantity, UnitID: unit, UnitRevision: revision, At: at,
		}); err != nil {
			return err
		}
	}
	return nil
}

func statusOf(kind string) string {
	switch kind {
	case ledger.KindReceive:
		return api.StatusReceived
	case ledger.KindIssue, ledger.KindShipmentIssue:
		return api.StatusIssued
	case ledger.KindAdjustIn, ledger.KindAdjustOut:
		return api.StatusAdjusted
	default:
		return kind
	}
}
