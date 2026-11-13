// Package fundrecord holds STL's FundRecord rules (tactical model STL-AGG-FundRecord, SM-STL-FUND,
// STL-INV-006; UL STL-FundRecord, STL-FxConversion; baseline CN-R10, CN-AT07/08/09/15; S5 plan D-16).
//
// A fund record is a verified movement of money on one of the company's bank accounts, recorded by
// a person with bank evidence. Its kind says what the money is: a receipt (a customer's payment,
// the only kind applications allocate), a non-goods receipt (never applied: it waits for
// verification, CN-R10), a payment, a refund or a returned remittance (both drawn on an earlier
// fund record), or an FX conversion (a transfer of an earlier receipt's currency into another,
// never a receipt: CN-AT09). A receipt keeps the payer's remitted amount, the bank's
// original-currency credit and the bank charges the advice states apart; the difference between
// remitted and received is unexplained until someone other than the recorder resolves it, in part
// or whole, as a bank charge or a discount borne by us (appended, never rewritten). Balances are
// derived from the immutable entries (S5 D-3):
//
//	available = received + resolved − applied − refunded or returned   (what applications may use)
//	cash      = received − refunded or returned − sold by conversions   (what is still on the account)
package fundrecord

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Kinds of a fund record.
const (
	KindReceipt            = "receipt"
	KindNonGoods           = "non_goods"
	KindPayment            = "payment"
	KindRefund             = "refund"
	KindReturnedRemittance = "returned_remittance"
	KindFXConversion       = "fx_conversion"
)

// Directions of the money on the company's account.
const (
	In  = "in"
	Out = "out"
)

// States (SM-STL-FUND): recorded, then difference_resolved from the first resolution on.
const (
	StatusRecorded           = "recorded"
	StatusDifferenceResolved = "difference_resolved"
)

// Resolutions of a difference: a bank charge or a discount borne by us (STL-INV-006).
const (
	ResolutionBankCharge = "bank_charge"
	ResolutionDiscount   = "discount"
)

// Owners and purposes of a POL bank account.
const (
	OwnerCompany   = "company"
	PurposeReceipt = "receipt"
	PurposePayment = "payment"
	PurposeBoth    = "both"
)

// Act is who did something, under which authority epoch, when, and why.
type Act struct {
	By     kernel.ActorRef
	Epoch  int64
	At     time.Time
	Reason string
}

// FX is a conversion's sale: Sold in the source's currency at the bank's Rate (target units per
// source unit, as the bank states it), and the bank's Fee in the target currency.
type FX struct {
	Sold kernel.Money
	Rate kernel.Decimal
	Fee  kernel.Money
}

// Resolution is one resolved part of a receipt's difference.
type Resolution struct {
	Seq         int
	Resolution  string
	Amount      kernel.Money
	EvidenceIDs []string
	By          Act
}

// FundRecord is the root of STL-AGG-FundRecord.
type FundRecord struct {
	ID                  string
	Kind                string
	Direction           string
	Status              string
	Version             int64
	CounterpartyID      string // "" for a conversion
	CurrencyRevision    int64
	BankAccountID       string
	BankAccountRevision int64
	Received            kernel.Money
	Remitted            kernel.Money
	BankCharges         kernel.Money
	ValueDate           time.Time
	BankReference       string
	EvidenceIDs         []string
	SourceID            string
	FX                  *FX
	OwnerActor          string
	OwnerTeam           string
	Recorded            Act
	Resolutions         []Resolution
}

// Entries are the immutable entries drawn on a fund record, summed in its currency: Applied by
// applications that stand, Drawn by refunds and returned remittances, Sold by conversions.
type Entries struct {
	Applied kernel.Money
	Drawn   kernel.Money
	Sold    kernel.Money
}

// NoEntries is a fund record with nothing drawn on it, in c.
func NoEntries(c kernel.Currency) Entries {
	z, _ := kernel.NewMoney(c, 0)
	return Entries{Applied: z, Drawn: z, Sold: z}
}

// Currency is the record's currency.
func (f *FundRecord) Currency() kernel.Currency { return f.Received.Currency() }

func must(m kernel.Money, err error) kernel.Money {
	if err != nil {
		panic("stl: fund record amounts in one currency: " + err.Error())
	}
	return m
}

// Difference is remitted − received.
func (f *FundRecord) Difference() kernel.Money { return must(f.Remitted.Sub(f.Received)) }

// Resolved is the part of the difference resolved so far.
func (f *FundRecord) Resolved() kernel.Money {
	sum := must(kernel.NewMoney(f.Currency(), 0))
	for _, r := range f.Resolutions {
		sum = must(sum.Add(r.Amount))
	}
	return sum
}

// Unexplained is the part of the difference not resolved: it never counts for applications or
// payment conditions (STL-INV-006).
func (f *FundRecord) Unexplained() kernel.Money { return must(f.Difference().Sub(f.Resolved())) }

