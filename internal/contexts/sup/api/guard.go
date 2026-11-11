package api

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// SupplyConditionGuard v1 (ADR-BR-004 option B, Context Map R09 and R20, guardLock true) lets a
// downstream context re-check, inside its own transaction, that the supply condition revision it
// adopts is in force, and keeps it so until that transaction ends. SUP publishes it as a SECURITY
// DEFINER function in its schema with a fixed search_path
// (migrations/sup/000002_sup_supply_condition_guard.sql):
//
//	brant_sup.guard_supply_condition_v1(p_kind text, p_id text, p_revision bigint, p_at timestamptz)
//	    RETURNS TABLE (known boolean, status text, code text, content_hash text)
//
// It always returns one row. It locks the exact revision row FOR SHARE in the caller's transaction,
// in the caller's scope (app.tenant_id and app.company_id, which bento.ScopeTransactor sets); for
// purchase terms of dependent validity it then locks the source offer revision row FOR SHARE too.
// Every SUP command that ends a revision (verifying or publishing the next one supersedes it,
// withdrawing or retiring ends it) updates that row, so it either committed before the guard read
// the row, and the guard reports the revision not in force, or it waits until the caller's
// transaction ends. The verdict is the revision's current state, not its history: it is in force
// when it is active now, was verified at or before at, and its validity [valid_from, valid_until)
// contains at; dependent purchase terms also need their source offer revision in force.
//
// A downstream command locks its own aggregate root FOR UPDATE first and calls the guard after
// (ADR-BR-011), with its own executor:
//
//	row := exec.QueryRow(ctx, supapi.SupplyConditionGuardSQL, supapi.SupplyConditionGuardArgs(ref, now)...)
//	verdict, err := supapi.ScanSupplyConditionGuard(ref, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/allocations/0/source")
//	}
//
// The guard answers about SUP's facts only: the supplier's party, the SKU and the POL
// configurations a condition cites are re-checked under the downstream's own relationships with
// PTY, CAT and POL. Data scope (own, team) is checked by the lookup before the transaction; the
// guard confines the row to the caller's tenant and company.
const (
	SupplyConditionGuardFunction = "brant_sup.guard_supply_condition_v1"
	SupplyConditionGuardSQL      = `SELECT known, status, code, content_hash FROM brant_sup.guard_supply_condition_v1($1, $2, $3, $4)`
)

// SupplyConditionGuardArgs returns the arguments of SupplyConditionGuardSQL for ref at instant at.
func SupplyConditionGuardArgs(ref SupplyConditionRef, at time.Time) []any {
	return []any{ref.Kind, ref.ID, ref.Revision, kernel.Instant(at)}
}

// SupplyConditionGuardVerdict is the guard's answer for Ref. Known is false when no such revision
// exists in the caller's company (Code REFERENCE_INVALID); otherwise Status is the revision's status
// and Code is empty when it is in force, SUPPLY_CONDITION_NOT_CURRENT when it is not active (a
// draft, superseded, withdrawn, retired or discarded), was verified after the instant or depends on
// a source offer that is not, and SUPPLY_CONDITION_EXPIRED when its validity, or its source offer's,
// does not contain the instant.
type SupplyConditionGuardVerdict struct {
	Ref         SupplyConditionRef
	Known       bool
	Status      string
	Code        string
	ContentHash string
}

// ScanSupplyConditionGuard reads the row of SupplyConditionGuardSQL for ref through scan, such as a
// pgx row's Scan.
func ScanSupplyConditionGuard(ref SupplyConditionRef, scan func(dest ...any) error) (SupplyConditionGuardVerdict, error) {
	v := SupplyConditionGuardVerdict{Ref: ref}
	var status, code, hash *string
	if err := scan(&v.Known, &status, &code, &hash); err != nil {
		return SupplyConditionGuardVerdict{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{status, &v.Status}, {code, &v.Code}, {hash, &v.ContentHash}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	return v, nil
}

// InForce reports whether the revision is in force.
func (v SupplyConditionGuardVerdict) InForce() bool { return v.Known && v.Code == "" }

// Check returns nil when the revision is in force and SUP's rejection otherwise, at path of the
// caller's request.
func (v SupplyConditionGuardVerdict) Check(path string) error {
	switch {
	case !v.Known:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	case v.Code != "":
		return Reject(v.Code, path)
	}
	return nil
}
