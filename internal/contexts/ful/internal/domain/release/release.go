// Package release holds the rules of FUL's CommercialRelease aggregate (tactical model
// FUL-AGG-CommercialRelease, SM-FUL-RELEASE, FUL-INV-002; UL-INV-28, MD-38; WF-07, ES-10, SC-08,
// BT13; S5 decision D-11; WBS S5-07): one release root per shipment and scope, its evaluations,
// its payment-condition exceptions and its consumption by shipment.confirm or
// document_release.record. The package knows no database: the application gives it what the
// guards answered.
package release

import (
	"math/big"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Scopes, states, outcomes and the ways documents are released.
const (
	ScopeShipment  = "shipment"
	ScopeDocuments = "documents"

	StatusBlocked  = "blocked"
	StatusGranted  = "granted"
	StatusConsumed = "consumed"

	OutcomeGranted = "granted"
	OutcomeBlocked = "blocked"

	KindOriginalBL   = "original_bl"
	KindTelexRelease = "telex_release"

	CommandConfirmShipment       = "shipment.confirm"
	CommandRecordDocumentRelease = "document_release.record"

	// GuardFulfilment names FUL's own quantity rules among the guards.
	GuardFulfilment = "ful.fulfilment"

	// PaymentStageUnpaid is STL's reason for a stage that is not fully applied: the only payment
	// verdict an exception can cover.
	PaymentStageUnpaid = "stage_unpaid"

	// MaxExceptionValidity bounds an exception's validity.
	MaxExceptionValidity = 90 * 24 * time.Hour
)

// Act is who did something, under which authority epoch, when.
type Act = shipment.Act

// Entry is one named value of a guard evidence.
type Entry = shipment.Entry

// Reject returns a rejection with a code of FUL's rules or a shared kernel code.
func Reject(code, path, reason string) *kernel.Rejection { return shipment.Reject(code, path, reason) }

// PaymentVerdict is STL's PaymentConditionGuard v1 on the order revision the shipment binds for the
// release's scope. Amounts are integer minor units of Currency.
type PaymentVerdict struct {
	Scope             string
	Satisfied         bool
	Status            string
	Reason            string
	OrderRevision     int64
	OrderHash         string
	Currency          string
	RequiredMinor     string
	AppliedMinor      string
	StageID           string
	StageTrigger      string
	StageDueMinor     string
	StageAppliedMinor string
	ShortfallMinor    string
}

func minor(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return new(big.Int)
	}
	return v
}

// ScopeShortfall is what the scope's stages miss together: required less applied.
func (p PaymentVerdict) ScopeShortfall() *big.Int {
	return new(big.Int).Sub(minor(p.RequiredMinor), minor(p.AppliedMinor))
}

// Evidence is the value an evaluation records for the payment condition.
func (p PaymentVerdict) Evidence() string {
	v := p.Status + "/" + p.Reason + "/r" + strconv.FormatInt(p.OrderRevision, 10) + "/" + p.Currency + "/applied=" + p.AppliedMinor +
		"/required=" + p.RequiredMinor
	if p.StageID != "" {
		v += "/stage=" + p.StageID + "/shortfall=" + p.ShortfallMinor
	}
	return v
}

func (p PaymentVerdict) refusal() string {
	switch p.Reason {
	case "no_account":
		return "The order has no order account in STL"
	case "revision_mismatch":
		return "The order account holds revision " + strconv.FormatInt(p.OrderRevision, 10) + ": its payment plan review is pending"
	}
	return "Payment stage " + p.StageID + " (" + p.StageTrigger + ") has " + p.StageAppliedMinor + " of " + p.StageDueMinor + " " + p.Currency +
		" applied: " + p.ShortfallMinor + " short for " + p.Scope
}

