package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes TRD declares for QuoteRoot (error catalog V0.1, rows whose contexts include TRD), with the
// category the catalog gives them (409 conflict, 422 invalid). The codes the server calculation
// decides are in calc_codes.go; QuoteRoot's freeze answers with some of those too
// (COMPARISON_BASIS_MISMATCH, BASE_QUANTITY_MISMATCH, DEPENDENCY_NOT_SUPPORTED,
// CHARGE_SCOPE_UNRESOLVED, FX_SCOPE_MISMATCH). TRD also answers with the shared kernel codes
// (SCHEMA_INVALID, NOT_FOUND, VERSION_CONFLICT, INVALID_STATE, DRAFT_NOT_READY, REFERENCE_INVALID,
// DUPLICATE_LINE_ID, INVALID_KNOWN_VALUE, PERMISSION_DENIED and the rest) and passes on its
// upstreams' verdicts: POL's CONFIG_NOT_ACTIVE and CONFIG_EXPIRED, CST's COST_SNAPSHOT_INELIGIBLE,
// and REFERENCE_STALE (409) of INQ, PTY and CAT.
const (
	CodeObjectControlConflict = "OBJECT_CONTROL_CONFLICT"    // 409: the root is bound to another task or the write lacks the current control
	CodeControlEpochStale     = "CONTROL_EPOCH_STALE"        // 409: the write claims an earlier control epoch
	CodeQuoteExpired          = "QUOTE_EXPIRED"              // as_of ≥ expires_at
	CodeQuoteValidityInvalid  = "QUOTE_VALIDITY_INVALID"     // the validity exceeds the pricing policy's
	CodeCostSnapshotMissing   = "COST_SNAPSHOT_MISSING"      // no cost snapshot bound
	CodeBusinessHashMismatch  = "BUSINESS_HASH_MISMATCH"     // a stored commercial document does not hash to its business hash, or a stated hash is not the revision's
	CodeCustomerReferenceMiss = "CUSTOMER_REFERENCE_MISSING" // no customer bound
	CodeContactReferenceMiss  = "CONTACT_REFERENCE_MISSING"  // no contact bound
	CodeReferenceStale        = "REFERENCE_STALE"            // 409: a referenced version or state no longer applies

	// Review and approval (WBS S3-03, S3-14; review.go).
	CodeBusinessKeyConflict          = "BUSINESS_KEY_CONFLICT"           // 409: the review request or approval ID is taken in the quote
	CodeQuoteActive                  = "QUOTE_ACTIVE"                    // 409: the root has an active revision (target quote.activate)
	CodeQuoteNotCurrent              = "QUOTE_NOT_CURRENT"               // 409: the active revision is not the one a replacement names
	CodeNoActiveRevision             = "NO_ACTIVE_REVISION"              // the root has no active revision to replace
	CodeApprovalRequired             = "APPROVAL_REQUIRED"               // no approval of the revision
	CodeApprovalRevoked              = "APPROVAL_REVOKED"                // the approval was revoked
	CodeApprovalExpired              = "APPROVAL_EXPIRED"                // the approval lapsed, or would outlive the quote
	CodeApprovalStale                = "APPROVAL_STALE"                  // 409: the approval's rules or its reviewer's qualification changed
	CodeApprovalReferenceMismatch    = "APPROVAL_REFERENCE_MISMATCH"     // the approval or exception names another revision, target or margin fact
	CodeIndependentReviewRequired    = "INDEPENDENT_REVIEW_REQUIRED"     // the reviewer contributed to, froze or submitted the revision
	CodeMarginExceptionNotAuthorized = "MARGIN_EXCEPTION_NOT_AUTHORIZED" // 403: no capability, or not independent of the revision's authors
)

var codes = map[string]kernel.Category{
	CodeObjectControlConflict: kernel.CategoryConflict,
	CodeControlEpochStale:     kernel.CategoryConflict,
	CodeQuoteExpired:          kernel.CategoryInvalid,
	CodeQuoteValidityInvalid:  kernel.CategoryInvalid,
	CodeCostSnapshotMissing:   kernel.CategoryInvalid,
	CodeBusinessHashMismatch:  kernel.CategoryInvalid,
	CodeCustomerReferenceMiss: kernel.CategoryInvalid,
	CodeContactReferenceMiss:  kernel.CategoryInvalid,
	CodeReferenceStale:        kernel.CategoryConflict,

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
}

// Category returns the category of a code TRD declares: QuoteRoot's, the calculation's or the
// formal document's.
func Category(code string) (kernel.Category, bool) {
	if c, ok := codes[code]; ok {
		return c, true
	}
	if c, ok := CalcCategory(code); ok {
		return c, true
	}
	return DocumentCategory(code)
}

// Reject returns a rejection with a code TRD declares (QuoteRoot's, the calculation's or the
// formal document's), at path of the caller's request. It panics on a code TRD does not declare.
func Reject(code, path string) *kernel.Rejection {
	c, ok := Category(code)
	if !ok {
		panic("trd: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, "")
}
