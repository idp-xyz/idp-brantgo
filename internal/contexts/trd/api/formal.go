package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Formalization and the end of a revision (WBS S3-04, S3-05; SM-TRD-REVISION; TRD-INV-001, 009,
// 010, 016). All four commands are human only (tactical model humanOnlyCommands) and pass the
// root's object control like every quote write.
//
//	quote.activate  pending_approval → active: the first active revision of the root, released by
//	                an approval for quote.activate.
//	quote.replace   pending_approval → active and the active revision → superseded, released by an
//	                approval for quote.replace of that revision, while the conversion slot is free
//	                (ROOT_CONVERSION_LOCKED).
//	quote.withdraw  frozen or pending_approval → withdrawn, with a reason and new evidence; an open
//	                review of the revision is cancelled.
//	quote.cancel    the active revision → cancelled, with a reason and new evidence, while the
//	                conversion slot is free (ROOT_CONVERSION_LOCKED); QUOTE_NOT_CURRENT for another
//	                revision.
//
// A revision a live (valid or disputed) acceptance accepts — a formed contract — is neither
// superseded by quote.replace, withdrawn nor cancelled (ACCEPTANCE_ALREADY_EXISTS, after the slot's
// ROOT_CONVERSION_LOCKED) until a human invalidates the acceptance (acceptance.invalidate, with a
// reason and evidence): the owner's decision of 2026-11-11. The database refuses it as well.
//
// quote.activate and quote.replace formalize the revision. Before TRD's transaction they check
// everything the transaction will check (so a refusal costs no number), read what the formal
// document prints (PTY: the customer's name at the bound revision and the contact's at the bound
// row version; CAT: each SKU's name at the bound specification revision; POL: the template, the
// currency and the trade terms the document pins), prepare the document (quote-text/0.1.0) and have
// POL issue its number (number.issue, as the acting human, for business reference
// "<quote_id>:<business_revision>" from the revision's number sequence: the same reference gets the
// same number back, a revision never formalized leaves a gap). Then, in one transaction: the quote
// root FOR UPDATE, the company's authority guard row FOR SHARE, the object control, the root
// version, the conversion slot (replacement), the approval's release on the reviewers' grants as
// they are (review.go), the sales exchange rate's age, and the upstream guards the freeze ran, in
// the same order (CAT SkuGuard, CST AdoptabilityGuard, INQ RfqGuard, POL ConfigGuard, PTY
// TradingProfileGuard and AddressRoleGuard: what the revision binds is still in force); the document
// rendered with the number; the document's bytes and binding, the commit evidence, the revision
// statuses, the action result, the audit entry and QuoteActivated or QuoteReplaced — all or nothing
// (TRD-INV-016). Refusals of a bound input point into the stored commercial document ("/data/...").
//
// Request members in snake_case: quote.activate {quote_id, expected_version, business_revision,
// approval_id, expected_approval_effectivity_version}; quote.replace the same and
// expected_active_revision (the revision it supersedes; Python expected_old_active_revision);
// quote.withdraw and quote.cancel {quote_id, expected_version, business_revision, reason,
// evidence}.
const (
	CommandActivate = "quote.activate"
	CommandReplace  = "quote.replace"
	CommandWithdraw = "quote.withdraw"
	CommandCancel   = "quote.cancel"

	EventQuoteActivated = "idp-brant.trd.quote_activated.v1"
	EventQuoteReplaced  = "idp-brant.trd.quote_replaced.v1"
	EventQuoteWithdrawn = "idp-brant.trd.quote_withdrawn.v1"
	EventQuoteCancelled = "idp-brant.trd.quote_cancelled.v1"

	// SlotFullV1 is the conversion slot of a quote root (TRD-INV-010).
	SlotFullV1 = "full_v1"
	// HoldApprovalRevoked is the cause of a risk hold registered by revoking the approval an active
	// revision was formalized with.
	HoldApprovalRevoked = "approval_revoked"
	// HoldCostSourceDenied is the cause of a risk hold TRD's consumer of CST's CostSourceDenied
	// registers on an active revision whose commit evidence names a snapshot resting on the denied
	// source, before a contract formed on it (WF-03, S4 decision D-9); its code is
	// COST_SOURCE_DENIED.
	HoldCostSourceDenied = "cost_source_denied"
)

// Activate formalizes approved revision BusinessRevision as the root's first active revision
// (Python t03 quote.activate). ExpectedApprovalEffectivityVersion is the approval's effectivity
// version (1 while valid). Human only.
type Activate struct {
	IdempotencyKey                     string
	QuoteID                            string
	ExpectedVersion                    int64
	BusinessRevision                   int64
	ApprovalID                         string
	ExpectedApprovalEffectivityVersion int64
}

// Replace formalizes approved revision BusinessRevision in place of active revision
// ExpectedActiveRevision, which becomes superseded (Python t03 quote.replace). Human only.
type Replace struct {
	IdempotencyKey                     string
	QuoteID                            string
	ExpectedVersion                    int64
	BusinessRevision                   int64
	ExpectedActiveRevision             int64
	ApprovalID                         string
	ExpectedApprovalEffectivityVersion int64
}

// Withdraw ends frozen or pending revision BusinessRevision before it takes effect (Python t03
// quote.withdraw), with a reason and new evidence. Human only.
type Withdraw struct {
	IdempotencyKey   string
	QuoteID          string
	ExpectedVersion  int64
	BusinessRevision int64
	Reason           string
	Evidence         []Evidence
}

// Cancel ends active revision BusinessRevision (Python t03 quote.cancel), with a reason and new
// evidence. Human only.
type Cancel struct {
	IdempotencyKey   string
	QuoteID          string
	ExpectedVersion  int64
	BusinessRevision int64
	Reason           string
	Evidence         []Evidence
}

// Validate checks the closed contract.
func (c Activate) Validate() error {
	if err := head(c.IdempotencyKey, c.QuoteID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if err := positive(c.BusinessRevision, "/args/business_revision"); err != nil {
		return err
	}
	if !kernel.ValidIdentifier(c.ApprovalID) {
		return schema("/args/approval_id")
	}
	return positive(c.ExpectedApprovalEffectivityVersion, "/args/expected_approval_effectivity_version")
}

// Validate checks the closed contract.
func (c Replace) Validate() error {
	if err := (Activate{IdempotencyKey: c.IdempotencyKey, QuoteID: c.QuoteID, ExpectedVersion: c.ExpectedVersion,
		BusinessRevision: c.BusinessRevision, ApprovalID: c.ApprovalID,
		ExpectedApprovalEffectivityVersion: c.ExpectedApprovalEffectivityVersion}).Validate(); err != nil {
		return err
	}
	return positive(c.ExpectedActiveRevision, "/args/expected_active_revision")
}

// Validate checks the closed contract.
func (c Withdraw) Validate() error {
	return closing(c.IdempotencyKey, c.QuoteID, c.ExpectedVersion, c.BusinessRevision, c.Reason, c.Evidence)
}

// Validate checks the closed contract.
func (c Cancel) Validate() error {
	return closing(c.IdempotencyKey, c.QuoteID, c.ExpectedVersion, c.BusinessRevision, c.Reason, c.Evidence)
}

func closing(key, id string, expected, revision int64, reason string, proofs []Evidence) error {
	if err := head(key, id, expected, 1); err != nil {
		return err
	}
	if err := positive(revision, "/args/business_revision"); err != nil {
		return err
	}
	if !text(reason, maxText) {
		return schema("/args/reason")
	}
	return evidence(proofs, "/args/evidence")
}

// FormalizationReceipt is the answer of quote.activate and quote.replace: the receipt (RowVersion
// the root version, Status active) and what the formalization bound. A replay rebuilds it from the
// stored root.
type FormalizationReceipt struct {
	Receipt
	BusinessRevision int64
	BusinessHash     string
	ApprovalID       string
	Number           string
	FileName         string
	MimeType         string
	DocumentSHA256   string
	SizeBytes        int
	ReplacedRevision int64 // quote.replace: the revision it superseded
}

// RevisionReceipt is the answer of quote.withdraw and quote.cancel: the receipt (RowVersion the
// root version, Status withdrawn or cancelled) and the revision it ended. A replay rebuilds it from
// the stored root.
type RevisionReceipt struct {
	Receipt
	BusinessRevision int64
	BusinessHash     string
}

// FormalView is what a revision's formalization bound, without the document's bytes
// (GetFormalDocument returns them): the document binding and a summary of the commit evidence
// (Python commit_history: the result entries only as their SHA-256).
type FormalView struct {
	Number          string
	SequenceRef     ConfigRef
	BusinessRef     string
	TemplateRef     ConfigRef
	TemplateHash    string
	RendererVersion string
	FileName        string
	MimeType        string
	DocumentSHA256  string
	SizeBytes       int
	Commit          CommitView
}

// CommitView summarizes the commit evidence of a formalization.
type CommitView struct {
	Command            string
	ApprovalID         string
	EffectivityVersion int64
	ReplacedRevision   int64
	CostSnapshotID     string
	AuthorityEpoch     int64
	ControlEpoch       int64
	RootVersion        int64
	CommittedBy        kernel.ActorRef
	CommittedAt        time.Time
	EntriesSHA256      string
}

// ClosureView is how a revision was withdrawn or cancelled.
type ClosureView struct {
	Status      string
	ClosedBy    kernel.ActorRef
	ClosedAt    time.Time
	Reason      string
	Evidence    []Evidence
	RootVersion int64
}

// HoldView is a risk hold on a formal revision: of a revoked approval (ApprovalID) or of a denied
// cost source (CST's attestation, the source, the snapshot the revision's commit evidence names and
// the event that carried the denial).
type HoldView struct {
	HoldID           string
	BusinessRevision int64
	Cause            string // approval_revoked, cost_source_denied
	ApprovalID       string
	CostSnapshotID   string
	CostSourceID     string
	AttestationID    string
	EventID          string
	Code             string // the code a use of the revision is refused with
	RegisteredBy     kernel.ActorRef
	RegisteredAt     time.Time
	RootVersion      int64
}

// ConversionView is the root's conversion slot, once taken.
type ConversionView struct {
	Slot             string
	BusinessRevision int64
	BusinessHash     string
	OrderID          string
	TakenBy          kernel.ActorRef
	TakenAt          time.Time
	RootVersion      int64
}

// FormalDocument is a revision's formal document with its bytes, verified against its binding on
// every read (FORMAL_DOCUMENT_MISSING, FORMAL_DOCUMENT_INTEGRITY_FAILURE, DOCUMENT_BINDING_MISSING,
// BUSINESS_HASH_MISMATCH otherwise), and the revision's status now.
type FormalDocument struct {
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	Status           string
	Formal           FormalView
	Content          []byte
}

// QuoteRevisionRef names a business revision of a quote {quote_id, business_revision}
// (QuoteRevisionRef of the tactical model).
type QuoteRevisionRef struct {
	QuoteID          string
	BusinessRevision int64
}

// FormalRevision is what TRD says of a formal revision now, for the steps that rely on it
// (customer responses, acceptance, orders): its status, business hash and validity, the number and
// the SHA-256 of its formal document (verified against the stored bytes), whether it is the active
// revision, whether a risk hold pauses it (and with which code), whether the root's conversion slot
// is taken, and Code, the first reason it does not serve new business ("" when it does:
// QUOTE_NOT_CURRENT, the hold's code, or a document integrity code).
type FormalRevision struct {
	Ref             QuoteRevisionRef
	Status          string
	BusinessHash    string
	IssuedAt        time.Time
	ExpiresAt       time.Time
	Number          string
	DocumentSHA256  string
	ApprovalID      string
	Active          bool
	OnHold          bool
	HoldCode        string
	ConversionTaken bool
	Code            string
}

// Check returns nil when the revision serves new business now, and its refusal at path otherwise.
// Whether it is still before its expires_at, and its validity for the caller's own step, are the
// caller's to judge.
func (r FormalRevision) Check(path string) error {
	if r.Code == "" {
		return nil
	}
	return refuse(r.Code, path)
}

// CheckConvertible is Check, and CONVERSION_SLOT_CONFLICT when the root's conversion slot is taken.
func (r FormalRevision) CheckConvertible(path string) error {
	if err := r.Check(path); err != nil {
		return err
	}
	if r.ConversionTaken {
		return Reject(CodeConversionSlotConflict, path)
	}
	return nil
}

// refuse returns a rejection with code at path: a code TRD declares with its category, or a shared
// kernel code (REFERENCE_INVALID).
func refuse(code, path string) error {
	if c, ok := Category(code); ok {
		return kernel.RejectAs(code, c, path, "")
	}
	return kernel.Reject(code, path, "")
}

// MaxRevisionRefs bounds one lookup.
const MaxRevisionRefs = 200

// Lookups answer QuoteRevisionRef lookups of the steps downstream of a formal quote (TRD's own
// customer response, acceptance and order steps, and contexts the Context Map makes TRD upstream
// of). They need no permission beyond an active grant; quotes outside the caller's data scope are
// absent, and so are revisions never formalized. Ask before opening your own transaction; inside
// it, use QuoteRevisionGuard (guard.go).
type Lookups interface {
	// ResolveQuoteRevisions answers at most MaxRevisionRefs references.
	ResolveQuoteRevisions(ctx context.Context, refs []QuoteRevisionRef) (map[QuoteRevisionRef]FormalRevision, error)
	// ResolveActiveRevisions returns the active revision of each quote that has one (at most
	// MaxRevisionRefs quote IDs).
	ResolveActiveRevisions(ctx context.Context, quoteIDs []string) (map[string]FormalRevision, error)
}

// ValidateRevisionRefs checks a lookup's references.
func ValidateRevisionRefs(refs []QuoteRevisionRef) error {
	if len(refs) > MaxRevisionRefs {
		return schema("/refs")
	}
	for i, r := range refs {
		if !kernel.ValidIdentifier(r.QuoteID) {
			return schema(index("/refs", i) + "/quote_id")
		}
		if err := positive(r.BusinessRevision, index("/refs", i)+"/business_revision"); err != nil {
			return err
		}
	}
	return nil
}

// ValidateQuoteIDs checks a lookup's quote IDs.
func ValidateQuoteIDs(ids []string) error {
	if len(ids) > MaxRevisionRefs {
		return schema("/quote_ids")
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) {
			return schema(index("/quote_ids", i))
		}
	}
	return nil
}
