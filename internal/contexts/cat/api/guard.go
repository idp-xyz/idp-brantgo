package api

import "go.idp.xyz/idp-brant/internal/kernel"

// SkuGuard v1 (ADR-BR-004 option B, Context Map R14 CAT->TRD "SkuRef v1" with a qualification
// guard, guardLock true) lets a downstream context re-check, inside its own transaction, that the
// specification revision of a SKU it adopts is still the SKU's current confirmed one, and keeps it
// so until that transaction ends. CAT publishes it as a SECURITY DEFINER function in its schema
// with a fixed search_path (migrations/cat/000003_cat_sku_guard.sql):
//
//	brant_cat.guard_sku_spec_current_v1(p_sku_id text, p_spec_revision bigint)
//	    RETURNS TABLE (known boolean, status text, current_revision bigint, code text)
//
// It always returns one row, in the caller's scope (app.tenant_id and app.company_id, which
// bento.ScopeTransactor sets), and decides what ResolveSpecs decides before the transaction
// (CAT-INV-001). It locks, FOR SHARE in the caller's transaction, the SKU row and then the confirmed
// revision row. sku.revise_spec, sku.confirm and sku.deactivate update the SKU row, so each either
// committed before the guard read it, and the verdict says so, or waits until the caller's
// transaction ends.
//
// A downstream command locks its own aggregate root FOR UPDATE first and calls the guard after
// (ADR-BR-011), with its own executor; CAT comes first in context code order, one call per SKU
// reference in (SKU, revision) order:
//
//	row := exec.QueryRow(ctx, catapi.SkuGuardSQL, catapi.SkuGuardArgs(ref)...)
//	verdict, err := catapi.ScanSkuGuard(ref, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/lines/0/sku")
//	}
//
// The guard does not apply the caller's data scope (own, team): the downstream checks visibility
// when it resolves the references before its transaction.
const (
	SkuGuardFunction = "brant_cat.guard_sku_spec_current_v1"
	SkuGuardSQL      = `SELECT known, status, current_revision, code FROM brant_cat.guard_sku_spec_current_v1($1, $2)`
)

// SkuGuardArgs returns the arguments of SkuGuardSQL for ref.
func SkuGuardArgs(ref SpecRef) []any { return []any{ref.SkuID, ref.SpecRevision} }

// SkuGuardVerdict is the guard's answer for Ref. Known is false when the SKU does not exist in the
// caller's company or never had the revision confirmed (Code REFERENCE_INVALID). Otherwise Code is
// empty when the revision is the current confirmed one of a confirmed SKU, REFERENCE_STALE when the
// SKU was reopened for revision or confirmed in a later revision, and REFERENCE_INVALID when the SKU
// is inactive. Status and CurrentRevision describe the SKU now, as SpecStanding does: its status,
// and its latest confirmed revision while it is confirmed, else 0; both are empty for an unknown
// SKU.
type SkuGuardVerdict struct {
	Ref             SpecRef
	Known           bool
	Status          string
	CurrentRevision int64
	Code            string
}

// ScanSkuGuard reads the row of SkuGuardSQL for ref through scan, such as a pgx row's Scan.
func ScanSkuGuard(ref SpecRef, scan func(dest ...any) error) (SkuGuardVerdict, error) {
	v := SkuGuardVerdict{Ref: ref}
	var status, code *string
	var current *int64
	if err := scan(&v.Known, &status, &current, &code); err != nil {
		return SkuGuardVerdict{}, err
	}
	if status != nil {
		v.Status = *status
	}
	if current != nil {
		v.CurrentRevision = *current
	}
	if code != nil {
		v.Code = *code
	}
	return v, nil
}

// Current reports whether the revision is the SKU's current confirmed one.
func (v SkuGuardVerdict) Current() bool { return v.Known && v.Code == "" }

// Check returns nil for a current revision and, at path of the caller's request, REFERENCE_STALE
// (409, refresh the reference and resubmit) for a revision the SKU has moved on from, and
// REFERENCE_INVALID for an unknown SKU or revision or an inactive SKU.
func (v SkuGuardVerdict) Check(path string) error {
	switch {
	case v.Current():
		return nil
	case v.Known && v.Code == CodeReferenceStale:
		return kernel.RejectAs(CodeReferenceStale, codes[CodeReferenceStale], path, "Specification revision is no longer current")
	case v.Known && v.Status == StatusInactive:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "The SKU is inactive")
	}
	return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
}
