package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Purchase commitment changes and cancellation (WBS S5-19; tactical model SM-PUR-COMMITMENT,
// SM-PUR-COMMITMENT-CHANGE, PUR-INV-011; A47 #6 and #7 of Python V0.50's
// docs/T04_A46_PURCHASE_DRAFT_REPORT_V0.1.md, design only). An issued commitment is never rewritten:
// what it buys moves by revisions — revision 1 is the commitment as issued, every change applied and
// the cancellation make the next one.
//
//	purchase.commitment.change.prepare  human or AI (purchaser): the full next revision of an issued
//	                                    or confirmed commitment, bound to its revision in force —
//	                                    every line of its draft with the quantity allocated to it
//	                                    (0: no longer bought), an optional overbuy, the condition line
//	                                    it is bought under (a new supply condition revision for a new
//	                                    price) and an optional supplier date (the condition's
//	                                    otherwise) —, with a reason and the supplier's EVD evidence.
//	                                    PUR prepares the draft's request again with these on the facts
//	                                    as they are now, against the demand allocation without the
//	                                    commitment's own entries (an increase beyond the line's room
//	                                    is PURCHASE_ALLOCATION_EXCEEDED), re-checks every reference
//	                                    through its owner's guard and seals the revision's document
//	                                    (contract brant.purchase-commitment-revision/0.1.0). No line
//	                                    buys less than its verified deliveries
//	                                    (SUPPLIER_DELIVERY_EXCEEDED).
//	purchase.commitment.change.decide   human only (order reviewer), independent of the change's
//	                                    preparer and the commitment's requester
//	                                    (INDEPENDENT_REVIEW_REQUIRED): approve or reject (reason
//	                                    required). An approval prepares the revision again inside the
//	                                    decision's transaction and requires it equal to the sealed one
//	                                    field by field (PURCHASE_DRAFT_STALE), re-runs the guards
//	                                    (supply condition current, order active at the revision it
//	                                    binds, SKU, configuration, party) and requires every deviation
//	                                    acknowledged with a reason — a draft's (source_differs,
//	                                    cost_above_costing, cost_not_comparable, overbuy) and a
//	                                    change's (delivery_changed, delivery_late, moq_not_met,
//	                                    order_multiple_invalid).
//	purchase.commitment.change.apply    human only (purchaser): the approved change becomes the
//	                                    commitment's next revision while the commitment is still at
//	                                    the revision it is bound to (PURCHASE_COMMITMENT_CHANGE_STALE:
//	                                    of two changes of one revision the first applied wins), after
//	                                    the same re-checks; the allocation moves to the revision's
//	                                    quantities; the commitment is issued again until the supplier
//	                                    confirms that revision (purchase.supplier_confirmation.record,
//	                                    optionally stating the revision).
//	purchase.commitment.cancel          human only (purchaser): an issued or confirmed commitment, with
//	                                    a reason and the supplier's EVD evidence; each line keeps what
//	                                    was verified delivered of it (they stay settlement and
//	                                    shipment sources) and the allocation of the rest is released.
//
// A short receipt is a change to exactly the verified quantities: the commitment then counts as
// fulfilled (CommitmentView.Fulfilled; state fulfilled in DownstreamCommitments v1).
//
// Request members in snake_case: purchase.commitment.change.prepare {change_id, commitment_id,
// revision, lines [{order_line_id, quantity, overbuy {quantity, reason}, source {kind, id, revision,
// condition_line_id}, supplier_date (YYYY-MM-DD)}], reason, evidence [{source_id, locator}]};
// purchase.commitment.change.decide {change_id, expected_version, change_hash, decision (approve or
// reject), acknowledgements [{order_line_id, kind, reason}] (approve), reason (reject; optional to
// approve)}; purchase.commitment.change.apply {change_id, expected_version};
// purchase.commitment.cancel {commitment_id, expected_version, reason, evidence [{source_id,
// locator}]}.

// Change commands, reads and events (command catalog, Context Map PUR facts PurchaseCommitmentChanged
// and PurchaseCommitmentCancelled).
const (
	CommandPrepareCommitmentChange = "purchase.commitment.change.prepare"
	CommandDecideCommitmentChange  = "purchase.commitment.change.decide"
	CommandApplyCommitmentChange   = "purchase.commitment.change.apply"
	CommandCancelCommitment        = "purchase.commitment.cancel"

	// Reads: one change and a commitment's changes (pur.read).
	CommandGetCommitmentChange  = "purchase.commitment.change.get"
	CommandGetCommitmentChanges = "purchase.commitment.change.list"

	EventPurchaseCommitmentChanged   = "idp-brant.pur.purchase_commitment_changed.v1"
	EventPurchaseCommitmentCancelled = "idp-brant.pur.purchase_commitment_cancelled.v1"

	// CommitmentCancelled is a commitment's terminal state after purchase.commitment.cancel;
	// DownstreamStateFulfilled is DownstreamCommitments v1's state of an issued or confirmed commitment
	// whose every line is verified delivered in full.
	CommitmentCancelled      = "cancelled"
	DownstreamStateFulfilled = "fulfilled"

	// States of a change (SM-PUR-COMMITMENT-CHANGE).
	ChangeDraft    = "draft"
	ChangeApproved = "approved"
	ChangeRejected = "rejected"
	ChangeApplied  = "applied"

	// Deviations a change's approval acknowledges besides a draft's.
	DeviationDeliveryChanged = "delivery_changed"       // the supplier's date is not the supply condition's
	DeviationDeliveryLate    = "delivery_late"          // the supplier's date is after the order's required date
	DeviationMOQNotMet       = "moq_not_met"            // the revision buys less than the condition's MOQ
	DeviationOrderMultiple   = "order_multiple_invalid" // the revision buys no multiple of the order multiple

	// RevisionContract names a commitment revision's document.
	RevisionContract = "brant.purchase-commitment-revision/0.1.0"

	// Kinds of revision after the first.
	RevisionChange = "change"
	RevisionCancel = "cancel"
)

var changeDeviationKinds = []string{DeviationSourceDiffers, DeviationCostAbove, DeviationNotComparable, DeviationOverbuy,
	DeviationDeliveryChanged, DeviationDeliveryLate, DeviationMOQNotMet, DeviationOrderMultiple}

// ChangeLine is one line of a change: Quantity (kernel.QuantityFormat, zero or positive, in the
// order line's unit) of order line OrderLineID allocated to the commitment, bought under Source
// (required unless Quantity is zero), an optional Overbuy, and an optional SupplierDate
// (YYYY-MM-DD; the condition's when empty).
type ChangeLine struct {
	OrderLineID  string
	Quantity     string
	Overbuy      *Overbuy
	Source       *SourceRef
	SupplierDate string
}

// PrepareCommitmentChange prepares change ChangeID of commitment CommitmentID at its revision
// Revision. Human or AI.
type PrepareCommitmentChange struct {
	IdempotencyKey string
	ChangeID       string
	CommitmentID   string
	Revision       int64
	Lines          []ChangeLine
	Reason         string
	Evidence       []Evidence
}

// DecideCommitmentChange approves or rejects change ChangeID at ExpectedVersion, stating the change
// hash the decider reviewed. Human only.
type DecideCommitmentChange struct {
	IdempotencyKey   string
	ChangeID         string
	ExpectedVersion  int64
	ChangeHash       string
	Decision         string
	Acknowledgements []Acknowledgement
	Reason           string
}

// ApplyCommitmentChange applies change ChangeID at ExpectedVersion. Human only.
type ApplyCommitmentChange struct {
	IdempotencyKey  string
	ChangeID        string
	ExpectedVersion int64
}

// CancelCommitment cancels commitment CommitmentID at ExpectedVersion for Reason, on the supplier's
// Evidence. Human only.
type CancelCommitment struct {
	IdempotencyKey  string
	CommitmentID    string
	ExpectedVersion int64
	Reason          string
	Evidence        []Evidence
}

func (PrepareCommitmentChange) purCommand() {}
func (DecideCommitmentChange) purCommand()  {}
func (ApplyCommitmentChange) purCommand()   {}
func (CancelCommitment) purCommand()        {}

func date(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil && len(s) == len(time.DateOnly)
}

// Validate checks the closed contract: identifiers, a revision, 1 to 200 lines with a quantity of
// zero or more, each order line once (DUPLICATE_LINE_ID), a complete source when stated, a positive
// overbuy quantity, a calendar date, a reason and 1 to 20 evidence sources. Whether the lines are
// the commitment's, and what a zero line may state, are the rules'.
func (c PrepareCommitmentChange) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ChangeID):
		return schema("/args/change_id")
	case !kernel.ValidIdentifier(c.CommitmentID):
		return schema("/args/commitment_id")
	case !version(c.Revision):
		return schema("/args/revision")
	case len(c.Lines) < 1 || len(c.Lines) > MaxLines:
		return schema("/args/lines")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	seen := map[string]bool{}
	for i, l := range c.Lines {
		p := index("/args/lines", i)
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		switch {
		case !kernel.ValidIdentifier(l.OrderLineID):
			return schema(p + "/order_line_id")
		case err != nil || q.Sign() < 0:
			return schema(p + "/quantity")
		case l.SupplierDate != "" && !date(l.SupplierDate):
			return schema(p + "/supplier_date")
		}
		if s := l.Source; s != nil {
			switch {
			case s.Kind != KindSupplierOffer && s.Kind != KindPurchaseTerms:
				return schema(p + "/source/kind")
			case !kernel.ValidIdentifier(s.ID):
				return schema(p + "/source/id")
			case !version(s.Revision):
				return schema(p + "/source/revision")
			case !kernel.ValidIdentifier(s.ConditionLineID):
				return schema(p + "/source/condition_line_id")
			}
		}
		if o := l.Overbuy; o != nil {
			if err := quantity(o.Quantity, p+"/overbuy/quantity"); err != nil {
				return err
			}
			if o.Reason != "" && !Reason(o.Reason) {
				return schema(p + "/overbuy/reason")
			}
		}
		if seen[l.OrderLineID] {
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/order_line_id", "")
		}
		seen[l.OrderLineID] = true
	}
	return validEvidence(c.Evidence, "/args/evidence")
}

