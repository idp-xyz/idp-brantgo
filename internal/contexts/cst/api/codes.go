package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes CST declares (error catalog V0.1, the rows whose contexts include CST), with the category
// the catalog gives them (422 invalid, 409 conflict). CST also answers with the shared kernel codes
// (SCHEMA_INVALID, INVALID_KNOWN_VALUE, REFERENCE_INVALID, DUPLICATE_LINE_ID, INVALID_STATE and the
// rest) and passes on POL's CONFIG_NOT_ACTIVE and CONFIG_EXPIRED for a configuration not in force.
const (
	CodeCalculationLimitExceeded = "CALCULATION_LIMIT_EXCEEDED" // input or arithmetic beyond the resource budget
	CodeDependencyNotSupported   = "DEPENDENCY_NOT_SUPPORTED"   // a dependency path not delivered yet
	CodeUnsupportedPricingMode   = "UNSUPPORTED_PRICING_MODE"   // a pricing mode not implemented
	CodeFXNotApplicable          = "FX_NOT_APPLICABLE"          // exchange rate direction, purpose or validity does not fit
	CodeTaxTreatmentUnknown      = "TAX_TREATMENT_UNKNOWN"      // the tax cost treatment is not verified
	CodeTaxRecoveryRateInvalid   = "TAX_RECOVERY_RATE_INVALID"  // partial recovery without a rate, or a rate above the tax rate
	CodeCostInputNotReady        = "COST_INPUT_NOT_READY"       // a required input or its evidence is missing
	CodeComparisonBasisMismatch  = "COMPARISON_BASIS_MISMATCH"  // specification, responsibility, delivery or tax cannot be unified
	CodeMOQNotMet                = "MOQ_NOT_MET"                // purchase quantity below a verified minimum order quantity
	CodeOrderMultipleInvalid     = "ORDER_MULTIPLE_INVALID"     // purchase quantity not a multiple of the order multiple
	CodePriceTierAmbiguous       = "PRICE_TIER_AMBIGUOUS"       // tiers overlap, leave a gap or have no determined meaning
	CodeChargeDoubleCount        = "CHARGE_DOUBLE_COUNT"        // the same charge obligation counted twice
	CodeChargeScopeUnresolved    = "CHARGE_SCOPE_UNRESOLVED"    // what a charge covers cannot be determined
	CodeAllocationInvalid        = "ALLOCATION_INVALID"         // the allocation basis does not hold
	CodeMeasurementConflict      = "MEASUREMENT_CONFLICT"       // measurement data contradict each other
	CodeEvidenceHashMismatch     = "EVIDENCE_HASH_MISMATCH"     // output evidence hash differs
	CodeReferenceStale           = "REFERENCE_STALE"            // 409: a referenced version or state no longer applies
	// COST_SNAPSHOT_INELIGIBLE is the catalog's TRD row "快照当前不可采纳"; CST declares it for the
	// AdoptabilityGuard's verdict a downstream context passes on.
	CodeCostSnapshotIneligible = "COST_SNAPSHOT_INELIGIBLE"
)

var codes = map[string]kernel.Category{
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
	CodeEvidenceHashMismatch:     kernel.CategoryInvalid,
	CodeReferenceStale:           kernel.CategoryConflict,
	CodeCostSnapshotIneligible:   kernel.CategoryInvalid,
}

// Category returns the category of a code CST declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Reject returns a rejection with a code CST declares, for callers that pass a CST verdict on at a
// path of their own request. It panics on a code CST does not declare.
func Reject(code, path string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("cst: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, "")
}
