package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TRD-INV-009 on read (BC-AT22; the database CHECK keeps stored bytes and their hash together, so a
// corruption the storage itself suffers is what this guards against): the stored bytes are checked
// against the revision's own binding — the bound bytes pass; other bytes are
// FORMAL_DOCUMENT_INTEGRITY_FAILURE, none FORMAL_DOCUMENT_MISSING, a binding of other business
// content BUSINESS_HASH_MISMATCH, a revision without a binding DOCUMENT_BINDING_MISSING.
func TestStoredDocumentIsVerifiedOnRead(t *testing.T) {
	content := []byte("Quotation\n\nNumber: QT-2026-00001\n")
	hash := quote.Hash([]byte("commercial"))
	formal := func(edit func(d *quote.Document)) *quote.Revision {
		d := quote.Document{Number: "QT-2026-00001", BusinessHash: hash, FileName: "QT-2026-00001.txt", MimeType: "text/plain",
			ContentSHA256: quote.Hash(content), SizeBytes: len(content)}
		if edit != nil {
			edit(&d)
		}
		return &quote.Revision{Number: 1, Status: quote.Active, Seal: &quote.Seal{BusinessHash: hash}, Formal: &quote.Formalization{Document: d}}
	}
	codeOf := func(err error) string {
		if rej, ok := kernel.AsRejection(err); ok {
			return rej.Code
		}
		if err != nil {
			return err.Error()
		}
		return "OK"
	}
	for name, c := range map[string]struct {
		rev     *quote.Revision
		content []byte
		want    string
	}{
		"the bound bytes":         {formal(nil), content, "OK"},
		"other bytes":             {formal(nil), []byte("Quotation\n\nNumber: QT-2026-00002\n"), "FORMAL_DOCUMENT_INTEGRITY_FAILURE"},
		"one byte more":           {formal(nil), append([]byte(nil), append(content, ' ')...), "FORMAL_DOCUMENT_INTEGRITY_FAILURE"},
		"no bytes":                {formal(nil), nil, "FORMAL_DOCUMENT_MISSING"},
		"another business hash":   {formal(func(d *quote.Document) { d.BusinessHash = quote.Hash([]byte("other")) }), content, "BUSINESS_HASH_MISMATCH"},
		"no binding of its own":   {&quote.Revision{Number: 1, Seal: &quote.Seal{BusinessHash: hash}}, content, "DOCUMENT_BINDING_MISSING"},
		"a binding of other size": {formal(func(d *quote.Document) { d.SizeBytes++ }), content, "FORMAL_DOCUMENT_INTEGRITY_FAILURE"},
	} {
		if got := codeOf(verifyDocument("quote.1", c.rev, c.content)); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	if v := formalView(nil); v != nil {
		t.Fatal("no formalization, no view")
	}
	v := formalView(&quote.Formalization{Document: quote.Document{Number: "QT-1"}, Commit: quote.Commit{Entries: []quote.Entry{{Name: "a", Value: "b"}}}})
	if v.Number != "QT-1" || v.Commit.EntriesSHA256 != quote.Hash([]byte(`[{"name":"a","value":"b"}]`)) {
		t.Fatalf("view %+v", v)
	}
}
