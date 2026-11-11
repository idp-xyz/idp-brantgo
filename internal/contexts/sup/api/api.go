// Package api is the contract of the SUP bounded context (supply sourcing and supply conditions:
// supplier RFQs, supplier offers, purchase terms and logistics offers) for transports, the AI layer
// and the contexts downstream of SUP (CST under R09, PUR under R20). It exposes no SUP domain type.
//
// A supplier RFQ records the human fact that the company asked a supplier for prices on lines of a
// ready customer RFQ; issuing it seals its content (SM-SUP-SRFQ).
//
// Supplier offers, purchase terms and logistics offers are supply conditions: roots with a root
// version every command compares and increments, and revisions with monotonic numbers (T02 §13.2,
// SM-SUP-OFFER). One draft revision is saved whole; a human verifies it (purchase terms: publishes
// it, always independently of every editor of the draft, SUP-INV-003), which seals its content for
// good and supersedes the active revision in the same transaction; a correction is a new revision
// (revise), never an edit (SUP-INV-001). Withdrawing (purchase terms: retiring) an exact sealed
// revision ends it; withdrawing the active one leaves the root without an active revision, and no
// older revision comes back.
//
// Downstream contexts adopt an exact revision as a SupplyConditionRef v1 and ask SUP whether it is
// in force at an instant: before their transaction through Queries.ResolveSupplyConditions, inside
// it through SupplyConditionGuard v1 (guard.go), the guard function SUP publishes in its schema.
package api

import (
	"context"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// Contract names the request and response shapes of SUP commands.
	Contract = "brant.sup/0.1.0"
	// Source identifies SUP as the producer of integration events.
	Source = "idp-brant.sup"

	EventSupplierRfqIssued      = "idp-brant.sup.supplier_rfq_issued.v1"
	EventSupplierOfferVerified  = "idp-brant.sup.supplier_offer_verified.v1"
	EventSupplierOfferWithdrawn = "idp-brant.sup.supplier_offer_withdrawn.v1"
	EventPurchaseTermsPublished = "idp-brant.sup.purchase_terms_published.v1"
	EventPurchaseTermsRetired   = "idp-brant.sup.purchase_terms_retired.v1"
	EventLogisticsOfferVerified = "idp-brant.sup.logistics_offer_verified.v1"
	// EventLogisticsOfferWithdrawn is not in the event catalog V0.1 yet; it tells CST that a
	// logistics offer revision ended, as EventSupplierOfferWithdrawn does for supplier offers.
	EventLogisticsOfferWithdrawn = "idp-brant.sup.logistics_offer_withdrawn.v1"
)

// Kinds of supply condition: the roots a SupplyConditionRef names.
const (
	KindSupplierOffer  = "supplier_offer"
	KindPurchaseTerms  = "purchase_terms"
	KindLogisticsOffer = "logistics_offer"
)

var kinds = []string{KindSupplierOffer, KindPurchaseTerms, KindLogisticsOffer}

// KindKnown reports whether kind is a supply condition kind.
func KindKnown(kind string) bool { return slices.Contains(kinds, kind) }

// Actions of a supply condition. A command is "<kind>.<action>"; purchase terms are published and
// retired, offers are verified and withdrawn.
const (
	ActionSaveDraft    = "save_draft"
	ActionRevise       = "revise"
	ActionDiscardDraft = "discard_draft"
	ActionVerify       = "verify"
	ActionPublish      = "publish"
	ActionWithdraw     = "withdraw"
	ActionRetire       = "retire"
)

