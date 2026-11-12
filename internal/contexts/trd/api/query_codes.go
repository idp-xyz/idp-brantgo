package api

// The codes of TRD's read side (WBS S3-09; queries.go): error catalog V0.1 rows whose contexts
// include TRD that readiness and dispatch report as reasons, with the category the catalog gives
// them (all 422, invalid). The queries also report the codes codes.go, calc_codes.go and
// document_codes.go declare and the upstreams' verdicts.

import "go.idp.xyz/idp-brant/internal/kernel"

const (
	CodeCommitEvidenceMissing             = "COMMIT_EVIDENCE_MISSING"              // the revision has no commit evidence, or the Outbox no activation event of it
	CodeApprovalMissing                   = "APPROVAL_MISSING"                     // the approval the commit evidence names is not the root's
	CodeRecipientSnapshotIntegrityFailure = "RECIPIENT_SNAPSHOT_INTEGRITY_FAILURE" // the sealed recipient snapshot does not hash to its business hash
	CodeExternalChannelNotConfigured      = "EXTERNAL_CHANNEL_NOT_CONFIGURED"      // no dispatch channel exists
)

var queryCodes = map[string]kernel.Category{
	CodeCommitEvidenceMissing:             kernel.CategoryInvalid,
	CodeApprovalMissing:                   kernel.CategoryInvalid,
	CodeRecipientSnapshotIntegrityFailure: kernel.CategoryInvalid,
	CodeExternalChannelNotConfigured:      kernel.CategoryInvalid,
}

// QueryCategory returns the category of a code the read side declares.
func QueryCategory(code string) (kernel.Category, bool) {
	c, ok := queryCodes[code]
	return c, ok
}
