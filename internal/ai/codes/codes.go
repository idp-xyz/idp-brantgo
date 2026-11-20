// Package codes holds the rejection codes of the AI layer (internal/ai) with their categories, kept
// in step with the archive's error catalog (category 任务与 AI; S4 owner decisions 2026-11-12). The
// shared codes (UNSUPPORTED_CONTRACT, SCHEMA_INVALID, PERMISSION_DENIED, ...) stay in the kernel.
package codes

import "go.idp.xyz/idp-brant/internal/kernel"

const (
	// GrantInvalid: the delegation or approval grant does not exist in the caller's scope, belongs to
	// another agent or task, is revoked or already used, or does not match the command, request hash
	// or object version (AI-INV-002; Q01 V0.1 test_grant_reference_is_checked).
	GrantInvalid = "GRANT_INVALID"
	// GrantExpired: the delegation or approval grant has expired (AI-INV-002).
	GrantExpired = "GRANT_EXPIRED"
	// BudgetExhausted: a call, retry, time or cost budget is used up; the task pauses (AI-INV-003). The
	// execution entry (unit C2) returns it.
	BudgetExhausted = "BUDGET_EXHAUSTED"
	// ExecutionBundleIncompatible: a persisted ExecutionBundle no longer fits the application,
	// contracts or configuration (baseline 17.3). The execution entry (unit C2) returns it.
	ExecutionBundleIncompatible = "EXECUTION_BUNDLE_INCOMPATIBLE"
	// ToolUnavailable: the model or a tool is temporarily unavailable; retried within the budget, then
	// the task pauses (AI-INV-003, BC-AT26). It is a transport answer (HTTP 503, retryable), not a
	// rejection: see model.ErrUnavailable.
	ToolUnavailable = "TOOL_UNAVAILABLE"
	// InputProfileUnsupported: a file or a task's files exceed Q01.upload.v0.1 (type, pages, sheets,
	// rows and columns, size, characters or attachments; baseline 14.2). Nothing is truncated: the
	// file goes to the manual path. internal/ai/ingest returns it.
	InputProfileUnsupported = "INPUT_PROFILE_UNSUPPORTED"
	// SourceAmbiguous: a value stands in more than one place of its source and nothing says which
	// (WBS S4-04b); internal/ai/locate returns it for a lookup, and a candidate line carries the field
	// as pending ambiguous until the human verifies it (INQ answers SOURCE_AMBIGUOUS then).
	SourceAmbiguous = "SOURCE_AMBIGUOUS"
)

// categories maps each rejection code of the AI layer to the category transports answer it with.
var categories = map[string]kernel.Category{
	GrantInvalid:                kernel.CategoryForbidden,
	GrantExpired:                kernel.CategoryForbidden,
	BudgetExhausted:             kernel.CategoryInvalid,
	ExecutionBundleIncompatible: kernel.CategoryConflict,
	InputProfileUnsupported:     kernel.CategoryInvalid,
	SourceAmbiguous:             kernel.CategoryInvalid,
}

// Category returns the category of a rejection code of the AI layer.
func Category(code string) (kernel.Category, bool) {
	c, ok := categories[code]
	return c, ok
}

// Reject returns a rejection with one of the AI layer's codes. It panics on another code: that is a
// programming error.
func Reject(code, path string) *kernel.Rejection {
	c, ok := categories[code]
	if !ok {
		panic("codes: not a rejection code of the AI layer: " + code)
	}
	return kernel.RejectAs(code, c, path, "")
}
