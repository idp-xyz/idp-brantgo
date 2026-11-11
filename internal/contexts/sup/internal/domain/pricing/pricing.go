// Package pricing holds the verify-time rules of a supply condition's prices and charges: the
// price tiers of SUP-INV-002, the units, quantities and MOQ of a condition line, and the tariff,
// chargeable basis and allocation of a logistics charge (T02 §5, §6, §13.1).
//
// CST evaluates the same conditions with its calculation rules (cst/internal/domain/rules), which
// SUP cannot import: CST is downstream of SUP. So that verification and evaluation never disagree,
// this package states the verify-time checks of those rules with the same semantics, the same order,
// the same codes and the same paths — Schedule.Check, Condition.Check, CheckConditions,
// ChargeableRule.Check, Billing.Check, AllocationRule.Check and CheckFees mirror the functions of the
// same names in CST — and its tests replay CST's test vectors for them (pricing_test.go). A change
// to either side changes both and their vectors. What only a scenario can decide (MOQ against the
// bought quantity, conversion into the demand unit, inclusions across documents, allocation bases)
// stays with CST.
//
// Every input a rejection can point at carries a Path, the JSON Pointer of that element in the
// stored document ("/args/data/lines/0"); a rejection's path is the Path followed by the contract
// field ("/args/data/lines/0/tiers/1/upper_exclusive").
package pricing

import (
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the error catalog rows the rules decide; package api declares the same codes.
const (
	CodeCalculationLimitExceeded = "CALCULATION_LIMIT_EXCEEDED"
	CodeUnsupportedPricingMode   = "UNSUPPORTED_PRICING_MODE"
	CodeCostInputNotReady        = "COST_INPUT_NOT_READY"
	CodePriceTierAmbiguous       = "PRICE_TIER_AMBIGUOUS"
	CodeChargeDoubleCount        = "CHARGE_DOUBLE_COUNT"
	CodeAllocationInvalid        = "ALLOCATION_INVALID"
)

// Categories maps the codes above to their error catalog category; all are 422.
var Categories = map[string]kernel.Category{
	CodeCalculationLimitExceeded: kernel.CategoryInvalid,
	CodeUnsupportedPricingMode:   kernel.CategoryInvalid,
	CodeCostInputNotReady:        kernel.CategoryInvalid,
	CodePriceTierAmbiguous:       kernel.CategoryInvalid,
	CodeChargeDoubleCount:        kernel.CategoryInvalid,
	CodeAllocationInvalid:        kernel.CategoryInvalid,
}

// Formats and bounds of T02 §3 and §13.6, as CST's rules state them.
const (
	QuantityScale = 6  // quantities, tier bounds, price basis quantities
	PriceScale    = 8  // unit prices, rates, amounts, minimum charges
	FactorScale   = 12 // divisors

	MaxTiers          = 50  // tiers of one condition line or tariff
	MaxComponents     = 500 // charges of one offer
	MaxExplicitWeight = 1_000_000_000
)

var (
	maxQuantity = kernel.DecimalFromInt(1_000_000)
	maxAmount   = kernel.DecimalFromInt(1_000_000_000_000)
)

// Fail returns a rejection with code at path; codes are this package's or the shared kernel codes.
func Fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

func index(path, list string, i int) string { return fmt.Sprintf("%s/%s/%d", path, list, i) }

// fits reports whether d has at most scale fractional digits.
func fits(d kernel.Decimal, scale int) bool { return d.RoundHalfUp(scale).Equal(d) }

// CheckQuantity admits a positive quantity of the quantity format, not above 10^6.
func CheckQuantity(d kernel.Decimal, path string) error {
	switch {
	case !fits(d, QuantityScale):
		return Fail(kernel.CodeSchemaInvalid, path, "A quantity has at most 6 fractional digits")
	case d.Sign() <= 0:
		return Fail(kernel.CodeInvalidKnownValue, path, "A quantity is positive")
	case d.Cmp(maxQuantity) > 0:
		return Fail(CodeCalculationLimitExceeded, path, "A quantity is at most 1000000")
	}
	return nil
}

// CheckPrecision admits a quantity of a unit with precision fractional digits; a fractional piece
// is refused, never rounded.
func CheckPrecision(d kernel.Decimal, precision int, path string) error {
	if precision < 0 || !fits(d, min(QuantityScale, precision)) {
		return Fail(kernel.CodeInvalidKnownValue, path, "The quantity has more fractional digits than its unit allows")
	}
	return nil
}

// checkAmount admits a non-negative unit price or amount of the price format, not above 10^12.
func checkAmount(d kernel.Decimal, path string) error {
	switch {
	case !fits(d, PriceScale):
		return Fail(kernel.CodeSchemaInvalid, path, "A price or amount has at most 8 fractional digits")
	case d.Sign() < 0:
		return Fail(kernel.CodeInvalidKnownValue, path, "Prices and charges are not negative; discounts are not charges")
	case d.Cmp(maxAmount) > 0:
		return Fail(CodeCalculationLimitExceeded, path, "A price or amount is at most 1000000000000")
	}
	return nil
}

// repeated returns the index of the first ID that repeats an earlier one, or -1.
func repeated(ids []string) int {
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if seen[id] {
			return i
		}
		seen[id] = true
	}
	return -1
}

// Unique refuses a repeated ID in ids (DUPLICATE_LINE_ID at path/i).
func Unique(ids []string, path string) error {
	if i := repeated(ids); i >= 0 {
		return Fail(kernel.CodeDuplicateLineID, fmt.Sprintf("%s/%d", path, i), "")
	}
	return nil
}
