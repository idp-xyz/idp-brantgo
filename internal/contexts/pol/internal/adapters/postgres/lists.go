package postgres

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/listquery"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ConfigListRow is one configuration of a list: the root, its latest revision's status and its
// active revision.
type ConfigListRow struct {
	ID, Status                           string
	RootVersion, LastRevision, ActiveRev int64
	ValidFrom, ValidUntil                *time.Time
	UpdatedAt                            time.Time
}

// ListConfigs returns the configurations of kind in the scope, in (updated_at, config_id)
// descending order (index config_root_listing), without locks and without reading a body.
// search narrows to the IDs that contain it; active to the roots with an active revision.
func ListConfigs(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, kind, search string, active bool, s listquery.Sight) ([]ConfigListRow, error) {
	var w listquery.Where
	w.Scope("r", scope)
	w.Add("r.kind = " + w.Arg(kind))
	if search != "" {
		w.Contains(search, "r.config_id")
	}
	if active {
		w.Add("a.revision IS NOT NULL")
	}
	w.After(s, "r.updated_at", "r.config_id")
	rows, err := exec.Query(ctx, `SELECT r.config_id, r.root_version, r.last_revision, coalesce(v.status, ''), coalesce(a.revision, 0),
		a.valid_from, a.valid_until, r.updated_at
		FROM brant_pol.config_root r
		LEFT JOIN brant_pol.config_revision v ON v.tenant_id = r.tenant_id AND v.company_id = r.company_id AND v.kind = r.kind
			AND v.config_id = r.config_id AND v.revision = r.last_revision
		LEFT JOIN brant_pol.config_revision a ON a.tenant_id = r.tenant_id AND a.company_id = r.company_id AND a.kind = r.kind
			AND a.config_id = r.config_id AND a.status = 'active'
		WHERE `+w.SQL()+` ORDER BY r.updated_at DESC, r.config_id DESC LIMIT `+w.Arg(s.Limit), w.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigListRow
	for rows.Next() {
		var r ConfigListRow
		if err := rows.Scan(&r.ID, &r.RootVersion, &r.LastRevision, &r.Status, &r.ActiveRev, &r.ValidFrom, &r.ValidUntil, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
