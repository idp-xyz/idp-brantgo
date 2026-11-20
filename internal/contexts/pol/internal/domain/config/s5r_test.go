package config

import (
	"encoding/json"
	"testing"
	"time"
)

// The POL additions of S5R (simulated sign-off E0 of 2026-11-13; S5R-D06, D14, D15, D23, D30, D38;
// POL-INV-001, POL-INV-006, POL-INV-007). Python V0.50 has none of them, so the tests cite the design
// IDs.

// s5rGolden are revisions of the kinds S5R extends as the code of 90925f3 (the S5R base, before R2)
// stored them, with their stored data and content hash, computed on that tree and pinned here:
// trade terms without an insurance percentage, a screening regulation without programme lists, an
// export tax policy of S5 (including not_applicable), the E-2 credit policy, payment terms against
// the documents and a company profile whose country is the unassigned UK. A revision stored before
// S5R must read back, and hash, exactly as it was stored (POL-INV-006: 旧修订保持可读、不改写).
var s5rGolden = []struct {
	content    func() Content
	hash, data string
}{
	{func() Content {
		return Content{Kind: KindTradeTerms, ValidFrom: s5rFrom, TradeTerms: &TradeTerms{Incoterm: "CIF", Edition: "2020", NamedPlace: "DEHAM",
			ResponsibilityNote: "synthetic CIF", Delivery: Delivery{Kind: "lead_time", AnchorEvent: "order_confirmed", Days: 30, DayBasis: "working",
				Calendar: &Ref{Kind: "calendar", ConfigID: "cal", Revision: 2}}}}
	}, "8608a31444cc61346109c9c511a7cdbb6735f94377d8ad65e6993e5687e071ce",
		`{"incoterm":"CIF","edition":"2020","named_place":"DEHAM","responsibility_note":"synthetic CIF","delivery":{"kind":"lead_time",` +
			`"anchor_event":"order_confirmed","days":30,"day_basis":"working","calendar":{"kind":"calendar","id":"cal","revision":2}}}`},
	{func() Content {
		return Content{Kind: KindScreeningRegulation, ValidFrom: s5rFrom, ScreeningRegulation: &ScreeningRegulation{Lists: []ScreeningList{
			{ListCode: "cn.uel", Jurisdiction: "CN"}, {ListCode: "us.sdn", Jurisdiction: "foreign", LegalConfirmationEvidenceID: "legal.1"}},
			RescreenIntervalDays: 30}}
	}, "523acaafe2d9ced5957e1a504c982e45a66e789a4a0a7a9870fca12b9a9673d0",
		`{"lists":[{"list_code":"cn.uel","jurisdiction":"CN"},{"list_code":"us.sdn","jurisdiction":"foreign",` +
			`"legal_confirmation_evidence_id":"legal.1"}],"rescreen_interval_days":30}`},
	{func() Content {
		return Content{Kind: KindExportTaxPolicy, ValidFrom: s5rFrom, ExportTaxPolicy: &ExportTaxPolicy{Name: "synthetic export tax policy",
			RefundMethod: "exempt_refund", PolicyBasis: "synthetic policy basis", ProfessionalReviewEvidenceID: "tax.review"}}
	}, "7a4b0d17c8704193bec47680590807cc07cbe8303aa235af8cf729c5b103bc82",
		`{"name":"synthetic export tax policy","refund_method":"exempt_refund","policy_basis":"synthetic policy basis",` +
			`"professional_review_evidence_id":"tax.review"}`},
	{func() Content {
		return Content{Kind: KindExportTaxPolicy, ValidFrom: s5rFrom, ValidUntil: s5rFrom.Add(24 * time.Hour), ExportTaxPolicy: &ExportTaxPolicy{
			Name: "synthetic", RefundMethod: "not_applicable", PolicyBasis: "synthetic", ProfessionalReviewEvidenceID: "tax.review"}}
	}, "27c89f383e0cc69d93d0ddd1119ea7e4acebbbefd6ec4408ee0985febd3ce265",
		`{"name":"synthetic","refund_method":"not_applicable","policy_basis":"synthetic","professional_review_evidence_id":"tax.review"}`},
	{func() Content {
		return Content{Kind: KindCreditPolicy, ValidFrom: s5rFrom, CreditPolicy: &CreditPolicy{Name: "E-2", Currency: "USD", DefaultLimit: "0",
			LimitSources: []string{"internal_assessment"}, ApprovalTiers: []CreditApprovalTier{{Level: "credit_officer", MaxAmount: "1000.00"},
				{Level: "credit_manager", MaxAmount: "5000.00"}}, OverdueToleranceDays: 30, DocumentsSecured: true, ReservationValidityDays: 30,
			DecisionValidityDays: 7, TwoApproversAboveTop: true}}
	}, "c236daadb37d190edca78e7b805a2d15afb0a69d1d785cd1430c200546e3878e",
		`{"name":"E-2","currency":"USD","default_limit":"0","limit_sources":["internal_assessment"],"approval_tiers":[{"level":"credit_officer",` +
			`"max_amount":"1000.00"},{"level":"credit_manager","max_amount":"5000.00"}],"overdue_tolerance_days":30,"documents_secured":true,` +
			`"reservation_validity_days":30,"decision_validity_days":7,"two_approvers_above_top":true}`},
	{func() Content {
		return Content{Kind: KindPaymentTerms, ValidFrom: s5rFrom, PaymentTerms: &PaymentTerms{Name: "30/70 CAD", Stages: []PaymentStage{
			{StageID: "deposit", Percent: "30", Trigger: "order_confirmed"},
			{StageID: "balance", Percent: "70", Trigger: "after_shipment", ReleaseControl: "documents"}}}}
	}, "ce8c1aa0b6f5f9e37bc28fad4140ee35ff79695e54e8aa223cdbe3c7563bb4db",
		`{"name":"30/70 CAD","stages":[{"stage_id":"deposit","percent":"30","trigger":"order_confirmed","days":0},{"stage_id":"balance",` +
			`"percent":"70","trigger":"after_shipment","days":0,"release_control":"documents"}]}`},
	{func() Content {
		return Content{Kind: KindCompanyProfile, ValidFrom: s5rFrom, CompanyProfile: &CompanyProfile{LegalName: "合成企业", RegistrationNo: "SYN",
			Country: "UK", Address: "合成地址", BaseCurrency: "CNY", TimeZone: "Asia/Shanghai", PolicyScope: "synthetic"}}
	}, "772a0a1c549530267a9774793cea438344a0aa089d5a0f59a4401f758a13a9f1",
		`{"legal_name":"合成企业","registration_no":"SYN","country":"UK","address":"合成地址","base_currency":"CNY","time_zone":"Asia/Shanghai",` +
			`"policy_scope":"synthetic"}`},
}

