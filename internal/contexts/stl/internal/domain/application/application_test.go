package application

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	usd      = kernel.MustCurrency("USD", 2)
	cny      = kernel.MustCurrency("CNY", 2)
	treasury = fundrecord.Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "treasury.1"}, Epoch: 1, At: time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)}
)

func money(t *testing.T, c kernel.Currency, s string) kernel.Money {
	t.Helper()
	m, err := kernel.ParseAmount(c, s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func want(t *testing.T, err error, code string) {
	t.Helper()
	r, ok := kernel.AsRejection(err)
	if !ok || r.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

func receipt(t *testing.T, id, payer string, c kernel.Currency, received, remitted string) *fundrecord.FundRecord {
	t.Helper()
	r, rm := money(t, c, received), money(t, c, remitted)
	f, err := fundrecord.Record(fundrecord.Input{ID: id, Kind: fundrecord.KindReceipt, CounterpartyID: payer, Currency: c, CurrencyRevision: 1,
		Account: fundrecord.BankAccount{ID: "bank", Revision: 1, OwnerKind: fundrecord.OwnerCompany, OwnerID: "company.A", Currency: c.Code(),
			Purpose: fundrecord.PurposeBoth}, Company: "company.A", Received: r, Remitted: &rm, BankReference: id, EvidenceIDs: []string{"advice"},
		By: treasury})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// SC-08's order: 30 % 288.00 on confirmation, 70 % 672.00 before shipment.
func account(t *testing.T) *orderaccount.Account {
	t.Helper()
	a, err := orderaccount.Open("order.1", 1, "h", "customer.1", 1, []orderaccount.Stage{
		{StageID: "P1", Trigger: orderaccount.TriggerOrderConfirmed, Percent: "30", Amount: money(t, usd, "288.00")},
		{StageID: "P2", Trigger: orderaccount.TriggerBeforeShipment, Percent: "70", Amount: money(t, usd, "672.00")}}, treasury, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func input(t *testing.T, f *fundrecord.FundRecord, a *orderaccount.Account, stage, amount, applied string) ApplyInput {
	return ApplyInput{ID: "app.1", Fund: f, FundEntries: fundrecord.NoEntries(f.Currency()), Account: a, StageID: stage,
		StageApplied: money(t, a.Currency(), applied), Amount: money(t, f.Currency(), amount), By: treasury}
}

// STL-INV-002 and STL-INV-006 (SC-08): 670.00 credited of 672.00 remitted; applying 672.00 to the
// stage while 2.00 is unexplained is FUND_DIFFERENCE_UNEXPLAINED, beyond that
// APPLICATION_EXCEEDS_AVAILABLE; 670.00 applies; after the 2.00 is resolved the last 2.00 applies,
// and nothing beyond the stage's 672.00 does.
func TestApplicationsStayWithinAvailableAndOpen(t *testing.T) {
	a := account(t)
	f := receipt(t, "fund.1", "customer.1", usd, "670.00", "672.00")
	_, err := Apply(input(t, f, a, "P2", "672.00", "0"))
	want(t, err, fundrecord.CodeFundDifferenceUnexplained)
	_, err = Apply(input(t, f, a, "P2", "672.01", "0"))
	want(t, err, fundrecord.CodeApplicationExceedsAvailable)
	x, err := Apply(input(t, f, a, "P2", "670.00", "0"))
	if err != nil || x.Status != StatusApplied || x.Version != 1 || x.OrderRevision != 1 || x.TargetKind != TargetOrderStage {
		t.Fatalf("670.00: %+v %v", x, err)
	}
	manager := fundrecord.Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "manager.1"}, Epoch: 1, At: treasury.At.Add(time.Hour), Reason: "银行费用"}
	if err = f.Resolve(fundrecord.ResolutionBankCharge, money(t, usd, "2.00"), []string{"advice"}, manager); err != nil {
		t.Fatal(err)
	}
	in := input(t, f, a, "P2", "2.01", "670.00")
	in.FundEntries.Applied = money(t, usd, "670.00")
	_, err = Apply(in)
	want(t, err, fundrecord.CodeApplicationExceedsAvailable)
	in.Amount = money(t, usd, "2.00")
	if _, err = Apply(in); err != nil {
		t.Fatal(err)
	}
	big := receipt(t, "fund.2", "customer.1", usd, "1000.00", "1000.00")
	_, err = Apply(input(t, big, a, "P2", "672.01", "0"))
	want(t, err, fundrecord.CodeApplicationExceedsOpen)
	_, err = Apply(input(t, big, a, "P1", "88.00", "200.00"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Apply(input(t, big, a, "P1", "88.01", "200.00"))
	want(t, err, fundrecord.CodeApplicationExceedsOpen)
	_, err = Apply(input(t, big, a, "P9", "1.00", "0"))
	want(t, err, kernel.CodeReferenceInvalid)
	_, err = Apply(input(t, big, a, "P1", "0", "0"))
	want(t, err, kernel.CodeInvalidKnownValue)
}

// CN-AT08 and STL-INV-007: a payer other than the order's customer is PARTY_ENTITY_MISMATCH; another
// currency is FX_SCOPE_MISMATCH (cross-currency write-off is C8's); a non-goods receipt is not applied.
func TestPayerCurrencyAndKindOfTheFund(t *testing.T) {
	a := account(t)
	_, err := Apply(input(t, receipt(t, "fund.3", "agent.9", usd, "288.00", "288.00"), a, "P1", "288.00", "0"))
	want(t, err, fundrecord.CodePartyEntityMismatch)
	_, err = Apply(input(t, receipt(t, "fund.4", "customer.1", cny, "2000.00", "2000.00"), a, "P1", "1.00", "0"))
	want(t, err, fundrecord.CodeFXScopeMismatch)
	ng := receipt(t, "fund.5", "customer.1", usd, "288.00", "288.00")
	ng.Kind = fundrecord.KindNonGoods
	_, err = Apply(input(t, ng, a, "P1", "1.00", "0"))
	want(t, err, kernel.CodeInvalidState)
}

// SM-STL-APPLICATION: unapply moves applied → unapplied once and keeps the record.
func TestUnapplyOnce(t *testing.T) {
	x, err := Apply(input(t, receipt(t, "fund.6", "customer.1", usd, "288.00", "288.00"), account(t), "P1", "288.00", "0"))
	if err != nil {
		t.Fatal(err)
	}
	by := treasury
	by.Reason = "客户退汇，撤销核销"
	if err = x.Unapply(by); err != nil || x.Status != StatusUnapplied || x.Version != 2 || x.Unapplied == nil || x.Stands() || x.Amount.Amount() != "288.00" {
		t.Fatalf("unapplied %+v %v", x, err)
	}
	want(t, x.Unapply(by), kernel.CodeInvalidState)
}
