package api

import (
	"context"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The payment plan review (S5 unit C4b-STL, WBS S5-09b; tactical model STL-AGG-OrderAccount, entity
// PaymentPlanReview; SM-STL-PAYMENT-PLAN-REVIEW; WF-06; event storming PL-09; S5 plan D-4, D-18,
// D-21). When TRD amends an order, the order account follows the new order revision.
//
//	payment_plan_review.open    human or system (payment_plan_review.open; STL's consumer of TRD's
//	                            OrderAmended as system:inbox.stl.order_account): moves the order
//	                            account of an active order to TRD's current revision with its payment
//	                            stages and opens the review of (order, revision), whose items are what
//	                            no longer fits — a stage that is gone, a stage with more applied than it
//	                            now asks, a changed credit portion. Applications made earlier remain
//	                            facts and stand on the stage of the same ID. An open review of an
//	                            earlier revision is superseded. An order without an account gets one at
//	                            that revision, with a review that has no items. The same revision
//	                            again answers the review as it is (replayed).
//	payment_plan_review.settle  human only (payment_plan_review.settle; template treasury): settles the
//	                            open review of the revision the account holds with a reason, disposing
//	                            of every item once as accepted or corrected (a corrected stage item must
//	                            be corrected by then: nothing standing on a gone stage, no more than a
//	                            stage asks).
//
// Request members: payment_plan_review.open {order_id, order_revision, order_hash};
// payment_plan_review.settle {order_id, order_revision, expected_version (the order account's
// version), reason, items [{seq, disposition, note}]}.
const (
	CommandOpenReview   = "payment_plan_review.open"
	CommandSettleReview = "payment_plan_review.settle"

	PermissionOpenReview   = "payment_plan_review.open"
	PermissionSettleReview = "payment_plan_review.settle"

	// Reads.
	CommandListReviews = "payment_plan_review.list"

	// Review states (SM-STL-PAYMENT-PLAN-REVIEW).
	ReviewOpen       = "open"
	ReviewSettled    = "settled"
	ReviewSuperseded = "superseded"

	// Item kinds.
	ItemStageRemoved     = "stage_removed"
	ItemStageOverApplied = "stage_over_applied"
	ItemCreditPortion    = "credit_portion_changed"

	// Dispositions of an item.
	DispositionAccepted  = "accepted"
	DispositionCorrected = "corrected"

	maxReviewItems = 200
	maxNote        = 500
)

// OpenPaymentPlanReview moves order OrderID's account to OrderRevision at OrderHash, TRD's current
// revision of the active order, and opens its payment plan review.
type OpenPaymentPlanReview struct {
	IdempotencyKey string
	OrderID        string
	OrderRevision  int64
	OrderHash      string
}

// ReviewDisposition disposes of the review item numbered Seq as Disposition (accepted or corrected)
// with Note.
type ReviewDisposition struct {
	Seq         int
	Disposition string
	Note        string
}

// SettlePaymentPlanReview settles the open review of order OrderID's revision OrderRevision for
// Reason, the order account being at ExpectedVersion, with one disposition per item. Human only.
type SettlePaymentPlanReview struct {
	IdempotencyKey  string
	OrderID         string
	OrderRevision   int64
	ExpectedVersion int64
	Reason          string
	Items           []ReviewDisposition
}

func (OpenPaymentPlanReview) stlCommand()   {}
func (SettlePaymentPlanReview) stlCommand() {}

// Validate checks the closed contract.
func (c OpenPaymentPlanReview) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !versionOK(c.OrderRevision):
		return schema("/args/order_revision")
	case !hash(c.OrderHash):
		return schema("/args/order_hash")
	}
	return nil
}

// Validate checks the closed contract: identifiers, versions, the reason, and up to 200 items with
// distinct positive numbers, a known disposition and a note of 1 to 500 characters.
func (c SettlePaymentPlanReview) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !versionOK(c.OrderRevision):
		return schema("/args/order_revision")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case !Reason(c.Reason):
		return schema("/args/reason")
	case len(c.Items) > maxReviewItems:
		return schema("/args/items")
	}
	seen := map[int]bool{}
	for i, d := range c.Items {
		path := "/args/items/" + strconv.Itoa(i)
		switch {
		case d.Seq < 1 || d.Seq > maxReviewItems || seen[d.Seq]:
			return schema(path + "/seq")
		case d.Disposition != DispositionAccepted && d.Disposition != DispositionCorrected:
			return schema(path + "/disposition")
		case !note(d.Note):
			return schema(path + "/note")
		}
		seen[d.Seq] = true
	}
	return nil
}

func note(s string) bool {
	n := len([]rune(s))
	return n >= 1 && n <= maxNote && Reason(s)
}

// PaymentPlanReviewReceipt is the answer of both commands: the receipt (ObjectID the order, RowVersion
// the order account's version, Status the review's state), the revision the review is of and the one
// it moved the account from (0: it opened the account), and its number of items.
type PaymentPlanReviewReceipt struct {
	Receipt
	OrderRevision int64
	OrderHash     string
	FromRevision  int64
	Items         int
}

// ReviewItemView is one item of a review, amounts in minor units of the account's currency: for a
// stage its due at the earlier revision (From), now (To) and what stands on it (Applied); for the
// credit portion the portion before and now; the disposition once settled.
type ReviewItemView struct {
	Seq          int
	Kind         string
	StageID      string
	FromMinor    string
	ToMinor      string
	AppliedMinor string
	Disposition  string
	Note         string
}

// PaymentPlanReviewView is one review of an order account.
type PaymentPlanReviewView struct {
	OrderRevision   int64
	OrderHash       string
	FromRevision    int64
	FromHash        string
	ResponsibleRole string
	Status          string
	Version         int64
	Items           []ReviewItemView
	OpenedBy        kernel.ActorRef
	OpenedAt        time.Time
	SettledBy       kernel.ActorRef
	SettledAt       time.Time
	SettleReason    string
	SupersededBy    int64
	SupersededAt    time.Time
}

// PaymentPlanReviewsView is what payment_plan_review.list shows of an order account: the revision and
// version it holds and its reviews, latest revision first.
type PaymentPlanReviewsView struct {
	OrderID        string
	OrderRevision  int64
	AccountVersion int64
	Currency       string
	Reviews        []PaymentPlanReviewView
	CheckedAt      time.Time
}

// ReviewCommands is STL's command entry point for payment plan reviews; errors as for Commands.
type ReviewCommands interface {
	OpenPaymentPlanReview(ctx context.Context, cmd OpenPaymentPlanReview) (PaymentPlanReviewReceipt, error)
	SettlePaymentPlanReview(ctx context.Context, cmd SettlePaymentPlanReview) (PaymentPlanReviewReceipt, error)
}

// ReviewQueries is the audited read side of payment plan reviews (stl.read); an account outside the
// caller's data scope is NOT_FOUND.
type ReviewQueries interface {
	ListPaymentPlanReviews(ctx context.Context, orderID string) (PaymentPlanReviewsView, error)
}
