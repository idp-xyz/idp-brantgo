package api

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Review, approval and margin exception of a frozen revision (WBS S3-03, S3-14; tactical model
// SM-TRD-REVISION, SM-TRD-REVIEW, SM-TRD-APPROVAL; TRD-INV-003, 004, 005, 007). All three commands
// are human only (tactical model humanOnlyCommands) and pass the root's object control like every
// quote write.
//
//	quote.submit_review   frozen → pending_approval: opens review request review_request_id for
//	                      business revision business_revision, for target_command quote.activate or
//	                      quote.replace (with expected_active_revision, the active revision it
//	                      replaces). A revision is reviewed once and a root has one open review.
//	quote.review.decide   the review's decision by an independent human reviewer (neither a
//	                      contributor to the revision, nor its freezer, nor the submitter: otherwise
//	                      INDEPENDENT_REVIEW_REQUIRED), restating the revision's business_hash
//	                      (BUSINESS_HASH_MISMATCH). decision approve creates approval approval_id
//	                      until expires_at (after now, before the quote's expires_at: APPROVAL_EXPIRED);
//	                      return and reject (reason required) end the review, and the revision is
//	                      frozen again: revise it for another review.
//	quote.approval.revoke valid → revoked at expected_effectivity_version, with a reason and new
//	                      evidence: a revoked approval never releases again.
//
// The approval binds the revision, its business hash, the target (and the active revision a
// replacement replaces) and the rule versions the revision was calculated under: the normalization
// version, the calculation version and the pricing policy revision. A revision with another hash
// or rule version never inherits it.
//
// When the server calculation's margin verdict is requires_approval (TRD-INV-007), an ordinary
// approval does not release the revision (MARGIN_POLICY_REQUIRES_APPROVAL, BC-AT06). A reviewer
// who also holds quote.margin_exception.approve and is independent of the revision's authors
// approves the exception explicitly with margin_exception in quote.review.decide, restating the
// pricing policy revision, the minimum margin and the exact shortfall the calculation sealed
// (APPROVAL_REFERENCE_MISMATCH otherwise); without the capability, or as an author, that is
// MARGIN_EXCEPTION_NOT_AUTHORIZED. The exception comes with the approval or, by another decide
// with decision approve on the approved review (same approval_id and expires_at), after it. Illegal
// amounts, scope errors and missing authority are refusals of the freeze or of the command and are
// never overridable.
//
// Whether an approval releases its revision is decided at the use (quote.activate and
// quote.replace, which come with formalization; CheckApproval meanwhile): the approval is valid
// (not revoked) and bound to this revision, hash, target and the current rule versions; neither
// the quote nor the approval expired (as_of < expires_at, the instant compared exactly: Python T03
// A35); the reviewer is still independent, human, holds quote.review.decide and trd.read and sees
// the quote (APPROVAL_STALE otherwise: T03 A33); and, when the margin requires approval, the
// exception is there and its approver still qualified the same way and holds the capability.
const (
	CommandSubmitReview   = "quote.submit_review"
	CommandDecideReview   = "quote.review.decide"
	CommandRevokeApproval = "quote.approval.revoke"

	// PermissionMarginException is the capability quote.review.decide checks of a decider who
	// approves a margin exception (role templates: capabilities).
	PermissionMarginException = "quote.margin_exception.approve"

	EventQuoteReviewRequested = "idp-brant.trd.quote_review_requested.v1"
	EventQuoteReviewDecided   = "idp-brant.trd.quote_review_decided.v1"
	EventQuoteApprovalRevoked = "idp-brant.trd.quote_approval_revoked.v1"

	TargetActivate = "quote.activate"
	TargetReplace  = "quote.replace"

	DecisionApprove = "approve"
	DecisionReturn  = "return"
	DecisionReject  = "reject"

	// Review request states (SM-TRD-REVIEW) and approval effectivity (SM-TRD-APPROVAL).
	ReviewOpen      = "open"
	ReviewApproved  = "approved"
	ReviewReturned  = "returned"
	ReviewRejected  = "rejected"
	ReviewCancelled = "cancelled"
	ApprovalValid   = "valid"
	ApprovalRevoked = "revoked"
)

// SubmitReview opens review request ReviewRequestID for BusinessRevision (Python
// t03_quote_submit_review, with the revision named rather than inferred). ExpectedActiveRevision is
// the active revision a replacement replaces (TargetCommand quote.replace) and 0 for
// quote.activate. Human only.
type SubmitReview struct {
	IdempotencyKey         string
	QuoteID                string
	ExpectedVersion        int64
	BusinessRevision       int64
	ReviewRequestID        string
	TargetCommand          string
	ExpectedActiveRevision int64
}

// MarginException is the explicit approval of a margin below the pricing policy's minimum: the
// pricing policy revision whose minimum applies, that minimum (a FactorFormat decimal) and the
// exact shortfall in major units of the comparison currency (ShortfallFormat), as the frozen
// calculation states them, with a reason and optional evidence.
type MarginException struct {
	PricingPolicyRef ConfigRef
	MinimumMargin    string
	Shortfall        string
	Reason           string
	Evidence         []Evidence
}

// DecideReview decides review request ReviewRequestID of the quote (Python t03_quote_decide_review).
// BusinessHash restates the revision's hash. Decision approve needs ApprovalID and ExpiresAt (a
// timestamp); return and reject need Reason and take neither. Evidence is optional. MarginException
// goes with approve only. Human only.
type DecideReview struct {
	IdempotencyKey  string
	QuoteID         string
	ExpectedVersion int64
	ReviewRequestID string
	BusinessHash    string
	Decision        string
	ApprovalID      string
	ExpiresAt       string
	Reason          string
	Evidence        []Evidence
	MarginException *MarginException
}

// RevokeApproval revokes approval ApprovalID of the quote at its effectivity version (Python
// t03_quote_revoke_approval), with a reason and new evidence. Human only.
type RevokeApproval struct {
	IdempotencyKey             string
	QuoteID                    string
	ApprovalID                 string
	ExpectedEffectivityVersion int64
	Reason                     string
	Evidence                   []Evidence
}

// Validate checks the closed contract.
func (c SubmitReview) Validate() error {
	if err := head(c.IdempotencyKey, c.QuoteID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if err := positive(c.BusinessRevision, "/args/business_revision"); err != nil {
		return err
	}
	if !kernel.ValidIdentifier(c.ReviewRequestID) {
		return schema("/args/review_request_id")
	}
	switch c.TargetCommand {
	case TargetActivate:
		if c.ExpectedActiveRevision != 0 {
			return schema("/args/expected_active_revision")
		}
	case TargetReplace:
		return positive(c.ExpectedActiveRevision, "/args/expected_active_revision")
	default:
		return schema("/args/target_command")
	}
	return nil
}

// Validate checks the closed contract.
func (c DecideReview) Validate() error {
	if err := head(c.IdempotencyKey, c.QuoteID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	switch {
	case !kernel.ValidIdentifier(c.ReviewRequestID):
		return schema("/args/review_request_id")
	case !hash64.MatchString(c.BusinessHash):
		return schema("/args/business_hash")
	}
	switch c.Decision {
	case DecisionApprove:
		if !kernel.ValidIdentifier(c.ApprovalID) {
			return schema("/args/approval_id")
		}
		if _, err := kernel.ParseTimestamp(c.ExpiresAt); err != nil {
			return schema("/args/expires_at")
		}
		if c.Reason != "" && !text(c.Reason, maxText) {
			return schema("/args/reason")
		}
	case DecisionReturn, DecisionReject:
		switch {
		case c.ApprovalID != "":
			return schema("/args/approval_id")
		case c.ExpiresAt != "":
			return schema("/args/expires_at")
		case !text(c.Reason, maxText):
			return schema("/args/reason")
		case c.MarginException != nil:
			return schema("/args/margin_exception")
		}
	default:
		return schema("/args/decision")
	}
	if c.Evidence != nil {
		if err := evidence(c.Evidence, "/args/evidence"); err != nil {
			return err
		}
	}
	if m := c.MarginException; m != nil {
		const p = "/args/margin_exception"
		if err := ref(m.PricingPolicyRef, p+"/pricing_policy_ref"); err != nil {
			return err
		}
		switch {
		case !factor(m.MinimumMargin):
			return schema(p + "/minimum_margin")
		case !shortfall(m.Shortfall):
			return schema(p + "/shortfall")
		case !text(m.Reason, maxText):
			return schema(p + "/reason")
		}
		if m.Evidence != nil {
			return evidence(m.Evidence, p+"/evidence")
		}
	}
	return nil
}

// Validate checks the closed contract.
func (c RevokeApproval) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.QuoteID):
		return schema("/args/quote_id")
	case !kernel.ValidIdentifier(c.ApprovalID):
		return schema("/args/approval_id")
	case c.ExpectedEffectivityVersion < 1 || c.ExpectedEffectivityVersion > maxVersion:
		return schema("/args/expected_effectivity_version")
	case !text(c.Reason, maxText):
		return schema("/args/reason")
	}
	return evidence(c.Evidence, "/args/evidence")
}

