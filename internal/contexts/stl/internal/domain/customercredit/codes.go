package customercredit

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes the customer credit rules give, with their categories (error catalog V0.1, STL rows); the
// api declares exactly these.
const (
	CodeCreditReservationRequired   = "CREDIT_RESERVATION_REQUIRED"
	CodeCreditLimitExceeded         = "CREDIT_LIMIT_EXCEEDED"
	CodeCreditOverdueExceeded       = "CREDIT_OVERDUE_EXCEEDED"
	CodeCreditDecisionNotAuthorized = "CREDIT_DECISION_NOT_AUTHORIZED"
	CodeFXScopeMismatch             = "FX_SCOPE_MISMATCH"
	CodeApprovalExpired             = "APPROVAL_EXPIRED"
	CodeOrderNotActive              = "ORDER_NOT_ACTIVE"
	CodeReferenceStale              = "REFERENCE_STALE"
)

// Codes maps each code to its category.
var Codes = map[string]kernel.Category{
	CodeCreditReservationRequired:   kernel.CategoryConflict,
	CodeCreditLimitExceeded:         kernel.CategoryConflict,
	CodeCreditOverdueExceeded:       kernel.CategoryConflict,
	CodeCreditDecisionNotAuthorized: kernel.CategoryForbidden,
	CodeFXScopeMismatch:             kernel.CategoryInvalid,
	CodeApprovalExpired:             kernel.CategoryInvalid,
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

func state(path, reason string) error { return kernel.Reject(kernel.CodeInvalidState, path, reason) }
