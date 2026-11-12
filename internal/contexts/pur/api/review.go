package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Allocation reviews after an order change (WBS S5-09b, PUR part; tactical model
// PUR-AGG-DemandAllocation entity AllocationReview, SM-PUR-ALLOCATION-REVIEW; WF-06 step 4; event
// storming PL-09, ES-09 step 5; S5 decisions D-18, D-21; BT10, SIM-02 steps 7 and 9). TRD's
// OrderAmended, OrderCancelled and OrderRemainderClosed reach PUR's Inbox consumer
// pur.allocation_review, which opens one review per order revision and change listing what the
// change touches of PUR's facts; a person settles it by reference to the dispositions actually made
// with PUR's own commands. The review disposes of nothing itself.
//
//	allocation_review.open    system only (the consumer's actor): one review of order OrderID at
//	                          OrderRevision for Cause, opened by TRD's event EventID at the order's row
//	                          version OrderVersion, with one item per allocation of an order line above
//	                          what the order now needs of it, per prepared draft never committed that
//	                          holds such a line (purchase.draft.void disposes of it) and per issued or
//	                          confirmed commitment that buys on such a line (purchase.commitment.change.*
//	                          or purchase.commitment.cancel). Open reviews of the order from an earlier
//	                          order version are superseded. Not routed.
//	allocation_review.settle  human only (purchaser, the review's responsible role): every item of an
//	                          open review with its disposition and a reason — draft_voided {draft_id},
//	                          commitment_changed {change_id}, commitment_cancelled {commitment_id} — each
//	                          of which must have happened, or no_action when nothing is to be done.
//
// Request members in snake_case: allocation_review.settle {review_id, expected_version, items
// [{item_no, disposition, draft_id, change_id, commitment_id, reason}], reason (required when the
// review has no items)}.
const (
	CommandOpenAllocationReview   = "allocation_review.open"
	CommandSettleAllocationReview = "allocation_review.settle"

	// Reads: one review and an order's reviews (pur.read).
	CommandGetAllocationReview  = "allocation_review.get"
	CommandGetAllocationReviews = "allocation_review.list"

	// States of a review (SM-PUR-ALLOCATION-REVIEW).
	ReviewOpen       = "open"
	ReviewSettled    = "settled"
	ReviewSuperseded = "superseded"

	// ResponsibleRole is the role template that settles a review (role templates: 采购, "订单变更的分配复核").
	ResponsibleRole = "purchaser"

	// What opened a review: the TRD event.
	CauseOrderAmended         = "order_amended"
	CauseOrderCancelled       = "order_cancelled"
	CauseOrderRemainderClosed = "order_remainder_closed"

	// Kinds of item.
	ItemAllocation = "allocation" // an order line allocated above what the order now needs
	ItemDraft      = "draft"      // a prepared draft never committed holding such a line
	ItemCommitment = "commitment" // an issued or confirmed commitment buying on such a line

	// Why an order line is touched.
	ImpactReduced              = "reduced"               // its quantity fell below what is allocated
	ImpactLineRemoved          = "line_removed"          // the revision no longer has the line
	ImpactSpecificationChanged = "specification_changed" // the line's SKU specification or unit is not the one bought
	ImpactOrderCancelled       = "order_cancelled"       // the order was cancelled: nothing more is needed
	ImpactRemainderClosed      = "remainder_closed"      // the order closed short: no more than was shipped is needed

	// Dispositions of an item.
	DispositionDraftVoided         = "draft_voided"
	DispositionCommitmentChanged   = "commitment_changed"
	DispositionCommitmentCancelled = "commitment_cancelled"
	DispositionNoAction            = "no_action"

	// MaxReviewItems bounds the items of one review.
	MaxReviewItems = 1000
)

var reviewCauses = []string{CauseOrderAmended, CauseOrderCancelled, CauseOrderRemainderClosed}

// ReviewTarget is what the order needs of one of its lines after the change: Quantity
// (kernel.QuantityFormat, zero or more, in the line's unit) and, after an amendment, the SKU
// specification revision and unit the revision states (empty after an order's end).
type ReviewTarget struct {
	OrderLineID  string
	Quantity     string
	SkuID        string
	SpecRevision int64
	UnitID       string
}

// OpenAllocationReview opens the allocation review of order OrderID at OrderRevision (hash
// OrderHash) for Cause, as TRD's event EventID of the order's row version OrderVersion says it:
// Lines are the order's lines after the change. IdempotencyKey is the event ID. System only.
type OpenAllocationReview struct {
	IdempotencyKey string
	OrderID        string
	OrderRevision  int64
	OrderHash      string
	OrderVersion   int64
	Cause          string
	EventID        string
	Lines          []ReviewTarget
}

// ReviewItemDisposition is the disposition of item ItemNo: Disposition, the object it names — the
// voided DraftID, the applied ChangeID, the cancelled CommitmentID, none for no_action — and the
// Reason.
type ReviewItemDisposition struct {
	ItemNo       int
	Disposition  string
	DraftID      string
	ChangeID     string
	CommitmentID string
	Reason       string
}

// SettleAllocationReview settles review ReviewID at ExpectedVersion with the disposition of every
// item; Reason is required when the review has no items. Human only.
type SettleAllocationReview struct {
	IdempotencyKey  string
	ReviewID        string
	ExpectedVersion int64
	Items           []ReviewItemDisposition
	Reason          string
}

func (OpenAllocationReview) purCommand()   {}
func (SettleAllocationReview) purCommand() {}

// Validate checks the closed contract.
func (c OpenAllocationReview) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !version(c.OrderRevision):
		return schema("/args/order_revision")
	case !hash(c.OrderHash):
		return schema("/args/order_hash")
	case c.OrderVersion < 1:
		return schema("/args/order_version")
	case !contains(reviewCauses, c.Cause):
		return schema("/args/cause")
	case !kernel.ValidIdentifier(c.EventID):
		return schema("/args/event_id")
	case len(c.Lines) > MaxLines:
		return schema("/args/lines")
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
		case l.SkuID != "" && (!kernel.ValidIdentifier(l.SkuID) || !version(l.SpecRevision)):
			return schema(p + "/sku_id")
		case l.UnitID != "" && !kernel.ValidIdentifier(l.UnitID):
			return schema(p + "/unit_id")
		case seen[l.OrderLineID]:
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/order_line_id", "")
		}
		seen[l.OrderLineID] = true
	}
	return nil
}

