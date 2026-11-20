package commitment

import (
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Dispositions of delivered goods (WBS S5R-04; simulated decision S5R-D05, E0; tactical model
// SM-PUR-COMMITMENT-DISPOSITION, PUR-INV-013; UL PUR-CommitmentDisposition). Goods the supplier delivered
// for a commitment line, verified and not shipped, are left without a purpose when the order is cancelled
// or closed short. A purchaser requests their disposition (purchase.commitment.dispose) — returned to the
// supplier with the evidence, written off as the order's sunk cost (S5R-D42) or held for reuse by a later
// order (recorded only while STK is not enabled) — and an order reviewer other than the requester decides
// it once (purchase.commitment.disposition.decide). An approved disposition counts the goods as disposed
// of in DownstreamCommitments v1 (R71), so TRD's order end no longer finds them open (TRD-INV-019). The
// disposition is an entity of the commitment: it never changes the commitment's status or version.
//
// The quantity of a line's dispositions — requested and approved at a request, approved at a decision —
// is at most what is verified delivered of it. What FUL shipped of the goods is not PUR's to read (no
// FUL→PUR relationship): the reviewer judges it, and at an order's cancellation nothing has shipped.

// States and kinds of a disposition (SM-PUR-COMMITMENT-DISPOSITION, PUR-INV-013).
const (
	DispositionRequested = "requested"
	DispositionApproved  = "approved"
	DispositionRejected  = "rejected"

	ReturnToSupplier = "return_to_supplier"
	WriteOff         = "write_off"
	HoldForReuse     = "hold_for_reuse"
)

// DispositionKinds are the kinds a disposition is of.
var DispositionKinds = []string{ReturnToSupplier, WriteOff, HoldForReuse}

// Unit is a POL unit revision: a disposition's is its order line's (PUR-INV-012).
type Unit struct {
	ID       string
	Revision int64
}

// DispositionDecision is a disposition's independent decision.
type DispositionDecision struct {
	Act
	Approve bool
}

// Disposition is a commitment line's disposition (the entity CommitmentDisposition of
// PUR-AGG-PurchaseCommitment).
type Disposition struct {
	ID           string
	CommitmentID string
	DraftID      string
	OrderID      string
	OrderLineID  string
	Status       string
	RowVersion   int64
	Stored       int64 // the row version as loaded (0: new)
	Kind         string
	Quantity     kernel.Decimal
	Unit         Unit
	Reason       string
	Evidence     []Evidence
	Requested    Act
	Decision     *DispositionDecision
}

// DispositionRequest is a purchase.commitment.dispose request, its structure validated.
type DispositionRequest struct {
	ID           string
	CommitmentID string
	OrderLineID  string
	Kind         string
	Quantity     kernel.Decimal
	Reason       string
	Evidence     []Evidence
}

// Disposed returns what the approved dispositions among xs add up to, by order line.
func Disposed(xs []*Disposition) map[string]kernel.Decimal {
	out := map[string]kernel.Decimal{}
	for _, x := range xs {
		if x.Status == DispositionApproved {
			out[x.OrderLineID] = out[x.OrderLineID].Add(x.Quantity)
		}
	}
	return out
}

// claimed is what the dispositions among xs of line, other than except, hold: approved ones, and with
// pending also those still requested.
func claimed(xs []*Disposition, line, except string, pending bool) kernel.Decimal {
	var sum kernel.Decimal
	for _, x := range xs {
		if x.OrderLineID != line || x.ID == except {
			continue
		}
		if x.Status == DispositionApproved || (pending && x.Status == DispositionRequested) {
			sum = sum.Add(x.Quantity)
		}
	}
	return sum
}

// Dispose decides purchase.commitment.dispose of r on commitment c (S5R-D05, PUR-INV-013) — whose draft
// buys lines, the order line IDs, and line r.OrderLineID is in unit — by a human under epoch at instant
// at, with verified, what is verified delivered of each line now, and others, the commitment's
// dispositions. It refuses, in this order: a commitment never issued (INVALID_STATE: only delivered goods
// are disposed of), a line the commitment does not buy (REFERENCE_INVALID), and a quantity beyond what is
// verified delivered of the line less its dispositions approved or still requested
// (INVALID_KNOWN_VALUE). The requester's kind and grant are the ledger's (human only).
func Dispose(r DispositionRequest, c *Commitment, lines []string, unit Unit, verified map[string]kernel.Decimal, others []*Disposition,
	by kernel.ActorRef, epoch int64, at time.Time) (*Disposition, error) {
	switch {
	case !IsCommitted(c.Status):
		return nil, fail(kernel.CodeInvalidState, "/args/commitment_id", "The commitment is "+c.Status+"; only goods delivered for an issued commitment are disposed of")
	case !slices.Contains(lines, r.OrderLineID):
		return nil, fail(kernel.CodeReferenceInvalid, "/args/order_line_id", "The commitment buys no such line")
	}
	if err := within(verified[r.OrderLineID], claimed(others, r.OrderLineID, r.ID, true), r.Quantity, r.OrderLineID, "/args/quantity",
		"approved or requested"); err != nil {
		return nil, err
	}
	return &Disposition{ID: r.ID, CommitmentID: c.ID, DraftID: c.DraftID, OrderID: c.OrderID, OrderLineID: r.OrderLineID,
		Status: DispositionRequested, RowVersion: 1, Kind: r.Kind, Quantity: r.Quantity, Unit: unit, Reason: r.Reason,
		Evidence: slices.Clone(r.Evidence), Requested: Act{By: by, Epoch: epoch, At: kernel.Instant(at)}}, nil
}

// within refuses quantity q of line when verified less what held already leaves less (INVALID_KNOWN_VALUE
// at path).
func within(verified, held, q kernel.Decimal, line, path, what string) error {
	if held.Add(q).Cmp(verified) <= 0 {
		return nil
	}
	left := verified.Sub(held)
	if left.Sign() < 0 {
		left = kernel.Decimal{}
	}
	return fail(kernel.CodeInvalidKnownValue, path, "Line "+line+" is verified delivered "+verified.String()+", "+held.String()+" of it "+what+
		" for disposition; at most "+left.String()+" more is disposed of")
}

// Answer decides a request whose disposition ID is taken: the same request by the same requester is
// answered from the stored disposition; anything else is VERSION_CONFLICT.
func (x *Disposition) Answer(r DispositionRequest, by kernel.ActorRef) error {
	if x.CommitmentID != r.CommitmentID || x.OrderLineID != r.OrderLineID || x.Kind != r.Kind || !x.Quantity.Equal(r.Quantity) ||
		x.Reason != r.Reason || !slices.Equal(x.Evidence, r.Evidence) || x.Requested.By != by {
		return fail(kernel.CodeVersionConflict, "/args/disposition_id", "The disposition ID is taken")
	}
	return nil
}

// Decidable decides, before anything else, whether by may decide the disposition now
// (purchase.commitment.disposition.decide; SM-PUR-COMMITMENT-DISPOSITION), in this order: the version
// (VERSION_CONFLICT), a requested disposition (INVALID_STATE: decided once), and a decider other than the
// requester (INDEPENDENT_REVIEW_REQUIRED; role templates' independence of
// purchase.commitment.disposition.decide from 处置申请人). The decider's kind and grant are the ledger's
// (human only; the order reviewer's template holds the permission).
func (x *Disposition) Decidable(expected int64, by kernel.ActorRef) error {
	switch {
	case x.RowVersion != expected:
		return fail(kernel.CodeVersionConflict, "/args/expected_version", "")
	case x.Status != DispositionRequested:
		return fail(kernel.CodeInvalidState, "/args/disposition_id", "The disposition is "+x.Status+"; it is decided once")
	case by.ID == x.Requested.By.ID:
		return fail(CodeIndependentReviewRequired, "/args/disposition_id", "The decider requested the disposition")
	}
	return nil
}

// Reject rejects the disposition by act, whose reason is required. The version moves.
func (x *Disposition) Reject(act Act) {
	act.At = kernel.Instant(act.At)
	x.Status, x.RowVersion, x.Decision = DispositionRejected, x.RowVersion+1, &DispositionDecision{Act: act}
}

// Approve approves the disposition by act (SM-PUR-COMMITMENT-DISPOSITION requested → approved), with
// verified, what is verified delivered of its line now, and others, the commitment's dispositions: its
// quantity and the line's other approved dispositions are at most what is verified (INVALID_KNOWN_VALUE at
// /disposition/quantity: a delivery was corrected since the request, or another disposition approved),
// and a return to the supplier cites its evidence (INVALID_STATE; the request requires it). The version
// moves.
func (x *Disposition) Approve(verified kernel.Decimal, others []*Disposition, act Act) error {
	if x.Kind == ReturnToSupplier && len(x.Evidence) == 0 {
		return fail(kernel.CodeInvalidState, "/disposition/evidence", "A return to the supplier cites its evidence")
	}
	if err := within(verified, claimed(others, x.OrderLineID, x.ID, false), x.Quantity, x.OrderLineID, "/disposition/quantity",
		"approved"); err != nil {
		return err
	}
	act.At = kernel.Instant(act.At)
	x.Status, x.RowVersion, x.Decision = DispositionApproved, x.RowVersion+1, &DispositionDecision{Act: act, Approve: true}
	return nil
}

// Undercut returns the first of lines, with what would be verified delivered of each, whose approved
// dispositions among xs exceed it ("" when none): a verification that leaves a line verified below its
// approved dispositions is refused (PUR-INV-013).
func Undercut(xs []*Disposition, verified map[string]kernel.Decimal, lines []string) string {
	disposed := Disposed(xs)
	for _, l := range lines {
		if disposed[l].Cmp(verified[l]) > 0 {
			return l
		}
	}
	return ""
}

// SortDispositions orders xs by disposition ID.
func SortDispositions(xs []*Disposition) {
	slices.SortFunc(xs, func(a, b *Disposition) int { return strings.Compare(a.ID, b.ID) })
}
