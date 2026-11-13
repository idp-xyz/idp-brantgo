// Package api is the contract of the INQ bounded context (customer RFQs) for transports, the AI
// layer and the contexts downstream of INQ. It exposes no INQ domain type.
package api

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"time"
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

	// CodeSourceAmbiguous: a field of a line still fits several values or places of its source
	// (error catalog SOURCE_AMBIGUOUS: 等待确认或更正); rfq.confirm_scope waits for the human to
	// state the value and, for several places, the place verified.
	CodeSourceAmbiguous = "SOURCE_AMBIGUOUS"
)

// The fields of a line that field locators, pending fields and verifications name (core design
// 5.2 RFQLine: requested item, matched SKU, quantity, unit).
const (
	FieldRequestedItem = "requested_item"
	FieldSkuID         = "sku_id"
	FieldQuantity      = "quantity"
	FieldUOM           = "uom"
)

// Fields lists the fields a field locator, a pending field or a verification may name.
var Fields = []string{FieldRequestedItem, FieldSkuID, FieldQuantity, FieldUOM}

// Why a field of a draft line is pending (AN01: 标记候选和缺项，等待用户补充). The human settles each
// pending field on rfq.confirm_scope; the scope is not confirmed before.
const (
	PendingAmbiguous = "ambiguous" // several values or places fit (SOURCE_AMBIGUOUS until settled)
	PendingConflict  = "conflict"  // the source contradicts itself (BC-AT05, CT03: matrix 120, total 118)
	PendingMissing   = "missing"   // the source gives no value (AN01: no unit given)
	PendingUnlocated = "unlocated" // the value stands nowhere in the source
	PendingNoted     = "noted"     // the value's place carries a reader's note: a formula's cached value, a hidden row or column, invisible text
)

// PendingReasons lists the reasons a field may be pending for.
var PendingReasons = []string{PendingAmbiguous, PendingConflict, PendingMissing, PendingUnlocated, PendingNoted}

// Bounds of the structured parts of a line.
const (
	MaxLineLocators = 16 // field locators of one line
	MaxCandidates   = 10 // candidate values of one pending field
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
	// Locators place the line's fields in its source, a file extract (WBS S4-04b; BC-AT27 字段定位):
	// the field, the place in the notation of the extract's reader and the source's own words there
	// (原始需求值; the line's field holds the verified structured value, core design 5.2). Several
	// locators of one field are alternative places. Left out of the request hash while empty.
	Locators []FieldLocator `json:",omitempty"`
	// Pending are the fields the draft leaves for the human to settle on rfq.confirm_scope.
	Pending []PendingField `json:",omitempty"`
}

// FieldLocator places one field of a line in the line's source.
type FieldLocator struct {
	Field   string // requested_item, sku_id, quantity or uom
	Locator string // text "line:col", PDF "page:line", XLSX "Sheet!B3" (the extractor version of the source defines it)
	Quote   string // the source's own words at the locator
}

