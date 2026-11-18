package api

import (
	"context"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The sales price list and the target margin (owner decision 2026-11-18: a quotation's unit price
// comes from a price list where one is in force, otherwise from the formula cost ÷ (1 − target
// margin); people may change the price, and a price under the minimum margin goes through the
// margin exception). POL holds both facts; TRD's price proposal reads them (trd/api
// price_proposal.go) and never writes them into a quote: quote.save_draft takes the price its
// author sends.
//
// The price list is the configurations of kind sales_price: each is ONE price — of one SKU, in one
// currency, per a basis quantity of one unit, for every customer or for one — and its revisions are
// that price's history, with the life cycle of every kind (a working revision saved whole, published
// by a human other than its last editor, superseded by the next, retired; SM-POL-REVISION), its own
// validity, ConfigRef v1, ConfigGuard v1 and ConfigAt v1. Two rules make the list unambiguous:
//
//   - the subject of a configuration — its SKU, customer and currency — is fixed by its first
//     publication (IMMUTABLE_DEFINITION otherwise), so a configuration never becomes another price;
//   - at most one configuration is active for a subject (BUSINESS_KEY_CONFLICT at config.publish, and
//     a unique index in migrations/pol/000007), so a lookup finds at most one general price and one
//     price of the customer's own for a SKU and a currency.
//
// A sales price is published under the rule of the general kinds (PublishCapability: config.retire,
// the configuration administrator); the role templates name no publisher of prices.

// SalesPrice is one price of the sales price list: UnitPrice of Currency for PriceBasisQuantity of
// Unit of SKU SkuID, for every customer (PartyID empty) or for the customer PartyID alone. Its
// validity is the revision's. The SKU is CAT's and the party PTY's: neither is upstream of POL, so
// POL stores both as given and the consuming context reads them from their owners. The price is a
// decimal string, never a float: 8.00 for 1 PCS, or 4.93 for 1000 PCS when one piece costs less
// than a cent. POL leaves an empty PartyID out of the stored data.
type SalesPrice struct {
	SkuID              string // identifier: the SKU the price is for
	PartyID            string `json:",omitempty"` // "" for every customer, or the party ID of the one customer
	Currency           string // currency code, configured and in force
	Unit               string // unit code, configured and in force: the unit the price is per
	UnitPrice          string // kernel.PriceFormat, positive
	PriceBasisQuantity string // kernel.QuantityFormat, positive, within the unit's precision: the price is for this many Unit
}

func (p *SalesPrice) validate() error {
	return first(field{"sku_id", kernel.ValidIdentifier(p.SkuID)}, field{"party_id", p.PartyID == "" || kernel.ValidIdentifier(p.PartyID)},
		field{"currency", currencyCode(p.Currency)}, field{"unit", kernel.UnitCode(p.Unit).Valid()},
		field{"unit_price", decimal(p.UnitPrice, kernel.PriceFormat)},
		field{"price_basis_quantity", decimal(p.PriceBasisQuantity, kernel.QuantityFormat)})
}

// SalesPriceQuery asks for the sales prices in force at instant At for the SKUs SkuIDs in Currency:
// the general price of each and, when PartyID names a customer, that customer's own.
type SalesPriceQuery struct {
	SkuIDs   []string // 1–MaxRefs distinct SKU IDs
	PartyID  string   // "" for the general prices only, or the customer's party ID
	Currency string   // currency code
	At       time.Time
}

// Validate checks the form of the query.
func (q SalesPriceQuery) Validate() error {
	if len(q.SkuIDs) < 1 || len(q.SkuIDs) > MaxRefs {
		return schema("/sku_ids")
	}
	for i, id := range q.SkuIDs {
		if !kernel.ValidIdentifier(id) || slices.Index(q.SkuIDs, id) != i {
			return schema("/sku_ids/" + strconv.Itoa(i))
		}
	}
	switch {
	case q.PartyID != "" && !kernel.ValidIdentifier(q.PartyID):
		return schema("/party_id")
	case !currencyCode(q.Currency):
		return schema("/currency")
	case q.At.IsZero():
		return schema("/at")
	}
	return nil
}

// SalesPriceInForce is one sales price in force at the instant asked: Ref the exact sales_price
// revision (a ConfigRef v1 a downstream may bind and guard with ConfigGuard v1), its content hash,
// subject, unit, price and validity. PartyID is empty for the general price.
type SalesPriceInForce struct {
	Ref                ConfigRef
	ContentHash        string
	SkuID              string
	PartyID            string
	Unit               string
	UnitPrice          kernel.Decimal
	PriceBasisQuantity kernel.Decimal
	ValidFrom          time.Time
	ValidUntil         time.Time // zero: open-ended
}

// SalesPrices is the answer of a SalesPriceQuery: Currency with the minor units of its published
// definition, and the prices in force in SKU order, the general price of a SKU before the
// customer's own; a SKU without a price has none.
type SalesPrices struct {
	Currency kernel.Currency
	Prices   []SalesPriceInForce
}

// SalesPriceQueries is the lookup of the sales price list by its content.
type SalesPriceQueries interface {
	// LookupSalesPrices returns the sales prices in force at the instant asked — published at or
	// before it, not superseded or retired by it, valid at it, with their unit in force — for the
	// SKUs, the currency and the customer of the query. The currency must be configured and in
	// force (REFERENCE_INVALID, CONFIG_NOT_ACTIVE or CONFIG_EXPIRED at /currency). It needs no
	// permission beyond an active grant and is not audited: the downstream read or command that
	// asked records its own outcome. Ask before opening your own transaction; a price a transaction
	// binds is re-checked with ConfigGuard v1.
	LookupSalesPrices(ctx context.Context, q SalesPriceQuery) (SalesPrices, error)
}
