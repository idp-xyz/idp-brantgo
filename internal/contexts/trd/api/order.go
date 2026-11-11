package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Sales orders in draft and their activation (WBS S3-07, the order half of S3-13; tactical model
// TRD-AGG-SalesOrder, SM-TRD-ORDER, SM-TRD-ORDER-REVIEW; TRD-INV-010, 013, 014, 016, 017, 021;
// ADR-BR-004, ADR-BR-013; Python t03_storage prepare_order and activate_order, t03_order_snapshot,
// t03_order_qualification and t04_order_review).
//
//	order.prepare            human or AI: converts the quote's active, unheld formal revision, at its
//	                         business hash, on a valid acceptance that formed the contract, into
//	                         order order_id, a draft. It takes the root's conversion slot full_v1 in the
//	                         same transaction (TRD-INV-010: once per root, kept for good; a second
//	                         preparation is CONVERSION_SLOT_CONFLICT, BC-AT15). The order's commercial
//	                         snapshot copies the revision's sealed commercial document whole, with the
//	                         acceptance it binds; its SHA-256 is the order hash. Nothing is
//	                         recalculated.
//	order.review.request     human or AI: opens review review_id of the draft order at its revision
//	                         and hash; one open review per order.
//	order.review.decide      human, independent of the preparer and the requester
//	                         (REVIEWER_NOT_INDEPENDENT), holding trd.read now: approve, with the
//	                         approval's expires_at (after the decision; the order approval's own
//	                         validity, unrelated to the quote's) and an acknowledgement of each
//	                         upstream risk found now (risk_acknowledgements; ORDER_RISK_UNACKNOWLEDGED
//	                         when one is missing), or reject with a reason.
//	order.approval.revoke    human: approved → revoked, with a reason. New activations on it stop; an
//	                         order it activated stays active, and ActiveOrderRef v1 reports the
//	                         revocation to PUR.
//	order.activate           human: draft → active on approved review review_id (see below).
//	order.cancel             human, draft → cancelled, with a reason and evidence; the slot stays taken.
//	order.close_unfulfilled  human, draft → closed_unfulfilled (the acceptance was invalidated, the
//	                         customer withdrew), with a reason and evidence; the slot stays taken.
//
// Every order command locks the quote root FOR UPDATE first (the conflict key of quotes,
// acceptances and orders, ADR-BR-002; tactical lockOrder) and passes its object control
// (TRD-INV-015), then locks the order FOR UPDATE; each moves the order's row version
// (expected_version, VERSION_CONFLICT), except order.prepare (new) and order.approval.revoke, which
// names the review.
//
// Contract formation decides, never the clock (TRD-INV-017, ADR-BR-013): prepare, decide and activate
// check that the acceptance is valid and arrived before the quote expired (ACCEPTANCE_NOT_VALID,
// ACCEPTANCE_LATE); none compares its own instant with the quote's expires_at, so an order is
// prepared, approved and activated after the quote expired when the contract formed in time (BC-AT13,
// SC-07).
//
// order.activate decides, in one transaction (TRD-INV-014, 016): the order is a draft at
// expected_version and its snapshot is intact (REFERENCE_INVALID); the acceptance is still valid and
// formed; the source revision is still the quote's active, unheld revision at its hash, its slot
// taken by this order, its formal document's bytes intact (FORMAL_DOCUMENT_INTEGRITY_FAILURE), its
// formalization approval not revoked and its reviewer still independent and qualified
// (APPROVAL_REVOKED, INDEPENDENT_REVIEW_REQUIRED, APPROVAL_STALE); the order review is approved,
// unrevoked, unexpired, of the order's revision and hash, by a reviewer still independent and
// qualified (ORDER_APPROVAL_REQUIRED, ORDER_APPROVAL_REVOKED, ORDER_APPROVAL_EXPIRED,
// ORDER_REVIEW_STALE, REVIEWER_NOT_INDEPENDENT); the parties the revision binds are screened clear
// (PTY PartyScreeningGuard v1: PARTY_SCREENING_NOT_CLEAR, TRD-INV-021, TD-AT09); and every upstream
// change since the contract formed — what CAT, CST, INQ, POL and PTY now say of what the revision
// binds, and the sales exchange rate's age — is a Risk the approval acknowledged
// (ORDER_RISK_UNACKNOWLEDGED; ADR-BR-004: a change of cost, configuration or exchange rate is
// acknowledged, not refused). STL's CreditGuard v1 (R57, TRD-INV-022) and CNX's
// TradeComplianceGuard v1 (R58, TRD-INV-023) are not wired (the owner's decision of 2026-11-10): the
// commit evidence and OrderActivated name them as unchecked, never as passed.
//
// Request members in snake_case: order.prepare {order_id, quote_id, business_revision,
// business_hash, acceptance_id}; order.review.request {order_id, expected_version, review_id};
// order.review.decide {order_id, expected_version, review_id, order_hash, decision ("approve" |
// "reject"), expires_at, risk_acknowledgements [{path, code, detail}], reason, evidence};
// order.activate {order_id, expected_version, review_id}; order.approval.revoke {order_id,
// review_id, reason, evidence}; order.cancel and order.close_unfulfilled {order_id,
// expected_version, reason, evidence}.
const (
	CommandPrepareOrder          = "order.prepare"
	CommandRequestOrderReview    = "order.review.request"
	CommandDecideOrderReview     = "order.review.decide"
	CommandActivateOrder         = "order.activate"
	CommandRevokeOrderApproval   = "order.approval.revoke"
	CommandCancelOrder           = "order.cancel"
	CommandCloseOrderUnfulfilled = "order.close_unfulfilled"

	EventOrderPrepared = "idp-brant.trd.order_prepared.v1"
	// EventOrderReviewRequested is the tactical model's OrderReviewRequested; the event catalog V0.1
	// does not list it yet.
	EventOrderReviewRequested   = "idp-brant.trd.order_review_requested.v1"
	EventOrderReviewDecided     = "idp-brant.trd.order_review_decided.v1"
	EventOrderActivated         = "idp-brant.trd.order_activated.v1"
	EventOrderApprovalRevoked   = "idp-brant.trd.order_approval_revoked.v1"
	EventOrderCancelled         = "idp-brant.trd.order_cancelled.v1"
	EventOrderClosedUnfulfilled = "idp-brant.trd.order_closed_unfulfilled.v1"

	// Order states (SM-TRD-ORDER).
	OrderDraft             = "draft"
	OrderActive            = "active"
	OrderCancelled         = "cancelled"
	OrderClosedUnfulfilled = "closed_unfulfilled"
	OrderClosed            = "closed"

	// Order review states (SM-TRD-ORDER-REVIEW).
	OrderReviewOpen     = "open"
	OrderReviewApproved = "approved"
	OrderReviewRejected = "rejected"
	OrderReviewRevoked  = "revoked"

	// OrderSnapshotContract names an order's commercial snapshot.
	OrderSnapshotContract = "brant.order-commercial-snapshot/0.1.0"

	// The extension points order.activate names as unchecked while their contexts are not wired.
	GuardSTLCredit          = "stl.credit_guard.v1"
	GuardCNXTradeCompliance = "cnx.trade_compliance_guard.v1"

	maxRiskAcknowledgements = 200
	maxRiskText             = 500
)

// Codes of sales orders (error catalog V0.1, rows whose contexts include TRD), with their
// categories. ORDER_NOT_ACTIVE is the catalog's PUR row; TRD's OrderGuard answers it.
const (
	CodeOrderNotDraft            = "ORDER_NOT_DRAFT"             // the order is not a draft
	CodeOrderApprovalRequired    = "ORDER_APPROVAL_REQUIRED"     // no approved, independent and qualified order review
	CodeOrderApprovalRevoked     = "ORDER_APPROVAL_REVOKED"      // the order approval was revoked
	CodeOrderApprovalExpired     = "ORDER_APPROVAL_EXPIRED"      // the order approval lapsed
	CodeOrderReviewExpiryInvalid = "ORDER_REVIEW_EXPIRY_INVALID" // the approval would be valid until no later than its decision
	CodeOrderReviewMismatch      = "ORDER_REVIEW_MISMATCH"       // the decision states another order hash
	CodeOrderReviewStale         = "ORDER_REVIEW_STALE"          // 409: the review is of another order revision or hash
	CodeOrderReviewImmutable     = "ORDER_REVIEW_IMMUTABLE"      // the review ID is taken, or the review was decided or revoked
	CodeReviewerNotIndependent   = "REVIEWER_NOT_INDEPENDENT"    // the reviewer prepared the order or requested the review
	CodeOrderRiskUnacknowledged  = "ORDER_RISK_UNACKNOWLEDGED"   // 409: an upstream risk is not acknowledged by the order approval
	CodeOrderNotActive           = "ORDER_NOT_ACTIVE"            // OrderGuard: the order is not active
)

var orderCodes = map[string]kernel.Category{
	CodeOrderNotDraft:            kernel.CategoryInvalid,
	CodeOrderApprovalRequired:    kernel.CategoryInvalid,
	CodeOrderApprovalRevoked:     kernel.CategoryInvalid,
	CodeOrderApprovalExpired:     kernel.CategoryInvalid,
	CodeOrderReviewExpiryInvalid: kernel.CategoryInvalid,
	CodeOrderReviewMismatch:      kernel.CategoryInvalid,
	CodeOrderReviewStale:         kernel.CategoryConflict,
	CodeOrderReviewImmutable:     kernel.CategoryInvalid,
	CodeReviewerNotIndependent:   kernel.CategoryInvalid,
	CodeOrderRiskUnacknowledged:  kernel.CategoryConflict,
	CodeOrderNotActive:           kernel.CategoryInvalid,
}

// PrepareOrder converts formal revision BusinessRevision of quote QuoteID, at BusinessHash, accepted
// by acceptance AcceptanceID, into sales order OrderID (Python order.prepare, with the order ID
// named rather than derived and the slot always full_v1). Human or AI.
type PrepareOrder struct {
	IdempotencyKey   string
	OrderID          string
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	AcceptanceID     string
}

// RequestOrderReview opens review ReviewID of draft order OrderID (Python order.review.request).
// Human or AI.
type RequestOrderReview struct {
	IdempotencyKey  string
	OrderID         string
	ExpectedVersion int64
	ReviewID        string
}

// RiskAcknowledgement names one upstream risk an approval acknowledges, as the order's reads show
// it: the path in the source revision's commercial document, the owner's code and the owner's
// detail ("" when none).
type RiskAcknowledgement struct {
	Path   string
	Code   string
	Detail string
}

// DecideOrderReview decides review ReviewID of order OrderID (Python order.review.decide).
// OrderHash restates the order's hash. Decision approve needs ExpiresAt (a timestamp) and lists the
// risks it acknowledges (each upstream risk found now, none else); reject needs Reason and takes
// neither. Evidence is optional. Human only.
type DecideOrderReview struct {
	IdempotencyKey       string
	OrderID              string
	ExpectedVersion      int64
	ReviewID             string
	OrderHash            string
	Decision             string
	ExpiresAt            string
	RiskAcknowledgements []RiskAcknowledgement
	Reason               string
	Evidence             []Evidence
}

// ActivateOrder activates draft order OrderID on approved review ReviewID (Python order.activate,
// with the formalization approval read from the revision's commit evidence rather than restated).
// Human only.
type ActivateOrder struct {
	IdempotencyKey  string
	OrderID         string
	ExpectedVersion int64
	ReviewID        string
}

// RevokeOrderApproval revokes the approval of review ReviewID of order OrderID (Python
// order.approval.revoke), with a reason and optional evidence. Human only.
type RevokeOrderApproval struct {
	IdempotencyKey string
	OrderID        string
	ReviewID       string
	Reason         string
	Evidence       []Evidence
}

// CancelOrder cancels draft order OrderID with a reason and evidence. Human only.
type CancelOrder struct {
	IdempotencyKey  string
	OrderID         string
	ExpectedVersion int64
	Reason          string
	Evidence        []Evidence
}

// CloseOrderUnfulfilled closes draft order OrderID unfulfilled with a reason and evidence. Human
// only.
type CloseOrderUnfulfilled struct {
	IdempotencyKey  string
	OrderID         string
	ExpectedVersion int64
	Reason          string
	Evidence        []Evidence
}

// Validate checks the closed contract.
func (c PrepareOrder) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !kernel.ValidIdentifier(c.QuoteID):
		return schema("/args/quote_id")
	}
	if err := positive(c.BusinessRevision, "/args/business_revision"); err != nil {
		return err
	}
	switch {
	case !hash64.MatchString(c.BusinessHash):
		return schema("/args/business_hash")
	case !kernel.ValidIdentifier(c.AcceptanceID):
		return schema("/args/acceptance_id")
	}
	return nil
}