var s5rFrom = time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

// POL-INV-006 (S5R-D06, D14, D15, D38): the new members are omitted when empty, so every revision
// stored before S5R keeps its stored data and content hash, decodes without a new member set, and
// hashes as stored after a jsonb round trip; a revision that S5R's rules refuse to save (UK,
// not_applicable) stays readable as it is.
func TestS5RRevisionsStoredBeforeKeepTheirDataAndHash(t *testing.T) {
	for _, g := range s5rGolden {
		c := g.content()
		raw, err := c.DataJSON()
		if err != nil {
			t.Fatal(c.Kind, err)
		}
		if hash, _ := c.Hash(); string(raw) != g.data || hash != g.hash {
			t.Errorf("%s now stores %s with hash %s", c.Kind, raw, hash)
		}
		var generic map[string]any
		if err = json.Unmarshal([]byte(g.data), &generic); err != nil {
			t.Fatal(err)
		}
		reordered, _ := json.Marshal(generic)
		back := Content{Kind: c.Kind, ValidFrom: c.ValidFrom, ValidUntil: c.ValidUntil}
		if err = back.DecodeData(reordered); err != nil {
			t.Fatal(c.Kind, err)
		}
		if hash, _ := back.Hash(); hash != g.hash {
			t.Errorf("%s read back from jsonb hashes as %s", c.Kind, hash)
		}
		switch {
		case back.TradeTerms != nil && back.TradeTerms.InsurancePercent != "":
			t.Errorf("%+v", back.TradeTerms)
		case back.ScreeningRegulation != nil && back.ScreeningRegulation.Programmes != nil:
			t.Errorf("%+v", back.ScreeningRegulation)
		}
	}
	// The stored S5 revisions S5R refuses as new ones read back as stored.
	for _, i := range []int{3, 6} {
		if err := Validate("x", s5rGolden[i].content()); code(err) != "INVALID_KNOWN_VALUE "+map[int]string{3: "/args/data/refund_method",
			6: "/args/data/country"}[i] {
			t.Errorf("%d: %s", i, code(err))
		}
	}
}