var dispositions = []string{DispositionDraftVoided, DispositionCommitmentChanged, DispositionCommitmentCancelled, DispositionNoAction}

// Validate checks the closed contract: each item once, a known disposition naming exactly the
// object it needs, a reason for each; a reason for the review when it states no item.
func (c SettleAllocationReview) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ReviewID):
		return schema("/args/review_id")
	case !version(c.ExpectedVersion):
		return schema("/args/expected_version")
	case len(c.Items) > MaxReviewItems:
		return schema("/args/items")
	case c.Reason != "" && !Reason(c.Reason):
		return schema("/args/reason")
	case len(c.Items) == 0 && c.Reason == "":
		return schema("/args/reason")
	}
	seen := map[int]bool{}
	for i, it := range c.Items {
		p := index("/args/items", i)
		named := []struct{ kind, id string }{{DispositionDraftVoided, it.DraftID}, {DispositionCommitmentChanged, it.ChangeID},
			{DispositionCommitmentCancelled, it.CommitmentID}}
		switch {
		case it.ItemNo < 1 || it.ItemNo > MaxReviewItems:
			return schema(p + "/item_no")
		case seen[it.ItemNo]:
			return schema(p + "/item_no")
		case !contains(dispositions, it.Disposition):
			return schema(p + "/disposition")
		case !Reason(it.Reason):
			return schema(p + "/reason")
		}
		seen[it.ItemNo] = true
		for _, n := range named {
			if (n.kind == it.Disposition && !kernel.ValidIdentifier(n.id)) || (n.kind != it.Disposition && n.id != "") {
				return schema(p + "/" + referenceMember(n.kind))
			}
		}
	}
	return nil
}

