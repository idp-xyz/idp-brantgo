package shipment

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes the shipment rules give besides the shared kernel codes, with the error catalog's
// categories. Package api declares the same table; the application's tests keep them equal.
const (
	CodeShipmentQuantityExceeded     = "SHIPMENT_QUANTITY_EXCEEDED"
	CodeQualityReleaseRequired       = "QUALITY_RELEASE_REQUIRED"
	CodeQualityReleaseInsufficient   = "QUALITY_RELEASE_INSUFFICIENT"
	CodeSupplierDeliveryUnverified   = "SUPPLIER_DELIVERY_UNVERIFIED"
	CodeSupplierDeliveryInsufficient = "SUPPLIER_DELIVERY_INSUFFICIENT"
	CodePartyScreeningNotClear       = "PARTY_SCREENING_NOT_CLEAR"
	CodeOrderNotActive               = "ORDER_NOT_ACTIVE"
	CodeOrderApprovalRequired        = "ORDER_APPROVAL_REQUIRED"
	CodeOrderApprovalRevoked         = "ORDER_APPROVAL_REVOKED"
	CodeReferenceStale               = "REFERENCE_STALE"
	CodeBusinessKeyConflict          = "BUSINESS_KEY_CONFLICT"
	CodeDependencyNotSupported       = "DEPENDENCY_NOT_SUPPORTED"
)

// Codes maps every code of the shipment rules to its category.
var Codes = map[string]kernel.Category{
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
}

// Reject returns a rejection with a code of the shipment rules or a shared kernel code, at path.
func Reject(code, path, reason string) *kernel.Rejection {
	if c, ok := Codes[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

// Upstream returns the refusal an upstream context gave by code — TRD's verdict on the order, PUR's
// on a supplier delivery — at path: with the code when the shipment rules know it, and as
// REFERENCE_INVALID otherwise, so an upstream code FUL does not declare never leaves FUL.
func Upstream(code, path, reason string) *kernel.Rejection {
	if _, ok := Codes[code]; ok {
		return Reject(code, path, reason)
	}
	return kernel.Reject(kernel.CodeReferenceInvalid, path, reason)
}
