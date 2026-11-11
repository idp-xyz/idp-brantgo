package calc

import (
	"math/big"
	"math/rand/v2"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// T03 §5.2 and Python A5/A28: the net sales in minor units, converted once into the comparison
// currency.
func TestCompareConvertsNetSalesOnce(t *testing.T) {
	for _, c := range []struct {
		name, source string
		net          kernel.Money
		fx           *FXRate
		revenue      string
	}{
		{"same currency: as is, no rate", "T03 §5.2", minor(t, usd, 96000), nil, "96000"},
		{"960.00 EUR at 1.1 is 1056.00 USD", "test_sales_fx_maps_eur_quote_to_usd_comparison_currency (PYT-0448)", minor(t, eur, 96000), eurRate(), "105600"},
		{"970.00 EUR at 1.1 is 1067.00 USD", "test_foreign_currency_taxed_fee_composition_keeps_payment_and_margin_units (PYT-0347)", minor(t, eur, 97000), eurRate(), "106700"},
		{"100.00 EUR at 1.1 is 110.00 USD", "T03 §5.3; T03-C08", minor(t, eur, 10000), eurRate(), "11000"},
		{"rounded once, HALF_UP", "T03 §5.2", minor(t, eur, 5), &FXRate{From: eur, To: usd, Rate: dec("1.1"), Purpose: PurposeQuotation, ObservedAt: at}, "6"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Compare("/args/data", c.net, usd, c.fx, at, time.Hour)
			ok(t, err)
			if got.Revenue.MinorString() != c.revenue || got.Revenue.Currency() != usd || got.Currency != usd || (got.FX == nil) != (c.fx == nil) {
				t.Fatalf("got %+v, want %s USD", got, c.revenue)
			}
		})
	}
}

// T03 §5.2, POL-INV-003 and Python A5/A32: a missing rate is never 1, a rate is never inverted, a
// cost rate never prices a sale, and the observation is at most max_fx_age_seconds old.
func TestCompareRefusals(t *testing.T) {
	stale := eurRate()
	stale.ObservedAt = at.Add(-time.Hour - time.Second)
	for _, c := range []struct {
		name, source string
		net          kernel.Money
		fx           func() *FXRate
		at           time.Time
		maxAge       time.Duration
		code, path   string
	}{
		{"a rate named for the same currency (Python: REFERENCE_INVALID)", "test_sales_fx_missing_reference_is_rejected (PYT-0446); POL DecideFX",
			minor(t, usd, 96000), eurRate, at, time.Hour, CodeFXScopeMismatch, "/args/data/sales_fx_ref"},
		{"no rate for another currency (Python: REFERENCE_INVALID)", "_server_calculation sales_fx_ref required; catalog FX_SCOPE_MISMATCH",
			minor(t, eur, 96000), func() *FXRate { return nil }, at, time.Hour, CodeFXScopeMismatch, "/args/data/sales_fx_ref"},
		{"a cost_estimate rate", "T03 §5.2: never the purchase cost_estimate FX",
			minor(t, eur, 96000), func() *FXRate { r := eurRate(); r.Purpose = PurposeCostEstimate; return r }, at, time.Hour, CodeFXScopeMismatch, "/args/data/sales_fx_ref"},
		{"the opposite direction", "T03 §5.2: never inverted",
			minor(t, eur, 96000), func() *FXRate { r := eurRate(); r.From, r.To, r.Rate = usd, eur, dec("0.909090909091"); return r }, at, time.Hour,
			CodeFXScopeMismatch, "/args/data/sales_fx_ref"},
		{"another pair", "T03 §5.2: never cross-derived",
			minor(t, eur, 96000), func() *FXRate { r := eurRate(); r.To = jpy; return r }, at, time.Hour, CodeFXScopeMismatch, "/args/data/sales_fx_ref"},
		{"one second beyond the age", "test_age_boundary_and_stale_formalization_leave_no_business_writes (PYT-0383)",
			minor(t, eur, 96000), eurRate, at.Add(time.Second), time.Hour, CodeFXStale, "/args/data/sales_fx_ref"},
		{"one microsecond beyond the age", "PYT-0383 at microsecond precision",
			minor(t, eur, 96000), eurRate, at.Add(time.Microsecond), time.Hour, CodeFXStale, "/args/data/sales_fx_ref"},
		{"observed after the calculation", "_current_sales_fx age < 0",
			minor(t, eur, 96000), func() *FXRate { r := eurRate(); r.ObservedAt = at.Add(time.Second); return r }, at, time.Hour, CodeFXStale, "/args/data/sales_fx_ref"},
		{"no observation", "_current_sales_fx observed_at",
			minor(t, eur, 96000), func() *FXRate { r := eurRate(); r.ObservedAt = time.Time{}; return r }, at, time.Hour, CodeFXStale, "/args/data/sales_fx_ref"},
		{"a rate of 0", "calculation.py rate > 0",
			minor(t, eur, 96000), func() *FXRate { r := eurRate(); r.Rate = dec("0"); return r }, at, time.Hour, "INVALID_KNOWN_VALUE", "/args/data/sales_fx_ref"},
		{"no maximum age", "POL pricing_policy max_fx_age_seconds ≥ 1",
			minor(t, eur, 96000), eurRate, at, 0, "INVALID_KNOWN_VALUE", "/args/data/pricing_policy_ref"},
		{"currency definitions that disagree", "kernel currency minor units",
			minor(t, eur, 96000), func() *FXRate { r := eurRate(); r.To = kernel.MustCurrency("USD", 3); return r }, at, time.Hour, "REFERENCE_INVALID", "/args/data/sales_fx_ref"},
		{"net sales that convert to 0", "_server_calculation comparison revenue > 0",
			minor(t, eur, 1), func() *FXRate { r := eurRate(); r.Rate = dec("0.1"); return r }, at, time.Hour, "INVALID_KNOWN_VALUE", "/args/data/lines"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Compare("/args/data", c.net, usd, c.fx(), c.at, c.maxAge)
			rejected(t, err, c.code, c.path)
		})
	}
	// Exactly max_fx_age_seconds old is still current (PYT-0383: valid at 09:00, stale at 09:00:01).
	_, err := Compare("/args/data", minor(t, eur, 96000), usd, eurRate(), at, time.Hour)
	ok(t, err)
	_, err = Compare("/args/data", minor(t, eur, 96000), usd, stale, at, time.Hour)
	rejected(t, err, CodeFXStale, "/args/data/sales_fx_ref")
}

