//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Charges, charge allocation and the order operating result on stand-ins for TRD, FUL and PUR (S5 unit C9, WBS
// S5-17; tactical model STL-AGG-Charge, STL-AGG-OrderResult; SM-STL-CHARGE, SM-STL-ORDER-RESULT; STL-INV-010,
// STL-INV-011; Context Map MD-49; baseline 4.5, 4.6 S-A, 4.13; BT18, BT24) on PostgreSQL 16. The result world is the
// ledger world (stl_settlement_ledger_integration_test.go) with STL's ports Orders, Shipments (with its
// order-level half) and Deliveries (with its order-level half: the order's commitment lines) steered, so an order
// carries S-A's numbers, TRD's analysis rate and its end. Python V0.50 has no STL.
//
// People: officer.1 records and allocates charges (credit_officer); treasury.1 confirms charges, settles, applies
// and reviews, closes and reopens results (treasury); agent.c9 is an AI agent holding every C9 permission — the
// human-only commands refuse it.

// resultShipments is the stand-in for FUL with the order-level half of STL's port: every line it shows is of a
// confirmed shipment.
type resultShipments struct{ *shipmentLines }

func (f resultShipments) ResolveOrderShipments(_ context.Context, ids []string) (map[string][]stl.OrderShipmentLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]stl.OrderShipmentLine{}
	for _, id := range ids {
		out[id] = []stl.OrderShipmentLine{}
	}
	for _, s := range f.lines {
		l := s.Line
		if slices.Contains(ids, l.OrderID) {
			out[l.OrderID] = append(out[l.OrderID], stl.OrderShipmentLine{ShipmentID: l.ShipmentID, LineID: l.LineID, Status: "confirmed",
				OrderLineID: l.OrderLineID, UnitID: l.UnitID, Quantity: l.Quantity})
		}
	}
	return out, nil
}

// resultPurchases is the stand-in for PUR: the verified delivery lines a purchase statement settles and the
// order's commitment lines.
type resultPurchases struct {
	mu          sync.Mutex
	lines       map[stlapi.SettlementSourceRef]stl.DeliverySource
	commitments map[string][]stl.OrderPurchaseLine
}

func (p *resultPurchases) ResolveDeliverySources(_ context.Context, refs []stlapi.SettlementSourceRef) (map[stlapi.SettlementSourceRef]stl.DeliverySource, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[stlapi.SettlementSourceRef]stl.DeliverySource{}
	for _, r := range refs {
		if l, ok := p.lines[r]; ok {
			out[r] = l
		}
	}
	return out, nil
}

func (p *resultPurchases) ResolveOrderPurchases(_ context.Context, order string) ([]stl.OrderPurchaseLine, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.commitments[order], true, nil
}

// bought has PUR show commitment id buying quantity of order o's line L1 from supplier for net CNY minor, verified
// delivered as delivery (version 1).
func (p *resultPurchases) bought(o stl.Order, id, supplier, delivery, quantity, netMinor string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ref := stlapi.SettlementSourceRef{DeliveryID: delivery, LineID: "L1", Version: 1}
	p.commitments[o.Binding.OrderID] = append(p.commitments[o.Binding.OrderID], stl.OrderPurchaseLine{CommitmentID: id, OrderLineID: "L1",
		Status: "confirmed", SupplierID: supplier, Currency: "CNY", MinorUnits: 2, NetMinor: netMinor, Deliveries: []stlapi.SettlementSourceRef{ref}})
	p.lines[ref] = stl.DeliverySource{Line: stl.DeliveryLine{DeliveryID: delivery, LineID: "L1", Version: 1, HandedOverAt: kernel.Instant(time.Now().Add(-72 * time.Hour)),
		CommitmentID: id, SupplierID: supplier, OrderID: o.Binding.OrderID, OrderRevision: o.Binding.OrderRevision, OrderHash: o.Binding.OrderHash,
		SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS", UnitRevision: 1, PurchaseQuantity: quantity, VerifiedQuantity: quantity, Currency: "CNY",
		MinorUnits: 2, LineNet: netMinor, LineTax: "0"}, Check: func(string) error { return nil }}
}

type resultWorld struct {
	*fundWorld
	ful              *shipmentLines
	pur              *resultPurchases
	officer, agent   context.Context
	freight, bankPay polapi.ConfigRef
	preRate, saRate  polapi.ConfigRef
	bill             string
}

func newResultWorld(t *testing.T) *resultWorld {
	t.Helper()
	fw := newFundWorld(t)
	w := &resultWorld{fundWorld: fw, ful: &shipmentLines{lines: map[stlapi.SettlementSourceRef]stl.ShipmentSource{}},
		pur: &resultPurchases{lines: map[stlapi.SettlementSourceRef]stl.DeliverySource{}, commitments: map[string][]stl.OrderPurchaseLine{}}}
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	fw.stl, err = stl.New(db, stl.Upstream{Orders: fw.orders, Configs: w.c.POL, Parties: w.c.PTY, Sources: w.c.EVD,
		Shipments: resultShipments{w.ful}, Deliveries: w.pur})
	w.must(err)
	w.officer = w.assign("officer.1", "credit_officer")
	w.agent = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.c9"},
		Permissions: []string{"charge.record", "charge.confirm", "charge.allocate", "order_result.review", "order_result.close",
			"order_result.reopen", "stl.read", "evd.read", "pty.read", "pol.read", "trd.read", "ful.read", "pur.read"}, DataScope: "company"})
	for _, p := range []string{"forwarder.1", "supplier.sa"} {
		w.party(w.editor, p, true, "evidence")
	}
	win := newWindow()
	w.freight = w.configure(w.editor, w.reviewer, polapi.KindFeeCategory, "freight", 0, win.from, time.Time{}, polapi.ConfigData{
		FeeCategory: &polapi.FeeCategory{Name: "海运费", Category: "freight", Side: polapi.FeeSidePurchaseCost, Inclusion: polapi.InclusionSeparate,
			AllocationBasis: polapi.AllocationQuantity, PolicyBasis: "synthetic"}}, true)
	w.bankPay = w.configure(w.editor, w.reviewer, polapi.KindBankAccount, "bank.cny.pay", 0, win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "CNY", polapi.AccountBoth), true)
	w.preRate = w.fxRate("cny.usd.pre", "CNY", "USD", "0.142857142857", polapi.PurposeSettlement)
	w.saRate = w.fxRate("cny.usd.sa", "CNY", "USD", "0.145454545455", polapi.PurposeSettlement)
	w.bill = w.source(w.editor, "bill.freight", "合成货代账单：海运费 CNY 650.00")
	return w
}

