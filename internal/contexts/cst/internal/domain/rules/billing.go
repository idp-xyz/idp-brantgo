package rules

import (
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// ChargeableKind is what a logistics charge is billed on (T02 §6).
type ChargeableKind string

const (
	ChargeFixed               ChargeableKind = "fixed"                 // a fixed charge, quantity 1
	ChargeActualWeight        ChargeableKind = "actual_weight"         // gross kg
	ChargeVolume              ChargeableKind = "volume"                // m³
	ChargeMaxActualVolumetric ChargeableKind = "max_actual_volumetric" // the larger of gross kg and volumetric kg
)

// Rounding is how a chargeable quantity is rounded to the billing increment.
type Rounding string

const (
	RoundCeil   Rounding = "ceil"
	RoundHalfUp Rounding = "half_up"
)

// RoundingScope is where the rounding (and, for max_actual_volumetric, the maximum) applies.
type RoundingScope string

const (
	PerPackage RoundingScope = "per_package" // per package, then times the count and summed
	Shipment   RoundingScope = "shipment"    // once, on the shipment totals
)

// ChargeableRule is how the chargeable quantity of a charge follows from a measurement. The
// volumetric divisor is explicit, in m³ per kg (volumetric kg = m³ ÷ divisor); no default such as
// 6000 or 5000 is assumed. MaxScope must equal RoundingScope for max_actual_volumetric: mixing them
// has no defined order (T02 §13.8).
type ChargeableRule struct {
	Path             string
	Kind             ChargeableKind
	BillingIncrement kernel.Decimal
	Rounding         Rounding
	RoundingScope    RoundingScope
	MaxScope         RoundingScope
	DivisorM3PerKG   kernel.Decimal
}

// Check applies the verify-time rules of a chargeable rule.
func (r ChargeableRule) Check() error {
	switch r.Kind {
	case ChargeFixed:
		return nil
	case ChargeActualWeight, ChargeVolume, ChargeMaxActualVolumetric:
	default:
		return fail(CodeUnsupportedPricingMode, r.Path+"/kind", fmt.Sprintf("Chargeable basis %q is not supported", r.Kind))
	}
	if r.BillingIncrement.Sign() <= 0 || !fits(r.BillingIncrement, QuantityScale) {
		return fail(kernel.CodeInvalidKnownValue, r.Path+"/billing_increment", "A billing increment is positive with at most 6 fractional digits")
	}
	switch r.Rounding {
	case RoundCeil, RoundHalfUp:
	default:
		return fail(kernel.CodeSchemaInvalid, r.Path+"/rounding_mode", "")
	}
	switch r.RoundingScope {
	case PerPackage, Shipment:
	default:
		return fail(kernel.CodeSchemaInvalid, r.Path+"/rounding_scope", "")
	}
	if r.Kind == ChargeMaxActualVolumetric {
		if r.MaxScope != r.RoundingScope {
			return fail(CodeUnsupportedPricingMode, r.Path+"/max_scope", "The maximum and the rounding apply at the same level")
		}
		if r.DivisorM3PerKG.Sign() <= 0 || !fits(r.DivisorM3PerKG, FactorScale) {
			return fail(kernel.CodeInvalidKnownValue, r.Path+"/divisor_m3_per_kg", "A volumetric divisor is positive and explicit")
		}
	}
	return nil
}

func (r ChargeableRule) round(q kernel.Decimal) kernel.Decimal {
	steps, _ := q.Quo(r.BillingIncrement) // positive increment (Check)
	var n kernel.Decimal
	if r.Rounding == RoundCeil {
		n = kernel.DecimalFromBigInt(steps.Neg().Floor()).Neg()
	} else {
		n = kernel.DecimalFromBigInt(steps.ScaledHalfUp(0))
	}
	return n.Mul(r.BillingIncrement)
}

// values returns the pair the rule takes the maximum of for one package: gross kg and volumetric
// kg, or the measured quantity twice.
func (r ChargeableRule) values(p Package) (kernel.Decimal, kernel.Decimal) {
	switch r.Kind {
	case ChargeActualWeight:
		return p.GrossKG, p.GrossKG
	case ChargeVolume:
		return p.Volume(), p.Volume()
	}
	volumetric, _ := p.Volume().Quo(r.DivisorM3PerKG) // positive divisor (Check)
	return p.GrossKG, volumetric
}

// Quantity returns the chargeable quantity of measurement m (T02 §13.8): per package, the maximum
// of the package's values rounded to the increment, times its count, summed; per shipment, the
// maximum of the summed values, rounded once. Two packages of 10 kg / 1 kg volumetric and 1 kg /
// 10 kg volumetric are 20 kg per package and 11 kg per shipment. A fixed charge is quantity 1.
func (r ChargeableRule) Quantity(m *Measurement) (kernel.Decimal, error) {
	if err := r.Check(); err != nil {
		return kernel.Decimal{}, err
	}
	if r.Kind == ChargeFixed {
		return one, nil
	}
	if m == nil {
		return kernel.Decimal{}, fail(CodeCostInputNotReady, r.Path+"/measurement_ref", "A measured charge needs a verified measurement")
	}
	larger := func(a, b kernel.Decimal) kernel.Decimal {
		if a.Cmp(b) >= 0 {
			return a
		}
		return b
	}
	total := kernel.DecimalFromInt(0)
	if r.RoundingScope == PerPackage {
		for _, p := range m.Packages {
			total = total.Add(r.round(larger(r.values(p))).Mul(kernel.DecimalFromInt(p.Count)))
		}
		return total, nil
	}
	sumA, sumB := kernel.DecimalFromInt(0), kernel.DecimalFromInt(0)
	for _, p := range m.Packages {
		a, b := r.values(p)
		n := kernel.DecimalFromInt(p.Count)
		sumA, sumB = sumA.Add(a.Mul(n)), sumB.Add(b.Mul(n))
	}
	return r.round(larger(sumA, sumB)), nil
}

// BillingKind is how a charge's amount follows from its chargeable quantity.
type BillingKind string

const (
	BillFixed    BillingKind = "fixed"     // Amount
	BillPerBasis BillingKind = "per_basis" // quantity ÷ BasisQuantity × Rate
	BillTiered   BillingKind = "tiered"    // Tiers over the quantity
)

// Billing is a charge's tariff. Per-basis and tiered charges are in Unit (KG, or M3 for a volume
// basis) and never below Minimum, applied to the same chargeable object.
type Billing struct {
	Path            string
	Kind            BillingKind
	Amount          kernel.Decimal
	Rate            kernel.Decimal
	BasisQuantity   kernel.Decimal
	Minimum         kernel.Decimal
	Unit            kernel.UnitCode
	Tiers           Schedule // tiered: mode and tiers; its basis is BasisQuantity
	ZeroPriceReason string
}

// Check applies the verify-time rules of a tariff together with its chargeable rule: a fixed
// chargeable basis only with a fixed tariff, a measured one only with a per-basis or tiered tariff
// in the unit it measures; amounts of the price format; a zero fixed amount or rate explained.
func (b Billing) Check(rule ChargeableRule) error {
	if err := rule.Check(); err != nil {
		return err
	}
	switch b.Kind {
	case BillFixed:
		if rule.Kind != ChargeFixed {
			return fail(CodeUnsupportedPricingMode, b.Path+"/kind", "A measured chargeable basis is billed per basis or by tiers")
		}
		if err := checkAmount(b.Amount, b.Path+"/amount"); err != nil {
			return err
		}
		if b.Amount.IsZero() && b.ZeroPriceReason == "" {
			return fail(CodeCostInputNotReady, b.Path+"/zero_price_reason", "A zero charge needs a verified reason")
		}
		return nil
	case BillPerBasis, BillTiered:
	default:
		return fail(CodeUnsupportedPricingMode, b.Path+"/kind", fmt.Sprintf("Billing %q is not supported", b.Kind))
	}
	if rule.Kind == ChargeFixed {
		return fail(CodeUnsupportedPricingMode, b.Path+"/kind", "A fixed chargeable basis takes a fixed amount")
	}
	want := kernel.UnitCode("KG")
	if rule.Kind == ChargeVolume {
		want = "M3"
	}
	if b.Unit != want {
		return fail(kernel.CodeReferenceInvalid, b.Path+"/unit_ref", fmt.Sprintf("The chargeable quantity is in %s", want))
	}
	if err := checkQuantity(b.BasisQuantity, b.Path+"/basis_quantity"); err != nil {
		return err
	}
	if err := checkAmount(b.Minimum, b.Path+"/minimum_amount"); err != nil {
		return err
	}
	if b.Kind == BillTiered {
		s := b.Tiers
		s.Path, s.BasisQuantity, s.Scope = b.Path, b.BasisQuantity, TierScopeSKU
		return s.Check()
	}
	if err := checkAmount(b.Rate, b.Path+"/rate"); err != nil {
		return err
	}
	if b.Rate.IsZero() && b.ZeroPriceReason == "" {
		return fail(CodeCostInputNotReady, b.Path+"/zero_price_reason", "A zero rate needs a verified reason")
	}
	return nil
}

// Charge returns the exact amount of a charge of chargeable quantity q, before tax: the fixed
// amount, or the per-basis or tiered amount raised to the minimum charge.
func (b Billing) Charge(rule ChargeableRule, q kernel.Decimal) (kernel.Decimal, error) {
	if err := b.Check(rule); err != nil {
		return kernel.Decimal{}, err
	}
	var raw kernel.Decimal
	switch b.Kind {
	case BillFixed:
		return b.Amount, nil
	case BillPerBasis:
		if err := checkMeasure(q, rule.Path); err != nil {
			return kernel.Decimal{}, err
		}
		v, _ := q.Quo(b.BasisQuantity)
		raw = v.Mul(b.Rate)
	default:
		s := b.Tiers
		s.Path, s.BasisQuantity, s.Scope = b.Path, b.BasisQuantity, TierScopeSKU
		var err error
		if raw, err = s.Amount(q, rule.Path); err != nil {
			return kernel.Decimal{}, err
		}
	}
	if raw.Cmp(b.Minimum) < 0 {
		return b.Minimum, nil
	}
	return raw, nil
}
