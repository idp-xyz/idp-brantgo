// Package commitment is PUR's PurchaseCommitment aggregate (tactical model PUR-AGG-PurchaseCommitment,
// SM-PUR-COMMITMENT, PUR-INV-009 and PUR-INV-010; WBS S5-01; S5 decision D-14). Python V0.50 has it
// as design only: docs/T04_A46_PURCHASE_DRAFT_REPORT_V0.1.md, "独立采购承诺审批设计（建议 A47）", points
// #1 to #8, cited below as A47 #n.
//
// A commitment binds one purchase draft at its draft hash (A47 #1), and through it the order at its
// revision and hash, the supplier, the currency and the end of the draft's supply validity. It is:
//
//	requested  by a purchaser, human or AI (purchase.commitment.request); a draft has one live
//	           commitment at a time;
//	approved   by a human independent of the draft's preparer and of the requester (A47 #2), after
//	           the draft was prepared again on the facts as they are now and found equal field by
//	           field (A47 #3), with a validity after the decision and not after the draft's supply
//	           validity (A47 #4) and every deviation from the costing — another source, a cost above
//	           it, a cost not comparable, an overbuy (A47 #5; D-14 moves the overbuy's approval here)
//	           — acknowledged with a reason; or rejected with a reason;
//	revoked    before it is issued, which stops the issue (A47 #6);
//	issued     once, on a live, unexpired approval whose decider is still qualified, with POL's
//	           purchase order number and the evidence of sending (A47 #7, PUR-INV-010): the draft is
//	           committed and is never voided;
//	confirmed  by the supplier as issued, a separate fact with its evidence and the time it was
//	           received; a confirmation with deviations is recorded and leaves the commitment issued,
//	           open for a change (A47 #7). Changes and cancellation after issue are S5-19.
//
// Concurrency is the commitment root's: every command locks it FOR UPDATE first, the decision and
// the issue then the DemandAllocation roots of the draft's lines (tactical lockOrder), so a
// decision, a revocation and an issue of one commitment have one outcome, and a draft's void and its
// commitment's issue serialize on the allocation roots. A commitment never takes an allocation of
// its own: the draft's entries stay effective while the draft is not voided (PUR-INV-002).
package commitment

