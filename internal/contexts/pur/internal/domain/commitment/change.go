package commitment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Commitment changes and cancellation (WBS S5-19; tactical model SM-PUR-COMMITMENT,
// SM-PUR-COMMITMENT-CHANGE, PUR-INV-011; A47 #6 and #7: 已登记的承诺不倒写，须走取消或变更流程；交期变更、
// 取消、短收和价格调整作为后续受控动作).
//
// An issued commitment is never rewritten. What it buys moves by revisions: revision 1 is the
// commitment as issued (its draft's lines); a change applied makes the next revision, and so does the
// cancellation. A change is prepared (human or AI) as the full next revision — every line of the
// draft with its allocated quantity, overbuy, condition line and supplier date — bound to the
// revision in force, with a reason and the supplier's evidence; it is decided by a human
// independent of its preparer and of the commitment's requester, who acknowledges each of its
// deviations; and it is applied by a purchaser while the commitment is still at that revision
// (PURCHASE_COMMITMENT_CHANGE_STALE otherwise). The commitment is then issued again until the
// supplier confirms the new revision. No revision buys less of a line than the supplier's verified
// deliveries of it (SUPPLIER_DELIVERY_EXCEEDED, PUR-INV-008); a short receipt is a change to exactly
// the verified quantities, after which the commitment counts as fulfilled. A cancellation keeps of
// each line exactly what was verified and releases the rest of the allocation.

// States of a change (SM-PUR-COMMITMENT-CHANGE).
const (
	ChangeDraft    = "draft"
	ChangeApproved = "approved"
	ChangeRejected = "rejected"
	ChangeApplied  = "applied"

	// Kinds of revision after the first.
	RevisionChange = "change"
	RevisionCancel = "cancel"

	// Deviations a change's decision acknowledges besides a draft's (Deviations): a supplier date
	// other than the supply condition's, one after the order's required date, and an MOQ or order
	// multiple the revision does not meet.
	DeviationDeliveryChanged = "delivery_changed"
	DeviationDeliveryLate    = "delivery_late"
	DeviationMOQNotMet       = draft.WaiverMOQNotMet
	DeviationOrderMultiple   = draft.WaiverOrderMultipleInvalid
)

// LineQuantity is what a commitment revision buys of one line of its draft: the allocated quantity,
// the overbuy and the supplier's date ("" for a line it no longer buys or a cancellation's).
type LineQuantity struct {
	OrderLineID  string
	Allocated    kernel.Decimal
	Overbuy      kernel.Decimal
	SupplierDate string
}

// Purchased is the quantity bought: allocated plus overbuy.
func (q LineQuantity) Purchased() kernel.Decimal { return q.Allocated.Add(q.Overbuy) }

// Revision is a revision of a commitment after the first: a change's (with its document) or the
// cancellation's, binding the order at the revision and hash it was prepared on, with a line per
// line of the draft. Fresh marks one made since the commitment was loaded.
type Revision struct {
	Number        int64
	Kind          string
	ChangeID      string
	OrderRevision int64
	OrderHash     string
	Document      []byte
	DocumentHash  string
	Lines         []LineQuantity
	Act
	Fresh bool
}

// Cancellation is the commitment's cancellation: who, when, why and the supplier's evidence.
type Cancellation struct {
	Act
	Evidence []Evidence
}

// DraftLines returns what revision 1 buys: the lines of draft d, with the supplier dates of its
// document, in the draft's order.
func DraftLines(d *draft.Draft) ([]LineQuantity, error) {
	doc, err := d.Decode()
	if err != nil {
		return nil, fail(kernel.CodeReferenceInvalid, "/args/commitment_id", "The stored draft is not intact")
	}
	out := make([]LineQuantity, len(d.Lines))
	for i, l := range d.Lines {
		out[i] = LineQuantity{OrderLineID: l.OrderLineID, Allocated: l.Quantity, Overbuy: l.Overbuy}
		if j := slices.IndexFunc(doc.Lines, func(x draft.DocLine) bool { return x.OrderLineID == l.OrderLineID }); j >= 0 {
			out[i].SupplierDate = doc.Lines[j].Delivery.SupplierDate
		}
	}
	return out, nil
}

// Current returns the revision in force, nil while it is revision 1.
func (c *Commitment) Current() *Revision {
	for i := range c.Revisions {
		if c.Revisions[i].Number == c.Revision && c.Revision > 1 {
			return &c.Revisions[i]
		}
	}
	return nil
}

// Lines returns what the commitment buys of each line at the revision in force; d is its draft.
func (c *Commitment) Lines(d *draft.Draft) ([]LineQuantity, error) {
	if r := c.Current(); r != nil {
		return slices.Clone(r.Lines), nil
	}
	return DraftLines(d)
}

// OrderBinding returns the order revision and hash of the revision in force.
func (c *Commitment) OrderBinding() (int64, string) {
	if r := c.Current(); r != nil {
		return r.OrderRevision, r.OrderHash
	}
	return c.OrderRevision, c.OrderHash
}

// Document returns the document of what the commitment buys now: that of the last change applied,
// or its draft's d.
func (c *Commitment) Document(d *draft.Draft) (draft.Document, error) {
	for i := len(c.Revisions) - 1; i >= 0; i-- {
		if r := c.Revisions[i]; r.Kind == RevisionChange && r.Number <= c.Revision {
			return draft.DecodeRevision(r.Document)
		}
	}
	return d.Decode()
}

// FreshRevision returns the revision made since the commitment was loaded, or nil.
func (c *Commitment) FreshRevision() *Revision {
	for i := range c.Revisions {
		if c.Revisions[i].Fresh {
			return &c.Revisions[i]
		}
	}
	return nil
}

// Fulfilled reports whether every line is verified delivered in full: what a short receipt makes a
// commitment (S5-19).
func Fulfilled(lines []LineQuantity, verified map[string]kernel.Decimal) bool {
	for _, l := range lines {
		if verified[l.OrderLineID].Cmp(l.Purchased()) < 0 {
			return false
		}
	}
	return len(lines) > 0
}

// Keeps refuses lines that buy less of a line than its verified deliveries (PUR-INV-008, PUR-INV-011:
// 已核实交付量不被变更或取消抹去): SUPPLIER_DELIVERY_EXCEEDED at the path at names for the line.
func Keeps(lines []LineQuantity, verified map[string]kernel.Decimal, at func(orderLineID string) string) error {
	for _, l := range lines {
		if v := verified[l.OrderLineID]; v.Cmp(l.Purchased()) > 0 {
			return fail(CodeSupplierDeliveryExceeded, at(l.OrderLineID), fmt.Sprintf("Line %s was verified delivered %s; it buys no less",
				l.OrderLineID, v))
		}
	}
	return nil
}

// ChangeDeviations returns the deviations of a change's proposed revision doc, line by line in order
// line order: a draft's (Deviations) and a change's own — the supplier's date not the condition's,
// a date after the order's, an MOQ or order multiple not met.
func ChangeDeviations(doc draft.Document) []Deviation {
	out := Deviations(doc)
	for _, l := range doc.Lines {
		if l.Delivery.ConditionDate != "" {
			out = append(out, Deviation{OrderLineID: l.OrderLineID, Kind: DeviationDeliveryChanged})
		}
		if l.Delivery.SlackDays < 0 {
			out = append(out, Deviation{OrderLineID: l.OrderLineID, Kind: DeviationDeliveryLate})
		}
		for _, w := range l.Waivers {
			out = append(out, Deviation{OrderLineID: l.OrderLineID, Kind: w})
		}
	}
	return out
}

// ChangeLine is a line of a change's request: the quantity of the order line allocated to the
// commitment (zero: the revision no longer buys it), the overbuy, the condition line it is bought
// under and the supplier's date ("" for the condition's).
type ChangeLine struct {
	OrderLineID  string
	Quantity     kernel.Decimal
	Overbuy      *draft.Overbuy
	Source       *draft.Source
	SupplierDate string
}

// ChangeRequest is a purchase.commitment.change.prepare request, its structure validated.
type ChangeRequest struct {
	ChangeID     string
	CommitmentID string
	Revision     int64
	Lines        []ChangeLine
	Reason       string
	Evidence     []Evidence
}

type changeRequestDoc struct {
	ChangeID     string          `json:"change_id"`
	CommitmentID string          `json:"commitment_id"`
	Revision     int64           `json:"revision"`
	Lines        []changeLineDoc `json:"lines"`
	Reason       string          `json:"reason"`
	Evidence     []evidenceDoc   `json:"evidence"`
}

type changeLineDoc struct {
	OrderLineID  string               `json:"order_line_id"`
	Quantity     string               `json:"quantity"`
	Overbuy      *overbuyDoc          `json:"overbuy,omitempty"`
	Source       *draft.DocLineSource `json:"source,omitempty"`
	SupplierDate string               `json:"supplier_date,omitempty"`
}

type overbuyDoc struct {
	Quantity string `json:"quantity"`
	Reason   string `json:"reason"`
}

type evidenceDoc struct {
	Locator  string `json:"locator"`
	SourceID string `json:"source_id"`
}

func decimalText(d kernel.Decimal) string {
	s, _ := d.Exact()
	return s
}

// Canonical returns the canonical JSON of r, which a change stores and a replay compares.
func (r ChangeRequest) Canonical() ([]byte, error) {
	doc := changeRequestDoc{ChangeID: r.ChangeID, CommitmentID: r.CommitmentID, Revision: r.Revision, Lines: []changeLineDoc{}, Reason: r.Reason,
		Evidence: []evidenceDoc{}}
	for _, l := range r.Lines {
		x := changeLineDoc{OrderLineID: l.OrderLineID, Quantity: decimalText(l.Quantity), SupplierDate: l.SupplierDate}
		if o := l.Overbuy; o != nil {
			x.Overbuy = &overbuyDoc{Quantity: decimalText(o.Quantity), Reason: o.Reason}
		}
		if s := l.Source; s != nil {
			x.Source = &draft.DocLineSource{Kind: s.Kind, ID: s.ID, Revision: s.Revision, ConditionLineID: s.ConditionLineID}
		}
		doc.Lines = append(doc.Lines, x)
	}
	for _, e := range r.Evidence {
		doc.Evidence = append(doc.Evidence, evidenceDoc{Locator: e.Locator, SourceID: e.SourceID})
	}
	return draft.Canonical(doc)
}

// DecodeChangeRequest returns the request a stored change answered.
func DecodeChangeRequest(b []byte) (ChangeRequest, error) {
	var doc changeRequestDoc
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return ChangeRequest{}, fmt.Errorf("commitment: stored change request: %w", err)
	}
	r := ChangeRequest{ChangeID: doc.ChangeID, CommitmentID: doc.CommitmentID, Revision: doc.Revision, Reason: doc.Reason}
	for _, l := range doc.Lines {
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil {
			return ChangeRequest{}, fmt.Errorf("commitment: stored change request: %w", err)
		}
		x := ChangeLine{OrderLineID: l.OrderLineID, Quantity: q, SupplierDate: l.SupplierDate}
		if o := l.Overbuy; o != nil {
			oq, err := kernel.ParseDecimal(o.Quantity, kernel.QuantityFormat)
			if err != nil {
				return ChangeRequest{}, fmt.Errorf("commitment: stored change request: %w", err)
			}
			x.Overbuy = &draft.Overbuy{Quantity: oq, Reason: o.Reason}
		}
		if s := l.Source; s != nil {
			x.Source = &draft.Source{Ref: draft.Ref{Kind: s.Kind, ID: s.ID, Revision: s.Revision}, ConditionLineID: s.ConditionLineID}
		}
		r.Lines = append(r.Lines, x)
	}
	for _, e := range doc.Evidence {
		r.Evidence = append(r.Evidence, Evidence{SourceID: e.SourceID, Locator: e.Locator})
	}
	return r, nil
}

