package kernel

import (
	"errors"
	"testing"
)

// The vectors in this file come from the Python V0.50 reference implementation and the business
// core design; they are the independent arithmetic cross-check that ADR-BR-REPO-10 asks for.

// BRC-001: four lines of 24 + 36 + 12 + 48 = 120 PCS at 8.00 USD; cost 120 × 5.00 + 60.00;
// payment 30 % / 70 %; purchase draft 120 × 5.00.
func TestBRC001Arithmetic(t *testing.T) {
	price := MustDecimal("8.00")
	var lines []Money
	quantity := Quantity{}
	for i, q := range []string{"24", "36", "12", "48"} {
		qty, err := ParseQuantity(q, "PCS")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			quantity = qty
		} else if quantity, err = quantity.Add(qty); err != nil {
			t.Fatal(err)
		}
		line, err := RoundMoney(usd, qty.Value().Mul(price))
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	revenue, err := Sum(usd, lines...)
	if err != nil {
		t.Fatal(err)
	}
	if quantity.String() != "120 PCS" || revenue.Amount() != "960.00" {
		t.Fatalf("quantity %s, revenue %s; want 120 PCS and 960.00", quantity, revenue.Amount())
	}

	unitCost := money(t, usd, "5.00")
	goods, err := unitCost.MulHalfUp(quantity.Value())
	if err != nil {
		t.Fatal(err)
	}
	cost, _ := goods.Add(money(t, usd, "60.00"))
	margin, _ := revenue.Sub(cost)
	rate, _ := margin.Decimal().Quo(revenue.Decimal())
	if cost.Amount() != "660.00" || margin.Amount() != "300.00" || rate.String() != "0.3125" || rate.Mul(DecimalFromInt(100)).String() != "31.25" {
		t.Fatalf("cost %s, margin %s, rate %s; want 660.00, 300.00, 0.3125", cost.Amount(), margin.Amount(), rate)
	}

	payments, err := revenue.Allocate(shares("P1", "30", "P2", "70"))
	if err != nil {
		t.Fatal(err)
	}
	if payments[0].Amount() != "288.00" || payments[1].Amount() != "672.00" {
		t.Fatalf("payments %s + %s, want 288.00 + 672.00", payments[0].Amount(), payments[1].Amount())
	}
	if goods.Amount() != "600.00" {
		t.Fatalf("purchase draft %s, want 600.00", goods.Amount())
	}
}

// T02 §11 and foundation.tax.preview: 100.00 net at 13 % is 13.00 tax, 113.00 gross; split back
// from 113.00 inclusive it is 100.00 + 13.00.
func TestTaxSplits(t *testing.T) {
	rate := MustDecimal("0.13")
	ex, err := TaxExclusive(cny, MustDecimal("100"), rate)
	if err != nil || ex.Net.Amount() != "100.00" || ex.Tax.Amount() != "13.00" || ex.Gross.Amount() != "113.00" {
		t.Fatalf("exclusive = %+v, %v", ex, err)
	}
	in, err := TaxInclusive(cny, MustDecimal("113"), rate)
	if err != nil || in.Net.Amount() != "100.00" || in.Tax.Amount() != "13.00" || in.Gross.Amount() != "113.00" {
		t.Fatalf("inclusive = %+v, %v", in, err)
	}
	// Python T03 test_inclusive_tax_is_split_from_line_payable_before_margin: 960.00 inclusive of
	// 13 % is 849.56 net (84955.75… rounded) and 110.44 tax.
	line, err := TaxInclusive(usd, MustDecimal("960"), rate)
	if err != nil || line.Net.MinorString() != "84956" || line.Tax.MinorString() != "11044" || line.Gross.MinorString() != "96000" {
		t.Fatalf("960.00 inclusive = %+v, %v", line, err)
	}
	for _, bad := range []string{"-0.01", "1.01"} {
		if _, err := TaxExclusive(cny, MustDecimal("1"), MustDecimal(bad)); !errors.Is(err, ErrInvalidTaxRate) {
			t.Errorf("rate %s: %v", bad, err)
		}
	}
	if _, err := TaxInclusive(cny, MustDecimal("-1"), rate); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("negative base: %v", err)
	}
}

