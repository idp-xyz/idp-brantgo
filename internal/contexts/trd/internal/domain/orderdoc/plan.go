package orderdoc

import (
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What an order document is filed as.
const (
	MimeText         = "text/plain"
	MaxDocumentBytes = 1 << 20
	fileExtension    = ".txt"
	maxIdentifier    = 64
)

// NumberRequest is the number a generation asks POL for before its transaction (number.issue):
// from the sequence the command names, of the document kind's type, for the order (BusinessRef is
// the order ID: one number per order and kind, kept by every revision).
type NumberRequest struct {
	Sequence     quote.Ref
	DocumentType string
	BusinessRef  string
}

// Plan is a generation checked up to the number: Generate succeeds for every well-formed number
// POL issues.
type Plan struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
	DocumentKind  string
	Number        NumberRequest
	Template      quote.Ref
	TemplateHash  string
	GeneratedAt   time.Time
	template      Template
	sheet         sheet
}

// Prepare checks order revision r, template t and the facts for a document generated at instant
// at, in this order: the template is one the renderer supports (Template.Supported); it prints the
// law section exactly when the revision carries legal terms (REFERENCE_INVALID); the sequence issues numbers of
// the kind's document type (REFERENCE_INVALID at /facts/sequence); the facts are those of the
// references the revision binds and the revision is whole (REFERENCE_INVALID); the text is UTF-8
// (REFERENCE_INVALID) and fits MaxDocumentBytes with the longest number
// (CALCULATION_LIMIT_EXCEEDED). Nothing is rendered for good yet: the number is still to be issued.
func Prepare(r *orderrev.Revision, t Template, f Facts, at time.Time) (*Plan, error) {
	if r == nil || len(r.Lines) == 0 || !kernel.ValidIdentifier(r.OrderID) || r.Number <= 0 || len(r.Hash) != 64 {
		return nil, fail(kernel.CodeReferenceInvalid, "/revision", "The order revision is needed whole")
	}
	if err := t.Supported(); err != nil {
		return nil, err
	}
	law, bound := slices.Contains(t.Sections, SectionLaw), r.LegalTerms != nil && r.LegalTermsRef != nil
	switch {
	case law && !bound:
		return nil, fail(kernel.CodeReferenceInvalid, "/template/sections", "The order binds no legal terms; use a template without the law section")
	case !law && bound:
		return nil, fail(kernel.CodeReferenceInvalid, "/template/sections", "The order binds legal terms; its documents print them")
	}
	if f.Sequence.Ref.Kind != quote.KindNumberSequence || f.Sequence.DocumentType != DocumentType(t.DocumentKind) {
		return nil, fail(kernel.CodeReferenceInvalid, "/facts/sequence", "A "+t.DocumentKind+" is numbered from a sequence of document type "+
			DocumentType(t.DocumentKind))
	}
	at = kernel.Instant(at)
	if at.IsZero() {
		return nil, fail(kernel.CodeReferenceInvalid, "/generated_at", "A document has its generation instant")
	}
	s, err := compose(r, t, f, at)
	if err != nil {
		return nil, err
	}
	longest := s
	longest.number = strings.Repeat("0", maxIdentifier)
	switch body := text(t, longest); {
	case !utf8.Valid(body):
		return nil, fail(kernel.CodeReferenceInvalid, "/", "The template and the facts are UTF-8 text")
	case len(body) > MaxDocumentBytes:
		return nil, fail(CodeCalculationLimitExceeded, "/", "An order document has at most 1 MiB")
	}
	return &Plan{OrderID: r.OrderID, OrderRevision: r.Number, OrderHash: r.Hash, DocumentKind: t.DocumentKind,
		Number:   NumberRequest{Sequence: f.Sequence.Ref, DocumentType: DocumentType(t.DocumentKind), BusinessRef: r.OrderID},
		Template: t.Ref, TemplateHash: t.ContentHash, GeneratedAt: at, template: t, sheet: s}, nil
}

