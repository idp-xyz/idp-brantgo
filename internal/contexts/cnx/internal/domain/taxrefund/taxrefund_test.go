package taxrefund

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Python V0.50 has no tax refund case ("—（Python 未实现）"): the vectors come from the design
// (CNX-INV-004 "同一购进凭证 1,000 件分两批出口分配 600＋400", CN-R04, CN-R08, CN-R09, CN-AT04, CN-AT10,
// CN-AT12) and are synthetic; CN-E01 forbids an invented refund arithmetic, so every amount is
// stated, none derived.

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 {
		return rej.Code + " " + rej.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

func d(s string) kernel.Decimal { return kernel.MustDecimal(s) }

// world is one verified invoice line of 1,000 PCS and two export lots of 600 and 500 PCS.
type world struct {
	invoice map[InvoiceLineRef]InvoiceLine
	used    map[LineKey]kernel.Decimal
}

func newWorld() *world {
	return &world{invoice: map[InvoiceLineRef]InvoiceLine{{InvoiceID: "pin.1", LineNo: 1}: {InvoiceID: "pin.1", InvoiceStatus: "verified",
		Colour: "blue", Kind: "special_vat", Number: "SYN0001", LineNo: 1, Unit: "PCS", Quantity: d("1000")}}, used: map[LineKey]kernel.Decimal{}}
}

func newCase(id, method string) *Case {
	return &Case{ID: id, ExportLotID: "lot." + id, Attempt: 1, Status: Expected, RowVersion: 1, Policy: Binding{PolicyID: "export.tax", Revision: 1,
		RefundMethod: method}}
}

func lot(id, quantity string) map[string]ExportLine {
	return map[string]ExportLine{id: {LineID: id, ShipmentID: "ship.1", OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 1, Quantity: d(quantity), Unit: "PCS"}}
}

// allocate allocates and, when it succeeds, counts the new allocations as the database would.
func (w *world) allocate(c *Case, existing []*Allocation, lines map[string]ExportLine, in ...AllocateInput) ([]*Allocation, error) {
	out, err := Allocate(Context{Case: c, Allocations: existing, ExportLines: lines, InvoiceLines: w.invoice, Used: w.used,
		By: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.tax"}, Epoch: 1, At: time.Now()}, in)
	if err == nil {
		for _, a := range out {
			k := LineKey{InvoiceID: a.InvoiceID, LineNo: a.LineNo, Purpose: a.Purpose}
			w.used[k] = w.used[k].Add(a.Quantity)
		}
	}
	return out, err
}

func in(id, exportLine, purpose, quantity string) AllocateInput {
	return AllocateInput{ID: id, ExportLineID: exportLine, InvoiceID: "pin.1", LineNo: 1, Purpose: purpose, Quantity: d(quantity)}
}

// CNX-INV-004 and CN-AT04: one purchase invoice line of 1,000 PCS used for two export lots, 600 +
// 400 for the refund; one more is TAX_EVIDENCE_EXCEEDED. The exemption purpose has its own balance
// ("相同凭证的不同用途不能混用同一个余额"); an export line takes at most its quantity per purpose.
func TestPurposeBalancesCNAT04(t *testing.T) {
	w := newWorld()
	a, b := newCase("a", MethodExemptRefund), newCase("b", MethodExemptRefund)
	first, err := w.allocate(a, nil, lot("x1", "600"), in("t1", "x1", ExportRefund, "600"))
	if err != nil || len(first) != 1 || first[0].Status() != Standing || !first[0].Counts() || first[0].InvoiceNumber != "SYN0001" ||
		first[0].ShipmentID != "ship.1" || !first[0].ExportQuantity.Equal(d("600")) {
		t.Fatalf("%+v %v", first, err)
	}
	if _, err := w.allocate(b, nil, lot("y1", "500"), in("t2", "y1", ExportRefund, "401")); code(err) != CodeTaxEvidenceExceeded+" /args/allocations/0/quantity" {
		t.Fatalf("600 + 401 of 1,000: %s", code(err))
	}
	if _, err := w.allocate(b, nil, lot("y1", "500"), in("t2", "y1", ExportRefund, "400")); err != nil {
		t.Fatalf("600 + 400 of 1,000: %v", err)
	}
	if _, err := w.allocate(b, nil, lot("y2", "500"), in("t3", "y2", ExportRefund, "1")); code(err) != CodeTaxEvidenceExceeded+" /args/allocations/0/quantity" {
		t.Fatalf("1,000 + 1: %s", code(err))
	}
	// The exemption purpose is a balance of its own; an export line of 500 holds 500 of it.
	if _, err := w.allocate(b, nil, lot("y1", "500"), in("t4", "y1", ExportExemption, "501")); code(err) != CodeTaxEvidenceExceeded+" /args/allocations/0/quantity" {
		t.Fatalf("501 on an export line of 500: %s", code(err))
	}
	if _, err := w.allocate(b, nil, lot("y1", "500"), in("t4", "y1", ExportExemption, "300"), in("t5", "y1", ExportExemption, "201")); code(err) !=
		CodeTaxEvidenceExceeded+" /args/allocations/1/quantity" {
		t.Fatalf("300 + 201 in one command on an export line of 500: %s", code(err))
	}
	if _, err := w.allocate(b, nil, lot("y1", "500"), in("t4", "y1", ExportExemption, "500")); err != nil {
		t.Fatalf("the exemption balance is untouched by the refund's: %v", err)
	}
	if Categories[CodeTaxEvidenceExceeded] != kernel.CategoryConflict {
		t.Fatal("TAX_EVIDENCE_EXCEEDED is a conflict")
	}
}

// What an allocation may use: an export line of the lot, a line of a verified blue invoice, a
// purpose the bound refund method admits, the invoice line's unit, a case still taking evidence.
func TestAllocationRefusals(t *testing.T) {
	w := newWorld()
	w.invoice[InvoiceLineRef{InvoiceID: "pin.2", LineNo: 1}] = InvoiceLine{InvoiceID: "pin.2", InvoiceStatus: "reversed", Colour: "blue", LineNo: 1,
		Unit: "PCS", Quantity: d("10")}
	w.invoice[InvoiceLineRef{InvoiceID: "pin.3", LineNo: 1}] = InvoiceLine{InvoiceID: "pin.3", InvoiceStatus: "verified", Colour: "red", LineNo: 1,
		Unit: "PCS", Quantity: d("10")}
	w.invoice[InvoiceLineRef{InvoiceID: "pin.4", LineNo: 1}] = InvoiceLine{InvoiceID: "pin.4", InvoiceStatus: "verified", Colour: "blue", LineNo: 1,
		Unit: "KGM", Quantity: d("10")}
	of := func(invoice string) AllocateInput {
		a := in("t1", "x1", ExportRefund, "1")
		a.InvoiceID = invoice
		return a
	}
	submitted := newCase("s", MethodExemptRefund)
	submitted.Status = Submitted
	for _, c := range []struct {
		c    *Case
		a    AllocateInput
		want string
	}{
		{submitted, in("t1", "x1", ExportRefund, "1"), "INVALID_STATE /args/export_lot_id"},
		{newCase("a", MethodExemptRefund), in("t1", "x9", ExportRefund, "1"), "REFERENCE_INVALID /args/allocations/0/export_line_id"},
		{newCase("a", MethodExemptRefund), of("pin.9"), "REFERENCE_INVALID /args/allocations/0/line_no"},
		{newCase("a", MethodExemptRefund), of("pin.2"), "INVALID_STATE /args/allocations/0/purchase_invoice_id"},
		{newCase("a", MethodExemptRefund), of("pin.3"), "INVALID_STATE /args/allocations/0/purchase_invoice_id"},
		{newCase("a", MethodNotApplicable), in("t1", "x1", ExportRefund, "1"), "INVALID_KNOWN_VALUE /args/allocations/0/purpose"},
		// S5R-D14: export_refund serves 免抵退 and 免退 only.
		{newCase("a", MethodExempt), in("t1", "x1", ExportRefund, "1"), "INVALID_KNOWN_VALUE /args/allocations/0/purpose"},
		{newCase("a", MethodTaxable), in("t1", "x1", ExportRefund, "1"), "INVALID_KNOWN_VALUE /args/allocations/0/purpose"},
		{newCase("a", MethodExemptRefund), of("pin.4"), "INVALID_KNOWN_VALUE /args/allocations/0/quantity"},
	} {
		if _, err := w.allocate(c.c, nil, lot("x1", "600"), c.a); code(err) != c.want {
			t.Errorf("%s, want %s", code(err), c.want)
		}
	}
	if _, err := w.allocate(newCase("a", MethodNotApplicable), nil, lot("x1", "600"), in("t1", "x1", ExportExemption, "1")); err != nil {
		t.Fatalf("an exemption under not_applicable: %v", err)
	}
}

// CN-R06/CN-AT05 meet CN-R09: once its invoice is reversed by a red one (an external fact), an
// allocation is under review and still holds its export line; a new allocation of the same export
// line from another invoice replaces it. Nothing else is replaced.
func TestReviewAndReplacementCNAT05(t *testing.T) {
	w := newWorld()
	w.invoice[InvoiceLineRef{InvoiceID: "pin.5", LineNo: 1}] = InvoiceLine{InvoiceID: "pin.5", InvoiceStatus: "verified", Colour: "blue", LineNo: 1,
		Unit: "PCS", Quantity: d("600")}
	c := newCase("a", MethodExemptRefund)
	old, err := w.allocate(c, nil, lot("x1", "600"), in("t1", "x1", ExportRefund, "600"))
	w.must(t, err)
	old[0].InvoiceStatus = "reversed"
	if old[0].Status() != Review || !old[0].Counts() {
		t.Fatalf("%s", old[0].Status())
	}
	repl := AllocateInput{ID: "t2", ExportLineID: "x1", InvoiceID: "pin.5", LineNo: 1, Purpose: ExportRefund, Quantity: d("600")}
	noRepl := repl
	if _, err := w.allocate(c, old, lot("x1", "600"), noRepl); code(err) != CodeTaxEvidenceExceeded+" /args/allocations/0/quantity" {
		t.Fatalf("the reviewed allocation still holds the export line: %s", code(err))
	}
	repl.Replaces = "t1"
	got, err := w.allocate(c, old, lot("x1", "600"), repl)
	if err != nil || got[0].Replaces != "t1" {
		t.Fatalf("%+v %v", got, err)
	}
	old[0].ReplacedBy = "t2"
	if old[0].Status() != Replaced || old[0].Counts() {
		t.Fatalf("%s", old[0].Status())
	}
	standing := []*Allocation{{ID: "t9", ExportLineID: "x1", Purpose: ExportRefund, Quantity: d("1"), InvoiceStatus: "verified"}}
	bad := AllocateInput{ID: "t3", ExportLineID: "x1", InvoiceID: "pin.5", LineNo: 1, Purpose: ExportRefund, Quantity: d("1"), Replaces: "t9"}
	if _, err := w.allocate(c, standing, lot("x1", "600"), bad); code(err) != "INVALID_STATE /args/allocations/0/replaces" {
		t.Fatalf("a standing allocation is never replaced: %s", code(err))
	}
	rejected := &Allocation{CaseStatus: Rejected, InvoiceStatus: "verified"}
	if rejected.Status() != Released || rejected.Counts() {
		t.Fatal("a rejected case's allocations are released")
	}
}

func (w *world) must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// CN-AT12: the revision in force at the export date is the one published at or before it, not
// ended by it and valid at it; a later revision does not reach back (CN-R08 "适用方法变化不追溯覆盖历史").
func TestRevisionInForceAtTheExportDateCNAT12(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	revisions := []Revision{
		{Revision: 1, Status: "superseded", PublishedAt: t0, EndedAt: t0.Add(48 * time.Hour), ValidFrom: t0},
		{Revision: 2, Status: "active", PublishedAt: t0.Add(48 * time.Hour), ValidFrom: t0},
		{Revision: 3, Status: "draft", ValidFrom: t0},
	}
	for at, want := range map[time.Time]int64{
		t0.Add(-time.Hour):     0,
		t0:                     1,
		t0.Add(24 * time.Hour): 1,
		t0.Add(48 * time.Hour): 2,
		t0.Add(96 * time.Hour): 2,
	} {
		if got := InForceAt(revisions, at); got != want {
			t.Errorf("at %s: %d, want %d", at, got, want)
		}
	}
	limited := []Revision{{Revision: 1, PublishedAt: t0, ValidFrom: t0.Add(time.Hour), ValidUntil: t0.Add(2 * time.Hour)}}
	if InForceAt(limited, t0) != 0 || InForceAt(limited, t0.Add(time.Hour)) != 1 || InForceAt(limited, t0.Add(2*time.Hour)) != 0 {
		t.Fatal("validity is half-open")
	}
}

func money(t *testing.T, amount string) *kernel.Money {
	t.Helper()
	m, err := kernel.ParseAmount(kernel.MustCurrency("CNY", 2), amount)
	if err != nil {
		t.Fatal(err)
	}
	return &m
}

// SM-CNX-TAXCASE, CN-R08 and CN-AT10: expected → planned → submitted → approved → received, or
// submitted → rejected; an estimate may be stated again while expected; a plan needs standing
// evidence and none under review; every amount of a case is in one currency. Expected 1,000.00,
// approved 950.00 with 30.00 offset, received 950.00 stay apart; the differences are exact
// subtractions, nothing else is derived.
func TestStagesAndAmountsCNAT10(t *testing.T) {
	c := newCase("a", MethodExemptCreditRefund)
	cny := &Currency{ConfigID: "CNY", Revision: 1, Code: "CNY", MinorUnits: 2}
	standing := []*Allocation{{ID: "t1", InvoiceStatus: "verified", CaseStatus: Expected}}
	var results []*Result
	step := func(r *Result, allocations []*Allocation, want string) {
		t.Helper()
		got, err := Record(c, results, allocations, r)
		if code(err) != want {
			t.Fatalf("%s from %s: %s, want %s", r.Stage, c.Status, code(err), want)
		}
		if err == nil {
			c.Status = got
			results = append(results, r)
		}
	}
	step(&Result{Stage: Submitted}, standing, "INVALID_STATE /args/stage")
	step(&Result{Stage: Expected, Currency: cny, Amount: money(t, "900.00")}, nil, "OK")
	step(&Result{Stage: Expected, Currency: cny, Amount: money(t, "1000.00")}, nil, "OK")
	step(&Result{Stage: Planned, Currency: cny, Amount: money(t, "1000.00")}, nil, "INVALID_STATE /args/stage")
	step(&Result{Stage: Planned, Currency: cny, Amount: money(t, "1000.00")},
		[]*Allocation{{ID: "t1", InvoiceStatus: "reversed", CaseStatus: Expected}}, "INVALID_STATE /args/stage")
	usd := &Currency{ConfigID: "USD", Revision: 1, Code: "USD", MinorUnits: 2}
	step(&Result{Stage: Planned, Currency: usd, Amount: money(t, "1000.00")}, standing, "INVALID_KNOWN_VALUE /args/currency")
	step(&Result{Stage: Planned, Currency: cny, Amount: money(t, "1000.00")}, standing, "OK")
	step(&Result{Stage: Expected, Currency: cny, Amount: money(t, "1.00")}, standing, "INVALID_STATE /args/stage")
	step(&Result{Stage: Approved}, standing, "INVALID_STATE /args/stage")
	step(&Result{Stage: Submitted}, standing, "OK")
	step(&Result{Stage: Approved, Currency: cny, Amount: money(t, "950.00"), Offset: money(t, "30.00")}, standing, "OK")
	step(&Result{Stage: Rejected}, standing, "INVALID_STATE /args/stage")
	step(&Result{Stage: Received, Currency: cny, Amount: money(t, "950.00")}, standing, "OK")
	step(&Result{Stage: Received, Currency: cny, Amount: money(t, "950.00")}, standing, "INVALID_STATE /args/stage")
	a := AmountsOf(results)
	if a.Currency != "CNY" || a.Expected.MinorString() != "100000" || a.Planned.MinorString() != "100000" || a.Approved.MinorString() != "95000" ||
		a.Offset.MinorString() != "3000" || a.Received.MinorString() != "95000" || a.ApprovedLessExpected.MinorString() != "-5000" ||
		a.ReceivedLessApproved.MinorString() != "0" {
		t.Fatalf("%+v", a)
	}
	if b := AmountsOf(nil); b.Expected != nil || b.ApprovedLessExpected != nil {
		t.Fatalf("nothing stated, nothing shown: %+v", b)
	}
	r := newCase("r", MethodExemptRefund)
	r.Status = Submitted
	if got, err := Record(r, nil, standing, &Result{Stage: Rejected}); err != nil || got != Rejected {
		t.Fatalf("an external rejection: %s %v", got, err)
	}
	if code(CheckVersion(r, 9)) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal("CAS")
	}
}
