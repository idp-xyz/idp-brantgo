package rfq

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	agent = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.q01"}
	clerk = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "alice"}
)

func fl(field, locator, quote string) FieldLocator {
	return FieldLocator{Field: field, Locator: locator, Quote: quote}
}

// candidates is a draft an AI agent saved from a file extract (WBS S4-04b; AN01): L1 located and
// complete; L2's style fits two SKUs and two places and its unit is missing; L3's quantity is a
// contradiction in the source (BC-AT05, CT03: matrix 120, total 118).
func candidates() *Rfq {
	r := New("rfq.1", "agent.q01", "")
	r.ReplaceDraft(Content{CustomerID: "customer.1", ContactID: "contact.1", SourceIDs: []string{"src.1"}, Lines: []Line{
		{ID: "L1", SkuID: "sku.1", Quantity: "24", UOM: "PCS", RequestedItem: "ST-100 红 S", SourceID: "src.1", Locator: "询价单!A2",
			Locators: []FieldLocator{fl("requested_item", "询价单!A2", "ST-100 红 S"), fl("quantity", "询价单!B2", "24")}},
		{ID: "L2", SkuID: "sku.1", Quantity: "36", RequestedItem: "ST-200", SourceID: "src.1", Locator: "询价单!A3",
			Locators: []FieldLocator{fl("requested_item", "询价单!A3", "ST-200"), fl("requested_item", "询价单!A7", "ST-200")},
			Pending: []PendingField{{Field: "requested_item", Reason: "ambiguous"}, {Field: "sku_id", Reason: "ambiguous", Candidates: []string{"sku.1", "sku.2"}},
				{Field: "uom", Reason: "missing"}}},
		{ID: "L3", SkuID: "sku.1", Quantity: "60", UOM: "PCS", RequestedItem: "ST-300", SourceID: "src.1", Locator: "询价单!A4",
			Pending: []PendingField{{Field: "quantity", Reason: "conflict", Candidates: []string{"60", "58"}}}},
	}}, agent)
	return r
}

func describe(cs []Correction) string {
	var out []string
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%d %s %s.%s %s %q>%q @%s %s%v by=%s", c.RowVersion, c.Command, c.LineID, c.Field, c.Kind, c.From, c.To,
			c.Locator, c.Reason, c.Candidates, c.CorrectedBy.ID))
	}
	return strings.Join(out, "\n")
}

// A line keeps the actor who proposed it while its content stays; a human's change, settlement,
// relocation or removal of a line the AI agent proposed is recorded as a correction (BC-AT27
// 逐行修正), an AI agent's own changes are not.
func TestHumanSavesOverCandidatesRecordCorrections(t *testing.T) {
	r := candidates()
	for _, l := range r.Lines {
		if l.ProposedBy != agent {
			t.Fatalf("%s proposed by %v", l.ID, l.ProposedBy)
		}
	}
	d := Content{CustomerID: "customer.1", ContactID: "contact.1", SourceIDs: []string{"src.1"}, Lines: append([]Line(nil), r.Lines...)}
	d.Lines[1].Quantity = "35"                                                     // changed
	d.Lines[1].Pending = d.Lines[1].Pending[1:]                                    // requested_item settled
	d.Lines[1].Locators = []FieldLocator{fl("requested_item", "询价单!A3", "ST-200")} // and placed once
	d.Lines = d.Lines[:2]                                                          // L3 removed
	got := r.ReplaceDraft(d, clerk)
	want := "2 rfq.save_draft L2.requested_item relocated \"ST-200\">\"ST-200\" @询价单!A3 ambiguous[] by=alice\n" +
		"2 rfq.save_draft L2.quantity changed \"36\">\"35\" @ [] by=alice\n" +
		"2 rfq.save_draft L3. removed \"\">\"\" @ [] by=alice"
	if describe(got) != want {
		t.Fatalf("corrections\n%s\nwant\n%s", describe(got), want)
	}
	if r.Lines[0].ProposedBy != agent || r.Lines[1].ProposedBy != clerk || r.RowVersion != 2 {
		t.Fatalf("provenance %v %v", r.Lines[0].ProposedBy, r.Lines[1].ProposedBy)
	}
	// The agent saves again: its changes are proposals, not corrections; L2 is the agent's again.
	d.Lines[1].Quantity = "36"
	if got := r.ReplaceDraft(d, agent); len(got) != 0 || r.Lines[1].ProposedBy != agent || r.Lines[0].ProposedBy != agent {
		t.Fatalf("%s", describe(got))
	}
	// A human re-saving the same content changes nothing about who proposed it.
	if got := r.ReplaceDraft(d, clerk); len(got) != 0 || r.Lines[1].ProposedBy != agent {
		t.Fatalf("%s", describe(got))
	}
}

