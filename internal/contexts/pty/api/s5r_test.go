package api

import (
	"strings"
	"testing"
)

// The S5R contract additions of PTY (simulated sign-off E0 of 2026-11-13): the notify address role
// (S5R-D40), service roles without a route (S5R-D36) and the permitted payers of a trading profile
// (S5R-D21).
func TestS5RPartyContracts(t *testing.T) {
	notify := SaveParty{IdempotencyKey: "k", PartyID: "c", Name: "c", Roles: []string{RoleCustomer},
		Addresses: []Address{{AddressID: "n", Role: AddressNotify, Address: "合成通知方地址", Country: "DE"}}}
	if got := code(notify.Validate()); got != "OK" || !AddressRoleKnown("notify") {
		t.Fatalf("a notify address: %s", got)
	}

	role := VerifyServiceRole{IdempotencyKey: "k", PartyID: "agency", ExpectedVersion: 2, ServiceRoleID: "qc", Role: ServiceInspectionAgency,
		ServiceCode: "final.inspection", ValidFrom: "2026-11-01T00:00:00Z", ValidUntil: "2026-12-01T00:00:00Z", EvidenceIDs: []string{"evidence"}}
	for _, c := range []struct {
		edit func(*VerifyServiceRole)
		want string
	}{
		{func(*VerifyServiceRole) {}, "OK"},
		{func(c *VerifyServiceRole) { c.Role = ServiceCustomsBroker }, "OK"},
		{func(c *VerifyServiceRole) { c.Role = ServiceInsurer }, "OK"},
		{func(c *VerifyServiceRole) { c.Origin, c.Destination = "CNNGB", "CNNGB" }, "OK"},
		{func(c *VerifyServiceRole) { c.Origin = "Ningbo" }, "SCHEMA_INVALID /args/origin"},
		{func(c *VerifyServiceRole) { c.Destination = "cnngb" }, "SCHEMA_INVALID /args/destination"},
		{func(c *VerifyServiceRole) { c.Role = ServiceForwarder }, "SCHEMA_INVALID /args/origin"},
		{func(c *VerifyServiceRole) { c.Role, c.Origin = ServiceCarrier, "CNSHA" }, "SCHEMA_INVALID /args/destination"},
		{func(c *VerifyServiceRole) { c.Role = ServiceInspection }, "SCHEMA_INVALID /args/origin"},
		{func(c *VerifyServiceRole) { c.Role = "inspection-agency" }, "SCHEMA_INVALID /args/role"},
	} {
		v := role
		c.edit(&v)
		if got := code(v.Validate()); got != c.want {
			t.Errorf("%s %q-%q: %s, want %s", v.Role, v.Origin, v.Destination, got, c.want)
		}
	}
	for kind, route := range map[string]bool{ServiceForwarder: true, ServiceCarrier: true, ServiceInsurer: false, ServiceInspectionAgency: false,
		ServiceCustomsBroker: false, ServiceBroker: true, ServiceInspection: true} {
		if !ServiceKindKnown(kind) || ServiceKindNeedsRoute(kind) != route {
			t.Errorf("%s", kind)
		}
	}

	profile := SaveTradingProfile{IdempotencyKey: "k", PartyID: "customer", PartyRevision: 1,
		PaymentTerms: TermsRef{Kind: KindPaymentTerms, ConfigID: "p", Revision: 1}, TradeTerms: TermsRef{Kind: KindTradeTerms, ConfigID: "t", Revision: 1},
		Owner: OwnerRef{ActorID: "sales"}, PermittedPayers: []string{"payer.1", "payer.2"}}
	for _, c := range []struct {
		payers []string
		want   string
	}{
		{nil, "OK"},
		{[]string{"payer.1", "payer.2"}, "OK"},
		{[]string{"payer.1", "payer.1"}, "SCHEMA_INVALID /args/permitted_payers/1"},
		{[]string{"-payer"}, "SCHEMA_INVALID /args/permitted_payers/0"},
		{strings.Split(strings.Repeat("p,", MaxPermittedPayers+1), ",")[:MaxPermittedPayers+1], "SCHEMA_INVALID /args/permitted_payers"},
	} {
		v := profile
		v.PermittedPayers = c.payers
		if got := code(v.Validate()); got != c.want {
			t.Errorf("%v: %s, want %s", c.payers, got, c.want)
		}
	}
}
