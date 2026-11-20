// Package purchaseinvoice holds CNX's PurchaseInvoice (tactical model CNX-AGG-PurchaseInvoice with
// its InvoiceLine and InvoiceMatch entities and the value objects InvoiceKind, InvoiceNumber,
// RedBlueLink and BuyerEntity; SM-CNX-INVOICE; CNX-INV-005; UL CNX-PurchaseInvoice; baseline 22.1,
// 22.2, CN-R06): the supplier's tax invoice as an original, a candidate read from it and the facts
// a human verified, kept apart, with the matches of its lines to PUR commitment lines.
//
// The invoice is the root (CAS on its row version). Its states: imported → verified | rejected;
// a verified blue invoice stays verified when a red invoice reverses part of it and becomes reversed
// with the red invoice that reverses what remained (S5R-D20, reversal.go). A reversal deletes nothing:
// the matches it touches are under review and the tax evidence allocated from a line is under review
// while it exceeds what the line evidences now. Uniqueness is by (kind, number) among invoices that are
// not rejected. A buyer that is not the company's entity is refused: the supplier reissues the invoice,
// there is no approved mode (S5R-D21).
package purchaseinvoice

import (
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes the domain decides with besides the kernel's (error catalog V0.1, conflict).
const (
	CodeInvoiceDuplicate    = "INVOICE_DUPLICATE"
	CodePartyEntityMismatch = "PARTY_ENTITY_MISMATCH"
	// CodeMatchExceeded: the quantity standing matches take of a purchase commitment line would exceed what it buys
	// (error catalog: PURCHASE_INVOICE_MATCH_EXCEEDED, 409, S5R).
	CodeMatchExceeded = "PURCHASE_INVOICE_MATCH_EXCEEDED"
)

// Categories are the categories of the domain's own codes.
var Categories = map[string]kernel.Category{
	CodeInvoiceDuplicate:    kernel.CategoryConflict,
	CodePartyEntityMismatch: kernel.CategoryConflict,
	CodeMatchExceeded:       kernel.CategoryConflict,
}

func reject(code, path, reason string) *kernel.Rejection {
	return kernel.RejectAs(code, Categories[code], path, reason)
}

// States, colours and decisions.
const (
	Imported = "imported"
	Verified = "verified"
	Rejected = "rejected"
	Reversed = "reversed"

	Blue = "blue"
	Red  = "red"

	Verify = "verify"
	Reject = "reject"

	Standing = "standing"
	Review   = "review"
	// Reversal is the state of a red invoice's match: it takes back part of a blue invoice's match (S5R-D20).
	Reversal = "reversal"
)

// Party is a party an invoice names.
type Party struct {
	PartyID string `json:"party_id,omitempty"`
	Name    string `json:"name"`
	TaxID   string `json:"tax_id"`
}

// Ref names an invoice by kind and number.
type Ref struct {
	Kind   string `json:"kind"`
	Number string `json:"number"`
}

// Line is an invoice line as printed.
type Line struct {
	LineNo      int    `json:"line_no"`
	Description string `json:"description"`
	Quantity    string `json:"quantity"`
	Unit        string `json:"unit"`
	NetAmount   string `json:"net_amount"`
	TaxRate     string `json:"tax_rate"`
	TaxAmount   string `json:"tax_amount"`
}

// Currency is the POL currency revision of an invoice and what it said.
type Currency struct {
	ConfigID   string `json:"id"`
	Revision   int64  `json:"revision"`
	Code       string `json:"code"`
	MinorUnits int    `json:"minor_units"`
}

// Kernel returns the kernel currency.
func (c Currency) Kernel() (kernel.Currency, error) { return kernel.NewCurrency(c.Code, c.MinorUnits) }

// Facts is what an invoice says, as a candidate or verified. Amounts are decimal magnitudes in
// Currency, as the contract carries them.
type Facts struct {
	Kind       string   `json:"kind"`
	Colour     string   `json:"colour"`
	Number     string   `json:"number"`
	IssuedOn   string   `json:"issued_on"`
	Supplier   Party    `json:"supplier"`
	Buyer      Party    `json:"buyer"`
	Currency   Currency `json:"currency"`
	Lines      []Line   `json:"lines"`
	NetTotal   string   `json:"net_total"`
	TaxTotal   string   `json:"tax_total"`
	GrossTotal string   `json:"gross_total"`
	RedOf      *Ref     `json:"red_of,omitempty"`
}

// Line returns the line numbered no.
func (f Facts) Line(no int) (Line, bool) {
	for _, l := range f.Lines {
		if l.LineNo == no {
			return l, true
		}
	}
	return Line{}, false
}

// CheckAmounts parses every amount in the invoice's currency — more digits than its minor units is
// INVALID_KNOWN_VALUE — and checks that the totals are the sums of the lines and that net plus tax
// is the gross (exact sums; no tax is computed, CN-E01). path is the facts' pointer.
func (f Facts) CheckAmounts(path string) error {
	c, err := f.Currency.Kernel()
	if err != nil {
		return kernel.Reject(kernel.CodeReferenceInvalid, path+"/currency", "")
	}
	parse := func(s, p string) (kernel.Money, error) {
		m, err := kernel.ParseAmount(c, s)
		if err != nil {
			return kernel.Money{}, kernel.Reject(kernel.CodeInvalidKnownValue, p, "More digits than "+c.Code()+" has minor units")
		}
		return m, nil
	}
	net, _ := kernel.NewMoney(c, 0)
	tax := net
	for i, l := range f.Lines {
		p := path + "/lines/" + strconv.Itoa(i)
		n, err := parse(l.NetAmount, p+"/net_amount")
		if err != nil {
			return err
		}
		t, err := parse(l.TaxAmount, p+"/tax_amount")
		if err != nil {
			return err
		}
		net, _ = net.Add(n)
		tax, _ = tax.Add(t)
	}
	totals := map[string]kernel.Money{}
	for _, x := range []struct{ name, value string }{{"net_total", f.NetTotal}, {"tax_total", f.TaxTotal}, {"gross_total", f.GrossTotal}} {
		m, err := parse(x.value, path+"/"+x.name)
		if err != nil {
			return err
		}
		totals[x.name] = m
	}
	gross, _ := totals["net_total"].Add(totals["tax_total"])
	switch {
	case !net.Equal(totals["net_total"]):
		return kernel.Reject(kernel.CodeInvalidKnownValue, path+"/net_total", "The net total is not the sum of the lines")
	case !tax.Equal(totals["tax_total"]):
		return kernel.Reject(kernel.CodeInvalidKnownValue, path+"/tax_total", "The tax total is not the sum of the lines")
	case !gross.Equal(totals["gross_total"]):
		return kernel.Reject(kernel.CodeInvalidKnownValue, path+"/gross_total", "The gross total is not net plus tax")
	}
	return nil
}

// Company is the company's entity as POL's company_profile states it: the buyer every purchase
// invoice of the company must name.
type Company struct {
	LegalName      string
	RegistrationNo string
}

// CheckBuyer refuses a buyer that is not the company's entity (CNX-INV-005 "受票主体与本公司不一致时直接拒绝，供应商须
// 重开", S5R-D21, CN-AT15): its taxpayer number and its name must be the company profile's. There is no approved
// mode: a VAT invoice's buyer must be right, the supplier reissues it. A buyer field never names another company of
// the tenant: the invoice belongs to the caller's company.
func CheckBuyer(f Facts, c Company, path string) error {
	switch {
	case c.RegistrationNo == "":
		return kernel.Reject(kernel.CodeReferenceInvalid, path+"/buyer", "The company has no profile in force")
	case f.Buyer.TaxID != c.RegistrationNo:
		return reject(CodePartyEntityMismatch, path+"/buyer/tax_id", "The buyer is not the company's entity")
	case f.Buyer.Name != c.LegalName:
		return reject(CodePartyEntityMismatch, path+"/buyer/name", "The buyer is not the company's entity")
	}
	return nil
}

// Difference is a field where the verified facts differ from the candidate.
type Difference struct {
	Field     string
	Candidate string
	Verified  string
}

func flatten(f Facts) map[string]string {
	raw, _ := json.Marshal(f)
	var v any
	_ = json.Unmarshal(raw, &v)
	out := map[string]string{}
	var walk func(p string, v any)
	walk = func(p string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(p+"/"+k, e)
			}
		case []any:
			for i, e := range x {
				walk(p+"/"+strconv.Itoa(i), e)
			}
		case nil:
		default:
			b, _ := json.Marshal(x)
			s := string(b)
			if str, ok := x.(string); ok {
				s = str
			}
			out[p] = s
		}
	}
	walk("", v)
	return out
}

