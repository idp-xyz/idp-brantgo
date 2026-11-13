package file

import (
	"strconv"
	"unicode/utf8"
)

// Formal documents of TRD that EVD registers as business files (WBS S4-06c; Context Map R65 TRD→EVD,
// authority "TRD（文档字节与绑定）／EVD（文件登记）", risk "EVD 登记的文件与 TRD 封存的字节不一致"; tactical
// model EVD-AGG-BusinessFile notes). TRD seals a formal document's bytes and binding in its own
// transaction and announces it (QuoteActivated, QuoteReplaced, OrderDocumentGenerated); EVD files
// the bytes later, in a transaction of its own, only when they are the ones TRD sealed: what TRD's
// read returns must be the document the event names, and the bytes must hash to the SHA-256 the
// event and TRD's binding carry.

// The kinds of formal document: a quote revision's (quote-text) and an order revision's S/C or PI
// (order-text).
const (
	FormalQuote           = "quote"
	FormalSalesContract   = "sales_contract"
	FormalProformaInvoice = "proforma_invoice"
)

// DocumentKey identifies a formal document of TRD: a quote revision's (QuoteID, BusinessRevision)
// or an order revision's S/C or PI (OrderID, OrderRevision, Kind).
type DocumentKey struct {
	Kind             string
	QuoteID          string
	BusinessRevision int64 // quote documents only
	OrderID          string
	OrderRevision    int64
}

// Formal is a formal document of TRD as one side describes it: the event that announced it (what
// TRD sealed) or TRD's read of it (its binding now).
type Formal struct {
	Kind             string
	QuoteID          string
	BusinessRevision int64  // the quote revision; for an order document, the revision the order came from
	BusinessHash     string // that revision's business hash
	ReplacedRevision int64  // a replacement: the revision it superseded; 0 otherwise
	OrderID          string // order documents
	OrderRevision    int64
	OrderHash        string
	Number           string
	FileName         string
	MimeType         string
	SHA256           string
	Size             int
}

// Key is the document f describes.
func (f Formal) Key() DocumentKey {
	if f.Kind == FormalQuote {
		return DocumentKey{Kind: f.Kind, QuoteID: f.QuoteID, BusinessRevision: f.BusinessRevision}
	}
	return DocumentKey{Kind: f.Kind, OrderID: f.OrderID, OrderRevision: f.OrderRevision}
}

// Valid reports whether f names a document of a known kind with what that kind carries: a quote
// document no order, an order document its order revision and hash; every document its number (at
// most 64 characters), file, SHA-256 and size.
func (f Formal) Valid() bool {
	switch f.Kind {
	case FormalQuote:
		if f.OrderID != "" || f.OrderRevision != 0 || f.OrderHash != "" || f.ReplacedRevision < 0 || f.ReplacedRevision == f.BusinessRevision {
			return false
		}
	case FormalSalesContract, FormalProformaInvoice:
		if f.OrderID == "" || f.OrderRevision < 1 || !hex64(f.OrderHash) || f.ReplacedRevision != 0 {
			return false
		}
	default:
		return false
	}
	return f.QuoteID != "" && f.BusinessRevision >= 1 && hex64(f.BusinessHash) && f.Number != "" && utf8.RuneCountInString(f.Number) <= maxNumber &&
		f.FileName != "" && f.MimeType != "" && hex64(f.SHA256) && f.Size >= 1
}

// maxNumber bounds a formal document's number (POL's numbers are far shorter).
const maxNumber = 64

// Mismatch is a formal document whose bytes or binding are not the ones TRD sealed. EVD never files
// it (R65).
type Mismatch struct{ Reason string }

func (e Mismatch) Error() string { return "file: formal document: " + e.Reason }

// Verify checks that TRD's read of a formal document — its binding read and its bytes content — is
// the document sealed, as the event that announced it names it: the same document, quote revision
// and business hash (and the revision a replacement superseded), the same order revision and hash,
// number, file name, type, binding SHA-256 and size, and bytes whose SHA-256 and size are the
// sealed ones. The first difference is a Mismatch.
func Verify(sealed, read Formal, content []byte) error {
	differ := func(what, want, got string) error {
		return Mismatch{what + " is " + strconv.Quote(got) + ", sealed " + strconv.Quote(want)}
	}
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	switch {
	case sealed.Key() != read.Key() || sealed.QuoteID != read.QuoteID:
		return Mismatch{"TRD's read is of another document"}
	case sealed.BusinessRevision != read.BusinessRevision:
		return differ("the business revision", n(sealed.BusinessRevision), n(read.BusinessRevision))
	case sealed.BusinessHash != read.BusinessHash:
		return differ("the business hash", sealed.BusinessHash, read.BusinessHash)
	case sealed.ReplacedRevision != read.ReplacedRevision:
		return differ("the replaced revision", n(sealed.ReplacedRevision), n(read.ReplacedRevision))
	case sealed.OrderHash != read.OrderHash:
		return differ("the order hash", sealed.OrderHash, read.OrderHash)
	case sealed.Number != read.Number:
		return differ("the number", sealed.Number, read.Number)
	case sealed.FileName != read.FileName:
		return differ("the file name", sealed.FileName, read.FileName)
	case sealed.MimeType != read.MimeType:
		return differ("the type", sealed.MimeType, read.MimeType)
	case sealed.SHA256 != read.SHA256:
		return differ("TRD's binding SHA-256", sealed.SHA256, read.SHA256)
	case sealed.Size != read.Size:
		return differ("TRD's binding size", strconv.Itoa(sealed.Size), strconv.Itoa(read.Size))
	}
	if sum := Hash(content); sum != sealed.SHA256 {
		return differ("the SHA-256 of the bytes", sealed.SHA256, sum)
	}
	if len(content) != sealed.Size {
		return differ("the size of the bytes", strconv.Itoa(sealed.Size), strconv.Itoa(len(content)))
	}
	return nil
}

func hex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
