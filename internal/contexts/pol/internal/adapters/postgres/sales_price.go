package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The sales price list (kind sales_price, migrations/pol/000007): the subject of a price — its SKU,
// customer and currency — lives in the revision's data, which the unique index
// config_revision_sales_price_subject and the two reads below address by the same expressions.

// SalesPriceHolder returns the ID of a configuration other than except whose active revision is the
// sales price of the subject (sku, party, currency), or "": at most one configuration is active
// for a subject. party is "" for the general price.
func SalesPriceHolder(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, sku, party, currency, except string) (string, error) {
	var id string
	err := exec.QueryRow(ctx, `SELECT config_id FROM brant_pol.config_revision
		WHERE tenant_id = $1 AND company_id = $2 AND kind = 'sales_price' AND status = 'active'
			AND (data->>'sku_id') = $3 AND COALESCE(data->>'party_id', '') = $4 AND (data->>'currency') = $5 AND config_id <> $6
		ORDER BY config_id COLLATE "C" LIMIT 1`,
		string(scope.Tenant), string(scope.Company), sku, party, currency, except).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// SalesPricesInForce returns the sales_price revisions that were published and not yet superseded
// or retired at instant at, valid at it, in currency, for any of skus, general or of party (""
// for the general ones only): in SKU order, a SKU's general price before the party's own. It takes
// no lock: published revisions never change.
func SalesPricesInForce(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, skus []string, party, currency string, at time.Time) ([]Stored, error) {
	return collectRevisions(exec.Query(ctx, `SELECT `+revisionColumns+` FROM brant_pol.config_revision
		WHERE tenant_id = $1 AND company_id = $2 AND kind = 'sales_price'
			AND published_at <= $3 AND (ended_at IS NULL OR ended_at > $3)
			AND valid_from <= $3 AND (valid_until IS NULL OR valid_until > $3)
			AND (data->>'currency') = $4 AND (data->>'sku_id') = ANY($5::text[])
			AND (COALESCE(data->>'party_id', '') = '' OR (data->>'party_id') = $6)
		ORDER BY (data->>'sku_id') COLLATE "C", COALESCE(data->>'party_id', '') COLLATE "C", config_id COLLATE "C", revision`,
		string(scope.Tenant), string(scope.Company), at, currency, skus, party))
}
