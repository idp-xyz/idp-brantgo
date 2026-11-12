package supplierdelivery

import (
	"strconv"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the supplier delivery rules (error catalog V0.1: the PUR rows SUPPLIER_DELIVERY_EXCEEDED
// and SUPPLIER_DELIVERY_UNVERIFIED, and the shared rows whose contexts include PUR), with the
// catalog's categories. The api package declares the same codes for callers; a test keeps the tables
// equal.
const (
	CodeSupplierDeliveryExceeded   = "SUPPLIER_DELIVERY_EXCEEDED"   // 409: the verified deliveries of a commitment line would exceed what it buys
	CodeSupplierDeliveryUnverified = "SUPPLIER_DELIVERY_UNVERIFIED" // 409: a claim no human verified; it never counts
	CodeIndependentReviewRequired  = "INDEPENDENT_REVIEW_REQUIRED"  // the verifier recorded the claim
	CodeReferenceStale             = "REFERENCE_STALE"              // 409: a version a correction superseded
)

// Codes maps this package's codes to their categories.
var Codes = map[string]kernel.Category{
	CodeSupplierDeliveryExceeded:   kernel.CategoryConflict,
	CodeSupplierDeliveryUnverified: kernel.CategoryConflict,
	CodeIndependentReviewRequired:  kernel.CategoryInvalid,
	CodeReferenceStale:             kernel.CategoryConflict,
}

// fail returns a rejection with a code of this package or a shared kernel code.
func fail(code, path, reason string) *kernel.Rejection {
	if c, ok := Codes[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func text(d kernel.Decimal) string {
	s, _ := d.Exact()
	return s
}
