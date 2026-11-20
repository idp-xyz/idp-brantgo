package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What a denied cost source leaves on TRD's objects (migrations/trd/000010; WF-03, S4 decision D-9):
// the risk hold on a revision goes with the root (formal.go writeHold), the risk record on an active
// order here. Records are only inserted.

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// RootsOfSnapshots returns, in ID order, the quote roots in scope whose active revision's commit
// evidence names one of snapshots: the roots a denial of those snapshots may hold, or whose order
// it may record a risk on. It locks nothing; the consumer locks each root and decides on it.
func RootsOfSnapshots(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, snapshots []string) ([]string, error) {
	rows, err := exec.Query(ctx, `SELECT DISTINCT e.quote_id FROM brant_trd.quote_commit_evidence e
		JOIN brant_trd.quote_revision r ON r.tenant_id = e.tenant_id AND r.company_id = e.company_id AND r.quote_id = e.quote_id
			AND r.revision = e.revision
		WHERE e.tenant_id = $1 AND e.company_id = $2 AND e.cost_snapshot_id = ANY($3) AND r.status = 'active'
		ORDER BY e.quote_id`, string(scope.Tenant), string(scope.Company), snapshots)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// LockRoots locks quote roots ids FOR UPDATE in ID order: the roots a denial concerns, before its
// commands take the authority guard (ADR-BR-011).
func LockRoots(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string) error {
	rows, err := exec.Query(ctx, `SELECT quote_id FROM brant_trd.quote_root WHERE tenant_id = $1 AND company_id = $2 AND quote_id = ANY($3)
		ORDER BY quote_id FOR UPDATE`, string(scope.Tenant), string(scope.Company), ids)
	if err != nil {
		return err
	}
	_, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return err
}

func loadOrderRisks(ctx context.Context, exec bentopg.Executor, t, c, id string) ([]*order.RiskRecord, error) {
	rows, err := exec.Query(ctx, `SELECT risk_id, cause, code, order_revision, order_row_version, cost_snapshot_id, cost_source_id,
		attestation_id, event_id, registered_by_kind, registered_by_id, register_epoch, registered_at
		FROM brant_trd.sales_order_risk WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY registered_at, risk_id`, t, c, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*order.RiskRecord, error) {
		r := &order.RiskRecord{}
		var kind string
		err := row.Scan(&r.ID, &r.Cause, &r.Code, &r.OrderRevision, &r.OrderRowVersion, &r.Denial.SnapshotID, &r.Denial.SourceID,
			&r.Denial.AttestationID, &r.Denial.EventID, &kind, &r.Registered.By.ID, &r.Registered.Epoch, &r.Registered.At)
		r.Registered.By.Kind, r.Registered.At = kernel.ActorKind(kind), kernel.Instant(r.Registered.At)
		return r, err
	})
}

// WriteOrderRisks inserts the fresh risk records of order o; the order row itself does not change.
func WriteOrderRisks(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, o *order.Order) error {
	for _, r := range o.Risks {
		if !r.Fresh {
			continue
		}
		d := r.Denial
		if _, err := exec.Exec(ctx, `INSERT INTO brant_trd.sales_order_risk (tenant_id, company_id, order_id, risk_id, cause, code,
			order_revision, order_row_version, cost_snapshot_id, cost_source_id, attestation_id, event_id, registered_by_kind,
			registered_by_id, register_epoch, registered_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
			string(scope.Tenant), string(scope.Company), o.ID, r.ID, r.Cause, r.Code, r.OrderRevision, r.OrderRowVersion, d.SnapshotID,
			d.SourceID, d.AttestationID, d.EventID, string(r.Registered.By.Kind), r.Registered.By.ID, r.Registered.Epoch,
			r.Registered.At); err != nil {
			return err
		}
		r.Fresh = false
	}
	return nil
}
