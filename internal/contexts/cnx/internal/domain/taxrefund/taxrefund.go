// Package taxrefund holds CNX's TaxRefundCase (tactical model CNX-AGG-TaxRefundCase with its
// TaxEvidenceAllocation entries and the value objects PolicyVersion and RefundStage; SM-CNX-TAXCASE;
// CNX-INV-004; UL CNX-TaxRefundCase; baseline 22.1, 22.2, CN-R04, CN-R08, CN-R09, CN-E01): the refund
// material of one FUL export lot — which purchase invoice lines evidence which export lines for
// which purpose — and the stages of its refund declaration as people record the external results.
//
// The case is the root, locked FOR UPDATE by every allocation and result. The evidence balances are
// derived from immutable allocation entries (S5 D-3): per purpose, Σ counting allocations of an
// invoice line never exceed its quantity — two allocations from different cases are serialized by
// the invoice root, which each locks FOR UPDATE —, nor those of an export line its quantity. An
// allocation counts unless a later one replaced it or its case was rejected by the external
// result: a local cancellation never returns evidence (CN-R09). Brant computes no refund
// (CN-E01): every amount of a result is a person's statement.
package taxrefund

import (
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// CodeTaxEvidenceExceeded is the domain's own code (error catalog V0.1, conflict).
const CodeTaxEvidenceExceeded = "TAX_EVIDENCE_EXCEEDED"

// Categories are the categories of the domain's own codes.
var Categories = map[string]kernel.Category{CodeTaxEvidenceExceeded: kernel.CategoryConflict,
	CodeOpeningBalanceDuplicate: kernel.CategoryConflict}

func exceeded(path, reason string) *kernel.Rejection {
	return kernel.RejectAs(CodeTaxEvidenceExceeded, Categories[CodeTaxEvidenceExceeded], path, reason)
}

// States of a case, purposes, allocation states and the refund methods of POL's export_tax_policy.
const (
	Expected  = "expected"
	Planned   = "planned"
	Submitted = "submitted"
	Approved  = "approved"
	Received  = "received"
	Rejected  = "rejected"

	ExportRefund    = "export_refund"
	ExportExemption = "export_exemption"

	Standing = "standing"
	Review   = "review"
	Replaced = "replaced"
	Released = "released"

	MethodExemptCreditRefund = "exempt_credit_refund"
	MethodExemptRefund       = "exempt_refund"
	MethodNotApplicable      = "not_applicable"
	// MethodExempt (免税) and MethodTaxable (征税) are the statutory methods without a refund (S5R-D14);
	// POL refuses not_applicable on a new revision.
	MethodExempt  = "exempt"
	MethodTaxable = "taxable"
)

// refunds reports whether method refunds VAT (免抵退 or 免退): only then may evidence be allocated for
// export_refund (S5R-D14: export_refund 对应免抵退或免退).
func refunds(method string) bool {
	return method == MethodExemptCreditRefund || method == MethodExemptRefund
}

// Binding is the export_tax_policy revision a case binds (PolicyVersion): the one in force at the
// export date (CN-AT12, CN-R08 "适用方法变化不追溯覆盖历史").
type Binding struct {
	PolicyID     string
	Revision     int64
	ContentHash  string
	RefundMethod string
	ValidFrom    time.Time
	ValidUntil   time.Time
}

// Revision is one revision of an export_tax_policy as POL lists it.
type Revision struct {
	Revision    int64
	Status      string
	PublishedAt time.Time
	EndedAt     time.Time
	ValidFrom   time.Time
	ValidUntil  time.Time
}

// InForceAt returns the revision of revisions in force at instant at: published at or before it,
// not ended by it and valid at it; 0 when none is (POL's ResolveConfigs decides it again).
func InForceAt(revisions []Revision, at time.Time) int64 {
	for _, r := range revisions {
		switch {
		case r.PublishedAt.IsZero() || r.PublishedAt.After(at):
		case !r.EndedAt.IsZero() && !r.EndedAt.After(at):
		case at.Before(r.ValidFrom):
		case !r.ValidUntil.IsZero() && !at.Before(r.ValidUntil):
		default:
			return r.Revision
		}
	}
	return 0
}

// Case is the TaxRefundCase root.
type Case struct {
	ID          string
	ExportLotID string
	Attempt     int
	OrderID     string
	ExportedAt  time.Time
	Policy      Binding
	Status      string
	RowVersion  int64
	OpenedBy    kernel.ActorRef
	Epoch       int64
	OpenedAt    time.Time
	Opening     bool // an opening case (S5 C10, opening.go): opened in its position at the cutoff
}

// CheckVersion compares the caller's expected row version with c's.
func CheckVersion(c *Case, expected int64) error {
	if c.RowVersion != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Allocation is a TaxEvidenceAllocation entry: an export line as FUL gave it, the invoice line it
// uses, the purpose and the quantity in the invoice line's unit. ReplacedBy, InvoiceStatus and
// CaseStatus are derived when it is loaded.
type Allocation struct {
	ID             string
	CaseID         string
	ExportLineID   string
	ShipmentID     string
	OrderLineID    string
	SkuID          string
	SpecRevision   int64
	ExportQuantity kernel.Decimal
	ExportUnit     kernel.UnitCode
	InvoiceID      string
	InvoiceKind    string
	InvoiceNumber  string
	LineNo         int
	Purpose        string
	Quantity       kernel.Decimal
	Unit           kernel.UnitCode
	Replaces       string
	ReplacedBy     string
	InvoiceStatus  string
	CaseStatus     string
	By             kernel.ActorRef
	Epoch          int64
	At             time.Time
}

// Status is the allocation's state: released once its case was rejected, replaced once a later
// allocation took its place, under review once its invoice was reversed, standing otherwise.
func (a *Allocation) Status() string {
	switch {
	case a.CaseStatus == Rejected:
		return Released
	case a.ReplacedBy != "":
		return Replaced
	case a.InvoiceStatus == "reversed":
		return Review
	}
	return Standing
}

// Counts reports whether the allocation takes evidence: standing or under review.
func (a *Allocation) Counts() bool { s := a.Status(); return s == Standing || s == Review }

// ExportLine is an export line of the case's lot as FUL gives it.
type ExportLine struct {
	LineID       string
	ShipmentID   string
	OrderLineID  string
	SkuID        string
	SpecRevision int64
	Quantity     kernel.Decimal
	Unit         kernel.UnitCode
}

// InvoiceLine is a verified purchase invoice line: its invoice's state and colour, kind and
// number, unit and quantity.
type InvoiceLine struct {
	InvoiceID     string
	InvoiceStatus string
	Colour        string
	Kind          string
	Number        string
	LineNo        int
	Unit          kernel.UnitCode
	Quantity      kernel.Decimal
}

// LineKey names an invoice line for a purpose.
type LineKey struct {
	InvoiceID string
	LineNo    int
	Purpose   string
}

// AllocateInput is one allocation asked for.
type AllocateInput struct {
	ID           string
	ExportLineID string
	InvoiceID    string
	LineNo       int
	Purpose      string
	Quantity     kernel.Decimal
	Replaces     string
}

// Context is what an allocation is judged on, read under the case and invoice root locks: the
// case's allocations, the lot's export lines by ID, the invoice lines asked for (absent: the
// company has no such verified line) and what counting allocations of every case take of each
// invoice line per purpose.
type Context struct {
	Case         *Case
	Allocations  []*Allocation
	ExportLines  map[string]ExportLine
	InvoiceLines map[InvoiceLineRef]InvoiceLine
	Used         map[LineKey]kernel.Decimal
	By           kernel.ActorRef
	Epoch        int64
	At           time.Time
}

// InvoiceLineRef names a line of a purchase invoice.
type InvoiceLineRef struct {
	InvoiceID string
	LineNo    int
}

// Allocate judges the allocations in and returns the entries to append (CNX-INV-004): the case
// takes allocations while expected or planned (INVALID_STATE); each names an export line of the lot
// (REFERENCE_INVALID), a line of a verified blue invoice of the company (REFERENCE_INVALID; a
// rejected, reversed, red or unverified one is INVALID_STATE), a purpose the bound refund method
// admits (export_refund needs a refund method; INVALID_KNOWN_VALUE) and the invoice line's unit
// (INVALID_KNOWN_VALUE: no conversion is guessed); a replacement names an allocation of the case on
// the same export line under review (INVALID_STATE). Then, per purpose, Σ counting allocations of
// the invoice line, with these, at most its quantity and of the export line at most its quantity
// (TAX_EVIDENCE_EXCEEDED).
func Allocate(x Context, in []AllocateInput) ([]*Allocation, error) {
	c := x.Case
	if c.Status != Expected && c.Status != Planned {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/export_lot_id", "The tax refund case is "+c.Status)
	}
	return allocate(x, in)
}

// allocate is Allocate's judgement of the allocations, whatever the case's state (an opening case takes its
// allocations when it opens, in its position: opening.go).
func allocate(x Context, in []AllocateInput) ([]*Allocation, error) {
	c := x.Case
	byID := map[string]*Allocation{}
	exportUsed := map[[2]string]kernel.Decimal{}
	for _, a := range x.Allocations {
		byID[a.ID] = a
		if a.Counts() {
			k := [2]string{a.ExportLineID, a.Purpose}
			exportUsed[k] = exportUsed[k].Add(a.Quantity)
		}
	}
	used := map[LineKey]kernel.Decimal{}
	for k, v := range x.Used {
		used[k] = v
	}
	var out []*Allocation
	for i, a := range in {
		p := "/args/allocations/" + strconv.Itoa(i)
		el, ok := x.ExportLines[a.ExportLineID]
		if !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, p+"/export_line_id", "No such line of the export lot")
		}
		il, ok := x.InvoiceLines[InvoiceLineRef{InvoiceID: a.InvoiceID, LineNo: a.LineNo}]
		switch {
		case !ok:
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, p+"/line_no", "No such verified invoice line")
		case il.InvoiceStatus != "verified":
			return nil, kernel.Reject(kernel.CodeInvalidState, p+"/purchase_invoice_id", "The invoice is "+il.InvoiceStatus)
		case il.Colour != "blue":
			return nil, kernel.Reject(kernel.CodeInvalidState, p+"/purchase_invoice_id", "A red invoice evidences nothing")
		case a.Purpose == ExportRefund && !refunds(c.Policy.RefundMethod):
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/purpose", "The bound export tax policy has no refund method")
		case il.Unit != el.Unit:
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/quantity", "The export line's unit is "+string(el.Unit)+", the invoice line's "+string(il.Unit))
		}
		if a.Replaces != "" {
			r, ok := byID[a.Replaces]
			if !ok || r.ExportLineID != a.ExportLineID || r.Status() != Review {
				return nil, kernel.Reject(kernel.CodeInvalidState, p+"/replaces", "Only an allocation of the export line under review is replaced")
			}
			k := [2]string{r.ExportLineID, r.Purpose}
			exportUsed[k] = exportUsed[k].Sub(r.Quantity)
		}
		lk := LineKey{InvoiceID: a.InvoiceID, LineNo: a.LineNo, Purpose: a.Purpose}
		if sum := used[lk].Add(a.Quantity); sum.Cmp(il.Quantity) > 0 {
			return nil, exceeded(p+"/quantity", "Invoice line "+strconv.Itoa(a.LineNo)+" has "+il.Quantity.Sub(used[lk]).String()+" left for "+a.Purpose)
		}
		ek := [2]string{a.ExportLineID, a.Purpose}
		if sum := exportUsed[ek].Add(a.Quantity); sum.Cmp(el.Quantity) > 0 {
			return nil, exceeded(p+"/quantity", "Export line "+a.ExportLineID+" has "+el.Quantity.Sub(exportUsed[ek]).String()+" left for "+a.Purpose)
		}
		used[lk] = used[lk].Add(a.Quantity)
		exportUsed[ek] = exportUsed[ek].Add(a.Quantity)
		out = append(out, &Allocation{ID: a.ID, CaseID: c.ID, ExportLineID: el.LineID, ShipmentID: el.ShipmentID, OrderLineID: el.OrderLineID,
			SkuID: el.SkuID, SpecRevision: el.SpecRevision, ExportQuantity: el.Quantity, ExportUnit: el.Unit, InvoiceID: il.InvoiceID,
			InvoiceKind: il.Kind, InvoiceNumber: il.Number, LineNo: il.LineNo, Purpose: a.Purpose, Quantity: a.Quantity, Unit: il.Unit,
			Replaces: a.Replaces, InvoiceStatus: il.InvoiceStatus, CaseStatus: c.Status, By: x.By, Epoch: x.Epoch, At: x.At})
	}
	return out, nil
}

