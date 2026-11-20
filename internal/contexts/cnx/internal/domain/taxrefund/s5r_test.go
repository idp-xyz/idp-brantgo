package taxrefund

import "testing"

// S5R-D14 (SM-CNX-TAXCASE expected|planned → converted_to_exemption): a case not declared in time converts to an
// exemption from expected or planned — only under a policy that refunds — and is final; its allocations keep counting
// (CN-R09: no evidence returns). S5R-D20: a filed case (submitted, approved, received) records the rollback of an
// invoice its counting evidence uses, once, without moving.
func TestS5RConversionAndRollbackD14D20(t *testing.T) {
	standing := []*Allocation{{ID: "t1", InvoiceID: "pin.1", Purpose: ExportRefund, InvoiceStatus: "verified", CaseStatus: Expected}}
	for _, from := range []string{Expected, Planned} {
		c := newCase("a", MethodExemptRefund)
		c.Status = from
		got, err := Record(c, nil, standing, &Result{Stage: ConvertedToExemption})
		if err != nil || got != ConvertedToExemption {
			t.Fatalf("from %s: %s %v", from, got, err)
		}
		c.Status = got
		for _, s := range []string{Expected, Planned, Submitted, Rejected, ConvertedToExemption, EvidenceRolledBack} {
			if _, err := Record(c, nil, standing, &Result{Stage: s, PurchaseInvoiceID: "pin.1"}); code(err) != "INVALID_STATE /args/stage" {
				t.Errorf("%s after the conversion: %s", s, code(err))
			}
		}
		a := *standing[0]
		a.CaseStatus = ConvertedToExemption
		if !a.Counts() || a.Status() != Standing {
			t.Fatal("a converted case's evidence keeps counting")
		}
	}
	exempt := newCase("x", MethodExempt)
	if _, err := Record(exempt, nil, nil, &Result{Stage: ConvertedToExemption}); code(err) != "INVALID_STATE /args/stage" {
		t.Fatalf("nothing to convert under an exemption: %s", code(err))
	}
	submitted := newCase("s", MethodExemptRefund)
	if _, err := Record(submitted, nil, standing, &Result{Stage: ConvertedToExemption}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{Submitted, Approved, Received} {
		c := newCase("f", MethodExemptRefund)
		c.Status = s
		if !Filed(s) {
			t.Fatalf("%s is filed", s)
		}
		if _, err := Record(c, nil, standing, &Result{Stage: EvidenceRolledBack, PurchaseInvoiceID: "pin.9"}); code(err) !=
			"INVALID_KNOWN_VALUE /args/purchase_invoice_id" {
			t.Errorf("%s: a rollback of an invoice the case does not use: %s", s, code(err))
		}
		first := &Result{ID: "trr.1", Stage: EvidenceRolledBack, PurchaseInvoiceID: "pin.1"}
		got, err := Record(c, nil, standing, first)
		if err != nil || got != s {
			t.Fatalf("%s: a rollback keeps the state: %s %v", s, got, err)
		}
		if _, err := Record(c, []*Result{first}, standing, &Result{Stage: EvidenceRolledBack, PurchaseInvoiceID: "pin.1"}); code(err) !=
			"INVALID_STATE /args/purchase_invoice_id" {
			t.Errorf("%s: a second rollback of the invoice: %s", s, code(err))
		}
	}
	if _, err := Record(newCase("e", MethodExemptRefund), nil, standing, &Result{Stage: EvidenceRolledBack, PurchaseInvoiceID: "pin.1"}); code(err) !=
		"INVALID_STATE /args/stage" {
		t.Fatalf("an unfiled case rolls nothing back: %s", code(err))
	}
	if Filed(Expected) || Filed(Planned) || Filed(Rejected) || Filed(ConvertedToExemption) {
		t.Fatal("only a filed case")
	}
}

// S5R-D20: after a partial reversal an invoice line evidences less; an allocation of a line whose evidence is then
// over-used is under review (it still counts and holds its export line, and blocks planned) and a replacement may take
// the remainder of the same line, its own quantity left out of the line's balance.
func TestS5ROverusedLineAndReplacementFromTheSameLineD20(t *testing.T) {
	w := newWorld()
	c := newCase("a", MethodExemptRefund)
	old, err := w.allocate(c, nil, lot("x1", "600"), in("t1", "x1", ExportRefund, "600"))
	w.must(t, err)
	// A red invoice reverses 500 of the line's 1,000: it evidences 500 now (InvoiceLines answers the remainder).
	w.invoice[InvoiceLineRef{InvoiceID: "pin.1", LineNo: 1}] = InvoiceLine{InvoiceID: "pin.1", InvoiceStatus: "verified", Colour: "blue",
		Kind: "special_vat", Number: "SYN0001", LineNo: 1, Unit: "PCS", Quantity: d("500")}
	old[0].LineOverused = true
	if old[0].Status() != Review || !old[0].Counts() {
		t.Fatalf("%s", old[0].Status())
	}
	if _, err := Record(c, nil, old, &Result{Stage: Planned}); code(err) != "INVALID_STATE /args/stage" {
		t.Fatalf("an allocation under review blocks planned: %s", code(err))
	}
	repl := in("t2", "x1", ExportRefund, "501")
	repl.Replaces = "t1"
	if _, err := w.allocate(c, old, lot("x1", "600"), repl); code(err) != CodeTaxEvidenceExceeded+" /args/allocations/0/quantity" {
		t.Fatalf("501 of the 500 the line evidences: %s", code(err))
	}
	repl.Quantity = d("500")
	got, err := w.allocate(c, old, lot("x1", "600"), repl)
	if err != nil || got[0].Replaces != "t1" || !got[0].Quantity.Equal(d("500")) {
		t.Fatalf("%+v %v", got, err)
	}
}
