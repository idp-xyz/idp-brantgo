// Package api is PUR's published contract (procurement and supply allocation): purchase drafts and
// the demand allocation of active order lines (WBS S3-10, S3-16; tactical model PUR-AGG-PurchaseDraft
// and PUR-AGG-DemandAllocation, SM-PUR-DRAFT, PUR-INV-001 to 007; Python t04_purchase, T04 A46),
// purchase commitments with supplier confirmations (WBS S5-01, commitment.go; PUR-INV-009, 010) and
// supplier deliveries on the direct delivery route (WBS S5-15, delivery.go; PUR-INV-008),
// commitment changes and cancellation (WBS S5-19, change.go; PUR-INV-011) and the allocation reviews
// that follow an order change (WBS S5-09b, review.go; SM-PUR-ALLOCATION-REVIEW).
//
//	purchase.draft.prepare  human only: binds lines of an active order, whose activating order
//	                        approval is not revoked, each with an explicitly allocated quantity, to
//	                        an exactly chosen condition line of a sealed supplier offer or purchase
//	                        terms revision in force (SUP), with a selection reason, and stores the
//	                        server's calculation as an immutable, hashed draft. One draft has one
//	                        supplier and one currency, payment and trade terms. An order line may be
//	                        split across drafts of several suppliers; their effective allocations
//	                        never exceed the line's quantity (PURCHASE_ALLOCATION_EXCEEDED). What MOQ,
//	                        order multiple or loss make a draft buy beyond its allocation is an
//	                        overbuy: marked with its quantity and reason, kept out of the cost of
//	                        goods sold, and approved by the purchase commitment decision (S5 decision
//	                        D-14). The draft is never a commitment (not_committed when prepared).
//	purchase.draft.void     human only: voids a prepared (or supply_expired) draft not committed to
//	                        the supplier — no purchase commitment of it was issued
//	                        (PURCHASE_DRAFT_COMMITTED) — with a reason; its allocations are released,
//	                        the record stays.
//
// Every draft binds the order at its order revision and order hash (TRD ActiveOrderRef v1, R19),
// the supply condition revisions with their content hashes (SUP SupplyConditionRef v1, R20), the
// supplier at the row version they bind (PTY PartyRef v1, R22), the order lines' specification
// revisions (CAT SkuRef v1, R52), the POL configurations it uses (ConfigRef v1, R48) and the cost
// snapshot of the order's quote, compared line by line for review only (CST CostSnapshotRef v1,
// R21). Each is read before the transaction and re-checked inside it, after PUR locked its own
// roots, through the owner's guard function (ADR-BR-004 option B, ADR-BR-011).
//
// Request members in snake_case: purchase.draft.prepare {draft_id, order_id, lines [{order_line_id,
// quantity, source {kind (supplier_offer or purchase_terms), id, revision, condition_line_id},
// overbuy {quantity, reason}}], selection_reason}; purchase.draft.void {draft_id, expected_version,
// reason}.
package api

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Source is PUR's event source.
const Source = "idp-brant.pur"

// Contract names PUR's command contract, the envelope's contract member on the command routes
// POST /api/brant/v1/pur/commands/<command> (command catalog V0.1 PUR rows).
const Contract = "brant.purchasing/0.1.0"

// Commands, reads, permissions and events (command catalog V0.1 PUR rows, event catalog V0.1).
const (
	CommandPrepareDraft = "purchase.draft.prepare"
	CommandVoidDraft    = "purchase.draft.void"

	// Reads: a draft (Python purchase.draft.get), an order's demand allocation and the audit trail.
	CommandGetDraft       = "purchase.draft.get"
	CommandGetAllocations = "demand_allocation.get"
	CommandAudit          = "pur.audit"
	PermissionRead        = "pur.read"
	PermissionAudit       = "pur.audit"

	EventPurchaseDraftPrepared = "idp-brant.pur.purchase_draft_prepared.v1"
	EventPurchaseDraftVoided   = "idp-brant.pur.purchase_draft_voided.v1"

	// Kinds of supply condition a line buys under (SUP's SupplyConditionRef v1).
	KindSupplierOffer = "supplier_offer"
	KindPurchaseTerms = "purchase_terms"

	// States of a draft (SM-PUR-DRAFT). StatusSupplyExpired is derived when read.
	StatusPrepared      = "prepared"
	StatusSupplyExpired = "supply_expired"
	StatusVoided        = "voided"

	// CommitmentNotCommitted is the commitment state of a draft no purchase commitment was issued
	// on (CommitmentCommitted otherwise, derived when read).
	CommitmentNotCommitted = "not_committed"

	// DocumentContract names a draft's body.
	DocumentContract = "brant.purchase-draft/0.2.0"

	MaxLines   = 200
	MaxReason  = 1000
	maxVersion = 2147483647
)

