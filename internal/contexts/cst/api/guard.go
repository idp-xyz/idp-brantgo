package api

import (
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// AdoptabilityGuard v1 (ADR-BR-004 option B, Context Map R11 CST→TRD "CostSnapshotRef v1＋
// AdoptabilityGuard", guardLock true) lets a downstream context re-check, inside its own
// transaction, that the cost snapshot it adopts is still adoptable, and keeps it so until that
// transaction ends. CST publishes it as a SECURITY DEFINER function in its schema with a fixed
// search_path (migrations/cst/000003_cst_cost_snapshots.sql):
//
//	brant_cst.guard_cost_snapshot_adoptable_v1(p_snapshot_id text, p_result_hash text, p_at timestamptz)
//	    RETURNS TABLE (known boolean, status text, code text, input text, result_hash text, evidence_hash text)
//
// It always returns one row, in the caller's scope (app.tenant_id and app.company_id, which
// bento.ScopeTransactor sets). It locks, in the caller's transaction, the snapshot row FOR SHARE,
// then the standing rows of the sources the snapshot's evaluation rests on (in ID order), then the
// roots of the CST revisions it bound, then calls RfqGuard v1, ConfigGuard v1 and
// SupplyConditionGuard v1 for its INQ, POL and SUP inputs at p_at (context code order), each locking
// its row FOR SHARE. A costing.attest_source denial updates the source's standing row, a freeze
// that supersedes the snapshot locks its row FOR UPDATE, and every command that ends an input
// updates the row its guard locks: each either committed before the guard read the row, and the
// verdict says so, or waits until the caller's transaction ends.
//
// A downstream command locks its own aggregate root FOR UPDATE first and calls the guard after
// (ADR-BR-011), with its own executor:
//
//	row := exec.QueryRow(ctx, cstapi.AdoptabilityGuardSQL, cstapi.AdoptabilityGuardArgs(ref, now)...)
//	verdict, err := cstapi.ScanAdoptabilityGuard(ref, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/cost_snapshot_ref")
//	}
//
// Data scope (own, team) is checked by SnapshotLookups before the transaction; the guard confines the
// row to the caller's tenant and company.
const (
	AdoptabilityGuardFunction = "brant_cst.guard_cost_snapshot_adoptable_v1"
	AdoptabilityGuardSQL      = `SELECT known, status, code, input, result_hash, evidence_hash FROM brant_cst.guard_cost_snapshot_adoptable_v1($1, $2, $3)`
)

// AdoptabilityGuardArgs returns the arguments of AdoptabilityGuardSQL for ref at instant at; an
// empty result hash is passed as NULL and binds the snapshot alone.
func AdoptabilityGuardArgs(ref CostSnapshotRef, at time.Time) []any {
	var hash *string
	if ref.ResultHash != "" {
		hash = &ref.ResultHash
	}
	return []any{ref.SnapshotID, hash, kernel.Instant(at)}
}

// AdoptabilityVerdict is the guard's answer for Ref. Known is false when no such snapshot exists
// in the caller's company, or its result hash is not Ref's (Code REFERENCE_INVALID). Otherwise
// Status is adoptable with an empty Code, or denied, superseded or not_current with Code
// COST_SNAPSHOT_INELIGIBLE and Input naming what decided it; ResultHash and EvidenceHash are the
// snapshot's.
type AdoptabilityVerdict struct {
	Ref          CostSnapshotRef
	Known        bool
	Status       string
	Code         string
	Input        string
	ResultHash   string
	EvidenceHash string
}

// ScanAdoptabilityGuard reads the row of AdoptabilityGuardSQL for ref through scan, such as a pgx
// row's Scan.
func ScanAdoptabilityGuard(ref CostSnapshotRef, scan func(dest ...any) error) (AdoptabilityVerdict, error) {
	v := AdoptabilityVerdict{Ref: ref}
	var status, code, input, result, evidence *string
	if err := scan(&v.Known, &status, &code, &input, &result, &evidence); err != nil {
		return AdoptabilityVerdict{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{status, &v.Status}, {code, &v.Code}, {input, &v.Input}, {result, &v.ResultHash}, {evidence, &v.EvidenceHash}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	return v, nil
}

// Adoptable reports whether the snapshot may be adopted.
func (v AdoptabilityVerdict) Adoptable() bool {
	return v.Known && v.Code == "" && v.Status == AdoptionAdoptable
}

// Check returns nil when the snapshot may be adopted and, at path of the caller's request,
// REFERENCE_INVALID for an unknown snapshot or another result hash, and COST_SNAPSHOT_INELIGIBLE
// (the error catalog's code for a snapshot not currently adoptable) otherwise.
func (v AdoptabilityVerdict) Check(path string) error {
	switch {
	case !v.Known:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	case v.Adoptable():
		return nil
	}
	return kernel.RejectAs(CodeCostSnapshotIneligible, codes[CodeCostSnapshotIneligible], path,
		fmt.Sprintf("The cost snapshot is %s (%s)", v.Status, v.Input))
}