func (w *resultWorld) record(ctx context.Context, cmd stlapi.RecordCharge) (stlapi.ChargeReceipt, error) {
	cmd.IdempotencyKey = w.key()
	return w.stl.RecordCharge(ctx, cmd)
}

// estimate is a new charge of the forwarder's offer: estimated amount CNY of the beneficiaries, same currency.
func (w *resultWorld) estimate(id, ref, amount string, beneficiaries ...stlapi.ChargeTarget) stlapi.RecordCharge {
	fee, cny := stlRef(w.freight), stlRef(w.cny)
	return stlapi.RecordCharge{ChargeID: id, Status: stlapi.ChargeEstimated, Source: stlapi.ChargeSource{Kind: stlapi.ChargeSourceLogistics,
		PartyID: "forwarder.1", Reference: ref}, Beneficiaries: beneficiaries, FeeCategory: &fee, Currency: &cny,
		Conversion: &stlapi.ChargeConversion{Kind: stlapi.ConversionSameCurrency}, Amount: amount, OccurredOn: "2026-11-01",
		AnalysisOn: "2026-11-01", Basis: "合成：货代报价的预计运费"}
}

// billOf is the forwarder's bill of charge id at version: pending, amount CNY, occurred on.
func (w *resultWorld) billOf(id string, version int64, ref, amount, on string) stlapi.RecordCharge {
	return stlapi.RecordCharge{ChargeID: id, ExpectedVersion: version, Status: stlapi.ChargePending, Source: stlapi.ChargeSource{
		Kind: stlapi.ChargeSourceSupplierBill, PartyID: "forwarder.1", Reference: ref}, Amount: amount, OccurredOn: on, AnalysisOn: on,
		Basis: "合成：货代账单", EvidenceIDs: []string{w.bill}}
}

func (w *resultWorld) confirmCharge(ctx context.Context, id string, version int64) (stlapi.ChargeReceipt, error) {
	return w.stl.ConfirmCharge(ctx, stlapi.ConfirmCharge{IdempotencyKey: w.key(), ChargeID: id, ExpectedVersion: version,
		EvidenceIDs: []string{w.bill}, Reason: "账单核对无误"})
}

func (w *resultWorld) allocate(ctx context.Context, id string, version int64, basis string, lines ...stlapi.AllocationLine) (stlapi.ChargeReceipt, error) {
	return w.stl.AllocateCharge(ctx, stlapi.AllocateCharge{IdempotencyKey: w.key(), ChargeID: id, ExpectedVersion: version, Basis: basis,
		DataBasis: "FUL 实际出运数量 / 合成数据", Lines: lines})
}

func orderTarget(order string) stlapi.ChargeTarget {
	return stlapi.ChargeTarget{Kind: stlapi.ChargeTargetOrder, OrderID: order}
}

func shipmentTarget(order, shipment string) stlapi.ChargeTarget {
	return stlapi.ChargeTarget{Kind: stlapi.ChargeTargetShipment, OrderID: order, ShipmentID: shipment}
}

func (w *resultWorld) review(ctx context.Context, order string, version int64) (stlapi.OrderResultReceipt, error) {
	return w.stl.ReviewOrderResult(ctx, stlapi.ReviewOrderResult{IdempotencyKey: w.key(), OrderID: order, ExpectedVersion: version,
		Note: "复核履约、账款、费用与案件状态"})
}

func (w *resultWorld) close(ctx context.Context, order string, version int64) (stlapi.OrderResultReceipt, error) {
	return w.stl.CloseOrderResult(ctx, stlapi.CloseOrderResult{IdempotencyKey: w.key(), OrderID: order, ExpectedVersion: version,
		Reason: "成本结算完成"})
}

func (w *resultWorld) result(order string) stlapi.OrderResultView {
	w.t.Helper()
	v, err := w.stl.GetOrderResult(w.treasury, order)
	w.must(err)
	return v
}

// ended has TRD's stand-in show order id closed by command end.
func (w *resultWorld) ended(id, end string) {
	w.orders.mu.Lock()
	defer w.orders.mu.Unlock()
	o := w.orders.orders[id]
	o.Status, o.Ended = "closed", end
	w.orders.orders[id] = o
}

// saOrder is baseline 4.6 S-A's order: 100 PCS of L1 at USD 10.00 = 1,000.00, 20 % before and 80 % 30 days after
// shipment, analysed in CNY at 7.0; PUR's commitment.sa buys the 100 PCS for CNY 4,000.00 (delivery.sa).
func (w *resultWorld) saOrder() stl.Order {
	w.t.Helper()
	o := w.activeOrder("order.sa", "customer.1", w.usd, payStage("P1", "order_confirmed", "20000"),
		stl.OrderStage{StageID: "P2", Trigger: "after_shipment", Days: 30, Percent: "80", AmountMinor: "80000"})
	o.Analysis = &stl.OrderAnalysis{OrderCurrency: "USD", Currency: "CNY", Rate: "7.0", CurrencyRevision: w.cny.Revision}
	o.NetMinor = "100000"
	w.orders.put(o)
	w.pur.bought(o, "commitment.sa", "supplier.sa", "delivery.sa", "100", "400000")
	return o
}

