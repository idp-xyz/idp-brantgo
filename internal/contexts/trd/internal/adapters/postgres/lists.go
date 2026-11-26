package postgres

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Sight is the caller's authorization decision as a list query applies it: the rows of the scope
// that authorization.Decision.Sees accepts, as SQL. After is the last entry of the previous page,
// nil for the first; Limit is the most rows to return.
type Sight struct {
	DataScope, ActorID, Team string
	After                    *kernel.ListCursor
	Limit                    int
}

func (s Sight) position() (*time.Time, string) {
	if s.After == nil {
		return nil, ""
	}
	return &s.After.At, s.After.ID
}

// QuoteListRow is one quote of a list: the root and its latest revision.
type QuoteListRow struct {
	ID, Status, OpenReview, CustomerID, RfqID, DocumentProfile, ControlState, OwnerActor string
	RootVersion, LastRevision, ActiveRevision                                            int64
	UpdatedAt                                                                            time.Time
}

// ListQuotes returns the quote roots of the scope the caller sees, in (updated_at, quote_id)
// descending order (index quote_root_listing), without locks. status, customer and rfq narrow
// by the latest revision when not empty.
func ListQuotes(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, status, customer, rfq string, s Sight) ([]QuoteListRow, error) {
	at, id := s.position()
	rows, err := exec.Query(ctx, `SELECT q.quote_id, q.root_version, q.last_revision, r.status,
		coalesce((SELECT a.revision FROM brant_trd.quote_revision a WHERE a.tenant_id = q.tenant_id AND a.company_id = q.company_id
			AND a.quote_id = q.quote_id AND a.status = 'active'), 0),
		coalesce((SELECT v.review_id FROM brant_trd.quote_review v WHERE v.tenant_id = q.tenant_id AND v.company_id = q.company_id
			AND v.quote_id = q.quote_id AND v.status = 'open'), ''),
		coalesce(r.data::jsonb #>> '{customer_ref,party_id}', ''), coalesce(r.data::jsonb #>> '{rfq_ref,rfq_id}', ''),
		q.document_profile, q.control_state, q.owner_actor_id, q.updated_at
		FROM brant_trd.quote_root q
		JOIN brant_trd.quote_revision r ON r.tenant_id = q.tenant_id AND r.company_id = q.company_id AND r.quote_id = q.quote_id
			AND r.revision = q.last_revision
		WHERE q.tenant_id = $1 AND q.company_id = $2
		  AND ($3 = '' OR r.status = $3)
		  AND ($4 = '' OR r.data::jsonb #>> '{customer_ref,party_id}' = $4)
		  AND ($5 = '' OR r.data::jsonb #>> '{rfq_ref,rfq_id}' = $5)
		  AND ($6 = 'company' OR q.owner_actor_id = $7 OR ($6 = 'team' AND $8 <> '' AND q.owner_team_id = $8))
		  AND ($9::timestamptz IS NULL OR (q.updated_at, q.quote_id) < ($9::timestamptz, $10))
		ORDER BY q.updated_at DESC, q.quote_id DESC
		LIMIT $11`,
		string(scope.Tenant), string(scope.Company), status, customer, rfq, s.DataScope, s.ActorID, s.Team, at, id, s.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuoteListRow
	for rows.Next() {
		var r QuoteListRow
		if err := rows.Scan(&r.ID, &r.RootVersion, &r.LastRevision, &r.Status, &r.ActiveRevision, &r.OpenReview, &r.CustomerID, &r.RfqID,
			&r.DocumentProfile, &r.ControlState, &r.OwnerActor, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OrderListRow is one order of a list.
type OrderListRow struct {
	ID, QuoteID, Status, Number, OpenReview, DocumentProfile string
	RowVersion, OrderRevision                                int64
	UpdatedAt                                                time.Time
}

// ListOrders returns the orders of the scope whose quote root the caller sees, in (updated_at,
// order_id) descending order (index sales_order_listing), without locks. status and quote narrow
// when not empty.
func ListOrders(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, status, quote string, s Sight) ([]OrderListRow, error) {
	at, id := s.position()
	rows, err := exec.Query(ctx, `SELECT o.order_id, o.quote_id, o.status, o.row_version, o.order_revision, coalesce(o.number, ''),
		coalesce((SELECT v.review_id FROM brant_trd.sales_order_review v WHERE v.tenant_id = o.tenant_id AND v.company_id = o.company_id
			AND v.order_id = o.order_id AND v.status = 'open' ORDER BY v.requested_at DESC, v.review_id DESC LIMIT 1), ''),
		o.document_profile, o.updated_at
		FROM brant_trd.sales_order o
		JOIN brant_trd.quote_root q ON q.tenant_id = o.tenant_id AND q.company_id = o.company_id AND q.quote_id = o.quote_id
		WHERE o.tenant_id = $1 AND o.company_id = $2
		  AND ($3 = '' OR o.status = $3)
		  AND ($4 = '' OR o.quote_id = $4)
		  AND ($5 = 'company' OR q.owner_actor_id = $6 OR ($5 = 'team' AND $7 <> '' AND q.owner_team_id = $7))
		  AND ($8::timestamptz IS NULL OR (o.updated_at, o.order_id) < ($8::timestamptz, $9))
		ORDER BY o.updated_at DESC, o.order_id DESC
		LIMIT $10`,
		string(scope.Tenant), string(scope.Company), status, quote, s.DataScope, s.ActorID, s.Team, at, id, s.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrderListRow
	for rows.Next() {
		var r OrderListRow
		if err := rows.Scan(&r.ID, &r.QuoteID, &r.Status, &r.RowVersion, &r.OrderRevision, &r.Number, &r.OpenReview, &r.DocumentProfile,
			&r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
