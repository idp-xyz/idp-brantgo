package orderrev

import (
	"fmt"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The fixture is BRC-001 as an order (S1-08's RFQ, kernel parity_test's arithmetic): quote.1
// revision 1, sealed by quote.SealDraft with the server calculation, quotes four lines of ST01 —
// L1 red S 24, L2 red M 36, L3 blue S 12, L4 blue M 48, 120 PCS in all — at 8.00 USD a piece
// without tax, 960.00 USD, on the 660.00 USD cost snapshot, delivered fixed at DEHAM, paid 30 % on
// order confirmation and 70 % 30 days after shipment (288.00 + 672.00, TRD-INV-022's example). The
// order's snapshot is built from it as package order's BuildSnapshot does (snapshot below), on
// acceptance.1, which arrived before the quote expired.

var (
	issuedAt  = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	expiresAt = time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	arrivedAt = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
)

const (
	resultHash   = "2a809a7b878bb2bedba3eed7534ecd55d43952b41bad144babe3c2d289cc677f"
	evidenceHash = "3668d7dc21b908c3ef5b25ffab99b94193b51c785d0f66a41950fed5107d221b"
	linesHash    = "3ee92f1118c480ab32896c9bbb35186fa05687b9e44c43f0e1544527267c868f"
	sourceHash   = "0397250103796959c07c27f59f03da066db3e42b27d81da397a18d2f2b936a61"
	profileHash  = "9572be119f04ad3072f7a8f98a14a6946efeec50764cb45d43bce99deafe8090"
)

// documentSHA is the SHA-256 of quote.1 r1's formal document, as the order binds it.
var documentSHA = hashOf("QT-2026-00001.txt")

var (
	company  = quote.Ref{Kind: quote.KindCompanyProfile, ID: "company.live", Revision: 1}
	usd      = quote.Ref{Kind: quote.KindCurrency, ID: "USD", Revision: 1}
	pcs      = quote.Ref{Kind: quote.KindUnit, ID: "PCS", Revision: 1}
	dozen    = quote.Ref{Kind: quote.KindUnit, ID: "DOZ", Revision: 1}
	sale     = quote.Ref{Kind: quote.KindTaxApplication, ID: "tax.sale", Revision: 1}
	zero     = quote.Ref{Kind: quote.KindTaxProfile, ID: "tax.zero", Revision: 1}
	policy   = quote.Ref{Kind: quote.KindPricingPolicy, ID: "policy.1", Revision: 1}
	trade    = quote.Ref{Kind: quote.KindTradeTerms, ID: "terms.fca", Revision: 1}
	payment  = quote.Ref{Kind: quote.KindPaymentTerms, ID: "payment.30-70", Revision: 1}
	template = quote.Ref{Kind: quote.KindQuoteDocumentTemplate, ID: "quote.template", Revision: 1}
	sequence = quote.Ref{Kind: quote.KindNumberSequence, ID: "quote.sequence", Revision: 1}
	freight  = quote.Ref{Kind: quote.KindFeeCategory, ID: "freight.sale", Revision: 1}
	packing  = quote.Ref{Kind: quote.KindFeeCategory, ID: "packing.sale", Revision: 1}
	chargeTx = quote.Ref{Kind: quote.KindTaxApplication, ID: "tax.charge", Revision: 1}
	proof    = []quote.Evidence{{SourceID: "source.1", Locator: "合成核实依据"}}
	hq       = quote.AddressRef{PartyID: "customer.1", PartyRevision: 2, AddressID: "hq"}
	deham    = quote.BusinessRef{Kind: "location", ID: "DEHAM", Revision: 1}
	handover = quote.Delivery{Kind: "fixed", Date: "2027-01-31", Milestone: "handover", Place: deham, TimeZone: "Asia/Shanghai"}
	dollars  = kernel.MustCurrency("USD", 2)
	scope    = kernel.Scope{Tenant: "tenant.1", Company: "company.live"}

	// BRC-001's lines: ID, SKU, quantity.
	brc001 = []struct{ id, sku, qty string }{{"L1", "ST01.red.S", "24"}, {"L2", "ST01.red.M", "36"}, {"L3", "ST01.blue.S", "12"},
		{"L4", "ST01.blue.M", "48"}}
)

func hashOf(s string) string { return quote.Hash([]byte(s)) }

func sku(id string) quote.SkuRef { return quote.SkuRef{SkuID: id, SpecRevision: 1} }

// draft is BRC-001's quote data, with the charges given.
func draft(charges ...quote.Charge) quote.Data {
	place, delivery := deham, handover
	notes := "合成样例"
	d := quote.Data{Issuer: &company, Customer: &quote.PartyRef{PartyID: "customer.1", Revision: 2},
		Contact: &quote.ContactRef{ContactID: "contact.1", RowVersion: 2}, Profile: &quote.ProfileRef{PartyID: "customer.1", Revision: 1},
		Addresses: []quote.AddressRef{hq}, Rfq: &quote.RfqRef{RfqID: "rfq.1", RowVersion: 2},
		Snapshot: &quote.SnapshotRef{SnapshotID: "snapshot.1", ResultHash: resultHash},
		Currency: &usd, PricingPolicy: &policy, TradeTerms: &trade, NamedPlace: &place, PaymentTerms: &payment, Delivery: &delivery,
		DocumentTemplate: &template, Sequence: &sequence, ExpiresAt: kernel.FormatTimestamp(expiresAt), CustomerNotes: &notes, Evidence: proof,
		Charges: charges}
	for _, l := range brc001 {
		d.Lines = append(d.Lines, quote.Line{LineID: l.id, RfqLineID: l.id, Sku: sku(l.sku), Quantity: l.qty + ".000000", Unit: pcs,
			UnitPrice: "8.00", PriceBasisQuantity: "1", DiscountRatio: "0", TaxApplication: sale, Evidence: proof})
	}
	return d
}

func freezeFacts() quote.Facts {
	specs := map[quote.SkuRef]quote.Verdict{}
	for _, l := range brc001 {
		specs[sku(l.sku)] = nil
	}
	return quote.Facts{Company: "company.live", Party: &quote.Party{Confirmed: true, Revision: 2, Customer: true},
		Contact: &quote.Contact{PartyID: "customer.1", RowVersion: 2, Confirmed: true},
		Profile: &quote.Profile{ContentHash: profileHash, PartyRevision: 2, PaymentTerms: payment, TradeTerms: trade,
			Contacts: []quote.ContactRef{{ContactID: "contact.1", RowVersion: 2}}},
		Addresses: map[quote.AddressRef]quote.Address{hq: {Role: "buyer", Address: "Synthetic buyer address\nHamburg"}},
		Rfq:       &quote.Rfq{RowVersion: 2, Ready: true, LinesHash: linesHash},
		Specs:     specs,
		Configs: map[quote.Ref]quote.Config{
			company: {ContentHash: hashOf("company"), Company: &quote.IssuerSnapshot{LegalName: "SYNTHETIC Exporter — no actual trade",
				RegistrationNo: "SYNTHETIC-NOT-REAL", Country: "CN", Address: "Synthetic Shanghai address"}},
			usd:      {ContentHash: hashOf("usd")},
			pcs:      {ContentHash: hashOf("pcs")},
			sale:     {ContentHash: hashOf("sale"), Nested: []quote.Ref{zero}},
			chargeTx: {ContentHash: hashOf("charge"), Nested: []quote.Ref{zero}},
			zero:     {ContentHash: hashOf("zero")},
			policy:   {ContentHash: hashOf("policy"), Nested: []quote.Ref{zero}, Policy: &quote.Policy{QuoteValidDays: 7}},
			trade:    {ContentHash: hashOf("trade")},
			payment:  {ContentHash: hashOf("payment")},
			template: {ContentHash: hashOf("template"), Renderer: quote.QuoteRendererV010},
			sequence: {ContentHash: hashOf("sequence"), SequenceType: quote.DocumentTypeQuote},
			freight:  {ContentHash: hashOf("freight")},
			packing:  {ContentHash: hashOf("packing")},
		},
		Adopted:  []quote.Ref{usd, pcs},
		Snapshot: &quote.Snapshot{RfqLinesHash: linesHash, EvidenceHash: evidenceHash},
		Sources:  map[string]string{"source.1": sourceHash},
	}
}

var zeroTax = calc.TaxProfile{Ref: quote.CalcRef(zero), Rate: kernel.MustDecimal("0"), PriceBasis: calc.TaxExclusive, Recovery: calc.Nonrecoverable}

// calculator is the server calculation of draft d as application.ServerCalculator resolves it on
// the fixture: USD with 2 minor digits, PCS without, the zero tax profile, the RFQ's four lines,
// the 660.00 USD snapshot of an actual costing of them, policy.1 at a minimum margin of 0.1, and
// payment.30-70's two stages.
func calculator(d quote.Data) quote.Calculate {
	return func() (*quote.Calculation, error) {
		q := calc.Quote{Path: "/args/data", Rfq: calc.RfqRef{ID: d.Rfq.RfqID, RowVersion: d.Rfq.RowVersion}, Customer: d.Customer.PartyID,
			Contact: d.Contact.ContactID, Currency: dollars, TradeTerms: quote.CalcRef(*d.TradeTerms), PaymentTerms: quote.CalcRef(*d.PaymentTerms),
			NamedPlace: quote.CalcPlace(*d.NamedPlace), Delivery: quote.CalcDelivery(*d.Delivery)}
		var rfq []calc.RfqLine
		var costs []calc.CostLine
		for i, l := range d.Lines {
			q.Lines = append(q.Lines, calc.Line{Path: fmt.Sprintf("/args/data/lines/%d", i), LineID: l.LineID, RfqLineID: l.RfqLineID,
				SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision, Quantity: kernel.MustDecimal(l.Quantity), Unit: calc.Unit{Code: l.Unit.ID},
				UnitPrice: kernel.MustDecimal(l.UnitPrice), PriceBasisQuantity: kernel.MustDecimal(l.PriceBasisQuantity),
				Discount: kernel.MustDecimal(l.DiscountRatio), Tax: &calc.TaxApplication{Ref: quote.CalcRef(l.TaxApplication), Side: calc.SideSales,
					SubjectKind: calc.SubjectSku, SubjectID: l.Sku.SkuID, Profile: zeroTax}})
			rfq = append(rfq, calc.RfqLine{LineID: l.RfqLineID, SkuID: l.Sku.SkuID, Quantity: kernel.MustDecimal(l.Quantity), UOM: "PCS"})
			costs = append(costs, calc.CostLine{LineID: l.RfqLineID, SkuID: l.Sku.SkuID, SpecRevision: 1, Quantity: kernel.MustDecimal(l.Quantity),
				Unit: calc.Unit{Code: "PCS"}})
		}
		for i, c := range d.Charges {
			q.Charges = append(q.Charges, calc.Charge{Path: fmt.Sprintf("/args/data/charges/%d", i), ChargeID: c.ChargeID,
				ObligationKey: c.ObligationKey, Fee: &calc.FeeCategory{Ref: quote.CalcRef(c.FeeCategory), Side: calc.FeeSalesCharge,
					Inclusion: c.Inclusion}, Amount: kernel.MustDecimal(c.Amount), Inclusion: c.Inclusion, IncludedLineID: c.IncludedLineID,
				Evidence: []calc.Evidence{{SourceID: "source.1", Locator: "合成核实依据"}},
				Tax: &calc.TaxApplication{Ref: quote.CalcRef(c.TaxApplication), Side: calc.SideSales, SubjectKind: calc.SubjectFeeCategory,
					SubjectID: c.FeeCategory.ID, Profile: zeroTax}})
		}
		total, _ := kernel.NewMoney(dollars, 66000)
		f := calc.Facts{At: issuedAt,
			Rfq:     calc.Rfq{Customer: "customer.1", Contact: "contact.1", Lines: rfq},
			Profile: calc.Profile{TradeTerms: quote.CalcRef(trade), PaymentTerms: quote.CalcRef(payment)},
			Cost: calc.Cost{ContextKind: calc.ContextActual, CostBasis: calc.CostBasisNominalDirect, Rfq: calc.RfqRef{ID: "rfq.1", RowVersion: 2},
				PricingPolicy: quote.CalcRef(policy), TradeTerms: quote.CalcRef(trade), PaymentTerms: quote.CalcRef(payment),
				NamedPlace: quote.CalcPlace(deham), Delivery: quote.CalcDelivery(handover), Lines: costs, Total: total,
				ExcludedCostCategories: []string{"unrealized_fx_hedge", "imputed_financing_cost", "expected_credit_loss"}},
			Policy: calc.Policy{Ref: quote.CalcRef(policy), CalculationCurrency: dollars, MinimumMargin: kernel.MustDecimal("0.1"), MaxFXAge: time.Hour},
			Stages: []calc.Stage{{StageID: "P1", Percent: kernel.MustDecimal("30")}, {StageID: "P2", Percent: kernel.MustDecimal("70")}}}
		c, err := calc.Calculate(q, f)
		if err != nil {
			return nil, err
		}
		return quote.NewCalculation(c, usd, map[string]quote.Stage{"P1": {Trigger: "order_confirmed"},
			"P2": {Trigger: "after_shipment", Days: 30}}), nil
	}
}

// sealed freezes BRC-001's quote with the charges given.
func sealed(t testing.TB, charges ...quote.Charge) *quote.Seal {
	t.Helper()
	d := draft(charges...)
	s, err := quote.SealDraft("quote.1", 1, d, freezeFacts(), calculator(d), issuedAt)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return s
}

// snapshot builds order order.1's commercial snapshot of the sealed revision exactly as package
// order's BuildSnapshot does (Python t03_order_snapshot.build): canonical JSON of the copied terms,
// lines, charges and calculation, the source and the acceptance, and the commercial document whole.
// It returns the snapshot and its order hash.
func snapshot(t testing.TB, s *quote.Seal) ([]byte, string) {
	t.Helper()
	tree, err := quote.Tree(s.Commercial)
	if err != nil {
		t.Fatal(err)
	}
	doc := tree.(map[string]any)
	data := doc["data"].(map[string]any)
	terms := map[string]any{"issued_at": doc["issued_at"]}
	for _, k := range []string{"trade_terms_ref", "named_place_ref", "payment_terms_ref", "delivery", "expires_at"} {
		terms[k] = data[k]
	}
	lines := []any{}
	for _, l := range brc001 {
		lines = append(lines, l.id)
	}
	body := map[string]any{
		"snapshot_contract": SnapshotContract,
		"scope":             map[string]any{"tenant": string(scope.Tenant), "company": string(scope.Company)},
		"order_id":          "order.1",
		"conversion_slot":   SlotFullV1,
		"customer_ref":      data["customer_ref"],
		"contact_ref":       data["contact_ref"],
		"currency_ref":      data["currency_ref"],
		"terms":             terms,
		"lines":             data["lines"],
		"charges":           data["charges"],
		"calculation":       doc["calculation"],
		"source_quote_id":   "quote.1", "source_quote_revision": 1, "source_quote_hash": s.BusinessHash,
		"source_quote_number": "QT-2026-00001", "source_document_sha256": documentSHA,
		"acceptance_id": "acceptance.1",
		"acceptance": map[string]any{"acceptance_id": "acceptance.1", "effectivity_version": 1,
			"arrival_at": kernel.FormatTimestamp(arrivedAt), "arrival_basis": "channel_server",
			"expires_at":   kernel.FormatTimestamp(expiresAt),
			"customer_ref": map[string]any{"party_id": "customer.1", "revision": 2},
			"contact_ref":  map[string]any{"contact_id": "contact.1", "row_version": 2},
			"line_ids":     lines},
		"source_quote_snapshot": tree,
	}
	b, err := quote.Canonical(body)
	if err != nil {
		t.Fatal(err)
	}
	return b, quote.Hash(b)
}

// revisionOne is BRC-001's order at revision 1, with the charges given.
func revisionOne(t testing.TB, charges ...quote.Charge) *Revision {
	t.Helper()
	doc, hash := snapshot(t, sealed(t, charges...))
	r, err := FromSnapshot(doc, hash)
	if err != nil {
		t.Fatalf("FromSnapshot: %v", err)
	}
	return r
}

// facts are what POL says of the fixture's references: USD, PCS and DOZ.
func facts() Facts {
	return Facts{Currency: dollars, Units: map[string]calc.Unit{"PCS": {Code: "PCS"}, "DOZ": {Code: "DOZ"}}}
}

var amendment1 = AmendmentRef{ID: "amendment.1", Hash: hashOf("amendment.1")}

// shipped is a Fulfilment of FUL's ShipmentFacts v1 with the shipped quantities given.
func shipped(lines ...string) Fulfilment {
	f := Fulfilment{Source: "ful.shipment_facts.v1", Shipped: map[string]kernel.Decimal{}}
	for i := 0; i+1 < len(lines); i += 2 {
		f.Shipped[lines[i]] = kernel.MustDecimal(lines[i+1])
	}
	return f
}

// code returns "CODE at path" of a rejection, "OK" for nil.
func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " at " + rej.Fields[0].Path
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}