// Proposal is a change's request as the draft's preparation takes it: the request of the lines it
// buys (a zero line is left out), with the change's reason as the selection reason, the supplier
// dates it states, and for each request line the index of its line in the change.
type Proposal struct {
	Request draft.Request
	Dates   map[string]string
	Index   []int
}

// Propose returns what r proposes of commitment c's draft d.
func (r ChangeRequest) Propose(d *draft.Draft) Proposal {
	out := Proposal{Request: draft.Request{DraftID: d.ID, OrderID: d.OrderID, SelectionReason: r.Reason}, Dates: map[string]string{}}
	for i, l := range r.Lines {
		if l.Quantity.Sign() <= 0 || l.Source == nil {
			continue
		}
		out.Request.Lines = append(out.Request.Lines, draft.RequestLine{OrderLineID: l.OrderLineID, Quantity: l.Quantity, Source: *l.Source,
			Overbuy: l.Overbuy})
		out.Index = append(out.Index, i)
		if l.SupplierDate != "" {
			out.Dates[l.OrderLineID] = l.SupplierDate
		}
	}
	return out
}

// Proposable decides, before anything upstream is asked, whether r may be prepared on commitment c
// (purchase.commitment.change.prepare; SM-PUR-COMMITMENT-CHANGE, PUR-INV-011), whose draft is d and
// whose document of what it buys now is current, in this order: an issued or confirmed commitment
// (INVALID_STATE), bound to the revision in force (PURCHASE_COMMITMENT_CHANGE_STALE at
// /args/revision), every line of the draft stated once (REFERENCE_INVALID for a line the draft
// does not have, INVALID_KNOWN_VALUE for one left out), a zero line without source or overbuy and
// any other with its source (INVALID_KNOWN_VALUE), something bought (INVALID_KNOWN_VALUE: a change
// to nothing is a cancellation), and something changed (INVALID_KNOWN_VALUE). It returns the lines
// the change proposes, in the draft's order.
func (r ChangeRequest) Proposable(c *Commitment, d *draft.Draft, current draft.Document) ([]LineQuantity, error) {
	switch {
	case !IsOpen(c.Status):
		return nil, fail(kernel.CodeInvalidState, "/args/commitment_id", "The commitment is "+c.Status+"; only an issued or confirmed commitment changes")
	case r.Revision != c.Revision:
		return nil, fail(CodePurchaseCommitmentChangeStale, "/args/revision", "The commitment is at revision "+itoa(c.Revision))
	}
	byLine := map[string]int{}
	for i, l := range r.Lines {
		p := "/args/lines/" + itoa(int64(i))
		if !slices.ContainsFunc(d.Lines, func(x draft.Line) bool { return x.OrderLineID == l.OrderLineID }) {
			return nil, fail(kernel.CodeReferenceInvalid, p+"/order_line_id", "The commitment buys no such line")
		}
		switch {
		case l.Quantity.Sign() == 0 && (l.Source != nil || l.Overbuy != nil || l.SupplierDate != ""):
			return nil, fail(kernel.CodeInvalidKnownValue, p, "A line the revision no longer buys states nothing else")
		case l.Quantity.Sign() > 0 && l.Source == nil:
			return nil, fail(kernel.CodeInvalidKnownValue, p+"/source", "A line the revision buys names its condition line")
		}
		byLine[l.OrderLineID] = i
	}
	var out []LineQuantity
	bought, changed := false, false
	for _, dl := range d.Lines {
		i, ok := byLine[dl.OrderLineID]
		if !ok {
			return nil, fail(kernel.CodeInvalidKnownValue, "/args/lines", "A change states every line of the commitment; line "+dl.OrderLineID+" is missing")
		}
		l := r.Lines[i]
		q := LineQuantity{OrderLineID: l.OrderLineID, Allocated: l.Quantity, SupplierDate: l.SupplierDate}
		if l.Overbuy != nil {
			q.Overbuy = l.Overbuy.Quantity
		}
		if l.Quantity.Sign() > 0 {
			bought = true
		}
		j := slices.IndexFunc(current.Lines, func(x draft.DocLine) bool { return x.OrderLineID == l.OrderLineID })
		switch {
		case j < 0:
			changed = changed || l.Quantity.Sign() > 0
		default:
			x := current.Lines[j]
			src := draft.DocLineSource{}
			if l.Source != nil {
				src = draft.DocLineSource{Kind: l.Source.Kind, ID: l.Source.ID, Revision: l.Source.Revision, ConditionLineID: l.Source.ConditionLineID}
			}
			date := x.Delivery.SupplierDate
			if l.SupplierDate != "" {
				date = l.SupplierDate
			}
			changed = changed || x.AllocatedQuantity != decimalText(q.Allocated) || x.OverbuyQuantity != decimalText(q.Overbuy) ||
				x.Source != src || x.Delivery.SupplierDate != date
		}
		out = append(out, q)
	}
	switch {
	case !bought:
		return nil, fail(kernel.CodeInvalidKnownValue, "/args/lines", "A change buys something; a commitment that buys nothing is cancelled")
	case !changed:
		return nil, fail(kernel.CodeInvalidKnownValue, "/args/lines", "The change states the revision in force")
	}
	return out, nil
}

