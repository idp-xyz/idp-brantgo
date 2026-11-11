package document

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes of the error catalog rows (contexts TRD) this package decides. Package api declares the
// formal document codes in document_codes.go and the others in codes.go and calc_codes.go, with
// the same categories. The package also answers with the shared kernel codes REFERENCE_INVALID,
// INVALID_KNOWN_VALUE and DRAFT_NOT_READY.
const (
	// DEPENDENCY_NOT_SUPPORTED: a template the renderer has no layout for (renderer version,
	// language).
	CodeDependencyNotSupported = "DEPENDENCY_NOT_SUPPORTED"
	// CALCULATION_LIMIT_EXCEEDED: a document beyond its size budget.
	CodeCalculationLimitExceeded = "CALCULATION_LIMIT_EXCEEDED"
	// BUSINESS_HASH_MISMATCH: a stored commercial document that is not the business-hashed one.
	CodeBusinessHashMismatch = "BUSINESS_HASH_MISMATCH"
	// REFERENCE_STALE (409): a revision read for the document is not the one the freeze pinned.
	CodeReferenceStale = "REFERENCE_STALE"
	// FORMAL_DOCUMENT_MISSING: the bound document has no bytes.
	CodeFormalDocumentMissing = "FORMAL_DOCUMENT_MISSING"
	// FORMAL_DOCUMENT_INTEGRITY_FAILURE: the stored bytes are not the bound ones.
	CodeFormalDocumentIntegrityFailure = "FORMAL_DOCUMENT_INTEGRITY_FAILURE"
	// DOCUMENT_BINDING_MISSING: the revision has no document binding of its own.
	CodeDocumentBindingMissing = "DOCUMENT_BINDING_MISSING"
)

// Categories maps the codes above to their error catalog category: REFERENCE_STALE is 409, the
// rest 422.
var Categories = map[string]kernel.Category{
	CodeDependencyNotSupported:         kernel.CategoryInvalid,
	CodeCalculationLimitExceeded:       kernel.CategoryInvalid,
	CodeBusinessHashMismatch:           kernel.CategoryInvalid,
	CodeReferenceStale:                 kernel.CategoryConflict,
	CodeFormalDocumentMissing:          kernel.CategoryInvalid,
	CodeFormalDocumentIntegrityFailure: kernel.CategoryInvalid,
	CodeDocumentBindingMissing:         kernel.CategoryInvalid,
}

// fail returns a rejection with code at path: one of the codes above or a shared kernel code.
func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}
