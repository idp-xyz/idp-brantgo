package orderdoc

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes of the error catalog rows (contexts TRD) this package decides, with the shared kernel codes
// REFERENCE_INVALID, INVALID_KNOWN_VALUE, INVALID_STATE, SCHEMA_INVALID and PERMISSION_DENIED.
const (
	CodeOrderDocumentStale       = "ORDER_DOCUMENT_STALE"       // 409: the document binds another order revision than the current one
	CodeDocumentBindingMissing   = "DOCUMENT_BINDING_MISSING"   // the order has no document of that kind bound to it
	CodeDocumentIntegrityFailure = "DOCUMENT_INTEGRITY_FAILURE" // the stored bytes are not the bound document
	CodeDependencyNotSupported   = "DEPENDENCY_NOT_SUPPORTED"   // a template the renderer has no layout for, or a section whose facts are not delivered
	CodeCalculationLimitExceeded = "CALCULATION_LIMIT_EXCEEDED" // a document beyond its size budget
	CodeBusinessKeyConflict      = "BUSINESS_KEY_CONFLICT"      // 409: the order revision has a document of that kind
)

// Categories maps the codes above to their error catalog category: ORDER_DOCUMENT_STALE and
// BUSINESS_KEY_CONFLICT are 409, the rest 422.
var Categories = map[string]kernel.Category{
	CodeOrderDocumentStale:       kernel.CategoryConflict,
	CodeDocumentBindingMissing:   kernel.CategoryInvalid,
	CodeDocumentIntegrityFailure: kernel.CategoryInvalid,
	CodeDependencyNotSupported:   kernel.CategoryInvalid,
	CodeCalculationLimitExceeded: kernel.CategoryInvalid,
	CodeBusinessKeyConflict:      kernel.CategoryConflict,
}

func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}