// Evidence is an EVD source a result rests on.
type Evidence struct {
	SourceID      string `json:"source_id"`
	ContentSHA256 string `json:"content_sha256"`
}

// Currency is the POL currency revision of a result's amounts.
type Currency struct {
	ConfigID   string
	Revision   int64
	Code       string
	MinorUnits int
}

// Result is a recorded stage (RefundStage): what moved the case from From to Stage (expected stays
// expected) and what it rests on.
type Result struct {
	ID                string
	CaseID            string
	Stage             string
	From              string
	Currency          *Currency
	Amount            *kernel.Money
	Offset            *kernel.Money
	TaxPeriod         string
	Reviewer          string
	ReviewEvidence    []Evidence
	ExternalReference string
	ResultText        string
	FundRecordID      string
	Evidence          []Evidence
	Reason            string
	By                kernel.ActorRef
	Epoch             int64
	At                time.Time
}

// next is SM-CNX-TAXCASE: the stage each state may record.
var next = map[string][]string{
	Expected:  {Expected, Planned},
	Planned:   {Submitted},
	Submitted: {Approved, Rejected},
	Approved:  {Received},
}

// Record judges result r on case c with its earlier results and allocations: the stage must follow
// the case's state (INVALID_STATE; expected → expected records a new estimate); a plan needs
// counting evidence and none under review (INVALID_STATE); every amount of a case is in one currency
// (INVALID_KNOWN_VALUE). It returns the case's state after r.
func Record(c *Case, results []*Result, allocations []*Allocation, r *Result) (string, error) {
	allowed := false
	for _, s := range next[c.Status] {
		allowed = allowed || s == r.Stage
	}
	if !allowed {
		return "", kernel.Reject(kernel.CodeInvalidState, "/args/stage", "The tax refund case is "+c.Status+": "+r.Stage+" does not follow")
	}
	if r.Stage == Planned {
		counting := 0
		for _, a := range allocations {
			switch a.Status() {
			case Review:
				return "", kernel.Reject(kernel.CodeInvalidState, "/args/stage", "Allocation "+a.ID+" is under review: its invoice was reversed")
			case Standing:
				counting++
			}
		}
		if counting == 0 {
			return "", kernel.Reject(kernel.CodeInvalidState, "/args/stage", "The case has no evidence allocated")
		}
	}
	if r.Currency != nil {
		for _, e := range results {
			if e.Currency != nil && e.Currency.Code != r.Currency.Code {
				return "", kernel.Reject(kernel.CodeInvalidKnownValue, "/args/currency", "The case's amounts are in "+e.Currency.Code)
			}
		}
	}
	return r.Stage, nil
}

