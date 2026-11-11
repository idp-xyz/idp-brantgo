package amendment

import (
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the error catalog rows (contexts TRD) this package decides, with orderrev's
// (QUANTITY_BELOW_SHIPPED, ORDER_NOT_ACTIVE and the calculation's) and the shared kernel codes.
// ORDER_REVIEW_MISMATCH, ORDER_REVIEW_IMMUTABLE, ORDER_APPROVAL_REQUIRED and REVIEWER_NOT_INDEPENDENT
// are the order review's codes (package order), applied to the amendment's approval.
const (
	CodeOrderAmendmentStale    = "ORDER_AMENDMENT_STALE"    // 409: the order is no longer at the revision the amendment binds
	CodeReviewerNotIndependent = "REVIEWER_NOT_INDEPENDENT" // the decider prepared or submitted the amendment
	CodeOrderReviewMismatch    = "ORDER_REVIEW_MISMATCH"    // the decision states another amendment hash
	CodeOrderReviewImmutable   = "ORDER_REVIEW_IMMUTABLE"   // the amendment was decided before
	CodeOrderApprovalRequired  = "ORDER_APPROVAL_REQUIRED"  // no approval in force: not approved, or its decider no longer qualified
	CodeOrderNotActive         = orderrev.CodeOrderNotActive
	CodeQuantityBelowShipped   = orderrev.CodeQuantityBelowShipped
)

// Categories maps the codes this package declares to their error catalog category.
var Categories = map[string]kernel.Category{
	CodeOrderAmendmentStale:    kernel.CategoryConflict,
	CodeReviewerNotIndependent: kernel.CategoryInvalid,
	CodeOrderReviewMismatch:    kernel.CategoryInvalid,
	CodeOrderReviewImmutable:   kernel.CategoryInvalid,
	CodeOrderApprovalRequired:  kernel.CategoryInvalid,
}

func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	if c, ok := orderrev.Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}
