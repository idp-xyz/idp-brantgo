package api

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// QuoteRevisionGuard v1 (ADR-BR-004 option B, ADR-BR-011) lets a step that relies on a formal quote
// revision — TRD's own customer acceptance and order steps, or a context downstream of TRD — re-check
// inside its own transaction that the revision is still the quote's active, unheld revision at the
// business hash it binds, and whether the root's conversion slot is taken, and keeps it so until
// that transaction ends. TRD publishes it as a SECURITY DEFINER function in its schema with a fixed
// search_path (migrations/trd/000003_trd_quote_formalization.sql):
//
//	brant_trd.guard_quote_revision_active_v1(p_quote_id text, p_revision bigint, p_business_hash text)
//	    RETURNS TABLE (known boolean, status text, active_revision bigint, business_hash text, number text,
//	                   content_sha256 text, expires_at timestamptz, on_hold boolean, conversion_taken boolean, code text)
//
// It always returns one row in the caller's scope (app.tenant_id and app.company_id) and locks the
// quote root FOR SHARE in the caller's transaction. quote.replace, quote.cancel,
// quote.approval.revoke (which registers a risk hold) and the order step that takes the conversion
// slot lock the root FOR UPDATE, so each either committed before the guard read it, and the verdict
// says so, or waits until the caller's transaction ends. A caller locks its own aggregate root FOR
// UPDATE first; TRD's own order step locks the quote root FOR UPDATE itself and decides with the
// aggregate (quote.TakeSlot), the guard being the same answer for callers that do not own the root:
//
//	row := exec.QueryRow(ctx, trdapi.QuoteRevisionGuardSQL, trdapi.QuoteRevisionGuardArgs(ref)...)
//	verdict, err := trdapi.ScanQuoteRevisionGuard(ref, row.Scan)
//	if err == nil {
//		err = verdict.CheckConvertible("/args/quote_revision_ref")
//	}
//
// The guard does not apply the caller's data scope (own, team): the caller checks visibility when it
// resolves the reference before its transaction (Lookups).
const (
	QuoteRevisionGuardFunction = "brant_trd.guard_quote_revision_active_v1"
	QuoteRevisionGuardSQL      = `SELECT known, status, active_revision, business_hash, number, content_sha256, expires_at, on_hold,
		conversion_taken, code FROM brant_trd.guard_quote_revision_active_v1($1, $2, $3)`
)

// QuoteRevisionBinding is a formal revision as a downstream fact binds it: the revision and its
// business hash.
type QuoteRevisionBinding struct {
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
}

// QuoteRevisionGuardArgs returns the arguments of QuoteRevisionGuardSQL for b.
func QuoteRevisionGuardArgs(b QuoteRevisionBinding) []any {
	return []any{b.QuoteID, b.BusinessRevision, b.BusinessHash}
}

// QuoteRevisionVerdict is the guard's answer for Ref. Known is false, with Code REFERENCE_INVALID,
// for a quote or revision that does not exist in the caller's company or a revision never
// formalized. Otherwise Status is the revision's, ActiveRevision the root's active revision (0:
// none), Number and DocumentSHA256 those of its formal document, ExpiresAt its validity end, and
// Code empty when the revision is active at the bound business hash and unheld,
// BUSINESS_HASH_MISMATCH for another hash, QUOTE_NOT_CURRENT when it is not the active revision,
// and the risk hold's code (APPROVAL_REVOKED) while a hold pauses it. ConversionTaken says whether
// the root's conversion slot is taken.
type QuoteRevisionVerdict struct {
	Ref             QuoteRevisionBinding
	Known           bool
	Status          string
	ActiveRevision  int64
	BusinessHash    string
	Number          string
	DocumentSHA256  string
	ExpiresAt       time.Time
	OnHold          bool
	ConversionTaken bool
	Code            string
}

// ScanQuoteRevisionGuard reads the row of QuoteRevisionGuardSQL for ref through scan, such as a pgx
// row's Scan.
func ScanQuoteRevisionGuard(ref QuoteRevisionBinding, scan func(dest ...any) error) (QuoteRevisionVerdict, error) {
	v := QuoteRevisionVerdict{Ref: ref}
	var status, hash, number, sha, code *string
	var active *int64
	var expires *time.Time
	if err := scan(&v.Known, &status, &active, &hash, &number, &sha, &expires, &v.OnHold, &v.ConversionTaken, &code); err != nil {
		return QuoteRevisionVerdict{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{status, &v.Status}, {hash, &v.BusinessHash}, {number, &v.Number}, {sha, &v.DocumentSHA256}, {code, &v.Code}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	if active != nil {
		v.ActiveRevision = *active
	}
	if expires != nil {
		v.ExpiresAt = kernel.Instant(*expires)
	}
	return v, nil
}

// Usable reports whether the revision is the active, unheld revision at the bound hash.
func (v QuoteRevisionVerdict) Usable() bool { return v.Known && v.Code == "" }

// Check returns nil for a usable revision and its refusal at path of the caller's request
// otherwise: REFERENCE_INVALID, BUSINESS_HASH_MISMATCH, QUOTE_NOT_CURRENT (409) or the hold's code.
// Whether the caller's instant is still before ExpiresAt is the caller's to judge.
func (v QuoteRevisionVerdict) Check(path string) error {
	switch {
	case v.Usable():
		return nil
	case !v.Known || v.Code == "":
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	return refuse(v.Code, path)
}

// CheckConvertible is Check, and CONVERSION_SLOT_CONFLICT (409) when the conversion slot is taken.
func (v QuoteRevisionVerdict) CheckConvertible(path string) error {
	if err := v.Check(path); err != nil {
		return err
	}
	if v.ConversionTaken {
		return Reject(CodeConversionSlotConflict, path)
	}
	return nil
}
