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
	QueryQuoteList = "quote.list"
	QueryOrderList = "order.list"
)

// QuoteStatuses are the revision statuses a QuoteListQuery may name.
var QuoteStatuses = []string{"working", "frozen", "pending_approval", "active", "superseded", "withdrawn", "cancelled", "discarded"}

// OrderStatuses are the order statuses an OrderListQuery may name.
var OrderStatuses = []string{"draft", "active", "cancelled", "closed_unfulfilled", "closed"}

// QuoteListQuery asks for a page of quotes. Status is the status of the quote's latest revision;
// CustomerID and RfqID are the customer and the RFQ that revision cites. After is the Next of the
// previous page, empty for the first. Limit is 0 (kernel.DefaultListPage) to kernel.MaxListPage.
type QuoteListQuery struct {
	Status     string
	CustomerID string
	RfqID      string
	After      string
	Limit      int
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
// none is.
type QuoteSummary struct {
	QuoteID         string
	RootVersion     int64
	LastRevision    int64
	Status          string
	ActiveRevision  int64
	OpenReviewID    string
	CustomerID      string
	RfqID           string
	DocumentProfile string
	ControlState    string
	OwnerActorID    string
	UpdatedAt       time.Time
}

// QuotePage is a page of a QuoteListQuery. Next is the After of the following page, empty on the last.
type QuotePage struct {
	Quotes []QuoteSummary
	Next   string
}

// OrderListQuery asks for a page of orders. QuoteID narrows the list to the orders one quote was
// converted into. After and Limit are QuoteListQuery's.
type OrderListQuery struct {
	Status  string
	QuoteID string
	After   string
	Limit   int
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
