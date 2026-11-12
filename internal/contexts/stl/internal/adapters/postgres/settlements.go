package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Settlement statements, their lines and installments, and the applications on them
// (migrations/stl/000004_stl_settlement_statements.sql).

func optText(s string) *string { return nullable(s) }

func optInt64(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

// InsertStatement writes a new statement with its lines and installments.
func InsertStatement(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, s *settlement.Statement) error {
	t, c := ts(sc)
	cur := s.Currency()
	kind, id, epoch, at := fact(s.Confirmed)
	var shipped *time.Time
	if !s.ShippedAt.IsZero() {
		shipped = &s.ShippedAt
	}
	evidence := s.EvidenceIDs
	if evidence == nil {
		evidence = []string{}
	}
	if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.settlement_statement (tenant_id, company_id, statement_id, kind, status, version,
		counterparty_id, order_id, order_revision, order_hash, shipped_at, currency, minor_units, net_minor, tax_minor, amount_minor, evidence_ids,
		owner_actor_id, owner_team_id, confirmed_by_kind, confirmed_by_id, confirm_epoch, confirmed_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7,
		$8, $9, $10, $11, $12, $13, $14::text::numeric, $15::text::numeric, $16::text::numeric, $17, $18, $19, $20, $21, $22, $23, $23)`,
		t, c, s.ID, s.Kind, s.Status, s.Version, s.CounterpartyID, optText(s.OrderID), optInt64(s.OrderRevision), optText(s.OrderHash), shipped,
		cur.Code(), int16(cur.MinorUnits()), s.Net.MinorString(), s.Tax.MinorString(), s.Amount.MinorString(), evidence, s.OwnerActor,
		team(s.OwnerTeam), kind, id, epoch, at); err != nil {
		return err
	}
	for _, l := range s.Lines {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.settlement_line (tenant_id, company_id, statement_id, seq, source_kind, source_id,
			source_line_id, source_version, order_id, order_revision, order_hash, order_line_id, commitment_id, supplier_id, sku_id, spec_revision, unit_id,
			unit_revision, quantity, basis_quantity, unit_price, basis_net_minor, basis_tax_minor, net_minor, tax_minor, amount_minor, occurred_at, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19::text::numeric, $20::text::numeric, $21,
			$22::text::numeric, $23::text::numeric, $24::text::numeric, $25::text::numeric, $26::text::numeric, $27, $28)`,
			t, c, s.ID, l.Seq, l.Source.Kind, l.Source.ID, l.Source.LineID, l.SourceVersion, l.OrderID, l.OrderRevision, l.OrderHash, l.OrderLineID,
			optText(l.CommitmentID), optText(l.SupplierID), l.SkuID, l.SpecRevision, l.UnitID, l.UnitRevision, l.Quantity.String(),
			l.BasisQuantity.String(), optText(l.UnitPrice), l.BasisNet.MinorString(), l.BasisTax.MinorString(), l.Net.MinorString(),
			l.Tax.MinorString(), l.Amount.MinorString(), l.OccurredAt, l.Status); err != nil {
			return err
		}
	}
	for _, i := range s.Installments {
		var trigger, control *string
		var days *int
		if i.StageID != "" {
			trigger, control, days = &i.Trigger, &i.ReleaseControl, &i.Days
		}
		if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.settlement_installment (tenant_id, company_id, statement_id, seq, order_id, order_revision,
			stage_id, stage_trigger, days, release_control, amount_minor, due_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::text::numeric,
			$12)`, t, c, s.ID, i.Seq, optText(i.OrderID), optInt64(i.OrderRevision), optText(i.StageID), trigger, days, control,
			i.Amount.MinorString(), i.DueAt); err != nil {
			return err
		}
	}
	return nil
}

const statementColumns = `statement_id, kind, status, version, counterparty_id, coalesce(order_id, ''), coalesce(order_revision, 0),
	coalesce(order_hash, ''), shipped_at, currency, minor_units, net_minor::text, tax_minor::text, amount_minor::text, evidence_ids, owner_actor_id,
	coalesce(owner_team_id, ''), confirmed_by_kind, confirmed_by_id, confirm_epoch, confirmed_at`

func scanStatement(row pgx.Row) (*settlement.Statement, error) {
	var s settlement.Statement
	var code, net, tax, amount, kind, by string
	var units int16
	var shipped *time.Time
	var epoch int64
	var at time.Time
	err := row.Scan(&s.ID, &s.Kind, &s.Status, &s.Version, &s.CounterpartyID, &s.OrderID, &s.OrderRevision, &s.OrderHash, &shipped, &code, &units,
		&net, &tax, &amount, &s.EvidenceIDs, &s.OwnerActor, &s.OwnerTeam, &kind, &by, &epoch, &at)
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
	if s.Net, err = money(cur, net); err != nil {
		return nil, err
	}
	if s.Tax, err = money(cur, tax); err != nil {
		return nil, err
	}
	if s.Amount, err = money(cur, amount); err != nil {
		return nil, err
	}
	if shipped != nil {
		s.ShippedAt = kernel.Instant(*shipped)
	}
	s.Confirmed = frAct(kind, by, epoch, at, "")
	return &s, nil
}

// LoadStatement returns statement id with its lines and installments, or nil; forUpdate locks its
// row.
func LoadStatement(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string, forUpdate bool) (*settlement.Statement, error) {
	t, c := ts(sc)
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	s, err := scanStatement(exec.QueryRow(ctx, `SELECT `+statementColumns+` FROM brant_stl.settlement_statement WHERE tenant_id = $1
		AND company_id = $2 AND statement_id = $3`+lock, t, c, id))
	if err != nil || s == nil {
		return s, err
	}
	return s, completeStatement(ctx, exec, sc, s)
}

func completeStatement(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, s *settlement.Statement) error {
	t, c := ts(sc)
	cur := s.Currency()
	rows, err := exec.Query(ctx, `SELECT seq, source_kind, source_id, source_line_id, source_version, order_id, order_revision, order_hash,
		order_line_id, coalesce(commitment_id, ''), coalesce(supplier_id, ''), sku_id, spec_revision, unit_id, unit_revision, quantity::text,
		basis_quantity::text, coalesce(unit_price, ''), basis_net_minor::text, basis_tax_minor::text, net_minor::text, tax_minor::text,
		amount_minor::text, occurred_at, status FROM brant_stl.settlement_line WHERE tenant_id = $1 AND company_id = $2 AND statement_id = $3
		ORDER BY seq`, t, c, s.ID)
	if err != nil {
		return err
	}
	s.Lines, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (settlement.Line, error) {
		var l settlement.Line
		var q, bq, bn, bt, n, x, a string
		if err := row.Scan(&l.Seq, &l.Source.Kind, &l.Source.ID, &l.Source.LineID, &l.SourceVersion, &l.OrderID, &l.OrderRevision, &l.OrderHash,
			&l.OrderLineID, &l.CommitmentID, &l.SupplierID, &l.SkuID, &l.SpecRevision, &l.UnitID, &l.UnitRevision, &q, &bq, &l.UnitPrice, &bn, &bt,
			&n, &x, &a, &l.OccurredAt, &l.Status); err != nil {
			return l, err
		}
		l.OccurredAt = kernel.Instant(l.OccurredAt)
		var err error
		if l.Quantity, err = stored(q); err != nil {
			return l, err
		}
		if l.BasisQuantity, err = stored(bq); err != nil {
			return l, err
		}
		for _, m := range []struct {
			dst *kernel.Money
			s   string
		}{{&l.BasisNet, bn}, {&l.BasisTax, bt}, {&l.Net, n}, {&l.Tax, x}, {&l.Amount, a}} {
			if *m.dst, err = money(cur, m.s); err != nil {
				return l, err
			}
		}
		return l, nil
	})
	if err != nil {
		return err
	}
	rows, err = exec.Query(ctx, `SELECT seq, coalesce(order_id, ''), coalesce(order_revision, 0), coalesce(stage_id, ''),
		coalesce(stage_trigger, ''), coalesce(days, 0), coalesce(release_control, ''), amount_minor::text, due_at FROM brant_stl.settlement_installment
		WHERE tenant_id = $1 AND company_id = $2 AND statement_id = $3 ORDER BY seq`, t, c, s.ID)
	if err != nil {
		return err
	}
	s.Installments, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (settlement.Installment, error) {
		var i settlement.Installment
		var a string
		if err := row.Scan(&i.Seq, &i.OrderID, &i.OrderRevision, &i.StageID, &i.Trigger, &i.Days, &i.ReleaseControl, &a, &i.DueAt); err != nil {
			return i, err
		}
		i.DueAt = kernel.Instant(i.DueAt)
		var err error
		i.Amount, err = money(cur, a)
		return i, err
	})
	return err
}

// stored parses a quantity PostgreSQL rendered from numeric(24,6).
func stored(s string) (kernel.Decimal, error) { return kernel.ParseDecimal(s, kernel.QuantityFormat) }

// Statements returns the statements of kind (sales or purchase) that are of order orderID: the
// sales statements of the order, or the purchase statements with a line of it, in the order they
// were confirmed.
func Statements(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, kind, orderID string) ([]*settlement.Statement, error) {
	t, c := ts(sc)
	where := ` AND order_id = $3`
	if kind == settlement.KindPurchase {
		where = ` AND statement_id IN (SELECT l.statement_id FROM brant_stl.settlement_line l WHERE l.tenant_id = $1 AND l.company_id = $2
			AND l.order_id = $3)`
	}
	rows, err := exec.Query(ctx, `SELECT `+statementColumns+` FROM brant_stl.settlement_statement WHERE tenant_id = $1 AND company_id = $2
		AND kind = $4`+where+` ORDER BY confirmed_at, statement_id`, t, c, orderID, kind)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*settlement.Statement, error) { return scanStatement(row) })
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		if err = completeStatement(ctx, exec, sc, s); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// SettledSources returns, of keys, the source lines a standing statement line settles, with that
// statement.
func SettledSources(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, keys []settlement.SourceKey) (map[settlement.SourceKey]string, error) {
	t, c := ts(sc)
	out := map[settlement.SourceKey]string{}
	if len(keys) == 0 {
		return out, nil
	}
	kinds, ids, lines := make([]string, len(keys)), make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		kinds[i], ids[i], lines[i] = k.Kind, k.ID, k.LineID
	}
	rows, err := exec.Query(ctx, `SELECT l.source_kind, l.source_id, l.source_line_id, l.statement_id FROM brant_stl.settlement_line l
		JOIN unnest($3::text[], $4::text[], $5::text[]) AS k(kind, id, line) ON l.source_kind = k.kind AND l.source_id = k.id AND l.source_line_id = k.line
		WHERE l.tenant_id = $1 AND l.company_id = $2 AND l.status = 'settled'`, t, c, kinds, ids, lines)
	if err != nil {
		return nil, err
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var k settlement.SourceKey
		var by string
		err := row.Scan(&k.Kind, &k.ID, &k.LineID, &by)
		out[k] = by
		return struct{}{}, err
	})
	return out, err
}

// SettledByStage returns, per stage ID, what the confirmed sales statements of order orderID put on
// it, in c.
func SettledByStage(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, c kernel.Currency) (map[string]kernel.Money, error) {
	t, s := ts(sc)
	rows, err := exec.Query(ctx, `SELECT i.stage_id, sum(i.amount_minor)::text FROM brant_stl.settlement_installment i
		JOIN brant_stl.settlement_statement st ON st.tenant_id = i.tenant_id AND st.company_id = i.company_id AND st.statement_id = i.statement_id
		WHERE i.tenant_id = $1 AND i.company_id = $2 AND i.order_id = $3 AND i.stage_id IS NOT NULL AND st.status = 'confirmed'
		GROUP BY i.stage_id`, t, s, orderID)
	if err != nil {
		return nil, err
	}
	out := map[string]kernel.Money{}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var id, amount string
		if err := row.Scan(&id, &amount); err != nil {
			return struct{}{}, err
		}
		m, err := money(c, amount)
		out[id] = m
		return struct{}{}, err
	})
	return out, err
}

// InstallmentApplied returns what stands applied on each installment of statement s, by seq.
func InstallmentApplied(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, s *settlement.Statement) (map[int]kernel.Money, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT installment_seq, sum(amount_minor)::text FROM brant_stl.application WHERE tenant_id = $1
		AND company_id = $2 AND statement_id = $3 AND status = 'applied' GROUP BY installment_seq`, t, c, s.ID)
	if err != nil {
		return nil, err
	}
	out := map[int]kernel.Money{}
	for _, i := range s.Installments {
		out[i.Seq], _ = kernel.NewMoney(s.Currency(), 0)
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var seq int
		var amount string
		if err := row.Scan(&seq, &amount); err != nil {
			return struct{}{}, err
		}
		m, err := money(s.Currency(), amount)
		out[seq] = m
		return struct{}{}, err
	})
	return out, err
}

