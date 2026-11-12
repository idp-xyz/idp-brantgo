// Package api is STL's published contract (business settlement). S5 unit A6 (WBS S5-12a) builds its
// first aggregate, CustomerCredit: credit limits, credit reservations and credit decisions (tactical
// model STL-AGG-CustomerCredit; SM-STL-CREDIT-LIMIT, SM-STL-CREDIT-RESERVATION,
// SM-STL-CREDIT-DECISION; STL-INV-004, STL-INV-005; Context Map R57 CreditGuard v1; S5 plan D-4 to
// D-7, owner decision E-2). Settlements, funds, applications and order accounts follow (B2a, B2b).
// Python V0.50 has no STL: every rule here comes from the design, not from Python vectors.
//
//	credit_limit.request     human or AI (sales clerk): asks for a limit version of a customer in the
//	                         credit policy's currency, from one or both accepted sources (internal
//	                         assessment, Sinosure buyer limit; the lower of both when both), each
//	                         with its evidence, valid from–until.
//	credit_limit.publish     human only (credit manager): publishes or rejects a requested limit. The
//	                         approver's tier — the highest credit_policy tier whose level is one of
//	                         the approver's role templates (D-6) — covers the requested amount; above
//	                         the top tier two distinct top-tier approvers publish; the approver is not
//	                         the requester and not a sales clerk (CREDIT_DECISION_NOT_AUTHORIZED).
//	credit.reserve           human, AI or system (sales clerk within the limit, D-7; credit officer):
//	                         reserves the credit portion of an order revision — the scheduled amounts
//	                         due after shipment or invoice, except document-controlled stages the
//	                         policy treats as secured (D-5) — when the customer's exposure (open
//	                         receivables + Σ max(0, live reservation − receivables formed for its
//	                         order), D-4) plus the portion stays within the limit in force and no
//	                         receivable is overdue beyond the policy's tolerance (STL-INV-004).
//	                         Otherwise a credit decision opens and commits (status decision_opened,
//	                         reasons CREDIT_LIMIT_EXCEEDED, CREDIT_OVERDUE_EXCEEDED), and nothing is
//	                         reserved; a released decision of the same binding lets the next
//	                         credit.reserve succeed within its validity. The order revision is TRD's
//	                         current one or, for an active order, one an amendment pending approval
//	                         or approved proposes (TRD OrderSnapshotFact.Proposed; S5 D-9), with that
//	                         revision's stages; any other binding is REFERENCE_STALE.
//	credit_exception.decide  human only (credit officer within its tier, credit manager): releases or
//	                         rejects an open decision; tiers as for limits, applied to the decision's
//	                         cover amount (STL-INV-005).
//	credit.release           human, AI or system: releases a reserved reservation before its order
//	                         is activated (or once it expired); a consumed one only by the system when
//	                         its order ends (PL-13). A person's or agent's release reads TRD's order
//	                         state inside the transaction, after the reservation row is locked
//	                         (brant_trd.order_state_v1): an order active at the reservation's revision
//	                         or a later one keeps it (INVALID_STATE), also when its activation
//	                         committed while the release waited for the row.
//	credit.consume           system only: marks a reserved reservation consumed when its order was
//	                         activated (PL-12), for the order binding it was reserved for.
//
// Request members in snake_case: credit_limit.request {request_id, party_id, currency {kind, id,
// revision}, sources [{source, amount, evidence_ids}], valid_from, valid_until, credit_policy {kind,
// id, revision}, reason}; credit_limit.publish {request_id, expected_version, decision ("publish" |
// "reject"), credit_policy, reason}; credit.reserve {reservation_id, order_id, order_revision,
// order_hash, credit_policy, fx_rate}; credit_exception.decide {decision_id, expected_version,
// decision ("release" | "reject"), credit_policy, reason}; credit.release {reservation_id,
// expected_version, reason}; credit.consume {reservation_id, order_id, order_revision, order_hash,
// activated_at}.
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

// Source is STL's event source.
const Source = "idp-brant.stl"

// Contract names STL's command contract, the envelope's contract member on the command routes
// POST /api/brant/v1/stl/commands/<command>.
const Contract = "brant.settlement/0.1.0"

