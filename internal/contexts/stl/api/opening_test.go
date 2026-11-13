package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The closed contract of opening_balance.record (S5 unit C10; STL-INV-012): each kind takes its members and omits
// the others' — a receivable or payable its installments, a prepayment the bank account, reference and value date.
func TestOpeningContract(t *testing.T) {
	usd := ConfigRef{Kind: KindCurrency, ConfigID: "USD", Revision: 1}
	bank := &ConfigRef{Kind: KindBankAccount, ConfigID: "bank.usd", Revision: 1}
	ar := RecordOpening{IdempotencyKey: "k", OpeningID: "ob.ar.1", OpeningKind: OpeningReceivable, CutoffAt: "2026-10-31T16:00:00Z",
		Source: OpeningSource{Document: "AR-2026/0107", IssuedAt: "2026-09-15T00:00:00Z", Amount: "10000.00"}, CounterpartyID: "customer.1",
		OrderReference: "SO-LEGACY-42", Currency: usd, Amount: "7000.00",
		Installments: []OpeningInstallment{{Amount: "7000.00", DueAt: "2026-11-15T00:00:00Z"}}, EvidenceIDs: []string{"ar.ledger"}}
	if err := ar.Validate(); err != nil {
		t.Fatal(err)
	}
	pre := RecordOpening{IdempotencyKey: "k", OpeningID: "ob.pre.1", OpeningKind: OpeningCustomerPrepayment, CutoffAt: ar.CutoffAt,
		Source: OpeningSource{Document: "RCV-0311", IssuedAt: "2026-10-10T00:00:00Z", Amount: "5000.00"}, CounterpartyID: "customer.1",
		Currency: usd, Amount: "2000.00", BankAccount: bank, BankReference: "BOC/2026/1010-7", ValueDate: "2026-10-10T00:00:00Z",
		EvidenceIDs: []string{"bank.advice"}}
	if err := pre.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		base   RecordOpening
		mutate func(*RecordOpening)
		path   string
	}{
		"kind":                  {ar, func(c *RecordOpening) { c.OpeningKind = "inventory" }, "/args/kind"},
		"id":                    {ar, func(c *RecordOpening) { c.OpeningID = "" }, "/args/opening_id"},
		"cutoff":                {ar, func(c *RecordOpening) { c.CutoffAt = "2026-10-31" }, "/args/cutoff_at"},
		"document":              {ar, func(c *RecordOpening) { c.Source.Document = "AR 0107" }, "/args/source/document"},
		"issued":                {ar, func(c *RecordOpening) { c.Source.IssuedAt = "" }, "/args/source/issued_at"},
		"source amount":         {ar, func(c *RecordOpening) { c.Source.Amount = "-1" }, "/args/source/amount"},
		"party":                 {ar, func(c *RecordOpening) { c.CounterpartyID = "" }, "/args/counterparty_id"},
		"order reference":       {ar, func(c *RecordOpening) { c.OrderReference = "SO 42" }, "/args/order_reference"},
		"currency":              {ar, func(c *RecordOpening) { c.Currency.Kind = "fx_rate" }, "/args/currency/kind"},
		"amount":                {ar, func(c *RecordOpening) { c.Amount = "7,000" }, "/args/amount"},
		"no installments":       {ar, func(c *RecordOpening) { c.Installments = nil }, "/args/installments"},
		"installment due":       {ar, func(c *RecordOpening) { c.Installments[0].DueAt = "soon" }, "/args/installments/0/due_at"},
		"receivable bank":       {ar, func(c *RecordOpening) { c.BankAccount = bank }, "/args/bank_account"},
		"receivable value date": {ar, func(c *RecordOpening) { c.ValueDate = pre.ValueDate }, "/args/value_date"},
		"no evidence":           {ar, func(c *RecordOpening) { c.EvidenceIDs = nil }, "/args/evidence_ids"},
		"prepayment schedule":   {pre, func(c *RecordOpening) { c.Installments = ar.Installments }, "/args/installments"},
		"prepayment no bank":    {pre, func(c *RecordOpening) { c.BankAccount = nil }, "/args/bank_account"},
		"prepayment reference":  {pre, func(c *RecordOpening) { c.BankReference = "" }, "/args/bank_reference"},
		"prepayment value date": {pre, func(c *RecordOpening) { c.ValueDate = "" }, "/args/value_date"},
		"prepayment bank kind":  {pre, func(c *RecordOpening) { c.BankAccount = &ConfigRef{Kind: KindCurrency, ConfigID: "x", Revision: 1} }, "/args/bank_account/kind"},
	} {
		x := c.base
		x.Installments = append([]OpeningInstallment(nil), c.base.Installments...)
		c.mutate(&x)
		if got := pathOf(x.Validate()); got != c.path {
			t.Errorf("%s: %q, want %q", name, got, c.path)
		}
	}
	if c, ok := Category(CodeOpeningBalanceDuplicate); !ok || c != kernel.CategoryConflict {
		t.Fatalf("OPENING_BALANCE_DUPLICATE: %v %v", c, ok)
	}
}
