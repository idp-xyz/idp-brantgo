package calc

import (
	"math/big"
	"math/rand/v2"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The price proposal (S4 unit G4a; owner decision 2026-11-18: a price list where there is one,
// otherwise the formula from cost and target margin). Python V0.50 proposes no price, so the
// vectors are BRC-001's (ES-01: 24 + 36 + 12 + 48 PCS, cost 660.00, 960.00 at 8.00, margin 31.25 %)
// and synthetic ones for the rounding.

func cost(t *testing.T, c kernel.Currency, amount int64, quantity, unit string) LineCost {
	t.Helper()
	return LineCost{Amount: minor(t, c, amount), Quantity: dec(quantity), Unit: unit}
}

// brcProposal is BRC-001 before a price is known: the four RFQ lines and the snapshot's cost of each
// (5.00 a piece and the 60.00 freight by quantity: 5.50 a piece), policy.1's floor of 0.25.
func brcProposal(t *testing.T) ([]PricedLine, ProposalBasis) {
	t.Helper()
	lines := []PricedLine{{"L1", "ST01.red.S", dec("24"), "PCS"}, {"L2", "ST01.red.M", dec("36"), "PCS"},
		{"L3", "ST01.blue.S", dec("12"), "PCS"}, {"L4", "ST01.blue.M", dec("48"), "PCS"}}
	costs := map[string]LineCost{"L1": cost(t, usd, 13200, "24", "PCS"), "L2": cost(t, usd, 19800, "36", "PCS"),
		"L3": cost(t, usd, 6600, "12", "PCS"), "L4": cost(t, usd, 26400, "48", "PCS")}
	return lines, ProposalBasis{Currency: usd, Customer: "customer.1", Cost: &SnapshotCost{Currency: usd, Lines: costs}, Minimum: dec("0.25")}
}

// BRC-001 by the formula: at a target margin of 0.3125 every line is 8.00 a piece on its unit cost
// of 5.50, the four nets are 192.00 + 288.00 + 96.00 + 384.00 = 960.00 against 660.00, and each
// line's margin is exactly the target, above the floor.
func TestFormulaProposesBRC001AtEightDollars(t *testing.T) {
	lines, b := brcProposal(t)
	b.Target = decp("0.3125")
	total := int64(0)
	for i, p := range ProposePrices(lines, b) {
		if p.Source != PriceFromFormula || p.Reason != "" || p.UnitPrice.StringFixed(2) != "8.00" || !p.BasisQuantity.Equal(dec("1")) ||
			p.UnitCost.String() != "5.5" || p.Target.String() != "0.3125" || p.List != nil || p.Line != lines[i] {
			t.Fatalf("%s: %+v", lines[i].LineID, p)
		}
		if p.Margin == nil || p.Margin.Rate.String() != "0.3125" || p.UnderFloor() || !p.Cost.Equal(b.Cost.Lines[lines[i].LineID].Amount) {
			t.Fatalf("%s: margin %+v", lines[i].LineID, p.Margin)
		}
		total += p.Net.Minor().Int64()
	}
	if total != 96000 {
		t.Fatalf("the four lines are %d, want 96000", total)
	}
	// The floor itself: 660.00 at 0.25 is 880.00 (TRD-INV-007's vector), 7.33 a piece after rounding.
	m, err := CheckMargin("", minor(t, usd, 88000), minor(t, usd, 66000), dec("0.25"))
	ok(t, err)
	if !m.Passes() || m.Rate.String() != "0.25" {
		t.Fatalf("%+v", m)
	}
}

// One HALF_UP rounding to the quote currency's minor units, after exact arithmetic.
func TestFormulaPriceRoundsOnceHalfUpToTheCurrencysMinorUnits(t *testing.T) {
	for _, c := range []struct {
		name     string
		currency kernel.Currency
		cost     int64 // minor units of the line
		quantity string
		target   string
		unitCost string // exact, a fraction when it does not terminate
		price    string // with the currency's digits
	}{
		{"5.50 at 0.3125 is 8.00 exactly", usd, 13200, "24", "0.3125", "5.5", "8.00"},
		{"5.50 at 0.25 is 7.3333… and rounds down", usd, 13200, "24", "0.25", "5.5", "7.33"},
		{"5.50 at 0.30 is 7.857142… and rounds up", usd, 13200, "24", "0.3", "5.5", "7.86"},
		{"6.004 at 0.2 is 7.505: a tie goes up", usd, 60040, "100", "0.2", "6.004", "7.51"},
		{"6.0032 at 0.2 is 7.504 and rounds down", usd, 60032, "100", "0.2", "6.0032", "7.50"},
		{"660.00 for 7 is 94.2857…: no rounding before the division", usd, 66000, "7", "0.3125", "660/7", "137.14"},
		{"a target of 0 proposes the cost", usd, 13200, "24", "0", "5.5", "5.50"},
		{"a target just below 1 on a small cost", usd, 1, "1", "0.99", "0.01", "1.00"},
		{"yen have no minor units: 333.33… at 0.25 is 444.44…", jpy, 1000, "3", "0.25", "1000/3", "444"},
		{"1.5 yen: a tie goes up", jpy, 3, "2", "0", "1.5", "2"},
		{"a fractional quantity", usd, 123456, "12.345", "0.4", "82304/823", "166.67"},
	} {
		t.Run(c.name, func(t *testing.T) {
			unitCost, price := FormulaPrice(c.currency, cost(t, c.currency, c.cost, c.quantity, "PCS"), dec(c.target))
			if got := price.StringFixed(c.currency.MinorUnits()); got != c.price || !price.RoundHalfUp(c.currency.MinorUnits()).Equal(price) {
				t.Fatalf("price %s (%s), want %s", got, price, c.price)
			}
			want, _ := kernel.DecimalFromMinor(big.NewInt(c.cost), c.currency.MinorUnits()).Quo(dec(c.quantity))
			if !unitCost.Equal(want) || unitCost.String() != c.unitCost {
				t.Fatalf("unit cost %s, want %s", unitCost, c.unitCost)
			}
		})
	}
}

// The formula against independent integer arithmetic: with the cost in minor units C, the quantity
// scaled by 10^6 to Q and the target scaled by 10^12 to T, the price in minor units is
// N ÷ D rounded half up, N = C·10^18 and D = Q·(10^12 − T): ⌊(2N + D) ÷ 2D⌋.
func TestFormulaPriceMatchesIndependentIntegerArithmetic(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261118, 4))
	e12 := new(big.Int).Exp(big.NewInt(10), big.NewInt(12), nil)
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	for i := 0; i < 3000; i++ {
		c := rng.Int64N(100_000_000)
		q := 1 + rng.Int64N(1_000_000_000)
		tm := rng.Int64N(999_999_999_999)
		currency := []kernel.Currency{usd, jpy}[i%2]
		line := LineCost{Amount: minor(t, currency, c), Quantity: kernel.DecimalFromMinor(big.NewInt(q), 6), Unit: "PCS"}
		_, price := FormulaPrice(currency, line, kernel.DecimalFromMinor(big.NewInt(tm), 12))
		n := new(big.Int).Mul(big.NewInt(c), e18)
		d := new(big.Int).Mul(big.NewInt(q), new(big.Int).Sub(e12, big.NewInt(tm)))
		want := new(big.Int).Add(new(big.Int).Lsh(n, 1), d)
		want.Quo(want, new(big.Int).Lsh(d, 1))
		if got := price.ScaledHalfUp(currency.MinorUnits()); got.Cmp(want) != 0 {
			t.Fatalf("C=%d Q=%d T=%d %s: %s minor units, want %s", c, q, tm, currency, got, want)
		}
	}
}

