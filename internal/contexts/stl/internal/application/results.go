package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderresult"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Order results (S5 unit C9, WBS S5-17; SM-STL-ORDER-RESULT, STL-INV-011; MD-49; BT18, BT24). The figures are derived
// when asked from what TRD (R32: the order's state, end, net sales and analysis rate), FUL (R30: its shipment
// lines), PUR (R31: its commitment lines and verified deliveries) and POL (R33: the currencies' minor units) say
// before the transaction, and from STL's own statements, credit notes, applications and charges read inside it.
// The result is the order's: it counts every STL record of the order, and the caller sees it when TRD shows the
// caller the order. order_result.close locks the result FOR UPDATE, then the order account and the order's
// purchase statements FOR SHARE in ID order — every application, unapplication, credit note, reversal and new
// sales statement of the order locks one of those FOR UPDATE, and every charge confirmation or allocation the
// result FOR SHARE — so the snapshot it appends is what stood when it committed.

// resultFacts are what the upstreams say of an order before STL's transaction.
type resultFacts struct {
	order          *Order
	orderCurrency  kernel.Currency
	resultCurrency kernel.Currency
	currencies     error
	shipments      []OrderShipmentLine
	shipmentsShown bool
	purchases      []OrderPurchaseLine
	purchasesShown bool
}

// resultFacts asks TRD, FUL, PUR and POL about order id (nothing locked).
func (s Service) resultFacts(ctx context.Context, id string) (resultFacts, error) {
	var f resultFacts
	orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{id})
	if err != nil {
		return f, err
	}
	o, ok := orders[id]
	if !ok {
		return f, nil
	}
	f.order = &o
	refs := map[string]polapi.ConfigRef{"order": {Kind: polapi.KindCurrency, ConfigID: o.Currency.ID, Revision: o.Currency.Revision}}
	if a := o.Analysis; a != nil && a.Currency != "" && a.Currency != orderCurrencyOf(o) && a.CurrencyRevision > 0 {
		refs["result"] = polapi.ConfigRef{Kind: polapi.KindCurrency, ConfigID: a.Currency, Revision: a.CurrencyRevision}
	}
	list := make([]polapi.ConfigRef, 0, len(refs))
	for _, r := range refs {
		list = append(list, r)
	}
	found, err := s.Upstream.Configs.ResolveConfigs(ctx, list, s.Clock.Now())
	if err != nil {
		return f, err
	}
	pick := func(key string) (kernel.Currency, bool) {
		rc, ok := found[refs[key]]
		if !ok || rc.Data.Currency == nil {
			return kernel.Currency{}, false
		}
		c, err := kernel.NewCurrency(rc.Data.Currency.Code, rc.Data.Currency.MinorUnits)
		return c, err == nil
	}
	var okOrder, okResult bool
	f.orderCurrency, okOrder = pick("order")
	f.resultCurrency, okResult = f.orderCurrency, okOrder
	if _, analysed := refs["result"]; analysed {
		f.resultCurrency, okResult = pick("result")
	} else if a := o.Analysis; a != nil && a.Currency != "" && a.Currency != orderCurrencyOf(o) {
		okResult = false
	}
	if !okOrder || !okResult {
		f.currencies = kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "POL shows no currency revision of the order's analysis")
	}
	shipments, shown, err := s.orderShipments(ctx, []string{id})
	if err != nil {
		return f, err
	}
	f.shipments, f.shipmentsShown = shipments[id], shown
	if p, ok := s.Upstream.Deliveries.(OrderPurchases); ok {
		if f.purchases, f.purchasesShown, err = p.ResolveOrderPurchases(ctx, id); err != nil {
			return f, err
		}
	}
	return f, nil
}

