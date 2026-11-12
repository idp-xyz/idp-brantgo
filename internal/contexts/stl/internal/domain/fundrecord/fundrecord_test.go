package fundrecord

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	usd      = kernel.MustCurrency("USD", 2)
	cny      = kernel.MustCurrency("CNY", 2)
	treasury = Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "treasury.1"}, Epoch: 1, At: time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)}
	manager  = Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "manager.1"}, Epoch: 1, At: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
		Reason: "中间行扣费，经批准由我方承担"}
)

func money(t *testing.T, c kernel.Currency, s string) kernel.Money {
	t.Helper()
	m, err := kernel.ParseAmount(c, s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ptr(m kernel.Money) *kernel.Money { return &m }

func account(c kernel.Currency, purpose string) BankAccount {
	return BankAccount{ID: "bank." + c.Code(), Revision: 1, OwnerKind: OwnerCompany, OwnerID: "company.A", Currency: c.Code(), Purpose: purpose}
}

func receiptInput(t *testing.T, id, received string) Input {
	return Input{ID: id, Kind: KindReceipt, CounterpartyID: "customer.1", Currency: usd, CurrencyRevision: 1, Account: account(usd, PurposeBoth),
		Company: "company.A", Received: money(t, usd, received), ValueDate: treasury.At, BankReference: "ref." + id, EvidenceIDs: []string{"advice"},
		By: treasury}
}

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func want(t *testing.T, err error, c string) {
	t.Helper()
	if code(err) != c {
		t.Fatalf("got %v, want %s", err, c)
	}
}

// STL-INV-006 and SC-08: the customer remitted 672.00, the bank credited 670.00; the 2.00 difference
// is unexplained, so only 670.00 is available; the recorder may not resolve it; a credit manager
// resolves it as our bank charge, and 672.00 is available.
func TestDifferenceStaysUnexplainedUntilAnIndependentResolution(t *testing.T) {
	in := receiptInput(t, "fund.1", "670.00")
	in.Remitted = ptr(money(t, usd, "672.00"))
	in.BankCharges = ptr(money(t, usd, "2.00"))
	f, err := Record(in)
	if err != nil {
		t.Fatal(err)
	}
	e := NoEntries(usd)
	if f.Difference().Amount() != "2.00" || f.Unexplained().Amount() != "2.00" || f.Available(e).Amount() != "670.00" || f.Status != StatusRecorded ||
		f.Direction != In || !f.Applicable() {
		t.Fatalf("recorded %+v: difference %s, available %s", f, f.Difference(), f.Available(e))
	}
	want(t, f.Resolve(ResolutionBankCharge, money(t, usd, "2.00"), []string{"advice"}, treasury), CodeIndependentReviewRequired)
	want(t, f.Resolve(ResolutionBankCharge, money(t, usd, "2.01"), []string{"advice"}, manager), kernel.CodeInvalidKnownValue)
	if err := f.Resolve(ResolutionBankCharge, money(t, usd, "2.00"), []string{"advice"}, manager); err != nil {
		t.Fatal(err)
	}
	if f.Unexplained().Sign() != 0 || f.Available(e).Amount() != "672.00" || f.Status != StatusDifferenceResolved || f.Version != 2 {
		t.Fatalf("resolved %+v", f)
	}
	want(t, f.Resolve(ResolutionDiscount, money(t, usd, "0.01"), []string{"advice"}, manager), kernel.CodeInvalidState)
}

// CN-AT07 and baseline 24.3: one remittance of 1,000.00 for two orders, credited 980.00 with 15.00
// charges stated; 15.00 is resolved as our bank charge with its basis, 5.00 stays unexplained.
func TestCombinedPaymentKeepsTheUnexplainedRemainder(t *testing.T) {
	in := receiptInput(t, "fund.7", "980.00")
	in.Remitted, in.BankCharges = ptr(money(t, usd, "1000.00")), ptr(money(t, usd, "15.00"))
	f, err := Record(in)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Resolve(ResolutionBankCharge, money(t, usd, "15.00"), []string{"advice"}, manager); err != nil {
		t.Fatal(err)
	}
	if f.Unexplained().Amount() != "5.00" || f.Available(NoEntries(usd)).Amount() != "995.00" || f.BankCharges.Amount() != "15.00" {
		t.Fatalf("after the charge: unexplained %s available %s", f.Unexplained(), f.Available(NoEntries(usd)))
	}
	over := receiptInput(t, "fund.8", "980.00")
	over.Remitted, over.BankCharges = ptr(money(t, usd, "1000.00")), ptr(money(t, usd, "20.01"))
	_, err = Record(over)
	want(t, err, kernel.CodeInvalidKnownValue)
	less := receiptInput(t, "fund.9", "980.00")
	less.Remitted = ptr(money(t, usd, "979.99"))
	_, err = Record(less)
	want(t, err, kernel.CodeInvalidKnownValue)
}

// CN-AT15 payee half and R33: money on an account that is not the company's is a mismatch, not this
// company's receipt; the account's currency and purpose must fit.
func TestTheBankAccountIsTheCompanysForTheMoney(t *testing.T) {
	in := receiptInput(t, "fund.2", "100.00")
	in.Account.OwnerKind, in.Account.OwnerID = "party", "agent.1"
	_, err := Record(in)
	want(t, err, CodePartyEntityMismatch)
	in = receiptInput(t, "fund.2", "100.00")
	in.Account.OwnerID = "company.B"
	_, err = Record(in)
	want(t, err, CodePartyEntityMismatch)
	in = receiptInput(t, "fund.2", "100.00")
	in.Account = account(cny, PurposeBoth)
	_, err = Record(in)
	want(t, err, kernel.CodeReferenceInvalid)
	in = receiptInput(t, "fund.2", "100.00")
	in.Account.Purpose = PurposePayment
	_, err = Record(in)
	want(t, err, kernel.CodeReferenceInvalid)
	pay := Input{ID: "pay.1", Kind: KindPayment, CounterpartyID: "supplier.1", Currency: usd, Account: account(usd, PurposeReceipt),
		Company: "company.A", Received: money(t, usd, "10.00"), By: treasury}
	_, err = Record(pay)
	want(t, err, kernel.CodeReferenceInvalid)
	pay.Account.Purpose = PurposePayment
	if f, err := Record(pay); err != nil || f.Direction != Out || f.Applicable() {
		t.Fatalf("payment %+v %v", f, err)
	}
}

// CN-AT08: refunds and returned remittances are fund records of their own drawn on the receipt;
// they take its counterparty and never exceed what is unapplied or still on the account.
func TestRefundsAndReturnsDrawOnTheirSource(t *testing.T) {
	src, err := Record(receiptInput(t, "fund.3", "500.00"))
	if err != nil {
		t.Fatal(err)
	}
	applied := NoEntries(usd)
	applied.Applied = money(t, usd, "400.00")
	refund := Input{ID: "refund.1", Kind: KindRefund, Currency: usd, Account: account(usd, PurposeBoth), Company: "company.A",
		Received: money(t, usd, "100.01"), Source: &Source{Record: src, Entries: applied}, By: treasury}
	_, err = Record(refund)
	want(t, err, CodeApplicationExceedsAvailable)
	refund.Received = money(t, usd, "100.00")
	r, err := Record(refund)
	if err != nil || r.Direction != Out || r.CounterpartyID != "customer.1" || r.Applicable() {
		t.Fatalf("refund %+v %v", r, err)
	}
	back := Input{ID: "return.1", Kind: KindReturnedRemittance, Currency: usd, Account: account(usd, PurposeBoth), Company: "company.A",
		Received: money(t, usd, "500.00"), Source: &Source{Record: src, Entries: NoEntries(usd)}, By: treasury}
	if b, err := Record(back); err != nil || b.Direction != Out {
		t.Fatalf("returned receipt %+v %v", b, err)
	}
	pay, err := Record(Input{ID: "pay.2", Kind: KindPayment, CounterpartyID: "supplier.1", Currency: usd, Account: account(usd, PurposeBoth),
		Company: "company.A", Received: money(t, usd, "50.00"), By: treasury})
	if err != nil {
		t.Fatal(err)
	}
	back.Source, back.Received = &Source{Record: pay, Entries: NoEntries(usd)}, money(t, usd, "50.00")
	if b, err := Record(back); err != nil || b.Direction != In || b.CounterpartyID != "supplier.1" {
		t.Fatalf("returned payment %+v %v", b, err)
	}
	refund.Source = &Source{Record: pay, Entries: NoEntries(usd)}
	_, err = Record(refund)
	want(t, err, kernel.CodeInvalidState)
}

// CN-AT09 and baseline 4.6 step 4: USD 800.00 received, sold at the bank's 6.875 for CNY 5,500.00:
// a conversion, not a receipt; the receipt's available balance stays 800.00 for its applications;
// the arithmetic must hold and the sale stays within the cash.
func TestConversionIsATransferNotAReceipt(t *testing.T) {
	src, err := Record(receiptInput(t, "fund.4", "800.00"))
	if err != nil {
		t.Fatal(err)
	}
	conv := Input{ID: "fx.1", Kind: KindFXConversion, Currency: cny, Account: account(cny, PurposeReceipt), Company: "company.A",
		Received: money(t, cny, "5500.00"), Source: &Source{Record: src, Entries: NoEntries(usd)},
		FX: &FX{Sold: money(t, usd, "800.00"), Rate: kernel.MustDecimal("6.875"), Fee: money(t, cny, "0")}, By: treasury}
	f, err := Record(conv)
	if err != nil {
		t.Fatal(err)
	}
	if f.Applicable() || f.CounterpartyID != "" || f.FX == nil || f.Direction != In {
		t.Fatalf("conversion %+v", f)
	}
	sold := NoEntries(usd)
	sold.Sold = money(t, usd, "800.00")
	if src.Available(sold).Amount() != "800.00" || src.Cash(sold).Sign() != 0 {
		t.Fatalf("source after the sale: available %s cash %s", src.Available(sold), src.Cash(sold))
	}
	conv.Received = money(t, cny, "5499.99")
	_, err = Record(conv)
	want(t, err, kernel.CodeInvalidKnownValue)
	conv.Received, conv.FX.Fee = money(t, cny, "5480.00"), money(t, cny, "20.00")
	if _, err = Record(conv); err != nil {
		t.Fatalf("with a fee: %v", err)
	}
	conv.Source = &Source{Record: src, Entries: sold}
	_, err = Record(conv)
	want(t, err, CodeApplicationExceedsAvailable)
	same := conv
	same.Source, same.Currency, same.Account = &Source{Record: src, Entries: NoEntries(usd)}, usd, account(usd, PurposeBoth)
	same.Received, same.FX = money(t, usd, "800.00"), &FX{Sold: money(t, usd, "800.00"), Rate: kernel.MustDecimal("1"), Fee: money(t, usd, "0")}
	_, err = Record(same)
	want(t, err, CodeFXScopeMismatch)
}
