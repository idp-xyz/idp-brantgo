package api

import "go.idp.xyz/idp-brant/internal/kernel"

// Codes TSK declares (error catalog V0.1 rows whose contexts include TSK, and the catalog codes TSK
// gives that the catalog lists for other contexts), with the catalog's categories (422 invalid, 409
// conflict). TSK also answers with the shared kernel codes (SCHEMA_INVALID, INVALID_KNOWN_VALUE,
// REFERENCE_INVALID, INVALID_STATE, VERSION_CONFLICT, NOT_FOUND, PERMISSION_DENIED and the rest) and
// passes on TRD's refusals of an object control command unchanged (OBJECT_CONTROL_CONFLICT,
// CONTROL_EPOCH_STALE and the rest, Context Map R17).
const (
	// CodeGoalUnsupported: the goal code is not registered; P0 supports quote_ready and
	// order_active only (catalog 任务与 AI).
	CodeGoalUnsupported = "GOAL_UNSUPPORTED"
	// CodeControlModeMismatch (409): the task's control mode does not admit this executor: a manual
	// task issues no claim to an AI actor (D-2, TSK-INV-004; Q01 V0.1 code).
	CodeControlModeMismatch = "CONTROL_MODE_MISMATCH"
	// CodeControlEpochStale (409): TRD answered a control epoch the task recorded before; TSK records
	// only an epoch above its own (TSK-INV-002; catalog TRD／TSK).
	CodeControlEpochStale = "CONTROL_EPOCH_STALE"
	// CodeExternalResultUnknown (409): the task has a submitted or unknown action; the stop barrier,
	// the release of the root and a failure wait until it is reconciled (TSK-INV-003).
	CodeExternalResultUnknown = "EXTERNAL_RESULT_UNKNOWN"
	// CodeBusinessKeyConflict (409): the task ID holds another request, or an open task already
	// serves the same goal for the same source (来源防重, SM-TSK-TASK created→scoping, AN06).
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
	// CodeDependencyNotSupported: a TRD command or query the command needs is not wired into this
	// process yet (object_control.enter_manual and object_control.handoff before S4 unit A2,
	// GoalReadiness v1 before unit A3).
	CodeDependencyNotSupported = "DEPENDENCY_NOT_SUPPORTED"
)

var codes = map[string]kernel.Category{
	CodeGoalUnsupported:        kernel.CategoryInvalid,
	CodeControlModeMismatch:    kernel.CategoryConflict,
	CodeControlEpochStale:      kernel.CategoryConflict,
	CodeExternalResultUnknown:  kernel.CategoryConflict,
	CodeBusinessKeyConflict:    kernel.CategoryConflict,
	CodeDependencyNotSupported: kernel.CategoryInvalid,
}

// Category returns the category of a code TSK declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Codes returns the codes TSK declares with their categories.
func Codes() map[string]kernel.Category {
	out := make(map[string]kernel.Category, len(codes))
	for k, v := range codes {
		out[k] = v
	}
	return out
}

// Reject returns a rejection with a code TSK declares, at path. It panics on a code TSK does not
// declare.
func Reject(code, path, reason string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("tsk: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, reason)
}