// Precedence: the customer's own list price, then the general one, then the formula; a list price
// applies in the line's unit only and is proposed as published.
func TestListPriceComesBeforeTheFormula(t *testing.T) {
	lines, b := brcProposal(t)
	b.Target = decp("0.3125")
	ref := func(id string) Ref { return Ref{Kind: "sales_price", ID: id, Revision: 1} }
	general := ListPrice{Ref: ref("price.red.S"), SkuID: "ST01.red.S", Unit: "PCS", UnitPrice: dec("8.4"), BasisQuantity: dec("1")}
	own := ListPrice{Ref: ref("price.red.S.customer.1"), SkuID: "ST01.red.S", Customer: "customer.1", Unit: "PCS", UnitPrice: dec("7.9"),
		BasisQuantity: dec("1")}
	other := ListPrice{Ref: ref("price.red.S.customer.2"), SkuID: "ST01.red.S", Customer: "customer.2", Unit: "PCS", UnitPrice: dec("1"),
		BasisQuantity: dec("1")}
	carton := ListPrice{Ref: ref("price.red.M.ctn"), SkuID: "ST01.red.M", Unit: "CTN", UnitPrice: dec("90"), BasisQuantity: dec("1")}
	perHundred := ListPrice{Ref: ref("price.blue.S"), SkuID: "ST01.blue.S", Unit: "PCS", UnitPrice: dec("812.5"), BasisQuantity: dec("100")}
	low := ListPrice{Ref: ref("price.blue.M"), SkuID: "ST01.blue.M", Unit: "PCS", UnitPrice: dec("7"), BasisQuantity: dec("1")}

	// The general price wins over the formula; lines without a price keep the formula.
	b.Prices = []ListPrice{general}
	got := ProposePrices(lines, b)
	if p := got[0]; p.Source != PriceFromList || p.ListScope != ListPriceGeneral || p.List.Ref != general.Ref || p.UnitPrice.String() != "8.4" ||
		p.UnitCost != nil || p.Target != nil || p.Net.MinorString() != "20160" || p.Margin.Rate.String() != "29/84" || p.UnderFloor() {
		t.Fatalf("general: %+v", p)
	}
	for _, p := range got[1:] {
		if p.Source != PriceFromFormula || p.UnitPrice.StringFixed(2) != "8.00" {
			t.Fatalf("%s: %+v", p.Line.LineID, p)
		}
	}
	// The customer's own price wins over the general one, wherever it stands in the list; another
	// customer's price is nobody else's.
	for _, prices := range [][]ListPrice{{general, own, other}, {other, own, general}} {
		b.Prices = prices
		if p := ProposePrices(lines, b)[0]; p.Source != PriceFromList || p.ListScope != ListPriceCustomer || p.List.Ref != own.Ref ||
			p.UnitPrice.String() != "7.9" || p.Net.MinorString() != "18960" {
			t.Fatalf("own: %+v", p)
		}
	}
	b.Prices = []ListPrice{other}
	if p := ProposePrices(lines, b)[0]; p.Source != PriceFromFormula {
		t.Fatalf("another customer's price: %+v", p)
	}
	b.Customer = ""
	b.Prices = []ListPrice{own, general}
	if p := ProposePrices(lines, b)[0]; p.ListScope != ListPriceGeneral {
		t.Fatalf("no customer: %+v", p)
	}
	b.Customer = "customer.1"

	// A price in another unit is not converted; a price per 100 is proposed per 100; a list price
	// under the floor is proposed and flagged.
	b.Prices = []ListPrice{carton, perHundred, low}
	got = ProposePrices(lines, b)
	if p := got[1]; p.Source != PriceFromFormula || p.UnitPrice.StringFixed(2) != "8.00" {
		t.Fatalf("carton price on a line in pieces: %+v", p)
	}
	if p := got[2]; p.Source != PriceFromList || p.UnitPrice.String() != "812.5" || p.BasisQuantity.String() != "100" || p.Net.MinorString() != "9750" ||
		p.Margin.Rate.String() != "21/65" || p.UnderFloor() {
		t.Fatalf("per hundred: %+v", p)
	}
	// 48 × 7.00 = 336.00 against 264.00: 3/14 ≈ 21.43 %, under the floor of 25 %.
	if p := got[3]; p.Source != PriceFromList || p.Net.MinorString() != "33600" || p.Margin.Rate.String() != "3/14" || !p.UnderFloor() ||
		p.Margin.Verdict != MarginRequiresApproval {
		t.Fatalf("under the floor: %+v", p)
	}
}

