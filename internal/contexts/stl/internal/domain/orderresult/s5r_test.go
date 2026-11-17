package orderresult

import (
	"strings"
	"testing"

	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/charge"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Unit tests of the order result after the S5R decisions (simulated sign-off E0 of 2026-11-13): S5R-D19 (FX difference
// at the actual conversion rate, the settlement rate of the application for what is not converted; charge payments in
// the net cash), S5R-D20 (a confirmed payable adjustment credits the purchase cost), S5R-D24 (a recharged charge is not
// in the result), S5R-D42 (the result of a cancelled order; write-offs as sunk cost). STL-INV-011; baseline 4.5, 4.6
// S-A. Python V0.50 has no STL.

// usdReceipt is a standing application of a USD receipt of fund to a USD receivable (no conversion), with the
// settlement rate USD→CNY it recorded when it was applied (nil: none).
func usdReceipt(id, fund, statement, stage string, usdMinor int64, settlementRate string) fa.Application {
	x := fa.Application{ID: id, FundRecordID: fund, TargetKind: fa.TargetReceivable, OrderID: "order.sa", StageID: stage, StatementID: statement,
		Amount: m(usd, usdMinor), Status: fa.StatusApplied, Version: 1}
	if settlementRate != "" {
		x.Settlement = &fa.SettlementRate{Use: fa.RateUse{ID: "fx.usd-cny.settlement", Revision: 3, Hash: strings.Repeat("c", 64)},
			Value: dec(settlementRate), Currency: cny}
	}
	return x
}

// conversion is an fx_conversion record of a USD fund into CNY at the bank's rate.
func conversion(id string, soldUSD int64, rate string) FundConversion {
	return FundConversion{ID: id, Sold: m(usd, soldUSD), Rate: dec(rate), Currency: cny}
}

// saUSD is S-A as the baseline tells it (4.6 steps 1 and 4): USD 200 received and converted at 7.0 (CNY 1,400), USD 800
// received and converted at 6.875 (CNY 5,500), the receipts applied in USD; the freight CNY 650 paid through STL.
func saUSD(t *testing.T) Input {
	in := saInput(t)
	in.Applications = []fa.Application{
		usdReceipt("app.pre", "fund.200", "statement.600", "P1", 20000, ""),
		usdReceipt("app.600", "fund.800", "statement.600", "P2", 40000, ""),
		usdReceipt("app.400", "fund.800", "statement.400", "P2", 40000, "")}
	in.Funds = map[string]FundBasis{
		"fund.200": {Received: m(usd, 20000), Conversions: []FundConversion{conversion("conv.200", 20000, "7.0")}},
		"fund.800": {Received: m(usd, 80000), Conversions: []FundConversion{conversion("conv.800", 80000, "6.875")}}}
	in.ChargePayments = map[string][]fa.Application{"freight.sa": {{ID: "app.freight", FundRecordID: "fund.freight", TargetKind: fa.TargetCharge,
		ChargeID: "freight.sa", Amount: m(cny, 65000), Status: fa.StatusApplied, Version: 1}}}
	return in
}

// S5R-D19 / STL-INV-011 positive: "S-A：一笔美元收款加一笔结汇得出 −100.00，净现金含费用付款得出 2,250" — baseline 4.6: actual
// receipts CNY 6,900 against the analysis revenue 7,000 (difference −100, realised by the conversions at 7.0 and
// 6.875), purchase CNY 4,000 and freight CNY 650 paid through STL: net cash CNY 2,250. The confirmed result stays
// 2,350.
func TestS5RD19SAFromUSDReceiptsAndConversions(t *testing.T) {
	f, dims := Compute(saUSD(t))
	is(t, "FX difference", f.FX.Difference, "-100.00 CNY")
	is(t, "realised", f.FX.Realized, "-100.00 CNY")
	is(t, "unrealised", f.FX.Unrealized, "0.00 CNY")
	if len(f.FX.Items) != 3 || len(f.FX.Gaps) != 0 {
		t.Fatalf("FX %+v", f.FX)
	}
	for _, x := range f.FX.Items {
		if x.Basis != FXConversion || x.Unrealized || len(x.ConversionIDs) != 1 {
			t.Fatalf("item %+v", x)
		}
	}
	if x := f.FX.Items[1]; x.ApplicationID != "app.600" || x.Rate != "6.875" || x.Target.String() != "2750.00 CNY" || x.Difference.String() != "-50.00 CNY" {
		t.Fatalf("app.600 %+v", x)
	}
	is(t, "receipts", f.Cash.Receipts, "6900.00 CNY")
	is(t, "charge payments", f.Cash.ChargePayments, "650.00 CNY")
	is(t, "payments", f.Cash.Payments, "4650.00 CNY")
	is(t, "net cash (baseline 4.6: 2,250)", f.Cash.Net, "2250.00 CNY")
	is(t, "unpaid charges", f.Cash.Unpaid, "0.00 CNY")
	if len(f.Cash.UnpaidSources) != 0 || len(f.Cash.Gaps) != 0 {
		t.Fatalf("cash %+v", f.Cash)
	}
	is(t, "confirmed result", f.Confirmed.Result, "2350.00 CNY")
	if err := NotSettled(dims); err != nil {
		t.Fatal(err)
	}
}

// S5R-D19 (b): "部分结汇显示 unrealized" — USD 800 received, only USD 500 converted at 6.875: each USD 400 application is
// 62.5 % converted (USD 250 realised at 6.875: −31.25) and USD 150 valued at the settlement rate 6.9 its application
// recorded (unrealised −15.00). An application that recorded no rate is valued at the analysis rate and stated as a gap.
func TestS5RD19PartialConversionIsUnrealized(t *testing.T) {
	in := saUSD(t)
	in.Funds["fund.800"] = FundBasis{Received: m(usd, 80000), Conversions: []FundConversion{conversion("conv.500", 50000, "6.875")}}
	in.Applications[1] = usdReceipt("app.600", "fund.800", "statement.600", "P2", 40000, "6.9")
	in.Applications[2] = usdReceipt("app.400", "fund.800", "statement.400", "P2", 40000, "6.9")
	f, _ := Compute(in)
	is(t, "realised", f.FX.Realized, "-62.50 CNY")
	is(t, "unrealised", f.FX.Unrealized, "-30.00 CNY")
	is(t, "FX difference", f.FX.Difference, "-92.50 CNY")
	var unrealised int
	for _, x := range f.FX.Items {
		if x.Unrealized {
			unrealised++
			if x.Basis != FXUnrealized || x.Rate != "6.9" || x.Source.String() != "150.00 USD" || x.Difference.String() != "-15.00 CNY" {
				t.Fatalf("unrealised item %+v", x)
			}
		}
	}
	if unrealised != 2 {
		t.Fatalf("items %+v", f.FX.Items)
	}
	is(t, "receipts (1,400 + 2 × (1,718.75 + 1,035.00))", f.Cash.Receipts, "6907.50 CNY")

	// No settlement rate recorded: the unconverted USD 150 is valued at the analysis rate, and said so.
	in.Applications[2] = usdReceipt("app.400", "fund.800", "statement.400", "P2", 40000, "")
	f, _ = Compute(in)
	is(t, "unrealised (one application)", f.FX.Unrealized, "-15.00 CNY")
	if len(f.FX.Gaps) != 1 || !strings.Contains(f.FX.Gaps[0], "app.400") || !strings.Contains(f.FX.Gaps[0], "no settlement rate") {
		t.Fatalf("gaps %v", f.FX.Gaps)
	}
}

// S5R-D19 (2): a charge not paid through STL is not in the net cash and is listed unpaid (the C9 S-A: 2,900).
func TestS5RD19UnpaidChargesAreListed(t *testing.T) {
	in := saUSD(t)
	in.ChargePayments = nil
	f, _ := Compute(in)
	is(t, "payments", f.Cash.Payments, "4000.00 CNY")
	is(t, "net cash", f.Cash.Net, "2900.00 CNY")
	is(t, "unpaid", f.Cash.Unpaid, "650.00 CNY")
	if len(f.Cash.UnpaidSources) != 1 || f.Cash.UnpaidSources[0].ID != "freight.sa" || !strings.Contains(f.Cash.UnpaidSources[0].Detail, "unpaid") {
		t.Fatalf("unpaid %+v", f.Cash.UnpaidSources)
	}
	// Half paid: CNY 325 counts (split 195 + 130 by the allocation, both the order's), 325 stays unpaid.
	in.ChargePayments = map[string][]fa.Application{"freight.sa": {{ID: "app.half", FundRecordID: "fund.freight", TargetKind: fa.TargetCharge,
		ChargeID: "freight.sa", Amount: m(cny, 32500), Status: fa.StatusApplied, Version: 1}}}
	f, _ = Compute(in)
	is(t, "charge payments", f.Cash.ChargePayments, "325.00 CNY")
	is(t, "unpaid", f.Cash.Unpaid, "325.00 CNY")
	if !strings.Contains(f.Cash.UnpaidSources[0].Detail, charge.PaymentPartlyPaid) {
		t.Fatalf("unpaid %+v", f.Cash.UnpaidSources)
	}
}

// S5R-D24 / STL-INV-010: a charge recharged to the buyer is advanced and recovered: neither cost nor cash of the result,
// and not a close condition; the result lists it apart.
func TestS5RD24RechargedChargeIsLeftOut(t *testing.T) {
	in := saUSD(t)
	main, err := charge.Record(charge.RecordInput{CostComponent: charge.ComponentMainCarriage, Recharged: true, ID: "ocean.sa",
		Status: charge.StatusPending, Source: charge.Source{Kind: charge.SourceSupplierBill, PartyID: "forwarder.1", Reference: "OCEAN-1"},
		Amount: m(cny, 120000), Conversion: charge.Conversion{Kind: charge.ConversionSameCurrency}, OccurredOn: at, AnalysisOn: at, Basis: "代垫海运费",
		Beneficiaries: []charge.Target{{Kind: charge.TargetOrder, OrderID: "order.sa"}}, EvidenceIDs: []string{"bill"}, By: by})
	if err != nil {
		t.Fatal(err)
	}
	in.Charges = append(in.Charges, main)
	f, dims := Compute(in)
	is(t, "confirmed result", f.Confirmed.Result, "2350.00 CNY")
	is(t, "executing charges", f.Executing.DirectCharges, "650.00 CNY")
	is(t, "net cash", f.Cash.Net, "2250.00 CNY")
	if len(f.Recharged) != 1 || f.Recharged[0].ID != "ocean.sa" {
		t.Fatalf("recharged %+v", f.Recharged)
	}
	if err := NotSettled(dims); err != nil {
		t.Fatalf("a pending recharged charge blocks the close: %v", err)
	}
}

// S5R-D20 / STL-INV-013: a confirmed payable adjustment (a price allowance of CNY 200 on the purchase statement, no tax)
// credits the purchase cost by its net part: cost of sales 3,800 (2,280 + 1,520 by the 60 / 40 sold), confirmed 2,550;
// a requested or rejected one does not count.
func TestS5RD20PayableAdjustmentCreditsThePurchaseCost(t *testing.T) {
	in := saUSD(t)
	allowance := settlement.PayableAdjustment{ID: "adj.1", StatementID: "statement.purchase", Kind: settlement.AdjustmentPriceAllowance,
		Status: settlement.AdjustmentConfirmed, Version: 2, Amount: m(cny, 20000), PurchaseInvoiceID: "invoice.red.1"}
	pending := allowance
	pending.ID, pending.Status, pending.Version = "adj.2", settlement.AdjustmentRequested, 1
	in.PayableAdjustments = map[string][]settlement.PayableAdjustment{"statement.purchase": {allowance, pending}}
	f, _ := Compute(in)
	is(t, "cost of sales", f.Confirmed.PurchaseCost, "3800.00 CNY")
	is(t, "confirmed result", f.Confirmed.Result, "2550.00 CNY")
	is(t, "executing confirmed purchase", f.Executing.PurchaseConfirmed, "3800.00 CNY")
	if f.Confirmed.Batches[1].CostOfSales.String() != "2280.00 CNY" || f.Confirmed.Batches[0].CostOfSales.String() != "1520.00 CNY" {
		t.Fatalf("batches %+v", f.Confirmed.Batches)
	}
	found := false
	for _, s := range f.Confirmed.Sources {
		found = found || (s.Kind == "payable_adjustment" && s.ID == "adj.1")
	}
	if !found {
		t.Fatalf("sources %+v", f.Confirmed.Sources)
	}
}

// S5R-D46(2) / STL-INV-008: a credit note on a statement with tax credits net sales by its net part (net 1,000.00, tax
// 130.00: a credit of 113.00 is 100.00 net + 13.00 tax).
func TestS5RD46CreditNoteCreditsNetSalesByItsNetPart(t *testing.T) {
	in := saUSD(t)
	st := in.Sales[0]
	st.Net, st.Tax, st.Amount = m(usd, 60000), m(usd, 7800), m(usd, 67800)
	in.CreditNotes = map[string][]settlement.CreditNote{st.ID: {{ID: "cn.1", StatementID: st.ID, OrderID: "order.sa",
		Kind: settlement.CreditPriceAllowance, Status: settlement.CreditConfirmed, Version: 2, Amount: m(usd, 11300)}}}
	f, _ := Compute(in)
	is(t, "credits (USD 100.00 at 7.0)", f.Confirmed.Credits, "700.00 CNY")
}

// cancelled is a cancelled order (S5R-D42): nothing shipped or sold; commitment.1 bought 100 PCS for CNY 4,000, 60 were
// verified delivered and written off (PUR's approved disposition); the freight of CNY 650 is the order's.
func cancelled(t *testing.T) Input {
	in := saUSD(t)
	in.Order.Status, in.Order.Ended = OrderCancelled, "order.cancel"
	in.Sales, in.Applications, in.Shipments = nil, nil, nil
	in.Applied = map[string]map[int]kernel.Money{"statement.purchase": {1: m(cny, 400000)}}
	in.Commitments = []CommitmentLine{{CommitmentID: "commitment.1", OrderLineID: "L1", Status: "cancelled", Net: m(cny, 400000),
		Purchased: dec("100"), Verified: dec("60"), Dispositions: []Disposition{{ID: "disposition.1", Kind: DispositionWriteOff, Quantity: dec("60")}}}}
	in.Purchases = nil
	in.Charges = []*charge.Charge{orderFreight(t)}
	in.ChargePayments = nil
	return in
}

// orderFreight is a confirmed freight of CNY 650 of order.sa (the order itself).
func orderFreight(t *testing.T) *charge.Charge {
	t.Helper()
	c, err := charge.Record(charge.RecordInput{CostComponent: charge.ComponentPreCarriage, ID: "freight.sa", Status: charge.StatusPending,
		Source: charge.Source{Kind: charge.SourceSupplierBill, PartyID: "forwarder.1", Reference: "BILL-1"}, Amount: m(cny, 65000),
		Conversion: charge.Conversion{Kind: charge.ConversionSameCurrency}, OccurredOn: at, AnalysisOn: at, Basis: "已付运费",
		Beneficiaries: []charge.Target{{Kind: charge.TargetOrder, OrderID: "order.sa"}}, EvidenceIDs: []string{"bill"}, By: by})
	if err == nil {
		err = c.Confirm(by, []string{"bill"})
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// S5R-D42 / STL-INV-011: "取消订单有运费时关闭为 cancelled" — a cancelled order with a confirmed freight and a write-off of
// the delivered goods: no revenue, the result is the sunk cost (write-off CNY 2,400 = 4,000 × 60 / 100, freight 650):
// −3,050.00, fulfilment met "cancelled with sunk costs". Without any charge or write-off it has no result (fulfilment
// unmet, no sunk cost).
func TestS5RD42CancelledOrderClosesOnItsSunkCost(t *testing.T) {
	f, dims := Compute(cancelled(t))
	if !f.Cancelled || !f.SunkCost {
		t.Fatalf("cancelled %v sunk %v", f.Cancelled, f.SunkCost)
	}
	is(t, "net sales", f.Confirmed.NetSales, "0.00 CNY")
	is(t, "write-off", f.Confirmed.WriteOffCost, "2400.00 CNY")
	is(t, "purchase cost", f.Confirmed.PurchaseCost, "2400.00 CNY")
	is(t, "charges", f.Confirmed.DirectCharges, "650.00 CNY")
	is(t, "result", f.Confirmed.Result, "-3050.00 CNY")
	is(t, "forecast net sales", f.Forecast.NetSales, "0.00 CNY")
	if d := dims[0]; d.Name != DimFulfilment || d.Status != Met || !strings.Contains(d.Items[0], "S5R-D42") {
		t.Fatalf("fulfilment %+v", d)
	}
	if err := NotSettled(dims); err != nil {
		t.Fatalf("the cancelled order does not settle: %v", err)
	}

	none := cancelled(t)
	none.Charges, none.Commitments = nil, nil
	f, dims = Compute(none)
	if !f.Cancelled || f.SunkCost || dims[0].Status != Unmet || !strings.Contains(dims[0].Items[0], "no operating result") {
		t.Fatalf("no sunk cost: %v %+v", f.SunkCost, dims[0])
	}
}

// S5R-D42 with R4's gap: a write-off never counts more than the order line has verified and not shipped (60 verified,
// 20 shipped: 40 of the 60 written off count, CNY 1,600, and the cap is stated); hold_for_reuse and return_to_supplier
// are no sunk cost.
func TestS5RD42WriteOffIsCappedByWhatShipped(t *testing.T) {
	in := cancelled(t)
	in.Order.Status, in.Order.Ended = OrderClosed, EndCloseRemainder
	in.Shipments = []ShipmentLine{{ShipmentID: "shipment.20", LineID: "S1", Status: ShipmentConfirmed, OrderLineID: "L1", Quantity: dec("20")}}
	in.Commitments[0].Dispositions = append(in.Commitments[0].Dispositions,
		Disposition{ID: "disposition.2", Kind: "hold_for_reuse", Quantity: dec("5")})
	f, _ := Compute(in)
	is(t, "write-off", f.Confirmed.WriteOffCost, "1600.00 CNY")
	if f.Cancelled {
		t.Fatal("a short-closed order is not cancelled")
	}
	capped := false
	for _, g := range f.Confirmed.Gaps {
		capped = capped || strings.Contains(g, "verified and not shipped")
	}
	if !capped {
		t.Fatalf("gaps %v", f.Confirmed.Gaps)
	}
}
