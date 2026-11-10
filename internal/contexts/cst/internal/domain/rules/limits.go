package rules

import "go.idp.xyz/idp-brant/internal/kernel"

// Formats and bounds of T02 §3 and §13.6. A value beyond a bound is CALCULATION_LIMIT_EXCEEDED; a
// value with more fractional digits than its format is SCHEMA_INVALID (it cannot have been parsed
// with kernel.ParseDecimal in that format).
const (
	QuantityScale = 6  // quantities, measurements, tier bounds, price basis quantities
	PriceScale    = 8  // unit prices, rate amounts, fixed amounts, minimum charges
	FactorScale   = 12 // exchange rates, tax and recovery rates, conversion factors

	MaxDemandLines      = 200  // demand lines of a costing context
	MaxTiers            = 50   // tiers of one condition line or billing rule
	MaxCandidateSources = 200  // distinct supply condition revisions a scenario draws on
	MaxComponents       = 500  // purchase components and charges of one evaluation, excluded ones included
	MaxInclusionEdges   = 2000 // inclusion edges of the charge graph
	MaxInclusionDepth   = 8    // inclusion chain length
	MaxPackages         = 1000 // package lines of one measurement
	MaxExplicitWeight   = 1_000_000_000
	MaxLeadDays         = 3650
)

var (
	one             = kernel.DecimalFromInt(1)
	maxQuantity     = kernel.DecimalFromInt(1_000_000)
	maxAmount       = kernel.DecimalFromInt(1_000_000_000_000) // unit prices and amounts; component and scenario totals, major units
	maxFXRate       = kernel.DecimalFromInt(1000)
	maxDimension    = kernel.DecimalFromInt(1000)          // metres per axis
	maxMeasureTotal = kernel.DecimalFromInt(1_000_000_000) // kg or m³ of one measurement
)

// fits reports whether d has at most scale fractional digits.
func fits(d kernel.Decimal, scale int) bool { return d.RoundHalfUp(scale).Equal(d) }

// checkQuantity admits a positive quantity of the quantity format, not above 10^6.
func checkQuantity(d kernel.Decimal, path string) error {
	switch {
	case !fits(d, QuantityScale):
		return fail(kernel.CodeSchemaInvalid, path, "A quantity has at most 6 fractional digits")
	case d.Sign() <= 0:
		return fail(kernel.CodeInvalidKnownValue, path, "A quantity is positive")
	case d.Cmp(maxQuantity) > 0:
		return fail(CodeCalculationLimitExceeded, path, "A quantity is at most 1000000")
	}
	return nil
}

// checkMeasure admits a positive quantity to price: an aggregated purchase quantity or a chargeable
// quantity rounded to its increment, with at most 6 fractional digits. Its size is bounded by the
// inputs it was derived from, not by the 10^6 bound of a single quantity field.
func checkMeasure(d kernel.Decimal, path string) error {
	if d.Sign() <= 0 || !fits(d, QuantityScale) {
		return fail(kernel.CodeInvalidKnownValue, path, "A quantity to price is positive with at most 6 fractional digits")
	}
	return nil
}

// checkPrecision admits a quantity of a unit with precision fractional digits; a fractional piece
// is refused, never rounded.
func checkPrecision(d kernel.Decimal, precision int, path string) error {
	if precision < 0 || !fits(d, min(QuantityScale, precision)) {
		return fail(kernel.CodeInvalidKnownValue, path, "The quantity has more fractional digits than its unit allows")
	}
	return nil
}

// checkAmount admits a non-negative unit price or amount of the price format, not above 10^12.
func checkAmount(d kernel.Decimal, path string) error {
	switch {
	case !fits(d, PriceScale):
		return fail(kernel.CodeSchemaInvalid, path, "A price or amount has at most 8 fractional digits")
	case d.Sign() < 0:
		return fail(kernel.CodeInvalidKnownValue, path, "Prices and charges are not negative; discounts are not charges")
	case d.Cmp(maxAmount) > 0:
		return fail(CodeCalculationLimitExceeded, path, "A price or amount is at most 1000000000000")
	}
	return nil
}

// checkMoney admits a component or total amount: not negative and at most 10^12 major units.
func checkMoney(m kernel.Money, path string) error {
	switch {
	case m.Sign() < 0:
		return fail(kernel.CodeInvalidKnownValue, path, "Negative costs are not supported")
	case m.Decimal().Cmp(maxAmount) > 0:
		return fail(CodeCalculationLimitExceeded, path, "An amount is at most 1000000000000")
	}
	return nil
}

// Total sums component amounts of the calculation currency into the scenario total and checks the
// business bound of 10^12 major units (T02 §13.9); path is where the total is reported.
func Total(c kernel.Currency, path string, amounts ...kernel.Money) (kernel.Money, error) {
	for i, a := range amounts {
		if a.Currency() != c {
			return kernel.Money{}, fail(kernel.CodeInvalidKnownValue, index(path, "components", i), "A component is not in the calculation currency")
		}
		if err := checkMoney(a, index(path, "components", i)); err != nil {
			return kernel.Money{}, err
		}
	}
	total, err := kernel.Sum(c, amounts...)
	if err != nil {
		return kernel.Money{}, fail(CodeCalculationLimitExceeded, path, "The total is out of range")
	}
	if err := checkMoney(total, path); err != nil {
		return kernel.Money{}, err
	}
	return total, nil
}
