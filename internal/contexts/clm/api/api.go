// Package api is CLM's published contract (after-sales claims, WBS S6-03; tactical model
// CLM-AGG-ClaimCase with ReturnRecord and Remedy; SM-CLM-CLAIM; CLM-INV-001..003; Context Map
// R34 ShipmentLineRef v1, R35 CreditRequest v1; S6-D03). Python V0.50 has no claims context.
//
//	claim.open        human or AI (sales clerk, merchandiser): opens a claim on a confirmed
//	                  shipment line. An unconfirmed or cancelled line is INVALID_STATE; an
//	                  unknown shipment or line is REFERENCE_INVALID. The shipment is not changed
//	                  (CLM-INV-001). Evidence IDs, when cited, are read from EVD before the
//	                  transaction. Emits ClaimOpened.
//	return.register   human or AI: records a returned quantity in the shipment line's unit.
//	                  Across every claim of that line that is not rejected, the sum of registered
//	                  quantities is at most the confirmed shipped quantity; one more is
//	                  CLAIM_RETURN_EXCEEDS_SHIPPED (CLM-INV-002). The return is a CLM fact only.
//	                  Emits ReturnRegistered.
//	remedy.decide     human only, not the actor who opened the claim
//	                  (INDEPENDENT_REVIEW_REQUIRED): one remedy. credit_note and refund take a
//	                  positive amount_minor in the shipment currency. reject, reshipment_ref and
//	                  rework_ref take no amount. Nothing is posted to STL. Emits RemedyDecided.
//	claim.close       human only, not the opener, and only after a remedy (a rejection included):
//	                  records a closure summary. Emits ClaimClosed.
//
// The claim's state is open or closed (SM-CLM-CLAIM). Opening, registering a return and deciding
// a remedy all leave it open.
//
// Request members: claim.open {claim_id, order_id, shipment_id, shipment_line_id, reason,
// evidence_ids}; return.register {claim_id, return_id, quantity, condition, note, evidence_ids};
// remedy.decide {claim_id, remedy_id, kind, amount_minor, reason, expected_version};
// claim.close {claim_id, expected_version, summary}. amount_minor is an integer string of minor
// units. The currency is the shipment line's, never a request field.
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

// Source is CLM's event source.
const Source = "idp-brant.clm"

// Contract names CLM's command contract on POST /api/brant/v1/clm/commands/<command>.
const Contract = "brant.claims/0.1.0"

// Commands, reads, permissions and events (Context Map CLM, SM-CLM-CLAIM).
const (
	CommandOpenClaim      = "claim.open"
	CommandRegisterReturn = "return.register"
	CommandDecideRemedy   = "remedy.decide"
	CommandCloseClaim     = "claim.close"
	CommandGetClaim       = "claim.get"
	CommandAudit          = "clm.audit"
	PermissionRead        = "clm.read"
	PermissionAudit       = "clm.audit"

	EventClaimOpened      = "idp-brant.clm.claim_opened.v1"
	EventReturnRegistered = "idp-brant.clm.return_registered.v1"
	EventRemedyDecided    = "idp-brant.clm.remedy_decided.v1"
	EventClaimClosed      = "idp-brant.clm.claim_closed.v1"

	// States (SM-CLM-CLAIM).
	StatusOpen   = "open"
	StatusClosed = "closed"

	// Remedy kinds.
	KindCreditNote = "credit_note"
	KindRefund     = "refund"
	KindReject     = "reject"
	KindReshipment = "reshipment_ref"
	KindRework     = "rework_ref"

	MaxReason    = 1000
	MaxCondition = 200
	MaxNote      = 1000
	MaxEvidence  = 20
	maxVersion   = 2147483647
)

// OpenClaim opens claim ClaimID on shipment line ShipmentLineID of shipment ShipmentID, which
// fulfils OrderID, for Reason. EvidenceIDs are EVD sources (none when the claim cites none).
// Human or AI.
type OpenClaim struct {
	IdempotencyKey string
	ClaimID        string
	OrderID        string
	ShipmentID     string
	ShipmentLineID string
	Reason         string
	EvidenceIDs    []string
}

