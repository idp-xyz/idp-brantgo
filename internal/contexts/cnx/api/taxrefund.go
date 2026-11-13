package api

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Tax refund cases (WBS S5-05b; tactical model CNX-AGG-TaxRefundCase with its TaxEvidenceAllocation
// entries and the value objects PolicyVersion and RefundStage, SM-CNX-TAXCASE, CNX-INV-004; UL
// CNX-TaxRefundCase; baseline 22.1, 22.2, CN-R04, CN-R08, CN-R09, CN-E01, CN-AT04, CN-AT10, CN-AT12;
// S5 decisions E-1, D-3, D-17). A case is the refund material of one FUL export lot: which purchase
// invoice lines evidence which export lines, for which purpose, and the external results of the
// refund declaration as people record them. Brant computes no tax and no refund (CN-E01): every
// amount is a person's statement of an estimate or of an external result.
//
//	tax_refund_case.allocate       human or AI (tax specialist): allocations of export lines of
//	                               export_lot_id to verified purchase invoice lines, each with a
//	                               purpose and a quantity in the invoice line's unit. The lot's
//	                               first allocation opens its case, which binds the export_tax_policy
//	                               revision of policy_id whose validity contains the lot's export date,
//	                               whenever it was published (POL ConfigAt v1, S5R-D23, CN-AT12); a taxable
//	                               revision opens no case (INVALID_STATE, S5R-D14). Per purpose, Σ standing
//	                               allocations of an invoice line never exceed what it evidences (its
//	                               quantity less what red invoices reversed, S5R-D20), nor those of an
//	                               export line its quantity (TAX_EVIDENCE_EXCEEDED; CN-R04). An allocation
//	                               from an invoice a red invoice reversed, or of a line whose evidence a
//	                               partial reversal left over-used, is under review; a new allocation of
//	                               the same export line replaces it (replaces), from the same line too.
//	tax_refund_case.record_result  human only (tax specialist), at expected_version, one stage of
//	                               SM-CNX-TAXCASE, kept apart (CN-R08): expected (the estimate,
//	                               stays expected), planned (拟申报: amount, tax_period and the
//	                               professional review), submitted (external_reference), approved
//	                               (result_text, the refund amount approved and the offset 免抵
//	                               amount), received (amount and the STL fund record, FundRecordRef
//	                               v1, R38), rejected (result_text); converted_to_exemption (S5R-D14:
//	                               from expected or planned, final; the reason and evidence of the missed
//	                               deadline); evidence_rolled_back (S5R-D20: a submitted, approved or
//	                               received case keeps its state; purchase_invoice_id, result_text and
//	                               evidence of the external rollback of that invoice's evidence, which a
//	                               red invoice reversing it needs first). Only a rejection, an external
//	                               result, returns the case's evidence to its balances: a local
//	                               cancellation does not exist (CN-R09).
//	tax_refund_case.rebind_policy  human only (tax specialist; S5R-D23), at expected_version: an expected
//	                               case binds revision, the one POL's ConfigAt v1 answers now for its
//	                               export date (a revision published later whose validity also contains
//	                               it: the case read shows policy_superseded), with a reason. Planned and
//	                               after, the binding no longer changes.
//
// Request members in snake_case: tax_refund_case.allocate {export_lot_id, policy_id, allocations
// [{allocation_id, export_line_id, purchase_invoice_id, line_no, purpose, quantity, replaces}]};
// tax_refund_case.record_result {case_id, expected_version, stage, currency {id, revision}, amount,
// offset_amount, tax_period, review {reviewer, evidence_ids}, external_reference, result_text,
// fund_record_id, purchase_invoice_id, evidence_ids, reason}; tax_refund_case.rebind_policy {case_id,
// expected_version, revision, reason}.

// Commands, reads and events of tax refund cases (role template tax specialist; humanOnlyCommands
// name tax_refund_case.record_result).
const (
	CommandAllocateTaxEvidence = "tax_refund_case.allocate"
	CommandRecordTaxResult     = "tax_refund_case.record_result"
	// CommandRebindPolicy (S5R-D23; R0's name): an expected case rebinds the revision ConfigAt v1 answers now.
	CommandRebindPolicy = "tax_refund_case.rebind_policy"

	// Reads: a case with its policy binding, allocations, results and amounts; the company's cases.
	CommandGetTaxCase          = "tax_refund_case.get"
	CommandTaxCases            = "tax_refund_case.list"
	EventTaxEvidenceAllocated  = "idp-brant.cnx.tax_evidence_allocated.v1"
	EventTaxRefundResultRecord = "idp-brant.cnx.tax_refund_result_recorded.v1"
)

// States of a case (SM-CNX-TAXCASE) and the stages record_result records.
const (
	TaxExpected  = "expected"
	TaxPlanned   = "planned"
	TaxSubmitted = "submitted"
	TaxApproved  = "approved"
	TaxReceived  = "received"
	TaxRejected  = "rejected"
	// TaxConvertedToExemption (S5R-D14): not declared for a refund in time, the export is exempt; final.
	TaxConvertedToExemption = "converted_to_exemption"
	// TaxEvidenceRolledBack (S5R-D20) is a stage a filed case records without moving: the external rollback of a
	// purchase invoice's evidence (进货凭证信息回退).
	TaxEvidenceRolledBack = "evidence_rolled_back"
)

// Purposes of a tax evidence allocation (CN-R04 "用途"; baseline 22.2 "相同凭证的不同用途不能混用同一个余额"):
// the refund (退税或免抵退) declaration, or the support of an exemption without refund (免税).
const (
	PurposeExportRefund    = "export_refund"
	PurposeExportExemption = "export_exemption"
)

// States of an allocation: standing; review (its invoice was reversed by a red one); replaced (a
// later allocation took its place); released (its case was rejected, an external result).
const (
	EvidenceStanding = "standing"
	EvidenceReview   = "review"
	EvidenceReplaced = "replaced"
	EvidenceReleased = "released"
)

// Limits of the tax refund case contract.
const (
	MaxTaxAllocations = 50
	MaxResultText     = 4000
	MaxReference      = 64
)

var taxPeriod = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)

