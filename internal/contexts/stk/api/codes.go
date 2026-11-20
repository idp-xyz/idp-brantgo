package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes STK declares (error catalog rows whose contexts include STK, and the shared codes STK
// answers with its own category). HTTP 409 is conflict, HTTP 422 is invalid.
const (
	// CodeStockInsufficient (409): available quantity does not cover the issue, the adjustment or
	// the reservation (STK-INV-001).
	CodeStockInsufficient = "STOCK_INSUFFICIENT"
	// CodeStockReservationConflict (409): the reservation is not active, does not match the
	// balance, or is an order hold that stock.issue must not consume (STK-INV-001).
	CodeStockReservationConflict = "STOCK_RESERVATION_CONFLICT"
	// CodeDependencyNotSupported (422): the company has not enabled STK (STK-INV-004). The default.
	CodeDependencyNotSupported = "DEPENDENCY_NOT_SUPPORTED"
	// CodeBusinessKeyConflict (409): the movement or reservation id already holds another request.
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
)

var codes = map[string]kernel.Category{
	CodeStockInsufficient:        kernel.CategoryConflict,
	CodeStockReservationConflict: kernel.CategoryConflict,
	CodeDependencyNotSupported:   kernel.CategoryInvalid,
	CodeBusinessKeyConflict:      kernel.CategoryConflict,
}

// Category returns the category of a code STK declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Codes returns the codes STK declares.
func Codes() map[string]kernel.Category {
	out := make(map[string]kernel.Category, len(codes))
	for k, v := range codes {
		out[k] = v
	}
	return out
}

// Reject returns a rejection with a code STK declares, at path.
func Reject(code, path, reason string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("stk: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, reason)
}
