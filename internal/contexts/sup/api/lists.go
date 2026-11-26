package api

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The list queries (S7-06 read models): a page of the supplier RFQs or the supplier offers the
// caller sees, most recently changed first (kernel.ListCursor), audited under sup.read. Each is
// seen as its read by ID decides it; the data scope is applied in the query. Together they are
// what a comparison of the offers made for one customer RFQ is built from.

// SupplierRfqStatuses are the statuses a SupplierRfqQuery may name.
var SupplierRfqStatuses = []string{StatusDraft, StatusIssued, StatusClosed}

// OfferStatuses are the revision statuses an OfferQuery may name.
var OfferStatuses = []string{StatusDraft, StatusActive, StatusSuperseded, StatusWithdrawn, StatusDiscarded}

// SupplierRfqQuery asks for a page of supplier RFQs. RfqID narrows to the ones sent for one
// customer RFQ, SupplierID to one supplier's. After is the Next of the previous page, empty for
// the first. Limit is 0 (kernel.DefaultListPage) to kernel.MaxListPage.
type SupplierRfqQuery struct {
	RfqID      string
	SupplierID string
	Status     string
	After      string
	Limit      int
}

// Validate checks the closed contract.
func (q SupplierRfqQuery) Validate() error {
	switch {
	case q.RfqID != "" && !kernel.ValidIdentifier(q.RfqID):
		return schema("/rfq_id")
	case q.SupplierID != "" && !kernel.ValidIdentifier(q.SupplierID):
		return schema("/supplier_id")
	case q.Status != "" && !slices.Contains(SupplierRfqStatuses, q.Status):
		return schema("/status")
	}
	if _, field := kernel.ListPosition(q.After, q.Limit); field != "" {
		return schema("/" + field)
	}
	return nil
}

// SupplierRfqSummary is one supplier RFQ of a list. Lines is the number of customer RFQ lines it asks about.
type SupplierRfqSummary struct {
	SupplierRfqID string
	Status        string
	RowVersion    int64
	RfqID         string
	SupplierID    string
	Lines         int
	OwnerActorID  string
	UpdatedAt     time.Time
}

// SupplierRfqPage is a page of a SupplierRfqQuery. Next is the After of the following page, empty on the last.
type SupplierRfqPage struct {
	SupplierRfqs []SupplierRfqSummary
	Next         string
}

// OfferQuery asks for a page of supplier offers. Status is the status of the offer's latest
// revision; RfqID and SupplierID are the customer RFQ and the supplier that revision names.
type OfferQuery struct {
	RfqID      string
	SupplierID string
	Status     string
	After      string
	Limit      int
}

// Validate checks the closed contract.
func (q OfferQuery) Validate() error {
	switch {
	case q.RfqID != "" && !kernel.ValidIdentifier(q.RfqID):
		return schema("/rfq_id")
	case q.SupplierID != "" && !kernel.ValidIdentifier(q.SupplierID):
		return schema("/supplier_id")
	case q.Status != "" && !slices.Contains(OfferStatuses, q.Status):
		return schema("/status")
	}
	if _, field := kernel.ListPosition(q.After, q.Limit); field != "" {
		return schema("/" + field)
	}
	return nil
}

// OfferSummary is one supplier offer of a list. Status, RfqID, SupplierID, SupplierRfqID, the
// validity and Lines are those of the latest revision (LastRevision); ActiveRevision is the
// revision in force, 0 when none is. What the offer prices is in supplier_offer.get.
type OfferSummary struct {
	OfferID        string
	RootVersion    int64
	LastRevision   int64
	Status         string
	ActiveRevision int64
	RfqID          string
	SupplierID     string
	SupplierRfqID  string
	ValidFrom      time.Time
	ValidUntil     time.Time
	Lines          int
	OwnerActorID   string
	UpdatedAt      time.Time
}

// OfferPage is a page of an OfferQuery. Next is the After of the following page, empty on the last.
type OfferPage struct {
	Offers []OfferSummary
	Next   string
}
