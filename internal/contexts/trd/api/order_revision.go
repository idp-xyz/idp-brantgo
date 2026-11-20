package api

import (
	"context"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// OrderRevisionRef v1 (S5R-D35, simulated sign-off E0; Context Map R27 TRD→FUL, R46 TRD→CNX, and
// R32 TRD→STL for the Incoterm of S5R-D24): any committed revision of an order, read by (order,
// revision) — the revision a shipment, its documents or a customs declaration bound, which a later
// amendment never rewrites. FUL's shipment preparation and document rendering and CNX's declarations
// read the bound revision through it rather than the current one (ActiveOrderRef v1,
// OrderCommercialSnapshot v1).
//
// A revision never changes once committed: revision 1 is the order's commercial snapshot, a later
// one the brant.order-revision/0.1.0 document order.amendment.apply wrote, each hashed to its order
// hash. The fact is read from it: the lines with their unit price, price basis quantity, discount
// ratio, net and tax amounts (and the costing cost of S5R-D33), the charges and totals, the terms
// (trade terms with the Incoterm and edition, named place, payment terms, the payment plan with its
// credit portion, delivery), the customer's addresses with their roles, sealed text and country,
// and the notify party (S5R-D40). A revision written since S5R seals the Incoterm, the edition and
// the countries; for one written before, TRD derives them from the POL trade_terms revision and the
// PTY party revision the revision binds (published and confirmed revisions never change) and sets
// Derived. The customer's addresses are the source quote revision's (no amendment changes them).
//
// Like OrderSnapshotLookups the lookup needs no permission beyond an active grant (S3-D153);
// orders of quotes outside the caller's data scope are absent, as are unknown orders and revisions
// the order does not have (yet). Ask before your own transaction: the revision is immutable, so
// nothing is re-checked inside it; whether the order still serves new work is OrderGuard v1's.
type OrderRevisionLookups interface {
	// ResolveOrderRevisions answers at most MaxOrderRefs keys (ValidateOrderRevisionKeys).
	ResolveOrderRevisions(ctx context.Context, keys []OrderRevisionKey) (map[OrderRevisionKey]OrderRevisionFact, error)
}

// OrderRevisionKey names one revision of an order: OrderRevision is 1 for the order's snapshot.
type OrderRevisionKey struct {
	OrderID       string
	OrderRevision int64
}

// ValidateOrderRevisionKeys checks a lookup's keys: at most MaxOrderRefs, identifiers and
// revisions from 1 (SCHEMA_INVALID at /keys/<i>/...).
func ValidateOrderRevisionKeys(keys []OrderRevisionKey) error {
	if len(keys) > MaxOrderRefs {
		return schema("/keys")
	}
	for i, k := range keys {
		p := index("/keys", i)
		if !kernel.ValidIdentifier(k.OrderID) {
			return schema(p + "/order_id")
		}
		if err := positive(k.OrderRevision, p+"/order_revision"); err != nil {
			return err
		}
	}
	return nil
}

// OrderRevisionFact is what OrderRevisionRef v1 says of one revision of an order: the revision's
// binding (its order hash; OrderBinding of OrderGuard v1), whether it is the order's current
// revision, the order's state now, the revision it amends (nil for revision 1) and the amendment
// that produced it ("" for revision 1), the customer and currency, the terms (OrderTerms, embedded:
// TradeTerms with the Incoterm and edition, Addresses with their countries, PaymentTerms, Stages,
// CreditPortionMinor, NotifyParty), the delivery requirement, the lines, charges and totals in minor
// units of the order currency, the legal terms revision the source quote sealed (nil when none),
// and Derived: true when the revision was written before S5R-D35 and the Incoterm, edition and
// address countries were derived from the bound POL and PTY revisions rather than read from its
// seal. Code is "" when the revision is intact and TRD could state its terms, REFERENCE_INVALID
// otherwise — then only Binding, Current and Status are set and nothing else may be relied on.
type OrderRevisionFact struct {
	Binding     OrderBinding
	Current     bool
	Status      string
	Previous    *OrderBinding
	AmendmentID string
	CustomerRef PartyRef
	CurrencyRef ConfigRef
	OrderTerms
	Delivery      Delivery
	Lines         []OrderLine
	Charges       []OrderCharge
	NetMinor      string
	TaxMinor      string
	PayableMinor  string
	LegalTermsRef *ConfigRef
	Derived       bool
	Code          string
}

// Check returns nil when the revision is intact and REFERENCE_INVALID at path otherwise. It says
// nothing of the order's state.
func (f OrderRevisionFact) Check(path string) error {
	if f.Code == "" {
		return nil
	}
	return refuse(f.Code, path)
}

// Line returns line id of the revision, if it has one.
func (f OrderRevisionFact) Line(id string) (OrderLine, bool) {
	for _, l := range f.Lines {
		if l.LineID == id {
			return l, true
		}
	}
	return OrderLine{}, false
}
