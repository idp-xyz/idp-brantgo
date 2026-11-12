// Package supplierdelivery is PUR's SupplierDelivery aggregate (tactical model
// PUR-AGG-SupplierDelivery with SupplierDeliveryLine and DeliveryVerification,
// SM-PUR-SUPPLIER-DELIVERY, PUR-INV-008; UL PUR-SupplierDelivery, UL-INV-32; fact authority MD-45; WBS
// S5-15; S5 decisions D-3 and D-10). Python V0.50 has no supplier delivery: the rules come from the
// design, and the tests cite its IDs.
//
// On the direct delivery route the supplier hands the goods over to a forwarder, a yard or the
// consignee; nothing passes through a warehouse of ours. A delivery states, against the lines of one
// issued purchase commitment, what was handed over, where (UN/LOCODE), when and on which evidence.
// It is:
//
//	recorded    by a human or an AI (supplier_delivery.record): the supplier's claim. A claim never
//	            counts, for shipments or for settlement (UL-INV-32: 供应商上报只是声明);
//	verified    by a human other than the recorder (supplier_delivery.verify), line by line: what the
//	            evidence shows, never more than was claimed, and for every commitment line never more,
//	            over all its deliveries, than the commitment buys (PUR-INV-008; no tolerance is
//	            configured in S5); or
//	rejected    with a reason, when the evidence does not carry the claim;
//	superseded  a verified version is, when a correction of the delivery — its next version, recorded
//	            as a claim again — is verified. A verified version is never edited.
//
// The root is one delivery; its versions are the claims, one awaiting verification at a time, at
// most one verified. A delivery line is its commitment line: it is named, in every version, by the
// order line the commitment's draft buys, so a correction cannot present the same goods as another
// line. Concurrency is the root's and its commitment's: a command locks the delivery root FOR UPDATE
// and then the commitment root FOR UPDATE, on which the verifications of one commitment serialize;
// the verified quantity of a commitment line is derived from the verified delivery lines, never
// stored as a balance (D-3). FUL's SupplierDeliveryGuard v1 holds the delivery root FOR SHARE.
package supplierdelivery

