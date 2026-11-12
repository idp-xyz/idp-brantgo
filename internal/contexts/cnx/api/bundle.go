package api

import (
	"context"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Document bundles and external reconciliation (WBS S5-05a; tactical model CNX-AGG-DocumentBundle
// with RequirementCheck and ExternalReconciliation, CNX-INV-008; baseline 22.1, CN-R11, CN-R13;
// CN-AT14 bundle half; S5 decisions E-1, D-17).
//
//	document_bundle.check            human or AI (documentation): states the whole requirement
//	                                 checklist of export lot export_lot_id — each requirement's
//	                                 requirement_id, kind, version, status missing|present|verified,
//	                                 owner {kind actor|party|role, id}, due_on (the due date the
//	                                 applicable policy gives, never derived from an upload date;
//	                                 required unless verified), evidence_ids (the attachments: none
//	                                 when missing, at least one otherwise), the declaration (CNX) of a
//	                                 customs_declaration requirement, the STL fund records of a
//	                                 fund_record requirement (FundRecordRef v1, R38, linked as a
//	                                 commercial allocation, never 出口收汇核销: CN-R11), and for verified
//	                                 the basis of the verification — at expected_version (0 opens the
//	                                 lot's bundle). Only a human verifies (PERMISSION_DENIED); presence
//	                                 is not validity: expect_complete with a requirement that is only
//	                                 present or missing, or a customs declaration stated verified that
//	                                 customs has not released, is DOCUMENT_REQUIREMENT_UNVERIFIED.
//	external_reconciliation.resolve  human only (tax specialist): records reconciliation_id on bundle_id
//	                                 at expected_version — the subject {kind export_lot|declaration|
//	                                 tax_refund_case|fund_record|document, id}, the measure (amount in a
//	                                 POL currency revision, or quantity in a unit), the external value
//	                                 and where it comes from with its evidence, the internal value and
//	                                 where it comes from, their exact difference (external − internal),
//	                                 the responsibility and the adjustment (none, or approved with what
//	                                 was approved). Brant computes no tax, duty or refund (CN-E01).
//
// Request members in snake_case: document_bundle.check {export_lot_id, expected_version,
// requirements [{requirement_id, kind, version, status, owner {kind, id}, due_on, evidence_ids,
// declaration_id, fund_record_ids, basis, note}], expect_complete}; external_reconciliation.resolve
// {bundle_id, expected_version, reconciliation_id, subject {kind, id}, measure, currency {id,
// revision}, unit, external {value, source, evidence_ids}, internal {value, source}, responsibility
// {kind, party_id, note}, adjustment {kind, note}, reason}.
const (
	CommandCheckBundle           = "document_bundle.check"
	CommandResolveReconciliation = "external_reconciliation.resolve"

	// Reads: a bundle with its checks and reconciliations, the company's bundles.
	CommandGetBundle = "document_bundle.get"
	CommandBundles   = "document_bundle.list"

	// DocumentBundleChecked (Context Map CNX facts): every check.
	EventDocumentBundleChecked = "idp-brant.cnx.document_bundle_checked.v1"
)

// Requirement kinds and statuses, bundle states, owner kinds, reconciliation subjects, measures and
// adjustments, and the label of fund records in a bundle.
const (
	RequirementCommercialInvoice     = "commercial_invoice"
	RequirementPackingList           = "packing_list"
	RequirementBillOfLading          = "bill_of_lading"
	RequirementCustomsDeclaration    = "customs_declaration"
	RequirementCertificateOfOrigin   = "certificate_of_origin"
	RequirementInsurance             = "insurance"
	RequirementInspectionCertificate = "inspection_certificate"
	RequirementExportLicense         = "export_license"
	RequirementPurchaseInvoice       = "purchase_invoice"
	RequirementTaxRefund             = "tax_refund"
	RequirementFundRecord            = "fund_record"
	RequirementOther                 = "other"

	RequirementMissing  = "missing"
	RequirementPresent  = "present"
	RequirementVerified = "verified"

	BundleIncomplete = "incomplete"
	BundleComplete   = "complete"

	OwnerActor = "actor"
	OwnerParty = "party"
	OwnerRole  = "role"

	SubjectExportLot     = "export_lot"
	SubjectDeclaration   = "declaration"
	SubjectTaxRefundCase = "tax_refund_case"
	SubjectFundRecord    = "fund_record"
	SubjectDocument      = "document"

	MeasureAmount   = "amount"
	MeasureQuantity = "quantity"

	AdjustmentNone     = "none"
	AdjustmentApproved = "approved"

	CommercialAllocation = "commercial_allocation"
)

// Limits of the bundle contract.
const (
	MaxRequirements    = 50
	MaxFundRecords     = 20
	MaxRequirementText = 1000
)

var (
	requirementKinds = []string{RequirementCommercialInvoice, RequirementPackingList, RequirementBillOfLading, RequirementCustomsDeclaration,
		RequirementCertificateOfOrigin, RequirementInsurance, RequirementInspectionCertificate, RequirementExportLicense,
		RequirementPurchaseInvoice, RequirementTaxRefund, RequirementFundRecord, RequirementOther}
	subjectKinds     = []string{SubjectExportLot, SubjectDeclaration, SubjectTaxRefundCase, SubjectFundRecord, SubjectDocument}
	responsibilities = []string{"none", "internal", "customer", "supplier", "broker", "carrier", "authority", "bank", "other"}
)

// RequirementOwner is who answers for a requirement.
type RequirementOwner struct {
	Kind string
	ID   string
}

// Requirement is one entry of a checklist.
type Requirement struct {
	RequirementID string
	Kind          string
	Version       string
	Status        string
	Owner         RequirementOwner
	DueOn         string
	EvidenceIDs   []string
	DeclarationID string
	FundRecordIDs []string
	Basis         string
	Note          string
}

// CheckBundle states the whole checklist of export lot ExportLotID's bundle at ExpectedVersion (0
// opens it).
type CheckBundle struct {
	IdempotencyKey  string
	ExportLotID     string
	ExpectedVersion int64
	Requirements    []Requirement
	ExpectComplete  bool
}

func date(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// Validate checks the closed contract.
func (c CheckBundle) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ExportLotID):
		return schema("/args/export_lot_id")
	case c.ExpectedVersion < 0 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case len(c.Requirements) < 1 || len(c.Requirements) > MaxRequirements:
		return schema("/args/requirements")
	}
	ids := map[string]bool{}
	for i, r := range c.Requirements {
		p := "/args/requirements/" + strconv.Itoa(i)
		present := r.Status == RequirementPresent || r.Status == RequirementVerified
		switch {
		case !kernel.ValidIdentifier(r.RequirementID) || ids[r.RequirementID]:
			return schema(p + "/requirement_id")
		case !contains(requirementKinds, r.Kind):
			return schema(p + "/kind")
		case !Text(r.Version, MaxDeclarationRef):
			return schema(p + "/version")
		case r.Status != RequirementMissing && !present:
			return schema(p + "/status")
		case r.Owner.Kind != OwnerActor && r.Owner.Kind != OwnerParty && r.Owner.Kind != OwnerRole:
			return schema(p + "/owner/kind")
		case !kernel.ValidIdentifier(r.Owner.ID):
			return schema(p + "/owner/id")
		case r.DueOn == "" && r.Status != RequirementVerified, r.DueOn != "" && !date(r.DueOn):
			return schema(p + "/due_on")
		case (len(r.EvidenceIDs) > 0) != present:
			return schema(p + "/evidence_ids")
		case r.Kind != RequirementCustomsDeclaration && r.DeclarationID != "",
			r.Kind == RequirementCustomsDeclaration && present != (r.DeclarationID != ""),
			r.DeclarationID != "" && !kernel.ValidIdentifier(r.DeclarationID):
			return schema(p + "/declaration_id")
		case r.Kind != RequirementFundRecord && len(r.FundRecordIDs) > 0,
			r.Kind == RequirementFundRecord && present != (len(r.FundRecordIDs) > 0), len(r.FundRecordIDs) > MaxFundRecords:
			return schema(p + "/fund_record_ids")
		case (r.Status == RequirementVerified) != (r.Basis != ""), r.Basis != "" && !Text(r.Basis, MaxRequirementText):
			return schema(p + "/basis")
		case r.Note != "" && !Text(r.Note, MaxRequirementText):
			return schema(p + "/note")
		}
		ids[r.RequirementID] = true
		if err := evidence(r.EvidenceIDs, p+"/evidence_ids", 0); err != nil {
			return err
		}
		seen := map[string]bool{}
		for j, f := range r.FundRecordIDs {
			if !kernel.ValidIdentifier(f) || seen[f] {
				return schema(p + "/fund_record_ids/" + strconv.Itoa(j))
			}
			seen[f] = true
		}
	}
	return nil
}

