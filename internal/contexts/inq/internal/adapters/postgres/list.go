package postgres

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ListFilter selects a page of RFQs. DataScope, ActorID and Team are the caller's authorization
// decision: the query returns only rows that decision sees (authorization.Decision.Sees, as SQL).
// After is the last entry of the previous page, nil for the first.
type ListFilter struct {
	Status, CustomerID       string
	DataScope, ActorID, Team string
	After                    *kernel.ListCursor
	Limit                    int
}

// ListRow is one RFQ of a list.
type ListRow struct {
	ID, Status, CustomerID, ContactID, DocumentProfile, OwnerActor string
	RowVersion                                                     int64
	Unresolved                                                     int
	UpdatedAt                                                      time.Time
}

// List returns at most f.Limit RFQs of the scope that the caller sees, in (updated_at, rfq_id)
// descending order (index customer_rfq_listing), without locks.
func List(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, f ListFilter) ([]ListRow, error) {
	var afterAt *time.Time
	afterID := ""
	if f.After != nil {
		afterAt, afterID = &f.After.At, f.After.ID
	}
	rows, err := exec.Query(ctx, `SELECT rfq_id, status, row_version, coalesce(customer_party_id, ''), coalesce(contact_id, ''),
		document_profile, coalesce(cardinality(unresolved_fields), 0), owner_actor_id, updated_at
		FROM brant_inq.customer_rfq
		WHERE tenant_id = $1 AND company_id = $2
		  AND ($3 = '' OR status = $3)
		  AND ($4 = '' OR customer_party_id = $4)
		  AND ($5 = 'company' OR owner_actor_id = $6 OR ($5 = 'team' AND $7 <> '' AND owner_team_id = $7))
		  AND ($8::timestamptz IS NULL OR (updated_at, rfq_id) < ($8::timestamptz, $9))
		ORDER BY updated_at DESC, rfq_id DESC
		LIMIT $10`,
		string(scope.Tenant), string(scope.Company), f.Status, f.CustomerID, f.DataScope, f.ActorID, f.Team, afterAt, afterID, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ListRow
	for rows.Next() {
		var r ListRow
		if err := rows.Scan(&r.ID, &r.Status, &r.RowVersion, &r.CustomerID, &r.ContactID, &r.DocumentProfile, &r.Unresolved,
			&r.OwnerActor, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