func orderHead(key, id string, expected int64) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(id):
		return schema("/args/order_id")
	}
	return positive(expected, "/args/expected_version")
}

// Validate checks the closed contract.
func (c RequestOrderReview) Validate() error {
	if err := orderHead(c.IdempotencyKey, c.OrderID, c.ExpectedVersion); err != nil {
		return err
	}
	if !kernel.ValidIdentifier(c.ReviewID) {
		return schema("/args/review_id")
	}
	return nil
}

// Validate checks the closed contract: approve with a UTC expires_at and at most 200 distinct risk
// acknowledgements; reject with a reason and neither.
func (c DecideOrderReview) Validate() error {
	if err := orderHead(c.IdempotencyKey, c.OrderID, c.ExpectedVersion); err != nil {
		return err
	}
	switch {
	case !kernel.ValidIdentifier(c.ReviewID):
		return schema("/args/review_id")
	case !hash64.MatchString(c.OrderHash):
		return schema("/args/order_hash")
	}
	switch c.Decision {
	case DecisionApprove:
		if _, err := kernel.ParseTimestamp(c.ExpiresAt); err != nil {
			return schema("/args/expires_at")
		}
		if c.Reason != "" && !text(c.Reason, maxText) {
			return schema("/args/reason")
		}
	case DecisionReject:
		switch {
		case c.ExpiresAt != "":
			return schema("/args/expires_at")
		case len(c.RiskAcknowledgements) > 0:
			return schema("/args/risk_acknowledgements")
		case !text(c.Reason, maxText):
			return schema("/args/reason")
		}
	default:
		return schema("/args/decision")
	}
	if len(c.RiskAcknowledgements) > maxRiskAcknowledgements {
		return schema("/args/risk_acknowledgements")
	}
	seen := map[RiskAcknowledgement]bool{}
	for i, r := range c.RiskAcknowledgements {
		p := index("/args/risk_acknowledgements", i)
		switch {
		case !text(r.Path, maxRiskText) || r.Path[0] != '/':
			return schema(p + "/path")
		case !kernel.ValidIdentifier(r.Code):
			return schema(p + "/code")
		case r.Detail != "" && !text(r.Detail, maxRiskText):
			return schema(p + "/detail")
		case seen[r]:
			return schema(p)
		}
		seen[r] = true
	}
	if len(c.Evidence) > 0 {
		return evidence(c.Evidence, "/args/evidence")
	}
	return nil
}