// input builds the order result's input from facts f and STL's own records, read with exec.
func (s Service) input(ctx context.Context, exec bentopg.Executor, f resultFacts) (orderresult.Input, error) {
	sc, o := scope(ctx), f.order
	id := o.Binding.OrderID
	in := orderresult.Input{At: kernel.Instant(s.Clock.Now()), Order: orderresult.Order{ID: id, Revision: o.Binding.OrderRevision,
		Hash: o.Binding.OrderHash, Status: o.Status, Ended: o.Ended, Currency: f.orderCurrency, Net: zeroIn(f.orderCurrency)},
		CreditNotes: map[string][]settlement.CreditNote{}, Applied: map[string]map[int]kernel.Money{},
		PayableApplications: map[string][]fa.Application{}, ShipmentsShown: f.shipmentsShown, CommitmentsShown: f.purchasesShown}
	if o.NetMinor != "" {
		net, err := kernel.ParseMinor(f.orderCurrency, o.NetMinor)
		if err != nil {
			return in, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD states the order's net sales in another precision")
		}
		in.Order.Net = net
	}
	if a := o.Analysis; a != nil && f.resultCurrency != f.orderCurrency {
		rate, err := kernel.ParseDecimal(a.Rate, kernel.FactorFormat)
		if err != nil || rate.Sign() <= 0 {
			return in, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD states no analysis rate of the order")
		}
		in.Order.Analysis = &orderresult.Analysis{Currency: f.resultCurrency, Rate: rate}
	}
	var err error
	if in.Sales, err = postgres.Statements(ctx, exec, sc, settlement.KindSales, id); err != nil {
		return in, err
	}
	if in.Purchases, err = postgres.Statements(ctx, exec, sc, settlement.KindPurchase, id); err != nil {
		return in, err
	}
	for _, st := range in.Sales {
		if in.Applied[st.ID], err = postgres.InstallmentApplied(ctx, exec, sc, st); err != nil {
			return in, err
		}
		if in.CreditNotes[st.ID], err = postgres.CreditNotes(ctx, exec, sc, st.ID, ""); err != nil {
			return in, err
		}
	}
	for _, st := range in.Purchases {
		if in.Applied[st.ID], err = postgres.InstallmentApplied(ctx, exec, sc, st); err != nil {
			return in, err
		}
		if in.PayableApplications[st.ID], err = postgres.StatementApplications(ctx, exec, sc, st.ID); err != nil {
			return in, err
		}
	}
	if in.Applications, err = postgres.Applications(ctx, exec, sc, "", id); err != nil {
		return in, err
	}
	if in.Charges, err = postgres.OrderCharges(ctx, exec, sc, id); err != nil {
		return in, err
	}
	for _, l := range f.shipments {
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil {
			continue
		}
		in.Shipments = append(in.Shipments, orderresult.ShipmentLine{ShipmentID: l.ShipmentID, LineID: l.LineID, Status: l.Status,
			OrderLineID: l.OrderLineID, Quantity: q})
	}
	for _, l := range f.purchases {
		c, err := kernel.NewCurrency(l.Currency, l.MinorUnits)
		if err != nil {
			continue
		}
		net, err := kernel.ParseMinor(c, l.NetMinor)
		if err != nil {
			continue
		}
		line := orderresult.CommitmentLine{CommitmentID: l.CommitmentID, OrderLineID: l.OrderLineID, Status: l.Status, SupplierID: l.SupplierID, Net: net}
		for _, d := range l.Deliveries {
			line.Deliveries = append(line.Deliveries, orderresult.Delivery{DeliveryID: d.DeliveryID, LineID: d.LineID, Version: d.Version})
		}
		in.Commitments = append(in.Commitments, line)
	}
	return in, nil
}

// derived is an order result's figures and close conditions as one command or read derived them.
type derived struct {
	figures orderresult.Figures
	dims    []orderresult.Dimension
	content api.SnapshotContent
	json    []byte
	sha256  string
}

func (s Service) derive(ctx context.Context, exec bentopg.Executor, f resultFacts) (derived, error) {
	in, err := s.input(ctx, exec, f)
	if err != nil {
		return derived{}, err
	}
	var d derived
	d.figures, d.dims = orderresult.Compute(in)
	d.content = api.SnapshotContent{OrderID: in.Order.ID, OrderRevision: in.Order.Revision, OrderHash: in.Order.Hash,
		Figures: figuresView(d.figures, f.orderCurrency), Conditions: conditionsView(d.dims)}
	if d.json, err = json.Marshal(d.content); err != nil {
		return derived{}, err
	}
	sum := sha256.Sum256(d.json)
	d.sha256 = hex.EncodeToString(sum[:])
	return d, nil
}

