package postgres

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/listquery"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SkuListRow is one SKU of a list.
type SkuListRow struct {
	ID, Name, Status, BaseUnit, OwnerActor string
	SpecRevision, RowVersion               int64
	UpdatedAt                              time.Time
}

// ListSkus returns the SKUs of the scope the caller sees, in (updated_at, sku_id) descending
// order (index sku_listing), without locks. status and search narrow when not empty; search
// matches the name or the ID.
func ListSkus(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, status, search string, s listquery.Sight) ([]SkuListRow, error) {
	var w listquery.Where
	w.Scope("", scope)
	if status != "" {
		w.Add("status = " + w.Arg(status))
	}
	if search != "" {
		w.Contains(search, "name", "sku_id")
	}
	w.Sees(s, "owner_actor_id", "owner_team_id")
	w.After(s, "updated_at", "sku_id")
	rows, err := exec.Query(ctx, `SELECT sku_id, coalesce(name, ''), status, coalesce(base_uom, ''), spec_revision, row_version, owner_actor_id, updated_at
		FROM brant_cat.sku WHERE `+w.SQL()+` ORDER BY updated_at DESC, sku_id DESC LIMIT `+w.Arg(s.Limit), w.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkuListRow
	for rows.Next() {
		var r SkuListRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Status, &r.BaseUnit, &r.SpecRevision, &r.RowVersion, &r.OwnerActor, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
