package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fr "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Payer verifications of receipts (S5R-D21; migrations/stl/000008_stl_credit_payers.sql).

// PayerVerifications returns the verifications of fund record fundID's payer, in order.
func PayerVerifications(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, fundID string) ([]fr.PayerVerification, error) {
	t, c := ts(sc)
	rows, err := exec.Query(ctx, `SELECT seq, payer_id, customer_id, evidence_ids, reason, verified_by_kind, verified_by_id, verify_epoch,
		verified_at FROM brant_stl.fund_payer_verification WHERE tenant_id = $1 AND company_id = $2 AND fund_record_id = $3 ORDER BY seq`,
		t, c, fundID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (fr.PayerVerification, error) {
		var v fr.PayerVerification
		var reason, kind, by string
		var epoch int64
		var at time.Time
		err := row.Scan(&v.Seq, &v.PayerID, &v.CustomerID, &v.EvidenceIDs, &reason, &kind, &by, &epoch, &at)
		v.By = frAct(kind, by, epoch, at, reason)
		return v, err
	})
}

// SavePayerVerification moves fund record f from version from to its version and appends v.
func SavePayerVerification(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, f *fr.FundRecord, from int64, v *fr.PayerVerification) error {
	t, c := ts(sc)
	tag, err := exec.Exec(ctx, `UPDATE brant_stl.fund_record SET version = $4, updated_at = $5 WHERE tenant_id = $1 AND company_id = $2
		AND fund_record_id = $3 AND version = $6`, t, c, f.ID, f.Version, v.By.At, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	kind, id, epoch, at := fact(v.By)
	_, err = exec.Exec(ctx, `INSERT INTO brant_stl.fund_payer_verification (tenant_id, company_id, fund_record_id, seq, payer_id, customer_id,
		evidence_ids, reason, verified_by_kind, verified_by_id, verify_epoch, verified_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		t, c, f.ID, v.Seq, v.PayerID, v.CustomerID, v.EvidenceIDs, v.By.Reason, kind, id, epoch, at)
	return err
}
