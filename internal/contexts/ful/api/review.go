package api

import (
	"context"
	"regexp"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Shipment reviews after an order change (S5 unit C4b-FUL, WBS S5-09b FUL part; tactical model
// FUL-AGG-OrderFulfilment entity ShipmentReview, SM-FUL-SHIPMENT-REVIEW; WF-06 step 5 with its
// idempotency and failure mode; event storming PL-09; S5 decisions D-18, D-21; BT10). TRD's
// OrderAmended, OrderCancelled and OrderRemainderClosed reach FUL's Inbox consumer
// ful.shipment_review, which opens one review per order revision and change listing what the change
// leaves stale of FUL's facts of the order; the merchandiser settles it by reference to what was
// actually done with FUL's own commands. The review disposes of nothing itself, and confirmed
// shipments stay facts: they are never items.
//
//	shipment_review.open    system only (the consumer's actor): one review of order OrderID at
//	                        OrderRevision for Cause, opened by TRD's event EventID at the order's row
//	                        version OrderVersion, under the order's fulfilment root. Its items are the
//	                        objects the change leaves stale: every prepared shipment that can no longer
//	                        be confirmed (after an amendment: bound to an earlier order revision; after
//	                        an end: any), its current packing plan and actual packing, its current
//	                        booking and the draft versions of its document set, and every granted,
//	                        unconsumed commercial release of such a shipment; and the granted documents
//	                        release of a confirmed shipment whose payment condition was judged on an
//	                        earlier order revision than the change's, or of a cancelled order (S5 C1b:
//	                        the documents release of a confirmed shipment judges the order's current
//	                        revision, and an order closed short still releases it). Open reviews of the
//	                        order from an earlier order version are superseded. Not routed.
//	shipment_review.settle  human only (merchandiser, the review's responsible role): every item of an
//	                        open review with its disposition and a reason, each of which must have
//	                        happened: shipment_cancelled (shipment.cancel of the item's shipment);
//	                        replaced {shipment_id} (the item's shipment cancelled and the named shipment
//	                        of the same order, bound to the review's revision or a later one, holding the
//	                        item's counterpart: itself for a shipment item, a packing record of the same
//	                        kind — packing.plan or packing.record_actual, the re-plan —, a booking —
//	                        booking.record, the booking correction —, a version of the same document kind
//	                        — shipment_document.prepare —, a release of the same scope —
//	                        commercial_release.evaluate); reevaluated (a release evaluated again since the
//	                        review opened, or — a confirmed shipment's documents release —
//	                        document_release.record since then, which judges it the same way, on the
//	                        order's current revision). no_action, which left a confirmed shipment's
//	                        release as it was, is no longer accepted since S5 C1b (INVALID_KNOWN_VALUE);
//	                        reviews settled before keep it.
//
// Request members in snake_case: shipment_review.settle {review_id, expected_version, items
// [{item_no, disposition, shipment_id, reason}], reason (required when the review has no items)}.
const (
	CommandOpenShipmentReview   = "shipment_review.open"
	CommandSettleShipmentReview = "shipment_review.settle"

	// Reads: one review and an order's reviews (ful.read).
	CommandGetShipmentReview   = "shipment_review.get"
	CommandListShipmentReviews = "shipment_review.list"

	// States of a review (SM-FUL-SHIPMENT-REVIEW).
	ReviewOpen       = "open"
	ReviewSettled    = "settled"
	ReviewSuperseded = "superseded"

	// ReviewResponsibleRole is the role template that settles a review (role templates: 跟单,
	// "订单变更的出运复核"; it holds shipment_review.settle).
	ReviewResponsibleRole = "merchandiser"

	// What opened a review: the TRD event.
	CauseOrderAmended         = "order_amended"
	CauseOrderCancelled       = "order_cancelled"
	CauseOrderRemainderClosed = "order_remainder_closed"

	// Kinds of item.
	ReviewItemShipment = "shipment" // a prepared shipment that can no longer be confirmed
	ReviewItemPacking  = "packing"  // its current packing plan or actual packing
	ReviewItemBooking  = "booking"  // its current booking
	ReviewItemDocument = "document" // a draft version of its document set
	ReviewItemRelease  = "release"  // a granted commercial release judged on what no longer holds

	// Why an item is stale.
	ImpactRevisionChanged = "revision_changed" // the shipment binds an earlier order revision than the amended one
	ImpactOrderCancelled  = "order_cancelled"  // the order was cancelled
	ImpactRemainderClosed = "remainder_closed" // the order closed short

	// What the amended revision does to a line a stale shipment ships.
	LineUnchanged            = "unchanged"             // the shipment's quantity still fits the line
	LineReduced              = "reduced"               // the line no longer leaves room for the shipment's quantity
	LineRemoved              = "line_removed"          // the revision no longer has the line
	LineSpecificationChanged = "specification_changed" // the line's SKU specification or unit is not the one the shipment ships

	// Dispositions of an item.
	DispositionShipmentCancelled = "shipment_cancelled"
	DispositionReplaced          = "replaced"
	DispositionReevaluated       = "reevaluated"
	DispositionNoAction          = "no_action" // no longer accepted (S5 C1b); stored dispositions keep it

	// MaxReviewItems bounds the items of one review.
	MaxReviewItems = 1000
)

var reviewCauses = []string{CauseOrderAmended, CauseOrderCancelled, CauseOrderRemainderClosed}

var reviewDispositions = []string{DispositionShipmentCancelled, DispositionReplaced, DispositionReevaluated, DispositionNoAction}

// ReviewTarget is a line of the amended order revision: Quantity (kernel.QuantityFormat, positive,
// in the line's unit), its SKU specification revision and unit.
type ReviewTarget struct {
	OrderLineID  string
	Quantity     string
	SkuID        string
	SpecRevision int64
	UnitID       string
}

// OpenShipmentReview opens the shipment review of order OrderID at OrderRevision (hash OrderHash)
// for Cause, as TRD's event EventID of the order's row version OrderVersion says it: Lines are the
// amended revision's lines (none after an order's end). IdempotencyKey is the event ID. System only.
type OpenShipmentReview struct {
	IdempotencyKey string
	OrderID        string
	OrderRevision  int64
	OrderHash      string
	OrderVersion   int64
	Cause          string
	EventID        string
	Lines          []ReviewTarget
}

// ReviewItemDisposition is the disposition of item ItemNo: Disposition, the replacement ShipmentID
// for replaced (none otherwise), and the Reason.
type ReviewItemDisposition struct {
	ItemNo      int
	Disposition string
	ShipmentID  string
	Reason      string
}

// SettleShipmentReview settles review ReviewID at ExpectedVersion with the disposition of every item;
// Reason is required when the review has no items. Human only.
type SettleShipmentReview struct {
	IdempotencyKey  string
	ReviewID        string
	ExpectedVersion int64
	Items           []ReviewItemDisposition
	Reason          string
}

func (OpenShipmentReview) fulCommand()   {}
func (SettleShipmentReview) fulCommand() {}

var orderHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks the closed contract.
func (c OpenShipmentReview) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case c.OrderRevision < 1 || c.OrderRevision > maxVersion:
		return schema("/args/order_revision")
	case !orderHashPattern.MatchString(c.OrderHash):
		return schema("/args/order_hash")
	case c.OrderVersion < 1:
		return schema("/args/order_version")
	case !slices.Contains(reviewCauses, c.Cause):
		return schema("/args/cause")
	case !kernel.ValidIdentifier(c.EventID):
		return schema("/args/event_id")
	case len(c.Lines) > MaxLines:
		return schema("/args/lines")
	case c.Cause != CauseOrderAmended && len(c.Lines) > 0:
		return schema("/args/lines")
	}
	seen := map[string]bool{}
	for i, l := range c.Lines {
		p := index("/args/lines", i)
		q, ok := Quantity(l.Quantity)
		switch {
		case !kernel.ValidIdentifier(l.OrderLineID):
			return schema(p + "/order_line_id")
		case !ok || q.Sign() <= 0:
			return schema(p + "/quantity")
		case !kernel.ValidIdentifier(l.SkuID) || l.SpecRevision < 1:
			return schema(p + "/sku_id")
		case !kernel.ValidIdentifier(l.UnitID):
			return schema(p + "/unit_id")
		case seen[l.OrderLineID]:
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/order_line_id", "")
		}
		seen[l.OrderLineID] = true
	}
	return nil
}

