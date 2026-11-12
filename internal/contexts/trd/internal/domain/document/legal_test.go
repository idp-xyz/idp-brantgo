package document

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
)

var (
	hamburg     = quote.Ref{Kind: quote.KindLegalTerms, ID: "legal.de-hamburg", Revision: 1}
	legalLayout = quote.Ref{Kind: quote.KindQuoteDocumentTemplate, ID: "quote.legal", Revision: 1}
)

// withLegalTerms seals the fixture under terms, named by the draft, with the quote-text/0.2.0
// template.
func withLegalTerms(terms quote.Legal) func(d *quote.Data, f *quote.Facts) {
	return func(d *quote.Data, f *quote.Facts) {
		d.LegalTerms, d.DocumentTemplate = &hamburg, &legalLayout
		f.Configs[hamburg] = quote.Config{ContentHash: hashOf("hamburg"), Legal: &terms}
		f.Configs[legalLayout] = quote.Config{ContentHash: hashOf("legal layout"), Renderer: quote.QuoteRendererV020}
	}
}

// legalTemplate is the fixture's template at quote-text/0.2.0, listing legal_terms last.
func legalTemplate() Template {
	t := template()
	t.Ref, t.ContentHash, t.RendererVersion, t.Sections = legalLayout, hashOf("legal layout"), RendererVersion020, SectionsFor(RendererVersion020)
	return t
}

var hamburgArbitration = quote.Legal{GoverningLaw: "DE", CISG: quote.CISGApplies, FormationRule: quote.FormationArrival,
	DisputeResolution: quote.DisputeResolution{Method: quote.DisputeArbitration,
		Institution: "Court of Arbitration of the Hamburg Chamber of Commerce", Seat: "Hamburg", Language: "de"}}

// WBS S3-17 (SIM-02 step 3; G0 F-02): quote-text/0.2.0 prints the legal terms a revision seals in
// its legal_terms section — the revision, the governing law and the CISG choice, the formation rule
// with its legal basis, and the dispute resolution — and is otherwise quote-text/0.1.0: the
// document up to the legal terms is the 0.1.0 rendering of the same revision. Its binding names
// quote-text/0.2.0. The bytes are pinned (SHA-256) and the same on every run.
func TestQuoteText020PrintsTheLegalTerms(t *testing.T) {
	r := seal(t, withLegalTerms(hamburgArbitration))
	f := formalize(t, r, legalTemplate(), facts(), "SYN-Q-2026-00001")
	block := "\n\nLegal terms: legal.de-hamburg r1\nGoverning law: DE | CISG: applies\n" +
		"Contract formation: acceptance effective on arrival | CISG Art. 18(2), 23\n" +
		"Disputes: arbitration | Court of Arbitration of the Hamburg Chamber of Commerce | seat Hamburg | language de\n"
	text := string(f.File.Content)
	if !strings.HasSuffix(text, block) || f.Binding.RendererVersion != RendererVersion020 {
		t.Fatalf("%s\nbinding %+v", text, f.Binding)
	}
	// Without the legal terms section the document is quote-text/0.1.0's rendering of the revision.
	plain := legalTemplate()
	plain.RendererVersion, plain.Sections = RendererVersion, Sections()
	p, err := Prepare(r, legalTemplate(), facts())
	if err != nil {
		t.Fatal(err)
	}
	p.template = plain
	without, _ := p.Formalize("SYN-Q-2026-00001")
	if string(without.File.Content) != strings.TrimSuffix(text, block)+"\n" {
		t.Fatalf("the rest of the document differs:\n%s", without.File.Content)
	}
	const pinned = "6a0e2f31d9e63405721b24e49cb9d7034f20166f365b548bfae81330df27d05d"
	if f.File.SHA256 != pinned {
		t.Errorf("quote-text/0.2.0 bytes changed: %s\n%s", f.File.SHA256, text)
	}
	again := formalize(t, seal(t, withLegalTerms(hamburgArbitration)), legalTemplate(), facts(), "SYN-Q-2026-00001")
	if !bytes.Equal(again.File.Content, f.File.Content) {
		t.Fatal("rendering is not deterministic")
	}
	// The template orders the section like any other.
	first := legalTemplate()
	first.Sections = append([]string{SectionLegalTerms}, Sections()...)
	g := formalize(t, r, first, facts(), "SYN-Q-2026-00001")
	if !strings.HasPrefix(string(g.File.Content), first.Title+"\n\nLegal terms: legal.de-hamburg r1\n") {
		t.Fatalf("%s", g.File.Content)
	}

	// PRC law without the CISG, before a court.
	prc := quote.Legal{GoverningLaw: "CN", CISG: quote.CISGExcluded, FormationRule: quote.FormationArrival,
		DisputeResolution: quote.DisputeResolution{Method: quote.DisputeLitigation, Court: "Shanghai International Commercial Court"}}
	h := formalize(t, seal(t, withLegalTerms(prc)), legalTemplate(), facts(), "SYN-Q-2026-00002")
	if !strings.HasSuffix(string(h.File.Content), "\n\nLegal terms: legal.de-hamburg r1\nGoverning law: CN | CISG: excluded\n"+
		"Contract formation: acceptance effective on arrival | PRC Civil Code Art. 137, 483, 484\n"+
		"Disputes: litigation | Shanghai International Commercial Court\n") {
		t.Fatalf("%s", h.File.Content)
	}
}

