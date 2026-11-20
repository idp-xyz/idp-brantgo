package application

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/charge"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// payment is a CNY payment of payee.
func payment(t *testing.T, id, payee, amount string) *fundrecord.FundRecord {
	t.Helper()
	f, err := fundrecord.Record(fundrecord.Input{ID: id, Kind: fundrecord.KindPayment, CounterpartyID: payee, Currency: cny, CurrencyRevision: 1,
		Account: fundrecord.BankAccount{ID: "bank.cny", Revision: 1, OwnerKind: fundrecord.OwnerCompany, OwnerID: "company.A", Currency: "CNY",
			Purpose: fundrecord.PurposeBoth}, Company: "company.A", Received: money(t, cny, amount), BankReference: id, EvidenceIDs: []string{"advice"},
		By: treasury})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// freight is S-A's freight: a confirmed CNY 650 bill of forwarder.1 (or an internal charge).
func freight(t *testing.T, source charge.Source, confirmed bool) *charge.Charge {
	t.Helper()
	c, err := charge.Record(charge.RecordInput{ID: "freight.sa", Status: charge.StatusPending, Source: source, Amount: money(t, cny, "650.00"),
		Conversion: charge.Conversion{Kind: charge.ConversionSameCurrency}, CostComponent: charge.ComponentPreCarriage, OccurredOn: treasury.At,
		AnalysisOn: treasury.At, Basis: "实际运费", EvidenceIDs: []string{"bill"}, Beneficiaries: []charge.Target{{Kind: charge.TargetOrder,
			OrderID: "order.sa"}}, By: treasury})
	if err == nil && confirmed {
		err = c.Confirm(treasury, []string{"bill"})
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// S5R-D19 / STL-INV-002 (target kind charge): an outgoing payment of the charge's source party pays a confirmed charge,
// within what is open (APPLICATION_EXCEEDS_OPEN) and what the payment has available (APPLICATION_EXCEEDS_AVAILABLE);
// another payee is PARTY_ENTITY_MISMATCH; a receipt, a pending or internal charge INVALID_STATE; another currency needs
// a settlement rate (FX_SCOPE_MISMATCH).
func TestS5RAPaymentPaysAConfirmedCharge(t *testing.T) {
	bill := charge.Source{Kind: charge.SourceSupplierBill, PartyID: "forwarder.1", Reference: "BILL-1"}
	c := freight(t, bill, true)
	pay := payment(t, "pay.freight", "forwarder.1", "650.00")
	in := ChargeInput{ID: "app.freight", Fund: pay, FundEntries: fundrecord.NoEntries(cny), Charge: c, Applied: money(t, cny, "0"),
		Amount: money(t, cny, "400.00"), By: treasury}
	x, err := ApplyToCharge(in)
	if err != nil {
		t.Fatal(err)
	}
	if x.TargetKind != TargetCharge || x.ChargeID != "freight.sa" || x.Amount.String() != "400.00 CNY" || x.OrderID != "" || x.StatementID != "" {
		t.Fatalf("application %+v", x)
	}
	in.Applied = money(t, cny, "400.00")
	in.Amount = money(t, cny, "250.01")
	_, err = ApplyToCharge(in)
	want(t, err, fundrecord.CodeApplicationExceedsOpen)
	in.Amount, in.FundEntries.Applied = money(t, cny, "250.00"), money(t, cny, "500.00")
	_, err = ApplyToCharge(in)
	want(t, err, fundrecord.CodeApplicationExceedsAvailable)
	in.FundEntries = fundrecord.NoEntries(cny)
	if open := ChargeOpen(c, in.Applied); open.String() != "250.00 CNY" || c.PaymentStatus(in.Applied) != charge.PaymentPartlyPaid ||
		c.PaymentStatus(money(t, cny, "650.00")) != charge.PaymentPaid || c.PaymentStatus(money(t, cny, "0")) != charge.PaymentUnpaid {
		t.Fatalf("open %s", open)
	}

	other := in
	other.Fund = payment(t, "pay.other", "forwarder.2", "650.00")
	_, err = ApplyToCharge(other)
	want(t, err, fundrecord.CodePartyEntityMismatch)
	pending := in
	pending.Charge = freight(t, bill, false)
	_, err = ApplyToCharge(pending)
	want(t, err, "INVALID_STATE")
	internal := in
	internal.Charge = freight(t, charge.Source{Kind: charge.SourceInternal}, true)
	_, err = ApplyToCharge(internal)
	if r, ok := kernel.AsRejection(err); !ok || r.Code != "INVALID_STATE" || !strings.Contains(r.Fields[0].Reason, "internal") {
		t.Fatalf("internal: %v", err)
	}
	rcpt := in
	rcpt.Fund = receipt(t, "fund.1", "forwarder.1", cny, "650.00", "650.00")
	_, err = ApplyToCharge(rcpt)
	want(t, err, "INVALID_STATE")
	usdPay := in
	usdPay.Fund = payment(t, "pay.usd", "forwarder.1", "100.00")
	usdPay.Fund.Received = money(t, usd, "100.00")
	usdPay.Fund.Remitted, usdPay.Fund.BankCharges = usdPay.Fund.Received, money(t, usd, "0")
	usdPay.FundEntries, usdPay.Amount = fundrecord.NoEntries(usd), money(t, usd, "10.00")
	_, err = ApplyToCharge(usdPay)
	want(t, err, fundrecord.CodeFXScopeMismatch)
	reversed := in
	reversed.Fund = payment(t, "pay.rev", "forwarder.1", "650.00")
	manager := treasury
	manager.By.ID = "manager.1"
	if err := reversed.Fund.Reverse(nil, nil, nil, manager); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyToCharge(reversed)
	want(t, err, "INVALID_STATE")
}

// S5R-D19 (b): a receipt in the order's currency records the settlement rate into the order's analysis currency when it
// is applied; a rate of another direction, an order analysed in its own currency or a converted receipt refuse it
// (FX_SCOPE_MISMATCH at /args/settlement_fx_rate).
func TestS5RTheSettlementRateOfAReceiptInTheOrdersCurrency(t *testing.T) {
	rate := func(from, to kernel.Currency, v string) *FXInput {
		r, err := kernel.NewExchangeRate(from, to, kernel.MustDecimal(v))
		if err != nil {
			t.Fatal(err)
		}
		return &FXInput{Rate: r, Use: RateUse{ID: "fx.settlement", Revision: 2, Hash: strings.Repeat("d", 64)}}
	}
	s, err := settlementRate(nil, usd, &SettlementInput{FX: rate(usd, cny, "6.9"), Analysis: "CNY"})
	if err != nil || s == nil || s.Value.String() != "6.9" || s.Currency != cny || s.Use.Revision != 2 {
		t.Fatalf("rate %+v %v", s, err)
	}
	for name, in := range map[string]*SettlementInput{
		"inverse":            {FX: rate(cny, usd, "0.145"), Analysis: "CNY"},
		"own currency":       {FX: rate(usd, cny, "6.9"), Analysis: "USD"},
		"no analysis":        {FX: rate(usd, cny, "6.9")},
		"another currency":   {FX: rate(usd, kernel.MustCurrency("EUR", 2), "0.9"), Analysis: "CNY"},
		"a converted fund":   nil,
		"none is no refusal": {Analysis: "CNY"},
	} {
		var conv *Conversion
		if name == "a converted fund" {
			conv, in = &Conversion{}, &SettlementInput{FX: rate(usd, cny, "6.9"), Analysis: "CNY"}
		}
		s, err := settlementRate(conv, usd, in)
		if name == "none is no refusal" {
			if s != nil || err != nil {
				t.Fatalf("%s: %+v %v", name, s, err)
			}
			continue
		}
		if r, ok := kernel.AsRejection(err); !ok || r.Code != fundrecord.CodeFXScopeMismatch || r.Fields[0].Path != "/args/settlement_fx_rate" {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
