package document

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What a formal quote document is filed as (Python t03_storage.activate: "<number>.txt",
// text/plain).
const (
	MimeText          = "text/plain"
	DocumentTypeQuote = "quote"
	fileExtension     = ".txt"
	maxIdentifier     = 64 // characters of an identifier (kernel.ValidIdentifier), a number and a business reference
)

// Revision is a frozen quote revision as stored: its commercial document, byte for byte, and its
// business hash.
type Revision struct {
	Commercial   []byte
	BusinessHash string
}

// NumberRequest is the number a formalization asks POL for before its transaction (POL
// number.issue): from the number sequence revision the frozen document names, for the business
// reference of the revision, which keeps its number on every retry.
type NumberRequest struct {
	Sequence     quote.Ref
	DocumentType string
	BusinessRef  string
}

// Plan is a formalization checked up to the number: Formalize succeeds for every well-formed
// number POL issues.
type Plan struct {
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	Number           NumberRequest
	Template         quote.Ref
	TemplateHash     string
	template         Template
	sheet            sheet
}

// Prepare checks a frozen revision, the template and the facts for a formal document, in this
// order: the stored document is the business-hashed one in its normalized canonical form
// (BUSINESS_HASH_MISMATCH), it has a server calculation that fits it (REFERENCE_INVALID, then
// Calculation.Check), the template is the revision the document names and pins
// (REFERENCE_INVALID, REFERENCE_STALE), one the renderer supports (Template.Supported) and one
// that prints the legal terms exactly when the document seals them (quote-text/0.2.0;
// DEPENDENCY_NOT_SUPPORTED for 0.1.0 with legal terms, REFERENCE_INVALID for 0.2.0 without), the
// facts are those of the references the document binds, the text is UTF-8 (REFERENCE_INVALID) and
// the document fits MaxDocumentBytes with the longest number (CALCULATION_LIMIT_EXCEEDED). Nothing
// is rendered for good yet: the number is still to be issued.
func Prepare(r Revision, t Template, f Facts) (*Plan, error) {
	c, err := quote.ParseCommercial(r.Commercial, r.BusinessHash)
	if err != nil {
		return nil, err
	}
	if !normalized(r.Commercial) {
		return nil, fail(CodeBusinessHashMismatch, "/business_hash", "The stored document is not its normalized canonical form")
	}
	if c.Calculation == nil {
		return nil, fail(refInvalid, "/calculation", "The revision has no server calculation")
	}
	if err = c.Calculation.Check(c.Data); err != nil {
		return nil, err
	}
	if t.Ref != c.Data.DocumentTemplate {
		return nil, fail(refInvalid, "/data/document_template_ref", "The template is not the revision the document names")
	}
	if err = pinned(c, t.Ref, t.ContentHash, "/data/document_template_ref"); err != nil {
		return nil, err
	}
	if err = t.Supported(); err != nil {
		return nil, err
	}
	switch {
	case c.LegalTerms != nil && t.RendererVersion == RendererVersion:
		return nil, fail(CodeDependencyNotSupported, "/template/renderer_version", "quote-text/0.1.0 prints no legal terms; the document seals them")
	case c.LegalTerms == nil && t.RendererVersion == RendererVersion020:
		return nil, fail(refInvalid, "/legal_terms", "quote-text/0.2.0 prints the legal terms of a document that seals them")
	}
	s, err := compose(c, r.BusinessHash, f)
	if err != nil {
		return nil, err
	}
	longest := s
	longest.number = strings.Repeat("0", maxIdentifier)
	switch body := text(t, longest); {
	case !utf8.Valid(body):
		return nil, fail(refInvalid, "/", "The template and the facts are UTF-8 text")
	case len(body) > MaxDocumentBytes:
		return nil, fail(CodeCalculationLimitExceeded, "/", "A quote document has at most 1 MiB")
	}
	return &Plan{QuoteID: c.QuoteID, BusinessRevision: c.BusinessRevision, BusinessHash: r.BusinessHash,
		Number:   NumberRequest{Sequence: c.Data.Sequence, DocumentType: DocumentTypeQuote, BusinessRef: BusinessRef(c.QuoteID, c.BusinessRevision)},
		Template: t.Ref, TemplateHash: t.ContentHash, template: t, sheet: s}, nil
}

// normalized reports whether doc is its own normalized canonical form, the form a freeze stores
// and hashes.
func normalized(doc []byte) bool {
	tree, err := quote.Tree(doc)
	if err != nil {
		return false
	}
	canonical, _, err := quote.Digest(tree)
	return err == nil && bytes.Equal(canonical, doc)
}

// BusinessRef is the business reference a revision's number is issued for:
// "<quote_id>:<business_revision>" (Python t03_storage.activate), or, when that exceeds an
// identifier's 64 characters, "quote-" and the first 58 hex digits of its SHA-256. A business
// reference of the first form always has a ':' and the second never does, so they never collide.
func BusinessRef(quoteID string, revision int64) string {
	ref := quoteID + ":" + strconv.FormatInt(revision, 10)
	if len(ref) <= maxIdentifier {
		return ref
	}
	return "quote-" + quote.Hash([]byte(ref))[:58]
}

// File is a business file: its name, MIME type, bytes, SHA-256 in lower-case hex and size.
type File struct {
	Name     string
	MimeType string
	Content  []byte
	SHA256   string
	Size     int
}

func newFile(number string, content []byte) File {
	return File{Name: number + fileExtension, MimeType: MimeText, Content: content, SHA256: quote.Hash(content), Size: len(content)}
}

// Formal is a rendered formal document: the file to store and register and the binding of number,
// file and template to the revision.
type Formal struct {
	File    File
	Binding Binding
}

// Formalize renders the plan's document with number, the number POL issued for Number. A number
// that is not 1–64 characters of letters, digits, '.', '_' and '-', starting with a letter or
// digit, is REFERENCE_INVALID at /number; nothing else can fail.
func (p *Plan) Formalize(number string) (*Formal, error) {
	if !validNumber(number) {
		return nil, fail(refInvalid, "/number", "A document number is 1-64 letters, digits, '.', '_' or '-'")
	}
	s := p.sheet
	s.number = number
	file := newFile(number, text(p.template, s))
	return &Formal{File: file, Binding: Binding{QuoteID: p.QuoteID, BusinessRevision: p.BusinessRevision, BusinessHash: p.BusinessHash,
		Number: number, Sequence: p.Number.Sequence, BusinessRef: p.Number.BusinessRef, Template: p.Template, TemplateHash: p.TemplateHash,
		RendererVersion: p.template.RendererVersion, FileName: file.Name, MimeType: file.MimeType, ContentSHA256: file.SHA256,
		SizeBytes: file.Size}}, nil
}

func validNumber(n string) bool { return kernel.ValidIdentifier(n) && !strings.Contains(n, ":") }

// Binding binds a formal document to its revision (QuoteDocumentBinding, TRD-INV-009): the
// number, the file and the template, each with its identity, against the business hash. The
// formalization stores it with the bytes in its transaction; at most one binding exists per
// revision.
type Binding struct {
	QuoteID          string    `json:"quote_id"`
	BusinessRevision int64     `json:"business_revision"`
	BusinessHash     string    `json:"business_hash"`
	Number           string    `json:"number"`
	Sequence         quote.Ref `json:"sequence_ref"`
	BusinessRef      string    `json:"business_ref"`
	Template         quote.Ref `json:"template_ref"`
	TemplateHash     string    `json:"template_content_hash"`
	RendererVersion  string    `json:"renderer_version"`
	FileName         string    `json:"file_name"`
	MimeType         string    `json:"mime_type"`
	ContentSHA256    string    `json:"content_sha256"`
	SizeBytes        int       `json:"size_bytes"`
}

// Entry is one named value of a commit evidence record.
type Entry struct {
	Name  string
	Value string
}

// Entries are the binding's entries of the commit evidence the formalization appends (TRD-INV-016;
// Python t03_commit_evidence: quote, revision, business hash, number and file SHA-256, and A19's
// template binding), in a fixed order. The command adds its own: the action, the approval, the
// authority and control epochs and the commit instant.
func (b Binding) Entries() []Entry {
	return []Entry{
		{"quote_id", b.QuoteID},
		{"business_revision", strconv.FormatInt(b.BusinessRevision, 10)},
		{"business_hash", b.BusinessHash},
		{"number", b.Number},
		{"sequence_ref", b.Sequence.String()},
		{"business_ref", b.BusinessRef},
		{"template_ref", b.Template.String()},
		{"template_content_hash", b.TemplateHash},
		{"renderer_version", b.RendererVersion},
		{"file_name", b.FileName},
		{"mime_type", b.MimeType},
		{"content_sha256", b.ContentSHA256},
		{"size_bytes", strconv.Itoa(b.SizeBytes)},
	}
}

// Verify checks a stored formal document on read (TRD-INV-009) for the revision (quoteID,
// revision, businessHash): a binding of another revision, or none, is DOCUMENT_BINDING_MISSING (a
// later revision never borrows an earlier one's document, BC-AT22); a binding of the revision with
// another business hash BUSINESS_HASH_MISMATCH; no bytes FORMAL_DOCUMENT_MISSING; bytes of another
// size or hash FORMAL_DOCUMENT_INTEGRITY_FAILURE.
func Verify(b *Binding, content []byte, quoteID string, revision int64, businessHash string) error {
	switch {
	case b == nil || b.QuoteID != quoteID || b.BusinessRevision != revision:
		return fail(CodeDocumentBindingMissing, "/document_binding", "The revision has no formal document binding of its own")
	case b.BusinessHash != businessHash:
		return fail(CodeBusinessHashMismatch, "/document_binding/business_hash", "The binding is for other business content")
	case len(content) == 0:
		return fail(CodeFormalDocumentMissing, "/document", "")
	case len(content) != b.SizeBytes || quote.Hash(content) != b.ContentSHA256:
		return fail(CodeFormalDocumentIntegrityFailure, "/document", "The stored bytes are not the bound document")
	}
	return nil
}
