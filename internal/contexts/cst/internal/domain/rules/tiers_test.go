package rules

import (
	"fmt"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// T02 §11 全量／累进阶梯 (tactical test T02-S04): 150 PCS on [0,100) at 5 and [100,∞) at 4 is
// 600 all_units and 100×5 + 50×4 = 700 graduated; an unknown mode is refused.
func TestTierModes(t *testing.T) {
	q := dec("150")
	for _, c := range []struct {
		mode PricingMode
		want string
	}{{ModeAllUnits, "600"}, {ModeGraduated, "700"}} {
		got, err := twoTiers(c.mode).Amount(q, "/quantity")
		ok(t, err)
		if got.String() != c.want {
			t.Errorf("%s: %s, want %s", c.mode, got, c.want)
		}
	}
	_, err := twoTiers("volume_discount").Amount(q, "/quantity")
	rejected(t, err, CodeUnsupportedPricingMode, "/lines/0/pricing_mode")
}

// SUP-INV-002 and T02 §5: the boundary only decides the band (100 graduated is 100×5, not
// 101 units); Python test_costing_domains.py test_graduated_tiers_price_each_band_once prices 120
// PCS at 100×5 + 20×4 = 580.
func TestGraduatedBoundary(t *testing.T) {
	for q, want := range map[string]string{"100": "500", "99": "495", "101": "504", "120": "580", "0.5": "2.5"} {
		got, err := twoTiers(ModeGraduated).Amount(dec(q), "/quantity")
		ok(t, err)
		if got.String() != want {
			t.Errorf("graduated %s: %s, want %s", q, got, want)
		}
	}
	got, err := twoTiers(ModeAllUnits).Amount(dec("100"), "/quantity")
	ok(t, err)
	if got.String() != "400" {
		t.Errorf("all_units 100: %s, want 400 (the tier that contains 100)", got)
	}
}

// T02 §11 两家同口径比较: supplier B quotes 480 USD per 100 PCS; 120 PCS cost 120 ÷ 100 × 480 = 576.
func TestPriceBasisQuantity(t *testing.T) {
	s := flat("/lines/0", "480")
	s.BasisQuantity = dec("100")
	got, err := s.Amount(dec("120"), "/quantity")
	ok(t, err)
	if got.String() != "576" {
		t.Fatalf("got %s, want 576", got)
	}
}

// SUP-INV-002: tiers start at 0, are contiguous without overlap, and only the last is open-ended;
// a mode the offer does not determine stays pending (PRICE_TIER_AMBIGUOUS); group tiers are
// UNSUPPORTED_PRICING_MODE (T02 §13.1, §13.9: 150 in a group is never 750, 700 or 1400).
func TestScheduleRefusals(t *testing.T) {
	gap := twoTiers(ModeGraduated)
	gap.Tiers[1].Lower = dec("101") // Python test_gap_in_price_tiers_cannot_be_verified
	overlap := twoTiers(ModeGraduated)
	overlap.Tiers[1].Lower = dec("90")
	firstNotZero := twoTiers(ModeGraduated)
	firstNotZero.Tiers[0].Lower = dec("1")
	openMiddle := twoTiers(ModeGraduated)
	openMiddle.Tiers[0].Bounded = false
	empty := twoTiers(ModeGraduated)
	empty.Tiers[1].Upper, empty.Tiers[1].Bounded = dec("100"), true
	flatTwo := twoTiers(ModeFlat)
	pending := twoTiers(ModeUndetermined)
	group := twoTiers(ModeGraduated)
	group.Scope = TierScopeExplicitGroup
	zero := flat("/lines/0", "0") // Python test_unexplained_zero_unit_price_is_rejected
	explained := flat("/lines/0", "0")
	explained.Tiers[0].ZeroPriceReason = "verified free sample"
	negative := flat("/lines/0", "0")
	negative.Tiers[0].UnitPrice = dec("-1")
	tooPrecise := flat("/lines/0", "0.000000001")
	tooLarge := flat("/lines/0", "1")
	tooLarge.Tiers[0].UnitPrice = kernel.DecimalFromInt(1_000_000_000_000).Add(dec("0.01"))
	noBasis := flat("/lines/0", "5")
	noBasis.BasisQuantity = dec("0")
	many := Schedule{Path: "/lines/0", Mode: ModeGraduated, BasisQuantity: dec("1")}
	for i := range MaxTiers + 1 {
		many.Tiers = append(many.Tiers, Tier{Lower: dec(fmt.Sprint(i)), Upper: dec(fmt.Sprint(i + 1)), Bounded: true, UnitPrice: dec("1")})
	}

	for _, c := range []struct {
		name       string
		s          Schedule
		code, path string
	}{
		{"gap", gap, CodePriceTierAmbiguous, "/lines/0/tiers/1/lower_inclusive"},
		{"overlap", overlap, CodePriceTierAmbiguous, "/lines/0/tiers/1/lower_inclusive"},
		{"first tier above 0", firstNotZero, CodePriceTierAmbiguous, "/lines/0/tiers/0/lower_inclusive"},
		{"open-ended middle tier", openMiddle, CodePriceTierAmbiguous, "/lines/0/tiers/0/upper_exclusive"},
		{"empty band", empty, CodePriceTierAmbiguous, "/lines/0/tiers/1/upper_exclusive"},
		{"flat with two tiers", flatTwo, CodePriceTierAmbiguous, "/lines/0/tiers"},
		{"undetermined mode stays pending", pending, CodePriceTierAmbiguous, "/lines/0/pricing_mode"},
		{"group tiers", group, CodeUnsupportedPricingMode, "/lines/0/price_tier_scope"},
		{"zero price without reason", zero, CodeCostInputNotReady, "/lines/0/tiers/0/zero_price_reason"},
		{"negative price", negative, kernel.CodeInvalidKnownValue, "/lines/0/tiers/0/unit_price"},
		{"nine fractional digits", tooPrecise, kernel.CodeSchemaInvalid, "/lines/0/tiers/0/unit_price"},
		{"price above 10^12", tooLarge, CodeCalculationLimitExceeded, "/lines/0/tiers/0/unit_price"},
		{"zero basis", noBasis, kernel.CodeInvalidKnownValue, "/lines/0/price_basis_quantity"},
		{"51 tiers", many, CodeCalculationLimitExceeded, "/lines/0/tiers"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rejected(t, c.s.Check(), c.code, c.path)
		})
	}
	ok(t, explained.Check())
}

