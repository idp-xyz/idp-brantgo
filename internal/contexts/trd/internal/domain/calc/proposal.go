package calc

import (
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The price proposal (owner decision 2026-11-18; S4 unit G4a): where a quotation's unit price comes
// from before a person changes it. For each RFQ line, in this order:
//
//  1. the sales price of the price list (POL kind sales_price) in force for the line's SKU in the
//     quote currency and in the line's unit: the customer's own before the general one;
//  2. the formula: the line's unit cost in the adopted cost snapshot divided by one minus the
//     pricing policy's target margin, the same definition of margin as the policy's floor
//     (CheckMargin: (R − C) ÷ R) — a cost of 660.00 gives 880.00 at 0.25 and 960.00 at 0.3125, 8.00 a
//     piece on a unit cost of 5.50;
//  3. no proposal, with the reason the formula has none.
//
// A list price is proposed as published. A formula price is computed exactly (cost ÷ costed
// quantity ÷ (1 − target margin), kernel.Decimal) and rounded once, HALF_UP, to the minor units of
// the quote currency — the precision of a proposed unit price: a price is an amount of the
// currency the customer pays in (USD 2 digits, JPY none) — for a basis quantity of 1 of the line's
// unit. A list price states its own basis quantity and may be finer (4.93 for 1000 PCS).
//
// Every proposal is judged as the freeze will judge the quote it becomes: the line's net amount at
// the proposed price (PriceLine's rounding, without discount or tax) against the snapshot's cost of
// the line, with CheckMargin on the policy's minimum margin, compared exactly. A formula price
// rounded down can lie a fraction under a floor that equals the target; the proposal says so and
// never rounds a price up on its own. It is a proposal: quote.save_draft takes the price its author
// sends, and a quote under the floor goes through the margin exception (TRD-INV-007).

// Sources of a proposed unit price.
const (
	PriceFromList    = "price_list"
	PriceFromFormula = "formula"
	PriceNone        = "none"
)

// Whose price a list price is.
const (
	ListPriceCustomer = "customer" // the customer's own
	ListPriceGeneral  = "general"  // for every customer
)

// Why a line has no proposal: no list price applies and the formula has none because
//
//   - no_adoptable_snapshot: no adoptable cost snapshot costs the line in its unit and quantity;
//   - another_currency: the snapshot's cost is in another currency than the quote's (the sales
//     exchange rate converts sales into the cost currency, never a cost into a price);
//   - no_target_margin: the pricing policy states no target margin;
//   - price_out_of_range: the formula's price is not one a quote line can carry: it rounds to
//     nothing in the currency's minor units (a cost of nothing, or of less than half a minor unit a
//     piece: such goods are priced by the list, per a larger basis quantity), or it has more than
//     12 integer digits.
const (
	NoAdoptableSnapshot = "no_adoptable_snapshot"
	AnotherCurrency     = "another_currency"
	NoTargetMargin      = "no_target_margin"
	PriceOutOfRange     = "price_out_of_range"
)

// PricedLine is one RFQ line a price is proposed for: its SKU and the quantity the customer asks
// for, in unit Unit (a unit code).
type PricedLine struct {
	LineID   string
	SkuID    string
	Quantity kernel.Decimal
	Unit     string
}

// ListPrice is one sales price in force in the quote currency: the POL revision Ref, the SKU, the
// customer it is for ("" for every customer), and UnitPrice per BasisQuantity of Unit.
type ListPrice struct {
	Ref           Ref
	SkuID         string
	Customer      string
	Unit          string
	UnitPrice     kernel.Decimal
	BasisQuantity kernel.Decimal
}

// LineCost is what the adopted cost snapshot costs one RFQ line: Amount for Quantity of Unit.
type LineCost struct {
	Amount   kernel.Money
	Quantity kernel.Decimal
	Unit     string
}

// SnapshotCost is an adoptable cost snapshot's cost by RFQ line ID, in its currency.
type SnapshotCost struct {
	Currency kernel.Currency
	Lines    map[string]LineCost
}

// ProposalBasis is what the proposals of one RFQ rest on: the quote currency, the customer, the
// sales prices in force in that currency, the adoptable snapshot's costs (nil when there is none)
// and the pricing policy's minimum margin and target margin (nil when it states none).
type ProposalBasis struct {
	Currency kernel.Currency
	Customer string
	Prices   []ListPrice
	Cost     *SnapshotCost
	Minimum  kernel.Decimal
	Target   *kernel.Decimal
}

// ProposedPrice is the proposal for one line. Source is price_list (List is the price used and
// ListScope whose it is), formula (UnitCost, exact, and Target are what it used) or none (Reason
// says why). UnitPrice is per BasisQuantity of the line's unit. Net is the line's net amount at the
// proposed price and Cost the snapshot's cost of the line, both nil unless the line has a price
// and an adoptable cost in the quote currency; Margin is then the margin rule's verdict on them
// (Rate, exact, and Passes: false when the price is under the policy's floor).
type ProposedPrice struct {
	Line          PricedLine
	Source        string
	Reason        string
	UnitPrice     kernel.Decimal
	BasisQuantity kernel.Decimal
	List          *ListPrice
	ListScope     string
	UnitCost      *kernel.Decimal
	Target        *kernel.Decimal
	Net           *kernel.Money
	Cost          *kernel.Money
	Margin        *Margin
}

// UnderFloor reports whether the proposed price was judged and lies under the policy's floor.
func (p ProposedPrice) UnderFloor() bool { return p.Margin != nil && !p.Margin.Passes() }

// ProposePrices proposes a unit price for every line on basis b, in the lines' order.
func ProposePrices(lines []PricedLine, b ProposalBasis) []ProposedPrice {
	out := make([]ProposedPrice, len(lines))
	for i, l := range lines {
		out[i] = proposePrice(l, b)
	}
	return out
}

func proposePrice(l PricedLine, b ProposalBasis) ProposedPrice {
	p := ProposedPrice{Line: l, Source: PriceNone}
	cost, costed := b.lineCost(l)
	if list, scope := b.listPrice(l); list != nil {
		p.Source, p.List, p.ListScope = PriceFromList, list, scope
		p.UnitPrice, p.BasisQuantity = list.UnitPrice, list.BasisQuantity
	} else {
		switch {
		case b.Cost == nil:
			p.Reason = NoAdoptableSnapshot
		case b.Cost.Currency != b.Currency:
			p.Reason = AnotherCurrency
		case !costed:
			p.Reason = NoAdoptableSnapshot
		case b.Target == nil || b.Target.Sign() < 0 || b.Target.Cmp(one) >= 0:
			p.Reason = NoTargetMargin
		default:
			unitCost, price := FormulaPrice(b.Currency, cost, *b.Target)
			if price.Sign() <= 0 || price.Cmp(integerLimit) >= 0 {
				p.Reason = PriceOutOfRange
				break
			}
			target := *b.Target
			p.Source, p.UnitPrice, p.BasisQuantity, p.UnitCost, p.Target = PriceFromFormula, price, one, &unitCost, &target
		}
	}
	if p.Source == PriceNone || !costed || b.Cost.Currency != b.Currency {
		return p
	}
	// The line's net amount as PriceLine rounds it, without discount or tax, judged as the freeze
	// judges the quote: exactly, on the policy's minimum margin.
	perUnit, _ := p.UnitPrice.Quo(p.BasisQuantity) // a proposed basis quantity is positive
	net, err := kernel.RoundMoney(b.Currency, l.Quantity.Mul(perUnit))
	if err != nil {
		return p
	}
	if m, err := CheckMargin("", net, cost.Amount, b.Minimum); err == nil {
		amount := cost.Amount
		p.Net, p.Cost, p.Margin = &net, &amount, &m
	}
	return p
}

// listPrice returns the sales price that applies to l and whose it is: of the prices for l's SKU
// in l's unit, the customer's own before the general one; nil when none applies. A price in another
// unit is not converted: a package is never guessed.
func (b ProposalBasis) listPrice(l PricedLine) (*ListPrice, string) {
	var general *ListPrice
	for i := range b.Prices {
		p := &b.Prices[i]
		if p.SkuID != l.SkuID || p.Unit != l.Unit || p.UnitPrice.Sign() <= 0 || p.BasisQuantity.Sign() <= 0 {
			continue
		}
		switch {
		case p.Customer != "" && p.Customer == b.Customer:
			return p, ListPriceCustomer
		case p.Customer == "" && general == nil:
			general = p
		}
	}
	if general != nil {
		return general, ListPriceGeneral
	}
	return nil, ""
}

// lineCost returns the snapshot's cost of l and whether it costs l as the customer asks for it: a
// line of the same ID in l's unit and quantity (the freeze requires both, BindScope), a positive
// quantity and a cost that is not negative.
func (b ProposalBasis) lineCost(l PricedLine) (LineCost, bool) {
	if b.Cost == nil {
		return LineCost{}, false
	}
	c, ok := b.Cost.Lines[l.LineID]
	if !ok || c.Unit != l.Unit || c.Quantity.Sign() <= 0 || !c.Quantity.Equal(l.Quantity) || c.Amount.Sign() < 0 ||
		c.Amount.Currency() != b.Cost.Currency {
		return LineCost{}, false
	}
	return c, true
}

// FormulaPrice is the formula of the price proposal: the unit cost of c (its amount ÷ its quantity,
// exact) and that unit cost ÷ (1 − target), rounded once HALF_UP to currency's minor units. The
// caller checked that c's quantity is positive and that target is at least 0 and below 1.
func FormulaPrice(currency kernel.Currency, c LineCost, target kernel.Decimal) (unitCost, price kernel.Decimal) {
	unitCost, _ = c.Amount.Decimal().Quo(c.Quantity)
	exact, _ := unitCost.Quo(one.Sub(target))
	return unitCost, exact.RoundHalfUp(currency.MinorUnits())
}
