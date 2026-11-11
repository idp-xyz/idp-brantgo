package quote

import (
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The legal terms of the S3-17 cases: SIM-02's German law with Hamburg arbitration (the CISG
// applies, as German law includes it), the design's default of the CISG with PRC law as the
// issuer's company profile names it, and the quote-text/0.2.0 template that prints them.
var (
	hamburg     = Ref{Kind: KindLegalTerms, ID: "legal.de-hamburg", Revision: 1}
	prcDefault  = Ref{Kind: KindLegalTerms, ID: "legal.default", Revision: 1}
	legalLayout = Ref{Kind: KindQuoteDocumentTemplate, ID: "quote.legal", Revision: 1}

	hamburgTerms = Legal{GoverningLaw: "DE", CISG: CISGApplies, FormationRule: FormationArrival, DisputeResolution: DisputeResolution{
		Method: DisputeArbitration, Institution: "Court of Arbitration of the Hamburg Chamber of Commerce", Seat: "Hamburg", Language: "de"}}
	prcTerms = Legal{GoverningLaw: GoverningLawCN, CISG: CISGApplies, FormationRule: FormationArrival, DisputeResolution: DisputeResolution{
		Method: DisputeArbitration, Institution: "China International Economic and Trade Arbitration Commission", Seat: "Shanghai",
		Language: "en"}}
)

// withLegal is the fixture with POL's answers about both legal terms and the quote-text/0.2.0
// template, the draft naming that template and, when own is set, the Hamburg terms; the issuer's
// profile names the PRC default when def is set.
func withLegal(own, def bool) (Data, Facts) {
	d, f := draft(), facts()
	d.DocumentTemplate = &legalLayout
	if own {
		d.LegalTerms = &hamburg
	}
	f.Configs[hamburg] = Config{ContentHash: hashOf("hamburg"), Legal: &hamburgTerms}
	f.Configs[prcDefault] = Config{ContentHash: hashOf("prc"), Legal: &prcTerms}
	f.Configs[legalLayout] = Config{ContentHash: hashOf("legal layout"), Renderer: QuoteRendererV020}
	if def {
		c := f.Configs[company]
		c.DefaultLegalTerms = &prcDefault
		f.Configs[company] = c
	}
	return d, f
}

// WBS S3-17's compatibility rule: a draft without legal terms is the draft it was before legal terms
// existed (its content hash pinned on the commit before them, edf2c7f), and its freeze seals the
// document TestCommercialDocumentGoldenVectors pins, byte for byte, with no legal_terms member and no
// legal_terms_ref; its acceptances form by the default, arrival.
func TestQuotesWithoutLegalTermsAreSealedAsBefore(t *testing.T) {
	d := draft()
	if got := d.Hash(); got != "cb290d3a1f5af9d9b3fe8a2fd577f070afbc4f1cc78e5fc75030f5545ceff92e" {
		t.Fatalf("the draft hash changed: %s", got)
	}
	s, err := sealed(d, "0.1")
	if err != nil || s.BusinessHash != "fdd9bebd46343277b863d306e619dcd68c68a5f6be10793761cd14979202b774" ||
		strings.Contains(string(s.Commercial), "legal_terms") {
		t.Fatalf("the sealed document changed: %v\n%s", err, s.Commercial)
	}
	if f, err := s.Formation(); err != nil || f != DefaultFormation || f.Check("/x") != nil {
		t.Fatalf("formation %+v %v", f, err)
	}
	// An issuer naming no default and a template of either renderer's predecessor change nothing.
	d, f := withLegal(false, false)
	d.DocumentTemplate = &template
	if s2, err := SealDraft("quote.1", 1, d, f, calculator(d, "0.1"), issuedAt); err != nil || s2.BusinessHash != s.BusinessHash {
		t.Fatalf("with legal terms configured but not adopted: %v", err)
	}
}

// WBS S3-17 (SIM-02 step 3, G0 F-02): the freeze adopts the legal terms the draft names, binds their
// revision in data.legal_terms_ref and the dependency manifest with their content hash, guards them
// with the other POL revisions, and seals their content with the formation they imply — German law
// with the CISG forms by arrival on the CISG — so the terms are part of the business hash. A draft
// naming none adopts its issuer's default (the CISG with PRC law, source issuer_default); a draft
// naming its own never adopts or binds the default.
func TestFreezeSealsTheLegalTermsAndTheirFormation(t *testing.T) {
	d, f := withLegal(true, true)
	s, err := SealDraft("quote.1", 1, d, f, calculator(d, "0.1"), issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseCommercial(s.Commercial, s.BusinessHash)
	want := LegalTerms{Source: LegalSourceQuote, GoverningLaw: "DE", CISG: CISGApplies, DisputeResolution: hamburgTerms.DisputeResolution,
		Formation: Formation{Rule: FormationArrival, Basis: FormationBasisCISG}}
	if err != nil || doc.Data.LegalTerms == nil || *doc.Data.LegalTerms != hamburg || doc.LegalTerms == nil || *doc.LegalTerms != want {
		t.Fatalf("sealed %+v %+v %v", doc.Data.LegalTerms, doc.LegalTerms, err)
	}
	bound := Dependency{Kind: KindLegalTerms, ID: "legal.de-hamburg", Version: 1, ContentHash: hashOf("hamburg")}
	if !slices.Contains(doc.DependencyManifest, bound) || !slices.Contains(s.Configs, bound) ||
		slices.ContainsFunc(s.Configs, func(c Dependency) bool { return c.ID == prcDefault.ID }) {
		t.Fatalf("manifest %+v", doc.DependencyManifest)
	}
	if !strings.Contains(string(s.Commercial), `"legal_terms":{"cisg":"applies","dispute_resolution":{"institution":"Court of Arbitration of `+
		`the Hamburg Chamber of Commerce","language":"de","method":"arbitration","seat":"Hamburg"},"formation":{"basis":"cisg","rule":"arrival"},`+
		`"governing_law":"DE","source":"quote"}`) || !strings.Contains(string(s.Commercial), `"legal_terms_ref":{"id":"legal.de-hamburg"`) {
		t.Fatalf("canonical bytes %s", s.Commercial)
	}
	if formation, err := s.Formation(); err != nil || formation != want.Formation {
		t.Fatalf("formation %+v %v", formation, err)
	}
	plain, _ := sealed(draft(), "0.1")
	if s.BusinessHash == plain.BusinessHash || s.BusinessHash != Hash(s.Commercial) {
		t.Fatal("the legal terms are part of the business hash")
	}

	d, f = withLegal(false, true)
	s, err = SealDraft("quote.1", 1, d, f, calculator(d, "0.1"), issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ = ParseCommercial(s.Commercial, s.BusinessHash)
	if doc.Data.LegalTerms == nil || *doc.Data.LegalTerms != prcDefault || doc.LegalTerms.Source != LegalSourceIssuerDefault ||
		doc.LegalTerms.GoverningLaw != GoverningLawCN || doc.Formation() != (Formation{Rule: FormationArrival, Basis: FormationBasisCISG}) ||
		!slices.Contains(s.Configs, Dependency{Kind: KindLegalTerms, ID: "legal.default", Version: 1, ContentHash: hashOf("prc")}) {
		t.Fatalf("the issuer's default %+v %+v", doc.Data.LegalTerms, doc.LegalTerms)
	}
	if d.LegalTerms != nil {
		t.Fatal("the freeze does not change the draft")
	}
}

// WBS S3-17: contract formation by governing law. Brant implements the arrival rule under the CISG
// (Art. 18(2), 23) and under PRC law (Civil Code Art. 137, 483, 484); another law without the CISG,
// or another rule, is refused with DEPENDENCY_NOT_SUPPORTED — never treated as arrival.
func TestFormationUnderTheGoverningLaw(t *testing.T) {
	for _, c := range []struct {
		law, cisg, rule string
		want            Formation
		code            string
	}{
		{"CN", CISGApplies, FormationArrival, Formation{FormationArrival, FormationBasisCISG}, "OK"},
		{"DE", CISGApplies, FormationArrival, Formation{FormationArrival, FormationBasisCISG}, "OK"},
		{"US-NY", CISGApplies, FormationArrival, Formation{FormationArrival, FormationBasisCISG}, "OK"},
		{"CN", CISGExcluded, FormationArrival, Formation{FormationArrival, FormationBasisPRC}, "OK"},
		{"DE", CISGExcluded, FormationArrival, Formation{}, "DEPENDENCY_NOT_SUPPORTED at /p"},
		{"GB-ENG", CISGExcluded, FormationDispatch, Formation{}, "DEPENDENCY_NOT_SUPPORTED at /p"},
		{"CN", CISGApplies, FormationDispatch, Formation{}, "DEPENDENCY_NOT_SUPPORTED at /p"},
	} {
		got, err := FormationUnder(Legal{GoverningLaw: c.law, CISG: c.cisg, FormationRule: c.rule}, "/p")
		if got != c.want || code(err) != c.code {
			t.Errorf("%s, CISG %s, %s: %+v %s", c.law, c.cisg, c.rule, got, code(err))
		}
	}
	for f, want := range map[Formation]string{DefaultFormation: "OK", {FormationArrival, FormationBasisPRC}: "OK",
		{FormationDispatch, FormationBasisCISG}: "DEPENDENCY_NOT_SUPPORTED at /f", {FormationArrival, "bgb"}: "DEPENDENCY_NOT_SUPPORTED at /f"} {
		if got := code(f.Check("/f")); got != want {
			t.Errorf("%+v: %s", f, got)
		}
	}
	if _, err := (&Seal{Commercial: []byte(`{}`), BusinessHash: strings.Repeat("0", 64)}).Formation(); code(err) !=
		"BUSINESS_HASH_MISMATCH at /business_hash" {
		t.Fatal("a corrupt document has no formation")
	}
}

// The freeze's refusals of legal terms, each at its path: the terms the draft names or its issuer's
// default must be known and in force, of a law and rule Brant forms contracts under, and printed by
// the template — quote-text/0.1.0 prints none, quote-text/0.2.0 needs them.
func TestFreezeRefusesLegalTermsItCannotFormContractsUnder(t *testing.T) {
	notActive := func(p string) error { return kernel.RejectAs("CONFIG_NOT_ACTIVE", kernel.CategoryInvalid, p, "") }
	england := Legal{GoverningLaw: "GB-ENG", CISG: CISGExcluded, FormationRule: FormationDispatch,
		DisputeResolution: DisputeResolution{Method: DisputeLitigation, Court: "High Court of Justice of England and Wales"}}
	germany := hamburgTerms
	germany.CISG = CISGExcluded
	for _, c := range []struct {
		name     string
		own, def bool
		edit     func(d *Data, f *Facts)
		want     string
	}{
		{"English law and the postal rule", true, false, func(_ *Data, f *Facts) { f.Configs[hamburg] = Config{ContentHash: "h", Legal: &england} },
			"DEPENDENCY_NOT_SUPPORTED at /args/data/legal_terms_ref"},
		{"German law without the CISG", true, false, func(_ *Data, f *Facts) { f.Configs[hamburg] = Config{ContentHash: "h", Legal: &germany} },
			"DEPENDENCY_NOT_SUPPORTED at /args/data/legal_terms_ref"},
		{"retired", true, false, func(_ *Data, f *Facts) { c := f.Configs[hamburg]; c.Check = notActive; f.Configs[hamburg] = c },
			"CONFIG_NOT_ACTIVE at /args/data/legal_terms_ref"},
		{"unknown", true, false, func(_ *Data, f *Facts) { delete(f.Configs, hamburg) }, "REFERENCE_INVALID at /args/data/legal_terms_ref"},
		{"not legal terms", true, false, func(d *Data, _ *Facts) { d.LegalTerms = &trade }, "REFERENCE_INVALID at /args/data/legal_terms_ref/kind"},
		{"printed by quote-text/0.1.0", true, false, func(d *Data, _ *Facts) { d.DocumentTemplate = &template },
			"DEPENDENCY_NOT_SUPPORTED at /args/data/document_template_ref"},
		{"quote-text/0.2.0 without legal terms", false, false, nil, "DRAFT_NOT_READY at /args/data/legal_terms_ref"},
		{"the issuer's default under English law", false, true, func(_ *Data, f *Facts) {
			f.Configs[prcDefault] = Config{ContentHash: "h", Legal: &england}
		}, "DEPENDENCY_NOT_SUPPORTED at /args/data/issuer_ref"},
		{"the issuer's default retired", false, true, func(_ *Data, f *Facts) {
			c := f.Configs[prcDefault]
			c.Check = notActive
			f.Configs[prcDefault] = c
		}, "CONFIG_NOT_ACTIVE at /args/data/issuer_ref"},
		{"the issuer's default unknown", false, true, func(_ *Data, f *Facts) { delete(f.Configs, prcDefault) },
			"REFERENCE_INVALID at /args/data/issuer_ref"},
		{"the issuer's default printed by quote-text/0.1.0", false, true, func(d *Data, _ *Facts) { d.DocumentTemplate = &template },
			"DEPENDENCY_NOT_SUPPORTED at /args/data/document_template_ref"},
		{"the draft's own terms win over a broken default", true, true, func(_ *Data, f *Facts) { delete(f.Configs, prcDefault) }, "OK"},
	} {
		d, f := withLegal(c.own, c.def)
		if c.edit != nil {
			c.edit(&d, &f)
		}
		_, err := SealDraft("quote.1", 1, d, f, calculator(d, "0.1"), issuedAt)
		if got := code(err); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
