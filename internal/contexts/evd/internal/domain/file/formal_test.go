package file

import (
	"errors"
	"strings"
	"testing"
)

var (
	hashA = strings.Repeat("a", 64)
	hashB = strings.Repeat("b", 64)
)

func sealedQuote(content string) Formal {
	return Formal{Kind: FormalQuote, QuoteID: "quote.1", BusinessRevision: 2, BusinessHash: hashA, ReplacedRevision: 1, Number: "QT-2026-00002",
		FileName: "QT-2026-00002.txt", MimeType: "text/plain", SHA256: Hash([]byte(content)), Size: len(content)}
}

// Context Map R65 (risk "EVD 登记的文件与 TRD 封存的字节不一致"), WBS S4-06c: EVD files a formal document
// only when TRD's read of it is the document the event announced and its bytes hash to the SHA-256
// TRD sealed; every other difference is a Mismatch.
func TestVerifyFilesOnlyTheSealedBytes(t *testing.T) {
	const content = "Quotation QT-2026-00002\n"
	sealed := sealedQuote(content)
	if err := Verify(sealed, sealed, []byte(content)); err != nil {
		t.Fatalf("the sealed document: %v", err)
	}
	order := Formal{Kind: FormalSalesContract, QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: hashA, OrderID: "order.1", OrderRevision: 2,
		OrderHash: hashB, Number: "SC-2026-00001", FileName: "SC-2026-00001-r2.txt", MimeType: "text/plain", SHA256: Hash([]byte(content)),
		Size: len(content)}
	if err := Verify(order, order, []byte(content)); err != nil {
		t.Fatalf("the sealed S/C: %v", err)
	}
	for _, c := range []struct {
		name    string
		sealed  Formal
		edit    func(*Formal)
		content string
		want    string
	}{
		{"other bytes", sealed, func(*Formal) {}, "Quotation QT-2026-00002 (edited)\n", "the SHA-256 of the bytes"},
		{"bytes of another size, same binding", sealed, func(*Formal) {}, content + " ", "the SHA-256 of the bytes"},
		{"another binding hash", sealed, func(f *Formal) { f.SHA256 = hashB }, content, "TRD's binding SHA-256"},
		{"another binding size", sealed, func(f *Formal) { f.Size++ }, content, "TRD's binding size"},
		{"another revision's document", sealed, func(f *Formal) { f.BusinessRevision = 3 }, content, "another document"},
		{"another quote", sealed, func(f *Formal) { f.QuoteID = "quote.2" }, content, "another document"},
		{"another business hash", sealed, func(f *Formal) { f.BusinessHash = hashB }, content, "the business hash"},
		{"another replaced revision", sealed, func(f *Formal) { f.ReplacedRevision = 0 }, content, "the replaced revision"},
		{"another number", sealed, func(f *Formal) { f.Number = "QT-2026-00009" }, content, "the number"},
		{"another file name", sealed, func(f *Formal) { f.FileName = "QT-2026-00002-copy.txt" }, content, "the file name"},
		{"another type", sealed, func(f *Formal) { f.MimeType = "application/pdf" }, content, "the type"},
		{"the PI instead of the S/C", order, func(f *Formal) { f.Kind = FormalProformaInvoice }, content, "another document"},
		{"another order revision", order, func(f *Formal) { f.OrderRevision = 1 }, content, "another document"},
		{"another order hash", order, func(f *Formal) { f.OrderHash = hashA }, content, "the order hash"},
	} {
		read := c.sealed
		c.edit(&read)
		err := Verify(c.sealed, read, []byte(c.content))
		if !errors.As(err, new(Mismatch)) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a mismatch of %s", c.name, err, c.want)
		}
	}
}