// POL-INV-006: the field rules of a new revision.
func TestS5RNewRevisionRules(t *testing.T) {
	trade := func(percent string) Content {
		c := s5rGolden[0].content()
		c.TradeTerms.InsurancePercent = percent
		return c
	}
	tax := func(method string) Content {
		c := s5rGolden[2].content()
		c.ExportTaxPolicy.RefundMethod = method
		return c
	}
	profile := func(country string) Content {
		c := company("company.A")
		c.CompanyProfile.Country = country
		return c
	}
	for _, c := range []struct {
		name    string
		content Content
		want    string
	}{
		// S5R-D06: the contract insurance percentage is optional, at least 110.
		{"no insurance percentage (110)", trade(""), "OK"},
		{"110", trade("110"), "OK"},
		{"120.5", trade("120.5"), "OK"},
		{"109.999999", trade("109.999999"), "INVALID_KNOWN_VALUE /args/data/insurance_percent"},
		{"100", trade("100"), "INVALID_KNOWN_VALUE /args/data/insurance_percent"},
		// S5R-D14: the four statutory methods; not_applicable only on revisions stored before.
		{"exempt_credit_refund", tax("exempt_credit_refund"), "OK"},
		{"exempt_refund", tax("exempt_refund"), "OK"},
		{"exempt", tax("exempt"), "OK"},
		{"taxable", tax("taxable"), "OK"},
		{"not_applicable", tax("not_applicable"), "INVALID_KNOWN_VALUE /args/data/refund_method"},
		// S5R-D15: officially assigned country codes only.
		{"company in GB", profile("GB"), "OK"},
		{"company in UK", profile("UK"), "INVALID_KNOWN_VALUE /args/data/country"},
		{"company in XK", profile("XK"), "INVALID_KNOWN_VALUE /args/data/country"},
		{"English law", legal("GB-ENG", "excluded", "dispatch"), "OK"},
		{"New York law", legal("US-NY", "excluded", "dispatch"), "OK"},
		{"UK law", legal("UK", "excluded", "dispatch"), "INVALID_KNOWN_VALUE /args/data/governing_law"},
		{"UK-ENG law", legal("UK-ENG", "excluded", "dispatch"), "INVALID_KNOWN_VALUE /args/data/governing_law"},
		{"ZZ law", legal("ZZ", "applies", "arrival"), "INVALID_KNOWN_VALUE /args/data/governing_law"},
	} {
		if got := code(Validate("x", c.content)); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	raw, err := trade("120").DataJSON()
	if err != nil || string(raw) != `{"incoterm":"CIF","edition":"2020","named_place":"DEHAM","responsibility_note":"synthetic CIF",`+
		`"delivery":{"kind":"lead_time","anchor_event":"order_confirmed","days":30,"day_basis":"working","calendar":{"kind":"calendar","id":"cal",`+
		`"revision":2}},"insurance_percent":"120"}` {
		t.Fatalf("trade terms with an insurance percentage store %s %v", raw, err)
	}
}

// S5R-D38: a screening regulation enables programme lists with the rules of its screening lists: a
// foreign programme list needs the tenant's legal confirmation, which the publication cites.
func TestS5RProgrammeListsFollowTheListRules(t *testing.T) {
	cn := ScreeningList{ListCode: "cn.uel", Jurisdiction: "CN"}
	reg := func(programmes ...ScreeningList) Content {
		c := regulation(cn)
		c.ScreeningRegulation.Programmes = programmes
		return c
	}
	cnProgrammes := ScreeningList{ListCode: "cn.country_programmes", Jurisdiction: "CN"}
	foreign := ScreeningList{ListCode: "eu.programmes", Jurisdiction: JurisdictionForeign}
	if got := code(Validate("screening", reg(cnProgrammes, foreign))); got != "INVALID_KNOWN_VALUE /args/data/programmes/1/legal_confirmation_evidence_id" {
		t.Fatalf("a foreign programme list without its legal confirmation: %s", got)
	}
	foreign.LegalConfirmationEvidenceID = "legal.eu"
	r := New(KindScreeningRegulation, "screening")
	mustSave(t, r, reg(cnProgrammes, foreign), editor, 1, start)
	if _, err := r.Publish(1, Facts{}, reviewer, 1, []string{"evidence"}, clock); code(err) != "REFERENCE_INVALID /args/evidence_ids" {
		t.Fatalf("the programme list's legal confirmation must be cited: %s", code(err))
	}
	if _, err := r.Publish(1, Facts{}, reviewer, 1, []string{"evidence", "legal.eu"}, clock); err != nil {
		t.Fatal(err)
	}
	raw, err := r.Active.Content.DataJSON()
	if err != nil || string(raw) != `{"lists":[{"list_code":"cn.uel","jurisdiction":"CN"}],"rescreen_interval_days":90,"programmes":[`+
		`{"list_code":"cn.country_programmes","jurisdiction":"CN"},{"list_code":"eu.programmes","jurisdiction":"foreign",`+
		`"legal_confirmation_evidence_id":"legal.eu"}]}` {
		t.Fatalf("stored %s %v", raw, err)
	}
	none, _ := regulation(cn).DataJSON()
	if string(none) != `{"lists":[{"list_code":"cn.uel","jurisdiction":"CN"}],"rescreen_interval_days":90}` {
		t.Fatalf("a regulation without programme lists stores %s", none)
	}
}

// S5R-D30: an approval tier names a role template that decides credit.
func TestS5RCreditTierLevelsAreTemplates(t *testing.T) {
	if len(CreditApprovalLevels) != 2 || CreditApprovalLevels[0] != "credit_officer" || CreditApprovalLevels[1] != "credit_manager" {
		t.Fatalf("%v", CreditApprovalLevels)
	}
	c := s5rGolden[4].content()
	c.CreditPolicy.ApprovalTiers[1].Level = "finance_manager"
	if got := code(Validate("credit", c)); got != "INVALID_KNOWN_VALUE /args/data/approval_tiers/1/level" {
		t.Fatalf("an unknown level: %s", got)
	}
}

// ConfigAt v1 (S5R-D23, POL-INV-007): the revision whose validity contains the instant, published
// before or after it, the latest published of several, never a working or discarded revision, and
// none from a retirement on.
func TestS5RConfigAtAppliesTheRevisionValidAtTheInstant(t *testing.T) {
	day := func(n int) time.Time { return start.Add(time.Duration(n) * 24 * time.Hour) }
	rev := func(number int64, status Status, from, until time.Time, published, ended time.Time) *Revision {
		r := &Revision{Number: number, Status: status, Content: Content{Kind: KindExportTaxPolicy, ValidFrom: from, ValidUntil: until}, EndedAt: ended}
		if !published.IsZero() {
			r.Publication = &Publication{By: reviewer, At: published}
		}
		return r
	}
	// Revision 1 is valid from day 0 for ever and published on day 0; revision 2, published on day
	// 20, is valid from day 10 (after an export on day 12 it applies to it); revision 3 is a draft;
	// revision 4 was discarded.
	r1 := rev(1, Superseded, day(0), time.Time{}, day(0), day(20))
	r2 := rev(2, Active, day(10), time.Time{}, day(20), time.Time{})
	r3 := rev(3, Working, day(0), time.Time{}, time.Time{}, time.Time{})
	r4 := rev(4, Discarded, day(0), time.Time{}, time.Time{}, day(25))
	all := []*Revision{r1, r2, r3, r4}
	for _, c := range []struct {
		at   time.Time
		want int64
	}{
		{day(-1), 0}, {day(5), 1}, {day(10), 2}, {day(12), 2}, {day(30), 2},
	} {
		got := At(all, c.at)
		if (got == nil && c.want != 0) || (got != nil && got.Number != c.want) {
			t.Errorf("at day %v: %+v, want %d", c.at.Sub(start).Hours()/24, got, c.want)
		}
	}
	// Retiring revision 2 on day 40 ends the configuration from then on; before it the revisions
	// apply as they did. Revision 5, published on day 50, valid from day 0, applies everywhere again.
	r2.Status, r2.EndedAt = Retired, day(40)
	if got := At(all, day(12)); got == nil || got.Number != 2 {
		t.Fatalf("before the retirement: %+v", got)
	}
	for _, at := range []time.Time{day(40), day(45)} {
		if got := At(all, at); got != nil {
			t.Fatalf("after the retirement revision %d applies", got.Number)
		}
	}
	r5 := rev(5, Active, day(0), day(100), day(50), time.Time{})
	all = append(all, r5)
	for _, at := range []time.Time{day(5), day(45), day(99)} {
		if got := At(all, at); got == nil || got.Number != 5 {
			t.Fatalf("at %v: %+v", at, got)
		}
	}
	if got := At(all, day(100)); got != nil {
		t.Fatalf("after revision 5's validity: %d", got.Number)
	}
	if At(nil, day(1)) != nil {
		t.Fatal("no revisions")
	}
}
