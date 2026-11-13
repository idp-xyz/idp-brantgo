package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes STL declares (error catalog V0.1, the rows whose contexts include STL that customer credit
// uses), with the catalog's categories (409 conflict, 403 forbidden, 422 invalid). STL also answers
// with the shared kernel codes (SCHEMA_INVALID, INVALID_KNOWN_VALUE, INVALID_STATE,
// REFERENCE_INVALID, VERSION_CONFLICT, NOT_FOUND and the rest) and passes on POL's verdicts on the
// configurations it names (CONFIG_NOT_ACTIVE, CONFIG_EXPIRED, POL's FX codes).
//
// CREDIT_LIMIT_EXCEEDED and CREDIT_OVERDUE_EXCEEDED are the reasons a credit decision opens with;
// credit.reserve commits the decision and never refuses with them (STL-INV-004).
const (
	CodeCreditReservationRequired   = "CREDIT_RESERVATION_REQUIRED"    // 409: no live reservation covers the order's credit portion
	CodeCreditLimitExceeded         = "CREDIT_LIMIT_EXCEEDED"          // 409: exposure and portion exceed the limit; a decision is needed
	CodeCreditOverdueExceeded       = "CREDIT_OVERDUE_EXCEEDED"        // 409: the earliest overdue receivable exceeds the tolerance
	CodeCreditDecisionNotAuthorized = "CREDIT_DECISION_NOT_AUTHORIZED" // 403: tier too low, the requester, or a sales clerk
	CodeFXScopeMismatch             = "FX_SCOPE_MISMATCH"              // the amount is in another currency than the policy's, without a credit rate
	CodeApprovalExpired             = "APPROVAL_EXPIRED"               // the decision's validity ended
	CodeOrderNotActive              = "ORDER_NOT_ACTIVE"               // the order is neither a draft nor active
	CodeReferenceStale              = "REFERENCE_STALE"                // 409: a bound revision or hash no longer applies

	// Funds, applications and order accounts (B2a).
	CodeApplicationExceedsAvailable = "APPLICATION_EXCEEDS_AVAILABLE" // 409: more than the fund record has available
	CodeApplicationExceedsOpen      = "APPLICATION_EXCEEDS_OPEN"      // 409: more than is open on the target
	CodeFundDifferenceUnexplained   = "FUND_DIFFERENCE_UNEXPLAINED"   // 409: only the unexplained difference would cover it
	CodePartyEntityMismatch         = "PARTY_ENTITY_MISMATCH"         // 409: payer or receiving entity is not the business party
	CodeIndependentReviewRequired   = "INDEPENDENT_REVIEW_REQUIRED"   // 422: the recorder resolves its own difference
	CodeBusinessKeyConflict         = "BUSINESS_KEY_CONFLICT"         // 409: the bank reference is recorded on the account
	CodeCommercialReleaseBlocked    = "COMMERCIAL_RELEASE_BLOCKED"    // 409: the payment condition of the scope is not met

	// Settlement statements (B2b).
	CodeSettlementSourceDuplicate = "SETTLEMENT_SOURCE_DUPLICATE" // 409: the source line is settled by a standing statement

	// Credit notes and settlement reversal (C8).
	CodeSettlementHasApplications = "SETTLEMENT_HAS_APPLICATIONS" // 409: something stands applied on the statement (unapply first)

	// Charges and order results (C9).
	CodeAllocationInvalid     = "ALLOCATION_INVALID"       // 422: a weight negative, or the weights sum to zero (STL-INV-010)
	CodeOrderResultNotSettled = "ORDER_RESULT_NOT_SETTLED" // 409: a close condition does not hold (STL-INV-011)
)

var codes = map[string]kernel.Category{
	CodeCreditReservationRequired:   kernel.CategoryConflict,
	CodeCreditLimitExceeded:         kernel.CategoryConflict,
	CodeCreditOverdueExceeded:       kernel.CategoryConflict,
	CodeCreditDecisionNotAuthorized: kernel.CategoryForbidden,
	CodeFXScopeMismatch:             kernel.CategoryInvalid,
	CodeApprovalExpired:             kernel.CategoryInvalid,
	CodeOrderNotActive:              kernel.CategoryInvalid,
	CodeReferenceStale:              kernel.CategoryConflict,
	CodeApplicationExceedsAvailable: kernel.CategoryConflict,
	CodeApplicationExceedsOpen:      kernel.CategoryConflict,
	CodeFundDifferenceUnexplained:   kernel.CategoryConflict,
	CodePartyEntityMismatch:         kernel.CategoryConflict,
	CodeIndependentReviewRequired:   kernel.CategoryInvalid,
	CodeBusinessKeyConflict:         kernel.CategoryConflict,
	CodeCommercialReleaseBlocked:    kernel.CategoryConflict,
	CodeSettlementSourceDuplicate:   kernel.CategoryConflict,
	CodeSettlementHasApplications:   kernel.CategoryConflict,
	CodeAllocationInvalid:           kernel.CategoryInvalid,
	CodeOrderResultNotSettled:       kernel.CategoryConflict,
	CodeOpeningBalanceDuplicate:     kernel.CategoryConflict, // C10 (opening.go)
}

// Category returns the category of a code STL declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Codes returns the codes STL declares with their categories.
func Codes() map[string]kernel.Category {
	out := make(map[string]kernel.Category, len(codes))
	for k, v := range codes {
		out[k] = v
	}
	return out
}

// Reject returns a rejection with a code STL declares, at path, with reason. It panics on a code
// STL does not declare.
func Reject(code, path, reason string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("stl: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, reason)
}
