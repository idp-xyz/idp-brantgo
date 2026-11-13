package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// LockOrderAllocations locks the DemandAllocation roots of every line of order orderID FOR UPDATE, in
// order_line_id order, and returns them with their entries: the lock an allocation review takes
// first, as every draft, void, commitment issue, change and cancellation of the order's lines does.
func LockOrderAllocations(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string) ([]*allocation.Allocation, error) {
	rows, err := exec.Query(ctx, `SELECT order_line_id FROM brant_pur.demand_allocation WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3
		ORDER BY order_line_id FOR UPDATE`, string(scope.Tenant), string(scope.Company), orderID)
	if err != nil {
		return nil, err
	}
	if _, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return nil, err
	}
	return AllocationsOfOrder(ctx, exec, scope, orderID)
}

// OrderHoldings returns what PUR holds of order orderID besides its allocation roots: every draft
// with its lines as its document bought them and its latest commitment, and every commitment that
// was issued with what it buys of each line at its revision in force and what was verified
// delivered of it. It locks nothing.
func OrderHoldings(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string) ([]allocation.HeldDraft, []allocation.HeldCommitment, error) {
	rows, err := exec.Query(ctx, `SELECT draft_id FROM brant_pur.purchase_draft WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3
		ORDER BY draft_id`, string(scope.Tenant), string(scope.Company), orderID)
	if err != nil {
		return nil, nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, nil, err
	}
	owners, err := CommitmentsOfOrder(ctx, exec, scope, orderID)
	if err != nil {
		return nil, nil, err
	}
	var drafts []allocation.HeldDraft
	for _, id := range ids {
		d, err := LoadDraft(ctx, exec, scope, id, NoLock)
		if err != nil {
			return nil, nil, err
		}
		doc, err := d.Decode()
		if err != nil {
			return nil, nil, err
		}
		h := allocation.HeldDraft{ID: d.ID, Hash: d.Hash, Status: d.Status, SupplierID: d.SupplierID}
		for _, o := range owners {
			// A draft has one live commitment at a time; rejected and revoked ones are history.
			if o.DraftID == d.ID && o.Status != commitment.StatusRejected && o.Status != commitment.StatusRevoked {
				h.CommitmentID, h.CommitmentStatus = o.ID, o.Status
				h.Committed = h.Committed || commitment.IsCommitted(o.Status)
			}
		}
		for _, l := range doc.Lines {
			q, err := quantity(l.AllocatedQuantity)
			if err != nil {
				return nil, nil, err
			}
			o, err := quantity(l.OverbuyQuantity)
			if err != nil {
				return nil, nil, err
			}
			h.Lines = append(h.Lines, allocation.DraftLine{LineID: l.OrderLineID, Quantity: q, Overbuy: o,
				Spec: allocation.Spec{SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision, UnitID: l.UnitRef.ID}})
		}
		drafts = append(drafts, h)
	}
	var commitments []allocation.HeldCommitment
	for _, o := range owners {
		if !commitment.IsCommitted(o.Status) {
			continue
		}
		c := allocation.HeldCommitment{ID: o.ID, DraftID: o.DraftID, Status: o.Status}
		var number *string
		if err = exec.QueryRow(ctx, `SELECT revision, supplier_id, po_number FROM brant_pur.purchase_commitment WHERE tenant_id = $1
			AND company_id = $2 AND commitment_id = $3`, string(scope.Tenant), string(scope.Company), o.ID).Scan(&c.Revision, &c.SupplierID, &number); err != nil {
			return nil, nil, err
		}
		if number != nil {
			c.PONumber = *number
		}
		lines, err := exec.Query(ctx, `SELECT q.order_line_id, q.allocated::text, q.overbuy::text, q.purchased::text,
			brant_pur.commitment_line_verified($1, $2, $3, q.order_line_id)::text FROM brant_pur.commitment_lines($1, $2, $3) q
			ORDER BY q.order_line_id`, string(scope.Tenant), string(scope.Company), o.ID)
		if err != nil {
			return nil, nil, err
		}
		c.Lines, err = pgx.CollectRows(lines, func(r pgx.CollectableRow) (allocation.CommitmentLine, error) {
			var l allocation.CommitmentLine
			var a, ob, p, v string
			if err := r.Scan(&l.LineID, &a, &ob, &p, &v); err != nil {
				return l, err
			}
			var err error
			for _, x := range []struct {
				s string
				d *kernel.Decimal
			}{{a, &l.Allocated}, {ob, &l.Overbuy}, {p, &l.Purchased}, {v, &l.Verified}} {
				if *x.d, err = quantity(x.s); err != nil {
					return l, err
				}
			}
			return l, nil
		})
		if err != nil {
			return nil, nil, err
		}
		c.Fulfilled = commitment.IsOpen(c.Status) && len(c.Lines) > 0
		for _, l := range c.Lines {
			c.Fulfilled = c.Fulfilled && l.Verified.Cmp(l.Purchased) >= 0
		}
		commitments = append(commitments, c)
	}
	return drafts, commitments, nil
}

