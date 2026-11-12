package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	tr "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/taxrefund"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Tax refund cases, their allocations and results (migrations/cnx/000003_cnx_invoices_tax_refunds.sql).

const caseColumns = `case_id, export_lot_id, attempt, order_id, exported_at, policy_id, policy_revision, policy_hash, refund_method,
	policy_valid_from, policy_valid_until, status, row_version, opened_by_kind, opened_by_id, authority_epoch, opened_at`

func scanCase(row pgx.Row) (*tr.Case, error) {
	var c tr.Case
	var kind string
	var until *time.Time
	err := row.Scan(&c.ID, &c.ExportLotID, &c.Attempt, &c.OrderID, &c.ExportedAt, &c.Policy.PolicyID, &c.Policy.Revision, &c.Policy.ContentHash,
		&c.Policy.RefundMethod, &c.Policy.ValidFrom, &until, &c.Status, &c.RowVersion, &kind, &c.OpenedBy.ID, &c.Epoch, &c.OpenedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if until != nil {
		c.Policy.ValidUntil = kernel.Instant(*until)
	}
	c.OpenedBy.Kind, c.ExportedAt, c.OpenedAt = kernel.ActorKind(kind), kernel.Instant(c.ExportedAt), kernel.Instant(c.OpenedAt)
	c.Policy.ValidFrom = kernel.Instant(c.Policy.ValidFrom)
	return &c, nil
}

// LoadCase returns a case by ID, or nil, taking lock.
func LoadCase(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, caseID string, lock Lock) (*tr.Case, error) {
	return scanCase(exec.QueryRow(ctx, `SELECT `+caseColumns+` FROM brant_cnx.tax_refund_case
		WHERE tenant_id = $1 AND company_id = $2 AND case_id = $3`+string(lock), scoped(scope, caseID)...))
}

// CaseOfLot returns the latest case of an export lot (its highest attempt), or nil, taking lock.
func CaseOfLot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, lotID string, lock Lock) (*tr.Case, error) {
	return scanCase(exec.QueryRow(ctx, `SELECT `+caseColumns+` FROM brant_cnx.tax_refund_case
		WHERE tenant_id = $1 AND company_id = $2 AND export_lot_id = $3 ORDER BY attempt DESC LIMIT 1`+string(lock), scoped(scope, lotID)...))
}

