package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes MFG declares. HTTP 409 is conflict, HTTP 422 is invalid. DEPENDENCY_NOT_SUPPORTED is the
// catalog code for a company that has not enabled manufacturing (MFG-INV-001).
const (
	// CodeDependencyNotSupported (422): manufacturing is off, or a handover needs stock and stock
	// is off (MFG-INV-001, R42).
	CodeDependencyNotSupported = "DEPENDENCY_NOT_SUPPORTED"
	// CodeBusinessKeyConflict (409): the plan, subcontract, progress or handover ID holds another
	// request.
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
)

var codes = map[string]kernel.Category{
	CodeDependencyNotSupported: kernel.CategoryInvalid,
	CodeBusinessKeyConflict:    kernel.CategoryConflict,
}

// Category returns the category of a code MFG declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Codes returns the codes MFG declares with their categories.
func Codes() map[string]kernel.Category {
	out := make(map[string]kernel.Category, len(codes))
	for k, v := range codes {
		out[k] = v
	}
	return out
}

// Reject returns a rejection with a code MFG declares, at path.
func Reject(code, path, reason string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("mfg: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, reason)
}
