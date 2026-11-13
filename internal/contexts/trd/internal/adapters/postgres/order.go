package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Sales orders, their reviews, commit evidence and closures (migrations/trd/000005), written in the
// transaction of the quote root they convert: the order once with its snapshot, then only its
// status, row version and activating review; a review once, decided once and revoked once; commit
// evidence and closures once. Nothing is deleted.

const orderColumns = `order_id, quote_id, slot, revision, business_hash, number, document_sha256, acceptance_id, acceptance_version,
	arrival_at, arrival_basis, expires_at, order_revision, snapshot, snapshot_hash, order_hash, status, row_version, prepared_by_kind,
	prepared_by_id, prepare_epoch, prepared_at, coalesce(activated_review_id, '')`

// LoadOrder returns order id with its reviews, commit evidence and closure, or nil, locking the
// order row as lock says.
func LoadOrder(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*order.Order, error) {
	return loadOrder(ctx, exec, scope, `order_id = $3`, id, lock)
}

// LoadOrderOfQuote returns the order quote id converted into, or nil, unlocked.
func LoadOrderOfQuote(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, quoteID string) (*order.Order, error) {
	return loadOrder(ctx, exec, scope, `quote_id = $3`, quoteID, NoLock)
}

// OrderQuote returns the quote order id converts, "" for none: what an order command locks first.
func OrderQuote(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (string, error) {
	var quoteID string
	err := exec.QueryRow(ctx, `SELECT quote_id FROM brant_trd.sales_order WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3`,
		string(scope.Tenant), string(scope.Company), id).Scan(&quoteID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return quoteID, err
}

func loadOrder(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, where, arg string, lock Lock) (*order.Order, error) {
	t, c := string(scope.Tenant), string(scope.Company)
	o := &order.Order{}
	var status, kind, snapshot string
	err := exec.QueryRow(ctx, `SELECT `+orderColumns+` FROM brant_trd.sales_order WHERE tenant_id = $1 AND company_id = $2 AND `+where+
		string(lock), t, c, arg).Scan(&o.ID, &o.Source.QuoteID, &o.Slot, &o.Source.Revision, &o.Source.BusinessHash, &o.Source.Number,
		&o.Source.DocumentSHA256, &o.Source.AcceptanceID, &o.Source.AcceptanceVersion, &o.Source.ArrivalAt, &o.Source.ArrivalBasis,
		&o.Source.ExpiresAt, &o.OrderRevision, &snapshot, &o.SnapshotHash, &o.OrderHash, &status, &o.RowVersion, &kind, &o.Prepared.By.ID,
		&o.Prepared.Epoch, &o.Prepared.At, &o.ActivatedReview)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.Snapshot, o.Status, o.Prepared.By.Kind = []byte(snapshot), order.Status(status), kernel.ActorKind(kind)
	o.Source.ArrivalAt, o.Source.ExpiresAt, o.Prepared.At = kernel.Instant(o.Source.ArrivalAt), kernel.Instant(o.Source.ExpiresAt),
		kernel.Instant(o.Prepared.At)
	if o.Reviews, err = loadOrderReviews(ctx, exec, t, c, o.ID); err != nil {
		return nil, err
	}
	if o.Commits, err = loadOrderCommits(ctx, exec, t, c, o.ID); err != nil {
		return nil, err
	}
	if o.Closure, err = loadOrderClosure(ctx, exec, t, c, o.ID); err != nil {
		return nil, err
	}
	if err = loadActive(ctx, exec, t, c, o); err != nil {
		return nil, err
	}
	if o.Risks, err = loadOrderRisks(ctx, exec, t, c, o.ID); err != nil {
		return nil, err
	}
	return o, nil
}

func loadOrderReviews(ctx context.Context, exec bentopg.Executor, t, c, id string) ([]*order.Review, error) {
	rows, err := exec.Query(ctx, `SELECT review_id, order_revision, order_hash, status, requested_by_kind, requested_by_id, request_epoch,
		requested_at, decided_by_kind, decided_by_id, decide_epoch, decided_at, decision_reason, decision_evidence, expires_at,
		acknowledged_risks, revoked_by_kind, revoked_by_id, revoke_epoch, revoked_at, revoke_reason, revoke_evidence
		FROM brant_trd.sales_order_review WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY requested_at, review_id`,
		t, c, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*order.Review, error) {
		r := &order.Review{}
		var status, kind string
		var dKind, dID, dReason, dProofs, risks, vKind, vID, vReason, vProofs *string
		var dEpoch, vEpoch *int64
		var dAt, expires, vAt *time.Time
		if err := row.Scan(&r.ID, &r.OrderRevision, &r.OrderHash, &status, &kind, &r.Requested.By.ID, &r.Requested.Epoch, &r.Requested.At,
			&dKind, &dID, &dEpoch, &dAt, &dReason, &dProofs, &expires, &risks, &vKind, &vID, &vEpoch, &vAt, &vReason, &vProofs); err != nil {
			return nil, err
		}
		r.Status, r.Requested.By.Kind, r.Requested.At = order.ReviewStatus(status), kernel.ActorKind(kind), kernel.Instant(r.Requested.At)
		if dAt != nil {
			d, err := act(*dKind, *dID, *dEpoch, *dAt, dReason, *dProofs)
			if err != nil {
				return nil, err
			}
			r.Decided = &d
		}
		if expires != nil {
			r.ExpiresAt = kernel.Instant(*expires)
		}
		if risks != nil {
			if err := json.Unmarshal([]byte(*risks), &r.Acknowledged); err != nil {
				return nil, err
			}
		}
		if vAt != nil {
			v, err := act(*vKind, *vID, *vEpoch, *vAt, vReason, *vProofs)
			if err != nil {
				return nil, err
			}
			r.Revoked = &v
		}
		return r, nil
	})
}

func loadOrderCommits(ctx context.Context, exec bentopg.Executor, t, c, id string) ([]*order.Commit, error) {
	rows, err := exec.Query(ctx, `SELECT idempotency_key, order_revision, order_hash, review_id, approval_id, cost_snapshot_id, authority_epoch,
		control_epoch, row_version, committed_by_kind, committed_by_id, committed_at, entries
		FROM brant_trd.sales_order_commit_evidence WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY order_revision`, t, c, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*order.Commit, error) {
		m := &order.Commit{}
		var kind, entries string
		if err := row.Scan(&m.IdempotencyKey, &m.OrderRevision, &m.OrderHash, &m.ReviewID, &m.ApprovalID, &m.CostSnapshotID, &m.AuthorityEpoch,
			&m.ControlEpoch, &m.RowVersion, &kind, &m.By.ID, &m.At, &entries); err != nil {
			return nil, err
		}
		m.By.Kind, m.At = kernel.ActorKind(kind), kernel.Instant(m.At)
		return m, json.Unmarshal([]byte(entries), &m.Entries)
	})
}

