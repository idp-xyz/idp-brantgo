package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The insurance policy record of a shipment document set (WBS S5-16; tactical model
// FUL-AGG-ShipmentDocumentSet entity InsurancePolicyRecord, value object InsuredAmount; FUL-INV-005;
// simulated walkthrough SIM-02 step 8, finding F-03; Context Map R44 PTY->FUL (the insurer's service
// role), R74 EVD->FUL (the policy file), R78 CST->FUL (the costing's insurance component); S5
// decision D-13: record-only). A policy is an external document recorded in the set like a bill of
// lading — its data and an EVD file — as versions of kind insurance_policy, issued with the set
// (shipment_document.issue).
//
//	insurance.record  human or AI (documentation): a new draft version of the set's policy record.
//	                  expected_version is the set's row version (0 creates the set, one per
//	                  shipment). The insurer is a PTY service role of kind insurer serving now; the
//	                  amounts are decimals of the POL currency revision named (at most its minor
//	                  digits); coverage_percent is the contract's percentage (default 110, never
//	                  below: Incoterms 2020 CIF/CIP A5). When the set's current commercial invoice
//	                  states CIF or CIP the insured amount must be at least that percentage of the
//	                  invoice total, rounded HALF_UP to the currency's minor units, in the invoice's
//	                  currency (INSURANCE_COVERAGE_INSUFFICIENT). The premium is compared with the
//	                  insurance component of the costing the order's quote adopted (CST, R78): the
//	                  outcome is kept with the record and shown, never refused.
//
// With the policy record the set's reconciliation and issue change thus: when the current commercial
// invoice states CIF or CIP, a set without a policy has the required difference /insurance_policy,
// which no acknowledgement covers (SHIPMENT_DOCUMENTS_UNRECONCILED); a policy dated after the bill
// of lading's shipped-on-board date is a difference against the B/L record; and the issue, after the
// reconciliation, refuses a current policy that does not cover the current invoice
// (INSURANCE_COVERAGE_INSUFFICIENT at /insurance_policy/insured_amount or /insurance_policy/currency).
//
// Request members in snake_case: insurance.record {document_set_id, shipment_id, expected_version,
// insurer {party_id, service_role_id, revision}, policy_number, clauses [text], issued_on, currency
// {kind, id, revision}, insured_amount, premium, coverage_percent, file_id, evidence_ids}.
const (
	CommandRecordInsurance = "insurance.record"
	CommandGetInsurance    = "insurance.get"

	// DocumentInsurancePolicy is the kind of a policy record's versions in a set.
	DocumentInsurancePolicy = "insurance_policy"
	// ServiceInsurer is the PTY service role kind of an insurer.
	ServiceInsurer = "insurer"
	// DefaultCoveragePercent is the contract percentage when none is stated.
	DefaultCoveragePercent = "110"
	KindCurrency           = "currency"

	// Outcomes of the premium's comparison with the costing (D-13).
	PremiumEqual         = "equal"
	PremiumDifferent     = "different"
	PremiumNotComparable = "not_comparable"
	PremiumNoComponent   = "no_component"
	PremiumUnavailable   = "unavailable"

	MaxClauses = 20
	MaxClause  = 200
)

var (
	amountFormat  = kernel.DecimalFormat{IntDigits: 18, Scale: 4}
	percentFormat = kernel.DecimalFormat{IntDigits: 4, Scale: 2}
)

// RecordInsurance records a new draft version of the insurance policy record of set DocumentSetID
// of shipment ShipmentID at the set's row version ExpectedVersion (0: the set is created). Human or
// AI.
type RecordInsurance struct {
	IdempotencyKey  string
	DocumentSetID   string
	ShipmentID      string
	ExpectedVersion int64
	Insurer         ServiceRoleRef // PTY ServiceRoleRef v1 of kind insurer
	PolicyNumber    string
	Clauses         []string // the risks covered, such as "ICC (A)"
	IssuedOn        string   // YYYY-MM-DD
	Currency        ConfigRef
	InsuredAmount   string
	Premium         string
	CoveragePercent string // "": DefaultCoveragePercent
	FileID          string // the policy, an EVD file
	EvidenceIDs     []string
}

func (RecordInsurance) fulCommand() {}

// Validate checks the closed contract: the set, shipment and insurer references, a policy number,
// 1 to MaxClauses distinct clauses, the date of issue, a POL currency revision, a positive insured
// amount and a premium of zero or more as decimals, the contract percentage as a decimal of at most
// two places, the policy's file and at most MaxEvidence sources.
func (c RecordInsurance) Validate() error {
	if err := head(c.IdempotencyKey, c.ShipmentID); err != nil {
		return err
	}
	switch {
	case !kernel.ValidIdentifier(c.DocumentSetID):
		return schema("/args/document_set_id")
	case c.ExpectedVersion < 0 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.Insurer.PartyID):
		return schema("/args/insurer/party_id")
	case !kernel.ValidIdentifier(c.Insurer.ServiceRoleID):
		return schema("/args/insurer/service_role_id")
	case c.Insurer.Revision < 1 || c.Insurer.Revision > maxVersion:
		return schema("/args/insurer/revision")
	case !documentNumber(c.PolicyNumber):
		return schema("/args/policy_number")
	case len(c.Clauses) < 1 || len(c.Clauses) > MaxClauses:
		return schema("/args/clauses")
	case !kernel.ValidIdentifier(c.FileID):
		return schema("/args/file_id")
	}
	seen := map[string]bool{}
	for i, x := range c.Clauses {
		if !Line(x, MaxClause) || seen[x] {
			return schema(index("/args/clauses", i))
		}
		seen[x] = true
	}
	if _, ok := PlannedOn(c.IssuedOn); !ok {
		return schema("/args/issued_on")
	}
	if err := c.Currency.validate(KindCurrency, "/args/currency"); err != nil {
		return err
	}
	if v, err := kernel.ParseDecimal(c.InsuredAmount, amountFormat); err != nil || v.Sign() <= 0 {
		return schema("/args/insured_amount")
	}
	if _, err := kernel.ParseDecimal(c.Premium, amountFormat); err != nil {
		return schema("/args/premium")
	}
	if c.CoveragePercent != "" {
		if _, err := kernel.ParseDecimal(c.CoveragePercent, percentFormat); err != nil {
			return schema("/args/coverage_percent")
		}
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// InsuranceCoverage is FUL-INV-005 applied to a policy and a commercial invoice version of CIF or
// CIP: the invoice's total, the percentage, the amount required (HALF_UP to the currency's minor
// units), the amount insured and whether it covers. Amounts are decimals of their currency.
type InsuranceCoverage struct {
	Incoterm        string
	InvoiceVersion  int64
	InvoiceNumber   string
	InvoiceCurrency string
	InvoiceAmount   string
	Percent         string
	RequiredAmount  string
	InsuredCurrency string
	InsuredAmount   string
	Sufficient      bool
}

// PremiumComparison is a policy's premium against the insurance component of the costing the
// order's quote adopted (R78, D-13): the outcome (PremiumEqual, PremiumDifferent,
// PremiumNotComparable, PremiumNoComponent, PremiumUnavailable), the cost snapshot, its currency,
// the component's amount and IDs, the premium and the difference premium − component (when the
// currencies match), and why there is nothing to compare when there is not.
type PremiumComparison struct {
	Status          string
	SnapshotID      string
	ResultHash      string
	Currency        string
	ComponentAmount string
	Components      []string
	PremiumCurrency string
	Premium         string
	Difference      string
	Reason          string
}

// InsuranceReceipt answers insurance.record: the receipt (ObjectID the set, RowVersion the set's,
// Status draft), the version recorded, the cover it was checked on (nil when the set's commercial
// invoice was not of CIF or CIP, or there was none) and the premium's comparison.
type InsuranceReceipt struct {
	Receipt
	Version  int64
	Coverage *InsuranceCoverage
	Costing  PremiumComparison
}

// InsurancePolicyView is one version of a set's policy record: its state, what the policy states,
// the cover and premium comparison found when it was recorded, the EVD file, who recorded it and its
// commit evidence, and the issue that issued it.
type InsurancePolicyView struct {
	Version         int64
	Status          string
	SetVersion      int64
	Insurer         ServiceRoleRef
	PolicyNumber    string
	Clauses         []string
	IssuedOn        string
	Currency        ConfigRef
	InsuredAmount   string
	Premium         string
	CoveragePercent string
	Coverage        *InsuranceCoverage
	Costing         PremiumComparison
	ContentSHA256   string
	FileID          string
	FileSHA256      string
	FileMimeType    string
	EvidenceIDs     []string
	Sources         []Entry
	RecordedBy      kernel.ActorRef
	RecordedAt      time.Time
	AuthorityEpoch  int64
	Entries         []Entry
	EntriesSHA256   string
	IssueSeq        int64
	IssuedAt        time.Time
	SupersededAt    time.Time
}

// InsuranceView is the insurance of a shipment's document set now: the Incoterm of the commercial
// invoice the set stands on, whether it needs a policy (CIF, CIP), the version the set stands on
// (its draft, else its issued one), FUL-INV-005 applied to it and that invoice now (nil when not
// required or no policy), and every version, oldest first.
type InsuranceView struct {
	DocumentSetID string
	ShipmentID    string
	RowVersion    int64
	Incoterm      string
	Required      bool
	Current       *InsurancePolicyView
	Coverage      *InsuranceCoverage
	Versions      []InsurancePolicyView
	CheckedAt     time.Time
}

// InsuranceCommands is FUL's command entry point for insurance policy records. Errors are
// *kernel.Rejection for refusals.
type InsuranceCommands interface {
	RecordInsurance(ctx context.Context, cmd RecordInsurance) (InsuranceReceipt, error)
}

// InsuranceQueries reads the insurance of document sets.
type InsuranceQueries interface {
	// GetInsurance returns the insurance of a set the caller sees, audited as insurance.get
	// (ful.read): NOT_FOUND otherwise.
	GetInsurance(ctx context.Context, setID string) (InsuranceView, error)
}
