package pricing

import (
	"fmt"
	"slices"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// PricingMode says how the tiers of a condition price a quantity (T02 §5).
type PricingMode string

const (
	// ModeUndetermined: the supplier's tiers do not say whether they price all units or each band.
	// The condition can be saved but stays pending: it is never verified (SUP-INV-002).
	ModeUndetermined PricingMode = ""
	ModeFlat         PricingMode = "flat"
	ModeAllUnits     PricingMode = "all_units"
	ModeGraduated    PricingMode = "graduated"
)

// TierScope is what a tier's quantity counts (T02 §13.1); only the SKU of one condition line.
type TierScope string

const (
	TierScopeSKU           TierScope = "sku" // also the meaning of an empty scope
	TierScopeExplicitGroup TierScope = "explicit_group"
)

// Tier is the half-open band [Lower, Upper) priced at UnitPrice per BasisQuantity of the schedule;
// a tier without Upper (Bounded false) is open-ended.
type Tier struct {
	Lower           kernel.Decimal
	Upper           kernel.Decimal
	Bounded         bool
	UnitPrice       kernel.Decimal
	ZeroPriceReason string
}

// Schedule is the price of one condition line or tariff: its mode, the quantity a unit price is
// quoted per and its tiers in document order.
type Schedule struct {
	Path          string
	Mode          PricingMode
	Scope         TierScope
	BasisQuantity kernel.Decimal
	Tiers         []Tier
}

// Check applies SUP-INV-002 and the price formats exactly as CST's Schedule.Check: a supported mode
// and scope (an undetermined mode is PRICE_TIER_AMBIGUOUS, an unknown one UNSUPPORTED_PRICING_MODE);
// tiers starting at 0, each ending where the next begins, so there is neither a gap nor an overlap,
// and only the last one open-ended; flat has exactly one tier; prices are not negative and a zero
// price is explained (COST_INPUT_NOT_READY otherwise).
func (s Schedule) Check() error {
	switch s.Scope {
	case "", TierScopeSKU:
	case TierScopeExplicitGroup:
		return Fail(CodeUnsupportedPricingMode, s.Path+"/price_tier_scope",
			"Tiers over an explicit group are not supported; the group's quantity is never priced per SKU instead")
	default:
		return Fail(kernel.CodeSchemaInvalid, s.Path+"/price_tier_scope", "")
	}
	switch s.Mode {
	case ModeFlat, ModeAllUnits, ModeGraduated:
	case ModeUndetermined:
		return Fail(CodePriceTierAmbiguous, s.Path+"/pricing_mode",
			"The tiers do not say whether they price all units or each band; the condition stays pending")
	default:
		return Fail(CodeUnsupportedPricingMode, s.Path+"/pricing_mode", fmt.Sprintf("Pricing mode %q is not supported", s.Mode))
	}
	if err := CheckQuantity(s.BasisQuantity, s.Path+"/price_basis_quantity"); err != nil {
		return err
	}
	n := len(s.Tiers)
	switch {
	case n == 0:
		return Fail(CodePriceTierAmbiguous, s.Path+"/tiers", "A condition has at least one tier")
	case n > MaxTiers:
		return Fail(CodeCalculationLimitExceeded, s.Path+"/tiers", "A condition has at most 50 tiers")
	case s.Mode == ModeFlat && n != 1:
		return Fail(CodePriceTierAmbiguous, s.Path+"/tiers", "A flat price has exactly one tier")
	}
	for i, t := range s.Tiers {
		p := index(s.Path, "tiers", i)
		if !fits(t.Lower, QuantityScale) {
			return Fail(kernel.CodeSchemaInvalid, p+"/lower_inclusive", "")
		}
		switch {
		case i == 0 && !t.Lower.IsZero():
			return Fail(CodePriceTierAmbiguous, p+"/lower_inclusive", "The first tier starts at 0")
		case i > 0 && !s.Tiers[i-1].Bounded:
			return Fail(CodePriceTierAmbiguous, index(s.Path, "tiers", i-1)+"/upper_exclusive", "Only the last tier is open-ended")
		case i > 0 && !s.Tiers[i-1].Upper.Equal(t.Lower):
			return Fail(CodePriceTierAmbiguous, p+"/lower_inclusive", "Tiers leave a gap or overlap; each tier starts where the previous one ends")
		}
		if t.Bounded {
			if err := CheckQuantity(t.Upper, p+"/upper_exclusive"); err != nil {
				return err
			}
			if t.Upper.Cmp(t.Lower) <= 0 {
				return Fail(CodePriceTierAmbiguous, p+"/upper_exclusive", "A tier ends after it starts")
			}
		}
		if err := checkAmount(t.UnitPrice, p+"/unit_price"); err != nil {
			return err
		}
		if t.UnitPrice.IsZero() && t.ZeroPriceReason == "" {
			return Fail(CodeCostInputNotReady, p+"/zero_price_reason", "A zero price needs a verified reason")
		}
	}
	return nil
}

// MOQScope is what a minimum order quantity counts: the SKU, or an explicit group of demand lines.
type MOQScope string

const (
	MOQScopeSKU           MOQScope = "sku"
	MOQScopeExplicitGroup MOQScope = "explicit_group"
)

// MOQ is a minimum order quantity; an explicit group names its GroupID and member demand lines.
type MOQ struct {
	Quantity kernel.Quantity
	Scope    MOQScope
	GroupID  string
	Members  []string
}

func (m MOQ) signature() string {
	members := slices.Clone(m.Members)
	slices.Sort(members)
	return fmt.Sprintf("%s|%s|%s|%s|%q", m.Quantity.Value(), m.Quantity.Unit(), m.Scope, m.GroupID, members)
}

// Condition is one condition line as the verify-time rules need it: purchase, price and tier units
// (the first supported path prices in the purchase unit, T02 §13.1), the purchase unit's precision,
// the price schedule, the MOQ, the order multiple (zero: none) and the obligations the price
// includes.
type Condition struct {
	Path                string
	LineID              string
	PurchaseUnit        kernel.UnitCode
	PurchasePrecision   int
	PriceUnit           kernel.UnitCode
	TierUnit            kernel.UnitCode
	Price               Schedule
	MOQ                 MOQ
	OrderMultiple       kernel.Decimal
	IncludedObligations []string
}

// Check applies the verify-time rules of one condition line exactly as CST's Condition.Check: its
// price schedule, units, quantities exact at the purchase unit's precision, its MOQ rule, and
// included obligations named once. demandLines are the line IDs of the request the revision
// answers; an explicit MOQ group's members must be among them.
func (c Condition) Check(demandLines []string) error {
	if err := c.Price.Check(); err != nil {
		return err
	}
	for _, u := range []struct {
		unit kernel.UnitCode
		path string
	}{{c.PriceUnit, "/price_unit_ref"}, {c.TierUnit, "/tier_unit_ref"}} {
		if u.unit != c.PurchaseUnit {
			return Fail(CodeUnsupportedPricingMode, c.Path+u.path, "Prices and tiers in another unit than the purchase unit are not supported")
		}
	}
	if c.MOQ.Quantity.Unit() != c.PurchaseUnit {
		return Fail(kernel.CodeReferenceInvalid, c.Path+"/moq/unit_ref", "The MOQ is stated in the purchase unit")
	}
	if err := CheckPrecision(c.Price.BasisQuantity, c.PurchasePrecision, c.Path+"/price_basis_quantity"); err != nil {
		return err
	}
	if err := CheckQuantity(c.MOQ.Quantity.Value(), c.Path+"/moq/quantity"); err != nil {
		return err
	}
	if err := CheckPrecision(c.MOQ.Quantity.Value(), c.PurchasePrecision, c.Path+"/moq/quantity"); err != nil {
		return err
	}
	if !c.OrderMultiple.IsZero() {
		if err := CheckQuantity(c.OrderMultiple, c.Path+"/order_multiple"); err != nil {
			return err
		}
		if err := CheckPrecision(c.OrderMultiple, c.PurchasePrecision, c.Path+"/order_multiple"); err != nil {
			return err
		}
	}
	switch c.MOQ.Scope {
	case MOQScopeSKU:
	case MOQScopeExplicitGroup:
		if c.MOQ.GroupID == "" || len(c.MOQ.Members) == 0 {
			return Fail(kernel.CodeReferenceInvalid, c.Path+"/moq", "An explicit MOQ group names its group and members")
		}
		if err := Unique(c.MOQ.Members, c.Path+"/moq/member_line_ids"); err != nil {
			return err
		}
		for i, m := range c.MOQ.Members {
			if !slices.Contains(demandLines, m) {
				return Fail(kernel.CodeReferenceInvalid, index(c.Path, "moq/member_line_ids", i), "The member is not a line of the request")
			}
		}
	default:
		return Fail(kernel.CodeSchemaInvalid, c.Path+"/moq/scope", "")
	}
	return Unique(c.IncludedObligations, c.Path+"/included_obligation_ids")
}

// CheckConditions checks the condition lines of one revision exactly as CST's CheckConditions: line
// IDs once, each line by Check, and one rule per explicit MOQ group (REFERENCE_INVALID when lines of
// the group state it differently).
func CheckConditions(lines []Condition, demandLines []string) error {
	ids := make([]string, len(lines))
	groups := map[string]string{}
	for i, c := range lines {
		ids[i] = c.LineID
		if err := c.Check(demandLines); err != nil {
			return err
		}
		if c.MOQ.Scope == MOQScopeExplicitGroup {
			sig := c.MOQ.signature()
			if prev, ok := groups[c.MOQ.GroupID]; ok && prev != sig {
				return Fail(kernel.CodeReferenceInvalid, c.Path+"/moq", "Lines of one MOQ group state different rules")
			}
			groups[c.MOQ.GroupID] = sig
		}
	}
	if i := repeated(ids); i >= 0 {
		return Fail(kernel.CodeDuplicateLineID, lines[i].Path+"/id", "")
	}
	return nil
}