// ChangeDecision is a change's independent decision.
type ChangeDecision struct {
	Act
	Approve          bool
	Acknowledgements []Acknowledgement
}

// Change is a commitment change (the entity CommitmentChange of PUR-AGG-PurchaseCommitment).
type Change struct {
	ID               string
	CommitmentID     string
	Status           string
	RowVersion       int64
	Stored           int64 // the row version as loaded (0: new)
	BaseRevision     int64
	DraftID          string
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	SupplierID       string
	Currency         string
	SupplyValidUntil time.Time
	Request          []byte
	RequestHash      string
	Document         []byte
	Hash             string
	Lines            []LineQuantity // in the draft's order
	Reason           string
	Evidence         []Evidence
	Prepared         Act
	Decision         *ChangeDecision
	Applied          *Act
	AppliedRevision  int64
}

// NewChange is the change r proposes on commitment c, a draft prepared by by under epoch at instant
// at: lines as Proposable returned them, the revision's document doc (sealed, with its hash) bound to
// the order at orderRevision and orderHash, valid while its supply is (until).
func NewChange(r ChangeRequest, c *Commitment, lines []LineQuantity, doc []byte, hash string, orderRevision int64, orderHash string,
	until time.Time, by kernel.ActorRef, epoch int64, at time.Time) (*Change, error) {
	req, err := r.Canonical()
	if err != nil {
		return nil, err
	}
	return &Change{ID: r.ChangeID, CommitmentID: c.ID, Status: ChangeDraft, RowVersion: 1, BaseRevision: r.Revision, DraftID: c.DraftID,
		OrderID: c.OrderID, OrderRevision: orderRevision, OrderHash: orderHash, SupplierID: c.SupplierID, Currency: c.Currency,
		SupplyValidUntil: kernel.Instant(until), Request: req, RequestHash: draft.Hash(req), Document: doc, Hash: hash, Lines: lines,
		Reason: r.Reason, Evidence: slices.Clone(r.Evidence), Prepared: Act{By: by, Epoch: epoch, At: kernel.Instant(at)}}, nil
}

