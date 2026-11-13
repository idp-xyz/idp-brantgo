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

// Opening refund material (migrations/cnx/000006_cnx_opening_refund_material.sql, unit C10).

// InsertTaxOpening appends opening item o (after an opening case it names).
func InsertTaxOpening(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, o *tr.Opening) error {
	evidence, err := json.Marshal(nonNil(o.Evidence))
	if err != nil {
		return err
	}
	var exported *time.Time
	if !o.ExportedAt.IsZero() {
		x := kernel.Instant(o.ExportedAt)
		exported = &x
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cnx.tax_opening (tenant_id, company_id, opening_id, kind, cutoff_at, source_document,
		purchase_invoice_id, case_id, export_reference, exported_at, order_reference, position, evidence, recorded_by_kind, recorded_by_id,
		authority_epoch, recorded_at) VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), nullif($9, ''), $10, nullif($11, ''),
		nullif($12, ''), $13, $14, $15, $16, $17)`,
		scoped(scope, o.ID, o.Kind, kernel.Instant(o.CutoffAt), o.SourceDocument, o.InvoiceID, o.CaseID, o.ExportReference, exported,
			o.OrderReference, o.Position, string(evidence), string(o.By.Kind), o.By.ID, o.Epoch, kernel.Instant(o.At))...)
	return err
}

// InsertConsumption appends the opening consumption of an invoice line for a purpose.
func InsertConsumption(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c tr.Consumption) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cnx.tax_evidence_opening (tenant_id, company_id, opening_id, purchase_invoice_id, line_no, purpose,
		consumed, unit) VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8)`,
		scoped(scope, c.OpeningID, c.InvoiceID, c.LineNo, c.Purpose, c.Consumed.String(), string(c.Unit))...)
	return err
}

const taxOpeningColumns = `opening_id, kind, cutoff_at, source_document, coalesce(purchase_invoice_id, ''), coalesce(case_id, ''),
	coalesce(export_reference, ''), exported_at, coalesce(order_reference, ''), coalesce(position, ''), evidence, recorded_by_kind, recorded_by_id,
	authority_epoch, recorded_at`

func scanTaxOpening(row pgx.Row) (*tr.Opening, error) {
	var o tr.Opening
	var exported *time.Time
	var evidence, kind string
	err := row.Scan(&o.ID, &o.Kind, &o.CutoffAt, &o.SourceDocument, &o.InvoiceID, &o.CaseID, &o.ExportReference, &exported, &o.OrderReference,
		&o.Position, &evidence, &kind, &o.By.ID, &o.Epoch, &o.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(evidence), &o.Evidence); err != nil {
		return nil, err
	}
	if exported != nil {
		o.ExportedAt = kernel.Instant(*exported)
	}
	o.CutoffAt, o.At, o.By.Kind = kernel.Instant(o.CutoffAt), kernel.Instant(o.At), kernel.ActorKind(kind)
	return &o, nil
}

// LoadTaxOpening returns opening item id, or nil.
func LoadTaxOpening(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*tr.Opening, error) {
	return scanTaxOpening(exec.QueryRow(ctx, `SELECT `+taxOpeningColumns+` FROM brant_cnx.tax_opening WHERE tenant_id = $1 AND company_id = $2
		AND opening_id = $3`, scoped(scope, id)...))
}

// TaxOpeningOf returns the opening item of kind recorded for source document document, or of invoice invoiceID's
// balance, or of case caseID ("" for those not asked), whichever exists first, or nil.
func TaxOpeningOf(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, kind, document, invoiceID, caseID string) (*tr.Opening, error) {
	return scanTaxOpening(exec.QueryRow(ctx, `SELECT `+taxOpeningColumns+` FROM brant_cnx.tax_opening WHERE tenant_id = $1 AND company_id = $2
		AND ((kind = $3 AND source_document = $4) OR purchase_invoice_id = nullif($5, '') OR case_id = nullif($6, ''))
		ORDER BY recorded_at, opening_id LIMIT 1`, scoped(scope, kind, document, invoiceID, caseID)...))
}

// TaxOpenings returns the scope's opening items, by cutoff and ID.
func TaxOpenings(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) ([]*tr.Opening, error) {
	rows, err := exec.Query(ctx, `SELECT `+taxOpeningColumns+` FROM brant_cnx.tax_opening WHERE tenant_id = $1 AND company_id = $2
		ORDER BY cutoff_at, opening_id`, scoped(scope)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*tr.Opening, error) { return scanTaxOpening(row) })
}

// Consumptions returns the opening consumption of invoice invoiceID, per line and purpose, as opening item
// openingID states it ("" every item's: an invoice has at most one).
func Consumptions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, invoiceID string) ([]tr.Consumption, error) {
	rows, err := exec.Query(ctx, `SELECT opening_id, line_no, purpose, trim_scale(consumed)::text, unit FROM brant_cnx.tax_evidence_opening
		WHERE tenant_id = $1 AND company_id = $2 AND purchase_invoice_id = $3 ORDER BY line_no, purpose`, scoped(scope, invoiceID)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (tr.Consumption, error) {
		c := tr.Consumption{InvoiceID: invoiceID}
		var q, unit string
		if err := row.Scan(&c.OpeningID, &c.LineNo, &c.Purpose, &q, &unit); err != nil {
			return c, err
		}
		c.Unit = kernel.UnitCode(unit)
		var err error
		c.Consumed, err = decimal(q)
		return c, err
	})
}

// DeclaredAs reports whether a declaration case of CNX states declaration number number: an export CNX declared
// itself, whose refund case is allocated from its FUL export lot.
func DeclaredAs(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, number string) (bool, error) {
	var found bool
	err := exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_cnx.declaration_case WHERE tenant_id = $1 AND company_id = $2
		AND declaration_number = $3)`, scoped(scope, number)...).Scan(&found)
	return found, err
}
