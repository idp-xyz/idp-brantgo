package api

import (
	"context"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// QualityReleaseRef v1 and QualityReleaseGuard v1 (Context Map R26 QLT→FUL, OHS with guardLock:
// 有效质量放行（数量、范围、有效期）; risk 放行撤回后仍出运; QLT-INV-002; ADR-BR-014). FUL reads a
// release before its own transaction through ReleaseLookups and binds it by ReleaseID; inside its
// transaction, after it locked its own roots FOR UPDATE (ADR-BR-011), it re-checks the release with
// QualityReleaseGuard v1 and keeps it so until that transaction ends. FUL keeps its own consumption
// of each release (its quantity less what FUL shipped on it): QLT knows nothing of shipments.
//
// QualityReleaseGuard v1 is a SECURITY DEFINER function in QLT's schema with a fixed search_path
// (migrations/qlt/000001_qlt_quality.sql):
//
//	brant_qlt.guard_quality_release_v1(p_release_id text, p_at timestamptz)
//	    RETURNS TABLE (known boolean, status text, subject_kind text, subject_id text, subject_line_id text,
//	                   supplier_id text, order_id text, order_line_id text, sku_id text, spec_revision bigint,
//	                   unit_id text, unit_revision bigint, released_quantity text, valid_from timestamptz,
//	                   valid_until timestamptz, code text)
//
// It always returns one row in the caller's scope (app.tenant_id and app.company_id) and locks the
// one release row FOR SHARE in the caller's transaction. quality_release.revoke locks that row FOR
// UPDATE and changes it, so a revocation either committed before the guard read the row, and the
// guard answers QUALITY_RELEASE_REQUIRED, or waits until the caller's transaction ends; a guard
// called while a revocation holds the row waits for it and reads the release as it left it. The
// guard locks nothing else, and QLT never locks a FUL row. A caller locks its own root first:
//
//	ref := qltapi.QualityReleaseRef{ReleaseID: id}
//	row := exec.QueryRow(ctx, qltapi.QualityReleaseGuardSQL, qltapi.QualityReleaseGuardArgs(ref, at)...)
//	verdict, err := qltapi.ScanQualityReleaseGuard(ref, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/lines/0/quality_release_id")
//	}
//
// Code is empty for a granted release whose validity [valid_from, valid_until) contains p_at, and
// QUALITY_RELEASE_REQUIRED (409) for an unknown release (known false, every other column NULL), a
// revoked one, and one not yet or no longer valid at p_at. The guard does not apply the caller's
// data scope (own, team): the caller checks visibility when it resolves the release before its
// transaction (ReleaseLookups).
const (
	QualityReleaseGuardFunction = "brant_qlt.guard_quality_release_v1"
	QualityReleaseGuardSQL      = `SELECT known, status, subject_kind, subject_id, subject_line_id, supplier_id, order_id, order_line_id,
		sku_id, spec_revision, unit_id, unit_revision, released_quantity, valid_from, valid_until, code
		FROM brant_qlt.guard_quality_release_v1($1, $2)`
)

// QualityReleaseRef is QualityReleaseRef v1: a release by its ID. A release never changes except by
// its revocation, so its ID is its binding.
type QualityReleaseRef struct {
	ReleaseID string
}

// QualityReleaseGuardArgs returns the arguments of QualityReleaseGuardSQL for ref at instant at.
func QualityReleaseGuardArgs(ref QualityReleaseRef, at time.Time) []any {
	return []any{ref.ReleaseID, kernel.Instant(at)}
}

// ReleaseScope is what a release covers: the subject (a PUR commitment line), its supplier and the
// sales order line it serves, the SKU specification revision and the unit of Quantity.
type ReleaseScope struct {
	Subject      SubjectRef
	SupplierID   string
	OrderID      string
	OrderLineID  string
	SkuID        string
	SpecRevision int64
	UnitID       string
	UnitRevision int64
}

// QualityReleaseVerdict is the guard's answer for Ref: Known false for a release that does not
// exist in the caller's company; otherwise its status, scope, released quantity and validity, and
// Code ("" when it serves a shipment at the instant asked).
type QualityReleaseVerdict struct {
	Ref        QualityReleaseRef
	Known      bool
	Status     string
	Scope      ReleaseScope
	Quantity   string
	ValidFrom  time.Time
	ValidUntil time.Time
	Code       string
}

// ScanQualityReleaseGuard reads the row of QualityReleaseGuardSQL for ref through scan, such as a
// pgx row's Scan.
func ScanQualityReleaseGuard(ref QualityReleaseRef, scan func(dest ...any) error) (QualityReleaseVerdict, error) {
	v := QualityReleaseVerdict{Ref: ref}
	var status, kind, id, line, supplier, order, orderLine, sku, unit, quantity, code *string
	var spec, unitRev *int64
	var from, until *time.Time
	if err := scan(&v.Known, &status, &kind, &id, &line, &supplier, &order, &orderLine, &sku, &spec, &unit, &unitRev, &quantity, &from, &until,
		&code); err != nil {
		return QualityReleaseVerdict{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{status, &v.Status}, {kind, &v.Scope.Subject.Kind}, {id, &v.Scope.Subject.ID}, {line, &v.Scope.Subject.LineID},
		{supplier, &v.Scope.SupplierID}, {order, &v.Scope.OrderID}, {orderLine, &v.Scope.OrderLineID}, {sku, &v.Scope.SkuID},
		{unit, &v.Scope.UnitID}, {quantity, &v.Quantity}, {code, &v.Code}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	if spec != nil {
		v.Scope.SpecRevision = *spec
	}
	if unitRev != nil {
		v.Scope.UnitRevision = *unitRev
	}
	if from != nil {
		v.ValidFrom = kernel.Instant(*from)
	}
	if until != nil {
		v.ValidUntil = kernel.Instant(*until)
	}
	return v, nil
}

// Usable reports whether the release serves a shipment at the instant the guard was asked.
func (v QualityReleaseVerdict) Usable() bool { return v.Known && v.Code == "" }

// Check returns nil for a usable release and QUALITY_RELEASE_REQUIRED (409) at path of the caller's
// request otherwise.
func (v QualityReleaseVerdict) Check(path string) error {
	if v.Usable() {
		return nil
	}
	reason := "The quality release is unknown"
	switch {
	case !v.Known:
	case v.Status == StatusRevoked:
		reason = "The quality release was revoked"
	default:
		reason = "The quality release is not valid at this instant"
	}
	return Reject(CodeQualityReleaseRequired, path, reason)
}

// ResolvedRelease is what QLT says of one release at an instant (QualityReleaseRef v1): its status
// and state then (granted, not_yet_valid, expired or revoked), row version, scope, quantity and
// validity, and Code as the guard decides it.
type ResolvedRelease struct {
	Ref        QualityReleaseRef
	Status     string
	State      string
	RowVersion int64
	Scope      ReleaseScope
	Quantity   string
	ValidFrom  time.Time
	ValidUntil time.Time
	Code       string
}

// Check returns nil when the release serves a shipment at the instant asked and
// QUALITY_RELEASE_REQUIRED at path otherwise.
func (r ResolvedRelease) Check(path string) error {
	if r.Code == "" {
		return nil
	}
	return Reject(CodeQualityReleaseRequired, path, "The quality release is "+r.State)
}

// ReleaseLookups answer QualityReleaseRef v1 lookups of the contexts downstream of QLT (FUL, R26).
// They need no permission beyond an active grant; releases outside the caller's company or data
// scope are absent. Ask before opening your own transaction; inside it, use QualityReleaseGuard v1.
type ReleaseLookups interface {
	// ResolveReleases answers at most MaxLookup release IDs at instant at.
	ResolveReleases(ctx context.Context, releaseIDs []string, at time.Time) (map[QualityReleaseRef]ResolvedRelease, error)
}

// ValidateReleaseIDs checks a lookup's release IDs.
func ValidateReleaseIDs(ids []string) error {
	if len(ids) > MaxLookup {
		return schema("/release_ids")
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) {
			return schema("/release_ids/" + strconv.Itoa(i))
		}
	}
	return nil
}
