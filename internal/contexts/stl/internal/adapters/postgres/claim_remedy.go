package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// RemedySettlement is one approved remedy STL has settled, by credit note or by refund.
type RemedySettlement struct {
	ClaimID      string
	RemedyID     string
	Kind         string
	SettlementID string
	OrderID      string
	AmountMinor  int64
	Currency     string
	RecordedAt   time.Time
}

// InsertRemedySettlement inserts the uniqueness row. A second insert of the same remedy is a
// unique violation (23505).
func InsertRemedySettlement(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, row RemedySettlement) error {
	t, c := ts(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stl.claim_remedy_settlement (tenant_id, company_id, claim_id, remedy_id, kind,
		settlement_id, order_id, amount_minor, currency, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		t, c, row.ClaimID, row.RemedyID, row.Kind, row.SettlementID, row.OrderID, row.AmountMinor, row.Currency, row.RecordedAt)
	return err
}

// UniqueViolation reports a primary-key collision.
func UniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
