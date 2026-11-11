package rules

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// twoPackages is T02 §13.8's example: actual / volumetric weight of 10 / 1 kg and 1 / 10 kg. With a
// divisor of 0.005 m³/kg the volumes are 0.005 and 0.05 m³.
func twoPackages() *Measurement {
	return &Measurement{Path: "/measurement", Kind: Measured, ObservedAt: clock, Packages: []Package{
		{Path: "/measurement/packages/0", ID: "P1", Count: 1, LengthM: dec("0.5"), WidthM: dec("0.1"), HeightM: dec("0.1"), NetKG: dec("9"), GrossKG: dec("10"), LineIDs: []string{"L1"}},
		{Path: "/measurement/packages/1", ID: "P2", Count: 1, LengthM: dec("0.5"), WidthM: dec("0.1"), HeightM: dec("1"), NetKG: dec("0.5"), GrossKG: dec("1"), LineIDs: []string{"L2"}},
	}}
}

func volumetric(scope RoundingScope) ChargeableRule {
	return ChargeableRule{Path: "/fees/0/chargeable", Kind: ChargeMaxActualVolumetric, BillingIncrement: dec("1"),
		Rounding: RoundCeil, RoundingScope: scope, MaxScope: scope, DivisorM3PerKG: dec("0.005")}
}

// T02 §13.8: per package max(10, 1) + max(1, 10) = 20 kg; per shipment max(11, 11) = 11 kg.
func TestChargeableScopes(t *testing.T) {
	for scope, want := range map[RoundingScope]string{PerPackage: "20", Shipment: "11"} {
		got, err := volumetric(scope).Quantity(twoPackages())
		ok(t, err)
		if got.String() != want {
			t.Errorf("%s: %s, want %s", scope, got, want)
		}
	}
	mixed := volumetric(PerPackage)
	mixed.MaxScope = Shipment
	_, err := mixed.Quantity(twoPackages())
	rejected(t, err, CodeUnsupportedPricingMode, "/fees/0/chargeable/max_scope")
	noDivisor := volumetric(Shipment)
	noDivisor.DivisorM3PerKG = dec("0")
	_, err = noDivisor.Quantity(twoPackages())
	rejected(t, err, kernel.CodeInvalidKnownValue, "/fees/0/chargeable/divisor_m3_per_kg")
	_, err = volumetric(Shipment).Quantity(nil)
	rejected(t, err, CodeCostInputNotReady, "/fees/0/chargeable/measurement_ref")
}

// Rounding to the billing increment: 10.2 kg in 0.5 kg steps is 10.5 (ceil) or 10 (half_up);
// 10.25 half_up is 10.5. Two packages of 3.2 kg per package round each to 3.5 before the count.
func TestBillingIncrement(t *testing.T) {
	m := &Measurement{Path: "/m", Kind: Measured, ObservedAt: clock, Packages: []Package{
		{Path: "/m/packages/0", ID: "P1", Count: 1, LengthM: dec("1"), WidthM: dec("1"), HeightM: dec("1"), NetKG: dec("10"), GrossKG: dec("10.2"), LineIDs: []string{"L1"}},
	}}
	rule := ChargeableRule{Path: "/c", Kind: ChargeActualWeight, BillingIncrement: dec("0.5"), Rounding: RoundCeil, RoundingScope: Shipment}
	for _, c := range []struct {
		rounding Rounding
		gross    string
		want     string
	}{{RoundCeil, "10.2", "10.5"}, {RoundHalfUp, "10.2", "10"}, {RoundHalfUp, "10.25", "10.5"}, {RoundCeil, "10", "10"}} {
		rule.Rounding = c.rounding
		m.Packages[0].GrossKG = dec(c.gross)
		got, err := rule.Quantity(m)
		ok(t, err)
		if got.String() != c.want {
			t.Errorf("%s %s: %s, want %s", c.rounding, c.gross, got, c.want)
		}
	}
	m.Packages[0].GrossKG, m.Packages[0].Count = dec("3.2"), 2
	rule.Rounding, rule.RoundingScope = RoundCeil, PerPackage
	got, err := rule.Quantity(m)
	ok(t, err)
	if got.String() != "7" {
		t.Errorf("per package: %s, want 7", got)
	}
	volume := ChargeableRule{Path: "/c", Kind: ChargeVolume, BillingIncrement: dec("0.1"), Rounding: RoundCeil, RoundingScope: Shipment}
	got, err = volume.Quantity(m) // 2 × 1 m³
	ok(t, err)
	if got.String() != "2" {
		t.Errorf("volume: %s, want 2", got)
	}
}

