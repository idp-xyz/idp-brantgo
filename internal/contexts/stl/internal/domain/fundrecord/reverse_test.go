package fundrecord

import "testing"

// S5R-D22 / STL-INV-014 / SM-STL-FUND: fund.reverse — independent of the recorder (INDEPENDENT_REVIEW_REQUIRED), only
// once nothing stands applied (FUND_RECORD_HAS_APPLICATIONS) and nothing drawn stands (INVALID_STATE), once; a reversed
// record has nothing available or on the account, takes no resolution or verification and is no source of a draw.
func TestS5RFundReverseIsIndependentAndNeedsNoApplications(t *testing.T) {
	f, err := Record(receiptInput(t, "fund.1", "672.00"))
	if err != nil {
		t.Fatal(err)
	}
	want(t, f.Reverse(nil, nil, nil, treasury), CodeIndependentReviewRequired)
	want(t, f.Reverse([]string{"app.1"}, nil, nil, manager), CodeFundRecordHasApplications)
	want(t, f.Reverse(nil, []string{"refund.1"}, nil, manager), "INVALID_STATE")
	if f.Status != StatusRecorded || f.Version != 1 {
		t.Fatalf("a refusal moved the record: %s v%d", f.Status, f.Version)
	}
	if err := f.Reverse(nil, nil, []string{"bank.recall"}, manager); err != nil {
		t.Fatal(err)
	}
	if f.Status != StatusReversed || f.Version != 2 || f.Reversal == nil || f.Reversal.By.Reason != manager.Reason || !f.Reversed() {
		t.Fatalf("reversed %+v", f)
	}
	e := NoEntries(usd)
	if !f.Available(e).IsZero() || !f.Cash(e).IsZero() || f.Applicable() {
		t.Fatalf("a reversed record still counts: available %s cash %s", f.Available(e), f.Cash(e))
	}
	want(t, f.Reverse(nil, nil, nil, manager), "INVALID_STATE")
	want(t, f.Resolve(ResolutionBankCharge, money(t, usd, "1.00"), []string{"x"}, manager), "INVALID_STATE")
	if _, err := f.VerifyPayer("customer.2", []string{"x"}, nil, manager); code(err) != "INVALID_STATE" {
		t.Fatalf("verify %v", err)
	}
	refund := receiptInput(t, "refund.1", "10.00")
	refund.Kind, refund.CounterpartyID, refund.Source = KindRefund, "", &Source{Record: f, Entries: e}
	if _, err := Record(refund); code(err) != "INVALID_STATE" {
		t.Fatalf("a draw on a reversed record: %v", err)
	}
}

// S5R-D22: a payment (and any other kind) is reversed the same way; its version moves from 1 to 2.
func TestS5RAPaymentIsReversedToo(t *testing.T) {
	in := receiptInput(t, "pay.1", "500.00")
	in.Kind, in.Account = KindPayment, account(usd, PurposePayment)
	f, err := Record(in)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Pays() {
		t.Fatal("a payment pays")
	}
	if err := f.Reverse(nil, nil, nil, manager); err != nil || f.Version != 2 || f.Pays() {
		t.Fatalf("reverse %v v%d", err, f.Version)
	}
}
