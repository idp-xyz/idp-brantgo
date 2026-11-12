package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	db "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/documentbundle"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Document bundles, their checks and reconciliations (migrations/cnx/000004_cnx_declarations_bundles.sql).

const bundleColumns = `bundle_id, export_lot_id, order_id, status, checks, reconciliations, row_version, opened_by_kind, opened_by_id,
	authority_epoch, opened_at`

func scanBundle(row pgx.Row) (*db.Bundle, error) {
	var b db.Bundle
	var kind string
	err := row.Scan(&b.ID, &b.ExportLotID, &b.OrderID, &b.Status, &b.Checks, &b.Reconciliations, &b.RowVersion, &kind, &b.OpenedBy.ID, &b.Epoch,
		&b.OpenedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.OpenedBy.Kind, b.OpenedAt = kernel.ActorKind(kind), kernel.Instant(b.OpenedAt)
	return &b, nil
}

// LoadBundle returns a bundle by ID, or nil, taking lock.
func LoadBundle(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*db.Bundle, error) {
	return scanBundle(exec.QueryRow(ctx, `SELECT `+bundleColumns+` FROM brant_cnx.document_bundle
		WHERE tenant_id = $1 AND company_id = $2 AND bundle_id = $3`+string(lock), scoped(scope, id)...))
}

// BundleOfLot returns the bundle of an export lot, or nil, taking lock.
func BundleOfLot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, lotID string, lock Lock) (*db.Bundle, error) {
	return scanBundle(exec.QueryRow(ctx, `SELECT `+bundleColumns+` FROM brant_cnx.document_bundle
		WHERE tenant_id = $1 AND company_id = $2 AND export_lot_id = $3`+string(lock), scoped(scope, lotID)...))
}

