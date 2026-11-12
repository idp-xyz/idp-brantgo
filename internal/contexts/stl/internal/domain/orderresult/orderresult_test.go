package orderresult

import (
	"strings"
	"testing"
	"time"

	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/charge"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Unit tests of the order result (baseline 4.5 definitions, 4.6 S-A numbers, STL-INV-011, BT18, BT24). Python
// V0.50 has no STL.

var (
	usd = currency("USD")
	cny = currency("CNY")
	at  = time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	by  = fundrecord.Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "treasury.1"}, Epoch: 1, At: at}
)

func currency(code string) kernel.Currency {
	c, err := kernel.NewCurrency(code, 2)
	if err != nil {
		panic(err)
	}
	return c
}

func m(c kernel.Currency, minor int64) kernel.Money {
	x, err := kernel.NewMoney(c, minor)
	if err != nil {
		panic(err)
	}
	return x
}

func dec(s string) kernel.Decimal { return kernel.MustDecimal(s) }

// sales is a standing sales statement of order.sa settling line S1 of shipment, quantity of L1 at USD 10.00.
func sales(id, shipment string, quantity int64) *settlement.Statement {
	net := m(usd, quantity*1000)
	return &settlement.Statement{ID: id, Kind: settlement.KindSales, Status: settlement.StatusConfirmed, Version: 1, OrderID: "order.sa",
		Net: net, Tax: m(usd, 0), Amount: net, Lines: []settlement.Line{{Seq: 1, Source: settlement.SourceKey{Kind: settlement.SourceShipmentLine,
			ID: shipment, LineID: "S1"}, OrderID: "order.sa", OrderLineID: "L1", Quantity: kernel.DecimalFromInt(quantity), Net: net, Tax: m(usd, 0),
			Amount: net}}, Installments: []settlement.Installment{{Seq: 1, Amount: net}}}
}

// purchase is the standing purchase statement of delivery.1: 100 PCS of L1 at CNY 40.00 from commitment.1.
func purchase() *settlement.Statement {
	net := m(cny, 400000)
	return &settlement.Statement{ID: "statement.purchase", Kind: settlement.KindPurchase, Status: settlement.StatusConfirmed, Version: 1,
		Net: net, Tax: m(cny, 0), Amount: net, Lines: []settlement.Line{{Seq: 1, Source: settlement.SourceKey{Kind: settlement.SourceDeliveryLine,
			ID: "delivery.1", LineID: "L1"}, OrderID: "order.sa", OrderLineID: "L1", CommitmentID: "commitment.1", Quantity: kernel.DecimalFromInt(100),
			Net: net, Tax: m(cny, 0), Amount: net}}, Installments: []settlement.Installment{{Seq: 1, Amount: net}}}
}

// receipt is a standing application of a CNY receipt to a USD target at the settlement rate, with its difference
// against the analysis rate 7.0.
func receipt(id, target, statement, stage string, cnyMinor, usdMinor, diffMinor int64) fa.Application {
	d := m(cny, diffMinor)
	return fa.Application{ID: id, FundRecordID: "fund." + id, TargetKind: target, OrderID: "order.sa", StageID: stage, StatementID: statement,
		Amount: m(usd, usdMinor), Status: fa.StatusApplied, Version: 1, Conversion: &fa.Conversion{Source: m(cny, cnyMinor),
			Analysis: &fa.Analysis{Currency: cny, Rate: dec("7"), Difference: &d}}}
}

