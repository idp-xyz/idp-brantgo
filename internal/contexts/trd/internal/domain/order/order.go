// Package order is the SalesOrder aggregate of TRD (tactical model TRD-AGG-SalesOrder; WBS S3-07,
// the order half of S3-13; SM-TRD-ORDER in draft, SM-TRD-ORDER-REVIEW; TRD-INV-010, 013, 014, 016,
// 017; ADR-BR-004, ADR-BR-013; Python t03_storage prepare_order and activate_order,
// t03_order_snapshot, t03_order_qualification and t04_order_review), identified by (tenant,
// company, order_id), one per quote root and conversion slot full_v1:
//
//	(none) → draft                 order.prepare: the quote's active, unheld formal revision with a
//	                               valid acceptance that formed the contract; the conversion slot is
//	                               taken in the same transaction (quote.TakeSlot) and kept for good
//	draft → active                 order.activate: the contract still formed (the acceptance valid),
//	                               the revision still active and unheld, an approved, unrevoked,
//	                               unexpired order review of the order's snapshot by a reviewer still
//	                               independent and qualified, and every upstream risk found now
//	                               acknowledged by that approval; the commit evidence is written with it
//	draft → cancelled              order.cancel, a human with a reason; the slot stays taken
//	draft → closed_unfulfilled     order.close_unfulfilled, a human with a reason (the acceptance was
//	                               invalidated, the customer withdrew); the slot stays taken
//
// and its reviews (SM-TRD-ORDER-REVIEW): open (order.review.request, human or AI) → approved or
// rejected (order.review.decide, a human independent of the preparer and the requester; an approval
// has the validity the reviewer sets, unrelated to the quote's, and acknowledges each upstream risk
// it found one by one) → revoked (order.approval.revoke: new activations stop, an active order is
// not rewritten). Active orders' amendments, cancellation and closes are WBS S3-15.
//
// The order's commercial snapshot (Snapshot) copies the sealed commercial document of the revision
// whole — data, server calculation, party snapshots and dependency manifest — with the order's
// identity and the acceptance it binds; its SHA-256 is the order hash. Nothing is recalculated and
// nothing is filled in (Python T03 A25), and neither ever changes.
//
// Contract formation decides, never the clock (TRD-INV-017): prepare, decide and activate check that
// the acceptance is valid and formed the contract (it arrived before the quote expired); none of
// them compares its own instant with the quote's expires_at. Upstream changes after the contract
// formed — a cost source denied or an input withdrawn (CST), a configuration retired or expired
// (POL), an exchange rate past its age, a specification, RFQ or party moved on (CAT, INQ, PTY) — do
// not refuse the order: each is a Risk the order approval acknowledges one by one, and activation
// refuses an unacknowledged one (ORDER_RISK_UNACKNOWLEDGED; ADR-BR-004, TRD-INV-014).
package order

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/acceptance"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the error catalog rows this package decides; package api declares the same with their
// categories.
const (
	CodeOrderNotDraft            = "ORDER_NOT_DRAFT"
	CodeOrderApprovalRequired    = "ORDER_APPROVAL_REQUIRED"
	CodeOrderApprovalRevoked     = "ORDER_APPROVAL_REVOKED"
	CodeOrderApprovalExpired     = "ORDER_APPROVAL_EXPIRED"
	CodeOrderReviewExpiryInvalid = "ORDER_REVIEW_EXPIRY_INVALID"
	CodeOrderReviewMismatch      = "ORDER_REVIEW_MISMATCH"
	CodeOrderReviewStale         = "ORDER_REVIEW_STALE"
	CodeOrderReviewImmutable     = "ORDER_REVIEW_IMMUTABLE"
	CodeReviewerNotIndependent   = "REVIEWER_NOT_INDEPENDENT"
	CodeOrderRiskUnacknowledged  = "ORDER_RISK_UNACKNOWLEDGED"
)

// Categories maps the codes above to their error catalog category.
var Categories = map[string]kernel.Category{
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
}