// EvidenceAllocation allocates Quantity (in the unit of the invoice line) of line LineNo of
// verified purchase invoice PurchaseInvoiceID to export line ExportLineID for Purpose, as
// AllocationID (client-chosen). Replaces names an allocation of the same export line under review.
type EvidenceAllocation struct {
	AllocationID      string
	ExportLineID      string
	PurchaseInvoiceID string
	LineNo            int
	Purpose           string
	Quantity          string
	Replaces          string
}

// AllocateTaxEvidence allocates purchase invoice lines to export lines of FUL export lot
// ExportLotID under the export_tax_policy PolicyID. No expected version: the case root's lock and
// the balances derived under the invoice roots' locks are the condition ("用途余额条件更新").
type AllocateTaxEvidence struct {
	IdempotencyKey string
	ExportLotID    string
	PolicyID       string
	Allocations    []EvidenceAllocation
}

func purpose(p string) bool { return p == PurposeExportRefund || p == PurposeExportExemption }

// Validate checks the closed contract.
func (c AllocateTaxEvidence) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ExportLotID):
		return schema("/args/export_lot_id")
	case len(c.PolicyID) > MaxRegulationIDLen || !kernel.ValidIdentifier(c.PolicyID):
		return schema("/args/policy_id")
	case len(c.Allocations) < 1 || len(c.Allocations) > MaxTaxAllocations:
		return schema("/args/allocations")
	}
	ids, replaced := map[string]bool{}, map[string]bool{}
	for i, a := range c.Allocations {
		p := "/args/allocations/" + strconv.Itoa(i)
		switch {
		case !kernel.ValidIdentifier(a.AllocationID) || ids[a.AllocationID]:
			return schema(p + "/allocation_id")
		case !kernel.ValidIdentifier(a.ExportLineID):
			return schema(p + "/export_line_id")
		case !kernel.ValidIdentifier(a.PurchaseInvoiceID):
			return schema(p + "/purchase_invoice_id")
		case a.LineNo < 1 || a.LineNo > MaxInvoiceLines:
			return schema(p + "/line_no")
		case !purpose(a.Purpose):
			return schema(p + "/purpose")
		case !positive(a.Quantity, kernel.QuantityFormat, false):
			return schema(p + "/quantity")
		case a.Replaces != "" && (!kernel.ValidIdentifier(a.Replaces) || replaced[a.Replaces] || ids[a.Replaces]):
			return schema(p + "/replaces")
		}
		ids[a.AllocationID], replaced[a.Replaces] = true, a.Replaces != ""
	}
	for _, a := range c.Allocations {
		if replaced[a.AllocationID] {
			return schema("/args/allocations")
		}
	}
	return nil
}