const reviewColumns = `review_id, order_id, order_revision, order_hash, order_version, cause, event_id, status, row_version, responsible_role,
	item_count, opened_by_kind, opened_by_id, open_epoch, opened_at, settled_by_kind, settled_by_id, settle_epoch, settled_at, settle_reason,
	coalesce(superseded_by, ''), superseded_at`

func scanReview(r row) (*allocation.Review, int, error) {
	v := &allocation.Review{}
	var count int
	var openKind string
	var sKind, sID, sReason *string
	var sEpoch *int64
	var sAt, supAt *time.Time
	if err := r.Scan(&v.ID, &v.OrderID, &v.Revision, &v.Hash, &v.Version, &v.Cause, &v.EventID, &v.Status, &v.RowVersion, &v.Role, &count,
		&openKind, &v.Opened.By.ID, &v.Opened.Epoch, &v.Opened.At, &sKind, &sID, &sEpoch, &sAt, &sReason, &v.SupersededBy, &supAt); err != nil {
		return nil, 0, err
	}
	v.Opened.By.Kind, v.Opened.At, v.Stored = kernel.ActorKind(openKind), kernel.Instant(v.Opened.At), v.RowVersion
	if sKind != nil && sID != nil && sEpoch != nil && sAt != nil {
		v.Settled = &allocation.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*sKind), ID: *sID}, Epoch: *sEpoch, At: kernel.Instant(*sAt)}
		if sReason != nil {
			v.Settled.Reason = *sReason
		}
	}
	if supAt != nil {
		v.SupersededAt = kernel.Instant(*supAt)
	}
	return v, count, nil
}

// LoadReview returns review id with its items, or nil; lock locks the review row.
func LoadReview(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*allocation.Review, error) {
	r, count, err := scanReview(exec.QueryRow(ctx, `SELECT `+reviewColumns+` FROM brant_pur.allocation_review WHERE tenant_id = $1
		AND company_id = $2 AND review_id = $3`+string(lock), string(scope.Tenant), string(scope.Company), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = reviewItems(ctx, exec, scope, []*allocation.Review{r}); err != nil {
		return nil, err
	}
	if len(r.Items) != count {
		r.Items = append(r.Items, allocation.Item{}) // not intact: Verify refuses it
	}
	return r, nil
}

// ReviewsOfOrder returns the reviews of order orderID with their items, by order version; lock locks
// the review rows in that order.
func ReviewsOfOrder(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string, lock Lock) ([]*allocation.Review, error) {
	rows, err := exec.Query(ctx, `SELECT `+reviewColumns+` FROM brant_pur.allocation_review WHERE tenant_id = $1 AND company_id = $2
		AND order_id = $3 ORDER BY order_version, review_id`+string(lock), string(scope.Tenant), string(scope.Company), orderID)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*allocation.Review, error) {
		v, n, err := scanReview(r)
		if err == nil {
			counts[v.ID] = n
		}
		return v, err
	})
	if err != nil {
		return nil, err
	}
	if err = reviewItems(ctx, exec, scope, out); err != nil {
		return nil, err
	}
	for _, r := range out {
		if len(r.Items) != counts[r.ID] {
			r.Items = append(r.Items, allocation.Item{})
		}
	}
	return out, nil
}