import (
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// States of a delivery version (SM-PUR-SUPPLIER-DELIVERY) and the decisions of a verification.
const (
	StatusRecorded   = "recorded"
	StatusVerified   = "verified"
	StatusRejected   = "rejected"
	StatusSuperseded = "superseded"

	DecisionVerify = "verify"
	DecisionReject = "reject"
)

// Act is who did something to a delivery, under which authority epoch, when and why.
type Act struct {
	By     kernel.ActorRef
	Epoch  int64
	At     time.Time
	Reason string
}

// Evidence is an EVD source (SourceRef v1, Context Map R76) and where in it the fact is shown.
type Evidence struct {
	SourceID string
	Locator  string
}

// File is an EVD business file (FileRef v1, R76) with the SHA-256 it had when it was cited: a
// forwarder's cargo receipt, a loading list, photos.
type File struct {
	FileID string
	SHA256 string
}

// CommitmentLine is a line of a purchase commitment as a delivery states it (the value object
// CommitmentLineRef with what the line buys): the order line the commitment's draft buys, its SKU
// specification revision and unit, and the quantity bought, allocated plus approved overbuy.
type CommitmentLine struct {
	OrderLineID  string
	SkuID        string
	SpecRevision int64
	UnitID       string
	UnitRevision int64
	Purchased    kernel.Decimal
}

// Commitment is what a delivery judges of its purchase commitment: its state (Committed when it is
// issued or confirmed), the order and the supplier it binds, who owns it and its lines by order line.
type Commitment struct {
	ID         string
	Status     string
	Committed  bool
	OrderID    string
	SupplierID string
	OwnerActor string
	OwnerTeam  string
	Lines      []CommitmentLine
}

// From returns what commitment c and its stored, verified draft d say to a delivery; a draft whose
// document does not show its lines is REFERENCE_INVALID at path.
func From(c *commitment.Commitment, d *draft.Draft, path string) (Commitment, error) {
	bad := fail(kernel.CodeReferenceInvalid, path, "The commitment's draft is not intact")
	doc, err := d.Decode()
	if err != nil {
		return Commitment{}, bad
	}
	out := Commitment{ID: c.ID, Status: c.Status, Committed: commitment.IsCommitted(c.Status), OrderID: c.OrderID, SupplierID: c.SupplierID,
		OwnerActor: c.OwnerActor, OwnerTeam: c.OwnerTeam}
	for _, l := range d.Lines {
		i := slices.IndexFunc(doc.Lines, func(x draft.DocLine) bool { return x.OrderLineID == l.OrderLineID })
		if i < 0 || l.OrderLineID == "" {
			return Commitment{}, bad
		}
		x := doc.Lines[i]
		out.Lines = append(out.Lines, CommitmentLine{OrderLineID: l.OrderLineID, SkuID: x.Sku.SkuID, SpecRevision: x.Sku.SpecRevision,
			UnitID: x.UnitRef.ID, UnitRevision: x.UnitRef.Revision, Purchased: l.Quantity.Add(l.Overbuy)})
	}
	slices.SortFunc(out.Lines, func(a, b CommitmentLine) int { return strings.Compare(a.OrderLineID, b.OrderLineID) })
	return out, nil
}

// Line returns the commitment's line of order line id.
func (c Commitment) Line(id string) (CommitmentLine, bool) {
	i := slices.IndexFunc(c.Lines, func(l CommitmentLine) bool { return l.OrderLineID == id })
	if i < 0 {
		return CommitmentLine{}, false
	}
	return c.Lines[i], true
}

// Line is a SupplierDeliveryLine: the commitment line (OrderLineID, the delivery line's ID), the
// quantity claimed in the line's unit and, once its version is verified (Decided), the quantity
// verified.
type Line struct {
	OrderLineID  string
	SkuID        string
	SpecRevision int64
	UnitID       string
	UnitRevision int64
	Claimed      kernel.Decimal
	Verified     kernel.Decimal
	Decided      bool
}

// Decision is the DeliveryVerification: who decided the claim, whether it was verified or rejected,
// why, and the evidence the verifier added to the claim's.
type Decision struct {
	Act
	Verify   bool
	Evidence []Evidence
}

// Version is one claim of a delivery and what became of it: where and when the goods were handed
// over, the supplier's own reference, the reason of a correction (versions after the first), the
// evidence and files of the claim, the hash of the request that recorded it, its lines in the
// order they were stated, its decision and its supersession.
type Version struct {
	Number            int64
	Status            string
	HandoverPlace     string
	HandedOverAt      time.Time
	SupplierReference string
	CorrectionReason  string
	Evidence          []Evidence
	Files             []File
	RequestHash       string
	Recorded          Act
	Decision          *Decision
	SupersededBy      int64
	SupersededAt      time.Time
	Lines             []Line
}

// Line returns the version's line of order line id.
func (v *Version) Line(id string) (Line, bool) {
	i := slices.IndexFunc(v.Lines, func(l Line) bool { return l.OrderLineID == id })
	if i < 0 {
		return Line{}, false
	}
	return v.Lines[i], true
}

// Delivery is the SupplierDelivery root: the commitment it is recorded against, with its order,
// supplier and owner, and its versions in order. RowVersion moves by one per command; Stored is
// the row version as loaded (0: new).
type Delivery struct {
	ID              string
	RowVersion      int64
	Stored          int64
	CommitmentID    string
	OrderID         string
	SupplierID      string
	OwnerActor      string
	OwnerTeam       string
	LatestVersion   int64
	VerifiedVersion int64 // 0: no version is verified now
	CreatedAt       time.Time
	Versions        []*Version

	fresh, decided, superseded *Version
}

// RequestLine is one line of a claim: Quantity (positive) of the commitment line OrderLineID.
type RequestLine struct {
	OrderLineID string
	Quantity    kernel.Decimal
}

// Request is a supplier_delivery.record as the rules judge it; the api checked its structure.
type Request struct {
	DeliveryID        string
	CommitmentID      string
	Lines             []RequestLine
	HandoverPlace     string
	HandedOverAt      time.Time
	Evidence          []Evidence
	Files             []File
	SupplierReference string
	Reason            string // of a correction
	RequestHash       string
}

// Inputs are the facts a claim is judged on: the commitment as locked, the EVD sources the recorder
// reads, the recorder, the authority epoch and the instant.
type Inputs struct {
	Commitment Commitment
	Readable   map[string]bool
	Recorder   kernel.ActorRef
	Epoch      int64
	Now        time.Time
}

// claim judges req on in as version number of a delivery. It refuses, in this order: a commitment
// that is not issued or confirmed (INVALID_STATE at /args/commitment_id); a line the commitment does
// not have (REFERENCE_INVALID at it); a handover after the instant it is recorded
// (INVALID_KNOWN_VALUE at /args/handed_over_at); evidence the recorder does not read
// (REFERENCE_INVALID at it). A claim is not measured against the commitment's quantities: it never
// counts (UL-INV-32).
func claim(req Request, in Inputs, number int64) (*Version, error) {
	c := in.Commitment
	now := kernel.Instant(in.Now)
	if !c.Committed {
		return nil, fail(kernel.CodeInvalidState, "/args/commitment_id", "The purchase commitment is "+c.Status+", not issued")
	}
	v := &Version{Number: number, Status: StatusRecorded, HandoverPlace: req.HandoverPlace, HandedOverAt: kernel.Instant(req.HandedOverAt),
		SupplierReference: req.SupplierReference, CorrectionReason: req.Reason, Evidence: slices.Clone(req.Evidence), Files: slices.Clone(req.Files),
		RequestHash: req.RequestHash, Recorded: Act{By: in.Recorder, Epoch: in.Epoch, At: now}}
	if v.Files == nil {
		v.Files = []File{}
	}
	for i, l := range req.Lines {
		cl, ok := c.Line(l.OrderLineID)
		if !ok {
			return nil, fail(kernel.CodeReferenceInvalid, "/args/lines/"+itoa(int64(i))+"/order_line_id", "The commitment has no such line")
		}
		v.Lines = append(v.Lines, Line{OrderLineID: cl.OrderLineID, SkuID: cl.SkuID, SpecRevision: cl.SpecRevision, UnitID: cl.UnitID,
			UnitRevision: cl.UnitRevision, Claimed: l.Quantity})
	}
	if v.HandedOverAt.After(now) {
		return nil, fail(kernel.CodeInvalidKnownValue, "/args/handed_over_at", "A delivery is handed over no later than it is recorded")
	}
	if err := cited(req.Evidence, in.Readable); err != nil {
		return nil, err
	}
	return v, nil
}

// cited refuses evidence the actor does not read (REFERENCE_INVALID at it).
func cited(es []Evidence, readable map[string]bool) error {
	for i, e := range es {
		if !readable[e.SourceID] {
			return fail(kernel.CodeReferenceInvalid, "/args/evidence/"+itoa(int64(i))+"/source_id", "No source the actor reads")
		}
	}
	return nil
}

// Record decides supplier_delivery.record of a new delivery (expected version 0): version 1, a
// claim, owned as its commitment is — who sees the commitment sees its deliveries. See claim for
// the refusals.
func Record(req Request, in Inputs) (*Delivery, error) {
	v, err := claim(req, in, 1)
	if err != nil {
		return nil, err
	}
	c := in.Commitment
	d := &Delivery{ID: req.DeliveryID, RowVersion: 1, CommitmentID: c.ID, OrderID: c.OrderID, SupplierID: c.SupplierID, OwnerActor: c.OwnerActor,
		OwnerTeam: c.OwnerTeam, LatestVersion: 1, CreatedAt: v.Recorded.At, Versions: []*Version{v}}
	d.fresh = v
	return d, nil
}

// Answer decides a record of a new delivery whose delivery ID is taken: the same request by the
// same recorder is answered from the stored delivery; anything else is VERSION_CONFLICT.
func (d *Delivery) Answer(requestHash string, by kernel.ActorRef) error {
	if first := d.Versions[0]; first.RequestHash != requestHash || first.Recorded.By != by {
		return fail(kernel.CodeVersionConflict, "/args/delivery_id", "The delivery ID is taken")
	}
	return nil
}

// Correct decides supplier_delivery.record of the delivery's next version, a correction (expected
// version at least 1; its reason is required by the contract). It refuses, in this order: another
// row version (VERSION_CONFLICT); another commitment than the delivery's (REFERENCE_INVALID at
// /args/commitment_id); a claim of the delivery still awaiting its verification (INVALID_STATE: one
// at a time); then claim's refusals. The correction is a claim again: the version verified before
// stays the verified one until the correction is verified.
func (d *Delivery) Correct(expected int64, req Request, in Inputs) error {
	switch {
	case d.RowVersion != expected:
		return fail(kernel.CodeVersionConflict, "/args/expected_version", "")
	case req.CommitmentID != d.CommitmentID:
		return fail(kernel.CodeReferenceInvalid, "/args/commitment_id", "The delivery is recorded against another commitment")
	}
	if p := d.Pending(); p != nil {
		return fail(kernel.CodeInvalidState, "/args/delivery_id", "Version "+itoa(p.Number)+" of the delivery awaits its verification")
	}
	v, err := claim(req, in, d.LatestVersion+1)
	if err != nil {
		return err
	}
	d.Versions = append(d.Versions, v)
	d.LatestVersion, d.RowVersion, d.fresh = v.Number, d.RowVersion+1, v
	return nil
}

// Pending returns the claim that awaits its verification, or nil.
func (d *Delivery) Pending() *Version {
	if n := len(d.Versions); n > 0 && d.Versions[n-1].Status == StatusRecorded {
		return d.Versions[n-1]
	}
	return nil
}

// Current returns the version that is verified now, or nil.
func (d *Delivery) Current() *Version { return d.Version(d.VerifiedVersion) }

// Version returns version number of the delivery, or nil.
func (d *Delivery) Version(number int64) *Version {
	for _, v := range d.Versions {
		if v.Number == number {
			return v
		}
	}
	return nil
}

// Status is the delivery's state now: verified while one of its versions is the verified one,
// otherwise the state of its latest claim (recorded or rejected).
func (d *Delivery) Status() string {
	if d.VerifiedVersion > 0 {
		return StatusVerified
	}
	return d.Versions[len(d.Versions)-1].Status
}

// Decidable decides, before anything else is judged, whether by may decide the delivery's pending
// claim (supplier_delivery.verify), in this order: the row version (VERSION_CONFLICT); a claim
// awaiting verification (INVALID_STATE: a claim is decided once); a verifier who did not record it
// (INDEPENDENT_REVIEW_REQUIRED: role templates, supplier_delivery.verify independentOf 交付的登记人).
// The verifier's kind and grant are the ledger's (human only, authorized now).
func (d *Delivery) Decidable(expected int64, by kernel.ActorRef) error {
	if d.RowVersion != expected {
		return fail(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	p := d.Pending()
	switch {
	case p == nil:
		return fail(kernel.CodeInvalidState, "/args/delivery_id", "No claim of the delivery awaits verification")
	case by.ID == p.Recorded.By.ID:
		return fail(CodeIndependentReviewRequired, "/args/delivery_id", "The verifier recorded the claim")
	}
	return nil
}

// Reject rejects the pending claim by act, whose reason is required (SM-PUR-SUPPLIER-DELIVERY
// recorded → rejected: 证据不足；理由必填). Nothing of it counts; a version verified before stays the
// verified one. Decidable decided before. The row version moves.
func (d *Delivery) Reject(act Act) {
	p := d.Pending()
	act.At = kernel.Instant(act.At)
	p.Status, p.Decision = StatusRejected, &Decision{Act: act, Evidence: []Evidence{}}
	d.RowVersion, d.decided = d.RowVersion+1, p
}

// Quantity is what a verification states of one line of the claim.
type Quantity struct {
	OrderLineID string
	Verified    kernel.Decimal
}

// Verification is what a verification of the pending claim is judged on: the commitment as locked,
// the quantities verified now for each of its lines by the verified versions of its other
// deliveries (Elsewhere, by order line), the evidence the verifier adds and the EVD sources the
// verifier reads.
type Verification struct {
	Lines      []Quantity
	Commitment Commitment
	Elsewhere  map[string]kernel.Decimal
	Evidence   []Evidence
	Readable   map[string]bool
}

// Verify verifies the pending claim by act (SM-PUR-SUPPLIER-DELIVERY recorded → verified,
// PUR-INV-008); Decidable decided before. It refuses, in this order: a stated line the claim does
// not have, and a line of the claim left unstated (REFERENCE_INVALID); a quantity above the claimed
// one (INVALID_KNOWN_VALUE at it: only what was claimed is verified); nothing verified at all,
// unless the claim corrects a verified version, which it may bring down to nothing
// (INVALID_KNOWN_VALUE at /args/lines: a claim the evidence does not carry is rejected); a
// commitment no longer issued or confirmed (INVALID_STATE); evidence of the verifier's, or a source
// the claim cites, that the verifier does not read (REFERENCE_INVALID: what is verified is the
// evidence); and, for each line, a quantity that with what the commitment line's other deliveries
// verified would exceed what the line buys (SUPPLIER_DELIVERY_EXCEEDED at it; no tolerance is
// configured in S5). The version verified before is superseded; the row version moves.
func (d *Delivery) Verify(x Verification, act Act) error {
	p := d.Pending()
	stated := make([]kernel.Decimal, len(p.Lines))
	seen := make([]bool, len(p.Lines))
	at := make([]int, len(p.Lines)) // the index in x.Lines of each line of the claim
	for i, q := range x.Lines {
		j := slices.IndexFunc(p.Lines, func(l Line) bool { return l.OrderLineID == q.OrderLineID })
		if j < 0 || seen[j] {
			return fail(kernel.CodeReferenceInvalid, "/args/lines/"+itoa(int64(i))+"/order_line_id", "The claim has no such line")
		}
		stated[j], seen[j], at[j] = q.Verified, true, i
	}
	for j, l := range p.Lines {
		if !seen[j] {
			return fail(kernel.CodeReferenceInvalid, "/args/lines", "Line "+l.OrderLineID+" of the claim is not stated")
		}
	}
	something := false
	for j, l := range p.Lines {
		if stated[j].Cmp(l.Claimed) > 0 {
			return fail(kernel.CodeInvalidKnownValue, "/args/lines/"+itoa(int64(at[j]))+"/verified_quantity",
				"Only what was claimed is verified ("+text(l.Claimed)+")")
		}
		something = something || stated[j].Sign() > 0
	}
	current := d.Current()
	if !something && current == nil {
		return fail(kernel.CodeInvalidKnownValue, "/args/lines", "Nothing is verified: a claim the evidence does not carry is rejected")
	}
	c := x.Commitment
	if !c.Committed {
		return fail(kernel.CodeInvalidState, "/args/delivery_id", "The purchase commitment is "+c.Status+", not issued")
	}
	if err := cited(x.Evidence, x.Readable); err != nil {
		return err
	}
	for _, e := range p.Evidence {
		if !x.Readable[e.SourceID] {
			return fail(kernel.CodeReferenceInvalid, "/args/delivery_id", "The verifier does not read source "+e.SourceID+" the claim cites")
		}
	}
	for j, l := range p.Lines {
		cl, ok := c.Line(l.OrderLineID)
		if !ok {
			return fail(kernel.CodeReferenceInvalid, "/args/delivery_id", "The commitment no longer has line "+l.OrderLineID)
		}
		if total := x.Elsewhere[l.OrderLineID].Add(stated[j]); total.Cmp(cl.Purchased) > 0 {
			return fail(CodeSupplierDeliveryExceeded, "/args/lines/"+itoa(int64(at[j]))+"/verified_quantity",
				"Line "+l.OrderLineID+" buys "+text(cl.Purchased)+"; its other deliveries verified "+text(x.Elsewhere[l.OrderLineID]))
		}
	}
	act.At = kernel.Instant(act.At)
	for j := range p.Lines {
		p.Lines[j].Verified, p.Lines[j].Decided = stated[j], true
	}
	evidence := slices.Clone(x.Evidence)
	if evidence == nil {
		evidence = []Evidence{}
	}
	if current != nil {
		current.Status, current.SupersededBy, current.SupersededAt = StatusSuperseded, p.Number, act.At
		d.superseded = current
	}
	p.Status, p.Decision = StatusVerified, &Decision{Act: act, Verify: true, Evidence: evidence}
	d.VerifiedVersion, d.RowVersion, d.decided = p.Number, d.RowVersion+1, p
	return nil
}

// Fresh returns the version recorded since the delivery was loaded, or nil.
func (d *Delivery) Fresh() *Version { return d.fresh }

// Decided returns the version decided since the delivery was loaded, or nil.
func (d *Delivery) Decided() *Version { return d.decided }

// Superseded returns the version superseded since the delivery was loaded, or nil.
func (d *Delivery) Superseded() *Version { return d.superseded }

// Code is SupplierDeliveryGuard v1's code for a line of a version in status: "" for the verified
// version, REFERENCE_STALE for one a correction superseded, SUPPLIER_DELIVERY_UNVERIFIED for a claim
// recorded or rejected (and for anything unknown).
func Code(status string) string {
	switch status {
	case StatusVerified:
		return ""
	case StatusSuperseded:
		return CodeReferenceStale
	}
	return CodeSupplierDeliveryUnverified
}

// Sum adds, by order line, the verified quantities of the verified versions of deliveries, skipping
// the delivery whose ID is except: what PUR-INV-008 measures against a commitment line.
func Sum(deliveries []*Delivery, except string) map[string]kernel.Decimal {
	out := map[string]kernel.Decimal{}
	for _, d := range deliveries {
		v := d.Current()
		if v == nil || d.ID == except {
			continue
		}
		for _, l := range v.Lines {
			out[l.OrderLineID] = out[l.OrderLineID].Add(l.Verified)
		}
	}
	return out
}