// Validate checks the closed contract.
func (c ActivateOrder) Validate() error {
	if err := orderHead(c.IdempotencyKey, c.OrderID, c.ExpectedVersion); err != nil {
		return err
	}
	if !kernel.ValidIdentifier(c.ReviewID) {
		return schema("/args/review_id")
	}
	return nil
}

// Validate checks the closed contract: a reason of 1–2000 characters and 0–200 evidence entries.
func (c RevokeOrderApproval) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !kernel.ValidIdentifier(c.ReviewID):
		return schema("/args/review_id")
	case !text(c.Reason, maxText):
		return schema("/args/reason")
	}
	if len(c.Evidence) > 0 {
		return evidence(c.Evidence, "/args/evidence")
	}
	return nil
}

// Validate checks the closed contract.
func (c CancelOrder) Validate() error {
	return orderEnd(c.IdempotencyKey, c.OrderID, c.ExpectedVersion, c.Reason, c.Evidence)
}

// Validate checks the closed contract.
func (c CloseOrderUnfulfilled) Validate() error {
	return orderEnd(c.IdempotencyKey, c.OrderID, c.ExpectedVersion, c.Reason, c.Evidence)
}

func orderEnd(key, id string, expected int64, reason string, proofs []Evidence) error {
	if err := orderHead(key, id, expected); err != nil {
		return err
	}
	if !text(reason, maxText) {
		return schema("/args/reason")
	}
	return evidence(proofs, "/args/evidence")
}

