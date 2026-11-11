package orderdoc

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The renderer this package implements, the one language it has a layout for, the two document
// kinds and the POL number sequence document type of each.
const (
	RendererVersion = "order-text/0.1.0"
	LanguageEn      = "en"

	KindSalesContract   = "sales_contract"
	KindProformaInvoice = "proforma_invoice"

	DocumentTypeOrder           = "order"            // an S/C carries the order's contract number
	DocumentTypeProformaInvoice = "proforma_invoice" // a PI has its own numbers, never a commercial invoice's

	// KindOrderDocumentTemplate is the POL configuration kind of an order document template.
	KindOrderDocumentTemplate = "order_document_template"

	MaxClauses        = 50
	maxClauseCharsDoc = 2000
)

// Sections of order-text/0.1.0.
const (
	SectionSeller     = "seller"
	SectionBuyer      = "buyer"
	SectionIdentity   = "identity"
	SectionLines      = "lines"
	SectionTotals     = "totals"
	SectionTerms      = "terms"
	SectionDelivery   = "delivery"
	SectionPayments   = "payments"
	SectionClauses    = "clauses"
	SectionLaw        = "law"
	SectionSignatures = "signatures"
	SectionNotes      = "notes"
)

var (
	required = map[string][]string{
		KindSalesContract: {SectionSeller, SectionBuyer, SectionIdentity, SectionLines, SectionTotals, SectionTerms, SectionDelivery,
			SectionPayments, SectionClauses, SectionSignatures, SectionNotes},
		KindProformaInvoice: {SectionSeller, SectionBuyer, SectionIdentity, SectionLines, SectionTotals, SectionTerms, SectionDelivery,
			SectionPayments, SectionNotes},
	}
	optional = map[string][]string{
		KindSalesContract:   {SectionLaw},
		KindProformaInvoice: {SectionClauses, SectionLaw},
	}
)

// RequiredSections returns the sections a template of document kind kind lists once each (nil for
// an unknown kind), and OptionalSections those it may list at most once.
func RequiredSections(kind string) []string { return slices.Clone(required[kind]) }

// OptionalSections returns the sections a template of kind may list at most once.
func OptionalSections(kind string) []string { return slices.Clone(optional[kind]) }

// DocumentType returns the POL number sequence document type of document kind kind.
func DocumentType(kind string) string {
	if kind == KindSalesContract {
		return DocumentTypeOrder
	}
	return DocumentTypeProformaInvoice
}

// Template is a POL order_document_template revision as the command read it: its reference and
// content hash, and its data (POL api OrderDocumentTemplate).
type Template struct {
	Ref             quote.Ref
	ContentHash     string
	DocumentKind    string
	Language        string
	RendererVersion string
	Title           string
	Sections        []string
	Clauses         []string
	FooterNote      string
}

// Supported refuses a template order-text/0.1.0 cannot render: another kind of configuration
// (REFERENCE_INVALID at /template/ref), another renderer version (DEPENDENCY_NOT_SUPPORTED), an
// unknown document kind (REFERENCE_INVALID), sections that are not the kind's required ones once
// each plus optional ones at most once (REFERENCE_INVALID at the first offender), clauses that do
// not match the clauses section (REFERENCE_INVALID), a blank title (REFERENCE_INVALID), another
// language than en (DEPENDENCY_NOT_SUPPORTED). POL refuses such a template with SCHEMA_INVALID when
// it is saved; TRD checks again what it renders.
func (t Template) Supported() error {
	switch {
	case t.Ref.Kind != KindOrderDocumentTemplate || !kernel.ValidIdentifier(t.Ref.ID) || t.Ref.Revision <= 0:
		return fail(kernel.CodeReferenceInvalid, "/template/ref", "The template is not an order document template revision")
	case t.RendererVersion != RendererVersion:
		return fail(CodeDependencyNotSupported, "/template/renderer_version", "Only the order-text/0.1.0 renderer is supported")
	case required[t.DocumentKind] == nil:
		return fail(kernel.CodeReferenceInvalid, "/template/document_kind", "A template is of a sales contract or a proforma invoice")
	case strings.TrimSpace(t.Title) == "" || !utf8.ValidString(t.Title):
		return fail(kernel.CodeReferenceInvalid, "/template/title", "A template has a title")
	}
	if i, ok := CheckSections(t.DocumentKind, t.Sections); !ok {
		return fail(kernel.CodeReferenceInvalid, "/template/sections/"+strconv.Itoa(i), "A "+t.DocumentKind+
			" template lists its required sections once each and its optional ones at most once")
	}
	lists := slices.Contains(t.Sections, SectionClauses)
	switch {
	case lists && (len(t.Clauses) == 0 || len(t.Clauses) > MaxClauses):
		return fail(kernel.CodeReferenceInvalid, "/template/clauses", "A template that prints clauses has 1 to 50 of them")
	case !lists && len(t.Clauses) > 0:
		return fail(kernel.CodeReferenceInvalid, "/template/clauses", "A template without a clauses section has no clauses")
	}
	for i, c := range t.Clauses {
		if strings.TrimSpace(c) == "" || !utf8.ValidString(c) || utf8.RuneCountInString(c) > maxClauseCharsDoc {
			return fail(kernel.CodeReferenceInvalid, "/template/clauses/"+strconv.Itoa(i), "A clause is 1 to 2000 characters")
		}
	}
	if t.Language != LanguageEn {
		return fail(CodeDependencyNotSupported, "/template/language", "order-text/0.1.0 has a layout for en only")
	}
	return nil
}

// CheckSections reports whether sections are document kind kind's required sections once each and
// its optional ones at most once, in any order; when not, it returns the index of the first
// offender (len(sections) for a required section missing).
func CheckSections(kind string, sections []string) (int, bool) {
	need, may := required[kind], optional[kind]
	seen := map[string]bool{}
	for i, s := range sections {
		if seen[s] || (!slices.Contains(need, s) && !slices.Contains(may, s)) {
			return i, false
		}
		seen[s] = true
	}
	for _, s := range need {
		if !seen[s] {
			return len(sections), false
		}
	}
	return 0, need != nil
}