// ProfessionalReview names the professional who reviewed a planned declaration (CN-R08
// "专业复核人") and the EVD sources of the review.
type ProfessionalReview struct {
	Reviewer    string
	EvidenceIDs []string
}

// RecordTaxResult records Stage of case CaseID at ExpectedVersion (human only). The members each
// stage takes:
//
//	expected   currency, amount, reason (the basis of the estimate); evidence_ids optional
//	planned    currency, amount, tax_period (YYYY-MM), review; reason optional
//	submitted  external_reference, evidence_ids; reason optional
//	approved   currency, amount (the refund approved), offset_amount (optional), result_text, evidence_ids
//	received   currency, amount, fund_record_id; reason optional
//	rejected   result_text, evidence_ids; reason optional
//	converted_to_exemption  evidence_ids (the missed deadline), reason (S5R-D14)
//	evidence_rolled_back    purchase_invoice_id, result_text, evidence_ids; reason optional (S5R-D20)
//
// The members a stage does not take are omitted.
type RecordTaxResult struct {
	IdempotencyKey    string
	CaseID            string
	ExpectedVersion   int64
	Stage             string
	Currency          *CurrencyRef
	Amount            string
	OffsetAmount      string
	TaxPeriod         string
	Review            *ProfessionalReview
	ExternalReference string
	ResultText        string
	FundRecordID      string
	PurchaseInvoiceID string `json:",omitempty"`
	EvidenceIDs       []string
	Reason            string
}