// ReconciliationSubject is what a reconciliation concerns.
type ReconciliationSubject struct {
	Kind string
	ID   string
}

// ReconciledValue is a value and where it comes from (an external one also with its evidence).
type ReconciledValue struct {
	Value       string
	Source      string
	EvidenceIDs []string
}

// Responsibility is who answers for a difference: none, internal, customer, supplier, broker,
// carrier, authority, bank or other, optionally a PTY party, and a note.
type Responsibility struct {
	Kind    string
	PartyID string
	Note    string
}

// Adjustment is none, or approved with what was approved.
type Adjustment struct {
	Kind string
	Note string
}

// ResolveReconciliation records reconciliation ReconciliationID on bundle BundleID at
// ExpectedVersion (human only).
type ResolveReconciliation struct {
	IdempotencyKey   string
	BundleID         string
	ExpectedVersion  int64
	ReconciliationID string
	Subject          ReconciliationSubject
	Measure          string
	Currency         *CurrencyRef
	Unit             string
	External         ReconciledValue
	Internal         ReconciledValue
	Responsibility   Responsibility
	Adjustment       Adjustment
	Reason           string
}

// Validate checks the closed contract.
func (c ResolveReconciliation) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.BundleID):
		return schema("/args/bundle_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.ReconciliationID):
		return schema("/args/reconciliation_id")
	case !contains(subjectKinds, c.Subject.Kind):
		return schema("/args/subject/kind")
	case !kernel.ValidIdentifier(c.Subject.ID):
		return schema("/args/subject/id")
	case c.Measure != MeasureAmount && c.Measure != MeasureQuantity:
		return schema("/args/measure")
	case (c.Measure == MeasureAmount) != (c.Currency != nil):
		return schema("/args/currency")
	case c.Currency != nil && (!kernel.ValidIdentifier(c.Currency.ID) || c.Currency.Revision < 1 || c.Currency.Revision > maxVersion):
		return schema("/args/currency")
	case (c.Measure == MeasureQuantity) != (c.Unit != ""), c.Unit != "" && !kernel.UnitCode(c.Unit).Valid():
		return schema("/args/unit")
	}
	format := valueFormat
	if c.Measure == MeasureQuantity {
		format = kernel.QuantityFormat
	}
	switch {
	case !positive(c.External.Value, format, true):
		return schema("/args/external/value")
	case !Text(c.External.Source, MaxRequirementText):
		return schema("/args/external/source")
	case !positive(c.Internal.Value, format, true):
		return schema("/args/internal/value")
	case !Text(c.Internal.Source, MaxRequirementText):
		return schema("/args/internal/source")
	case len(c.Internal.EvidenceIDs) > 0:
		return schema("/args/internal/evidence_ids")
	case !contains(responsibilities, c.Responsibility.Kind):
		return schema("/args/responsibility/kind")
	case c.Responsibility.PartyID != "" && !kernel.ValidIdentifier(c.Responsibility.PartyID):
		return schema("/args/responsibility/party_id")
	case !Text(c.Responsibility.Note, MaxRequirementText):
		return schema("/args/responsibility/note")
	case c.Adjustment.Kind != AdjustmentNone && c.Adjustment.Kind != AdjustmentApproved:
		return schema("/args/adjustment/kind")
	case (c.Adjustment.Kind == AdjustmentApproved) != (c.Adjustment.Note != ""), c.Adjustment.Note != "" && !Text(c.Adjustment.Note, MaxRequirementText):
		return schema("/args/adjustment/note")
	case !Text(c.Reason, MaxReason):
		return schema("/args/reason")
	}
	return evidence(c.External.EvidenceIDs, "/args/external/evidence_ids", 1)
}

