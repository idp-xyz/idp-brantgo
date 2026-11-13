package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	oa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Payment plan reviews and the order account's moves (migrations/stl/000003_stl_payment_plan_review.sql).

// MoveAccount writes account a, moved by its payment plan review to a later revision at its next
// version, with that revision's stages; ErrConflict when the stored account is not at the version
// before.
func MoveAccount(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, a *oa.Account, at time.Time) error {
	t, c := ts(sc)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.order_account SET order_revision = $4, order_hash = $5, currency_revision = $6, version = $7,
		updated_at = $8 WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 AND version = $7 - 1`, t, c, a.OrderID, a.OrderRevision,
		a.OrderHash, a.CurrencyRevision, a.Version, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
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

// BumpAccount writes account a at its next version at the same revision (a review settled).
func BumpAccount(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, a *oa.Account, at time.Time) error {
	t, c := ts(sc)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.order_account SET version = $4, updated_at = $5 WHERE tenant_id = $1 AND company_id = $2
		AND order_id = $3 AND version = $4 - 1`, t, c, a.OrderID, a.Version, at)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// InsertReview writes a new open review with its items.
func InsertReview(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, r *oa.Review) error {
	t, c := ts(sc)
	kind, id, epoch, at := fact(r.Opened)
	if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.payment_plan_review (tenant_id, company_id, order_id, order_revision, order_hash, from_revision,
		from_hash, responsible_role, status, version, opened_by_kind, opened_by_id, open_epoch, opened_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)`, t, c, r.OrderID, r.OrderRevision, r.OrderHash, r.FromRevision,
		r.FromHash, r.ResponsibleRole, r.Status, r.Version, kind, id, epoch, at); err != nil {
		return err
	}
	for _, it := range r.Items {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.payment_plan_review_item (tenant_id, company_id, order_id, order_revision, seq, kind,
			stage_id, from_minor, to_minor, applied_minor) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::text::numeric, $9::text::numeric, $10::text::numeric)`,
			t, c, r.OrderID, r.OrderRevision, it.Seq, it.Kind, nullable(it.StageID), it.From.MinorString(), it.To.MinorString(),
			it.Applied.MinorString()); err != nil {
			return err
		}
	}
	return nil
}

