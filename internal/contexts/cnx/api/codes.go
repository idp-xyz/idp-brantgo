package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes CNX declares (error catalog V0.1, the rows whose contexts include CNX that the trade
// compliance check, the export licences, the purchase invoices, the tax refund cases, the
// declarations and the document bundles give, with the catalog's categories: 409 conflict, 422
// invalid). CNX also answers with the shared kernel codes (SCHEMA_INVALID, INVALID_KNOWN_VALUE,
// REFERENCE_INVALID, INVALID_STATE, VERSION_CONFLICT, NOT_FOUND, PERMISSION_DENIED and the rest) and
// passes on POL's CONFIG_NOT_ACTIVE and CONFIG_EXPIRED for the regulation and currency revisions.
const (
	// TradeComplianceGuard v1's refusal: no valid clear conclusion covers the order revision.
	CodeTradeComplianceNotClear = "TRADE_COMPLIANCE_NOT_CLEAR"
	// The order is neither a draft nor active: it ended.
	CodeOrderNotActive = "ORDER_NOT_ACTIVE"
	// A line has no declaration profile an authorized professional verified (CAT-INV-002).
	CodeDeclarationProfileUnverified = "DECLARATION_PROFILE_UNVERIFIED"
	// A bound version no longer applies: the order's revision, the current evaluation.
	CodeReferenceStale = "REFERENCE_STALE"
	// ExportLicenseGuard v1's refusal: the goods need a licence and no allocation covers them.
	CodeExportLicenseRequired = "EXPORT_LICENSE_REQUIRED"
	// An allocation would take Σ allocated quantity or value above the licensed one (CNX-INV-002).
	CodeExportLicenseExceeded = "EXPORT_LICENSE_EXCEEDED"
	// The licence is outside its validity, or its destination, end user or goods are not the
	// target's.
	CodeExportLicenseExpired = "EXPORT_LICENSE_EXPIRED"
	// The licence number of the issuer, or the allocation ID, is taken.
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
	// A purchase invoice of the same kind and number is imported and not rejected (CNX-INV-005).
	CodeInvoiceDuplicate = "INVOICE_DUPLICATE"
	// An allocation would take the evidence of a purpose above its remaining balance (CNX-INV-004).
	CodeTaxEvidenceExceeded = "TAX_EVIDENCE_EXCEEDED"
	// The invoice's buyer is not the company's entity, or its supplier not the commitment's.
	CodePartyEntityMismatch = "PARTY_ENTITY_MISMATCH"
	// A declaration receipt's external version or business order is not after the one recorded: it
	// never overrides it (CNX-INV-003, CN-R05, CN-AT06).
	CodeDeclarationReceiptStale = "DECLARATION_RECEIPT_STALE"
	// A declaration item's mapping of names and units to the declaration has no professional basis
	// (CNX-INV-006, CN-AT03).
	CodeDeclarationMappingUnverified = "DECLARATION_MAPPING_UNVERIFIED"
	// The declaration's submission result is unknown: verify it before anything is submitted again
	// (CNX-INV-003, CN-AT13).
	CodeExternalResultUnknown = "EXTERNAL_RESULT_UNKNOWN"
	// A document requirement has only an attachment, not a verification, and cannot count as met
	// (CNX-INV-008, CN-AT14).
	CodeDocumentRequirementUnverified = "DOCUMENT_REQUIREMENT_UNVERIFIED"
	// An official certificate's object, market or validity is not the use's: another SKU
	// specification, shipment or lot, another market, or expired (CNX-INV-007, CN-AT14).
	CodeCertificateNotApplicable = "CERTIFICATE_NOT_APPLICABLE"
)

var codes = map[string]kernel.Category{
	CodeTradeComplianceNotClear:       kernel.CategoryConflict,
	CodeOrderNotActive:                kernel.CategoryInvalid,
	CodeDeclarationProfileUnverified:  kernel.CategoryInvalid,
	CodeReferenceStale:                kernel.CategoryConflict,
	CodeExportLicenseRequired:         kernel.CategoryConflict,
	CodeExportLicenseExceeded:         kernel.CategoryConflict,
	CodeExportLicenseExpired:          kernel.CategoryConflict,
	CodeBusinessKeyConflict:           kernel.CategoryConflict,
	CodeInvoiceDuplicate:              kernel.CategoryConflict,
	CodeTaxEvidenceExceeded:           kernel.CategoryConflict,
	CodePartyEntityMismatch:           kernel.CategoryConflict,
	CodeDeclarationReceiptStale:       kernel.CategoryConflict,
	CodeDeclarationMappingUnverified:  kernel.CategoryInvalid,
	CodeExternalResultUnknown:         kernel.CategoryConflict,
	CodeDocumentRequirementUnverified: kernel.CategoryConflict,
	CodeCertificateNotApplicable:      kernel.CategoryConflict,
	CodeOpeningBalanceDuplicate:       kernel.CategoryConflict, // C10 (opening.go)
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
