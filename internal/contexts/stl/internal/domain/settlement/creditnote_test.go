package settlement

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Credit notes and settlement reversal (S5 unit C8, WBS S5-18; SM-STL-CREDIT-NOTE, SM-STL-SETTLEMENT;
// STL-INV-008, STL-INV-009; baseline 4.5, 4.7 S-B). Python V0.50 has no STL.

var (
	clerk   = fundrecord.Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "sales.1"}, Epoch: 1, At: treasury.At}
	manager = fundrecord.Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "manager.1"}, Epoch: 1, At: treasury.At, Reason: "折让经核准"}
)

// sb is baseline 4.7 S-B's sale: 100 PCS at USD 10.00 = 1,000.00, due 30 days after shipment.
func sb(t *testing.T) *Statement {
	t.Helper()
	a := account(t, orderaccount.Stage{StageID: "P1", Trigger: orderaccount.TriggerAfterShipment, Days: 30, Percent: "100",
		Amount: money(t, "1000.00")})
	st, err := sales(t, a, nil, line("S1", "100", "100000"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func creditIn(st *Statement, id, amount string, t *testing.T) CreditInput {
	return CreditInput{ID: id, Statement: st, OrderID: "order.1", Kind: CreditPriceAllowance, Amount: money(t, amount), Basis: "色差折让",
		EvidenceIDs: []string{"claim.1"}, Applied: map[int]kernel.Money{}, By: clerk}
}

// S-B (baseline 4.7, STL-INV-008 positive "折让 100.00 经信用经理确认后冲减应收"): the sales clerk requests a
// price allowance of 100.00 on the 1,000.00 statement; its requester may not confirm it
// (INDEPENDENT_REVIEW_REQUIRED); the credit manager does: confirmed, it counts against the statement's sales
// (net 1,000.00 − 100.00 = 900.00) and leaves 900.00 of credit room while unapplied. The statement's lines
// keep their quantity: a credit note writes nothing to them.
func TestSBCreditNoteIsRequestedAndConfirmedIndependently(t *testing.T) {
	st := sb(t)
	n, err := RequestCredit(creditIn(st, "cn.1", "100.00", t))
	if err != nil || n.Status != CreditRequested || n.Version != 1 || n.StatementID != st.ID || n.OrderID != "order.1" ||
		n.Amount.String() != "100.00 USD" || n.OwnerActor != "sales.1" || n.Effective(st) {
		t.Fatalf("%+v %v", n, err)
	}
	room := CreditRoom(st, map[int]kernel.Money{}, nil, nil, n.ID)
	err = n.Decide(DecisionConfirm, st, room, clerk)
	want(t, err, fundrecord.CodeIndependentReviewRequired, "/args/credit_note_id")
	if err = n.Decide(DecisionConfirm, st, room, manager); err != nil || n.Status != CreditConfirmed || n.Version != 2 || n.Decided == nil ||
		n.Decided.Reason != "折让经核准" || !n.Effective(st) {
		t.Fatalf("%+v %v", n, err)
	}
	err = n.Decide(DecisionReject, st, room, manager)
	want(t, err, kernel.CodeInvalidState, "/args/credit_note_id")
	if net := must(st.Net.Sub(n.Amount)); net.String() != "900.00 USD" || st.Lines[0].Quantity.String() != "100" {
		t.Fatalf("net %s, quantity %s", net, st.Lines[0].Quantity)
	}
	notes := []CreditNote{*n}
	if r := CreditRoom(st, map[int]kernel.Money{}, notes, map[string]kernel.Money{}, ""); r.String() != "900.00 USD" {
		t.Fatalf("room with the note unapplied %s", r)
	}
	// Applied (the receivable's installment holds it), its rest is 0 and the room is what is open.
	applied := map[int]kernel.Money{1: money(t, "100.00")}
	if r := CreditRoom(st, applied, notes, map[string]kernel.Money{"cn.1": money(t, "100.00")}, ""); r.String() != "900.00 USD" {
		t.Fatalf("room with the note applied %s", r)
	}
}

// STL-INV-008 negatives: a credit note beyond the room (APPLICATION_EXCEEDS_OPEN: the receivable less the
// confirmed notes not yet applied), of another order (INVALID_KNOWN_VALUE), of a purchase statement or a
// reversed one (INVALID_STATE); a second note confirmed after the first took the room is refused at
// confirmation; a rejection needs no room.
func TestCreditNotesStayWithinTheReceivable(t *testing.T) {
	st := sb(t)
	_, err := RequestCredit(creditIn(st, "cn.big", "1000.01", t))
	want(t, err, fundrecord.CodeApplicationExceedsOpen, "/args/amount")
	in := creditIn(st, "cn.x", "10.00", t)
	in.OrderID = "order.2"
	_, err = RequestCredit(in)
	want(t, err, kernel.CodeInvalidKnownValue, "/args/order_id")

	first, err := RequestCredit(creditIn(st, "cn.1", "600.00", t))
	if err != nil {
		t.Fatal(err)
	}
	second, err := RequestCredit(creditIn(st, "cn.2", "500.00", t))
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Decide(DecisionConfirm, st, CreditRoom(st, nil, []CreditNote{*first, *second}, nil, first.ID), manager); err != nil {
		t.Fatal(err)
	}
	notes := []CreditNote{*first, *second}
	err = second.Decide(DecisionConfirm, st, CreditRoom(st, nil, notes, nil, second.ID), manager)
	want(t, err, fundrecord.CodeApplicationExceedsOpen, "/args/amount")
	if err = second.Decide(DecisionReject, st, CreditRoom(st, nil, notes, nil, second.ID), manager); err != nil || second.Status != CreditRejected {
		t.Fatalf("%+v %v", second, err)
	}
	third := creditIn(st, "cn.3", "400.01", t)
	third.Notes = []CreditNote{*first, *second}
	_, err = RequestCredit(third)
	want(t, err, fundrecord.CodeApplicationExceedsOpen, "/args/amount")
	third.Amount = money(t, "400.00")
	if _, err = RequestCredit(third); err != nil {
		t.Fatal(err)
	}

	p := &Statement{ID: "purchase.1", Kind: KindPurchase, Status: StatusConfirmed, Amount: money(t, "10.00")}
	_, err = RequestCredit(creditIn(p, "cn.p", "1.00", t))
	want(t, err, kernel.CodeInvalidState, "/args/statement_id")
	st.Status = StatusReversed
	_, err = RequestCredit(creditIn(st, "cn.r", "1.00", t))
	want(t, err, kernel.CodeInvalidState, "/args/statement_id")
}

// SM-STL-SETTLEMENT and STL-INV-009: the confirmer may not reverse its statement
// (INDEPENDENT_REVIEW_REQUIRED); while something stands applied it is SETTLEMENT_HAS_APPLICATIONS (unapply
// first), while a credit note waits for its decision INVALID_STATE; then the statement and every line are
// reversed, one version on (their sources are free again); reversed is terminal.
func TestAStatementIsReversedWithoutApplicationsByAnotherPerson(t *testing.T) {
	st := sb(t)
	r := Reversal{Reason: "单价错误，按正确价格重新结算", By: manager}
	err := st.Reverse(nil, nil, Reversal{Reason: "x", By: treasury})
	want(t, err, fundrecord.CodeIndependentReviewRequired, "/args/statement_id")
	err = st.Reverse([]string{"app.1", "app.cn"}, nil, r)
	want(t, err, CodeSettlementHasApplications, "/args/statement_id")
	if c, ok := Codes[CodeSettlementHasApplications]; !ok || c != kernel.CategoryConflict {
		t.Fatalf("SETTLEMENT_HAS_APPLICATIONS is %v", c)
	}
	err = st.Reverse(nil, []string{"cn.1"}, r)
	want(t, err, kernel.CodeInvalidState, "/args/statement_id")
	if st.Status != StatusConfirmed || st.Version != 1 {
		t.Fatalf("refused reversals changed the statement %+v", st)
	}
	if err = st.Reverse(nil, nil, r); err != nil || st.Status != StatusReversed || st.Version != 2 || st.Lines[0].Status != LineReversed {
		t.Fatalf("%+v %v", st, err)
	}
	err = st.Reverse(nil, nil, r)
	want(t, err, kernel.CodeInvalidState, "/args/statement_id")
	n := &CreditNote{ID: "cn.1", StatementID: st.ID, Status: CreditRequested, Version: 1, Amount: money(t, "1.00"), Requested: clerk}
	err = n.Decide(DecisionConfirm, st, money(t, "1000.00"), manager)
	want(t, err, kernel.CodeInvalidState, "/args/credit_note_id")
	if err = n.Decide(DecisionReject, st, money(t, "0"), manager); err != nil || n.Effective(st) {
		t.Fatalf("a requested note of a reversed statement is rejected: %+v %v", n, err)
	}
}
