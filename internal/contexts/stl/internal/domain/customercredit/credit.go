package customercredit

import (
	"fmt"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// States, bases and sources (SM-STL-CREDIT-LIMIT, SM-STL-CREDIT-RESERVATION, SM-STL-CREDIT-DECISION).
const (
	RequestRequested = "requested"
	RequestPublished = "published"
	RequestRejected  = "rejected"

	ReservationReserved = "reserved"
	ReservationConsumed = "consumed"
	ReservationReleased = "released"
	StateExpired        = "expired"

	DecisionOpen     = "open"
	DecisionReleased = "released"
	DecisionRejected = "rejected"

	BasisWithinLimit      = "within_limit"
	BasisCreditDecision   = "credit_decision"
	BasisDocumentsSecured = "documents_secured"

	SourceInternalAssessment = "internal_assessment"
	SourceSinosureBuyerLimit = "sinosure_buyer_limit"
	SourceLowerOfBoth        = "lower_of_both"

	// ReleaseExpired is the reason credit.reserve records when it releases the expired reservation
	// of the order revision it reserves again (WF-08: 占用到期后须重新占用).
	ReleaseExpired = "expired: reserved again"
)

// Binding is an order revision as STL binds it: the order, its order revision and order hash.
type Binding struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
}

// Act is who did something, under which authority epoch, when, and why.
type Act struct {
	By     kernel.ActorRef
	Epoch  int64
	At     time.Time
	Reason string
}

// Root is the CustomerCredit root of one customer: its row version and how many limit versions it
// has published.
type Root struct {
	PartyID      string
	RowVersion   int64
	LimitVersion int64
	CreatedAt    time.Time
}

// SourceAmount is one source of a limit request: its amount and evidence.
type SourceAmount struct {
	Amount      kernel.Money
	EvidenceIDs []string
}

// LimitRequest is a CreditLimitRequest (SM-STL-CREDIT-LIMIT): an amount in the policy's currency
// from one or both sources (the lower when both), valid from–until, requested under a policy
// revision; requested → published | rejected, its version moving by one per approval.
type LimitRequest struct {
	ID               string
	PartyID          string
	Status           string
	Version          int64
	Currency         kernel.Currency
	CurrencyRevision int64
	Amount           kernel.Money
	Internal         *SourceAmount
	Sinosure         *SourceAmount
	ValidFrom        time.Time
	ValidUntil       time.Time
	Policy           ConfigUse
	Reason           string
	OwnerActor       string
	OwnerTeam        string
	Requested        Act
	Decided          *Act
	LimitVersion     int64
	Approvals        []Approval
}

// LimitInput is what credit_limit.request asks for: amounts in Currency by source.
type LimitInput struct {
	ID               string
	PartyID          string
	Currency         kernel.Currency
	CurrencyRevision int64
	Sources          map[string]SourceAmount
	ValidFrom        time.Time
	ValidUntil       time.Time
	Reason           string
	Team             string
	By               Act
}

// NewLimitRequest checks a limit request against the policy (ADR-BR-016, SM-STL-CREDIT-LIMIT): the
// limit is in the policy's currency (FX_SCOPE_MISMATCH), every source is one the policy accepts,
// and the interval ends in the future; the amount is the lower of both sources.
func NewLimitRequest(in LimitInput, p Policy) (*LimitRequest, error) {
	if in.Currency != p.Currency {
		return nil, Reject(CodeFXScopeMismatch, "/args/currency", fmt.Sprintf("A limit is in the credit policy's currency %s", p.Currency))
	}
	r := &LimitRequest{ID: in.ID, PartyID: in.PartyID, Status: RequestRequested, Version: 1, Currency: in.Currency,
		CurrencyRevision: in.CurrencyRevision, ValidFrom: kernel.Instant(in.ValidFrom), ValidUntil: kernel.Instant(in.ValidUntil),
		Policy: p.Use, Reason: in.Reason, OwnerActor: in.By.By.ID, OwnerTeam: in.Team, Requested: in.By}
	i := 0
	for _, src := range []string{SourceInternalAssessment, SourceSinosureBuyerLimit} {
		s, ok := in.Sources[src]
		if !ok {
			continue
		}
		if !slices.Contains(p.LimitSources, src) {
			return nil, invalid(fmt.Sprintf("/args/sources/%d/source", i), "The credit policy does not accept "+src)
		}
		if s.Amount.Currency() != in.Currency {
			return nil, Reject(CodeFXScopeMismatch, fmt.Sprintf("/args/sources/%d/amount", i), "")
		}
		s.EvidenceIDs = slices.Clone(s.EvidenceIDs)
		if src == SourceInternalAssessment {
			r.Internal = &s
		} else {
			r.Sinosure = &s
		}
		if c, _ := s.Amount.Cmp(r.Amount); i == 0 || c < 0 {
			r.Amount = s.Amount
		}
		i++
	}
	if i == 0 {
		return nil, invalid("/args/sources", "A limit has a source")
	}
	if !r.ValidUntil.After(in.By.At) {
		return nil, invalid("/args/valid_until", "The limit would already have ended")
	}
	return r, nil
}

