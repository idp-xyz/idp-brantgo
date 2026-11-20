package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Commitment dispositions (migrations/pur/000006_pur_commitment_dispositions.sql; S5R-D05): the
// disposition row a decision locks after its commitment root, and the commitment's dispositions, read
// under that root's lock (every disposition command and every delivery verification locks it first).

const dispositionColumns = `disposition_id, commitment_id, status, row_version, draft_id, order_id, order_line_id, kind, quantity::text,
	unit_id, unit_revision, reason, evidence, requested_by_kind, requested_by_id, request_epoch, requested_at, decided_by_kind, decided_by_id,
	decide_epoch, decided_at, decision, decision_reason`

func scanDisposition(r row) (*commitment.Disposition, error) {
	x := &commitment.Disposition{}
	var q, evidence, rKind string
	var dKind, dID, decision, dReason *string
	var dEpoch *int64
	var dAt *time.Time
	if err := r.Scan(&x.ID, &x.CommitmentID, &x.Status, &x.RowVersion, &x.DraftID, &x.OrderID, &x.OrderLineID, &x.Kind, &q, &x.Unit.ID,
		&x.Unit.Revision, &x.Reason, &evidence, &rKind, &x.Requested.By.ID, &x.Requested.Epoch, &x.Requested.At, &dKind, &dID, &dEpoch, &dAt,
		&decision, &dReason); err != nil {
		return nil, err
	}
	x.Stored, x.Requested.By.Kind, x.Requested.At = x.RowVersion, kernel.ActorKind(rKind), kernel.Instant(x.Requested.At)
	var err error
	if x.Quantity, err = quantity(q); err != nil {
		return nil, err
	}
	if x.Evidence, err = evidenceOf(evidence); err != nil {
		return nil, err
	}
	if dKind != nil && dID != nil && dEpoch != nil && dAt != nil && decision != nil {
		d := &commitment.DispositionDecision{Act: commitment.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*dKind), ID: *dID}, Epoch: *dEpoch,
			At: kernel.Instant(*dAt)}, Approve: *decision == commitment.DecisionApprove}
		if dReason != nil {
			d.Reason = *dReason
		}
		x.Decision = d
	}
	return x, nil
}

// LoadDisposition returns disposition id, or nil; lock locks its row.
func LoadDisposition(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*commitment.Disposition, error) {
	x, err := scanDisposition(exec.QueryRow(ctx, `SELECT `+dispositionColumns+` FROM brant_pur.purchase_commitment_disposition
		WHERE tenant_id = $1 AND company_id = $2 AND disposition_id = $3`+string(lock), string(scope.Tenant), string(scope.Company), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return x, err
}

// DispositionsOfCommitment returns the dispositions of commitment commitmentID, by disposition ID. Call
// it with the commitment root locked for a consistent view of a line.
func DispositionsOfCommitment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, commitmentID string) ([]*commitment.Disposition, error) {
	rows, err := exec.Query(ctx, `SELECT `+dispositionColumns+` FROM brant_pur.purchase_commitment_disposition WHERE tenant_id = $1
		AND company_id = $2 AND commitment_id = $3 ORDER BY disposition_id`, string(scope.Tenant), string(scope.Company), commitmentID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*commitment.Disposition, error) { return scanDisposition(r) })
}

// DispositionsOfCommitments returns the dispositions of the commitments ids, by commitment ID, each by
// disposition ID.
func DispositionsOfCommitments(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string) (map[string][]*commitment.Disposition, error) {
	out := map[string][]*commitment.Disposition{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := exec.Query(ctx, `SELECT `+dispositionColumns+` FROM brant_pur.purchase_commitment_disposition WHERE tenant_id = $1
		AND company_id = $2 AND commitment_id = ANY($3) ORDER BY commitment_id, disposition_id`, string(scope.Tenant), string(scope.Company), ids)
	if err != nil {
		return nil, err
	}
	xs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*commitment.Disposition, error) { return scanDisposition(r) })
	for _, x := range xs {
		out[x.CommitmentID] = append(out[x.CommitmentID], x)
	}
	return out, err
}

// DispositionCommitment returns the commitment disposition id belongs to, read without a lock ("" when
// there is no such disposition): what a decision locks before the disposition.
func DispositionCommitment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (string, error) {
	var out string
	err := exec.QueryRow(ctx, `SELECT commitment_id FROM brant_pur.purchase_commitment_disposition WHERE tenant_id = $1 AND company_id = $2
		AND disposition_id = $3`, string(scope.Tenant), string(scope.Company), id).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return out, err
}

// InsertDisposition stores a requested disposition.
func InsertDisposition(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, x *commitment.Disposition) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pur.purchase_commitment_disposition (tenant_id, company_id, disposition_id, commitment_id,
		status, row_version, draft_id, order_id, order_line_id, kind, quantity, unit_id, unit_revision, reason, evidence, requested_by_kind,
		requested_by_id, request_epoch, requested_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::numeric, $12, $13, $14, $15, $16, $17, $18, $19, $19)`,
		string(scope.Tenant), string(scope.Company), x.ID, x.CommitmentID, x.Status, x.RowVersion, x.DraftID, x.OrderID, x.OrderLineID, x.Kind,
		decimalText(x.Quantity), x.Unit.ID, x.Unit.Revision, x.Reason, evidenceText(x.Evidence), string(x.Requested.By.Kind),
		x.Requested.By.ID, x.Requested.Epoch, x.Requested.At)
	return err
}

// SaveDisposition writes x's decision if the stored row is still at the version it was loaded at
// (ErrConflict otherwise).
func SaveDisposition(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, x *commitment.Disposition, at time.Time) error {
	d := x.Decision
	if d == nil {
		return errors.New("pur: a disposition is saved with its decision")
	}
	decision := commitment.DecisionReject
	if d.Approve {
		decision = commitment.DecisionApprove
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_pur.purchase_commitment_disposition SET status = $4, row_version = $5, decided_by_kind = $6,
		decided_by_id = $7, decide_epoch = $8, decided_at = $9, decision = $10, decision_reason = $11, updated_at = $12
		WHERE tenant_id = $1 AND company_id = $2 AND disposition_id = $3 AND row_version = $13`,
		string(scope.Tenant), string(scope.Company), x.ID, x.Status, x.RowVersion, string(d.By.Kind), d.By.ID, d.Epoch, d.At, decision,
		optText(d.Reason), at, x.Stored)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// PendingDeliveries returns the deliveries of commitment commitmentID with a claim awaiting its
// verification (a version recorded), by delivery ID. Call it with the commitment root locked: every
// delivery command locks it before it writes (S5R-D41).
func PendingDeliveries(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, commitmentID string) ([]string, error) {
	rows, err := exec.Query(ctx, `SELECT DISTINCT d.delivery_id FROM brant_pur.supplier_delivery d
		JOIN brant_pur.supplier_delivery_version v ON v.tenant_id = d.tenant_id AND v.company_id = d.company_id AND v.delivery_id = d.delivery_id
		WHERE d.tenant_id = $1 AND d.company_id = $2 AND d.commitment_id = $3 AND v.status = 'recorded' ORDER BY d.delivery_id`,
		string(scope.Tenant), string(scope.Company), commitmentID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
