package postgres

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/charge"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderresult"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Charges and order results (migrations/stl/000006).

func optFact(a *fundrecord.Act) (kind, id *string, epoch *int64, at *time.Time) {
	if a == nil {
		return nil, nil, nil, nil
	}
	k := string(a.By.Kind)
	return &k, &a.By.ID, &a.Epoch, &a.At
}

func optFrAct(kind, id *string, epoch *int64, at *time.Time, reason *string) *fundrecord.Act {
	if kind == nil || id == nil || epoch == nil || at == nil {
		return nil
	}
	a := frAct(*kind, *id, *epoch, *at, "")
	if reason != nil {
		a.Reason = *reason
	}
	return &a
}

// InsertCharge writes a new charge and its beneficiaries.
func InsertCharge(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, c *charge.Charge) error {
	t, co := ts(sc)
	cur := c.Currency()
	var estimate *string
	if c.Estimate != nil {
		e := c.Estimate.MinorString()
		estimate = &e
	}
	rateID, rateRevision, rateHash, rate, rateTo := rateArgs(c.Conversion.Rate)
	kind, id, epoch, at := fact(c.Recorded)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.charge (tenant_id, company_id, charge_id, status, version, source_kind, source_party_id,
		source_reference, fee_category_id, fee_category_revision, currency, minor_units, currency_revision, estimate_minor, amount_minor,
		conversion_kind, fx_rate_id, fx_rate_revision, fx_rate_hash, fx_rate, fx_target_currency, occurred_on, analysis_on, basis, evidence_ids,
		owner_actor_id, owner_team_id, recorded_by_kind, recorded_by_id, record_epoch, recorded_at, allocation_seq, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::text::numeric, $15::text::numeric, $16, $17, $18, $19,
		$20::text::numeric, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, 0, $31)`,
		t, co, c.ID, c.Status, c.Version, c.Source.Kind, nullable(c.Source.PartyID), nullable(c.Source.Reference), c.FeeCategory.ID,
		c.FeeCategory.Revision, cur.Code(), int16(cur.MinorUnits()), c.CurrencyRevision, estimate, c.Amount.MinorString(), c.Conversion.Kind,
		rateID, rateRevision, rateHash, rate, rateTo, c.OccurredOn, c.AnalysisOn, c.Basis, nonNil(c.EvidenceIDs), c.OwnerActor, team(c.OwnerTeam),
		kind, id, epoch, at)
	if err != nil {
		return err
	}
	for i, b := range c.Beneficiaries {
		if _, err = exec.Exec(ctx, `INSERT INTO brant_stl.charge_beneficiary (tenant_id, company_id, charge_id, seq, kind, order_id, shipment_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, t, co, c.ID, i+1, b.Kind, b.OrderID, nullable(b.ShipmentID)); err != nil {
			return err
		}
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func rateArgs(r *charge.RateUse) (id *string, revision *int64, hash, rate, to *string) {
	if r == nil {
		return nil, nil, nil, nil, nil
	}
	v := r.Value.String()
	return &r.ID, &r.Revision, &r.Hash, &v, &r.To
}

// SaveCharge moves charge c from version from to its state now (a bill, a confirmation or a new allocation).
func SaveCharge(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, c *charge.Charge, from int64, now time.Time) error {
	t, co := ts(sc)
	bKind, bID, bEpoch, bAt := optFact(c.Billed)
	cKind, cID, cEpoch, cAt := optFact(c.Confirmed)
	var reason *string
	var confirmEvidence []string
	if c.Confirmed != nil {
		reason, confirmEvidence = nullable(c.Confirmed.Reason), nonNil(c.ConfirmEvidence)
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.charge SET status = $4, version = $5, source_kind = $6, source_party_id = $7, source_reference = $8,
		estimate_reference = $9, amount_minor = $10::text::numeric, occurred_on = $11, analysis_on = $12, basis = $13, evidence_ids = $14,
		billed_by_kind = $15, billed_by_id = $16, bill_epoch = $17, billed_at = $18, confirmed_by_kind = $19, confirmed_by_id = $20,
		confirm_epoch = $21, confirmed_at = $22, confirm_evidence_ids = $23, confirm_reason = $24, allocation_seq = $25, updated_at = $26
		WHERE tenant_id = $1 AND company_id = $2 AND charge_id = $3 AND version = $27`,
		t, co, c.ID, c.Status, c.Version, c.Source.Kind, nullable(c.Source.PartyID), nullable(c.Source.Reference), nullable(c.EstimateReference),
		c.Amount.MinorString(), c.OccurredOn, c.AnalysisOn, c.Basis, nonNil(c.EvidenceIDs), bKind, bID, bEpoch, bAt, cKind, cID, cEpoch, cAt,
		confirmEvidence, reason, c.AllocationSeq, kernel.Instant(now), from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// InsertAllocation appends allocation a of charge chargeID with its lines.
func InsertAllocation(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, chargeID string, a *charge.Allocation) error {
	t, co := ts(sc)
	cur := a.Amount.Currency()
	kind, id, epoch, at := fact(a.By)
	if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.charge_allocation (tenant_id, company_id, charge_id, allocation_seq, charge_version, basis,
		data_basis, evidence_ids, currency, minor_units, amount_minor, allocated_by_kind, allocated_by_id, allocate_epoch, allocated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::text::numeric, $12, $13, $14, $15)`,
		t, co, chargeID, a.Seq, a.ChargeVersion, a.Basis, a.DataBasis, nonNil(a.EvidenceIDs), cur.Code(), int16(cur.MinorUnits()),
		a.Amount.MinorString(), kind, id, epoch, at); err != nil {
		return err
	}
	for _, l := range a.Lines {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.charge_allocation_line (tenant_id, company_id, charge_id, allocation_seq, line_id, seq,
			target_kind, order_id, shipment_id, weight, amount_minor) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::text::numeric,
			$11::text::numeric)`, t, co, chargeID, a.Seq, l.LineID, l.Seq, l.Target.Kind, l.Target.OrderID, nullable(l.Target.ShipmentID),
			l.Weight.String(), l.Amount.MinorString()); err != nil {
			return err
		}
	}
	return nil
}

const chargeColumns = `charge_id, status, version, source_kind, coalesce(source_party_id, ''), coalesce(source_reference, ''),
	coalesce(estimate_reference, ''), fee_category_id, fee_category_revision, currency, minor_units, currency_revision, estimate_minor::text,
	amount_minor::text, conversion_kind, fx_rate_id, fx_rate_revision, fx_rate_hash, fx_rate::text, fx_target_currency, occurred_on, analysis_on,
	basis, evidence_ids, owner_actor_id, coalesce(owner_team_id, ''), recorded_by_kind, recorded_by_id, record_epoch, recorded_at, billed_by_kind,
	billed_by_id, bill_epoch, billed_at, confirmed_by_kind, confirmed_by_id, confirm_epoch, confirmed_at, confirm_evidence_ids, confirm_reason,
	allocation_seq`

func scanCharge(row pgx.Row) (*charge.Charge, error) {
	var c charge.Charge
	var code, amount, rKind, rBy string
	var units int16
	var rEpoch int64
	var rAt time.Time
	var estimate, rateID, rateHash, rate, rateTo *string
	var rateRevision *int64
	var bKind, bBy, cKind, cBy, cReason *string
	var bEpoch, cEpoch *int64
	var bAt, cAt *time.Time
	err := row.Scan(&c.ID, &c.Status, &c.Version, &c.Source.Kind, &c.Source.PartyID, &c.Source.Reference, &c.EstimateReference, &c.FeeCategory.ID,
		&c.FeeCategory.Revision, &code, &units, &c.CurrencyRevision, &estimate, &amount, &c.Conversion.Kind, &rateID, &rateRevision, &rateHash, &rate,
		&rateTo, &c.OccurredOn, &c.AnalysisOn, &c.Basis, &c.EvidenceIDs, &c.OwnerActor, &c.OwnerTeam, &rKind, &rBy, &rEpoch, &rAt, &bKind, &bBy,
		&bEpoch, &bAt, &cKind, &cBy, &cEpoch, &cAt, &c.ConfirmEvidence, &cReason, &c.AllocationSeq)
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
	if c.Amount, err = money(cur, amount); err != nil {
		return nil, err
	}
	if estimate != nil {
		e, err := money(cur, *estimate)
		if err != nil {
			return nil, err
		}
		c.Estimate = &e
	}
	if rateID != nil && rateRevision != nil && rateHash != nil && rate != nil && rateTo != nil {
		v, err := kernel.ParseDecimal(*rate, kernel.DecimalFormat{IntDigits: 18, Scale: 12})
		if err != nil {
			return nil, err
		}
		c.Conversion.Rate = &charge.RateUse{ID: *rateID, Revision: *rateRevision, Hash: *rateHash, Value: v, To: *rateTo}
	}
	c.OccurredOn, c.AnalysisOn = c.OccurredOn.UTC(), c.AnalysisOn.UTC()
	c.Recorded = frAct(rKind, rBy, rEpoch, rAt, "")
	c.Billed = optFrAct(bKind, bBy, bEpoch, bAt, nil)
	c.Confirmed = optFrAct(cKind, cBy, cEpoch, cAt, cReason)
	if c.Confirmed != nil && cReason == nil {
		c.Confirmed.Reason = ""
	}
	return &c, nil
}

func completeCharge(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, c *charge.Charge) error {
	t, co := ts(sc)
	rows, err := exec.Query(ctx, `SELECT kind, order_id, coalesce(shipment_id, '') FROM brant_stl.charge_beneficiary WHERE tenant_id = $1
		AND company_id = $2 AND charge_id = $3 ORDER BY seq`, t, co, c.ID)
	if err != nil {
		return err
	}
	if c.Beneficiaries, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (charge.Target, error) {
		var b charge.Target
		err := row.Scan(&b.Kind, &b.OrderID, &b.ShipmentID)
		return b, err
	}); err != nil {
		return err
	}
	rows, err = exec.Query(ctx, `SELECT allocation_seq, charge_version, basis, data_basis, evidence_ids, amount_minor::text, allocated_by_kind,
		allocated_by_id, allocate_epoch, allocated_at FROM brant_stl.charge_allocation WHERE tenant_id = $1 AND company_id = $2 AND charge_id = $3
		ORDER BY allocation_seq`, t, co, c.ID)
	if err != nil {
		return err
	}
	cur := c.Currency()
	if c.Allocations, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (charge.Allocation, error) {
		var a charge.Allocation
		var amount, kind, by string
		var epoch int64
		var at time.Time
		if err := row.Scan(&a.Seq, &a.ChargeVersion, &a.Basis, &a.DataBasis, &a.EvidenceIDs, &amount, &kind, &by, &epoch, &at); err != nil {
			return a, err
		}
		a.By = frAct(kind, by, epoch, at, "")
		var err error
		a.Amount, err = money(cur, amount)
		return a, err
	}); err != nil {
		return err
	}
	rows, err = exec.Query(ctx, `SELECT allocation_seq, line_id, seq, target_kind, order_id, coalesce(shipment_id, ''), weight::text,
		amount_minor::text FROM brant_stl.charge_allocation_line WHERE tenant_id = $1 AND company_id = $2 AND charge_id = $3
		ORDER BY allocation_seq, seq`, t, co, c.ID)
	if err != nil {
		return err
	}
	type lineRow struct {
		seq  int
		line charge.Line
	}
	lines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (lineRow, error) {
		var x lineRow
		var w, amount string
		if err := row.Scan(&x.seq, &x.line.LineID, &x.line.Seq, &x.line.Target.Kind, &x.line.Target.OrderID, &x.line.Target.ShipmentID, &w,
			&amount); err != nil {
			return x, err
		}
		var err error
		if x.line.Weight, err = stored(w); err != nil {
			return x, err
		}
		x.line.Amount, err = money(cur, amount)
		return x, err
	})
	if err != nil {
		return err
	}
	for _, l := range lines {
		for i := range c.Allocations {
			if c.Allocations[i].Seq == l.seq {
				c.Allocations[i].Lines = append(c.Allocations[i].Lines, l.line)
			}
		}
	}
	return nil
}

// LoadCharge returns charge id with its beneficiaries and allocations, or nil; forUpdate locks it.
func LoadCharge(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string, forUpdate bool) (*charge.Charge, error) {
	t, co := ts(sc)
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	c, err := scanCharge(exec.QueryRow(ctx, `SELECT `+chargeColumns+` FROM brant_stl.charge WHERE tenant_id = $1 AND company_id = $2
		AND charge_id = $3`+lock, t, co, id))
	if err != nil || c == nil {
		return c, err
	}
	return c, completeCharge(ctx, exec, sc, c)
}

// ChargeBySource returns the charge recorded with source (kind, party, reference), "" when none.
func ChargeBySource(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, s charge.Source) (string, error) {
	t, co := ts(sc)
	var id string
	err := exec.QueryRow(ctx, `SELECT charge_id FROM brant_stl.charge WHERE tenant_id = $1 AND company_id = $2 AND source_kind = $3
		AND source_party_id IS NOT DISTINCT FROM $4 AND source_reference = $5`, t, co, s.Kind, nullable(s.PartyID), s.Reference).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// OrderCharges returns the charges with a beneficiary of order orderID, by charge ID.
func OrderCharges(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string) ([]*charge.Charge, error) {
	t, co := ts(sc)
	rows, err := exec.Query(ctx, `SELECT `+chargeColumns+` FROM brant_stl.charge c WHERE tenant_id = $1 AND company_id = $2 AND EXISTS (
		SELECT 1 FROM brant_stl.charge_beneficiary b WHERE b.tenant_id = c.tenant_id AND b.company_id = c.company_id AND b.charge_id = c.charge_id
		AND b.order_id = $3) ORDER BY charge_id`, t, co, orderID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*charge.Charge, error) { return scanCharge(row) })
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		if err = completeCharge(ctx, exec, sc, c); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// EnsureResults creates, in order ID order, the order results of orders that have none yet, open at version 0 (the
// lock anchor), owned by owner and team.
func EnsureResults(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orders []string, owner, team string, now time.Time) error {
	t, co := ts(sc)
	ids := append([]string(nil), orders...)
	sort.Strings(ids)
	for _, id := range ids {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.order_result (tenant_id, company_id, order_id, status, version, owner_actor_id,
			owner_team_id, created_at, updated_at) VALUES ($1, $2, $3, 'open', 0, $4, $5, $6, $6) ON CONFLICT DO NOTHING`, t, co, id, owner,
			nullable(team), kernel.Instant(now)); err != nil {
			return err
		}
	}
	return nil
}