// Source returns the source the limit version records.
func (r *LimitRequest) Source() string {
	switch {
	case r.Internal != nil && r.Sinosure != nil:
		return SourceLowerOfBoth
	case r.Sinosure != nil:
		return SourceSinosureBuyerLimit
	}
	return SourceInternalAssessment
}

// LimitVersion is a published credit limit: immutable, valid from–until.
type LimitVersion struct {
	PartyID    string
	Version    int64
	RequestID  string
	Currency   kernel.Currency
	Amount     kernel.Money
	Source     string
	ValidFrom  time.Time
	ValidUntil time.Time
	Policy     ConfigUse
	Published  Act
}

// InForce reports whether v applies at t.
func (v LimitVersion) InForce(t time.Time) bool {
	return !t.Before(v.ValidFrom) && t.Before(v.ValidUntil)
}

// LimitInForce returns the limit version in force at t: the latest published version whose
// interval contains t, or nil (the policy's default limit applies, ADR-BR-016: 新客户默认额度为 0).
func LimitInForce(versions []LimitVersion, t time.Time) *LimitVersion {
	var best *LimitVersion
	for i := range versions {
		if v := &versions[i]; v.InForce(t) && (best == nil || v.Version > best.Version) {
			best = v
		}
	}
	return best
}

// Publish records a's approval of r under p, at act; once r has the approvals its amount needs it
// is published and the limit version next returned (nil while a second approval is awaited).
func (r *LimitRequest) Publish(p Policy, a Approver, act Act, next int64) (*LimitVersion, int, error) {
	if r.Status != RequestRequested {
		return nil, 0, state("/args/request_id", "The limit request is "+r.Status)
	}
	if r.Currency != p.Currency {
		return nil, 0, Reject(CodeFXScopeMismatch, "/args/credit_policy", fmt.Sprintf("The limit is in %s, the credit policy in %s", r.Currency, p.Currency))
	}
	ap, required, err := p.Approve(r.Amount, a, r.Requested.By.ID, r.Approvals, "/args/request_id")
	if err != nil {
		return nil, 0, err
	}
	ap.Epoch, ap.At = act.Epoch, act.At
	r.Approvals = append(r.Approvals, ap)
	r.Version++
	if len(r.Approvals) < required {
		return nil, required, nil
	}
	r.Status, r.Decided, r.LimitVersion = RequestPublished, &act, next
	return &LimitVersion{PartyID: r.PartyID, Version: next, RequestID: r.ID, Currency: r.Currency, Amount: r.Amount, Source: r.Source(),
		ValidFrom: r.ValidFrom, ValidUntil: r.ValidUntil, Policy: p.Use, Published: act}, required, nil
}

// Reject rejects r (a reason is required by the contract); the approver is independent of the
// requester.
func (r *LimitRequest) Reject(a Approver, act Act) error {
	if r.Status != RequestRequested {
		return state("/args/request_id", "The limit request is "+r.Status)
	}
	if err := Independent(a, r.Requested.By.ID, "/args/request_id"); err != nil {
		return err
	}
	r.Status, r.Decided = RequestRejected, &act
	r.Version++
	return nil
}

// Decision is a CreditDecision (SM-STL-CREDIT-DECISION): opened by credit.reserve for one order
// binding when the reservation exceeds the limit or a receivable is overdue; bound to the order,
// the portion, the reasons and its validity; open → released | rejected by credit staff, expired
// (derived) once its validity ends unused.
type Decision struct {
	ID            string
	PartyID       string
	Binding       Binding
	Portion       kernel.Money // order currency
	Converted     kernel.Money // the portion in the limit currency
	FX            *ConfigUse
	Limit         kernel.Money
	LimitVersion  int64
	Exposure      kernel.Money
	Excess        kernel.Money
	Cover         kernel.Money
	OverdueDays   int // -1: no overdue reason
	Reasons       []string
	Policy        ConfigUse
	Status        string
	Version       int64
	ExpiresAt     time.Time
	OwnerActor    string
	OwnerTeam     string
	Opened        Act
	Decided       *Act
	Approvals     []Approval
	ReservationID string // the reservation made under it, if any
}

// Expired reports whether d's validity ended at t without a reservation using it.
func (d *Decision) Expired(t time.Time) bool {
	return (d.Status == DecisionOpen || d.Status == DecisionReleased) && d.ReservationID == "" && !t.Before(d.ExpiresAt)
}

// State returns d's state at t.
func (d *Decision) State(t time.Time) string {
	if d.Expired(t) {
		return StateExpired
	}
	return d.Status
}

// Release records a's approval of d under p at act (STL-INV-005): the approver's tier covers d's
// cover amount, two distinct top-tier approvers above the top tier. It reports whether d is now
// released and how many approvals it needs.
func (d *Decision) Release(p Policy, a Approver, act Act) (bool, int, error) {
	if err := d.decidable(act.At); err != nil {
		return false, 0, err
	}
	ap, required, err := p.Approve(d.Cover, a, d.Opened.By.ID, d.Approvals, "/args/decision_id")
	if err != nil {
		return false, 0, err
	}
	ap.Epoch, ap.At = act.Epoch, act.At
	d.Approvals = append(d.Approvals, ap)
	d.Version++
	if len(d.Approvals) < required {
		return false, required, nil
	}
	d.Status, d.Decided = DecisionReleased, &act
	return true, required, nil
}

