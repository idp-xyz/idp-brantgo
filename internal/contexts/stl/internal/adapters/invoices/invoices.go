// Package invoices implements STL's port to CNX's purchase invoices (application.PurchaseInvoices) on CNX's published
// contract (Context Map R55 CNX→STL, PurchaseInvoiceRef v1, guardLock false; S5R-D20): CNX's PurchaseInvoiceLookups
// before STL's transaction. STL never locks a CNX row and never calls CNX's transaction owner.
package invoices

import (
	"context"

	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/application"
)

// CNX answers STL's port from CNX's api. CNX is composed after STL, so Lookups returns CNX's lookups once CNX exists
// (nil: CNX shows STL no purchase invoice).
type CNX struct {
	Lookups func() cnxapi.PurchaseInvoiceLookups
}

var _ application.PurchaseInvoices = CNX{}

// ResolvePurchaseInvoices asks CNX's ResolvePurchaseInvoices (an active grant, no lock), at most
// cnxapi.MaxInvoiceLookups identifiers per lookup, and returns each invoice once, in the order CNX answered.
func (c CNX) ResolvePurchaseInvoices(ctx context.Context, q application.PurchaseInvoiceQuery) ([]application.PurchaseInvoice, error) {
	var lookups cnxapi.PurchaseInvoiceLookups
	if c.Lookups != nil {
		lookups = c.Lookups()
	}
	if lookups == nil {
		return nil, nil
	}
	var out []application.PurchaseInvoice
	seen := map[string]bool{}
	for _, batch := range batches(q) {
		refs, err := lookups.ResolvePurchaseInvoices(ctx, batch)
		if err != nil {
			return nil, err
		}
		for _, r := range refs {
			if seen[r.PurchaseInvoiceID] {
				continue
			}
			seen[r.PurchaseInvoiceID] = true
			out = append(out, invoiceOf(r))
		}
	}
	return out, nil
}

// batches splits q into lookups of at most cnxapi.MaxInvoiceLookups identifiers.
func batches(q application.PurchaseInvoiceQuery) []cnxapi.PurchaseInvoiceQuery {
	var out []cnxapi.PurchaseInvoiceQuery
	cur := cnxapi.PurchaseInvoiceQuery{}
	flush := func() {
		if len(cur.CommitmentIDs)+len(cur.InvoiceIDs) > 0 {
			out = append(out, cur)
			cur = cnxapi.PurchaseInvoiceQuery{}
		}
	}
	for _, id := range q.CommitmentIDs {
		if len(cur.CommitmentIDs)+len(cur.InvoiceIDs) == cnxapi.MaxInvoiceLookups {
			flush()
		}
		cur.CommitmentIDs = append(cur.CommitmentIDs, id)
	}
	for _, id := range q.InvoiceIDs {
		if len(cur.CommitmentIDs)+len(cur.InvoiceIDs) == cnxapi.MaxInvoiceLookups {
			flush()
		}
		cur.InvoiceIDs = append(cur.InvoiceIDs, id)
	}
	flush()
	return out
}

// invoiceOf is CNX's PurchaseInvoiceRef v1 as STL's port carries it.
func invoiceOf(r cnxapi.PurchaseInvoiceRef) application.PurchaseInvoice {
	p := application.PurchaseInvoice{PurchaseInvoiceID: r.PurchaseInvoiceID, Status: r.Status, Colour: r.Colour, Number: r.Number,
		SupplierPartyID: r.SupplierPartyID, CurrencyCode: r.CurrencyCode, RedOf: r.RedOf, ReversedBy: append([]string(nil), r.ReversedBy...),
		Reversal: r.Reversal}
	for _, m := range r.Matches {
		p.Matches = append(p.Matches, application.PurchaseInvoiceMatch{LineNo: m.LineNo, CommitmentID: m.CommitmentID, OrderLineID: m.OrderLineID})
	}
	return p
}
