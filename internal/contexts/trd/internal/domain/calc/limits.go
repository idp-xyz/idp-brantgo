package calc

import (
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Formats and bounds of T03 §5.1 and §10. A value with more fractional digits than its format is
// SCHEMA_INVALID (it cannot have been parsed with kernel.ParseDecimal in that format); a value of
// more than 12 integer digits, more lines or charges than their bound and an amount of 10^24 minor
// units or more (kernel.Money refuses it) are CALCULATION_LIMIT_EXCEEDED. Payment stages and the
// evidence of a charge beyond their bound are PAYMENT_TOTAL_MISMATCH and CHARGE_SCOPE_UNRESOLVED,
// as in Python.
const (
	QuantityScale = 6  // quantities
	PriceScale    = 8  // unit prices, price basis quantities, charge amounts, discount ratios
	FactorScale   = 12 // tax rates, exchange rates, conversion factors, the minimum margin
	PercentScale  = 6  // payment stage percentages (POL PercentFormat)

	MaxLines    = 200 // quote lines, as RFQ lines
	MaxCharges  = 100 // sales charges of one quote
	MaxStages   = 20  // payment stages of one payment terms revision
	MaxEvidence = 200 // evidence of one sales charge
)

var (
	one          = kernel.DecimalFromInt(1)
	hundred      = kernel.DecimalFromInt(100)
	integerLimit = kernel.DecimalFromInt(1_000_000_000_000) // exclusive: at most 12 integer digits
)

// fits reports whether d has at most scale fractional digits.
func fits(d kernel.Decimal, scale int) bool { return d.RoundHalfUp(scale).Equal(d) }

// positive admits a positive contract decimal of scale fractional digits below 10^12, at path.
func positive(d kernel.Decimal, scale int, path, what string) error {
	switch {
	case !fits(d, scale):
		return fail(kernel.CodeSchemaInvalid, path, fmt.Sprintf("%s has at most %d fractional digits", what, scale))
	case d.Sign() <= 0:
		return fail(kernel.CodeInvalidKnownValue, path, what+" is positive")
	case d.Cmp(integerLimit) >= 0:
		return fail(CodeCalculationLimitExceeded, path, what+" has at most 12 integer digits")
	}
	return nil
}

// zero returns no money of c, a currency the caller checked is not the zero Currency.
func zero(c kernel.Currency) kernel.Money {
	m, _ := kernel.NewMoney(c, 0)
	return m
}

// limited is the rejection of an amount the kernel refuses as out of range.
func limited(path, what string) error {
	return fail(CodeCalculationLimitExceeded, path, what+" is 10^24 minor units or more")
}
