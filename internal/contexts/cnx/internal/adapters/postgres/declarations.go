package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	dcl "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/declaration"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Declaration cases, their versions, confirmations, submissions and receipts
// (migrations/cnx/000004_cnx_declarations_bundles.sql).

// ForShare is the lock a command takes on another root of CNX it relies on without changing it (a
// tax refund allocation on the lot's declaration, for its export date).
const ForShare Lock = " FOR SHARE"

const declarationColumns = `declaration_id, export_lot_id, attempt, shipment_id, order_id, order_revision, order_hash, code_table_version,
	status, current_version, mapping, coalesce(declaration_number, ''), exported_at, coalesce(last_sequence, 0), submissions, receipts,
	row_version, opened_by_kind, opened_by_id, authority_epoch, opened_at`

func scanDeclaration(row pgx.Row) (*dcl.Case, error) {
	var c dcl.Case
	var kind string
	var exported *time.Time
	err := row.Scan(&c.ID, &c.ExportLotID, &c.Attempt, &c.ShipmentID, &c.OrderID, &c.OrderRevision, &c.OrderHash, &c.CodeTableVersion, &c.Status,
		&c.Version, &c.Mapping, &c.DeclarationNumber, &exported, &c.LastSequence, &c.Submissions, &c.Receipts, &c.RowVersion, &kind, &c.OpenedBy.ID,
		&c.Epoch, &c.OpenedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if exported != nil {
		c.ExportedAt = kernel.Instant(*exported)
	}
	c.OpenedBy.Kind, c.OpenedAt = kernel.ActorKind(kind), kernel.Instant(c.OpenedAt)
	return &c, nil
}

// LoadDeclaration returns a case by ID, or nil, taking lock.
func LoadDeclaration(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*dcl.Case, error) {
	return scanDeclaration(exec.QueryRow(ctx, `SELECT `+declarationColumns+` FROM brant_cnx.declaration_case
		WHERE tenant_id = $1 AND company_id = $2 AND declaration_id = $3`+string(lock), scoped(scope, id)...))
}

// DeclarationOfLot returns the latest case of an export lot (its highest attempt), or nil, taking
// lock.
func DeclarationOfLot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, lotID string, lock Lock) (*dcl.Case, error) {
	return scanDeclaration(exec.QueryRow(ctx, `SELECT `+declarationColumns+` FROM brant_cnx.declaration_case
		WHERE tenant_id = $1 AND company_id = $2 AND export_lot_id = $3 ORDER BY attempt DESC LIMIT 1`+string(lock), scoped(scope, lotID)...))
}

// DeclarationsOfShipment returns the latest case of every export lot of a shipment, by lot.
func DeclarationsOfShipment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, shipmentID string) ([]*dcl.Case, error) {
	return queryDeclarations(ctx, exec, `SELECT DISTINCT ON (export_lot_id) `+declarationColumns+` FROM brant_cnx.declaration_case
		WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3 ORDER BY export_lot_id, attempt DESC`, scoped(scope, shipmentID)...)
}

// Declarations returns the scope's cases, newest first.
func Declarations(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) ([]*dcl.Case, error) {
	return queryDeclarations(ctx, exec, `SELECT `+declarationColumns+` FROM brant_cnx.declaration_case WHERE tenant_id = $1 AND company_id = $2
		ORDER BY opened_at DESC, declaration_id`, scoped(scope)...)
}

