package postgres

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/listquery"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Sight is the caller's sight and page of a list query (listquery.Sight).
type Sight = listquery.Sight

// QuoteListRow is one quote of a list: the root and its latest revision.
type QuoteListRow struct {
	ID, Status, OpenReview, CustomerID, RfqID, DocumentProfile, ControlState, OwnerActor string
	RootVersion, LastRevision, ActiveRevision                                            int64
	Unanswered                                                                           bool
	UpdatedAt                                                                            time.Time
}

// The two values of a quote's latest revision a list filters on. Index quote_revision_customer and
// quote_revision_rfq are on exactly these expressions.
const (
	revisionCustomer = `(r.data::jsonb #>> '{customer_ref,party_id}')`
	revisionRfq      = `(r.data::jsonb #>> '{rfq_ref,rfq_id}')`
	// liveStatuses are the revision statuses of a quote that is still in play.
	liveStatuses = `('working', 'frozen', 'pending_approval', 'active')`
	// unanswered holds for a quote root q whose active revision no customer response answers.
	unanswered = `EXISTS (SELECT 1 FROM brant_trd.quote_revision a WHERE a.tenant_id = q.tenant_id AND a.company_id = q.company_id
			AND a.quote_id = q.quote_id AND a.status = 'active'
			AND NOT EXISTS (SELECT 1 FROM brant_trd.customer_response c WHERE c.tenant_id = a.tenant_id AND c.company_id = a.company_id
				AND c.quote_id = a.quote_id AND c.revision = a.revision))`
)

// QuoteFilter narrows a quote list by the latest revision (Status, CustomerID, RfqID) and, with
// Unanswered, to the roots whose active revision no customer response answers.
type QuoteFilter struct {
	Status, CustomerID, RfqID string
	Unanswered                bool
}

