package api

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Opening refund material (S5 unit C10, WBS S5-20; tactical model CNX-AGG-TaxRefundCase value object
// OpeningPosition, SM-CNX-TAXCASE, CNX-INV-004; Context Map MD-51; baseline 11.2 期初、防重、切换, CN-R04, CN-R08, CN-R09,
// CN-E01; CN-AT16 CNX half, BT17).
//
//	tax_refund_case.record_opening  human only (tax specialist): registers one item of the refund material the books
//	                                held at the cutoff instant cutoff_at, from source_document (its reference in the
//	                                register it comes from) with EVD evidence:
//	                                  invoice_balance  purchase_invoice_id, a verified blue purchase invoice in CNX
//	                                                   (imported and verified as any other), and per line and purpose
//	                                                   balances [{line_no, purpose, consumed}]: the quantity, in the
//	                                                   invoice line's unit, that refund declarations finished before
//	                                                   the cutoff consumed. It counts for ever: an external result
//	                                                   consumed it, so no local action gives it back (CN-R09). What is
//	                                                   left is the line's remaining purpose balance for allocations.
//	                                  refund_case      a refund case of an export declared before the cutoff, outside
//	                                                   Brant's FUL lots: export {reference — the customs declaration
//	                                                   number —, exported_at — the 出口日期 it states, not after the
//	                                                   cutoff —, order_reference, lines [{line_id, quantity, unit}]};
//	                                                   policy_id, whose export_tax_policy revision valid at the export
//	                                                   date the case binds (CN-AT12; published, possibly after the export:
//	                                                   at the cutover the professional records the policy that applied
//	                                                   then — a FUL lot's case binds only one published by its export
//	                                                   date); allocations [{allocation_id,
//	                                                   export_line_id, purchase_invoice_id, line_no, purpose, quantity}]
//	                                                   — the invoice lines its declaration uses, as allocate takes them
//	                                                   (TAX_EVIDENCE_EXCEEDED beyond a balance); position — the stage the
//	                                                   external process had reached at the cutoff (expected, planned,
//	                                                   submitted or approved) with that stage's members as
//	                                                   record_result takes them. The case opens in that state; from there
//	                                                   record_result continues it (approved → received with the STL fund
//	                                                   record, submitted → approved or rejected). Its allocations count
//	                                                   until an external rejection releases them, as any case's.
//	                                A source document recorded before as an item of the kind, a second balance of an
//	                                invoice, or a second live case of an export, is OPENING_BALANCE_DUPLICATE: no
//	                                usage is created twice. Brant computes no tax and no refund (CN-E01).
//
// The export date of an opening case is a person's statement at the cutoff with the declaration's evidence; the
// export date of a declared FUL lot (S5 C3a) is what customs' receipt states, recorded by record_receipt with its
// original text, mapping version and external sequence, and it moves with later receipts. An opening case has no
// declaration case in CNX and no receipts: its date never moves.
//
// Request members: tax_refund_case.record_opening {opening_id, kind, cutoff_at, source_document, evidence_ids,
// purchase_invoice_id, balances [{line_no, purpose, consumed}], export {reference, exported_at, order_reference,
// lines [{line_id, quantity, unit}]}, policy_id, allocations [{allocation_id, export_line_id, purchase_invoice_id,
// line_no, purpose, quantity}], position {stage, currency {id, revision}, amount, offset_amount, tax_period, review
// {reviewer, evidence_ids}, external_reference, result_text, evidence_ids, reason}}.
const (
	CommandRecordTaxOpening = "tax_refund_case.record_opening"

	// Reads: an opening item now, and the company's opening items.
	CommandGetTaxOpening = "tax_opening.get"
	CommandTaxOpenings   = "tax_opening.list"

	EventTaxOpeningRecorded = "idp-brant.cnx.tax_opening_recorded.v1"

	// Kinds of an opening item.
	OpeningInvoiceBalance = "invoice_balance"
	OpeningRefundCase     = "refund_case"

	// FromOpening is the From of the result an opening case opens with.
	FromOpening = "opening"

	// Limits of the opening contract.
	MaxOpeningLines = 50
)

// CodeOpeningBalanceDuplicate (409): the source document, the invoice's balance or the export's case is recorded
// as opening material already (STL-INV-012's code, catalog contexts STL／CNX).
const CodeOpeningBalanceDuplicate = "OPENING_BALANCE_DUPLICATE"

var openingReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$`)

// OpeningBalance is what declarations finished before the cutoff consumed of line LineNo for Purpose (Consumed, in
// the invoice line's unit).
type OpeningBalance struct {
	LineNo   int
	Purpose  string
	Consumed string
}

// OpeningExportLine is a line of an export declared before the cutoff: its ID in the opening case, its quantity and
// unit (the unit of the invoice lines it is allocated).
type OpeningExportLine struct {
	LineID   string
	Quantity string
	Unit     string
}

// OpeningExport is an export declared before the cutoff: the customs declaration number (Reference), the export
// date it states, the order it shipped (a stated reference) and its lines.
type OpeningExport struct {
	Reference      string
	ExportedAt     string
	OrderReference string
	Lines          []OpeningExportLine
}

// OpeningAllocation is an invoice line an opening case's declaration uses (tax_refund_case.allocate's members
// without replaces).
type OpeningAllocation struct {
	AllocationID      string
	ExportLineID      string
	PurchaseInvoiceID string
	LineNo            int
	Purpose           string
	Quantity          string
}

// OpeningPosition is the stage an opening case's external process had reached at the cutoff, with that stage's
// members as RecordTaxResult takes them (expected, planned, submitted, approved).
type OpeningPosition struct {
	Stage             string
	Currency          *CurrencyRef
	Amount            string
	OffsetAmount      string
	TaxPeriod         string
	Review            *ProfessionalReview
	ExternalReference string
	ResultText        string
	EvidenceIDs       []string
	Reason            string
}

// Result is the position as the stage record_result would record.
func (p OpeningPosition) Result(caseID string) RecordTaxResult {
	return RecordTaxResult{IdempotencyKey: "opening", CaseID: caseID, ExpectedVersion: 1, Stage: p.Stage, Currency: p.Currency, Amount: p.Amount,
		OffsetAmount: p.OffsetAmount, TaxPeriod: p.TaxPeriod, Review: p.Review, ExternalReference: p.ExternalReference, ResultText: p.ResultText,
		EvidenceIDs: p.EvidenceIDs, Reason: p.Reason}
}

// RecordTaxOpening records opening item OpeningID of OpeningKind ("kind") at CutoffAt (human only). See the comment
// above for the members each kind takes; the members the other kind takes are omitted.
type RecordTaxOpening struct {
	IdempotencyKey    string
	OpeningID         string
	OpeningKind       string `json:"kind"`
	CutoffAt          string
	SourceDocument    string
	EvidenceIDs       []string
	PurchaseInvoiceID string
	Balances          []OpeningBalance
	Export            *OpeningExport
	PolicyID          string
	Allocations       []OpeningAllocation
	Position          *OpeningPosition
}

func (RecordTaxOpening) cnxCommand() {}

// Validate checks the closed contract.
func (c RecordTaxOpening) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OpeningID):
		return schema("/args/opening_id")
	case c.OpeningKind != OpeningInvoiceBalance && c.OpeningKind != OpeningRefundCase:
		return schema("/args/kind")
	}
	if _, err := kernel.ParseTimestamp(c.CutoffAt); err != nil {
		return schema("/args/cutoff_at")
	}
	if !openingReference.MatchString(c.SourceDocument) {
		return schema("/args/source_document")
	}
	if err := evidence(c.EvidenceIDs, "/args/evidence_ids", 1); err != nil {
		return err
	}
	if c.OpeningKind == OpeningInvoiceBalance {
		return c.validateBalance()
	}
	return c.validateCase()
}

func (c RecordTaxOpening) validateBalance() error {
	switch {
	case !kernel.ValidIdentifier(c.PurchaseInvoiceID):
		return schema("/args/purchase_invoice_id")
	case len(c.Balances) < 1 || len(c.Balances) > 2*MaxInvoiceLines:
		return schema("/args/balances")
	case c.Export != nil:
		return schema("/args/export")
	case c.PolicyID != "":
		return schema("/args/policy_id")
	case len(c.Allocations) > 0:
		return schema("/args/allocations")
	case c.Position != nil:
		return schema("/args/position")
	}
	seen := map[[2]string]bool{}
	for i, b := range c.Balances {
		p := "/args/balances/" + strconv.Itoa(i)
		k := [2]string{strconv.Itoa(b.LineNo), b.Purpose}
		switch {
		case b.LineNo < 1 || b.LineNo > MaxInvoiceLines:
			return schema(p + "/line_no")
		case !purpose(b.Purpose) || seen[k]:
			return schema(p + "/purpose")
		case !positive(b.Consumed, kernel.QuantityFormat, true):
			return schema(p + "/consumed")
		}
		seen[k] = true
	}
	return nil
}

func (c RecordTaxOpening) validateCase() error {
	switch {
	case c.PurchaseInvoiceID != "":
		return schema("/args/purchase_invoice_id")
	case len(c.Balances) > 0:
		return schema("/args/balances")
	case c.Export == nil:
		return schema("/args/export")
	case len(c.PolicyID) > MaxRegulationIDLen || !kernel.ValidIdentifier(c.PolicyID):
		return schema("/args/policy_id")
	case len(c.Allocations) < 1 || len(c.Allocations) > MaxTaxAllocations:
		return schema("/args/allocations")
	case c.Position == nil:
		return schema("/args/position")
	}
	x := c.Export
	if !kernel.ValidIdentifier(x.Reference) {
		return schema("/args/export/reference")
	}
	if _, err := kernel.ParseTimestamp(x.ExportedAt); err != nil {
		return schema("/args/export/exported_at")
	}
	switch {
	case x.OrderReference != "" && !openingReference.MatchString(x.OrderReference):
		return schema("/args/export/order_reference")
	case len(x.Lines) < 1 || len(x.Lines) > MaxOpeningLines:
		return schema("/args/export/lines")
	}
	lines := map[string]bool{}
	for i, l := range x.Lines {
		p := "/args/export/lines/" + strconv.Itoa(i)
		switch {
		case !kernel.ValidIdentifier(l.LineID) || lines[l.LineID]:
			return schema(p + "/line_id")
		case !positive(l.Quantity, kernel.QuantityFormat, false):
			return schema(p + "/quantity")
		case !kernel.UnitCode(l.Unit).Valid():
			return schema(p + "/unit")
		}
		lines[l.LineID] = true
	}
	ids := map[string]bool{}
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
		}
		ids[a.AllocationID] = true
	}
	switch c.Position.Stage {
	case TaxExpected, TaxPlanned, TaxSubmitted, TaxApproved:
	default:
		return schema("/args/position/stage")
	}
	if err := c.Position.Result(c.OpeningID).Validate(); err != nil {
		if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) == 1 && strings.HasPrefix(rej.Fields[0].Path, "/args/") {
			return schema("/args/position/" + rej.Fields[0].Path[len("/args/"):])
		}
		return err
	}
	return nil
}

// TaxOpeningReceipt answers tax_refund_case.record_opening: the receipt (ObjectID the opening item, Status the
// position of an opening case or "recorded"), its kind and cutoff, and the invoice or the case it is.
type TaxOpeningReceipt struct {
	Receipt
	OpeningKind       string `json:"kind"`
	CutoffAt          time.Time
	SourceDocument    string
	PurchaseInvoiceID string
	CaseID            string
	AllocationIDs     []string
	ResultID          string
}

// TaxOpeningLineView is a line and purpose of an invoice balance: the line's quantity, what declarations finished
// before the cutoff consumed, the remaining purpose balance at the cutoff left of it, what the cases' allocations
// take of it now (opening cases' included) and what is available now.
type TaxOpeningLineView struct {
	LineNo         int
	Unit           string
	Purpose        string
	Quantity       string
	Consumed       string
	RemainingAtCut string
	AllocatedNow   string
	AvailableNow   string
}

// TaxOpeningView is an opening item (tax_opening.get): its kind, cutoff, source document and evidence; an invoice
// balance's lines; an opening case's export, the position it opened in and the case now.
type TaxOpeningView struct {
	OpeningID         string
	OpeningKind       string `json:"kind"`
	CutoffAt          time.Time
	SourceDocument    string
	Evidence          []EvidenceView
	PurchaseInvoiceID string
	Lines             []TaxOpeningLineView
	CaseID            string
	ExportReference   string
	ExportedAt        time.Time
	OrderReference    string
	Position          string
	Case              *TaxCaseSummary
	RecordedBy        kernel.ActorRef
	RecordedAt        time.Time
	AuthorityEpoch    int64
	CheckedAt         time.Time
}

// TaxOpeningRef is the opening an opening case was recorded from, as TaxCaseView shows it.
type TaxOpeningRef struct {
	OpeningID       string
	CutoffAt        time.Time
	SourceDocument  string
	ExportReference string
	OrderReference  string
	Position        string
}

// TaxOpeningCommands is the opening part of CNX's command entry point.
type TaxOpeningCommands interface {
	RecordTaxOpening(ctx context.Context, cmd RecordTaxOpening) (TaxOpeningReceipt, error)
}

// TaxOpeningQueries is the opening part of CNX's audited read side (cnx.read; company-wide like the cases).
type TaxOpeningQueries interface {
	// GetTaxOpening returns an opening item, audited as tax_opening.get; NOT_FOUND otherwise.
	GetTaxOpening(ctx context.Context, openingID string) (TaxOpeningView, error)
	// TaxOpenings returns the company's opening items, by cutoff and ID, audited as tax_opening.list.
	TaxOpenings(ctx context.Context) ([]TaxOpeningView, error)
}
