package calc

import (
	"math/big"
	"math/rand/v2"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func priced(quantity, price, basis, discount string, tax TaxProfile) Line {
	l := line()
	l.Quantity, l.UnitPrice, l.PriceBasisQuantity, l.Discount, l.Tax = dec(quantity), dec(price), dec(basis), dec(discount), goodsTax(tax)
	return l
}

// T03 §5.1 and §5.3, TRD-INV-008: each line is rounded to the currency's minor units on its own;
// the tax split conserves the payable.
func TestLineAmounts(t *testing.T) {
	inc13 := profile("sales.tax.inc", "0.13", TaxInclusive)
	exc13 := profile("sales.tax.13", "0.13", TaxExclusive)
	for _, c := range []struct {
		name              string
		source            string
		line              Line
		currency          kernel.Currency
		net, tax, payable string
	}{
		{"120 × 8 USD without tax", "T03 §5.3; test_freeze_ignores_client_calculation_and_persists_server_result (PYT-0442)",
			priced("120", "8", "1", "0", zeroTax), usd, "96000", "0", "96000"},
		{"1 × 0.005 USD rounds to one cent", "T03 §5.3; test_multiple_lines_round_before_aggregation_and_reject_duplicate_ids (PYT-0450)",
			priced("1", "0.005", "1", "0", zeroTax), usd, "1", "0", "1"},
		{"inclusive 0.13 on a payable of 960.00", "test_inclusive_tax_is_split_from_line_payable_before_margin (PYT-0451); test_inclusive_sales_tax_preserves_payable_and_margin_base (PYT-0320)",
			priced("120", "8", "1", "0", inc13), usd, "84956", "11044", "96000"},
		{"exclusive 0.13 on a net of 960.00", "test_exclusive_sales_tax_uses_dedicated_reviewed_application (PYT-0319)",
			priced("120", "8", "1", "0", exc13), usd, "96000", "12480", "108480"},
		{"inclusive payable 113 at 0.13", "T03 §5.3",
			priced("1", "113", "1", "0", inc13), usd, "10000", "1300", "11300"},
		{"a price per 100 with a discount", "T03 §5.1: B = quantity × unit_price ÷ price_basis_quantity × (1 − discount_ratio)",
			priced("120", "800", "100", "0.25", zeroTax), usd, "72000", "0", "72000"},
		{"trailing zeros are the same value", "t03_candidate/examples/commercial.json (8.0000, 1.0, 0.00)",
			priced("120.000000", "8.0000", "1.0", "0.00", zeroTax), usd, "96000", "0", "96000"},
		{"a currency without minor units", "kernel HALF_UP at 0 digits",
			priced("3", "0.5", "1", "0", zeroTax), jpy, "2", "0", "2"},
		{"a large discounted line against independent Fraction truth", "test_large_discounted_line_rounding_matches_independent_fraction_truth (PYT-0326)",
			priced("120", "112827580121.48491183", "0.00000001", "0.01586449", zeroTax), usd,
			"133245153725908098793346", "0", "133245153725908098793346"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, err := PriceLine(c.currency, c.line)
			ok(t, err)
			if a.Net.MinorString() != c.net || a.Tax.MinorString() != c.tax || a.Payable.MinorString() != c.payable || a.ID != "L1" {
				t.Fatalf("got %s + %s = %s, want %s + %s = %s", a.Net.MinorString(), a.Tax.MinorString(), a.Payable.MinorString(), c.net, c.tax, c.payable)
			}
		})
	}
}