func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	if c, ok := quote.Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	if c, ok := acceptance.Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

// Permissions an order reviewer holds when the order is activated on their approval: the decision
// itself and TRD's read (the reviewer reads what they approve, as a quote reviewer must).
const (
	PermissionDecide = "order.review.decide"
	PermissionRead   = quote.PermissionRead
)

// Status of an order (SM-TRD-ORDER).
type Status string

const (
	Draft             Status = "draft"
	Active            Status = "active"
	Cancelled         Status = "cancelled"
	ClosedUnfulfilled Status = "closed_unfulfilled"
	Closed            Status = "closed"
)

// ReviewStatus is the state of an order review (SM-TRD-ORDER-REVIEW).
type ReviewStatus string

const (
	ReviewOpen     ReviewStatus = "open"
	ReviewApproved ReviewStatus = "approved"
	ReviewRejected ReviewStatus = "rejected"
	ReviewRevoked  ReviewStatus = "revoked"
)

// Source is what an order binds of the quote revision it converts and of the acceptance that formed
// the contract: the revision, its business hash and formal document (number and SHA-256); the
// acceptance at its effectivity version, when it arrived, on which basis, and the expires_at it was
// judged against.
type Source struct {
	QuoteID           string
	Revision          int64
	BusinessHash      string
	Number            string
	DocumentSHA256    string
	AcceptanceID      string
	AcceptanceVersion int64
	ArrivalAt         time.Time
	ArrivalBasis      string
	ExpiresAt         time.Time
}

