package rules

import (
	"math/big"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// clock is the Python costing fixture's clock, 2026-10-22T09:00:00Z; its pricing policy allows
// exchange rates observed up to 3600 s before.
var clock = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

func validity(t *testing.T, from, until string) kernel.ValidityInterval {
	t.Helper()
	f, err := kernel.ParseTimestamp(from)
	ok(t, err)
	u, err := kernel.ParseTimestamp(until)
	ok(t, err)
	v, err := kernel.NewValidityInterval(f, u)
	ok(t, err)
	return v
}

// pythonRate is Python test_costing_domains.py fx(): EUR→USD 1.1 for cost_estimate, observed
// 08:50, valid 2026-10-22T08:50 – 2026-10-29.
func pythonRate(t *testing.T) FXRate {
	return FXRate{Path: "/fx/domain.fx", Ref: "fx_rate:domain.fx@1", From: eur, To: usd, Rate: dec("1.1"),
		Purpose: PurposeCostEstimate, ObservedAt: time.Date(2026, 10, 22, 8, 50, 0, 0, time.UTC),
		Validity: validity(t, "2026-10-22T08:50:00Z", "2026-10-29T00:00:00Z")}
}

func toUSD(rates ...FXRate) FX {
	return FX{Path: "/fx_refs", Target: usd, Rates: rates, At: clock, MaxAge: time.Hour}
}

// CST-INV-006 and T02 §11 跨币种 (tactical test T02-S07): 100 USD at a cost_estimate rate of
// 7.123456 CNY/USD is 712.35 CNY, rounded once.
func TestFXCrossCurrency(t *testing.T) {
	r := FXRate{Path: "/fx/usd.cny", Ref: "fx_rate:usd.cny@1", From: usd, To: cny, Rate: dec("7.123456"), Purpose: PurposeCostEstimate,
		ObservedAt: clock.Add(-time.Minute), Validity: validity(t, "2026-10-22T00:00:00Z", "2026-10-23T00:00:00Z")}
	got, ref, err := FX{Path: "/fx_refs", Target: cny, Rates: []FXRate{r}, At: clock, MaxAge: time.Hour}.Convert(money(t, usd, 10000))
	ok(t, err)
	if got.String() != "712.35 CNY" || ref != "fx_rate:usd.cny@1" {
		t.Fatalf("got %s with %q", got, ref)
	}
	// Independent arithmetic: 10000 cents × 7123456 / 10^6 = 71234.56 → HALF_UP 71235.
	want := new(big.Int).Mul(big.NewInt(10000), big.NewInt(7123456))
	want.Add(want, big.NewInt(500000)).Quo(want, big.NewInt(1000000))
	if got.Minor().Cmp(want) != 0 {
		t.Fatalf("got %s, oracle %s", got.Minor(), want)
	}
	// The target's minor units decide the rounding point: 100 USD at 151.235 JPY/USD is 15123.5,
	// HALF_UP 15124 JPY (no minor units).
	r = FXRate{Path: "/fx/usd.jpy", From: usd, To: jpy, Rate: dec("151.235"), Purpose: PurposeCostEstimate,
		ObservedAt: clock, Validity: validity(t, "2026-10-22T00:00:00Z", "2026-10-23T00:00:00Z")}
	got, _, err = FX{Path: "/fx_refs", Target: jpy, Rates: []FXRate{r}, At: clock, MaxAge: time.Hour}.Convert(money(t, usd, 10000))
	ok(t, err)
	if got.String() != "15124 JPY" {
		t.Fatalf("got %s, want 15124 JPY", got)
	}
}

// CST-INV-006 positive: the same currency is an explicit 1 and needs no rate.
func TestFXSameCurrency(t *testing.T) {
	m := money(t, usd, 66000)
	got, ref, err := toUSD().Convert(m)
	ok(t, err)
	if !got.Equal(m) || ref != "" {
		t.Fatalf("got %s %q", got, ref)
	}
	_, _, err = toUSD().Convert(money(t, kernel.MustCurrency("USD", 3), 1))
	rejected(t, err, kernel.CodeReferenceInvalid, "/fx_refs")
}

// Python test_costing_domains.py FX vectors on 600 EUR: missing, reversed, quotation-purpose and
// over-age rates are FX_NOT_APPLICABLE; test_explicit_fx_produces_independent_known_cost converts
// 600 EUR at 1.1 into 660.00 USD (72000 cents with the 60 USD freight).
func TestPythonFXVectors(t *testing.T) {
	eur600 := money(t, eur, 60000)
	got, ref, err := toUSD(pythonRate(t)).Convert(eur600)
	ok(t, err)
	if got.String() != "660.00 USD" || ref != "fx_rate:domain.fx@1" {
		t.Fatalf("got %s %q", got, ref)
	}

	reverse := pythonRate(t)
	reverse.From, reverse.To = usd, eur
	quotation := pythonRate(t)
	quotation.Purpose = PurposeQuotation
	old := pythonRate(t)
	old.ObservedAt = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) // 9 h before, policy 1 h
	expired := pythonRate(t)
	expired.Validity = validity(t, "2026-10-21T00:00:00Z", "2026-10-22T08:59:59Z")
	future := pythonRate(t)
	future.ObservedAt = clock.Add(time.Second)
	tooHigh := pythonRate(t)
	tooHigh.Rate = dec("1000.000000000001")
	for _, c := range []struct {
		name  string
		rates []FXRate
		path  string
	}{
		{"missing is not 1", nil, "/fx_refs"},                              // test_missing_cross_currency_fx_is_not_assumed_one
		{"reverse is not inverted", []FXRate{reverse}, "/fx_refs"},         // test_reverse_fx_is_not_inverted_automatically
		{"quotation does not price cost", []FXRate{quotation}, "/fx_refs"}, // test_quotation_fx_cannot_price_cost
		{"older than the policy", []FXRate{old}, "/fx/domain.fx"},          // test_fx_older_than_policy_limit_is_rejected
		{"not in force", []FXRate{expired}, "/fx/domain.fx"},               // T02 §11: 过期不放行
		{"observed in the future", []FXRate{future}, "/fx/domain.fx"},      // age < 0
		{"two rates for one direction", []FXRate{pythonRate(t), pythonRate(t)}, "/fx_refs"},
		{"rate above 1000", []FXRate{tooHigh}, "/fx/domain.fx"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := toUSD(c.rates...).Convert(eur600)
			rejected(t, err, CodeFXNotApplicable, c.path)
		})
	}
	noPolicy := toUSD(pythonRate(t))
	noPolicy.MaxAge = 0
	_, _, err = noPolicy.Convert(eur600)
	rejected(t, err, CodeCostInputNotReady, "/fx_refs")
}