func (PrepareOrder) trdCommand()          {}
func (RequestOrderReview) trdCommand()    {}
func (DecideOrderReview) trdCommand()     {}
func (ActivateOrder) trdCommand()         {}
func (RevokeOrderApproval) trdCommand()   {}
func (CancelOrder) trdCommand()           {}
func (CloseOrderUnfulfilled) trdCommand() {}

// OrderReceipt is the answer of every order command: the receipt (ObjectID the order ID, RowVersion
// the order's row version, Status the order's state — or, for order.review.request,
// order.review.decide and order.approval.revoke, the review's) and what the order binds: the quote
// revision and business hash, the acceptance, the order revision and hash, the order's state, and
// the review the command acted on (review commands and activation) with its state. A replay
// rebuilds it from the stored order.
type OrderReceipt struct {
	Receipt
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	AcceptanceID     string
	OrderRevision    int64
	OrderHash        string
	OrderStatus      string
	ReviewID         string
	ReviewStatus     string
}

// Risk is an upstream change found when an order is checked, reviewed or activated (ADR-BR-004):
// where in the source revision's commercial document the changed fact is bound (Path), the owner's
// verdict (Code) and the owner's own pointer (Detail, such as CST's /sources/<id>; "" when none). An
// approval acknowledges a risk by all three (RiskAcknowledgement).
type Risk struct {
	Path   string
	Code   string
	Detail string
}