// T02 §11: 100 USD at 7.123456 CNY/USD for cost_estimate is 712.35 CNY, rounded once.
func TestExchangeRateConversion(t *testing.T) {
	x, err := ParseExchangeRate(usd, cny, "7.123456")
	if err != nil {
		t.Fatal(err)
	}
	got, err := x.Convert(money(t, usd, "100"))
	if err != nil || got.String() != "712.35 CNY" {
		t.Fatalf("100 USD = %v, %v; want 712.35 CNY", got, err)
	}
	if _, err = x.Convert(money(t, cny, "1")); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("converting CNY with a USD rate: %v", err)
	}
	for _, tc := range []struct {
		from, to Currency
		rate     string
	}{{usd, cny, "0"}, {usd, usd, "1.1"}, {usd, cny, "7.1234567890123"}, {usd, cny, "-7"}} {
		if _, err := ParseExchangeRate(tc.from, tc.to, tc.rate); !errors.Is(err, ErrInvalidExchangeRate) {
			t.Errorf("rate %s %s/%s: %v", tc.rate, tc.to, tc.from, err)
		}
	}
	if _, err := ParseExchangeRate(usd, usd, "1"); err != nil {
		t.Errorf("identity rate: %v", err)
	}
}

// Python T04 A46 tiers: [0, 100) at 6, [100, ∞) at 5, quantity 120. Graduated prices each band
// (100 × 6 + 20 × 5 = 700.00); all-units prices everything at the tier reached (120 × 5 =
// 600.00); the cost variance is 100.00.
func TestTierArithmetic(t *testing.T) {
	q := MustDecimal("120")
	band := MustDecimal("100")
	graduated, _ := RoundMoney(usd, band.Mul(MustDecimal("6")).Add(q.Sub(band).Mul(MustDecimal("5"))))
	allUnits, _ := RoundMoney(usd, q.Mul(MustDecimal("5")))
	variance, _ := graduated.Sub(allUnits)
	if graduated.MinorString() != "70000" || allUnits.MinorString() != "60000" || variance.MinorString() != "10000" {
		t.Fatalf("graduated %s, all units %s, variance %s", graduated.MinorString(), allUnits.MinorString(), variance.MinorString())
	}
}

// Python T03 margin rule: profit ≥ revenue × minimum, compared exactly. With cost 660.00 and a
// 25 % minimum, 880.00 passes exactly at the boundary and 879.99 needs approval.
func TestMarginFloorIsComparedExactly(t *testing.T) {
	minimum, err := ParseRatio("0.25")
	if err != nil {
		t.Fatal(err)
	}
	cost := money(t, usd, "660.00")
	for revenue, pass := range map[string]bool{"880.00": true, "879.99": false, "1000.00": true} {
		r := money(t, usd, revenue)
		profit, _ := r.Sub(cost)
		if got := minimum.MeetsShare(profit.Decimal(), r.Decimal()); got != pass {
			t.Errorf("revenue %s: pass = %v, want %v", revenue, got, pass)
		}
	}
}

// Python costing domains: 10 DOZEN with a published factor of 12 PCS per DOZEN is 120 PCS.
func TestUnitConversionUsesThePublishedFactor(t *testing.T) {
	dozens, err := ParseQuantity("10", "DOZEN")
	if err != nil {
		t.Fatal(err)
	}
	pcs, err := dozens.Convert("PCS", MustDecimal("12"))
	if err != nil || pcs.String() != "120 PCS" {
		t.Fatalf("10 DOZEN = %v, %v", pcs, err)
	}
	if _, err = dozens.Convert("PCS", MustDecimal("0")); !errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("zero factor: %v", err)
	}
	if _, err = dozens.Add(pcs); !errors.Is(err, ErrUnitMismatch) {
		t.Fatalf("DOZEN + PCS: %v", err)
	}
	eleven, _ := ParseQuantity("11", "DOZEN")
	if _, err = dozens.Sub(eleven); !errors.Is(err, ErrInvalidQuantity) {
		t.Fatalf("negative difference: %v", err)
	}
}