// T02 §13.3 fixed example: 113 CNY inclusive of 0.13, recoverable, is 100.00 CNY net and 14.00 USD
// at an explicit 0.14 USD/CNY; nonrecoverable 113 × 0.14 = 15.82 USD.
func TestCostTaxThenFX(t *testing.T) {
	r := FXRate{Path: "/fx/cny.usd", Ref: "fx_rate:cny.usd@1", From: cny, To: usd, Rate: dec("0.14"), Purpose: PurposeCostEstimate,
		ObservedAt: clock, Validity: validity(t, "2026-10-22T00:00:00Z", "2026-10-23T00:00:00Z")}
	for recovery, want := range map[Recovery]string{Recoverable: "14.00 USD", Nonrecoverable: "15.82 USD"} {
		got, err := Cost("/lines/0", dec("113"), cny, treatment("0.13", TaxInclusive, recovery, nil), toUSD(r))
		ok(t, err)
		if got.Amount.String() != want || got.FXRate != "fx_rate:cny.usd@1" {
			t.Errorf("%s: %s, want %s", recovery, got.Amount, want)
		}
	}
	// S2-08 through conversion: 104.00 CNY × 0.14 = 14.56 USD.
	got, err := Cost("/lines/0", dec("113"), cny, treatment("0.13", TaxInclusive, PartiallyRecoverable, decp("0.09")), toUSD(r))
	ok(t, err)
	if got.Amount.String() != "14.56 USD" || got.Tax.Cost.String() != "104.00 CNY" {
		t.Errorf("partial: %+v", got)
	}
}

// T02 §13.3 fixed counter-example: two independent lines of 0.005 USD are 0.01 each (0.02 in all);
// two tier bands of 0.005 within one line are summed first and round to 0.01 once.
func TestRoundingPerComponent(t *testing.T) {
	zero := treatment("0", TaxExclusive, Nonrecoverable, nil)
	var amounts []kernel.Money
	for range 2 {
		c, err := Cost("/lines/0", dec("0.005"), usd, zero, toUSD())
		ok(t, err)
		amounts = append(amounts, c.Amount)
	}
	total, err := Total(usd, "/total", amounts...)
	ok(t, err)
	if total.Amount() != "0.02" {
		t.Fatalf("two lines: %s, want 0.02", total.Amount())
	}
	s := Schedule{Path: "/lines/0", Mode: ModeGraduated, BasisQuantity: dec("1000"), Tiers: []Tier{
		{Lower: dec("0"), Upper: dec("1"), Bounded: true, UnitPrice: dec("5")},
		{Lower: dec("1"), UnitPrice: dec("5")},
	}}
	raw, err := s.Amount(dec("2"), "/quantity") // 1/1000 × 5 + 1/1000 × 5 = 0.01
	ok(t, err)
	one, err := Cost("/lines/0", raw, usd, zero, toUSD())
	ok(t, err)
	if raw.String() != "0.01" || one.Amount.Amount() != "0.01" {
		t.Fatalf("one line, two bands: %s → %s, want 0.01", raw, one.Amount.Amount())
	}
}

// T02 §13.9: unit price 10^12 × quantity 10^6 ÷ basis 10^6 is a legal 10^12 component although the
// product 10^18 passes through; a total of 10^12 + 0.01 is CALCULATION_LIMIT_EXCEEDED.
func TestBusinessBounds(t *testing.T) {
	s := flat("/lines/0", "1")
	s.Tiers[0].UnitPrice = kernel.DecimalFromInt(1_000_000_000_000)
	s.BasisQuantity = kernel.DecimalFromInt(1_000_000)
	raw, err := s.Amount(kernel.DecimalFromInt(1_000_000), "/quantity")
	ok(t, err)
	c, err := Cost("/lines/0", raw, usd, treatment("0", TaxExclusive, Nonrecoverable, nil), toUSD())
	ok(t, err)
	if c.Amount.Amount() != "1000000000000.00" {
		t.Fatalf("got %s", c.Amount.Amount())
	}
	_, err = Total(usd, "/total", c.Amount, money(t, usd, 1))
	rejected(t, err, CodeCalculationLimitExceeded, "/total")
	_, err = Cost("/lines/1", raw.Add(dec("0.01")), usd, treatment("0", TaxExclusive, Nonrecoverable, nil), toUSD())
	rejected(t, err, CodeCalculationLimitExceeded, "/lines/1")
	_, err = Total(usd, "/total", money(t, cny, 1))
	rejected(t, err, kernel.CodeInvalidKnownValue, "/total/components/0")
}