func TestLineRefusals(t *testing.T) {
	for _, c := range []struct {
		name, source string
		edit         func(*Line)
		code, path   string
	}{
		{"zero quantity", "t03_candidate/verify.py zero quantity", func(l *Line) { l.Quantity = dec("0") }, "INVALID_KNOWN_VALUE", "/args/data/lines/0/quantity"},
		{"a discount of 1", "t03_candidate/verify.py invalid discount", func(l *Line) { l.Discount = dec("1") }, "INVALID_KNOWN_VALUE", "/args/data/lines/0/discount_ratio"},
		{"a negative discount", "T03 §5.1 discount 0–1", func(l *Line) { l.Discount = dec("-0.1") }, "INVALID_KNOWN_VALUE", "/args/data/lines/0/discount_ratio"},
		{"zero price basis", "t03_candidate/verify.py zero price basis", func(l *Line) { l.PriceBasisQuantity = dec("0") }, "INVALID_KNOWN_VALUE", "/args/data/lines/0/price_basis_quantity"},
		{"a gift at price 0", "T03 §5.1: no zero-price gifts", func(l *Line) { l.UnitPrice = dec("0") }, "INVALID_KNOWN_VALUE", "/args/data/lines/0/unit_price"},
		{"a price of 9 fractional digits", "T03 §5.1: prices have at most 8", func(l *Line) { l.UnitPrice = dec("8.000000001") }, "SCHEMA_INVALID", "/args/data/lines/0/unit_price"},
		{"a price of 13 integer digits", "T03 §10: at most 12 integer digits", func(l *Line) { l.UnitPrice = kernel.DecimalFromInt(1_000_000_000_000) }, CodeCalculationLimitExceeded, "/args/data/lines/0/unit_price"},
		{"an amount of 10^24 minor units", "test_calculation_rejects_amounts_outside_the_minor_unit_contract (PYT-0327)",
			func(l *Line) { l.UnitPrice, l.PriceBasisQuantity = dec("999999999999.99999999"), dec("0.00000001") }, CodeCalculationLimitExceeded, "/args/data/lines/0"},
		{"no tax application", "T03 §5.1: a missing dependency is never zero tax", func(l *Line) { l.Tax = nil }, CodeTaxPolicyUnconfirmed, "/args/data/lines/0/tax_application_ref"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := line()
			c.edit(&l)
			_, err := PriceLine(usd, l)
			rejected(t, err, c.code, c.path)
		})
	}
}

// Python test_separate_fixed_sales_charge_is_added_once (PYT-0302), test_separate_exclusive_fee_has_its_own_tax_split
// (PYT-0321), test_separate_inclusive_fee_has_its_own_tax_split (PYT-0322) and
// test_included_taxed_fee_does_not_add_a_second_tax_amount (PYT-0323).
func TestChargeAmounts(t *testing.T) {
	taxed := func(c Charge, p TaxProfile) Charge { c.Tax = feeTax(c.Fee.Ref.ID, p); return c }
	inc13 := profile("sales.tax.inc", "0.13", TaxInclusive)
	for _, c := range []struct {
		name              string
		charge            Charge
		net, tax, payable string
		included          bool
	}{
		{"a separate fixed fee of 10", salesCharge("sales.freight", InclusionSeparate, "10"), "1000", "0", "1000", false},
		{"exclusive 0.13 on 10", taxed(salesCharge("sales.freight", InclusionSeparate, "10"), profile("sales.tax.13", "0.13", TaxExclusive)), "1000", "130", "1130", false},
		{"inclusive 0.13 on 11.30", taxed(salesCharge("sales.freight", InclusionSeparate, "11.30"), inc13), "1000", "130", "1130", false},
		{"an included fee adds nothing", taxed(included("sales.freight", "11.30"), inc13), "0", "0", "0", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, err := PriceCharge(usd, c.charge)
			ok(t, err)
			if a.Net.MinorString() != c.net || a.Tax.MinorString() != c.tax || a.Payable.MinorString() != c.payable || a.Included != c.included ||
				a.ID != c.charge.ChargeID || a.ObligationKey != c.charge.ObligationKey || a.Net.Currency() != usd {
				t.Fatalf("got %+v", a)
			}
		})
	}
}

// TRD-INV-008: totals are sums of the rounded amounts, and the net sales are positive.
func TestTotalSumsRoundedAmounts(t *testing.T) {
	half := priced("1", "0.005", "1", "0", zeroTax)
	a, err := PriceLine(usd, half)
	ok(t, err)
	totals, err := Total(usd, "/args/data", []Amounts{a, a}, nil)
	ok(t, err)
	if totals.Net.MinorString() != "2" || totals.Payable.MinorString() != "2" {
		t.Fatalf("two lines of 0.005 USD are 0.02, never 0.01: %+v", totals)
	}

	tiny := priced("1", "0.004", "1", "0", zeroTax)
	a, err = PriceLine(usd, tiny)
	ok(t, err)
	_, err = Total(usd, "/args/data", []Amounts{a}, nil)
	rejected(t, err, "INVALID_KNOWN_VALUE", "/args/data/lines")

	big1 := Amounts{ID: "L1", Net: minor(t, usd, 1), Tax: minor(t, usd, 0), Payable: minor(t, usd, 1)}
	huge, _ := kernel.MoneyFromMinor(usd, new(big.Int).Sub(new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil), big.NewInt(1)))
	_, err = Total(usd, "/args/data", []Amounts{big1, {ID: "L2", Net: huge, Tax: minor(t, usd, 0), Payable: huge}}, nil)
	rejected(t, err, CodeCalculationLimitExceeded, "/args/data")
}