// SourceRef is the condition line a draft line buys under: line ConditionLineID of revision Revision
// of supply condition ID of kind Kind.
type SourceRef struct {
	Kind            string
	ID              string
	Revision        int64
	ConditionLineID string
}

// Overbuy is what a line buys beyond its allocation, explicitly marked: a positive Quantity in the
// order line's unit and its Reason (MOQ, order multiple or loss).
type Overbuy struct {
	Quantity string
	Reason   string
}

// DraftLine is one line of a draft: Quantity of order line OrderLineID (kernel.QuantityFormat,
// positive, in the order line's unit) allocated to the draft and bought under Source, and an
// optional Overbuy.
type DraftLine struct {
	OrderLineID string
	Quantity    string
	Source      SourceRef
	Overbuy     *Overbuy
}

// PrepareDraft prepares purchase draft DraftID from order OrderID (Python
// t04_purchase_prepare_draft, with an allocated quantity per line). Human only.
type PrepareDraft struct {
	IdempotencyKey  string
	DraftID         string
	OrderID         string
	Lines           []DraftLine
	SelectionReason string
}

// VoidDraft voids draft DraftID at ExpectedVersion for Reason. Human only.
type VoidDraft struct {
	IdempotencyKey  string
	DraftID         string
	ExpectedVersion int64
	Reason          string
}

// Command is one of PUR's commands: PrepareDraft, VoidDraft, the commitment commands and the
// supplier delivery commands.
type Command interface{ purCommand() }

func (PrepareDraft) purCommand() {}
func (VoidDraft) purCommand()    {}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

// Reason reports whether s is a reason: 1 to MaxReason characters of valid UTF-8, not only white
// space, without control characters (Python _request).
func Reason(s string) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= MaxReason && utf8.ValidString(s) && strings.TrimSpace(s) != "" &&
		strings.IndexFunc(s, unicode.IsControl) < 0
}

func quantity(s, path string) error {
	q, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil || q.Sign() <= 0 {
		return schema(path)
	}
	return nil
}

// Validate checks the closed contract (Python _request): identifiers, 1 to 200 lines with a
// positive quantity and a complete source each, an order line at most once (DUPLICATE_LINE_ID), a
// positive overbuy quantity, and a selection reason. An overbuy's missing reason is the rules'
// OVERBUY_NOT_APPROVED, not a structural error.
func (c PrepareDraft) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DraftID):
		return schema("/args/draft_id")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case len(c.Lines) < 1 || len(c.Lines) > MaxLines:
		return schema("/args/lines")
	}
	seen := map[string]bool{}
	for i, l := range c.Lines {
		p := "/args/lines/" + strconv.Itoa(i)
		s := l.Source
		switch {
		case !kernel.ValidIdentifier(l.OrderLineID):
			return schema(p + "/order_line_id")
		case quantity(l.Quantity, p+"/quantity") != nil:
			return schema(p + "/quantity")
		case s.Kind != KindSupplierOffer && s.Kind != KindPurchaseTerms:
			return schema(p + "/source/kind")
		case !kernel.ValidIdentifier(s.ID):
			return schema(p + "/source/id")
		case s.Revision < 1 || s.Revision > maxVersion:
			return schema(p + "/source/revision")
		case !kernel.ValidIdentifier(s.ConditionLineID):
			return schema(p + "/source/condition_line_id")
		}
		if o := l.Overbuy; o != nil {
			if err := quantity(o.Quantity, p+"/overbuy/quantity"); err != nil {
				return err
			}
			if o.Reason != "" && !Reason(o.Reason) {
				return schema(p + "/overbuy/reason")
			}
		}
		if seen[l.OrderLineID] {
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/order_line_id", "")
		}
		seen[l.OrderLineID] = true
	}
	if !Reason(c.SelectionReason) {
		return schema("/args/selection_reason")
	}
	return nil
}

// Validate checks the closed contract.
func (c VoidDraft) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DraftID):
		return schema("/args/draft_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return nil
}