// A bounded last tier does not cover quantities at or beyond its end.
func TestQuantityBeyondLastTier(t *testing.T) {
	s := twoTiers(ModeAllUnits)
	s.Tiers[1].Upper, s.Tiers[1].Bounded = dec("200"), true
	_, err := s.Amount(dec("200"), "/allocations/0/purchase_quantity")
	rejected(t, err, CodePriceTierAmbiguous, "/allocations/0/purchase_quantity")
	_, err = s.Amount(dec("0"), "/allocations/0/purchase_quantity")
	rejected(t, err, kernel.CodeInvalidKnownValue, "/allocations/0/purchase_quantity")
}

// Independent arithmetic: for whole quantities, a graduated amount equals the sum over single units
// each priced at the band it falls in, and an all_units amount equals the quantity times the price
// of the band of the last unit, computed in integer cents.
func TestTiersAgainstUnitByUnitOracle(t *testing.T) {
	s := Schedule{Path: "/lines/0", Mode: ModeGraduated, BasisQuantity: dec("1"), Tiers: []Tier{
		{Lower: dec("0"), Upper: dec("10"), Bounded: true, UnitPrice: dec("7.25")},
		{Lower: dec("10"), Upper: dec("37"), Bounded: true, UnitPrice: dec("6.10")},
		{Lower: dec("37"), Upper: dec("120"), Bounded: true, UnitPrice: dec("5.05")},
		{Lower: dec("120"), UnitPrice: dec("4.99")},
	}}
	cents := []int64{725, 610, 505, 499}
	bounds := []int64{0, 10, 37, 120}
	band := func(unit int64) int { // unit u occupies [u-1, u)
		b := 0
		for i, lo := range bounds {
			if unit-1 >= lo {
				b = i
			}
		}
		return b
	}
	for q := int64(1); q <= 400; q++ {
		var graduated int64
		for u := int64(1); u <= q; u++ {
			graduated += cents[band(u)]
		}
		allUnits := q * cents[band(q+1)] // the band that contains q itself
		for mode, want := range map[PricingMode]int64{ModeGraduated: graduated, ModeAllUnits: allUnits} {
			s.Mode = mode
			got, err := s.Amount(kernel.DecimalFromInt(q), "/quantity")
			ok(t, err)
			if got.ScaledHalfUp(2).Int64() != want || !got.RoundHalfUp(2).Equal(got) {
				t.Fatalf("%s %d: %s, want %d cents", mode, q, got, want)
			}
		}
	}
}
