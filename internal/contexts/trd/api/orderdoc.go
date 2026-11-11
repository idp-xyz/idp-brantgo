package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The export sales contract (S/C) and proforma invoice (PI) of a sales order and their signed copies
// (WBS S3-15; tactical model TRD-AGG-SalesOrder OrderContractDocument; TRD-INV-020; master data
// MD-35; SIM-02 step 6; SAP reference O2C-05). Python has none of this.
//
//	order.document.generate      human or AI: renders the S/C or PI of draft or active order
//	                             order_id at its current revision (expected_order_revision and
//	                             expected_order_hash; ORDER_DOCUMENT_STALE when the order moved) with
//	                             order-text/0.1.0 from POL order_document_template revision
//	                             template_ref (in force; its document kind is document_kind), numbered
//	                             by POL from number_sequence revision sequence_ref (document type
//	                             order for an S/C, proforma_invoice for a PI; business reference the
//	                             order ID, so every revision's S/C carries the order's one contract
//	                             number). The binding (order revision and hash, number, template and
//	                             its content hash, renderer, file), the bytes and the commit evidence
//	                             are stored in one transaction. One document of each kind per order
//	                             revision (BUSINESS_KEY_CONFLICT). An order that ended is INVALID_STATE.
//	order.document.record_signed human: records the signed copy (signature_kind signed: ours) or the
//	                             countersigned copy (countersigned: the customer's, 回签) of the kind
//	                             document of order revision order_revision, an EVD business file
//	                             file_id the caller reads, with 0–200 EVD sources as evidence (the
//	                             message that carried it, say). The document must bind the order's
//	                             current revision (ORDER_DOCUMENT_STALE once amended;
//	                             DOCUMENT_BINDING_MISSING for none); the generated, unsigned bytes are
//	                             never a signed copy (INVALID_KNOWN_VALUE at /args/file_id); a kind
//	                             is recorded once and never changes (the same file again is a replay,
//	                             another INVALID_STATE).
//
// Both lock the quote root FOR UPDATE, pass its object control, then lock the order FOR UPDATE; the
// authority guard row follows (FOR SHARE), then, for generate, POL's ConfigGuard of the template and
// the sequence at the content hashes read before the transaction. Neither moves the order's row
// version. Request members in snake_case: order.document.generate {order_id, expected_order_revision,
// expected_order_hash, document_kind, template_ref {kind, id, revision}, sequence_ref {kind, id,
// revision}}; order.document.record_signed {order_id, order_revision, document_kind, signature_kind,
// file_id, evidence}.
const (
	CommandGenerateOrderDocument     = "order.document.generate"
	CommandRecordSignedOrderDocument = "order.document.record_signed"

	// EventOrderDocumentGenerated and EventOrderDocumentSigned are the tactical model's events of
	// the two commands; the event catalog V0.1 lists neither yet. EVD registers the generated file's
	// reference from OrderDocumentGenerated (tactical model EVD notes), not in TRD's transaction.
	EventOrderDocumentGenerated = "idp-brant.trd.order_document_generated.v1"
	EventOrderDocumentSigned    = "idp-brant.trd.order_document_signed.v1"

	// The document kinds and the kinds of a signed copy.
	DocumentSalesContract   = "sales_contract"
	DocumentProformaInvoice = "proforma_invoice"
	SignatureSigned         = "signed"
	SignatureCountersigned  = "countersigned"

	// StatusGenerated is the receipt status of order.document.generate; record_signed answers with
	// the signature kind.
	StatusGenerated = "generated"
)

// GenerateOrderDocument renders and binds the DocumentKind document of order OrderID's current
// revision. Human or AI.
type GenerateOrderDocument struct {
	IdempotencyKey        string
	OrderID               string
	ExpectedOrderRevision int64
	ExpectedOrderHash     string
	DocumentKind          string    // sales_contract or proforma_invoice
	TemplateRef           ConfigRef // an order_document_template revision
	SequenceRef           ConfigRef // a number_sequence revision
}

// RecordSignedOrderDocument records a signed or countersigned copy of the DocumentKind document of
// order revision OrderRevision. Human only.
type RecordSignedOrderDocument struct {
	IdempotencyKey string
	OrderID        string
	OrderRevision  int64
	DocumentKind   string
	SignatureKind  string // signed or countersigned
	FileID         string // an EVD business file
	Evidence       []Evidence
}

func documentKind(kind string) error {
	if kind != DocumentSalesContract && kind != DocumentProformaInvoice {
		return schema("/args/document_kind")
	}
	return nil
}

