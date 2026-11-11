package orderrev

import (
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the error catalog rows (contexts TRD, and ORDER_NOT_ACTIVE, the PUR row TRD's OrderGuard
// answers too) this package decides. The package also answers with the calculation's codes
// (CALCULATION_LIMIT_EXCEEDED, TAX_POLICY_UNCONFIRMED, PAYMENT_TOTAL_MISMATCH,
// DEPENDENCY_NOT_SUPPORTED) and the shared kernel codes SCHEMA_INVALID, INVALID_KNOWN_VALUE,
// INVALID_STATE, REFERENCE_INVALID and DUPLICATE_LINE_ID.
const (
	CodeQuantityBelowShipped     = "QUANTITY_BELOW_SHIPPED"     // a line would hold less than its confirmed shipments
	CodeOrderHasShipments        = "ORDER_HAS_SHIPMENTS"        // 409: the order has confirmed shipments; close the remainder instead
	CodeDownstreamCommitmentOpen = "DOWNSTREAM_COMMITMENT_OPEN" // 409: a purchase commitment or reservation is open without an approved disposition
	CodeOrderNotFulfilled        = "ORDER_NOT_FULFILLED"        // 409: the confirmed shipments do not reach the order quantity
	CodeOrderNotActive           = "ORDER_NOT_ACTIVE"           // the order is not active
	CodeOrderNotDraft            = "ORDER_NOT_DRAFT"            // the order is not a draft
)

// Categories maps the codes above to their error catalog category.
var Categories = map[string]kernel.Category{
	CodeQuantityBelowShipped:     kernel.CategoryInvalid,
	CodeOrderHasShipments:        kernel.CategoryConflict,
	CodeDownstreamCommitmentOpen: kernel.CategoryConflict,
	CodeOrderNotFulfilled:        kernel.CategoryConflict,
	CodeOrderNotActive:           kernel.CategoryInvalid,
	CodeOrderNotDraft:            kernel.CategoryInvalid,
}

// fail returns a rejection with code at path: one of the codes above, a calculation code or a
// shared kernel code.
func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	if c, ok := calc.Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}