import (
	"math/big"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// States of a commitment (SM-PUR-COMMITMENT). StateExpired is derived: an approval read after its
// validity ended.
const (
	StatusRequested = "requested"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
	StatusRevoked   = "revoked"
	StatusIssued    = "issued"
	StatusConfirmed = "confirmed"
	StateExpired    = "expired"

	// Committed is the commitment state of a draft whose commitment was issued (derived).
	Committed = "committed"

	DecisionApprove = "approve"
	DecisionReject  = "reject"

	// Deviations an approval acknowledges line by line (A47 #5, PUR-INV-009).
	DeviationSourceDiffers = "source_differs"      // not the costing's single source
	DeviationCostAbove     = "cost_above_costing"  // the cost of the allocated quantity is above the costing's share
	DeviationNotComparable = "cost_not_comparable" // no variance: another currency, or the costing has no cost for the line
	DeviationOverbuy       = "overbuy"             // the line buys beyond its allocation (D-14)

	// Results of a supplier confirmation.
	ResultAsIssued       = "as_issued"
	ResultWithDeviations = "with_deviations"

	// Permissions the issue re-reads of the decider (current qualification, A47 #2).
	PermissionDecide = "purchase.commitment.decide"
	PermissionRead   = "pur.read"
)

// Act is who did something to a commitment, under which authority epoch, when and why.
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

// Deviation is one way a draft line departs from the costing it is compared with (A47 #5).
type Deviation struct {
	OrderLineID string
	Kind        string
}

// Acknowledgement is an approval's acknowledgement of a deviation, with its reason.
type Acknowledgement struct {
	OrderLineID string
	Kind        string
	Reason      string
}

// Decision is the independent decision: approve or reject, the approval's validity and the
// deviations it acknowledged.
type Decision struct {
	Act
	Approve          bool
	ValidUntil       time.Time
	Acknowledgements []Acknowledgement
}

// Sequence is a POL number_sequence revision.
type Sequence struct {
	ID       string
	Revision int64
}

// Issue is the commitment as sent to the supplier: POL's purchase order number from Sequence, when
// it was sent, the evidence of sending and, optionally, the EVD business file sent.
type Issue struct {
	Act
	Number     string
	Sequence   Sequence
	SentAt     time.Time
	Evidence   []Evidence
	FileID     string
	FileSHA256 string
}

// Confirmation is what the supplier answered to the issued commitment, recorded at commitment
// version Version.
type Confirmation struct {
	Act
	Version           int64
	Result            string
	ReceivedAt        time.Time
	Evidence          []Evidence
	Deviation         string
	SupplierReference string
	Fresh             bool
}

// Commitment is the PurchaseCommitment root.
type Commitment struct {
	ID               string
	Status           string
	RowVersion       int64
	Stored           int64 // the row version as loaded (0: new)
	DraftID          string
	DraftHash        string
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	SupplierID       string
	Currency         string
	SupplyValidUntil time.Time
	OwnerActor       string
	OwnerTeam        string
	Requested        Act
	Decision         *Decision
	Revocation       *Act
	Issue            *Issue
	Confirmations    []Confirmation
}

// Draft is what a commitment judges of its purchase draft, a stored and verified one (From).
type Draft struct {
	ID               string
	Hash             string
	Voided           bool
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	SupplierID       string
	Currency         string
	SupplyValidUntil time.Time
	PreparedBy       kernel.ActorRef
	Deviations       []Deviation
}

// From returns what d, verified, says to a commitment: its binding, whether it is voided, its
// preparer and its deviations.
func From(d *draft.Draft) (Draft, error) {
	doc, err := d.Decode()
	if err != nil {
		return Draft{}, fail(kernel.CodeReferenceInvalid, "/args/draft_id", "The stored draft is not intact")
	}
	return Draft{ID: d.ID, Hash: d.Hash, Voided: d.Status == draft.StatusVoided, OrderID: d.OrderID, OrderRevision: d.OrderRevision,
		OrderHash: d.OrderHash, SupplierID: d.SupplierID, Currency: d.Currency, SupplyValidUntil: kernel.Instant(d.SupplyValidUntil),
		PreparedBy: d.Prepared.By, Deviations: Deviations(doc)}, nil
}

// Deviations returns the deviations of a draft's document from its costing comparison, line by line
// in order line order (A47 #5, PUR-INV-009): another source than the costing's single one, a cost of
// the allocated quantity above the costing's share, a cost that cannot be compared (another
// currency: no exchange rate is inferred; or no costing cost for the line), and an overbuy (D-14).
func Deviations(doc draft.Document) []Deviation {
	var out []Deviation
	for _, l := range doc.Lines {
		c := l.CostingReference
		if !c.SameSource {
			out = append(out, Deviation{OrderLineID: l.OrderLineID, Kind: DeviationSourceDiffers})
		}
		switch v := c.CostVarianceMinor; {
		case v == nil:
			out = append(out, Deviation{OrderLineID: l.OrderLineID, Kind: DeviationNotComparable})
		case positive(*v):
			out = append(out, Deviation{OrderLineID: l.OrderLineID, Kind: DeviationCostAbove})
		}
		if l.Overbuy != nil {
			out = append(out, Deviation{OrderLineID: l.OrderLineID, Kind: DeviationOverbuy})
		}
	}
	return out
}

// positive reports whether minor, an integer amount in minor units, is above zero.
func positive(minor string) bool {
	n, ok := new(big.Int).SetString(minor, 10)
	return ok && n.Sign() > 0
}

// Live is a commitment of a draft, as the request's one-live-commitment rule reads it.
type Live struct {
	ID     string
	Status string
}

// IsLive reports whether a commitment in status counts as the draft's live one.
func IsLive(status string) bool {
	return status == StatusRequested || status == StatusApproved || status == StatusIssued || status == StatusConfirmed
}

// IsCommitted reports whether a commitment in status was issued: its draft is committed.
func IsCommitted(status string) bool { return status == StatusIssued || status == StatusConfirmed }

// Request decides purchase.commitment.request of commitment id for draft d (A47 #1), stated at hash
// by its requester (human or AI) under epoch at instant now: the draft as stated (REFERENCE_STALE at
// /args/draft_hash), not voided (INVALID_STATE), its supply in force (SUPPLY_CONDITION_EXPIRED),
// without a live commitment (PURCHASE_DRAFT_COMMITTED for an issued one, INVALID_STATE for one
// requested or approved). The requester and the requester's team own it.
func Request(id string, d Draft, hash string, live []Live, by kernel.ActorRef, epoch int64, now time.Time, team string) (*Commitment, error) {
	at := kernel.Instant(now)
	switch {
	case hash != d.Hash:
		return nil, fail(CodeReferenceStale, "/args/draft_hash", "The draft has another hash")
	case d.Voided:
		return nil, fail(kernel.CodeInvalidState, "/args/draft_id", "The draft is voided")
	case !at.Before(d.SupplyValidUntil):
		return nil, fail(CodeSupplyConditionExpired, "/args/draft_id", "The draft's supply validity has ended")
	}
	for _, l := range live {
		switch {
		case IsCommitted(l.Status):
			return nil, fail(CodePurchaseDraftCommitted, "/args/draft_id", "Commitment "+l.ID+" of the draft was issued")
		case IsLive(l.Status):
			return nil, fail(kernel.CodeInvalidState, "/args/draft_id", "Commitment "+l.ID+" of the draft is "+l.Status)
		}
	}
	return &Commitment{ID: id, Status: StatusRequested, RowVersion: 1, DraftID: d.ID, DraftHash: d.Hash, OrderID: d.OrderID,
		OrderRevision: d.OrderRevision, OrderHash: d.OrderHash, SupplierID: d.SupplierID, Currency: d.Currency,
		SupplyValidUntil: d.SupplyValidUntil, OwnerActor: by.ID, OwnerTeam: team, Requested: Act{By: by, Epoch: epoch, At: at}}, nil
}

// Answer decides a request whose commitment ID is taken: the same draft and hash by the same
// requester is answered from the stored commitment; anything else is VERSION_CONFLICT.
func (c *Commitment) Answer(draftID, hash string, by kernel.ActorRef) error {
	if c.DraftID != draftID || c.DraftHash != hash || c.Requested.By != by {
		return fail(kernel.CodeVersionConflict, "/args/commitment_id", "The commitment ID is taken")
	}
	return nil
}

// State is the commitment's state at instant now: its status, or expired for an approval whose
// validity has ended (derived; nothing is rewritten).
func (c *Commitment) State(now time.Time) string {
	if c.Status == StatusApproved && c.Decision != nil && !kernel.Instant(now).Before(c.Decision.ValidUntil) {
		return StateExpired
	}
	return c.Status
}

func (c *Commitment) version(expected int64) error {
	if c.RowVersion != expected {
		return fail(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

func (c *Commitment) intact(d Draft) error {
	if d.ID != c.DraftID || d.Hash != c.DraftHash || d.OrderID != c.OrderID || d.OrderRevision != c.OrderRevision ||
		d.OrderHash != c.OrderHash || d.SupplierID != c.SupplierID || d.Currency != c.Currency || !d.SupplyValidUntil.Equal(c.SupplyValidUntil) {
		return fail(kernel.CodeReferenceInvalid, "/args/commitment_id", "The commitment does not bind its draft as stored")
	}
	return nil
}

// Decidable decides, before anything upstream is asked, whether by may decide the commitment now
// (purchase.commitment.decide; A47 #2, PUR-INV-009), in this order: the version (VERSION_CONFLICT),
// a requested commitment (INVALID_STATE: decided once), a decider who is neither the draft's
// preparer nor the requester (INDEPENDENT_REVIEW_REQUIRED), the draft as bound and not voided
// (REFERENCE_INVALID, INVALID_STATE), and the draft hash the decider states (REFERENCE_STALE: the
// decider reviewed another draft). The decider's kind and grant are the ledger's (human only,
// authorized now).
func (c *Commitment) Decidable(expected int64, d Draft, by kernel.ActorRef, hash string) error {
	if err := c.version(expected); err != nil {
		return err
	}
	switch {
	case c.Status != StatusRequested:
		return fail(kernel.CodeInvalidState, "/args/commitment_id", "The commitment is "+c.Status+"; it is decided once")
	case by.ID == d.PreparedBy.ID || by.ID == c.Requested.By.ID:
		return fail(CodeIndependentReviewRequired, "/args/commitment_id", "The decider prepared the draft or requested the commitment")
	}
	if err := c.intact(d); err != nil {
		return err
	}
	switch {
	case d.Voided:
		return fail(kernel.CodeInvalidState, "/args/commitment_id", "The draft is voided")
	case hash != c.DraftHash:
		return fail(CodeReferenceStale, "/args/draft_hash", "The commitment binds another draft hash")
	}
	return nil
}

// Reject rejects the commitment by act, whose reason is required (SM-PUR-COMMITMENT requested →
// rejected). The version moves.
func (c *Commitment) Reject(act Act) {
	act.At = kernel.Instant(act.At)
	c.Status, c.RowVersion = StatusRejected, c.RowVersion+1
	c.Decision = &Decision{Act: act}
}

// Approve approves the commitment by act, once the draft was found equal on the facts as they are
// now (A47 #3; the caller recomputed it): an approval valid until a time after the decision and not
// after the draft's supply validity (INVALID_KNOWN_VALUE at /args/valid_until, A47 #4), every
// acknowledgement one of the draft's deviations (REFERENCE_INVALID at it) and every deviation
// acknowledged with a reason (PURCHASE_DEVIATION_UNACKNOWLEDGED, A47 #5; an overbuy is approved here,
// D-14). The version moves.
func (c *Commitment) Approve(d Draft, validUntil time.Time, acks []Acknowledgement, act Act) error {
	at, until := kernel.Instant(act.At), kernel.Instant(validUntil)
	switch {
	case !until.After(at):
		return fail(kernel.CodeInvalidKnownValue, "/args/valid_until", "An approval is valid after its decision")
	case until.After(c.SupplyValidUntil):
		return fail(kernel.CodeInvalidKnownValue, "/args/valid_until", "An approval is valid no longer than the draft's supply ("+
			kernel.FormatTimestamp(c.SupplyValidUntil)+")")
	}
	for i, a := range acks {
		if !slices.Contains(d.Deviations, Deviation{OrderLineID: a.OrderLineID, Kind: a.Kind}) {
			return fail(kernel.CodeReferenceInvalid, "/args/acknowledgements/"+itoa(int64(i)), "The draft has no such deviation")
		}
	}
	for _, dv := range d.Deviations {
		if !slices.ContainsFunc(acks, func(a Acknowledgement) bool {
			return a.OrderLineID == dv.OrderLineID && a.Kind == dv.Kind && strings.TrimSpace(a.Reason) != ""
		}) {
			return fail(CodePurchaseDeviationUnacknowledged, "/args/acknowledgements", "Line "+dv.OrderLineID+" deviates ("+dv.Kind+
				") and the approval does not acknowledge it with a reason")
		}
	}
	act.At = at
	sorted := slices.Clone(acks)
	slices.SortFunc(sorted, func(a, b Acknowledgement) int {
		return strings.Compare(a.OrderLineID+"\x00"+a.Kind, b.OrderLineID+"\x00"+b.Kind)
	})
	if sorted == nil {
		sorted = []Acknowledgement{}
	}
	c.Status, c.RowVersion = StatusApproved, c.RowVersion+1
	c.Decision = &Decision{Act: act, Approve: true, ValidUntil: until, Acknowledgements: sorted}
	return nil
}

// Revoke revokes the approval by act, whose reason is required (A47 #6; SM-PUR-COMMITMENT approved →
// revoked): at the version (VERSION_CONFLICT), only an approved commitment (INVALID_STATE; one issued
// is cancelled or changed instead, S5-19). Issuing it afterwards is PURCHASE_COMMITMENT_NOT_APPROVED.
// The version moves.
func (c *Commitment) Revoke(expected int64, act Act) error {
	if err := c.version(expected); err != nil {
		return err
	}
	switch {
	case IsCommitted(c.Status):
		return fail(kernel.CodeInvalidState, "/args/commitment_id", "The commitment was issued; it is cancelled or changed, not revoked")
	case c.Status != StatusApproved:
		return fail(kernel.CodeInvalidState, "/args/commitment_id", "The commitment is "+c.Status+", not approved")
	}
	act.At = kernel.Instant(act.At)
	c.Status, c.RowVersion, c.Revocation = StatusRevoked, c.RowVersion+1, &act
	return nil
}

// Issuable decides, before anything upstream is asked, whether the commitment may be issued at
// instant now (PUR-INV-010, A47 #6 and #7), in this order: the version (VERSION_CONFLICT); not
// issued before (INVALID_STATE: once); an approval (PURCHASE_COMMITMENT_NOT_APPROVED for one
// requested, rejected or revoked); still valid (APPROVAL_EXPIRED); its decider still a qualified
// human (qualified; PURCHASE_COMMITMENT_NOT_APPROVED otherwise: the approval no longer counts); the
// draft as bound and not voided (REFERENCE_INVALID, INVALID_STATE); every overbuy of the draft
// approved by the decision (OVERBUY_NOT_APPROVED, PUR-INV-005).
func (c *Commitment) Issuable(expected int64, d Draft, now time.Time, qualified bool) error {
	if err := c.version(expected); err != nil {
		return err
	}
	switch {
	case IsCommitted(c.Status):
		return fail(kernel.CodeInvalidState, "/args/commitment_id", "The commitment was issued; it is issued once")
	case c.Status != StatusApproved || c.Decision == nil || !c.Decision.Approve:
		return fail(CodePurchaseCommitmentNotApproved, "/args/commitment_id", "The commitment is "+c.Status+", not approved")
	case !kernel.Instant(now).Before(c.Decision.ValidUntil):
		return fail(CodeApprovalExpired, "/args/commitment_id", "The approval was valid until "+kernel.FormatTimestamp(c.Decision.ValidUntil))
	case !qualified:
		return fail(CodePurchaseCommitmentNotApproved, "/args/commitment_id", "The commitment's decider is no longer qualified")
	}
	if err := c.intact(d); err != nil {
		return err
	}
	if d.Voided {
		return fail(kernel.CodeInvalidState, "/args/commitment_id", "The draft is voided")
	}
	for _, dv := range d.Deviations {
		if dv.Kind == DeviationOverbuy && !slices.ContainsFunc(c.Decision.Acknowledgements, func(a Acknowledgement) bool {
			return a.OrderLineID == dv.OrderLineID && a.Kind == DeviationOverbuy
		}) {
			return fail(CodeOverbuyNotApproved, "/args/commitment_id", "The overbuy of line "+dv.OrderLineID+" was not approved by the decision")
		}
	}
	return nil
}

// Send records the issue is (A47 #7): sent at a time after the approval and not after now
// (INVALID_KNOWN_VALUE at /args/sent_at). Issuable decided before. The version moves.
func (c *Commitment) Send(is Issue) error {
	at, sent := kernel.Instant(is.At), kernel.Instant(is.SentAt)
	switch {
	case sent.After(at):
		return fail(kernel.CodeInvalidKnownValue, "/args/sent_at", "The commitment was sent no later than it is recorded")
	case sent.Before(c.Decision.At):
		return fail(kernel.CodeInvalidKnownValue, "/args/sent_at", "A commitment is sent after its approval")
	}
	is.At, is.SentAt, is.Evidence = at, sent, slices.Clone(is.Evidence)
	c.Status, c.RowVersion, c.Issue = StatusIssued, c.RowVersion+1, &is
	return nil
}

// Confirm records the supplier's confirmation conf (A47 #7; SM-PUR-COMMITMENT issued → confirmed): at
// the version (VERSION_CONFLICT), of an issued commitment (INVALID_STATE: confirmed already, or not
// issued), received after the commitment was sent and not after now (INVALID_KNOWN_VALUE at
// /args/received_at). As issued, the commitment is confirmed; with deviations it stays issued, open
// for a change (S5-19). The version moves either way.
func (c *Commitment) Confirm(expected int64, conf Confirmation) error {
	if err := c.version(expected); err != nil {
		return err
	}
	switch c.Status {
	case StatusIssued:
	case StatusConfirmed:
		return fail(kernel.CodeInvalidState, "/args/commitment_id", "The supplier confirmed the commitment as issued already")
	default:
		return fail(kernel.CodeInvalidState, "/args/commitment_id", "The commitment is "+c.Status+", not issued")
	}
	at, received := kernel.Instant(conf.At), kernel.Instant(conf.ReceivedAt)
	switch {
	case received.After(at):
		return fail(kernel.CodeInvalidKnownValue, "/args/received_at", "A confirmation is received no later than it is recorded")
	case received.Before(c.Issue.SentAt):
		return fail(kernel.CodeInvalidKnownValue, "/args/received_at", "A confirmation is received after the commitment was sent")
	}
	c.RowVersion++
	conf.At, conf.ReceivedAt, conf.Version, conf.Evidence, conf.Fresh = at, received, c.RowVersion, slices.Clone(conf.Evidence), true
	c.Confirmations = append(c.Confirmations, conf)
	if conf.Result == ResultAsIssued {
		c.Status = StatusConfirmed
	}
	return nil
}

// Fresh returns the confirmation recorded since the commitment was loaded, or nil.
func (c *Commitment) Fresh() *Confirmation {
	for i := range c.Confirmations {
		if c.Confirmations[i].Fresh {
			return &c.Confirmations[i]
		}
	}
	return nil
}
