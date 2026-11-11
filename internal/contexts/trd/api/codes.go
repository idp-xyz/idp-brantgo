package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes TRD declares for QuoteRoot (error catalog V0.1, rows whose contexts include TRD), with the
// category the catalog gives them (409 conflict, 422 invalid). TRD also answers with the shared
// kernel codes (SCHEMA_INVALID, NOT_FOUND, VERSION_CONFLICT, INVALID_STATE, DRAFT_NOT_READY,
// REFERENCE_INVALID, DUPLICATE_LINE_ID, INVALID_KNOWN_VALUE, PERMISSION_DENIED and the rest) and
// passes on its upstreams' verdicts: POL's CONFIG_NOT_ACTIVE and CONFIG_EXPIRED, CST's
// COST_SNAPSHOT_INELIGIBLE, and REFERENCE_STALE (409) of INQ, PTY and CAT.
const (
	CodeObjectControlConflict   = "OBJECT_CONTROL_CONFLICT"    // 409: the root is bound to another task or the write lacks the current control
	CodeControlEpochStale       = "CONTROL_EPOCH_STALE"        // 409: the write claims an earlier control epoch
	CodeQuoteExpired            = "QUOTE_EXPIRED"              // as_of ≥ expires_at
	CodeQuoteValidityInvalid    = "QUOTE_VALIDITY_INVALID"     // the validity exceeds the pricing policy's
	CodeCostSnapshotMissing     = "COST_SNAPSHOT_MISSING"      // no cost snapshot bound
	CodeComparisonBasisMismatch = "COMPARISON_BASIS_MISMATCH"  // lines, terms or snapshot do not fit together
	CodeBaseQuantityMismatch    = "BASE_QUANTITY_MISMATCH"     // the quantity in the RFQ's unit is not the RFQ line's
	CodeBusinessHashMismatch    = "BUSINESS_HASH_MISMATCH"     // a stored commercial document does not hash to its business hash
	CodeCustomerReferenceMiss   = "CUSTOMER_REFERENCE_MISSING" // no customer bound
	CodeContactReferenceMiss    = "CONTACT_REFERENCE_MISSING"  // no contact bound
	CodeDependencyNotSupported  = "DEPENDENCY_NOT_SUPPORTED"   // a dependency path not delivered yet
	CodeChargeScopeUnresolved   = "CHARGE_SCOPE_UNRESOLVED"    // a charge obligation counted twice
	CodeFXScopeMismatch         = "FX_SCOPE_MISMATCH"          // the exchange rate's direction or purpose does not fit
	CodeReferenceStale          = "REFERENCE_STALE"            // 409: a referenced version or state no longer applies
)

var codes = map[string]kernel.Category{
	CodeObjectControlConflict:   kernel.CategoryConflict,
	CodeControlEpochStale:       kernel.CategoryConflict,
	CodeQuoteExpired:            kernel.CategoryInvalid,
	CodeQuoteValidityInvalid:    kernel.CategoryInvalid,
	CodeCostSnapshotMissing:     kernel.CategoryInvalid,
	CodeComparisonBasisMismatch: kernel.CategoryInvalid,
	CodeBaseQuantityMismatch:    kernel.CategoryInvalid,
	CodeBusinessHashMismatch:    kernel.CategoryInvalid,
	CodeCustomerReferenceMiss:   kernel.CategoryInvalid,
	CodeContactReferenceMiss:    kernel.CategoryInvalid,
	CodeDependencyNotSupported:  kernel.CategoryInvalid,
	CodeChargeScopeUnresolved:   kernel.CategoryInvalid,
	CodeFXScopeMismatch:         kernel.CategoryInvalid,
	CodeReferenceStale:          kernel.CategoryConflict,
}

// Category returns the category of a code TRD declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}
