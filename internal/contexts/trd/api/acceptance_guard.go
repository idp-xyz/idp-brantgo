package api

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// AcceptanceGuard v1 (ADR-BR-004 option B, ADR-BR-011) lets a step that relies on a formed contract —
// TRD's own order step, or a context downstream of TRD — re-check inside its own transaction that an
// acceptance is still valid and formed for the revision the step binds, and keeps it so until that
// transaction ends. TRD publishes it as a SECURITY DEFINER function in its schema with a fixed
// search_path (migrations/trd/000004_trd_customer_acceptance.sql):
//
//	brant_trd.guard_acceptance_valid_v1(p_acceptance_id text, p_quote_id text, p_revision bigint, p_business_hash text)
//	    RETURNS TABLE (known boolean, quote_id text, revision bigint, business_hash text, response_id text, effectivity text,
//	                   effectivity_version bigint, arrival_at timestamptz, arrival_basis text, expires_at timestamptz,
//	                   formed boolean, code text)
//
// It always returns one row in the caller's scope (app.tenant_id and app.company_id). It locks the
// quote root p_quote_id FOR SHARE, as QuoteRevisionGuard does, then the acceptance row FOR SHARE, in
// the caller's transaction. acceptance.invalidate and acceptance.mark_disputed lock that root FOR
// UPDATE and then the acceptance row FOR UPDATE, so each either committed before the guard read it,
// and the verdict says so, or waits until the caller's transaction ends; a guard called while one
// of them holds its locks waits for it and reads the acceptance as it left it. Because the root
// comes first in both, the guard and QuoteRevisionGuard may be called in either order. A caller
// locks its own aggregate root FOR UPDATE first; TRD's own order step locks the quote root FOR
// UPDATE itself:
//
//	b := trdapi.AcceptanceBinding{AcceptanceID: id, QuoteID: quote, BusinessRevision: rev, BusinessHash: hash}
//	row := exec.QueryRow(ctx, trdapi.AcceptanceGuardSQL, trdapi.AcceptanceGuardArgs(b)...)
//	verdict, err := trdapi.ScanAcceptanceGuard(b, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/acceptance_id")
//	}
//
// The guard does not apply the caller's data scope (own, team): the caller checks visibility when it
// resolves the acceptance before its transaction (AcceptanceLookups). It does not compare any clock
// with ExpiresAt: a formed contract stays formed after the quote expires (TRD-INV-017, BC-AT13).
const (
	AcceptanceGuardFunction = "brant_trd.guard_acceptance_valid_v1"
	AcceptanceGuardSQL      = `SELECT known, quote_id, revision, business_hash, response_id, effectivity, effectivity_version, arrival_at,
		arrival_basis, expires_at, formed, code FROM brant_trd.guard_acceptance_valid_v1($1, $2, $3, $4)`
)

// AcceptanceBinding is an acceptance as a downstream fact binds it: the acceptance and the revision
// and business hash it accepts.
type AcceptanceBinding struct {
	AcceptanceID     string
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
}

// AcceptanceGuardArgs returns the arguments of AcceptanceGuardSQL for b.
func AcceptanceGuardArgs(b AcceptanceBinding) []any {
	return []any{b.AcceptanceID, b.QuoteID, b.BusinessRevision, b.BusinessHash}
}

// AcceptanceVerdict is the guard's answer for Ref. Known is false, with Code REFERENCE_INVALID, for
// an acceptance that does not exist in the caller's company. Otherwise the fields are the
// acceptance's — the revision it accepts, its effectivity at its version, its arrival, the
// expires_at it was judged against and whether it formed the contract — and Code is empty when it
// is valid and formed for the bound revision, ACCEPTANCE_SCOPE_MISMATCH when it accepts another
// quote, revision or business hash, ACCEPTANCE_NOT_VALID (409) while invalidated or disputed, and
// ACCEPTANCE_LATE for an acceptance that did not form a contract (the database stores none).
type AcceptanceVerdict struct {
	Ref                AcceptanceBinding
	Known              bool
	QuoteID            string
	BusinessRevision   int64
	BusinessHash       string
	ResponseID         string
	Effectivity        string
	EffectivityVersion int64
	ArrivalAt          time.Time
	ArrivalBasis       string
	ExpiresAt          time.Time
	Formed             bool
	Code               string
}

// ScanAcceptanceGuard reads the row of AcceptanceGuardSQL for b through scan, such as a pgx row's
// Scan.
func ScanAcceptanceGuard(b AcceptanceBinding, scan func(dest ...any) error) (AcceptanceVerdict, error) {
	v := AcceptanceVerdict{Ref: b}
	var quote, hash, response, effectivity, basis, code *string
	var revision, version *int64
	var arrival, expires *time.Time
	var formed *bool
	if err := scan(&v.Known, &quote, &revision, &hash, &response, &effectivity, &version, &arrival, &basis, &expires, &formed,
		&code); err != nil {
		return AcceptanceVerdict{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{quote, &v.QuoteID}, {hash, &v.BusinessHash}, {response, &v.ResponseID}, {effectivity, &v.Effectivity}, {basis, &v.ArrivalBasis},
		{code, &v.Code}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	if revision != nil {
		v.BusinessRevision = *revision
	}
	if version != nil {
		v.EffectivityVersion = *version
	}
	if arrival != nil {
		v.ArrivalAt = kernel.Instant(*arrival)
	}
	if expires != nil {
		v.ExpiresAt = kernel.Instant(*expires)
	}
	if formed != nil {
		v.Formed = *formed
	}
	return v, nil
}

// Usable reports whether the acceptance is valid and formed for the bound revision.
func (v AcceptanceVerdict) Usable() bool { return v.Known && v.Code == "" }

// Check returns nil for a usable acceptance and its refusal at path of the caller's request
// otherwise: REFERENCE_INVALID, ACCEPTANCE_SCOPE_MISMATCH, ACCEPTANCE_NOT_VALID (409) or
// ACCEPTANCE_LATE.
func (v AcceptanceVerdict) Check(path string) error {
	switch {
	case v.Usable():
		return nil
	case !v.Known || v.Code == "":
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	return refuse(v.Code, path)
}
