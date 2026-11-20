package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// TRD's formal documents as EVD business files (WBS S4-06c; Context Map R65 TRD→EVD, Published
// Language events, no guard lock; S4 decision D-8; IG-08; tactical model EVD-AGG-BusinessFile notes:
// "正式文档字节由 TRD 在自身 schema 同事务封存，EVD 经 QuoteActivated／OrderDocumentGenerated 事件登记文件
// 引用，不在 TRD 事务内写入").
//
// TRD seals the bytes and binding of a quote revision's formal document when it activates or
// replaces the revision, and of an order revision's S/C or PI when it generates one, and announces
// them (QuoteActivated, QuoteReplaced, OrderDocumentGenerated). EVD's Inbox consumer evd.formal_files
// reads each announced document through TRD's FormalDocument read before its own transaction and
// registers its bytes as a business file — FileRef v1 for every context downstream of EVD — once
// per event, with what it holds: the TRD document, its business and order hashes, its number, the
// SHA-256 TRD sealed and the event. Bytes that are not the sealed ones are never filed: the event is
// refused for good (dead letter and alert, ADR-BR-015). The file is owned by the quote root's owner,
// so whoever sees the quote in TRD sees its formal files in EVD (data scope).
const (
	// CommandRegisterFormalFile is the consumer's command: system actors only, granted to
	// system:inbox.evd.formal_files by brant-admin inbox. It is not a command of the Context Map's EVD
	// command list (a consumer's registration, like TRD's quote.risk_hold.register).
	CommandRegisterFormalFile = "formal_file.register"

	// The kinds of formal document: a quote revision's, an order revision's sales contract (S/C) or
	// proforma invoice (PI) (TRD's document kinds).
	FormalQuote           = "quote"
	FormalSalesContract   = "sales_contract"
	FormalProformaInvoice = "proforma_invoice"
)

// FormalDocumentRef names a formal document of TRD: a quote revision's (Kind quote, QuoteID and
// BusinessRevision) or the S/C or PI of an order revision (Kind sales_contract or proforma_invoice,
// OrderID and OrderRevision).
type FormalDocumentRef struct {
	Kind             string
	QuoteID          string
	BusinessRevision int64
	OrderID          string
	OrderRevision    int64
}

// Validate checks the reference: the members of its kind and no others.
func (r FormalDocumentRef) Validate() error {
	switch r.Kind {
	case FormalQuote:
		switch {
		case !kernel.ValidIdentifier(r.QuoteID):
			return schema("/quote_id")
		case r.BusinessRevision < 1:
			return schema("/business_revision")
		case r.OrderID != "" || r.OrderRevision != 0:
			return schema("/order_id")
		}
	case FormalSalesContract, FormalProformaInvoice:
		switch {
		case !kernel.ValidIdentifier(r.OrderID):
			return schema("/order_id")
		case r.OrderRevision < 1:
			return schema("/order_revision")
		case r.QuoteID != "" || r.BusinessRevision != 0:
			return schema("/quote_id")
		}
	default:
		return schema("/kind")
	}
	return nil
}

// FormalFile is a business file EVD registered for a formal document of TRD: the file and its
// SHA-256 (FileRef v1), the document it holds, as TRD sealed it — the quote revision and its
// business hash (for an order document: the revision the order came from), the revision a
// replacement superseded, the order revision's hash, the number, file name, type and size — and the
// event that registered it.
type FormalFile struct {
	FileID           string
	FileSHA256       string // the SHA-256 TRD sealed, and that of the file's bytes
	Document         FormalDocumentRef
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	ReplacedRevision int64 // QuoteReplaced: the revision the document's revision superseded
	OrderHash        string
	Number           string
	FileName         string
	MimeType         string
	SizeBytes        int
	EventID          string
	EventType        string // TRD's QuoteActivated, QuoteReplaced or OrderDocumentGenerated
	RegisteredAt     time.Time
}

// FormalFiles reads the formal files EVD registered.
type FormalFiles interface {
	// GetFormalFile returns the file EVD registered for TRD's formal document ref, audited as
	// file.formal.get (evd.read): NOT_FOUND when EVD holds none or the caller does not see the file;
	// SCHEMA_INVALID for an invalid reference.
	GetFormalFile(ctx context.Context, ref FormalDocumentRef) (FormalFile, error)
}