// The bindings of an evaluation: what each guard answered (TradeComplianceRef, LicenseAllocationRef,
// ScreeningRef, the supplier delivery lines, QualityReleaseRef, PaymentConditionRef and the order).
// Their JSON is the published api.ReleaseBindings.
type (
	ComplianceBinding struct {
		Status             string    `json:"status"`
		CheckID            string    `json:"check_id"`
		DecisionID         string    `json:"decision_id"`
		LicenseRequired    bool      `json:"license_required"`
		RegulationID       string    `json:"regulation_id"`
		RegulationRevision int64     `json:"regulation_revision"`
		ValidUntil         time.Time `json:"valid_until"`
	}
	LicenseBinding struct {
		Status       string `json:"status"`
		LicenseID    string `json:"license_id"`
		AllocationID string `json:"allocation_id"`
		LinesHash    string `json:"lines_hash"`
		Quantity     string `json:"quantity"`
		Unit         string `json:"unit"`
	}
	ScreeningBinding struct {
		PartyID       string `json:"party_id"`
		Status        string `json:"status"`
		ScreeningID   string `json:"screening_id"`
		PartyRevision int64  `json:"party_revision"`
	}
	DeliveryBinding struct {
		DeliveryID      string `json:"delivery_id"`
		LineID          string `json:"line_id"`
		Version         int64  `json:"version"`
		Status          string `json:"status"`
		VerifiedVersion int64  `json:"verified_version"`
		Verified        string `json:"verified_quantity"`
	}
	QualityBinding struct {
		ReleaseID  string    `json:"release_id"`
		Status     string    `json:"status"`
		Released   string    `json:"released_quantity"`
		ValidUntil time.Time `json:"valid_until"`
	}
	PaymentBinding struct {
		Scope          string `json:"scope"`
		Status         string `json:"status"`
		Reason         string `json:"reason"`
		OrderRevision  int64  `json:"order_revision"`
		OrderHash      string `json:"order_hash"`
		Currency       string `json:"currency"`
		RequiredMinor  string `json:"required_minor"`
		AppliedMinor   string `json:"applied_minor"`
		StageID        string `json:"stage_id"`
		ShortfallMinor string `json:"shortfall_minor"`
		ExceptionSeq   int64  `json:"exception_seq"`
	}
	OrderBindingSeen struct {
		OrderID       string `json:"order_id"`
		OrderRevision int64  `json:"order_revision"`
		OrderHash     string `json:"order_hash"`
		Status        string `json:"status"`
	}
	Bindings struct {
		TradeCompliance *ComplianceBinding `json:"trade_compliance,omitempty"`
		ExportLicense   *LicenseBinding    `json:"export_license,omitempty"`
		Screenings      []ScreeningBinding `json:"screenings"`
		Deliveries      []DeliveryBinding  `json:"supplier_deliveries"`
		QualityReleases []QualityBinding   `json:"quality_releases"`
		Payment         *PaymentBinding    `json:"payment_condition,omitempty"`
		Order           *OrderBindingSeen  `json:"order,omitempty"`
	}
)

// Evaluation is one commercial_release.evaluate: its outcome, the first unmet guard with its code
// and why (blocked), the exception that covered the payment condition (granted), the guard evidence
// sealed with its SHA-256, and the bindings.
type Evaluation struct {
	Seq            int64
	ReleaseVersion int64
	Outcome        string
	Guard          string
	Code           string
	Reason         string
	ExceptionSeq   int64
	Entries        []Entry
	EntriesJSON    string
	EntriesSHA256  string
	Bindings       Bindings
	Evaluated      Act
}

// Exception is a payment-condition exception: one unpaid stage of the order revision the shipment
// binds, short ShortfallMinor of Currency in the release's scope, until ValidUntil.
type Exception struct {
	Seq            int64
	ReleaseVersion int64
	Order          shipment.OrderBinding
	StageID        string
	Currency       string
	ShortfallMinor string
	ValidUntil     time.Time
	Reason         string
	Evidence       []string
	Entries        []Entry
	EntriesJSON    string
	EntriesSHA256  string
	Granted        Act
}

