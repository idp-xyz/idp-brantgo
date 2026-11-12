// Package postgres stores STL's customer credit in schema brant_stl, on the executor of STL's own
// transaction (migrations/stl/000001_stl_customer_credit.sql).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is STL's schema.
const Schema = "brant_stl"

// ErrConflict means a compare-and-set update found another version.
var ErrConflict = errors.New("stl: version changed")

// Subjects of an approval and of PartyOf.
const (
	SubjectLimitRequest = "limit_request"
	SubjectDecision     = "decision"
	SubjectReservation  = "reservation"
)

func ts(sc kernel.Scope) (string, string) { return string(sc.Tenant), string(sc.Company) }

func currency(code string, units int16) (kernel.Currency, error) {
	c, err := kernel.NewCurrency(code, int(units))
	if err != nil {
		return kernel.Currency{}, fmt.Errorf("stl: stored currency %s/%d: %w", code, units, err)
	}
	return c, nil
}

func money(c kernel.Currency, minor string) (kernel.Money, error) {
	m, err := kernel.ParseMinor(c, minor)
	if err != nil {
		return kernel.Money{}, fmt.Errorf("stl: stored amount %q %s: %w", minor, c, err)
	}
	return m, nil
}

func act(kind, id string, epoch int64, at time.Time, reason string) cc.Act {
	return cc.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(kind), ID: id}, Epoch: epoch, At: kernel.Instant(at), Reason: reason}
}

func optAct(kind, id *string, epoch *int64, at *time.Time, reason *string) *cc.Act {
	if kind == nil || id == nil || epoch == nil || at == nil {
		return nil
	}
	a := act(*kind, *id, *epoch, *at, "")
	if reason != nil {
		a.Reason = *reason
	}
	return &a
}

func optUse(id *string, revision *int64, hash *string) *cc.ConfigUse {
	if id == nil || revision == nil || hash == nil {
		return nil
	}
	return &cc.ConfigUse{ID: *id, Revision: *revision, Hash: *hash}
}

func useArgs(u *cc.ConfigUse) (id *string, revision *int64, hash *string) {
	if u == nil {
		return nil, nil, nil
	}
	return &u.ID, &u.Revision, &u.Hash
}

func actArgs(a *cc.Act) (kind, id *string, epoch *int64, at *time.Time, reason *string) {
	if a == nil {
		return nil, nil, nil, nil, nil
	}
	k := string(a.By.Kind)
	return &k, &a.By.ID, &a.Epoch, &a.At, &a.Reason
}

// EnsureRoot creates the customer's credit root at version 0 when it is missing; the command that
// creates it moves it to 1 before it commits, or rolls it back.
func EnsureRoot(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, partyID string, now time.Time) error {
	t, c := ts(sc)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.customer_credit (tenant_id, company_id, party_id, row_version, limit_version, created_at,
		updated_at) VALUES ($1, $2, $3, 0, 0, $4, $4) ON CONFLICT DO NOTHING`, t, c, partyID, kernel.Instant(now))
	return err
}

// LoadRoot returns the customer's credit root, or nil; forUpdate locks it.
func LoadRoot(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, partyID string, forUpdate bool) (*cc.Root, error) {
	t, c := ts(sc)
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var r cc.Root
	err := exec.QueryRow(ctx, `SELECT party_id, row_version, limit_version, created_at FROM brant_stl.customer_credit
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3`+lock, t, c, partyID).Scan(&r.PartyID, &r.RowVersion, &r.LimitVersion, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = kernel.Instant(r.CreatedAt)
	return &r, nil
}

// SaveRoot moves the root from row version r.RowVersion − 1 to r.RowVersion with its count of limit
// versions.
func SaveRoot(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, r *cc.Root, now time.Time) error {
	t, c := ts(sc)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.customer_credit SET row_version = $4, limit_version = $5, updated_at = $6
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3 AND row_version = $4 - 1`, t, c, r.PartyID, r.RowVersion, r.LimitVersion,
		kernel.Instant(now))
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// PartyOf returns the customer the subject (limit_request, decision or reservation) id belongs to,
// "" when there is none.
func PartyOf(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, subject, id string) (string, error) {
	var q string
	switch subject {
	case SubjectLimitRequest:
		q = `SELECT party_id FROM brant_stl.credit_limit_request WHERE tenant_id = $1 AND company_id = $2 AND request_id = $3`
	case SubjectDecision:
		q = `SELECT party_id FROM brant_stl.credit_decision WHERE tenant_id = $1 AND company_id = $2 AND decision_id = $3`
	case SubjectReservation:
		q = `SELECT party_id FROM brant_stl.credit_reservation WHERE tenant_id = $1 AND company_id = $2 AND reservation_id = $3`
	default:
		return "", fmt.Errorf("stl: unknown subject %q", subject)
	}
	t, c := ts(sc)
	var party string
	err := exec.QueryRow(ctx, q, t, c, id).Scan(&party)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return party, err
}

