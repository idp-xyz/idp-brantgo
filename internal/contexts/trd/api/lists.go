package api

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The list queries (S7-06 read models): a page of the quotes or the orders the caller sees, most
// recently changed first (kernel.ListCursor), audited under trd.read like every other read. A
// quote is seen through its root's owner and an order through the quote it converts, exactly as
// quote.get and order.get decide it; the data scope is applied in the query, so a page never
// spends its limit on rows the caller does not see.
const (
	QueryQuoteList    = "quote.list"
	QueryOrderList    = "order.list"
	QueryResponseList = "quote.response.list"
	QueryRfqCoverage  = "quote.rfq_coverage"
)

// QuoteStatuses are the revision statuses a QuoteListQuery may name.
var QuoteStatuses = []string{"working", "frozen", "pending_approval", "active", "superseded", "withdrawn", "cancelled", "discarded"}

// OrderStatuses are the order statuses an OrderListQuery may name.
var OrderStatuses = []string{"draft", "active", "cancelled", "closed_unfulfilled", "closed"}

// QuoteListQuery asks for a page of quotes. Status is the status of the quote's latest revision;
// CustomerID and RfqID are the customer and the RFQ that revision cites. After is the Next of the
// previous page, empty for the first. Limit is 0 (kernel.DefaultListPage) to kernel.MaxListPage.
//
// AwaitingResponse narrows to the quotes whose active revision no customer response answers yet,
// whatever the status of a later revision.
type QuoteListQuery struct {
	Status           string
	CustomerID       string
	RfqID            string
	AwaitingResponse bool
	After            string
	Limit            int
}

// Validate checks the closed contract.
func (q QuoteListQuery) Validate() error {
	switch {
	case q.Status != "" && !slices.Contains(QuoteStatuses, q.Status):
		return schema("/status")
	case q.CustomerID != "" && !kernel.ValidIdentifier(q.CustomerID):
		return schema("/customer_id")
	case q.RfqID != "" && !kernel.ValidIdentifier(q.RfqID):
		return schema("/rfq_id")
	}
	if _, field := kernel.ListPosition(q.After, q.Limit); field != "" {
		return schema("/" + field)
	}
	return nil
}

// QuoteSummary is one quote of a list: what a worklist shows before the quote is opened. Status,
// CustomerID and RfqID are those of the latest revision (LastRevision); ActiveRevision is the
// revision in force, 0 when none is; OpenReviewID is the review waiting for a decision, empty when
// none is. AwaitingResponse says the active revision has no customer response yet.
type QuoteSummary struct {
	QuoteID          string
	RootVersion      int64
	LastRevision     int64
	Status           string
	ActiveRevision   int64
	OpenReviewID     string
	AwaitingResponse bool
	CustomerID       string
	RfqID            string
	DocumentProfile  string
	ControlState     string
	OwnerActorID     string
	UpdatedAt        time.Time
}

// QuotePage is a page of a QuoteListQuery. Next is the After of the following page, empty on the last.
type QuotePage struct {
	Quotes []QuoteSummary
	Next   string
}

// OrderListQuery asks for a page of orders. QuoteID narrows the list to the orders one quote was
// converted into. After and Limit are QuoteListQuery's.
//
// OpenReview narrows to the orders that have a review waiting for a decision.
type OrderListQuery struct {
	Status     string
	QuoteID    string
	OpenReview bool
	After      string
	Limit      int
}

// Validate checks the closed contract.
func (q OrderListQuery) Validate() error {
	switch {
	case q.Status != "" && !slices.Contains(OrderStatuses, q.Status):
		return schema("/status")
	case q.QuoteID != "" && !kernel.ValidIdentifier(q.QuoteID):
		return schema("/quote_id")
	}
	if _, field := kernel.ListPosition(q.After, q.Limit); field != "" {
		return schema("/" + field)
	}
	return nil
}

// OrderSummary is one order of a list. OpenReviewID is the review waiting for a decision, empty
// when none is; Number is empty until the order has one.
type OrderSummary struct {
	OrderID         string
	QuoteID         string
	Status          string
	RowVersion      int64
	OrderRevision   int64
	Number          string
	OpenReviewID    string
	DocumentProfile string
	UpdatedAt       time.Time
}

// OrderPage is a page of an OrderListQuery. Next is the After of the following page, empty on the last.
type OrderPage struct {
	Orders []OrderSummary
	Next   string
}

// ResponseClassifications are the classifications a ResponseListQuery may name.
var ResponseClassifications = []string{"full_unconditional", "conditional", "partial", "reject", "ambiguous"}

// ResponseListQuery asks for a page of recorded customer responses, most recently recorded first.
// A response never changes, so its position in the list does not either. QuoteID and
// Classification narrow; Unverified narrows to the responses quote.response.verify can still
// take: full_unconditional, answering the revision that is active now, with no acceptance made
// from them, and whose revision has no valid or disputed acceptance from another reply. After and
// Limit are QuoteListQuery's.
type ResponseListQuery struct {
	QuoteID        string
	Classification string
	Unverified     bool
	After          string
	Limit          int
}

// Validate checks the closed contract.
func (q ResponseListQuery) Validate() error {
	switch {
	case q.QuoteID != "" && !kernel.ValidIdentifier(q.QuoteID):
		return schema("/quote_id")
	case q.Classification != "" && !slices.Contains(ResponseClassifications, q.Classification):
		return schema("/classification")
	}
	if _, field := kernel.ListPosition(q.After, q.Limit); field != "" {
		return schema("/" + field)
	}
	return nil
}

// ResponseSummary is one customer response of a list. It identifies the response and says how it
// was classified; what the customer wrote and where it was found stay in customer_response.get,
// which re-checks the reader's access to the sources. Verified says an acceptance was made from it.
type ResponseSummary struct {
	ResponseID       string
	QuoteID          string
	BusinessRevision int64
	Classification   string
	Verified         bool
	RecordedByID     string
	ReceivedAt       time.Time
	RecordedAt       time.Time
}

// ResponsePage is a page of a ResponseListQuery. Next is the After of the following page, empty on the last.
type ResponsePage struct {
	Responses []ResponseSummary
	Next      string
}

// MaxCoverageRfqs is the most RFQs one RfqCoverageQuery names.
const MaxCoverageRfqs = 50

// RfqCoverageQuery asks which of up to MaxCoverageRfqs RFQs already have a live quote the caller
// sees: a quote whose latest revision cites the RFQ and is working, frozen, pending approval or
// active. A worklist of RFQs to price uses it to leave out the ones already being quoted.
type RfqCoverageQuery struct {
	RfqIDs []string
}

// Validate checks the closed contract.
func (q RfqCoverageQuery) Validate() error {
	if len(q.RfqIDs) == 0 || len(q.RfqIDs) > MaxCoverageRfqs {
		return schema("/rfq_id")
	}
	seen := map[string]bool{}
	for _, id := range q.RfqIDs {
		if !kernel.ValidIdentifier(id) || seen[id] {
			return schema("/rfq_id")
		}
		seen[id] = true
	}
	return nil
}

// RfqCoverage lists the asked RFQs that have a live quote the caller sees; an RFQ that has none
// is absent. Quotes is how many, LatestQuoteID the one changed last.
type RfqCoverage struct {
	Covered []CoveredRfq
}

// CoveredRfq is one RFQ with live quotes.
type CoveredRfq struct {
	RfqID         string
	Quotes        int
	LatestQuoteID string
}