// Covers reports whether e covers the unmet payment verdict p at now: within its validity, the same
// order revision, currency and first unpaid stage, and no more short in the whole scope than it
// was granted for.
func (e Exception) Covers(p PaymentVerdict, now time.Time) bool {
	return !p.Satisfied && p.Reason == PaymentStageUnpaid && now.Before(e.ValidUntil) && p.OrderRevision == e.Order.OrderRevision &&
		p.StageID == e.StageID && p.Currency == e.Currency && p.ScopeShortfall().Cmp(minor(e.ShortfallMinor)) <= 0
}

// Consumption is what consumed a release: the command, how the documents were released
// (document_release.record), the evidence cited and the guard evidence of the consuming
// transaction.
type Consumption struct {
	Command       string
	ReleaseKind   string
	Evidence      []string
	Entries       []Entry
	EntriesJSON   string
	EntriesSHA256 string
	Consumed      Act
}

// Release is the aggregate root. RowVersion counts its changes: one per evaluation, per exception
// and for its consumption.
type Release struct {
	ID          string
	ShipmentID  string
	Scope       string
	Status      string
	RowVersion  int64
	Order       shipment.OrderBinding
	Opened      Act
	Evaluations []Evaluation
	Exceptions  []Exception
	Consumption *Consumption
}

// New opens the release of shipment shipmentID for scope, binding the shipment's order, by its
// first evaluation (SM-FUL-RELEASE initial state blocked, row version 0 until it is evaluated).
func New(id, shipmentID, scope string, order shipment.OrderBinding, act Act) *Release {
	act.At = kernel.Instant(act.At)
	return &Release{ID: id, ShipmentID: shipmentID, Scope: scope, Status: StatusBlocked, Order: order, Opened: act}
}

// Evaluable refuses to evaluate a consumed release (INVALID_STATE): it is terminal.
func (r *Release) Evaluable() error {
	if r.Status == StatusConsumed {
		return Reject(kernel.CodeInvalidState, "/args/scope", "The "+r.Scope+" release is consumed")
	}
	return nil
}

// LiveException returns the latest exception of r valid at now, or nil.
func (r *Release) LiveException(now time.Time) *Exception {
	for i := len(r.Exceptions) - 1; i >= 0; i-- {
		if now.Before(r.Exceptions[i].ValidUntil) {
			return &r.Exceptions[i]
		}
	}
	return nil
}

// CheckPayment judges STL's verdict for the release's scope: satisfied, or an unmet condition the
// live exception covers (its sequence is returned), or COMMERCIAL_RELEASE_BLOCKED at path. r may be
// nil (no release yet: no exception).
func (r *Release) CheckPayment(p PaymentVerdict, now time.Time, path string) (Entry, int64, error) {
	e := Entry{Name: shipment.GuardPaymentCondition, Value: p.Evidence()}
	if p.Satisfied {
		return e, 0, nil
	}
	if r != nil {
		if x := r.LiveException(now); x != nil && x.Covers(p, now) {
			e.Value += "/exception=" + strconv.FormatInt(x.Seq, 10)
			return e, x.Seq, nil
		}
	}
	return e, 0, Reject(shipment.CodeCommercialReleaseBlocked, path, p.refusal())
}

// Verdict is what the guards of one evaluation answered: the evidence entries and bindings, the
// first unmet guard's refusal and name (nil: every guard satisfied) and the exception that covered
// the payment condition.
type Verdict struct {
	Entries      []Entry
	Bindings     Bindings
	Failure      *kernel.Rejection
	Guard        string
	ExceptionSeq int64
}

func reasonOf(r *kernel.Rejection) string {
	if r == nil || len(r.Fields) == 0 {
		return ""
	}
	return r.Fields[0].Reason
}

