package rules

import (
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the CST error catalog rows the calculation rules decide. Package api declares the same
// codes with the same categories for callers; a test of the application layer compares them.
const (
	CodeCalculationLimitExceeded = "CALCULATION_LIMIT_EXCEEDED"
	CodeDependencyNotSupported   = "DEPENDENCY_NOT_SUPPORTED"
	CodeUnsupportedPricingMode   = "UNSUPPORTED_PRICING_MODE"
	CodeFXNotApplicable          = "FX_NOT_APPLICABLE"
	CodeTaxTreatmentUnknown      = "TAX_TREATMENT_UNKNOWN"
	CodeTaxRecoveryRateInvalid   = "TAX_RECOVERY_RATE_INVALID"
	CodeCostInputNotReady        = "COST_INPUT_NOT_READY"
	CodeComparisonBasisMismatch  = "COMPARISON_BASIS_MISMATCH"
	CodeMOQNotMet                = "MOQ_NOT_MET"
	CodeOrderMultipleInvalid     = "ORDER_MULTIPLE_INVALID"
	CodePriceTierAmbiguous       = "PRICE_TIER_AMBIGUOUS"
	CodeChargeDoubleCount        = "CHARGE_DOUBLE_COUNT"
	CodeChargeScopeUnresolved    = "CHARGE_SCOPE_UNRESOLVED"
	CodeAllocationInvalid        = "ALLOCATION_INVALID"
	CodeMeasurementConflict      = "MEASUREMENT_CONFLICT"
)

// Categories maps the codes above to their error catalog category; all are 422.
var Categories = map[string]kernel.Category{
	CodeCalculationLimitExceeded: kernel.CategoryInvalid,
	CodeDependencyNotSupported:   kernel.CategoryInvalid,
	CodeUnsupportedPricingMode:   kernel.CategoryInvalid,
	CodeFXNotApplicable:          kernel.CategoryInvalid,
	CodeTaxTreatmentUnknown:      kernel.CategoryInvalid,
	CodeTaxRecoveryRateInvalid:   kernel.CategoryInvalid,
	CodeCostInputNotReady:        kernel.CategoryInvalid,
	CodeComparisonBasisMismatch:  kernel.CategoryInvalid,
	CodeMOQNotMet:                kernel.CategoryInvalid,
	CodeOrderMultipleInvalid:     kernel.CategoryInvalid,
	CodePriceTierAmbiguous:       kernel.CategoryInvalid,
	CodeChargeDoubleCount:        kernel.CategoryInvalid,
	CodeChargeScopeUnresolved:    kernel.CategoryInvalid,
	CodeAllocationInvalid:        kernel.CategoryInvalid,
	CodeMeasurementConflict:      kernel.CategoryInvalid,
}

// fail returns a rejection with code at path and, for a refusal that concerns several fields, at
// the further paths with the same reason. Codes are CST's own or the shared kernel codes.
func fail(code, path, reason string, more ...string) error {
	make1 := func(p string) *kernel.Rejection {
		if c, ok := Categories[code]; ok {
			return kernel.RejectAs(code, c, p, reason)
		}
		return kernel.Reject(code, p, reason)
	}
	r := make1(path)
	for _, p := range more {
		r.Fields = append(r.Fields, make1(p).Fields[0])
	}
	return r
}

func index(path, list string, i int) string { return fmt.Sprintf("%s/%s/%d", path, list, i) }

// Difference is a finding that keeps a result from being comparable without refusing the
// calculation: the result stays analyzable and is reported not_comparable with these paths.
type Difference struct {
	Path   string
	Reason string
}
