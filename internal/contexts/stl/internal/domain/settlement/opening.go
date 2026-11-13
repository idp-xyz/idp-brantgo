package settlement

import (
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Opening balances (S5 unit C10, WBS S5-20; tactical model STL-AGG-SettlementStatement entity OpeningBalance,
// value object Cutoff; STL-INV-012; Context Map MD-50; baseline 11.2 期初、防重、切换; BT17, CN-AT16 STL half).
//
// An opening item states what the books held at a cutoff instant, from a source document: an open receivable or
// payable of a party — an opening statement, a sales or purchase statement marked opening without lines (no
// shipment or delivery source: STL-INV-001's sources stay free) and without a Brant order — or an unapplied
// prepayment, an opening fund record (a receipt from a customer or a payment to a supplier; fundrecord.Record's
// rules). The item keeps the source document's whole amount and the amount open at the cutoff: the difference
// was settled before it. A source document stands once per kind whatever the cutoff (OPENING_BALANCE_DUPLICATE):
// recorded twice, or at a later cutoff again, it would form a second receivable, payable or cash (BT17).

// Kinds and states of an opening item.
const (
	OpeningReceivable         = "receivable"
	OpeningPayable            = "payable"
	OpeningCustomerPrepayment = "customer_prepayment"
	OpeningSupplierPrepayment = "supplier_prepayment"
	OpeningRecorded           = "recorded"
	OpeningReversed           = "reversed"
)

// CodeOpeningBalanceDuplicate: the source document is recorded as an opening item of the kind (STL-INV-012).
const CodeOpeningBalanceDuplicate = "OPENING_BALANCE_DUPLICATE"

// Opening is an OpeningBalance entry: of an opening statement (StatementID) or an opening fund record
// (FundRecordID), both its ID.
type Opening struct {
	ID               string
	Kind             string
	Status           string
	CutoffAt         time.Time
	SourceDocument   string
	SourceIssuedAt   time.Time
	SourceAmount     kernel.Money
	Amount           kernel.Money
	CurrencyRevision int64
	CounterpartyID   string
	OrderReference   string
	StatementID      string
	FundRecordID     string
	EvidenceIDs      []string
	OwnerActor       string
	OwnerTeam        string
	Recorded         fundrecord.Act
	UpdatedAt        time.Time
}

// Prepayment reports whether the item is an unapplied prepayment (an opening fund record).
func (o *Opening) Prepayment() bool {
	return o.Kind == OpeningCustomerPrepayment || o.Kind == OpeningSupplierPrepayment
}

// SettledBefore is what of the source document was settled before the cutoff.
func (o *Opening) SettledBefore() kernel.Money { return must(o.SourceAmount.Sub(o.Amount)) }

// StatementKind is the kind of statement an opening receivable or payable is.
func StatementKind(kind string) string {
	if kind == OpeningPayable {
		return KindPurchase
	}
	return KindSales
}

// OpeningInput is an opening_balance.record as the rules see it: the item stated, and Standing, the opening item
// of the kind and source document that stands already (nil: none).
type OpeningInput struct {
	ID               string
	Kind             string
	CutoffAt         time.Time
	SourceDocument   string
	SourceIssuedAt   time.Time
	SourceAmount     kernel.Money
	Amount           kernel.Money
	CurrencyRevision int64
	CounterpartyID   string
	OrderReference   string
	Installments     []Installment // a receivable or payable: each one's amount and due instant
	ValueDate        time.Time     // a prepayment: the bank's value date
	EvidenceIDs      []string
	Standing         *Opening
	By               fundrecord.Act
	Team             string
}

// Duplicate refuses a source document that stands as an opening item of the kind (STL-INV-012).
func Duplicate(standing *Opening) error {
	if standing == nil {
		return nil
	}
	return Reject(CodeOpeningBalanceDuplicate, "/args/source/document", "Source document "+standing.SourceDocument+" is opening item "+
		standing.ID+" at cutoff "+standing.CutoffAt.Format(time.RFC3339))
}

// RecordOpening applies opening_balance.record's rules (STL-INV-012): the source document stands as no opening item
// of the kind (OPENING_BALANCE_DUPLICATE); the cutoff is not after now, the source document not issued after the
// cutoff, the amount open at the cutoff above zero and within the source document's amount, and a prepayment's
// value date not after the cutoff (INVALID_KNOWN_VALUE); a receivable's or payable's installments add up to the
// amount (INVALID_KNOWN_VALUE). It returns the item and, for a receivable or payable, its opening statement: one
// installment per installment stated, no line, no order, net the amount and no tax (an open balance is not split).
// A prepayment's fund record is fundrecord.Record's.
func RecordOpening(in OpeningInput) (*Opening, *Statement, error) {
	if err := Duplicate(in.Standing); err != nil {
		return nil, nil, err
	}
	c := in.Amount.Currency()
	switch {
	case in.CutoffAt.After(in.By.At):
		return nil, nil, invalid("/args/cutoff_at", "The cutoff is after now")
	case in.SourceIssuedAt.After(in.CutoffAt):
		return nil, nil, invalid("/args/source/issued_at", "The source document is issued after the cutoff")
	case in.Amount.Sign() <= 0:
		return nil, nil, invalid("/args/amount", "The amount open at the cutoff is above zero")
	case in.SourceAmount.Currency() != c:
		return nil, nil, invalid("/args/source/amount", "The source document's amount is in "+in.SourceAmount.Currency().Code())
	}
	if cmp, _ := in.SourceAmount.Cmp(in.Amount); cmp < 0 {
		return nil, nil, invalid("/args/source/amount", "The source document amounts to less than is open at the cutoff")
	}
	o := &Opening{ID: in.ID, Kind: in.Kind, Status: OpeningRecorded, CutoffAt: kernel.Instant(in.CutoffAt), SourceDocument: in.SourceDocument,
		SourceIssuedAt: kernel.Instant(in.SourceIssuedAt), SourceAmount: in.SourceAmount, Amount: in.Amount, CurrencyRevision: in.CurrencyRevision,
		CounterpartyID: in.CounterpartyID, OrderReference: in.OrderReference, EvidenceIDs: in.EvidenceIDs, OwnerActor: in.By.By.ID,
		OwnerTeam: in.Team, Recorded: in.By, UpdatedAt: in.By.At}
	if o.Prepayment() {
		if in.ValueDate.After(in.CutoffAt) {
			return nil, nil, invalid("/args/value_date", "An unapplied prepayment at the cutoff was received or paid before it")
		}
		o.FundRecordID = in.ID
		return o, nil, nil
	}
	st := &Statement{ID: in.ID, Kind: StatementKind(in.Kind), Status: StatusConfirmed, Version: 1, Opening: true, CounterpartyID: in.CounterpartyID,
		Net: in.Amount, Tax: must(kernel.NewMoney(c, 0)), Amount: in.Amount, EvidenceIDs: in.EvidenceIDs, OwnerActor: in.By.By.ID,
		OwnerTeam: in.Team, Confirmed: in.By}
	sum := must(kernel.NewMoney(c, 0))
	for i, x := range in.Installments {
		p := "/args/installments/" + strconv.Itoa(i) + "/amount"
		switch {
		case x.Amount.Currency() != c:
			return nil, nil, invalid(p, "Installments are in the item's currency")
		case x.Amount.Sign() <= 0:
			return nil, nil, invalid(p, "An installment is above zero")
		}
		sum = must(sum.Add(x.Amount))
		st.Installments = append(st.Installments, Installment{Seq: i + 1, Amount: x.Amount, DueAt: kernel.Instant(x.DueAt)})
	}
	if !sum.Equal(in.Amount) {
		return nil, nil, invalid("/args/installments", "The installments add up to "+sum.String()+", not to the amount "+in.Amount.String())
	}
	o.StatementID = in.ID
	return o, st, nil
}

// ReverseOpening is what settlement.reverse of an opening statement does to its item: reversed, so its source
// document may be recorded again.
func (o *Opening) ReverseOpening(at time.Time) error {
	if o.Status != OpeningRecorded || o.StatementID == "" {
		return state("/args/statement_id", "The opening item is "+o.Status)
	}
	o.Status, o.UpdatedAt = OpeningReversed, kernel.Instant(at)
	return nil
}