// freight is S-A's freight: estimated at CNY 500, billed at 650, confirmed, allocated by quantity 60 / 40.
func freight(t *testing.T) *charge.Charge {
	t.Helper()
	s60 := charge.Target{Kind: charge.TargetShipment, OrderID: "order.sa", ShipmentID: "shipment.60"}
	s40 := charge.Target{Kind: charge.TargetShipment, OrderID: "order.sa", ShipmentID: "shipment.40"}
	c, err := charge.Record(charge.RecordInput{ID: "freight.sa", Status: charge.StatusEstimated, Source: charge.Source{Kind: charge.SourceLogisticsOffer,
		PartyID: "forwarder.1", Reference: "OFFER-1"}, Amount: m(cny, 50000), Conversion: charge.Conversion{Kind: charge.ConversionSameCurrency},
		OccurredOn: at, AnalysisOn: at, Basis: "预计运费", Beneficiaries: []charge.Target{s60, s40}, By: by})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Bill(charge.BillInput{Source: charge.Source{Kind: charge.SourceSupplierBill, PartyID: "forwarder.1", Reference: "BILL-1"},
		Amount: m(cny, 65000), OccurredOn: at, AnalysisOn: at, Basis: "实际运费", EvidenceIDs: []string{"bill"}, By: by}); err != nil {
		t.Fatal(err)
	}
	if err = c.Confirm(by, []string{"bill"}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Allocate(charge.AllocateInput{Basis: charge.BasisQuantity, DataBasis: "60 / 40 PCS shipped", By: by,
		Allowed: map[string]bool{s60.Key(): true, s40.Key(): true},
		Lines:   []charge.LineInput{{LineID: "A", Target: s60, Weight: "60"}, {LineID: "B", Target: s40, Weight: "40"}}}); err != nil {
		t.Fatal(err)
	}
	return c
}

// saInput is baseline 4.6 S-A at step 7: 100 PCS at USD 10 (analysis rate 7.0 CNY per USD), prepaid CNY 1,400 =
// USD 200 moved to the first receivable, shipments of 60 and 40 settled at USD 600 + 400, CNY 5,500 received for
// USD 800 (2,750 for each 400), purchase CNY 40 × 100 = 4,000 settled and paid, freight estimated 500, actual 650
// confirmed and allocated 390 + 260; TRD closed the order complete.
func saInput(t *testing.T) Input {
	s600, s400, p := sales("statement.600", "shipment.60", 60), sales("statement.400", "shipment.40", 40), purchase()
	pay := fa.Application{ID: "app.pay", FundRecordID: "fund.pay", TargetKind: fa.TargetPayable, StatementID: p.ID, InstallmentSeq: 1,
		Amount: m(cny, 400000), Status: fa.StatusApplied, Version: 1}
	return Input{At: at, Order: Order{ID: "order.sa", Revision: 1, Hash: strings.Repeat("a", 64), Status: OrderClosed, Ended: EndClose,
		Currency: usd, Net: m(usd, 100000), Analysis: &Analysis{Currency: cny, Rate: dec("7.0")}},
		Sales: []*settlement.Statement{s600, s400}, Purchases: []*settlement.Statement{p},
		Applied: map[string]map[int]kernel.Money{s600.ID: {1: m(usd, 60000)}, s400.ID: {1: m(usd, 40000)}, p.ID: {1: m(cny, 400000)}},
		Applications: []fa.Application{
			receipt("app.pre", fa.TargetReceivable, s600.ID, "P1", 140000, 20000, 0),
			receipt("app.600", fa.TargetReceivable, s600.ID, "P2", 275000, 40000, -5000),
			receipt("app.400", fa.TargetReceivable, s400.ID, "P2", 275000, 40000, -5000)},
		PayableApplications: map[string][]fa.Application{p.ID: {pay}},
		Charges:             []*charge.Charge{freight(t)},
		Shipments: []ShipmentLine{{ShipmentID: "shipment.60", LineID: "S1", Status: ShipmentConfirmed, OrderLineID: "L1", Quantity: dec("60")},
			{ShipmentID: "shipment.40", LineID: "S1", Status: ShipmentConfirmed, OrderLineID: "L1", Quantity: dec("40")}},
		ShipmentsShown: true,
		Commitments: []CommitmentLine{{CommitmentID: "commitment.1", OrderLineID: "L1", Status: "confirmed", Net: m(cny, 400000),
			Deliveries: []Delivery{{DeliveryID: "delivery.1", LineID: "L1", Version: 1}}}},
		CommitmentsShown: true}
}

func is(t *testing.T, what string, got kernel.Money, want string) {
	t.Helper()
	if got.String() != want {
		t.Fatalf("%s: %s, want %s", what, got, want)
	}
}

// Baseline 4.6 S-A exactly (STL-AGG-OrderResult; baseline 4.5 definitions): forecast CNY 2,500 (7,000 − 4,000 −
// the estimated freight 500); confirmed CNY 2,350 = 1,410 + 940 (4,200 − 2,400 − 390 and 2,800 − 1,600 − 260: the
// purchase cost by the 60 / 40 sold, the freight by its allocation); actual receipts CNY 6,900 against the
// analysis revenue 7,000: the business settlement FX difference −100, apart; net cash 6,900 − 4,000 paid for the
// purchase = 2,900 (the freight is not paid through STL: S-A's 2,250 counts it paid). Every close condition but
// CLM is met; CLM is not_enabled, never met.
func TestBaseline46SAOrderResult(t *testing.T) {
	f, dims := Compute(saInput(t))
	if f.Currency != cny || f.Rate != "7" {
		t.Fatalf("result currency %s at %s", f.Currency, f.Rate)
	}
	fc := f.Forecast
	is(t, "forecast net sales", fc.NetSales, "7000.00 CNY")
	is(t, "forecast purchase", fc.PurchaseCost, "4000.00 CNY")
	is(t, "forecast charges (the estimate)", fc.DirectCharges, "500.00 CNY")
	is(t, "forecast result", fc.Result, "2500.00 CNY")
	if fc.Margin != "35.71" || len(fc.Gaps) != 0 {
		t.Fatalf("forecast margin %s gaps %v", fc.Margin, fc.Gaps)
	}
	ex := f.Executing
	is(t, "executing confirmed purchase", ex.PurchaseConfirmed, "4000.00 CNY")
	is(t, "executing estimated rest", ex.PurchaseEstimated, "0.00 CNY")
	is(t, "executing charges", ex.ChargesConfirmed, "650.00 CNY")
	is(t, "executing result", ex.Result, "2350.00 CNY")
	c := f.Confirmed
	is(t, "confirmed net sales", c.NetSales, "7000.00 CNY")
	is(t, "confirmed cost of sales", c.PurchaseCost, "4000.00 CNY")
	is(t, "confirmed charges", c.DirectCharges, "650.00 CNY")
	is(t, "confirmed result", c.Result, "2350.00 CNY")
	if len(c.Batches) != 2 || c.Margin != "33.57" || !c.OrderLevelCharges.IsZero() || !c.UnsoldPurchaseCost.IsZero() || len(c.Gaps) != 0 {
		t.Fatalf("confirmed %+v", c)
	}
	for _, x := range []struct {
		b                           Batch
		id, net, cost, charges, res string
	}{{c.Batches[0], "shipment.40", "2800.00 CNY", "1600.00 CNY", "260.00 CNY", "940.00 CNY"},
		{c.Batches[1], "shipment.60", "4200.00 CNY", "2400.00 CNY", "390.00 CNY", "1410.00 CNY"}} {
		if x.b.ShipmentID != x.id || x.b.NetSales.String() != x.net || x.b.CostOfSales.String() != x.cost || x.b.DirectCharges.String() != x.charges ||
			x.b.Result.String() != x.res {
			t.Fatalf("batch %+v, want %s %s − %s − %s = %s", x.b, x.id, x.net, x.cost, x.charges, x.res)
		}
	}
	is(t, "FX difference", f.FX.Difference, "-100.00 CNY")
	if len(f.FX.Items) != 3 || len(f.FX.Gaps) != 0 {
		t.Fatalf("FX %+v", f.FX)
	}
	is(t, "receipts", f.Cash.Receipts, "6900.00 CNY")
	is(t, "payments", f.Cash.Payments, "4000.00 CNY")
	is(t, "net cash", f.Cash.Net, "2900.00 CNY")
	want := []string{"fulfilment met", "receivables met", "payables met", "purchase_costs met", "direct_charges met", "claims not_enabled"}
	for i, d := range dims {
		if d.Name+" "+d.Status != want[i] {
			t.Fatalf("condition %d %+v, want %s", i, d, want[i])
		}
	}
	if err := NotSettled(dims); err != nil {
		t.Fatalf("S-A at step 7 does not settle: %v", err)
	}
}

// BT18 (基线 9.3 "发货完成但有未结费用或未决事项"): everything shipped, but a charge pending, a receivable open and the
// order still active — close is ORDER_RESULT_NOT_SETTLED with every unmet dimension listed, not a pass because the
// goods left; CLM stays not_enabled.
func TestBT18ShippedButNotSettled(t *testing.T) {
	in := saInput(t)
	in.Order.Status, in.Order.Ended = "active", ""
	in.Applications = in.Applications[:2]
	in.Applied["statement.400"] = map[int]kernel.Money{}
	late, err := charge.Record(charge.RecordInput{ID: "inspection", Status: charge.StatusPending, Source: charge.Source{Kind: charge.SourceInternal},
		Amount: m(cny, 12000), Conversion: charge.Conversion{Kind: charge.ConversionSameCurrency}, OccurredOn: at, AnalysisOn: at, Basis: "验货费",
		Beneficiaries: []charge.Target{{Kind: charge.TargetOrder, OrderID: "order.sa"}}, By: by})
	if err != nil {
		t.Fatal(err)
	}
	in.Charges = append(in.Charges, late)
	f, dims := Compute(in)
	err = NotSettled(dims)
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != CodeOrderResultNotSettled || rej.Category != kernel.CategoryConflict || len(rej.Fields) != 3 ||
		rej.Fields[0].Path != "/conditions/fulfilment" || rej.Fields[1].Path != "/conditions/receivables" ||
		!strings.Contains(rej.Fields[1].Reason, "receivable statement.400 open 400.00 USD") || rej.Fields[2].Path != "/conditions/direct_charges" ||
		!strings.Contains(rej.Fields[2].Reason, "charge inspection is pending") {
		t.Fatalf("BT18 refusal %v", err)
	}
	if dims[5].Status != NotEnabled {
		t.Fatalf("CLM %+v", dims[5])
	}
	is(t, "executing counts the pending charge", f.Executing.ChargesEstimated, "120.00 CNY")
	is(t, "the confirmed result does not", f.Confirmed.DirectCharges, "650.00 CNY")
	r := &Result{OrderID: "order.sa", Status: StatusReviewed, Version: 1}
	if err := r.Close(dims, Snapshot{}); err == nil || r.Status != StatusReviewed || len(r.Snapshots) != 0 {
		t.Fatalf("closed with BT18's conditions: %v %+v", err, r)
	}
}

// SM-STL-ORDER-RESULT and BT24: open → reviewed → settled (snapshot 1) → open (reopening of 1, the snapshot kept)
// → reviewed → settled (snapshot 2); a close before a review, a review of a settled result and a reopen of an
// open one are INVALID_STATE.
func TestReviewCloseReopenKeepsEverySnapshot(t *testing.T) {
	_, dims := Compute(saInput(t))
	r := New("order.sa", "treasury.1", "", at)
	if err := r.Close(dims, Snapshot{}); err == nil {
		t.Fatal("closed without a review")
	}
	if err := r.Reopen(Reopening{}); err == nil {
		t.Fatal("reopened an open result")
	}
	steps := []func() error{
		func() error { return r.Review(Review{OrderRevision: 1, By: by}) },
		func() error { return r.Close(dims, Snapshot{ContentSHA256: "first", By: by}) },
		func() error { return r.Review(Review{By: by}) },
		func() error { return r.Reopen(Reopening{Reason: "次月运费补录", By: by}) },
		func() error { return r.Review(Review{By: by}) },
		func() error { return r.Close(dims, Snapshot{ContentSHA256: "second", By: by}) },
	}
	wantErr := []bool{false, false, true, false, false, false}
	for i, step := range steps {
		if err := step(); (err != nil) != wantErr[i] {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if r.Status != StatusSettled || r.Version != 5 || len(r.Snapshots) != 2 || r.Snapshots[0].ContentSHA256 != "first" || r.Snapshots[0].Seq != 1 ||
		r.Latest().Seq != 2 || len(r.Reopenings) != 1 || r.Reopenings[0].Seq != 1 || len(r.Reviews) != 2 {
		t.Fatalf("after BT24 %+v", r)
	}
}

// BRC-001 (S5 plan §5 D1 step 27) in its own currency: 960.00 − 600.00 − 60.00 = 300.00, 31.25 %; the freight
// allocated to the order is the order's, not a batch's.
func TestBRC001ConfirmedResult(t *testing.T) {
	st := &settlement.Statement{ID: "statement.sales", Kind: settlement.KindSales, Status: settlement.StatusConfirmed, Version: 1, OrderID: "order.1",
		Net: m(usd, 96000), Amount: m(usd, 96000), Lines: []settlement.Line{{Seq: 1, Source: settlement.SourceKey{Kind: settlement.SourceShipmentLine,
			ID: "shipment.1", LineID: "S1"}, OrderID: "order.1", OrderLineID: "L1", Quantity: dec("120"), Net: m(usd, 96000), Amount: m(usd, 96000)}}}
	p := &settlement.Statement{ID: "statement.purchase", Kind: settlement.KindPurchase, Status: settlement.StatusConfirmed, Version: 1,
		Net: m(usd, 60000), Amount: m(usd, 60000), Lines: []settlement.Line{{Seq: 1, Source: settlement.SourceKey{Kind: settlement.SourceDeliveryLine,
			ID: "delivery.1", LineID: "L1"}, OrderID: "order.1", OrderLineID: "L1", CommitmentID: "commitment.1", Quantity: dec("120"),
			Net: m(usd, 60000), Amount: m(usd, 60000)}}}
	order := charge.Target{Kind: charge.TargetOrder, OrderID: "order.1"}
	fr, err := charge.Record(charge.RecordInput{ID: "freight.1", Status: charge.StatusEstimated, Source: charge.Source{Kind: charge.SourceInternal},
		Amount: m(usd, 6000), Conversion: charge.Conversion{Kind: charge.ConversionSameCurrency}, OccurredOn: at, AnalysisOn: at, Basis: "运费",
		Beneficiaries: []charge.Target{order}, By: by})
	if err != nil {
		t.Fatal(err)
	}
	if err = fr.Confirm(by, []string{"bill"}); err != nil {
		t.Fatal(err)
	}
	f, _ := Compute(Input{At: at, Order: Order{ID: "order.1", Revision: 1, Status: OrderClosed, Ended: EndClose, Currency: usd, Net: m(usd, 96000),
		Analysis: &Analysis{Currency: usd, Rate: dec("1")}}, Sales: []*settlement.Statement{st}, Purchases: []*settlement.Statement{p},
		Charges: []*charge.Charge{fr}, CommitmentsShown: true, ShipmentsShown: true, Commitments: []CommitmentLine{{CommitmentID: "commitment.1",
			OrderLineID: "L1", Status: "issued", Net: m(usd, 60000)}}})
	c := f.Confirmed
	is(t, "BRC-001 confirmed", c.Result, "300.00 USD")
	is(t, "BRC-001 forecast", f.Forecast.Result, "300.00 USD")
	is(t, "the order's freight", c.OrderLevelCharges, "60.00 USD")
	if c.Margin != "31.25" || f.Currency != usd || len(c.Batches) != 1 || c.Batches[0].Result.String() != "360.00 USD" {
		t.Fatalf("BRC-001 %+v", c)
	}
}

// Baseline 4.7 S-B as far as STL goes (C8's credit notes in the confirmed result): 100 PCS at USD 10.00 settled,
// a confirmed price allowance of USD 100.00 → net sales 900.00 (1,000.00 − 100.00, the credit on the statement's
// batch); cost USD 4.00 × 100 = 400.00 (S-B's return of 10 PCS to stock, cost 40.00, is STK's — S6 — so the cost
// of sales stays 400.00 here); freight 50.00 → confirmed result 450.00, margin 50.00 % of 900.00. A requested
// (unconfirmed) or rejected credit note does not count, nor does a reversed statement.
func TestSBCreditNoteInTheConfirmedResult(t *testing.T) {
	st := &settlement.Statement{ID: "statement.sb", Kind: settlement.KindSales, Status: settlement.StatusConfirmed, Version: 4, OrderID: "order.sb",
		Net: m(usd, 100000), Amount: m(usd, 100000), Lines: []settlement.Line{{Seq: 1, Source: settlement.SourceKey{Kind: settlement.SourceShipmentLine,
			ID: "shipment.sb", LineID: "S1"}, OrderID: "order.sb", OrderLineID: "L1", Quantity: dec("100"), Net: m(usd, 100000), Amount: m(usd, 100000)}}}
	reversed := &settlement.Statement{ID: "statement.old", Kind: settlement.KindSales, Status: settlement.StatusReversed, Version: 2,
		OrderID: "order.sb", Net: m(usd, 120000), Amount: m(usd, 120000), Lines: []settlement.Line{{Seq: 1, Source: settlement.SourceKey{
			Kind: settlement.SourceShipmentLine, ID: "shipment.sb", LineID: "S1"}, OrderID: "order.sb", OrderLineID: "L1", Quantity: dec("100"),
			Net: m(usd, 120000), Amount: m(usd, 120000)}}}
	p := &settlement.Statement{ID: "statement.p", Kind: settlement.KindPurchase, Status: settlement.StatusConfirmed, Version: 1, Net: m(usd, 40000),
		Amount: m(usd, 40000), Lines: []settlement.Line{{Seq: 1, Source: settlement.SourceKey{Kind: settlement.SourceDeliveryLine, ID: "d", LineID: "L1"},
			OrderID: "order.sb", OrderLineID: "L1", CommitmentID: "c", Quantity: dec("100"), Net: m(usd, 40000), Amount: m(usd, 40000)}}}
	fr, err := charge.Record(charge.RecordInput{ID: "freight.sb", Status: charge.StatusPending, Source: charge.Source{Kind: charge.SourceInternal},
		Amount: m(usd, 5000), Conversion: charge.Conversion{Kind: charge.ConversionSameCurrency}, OccurredOn: at, AnalysisOn: at, Basis: "运费",
		Beneficiaries: []charge.Target{{Kind: charge.TargetShipment, OrderID: "order.sb", ShipmentID: "shipment.sb"}}, By: by})
	if err != nil {
		t.Fatal(err)
	}
	if err = fr.Confirm(by, []string{"bill"}); err != nil {
		t.Fatal(err)
	}
	note := func(id, status string, minor int64) settlement.CreditNote {
		return settlement.CreditNote{ID: id, StatementID: st.ID, OrderID: "order.sb", Kind: settlement.CreditPriceAllowance, Status: status,
			Version: 2, Amount: m(usd, minor)}
	}
	f, _ := Compute(Input{At: at, Order: Order{ID: "order.sb", Revision: 1, Status: OrderClosed, Ended: EndClose, Currency: usd, Net: m(usd, 100000)},
		Sales: []*settlement.Statement{reversed, st}, Purchases: []*settlement.Statement{p}, Charges: []*charge.Charge{fr},
		CreditNotes: map[string][]settlement.CreditNote{st.ID: {note("cn.1", settlement.CreditConfirmed, 10000), note("cn.2",
			settlement.CreditRequested, 5000), note("cn.3", settlement.CreditRejected, 7000)}, reversed.ID: {note("cn.old", settlement.CreditConfirmed, 2000)}},
		CommitmentsShown: true, ShipmentsShown: true})
	c := f.Confirmed
	is(t, "net sales", c.NetSales, "1000.00 USD")
	is(t, "credits", c.Credits, "100.00 USD")
	is(t, "cost of sales", c.PurchaseCost, "400.00 USD")
	is(t, "result", c.Result, "450.00 USD")
	if c.Margin != "50.00" || len(c.Batches) != 1 || c.Batches[0].Credits.String() != "100.00 USD" || c.Batches[0].DirectCharges.String() != "50.00 USD" ||
		c.Batches[0].Result.String() != "450.00 USD" {
		t.Fatalf("S-B confirmed %+v", c)
	}
}
