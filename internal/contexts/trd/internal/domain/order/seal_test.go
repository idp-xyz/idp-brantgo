package order

import (
	"bytes"
	"encoding/json"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
)

// S5R-D35 and S5R-D40 for revision 1 (simulated sign-off E0; Python V0.50 has no seal, its
// t03_order_snapshot.build is the snapshot without one): order.prepare seals the Incoterm and
// edition of the revision's trade terms, the countries of its customer addresses and the notify
// party it binds — seal at the top of the snapshot, terms.notify_party_ref with the terms — and the
// snapshot still verifies as the copy of its source with what it sealed. Without the seal's facts
// the snapshot is the one Python builds, byte for byte (the S3 form every order before S5R has).
func TestS5RD35PrepareSealsRevisionOne(t *testing.T) {
	q, a := accepted(t)
	legacy, err := Prepare(preparation(q), q, a, scope, agent, 3, prepareOn)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(legacy.Snapshot, []byte(`"seal"`)) || bytes.Contains(legacy.Snapshot, []byte(`notify_party_ref`)) {
		t.Fatalf("the snapshot without the seal's facts: %s", legacy.Snapshot)
	}

	notify := quote.AddressRef{PartyID: "notify.1", PartyRevision: 1, AddressID: "rotterdam"}
	facts := &orderrev.SealFacts{TradeTerms: quote.Ref{Kind: "trade_terms", ID: "fob", Revision: 1}, Incoterm: "FOB", Edition: "2020",
		Countries: map[string]string{}, Notify: &orderrev.NotifyFact{Ref: notify, Role: orderrev.RoleNotify, Address: "合成通知方", Country: "NL"}}
	q, a = accepted(t)
	p := preparation(q)
	p.NotifyParty, p.Seal = &notify, facts
	o, err := Prepare(p, q, a, scope, agent, 3, prepareOn)
	if err != nil {
		t.Fatal(err)
	}
	if o.OrderHash == legacy.OrderHash || quote.Hash(o.Snapshot) != o.OrderHash {
		t.Fatalf("another snapshot: %s", o.OrderHash)
	}
	var body map[string]any
	if err := json.Unmarshal(o.Snapshot, &body); err != nil {
		t.Fatal(err)
	}
	seal, _ := body["seal"].(map[string]any)
	terms := body["terms"].(map[string]any)
	if seal["incoterm"] != "FOB" || seal["incoterm_edition"] != "2020" || len(seal["addresses"].([]any)) != 0 ||
		seal["notify_party"].(map[string]any)["country"] != "NL" || terms["notify_party_ref"].(map[string]any)["party_id"] != "notify.1" {
		t.Fatalf("the seal %v, terms %v", seal, terms)
	}
	s, err := o.Verify(scope, "/x")
	if err != nil || s.Sealing().Seal == nil || *s.Sealing().NotifyParty != notify {
		t.Fatalf("verified: %v %+v", err, s.Sealing())
	}
	if sl := s.Sealing().Seal; sl.Incoterm != "FOB" || sl.Notify.Address != "合成通知方" || len(sl.Addresses) != 0 {
		t.Fatalf("the snapshot's seal: %+v", sl)
	}
	// The seal is part of the hashed snapshot: changed and rehashed, it is no longer the copy order.prepare
	// wrote of the terms it names (FromSnapshot refuses a seal that does not fit; the hash binds the rest).
	tampered := *o
	tampered.Snapshot = bytes.Replace(o.Snapshot, []byte(`"notify_party_ref":{`), []byte(`"notify_party_ref_x":{`), 1)
	tampered.OrderHash, tampered.SnapshotHash = quote.Hash(tampered.Snapshot), quote.Hash(tampered.Snapshot)
	if _, err := tampered.Verify(scope, "/x"); code(err) != "REFERENCE_INVALID /x" {
		t.Errorf("a renamed member: %s", code(err))
	}

	for name, c := range map[string]struct {
		edit func(p *Preparation)
		want string
	}{
		"a notify party without the seal's facts": {func(p *Preparation) { p.Seal = nil }, "REFERENCE_INVALID /args/notify_party_ref"},
		"a notify party PTY did not resolve": {func(p *Preparation) {
			f := *facts
			f.Notify = nil
			p.Seal = &f
		}, "REFERENCE_INVALID /args/notify_party_ref"},
		"an address of another role": {func(p *Preparation) {
			f, n := *facts, *facts.Notify
			n.Role = "consignee"
			f.Notify = &n
			p.Seal = &f
		}, "INVALID_KNOWN_VALUE /args/notify_party_ref"},
		"no Incoterm": {func(p *Preparation) {
			f := *facts
			f.Incoterm = ""
			p.Seal = &f
		}, "REFERENCE_INVALID /args/business_revision"},
	} {
		q, a := accepted(t)
		p := preparation(q)
		p.NotifyParty, p.Seal = &notify, facts
		c.edit(&p)
		if _, err := Prepare(p, q, a, scope, agent, 3, prepareOn); code(err) != c.want || q.Slot != nil {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
}

// S5R-D33 (R32 OrderCommercialSnapshot v1, OrderRevisionRef v1): a line's costing cost is what the
// cost snapshot the source revision adopted gives its RFQ line, in the calculation currency — 60 PCS
// of L1 at 330.00 USD, 60 of L2 at 330.00 —; amended to 45 PCS, L2 costs 330.00 × 45 ÷ 60 = 247.50,
// to 7 PCS 330.00 × 7 ÷ 60 = 38.50 HALF_UP; a line no costing priced (added, or another SKU
// specification) has none, and none has one when the costing is not the one the calculation sealed
// (another currency, lines not adding up to the sealed cost_minor) or unreadable.
func TestS5RD33LineCostsAreTheSealedCosting(t *testing.T) {
	s := Snapshot{Commercial: quote.Commercial{Calculation: &quote.Calculation{CostMinor: "66000", CostScope: quote.ScopeBinding{Currency: "USD",
		Lines: []quote.LineBinding{{LineID: "L1", RfqLineID: "R1", SkuID: "sku.1", SpecRevision: 1, NormalizedQuantity: "60", CostUnit: "PCS"},
			{LineID: "L2", RfqLineID: "R2", SkuID: "sku.2", SpecRevision: 1, NormalizedQuantity: "60", CostUnit: "PCS"}}}}}}
	costed := []CostedLine{{RfqLineID: "R1", Quantity: "60", Unit: "PCS", AmountMinor: "33000"}, {RfqLineID: "R2", Quantity: "60", Unit: "PCS",
		AmountMinor: "33000"}}
	line := func(id, rfq, skuID string, q string) orderrev.Line {
		return orderrev.Line{LineID: id, RfqLineID: rfq, Sku: quote.SkuRef{SkuID: skuID, SpecRevision: 1}, NormalizedQuantity: q, CostUnit: "PCS"}
	}
	r := &orderrev.Revision{Lines: []orderrev.Line{line("L1", "R1", "sku.1", "60"), line("L2", "R2", "sku.2", "45"), line("L3", "", "sku.3", "5")}}
	got := s.LineCosts(r, "USD", costed)
	if len(got) != 2 || got["L1"] != "33000" || got["L2"] != "24750" {
		t.Fatalf("costs %v", got)
	}
	r.Lines[1].NormalizedQuantity = "7"
	if got := s.LineCosts(r, "USD", costed); got["L2"] != "3850" {
		t.Fatalf("7 of 60: %v", got)
	}
	r.Lines[1].NormalizedQuantity = "0.001"
	if got := s.LineCosts(r, "USD", costed); got["L2"] != "1" {
		t.Fatalf("0.55 rounds HALF_UP to 1: %v", got)
	}
	r.Lines[1].Sku.SpecRevision = 2
	if got := s.LineCosts(r, "USD", costed); len(got) != 1 {
		t.Fatalf("another specification: %v", got)
	}
	for name, c := range map[string]struct {
		currency string
		costed   []CostedLine
	}{
		"another currency":  {"EUR", costed},
		"another total":     {"USD", costed[:1]},
		"an unreadable one": {"USD", []CostedLine{{RfqLineID: "R1", Quantity: "60", Unit: "PCS", AmountMinor: "x"}}},
	} {
		if got := s.LineCosts(r, c.currency, c.costed); len(got) != 0 {
			t.Errorf("%s: %v", name, got)
		}
	}
	if got := (Snapshot{}).LineCosts(r, "USD", costed); len(got) != 0 {
		t.Errorf("no calculation: %v", got)
	}
}