func sourcesView(s []orderresult.Source) []api.ResultSourceView {
	out := make([]api.ResultSourceView, 0, len(s))
	for _, x := range s {
		out = append(out, api.ResultSourceView{Kind: x.Kind, ID: x.ID, Version: x.Version, Detail: x.Detail})
	}
	return out
}

func gapsOf(g []string) []string {
	if g == nil {
		return []string{}
	}
	return g
}

func figureView(f orderresult.Figure) api.FigureView {
	return api.FigureView{NetSalesMinor: f.NetSales.MinorString(), CreditsMinor: f.Credits.MinorString(), PurchaseCostMinor: f.PurchaseCost.MinorString(),
		PurchaseConfirmedMinor: f.PurchaseConfirmed.MinorString(), PurchaseEstimatedMinor: f.PurchaseEstimated.MinorString(),
		DirectChargesMinor: f.DirectCharges.MinorString(), ChargesConfirmedMinor: f.ChargesConfirmed.MinorString(),
		ChargesEstimatedMinor: f.ChargesEstimated.MinorString(), ResultMinor: f.Result.MinorString(), Margin: f.Margin,
		Sources: sourcesView(f.Sources), Gaps: gapsOf(f.Gaps)}
}

func figuresView(f orderresult.Figures, orderCurrency kernel.Currency) api.ResultFiguresView {
	v := api.ResultFiguresView{AsOf: f.AsOf, Currency: f.Currency.Code(), MinorUnits: f.Currency.MinorUnits(), OrderCurrency: orderCurrency.Code(),
		AnalysisRate: f.Rate, Forecast: figureView(f.Forecast), Executing: figureView(f.Executing),
		Confirmed: api.ConfirmedResultView{FigureView: figureView(f.Confirmed.Figure), Batches: []api.BatchView{},
			OrderLevelChargesMinor: f.Confirmed.OrderLevelCharges.MinorString(), UnsoldPurchaseCostMinor: f.Confirmed.UnsoldPurchaseCost.MinorString()},
		FXDifference: api.FXResultView{DifferenceMinor: f.FX.Difference.MinorString(), Items: []api.FXItemView{}, Gaps: gapsOf(f.FX.Gaps)},
		NetCash: api.CashView{ReceiptsMinor: f.Cash.Receipts.MinorString(), PaymentsMinor: f.Cash.Payments.MinorString(),
			NetMinor: f.Cash.Net.MinorString(), Sources: sourcesView(f.Cash.Sources), Gaps: gapsOf(f.Cash.Gaps)}}
	for _, b := range f.Confirmed.Batches {
		v.Confirmed.Batches = append(v.Confirmed.Batches, api.BatchView{ShipmentID: b.ShipmentID, StatementIDs: b.StatementIDs,
			NetSalesMinor: b.NetSales.MinorString(), CreditsMinor: b.Credits.MinorString(), CostOfSalesMinor: b.CostOfSales.MinorString(),
			DirectChargesMinor: b.DirectCharges.MinorString(), ResultMinor: b.Result.MinorString()})
	}
	for _, x := range f.FX.Items {
		v.FXDifference.Items = append(v.FXDifference.Items, api.FXItemView{ApplicationID: x.ApplicationID, TargetKind: x.TargetKind,
			StatementID: x.StatementID, StageID: x.StageID, SourceCurrency: x.Source.Currency().Code(), SourceAmountMinor: x.Source.MinorString(),
			TargetCurrency: x.Target.Currency().Code(), TargetAmountMinor: x.Target.MinorString(), DifferenceMinor: x.Difference.MinorString()})
	}
	return v
}

func conditionsView(dims []orderresult.Dimension) []api.CloseConditionView {
	out := make([]api.CloseConditionView, 0, len(dims))
	for _, d := range dims {
		out = append(out, api.CloseConditionView{Dimension: d.Name, Status: d.Status, Items: d.Items})
	}
	return out
}

