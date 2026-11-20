package purchaseinvoice

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Python V0.50 has no purchase invoice ("—（Python 未实现）"): the vectors come from the design
// (CNX-INV-005 "专票核验后匹配 120 件交付行", CN-R06, CN-AT05, CN-AT15, CN-AT18) and are synthetic.

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 {
		return rej.Code + " " + rej.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

var company = Company{LegalName: "合成企业", RegistrationNo: "SYNTHETIC-NOT-REAL"}

// blue is a special VAT invoice of 120 PCS at 5.00 CNY net, 13 % tax as printed (78.00).
func blue() Facts {
	return Facts{Kind: "special_vat", Colour: Blue, Number: "SYN0001", IssuedOn: "2026-11-10",
		Supplier: Party{PartyID: "supplier.1", Name: "合成供应商", TaxID: "SUPPLIER1"}, Buyer: Party{Name: "合成企业", TaxID: "SYNTHETIC-NOT-REAL"},
		Currency: Currency{ConfigID: "CNY", Revision: 1, Code: "CNY", MinorUnits: 2},
		Lines:    []Line{{LineNo: 1, Description: "合成针织衫", Quantity: "120", Unit: "PCS", NetAmount: "600.00", TaxRate: "0.13", TaxAmount: "78.00"}},
		NetTotal: "600.00", TaxTotal: "78.00", GrossTotal: "678.00"}
}

// The totals are the sums of the lines and net + tax is the gross; nothing is computed from the
// printed rate (CN-E01): a tax amount that is not 13 % of the net passes when the totals add up.
func TestAmountsAddUpWithoutAnyTaxComputation(t *testing.T) {
	if err := blue().CheckAmounts("/f"); err != nil {
		t.Fatal(err)
	}
	f := blue()
	f.Lines[0].TaxAmount, f.TaxTotal, f.GrossTotal = "77.99", "77.99", "677.99"
	if err := f.CheckAmounts("/f"); err != nil {
		t.Fatalf("the printed tax is recorded as printed: %v", err)
	}
	for _, c := range []struct {
		edit func(*Facts)
		want string
	}{
		{func(f *Facts) { f.NetTotal = "600.01" }, "INVALID_KNOWN_VALUE /f/net_total"},
		{func(f *Facts) { f.TaxTotal = "78.01" }, "INVALID_KNOWN_VALUE /f/tax_total"},
		{func(f *Facts) { f.GrossTotal = "678.01" }, "INVALID_KNOWN_VALUE /f/gross_total"},
		{func(f *Facts) { f.Lines[0].NetAmount = "600.001" }, "INVALID_KNOWN_VALUE /f/lines/0/net_amount"},
		{func(f *Facts) { f.Currency.Code = "" }, "REFERENCE_INVALID /f/currency"},
	} {
		f := blue()
		c.edit(&f)
		if got := code(f.CheckAmounts("/f")); got != c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
}

// CNX-INV-005 negative and CN-AT15 (invoice buyer): a buyer other than the company's entity — by
// taxpayer number or name — is PARTY_ENTITY_MISMATCH; a company without a profile cannot accept any.
func TestBuyerMustBeTheCompanysEntityCNAT15(t *testing.T) {
	if err := CheckBuyer(blue(), company, "/f"); err != nil {
		t.Fatal(err)
	}
	other := blue()
	other.Buyer.TaxID = "AFFILIATE9"
	if got := code(CheckBuyer(other, company, "/f")); got != CodePartyEntityMismatch+" /f/buyer/tax_id" {
		t.Fatal(got)
	}
	other = blue()
	other.Buyer.Name = "合成关联企业"
	if got := code(CheckBuyer(other, company, "/f")); got != CodePartyEntityMismatch+" /f/buyer/name" {
		t.Fatal(got)
	}
	if got := code(CheckBuyer(blue(), Company{}, "/f")); got != "REFERENCE_INVALID /f/buyer" {
		t.Fatal(got)
	}
	if Categories[CodePartyEntityMismatch] != kernel.CategoryConflict || Categories[CodeInvoiceDuplicate] != kernel.CategoryConflict {
		t.Fatal("PARTY_ENTITY_MISMATCH and INVOICE_DUPLICATE are conflicts")
	}
}

// CN-AT18 "显示来源与差异": an AI candidate that misread the quantity and the number shows both
// fields, with the candidate's and the verified value; equal facts show none.
func TestDifferencesBetweenCandidateAndVerifiedCNAT18(t *testing.T) {
	if d := Differences(blue(), blue()); len(d) != 0 {
		t.Fatalf("%+v", d)
	}
	candidate := blue()
	candidate.Number, candidate.Lines[0].Quantity = "SYN0007", "12"
	d := Differences(candidate, blue())
	if len(d) != 2 || d[0] != (Difference{Field: "/lines/0/quantity", Candidate: "12", Verified: "120"}) ||
		d[1] != (Difference{Field: "/number", Candidate: "SYN0007", Verified: "SYN0001"}) {
		t.Fatalf("%+v", d)
	}
	red := blue()
	red.Colour, red.RedOf = Red, &Ref{Kind: "special_vat", Number: "SYN0000"}
	d = Differences(blue(), red)
	if len(d) != 3 || d[0].Field != "/colour" || d[1] != (Difference{Field: "/red_of/kind", Verified: "special_vat"}) {
		t.Fatalf("a member only one side has %+v", d)
	}
}

// CNX-INV-005 positive "专票核验后匹配 120 件交付行" and baseline 22.2 (先货后票／先票后货 分别登记): an
// invoice line matches commitment lines of its supplier in its unit, at most its quantity, and a
// commitment line takes at most what it buys across invoices; a match records what was delivered
// then (100 of 120: the invoice came before the rest of the goods).
func TestMatchesToPurchaseLines(t *testing.T) {
	l1 := LineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}
	l2 := LineRef{CommitmentID: "commitment.2", OrderLineID: "L1"}
	lines := map[LineRef]PurchaseLine{
		l1: {SupplierID: "supplier.1", Unit: "PCS", PONumber: "PO-1", PurchaseQuantity: kernel.MustDecimal("120"), DeliveredQuantity: kernel.MustDecimal("100")},
		l2: {SupplierID: "supplier.1", Unit: "PCS", PONumber: "PO-2", PurchaseQuantity: kernel.MustDecimal("50")},
	}
	at := time.Now()
	ms, err := CheckMatches(blue(), "pin.1", []MatchInput{{LineNo: 1, Ref: l1, Quantity: kernel.MustDecimal("100")},
		{LineNo: 1, Ref: l2, Quantity: kernel.MustDecimal("20")}}, lines, nil, at)
	if err != nil || len(ms) != 2 || !ms[0].DeliveredQuantity.Equal(kernel.MustDecimal("100")) || ms[0].PONumber != "PO-1" ||
		ms[1].DeliveredQuantity.Sign() != 0 || ms[0].Status(Verified, Blue, kernel.Decimal{}) != Standing || ms[0].Status(Reversed, Blue, kernel.Decimal{}) != Review ||
		ms[0].Status(Verified, Blue, kernel.MustDecimal("1")) != Review || ms[0].Status(Verified, Red, kernel.Decimal{}) != Reversal {
		t.Fatalf("%+v %v", ms, err)
	}
	for _, c := range []struct {
		in       []MatchInput
		lines    map[LineRef]PurchaseLine
		standing map[LineRef]kernel.Decimal
		want     string
	}{
		{[]MatchInput{{LineNo: 1, Ref: LineRef{CommitmentID: "commitment.9", OrderLineID: "L1"}, Quantity: kernel.MustDecimal("1")}}, lines, nil,
			"REFERENCE_INVALID /args/matches/0/commitment_id"},
		{[]MatchInput{{LineNo: 1, Ref: l1, Quantity: kernel.MustDecimal("1")}},
			map[LineRef]PurchaseLine{l1: {Code: "INVALID_STATE", Status: "approved", SupplierID: "supplier.1", Unit: "PCS"}}, nil,
			"INVALID_STATE /args/matches/0/commitment_id"},
		{[]MatchInput{{LineNo: 1, Ref: l1, Quantity: kernel.MustDecimal("1")}},
			map[LineRef]PurchaseLine{l1: {SupplierID: "supplier.2", Unit: "PCS", PurchaseQuantity: kernel.MustDecimal("120")}}, nil,
			CodePartyEntityMismatch + " /args/matches/0/commitment_id"},
		{[]MatchInput{{LineNo: 1, Ref: l1, Quantity: kernel.MustDecimal("1")}},
			map[LineRef]PurchaseLine{l1: {SupplierID: "supplier.1", Unit: "KGM", PurchaseQuantity: kernel.MustDecimal("120")}}, nil,
			"INVALID_KNOWN_VALUE /args/matches/0/quantity"},
		{[]MatchInput{{LineNo: 1, Ref: l1, Quantity: kernel.MustDecimal("100")}, {LineNo: 1, Ref: l2, Quantity: kernel.MustDecimal("21")}}, lines, nil,
			"INVALID_KNOWN_VALUE /args/matches/1/quantity"},
		// S5R: the commitment line's cap across invoices answers PURCHASE_INVOICE_MATCH_EXCEEDED (catalog, R10 adopts it).
		{[]MatchInput{{LineNo: 1, Ref: l1, Quantity: kernel.MustDecimal("21")}}, lines, map[LineRef]kernel.Decimal{l1: kernel.MustDecimal("100")},
			CodeMatchExceeded + " /args/matches/0/quantity"},
	} {
		if _, err := CheckMatches(blue(), "pin.1", c.in, c.lines, c.standing, at); code(err) != c.want {
			t.Errorf("%s, want %s", code(err), c.want)
		}
	}
	if refs := Refs([]MatchInput{{Ref: l2}, {Ref: l1}, {Ref: l2}}); len(refs) != 2 || refs[0] != l1 || refs[1] != l2 {
		t.Fatalf("lock order %+v", refs)
	}
}

// CN-R06 and SM-CNX-INVOICE (verified → reversed): a red invoice reverses a verified blue invoice of
// the same supplier and buyer in the same currency, nothing else.
func TestRedInvoiceReversesOnlyAVerifiedBlueOneCNR06(t *testing.T) {
	red := blue()
	red.Colour, red.Number, red.RedOf = Red, "SYN0002", &Ref{Kind: "special_vat", Number: "SYN0001"}
	original := &Invoice{ID: "pin.1", Status: Verified, Candidate: blue()}
	v := blue()
	original.Verified = &v
	if err := CheckReversible(original, red, "/r"); err != nil {
		t.Fatal(err)
	}
	if got := code(CheckReversible(nil, red, "/r")); got != "REFERENCE_INVALID /r" {
		t.Fatal(got)
	}
	for _, c := range []struct {
		edit func(i *Invoice, red *Facts)
		want string
	}{
		{func(i *Invoice, _ *Facts) { i.Status = Reversed }, "INVALID_STATE /r"},
		{func(i *Invoice, _ *Facts) { i.Status, i.Verified = Imported, nil }, "INVALID_STATE /r"},
		{func(i *Invoice, _ *Facts) { i.Verified.Colour = Red }, "INVALID_STATE /r"},
		{func(_ *Invoice, r *Facts) { r.Supplier.TaxID = "SUPPLIER2" }, CodePartyEntityMismatch + " /r"},
		{func(_ *Invoice, r *Facts) { r.Buyer.TaxID = "AFFILIATE9" }, CodePartyEntityMismatch + " /r"},
		{func(_ *Invoice, r *Facts) { r.Currency.Code = "USD" }, "INVALID_KNOWN_VALUE /r"},
	} {
		o := &Invoice{ID: "pin.1", Status: Verified, Candidate: blue()}
		ov := blue()
		o.Verified = &ov
		r := red
		c.edit(o, &r)
		if got := code(CheckReversible(o, r, "/r")); got != c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
	i := &Invoice{Status: Imported, RowVersion: 1, Candidate: blue()}
	if CheckDecidable(i) != nil || code(CheckVersion(i, 2)) != "VERSION_CONFLICT /args/expected_version" || i.Key() != (Ref{Kind: "special_vat", Number: "SYN0001"}) {
		t.Fatal("an imported invoice is decidable at its version, keyed by its candidate")
	}
	i.Status, i.Verified = Verified, &red
	if code(CheckDecidable(i)) != "INVALID_STATE /args/purchase_invoice_id" || i.Key().Number != "SYN0002" || i.Facts().Colour != Red {
		t.Fatal("a verified invoice is decided once and keyed by its verified facts")
	}
}