// RegisterReturn registers return ReturnID of Quantity (kernel.QuantityFormat, above zero) on
// claim ClaimID, in Condition, with Note. Human or AI.
type RegisterReturn struct {
	IdempotencyKey string
	ClaimID        string
	ReturnID       string
	Quantity       string
	Condition      string
	Note           string
	EvidenceIDs    []string
}

// DecideRemedy decides remedy RemedyID of claim ClaimID at ExpectedVersion. RemedyKind is
// credit_note, refund, reject, reshipment_ref or rework_ref. AmountMinor is required and above
// zero for a credit note or a refund, and omitted or "0" otherwise, in the shipment currency's
// minor units. Human only, and not the opener.
type DecideRemedy struct {
	IdempotencyKey  string
	ClaimID         string
	RemedyID        string
	RemedyKind      string `json:"kind"`
	AmountMinor     string
	Reason          string
	ExpectedVersion int64
}

// CloseClaim closes claim ClaimID at ExpectedVersion with Summary. Human only, not the opener,
// and only once a remedy has been decided.
type CloseClaim struct {
	IdempotencyKey  string
	ClaimID         string
	ExpectedVersion int64
	Summary         string
}

// Command is one of CLM's commands.
type Command interface{ clmCommand() }

func (OpenClaim) clmCommand()      {}
func (RegisterReturn) clmCommand() {}
func (DecideRemedy) clmCommand()   {}
func (CloseClaim) clmCommand()     {}

// Receipt is the closed success answer of a command. ObjectID is the claim. Status is open or
// closed. A command answered from a stored result is Replayed.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// ReturnView is one registered return.
type ReturnView struct {
	ReturnID     string
	Quantity     string
	Condition    string
	Note         string
	EvidenceIDs  []string
	RegisteredBy kernel.ActorRef
	RegisteredAt time.Time
}

// RemedyView is the claim's remedy. AmountMinor is an integer string; zero for a kind that moves
// no money.
type RemedyView struct {
	RemedyID    string
	Kind        string
	AmountMinor string
	Currency    string
	Reason      string
	DecidedBy   kernel.ActorRef
	DecidedAt   time.Time
}

// ClaimView is a claim as claim.get shows it. There is no HTTP route for this read: the Context
// Map lists no CLM query.
type ClaimView struct {
	ClaimID         string
	Status          string
	RowVersion      int64
	OrderID         string
	CustomerID      string
	ShipmentID      string
	ShipmentLineID  string
	ShippedQuantity string
	UnitID          string
	Currency        string
	MinorUnits      int
	Reason          string
	EvidenceIDs     []string
	OpenedBy        kernel.ActorRef
	OpenedAt        time.Time
	Returns         []ReturnView
	Remedy          *RemedyView
	Summary         string
	ClosedBy        kernel.ActorRef
	ClosedAt        time.Time
}

// CreditRequest is CreditRequest v1 (Context Map R35 CLM→STL). Approved is true when the remedy
// is a credit_note or a refund with a positive amount. Status is the claim's state (open or
// closed); approval does not wait for close. Amount is in the shipment currency's minor units.
// STL reads this before credit_note.request or fund.record kind refund, and keeps its own
// uniqueness so the same remedy is not settled twice (CLAIM_REMEDY_ALREADY_SETTLED, Wave 2).
type CreditRequest struct {
	ClaimID        string
	Status         string
	OrderID        string
	CustomerID     string
	ShipmentID     string
	ShipmentLineID string
	RemedyID       string
	Kind           string
	Amount         kernel.Money
	Approved       bool
}

// OrderClaim is one claim of an order for STL-INV-011. Open is true while Status is not closed,
// including a claim whose remedy is reject and that has not been closed.
type OrderClaim struct {
	ClaimID        string
	Status         string
	Open           bool
	OrderID        string
	ShipmentID     string
	ShipmentLineID string
	RemedyKind     string
}

// Commands is CLM's command entry point. Errors are *kernel.Rejection for refusals.
type Commands interface {
	OpenClaim(ctx context.Context, cmd OpenClaim) (Receipt, error)
	RegisterReturn(ctx context.Context, cmd RegisterReturn) (Receipt, error)
	DecideRemedy(ctx context.Context, cmd DecideRemedy) (Receipt, error)
	CloseClaim(ctx context.Context, cmd CloseClaim) (Receipt, error)
}

