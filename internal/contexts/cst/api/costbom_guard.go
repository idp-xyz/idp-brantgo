package api

import "go.idp.xyz/idp-brant/internal/kernel"

// CostBOMGuard v1 (WBS S7-02) lets a downstream context re-check, inside its own transaction,
// that the cost BOM revision it cites is still the verified current revision, and keeps the
// root and that revision locked FOR SHARE until the transaction ends. CST publishes it as
// brant_cst.guard_cost_bom_current_v1 (migrations/cst/000004_cst_cost_boms.sql):
//
//	(p_bom_id text, p_revision bigint, p_content_sha256 text, p_size_code text, p_substitution_id text)
//	    RETURNS TABLE (known boolean, status text, code text, content_sha256 text, revision bigint, document_profile text)
//
// An empty hash, size or substitution is NULL and skips that check. A size-graded revision asked
// for a size it does not store answers BOM_SIZE_GRADE_MISSING and does not use another size.
// The caller locks its own aggregate root FOR UPDATE first (ADR-BR-011).
const (
	CostBOMGuardFunction = "brant_cst.guard_cost_bom_current_v1"
	CostBOMGuardSQL      = `SELECT known, status, code, content_sha256, revision, document_profile FROM brant_cst.guard_cost_bom_current_v1($1, $2, $3, $4, $5)`
)

// CostBOMRef is a revision of a cost BOM, optionally pinned by its frozen hash, asked at one size,
// and optionally required to be covered by one substitution approval.
type CostBOMRef struct {
	BomID          string
	Revision       int64
	ContentSHA256  string
	SizeCode       string
	SubstitutionID string
}

// CostBOMGuardArgs returns the arguments of CostBOMGuardSQL. Empty optional strings are NULL.
func CostBOMGuardArgs(ref CostBOMRef) []any {
	return []any{ref.BomID, ref.Revision, nullText(ref.ContentSHA256), nullText(ref.SizeCode), nullText(ref.SubstitutionID)}
}

func nullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// CostBOMVerdict is the guard's answer for Ref.
type CostBOMVerdict struct {
	Ref             CostBOMRef
	Known           bool
	Status          string
	Code            string
	ContentSHA256   string
	Revision        int64
	DocumentProfile string
}

// ScanCostBOMGuard reads the row of CostBOMGuardSQL for ref through scan.
func ScanCostBOMGuard(ref CostBOMRef, scan func(dest ...any) error) (CostBOMVerdict, error) {
	v := CostBOMVerdict{Ref: ref}
	var status, code, hash, profile *string
	var revision *int64
	if err := scan(&v.Known, &status, &code, &hash, &revision, &profile); err != nil {
		return CostBOMVerdict{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{status, &v.Status}, {code, &v.Code}, {hash, &v.ContentSHA256}, {profile, &v.DocumentProfile}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	if revision != nil {
		v.Revision = *revision
	}
	return v, nil
}

// Adoptable reports whether the revision may be adopted.
func (v CostBOMVerdict) Adoptable() bool {
	return v.Known && v.Code == "" && v.Status == StatusVerifiedBOM
}

// StatusVerifiedBOM is the guard status of a revision that may be adopted.
const StatusVerifiedBOM = "verified"

// Check returns nil when the revision may be adopted. An unknown BOM or a hash that is not current
// is REFERENCE_INVALID. The guard's own codes are passed on.
func (v CostBOMVerdict) Check(path string) error {
	switch {
	case !v.Known || v.Status == "not_current":
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	case v.Adoptable():
		return nil
	case v.Code != "":
		return Reject(v.Code, path)
	default:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
}