// A document prints the legal terms exactly when it seals them: quote-text/0.1.0 refuses a revision
// with legal terms (it would drop them), quote-text/0.2.0 one without (it has none to print); a
// 0.2.0 template lists the ten sections and legal_terms once each.
func TestRendererAndLegalTermsArePaired(t *testing.T) {
	legal := seal(t, withLegalTerms(hamburgArbitration))
	old := template()
	old.Ref, old.ContentHash = legalLayout, hashOf("legal layout")
	if _, err := Prepare(legal, old, facts()); code(err) != "DEPENDENCY_NOT_SUPPORTED at /template/renderer_version" {
		t.Fatalf("0.1.0 with legal terms: %s", code(err))
	}
	plain := legalTemplate()
	plain.Ref, plain.ContentHash = templateRef, hashOf("template")
	if _, err := Prepare(seal(t, nil), plain, facts()); code(err) != "REFERENCE_INVALID at /legal_terms" {
		t.Fatalf("0.2.0 without legal terms: %s", code(err))
	}
	ten := legalTemplate()
	ten.Sections = Sections()
	if code(ten.Supported()) != "REFERENCE_INVALID at /template/sections" {
		t.Fatal("a 0.2.0 template lists eleven sections")
	}
	twice := legalTemplate()
	twice.Sections[0] = SectionLegalTerms
	if code(twice.Supported()) != "REFERENCE_INVALID at /template/sections/10" {
		t.Fatal(code(twice.Supported()))
	}
	if !slices.Equal(SectionsFor(RendererVersion), Sections()) || len(SectionsFor(RendererVersion020)) != 11 || SectionsFor("x") != nil {
		t.Fatal("sections by renderer")
	}
	// Legal terms a freeze could not have sealed are refused, never printed.
	for name, edit := range map[string]func(c *quote.Commercial){
		"another rule":     func(c *quote.Commercial) { c.LegalTerms.Formation.Rule = quote.FormationDispatch },
		"no dispute seat":  func(c *quote.Commercial) { c.LegalTerms.DisputeResolution.Seat = "" },
		"no governing law": func(c *quote.Commercial) { c.LegalTerms.GoverningLaw = "" },
		"unpinned revision": func(c *quote.Commercial) {
			c.Data.LegalTerms = &quote.Ref{Kind: quote.KindLegalTerms, ID: "other", Revision: 1}
		},
		"no revision at all": func(c *quote.Commercial) { c.Data.LegalTerms = nil },
	} {
		c, err := quote.ParseCommercial(legal.Commercial, legal.BusinessHash)
		if err != nil {
			t.Fatal(err)
		}
		edit(&c)
		if _, err = legalLines(c); err == nil || !strings.HasPrefix(code(err), "REFERENCE_INVALID at /") {
			t.Errorf("%s: %s", name, code(err))
		}
	}
}
