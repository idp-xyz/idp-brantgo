package costing

import (
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	author  = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "author"}
	agent   = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent"}
	checker = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "checker"}
	now     = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

	usd      = Ref{Kind: KindCurrency, ID: "USD", Revision: 1}
	pcs      = Ref{Kind: KindUnit, ID: "PCS", Revision: 1}
	dozen    = Ref{Kind: KindUnit, ID: "DOZEN", Revision: 1}
	policyR  = Ref{Kind: KindPricingPolicy, ID: "policy.1", Revision: 1}
	fee      = Ref{Kind: KindFeeCategory, ID: "freight", Revision: 1}
	trade    = Ref{Kind: KindTradeTerms, ID: "terms", Revision: 1}
	payment  = Ref{Kind: KindPaymentTerms, ID: "payment", Revision: 1}
	calendar = Ref{Kind: KindCalendar, ID: "cal", Revision: 1}
	conv     = Ref{Kind: KindUnitConversion, ID: "sku.1.dozen", Revision: 1}
	place    = Ref{Kind: KindLocation, ID: "port.test", Revision: 1}
)

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code
	}
	if err != nil {
		return "ERROR: " + err.Error()
	}
	return "OK"
}

func at(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Fields[0].Path
	}
	return ""
}

func want(t *testing.T, err error, wantCode, wantPath string) {
	t.Helper()
	if code(err) != wantCode || at(err) != wantPath {
		t.Fatalf("%s at %s, want %s at %s", code(err), at(err), wantCode, wantPath)
	}
}

// fixture is the Python costing fixture's context.1: rfq.1 at row version 2, L1 120 PCS of sku.1.
func fixture() Data {
	return Data{Rfq: &RfqRef{RfqID: "rfq.1", RowVersion: 2}, Kind: Actual,
		Lines:               []Line{{LineID: "L1", SkuID: "sku.1", SkuSpecRevision: 1, Quantity: "120", Unit: pcs}},
		CalculationCurrency: &usd, PricingPolicy: &policyR,
		Basis: &Basis{SpecificationBasis: "SPEC-1", NamedPlace: place, RequiredFeeTypes: []Ref{fee}, TaxBasis: "zero",
			Delivery:  Delivery{Kind: "fixed", Date: "2026-11-15", Milestone: "handover", Place: place, TimeZone: "Asia/Shanghai"},
			CostBasis: "nominal_direct_cost", SalesTradeTerms: trade, SalesPaymentTerms: payment}}
}

func inForce(kind string) Config { return Config{Kind: kind} }

func facts() Facts {
	f := Facts{
		Rfq: &Rfq{RfqID: "rfq.1", RowVersion: 2, Ready: true, LinesHash: "h", Lines: []RfqLine{
			{LineID: "L1", SkuID: "sku.1", SkuSpecRevision: 1, Quantity: "120", UOM: "PCS"}}},
		Skus:    map[string]bool{"sku.1": true},
		Specs:   map[Spec]func(string) error{{SkuID: "sku.1", Revision: 1}: func(string) error { return nil }},
		Configs: map[Ref]Config{},
	}
	for _, r := range []Ref{fee, trade, payment, calendar} {
		f.Configs[r] = inForce(r.Kind)
	}
	f.Configs[usd] = Config{Kind: KindCurrency, CurrencyCode: "USD"}
	f.Configs[policyR] = Config{Kind: KindPricingPolicy, PolicyCurrency: "USD"}
	f.Configs[pcs] = Config{Kind: KindUnit, Unit: &Unit{Code: "PCS", Precision: 0}}
	f.Configs[dozen] = Config{Kind: KindUnit, Unit: &Unit{Code: "DOZEN", Precision: 0}}
	f.Configs[calendar] = Config{Kind: KindCalendar, TimeZone: "Asia/Shanghai"}
	f.Configs[conv] = Config{Kind: KindUnitConversion, Conversion: &Conversion{SkuID: "sku.1", SpecRevision: 1, From: "DOZEN", To: "PCS"}}
	return f
}

