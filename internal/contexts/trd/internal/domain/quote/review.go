package quote

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Review, approval and margin exception of a frozen revision (WBS S3-03, S3-14; SM-TRD-REVIEW,
// SM-TRD-APPROVAL; TRD-INV-003, 004, 005, 007; Python t03_storage submit_review, decide_review,
// revoke_approval and current_reviewer, T03 A3, A33–A35).
//
//	review request  open → approved | returned | rejected    quote.review.decide
//	                open → cancelled                         quote.withdraw (S3-05)
//	approval        valid → revoked                          quote.approval.revoke
//
// A revision is reviewed once (Python keys review requests by revision): a returned or rejected
// revision is frozen again and is revised for another review. A root has one open review at a
// time. The reviewer is a human independent of the revision's authors — its contributors, its
// freezer and the submitter — and decides on the exact business hash. An approval binds the
// revision, its hash, its target (and the active revision a replacement replaces) and the rule
// versions the revision was calculated under, and lapses at its expires_at, before the quote's. A
// margin below the pricing policy's minimum (calc's requires_approval) needs an explicit margin
// exception besides the approval, bound to the same hash, rule versions and exact shortfall and
// given by a human holding the capability who is independent of the authors too; an ordinary
// approval never stands in for it (BC-AT06). Whether an approval releases its revision is decided
// when it is used (Releasable), on the reviewers' grants as they are then (T03 A33).

// Targets of a review: the command its approval is for.
const (
	TargetActivate = "quote.activate"
	TargetReplace  = "quote.replace"
)

// Decisions of a review.
const (
	DecisionApprove = "approve"
	DecisionReturn  = "return"
	DecisionReject  = "reject"
)

// Permissions a reviewer holds and keeps (T03 A33: quote.review.decide, and the quote read
// permission); an exception approver holds the capability besides.
const (
	PermissionReview          = "quote.review.decide"
	PermissionMarginException = "quote.margin_exception.approve"
	PermissionRead            = "trd.read"
)

// ReviewStatus is the state of a review request (SM-TRD-REVIEW).
type ReviewStatus string

const (
	ReviewOpen      ReviewStatus = "open"
	ReviewApproved  ReviewStatus = "approved"
	ReviewReturned  ReviewStatus = "returned"
	ReviewRejected  ReviewStatus = "rejected"
	ReviewCancelled ReviewStatus = "cancelled"
)

// Effectivity is the state of an approval (SM-TRD-APPROVAL).
type Effectivity string

const (
	Valid   Effectivity = "valid"
	Revoked Effectivity = "revoked"
)

// Act is who did something to a review or approval, under which authority epoch, when, why and on
// which evidence.
type Act struct {
	By       kernel.ActorRef
	Epoch    int64
	At       time.Time
	Reason   string
	Evidence []Evidence
}

// Review is a review request of one frozen revision.
type Review struct {
	ID                      string
	Revision                int64
	BusinessHash            string
	Target                  string
	ExpectedActive          int64 // replace: the active revision it replaces
	MarginExceptionRequired bool  // the revision's margin requires approval
	Status                  ReviewStatus
	Requested               Act  // Reason and Evidence empty
	Decided                 *Act // nil while open
	Fresh                   bool // not stored yet
	Changed                 bool // stored, changed by this command
}

// RuleVersions are the rules a revision was calculated under: the normalization of its commercial
// document, the calculation rules and the pricing policy revision whose minimum margin applied.
type RuleVersions struct {
	NormalizationVersion string
	CalculationVersion   string
	PricingPolicy        Ref
}

// Approval is the approval a review gave: what it binds, until when, its effectivity and its margin
// exception.
type Approval struct {
	ID                 string
	ReviewID           string
	Revision           int64
	BusinessHash       string
	Target             string
	ExpectedActive     int64
	Rules              RuleVersions
	Approved           Act // the reviewer
	ExpiresAt          time.Time
	Effectivity        Effectivity
	EffectivityVersion int64
	Revocation         *Act
	Exception          *MarginException
	Fresh              bool
	Changed            bool
}

// MarginException is the explicit approval of a margin below the minimum: the business hash, the
// rule versions, the minimum margin and the exact shortfall (major units of Currency, the
// comparison currency) it binds, and who approved it.
type MarginException struct {
	BusinessHash  string
	Rules         RuleVersions
	MinimumMargin string
	Shortfall     string
	Currency      string
	Approved      Act
	Fresh         bool
}

// Review returns the review request id, or nil.
func (q *Quote) Review(id string) *Review {
	for _, r := range q.Reviews {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// Approval returns approval id, or nil.
func (q *Quote) Approval(id string) *Approval {
	for _, a := range q.Approvals {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (q *Quote) approvalOf(review string) *Approval {
	for _, a := range q.Approvals {
		if a.ReviewID == review {
			return a
		}
	}
	return nil
}

// Calculation returns the server calculation a sealed revision's commercial document holds, after
// checking the document against its business hash; nil when it was sealed without one.
func (r *Revision) Calculation() (*Calculation, error) {
	if r.Seal == nil {
		return nil, nil
	}
	doc, err := ParseCommercial(r.Seal.Commercial, r.Seal.BusinessHash)
	if err != nil {
		return nil, err
	}
	return doc.Calculation, nil
}

// rulesOf are the rule versions of a calculation.
func rulesOf(c *Calculation) RuleVersions {
	return RuleVersions{NormalizationVersion: NormalizationVersion, CalculationVersion: c.CalculationVersion,
		PricingPolicy: c.CostScope.PricingPolicy}
}

// current are the rule versions this code calculates and normalizes under for a revision whose
// calculation used policy.
func current(policy Ref) RuleVersions {
	return RuleVersions{NormalizationVersion: NormalizationVersion, CalculationVersion: CalculationVersion, PricingPolicy: policy}
}

// authors are the actors a reviewer of rev is independent of: its contributors, its freezer and
// the submitter of review.
func authors(rev *Revision, review *Review) []string {
	out := slices.Clone(rev.Contributors)
	if rev.Seal != nil {
		out = append(out, rev.Seal.By.ID)
	}
	return append(out, review.Requested.By.ID)
}

// target checks that the root is as a review for target expects it: no active revision for
// quote.activate (QUOTE_ACTIVE), the named active revision for quote.replace (NO_ACTIVE_REVISION,
// QUOTE_NOT_CURRENT).
func (q *Quote) target(target string, expectedActive int64, path string) error {
	active := q.Active()
	switch {
	case target == TargetActivate && active != nil:
		return fail(CodeQuoteActive, path, "The quote has an active revision; a replacement replaces it")
	case target == TargetReplace && active == nil:
		return fail(CodeNoActiveRevision, path, "")
	case target == TargetReplace && active.Number != expectedActive:
		return fail(CodeQuoteNotCurrent, path, "Another revision is active")
	}
	return nil
}

// ReviewRequest is what quote.submit_review asks.
type ReviewRequest struct {
	ID             string
	Revision       int64
	Target         string
	ExpectedActive int64
}

// SubmitReview opens a review request of a frozen revision for its target (frozen →
// pending_approval). The revision carries the server calculation under the current rules, was
// never reviewed, and has not expired; the root has no other open review, and is as the target
// expects it. by submits now.
func (q *Quote) SubmitReview(r ReviewRequest, by kernel.ActorRef, epoch int64, now time.Time) (*Review, error) {
	rev := q.Revision(r.Revision)
	if rev == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/business_revision", "")
	}
	if rev.Status != Frozen {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/business_revision", "Only a frozen revision is submitted for review")
	}
	calc, err := rev.Calculation()
	if err != nil {
		return nil, err
	}
	if calc == nil || rulesOf(calc) != current(calc.CostScope.PricingPolicy) {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/business_revision",
			"The revision was not calculated under the current rules; revise and freeze it again")
	}
	switch {
	case slices.ContainsFunc(q.Reviews, func(o *Review) bool { return o.Revision == r.Revision }):
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/business_revision", "The revision was reviewed; revise it for another review")
	case q.Review(r.ID) != nil:
		return nil, fail(CodeBusinessKeyConflict, "/args/review_request_id", "")
	case slices.ContainsFunc(q.Reviews, func(o *Review) bool { return o.Status == ReviewOpen }):
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/quote_id", "Another review of the quote is open")
	}
	if err = q.target(r.Target, r.ExpectedActive, "/args/expected_active_revision"); err != nil {
		return nil, err
	}
	if !kernel.Instant(now).Before(rev.Seal.ExpiresAt) {
		return nil, fail(CodeQuoteExpired, "/args/business_revision", "The quote expired")
	}
	review := &Review{ID: r.ID, Revision: r.Revision, BusinessHash: rev.Seal.BusinessHash, Target: r.Target, ExpectedActive: r.ExpectedActive,
		MarginExceptionRequired: calc.MarginRule == MarginRequiresApproval, Status: ReviewOpen,
		Requested: Act{By: by, Epoch: epoch, At: kernel.Instant(now)}, Fresh: true}
	q.Reviews = append(q.Reviews, review)
	rev.Status, rev.Changed, rev.StatusOnly = PendingApproval, true, true
	q.RootVersion++
	return review, nil
}

// ExceptionClaim is the margin exception a decision approves: what the decider restates of the
// frozen calculation (the pricing policy revision, its minimum margin and the exact shortfall), why
// and on which evidence.
type ExceptionClaim struct {
	PricingPolicy Ref
	MinimumMargin kernel.Decimal
	Shortfall     kernel.Decimal
	Reason        string
	Evidence      []Evidence
}

// ReviewDecision is what quote.review.decide decides.
type ReviewDecision struct {
	ReviewID     string
	BusinessHash string
	Decision     string
	ApprovalID   string    // approve
	ExpiresAt    time.Time // approve
	Reason       string
	Evidence     []Evidence
	Exception    *ExceptionClaim // approve
}

// Decider is the human who decides: the actor, its authority epoch and whether its grant holds
// the margin exception capability now.
type Decider struct {
	Actor           kernel.ActorRef
	Epoch           int64
	MarginException bool
}

// Decide decides review d.ReviewID at now. On an open review: approve creates approval
// d.ApprovalID (with the margin exception when d.Exception is given); return and reject end the
// review and freeze the revision again. On an approved review whose valid approval lacks the margin
// exception the revision needs, approve with d.Exception, the same approval ID and the same
// expires_at adds it. It returns the review and the approval it created or completed.
func (q *Quote) Decide(d ReviewDecision, by Decider, now time.Time) (*Review, *Approval, error) {
	review := q.Review(d.ReviewID)
	if review == nil {
		return nil, nil, kernel.Reject(kernel.CodeNotFound, "/args/review_request_id", "")
	}
	rev := q.Revision(review.Revision)
	if d.BusinessHash != review.BusinessHash || rev == nil || rev.Seal == nil || rev.Seal.BusinessHash != d.BusinessHash {
		return nil, nil, fail(CodeBusinessHashMismatch, "/args/business_hash", "The decision is not on the revision's business hash")
	}
	at := kernel.Instant(now)
	if d.Exception != nil {
		// Authority over the exception first: an author asking for one is refused for that
		// (TRD-INV-007: 编制人自批例外返回 MARGIN_EXCEPTION_NOT_AUTHORIZED).
		if err := exceptionAuthority(by, rev, review); err != nil {
			return nil, nil, err
		}
	}
	switch {
	case review.Status == ReviewOpen:
	case review.Status == ReviewApproved && d.Decision == DecisionApprove && d.Exception != nil:
		a := q.approvalOf(review.ID)
		if a == nil || a.Exception != nil || !review.MarginExceptionRequired {
			return nil, nil, kernel.Reject(kernel.CodeInvalidState, "/args/review_request_id", "The review is decided")
		}
		if err := q.except(d, by, rev, review, a, at, true); err != nil {
			return nil, nil, err
		}
		q.RootVersion++
		return review, a, nil
	default:
		return nil, nil, kernel.Reject(kernel.CodeInvalidState, "/args/review_request_id", "The review is decided")
	}
	if slices.Contains(authors(rev, review), by.Actor.ID) {
		return nil, nil, fail(CodeIndependentReviewRequired, "/args/review_request_id",
			"The reviewer contributed to, froze or submitted the revision")
	}
	act := Act{By: by.Actor, Epoch: by.Epoch, At: at, Reason: d.Reason, Evidence: slices.Clone(d.Evidence)}
	if d.Decision != DecisionApprove {
		review.Status = ReviewReturned
		if d.Decision == DecisionReject {
			review.Status = ReviewRejected
		}
		review.Decided, review.Changed = &act, true
		rev.Status, rev.Changed, rev.StatusOnly = Frozen, true, true
		q.RootVersion++
		return review, nil, nil
	}
	if q.Approval(d.ApprovalID) != nil {
		return nil, nil, fail(CodeBusinessKeyConflict, "/args/approval_id", "")
	}
	calc, err := rev.Calculation()
	if err != nil {
		return nil, nil, err
	}
	if calc == nil || rulesOf(calc) != current(calc.CostScope.PricingPolicy) {
		return nil, nil, kernel.Reject(kernel.CodeInvalidState, "/args/review_request_id", "The revision was not calculated under the current rules")
	}
	if err = q.target(review.Target, review.ExpectedActive, "/args/review_request_id"); err != nil {
		return nil, nil, err
	}
	if err = timely(at, rev.Seal.ExpiresAt, d.ExpiresAt); err != nil {
		return nil, nil, err
	}
	a := &Approval{ID: d.ApprovalID, ReviewID: review.ID, Revision: review.Revision, BusinessHash: review.BusinessHash, Target: review.Target,
		ExpectedActive: review.ExpectedActive, Rules: rulesOf(calc), Approved: act, ExpiresAt: kernel.Instant(d.ExpiresAt),
		Effectivity: Valid, EffectivityVersion: 1, Fresh: true}
	if d.Exception != nil {
		if err = q.except(d, by, rev, review, a, at, false); err != nil {
			return nil, nil, err
		}
	}
	q.Approvals = append(q.Approvals, a)
	review.Status, review.Decided, review.Changed = ReviewApproved, &act, true
	q.RootVersion++
	return review, a, nil
}

// timely checks the time qualification of an approval decided at instant at (TRD-INV-005, Python
// decide_review and T03 A35): the quote has not expired (at < its expires_at) and the approval
// lapses after at and strictly before the quote does.
func timely(at, quoteExpires, approvalExpires time.Time) error {
	if !at.Before(quoteExpires) {
		return fail(CodeQuoteExpired, "/args/review_request_id", "The quote expired")
	}
	approval := kernel.Instant(approvalExpires)
	switch {
	case !approval.After(at):
		return fail(CodeApprovalExpired, "/args/expires_at", "The approval would lapse at or before it is given")
	case !approval.Before(quoteExpires):
		return fail(CodeApprovalExpired, "/args/expires_at", "An approval lapses before the quote expires")
	}
	return nil
}

// except attaches the margin exception d claims to approval a of review: the decider holds the
// capability and is independent of the revision's authors (MARGIN_EXCEPTION_NOT_AUTHORIZED); the
// revision's margin requires approval (INVALID_STATE otherwise); the claim restates the frozen
// calculation's pricing policy, minimum and shortfall (APPROVAL_REFERENCE_MISMATCH); completing an
// approval restates its ID and expires_at, and the approval is valid and unexpired.
func (q *Quote) except(d ReviewDecision, by Decider, rev *Revision, review *Review, a *Approval, at time.Time, completing bool) error {
	const p = "/args/margin_exception"
	if err := exceptionAuthority(by, rev, review); err != nil {
		return err
	}
	calc, err := rev.Calculation()
	if err != nil {
		return err
	}
	if calc == nil || calc.MarginRule != MarginRequiresApproval {
		return kernel.Reject(kernel.CodeInvalidState, p, "The margin passes; there is no exception to approve")
	}
	minimum, err := kernel.ParseDecimal(calc.MinimumMargin, kernel.FactorFormat)
	if err != nil {
		return err
	}
	shortfall, err := kernel.ParseDecimal(calc.MarginShortfall, ShortfallFormat)
	if err != nil {
		return err
	}
	c := d.Exception
	switch {
	case c.PricingPolicy != calc.CostScope.PricingPolicy:
		return fail(CodeApprovalReferenceMismatch, p+"/pricing_policy_ref", "The calculation applied another pricing policy revision")
	case !c.MinimumMargin.Equal(minimum):
		return fail(CodeApprovalReferenceMismatch, p+"/minimum_margin", "The calculation applied minimum margin "+calc.MinimumMargin)
	case !c.Shortfall.Equal(shortfall):
		return fail(CodeApprovalReferenceMismatch, p+"/shortfall", "The calculation's shortfall is "+calc.MarginShortfall)
	}
	if completing {
		switch {
		case d.ApprovalID != a.ID:
			return fail(CodeApprovalReferenceMismatch, "/args/approval_id", "The exception completes the review's approval")
		case !kernel.Instant(d.ExpiresAt).Equal(a.ExpiresAt):
			return fail(CodeApprovalReferenceMismatch, "/args/expires_at", "The exception lapses with the approval it completes")
		case a.Effectivity != Valid:
			return fail(CodeApprovalRevoked, "/args/approval_id", "")
		}
		if err = timely(at, rev.Seal.ExpiresAt, a.ExpiresAt); err != nil {
			return err
		}
	}
	a.Exception = &MarginException{BusinessHash: rev.Seal.BusinessHash, Rules: rulesOf(calc), MinimumMargin: calc.MinimumMargin,
		Shortfall: calc.MarginShortfall, Currency: calc.ComparisonCurrency.ID,
		Approved: Act{By: by.Actor, Epoch: by.Epoch, At: at, Reason: c.Reason, Evidence: slices.Clone(c.Evidence)}, Fresh: true}
	return nil
}

// exceptionAuthority admits a decider to approve a margin exception of review: it holds the
// capability and is independent of the revision's authors (MARGIN_EXCEPTION_NOT_AUTHORIZED).
func exceptionAuthority(by Decider, rev *Revision, review *Review) error {
	if !by.MarginException || slices.Contains(authors(rev, review), by.Actor.ID) {
		return fail(CodeMarginExceptionNotAuthorized, "/args/margin_exception",
			"A margin exception is approved by a human holding the capability who is independent of the revision's authors")
	}
	return nil
}

// Revocation is what quote.approval.revoke asks.
type Revocation struct {
	ApprovalID          string
	ExpectedEffectivity int64
	Reason              string
	Evidence            []Evidence
}

// Revoke revokes an approval at its effectivity version (valid → revoked): it never releases again,
// and its margin exception with it. The root version moves. Revoking the approval the active
// revision was formalized with registers a risk hold on that revision (SM-TRD-APPROVAL 生效后撤销登记风险暂停;
// formal.go), returned with the approval; the revision stays active.
func (q *Quote) Revoke(r Revocation, by kernel.ActorRef, epoch int64, now time.Time) (*Approval, *Hold, error) {
	a := q.Approval(r.ApprovalID)
	switch {
	case a == nil:
		return nil, nil, kernel.Reject(kernel.CodeNotFound, "/args/approval_id", "")
	case a.EffectivityVersion != r.ExpectedEffectivity:
		return nil, nil, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_effectivity_version", "")
	case a.Effectivity != Valid:
		return nil, nil, kernel.Reject(kernel.CodeInvalidState, "/args/approval_id", "The approval is revoked")
	}
	at := kernel.Instant(now)
	a.Effectivity, a.EffectivityVersion, a.Changed = Revoked, a.EffectivityVersion+1, true
	a.Revocation = &Act{By: by, Epoch: epoch, At: at, Reason: r.Reason, Evidence: slices.Clone(r.Evidence)}
	q.RootVersion++
	return a, q.hold(a, by, epoch, at), nil
}

// Standing is an actor's grant as TRD reads it at a use of an approval: whether it is an active
// grant of a human, the permissions it holds, and whether it sees the quote root.
type Standing struct {
	Human       bool
	Permissions []string
	SeesRoot    bool
}

// qualified reports whether the standing is an active human grant seeing the root and holding every
// permission of needs.
func (s Standing) qualified(needs ...string) bool {
	if !s.Human || !s.SeesRoot {
		return false
	}
	for _, p := range needs {
		if !slices.Contains(s.Permissions, p) {
			return false
		}
	}
	return true
}

// Qualified reports whether the standing is an active human grant seeing the quote root and
// holding every permission of needs (an order's reviewer at its activation: package order).
func (s Standing) Qualified(needs ...string) bool { return s.qualified(needs...) }

// Standings reads an actor's standing now.
type Standings func(actorID string) (Standing, error)

// Release is a use of an approval: releasing revision Revision for Target (and, for a replacement,
// replacing ExpectedActive) with approval ApprovalID at its effectivity version
// ExpectedEffectivity (0: any).
type Release struct {
	Revision            int64
	Target              string
	ExpectedActive      int64
	ApprovalID          string
	ExpectedEffectivity int64
}

// Releasable decides whether approval r.ApprovalID releases revision r.Revision for r.Target at
// instant at, on the reviewers' standings now, and returns the approval and the revision's
// calculation. In order: the revision is pending approval; the approval exists (APPROVAL_REQUIRED),
// is of this revision (APPROVAL_REFERENCE_MISMATCH), at the expected effectivity version
// (VERSION_CONFLICT), not revoked (APPROVAL_REVOKED), for this target (APPROVAL_REFERENCE_MISMATCH)
// on a root as the target expects it (QUOTE_ACTIVE, NO_ACTIVE_REVISION, QUOTE_NOT_CURRENT); its
// hash and rule versions are the revision's and the current ones (APPROVAL_STALE); neither the
// quote nor the approval expired at at (QUOTE_EXPIRED, APPROVAL_EXPIRED); the reviewer is still
// independent (INDEPENDENT_REVIEW_REQUIRED) and an active human grant holding quote.review.decide
// and trd.read that sees the quote (APPROVAL_STALE, T03 A33); and a margin that requires approval
// has its exception (MARGIN_POLICY_REQUIRES_APPROVAL), bound like the approval and given by an
// approver still qualified the same way with the capability (APPROVAL_STALE). Activation and
// replacement call it inside their transaction after the root is locked.
func (q *Quote) Releasable(r Release, at time.Time, standings Standings) (*Approval, *Calculation, error) {
	rev := q.Revision(r.Revision)
	switch {
	case rev == nil:
		return nil, nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/business_revision", "")
	case rev.Status != PendingApproval:
		return nil, nil, kernel.Reject(kernel.CodeInvalidState, "/args/business_revision", "The revision is not pending approval")
	}
	a := q.Approval(r.ApprovalID)
	switch {
	case a == nil:
		return nil, nil, fail(CodeApprovalRequired, "/args/approval_id", "")
	case a.Revision != r.Revision:
		return nil, nil, fail(CodeApprovalReferenceMismatch, "/args/approval_id", "The approval is of another revision")
	case r.ExpectedEffectivity != 0 && a.EffectivityVersion != r.ExpectedEffectivity:
		return nil, nil, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_approval_effectivity_version", "")
	case a.Effectivity != Valid:
		return nil, nil, fail(CodeApprovalRevoked, "/args/approval_id", "")
	case a.Target != r.Target || (r.Target == TargetReplace && a.ExpectedActive != r.ExpectedActive):
		return nil, nil, fail(CodeApprovalReferenceMismatch, "/args/approval_id", "The approval is for another target")
	}
	if err := q.target(a.Target, a.ExpectedActive, "/args/approval_id"); err != nil {
		return nil, nil, err
	}
	calc, err := rev.Calculation()
	if err != nil {
		return nil, nil, err
	}
	if calc == nil || a.BusinessHash != rev.Seal.BusinessHash || a.Rules != rulesOf(calc) || a.Rules != current(calc.CostScope.PricingPolicy) {
		return nil, nil, fail(CodeApprovalStale, "/args/approval_id", "The approval is of other content or rules")
	}
	at = kernel.Instant(at)
	switch {
	case !at.Before(rev.Seal.ExpiresAt):
		return nil, nil, fail(CodeQuoteExpired, "/args/business_revision", "The quote expired")
	case !at.Before(a.ExpiresAt):
		return nil, nil, fail(CodeApprovalExpired, "/args/approval_id", "")
	}
	if err := q.stands(rev, a, calc, standings, "/args/approval_id"); err != nil {
		return nil, nil, err
	}
	return a, calc, nil
}

// stands decides whether approval a of sealed revision rev, calculated as calc, is still given by
// qualified people, on their standings now: the reviewer is independent of the revision's authors
// (INDEPENDENT_REVIEW_REQUIRED) and an active human grant holding quote.review.decide and trd.read
// that sees the quote (APPROVAL_STALE, T03 A33); a margin that requires approval has its exception
// (MARGIN_POLICY_REQUIRES_APPROVAL), bound like the approval (APPROVAL_STALE) and given by an
// independent approver (MARGIN_EXCEPTION_NOT_AUTHORIZED) still qualified the same way with the
// capability (APPROVAL_STALE). Refusals point at path. Releasable and the readiness of a formal
// revision (ready.go) decide with it.
func (q *Quote) stands(rev *Revision, a *Approval, calc *Calculation, standings Standings, path string) error {
	review := q.Review(a.ReviewID)
	if review == nil {
		return fail(CodeApprovalStale, path, "The approval's review is missing")
	}
	if slices.Contains(authors(rev, review), a.Approved.By.ID) {
		return fail(CodeIndependentReviewRequired, path, "")
	}
	s, err := standings(a.Approved.By.ID)
	if err != nil {
		return err
	}
	if a.Approved.By.Kind != kernel.ActorHuman || !s.qualified(PermissionReview, PermissionRead) {
		return fail(CodeApprovalStale, path, "The reviewer is no longer qualified")
	}
	if calc.MarginRule != MarginRequiresApproval {
		return nil
	}
	e := a.Exception
	if e == nil {
		return fail(CodeMarginPolicyRequiresApproval, path,
			"The margin is below the minimum by "+calc.MarginShortfall+" "+calc.ComparisonCurrency.ID+"; it needs a margin exception approval")
	}
	if e.BusinessHash != rev.Seal.BusinessHash || e.Rules != a.Rules || e.Shortfall != calc.MarginShortfall || e.MinimumMargin != calc.MinimumMargin {
		return fail(CodeApprovalStale, path, "The margin exception is of other content or rules")
	}
	if slices.Contains(authors(rev, review), e.Approved.By.ID) {
		return fail(CodeMarginExceptionNotAuthorized, path, "")
	}
	if s, err = standings(e.Approved.By.ID); err != nil {
		return err
	}
	if e.Approved.By.Kind != kernel.ActorHuman || !s.qualified(PermissionReview, PermissionRead, PermissionMarginException) {
		return fail(CodeApprovalStale, path, "The margin exception's approver is no longer qualified")
	}
	return nil
}
