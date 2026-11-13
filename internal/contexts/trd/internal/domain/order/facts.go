package order

import (
	"math/big"

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

// CostedLine is one line of the cost snapshot the source revision adopted, as CST gives it
// (CostSnapshotRef v1, Context Map R11): its RFQ line (the costing context's line), its quantity in
// the cost unit and its cost in minor units of the snapshot's currency.
type CostedLine struct {
	RfqLineID   string
	Quantity    string
	Unit        string
	AmountMinor string
}

// LineCosts returns the costing cost of the lines of revision r of the order whose snapshot is s
// (S5R-D33; R32 OrderCommercialSnapshot v1, OrderRevisionRef v1), by line ID, in minor units of the
// calculation's comparison currency: for a line that quotes an RFQ line in the SKU specification and
// cost unit the costing costed, what costed snapshot line gives that RFQ line at the costed
// quantity, and for another quantity (an amendment changed it) that cost × the line's normalized
// quantity ÷ the costed quantity, rounded HALF_UP to the minor unit once. The costing is used only
// when it is the one the source revision's calculation sealed: in its calculation currency
// (currency, the snapshot's) with lines adding up to the sealed cost (cost_minor); otherwise, and for
// a line no costing priced (added by an amendment, another SKU specification), there is none.
func (s Snapshot) LineCosts(r *orderrev.Revision, currency string, costed []CostedLine) map[string]string {
	out := map[string]string{}
	c := s.Commercial.Calculation
	if c == nil || r == nil || currency == "" || currency != c.CostScope.Currency {
		return out
	}
	byRfq := map[string]CostedLine{}
	sum := new(big.Int)
	for _, l := range costed {
		amount, ok := new(big.Int).SetString(l.AmountMinor, 10)
		if !ok || amount.Sign() < 0 {
			return out
		}
		sum.Add(sum, amount)
		byRfq[l.RfqLineID] = l
	}
	if sealed, ok := new(big.Int).SetString(c.CostMinor, 10); !ok || sealed.Cmp(sum) != 0 {
		return out
	}
	bindings := map[string]quote.LineBinding{}
	for _, b := range c.CostScope.Lines {
		bindings[b.RfqLineID] = b
	}
	for _, l := range r.Lines {
		b, bound := bindings[l.RfqLineID]
		cl, priced := byRfq[l.RfqLineID]
		if l.RfqLineID == "" || !bound || !priced || l.Sku.SkuID != b.SkuID || l.Sku.SpecRevision != b.SpecRevision || l.CostUnit != b.CostUnit ||
			cl.Unit != b.CostUnit {
			continue
		}
		costedQ, err1 := kernel.ParseDecimal(cl.Quantity, wideQuantity)
		q, err2 := kernel.ParseDecimal(l.NormalizedQuantity, wideQuantity)
		amount, err3 := kernel.ParseDecimal(cl.AmountMinor, wideQuantity)
		if err1 != nil || err2 != nil || err3 != nil || costedQ.Sign() <= 0 {
			continue
		}
		if q.Equal(costedQ) {
			out[l.LineID] = cl.AmountMinor
			continue
		}
		share, err := amount.Mul(q).Quo(costedQ)
		if err != nil {
			continue
		}
		out[l.LineID] = share.ScaledHalfUp(0).String()
	}
	return out
}

// wideQuantity reads the stored decimals of a costing (normalized quantities have up to 18
// fractional digits; an amount in minor units is an integer of up to 24 digits).
var wideQuantity = kernel.DecimalFormat{IntDigits: 24, Scale: 24}