// drafted returns a context with the fixture saved by author as revision 1.
func drafted(d Data) *Context {
	c := New("context.1", "author", "trade")
	c.Save(d, author, 1, now)
	return c
}

func TestSaveStartsAndReplacesTheWorkingRevision(t *testing.T) {
	if err := CheckSave(nil, false, 1); code(err) != kernel.CodeNotFound {
		t.Fatalf("missing context at version 1: %v", err)
	}
	if err := CheckSave(nil, false, 0); err != nil {
		t.Fatal(err)
	}
	c := drafted(Data{})
	if c.RootVersion != 1 || c.LastRevision != 1 || c.Working().Number != 1 || !c.Working().Fresh {
		t.Fatalf("draft %+v", c)
	}
	want(t, CheckSave(c, false, 1), kernel.CodeNotFound, "/args/id")
	want(t, CheckSave(c, true, 2), kernel.CodeVersionConflict, "/args/expected_version")
	c.Save(fixture(), agent, 2, now.Add(time.Minute))
	w := c.Working()
	if c.RootVersion != 2 || c.LastRevision != 1 || w.Hash != fixture().Hash() || !slices.Equal(w.Contributors, []string{"author", "agent"}) ||
		w.SavedBy != agent {
		t.Fatalf("second save %+v", w)
	}
}

