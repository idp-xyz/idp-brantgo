package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	fr "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	oa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Fund records, order accounts and applications (migrations/stl/000002_stl_funds_applications.sql).

// Subjects of the fund side for Visible.
const (
	SubjectFundRecord   = "fund_record"
	SubjectOrderAccount = "order_account"
	SubjectApplication  = "application"
)

var rateFormat = kernel.DecimalFormat{IntDigits: 8, Scale: 10}

func fact(a fr.Act) (kind, id string, epoch int64, at time.Time) {
	return string(a.By.Kind), a.By.ID, a.Epoch, a.At
}

func frAct(kind, id string, epoch int64, at time.Time, reason string) fr.Act {
	return fr.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(kind), ID: id}, Epoch: epoch, At: kernel.Instant(at), Reason: reason}
}

func team(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// InsertFund writes a new fund record.
func InsertFund(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, f *fr.FundRecord) error {
	t, c := ts(sc)
	var soldCur *string
	var soldUnits *int16
	var sold, rate, fee *string
	if x := f.FX; x != nil {
		code, units, s, r, e := x.Sold.Currency().Code(), int16(x.Sold.Currency().MinorUnits()), x.Sold.MinorString(), x.Rate.String(), x.Fee.MinorString()
		soldCur, soldUnits, sold, rate, fee = &code, &units, &s, &r, &e
	}
	kind, id, epoch, at := fact(f.Recorded)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.fund_record (tenant_id, company_id, fund_record_id, kind, direction, status, version,
		counterparty_id, currency, minor_units, currency_revision, bank_account_id, bank_account_revision, received_minor, remitted_minor,
		bank_charges_minor, value_date, bank_reference, evidence_ids, source_fund_record_id, sold_currency, sold_minor_units, sold_minor, bank_rate,
		fee_minor, owner_actor_id, owner_team_id, recorded_by_kind, recorded_by_id, record_epoch, recorded_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::text::numeric, $15::text::numeric, $16::text::numeric, $17, $18, $19,
		$20, $21, $22, $23::text::numeric, $24::text::numeric, $25::text::numeric, $26, $27, $28, $29, $30, $31, $31)`,
		t, c, f.ID, f.Kind, f.Direction, f.Status, f.Version, nullable(f.CounterpartyID), f.Currency().Code(), int16(f.Currency().MinorUnits()),
		f.CurrencyRevision, f.BankAccountID, f.BankAccountRevision, f.Received.MinorString(), f.Remitted.MinorString(), f.BankCharges.MinorString(),
		kernel.Instant(f.ValueDate), f.BankReference, f.EvidenceIDs, nullable(f.SourceID), soldCur, soldUnits, sold, rate, fee, f.OwnerActor,
		team(f.OwnerTeam), kind, id, epoch, at)
	return err
}

const fundColumns = `fund_record_id, kind, direction, status, version, coalesce(counterparty_id, ''), currency, minor_units, currency_revision,
	bank_account_id, bank_account_revision, received_minor::text, remitted_minor::text, bank_charges_minor::text, value_date, bank_reference,
	evidence_ids, coalesce(source_fund_record_id, ''), sold_currency, sold_minor_units, sold_minor::text, bank_rate::text, fee_minor::text,
	owner_actor_id, coalesce(owner_team_id, ''), recorded_by_kind, recorded_by_id, record_epoch, recorded_at`

func scanFund(row pgx.Row) (*fr.FundRecord, error) {
	var f fr.FundRecord
	var code, received, remitted, charges, rKind, rID string
	var units int16
	var soldCur, sold, rate, fee *string
	var soldUnits *int16
	var epoch int64
	var at time.Time
	err := row.Scan(&f.ID, &f.Kind, &f.Direction, &f.Status, &f.Version, &f.CounterpartyID, &code, &units, &f.CurrencyRevision, &f.BankAccountID,
		&f.BankAccountRevision, &received, &remitted, &charges, &f.ValueDate, &f.BankReference, &f.EvidenceIDs, &f.SourceID, &soldCur, &soldUnits,
		&sold, &rate, &fee, &f.OwnerActor, &f.OwnerTeam, &rKind, &rID, &epoch, &at)
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
	if f.Received, err = money(cur, received); err != nil {
		return nil, err
	}
	if f.Remitted, err = money(cur, remitted); err != nil {
		return nil, err
	}
	if f.BankCharges, err = money(cur, charges); err != nil {
		return nil, err
	}
	f.ValueDate, f.Recorded = kernel.Instant(f.ValueDate), frAct(rKind, rID, epoch, at, "")
	if soldCur != nil && soldUnits != nil && sold != nil && rate != nil && fee != nil {
		sc, err := currency(*soldCur, *soldUnits)
		if err != nil {
			return nil, err
		}
		x := &fr.FX{}
		if x.Sold, err = money(sc, *sold); err != nil {
			return nil, err
		}
		if x.Fee, err = money(cur, *fee); err != nil {
			return nil, err
		}
		if x.Rate, err = kernel.ParseDecimal(*rate, rateFormat); err != nil {
			return nil, err
		}
		f.FX = x
	}
	return &f, nil
}

// LoadFund returns fund record id with its resolutions, or nil; forUpdate locks its row.
func LoadFund(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string, forUpdate bool) (*fr.FundRecord, error) {
	t, c := ts(sc)
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	f, err := scanFund(exec.QueryRow(ctx, `SELECT `+fundColumns+` FROM brant_stl.fund_record WHERE tenant_id = $1 AND company_id = $2
		AND fund_record_id = $3`+lock, t, c, id))
	if err != nil || f == nil {
		return f, err
	}
	rows, err := exec.Query(ctx, `SELECT seq, resolution, amount_minor::text, evidence_ids, reason, resolved_by_kind, resolved_by_id, resolve_epoch,
		resolved_at FROM brant_stl.fund_difference_resolution WHERE tenant_id = $1 AND company_id = $2 AND fund_record_id = $3 ORDER BY seq`, t, c, id)
	if err != nil {
		return nil, err
	}
	f.Resolutions, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (fr.Resolution, error) {
		var r fr.Resolution
		var amount, reason, kind, by string
		var epoch int64
		var at time.Time
		if err := row.Scan(&r.Seq, &r.Resolution, &amount, &r.EvidenceIDs, &reason, &kind, &by, &epoch, &at); err != nil {
			return r, err
		}
		r.By = frAct(kind, by, epoch, at, reason)
		var err error
		r.Amount, err = money(f.Currency(), amount)
		return r, err
	})
	return f, err
}

// FundByReference returns the fund record the bank reference is recorded on for the account, "" when none.
func FundByReference(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, account, reference string) (string, error) {
	t, c := ts(sc)
	var id string
	err := exec.QueryRow(ctx, `SELECT fund_record_id FROM brant_stl.fund_record WHERE tenant_id = $1 AND company_id = $2 AND bank_account_id = $3
		AND bank_reference = $4`, t, c, account, reference).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Draw is a refund, returned remittance or conversion drawn on a fund record: its amount in the
// source's currency (a conversion's sold amount).
type Draw struct {
	FundRecordID string
	Kind         string
	Amount       kernel.Money
}

// Draws returns what is drawn on fund record f.
func Draws(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, f *fr.FundRecord) ([]Draw, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT fund_record_id, kind, CASE WHEN kind = 'fx_conversion' THEN sold_minor ELSE received_minor END::text
		FROM brant_stl.fund_record WHERE tenant_id = $1 AND company_id = $2 AND source_fund_record_id = $3 ORDER BY recorded_at, fund_record_id`,
		t, c, f.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Draw, error) {
		var d Draw
		var amount string
		if err := row.Scan(&d.FundRecordID, &d.Kind, &amount); err != nil {
			return d, err
		}
		var err error
		d.Amount, err = money(f.Currency(), amount)
		return d, err
	})
}

