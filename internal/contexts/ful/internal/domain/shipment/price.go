package shipment

import (
	"math/big"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Price is an order line's price at the order revision a shipment binds, as TRD sealed it (UL
// FUL-ExportLot "数量与金额"; baseline 4.1 "出运保存价格、币种"; WBS S5-03b): the order currency (POL
// currency configuration ID, the ISO code, and revision), the unit price as TRD shows it, and the
// order line's net, tax and payable amounts in minor units for its whole quantity (the shipment
// line's OrderQuantity). TRD shows an order only at its current revision, so shipment.prepare keeps
// the price with the shipment for every later use.
type Price struct {
	CurrencyID       string
	CurrencyRevision int64
	UnitPrice        kernel.Decimal
	Net              *big.Int
	Tax              *big.Int
	Payable          *big.Int
}

// Amounts are net, tax and payable amounts in minor units of the order currency.
type Amounts struct {
	Net     *big.Int
	Tax     *big.Int
	Payable *big.Int
}

// LinePriceFact is what TRD says of one order line's price at the order's current revision: its unit
// price (PriceFormat) and its amounts in minor units (integer strings).
type LinePriceFact struct {
	UnitPrice    string
	NetMinor     string
	TaxMinor     string
	PayableMinor string
}

// PriceFact is what TRD says of the order's prices at its current revision: the currency and the
// prices by order line.
type PriceFact struct {
	CurrencyID       string
	CurrencyRevision int64
	Lines            map[string]LinePriceFact
}

// Minor parses an amount in minor units: "0" or an integer without leading zeros, an optional '-' and
// at most kernel.MaxMinorDigits digits.
func Minor(s string) (*big.Int, bool) {
	body := s
	if len(body) > 1 && body[0] == '-' {
		body = body[1:]
	}
	if body == "" || len(body) > kernel.MaxMinorDigits || (body[0] == '0' && (len(body) > 1 || body != s)) {
		return nil, false
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return nil, false
		}
	}
	v, ok := new(big.Int).SetString(s, 10)
	return v, ok
}

// Prices returns the price of every line of s, by line ID, from f, which TRD gave at the revision s
// binds. A line whose order line f does not price, or prices in a form FUL cannot read, makes the
// order not intact for FUL (REFERENCE_INVALID): no shipment is prepared without its prices.
func (s *Shipment) Prices(f PriceFact) (map[string]Price, error) {
	if !kernel.ValidIdentifier(f.CurrencyID) || f.CurrencyRevision < 1 {
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/order_id", "The order states no currency")
	}
	out := make(map[string]Price, len(s.Lines))
	for i, l := range s.Lines {
		p, ok := f.Lines[l.OrderLineID]
		unit, err := kernel.ParseDecimal(p.UnitPrice, kernel.PriceFormat)
		net, okNet := Minor(p.NetMinor)
		tax, okTax := Minor(p.TaxMinor)
		payable, okPayable := Minor(p.PayableMinor)
		if !ok || err != nil || !okNet || !okTax || !okPayable || net.Sign() < 0 || tax.Sign() < 0 || payable.Sign() < 0 {
			return nil, Reject(kernel.CodeReferenceInvalid, linePath(i, "order_line_id"), "The order line has no price FUL can read")
		}
		out[l.ID] = Price{CurrencyID: f.CurrencyID, CurrencyRevision: f.CurrencyRevision, UnitPrice: unit, Net: net, Tax: tax, Payable: payable}
	}
	return out, nil
}

// share is amount × part / whole rounded HALF_UP to an integer number of minor units.
func share(amount *big.Int, part, whole kernel.Decimal) *big.Int {
	q, err := kernel.DecimalFromMinor(amount, 0).Mul(part).Quo(whole)
	if err != nil {
		return new(big.Int)
	}
	return q.ScaledHalfUp(0)
}

// Of returns the amounts of quantity q of an order line ordered: the order line's net and tax
// amounts × q / ordered, each rounded HALF_UP to minor units once, and payable their sum. q equal to
// ordered gives the order line's net and tax exactly.
func (p Price) Of(q, ordered kernel.Decimal) Amounts {
	net, tax := share(p.Net, q, ordered), share(p.Tax, q, ordered)
	return Amounts{Net: net, Tax: tax, Payable: new(big.Int).Add(net, tax)}
}

// Add returns a + b.
func (a Amounts) Add(b Amounts) Amounts {
	z := func(x *big.Int) *big.Int {
		if x == nil {
			return new(big.Int)
		}
		return x
	}
	return Amounts{Net: new(big.Int).Add(z(a.Net), z(b.Net)), Tax: new(big.Int).Add(z(a.Tax), z(b.Tax)),
		Payable: new(big.Int).Add(z(a.Payable), z(b.Payable))}
}
