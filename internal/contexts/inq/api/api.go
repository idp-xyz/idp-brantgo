// Package api is the contract of the INQ bounded context (customer RFQs) for transports, the AI
// layer and the contexts downstream of INQ. It exposes no INQ domain type.
package api

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// Contract names the request and response shapes of INQ commands.
	Contract = "brant.inq/0.1.0"
	// Source identifies INQ as the producer of integration events.
	Source = "idp-brant.inq"

	EventRfqScopeConfirmed = "idp-brant.inq.rfq_scope_confirmed.v1"
	EventRfqCancelled      = "idp-brant.inq.rfq_cancelled.v1"
	EventRfqClosed         = "idp-brant.inq.rfq_closed.v1"

	CommandSaveRfq    = "rfq.save_draft"
	CommandConfirmRfq = "rfq.confirm_scope"
	CommandCancelRfq  = "rfq.cancel"
	CommandCloseRfq   = "rfq.close"

	StatusDraft     = "draft"
	StatusReady     = "ready"
	StatusCancelled = "cancelled"
	StatusClosed    = "closed"

	// UnitPCS is the only unit of S1 (Python T01).
	UnitPCS = "PCS"
	// MaxLines bounds an RFQ (resource limits: 200 RFQ lines).
	MaxLines = 200
)

// quantityPattern is the Python T01 quantity: a decimal string from 0 to 1,000,000 with at most six
// fractional digits; a known quantity must also be positive.
var quantityPattern = regexp.MustCompile(`^(?:(?:0|[1-9][0-9]{0,5})(?:\.[0-9]{1,6})?|1000000(?:\.0{1,6})?)$`)

// RfqLine is one requested item. Every field but LineID may be unknown in a draft; an empty string
// is unknown.
type RfqLine struct {
	LineID        string
	SkuID         string
	Quantity      string // decimal string, never a float
	UOM           string
	RequestedItem string // the customer's own words for an item not yet mapped to a SKU
	SourceID      string
	Locator       string // where in the source the line is, such as "第1段，第2项"
}

// SaveRfq creates a draft RFQ at ExpectedVersion 0 or replaces the whole draft at its current
// version: an omitted field clears it, so a caller sends the full current draft (not a patch).
// Nil and empty slices are different requests for idempotency.
type SaveRfq struct {
	IdempotencyKey   string
	RfqID            string
	ExpectedVersion  int64
	CustomerID       string
	ContactID        string
	Lines            []RfqLine
	SourceIDs        []string
	UnresolvedFields []string
}

// ConfirmRfqScope makes a complete draft ready. Only a human confirms, and the evidence must cover
// every source the RFQ cites (INQ-INV-001).
type ConfirmRfqScope struct {
	IdempotencyKey  string
	RfqID           string
	ExpectedVersion int64
	EvidenceIDs     []string
}

// CancelRfq cancels a draft or ready RFQ for a recorded reason; lines and sources stay.
type CancelRfq struct {
	IdempotencyKey  string
	RfqID           string
	ExpectedVersion int64
	Reason          string
}

// CloseRfq closes a ready RFQ for a recorded reason.
type CloseRfq struct {
	IdempotencyKey  string
	RfqID           string
	ExpectedVersion int64
	Reason          string
}

