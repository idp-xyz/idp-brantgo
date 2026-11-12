package settlement

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Settlement statements (S5-04b): STL-INV-001, D-4's installments, baseline 4.6 S-A, BRC-001. Python
// V0.50 has no STL; the tests cite design IDs.

var (
	usd      = kernel.MustCurrency("USD", 2)
	shipped  = time.Date(2026, 11, 10, 6, 0, 0, 0, time.UTC)
	treasury = fundrecord.Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "treasury.1"}, Epoch: 1, At: time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)}
)

func money(t *testing.T, s string) kernel.Money {
	t.Helper()
	m, err := kernel.ParseAmount(usd, s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func want(t *testing.T, err error, code, path string) {
	t.Helper()
	r, ok := kernel.AsRejection(err)
	if !ok || r.Code != code || (path != "" && (len(r.Fields) == 0 || r.Fields[0].Path != path)) {
		t.Fatalf("got %v, want %s at %s", err, code, path)
	}
}

func account(t *testing.T, stages ...orderaccount.Stage) *orderaccount.Account {
	t.Helper()
	a, err := orderaccount.Open("order.1", 1, "h", "customer.1", 1, stages, treasury, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// brc001 is BRC-001's plan: 30 % 288.00 on order confirmation, 70 % 672.00 30 days after shipment.
func brc001(t *testing.T) *orderaccount.Account {
	return account(t, orderaccount.Stage{StageID: "P1", Trigger: orderaccount.TriggerOrderConfirmed, Percent: "30", Amount: money(t, "288.00")},
		orderaccount.Stage{StageID: "P2", Trigger: orderaccount.TriggerAfterShipment, Days: 30, Percent: "70", Amount: money(t, "672.00")})
}

// line is shipment line id of order.1 shipping q of the order line's 120 at 8.00 (960.00), valued
// net = payable = payable.
func line(id, q, payable string) ShipmentLine {
	return ShipmentLine{ShipmentID: "shipment.1", LineID: id, ShipmentVersion: 2, ShippedAt: shipped, OrderID: "order.1", OrderRevision: 1,
		OrderHash: "h", CustomerID: "customer.1", OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS", UnitRevision: 1,
		OrderLineQuantity: "120", Quantity: q, Currency: "USD", UnitPrice: "8", BasisNet: "96000", BasisTax: "0", Net: payable, Tax: "0",
		Payable: payable}
}

func sales(t *testing.T, a *orderaccount.Account, prior map[string]kernel.Money, lines ...ShipmentLine) (*Statement, error) {
	t.Helper()
	return Sales(SalesInput{ID: "statement.1", Account: a, Lines: lines, Prior: prior, By: treasury})
}

func installments(st *Statement) [][3]string {
	var out [][3]string
	for _, i := range st.Installments {
		out = append(out, [3]string{i.StageID, i.Amount.MinorString(), i.DueAt.Format(time.RFC3339)})
	}
	return out
}

// BRC-001 (S5 plan §5 D1 step 19): four shipment lines of 30 PCS at 8.00 make a receivable of
// 960.00, due 288.00 when the goods left (P1, on order confirmation: already due) and 672.00 30 days
// after shipment (P2). The statement keeps the account revision, the customer and the lines.
func TestSalesStatementFollowsThePaymentStages(t *testing.T) {
	st, err := sales(t, brc001(t), nil, line("S1", "30", "24000"), line("S2", "30", "24000"), line("S3", "30", "24000"), line("S4", "30", "24000"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Kind != KindSales || st.Amount.MinorString() != "96000" || st.Net.MinorString() != "96000" || st.Tax.MinorString() != "0" ||
		st.CounterpartyID != "customer.1" || st.OrderID != "order.1" || st.OrderRevision != 1 || len(st.Lines) != 4 || !st.ShippedAt.Equal(shipped) {
		t.Fatalf("statement %+v", st)
	}
	got := installments(st)
	if len(got) != 2 || got[0] != [3]string{"P1", "28800", "2026-11-10T06:00:00Z"} || got[1] != [3]string{"P2", "67200", "2026-12-10T06:00:00Z"} {
		t.Fatalf("installments %v", got)
	}
	l := st.Lines[0]
	if l.Source != (SourceKey{Kind: SourceShipmentLine, ID: "shipment.1", LineID: "S1"}) || l.SourceVersion != 2 || l.Quantity.String() != "30" ||
		l.BasisQuantity.String() != "120" || l.BasisNet.MinorString() != "96000" || l.Status != LineSettled {
		t.Fatalf("line %+v", l)
	}
	if open := st.Open(map[int]kernel.Money{1: money(t, "288.00")}); open.MinorString() != "67200" {
		t.Fatalf("open after the prepayment moved: %s", open)
	}
}

// Baseline 4.6 S-A: sale 100 × USD 10 on 20 % before (200.00) and 80 % 30 days after shipment. The
// first shipment of 60 settles 600.00: the order's settled amount fills the plan in order, so the
// 200.00 prepayment's stage takes 200.00 and 400.00 stays open; the second of 40 settles 400.00, all
// of P2. What goes beyond the plan (rounding of parts, a price change) is due with its last stage.
func TestInstallmentsFillThePlanInOrderBaseline46(t *testing.T) {
	a := account(t, orderaccount.Stage{StageID: "P1", Trigger: orderaccount.TriggerOrderConfirmed, Percent: "20", Amount: money(t, "200.00")},
		orderaccount.Stage{StageID: "P2", Trigger: orderaccount.TriggerAfterShipment, Days: 30, Percent: "80", Amount: money(t, "800.00")})
	first := Schedule(a, nil, money(t, "600.00"), shipped, treasury.At)
	if len(first) != 2 || first[0].StageID != "P1" || first[0].Amount.MinorString() != "20000" || first[1].StageID != "P2" ||
		first[1].Amount.MinorString() != "40000" || first[1].OrderRevision != 1 {
		t.Fatalf("first statement %+v", first)
	}
	prior := map[string]kernel.Money{"P1": money(t, "200.00"), "P2": money(t, "400.00")}
	second := Schedule(a, prior, money(t, "400.00"), shipped, treasury.At)
	if len(second) != 1 || second[0].StageID != "P2" || second[0].Amount.MinorString() != "40000" || second[0].Seq != 1 {
		t.Fatalf("second statement %+v", second)
	}
	prior["P2"] = money(t, "800.00")
	beyond := Schedule(a, prior, money(t, "0.01"), shipped, treasury.At)
	if len(beyond) != 1 || beyond[0].StageID != "" || beyond[0].OrderRevision != 0 || beyond[0].Amount.MinorString() != "1" ||
		!beyond[0].DueAt.Equal(shipped.AddDate(0, 0, 30)) {
		t.Fatalf("beyond the plan %+v", beyond)
	}
	// A stage due after invoice counts from the statement; one before shipment is due when the goods left.
	b := account(t, orderaccount.Stage{StageID: "B", Trigger: orderaccount.TriggerBeforeShipment, Amount: money(t, "100.00")},
		orderaccount.Stage{StageID: "I", Trigger: orderaccount.TriggerAfterInvoice, Days: 15, Amount: money(t, "100.00")})
	got := Schedule(b, nil, money(t, "200.00"), shipped, treasury.At)
	if !got[0].DueAt.Equal(shipped) || !got[1].DueAt.Equal(treasury.At.AddDate(0, 0, 15)) {
		t.Fatalf("due instants %+v", got)
	}
}

// STL-INV-001 and the statement's consistency: a line a standing statement settles is
// SETTLEMENT_SOURCE_DUPLICATE; a sales statement is of one order, its customer and currency; it
// needs the order account; the shipped quantity stays within the order line; the payable amount is
// net and tax.
func TestSalesStatementRefusals(t *testing.T) {
	a := brc001(t)
	settled := map[SourceKey]string{{Kind: SourceShipmentLine, ID: "shipment.1", LineID: "S2"}: "statement.0"}
	_, err := Sales(SalesInput{ID: "statement.1", Account: a, Lines: []ShipmentLine{line("S1", "30", "24000"), line("S2", "30", "24000")},
		Settled: settled, By: treasury})
	want(t, err, CodeSettlementSourceDuplicate, "/args/sources/1")
	other := line("S2", "30", "24000")
	other.OrderID = "order.2"
	_, err = sales(t, a, nil, line("S1", "30", "24000"), other)
	want(t, err, kernel.CodeInvalidKnownValue, "/args/sources/1")
	customer := line("S1", "30", "24000")
	customer.CustomerID = "customer.2"
	_, err = sales(t, a, nil, customer)
	want(t, err, kernel.CodeReferenceInvalid, "/args/sources/0")
	eur := line("S1", "30", "24000")
	eur.Currency = "EUR"
	_, err = sales(t, a, nil, eur)
	want(t, err, kernel.CodeReferenceInvalid, "/args/sources/0")
	_, err = sales(t, nil, nil, line("S1", "30", "24000"))
	want(t, err, kernel.CodeInvalidState, "/args/sources/0")
	over := line("S1", "121", "96800")
	_, err = sales(t, a, nil, over)
	want(t, err, kernel.CodeReferenceInvalid, "/args/sources/0")
	broken := line("S1", "30", "24000")
	broken.Tax = "1"
	_, err = sales(t, a, nil, broken)
	want(t, err, kernel.CodeReferenceInvalid, "/args/sources/0")
	_, err = sales(t, a, nil, line("S1", "30", "0"))
	want(t, err, kernel.CodeInvalidKnownValue, "/args/sources")
}

// delivery is verified delivery line L1 of commitment.1 (supplier.1) of q of the 120 PCS bought for
// 600.00 USD net (BRC-001's commitment at 5.00).
func delivery(id, q string) DeliveryLine {
	return DeliveryLine{DeliveryID: id, LineID: "L1", Version: 1, HandedOverAt: shipped, CommitmentID: "commitment.1", SupplierID: "supplier.1",
		OrderID: "order.1", OrderRevision: 1, OrderHash: "h", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS", UnitRevision: 1, PurchaseQuantity: "120",
		VerifiedQuantity: q, Currency: "USD", MinorUnits: 2, LineNet: "60000", LineTax: "0"}
}

// A purchase statement (R31, MD-45, BRC-001 step 21): a verified delivery of 120 at 5.00 makes a
// payable of 600.00, one installment due at confirmation; a part is valued at its share, HALF_UP,
// net and tax apart (1 of 3 at 10.00 + 1.30 tax: 3.33 + 0.43); the statement is of one supplier and
// currency; nothing verified, or a line already settled, is refused.
func TestPurchaseStatementValuesVerifiedDeliveries(t *testing.T) {
	st, err := Purchase(PurchaseInput{ID: "statement.p", Lines: []DeliveryLine{delivery("delivery.1", "120")}, By: treasury})
	if err != nil {
		t.Fatal(err)
	}
	if st.Kind != KindPurchase || st.CounterpartyID != "supplier.1" || st.Amount.MinorString() != "60000" || st.OrderID != "" ||
		len(st.Installments) != 1 || st.Installments[0].StageID != "" || !st.Installments[0].DueAt.Equal(treasury.At) {
		t.Fatalf("statement %+v", st)
	}
	l := st.Lines[0]
	if l.Source != (SourceKey{Kind: SourceDeliveryLine, ID: "delivery.1", LineID: "L1"}) || l.CommitmentID != "commitment.1" ||
		l.Quantity.String() != "120" || l.Amount.MinorString() != "60000" {
		t.Fatalf("line %+v", l)
	}
	part := delivery("delivery.2", "1")
	part.PurchaseQuantity, part.LineNet, part.LineTax = "3", "1000", "130"
	st, err = Purchase(PurchaseInput{ID: "statement.q", Lines: []DeliveryLine{part, delivery("delivery.3", "100")}, By: treasury})
	if err != nil {
		t.Fatal(err)
	}
	if st.Lines[0].Net.MinorString() != "333" || st.Lines[0].Tax.MinorString() != "43" || st.Lines[0].Amount.MinorString() != "376" ||
		st.Lines[1].Amount.MinorString() != "50000" || st.Amount.MinorString() != "50376" || st.Tax.MinorString() != "43" {
		t.Fatalf("shares %+v", st)
	}
	other := delivery("delivery.4", "10")
	other.SupplierID = "supplier.2"
	_, err = Purchase(PurchaseInput{ID: "s", Lines: []DeliveryLine{delivery("delivery.1", "10"), other}, By: treasury})
	want(t, err, kernel.CodeInvalidKnownValue, "/args/sources/1")
	cny := delivery("delivery.4", "10")
	cny.Currency = "CNY"
	_, err = Purchase(PurchaseInput{ID: "s", Lines: []DeliveryLine{delivery("delivery.1", "10"), cny}, By: treasury})
	want(t, err, kernel.CodeInvalidKnownValue, "/args/sources/1")
	_, err = Purchase(PurchaseInput{ID: "s", Lines: []DeliveryLine{delivery("delivery.1", "0")}, By: treasury})
	want(t, err, kernel.CodeInvalidState, "/args/sources/0")
	_, err = Purchase(PurchaseInput{ID: "s", Lines: []DeliveryLine{delivery("delivery.1", "120")},
		Settled: map[SourceKey]string{{Kind: SourceDeliveryLine, ID: "delivery.1", LineID: "L1"}: "statement.p"}, By: treasury})
	want(t, err, CodeSettlementSourceDuplicate, "/args/sources/0")
	_, err = Purchase(PurchaseInput{ID: "s", Lines: []DeliveryLine{delivery("delivery.1", "121")}, By: treasury})
	want(t, err, kernel.CodeReferenceInvalid, "/args/sources/0")
}
