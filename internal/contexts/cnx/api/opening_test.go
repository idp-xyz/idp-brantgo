package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The closed contract of tax_refund_case.record_opening (S5 unit C10; OpeningPosition; CN-AT16 CNX half): each kind
// takes its members and omits the other's; an opening case's position takes the members record_result takes for its
// stage, at /args/position; received and rejected are outcomes, no position. A credit_note reconciliation is an
// amount (CN-AT11).
func TestTaxOpeningContract(t *testing.T) {
	balance := RecordTaxOpening{IdempotencyKey: "k", OpeningID: "op.1", OpeningKind: OpeningInvoiceBalance, CutoffAt: "2026-10-31T16:00:00Z",
		SourceDocument: "TR-REG/2026-10/0042", EvidenceIDs: []string{"tax.register"}, PurchaseInvoiceID: "pin.1",
		Balances: []OpeningBalance{{LineNo: 1, Purpose: PurposeExportRefund, Consumed: "600"}, {LineNo: 1, Purpose: PurposeExportExemption, Consumed: "0"}}}
	if err := balance.Validate(); err != nil {
		t.Fatal(err)
	}
	usd := &CurrencyRef{ID: "CNY", Revision: 1}
	opening := RecordTaxOpening{IdempotencyKey: "k", OpeningID: "op.2", OpeningKind: OpeningRefundCase, CutoffAt: balance.CutoffAt,
		SourceDocument: "TR-REG/2026-10/0043", EvidenceIDs: []string{"declaration.copy"},
		Export: &OpeningExport{Reference: "223320260001234567", ExportedAt: "2026-09-20T00:00:00Z", OrderReference: "SO-LEGACY-42",
			Lines: []OpeningExportLine{{LineID: "1", Quantity: "300", Unit: "PCS"}}},
		PolicyID:    "export.tax",
		Allocations: []OpeningAllocation{{AllocationID: "oa.1", ExportLineID: "1", PurchaseInvoiceID: "pin.2", LineNo: 1, Purpose: PurposeExportRefund, Quantity: "300"}},
		Position: &OpeningPosition{Stage: TaxApproved, Currency: usd, Amount: "3900.00", ResultText: "核准通知书（合成）",
			EvidenceIDs: []string{"approval.notice"}}}
	if err := opening.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		base   RecordTaxOpening
		mutate func(*RecordTaxOpening)
		path   string
	}{
		"kind":              {balance, func(c *RecordTaxOpening) { c.OpeningKind = "invoice" }, "/args/kind"},
		"cutoff":            {balance, func(c *RecordTaxOpening) { c.CutoffAt = "2026-10-31" }, "/args/cutoff_at"},
		"source":            {balance, func(c *RecordTaxOpening) { c.SourceDocument = "" }, "/args/source_document"},
		"no evidence":       {balance, func(c *RecordTaxOpening) { c.EvidenceIDs = nil }, "/args/evidence_ids"},
		"invoice":           {balance, func(c *RecordTaxOpening) { c.PurchaseInvoiceID = "" }, "/args/purchase_invoice_id"},
		"no balances":       {balance, func(c *RecordTaxOpening) { c.Balances = nil }, "/args/balances"},
		"purpose twice":     {balance, func(c *RecordTaxOpening) { c.Balances[1].Purpose = PurposeExportRefund }, "/args/balances/1/purpose"},
		"consumed":          {balance, func(c *RecordTaxOpening) { c.Balances[0].Consumed = "-1" }, "/args/balances/0/consumed"},
		"balance export":    {balance, func(c *RecordTaxOpening) { c.Export = opening.Export }, "/args/export"},
		"case invoice":      {opening, func(c *RecordTaxOpening) { c.PurchaseInvoiceID = "pin.1" }, "/args/purchase_invoice_id"},
		"case no export":    {opening, func(c *RecordTaxOpening) { c.Export = nil }, "/args/export"},
		"case no policy":    {opening, func(c *RecordTaxOpening) { c.PolicyID = "" }, "/args/policy_id"},
		"case no position":  {opening, func(c *RecordTaxOpening) { c.Position = nil }, "/args/position"},
		"position received": {opening, func(c *RecordTaxOpening) { p := *c.Position; p.Stage = TaxReceived; c.Position = &p }, "/args/position/stage"},
		"position text":     {opening, func(c *RecordTaxOpening) { p := *c.Position; p.ResultText = ""; c.Position = &p }, "/args/position/result_text"},
		"no allocations":    {opening, func(c *RecordTaxOpening) { c.Allocations = nil }, "/args/allocations"},
	} {
		x := c.base
		x.Balances = append([]OpeningBalance(nil), c.base.Balances...)
		c.mutate(&x)
		if got := pathOf(x.Validate()); got != kernel.CodeSchemaInvalid+" "+c.path {
			t.Errorf("%s: %q, want %q", name, got, c.path)
		}
	}
	r := ResolveReconciliation{IdempotencyKey: "k", BundleID: "dbn.1", ExpectedVersion: 1, ReconciliationID: "rec.cn", Subject: ReconciliationSubject{
		Kind: SubjectCreditNote, ID: "cn.1"}, Measure: MeasureAmount, Currency: &CurrencyRef{ID: "USD", Revision: 1},
		External: ReconciledValue{Value: "0.00", Source: "海关未改单，退税未调整（合成）", EvidenceIDs: []string{"e"}},
		Internal: ReconciledValue{Value: "100.00", Source: "STL 贷项 cn.1"}, Responsibility: Responsibility{Kind: "internal", Note: "待专业评估"},
		Adjustment: Adjustment{Kind: AdjustmentNone}, Reason: "出口后折让的税务影响记录"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	r.Measure, r.Currency, r.Unit = MeasureQuantity, nil, "PCS"
	if got := pathOf(r.Validate()); got != kernel.CodeSchemaInvalid+" /args/measure" {
		t.Fatalf("a credit note as a quantity: %s", got)
	}
}