// Commands, reads, permissions and events (command catalog V0.1 STL rows, event catalog V0.1).
const (
	CommandRequestLimit    = "credit_limit.request"
	CommandPublishLimit    = "credit_limit.publish"
	CommandReserve         = "credit.reserve"
	CommandDecideException = "credit_exception.decide"
	CommandRelease         = "credit.release"
	CommandConsume         = "credit.consume"

	// Reads: a customer's credit, a reservation, a decision, and the audit trail.
	CommandGetCredit      = "customer_credit.get"
	CommandGetReservation = "credit_reservation.get"
	CommandGetDecision    = "credit_decision.get"
	CommandAudit          = "stl.audit"
	PermissionRead        = "stl.read"
	PermissionAudit       = "stl.audit"

	EventCreditLimitRequested      = "idp-brant.stl.credit_limit_requested.v1"
	EventCreditLimitPublished      = "idp-brant.stl.credit_limit_published.v1"
	EventCreditReserved            = "idp-brant.stl.credit_reserved.v1"
	EventCreditDecisionOpened      = "idp-brant.stl.credit_decision_opened.v1"
	EventCreditExceptionDecided    = "idp-brant.stl.credit_exception_decided.v1"
	EventCreditReservationReleased = "idp-brant.stl.credit_reservation_released.v1"
	EventCreditReservationConsumed = "idp-brant.stl.credit_reservation_consumed.v1"

	// Sources of a credit limit (POL credit_policy limit_sources, ADR-BR-016); a version from both
	// takes the lower amount and records SourceLowerOfBoth.
	SourceInternalAssessment = "internal_assessment"
	SourceSinosureBuyerLimit = "sinosure_buyer_limit"
	SourceLowerOfBoth        = "lower_of_both"

	// Decisions of credit_limit.publish and credit_exception.decide.
	DecisionPublish = "publish"
	DecisionRelease = "release"
	DecisionReject  = "reject"

	// States of a limit request (SM-STL-CREDIT-LIMIT).
	RequestRequested = "requested"
	RequestPublished = "published"
	RequestRejected  = "rejected"

	// States of a reservation (SM-STL-CREDIT-RESERVATION). StateExpired is derived when read: a
	// reserved reservation at or after its expiry.
	ReservationReserved = "reserved"
	ReservationConsumed = "consumed"
	ReservationReleased = "released"
	StateExpired        = "expired"

	// States of a decision (SM-STL-CREDIT-DECISION); expired is derived (StateExpired).
	DecisionOpen     = "open"
	DecisionReleased = "released"
	DecisionRejected = "rejected"

	// StatusDecisionOpened is credit.reserve's status when it opened (or found open) a decision
	// instead of reserving.
	StatusDecisionOpened = "decision_opened"

	// Bases of a reservation.
	BasisWithinLimit      = "within_limit"
	BasisCreditDecision   = "credit_decision"
	BasisDocumentsSecured = "documents_secured"

	// POL configuration kinds STL names.
	KindCreditPolicy = "credit_policy"
	KindCurrency     = "currency"
	KindFXRate       = "fx_rate"

	MaxEvidence = 20
	MaxReason   = 1000
	maxVersion  = 2147483647
)

// ConfigRef is a POL configuration revision (ConfigRef v1): {kind, id, revision}.
type ConfigRef struct {
	Kind     string
	ConfigID string
	Revision int64
}

// LimitSource is one source of a requested limit: Source (internal_assessment or
// sinosure_buyer_limit), Amount in the limit currency (a decimal with at most its minor units, as
// kernel.ParseAmount reads it) and the EVD sources it rests on.
type LimitSource struct {
	Source      string
	Amount      string
	EvidenceIDs []string
}

// RequestCreditLimit asks for limit request RequestID of customer PartyID: an amount in Currency (a
// POL currency revision, which must be the credit policy's currency) from Sources, valid from
// ValidFrom until ValidUntil (contract timestamps), under CreditPolicy, for Reason.
type RequestCreditLimit struct {
	IdempotencyKey string
	RequestID      string
	PartyID        string
	Currency       ConfigRef
	Sources        []LimitSource
	ValidFrom      string
	ValidUntil     string
	CreditPolicy   ConfigRef
	Reason         string
}

// PublishCreditLimit publishes (Decision "publish") or rejects ("reject") limit request RequestID at
// ExpectedVersion under CreditPolicy, for Reason. Human only.
type PublishCreditLimit struct {
	IdempotencyKey  string
	RequestID       string
	ExpectedVersion int64
	Decision        string
	CreditPolicy    ConfigRef
	Reason          string
}

// ReserveCredit reserves credit for order OrderID at OrderRevision and OrderHash (TRD's current
// revision, draft or active) as reservation ReservationID under CreditPolicy. FXRate names the POL
// fx_rate revision of purpose credit that converts the order currency into the policy's currency;
// it is required when the two differ (FX_SCOPE_MISMATCH otherwise).
type ReserveCredit struct {
	IdempotencyKey string
	ReservationID  string
	OrderID        string
	OrderRevision  int64
	OrderHash      string
	CreditPolicy   ConfigRef
	FXRate         *ConfigRef
}

