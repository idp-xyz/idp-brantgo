package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Credit notes, settlement reversals and the conversions of applications (migrations/stl/000005).

// conversionRow is an application's conversion as stored: NULL columns without one.
type conversionRow struct {
	currency, amount, rateID, rateHash, rate, analysisCurrency, analysisRate, difference *string
	units, analysisUnits                                                                 *int16
	rateRevision                                                                         *int64
}

func conversionArgs(c *fa.Conversion) conversionRow {
	var r conversionRow
	if c == nil {
		return r
	}
	code, units, amount, rate := c.Source.Currency().Code(), int16(c.Source.Currency().MinorUnits()), c.Source.MinorString(), c.Value.String()
	id, revision, hash := c.Rate.ID, c.Rate.Revision, c.Rate.Hash
	r.currency, r.units, r.amount, r.rateID, r.rateRevision, r.rateHash, r.rate = &code, &units, &amount, &id, &revision, &hash, &rate
	if a := c.Analysis; a != nil {
		ac, au, ar := a.Currency.Code(), int16(a.Currency.MinorUnits()), a.Rate.String()
		r.analysisCurrency, r.analysisUnits, r.analysisRate = &ac, &au, &ar
		if a.Difference != nil {
			d := a.Difference.MinorString()
			r.difference = &d
		}
	}
	return r
}

func (r conversionRow) conversion() (*fa.Conversion, error) {
	if r.currency == nil || r.units == nil || r.amount == nil || r.rateID == nil || r.rateRevision == nil || r.rateHash == nil || r.rate == nil {
		return nil, nil
	}
	cur, err := currency(*r.currency, *r.units)
	if err != nil {
		return nil, err
	}
	c := &fa.Conversion{Rate: fa.RateUse{ID: *r.rateID, Revision: *r.rateRevision, Hash: *r.rateHash}}
	if c.Source, err = money(cur, *r.amount); err != nil {
		return nil, err
	}
	if c.Value, err = kernel.ParseDecimal(*r.rate, kernel.FactorFormat); err != nil {
		return nil, err
	}
	if r.analysisCurrency == nil || r.analysisUnits == nil || r.analysisRate == nil {
		return c, nil
	}
	a := &fa.Analysis{}
	if a.Currency, err = currency(*r.analysisCurrency, *r.analysisUnits); err != nil {
		return nil, err
	}
	if a.Rate, err = kernel.ParseDecimal(*r.analysisRate, kernel.FactorFormat); err != nil {
		return nil, err
	}
	if r.difference != nil {
		d, err := money(a.Currency, *r.difference)
		if err != nil {
			return nil, err
		}
		a.Difference = &d
	}
	c.Analysis = a
	return c, nil
}

// InsertCreditNote writes a new credit note.
func InsertCreditNote(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, n *settlement.CreditNote) error {
	t, c := ts(sc)
	cur := n.Amount.Currency()
	kind, id, epoch, at := fact(n.Requested)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.credit_note (tenant_id, company_id, credit_note_id, statement_id, order_id, kind, status, version,
		currency, minor_units, amount_minor, basis, evidence_ids, owner_actor_id, owner_team_id, requested_by_kind, requested_by_id, request_epoch,
		requested_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::text::numeric, $12, $13, $14, $15, $16, $17, $18, $19, $19)`,
		t, c, n.ID, n.StatementID, n.OrderID, n.Kind, n.Status, n.Version, cur.Code(), int16(cur.MinorUnits()), n.Amount.MinorString(), n.Basis,
		n.EvidenceIDs, n.OwnerActor, team(n.OwnerTeam), kind, id, epoch, at)
	return err
}

// SaveCreditDecision moves credit note n from version from to its decided state.
func SaveCreditDecision(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, n *settlement.CreditNote, from int64) error {
	t, c := ts(sc)
	kind, id, epoch, at := fact(*n.Decided)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.credit_note SET status = $4, version = $5, decided_by_kind = $6, decided_by_id = $7, decide_epoch = $8,
		decided_at = $9, decision_reason = $10, updated_at = $9 WHERE tenant_id = $1 AND company_id = $2 AND credit_note_id = $3 AND version = $11`,
		t, c, n.ID, n.Status, n.Version, kind, id, epoch, at, n.Decided.Reason, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

const creditNoteColumns = `credit_note_id, statement_id, order_id, kind, status, version, currency, minor_units, amount_minor::text, basis,
	evidence_ids, owner_actor_id, coalesce(owner_team_id, ''), requested_by_kind, requested_by_id, request_epoch, requested_at, decided_by_kind,
	decided_by_id, decide_epoch, decided_at, decision_reason`

func scanCreditNote(row pgx.Row) (*settlement.CreditNote, error) {
	var n settlement.CreditNote
	var code, amount, kind, by string
	var units int16
	var epoch int64
	var at time.Time
	var dKind, dBy, dReason *string
	var dEpoch *int64
	var dAt *time.Time
	err := row.Scan(&n.ID, &n.StatementID, &n.OrderID, &n.Kind, &n.Status, &n.Version, &code, &units, &amount, &n.Basis, &n.EvidenceIDs, &n.OwnerActor,
		&n.OwnerTeam, &kind, &by, &epoch, &at, &dKind, &dBy, &dEpoch, &dAt, &dReason)
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
	if n.Amount, err = money(cur, amount); err != nil {
		return nil, err
	}
	n.Requested = frAct(kind, by, epoch, at, "")
	if dKind != nil && dBy != nil && dEpoch != nil && dAt != nil && dReason != nil {
		d := frAct(*dKind, *dBy, *dEpoch, *dAt, *dReason)
		n.Decided = &d
	}
	return &n, nil
}

// LoadCreditNote returns credit note id, or nil. Its statement's lock guards it (an entity of the statement).
func LoadCreditNote(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string) (*settlement.CreditNote, error) {
	t, c := ts(sc)
	return scanCreditNote(exec.QueryRow(ctx, `SELECT `+creditNoteColumns+` FROM brant_stl.credit_note WHERE tenant_id = $1 AND company_id = $2
		AND credit_note_id = $3`, t, c, id))
}

// CreditNotes returns the credit notes of statement statementID (when not empty) or of order orderID, in the
// order they were requested.
func CreditNotes(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, statementID, orderID string) ([]settlement.CreditNote, error) {
	t, c := ts(sc)
	where, key := ` AND statement_id = $3`, statementID
	if statementID == "" {
		where, key = ` AND order_id = $3`, orderID
	}
	rows, err := exec.Query(ctx, `SELECT `+creditNoteColumns+` FROM brant_stl.credit_note WHERE tenant_id = $1 AND company_id = $2`+where+`
		ORDER BY requested_at, credit_note_id`, t, c, key)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (settlement.CreditNote, error) {
		n, err := scanCreditNote(row)
		if err != nil || n == nil {
			return settlement.CreditNote{}, err
		}
		return *n, nil
	})
}

