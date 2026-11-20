package application

import (
	"context"
	"slices"
	"sort"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	pin "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/purchaseinvoice"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ResolvePurchaseInvoices answers PurchaseInvoiceRef v1 (api.PurchaseInvoiceLookups; Context Map R55 CNX→STL, S5R-D20):
// the verified invoices matched to the commitments asked, the red invoices reversing them, and the invoices asked by
// ID, in ID order, read without a lock in CNX's scope with an active grant only.
func (s Service) ResolvePurchaseInvoices(ctx context.Context, q api.PurchaseInvoiceQuery) ([]api.PurchaseInvoiceRef, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	out := []api.PurchaseInvoiceRef{}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		ids, err := postgres.InvoicesOfCommitments(ctx, exec, sc, q.CommitmentIDs)
		if err != nil {
			return err
		}
		for _, id := range q.InvoiceIDs {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			inv, err := postgres.LoadInvoice(ctx, exec, sc, id, postgres.NoLock)
			if err != nil {
				return err
			}
			if inv == nil || inv.Verified == nil || (inv.Status != pin.Verified && inv.Status != pin.Reversed) {
				continue
			}
			ref, err := invoiceRef(ctx, exec, sc, inv)
			if err != nil {
				return err
			}
			out = append(out, ref)
		}
		return nil
	})
	return out, err
}

func minorOf(c kernel.Currency, s string) string {
	m, err := kernel.ParseAmount(c, s)
	if err != nil {
		return ""
	}
	return m.MinorString()
}

func decimalMinor(c kernel.Currency, d kernel.Decimal) string {
	return d.Mul(kernel.DecimalFromInt(pow10(c.MinorUnits()))).String()
}

func pow10(n int) int64 {
	out := int64(1)
	for i := 0; i < n; i++ {
		out *= 10
	}
	return out
}

// invoiceRef is PurchaseInvoiceRef v1's answer for verified invoice inv.
func invoiceRef(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, inv *pin.Invoice) (api.PurchaseInvoiceRef, error) {
	f := *inv.Verified
	c, err := f.Currency.Kernel()
	if err != nil {
		return api.PurchaseInvoiceRef{}, kernel.Reject(kernel.CodeReferenceInvalid, "/invoice_ids", "The invoice's currency is not intact")
	}
	out := api.PurchaseInvoiceRef{PurchaseInvoiceID: inv.ID, Status: inv.Status, Version: inv.RowVersion, Kind: f.Kind, Colour: f.Colour, Number: f.Number,
		IssuedOn: f.IssuedOn, SupplierPartyID: f.Supplier.PartyID, CurrencyCode: c.Code(), MinorUnits: c.MinorUnits(), NetMinor: minorOf(c, f.NetTotal),
		TaxMinor: minorOf(c, f.TaxTotal), GrossMinor: minorOf(c, f.GrossTotal), VerifiedAt: inv.DecidedAt, RedOf: inv.RedOfID, ReversedBy: []string{},
		Lines: []api.PurchaseInvoiceLineRef{}, Matches: []api.PurchaseInvoiceMatchRef{}}
	var reds []*pin.Invoice
	var taken []pin.Match
	if f.Colour == pin.Blue {
		if reds, err = postgres.RedInvoices(ctx, exec, sc, inv.ID); err != nil {
			return out, err
		}
		if taken, err = postgres.TakeBacks(ctx, exec, sc, inv.ID); err != nil {
			return out, err
		}
		for _, r := range reds {
			out.ReversedBy = append(out.ReversedBy, r.ID)
		}
		switch {
		case inv.Status == pin.Reversed:
			out.Reversal = api.ReversalFull
		case len(reds) > 0:
			out.Reversal = api.ReversalPartial
		}
	}
	reversed, takenBack := pin.Reversals(f, reds, taken)
	for _, l := range f.Lines {
		lr := api.PurchaseInvoiceLineRef{LineNo: l.LineNo, Unit: l.Unit, Quantity: l.Quantity, NetMinor: minorOf(c, l.NetAmount), TaxMinor: minorOf(c, l.TaxAmount)}
		if f.Colour == pin.Blue {
			x := reversed[l.LineNo]
			lr.ReversedQuantity, lr.ReversedNetMinor, lr.ReversedTaxMinor = x.Quantity.String(), decimalMinor(c, x.Net), decimalMinor(c, x.Tax)
		}
		out.Lines = append(out.Lines, lr)
	}
	matches, err := postgres.Matches(ctx, exec, sc, inv.ID)
	if err != nil {
		return out, err
	}
	for _, m := range matches {
		mr := api.PurchaseInvoiceMatchRef{LineNo: m.LineNo, CommitmentID: m.Ref.CommitmentID, OrderLineID: m.Ref.OrderLineID, Quantity: m.Quantity.String(),
			Unit: m.Unit, Status: m.Status(inv.Status, f.Colour, takenBack[m.Key()])}
		if f.Colour == pin.Blue {
			standing := m.Quantity.Sub(takenBack[m.Key()])
			if inv.Status == pin.Reversed {
				standing = kernel.Decimal{}
			}
			mr.TakenBack, mr.StandingQuantity = takenBack[m.Key()].String(), standing.String()
		}
		out.Matches = append(out.Matches, mr)
	}
	return out, nil
}
