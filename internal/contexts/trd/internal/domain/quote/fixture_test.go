package quote

import (
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The synthetic quote of the S3 suites on TRD's references: BRC-001's RFQ line L1 (120 PCS of
// sku.1), 8 USD a piece, the 660.00 USD cost snapshot under policy.1 (USD, minimum margin 0.1,
// quotes valid 7 days), delivered fixed at DEHAM, a customer with a buyer address, Chinese customer
// notes with a line break (Python examples/quote.json).

var issuedAt = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

const (
	resultHash   = "2a809a7b878bb2bedba3eed7534ecd55d43952b41bad144babe3c2d289cc677f"
	evidenceHash = "3668d7dc21b908c3ef5b25ffab99b94193b51c785d0f66a41950fed5107d221b"
	linesHash    = "3ee92f1118c480ab32896c9bbb35186fa05687b9e44c43f0e1544527267c868f"
	sourceHash   = "0397250103796959c07c27f59f03da066db3e42b27d81da397a18d2f2b936a61"
	profileHash  = "9572be119f04ad3072f7a8f98a14a6946efeec50764cb45d43bce99deafe8090"
)

var (
	company  = Ref{Kind: KindCompanyProfile, ID: "company.live", Revision: 1}
	usd      = Ref{Kind: KindCurrency, ID: "USD", Revision: 1}
	pcs      = Ref{Kind: KindUnit, ID: "PCS", Revision: 1}
	sale     = Ref{Kind: KindTaxApplication, ID: "tax.sale", Revision: 1}
	zero     = Ref{Kind: KindTaxProfile, ID: "tax.zero", Revision: 1}
	policy   = Ref{Kind: KindPricingPolicy, ID: "policy.1", Revision: 1}
	trade    = Ref{Kind: KindTradeTerms, ID: "terms.test", Revision: 1}
	payment  = Ref{Kind: KindPaymentTerms, ID: "payment.test", Revision: 1}
	template = Ref{Kind: KindQuoteDocumentTemplate, ID: "quote.template", Revision: 1}
	sequence = Ref{Kind: KindNumberSequence, ID: "quote.sequence", Revision: 1}
	freight  = Ref{Kind: KindFeeCategory, ID: "freight.sale", Revision: 1}
	chargeTx = Ref{Kind: KindTaxApplication, ID: "tax.charge", Revision: 1}
	dozen    = Ref{Kind: KindUnit, ID: "DOZ", Revision: 1}
	toPieces = Ref{Kind: KindUnitConversion, ID: "sku.1.doz", Revision: 1}
	proof    = []Evidence{{SourceID: "source.1", Locator: "合成核实依据"}}
	hq       = AddressRef{PartyID: "customer.1", PartyRevision: 2, AddressID: "hq"}
	deham    = BusinessRef{Kind: "location", ID: "DEHAM", Revision: 1}
	handover = Delivery{Kind: "fixed", Date: "2027-01-31", Milestone: "handover", Place: deham, TimeZone: "Asia/Shanghai"}
	dollars  = kernel.MustCurrency("USD", 2)
)

func hashOf(s string) string { return Hash([]byte(s)) }

func draft() Data {
	notes := "客户可见说明：合成样例\nKeep this line break."
	place, delivery := deham, handover
	return Data{Issuer: &company, Customer: &PartyRef{PartyID: "customer.1", Revision: 2},
		Contact: &ContactRef{ContactID: "contact.1", RowVersion: 2}, Profile: &ProfileRef{PartyID: "customer.1", Revision: 1},
		Addresses: []AddressRef{hq}, Rfq: &RfqRef{RfqID: "rfq.1", RowVersion: 2},
		Snapshot: &SnapshotRef{SnapshotID: "snapshot.1", ResultHash: resultHash},
		Lines: []Line{{LineID: "L1", RfqLineID: "L1", Sku: SkuRef{SkuID: "sku.1", SpecRevision: 1}, Quantity: "120.000000", Unit: pcs,
			UnitPrice: "8.0000", PriceBasisQuantity: "1.0", DiscountRatio: "0.00", TaxApplication: sale, Evidence: proof}},
		Currency: &usd, PricingPolicy: &policy, TradeTerms: &trade, NamedPlace: &place, PaymentTerms: &payment, Delivery: &delivery,
		DocumentTemplate: &template, Sequence: &sequence, ExpiresAt: "2026-10-28T08:00:00.000000Z", CustomerNotes: &notes, Evidence: proof}
}

func facts() Facts {
	return Facts{Company: "company.live", Party: &Party{Confirmed: true, Revision: 2, Customer: true},
		Contact: &Contact{PartyID: "customer.1", RowVersion: 2, Confirmed: true},
		Profile: &Profile{ContentHash: profileHash, PartyRevision: 2, PaymentTerms: payment, TradeTerms: trade,
			Contacts: []ContactRef{{ContactID: "contact.1", RowVersion: 2}}},
		Addresses: map[AddressRef]Address{hq: {Role: "buyer", Address: "Synthetic buyer address\n上海"},
			{PartyID: "customer.1", PartyRevision: 2, AddressID: "ap"}: {Role: "billing", Address: "Synthetic billing address"}},
		Rfq:   &Rfq{RowVersion: 2, Ready: true, LinesHash: linesHash},
		Specs: map[SkuRef]Verdict{{SkuID: "sku.1", SpecRevision: 1}: nil},
		Configs: map[Ref]Config{
			company: {ContentHash: hashOf("company"), Company: &IssuerSnapshot{LegalName: "SYNTHETIC Exporter — no actual trade",
				RegistrationNo: "SYNTHETIC-NOT-REAL", Country: "CN", Address: "Synthetic Shanghai address"}},
			usd:      {ContentHash: hashOf("usd")},
			pcs:      {ContentHash: hashOf("pcs")},
			dozen:    {ContentHash: hashOf("doz")},
			toPieces: {ContentHash: hashOf("conv")},
			sale:     {ContentHash: hashOf("sale"), Nested: []Ref{zero}},
			chargeTx: {ContentHash: hashOf("charge"), Nested: []Ref{zero}},
			zero:     {ContentHash: hashOf("zero")},
			policy:   {ContentHash: hashOf("policy"), Nested: []Ref{zero}, Policy: &Policy{QuoteValidDays: 7}},
			trade:    {ContentHash: hashOf("trade")},
			payment:  {ContentHash: hashOf("payment")},
			template: {ContentHash: hashOf("template"), Renderer: QuoteRendererV010},
			sequence: {ContentHash: hashOf("sequence"), SequenceType: DocumentTypeQuote},
			freight:  {ContentHash: hashOf("freight")},
		},
		Adopted:  []Ref{usd, pcs},
		Snapshot: &Snapshot{RfqLinesHash: linesHash, EvidenceHash: evidenceHash},
		Sources:  map[string]string{"source.1": sourceHash},
	}
}

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " at " + rej.Fields[0].Path
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

// calcEdit changes the calculation's inputs of a test before it runs.
type calcEdit func(q *calc.Quote, f *calc.Facts)

// calculator is the server calculation of draft d as the application resolves it against the
// fixture's upstream answers (application.ServerCalculator): POL's USD (2 minor digits), PCS and DOZ
// (0 digits, 12 PCS a DOZ for sku.1 at specification 1), the zero tax profile, the sales charge
// fee category, policy.1 with minimum margin minimum, the RFQ of customer.1 and contact.1, the
// 660.00 USD snapshot of an actual costing at DEHAM, one payment stage of 100 before shipment.
func calculator(d Data, minimum string, edits ...calcEdit) Calculate {
	return func() (*Calculation, error) {
		zeroTax := calc.TaxProfile{Ref: CalcRef(zero), Rate: kernel.MustDecimal("0"), PriceBasis: calc.TaxExclusive, Recovery: calc.Nonrecoverable}
		units := map[Ref]calc.Unit{pcs: {Code: "PCS"}, dozen: {Code: "DOZ"}}
		q := calc.Quote{Path: "/args/data", Rfq: calc.RfqRef{ID: d.Rfq.RfqID, RowVersion: d.Rfq.RowVersion}, Customer: d.Customer.PartyID,
			Contact: d.Contact.ContactID, Currency: dollars, TradeTerms: CalcRef(*d.TradeTerms), PaymentTerms: CalcRef(*d.PaymentTerms),
			NamedPlace: CalcPlace(*d.NamedPlace), Delivery: CalcDelivery(*d.Delivery)}
		for i, l := range d.Lines {
			line := calc.Line{Path: fmt.Sprintf("/args/data/lines/%d", i), LineID: l.LineID, RfqLineID: l.RfqLineID, SkuID: l.Sku.SkuID,
				SpecRevision: l.Sku.SpecRevision, Quantity: kernel.MustDecimal(l.Quantity), Unit: units[l.Unit],
				UnitPrice: kernel.MustDecimal(l.UnitPrice), PriceBasisQuantity: kernel.MustDecimal(l.PriceBasisQuantity),
				Discount: kernel.MustDecimal(l.DiscountRatio), Tax: &calc.TaxApplication{Ref: CalcRef(l.TaxApplication), Side: calc.SideSales,
					SubjectKind: calc.SubjectSku, SubjectID: "sku.1", Profile: zeroTax}}
			if l.BaseQuantity != "" {
				b := kernel.MustDecimal(l.BaseQuantity)
				line.BaseQuantity = &b
			}
			if l.Conversion != nil {
				line.Conversion = &calc.Conversion{Ref: CalcRef(*l.Conversion), SkuID: "sku.1", SpecRevision: 1, From: "DOZ", To: "PCS",
					Factor: kernel.MustDecimal("12")}
			}
			q.Lines = append(q.Lines, line)
		}
		for i, c := range d.Charges {
			q.Charges = append(q.Charges, calc.Charge{Path: fmt.Sprintf("/args/data/charges/%d", i), ChargeID: c.ChargeID,
				ObligationKey: c.ObligationKey, Fee: &calc.FeeCategory{Ref: CalcRef(c.FeeCategory), Side: calc.FeeSalesCharge, Inclusion: c.Inclusion},
				Amount: kernel.MustDecimal(c.Amount), Inclusion: c.Inclusion, IncludedLineID: c.IncludedLineID,
				Evidence: []calc.Evidence{{SourceID: "source.1", Locator: "合成核实依据"}},
				Tax: &calc.TaxApplication{Ref: CalcRef(c.TaxApplication), Side: calc.SideSales, SubjectKind: calc.SubjectFeeCategory,
					SubjectID: c.FeeCategory.ID, Profile: zeroTax}})
		}
		total, _ := kernel.NewMoney(dollars, 66000)
		f := calc.Facts{At: issuedAt,
			Rfq: calc.Rfq{Customer: "customer.1", Contact: "contact.1", Lines: []calc.RfqLine{{LineID: "L1", SkuID: "sku.1",
				Quantity: kernel.MustDecimal("120"), UOM: "PCS"}}},
			Profile: calc.Profile{TradeTerms: CalcRef(trade), PaymentTerms: CalcRef(payment)},
			Cost: calc.Cost{ContextKind: calc.ContextActual, CostBasis: calc.CostBasisNominalDirect, Rfq: calc.RfqRef{ID: "rfq.1", RowVersion: 2},
				PricingPolicy: CalcRef(policy), TradeTerms: CalcRef(trade), PaymentTerms: CalcRef(payment), NamedPlace: CalcPlace(deham),
				Delivery: CalcDelivery(handover), Lines: []calc.CostLine{{LineID: "L1", SkuID: "sku.1", SpecRevision: 1,
					Quantity: kernel.MustDecimal("120"), Unit: calc.Unit{Code: "PCS"}}}, Total: total,
				ExcludedCostCategories: []string{"unrealized_fx_hedge", "imputed_financing_cost", "expected_credit_loss"}},
			Policy: calc.Policy{Ref: CalcRef(policy), CalculationCurrency: dollars, MinimumMargin: kernel.MustDecimal(minimum), MaxFXAge: time.Hour},
			Stages: []calc.Stage{{StageID: "P1", Percent: kernel.MustDecimal("100.00")}}}
		for _, edit := range edits {
			edit(&q, &f)
		}
		c, err := calc.Calculate(q, f)
		if err != nil {
			return nil, err
		}
		return NewCalculation(c, usd, map[string]Stage{"P1": {Trigger: "before_shipment"}}), nil
	}
}

// sealed seals draft d on the fixture at minimum margin minimum.
func sealed(d Data, minimum string) (*Seal, error) {
	return SealDraft("quote.1", 1, d, facts(), calculator(d, minimum), issuedAt)
}