// Tariffs: fixed (the fixture's 60 USD freight), per basis with a minimum, tiered; a fixed
// chargeable basis takes a fixed amount only; a zero charge is explained; a measured basis is in KG
// or M3.
func TestBilling(t *testing.T) {
	fixed := ChargeableRule{Path: "/fees/0/chargeable", Kind: ChargeFixed}
	weight := ChargeableRule{Path: "/fees/0/chargeable", Kind: ChargeActualWeight, BillingIncrement: dec("1"), Rounding: RoundCeil, RoundingScope: Shipment}
	freight := Billing{Path: "/fees/0/billing", Kind: BillFixed, Amount: dec("60")}
	q, err := fixed.Quantity(nil)
	ok(t, err)
	got, err := freight.Charge(fixed, q)
	ok(t, err)
	if got.String() != "60" {
		t.Fatalf("fixed: %s", got)
	}
	perKG := Billing{Path: "/fees/0/billing", Kind: BillPerBasis, Rate: dec("2.5"), BasisQuantity: dec("10"), Minimum: dec("30"), Unit: "KG"}
	for kg, want := range map[string]string{"200": "50", "100": "30"} { // 200/10×2.5 = 50; 25 < 30 → 30
		got, err := perKG.Charge(weight, dec(kg))
		ok(t, err)
		if got.String() != want {
			t.Errorf("per basis %s kg: %s, want %s", kg, got, want)
		}
	}
	tiered := Billing{Path: "/fees/0/billing", Kind: BillTiered, BasisQuantity: dec("1"), Minimum: dec("0"), Unit: "KG", Tiers: twoTiers(ModeGraduated)}
	got, err = tiered.Charge(weight, dec("150"))
	ok(t, err)
	if got.String() != "700" {
		t.Errorf("tiered: %s, want 700", got)
	}

	zero := freight
	zero.Amount = dec("0")
	explained := zero
	explained.ZeroPriceReason = "included in the annual contract, verified"
	ok(t, explained.Check(fixed))
	for _, c := range []struct {
		name       string
		b          Billing
		rule       ChargeableRule
		code, path string
	}{
		{"zero fixed charge", zero, fixed, CodeCostInputNotReady, "/fees/0/billing/zero_price_reason"},
		{"fixed basis billed per kg", perKG, fixed, CodeUnsupportedPricingMode, "/fees/0/billing/kind"},
		{"measured basis billed fixed", freight, weight, CodeUnsupportedPricingMode, "/fees/0/billing/kind"},
		{"weight billed in M3", Billing{Path: "/fees/0/billing", Kind: BillPerBasis, Rate: dec("1"), BasisQuantity: dec("1"), Minimum: dec("0"), Unit: "M3"}, weight,
			kernel.CodeReferenceInvalid, "/fees/0/billing/unit_ref"},
		{"zero rate", Billing{Path: "/fees/0/billing", Kind: BillPerBasis, Rate: dec("0"), BasisQuantity: dec("1"), Minimum: dec("0"), Unit: "KG"}, weight,
			CodeCostInputNotReady, "/fees/0/billing/zero_price_reason"},
		{"negative charge is not a discount", Billing{Path: "/fees/0/billing", Kind: BillFixed, Amount: dec("-5")}, fixed,
			kernel.CodeInvalidKnownValue, "/fees/0/billing/amount"},
		{"unknown tariff", Billing{Path: "/fees/0/billing", Kind: "zone_matrix"}, weight, CodeUnsupportedPricingMode, "/fees/0/billing/kind"},
		{"unknown chargeable basis", freight, ChargeableRule{Path: "/fees/0/chargeable", Kind: "per_container"}, CodeUnsupportedPricingMode, "/fees/0/chargeable/kind"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rejected(t, c.b.Check(c.rule), c.code, c.path)
		})
	}
}