// Python costing_context.save_draft provenance: an assumption keeps its proposer while it stays the
// same; a new or changed assumption is proposed by whoever saved it; revise copies the proposers.
func TestAssumptionProvenance(t *testing.T) {
	a := Assumption{ID: "a.qty", Path: "/context/lines/0/quantity", ValueText: "120", Reason: "r", Evidence: []Evidence{{SourceID: "s", Locator: "l"}}}
	b := Assumption{ID: "a.days", Path: "/context/comparison_basis/delivery/days", ValueText: "10", Reason: "r", Evidence: a.Evidence}
	d := fixture()
	d.Assumptions = []Assumption{a}
	c := drafted(d)
	d.Assumptions = []Assumption{a, b}
	c.Save(d, agent, 1, now.Add(time.Hour))
	p := c.Working().Proposals
	if p["a.qty"].By != author || !p["a.qty"].At.Equal(now) || p["a.days"].By != agent {
		t.Fatalf("proposals %+v", p)
	}
	changed := a
	changed.Reason = "另一理由"
	d.Assumptions = []Assumption{changed, b}
	c.Save(d, checker, 1, now.Add(2*time.Hour))
	if p = c.Working().Proposals; p["a.qty"].By != checker || p["a.days"].By != agent || len(p) != 2 {
		t.Fatalf("proposals after a change %+v", p)
	}
	// The days assumption names a fixed delivery's days: there is no such input.
	_, err := c.Verify(1, 0, kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"}, 1, now, nil, facts())
	want(t, err, kernel.CodeReferenceInvalid, "/args/data/assumptions/1/path")
	d.Assumptions = []Assumption{changed}
	c.Save(d, checker, 1, now.Add(3*time.Hour))
	if _, err = c.Verify(1, 0, kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"}, 1, now, nil, facts()); err != nil {
		t.Fatal(err)
	}
	r, err := c.Revise(1, author, 1, now.Add(4*time.Hour))
	if err != nil || r.Proposals["a.qty"].By != checker || r.Base != 1 || !slices.Equal(r.Contributors, []string{"author"}) {
		t.Fatalf("revision %+v, %v", r, err)
	}
}

func TestRevisionTransitions(t *testing.T) {
	c := drafted(fixture())
	_, err := c.Revise(1, author, 1, now)
	want(t, err, kernel.CodeInvalidState, "/args/base_revision")
	_, err = c.Withdraw(1, checker, 1, now, "撤销", nil)
	want(t, err, kernel.CodeInvalidState, "/args/target_revision")
	if _, err = c.Discard(2, author, 1, now, "丢弃"); code(err) != kernel.CodeInvalidState {
		t.Fatalf("discard another revision: %v", err)
	}
	r, err := c.Discard(1, author, 1, now, "丢弃")
	if err != nil || r.Status != Discarded || r.End.Reason != "丢弃" || c.Working() != nil || c.RootVersion != 2 {
		t.Fatalf("discard %+v, %v", r, err)
	}
	if err = CheckSave(c, true, 2); err != nil {
		t.Fatalf("a context without a sealed revision is saved again: %v", err)
	}
	c.Save(fixture(), author, 1, now)
	if c.Working().Number != 2 {
		t.Fatalf("numbers are not reused: %+v", c.Working())
	}
	if _, err = c.Verify(2, 0, checker, 7, now, []Evidence{{SourceID: "s", Locator: "l"}}, facts()); err != nil {
		t.Fatal(err)
	}
	want(t, CheckSave(c, true, 4), kernel.CodeInvalidState, "/args/working_revision")
	if _, err = c.Revise(2, author, 1, now); err != nil {
		t.Fatal(err)
	}
	_, err = c.Revise(2, author, 1, now)
	want(t, err, kernel.CodeInvalidState, "/args/base_revision")
	want(t, c.CheckVerify(3, 0, checker), CodeReferenceStale, "/args/supersedes_revision")
	sealed, err := c.Verify(3, 2, checker, 8, now.Add(time.Minute), nil, facts())
	if err != nil || sealed.Status != Active || c.Revision(2).Status != Superseded || !c.Revision(2).SupersededAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("supersede %+v, %v", c.Revisions, err)
	}
	if r, err = c.Withdraw(2, checker, 9, now, "撤销旧修订", []Evidence{{SourceID: "s", Locator: "l"}}); err != nil || r.Status != Withdrawn ||
		c.Active().Number != 3 {
		t.Fatalf("withdraw superseded %+v, %v", r, err)
	}
	_, err = c.Withdraw(2, checker, 9, now, "again", nil)
	want(t, err, kernel.CodeInvalidState, "/args/target_revision")
	if _, err = c.Withdraw(3, checker, 9, now, "撤销", nil); err != nil || c.Active() != nil {
		t.Fatalf("withdrawing the active revision leaves none: %v", err)
	}
}

// T02 §13.2, §17.2: verification needs the working revision, the exact active revision to supersede,
// a complete content and a verifier who did not contribute.
func TestCheckVerify(t *testing.T) {
	c := drafted(Data{Kind: Actual})
	want(t, c.CheckVerify(2, 0, checker), kernel.CodeInvalidState, "/args/working_revision")
	want(t, c.CheckVerify(1, 1, checker), CodeReferenceStale, "/args/supersedes_revision")
	want(t, c.CheckVerify(1, 0, checker), CodeCostInputNotReady, "/args/data/rfq_ref")
	for field, edit := range map[string]func(*Data){
		"/args/data/context_kind":             func(d *Data) { d.Kind = "" },
		"/args/data/lines":                    func(d *Data) { d.Lines = nil },
		"/args/data/calculation_currency_ref": func(d *Data) { d.CalculationCurrency = nil },
		"/args/data/pricing_policy_ref":       func(d *Data) { d.PricingPolicy = nil },
		"/args/data/comparison_basis":         func(d *Data) { d.Basis = nil },
	} {
		d := fixture()
		edit(&d)
		want(t, drafted(d).CheckVerify(1, 0, checker), CodeCostInputNotReady, field)
	}
	c = drafted(fixture())
	c.Save(fixture(), checker, 1, now)
	want(t, c.CheckVerify(1, 0, checker), kernel.CodePermissionDenied, "/")
	if err := c.CheckVerify(1, 0, kernel.ActorRef{Kind: kernel.ActorHuman, ID: "third"}); err != nil {
		t.Fatal(err)
	}
}

