package tradecompliancecheck

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Python V0.50 has no CNX (S5 plan "Python parity": none); the tests cite the design IDs.

var (
	at        = time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	human     = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "compliance.1"}
	agent     = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}
	binding   = Binding{OrderID: "o1", OrderRevision: 1, OrderHash: strings.Repeat("a", 64)}
	evidence1 = Evidence{SourceID: "src.1", ContentSHA256: strings.Repeat("1", 64)}
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

// baseInput is BRC-001's synthetic order to a German buyer: buyer and consignee in DE, one line of
// apparel a verified profile classifies as not controlled, end use stated, a synthetic CN and UN
// programme list used (E-3). hits returns the programmes of those lists naming countries: a CN
// embargo on KP and a UN arms embargo on IR.
func baseInput() Input {
	return Input{CheckID: "tcc.1", EvaluationID: "tce.1", Seq: 1, Binding: binding,
		Addresses: []Address{
			{Role: "buyer", PartyID: "cust", PartyRevision: 1, AddressID: "hq", Known: true, Country: "DE"},
			{Role: RoleConsignee, PartyID: "cust", PartyRevision: 1, AddressID: "wh", Known: true, Country: "DE"},
		},
		Lines: []Line{{LineID: "L1", SkuID: "sku.shirt", SpecRevision: 1, Profile: &Profile{ProfileID: "dp.1", Revision: 1, CodeTableVersion: "2026",
			Verified: true, CommodityCode: "6205200099", ExportControl: NotControlled, ContentHash: strings.Repeat("c", 64)}}},
		EndUse:     &EndUse{Statement: "Retail sale of apparel", Evidence: []Evidence{evidence1}},
		Regulation: Regulation{ID: "screening", Revision: 1, ContentHash: strings.Repeat("r", 64), RescreenDays: 30},
		Lists:      []string{"un.sc", "cn.embargo"},
		Versions: map[string]ListVersion{
			"cn.embargo": {ListCode: "cn.embargo", Jurisdiction: "CN", Version: "v1", ContentHash: strings.Repeat("e", 64)},
			"un.sc":      {ListCode: "un.sc", Jurisdiction: "UN", Version: "2026-11", ContentHash: strings.Repeat("u", 64)},
		},
		By: human, Epoch: 3, At: at}
}

func hits(countries ...string) []ProgrammeHit {
	all := map[string]ProgrammeHit{
		"KP": {ListCode: "cn.embargo", Jurisdiction: "CN", Version: "v1", Country: "KP", ProgrammeID: "P1", Name: "合成禁运项目",
			Measure: MeasureEmbargo, ContentHash: strings.Repeat("1", 64)},
		"IR": {ListCode: "un.sc", Jurisdiction: "UN", Version: "2026-11", Country: "IR", ProgrammeID: "U7", Name: "Synthetic arms embargo",
			Measure: MeasureArmsEmbargo, Scope: "arms and related materiel", ContentHash: strings.Repeat("2", 64)},
	}
	var out []ProgrammeHit
	for _, c := range countries {
		if h, ok := all[c]; ok {
			out = append(out, h)
		}
	}
	return out
}

