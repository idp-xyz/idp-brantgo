package rules

import (
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// PricingMode says how the tiers of a supply condition price a quantity (T02 §5).
type PricingMode string

const (
	// ModeUndetermined: the supplier's tiers do not say whether they price all units or each band.
	// The condition can be saved but stays pending: it is never verified or priced (SUP-INV-002).
	ModeUndetermined PricingMode = ""
	// ModeFlat: one tier, one price for every quantity.
	ModeFlat PricingMode = "flat"
	// ModeAllUnits: the tier the whole quantity falls in prices every unit.
	ModeAllUnits PricingMode = "all_units"
	// ModeGraduated: each band prices only the units inside it; a boundary decides which band a
	// unit belongs to and adds no unit (150 at [0,100) 5 and [100,∞) 4 is 100×5 + 50×4).
	ModeGraduated PricingMode = "graduated"
)

// TierScope is what a tier's quantity counts (T02 §13.1). Only the SKU of one condition line is
// supported; tiers over an explicit group would need their own charge object and allocation and
// are refused rather than priced per SKU.
type TierScope string

const (
	TierScopeSKU           TierScope = "sku" // also the meaning of an empty scope
	TierScopeExplicitGroup TierScope = "explicit_group"
)

// Tier is the half-open band [Lower, Upper) priced at UnitPrice per BasisQuantity of the schedule;
// a tier without Upper (Bounded false) is open-ended. A zero UnitPrice needs a ZeroPriceReason.
type Tier struct {
	Lower           kernel.Decimal
	Upper           kernel.Decimal
	Bounded         bool
	UnitPrice       kernel.Decimal
	ZeroPriceReason string
}

// Schedule is the price of one condition line or billing rule: its mode, the quantity a unit price
// is quoted per (price_basis_quantity: 1 for each, 12 for a dozen, 100 for a hundred) and its tiers
// in document order, which must be ascending and contiguous from 0 (SUP-INV-002).
type Schedule struct {
	Path          string
	Mode          PricingMode
	Scope         TierScope
	BasisQuantity kernel.Decimal
	Tiers         []Tier
}

// Check applies SUP-INV-002 and the price formats: a supported mode and scope (an undetermined mode
// is PRICE_TIER_AMBIGUOUS, an unknown one UNSUPPORTED_PRICING_MODE); tiers starting at 0, each
// ending where the next begins, so there is neither a gap nor an overlap, and only the last one
// open-ended; flat has exactly one tier; prices are not negative and a zero price is explained
// (COST_INPUT_NOT_READY otherwise). A supply condition is verified only when Check passes.
func (s Schedule) Check() error {
	switch s.Scope {
	case "", TierScopeSKU:
	case TierScopeExplicitGroup:
		return fail(CodeUnsupportedPricingMode, s.Path+"/price_tier_scope",
			"Tiers over an explicit group are not supported; the group's quantity is never priced per SKU instead")
	default:
		return fail(kernel.CodeSchemaInvalid, s.Path+"/price_tier_scope", "")
	}
	switch s.Mode {
	case ModeFlat, ModeAllUnits, ModeGraduated:
	case ModeUndetermined:
		return fail(CodePriceTierAmbiguous, s.Path+"/pricing_mode",
			"The tiers do not say whether they price all units or each band; the condition stays pending")
	default:
		return fail(CodeUnsupportedPricingMode, s.Path+"/pricing_mode", fmt.Sprintf("Pricing mode %q is not supported", s.Mode))
	}
	if err := checkQuantity(s.BasisQuantity, s.Path+"/price_basis_quantity"); err != nil {
		return err
	}
	n := len(s.Tiers)
	switch {
	case n == 0:
		return fail(CodePriceTierAmbiguous, s.Path+"/tiers", "A condition has at least one tier")
	case n > MaxTiers:
		return fail(CodeCalculationLimitExceeded, s.Path+"/tiers", "A condition has at most 50 tiers")
	case s.Mode == ModeFlat && n != 1:
		return fail(CodePriceTierAmbiguous, s.Path+"/tiers", "A flat price has exactly one tier")
	}
	for i, t := range s.Tiers {
		p := index(s.Path, "tiers", i)
		if !fits(t.Lower, QuantityScale) {
			return fail(kernel.CodeSchemaInvalid, p+"/lower_inclusive", "")
		}
		switch {
		case i == 0 && !t.Lower.IsZero():
			return fail(CodePriceTierAmbiguous, p+"/lower_inclusive", "The first tier starts at 0")
		case i > 0 && !s.Tiers[i-1].Bounded:
			return fail(CodePriceTierAmbiguous, index(s.Path, "tiers", i-1)+"/upper_exclusive", "Only the last tier is open-ended")
		case i > 0 && !s.Tiers[i-1].Upper.Equal(t.Lower):
			return fail(CodePriceTierAmbiguous, p+"/lower_inclusive", "Tiers leave a gap or overlap; each tier starts where the previous one ends")
		}
		if t.Bounded {
			if err := checkQuantity(t.Upper, p+"/upper_exclusive"); err != nil {
				return err
			}
			if t.Upper.Cmp(t.Lower) <= 0 {
				return fail(CodePriceTierAmbiguous, p+"/upper_exclusive", "A tier ends after it starts")
			}
		}
		if err := checkAmount(t.UnitPrice, p+"/unit_price"); err != nil {
			return err
		}
		if t.UnitPrice.IsZero() && t.ZeroPriceReason == "" {
			return fail(CodeCostInputNotReady, p+"/zero_price_reason", "A zero price needs a verified reason")
		}
	}
	return nil
}

// Amount returns the exact amount of quantity q, in the schedule's price unit, before tax and
// before any rounding (T02 §13.3: an amount is rounded only at the tax split). all_units and flat
// price every unit at the tier that contains q; graduated prices each band's units at its own tier
// and sums the bands exactly. A quantity at or beyond the end of a bounded last tier is not
// covered (PRICE_TIER_AMBIGUOUS at quantityPath).
func (s Schedule) Amount(q kernel.Decimal, quantityPath string) (kernel.Decimal, error) {
	if err := s.Check(); err != nil {
		return kernel.Decimal{}, err
	}
	if err := checkMeasure(q, quantityPath); err != nil {
		return kernel.Decimal{}, err
	}
	last := s.Tiers[len(s.Tiers)-1]
	if last.Bounded && q.Cmp(last.Upper) >= 0 {
		return kernel.Decimal{}, fail(CodePriceTierAmbiguous, quantityPath, "The quantity lies beyond the last tier")
	}
	per := func(units kernel.Decimal, price kernel.Decimal) kernel.Decimal {
		v, _ := units.Quo(s.BasisQuantity) // BasisQuantity is positive (Check)
		return v.Mul(price)
	}
	if s.Mode != ModeGraduated {
		for _, t := range s.Tiers {
			if t.Lower.Cmp(q) <= 0 && (!t.Bounded || q.Cmp(t.Upper) < 0) {
				return per(q, t.UnitPrice), nil
			}
		}
		return kernel.Decimal{}, fail(CodePriceTierAmbiguous, quantityPath, "No tier contains the quantity")
	}
	total := kernel.DecimalFromInt(0)
	for _, t := range s.Tiers {
		hi := q
		if t.Bounded && t.Upper.Cmp(q) < 0 {
			hi = t.Upper
		}
		if band := hi.Sub(t.Lower); band.Sign() > 0 {
			total = total.Add(per(band, t.UnitPrice))
		}
	}
	return total, nil
}
