package draft

import (
	"fmt"
	"time"

	_ "time/tzdata" // delivery time zones do not depend on the host's zone database

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The pricing rules a draft applies to its price groups restate CST's (PUR-INV-005 "MOQ、倍数与阶梯
// 复用核价规则"; CST domain/rules Schedule.Check, Schedule.Amount, Purchase, TaxTreatment.Cost and
// Python costing_engine.rules tiers_cost, split_tax): PUR cannot import CST's domain, and SUP
// restates the same rules for verification (sup/internal/domain/pricing). CST's order, codes and
// meanings are kept; a change on either side changes both and their vectors.

// Pricing modes and tier scopes.
const (
	ModeFlat      = "flat"
	ModeAllUnits  = "all_units"
	ModeGraduated = "graduated"
	TierScopeSKU  = "sku"

	// Tax price bases and recovery treatments (POL tax_profile).
	BasisInclusive     = "inclusive"
	BasisExclusive     = "exclusive"
	BasisNotApplicable = "not_applicable"
	RecoveryUnknown    = "unknown"
	Recoverable        = "recoverable"
	Nonrecoverable     = "nonrecoverable"
)

var (
	zero        = kernel.DecimalFromInt(0)
	one         = kernel.DecimalFromInt(1)
	maxQuantity = kernel.DecimalFromInt(1_000_000)
)

type tier struct {
	lower, upper kernel.Decimal
	bounded      bool
	price        kernel.Decimal
	raw          Tier
}

// schedule is a condition line's price: mode, basis quantity and tiers in document order.
type schedule struct {
	mode  string
	basis kernel.Decimal
	tiers []tier
}

// AppliedTier is a tier with the quantity it priced.
type AppliedTier struct {
	Tier
	Quantity kernel.Decimal
}

func decimal(s string, f kernel.DecimalFormat, path string) (kernel.Decimal, error) {
	d, err := kernel.ParseDecimal(s, f)
	if err != nil {
		return kernel.Decimal{}, fail(kernel.CodeReferenceInvalid, path, "SUP's content holds no valid decimal here")
	}
	return d, nil
}

// parseSchedule reads and checks c's price (CST Schedule.Check): a supported mode and tier scope,
// a positive basis quantity, tiers starting at 0 and contiguous, only the last one open-ended,
// exactly one for flat, prices not negative and a zero price explained.
func parseSchedule(c Condition, path string) (schedule, error) {
	switch c.PriceTierScope {
	case "", TierScopeSKU:
	default:
		return schedule{}, fail(CodeUnsupportedPricingMode, path, "Tiers over an explicit group are not supported")
	}
	switch c.PricingMode {
	case ModeFlat, ModeAllUnits, ModeGraduated:
	case "":
		return schedule{}, fail(CodePriceTierAmbiguous, path, "The tiers do not say whether they price all units or each band")
	default:
		return schedule{}, fail(CodeUnsupportedPricingMode, path, fmt.Sprintf("Pricing mode %q is not supported", c.PricingMode))
	}
	basis, err := decimal(c.PriceBasisQuantity, kernel.QuantityFormat, path)
	if err != nil {
		return schedule{}, err
	}
	if basis.Sign() <= 0 {
		return schedule{}, fail(kernel.CodeInvalidKnownValue, path, "The price basis quantity is positive")
	}
	n := len(c.Tiers)
	if n == 0 || (c.PricingMode == ModeFlat && n != 1) {
		return schedule{}, fail(CodePriceTierAmbiguous, path, "A flat price has exactly one tier; others at least one")
	}
	s := schedule{mode: c.PricingMode, basis: basis}
	for i, t := range c.Tiers {
		lower, err := decimal(t.Lower, kernel.QuantityFormat, path)
		if err != nil {
			return schedule{}, err
		}
		price, err := decimal(t.UnitPrice, kernel.PriceFormat, path)
		if err != nil {
			return schedule{}, err
		}
		x := tier{lower: lower, price: price, raw: t}
		if t.Upper != "" {
			if x.upper, err = decimal(t.Upper, kernel.QuantityFormat, path); err != nil {
				return schedule{}, err
			}
			x.bounded = true
		}
		switch {
		case i == 0 && !lower.IsZero():
			return schedule{}, fail(CodePriceTierAmbiguous, path, "The first tier starts at 0")
		case i > 0 && !s.tiers[i-1].bounded:
			return schedule{}, fail(CodePriceTierAmbiguous, path, "Only the last tier is open-ended")
		case i > 0 && !s.tiers[i-1].upper.Equal(lower):
			return schedule{}, fail(CodePriceTierAmbiguous, path, "Tiers leave a gap or overlap")
		case x.bounded && x.upper.Cmp(lower) <= 0:
			return schedule{}, fail(CodePriceTierAmbiguous, path, "A tier ends after it starts")
		case price.IsZero() && t.ZeroPriceReason == "":
			return schedule{}, fail(kernel.CodeReferenceInvalid, path, "A zero price is explained")
		}
		s.tiers = append(s.tiers, x)
	}
	return s, nil
}

// amount prices quantity q exactly, before tax and rounding (CST Schedule.Amount): flat and
// all_units price every unit at the tier that contains q; graduated prices each band's units at its
// own tier. It returns the tiers applied with the quantity each priced (Python _applied).
func (s schedule) amount(q kernel.Decimal, path string) (kernel.Decimal, []AppliedTier, error) {
	last := s.tiers[len(s.tiers)-1]
	if last.bounded && q.Cmp(last.upper) >= 0 {
		return kernel.Decimal{}, nil, fail(CodePriceTierAmbiguous, path, "The quantity lies beyond the last tier")
	}
	per := func(units, price kernel.Decimal) kernel.Decimal {
		v, _ := units.Quo(s.basis) // the basis is positive
		return v.Mul(price)
	}
	if s.mode != ModeGraduated {
		for _, t := range s.tiers {
			if t.lower.Cmp(q) <= 0 && (!t.bounded || q.Cmp(t.upper) < 0) {
				return per(q, t.price), []AppliedTier{{Tier: t.raw, Quantity: q}}, nil
			}
		}
		return kernel.Decimal{}, nil, fail(CodePriceTierAmbiguous, path, "No tier contains the quantity")
	}
	total := zero
	var applied []AppliedTier
	for _, t := range s.tiers {
		hi := q
		if t.bounded && t.upper.Cmp(q) < 0 {
			hi = t.upper
		}
		if band := hi.Sub(t.lower); band.Sign() > 0 {
			total = total.Add(per(band, t.price))
			applied = append(applied, AppliedTier{Tier: t.raw, Quantity: band})
		}
	}
	return total, applied, nil
}

// keptPrice returns the tier a decrease-only change keeps the unit price of for the price group of
// condition line src priced by s (S5R-D41: 单价沿用当前修订): the one the revision in force applied, for
// every mode but graduated, whose bands keep their prices by themselves.
func (in Inputs) keptPrice(src Source, s schedule) (DocTier, bool) {
	if in.Change == nil || !in.Change.DecreaseOnly || s.mode == ModeGraduated {
		return DocTier{}, false
	}
	t, ok := in.Change.Prices[src]
	return t, ok
}

// at prices quantity q exactly at the unit price of tier pin of the schedule, whatever tier contains q
// (S5R-D41: a decrease keeps the unit price of the revision in force). A pin the schedule does not have
// is REFERENCE_INVALID: the condition line is the revision in force's own, so it has every tier.
func (s schedule) at(q kernel.Decimal, pin DocTier, path string) (kernel.Decimal, []AppliedTier, error) {
	for _, t := range s.tiers {
		if t.raw.Lower == pin.LowerInclusive && t.raw.Upper == pin.UpperExclusive && t.raw.UnitPrice == pin.UnitPrice {
			v, _ := q.Quo(s.basis) // the basis is positive
			return v.Mul(t.price), []AppliedTier{{Tier: t.raw, Quantity: q}}, nil
		}
	}
	return kernel.Decimal{}, nil, fail(kernel.CodeReferenceInvalid, path, "The revision in force priced under a tier the condition line does not have")
}

// KeptPrices returns, by condition line, the tier each price group of doc — the document of a
// commitment's revision in force — other than a graduated one applied: the unit prices a decrease-only
// change keeps (S5R-D41).
func KeptPrices(doc Document) map[Source]DocTier {
	out := map[Source]DocTier{}
	for _, g := range doc.PriceGroups {
		if g.PricingMode == ModeGraduated || len(g.AppliedTiers) != 1 {
			continue
		}
		src := Source{Ref: Ref{Kind: g.Source.Kind, ID: g.Source.ID, Revision: g.Source.Revision}, ConditionLineID: g.Source.ConditionLineID}
		out[src] = g.AppliedTiers[0]
	}
	return out
}

// taxSplit splits amount by the tax profile (Python split_tax, CST TaxTreatment.Cost without
// partial recovery, which POL's tax profiles do not have): an inclusive amount is rounded as the
// gross, an exclusive or not applicable one as the net, every rounding HALF_UP to the currency's
// minor units. The cost is the net when the tax is recoverable and the gross when it is not; an
// unknown treatment is TAX_TREATMENT_UNKNOWN and is never assumed.
func taxSplit(c kernel.Currency, amount kernel.Decimal, p TaxProfile, path string) (kernel.TaxSplit, kernel.Money, error) {
	switch p.Recovery {
	case Recoverable, Nonrecoverable:
	case RecoveryUnknown:
		return kernel.TaxSplit{}, kernel.Money{}, fail(CodeTaxTreatmentUnknown, path, "The tax recovery treatment is not verified; it is never assumed")
	default:
		return kernel.TaxSplit{}, kernel.Money{}, fail(CodeDependencyNotSupported, path, fmt.Sprintf("Recovery treatment %q is not supported", p.Recovery))
	}
	rate, err := kernel.ParseDecimal(p.Rate, kernel.FactorFormat)
	if err != nil || rate.Cmp(one) > 0 {
		return kernel.TaxSplit{}, kernel.Money{}, fail(kernel.CodeInvalidKnownValue, path, "A tax rate is between 0 and 1")
	}
	var split kernel.TaxSplit
	switch p.PriceBasis {
	case BasisInclusive:
		split, err = kernel.TaxInclusive(c, amount, rate)
	case BasisExclusive:
		split, err = kernel.TaxExclusive(c, amount, rate)
	case BasisNotApplicable:
		if !rate.IsZero() {
			return kernel.TaxSplit{}, kernel.Money{}, fail(kernel.CodeInvalidKnownValue, path, "Where no tax applies the rate is 0")
		}
		split, err = kernel.TaxExclusive(c, amount, rate)
	default:
		return kernel.TaxSplit{}, kernel.Money{}, fail(kernel.CodeReferenceInvalid, path, "Unknown tax price basis")
	}
	if err != nil {
		return kernel.TaxSplit{}, kernel.Money{}, fail(CodeCalculationLimitExceeded, path, "The taxed amount is out of range")
	}
	if p.Recovery == Recoverable {
		return split, split.Net, nil
	}
	return split, split.Gross, nil
}

// exactAt reports whether q has at most precision fractional digits.
func exactAt(q kernel.Decimal, precision int) bool {
	return q.RoundHalfUp(precision).Equal(q)
}

// compareDelivery compares the supplier's commitment with the order's requirement at instant now
// (PUR-INV-006, Python _delivery): only fixed dates are accepted in this stage; a lead time after an
// event is DELIVERY_UNRESOLVED (no estimate), a requirement that is not a fixed date or another
// milestone, place or time zone DELIVERY_NOT_COMPARABLE, a date after the order's DELIVERY_LATE and
// one before today in the order's time zone DELIVERY_DATE_PASSED. It returns the slack in days.
func compareDelivery(supply *SupplyDelivery, required RequiredDelivery, now time.Time, path string) (DocDelivery, error) {
	if supply == nil || supply.Kind != DeliveryFixed {
		return DocDelivery{}, fail(CodeDeliveryUnresolved, path, "The supplier's delivery is a lead time after an event; no date is estimated")
	}
	if supply.Milestone != required.Milestone || supply.Place != required.Place || supply.TimeZone != required.TimeZone {
		return DocDelivery{}, fail(CodeDeliveryNotComparable, path, "Delivery milestone, place or time zone differs from the order's")
	}
	supplied, err1 := time.Parse(time.DateOnly, supply.Date)
	deadline, err2 := time.Parse(time.DateOnly, required.Date)
	zone, err3 := time.LoadLocation(required.TimeZone)
	if err1 != nil || err2 != nil || err3 != nil {
		return DocDelivery{}, fail(kernel.CodeReferenceInvalid, path, "A delivery date or time zone does not parse")
	}
	local := now.In(zone)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	switch {
	case supplied.Before(today):
		return DocDelivery{}, fail(CodeDeliveryDatePassed, path, "The supplier's date is before today in the order's time zone")
	case supplied.After(deadline):
		return DocDelivery{}, fail(CodeDeliveryLate, path, "The supplier's date is after the order's required date")
	}
	return DocDelivery{Milestone: supply.Milestone, Place: supply.Place, TimeZone: supply.TimeZone, SupplierDate: supply.Date,
		RequiredDate: required.Date, SlackDays: int(deadline.Sub(supplied).Hours() / 24)}, nil
}

// changedDelivery compares, for a commitment change (Change), the supplier's date the change states
// (stated; "" for the supply condition's) with the order's requirement: the supply's milestone,
// place and time zone are the order's (DELIVERY_UNRESOLVED, DELIVERY_NOT_COMPARABLE as for a draft),
// but a date after the order's or already passed is recorded, not refused — SlackDays negative, and
// ConditionDate the condition's date when the stated one differs — for the change's decision to
// acknowledge.
func changedDelivery(supply *SupplyDelivery, stated string, required RequiredDelivery, path string) (DocDelivery, error) {
	if supply == nil || supply.Kind != DeliveryFixed {
		return DocDelivery{}, fail(CodeDeliveryUnresolved, path, "The supplier's delivery is a lead time after an event; no date is estimated")
	}
	if supply.Milestone != required.Milestone || supply.Place != required.Place || supply.TimeZone != required.TimeZone {
		return DocDelivery{}, fail(CodeDeliveryNotComparable, path, "Delivery milestone, place or time zone differs from the order's")
	}
	date := supply.Date
	if stated != "" {
		date = stated
	}
	supplied, err1 := time.Parse(time.DateOnly, date)
	deadline, err2 := time.Parse(time.DateOnly, required.Date)
	if err1 != nil || err2 != nil {
		return DocDelivery{}, fail(kernel.CodeReferenceInvalid, path, "A delivery date does not parse")
	}
	d := DocDelivery{Milestone: supply.Milestone, Place: supply.Place, TimeZone: supply.TimeZone, SupplierDate: date,
		RequiredDate: required.Date, SlackDays: int(deadline.Sub(supplied).Hours() / 24)}
	if date != supply.Date {
		d.ConditionDate = supply.Date
	}
	return d, nil
}
