package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	pin "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/purchaseinvoice"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Purchase invoices, their verified lines and matches (migrations/cnx/000003_cnx_invoices_tax_refunds.sql).

const invoiceColumns = `purchase_invoice_id, status, row_version, file_id, file_sha256, candidate, imported_by_kind, imported_by_id,
	import_epoch, imported_at, coalesce(verified, ''), coalesce(decision, ''), coalesce(decision_reason, ''), coalesce(decided_by_kind, ''),
	coalesce(decided_by_id, ''), coalesce(decide_epoch, 0), decided_at, coalesce(red_of_invoice_id, ''), coalesce(reversed_by_invoice_id, '')`

func scanInvoice(row pgx.Row) (*pin.Invoice, error) {
	var i pin.Invoice
	var candidate, verified, importKind, decideKind string
	var decidedAt *time.Time
	err := row.Scan(&i.ID, &i.Status, &i.RowVersion, &i.FileID, &i.FileSHA256, &candidate, &importKind, &i.ImportedBy.ID, &i.ImportEpoch,
		&i.ImportedAt, &verified, &i.Decision, &i.Reason, &decideKind, &i.DecidedBy.ID, &i.DecideEpoch, &decidedAt, &i.RedOfID, &i.ReversedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(candidate), &i.Candidate); err != nil {
		return nil, err
	}
	if verified != "" {
		var f pin.Facts
		if err = json.Unmarshal([]byte(verified), &f); err != nil {
			return nil, err
		}
		i.Verified = &f
	}
	i.ImportedBy.Kind, i.DecidedBy.Kind, i.ImportedAt = kernel.ActorKind(importKind), kernel.ActorKind(decideKind), kernel.Instant(i.ImportedAt)
	if decidedAt != nil {
		i.DecidedAt = kernel.Instant(*decidedAt)
	}
	return &i, nil
}

// LoadInvoice returns an invoice by ID, or nil, taking lock.
func LoadInvoice(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, invoiceID string, lock Lock) (*pin.Invoice, error) {
	return scanInvoice(exec.QueryRow(ctx, `SELECT `+invoiceColumns+` FROM brant_cnx.purchase_invoice
		WHERE tenant_id = $1 AND company_id = $2 AND purchase_invoice_id = $3`+string(lock), scoped(scope, invoiceID)...))
}