// Evaluate records an evaluation (SM-FUL-RELEASE: blocked or granted to granted when every guard is
// satisfied, to blocked otherwise) and reports whether it granted a release that was not granted
// before. The caller has run Evaluable.
func (r *Release) Evaluate(v Verdict, act Act) bool {
	act.At = kernel.Instant(act.At)
	doc, sum := shipment.Seal(v.Entries)
	e := Evaluation{Seq: int64(len(r.Evaluations)) + 1, ReleaseVersion: r.RowVersion + 1, Outcome: OutcomeGranted, ExceptionSeq: v.ExceptionSeq,
		Entries: v.Entries, EntriesJSON: doc, EntriesSHA256: sum, Bindings: v.Bindings, Evaluated: act}
	was := r.Status
	r.Status = StatusGranted
	if v.Failure != nil {
		e.Outcome, e.Guard, e.Code, e.Reason, e.ExceptionSeq = OutcomeBlocked, v.Guard, v.Failure.Code, reasonOf(v.Failure), 0
		r.Status = StatusBlocked
	}
	r.RowVersion++
	r.Evaluations = append(r.Evaluations, e)
	return r.Status == StatusGranted && was != StatusGranted
}

// Latest returns the latest evaluation, or nil.
func (r *Release) Latest() *Evaluation {
	if len(r.Evaluations) == 0 {
		return nil
	}
	return &r.Evaluations[len(r.Evaluations)-1]
}

// ExceptionRequest is what a credit manager states for an exception.
type ExceptionRequest struct {
	ExpectedVersion int64
	StageID         string
	Currency        string
	ShortfallMinor  string
	ValidUntil      time.Time
	Reason          string
	Evidence        []string
}