// Compare against an independent integer HALF_UP: R = (2·n·x·10^p + k) div 2k with k = 10^(q+12),
// n the net in minor units of a currency of q digits and x the rate scaled by 10^12.
func TestCompareMatchesIndependentIntegerArithmetic(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261111, 32))
	for i := 0; i < 2000; i++ {
		n := 1 + rng.Int64N(1_000_000_000_000)
		x := 1 + rng.Int64N(10_000_000_000_000) // up to 10 with 12 digits
		from := []kernel.Currency{eur, jpy}[rng.IntN(2)]
		to := []kernel.Currency{usd, kernel.MustCurrency("KWD", 3)}[rng.IntN(2)]
		rate := &FXRate{From: from, To: to, Rate: kernel.DecimalFromMinor(big.NewInt(x), 12), Purpose: PurposeQuotation, ObservedAt: at}
		got, err := Compare("/args/data", minor(t, from, n), to, rate, at, time.Hour)
		k := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(from.MinorUnits()+12)), nil)
		num := new(big.Int).Mul(big.NewInt(n), big.NewInt(x))
		num.Mul(num, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(to.MinorUnits())), nil))
		num.Lsh(num, 1).Add(num, k)
		want := num.Quo(num, new(big.Int).Lsh(k, 1))
		if want.Sign() == 0 {
			rejected(t, err, "INVALID_KNOWN_VALUE", "/args/data/lines") // converted to 0
			continue
		}
		ok(t, err)
		if got.Revenue.Minor().Cmp(want) != 0 {
			t.Fatalf("%d %s at %d: got %s, want %s", n, from, x, got.Revenue.MinorString(), want)
		}
	}
}

func TestCompareNeedsTheCalculationInstant(t *testing.T) {
	_, err := Compare("/args/data", minor(t, eur, 96000), usd, eurRate(), time.Time{}, time.Hour)
	rejected(t, err, "INVALID_KNOWN_VALUE", "/args/data/sales_fx_ref")
	_, err = Compare("/args/data", minor(t, kernel.MustCurrency("USD", 3), 96000), usd, nil, at, time.Hour)
	rejected(t, err, "REFERENCE_INVALID", "/args/data/currency_ref")
}
