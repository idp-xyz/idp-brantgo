package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes QLT declares (error catalog V0.1 rows whose contexts include QLT, and the codes QLT gives
// that the catalog lists for other contexts), with the catalog's categories (422 invalid, 409
// conflict). QLT also answers with the shared kernel codes (SCHEMA_INVALID, INVALID_KNOWN_VALUE,
// REFERENCE_INVALID, INVALID_STATE, VERSION_CONFLICT, NOT_FOUND, PERMISSION_DENIED and the rest)
// and passes on PTY's REFERENCE_STALE for a service role that no longer serves.
const (
	// CodeQualityReleaseExceedsConforming: the release would exceed the conforming quantity of the
	// subject's valid inspections less what was released before (QLT-INV-001).
	CodeQualityReleaseExceedsConforming = "QUALITY_RELEASE_EXCEEDS_CONFORMING"
	// CodeQualityReleaseRequired (409): the release is unknown, revoked or outside its validity at
	// the instant asked; QualityReleaseGuard v1 answers it to FUL (QLT-INV-002).
	CodeQualityReleaseRequired = "QUALITY_RELEASE_REQUIRED"
	// CodeReferenceStale (409): the subject no longer serves inspection or release (PUR's verdict),
	// or a cited service role is not current (PTY's verdict).
	CodeReferenceStale = "REFERENCE_STALE"
	// CodeBusinessKeyConflict (409): the inspection or release ID holds another request.
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
	// CodeDependencyNotSupported: PUR's InspectionSubject v1 is not wired into this process, or the
	// subject kind is not delivered yet.
	CodeDependencyNotSupported = "DEPENDENCY_NOT_SUPPORTED"
)

var codes = map[string]kernel.Category{
	CodeQualityReleaseExceedsConforming: kernel.CategoryInvalid,
	CodeQualityReleaseRequired:          kernel.CategoryConflict,
	CodeReferenceStale:                  kernel.CategoryConflict,
	CodeBusinessKeyConflict:             kernel.CategoryConflict,
	CodeDependencyNotSupported:          kernel.CategoryInvalid,
}

// Category returns the category of a code QLT declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Codes returns the codes QLT declares with their categories.
func Codes() map[string]kernel.Category {
	out := make(map[string]kernel.Category, len(codes))
	for k, v := range codes {
		out[k] = v
	}
	return out
}

// Reject returns a rejection with a code QLT declares, at path. It panics on a code QLT does not
// declare.
func Reject(code, path, reason string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("qlt: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, reason)
}
