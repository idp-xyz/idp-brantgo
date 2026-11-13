package application

import (
	"context"
	"testing"
	"time"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
)

type revisionClock struct{}

func (revisionClock) Now() time.Time { return time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC) }

// polTrade answers POL's ConfigRef v1 lookup with the trade terms it holds; anything else panics.
type polTrade struct {
	polapi.Queries
	terms map[polapi.ConfigRef]polapi.TradeTerms
}

func (p polTrade) ResolveConfigs(_ context.Context, refs []polapi.ConfigRef, _ time.Time) (map[polapi.ConfigRef]polapi.ResolvedConfig, error) {
	out := map[polapi.ConfigRef]polapi.ResolvedConfig{}
	for _, r := range refs {
		if t, ok := p.terms[r]; ok {
			out[r] = polapi.ResolvedConfig{Ref: r, Status: "superseded", Data: polapi.ConfigData{TradeTerms: &t}}
		}
	}
	return out, nil
}

// ptyCountries answers PTY's AddressRoleRef v1 lookup with the countries it holds; an address it
// does not hold is absent (not shown to the caller).
type ptyCountries struct {
	ptyapi.ProfileQueries
	countries map[ptyapi.AddressRoleRef]string
}

func (p ptyCountries) ResolveAddressRoles(_ context.Context, refs []ptyapi.AddressRoleRef) (map[ptyapi.AddressRoleRef]ptyapi.ResolvedAddressRole,
	error) {
	out := map[ptyapi.AddressRoleRef]ptyapi.ResolvedAddressRole{}
	for _, r := range refs {
		if c, ok := p.countries[r]; ok {
			out[r] = ptyapi.ResolvedAddressRole{Ref: r, Role: "buyer", Country: c, Code: "REFERENCE_STALE", Stale: ptyapi.StalePartyRevised}
		}
	}
	return out, nil
}

// S5R-D35 "旧修订从绑定的 POL、PTY 修订推导并标记 derived": a revision written before S5R gets the Incoterm and
// edition of the trade_terms revision it binds — whatever its state now — and each address the
// country of the party revision it binds — stale or not, a confirmed revision never changes —,
// marked Derived; an address PTY does not show the caller has none; trade terms POL does not know
// make the fact REFERENCE_INVALID; a sealed revision is left as sealed (no POL or PTY answer needed).
func TestS5RD35ARevisionBeforeS5RIsDerivedFromItsBoundRevisions(t *testing.T) {
	fca := api.ConfigRef{Kind: "trade_terms", ID: "fca", Revision: 1}
	hq := api.OrderAddress{Role: "buyer", PartyID: "customer.1", PartyRevision: 2, AddressID: "hq"}
	ap := api.OrderAddress{Role: "billing", PartyID: "customer.1", PartyRevision: 2, AddressID: "ap"}
	s := Service{Clock: revisionClock{}, Upstream: Upstream{
		Configs:  polTrade{terms: map[polapi.ConfigRef]polapi.TradeTerms{{Kind: "trade_terms", ConfigID: "fca", Revision: 1}: {Incoterm: "FCA", Edition: "2020"}}},
		Profiles: ptyCountries{countries: map[ptyapi.AddressRoleRef]string{{PartyID: "customer.1", PartyRevision: 2, AddressID: "hq"}: "DE"}}}}
	derived := api.OrderRevisionFact{Binding: api.OrderBinding{OrderID: "order.1", OrderRevision: 1, OrderHash: "h1"}, Derived: true,
		OrderTerms: api.OrderTerms{TradeTerms: api.TradeTerms{Ref: fca}, Addresses: []api.OrderAddress{hq, ap}}}
	sealed := api.OrderRevisionFact{Binding: api.OrderBinding{OrderID: "order.1", OrderRevision: 2, OrderHash: "h2"},
		OrderTerms: api.OrderTerms{TradeTerms: api.TradeTerms{Ref: api.ConfigRef{Kind: "trade_terms", ID: "cif", Revision: 9}, Incoterm: "CIF",
			Edition: "2020"}, Addresses: []api.OrderAddress{{AddressID: "hq", Country: "FR"}}}}
	unknown := derived
	unknown.Binding.OrderID, unknown.TradeTerms.Ref.Revision = "order.2", 7
	unknown.Addresses = nil
	facts := map[api.OrderRevisionKey]api.OrderRevisionFact{{OrderID: "order.1", OrderRevision: 1}: derived,
		{OrderID: "order.1", OrderRevision: 2}: sealed, {OrderID: "order.2", OrderRevision: 1}: unknown}
	if err := s.stateRevisions(context.Background(), facts, map[api.OrderRevisionKey]revisionRead{}); err != nil {
		t.Fatal(err)
	}
	d := facts[api.OrderRevisionKey{OrderID: "order.1", OrderRevision: 1}]
	if d.Code != "" || !d.Derived || d.TradeTerms.Incoterm != "FCA" || d.TradeTerms.Edition != "2020" || d.Addresses[0].Country != "DE" ||
		d.Addresses[1].Country != "" {
		t.Fatalf("derived %+v", d)
	}
	if got := facts[api.OrderRevisionKey{OrderID: "order.1", OrderRevision: 2}]; got.Derived || got.TradeTerms.Incoterm != "CIF" ||
		got.Addresses[0].Country != "FR" {
		t.Fatalf("sealed %+v", got)
	}
	if got := facts[api.OrderRevisionKey{OrderID: "order.2", OrderRevision: 1}]; got.Check("/x") == nil || got.Binding.OrderHash != "h1" ||
		got.Lines != nil {
		t.Fatalf("unknown trade terms %+v", got)
	}
}