// Validate checks the closed contract: each item once, a known disposition, a replacement shipment
// exactly for replaced, a reason for each; a reason for the review when it states no item.
func (c SettleShipmentReview) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ReviewID):
		return schema("/args/review_id")
	}
	if err := version(c.ExpectedVersion); err != nil {
		return err
	}
	switch {
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
		switch {
		case it.ItemNo < 1 || it.ItemNo > MaxReviewItems || seen[it.ItemNo]:
			return schema(p + "/item_no")
		case !slices.Contains(reviewDispositions, it.Disposition):
			return schema(p + "/disposition")
		case (it.Disposition == DispositionReplaced) != (it.ShipmentID != ""):
			return schema(p + "/shipment_id")
		case it.ShipmentID != "" && !kernel.ValidIdentifier(it.ShipmentID):
			return schema(p + "/shipment_id")
		case !Reason(it.Reason):
			return schema(p + "/reason")
		}
		seen[it.ItemNo] = true
	}
	return nil
}

// ReviewLineView is what a shipment item says of one of its lines after an amendment: the order
// line, the SKU specification, unit and quantity the shipment ships, what the amended revision states
// of the line (TargetQuantity, empty when the line is gone), what confirmed shipments shipped of it
// over every revision (ShippedQuantity), what is left of it (RemainingQuantity) and the Impact. After
// an order's end the impact is the end's and nothing is left.
type ReviewLineView struct {
	LineID            string
	OrderLineID       string
	SkuID             string
	SpecRevision      int64
	UnitID            string
	Quantity          string
	TargetQuantity    string
	ShippedQuantity   string
	RemainingQuantity string
	Impact            string
}

