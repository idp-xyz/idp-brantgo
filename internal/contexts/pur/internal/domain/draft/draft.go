// Package draft is PUR's PurchaseDraft aggregate (tactical model PUR-AGG-PurchaseDraft, SM-PUR-DRAFT,
// PUR-INV-001 to 007; WBS S3-10, S3-16; Python t04_purchase, T04 A46): an immutable, hashed and
// reviewable binding of an active order's lines to exactly chosen, currently effective supply
// conditions, with the server's calculation of quantities, prices, tax and delivery. It is never a
// commitment to the supplier (commitment_state not_committed, UL-INV-20). A draft is prepared once
// and never edited; a wrong one is voided (purchase.draft.void) and its allocations released, the
// record kept (PUR-INV-007). Its state is prepared, supply_expired (derived from the clock when the
// supply's validity has ended; history is not rewritten) or voided.
package draft

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// States of a draft (SM-PUR-DRAFT). Prepared and Voided are stored; SupplyExpired is derived.
const (
	StatusPrepared = "prepared"
	StatusVoided   = "voided"
	SupplyExpired  = "supply_expired"
)

// Act is who did something to a draft, under which authority epoch, when and why.
type Act struct {
	By     kernel.ActorRef
	Epoch  int64
	At     time.Time
	Reason string
}

// Draft is a stored purchase draft: the request it answered and the document it decided, both as
// hashed, its lines' claims, who prepared it and how it ended.
type Draft struct {
	ID               string
	Status           string
	RowVersion       int64
	CommitmentState  string
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	SupplierID       string
	Currency         string
	Request          []byte
	RequestHash      string
	Document         []byte
	Hash             string
	SupplyValidUntil time.Time
	Lines            []Line // in request order
	OwnerActor       string
	OwnerTeam        string
	Prepared         Act
	Void             *Act
}

type requestDoc struct {
	DraftID         string           `json:"draft_id"`
	OrderID         string           `json:"order_id"`
	Lines           []requestLineDoc `json:"lines"`
	SelectionReason string           `json:"selection_reason"`
}

type requestLineDoc struct {
	OrderLineID string        `json:"order_line_id"`
	Quantity    string        `json:"quantity"`
	Source      DocLineSource `json:"source"`
	Overbuy     *overbuyDoc   `json:"overbuy,omitempty"`
}

type overbuyDoc struct {
	Quantity string `json:"quantity"`
	Reason   string `json:"reason"`
}

// CanonicalRequest returns the canonical JSON of r, which a draft stores and a replay compares.
func CanonicalRequest(r Request) ([]byte, error) {
	d := requestDoc{DraftID: r.DraftID, OrderID: r.OrderID, Lines: []requestLineDoc{}, SelectionReason: r.SelectionReason}
	for _, l := range r.Lines {
		x := requestLineDoc{OrderLineID: l.OrderLineID, Quantity: text(l.Quantity), Source: docSource(l.Source)}
		if l.Overbuy != nil {
			x.Overbuy = &overbuyDoc{Quantity: text(l.Overbuy.Quantity), Reason: l.Overbuy.Reason}
		}
		d.Lines = append(d.Lines, x)
	}
	return canonical(d)
}

// Seal completes a preparation with the cost snapshot's adoptability as AdoptabilityGuard v1
// decided it inside the transaction (status, code and input; recorded, never blocking: R21) and
// returns the draft: the request and the document in canonical JSON with their SHA-256, owned by
// the preparer and the preparer's team.
func (p *Prepared) Seal(status, code, input, ownerTeam string) (*Draft, error) {
	p.Doc.CostingSnapshot.Adoptability, p.Doc.CostingSnapshot.Code, p.Doc.CostingSnapshot.Input = status, code, input
	doc, err := canonical(p.Doc)
	if err != nil {
		return nil, err
	}
	req, err := CanonicalRequest(p.request)
	if err != nil {
		return nil, err
	}
	at, _ := kernel.ParseTimestamp(p.Doc.CheckedAt)
	by := kernel.ActorRef{Kind: kernel.ActorKind(p.Doc.PreparedBy.Kind), ID: p.Doc.PreparedBy.ID}
	return &Draft{ID: p.Doc.DraftID, Status: StatusPrepared, RowVersion: 1, CommitmentState: CommitmentNotCommitted,
		OrderID: p.Doc.Order.OrderID, OrderRevision: p.Doc.Order.OrderRevision, OrderHash: p.Doc.Order.OrderHash,
		SupplierID: p.Doc.Supplier.PartyID, Currency: p.Doc.Currency, Request: req, RequestHash: Hash(req), Document: doc,
		Hash: Hash(doc), SupplyValidUntil: p.SupplyValidUntil, Lines: slices.Clone(p.Lines), OwnerActor: by.ID, OwnerTeam: ownerTeam,
		Prepared: Act{By: by, Epoch: p.Doc.AuthorityEpoch, At: at}}, nil
}

// State is the draft's state at instant now: voided, or prepared until its supply's validity
// ends, supply_expired after (derived; nothing is rewritten).
func (d *Draft) State(now time.Time) string {
	switch {
	case d.Status == StatusVoided:
		return StatusVoided
	case !now.Before(d.SupplyValidUntil):
		return SupplyExpired
	}
	return StatusPrepared
}