// Answer decides a prepare whose change ID is taken: the same request by the same preparer is
// answered from the stored change; anything else is VERSION_CONFLICT.
func (x *Change) Answer(r ChangeRequest, by kernel.ActorRef) error {
	req, err := r.Canonical()
	if err != nil {
		return err
	}
	if !bytes.Equal(req, x.Request) || x.Prepared.By != by {
		return fail(kernel.CodeVersionConflict, "/args/change_id", "The change ID is taken")
	}
	return nil
}

// Decode returns the change's proposed revision document.
func (x *Change) Decode() (draft.Document, error) { return draft.DecodeRevision(x.Document) }

// Decidable decides, before anything upstream is asked, whether by may decide change x of commitment
// c now (purchase.commitment.change.decide; PUR-INV-011), in this order: the version
// (VERSION_CONFLICT), a change still a draft (INVALID_STATE: decided once), a decider who is neither
// the change's preparer nor the commitment's requester (INDEPENDENT_REVIEW_REQUIRED, role templates'
// independence of purchase.commitment.change.decide from 变更的编制人与申请人), the change hash the
// decider states (REFERENCE_STALE: another change was reviewed), an issued or confirmed commitment
// (INVALID_STATE) still at the revision the change is bound to (PURCHASE_COMMITMENT_CHANGE_STALE).
func (x *Change) Decidable(expected int64, c *Commitment, by kernel.ActorRef, hash string) error {
	switch {
	case x.RowVersion != expected:
		return fail(kernel.CodeVersionConflict, "/args/expected_version", "")
	case x.Status != ChangeDraft:
		return fail(kernel.CodeInvalidState, "/args/change_id", "The change is "+x.Status+"; it is decided once")
	case by.ID == x.Prepared.By.ID || by.ID == c.Requested.By.ID:
		return fail(CodeIndependentReviewRequired, "/args/change_id", "The decider prepared the change or requested the commitment")
	case hash != x.Hash:
		return fail(CodeReferenceStale, "/args/change_hash", "The change has another hash")
	}
	return x.bound(c)
}