// Queries is CLM's audited read side. GetClaim is in-process only.
type Queries interface {
	// GetClaim returns a claim the caller sees, audited as claim.get (clm.read): NOT_FOUND otherwise.
	GetClaim(ctx context.Context, claimID string) (ClaimView, error)
	// Audit returns CLM's audit trail (clm.audit).
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

// CreditLookups answer CreditRequest v1 and the open-claims lookup for STL (R35, STL-INV-011).
// They need no permission beyond an active grant. Claims outside the caller's data scope are
// absent. Ask before opening your own transaction: CLM publishes no guard.
type CreditLookups interface {
	// ResolveCreditRequest returns the remedy of claimID. NOT_FOUND when the claim is unknown to
	// the caller or it has no remedy with remedyID. A known remedy that is not an approved credit
	// or refund is returned with Approved false.
	ResolveCreditRequest(ctx context.Context, claimID, remedyID string) (CreditRequest, error)
	// ResolveOrderClaims returns the claims of orderID the caller sees, by claim ID. An order
	// with none has an empty list.
	ResolveOrderClaims(ctx context.Context, orderID string) ([]OrderClaim, error)
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

// Reconciler recovers from an unknown outcome without executing anything. Reconciling needs
// clm.read, not the command's own permission.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func invalid(path, reason string) error {
	return kernel.Reject(kernel.CodeInvalidKnownValue, path, reason)
}

// Reason reports whether s is a reason: 1 to MaxReason characters of valid UTF-8, not only white
// space, without control characters.
func Reason(s string) bool { return text(s, MaxReason) }

func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s) && strings.TrimSpace(s) != "" && strings.IndexFunc(s, unicode.IsControl) < 0
}

func evidence(ids []string, path string) error {
	if len(ids) > MaxEvidence {
		return schema(path)
	}
	seen := map[string]bool{}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) || seen[id] {
			return schema(path + "/" + strconv.Itoa(i))
		}
		seen[id] = true
	}
	return nil
}

func quantity(s string) (kernel.Decimal, bool) {
	q, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	return q, err == nil
}

// minorForm reports whether s is a minor-unit integer of the contracts: "0" or an integer of at
// most 24 digits without a leading zero.
func minorForm(s string) bool {
	if s == "0" {
		return true
	}
	if s == "" || len(s) > 24 || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func moneyKind(kind string) bool {
	return kind == KindCreditNote || kind == KindRefund
}

// Validate checks claim.open.
func (c OpenClaim) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ClaimID):
		return schema("/args/claim_id")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !kernel.ValidIdentifier(c.ShipmentID):
		return schema("/args/shipment_id")
	case !kernel.ValidIdentifier(c.ShipmentLineID):
		return schema("/args/shipment_line_id")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// Validate checks return.register. Quantity is above zero.
func (c RegisterReturn) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ClaimID):
		return schema("/args/claim_id")
	case !kernel.ValidIdentifier(c.ReturnID):
		return schema("/args/return_id")
	}
	if q, ok := quantity(c.Quantity); !ok || q.Sign() <= 0 {
		return schema("/args/quantity")
	}
	if !text(c.Condition, MaxCondition) {
		return schema("/args/condition")
	}
	if c.Note != "" && !text(c.Note, MaxNote) {
		return schema("/args/note")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// Validate checks remedy.decide.
func (c DecideRemedy) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ClaimID):
		return schema("/args/claim_id")
	case !kernel.ValidIdentifier(c.RemedyID):
		return schema("/args/remedy_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	switch c.RemedyKind {
	case KindCreditNote, KindRefund, KindReject, KindReshipment, KindRework:
	default:
		return schema("/args/kind")
	}
	switch {
	case moneyKind(c.RemedyKind):
		if !minorForm(c.AmountMinor) || c.AmountMinor == "0" {
			return invalid("/args/amount_minor", "The amount is above zero")
		}
	case c.AmountMinor != "" && c.AmountMinor != "0":
		return invalid("/args/amount_minor", "This remedy records no amount")
	}
	return nil
}

// Validate checks claim.close.
func (c CloseClaim) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ClaimID):
		return schema("/args/claim_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !Reason(c.Summary):
		return schema("/args/summary")
	}
	return nil
}