// saSettled runs S-A steps 1–4 and the purchase side: CNY 1,400.00 prepays P1 (USD 200.00 at the settlement rate,
// analysis difference 0); the shipments of 60 and 40 settle at USD 600.00 + 400.00 (the prepayment moves); CNY
// 5,500.00 pays the 800.00 open (2,750.00 for each 400.00, −50.00 each against the analysis rate); the purchase
// statement of delivery.sa makes the payable CNY 4,000.00, paid.
func (w *resultWorld) saSettled(o stl.Order) {
	w.t.Helper()
	w.cnyReceipt("fund.pre", "1400.00")
	a, err := w.applyAt("app.pre", "fund.pre", stlapi.ApplicationTarget{Kind: stlapi.TargetOrderStage, OrderID: "order.sa", StageID: "P1"}, "1400.00",
		&w.preRate)
	w.must(err)
	if a.AmountMinor != "20000" || a.Conversion == nil || a.Conversion.FXDifferenceMinor != "0" {
		w.t.Fatalf("prepayment %+v %+v", a, a.Conversion)
	}
	w.ful.shipped(o, "shipment.60", "60", "60000", time.Now().Add(-48*time.Hour))
	w.ful.shipped(o, "shipment.40", "40", "40000", time.Now().Add(-24*time.Hour))
	w.settle("statement.600", "shipment.60")
	w.settle("statement.400", "shipment.40")
	w.cnyReceipt("fund.cny", "5500.00")
	for _, x := range []struct{ id, statement string }{{"app.600", "statement.600"}, {"app.400", "statement.400"}} {
		a, err := w.applyAt(x.id, "fund.cny", toStatement(stlapi.TargetReceivable, x.statement, "P2"), "2750.00", &w.saRate)
		w.must(err)
		if a.AmountMinor != "40000" || a.Conversion.FXDifferenceMinor != "-5000" {
			w.t.Fatalf("%s %+v", x.id, a)
		}
	}
	p, err := w.stl.ConfirmSettlement(w.treasury, stlapi.ConfirmSettlement{IdempotencyKey: w.key(), StatementID: "statement.purchase.sa",
		StatementKind: stlapi.StatementPurchase, Sources: []stlapi.SettlementSourceRef{{DeliveryID: "delivery.sa", LineID: "L1", Version: 1}}})
	w.must(err)
	if p.AmountMinor != "400000" || p.Currency != "CNY" {
		w.t.Fatalf("purchase statement %+v", p)
	}
	_, err = w.stl.RecordFund(w.treasury, stlapi.RecordFund{IdempotencyKey: w.key(), FundRecordID: "fund.pay", FundKind: stlapi.FundPayment,
		CounterpartyID: "supplier.sa", Currency: stlRef(w.cny), BankAccount: stlRef(w.bankPay), ReceivedAmount: "4000.00",
		ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "pay.sa", EvidenceIDs: []string{w.advice}})
	w.must(err)
	_, err = w.stl.Apply(w.treasury, stlapi.Apply{IdempotencyKey: w.key(), ApplicationID: "app.pay", FundRecordID: "fund.pay",
		Target: toStatement(stlapi.TargetPayable, "statement.purchase.sa", ""), Amount: "4000.00"})
	w.must(err)
}

func conditionOf(v []stlapi.CloseConditionView, dim string) stlapi.CloseConditionView {
	for _, c := range v {
		if c.Dimension == dim {
			return c
		}
	}
	return stlapi.CloseConditionView{}
}

// saEstimate records S-A's freight estimate: CNY 500.00 for the order (the forwarder's offer).
func (w *resultWorld) saEstimate() {
	w.t.Helper()
	r, err := w.record(w.officer, w.estimate("freight.sa", "OFFER-SA", "500.00", orderTarget("order.sa")))
	w.must(err)
	if r.Status != stlapi.ChargeEstimated || r.RowVersion != 1 || r.EstimateMinor != "50000" || r.AmountMinor != "50000" {
		w.t.Fatalf("estimate %+v", r)
	}
}

// saFreightBilled records the forwarder's bill of CNY 650.00 for the estimate, confirms it and allocates it by
// quantity 60 / 40 to the two shipments: 390.00 + 260.00 (S-A step 5, STL-INV-010's positive example).
func (w *resultWorld) saFreightBilled() {
	w.t.Helper()
	r, err := w.record(w.officer, w.billOf("freight.sa", 1, "BILL-SA", "650.00", "2026-11-10"))
	if err != nil || r.Status != stlapi.ChargePending || r.RowVersion != 2 || r.AmountMinor != "65000" || r.EstimateMinor != "50000" {
		w.t.Fatalf("bill %+v %v", r, err)
	}
	if r, err = w.confirmCharge(w.treasury, "freight.sa", 2); err != nil || r.Status != stlapi.ChargeConfirmed || r.RowVersion != 3 {
		w.t.Fatalf("confirmed %+v %v", r, err)
	}
	r, err = w.allocate(w.officer, "freight.sa", 3, stlapi.AllocationByQuantity,
		stlapi.AllocationLine{LineID: "A", Target: shipmentTarget("order.sa", "shipment.60"), Weight: "60"},
		stlapi.AllocationLine{LineID: "B", Target: shipmentTarget("order.sa", "shipment.40"), Weight: "40"})
	w.must(err)
	if r.RowVersion != 4 || r.AllocationSeq != 1 || len(r.Lines) != 2 || r.Lines[0].AmountMinor != "39000" || r.Lines[1].AmountMinor != "26000" {
		w.t.Fatalf("allocation %+v", r)
	}
}

