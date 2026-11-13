package profile

import (
	"slices"
	"testing"
)

// S5R-D21 (UL PTY-TradingProfile, R43): a trading profile lists the permitted payers of the party.
// They are content: sorted, part of the hash, and left out of it when there are none, so a revision
// saved before they existed keeps its hash. The hashes of profiles without payers were computed by
// the code of 90925f3 (the S5R base, before R2) and are pinned here.
func TestS5RPermittedPayersAreContent(t *testing.T) {
	pinned := NewContent(3, TermsRef{Kind: KindPaymentTerms, ConfigID: "payment.cad", Revision: 2},
		TermsRef{Kind: KindTradeTerms, ConfigID: "trade.cif", Revision: 1}, Owner{ActorID: "sales.1", TeamID: "trade"},
		[]ContactRef{{ContactID: "contact.b", RowVersion: 2}, {ContactID: "contact.a", RowVersion: 1}})
	empty := NewContent(1, TermsRef{Kind: KindPaymentTerms, ConfigID: "p", Revision: 1}, TermsRef{Kind: KindTradeTerms, ConfigID: "t", Revision: 1},
		Owner{ActorID: "o"}, nil)
	if pinned.Hash() != "524f32d5ba4aea5120200298a88002087640fa4814c7353893790a91ac554d82" ||
		empty.Hash() != "9e2bd4457440e070423306c32f950b752c997c42161b55c6253710bf5dbf9b68" {
		t.Fatalf("profiles without payers hash as %s and %s", pinned.Hash(), empty.Hash())
	}
	withPayers := pinned
	withPayers.PermittedPayers = []string{"payer.b", "payer.a"}
	if withPayers.Hash() == pinned.Hash() {
		t.Fatal("permitted payers are part of the content")
	}
	again := pinned
	again.PermittedPayers = []string{"payer.a", "payer.b"}
	if again.Hash() != withPayers.Hash() || !slices.Equal(withPayers.Canonical().PermittedPayers, []string{"payer.a", "payer.b"}) {
		t.Fatal("permitted payers are sorted")
	}
	none := pinned
	none.PermittedPayers = []string{}
	if none.Hash() != pinned.Hash() || none.Canonical().PermittedPayers != nil {
		t.Fatal("no payers are none")
	}
	r := New("customer")
	rev, _ := r.Save(withPayers, editor, 1, at)
	if !slices.Equal(rev.Content.PermittedPayers, []string{"payer.a", "payer.b"}) || rev.Hash != withPayers.Hash() {
		t.Fatalf("saved %+v", rev.Content)
	}
}

// S5R-D21: a permitted payer is another party, confirmed, that the actor sees; it never makes a
// published revision stale.
func TestS5RPermittedPayersAreConfirmedOtherParties(t *testing.T) {
	party := PartyFact{Visible: true, Confirmed: true, Revision: 1}
	payers := map[string]PayerFact{"payer": {Confirmed: true}, "draft": {}}
	with := func(ids ...string) Content {
		c := content()
		c.PermittedPayers = ids
		return c
	}
	for _, c := range []struct {
		name    string
		content Content
		want    string
	}{
		{"none", with(), "OK"},
		{"a confirmed payer", with("payer"), "OK"},
		{"the party itself", with("payer", "customer"), "INVALID_KNOWN_VALUE /args/permitted_payers/1"},
		{"a draft payer", with("draft"), "REFERENCE_INVALID /args/permitted_payers/0"},
		{"a payer the actor does not see", with("payer", "hidden"), "REFERENCE_INVALID /args/permitted_payers/1"},
	} {
		if got := code(CheckReferences("customer", c.content, Facts{Party: party, Payers: payers})); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	r := New("customer")
	rev, _ := r.Save(with("payer"), editor, 1, at)
	r.Publish(reviewer, 1, []string{"evidence"}, at)
	if got := Usability(rev, party, nil); got != Usable {
		t.Fatalf("a published revision with payers: %q", got)
	}
}