// resultCommand is what order_result.review, close and reopen share: the facts read before the transaction, the
// result locked FOR UPDATE in it, and what the command derived.
type resultCommand struct {
	s       Service
	orderID string
	facts   resultFacts
	result  *orderresult.Result
	derived *derived
}

func (h *resultCommand) prepare(ctx context.Context) error {
	var err error
	h.facts, err = h.s.resultFacts(ctx, h.orderID)
	return err
}

// visible refuses an order TRD does not show the caller, a draft, a result of another version, and POL not
// stating the currencies.
func (h *resultCommand) visible(expected int64) error {
	switch {
	case h.facts.order == nil:
		return kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
	case h.facts.order.Status == OrderDraft:
		return kernel.Reject(kernel.CodeInvalidState, "/args/order_id", "A draft order has no operating result")
	case h.result == nil && expected != 0, h.result != nil && h.result.Version != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return h.facts.currencies
}

func (h *resultCommand) derive(ctx context.Context, exec bentopg.Executor) (derived, error) {
	d, err := h.s.derive(ctx, exec, h.facts)
	if err == nil {
		h.derived = &d
	}
	return d, err
}

func resultEvent(typ string, r *orderresult.Result, fields map[string]any) action.Event {
	fields["order_id"], fields["order_result_version"] = r.OrderID, r.Version
	return action.Event{Type: typ, AggregateType: "OrderResult", AggregateID: r.OrderID, AggregateVersion: r.Version, Fields: fields}
}

// reviewResult runs order_result.review (open → reviewed; human or AI).
type reviewResult struct {
	resultCommand
	cmd api.ReviewOrderResult
}

func (h *reviewResult) Prepare(ctx context.Context) error { return h.prepare(ctx) }

func (h *reviewResult) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.result, h.derived = nil, nil
	if h.facts.order == nil || h.cmd.ExpectedVersion > 0 {
		var err error
		h.result, err = postgres.LoadResult(ctx, exec, scope(ctx), h.orderID, postgres.ForUpdate)
		return err
	}
	results, err := h.s.lockResults(ctx, exec, []string{h.orderID}, postgres.ForUpdate)
	h.result = results[h.orderID]
	return err
}

func (h *reviewResult) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, now, r := scope(ctx), kernel.Instant(h.s.Clock.Now()), h.result
	if err := h.visible(h.cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	x, err := h.derive(ctx, exec)
	if err != nil {
		return action.Outcome{}, err
	}
	from := r.Version
	if err = r.Review(orderresult.Review{OrderRevision: h.facts.order.Binding.OrderRevision, Summary: x.json, SummarySHA256: x.sha256,
		Note: h.cmd.Note, By: fundAct(d, now, "")}); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertResultReview(ctx, exec, sc, r.OrderID, r.Reviews[len(r.Reviews)-1]); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveResult(ctx, exec, sc, r, from, now); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: r.OrderID, RowVersion: r.Version, Status: r.Status}, nil
}

// ReviewOrderResult reviews an order's result (order_result.review).
func (s Service) ReviewOrderResult(ctx context.Context, cmd api.ReviewOrderResult) (api.OrderResultReceipt, error) {
	h := &reviewResult{resultCommand: resultCommand{s: s, orderID: cmd.OrderID}, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandReviewOrderResult, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.OrderResultReceipt{}, err
	}
	return s.resultReceipt(ctx, res, h.derived)
}

// closeResult runs order_result.close (reviewed → settled, STL-INV-011; human only).
type closeResult struct {
	resultCommand
	cmd api.CloseOrderResult
}

func (h *closeResult) Prepare(ctx context.Context) error { return h.prepare(ctx) }

func (h *closeResult) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.result, h.derived = nil, nil
	sc := scope(ctx)
	var err error
	if h.result, err = postgres.LoadResult(ctx, exec, sc, h.orderID, postgres.ForUpdate); err != nil || h.result == nil {
		return err
	}
	purchases, err := postgres.Statements(ctx, exec, sc, settlement.KindPurchase, h.orderID)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(purchases))
	for _, st := range purchases {
		ids = append(ids, st.ID)
	}
	return postgres.LockSettlementRoots(ctx, exec, sc, h.orderID, ids)
}