// Without a list price the formula needs an adoptable snapshot that costs the line, in the quote
// currency, and a target margin; it says which is missing, in that order.
func TestTheFormulaSaysWhyItProposesNone(t *testing.T) {
	lines, full := brcProposal(t)
	full.Target = decp("0.3125")
	for _, c := range []struct {
		name   string
		edit   func(*ProposalBasis)
		reason string // of line L1
	}{
		{"no snapshot", func(b *ProposalBasis) { b.Cost = nil }, NoAdoptableSnapshot},
		{"no snapshot and no target margin", func(b *ProposalBasis) { b.Cost, b.Target = nil, nil }, NoAdoptableSnapshot},
		{"the cost is in another currency", func(b *ProposalBasis) { b.Currency = eur }, AnotherCurrency},
		{"another currency and no target margin", func(b *ProposalBasis) { b.Currency, b.Target = eur, nil }, AnotherCurrency},
		{"no target margin", func(b *ProposalBasis) { b.Target = nil }, NoTargetMargin},
		{"a target margin of 1", func(b *ProposalBasis) { b.Target = decp("1") }, NoTargetMargin},
		{"the snapshot does not cost the line", func(b *ProposalBasis) { delete(b.Cost.Lines, "L1") }, NoAdoptableSnapshot},
		{"it costs it in another unit", func(b *ProposalBasis) { b.Cost.Lines["L1"] = cost(t, usd, 13200, "24", "CTN") }, NoAdoptableSnapshot},
		{"it costs another quantity", func(b *ProposalBasis) { b.Cost.Lines["L1"] = cost(t, usd, 13200, "25", "PCS") }, NoAdoptableSnapshot},
		{"a cost of nothing", func(b *ProposalBasis) { b.Cost.Lines["L1"] = cost(t, usd, 0, "24", "PCS") }, PriceOutOfRange},
		{"less than half a cent a piece", func(b *ProposalBasis) { b.Cost.Lines["L1"] = cost(t, usd, 5, "24", "PCS") }, PriceOutOfRange},
		{"more than twelve integer digits", func(b *ProposalBasis) { b.Target = decp("0.999999999999") }, PriceOutOfRange},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines, b := brcProposal(t)
			b.Target = decp("0.3125")
			c.edit(&b)
			p := ProposePrices(lines, b)[0]
			if p.Source != PriceNone || p.Reason != c.reason || p.UnitPrice.Sign() != 0 || p.List != nil || p.UnitCost != nil || p.Margin != nil ||
				p.Net != nil || p.UnderFloor() {
				t.Fatalf("%+v", p)
			}
		})
	}
	// A list price needs neither: it is proposed without a snapshot, and then it is not judged.
	b := full
	b.Cost, b.Target = nil, nil
	b.Prices = []ListPrice{{Ref: Ref{Kind: "sales_price", ID: "p", Revision: 3}, SkuID: "ST01.red.S", Unit: "PCS", UnitPrice: dec("8"), BasisQuantity: dec("1")}}
	if p := ProposePrices(lines, b)[0]; p.Source != PriceFromList || p.Margin != nil || p.Net != nil || p.Cost != nil || p.UnderFloor() {
		t.Fatalf("%+v", p)
	}
	// Nor is it judged against a cost in another currency.
	b = full
	b.Currency = eur
	b.Prices = []ListPrice{{Ref: Ref{Kind: "sales_price", ID: "p", Revision: 3}, SkuID: "ST01.red.S", Unit: "PCS", UnitPrice: dec("8"), BasisQuantity: dec("1")}}
	if p := ProposePrices(lines, b)[0]; p.Source != PriceFromList || p.Margin != nil {
		t.Fatalf("%+v", p)
	}
}

// A formula price rounded down can lie under a floor equal to the target: 5.50 at 0.25 is 7.33,
// 24 × 7.33 = 175.92 against 132.00 is 24.966 %. The proposal flags it; it never rounds up by itself.
func TestARoundedFormulaPriceIsJudgedAgainstTheFloor(t *testing.T) {
	lines, b := brcProposal(t)
	b.Target = decp("0.25")
	p := ProposePrices(lines, b)[0]
	if p.Source != PriceFromFormula || p.UnitPrice.StringFixed(2) != "7.33" || p.Net.MinorString() != "17592" || !p.UnderFloor() ||
		p.Margin.Rate.String() != "183/733" || !p.Margin.Shortfall.Equal(dec("0.06")) {
		t.Fatalf("%+v %+v", p, p.Margin)
	}
	// 0.30 rounds up to 7.86: above the floor.
	b.Target = decp("0.3")
	if p = ProposePrices(lines, b)[0]; p.UnitPrice.StringFixed(2) != "7.86" || p.UnderFloor() || p.Net.MinorString() != "18864" {
		t.Fatalf("%+v", p)
	}
}