// Validate checks the closed contract: an approval states its acknowledgements, each of a known kind
// with a reason and at most once; a rejection states none, and its reason.
func (c DecideCommitmentChange) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ChangeID):
		return schema("/args/change_id")
	case !version(c.ExpectedVersion):
		return schema("/args/expected_version")
	case !hash(c.ChangeHash):
		return schema("/args/change_hash")
	case c.Reason != "" && !Reason(c.Reason):
		return schema("/args/reason")
	}
	switch c.Decision {
	case DecisionApprove:
		if len(c.Acknowledgements) > MaxAcknowledged {
			return schema("/args/acknowledgements")
		}
		type key struct{ line, kind string }
		seen := map[key]bool{}
		for i, a := range c.Acknowledgements {
			p := index("/args/acknowledgements", i)
			k := key{a.OrderLineID, a.Kind}
			switch {
			case !kernel.ValidIdentifier(a.OrderLineID):
				return schema(p + "/order_line_id")
			case !contains(changeDeviationKinds, a.Kind):
				return schema(p + "/kind")
			case !Reason(a.Reason):
				return schema(p + "/reason")
			case seen[k]:
				return schema(p)
			}
			seen[k] = true
		}
	case DecisionReject:
		switch {
		case c.Acknowledgements != nil:
			return schema("/args/acknowledgements")
		case c.Reason == "":
			return schema("/args/reason")
		}
	default:
		return schema("/args/decision")
	}
	return nil
}