func queryDeclarations(ctx context.Context, exec bentopg.Executor, sql string, args ...any) ([]*dcl.Case, error) {
	rows, err := exec.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dcl.Case
	for rows.Next() {
		c, err := scanDeclaration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	i := kernel.Instant(t)
	return &i
}

// InsertDeclaration opens case c as a draft at row version 1.
func InsertDeclaration(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *dcl.Case) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cnx.declaration_case (tenant_id, company_id, declaration_id, export_lot_id, attempt, shipment_id,
		order_id, order_revision, order_hash, code_table_version, status, current_version, mapping, submissions, receipts, row_version,
		opened_by_kind, opened_by_id, authority_epoch, opened_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'draft', 1, $11, 0, 0, 1, $12, $13, $14, $15, $15)`,
		scoped(scope, c.ID, c.ExportLotID, c.Attempt, c.ShipmentID, c.OrderID, c.OrderRevision, c.OrderHash, c.CodeTableVersion, c.Mapping,
			string(c.OpenedBy.Kind), c.OpenedBy.ID, c.Epoch, kernel.Instant(c.OpenedAt))...)
	return err
}

// SaveDeclaration moves case c from row version from to what it holds now.
func SaveDeclaration(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *dcl.Case, from int64, at time.Time) error {
	var number *string
	if c.DeclarationNumber != "" {
		number = &c.DeclarationNumber
	}
	var sequence *int64
	if c.LastSequence > 0 {
		sequence = &c.LastSequence
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_cnx.declaration_case SET status = $4, current_version = $5, mapping = $6, declaration_number = $7,
		exported_at = $8, last_sequence = $9, submissions = $10, receipts = $11, row_version = $12, updated_at = $13
		WHERE tenant_id = $1 AND company_id = $2 AND declaration_id = $3 AND row_version = $14`,
		scoped(scope, c.ID, c.Status, c.Version, c.Mapping, number, optionalTime(c.ExportedAt), sequence, c.Submissions, c.Receipts, c.RowVersion,
			kernel.Instant(at), from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// InsertVersion appends version v of its case, made by by at epoch at instant at.
func InsertVersion(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, v *dcl.Version, by kernel.ActorRef, epoch int64, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cnx.declaration_version (tenant_id, company_id, declaration_id, version_no, kind, mapping, document,
		document_sha256, receipt_id, created_by_kind, created_by_id, authority_epoch, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''), $10, $11, $12, $13)`,
		scoped(scope, v.DeclarationID, v.Document.Version, v.Kind, v.Mapping, string(v.Bytes), v.SHA256, v.ReceiptID, string(by.Kind), by.ID, epoch,
			kernel.Instant(at))...)
	return err
}

// Versions returns the versions of case id, oldest first; with version > 0 only that one. A
// document that does not match its stored hash is REFERENCE_INVALID.
func Versions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, version int) ([]*dcl.Version, error) {
	rows, err := exec.Query(ctx, `SELECT document, document_sha256 FROM brant_cnx.declaration_version WHERE tenant_id = $1 AND company_id = $2
		AND declaration_id = $3 AND ($4 = 0 OR version_no = $4) ORDER BY version_no`, scoped(scope, id, version)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dcl.Version
	for rows.Next() {
		var doc, hash string
		if err := rows.Scan(&doc, &hash); err != nil {
			return nil, err
		}
		v, err := dcl.ParseVersion([]byte(doc), hash)
		if errors.Is(err, dcl.ErrDocumentNotIntact) {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/id", "A declaration version is not intact")
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func evidenceText(e []dcl.Evidence) (string, error) {
	if e == nil {
		e = []dcl.Evidence{}
	}
	b, err := json.Marshal(e)
	return string(b), err
}

// InsertConfirmation appends a confirmation.
func InsertConfirmation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *dcl.Confirmation) error {
	evidence, err := evidenceText(c.Evidence)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.declaration_confirmation (tenant_id, company_id, declaration_id, version_no, evidence, reason,
		confirmed_by_kind, confirmed_by_id, authority_epoch, confirmed_at) VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7, $8, $9, $10)`,
		scoped(scope, c.DeclarationID, c.Version, evidence, c.Reason, string(c.By.Kind), c.By.ID, c.Epoch, kernel.Instant(c.At))...)
	return err
}