// The rounding of PriceLine against an independent integer implementation of T03 §5.1: with q, u,
// b and d the scaled integers of quantity (6 digits), unit price, price basis quantity and discount
// (8 digits), B × 10^m = q·u·(10^8 − d)·10^m ÷ (10^14·b), and HALF_UP of n ÷ k is (2n + k) div 2k;
// exclusive tax is HALF_UP(N·r ÷ 10^12) and the inclusive net HALF_UP(G·10^12 ÷ (10^12 + r)).
func TestRoundingMatchesIndependentIntegerArithmetic(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261111, 3))
	pow := func(n int64) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil) }
	halfUp := func(n, k *big.Int) *big.Int {
		num := new(big.Int).Add(new(big.Int).Lsh(n, 1), k)
		return num.Quo(num, new(big.Int).Lsh(k, 1))
	}
	scaled := func(v int64, scale int) kernel.Decimal { return kernel.DecimalFromMinor(big.NewInt(v), scale) }
	for i := 0; i < 3000; i++ {
		q := 1 + rng.Int64N(1_000_000_000)   // up to 1000 with 6 digits
		u := 1 + rng.Int64N(100_000_000_000) // up to 1000 with 8 digits
		b := 1 + rng.Int64N(10_000_000_000)  // up to 100 with 8 digits
		d := rng.Int64N(100_000_000)         // [0, 1) with 8 digits
		r := rng.Int64N(300_000_000_000)     // tax rate up to 0.3 with 12 digits
		cur := []kernel.Currency{usd, jpy, kernel.MustCurrency("KWD", 3)}[rng.IntN(3)]
		m := int64(cur.MinorUnits())
		inclusive := rng.IntN(2) == 0
		basis := TaxExclusive
		if inclusive {
			basis = TaxInclusive
		}
		l := line()
		l.Quantity, l.UnitPrice, l.PriceBasisQuantity, l.Discount = scaled(q, 6), scaled(u, 8), scaled(b, 8), scaled(d, 8)
		l.Tax = goodsTax(TaxProfile{Ref: Ref{Kind: "tax_profile", ID: "t", Revision: 1}, Rate: scaled(r, 12), PriceBasis: basis, Recovery: Nonrecoverable})
		got, err := PriceLine(cur, l)
		ok(t, err)

		n := new(big.Int).Mul(big.NewInt(q), big.NewInt(u))
		n.Mul(n, new(big.Int).Sub(pow(8), big.NewInt(d))).Mul(n, pow(m))
		k := new(big.Int).Mul(pow(14), big.NewInt(b))
		rounded := halfUp(n, k)
		var net, tax, payable *big.Int
		if inclusive {
			payable = rounded
			net = halfUp(new(big.Int).Mul(payable, pow(12)), new(big.Int).Add(pow(12), big.NewInt(r)))
			tax = new(big.Int).Sub(payable, net)
		} else {
			net = rounded
			tax = halfUp(new(big.Int).Mul(net, big.NewInt(r)), pow(12))
			payable = new(big.Int).Add(net, tax)
		}
		if got.Net.Minor().Cmp(net) != 0 || got.Tax.Minor().Cmp(tax) != 0 || got.Payable.Minor().Cmp(payable) != 0 {
			t.Fatalf("q=%d u=%d b=%d d=%d r=%d %s inclusive=%v: got %s+%s=%s, want %s+%s=%s", q, u, b, d, r, cur, inclusive,
				got.Net.MinorString(), got.Tax.MinorString(), got.Payable.MinorString(), net, tax, payable)
		}
	}
}

func TestChargeAndSplitRefusals(t *testing.T) {
	noTax := salesCharge("sales.freight", InclusionSeparate, "10")
	noTax.Tax = nil
	_, err := PriceCharge(usd, noTax)
	rejected(t, err, CodeTaxPolicyUnconfirmed, "/args/data/charges/0/tax_application_ref")
	_, err = PriceCharge(usd, salesCharge("sales.freight", InclusionSeparate, "0"))
	rejected(t, err, "INVALID_KNOWN_VALUE", "/args/data/charges/0/amount")
	_, err = PriceCharge(usd, salesCharge("sales.freight", InclusionSeparate, "10.000000001"))
	rejected(t, err, "SCHEMA_INVALID", "/args/data/charges/0/amount")
	_, err = zeroTax.Split(usd, dec("-1"), "/args/data/lines/0/tax_application_ref")
	rejected(t, err, "INVALID_KNOWN_VALUE", "/args/data/lines/0/tax_application_ref")
	_, err = TaxProfile{Rate: dec("0"), PriceBasis: "gross", Recovery: Recoverable}.Split(usd, dec("1"), "/p")
	rejected(t, err, CodeTaxPolicyUnconfirmed, "/p")
}
