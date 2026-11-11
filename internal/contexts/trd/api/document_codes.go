package api

// The codes of TRD's formal quote documents (WBS S3-04, TRD-INV-009; package
// internal/domain/document decides them).

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes the formal document declares: the error catalog V0.1 rows whose contexts include TRD and
// that a stored formal document and its binding decide, with the category the catalog gives them
// (all 422, invalid). Rendering and planning a formal document also answer with
// DEPENDENCY_NOT_SUPPORTED and CALCULATION_LIMIT_EXCEEDED (calc_codes.go), BUSINESS_HASH_MISMATCH
// and REFERENCE_STALE (codes.go), and the shared kernel codes REFERENCE_INVALID,
// INVALID_KNOWN_VALUE and DRAFT_NOT_READY.
const (
	CodeFormalDocumentMissing          = "FORMAL_DOCUMENT_MISSING"           // the bound formal document has no bytes
	CodeFormalDocumentIntegrityFailure = "FORMAL_DOCUMENT_INTEGRITY_FAILURE" // the stored bytes are not the bound ones
	CodeDocumentBindingMissing         = "DOCUMENT_BINDING_MISSING"          // the revision has no document binding of its own
)

var documentCodes = map[string]kernel.Category{
	CodeFormalDocumentMissing:          kernel.CategoryInvalid,
	CodeFormalDocumentIntegrityFailure: kernel.CategoryInvalid,
	CodeDocumentBindingMissing:         kernel.CategoryInvalid,
}

// DocumentCategory returns the category of a code the formal document declares.
func DocumentCategory(code string) (kernel.Category, bool) {
	c, ok := documentCodes[code]
	return c, ok
}

// RejectDocument returns a rejection with a code the formal document declares, at path of the
// caller's request. It panics on a code the formal document does not declare.
func RejectDocument(code, path string) *kernel.Rejection {
	c, ok := documentCodes[code]
	if !ok {
		panic("trd: undeclared formal document code " + code)
	}
	return kernel.RejectAs(code, c, path, "")
}