// Validate checks the closed contract.
func (c RecordTaxResult) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CaseID):
		return schema("/args/case_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	}
	var money, offset, period, review, reference, text, fund, invoice, evidenceMin, reasonRequired bool
	evidenceAllowed := true
	switch c.Stage {
	case TaxExpected:
		money, reasonRequired = true, true
	case TaxPlanned:
		money, period, review, evidenceAllowed = true, true, true, false
	case TaxSubmitted:
		reference, evidenceMin = true, true
	case TaxApproved:
		money, offset, text, evidenceMin = true, true, true, true
	case TaxReceived:
		money, fund, evidenceAllowed = true, true, false
	case TaxRejected:
		text, evidenceMin = true, true
	case TaxConvertedToExemption:
		evidenceMin, reasonRequired = true, true
	case TaxEvidenceRolledBack:
		invoice, text, evidenceMin = true, true, true
	default:
		return schema("/args/stage")
	}
	if money {
		switch {
		case c.Currency == nil || !kernel.ValidIdentifier(c.Currency.ID):
			return schema("/args/currency/id")
		case c.Currency.Revision < 1 || c.Currency.Revision > maxVersion:
			return schema("/args/currency/revision")
		case !amount(c.Amount):
			return schema("/args/amount")
		}
	} else if c.Currency != nil {
		return schema("/args/currency")
	} else if c.Amount != "" {
		return schema("/args/amount")
	}
	switch {
	case offset && c.OffsetAmount != "" && !amount(c.OffsetAmount), !offset && c.OffsetAmount != "":
		return schema("/args/offset_amount")
	case period && !taxPeriod.MatchString(c.TaxPeriod), !period && c.TaxPeriod != "":
		return schema("/args/tax_period")
	case review != (c.Review != nil):
		return schema("/args/review")
	case reference && !Text(c.ExternalReference, MaxReference), !reference && c.ExternalReference != "":
		return schema("/args/external_reference")
	case text && !Text(c.ResultText, MaxResultText), !text && c.ResultText != "":
		return schema("/args/result_text")
	case fund && !kernel.ValidIdentifier(c.FundRecordID), !fund && c.FundRecordID != "":
		return schema("/args/fund_record_id")
	case invoice && !kernel.ValidIdentifier(c.PurchaseInvoiceID), !invoice && c.PurchaseInvoiceID != "":
		return schema("/args/purchase_invoice_id")
	case !evidenceAllowed && len(c.EvidenceIDs) != 0:
		return schema("/args/evidence_ids")
	case reasonRequired && !Text(c.Reason, MaxReason), !reasonRequired && c.Reason != "" && !Text(c.Reason, MaxReason):
		return schema("/args/reason")
	}
	if r := c.Review; r != nil {
		if !Text(r.Reviewer, MaxInvoiceText) {
			return schema("/args/review/reviewer")
		}
		if err := evidence(r.EvidenceIDs, "/args/review/evidence_ids", 1); err != nil {
			return err
		}
	}
	min := 0
	if evidenceMin {
		min = 1
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", min)
}

// RebindPolicy rebinds expected case CaseID at ExpectedVersion to Revision of its export_tax_policy, the revision POL's
// ConfigAt v1 answers now for the case's export date, for Reason (human only; S5R-D23).
type RebindPolicy struct {
	IdempotencyKey  string
	CaseID          string
	ExpectedVersion int64
	Revision        int64
	Reason          string
}

// Validate checks the closed contract.
func (c RebindPolicy) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CaseID):
		return schema("/args/case_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case c.Revision < 1 || c.Revision > maxVersion:
		return schema("/args/revision")
	case !Text(c.Reason, MaxReason):
		return schema("/args/reason")
	}
	return nil
}

func (AllocateTaxEvidence) cnxCommand() {}
func (RecordTaxResult) cnxCommand()     {}
func (RebindPolicy) cnxCommand()        {}

// PolicyBindingView is the export_tax_policy revision a case binds: the one in force at the export
// date, with its content hash, refund method and validity.
type PolicyBindingView struct {
	PolicyID     string
	Revision     int64
	ContentHash  string
	RefundMethod string
	ValidFrom    time.Time
	ValidUntil   time.Time // zero: open-ended
}

// TaxCaseReceipt is the answer of a case command: the receipt (ObjectID the case, its row version
// and status), its export lot and attempt, the policy revision it binds and what the command
// wrote (the allocations, or the result; ResultID is "" on a replay).
type TaxCaseReceipt struct {
	Receipt
	CaseID        string
	ExportLotID   string
	Attempt       int
	Policy        PolicyBindingView
	AllocationIDs []string
	ResultID      string
}

// TaxCaseCommands is the tax refund part of CNX's command entry point.
type TaxCaseCommands interface {
	AllocateTaxEvidence(ctx context.Context, cmd AllocateTaxEvidence) (TaxCaseReceipt, error)
	RecordTaxResult(ctx context.Context, cmd RecordTaxResult) (TaxCaseReceipt, error)
	RebindPolicy(ctx context.Context, cmd RebindPolicy) (TaxCaseReceipt, error)
}

// TaxAllocationView is an allocation: the export line FUL gave (lot, shipment, order line, SKU
// specification, quantity and unit), the invoice line it uses, purpose, quantity and state now.
type TaxAllocationView struct {
	AllocationID      string
	ExportLineID      string
	ShipmentID        string
	OrderLineID       string
	SkuID             string
	SpecRevision      int64
	ExportQuantity    string
	ExportUnit        string
	PurchaseInvoiceID string
	InvoiceKind       string
	InvoiceNumber     string
	LineNo            int
	Purpose           string
	Quantity          string
	Unit              string
	Status            string
	Replaces          string
	ReplacedBy        string
	AllocatedBy       kernel.ActorRef
	AllocatedAt       time.Time
	AuthorityEpoch    int64
}