// saClosed is S-A at step 7 with its result settled: snapshot 1.
func (w *resultWorld) saClosed() stlapi.OrderResultView {
	w.t.Helper()
	o := w.saOrder()
	w.saEstimate()
	w.saSettled(o)
	w.saFreightBilled()
	w.ended("order.sa", "order.close")
	_, err := w.review(w.treasury, "order.sa", 0)
	w.must(err)
	_, err = w.close(w.treasury, "order.sa", 1)
	w.must(err)
	return w.result("order.sa")
}

// saFigures checks S-A's figures (baseline 4.6) in minor units of CNY.
func saFigures(t *testing.T, what string, f stlapi.ResultFiguresView, confirmedCharges, orderLevel, confirmed string) {
	t.Helper()
	fc, ex, c := f.Forecast, f.Executing, f.Confirmed
	switch {
	case f.Currency != "CNY" || f.MinorUnits != 2 || f.OrderCurrency != "USD" || f.AnalysisRate != "7":
		t.Fatalf("%s: currency %+v", what, f)
	case fc.NetSalesMinor != "700000" || fc.PurchaseCostMinor != "400000" || fc.DirectChargesMinor != "50000" || fc.ResultMinor != "250000":
		t.Fatalf("%s: forecast %+v, want 7,000 − 4,000 − 500 = 2,500", what, fc)
	case c.NetSalesMinor != "700000" || c.CreditsMinor != "0" || c.PurchaseCostMinor != "400000" || c.DirectChargesMinor != confirmedCharges ||
		c.ResultMinor != confirmed || c.OrderLevelChargesMinor != orderLevel || c.UnsoldPurchaseCostMinor != "0" || len(c.Batches) != 2:
		t.Fatalf("%s: confirmed %+v", what, c)
	case ex.PurchaseConfirmedMinor != "400000" || ex.PurchaseEstimatedMinor != "0" || ex.ResultMinor != c.ResultMinor:
		t.Fatalf("%s: executing %+v", what, ex)
	case f.FXDifference.DifferenceMinor != "-10000" || len(f.FXDifference.Items) != 3:
		t.Fatalf("%s: FX difference %+v, want −100.00", what, f.FXDifference)
	case f.NetCash.ReceiptsMinor != "690000" || f.NetCash.PaymentsMinor != "400000" || f.NetCash.NetMinor != "290000":
		t.Fatalf("%s: net cash %+v", what, f.NetCash)
	}
	for i, want := range [][5]string{{"shipment.40", "280000", "160000", "26000", "94000"}, {"shipment.60", "420000", "240000", "39000", "141000"}} {
		b := c.Batches[i]
		if got := [5]string{b.ShipmentID, b.NetSalesMinor, b.CostOfSalesMinor, b.DirectChargesMinor, b.ResultMinor}; got != want {
			t.Fatalf("%s: batch %d %v, want %v", what, i, got, want)
		}
	}
}

