package api

import "go.idp.xyz/idp-brant/internal/kernel"

// SampleGuard v1 (ADR-BR-004 option B) lets a downstream context re-check, inside its own
// transaction, that a SKU specification revision still has a current approval for a sample stage.
// CAT publishes it as brant_cat.guard_sample_approved_v1 (migrations/cat/000006_cat_sample_approvals.sql).
// It locks the approval FOR SHARE and then the SKU FOR SHARE, in the caller's scope.
//
// A downstream command locks its own root FOR UPDATE first. quote.freeze calls the guard when the
// quote's profile is apparel and a line names a stage, after the plugin guard and before SkuGuard:
//
//	v, err := catapi.ScanSampleGuard(ref, exec.QueryRow(ctx, catapi.SampleGuardSQL, catapi.SampleGuardArgs(ref)...).Scan)
//	if err == nil {
//		err = v.Check("/args/data/lines/0/sample_stage")
//	}
const (
	SampleGuardFunction = "brant_cat.guard_sample_approved_v1"
	SampleGuardSQL      = `SELECT known, approved, sample_approval_id, approved_spec_revision, code FROM brant_cat.guard_sample_approved_v1($1, $2, $3)`

	// CodeSampleApprovalRequired is SAMPLE_APPROVAL_REQUIRED (422): no current approval for the stage.
	CodeSampleApprovalRequired = "SAMPLE_APPROVAL_REQUIRED"
	// CodeSampleSpecMismatch is SAMPLE_SPEC_MISMATCH (409): the current approval binds another specification revision.
	CodeSampleSpecMismatch = "SAMPLE_SPEC_MISMATCH"
)

// SampleRef is one SKU specification revision and the sample stage a caller requires.
type SampleRef struct {
	SkuID        string
	SpecRevision int64
	Stage        string
}

// SampleGuardArgs returns the arguments of SampleGuardSQL for ref.
func SampleGuardArgs(ref SampleRef) []any {
	return []any{ref.SkuID, ref.SpecRevision, ref.Stage}
}

// SampleGuardVerdict is the guard's one row. Approved is true only when Code is empty.
type SampleGuardVerdict struct {
	Ref          SampleRef
	Known        bool
	Approved     bool
	ApprovalID   string
	ApprovedSpec int64
	Code         string
}

// ScanSampleGuard reads the row of SampleGuardSQL for ref.
func ScanSampleGuard(ref SampleRef, scan func(dest ...any) error) (SampleGuardVerdict, error) {
	v := SampleGuardVerdict{Ref: ref}
	var id, code *string
	var spec *int64
	if err := scan(&v.Known, &v.Approved, &id, &spec, &code); err != nil {
		return SampleGuardVerdict{}, err
	}
	if id != nil {
		v.ApprovalID = *id
	}
	if spec != nil {
		v.ApprovedSpec = *spec
	}
	if code != nil {
		v.Code = *code
	}
	return v, nil
}

// Check returns nil when the revision is approved for the stage. A missing approval is
// SAMPLE_APPROVAL_REQUIRED (422). A different bound revision is SAMPLE_SPEC_MISMATCH (409).
// An unknown or inactive SKU is REFERENCE_INVALID.
func (v SampleGuardVerdict) Check(path string) error {
	if v.Approved && v.Code == "" {
		return nil
	}
	switch v.Code {
	case CodeSampleSpecMismatch:
		return Reject(CodeSampleSpecMismatch, path)
	case CodeSampleApprovalRequired:
		return Reject(CodeSampleApprovalRequired, path)
	case kernel.CodeReferenceInvalid:
		if v.Known {
			return kernel.Reject(kernel.CodeReferenceInvalid, path, "The SKU is inactive")
		}
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	default:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
}