// referenceMember is the request member that names the object of disposition kind.
func referenceMember(kind string) string {
	switch kind {
	case DispositionDraftVoided:
		return "draft_id"
	case DispositionCommitmentChanged:
		return "change_id"
	}
	return "commitment_id"
}

// ReviewLineView is what an item says of one order line: why it is touched (Impact), what the order
// now needs of it (TargetQuantity), and, as the item's kind has them, PUR's allocation of it
// (DemandQuantity at DemandRevision, AllocatedQuantity, ExcessQuantity), what a draft allocates and
// overbuys of it, and what a commitment buys of it at its revision and what is verified delivered.
// Quantities are in the line's unit.
type ReviewLineView struct {
	OrderLineID       string
	Impact            string
	TargetQuantity    string
	UnitID            string
	DemandQuantity    string
	DemandRevision    int64
	AllocatedQuantity string
	ExcessQuantity    string
	OverbuyQuantity   string
	PurchaseQuantity  string
	VerifiedQuantity  string
}

// ReviewHolderView is one draft holding an allocation item's line: its effective Quantity and, once
// a commitment of the draft was issued, that commitment's ID, state and revision.
type ReviewHolderView struct {
	DraftID            string
	Quantity           string
	CommitmentID       string
	CommitmentStatus   string
	CommitmentRevision int64
}

// ReviewDispositionView is how an item was settled: the disposition, the object it names, what that
// object shows of it (Revision: the commitment revision the change or the cancellation made; At:
// when the draft was voided, the change applied or the commitment cancelled) and the reason.
type ReviewDispositionView struct {
	Disposition  string
	DraftID      string
	ChangeID     string
	CommitmentID string
	Revision     int64
	At           time.Time
	Reason       string
}

// ReviewItemView is one item of a review as PUR knew it when the review opened: its kind, the object
// it is about (an order line, a draft, a commitment at CommitmentRevision), the draft's or
// commitment's state, supplier, purchase order number and whether it was fulfilled, the lines, the
// holders of an allocation item, the canonical JSON Facts the item stores, and its disposition.
type ReviewItemView struct {
	ItemNo             int
	Kind               string
	OrderLineID        string
	DraftID            string
	DraftHash          string
	CommitmentID       string
	CommitmentRevision int64
	Status             string
	SupplierID         string
	PONumber           string
	Fulfilled          bool
	Lines              []ReviewLineView
	Holders            []ReviewHolderView
	Facts              string
	Disposition        *ReviewDispositionView
}

// AllocationReviewView is a review as the reads show it: the order revision and change it follows,
// the TRD event and order row version that opened it, its state and version, the responsible role,
// the items, who opened (the consumer's system actor) and settled it, and the review that superseded
// it.
type AllocationReviewView struct {
	ReviewID        string
	OrderID         string
	OrderRevision   int64
	OrderHash       string
	OrderVersion    int64
	Cause           string
	EventID         string
	Status          string
	RowVersion      int64
	ResponsibleRole string
	Items           []ReviewItemView
	OpenedBy        kernel.ActorRef
	OpenedAt        time.Time
	OpenEpoch       int64
	SettledBy       kernel.ActorRef
	SettledAt       time.Time
	SettleEpoch     int64
	SettleReason    string
	SupersededBy    string
	SupersededAt    time.Time
}

// ReviewCommands is PUR's command entry point for allocation reviews a person sends.
type ReviewCommands interface {
	SettleAllocationReview(ctx context.Context, cmd SettleAllocationReview) (Receipt, error)
}

// ReviewQueries is PUR's audited read side of allocation reviews.
type ReviewQueries interface {
	// GetAllocationReview returns a review of an order the caller reads, audited as
	// allocation_review.get (pur.read): NOT_FOUND otherwise.
	GetAllocationReview(ctx context.Context, reviewID string) (AllocationReviewView, error)
	// GetAllocationReviews returns the reviews of an order the caller reads, by order version, audited
	// as allocation_review.list (pur.read).
	GetAllocationReviews(ctx context.Context, orderID string) ([]AllocationReviewView, error)
}
