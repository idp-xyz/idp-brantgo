package calc

import (
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the error catalog rows the calculation decides. Package api declares the same codes
// with the same categories (calc_codes.go); a test of the application layer compares them.
const (
	CodeCalculationLimitExceeded     = "CALCULATION_LIMIT_EXCEEDED"
	CodeDependencyNotSupported       = "DEPENDENCY_NOT_SUPPORTED"
	CodeComparisonBasisMismatch      = "COMPARISON_BASIS_MISMATCH"
	CodeBaseQuantityMismatch         = "BASE_QUANTITY_MISMATCH"
	CodeCostSnapshotNotEligible      = "COST_SNAPSHOT_NOT_ELIGIBLE"
	CodeChargeDoubleCount            = "CHARGE_DOUBLE_COUNT"
	CodeChargeScopeUnresolved        = "CHARGE_SCOPE_UNRESOLVED"
	CodeTaxPolicyUnconfirmed         = "TAX_POLICY_UNCONFIRMED"
	CodeFXScopeMismatch              = "FX_SCOPE_MISMATCH"
	CodeFXStale                      = "FX_STALE"
	CodePaymentTotalMismatch         = "PAYMENT_TOTAL_MISMATCH"
	CodeMarginPolicyRequiresApproval = "MARGIN_POLICY_REQUIRES_APPROVAL"
)

// Categories maps the codes above to their error catalog category; all are 422.
var Categories = map[string]kernel.Category{
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

// fail returns a rejection with code at path; codes are the calculation's own or the shared kernel
// codes.
func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

func index(path, list string, i int) string { return fmt.Sprintf("%s/%s/%d", path, list, i) }