// Reject rejects d; the approver is independent of the requester.
func (d *Decision) Reject(a Approver, act Act) error {
	if err := d.decidable(act.At); err != nil {
		return err
	}
	if err := Independent(a, d.Opened.By.ID, "/args/decision_id"); err != nil {
		return err
	}
	d.Status, d.Decided = DecisionRejected, &act
	d.Version++
	return nil
}

func (d *Decision) decidable(t time.Time) error {
	switch {
	case d.Status != DecisionOpen:
		return state("/args/decision_id", "The credit decision is "+d.Status)
	case d.Expired(t):
		return Reject(CodeApprovalExpired, "/args/decision_id", "The credit decision expired; credit.reserve opens a new one")
	}
	return nil
}

// Covers reports whether released decision d lets a reservation of binding b with portion (order
// currency) and verdict v be made at t: same binding, unused, still valid, a portion no larger than
// d's, an excess no larger than d's, and an overdue receivable only when d was released for one,
// no more days overdue than then.
func (d *Decision) Covers(b Binding, portion kernel.Money, v Verdict, t time.Time) bool {
	if d.Status != DecisionReleased || d.ReservationID != "" || !t.Before(d.ExpiresAt) || d.Binding != b {
		return false
	}
	if c, err := portion.Cmp(d.Portion); err != nil || c > 0 {
		return false
	}
	if c, err := v.Excess.Cmp(d.Excess); err != nil || c > 0 {
		return false
	}
	if v.Overdue() && (!slices.Contains(d.Reasons, CodeCreditOverdueExceeded) || v.OverdueDays > d.OverdueDays) {
		return false
	}
	return true
}

// Reservation is a CreditReservation (SM-STL-CREDIT-RESERVATION): the credit portion of one order
// revision held against the customer's limit; reserved → consumed (system, on activation) →
// released (system, on the order's end), or reserved → released. A reserved reservation expires
// (derived) at ExpiresAt.
type Reservation struct {
	ID          string
	PartyID     string
	Binding     Binding
	Amount      kernel.Money // the portion, order currency
	Secured     kernel.Money // document-controlled stages treated as secured, order currency
	Exposure    kernel.Money // the portion in the limit currency
	FX          *ConfigUse
	Basis       string
	DecisionID  string
	Policy      ConfigUse
	Status      string
	Version     int64
	ExpiresAt   time.Time
	OwnerActor  string
	OwnerTeam   string
	Reserved    Act
	Consumed    *Act
	ActivatedAt time.Time
	Released    *Act
}

// Live reports whether r holds credit at t: consumed, or reserved and not expired.
func (r *Reservation) Live(t time.Time) bool {
	return r.Status == ReservationConsumed || (r.Status == ReservationReserved && t.Before(r.ExpiresAt))
}

// State returns r's state at t.
func (r *Reservation) State(t time.Time) string {
	if r.Status == ReservationReserved && !t.Before(r.ExpiresAt) {
		return StateExpired
	}
	return r.Status
}

// Consume marks r consumed by the activation of order binding b at activatedAt (PL-12): r is
// reserved for exactly b and was not expired when the order was activated.
func (r *Reservation) Consume(b Binding, activatedAt time.Time, act Act) error {
	switch {
	case r.Binding.OrderID != b.OrderID:
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "The reservation is for order "+r.Binding.OrderID)
	case r.Binding != b:
		return Reject(CodeReferenceStale, "/args/order_revision", fmt.Sprintf("The reservation is for order revision %d", r.Binding.OrderRevision))
	case r.Status != ReservationReserved:
		return state("/args/reservation_id", "The reservation is "+r.Status)
	case activatedAt.Before(r.Reserved.At) || activatedAt.After(act.At):
		return invalid("/args/activated_at", "The order was activated before the reservation or in the future")
	case !activatedAt.Before(r.ExpiresAt):
		return state("/args/activated_at", "The reservation had expired when the order was activated")
	}
	r.Status, r.Consumed, r.ActivatedAt = ReservationConsumed, &act, kernel.Instant(activatedAt)
	r.Version++
	return nil
}

// Release releases r at act: a reserved reservation by anyone allowed to (the order not active, or
// r expired: application's check), a consumed one only by the system when its order ends (PL-13).
func (r *Reservation) Release(act Act) error {
	switch {
	case r.Status == ReservationReleased:
		return state("/args/reservation_id", "The reservation is released")
	case r.Status == ReservationConsumed && act.By.Kind != kernel.ActorSystem:
		return state("/args/reservation_id", "A consumed reservation is released when its order ends")
	}
	r.Status, r.Released = ReservationReleased, &act
	r.Version++
	return nil
}