// File is a business file: its name, MIME type, bytes, SHA-256 in lower-case hex and size.
type File struct {
	Name     string
	MimeType string
	Content  []byte
	SHA256   string
	Size     int
}

// Generated is a rendered order document: the file to store and register, and its binding.
type Generated struct {
	File    File
	Binding Binding
}

// Generate renders the plan's document with number, the number POL issued for Number: a number
// that is not 1–64 letters, digits, '.', '_' or '-' starting with a letter or digit is
// REFERENCE_INVALID at /number; nothing else can fail. The file is "<number>-r<order revision>.txt".
func (p *Plan) Generate(number string) (*Generated, error) {
	if !kernel.ValidIdentifier(number) || strings.Contains(number, ":") {
		return nil, fail(kernel.CodeReferenceInvalid, "/number", "A document number is 1-64 letters, digits, '.', '_' or '-'")
	}
	s := p.sheet
	s.number = number
	content := text(p.template, s)
	file := File{Name: number + "-r" + strconv.FormatInt(p.OrderRevision, 10) + fileExtension, MimeType: MimeText, Content: content,
		SHA256: quote.Hash(content), Size: len(content)}
	return &Generated{File: file, Binding: Binding{OrderID: p.OrderID, OrderRevision: p.OrderRevision, OrderHash: p.OrderHash,
		DocumentKind: p.DocumentKind, Number: number, Sequence: p.Number.Sequence, BusinessRef: p.Number.BusinessRef, Template: p.Template,
		TemplateHash: p.TemplateHash, RendererVersion: RendererVersion, GeneratedAt: kernel.FormatTimestamp(p.GeneratedAt), FileName: file.Name,
		MimeType: file.MimeType, ContentSHA256: file.SHA256, SizeBytes: file.Size}}, nil
}

// Binding binds an order document to its order revision (SalesContractDocumentRef v1, TRD-INV-020):
// the document kind, the number, the file and the template, each with its identity, against the
// order hash. At most one binding of a kind exists per order revision.
type Binding struct {
	OrderID         string    `json:"order_id"`
	OrderRevision   int64     `json:"order_revision"`
	OrderHash       string    `json:"order_hash"`
	DocumentKind    string    `json:"document_kind"`
	Number          string    `json:"number"`
	Sequence        quote.Ref `json:"sequence_ref"`
	BusinessRef     string    `json:"business_ref"`
	Template        quote.Ref `json:"template_ref"`
	TemplateHash    string    `json:"template_content_hash"`
	RendererVersion string    `json:"renderer_version"`
	GeneratedAt     string    `json:"generated_at"`
	FileName        string    `json:"file_name"`
	MimeType        string    `json:"mime_type"`
	ContentSHA256   string    `json:"content_sha256"`
	SizeBytes       int       `json:"size_bytes"`
}

// Entries are the binding's entries of the commit evidence order.document.generate appends, in a
// fixed order; the command adds its own (the action, the epochs and the commit instant).
func (b Binding) Entries() []quote.Entry {
	return []quote.Entry{
		{Name: "order_id", Value: b.OrderID},
		{Name: "order_revision", Value: strconv.FormatInt(b.OrderRevision, 10)},
		{Name: "order_hash", Value: b.OrderHash},
		{Name: "document_kind", Value: b.DocumentKind},
		{Name: "number", Value: b.Number},
		{Name: "sequence_ref", Value: b.Sequence.String()},
		{Name: "business_ref", Value: b.BusinessRef},
		{Name: "template_ref", Value: b.Template.String()},
		{Name: "template_content_hash", Value: b.TemplateHash},
		{Name: "renderer_version", Value: b.RendererVersion},
		{Name: "generated_at", Value: b.GeneratedAt},
		{Name: "file_name", Value: b.FileName},
		{Name: "mime_type", Value: b.MimeType},
		{Name: "content_sha256", Value: b.ContentSHA256},
		{Name: "size_bytes", Value: strconv.Itoa(b.SizeBytes)},
	}
}

