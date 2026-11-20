package application

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R-D21, STL-INV-006 and CN-AT08 (tactical testCatalog: "许可付款人的收款可核销；临时第三方付款经 fund.verify_payer
// 核实（信用经理，独立于资金登记人，附证据）后可核销；未核实前 PARTY_ENTITY_MISMATCH 待核"): a receipt of agent.9 for
// customer.1's order is PARTY_ENTITY_MISMATCH until the customer's trading profile permits agent.9 (the
// application records permitted_payer and the profile revision) or a credit manager verified the payment for
// customer.1 (verified_payer); a verification for another customer does not count. The basis travels with
// the stage application when a sales statement moves it to the receivable.
func TestAThirdPartyPaysWhenPermittedOrVerified(t *testing.T) {
	a := account(t)
	f := receipt(t, "fund.3p", "agent.9", usd, "288.00", "288.00")
	in := input(t, f, a, "P1", "288.00", "0")
	_, err := Apply(in)
	want(t, err, fundrecord.CodePartyEntityMismatch)

	in.Payers = Payers{Verified: []fundrecord.PayerVerification{{Seq: 1, PayerID: "agent.9", CustomerID: "customer.2"}}}
	_, err = Apply(in)
	want(t, err, fundrecord.CodePartyEntityMismatch)

	in.Payers = Payers{Permitted: 3}
	x, err := Apply(in)
	if err != nil || x.PayerBasis != PayerPermitted || x.PayerProfileRevision != 3 {
		t.Fatalf("permitted payer: %+v %v", x, err)
	}
	in.Payers = Payers{Verified: []fundrecord.PayerVerification{{Seq: 1, PayerID: "agent.9", CustomerID: "customer.1"}}}
	if x, err = Apply(in); err != nil || x.PayerBasis != PayerVerified || x.PayerProfileRevision != 0 {
		t.Fatalf("verified payer: %+v %v", x, err)
	}
	own := input(t, receipt(t, "fund.own", "customer.1", usd, "288.00", "288.00"), a, "P1", "288.00", "0")
	own.Payers = Payers{Permitted: 3}
	if x, err := Apply(own); err != nil || x.PayerBasis != PayerCustomer {
		t.Fatalf("the customer's own receipt: %+v %v", x, err)
	}

	// A receivable of customer.1 (S5-04b): the same rule; a moved prepayment keeps its basis.
	st := &settlement.Statement{ID: "st.1", Kind: settlement.KindSales, Status: settlement.StatusConfirmed, CounterpartyID: "customer.1",
		OrderID: "order.1", OrderRevision: 1, Amount: money(t, usd, "288.00"), Installments: []settlement.Installment{{Seq: 1, StageID: "P1", Amount: money(t, usd, "288.00"),
			DueAt: treasury.At}}}
	sin := StatementInput{ID: "app.r", Fund: f, FundEntries: fundrecord.NoEntries(usd), Statement: st, Applied: map[int]kernel.Money{},
		Amount: money(t, usd, "100.00"), By: treasury}
	_, err = ApplyToStatement(sin)
	want(t, err, fundrecord.CodePartyEntityMismatch)
	sin.Payers = Payers{Permitted: 4}
	if r, err := ApplyToStatement(sin); err != nil || r.PayerBasis != PayerPermitted || r.PayerProfileRevision != 4 {
		t.Fatalf("receivable from a permitted payer: %+v %v", r, err)
	}
	moved := MoveToReceivable(st, []Application{*x}, func() string { return "app.m" }, treasury, "")
	if len(moved) != 1 || moved[0].Replacement.PayerBasis != PayerVerified {
		t.Fatalf("moved %+v", moved)
	}
	// A payable is paid to its supplier only: no third-party basis.
	pay := receipt(t, "fund.pay", "agent.9", usd, "50.00", "50.00")
	pay.Kind, pay.Direction = fundrecord.KindPayment, fundrecord.Out
	ps := &settlement.Statement{ID: "st.p", Kind: settlement.KindPurchase, Status: settlement.StatusConfirmed, CounterpartyID: "supplier.1",
		Amount: money(t, usd, "50.00"), Installments: []settlement.Installment{{Seq: 1, Amount: money(t, usd, "50.00"), DueAt: treasury.At}}}
	_, err = ApplyToStatement(StatementInput{ID: "app.p", Fund: pay, FundEntries: fundrecord.NoEntries(usd), Statement: ps,
		Applied: map[int]kernel.Money{}, Amount: money(t, usd, "50.00"), Payers: Payers{Permitted: 9}, By: treasury})
	want(t, err, fundrecord.CodePartyEntityMismatch)
}

// SM-STL-FUND fund.verify_payer (S5R-D21): a credit manager other than the recorder verifies a receipt's
// third-party payer for a customer, once per customer; the version moves by one, the state stays. The
// recorder is INDEPENDENT_REVIEW_REQUIRED; the customer's own receipt and a payment are INVALID_STATE.
func TestVerifyingAThirdPartyPayer(t *testing.T) {
	f := receipt(t, "fund.v", "agent.9", usd, "670.00", "672.00")
	manager := fundrecord.Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "manager.1"}, Epoch: 2, At: treasury.At.Add(time.Hour), Reason: "付款委托书"}
	_, err := f.VerifyPayer("customer.1", []string{"mandate"}, nil, treasury)
	want(t, err, fundrecord.CodeIndependentReviewRequired)
	v, err := f.VerifyPayer("customer.1", []string{"mandate"}, nil, manager)
	if err != nil || v.Seq != 1 || v.PayerID != "agent.9" || v.CustomerID != "customer.1" || f.Version != 2 || f.Status != fundrecord.StatusRecorded {
		t.Fatalf("verified %+v %+v %v", v, f, err)
	}
	_, err = f.VerifyPayer("customer.1", []string{"mandate"}, []fundrecord.PayerVerification{*v}, manager)
	want(t, err, kernel.CodeInvalidState)
	if v2, err := f.VerifyPayer("customer.2", []string{"agreement"}, []fundrecord.PayerVerification{*v}, manager); err != nil || v2.Seq != 2 || f.Version != 3 {
		t.Fatalf("a second customer %+v %v", v2, err)
	}
	_, err = receipt(t, "fund.c", "customer.1", usd, "1.00", "1.00").VerifyPayer("customer.1", []string{"x"}, nil, manager)
	want(t, err, kernel.CodeInvalidState)
	p := receipt(t, "fund.p", "supplier.1", usd, "1.00", "1.00")
	p.Kind = fundrecord.KindPayment
	_, err = p.VerifyPayer("customer.1", []string{"x"}, nil, manager)
	want(t, err, kernel.CodeInvalidState)
}
