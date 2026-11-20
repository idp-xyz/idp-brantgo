// Package taxrefund holds CNX's TaxRefundCase (tactical model CNX-AGG-TaxRefundCase with its
// TaxEvidenceAllocation entries and the value objects PolicyVersion and RefundStage; SM-CNX-TAXCASE;
// CNX-INV-004; UL CNX-TaxRefundCase; baseline 22.1, 22.2, CN-R04, CN-R08, CN-R09, CN-E01): the refund
// material of one FUL export lot — which purchase invoice lines evidence which export lines for
// which purpose — and the stages of its refund declaration as people record the external results.
//
// The case is the root, locked FOR UPDATE by every allocation and result. The evidence balances are
// derived from immutable allocation entries (S5 D-3): per purpose, Σ counting allocations of an
// invoice line never exceed what it evidences — its quantity less what red invoices reversed of it
// (S5R-D20) — two allocations from different cases are serialized by the invoice root, which each locks
// FOR UPDATE —, nor those of an export line its quantity. An allocation counts unless a later one
// replaced it or its case was rejected by the external result: a local cancellation never returns
// evidence (CN-R09). Brant computes no refund (CN-E01): every amount of a result is a person's
// statement.
//
// S5R: a case binds the export_tax_policy revision POL's ConfigAt v1 answers for the export date (its validity
// contains it, whenever it was published; the latest published when several do), and an expected case may rebind
// the revision ConfigAt answers later (Rebind, S5R-D23); a taxable export opens no case (S5R-D14); a case not
// declared in time converts to an exemption (converted_to_exemption, S5R-D14); a filed case records the external
// rollback of a purchase invoice's evidence before a red invoice may reverse it (evidence_rolled_back, S5R-D20).
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
	// ConvertedToExemption (S5R-D14): the export was not declared for a refund in time and is exempt; final. Its
	// allocations keep counting: the invoice lines evidenced this export whatever its treatment (CN-R09).
	ConvertedToExemption = "converted_to_exemption"
	// EvidenceRolledBack (S5R-D20) is a stage a filed case (submitted, approved, received) records without moving:
	// the external rollback of a purchase invoice's evidence (进货凭证信息回退), which a red invoice reversing that invoice
	// needs first.
	EvidenceRolledBack = "evidence_rolled_back"

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

// Refunds reports whether method refunds VAT (免抵退 or 免退; S5R-D14).
func Refunds(method string) bool { return refunds(method) }

// CheckOpens refuses a case under a taxable revision (S5R-D14: 征税的出口批次不开退税案件; INVALID_STATE at path).
func CheckOpens(method, path string) error {
	if method == MethodTaxable {
		return kernel.Reject(kernel.CodeInvalidState, path, "The export tax policy revision in force at the export date is taxable (征税): "+
			"a taxable export opens no tax refund case")
	}
	return nil
}

// Binding is the export_tax_policy revision a case binds (PolicyVersion): the one whose validity contains the export
// date, whenever it was published (POL ConfigAt v1; S5R-D23, CN-AT12, CN-R08 "适用方法变化不追溯覆盖历史").
type Binding struct {
	PolicyID     string
	Revision     int64
	ContentHash  string
	RefundMethod string
	ValidFrom    time.Time
	ValidUntil   time.Time
}

// Rebind judges rebinding expected case c to revision b of its policy, the one POL's ConfigAt v1 answers now for its
// export date (tax_refund_case.rebind_policy, S5R-D23), with the case's allocations: only an expected case rebinds
// (INVALID_STATE /args/case_id: planned and after the binding no longer changes); a revision it binds already is no
// rebinding (INVALID_STATE /args/revision); never to a taxable revision (CheckOpens), nor to one that refunds nothing
// while export_refund evidence counts in the case (INVALID_STATE /args/revision).
func Rebind(c *Case, b Binding, allocations []*Allocation) error {
	switch {
	case c.Status != Expected:
		return kernel.Reject(kernel.CodeInvalidState, "/args/case_id", "The tax refund case is "+c.Status+": only an expected case rebinds its policy")
	case b.Revision == c.Policy.Revision:
		return kernel.Reject(kernel.CodeInvalidState, "/args/revision", "The case binds revision "+strconv.FormatInt(b.Revision, 10)+" already")
	}
	if err := CheckOpens(b.RefundMethod, "/args/revision"); err != nil {
		return err
	}
	if !refunds(b.RefundMethod) {
		for _, a := range allocations {
			if a.Purpose == ExportRefund && a.Counts() {
				return kernel.Reject(kernel.CodeInvalidState, "/args/revision", "Revision "+strconv.FormatInt(b.Revision, 10)+" refunds nothing ("+
					b.RefundMethod+") and allocation "+a.ID+" evidences export_refund")
			}
		}
	}
	return nil
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
	// LineOverused: what counting allocations and the opening consumption take of the invoice line for the purpose
	// exceeds what it evidences now, since a red invoice reversed part of it (S5R-D20).
	LineOverused bool
	By           kernel.ActorRef
	Epoch        int64
	At           time.Time
}