// Validate checks the closed contract (Python BC_T01_commands.schema.json, rfq.save_draft).
func (c SaveRfq) Validate() error {
	if err := head(c.IdempotencyKey, c.RfqID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	if c.CustomerID != "" && !kernel.ValidIdentifier(c.CustomerID) {
		return schema("/args/customer_id")
	}
	if c.ContactID != "" && !kernel.ValidIdentifier(c.ContactID) {
		return schema("/args/contact_id")
	}
	if len(c.Lines) > MaxLines {
		return schema("/args/lines")
	}
	for i, l := range c.Lines {
		p := "/args/lines/" + strconv.Itoa(i)
		switch {
		case !kernel.ValidIdentifier(l.LineID):
			return schema(p + "/line_id")
		case l.SkuID != "" && !kernel.ValidIdentifier(l.SkuID):
			return schema(p + "/sku_id")
		case l.Quantity != "" && !quantityPattern.MatchString(l.Quantity):
			return schema(p + "/quantity")
		case l.UOM != "" && l.UOM != UnitPCS:
			return schema(p + "/uom")
		case l.RequestedItem != "" && !text(l.RequestedItem, 200):
			return schema(p + "/requested_item")
		case l.SourceID != "" && !kernel.ValidIdentifier(l.SourceID):
			return schema(p + "/source_id")
		case l.Locator != "" && !text(l.Locator, 200):
			return schema(p + "/locator")
		}
	}
	if err := uniqueIDs(c.SourceIDs, 0, "/args/source_ids"); err != nil {
		return err
	}
	if len(c.UnresolvedFields) > 200 {
		return schema("/args/unresolved_fields")
	}
	for i, f := range c.UnresolvedFields {
		if !text(f, 200) || slices.Index(c.UnresolvedFields, f) != i {
			return schema("/args/unresolved_fields")
		}
	}
	return nil
}

func (c ConfirmRfqScope) Validate() error {
	if err := head(c.IdempotencyKey, c.RfqID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	return uniqueIDs(c.EvidenceIDs, 1, "/args/evidence_ids")
}

func (c CancelRfq) Validate() error {
	return reasoned(c.IdempotencyKey, c.RfqID, c.ExpectedVersion, c.Reason)
}

func (c CloseRfq) Validate() error {
	return reasoned(c.IdempotencyKey, c.RfqID, c.ExpectedVersion, c.Reason)
}

// Auditor records a request an inbound adapter refused before it became a command, such as a body
// that is not valid JSON, in the context's audit trail. The principal in ctx is the requester.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Receipt is the closed success answer of a command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Commands is INQ's command entry point. Errors are *kernel.Rejection for refusals. An error that
// is application.ErrCommitUncertain from IDP-Bento-Go means the outcome is unknown: the caller
// reconciles (Reconciler) or sends the same command with the same idempotency key again.
type Commands interface {
	SaveRfq(ctx context.Context, cmd SaveRfq) (Receipt, error)
	ConfirmRfqScope(ctx context.Context, cmd ConfirmRfqScope) (Receipt, error)
	CancelRfq(ctx context.Context, cmd CancelRfq) (Receipt, error)
	CloseRfq(ctx context.Context, cmd CloseRfq) (Receipt, error)
}

// Command is one of INQ's commands: SaveRfq, ConfirmRfqScope, CancelRfq or CloseRfq.
type Command interface{ inqCommand() }

func (SaveRfq) inqCommand()         {}
func (ConfirmRfqScope) inqCommand() {}
func (CancelRfq) inqCommand()       {}
func (CloseRfq) inqCommand()        {}

// Reconciliation is what became of a command whose answer the caller lost.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string   // committed and payload_conflict
	ObjectID string   // committed and payload_conflict
	Receipt  *Receipt // the stored receipt; committed only
}

// Reconciler recovers from an unknown outcome without executing anything (contract 06, 动作对账).
// The caller passes the command exactly as it sent it, idempotency key included; INQ hashes it as
// the command did. Reconciling needs the read permission of RFQs, not the command's own, and
// re-checks it and the caller's data scope now: PERMISSION_DENIED and NOT_FOUND as for a read.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}

// RfqLineView is a stored line; SkuSpecRevision is the SKU revision the confirmation saw (0 before).
type RfqLineView struct {
	RfqLine
	SkuSpecRevision int64
}

// Confirmation is what a ready RFQ was confirmed against: the upstream versions and the evidence.
type Confirmation struct {
	CustomerRowVersion int64
	ContactRowVersion  int64
	EvidenceIDs        []string
	ConfirmedBy        kernel.ActorRef
	AuthorityEpoch     int64
}

// RfqView is an RFQ as its reader sees it. SourceIDs are every source the RFQ uses: its own
// source list, the lines' sources and the confirmation evidence.
type RfqView struct {
	RfqID            string
	Status           string
	RowVersion       int64
	CustomerID       string
	ContactID        string
	Lines            []RfqLineView
	SourceIDs        []string
	UnresolvedFields []string
	Confirmation     *Confirmation
}

// Queries is INQ's read side.
type Queries interface {
	GetRfq(ctx context.Context, rfqID string) (RfqView, error)
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

// RfqLineRef is RfqLineRef v1 (Context Map R04, T02 §13.1): one line of a customer RFQ at the
// RFQ's row version. Lines have no versions of their own; the RFQ's row version versions them all.
type RfqLineRef struct {
	RfqID      string
	RowVersion int64
	LineID     string
}

// ResolvedRfq is what INQ says about an RFQ now: its state, its row version and its lines with the
// SKU specification revisions a confirmation fixed. A ready RFQ at the row version a downstream fact
// names is current; any other state or row version is not.
type ResolvedRfq struct {
	RfqID      string
	Status     string
	RowVersion int64
	CustomerID string
	Lines      []RfqLineView
}

// MaxLookup bounds the RFQs of one lookup.
const MaxLookup = 200

// RfqLookup answers RfqLineRef v1 lookups of the contexts downstream of INQ (SUP, R04).
type RfqLookup interface {
	// ResolveRfqs returns, among ids, the RFQs of the caller's company inside the caller's data
	// scope; others are absent. It needs no permission beyond an active grant. Ask before opening
	// your own transaction: INQ answers in a transaction of its own.
	ResolveRfqs(ctx context.Context, ids []string) (map[string]ResolvedRfq, error)
}

func head(key, id string, expected, minVersion int64) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(id):
		return schema("/args/id")
	case expected < minVersion || expected > 2147483647:
		return schema("/args/expected_version")
	}
	return nil
}

func reasoned(key, id string, expected int64, reason string) error {
	if err := head(key, id, expected, 1); err != nil {
		return err
	}
	if !text(reason, 1000) {
		return schema("/args/reason")
	}
	return nil
}

func uniqueIDs(ids []string, min int, path string) error {
	if len(ids) < min || len(ids) > 200 {
		return schema(path)
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) || slices.Index(ids, id) != i {
			return schema(path)
		}
	}
	return nil
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s)
}