// Confirmations returns the confirmations of case id, oldest first.
func Confirmations(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*dcl.Confirmation, error) {
	rows, err := exec.Query(ctx, `SELECT version_no, evidence, coalesce(reason, ''), confirmed_by_kind, confirmed_by_id, authority_epoch, confirmed_at
		FROM brant_cnx.declaration_confirmation WHERE tenant_id = $1 AND company_id = $2 AND declaration_id = $3 ORDER BY version_no`,
		scoped(scope, id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dcl.Confirmation
	for rows.Next() {
		c := dcl.Confirmation{DeclarationID: id}
		var evidence, kind string
		if err := rows.Scan(&c.Version, &evidence, &c.Reason, &kind, &c.By.ID, &c.Epoch, &c.At); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(evidence), &c.Evidence); err != nil {
			return nil, err
		}
		c.By.Kind, c.At = kernel.ActorKind(kind), kernel.Instant(c.At)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// OperationTaken reports whether a submission of the scope used the operation reference.
func OperationTaken(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, reference string) (bool, error) {
	var n int
	err := exec.QueryRow(ctx, `SELECT count(*) FROM brant_cnx.declaration_submission WHERE tenant_id = $1 AND company_id = $2
		AND operation_reference = $3`, scoped(scope, reference)...).Scan(&n)
	return n > 0, err
}

// InsertSubmission appends a submission.
func InsertSubmission(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *dcl.Submission) error {
	evidence, err := evidenceText(s.Evidence)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.declaration_submission (tenant_id, company_id, declaration_id, submission_no, version_no,
		operation_reference, submitter_kind, submitter_party_id, submitter_name, submitted_at, evidence, note, recorded_by_kind, recorded_by_id,
		authority_epoch, recorded_at) VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), $9, $10, $11, nullif($12, ''), $13, $14, $15, $16)`,
		scoped(scope, s.DeclarationID, s.No, s.Version, s.OperationReference, s.SubmitterKind, s.SubmitterPartyID, s.SubmitterName,
			kernel.Instant(s.SubmittedAt), evidence, s.Note, string(s.By.Kind), s.By.ID, s.Epoch, kernel.Instant(s.At))...)
	return err
}

// Submissions returns the submissions of case id, oldest first.
func Submissions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*dcl.Submission, error) {
	rows, err := exec.Query(ctx, `SELECT submission_no, version_no, operation_reference, submitter_kind, coalesce(submitter_party_id, ''),
		submitter_name, submitted_at, evidence, coalesce(note, ''), recorded_by_kind, recorded_by_id, authority_epoch, recorded_at
		FROM brant_cnx.declaration_submission WHERE tenant_id = $1 AND company_id = $2 AND declaration_id = $3 ORDER BY submission_no`,
		scoped(scope, id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dcl.Submission
	for rows.Next() {
		s := dcl.Submission{DeclarationID: id}
		var evidence, kind string
		if err := rows.Scan(&s.No, &s.Version, &s.OperationReference, &s.SubmitterKind, &s.SubmitterPartyID, &s.SubmitterName, &s.SubmittedAt,
			&evidence, &s.Note, &kind, &s.By.ID, &s.Epoch, &s.At); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(evidence), &s.Evidence); err != nil {
			return nil, err
		}
		s.By.Kind, s.SubmittedAt, s.At = kernel.ActorKind(kind), kernel.Instant(s.SubmittedAt), kernel.Instant(s.At)
		out = append(out, &s)
	}
	return out, rows.Err()
}

// InsertReceipt appends a receipt.
func InsertReceipt(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *dcl.Receipt) error {
	evidence, err := evidenceText(r.Evidence)
	if err != nil {
		return err
	}
	var sequence *int64
	if r.Sequence > 0 {
		sequence = &r.Sequence
	}
	var version *int
	if r.Version > 0 {
		version = &r.Version
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.declaration_receipt (tenant_id, company_id, receipt_id, declaration_id, receipt_no, outcome,
		from_status, external_sequence, external_status, original_text, mapping_version, received_at, declaration_number, exported_at, version_no,
		evidence, reason, recorded_by_kind, recorded_by_id, authority_epoch, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''), nullif($10, ''), nullif($11, ''), $12, nullif($13, ''), $14, $15, $16,
		nullif($17, ''), $18, $19, $20, $21)`,
		scoped(scope, r.ID, r.DeclarationID, r.No, r.Outcome, r.From, sequence, r.StatusCode, r.OriginalText, r.MappingVersion,
			optionalTime(r.ReceivedAt), r.DeclarationNumber, optionalTime(r.ExportedAt), version, evidence, r.Reason, string(r.By.Kind), r.By.ID,
			r.Epoch, kernel.Instant(r.At))...)
	return err
}

const receiptColumns = `receipt_id, declaration_id, receipt_no, outcome, from_status, coalesce(external_sequence, 0), coalesce(external_status, ''),
	coalesce(original_text, ''), coalesce(mapping_version, ''), received_at, coalesce(declaration_number, ''), exported_at,
	coalesce(version_no, 0), evidence, coalesce(reason, ''), recorded_by_kind, recorded_by_id, authority_epoch, recorded_at`

func scanReceipt(row pgx.Row) (*dcl.Receipt, error) {
	var r dcl.Receipt
	var received, exported *time.Time
	var evidence, kind string
	err := row.Scan(&r.ID, &r.DeclarationID, &r.No, &r.Outcome, &r.From, &r.Sequence, &r.StatusCode, &r.OriginalText, &r.MappingVersion, &received,
		&r.DeclarationNumber, &exported, &r.Version, &evidence, &r.Reason, &kind, &r.By.ID, &r.Epoch, &r.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(evidence), &r.Evidence); err != nil {
		return nil, err
	}
	if received != nil {
		r.ReceivedAt = kernel.Instant(*received)
	}
	if exported != nil {
		r.ExportedAt = kernel.Instant(*exported)
	}
	r.By.Kind, r.At = kernel.ActorKind(kind), kernel.Instant(r.At)
	return &r, nil
}

// LoadReceipt returns a receipt by ID, or nil.
func LoadReceipt(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*dcl.Receipt, error) {
	return scanReceipt(exec.QueryRow(ctx, `SELECT `+receiptColumns+` FROM brant_cnx.declaration_receipt
		WHERE tenant_id = $1 AND company_id = $2 AND receipt_id = $3`, scoped(scope, id)...))
}

// Receipts returns the receipts of case id, oldest first.
func Receipts(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*dcl.Receipt, error) {
	rows, err := exec.Query(ctx, `SELECT `+receiptColumns+` FROM brant_cnx.declaration_receipt WHERE tenant_id = $1 AND company_id = $2
		AND declaration_id = $3 ORDER BY receipt_no`, scoped(scope, id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*dcl.Receipt
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