// Baseline 4.6 S-A exactly, on PG16 (baseline 4.5 definitions; STL-INV-010, STL-INV-011, BT18):
//
//   - The forecast is CNY 2,500.00 throughout: the order's USD 1,000.00 at the analysis rate 7.0 less PUR's
//     commitment of CNY 4,000.00 less the freight estimate of CNY 500.00 — kept when the bill replaces it.
//   - BT18: everything shipped, settled, paid and bought, the freight only estimated and the order still active:
//     review, then close is ORDER_RESULT_NOT_SETTLED listing fulfilment and direct_charges — never a pass because
//     the goods left; receivables, payables and purchase costs are met, claims stay not_enabled.
//   - The freight billed at CNY 650.00, confirmed and allocated 390.00 + 260.00 by quantity; TRD closes the order;
//     close settles the result: confirmed CNY 2,350.00 = 1,410.00 (4,200 − 2,400 − 390) + 940.00 (2,800 − 1,600 −
//     260); the business settlement FX difference −100.00 (CNY 6,900 received against 7,000 analysed), apart; net
//     cash 6,900.00 − 4,000.00 = 2,900.00 (the freight is not paid through STL). The snapshot holds the figures and
//     conditions with their SHA-256; ChargeAllocated and OrderResultClosed are published.
func TestBaseline46SAOrderResult(t *testing.T) {
	w := newResultWorld(t)
	o := w.saOrder()
	w.saEstimate()
	v := w.result("order.sa")
	if v.Status != stlapi.ResultOpen || v.Version != 0 || v.Settleable || v.Live.Forecast.ResultMinor != "250000" ||
		v.Live.Executing.ResultMinor != "250000" || v.Live.Confirmed.ResultMinor != "0" {
		t.Fatalf("before anything settles %+v", v)
	}
	for dim, item := range map[string]string{stlapi.DimensionFulfilment: "TRD: the order is active",
		stlapi.DimensionPurchaseCosts: "delivery delivery.sa of commitment commitment.sa line L1 is verified, not settled",
		stlapi.DimensionDirectCharges: "charge freight.sa is estimated"} {
		if c := conditionOf(v.Conditions, dim); c.Status != stlapi.ConditionUnmet || !strings.Contains(strings.Join(c.Items, "; "), item) {
			t.Fatalf("%s before anything settles %+v", dim, c)
		}
	}
	w.saSettled(o)

	rv, err := w.review(w.treasury, "order.sa", 0)
	w.must(err)
	if rv.Status != stlapi.ResultReviewed || rv.RowVersion != 1 || rv.Currency != "CNY" || len(rv.Conditions) != 6 {
		t.Fatalf("review %+v", rv)
	}
	_, err = w.close(w.treasury, "order.sa", 1)
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != stlapi.CodeOrderResultNotSettled || len(rej.Fields) != 2 || rej.Fields[0].Path != "/conditions/fulfilment" ||
		rej.Fields[1].Path != "/conditions/direct_charges" || !strings.Contains(rej.Fields[1].Reason, "charge freight.sa is estimated") {
		t.Fatalf("BT18 close %v", err)
	}
	v = w.result("order.sa")
	for dim, status := range map[string]string{stlapi.DimensionReceivables: stlapi.ConditionMet, stlapi.DimensionPayables: stlapi.ConditionMet,
		stlapi.DimensionPurchaseCosts: stlapi.ConditionMet, stlapi.DimensionClaims: stlapi.ConditionNotEnabled} {
		if c := conditionOf(v.Conditions, dim); c.Status != status {
			t.Fatalf("BT18 %s %+v", dim, c)
		}
	}
	if v.Status != stlapi.ResultReviewed || len(v.Snapshots) != 0 || len(v.Reviews) != 1 || v.Live.Confirmed.ResultMinor != "300000" {
		t.Fatalf("after BT18 %+v", v)
	}

	w.saFreightBilled()
	w.ended("order.sa", "order.close")
	c, err := w.close(w.treasury, "order.sa", 1)
	w.must(err)
	if c.Status != stlapi.ResultSettled || c.RowVersion != 2 || c.SnapshotSeq != 1 || c.ConfirmedResultMinor != "235000" || c.Margin != "33.57" {
		t.Fatalf("close %+v", c)
	}
	v = w.result("order.sa")
	saFigures(t, "live", v.Live, "65000", "0", "235000")
	if v.Status != stlapi.ResultSettled || v.Version != 2 || !v.Settleable || len(v.Snapshots) != 1 || len(v.Snapshots[0].ContentSHA256) != 64 ||
		v.Snapshots[0].ClosedBy.ID != "treasury.1" || v.Snapshots[0].Reopened || v.OrderEnded != "order.close" {
		t.Fatalf("settled %+v", v)
	}
	s := v.Snapshots[0]
	saFigures(t, "snapshot 1", s.Figures, "65000", "0", "235000")
	if s.OrderID != "order.sa" || s.OrderRevision != 1 || conditionOf(s.Conditions, stlapi.DimensionClaims).Items[0] !=
		"clm: not_enabled — CLM is not enabled in S5; no open return or claim can be checked" {
		t.Fatalf("snapshot %+v", s)
	}
	for _, d := range []string{stlapi.DimensionFulfilment, stlapi.DimensionReceivables, stlapi.DimensionPayables, stlapi.DimensionPurchaseCosts,
		stlapi.DimensionDirectCharges} {
		if conditionOf(s.Conditions, d).Status != stlapi.ConditionMet {
			t.Fatalf("snapshot condition %s %+v", d, s.Conditions)
		}
	}
	var allocated, closed []map[string]any
	for _, e := range w.events("company.A", stlapi.Source) {
		switch e.Type {
		case stlapi.EventChargeAllocated:
			allocated = append(allocated, e.Payload)
		case stlapi.EventOrderResultClosed:
			closed = append(closed, e.Payload)
		}
	}
	if len(allocated) != 1 || allocated[0]["charge_id"] != "freight.sa" || allocated[0]["allocation_seq"] != float64(1) ||
		len(allocated[0]["shipment_ids"].([]any)) != 2 || len(closed) != 1 || closed[0]["order_id"] != "order.sa" ||
		closed[0]["snapshot_seq"] != float64(1) || closed[0]["claims"] != "not_enabled" || closed[0]["content_sha256"] != s.ContentSHA256 {
		t.Fatalf("events %v %v", allocated, closed)
	}
	ch, err := w.stl.GetCharge(w.treasury, "freight.sa")
	w.must(err)
	if ch.Status != stlapi.ChargeConfirmed || ch.EstimateMinor != "50000" || ch.AmountMinor != "65000" || ch.EstimateReference != "OFFER-SA" ||
		ch.Source.Reference != "BILL-SA" || ch.OccurredOn != "2026-11-10" || len(ch.Allocations) != 1 || !ch.Allocations[0].Current ||
		ch.ConfirmedBy.ID != "treasury.1" || ch.RecordedBy.ID != "officer.1" || ch.BilledBy.ID != "officer.1" {
		t.Fatalf("charge %+v", ch)
	}
}

