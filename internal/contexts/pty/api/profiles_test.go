package api

import (
	"strings"
	"testing"
)

func TestLocationsAreExactUNLOCODEs(t *testing.T) {
	for s, want := range map[string]bool{"CNSHA": true, "DEHAM": true, "US2NY": true, "cnsha": false, "CN-SH": false, "CNSH": false,
		"CNSHA1": false, "C1SHA": false, "CNSH1": false, "": false} {
		if Location(s) != want {
			t.Errorf("%q: %v", s, !want)
		}
	}
}

func TestPartyAddressContract(t *testing.T) {
	ok := SaveParty{IdempotencyKey: "k", PartyID: "c", Name: "c", Roles: []string{RoleCustomer},
		Addresses: []Address{{AddressID: "hq", Role: AddressBuyer, Address: "合成地址"}, {AddressID: "ap", Role: AddressBilling, Address: "x"}}}
	with := func(a ...Address) SaveParty { c := ok; c.Addresses = a; return c }
	for _, c := range []struct {
		cmd  SaveParty
		want string
	}{
		{ok, "OK"},
		{with(), "OK"},
		{with(Address{AddressID: "hq", Role: "warehouse", Address: "x"}), "SCHEMA_INVALID /args/addresses/0/role"},
		{with(Address{AddressID: "-x", Role: AddressPayer, Address: "x"}), "SCHEMA_INVALID /args/addresses/0/address_id"},
		{with(Address{AddressID: "hq", Role: AddressPayer, Address: " \t"}), "SCHEMA_INVALID /args/addresses/0/address"},
		{with(Address{AddressID: "hq", Role: AddressPayer, Address: strings.Repeat("地", 501)}), "SCHEMA_INVALID /args/addresses/0/address"},
		{with(Address{AddressID: "hq", Role: AddressPayer, Address: "x"}, Address{AddressID: "hq", Role: AddressPayee, Address: "y"}),
			"INVALID_KNOWN_VALUE /args/addresses/1/address_id"},
		{with(make([]Address, MaxAddresses+1)...), "SCHEMA_INVALID /args/addresses"},
	} {
		if got := code(c.cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", c.cmd.Addresses, got, c.want)
		}
	}
}

func TestServiceRoleContract(t *testing.T) {
	ok := VerifyServiceRole{IdempotencyKey: "k", PartyID: "forwarder", ExpectedVersion: 2, ServiceRoleID: "sea", Role: ServiceForwarder,
		ServiceCode: "sea.fcl", Origin: "CNSHA", Destination: "DEHAM", ValidFrom: "2026-11-01T00:00:00Z", ValidUntil: "2026-12-01T00:00:00Z",
		EvidenceIDs: []string{"evidence"}}
	for _, c := range []struct {
		edit func(*VerifyServiceRole)
		want string
	}{
		{func(*VerifyServiceRole) {}, "OK"},
		{func(c *VerifyServiceRole) { c.ExpectedVersion = 0 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *VerifyServiceRole) { c.Role = "customer" }, "SCHEMA_INVALID /args/role"},
		{func(c *VerifyServiceRole) { c.Origin = "Shanghai" }, "SCHEMA_INVALID /args/origin"},
		{func(c *VerifyServiceRole) { c.Destination = "" }, "SCHEMA_INVALID /args/destination"},
		{func(c *VerifyServiceRole) { c.ValidUntil = "2026-12-01" }, "SCHEMA_INVALID /args/valid_until"},
		{func(c *VerifyServiceRole) { c.ValidUntil = c.ValidFrom }, "INVALID_KNOWN_VALUE /args/valid_until"},
		{func(c *VerifyServiceRole) { c.EvidenceIDs = nil }, "SCHEMA_INVALID /args/evidence_ids"},
	} {
		cmd := ok
		c.edit(&cmd)
		if got := code(cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", cmd, got, c.want)
		}
	}
}

func TestTradingProfileContracts(t *testing.T) {
	ok := SaveTradingProfile{IdempotencyKey: "k", PartyID: "customer", PartyRevision: 1,
		PaymentTerms: TermsRef{Kind: KindPaymentTerms, ConfigID: "payment", Revision: 1}, TradeTerms: TermsRef{Kind: KindTradeTerms, ConfigID: "FCA", Revision: 1},
		Owner: OwnerRef{ActorID: "sales.owner"}, ContactRefs: []PinnedContact{{ContactID: "contact", RowVersion: 2}}}
	for _, c := range []struct {
		edit func(*SaveTradingProfile)
		want string
	}{
		{func(*SaveTradingProfile) {}, "OK"},
		{func(c *SaveTradingProfile) { c.ContactRefs = nil }, "OK"},
		{func(c *SaveTradingProfile) { c.PartyRevision = 0 }, "SCHEMA_INVALID /args/party_revision"},
		{func(c *SaveTradingProfile) { c.PaymentTerms.Kind = KindTradeTerms }, "SCHEMA_INVALID /args/payment_terms/kind"},
		{func(c *SaveTradingProfile) { c.TradeTerms.ConfigID = strings.Repeat("x", 41) }, "SCHEMA_INVALID /args/trade_terms/id"},
		{func(c *SaveTradingProfile) { c.TradeTerms.Revision = 0 }, "SCHEMA_INVALID /args/trade_terms/revision"},
		{func(c *SaveTradingProfile) { c.Owner.ActorID = "" }, "SCHEMA_INVALID /args/owner/actor_id"},
		{func(c *SaveTradingProfile) { c.Owner.TeamID = "a team" }, "SCHEMA_INVALID /args/owner/team_id"},
		{func(c *SaveTradingProfile) { c.ContactRefs[0].RowVersion = 0 }, "SCHEMA_INVALID /args/contact_refs/0/row_version"},
		{func(c *SaveTradingProfile) {
			c.ContactRefs = []PinnedContact{{ContactID: "a", RowVersion: 2}, {ContactID: "a", RowVersion: 3}}
		}, "SCHEMA_INVALID /args/contact_refs"},
		{func(c *SaveTradingProfile) { c.ContactRefs = make([]PinnedContact, MaxContactRefs+1) }, "SCHEMA_INVALID /args/contact_refs"},
	} {
		cmd := ok
		cmd.ContactRefs = append([]PinnedContact(nil), ok.ContactRefs...)
		c.edit(&cmd)
		if got := code(cmd.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", cmd, got, c.want)
		}
	}
	publish := PublishTradingProfile{IdempotencyKey: "k", PartyID: "customer", ExpectedVersion: 1, Revision: 1, EvidenceIDs: []string{"e"}, Reason: "核实"}
	if code(publish.Validate()) != "OK" {
		t.Fatal(publish.Validate())
	}
	publish.Reason = ""
	if got := code(publish.Validate()); got != "SCHEMA_INVALID /args/reason" {
		t.Fatal(got)
	}
	if got := code(ReviseContact{IdempotencyKey: "k", ContactID: "c", ExpectedVersion: 2}.Validate()); got != "SCHEMA_INVALID /args/reason" {
		t.Fatal(got)
	}
}

func TestStaleReferencesAreConflicts(t *testing.T) {
	r := ResolvedTradingProfile{Code: CodeReferenceStale, Stale: StaleContactChanged}
	if got := code(r.Check("/args/trading_profile")); got != "REFERENCE_STALE /args/trading_profile" {
		t.Fatal(got)
	}
	if c, _ := Category(CodeReferenceStale); c != "conflict" {
		t.Fatal(c)
	}
	if (ResolvedAddressRole{}).Check("/x") != nil || ValidateLookup(MaxRefs+1) == nil {
		t.Fatal("lookup checks")
	}
}