func mustEvaluate(t *testing.T, in Input) *Evaluation {
	t.Helper()
	ev, err := Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// CNX-INV-001 positive: destination (the consignee address's country, D-8) and party countries
// named by no programme of the lists used, the goods not controlled, the end use stated: no_match,
// the document binds the order revision, the regulation and the list versions used, and its hash
// covers its bytes.
func TestEvaluationOfACleanOrderFindsNothing(t *testing.T) {
	ev := mustEvaluate(t, baseInput())
	d := ev.Document
	if d.Outcome != NoMatch || len(d.Findings) != 0 || len(d.Gaps) != 0 {
		t.Fatalf("%+v", d)
	}
	if d.Destination.Country != "DE" || d.Destination.Source != SourceConsignee || d.Destination.AddressID != "wh" {
		t.Fatalf("destination %+v", d.Destination)
	}
	if len(d.Programmes) != 2 || d.Programmes[0].Code != "cn.embargo" || d.Programmes[1].Code != "un.sc" {
		t.Fatalf("programmes %+v", d.Programmes)
	}
	if d.Order != binding || d.Regulation.ID != "screening" || d.Lines[0].Classification != NotControlled || d.ValidUntil != "2026-12-12T08:00:00.000000Z" {
		t.Fatalf("%+v", d)
	}
	again, err := Parse(ev.Bytes, ev.FactsHash)
	if err != nil || again.Outcome != NoMatch {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Parse(append([]byte(nil), ev.Bytes[:len(ev.Bytes)-1]...), ev.FactsHash); code(err) != "REFERENCE_INVALID /evaluation" {
		t.Fatalf("tampered: %s", code(err))
	}
	if Hash(ev.Bytes) != ev.FactsHash || mustEvaluate(t, baseInput()).FactsHash != ev.FactsHash {
		t.Fatal("the document and its hash are deterministic")
	}
}

// TD-AT12 (CNX half), UL CNX-EmbargoCheck: a destination under an embargo programme is a finding
// that applies whatever the goods; a party country under an arms embargo (controlled goods only) is
// recorded but applies only when a line is controlled; a controlled line is a finding of its own; a
// hit of a list or version the evaluation did not use is ignored.
func TestProgrammeMatchesFollowDestinationPartiesAndGoods(t *testing.T) {
	in := baseInput()
	in.Addresses[1].Country = "KP"
	in.Hits = append(hits("KP"), ProgrammeHit{ListCode: "cn.embargo", Version: "v0", Country: "KP", ProgrammeID: "OLD", Measure: MeasureEmbargo},
		ProgrammeHit{ListCode: "us.ofac", Version: "v1", Country: "KP", ProgrammeID: "X", Measure: MeasureEmbargo})
	d := mustEvaluate(t, in).Document
	if d.Outcome != Match || len(d.Findings) != 1 {
		t.Fatalf("%+v", d.Findings)
	}
	f := d.Findings[0]
	if f.Kind != FindingProgramme || f.ListCode != "cn.embargo" || f.ProgrammeID != "P1" || f.Country != "KP" || !f.Applies ||
		f.Goods != GoodsAll || strings.Join(f.Roles, ",") != "destination,consignee" {
		t.Fatalf("%+v", f)
	}

	in = baseInput()
	in.Addresses[0].Country = "IR" // the buyer, under an arms embargo
	in.Hits = hits("IR")
	d = mustEvaluate(t, in).Document
	if d.Outcome != NoMatch || len(d.Findings) != 1 || d.Findings[0].Applies || strings.Join(d.Findings[0].Roles, ",") != "buyer" {
		t.Fatalf("controlled-goods programme on uncontrolled goods: %+v", d)
	}
	in.Lines[0].Profile.ExportControl, in.Lines[0].Profile.ListReference = Controlled, "合成管制清单 1A001"
	d = mustEvaluate(t, in).Document
	if d.Outcome != Match || len(d.Findings) != 2 || !d.Findings[0].Applies || d.Findings[1].Kind != FindingControlled ||
		d.Findings[1].ListReference != "合成管制清单 1A001" || d.Lines[0].Classification != Controlled {
		t.Fatalf("controlled goods: %+v", d.Findings)
	}
}

// S5 D-8: the destination is the consignee address's country unless the evaluator enters another
// with a reason and evidence; a party address PTY has no country for is a gap until a country is
// entered; an entry for an address the order does not bind, or one PTY records a country for, is
// refused.
func TestCountriesComeFromPTYOrAreEnteredWithEvidence(t *testing.T) {
	in := baseInput()
	in.Addresses[0].Country, in.Addresses[1].Country = "", ""
	d := mustEvaluate(t, in).Document
	if d.Outcome != Incomplete || len(d.Gaps) != 3 || d.Gaps[0].Kind != GapDestinationUnknown || d.Gaps[1] != (Gap{Kind: GapCountryUnknown, Ref: "cust/hq"}) {
		t.Fatalf("gaps %+v", d.Gaps)
	}
	in.Countries = []Entered{{PartyID: "cust", AddressID: "hq", Country: "DE", Evidence: []Evidence{evidence1}},
		{PartyID: "cust", AddressID: "wh", Country: "NL", Evidence: []Evidence{evidence1}}}
	d = mustEvaluate(t, in).Document
	if d.Outcome != NoMatch || d.Parties[0].Source != SourceEntered || d.Destination.Country != "NL" || d.Destination.Source != SourceConsignee {
		t.Fatalf("entered %+v", d)
	}
	in.Destination = &Entered{Country: "KP", Reason: "B/L shows onward carriage", Evidence: []Evidence{evidence1}}
	in.Hits = hits("KP")
	d = mustEvaluate(t, in).Document
	if d.Destination.Source != SourceEntered || d.Destination.Reason == "" || d.Outcome != Match {
		t.Fatalf("destination entered %+v", d.Destination)
	}

	in = baseInput()
	in.Countries = []Entered{{PartyID: "cust", AddressID: "other", Country: "DE"}}
	if _, err := Evaluate(in); code(err) != "REFERENCE_INVALID /args/address_countries/0/address_id" {
		t.Fatal(code(err))
	}
	in.Countries = []Entered{{PartyID: "cust", AddressID: "hq", Country: "FR"}}
	if _, err := Evaluate(in); code(err) != "INVALID_KNOWN_VALUE /args/address_countries/0/country" {
		t.Fatal(code(err))
	}

	in = baseInput()
	in.Addresses = append(in.Addresses, Address{Role: RoleConsignee, PartyID: "c2", PartyRevision: 2, AddressID: "x", Known: true, Country: "FR"})
	if d = mustEvaluate(t, in).Document; d.Gaps[0].Kind != GapDestinationAmbiguous {
		t.Fatalf("two consignee countries: %+v", d.Gaps)
	}

	// Only the buyer's, payer's and consignee's countries are required; another role's is matched
	// when PTY records it.
	in = baseInput()
	in.Addresses = append(in.Addresses, Address{Role: "billing", PartyID: "cust", PartyRevision: 1, AddressID: "bill", Known: true},
		Address{Role: "shipper", PartyID: "fwd", PartyRevision: 1, AddressID: "s", Known: true, Country: "KP"})
	in.Hits = hits("KP")
	if d = mustEvaluate(t, in).Document; len(d.Gaps) != 0 || d.Outcome != Match || strings.Join(d.Findings[0].Roles, ",") != "shipper" {
		t.Fatalf("other roles: %+v", d)
	}
	if got := Countries(in.Addresses, &Entered{Country: "NL"}, []Entered{{Country: "FR"}}); strings.Join(got, ",") != "DE,FR,KP,NL" {
		t.Fatalf("countries asked: %v", got)
	}
}

// CNX-INV-001 (商品管制分类与最终用途): a line without a verified declaration profile, a missing
// end use, a programme list the company never imported (A3b: absent, no hits — never a clear one)
// and no list at all are gaps.
func TestMissingClassificationEndUseAndProgrammesAreGaps(t *testing.T) {
	in := baseInput()
	in.Lines = append(in.Lines, Line{LineID: "L2", SkuID: "sku.pants", SpecRevision: 2},
		Line{LineID: "L3", SkuID: "sku.cap", SpecRevision: 1, Profile: &Profile{ProfileID: "dp.3", Revision: 1}})
	in.EndUse = nil
	delete(in.Versions, "un.sc")
	d := mustEvaluate(t, in).Document
	var kinds []string
	for _, g := range d.Gaps {
		kinds = append(kinds, g.Kind+":"+g.Ref)
	}
	want := "classification_unknown:L2 classification_unverified:L3 end_use_missing: programme_list_missing:un.sc"
	if d.Outcome != Incomplete || strings.Join(kinds, " ") != want || len(d.Programmes) != 1 || strings.Join(d.Lists, ",") != "cn.embargo,un.sc" {
		t.Fatalf("%v %+v", kinds, d.Programmes)
	}
	in = baseInput()
	in.Lists = nil
	if d = mustEvaluate(t, in).Document; d.Gaps[0].Kind != GapProgrammesMissing {
		t.Fatalf("%+v", d.Gaps)
	}
}

func decided(t *testing.T, c *Check, ev *Evaluation, conclusion Status, license bool, when time.Time) (*Decision, error) {
	t.Helper()
	return Decide(c, ev, DecideInput{ID: "tcd.1", EvaluationID: ev.EvaluationID, Conclusion: conclusion, Reason: "核查完成", LicenseRequired: license,
		By: human, Epoch: 4, At: when})
}

func openEvaluated(t *testing.T, in Input) (*Check, *Evaluation) {
	t.Helper()
	c := Open("tcc.1", binding, "", human, "", 3, at)
	ev := mustEvaluate(t, in)
	if err := c.Evaluated(ev.EvaluationID, true); err != nil || c.RowVersion != 1 || c.Status != Pending {
		t.Fatalf("%+v %v", c, err)
	}
	return c, ev
}

// SM-CNX-COMPLIANCE pending → clear: the conclusion binds the evaluation's regulation revision, the
// licence statement and a validity of the rescreen interval from the evaluation, never past the
// regulation's own validity.
func TestClearBindsRegulationLicenceAndValidity(t *testing.T) {
	in := baseInput()
	in.Regulation.ValidUntil = at.AddDate(0, 0, 10)
	c, ev := openEvaluated(t, in)
	d, err := decided(t, c, ev, Clear, true, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !d.ValidUntil.Equal(at.AddDate(0, 0, 10)) || !d.LicenseRequired || d.Regulation.ID != "screening" || d.Regulation.Revision != 1 {
		t.Fatalf("%+v", d)
	}
	c.Decided(d)
	if c.Status != Clear || c.Decision != "tcd.1" || c.RowVersion != 2 {
		t.Fatalf("%+v", c)
	}
	if _, err := decided(t, c, ev, Blocked, false, at); code(err) != "INVALID_STATE /args/check_id" {
		t.Fatalf("a decided check is not decided again: %s", code(err))
	}
	if err := c.Evaluated("tce.2", false); code(err) != "INVALID_STATE /args/order_revision" {
		t.Fatalf("a clear check is reopened before it is evaluated: %s", code(err))
	}
}

// CNX-INV-001 negatives: AI or system conclusions are PERMISSION_DENIED; another evaluation is
// REFERENCE_STALE; an incomplete evaluation never concludes clear (DECLARATION_PROFILE_UNVERIFIED
// for a line without a verified profile, INVALID_STATE otherwise) but may be blocked; an evaluation
// older than the rescreen interval no longer supports clear.
func TestDecisionRefusals(t *testing.T) {
	c, ev := openEvaluated(t, baseInput())
	if _, err := Decide(c, ev, DecideInput{ID: "x", EvaluationID: ev.EvaluationID, Conclusion: Clear, By: agent, At: at}); code(err) != "PERMISSION_DENIED /" {
		t.Fatal(code(err))
	}
	if _, err := Decide(c, ev, DecideInput{ID: "x", EvaluationID: "tce.old", Conclusion: Clear, By: human, At: at}); code(err) != "REFERENCE_STALE /args/evaluation_id" {
		t.Fatal(code(err))
	}
	if _, err := decided(t, c, ev, Clear, false, at.AddDate(0, 0, 30)); code(err) != "REFERENCE_STALE /args/evaluation_id" {
		t.Fatalf("an evaluation past its validity: %s", code(err))
	}
	if _, err := decided(t, c, ev, Status("maybe"), false, at); code(err) != "SCHEMA_INVALID /args/conclusion" {
		t.Fatal(code(err))
	}

	in := baseInput()
	in.Lines[0].Profile.Verified = false
	c, ev = openEvaluated(t, in)
	if _, err := decided(t, c, ev, Clear, false, at); code(err) != "DECLARATION_PROFILE_UNVERIFIED /args/evaluation_id" {
		t.Fatal(code(err))
	}
	in = baseInput()
	in.EndUse = nil
	c, ev = openEvaluated(t, in)
	if _, err := decided(t, c, ev, Clear, false, at); code(err) != "INVALID_STATE /args/evaluation_id" {
		t.Fatal(code(err))
	}
	d, err := decided(t, c, ev, Blocked, false, at)
	if err != nil || !d.ValidUntil.IsZero() || d.LicenseRequired {
		t.Fatalf("blocked on incomplete facts: %+v %v", d, err)
	}

	empty := Open("tcc.2", binding, "", human, "", 3, at)
	if _, err := Decide(empty, nil, DecideInput{ID: "x", EvaluationID: "tce.1", Conclusion: Blocked, By: human, At: at}); code(err) != "INVALID_STATE /args/evaluation_id" {
		t.Fatalf("a check opened for a new revision has nothing to decide: %s", code(err))
	}
}

// TD-AT12 (CNX half), SM-CNX-COMPLIANCE: blocked never lapses and is not reopened; only a new
// evaluation after the facts changed returns it to pending, without a current decision.
func TestBlockedReturnsToPendingOnlyByEvaluation(t *testing.T) {
	in := baseInput()
	in.Addresses[1].Country = "KP"
	in.Hits = hits("KP")
	c, ev := openEvaluated(t, in)
	if ev.Outcome != Match {
		t.Fatalf("%+v", ev.Document)
	}
	d, err := decided(t, c, ev, Blocked, false, at)
	if err != nil {
		t.Fatal(err)
	}
	c.Decided(d)
	if _, err := Reopen(c, d, CauseRegulationRevised, &RegulationRef{ID: "screening", Revision: 2}, nil, Reopening{At: at}); code(err) != "INVALID_STATE /args/check_id" {
		t.Fatalf("blocked is not reopened: %s", code(err))
	}
	if err := c.Evaluated("tce.2", false); err != nil || c.Status != Pending || c.Decision != "" || c.Evaluation != "tce.2" || c.RowVersion != 3 {
		t.Fatalf("%+v %v", c, err)
	}
}

// SM-CNX-COMPLIANCE clear → reopen → pending: a regulation revision other than the conclusion's,
// or a conclusion past its validity; the reopened check has no current evaluation.
func TestReopenCauses(t *testing.T) {
	setup := func() (*Check, *Decision) {
		c, ev := openEvaluated(t, baseInput())
		d, err := decided(t, c, ev, Clear, false, at)
		if err != nil {
			t.Fatal(err)
		}
		c.Decided(d)
		return c, d
	}
	c, d := setup()
	if _, err := Reopen(c, d, CauseRegulationRevised, &RegulationRef{ID: "screening", Revision: 1}, nil, Reopening{At: at}); code(err) != "INVALID_STATE /args/regulation_revision" {
		t.Fatal(code(err))
	}
	if _, err := Reopen(c, d, CauseExpired, nil, nil, Reopening{At: at.AddDate(0, 0, 29)}); code(err) != "INVALID_STATE /args/check_id" {
		t.Fatalf("still valid: %s", code(err))
	}
	if _, err := Reopen(c, d, CauseProgrammeUpdated, nil, nil, Reopening{At: at}); code(err) != "INVALID_STATE /args/check_id" {
		t.Fatalf("every list current: %s", code(err))
	}
	r, err := Reopen(c, d, CauseRegulationRevised, &RegulationRef{ID: "screening", Revision: 2}, nil, Reopening{ID: "tcr.1", Reason: "新修订", At: at})
	if err != nil || c.Status != Pending || c.Evaluation != "" || c.Decision != "" || c.RowVersion != 3 || r.DecisionID != d.ID || r.CheckID != c.ID {
		t.Fatalf("%+v %+v %v", c, r, err)
	}
	c, d = setup()
	r, err = Reopen(c, d, CauseExpired, nil, nil, Reopening{ID: "tcr.2", At: at.AddDate(0, 0, 30)})
	if err != nil || r.Regulation != nil || c.Status != Pending {
		t.Fatalf("%+v %v", r, err)
	}
	c, d = setup()
	r, err = Reopen(c, d, CauseProgrammeUpdated, nil, []string{"un.sc", "cn.embargo"}, Reopening{ID: "tcr.3", At: at})
	if err != nil || strings.Join(r.Updated, ",") != "cn.embargo,un.sc" || c.Status != Pending {
		t.Fatalf("%+v %v", r, err)
	}
	if err := CheckVersion(nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := CheckVersion(c, 2); code(err) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal(code(err))
	}
}
