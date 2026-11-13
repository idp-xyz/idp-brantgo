package api

import (
	"testing"
	"time"
)

// S5R contracts of R10 (simulated sign-off E0): not_received (D13), the stages converted_to_exemption (D14) and
// evidence_rolled_back (D20), tax_refund_case.rebind_policy (D23), the preferential certificate of origin (D12) and
// PurchaseInvoiceRef v1's query (D20, R55). New members are omitempty: a request hash stored before keeps its value.
func TestS5RContracts(t *testing.T) {
	notReceived := RecordReceipt{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 3, ReceiptID: "r", Outcome: DeclarationNotReceived,
		Reason: "海关总署报关数据查询无入库记录（合成）"}
	for _, x := range []struct {
		cmd  interface{ Validate() error }
		want string
	}{
		{notReceived, "OK"}, // its evidence is the domain's: INVALID_KNOWN_VALUE without (CNX-INV-003)
		{func() RecordReceipt { c := notReceived; c.EvidenceIDs = []string{"e1"}; return c }(), "OK"},
		{func() RecordReceipt { c := notReceived; c.Reason = ""; return c }(), "SCHEMA_INVALID /args/reason"},
		{func() RecordReceipt {
			c := notReceived
			c.External = &ExternalReceipt{Sequence: 1, StatusCode: "x", OriginalText: "x", ReceivedAt: "2026-11-12T08:00:00Z"}
			return c
		}(), "SCHEMA_INVALID /args/external"},
		{func() RecordReceipt { c := notReceived; c.DeclarationNumber = "SYN-1"; return c }(), "SCHEMA_INVALID /args/declaration_number"},
		{RecordTaxResult{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 2, Stage: TaxConvertedToExemption, EvidenceIDs: []string{"e1"},
			Reason: "逾期未申报（合成）"}, "OK"},
		{RecordTaxResult{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 2, Stage: TaxConvertedToExemption, EvidenceIDs: []string{"e1"}},
			"SCHEMA_INVALID /args/reason"},
		{RecordTaxResult{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 2, Stage: TaxConvertedToExemption, Reason: "x"},
			"SCHEMA_INVALID /args/evidence_ids"},
		{RecordTaxResult{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 2, Stage: TaxEvidenceRolledBack, PurchaseInvoiceID: "pin.1",
			ResultText: "进货凭证信息回退已确认（合成）", EvidenceIDs: []string{"e1"}}, "OK"},
		{RecordTaxResult{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 2, Stage: TaxEvidenceRolledBack, ResultText: "x", EvidenceIDs: []string{"e1"}},
			"SCHEMA_INVALID /args/purchase_invoice_id"},
		{RecordTaxResult{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 2, Stage: TaxEvidenceRolledBack, PurchaseInvoiceID: "pin.1",
			EvidenceIDs: []string{"e1"}}, "SCHEMA_INVALID /args/result_text"},
		{RecordTaxResult{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 2, Stage: TaxRejected, PurchaseInvoiceID: "pin.1", ResultText: "x",
			EvidenceIDs: []string{"e1"}}, "SCHEMA_INVALID /args/purchase_invoice_id"},
		{RebindPolicy{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 2, Revision: 3, Reason: "出口后发布的修订覆盖出口日期"}, "OK"},
		{RebindPolicy{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 2, Revision: 0, Reason: "x"}, "SCHEMA_INVALID /args/revision"},
		{RebindPolicy{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 2, Revision: 3}, "SCHEMA_INVALID /args/reason"},
		{RebindPolicy{IdempotencyKey: "k", CaseID: "trc.1", Revision: 3, Reason: "x"}, "SCHEMA_INVALID /args/expected_version"},
	} {
		if got := verdict(x.cmd.Validate()); got != x.want {
			t.Errorf("%T %+v: %s, want %s", x.cmd, x.cmd, got, x.want)
		}
	}
	pref := validCertificate()
	pref.CertificateKind, pref.AgreementCode, pref.OriginForm, pref.IssuingBody = CertificatePreferentialOrigin, "RCEP", OriginFormCertificate, IssuingBodyCustoms
	for _, x := range []struct {
		edit func(*RegisterCertificate)
		want string
	}{
		{func(*RegisterCertificate) {}, "OK"},
		{func(c *RegisterCertificate) { c.OriginForm, c.IssuingBody = OriginFormOriginDeclaration, "" }, "OK"},
		{func(c *RegisterCertificate) { c.AgreementCode = "" }, "SCHEMA_INVALID /args/agreement_code"},
		{func(c *RegisterCertificate) { c.AgreementCode = "rcep" }, "SCHEMA_INVALID /args/agreement_code"},
		{func(c *RegisterCertificate) { c.OriginForm = "" }, "SCHEMA_INVALID /args/origin_form"},
		{func(c *RegisterCertificate) { c.IssuingBody = "" }, "SCHEMA_INVALID /args/issuing_body"},
		{func(c *RegisterCertificate) { c.IssuingBody = "chamber" }, "SCHEMA_INVALID /args/issuing_body"},
		{func(c *RegisterCertificate) { c.OriginForm = OriginFormOriginDeclaration }, "SCHEMA_INVALID /args/issuing_body"},
		{func(c *RegisterCertificate) { c.CertificateKind = CertificateOfOrigin }, "SCHEMA_INVALID /args/agreement_code"},
	} {
		c := pref
		x.edit(&c)
		if got := verdict(c.Validate()); got != x.want {
			t.Errorf("%+v: %s, want %s", c, got, x.want)
		}
	}
	use := CertificateUse{Ref: CertificateRef{CertificateID: "crt.1"}, Kind: CertificatePreferentialOrigin, Target: ExportTarget{Kind: TargetShipment, ID: "s"},
		At: time.Now()}
	if err := use.Validate("/uses/0"); err != nil {
		t.Fatalf("a use may need a preferential certificate: %v", err)
	}
	for _, x := range []struct {
		q    PurchaseInvoiceQuery
		want string
	}{
		{PurchaseInvoiceQuery{CommitmentIDs: []string{"commitment.1"}}, "OK"},
		{PurchaseInvoiceQuery{InvoiceIDs: []string{"pin.1"}}, "OK"},
		{PurchaseInvoiceQuery{}, "SCHEMA_INVALID /commitment_ids"},
		{PurchaseInvoiceQuery{CommitmentIDs: []string{""}}, "SCHEMA_INVALID /commitment_ids/0"},
		{PurchaseInvoiceQuery{InvoiceIDs: []string{"pin 1"}}, "SCHEMA_INVALID /invoice_ids/0"},
		{PurchaseInvoiceQuery{CommitmentIDs: make([]string, MaxInvoiceLookups+1)}, "SCHEMA_INVALID /commitment_ids"},
	} {
		if got := verdict(x.q.Validate()); got != x.want {
			t.Errorf("%+v: %s, want %s", x.q, got, x.want)
		}
	}
	if (PurchaseInvoiceRef{}).Reversed() || !(PurchaseInvoiceRef{Reversal: ReversalPartial}).Reversed() {
		t.Fatal("invoice_reversed")
	}
}
