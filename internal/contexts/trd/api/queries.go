package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The read side of QuoteRoot (WBS S3-09; Python T03 A8–A16, t03_query.py and t03_storage.py
// quote_ready, approval_history, document_history, outbox_history, commit_history,
// dispatch_preflight and dispatch_target). Every query is an audited read under trd.read: it
// authorizes the caller now, answers NOT_FOUND for a quote outside the caller's data scope or
// company, records the read (or its refusal) in TRD's audit trail, and changes nothing else
// (X-INV-003). A malformed query is SCHEMA_INVALID, audited too.
//
// Readiness (QuoteReady, T03 A8 quote_ready; Context Map R18 GoalReadiness) says whether the root's
// active revision serves the next step now: it re-checks what quote.activate checked and what the
// order step will (TRD-INV-014) against the current state — the object control as a write by the
// caller would meet it, the formal document's bytes, risk holds, the validity, the approval the
// commit evidence names with its reviewer's (and margin exception approver's) current
// qualification, the upstream guards of everything the revision binds (CAT, CST, INQ, POL, PTY, in
// the freeze's order), the cost scope and sales terms bound again on the current inputs and
// compared with the frozen bindings, and the sales exchange rate's age. A reason is a refusal code
// with the check that gave it; Ready is true when there is none. It is advice: no command takes it
// as a token, each re-checks in its own transaction.
//
// Histories (T03 A9–A13) are paged, payload-free projections of one quote root in a fixed order:
// its revisions (QuoteHistory, by revision number), its review requests with their approvals and
// effectivity (ApprovalHistory, in request order), its formal documents with their integrity now
// (DocumentHistory, by revision), its commit evidence (CommitHistory, by revision), and the
// integration events TRD published for it and for its customer responses and acceptances through
// the Outbox with the relay's delivery state (OutboxHistory, in enqueue order). None returns a
// draft, a commercial document, a document's bytes, labels, internal notes, reasons, evidence or an
// event payload; GetQuote, GetFormalDocument, GetCustomerResponse and GetAcceptance do.
//
// Dispatch (T03 A14–A16) says what holds before a formal revision is sent to the customer: the
// preflight closes the local chain (commit evidence, the active revision, no risk hold, the
// validity, the document's bytes, the approval the commit names still valid, the activation event
// in the Outbox) and the recipient still as bound (PTY: the customer at the bound revision, the
// contact at the bound row version, the address roles in force); the target adds the recipient
// snapshot the sealed commercial document holds (customer, contact, addresses; immutable and
// covered by the business hash) and the channel. Brant has no dispatch channel yet, so a target
// is never ready (EXTERNAL_CHANNEL_NOT_CONFIGURED) and nothing is ever reported as sent.

// Audited read names of the queries (trd.read).
const (
	QueryQuoteReady        = "quote.ready"
	QueryQuoteHistory      = "quote.history"
	QueryApprovalHistory   = "quote.approval.history"
	QueryDocumentHistory   = "quote.document.history"
	QueryCommitHistory     = "quote.commit.history"
	QueryOutboxHistory     = "quote.outbox.history"
	QueryDispatchPreflight = "quote.dispatch.preflight"
	QueryDispatchTarget    = "quote.dispatch.target"
)

// Paging of the histories.
const (
	DefaultPage = 50  // entries of a page when the query names no limit
	MaxPage     = 200 // most entries of one page
)

// Values the dispatch queries report.
const (
	// ExternalDeliveryNotStarted is every dispatch answer's external delivery status: nothing is
	// sent to a customer by Brant yet (Python external_delivery_status).
	ExternalDeliveryNotStarted = "not_started"
	// ChannelNotConfigured is the channel status while Brant has no dispatch channel.
	ChannelNotConfigured = "not_configured"
	// DocumentIntact is a formal document's integrity when its stored bytes are its bound ones.
	DocumentIntact = "intact"
)

// Checks a reason names (QuoteReadiness.Reasons, DispatchPreflight.Reasons).
const (
	CheckRevision          = "revision"           // the revision named is the root's active one
	CheckObjectControl     = "object_control"     // the root's object control admits the caller as a write
	CheckCommitEvidence    = "commit_evidence"    // the revision has its commit evidence
	CheckDocument          = "document"           // the formal document's stored bytes are its bound ones
	CheckRiskHold          = "risk_hold"          // no risk hold pauses the revision
	CheckValidity          = "validity"           // now is before the revision's expires_at
	CheckApproval          = "approval"           // the approval the commit evidence names still releases it
	CheckUpstream          = "upstream"           // the upstream guards of what the revision binds
	CheckInputs            = "inputs"             // the current inputs could be read
	CheckCostScope         = "cost_scope"         // the cost scope bound again equals the frozen binding
	CheckSalesTerms        = "sales_terms"        // the sales terms bound again equal the frozen binding
	CheckSalesFX           = "sales_fx"           // the sales exchange rate is not older than the pricing policy allows
	CheckActivationEvent   = "activation_event"   // QuoteActivated or QuoteReplaced of the revision is in the Outbox
	CheckCustomer          = "customer"           // the customer is confirmed at the bound revision
	CheckContact           = "contact"            // the contact is confirmed at the bound row version, of the customer
	CheckAddresses         = "addresses"          // the address roles the revision binds are in force
	CheckRecipientSnapshot = "recipient_snapshot" // the recipient snapshot is the sealed one
	CheckChannel           = "channel"            // a dispatch channel is configured
)

// Page asks for one page of a history: the entries after position After (0: from the first) in
// the history's order, at most Limit of them (1–MaxPage; 0: DefaultPage). A page's Next is the
// After of the following page, 0 on the last.
type Page struct {
	After int64
	Limit int
}

// HistoryQuery names a quote root and a page of one of its histories {quote_id, after, limit}.
type HistoryQuery struct {
	QuoteID string
	Page
}

// Validate checks the closed contract.
func (q HistoryQuery) Validate() error {
	switch {
	case !kernel.ValidIdentifier(q.QuoteID):
		return schema("/quote_id")
	case q.After < 0 || q.After > maxPosition:
		return schema("/after")
	case q.Limit < 0 || q.Limit > MaxPage:
		return schema("/limit")
	}
	return nil
}

// Size is the number of entries the page asks for.
func (p Page) Size() int {
	if p.Limit == 0 {
		return DefaultPage
	}
	return p.Limit
}

const maxPosition = 1<<53 - 1 // JSON-safe

// ReadyQuery asks whether a quote's active revision is ready now {quote_id, business_revision}.
// BusinessRevision, when not 0, is the revision the caller relies on: another one active is
// QUOTE_NOT_CURRENT (a TSK goal verifies the revision it was set for).
type ReadyQuery struct {
	QuoteID          string
	BusinessRevision int64
}

// Validate checks the closed contract.
func (q ReadyQuery) Validate() error {
	switch {
	case !kernel.ValidIdentifier(q.QuoteID):
		return schema("/quote_id")
	case q.BusinessRevision < 0 || q.BusinessRevision > maxVersion:
		return schema("/business_revision")
	}
	return nil
}

// DispatchQuery names the formal revision to be sent {quote_id, business_revision}.
type DispatchQuery struct {
	QuoteID          string
	BusinessRevision int64
}

// Validate checks the closed contract.
func (q DispatchQuery) Validate() error {
	if !kernel.ValidIdentifier(q.QuoteID) {
		return schema("/quote_id")
	}
	return positive(q.BusinessRevision, "/business_revision")
}

// Reason is why a revision is not ready: the check (Check* constants) that refused, the refusal's
// code (an error catalog row) and the path it points at (in the stored commercial document,
// "/data/...", or the query's own members).
type Reason struct {
	Check string
	Code  string
	Path  string
}

// CostQualification is what CST's AdoptabilityGuard answers now of the cost snapshot a revision
// binds (Python current_cost_qualification): kept apart from the frozen facts, which never change.
type CostQualification struct {
	SnapshotID   string
	ResultHash   string
	EvidenceHash string
	Status       string // CST's adoptability status
	Adoptable    bool
}

// QuoteReadiness is whether a quote's active revision serves the next step now (QuoteReady).
// BusinessRevision is the revision judged: the active one, or the one the query named; 0 when the
// root has none. CostSnapshot is nil when the upstream guards did not reach CST.
type QuoteReadiness struct {
	QuoteID          string
	RootVersion      int64
	BusinessRevision int64
	BusinessHash     string
	Status           string // the judged revision's status
	ExpiresAt        time.Time
	ApprovalID       string // the approval its commit evidence names
	ControlState     string // unbound, bound, stopping or manual
	CostSnapshot     *CostQualification
	Ready            bool
	Reasons          []Reason
	CheckedAt        time.Time
}

// HistoryEvent is one step in a history: what happened (saved, frozen, discarded, review_requested,
// review_approved, review_returned, review_rejected, review_cancelled, margin_exception_approved,
// approval_revoked, activated, replaced, superseded, withdrawn, cancelled, risk_held,
// conversion_slot_taken), by whom and when, the root version it committed at (0 where the record
// keeps none) and, for approval steps, the approval's effectivity version after it.
type HistoryEvent struct {
	Event              string
	By                 kernel.ActorRef
	At                 time.Time
	RootVersion        int64
	EffectivityVersion int64
}

// Events of the histories.
const (
	EventSaved                   = "saved"
	EventFrozen                  = "frozen"
	EventDiscarded               = "discarded"
	EventReviewRequested         = "review_requested"
	EventReviewApproved          = "review_approved"
	EventReviewReturned          = "review_returned"
	EventReviewRejected          = "review_rejected"
	EventReviewCancelled         = "review_cancelled"
	EventMarginExceptionApproved = "margin_exception_approved"
	EventApprovalRevoked         = "approval_revoked"
	EventActivated               = "activated"
	EventReplaced                = "replaced"
	EventSuperseded              = "superseded"
	EventWithdrawn               = "withdrawn"
	EventCancelled               = "cancelled"
	EventRiskHeld                = "risk_held"
	EventConversionSlotTaken     = "conversion_slot_taken"
)

// CalculationSummary is the amounts of a frozen revision's server calculation (Python quote_history
// calculation): minor units of the quote currency, the comparison amounts in the comparison
// currency's, and the margin verdict.
type CalculationSummary struct {
	NetMinor               string
	TaxMinor               string
	PayableMinor           string
	ComparisonRevenueMinor string
	CostMinor              string
	ProfitMinor            string
	MarginRule             string
}

// RevisionHistory is one business revision without its content: status, base, business hash and
// validity, the calculation summary, its formal number and the steps it went through in order.
type RevisionHistory struct {
	BusinessRevision int64
	Status           string
	BaseRevision     int64
	BusinessHash     string // sealed revisions
	IssuedAt         time.Time
	ExpiresAt        time.Time
	Calculation      *CalculationSummary
	Number           string // formalized revisions
	Events           []HistoryEvent
}

// QuoteHistoryPage is a page of a quote root's revisions in revision order (QuoteHistory; Python
// T03 A9 quote_history): the root's versions and pointers, and Next, the last revision of the page
// when more follow.
type QuoteHistoryPage struct {
	QuoteID         string
	RootVersion     int64
	WorkingRevision int64
	ActiveRevision  int64
	LastRevision    int64
	Revisions       []RevisionHistory
	Next            int64
}

// ReviewHistory is one review request with the approval it gave (Python T03 A10 approval_history):
// Seq is its position in request order (from 1). The approval fields are empty for a review that
// gave none. Events are its steps (requested, decided, the margin exception, the revocation) in
// order, without reasons or evidence.
type ReviewHistory struct {
	Seq                     int64
	ReviewRequestID         string
	BusinessRevision        int64
	BusinessHash            string
	TargetCommand           string
	ExpectedActiveRevision  int64
	ReviewStatus            string
	MarginExceptionRequired bool
	ApprovalID              string
	ApprovalExpiresAt       time.Time
	Effectivity             string
	EffectivityVersion      int64
	MarginExceptionApproved bool
	Events                  []HistoryEvent
}

// ApprovalHistoryPage is a page of a quote root's review requests in request order
// (ApprovalHistory); Next is the last Seq of the page when more follow.
type ApprovalHistoryPage struct {
	QuoteID string
	Reviews []ReviewHistory
	Next    int64
}

// DocumentHistory is a formal document's binding without its bytes (Python T03 A11
// document_history), the revision's status now, who formalized it and when, and Integrity: intact
// when the stored bytes are the bound ones now, otherwise the refusal code a read of the document
// gets (FORMAL_DOCUMENT_MISSING, FORMAL_DOCUMENT_INTEGRITY_FAILURE, DOCUMENT_BINDING_MISSING,
// BUSINESS_HASH_MISMATCH).
type DocumentHistory struct {
	BusinessRevision int64
	BusinessHash     string
	RevisionStatus   string
	Number           string
	SequenceRef      ConfigRef
	BusinessRef      string
	TemplateRef      ConfigRef
	TemplateHash     string
	RendererVersion  string
	FileName         string
	MimeType         string
	DocumentSHA256   string
	SizeBytes        int
	FormalizedBy     kernel.ActorRef
	FormalizedAt     time.Time
	Integrity        string
}

// DocumentHistoryPage is a page of a quote root's formal documents in revision order
// (DocumentHistory); Next is the last revision of the page when more follow.
type DocumentHistoryPage struct {
	QuoteID   string
	Documents []DocumentHistory
	Next      int64
}

// CommitHistory is a formalization's commit evidence (Python T03 A13 commit_history): the command
// and the action it committed (ActionID, its action result), the approval at its effectivity
// version, the revision it superseded, the cost snapshot the guards found adoptable and whether
// CST's verdict is recorded, the epochs and root version it committed at, who and when, the
// formal number and document SHA-256, and the SHA-256 of its entries (never the entries
// themselves).
type CommitHistory struct {
	BusinessRevision     int64
	BusinessHash         string
	Command              string
	ActionID             string
	ApprovalID           string
	EffectivityVersion   int64
	ReplacedRevision     int64
	CostSnapshotID       string
	HasCostQualification bool
	AuthorityEpoch       int64
	ControlEpoch         int64
	RootVersion          int64
	CommittedBy          kernel.ActorRef
	CommittedAt          time.Time
	Number               string
	DocumentSHA256       string
	EntriesSHA256        string
}

// CommitHistoryPage is a page of a quote root's commit evidence in revision order (CommitHistory);
// Next is the last revision of the page when more follow.
type CommitHistoryPage struct {
	QuoteID string
	Commits []CommitHistory
	Next    int64
}

// Relay states of an Outbox event (the IDP-Bento-Go relay's): pending until a worker leases it,
// published once the sink took it, abandoned after its failure budget (a dead letter, replayed by
// a human with the same event ID).
const (
	RelayPending   = "pending"
	RelayLeased    = "leased"
	RelayPublished = "published"
	RelayAbandoned = "abandoned"
)

// OutboxEntry is one integration event TRD published (Python T03 A12 outbox_history), without its
// payload: Seq is its position in the Outbox (enqueue order), the aggregate it belongs to (QuoteRoot,
// CustomerResponse or Acceptance) at its version, the business revision it names (0: none), the
// action that published it, the SHA-256 of its stored payload, and the relay's delivery state with
// its attempts. ExternalDelivery is always ExternalDeliveryNotStarted: delivery to the integration
// sink is not delivery to a customer.
type OutboxEntry struct {
	Seq              int64
	EventID          string
	EventType        string
	AggregateType    string
	AggregateID      string
	AggregateVersion int64
	BusinessRevision int64
	ActionID         string
	OccurredAt       time.Time
	PayloadSHA256    string
	RelayStatus      string
	Attempts         int64
	PublishedAt      time.Time
	AbandonedAt      time.Time
	ExternalDelivery string
}

// OutboxHistoryPage is a page of the events TRD published for a quote root, its customer responses
// and their acceptances, in enqueue order (OutboxHistory); Next is the last Seq of the page when
// more follow.
type OutboxHistoryPage struct {
	QuoteID string
	Events  []OutboxEntry
	Next    int64
}

// DispatchPreflight is what holds before formal revision BusinessRevision is sent to the customer
// (Python T03 A14 dispatch_preflight): the facts it binds, the activation event's ID, the reasons
// it may not be sent (Ready when there is none) and the external delivery status, always
// ExternalDeliveryNotStarted.
type DispatchPreflight struct {
	QuoteID           string
	BusinessRevision  int64
	BusinessHash      string
	Status            string
	Number            string
	DocumentSHA256    string
	ApprovalID        string
	ActionID          string // the formalization's action
	ActivationEventID string // QuoteActivated or QuoteReplaced of the revision; "" when the Outbox has none
	Ready             bool
	Reasons           []Reason
	CheckedAt         time.Time
	ExternalDelivery  string
}

// RecipientAddress is an address of the recipient snapshot: the bound address role with its role
// and text as the freeze read them.
type RecipientAddress struct {
	Ref     AddressRoleRef
	Role    string
	Address string
}

// DispatchTarget is the recipient of a formal revision (Python T03 A15 dispatch_target and A16
// recipient snapshot): the customer, contact and addresses its sealed commercial document binds,
// the SHA-256 of that snapshot (canonical JSON of {customer_ref, contact_ref, addresses}), the
// preflight's verdict, and the channel. TargetReady needs a ready preflight and a configured
// channel, so it is false while Brant has none; the reasons are the preflight's and the channel's.
type DispatchTarget struct {
	QuoteID                 string
	BusinessRevision        int64
	BusinessHash            string
	CustomerRef             PartyRef
	ContactRef              ContactRef
	Addresses               []RecipientAddress
	RecipientSnapshotSHA256 string
	PreflightReady          bool
	ChannelStatus           string
	TargetReady             bool
	Reasons                 []Reason
	CheckedAt               time.Time
	ExternalDelivery        string
}

// ReadQueries are the read side of WBS S3-09, each an audited read under trd.read.
type ReadQueries interface {
	// QuoteReady says whether the quote's active revision serves the next step now, audited as
	// quote.ready. A refusal of a check is a reason, not an error.
	QuoteReady(ctx context.Context, q ReadyQuery) (QuoteReadiness, error)
	// QuoteHistory returns a page of the quote's revisions, audited as quote.history.
	QuoteHistory(ctx context.Context, q HistoryQuery) (QuoteHistoryPage, error)
	// ApprovalHistory returns a page of the quote's review requests and approvals, audited as
	// quote.approval.history.
	ApprovalHistory(ctx context.Context, q HistoryQuery) (ApprovalHistoryPage, error)
	// DocumentHistory returns a page of the quote's formal documents with their integrity now,
	// audited as quote.document.history.
	DocumentHistory(ctx context.Context, q HistoryQuery) (DocumentHistoryPage, error)
	// CommitHistory returns a page of the quote's commit evidence, audited as quote.commit.history.
	CommitHistory(ctx context.Context, q HistoryQuery) (CommitHistoryPage, error)
	// OutboxHistory returns a page of the events TRD published for the quote, its responses and
	// acceptances, audited as quote.outbox.history.
	OutboxHistory(ctx context.Context, q HistoryQuery) (OutboxHistoryPage, error)
	// DispatchPreflight says what holds before the revision is sent, audited as
	// quote.dispatch.preflight; NOT_FOUND for a revision the root does not have.
	DispatchPreflight(ctx context.Context, q DispatchQuery) (DispatchPreflight, error)
	// DispatchTarget returns the revision's recipient and whether it can be sent, audited as
	// quote.dispatch.target; NOT_FOUND for a revision the root does not have.
	DispatchTarget(ctx context.Context, q DispatchQuery) (DispatchTarget, error)
}
