package application

import (
	"context"
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The list queries (api/lists.go). Each is an audited read under trd.read; a query that fails
// validation is refused and audited, as readQuote does.

// listRead runs the audited list read command. fetch returns up to limit+1 rows of the caller's
// sight; listRead reports how many of them are the page and whether more follow.
func (s Service) listRead(ctx context.Context, command string, validation error, after string, limit int,
	fetch func(ctx context.Context, exec bentopg.Executor, sight postgres.Sight) (int, error)) (size int, more bool, err error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if aerr := s.Ledger.AuditRejected(ctx, command, "", rej.Code); aerr != nil {
				return 0, false, errors.Join(validation, aerr)
			}
		}
		return 0, false, validation
	}
	pos, _ := kernel.ListPosition(after, limit) // validation accepted it
	size = kernel.ListSize(limit)
	err = s.Ledger.Read(ctx, command, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		n, err := fetch(ctx, exec, postgres.Sight{DataScope: string(d.DataScope), ActorID: d.Actor.ID, Team: d.Team, After: pos, Limit: size + 1})
		more = n > size
		return err
	})
	return size, more, err
}

// ListQuotes returns a page of the quotes the caller sees, audited as quote.list.
func (s Service) ListQuotes(ctx context.Context, q api.QuoteListQuery) (api.QuotePage, error) {
	var rows []postgres.QuoteListRow
	size, more, err := s.listRead(ctx, api.QueryQuoteList, q.Validate(), q.After, q.Limit,
		func(ctx context.Context, exec bentopg.Executor, sight postgres.Sight) (int, error) {
			var err error
			rows, err = postgres.ListQuotes(ctx, exec, scope(ctx), postgres.QuoteFilter{Status: q.Status, CustomerID: q.CustomerID,
				RfqID: q.RfqID, Unanswered: q.AwaitingResponse}, sight)
			return len(rows), err
		})
	if err != nil {
		return api.QuotePage{}, err
	}
	out := api.QuotePage{Quotes: []api.QuoteSummary{}}
	if more {
		rows = rows[:size]
		out.Next = kernel.ListCursor{At: rows[size-1].UpdatedAt, ID: rows[size-1].ID}.String()
	}
	for _, r := range rows {
		out.Quotes = append(out.Quotes, api.QuoteSummary{QuoteID: r.ID, RootVersion: r.RootVersion, LastRevision: r.LastRevision,
			Status: r.Status, ActiveRevision: r.ActiveRevision, OpenReviewID: r.OpenReview, AwaitingResponse: r.Unanswered,
			CustomerID: r.CustomerID, RfqID: r.RfqID,
			DocumentProfile: r.DocumentProfile, ControlState: r.ControlState, OwnerActorID: r.OwnerActor, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

// ListOrders returns a page of the orders the caller sees, audited as order.list.
func (s Service) ListOrders(ctx context.Context, q api.OrderListQuery) (api.OrderPage, error) {
	var rows []postgres.OrderListRow
	size, more, err := s.listRead(ctx, api.QueryOrderList, q.Validate(), q.After, q.Limit,
		func(ctx context.Context, exec bentopg.Executor, sight postgres.Sight) (int, error) {
			var err error
			rows, err = postgres.ListOrders(ctx, exec, scope(ctx), q.Status, q.QuoteID, q.OpenReview, sight)
			return len(rows), err
		})
	if err != nil {
		return api.OrderPage{}, err
	}
	out := api.OrderPage{Orders: []api.OrderSummary{}}
	if more {
		rows = rows[:size]
		out.Next = kernel.ListCursor{At: rows[size-1].UpdatedAt, ID: rows[size-1].ID}.String()
	}
	for _, r := range rows {
		out.Orders = append(out.Orders, api.OrderSummary{OrderID: r.ID, QuoteID: r.QuoteID, Status: r.Status, RowVersion: r.RowVersion,
			OrderRevision: r.OrderRevision, Number: r.Number, OpenReviewID: r.OpenReview, DocumentProfile: r.DocumentProfile, UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

// ListResponses returns a page of the customer responses the caller sees, audited as
// quote.response.list. A summary carries nothing of what the customer wrote.
func (s Service) ListResponses(ctx context.Context, q api.ResponseListQuery) (api.ResponsePage, error) {
	var rows []postgres.ResponseListRow
	size, more, err := s.listRead(ctx, api.QueryResponseList, q.Validate(), q.After, q.Limit,
		func(ctx context.Context, exec bentopg.Executor, sight postgres.Sight) (int, error) {
			var err error
			rows, err = postgres.ListResponses(ctx, exec, scope(ctx), q.QuoteID, q.Classification, q.Unverified, sight)
			return len(rows), err
		})
	if err != nil {
		return api.ResponsePage{}, err
	}
	out := api.ResponsePage{Responses: []api.ResponseSummary{}}
	if more {
		rows = rows[:size]
		out.Next = kernel.ListCursor{At: rows[size-1].RecordedAt, ID: rows[size-1].ID}.String()
	}
	for _, r := range rows {
		v := api.ResponseSummary{ResponseID: r.ID, QuoteID: r.QuoteID, BusinessRevision: r.Revision, Classification: r.Classification,
			Verified: r.Verified, RecordedByID: r.RecordedBy, RecordedAt: r.RecordedAt}
		if r.ReceivedAt != nil {
			v.ReceivedAt = *r.ReceivedAt
		}
		out.Responses = append(out.Responses, v)
	}
	return out, nil
}

// RfqCoverage says which of the asked RFQs have a live quote the caller sees, audited as
// quote.rfq_coverage.
func (s Service) RfqCoverage(ctx context.Context, q api.RfqCoverageQuery) (api.RfqCoverage, error) {
	out := api.RfqCoverage{Covered: []api.CoveredRfq{}}
	_, _, err := s.listRead(ctx, api.QueryRfqCoverage, q.Validate(), "", 0,
		func(ctx context.Context, exec bentopg.Executor, sight postgres.Sight) (int, error) {
			rows, err := postgres.RfqCoverage(ctx, exec, scope(ctx), q.RfqIDs, sight)
			for _, r := range rows {
				out.Covered = append(out.Covered, api.CoveredRfq{RfqID: r.RfqID, Quotes: r.Quotes, LatestQuoteID: r.LatestQuoteID})
			}
			return 0, err
		})
	if err != nil {
		return api.RfqCoverage{}, err
	}
	return out, nil
}
