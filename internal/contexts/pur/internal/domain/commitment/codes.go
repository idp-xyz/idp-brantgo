package commitment

import (
	"strconv"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the purchase commitment rules (error catalog V0.1, the PUR rows S5 A0 registered, and the
// shared rows whose contexts include PUR), with the catalog's categories. The api package declares
// the same codes for callers; a test keeps the tables equal.
const (
	CodeIndependentReviewRequired       = "INDEPENDENT_REVIEW_REQUIRED"       // the decider prepared the draft or requested the commitment
	CodePurchaseDeviationUnacknowledged = "PURCHASE_DEVIATION_UNACKNOWLEDGED" // a deviation of the draft is not acknowledged with a reason
	CodePurchaseCommitmentNotApproved   = "PURCHASE_COMMITMENT_NOT_APPROVED"  // 409: no live, unrevoked approval of a qualified decider
	CodeApprovalExpired                 = "APPROVAL_EXPIRED"                  // the approval's validity has ended
	CodeOverbuyNotApproved              = "OVERBUY_NOT_APPROVED"              // an overbuy the decision did not approve
	CodePurchaseDraftCommitted          = "PURCHASE_DRAFT_COMMITTED"          // 409: the draft has an issued commitment
	CodeReferenceStale                  = "REFERENCE_STALE"                   // 409: a stated hash is not the bound one
	CodeSupplyConditionExpired          = "SUPPLY_CONDITION_EXPIRED"          // the draft's supply validity has ended
	CodePurchaseCommitmentChangeStale   = "PURCHASE_COMMITMENT_CHANGE_STALE"  // 409: the commitment revision a change is bound to moved
	CodeSupplierDeliveryExceeded        = "SUPPLIER_DELIVERY_EXCEEDED"        // 409: a revision would buy less of a line than was verified delivered
)

// Codes maps this package's codes to their categories.
var Codes = map[string]kernel.Category{
	CodeIndependentReviewRequired:       kernel.CategoryInvalid,
	CodePurchaseDeviationUnacknowledged: kernel.CategoryInvalid,
	CodePurchaseCommitmentNotApproved:   kernel.CategoryConflict,
	CodeApprovalExpired:                 kernel.CategoryInvalid,
	CodeOverbuyNotApproved:              kernel.CategoryInvalid,
	CodePurchaseDraftCommitted:          kernel.CategoryConflict,
	CodeReferenceStale:                  kernel.CategoryConflict,
	CodeSupplyConditionExpired:          kernel.CategoryInvalid,
	CodePurchaseCommitmentChangeStale:   kernel.CategoryConflict,
	CodeSupplierDeliveryExceeded:        kernel.CategoryConflict,
}

// fail returns a rejection with a code of this package or a shared kernel code.
func fail(code, path, reason string) *kernel.Rejection {
	if c, ok := Codes[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