// Decode returns the draft's document.
func (d *Draft) Decode() (Document, error) {
	var doc Document
	dec := json.NewDecoder(bytes.NewReader(d.Document))
	dec.DisallowUnknownFields()
	err := dec.Decode(&doc)
	return doc, err
}

// Verify checks, before a draft is read, replayed or voided, that its stored record is intact
// (Python _load): the request and the document hash to their stored hashes, the document is this
// draft's, in scope, of the stored order, supplier, currency, preparer, epoch and instants, its
// lines are the request's and the stored claims, and the request is the draft's. Anything else is
// REFERENCE_INVALID at path: a damaged record is never served.
func (d *Draft) Verify(scope kernel.Scope, path string) error {
	bad := fail(kernel.CodeReferenceInvalid, path, "The stored draft is not intact")
	if Hash(d.Document) != d.Hash || Hash(d.Request) != d.RequestHash {
		return bad
	}
	doc, err := d.Decode()
	if err != nil {
		return bad
	}
	var req requestDoc
	dec := json.NewDecoder(bytes.NewReader(d.Request))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&req); err != nil {
		return bad
	}
	if again, err := canonical(doc); err != nil || !bytes.Equal(again, d.Document) {
		return bad
	}
	switch {
	case doc.Contract != DocumentContract, doc.DraftID != d.ID, req.DraftID != d.ID,
		doc.Scope != (DocScope{Tenant: string(scope.Tenant), Company: string(scope.Company)}),
		doc.CommitmentState != d.CommitmentState, d.CommitmentState != CommitmentNotCommitted,
		doc.Order.OrderID != d.OrderID, req.OrderID != d.OrderID, doc.Order.OrderRevision != d.OrderRevision,
		doc.Order.OrderHash != d.OrderHash, doc.Supplier.PartyID != d.SupplierID, doc.Currency != d.Currency,
		doc.SelectionReason != req.SelectionReason, doc.CheckedAt != kernel.FormatTimestamp(d.Prepared.At),
		doc.PreparedBy != (DocActor{Kind: string(d.Prepared.By.Kind), ID: d.Prepared.By.ID}), doc.AuthorityEpoch != d.Prepared.Epoch,
		doc.SupplyValidUntil != kernel.FormatTimestamp(d.SupplyValidUntil), d.OwnerActor != d.Prepared.By.ID,
		len(req.Lines) != len(d.Lines), len(doc.Lines) != len(d.Lines):
		return bad
	}
	for i, l := range d.Lines {
		r := req.Lines[i]
		overbuy := ""
		if r.Overbuy != nil {
			overbuy = r.Overbuy.Quantity
		}
		if r.OrderLineID != l.OrderLineID || r.Quantity != text(l.Quantity) || r.Source != docSource(l.Source) ||
			(overbuy == "") != l.Overbuy.IsZero() || (overbuy != "" && overbuy != text(l.Overbuy)) {
			return bad
		}
	}
	sorted := slices.Clone(d.Lines)
	slices.SortFunc(sorted, func(a, b Line) int { return strings.Compare(a.OrderLineID, b.OrderLineID) })
	for i, l := range sorted {
		x := doc.Lines[i]
		if x.OrderLineID != l.OrderLineID || x.AllocatedQuantity != text(l.Quantity) || x.OverbuyQuantity != text(l.Overbuy) ||
			x.Source != docSource(l.Source) {
			return bad
		}
	}
	return nil
}

// Answer decides a purchase.draft.prepare whose draft ID is taken (PUR-INV-007, Python prepare's
// write replay): the same preparer with the same request is answered from the stored draft; any
// other request, or another preparer, is PURCHASE_DRAFT_IMMUTABLE. The body is never replaced.
func (d *Draft) Answer(r Request, by kernel.ActorRef) error {
	req, err := CanonicalRequest(r)
	if err != nil {
		return err
	}
	if !bytes.Equal(req, d.Request) || d.Prepared.By != by {
		return fail(CodePurchaseDraftImmutable, "/args/draft_id", "A prepared draft is never modified or replaced; void it and prepare a new one")
	}
	return nil
}

// Voidable decides purchase.draft.void before anything changes (SM-PUR-DRAFT, PUR-INV-007,
// TD-AT06): the draft at expected version, not committed to the supplier (PURCHASE_DRAFT_COMMITTED;
// commitments come with S5) and not voided already (INVALID_STATE). A supply_expired draft is voided
// like a prepared one.
func (d *Draft) Voidable(expected int64) error {
	switch {
	case d.RowVersion != expected:
		return fail(kernel.CodeVersionConflict, "/args/expected_version", "")
	case d.CommitmentState != CommitmentNotCommitted:
		return fail(CodePurchaseDraftCommitted, "/args/draft_id", "A draft that formed a purchase commitment is not voided")
	case d.Status != StatusPrepared:
		return fail(kernel.CodeInvalidState, "/args/draft_id", "The draft is voided already")
	}
	return nil
}

// VoidBy voids the draft for reason, by a human under epoch at instant at. The document, the
// request and the claims stay as they were.
func (d *Draft) VoidBy(by kernel.ActorRef, epoch int64, at time.Time, reason string) {
	d.Status, d.RowVersion = StatusVoided, d.RowVersion+1
	d.Void = &Act{By: by, Epoch: epoch, At: kernel.Instant(at), Reason: reason}
}