// Validate checks the closed contract.
func (c ApplyCommitmentChange) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ChangeID):
		return schema("/args/change_id")
	case !version(c.ExpectedVersion):
		return schema("/args/expected_version")
	}
	return nil
}

// Validate checks the closed contract: a reason and 1 to 20 evidence sources of the supplier's.
func (c CancelCommitment) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CommitmentID):
		return schema("/args/commitment_id")
	case !version(c.ExpectedVersion):
		return schema("/args/expected_version")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return validEvidence(c.Evidence, "/args/evidence")
}

// ChangeReceipt is the answer of the change commands: the receipt (ObjectID the change ID, RowVersion
// its row version — 1 draft, 2 decided, 3 applied —, Status its state) and what it binds: the
// commitment, the revision it is bound to, the revision its application made (0 before) and the
// hash of its proposed revision's document.
type ChangeReceipt struct {
	Receipt
	CommitmentID    string
	BaseRevision    int64
	AppliedRevision int64
	ChangeHash      string
}

// RevisionLineView is what a revision buys of one line: the allocated quantity, the overbuy, their
// sum and the supplier's date.
type RevisionLineView struct {
	OrderLineID       string
	AllocatedQuantity string
	OverbuyQuantity   string
	PurchaseQuantity  string
	SupplierDate      string
}

