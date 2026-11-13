package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Opening items (migrations/stl/000007_stl_opening_balances.sql, unit C10).

// InsertOpening writes opening item o, after the opening statement or fund record it is.
func InsertOpening(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, o *settlement.Opening) error {
	t, c := ts(sc)
	kind, id, epoch, at := fact(o.Recorded)
	cur := o.Amount.Currency()
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.opening_balance (tenant_id, company_id, opening_id, kind, status, cutoff_at, source_document,
		source_issued_at, source_amount_minor, amount_minor, currency, minor_units, currency_revision, counterparty_id, order_reference, statement_id,
		fund_record_id, evidence_ids, owner_actor_id, owner_team_id, recorded_by_kind, recorded_by_id, record_epoch, recorded_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::text::numeric, $10::text::numeric, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22,
		$23, $24, $24)`,
		t, c, o.ID, o.Kind, o.Status, kernel.Instant(o.CutoffAt), o.SourceDocument, kernel.Instant(o.SourceIssuedAt), o.SourceAmount.MinorString(),
		o.Amount.MinorString(), cur.Code(), int16(cur.MinorUnits()), o.CurrencyRevision, o.CounterpartyID, nullable(o.OrderReference),
		nullable(o.StatementID), nullable(o.FundRecordID), o.EvidenceIDs, o.OwnerActor, team(o.OwnerTeam), kind, id, epoch, at)
	return err
}

const openingColumns = `opening_id, kind, status, cutoff_at, source_document, source_issued_at, source_amount_minor::text, amount_minor::text,
	currency, minor_units, currency_revision, counterparty_id, coalesce(order_reference, ''), coalesce(statement_id, ''), coalesce(fund_record_id, ''),
	evidence_ids, owner_actor_id, coalesce(owner_team_id, ''), recorded_by_kind, recorded_by_id, record_epoch, recorded_at, updated_at`

func scanOpening(row pgx.Row) (*settlement.Opening, error) {
	var o settlement.Opening
	var source, amount, code, kind, by string
	var units int16
	var epoch int64
	var at time.Time
	err := row.Scan(&o.ID, &o.Kind, &o.Status, &o.CutoffAt, &o.SourceDocument, &o.SourceIssuedAt, &source, &amount, &code, &units, &o.CurrencyRevision,
		&o.CounterpartyID, &o.OrderReference, &o.StatementID, &o.FundRecordID, &o.EvidenceIDs, &o.OwnerActor, &o.OwnerTeam, &kind, &by, &epoch, &at,
		&o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cur, err := currency(code, units)
	if err != nil {
		return nil, err
	}
	if o.SourceAmount, err = money(cur, source); err != nil {
		return nil, err
	}
	if o.Amount, err = money(cur, amount); err != nil {
		return nil, err
	}
	o.CutoffAt, o.SourceIssuedAt, o.UpdatedAt = kernel.Instant(o.CutoffAt), kernel.Instant(o.SourceIssuedAt), kernel.Instant(o.UpdatedAt)
	o.Recorded = frAct(kind, by, epoch, at, "")
	return &o, nil
}

// LoadOpening returns opening item id, or nil; forUpdate locks its row.
func LoadOpening(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string, forUpdate bool) (*settlement.Opening, error) {
	t, c := ts(sc)
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	return scanOpening(exec.QueryRow(ctx, `SELECT `+openingColumns+` FROM brant_stl.opening_balance WHERE tenant_id = $1 AND company_id = $2
		AND opening_id = $3`+lock, t, c, id))
}

// StandingOpening returns the opening item of kind that stands (recorded) for source document document, or nil.
func StandingOpening(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, kind, document string) (*settlement.Opening, error) {
	t, c := ts(sc)
	return scanOpening(exec.QueryRow(ctx, `SELECT `+openingColumns+` FROM brant_stl.opening_balance WHERE tenant_id = $1 AND company_id = $2
		AND kind = $3 AND source_document = $4 AND status = 'recorded'`, t, c, kind, document))
}

// Openings returns the opening items, of cutoff only when it is not zero, by cutoff and ID.
func Openings(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, cutoff time.Time) ([]*settlement.Opening, error) {
	t, c := ts(sc)
	var at *time.Time
	if !cutoff.IsZero() {
		x := kernel.Instant(cutoff)
		at = &x
	}
	rows, err := exec.Query(ctx, `SELECT `+openingColumns+` FROM brant_stl.opening_balance WHERE tenant_id = $1 AND company_id = $2
		AND ($3::timestamptz IS NULL OR cutoff_at = $3) ORDER BY cutoff_at, opening_id`, t, c, at)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*settlement.Opening, error) { return scanOpening(row) })
}

// SaveOpeningReversed moves opening item o, standing recorded, to reversed (its statement was reversed).
func SaveOpeningReversed(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, o *settlement.Opening) error {
	t, c := ts(sc)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.opening_balance SET status = $4, updated_at = $5 WHERE tenant_id = $1 AND company_id = $2
		AND opening_id = $3 AND status = 'recorded'`, t, c, o.ID, o.Status, o.UpdatedAt)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}