// Cases returns the scope's cases, newest first.
func Cases(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) ([]*tr.Case, error) {
	rows, err := exec.Query(ctx, `SELECT `+caseColumns+` FROM brant_cnx.tax_refund_case WHERE tenant_id = $1 AND company_id = $2
		ORDER BY opened_at DESC, case_id`, scoped(scope)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tr.Case
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// InsertCase opens a case at row version 1.
func InsertCase(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *tr.Case) error {
	var until *time.Time
	if !c.Policy.ValidUntil.IsZero() {
		u := kernel.Instant(c.Policy.ValidUntil)
		until = &u
	}
	_, err := exec.Exec(ctx, `INSERT INTO brant_cnx.tax_refund_case (tenant_id, company_id, case_id, export_lot_id, attempt, order_id, exported_at,
		policy_id, policy_revision, policy_hash, refund_method, policy_valid_from, policy_valid_until, status, row_version, opened_by_kind,
		opened_by_id, authority_epoch, opened_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'expected', 1, $14, $15, $16, $17, $17)`,
		scoped(scope, c.ID, c.ExportLotID, c.Attempt, c.OrderID, kernel.Instant(c.ExportedAt), c.Policy.PolicyID, c.Policy.Revision,
			c.Policy.ContentHash, c.Policy.RefundMethod, kernel.Instant(c.Policy.ValidFrom), until, string(c.OpenedBy.Kind), c.OpenedBy.ID,
			c.Epoch, kernel.Instant(c.OpenedAt))...)
	return err
}

// TouchCase moves case c from row version from to its row version and status now.
func TouchCase(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *tr.Case, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cnx.tax_refund_case SET status = $4, row_version = $5, updated_at = $6
		WHERE tenant_id = $1 AND company_id = $2 AND case_id = $3 AND row_version = $7`,
		scoped(scope, c.ID, c.Status, c.RowVersion, kernel.Instant(at), from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// LockInvoices locks the invoice roots ids FOR UPDATE, in their order (sorted by the caller).
func LockInvoices(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string) error {
	for _, id := range ids {
		if _, err := exec.Exec(ctx, `SELECT 1 FROM brant_cnx.purchase_invoice WHERE tenant_id = $1 AND company_id = $2
			AND purchase_invoice_id = $3 FOR UPDATE`, scoped(scope, id)...); err != nil {
			return err
		}
	}
	return nil
}

// InvoiceLines returns the verified lines refs name that exist, with their invoice's state.
func InvoiceLines(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, refs []tr.InvoiceLineRef) (map[tr.InvoiceLineRef]tr.InvoiceLine, error) {
	out := map[tr.InvoiceLineRef]tr.InvoiceLine{}
	for _, r := range refs {
		if _, ok := out[r]; ok {
			continue
		}
		l := tr.InvoiceLine{InvoiceID: r.InvoiceID, LineNo: r.LineNo}
		var unit, q string
		err := exec.QueryRow(ctx, `SELECT i.status, i.colour, i.invoice_kind, i.invoice_number, l.unit, trim_scale(l.quantity)::text
			FROM brant_cnx.purchase_invoice_line l JOIN brant_cnx.purchase_invoice i ON i.tenant_id = l.tenant_id AND i.company_id = l.company_id
			AND i.purchase_invoice_id = l.purchase_invoice_id WHERE l.tenant_id = $1 AND l.company_id = $2 AND l.purchase_invoice_id = $3
			AND l.line_no = $4`, scoped(scope, r.InvoiceID, r.LineNo)...).Scan(&l.InvoiceStatus, &l.Colour, &l.Kind, &l.Number, &unit, &q)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		l.Unit = kernel.UnitCode(unit)
		if l.Quantity, err = decimal(q); err != nil {
			return nil, err
		}
		out[r] = l
	}
	return out, nil
}

const counts = `brant_cnx.tax_evidence_counts(a.tenant_id, a.company_id, a.allocation_id, a.case_id)`

// UsedEvidence returns what counting allocations of every case take of each invoice line and
// purpose of keys.
func UsedEvidence(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, keys []tr.LineKey) (map[tr.LineKey]kernel.Decimal, error) {
	out := map[tr.LineKey]kernel.Decimal{}
	for _, k := range keys {
		var q string
		if err := exec.QueryRow(ctx, `SELECT coalesce(trim_scale(sum(a.quantity)), 0)::text FROM brant_cnx.tax_evidence_allocation a
			WHERE a.tenant_id = $1 AND a.company_id = $2 AND a.purchase_invoice_id = $3 AND a.line_no = $4 AND a.purpose = $5 AND `+counts,
			scoped(scope, k.InvoiceID, k.LineNo, k.Purpose)...).Scan(&q); err != nil {
			return nil, err
		}
		d, err := decimal(q)
		if err != nil {
			return nil, err
		}
		out[k] = d
	}
	return out, nil
}

// Balance is what counting allocations take of an invoice line for a purpose.
type Balance struct {
	LineNo  int
	Purpose string
	Used    kernel.Decimal
}

// InvoiceUsage returns, per line and purpose, what counting allocations take of invoice id.
func InvoiceUsage(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]Balance, error) {
	rows, err := exec.Query(ctx, `SELECT a.line_no, a.purpose, trim_scale(sum(a.quantity))::text FROM brant_cnx.tax_evidence_allocation a
		WHERE a.tenant_id = $1 AND a.company_id = $2 AND a.purchase_invoice_id = $3 AND `+counts+` GROUP BY a.line_no, a.purpose
		ORDER BY a.line_no, a.purpose`, scoped(scope, id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Balance
	for rows.Next() {
		var b Balance
		var q string
		if err := rows.Scan(&b.LineNo, &b.Purpose, &q); err != nil {
			return nil, err
		}
		if b.Used, err = decimal(q); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// TaxAllocationExists reports whether an allocation ID is taken in the scope.
func TaxAllocationExists(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (bool, error) {
	var taken bool
	err := exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_cnx.tax_evidence_allocation WHERE tenant_id = $1 AND company_id = $2
		AND allocation_id = $3)`, scoped(scope, id)...).Scan(&taken)
	return taken, err
}

// CaseAllocations returns the allocations of case c, oldest first, with what replaced them and the
// state of their invoice.
func CaseAllocations(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *tr.Case) ([]*tr.Allocation, error) {
	rows, err := exec.Query(ctx, `SELECT a.allocation_id, a.export_line_id, a.shipment_id, a.order_line_id, a.sku_id, a.spec_revision,
		trim_scale(a.export_quantity)::text, a.export_unit, a.purchase_invoice_id, i.invoice_kind, i.invoice_number, i.status, a.line_no, a.purpose,
		trim_scale(a.quantity)::text, a.unit, coalesce(a.replaces, ''), coalesce((SELECT r.allocation_id FROM brant_cnx.tax_evidence_allocation r
		WHERE r.tenant_id = a.tenant_id AND r.company_id = a.company_id AND r.replaces = a.allocation_id), ''), a.allocated_by_kind,
		a.allocated_by_id, a.authority_epoch, a.allocated_at
		FROM brant_cnx.tax_evidence_allocation a JOIN brant_cnx.purchase_invoice i ON i.tenant_id = a.tenant_id AND i.company_id = a.company_id
		AND i.purchase_invoice_id = a.purchase_invoice_id
		WHERE a.tenant_id = $1 AND a.company_id = $2 AND a.case_id = $3 ORDER BY a.allocated_at, a.allocation_id`, scoped(scope, c.ID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tr.Allocation
	for rows.Next() {
		a := tr.Allocation{CaseID: c.ID, CaseStatus: c.Status}
		var eq, eu, q, u, kind string
		if err := rows.Scan(&a.ID, &a.ExportLineID, &a.ShipmentID, &a.OrderLineID, &a.SkuID, &a.SpecRevision, &eq, &eu, &a.InvoiceID,
			&a.InvoiceKind, &a.InvoiceNumber, &a.InvoiceStatus, &a.LineNo, &a.Purpose, &q, &u, &a.Replaces, &a.ReplacedBy, &kind, &a.By.ID,
			&a.Epoch, &a.At); err != nil {
			return nil, err
		}
		if a.ExportQuantity, err = decimal(eq); err != nil {
			return nil, err
		}
		if a.Quantity, err = decimal(q); err != nil {
			return nil, err
		}
		a.ExportUnit, a.Unit, a.By.Kind, a.At = kernel.UnitCode(eu), kernel.UnitCode(u), kernel.ActorKind(kind), kernel.Instant(a.At)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// InsertTaxAllocation appends an allocation.
func InsertTaxAllocation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, a *tr.Allocation) error {
	var replaces *string
	if a.Replaces != "" {
		replaces = &a.Replaces
	}
	_, err := exec.Exec(ctx, `INSERT INTO brant_cnx.tax_evidence_allocation (tenant_id, company_id, allocation_id, case_id, export_line_id,
		shipment_id, order_line_id, sku_id, spec_revision, export_quantity, export_unit, purchase_invoice_id, line_no, purpose, quantity, unit,
		replaces, allocated_by_kind, allocated_by_id, authority_epoch, allocated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::numeric, $11, $12, $13, $14, $15::numeric, $16, $17, $18, $19, $20, $21)`,
		scoped(scope, a.ID, a.CaseID, a.ExportLineID, a.ShipmentID, a.OrderLineID, a.SkuID, a.SpecRevision, a.ExportQuantity.String(),
			string(a.ExportUnit), a.InvoiceID, a.LineNo, a.Purpose, a.Quantity.String(), string(a.Unit), replaces, string(a.By.Kind), a.By.ID,
			a.Epoch, kernel.Instant(a.At))...)
	return err
}

// InsertTaxResult appends a result.
func InsertTaxResult(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *tr.Result) error {
	review, err := json.Marshal(nonNil(r.ReviewEvidence))
	if err != nil {
		return err
	}
	evidence, err := json.Marshal(nonNil(r.Evidence))
	if err != nil {
		return err
	}
	var currencyID, code *string
	var revision *int64
	var minorUnits *int
	var amount, offset *string
	if c := r.Currency; c != nil {
		currencyID, revision, code, minorUnits = &c.ConfigID, &c.Revision, &c.Code, &c.MinorUnits
	}
	if r.Amount != nil {
		s := r.Amount.MinorString()
		amount = &s
	}
	if r.Offset != nil {
		s := r.Offset.MinorString()
		offset = &s
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.tax_refund_result (tenant_id, company_id, result_id, case_id, stage, from_status, currency_id,
		currency_revision, currency_code, minor_units, amount_minor, offset_minor, tax_period, reviewer, review_evidence, external_reference,
		result_text, fund_record_id, evidence, reason, recorded_by_kind, recorded_by_id, authority_epoch, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::numeric, $12::numeric, nullif($13, ''), nullif($14, ''), $15, nullif($16, ''),
		nullif($17, ''), nullif($18, ''), $19, nullif($20, ''), $21, $22, $23, $24)`,
		scoped(scope, r.ID, r.CaseID, r.Stage, r.From, currencyID, revision, code, minorUnits, amount, offset, r.TaxPeriod, r.Reviewer,
			string(review), r.ExternalReference, r.ResultText, r.FundRecordID, string(evidence), r.Reason, string(r.By.Kind), r.By.ID, r.Epoch,
			kernel.Instant(r.At))...)
	return err
}

func nonNil(e []tr.Evidence) []tr.Evidence {
	if e == nil {
		return []tr.Evidence{}
	}
	return e
}

// TaxResults returns the results of case id, oldest first.
func TaxResults(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*tr.Result, error) {
	rows, err := exec.Query(ctx, `SELECT result_id, stage, from_status, coalesce(currency_id, ''), coalesce(currency_revision, 0),
		coalesce(currency_code, ''), coalesce(minor_units, 0), coalesce(amount_minor::text, ''), coalesce(offset_minor::text, ''),
		coalesce(tax_period, ''), coalesce(reviewer, ''), review_evidence, coalesce(external_reference, ''), coalesce(result_text, ''),
		coalesce(fund_record_id, ''), evidence, coalesce(reason, ''), recorded_by_kind, recorded_by_id, authority_epoch, recorded_at
		FROM brant_cnx.tax_refund_result WHERE tenant_id = $1 AND company_id = $2 AND case_id = $3 ORDER BY recorded_at, result_id`, scoped(scope, id)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tr.Result
	for rows.Next() {
		r := tr.Result{CaseID: id}
		var c tr.Currency
		var amount, offset, review, evidence, kind string
		if err := rows.Scan(&r.ID, &r.Stage, &r.From, &c.ConfigID, &c.Revision, &c.Code, &c.MinorUnits, &amount, &offset, &r.TaxPeriod, &r.Reviewer,
			&review, &r.ExternalReference, &r.ResultText, &r.FundRecordID, &evidence, &r.Reason, &kind, &r.By.ID, &r.Epoch, &r.At); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(review), &r.ReviewEvidence); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(evidence), &r.Evidence); err != nil {
			return nil, err
		}
		if c.Code != "" {
			r.Currency = &c
			kc, err := kernel.NewCurrency(c.Code, c.MinorUnits)
			if err != nil {
				return nil, err
			}
			for _, x := range []struct {
				s  string
				to **kernel.Money
			}{{amount, &r.Amount}, {offset, &r.Offset}} {
				if x.s == "" {
					continue
				}
				m, err := minor(kc, x.s)
				if err != nil {
					return nil, err
				}
				*x.to = &m
			}
		}
		r.By.Kind, r.At = kernel.ActorKind(kind), kernel.Instant(r.At)
		out = append(out, &r)
	}
	return out, rows.Err()
}
