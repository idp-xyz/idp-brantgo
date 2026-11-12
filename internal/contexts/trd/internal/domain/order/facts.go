package order

import (
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What the order's snapshot says to downstream contexts beyond its current revision (S5 plan unit
// A2; OrderCommercialSnapshot v1, SalesOrderRef v1): the customer addresses and the analysis rate,
// both sealed with the source quote revision and changed by no amendment, and how the order ended.

// Addresses returns the customer addresses the source revision binds (customer_address_refs), each
// with the role and text its party snapshot sealed, in the order the revision binds them — the
// pairing quote.RecipientOf makes of a formal revision. A party snapshot that is not the customer's
// at its revision or does not pair one by one with the references is REFERENCE_INVALID at path.
func (s Snapshot) Addresses(path string) ([]quote.RecipientAddress, error) {
	c := s.Commercial
	snap := c.PartySnapshots.Customer
	if snap.PartyID != c.Data.Customer.PartyID || snap.PartyRevision != c.Data.Customer.Revision || len(snap.Addresses) != len(c.Data.Addresses) {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "The order's party snapshot is not its customer's")
	}
	out := make([]quote.RecipientAddress, 0, len(c.Data.Addresses))
	for i, a := range c.Data.Addresses {
		sealed := snap.Addresses[i]
		if sealed.AddressID != a.AddressID {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "The order's party snapshot does not pair with its address references")
		}
		out = append(out, quote.RecipientAddress{Ref: a, Role: sealed.Role, Address: sealed.Address})
	}
	return out, nil
}

// Analysis is the order's analysis rate (STL-INV-007, baseline 4.5): the comparison the source
// revision's calculation made of the order's net sales — the order currency's code, the POL revision
// of the comparison (the pricing policy's calculation) currency, and the fx_rate revision and rate
// it converted by; FX is nil and Rate "" when the two currencies are one (a factor of exactly 1).
type Analysis struct {
	OrderCurrency string
	Comparison    quote.Ref
	FX            *quote.Ref
	Rate          string
}

// Analysis returns the analysis rate of the source revision's sealed calculation, REFERENCE_INVALID
// at path for a snapshot without one or with a rate that is not stated with its revision.
func (s Snapshot) Analysis(path string) (Analysis, error) {
	c := s.Commercial.Calculation
	switch {
	case c == nil:
		return Analysis{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "The order's snapshot has no calculation")
	case (c.SalesFX == nil) != (c.ComparisonFXRate == ""):
		return Analysis{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "The calculation states a rate without its revision, or a revision without its rate")
	}
	a := Analysis{OrderCurrency: s.Commercial.Data.Currency.ID, Comparison: c.ComparisonCurrency, Rate: c.ComparisonFXRate}
	if fx := c.SalesFX; fx != nil {
		r := *fx
		a.FX = &r
	}
	return a, nil
}

// Ended returns the command that ended the order: order.cancel or order.close_unfulfilled for a
// draft (its closure), order.cancel, order.close_remainder or order.close for an active order (its
// end); "" while the order is a draft or active.
func (o *Order) Ended() string {
	switch {
	case o.End != nil:
		return o.End.Command
	case o.Closure == nil:
		return ""
	case o.Closure.Status == Cancelled:
		return orderrev.CommandCancel
	}
	return orderrev.CommandCloseUnfulfilled
}