func (x *Change) bound(c *Commitment) error {
	switch {
	case !IsOpen(c.Status):
		return fail(kernel.CodeInvalidState, "/args/change_id", "The commitment is "+c.Status)
	case c.Revision != x.BaseRevision:
		return fail(CodePurchaseCommitmentChangeStale, "/args/change_id", "The change is bound to revision "+itoa(x.BaseRevision)+
			"; the commitment is at revision "+itoa(c.Revision))
	}
	return nil
}

// Reject rejects the change by act, whose reason is required. The version moves.
func (x *Change) Reject(act Act) {
	act.At = kernel.Instant(act.At)
	x.Status, x.RowVersion, x.Decision = ChangeRejected, x.RowVersion+1, &ChangeDecision{Act: act}
}

// Approve approves the change by act, once its revision was found equal on the facts as they are now
// (the caller recomputed it): every acknowledgement one of the revision's deviations (REFERENCE_INVALID
// at it) and every deviation acknowledged with a reason (PURCHASE_DEVIATION_UNACKNOWLEDGED; PUR-INV-011:
// 偏差确认同 PUR-INV-009). The version moves.
func (x *Change) Approve(deviations []Deviation, acks []Acknowledgement, act Act) error {
	sorted, err := acknowledged(deviations, acks, "The change")
	if err != nil {
		return err
	}
	act.At = kernel.Instant(act.At)
	x.Status, x.RowVersion = ChangeApproved, x.RowVersion+1
	x.Decision = &ChangeDecision{Act: act, Approve: true, Acknowledgements: sorted}
	return nil
}

