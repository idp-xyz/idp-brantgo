// Package quote is the QuoteRoot aggregate of TRD (tactical model TRD-AGG-QuoteRoot; WBS S3-01 and
// S3-08), identified by (tenant, company, quote_id). Its commercial content lives in revisions
// with monotonic numbers (SM-TRD-REVISION, the S3-01 part):
//
//	working → frozen     quote.freeze: a human; the draft complete and every reference in effect;
//	                     the revision gets issued_at, its commercial document and business hash
//	working → discarded  quote.discard_draft
//
// One working revision is saved whole (quote.save_draft) or copied from a frozen one
// (quote.revise). A frozen revision's commercial document and business hash never change
// (TRD-INV-002); internal labels and notes live on the root and change its version only
// (quote.metadata.update). At most one revision of a root is active (TRD-INV-001): activation comes
// with the review wave; the storage admits one active revision per root.
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
// same with their categories.
const (
	CodeObjectControlConflict   = "OBJECT_CONTROL_CONFLICT"
	CodeControlEpochStale       = "CONTROL_EPOCH_STALE"
	CodeQuoteExpired            = "QUOTE_EXPIRED"
	CodeQuoteValidityInvalid    = "QUOTE_VALIDITY_INVALID"
	CodeCostSnapshotMissing     = "COST_SNAPSHOT_MISSING"
	CodeComparisonBasisMismatch = "COMPARISON_BASIS_MISMATCH"
	CodeBaseQuantityMismatch    = "BASE_QUANTITY_MISMATCH"
	CodeBusinessHashMismatch    = "BUSINESS_HASH_MISMATCH"
	CodeCustomerReference       = "CUSTOMER_REFERENCE_MISSING"
	CodeContactReference        = "CONTACT_REFERENCE_MISSING"
	CodeDependencyNotSupported  = "DEPENDENCY_NOT_SUPPORTED"
	CodeChargeScopeUnresolved   = "CHARGE_SCOPE_UNRESOLVED"
	CodeFXScopeMismatch         = "FX_SCOPE_MISMATCH"
	CodeReferenceStale          = "REFERENCE_STALE"
)

// Categories maps the codes above to their error catalog category.
var Categories = map[string]kernel.Category{
	CodeObjectControlConflict:   kernel.CategoryConflict,
	CodeControlEpochStale:       kernel.CategoryConflict,
	CodeQuoteExpired:            kernel.CategoryInvalid,
	CodeQuoteValidityInvalid:    kernel.CategoryInvalid,
	CodeCostSnapshotMissing:     kernel.CategoryInvalid,
	CodeComparisonBasisMismatch: kernel.CategoryInvalid,
	CodeBaseQuantityMismatch:    kernel.CategoryInvalid,
	CodeBusinessHashMismatch:    kernel.CategoryInvalid,
	CodeCustomerReference:       kernel.CategoryInvalid,
	CodeContactReference:        kernel.CategoryInvalid,
	CodeDependencyNotSupported:  kernel.CategoryInvalid,
	CodeChargeScopeUnresolved:   kernel.CategoryInvalid,
	CodeFXScopeMismatch:         kernel.CategoryInvalid,
	CodeReferenceStale:          kernel.CategoryConflict,
}

func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}
