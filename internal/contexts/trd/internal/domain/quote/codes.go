// Package quote is the QuoteRoot aggregate of TRD (tactical model TRD-AGG-QuoteRoot; WBS S3-01,
// S3-03, S3-08, S3-14), identified by (tenant, company, quote_id). Its commercial content lives in
// revisions with monotonic numbers (SM-TRD-REVISION, the parts before formalization):
//
//	working → frozen              quote.freeze: a human; the draft complete, every reference in
//	                              effect and the server calculation (package calc) computed; the
//	                              revision gets issued_at, its commercial document and business hash
//	working → discarded           quote.discard_draft
//	frozen → pending_approval     quote.submit_review: a review request for quote.activate or
//	                              quote.replace (review.go)
//	pending_approval → frozen     quote.review.decide return or reject
//	pending_approval → active     quote.activate, quote.replace (the active one → superseded): the
//	                              formal document and commit evidence bound (formal.go)
//	frozen, pending → withdrawn   quote.withdraw
//	active → cancelled            quote.cancel
//
// One working revision is saved whole (quote.save_draft) or copied from a frozen one
// (quote.revise). A frozen revision's commercial document and business hash never change
// (TRD-INV-002); internal labels and notes live on the root and change its version only
// (quote.metadata.update). At most one revision of a root is active (TRD-INV-001); a risk hold
// pauses its forward use, and the conversion slot, once taken, keeps it (TRD-INV-010).
//
// A review request (SM-TRD-REVIEW) is decided by an independent human; an approval
// (SM-TRD-APPROVAL) binds the revision, its hash, its target and the rule versions, and is valid
// until revoked; a margin below the pricing policy's minimum needs an explicit margin exception
// besides it (TRD-INV-003, 004, 005, 007; review.go).
//
// The root carries its object control (SM-TRD-CONTROL, TRD-INV-015): a task binds the root
// (object_control.bind), a human stops it (object_control.stop) or releases it
// (object_control.release); each change increments the control epoch, and while the root is bound
// every write must claim the binding's task and current epoch.
//
// Upstream facts (PTY, INQ, CAT, POL, CST, EVD) are read before TRD's transaction and judged here
// as Facts; the freeze re-checks them inside its transaction through the upstream guards.
package quote

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes of the error catalog rows this package decides (contexts TRD); package api declares the
// same with their categories. The calculation's codes are package calc's.
const (
	CodeObjectControlConflict   = "OBJECT_CONTROL_CONFLICT"
	CodeControlEpochStale       = "CONTROL_EPOCH_STALE"
	CodeQuoteExpired            = "QUOTE_EXPIRED"
	CodeQuoteValidityInvalid    = "QUOTE_VALIDITY_INVALID"
	CodeCostSnapshotMissing     = "COST_SNAPSHOT_MISSING"
	CodeComparisonBasisMismatch = "COMPARISON_BASIS_MISMATCH"
	CodeBusinessHashMismatch    = "BUSINESS_HASH_MISMATCH"
	CodeCustomerReference       = "CUSTOMER_REFERENCE_MISSING"
	CodeContactReference        = "CONTACT_REFERENCE_MISSING"
	CodeDependencyNotSupported  = "DEPENDENCY_NOT_SUPPORTED"
	CodeReferenceStale          = "REFERENCE_STALE"

	CodeBusinessKeyConflict          = "BUSINESS_KEY_CONFLICT"
	CodeQuoteActive                  = "QUOTE_ACTIVE"
	CodeQuoteNotCurrent              = "QUOTE_NOT_CURRENT"
	CodeNoActiveRevision             = "NO_ACTIVE_REVISION"
	CodeApprovalRequired             = "APPROVAL_REQUIRED"
	CodeApprovalRevoked              = "APPROVAL_REVOKED"
	CodeApprovalExpired              = "APPROVAL_EXPIRED"
	CodeApprovalStale                = "APPROVAL_STALE"
	CodeApprovalReferenceMismatch    = "APPROVAL_REFERENCE_MISMATCH"
	CodeIndependentReviewRequired    = "INDEPENDENT_REVIEW_REQUIRED"
	CodeMarginExceptionNotAuthorized = "MARGIN_EXCEPTION_NOT_AUTHORIZED"
	CodeMarginPolicyRequiresApproval = "MARGIN_POLICY_REQUIRES_APPROVAL"

	CodeRootConversionLocked   = "ROOT_CONVERSION_LOCKED"
	CodeConversionSlotConflict = "CONVERSION_SLOT_CONFLICT"
	CodeFXStale                = "FX_STALE"

	// CodeAcceptanceAlreadyExists refuses ending a revision a live acceptance accepts (the owner's
	// decision of 2026-11-11; formal.go).
	CodeAcceptanceAlreadyExists = "ACCEPTANCE_ALREADY_EXISTS"
)

// Categories maps the codes above to their error catalog category.
var Categories = map[string]kernel.Category{
	CodeObjectControlConflict:   kernel.CategoryConflict,
	CodeControlEpochStale:       kernel.CategoryConflict,
	CodeQuoteExpired:            kernel.CategoryInvalid,
	CodeQuoteValidityInvalid:    kernel.CategoryInvalid,
	CodeCostSnapshotMissing:     kernel.CategoryInvalid,
	CodeComparisonBasisMismatch: kernel.CategoryInvalid,
	CodeBusinessHashMismatch:    kernel.CategoryInvalid,
	CodeCustomerReference:       kernel.CategoryInvalid,
	CodeContactReference:        kernel.CategoryInvalid,
	CodeDependencyNotSupported:  kernel.CategoryInvalid,
	CodeReferenceStale:          kernel.CategoryConflict,

	CodeBusinessKeyConflict:          kernel.CategoryConflict,
	CodeQuoteActive:                  kernel.CategoryConflict,
	CodeQuoteNotCurrent:              kernel.CategoryConflict,
	CodeNoActiveRevision:             kernel.CategoryInvalid,
	CodeApprovalRequired:             kernel.CategoryInvalid,
	CodeApprovalRevoked:              kernel.CategoryInvalid,
	CodeApprovalExpired:              kernel.CategoryInvalid,
	CodeApprovalStale:                kernel.CategoryConflict,
	CodeApprovalReferenceMismatch:    kernel.CategoryInvalid,
	CodeIndependentReviewRequired:    kernel.CategoryInvalid,
	CodeMarginExceptionNotAuthorized: kernel.CategoryForbidden,
	CodeMarginPolicyRequiresApproval: kernel.CategoryInvalid,

	CodeRootConversionLocked:   kernel.CategoryConflict,
	CodeConversionSlotConflict: kernel.CategoryConflict,
	CodeFXStale:                kernel.CategoryInvalid,

	CodeAcceptanceAlreadyExists: kernel.CategoryConflict,
}

func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}