// StatementApplications returns the applications of statement id, in the order they were made.
func StatementApplications(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string) ([]fa.Application, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT `+applicationColumns+applicationFrom+` WHERE a.tenant_id = $1 AND a.company_id = $2
		AND a.statement_id = $3 ORDER BY a.applied_at, a.application_id`, t, c, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (fa.Application, error) {
		x, err := scanApplication(row)
		if err != nil || x == nil {
			return fa.Application{}, err
		}
		return *x, nil
	})
}

// StandingStageApplications returns the stage applications of order orderID that stand, made at
// account revision revision or an earlier one, in the order they were made, with the fund records
// they draw on.
func StandingStageApplications(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, revision int64) ([]fa.Application, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT `+applicationColumns+applicationFrom+` WHERE a.tenant_id = $1 AND a.company_id = $2
		AND a.order_id = $3 AND a.target_kind = 'order_stage' AND a.status = 'applied' AND a.order_revision <= $4
		ORDER BY a.applied_at, a.application_id`, t, c, orderID, revision)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (fa.Application, error) {
		x, err := scanApplication(row)
		if err != nil || x == nil {
			return fa.Application{}, err
		}
		return *x, nil
	})
}

// StagePrepaid returns, per stage ID, what stands on order orderID's stages as stage applications
// (prepayments) made at account revision revision or an earlier one, in c.
func StagePrepaid(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, revision int64, c kernel.Currency) (map[string]kernel.Money, error) {
	apps, err := StandingStageApplications(ctx, exec, sc, orderID, revision)
	if err != nil {
		return nil, err
	}
	out := map[string]kernel.Money{}
	for _, x := range apps {
		cur, ok := out[x.StageID]
		if !ok {
			cur, _ = kernel.NewMoney(c, 0)
		}
		if out[x.StageID], err = cur.Add(x.Amount); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SaveMoved moves application x, standing at version 1, to moved.
func SaveMoved(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, x *fa.Application, at time.Time) error {
	t, c := ts(sc)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.application SET status = $4, version = $5, updated_at = $6 WHERE tenant_id = $1 AND company_id = $2
		AND application_id = $3 AND version = $5 - 1 AND status = 'applied'`, t, c, x.ID, x.Status, x.Version, at)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// OwedInstallments returns the installments of party's confirmed sales statements with what stands
// applied on each (the credit exposure's receivables, D-4).
func OwedInstallments(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, party string) ([]cc.OwedInstallment, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT s.order_id, s.currency, s.minor_units, coalesce(i.stage_trigger, ''), coalesce(i.release_control, ''),
		i.amount_minor::text, (SELECT coalesce(sum(a.amount_minor), 0) FROM brant_stl.application a WHERE a.tenant_id = i.tenant_id
			AND a.company_id = i.company_id AND a.statement_id = i.statement_id AND a.installment_seq = i.seq AND a.status = 'applied')::text, i.due_at
		FROM brant_stl.settlement_statement s JOIN brant_stl.settlement_installment i ON i.tenant_id = s.tenant_id AND i.company_id = s.company_id
			AND i.statement_id = s.statement_id
		WHERE s.tenant_id = $1 AND s.company_id = $2 AND s.counterparty_id = $3 AND s.kind = 'sales' AND s.status = 'confirmed'
		ORDER BY i.due_at, s.statement_id, i.seq`, t, c, party)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (cc.OwedInstallment, error) {
		var o cc.OwedInstallment
		var code, amount, applied string
		var units int16
		if err := row.Scan(&o.OrderID, &code, &units, &o.Trigger, &o.ReleaseControl, &amount, &applied, &o.DueAt); err != nil {
			return o, err
		}
		o.DueAt = kernel.Instant(o.DueAt)
		cur, err := currency(code, units)
		if err != nil {
			return o, err
		}
		if o.Amount, err = money(cur, amount); err != nil {
			return o, err
		}
		o.Applied, err = money(cur, applied)
		return o, err
	})
}
