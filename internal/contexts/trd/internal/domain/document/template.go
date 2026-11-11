package document

import (
	"fmt"
	"slices"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
)

// The renderer this package implements and the one language it has a layout for (Python
// document.py and t03_config.py require both of a template they render).
const (
	RendererVersion = "quote-text/0.1.0"
	LanguageEn      = "en"
)

// Sections are the ten sections of a quote document template (POL QuoteSections); a template
// lists each once, in the order the document prints them.
func Sections() []string { return slices.Clone(sections) }

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

// Supported refuses a template quote-text/0.1.0 cannot render, in Python's order: another renderer
// version (DEPENDENCY_NOT_SUPPORTED), sections that are not the ten each once (REFERENCE_INVALID, at
// the first unknown or repeated one), another language than en (DEPENDENCY_NOT_SUPPORTED).
func (t Template) Supported() error {
	if t.RendererVersion != RendererVersion {
		return fail(CodeDependencyNotSupported, "/template/renderer_version", "Only the quote-text/0.1.0 renderer is supported")
	}
	if len(t.Sections) != len(sections) {
		return fail(refInvalid, "/template/sections", "A quote template lists the ten sections once each")
	}
	seen := map[string]bool{}
	for i, s := range t.Sections {
		if !slices.Contains(sections, s) || seen[s] {
			return fail(refInvalid, fmt.Sprintf("/template/sections/%d", i), "A quote template lists the ten sections once each")
		}
		seen[s] = true
	}
	if t.Language != LanguageEn {
		return fail(CodeDependencyNotSupported, "/template/language", "quote-text/0.1.0 has a layout for en only")
	}
	return nil
}