// What an event must name for EVD to look the document up: a known kind with what the kind carries.
func TestFormalNamesADocumentOfItsKind(t *testing.T) {
	q := sealedQuote("x")
	activation := q
	activation.ReplacedRevision = 0
	o := Formal{Kind: FormalProformaInvoice, QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: hashA, OrderID: "order.1", OrderRevision: 1,
		OrderHash: hashB, Number: "PI-2026-00001", FileName: "PI-2026-00001-r1.txt", MimeType: "text/plain", SHA256: hashA, Size: 1}
	for _, f := range []Formal{q, activation, o} {
		if !f.Valid() {
			t.Errorf("%+v is not valid", f)
		}
	}
	if q.Key() != (DocumentKey{Kind: FormalQuote, QuoteID: "quote.1", BusinessRevision: 2}) ||
		o.Key() != (DocumentKey{Kind: FormalProformaInvoice, OrderID: "order.1", OrderRevision: 1}) {
		t.Fatalf("keys %+v %+v", q.Key(), o.Key())
	}
	for name, edit := range map[string]func(*Formal){
		"unknown kind":              func(f *Formal) { f.Kind = "invoice" },
		"quote with an order":       func(f *Formal) { f.OrderID = "order.1" },
		"replaces itself":           func(f *Formal) { f.ReplacedRevision = f.BusinessRevision },
		"no quote":                  func(f *Formal) { f.QuoteID = "" },
		"no revision":               func(f *Formal) { f.BusinessRevision = 0 },
		"business hash not SHA-256": func(f *Formal) { f.BusinessHash = "A" + hashA[1:] },
		"no number":                 func(f *Formal) { f.Number = "" },
		"number too long":           func(f *Formal) { f.Number = strings.Repeat("N", 65) },
		"no file name":              func(f *Formal) { f.FileName = "" },
		"no type":                   func(f *Formal) { f.MimeType = "" },
		"SHA-256 too short":         func(f *Formal) { f.SHA256 = hashA[1:] },
		"empty":                     func(f *Formal) { f.Size = 0 },
	} {
		f := q
		edit(&f)
		if f.Valid() {
			t.Errorf("%s: %+v is valid", name, f)
		}
	}
	for name, edit := range map[string]func(*Formal){
		"order document without order": func(f *Formal) { f.OrderID = "" },
		"without order revision":       func(f *Formal) { f.OrderRevision = 0 },
		"without order hash":           func(f *Formal) { f.OrderHash = "" },
		"replacing a revision":         func(f *Formal) { f.ReplacedRevision = 1 },
	} {
		f := o
		edit(&f)
		if f.Valid() {
			t.Errorf("%s: %+v is valid", name, f)
		}
	}
}

// A formal document arrives as bytes: Check applies the upload's rules to them (type, name,
// signature, size) and hashes them as Decode does.
func TestCheckAppliesTheUploadRulesToBytes(t *testing.T) {
	content, sum, err := Check("QT-2026-00001.txt", "text/plain", []byte("原文"), 10<<20)
	if err != nil || string(content) != "原文" || sum != Hash([]byte("原文")) {
		t.Fatalf("Check = %q, %s, %v", content, sum, err)
	}
	_, decoded, err := Decode("QT-2026-00001.txt", "text/plain", b64("原文"), 10<<20)
	if err != nil || decoded != sum {
		t.Fatalf("Decode hashes %s, Check %s (%v)", decoded, sum, err)
	}
	for _, c := range []struct {
		name, mime string
		content    []byte
	}{
		{"x.pdf", "text/plain", []byte("ok")},
		{"x.txt", "text/markdown", []byte("ok")},
		{"a/x.txt", "text/plain", []byte("ok")},
		{strings.Repeat("n", 252) + ".txt", "text/plain", []byte("ok")},
		{"x.txt", "text/plain", nil},
		{"x.txt", "text/plain", []byte{0xff}},
		{"x.txt", "text/plain", make([]byte, 11)},
	} {
		if _, _, err := Check(c.name, c.mime, c.content, 10); !errors.As(err, new(Invalid)) {
			t.Errorf("%.20s %s %d bytes: %v", c.name, c.mime, len(c.content), err)
		}
	}
}