// DecideCreditException releases (Decision "release") or rejects ("reject") credit decision
// DecisionID at ExpectedVersion under CreditPolicy, for Reason. Human only.
type DecideCreditException struct {
	IdempotencyKey  string
	DecisionID      string
	ExpectedVersion int64
	Decision        string
	CreditPolicy    ConfigRef
	Reason          string
}

// ReleaseCredit releases reservation ReservationID at ExpectedVersion for Reason.
type ReleaseCredit struct {
	IdempotencyKey  string
	ReservationID   string
	ExpectedVersion int64
	Reason          string
}

// ConsumeCredit marks reservation ReservationID consumed by the activation of order OrderID at
// OrderRevision and OrderHash at ActivatedAt (the OrderActivated event's instant). System only.
type ConsumeCredit struct {
	IdempotencyKey string
	ReservationID  string
	OrderID        string
	OrderRevision  int64
	OrderHash      string
	ActivatedAt    string
}

// Command is one of STL's commands.
type Command interface{ stlCommand() }

func (RequestCreditLimit) stlCommand()    {}
func (PublishCreditLimit) stlCommand()    {}
func (ReserveCredit) stlCommand()         {}
func (DecideCreditException) stlCommand() {}
func (ReleaseCredit) stlCommand()         {}
func (ConsumeCredit) stlCommand()         {}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

// Reason reports whether s is a reason: 1 to MaxReason characters of valid UTF-8, not only white
// space, without control characters.
func Reason(s string) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= MaxReason && utf8.ValidString(s) && strings.TrimSpace(s) != "" &&
		strings.IndexFunc(s, unicode.IsControl) < 0
}

// Validate checks the form of the reference, at path, and that it is of kind.
func (r ConfigRef) Validate(kind, path string) error {
	switch {
	case r.Kind != kind:
		return schema(path + "/kind")
	case !kernel.ValidIdentifier(r.ConfigID):
		return schema(path + "/id")
	case !versionOK(r.Revision):
		return schema(path + "/revision")
	}
	return nil
}

func versionOK(v int64) bool { return v >= 1 && v <= maxVersion }

func hash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func amount(s string) bool {
	d, err := kernel.ParseDecimal(s, kernel.DecimalFormat{IntDigits: kernel.MaxMinorDigits - 4, Scale: 4})
	return err == nil && d.Sign() >= 0
}

// Validate checks the closed contract: identifiers, one or two distinct sources, each with a
// non-negative amount and 1 to 20 distinct evidence IDs, an interval that ends after it starts, the
// references and a reason.
func (c RequestCreditLimit) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.RequestID):
		return schema("/args/request_id")
	case !kernel.ValidIdentifier(c.PartyID):
		return schema("/args/party_id")
	}
	if err := c.Currency.Validate(KindCurrency, "/args/currency"); err != nil {
		return err
	}
	if len(c.Sources) < 1 || len(c.Sources) > 2 {
		return schema("/args/sources")
	}
	for i, s := range c.Sources {
		p := "/args/sources/" + strconv.Itoa(i)
		switch {
		case s.Source != SourceInternalAssessment && s.Source != SourceSinosureBuyerLimit:
			return schema(p + "/source")
		case i == 1 && s.Source == c.Sources[0].Source:
			return schema(p + "/source")
		case !amount(s.Amount):
			return schema(p + "/amount")
		case len(s.EvidenceIDs) < 1 || len(s.EvidenceIDs) > MaxEvidence:
			return schema(p + "/evidence_ids")
		}
		for j, e := range s.EvidenceIDs {
			if !kernel.ValidIdentifier(e) || indexOf(s.EvidenceIDs, e) != j {
				return schema(p + "/evidence_ids/" + strconv.Itoa(j))
			}
		}
	}
	from, err := kernel.ParseTimestamp(c.ValidFrom)
	if err != nil {
		return schema("/args/valid_from")
	}
	until, err := kernel.ParseTimestamp(c.ValidUntil)
	if err != nil || !until.After(from) {
		return schema("/args/valid_until")
	}
	if err = c.CreditPolicy.Validate(KindCreditPolicy, "/args/credit_policy"); err != nil {
		return err
	}
	if !Reason(c.Reason) {
		return schema("/args/reason")
	}
	return nil
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// Validate checks the closed contract.
func (c PublishCreditLimit) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.RequestID):
		return schema("/args/request_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case c.Decision != DecisionPublish && c.Decision != DecisionReject:
		return schema("/args/decision")
	}
	if err := c.CreditPolicy.Validate(KindCreditPolicy, "/args/credit_policy"); err != nil {
		return err
	}
	if !Reason(c.Reason) {
		return schema("/args/reason")
	}
	return nil
}