// SupersedeReview writes review r superseded.
func SupersedeReview(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, r *oa.Review) error {
	t, c := ts(sc)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.payment_plan_review SET status = $5, version = $6, superseded_by_revision = $7, superseded_at = $8,
		updated_at = $8 WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 AND order_revision = $4 AND version = $6 - 1`, t, c, r.OrderID,
		r.OrderRevision, r.Status, r.Version, r.SupersededBy, r.SupersededAt)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// SettleReview writes review r settled with the disposition of each item.
func SettleReview(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, r *oa.Review) error {
	t, c := ts(sc)
	kind, id, epoch, at := fact(*r.Settled)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.payment_plan_review SET status = $5, version = $6, settled_by_kind = $7, settled_by_id = $8,
		settle_epoch = $9, settled_at = $10, settle_reason = $11, updated_at = $10 WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3
		AND order_revision = $4 AND version = $6 - 1`, t, c, r.OrderID, r.OrderRevision, r.Status, r.Version, kind, id, epoch, at, r.Settled.Reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	for _, it := range r.Items {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.payment_plan_review_disposition (tenant_id, company_id, order_id, order_revision, seq,
			disposition, note) VALUES ($1, $2, $3, $4, $5, $6, $7)`, t, c, r.OrderID, r.OrderRevision, it.Seq, it.Disposition, it.Note); err != nil {
			return err
		}
	}
	return nil
}

const reviewColumns = `order_id, order_revision, order_hash, from_revision, from_hash, responsible_role, status, version, opened_by_kind,
	opened_by_id, open_epoch, opened_at, settled_by_kind, settled_by_id, settle_epoch, settled_at, settle_reason, coalesce(superseded_by_revision, 0),
	superseded_at`

func scanReview(row pgx.Row) (*oa.Review, error) {
	var r oa.Review
	var kind, by string
	var epoch int64
	var at time.Time
	var sKind, sBy, sReason *string
	var sEpoch *int64
	var sAt, supAt *time.Time
	if err := row.Scan(&r.OrderID, &r.OrderRevision, &r.OrderHash, &r.FromRevision, &r.FromHash, &r.ResponsibleRole, &r.Status, &r.Version, &kind, &by,
		&epoch, &at, &sKind, &sBy, &sEpoch, &sAt, &sReason, &r.SupersededBy, &supAt); err != nil {
		return nil, err
	}
	r.Opened = frAct(kind, by, epoch, at, "")
	if sBy != nil {
		s := frAct(*sKind, *sBy, *sEpoch, *sAt, *sReason)
		r.Settled = &s
	}
	if supAt != nil {
		r.SupersededAt = kernel.Instant(*supAt)
	}
	return &r, nil
}

// loadItems completes r with its items and their dispositions, amounts in cur.
func loadItems(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, r *oa.Review, cur kernel.Currency) error {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT i.seq, i.kind, coalesce(i.stage_id, ''), i.from_minor::text, i.to_minor::text, i.applied_minor::text,
		coalesce(d.disposition, ''), coalesce(d.note, '') FROM brant_stl.payment_plan_review_item i
		LEFT JOIN brant_stl.payment_plan_review_disposition d ON d.tenant_id = i.tenant_id AND d.company_id = i.company_id AND d.order_id = i.order_id
			AND d.order_revision = i.order_revision AND d.seq = i.seq
		WHERE i.tenant_id = $1 AND i.company_id = $2 AND i.order_id = $3 AND i.order_revision = $4 ORDER BY i.seq`, t, c, r.OrderID, r.OrderRevision)
	if err != nil {
		return err
	}
	r.Items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (oa.Item, error) {
		var it oa.Item
		var from, to, applied string
		if err := row.Scan(&it.Seq, &it.Kind, &it.StageID, &from, &to, &applied, &it.Disposition, &it.Note); err != nil {
			return it, err
		}
		var err error
		if it.From, err = money(cur, from); err != nil {
			return it, err
		}
		if it.To, err = money(cur, to); err != nil {
			return it, err
		}
		it.Applied, err = money(cur, applied)
		return it, err
	})
	if r.Items == nil {
		r.Items = []oa.Item{}
	}
	return err
}

// LoadReview returns the review of orderID's revision with its items, or nil; amounts in cur.
func LoadReview(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, revision int64, cur kernel.Currency) (*oa.Review, error) {
	t, c := ts(sc)
	r, err := scanReview(exec.QueryRow(ctx, `SELECT `+reviewColumns+` FROM brant_stl.payment_plan_review WHERE tenant_id = $1 AND company_id = $2
		AND order_id = $3 AND order_revision = $4`, t, c, orderID, revision))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, loadItems(ctx, exec, sc, r, cur)
}

// LoadOpenReview returns orderID's open review with its items, or nil; amounts in cur.
func LoadOpenReview(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, cur kernel.Currency) (*oa.Review, error) {
	t, c := ts(sc)
	var revision int64
	err := exec.QueryRow(ctx, `SELECT order_revision FROM brant_stl.payment_plan_review WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3
		AND status = 'open'`, t, c, orderID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return LoadReview(ctx, exec, sc, orderID, revision, cur)
}

// LoadReviews returns every review of orderID with its items, latest revision first; amounts in cur.
func LoadReviews(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, orderID string, cur kernel.Currency) ([]oa.Review, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT `+reviewColumns+` FROM brant_stl.payment_plan_review WHERE tenant_id = $1 AND company_id = $2
		AND order_id = $3 ORDER BY order_revision DESC`, t, c, orderID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (oa.Review, error) {
		r, err := scanReview(row)
		if err != nil {
			return oa.Review{}, err
		}
		return *r, nil
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err := loadItems(ctx, exec, sc, &out[i], cur); err != nil {
			return nil, err
		}
	}
	return out, nil
}
