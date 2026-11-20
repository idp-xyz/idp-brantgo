package api

import "testing"

// The contracts S5R unit R8 adds (simulated sign-off E0, 2026-11-13): payable_adjustment.request and .confirm
// (S5R-D20), fund.reverse (S5R-D22), application.apply to a charge and with a settlement_fx_rate (S5R-D19), charge.record
// cost_component and recharged (S5R-D24), credit_note.request without an order (S5R-D22).
func TestS5R8Contracts(t *testing.T) {
	rate := &ConfigRef{Kind: KindFXRate, ConfigID: "fx.usd-cny.settlement", Revision: 2}
	request := RequestPayableAdjustment{IdempotencyKey: "k1", AdjustmentID: "adj.1", StatementID: "st.p", AdjustmentKind: AdjustmentPriceAllowance,
		Amount: "50.00", PurchaseInvoiceID: "invoice.red.1", Basis: "工厂折让（合成）", EvidenceIDs: []string{"red.1"}}
	confirm := ConfirmPayableAdjustment{IdempotencyKey: "k1", AdjustmentID: "adj.1", ExpectedVersion: 1, Decision: DecisionConfirm, Reason: "核对红票"}
	reverse := ReverseFund{IdempotencyKey: "k1", FundRecordID: "fund.1", ExpectedVersion: 1, Reason: "银行退回（合成）"}
	pay := Apply{IdempotencyKey: "k1", ApplicationID: "app.1", FundRecordID: "pay.1", Target: ApplicationTarget{Kind: TargetCharge, ChargeID: "freight.1"},
		Amount: "650.00"}
	receipt := Apply{IdempotencyKey: "k1", ApplicationID: "app.2", FundRecordID: "fund.usd", Target: ApplicationTarget{Kind: TargetReceivable,
		StatementID: "st.1"}, Amount: "400.00", SettlementFXRate: rate}
	for _, c := range []struct {
		name string
		cmd  interface{ Validate() error }
		want string
	}{
		{"request an adjustment", request, "OK"},
		{"under-invoicing", func() RequestPayableAdjustment { c := request; c.AdjustmentKind = AdjustmentUnderInvoiced; return c }(), "OK"},
		{"another kind", func() RequestPayableAdjustment { c := request; c.AdjustmentKind = "refund"; return c }(), "SCHEMA_INVALID /args/kind"},
		{"no invoice", func() RequestPayableAdjustment { c := request; c.PurchaseInvoiceID = ""; return c }(),
			"SCHEMA_INVALID /args/purchase_invoice_id"},
		{"no evidence", func() RequestPayableAdjustment { c := request; c.EvidenceIDs = nil; return c }(), "SCHEMA_INVALID /args/evidence_ids"},
		{"a negative amount", func() RequestPayableAdjustment { c := request; c.Amount = "-1.00"; return c }(), "SCHEMA_INVALID /args/amount"},
		{"confirm", confirm, "OK"},
		{"confirm without a reason", func() ConfirmPayableAdjustment { c := confirm; c.Reason = ""; return c }(), "SCHEMA_INVALID /args/reason"},
		{"another decision", func() ConfirmPayableAdjustment { c := confirm; c.Decision = "approve"; return c }(), "SCHEMA_INVALID /args/decision"},
		{"reverse", reverse, "OK"},
		{"reverse without a reason", func() ReverseFund { c := reverse; c.Reason = ""; return c }(), "SCHEMA_INVALID /args/reason"},
		{"reverse at version 0", func() ReverseFund { c := reverse; c.ExpectedVersion = 0; return c }(), "SCHEMA_INVALID /args/expected_version"},
		{"pay a charge", pay, "OK"},
		{"pay a charge with an order", func() Apply { c := pay; c.Target.OrderID = "o"; return c }(), "SCHEMA_INVALID /args/target/charge_id"},
		{"a charge ID on a payable", func() Apply {
			c := pay
			c.Target = ApplicationTarget{Kind: TargetPayable, StatementID: "st.p", ChargeID: "x"}
			return c
		}(), "SCHEMA_INVALID /args/target/charge_id"},
		{"a receipt with its settlement rate", receipt, "OK"},
		{"a settlement rate on a charge", func() Apply { c := pay; c.SettlementFXRate = rate; return c }(), "SCHEMA_INVALID /args/settlement_fx_rate"},
		{"a settlement rate of another kind", func() Apply {
			c := receipt
			c.SettlementFXRate = &ConfigRef{Kind: KindCurrency, ConfigID: "CNY", Revision: 1}
			return c
		}(), "SCHEMA_INVALID /args/settlement_fx_rate/kind"},
	} {
		if got := code(c.cmd.Validate()); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	fee, cur := ConfigRef{Kind: KindFeeCategory, ConfigID: "freight", Revision: 1}, ConfigRef{Kind: KindCurrency, ConfigID: "CNY", Revision: 1}
	charge := RecordCharge{IdempotencyKey: "k", ChargeID: "c", Status: ChargeEstimated, Source: ChargeSource{Kind: ChargeSourceInternal},
		Beneficiaries: []ChargeTarget{{Kind: ChargeTargetOrder, OrderID: "o"}}, FeeCategory: &fee, Currency: &cur,
		Conversion: &ChargeConversion{Kind: ConversionSameCurrency}, Amount: "60.00", OccurredOn: "2026-11-12", AnalysisOn: "2026-11-12", Basis: "运费"}
	if got := code(charge.Validate()); got != "SCHEMA_INVALID /args/cost_component" {
		t.Fatalf("a new charge without its cost component: %s", got)
	}
	charge.CostComponent, charge.Recharged = ComponentMainCarriage, true
	if got := code(charge.Validate()); got != "OK" {
		t.Fatalf("a recharged main carriage: %s", got)
	}
	bill := RecordCharge{IdempotencyKey: "k", ChargeID: "c", ExpectedVersion: 1, Status: ChargePending, Source: ChargeSource{Kind: ChargeSourceInternal},
		CostComponent: ComponentOther, Amount: "60.00", OccurredOn: "2026-11-12", AnalysisOn: "2026-11-12", Basis: "账单", EvidenceIDs: []string{"b"}}
	if got := code(bill.Validate()); got != "SCHEMA_INVALID /args/cost_component" {
		t.Fatalf("a bill names no component: %s", got)
	}
	credit := RequestCreditNote{IdempotencyKey: "k", CreditNoteID: "cn.1", CreditKind: CreditPriceAllowance, StatementID: "ob.1", Amount: "10.00",
		Basis: "期初折让", EvidenceIDs: []string{"e"}}
	if got := code(credit.Validate()); got != "OK" {
		t.Fatalf("a credit note without an order (S5R-D22): %s", got)
	}
}