// ListQuotes returns the quote roots of the scope the caller sees, in (updated_at, quote_id)
// descending order (index quote_root_listing), without locks.
func ListQuotes(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, f QuoteFilter, s Sight) ([]QuoteListRow, error) {
	var w listquery.Where
	w.Add("q.tenant_id = " + w.Arg(string(scope.Tenant)))
	w.Add("q.company_id = " + w.Arg(string(scope.Company)))
	if f.Status != "" {
		w.Add("r.status = " + w.Arg(f.Status))
	}
	if f.CustomerID != "" {
		w.Add(revisionCustomer + " = " + w.Arg(f.CustomerID))
	}
	if f.RfqID != "" {
		w.Add(revisionRfq + " = " + w.Arg(f.RfqID))
	}
	if f.Unanswered {
		w.Add(unanswered)
	}
	w.Sees(s, "q.owner_actor_id", "q.owner_team_id")
	w.After(s, "q.updated_at", "q.quote_id")
	rows, err := exec.Query(ctx, `SELECT q.quote_id, q.root_version, q.last_revision, r.status,
		coalesce((SELECT a.revision FROM brant_trd.quote_revision a WHERE a.tenant_id = q.tenant_id AND a.company_id = q.company_id
			AND a.quote_id = q.quote_id AND a.status = 'active'), 0),
		coalesce((SELECT v.review_id FROM brant_trd.quote_review v WHERE v.tenant_id = q.tenant_id AND v.company_id = q.company_id
			AND v.quote_id = q.quote_id AND v.status = 'open'), ''),
		coalesce(`+revisionCustomer+`, ''), coalesce(`+revisionRfq+`, ''), `+unanswered+`,
		q.document_profile, q.control_state, q.owner_actor_id, q.updated_at
		FROM brant_trd.quote_root q
		JOIN brant_trd.quote_revision r ON r.tenant_id = q.tenant_id AND r.company_id = q.company_id AND r.quote_id = q.quote_id
			AND r.revision = q.last_revision
		WHERE `+w.SQL()+`
		ORDER BY q.updated_at DESC, q.quote_id DESC
		LIMIT `+w.Arg(s.Limit), w.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuoteListRow
	for rows.Next() {
		var r QuoteListRow
		if err := rows.Scan(&r.ID, &r.RootVersion, &r.LastRevision, &r.Status, &r.ActiveRevision, &r.OpenReview, &r.CustomerID, &r.RfqID,
			&r.Unanswered, &r.DocumentProfile, &r.ControlState, &r.OwnerActor, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RfqCoverageRow says how many live quotes the caller sees for an RFQ and which changed last.
type RfqCoverageRow struct {
	RfqID, LatestQuoteID string
	Quotes               int
}

// RfqCoverage returns, for each of rfqs that has one, the live quotes of the scope the caller sees
// whose latest revision cites it: a quote whose latest revision is working, frozen, pending
// approval or active (index quote_revision_rfq).
func RfqCoverage(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, rfqs []string, s Sight) ([]RfqCoverageRow, error) {
	var w listquery.Where
	w.Add("q.tenant_id = " + w.Arg(string(scope.Tenant)))
	w.Add("q.company_id = " + w.Arg(string(scope.Company)))
	w.Add(revisionRfq + " = ANY(" + w.Arg(rfqs) + "::text[])")
	w.Add("r.status IN " + liveStatuses)
	w.Sees(s, "q.owner_actor_id", "q.owner_team_id")
	rows, err := exec.Query(ctx, `SELECT `+revisionRfq+` AS rfq_id, count(*),
		(array_agg(q.quote_id ORDER BY q.updated_at DESC, q.quote_id DESC))[1]
		FROM brant_trd.quote_root q
		JOIN brant_trd.quote_revision r ON r.tenant_id = q.tenant_id AND r.company_id = q.company_id AND r.quote_id = q.quote_id
			AND r.revision = q.last_revision
		WHERE `+w.SQL()+`
		GROUP BY 1 ORDER BY 1`, w.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RfqCoverageRow
	for rows.Next() {
		var r RfqCoverageRow
		if err := rows.Scan(&r.RfqID, &r.Quotes, &r.LatestQuoteID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResponseListRow is one customer response of a list: what identifies it, never what it says.
type ResponseListRow struct {
	ID, QuoteID, Classification, RecordedBy string
	Revision                                int64
	Verified                                bool
	ReceivedAt                              *time.Time
	RecordedAt                              time.Time
}

// ListResponses returns the customer responses of the scope whose quote root the caller sees, in
// (recorded_at, response_id) descending order (index customer_response_listing), without locks.
// unverified narrows to the responses quote.response.verify can still take: full_unconditional,
// answering the revision that is active now, with no acceptance made from them, and whose
// revision has no valid or disputed acceptance from another reply.
func ListResponses(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, quote, classification string, unverified bool, s Sight) ([]ResponseListRow, error) {
	const verified = `EXISTS (SELECT 1 FROM brant_trd.acceptance x WHERE x.tenant_id = c.tenant_id AND x.company_id = c.company_id
			AND x.response_id = c.response_id)`
	var w listquery.Where
	w.Add("c.tenant_id = " + w.Arg(string(scope.Tenant)))
	w.Add("c.company_id = " + w.Arg(string(scope.Company)))
	if quote != "" {
		w.Add("c.quote_id = " + w.Arg(quote))
	}
	if classification != "" {
		w.Add("c.classification = " + w.Arg(classification))
	}
	if unverified {
		w.Add("c.classification = 'full_unconditional'")
		w.Add("NOT " + verified)
		w.Add(`EXISTS (SELECT 1 FROM brant_trd.quote_revision a WHERE a.tenant_id = c.tenant_id AND a.company_id = c.company_id
			AND a.quote_id = c.quote_id AND a.revision = c.revision AND a.status = 'active')`)
		// A revision takes one live acceptance (index acceptance_valid_revision, and a disputed one
		// still stands in its way): once it has one, its other replies can no longer be verified.
		w.Add(`NOT EXISTS (SELECT 1 FROM brant_trd.acceptance x WHERE x.tenant_id = c.tenant_id AND x.company_id = c.company_id
			AND x.quote_id = c.quote_id AND x.revision = c.revision AND x.effectivity IN ('valid', 'disputed'))`)
	}
	w.Sees(s, "q.owner_actor_id", "q.owner_team_id")
	w.After(s, "c.recorded_at", "c.response_id")
	rows, err := exec.Query(ctx, `SELECT c.response_id, c.quote_id, c.revision, c.classification, c.recorded_by_id, `+verified+`,
		c.received_at, c.recorded_at
		FROM brant_trd.customer_response c
		JOIN brant_trd.quote_root q ON q.tenant_id = c.tenant_id AND q.company_id = c.company_id AND q.quote_id = c.quote_id
		WHERE `+w.SQL()+`
		ORDER BY c.recorded_at DESC, c.response_id DESC
		LIMIT `+w.Arg(s.Limit), w.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ResponseListRow
	for rows.Next() {
		var r ResponseListRow
		if err := rows.Scan(&r.ID, &r.QuoteID, &r.Revision, &r.Classification, &r.RecordedBy, &r.Verified, &r.ReceivedAt, &r.RecordedAt); err != nil {
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
// when not empty, openReview to the orders with a review waiting for a decision.
func ListOrders(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, status, quote string, openReview bool, s Sight) ([]OrderListRow, error) {
	var w listquery.Where
	w.Add("o.tenant_id = " + w.Arg(string(scope.Tenant)))
	w.Add("o.company_id = " + w.Arg(string(scope.Company)))
	if status != "" {
		w.Add("o.status = " + w.Arg(status))
	}
	if quote != "" {
		w.Add("o.quote_id = " + w.Arg(quote))
	}
	if openReview {
		w.Add(`EXISTS (SELECT 1 FROM brant_trd.sales_order_review v WHERE v.tenant_id = o.tenant_id AND v.company_id = o.company_id
			AND v.order_id = o.order_id AND v.status = 'open')`)
	}
	w.Sees(s, "q.owner_actor_id", "q.owner_team_id")
	w.After(s, "o.updated_at", "o.order_id")
	rows, err := exec.Query(ctx, `SELECT o.order_id, o.quote_id, o.status, o.row_version, o.order_revision, coalesce(o.number, ''),
		coalesce((SELECT v.review_id FROM brant_trd.sales_order_review v WHERE v.tenant_id = o.tenant_id AND v.company_id = o.company_id
			AND v.order_id = o.order_id AND v.status = 'open' ORDER BY v.requested_at DESC, v.review_id DESC LIMIT 1), ''),
		o.document_profile, o.updated_at
		FROM brant_trd.sales_order o
		JOIN brant_trd.quote_root q ON q.tenant_id = o.tenant_id AND q.company_id = o.company_id AND q.quote_id = o.quote_id
		WHERE `+w.SQL()+`
		ORDER BY o.updated_at DESC, o.order_id DESC
		LIMIT `+w.Arg(s.Limit), w.Args...)
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
