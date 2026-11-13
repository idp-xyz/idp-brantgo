package api

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Purchase invoices (WBS S5-05b; tactical model CNX-AGG-PurchaseInvoice with its InvoiceLine and
// InvoiceMatch entities, SM-CNX-INVOICE, CNX-INV-005; UL CNX-PurchaseInvoice; baseline 22.1, 22.2,
// CN-R06, CN-AT05, CN-AT15, CN-AT18; S5 decisions E-1, D-3, D-17). A purchase invoice is the
// supplier's tax invoice (购进发票), never FUL's commercial invoice (UL collision "发票"). Its
// original (an EVD file), the candidate read from it (by a person or an AI agent) and the facts a
// human verified are kept apart; Brant issues no invoice and confirms no tax: the tax authority's
// channels do (UL exclusion).
//
//	purchase_invoice.import  human or AI (purchaser, tax specialist): file_id, the EVD file of the
//	                         original, and candidate, the facts read from it. A second invoice of the
//	                         same kind and number that is not rejected is INVOICE_DUPLICATE.
//	purchase_invoice.verify  human only (tax specialist): decision verify, with the verified facts
//	                         (supplier, buyer entity, kind, colour, number, issue date, currency,
//	                         lines with quantities, net and tax amounts as stated, totals, and for a
//	                         red invoice the blue one it reverses) and the matches of its lines to
//	                         PUR commitment lines (PurchaseLineRef v1, R37; Σ standing matches of a
//	                         commitment line above what it buys is PURCHASE_INVOICE_MATCH_EXCEEDED); or
//	                         decision reject with a reason. A buyer that is not the company's entity (POL
//	                         company_profile) is PARTY_ENTITY_MISMATCH: refused outright, no approved mode;
//	                         the supplier reissues the invoice (S5R-D21) and this one is rejected.
//	                         A red invoice reverses the blue one wholly or in part (S5R-D20): each red line
//	                         names the blue line it reverses by its line_no, in its unit, at most what
//	                         remains of it; its matches name the blue matches it takes back. One that
//	                         reverses what remained reverses the blue one: its matches and the tax evidence
//	                         allocated from it return to review (CN-R06, CN-AT05); a partial one leaves it
//	                         verified with less evidence. A blue invoice whose evidence a filed tax refund
//	                         case uses needs that case's evidence_rolled_back first (INVALID_STATE).
//
// Amounts are magnitudes in the invoice's currency (a POL currency revision); a red invoice states
// what it reverses. The totals must be the sums of the lines and net + tax the gross: CNX adds, it
// never computes a tax (CN-E01).
//
// Request members in snake_case: purchase_invoice.import {file_id, candidate}; purchase_invoice.verify
// {purchase_invoice_id, expected_version, decision, facts, matches [{line_no, commitment_id,
// order_line_id, quantity}] (for a red invoice: the blue matches it takes back), reason}; facts and candidate {kind,
// colour, number, issued_on, supplier
// {party_id, name, tax_id}, buyer {name, tax_id}, currency {id, revision}, lines [{line_no,
// description, quantity, unit, net_amount, tax_rate, tax_amount}], net_total, tax_total, gross_total,
// red_of {kind, number}}.

// Commands, reads and events of purchase invoices (role templates: purchaser and tax specialist
// import, the tax specialist verifies; humanOnlyCommands name purchase_invoice.verify).
const (
	CommandImportInvoice = "purchase_invoice.import"
	CommandVerifyInvoice = "purchase_invoice.verify"

	// Reads: an invoice with its candidate, verified facts, differences, matches and evidence
	// balances; the company's invoices.
	CommandGetInvoice    = "purchase_invoice.get"
	CommandInvoices      = "purchase_invoice.list"
	EventInvoiceVerified = "idp-brant.cnx.purchase_invoice_verified.v1"
)

// States of an invoice (SM-CNX-INVOICE).
const (
	InvoiceImported = "imported"
	InvoiceVerified = "verified"
	InvoiceRejected = "rejected"
	InvoiceReversed = "reversed"
)

// Kinds (票种) and colours of an invoice.
const (
	InvoiceSpecialVAT      = "special_vat"      // 增值税专用发票
	InvoiceOrdinaryVAT     = "ordinary_vat"     // 增值税普通发票
	InvoiceDigitalSpecial  = "digital_special"  // 数电票（增值税专用发票）
	InvoiceDigitalOrdinary = "digital_ordinary" // 数电票（普通发票）

	ColourBlue = "blue"
	ColourRed  = "red"
)

// Decisions of purchase_invoice.verify.
const (
	DecisionVerify = "verify"
	DecisionReject = "reject"
)

// States of a match: standing, or review once the invoice was reversed by a red one or a red one took back part of
// it; a red invoice's match is a reversal (it takes back a blue match, S5R-D20).
const (
	MatchStanding = "standing"
	MatchReview   = "review"
	MatchReversal = "reversal"
)

// How much of a blue invoice red invoices reversed (S5R-D20): part of it (it stays verified) or all (reversed).
const (
	ReversalPartial = "partial"
	ReversalFull    = "full"
)

// Limits of the invoice contract.
const (
	MaxInvoiceLines   = 100
	MaxInvoiceMatches = 200
	MaxInvoiceText    = 200
	MaxTaxID          = 32
)

var (
	invoiceNumber = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
	taxID         = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{0,31}$`)
	// rateFormat bounds a stated tax rate, such as 0.13.
	rateFormat = kernel.DecimalFormat{IntDigits: 1, Scale: 4}
)

func invoiceKind(k string) bool {
	return k == InvoiceSpecialVAT || k == InvoiceOrdinaryVAT || k == InvoiceDigitalSpecial || k == InvoiceDigitalOrdinary
}

// Date reports whether s is a calendar date YYYY-MM-DD.
func Date(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil && len(s) == 10
}

// InvoiceParty is a party an invoice names: its name and taxpayer identification number as
// printed and, for the supplier, the PTY party it is.
type InvoiceParty struct {
	PartyID string
	Name    string
	TaxID   string
}

// InvoiceRef names an invoice by kind and number.
type InvoiceRef struct {
	Kind   string
	Number string
}

// InvoiceLine is a line of an invoice: its quantity in a unit and its net and tax amounts as
// printed, with the tax rate printed (recorded, never applied).
type InvoiceLine struct {
	LineNo      int
	Description string
	Quantity    string
	Unit        string
	NetAmount   string
	TaxRate     string
	TaxAmount   string
}

// InvoiceFacts is what an invoice says, as a candidate or as verified.
type InvoiceFacts struct {
	Kind       string
	Colour     string
	Number     string
	IssuedOn   string
	Supplier   InvoiceParty
	Buyer      InvoiceParty
	Currency   CurrencyRef
	Lines      []InvoiceLine
	NetTotal   string
	TaxTotal   string
	GrossTotal string
	RedOf      *InvoiceRef
}

func amount(s string) bool { return positive(s, valueFormat, true) }

func (f InvoiceFacts) validate(path string) error {
	switch {
	case !invoiceKind(f.Kind):
		return schema(path + "/kind")
	case f.Colour != ColourBlue && f.Colour != ColourRed:
		return schema(path + "/colour")
	case !invoiceNumber.MatchString(f.Number):
		return schema(path + "/number")
	case !Date(f.IssuedOn):
		return schema(path + "/issued_on")
	case !kernel.ValidIdentifier(f.Supplier.PartyID):
		return schema(path + "/supplier/party_id")
	case !Text(f.Supplier.Name, MaxInvoiceText):
		return schema(path + "/supplier/name")
	case !taxID.MatchString(f.Supplier.TaxID):
		return schema(path + "/supplier/tax_id")
	case f.Buyer.PartyID != "":
		return schema(path + "/buyer/party_id")
	case !Text(f.Buyer.Name, MaxInvoiceText):
		return schema(path + "/buyer/name")
	case !taxID.MatchString(f.Buyer.TaxID):
		return schema(path + "/buyer/tax_id")
	case !kernel.ValidIdentifier(f.Currency.ID):
		return schema(path + "/currency/id")
	case f.Currency.Revision < 1 || f.Currency.Revision > maxVersion:
		return schema(path + "/currency/revision")
	case len(f.Lines) < 1 || len(f.Lines) > MaxInvoiceLines:
		return schema(path + "/lines")
	}
	seen := map[int]bool{}
	for i, l := range f.Lines {
		p := path + "/lines/" + strconv.Itoa(i)
		switch {
		case l.LineNo < 1 || l.LineNo > MaxInvoiceLines || seen[l.LineNo]:
			return schema(p + "/line_no")
		case !Text(l.Description, MaxInvoiceText):
			return schema(p + "/description")
		case !positive(l.Quantity, kernel.QuantityFormat, false):
			return schema(p + "/quantity")
		case !kernel.UnitCode(l.Unit).Valid():
			return schema(p + "/unit")
		case !amount(l.NetAmount):
			return schema(p + "/net_amount")
		case !positive(l.TaxRate, rateFormat, true):
			return schema(p + "/tax_rate")
		case !amount(l.TaxAmount):
			return schema(p + "/tax_amount")
		}
		if r, _ := kernel.ParseDecimal(l.TaxRate, rateFormat); r.Cmp(kernel.DecimalFromInt(1)) >= 0 {
			return schema(p + "/tax_rate")
		}
		seen[l.LineNo] = true
	}
	switch {
	case !amount(f.NetTotal):
		return schema(path + "/net_total")
	case !amount(f.TaxTotal):
		return schema(path + "/tax_total")
	case !amount(f.GrossTotal):
		return schema(path + "/gross_total")
	case (f.Colour == ColourRed) != (f.RedOf != nil):
		return schema(path + "/red_of")
	}
	if r := f.RedOf; r != nil {
		switch {
		case !invoiceKind(r.Kind):
			return schema(path + "/red_of/kind")
		case !invoiceNumber.MatchString(r.Number) || (r.Kind == f.Kind && r.Number == f.Number):
			return schema(path + "/red_of/number")
		}
	}
	return nil
}

// ImportInvoice imports a purchase invoice as a candidate: FileID is the EVD file of the original,
// Candidate what was read from it (by the actor, human or AI).
type ImportInvoice struct {
	IdempotencyKey string
	FileID         string
	Candidate      InvoiceFacts
}

// Validate checks the closed contract.
func (c ImportInvoice) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.FileID):
		return schema("/args/file_id")
	}
	return c.Candidate.validate("/args/candidate")
}

// InvoiceMatch matches quantity of invoice line LineNo to the PUR commitment line (CommitmentID,
// OrderLineID) — the goods bought, delivered or still to come (先货后票 and 先票后货).
type InvoiceMatch struct {
	LineNo       int
	CommitmentID string
	OrderLineID  string
	Quantity     string
}

// VerifyInvoice decides invoice PurchaseInvoiceID at ExpectedVersion (human only): Decision verify
// with the verified Facts and its Matches (optional, at most MaxInvoiceMatches: a blue invoice's to PUR commitment
// lines, a red invoice's the blue matches it takes back, S5R-D20); or reject with Reason, without facts or matches.
type VerifyInvoice struct {
	IdempotencyKey    string
	PurchaseInvoiceID string
	ExpectedVersion   int64
	Decision          string
	Facts             *InvoiceFacts
	Matches           []InvoiceMatch
	Reason            string
}

// Validate checks the closed contract.
func (c VerifyInvoice) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.PurchaseInvoiceID):
		return schema("/args/purchase_invoice_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	}
	switch c.Decision {
	case DecisionReject:
		switch {
		case c.Facts != nil:
			return schema("/args/facts")
		case len(c.Matches) != 0:
			return schema("/args/matches")
		case !Text(c.Reason, MaxReason):
			return schema("/args/reason")
		}
		return nil
	case DecisionVerify:
	default:
		return schema("/args/decision")
	}
	if c.Facts == nil {
		return schema("/args/facts")
	}
	if err := c.Facts.validate("/args/facts"); err != nil {
		return err
	}
	if len(c.Matches) > MaxInvoiceMatches {
		return schema("/args/matches")
	}
	lines := map[int]bool{}
	for _, l := range c.Facts.Lines {
		lines[l.LineNo] = true
	}
	seen := map[InvoiceMatch]bool{}
	for i, m := range c.Matches {
		p := "/args/matches/" + strconv.Itoa(i)
		key := InvoiceMatch{LineNo: m.LineNo, CommitmentID: m.CommitmentID, OrderLineID: m.OrderLineID}
		switch {
		case !lines[m.LineNo]:
			return schema(p + "/line_no")
		case !kernel.ValidIdentifier(m.CommitmentID):
			return schema(p + "/commitment_id")
		case !kernel.ValidIdentifier(m.OrderLineID) || seen[key]:
			return schema(p + "/order_line_id")
		case !positive(m.Quantity, kernel.QuantityFormat, false):
			return schema(p + "/quantity")
		}
		seen[key] = true
	}
	if c.Reason != "" && !Text(c.Reason, MaxReason) {
		return schema("/args/reason")
	}
	return nil
}

func (ImportInvoice) cnxCommand() {}
func (VerifyInvoice) cnxCommand() {}

// InvoiceReceipt is the answer of an invoice command: the receipt (ObjectID the invoice, its row
// version and status), the invoice's kind and number and, for a verified red invoice, the blue
// invoice it reversed.
type InvoiceReceipt struct {
	Receipt
	PurchaseInvoiceID string
	Kind              string
	Number            string
	ReversedInvoiceID string
}

// InvoiceCommands is the purchase invoice part of CNX's command entry point.
type InvoiceCommands interface {
	ImportInvoice(ctx context.Context, cmd ImportInvoice) (InvoiceReceipt, error)
	VerifyInvoice(ctx context.Context, cmd VerifyInvoice) (InvoiceReceipt, error)
}

// FactsView is an invoice's facts as stored, with the code and minor units of its currency.
type FactsView struct {
	InvoiceFacts
	CurrencyCode string
	MinorUnits   int
}

// SourceView names who or what a value came from: the import (an actor of kind human or ai, with
// the original file) or the verification (a human).
type SourceView struct {
	Step   string // import | verify
	Actor  kernel.ActorRef
	FileID string
	At     time.Time
}

// DifferenceView is a field where the verified facts differ from the candidate: the JSON pointer
// of the field within the facts, both values and where each came from (CN-AT18).
type DifferenceView struct {
	Field           string
	Candidate       string
	Verified        string
	CandidateSource SourceView
	VerifiedSource  SourceView
}

// MatchView is a match of an invoice line to a PUR commitment line: what PUR said at the match (the
// supplier, purchase order number, quantity bought and verified as delivered) and its state now; TakenBack is what red
// invoices took back of a blue match (S5R-D20). A red invoice's matches are what it took back (state reversal).
type MatchView struct {
	LineNo            int
	CommitmentID      string
	OrderLineID       string
	Quantity          string
	TakenBack         string
	Unit              string
	SupplierID        string
	PONumber          string
	PurchaseQuantity  string
	DeliveredQuantity string
	Status            string
	MatchedAt         time.Time
}

// EvidenceBalanceView is the tax evidence balance of a verified invoice line for one purpose:
// its quantity, what standing allocations use, what declarations finished before the cutoff consumed
// (OpeningConsumed, S5 C10: "" without an opening balance) and what remains (CN-R04).
type EvidenceBalanceView struct {
	LineNo          int
	Unit            string
	Purpose         string
	Quantity        string
	Reversed        string // what verified red invoices reversed of the line (S5R-D20); Available is net of it
	Allocated       string
	OpeningConsumed string
	Available       string
}

// InvoiceView is a purchase invoice: its state, the original, the candidate and who produced it,
// the verified facts and who verified them, the differences between both, its matches, the tax
// evidence balances of its lines, and the red invoice that reversed it or the blue one it
// reversed.
type InvoiceView struct {
	PurchaseInvoiceID string
	Status            string
	RowVersion        int64
	File              LicenseFileView
	Candidate         FactsView
	ImportedBy        kernel.ActorRef
	ImportedAt        time.Time
	Verified          *FactsView
	Decision          string
	Reason            string
	DecidedBy         kernel.ActorRef
	DecidedAt         time.Time
	Differences       []DifferenceView
	Matches           []MatchView
	Balances          []EvidenceBalanceView
	ReversesInvoiceID string
	ReversedByID      string   // the red invoice that reversed what remained (state reversed)
	ReversedByIDs     []string // every verified red invoice that reverses it, oldest first (S5R-D20)
	Reversal          string   // "" none, partial or full (S5R-D20)
	AuthorityEpoch    int64
	CheckedAt         time.Time
}

// InvoiceSummary is one invoice of the company.
type InvoiceSummary struct {
	PurchaseInvoiceID string
	Status            string
	Kind              string
	Colour            string
	Number            string
	IssuedOn          string
	SupplierName      string
	GrossTotal        string
	CurrencyCode      string
	RowVersion        int64
	ImportedBy        kernel.ActorRef
	ImportedAt        time.Time
}

// InvoiceQueries is the purchase invoice part of CNX's audited read side (cnx.read).
type InvoiceQueries interface {
	// GetInvoice returns an invoice of the company, audited as purchase_invoice.get; NOT_FOUND
	// otherwise.
	GetInvoice(ctx context.Context, invoiceID string) (InvoiceView, error)
	// Invoices returns the company's invoices, newest first, audited as purchase_invoice.list.
	Invoices(ctx context.Context) ([]InvoiceSummary, error)
}
