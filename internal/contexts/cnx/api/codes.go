package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes CNX declares (error catalog V0.1, the rows whose contexts include CNX that the trade
// compliance check gives, with the catalog's categories: 409 conflict, 422 invalid). CNX also
// answers with the shared kernel codes (SCHEMA_INVALID, INVALID_KNOWN_VALUE, REFERENCE_INVALID,
// INVALID_STATE, VERSION_CONFLICT, NOT_FOUND, PERMISSION_DENIED and the rest) and passes on POL's
// CONFIG_NOT_ACTIVE and CONFIG_EXPIRED for the regulation revision.
const (
	// TradeComplianceGuard v1's refusal: no valid clear conclusion covers the order revision.
	CodeTradeComplianceNotClear = "TRADE_COMPLIANCE_NOT_CLEAR"
	// The order is neither a draft nor active: it ended.
	CodeOrderNotActive = "ORDER_NOT_ACTIVE"
	// A line has no declaration profile an authorized professional verified (CAT-INV-002).
	CodeDeclarationProfileUnverified = "DECLARATION_PROFILE_UNVERIFIED"
	// A bound version no longer applies: the order's revision, the current evaluation.
	CodeReferenceStale = "REFERENCE_STALE"
)

var codes = map[string]kernel.Category{
	CodeTradeComplianceNotClear:      kernel.CategoryConflict,
	CodeOrderNotActive:               kernel.CategoryInvalid,
	CodeDeclarationProfileUnverified: kernel.CategoryInvalid,
	CodeReferenceStale:               kernel.CategoryConflict,
}

// Category returns the category of a code CNX declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Codes returns the codes CNX declares with their categories.
func Codes() map[string]kernel.Category {
	out := make(map[string]kernel.Category, len(codes))
	for k, v := range codes {
		out[k] = v
	}
	return out
}

// Reject returns a rejection with a code CNX declares, at path. It panics on a code CNX does not
// declare.
func Reject(code, path, reason string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("cnx: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, reason)
}
