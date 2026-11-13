package taxrefund

import (
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Opening refund material (S5 unit C10, WBS S5-20; tactical model CNX-AGG-TaxRefundCase value object
// OpeningPosition; Context Map MD-51; baseline 11.2; CN-R04, CN-R09; CN-AT16 CNX half, BT17).
//
// An invoice balance states what refund declarations finished before the cutoff consumed of a verified blue
// invoice's lines, per purpose: that consumption counts next to the cases' allocations for ever — an external
// result consumed it, no local action returns it (CN-R09). An opening case is a case of an export declared before
// the cutoff, opened in the state its declaration had then (expected, planned, submitted or approved) with the
// allocations its declaration uses: they are judged as tax_refund_case.allocate judges them (CNX-INV-004), and an
// external rejection of the case releases them as any case's.

// Positions an opening case opens in: the states of SM-CNX-TAXCASE before an external outcome.
var Positions = []string{Expected, Planned, Submitted, Approved}

// OpenAllocations judges the allocations of an opening case x.Case, opened in its position: Allocate's rules,
// whatever that position is.
func OpenAllocations(x Context, in []AllocateInput) ([]*Allocation, error) { return allocate(x, in) }

// Consumption is what declarations finished before the cutoff consumed of an invoice line for a purpose.
type Consumption struct {
	OpeningID string
	InvoiceID string
	LineNo    int
	Purpose   string
	Consumed  kernel.Decimal
	Unit      kernel.UnitCode
}

// BalanceInput is one line and purpose of an invoice balance as stated.
type BalanceInput struct {
	LineNo   int
	Purpose  string
	Consumed kernel.Decimal
}

// ConsumedAtCutoff judges invoice balance openingID of invoice invoiceID: the invoice is a verified blue one
// (INVALID_STATE; REFERENCE_INVALID when the company has none of its lines), each line is one of its verified lines
// (REFERENCE_INVALID), and per purpose what counting allocations and opening consumption take of the line already,
// with this, stays within its quantity (TAX_EVIDENCE_EXCEEDED). lines are the invoice's lines asked for, used what
// is taken of each line and purpose now.
func ConsumedAtCutoff(openingID, invoiceID string, lines map[InvoiceLineRef]InvoiceLine, used map[LineKey]kernel.Decimal, in []BalanceInput) ([]Consumption, error) {
	var out []Consumption
	for i, b := range in {
		p := "/args/balances/" + strconv.Itoa(i)
		l, ok := lines[InvoiceLineRef{InvoiceID: invoiceID, LineNo: b.LineNo}]
		switch {
		case !ok:
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, p+"/line_no", "No such verified invoice line")
		case l.InvoiceStatus != "verified":
			return nil, kernel.Reject(kernel.CodeInvalidState, "/args/purchase_invoice_id", "The invoice is "+l.InvoiceStatus)
		case l.Colour != "blue":
			return nil, kernel.Reject(kernel.CodeInvalidState, "/args/purchase_invoice_id", "A red invoice evidences nothing")
		}
		k := LineKey{InvoiceID: invoiceID, LineNo: b.LineNo, Purpose: b.Purpose}
		if sum := used[k].Add(b.Consumed); sum.Cmp(l.Quantity) > 0 {
			return nil, exceeded(p+"/consumed", "Invoice line "+strconv.Itoa(b.LineNo)+" has "+l.Quantity.Sub(used[k]).String()+" left for "+b.Purpose)
		}
		out = append(out, Consumption{OpeningID: openingID, InvoiceID: invoiceID, LineNo: b.LineNo, Purpose: b.Purpose, Consumed: b.Consumed, Unit: l.Unit})
	}
	return out, nil
}

// Kinds of an opening item.
const (
	OpeningInvoiceBalance = "invoice_balance"
	OpeningRefundCase     = "refund_case"
)

// CodeOpeningBalanceDuplicate: the source document, the invoice's balance or the export's case is opening material
// already (catalog contexts STL／CNX).
const CodeOpeningBalanceDuplicate = "OPENING_BALANCE_DUPLICATE"

// Duplicate refuses opening material recorded already, at path, saying by what.
func Duplicate(path, reason string) *kernel.Rejection {
	return kernel.RejectAs(CodeOpeningBalanceDuplicate, Categories[CodeOpeningBalanceDuplicate], path, reason)
}

// Opening is an opening item: an invoice balance (InvoiceID) or an opening case (CaseID, with the export it states
// and the position it opened in).
type Opening struct {
	ID              string
	Kind            string
	CutoffAt        time.Time
	SourceDocument  string
	Evidence        []Evidence
	InvoiceID       string
	CaseID          string
	ExportReference string
	ExportedAt      time.Time
	OrderReference  string
	Position        string
	By              kernel.ActorRef
	Epoch           int64
	At              time.Time
}