// Receipt is the closed success answer of a command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// DraftReceipt is the answer of both commands: the receipt (ObjectID the draft ID, RowVersion the
// draft's row version, 1 prepared and 2 voided, Status prepared or voided) and what the draft binds:
// the order at its revision and hash, the supplier, the currency, the draft hash, the commitment
// state, the end of its supply's validity and its totals in minor units. A replay, and a prepare
// answered from the stored draft (the same request by the same preparer under another key), are
// Replayed and rebuilt from the stored draft.
type DraftReceipt struct {
	Receipt
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	SupplierID       string
	Currency         string
	DraftHash        string
	CommitmentState  string
	SupplyValidUntil time.Time
	NetMinor         string
	TaxMinor         string
	GrossMinor       string
}

// LineView is one line of a draft as the reads show it, from its document.
type LineView struct {
	OrderLineID       string
	RfqLineID         string
	SkuID             string
	SpecRevision      int64
	UnitID            string
	OrderQuantity     string
	AllocatedQuantity string
	OverbuyQuantity   string
	PurchaseQuantity  string
	OverbuyReason     string
	Source            SourceRef
	PriceGroupID      string
	NetMinor          string
	TaxMinor          string
	GrossMinor        string
	CostMinor         string
	SoldCostMinor     string
	SupplierDate      string
	RequiredDate      string
	SlackDays         int
	SameSource        bool
	CostVarianceMinor string // "" when the currencies differ
}

// DraftView is a draft as the reads show it: what it binds, its state now (prepared,
// supply_expired after its supply's validity ended, or voided), its commitment state now
// (not_committed, or committed once a purchase commitment of it was issued) and the commitments of it
// the reader sees, its document (the exact bytes DraftHash covers, contract DocumentContract) with
// its lines and totals, who prepared it and how it ended. CheckedAt is when the read decided the
// state.
type DraftView struct {
	DraftID          string
	Status           string
	State            string
	RowVersion       int64
	CommitmentState  string
	CommitmentIDs    []string
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	SupplierID       string
	Currency         string
	DraftHash        string
	Document         string
	Lines            []LineView
	NetMinor         string
	TaxMinor         string
	GrossMinor       string
	SelectionReason  string
	SupplyValidUntil time.Time
	PreparedBy       kernel.ActorRef
	PreparedAt       time.Time
	AuthorityEpoch   int64
	VoidedBy         kernel.ActorRef
	VoidedAt         time.Time
	VoidReason       string
	VoidEpoch        int64
	CheckedAt        time.Time
}

// AllocationEntryView is one draft's share of an order line. DraftID is empty for a draft the
// reader does not see; the quantity still counts.
type AllocationEntryView struct {
	DraftID    string
	Quantity   string
	Effective  bool
	ReleasedAt time.Time
}

// AllocationView is the DemandAllocation of one order line: the demand at the order revision it is
// bound to, in the line's unit, what is allocated and what remains, and the entries.
type AllocationView struct {
	OrderID       string
	OrderLineID   string
	OrderRevision int64
	UnitID        string
	UnitRevision  int64
	Demand        string
	Allocated     string
	Remaining     string
	RowVersion    int64
	Entries       []AllocationEntryView
}

// Commands is PUR's command entry point. Errors are *kernel.Rejection for refusals. An error that
// is application.ErrCommitUncertain from IDP-Bento-Go means the outcome is unknown: the caller sends
// the same command with the same idempotency key again, or reconciles it.
type Commands interface {
	PrepareDraft(ctx context.Context, cmd PrepareDraft) (DraftReceipt, error)
	VoidDraft(ctx context.Context, cmd VoidDraft) (DraftReceipt, error)
	CommitmentCommands
	DeliveryCommands
	ChangeCommands
	ReviewCommands
}

// Queries is PUR's audited read side.
type Queries interface {
	// GetDraft returns a draft the caller sees, audited as purchase.draft.get (pur.read): NOT_FOUND
	// otherwise. It re-checks now that the record is intact and that the caller still reads the
	// order and every supply condition it binds (REFERENCE_INVALID otherwise, Python _load).
	GetDraft(ctx context.Context, draftID string) (DraftView, error)
	// GetAllocations returns the demand allocation of every line of an order the caller sees,
	// ordered by line, audited as demand_allocation.get (pur.read); lines never drafted are absent.
	GetAllocations(ctx context.Context, orderID string) ([]AllocationView, error)
	// Audit returns PUR's audit trail (pur.audit).
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
	CommitmentQueries
	DeliveryQueries
	ChangeQueries
	ReviewQueries
}

// Auditor records a request an inbound adapter refused before it became a command.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Reconciliation is what became of a command whose answer the caller lost.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string
	ObjectID string
	Receipt  *Receipt
}

// Reconciler recovers from an unknown outcome without executing anything (contract 06, 动作对账).
// Reconciling needs pur.read, not the command's own permission.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}