func (CheckBundle) cnxCommand()           {}
func (ResolveReconciliation) cnxCommand() {}

// BundleReceipt answers the bundle commands: the receipt (ObjectID the bundle ID, Status
// incomplete|complete) and the bundle as stored now.
type BundleReceipt struct {
	Receipt
	BundleID         string
	ExportLotID      string
	CheckNo          int
	Missing          []string
	Unverified       []string
	ReconciliationID string
}

// BundleCommands is CNX's command entry point for document bundles.
type BundleCommands interface {
	CheckBundle(ctx context.Context, cmd CheckBundle) (BundleReceipt, error)
	ResolveReconciliation(ctx context.Context, cmd ResolveReconciliation) (BundleReceipt, error)
}

// DeclarationLink is the declaration a customs_declaration requirement rests on, as CNX held it at
// the check.
type DeclarationLink struct {
	DeclarationID     string
	Status            string
	DeclarationNumber string
	ExportedAt        string
	CustomsReleased   bool
}

// FundLink is an STL fund record a fund_record requirement links (R38), labelled
// commercial_allocation: STL's applications are a commercial allocation, never 出口收汇核销 (CN-R11).
type FundLink struct {
	FundRecordID  string
	Kind          string
	Direction     string
	Currency      string
	ReceivedMinor string
	Label         string
}

