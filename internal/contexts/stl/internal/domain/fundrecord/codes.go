package fundrecord

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes the fund, application and order account rules give, with their categories (error catalog
// V0.1, the rows whose contexts include STL, and BUSINESS_KEY_CONFLICT for a bank reference recorded
// twice); the api declares these besides customer credit's.
const (
	CodeApplicationExceedsAvailable = "APPLICATION_EXCEEDS_AVAILABLE"
	CodeApplicationExceedsOpen      = "APPLICATION_EXCEEDS_OPEN"
	CodeFundDifferenceUnexplained   = "FUND_DIFFERENCE_UNEXPLAINED"
	CodePartyEntityMismatch         = "PARTY_ENTITY_MISMATCH"
	CodeIndependentReviewRequired   = "INDEPENDENT_REVIEW_REQUIRED"
	CodeBusinessKeyConflict         = "BUSINESS_KEY_CONFLICT"
	CodeCommercialReleaseBlocked    = "COMMERCIAL_RELEASE_BLOCKED"
	CodeFXScopeMismatch             = "FX_SCOPE_MISMATCH"
	CodeOrderNotActive              = "ORDER_NOT_ACTIVE"
	CodeReferenceStale              = "REFERENCE_STALE"
)

// Codes maps each code to its category.
var Codes = map[string]kernel.Category{
	CodeApplicationExceedsAvailable: kernel.CategoryConflict,
	CodeApplicationExceedsOpen:      kernel.CategoryConflict,
	CodeFundDifferenceUnexplained:   kernel.CategoryConflict,
	CodePartyEntityMismatch:         kernel.CategoryConflict,
	CodeIndependentReviewRequired:   kernel.CategoryInvalid,
	CodeBusinessKeyConflict:         kernel.CategoryConflict,
	CodeCommercialReleaseBlocked:    kernel.CategoryConflict,
	CodeFXScopeMismatch:             kernel.CategoryInvalid,
	CodeOrderNotActive:              kernel.CategoryInvalid,
	CodeReferenceStale:              kernel.CategoryConflict,
}

// Reject returns a rejection with one of Codes at path.
func Reject(code, path, reason string) *kernel.Rejection {
	return kernel.RejectAs(code, Codes[code], path, reason)
}

func invalid(path, reason string) error {
	return kernel.Reject(kernel.CodeInvalidKnownValue, path, reason)
}

func reference(path, reason string) error {
	return kernel.Reject(kernel.CodeReferenceInvalid, path, reason)
}

func state(path, reason string) error { return kernel.Reject(kernel.CodeInvalidState, path, reason) }