// Available is what applications may still allocate of the record given e.
func (f *FundRecord) Available(e Entries) kernel.Money {
	return must(must(must(f.Received.Add(f.Resolved())).Sub(e.Applied)).Sub(e.Drawn))
}

// Cash is what is still on the account of the record given e.
func (f *FundRecord) Cash(e Entries) kernel.Money {
	return must(must(f.Received.Sub(e.Drawn)).Sub(e.Sold))
}

// Applicable reports whether applications may allocate the record: receipts only.
func (f *FundRecord) Applicable() bool { return f.Kind == KindReceipt }

// BankAccount is the POL bank_account revision the money moved on.
type BankAccount struct {
	ID        string
	Revision  int64
	OwnerKind string
	OwnerID   string
	Currency  string
	Purpose   string
}

// Source is the fund record a refund, a returned remittance or a conversion draws on, with its
// entries, read under its lock.
type Source struct {
	Record  *FundRecord
	Entries Entries
}

// Input is a fund.record request as the rules see it, its amounts parsed in Currency.
type Input struct {
	ID               string
	Kind             string
	CounterpartyID   string
	Currency         kernel.Currency
	CurrencyRevision int64
	Account          BankAccount
	Company          string // the caller's company (identity.Principal)
	Received         kernel.Money
	Remitted         *kernel.Money
	BankCharges      *kernel.Money
	ValueDate        time.Time
	BankReference    string
	EvidenceIDs      []string
	Source           *Source
	FX               *FX
	By               Act
	Team             string
}

func direction(kind string) string {
	switch kind {
	case KindPayment, KindRefund:
		return Out
	}
	return In
}

// Record applies fund.record's rules and returns the new record (STL-INV-006, CN-R10, CN-AT09,
// CN-AT15 payee half).
func Record(in Input) (*FundRecord, error) {
	f := &FundRecord{ID: in.ID, Kind: in.Kind, Direction: direction(in.Kind), Status: StatusRecorded, Version: 1,
		CounterpartyID: in.CounterpartyID, CurrencyRevision: in.CurrencyRevision, BankAccountID: in.Account.ID,
		BankAccountRevision: in.Account.Revision, Received: in.Received, Remitted: in.Received, BankCharges: must(kernel.NewMoney(in.Currency, 0)),
		ValueDate: in.ValueDate, BankReference: in.BankReference, EvidenceIDs: in.EvidenceIDs, OwnerActor: in.By.By.ID, OwnerTeam: in.Team,
		Recorded: in.By}
	drawn := in.Kind == KindRefund || in.Kind == KindReturnedRemittance || in.Kind == KindFXConversion
	switch {
	case drawn != (in.Source != nil):
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/args/source_fund_record_id", "")
	case in.Received.Sign() <= 0:
		return nil, invalid("/args/received_amount", "The received amount is above zero")
	}
	if src := in.Source; src != nil {
		f.SourceID = src.Record.ID
		if err := f.drawOn(in, src); err != nil {
			return nil, err
		}
	}
	if err := f.account(in); err != nil {
		return nil, err
	}
	if in.Kind == KindReceipt {
		if in.Remitted != nil {
			f.Remitted = *in.Remitted
		}
		if c, _ := f.Remitted.Cmp(f.Received); c < 0 {
			return nil, invalid("/args/remitted_amount", "The payer cannot have remitted less than the bank credited")
		}
		if in.BankCharges != nil {
			f.BankCharges = *in.BankCharges
		}
		if c, _ := f.BankCharges.Cmp(f.Difference()); c > 0 {
			return nil, invalid("/args/bank_charges", "The stated bank charges exceed the difference between remitted and received")
		}
	}
	return f, nil
}

// account checks the bank account: the company's own (CN-AT15: money on another entity's account is
// a mismatch, not a receipt of this company), in the record's currency, for the direction.
func (f *FundRecord) account(in Input) error {
	a := in.Account
	switch {
	case a.OwnerKind != OwnerCompany || a.OwnerID != in.Company:
		return Reject(CodePartyEntityMismatch, "/args/bank_account", "The bank account belongs to "+a.OwnerKind+" "+a.OwnerID+
			", not to this company: review it in the approved mode")
	case a.Currency != in.Currency.Code():
		return reference("/args/bank_account", "The bank account is in "+a.Currency+", the money in "+in.Currency.Code())
	case a.Purpose != PurposeBoth && (f.Direction == In) != (a.Purpose == PurposeReceipt):
		return reference("/args/bank_account", "The bank account is for "+a.Purpose+"s")
	}
	return nil
}