// TaxResultView is a recorded stage with what it rests on.
type TaxResultView struct {
	ResultID          string
	Stage             string
	From              string
	CurrencyCode      string
	AmountMinor       string
	OffsetMinor       string
	TaxPeriod         string
	Reviewer          string
	ReviewEvidence    []EvidenceView
	ExternalReference string
	ResultText        string
	FundRecordID      string
	PurchaseInvoiceID string // evidence_rolled_back (S5R-D20)
	Evidence          []EvidenceView
	Reason            string
	RecordedBy        kernel.ActorRef
	RecordedAt        time.Time
	AuthorityEpoch    int64
}

// TaxAmountsView shows the amounts of a case side by side (CN-R08, CN-AT10), in minor units of
// CurrencyCode: the latest estimate, the planned, the approved refund and offset and the received
// amount ("" where no stage stated one), and the differences approved − expected and received −
// approved ("" unless both are stated in one currency). Brant derives nothing else.
type TaxAmountsView struct {
	CurrencyCode              string
	ExpectedMinor             string
	PlannedMinor              string
	ApprovedMinor             string
	OffsetMinor               string
	ReceivedMinor             string
	ApprovedLessExpectedMinor string
	ReceivedLessApprovedMinor string
}

// PolicyAtView is what POL's ConfigAt v1 answers now for a case's policy at its export date (S5R-D23): the revision
// and its content hash, or POL's code (CONFIG_NOT_ACTIVE, REFERENCE_INVALID) when none applies.
type PolicyAtView struct {
	Revision    int64
	ContentHash string
	Code        string
}

// RebindingView is a rebinding of an expected case (tax_refund_case.rebind_policy, S5R-D23).
type RebindingView struct {
	RebindingID    string
	CaseVersion    int64
	FromRevision   int64
	FromHash       string
	FromMethod     string
	ToRevision     int64
	ToHash         string
	ToMethod       string
	Reason         string
	ReboundBy      kernel.ActorRef
	AuthorityEpoch int64
	ReboundAt      time.Time
}

// TaxCaseView is a case: the export lot, the policy binding, its state, every allocation and
// result, oldest first, and the amounts. While the case is expected, PolicyAt is what ConfigAt v1 answers now for
// its export date and PolicySuperseded reports that it answers another revision than the one bound (a revision
// published later whose validity also contains the export date: rebind it, S5R-D23); planned and after, the binding
// no longer changes and PolicyAt is nil. Rebindings lists every rebinding, oldest first.
type TaxCaseView struct {
	CaseID           string
	ExportLotID      string
	Attempt          int
	OrderID          string
	ExportedAt       time.Time
	Policy           PolicyBindingView
	PolicyAt         *PolicyAtView
	PolicySuperseded bool
	Rebindings       []RebindingView
	Status           string
	RowVersion       int64
	Allocations      []TaxAllocationView
	Results          []TaxResultView
	Amounts          TaxAmountsView
	OpenedBy         kernel.ActorRef
	OpenedAt         time.Time
	AuthorityEpoch   int64
	CheckedAt        time.Time
	Opening          *TaxOpeningRef // S5 C10: an opening case's opening item (its export is no FUL lot)
}

// TaxCaseSummary is one case of the company.
type TaxCaseSummary struct {
	CaseID         string
	ExportLotID    string
	Attempt        int
	Status         string
	PolicyID       string
	PolicyRevision int64
	RefundMethod   string
	RowVersion     int64
	OpenedAt       time.Time
}

// TaxCaseQueries is the tax refund part of CNX's audited read side (cnx.read).
type TaxCaseQueries interface {
	// GetTaxCase returns a case of the company, audited as tax_refund_case.get; NOT_FOUND otherwise.
	GetTaxCase(ctx context.Context, caseID string) (TaxCaseView, error)
	// TaxCases returns the company's cases, newest first, audited as tax_refund_case.list.
	TaxCases(ctx context.Context) ([]TaxCaseSummary, error)
}