// Appliable decides, before anything upstream is asked, whether change x may be applied to
// commitment c (purchase.commitment.change.apply; SM-PUR-COMMITMENT-CHANGE approved → applied), in
// this order: the version (VERSION_CONFLICT), an approved change (INVALID_STATE: one applied is
// applied once, a draft or a rejected one is not approved), an issued or confirmed commitment
// (INVALID_STATE) still at the revision the change is bound to (PURCHASE_COMMITMENT_CHANGE_STALE: of
// two changes of one revision, the first applied wins).
func (x *Change) Appliable(expected int64, c *Commitment) error {
	switch {
	case x.RowVersion != expected:
		return fail(kernel.CodeVersionConflict, "/args/expected_version", "")
	case x.Status == ChangeApplied:
		return fail(kernel.CodeInvalidState, "/args/change_id", "The change was applied; it is applied once")
	case x.Status != ChangeApproved:
		return fail(kernel.CodeInvalidState, "/args/change_id", "The change is "+x.Status+", not approved")
	}
	return x.bound(c)
}

// Apply applies the approved change x by act (Appliable decided before): the commitment's next
// revision buys what x proposes, binds the order as x does and carries x's document; the commitment
// is issued again until the supplier confirms that revision (SM-PUR-COMMITMENT confirmed | issued →
// issued). Both versions move. It returns the new revision.
func (c *Commitment) Apply(x *Change, act Act) *Revision {
	act.At = kernel.Instant(act.At)
	r := Revision{Number: c.Revision + 1, Kind: RevisionChange, ChangeID: x.ID, OrderRevision: x.OrderRevision, OrderHash: x.OrderHash,
		Document: x.Document, DocumentHash: x.Hash, Lines: slices.Clone(x.Lines), Act: act, Fresh: true}
	c.Revisions = append(c.Revisions, r)
	c.Revision, c.Status, c.RowVersion = r.Number, StatusIssued, c.RowVersion+1
	x.Status, x.RowVersion, x.Applied, x.AppliedRevision = ChangeApplied, x.RowVersion+1, &act, r.Number
	return &c.Revisions[len(c.Revisions)-1]
}