// InvoiceByKey returns the ID of the invoice that is not rejected and holds (kind, number), or "".
func InvoiceByKey(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, kind, number string) (string, error) {
	var id string
	err := exec.QueryRow(ctx, `SELECT purchase_invoice_id FROM brant_cnx.purchase_invoice WHERE tenant_id = $1 AND company_id = $2
		AND invoice_kind = $3 AND invoice_number = $4 AND status <> 'rejected'`, scoped(scope, kind, number)...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Invoices returns the scope's invoices, newest first.
func Invoices(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) ([]*pin.Invoice, error) {
	rows, err := exec.Query(ctx, `SELECT `+invoiceColumns+` FROM brant_cnx.purchase_invoice WHERE tenant_id = $1 AND company_id = $2
		ORDER BY imported_at DESC, purchase_invoice_id`, scoped(scope)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pin.Invoice
	for rows.Next() {
		i, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// InsertInvoice inserts an imported invoice at row version 1.
func InsertInvoice(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, i *pin.Invoice) error {
	candidate, err := json.Marshal(i.Candidate)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.purchase_invoice (tenant_id, company_id, purchase_invoice_id, invoice_kind, invoice_number,
		colour, status, row_version, file_id, file_sha256, candidate, imported_by_kind, imported_by_id, import_epoch, imported_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'imported', 1, $7, $8, $9, $10, $11, $12, $13, $13)`,
		scoped(scope, i.ID, i.Candidate.Kind, i.Candidate.Number, i.Candidate.Colour, i.FileID, i.FileSHA256, string(candidate),
			string(i.ImportedBy.Kind), i.ImportedBy.ID, i.ImportEpoch, kernel.Instant(i.ImportedAt))...)
	return err
}

// DecideInvoice stores the decision of invoice i, moving it from row version from.
func DecideInvoice(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, i *pin.Invoice, from int64) error {
	var verified *string
	key := i.Key()
	colour := i.Candidate.Colour
	if i.Verified != nil {
		raw, err := json.Marshal(i.Verified)
		if err != nil {
			return err
		}
		s := string(raw)
		verified, colour = &s, i.Verified.Colour
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_cnx.purchase_invoice SET status = $4, row_version = $5, invoice_kind = $6, invoice_number = $7,
		colour = $8, verified = $9, decision = $10, decision_reason = nullif($11, ''), decided_by_kind = $12, decided_by_id = $13,
		decide_epoch = $14, decided_at = $15, red_of_invoice_id = nullif($16, ''), updated_at = $15
		WHERE tenant_id = $1 AND company_id = $2 AND purchase_invoice_id = $3 AND row_version = $17`,
		scoped(scope, i.ID, i.Status, i.RowVersion, key.Kind, key.Number, colour, verified, i.Decision, i.Reason, string(i.DecidedBy.Kind),
			i.DecidedBy.ID, i.DecideEpoch, kernel.Instant(i.DecidedAt), i.RedOfID, from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// TouchInvoice moves verified blue invoice i, which a red invoice reversed part of, from row version from to its row
// version, still verified (S5R-D20).
func TouchInvoice(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, i *pin.Invoice, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cnx.purchase_invoice SET row_version = $4, updated_at = $5 WHERE tenant_id = $1 AND company_id = $2
		AND purchase_invoice_id = $3 AND row_version = $6 AND status = 'verified'`, scoped(scope, i.ID, i.RowVersion, kernel.Instant(at), from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// RedInvoices returns the verified red invoices that reverse blue invoice id, oldest first.
func RedInvoices(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*pin.Invoice, error) {
	rows, err := exec.Query(ctx, `SELECT `+invoiceColumns+` FROM brant_cnx.purchase_invoice WHERE tenant_id = $1 AND company_id = $2
		AND red_of_invoice_id = $3 AND status = 'verified' ORDER BY decided_at, purchase_invoice_id`, scoped(scope, id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pin.Invoice
	for rows.Next() {
		i, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// TakeBacks returns the matches of the verified red invoices that reverse blue invoice id: what they took back of its
// matches (S5R-D20).
func TakeBacks(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]pin.Match, error) {
	reds, err := RedInvoices(ctx, exec, scope, id)
	if err != nil {
		return nil, err
	}
	var out []pin.Match
	for _, r := range reds {
		ms, err := Matches(ctx, exec, scope, r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	return out, nil
}

// InvoicesOfCommitments returns the IDs of the verified blue invoices (verified or reversed) with a match to a line of
// one of commitments, and of the verified red invoices that reverse them (PurchaseInvoiceRef v1, S5R-D20).
func InvoicesOfCommitments(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, commitments []string) ([]string, error) {
	if len(commitments) == 0 {
		return []string{}, nil
	}
	rows, err := exec.Query(ctx, `WITH blue AS (SELECT DISTINCT i.purchase_invoice_id FROM brant_cnx.purchase_invoice_match m
			JOIN brant_cnx.purchase_invoice i ON i.tenant_id = m.tenant_id AND i.company_id = m.company_id AND i.purchase_invoice_id = m.purchase_invoice_id
			WHERE m.tenant_id = $1 AND m.company_id = $2 AND m.commitment_id = ANY($3) AND i.colour = 'blue' AND i.status IN ('verified', 'reversed'))
		SELECT purchase_invoice_id FROM blue
		UNION SELECT r.purchase_invoice_id FROM brant_cnx.purchase_invoice r WHERE r.tenant_id = $1 AND r.company_id = $2 AND r.status = 'verified'
			AND r.red_of_invoice_id IN (SELECT purchase_invoice_id FROM blue)
		ORDER BY 1`, scoped(scope, commitments)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FiledCasesWithoutRollback returns the tax refund cases, filed (submitted, approved or received), whose counting
// allocations use invoice id and which recorded no evidence_rolled_back of it (S5R-D20: a red invoice reversing it
// waits for the rollback), in ID order.
func FiledCasesWithoutRollback(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]string, error) {
	rows, err := exec.Query(ctx, `SELECT c.case_id || ' (' || c.status || ')' FROM brant_cnx.tax_refund_case c
		WHERE c.tenant_id = $1 AND c.company_id = $2 AND c.status IN ('submitted', 'approved', 'received')
		AND EXISTS (SELECT 1 FROM brant_cnx.tax_evidence_allocation a WHERE a.tenant_id = c.tenant_id AND a.company_id = c.company_id
			AND a.case_id = c.case_id AND a.purchase_invoice_id = $3 AND `+counts+`)
		AND NOT EXISTS (SELECT 1 FROM brant_cnx.tax_refund_result r WHERE r.tenant_id = c.tenant_id AND r.company_id = c.company_id
			AND r.case_id = c.case_id AND r.stage = 'evidence_rolled_back' AND r.purchase_invoice_id = $3)
		ORDER BY c.case_id`, scoped(scope, id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReverseInvoice marks verified blue invoice i reversed by red invoice redID, moving it from row
// version from.
func ReverseInvoice(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, i *pin.Invoice, redID string, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cnx.purchase_invoice SET status = 'reversed', row_version = $4, reversed_by_invoice_id = $5,
		updated_at = $6 WHERE tenant_id = $1 AND company_id = $2 AND purchase_invoice_id = $3 AND row_version = $7`,
		scoped(scope, i.ID, i.RowVersion, redID, kernel.Instant(at), from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// InsertInvoiceLines inserts the verified lines of invoice id.
func InsertInvoiceLines(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, f pin.Facts) error {
	for _, l := range f.Lines {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_cnx.purchase_invoice_line (tenant_id, company_id, purchase_invoice_id, line_no, unit, quantity)
			VALUES ($1, $2, $3, $4, $5, $6::numeric)`, scoped(scope, id, l.LineNo, l.Unit, l.Quantity)...); err != nil {
			return err
		}
	}
	return nil
}

// LockMatchLines takes the transaction's advisory locks on commitment lines refs, in their order
// (sorted by the caller): verifications that match the same commitment line are serialized, so the
// Σ standing matched read after them is the one the commit keeps. PUR's rows are neither read nor
// locked here.
func LockMatchLines(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, refs []pin.LineRef) error {
	for _, r := range refs {
		h := fnv.New64a()
		_, _ = h.Write([]byte("brant_cnx.purchase_invoice_match\x00" + string(scope.Tenant) + "\x00" + string(scope.Company) + "\x00" +
			r.CommitmentID + "\x00" + r.OrderLineID))
		if _, err := exec.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(h.Sum64())); err != nil {
			return err
		}
	}
	return nil
}

// StandingMatched returns, for each of refs, the quantity standing matches take of it, other than invoice except's:
// the matches of verified blue invoices (not reversed) less what verified red invoices took back of them (S5R-D20).
func StandingMatched(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, refs []pin.LineRef, except string) (map[pin.LineRef]kernel.Decimal, error) {
	out := map[pin.LineRef]kernel.Decimal{}
	for _, r := range refs {
		var q string
		if err := exec.QueryRow(ctx, `SELECT coalesce(trim_scale(sum(CASE WHEN i.colour = 'blue' THEN m.quantity ELSE -m.quantity END)), 0)::text
			FROM brant_cnx.purchase_invoice_match m
			JOIN brant_cnx.purchase_invoice i ON i.tenant_id = m.tenant_id AND i.company_id = m.company_id AND i.purchase_invoice_id = m.purchase_invoice_id
			LEFT JOIN brant_cnx.purchase_invoice b ON b.tenant_id = i.tenant_id AND b.company_id = i.company_id AND b.purchase_invoice_id = i.red_of_invoice_id
			WHERE m.tenant_id = $1 AND m.company_id = $2 AND m.commitment_id = $3 AND m.order_line_id = $4 AND i.status = 'verified'
			AND (i.colour = 'blue' OR b.status = 'verified') AND m.purchase_invoice_id <> $5`,
			scoped(scope, r.CommitmentID, r.OrderLineID, except)...).Scan(&q); err != nil {
			return nil, err
		}
		d, err := decimal(q)
		if err != nil {
			return nil, err
		}
		out[r] = d
	}
	return out, nil
}

// InsertMatches appends the matches of a verification.
func InsertMatches(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ms []pin.Match) error {
	for _, m := range ms {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_cnx.purchase_invoice_match (tenant_id, company_id, purchase_invoice_id, line_no, commitment_id,
			order_line_id, quantity, unit, supplier_id, po_number, purchase_quantity, delivered_quantity, matched_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9, $10, $11::numeric, $12::numeric, $13)`,
			scoped(scope, m.InvoiceID, m.LineNo, m.Ref.CommitmentID, m.Ref.OrderLineID, m.Quantity.String(), m.Unit, m.SupplierID, m.PONumber,
				m.PurchaseQuantity.String(), m.DeliveredQuantity.String(), kernel.Instant(m.MatchedAt))...); err != nil {
			return err
		}
	}
	return nil
}

// Matches returns the matches of invoice id in line order.
func Matches(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]pin.Match, error) {
	rows, err := exec.Query(ctx, `SELECT line_no, commitment_id, order_line_id, trim_scale(quantity)::text, unit, supplier_id, po_number,
		trim_scale(purchase_quantity)::text, trim_scale(delivered_quantity)::text, matched_at FROM brant_cnx.purchase_invoice_match
		WHERE tenant_id = $1 AND company_id = $2 AND purchase_invoice_id = $3 ORDER BY line_no, commitment_id, order_line_id`, scoped(scope, id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pin.Match
	for rows.Next() {
		m := pin.Match{InvoiceID: id}
		var q, pq, dq string
		if err := rows.Scan(&m.LineNo, &m.Ref.CommitmentID, &m.Ref.OrderLineID, &q, &m.Unit, &m.SupplierID, &m.PONumber, &pq, &dq, &m.MatchedAt); err != nil {
			return nil, err
		}
		for _, x := range []struct {
			s string
			d *kernel.Decimal
		}{{q, &m.Quantity}, {pq, &m.PurchaseQuantity}, {dq, &m.DeliveredQuantity}} {
			if *x.d, err = decimal(x.s); err != nil {
				return nil, err
			}
		}
		m.MatchedAt = kernel.Instant(m.MatchedAt)
		out = append(out, m)
	}
	return out, rows.Err()
}
