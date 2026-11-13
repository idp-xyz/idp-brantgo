package application

import (
	"strconv"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Applications to receivables and payables and the move of stage applications (S5-04b; STL-INV-002,
// STL-INV-003; S5 plan D-4; baseline 4.6 S-A; BRC-001).

// brcAccount is BRC-001's plan: 288.00 on order confirmation, 672.00 30 days after shipment.
func brcAccount(t *testing.T) *orderaccount.Account {
	t.Helper()
	a, err := orderaccount.Open("order.1", 1, "h", "customer.1", 1, []orderaccount.Stage{
		{StageID: "P1", Trigger: orderaccount.TriggerOrderConfirmed, Percent: "30", Amount: money(t, usd, "288.00")},
		{StageID: "P2", Trigger: orderaccount.TriggerAfterShipment, Days: 30, Percent: "70", Amount: money(t, usd, "672.00")}}, treasury, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func statementOf(t *testing.T, a *orderaccount.Account, amount string) *settlement.Statement {
	t.Helper()
	st, err := settlement.Sales(settlement.SalesInput{ID: "statement.1", Account: a, By: treasury, Lines: []settlement.ShipmentLine{{
		ShipmentID: "shipment.1", LineID: "S1", ShipmentVersion: 2, ShippedAt: treasury.At, OrderID: "order.1", OrderRevision: 1, OrderHash: "h",
		CustomerID: "customer.1", OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS", UnitRevision: 1, OrderLineQuantity: "120",
		Quantity: "120", Currency: "USD", UnitPrice: "8", BasisNet: "96000", BasisTax: "0", Net: amount, Tax: "0", Payable: amount}}})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func stageApp(t *testing.T, id, fund, stage, amount string) Application {
	return Application{ID: id, FundRecordID: fund, TargetKind: TargetOrderStage, OrderID: "order.1", OrderRevision: 1, StageID: stage,
		Amount: money(t, usd, amount), Status: StatusApplied, Version: 1, Applied: treasury}
}

func ids() func() string {
	n := 0
	return func() string { n++; return "app.new." + strconv.Itoa(n) }
}

// D-4, BRC-001 step 19: settling 960.00 moves the 288.00 applied to P1 (two receipts, 200.00 and
// 88.00) to the receivable's P1 installment, keeping fund, stage and amount: the moved applications
// stand no more, their replacements stand on the receivable for the stage — so the stage reads paid
// and the fund's available balance does not change. An application to P2 (none here) would move to
// P2's installment.
func TestStageApplicationsMoveToTheReceivable(t *testing.T) {
	st := statementOf(t, brcAccount(t), "96000")
	standing := []Application{stageApp(t, "app.1", "fund.1", "P1", "200.00"), stageApp(t, "app.2", "fund.2", "P1", "88.00")}
	moves := MoveToReceivable(st, standing, ids(), treasury, "team.t")
	if len(moves) != 2 {
		t.Fatalf("moves %+v", moves)
	}
	for i, m := range moves {
		r := m.Replacement
		if m.From.Status != StatusMoved || m.From.Version != 2 || m.Remainder != nil || r.TargetKind != TargetReceivable ||
			r.FundRecordID != standing[i].FundRecordID || r.StageID != "P1" || r.StatementID != "statement.1" || r.InstallmentSeq != 1 ||
			r.MovedFrom != standing[i].ID || !r.Amount.Equal(standing[i].Amount) || r.OrderID != "order.1" || r.OrderRevision != 1 ||
			r.Status != StatusApplied || r.OwnerTeam != "team.t" {
			t.Fatalf("move %d: %+v %+v", i, m.From, r)
		}
	}
	if err := moves[0].From.Unapply(treasury); err == nil {
		t.Fatal("a moved application is unapplied")
	} else {
		want(t, err, kernel.CodeInvalidState)
	}
}

// Baseline 4.6 S-A with a deposit of 50 % (500.00) and a first shipment settling 300.00: the P1
// installment of the first statement is 300.00, so 300.00 of the deposit moves and its remainder of
// 200.00 stays on the stage at the revision it was made at, waiting for the next statement.
func TestAPartOfAPrepaymentMovesAndTheRemainderStays(t *testing.T) {
	a, err := orderaccount.Open("order.1", 1, "h", "customer.1", 1, []orderaccount.Stage{
		{StageID: "P1", Trigger: orderaccount.TriggerOrderConfirmed, Percent: "50", Amount: money(t, usd, "500.00")},
		{StageID: "P2", Trigger: orderaccount.TriggerAfterShipment, Days: 30, Percent: "50", Amount: money(t, usd, "500.00")}}, treasury, "")
	if err != nil {
		t.Fatal(err)
	}
	st := statementOf(t, a, "30000")
	if len(st.Installments) != 1 || st.Installments[0].StageID != "P1" || st.Installments[0].Amount.MinorString() != "30000" {
		t.Fatalf("installments %+v", st.Installments)
	}
	moves := MoveToReceivable(st, []Application{stageApp(t, "app.1", "fund.1", "P1", "500.00"), stageApp(t, "app.9", "fund.9", "P2", "1.00")},
		ids(), treasury, "")
	if len(moves) != 1 {
		t.Fatalf("moves %+v", moves)
	}
	m := moves[0]
	if m.Replacement.Amount.MinorString() != "30000" || m.Remainder == nil || m.Remainder.Amount.MinorString() != "20000" ||
		m.Remainder.TargetKind != TargetOrderStage || m.Remainder.StageID != "P1" || m.Remainder.OrderRevision != 1 ||
		m.Remainder.MovedFrom != "app.1" || m.Remainder.StatementID != "" {
		t.Fatalf("move %+v / %+v", m.Replacement, m.Remainder)
	}
}

// STL-INV-002 on a receivable (4.6 S-A step 4, BRC-001 step 20): a receipt of the customer is
// applied to an installment — the one named, or the first still open — within what the fund has
// available and the installment has open; a payer other than the customer, a non-receipt, another
// currency, an unexplained difference and more than open are refused. A payable takes payments to
// its supplier only.
func TestApplicationsToReceivablesAndPayables(t *testing.T) {
	st := statementOf(t, brcAccount(t), "96000")
	paid := map[int]kernel.Money{1: money(t, usd, "288.00")}
	f := receipt(t, "fund.670", "customer.1", usd, "670.00", "672.00")
	in := StatementInput{ID: "app.r", Fund: f, FundEntries: fundrecord.NoEntries(usd), Statement: st, Applied: paid,
		Amount: money(t, usd, "670.00"), By: treasury}
	x, err := ApplyToStatement(in)
	if err != nil {
		t.Fatal(err)
	}
	if x.TargetKind != TargetReceivable || x.InstallmentSeq != 2 || x.StageID != "P2" || x.OrderID != "order.1" || x.OrderRevision != 1 ||
		x.StatementID != "statement.1" || x.Amount.MinorString() != "67000" {
		t.Fatalf("application %+v", x)
	}
	in.Amount = money(t, usd, "672.00")
	_, err = ApplyToStatement(in)
	want(t, err, fundrecord.CodeFundDifferenceUnexplained)
	in.StageID = "P1"
	in.Amount = money(t, usd, "1.00")
	_, err = ApplyToStatement(in)
	want(t, err, fundrecord.CodeApplicationExceedsOpen)
	in.StageID = "P9"
	_, err = ApplyToStatement(in)
	want(t, err, kernel.CodeReferenceInvalid)
	in.StageID = ""
	in.Fund = receipt(t, "fund.x", "customer.2", usd, "10.00", "10.00")
	_, err = ApplyToStatement(in)
	want(t, err, fundrecord.CodePartyEntityMismatch)
	in.Fund = receipt(t, "fund.c", "customer.1", cny, "10.00", "10.00")
	_, err = ApplyToStatement(in)
	want(t, err, fundrecord.CodeFXScopeMismatch)
	all := map[int]kernel.Money{1: money(t, usd, "288.00"), 2: money(t, usd, "672.00")}
	in.Fund, in.Applied = f, all
	_, err = ApplyToStatement(in)
	want(t, err, fundrecord.CodeApplicationExceedsOpen)

	// A payable: a payment to supplier.1.
	payable := &settlement.Statement{ID: "statement.p", Kind: settlement.KindPurchase, Status: settlement.StatusConfirmed, CounterpartyID: "supplier.1",
		Amount: money(t, usd, "600.00"), Installments: []settlement.Installment{{Seq: 1, Amount: money(t, usd, "600.00")}}}
	pay := receipt(t, "fund.p", "supplier.1", usd, "600.00", "600.00")
	_, err = ApplyToStatement(StatementInput{ID: "app.p", Fund: pay, FundEntries: fundrecord.NoEntries(usd), Statement: payable,
		Amount: money(t, usd, "600.00"), By: treasury})
	want(t, err, kernel.CodeInvalidState)
	pay.Kind, pay.Direction = fundrecord.KindPayment, fundrecord.Out
	x, err = ApplyToStatement(StatementInput{ID: "app.p", Fund: pay, FundEntries: fundrecord.NoEntries(usd), Statement: payable,
		Amount: money(t, usd, "600.00"), By: treasury})
	if err != nil || x.TargetKind != TargetPayable || x.OrderID != "" || x.StageID != "" || x.InstallmentSeq != 1 {
		t.Fatalf("payable application %+v %v", x, err)
	}
	if TargetOf(settlement.KindSales) != TargetReceivable || TargetOf(settlement.KindPurchase) != TargetPayable {
		t.Fatal("target kinds")
	}
}