// FundEntries sums what is drawn on fund record f and applied from it.
func FundEntries(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, f *fr.FundRecord) (fr.Entries, error) {
	e := fr.NoEntries(f.Currency())
	draws, err := Draws(ctx, exec, sc, f)
	if err != nil {
		return e, err
	}
	for _, d := range draws {
		if d.Kind == fr.KindFXConversion {
			e.Sold, err = e.Sold.Add(d.Amount)
		} else {
			e.Drawn, err = e.Drawn.Add(d.Amount)
		}
		if err != nil {
			return e, err
		}
	}
	t, c := ts(sc)
	var applied string
	if err = exec.QueryRow(ctx, `SELECT coalesce(sum(amount_minor), 0)::text FROM brant_stl.application WHERE tenant_id = $1 AND company_id = $2
		AND fund_record_id = $3 AND status = 'applied'`, t, c, f.ID).Scan(&applied); err != nil {
		return e, err
	}
	e.Applied, err = money(f.Currency(), applied)
	return e, err
}

// SaveResolution moves fund record f from version from to its version and appends its last
// resolution.
func SaveResolution(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, f *fr.FundRecord, from int64) error {
	t, c := ts(sc)
	r := f.Resolutions[len(f.Resolutions)-1]
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.fund_record SET status = $4, version = $5, updated_at = $6 WHERE tenant_id = $1 AND company_id = $2
		AND fund_record_id = $3 AND version = $7`, t, c, f.ID, f.Status, f.Version, r.By.At, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	kind, id, epoch, at := fact(r.By)
	_, err = exec.Exec(ctx, `INSERT INTO brant_stl.fund_difference_resolution (tenant_id, company_id, fund_record_id, seq, resolution, amount_minor,
		reason, evidence_ids, resolved_by_kind, resolved_by_id, resolve_epoch, resolved_at) VALUES ($1, $2, $3, $4, $5, $6::text::numeric, $7, $8, $9,
		$10, $11, $12)`, t, c, f.ID, r.Seq, r.Resolution, r.Amount.MinorString(), r.By.Reason, r.EvidenceIDs, kind, id, epoch, at)
	return err
}

// InsertAccount writes a new order account and its stages.
func InsertAccount(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, a *oa.Account) error {
	t, c := ts(sc)
	cur := a.Currency()
	kind, id, epoch, at := fact(a.Opened)
	if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.order_account (tenant_id, company_id, order_id, order_revision, order_hash, party_id, currency,
		minor_units, currency_revision, status, version, owner_actor_id, owner_team_id, opened_by_kind, opened_by_id, open_epoch, opened_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $17)`, t, c, a.OrderID, a.OrderRevision, a.OrderHash,
		a.PartyID, cur.Code(), int16(cur.MinorUnits()), a.CurrencyRevision, a.Status, a.Version, a.OwnerActor, team(a.OwnerTeam), kind, id, epoch,
		at); err != nil {
		return err
	}
	for _, s := range a.Stages {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.order_account_stage (tenant_id, company_id, order_id, order_revision, stage_id, seq,
			stage_trigger, days, percent, amount_minor, release_control) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::text::numeric, $11)`,
			t, c, a.OrderID, a.OrderRevision, s.StageID, s.Seq, s.Trigger, s.Days, s.Percent, s.Amount.MinorString(), s.ReleaseControl); err != nil {
			return err
		}
	}
	return nil
}

// LoadAccount returns the order account of orderID with the stages of its revision, or nil;
// forUpdate locks its row.
func LoadAccount(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, forUpdate bool) (*oa.Account, error) {
	t, c := ts(sc)
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var a oa.Account
	var code, kind, by string
	var units int16
	var epoch int64
	var at time.Time
	err := exec.QueryRow(ctx, `SELECT order_id, order_revision, order_hash, party_id, currency, minor_units, currency_revision, status, version,
		owner_actor_id, coalesce(owner_team_id, ''), opened_by_kind, opened_by_id, open_epoch, opened_at FROM brant_stl.order_account
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3`+lock, t, c, orderID).Scan(&a.OrderID, &a.OrderRevision, &a.OrderHash, &a.PartyID,
		&code, &units, &a.CurrencyRevision, &a.Status, &a.Version, &a.OwnerActor, &a.OwnerTeam, &kind, &by, &epoch, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.Opened = frAct(kind, by, epoch, at, "")
	cur, err := currency(code, units)
	if err != nil {
		return nil, err
	}
	rows, err := exec.Query(ctx, `SELECT stage_id, seq, stage_trigger, days, percent, amount_minor::text, release_control FROM brant_stl.order_account_stage
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 AND order_revision = $4 ORDER BY seq`, t, c, orderID, a.OrderRevision)
	if err != nil {
		return nil, err
	}
	a.Stages, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (oa.Stage, error) {
		var s oa.Stage
		var amount string
		if err := row.Scan(&s.StageID, &s.Seq, &s.Trigger, &s.Days, &s.Percent, &amount, &s.ReleaseControl); err != nil {
			return s, err
		}
		var err error
		s.Amount, err = money(cur, amount)
		return s, err
	})
	return &a, err
}

// StageApplied returns what stands applied on each stage of account a.
func StageApplied(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, a *oa.Account) (map[string]kernel.Money, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT stage_id, sum(amount_minor)::text FROM brant_stl.application WHERE tenant_id = $1 AND company_id = $2
		AND order_id = $3 AND order_revision = $4 AND status = 'applied' GROUP BY stage_id`, t, c, a.OrderID, a.OrderRevision)
	if err != nil {
		return nil, err
	}
	out := map[string]kernel.Money{}
	for _, s := range a.Stages {
		out[s.StageID], _ = kernel.NewMoney(a.Currency(), 0)
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var id, amount string
		if err := row.Scan(&id, &amount); err != nil {
			return struct{}{}, err
		}
		m, err := money(a.Currency(), amount)
		out[id] = m
		return struct{}{}, err
	})
	return out, err
}