// RequirementView is a requirement as a check stated it.
type RequirementView struct {
	RequirementID string
	Kind          string
	Version       string
	Status        string
	Owner         RequirementOwner
	DueOn         string
	Evidence      []EvidenceView
	Declaration   *DeclarationLink
	FundRecords   []FundLink
	Basis         string
	Note          string
}

// BundleCheckView is a check: the checklist, its counts and who checked when.
type BundleCheckView struct {
	CheckNo        int
	DocumentSHA256 string
	Status         string
	Missing        int
	Present        int
	Verified       int
	Requirements   []RequirementView
	CheckedBy      kernel.ActorRef
	AuthorityEpoch int64
	CheckedAt      time.Time
}

// ReconciliationView is an external reconciliation.
type ReconciliationView struct {
	ReconciliationID string
	ReconciliationNo int
	Subject          ReconciliationSubject
	Measure          string
	Unit             string
	ExternalValue    string
	ExternalSource   string
	ExternalEvidence []EvidenceView
	InternalValue    string
	InternalSource   string
	Difference       string
	Responsibility   Responsibility
	Adjustment       Adjustment
	Reason           string
	ResolvedBy       kernel.ActorRef
	AuthorityEpoch   int64
	ResolvedAt       time.Time
}

// BundleView is a bundle: its lot, state, the current checklist (the latest check's), every check
// and every reconciliation.
type BundleView struct {
	BundleID        string
	ExportLotID     string
	OrderID         string
	Status          string
	RowVersion      int64
	Requirements    []RequirementView
	Checks          []BundleCheckView
	Reconciliations []ReconciliationView
	OpenedBy        kernel.ActorRef
	OpenedAt        time.Time
}

// BundleSummary is one bundle of the company's.
type BundleSummary struct {
	BundleID        string
	ExportLotID     string
	Status          string
	RowVersion      int64
	Checks          int
	Reconciliations int
	OpenedAt        time.Time
}

// BundleQueries is the audited read of document bundles (cnx.read).
type BundleQueries interface {
	// GetBundle returns a bundle, audited as document_bundle.get: NOT_FOUND otherwise; a check
	// document that does not match its hash is REFERENCE_INVALID.
	GetBundle(ctx context.Context, bundleID string) (BundleView, error)
	// Bundles returns the company's bundles, newest first, audited as document_bundle.list.
	Bundles(ctx context.Context) ([]BundleSummary, error)
}