// BT24 (基线 9.4 "订单关闭后补入次月运费，并尝试改写上期快照"; STL-INV-011, MD-49): after S-A's result is settled, next
// month's freight of CNY 100.00 is recorded (occurred and analysed on 2026-12-05, recorded now) — allowed, and the
// live view says the result no longer settles. Confirming or allocating it is INVALID_STATE until the result is
// reopened; the AI agent cannot reopen (human only); the treasurer reopens with a reason, the charge is confirmed
// and allocated to the order, the result is reviewed and closed again: snapshot 2 says CNY 2,250.00, snapshot 1
// still says 2,350.00 with the same SHA-256; the database refuses to change or delete a snapshot.
func TestBT24LateFreightReopensTheResult(t *testing.T) {
	w := newResultWorld(t)
	first := w.saClosed().Snapshots[0]
	late := w.billOf("freight.dec", 0, "BILL-DEC", "100.00", "2026-12-05")
	fee, cny := stlRef(w.freight), stlRef(w.cny)
	late.Beneficiaries, late.FeeCategory, late.Currency = []stlapi.ChargeTarget{orderTarget("order.sa")}, &fee, &cny
	late.Conversion = &stlapi.ChargeConversion{Kind: stlapi.ConversionSameCurrency}
	r, err := w.record(w.officer, late)
	w.must(err)
	if r.Status != stlapi.ChargePending || r.EstimateMinor != "" {
		t.Fatalf("late freight %+v", r)
	}
	v := w.result("order.sa")
	if v.Status != stlapi.ResultSettled || v.Settleable || conditionOf(v.Conditions, stlapi.DimensionDirectCharges).Status != stlapi.ConditionUnmet ||
		v.Snapshots[0].ContentSHA256 != first.ContentSHA256 || v.Live.Executing.ResultMinor != "225000" || v.Live.Confirmed.ResultMinor != "235000" {
		t.Fatalf("settled with a late charge %+v", v)
	}
	_, err = w.confirmCharge(w.treasury, "freight.dec", 1)
	wantRefused(t, "confirm while settled", err, kernel.CodeInvalidState, "/args/charge_id")
	_, err = w.allocate(w.officer, "freight.dec", 1, stlapi.AllocationByAmount, stlapi.AllocationLine{LineID: "A", Target: orderTarget("order.sa"),
		Weight: "1"})
	wantRefused(t, "allocate while settled", err, kernel.CodeInvalidState, "/args/charge_id")
	reopen := stlapi.ReopenOrderResult{IdempotencyKey: w.key(), OrderID: "order.sa", ExpectedVersion: 2, Reason: "次月运费补录：BILL-DEC",
		EvidenceIDs: []string{w.bill}}
	_, err = w.stl.ReopenOrderResult(w.agent, reopen)
	w.want(err, kernel.CodePermissionDenied)
	reopen.IdempotencyKey = w.key()
	o, err := w.stl.ReopenOrderResult(w.treasury, reopen)
	w.must(err)
	if o.Status != stlapi.ResultOpen || o.RowVersion != 3 || o.SnapshotSeq != 1 || o.ConfirmedResultMinor != "235000" {
		t.Fatalf("reopen %+v", o)
	}
	w.must2(w.confirmCharge(w.treasury, "freight.dec", 1))
	w.must2(w.allocate(w.officer, "freight.dec", 2, stlapi.AllocationByAmount, stlapi.AllocationLine{LineID: "A", Target: orderTarget("order.sa"),
		Weight: "1"}))
	_, err = w.close(w.treasury, "order.sa", 3)
	wantRefused(t, "close without a review", err, kernel.CodeInvalidState, "/args/order_id")
	w.must2(w.review(w.treasury, "order.sa", 3))
	c, err := w.close(w.treasury, "order.sa", 4)
	w.must(err)
	if c.RowVersion != 5 || c.SnapshotSeq != 2 || c.ConfirmedResultMinor != "225000" {
		t.Fatalf("second close %+v", c)
	}
	v = w.result("order.sa")
	if len(v.Snapshots) != 2 || len(v.Reopenings) != 1 || v.Reopenings[0].Seq != 1 || v.Reopenings[0].Reason != reopen.Reason || len(v.Reviews) != 2 ||
		!v.Snapshots[0].Reopened || v.Snapshots[1].Reopened || v.Snapshots[0].ContentSHA256 != first.ContentSHA256 {
		t.Fatalf("after BT24 %+v", v)
	}
	saFigures(t, "snapshot 1 kept", v.Snapshots[0].Figures, "65000", "0", "235000")
	saFigures(t, "snapshot 2", v.Snapshots[1].Figures, "75000", "10000", "225000")
	var reopened []map[string]any
	for _, e := range w.events("company.A", stlapi.Source) {
		if e.Type == stlapi.EventOrderResultReopened {
			reopened = append(reopened, e.Payload)
		}
	}
	if len(reopened) != 1 || reopened[0]["snapshot_seq"] != float64(1) || reopened[0]["order_result_version"] != float64(3) {
		t.Fatalf("OrderResultReopened %v", reopened)
	}
	for name, sql := range map[string]string{
		"overwrite snapshot 1":             `UPDATE brant_stl.order_result_snapshot SET result_minor = 0 WHERE order_id = 'order.sa' AND seq = 1`,
		"delete snapshot 1":                `DELETE FROM brant_stl.order_result_snapshot WHERE order_id = 'order.sa' AND seq = 1`,
		"a third close without a snapshot": `UPDATE brant_stl.order_result SET status = 'open', version = 6 WHERE order_id = 'order.sa'`,
	} {
		if err := w.exec(sql); err == nil {
			t.Errorf("%s: committed", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
	if got := w.sum(`SELECT result_minor::text FROM brant_stl.order_result_snapshot WHERE order_id = 'order.sa' AND seq = 1`); got != "235000" {
		t.Fatalf("snapshot 1 now %s", got)
	}
}

// STL-INV-010's negative example and the commands' refusals: weights summing to zero and a negative weight are
// ALLOCATION_INVALID, a target outside the charge's orders INVALID_KNOWN_VALUE, a stale version VERSION_CONFLICT; a
// beneficiary FUL does not show is REFERENCE_INVALID, a draft INVALID_STATE, a conversion basis that does not lead
// into the order's analysis currency FX_SCOPE_MISMATCH, a bill recorded twice BUSINESS_KEY_CONFLICT. The AI agent
// records and allocates (human or AI) and reviews, but never confirms or closes (human only). A negative adjustment
// of CNY −0.05 over 1 : 1 is −0.03 and −0.02 (largest remainder on the absolute value, ties by line ID, signed). A
// USD charge converts at the order's analysis rate or at a POL settlement rate USD→CNY (recorded with its revision,
// hash and value; ConfigGuard v1 in the transaction); a credit-purpose rate is FX_SCOPE_MISMATCH.
func TestChargeRefusalsAndActors(t *testing.T) {
	w := newResultWorld(t)
	o := w.saOrder()
	w.ful.shipped(o, "shipment.a", "50", "50000", time.Now().Add(-time.Hour))
	w.ful.shipped(o, "shipment.b", "50", "50000", time.Now().Add(-time.Hour))
	draft := stl.Order{Binding: stl.OrderBinding{OrderID: "order.draft", OrderRevision: 1, OrderHash: orderHashOf("order.draft", 1)},
		Status: stl.OrderDraft, CustomerID: "customer.1", Currency: stl.CurrencyRef{ID: w.usd.ConfigID, Revision: w.usd.Revision}}
	w.orders.put(draft)
	both := []stlapi.ChargeTarget{shipmentTarget("order.sa", "shipment.a"), shipmentTarget("order.sa", "shipment.b")}
	r, err := w.record(w.agent, w.estimate("charge.ai", "OFFER-AI", "-0.05", both...))
	w.must(err)
	if r.AmountMinor != "-5" || r.Status != stlapi.ChargeEstimated {
		t.Fatalf("AI records %+v", r)
	}
	for name, x := range map[string]struct {
		cmd        stlapi.RecordCharge
		code, path string
	}{
		"FUL shows no such shipment": {w.estimate("c.1", "O1", "1.00", shipmentTarget("order.sa", "shipment.x")), kernel.CodeReferenceInvalid,
			"/args/beneficiaries/0/shipment_id"},
		"TRD shows no such order": {w.estimate("c.2", "O2", "1.00", orderTarget("order.none")), kernel.CodeReferenceInvalid,
			"/args/beneficiaries/0/order_id"},
		"a draft order":   {w.estimate("c.3", "O3", "1.00", orderTarget("order.draft")), kernel.CodeInvalidState, "/args/beneficiaries/0/order_id"},
		"the offer again": {w.estimate("c.4", "OFFER-AI", "1.00", orderTarget("order.sa")), stlapi.CodeBusinessKeyConflict, "/args/source/reference"},
		"a USD charge as the same currency": {func() stlapi.RecordCharge {
			c := w.estimate("c.5", "O5", "1.00", orderTarget("order.sa"))
			usd := stlRef(w.usd)
			c.Currency = &usd
			return c
		}(), stlapi.CodeFXScopeMismatch, "/args/conversion"},
	} {
		_, err := w.record(w.officer, x.cmd)
		wantRefused(t, name, err, x.code, x.path)
	}
	usdCharge := w.estimate("c.usd", "O-USD", "10.00", orderTarget("order.sa"))
	usd := stlRef(w.usd)
	usdCharge.Currency, usdCharge.Conversion = &usd, &stlapi.ChargeConversion{Kind: stlapi.ConversionOrderAnalysis}
	w.must2(w.record(w.officer, usdCharge))
	settlement := stlRef(w.fxRate("usd.cny.sa", "USD", "CNY", "6.9", polapi.PurposeSettlement))
	credit := stlRef(w.fxRate("usd.cny.credit", "USD", "CNY", "6.9", polapi.PurposeCredit))
	atRate := func(id string, rate stlapi.ConfigRef) stlapi.RecordCharge {
		c := w.estimate(id, "O-"+id, "10.00", orderTarget("order.sa"))
		c.Currency, c.Conversion = &usd, &stlapi.ChargeConversion{Kind: stlapi.ConversionFXRate, FXRate: &rate}
		return c
	}
	_, err = w.record(w.officer, atRate("c.credit", credit))
	wantRefused(t, "a credit rate", err, stlapi.CodeFXScopeMismatch, "/args/conversion/fx_rate")
	fx, err := w.record(w.officer, atRate("c.fx", settlement))
	w.must(err)
	if ch, err := w.stl.GetCharge(w.officer, fx.ObjectID); err != nil || ch.Conversion.Rate != "6.9" || ch.Conversion.To != "CNY" ||
		ch.Conversion.FXRate == nil || ch.Conversion.FXRate.ConfigID != "usd.cny.sa" || len(ch.Conversion.FXRateHash) != 64 {
		t.Fatalf("charge at a settlement rate %+v %v", ch, err)
	}

	line := func(id, shipment, weight string) stlapi.AllocationLine {
		return stlapi.AllocationLine{LineID: id, Target: shipmentTarget("order.sa", shipment), Weight: weight}
	}
	for name, x := range map[string]struct {
		lines      []stlapi.AllocationLine
		version    int64
		code, path string
	}{
		"zero weights":    {[]stlapi.AllocationLine{line("A", "shipment.a", "0"), line("B", "shipment.b", "0.000000")}, 1, stlapi.CodeAllocationInvalid, "/args/lines"},
		"a negative":      {[]stlapi.AllocationLine{line("A", "shipment.a", "1"), line("B", "shipment.b", "-1")}, 1, stlapi.CodeAllocationInvalid, "/args/lines/1/weight"},
		"no line":         {nil, 1, stlapi.CodeAllocationInvalid, "/args/lines"},
		"another order":   {[]stlapi.AllocationLine{{LineID: "A", Target: orderTarget("order.draft"), Weight: "1"}}, 1, kernel.CodeInvalidKnownValue, "/args/lines/0/target"},
		"a stale version": {[]stlapi.AllocationLine{line("A", "shipment.a", "1")}, 2, kernel.CodeVersionConflict, "/args/expected_version"},
	} {
		_, err := w.allocate(w.officer, "charge.ai", x.version, stlapi.AllocationByWeight, x.lines...)
		wantRefused(t, name, err, x.code, x.path)
	}
	a, err := w.allocate(w.agent, "charge.ai", 1, stlapi.AllocationByWeight, line("B", "shipment.b", "1"), line("A", "shipment.a", "1"))
	w.must(err)
	if a.RowVersion != 2 || a.Lines[0].LineID != "B" || a.Lines[0].AmountMinor != "-2" || a.Lines[1].AmountMinor != "-3" {
		t.Fatalf("negative adjustment %+v", a)
	}
	_, err = w.confirmCharge(w.agent, "charge.ai", 2)
	w.want(err, kernel.CodePermissionDenied)
	w.must2(w.review(w.agent, "order.sa", 0))
	_, err = w.close(w.agent, "order.sa", 1)
	w.want(err, kernel.CodePermissionDenied)
	v := w.result("order.sa")
	if v.Status != stlapi.ResultReviewed || v.Reviews[0].ReviewedBy.ID != "agent.c9" ||
		v.Live.Forecast.DirectChargesMinor != "13895" || v.Live.Forecast.ResultMinor != "286105" {
		// 10.00 USD at the analysis rate 7.0 = CNY 70.00, 10.00 USD at the settlement rate 6.9 = CNY 69.00, less 0.05.
		t.Fatalf("result with the AI's charges %+v", v.Live.Forecast)
	}
	for name, sql := range map[string]string{
		"an allocation that does not add up": `INSERT INTO brant_stl.charge_allocation (tenant_id, company_id, charge_id, allocation_seq, charge_version,
			basis, data_basis, evidence_ids, currency, minor_units, amount_minor, allocated_by_kind, allocated_by_id, allocate_epoch, allocated_at)
			SELECT tenant_id, company_id, charge_id, 2, 3, 'amount', 'x', '{}', currency, minor_units, -5, 'human', 'officer.1', 1, now()
			FROM brant_stl.charge WHERE charge_id = 'charge.ai'; UPDATE brant_stl.charge SET version = 3, allocation_seq = 2 WHERE charge_id = 'charge.ai'`,
		"zero weights": `INSERT INTO brant_stl.charge_allocation (tenant_id, company_id, charge_id, allocation_seq, charge_version, basis, data_basis,
			evidence_ids, currency, minor_units, amount_minor, allocated_by_kind, allocated_by_id, allocate_epoch, allocated_at) SELECT tenant_id,
			company_id, charge_id, 2, 3, 'amount', 'x', '{}', currency, minor_units, -5, 'human', 'officer.1', 1, now() FROM brant_stl.charge
			WHERE charge_id = 'charge.ai'; INSERT INTO brant_stl.charge_allocation_line (tenant_id, company_id, charge_id, allocation_seq, line_id, seq,
			target_kind, order_id, weight, amount_minor) SELECT tenant_id, company_id, charge_id, 2, 'A', 1, 'order', 'order.sa', 0, -5
			FROM brant_stl.charge WHERE charge_id = 'charge.ai'; UPDATE brant_stl.charge SET version = 3, allocation_seq = 2 WHERE charge_id = 'charge.ai'`,
		"confirmed without its bill":          `UPDATE brant_stl.charge SET status = 'confirmed', version = 3 WHERE charge_id = 'charge.ai'`,
		"a settled result without a snapshot": `UPDATE brant_stl.order_result SET status = 'settled', version = 2 WHERE order_id = 'order.sa'`,
		"a line changed":                      `UPDATE brant_stl.charge_allocation_line SET amount_minor = -5 WHERE charge_id = 'charge.ai'`,
	} {
		if err := w.exec(strings.Split(sql, "; ")...); err == nil {
			t.Errorf("%s: committed", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
}

// must2 fails the test on a command's error.
func (w *resultWorld) must2(_ any, err error) {
	w.t.Helper()
	w.must(err)
}

// The lock anchor (STL-INV-011 "关闭快照不被覆盖"): order_result.close locks the result FOR UPDATE, charge.allocate
// and charge.confirm lock the results of the charge's orders FOR SHARE. A charge of CNY 1.00 benefits race.N and
// other.N, allocated 1 : 1; while the treasurer closes race.N, the credit officer re-allocates it 1 : 0. Over five
// rounds either the allocation commits first and the snapshot counts race.N's 1.00 (100 minor), or the close commits
// first and the allocation is INVALID_STATE (settled: reopen first) — never a settled snapshot of 0.50 beside a
// committed allocation of 1.00.
func TestCloseAndALateAllocationOneWins(t *testing.T) {
	w := newResultWorld(t)
	wins := map[string]int{}
	for i := range 5 {
		race, other, id := "race."+string(rune('a'+i)), "other."+string(rune('a'+i)), "charge.race."+string(rune('a'+i))
		for _, o := range []string{race, other} {
			x := w.activeOrder(o, "customer.1", w.usd, payStage("P1", "after_shipment", "10000"))
			x.Analysis = &stl.OrderAnalysis{OrderCurrency: "USD", Currency: "CNY", Rate: "7.0", CurrencyRevision: w.cny.Revision}
			x.NetMinor = "10000"
			w.orders.put(x)
			w.ended(o, "order.close")
			w.pur.mu.Lock()
			w.pur.commitments[o] = []stl.OrderPurchaseLine{}
			w.pur.mu.Unlock()
		}
		c := w.estimate(id, "OFFER-RACE-"+id, "1.00", orderTarget(race), orderTarget(other))
		c.Status, c.EvidenceIDs = stlapi.ChargePending, []string{w.bill}
		w.must2(w.record(w.officer, c))
		w.must2(w.confirmCharge(w.treasury, id, 1))
		w.must2(w.allocate(w.officer, id, 2, stlapi.AllocationByAmount, stlapi.AllocationLine{LineID: "A", Target: orderTarget(race), Weight: "1"},
			stlapi.AllocationLine{LineID: "B", Target: orderTarget(other), Weight: "1"}))
		w.must2(w.review(w.treasury, race, 0))
		var wg sync.WaitGroup
		var closeErr, allocErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, closeErr = w.close(w.treasury, race, 1)
		}()
		go func() {
			defer wg.Done()
			_, allocErr = w.allocate(w.officer, id, 3, stlapi.AllocationByAmount, stlapi.AllocationLine{LineID: "A", Target: orderTarget(race), Weight: "1"},
				stlapi.AllocationLine{LineID: "B", Target: orderTarget(other), Weight: "0"})
		}()
		wg.Wait()
		w.must(closeErr)
		snap := w.result(race).Snapshots[0].Figures.Confirmed
		switch {
		case allocErr == nil && snap.DirectChargesMinor == "100":
			wins["allocation first"]++
		case code(allocErr) == kernel.CodeInvalidState && snap.DirectChargesMinor == "50":
			wins["close first"]++
		default:
			t.Fatalf("round %d: allocation %v, snapshot charges %s", i, allocErr, snap.DirectChargesMinor)
		}
	}
	t.Logf("winners over five rounds: %v", wins)
}
