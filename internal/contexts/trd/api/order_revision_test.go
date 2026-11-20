package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// S5R-D35, S5R-D40 contracts: OrderRevisionRef v1's keys, the optional notify party of order.prepare
// and of an amendment's terms, and their request hashes — the new members are left out when absent,
// so a command stored before S5R hashes as it did and replays (action.RequestHash marshals the
// command).
func TestS5RD35OrderRevisionRefAndNotifyPartyContracts(t *testing.T) {
	keys := []OrderRevisionKey{{OrderID: "order.1", OrderRevision: 1}, {OrderID: "order.1", OrderRevision: 2}}
	if err := ValidateOrderRevisionKeys(keys); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		keys []OrderRevisionKey
		want string
	}{
		"revision 0":   {[]OrderRevisionKey{{OrderID: "order.1"}}, "SCHEMA_INVALID /keys/0/order_revision"},
		"no order":     {[]OrderRevisionKey{{OrderRevision: 1}}, "SCHEMA_INVALID /keys/0/order_id"},
		"too many":     {make([]OrderRevisionKey, MaxOrderRefs+1), "SCHEMA_INVALID /keys"},
		"a second bad": {[]OrderRevisionKey{keys[0], {OrderID: "order 1", OrderRevision: 1}}, "SCHEMA_INVALID /keys/1/order_id"},
	} {
		if got := pathOf(ValidateOrderRevisionKeys(c.keys)); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	f := OrderRevisionFact{Lines: []OrderLine{{LineID: "L1", CostMinor: "33000"}}}
	if f.Check("/x") != nil || (OrderRevisionFact{Code: "REFERENCE_INVALID"}).Check("/x") == nil {
		t.Fatal("Check")
	}
	if l, ok := f.Line("L1"); !ok || l.CostMinor != "33000" {
		t.Fatalf("Line %+v", l)
	}
	if _, ok := f.Line("L2"); ok {
		t.Fatal("no L2")
	}

	prepare := PrepareOrder{IdempotencyKey: "k", OrderID: "order.1", QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: strings.Repeat("a", 64),
		AcceptanceID: "acceptance.1"}
	b, err := json.Marshal(prepare)
	if err != nil || strings.Contains(string(b), "Notify") {
		t.Fatalf("an order.prepare without a notify party hashes as before S5R: %s", b)
	}
	if err := prepare.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		ref  AddressRoleRef
		want string
	}{
		"well formed":     {AddressRoleRef{PartyID: "notify.1", PartyRevision: 1, AddressID: "rotterdam"}, "OK"},
		"no revision":     {AddressRoleRef{PartyID: "notify.1", AddressID: "rotterdam"}, "SCHEMA_INVALID /args/notify_party_ref/party_revision"},
		"no party":        {AddressRoleRef{PartyRevision: 1, AddressID: "rotterdam"}, "SCHEMA_INVALID /args/notify_party_ref/party_id"},
		"a blank address": {AddressRoleRef{PartyID: "notify.1", PartyRevision: 1, AddressID: " "}, "SCHEMA_INVALID /args/notify_party_ref/address_id"},
	} {
		p := prepare
		ref := c.ref
		p.NotifyPartyRef = &ref
		if got := pathOf(p.Validate()); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	terms := AmendmentTerms{TradeTermsRef: ConfigRef{Kind: "trade_terms", ID: "fca", Revision: 1}}
	if b, err := json.Marshal(terms); err != nil || strings.Contains(string(b), "Notify") {
		t.Fatalf("amendment terms without a notify party hash as before S5R: %s", b)
	}
}
