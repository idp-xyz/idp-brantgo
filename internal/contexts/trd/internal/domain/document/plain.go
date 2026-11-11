package document

import (
	"slices"
	"strings"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// DraftBanner opens every document rendered from a plain-text template: such a document is a
// business draft, never bound to an active quote (Python foundation.document_render).
const DraftBanner = "DRAFT / 业务草稿（未绑定已生效报价）\n"

// MaxPlainBytes bounds what a plain-text template renders after the banner (Python
// document_render: 10 MiB less 200 bytes, the business file limit with room for the banner).
const MaxPlainBytes = 10<<20 - 200

// FieldDocumentNumber is the field the issued number fills.
const FieldDocumentNumber = "document.number"

// TemplateFields are the fields a plain-text template may name (POL api TemplateFields).
func TemplateFields() []string { return slices.Clone(templateFields) }

var templateFields = []string{"company.name", "customer.name", FieldDocumentNumber, "document.date", "lines.summary", "quote.total", "quote.currency"}

// PlainTemplate is a POL document_template revision: literal text with "{{" and "}}" for braces
// and "{field}" placeholders naming exactly the fields of FieldNames.
type PlainTemplate struct {
	DocumentType string
	Text         string
	FieldNames   []string
}

// Number is an issued document number and the document type of its sequence.
type Number struct {
	Value        string
	DocumentType string
}

// RenderPlain renders a plain-text template with an issued number and the values of its fields
// (Python foundation.document_render, the rendering half of PYT-0135): DraftBanner, then the text
// with each placeholder replaced by its value verbatim and doubled braces made single, filed as
// "<number>.txt", text/plain. The number fills document.number. Refusals, in Python's order after
// the template's own form: a template that does not parse or names other fields than it declares
// (REFERENCE_INVALID at /template/template_text or /template/field_names: POL refuses such a
// template with TEMPLATE_INVALID when it is saved), a number of another document type
// (REFERENCE_INVALID at /template/document_type) or not well formed (REFERENCE_INVALID at
// /number), a document.number value other than the number (INVALID_KNOWN_VALUE), a declared field
// without a value (DRAFT_NOT_READY at /values/<field>), a value that is not UTF-8
// (INVALID_KNOWN_VALUE) and more than MaxPlainBytes (CALCULATION_LIMIT_EXCEEDED; Python says
// FILE_INVALID, a code the error catalog gives EVD and POL only).
func RenderPlain(t PlainTemplate, n Number, values map[string]string) (*File, error) {
	segments, ok := parsePlain(t.Text)
	if !ok {
		return nil, fail(refInvalid, "/template/template_text", "The template text does not parse")
	}
	var used []string
	for _, s := range segments {
		if s.field != "" {
			used = append(used, s.field)
		}
	}
	if !slices.Equal(slices.Compact(slices.Sorted(slices.Values(used))), slices.Compact(slices.Sorted(slices.Values(t.FieldNames)))) {
		return nil, fail(refInvalid, "/template/field_names", "The template names exactly its declared fields")
	}
	switch {
	case t.DocumentType != n.DocumentType:
		return nil, fail(refInvalid, "/template/document_type", "The number is of another document type")
	case !validNumber(n.Value):
		return nil, fail(refInvalid, "/number", "A document number is 1-64 letters, digits, '.', '_' or '-'")
	}
	if v, ok := values[FieldDocumentNumber]; ok && v != n.Value {
		return nil, fail(kernel.CodeInvalidKnownValue, "/values/"+FieldDocumentNumber, "The number fills document.number")
	}
	filled := map[string]string{FieldDocumentNumber: n.Value}
	for _, field := range t.FieldNames {
		if field == FieldDocumentNumber {
			continue
		}
		v, ok := values[field]
		switch {
		case !ok:
			return nil, fail(kernel.CodeDraftNotReady, "/values/"+field, "Every declared field has a value")
		case !utf8.ValidString(v):
			return nil, fail(kernel.CodeInvalidKnownValue, "/values/"+field, "")
		}
		filled[field] = v
	}
	var b strings.Builder
	b.WriteString(DraftBanner)
	for _, s := range segments {
		b.WriteString(s.literal)
		b.WriteString(filled[s.field])
		if b.Len()-len(DraftBanner) > MaxPlainBytes {
			return nil, fail(CodeCalculationLimitExceeded, "/values", "The document exceeds the business file limit")
		}
	}
	f := newFile(n.Value, []byte(b.String()))
	return &f, nil
}

// segment is literal text followed by a field ("" for none).
type segment struct{ literal, field string }

// parsePlain parses a plain-text template as POL's TemplateFieldNames does: literal text, "{{" and
// "}}" for literal braces and "{field}" placeholders, each one of TemplateFields; anything else
// (a format specification, conversion, attribute or index, an unbalanced brace, text that is not
// UTF-8) is not a template. Each placeholder ends a segment, as Python's string.Formatter().parse
// splits the text.
func parsePlain(text string) ([]segment, bool) {
	if !utf8.ValidString(text) {
		return nil, false
	}
	var out []segment
	var literal strings.Builder
	for i := 0; i < len(text); {
		switch text[i] {
		case '{':
			if i+1 < len(text) && text[i+1] == '{' {
				literal.WriteByte('{')
				i += 2
				continue
			}
			end := strings.IndexByte(text[i+1:], '}')
			if end < 0 {
				return nil, false
			}
			field := text[i+1 : i+1+end]
			if !slices.Contains(templateFields, field) {
				return nil, false
			}
			out = append(out, segment{literal: literal.String(), field: field})
			literal.Reset()
			i += end + 2
		case '}':
			if i+1 < len(text) && text[i+1] == '}' {
				literal.WriteByte('}')
				i += 2
				continue
			}
			return nil, false
		default:
			literal.WriteByte(text[i])
			i++
		}
	}
	if literal.Len() > 0 {
		out = append(out, segment{literal: literal.String()})
	}
	return out, true
}
