package orderrev

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Revision 1 is the snapshot order.prepare stored, read as it is: BRC-001's four lines with their
// cost-unit quantities and tax bindings, the calculation's amounts and payment plan, the source and
// the parties.
func TestRevisionOneIsTheOrdersSnapshot(t *testing.T) {
	doc, hash := snapshot(t, sealed(t))
	r, err := FromSnapshot(doc, hash)
	if err != nil {
		t.Fatal(err)
	}
	if r.Number != 1 || r.Hash != hash || !bytes.Equal(r.Document, doc) || r.Contract != SnapshotContract || r.OrderID != "order.1" ||
		r.Previous != nil || r.Amendment != nil || r.Scope != (Scope{Tenant: "tenant.1", Company: "company.live"}) {
		t.Fatalf("identity: %+v", r)
	}
	if r.Link() != (Link{Revision: 1, Hash: hash}) || r.Source.QuoteID != "quote.1" || r.Source.Revision != 1 ||
		r.Source.Number != "QT-2026-00001" || r.Source.AcceptanceID != "acceptance.1" || r.Source.DocumentSHA256 != documentSHA ||
		r.Customer.PartyID != "customer.1" || r.Contact.ContactID != "contact.1" || r.Currency != usd || r.Terms.TradeTerms != trade ||
		r.Terms.PaymentTerms != payment || r.Terms.Delivery.Date != "2027-01-31" || r.Parties.Issuer.LegalName == "" ||
		len(r.Parties.Customer.Addresses) != 1 {
		t.Fatalf("source, parties or terms: %+v", r)
	}
	var got []string
	for _, l := range r.Lines {
		a, _ := r.LineAmounts(l.LineID)
		got = append(got, strings.Join([]string{l.LineID, l.RfqLineID, l.Sku.SkuID, l.Quantity, l.Unit.ID, l.NormalizedQuantity, l.CostUnit,
			l.UnitPrice, l.PriceBasisQuantity, l.DiscountRatio, l.Tax.Application.ID, l.Tax.Profile.ID, l.Tax.PriceBasis, l.Tax.Rate, a.NetMinor,
			a.PayableMinor}, " "))
	}
	want := []string{
		"L1 L1 ST01.red.S 24 PCS 24 PCS 8 1 0 tax.sale tax.zero exclusive 0 19200 19200",
		"L2 L2 ST01.red.M 36 PCS 36 PCS 8 1 0 tax.sale tax.zero exclusive 0 28800 28800",
		"L3 L3 ST01.blue.S 12 PCS 12 PCS 8 1 0 tax.sale tax.zero exclusive 0 9600 9600",
		"L4 L4 ST01.blue.M 48 PCS 48 PCS 8 1 0 tax.sale tax.zero exclusive 0 38400 38400",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("lines\n got %q\nwant %q", got, want)
	}
	pay := r.Amounts.Payments
	if r.Amounts.Net != "96000" || r.Amounts.Tax != "0" || r.Amounts.Payable != "96000" || len(pay) != 2 ||
		pay[0].StageID != "P1" || pay[0].AmountMinor != "28800" || pay[0].Trigger != "order_confirmed" ||
		pay[1].StageID != "P2" || pay[1].AmountMinor != "67200" || pay[1].Days != 30 || pay[1].Percent != "70" {
		t.Fatalf("amounts: %+v", r.Amounts)
	}
}