// Risk is an upstream change found when the order is reviewed or activated (ADR-BR-004): where in
// the source revision's commercial document the changed fact is bound (Path, such as
// /data/cost_snapshot_ref), the owner's verdict (Code, such as COST_SNAPSHOT_INELIGIBLE,
// CONFIG_NOT_ACTIVE, CONFIG_EXPIRED, REFERENCE_STALE or FX_STALE) and the owner's own pointer
// (Detail, such as CST's /sources/<id>; empty when the owner gives none). An approval acknowledges
// a risk by all three.
type Risk struct {
	Path   string `json:"path"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Review is an order review: the order revision and hash it reviews, its state, who requested it,
// the decision (an approval's validity and acknowledged risks) and the revocation.
type Review struct {
	ID            string
	OrderRevision int64
	OrderHash     string
	Status        ReviewStatus
	Requested     quote.Act
	Decided       *quote.Act
	ExpiresAt     time.Time // approved and revoked
	Acknowledged  []Risk    // approved and revoked
	Revoked       *quote.Act
	Fresh         bool
	Changed       bool
}

// Commit is the commit evidence of an activation (TRD-INV-016; SC-07: 提交证据记录承诺到达时间、风险确认与审批):
// the command's idempotency key, the order revision and hash, the review it used, the formalization
// approval of the source revision, the cost snapshot CST found (adoptable or not), the epochs and
// row version it committed at, who and when, and every entry in a fixed order.
type Commit struct {
	IdempotencyKey string
	OrderRevision  int64
	OrderHash      string
	ReviewID       string
	ApprovalID     string
	CostSnapshotID string
	AuthorityEpoch int64
	ControlEpoch   int64
	RowVersion     int64
	By             kernel.ActorRef
	At             time.Time
	Entries        []quote.Entry
	Fresh          bool
}

// EntriesJSON returns the entries as stored: canonical JSON, the bytes their SHA-256 covers.
func (c Commit) EntriesJSON() ([]byte, error) {
	return quote.Commit{Entries: c.Entries}.EntriesJSON()
}

// Closure is how a draft order ended: cancelled or closed unfulfilled, by whom, why, on which
// evidence, at which row version.
type Closure struct {
	Status     Status
	Closed     quote.Act
	RowVersion int64
	Fresh      bool
}

// Order is the SalesOrder aggregate.
type Order struct {
	ID              string
	Source          Source
	Slot            string
	OrderRevision   int64
	Snapshot        []byte // canonical JSON (snapshot.go)
	OrderHash       string // SHA-256 of Snapshot
	Status          Status
	RowVersion      int64
	Prepared        quote.Act
	ActivatedReview string
	Reviews         []*Review // in the order they were requested
	Commits         []*Commit // one per activated order revision
	Closure         *Closure
	Fresh           bool
	Changed         bool
}

// Preparation is what order.prepare names: the order to create, the formal revision and business
// hash it converts and the acceptance that formed the contract on it.
type Preparation struct {
	OrderID      string
	QuoteID      string
	Revision     int64
	BusinessHash string
	AcceptanceID string
}

// Prepare decides order.prepare on quote root q (locked FOR UPDATE by the caller, its visibility and
// object control passed) with acceptance a (nil: unknown), by by at now (Python prepare_order;
// TRD-INV-010, 017): the revision is the root's active, unheld formal revision at the stated hash
// (quote.Usable: REFERENCE_INVALID, BUSINESS_HASH_MISMATCH, QUOTE_NOT_CURRENT, APPROVAL_REVOKED);
// the acceptance accepts exactly it (ACCEPTANCE_SCOPE_MISMATCH, also for an unknown one, as Python
// T03 A20), is valid (ACCEPTANCE_NOT_VALID) and formed the contract (ACCEPTANCE_LATE); the root's
// conversion slot is free (CONVERSION_SLOT_CONFLICT, BC-AT15), and is taken for the order. The quote's
// expiry is not judged. The order is a draft at row version 1 with its snapshot (snapshot.go).
func Prepare(p Preparation, q *quote.Quote, a *acceptance.Acceptance, scope kernel.Scope, by kernel.ActorRef, epoch int64,
	now time.Time) (*Order, error) {
	if err := q.Usable(p.Revision, p.BusinessHash, "/args/business_revision"); err != nil {
		return nil, err
	}
	if a == nil {
		return nil, fail(acceptance.CodeAcceptanceScopeMismatch, "/args/acceptance_id", "No such acceptance of the revision")
	}
	if err := a.Check(p.QuoteID, p.Revision, p.BusinessHash, "/args/acceptance_id"); err != nil {
		return nil, err
	}
	rev := q.Revision(p.Revision)
	src := Source{QuoteID: q.ID, Revision: rev.Number, BusinessHash: rev.Seal.BusinessHash, Number: rev.Formal.Document.Number,
		DocumentSHA256: rev.Formal.Document.ContentSHA256, AcceptanceID: a.ID, AcceptanceVersion: a.Version, ArrivalAt: a.ArrivalAt,
		ArrivalBasis: a.ArrivalBasis, ExpiresAt: a.ExpiresAt}
	snapshot, err := BuildSnapshot(scope, p.OrderID, src, rev.Seal.Commercial, a)
	if err != nil {
		return nil, err
	}
	if _, err = q.TakeSlot(p.Revision, p.BusinessHash, p.OrderID, by, epoch, now); err != nil {
		return nil, err
	}
	at := kernel.Instant(now)
	return &Order{ID: p.OrderID, Source: src, Slot: quote.SlotFullV1, OrderRevision: 1, Snapshot: snapshot, OrderHash: quote.Hash(snapshot),
		Status: Draft, RowVersion: 1, Prepared: quote.Act{By: by, Epoch: epoch, At: at}, Fresh: true}, nil
}

// Check admits a command on order o at the expected row version (VERSION_CONFLICT).
func (o *Order) Check(expected int64) error {
	if o.RowVersion != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Review returns review id, or nil.
func (o *Order) Review(id string) *Review {
	for _, r := range o.Reviews {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// Open returns the open review, or nil.
func (o *Order) Open() *Review {
	for _, r := range o.Reviews {
		if r.Status == ReviewOpen {
			return r
		}
	}
	return nil
}

// draft refuses a command that needs a draft order (ORDER_NOT_DRAFT).
func (o *Order) draft() error {
	if o.Status != Draft {
		return fail(CodeOrderNotDraft, "/args/order_id", "The order is "+string(o.Status))
	}
	return nil
}

// RequestReview opens review id of the draft order at its revision and hash (order.review.request;
// Python t04_order_review.request), by by at now: ORDER_NOT_DRAFT for another state,
// ORDER_REVIEW_IMMUTABLE for a review ID the order has, INVALID_STATE while another review is open.
// The row version moves.
func (o *Order) RequestReview(id string, by kernel.ActorRef, epoch int64, now time.Time) (*Review, error) {
	if err := o.draft(); err != nil {
		return nil, err
	}
	if o.Review(id) != nil {
		return nil, fail(CodeOrderReviewImmutable, "/args/review_id", "The order has review "+id)
	}
	if open := o.Open(); open != nil {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/review_id", "Review "+open.ID+" of the order is open")
	}
	r := &Review{ID: id, OrderRevision: o.OrderRevision, OrderHash: o.OrderHash, Status: ReviewOpen,
		Requested: quote.Act{By: by, Epoch: epoch, At: kernel.Instant(now)}, Fresh: true}
	o.Reviews = append(o.Reviews, r)
	o.RowVersion++
	o.Changed = true
	return r, nil
}

// Decision is what order.review.decide states: the review, the order hash the reviewer reviewed,
// approve or reject, an approval's validity and the risks it acknowledges, a reason (required to
// reject) and evidence.
type Decision struct {
	ReviewID     string
	OrderHash    string
	Approve      bool
	ExpiresAt    time.Time
	Acknowledged []Risk
	Reason       string
	Evidence     []quote.Evidence
}

// Decide decides open review d.ReviewID by by at now (order.review.decide; Python
// t04_order_review.decide; TRD-INV-013, SM-TRD-ORDER-REVIEW). It refuses, in this order: an unknown
// review (NOT_FOUND); a decider who prepared the order or requested the review
// (REVIEWER_NOT_INDEPENDENT); a review decided before (ORDER_REVIEW_IMMUTABLE); an order no longer a
// draft (ORDER_NOT_DRAFT) or at another revision or hash than the review (ORDER_REVIEW_STALE); a
// stated order hash that is not the order's (ORDER_REVIEW_MISMATCH). An approval is further refused
// when valid until no later than now (ORDER_REVIEW_EXPIRY_INVALID: its validity is the order
// approval's own and is not bounded by the quote's, which may have expired — ADR-BR-013); then
// upstream, which the caller gives, checks that the contract still stands (TRD-INV-017) and returns
// the upstream risks found now; an acknowledgement of a risk not found is REFERENCE_INVALID, and a
// risk found that the approval does not acknowledge ORDER_RISK_UNACKNOWLEDGED
// (逐项确认订单准备后出现的上游风险). The caller checked the decider's current qualification. The row
// version moves.
func (o *Order) Decide(d Decision, by kernel.ActorRef, epoch int64, upstream func() ([]Risk, error), now time.Time) (*Review, error) {
	r := o.Review(d.ReviewID)
	switch {
	case r == nil:
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/review_id", "")
	case by.ID == r.Requested.By.ID || by.ID == o.Prepared.By.ID:
		return nil, fail(CodeReviewerNotIndependent, "/args/review_id", "The reviewer prepared the order or requested the review")
	case r.Status != ReviewOpen:
		return nil, fail(CodeOrderReviewImmutable, "/args/review_id", "The review is "+string(r.Status))
	}
	if err := o.draft(); err != nil {
		return nil, err
	}
	switch {
	case r.OrderRevision != o.OrderRevision || r.OrderHash != o.OrderHash:
		return nil, fail(CodeOrderReviewStale, "/args/review_id", "The review is of another order revision")
	case d.OrderHash != o.OrderHash:
		return nil, fail(CodeOrderReviewMismatch, "/args/order_hash", "The order has another hash")
	}
	at := kernel.Instant(now)
	act := quote.Act{By: by, Epoch: epoch, At: at, Reason: d.Reason, Evidence: slices.Clone(d.Evidence)}
	if !d.Approve {
		r.Status, r.Decided, r.Changed = ReviewRejected, &act, true
		o.RowVersion++
		o.Changed = true
		return r, nil
	}
	expires := kernel.Instant(d.ExpiresAt)
	if !expires.After(at) {
		return nil, fail(CodeOrderReviewExpiryInvalid, "/args/expires_at", "An approval is valid after its decision")
	}
	risks, err := upstream()
	if err != nil {
		return nil, err
	}
	for i, k := range d.Acknowledged {
		if !slices.Contains(risks, k) {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/risk_acknowledgements/"+itoa(int64(i)),
				"No such risk of the order now")
		}
	}
	if err := covered(risks, d.Acknowledged, "/args/risk_acknowledgements"); err != nil {
		return nil, err
	}
	r.Status, r.Decided, r.ExpiresAt, r.Changed = ReviewApproved, &act, expires, true
	r.Acknowledged = slices.Clone(d.Acknowledged)
	if r.Acknowledged == nil {
		r.Acknowledged = []Risk{}
	}
	o.RowVersion++
	o.Changed = true
	return r, nil
}

// covered refuses the first risk of risks that acknowledged does not name (ORDER_RISK_UNACKNOWLEDGED
// at path, the risk in the reason).
func covered(risks, acknowledged []Risk, path string) error {
	if missing := Unacknowledged(risks, acknowledged); len(missing) > 0 {
		k := missing[0]
		return fail(CodeOrderRiskUnacknowledged, path, "Upstream risk "+k.Code+" at "+k.Path+describe(k.Detail)+
			" is not acknowledged by the order approval")
	}
	return nil
}

// Unacknowledged returns the risks of risks that acknowledged does not name, in their order.
func Unacknowledged(risks, acknowledged []Risk) []Risk {
	var out []Risk
	for _, k := range risks {
		if !slices.Contains(acknowledged, k) {
			out = append(out, k)
		}
	}
	return out
}

// CheckDraft refuses a command that needs a draft order (ORDER_NOT_DRAFT).
func (o *Order) CheckDraft() error { return o.draft() }

func describe(detail string) string {
	if detail == "" {
		return ""
	}
	return " (" + detail + ")"
}

// Revoke revokes the approval of review id by act (order.approval.revoke; Python
// t04_order_review.revoke; SM-TRD-ORDER-REVIEW approved → revoked): NOT_FOUND for an unknown review,
// ORDER_REVIEW_IMMUTABLE for a revoked one, ORDER_APPROVAL_REQUIRED for one not approved,
// INVALID_STATE on an order that ended. New activations on it stop; an order it activated stays
// active (阻止新激活，不倒写已激活订单). The row version moves.
func (o *Order) Revoke(id string, act quote.Act) (*Review, error) {
	r := o.Review(id)
	switch {
	case r == nil:
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/review_id", "")
	case r.Status == ReviewRevoked:
		return nil, fail(CodeOrderReviewImmutable, "/args/review_id", "The approval is revoked")
	case r.Status != ReviewApproved:
		return nil, fail(CodeOrderApprovalRequired, "/args/review_id", "The review is "+string(r.Status)+", not approved")
	case o.Status != Draft && o.Status != Active:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/order_id", "The order is "+string(o.Status))
	}
	act.At, act.Evidence = kernel.Instant(act.At), slices.Clone(act.Evidence)
	r.Status, r.Revoked, r.Changed = ReviewRevoked, &act, true
	o.RowVersion++
	o.Changed = true
	return r, nil
}

// Admit decides, inside activation's transaction, whether review id approves activating o now on
// the reviewer's standing (Python t04_order_review.qualify; TRD-INV-013), in this order: o is a
// draft (ORDER_NOT_DRAFT); the review exists (NOT_FOUND); it reviews the order's current revision and
// hash (ORDER_REVIEW_STALE); it is approved (ORDER_APPROVAL_REQUIRED for open and rejected) and not
// revoked (ORDER_APPROVAL_REVOKED); the reviewer is independent of the preparer and the requester
// (REVIEWER_NOT_INDEPENDENT) and still an active human grant holding order.review.decide and
// trd.read that sees the quote (ORDER_APPROVAL_REQUIRED: the approval no longer counts as an
// independent, qualified one); and now is before the approval's expires_at (ORDER_APPROVAL_EXPIRED,
// equality refused). It returns the review.
func (o *Order) Admit(id string, standings quote.Standings, now time.Time) (*Review, error) {
	if err := o.draft(); err != nil {
		return nil, err
	}
	r := o.Review(id)
	switch {
	case r == nil:
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/review_id", "")
	case r.OrderRevision != o.OrderRevision || r.OrderHash != o.OrderHash:
		return nil, fail(CodeOrderReviewStale, "/args/review_id", "The review is of another order revision")
	case r.Status == ReviewRevoked:
		return nil, fail(CodeOrderApprovalRevoked, "/args/review_id", "")
	case r.Status != ReviewApproved || r.Decided == nil:
		return nil, fail(CodeOrderApprovalRequired, "/args/review_id", "The review is "+string(r.Status))
	case r.Decided.By.ID == r.Requested.By.ID || r.Decided.By.ID == o.Prepared.By.ID:
		return nil, fail(CodeReviewerNotIndependent, "/args/review_id", "")
	}
	s, err := standings(r.Decided.By.ID)
	if err != nil {
		return nil, err
	}
	if r.Decided.By.Kind != kernel.ActorHuman || !s.Qualified(PermissionDecide, PermissionRead) {
		return nil, fail(CodeOrderApprovalRequired, "/args/review_id", "The order reviewer is no longer qualified")
	}
	if !kernel.Instant(now).Before(r.ExpiresAt) {
		return nil, fail(CodeOrderApprovalExpired, "/args/review_id", "")
	}
	return r, nil
}

// Activation is what order.activate brings once the source checks and the order gate (Admit)
// passed in its transaction: the review admitted, the command's idempotency key, the upstream risks
// found now, the formalization approval of the source revision, the cost snapshot CST answered for,
// what the upstream guards answered (Upstream entries, the extension points not wired among them),
// and the quote root's control epoch.
type Activation struct {
	Review         *Review
	IdempotencyKey string
	Risks          []Risk
	ApprovalID     string
	CostSnapshotID string
	Upstream       []quote.Entry
	ControlEpoch   int64
}

// Activate makes draft order o active on a.Review by by under authority epoch epoch at now
// (SM-TRD-ORDER draft → active): every risk of a.Risks must be acknowledged by the approval
// (ORDER_RISK_UNACKNOWLEDGED at /args/review_id; SC-07, BC-AT13), and the commit evidence records
// the acceptance's arrival, the quote's expires_at as bound (never compared with now), the review,
// its reviewer and validity, the risks found and acknowledged, and the upstream answers. The row
// version moves once.
func (o *Order) Activate(a Activation, by kernel.ActorRef, epoch int64, now time.Time) (*Commit, error) {
	if err := o.draft(); err != nil {
		return nil, err
	}
	r := a.Review
	if r == nil || o.Review(r.ID) != r || r.Status != ReviewApproved {
		return nil, fail(CodeOrderApprovalRequired, "/args/review_id", "")
	}
	if err := covered(a.Risks, r.Acknowledged, "/args/review_id"); err != nil {
		return nil, err
	}
	at := kernel.Instant(now)
	o.RowVersion++
	src := o.Source
	risks, err := quote.TreeOf(nonNil(a.Risks))
	if err != nil {
		return nil, err
	}
	found, err := quote.Canonical(risks)
	if err != nil {
		return nil, err
	}
	c := &Commit{IdempotencyKey: a.IdempotencyKey, OrderRevision: o.OrderRevision, OrderHash: o.OrderHash, ReviewID: r.ID,
		ApprovalID: a.ApprovalID, CostSnapshotID: a.CostSnapshotID, AuthorityEpoch: epoch, ControlEpoch: a.ControlEpoch,
		RowVersion: o.RowVersion, By: by, At: at, Fresh: true}
	c.Entries = []quote.Entry{{Name: "command", Value: "order.activate"}, {Name: "idempotency_key", Value: a.IdempotencyKey},
		{Name: "order_id", Value: o.ID}, {Name: "order_revision", Value: itoa(o.OrderRevision)}, {Name: "order_hash", Value: o.OrderHash},
		{Name: "quote_id", Value: src.QuoteID}, {Name: "business_revision", Value: itoa(src.Revision)},
		{Name: "business_hash", Value: src.BusinessHash}, {Name: "number", Value: src.Number},
		{Name: "document_sha256", Value: src.DocumentSHA256}, {Name: "formalization_approval_id", Value: a.ApprovalID},
		{Name: "acceptance_id", Value: src.AcceptanceID}, {Name: "acceptance_effectivity_version", Value: itoa(src.AcceptanceVersion)},
		{Name: "arrival_at", Value: kernel.FormatTimestamp(src.ArrivalAt)}, {Name: "arrival_basis", Value: src.ArrivalBasis},
		{Name: "quote_expires_at", Value: kernel.FormatTimestamp(src.ExpiresAt)}, {Name: "contract_formed", Value: "true"},
		{Name: "review_id", Value: r.ID}, {Name: "reviewer", Value: string(r.Decided.By.Kind) + ":" + r.Decided.By.ID},
		{Name: "review_decided_at", Value: kernel.FormatTimestamp(r.Decided.At)},
		{Name: "review_expires_at", Value: kernel.FormatTimestamp(r.ExpiresAt)}, {Name: "risks_found", Value: string(found)},
		{Name: "risks_acknowledged", Value: itoa(int64(len(a.Risks)))}}
	c.Entries = append(c.Entries, a.Upstream...)
	c.Entries = append(c.Entries, quote.Entry{Name: "authority_epoch", Value: itoa(epoch)},
		quote.Entry{Name: "control_epoch", Value: itoa(a.ControlEpoch)}, quote.Entry{Name: "row_version", Value: itoa(o.RowVersion)},
		quote.Entry{Name: "committed_by", Value: string(by.Kind) + ":" + by.ID}, quote.Entry{Name: "committed_at", Value: kernel.FormatTimestamp(at)})
	o.Status, o.ActivatedReview, o.Changed = Active, r.ID, true
	o.Commits = append(o.Commits, c)
	return c, nil
}

func nonNil(risks []Risk) []Risk {
	if risks == nil {
		return []Risk{}
	}
	return risks
}

// Cancel cancels the draft order by act (order.cancel in draft; SM-TRD-ORDER: 授权负责人与理由；名额保留).
// The conversion slot stays taken; the row version moves.
func (o *Order) Cancel(act quote.Act) (*Closure, error) {
	return o.close(Cancelled, act)
}

// CloseUnfulfilled closes the draft order unfulfilled by act (order.close_unfulfilled in draft:
// the acceptance was invalidated, the customer withdrew). The conversion slot stays taken; the row
// version moves.
func (o *Order) CloseUnfulfilled(act quote.Act) (*Closure, error) {
	return o.close(ClosedUnfulfilled, act)
}

func (o *Order) close(to Status, act quote.Act) (*Closure, error) {
	if err := o.draft(); err != nil {
		return nil, err
	}
	act.At, act.Evidence = kernel.Instant(act.At), slices.Clone(act.Evidence)
	o.RowVersion++
	o.Status, o.Changed = to, true
	o.Closure = &Closure{Status: to, Closed: act, RowVersion: o.RowVersion, Fresh: true}
	return o.Closure, nil
}

// ActivatedAt returns the commit of the activation that committed at row version v, or nil
// (receipts of replayed activations).
func (o *Order) ActivatedAt(v int64) *Commit {
	for _, c := range o.Commits {
		if c.RowVersion == v {
			return c
		}
	}
	return nil
}