func loadOrderClosure(ctx context.Context, exec bentopg.Executor, t, c, id string) (*order.Closure, error) {
	var status, kind, reason, proofs string
	cl := &order.Closure{}
	var at time.Time
	err := exec.QueryRow(ctx, `SELECT status, closed_by_kind, closed_by_id, close_epoch, closed_at, reason, evidence, row_version
		FROM brant_trd.sales_order_closure WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3`, t, c, id).
		Scan(&status, &kind, &cl.Closed.By.ID, &cl.Closed.Epoch, &at, &reason, &proofs, &cl.RowVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a, err := act(kind, cl.Closed.By.ID, cl.Closed.Epoch, at, &reason, proofs)
	cl.Status, cl.Closed = order.Status(status), a
	return cl, err
}

// SaveOrder writes what a command did to o: a fresh order, or its status, row version and activating
// review if it is still at row version from; then fresh and changed reviews, fresh commit evidence
// and a fresh closure. Commit evidence and a closure are written before the status they allow (the
// trigger checks them), reviews after the order they belong to.
func SaveOrder(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, o *order.Order, from int64, at time.Time) error {
	t, c := string(scope.Tenant), string(scope.Company)
	if o.Fresh {
		s := o.Source
		_, err := exec.Exec(ctx, `INSERT INTO brant_trd.sales_order (tenant_id, company_id, order_id, quote_id, slot, revision, business_hash,
			number, document_sha256, acceptance_id, acceptance_version, arrival_at, arrival_basis, expires_at, order_revision, snapshot,
			order_hash, status, row_version, prepared_by_kind, prepared_by_id, prepare_epoch, prepared_at, updated_at, snapshot_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $23, $24)`,
			t, c, o.ID, s.QuoteID, o.Slot, s.Revision, s.BusinessHash, s.Number, s.DocumentSHA256, s.AcceptanceID, s.AcceptanceVersion,
			s.ArrivalAt, s.ArrivalBasis, s.ExpiresAt, o.OrderRevision, string(o.Snapshot), o.OrderHash, string(o.Status), o.RowVersion,
			string(o.Prepared.By.Kind), o.Prepared.By.ID, o.Prepared.Epoch, o.Prepared.At, o.SnapshotHash)
		if err != nil {
			return err
		}
	}
	for _, m := range o.Commits {
		if m.Fresh {
			if err := writeOrderCommit(ctx, exec, t, c, o, m); err != nil {
				return err
			}
		}
	}
	if cl := o.Closure; cl != nil && cl.Fresh {
		proofs, err := proofsOf(cl.Closed.Evidence)
		if err != nil {
			return err
		}
		if _, err = exec.Exec(ctx, `INSERT INTO brant_trd.sales_order_closure (tenant_id, company_id, order_id, status, closed_by_kind,
			closed_by_id, close_epoch, closed_at, reason, evidence, row_version) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			t, c, o.ID, string(cl.Status), string(cl.Closed.By.Kind), cl.Closed.By.ID, cl.Closed.Epoch, cl.Closed.At, cl.Closed.Reason, proofs,
			cl.RowVersion); err != nil {
			return err
		}
	}
	// The active order's writes come before the order's own update, which the trigger checks against
	// them: the revision an apply produced, then the amendments (the applied one names that revision),
	// the apply's commit evidence, and how the order ended.
	if err := saveActive(ctx, exec, t, c, o, at); err != nil {
		return err
	}
	if o.Changed && !o.Fresh {
		var review any
		if o.ActivatedReview != "" {
			review = o.ActivatedReview
		}
		tag, err := exec.Exec(ctx, `UPDATE brant_trd.sales_order SET status = $4, row_version = $5, activated_review_id = $6, updated_at = $7,
			order_revision = $9, order_hash = $10 WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 AND row_version = $8`,
			t, c, o.ID, string(o.Status), o.RowVersion, review, at, from, o.OrderRevision, o.OrderHash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
	}
	for _, r := range o.Reviews {
		if err := writeOrderReview(ctx, exec, t, c, o.ID, r); err != nil {
			return err
		}
	}
	return nil
}

func writeOrderCommit(ctx context.Context, exec bentopg.Executor, t, c string, o *order.Order, m *order.Commit) error {
	entries, err := m.EntriesJSON()
	if err != nil {
		return err
	}
	s := o.Source
	_, err = exec.Exec(ctx, `INSERT INTO brant_trd.sales_order_commit_evidence (tenant_id, company_id, order_id, order_revision, command,
		idempotency_key, order_hash, review_id, quote_id, revision, business_hash, approval_id, acceptance_id, acceptance_version, arrival_at,
		expires_at, cost_snapshot_id, authority_epoch, control_epoch, row_version, committed_by_kind, committed_by_id, committed_at, entries,
		entries_sha256) VALUES ($1, $2, $3, $4, 'order.activate', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21,
		$22, $23, $24)`,
		t, c, o.ID, m.OrderRevision, m.IdempotencyKey, m.OrderHash, m.ReviewID, s.QuoteID, s.Revision, s.BusinessHash, m.ApprovalID,
		s.AcceptanceID, s.AcceptanceVersion, s.ArrivalAt, s.ExpiresAt, m.CostSnapshotID, m.AuthorityEpoch, m.ControlEpoch, m.RowVersion,
		string(m.By.Kind), m.By.ID, m.At, string(entries), quote.Hash(entries))
	return err
}

func writeOrderReview(ctx context.Context, exec bentopg.Executor, t, c, id string, r *order.Review) error {
	if r.Fresh {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_trd.sales_order_review (tenant_id, company_id, order_id, review_id, order_revision,
			order_hash, status, requested_by_kind, requested_by_id, request_epoch, requested_at) VALUES ($1, $2, $3, $4, $5, $6, 'open', $7, $8,
			$9, $10)`, t, c, id, r.ID, r.OrderRevision, r.OrderHash, string(r.Requested.By.Kind), r.Requested.By.ID, r.Requested.Epoch,
			r.Requested.At); err != nil {
			return err
		}
	} else if !r.Changed {
		return nil
	}
	if r.Status == order.ReviewOpen {
		return nil
	}
	if d := r.Decided; d != nil && (r.Status == order.ReviewApproved || r.Status == order.ReviewRejected) {
		proofs, err := proofsOf(d.Evidence)
		if err != nil {
			return err
		}
		var expires, risks any
		if r.Status == order.ReviewApproved {
			doc, err := json.Marshal(r.Acknowledged)
			if err != nil {
				return err
			}
			expires, risks = r.ExpiresAt, string(doc)
		}
		tag, err := exec.Exec(ctx, `UPDATE brant_trd.sales_order_review SET status = $5, decided_by_kind = $6, decided_by_id = $7,
			decide_epoch = $8, decided_at = $9, decision_reason = $10, decision_evidence = $11, expires_at = $12, acknowledged_risks = $13
			WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 AND review_id = $4 AND status = 'open'`,
			t, c, id, r.ID, string(r.Status), string(d.By.Kind), d.By.ID, d.Epoch, d.At, reasonOf(d.Reason), proofs, expires, risks)
		if err == nil && tag.RowsAffected() != 1 {
			err = ErrConflict
		}
		return err
	}
	v := r.Revoked
	proofs, err := proofsOf(v.Evidence)
	if err != nil {
		return err
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_trd.sales_order_review SET status = 'revoked', revoked_by_kind = $5, revoked_by_id = $6,
		revoke_epoch = $7, revoked_at = $8, revoke_reason = $9, revoke_evidence = $10
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 AND review_id = $4 AND status = 'approved'`,
		t, c, id, r.ID, string(v.By.Kind), v.By.ID, v.Epoch, v.At, v.Reason, proofs)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// loadContracts fills q.Contracts: the live (valid or disputed) acceptances of each revision.
func loadContracts(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, q *quote.Quote) error {
	rows, err := exec.Query(ctx, `SELECT revision, acceptance_id FROM brant_trd.acceptance WHERE tenant_id = $1 AND company_id = $2
		AND quote_id = $3 AND effectivity IN ('valid', 'disputed') ORDER BY revision, verified_at, acceptance_id`,
		string(scope.Tenant), string(scope.Company), q.ID)
	if err != nil {
		return err
	}
	q.Contracts = map[int64][]string{}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var rev int64
		var id string
		if err := row.Scan(&rev, &id); err != nil {
			return struct{}{}, err
		}
		q.Contracts[rev] = append(q.Contracts[rev], id)
		return struct{}{}, nil
	})
	return err
}
