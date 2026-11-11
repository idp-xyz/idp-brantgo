package api

// The codes of TRD's server-side quotation calculation (WBS S3-02; package
// internal/domain/calc decides them).

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes the TRD calculation declares: the error catalog V0.1 rows whose contexts include TRD and
// that the cost scope, the sales terms, the amounts, the sales exchange rate, the margin rule and
// the payment plan decide, with the category the catalog gives them (all 422, invalid). The
// calculation also answers with the shared kernel codes (SCHEMA_INVALID, INVALID_KNOWN_VALUE,
// REFERENCE_INVALID, DUPLICATE_LINE_ID, DRAFT_NOT_READY).
const (
	CodeCalculationLimitExceeded = "CALCULATION_LIMIT_EXCEEDED" // input or arithmetic beyond the resource budget
	CodeDependencyNotSupported   = "DEPENDENCY_NOT_SUPPORTED"   // a dependency path not delivered yet, such as an unconverted unit
	CodeComparisonBasisMismatch  = "COMPARISON_BASIS_MISMATCH"  // the quote's scope is not the adopted cost snapshot's
	CodeBaseQuantityMismatch     = "BASE_QUANTITY_MISMATCH"     // the stated base quantity is not the converted quote quantity
	CodeCostSnapshotNotEligible  = "COST_SNAPSHOT_NOT_ELIGIBLE" // the snapshot can never price a quote, such as a what_if costing
	CodeChargeDoubleCount        = "CHARGE_DOUBLE_COUNT"        // the same charge obligation counted twice
	CodeChargeScopeUnresolved    = "CHARGE_SCOPE_UNRESOLVED"    // what a sales charge is or covers cannot be determined
	CodeTaxPolicyUnconfirmed     = "TAX_POLICY_UNCONFIRMED"     // no verified sales-side tax applicability for the subject
	CodeFXScopeMismatch          = "FX_SCOPE_MISMATCH"          // the exchange rate's direction or purpose does not fit, or it is missing
	CodeFXStale                  = "FX_STALE"                   // the exchange rate was observed too long before the calculation
	CodePaymentTotalMismatch     = "PAYMENT_TOTAL_MISMATCH"     // the payment stages do not sum to 100 or leave a stage at 0
	// MARGIN_POLICY_REQUIRES_APPROVAL is how a quote below the minimum margin is refused where it
	// needs an independent margin exception approval (TRD-INV-007); an ordinary approval never
	// clears it.
	CodeMarginPolicyRequiresApproval = "MARGIN_POLICY_REQUIRES_APPROVAL"
)

var calcCodes = map[string]kernel.Category{
	CodeCalculationLimitExceeded:     kernel.CategoryInvalid,
	CodeDependencyNotSupported:       kernel.CategoryInvalid,
	CodeComparisonBasisMismatch:      kernel.CategoryInvalid,
	CodeBaseQuantityMismatch:         kernel.CategoryInvalid,
	CodeCostSnapshotNotEligible:      kernel.CategoryInvalid,
	CodeChargeDoubleCount:            kernel.CategoryInvalid,
	CodeChargeScopeUnresolved:        kernel.CategoryInvalid,
	CodeTaxPolicyUnconfirmed:         kernel.CategoryInvalid,
	CodeFXScopeMismatch:              kernel.CategoryInvalid,
	CodeFXStale:                      kernel.CategoryInvalid,
	CodePaymentTotalMismatch:         kernel.CategoryInvalid,
	CodeMarginPolicyRequiresApproval: kernel.CategoryInvalid,
}

// CalcCategory returns the category of a code the TRD calculation declares.
func CalcCategory(code string) (kernel.Category, bool) {
	c, ok := calcCodes[code]
	return c, ok
}

// RejectCalc returns a rejection with a code the TRD calculation declares, at path of the caller's
// request. It panics on a code the calculation does not declare.
func RejectCalc(code, path string) *kernel.Rejection {
	c, ok := calcCodes[code]
	if !ok {
		panic("trd: undeclared calculation code " + code)
	}
	return kernel.RejectAs(code, c, path, "")
}
