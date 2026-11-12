package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Commitment changes (migrations/pur/000004_pur_commitment_changes.sql): the change row a decision and
// an application lock after the commitment root, and its lines in the draft's order.

const changeColumns = `change_id, commitment_id, status, row_version, base_revision, draft_id, order_id, order_revision, order_hash,
	supplier_id, currency, supply_valid_until, request, request_hash, document, change_hash, reason, evidence, prepared_by_kind,
	prepared_by_id, prepare_epoch, prepared_at, decided_by_kind, decided_by_id, decide_epoch, decided_at, decision, decision_reason,
	acknowledgements, applied_by_kind, applied_by_id, apply_epoch, applied_at, applied_revision`

func scanChange(r row) (*commitment.Change, error) {
	x := &commitment.Change{}
	var request, document, evidence, pKind string
	var dKind, dID, decision, dReason, acks, aKind, aID *string
	var dEpoch, aEpoch, applied *int64
	var dAt, aAt *time.Time
	if err := r.Scan(&x.ID, &x.CommitmentID, &x.Status, &x.RowVersion, &x.BaseRevision, &x.DraftID, &x.OrderID, &x.OrderRevision, &x.OrderHash,
		&x.SupplierID, &x.Currency, &x.SupplyValidUntil, &request, &x.RequestHash, &document, &x.Hash, &x.Reason, &evidence, &pKind,
		&x.Prepared.By.ID, &x.Prepared.Epoch, &x.Prepared.At, &dKind, &dID, &dEpoch, &dAt, &decision, &dReason, &acks, &aKind, &aID, &aEpoch,
		&aAt, &applied); err != nil {
		return nil, err
	}
	x.Stored, x.Request, x.Document = x.RowVersion, []byte(request), []byte(document)
	x.Prepared.By.Kind, x.Prepared.At, x.SupplyValidUntil = kernel.ActorKind(pKind), kernel.Instant(x.Prepared.At), kernel.Instant(x.SupplyValidUntil)
	var err error
	if x.Evidence, err = evidenceOf(evidence); err != nil {
		return nil, err
	}
	if dKind != nil && dID != nil && dEpoch != nil && dAt != nil && decision != nil {
		d := &commitment.ChangeDecision{Act: commitment.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*dKind), ID: *dID}, Epoch: *dEpoch,
			At: kernel.Instant(*dAt)}, Approve: *decision == commitment.DecisionApprove}
		if dReason != nil {
			d.Reason = *dReason
		}
		if acks != nil {
			var docs []ackDoc
			if err := json.Unmarshal([]byte(*acks), &docs); err != nil {
				return nil, fmt.Errorf("pur: stored acknowledgements of change %s: %w", x.ID, err)
			}
			d.Acknowledgements = []commitment.Acknowledgement{}
			for _, a := range docs {
				d.Acknowledgements = append(d.Acknowledgements, commitment.Acknowledgement{OrderLineID: a.OrderLineID, Kind: a.Kind, Reason: a.Reason})
			}
		}
		x.Decision = d
	}
	if aKind != nil && aID != nil && aEpoch != nil && aAt != nil && applied != nil {
		x.Applied = &commitment.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*aKind), ID: *aID}, Epoch: *aEpoch, At: kernel.Instant(*aAt)}
		x.AppliedRevision = *applied
	}
	return x, nil
}

// LoadChange returns change id with its lines, or nil; lock locks the change row.
func LoadChange(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*commitment.Change, error) {
	x, err := scanChange(exec.QueryRow(ctx, `SELECT `+changeColumns+` FROM brant_pur.purchase_commitment_change WHERE tenant_id = $1
		AND company_id = $2 AND change_id = $3`+string(lock), string(scope.Tenant), string(scope.Company), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return x, changeLines(ctx, exec, scope, []*commitment.Change{x})
}

// ChangesOfCommitment returns the changes of commitment commitmentID with their lines, by change ID.
func ChangesOfCommitment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, commitmentID string) ([]*commitment.Change, error) {
	rows, err := exec.Query(ctx, `SELECT `+changeColumns+` FROM brant_pur.purchase_commitment_change WHERE tenant_id = $1 AND company_id = $2
		AND commitment_id = $3 ORDER BY change_id`, string(scope.Tenant), string(scope.Company), commitmentID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*commitment.Change, error) { return scanChange(r) })
	if err != nil {
		return nil, err
	}
	return out, changeLines(ctx, exec, scope, out)
}