// CreditNoteApplied returns what stands applied from each credit note of statement statementID, in c.
func CreditNoteApplied(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, statementID string, c kernel.Currency) (map[string]kernel.Money, error) {
	t, s := ts(sc)
	rows, err := exec.Query(ctx, `SELECT a.credit_note_id, sum(a.amount_minor)::text FROM brant_stl.application a WHERE a.tenant_id = $1
		AND a.company_id = $2 AND a.credit_note_id IN (SELECT n.credit_note_id FROM brant_stl.credit_note n WHERE n.tenant_id = $1 AND n.company_id = $2
		AND n.statement_id = $3) AND a.status = 'applied' GROUP BY a.credit_note_id`, t, s, statementID)
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

// CreditNoteApplications returns the applications of credit note id, in the order they were made.
func CreditNoteApplications(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string) ([]fa.Application, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT `+applicationColumns+applicationFrom+` WHERE a.tenant_id = $1 AND a.company_id = $2
		AND a.credit_note_id = $3 ORDER BY a.applied_at, a.application_id`, t, c, id)
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

// BumpStatement moves statement s, at version from, to its version and state (a credit note requested or
// decided: confirmed, version + 1; a reversal: reversed).
func BumpStatement(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, s *settlement.Statement, from int64, at time.Time) error {
	t, c := ts(sc)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.settlement_statement SET status = $4, version = $5, updated_at = $6 WHERE tenant_id = $1
		AND company_id = $2 AND statement_id = $3 AND version = $7`, t, c, s.ID, s.Status, s.Version, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// SaveReversal records the reversal of statement s, already moved to reversed with BumpStatement: its lines
// move to reversed and the reversal is appended.
func SaveReversal(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, s *settlement.Statement, r settlement.Reversal) error {
	t, c := ts(sc)
	if _, err := exec.Exec(ctx, `UPDATE brant_stl.settlement_line SET status = 'reversed' WHERE tenant_id = $1 AND company_id = $2
		AND statement_id = $3 AND status = 'settled'`, t, c, s.ID); err != nil {
		return err
	}
	evidence := r.EvidenceIDs
	if evidence == nil {
		evidence = []string{}
	}
	kind, id, epoch, at := fact(r.By)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.settlement_reversal (tenant_id, company_id, statement_id, reason, evidence_ids, reversed_by_kind,
		reversed_by_id, reverse_epoch, reversed_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, t, c, s.ID, r.Reason, evidence, kind, id, epoch, at)
	return err
}

// LoadReversal returns the reversal of statement id, or nil.
func LoadReversal(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string) (*settlement.Reversal, error) {
	t, c := ts(sc)
	var r settlement.Reversal
	var kind, by string
	var epoch int64
	var at time.Time
	err := exec.QueryRow(ctx, `SELECT reason, evidence_ids, reversed_by_kind, reversed_by_id, reverse_epoch, reversed_at
		FROM brant_stl.settlement_reversal WHERE tenant_id = $1 AND company_id = $2 AND statement_id = $3`, t, c, id).Scan(&r.Reason, &r.EvidenceIDs,
		&kind, &by, &epoch, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.By = frAct(kind, by, epoch, at, r.Reason)
	return &r, nil
}

// OrderConversions returns the applications to order orderID's stages and receivables that convert a fund
// in another currency (STL-INV-007), standing or not, in the order they were made: the business settlement
// FX differences of the order result (unit C9) are those that stand.
func OrderConversions(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string) ([]fa.Application, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT `+applicationColumns+applicationFrom+` WHERE a.tenant_id = $1 AND a.company_id = $2
		AND a.order_id = $3 AND a.source_currency IS NOT NULL ORDER BY a.applied_at, a.application_id`, t, c, orderID)
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