// drawOn checks a refund, returned remittance or conversion against its source and takes the
// source's counterparty.
func (f *FundRecord) drawOn(in Input, src *Source) error {
	s := src.Record
	path := "/args/source_fund_record_id"
	switch in.Kind {
	case KindRefund:
		if s.Kind != KindReceipt && s.Kind != KindNonGoods {
			return state(path, "A refund pays back a receipt; "+s.ID+" is a "+s.Kind)
		}
	case KindReturnedRemittance:
		switch s.Kind {
		case KindReceipt, KindNonGoods:
			f.Direction = Out
		case KindPayment:
			f.Direction = In
		default:
			return state(path, "A returned remittance returns a receipt or a payment; "+s.ID+" is a "+s.Kind)
		}
	case KindFXConversion:
		if s.Kind != KindReceipt && s.Kind != KindNonGoods {
			return state(path, "A conversion sells the currency of a receipt; "+s.ID+" is a "+s.Kind)
		}
		return f.convert(in, src)
	}
	if s.Currency() != in.Currency {
		return reference("/args/currency", "The source is in "+s.Currency().Code())
	}
	f.CounterpartyID = s.CounterpartyID
	if c, _ := in.Received.Cmp(s.Cash(src.Entries)); c > 0 {
		return Reject(CodeApplicationExceedsAvailable, "/args/received_amount", "Only "+s.Cash(src.Entries).String()+" of "+s.ID+
			" is still on the account")
	}
	if s.Direction == In {
		if c, _ := in.Received.Cmp(s.Available(src.Entries)); c > 0 {
			return Reject(CodeApplicationExceedsAvailable, "/args/received_amount", "Only "+s.Available(src.Entries).String()+" of "+s.ID+
				" is not applied: unapply first")
		}
	}
	return nil
}

// convert checks a conversion: it sells the source's currency into another, within the source's
// cash, and the bank's arithmetic holds: received + fee = sold × rate, HALF_UP to the target's
// minor units (UL STL-FxConversion; baseline 4.6 step 4: USD 800 at 6.875 = CNY 5,500).
func (f *FundRecord) convert(in Input, src *Source) error {
	s, x := src.Record, in.FX
	switch {
	case x == nil:
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/fx", "")
	case x.Sold.Currency() != s.Currency():
		return reference("/args/fx/sold_amount", "The source is in "+s.Currency().Code())
	case s.Currency() == in.Currency:
		return Reject(CodeFXScopeMismatch, "/args/currency", "A conversion changes the currency")
	case x.Sold.Sign() <= 0 || x.Rate.Sign() <= 0:
		return invalid("/args/fx", "The sold amount and the rate are above zero")
	case x.Fee.Currency() != in.Currency || x.Fee.Sign() < 0:
		return invalid("/args/fx/fee", "The fee is in the target currency and not negative")
	}
	if c, _ := x.Sold.Cmp(s.Cash(src.Entries)); c > 0 {
		return Reject(CodeApplicationExceedsAvailable, "/args/fx/sold_amount", "Only "+s.Cash(src.Entries).String()+" of "+s.ID+
			" is still on the account")
	}
	gross, err := kernel.RoundMoney(in.Currency, x.Sold.Decimal().Mul(x.Rate))
	if err != nil {
		return invalid("/args/fx/bank_rate", err.Error())
	}
	if !gross.Equal(must(in.Received.Add(x.Fee))) {
		return invalid("/args/fx/bank_rate", x.Sold.String()+" at the rate is "+gross.String()+", not received + fee")
	}
	f.FX = &FX{Sold: x.Sold, Rate: x.Rate, Fee: x.Fee}
	f.CounterpartyID = ""
	return nil
}

// Resolve resolves amount of the receipt's unexplained difference as resolution (bank charge or
// discount borne by us) with its evidence, by someone other than the recorder (SM-STL-FUND;
// independence row fund.resolve_difference independentOf 资金登记人).
func (f *FundRecord) Resolve(resolution string, amount kernel.Money, evidence []string, by Act) error {
	switch {
	case f.Kind != KindReceipt:
		return state("/args/fund_record_id", "Only a receipt has a difference to resolve")
	case by.By == f.Recorded.By:
		return Reject(CodeIndependentReviewRequired, "/args/fund_record_id", "The recorder of the fund record does not resolve its difference")
	case f.Unexplained().Sign() == 0:
		return state("/args/fund_record_id", "The fund record has no unexplained difference")
	case amount.Currency() != f.Currency() || amount.Sign() <= 0:
		return invalid("/args/amount", "The amount is above zero, in "+f.Currency().Code())
	}
	if c, _ := amount.Cmp(f.Unexplained()); c > 0 {
		return invalid("/args/amount", "Only "+f.Unexplained().String()+" is unexplained")
	}
	f.Resolutions = append(f.Resolutions, Resolution{Seq: len(f.Resolutions) + 1, Resolution: resolution, Amount: amount, EvidenceIDs: evidence,
		By: by})
	f.Status, f.Version = StatusDifferenceResolved, f.Version+1
	return nil
}
