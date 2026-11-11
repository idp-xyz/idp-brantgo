package draft

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The Python A46 fixture (tests/test_t04_a46_purchase_draft.py PurchaseDraftTests, on the A38 order
// fixture) as the rules read it: order order.1, active on review review.order, line L1 of RFQ line L1
// (sku.1 at specification revision 1, 120 PCS) with a fixed delivery on 2026-11-15 at handover in
// port.test, Asia/Shanghai; offer.1 revision 1 of supplier.1, line OL1 at 5 USD per PCS (flat), MOQ
// 100 PCS per SKU, zero tax not recoverable, the same delivery, valid until 2026-10-29; the order's
// quote adopted snapshot.1, which bought L1 under offer.1 OL1 for 600.00 USD. The clock reads
// 2026-10-22T12:00:00Z.

var (
	now     = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	pcs     = ConfigKey{Kind: "unit", ID: "PCS", Revision: 1}
	usd     = ConfigKey{Kind: "currency", ID: "USD", Revision: 1}
	cny     = ConfigKey{Kind: "currency", ID: "CNY", Revision: 1}
	zeroTax = ConfigKey{Kind: "tax_profile", ID: "tax.zero", Revision: 1}
	vat     = ConfigKey{Kind: "tax_profile", ID: "tax.vat", Revision: 1}
	goods   = ConfigKey{Kind: "tax_application", ID: "tax.goods", Revision: 1}
	trade   = ConfigKey{Kind: "trade_terms", ID: "terms.buy", Revision: 1}
	payment = ConfigKey{Kind: "payment_terms", ID: "payment.buy", Revision: 1}
	offer1  = Ref{Kind: KindSupplierOffer, ID: "offer.1", Revision: 1}
	ol1     = Source{Ref: offer1, ConditionLineID: "OL1"}
)

func inForce(string) error { return nil }

func condition(id, sku string) Condition {
	k := func(c ConfigKey) *ConfigKey { return &c }
	return Condition{ID: id, SkuID: sku, SpecRevision: 1, PurchaseUnit: k(pcs), PriceUnit: k(pcs), TierUnit: k(pcs), PriceBasisQuantity: "1",
		PriceTierScope: TierScopeSKU, PricingMode: ModeFlat, Tiers: []Tier{{Lower: "0", UnitPrice: "5"}},
		MOQ: &MOQ{Quantity: "100", Unit: k(pcs), Scope: MOQScopeSKU}, Currency: k(usd), TaxProfile: k(zeroTax), TaxApplication: k(goods),
		TradeTerms: k(trade), PaymentTerms: k(payment),
		Delivery: &SupplyDelivery{Kind: DeliveryFixed, Date: "2026-11-15", Milestone: "handover", Place: "port.test", TimeZone: "Asia/Shanghai"}}
}

func offer(ref Ref, supplier string, lines ...Condition) Supply {
	s := Supply{Ref: ref, Check: inForce, ContentHash: "hash-" + ref.ID, SupplierID: supplier, SupplierRowVersion: 2,
		Rfq: &RfqRef{RfqID: "rfq.1", RowVersion: 2}, RfqLineIDs: []string{"L1", "L2"},
		ValidFrom: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), ValidUntil: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC),
		VerifiedBy: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"}, VerifiedAt: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
		Lines: map[string]Condition{}}
	for _, c := range lines {
		s.Lines[c.ID] = c
	}
	return s
}

func line(id, rfqLine, sku, quantity string) OrderLine {
	return OrderLine{LineID: id, RfqLineID: rfqLine, SkuID: sku, SpecRevision: 1, Quantity: kernel.MustDecimal(quantity), Unit: pcs}
}

type world struct {
	in  Inputs
	req Request
}

