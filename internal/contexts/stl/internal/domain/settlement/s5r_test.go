package settlement

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R unit R8 (simulated sign-off E0 of 2026-11-13): S5R-D22 credit notes on opening receivables, S5R-D46(2) the tax
// split of a credit, S5R-D20 payable adjustments (STL-INV-008, STL-INV-013). Python V0.50 has no STL.

// S5R-D22 / STL-INV-008 positive "期初应收开贷项": a credit note of an opening receivable binds no order (order_id left
// out); naming one is INVALID_KNOWN_VALUE at /args/order_id.
func TestS5ROpeningReceivableTakesACreditNoteWithoutAnOrder(t *testing.T) {
	_, st, err := RecordOpening(openingIn(t, OpeningReceivable, "AR-2026-0107", "7000.00", "7000.00",
		owing(t, "7000.00", time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC))))
	if err != nil {
		t.Fatal(err)
	}
	in := creditIn(st, "cn.opening", "500.00", t)
	_, err = RequestCredit(in)
	want(t, err, kernel.CodeInvalidKnownValue, "/args/order_id")
	in.OrderID = ""
	n, err := RequestCredit(in)
	if err != nil || n.OrderID != "" || n.StatementID != st.ID {
		t.Fatalf("%+v %v", n, err)
	}
	if err = n.Decide(DecisionConfirm, st, CreditRoom(st, map[int]kernel.Money{}, nil, nil, n.ID), manager); err != nil || !n.Effective(st) {
		t.Fatalf("confirm %v", err)
	}
	net, tax := n.Split(st)
	if net.String() != "500.00 USD" || !tax.IsZero() {
		t.Fatalf("split %s %s", net, tax)
	}
}

// S5R-D46(2) / STL-INV-008 "结算单带税额时贷项按净额与税额比例拆分（HALF_UP，各自一次）": on net 1,000.00 + tax 130.00, a credit
// of 113.00 is 100.00 + 13.00; of 100.00, 88.50 (88.4955… HALF_UP) + 11.50; of 0.01, 0.01 + 0.00 — the parts always add
// up to the credit; without tax the credit is all net.
func TestS5RACreditSplitsByTheStatementsNetAndTax(t *testing.T) {
	st := &Statement{Net: money(t, "1000.00"), Tax: money(t, "130.00"), Amount: money(t, "1130.00")}
	for amount, parts := range map[string][2]string{"113.00": {"100.00 USD", "13.00 USD"}, "100.00": {"88.50 USD", "11.50 USD"},
		"0.01": {"0.01 USD", "0.00 USD"}, "1130.00": {"1000.00 USD", "130.00 USD"}} {
		net, tax := SplitTax(money(t, amount), st)
		if net.String() != parts[0] || tax.String() != parts[1] {
			t.Fatalf("%s: %s + %s, want %s + %s", amount, net, tax, parts[0], parts[1])
		}
	}
	untaxed := &Statement{Net: money(t, "1000.00"), Tax: money(t, "0"), Amount: money(t, "1000.00")}
	if net, tax := SplitTax(money(t, "100.00"), untaxed); net.String() != "100.00 USD" || !tax.IsZero() {
		t.Fatalf("untaxed %s %s", net, tax)
	}
}

// payable is a confirmed purchase statement of 600.00 of supplier.1, one installment.
func payable(t *testing.T) *Statement {
	return &Statement{ID: "statement.purchase", Kind: KindPurchase, Status: StatusConfirmed, Version: 1, CounterpartyID: "supplier.1",
		Net: money(t, "600.00"), Tax: money(t, "0"), Amount: money(t, "600.00"), Installments: []Installment{{Seq: 1, Amount: money(t, "600.00")}},
		Confirmed: treasury}
}

// S5R-D20 / STL-INV-013: treasury requests a price allowance on a payable (within what is open: APPLICATION_EXCEEDS_OPEN);
// its requester may not confirm it (INDEPENDENT_REVIEW_REQUIRED); the credit manager does, once; a sales statement is no
// payable (INVALID_STATE); confirming is refused when the payable no longer has the amount open.
func TestS5RPayableAdjustmentIsRequestedAndConfirmedIndependently(t *testing.T) {
	st := payable(t)
	in := AdjustmentInput{ID: "adj.1", Statement: st, Kind: AdjustmentPriceAllowance, Amount: money(t, "50.00"), PurchaseInvoiceID: "invoice.red.1",
		Basis: "工厂折让", EvidenceIDs: []string{"red.invoice"}, Applied: map[int]kernel.Money{1: money(t, "560.00")}, By: treasury}
	_, err := RequestAdjustment(in)
	want(t, err, fundrecord.CodeApplicationExceedsOpen, "/args/amount")
	in.Applied = map[int]kernel.Money{1: money(t, "500.00")}
	a, err := RequestAdjustment(in)
	if err != nil || a.Status != AdjustmentRequested || a.Version != 1 || a.Effective(st) || a.OwnerActor != "treasury.1" {
		t.Fatalf("%+v %v", a, err)
	}
	err = a.Decide(DecisionConfirm, st, money(t, "100.00"), treasury)
	want(t, err, fundrecord.CodeIndependentReviewRequired, "/args/adjustment_id")
	err = a.Decide(DecisionConfirm, st, money(t, "49.99"), manager)
	want(t, err, fundrecord.CodeApplicationExceedsOpen, "/args/amount")
	if err = a.Decide(DecisionConfirm, st, money(t, "100.00"), manager); err != nil || a.Status != AdjustmentConfirmed || a.Version != 2 ||
		!a.Effective(st) {
		t.Fatalf("%+v %v", a, err)
	}
	err = a.Decide(DecisionReject, st, money(t, "100.00"), manager)
	want(t, err, kernel.CodeInvalidState, "/args/adjustment_id")
	sales := sb(t)
	in.Statement = sales
	_, err = RequestAdjustment(in)
	want(t, err, kernel.CodeInvalidState, "/args/statement_id")
	// A rejection needs no room; a reversed statement keeps a confirmed adjustment, no longer effective.
	b, _ := RequestAdjustment(AdjustmentInput{ID: "adj.2", Statement: st, Kind: AdjustmentUnderInvoiced, Amount: money(t, "10.00"),
		PurchaseInvoiceID: "invoice.blue.2", Basis: "少开", EvidenceIDs: []string{"x"}, Applied: map[int]kernel.Money{}, By: treasury})
	if err = b.Decide(DecisionReject, st, money(t, "0"), manager); err != nil || b.Status != AdjustmentRejected {
		t.Fatalf("reject %v", err)
	}
	st.Status = StatusReversed
	if a.Effective(st) {
		t.Fatal("an adjustment of a reversed statement counts")
	}
}
