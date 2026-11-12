package order

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
)

// sealedParties is a snapshot whose source revision binds the customer's buyer and consignee
// addresses, sealed by the freeze with their roles and text (quote.parties).
func sealedParties() Snapshot {
	hq := quote.AddressRef{PartyID: "customer.1", PartyRevision: 2, AddressID: "hq"}
	dock := quote.AddressRef{PartyID: "customer.1", PartyRevision: 2, AddressID: "dock"}
	return Snapshot{Commercial: quote.Commercial{
		Data: quote.Terms{Customer: customer, Addresses: []quote.AddressRef{hq, dock}, Currency: quote.Ref{Kind: "currency", ID: "EUR", Revision: 3}},
		PartySnapshots: quote.PartySnapshots{Customer: quote.CustomerSnapshot{PartyID: "customer.1", PartyRevision: 2,
			Addresses: []quote.AddressSnapshot{{AddressID: "hq", Role: "buyer", Address: "合成买方地址"}, {AddressID: "dock", Role: "consignee",
				Address: "Synthetic consignee, Hamburg"}}}},
		Calculation: &quote.Calculation{ComparisonCurrency: quote.Ref{Kind: "currency", ID: "USD", Revision: 1},
			SalesFX: &quote.Ref{Kind: "fx_rate", ID: "eur.usd", Revision: 4}, ComparisonFXRate: "1.1"}}}
}

// R46 SalesOrderRef v1 and R27 (S5 plan unit A2: Addresses [{Role, PartyID, PartyRevision,
// AddressID}]): the customer addresses an order binds are the source revision's references, each
// with the role and text the freeze sealed, in their order (the pairing quote.RecipientOf makes).
// A party snapshot of another party or revision, of another length or out of step with the
// references is no snapshot TRD sealed.
func TestTheOrdersAddressesAreTheSealedOnes(t *testing.T) {
	got, err := sealedParties().Addresses("/x")
	if err != nil || len(got) != 2 || got[0].Ref.AddressID != "hq" || got[0].Role != "buyer" || got[1].Ref.PartyRevision != 2 ||
		got[1].Role != "consignee" || got[1].Address != "Synthetic consignee, Hamburg" {
		t.Fatalf("addresses %+v %v", got, err)
	}
	for name, spoil := range map[string]func(*Snapshot){
		"another party":    func(s *Snapshot) { s.Commercial.PartySnapshots.Customer.PartyID = "customer.2" },
		"another revision": func(s *Snapshot) { s.Commercial.PartySnapshots.Customer.PartyRevision = 1 },
		"one address short": func(s *Snapshot) {
			s.Commercial.PartySnapshots.Customer.Addresses = s.Commercial.PartySnapshots.Customer.Addresses[:1]
		},
		"out of step": func(s *Snapshot) {
			a := s.Commercial.PartySnapshots.Customer.Addresses
			a[0], a[1] = a[1], a[0]
		},
	} {
		s := sealedParties()
		spoil(&s)
		if _, err := s.Addresses("/order_ids"); code(err) != "REFERENCE_INVALID /order_ids" {
			t.Errorf("%s: %s", name, code(err))
		}
	}
}

// STL-INV-007 and baseline 4.5 (S5 plan unit A2: AnalysisFX, the order analysis rate, for C9): the
// rate is the comparison the source revision's sealed calculation made — EUR sales compared in USD
// at fx_rate eur.usd r4, 1.1 — and an order in the comparison currency names no rate. A calculation
// that states a rate without its revision, or the reverse, and a snapshot without a calculation are
// refused.
func TestTheAnalysisRateIsTheSealedComparison(t *testing.T) {
	a, err := sealedParties().Analysis("/x")
	if err != nil || a.OrderCurrency != "EUR" || a.Comparison.ID != "USD" || a.FX == nil || a.FX.ID != "eur.usd" || a.FX.Revision != 4 ||
		a.Rate != "1.1" {
		t.Fatalf("analysis %+v %v", a, err)
	}
	same := sealedParties()
	same.Commercial.Data.Currency = quote.Ref{Kind: "currency", ID: "USD", Revision: 1}
	same.Commercial.Calculation.SalesFX, same.Commercial.Calculation.ComparisonFXRate = nil, ""
	if a, err = same.Analysis("/x"); err != nil || a.FX != nil || a.Rate != "" || a.OrderCurrency != "USD" {
		t.Fatalf("one currency %+v %v", a, err)
	}
	for name, spoil := range map[string]func(*Snapshot){
		"no calculation":      func(s *Snapshot) { s.Commercial.Calculation = nil },
		"a rate, no revision": func(s *Snapshot) { s.Commercial.Calculation.SalesFX = nil },
		"a revision, no rate": func(s *Snapshot) { s.Commercial.Calculation.ComparisonFXRate = "" },
	} {
		s := sealedParties()
		spoil(&s)
		if _, err := s.Analysis("/order_ids"); code(err) != "REFERENCE_INVALID /order_ids" {
			t.Errorf("%s: %s", name, code(err))
		}
	}
}

// SM-TRD-ORDER (S5 plan unit A2: STL releases on order ends, PL-13; C9 needs "fulfilment complete
// or short-closed"): an order's end names its command — a draft's closure order.cancel or
// order.close_unfulfilled, an active order's end order.cancel, order.close_remainder or
// order.close — and a draft or active order has none.
func TestEndedNamesTheCommandThatEndedTheOrder(t *testing.T) {
	for want, o := range map[string]*Order{
		"":                               {Status: Draft},
		orderrev.CommandCancel:           {Status: Cancelled, Closure: &Closure{Status: Cancelled}},
		orderrev.CommandCloseUnfulfilled: {Status: ClosedUnfulfilled, Closure: &Closure{Status: ClosedUnfulfilled}},
		orderrev.CommandCloseRemainder:   {Status: Closed, End: &End{Ending: orderrev.Ending{Command: orderrev.CommandCloseRemainder}}},
		orderrev.CommandClose:            {Status: Closed, End: &End{Ending: orderrev.Ending{Command: orderrev.CommandClose}}},
	} {
		if got := o.Ended(); got != want {
			t.Errorf("%+v: %q, want %q", o, got, want)
		}
	}
	active := &Order{Status: Active, End: &End{Ending: orderrev.Ending{Command: orderrev.CommandCancel}}}
	if active.Ended() != orderrev.CommandCancel {
		t.Fatalf("an active order cancelled: %q", active.Ended())
	}
}