// Commands of SUP. The Context Map lists supplier_rfq.*, supplier_offer.save_draft, verify, revise
// and withdraw, purchase_terms.save_draft, publish and retire, and logistics_offer.save_draft,
// verify and withdraw; the tactical model and T02 §13.2 add discard_draft and revise for every
// supply condition kind.
const (
	CommandSaveSupplierRfq  = "supplier_rfq.save_draft"
	CommandIssueSupplierRfq = "supplier_rfq.record_issued" // human only
	CommandCloseSupplierRfq = "supplier_rfq.close"         // human only

	CommandSaveSupplierOffer     = "supplier_offer.save_draft"
	CommandReviseSupplierOffer   = "supplier_offer.revise"
	CommandDiscardSupplierOffer  = "supplier_offer.discard_draft"
	CommandVerifySupplierOffer   = "supplier_offer.verify"   // human only, independent of the draft's editors
	CommandWithdrawSupplierOffer = "supplier_offer.withdraw" // human only

	CommandSavePurchaseTerms    = "purchase_terms.save_draft"
	CommandRevisePurchaseTerms  = "purchase_terms.revise"
	CommandDiscardPurchaseTerms = "purchase_terms.discard_draft"
	CommandPublishPurchaseTerms = "purchase_terms.publish" // human only, independent of the draft's editors (SUP-INV-003)
	CommandRetirePurchaseTerms  = "purchase_terms.retire"  // human only

	CommandSaveLogisticsOffer     = "logistics_offer.save_draft"
	CommandReviseLogisticsOffer   = "logistics_offer.revise"
	CommandDiscardLogisticsOffer  = "logistics_offer.discard_draft"
	CommandVerifyLogisticsOffer   = "logistics_offer.verify"   // human only, independent of the draft's editors
	CommandWithdrawLogisticsOffer = "logistics_offer.withdraw" // human only
)

// SealAction is the action that seals a draft revision of kind: publish for purchase terms, verify
// otherwise.
func SealAction(kind string) string {
	if kind == KindPurchaseTerms {
		return ActionPublish
	}
	return ActionVerify
}

// EndAction is the action that ends a sealed revision of kind: retire for purchase terms, withdraw
// otherwise.
func EndAction(kind string) string {
	if kind == KindPurchaseTerms {
		return ActionRetire
	}
	return ActionWithdraw
}

// Command returns the command name of action on kind, such as "supplier_offer.verify".
func Command(kind, action string) string { return kind + "." + action }

// Statuses of a supplier RFQ (SM-SUP-SRFQ) and of a supply condition revision (SM-SUP-OFFER).
const (
	StatusDraft      = "draft"
	StatusIssued     = "issued"
	StatusClosed     = "closed"
	StatusActive     = "active"
	StatusSuperseded = "superseded"
	StatusWithdrawn  = "withdrawn"
	StatusRetired    = "retired"
	StatusDiscarded  = "discarded"
)

// Codes SUP declares, with the category the error catalog V0.1 gives them (422 invalid, 409
// conflict). The pricing codes are CST's too: SUP verifies a supply condition with the rules CST
// evaluates it with, so both answer the same input with the same code.
const (
	CodeIndependentReviewRequired = "INDEPENDENT_REVIEW_REQUIRED"
	CodePriceTierAmbiguous        = "PRICE_TIER_AMBIGUOUS"
	CodeUnsupportedPricingMode    = "UNSUPPORTED_PRICING_MODE"
	CodeCostInputNotReady         = "COST_INPUT_NOT_READY"
	CodeCalculationLimitExceeded  = "CALCULATION_LIMIT_EXCEEDED"
	CodeChargeDoubleCount         = "CHARGE_DOUBLE_COUNT"
	CodeAllocationInvalid         = "ALLOCATION_INVALID"
	CodeReferenceStale            = "REFERENCE_STALE"              // 409: refresh the reference and resubmit
	CodeSupplyConditionNotCurrent = "SUPPLY_CONDITION_NOT_CURRENT" // 409: superseded, withdrawn, retired or not verified
	CodeSupplyConditionExpired    = "SUPPLY_CONDITION_EXPIRED"     // the validity does not contain the instant
)

var codes = map[string]kernel.Category{
	CodeIndependentReviewRequired: kernel.CategoryInvalid,
	CodePriceTierAmbiguous:        kernel.CategoryInvalid,
	CodeUnsupportedPricingMode:    kernel.CategoryInvalid,
	CodeCostInputNotReady:         kernel.CategoryInvalid,
	CodeCalculationLimitExceeded:  kernel.CategoryInvalid,
	CodeChargeDoubleCount:         kernel.CategoryInvalid,
	CodeAllocationInvalid:         kernel.CategoryInvalid,
	CodeReferenceStale:            kernel.CategoryConflict,
	CodeSupplyConditionNotCurrent: kernel.CategoryConflict,
	CodeSupplyConditionExpired:    kernel.CategoryInvalid,
}

