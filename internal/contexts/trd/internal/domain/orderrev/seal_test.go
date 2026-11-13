package orderrev

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
)

// S5R-D35 and S5R-D40 (simulated sign-off E0; tactical model TRD-AGG-SalesOrder notes, UL
// TRD-SalesOrder; WBS S5R-03 acceptance "旧修订哈希不变"). Python V0.50 has no order revision seal: the
// vectors come from the design.

// brc001R2SealedHash is the order hash of testdata/brc001-r2-sealed.json, BRC-001's revision 2 as
// order.amendment.apply writes it since S5R; Python's json.dumps(sort_keys=True, separators=(",",
// ":"), ensure_ascii=False) of the parsed file reproduces its bytes (scratchpad check, python3 -I).
const brc001R2SealedHash = "47adf01158feb82881ba48d2c69c4e6d5c243b810f35c60a74718c9ee5b185cb"

// notifyRef is the notify party of the fixture: an address of role notify of another party.
var notifyRef = quote.AddressRef{PartyID: "notify.1", PartyRevision: 1, AddressID: "rotterdam"}

// sealFacts are what POL and PTY say of the fixture: FCA Incoterms 2020, the buyer address hq in
// DE, and the notify party's address in NL.
func sealFacts(notify bool) *SealFacts {
	f := &SealFacts{TradeTerms: trade, Incoterm: "FCA", Edition: "2020", Countries: map[string]string{"hq": "DE"}}
	if notify {
		f.Notify = &NotifyFact{Ref: notifyRef, Role: RoleNotify, Address: "Synthetic notify party\nRotterdam", Country: "NL"}
	}
	return f
}

func sealedFacts(notify bool) Facts {
	f := facts()
	f.Seal = sealFacts(notify)
	return f
}

// S5R-D35 "旧修订哈希不变": a revision written before S5R has no seal and no notify party; it parses
// back to its bytes and order hash (the S3 golden vector brc001-r2.json, unchanged), and Next without
// the seal's facts still writes exactly it. A revision written now seals the Incoterm, the edition
// and the countries of the customer's addresses (golden vector brc001-r2-sealed.json): it is
// another document with another order hash, which Parse reads back with its seal.
func TestS5RD35NewRevisionsSealTheIncotermAndTheCountries(t *testing.T) {
	r1 := revisionOne(t)
	if r1.Seal != nil || r1.Terms.NotifyParty != nil {
		t.Fatalf("a snapshot written before S5R seals nothing: %+v", r1.Seal)
	}
	stored, err := os.ReadFile(filepath.Join("testdata", "brc001-r2.json"))
	if err != nil {
		t.Fatal(err)
	}
	old, err := Parse(stored, brc001R2Hash)
	if err != nil || old.Seal != nil || old.Terms.NotifyParty != nil || old.Hash != brc001R2Hash || !bytes.Equal(old.Document, stored) {
		t.Fatalf("the revision written before S5R: %v %+v", err, old)
	}
	if bytes.Contains(stored, []byte(`"seal"`)) || bytes.Contains(stored, []byte(`notify_party_ref`)) {
		t.Fatal("the stored revision has no seal member")
	}

	p := ProposalOf(r1)
	p.Lines[3].Quantity = "44"
	legacy, _, err := Next(r1, amendment1, p, shipped("L4", "40"), facts(), "/args/proposal")
	if err != nil || legacy.Hash != brc001R2Hash {
		t.Fatalf("without the seal's facts Next writes the S3 revision: %v", err)
	}
	r2, d, err := Next(r1, amendment1, p, shipped("L4", "40"), sealedFacts(false), "/args/proposal")
	if err != nil {
		t.Fatal(err)
	}
	want := &Seal{Incoterm: "FCA", Edition: "2020", Addresses: []SealedCountry{{AddressID: "hq", Country: "DE"}}}
	if r2.Seal == nil || r2.Seal.Incoterm != want.Incoterm || r2.Seal.Edition != want.Edition || !slices.Equal(r2.Seal.Addresses, want.Addresses) ||
		r2.Seal.Notify != nil || r2.Hash == brc001R2Hash || r2.Amounts.Net != legacy.Amounts.Net || len(d.Terms) != 0 {
		t.Fatalf("sealed r2 %+v %+v", r2.Seal, d)
	}
	if country, ok := r2.Seal.Country("hq"); !ok || country != "DE" {
		t.Fatalf("country of hq: %q %v", country, ok)
	}
	golden(t, "brc001-r2-sealed.json", r2.Document)
	if r2.Hash != brc001R2SealedHash {
		t.Fatalf("sealed r2 hash %s, want %s", r2.Hash, brc001R2SealedHash)
	}
	if !bytes.Contains(r2.Document, []byte(`"seal":{"addresses":[{"address_id":"hq","country":"DE"}],"incoterm":"FCA","incoterm_edition":"2020"}`)) {
		t.Fatalf("the seal member: %s", r2.Document)
	}
	back, err := Parse(r2.Document, r2.Hash)
	if err != nil || back.Seal == nil || back.Seal.Incoterm != "FCA" || !slices.Equal(back.Seal.Addresses, want.Addresses) {
		t.Fatalf("parsed: %v %+v", err, back)
	}
	// The chain goes on from a sealed revision; a country PTY does not state is sealed as unknown.
	f := sealedFacts(false)
	f.Seal.Countries = map[string]string{"hq": ""}
	r3, _, err := Next(back, AmendmentRef{ID: "amendment.2", Hash: hashOf("amendment.2")}, ProposalOf(back), shipped(), f, "/args/proposal")
	if err != nil || r3.Seal.Addresses[0] != (SealedCountry{AddressID: "hq"}) || bytes.Contains(r3.Document, []byte(`"country":""`)) {
		t.Fatalf("r3 %v %+v", err, r3)
	}
}

