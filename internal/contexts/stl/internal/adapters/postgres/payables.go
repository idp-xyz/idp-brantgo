package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	fr "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Fund reversals, charge payments, settlement rates and payable adjustments (S5R unit R8;
// migrations/stl/000009_stl_settlement_results.sql).

// settlementRow is an application's settlement rate as stored: NULL columns without one (S5R-D19).
type settlementRow struct {
	id, hash, rate, currency *string
	revision                 *int64
	units                    *int16
}

func settlementArgs(s *fa.SettlementRate) settlementRow {
	var r settlementRow
	if s == nil {
		return r
	}
	id, hash, rate, code, revision, units := s.Use.ID, s.Use.Hash, s.Value.String(), s.Currency.Code(), s.Use.Revision, int16(s.Currency.MinorUnits())
	r.id, r.hash, r.rate, r.currency, r.revision, r.units = &id, &hash, &rate, &code, &revision, &units
	return r
}

func (r settlementRow) settlement() (*fa.SettlementRate, error) {
	if r.id == nil || r.hash == nil || r.rate == nil || r.currency == nil || r.revision == nil || r.units == nil {
		return nil, nil
	}
	cur, err := currency(*r.currency, *r.units)
	if err != nil {
		return nil, err
	}
	v, err := kernel.ParseDecimal(*r.rate, kernel.FactorFormat)
	if err != nil {
		return nil, err
	}
	return &fa.SettlementRate{Use: fa.RateUse{ID: *r.id, Revision: *r.revision, Hash: *r.hash}, Value: v, Currency: cur}, nil
}

func loadFundReversal(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string) (*fr.Reversal, error) {
	t, c := ts(sc)
	var r fr.Reversal
	var reason, kind, by string
	var epoch int64
	var at time.Time
	err := exec.QueryRow(ctx, `SELECT reason, evidence_ids, reversed_by_kind, reversed_by_id, reverse_epoch, reversed_at FROM brant_stl.fund_reversal
		WHERE tenant_id = $1 AND company_id = $2 AND fund_record_id = $3`, t, c, id).Scan(&reason, &r.EvidenceIDs, &kind, &by, &epoch, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.By = frAct(kind, by, epoch, at, reason)
	return &r, nil
}

// SaveFundReversal moves fund record f from version from to reversed and appends its reversal (S5R-D22).
func SaveFundReversal(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, f *fr.FundRecord, from int64) error {
	t, c := ts(sc)
	r := f.Reversal
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.fund_record SET status = $4, version = $5, updated_at = $6 WHERE tenant_id = $1 AND company_id = $2
		AND fund_record_id = $3 AND version = $7`, t, c, f.ID, f.Status, f.Version, r.By.At, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	evidence := r.EvidenceIDs
	if evidence == nil {
		evidence = []string{}
	}
	kind, id, epoch, at := fact(r.By)
	_, err = exec.Exec(ctx, `INSERT INTO brant_stl.fund_reversal (tenant_id, company_id, fund_record_id, reason, evidence_ids, reversed_by_kind,
		reversed_by_id, reverse_epoch, reversed_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, t, c, f.ID, r.By.Reason, evidence, kind, id, epoch, at)
	return err
}

// DrawIDs returns the refunds, returned remittances and conversions drawn on fund record id that stand.
func DrawIDs(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string) ([]string, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT fund_record_id FROM brant_stl.fund_record WHERE tenant_id = $1 AND company_id = $2
		AND source_fund_record_id = $3 AND status <> 'reversed' ORDER BY recorded_at, fund_record_id`, t, c, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// FundConversions returns, for each of fund records ids, its fx_conversion records that stand (S5R-D19), in the order
// they were recorded.
func FundConversions(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, ids []string) (map[string][]*fr.FundRecord, error) {
	out := map[string][]*fr.FundRecord{}
	if len(ids) == 0 {
		return out, nil
	}
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT `+fundColumns+` FROM brant_stl.fund_record WHERE tenant_id = $1 AND company_id = $2
		AND source_fund_record_id = ANY($3) AND kind = 'fx_conversion' AND status <> 'reversed' ORDER BY recorded_at, fund_record_id`, t, c, ids)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*fr.FundRecord, error) { return scanFund(row) })
	if err != nil {
		return nil, err
	}
	for _, f := range list {
		out[f.SourceID] = append(out[f.SourceID], f)
	}
	return out, nil
}

