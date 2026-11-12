package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cert "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/certificate"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Certificate results and their revisions (migrations/cnx/000005_cnx_certificates.sql).

const certificateColumns = `certificate_id, kind, issuer, number, current_revision, row_version, registered_by_kind, registered_by_id,
	authority_epoch, registered_at, updated_at`

func scanCertificate(row pgx.Row) (*cert.Certificate, error) {
	var c cert.Certificate
	var kind string
	err := row.Scan(&c.ID, &c.Kind, &c.Issuer, &c.Number, &c.CurrentRevision, &c.RowVersion, &kind, &c.RegisteredBy.ID, &c.Epoch,
		&c.RegisteredAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.RegisteredBy.Kind, c.RegisteredAt, c.UpdatedAt = kernel.ActorKind(kind), kernel.Instant(c.RegisteredAt), kernel.Instant(c.UpdatedAt)
	return &c, nil
}

// LoadCertificate returns a certificate by ID, or nil, taking lock.
func LoadCertificate(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*cert.Certificate, error) {
	return scanCertificate(exec.QueryRow(ctx, `SELECT `+certificateColumns+` FROM brant_cnx.certificate_result
		WHERE tenant_id = $1 AND company_id = $2 AND certificate_id = $3`+string(lock), scoped(scope, id)...))
}

// CertificateByKey returns the ID of the certificate whose current revision is the issuer's number of
// the kind, or "".
func CertificateByKey(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, kind, issuer, number string) (string, error) {
	var id string
	err := exec.QueryRow(ctx, `SELECT certificate_id FROM brant_cnx.certificate_result WHERE tenant_id = $1 AND company_id = $2
		AND kind = $3 AND issuer = $4 AND number = $5`, scoped(scope, kind, issuer, number)...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Certificates returns the scope's certificates, newest first.
func Certificates(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) ([]*cert.Certificate, error) {
	rows, err := exec.Query(ctx, `SELECT `+certificateColumns+` FROM brant_cnx.certificate_result WHERE tenant_id = $1 AND company_id = $2
		ORDER BY registered_at DESC, certificate_id`, scoped(scope)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*cert.Certificate
	for rows.Next() {
		c, err := scanCertificate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// InsertCertificate opens certificate c at revision 1; its revision follows in the same transaction.
func InsertCertificate(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *cert.Certificate) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cnx.certificate_result (tenant_id, company_id, certificate_id, kind, issuer, number,
		current_revision, row_version, registered_by_kind, registered_by_id, authority_epoch, registered_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 1, 1, $7, $8, $9, $10, $10)`,
		scoped(scope, c.ID, c.Kind, c.Issuer, c.Number, string(c.RegisteredBy.Kind), c.RegisteredBy.ID, c.Epoch, kernel.Instant(c.RegisteredAt))...)
	return err
}

// SaveCertificate moves certificate c from row version from to its current revision.
func SaveCertificate(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *cert.Certificate, from int64) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cnx.certificate_result SET issuer = $4, number = $5, current_revision = $6, row_version = $7,
		updated_at = $8 WHERE tenant_id = $1 AND company_id = $2 AND certificate_id = $3 AND row_version = $9`,
		scoped(scope, c.ID, c.Issuer, c.Number, c.CurrentRevision, c.RowVersion, kernel.Instant(c.UpdatedAt), from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// InsertCertificateRevision appends revision r of its certificate.
func InsertCertificateRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *cert.Revision) error {
	var objectID, license, reason *string
	if r.Object.ID != "" {
		objectID = &r.Object.ID
	}
	if r.LicenseID != "" {
		license = &r.LicenseID
	}
	if r.Reason != "" {
		reason = &r.Reason
	}
	at, err := kernel.ParseTimestamp(r.RegisteredAt)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.certificate_revision (tenant_id, company_id, certificate_id, revision, kind, issuer, number,
		issued_on, object_kind, object_id, market, valid_from, valid_until, file_id, file_sha256, license_id, document, document_sha256, reason,
		verified_by_kind, verified_by_id, authority_epoch, registered_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::date, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)`,
		scoped(scope, r.CertificateID, r.Revision, r.Kind, r.Issuer, r.Number, r.IssuedOn, r.Object.Kind, objectID, r.Market,
			r.Validity.From, r.Validity.Until, r.File.SourceID, r.File.ContentSHA256, license, string(r.Bytes), r.SHA256, reason,
			r.VerifiedBy.Kind, r.VerifiedBy.ID, r.AuthorityEpoch, kernel.Instant(at))...)
	return err
}

// CertificateRevisions returns the revisions of certificate id, oldest first. A document that does not
// match its stored hash is REFERENCE_INVALID.
func CertificateRevisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*cert.Revision, error) {
	rows, err := exec.Query(ctx, `SELECT document, document_sha256 FROM brant_cnx.certificate_revision WHERE tenant_id = $1 AND company_id = $2
		AND certificate_id = $3 ORDER BY revision`, scoped(scope, id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*cert.Revision
	for rows.Next() {
		var doc, hash string
		if err := rows.Scan(&doc, &hash); err != nil {
			return nil, err
		}
		r, ok, err := cert.Unseal([]byte(doc), hash)
		if err != nil {
			return nil, err
		}
		if !ok || r.CertificateID != id {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/id", "A certificate revision is not intact")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