// loadApprovals returns the approvals of subject id in the order they were given.
func loadApprovals(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, subject, id string, c kernel.Currency) ([]cc.Approval, error) {
	t, co := ts(sc)
	rows, err := exec.Query(ctx, `SELECT approver_kind, approver_id, tier_level, tier_max_minor::text, approval_epoch, approved_at
		FROM brant_stl.credit_approval WHERE tenant_id = $1 AND company_id = $2 AND subject = $3 AND subject_id = $4
		ORDER BY approved_at, approver_id`, t, co, subject, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (cc.Approval, error) {
		var a cc.Approval
		var kind, maxMinor string
		if err := row.Scan(&kind, &a.Approver.ID, &a.Level, &maxMinor, &a.Epoch, &a.At); err != nil {
			return a, err
		}
		a.Approver.Kind, a.At = kernel.ActorKind(kind), kernel.Instant(a.At)
		var err error
		a.Max, err = money(c, maxMinor)
		return a, err
	})
}

// insertApprovals appends the approvals from index from on.
func insertApprovals(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, subject, id string, approvals []cc.Approval, from int) error {
	t, c := ts(sc)
	for _, a := range approvals[from:] {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.credit_approval (tenant_id, company_id, subject, subject_id, approver_id, approver_kind,
			tier_level, tier_max_minor, approval_epoch, approved_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::text::numeric, $9, $10)`,
			t, c, subject, id, a.Approver.ID, string(a.Approver.Kind), a.Level, a.Max.MinorString(), a.Epoch, a.At); err != nil {
			return err
		}
	}
	return nil
}

const requestColumns = `request_id, party_id, status, version, currency, minor_units, currency_revision, amount_minor::text,
	internal_minor::text, internal_evidence, sinosure_minor::text, sinosure_evidence, valid_from, valid_until, policy_id, policy_revision,
	policy_hash, reason, owner_actor_id, coalesce(owner_team_id, ''), requested_by_kind, requested_by_id, request_epoch, requested_at,
	decided_by_kind, decided_by_id, decide_epoch, decided_at, decision_reason, coalesce(limit_version, 0)`

func scanRequest(row pgx.Row) (*cc.LimitRequest, error) {
	var r cc.LimitRequest
	var code, amount, rKind string
	var units int16
	var internal, sinosure *string
	var internalEv, sinosureEv []string
	var dKind, dID, dReason *string
	var dEpoch *int64
	var dAt *time.Time
	err := row.Scan(&r.ID, &r.PartyID, &r.Status, &r.Version, &code, &units, &r.CurrencyRevision, &amount, &internal, &internalEv, &sinosure,
		&sinosureEv, &r.ValidFrom, &r.ValidUntil, &r.Policy.ID, &r.Policy.Revision, &r.Policy.Hash, &r.Reason, &r.OwnerActor, &r.OwnerTeam,
		&rKind, &r.Requested.By.ID, &r.Requested.Epoch, &r.Requested.At, &dKind, &dID, &dEpoch, &dAt, &dReason, &r.LimitVersion)
	if err != nil {
		return nil, err
	}
	if r.Currency, err = currency(code, units); err != nil {
		return nil, err
	}
	if r.Amount, err = money(r.Currency, amount); err != nil {
		return nil, err
	}
	for _, s := range []struct {
		minor *string
		ev    []string
		to    **cc.SourceAmount
	}{{internal, internalEv, &r.Internal}, {sinosure, sinosureEv, &r.Sinosure}} {
		if s.minor == nil {
			continue
		}
		m, err := money(r.Currency, *s.minor)
		if err != nil {
			return nil, err
		}
		*s.to = &cc.SourceAmount{Amount: m, EvidenceIDs: s.ev}
	}
	r.Requested.By.Kind = kernel.ActorKind(rKind)
	r.Requested.At, r.ValidFrom, r.ValidUntil = kernel.Instant(r.Requested.At), kernel.Instant(r.ValidFrom), kernel.Instant(r.ValidUntil)
	r.Requested.Reason = r.Reason
	r.Decided = optAct(dKind, dID, dEpoch, dAt, dReason)
	return &r, nil
}

// LoadRequest returns limit request id with its approvals, or nil.
func LoadRequest(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string) (*cc.LimitRequest, error) {
	t, c := ts(sc)
	r, err := scanRequest(exec.QueryRow(ctx, `SELECT `+requestColumns+` FROM brant_stl.credit_limit_request
		WHERE tenant_id = $1 AND company_id = $2 AND request_id = $3`, t, c, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Approvals, err = loadApprovals(ctx, exec, sc, SubjectLimitRequest, r.ID, r.Currency)
	return r, err
}

// LoadRequests returns the customer's limit requests, oldest first, with their approvals.
func LoadRequests(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, partyID string) ([]cc.LimitRequest, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT `+requestColumns+` FROM brant_stl.credit_limit_request WHERE tenant_id = $1 AND company_id = $2
		AND party_id = $3 ORDER BY requested_at, request_id`, t, c, partyID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (cc.LimitRequest, error) {
		r, err := scanRequest(row)
		if err != nil {
			return cc.LimitRequest{}, err
		}
		return *r, nil
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Approvals, err = loadApprovals(ctx, exec, sc, SubjectLimitRequest, out[i].ID, out[i].Currency); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func sourceArgs(s *cc.SourceAmount) (*string, []string) {
	if s == nil {
		return nil, nil
	}
	m := s.Amount.MinorString()
	return &m, s.EvidenceIDs
}

// InsertRequest stores a new limit request.
func InsertRequest(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, r *cc.LimitRequest) error {
	t, c := ts(sc)
	internal, internalEv := sourceArgs(r.Internal)
	sinosure, sinosureEv := sourceArgs(r.Sinosure)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.credit_limit_request (tenant_id, company_id, request_id, party_id, status, version, currency,
		minor_units, currency_revision, amount_minor, internal_minor, internal_evidence, sinosure_minor, sinosure_evidence, valid_from,
		valid_until, policy_id, policy_revision, policy_hash, reason, owner_actor_id, owner_team_id, requested_by_kind, requested_by_id,
		request_epoch, requested_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::text::numeric, $11::text::numeric, $12,
		$13::text::numeric, $14, $15, $16, $17, $18, $19, $20, $21, nullif($22, ''), $23, $24, $25, $26)`,
		t, c, r.ID, r.PartyID, r.Status, r.Version, r.Currency.Code(), r.Currency.MinorUnits(), r.CurrencyRevision, r.Amount.MinorString(),
		internal, internalEv, sinosure, sinosureEv, r.ValidFrom, r.ValidUntil, r.Policy.ID, r.Policy.Revision, r.Policy.Hash, r.Reason,
		r.OwnerActor, r.OwnerTeam, string(r.Requested.By.Kind), r.Requested.By.ID, r.Requested.Epoch, r.Requested.At)
	return err
}

// UpdateRequest moves request r from version from to r.Version and appends the approvals beyond
// the prior count.
func UpdateRequest(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, r *cc.LimitRequest, from int64, prior int) error {
	t, c := ts(sc)
	kind, id, epoch, at, reason := actArgs(r.Decided)
	var lv *int64
	if r.LimitVersion > 0 {
		lv = &r.LimitVersion
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.credit_limit_request SET status = $4, version = $5, decided_by_kind = $6, decided_by_id = $7,
		decide_epoch = $8, decided_at = $9, decision_reason = $10, limit_version = $11
		WHERE tenant_id = $1 AND company_id = $2 AND request_id = $3 AND version = $12`,
		t, c, r.ID, r.Status, r.Version, kind, id, epoch, at, reason, lv, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	return insertApprovals(ctx, exec, sc, SubjectLimitRequest, r.ID, r.Approvals, prior)
}

// LoadVersions returns the customer's limit versions, oldest first.
func LoadVersions(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, partyID string) ([]cc.LimitVersion, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT party_id, version, request_id, currency, minor_units, amount_minor::text, source, valid_from,
		valid_until, policy_id, policy_revision, policy_hash, published_by_id, publish_epoch, published_at FROM brant_stl.credit_limit_version
		WHERE tenant_id = $1 AND company_id = $2 AND party_id = $3 ORDER BY version`, t, c, partyID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (cc.LimitVersion, error) {
		var v cc.LimitVersion
		var code, amount string
		var units int16
		if err := row.Scan(&v.PartyID, &v.Version, &v.RequestID, &code, &units, &amount, &v.Source, &v.ValidFrom, &v.ValidUntil, &v.Policy.ID,
			&v.Policy.Revision, &v.Policy.Hash, &v.Published.By.ID, &v.Published.Epoch, &v.Published.At); err != nil {
			return v, err
		}
		v.Published.By.Kind = kernel.ActorHuman
		v.ValidFrom, v.ValidUntil, v.Published.At = kernel.Instant(v.ValidFrom), kernel.Instant(v.ValidUntil), kernel.Instant(v.Published.At)
		var err error
		if v.Currency, err = currency(code, units); err != nil {
			return v, err
		}
		v.Amount, err = money(v.Currency, amount)
		return v, err
	})
}

// InsertVersion stores a published limit version.
func InsertVersion(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, v *cc.LimitVersion) error {
	t, c := ts(sc)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.credit_limit_version (tenant_id, company_id, party_id, version, request_id, currency,
		minor_units, amount_minor, source, valid_from, valid_until, policy_id, policy_revision, policy_hash, published_by_id, publish_epoch,
		published_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::text::numeric, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		t, c, v.PartyID, v.Version, v.RequestID, v.Currency.Code(), v.Currency.MinorUnits(), v.Amount.MinorString(), v.Source, v.ValidFrom,
		v.ValidUntil, v.Policy.ID, v.Policy.Revision, v.Policy.Hash, v.Published.By.ID, v.Published.Epoch, v.Published.At)
	return err
}

const decisionColumns = `d.decision_id, d.party_id, d.order_id, d.order_revision, d.order_hash, d.currency, d.minor_units,
	d.portion_minor::text, d.limit_currency, d.limit_minor_units, d.converted_minor::text, d.fx_id, d.fx_revision, d.fx_hash,
	d.limit_minor::text, d.limit_version, d.exposure_minor::text, d.excess_minor::text, d.cover_minor::text, coalesce(d.overdue_days, -1),
	d.reasons, d.policy_id, d.policy_revision, d.policy_hash, d.status, d.version, d.expires_at, d.owner_actor_id,
	coalesce(d.owner_team_id, ''), d.opened_by_kind, d.opened_by_id, d.open_epoch, d.opened_at, d.decided_by_kind, d.decided_by_id,
	d.decide_epoch, d.decided_at, d.decision_reason,
	coalesce((SELECT r.reservation_id FROM brant_stl.credit_reservation r WHERE r.tenant_id = d.tenant_id AND r.company_id = d.company_id
		AND r.decision_id = d.decision_id), '')`

func scanDecision(row pgx.Row) (*cc.Decision, error) {
	var d cc.Decision
	var code, limitCode, oKind string
	var units, limitUnits int16
	var portion, converted, limit, exposure, excess, cover string
	var fxID, fxHash *string
	var fxRev *int64
	var dKind, dID, dReason *string
	var dEpoch *int64
	var dAt *time.Time
	err := row.Scan(&d.ID, &d.PartyID, &d.Binding.OrderID, &d.Binding.OrderRevision, &d.Binding.OrderHash, &code, &units, &portion, &limitCode,
		&limitUnits, &converted, &fxID, &fxRev, &fxHash, &limit, &d.LimitVersion, &exposure, &excess, &cover, &d.OverdueDays, &d.Reasons,
		&d.Policy.ID, &d.Policy.Revision, &d.Policy.Hash, &d.Status, &d.Version, &d.ExpiresAt, &d.OwnerActor, &d.OwnerTeam, &oKind,
		&d.Opened.By.ID, &d.Opened.Epoch, &d.Opened.At, &dKind, &dID, &dEpoch, &dAt, &dReason, &d.ReservationID)
	if err != nil {
		return nil, err
	}
	oc, err := currency(code, units)
	if err != nil {
		return nil, err
	}
	lc, err := currency(limitCode, limitUnits)
	if err != nil {
		return nil, err
	}
	for _, m := range []struct {
		c  kernel.Currency
		s  string
		to *kernel.Money
	}{{oc, portion, &d.Portion}, {lc, converted, &d.Converted}, {lc, limit, &d.Limit}, {lc, exposure, &d.Exposure}, {lc, excess, &d.Excess},
		{lc, cover, &d.Cover}} {
		if *m.to, err = money(m.c, m.s); err != nil {
			return nil, err
		}
	}
	d.FX = optUse(fxID, fxRev, fxHash)
	d.Opened.By.Kind = kernel.ActorKind(oKind)
	d.Opened.At, d.ExpiresAt = kernel.Instant(d.Opened.At), kernel.Instant(d.ExpiresAt)
	d.Decided = optAct(dKind, dID, dEpoch, dAt, dReason)
	return &d, nil
}

func loadDecisions(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, where string, args ...any) ([]cc.Decision, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT `+decisionColumns+` FROM brant_stl.credit_decision d WHERE d.tenant_id = $1 AND d.company_id = $2
		AND `+where+` ORDER BY d.opened_at, d.decision_id`, append([]any{t, c}, args...)...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (cc.Decision, error) {
		d, err := scanDecision(row)
		if err != nil {
			return cc.Decision{}, err
		}
		return *d, nil
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Approvals, err = loadApprovals(ctx, exec, sc, SubjectDecision, out[i].ID, out[i].Limit.Currency()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LoadDecision returns decision id with its approvals, or nil.
func LoadDecision(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string) (*cc.Decision, error) {
	out, err := loadDecisions(ctx, exec, sc, `d.decision_id = $3`, id)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// LoadDecisions returns the customer's decisions, oldest first.
func LoadDecisions(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, partyID string) ([]cc.Decision, error) {
	return loadDecisions(ctx, exec, sc, `d.party_id = $3`, partyID)
}

// LoadOrderDecisions returns the decisions of an order revision, oldest first.
func LoadOrderDecisions(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, revision int64) ([]cc.Decision, error) {
	return loadDecisions(ctx, exec, sc, `d.order_id = $3 AND d.order_revision = $4`, orderID, revision)
}

// InsertDecision stores a new decision.
func InsertDecision(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, d *cc.Decision) error {
	t, c := ts(sc)
	fxID, fxRev, fxHash := useArgs(d.FX)
	var overdue *int
	if d.OverdueDays >= 0 {
		overdue = &d.OverdueDays
	}
	oc, lc := d.Portion.Currency(), d.Limit.Currency()
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.credit_decision (tenant_id, company_id, decision_id, party_id, order_id, order_revision,
		order_hash, currency, minor_units, portion_minor, limit_currency, limit_minor_units, converted_minor, fx_id, fx_revision, fx_hash,
		limit_minor, limit_version, exposure_minor, excess_minor, cover_minor, overdue_days, reasons, policy_id, policy_revision, policy_hash,
		status, version, expires_at, owner_actor_id, owner_team_id, opened_by_kind, opened_by_id, open_epoch, opened_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::text::numeric, $11, $12, $13::text::numeric, $14, $15, $16, $17::text::numeric, $18,
		$19::text::numeric, $20::text::numeric, $21::text::numeric, $22, $23, $24, $25, $26, $27, $28, $29, $30, nullif($31, ''), $32, $33,
		$34, $35)`,
		t, c, d.ID, d.PartyID, d.Binding.OrderID, d.Binding.OrderRevision, d.Binding.OrderHash, oc.Code(), oc.MinorUnits(), d.Portion.MinorString(),
		lc.Code(), lc.MinorUnits(), d.Converted.MinorString(), fxID, fxRev, fxHash, d.Limit.MinorString(), d.LimitVersion, d.Exposure.MinorString(),
		d.Excess.MinorString(), d.Cover.MinorString(), overdue, d.Reasons, d.Policy.ID, d.Policy.Revision, d.Policy.Hash, d.Status, d.Version,
		d.ExpiresAt, d.OwnerActor, d.OwnerTeam, string(d.Opened.By.Kind), d.Opened.By.ID, d.Opened.Epoch, d.Opened.At)
	return err
}

// UpdateDecision moves decision d from version from to d.Version and appends the approvals beyond
// the prior count.
func UpdateDecision(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, d *cc.Decision, from int64, prior int) error {
	t, c := ts(sc)
	kind, id, epoch, at, reason := actArgs(d.Decided)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.credit_decision SET status = $4, version = $5, decided_by_kind = $6, decided_by_id = $7,
		decide_epoch = $8, decided_at = $9, decision_reason = $10 WHERE tenant_id = $1 AND company_id = $2 AND decision_id = $3 AND version = $11`,
		t, c, d.ID, d.Status, d.Version, kind, id, epoch, at, reason, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	return insertApprovals(ctx, exec, sc, SubjectDecision, d.ID, d.Approvals, prior)
}

const reservationColumns = `reservation_id, party_id, order_id, order_revision, order_hash, currency, minor_units, amount_minor::text,
	secured_minor::text, limit_currency, limit_minor_units, exposure_minor::text, fx_id, fx_revision, fx_hash, basis, coalesce(decision_id, ''),
	policy_id, policy_revision, policy_hash, status, version, expires_at, owner_actor_id, coalesce(owner_team_id, ''), reserved_by_kind,
	reserved_by_id, reserve_epoch, reserved_at, consumed_by_kind, consumed_by_id, consume_epoch, consumed_at, activated_at, released_by_kind,
	released_by_id, release_epoch, released_at, release_reason`

func scanReservation(row pgx.Row) (*cc.Reservation, error) {
	var r cc.Reservation
	var code, limitCode, amount, secured, exposure, rKind string
	var units, limitUnits int16
	var fxID, fxHash *string
	var fxRev *int64
	var cKind, cID, lKind, lID, lReason *string
	var cEpoch, lEpoch *int64
	var cAt, activated, lAt *time.Time
	err := row.Scan(&r.ID, &r.PartyID, &r.Binding.OrderID, &r.Binding.OrderRevision, &r.Binding.OrderHash, &code, &units, &amount, &secured,
		&limitCode, &limitUnits, &exposure, &fxID, &fxRev, &fxHash, &r.Basis, &r.DecisionID, &r.Policy.ID, &r.Policy.Revision, &r.Policy.Hash,
		&r.Status, &r.Version, &r.ExpiresAt, &r.OwnerActor, &r.OwnerTeam, &rKind, &r.Reserved.By.ID, &r.Reserved.Epoch, &r.Reserved.At,
		&cKind, &cID, &cEpoch, &cAt, &activated, &lKind, &lID, &lEpoch, &lAt, &lReason)
	if err != nil {
		return nil, err
	}
	oc, err := currency(code, units)
	if err != nil {
		return nil, err
	}
	lc, err := currency(limitCode, limitUnits)
	if err != nil {
		return nil, err
	}
	if r.Amount, err = money(oc, amount); err != nil {
		return nil, err
	}
	if r.Secured, err = money(oc, secured); err != nil {
		return nil, err
	}
	if r.Exposure, err = money(lc, exposure); err != nil {
		return nil, err
	}
	r.FX = optUse(fxID, fxRev, fxHash)
	r.Reserved.By.Kind = kernel.ActorKind(rKind)
	r.Reserved.At, r.ExpiresAt = kernel.Instant(r.Reserved.At), kernel.Instant(r.ExpiresAt)
	r.Consumed = optAct(cKind, cID, cEpoch, cAt, nil)
	if activated != nil {
		r.ActivatedAt = kernel.Instant(*activated)
	}
	r.Released = optAct(lKind, lID, lEpoch, lAt, lReason)
	return &r, nil
}

// LoadReservation returns reservation id, or nil.
func LoadReservation(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, id string) (*cc.Reservation, error) {
	t, c := ts(sc)
	r, err := scanReservation(exec.QueryRow(ctx, `SELECT `+reservationColumns+` FROM brant_stl.credit_reservation
		WHERE tenant_id = $1 AND company_id = $2 AND reservation_id = $3`, t, c, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// LoadReservations returns the customer's reservations, oldest first.
func LoadReservations(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, partyID string) ([]cc.Reservation, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT `+reservationColumns+` FROM brant_stl.credit_reservation WHERE tenant_id = $1 AND company_id = $2
		AND party_id = $3 ORDER BY reserved_at, reservation_id`, t, c, partyID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (cc.Reservation, error) {
		r, err := scanReservation(row)
		if err != nil {
			return cc.Reservation{}, err
		}
		return *r, nil
	})
}

// InsertReservation stores a new reservation.
func InsertReservation(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, r *cc.Reservation) error {
	t, c := ts(sc)
	fxID, fxRev, fxHash := useArgs(r.FX)
	var decision *string
	if r.DecisionID != "" {
		decision = &r.DecisionID
	}
	oc, lc := r.Amount.Currency(), r.Exposure.Currency()
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.credit_reservation (tenant_id, company_id, reservation_id, party_id, order_id, order_revision,
		order_hash, currency, minor_units, amount_minor, secured_minor, limit_currency, limit_minor_units, exposure_minor, fx_id, fx_revision,
		fx_hash, basis, decision_id, policy_id, policy_revision, policy_hash, status, version, expires_at, owner_actor_id, owner_team_id,
		reserved_by_kind, reserved_by_id, reserve_epoch, reserved_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::text::numeric,
		$11::text::numeric, $12, $13, $14::text::numeric, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, nullif($27, ''), $28, $29,
		$30, $31)`,
		t, c, r.ID, r.PartyID, r.Binding.OrderID, r.Binding.OrderRevision, r.Binding.OrderHash, oc.Code(), oc.MinorUnits(), r.Amount.MinorString(),
		r.Secured.MinorString(), lc.Code(), lc.MinorUnits(), r.Exposure.MinorString(), fxID, fxRev, fxHash, r.Basis, decision, r.Policy.ID,
		r.Policy.Revision, r.Policy.Hash, r.Status, r.Version, r.ExpiresAt, r.OwnerActor, r.OwnerTeam, string(r.Reserved.By.Kind), r.Reserved.By.ID,
		r.Reserved.Epoch, r.Reserved.At)
	return err
}

// UpdateReservation moves reservation r from version from to r.Version.
func UpdateReservation(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, r *cc.Reservation, from int64) error {
	t, c := ts(sc)
	cKind, cID, cEpoch, cAt, _ := actArgs(r.Consumed)
	lKind, lID, lEpoch, lAt, lReason := actArgs(r.Released)
	var activated *time.Time
	if r.Consumed != nil {
		activated = &r.ActivatedAt
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.credit_reservation SET status = $4, version = $5, consumed_by_kind = $6, consumed_by_id = $7,
		consume_epoch = $8, consumed_at = $9, activated_at = $10, released_by_kind = $11, released_by_id = $12, release_epoch = $13,
		released_at = $14, release_reason = $15 WHERE tenant_id = $1 AND company_id = $2 AND reservation_id = $3 AND version = $16`,
		t, c, r.ID, r.Status, r.Version, cKind, cID, cEpoch, cAt, activated, lKind, lID, lEpoch, lAt, lReason, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// OpenReceivables returns what the customer owes on business receivables in the limit currency c
// (D-4). STL keeps no receivables before settlement statements (B2b) and opening balances (C10), so
// it answers none; those units replace it with the sum over their own tables, read here under the
// credit root's lock.
func OpenReceivables(_ context.Context, _ bentopg.Executor, _ kernel.Scope, _ string, c kernel.Currency) (cc.Receivables, error) {
	zero, err := kernel.NewMoney(c, 0)
	return cc.Receivables{Open: zero}, err
}