// OrderView is a sales order as the reads show it: what it binds, its snapshot (the exact bytes the
// order hash covers), its state and row version, who prepared it, its reviews, the commit evidence
// of its activation and its closure.
type OrderView struct {
	OrderID           string
	QuoteID           string
	BusinessRevision  int64
	BusinessHash      string
	Number            string
	DocumentSHA256    string
	AcceptanceID      string
	AcceptanceVersion int64
	ArrivalAt         time.Time
	ArrivalBasis      string
	QuoteExpiresAt    time.Time
	Slot              string
	OrderRevision     int64
	OrderHash         string
	Snapshot          string
	Status            string
	RowVersion        int64
	PreparedBy        kernel.ActorRef
	PreparedAt        time.Time
	ActivatedReviewID string
	Reviews           []OrderReviewView
	Commits           []OrderCommitView
	Closure           *OrderClosureView
}

// OrderReviewView is an order review. DecisionState is its state as of the read: pending (open),
// approved, expired (approved, at or after ExpiresAt), rejected or revoked (Python A42).
type OrderReviewView struct {
	ReviewID      string
	OrderRevision int64
	OrderHash     string
	Status        string
	DecisionState string
	RequestedBy   kernel.ActorRef
	RequestedAt   time.Time
	DecidedBy     kernel.ActorRef
	DecidedAt     time.Time
	ExpiresAt     time.Time
	Reason        string
	Evidence      []Evidence
	Acknowledged  []Risk
	RevokedBy     kernel.ActorRef
	RevokedAt     time.Time
	RevokeReason  string
	Activated     bool // the review activated the order
}

// OrderCommitView is the commit evidence of an activation: a summary and the SHA-256 of its entries,
// which are shown whole.
type OrderCommitView struct {
	IdempotencyKey string
	OrderRevision  int64
	OrderHash      string
	ReviewID       string
	ApprovalID     string
	CostSnapshotID string
	AuthorityEpoch int64
	ControlEpoch   int64
	RowVersion     int64
	CommittedBy    kernel.ActorRef
	CommittedAt    time.Time
	Entries        map[string]string
	EntriesSHA256  string
}

// OrderClosureView is how a draft order ended.
type OrderClosureView struct {
	Status     string
	ClosedBy   kernel.ActorRef
	ClosedAt   time.Time
	Reason     string
	Evidence   []Evidence
	RowVersion int64
}

// OrderCheck says what order.activate would decide now on review ReviewID ("" for no review),
// without changing anything: the upstream risks found now (what an approval acknowledges, one by
// one), the risks the review's approval does not acknowledge, and Code and Path, the first refusal
// activation would give ("" when it would activate). CheckedAt is the instant.
type OrderCheck struct {
	OrderID        string
	Status         string
	RowVersion     int64
	OrderHash      string
	ReviewID       string
	Risks          []Risk
	Unacknowledged []Risk
	Unchecked      []string // extension points not wired (GuardSTLCredit, GuardCNXTradeCompliance)
	Activatable    bool
	Code           string
	Path           string
	CheckedAt      time.Time
}

// OrderCommands is the command entry point of sales orders. Errors are as Commands describes them.
type OrderCommands interface {
	PrepareOrder(ctx context.Context, cmd PrepareOrder) (OrderReceipt, error)
	RequestOrderReview(ctx context.Context, cmd RequestOrderReview) (OrderReceipt, error)
	DecideOrderReview(ctx context.Context, cmd DecideOrderReview) (OrderReceipt, error)
	ActivateOrder(ctx context.Context, cmd ActivateOrder) (OrderReceipt, error)
	RevokeOrderApproval(ctx context.Context, cmd RevokeOrderApproval) (OrderReceipt, error)
	CancelOrder(ctx context.Context, cmd CancelOrder) (OrderReceipt, error)
	CloseOrderUnfulfilled(ctx context.Context, cmd CloseOrderUnfulfilled) (OrderReceipt, error)
}

// OrderQueries is the audited read side of sales orders.
type OrderQueries interface {
	// GetOrder returns an order of a quote the caller sees, audited as order.get (trd.read):
	// NOT_FOUND otherwise; an order whose snapshot is not intact is REFERENCE_INVALID.
	GetOrder(ctx context.Context, orderID string) (OrderView, error)
	// CheckOrder runs, in a read transaction, the checks order.activate makes in its own on review
	// reviewID ("": the order's source and upstream risks only), audited as order.check
	// (trd.read). A refusal of the check is its answer, not an error.
	CheckOrder(ctx context.Context, orderID, reviewID string) (OrderCheck, error)
}
