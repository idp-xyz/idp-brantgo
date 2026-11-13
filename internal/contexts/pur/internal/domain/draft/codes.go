package draft

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes of the purchase draft rules (error catalog V0.1, the rows whose contexts include PUR, and
// CST's pricing codes the restated rules give), with the catalog's categories (422 invalid, 409
// conflict, 404 not found). The api package declares the same codes for callers; a test keeps the
// two tables equal.
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
	CodeUnitScopeMismatch          = "UNIT_SCOPE_MISMATCH"          // a purchase, price, tier or MOQ unit is not the order line's (PUR-INV-012, S5R-D41)
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
	CodePurchaseAllocationExceeded = "PURCHASE_ALLOCATION_EXCEEDED" // 409: see package allocation
	CodePurchaseDraftStale         = "PURCHASE_DRAFT_STALE"         // 409: recomputed now, the draft differs (A47 #3)
)

// Codes maps this package's codes to their categories.
var Codes = map[string]kernel.Category{
	CodeOrderNotActive:             kernel.CategoryInvalid,
	CodeOrderApprovalRequired:      kernel.CategoryInvalid,
	CodeOrderApprovalRevoked:       kernel.CategoryInvalid,
	CodeOrderLineNotFound:          kernel.CategoryNotFound,
	CodeReferenceStale:             kernel.CategoryConflict,
	CodeSupplierMismatch:           kernel.CategoryInvalid,
	CodePurchaseTermsInconsistent:  kernel.CategoryInvalid,
	CodeSupplyScopeMismatch:        kernel.CategoryInvalid,
	CodeSpecificationMismatch:      kernel.CategoryInvalid,
	CodeUnitScopeMismatch:          kernel.CategoryInvalid,
	CodeDeliveryNotComparable:      kernel.CategoryInvalid,
	CodeDeliveryLate:               kernel.CategoryInvalid,
	CodeDeliveryDatePassed:         kernel.CategoryInvalid,
	CodeDeliveryUnresolved:         kernel.CategoryInvalid,
	CodeMOQNotMet:                  kernel.CategoryInvalid,
	CodeOrderMultipleInvalid:       kernel.CategoryInvalid,
	CodeOverbuyNotApproved:         kernel.CategoryInvalid,
	CodePriceTierAmbiguous:         kernel.CategoryInvalid,
	CodeUnsupportedPricingMode:     kernel.CategoryInvalid,
	CodeDependencyNotSupported:     kernel.CategoryInvalid,
	CodeTaxTreatmentUnknown:        kernel.CategoryInvalid,
	CodeCalculationLimitExceeded:   kernel.CategoryInvalid,
	CodeCostSnapshotMissing:        kernel.CategoryInvalid,
	CodePurchaseDraftImmutable:     kernel.CategoryInvalid,
	CodePurchaseDraftCommitted:     kernel.CategoryConflict,
	CodePurchaseAllocationExceeded: kernel.CategoryConflict,
	CodePurchaseDraftStale:         kernel.CategoryConflict,
}

// fail returns a rejection with a code of this package or a shared kernel code.
func fail(code, path, reason string) *kernel.Rejection {
	if c, ok := Codes[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}
