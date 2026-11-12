package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes PUR declares (error catalog V0.1, the rows whose contexts include PUR, and CST's pricing
// codes the restated rules give), with the catalog's categories (422 invalid, 409 conflict, 404 not
// found). PUR also answers with the shared kernel codes (SCHEMA_INVALID, INVALID_KNOWN_VALUE,
// REFERENCE_INVALID, DUPLICATE_LINE_ID, INVALID_STATE, VERSION_CONFLICT, NOT_FOUND and the rest) and
// passes on its upstreams' verdicts: SUP's SUPPLY_CONDITION_NOT_CURRENT and SUPPLY_CONDITION_EXPIRED,
// POL's CONFIG_NOT_ACTIVE and CONFIG_EXPIRED, CAT's and PTY's REFERENCE_STALE, and TRD's order
// verdicts below.
const (
	CodeOrderNotActive             = "ORDER_NOT_ACTIVE"             // the order is not active
	CodeOrderApprovalRequired      = "ORDER_APPROVAL_REQUIRED"      // the order was not activated on an independent order approval
	CodeOrderApprovalRevoked       = "ORDER_APPROVAL_REVOKED"       // the approval that activated the order was revoked
	CodeOrderLineNotFound          = "ORDER_LINE_NOT_FOUND"         // 404: the order has no such line
	CodeReferenceStale             = "REFERENCE_STALE"              // 409: a bound version no longer applies
	CodeSupplierMismatch           = "SUPPLIER_MISMATCH"            // the draft mixes suppliers
	CodePurchaseTermsInconsistent  = "PURCHASE_TERMS_INCONSISTENT"  // currency, payment or trade terms differ within the draft
	CodeSupplyScopeMismatch        = "SUPPLY_SCOPE_MISMATCH"        // the offer does not answer the order's RFQ line
	CodeSpecificationMismatch      = "SPECIFICATION_MISMATCH"       // the condition line's specification revision is not the order line's
	CodeDeliveryNotComparable      = "DELIVERY_NOT_COMPARABLE"      // milestone, place or time zone differ, or the order's is not a fixed date
	CodeDeliveryLate               = "DELIVERY_LATE"                // the supplier's date is after the order's
	CodeDeliveryDatePassed         = "DELIVERY_DATE_PASSED"         // the supplier's date is before today, locally
	CodeDeliveryUnresolved         = "DELIVERY_UNRESOLVED"          // a lead time after an event that has not happened
	CodeMOQNotMet                  = "MOQ_NOT_MET"                  // the draft buys less than the minimum order quantity
	CodeOrderMultipleInvalid       = "ORDER_MULTIPLE_INVALID"       // the draft buys no multiple of the order multiple
	CodeOverbuyNotApproved         = "OVERBUY_NOT_APPROVED"         // an overbuy without a reason or an approval
	CodePriceTierAmbiguous         = "PRICE_TIER_AMBIGUOUS"         // tiers that do not price the quantity
	CodeUnsupportedPricingMode     = "UNSUPPORTED_PRICING_MODE"     // a pricing mode or tier scope not implemented
	CodeDependencyNotSupported     = "DEPENDENCY_NOT_SUPPORTED"     // a unit conversion or tax treatment not delivered
	CodeTaxTreatmentUnknown        = "TAX_TREATMENT_UNKNOWN"        // the tax recovery treatment is not verified
	CodeCalculationLimitExceeded   = "CALCULATION_LIMIT_EXCEEDED"   // an amount beyond the money range
	CodeCostSnapshotMissing        = "COST_SNAPSHOT_MISSING"        // the order binds no cost snapshot to compare with
	CodePurchaseDraftImmutable     = "PURCHASE_DRAFT_IMMUTABLE"     // the draft ID holds another request or another preparer's draft
	CodePurchaseDraftCommitted     = "PURCHASE_DRAFT_COMMITTED"     // 409: a committed draft is not voided
	CodePurchaseAllocationExceeded = "PURCHASE_ALLOCATION_EXCEEDED" // 409: the line's effective allocations would exceed its demand

	// Purchase commitments (S5-01; PUR-INV-009, 010).
	CodePurchaseDraftStale              = "PURCHASE_DRAFT_STALE"              // 409: recomputed now, the draft differs field by field (A47 #3)
	CodeIndependentReviewRequired       = "INDEPENDENT_REVIEW_REQUIRED"       // the decider prepared the draft or requested the commitment
	CodePurchaseDeviationUnacknowledged = "PURCHASE_DEVIATION_UNACKNOWLEDGED" // a deviation of the draft is not acknowledged with a reason
	CodePurchaseCommitmentNotApproved   = "PURCHASE_COMMITMENT_NOT_APPROVED"  // 409: no live, unrevoked approval of a qualified decider
	CodeApprovalExpired                 = "APPROVAL_EXPIRED"                  // the approval's validity has ended
	CodeSupplyConditionExpired          = "SUPPLY_CONDITION_EXPIRED"          // the draft's supply validity has ended
)

var codes = map[string]kernel.Category{
	CodeOrderNotActive:                  kernel.CategoryInvalid,
	CodeOrderApprovalRequired:           kernel.CategoryInvalid,
	CodeOrderApprovalRevoked:            kernel.CategoryInvalid,
	CodeOrderLineNotFound:               kernel.CategoryNotFound,
	CodeReferenceStale:                  kernel.CategoryConflict,
	CodeSupplierMismatch:                kernel.CategoryInvalid,
	CodePurchaseTermsInconsistent:       kernel.CategoryInvalid,
	CodeSupplyScopeMismatch:             kernel.CategoryInvalid,
	CodeSpecificationMismatch:           kernel.CategoryInvalid,
	CodeDeliveryNotComparable:           kernel.CategoryInvalid,
	CodeDeliveryLate:                    kernel.CategoryInvalid,
	CodeDeliveryDatePassed:              kernel.CategoryInvalid,
	CodeDeliveryUnresolved:              kernel.CategoryInvalid,
	CodeMOQNotMet:                       kernel.CategoryInvalid,
	CodeOrderMultipleInvalid:            kernel.CategoryInvalid,
	CodeOverbuyNotApproved:              kernel.CategoryInvalid,
	CodePriceTierAmbiguous:              kernel.CategoryInvalid,
	CodeUnsupportedPricingMode:          kernel.CategoryInvalid,
	CodeDependencyNotSupported:          kernel.CategoryInvalid,
	CodeTaxTreatmentUnknown:             kernel.CategoryInvalid,
	CodeCalculationLimitExceeded:        kernel.CategoryInvalid,
	CodeCostSnapshotMissing:             kernel.CategoryInvalid,
	CodePurchaseDraftImmutable:          kernel.CategoryInvalid,
	CodePurchaseDraftCommitted:          kernel.CategoryConflict,
	CodePurchaseAllocationExceeded:      kernel.CategoryConflict,
	CodePurchaseDraftStale:              kernel.CategoryConflict,
	CodeIndependentReviewRequired:       kernel.CategoryInvalid,
	CodePurchaseDeviationUnacknowledged: kernel.CategoryInvalid,
	CodePurchaseCommitmentNotApproved:   kernel.CategoryConflict,
	CodeApprovalExpired:                 kernel.CategoryInvalid,
	CodeSupplyConditionExpired:          kernel.CategoryInvalid,
}

// Category returns the category of a code PUR declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Codes returns the codes PUR declares with their categories.
func Codes() map[string]kernel.Category {
	out := make(map[string]kernel.Category, len(codes))
	for k, v := range codes {
		out[k] = v
	}
	return out
}

// Reject returns a rejection with a code PUR declares, at path. It panics on a code PUR does not
// declare.
func Reject(code, path string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("pur: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, "")
}