// ChargeApplications returns the applications to each of charges ids (S5R-D19), in the order they were made.
func ChargeApplications(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, ids []string) (map[string][]fa.Application, error) {
	out := map[string][]fa.Application{}
	if len(ids) == 0 {
		return out, nil
	}
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT `+applicationColumns+applicationFrom+` WHERE a.tenant_id = $1 AND a.company_id = $2
		AND a.charge_id = ANY($3) ORDER BY a.applied_at, a.application_id`, t, c, ids)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (fa.Application, error) {
		x, err := scanApplication(row)
		if err != nil || x == nil {
			return fa.Application{}, err
		}
		return *x, nil
	})
	for _, x := range list {
		out[x.ChargeID] = append(out[x.ChargeID], x)
	}
	return out, err
}

// InsertAdjustment writes a new payable adjustment (S5R-D20).
func InsertAdjustment(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, a *settlement.PayableAdjustment) error {
	t, c := ts(sc)
	cur := a.Amount.Currency()
	kind, id, epoch, at := fact(a.Requested)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.payable_adjustment (tenant_id, company_id, adjustment_id, statement_id, kind, status, version,
		currency, minor_units, amount_minor, purchase_invoice_id, basis, evidence_ids, owner_actor_id, owner_team_id, requested_by_kind,
		requested_by_id, request_epoch, requested_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::text::numeric, $11, $12, $13, $14,
		$15, $16, $17, $18, $19, $19)`,
		t, c, a.ID, a.StatementID, a.Kind, a.Status, a.Version, cur.Code(), int16(cur.MinorUnits()), a.Amount.MinorString(), a.PurchaseInvoiceID,
		a.Basis, a.EvidenceIDs, a.OwnerActor, team(a.OwnerTeam), kind, id, epoch, at)
	return err
}

// SaveAdjustmentDecision moves payable adjustment a from version from to its decided state.
func SaveAdjustmentDecision(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, a *settlement.PayableAdjustment, from int64) error {
	t, c := ts(sc)
	kind, id, epoch, at := fact(*a.Decided)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.payable_adjustment SET status = $4, version = $5, decided_by_kind = $6, decided_by_id = $7,
		decide_epoch = $8, decided_at = $9, decision_reason = $10, updated_at = $9 WHERE tenant_id = $1 AND company_id = $2 AND adjustment_id = $3
		AND version = $11`, t, c, a.ID, a.Status, a.Version, kind, id, epoch, at, a.Decided.Reason, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

const adjustmentColumns = `adjustment_id, statement_id, kind, status, version, currency, minor_units, amount_minor::text, purchase_invoice_id, basis,
	evidence_ids, owner_actor_id, coalesce(owner_team_id, ''), requested_by_kind, requested_by_id, request_epoch, requested_at, decided_by_kind,
	decided_by_id, decide_epoch, decided_at, decision_reason`

func scanAdjustment(row pgx.Row) (*settlement.PayableAdjustment, error) {
	var a settlement.PayableAdjustment
	var code, amount, kind, by string
	var units int16
	var epoch int64
	var at time.Time
	var dKind, dBy, dReason *string
	var dEpoch *int64
	var dAt *time.Time
	err := row.Scan(&a.ID, &a.StatementID, &a.Kind, &a.Status, &a.Version, &code, &units, &amount, &a.PurchaseInvoiceID, &a.Basis, &a.EvidenceIDs,
		&a.OwnerActor, &a.OwnerTeam, &kind, &by, &epoch, &at, &dKind, &dBy, &dEpoch, &dAt, &dReason)
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
	if a.Amount, err = money(cur, amount); err != nil {
		return nil, err
	}
	a.Requested = frAct(kind, by, epoch, at, "")
	if dKind != nil && dBy != nil && dEpoch != nil && dAt != nil && dReason != nil {
		d := frAct(*dKind, *dBy, *dEpoch, *dAt, *dReason)
		a.Decided = &d
	}
	return &a, nil
}

// LoadAdjustment returns payable adjustment id, or nil. Its statement's lock guards it (an entity of the statement).
func LoadAdjustment(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string) (*settlement.PayableAdjustment, error) {
	t, c := ts(sc)
	return scanAdjustment(exec.QueryRow(ctx, `SELECT `+adjustmentColumns+` FROM brant_stl.payable_adjustment WHERE tenant_id = $1 AND company_id = $2
		AND adjustment_id = $3`, t, c, id))
}

// Adjustments returns the payable adjustments of statement id, in the order they were requested.
func Adjustments(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string) ([]settlement.PayableAdjustment, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT `+adjustmentColumns+` FROM brant_stl.payable_adjustment WHERE tenant_id = $1 AND company_id = $2
		AND statement_id = $3 ORDER BY requested_at, adjustment_id`, t, c, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (settlement.PayableAdjustment, error) {
		a, err := scanAdjustment(row)
		if err != nil || a == nil {
			return settlement.PayableAdjustment{}, err
		}
		return *a, nil
	})
}

// adjusted is what the confirmed adjustments of purchase statement s took off its payable (S5R-D20), in s's currency.
func adjusted(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, s *settlement.Statement) (kernel.Money, error) {
	t, c := ts(sc)
	var sum string
	if err := exec.QueryRow(ctx, `SELECT coalesce(sum(amount_minor), 0)::text FROM brant_stl.payable_adjustment WHERE tenant_id = $1 AND company_id = $2
		AND statement_id = $3 AND status = 'confirmed'`, t, c, s.ID).Scan(&sum); err != nil {
		return kernel.Money{}, err
	}
	return money(s.Currency(), sum)
}

// Adjusted is what the confirmed adjustments of statement s took off its payable (zero for a sales statement).
func Adjusted(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, s *settlement.Statement) (kernel.Money, error) {
	if s.Kind != settlement.KindPurchase {
		return kernel.NewMoney(s.Currency(), 0)
	}
	return adjusted(ctx, exec, sc, s)
}
