package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Cross-currency applications and credit notes as sources (S5 unit C8; STL-INV-007, STL-INV-008; UL-INV-22;
// MD-10; baseline 4.5 业务结算汇率差额, 4.6 S-A, 4.7 S-B). Python V0.50 has no STL.

// cnyToUSD is a POL settlement rate CNY → USD: one CNY is 0.145454545455 USD (1 ÷ 6.875 at FactorFormat's
// 12 places).
func cnyToUSD(t *testing.T) kernel.ExchangeRate {
	t.Helper()
	x, err := kernel.ParseExchangeRate(cny, usd, "0.145454545455")
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func settlementFX(t *testing.T, basis *AnalysisBasis) *FXInput {
	return &FXInput{Rate: cnyToUSD(t), Use: RateUse{ID: "cny.usd.settlement", Revision: 1, Hash: "h"}, Basis: basis}
}

// analysedInCNY is baseline 4.6 S-A's order analysis rate: one USD is 7.0 CNY.
var analysedInCNY = &AnalysisBasis{OrderCurrency: "USD", Currency: cny, Rate: kernel.MustDecimal("7.0")}

// S-A's 800.00 in two receivables of 400.00, received in CNY at the bank's 6.875 and applied at the
// settlement rate (STL-INV-007 positive "CNY 到账按 settlement 汇率核销 USD 应收并单列汇率差额"): CNY 2,750.00 gives
// USD 400.00 (2,750 × 0.145454545455 = 400.00000000125, HALF_UP), which the installment receives; the fund
// gives 2,750.00 — its available balance is counted in CNY. Against the analysis rate the 400.00 were worth
// CNY 2,800.00: the business settlement FX difference is −50.00 CNY, twice −100.00 as S-A's "相对订单分析收入 CNY
// 7,000 单列差额 -100" — an item of its own, not a receipt and not revenue.
func TestACNYReceiptPaysAUSDReceivableAtTheSettlementRate(t *testing.T) {
	st := statementOf(t, brcAccount(t), "96000")
	f := receipt(t, "fund.cny", "customer.1", cny, "5500.00", "5500.00")
	x, err := ApplyToStatement(StatementInput{ID: "app.fx", Fund: f, FundEntries: fundrecord.NoEntries(cny), Statement: st, StageID: "P2",
		Applied: map[int]kernel.Money{}, Amount: money(t, cny, "2750.00"), FX: settlementFX(t, analysedInCNY), By: treasury})
	if err != nil {
		t.Fatal(err)
	}
	c := x.Conversion
	if x.Amount.String() != "400.00 USD" || x.Taken().String() != "2750.00 CNY" || c == nil || c.Rate.ID != "cny.usd.settlement" ||
		c.Value.String() != "0.145454545455" || c.Analysis == nil || c.Analysis.Currency != cny || c.Analysis.Rate.String() != "7" ||
		c.Analysis.Difference == nil || c.Analysis.Difference.String() != "-50.00 CNY" || x.StageID != "P2" || x.InstallmentSeq != 2 {
		t.Fatalf("%+v %+v", x, c)
	}

	// The fund answers in its own currency: 5,500.00 available, 5,600.00 is too much.
	_, err = ApplyToStatement(StatementInput{ID: "app.fx2", Fund: f, FundEntries: fundrecord.NoEntries(cny), Statement: st, StageID: "P2",
		Applied: map[int]kernel.Money{}, Amount: money(t, cny, "5600.00"), FX: settlementFX(t, analysedInCNY), By: treasury})
	want(t, err, fundrecord.CodeApplicationExceedsAvailable)
	// The installment answers in its: CNY 4,700.00 would give USD 683.64 on 672.00 open.
	_, err = ApplyToStatement(StatementInput{ID: "app.fx3", Fund: f, FundEntries: fundrecord.NoEntries(cny), Statement: st, StageID: "P2",
		Applied: map[int]kernel.Money{}, Amount: money(t, cny, "4700.00"), FX: settlementFX(t, analysedInCNY), By: treasury})
	want(t, err, fundrecord.CodeApplicationExceedsOpen)
}

// MD-10 / STL-INV-007 negatives: without a settlement rate a fund in another currency is
// FX_SCOPE_MISMATCH at /args/fx_rate (never a default of 1); a rate of the opposite direction is never
// inverted; a rate for a fund already in the target's currency is refused as well.
func TestCrossCurrencyNeedsExactlyThatDirection(t *testing.T) {
	st := statementOf(t, brcAccount(t), "96000")
	cnyFund := receipt(t, "fund.cny", "customer.1", cny, "100.00", "100.00")
	usdFund := receipt(t, "fund.usd", "customer.1", usd, "100.00", "100.00")
	in := StatementInput{ID: "app", Fund: cnyFund, FundEntries: fundrecord.NoEntries(cny), Statement: st, Applied: map[int]kernel.Money{},
		Amount: money(t, cny, "10.00"), By: treasury}
	_, err := ApplyToStatement(in)
	wantAt(t, err, fundrecord.CodeFXScopeMismatch, "/args/fx_rate")

	inverse, err := kernel.ParseExchangeRate(usd, cny, "6.875")
	if err != nil {
		t.Fatal(err)
	}
	in.FX = &FXInput{Rate: inverse, Use: RateUse{ID: "usd.cny", Revision: 1, Hash: "h"}}
	_, err = ApplyToStatement(in)
	wantAt(t, err, fundrecord.CodeFXScopeMismatch, "/args/fx_rate")

	in.Fund, in.FundEntries, in.Amount, in.FX = usdFund, fundrecord.NoEntries(usd), money(t, usd, "10.00"), settlementFX(t, nil)
	_, err = ApplyToStatement(in)
	wantAt(t, err, fundrecord.CodeFXScopeMismatch, "/args/fx_rate")

	// The same holds for a prepayment on an order stage.
	_, err = Apply(ApplyInput{ID: "app", Fund: cnyFund, FundEntries: fundrecord.NoEntries(cny), Account: brcAccount(t), StageID: "P1",
		StageApplied: money(t, usd, "0"), Amount: money(t, cny, "10.00"), By: treasury})
	wantAt(t, err, fundrecord.CodeFXScopeMismatch, "/args/fx_rate")
}

func wantAt(t *testing.T, err error, code, path string) {
	t.Helper()
	want(t, err, code)
	if r, _ := kernel.AsRejection(err); r.Fields[0].Path != path {
		t.Fatalf("%v at %s, want %s", err, r.Fields[0].Path, path)
	}
}

// HALF_UP at the target's minor unit, once, from the exact product; the difference against an analysis in
// the order's own currency is 0 (the conversion is the analysis value); against an analysis currency that is
// neither the fund's nor the target's no rate known to STL relates them: no difference is invented.
func TestConversionRoundsOnceAndComparesWithTheAnalysis(t *testing.T) {
	c := money(t, cny, "0.11")
	got, conv, err := convert(c, usd, settlementFX(t, analysedInCNY))
	if err != nil || got.String() != "0.02 USD" || conv.Analysis.Difference.String() != "-0.03 CNY" {
		// 0.11 × 0.145454545455 = 0.016 → 0.02 USD; 0.02 × 7 = 0.14 CNY; 0.11 − 0.14 = −0.03.
		t.Fatalf("%v %+v %v", got, conv, err)
	}
	inUSD := &AnalysisBasis{OrderCurrency: "USD", Currency: usd, Rate: kernel.DecimalFromInt(1)}
	got, conv, err = convert(money(t, cny, "2750.00"), usd, settlementFX(t, inUSD))
	if err != nil || got.String() != "400.00 USD" || conv.Analysis.Difference.String() != "0.00 USD" {
		t.Fatalf("%v %+v %v", got, conv, err)
	}
	eur := kernel.MustCurrency("EUR", 2)
	third := &AnalysisBasis{OrderCurrency: "USD", Currency: eur, Rate: kernel.MustDecimal("0.9")}
	_, conv, err = convert(money(t, cny, "2750.00"), usd, settlementFX(t, third))
	if err != nil || conv.Analysis == nil || conv.Analysis.Difference != nil {
		t.Fatalf("%+v %v", conv, err)
	}
	if _, _, err = convert(money(t, cny, "0.01"), usd, settlementFX(t, nil)); err == nil {
		t.Fatal("CNY 0.01 converts to USD 0.00")
	}
}

// D-4 with a cross-currency prepayment: CNY 2,094.75 prepays BRC-001's P1 of 288.00 at the settlement rate
// (2,094.75 × 0.145454545455 = 304.6909…, the test takes 1,980.00 = 288.00); a first statement settling only
// 200.00 on P1 moves 200.00 and leaves 88.00: the conversion moves in proportion, HALF_UP on the moved part,
// the rest to the remainder, so source and difference add up to the original exactly.
func TestACrossCurrencyPrepaymentMovesInProportion(t *testing.T) {
	a := brcAccount(t)
	f := receipt(t, "fund.cny", "customer.1", cny, "5000.00", "5000.00")
	x, err := Apply(ApplyInput{ID: "app.p1", Fund: f, FundEntries: fundrecord.NoEntries(cny), Account: a, StageID: "P1",
		StageApplied: money(t, usd, "0"), Amount: money(t, cny, "1980.00"), FX: settlementFX(t, analysedInCNY), By: treasury})
	if err != nil || x.Amount.String() != "288.00 USD" || x.Conversion.Analysis.Difference.String() != "-36.00 CNY" {
		// 1,980 × 0.145454545455 = 288.0000000009 → 288.00; 288 × 7 = 2,016.00; 1,980 − 2,016 = −36.00.
		t.Fatalf("%+v %v", x, err)
	}
	st := statementOf(t, a, "20000")
	moves := MoveToReceivable(st, []Application{*x}, ids(), treasury, "")
	if len(moves) != 1 || moves[0].Remainder == nil {
		t.Fatalf("%+v", moves)
	}
	r, rest := moves[0].Replacement, moves[0].Remainder
	if r.Amount.String() != "200.00 USD" || r.Conversion.Source.String() != "1375.00 CNY" || r.Conversion.Analysis.Difference.String() != "-25.00 CNY" ||
		rest.Amount.String() != "88.00 USD" || rest.Conversion.Source.String() != "605.00 CNY" || rest.Conversion.Analysis.Difference.String() != "-11.00 CNY" ||
		r.Conversion.Rate != x.Conversion.Rate || rest.Taken().String() != "605.00 CNY" {
		t.Fatalf("replacement %+v %+v\nremainder %+v %+v", r, r.Conversion, rest, rest.Conversion)
	}
}

// S-B (baseline 4.7) on a receivable of USD 1,000.00: a confirmed credit note of 100.00 is applied to it —
// the receivable is open 900.00, the stage keeps it — and never more than the note's rest
// (APPLICATION_EXCEEDS_AVAILABLE) nor what is open (APPLICATION_EXCEEDS_OPEN); a requested note is not a
// source, and a note credits its own statement only (STL-INV-008).
func TestAConfirmedCreditNoteIsASourceForItsReceivable(t *testing.T) {
	a, err := orderaccount.Open("order.1", 1, "h", "customer.1", 1, []orderaccount.Stage{
		{StageID: "P1", Trigger: orderaccount.TriggerAfterShipment, Days: 30, Percent: "100", Amount: money(t, usd, "1000.00")}}, treasury, "")
	if err != nil {
		t.Fatal(err)
	}
	st := statementOf(t, a, "100000")
	note := &settlement.CreditNote{ID: "cn.1", StatementID: st.ID, OrderID: "order.1", Kind: settlement.CreditPriceAllowance,
		Status: settlement.CreditConfirmed, Version: 2, Amount: money(t, usd, "100.00")}
	in := CreditInput{ID: "app.cn", Note: note, NoteApplied: money(t, usd, "0"), Statement: st, TargetKind: TargetReceivable,
		Applied: map[int]kernel.Money{}, Amount: money(t, usd, "100.00"), By: treasury}
	x, err := ApplyCredit(in)
	if err != nil || x.CreditNoteID != "cn.1" || x.FundRecordID != "" || x.Amount.String() != "100.00 USD" || x.StageID != "P1" ||
		x.InstallmentSeq != 1 || x.TargetKind != TargetReceivable || x.Conversion != nil {
		t.Fatalf("%+v %v", x, err)
	}
	if open := st.Open(map[int]kernel.Money{1: x.Amount}); open.String() != "900.00 USD" {
		t.Fatalf("open %s", open)
	}
	in.NoteApplied = money(t, usd, "60.00")
	_, err = ApplyCredit(in)
	want(t, err, fundrecord.CodeApplicationExceedsAvailable)
	in.NoteApplied, in.Applied = money(t, usd, "0"), map[int]kernel.Money{1: money(t, usd, "950.00")}
	_, err = ApplyCredit(in)
	want(t, err, fundrecord.CodeApplicationExceedsOpen)
	in.Applied = map[int]kernel.Money{}
	note.Status = settlement.CreditRequested
	_, err = ApplyCredit(in)
	want(t, err, kernel.CodeInvalidState)
	note.Status, note.StatementID = settlement.CreditConfirmed, "statement.other"
	_, err = ApplyCredit(in)
	want(t, err, kernel.CodeReferenceInvalid)
}