// Status is the allocation's state: released once its case was rejected, replaced once a later
// allocation took its place, under review once its invoice was reversed or its invoice line's evidence is
// over-used after a partial reversal (S5R-D20), standing otherwise.
func (a *Allocation) Status() string {
	switch {
	case a.CaseStatus == Rejected:
		return Released
	case a.ReplacedBy != "":
		return Replaced
	case a.InvoiceStatus == "reversed" || a.LineOverused:
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
// number, unit and the quantity it evidences now — its printed quantity less what verified red invoices
// reversed of it (S5R-D20).
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
// the invoice line, with these, at most what it evidences and of the export line at most its quantity
// (TAX_EVIDENCE_EXCEEDED); a replacement leaves the allocation it replaces out of both, so it may take the
// same invoice line again (S5R-D20).
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
			rk := LineKey{InvoiceID: r.InvoiceID, LineNo: r.LineNo, Purpose: r.Purpose}
			if _, ok := used[rk]; ok {
				used[rk] = used[rk].Sub(r.Quantity)
			}
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
	PurchaseInvoiceID string // evidence_rolled_back: the invoice whose evidence was rolled back (S5R-D20)
	Evidence          []Evidence
	Reason            string
	By                kernel.ActorRef
	Epoch             int64
	At                time.Time
}

// next is SM-CNX-TAXCASE: the stage each state may record. S5R adds converted_to_exemption from expected and planned
// (S5R-D14) and evidence_rolled_back on a filed case, which keeps its state (S5R-D20).
var next = map[string][]string{
	Expected:  {Expected, Planned, ConvertedToExemption},
	Planned:   {Submitted, ConvertedToExemption},
	Submitted: {Approved, Rejected, EvidenceRolledBack},
	Approved:  {Received, EvidenceRolledBack},
	Received:  {EvidenceRolledBack},
}

// Filed reports whether a case in state s was submitted to the tax authority and not rejected: a red invoice
// reversing evidence it uses needs the evidence rolled back first (S5R-D20).
func Filed(s string) bool { return s == Submitted || s == Approved || s == Received }

// Record judges result r on case c with its earlier results and allocations: the stage must follow
// the case's state (INVALID_STATE; expected → expected records a new estimate); a plan needs
// counting evidence and none under review (INVALID_STATE); a conversion to an exemption needs a policy
// that refunds (INVALID_STATE); a rollback names an invoice the case's counting evidence uses
// (INVALID_KNOWN_VALUE) and not rolled back in the case before (INVALID_STATE); every amount of a case is
// in one currency (INVALID_KNOWN_VALUE). It returns the case's state after r: a rollback keeps it.
func Record(c *Case, results []*Result, allocations []*Allocation, r *Result) (string, error) {
	allowed := false
	for _, s := range next[c.Status] {
		allowed = allowed || s == r.Stage
	}
	if !allowed {
		return "", kernel.Reject(kernel.CodeInvalidState, "/args/stage", "The tax refund case is "+c.Status+": "+r.Stage+" does not follow")
	}
	switch r.Stage {
	case ConvertedToExemption:
		if !refunds(c.Policy.RefundMethod) {
			return "", kernel.Reject(kernel.CodeInvalidState, "/args/stage", "The case's export tax policy refunds nothing ("+c.Policy.RefundMethod+
				"): there is no refund to convert to an exemption")
		}
	case EvidenceRolledBack:
		uses := false
		for _, a := range allocations {
			uses = uses || (a.InvoiceID == r.PurchaseInvoiceID && a.Counts())
		}
		if !uses {
			return "", kernel.Reject(kernel.CodeInvalidKnownValue, "/args/purchase_invoice_id", "The case's evidence uses no line of invoice "+r.PurchaseInvoiceID)
		}
		for _, e := range results {
			if e.Stage == EvidenceRolledBack && e.PurchaseInvoiceID == r.PurchaseInvoiceID {
				return "", kernel.Reject(kernel.CodeInvalidState, "/args/purchase_invoice_id", "Invoice "+r.PurchaseInvoiceID+
					"'s evidence was rolled back in the case by result "+e.ID)
			}
		}
		return c.Status, nil
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

// Rebinding is a rebinding of an expected case (tax_refund_case.rebind_policy, S5R-D23): from which revision of its
// policy to which, the case's version it made, why, by whom, under which authority epoch, when.
type Rebinding struct {
	ID          string
	CaseID      string
	CaseVersion int64
	PolicyID    string
	From, To    Binding
	Reason      string
	By          kernel.ActorRef
	Epoch       int64
	At          time.Time
}
