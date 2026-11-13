package api

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func fundCmd() RecordFund {
	return RecordFund{IdempotencyKey: "k", FundRecordID: "fund.1", FundKind: FundReceipt, CounterpartyID: "customer.1",
		Currency: ConfigRef{Kind: KindCurrency, ConfigID: "USD", Revision: 1}, BankAccount: ConfigRef{Kind: KindBankAccount, ConfigID: "bank", Revision: 1},
		ReceivedAmount: "670.00", RemittedAmount: "672.00", BankCharges: "2.00", ValueDate: "2026-11-12T08:00:00Z", BankReference: "MT103/0001",
		EvidenceIDs: []string{"advice"}}
}

func pathOf(err error) string {
	if r, ok := kernel.AsRejection(err); ok && r.Code == kernel.CodeSchemaInvalid && len(r.Fields) > 0 {
		return r.Fields[0].Path
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// fund.record's closed contract: the kind decides which members it takes (D2 of the B2a note:
// counterparty for receipts, non-goods receipts and payments; the source for refunds, returns and
// conversions; fx only for a conversion; remitted amount and charges only for a receipt).
func TestRecordFundContract(t *testing.T) {
	if err := fundCmd().Validate(); err != nil {
		t.Fatal(err)
	}
	conv := fundCmd()
	conv.FundKind, conv.CounterpartyID, conv.SourceFundRecordID, conv.RemittedAmount, conv.BankCharges = FundFXConversion, "", "fund.0", "", ""
	conv.FX = &FXSale{SoldAmount: "800.00", BankRate: "6.875"}
	if err := conv.Validate(); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(*RecordFund){
		"/args/kind":                  func(c *RecordFund) { c.FundKind = "deposit" },
		"/args/counterparty_id":       func(c *RecordFund) { c.CounterpartyID = "" },
		"/args/source_fund_record_id": func(c *RecordFund) { c.FundKind = FundRefund },
		"/args/fx":                    func(c *RecordFund) { c.FX = &FXSale{SoldAmount: "1", BankRate: "1"} },
		"/args/remitted_amount":       func(c *RecordFund) { c.FundKind = FundPayment },
		"/args/received_amount":       func(c *RecordFund) { c.ReceivedAmount = "-1" },
		"/args/bank_charges":          func(c *RecordFund) { c.BankCharges = "x" },
		"/args/currency/kind":         func(c *RecordFund) { c.Currency.Kind = KindFXRate },
		"/args/bank_account/kind":     func(c *RecordFund) { c.BankAccount.Kind = KindCurrency },
		"/args/value_date":            func(c *RecordFund) { c.ValueDate = "2026-11-12" },
		"/args/bank_reference":        func(c *RecordFund) { c.BankReference = "a b" },
		"/args/evidence_ids":          func(c *RecordFund) { c.EvidenceIDs = nil },
		"/args/evidence_ids/1":        func(c *RecordFund) { c.EvidenceIDs = []string{"a", "a"} },
	} {
		c := fundCmd()
		edit(&c)
		if got := pathOf(c.Validate()); got != want {
			t.Errorf("%s: got %q", want, got)
		}
	}
	bad := conv
	bad.FX = &FXSale{SoldAmount: "800.00", BankRate: "6.875.1"}
	if got := pathOf(bad.Validate()); got != "/args/fx/bank_rate" {
		t.Errorf("rate: %q", got)
	}
}

func TestApplyAndResolveContracts(t *testing.T) {
	a := Apply{IdempotencyKey: "k", ApplicationID: "app.1", FundRecordID: "fund.1", Target: ApplicationTarget{Kind: TargetOrderStage, OrderID: "o", StageID: "P1"},
		Amount: "1.00"}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	a.Target.Kind = "invoice"
	if got := pathOf(a.Validate()); got != "/args/target/kind" {
		t.Errorf("target kind: %q", got)
	}
	r := ResolveFundDifference{IdempotencyKey: "k", FundRecordID: "fund.1", ExpectedVersion: 1, Resolution: ResolutionDiscount, Amount: "2.00",
		EvidenceIDs: []string{"advice"}, Reason: "折让"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	r.Resolution = "write_off"
	if got := pathOf(r.Validate()); got != "/args/resolution" {
		t.Errorf("resolution: %q", got)
	}
	u := Unapply{IdempotencyKey: "k", ApplicationID: "app.1", ExpectedVersion: 1}
	if got := pathOf(u.Validate()); got != "/args/reason" {
		t.Errorf("reason: %q", got)
	}
}

// PaymentConditionGuard v1's row as FUL reads it: satisfied passes; a blocked stage, a missing
// account and another revision answer COMMERCIAL_RELEASE_BLOCKED (409) saying why.
func TestPaymentVerdicts(t *testing.T) {
	q := PaymentQuery{OrderID: "order.1", OrderRevision: 2, Scope: ScopeShipment, At: time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)}
	str := func(s string) *string { return &s }
	row := func(status, reason string, rev *int64, stage, code *string) func(dest ...any) error {
		return func(dest ...any) error {
			*dest[0].(*string), *dest[1].(*string) = status, reason
			*dest[2].(**int64) = rev
			*dest[3].(**string), *dest[4].(**string) = str("h"), str("USD")
			*dest[5].(**string), *dest[6].(**string) = str("96000"), str("28800")
			*dest[7].(**string) = stage
			if stage != nil {
				*dest[8].(**string), *dest[9].(**string), *dest[10].(**string), *dest[11].(**string) = str("before_shipment"), str("67200"), str("0"), str("67200")
			}
			at := q.At
			*dest[12].(**time.Time) = &at
			*dest[13].(**string) = code
			return nil
		}
	}
	two, one := int64(2), int64(1)
	ok, err := ScanPaymentCondition(q, row(PaymentSatisfied, "", &two, nil, nil))
	if err != nil || !ok.Satisfied() || ok.Check("/x") != nil || ok.RequiredMinor != "96000" || !ok.CheckedAt.Equal(q.At) {
		t.Fatalf("satisfied %+v %v", ok, err)
	}
	blocked := str(CodeCommercialReleaseBlocked)
	for _, c := range []struct {
		reason string
		rev    *int64
		stage  *string
	}{{PaymentStageUnpaid, &two, str("P2")}, {PaymentNoAccount, nil, nil}, {PaymentRevisionMismatch, &one, nil}} {
		v, err := ScanPaymentCondition(q, row(PaymentBlocked, c.reason, c.rev, c.stage, blocked))
		if err != nil || v.Satisfied() {
			t.Fatalf("%s %+v %v", c.reason, v, err)
		}
		rej, isRej := kernel.AsRejection(v.Check("/args/shipment_id"))
		if !isRej || rej.Code != CodeCommercialReleaseBlocked || rej.Category != kernel.CategoryConflict {
			t.Fatalf("%s: %v", c.reason, v.Check("/x"))
		}
	}
	if _, err := ScanPaymentCondition(q, func(...any) error { return errors.New("scan") }); err == nil {
		t.Fatal("scan error")
	}
}