// A draft with candidate lines is confirmed line by line (WBS S4-04b): every line verified
// (DRAFT_NOT_READY), a verification names a draft line (REFERENCE_INVALID) and one of the field's
// locators (REFERENCE_INVALID), a field with several places names the place verified
// (SOURCE_AMBIGUOUS), a verified quantity is positive (INVALID_KNOWN_VALUE); no field may stay
// pending: SOURCE_AMBIGUOUS for an ambiguous one, DRAFT_NOT_READY for a missing unit (AN01) or a
// contradiction (BC-AT05, CT03: never smoothed over).
func TestCandidatesAreConfirmedLineByLine(t *testing.T) {
	allLines := func(extra ...Verification) []Verification {
		v := []Verification{{LineID: "L1"}, {LineID: "L2"}, {LineID: "L3"}}
		for _, e := range extra {
			for i := range v {
				if v[i].LineID == e.LineID {
					v[i] = e
				}
			}
		}
		return v
	}
	l2 := func(fields ...VerifiedField) Verification { return Verification{LineID: "L2", Fields: fields} }
	settled := l2(VerifiedField{Field: "requested_item", Value: "ST-200", Locator: "询价单!A3"}, VerifiedField{Field: "sku_id", Value: "sku.1"},
		VerifiedField{Field: "uom", Value: "PCS"})
	l3 := Verification{LineID: "L3", Fields: []VerifiedField{{Field: "quantity", Value: "60"}}}
	for _, c := range []struct {
		v    []Verification
		want string
	}{
		{nil, "DRAFT_NOT_READY /args/verified_lines"},
		{[]Verification{{LineID: "L1"}, {LineID: "L2"}}, "DRAFT_NOT_READY /args/verified_lines"},
		{append(allLines(), Verification{LineID: "L9"}), "REFERENCE_INVALID /args/verified_lines/3/line_id"},
		{allLines(l2(VerifiedField{Field: "requested_item", Value: "ST-200"})), "SOURCE_AMBIGUOUS /args/verified_lines/1/fields/0/locator"},
		{allLines(l2(VerifiedField{Field: "requested_item", Value: "ST-200", Locator: "询价单!A9"})), "REFERENCE_INVALID /args/verified_lines/1/fields/0/locator"},
		{allLines(l2(VerifiedField{Field: "quantity", Value: "0"})), "INVALID_KNOWN_VALUE /args/verified_lines/1/fields/0/value"},
		{allLines(), "SOURCE_AMBIGUOUS /args/lines/1/requested_item"},
		{allLines(l2(VerifiedField{Field: "requested_item", Value: "ST-200", Locator: "询价单!A3"})), "SOURCE_AMBIGUOUS /args/lines/1/sku_id"},
		{allLines(l2(VerifiedField{Field: "requested_item", Value: "ST-200", Locator: "询价单!A3"}, VerifiedField{Field: "sku_id", Value: "sku.1"})),
			"DRAFT_NOT_READY /args/lines/1/uom"},
		{allLines(settled), "DRAFT_NOT_READY /args/lines/2/quantity"},
	} {
		r := candidates()
		if _, err := r.Confirm([]string{"src.1"}, c.v, facts, clerk, 3); code(err) != c.want {
			t.Errorf("%+v: %s, want %s", c.v, code(err), c.want)
		}
		if r.Status != Draft || r.RowVersion != 1 || len(r.Lines[1].Pending) != 3 {
			t.Fatalf("a refused confirmation changed the draft: %+v", r)
		}
	}
	r := candidates()
	got, err := r.Confirm([]string{"src.1"}, allLines(settled, l3,
		Verification{LineID: "L1", Fields: []VerifiedField{{Field: "quantity", Value: "24", Locator: "询价单!B2"}}}), facts, clerk, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := "2 rfq.confirm_scope L1.quantity verified \"24\">\"24\" @询价单!B2 [] by=alice\n" +
		"2 rfq.confirm_scope L2.requested_item verified \"ST-200\">\"ST-200\" @询价单!A3 ambiguous[] by=alice\n" +
		"2 rfq.confirm_scope L2.sku_id verified \"sku.1\">\"sku.1\" @ ambiguous[sku.1 sku.2] by=alice\n" +
		"2 rfq.confirm_scope L2.uom changed \"\">\"PCS\" @ missing[] by=alice\n" +
		"2 rfq.confirm_scope L3.quantity verified \"60\">\"60\" @ conflict[60 58] by=alice"
	if describe(got) != want {
		t.Fatalf("corrections\n%s\nwant\n%s", describe(got), want)
	}
	l := r.Lines[1]
	if r.Status != Ready || l.UOM != "PCS" || len(l.Pending) != 0 || !reflect.DeepEqual(l.Locators, []FieldLocator{fl("requested_item", "询价单!A3", "ST-200")}) ||
		l.ProposedBy != agent || l.SkuSpecRevision != 1 {
		t.Fatalf("confirmed %+v", r)
	}
	if got[3].ProposedBy != agent {
		t.Fatalf("proposer %+v", got[3])
	}
}

// A draft without candidate lines confirms as before; its lines may still be verified.
func TestHumanDraftNeedsNoLineByLineVerification(t *testing.T) {
	r := ready()
	if err := confirm(r, []string{"src.1"}, clerk, 1); err != nil {
		t.Fatal(err)
	}
	r = ready()
	got, err := r.Confirm([]string{"src.1"}, []Verification{{LineID: "L1", Fields: []VerifiedField{{Field: "quantity", Value: "118"}}}}, facts, clerk, 1)
	if err != nil || describe(got) != "2 rfq.confirm_scope L1.quantity changed \"120\">\"118\" @ [] by=alice" || r.Lines[0].Quantity != "118" {
		t.Fatalf("%s %v", describe(got), err)
	}
}