// Locks a row may be read with.
const (
	NoLock    = ""
	ForUpdate = " FOR UPDATE"
	ForShare  = " FOR SHARE"
)

// LoadResult returns the order result of order orderID with its reviews, snapshots and reopenings, or nil, read
// with lock (NoLock, ForUpdate, ForShare).
func LoadResult(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID, lock string) (*orderresult.Result, error) {
	t, co := ts(sc)
	var r orderresult.Result
	err := exec.QueryRow(ctx, `SELECT order_id, status, version, owner_actor_id, coalesce(owner_team_id, ''), created_at FROM brant_stl.order_result
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3`+lock, t, co, orderID).Scan(&r.OrderID, &r.Status, &r.Version, &r.OwnerActor,
		&r.OwnerTeam, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := exec.Query(ctx, `SELECT seq, order_revision, summary::text, summary_sha256, coalesce(note, ''), reviewed_by_kind, reviewed_by_id,
		review_epoch, reviewed_at FROM brant_stl.order_result_review WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY seq`,
		t, co, orderID)
	if err != nil {
		return nil, err
	}
	if r.Reviews, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (orderresult.Review, error) {
		var v orderresult.Review
		var summary, kind, by string
		var epoch int64
		var at time.Time
		err := row.Scan(&v.Seq, &v.OrderRevision, &summary, &v.SummarySHA256, &v.Note, &kind, &by, &epoch, &at)
		v.Summary, v.By = []byte(summary), frAct(kind, by, epoch, at, "")
		return v, err
	}); err != nil {
		return nil, err
	}
	rows, err = exec.Query(ctx, `SELECT seq, order_revision, order_hash, currency, minor_units, net_sales_minor::text, result_minor::text,
		content::text, content_sha256, coalesce(reason, ''), closed_by_kind, closed_by_id, close_epoch, closed_at FROM brant_stl.order_result_snapshot
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY seq`, t, co, orderID)
	if err != nil {
		return nil, err
	}
	if r.Snapshots, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (orderresult.Snapshot, error) {
		var s orderresult.Snapshot
		var code, net, result, content, kind, by string
		var units int16
		var epoch int64
		var at time.Time
		if err := row.Scan(&s.Seq, &s.OrderRevision, &s.OrderHash, &code, &units, &net, &result, &content, &s.ContentSHA256, &s.Reason, &kind,
			&by, &epoch, &at); err != nil {
			return s, err
		}
		s.Content, s.By = []byte(content), frAct(kind, by, epoch, at, s.Reason)
		var err error
		if s.Currency, err = currency(code, units); err != nil {
			return s, err
		}
		if s.NetSales, err = money(s.Currency, net); err != nil {
			return s, err
		}
		s.Result, err = money(s.Currency, result)
		return s, err
	}); err != nil {
		return nil, err
	}
	rows, err = exec.Query(ctx, `SELECT seq, reason, evidence_ids, reopened_by_kind, reopened_by_id, reopen_epoch, reopened_at
		FROM brant_stl.order_result_reopening WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY seq`, t, co, orderID)
	if err != nil {
		return nil, err
	}
	if r.Reopenings, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (orderresult.Reopening, error) {
		var v orderresult.Reopening
		var kind, by string
		var epoch int64
		var at time.Time
		err := row.Scan(&v.Seq, &v.Reason, &v.EvidenceIDs, &kind, &by, &epoch, &at)
		v.By = frAct(kind, by, epoch, at, v.Reason)
		return v, err
	}); err != nil {
		return nil, err
	}
	r.CreatedAt = kernel.Instant(r.CreatedAt)
	return &r, nil
}