// ReviewOfEvent returns the ID of the review TRD's event eventID opened, or "".
func ReviewOfEvent(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, eventID string) (string, error) {
	var id string
	err := exec.QueryRow(ctx, `SELECT review_id FROM brant_pur.allocation_review WHERE tenant_id = $1 AND company_id = $2 AND event_id = $3`,
		string(scope.Tenant), string(scope.Company), eventID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func reviewItems(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, rs []*allocation.Review) error {
	if len(rs) == 0 {
		return nil
	}
	byID := map[string]*allocation.Review{}
	ids := make([]string, len(rs))
	for i, r := range rs {
		byID[r.ID], ids[i] = r, r.ID
	}
	rows, err := exec.Query(ctx, `SELECT review_id, item_no, facts, disposition, coalesce(disposition_draft_id, ''),
		coalesce(disposition_change_id, ''), coalesce(disposition_commitment_id, ''), coalesce(disposition_revision, 0), disposition_at,
		coalesce(disposition_reason, '') FROM brant_pur.allocation_review_item WHERE tenant_id = $1 AND company_id = $2 AND review_id = ANY($3)
		ORDER BY review_id, item_no`, string(scope.Tenant), string(scope.Company), ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, facts string
		var it allocation.Item
		var kind *string
		var d allocation.Disposition
		var at *time.Time
		if err = rows.Scan(&id, &it.No, &facts, &kind, &d.DraftID, &d.ChangeID, &d.CommitmentID, &d.Revision, &at, &d.Reason); err != nil {
			return err
		}
		if it.Facts, err = allocation.ParseFacts(facts); err != nil {
			return err
		}
		if kind != nil {
			d.Kind = *kind
			if at != nil {
				d.At = kernel.Instant(*at)
			}
			it.Done = &d
		}
		if r := byID[id]; r != nil {
			r.Items = append(r.Items, it)
		}
	}
	return rows.Err()
}

// InsertReview stores a new open review with its items.
func InsertReview(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *allocation.Review) error {
	if _, err := exec.Exec(ctx, `INSERT INTO brant_pur.allocation_review (tenant_id, company_id, review_id, order_id, order_revision, order_hash,
		order_version, cause, event_id, status, row_version, responsible_role, item_count, opened_by_kind, opened_by_id, open_epoch, opened_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		string(scope.Tenant), string(scope.Company), r.ID, r.OrderID, r.Revision, r.Hash, r.Version, r.Cause, r.EventID, r.Status, r.RowVersion,
		r.Role, len(r.Items), string(r.Opened.By.Kind), r.Opened.By.ID, r.Opened.Epoch, r.Opened.At); err != nil {
		return err
	}
	for _, it := range r.Items {
		f := it.Facts
		var rev *int64
		if f.Kind == allocation.ItemCommitment {
			rev = &f.CommitmentRevision
		}
		var draftID *string
		if f.Kind != allocation.ItemAllocation {
			draftID = &f.DraftID
		}
		var commitmentID *string
		if f.Kind == allocation.ItemCommitment {
			commitmentID = &f.CommitmentID
		}
		if _, err := exec.Exec(ctx, `INSERT INTO brant_pur.allocation_review_item (tenant_id, company_id, review_id, item_no, kind, order_line_id,
			draft_id, commitment_id, commitment_revision, facts) VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7, $8, $9, $10)`,
			string(scope.Tenant), string(scope.Company), r.ID, it.No, f.Kind, f.OrderLineID, draftID, commitmentID, rev, f.FactsJSON()); err != nil {
			return err
		}
	}
	return nil
}

// SupersedeReview writes r's supersession if the stored row is still at the version it was loaded at.
func SupersedeReview(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *allocation.Review) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_pur.allocation_review SET status = $4, row_version = $5, superseded_by = $6, superseded_at = $7
		WHERE tenant_id = $1 AND company_id = $2 AND review_id = $3 AND row_version = $8`,
		string(scope.Tenant), string(scope.Company), r.ID, r.Status, r.RowVersion, r.SupersededBy, r.SupersededAt, r.Stored)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// SettleReview writes the dispositions of r's items, then r's settlement, if the stored row is still
// at the version it was loaded at.
func SettleReview(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *allocation.Review) error {
	for _, it := range r.Items {
		d := it.Done
		var at *time.Time
		if !d.At.IsZero() {
			at = &d.At
		}
		var rev *int64
		if d.Revision > 0 {
			rev = &d.Revision
		}
		tag, err := exec.Exec(ctx, `UPDATE brant_pur.allocation_review_item SET disposition = $5, disposition_draft_id = nullif($6, ''),
			disposition_change_id = nullif($7, ''), disposition_commitment_id = nullif($8, ''), disposition_revision = $9, disposition_at = $10,
			disposition_reason = $11 WHERE tenant_id = $1 AND company_id = $2 AND review_id = $3 AND item_no = $4 AND disposition IS NULL`,
			string(scope.Tenant), string(scope.Company), r.ID, it.No, d.Kind, d.DraftID, d.ChangeID, d.CommitmentID, rev, at, d.Reason)
		if err == nil && tag.RowsAffected() != 1 {
			err = ErrConflict
		}
		if err != nil {
			return err
		}
	}
	s := r.Settled
	tag, err := exec.Exec(ctx, `UPDATE brant_pur.allocation_review SET status = $4, row_version = $5, settled_by_kind = $6, settled_by_id = $7,
		settle_epoch = $8, settled_at = $9, settle_reason = nullif($10, '') WHERE tenant_id = $1 AND company_id = $2 AND review_id = $3
		AND row_version = $11`,
		string(scope.Tenant), string(scope.Company), r.ID, r.Status, r.RowVersion, string(s.By.Kind), s.By.ID, s.Epoch, s.At, s.Reason, r.Stored)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// ReviewNow returns what PUR shows now of the drafts, commitments and changes a settlement names
// and the effective allocation of each line of order orderID. It locks nothing.
func ReviewNow(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string, drafts, commitments, changes []string) (allocation.Now, error) {
	now := allocation.Now{Drafts: map[string]allocation.DraftNow{}, Commitments: map[string]allocation.CommitmentNow{},
		Changes: map[string]allocation.ChangeNow{}, Allocated: map[string]kernel.Decimal{}}
	t, c := string(scope.Tenant), string(scope.Company)
	rows, err := exec.Query(ctx, `SELECT draft_id, status, voided_at FROM brant_pur.purchase_draft WHERE tenant_id = $1 AND company_id = $2
		AND draft_id = ANY($3)`, t, c, drafts)
	if err != nil {
		return now, err
	}
	if _, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (struct{}, error) {
		var id string
		var d allocation.DraftNow
		var at *time.Time
		err := r.Scan(&id, &d.Status, &at)
		if at != nil {
			d.VoidedAt = kernel.Instant(*at)
		}
		now.Drafts[id] = d
		return struct{}{}, err
	}); err != nil {
		return now, err
	}
	rows, err = exec.Query(ctx, `SELECT commitment_id, status, revision, cancelled_at FROM brant_pur.purchase_commitment WHERE tenant_id = $1
		AND company_id = $2 AND commitment_id = ANY($3)`, t, c, commitments)
	if err != nil {
		return now, err
	}
	if _, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (struct{}, error) {
		var id string
		var x allocation.CommitmentNow
		var at *time.Time
		err := r.Scan(&id, &x.Status, &x.Revision, &at)
		if at != nil {
			x.CancelledAt = kernel.Instant(*at)
		}
		now.Commitments[id] = x
		return struct{}{}, err
	}); err != nil {
		return now, err
	}
	rows, err = exec.Query(ctx, `SELECT change_id, commitment_id, status, base_revision, coalesce(applied_revision, 0), applied_at
		FROM brant_pur.purchase_commitment_change WHERE tenant_id = $1 AND company_id = $2 AND change_id = ANY($3)`, t, c, changes)
	if err != nil {
		return now, err
	}
	if _, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (struct{}, error) {
		var id string
		var x allocation.ChangeNow
		var at *time.Time
		err := r.Scan(&id, &x.CommitmentID, &x.Status, &x.BaseRevision, &x.AppliedRevision, &at)
		if at != nil {
			x.AppliedAt = kernel.Instant(*at)
		}
		now.Changes[id] = x
		return struct{}{}, err
	}); err != nil {
		return now, err
	}
	rows, err = exec.Query(ctx, `SELECT order_line_id, allocated::text FROM brant_pur.demand_allocation WHERE tenant_id = $1 AND company_id = $2
		AND order_id = $3`, t, c, orderID)
	if err != nil {
		return now, err
	}
	_, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (struct{}, error) {
		var line, q string
		if err := r.Scan(&line, &q); err != nil {
			return struct{}{}, err
		}
		d, err := quantity(q)
		now.Allocated[line] = d
		return struct{}{}, err
	})
	return now, err
}