func (h *closeResult) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, now, r := scope(ctx), kernel.Instant(h.s.Clock.Now()), h.result
	if h.facts.order != nil && r == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/order_id", "The order result is open: review it first")
	}
	if err := h.visible(h.cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	x, err := h.derive(ctx, exec)
	if err != nil {
		return action.Outcome{}, err
	}
	from := r.Version
	conf := x.figures.Confirmed
	snap := orderresult.Snapshot{OrderRevision: h.facts.order.Binding.OrderRevision, OrderHash: h.facts.order.Binding.OrderHash,
		Currency: x.figures.Currency, NetSales: conf.NetSales, Result: conf.Result, Content: x.json, ContentSHA256: x.sha256, Reason: h.cmd.Reason,
		By: fundAct(d, now, h.cmd.Reason)}
	if err = r.Close(x.dims, snap); err != nil {
		return action.Outcome{}, err
	}
	s := r.Latest()
	if err = postgres.InsertSnapshot(ctx, exec, sc, r.OrderID, *s); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveResult(ctx, exec, sc, r, from, now); err != nil {
		return action.Outcome{}, err
	}
	ev := resultEvent(api.EventOrderResultClosed, r, map[string]any{"snapshot_seq": s.Seq, "order_revision": s.OrderRevision,
		"content_sha256": s.ContentSHA256, "claims": orderresult.NotEnabled})
	return action.Outcome{ObjectID: r.OrderID, RowVersion: r.Version, Status: r.Status, Events: []action.Event{ev}}, nil
}

// CloseOrderResult closes an order's result (order_result.close). Human only.
func (s Service) CloseOrderResult(ctx context.Context, cmd api.CloseOrderResult) (api.OrderResultReceipt, error) {
	h := &closeResult{resultCommand: resultCommand{s: s, orderID: cmd.OrderID}, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandCloseOrderResult, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.OrderResultReceipt{}, err
	}
	return s.resultReceipt(ctx, res, h.derived)
}

// reopenResult runs order_result.reopen (settled → open, BT24; human only).
type reopenResult struct {
	resultCommand
	cmd      api.ReopenOrderResult
	evidence map[string]bool
}

func (h *reopenResult) Prepare(ctx context.Context) error {
	var err error
	if h.evidence, err = h.s.resolveEvidence(ctx, h.cmd.EvidenceIDs); err != nil {
		return err
	}
	return h.prepare(ctx)
}

func (h *reopenResult) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.result, h.derived = nil, nil
	var err error
	h.result, err = postgres.LoadResult(ctx, exec, scope(ctx), h.orderID, postgres.ForUpdate)
	return err
}