// ReviewDispositionView is how an item was settled: the disposition, the replacement shipment and
// the order revision it binds (replaced), the counterpart found (Ref: plan/<seq>, actual/<seq>,
// booking/<version>, <kind>/<version> of the replacement's document set, or the release ID) and its
// state (Status), when it happened (At: when the item's shipment was cancelled, or when the release
// was last evaluated) and the reason.
type ReviewDispositionView struct {
	Disposition string
	ShipmentID  string
	Revision    int64
	Ref         string
	Status      string
	At          time.Time
	Reason      string
}

// ShipmentReviewItemView is one item of a review as FUL knew it when the review opened: its kind,
// the shipment it hangs on (state, row version and the order revision it binds), why it is stale,
// the object — the shipment's lines, a packing record (kind, sequence number, cartons, packages), a
// booking version (number, vessel, voyage, ETD), a document version (set, kind, version, number), a
// release (ID, scope, state, row version, evaluations) —, the canonical JSON Facts the item stores,
// and its disposition.
type ShipmentReviewItemView struct {
	ItemNo             int
	Kind               string
	ShipmentID         string
	ShipmentStatus     string
	ShipmentVersion    int64
	ShipmentRevision   int64
	Impact             string
	Lines              []ReviewLineView
	PackingKind        string
	PackingSeq         int64
	Cartons            int64
	Packages           int64
	BookingVersion     int64
	BookingNumber      string
	Vessel             string
	Voyage             string
	ETD                string
	DocumentSetID      string
	DocumentKind       string
	DocumentVersion    int64
	DocumentNumber     string
	ReleaseID          string
	ReleaseScope       string
	ReleaseStatus      string
	ReleaseVersion     int64
	ReleaseEvaluations int64
	Facts              string
	Disposition        *ReviewDispositionView
}

// ShipmentReviewView is a review as the reads show it: the order revision and change it follows, the
// TRD event and order row version that opened it, its state and version, the responsible role, the
// items, who opened (the consumer's system actor) and settled it, and the review that superseded it.
type ShipmentReviewView struct {
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
	Items           []ShipmentReviewItemView
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

// OrderShipmentReviews is the shipment reviews of an order, by order version.
type OrderShipmentReviews struct {
	OrderID         string
	ShipmentReviews []ShipmentReviewView
}

// ReviewCommands is FUL's command entry point for shipment reviews a person sends.
type ReviewCommands interface {
	SettleShipmentReview(ctx context.Context, cmd SettleShipmentReview) (Receipt, error)
}

// ReviewQueries is FUL's audited read side of shipment reviews (ful.read).
type ReviewQueries interface {
	// GetShipmentReview returns a review of an order the caller reads through TRD, audited as
	// shipment_review.get: NOT_FOUND otherwise.
	GetShipmentReview(ctx context.Context, reviewID string) (ShipmentReviewView, error)
	// ListShipmentReviews returns the reviews of an order the caller reads through TRD, by order
	// version, audited as shipment_review.list: NOT_FOUND otherwise.
	ListShipmentReviews(ctx context.Context, orderID string) (OrderShipmentReviews, error)
}