// The document rules of verification (Python _document_ready of costing_context, T02 §13.1).
func TestReady(t *testing.T) {
	cases := []struct {
		name  string
		data  func(d *Data)
		facts func(f *Facts)
		code  string
		where string
	}{
		{"ready", nil, nil, "OK", ""},
		{"unknown RFQ", nil, func(f *Facts) { f.Rfq = nil }, kernel.CodeReferenceInvalid, "/args/data/rfq_ref"},
		{"draft RFQ", nil, func(f *Facts) { f.Rfq.Ready = false }, CodeReferenceStale, "/args/data/rfq_ref"},
		{"RFQ at another version", nil, func(f *Facts) { f.Rfq.RowVersion = 3 }, CodeReferenceStale, "/args/data/rfq_ref"},
		{"unknown SKU", nil, func(f *Facts) { f.Skus = nil }, kernel.CodeReferenceInvalid, "/args/data/lines/0/sku_id"},
		{"stale specification", nil, func(f *Facts) {
			f.Specs[Spec{SkuID: "sku.1", Revision: 1}] = func(p string) error { return kernel.RejectAs("REFERENCE_STALE", kernel.CategoryConflict, p, "") }
		}, CodeReferenceStale, "/args/data/lines/0/sku_spec_revision"},
		{"specification not answered", nil, func(f *Facts) { delete(f.Specs, Spec{SkuID: "sku.1", Revision: 1}) }, kernel.CodeReferenceInvalid,
			"/args/data/lines/0/sku_spec_revision"},
		{"configuration not in force", nil, func(f *Facts) {
			c := f.Configs[fee]
			c.Check = func(p string) error { return kernel.RejectAs("CONFIG_NOT_ACTIVE", kernel.CategoryInvalid, p, "") }
			f.Configs[fee] = c
		}, "CONFIG_NOT_ACTIVE", "/args/data/comparison_basis/required_fee_type_refs/0"},
		{"configuration of the wrong kind", func(d *Data) { d.Basis.SalesTradeTerms = payment }, nil, kernel.CodeReferenceInvalid,
			"/args/data/comparison_basis/sales_trade_terms_ref/kind"},
		{"unknown configuration", nil, func(f *Facts) { delete(f.Configs, trade) }, kernel.CodeReferenceInvalid,
			"/args/data/comparison_basis/sales_trade_terms_ref"},
		{"partial line set", nil, func(f *Facts) {
			f.Rfq.Lines = append(f.Rfq.Lines, RfqLine{LineID: "L2", SkuID: "sku.1", SkuSpecRevision: 1, Quantity: "1", UOM: "PCS"})
		}, CodeComparisonBasisMismatch, "/args/data/lines"},
		{"another line", func(d *Data) { d.Lines[0].LineID = "L9" }, nil, CodeComparisonBasisMismatch, "/args/data/lines"},
		{"another specification", func(d *Data) { d.Lines[0].SkuSpecRevision = 2 }, func(f *Facts) {
			f.Specs[Spec{SkuID: "sku.1", Revision: 2}] = func(string) error { return nil }
		}, CodeComparisonBasisMismatch, "/args/data/lines/0"},
		{"another unit", func(d *Data) { d.Lines[0].Unit = dozen; d.Lines[0].Quantity = "10" }, nil, CodeComparisonBasisMismatch, "/args/data/lines/0"},
		{"another quantity", func(d *Data) { d.Lines[0].Quantity = "121" }, nil, CodeComparisonBasisMismatch, "/args/data/lines/0/quantity"},
		{"the same quantity written otherwise", func(d *Data) { d.Lines[0].Quantity = "120.000" }, nil, "OK", ""},
		{"a fractional piece", func(d *Data) { d.Lines[0].Quantity = "120.5" }, nil, kernel.CodeInvalidKnownValue, "/args/data/lines/0/quantity"},
		{"over a million", func(d *Data) { d.Lines[0].Quantity = "1000001" }, nil, kernel.CodeInvalidKnownValue, "/args/data/lines/0/quantity"},
		{"what_if quantity", func(d *Data) { d.Kind = WhatIf; d.Lines[0].Quantity = "1200" }, nil, "OK", ""},
		{"what_if SKU", func(d *Data) { d.Kind = WhatIf; d.Lines[0].SkuID = "sku.2" }, func(f *Facts) {
			f.Skus["sku.2"] = true
			f.Specs[Spec{SkuID: "sku.2", Revision: 1}] = func(string) error { return nil }
		}, CodeComparisonBasisMismatch, "/args/data/lines/0"},
		{"duplicate line", func(d *Data) { d.Lines = append(d.Lines, d.Lines[0]) }, nil, kernel.CodeDuplicateLineID, "/args/data/lines"},
		{"policy of another currency", nil, func(f *Facts) { f.Configs[policyR] = Config{Kind: KindPricingPolicy, PolicyCurrency: "CNY"} },
			kernel.CodeReferenceInvalid, "/args/data/pricing_policy_ref"},
		{"conversion of the line's SKU", func(d *Data) { d.Lines[0].Conversions = []Ref{conv} }, nil, "OK", ""},
		{"conversion of another SKU", func(d *Data) { d.Lines[0].Conversions = []Ref{conv} }, func(f *Facts) {
			f.Configs[conv] = Config{Kind: KindUnitConversion, Conversion: &Conversion{SkuID: "sku.2", SpecRevision: 1, From: "DOZEN", To: "PCS"}}
		}, kernel.CodeReferenceInvalid, "/args/data/lines/0/conversion_refs/0"},
		{"unknown time zone", func(d *Data) { d.Basis.Delivery.TimeZone = "Invalid/Nowhere" }, nil, kernel.CodeInvalidKnownValue,
			"/args/data/comparison_basis/delivery/time_zone"},
		{"local time zone", func(d *Data) { d.Basis.Delivery.TimeZone = "Local" }, nil, kernel.CodeInvalidKnownValue,
			"/args/data/comparison_basis/delivery/time_zone"},
		{"lead time on its calendar", lead("Asia/Shanghai", "costing_context:context.1"), nil, "OK", ""},
		{"lead time on a calendar of another zone", lead("Europe/Berlin", "costing_context:context.1"), nil, kernel.CodeInvalidKnownValue,
			"/args/data/comparison_basis/delivery/calendar_ref"},
		{"lead time after an unmapped event", lead("Asia/Shanghai", "order_confirmed:o.1"), nil, CodeDependencyNotSupported,
			"/args/data/comparison_basis/delivery/anchor_ref/id"},
		{"assumption on the quantity", assume("/context/lines/0/quantity", "120"), nil, "OK", ""},
		{"assumption on a supply price", assume("/supplier_offer/offer.1/lines/0/tiers/0/unit_price", "5"), nil, "OK", ""},
		{"assumption with another value", assume("/context/lines/0/quantity", "100"), nil, kernel.CodeInvalidKnownValue,
			"/args/data/assumptions/0/value_text"},
		{"assumption on no line", assume("/context/lines/1/quantity", "120"), nil, kernel.CodeReferenceInvalid, "/args/data/assumptions/0/path"},
		{"assumption on the fixed date", assume("/context/comparison_basis/delivery/date", "2026-11-15"), nil, "OK", ""},
		{"assumption on lead days", func(d *Data) {
			lead("Asia/Shanghai", "costing_context:context.1")(d)
			assume("/context/comparison_basis/delivery/days", "10")(d)
		}, nil, "OK", ""},
		{"unsupported assumption", assume("/context/calculation_currency_ref", "USD"), nil, CodeDependencyNotSupported,
			"/args/data/assumptions/0/path"},
	}
	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			d, f := fixture(), facts()
			d.Lines = slices.Clone(d.Lines)
			basis := *d.Basis
			d.Basis = &basis
			if k.data != nil {
				k.data(&d)
			}
			if k.facts != nil {
				k.facts(&f)
			}
			b, err := Ready(d, f)
			want(t, err, k.code, k.where)
			if err == nil && (b.RfqID != "rfq.1" || b.RowVersion != 2 || b.LinesHash != "h") {
				t.Fatalf("binding %+v", b)
			}
		})
	}
}