// InsertApplication writes a new application.
func InsertApplication(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, x *fa.Application) error {
	t, c := ts(sc)
	kind, id, epoch, at := fact(x.Applied)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.application (tenant_id, company_id, application_id, fund_record_id, target_kind, order_id,
		order_revision, stage_id, currency, minor_units, amount_minor, status, version, owner_actor_id, owner_team_id, applied_by_kind, applied_by_id,
		apply_epoch, applied_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::text::numeric, $12, $13, $14, $15, $16, $17, $18,
		$19, $19)`, t, c, x.ID, x.FundRecordID, x.TargetKind, x.OrderID, x.OrderRevision, x.StageID, x.Amount.Currency().Code(),
		int16(x.Amount.Currency().MinorUnits()), x.Amount.MinorString(), x.Status, x.Version, x.OwnerActor, team(x.OwnerTeam), kind, id, epoch, at)
	return err
}

const applicationColumns = `a.application_id, a.fund_record_id, a.target_kind, coalesce(a.order_id, ''), coalesce(a.order_revision, 0),
	coalesce(a.stage_id, ''), a.currency, a.minor_units, a.amount_minor::text, a.status, a.version, a.owner_actor_id, coalesce(a.owner_team_id, ''),
	a.applied_by_kind, a.applied_by_id, a.apply_epoch, a.applied_at, u.unapplied_by_kind, u.unapplied_by_id, u.unapply_epoch, u.unapplied_at, u.reason`

const applicationFrom = ` FROM brant_stl.application a LEFT JOIN brant_stl.application_unapply u ON u.tenant_id = a.tenant_id
	AND u.company_id = a.company_id AND u.application_id = a.application_id`

func scanApplication(row pgx.Row) (*fa.Application, error) {
	var x fa.Application
	var code, amount, kind, by string
	var units int16
	var epoch int64
	var at time.Time
	var uKind, uBy, uReason *string
	var uEpoch *int64
	var uAt *time.Time
	err := row.Scan(&x.ID, &x.FundRecordID, &x.TargetKind, &x.OrderID, &x.OrderRevision, &x.StageID, &code, &units, &amount, &x.Status, &x.Version,
		&x.OwnerActor, &x.OwnerTeam, &kind, &by, &epoch, &at, &uKind, &uBy, &uEpoch, &uAt, &uReason)
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
	if x.Amount, err = money(cur, amount); err != nil {
		return nil, err
	}
	x.Applied = frAct(kind, by, epoch, at, "")
	if uKind != nil && uBy != nil && uEpoch != nil && uAt != nil && uReason != nil {
		u := frAct(*uKind, *uBy, *uEpoch, *uAt, *uReason)
		x.Unapplied = &u
	}
	return &x, nil
}

// LoadApplication returns application id, or nil; forUpdate locks its row.
func LoadApplication(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string, forUpdate bool) (*fa.Application, error) {
	t, c := ts(sc)
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE OF a"
	}
	return scanApplication(exec.QueryRow(ctx, `SELECT `+applicationColumns+applicationFrom+` WHERE a.tenant_id = $1 AND a.company_id = $2
		AND a.application_id = $3`+lock, t, c, id))
}

// Applications returns the applications of fund record fundID (when not empty) or of order orderID,
// in the order they were made.
func Applications(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, fundID, orderID string) ([]fa.Application, error) {
	t, c := ts(sc)
	where := ` WHERE a.tenant_id = $1 AND a.company_id = $2 AND a.fund_record_id = $3`
	key := fundID
	if fundID == "" {
		where, key = ` WHERE a.tenant_id = $1 AND a.company_id = $2 AND a.order_id = $3`, orderID
	}
	rows, err := exec.Query(ctx, `SELECT `+applicationColumns+applicationFrom+where+` ORDER BY a.applied_at, a.application_id`, t, c, key)
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

// SaveUnapply moves application x from version 1 to unapplied and appends its unapply record.
func SaveUnapply(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, x *fa.Application) error {
	t, c := ts(sc)
	u := x.Unapplied
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.application SET status = $4, version = $5, updated_at = $6 WHERE tenant_id = $1 AND company_id = $2
		AND application_id = $3 AND version = $5 - 1`, t, c, x.ID, x.Status, x.Version, u.At)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	kind, id, epoch, at := fact(*u)
	_, err = exec.Exec(ctx, `INSERT INTO brant_stl.application_unapply (tenant_id, company_id, application_id, reason, unapplied_by_kind,
		unapplied_by_id, unapply_epoch, unapplied_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, t, c, x.ID, u.Reason, kind, id, epoch, at)
	return err
}
