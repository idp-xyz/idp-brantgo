package document

import (
	"fmt"
	"slices"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
)

// The renderers this package implements and the one language they have a layout for (Python
// document.py and t03_config.py require both of a template they render): quote-text/0.1.0, Python's,
// and quote-text/0.2.0, the same with the legal_terms section (WBS S3-17).
const (
	RendererVersion    = "quote-text/0.1.0"
	RendererVersion020 = "quote-text/0.2.0"
	LanguageEn         = "en"
	SectionLegalTerms  = "legal_terms"
)

// Sections are the ten sections of a quote document template of quote-text/0.1.0 (POL
// QuoteSections); a template lists each once, in the order the document prints them.
func Sections() []string { return slices.Clone(sections) }

// SectionsFor returns the sections of a template of renderer version: the ten of quote-text/0.1.0,
// those and legal_terms for quote-text/0.2.0 (POL QuoteSectionsFor), nil for another version.
func SectionsFor(version string) []string {
	switch version {
	case RendererVersion:
		return Sections()
	case RendererVersion020:
		return append(Sections(), SectionLegalTerms)
	}
	return nil
}

var sections = []string{"issuer", "recipient", "identity", "lines", "totals", "terms", "delivery", "payments", "validity", "notes"}

// Template is a POL quote_document_template revision as the formalization read it: its reference
// and content hash, which must be the revision and hash the frozen document's dependency manifest
// pins, and its data (POL api QuoteDocumentTemplate).
type Template struct {
	Ref             quote.Ref
	ContentHash     string
	Language        string
	RendererVersion string
	Title           string
	Sections        []string
	FooterNote      string
}

// Supported refuses a template neither renderer can render, in Python's order: another renderer
// version (DEPENDENCY_NOT_SUPPORTED), sections that are not its version's each once
// (REFERENCE_INVALID, at the first unknown or repeated one), another language than en
// (DEPENDENCY_NOT_SUPPORTED).
func (t Template) Supported() error {
	want := SectionsFor(t.RendererVersion)
	if want == nil {
		return fail(CodeDependencyNotSupported, "/template/renderer_version", "Only the quote-text/0.1.0 and 0.2.0 renderers are supported")
	}
	if len(t.Sections) != len(want) {
		return fail(refInvalid, "/template/sections", "A quote template lists its renderer's sections once each")
	}
	seen := map[string]bool{}
	for i, s := range t.Sections {
		if !slices.Contains(want, s) || seen[s] {
			return fail(refInvalid, fmt.Sprintf("/template/sections/%d", i), "A quote template lists its renderer's sections once each")
		}
		seen[s] = true
	}
	if t.Language != LanguageEn {
		return fail(CodeDependencyNotSupported, "/template/language", t.RendererVersion+" has a layout for en only")
	}
	return nil
}
