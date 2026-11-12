package shipmentdocumentset

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes of the document set's rules besides the shared kernel codes, with the error catalog's
// categories. Package api declares the same; the application's tests keep them equal.
const (
	// CodeShipmentDocumentsUnreconciled (409): a difference is neither resolved nor acknowledged, or
	// a required document is missing (FUL-INV-004, BT12).
	CodeShipmentDocumentsUnreconciled = "SHIPMENT_DOCUMENTS_UNRECONCILED"
	// CodeDocumentIntegrityFailure (422): stored bytes are not the ones bound.
	CodeDocumentIntegrityFailure = "DOCUMENT_INTEGRITY_FAILURE"
	// CodeReferenceStale (409): a POL revision moved on from the content hash a version was
	// rendered from, or a PTY service role no longer serves.
	CodeReferenceStale = "REFERENCE_STALE"
	// CodeDependencyNotSupported: a template of another renderer version.
	CodeDependencyNotSupported = "DEPENDENCY_NOT_SUPPORTED"
	// CodeBusinessKeyConflict (409): the set ID or the shipment's set is taken.
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
	// CodeInsuranceCoverageInsufficient (422): the insured amount of a CIF or CIP policy is below the
	// contract percentage of the invoice value, or in another currency than the invoice (FUL-INV-005;
	// S5-16).
	CodeInsuranceCoverageInsufficient = "INSURANCE_COVERAGE_INSUFFICIENT"
)

// Codes maps every code of the set's rules to its category.
var Codes = map[string]kernel.Category{
	CodeShipmentDocumentsUnreconciled: kernel.CategoryConflict,
	CodeDocumentIntegrityFailure:      kernel.CategoryInvalid,
	CodeReferenceStale:                kernel.CategoryConflict,
	CodeDependencyNotSupported:        kernel.CategoryInvalid,
	CodeBusinessKeyConflict:           kernel.CategoryConflict,
	CodeInsuranceCoverageInsufficient: kernel.CategoryInvalid,
	CodeCertificateNotApplicable:      kernel.CategoryConflict, // S5 C5, certificate.go
}

// Reject returns a rejection with a code of the set's rules or a shared kernel code, at path.
func Reject(code, path, reason string) *kernel.Rejection {
	if c, ok := Codes[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}
