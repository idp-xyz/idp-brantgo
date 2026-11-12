package api

import (
	"context"
	"regexp"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The commercial release (S5 unit C1, WBS S5-07; tactical model FUL-AGG-CommercialRelease,
// SM-FUL-RELEASE, FUL-INV-002; WF-07, ES-10, SC-08, BT13; S5 decision D-11). One release root per
// shipment and scope — shipment (the actual shipment) or documents (the original bill of lading or
// the telex release) — records the decision of every evaluation, the payment-condition exceptions
// and its consumption.
//
//	commercial_release.evaluate       human, AI or system (documentation): in FUL's transaction, with
//	                                  the shipment, the order's fulfilment root and the release locked,
//	                                  runs every guard in context code order — CNX TradeComplianceGuard
//	                                  on the order revision the shipment binds and, when its conclusion
//	                                  says a licence is required, ExportLicenseGuard on the shipment's
//	                                  lines; PTY PartyScreeningGuard on the order's parties; PUR
//	                                  SupplierDeliveryGuard; QLT QualityReleaseGuard; STL
//	                                  PaymentConditionGuard for the scope; TRD OrderGuard; then FUL's
//	                                  own quantity rules — and records granted, binding what each guard
//	                                  answered, or blocked with the first unmet guard, its code and why.
//	                                  Both are committed outcomes. A consumed release is INVALID_STATE.
//	commercial_release.grant_exception human only (credit manager, exclusive): grants a blocked release
//	                                  whose only unmet guard is the payment condition, in this
//	                                  transaction, for one unpaid stage: bound to the order revision,
//	                                  the shipment, the scope, the stage, the currency and the shortfall
//	                                  in minor units STL states now, until valid_until. Quality,
//	                                  screening, trade compliance, licence, delivery and order guards are
//	                                  never excepted: their own code refuses the exception.
//	document_release.record           human only (documentation): records the release of the original
//	                                  bill of lading or a telex release of a confirmed shipment, with
//	                                  evidence. It needs a granted documents release and re-runs every
//	                                  guard in its own transaction; it consumes the release. Since S5-08
//	                                  it is recorded on the issued bill of lading record of the
//	                                  shipment's document set (SHIPMENT_DOCUMENTS_UNRECONCILED without
//	                                  one), which the consumption names (ConsumptionView.BillOfLading).
//
// shipment.confirm needs a granted shipment release, re-runs every guard and consumes it. A refused
// confirmation or document release leaves the release as it was.
//
// Request members in snake_case: commercial_release.evaluate {shipment_id, scope};
// commercial_release.grant_exception {shipment_id, scope, expected_version, stage_id, currency,
// shortfall_minor, valid_until, reason, evidence_ids}; document_release.record {shipment_id,
// expected_version, release_kind, evidence_ids}.

// Commands, reads, scopes, states and events of the commercial release.
const (
	CommandEvaluateRelease        = "commercial_release.evaluate"
	CommandGrantReleaseException  = "commercial_release.grant_exception"
	CommandRecordDocumentRelease  = "document_release.record"
	CommandGetRelease             = "commercial_release.get"
	CommandListShipmentReleases   = "commercial_release.list"
	EventCommercialReleaseGranted = "idp-brant.ful.commercial_release_granted.v1"
	EventDocumentsReleased        = "idp-brant.ful.documents_released.v1"

	// Scopes of a release (ReleaseScope).
	ScopeShipment  = "shipment"
	ScopeDocuments = "documents"

	// States of a release (SM-FUL-RELEASE).
	ReleaseBlocked  = "blocked"
	ReleaseGranted  = "granted"
	ReleaseConsumed = "consumed"

	// How the documents were released.
	ReleaseOriginalBL   = "original_bl"
	ReleaseTelexRelease = "telex_release"

	// GuardFulfilment names FUL's own quantity rules among a release's guards (FUL-INV-001,
	// FUL-INV-003 and the release consumption).
	GuardFulfilment = "ful.fulfilment"

	// MaxExceptionDays bounds an exception's validity.
	MaxExceptionDays = 90
)

var minorPattern = regexp.MustCompile(`^[1-9][0-9]{0,23}$`)
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// EvaluateRelease evaluates the release of shipment ShipmentID for Scope. Human, AI or system.
type EvaluateRelease struct {
	IdempotencyKey string
	ShipmentID     string
	Scope          string
}

// GrantReleaseException grants the blocked release of shipment ShipmentID for Scope at
// ExpectedVersion by an exception for payment stage StageID, short ShortfallMinor (integer minor
// units of Currency, as STL's PaymentConditionGuard states them), until ValidUntil (a UTC timestamp,
// at most MaxExceptionDays ahead), for Reason, citing EvidenceIDs. Human only.
type GrantReleaseException struct {
	IdempotencyKey  string
	ShipmentID      string
	Scope           string
	ExpectedVersion int64
	StageID         string
	Currency        string
	ShortfallMinor  string
	ValidUntil      string
	Reason          string
	EvidenceIDs     []string
}

// RecordDocumentRelease records the release of the documents of confirmed shipment ShipmentID by
// ReleaseKind (original_bl or telex_release) on its documents release at ExpectedVersion, citing
// EvidenceIDs (1 to MaxEvidence EVD sources: the B/L release or the telex release message). Human
// only.
type RecordDocumentRelease struct {
	IdempotencyKey  string
	ShipmentID      string
	ExpectedVersion int64
	ReleaseKind     string
	EvidenceIDs     []string
}

func (EvaluateRelease) fulCommand()       {}
func (GrantReleaseException) fulCommand() {}
func (RecordDocumentRelease) fulCommand() {}

func releaseScope(s string) error {
	if s != ScopeShipment && s != ScopeDocuments {
		return schema("/args/scope")
	}
	return nil
}

// Validate checks the closed contract.
func (c EvaluateRelease) Validate() error {
	if err := head(c.IdempotencyKey, c.ShipmentID); err != nil {
		return err
	}
	return releaseScope(c.Scope)
}

// Validate checks the closed contract: a known scope, a stage, a currency code, a positive integer
// shortfall in minor units, a UTC timestamp, a reason and the evidence.
func (c GrantReleaseException) Validate() error {
	if err := head(c.IdempotencyKey, c.ShipmentID); err != nil {
		return err
	}
	if err := releaseScope(c.Scope); err != nil {
		return err
	}
	if err := version(c.ExpectedVersion); err != nil {
		return err
	}
	switch {
	case !kernel.ValidIdentifier(c.StageID):
		return schema("/args/stage_id")
	case !currencyPattern.MatchString(c.Currency):
		return schema("/args/currency")
	case !minorPattern.MatchString(c.ShortfallMinor):
		return schema("/args/shortfall_minor")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	if _, err := kernel.ParseTimestamp(c.ValidUntil); err != nil {
		return schema("/args/valid_until")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// Validate checks the closed contract: a known release kind and 1 to MaxEvidence sources.
func (c RecordDocumentRelease) Validate() error {
	if err := head(c.IdempotencyKey, c.ShipmentID); err != nil {
		return err
	}
	if err := version(c.ExpectedVersion); err != nil {
		return err
	}
	switch {
	case c.ReleaseKind != ReleaseOriginalBL && c.ReleaseKind != ReleaseTelexRelease:
		return schema("/args/release_kind")
	case len(c.EvidenceIDs) == 0:
		return schema("/args/evidence_ids")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// ReleaseReceipt answers every release command: the receipt (ObjectID the release, Status its
// state), the shipment and scope, and for an evaluation that blocked it the first unmet guard, its
// code and why.
type ReleaseReceipt struct {
	Receipt
	ReleaseID  string
	ShipmentID string
	Scope      string
	Guard      string
	Code       string
	Reason     string
}

// ComplianceBinding is the trade compliance conclusion a release saw (TradeComplianceRef).
type ComplianceBinding struct {
	Status             string    `json:"status"`
	CheckID            string    `json:"check_id"`
	DecisionID         string    `json:"decision_id"`
	LicenseRequired    bool      `json:"license_required"`
	RegulationID       string    `json:"regulation_id"`
	RegulationRevision int64     `json:"regulation_revision"`
	ValidUntil         time.Time `json:"valid_until"`
}

// LicenseBinding is the export licence allocation a release saw (LicenseAllocationRef).
type LicenseBinding struct {
	Status       string `json:"status"`
	LicenseID    string `json:"license_id"`
	AllocationID string `json:"allocation_id"`
	LinesHash    string `json:"lines_hash"`
	Quantity     string `json:"quantity"`
	Unit         string `json:"unit"`
}

// ScreeningBinding is a party screening a release saw (ScreeningRef).
type ScreeningBinding struct {
	PartyID       string `json:"party_id"`
	Status        string `json:"status"`
	ScreeningID   string `json:"screening_id"`
	PartyRevision int64  `json:"party_revision"`
}

// DeliveryBinding is a supplier delivery line a release saw at the version the shipment binds.
type DeliveryBinding struct {
	DeliveryID      string `json:"delivery_id"`
	LineID          string `json:"line_id"`
	Version         int64  `json:"version"`
	Status          string `json:"status"`
	VerifiedVersion int64  `json:"verified_version"`
	Verified        string `json:"verified_quantity"`
}

// QualityBinding is a quality release a release saw (QualityReleaseRef).
type QualityBinding struct {
	ReleaseID  string    `json:"release_id"`
	Status     string    `json:"status"`
	Released   string    `json:"released_quantity"`
	ValidUntil time.Time `json:"valid_until"`
}

// PaymentBinding is the payment condition a release saw for its scope (PaymentConditionRef):
// amounts in minor units of Currency, the first unpaid stage when blocked, and the exception that
// covered it.
type PaymentBinding struct {
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

// OrderBindingSeen is the order as TRD's OrderGuard answered.
type OrderBindingSeen struct {
	OrderID       string `json:"order_id"`
	OrderRevision int64  `json:"order_revision"`
	OrderHash     string `json:"order_hash"`
	Status        string `json:"status"`
}

// ReleaseBindings is what an evaluation bound of each guard it ran; a guard it did not reach is
// absent (nil or empty).
type ReleaseBindings struct {
	TradeCompliance *ComplianceBinding `json:"trade_compliance,omitempty"`
	ExportLicense   *LicenseBinding    `json:"export_license,omitempty"`
	Screenings      []ScreeningBinding `json:"screenings"`
	Deliveries      []DeliveryBinding  `json:"supplier_deliveries"`
	QualityReleases []QualityBinding   `json:"quality_releases"`
	Payment         *PaymentBinding    `json:"payment_condition,omitempty"`
	Order           *OrderBindingSeen  `json:"order,omitempty"`
}

// EvaluationView is one evaluation of a release.
type EvaluationView struct {
	Seq            int64
	ReleaseVersion int64
	Outcome        string
	Guard          string
	Code           string
	Reason         string
	ExceptionSeq   int64
	Guards         []GuardEvidence
	GuardsSHA256   string
	Bindings       ReleaseBindings
	EvaluatedBy    kernel.ActorRef
	EvaluatedAt    time.Time
	AuthorityEpoch int64
}

// ExceptionView is a payment-condition exception of a release.
type ExceptionView struct {
	Seq            int64
	ReleaseVersion int64
	Order          OrderBinding
	StageID        string
	Currency       string
	ShortfallMinor string
	ValidUntil     time.Time
	Reason         string
	EvidenceIDs    []string
	Guards         []GuardEvidence
	GuardsSHA256   string
	GrantedBy      kernel.ActorRef
	GrantedAt      time.Time
	AuthorityEpoch int64
}

// ConsumptionView is what consumed a release: shipment.confirm, or document_release.record with
// how the documents were released.
type ConsumptionView struct {
	Command        string
	ReleaseKind    string
	BillOfLading   *BillOfLadingRef // the issued B/L record a documents release was recorded on (S5-08)
	EvidenceIDs    []string
	Guards         []GuardEvidence
	GuardsSHA256   string
	ConsumedBy     kernel.ActorRef
	ConsumedAt     time.Time
	AuthorityEpoch int64
}

// ReleaseView is a commercial release as the reads show it.
type ReleaseView struct {
	ReleaseID   string
	ShipmentID  string
	Scope       string
	Status      string
	RowVersion  int64
	Order       OrderBinding
	Evaluations []EvaluationView
	Exceptions  []ExceptionView
	Consumption *ConsumptionView
}

// ShipmentReleases is the releases of a shipment, one per scope at most.
type ShipmentReleases struct {
	ShipmentID string
	Releases   []ReleaseView
}

// ReleaseCommands is FUL's commercial release entry point (errors as for Commands).
type ReleaseCommands interface {
	EvaluateRelease(ctx context.Context, cmd EvaluateRelease) (ReleaseReceipt, error)
	GrantReleaseException(ctx context.Context, cmd GrantReleaseException) (ReleaseReceipt, error)
	RecordDocumentRelease(ctx context.Context, cmd RecordDocumentRelease) (ReleaseReceipt, error)
}

// ReleaseQueries is FUL's audited read side of the commercial release (ful.read).
type ReleaseQueries interface {
	// GetRelease returns a release of a shipment the caller sees, audited as commercial_release.get:
	// NOT_FOUND otherwise.
	GetRelease(ctx context.Context, releaseID string) (ReleaseView, error)
	// ShipmentReleases returns the releases of a shipment the caller sees, audited as
	// commercial_release.list: NOT_FOUND otherwise.
	ShipmentReleases(ctx context.Context, shipmentID string) (ShipmentReleases, error)
}