// Differences returns every field where verified differs from candidate, by JSON pointer (CN-AT18
// "显示来源与差异"); a field one of them lacks shows "" on that side.
func Differences(candidate, verified Facts) []Difference {
	c, v := flatten(candidate), flatten(verified)
	var keys []string
	for k := range c {
		keys = append(keys, k)
	}
	for k := range v {
		if _, ok := c[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := []Difference{}
	for _, k := range keys {
		if c[k] != v[k] {
			out = append(out, Difference{Field: k, Candidate: c[k], Verified: v[k]})
		}
	}
	return out
}

// Invoice is the PurchaseInvoice root.
type Invoice struct {
	ID          string
	Status      string
	RowVersion  int64
	FileID      string
	FileSHA256  string
	Candidate   Facts
	ImportedBy  kernel.ActorRef
	ImportEpoch int64
	ImportedAt  time.Time
	Verified    *Facts
	Decision    string
	Reason      string
	DecidedBy   kernel.ActorRef
	DecideEpoch int64
	DecidedAt   time.Time
	RedOfID     string // the blue invoice a verified red one reversed
	ReversedBy  string // the red invoice that reversed this one
}

// Key returns the (kind, number) the invoice holds: the verified facts' once verified.
func (i *Invoice) Key() Ref {
	if i.Verified != nil {
		return Ref{Kind: i.Verified.Kind, Number: i.Verified.Number}
	}
	return Ref{Kind: i.Candidate.Kind, Number: i.Candidate.Number}
}

// Facts returns the verified facts, or the candidate's before verification.
func (i *Invoice) Facts() Facts {
	if i.Verified != nil {
		return *i.Verified
	}
	return i.Candidate
}

// CheckVersion compares the caller's expected row version with i's.
func CheckVersion(i *Invoice, expected int64) error {
	if i.RowVersion != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// CheckDecidable refuses a decision on an invoice that is not imported (SM-CNX-INVOICE).
func CheckDecidable(i *Invoice) error {
	if i.Status != Imported {
		return kernel.Reject(kernel.CodeInvalidState, "/args/purchase_invoice_id", "The invoice is "+i.Status)
	}
	return nil
}

// CheckReversible checks the blue invoice a red one names: verified, blue, from the same supplier to
// the same buyer, in the same currency (CN-R06). path is the red facts' red_of pointer.
func CheckReversible(original *Invoice, red Facts, path string) error {
	if original == nil {
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "No such invoice")
	}
	f := original.Facts()
	switch {
	case original.Status != Verified || f.Colour != Blue:
		return kernel.Reject(kernel.CodeInvalidState, path, "The invoice is "+original.Status+", not a verified blue invoice")
	case f.Supplier.TaxID != red.Supplier.TaxID || f.Supplier.PartyID != red.Supplier.PartyID:
		return reject(CodePartyEntityMismatch, path, "The red invoice's supplier is not the original's")
	case f.Buyer != red.Buyer:
		return reject(CodePartyEntityMismatch, path, "The red invoice's buyer is not the original's")
	case f.Currency.Code != red.Currency.Code:
		return kernel.Reject(kernel.CodeInvalidKnownValue, path, "The red invoice's currency is not the original's")
	}
	return nil
}

// LineRef names a PUR commitment line (PurchaseLineRef v1).
type LineRef struct {
	CommitmentID string
	OrderLineID  string
}

// PurchaseLine is what PUR says of a commitment line (PurchaseLineRef v1, R37): its state code (""
// for a line of an issued or confirmed commitment), supplier, unit, purchase order number, the
// quantity bought and the quantity verified as delivered.
type PurchaseLine struct {
	Code              string
	Status            string
	SupplierID        string
	Unit              string
	PONumber          string
	PurchaseQuantity  kernel.Decimal
	DeliveredQuantity kernel.Decimal
}

// MatchInput is one match the verifier states.
type MatchInput struct {
	LineNo   int
	Ref      LineRef
	Quantity kernel.Decimal
}

// Match is a match of an invoice line to a commitment line, with what PUR said at the match.
type Match struct {
	InvoiceID         string
	LineNo            int
	Ref               LineRef
	Quantity          kernel.Decimal
	Unit              string
	SupplierID        string
	PONumber          string
	PurchaseQuantity  kernel.Decimal
	DeliveredQuantity kernel.Decimal
	MatchedAt         time.Time
}

// Status is the match's state as its invoice's state and the take-backs of red invoices decide it (CN-R06
// "红冲或作废使相关匹配待复核", S5R-D20): a red invoice's match is a reversal; a blue one's is under review once the
// invoice was reversed or a red invoice took back part of it, standing otherwise.
func (m Match) Status(invoiceStatus, colour string, takenBack kernel.Decimal) string {
	switch {
	case colour == Red:
		return Reversal
	case invoiceStatus == Reversed || takenBack.Sign() > 0:
		return Review
	}
	return Standing
}

// CheckMatches judges the matches of verified blue facts f against PUR's lines (absent: PUR does not
// resolve it for the caller) and the quantity standing matches of other invoices already take of
// each commitment line: the commitment line must be issued or confirmed (INVALID_STATE), its
// supplier the invoice's (PARTY_ENTITY_MISMATCH), its unit the invoice line's (INVALID_KNOWN_VALUE:
// no conversion is guessed), Σ matched of an invoice line at most its quantity (INVALID_KNOWN_VALUE) and Σ
// standing matched of a commitment line at most what it buys (PURCHASE_INVOICE_MATCH_EXCEEDED, S5R; standing:
// the matches of verified blue invoices less what red invoices took back). A match records the
// quantity delivered when it was made: an invoice may come before the goods (先票后货).
func CheckMatches(f Facts, invoiceID string, in []MatchInput, lines map[LineRef]PurchaseLine, standing map[LineRef]kernel.Decimal, at time.Time) ([]Match, error) {
	perLine := map[int]kernel.Decimal{}
	perRef := map[LineRef]kernel.Decimal{}
	out := []Match{}
	for i, m := range in {
		p := "/args/matches/" + strconv.Itoa(i)
		l, ok := lines[m.Ref]
		il, _ := f.Line(m.LineNo)
		switch {
		case !ok:
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, p+"/commitment_id", "PUR resolves no such commitment line")
		case l.Code != "":
			return nil, kernel.Reject(kernel.CodeInvalidState, p+"/commitment_id", "The purchase commitment is "+l.Status+", not issued")
		case l.SupplierID != f.Supplier.PartyID:
			return nil, reject(CodePartyEntityMismatch, p+"/commitment_id", "The commitment's supplier is not the invoice's")
		case l.Unit != il.Unit:
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/quantity", "The commitment line's unit is "+l.Unit+", the invoice line's "+il.Unit)
		}
		q, _ := kernel.ParseDecimal(il.Quantity, kernel.QuantityFormat)
		sum := perLine[m.LineNo].Add(m.Quantity)
		if sum.Cmp(q) > 0 {
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/quantity", "More than invoice line "+strconv.Itoa(m.LineNo)+" holds")
		}
		perLine[m.LineNo] = sum
		ref := perRef[m.Ref].Add(m.Quantity)
		if standing[m.Ref].Add(ref).Cmp(l.PurchaseQuantity) > 0 {
			return nil, reject(CodeMatchExceeded, p+"/quantity", "Invoices would match "+standing[m.Ref].Add(ref).String()+" of the "+
				l.PurchaseQuantity.String()+" the commitment line buys")
		}
		perRef[m.Ref] = ref
		out = append(out, Match{InvoiceID: invoiceID, LineNo: m.LineNo, Ref: m.Ref, Quantity: m.Quantity, Unit: l.Unit, SupplierID: l.SupplierID,
			PONumber: l.PONumber, PurchaseQuantity: l.PurchaseQuantity, DeliveredQuantity: l.DeliveredQuantity, MatchedAt: at})
	}
	return out, nil
}

// Refs returns the commitment lines of in, sorted, once each: the order their match locks are taken.
func Refs(in []MatchInput) []LineRef {
	var out []LineRef
	for _, m := range in {
		if !slices.Contains(out, m.Ref) {
			out = append(out, m.Ref)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].CommitmentID != out[b].CommitmentID {
			return out[a].CommitmentID < out[b].CommitmentID
		}
		return out[a].OrderLineID < out[b].OrderLineID
	})
	return out
}