func (h *reopenResult) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, now, r := scope(ctx), kernel.Instant(h.s.Clock.Now()), h.result
	if h.facts.order != nil && r == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/order_id", "The order result is open: nothing to reopen")
	}
	if err := h.visible(h.cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	if err := evidenceFound(h.evidence, h.cmd.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	from := r.Version
	if err := r.Reopen(orderresult.Reopening{Reason: h.cmd.Reason, EvidenceIDs: h.cmd.EvidenceIDs, By: fundAct(d, now, h.cmd.Reason)}); err != nil {
		return action.Outcome{}, err
	}
	re := r.Reopenings[len(r.Reopenings)-1]
	if err := postgres.InsertReopening(ctx, exec, sc, r.OrderID, re); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.SaveResult(ctx, exec, sc, r, from, now); err != nil {
		return action.Outcome{}, err
	}
	ev := resultEvent(api.EventOrderResultReopened, r, map[string]any{"snapshot_seq": re.Seq, "reopening_seq": len(r.Reopenings)})
	return action.Outcome{ObjectID: r.OrderID, RowVersion: r.Version, Status: r.Status, Events: []action.Event{ev}}, nil
}

// ReopenOrderResult reopens a settled order result (order_result.reopen). Human only.
func (s Service) ReopenOrderResult(ctx context.Context, cmd api.ReopenOrderResult) (api.OrderResultReceipt, error) {
	h := &reopenResult{resultCommand: resultCommand{s: s, orderID: cmd.OrderID}, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandReopenOrderResult, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.OrderResultReceipt{}, err
	}
	return s.resultReceipt(ctx, res, h.derived)
}

// resultReceipt completes a receipt with what the command derived, or — a replay, a reopening — with the newest
// snapshot.
func (s Service) resultReceipt(ctx context.Context, res api.Receipt, x *derived) (api.OrderResultReceipt, error) {
	out := api.OrderResultReceipt{Receipt: res, Conditions: []api.CloseConditionView{}}
	if x != nil {
		out.Currency, out.ConfirmedResultMinor, out.Margin = x.content.Figures.Currency, x.content.Figures.Confirmed.ResultMinor,
			x.content.Figures.Confirmed.Margin
		out.Conditions = x.content.Conditions
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		r, err := postgres.LoadResult(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || r == nil {
			return err
		}
		snap := r.Latest()
		if snap == nil {
			return nil
		}
		out.SnapshotSeq = snap.Seq
		if x == nil {
			var c api.SnapshotContent
			if err := json.Unmarshal(snap.Content, &c); err != nil {
				return err
			}
			out.Currency, out.ConfirmedResultMinor, out.Margin, out.Conditions = c.Figures.Currency, c.Figures.Confirmed.ResultMinor,
				c.Figures.Confirmed.Margin, c.Conditions
		}
		return nil
	})
	return out, err
}

// GetOrderResult returns an order's result (order_result.get): its state, the figures and close conditions
// derived now, every snapshot, review and reopening. NOT_FOUND when TRD does not show the caller the order.
func (s Service) GetOrderResult(ctx context.Context, orderID string) (api.OrderResultView, error) {
	var v api.OrderResultView
	if !kernel.ValidIdentifier(orderID) {
		return v, s.read(ctx, api.CommandGetOrderResult, orderID, nil)
	}
	facts, err := s.resultFacts(ctx, orderID)
	if err != nil {
		return v, err
	}
	err = s.read(ctx, api.CommandGetOrderResult, orderID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		if facts.order == nil || facts.order.Status == OrderDraft {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		if facts.currencies != nil {
			return facts.currencies
		}
		x, err := s.derive(ctx, exec, facts)
		if err != nil {
			return err
		}
		v = api.OrderResultView{OrderID: orderID, Status: orderresult.StatusOpen, OrderRevision: facts.order.Binding.OrderRevision,
			OrderStatus: facts.order.Status, OrderEnded: facts.order.Ended, Live: x.content.Figures, Conditions: x.content.Conditions,
			Settleable: orderresult.NotSettled(x.dims) == nil, Snapshots: []api.SnapshotView{}, Reviews: []api.ReviewView{},
			Reopenings: []api.ReopeningView{}, CheckedAt: kernel.Instant(s.Clock.Now())}
		r, err := postgres.LoadResult(ctx, exec, scope(ctx), orderID, postgres.NoLock)
		if err != nil || r == nil {
			return err
		}
		v.Status, v.Version = r.Status, r.Version
		for _, sn := range r.Snapshots {
			var c api.SnapshotContent
			if err := json.Unmarshal(sn.Content, &c); err != nil {
				return err
			}
			v.Snapshots = append(v.Snapshots, api.SnapshotView{Seq: sn.Seq, SnapshotContent: c, ContentSHA256: sn.ContentSHA256, Reason: sn.Reason,
				ClosedBy: sn.By.By, ClosedAt: sn.By.At, Reopened: sn.Seq <= len(r.Reopenings)})
		}
		for _, rv := range r.Reviews {
			v.Reviews = append(v.Reviews, api.ReviewView{Seq: rv.Seq, OrderRevision: rv.OrderRevision, SummarySHA256: rv.SummarySHA256, Note: rv.Note,
				ReviewedBy: rv.By.By, ReviewedAt: rv.By.At})
		}
		for _, re := range r.Reopenings {
			v.Reopenings = append(v.Reopenings, api.ReopeningView{Seq: re.Seq, Reason: re.Reason, EvidenceIDs: re.EvidenceIDs, ReopenedBy: re.By.By,
				ReopenedAt: re.By.At})
		}
		return nil
	})
	return v, err
}