// Bundles returns the scope's bundles, newest first.
func Bundles(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) ([]*db.Bundle, error) {
	rows, err := exec.Query(ctx, `SELECT `+bundleColumns+` FROM brant_cnx.document_bundle WHERE tenant_id = $1 AND company_id = $2
		ORDER BY opened_at DESC, bundle_id`, scoped(scope)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*db.Bundle
	for rows.Next() {
		b, err := scanBundle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// InsertBundle opens bundle b with its first check at row version 1.
func InsertBundle(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, b *db.Bundle) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cnx.document_bundle (tenant_id, company_id, bundle_id, export_lot_id, order_id, status, checks,
		reconciliations, row_version, opened_by_kind, opened_by_id, authority_epoch, opened_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 1, 0, 1, $7, $8, $9, $10, $10)`,
		scoped(scope, b.ID, b.ExportLotID, b.OrderID, b.Status, string(b.OpenedBy.Kind), b.OpenedBy.ID, b.Epoch, kernel.Instant(b.OpenedAt))...)
	return err
}

// SaveBundle moves bundle b from row version from to what it holds now.
func SaveBundle(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, b *db.Bundle, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cnx.document_bundle SET status = $4, checks = $5, reconciliations = $6, row_version = $7, updated_at = $8
		WHERE tenant_id = $1 AND company_id = $2 AND bundle_id = $3 AND row_version = $9`,
		scoped(scope, b.ID, b.Status, b.Checks, b.Reconciliations, b.RowVersion, kernel.Instant(at), from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// InsertBundleCheck appends check c of its bundle.
func InsertBundleCheck(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *db.Sealed, by kernel.ActorRef, epoch int64, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cnx.document_bundle_check (tenant_id, company_id, bundle_id, check_no, document, document_sha256,
		complete, missing, present, verified, checked_by_kind, checked_by_id, authority_epoch, checked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		scoped(scope, c.BundleID, c.CheckNo, string(c.Bytes), c.SHA256, c.Status == db.Complete, c.Missing, c.Present, c.Verified, string(by.Kind),
			by.ID, epoch, kernel.Instant(at))...)
	return err
}

// BundleChecks returns the checks of bundle id, oldest first. A document that does not match its
// stored hash is REFERENCE_INVALID.
func BundleChecks(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*db.Sealed, error) {
	rows, err := exec.Query(ctx, `SELECT document, document_sha256 FROM brant_cnx.document_bundle_check WHERE tenant_id = $1 AND company_id = $2
		AND bundle_id = $3 ORDER BY check_no`, scoped(scope, id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*db.Sealed
	for rows.Next() {
		var doc, hash string
		if err := rows.Scan(&doc, &hash); err != nil {
			return nil, err
		}
		s, ok, err := db.Unseal([]byte(doc), hash)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/id", "A bundle check is not intact")
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReconciliationExists reports whether a reconciliation has the ID.
func ReconciliationExists(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (bool, error) {
	var n int
	err := exec.QueryRow(ctx, `SELECT count(*) FROM brant_cnx.external_reconciliation WHERE tenant_id = $1 AND company_id = $2
		AND reconciliation_id = $3`, scoped(scope, id)...).Scan(&n)
	return n > 0, err
}

// InsertReconciliation appends reconciliation r.
func InsertReconciliation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *db.Reconciliation) error {
	evidence := r.Evidence
	if evidence == nil {
		evidence = []db.Evidence{}
	}
	b, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.external_reconciliation (tenant_id, company_id, reconciliation_id, bundle_id, reconciliation_no,
		subject_kind, subject_id, measure, unit, external_value, internal_value, difference, external_source, internal_source, responsibility_kind,
		responsibility_party_id, responsibility_note, adjustment, adjustment_note, evidence, reason, resolved_by_kind, resolved_by_id,
		authority_epoch, resolved_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::numeric, $11::numeric, $12::numeric, $13, $14, $15,
		nullif($16, ''), $17, $18, nullif($19, ''), $20, $21, $22, $23, $24, $25)`,
		scoped(scope, r.ID, r.BundleID, r.No, r.SubjectKind, r.SubjectID, r.Measure, r.Unit, r.External.String(), r.Internal.String(),
			r.Difference.String(), r.ExternalSource, r.InternalSource, r.ResponsibilityKind, r.ResponsibilityPartyID, r.ResponsibilityNote,
			r.Adjustment, r.AdjustmentNote, string(b), r.Reason, string(r.By.Kind), r.By.ID, r.Epoch, kernel.Instant(r.At))...)
	return err
}

// Reconciliations returns the reconciliations of bundle id, oldest first.
func Reconciliations(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*db.Reconciliation, error) {
	rows, err := exec.Query(ctx, `SELECT reconciliation_id, reconciliation_no, subject_kind, subject_id, measure, unit, external_value::text,
		internal_value::text, difference::text, external_source, internal_source, responsibility_kind, coalesce(responsibility_party_id, ''),
		responsibility_note, adjustment, coalesce(adjustment_note, ''), evidence, reason, resolved_by_kind, resolved_by_id, authority_epoch,
		resolved_at FROM brant_cnx.external_reconciliation WHERE tenant_id = $1 AND company_id = $2 AND bundle_id = $3 ORDER BY reconciliation_no`,
		scoped(scope, id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*db.Reconciliation
	for rows.Next() {
		r := db.Reconciliation{BundleID: id}
		var external, internal, difference, evidence, kind string
		if err := rows.Scan(&r.ID, &r.No, &r.SubjectKind, &r.SubjectID, &r.Measure, &r.Unit, &external, &internal, &difference, &r.ExternalSource,
			&r.InternalSource, &r.ResponsibilityKind, &r.ResponsibilityPartyID, &r.ResponsibilityNote, &r.Adjustment, &r.AdjustmentNote, &evidence,
			&r.Reason, &kind, &r.By.ID, &r.Epoch, &r.At); err != nil {
			return nil, err
		}
		for _, x := range []struct {
			s  string
			to *kernel.Decimal
		}{{external, &r.External}, {internal, &r.Internal}, {difference, &r.Difference}} {
			d, err := signedDecimal(x.s)
			if err != nil {
				return nil, err
			}
			*x.to = d
		}
		if err := json.Unmarshal([]byte(evidence), &r.Evidence); err != nil {
			return nil, err
		}
		r.By.Kind, r.At = kernel.ActorKind(kind), kernel.Instant(r.At)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// signedDecimal reads a numeric's text, which may be negative and carry trailing zeros.
func signedDecimal(s string) (kernel.Decimal, error) {
	neg := len(s) > 0 && s[0] == '-'
	if neg {
		s = s[1:]
	}
	d, err := decimal(s)
	if err != nil {
		return kernel.Decimal{}, err
	}
	if neg {
		d = d.Neg()
	}
	return d, nil
}