// PendingField is a field the source does not settle.
type PendingField struct {
	Field      string
	Reason     string   // ambiguous, conflict, missing, unlocated or noted
	Candidates []string `json:",omitempty"` // values the field could take, such as two SKUs for one customer style (AN01)
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
//
// VerifiedLines are the human's line-by-line verification (core design: rfq.confirm_scope takes the
// verified fields and forms the clarification results): for a line, the fields whose value the
// human states, and for a field placed in several places, the locator verified. When any line of
// the draft was proposed by an AI actor, every line must be verified (SD-07: AI output is a
// candidate). A pending field must be verified; each verified field is recorded as a correction.
// Left out of the request hash while empty.
type ConfirmRfqScope struct {
	IdempotencyKey  string
	RfqID           string
	ExpectedVersion int64
	EvidenceIDs     []string
	VerifiedLines   []LineVerification `json:",omitempty"`
}

// LineVerification is the human's verification of one draft line; no fields verifies the line as
// it is.
type LineVerification struct {
	LineID string
	Fields []VerifiedField `json:",omitempty"`
}

// VerifiedField states a field's verified value and, optionally, the one of the field's locators
// the value was verified at.
type VerifiedField struct {
	Field   string
	Value   string
	Locator string `json:",omitempty"`
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
		if err := l.validateStructure(p); err != nil {
			return err
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

// validateStructure checks a line's field locators and pending fields.
func (l RfqLine) validateStructure(p string) error {
	if len(l.Locators) > MaxLineLocators {
		return schema(p + "/locators")
	}
	for j, x := range l.Locators {
		q := p + "/locators/" + strconv.Itoa(j)
		switch {
		case !slices.Contains(Fields, x.Field):
			return schema(q + "/field")
		case !text(x.Locator, 200):
			return schema(q + "/locator")
		case !text(x.Quote, 200):
			return schema(q + "/quote")
		case l.SourceID == "":
			return schema(p + "/source_id") // a locator points into the line's source
		}
		for _, y := range l.Locators[:j] {
			if y.Field == x.Field && y.Locator == x.Locator {
				return schema(q + "/locator")
			}
		}
	}
	if len(l.Pending) > len(Fields) {
		return schema(p + "/pending")
	}
	for j, x := range l.Pending {
		q := p + "/pending/" + strconv.Itoa(j)
		switch {
		case !slices.Contains(Fields, x.Field) || slices.IndexFunc(l.Pending[:j], func(y PendingField) bool { return y.Field == x.Field }) >= 0:
			return schema(q + "/field")
		case !slices.Contains(PendingReasons, x.Reason):
			return schema(q + "/reason")
		case len(x.Candidates) > MaxCandidates:
			return schema(q + "/candidates")
		}
		for k, c := range x.Candidates {
			if !text(c, 200) || slices.Index(x.Candidates, c) != k {
				return schema(q + "/candidates/" + strconv.Itoa(k))
			}
		}
	}
	return nil
}

func (c ConfirmRfqScope) Validate() error {
	if err := head(c.IdempotencyKey, c.RfqID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if err := uniqueIDs(c.EvidenceIDs, 1, "/args/evidence_ids"); err != nil {
		return err
	}
	if len(c.VerifiedLines) > MaxLines {
		return schema("/args/verified_lines")
	}
	for k, v := range c.VerifiedLines {
		p := "/args/verified_lines/" + strconv.Itoa(k)
		if !kernel.ValidIdentifier(v.LineID) || slices.IndexFunc(c.VerifiedLines[:k], func(w LineVerification) bool { return w.LineID == v.LineID }) >= 0 {
			return schema(p + "/line_id")
		}
		if len(v.Fields) > len(Fields) {
			return schema(p + "/fields")
		}
		for j, f := range v.Fields {
			q := p + "/fields/" + strconv.Itoa(j)
			if !slices.Contains(Fields, f.Field) || slices.IndexFunc(v.Fields[:j], func(g VerifiedField) bool { return g.Field == f.Field }) >= 0 {
				return schema(q + "/field")
			}
			if !validValue(f.Field, f.Value) {
				return schema(q + "/value")
			}
			if f.Locator != "" && !text(f.Locator, 200) {
				return schema(q + "/locator")
			}
		}
	}
	return nil
}

// validValue reports whether value has the shape of field's values; whether it is a valid known
// value (a positive quantity, a confirmed SKU) is the domain's decision.
func validValue(field, value string) bool {
	switch field {
	case FieldSkuID:
		return kernel.ValidIdentifier(value)
	case FieldQuantity:
		return quantityPattern.MatchString(value)
	case FieldUOM:
		return value == UnitPCS
	case FieldRequestedItem:
		return text(value, 200)
	}
	return false
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

// ActionReconciler is INQ's ActionReconciliation v1 (Context Map R67 INQ→TSK, S4 decision D-11):
// reconciliation by the ledger identity of a command, for a caller that kept the request rather
// than the command (TSK's ActionExecution, which records it before the command is sent).
//
// RequestOf returns the command name, idempotency key and request hash INQ's ledger records for cmd
// — hashed exactly as executing it hashes it — and executes and reads nothing; a command INQ does
// not know is SCHEMA_INVALID. ReconcileHash answers what Reconcile answers for the command that
// hashes to requestHash: committed with the stored receipt, payload_conflict or not_committed,
// after the same checks (inq.read and the caller's data scope now, X-INV-003), audited the same way
// (action.reconcile), executing nothing. An unknown command name, an invalid key or a request hash
// that is not 64 lower-case hexadecimal digits is SCHEMA_INVALID.
type ActionReconciler interface {
	RequestOf(cmd Command) (kernel.ActionRequest, error)
	ReconcileHash(ctx context.Context, command, idempotencyKey, requestHash string) (Reconciliation, error)
}

// RfqLineView is a stored line; SkuSpecRevision is the SKU revision the confirmation saw (0 before).
type RfqLineView struct {
	RfqLine
	SkuSpecRevision int64
	// ProposedBy is the actor whose save last changed the line: a line an AI actor proposed is a
	// candidate (SD-07) until a human verifies it. Zero for lines saved before S4.
	ProposedBy kernel.ActorRef
	// QuotesWithheld: the reader cannot read the line's source now, so the quotes of its locators
	// are left out (CT16: a reference never bypasses current access).
	QuotesWithheld bool
}

// Correction records a human's correction of a field an AI actor proposed, or the human's
// verification of a pending field (INQ Clarification; BC-AT27 逐行修正; core design 5.2: the
// original and the verified values are kept apart). Corrections are never changed or removed.
type Correction struct {
	Seq         int64
	RowVersion  int64  // the RFQ row version the correction produced
	Command     string // rfq.save_draft or rfq.confirm_scope
	LineID      string
	Field       string // requested_item, sku_id, quantity, uom, source_id or locator; "" for a removed line
	Kind        string // changed, verified, relocated or removed
	From        string // the value before ("" unknown)
	To          string // the value after
	Locator     string // the place the human verified the value at
	Reason      string // the pending reason the correction settled
	Candidates  []string
	ProposedBy  kernel.ActorRef // who proposed the corrected value
	CorrectedBy kernel.ActorRef
	RecordedAt  time.Time
}

// Kinds of corrections.
const (
	CorrectionChanged   = "changed"   // the value was changed
	CorrectionVerified  = "verified"  // the value was kept and verified, settling a pending field
	CorrectionRelocated = "relocated" // the value was kept, its place in the source changed
	CorrectionRemoved   = "removed"   // a line an AI actor proposed was removed
)

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
	// Corrections are every human correction and verification of the RFQ's lines, in order.
	Corrections []Correction
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

// MaxLookup bounds the RFQs of one lookup.
const MaxLookup = 200

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