// Validate checks the closed contract.
func (c GenerateOrderDocument) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case c.ExpectedOrderRevision < 1 || c.ExpectedOrderRevision > maxVersion:
		return schema("/args/expected_order_revision")
	case !hash64.MatchString(c.ExpectedOrderHash):
		return schema("/args/expected_order_hash")
	}
	if err := documentKind(c.DocumentKind); err != nil {
		return err
	}
	if err := ref(c.TemplateRef, "/args/template_ref"); err != nil {
		return err
	}
	return ref(c.SequenceRef, "/args/sequence_ref")
}

// Validate checks the closed contract: 0–200 evidence entries.
func (c RecordSignedOrderDocument) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case c.OrderRevision < 1 || c.OrderRevision > maxVersion:
		return schema("/args/order_revision")
	}
	if err := documentKind(c.DocumentKind); err != nil {
		return err
	}
	switch {
	case c.SignatureKind != SignatureSigned && c.SignatureKind != SignatureCountersigned:
		return schema("/args/signature_kind")
	case !kernel.ValidIdentifier(c.FileID):
		return schema("/args/file_id")
	}
	if len(c.Evidence) > 0 {
		return evidence(c.Evidence, "/args/evidence")
	}
	return nil
}

func (GenerateOrderDocument) trdCommand()     {}
func (RecordSignedOrderDocument) trdCommand() {}

// OrderDocumentReceipt is the answer of the document commands: the receipt (ObjectID the order ID,
// RowVersion the order's row version, which neither command moves; Status generated, or the
// signature kind) and the document it acted on — its order revision and hash, kind, number, file
// and SHA-256 — and, for record_signed, the signed copy's file and SHA-256. A replay rebuilds it
// from the stored order.
type OrderDocumentReceipt struct {
	Receipt
	OrderRevision  int64
	OrderHash      string
	DocumentKind   string
	Number         string
	FileName       string
	MimeType       string
	DocumentSHA256 string
	SizeBytes      int
	SignatureKind  string
	SignedFileID   string
	SignedSHA256   string
}

// OrderDocumentView is an S/C or PI of an order: its binding (order revision and hash, kind, number
// and the POL sequence it came from, template and content hash, renderer, generation instant, file),
// who generated it and its commit evidence, its signed copies, and whether it binds the order's
// current revision (Current; a document of an earlier revision is read only).
type OrderDocumentView struct {
	OrderRevision   int64
	OrderHash       string
	DocumentKind    string
	Number          string
	SequenceRef     ConfigRef
	BusinessRef     string
	TemplateRef     ConfigRef
	TemplateHash    string
	RendererVersion string
	GeneratedAt     string // kernel.FormatTimestamp
	FileName        string
	MimeType        string
	DocumentSHA256  string
	SizeBytes       int
	IdempotencyKey  string
	AuthorityEpoch  int64
	ControlEpoch    int64
	GeneratedBy     kernel.ActorRef
	CommittedAt     time.Time
	Entries         map[string]string
	EntriesSHA256   string
	Current         bool
	Signatures      []OrderSignatureView
}

// OrderSignatureView is a signed copy of an order document: its kind, the EVD file, its SHA-256 and
// MIME type, who recorded it, when and on which evidence.
type OrderSignatureView struct {
	SignatureKind string
	FileID        string
	FileSHA256    string
	MimeType      string
	RecordedBy    kernel.ActorRef
	RecordedAt    time.Time
	Evidence      []Evidence
}

// OrderDocument is an order document with its bytes, verified against its binding on every read
// (DOCUMENT_INTEGRITY_FAILURE otherwise), current or not.
type OrderDocument struct {
	OrderID  string
	Document OrderDocumentView
	Content  []byte
}

// OrderDocumentCommands is the command entry point of order documents. Errors are as Commands
// describes them. It is not part of Commands until the transport routes it.
type OrderDocumentCommands interface {
	GenerateOrderDocument(ctx context.Context, cmd GenerateOrderDocument) (OrderDocumentReceipt, error)
	RecordSignedOrderDocument(ctx context.Context, cmd RecordSignedOrderDocument) (OrderDocumentReceipt, error)
}

// OrderDocumentQueries reads order documents.
type OrderDocumentQueries interface {
	// GetOrderDocument returns the kind document of order revision orderRevision with its bytes,
	// audited as order.document.get (trd.read): NOT_FOUND for an order of a quote the caller does not
	// see or no such document; DOCUMENT_INTEGRITY_FAILURE for bytes that are not the bound ones.
	GetOrderDocument(ctx context.Context, orderID string, orderRevision int64, documentKind string) (OrderDocument, error)
}