// fixture is the Python A46 single-line fixture.
func fixture() *world {
	minor := kernel.MustDecimal("60000")
	in := Inputs{Scope: kernel.Scope{Tenant: "tenant.1", Company: "company.A"}, Now: now,
		Preparer: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "author"}, Epoch: 7,
		Order: &Order{OrderID: "order.1", OrderRevision: 1, OrderHash: "orderhash", Status: "active", QuoteID: "quote.a20", QuoteRevision: 1,
			QuoteHash: "quotehash", ReviewID: "review.order", CustomerID: "customer.1", Rfq: &RfqRef{RfqID: "rfq.1", RowVersion: 2},
			Snapshot: &SnapshotRef{SnapshotID: "snapshot.1", ResultHash: "resulthash"},
			Delivery: &RequiredDelivery{Kind: DeliveryFixed, Date: "2026-11-15", Milestone: "handover", Place: "port.test", TimeZone: "Asia/Shanghai"},
			Lines:    map[string]OrderLine{"L1": line("L1", "L1", "sku.1", "120")}},
		Supply:  map[Ref]Supply{offer1: offer(offer1, "supplier.1", condition("OL1", "sku.1"))},
		Parties: map[string]Party{"supplier.1": {Status: PartyConfirmed, RowVersion: 2, Supplier: true}},
		Specs:   map[Spec]func(string) error{{SkuID: "sku.1", Revision: 1}: inForce, {SkuID: "sku.2", Revision: 1}: inForce},
		Configs: map[ConfigKey]Config{
			pcs:     {Check: inForce, ContentHash: "h.pcs", Unit: &Unit{Code: "PCS", Precision: 0}},
			usd:     {Check: inForce, ContentHash: "h.usd", Currency: &Currency{Code: "USD", MinorUnits: 2}},
			cny:     {Check: inForce, ContentHash: "h.cny", Currency: &Currency{Code: "CNY", MinorUnits: 2}},
			zeroTax: {Check: inForce, ContentHash: "h.tax", TaxProfile: &TaxProfile{Rate: "0", PriceBasis: BasisExclusive, Recovery: Nonrecoverable}},
			vat:     {Check: inForce, ContentHash: "h.vat", TaxProfile: &TaxProfile{Rate: "0.13", PriceBasis: BasisInclusive, Recovery: Recoverable}},
			goods:   {Check: inForce, ContentHash: "h.goods"},
			trade:   {Check: inForce, ContentHash: "h.trade"},
			payment: {Check: inForce, ContentHash: "h.payment"},
		},
		Costing: &Costing{Ref: SnapshotRef{SnapshotID: "snapshot.1", ResultHash: "resulthash"}, Currency: "USD",
			Lines: map[string]CostedLine{"L1": {Sources: []Source{ol1}, PurchaseMinor: &minor}}},
		Allocations: map[allocation.Key]*allocation.Allocation{}}
	return &world{in: in, req: Request{DraftID: "purchase.a46", OrderID: "order.1",
		Lines:           []RequestLine{{OrderLineID: "L1", Quantity: kernel.MustDecimal("120"), Source: ol1}},
		SelectionReason: "Synthetic costed supplier condition"}}
}

// variant adds a verified offer whose line OL1 is edit's change of OL1 and returns its source.
func (w *world) variant(id string, edit func(c *Condition)) Source {
	c := condition("OL1", "sku.1")
	edit(&c)
	ref := Ref{Kind: KindSupplierOffer, ID: id, Revision: 1}
	w.in.Supply[ref] = offer(ref, "supplier.1", c)
	return Source{Ref: ref, ConditionLineID: "OL1"}
}

func (w *world) with(src Source) *world {
	for i := range w.req.Lines {
		w.req.Lines[i].Source = src
	}
	return w
}

func (w *world) prepare(t *testing.T) *Prepared {
	t.Helper()
	p, err := Prepare(w.req, w.in)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return p
}

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

func (w *world) refused(t *testing.T, want string) {
	t.Helper()
	_, err := Prepare(w.req, w.in)
	if got := code(err); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
