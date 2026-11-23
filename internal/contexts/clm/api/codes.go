package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes CLM declares (error catalog rows whose contexts include CLM, and BUSINESS_KEY_CONFLICT
// which CLM gives for a taken claim, return or remedy ID). CLM also answers with the shared kernel
// codes (SCHEMA_INVALID, INVALID_KNOWN_VALUE, INVALID_STATE, REFERENCE_INVALID, VERSION_CONFLICT,
// NOT_FOUND, PERMISSION_DENIED).
//
// CLAIM_REMEDY_ALREADY_SETTLED is STL's, when a credit note or a refund cites an approved remedy
// that STL has already settled (R35, Wave 2). CLM does not settle.
const (
	// CodeClaimReturnExceedsShipped (422): the return would take the counted quantity of the
	// shipment line above the confirmed shipped quantity (CLM-INV-002).
	CodeClaimReturnExceedsShipped = "CLAIM_RETURN_EXCEEDS_SHIPPED"
	// CodeIndependentReviewRequired (422): the actor who opened the claim decides its remedy or
	// closes it (CLM-INV-003).
	CodeIndependentReviewRequired = "INDEPENDENT_REVIEW_REQUIRED"
	// CodeBusinessKeyConflict (409): the claim, return or remedy ID holds another request.
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
)

var codes = map[string]kernel.Category{
	CodeClaimReturnExceedsShipped: kernel.CategoryInvalid,
	CodeIndependentReviewRequired: kernel.CategoryInvalid,
	CodeBusinessKeyConflict:       kernel.CategoryConflict,
}

// Category returns the category of a code CLM declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Codes returns the codes CLM declares with their categories.
func Codes() map[string]kernel.Category {
	out := make(map[string]kernel.Category, len(codes))
	for k, v := range codes {
		out[k] = v
	}
	return out
}

// Reject returns a rejection with a code CLM declares, at path. It panics on a code CLM does not
// declare.
func Reject(code, path, reason string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("clm: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, reason)
}
