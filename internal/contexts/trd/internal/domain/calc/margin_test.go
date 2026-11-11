package calc

import (
	"math/big"
	"math/rand/v2"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// TRD-INV-007 and T03 §5.3: R − C ≥ R × minimum_margin compared exactly.
func TestCheckMargin(t *testing.T) {
	for _, c := range []struct {
		name, source  string
		revenue, cost int64
		minimum       string
		verdict       string
		profit        string
		rate          string
		markup        string // "" for undefined
		shortfall     string
	}{
		{"960 against 660", "T03 §5.3 (31.25%, markup 45.4545…%); test_freeze_ignores_client_calculation_and_persists_server_result (PYT-0442)",
			96000, 66000, "0.1", MarginPass, "300.00", "0.3125", "5/11", "0"},
		{"840 against 660 below 0.25", "T03 §5.3 (3/14 ≈ 21.428571%); BC-AT06 (180/840)",
			84000, 66000, "0.25", MarginRequiresApproval, "180.00", "3/14", "3/11", "30"},
		{"880 against 660 is exactly 0.25", "TRD-INV-007; T03 §5.3",
			88000, 66000, "0.25", MarginPass, "220.00", "0.25", "1/3", "0"},
		{"879.99 against 660 is below 0.25 whatever a screen shows", "TRD-INV-007; T03 §5.3",
			87999, 66000, "0.25", MarginRequiresApproval, "219.99", "7333/29333", "7333/22000", "0.0075"},
		{"879.9999996 rounds to 880.00 and passes", "t03_candidate/verify.py margin threshold at price 7.33333333",
			88000, 66000, "0.25", MarginPass, "220.00", "0.25", "1/3", "0"},
		{"110 USD against 66 USD", "T03 §5.3 (EUR 100 at 1.1: profit 44, margin 40%); T03-C08",
			11000, 6600, "0.25", MarginPass, "44.00", "0.4", "2/3", "0"},
		{"a loss", "T03 §5.2: R − C may be negative",
			50000, 66000, "0", MarginRequiresApproval, "-160.00", "-0.32", "-8/33", "160"},
		{"no cost: the markup is undefined, never 0", "T03 §5.2",
			10000, 0, "0.25", MarginPass, "100.00", "1", "", "0"},
		{"a minimum of 0 passes any profit of 0 or more", "T03 §5.2",
			66000, 66000, "0", MarginPass, "0.00", "0", "0", "0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, err := CheckMargin("/args/data", minor(t, usd, c.revenue), minor(t, usd, c.cost), dec(c.minimum))
			ok(t, err)
			if m.Verdict != c.verdict || m.Profit.Amount() != c.profit || m.Rate.String() != c.rate || !m.Shortfall.Equal(dec(c.shortfall)) {
				t.Fatalf("got %s, profit %s, rate %s, shortfall %s", m.Verdict, m.Profit.Amount(), m.Rate, m.Shortfall)
			}
			if (m.Markup == nil) != (c.markup == "") || (m.Markup != nil && m.Markup.String() != c.markup) {
				t.Fatalf("markup %v, want %q", m.Markup, c.markup)
			}
			err = m.Check("/args/quote_id")
			if c.verdict == MarginPass {
				ok(t, err)
			} else {
				rejected(t, err, CodeMarginPolicyRequiresApproval, "/args/quote_id")
			}
		})
	}
}

// TRD-INV-007: illegal amounts and scope errors are refusals, not a verdict an exception could
// override.
func TestCheckMarginRefusals(t *testing.T) {
	for _, c := range []struct {
		name          string
		revenue, cost kernel.Money
		minimum       string
		code, path    string
	}{
		{"no net sales", minor(t, usd, 0), minor(t, usd, 66000), "0.1", "INVALID_KNOWN_VALUE", "/args/data/lines"},
		{"a negative cost", minor(t, usd, 96000), minor(t, usd, -1), "0.1", "INVALID_KNOWN_VALUE", "/args/data/cost_snapshot_ref"},
		{"two currencies", minor(t, eur, 96000), minor(t, usd, 66000), "0.1", CodeComparisonBasisMismatch, "/args/data/pricing_policy_ref"},
		{"a minimum above 1", minor(t, usd, 96000), minor(t, usd, 66000), "1.5", "INVALID_KNOWN_VALUE", "/args/data/pricing_policy_ref"},
		{"a negative minimum", minor(t, usd, 96000), minor(t, usd, 66000), "-0.1", "INVALID_KNOWN_VALUE", "/args/data/pricing_policy_ref"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := CheckMargin("/args/data", c.revenue, c.cost, dec(c.minimum))
			rejected(t, err, c.code, c.path)
		})
	}
}

// The verdict against an independent integer comparison: with R and C in minor units and the
// minimum scaled by 10^12 to M, the quote passes iff (R − C)·10^12 ≥ R·M; the shortfall is
// (R·M − (R − C)·10^12) ÷ 10^(12+2) USD.
func TestCheckMarginMatchesIndependentIntegerArithmetic(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261111, 7))
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(12), nil)
	for i := 0; i < 5000; i++ {
		r := 1 + rng.Int64N(10_000_000)
		c := rng.Int64N(12_000_000)
		mm := rng.Int64N(1_000_000_000_001) // 0–1 with 12 digits
		m, err := CheckMargin("/args/data", minor(t, usd, r), minor(t, usd, c), kernel.DecimalFromMinor(big.NewInt(mm), 12))
		ok(t, err)
		left := new(big.Int).Mul(big.NewInt(r-c), scale)
		right := new(big.Int).Mul(big.NewInt(r), big.NewInt(mm))
		pass := left.Cmp(right) >= 0
		if m.Passes() != pass {
			t.Fatalf("R=%d C=%d M=%d: got %s", r, c, mm, m.Verdict)
		}
		want := kernel.Decimal{}
		if !pass {
			want = kernel.DecimalFromMinor(new(big.Int).Sub(right, left), 14)
		}
		if !m.Shortfall.Equal(want) {
			t.Fatalf("R=%d C=%d M=%d: shortfall %s, want %s", r, c, mm, m.Shortfall, want)
		}
	}
}
