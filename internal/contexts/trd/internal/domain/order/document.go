package order

import (
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderdoc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The export sales contract and the proforma invoice of a sales order (WBS S3-15, TRD-INV-020;
// order.document.generate, order.document.record_signed; UL TRD-SalesContractDocument). A document
// binds one order revision: at most one of each kind per revision, generated while the revision is
// the order's current one and the order is a draft or active (SIM-02 step 6 sends the PI and records
// the customer's countersigned copy before order.activate; TRD-INV-020's positive case: an r1 order
// generates its PI and records the countersigned copy), with its binding, bytes and commit evidence
// in one transaction. Once the order moves to another revision, the old revision's documents stay
// readable and take no more signed copies (ORDER_DOCUMENT_STALE); the new revision needs its own. A
// signed or countersigned copy is an EVD business file, recorded once per kind and never changed,
// also after the order ended (evidence of what was signed). Neither command moves the order's row
// version.

// Document is an order document: its binding (orderdoc.Binding), who generated it and when, the
// command's key, the epochs and the commit evidence entries, and its signed copies. The bytes are
// stored with it and read on their own (postgres.LoadOrderDocumentContent).
type Document struct {
	Binding        orderdoc.Binding
	Content        []byte // set when generated; not loaded with the order
	By             kernel.ActorRef
	Epoch          int64
	At             time.Time
	IdempotencyKey string
	ControlEpoch   int64
	Entries        []quote.Entry
	Signatures     []*Signature
	Fresh          bool
}

// EntriesJSON returns the entries as stored: canonical JSON, the bytes their SHA-256 covers.
func (d Document) EntriesJSON() ([]byte, error) {
	return quote.Commit{Entries: d.Entries}.EntriesJSON()
}

// Signature is a signed copy of a document as recorded (orderdoc.Signature), with the MIME type EVD
// registered its file with and the key of the command that recorded it.
type Signature struct {
	orderdoc.Signature
	MimeType       string
	IdempotencyKey string
	Fresh          bool
}

// Document returns the order's document of kind for order revision rev, or nil.
func (o *Order) Document(rev int64, kind string) *Document {
	for _, d := range o.Documents {
		if d.Binding.OrderRevision == rev && d.Binding.DocumentKind == kind {
			return d
		}
	}
	return nil
}

// Generation is what order.document.generate brings once its transaction holds the order: the
// rendered document, the command's key, the quote root's control epoch and the entries of the
// upstream guards it re-checked (POL's template and sequence in force at the hashes it rendered).
type Generation struct {
	Generated      *orderdoc.Generated
	IdempotencyKey string
	ControlEpoch   int64
	Upstream       []quote.Entry
}

// CheckDocument decides whether order revision rev with hash may get a document of kind now, in
// this order: an order that ended is INVALID_STATE (its documents are read only); another revision
// or hash than the order's current one ORDER_DOCUMENT_STALE (the order moved); a revision that has
// a document of that kind BUSINESS_KEY_CONFLICT. order.document.generate checks it before it asks
// POL for a number and again in its transaction (AddDocument).
func (o *Order) CheckDocument(rev int64, hash, kind string) error {
	switch {
	case o.Status != Draft && o.Status != Active:
		return kernel.Reject(kernel.CodeInvalidState, "/args/order_id", "The order is "+string(o.Status)+"; its documents are read only")
	case rev != o.OrderRevision || hash != o.OrderHash:
		return kernel.RejectAs(orderdoc.CodeOrderDocumentStale, orderdoc.Categories[orderdoc.CodeOrderDocumentStale], "/args/expected_order_revision",
			"The order is at revision "+strconv.FormatInt(o.OrderRevision, 10))
	case o.Document(rev, kind) != nil:
		return kernel.RejectAs(orderdoc.CodeBusinessKeyConflict, orderdoc.Categories[orderdoc.CodeBusinessKeyConflict], "/args/document_kind",
			"Order revision "+strconv.FormatInt(rev, 10)+" has its "+kind)
	}
	return nil
}

// AddDocument records document g.Generated of the order (order.document.generate) by by under
// authority epoch epoch at now, after CheckDocument on its binding (a document of another order is
// REFERENCE_INVALID). The commit evidence is the command's entries, the binding's and the upstream
// guards'.
func (o *Order) AddDocument(g Generation, by kernel.ActorRef, epoch int64, now time.Time) (*Document, error) {
	b := g.Generated.Binding
	if b.OrderID != o.ID {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "The document is another order's")
	}
	if err := o.CheckDocument(b.OrderRevision, b.OrderHash, b.DocumentKind); err != nil {
		return nil, err
	}
	at := kernel.Instant(now)
	d := &Document{Binding: b, Content: slices.Clone(g.Generated.File.Content), By: by, Epoch: epoch, At: at, IdempotencyKey: g.IdempotencyKey,
		ControlEpoch: g.ControlEpoch, Fresh: true}
	d.Entries = append([]quote.Entry{{Name: "command", Value: "order.document.generate"}, {Name: "idempotency_key", Value: g.IdempotencyKey}},
		b.Entries()...)
	d.Entries = append(d.Entries, g.Upstream...)
	d.Entries = append(d.Entries, quote.Entry{Name: "authority_epoch", Value: strconv.FormatInt(epoch, 10)},
		quote.Entry{Name: "control_epoch", Value: strconv.FormatInt(g.ControlEpoch, 10)},
		quote.Entry{Name: "generated_by", Value: string(by.Kind) + ":" + by.ID}, quote.Entry{Name: "committed_at", Value: kernel.FormatTimestamp(at)})
	o.Documents = append(o.Documents, d)
	return d, nil
}

// RecordSignature records signed copy s of the order's kind document of revision rev
// (order.document.record_signed; orderdoc.Sign on s.Signature): DOCUMENT_BINDING_MISSING for no such document,
// ORDER_DOCUMENT_STALE once the order moved past that revision, INVALID_KNOWN_VALUE for the unsigned
// file itself, INVALID_STATE for another copy of a recorded kind. It returns the signature and
// whether it was recorded before (the same file: nothing is written).
func (o *Order) RecordSignature(rev int64, kind string, s Signature) (*Signature, bool, error) {
	d := o.Document(rev, kind)
	if d == nil {
		return nil, false, kernel.RejectAs(orderdoc.CodeDocumentBindingMissing, orderdoc.Categories[orderdoc.CodeDocumentBindingMissing],
			"/args/order_revision", "The order revision has no "+kind)
	}
	r, err := o.Revision()
	if err != nil {
		return nil, false, err
	}
	recorded := make([]orderdoc.Signature, len(d.Signatures))
	for i, x := range d.Signatures {
		recorded[i] = x.Signature
	}
	s.Recorded.At, s.Recorded.Evidence = kernel.Instant(s.Recorded.At), slices.Clone(s.Recorded.Evidence)
	replayed, err := orderdoc.Sign(&d.Binding, r, s.Signature, recorded)
	if err != nil {
		return nil, false, err
	}
	if replayed {
		for _, x := range d.Signatures {
			if x.Kind == s.Kind {
				return x, true, nil
			}
		}
	}
	s.Fresh = true
	sig := &s
	d.Signatures = append(d.Signatures, sig)
	return sig, false, nil
}