// Amounts are the latest amount of each stage of a case, nil where none was stated.
type Amounts struct {
	Currency                                   string
	Expected, Planned, Approved, Offset        *kernel.Money
	Received                                   *kernel.Money
	ApprovedLessExpected, ReceivedLessApproved *kernel.Money
}

// AmountsOf lays the amounts of results side by side (CN-R08, CN-AT10): expected, planned,
// approved (and offset) and received stay apart; the only arithmetic is the two differences, exact
// in minor units.
func AmountsOf(results []*Result) Amounts {
	var a Amounts
	for _, r := range results {
		if r.Currency != nil {
			a.Currency = r.Currency.Code
		}
		switch r.Stage {
		case Expected:
			a.Expected = r.Amount
		case Planned:
			a.Planned = r.Amount
		case Approved:
			a.Approved, a.Offset = r.Amount, r.Offset
		case Received:
			a.Received = r.Amount
		}
	}
	diff := func(x, y *kernel.Money) *kernel.Money {
		if x == nil || y == nil {
			return nil
		}
		d, err := x.Sub(*y)
		if err != nil {
			return nil
		}
		return &d
	}
	a.ApprovedLessExpected, a.ReceivedLessApproved = diff(a.Approved, a.Expected), diff(a.Received, a.Approved)
	return a
}