func changeLines(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, xs []*commitment.Change) error {
	if len(xs) == 0 {
		return nil
	}
	byID := map[string]*commitment.Change{}
	ids := make([]string, len(xs))
	for i, x := range xs {
		byID[x.ID], ids[i] = x, x.ID
	}
	rows, err := exec.Query(ctx, `SELECT change_id, order_line_id, allocated_quantity::text, overbuy_quantity::text,
		coalesce(to_char(supplier_date, 'YYYY-MM-DD'), '') FROM brant_pur.purchase_commitment_change_line
		WHERE tenant_id = $1 AND company_id = $2 AND change_id = ANY($3) ORDER BY change_id, line_no`,
		string(scope.Tenant), string(scope.Company), ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, a, o string
		var q commitment.LineQuantity
		if err = rows.Scan(&id, &q.OrderLineID, &a, &o, &q.SupplierDate); err != nil {
			return err
		}
		if q.Allocated, err = quantity(a); err != nil {
			return err
		}
		if q.Overbuy, err = quantity(o); err != nil {
			return err
		}
		if x := byID[id]; x != nil {
			x.Lines = append(x.Lines, q)
		}
	}
	return rows.Err()
}

// InsertChange stores a prepared change and its lines.
func InsertChange(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, x *commitment.Change) error {
	if _, err := exec.Exec(ctx, `INSERT INTO brant_pur.purchase_commitment_change (tenant_id, company_id, change_id, commitment_id, status,
		row_version, base_revision, draft_id, order_id, order_revision, order_hash, supplier_id, currency, supply_valid_until, line_count,
		request, request_hash, document, change_hash, reason, evidence, prepared_by_kind, prepared_by_id, prepare_epoch, prepared_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $25)`,
		string(scope.Tenant), string(scope.Company), x.ID, x.CommitmentID, x.Status, x.RowVersion, x.BaseRevision, x.DraftID, x.OrderID,
		x.OrderRevision, x.OrderHash, x.SupplierID, x.Currency, x.SupplyValidUntil, len(x.Lines), string(x.Request), x.RequestHash,
		string(x.Document), x.Hash, x.Reason, evidenceText(x.Evidence), string(x.Prepared.By.Kind), x.Prepared.By.ID, x.Prepared.Epoch,
		x.Prepared.At); err != nil {
		return err
	}
	for i, l := range x.Lines {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_pur.purchase_commitment_change_line (tenant_id, company_id, change_id, order_line_id,
			line_no, allocated_quantity, overbuy_quantity, purchase_quantity, supplier_date)
			VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8::numeric, $9::date)`,
			string(scope.Tenant), string(scope.Company), x.ID, l.OrderLineID, i, decimalText(l.Allocated), decimalText(l.Overbuy),
			decimalText(l.Purchased()), optDate(l.SupplierDate)); err != nil {
			return err
		}
	}
	return nil
}

// SaveChange writes x's state, decision and application if the stored row is still at the version
// it was loaded at (ErrConflict otherwise).
func SaveChange(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, x *commitment.Change, at time.Time) error {
	var dKind, dID, decision, dReason, acks *string
	var dEpoch *int64
	var dAt *time.Time
	if d := x.Decision; d != nil {
		kind, decided := string(d.By.Kind), commitment.DecisionReject
		if d.Approve {
			decided = commitment.DecisionApprove
			a := acksText(d.Acknowledgements)
			acks = &a
		}
		dKind, dID, dEpoch, dAt, decision, dReason = &kind, &d.By.ID, &d.Epoch, &d.At, &decided, optText(d.Reason)
	}
	var aKind, aID *string
	var aEpoch, applied *int64
	var aAt *time.Time
	if a := x.Applied; a != nil {
		kind := string(a.By.Kind)
		aKind, aID, aEpoch, aAt, applied = &kind, &a.By.ID, &a.Epoch, &a.At, &x.AppliedRevision
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_pur.purchase_commitment_change SET status = $4, row_version = $5, decided_by_kind = $6,
		decided_by_id = $7, decide_epoch = $8, decided_at = $9, decision = $10, decision_reason = $11, acknowledgements = $12,
		applied_by_kind = $13, applied_by_id = $14, apply_epoch = $15, applied_at = $16, applied_revision = $17, updated_at = $18
		WHERE tenant_id = $1 AND company_id = $2 AND change_id = $3 AND row_version = $19`,
		string(scope.Tenant), string(scope.Company), x.ID, x.Status, x.RowVersion, dKind, dID, dEpoch, dAt, decision, dReason, acks,
		aKind, aID, aEpoch, aAt, applied, at, x.Stored)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// ChangeCommitment returns the commitment change id belongs to, read without a lock ("" when there
// is no such change): what a decision or an application locks before the change.
func ChangeCommitment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (string, error) {
	var out string
	err := exec.QueryRow(ctx, `SELECT commitment_id FROM brant_pur.purchase_commitment_change WHERE tenant_id = $1 AND company_id = $2
		AND change_id = $3`, string(scope.Tenant), string(scope.Company), id).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return out, err
}
