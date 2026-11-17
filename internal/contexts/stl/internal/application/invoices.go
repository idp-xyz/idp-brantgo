package application

import (
	"context"
	"slices"
	"sort"

	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// PurchaseInvoices is STL's port to CNX for purchase invoices (Context Map R55 CNX→STL, OHS / Published Language:
// "PurchaseInvoiceRef v1（发票、红蓝关系、匹配的承诺行与金额；无锁查询，S5R-D20）", guardLock false; STL-INV-013). STL
// reads it before its transaction, never locks a CNX row and writes nothing from it: it derives the invoice_reversed
// flag of a payable and checks the invoice a payable adjustment references.
//
// ResolvePurchaseInvoices answers, in invoice ID order, the verified blue invoices (verified or reversed) matched to a
// line of a commitment of CommitmentIDs with the verified red invoices reversing them, and the invoices of InvoiceIDs
// CNX verified (blue or red) or reversed; others are absent. CNX's PurchaseInvoiceLookups (cnxapi.PurchaseInvoiceRef
// v1, S5R unit R10) answers it through adapters/invoices; without the port (Upstream.Invoices nil) CNX shows STL no
// invoice.
type PurchaseInvoices interface {
	ResolvePurchaseInvoices(ctx context.Context, q PurchaseInvoiceQuery) ([]PurchaseInvoice, error)
}

// PurchaseInvoiceQuery names the commitments whose matched invoices STL asks for, and invoices by ID.
type PurchaseInvoiceQuery struct {
	CommitmentIDs []string
	InvoiceIDs    []string
}

// Colours and reversals of a purchase invoice (CNX).
const (
	InvoiceBlue     = "blue"
	InvoiceRed      = "red"
	InvoiceReversed = "reversed"
)

// PurchaseInvoice is what CNX says of one verified purchase invoice (PurchaseInvoiceRef v1, the members STL uses): its
// state (verified, or reversed when red invoices reversed it wholly), colour, number, supplier and currency; the blue
// invoice a red one reverses (RedOf), the red ones reversing a blue one (ReversedBy), Reversal "" (none), partial or
// full; and its matches to commitment lines.
type PurchaseInvoice struct {
	PurchaseInvoiceID string
	Status            string
	Colour            string
	Number            string
	SupplierPartyID   string
	CurrencyCode      string
	RedOf             string
	ReversedBy        []string
	Reversal          string
	Matches           []PurchaseInvoiceMatch
}

// PurchaseInvoiceMatch is a match of an invoice line to a commitment line.
type PurchaseInvoiceMatch struct {
	LineNo       int
	CommitmentID string
	OrderLineID  string
}

// Reversed reports whether red invoices reversed the invoice, wholly or in part (STL's invoice_reversed).
func (p PurchaseInvoice) Reversed() bool { return p.Reversal != "" }

// invoiceFacts are what CNX showed of a purchase statement's invoices: whether CNX answered at all.
type invoiceFacts struct {
	shown    bool
	invoices []PurchaseInvoice
}

// statementCommitments are the commitment lines a purchase statement settles, by commitment.
func statementCommitments(st *settlement.Statement) map[string][]string {
	out := map[string][]string{}
	for _, l := range st.Lines {
		if l.CommitmentID != "" && !slices.Contains(out[l.CommitmentID], l.OrderLineID) {
			out[l.CommitmentID] = append(out[l.CommitmentID], l.OrderLineID)
		}
	}
	return out
}

// statementInvoices asks CNX, before STL's transaction, for the invoices of the commitments purchase statement st
// settles (and of ids). Without the port CNX shows nothing.
func (s Service) statementInvoices(ctx context.Context, st *settlement.Statement, ids ...string) (invoiceFacts, error) {
	if s.Upstream.Invoices == nil || st == nil || st.Kind != settlement.KindPurchase {
		return invoiceFacts{}, nil
	}
	commitments := statementCommitments(st)
	q := PurchaseInvoiceQuery{InvoiceIDs: ids}
	for id := range commitments {
		q.CommitmentIDs = append(q.CommitmentIDs, id)
	}
	sort.Strings(q.CommitmentIDs)
	if len(q.CommitmentIDs)+len(q.InvoiceIDs) == 0 {
		return invoiceFacts{shown: true}, nil
	}
	list, err := s.Upstream.Invoices.ResolvePurchaseInvoices(ctx, q)
	return invoiceFacts{shown: true, invoices: list}, err
}

// touches reports whether invoice p is matched to a commitment line statement st settles.
func touches(p PurchaseInvoice, lines map[string][]string) bool {
	return slices.ContainsFunc(p.Matches, func(m PurchaseInvoiceMatch) bool {
		return slices.Contains(lines[m.CommitmentID], m.OrderLineID)
	})
}

// invoiceFlag derives a purchase statement's invoice_reversed flag (S5R-D20, STL-INV-013): a blue invoice matched to a
// commitment line the statement settles that red invoices reversed, wholly or in part. It returns the flag and the
// invoices it rests on (the reversed blue ones and the red ones reversing them).
func invoiceFlag(st *settlement.Statement, f invoiceFacts) (bool, []api.PurchaseInvoiceView) {
	views := []api.PurchaseInvoiceView{}
	if !f.shown {
		return false, views
	}
	lines := statementCommitments(st)
	reversed := map[string]bool{}
	for _, p := range f.invoices {
		if p.Colour == InvoiceBlue && p.Reversed() && touches(p, lines) {
			reversed[p.PurchaseInvoiceID] = true
		}
	}
	for _, p := range f.invoices {
		if reversed[p.PurchaseInvoiceID] || (p.Colour == InvoiceRed && reversed[p.RedOf]) {
			views = append(views, api.PurchaseInvoiceView{PurchaseInvoiceID: p.PurchaseInvoiceID, Colour: p.Colour, Status: p.Status, Number: p.Number,
				RedOf: p.RedOf, ReversedBy: nonNilStrings(p.ReversedBy), Reversal: p.Reversal})
		}
	}
	return len(reversed) > 0, views
}

// adjustmentInvoice checks the invoice a payable adjustment of purchase statement st references (S5R-D20): CNX shows it
// verified, of the statement's supplier and currency, matched to — or, red, reversing an invoice matched to — a
// commitment line the statement settles; an allowance rests on a red invoice. REFERENCE_INVALID at
// /args/purchase_invoice_id otherwise.
func adjustmentInvoice(st *settlement.Statement, kind, id string, f invoiceFacts) error {
	byID := map[string]PurchaseInvoice{}
	for _, p := range f.invoices {
		byID[p.PurchaseInvoiceID] = p
	}
	p, ok := byID[id]
	refuse := func(reason string) error {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/purchase_invoice_id", reason)
	}
	switch {
	case !f.shown || !ok:
		return refuse("CNX shows no such verified purchase invoice")
	case p.SupplierPartyID != st.CounterpartyID:
		return refuse("The invoice is the supplier " + p.SupplierPartyID + "'s, the payable " + st.CounterpartyID + "'s")
	case p.CurrencyCode != st.Currency().Code():
		return refuse("The invoice is in " + p.CurrencyCode + ", the payable in " + st.Currency().Code())
	case kind == settlement.AdjustmentPriceAllowance && p.Colour != InvoiceRed:
		return refuse("A price allowance rests on a red invoice")
	}
	lines := statementCommitments(st)
	if touches(p, lines) {
		return nil
	}
	if blue, ok := byID[p.RedOf]; ok && p.Colour == InvoiceRed && touches(blue, lines) {
		return nil
	}
	return refuse("The invoice is matched to no commitment line the statement settles")
}