// Category returns the category of a code SUP declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Reject returns a rejection with a code SUP declares, for downstream contexts that pass a SUP
// verdict on at a path of their own request. It panics on a code SUP does not declare.
func Reject(code, path string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("sup: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, "")
}

// Limits of the contract (T02 §13.6 and the Python V0.50 command schema).
const (
	MaxLines       = 200 // condition lines of a revision, RFQ lines an offer or supplier RFQ covers
	MaxTiers       = 50  // tiers of one condition line or tariff
	MaxEvidence    = 100 // evidence entries of a document or a command
	MaxFees        = 500 // charges of a logistics offer
	MaxConversions = 8   // unit conversions one condition line cites
	MaxObligations = 100 // obligations a price includes
	MaxLookup      = 200 // references of one lookup

	maxVersion       = 2147483647
	maxText          = 1000
	maxItemCode      = 500
	maxTimeZone      = 64
	maxExplicitDigit = 18
)

// SupplyConditionRef is SupplyConditionRef v1: a supply condition and the exact revision a
// downstream fact adopted.
type SupplyConditionRef struct {
	Kind     string `json:"kind"` // supplier_offer, purchase_terms or logistics_offer
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// Validate checks the form of the reference; path is where it sits in the request.
func (r SupplyConditionRef) Validate(path string) error {
	switch {
	case !KindKnown(r.Kind):
		return schema(path + "/kind")
	case !kernel.ValidIdentifier(r.ID):
		return schema(path + "/id")
	case r.Revision < 1 || r.Revision > maxVersion:
		return schema(path + "/revision")
	}
	return nil
}

// Evidence is a source a document or a command cites and where in it (Python evidence:
// source_id and locator).
type Evidence struct {
	SourceID string `json:"source_id"`
	Locator  string `json:"locator"`
}

// SaveSupplierRfq creates a draft supplier RFQ at ExpectedVersion 0 or replaces its whole draft at
// the current row version; an omitted field clears it.
type SaveSupplierRfq struct {
	IdempotencyKey  string
	SupplierRfqID   string // args.id
	ExpectedVersion int64
	Data            SupplierRfq
}

// RecordSupplierRfqIssued records that a human sent the complete draft to its supplier, on evidence
// of the sending, and seals its content. Only a human records it.
type RecordSupplierRfqIssued struct {
	IdempotencyKey  string
	SupplierRfqID   string // args.id
	ExpectedVersion int64
	Evidence        []Evidence
	Reason          string
}

// CloseSupplierRfq closes an issued supplier RFQ for a recorded reason; the issued content stays.
// A correction is a new supplier RFQ. Only a human closes it.
type CloseSupplierRfq struct {
	IdempotencyKey  string
	SupplierRfqID   string // args.id
	ExpectedVersion int64
	Evidence        []Evidence
	Reason          string
}

// SaveSupplierOffer, SavePurchaseTerms and SaveLogisticsOffer create a supply condition at
// ExpectedVersion 0 or replace its draft revision whole at the current root version. A root whose
// draft was discarded and that was never sealed starts the next draft; a root with a sealed
// revision takes a new draft only through Revise. A draft may omit anything; what it states must be
// well formed and its references must exist and be visible to the caller.
type SaveSupplierOffer struct {
	IdempotencyKey  string
	OfferID         string // args.id
	ExpectedVersion int64
	Data            SupplierOffer
}

type SavePurchaseTerms struct {
	IdempotencyKey  string
	TermsID         string // args.id
	ExpectedVersion int64
	Data            PurchaseTerms
}

type SaveLogisticsOffer struct {
	IdempotencyKey  string
	OfferID         string // args.id
	ExpectedVersion int64
	Data            LogisticsOffer
}

// Revise starts a new draft revision of a supply condition from the content of the sealed revision
// BaseRevision, for a recorded reason; the root has no draft. The active revision stays in force.
type Revise struct {
	IdempotencyKey  string
	Kind            string
	ID              string
	ExpectedVersion int64
	BaseRevision    int64
	Reason          string
}

// DiscardDraft ends the draft revision WorkingRevision for a recorded reason; its number is not
// reused and the active revision is untouched.
type DiscardDraft struct {
	IdempotencyKey  string
	Kind            string
	ID              string
	ExpectedVersion int64
	WorkingRevision int64
	Reason          string
}

// Seal verifies (purchase terms: publishes) the draft revision WorkingRevision on Evidence the
// human may read. SupersedesRevision must name the active revision, 0 when there is none
// (SUP-INV-001); it is superseded in the same transaction. The human may not have edited the draft
// (SUP-INV-003). The content must be complete, valid now, and its references current.
type Seal struct {
	IdempotencyKey     string
	Kind               string
	ID                 string
	ExpectedVersion    int64
	WorkingRevision    int64
	SupersedesRevision int64 // 0: the root has no active revision
	Evidence           []Evidence
}

// End withdraws (purchase terms: retires) the sealed revision TargetRevision for a recorded reason
// on evidence the human may read. Ending the active revision leaves the root without one; ending a
// superseded revision records the fact and changes nothing in force.
type End struct {
	IdempotencyKey  string
	Kind            string
	ID              string
	ExpectedVersion int64
	TargetRevision  int64
	Reason          string
	Evidence        []Evidence
}

// Validate checks the closed contract.
func (c SaveSupplierRfq) Validate() error {
	if err := head(c.IdempotencyKey, c.SupplierRfqID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	return c.Data.Validate("/args/data")
}

func (c RecordSupplierRfqIssued) Validate() error {
	return evidenced(c.IdempotencyKey, c.SupplierRfqID, c.ExpectedVersion, c.Evidence, c.Reason)
}

func (c CloseSupplierRfq) Validate() error {
	return evidenced(c.IdempotencyKey, c.SupplierRfqID, c.ExpectedVersion, c.Evidence, c.Reason)
}

func (c SaveSupplierOffer) Validate() error {
	if err := head(c.IdempotencyKey, c.OfferID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	return c.Data.Validate("/args/data")
}

func (c SavePurchaseTerms) Validate() error {
	if err := head(c.IdempotencyKey, c.TermsID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	return c.Data.Validate("/args/data")
}

func (c SaveLogisticsOffer) Validate() error {
	if err := head(c.IdempotencyKey, c.OfferID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	return c.Data.Validate("/args/data")
}

func (c Revise) Validate() error {
	if err := conditionHead(c.IdempotencyKey, c.Kind, c.ID, c.ExpectedVersion); err != nil {
		return err
	}
	if err := revision(c.BaseRevision, "/args/base_revision"); err != nil {
		return err
	}
	return reason(c.Reason)
}

func (c DiscardDraft) Validate() error {
	if err := conditionHead(c.IdempotencyKey, c.Kind, c.ID, c.ExpectedVersion); err != nil {
		return err
	}
	if err := revision(c.WorkingRevision, "/args/working_revision"); err != nil {
		return err
	}
	return reason(c.Reason)
}

func (c Seal) Validate() error {
	if err := conditionHead(c.IdempotencyKey, c.Kind, c.ID, c.ExpectedVersion); err != nil {
		return err
	}
	if err := revision(c.WorkingRevision, "/args/working_revision"); err != nil {
		return err
	}
	if c.SupersedesRevision < 0 || c.SupersedesRevision > maxVersion {
		return schema("/args/supersedes_revision")
	}
	return evidence(c.Evidence, 1, "/args/evidence")
}

func (c End) Validate() error {
	if err := conditionHead(c.IdempotencyKey, c.Kind, c.ID, c.ExpectedVersion); err != nil {
		return err
	}
	if err := revision(c.TargetRevision, "/args/target_revision"); err != nil {
		return err
	}
	if err := reason(c.Reason); err != nil {
		return err
	}
	return evidence(c.Evidence, 1, "/args/evidence")
}

// Auditor records a request an inbound adapter refused before it became a command, such as a body
// that is not valid JSON, in the context's audit trail. The principal in ctx is the requester.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Receipt is the closed success answer of a command. ObjectID is the supplier RFQ or supply
// condition ID. For a supplier RFQ RowVersion is its row version and Status its status; for a
// supply condition RowVersion is the root version and Status the status of the revision the command
// changed (draft, active, discarded, withdrawn or retired). GetCondition shows the revision numbers.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Commands is SUP's command entry point. Errors are *kernel.Rejection for refusals. An error that
// is application.ErrCommitUncertain from IDP-Bento-Go means the outcome is unknown: the caller sends
// the same command with the same idempotency key again, which returns the stored result if the
// commit happened.
type Commands interface {
	SaveSupplierRfq(ctx context.Context, cmd SaveSupplierRfq) (Receipt, error)
	RecordSupplierRfqIssued(ctx context.Context, cmd RecordSupplierRfqIssued) (Receipt, error)
	CloseSupplierRfq(ctx context.Context, cmd CloseSupplierRfq) (Receipt, error)
	SaveSupplierOffer(ctx context.Context, cmd SaveSupplierOffer) (Receipt, error)
	SavePurchaseTerms(ctx context.Context, cmd SavePurchaseTerms) (Receipt, error)
	SaveLogisticsOffer(ctx context.Context, cmd SaveLogisticsOffer) (Receipt, error)
	// Revise runs <kind>.revise.
	Revise(ctx context.Context, cmd Revise) (Receipt, error)
	// DiscardDraft runs <kind>.discard_draft.
	DiscardDraft(ctx context.Context, cmd DiscardDraft) (Receipt, error)
	// Seal runs supplier_offer.verify, purchase_terms.publish or logistics_offer.verify.
	Seal(ctx context.Context, cmd Seal) (Receipt, error)
	// End runs supplier_offer.withdraw, purchase_terms.retire or logistics_offer.withdraw.
	End(ctx context.Context, cmd End) (Receipt, error)
}

// Act is a recorded human act on a document: who, under which authority epoch, when, why and on
// which evidence.
type Act struct {
	By             kernel.ActorRef
	AuthorityEpoch int64
	At             time.Time
	Reason         string // empty for a verification
	Evidence       []Evidence
}

// SupplierRfqView is a supplier RFQ as its reader sees it. Issued is the row version the issue
// sealed; the content never changes after it.
type SupplierRfqView struct {
	SupplierRfqID string
	Status        string
	RowVersion    int64
	ContentHash   string
	Data          SupplierRfq
	SavedBy       kernel.ActorRef // the last editor
	SavedAt       time.Time
	IssuedVersion int64 // 0 before the issue
	Issue         *Act
	Close         *Act
}

// RevisionView is one revision of a supply condition; exactly the member of the condition's kind
// is set. Sealed revisions never change their content.
type RevisionView struct {
	Revision     int64
	Status       string
	BaseRevision int64  // the sealed revision a revise copied; 0 for a first draft
	ContentHash  string // SHA-256 of the revision's canonical content
	Offer        *SupplierOffer
	Terms        *PurchaseTerms
	Logistics    *LogisticsOffer
	SavedBy      kernel.ActorRef // the last editor
	SavedAt      time.Time
	Contributors []string // every actor who edited the draft; none of them may seal it
	Verification *Act     // nil before sealing
	SupersededAt time.Time
	End          *Act // the withdrawal, retirement or discard
}

// ConditionView is a supply condition and all its revisions, oldest first.
type ConditionView struct {
	Kind            string
	ID              string
	RootVersion     int64
	WorkingRevision int64 // 0: no draft
	ActiveRevision  int64 // 0: none in force
	Revisions       []RevisionView
}

// ResolvedSupplyCondition is what SUP says about one SupplyConditionRef at an instant, with the
// revision's content, for CST's evaluation (R09) and PUR's drafts (R20). Code is empty when the
// revision is in force then: active now, verified at or before the instant, its validity containing
// the instant, and, for purchase terms of dependent validity, the source offer revision in force
// too; otherwise Code is SUPPLY_CONDITION_NOT_CURRENT or SUPPLY_CONDITION_EXPIRED, as the guard
// decides. Re-check inside your transaction with SupplyConditionGuard v1.
type ResolvedSupplyCondition struct {
	Ref          SupplyConditionRef
	Status       string
	Code         string
	ContentHash  string
	ValidFrom    time.Time
	ValidUntil   time.Time
	Verification Act
	Offer        *SupplierOffer  // a supplier_offer revision
	Terms        *PurchaseTerms  // a purchase_terms revision
	Logistics    *LogisticsOffer // a logistics_offer revision
}

// Check returns nil when the revision is in force and SUP's rejection otherwise, at path of the
// caller's request.
func (r ResolvedSupplyCondition) Check(path string) error {
	if r.Code == "" {
		return nil
	}
	return Reject(r.Code, path)
}

// Queries is SUP's read side.
type Queries interface {
	// GetSupplierRfq returns a supplier RFQ the caller sees, audited as supplier_rfq.get.
	GetSupplierRfq(ctx context.Context, supplierRfqID string) (SupplierRfqView, error)
	// GetCondition returns a supply condition the caller sees and all its revisions, audited as
	// <kind>.get.
	GetCondition(ctx context.Context, kind, id string) (ConditionView, error)
	// ResolveSupplyConditions answers SupplyConditionRef v1 lookups of downstream contexts at
	// instant at. It needs no permission beyond an active grant; references that do not exist in
	// the caller's company or data scope, and drafts, are absent. Ask before opening your own
	// transaction; inside it, use SupplyConditionGuard v1.
	ResolveSupplyConditions(ctx context.Context, refs []SupplyConditionRef, at time.Time) (map[SupplyConditionRef]ResolvedSupplyCondition, error)
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

// ValidateRefs checks the references of a lookup: at most MaxLookup, each well formed, none twice.
func ValidateRefs(rs []SupplyConditionRef) error {
	if len(rs) > MaxLookup {
		return schema("/refs")
	}
	for i, r := range rs {
		if err := r.Validate("/refs/" + strconv.Itoa(i)); err != nil {
			return err
		}
		if slices.Index(rs, r) != i {
			return schema("/refs")
		}
	}
	return nil
}

func head(key, id string, expected, minVersion int64) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(id):
		return schema("/args/id")
	case expected < minVersion || expected > maxVersion:
		return schema("/args/expected_version")
	}
	return nil
}

func conditionHead(key, kind, id string, expected int64) error {
	if !kernel.ValidIdentifier(key) {
		return schema("/idempotency_key")
	}
	if !KindKnown(kind) {
		return schema("/args/kind")
	}
	return head(key, id, expected, 1)
}

func evidenced(key, id string, expected int64, ev []Evidence, why string) error {
	if err := head(key, id, expected, 1); err != nil {
		return err
	}
	if err := evidence(ev, 1, "/args/evidence"); err != nil {
		return err
	}
	return reason(why)
}

func revision(n int64, path string) error {
	if n < 1 || n > maxVersion {
		return schema(path)
	}
	return nil
}

func reason(s string) error {
	if !text(s, maxText) {
		return schema("/args/reason")
	}
	return nil
}

// evidence checks a list of evidence: min to MaxEvidence entries, each a source ID and a locator.
// The same source may be cited at several locators, but not the same locator twice.
func evidence(ev []Evidence, min int, path string) error {
	if len(ev) < min || len(ev) > MaxEvidence {
		return schema(path)
	}
	for i, e := range ev {
		p := path + "/" + strconv.Itoa(i)
		switch {
		case !kernel.ValidIdentifier(e.SourceID):
			return schema(p + "/source_id")
		case !text(e.Locator, maxText):
			return schema(p + "/locator")
		case slices.Index(ev, e) != i:
			return schema(p)
		}
	}
	return nil
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s)
}
