package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes FUL declares (error catalog V0.1 rows whose contexts include FUL, and the codes FUL gives or
// passes on that the catalog lists for other contexts), with the catalog's categories (422 invalid,
// 409 conflict). FUL also answers with the shared kernel codes (SCHEMA_INVALID, INVALID_KNOWN_VALUE,
// REFERENCE_INVALID, INVALID_STATE, VERSION_CONFLICT, NOT_FOUND, PERMISSION_DENIED and the rest).
const (
	// CodeCommercialReleaseBlocked (409): the payment condition of the release's scope is not met
	// (STL's PaymentConditionGuard v1) and no exception covers it, or the command needs a granted
	// release of its scope and there is none.
	CodeCommercialReleaseBlocked = "COMMERCIAL_RELEASE_BLOCKED"
	// CodeTradeComplianceNotClear (409): no valid clear trade compliance conclusion covers the order
	// revision the shipment binds (CNX's TradeComplianceGuard v1).
	CodeTradeComplianceNotClear = "TRADE_COMPLIANCE_NOT_CLEAR"
	// CodeExportLicenseRequired (409): the conclusion says the goods need an export licence and no
	// allocation covers the shipment's lines (CNX's ExportLicenseGuard v1).
	CodeExportLicenseRequired = "EXPORT_LICENSE_REQUIRED"
	// CodeShipmentQuantityExceeded (409): the confirmed shipments of an order line and this one
	// exceed the line's quantity (FUL-INV-001; zero tolerance in S5).
	CodeShipmentQuantityExceeded = "SHIPMENT_QUANTITY_EXCEEDED"
	// CodeQualityReleaseRequired (409): a line's quality release is unknown, revoked or outside its
	// validity (QLT's QualityReleaseGuard v1).
	CodeQualityReleaseRequired = "QUALITY_RELEASE_REQUIRED"
	// CodeQualityReleaseInsufficient (409): the shipment would ship more on a quality release than
	// the release has left after FUL's confirmed shipments.
	CodeQualityReleaseInsufficient = "QUALITY_RELEASE_INSUFFICIENT"
	// CodeSupplierDeliveryUnverified (409): a line's supplier delivery is not verified (PUR's
	// SupplierDeliveryGuard v1; a supplier's claim never counts).
	CodeSupplierDeliveryUnverified = "SUPPLIER_DELIVERY_UNVERIFIED"
	// CodeSupplierDeliveryInsufficient (409): the shipment would ship more of a supplier delivery
	// line than its verified quantity less FUL's confirmed shipments (FUL-INV-003).
	CodeSupplierDeliveryInsufficient = "SUPPLIER_DELIVERY_INSUFFICIENT"
	// CodePartyScreeningNotClear (409): a party of the order is not screened clear now (PTY's
	// PartyScreeningGuard v1).
	CodePartyScreeningNotClear = "PARTY_SCREENING_NOT_CLEAR"
	// CodeOrderNotActive: the order is not active (TRD's verdict).
	CodeOrderNotActive = "ORDER_NOT_ACTIVE"
	// CodeOrderApprovalRequired and CodeOrderApprovalRevoked: the order's activating approval is
	// missing or was revoked (TRD's verdicts, passed on).
	CodeOrderApprovalRequired = "ORDER_APPROVAL_REQUIRED"
	CodeOrderApprovalRevoked  = "ORDER_APPROVAL_REVOKED"
	// CodeReferenceStale (409): the order moved to another revision or hash, or the supplier
	// delivery to another version, than the shipment binds.
	CodeReferenceStale = "REFERENCE_STALE"
	// CodeBusinessKeyConflict (409): the shipment ID holds another request.
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
	// CodeDependencyNotSupported: the stock route (STK, S6-01) or a unit conversion between an order
	// line and its release or delivery.
	CodeDependencyNotSupported = "DEPENDENCY_NOT_SUPPORTED"
	// CodeShipmentDocumentsUnreconciled (409): a difference of the document set is neither resolved
	// nor acknowledged, or the set lacks a required document (FUL-INV-004, BT12; S5-08).
	CodeShipmentDocumentsUnreconciled = "SHIPMENT_DOCUMENTS_UNRECONCILED"
	// CodeDocumentIntegrityFailure: a stored document's bytes are not the ones bound (the catalog
	// row lists TRD; FUL reads its renditions the same way).
	CodeDocumentIntegrityFailure = "DOCUMENT_INTEGRITY_FAILURE"
	// CodeInsuranceCoverageInsufficient (422): the insured amount of a CIF or CIP policy is below the
	// contract percentage (default 110 %) of the commercial invoice's value, or in another currency
	// (FUL-INV-005; S5-16).
	CodeInsuranceCoverageInsufficient = "INSURANCE_COVERAGE_INSUFFICIENT"
	// CodeCertificateNotApplicable (409): the certificate result a certificate of origin record names
	// does not apply to the shipment — another kind, market or specification, or outside its validity
	// (CNX's CertificateResultRef v1; CNX-INV-007; S5 C5).
	CodeCertificateNotApplicable = "CERTIFICATE_NOT_APPLICABLE"
)

var codes = map[string]kernel.Category{
	CodeCommercialReleaseBlocked:     kernel.CategoryConflict,
	CodeTradeComplianceNotClear:      kernel.CategoryConflict,
	CodeExportLicenseRequired:        kernel.CategoryConflict,
	CodeShipmentQuantityExceeded:     kernel.CategoryConflict,
	CodeQualityReleaseRequired:       kernel.CategoryConflict,
	CodeQualityReleaseInsufficient:   kernel.CategoryConflict,
	CodeSupplierDeliveryUnverified:   kernel.CategoryConflict,
	CodeSupplierDeliveryInsufficient: kernel.CategoryConflict,
	CodePartyScreeningNotClear:       kernel.CategoryConflict,
	CodeOrderNotActive:               kernel.CategoryInvalid,
	CodeOrderApprovalRequired:        kernel.CategoryInvalid,
	CodeOrderApprovalRevoked:         kernel.CategoryInvalid,
	CodeReferenceStale:               kernel.CategoryConflict,
	CodeBusinessKeyConflict:          kernel.CategoryConflict,
	CodeDependencyNotSupported:       kernel.CategoryInvalid,
	// S5-08
	CodeShipmentDocumentsUnreconciled: kernel.CategoryConflict,
	CodeDocumentIntegrityFailure:      kernel.CategoryInvalid,
	// S5-16
	CodeInsuranceCoverageInsufficient: kernel.CategoryInvalid,
	// S5 C5
	CodeCertificateNotApplicable: kernel.CategoryConflict,
}

// Category returns the category of a code FUL declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Codes returns the codes FUL declares with their categories.
func Codes() map[string]kernel.Category {
	out := make(map[string]kernel.Category, len(codes))
	for k, v := range codes {
		out[k] = v
	}
	return out
}

// Reject returns a rejection with a code FUL declares, at path. It panics on a code FUL does not
// declare.
func Reject(code, path, reason string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("ful: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, reason)
}