// factor reports whether s is a FactorFormat decimal.
func factor(s string) bool {
	_, err := kernel.ParseDecimal(s, kernel.FactorFormat)
	return err == nil
}

// ShortfallFormat is the lexical form of a margin shortfall: an exact decimal in major units of the
// comparison currency (R × minimum − (R − C), R and C of at most 4 minor digits, the minimum of at
// most 12), below 10^24.
var ShortfallFormat = kernel.DecimalFormat{IntDigits: 24, Scale: 16}

// shortfall reports whether s is a positive ShortfallFormat decimal.
func shortfall(s string) bool {
	d, err := kernel.ParseDecimal(s, ShortfallFormat)
	return err == nil && d.Sign() > 0
}

// ReviewReceipt is the answer of the review commands: the receipt (RowVersion the root version;
// Status pending_approval for quote.submit_review, the review's state approved, returned or
// rejected for quote.review.decide, revoked for quote.approval.revoke) and what the command decided
// on. A replay rebuilds it from the stored root.
type ReviewReceipt struct {
	Receipt
	BusinessRevision        int64
	BusinessHash            string
	ReviewRequestID         string
	ApprovalID              string // approvals only
	EffectivityVersion      int64  // approvals only
	MarginExceptionRequired bool
	MarginExceptionApproved bool
}

