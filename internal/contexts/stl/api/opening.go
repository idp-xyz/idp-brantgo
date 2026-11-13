package api

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Opening balances (S5 unit C10, WBS S5-20; tactical model STL-AGG-SettlementStatement entity OpeningBalance,
// value object Cutoff; STL-INV-012; Context Map MD-50, R75 期初来源; baseline 11.2 期初、防重、切换; BT17, CN-AT16 STL
// half; S5 plan D-3, D-4).
//
//	opening_balance.record  human only (treasury): registers one item the books held at the cutoff instant cutoff_at,
//	                        from a source document (source {document, issued_at, amount}: its reference in the
//	                        books it comes from, when it was issued and its whole amount) with EVD evidence:
//	                          receivable           an open receivable of customer counterparty_id: an opening sales
//	                                               statement — no shipment source, no Brant order (order_reference
//	                                               states the order it came from) — of amount, due in installments
//	                                               [{amount, due_at}] that add up to it;
//	                          payable              an open payable to supplier counterparty_id: an opening purchase
//	                                               statement, likewise;
//	                          customer_prepayment  money a customer paid before the cutoff and not applied at it: an
//	                                               opening fund record, a receipt of amount on the company's
//	                                               bank_account with the bank's bank_reference and value_date;
//	                          supplier_prepayment  money paid to a supplier before the cutoff and not applied at it:
//	                                               an opening fund record, a payment, likewise.
//	                        amount is what is open (unapplied) at the cutoff; source.amount − amount was settled
//	                        before it. A source document of the kind recorded before — at this cutoff or another,
//	                        and not reversed since — is OPENING_BALANCE_DUPLICATE: an item is recorded once, and
//	                        after go-live no second receivable, payable or cash arises for it (STL-INV-012). A bank
//	                        reference recorded on the account before is BUSINESS_KEY_CONFLICT, so the same bank
//	                        movement is never recorded again by fund.record either. Applications to opening
//	                        statements and from opening fund records work as for any other; an opening receivable
//	                        counts in its customer's credit exposure as an open receivable and its installments'
//	                        due instants in the overdue tolerance (STL-INV-004). settlement.reverse of an opening
//	                        statement reverses its item (its source document may then be recorded again).
//
// Request members: opening_balance.record {opening_id, kind, cutoff_at, source {document, issued_at, amount},
// counterparty_id, order_reference, currency {kind, id, revision}, amount, installments [{amount, due_at}],
// bank_account {kind, id, revision}, bank_reference, value_date, evidence_ids}.
const (
	CommandRecordOpening = "opening_balance.record"

	// Reads: an opening item now, and the opening items with their totals per cutoff, kind and currency.
	CommandGetOpening = "opening_balance.get"
	CommandOpenings   = "opening_balance.list"

	EventOpeningBalanceRecorded = "idp-brant.stl.opening_balance_recorded.v1"

	// Kinds and states of an opening item.
	OpeningReceivable         = "receivable"
	OpeningPayable            = "payable"
	OpeningCustomerPrepayment = "customer_prepayment"
	OpeningSupplierPrepayment = "supplier_prepayment"
	OpeningRecorded           = "recorded"
	OpeningReversed           = "reversed"

	// MaxOpeningInstallments bounds the installments of an opening receivable or payable.
	MaxOpeningInstallments = 24
)

// CodeOpeningBalanceDuplicate (409): the source document is recorded as an opening item of the kind (STL-INV-012).
const CodeOpeningBalanceDuplicate = "OPENING_BALANCE_DUPLICATE"

var sourceDocument = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$`)

// OpeningSource is the source document of an opening item: its reference in the books it comes from, when it was
// issued and its whole amount, in the item's currency.
type OpeningSource struct {
	Document string
	IssuedAt string
	Amount   string
}

// OpeningInstallment is part of an opening receivable or payable due at DueAt.
type OpeningInstallment struct {
	Amount string
	DueAt  string
}

// RecordOpening records opening item OpeningID of OpeningKind ("kind") at CutoffAt (opening_balance.record). Human
// only. See the package comment of this file for the members each kind takes.
type RecordOpening struct {
	IdempotencyKey string
	OpeningID      string
	OpeningKind    string `json:"kind"`
	CutoffAt       string
	Source         OpeningSource
	CounterpartyID string
	OrderReference string
	Currency       ConfigRef
	Amount         string
	Installments   []OpeningInstallment
	BankAccount    *ConfigRef
	BankReference  string
	ValueDate      string
	EvidenceIDs    []string
}

func (RecordOpening) stlCommand() {}

// Prepayment reports whether kind is an unapplied prepayment (an opening fund record).
func Prepayment(kind string) bool {
	return kind == OpeningCustomerPrepayment || kind == OpeningSupplierPrepayment
}

// Validate checks the closed contract.
func (c RecordOpening) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OpeningID):
		return schema("/args/opening_id")
	case c.OpeningKind != OpeningReceivable && c.OpeningKind != OpeningPayable && !Prepayment(c.OpeningKind):
		return schema("/args/kind")
	}
	if _, err := kernel.ParseTimestamp(c.CutoffAt); err != nil {
		return schema("/args/cutoff_at")
	}
	switch {
	case !sourceDocument.MatchString(c.Source.Document):
		return schema("/args/source/document")
	case !timestampOK(c.Source.IssuedAt):
		return schema("/args/source/issued_at")
	case !amount(c.Source.Amount):
		return schema("/args/source/amount")
	case !kernel.ValidIdentifier(c.CounterpartyID):
		return schema("/args/counterparty_id")
	case c.OrderReference != "" && !sourceDocument.MatchString(c.OrderReference):
		return schema("/args/order_reference")
	}
	if err := c.Currency.Validate(KindCurrency, "/args/currency"); err != nil {
		return err
	}
	if !amount(c.Amount) {
		return schema("/args/amount")
	}
	if Prepayment(c.OpeningKind) {
		switch {
		case len(c.Installments) > 0:
			return schema("/args/installments")
		case c.BankAccount == nil:
			return schema("/args/bank_account")
		case !bankReference.MatchString(c.BankReference):
			return schema("/args/bank_reference")
		case !timestampOK(c.ValueDate):
			return schema("/args/value_date")
		}
		if err := c.BankAccount.Validate(KindBankAccount, "/args/bank_account"); err != nil {
			return err
		}
	} else {
		switch {
		case c.BankAccount != nil:
			return schema("/args/bank_account")
		case c.BankReference != "":
			return schema("/args/bank_reference")
		case c.ValueDate != "":
			return schema("/args/value_date")
		case len(c.Installments) < 1 || len(c.Installments) > MaxOpeningInstallments:
			return schema("/args/installments")
		}
		for i, x := range c.Installments {
			p := "/args/installments/" + strconv.Itoa(i)
			if !amount(x.Amount) {
				return schema(p + "/amount")
			}
			if !timestampOK(x.DueAt) {
				return schema(p + "/due_at")
			}
		}
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 1)
}

func timestampOK(s string) bool {
	_, err := kernel.ParseTimestamp(s)
	return err == nil
}

// OpeningReceipt answers opening_balance.record: the receipt (ObjectID the item, its status recorded) and the item
// as stored: its kind, cutoff, source document, party, currency, the amount open at the cutoff and the opening
// statement or fund record it is.
type OpeningReceipt struct {
	Receipt
	OpeningKind    string `json:"kind"`
	CutoffAt       time.Time
	SourceDocument string
	CounterpartyID string
	Currency       string
	AmountMinor    string
	StatementID    string
	FundRecordID   string
}

// OpeningView is an opening item now (opening_balance.get): what it states at its cutoff — the source document's
// amount, what was settled before the cutoff (source − opening amount), the amount open (unapplied) at the cutoff —
// and what became of it since: what stands applied on (from) it and what is open (available) now. Balance is the
// opening statement's receivable or payable (receivable, payable); FundRecord the opening fund record
// (prepayments). Status is reversed once its statement was reversed (nothing is open then).
type OpeningView struct {
	OpeningID          string
	OpeningKind        string `json:"kind"`
	Status             string
	CutoffAt           time.Time
	SourceDocument     string
	SourceIssuedAt     time.Time
	CounterpartyID     string
	OrderReference     string
	Currency           string
	CurrencyRevision   int64
	SourceAmountMinor  string
	SettledBeforeMinor string
	AmountMinor        string
	AppliedSinceMinor  string
	OpenMinor          string
	StatementID        string
	FundRecordID       string
	Balance            *BalanceView
	FundRecord         *FundRecordRef
	EvidenceIDs        []string
	RecordedBy         kernel.ActorRef
	RecordedAt         time.Time
	CheckedAt          time.Time
}

// OpeningTotalView adds up the standing opening items of one cutoff, kind and currency (CN-AT16 截止、来源余额及已办状态
// 可核对): how many, the source documents' amounts, what was settled before the cutoff, what was open at it, what
// was applied since and what is open now; minor units.
type OpeningTotalView struct {
	CutoffAt           time.Time
	OpeningKind        string `json:"kind"`
	Currency           string
	Items              int
	SourceAmountMinor  string
	SettledBeforeMinor string
	AmountMinor        string
	AppliedSinceMinor  string
	OpenMinor          string
}

// OpeningsView is opening_balance.list: the opening items the caller sees (of one cutoff when asked), oldest cutoff
// first, and the totals of the standing ones.
type OpeningsView struct {
	Items     []OpeningView
	Totals    []OpeningTotalView
	CheckedAt time.Time
}

// OpeningCommands is STL's command entry point for opening balances; errors as for Commands.
type OpeningCommands interface {
	RecordOpening(ctx context.Context, cmd RecordOpening) (OpeningReceipt, error)
}

// OpeningQueries is the audited read side of opening balances (stl.read); an item outside the caller's data scope
// is NOT_FOUND (and left out of the list).
type OpeningQueries interface {
	GetOpening(ctx context.Context, id string) (OpeningView, error)
	// Openings lists the opening items; cutoffAt "" lists every cutoff, else one (a contract timestamp:
	// SCHEMA_INVALID otherwise).
	Openings(ctx context.Context, cutoffAt string) (OpeningsView, error)
}