// A stored snapshot that is not order.prepare's is refused, never repaired.
func TestOnlyAnOrdersSnapshotIsRevisionOne(t *testing.T) {
	doc, hash := snapshot(t, sealed(t))
	if _, err := FromSnapshot(doc, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := FromSnapshot(doc, strings.Repeat("0", 64)); code(err) != "REFERENCE_INVALID at /snapshot" {
		t.Errorf("other hash: %s", code(err))
	}
	edit := func(f func(m map[string]any)) ([]byte, string) {
		tree, _ := quote.Tree(doc)
		f(tree.(map[string]any))
		b, err := quote.Canonical(tree)
		if err != nil {
			t.Fatal(err)
		}
		return b, quote.Hash(b)
	}
	for name, c := range map[string]struct {
		edit func(m map[string]any)
		want string
	}{
		"another contract": {func(m map[string]any) { m["snapshot_contract"] = RevisionContract }, "REFERENCE_INVALID at /snapshot/snapshot_contract"},
		"another slot":     {func(m map[string]any) { m["conversion_slot"] = "partial" }, "REFERENCE_INVALID at /snapshot/snapshot_contract"},
		"no calculation":   {func(m map[string]any) { m["calculation"] = nil }, "REFERENCE_INVALID at /snapshot/calculation"},
		"a line without its tax": {func(m map[string]any) {
			c := m["calculation"].(map[string]any)
			terms := c["sales_terms_binding"].(map[string]any)
			terms["tax_bindings"] = terms["tax_bindings"].([]any)[1:]
		}, "REFERENCE_INVALID at /snapshot/lines/0"},
		"no lines": {func(m map[string]any) { m["lines"] = []any{} }, "REFERENCE_INVALID at /snapshot/lines"},
	} {
		b, h := edit(c.edit)
		if _, err := FromSnapshot(b, h); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
}

// TD-AT01 and TRD-INV-018: BRC-001's order has shipped 40 of blue M (L4, 48). An amendment to 36 is
// QUANTITY_BELOW_SHIPPED; to 44 it yields order revision 2: L4 at 44 PCS (352.00), 928.00 in all,
// paid 278.40 + 649.60, naming revision 1's hash and the amendment, hashed as its canonical
// document (golden vector testdata/brc001-r2.json). Parse reads it back.
func TestTDAT01AmendmentKeepsTheShippedQuantity(t *testing.T) {
	r1 := revisionOne(t)
	f := shipped("L4", "40")
	p := ProposalOf(r1)
	p.Lines[3].Quantity = "36"
	if _, _, err := Next(r1, amendment1, p, f, facts(), "/args/proposal"); code(err) != "QUANTITY_BELOW_SHIPPED at /args/proposal/lines/3/quantity" {
		t.Fatalf("36 after 40 shipped: %s", code(err))
	}
	p.Lines[3].Quantity = "44"
	r2, d, err := Next(r1, amendment1, p, f, facts(), "/args/proposal")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := r2.LineAmounts("L4")
	if r2.Number != 2 || *r2.Previous != r1.Link() || *r2.Amendment != amendment1 || r2.Contract != RevisionContract ||
		r2.Line("L4").Quantity != "44" || r2.Line("L4").NormalizedQuantity != "44" || a.NetMinor != "35200" || r2.Amounts.Net != "92800" ||
		r2.Amounts.Payable != "92800" || r2.Amounts.Payments[0].AmountMinor != "27840" || r2.Amounts.Payments[1].AmountMinor != "64960" ||
		r2.Hash != quote.Hash(r2.Document) || r2.Source != r1.Source || r2.Line("L1").RfqLineID != "L1" {
		t.Fatalf("r2: %+v", r2)
	}
	if len(d.Lines) != 1 || d.Lines[0].LineID != "L4" || d.Lines[0].Change != Changed || !slices.Equal(d.Lines[0].Fields, []string{"quantity"}) ||
		d.Lines[0].From != "48" || d.Lines[0].To != "44" || d.Lines[0].Unit != "PCS" ||
		len(d.Charges) != 0 || len(d.Terms) != 0 || d.From.Net != "96000" || d.To.Net != "92800" || d.Empty() {
		t.Fatalf("diff: %+v", d)
	}
	golden(t, "brc001-r2.json", r2.Document)
	if r2.Hash != brc001R2Hash {
		t.Fatalf("r2 hash %s, want %s", r2.Hash, brc001R2Hash)
	}
	back, err := Parse(r2.Document, r2.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if back.Hash != r2.Hash || !bytes.Equal(back.Document, r2.Document) || back.Line("L4").Quantity != "44" || *back.Previous != r1.Link() {
		t.Fatalf("parsed: %+v", back)
	}
	// The chain goes on from a parsed revision: revision 3 names revision 2.
	p3 := ProposalOf(back)
	p3.Lines[3].Quantity = "40"
	r3, _, err := Next(back, AmendmentRef{ID: "amendment.2", Hash: hashOf("amendment.2")}, p3, f, facts(), "/args/proposal")
	if err != nil || r3.Number != 3 || *r3.Previous != r2.Link() || r3.Amounts.Net != "89600" {
		t.Fatalf("r3: %v %+v", err, r3)
	}
	// Restating the revision changes nothing.
	if _, d, err := Next(r1, amendment1, ProposalOf(r1), f, facts(), "/args/proposal"); err != nil || !d.Empty() {
		t.Fatalf("unchanged: %v %+v", err, d)
	}
}

// What an amendment may and may not change of the lines, with the facts each change needs.
func TestWhatAnAmendmentChangesOfTheLines(t *testing.T) {
	r1 := revisionOne(t)
	f := shipped("L4", "40", "L1", "0")
	newTax := quote.Ref{Kind: quote.KindTaxApplication, ID: "tax.sale.navy", Revision: 1}
	navy := quote.SkuRef{SkuID: "ST01.navy.M", SpecRevision: 1}
	withNavy := func() Facts {
		x := facts()
		x.Measures = map[Measure]Measurement{{Sku: navy, Unit: "PCS"}: {CostUnit: "PCS"}}
		x.Taxes = map[quote.Ref]*calc.TaxApplication{newTax: {Ref: quote.CalcRef(newTax), Side: calc.SideSales, SubjectKind: calc.SubjectSku,
			SubjectID: navy.SkuID, Profile: zeroTax}}
		return x
	}
	for name, c := range map[string]struct {
		edit  func(p *Proposal)
		facts func() Facts
		want  string
	}{
		"removing a shipped line":          {func(p *Proposal) { p.Lines = p.Lines[:3] }, facts, "QUANTITY_BELOW_SHIPPED at /args/proposal/lines"},
		"repricing a shipped line":         {func(p *Proposal) { p.Lines[3].UnitPrice = "7.5" }, facts, "INVALID_STATE at /args/proposal/lines/3"},
		"another SKU on a shipped line":    {func(p *Proposal) { p.Lines[3].Sku = navy }, withNavy, "INVALID_STATE at /args/proposal/lines/3"},
		"another unit":                     {func(p *Proposal) { p.Lines[0].Unit = dozen }, facts, "INVALID_KNOWN_VALUE at /args/proposal/lines/0/unit_ref"},
		"a fraction of a piece":            {func(p *Proposal) { p.Lines[0].Quantity = "23.5" }, facts, "INVALID_KNOWN_VALUE at /args/proposal/lines/0/quantity"},
		"a zero quantity":                  {func(p *Proposal) { p.Lines[0].Quantity = "0" }, facts, "INVALID_KNOWN_VALUE at /args/proposal/lines/0/quantity"},
		"a float-like quantity":            {func(p *Proposal) { p.Lines[0].Quantity = "1e3" }, facts, "SCHEMA_INVALID at /args/proposal/lines/0/quantity"},
		"a line twice":                     {func(p *Proposal) { p.Lines[1].LineID = "L1" }, facts, "DUPLICATE_LINE_ID at /args/proposal/lines/1/line_id"},
		"no line":                          {func(p *Proposal) { p.Lines = nil }, facts, "INVALID_KNOWN_VALUE at /args/proposal/lines"},
		"another SKU without its measures": {func(p *Proposal) { p.Lines[0].Sku = navy }, facts, "REFERENCE_INVALID at /args/proposal/lines/0/sku_ref"},
		"another SKU without its tax": {func(p *Proposal) { p.Lines[0].Sku = navy }, func() Facts {
			x := withNavy()
			x.Taxes = nil
			return x
		}, "REFERENCE_INVALID at /args/proposal/lines/0/tax_application_ref"},
		"another SKU under the old SKU's tax": {func(p *Proposal) { p.Lines[0].Sku = navy }, func() Facts {
			x := withNavy()
			x.Taxes[sale] = &calc.TaxApplication{Ref: quote.CalcRef(sale), Side: calc.SideSales, SubjectKind: calc.SubjectSku,
				SubjectID: "ST01.red.S", Profile: zeroTax}
			return x
		}, "TAX_POLICY_UNCONFIRMED at /args/proposal/lines/0/tax_application_ref"},
		"another SKU with its tax": {func(p *Proposal) { p.Lines[0].Sku, p.Lines[0].TaxApplication = navy, newTax }, withNavy, "OK"},
		"an added line": {func(p *Proposal) {
			p.Lines = append(p.Lines, ProposedLine{LineID: "L5", Sku: navy, Quantity: "10", Unit: pcs, UnitPrice: "8.2", PriceBasisQuantity: "1",
				DiscountRatio: "0", TaxApplication: newTax})
		}, withNavy, "OK"},
		"an added line in dozens without a conversion": {func(p *Proposal) {
			p.Lines = append(p.Lines, ProposedLine{LineID: "L5", Sku: navy, Quantity: "1", Unit: dozen, UnitPrice: "96", PriceBasisQuantity: "1",
				DiscountRatio: "0", TaxApplication: newTax})
		}, func() Facts {
			x := withNavy()
			x.Measures[Measure{Sku: navy, Unit: "DOZ"}] = Measurement{CostUnit: "PCS"}
			return x
		}, "DEPENDENCY_NOT_SUPPORTED at /args/proposal/lines/4/unit_ref"},
		"a conversion on a line in its cost unit": {func(p *Proposal) {
			c := quote.Ref{Kind: quote.KindUnitConversion, ID: "x", Revision: 1}
			p.Lines[0].Conversion = &c
		}, facts, "INVALID_KNOWN_VALUE at /args/proposal/lines/0/conversion_ref"},
		"an added line naming another conversion than its measures": {func(p *Proposal) {
			c := quote.Ref{Kind: quote.KindUnitConversion, ID: "x", Revision: 1}
			p.Lines = append(p.Lines, ProposedLine{LineID: "L5", Sku: navy, Quantity: "10", Unit: pcs, Conversion: &c, UnitPrice: "8",
				PriceBasisQuantity: "1", DiscountRatio: "0", TaxApplication: newTax})
		}, withNavy, "REFERENCE_INVALID at /args/proposal/lines/4/conversion_ref"},
		"a payment terms change without its stages": {func(p *Proposal) {
			p.Terms.PaymentTerms = quote.Ref{Kind: quote.KindPaymentTerms, ID: "payment.100", Revision: 1}
		}, facts, "REFERENCE_INVALID at /args/proposal/terms/payment_terms_ref"},
		"a delivery without a date": {func(p *Proposal) { p.Terms.Delivery.Date = "" }, facts, "INVALID_KNOWN_VALUE at /args/proposal/terms/delivery"},
		"trade terms of another kind": {func(p *Proposal) { p.Terms.TradeTerms = payment }, facts,
			"SCHEMA_INVALID at /args/proposal/terms/trade_terms_ref"},
		"a currency fact of another currency": {func(*Proposal) {}, func() Facts {
			x := facts()
			x.Currency = kernel.MustCurrency("EUR", 2)
			return x
		}, "REFERENCE_INVALID at /facts/currency"},
		"no precision of the unit": {func(p *Proposal) { p.Lines[0].Quantity = "30" }, func() Facts {
			x := facts()
			delete(x.Units, "PCS")
			return x
		}, "REFERENCE_INVALID at /args/proposal/lines/0/unit_ref"},
	} {
		p := ProposalOf(r1)
		c.edit(&p)
		if _, _, err := Next(r1, amendment1, p, f, c.facts(), "/args/proposal"); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
	// Shipments FUL reports of a line the order never had are refused.
	if _, _, err := Next(r1, amendment1, ProposalOf(r1), shipped("L9", "1"), facts(), "/args/proposal"); code(err) != "REFERENCE_INVALID at /fulfilment/shipped/L9" {
		t.Errorf("shipments of L9: %s", code(err))
	}
}

// SIM-02 step 2's kind of change on an order: blue S (L3, unshipped) becomes navy S at 8.40, a
// dozen of navy M is added at 96.00 a dozen through POL's conversion, delivery moves and the
// payment terms become one stage of 100 % before shipment. Every amount is recalculated; the diff
// says what changed.
func TestAmendmentRecalculatesEveryAmount(t *testing.T) {
	r1 := revisionOne(t)
	navyS := quote.SkuRef{SkuID: "ST01.navy.S", SpecRevision: 1}
	navyM := quote.SkuRef{SkuID: "ST01.navy.M", SpecRevision: 2}
	navyTax := quote.Ref{Kind: quote.KindTaxApplication, ID: "tax.sale.navy", Revision: 1}
	conv := quote.Ref{Kind: quote.KindUnitConversion, ID: "navy.M.doz", Revision: 1}
	upfront := quote.Ref{Kind: quote.KindPaymentTerms, ID: "payment.100", Revision: 1}
	x := facts()
	x.Measures = map[Measure]Measurement{{Sku: navyS, Unit: "PCS"}: {CostUnit: "PCS"},
		{Sku: navyM, Unit: "DOZ"}: {CostUnit: "PCS", Conversion: &calc.Conversion{Ref: quote.CalcRef(conv), SkuID: navyM.SkuID, SpecRevision: 2,
			From: "DOZ", To: "PCS", Factor: kernel.MustDecimal("12")}}}
	x.Taxes = map[quote.Ref]*calc.TaxApplication{navyTax: {Ref: quote.CalcRef(navyTax), Side: calc.SideSales, SubjectKind: calc.SubjectSku,
		SubjectID: navyS.SkuID, Profile: zeroTax}}
	x.Stages = []Stage{{StageID: "ALL", Percent: "100", Trigger: "before_shipment"}}
	p := ProposalOf(r1)
	p.Lines[2] = ProposedLine{LineID: "L3", Sku: navyS, Quantity: "12", Unit: pcs, UnitPrice: "8.40", PriceBasisQuantity: "1", DiscountRatio: "0",
		TaxApplication: navyTax}
	p.Lines = append(p.Lines, ProposedLine{LineID: "L5", Sku: navyM, Quantity: "1", Unit: dozen, Conversion: &conv, UnitPrice: "96.00",
		PriceBasisQuantity: "1", DiscountRatio: "0.05", TaxApplication: sale})
	x.Taxes[sale] = &calc.TaxApplication{Ref: quote.CalcRef(sale), Side: calc.SideSales, SubjectKind: calc.SubjectSku, SubjectID: navyM.SkuID,
		Profile: zeroTax}
	p.Terms.PaymentTerms = upfront
	p.Terms.Delivery.Date = "2027-02-15"
	r2, d, err := Next(r1, amendment1, p, Fulfilment{Source: NotWired}, x, "/args/proposal")
	if err != nil {
		t.Fatal(err)
	}
	l3, l5 := r2.Line("L3"), r2.Line("L5")
	a3, _ := r2.LineAmounts("L3")
	a5, _ := r2.LineAmounts("L5")
	if l3.Sku != navyS || l3.RfqLineID != "L3" || l3.Tax.Application != navyTax || a3.NetMinor != "10080" ||
		l5.RfqLineID != "" || l5.NormalizedQuantity != "12" || l5.CostUnit != "PCS" || *l5.Conversion != conv || a5.NetMinor != "9120" ||
		r2.Amounts.Net != "105600" || len(r2.Amounts.Payments) != 1 || r2.Amounts.Payments[0].AmountMinor != "105600" ||
		r2.Amounts.Payments[0].Trigger != "before_shipment" {
		t.Fatalf("r2: %+v %+v %+v %+v", *l3, *l5, r2.Amounts, a3)
	}
	var changes []string
	for _, c := range d.Lines {
		changes = append(changes, c.LineID+" "+c.Change+" "+strings.Join(c.Fields, ",")+" "+c.From+">"+c.To+" "+c.Unit)
	}
	if !slices.Equal(changes, []string{"L3 changed sku_ref,unit_price,tax_application_ref 12>12 PCS", "L5 added  >1 DOZ"}) ||
		!slices.Equal(d.Terms, []string{"payment_terms_ref", "delivery"}) {
		t.Fatalf("diff: %q %q", changes, d.Terms)
	}
}

// Charges: an amount changes or a charge goes; a new one is not supported yet, and an included
// charge follows its line.
func TestWhatAnAmendmentChangesOfTheCharges(t *testing.T) {
	r1 := revisionOne(t,
		quote.Charge{ChargeID: "C1", FeeCategory: freight, ObligationKey: "freight.main", Amount: "60.00", Inclusion: quote.InclusionSeparate,
			TaxApplication: chargeTx, Evidence: proof},
		quote.Charge{ChargeID: "C2", FeeCategory: packing, ObligationKey: "packing.L2", Amount: "5.00", Inclusion: quote.InclusionIncludedInLine,
			IncludedLineID: "L2", TaxApplication: chargeTx, Evidence: proof})
	if r1.Amounts.Net != "102000" || len(r1.Charges) != 2 {
		t.Fatalf("r1 with charges: %+v", r1.Amounts)
	}
	p := ProposalOf(r1)
	p.Charges[0].Amount = "75.5"
	r2, d, err := Next(r1, amendment1, p, Fulfilment{}, facts(), "/args/proposal")
	if err != nil || r2.Amounts.Net != "103550" || len(d.Charges) != 1 || d.Charges[0] != (ChargeChange{ChargeID: "C1", Change: Changed, From: "60",
		To: "75.5"}) {
		t.Fatalf("freight 75.50: %v %+v %+v", err, r2, d)
	}
	p = ProposalOf(r1)
	p.Charges = p.Charges[1:]
	if r2, d, err = Next(r1, amendment1, p, Fulfilment{}, facts(), "/args/proposal"); err != nil || r2.Amounts.Net != "96000" ||
		d.Charges[0].Change != Removed {
		t.Fatalf("freight removed: %v %+v", err, d)
	}
	for name, c := range map[string]struct {
		edit func(p *Proposal)
		want string
	}{
		"a new charge":          {func(p *Proposal) { p.Charges = append(p.Charges, ProposedCharge{ChargeID: "C3", Amount: "1"}) }, "DEPENDENCY_NOT_SUPPORTED at /args/proposal/charges/2/charge_id"},
		"a charge twice":        {func(p *Proposal) { p.Charges[1].ChargeID = "C1" }, "INVALID_KNOWN_VALUE at /args/proposal/charges/1/charge_id"},
		"a zero charge":         {func(p *Proposal) { p.Charges[0].Amount = "0" }, "INVALID_KNOWN_VALUE at /args/proposal/charges/0/amount"},
		"its line removed":      {func(p *Proposal) { p.Lines = append(p.Lines[:1], p.Lines[2:]...) }, "REFERENCE_INVALID at /args/proposal/charges/1"},
		"an amount of 9 digits": {func(p *Proposal) { p.Charges[0].Amount = "1.123456789" }, "SCHEMA_INVALID at /args/proposal/charges/0/amount"},
	} {
		p := ProposalOf(r1)
		c.edit(&p)
		if _, _, err := Next(r1, amendment1, p, Fulfilment{}, facts(), "/args/proposal"); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
}

// The amounts of a revision are the quote calculation's own: an inclusive 13 % tax splits 113.00
// into 100.00 + 13.00, as calc.PriceLine does for the same line.
func TestRecalculationIsTheQuoteCalculations(t *testing.T) {
	r1 := revisionOne(t)
	vat := quote.Ref{Kind: quote.KindTaxApplication, ID: "tax.vat13", Revision: 1}
	vat13 := calc.TaxProfile{Ref: calc.Ref{Kind: quote.KindTaxProfile, ID: "vat13", Revision: 1}, Rate: kernel.MustDecimal("0.13"),
		PriceBasis: calc.TaxInclusive, Recovery: calc.Nonrecoverable}
	app := &calc.TaxApplication{Ref: quote.CalcRef(vat), Side: calc.SideSales, SubjectKind: calc.SubjectSku, SubjectID: "ST01.red.S", Profile: vat13}
	x := facts()
	x.Taxes = map[quote.Ref]*calc.TaxApplication{vat: app}
	p := ProposalOf(r1)
	p.Lines[0].TaxApplication, p.Lines[0].Quantity, p.Lines[0].UnitPrice, p.Lines[0].PriceBasisQuantity = vat, "113", "1", "1"
	r2, _, err := Next(r1, amendment1, p, Fulfilment{}, x, "/args/proposal")
	if err != nil {
		t.Fatal(err)
	}
	want, err := calc.PriceLine(dollars, calc.Line{Path: "/l", LineID: "L1", Quantity: kernel.MustDecimal("113"), UnitPrice: kernel.MustDecimal("1"),
		PriceBasisQuantity: kernel.MustDecimal("1"), Tax: app})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := r2.LineAmounts("L1")
	if a.NetMinor != want.Net.MinorString() || a.TaxMinor != want.Tax.MinorString() || a.PayableMinor != want.Payable.MinorString() ||
		a.NetMinor != "10000" || a.TaxMinor != "1300" || r2.Line("L1").Tax.PriceBasis != "inclusive" || r2.Line("L1").Tax.Rate != "0.13" {
		t.Fatalf("inclusive 13 %%: %+v, calc %v %v %v", a, want.Net, want.Tax, want.Payable)
	}
}

// A later revision is read back only in its canonical form at its hash.
func TestParseRefusesWhatNextDidNotWrite(t *testing.T) {
	r1 := revisionOne(t)
	p := ProposalOf(r1)
	p.Lines[3].Quantity = "44"
	r2, _, err := Next(r1, amendment1, p, Fulfilment{}, facts(), "/args/proposal")
	if err != nil {
		t.Fatal(err)
	}
	spaced := append([]byte(" "), r2.Document...)
	tree, _ := quote.Tree(r2.Document)
	tree.(map[string]any)["order_revision"] = 1
	first, _ := quote.Canonical(tree)
	tree.(map[string]any)["order_revision"] = 2
	tree.(map[string]any)["extra"] = "x"
	extra, _ := quote.Canonical(tree)
	for name, doc := range map[string][]byte{"not canonical": spaced, "revision 1": first, "an unknown member": extra} {
		if _, err := Parse(doc, quote.Hash(doc)); code(err) != "REFERENCE_INVALID at /revision" {
			t.Errorf("%s: %s", name, code(err))
		}
	}
	if _, err := Parse(r2.Document, r1.Hash); code(err) != "REFERENCE_INVALID at /revision" {
		t.Errorf("another hash: %s", code(err))
	}
}

// brc001R2Hash is the order hash of testdata/brc001-r2.json; Python's json.dumps(sort_keys=True,
// separators=(",", ":"), ensure_ascii=False) of the parsed file reproduces its bytes, and decimal
// arithmetic its line amounts and largest-remainder payment plan (scratchpad check, run with
// python3 -I).
const brc001R2Hash = "b76b4742d8a2b3f80518a4a7241e25ef80485995069a70fbb01a28d564116cbc"

// golden compares got with testdata/name, or writes it when ORDERREV_WRITE_VECTORS=1.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("ORDERREV_WRITE_VECTORS") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the golden vector:\n got %s\nwant %s", name, got, want)
	}
}

// WBS S3-17: the legal terms the source quote sealed reach the order revision from the snapshot's
// commercial document — never from POL — and every later revision carries them unchanged; terms
// without their revision, or the reverse, are not a snapshot order.prepare wrote.
func TestLegalTermsComeFromTheSourceQuote(t *testing.T) {
	doc, _ := snapshot(t, sealed(t))
	legal := map[string]any{"source": "issuer_default", "governing_law": "CN", "cisg": "applies",
		"dispute_resolution": map[string]any{"method": "arbitration", "institution": "CIETAC", "seat": "Shanghai", "language": "en"},
		"formation":          map[string]any{"rule": "arrival", "basis": "cisg"}}
	ref := map[string]any{"kind": quote.KindLegalTerms, "id": "legal.default", "revision": 1}
	with := func(terms, termsRef any) ([]byte, string) {
		tree, _ := quote.Tree(doc)
		src := tree.(map[string]any)["source_quote_snapshot"].(map[string]any)
		if terms != nil {
			src["legal_terms"] = terms
		}
		if termsRef != nil {
			src["data"].(map[string]any)["legal_terms_ref"] = termsRef
		}
		b, err := quote.Canonical(tree)
		if err != nil {
			t.Fatal(err)
		}
		return b, quote.Hash(b)
	}
	b, h := with(legal, ref)
	r1, err := FromSnapshot(b, h)
	if err != nil {
		t.Fatal(err)
	}
	if r1.LegalTermsRef == nil || *r1.LegalTermsRef != (quote.Ref{Kind: quote.KindLegalTerms, ID: "legal.default", Revision: 1}) ||
		r1.LegalTerms == nil || r1.LegalTerms.GoverningLaw != "CN" || r1.LegalTerms.DisputeResolution.Seat != "Shanghai" ||
		r1.LegalTerms.Formation.Basis != quote.FormationBasisCISG {
		t.Fatalf("revision 1's legal terms: %+v %+v", r1.LegalTermsRef, r1.LegalTerms)
	}
	p := ProposalOf(r1)
	p.Lines[3].Quantity = "44"
	r2, _, err := Next(r1, amendment1, p, Fulfilment{}, facts(), "/args")
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(r2.Document, r2.Hash)
	if err != nil || *back.LegalTerms != *r1.LegalTerms || *back.LegalTermsRef != *r1.LegalTermsRef ||
		!strings.Contains(string(r2.Document), `"legal_terms":{"cisg":"applies"`) {
		t.Fatalf("revision 2's legal terms: %v %s", err, r2.Document)
	}
	for name, b := range map[string][2]any{"terms without their revision": {legal, nil}, "a revision without terms": {nil, ref}} {
		doc, hash := with(b[0], b[1])
		if _, err := FromSnapshot(doc, hash); code(err) != "REFERENCE_INVALID at /snapshot/source_quote_snapshot/legal_terms" {
			t.Errorf("%s: %s", name, code(err))
		}
	}
}