// DeviationView is a deviation of a change's proposed revision.
type DeviationView struct {
	OrderLineID string
	Kind        string
}

// ChangeView is a change as the reads show it: what it binds, its state, its proposed revision's
// document (the exact bytes ChangeHash covers, contract RevisionContract) with its lines and the
// deviations an approval acknowledges, the reason and evidence, who prepared, decided and applied it.
type ChangeView struct {
	ChangeID         string
	CommitmentID     string
	Status           string
	RowVersion       int64
	BaseRevision     int64
	DraftID          string
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	SupplierID       string
	Currency         string
	SupplyValidUntil time.Time
	ChangeHash       string
	Document         string
	Lines            []RevisionLineView
	Deviations       []DeviationView
	Reason           string
	Evidence         []Evidence
	PreparedBy       kernel.ActorRef
	PreparedAt       time.Time
	PrepareEpoch     int64
	Decision         string
	DecidedBy        kernel.ActorRef
	DecidedAt        time.Time
	DecideEpoch      int64
	DecisionReason   string
	Acknowledgements []AcknowledgementView
	AppliedBy        kernel.ActorRef
	AppliedAt        time.Time
	ApplyEpoch       int64
	AppliedRevision  int64
}

// RevisionView is a commitment revision after the first: a change's or the cancellation's, the order
// revision and hash it binds, the hash of its document (a change's) and its lines.
type RevisionView struct {
	Revision       int64
	Kind           string
	ChangeID       string
	OrderRevision  int64
	OrderHash      string
	DocumentHash   string
	Lines          []RevisionLineView
	CreatedBy      kernel.ActorRef
	CreatedAt      time.Time
	AuthorityEpoch int64
}

// CommitmentLineView is what a commitment buys of a line at its revision in force and what the
// supplier's verified deliveries add up to for it.
type CommitmentLineView struct {
	OrderLineID       string
	AllocatedQuantity string
	OverbuyQuantity   string
	PurchaseQuantity  string
	VerifiedQuantity  string
	SupplierDate      string
}

// ChangeCommands is PUR's command entry point for commitment changes and cancellation.
type ChangeCommands interface {
	PrepareCommitmentChange(ctx context.Context, cmd PrepareCommitmentChange) (ChangeReceipt, error)
	DecideCommitmentChange(ctx context.Context, cmd DecideCommitmentChange) (ChangeReceipt, error)
	ApplyCommitmentChange(ctx context.Context, cmd ApplyCommitmentChange) (ChangeReceipt, error)
	CancelCommitment(ctx context.Context, cmd CancelCommitment) (CommitmentReceipt, error)
}

// ChangeQueries is PUR's audited read side of commitment changes.
type ChangeQueries interface {
	// GetCommitmentChange returns a change of a commitment the caller sees, audited as
	// purchase.commitment.change.get (pur.read): NOT_FOUND otherwise; REFERENCE_INVALID when the
	// caller no longer reads its order.
	GetCommitmentChange(ctx context.Context, changeID string) (ChangeView, error)
	// GetCommitmentChanges returns the changes of a commitment the caller sees, by change ID, audited
	// as purchase.commitment.change.list (pur.read).
	GetCommitmentChanges(ctx context.Context, commitmentID string) ([]ChangeView, error)
}
