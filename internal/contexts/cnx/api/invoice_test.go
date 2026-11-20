package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func pathOf(err error) string {
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 {
		return rej.Code + " " + rej.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

func facts() InvoiceFacts {
	return InvoiceFacts{Kind: InvoiceSpecialVAT, Colour: ColourBlue, Number: "SYN0001", IssuedOn: "2026-11-10",
		Supplier: InvoiceParty{PartyID: "supplier.1", Name: "合成供应商", TaxID: "SUPPLIER1"}, Buyer: InvoiceParty{Name: "合成企业", TaxID: "SYNTHETIC"},
		Currency: CurrencyRef{ID: "CNY", Revision: 1},
		Lines:    []InvoiceLine{{LineNo: 1, Description: "合成针织衫", Quantity: "120", Unit: "PCS", NetAmount: "600.00", TaxRate: "0.13", TaxAmount: "78.00"}},
		NetTotal: "600.00", TaxTotal: "78.00", GrossTotal: "678.00"}
}

// The closed invoice contracts (CNX-INV-005): an import carries the original's file and a whole
// candidate; a verification carries verified facts and matches of its own lines, a rejection a
// reason and nothing else; a red invoice names the blue one it reverses and matches nothing; a
// buyer is an entity of the invoice, never a PTY party of the request.
func TestInvoiceContracts(t *testing.T) {
	imp := ImportInvoice{IdempotencyKey: "k", FileID: "f", Candidate: facts()}
	if err := imp.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		edit func(*InvoiceFacts)
		want string
	}{
		{func(f *InvoiceFacts) { f.Kind = "commercial_invoice" }, "/kind"},
		{func(f *InvoiceFacts) { f.Colour = "green" }, "/colour"},
		{func(f *InvoiceFacts) { f.Number = "SYN 1" }, "/number"},
		{func(f *InvoiceFacts) { f.IssuedOn = "2026-13-01" }, "/issued_on"},
		{func(f *InvoiceFacts) { f.Supplier.TaxID = "supplier1" }, "/supplier/tax_id"},
		{func(f *InvoiceFacts) { f.Buyer.PartyID = "company.B" }, "/buyer/party_id"},
		{func(f *InvoiceFacts) { f.Lines = nil }, "/lines"},
		{func(f *InvoiceFacts) { f.Lines = append(f.Lines, f.Lines[0]) }, "/lines/1/line_no"},
		{func(f *InvoiceFacts) { f.Lines[0].Quantity = "0" }, "/lines/0/quantity"},
		{func(f *InvoiceFacts) { f.Lines[0].TaxRate = "1" }, "/lines/0/tax_rate"},
		{func(f *InvoiceFacts) { f.Lines[0].NetAmount = "-1" }, "/lines/0/net_amount"},
		{func(f *InvoiceFacts) { f.RedOf = &InvoiceRef{Kind: InvoiceSpecialVAT, Number: "SYN0000"} }, "/red_of"},
		{func(f *InvoiceFacts) { f.Colour = ColourRed }, "/red_of"},
		{func(f *InvoiceFacts) {
			f.Colour, f.RedOf = ColourRed, &InvoiceRef{Kind: InvoiceSpecialVAT, Number: "SYN0001"}
		}, "/red_of/number"},
	} {
		x := imp
		x.Candidate = facts()
		c.edit(&x.Candidate)
		if got := pathOf(x.Validate()); got != "SCHEMA_INVALID /args/candidate"+c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
	f := facts()
	verify := VerifyInvoice{IdempotencyKey: "k", PurchaseInvoiceID: "pin.1", ExpectedVersion: 1, Decision: DecisionVerify, Facts: &f,
		Matches: []InvoiceMatch{{LineNo: 1, CommitmentID: "commitment.1", OrderLineID: "L1", Quantity: "120"}}}
	if err := verify.Validate(); err != nil {
		t.Fatal(err)
	}
	reject := VerifyInvoice{IdempotencyKey: "k", PurchaseInvoiceID: "pin.1", ExpectedVersion: 1, Decision: DecisionReject, Reason: "重复件"}
	if err := reject.Validate(); err != nil {
		t.Fatal(err)
	}
	red := facts()
	red.Colour, red.Number, red.RedOf = ColourRed, "SYN0002", &InvoiceRef{Kind: InvoiceSpecialVAT, Number: "SYN0001"}
	for _, c := range []struct {
		cmd  VerifyInvoice
		want string
	}{
		{VerifyInvoice{IdempotencyKey: "k", PurchaseInvoiceID: "pin.1", ExpectedVersion: 1, Decision: DecisionVerify}, "/args/facts"},
		{VerifyInvoice{IdempotencyKey: "k", PurchaseInvoiceID: "pin.1", ExpectedVersion: 1, Decision: "accept", Facts: &f}, "/args/decision"},
		{VerifyInvoice{IdempotencyKey: "k", PurchaseInvoiceID: "pin.1", ExpectedVersion: 1, Decision: DecisionReject, Facts: &f, Reason: "x"}, "/args/facts"},
		{VerifyInvoice{IdempotencyKey: "k", PurchaseInvoiceID: "pin.1", ExpectedVersion: 1, Decision: DecisionReject}, "/args/reason"},
		{VerifyInvoice{IdempotencyKey: "k", PurchaseInvoiceID: "pin.1", ExpectedVersion: 0, Decision: DecisionReject, Reason: "x"}, "/args/expected_version"},
		{VerifyInvoice{IdempotencyKey: "k", PurchaseInvoiceID: "pin.1", ExpectedVersion: 1, Decision: DecisionVerify, Facts: &f,
			Matches: []InvoiceMatch{{LineNo: 2, CommitmentID: "c", OrderLineID: "L1", Quantity: "1"}}}, "/args/matches/0/line_no"},
		{VerifyInvoice{IdempotencyKey: "k", PurchaseInvoiceID: "pin.1", ExpectedVersion: 1, Decision: DecisionVerify, Facts: &f,
			Matches: []InvoiceMatch{verify.Matches[0], verify.Matches[0]}}, "/args/matches/1/order_line_id"},
	} {
		if got := pathOf(c.cmd.Validate()); got != "SCHEMA_INVALID "+c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
	// S5R-D20: a red invoice's matches are the blue matches it takes back, on its own lines.
	if err := (VerifyInvoice{IdempotencyKey: "k", PurchaseInvoiceID: "pin.1", ExpectedVersion: 1, Decision: DecisionVerify, Facts: &red,
		Matches: verify.Matches}).Validate(); err != nil {
		t.Fatalf("a red invoice takes back matches: %v", err)
	}
	for _, code := range []string{CodeInvoiceDuplicate, CodeTaxEvidenceExceeded, CodePartyEntityMismatch} {
		if c, ok := Category(code); !ok || c != kernel.CategoryConflict {
			t.Errorf("%s: %q %v", code, c, ok)
		}
	}
}

// The closed tax refund contracts (CNX-INV-004, SM-CNX-TAXCASE, CN-R08): an allocation names its
// export line, invoice line, purpose and quantity; a replacement never replaces one of its own
// command; each stage of record_result takes its members and no other.
func TestTaxRefundContracts(t *testing.T) {
	alloc := AllocateTaxEvidence{IdempotencyKey: "k", ExportLotID: "lot.1", PolicyID: "export.tax", Allocations: []EvidenceAllocation{
		{AllocationID: "t1", ExportLineID: "x1", PurchaseInvoiceID: "pin.1", LineNo: 1, Purpose: PurposeExportRefund, Quantity: "600"},
		{AllocationID: "t2", ExportLineID: "x2", PurchaseInvoiceID: "pin.1", LineNo: 1, Purpose: PurposeExportExemption, Quantity: "1", Replaces: "t0"}}}
	if err := alloc.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		edit func(*AllocateTaxEvidence)
		want string
	}{
		{func(a *AllocateTaxEvidence) { a.Allocations = nil }, "/args/allocations"},
		{func(a *AllocateTaxEvidence) { a.PolicyID = "" }, "/args/policy_id"},
		{func(a *AllocateTaxEvidence) { a.Allocations[1].AllocationID = "t1" }, "/args/allocations/1/allocation_id"},
		{func(a *AllocateTaxEvidence) { a.Allocations[0].Purpose = "input_credit" }, "/args/allocations/0/purpose"},
		{func(a *AllocateTaxEvidence) { a.Allocations[0].Quantity = "0" }, "/args/allocations/0/quantity"},
		{func(a *AllocateTaxEvidence) { a.Allocations[1].Replaces = "t1" }, "/args/allocations/1/replaces"},
		{func(a *AllocateTaxEvidence) { a.Allocations[0].Replaces = "t2" }, "/args/allocations"},
	} {
		x := alloc
		x.Allocations = append([]EvidenceAllocation(nil), alloc.Allocations...)
		c.edit(&x)
		if got := pathOf(x.Validate()); got != "SCHEMA_INVALID "+c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
	cny := &CurrencyRef{ID: "CNY", Revision: 1}
	base := RecordTaxResult{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 2}
	with := func(edit func(*RecordTaxResult)) RecordTaxResult { r := base; edit(&r); return r }
	ok := []RecordTaxResult{
		with(func(r *RecordTaxResult) {
			r.Stage, r.Currency, r.Amount, r.Reason = TaxExpected, cny, "1000.00", "按合成口径估计"
		}),
		with(func(r *RecordTaxResult) {
			r.Stage, r.Currency, r.Amount, r.TaxPeriod = TaxPlanned, cny, "1000.00", "2026-11"
			r.Review = &ProfessionalReview{Reviewer: "合成税务师", EvidenceIDs: []string{"e1"}}
		}),
		with(func(r *RecordTaxResult) {
			r.Stage, r.ExternalReference, r.EvidenceIDs = TaxSubmitted, "SB-1", []string{"e1"}
		}),
		with(func(r *RecordTaxResult) {
			r.Stage, r.Currency, r.Amount, r.OffsetAmount, r.ResultText, r.EvidenceIDs = TaxApproved, cny, "950.00", "30.00", "核准", []string{"e1"}
		}),
		with(func(r *RecordTaxResult) {
			r.Stage, r.Currency, r.Amount, r.FundRecordID = TaxReceived, cny, "950.00", "fund.1"
		}),
		with(func(r *RecordTaxResult) {
			r.Stage, r.ResultText, r.EvidenceIDs = TaxRejected, "不予核准", []string{"e1"}
		}),
	}
	for _, r := range ok {
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", r.Stage, err)
		}
	}
	for _, c := range []struct {
		r    RecordTaxResult
		want string
	}{
		{with(func(r *RecordTaxResult) { r.Stage = "cancelled" }), "/args/stage"},
		{with(func(r *RecordTaxResult) { r.Stage, r.Currency, r.Amount = TaxExpected, cny, "1000.00" }), "/args/reason"},
		{with(func(r *RecordTaxResult) { r.Stage, r.Amount, r.Reason = TaxExpected, "1000.00", "x" }), "/args/currency/id"},
		{with(func(r *RecordTaxResult) { r.Stage, r.Currency, r.Amount, r.TaxPeriod = TaxPlanned, cny, "1", "2026-13" }), "/args/tax_period"},
		{with(func(r *RecordTaxResult) { r.Stage, r.Currency, r.Amount, r.TaxPeriod = TaxPlanned, cny, "1", "2026-11" }), "/args/review"},
		{with(func(r *RecordTaxResult) { r.Stage, r.ExternalReference = TaxSubmitted, "SB-1" }), "/args/evidence_ids"},
		{with(func(r *RecordTaxResult) {
			r.Stage, r.ExternalReference, r.EvidenceIDs, r.Amount = TaxSubmitted, "SB-1", []string{"e"}, "1"
		}), "/args/amount"},
		{with(func(r *RecordTaxResult) { r.Stage, r.Currency, r.Amount, r.FundRecordID = TaxReceived, cny, "1", "" }), "/args/fund_record_id"},
		{with(func(r *RecordTaxResult) {
			r.Stage, r.Currency, r.Amount, r.FundRecordID, r.OffsetAmount = TaxReceived, cny, "1", "fund.1", "1"
		}), "/args/offset_amount"},
		{with(func(r *RecordTaxResult) { r.Stage, r.EvidenceIDs = TaxRejected, []string{"e1"} }), "/args/result_text"},
	} {
		if got := pathOf(c.r.Validate()); got != "SCHEMA_INVALID "+c.want {
			t.Errorf("%s: %s, want %s", c.r.Stage, got, c.want)
		}
	}
}
