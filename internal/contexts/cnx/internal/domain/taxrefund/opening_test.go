package taxrefund

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Opening refund material (S5 unit C10, WBS S5-20; OpeningPosition; CNX-INV-004; MD-51; CN-R09; CN-AT16 CNX half,
// BT17). Synthetic vectors: CN-E01 forbids invented refund arithmetic, so only quantities are judged.

// CN-AT16 (未退税资料导入; 截止、来源余额可核对): declarations finished before the cutoff consumed 600 of the 1,000 PCS
// invoice line for export_refund. The remaining 400 is all a later allocation may take (TAX_EVIDENCE_EXCEEDED for
// 401); the export_exemption purpose keeps its own 1,000. A line the invoice lacks is REFERENCE_INVALID, a red or
// unverified invoice INVALID_STATE.
func TestAnInvoiceBalanceConsumesWhatFinishedDeclarationsUsed(t *testing.T) {
	w := newWorld()
	got, err := ConsumedAtCutoff("op.1", "pin.1", w.invoice, w.used, []BalanceInput{{LineNo: 1, Purpose: ExportRefund, Consumed: d("600")}})
	if err != nil || len(got) != 1 || got[0].Consumed.String() != "600" || got[0].Unit != "PCS" || got[0].OpeningID != "op.1" {
		t.Fatalf("%+v %v", got, err)
	}
	w.used[LineKey{InvoiceID: "pin.1", LineNo: 1, Purpose: ExportRefund}] = d("600") // as UsedEvidence counts it from now on
	c := newCase("c1", MethodExemptCreditRefund)
	if _, err = w.allocate(c, nil, lot("e1", "600"), in("a1", "e1", ExportRefund, "401")); code(err) != CodeTaxEvidenceExceeded+" /args/allocations/0/quantity" {
		t.Fatalf("401 of the remaining 400: %s", code(err))
	}
	if _, err = w.allocate(c, nil, lot("e1", "600"), in("a1", "e1", ExportRefund, "400")); err != nil {
		t.Fatalf("400: %v", err)
	}
	if _, err = w.allocate(c, nil, lot("e1", "600"), in("a2", "e1", ExportExemption, "600")); err != nil {
		t.Fatalf("another purpose: %v", err)
	}
	// Stated again on top of what is used now, the consumption could not fit: nothing is consumed twice.
	_, err = ConsumedAtCutoff("op.2", "pin.1", w.invoice, w.used, []BalanceInput{{LineNo: 1, Purpose: ExportRefund, Consumed: d("1")}})
	if code(err) != CodeTaxEvidenceExceeded+" /args/balances/0/consumed" {
		t.Fatalf("over the balance: %s", code(err))
	}
	if _, err = ConsumedAtCutoff("op.3", "pin.1", w.invoice, w.used, []BalanceInput{{LineNo: 2, Purpose: ExportRefund}}); code(err) != kernel.CodeReferenceInvalid+" /args/balances/0/line_no" {
		t.Fatalf("no such line: %s", code(err))
	}
	red := newWorld()
	l := red.invoice[InvoiceLineRef{InvoiceID: "pin.1", LineNo: 1}]
	l.Colour = "red"
	red.invoice[InvoiceLineRef{InvoiceID: "pin.1", LineNo: 1}] = l
	if _, err = ConsumedAtCutoff("op.4", "pin.1", red.invoice, red.used, []BalanceInput{{LineNo: 1, Purpose: ExportRefund}}); code(err) != kernel.CodeInvalidState+" /args/purchase_invoice_id" {
		t.Fatalf("red: %s", code(err))
	}
	if Categories[CodeOpeningBalanceDuplicate] != kernel.CategoryConflict || code(Duplicate("/args/source_document", "x")) != CodeOpeningBalanceDuplicate+" /args/source_document" {
		t.Fatal("OPENING_BALANCE_DUPLICATE is the domain's conflict")
	}
}

// An opening case opens in its position — here submitted at the cutoff, where tax_refund_case.allocate takes
// nothing (INVALID_STATE) — with the allocations its declaration uses, judged by allocate's rules (CNX-INV-004:
// per purpose within the invoice line and the export line); a rejection, the external result, releases them
// (CN-R09), nothing else does.
func TestAnOpeningCaseTakesItsAllocationsInItsPosition(t *testing.T) {
	w := newWorld()
	c := newCase("op", MethodExemptCreditRefund)
	c.Status, c.Opening = Submitted, true
	x := Context{Case: c, ExportLines: lot("1", "300"), InvoiceLines: w.invoice, Used: w.used, By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "tax.1"},
		Epoch: 1, At: time.Now()}
	if _, err := Allocate(x, []AllocateInput{in("a1", "1", ExportRefund, "300")}); code(err) != kernel.CodeInvalidState+" /args/export_lot_id" {
		t.Fatalf("allocate on a submitted case: %s", code(err))
	}
	if _, err := OpenAllocations(x, []AllocateInput{in("a1", "1", ExportRefund, "301")}); code(err) != CodeTaxEvidenceExceeded+" /args/allocations/0/quantity" {
		t.Fatalf("over the export line: %s", code(err))
	}
	out, err := OpenAllocations(x, []AllocateInput{in("a1", "1", ExportRefund, "300")})
	if err != nil || len(out) != 1 || out[0].CaseStatus != Submitted || !out[0].Counts() {
		t.Fatalf("%+v %v", out, err)
	}
	out[0].CaseStatus = Rejected
	if out[0].Counts() || out[0].Status() != Released {
		t.Fatalf("a rejected opening case releases its evidence: %s", out[0].Status())
	}
	for _, p := range Positions {
		if p == Received || p == Rejected {
			t.Fatalf("an opening case opens before an external outcome, not %s", p)
		}
	}
}