// Verify checks stored bytes against their binding on every read, current or stale: no bytes, or
// bytes of another size or hash, are DOCUMENT_INTEGRITY_FAILURE.
func Verify(b *Binding, content []byte) error {
	switch {
	case b == nil:
		return fail(CodeDocumentBindingMissing, "/document_binding", "")
	case len(content) == 0 || len(content) != b.SizeBytes || quote.Hash(content) != b.ContentSHA256:
		return fail(CodeDocumentIntegrityFailure, "/document", "The stored bytes are not the bound document")
	}
	return nil
}

// Current checks that binding b is a kind document of order revision r, the order's current one
// (TRD-INV-020): no binding, or one of another order or kind, is DOCUMENT_BINDING_MISSING; one of
// another order revision or order hash ORDER_DOCUMENT_STALE (an amended order's old documents are
// read only; its new revision needs its own).
func Current(b *Binding, kind string, r *orderrev.Revision) error {
	switch {
	case b == nil || b.OrderID != r.OrderID || b.DocumentKind != kind:
		return fail(CodeDocumentBindingMissing, "/document_binding", "The order has no "+kind+" of its own")
	case b.OrderRevision != r.Number || b.OrderHash != r.Hash:
		return fail(CodeOrderDocumentStale, "/document_binding", "The "+kind+" binds order revision "+strconv.FormatInt(b.OrderRevision, 10)+
			"; the order is at revision "+strconv.FormatInt(r.Number, 10))
	}
	return nil
}

// Kinds of a signed copy.
const (
	Signed        = "signed"        // signed by us
	Countersigned = "countersigned" // signed back by the customer (回签)
)

// Signature is a signed copy of an order document (OrderDocumentSigned): its kind, the EVD business
// file holding it and that file's SHA-256, and who recorded it, when and on which evidence.
type Signature struct {
	Kind       string
	FileID     string
	FileSHA256 string
	Recorded   quote.Act
}

// Sign decides order.document.record_signed of signature s for the document b binds, given the
// order's current revision r and the signatures recorded for b so far: the document must be current
// (Current: ORDER_DOCUMENT_STALE once the order was amended), the kind known and the file named
// (SCHEMA_INVALID), the file not the generated, unsigned bytes (INVALID_KNOWN_VALUE:
// 草稿不能冒充已签署件), a human records it (PERMISSION_DENIED). A copy of a kind already recorded is a
// replay when it is the same file (replayed true) and INVALID_STATE otherwise: a recorded signed copy
// is immutable evidence.
func Sign(b *Binding, r *orderrev.Revision, s Signature, recorded []Signature) (bool, error) {
	if b == nil {
		return false, fail(CodeDocumentBindingMissing, "/document_binding", "")
	}
	if err := Current(b, b.DocumentKind, r); err != nil {
		return false, err
	}
	switch {
	case s.Kind != Signed && s.Kind != Countersigned:
		return false, kernel.Reject(kernel.CodeSchemaInvalid, "/args/signature_kind", "")
	case !kernel.ValidIdentifier(s.FileID) || len(s.FileSHA256) != 64:
		return false, kernel.Reject(kernel.CodeSchemaInvalid, "/args/file_id", "")
	case s.FileSHA256 == b.ContentSHA256:
		return false, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/file_id", "The generated, unsigned document is not a signed copy")
	case s.Recorded.By.Kind != kernel.ActorHuman:
		return false, kernel.Reject(kernel.CodePermissionDenied, "/", "A human records a signed copy")
	}
	for _, x := range recorded {
		if x.Kind != s.Kind {
			continue
		}
		if x.FileID == s.FileID && x.FileSHA256 == s.FileSHA256 {
			return true, nil
		}
		return false, kernel.Reject(kernel.CodeInvalidState, "/args/signature_kind", "The "+s.Kind+" copy of "+b.Number+" is recorded; it never changes")
	}
	return false, nil
}