// ReviewView is one review request of a quote.
type ReviewView struct {
	ReviewRequestID         string
	BusinessRevision        int64
	BusinessHash            string
	TargetCommand           string
	ExpectedActiveRevision  int64 // replace only
	MarginExceptionRequired bool
	Status                  string
	RequestedBy             kernel.ActorRef
	RequestedAt             time.Time
	DecidedBy               kernel.ActorRef // zero while open
	DecidedAt               time.Time
	Reason                  string
	Evidence                []Evidence
}

// ApprovalView is one approval with its effectivity and margin exception.
type ApprovalView struct {
	ApprovalID             string
	ReviewRequestID        string
	BusinessRevision       int64
	BusinessHash           string
	TargetCommand          string
	ExpectedActiveRevision int64
	NormalizationVersion   string
	CalculationVersion     string
	PricingPolicyRef       ConfigRef
	Reviewer               kernel.ActorRef
	DecidedAt              time.Time
	ExpiresAt              time.Time
	Reason                 string
	Evidence               []Evidence
	Effectivity            string
	EffectivityVersion     int64
	RevokedBy              kernel.ActorRef
	RevokedAt              time.Time
	RevokeReason           string
	RevokeEvidence         []Evidence
	MarginException        *MarginExceptionView
}

// MarginExceptionView is an approved margin exception and what it binds.
type MarginExceptionView struct {
	ApprovedBy         kernel.ActorRef
	ApprovedAt         time.Time
	BusinessHash       string
	PricingPolicyRef   ConfigRef
	CalculationVersion string
	MinimumMargin      string
	Shortfall          string
	Currency           string
	Reason             string
	Evidence           []Evidence
}

// ApprovalCheck is CheckApproval's answer: the approval, what it is for, and whether it releases its
// revision at CheckedAt; Code and Path are the first refusal when it does not (APPROVAL_REVOKED,
// APPROVAL_EXPIRED, QUOTE_EXPIRED, APPROVAL_STALE, INDEPENDENT_REVIEW_REQUIRED,
// MARGIN_POLICY_REQUIRES_APPROVAL, QUOTE_ACTIVE, QUOTE_NOT_CURRENT, INVALID_STATE and the like).
type ApprovalCheck struct {
	QuoteID                 string
	ApprovalID              string
	BusinessRevision        int64
	BusinessHash            string
	TargetCommand           string
	EffectivityVersion      int64
	MarginRule              string
	MarginExceptionRequired bool
	MarginExceptionApproved bool
	Releasable              bool
	Code                    string
	Path                    string
	CheckedAt               time.Time
}