// Exceptable refuses an exception for a release that was never evaluated or is not blocked
// (INVALID_STATE) and one at another version than expected (VERSION_CONFLICT).
func (r *Release) Exceptable(expected int64) error {
	if r.Status != StatusBlocked || r.RowVersion == 0 {
		return Reject(kernel.CodeInvalidState, "/args/scope", "The "+r.Scope+" release is "+r.Status+"; only a blocked release takes an exception")
	}
	if r.RowVersion != expected {
		return Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// GrantException decides commercial_release.grant_exception on what the guards answer in its
// transaction (FUL-INV-002, BT13, SM-FUL-RELEASE blocked → granted): other is the first unmet guard
// other than the payment condition (nil when all are satisfied) and p STL's payment verdict. It
// refuses, in this order: any other unmet guard, with its own code (quality, screening, trade
// compliance, licence, delivery and order are never excepted); a met payment condition
// (INVALID_STATE: evaluate instead); an unmet condition that is not an unpaid stage — no account, a
// pending payment plan review — or more than one unpaid stage (COMMERCIAL_RELEASE_BLOCKED); and a
// request that is not what STL says now: another stage, currency or shortfall, or a validity that
// ended or exceeds MaxExceptionValidity (INVALID_KNOWN_VALUE). It grants the release by the new
// exception. The caller has run Exceptable.
func (r *Release) GrantException(req ExceptionRequest, other *kernel.Rejection, p PaymentVerdict, entries []Entry, act Act) (*Exception, error) {
	if !act.By.IsHuman() {
		return nil, Reject(kernel.CodePermissionDenied, "/", "")
	}
	switch {
	case other != nil:
		return nil, other
	case p.Satisfied:
		return nil, Reject(kernel.CodeInvalidState, "/args/scope", "The payment condition is met: evaluate the release")
	case p.Reason != PaymentStageUnpaid:
		return nil, Reject(shipment.CodeCommercialReleaseBlocked, "/args/scope", p.refusal()+"; an exception covers an unpaid stage only")
	case p.ScopeShortfall().Cmp(minor(p.ShortfallMinor)) != 0:
		return nil, Reject(shipment.CodeCommercialReleaseBlocked, "/args/scope",
			"More than one payment stage is unpaid ("+p.ScopeShortfall().String()+" short in all); an exception covers one stage")
	case req.StageID != p.StageID:
		return nil, Reject(kernel.CodeInvalidKnownValue, "/args/stage_id", "The first unpaid stage is "+p.StageID)
	case req.Currency != p.Currency:
		return nil, Reject(kernel.CodeInvalidKnownValue, "/args/currency", "The order's currency is "+p.Currency)
	case minor(req.ShortfallMinor).Cmp(minor(p.ShortfallMinor)) != 0:
		return nil, Reject(kernel.CodeInvalidKnownValue, "/args/shortfall_minor", "The stage is "+p.ShortfallMinor+" short")
	}
	act.At = kernel.Instant(act.At)
	until := kernel.Instant(req.ValidUntil)
	if !until.After(act.At) || until.Sub(act.At) > MaxExceptionValidity {
		return nil, Reject(kernel.CodeInvalidKnownValue, "/args/valid_until", "An exception is valid from now for at most 90 days")
	}
	doc, sum := shipment.Seal(entries)
	x := Exception{Seq: int64(len(r.Exceptions)) + 1, ReleaseVersion: r.RowVersion + 1, Order: r.Order, StageID: p.StageID, Currency: p.Currency,
		ShortfallMinor: p.ShortfallMinor, ValidUntil: until, Reason: req.Reason, Evidence: req.Evidence, Entries: entries, EntriesJSON: doc,
		EntriesSHA256: sum, Granted: act}
	if x.Evidence == nil {
		x.Evidence = []string{}
	}
	r.Exceptions = append(r.Exceptions, x)
	r.Status, r.RowVersion = StatusGranted, r.RowVersion+1
	return &r.Exceptions[len(r.Exceptions)-1], nil
}

// Consumable refuses a consumed release (INVALID_STATE) and a missing or blocked one
// (COMMERCIAL_RELEASE_BLOCKED) at path. r may be nil.
func (r *Release) Consumable(scope, path string) error {
	switch {
	case r == nil:
		return Reject(shipment.CodeCommercialReleaseBlocked, path, "The shipment has no "+scope+" release: evaluate it")
	case r.Status == StatusConsumed:
		return Reject(kernel.CodeInvalidState, path, "The "+scope+" release is consumed")
	case r.Status != StatusGranted:
		return Reject(shipment.CodeCommercialReleaseBlocked, path, "The "+scope+" release is "+r.Status)
	}
	return nil
}

// Entry is the value the consuming command records for the release.
func (r *Release) Entry(exception int64) Entry {
	v := r.ID + "/v" + strconv.FormatInt(r.RowVersion, 10) + "/" + r.Status
	if exception > 0 {
		v += "/exception=" + strconv.FormatInt(exception, 10)
	}
	return Entry{Name: shipment.GuardCommercialRelease, Value: v}
}

// Consume consumes a granted release (SM-FUL-RELEASE granted → consumed) by shipment.confirm (scope
// shipment) or document_release.record (scope documents, with how the documents were released),
// by a human, citing evidence, with the guard evidence of the consuming transaction. The caller has
// run Consumable and every guard.
func (r *Release) Consume(kind string, evidence []string, entries []Entry, act Act) error {
	if !act.By.IsHuman() {
		return Reject(kernel.CodePermissionDenied, "/", "")
	}
	if err := r.Consumable(r.Scope, "/args/shipment_id"); err != nil {
		return err
	}
	c := &Consumption{Command: CommandConfirmShipment, Evidence: evidence, Entries: entries, Consumed: act}
	if r.Scope == ScopeDocuments {
		if kind != KindOriginalBL && kind != KindTelexRelease {
			return Reject(kernel.CodeSchemaInvalid, "/args/release_kind", "")
		}
		c.Command, c.ReleaseKind = CommandRecordDocumentRelease, kind
	}
	if c.Evidence == nil {
		c.Evidence = []string{}
	}
	c.Consumed.At = kernel.Instant(act.At)
	c.EntriesJSON, c.EntriesSHA256 = shipment.Seal(entries)
	r.Status, r.RowVersion, r.Consumption = StatusConsumed, r.RowVersion+1, c
	return nil
}