// Validate checks the closed contract.
func (c ReserveCredit) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ReservationID):
		return schema("/args/reservation_id")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !versionOK(c.OrderRevision):
		return schema("/args/order_revision")
	case !hash(c.OrderHash):
		return schema("/args/order_hash")
	}
	if err := c.CreditPolicy.Validate(KindCreditPolicy, "/args/credit_policy"); err != nil {
		return err
	}
	if c.FXRate != nil {
		return c.FXRate.Validate(KindFXRate, "/args/fx_rate")
	}
	return nil
}

// Validate checks the closed contract.
func (c DecideCreditException) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DecisionID):
		return schema("/args/decision_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case c.Decision != DecisionRelease && c.Decision != DecisionReject:
		return schema("/args/decision")
	}
	if err := c.CreditPolicy.Validate(KindCreditPolicy, "/args/credit_policy"); err != nil {
		return err
	}
	if !Reason(c.Reason) {
		return schema("/args/reason")
	}
	return nil
}

// Validate checks the closed contract.
func (c ReleaseCredit) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ReservationID):
		return schema("/args/reservation_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return nil
}

// Validate checks the closed contract.
func (c ConsumeCredit) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ReservationID):
		return schema("/args/reservation_id")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !versionOK(c.OrderRevision):
		return schema("/args/order_revision")
	case !hash(c.OrderHash):
		return schema("/args/order_hash")
	}
	if _, err := kernel.ParseTimestamp(c.ActivatedAt); err != nil {
		return schema("/args/activated_at")
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

// LimitReceipt is the answer of credit_limit.request and credit_limit.publish: the receipt
// (ObjectID the request ID, RowVersion its version, Status its state), the customer, the amount
// and currency, how many approvals it has and needs (0 until a publish decides), and the limit
// version a publication created.
type LimitReceipt struct {
	Receipt
	PartyID           string
	Currency          string
	AmountMinor       string
	Approvals         int
	RequiredApprovals int
	LimitVersion      int64
}

// ReservationReceipt is the answer of credit.reserve, credit.release and credit.consume. For a
// reservation, ObjectID is its ID, RowVersion its version and Status its state; when credit.reserve
// opened (or found open) a decision instead, ObjectID is the decision's ID, RowVersion its version
// and Status decision_opened, with the decision's Reasons. The amounts: the order's credit portion
// and secured amount in the order currency, and, in the limit currency, the limit in force, the
// exposure before this portion and the excess over the limit.
type ReservationReceipt struct {
	Receipt
	PartyID       string
	ReservationID string
	DecisionID    string
	OrderID       string
	OrderRevision int64
	OrderHash     string
	Currency      string
	PortionMinor  string
	SecuredMinor  string
	Basis         string
	Reasons       []string
	LimitCurrency string
	LimitMinor    string
	ExposureMinor string
	ExcessMinor   string
	ExpiresAt     time.Time
}

// DecisionReceipt is the answer of credit_exception.decide: the receipt (ObjectID the decision ID,
// RowVersion its version, Status open — one of two approvals recorded — released or rejected) and
// its approvals.
type DecisionReceipt struct {
	Receipt
	PartyID           string
	Approvals         int
	RequiredApprovals int
	ExpiresAt         time.Time
}

// ApprovalView is one approval of a limit request or a decision: the approver, the policy tier it
// approved with and when.
type ApprovalView struct {
	Approver   kernel.ActorRef
	Level      string
	MaxMinor   string
	Epoch      int64
	ApprovedAt time.Time
}

// LimitSourceView is one source of a limit request.
type LimitSourceView struct {
	Source      string
	AmountMinor string
	EvidenceIDs []string
}

// LimitRequestView is a limit request as the reads show it.
type LimitRequestView struct {
	RequestID      string
	Status         string
	Version        int64
	Currency       string
	AmountMinor    string
	Sources        []LimitSourceView
	ValidFrom      time.Time
	ValidUntil     time.Time
	CreditPolicy   ConfigRef
	PolicyHash     string
	Reason         string
	RequestedBy    kernel.ActorRef
	RequestedAt    time.Time
	Approvals      []ApprovalView
	DecidedBy      kernel.ActorRef
	DecidedAt      time.Time
	DecisionReason string
	LimitVersion   int64
}

// LimitVersionView is a published limit version: immutable once published.
type LimitVersionView struct {
	Version     int64
	RequestID   string
	Currency    string
	AmountMinor string
	Source      string
	ValidFrom   time.Time
	ValidUntil  time.Time
	InForce     bool
	PublishedBy kernel.ActorRef
	PublishedAt time.Time
}

// ReservationView is a reservation as the reads show it. State is its state now: expired for a
// reserved reservation at or after its expiry, otherwise Status.
type ReservationView struct {
	ReservationID string
	PartyID       string
	OrderID       string
	OrderRevision int64
	OrderHash     string
	Status        string
	State         string
	Version       int64
	Currency      string
	PortionMinor  string
	SecuredMinor  string
	LimitCurrency string
	ExposureMinor string
	FXRate        *ConfigRef
	CreditPolicy  ConfigRef
	Basis         string
	DecisionID    string
	ExpiresAt     time.Time
	ReservedBy    kernel.ActorRef
	ReservedAt    time.Time
	ConsumedBy    kernel.ActorRef
	ConsumedAt    time.Time
	ActivatedAt   time.Time
	ReleasedBy    kernel.ActorRef
	ReleasedAt    time.Time
	ReleaseReason string
	CheckedAt     time.Time
}

// DecisionView is a credit decision as the reads show it. State is its state now: expired for an
// open or released decision at or after its expiry that no reservation used, otherwise Status.
// CoverMinor is the amount an approver's tier must cover (the excess, or the converted portion
// when a receivable is overdue).
type DecisionView struct {
	DecisionID     string
	PartyID        string
	OrderID        string
	OrderRevision  int64
	OrderHash      string
	Status         string
	State          string
	Version        int64
	Reasons        []string
	Currency       string
	PortionMinor   string
	LimitCurrency  string
	ConvertedMinor string
	LimitMinor     string
	LimitVersion   int64
	ExposureMinor  string
	ExcessMinor    string
	CoverMinor     string
	OverdueDays    int
	CreditPolicy   ConfigRef
	ExpiresAt      time.Time
	OpenedBy       kernel.ActorRef
	OpenedAt       time.Time
	Approvals      []ApprovalView
	DecidedBy      kernel.ActorRef
	DecidedAt      time.Time
	DecisionReason string
	ReservationID  string
	CheckedAt      time.Time
}

// CustomerCreditView is a customer's credit: the limit version in force now (none: the credit
// policy's default limit applies), every version, the limit requests, reservations and decisions
// the caller sees, and the exposure of the live reservations now in the limit currency (D-4; open
// receivables count from B2b).
type CustomerCreditView struct {
	PartyID          string
	RowVersion       int64
	LimitInForce     *LimitVersionView
	Versions         []LimitVersionView
	Requests         []LimitRequestView
	Reservations     []ReservationView
	Decisions        []DecisionView
	ExposureCurrency string
	ExposureMinor    string
	CheckedAt        time.Time
}

// Commands is STL's command entry point. Errors are *kernel.Rejection for refusals. An error that
// is application.ErrCommitUncertain from IDP-Bento-Go means the outcome is unknown: the caller sends
// the same command with the same idempotency key again, or reconciles it.
type Commands interface {
	RequestCreditLimit(ctx context.Context, cmd RequestCreditLimit) (LimitReceipt, error)
	PublishCreditLimit(ctx context.Context, cmd PublishCreditLimit) (LimitReceipt, error)
	ReserveCredit(ctx context.Context, cmd ReserveCredit) (ReservationReceipt, error)
	DecideCreditException(ctx context.Context, cmd DecideCreditException) (DecisionReceipt, error)
	ReleaseCredit(ctx context.Context, cmd ReleaseCredit) (ReservationReceipt, error)
	ConsumeCredit(ctx context.Context, cmd ConsumeCredit) (ReservationReceipt, error)
}

// Queries is STL's audited read side.
type Queries interface {
	// GetCustomerCredit returns a customer's credit, audited as customer_credit.get (stl.read):
	// NOT_FOUND for a customer STL has no credit record of. Requests, reservations and decisions
	// outside the caller's data scope are left out; the exposure counts them all.
	GetCustomerCredit(ctx context.Context, partyID string) (CustomerCreditView, error)
	// GetReservation returns a reservation the caller sees, audited as credit_reservation.get.
	GetReservation(ctx context.Context, reservationID string) (ReservationView, error)
	// GetDecision returns a decision the caller sees, audited as credit_decision.get.
	GetDecision(ctx context.Context, decisionID string) (DecisionView, error)
	// Audit returns STL's audit trail (stl.audit).
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
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
// Reconciling needs stl.read, not the command's own permission.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}
