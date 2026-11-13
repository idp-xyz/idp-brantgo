package api

import (
	"context"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// PurchaseInvoiceRef v1 (Context Map R55 CNX→STL, OHS / Published Language: "PurchaseInvoiceRef v1（发票、红蓝关系、匹配的
// 承诺行与金额；无锁查询，S5R-D20）"; STL-INV-013) is how STL reads CNX's purchase invoice facts for its payables: which
// verified invoices match the lines of the purchase commitments a payable settles, how red invoices reversed them, and
// their amounts. STL derives its invoice_reversed flag from it (a matched invoice that red invoices reversed, wholly or
// in part) on the reads of payables and purchase statements; it writes no state from it and subscribes to nothing:
// a payable changes only by a delivery correction or a confirmed payable_adjustment (S5R-D20).
//
// ResolvePurchaseInvoices is a lookup the caller runs before its own transaction, with an active grant only (no read
// permission; not audited), without a lock: an invoice's verified facts never change, and a red invoice verified
// after the lookup shows in the next one. It answers, in invoice ID order:
//
//   - every verified blue invoice (verified or reversed) with a match to a line of a commitment of CommitmentIDs,
//     and every verified red invoice that reverses one of them;
//   - every invoice of InvoiceIDs that is verified (blue or red) or reversed; others (imported, rejected, unknown,
//     another company's) are absent.
//
// Amounts are minor units of CurrencyCode as strings (net, tax and gross as printed, magnitudes; a red invoice states
// what it reverses). A blue invoice's lines show what red invoices reversed of them; its matches show what was taken
// back and what still stands toward the commitment line (StandingQuantity: zero once the invoice is reversed).
type PurchaseInvoiceLookups interface {
	ResolvePurchaseInvoices(ctx context.Context, q PurchaseInvoiceQuery) ([]PurchaseInvoiceRef, error)
}

// PurchaseInvoiceRefName is the lookup's name in a caller's sources or evidence.
const PurchaseInvoiceRefName = "cnx.purchase_invoice_ref.v1"

// MaxInvoiceLookups bounds the commitments and invoices one lookup names together.
const MaxInvoiceLookups = 100

// PurchaseInvoiceQuery names the purchase commitments whose matched invoices are asked for, and invoices by ID.
type PurchaseInvoiceQuery struct {
	CommitmentIDs []string
	InvoiceIDs    []string
}

// Validate checks the query: 1 to MaxInvoiceLookups identifiers in all.
func (q PurchaseInvoiceQuery) Validate() error {
	if n := len(q.CommitmentIDs) + len(q.InvoiceIDs); n < 1 || n > MaxInvoiceLookups {
		return schema("/commitment_ids")
	}
	for i, id := range q.CommitmentIDs {
		if !kernel.ValidIdentifier(id) {
			return schema("/commitment_ids/" + strconv.Itoa(i))
		}
	}
	for i, id := range q.InvoiceIDs {
		if !kernel.ValidIdentifier(id) {
			return schema("/invoice_ids/" + strconv.Itoa(i))
		}
	}
	return nil
}

// PurchaseInvoiceRef is what CNX says of one verified purchase invoice (PurchaseInvoiceRef v1).
type PurchaseInvoiceRef struct {
	PurchaseInvoiceID string
	Status            string // verified, or reversed (a blue invoice red invoices reversed wholly)
	Version           int64  // the invoice's row version: it moves with every red invoice that reverses part of it
	Kind              string
	Colour            string
	Number            string
	IssuedOn          string
	SupplierPartyID   string
	CurrencyCode      string
	MinorUnits        int
	NetMinor          string
	TaxMinor          string
	GrossMinor        string
	VerifiedAt        time.Time
	// RedOf is the blue invoice a red one reverses; ReversedBy the verified red invoices reversing a blue one, oldest
	// first; Reversal "" (none), partial or full.
	RedOf      string
	ReversedBy []string
	Reversal   string
	Lines      []PurchaseInvoiceLineRef
	Matches    []PurchaseInvoiceMatchRef
}

// Reversed reports whether red invoices reversed the invoice, wholly or in part (STL's invoice_reversed).
func (r PurchaseInvoiceRef) Reversed() bool { return r.Reversal != "" }

// PurchaseInvoiceLineRef is an invoice line: quantity in its unit, net and tax amounts as printed; for a blue invoice
// what red invoices reversed of it.
type PurchaseInvoiceLineRef struct {
	LineNo           int
	Unit             string
	Quantity         string
	NetMinor         string
	TaxMinor         string
	ReversedQuantity string
	ReversedNetMinor string
	ReversedTaxMinor string
}

// PurchaseInvoiceMatchRef is a match of an invoice line to a purchase commitment line. For a blue invoice: the quantity
// matched, what red invoices took back and what stands toward the commitment line now, and its state (standing or
// review); for a red invoice: the quantity it took back of the blue match (state reversal).
type PurchaseInvoiceMatchRef struct {
	LineNo           int
	CommitmentID     string
	OrderLineID      string
	Quantity         string
	Unit             string
	TakenBack        string
	StandingQuantity string
	Status           string
}