// Cancel cancels the commitment by act with the supplier's evidence (purchase.commitment.cancel;
// SM-PUR-COMMITMENT issued | confirmed → cancelled; PUR-INV-011), at the version (VERSION_CONFLICT),
// of an issued or confirmed commitment (INVALID_STATE). Its next revision keeps of each line of
// current, what it buys now, exactly what was verified delivered (verified; the allocated quantity
// first, the rest as overbuy): the allocation of the undelivered quantity is released and the
// verified deliveries stay settlement and shipment sources. The version moves. It returns the new
// revision.
func (c *Commitment) Cancel(expected int64, current []LineQuantity, verified map[string]kernel.Decimal, act Act, evidence []Evidence) (*Revision, error) {
	if err := c.version(expected); err != nil {
		return nil, err
	}
	switch {
	case c.Status == StatusCancelled:
		return nil, fail(kernel.CodeInvalidState, "/args/commitment_id", "The commitment is cancelled already")
	case !IsOpen(c.Status):
		return nil, fail(kernel.CodeInvalidState, "/args/commitment_id", "The commitment is "+c.Status+"; one not issued is revoked or left")
	}
	act.At = kernel.Instant(act.At)
	orderRevision, orderHash := c.OrderBinding()
	r := Revision{Number: c.Revision + 1, Kind: RevisionCancel, OrderRevision: orderRevision, OrderHash: orderHash, Act: act, Fresh: true}
	for _, l := range current {
		v := verified[l.OrderLineID]
		kept := LineQuantity{OrderLineID: l.OrderLineID, Allocated: v, SupplierDate: l.SupplierDate}
		if v.Cmp(l.Allocated) > 0 {
			kept.Allocated, kept.Overbuy = l.Allocated, v.Sub(l.Allocated)
		}
		r.Lines = append(r.Lines, kept)
	}
	c.Revisions = append(c.Revisions, r)
	c.Revision, c.Status, c.RowVersion = r.Number, StatusCancelled, c.RowVersion+1
	c.Cancellation = &Cancellation{Act: act, Evidence: slices.Clone(evidence)}
	return &c.Revisions[len(c.Revisions)-1], nil
}

// ChangeLines returns lines in order line order, for events and views.
func ChangeLines(lines []LineQuantity) []LineQuantity {
	out := slices.Clone(lines)
	slices.SortFunc(out, func(a, b LineQuantity) int { return strings.Compare(a.OrderLineID, b.OrderLineID) })
	return out
}