func lead(zone, anchor string) func(*Data) {
	return func(d *Data) {
		cal := calendar
		d.Basis.Delivery = Delivery{Kind: "lead_time", Anchor: &Ref{Kind: KindDeliveryAnchor, ID: anchor, Revision: 1}, Days: 10,
			DayBasis: "working", Calendar: &cal, Milestone: "handover", Place: place, TimeZone: zone}
	}
}

func assume(path, value string) func(*Data) {
	return func(d *Data) {
		d.Assumptions = append(d.Assumptions, Assumption{ID: "a." + value, Path: path, ValueText: value, Reason: "试算",
			Evidence: []Evidence{{SourceID: "s", Locator: "l"}}})
	}
}

// A draft may name an RFQ that is not ready and configurations not in force; what it names must
// exist, be of its field's kind and appear once.
func TestCheckDraft(t *testing.T) {
	f := facts()
	f.Rfq.Ready = false
	if err := CheckDraft(fixture(), f); err != nil {
		t.Fatal(err)
	}
	if err := CheckDraft(Data{}, Facts{}); err != nil {
		t.Fatal(err)
	}
	d := fixture()
	d.Assumptions = []Assumption{{ID: "a", Path: "/context/lines/0/quantity"}, {ID: "b", Path: "/context/lines/0/quantity"}}
	want(t, CheckDraft(d, facts()), kernel.CodeDuplicateLineID, "/args/data/assumptions")
	d.Assumptions = []Assumption{{ID: "a", Path: "/context/lines/0/quantity"}, {ID: "a", Path: "/context/comparison_basis/delivery/date"}}
	want(t, CheckDraft(d, facts()), kernel.CodeDuplicateLineID, "/args/data/assumptions")
	d = fixture()
	d.Rfq.RfqID = "rfq.other"
	want(t, CheckDraft(d, facts()), kernel.CodeReferenceInvalid, "/args/data/rfq_ref")
	d = fixture()
	d.RequestedCurrency = &policyR
	want(t, CheckDraft(d, facts()), kernel.CodeReferenceInvalid, "/args/data/requested_currency_ref/kind")
}

// Verify seals the working revision with the RFQ binding and supersedes the active one; the content
// hash stays the content's.
func TestVerifySealsWithTheBinding(t *testing.T) {
	c := drafted(fixture())
	proof := []Evidence{{SourceID: "src.1", Locator: "核实"}}
	r, err := c.Verify(1, 0, checker, 4, now, proof, facts())
	if err != nil || r.Status != Active || r.Seal.By != checker || r.Seal.Epoch != 4 || r.Seal.Rfq != (Binding{RfqID: "rfq.1", RowVersion: 2, LinesHash: "h"}) ||
		!slices.Equal(r.Seal.Evidence, proof) || r.Hash != fixture().Hash() || c.RootVersion != 2 || c.Working() != nil ||
		!slices.Equal(r.Seal.RfqLines, facts().Rfq.Lines) {
		t.Fatalf("verify %+v, %v", r, err)
	}
	refs := fixture().ConfigRefs()
	if !slices.Equal(refs, []Ref{usd, fee, payment, policyR, trade, pcs}) {
		t.Fatalf("config refs %+v", refs)
	}
}