// A fact the seal needs and does not find is refused, never sealed as a neutral value: the Incoterm
// of the proposal's trade terms (POL's answer for another revision does not do) and the country of
// every customer address the order binds.
func TestS5RD35TheSealNeedsItsFacts(t *testing.T) {
	r1 := revisionOne(t)
	for name, c := range map[string]struct {
		edit func(f *SealFacts)
		want string
	}{
		"no Incoterm":           {func(f *SealFacts) { f.Incoterm = "" }, "REFERENCE_INVALID at /args/proposal/terms/trade_terms_ref"},
		"other trade terms":     {func(f *SealFacts) { f.TradeTerms.Revision = 2 }, "REFERENCE_INVALID at /args/proposal/terms/trade_terms_ref"},
		"an address's country":  {func(f *SealFacts) { f.Countries = map[string]string{} }, "REFERENCE_INVALID at /facts/seal/addresses"},
		"another notify party":  {func(f *SealFacts) { f.Notify = &NotifyFact{Ref: hq, Role: RoleNotify} }, "OK"},
		"no notify party named": {func(f *SealFacts) {}, "OK"},
	} {
		f := sealedFacts(false)
		c.edit(f.Seal)
		if _, _, err := Next(r1, amendment1, ProposalOf(r1), shipped(), f, "/args/proposal"); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
}

// S5R-D40: the notify party is a term an amendment binds, changes or drops (terms.notify_party_ref,
// an AddressRoleRef v1 of PTY's role notify), and the revision seals its address and country. A
// notify party without its address, of another address, of another role or malformed is refused;
// the diff names it; restating the revision keeps it.
func TestS5RD40TheNotifyPartyIsATermTheRevisionSeals(t *testing.T) {
	r1 := revisionOne(t)
	p := ProposalOf(r1)
	n := notifyRef
	p.Terms.NotifyParty = &n
	r2, d, err := Next(r1, amendment1, p, shipped(), sealedFacts(true), "/args/proposal")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Terms.NotifyParty == nil || *r2.Terms.NotifyParty != notifyRef || r2.Seal.Notify == nil ||
		*r2.Seal.Notify != (SealedAddress{Address: "Synthetic notify party\nRotterdam", Country: "NL"}) ||
		!slices.Equal(d.Terms, []string{"notify_party_ref"}) || d.Empty() || len(d.Lines) != 0 {
		t.Fatalf("bound: %+v %+v %+v", r2.Terms.NotifyParty, r2.Seal.Notify, d)
	}
	if !bytes.Contains(r2.Document, []byte(`"notify_party_ref":{"address_id":"rotterdam","party_id":"notify.1","party_revision":1}`)) ||
		!bytes.Contains(r2.Document, []byte(`"notify_party":{"address":"Synthetic notify party\nRotterdam","country":"NL"}`)) {
		t.Fatalf("document: %s", r2.Document)
	}
	back, err := Parse(r2.Document, r2.Hash)
	if err != nil || *back.Terms.NotifyParty != notifyRef || back.Seal.Notify.Country != "NL" {
		t.Fatalf("parsed %v %+v", err, back)
	}
	// Restated, the proposal keeps it (a copy, not the revision's own reference); dropped, the diff
	// says so.
	again := ProposalOf(back)
	if again.Terms.NotifyParty == back.Terms.NotifyParty || *again.Terms.NotifyParty != notifyRef {
		t.Fatalf("restated %+v", again.Terms.NotifyParty)
	}
	if _, d, err := Next(back, amendment1, again, shipped(), sealedFacts(true), "/args/proposal"); err != nil || !d.Empty() {
		t.Fatalf("restated: %v %+v", err, d)
	}
	again.Terms.NotifyParty = nil
	r3, d, err := Next(back, amendment1, again, shipped(), sealedFacts(false), "/args/proposal")
	if err != nil || r3.Terms.NotifyParty != nil || r3.Seal.Notify != nil || !slices.Equal(d.Terms, []string{"notify_party_ref"}) {
		t.Fatalf("dropped: %v %+v", err, d)
	}

	for name, c := range map[string]struct {
		ref   quote.AddressRef
		facts func() Facts
		want  string
	}{
		"without the seal's facts": {notifyRef, facts, "REFERENCE_INVALID at /args/proposal/terms/notify_party_ref"},
		"without its address":      {notifyRef, func() Facts { return sealedFacts(false) }, "REFERENCE_INVALID at /args/proposal/terms/notify_party_ref"},
		"of another address": {quote.AddressRef{PartyID: "notify.1", PartyRevision: 2, AddressID: "rotterdam"},
			func() Facts { return sealedFacts(true) }, "REFERENCE_INVALID at /args/proposal/terms/notify_party_ref"},
		"of another role": {notifyRef, func() Facts {
			f := sealedFacts(true)
			f.Seal.Notify.Role = "consignee"
			return f
		}, "INVALID_KNOWN_VALUE at /args/proposal/terms/notify_party_ref"},
		"malformed": {quote.AddressRef{PartyID: "notify.1", AddressID: "rotterdam"}, func() Facts { return sealedFacts(true) },
			"SCHEMA_INVALID at /args/proposal/terms/notify_party_ref"},
	} {
		p := ProposalOf(r1)
		ref := c.ref
		p.Terms.NotifyParty = &ref
		if _, _, err := Next(r1, amendment1, p, shipped(), c.facts(), "/args/proposal"); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
}

// A stored revision whose seal does not fit what it binds is not one Next wrote (REFERENCE_INVALID
// at /revision): a notify party it names and does not seal, a seal of other addresses, a seal
// without an Incoterm, a sealed notify party its terms do not name.
func TestS5RD35ParseRefusesASealThatDoesNotFit(t *testing.T) {
	r1 := revisionOne(t)
	p := ProposalOf(r1)
	n := notifyRef
	p.Terms.NotifyParty = &n
	r2, _, err := Next(r1, amendment1, p, shipped(), sealedFacts(true), "/args/proposal")
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(r *Revision){
		"a notify party it does not seal": func(r *Revision) { r.Seal = nil },
		"other addresses":                 func(r *Revision) { r.Seal.Addresses = []SealedCountry{{AddressID: "dock"}} },
		"no Incoterm":                     func(r *Revision) { r.Seal.Incoterm = "" },
		"a notify party it does not name": func(r *Revision) { r.Terms.NotifyParty = nil },
	} {
		r := *r2
		seal := *r2.Seal
		r.Seal = &seal
		terms := r2.Terms
		r.Terms = terms
		edit(&r)
		doc, hash, err := encode(&r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(doc, hash); code(err) != "REFERENCE_INVALID at /revision" {
			t.Errorf("%s: %s", name, code(err))
		}
	}
}

// Revision 1 reads the seal and the notify party order.prepare wrote into the snapshot (package
// order's Sealing): seal at the top, terms.notify_party_ref with the terms. One whose seal does not
// fit is REFERENCE_INVALID at /snapshot/seal.
func TestS5RD35RevisionOneReadsTheSnapshotsSeal(t *testing.T) {
	doc, _ := snapshot(t, sealed(t))
	edit := func(f func(m map[string]any)) ([]byte, string) {
		tree, _ := quote.Tree(doc)
		f(tree.(map[string]any))
		b, err := quote.Canonical(tree)
		if err != nil {
			t.Fatal(err)
		}
		return b, quote.Hash(b)
	}
	seal := func(m map[string]any) {
		m["seal"] = map[string]any{"incoterm": "FCA", "incoterm_edition": "2020", "addresses": []any{map[string]any{"address_id": "hq", "country": "DE"}},
			"notify_party": map[string]any{"address": "Synthetic notify party", "country": "NL"}}
		m["terms"].(map[string]any)["notify_party_ref"] = map[string]any{"party_id": "notify.1", "party_revision": 1, "address_id": "rotterdam"}
	}
	b, h := edit(seal)
	r, err := FromSnapshot(b, h)
	if err != nil || r.Seal == nil || r.Seal.Incoterm != "FCA" || r.Seal.Notify.Country != "NL" || *r.Terms.NotifyParty != notifyRef {
		t.Fatalf("revision 1 with a seal: %v %+v", err, r)
	}
	b, h = edit(func(m map[string]any) {
		seal(m)
		delete(m, "seal")
	})
	if _, err := FromSnapshot(b, h); code(err) != "REFERENCE_INVALID at /snapshot/seal" {
		t.Errorf("a notify party without the seal: %s", code(err))
	}
	b, h = edit(func(m map[string]any) {
		seal(m)
		m["seal"].(map[string]any)["addresses"] = []any{}
	})
	if _, err := FromSnapshot(b, h); !strings.HasPrefix(code(err), "REFERENCE_INVALID at /snapshot/seal") {
		t.Errorf("a seal of no address: %s", code(err))
	}
}