// SaveResult moves order result r from version from to its state now.
func SaveResult(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, r *orderresult.Result, from int64, now time.Time) error {
	t, co := ts(sc)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.order_result SET status = $4, version = $5, updated_at = $6 WHERE tenant_id = $1 AND company_id = $2
		AND order_id = $3 AND version = $7`, t, co, r.OrderID, r.Status, r.Version, kernel.Instant(now), from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// InsertResultReview appends review v of order result orderID.
func InsertResultReview(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, v orderresult.Review) error {
	t, co := ts(sc)
	kind, id, epoch, at := fact(v.By)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.order_result_review (tenant_id, company_id, order_id, seq, order_revision, summary,
		summary_sha256, note, reviewed_by_kind, reviewed_by_id, review_epoch, reviewed_at) VALUES ($1, $2, $3, $4, $5, $6::text::jsonb, $7, $8, $9,
		$10, $11, $12)`, t, co, orderID, v.Seq, v.OrderRevision, string(v.Summary), v.SummarySHA256, nullable(v.Note), kind, id, epoch, at)
	return err
}

// InsertSnapshot appends closing snapshot s of order result orderID.
func InsertSnapshot(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, s orderresult.Snapshot) error {
	t, co := ts(sc)
	kind, id, epoch, at := fact(s.By)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.order_result_snapshot (tenant_id, company_id, order_id, seq, order_revision, order_hash, currency,
		minor_units, net_sales_minor, result_minor, content, content_sha256, reason, closed_by_kind, closed_by_id, close_epoch, closed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::text::numeric, $10::text::numeric, $11::text::jsonb, $12, $13, $14, $15, $16, $17)`,
		t, co, orderID, s.Seq, s.OrderRevision, s.OrderHash, s.Currency.Code(), int16(s.Currency.MinorUnits()), s.NetSales.MinorString(),
		s.Result.MinorString(), string(s.Content), s.ContentSHA256, nullable(s.Reason), kind, id, epoch, at)
	return err
}

// InsertReopening appends reopening v of order result orderID.
func InsertReopening(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, v orderresult.Reopening) error {
	t, co := ts(sc)
	kind, id, epoch, at := fact(v.By)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.order_result_reopening (tenant_id, company_id, order_id, seq, reason, evidence_ids,
		reopened_by_kind, reopened_by_id, reopen_epoch, reopened_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		t, co, orderID, v.Seq, v.Reason, nonNil(v.EvidenceIDs), kind, id, epoch, at)
	return err
}

// LockSettlementRoots locks, FOR SHARE in ID order, the order account of order orderID and the statements named:
// what order_result.close judges stays as read until it commits — applications, unapplications, credit notes,
// reversals and new sales statements of the order lock the same rows FOR UPDATE.
func LockSettlementRoots(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, statements []string) error {
	t, co := ts(sc)
	type root struct{ id, table, key string }
	roots := []root{{orderID, "order_account", "order_id"}}
	for _, s := range statements {
		roots = append(roots, root{s, "settlement_statement", "statement_id"})
	}
	sort.SliceStable(roots, func(i, j int) bool { return roots[i].id < roots[j].id })
	for _, r := range roots {
		rows, err := exec.Query(ctx, `SELECT 1 FROM brant_stl.`+r.table+` WHERE tenant_id = $1 AND company_id = $2 AND `+r.key+` = $3 FOR SHARE`,
			t, co, r.id)
		if err != nil {
			return err
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
	}
	return nil
}
