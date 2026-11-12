package shipmentdocumentset

import (
	"math/big"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The insurance policy record (WBS S5-16; FUL-INV-005; simulated walkthrough SIM-02 step 8 and
// finding F-03; S5 decision D-13). SIM-02: 1,000 shirts CIF Hamburg at USD 9.20 (9,200.00, insured
// 10,120.00); after the customer's 10 % reduction (900) the factory delivers 850 and 850 ship:
// 7,820.00 on the invoice, insured at least 8,602.00. Python V0.50 has no fulfilment context: the
// vectors are the walkthrough's.

func usd(minor int64) kernel.Money {
	m, err := kernel.NewMoney(kernel.MustCurrency("USD", 2), minor)
	if err != nil {
		panic(err)
	}
	return m
}

// Incoterms 2020: the seller insures under CIF and CIP only.
func TestInsuranceRequiredOnCIFAndCIP(t *testing.T) {
	for _, x := range []string{"CIF", "CIP"} {
		if !InsuranceRequired(x) {
			t.Error(x)
		}
	}
	for _, x := range []string{"EXW", "FCA", "FAS", "FOB", "CFR", "CPT", "DAP", "DPU", "DDP", "cif", ""} {
		if InsuranceRequired(x) {
			t.Error(x)
		}
	}
}

// SIM-02 step 8 and FUL-INV-005: the cover is the invoice value times the contract percentage,
// rounded HALF_UP once at the currency's minor units.
func TestCoverIsTheContractPercentageHalfUp(t *testing.T) {
	jpy := kernel.MustCurrency("JPY", 0)
	yen := func(minor int64) kernel.Money {
		m, _ := kernel.NewMoney(jpy, minor)
		return m
	}
	for _, c := range []struct {
		invoice kernel.Money
		percent string
		want    string
	}{
		{usd(920000), "110", "10120.00 USD"}, // SIM-02: 1,000 × 9.20
		{usd(782000), "110", "8602.00 USD"},  // SIM-02: 850 × 9.20 after the reduction and the short delivery
		{usd(782005), "110", "8602.06 USD"},  // 8,602.055 rounds half up
		{usd(782004), "110", "8602.04 USD"},  // 8,602.044
		{usd(5), "110", "0.06 USD"},          // 0.055
		{usd(782000), "120", "9384.00 USD"},  // a contract asking 120 %
		{usd(782000), "110.5", "8641.10 USD"},
		{yen(1005), "110", "1106 JPY"}, // 1,105.5
		{yen(1001), "110", "1101 JPY"}, // 1,101.1
	} {
		got, err := Cover(c.invoice, kernel.MustDecimal(c.percent))
		if err != nil || got.String() != c.want {
			t.Errorf("%s × %s %%: %s %v, want %s", c.invoice, c.percent, got, err, c.want)
		}
	}
}

// sim02Facts is SIM-02's shipment: n PCS of sku.1 shipped on order line L1 of q PCS at 9.20 USD
// (the order line's payable q × 9.20), packed 10 to a carton.
func sim02Facts(n, q int64) Facts {
	cartons := n / 10
	sh := &shipment.Shipment{ID: "shipment.sim02", Status: shipment.StatusPrepared, RowVersion: 2, Route: shipment.RouteSupplierDirect,
		Order: shipment.OrderBinding{OrderID: "order.sim02", OrderRevision: 2, OrderHash: strings.Repeat("e", 64)}, CustomerID: "customer.b2",
		Lines: []shipment.Line{{ID: "S1", OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS", UnitRevision: 1,
			OrderQuantity: kernel.DecimalFromInt(q), Quantity: kernel.DecimalFromInt(n)}},
		Parties: []shipment.Party{{Role: shipment.RoleCustomer, PartyID: "customer.b2", PartyRevision: 1}},
		Actual: &shipment.Packing{Kind: shipment.KindActual, Seq: 1, Packages: []shipment.Package{{Cartons: cartons, CartonFrom: 1,
			Contents: []shipment.Content{{LineID: "S1", PerCarton: dec("10")}}, Gross: ptr("5"), Net: ptr("4.5"), Volume: ptr("0.05")}}}}
	payable := big.NewInt(920 * q)
	return Facts{Shipment: sh, Prices: map[string]shipment.Price{"S1": {CurrencyID: "USD", CurrencyRevision: 1, UnitPrice: dec("9.20"),
		Net: payable, Tax: big.NewInt(0), Payable: payable}}}
}

// sim02Upstream is the order's CIF Hamburg terms (or incoterm's) as TRD shows them.
func sim02Upstream(incoterm, number string) Upstream {
	return Upstream{Order: &OrderFacts{Binding: OrderBinding{OrderID: "order.sim02", OrderRevision: 2, OrderHash: strings.Repeat("e", 64)},
		Incoterm: incoterm, Edition: "Incoterms 2020", NamedPlace: "DEHAM", Stages: []Stage{{StageID: "all", Percent: "100", Trigger: "after_shipment",
			Days: 60, DayBasis: "calendar"}}, Addresses: []Address{{Role: "buyer", PartyID: "customer.b2", PartyRevision: 1,
			Text: "Synthetic importer B2\nHamburg"}}},
		Names:    map[PartyKey]string{{PartyID: "customer.b2", Revision: 1}: "Synthetic Importer B2 GmbH"},
		Currency: &Currency{Code: "USD", Revision: 1, MinorUnits: 2}, Found: map[string]bool{"evidence": true}, Number: number}
}

func sim02Request(kind string) Request {
	r := request(kind)
	r.Particulars.Names = map[string]map[string]string{"sku.1": {LanguageEn: "Woven shirt, navy", LanguageZhCN: "藏青梭织衬衫"}}
	r.Particulars.NameOrder = []NameRef{{"sku.1", LanguageEn}, {"sku.1", LanguageZhCN}}
	r.Particulars.Notify = nil
	return r
}

// sim02Set is SIM-02's document set with a CI and a PL of n of q PCS under incoterm.
func sim02Set(t *testing.T, incoterm string, n, q int64) (*Set, Facts) {
	t.Helper()
	f := sim02Facts(n, q)
	s := NewSet("docs.sim02", f.Shipment, "", act())
	for _, kind := range []string{KindCommercialInvoice, KindPackingList} {
		d, err := Prepare(s, sim02Request(kind), f, sim02Upstream(incoterm, strings.ToUpper(kind[:2])+"-SIM02"), act())
		if err != nil {
			t.Fatal(err)
		}
		s.Add(d)
	}
	return s, f
}

func insurer() *shipment.ServiceRoleFact {
	return &shipment.ServiceRoleFact{Ref: shipment.ServiceRoleRef{PartyID: "insurer.1", ServiceRoleID: "cargo", Revision: 1}, Role: ServiceInsurer}
}

func policyRequest(insured string) InsuranceRequest {
	return InsuranceRequest{Insurer: insurer().Ref, PolicyNumber: "PIC-2026-0001", Clauses: []string{"ICC (A)", "Institute War Clauses (Cargo)"},
		IssuedOn: "2026-12-18", Insured: insured, Premium: "12.00", FileID: "file.policy", Evidence: []string{"evidence"}, IdempotencyKey: "k.ins"}
}

func policyUpstream() InsuranceUpstream {
	return InsuranceUpstream{Insurer: insurer(), Currency: &Currency{Code: "USD", Revision: 1, MinorUnits: 2}, Found: map[string]bool{"evidence": true},
		File: &File{ID: "file.policy", SHA256: strings.Repeat("f", 64), MimeType: "application/pdf"},
		Costing: Costing{Available: true, SnapshotID: "snapshot.ins", ResultHash: strings.Repeat("9", 64), Currency: "USD", MinorUnits: 2,
			InsuranceMinor: "1200", Components: []string{"fee.2"}}}
}

func recorded(t *testing.T, s *Set, req InsuranceRequest, f Facts) *Document {
	t.Helper()
	d, err := RecordInsurance(s, req, f, policyUpstream(), act())
	if err != nil {
		t.Fatal(err)
	}
	s.Add(d)
	return d
}

// SIM-02 step 8 and FUL-INV-005: on the CIF invoice of 850 × 9.20 = 7,820.00 a policy insuring
// 8,601.99 is refused with INSURANCE_COVERAGE_INSUFFICIENT and one insuring 8,602.00 is recorded
// with the cover it was checked on; another currency than the invoice's never covers; a contract
// may ask more than 110 % but never less; and the refusals come in their order.
func TestRecordInsuranceCoversTheCIFInvoiceSIM02(t *testing.T) {
	s, f := sim02Set(t, "CIF", 850, 900)
	ci := s.Current(KindCommercialInvoice)
	if ci.Content.Totals.PayableMinor != "782000" || ci.Content.Terms.Incoterm != "CIF" {
		t.Fatalf("the invoice %+v", ci.Content.Totals)
	}
	_, err := RecordInsurance(s, policyRequest("8601.99"), f, policyUpstream(), act())
	if rej, _ := kernel.AsRejection(err); outcome(err) != CodeInsuranceCoverageInsufficient+" /args/insured_amount" || rej.Category != kernel.CategoryInvalid ||
		!strings.Contains(reason(err), "8601.99 USD is below 110 % of the CIF commercial invoice CO-SIM02 v1 of 7820.00 USD: 8602.00 USD") {
		t.Fatalf("8,601.99: %v", err)
	}
	d := recorded(t, s, policyRequest("8602.00"), f)
	p := d.Policy()
	if d.Kind != KindInsurancePolicy || d.Version != 1 || d.Status != Draft || d.Number != "PIC-2026-0001" || d.File.ID != "file.policy" ||
		p.InsuredMinor != "860200" || p.PremiumMinor != "1200" || p.CoveragePercent != "110" || p.Currency != (CurrencyContent{Code: "USD", Revision: 1,
		MinorUnits: 2}) || p.Insurer != (Insurer{PartyID: "insurer.1", ServiceRoleID: "cargo", Revision: 1}) || p.Coverage == nil ||
		*p.Coverage != (Coverage{Incoterm: "CIF", InvoiceVersion: 1, InvoiceNumber: "CO-SIM02", InvoiceCurrency: "USD", MinorUnits: 2,
			InvoiceMinor: "782000", Percent: "110", RequiredMinor: "860200", InsuredCurrency: "USD", InsuredMinor: "860200", Sufficient: true}) {
		t.Fatalf("recorded %+v / %+v", d, p)
	}
	if json, sum := Canonical(d.Record); json != d.ContentJSON || sum != d.ContentSHA256 || !strings.Contains(json, `"insurance_policy":{"insurer"`) {
		t.Fatalf("content %s", d.ContentJSON)
	}
	if s.CoverageNow() == nil || !s.CoverageNow().Sufficient || s.CheckCoverage() != nil {
		t.Fatal("the set is covered")
	}

	eur := policyUpstream()
	eur.Currency = &Currency{Code: "EUR", Revision: 1, MinorUnits: 2}
	_, err = RecordInsurance(s, policyRequest("9000.00"), f, eur, act())
	if outcome(err) != CodeInsuranceCoverageInsufficient+" /args/currency" || !strings.Contains(reason(err), "insures in EUR") {
		t.Fatalf("EUR: %v", err)
	}
	more := policyRequest("8602.00")
	more.CoveragePercent = "120"
	_, err = RecordInsurance(s, more, f, policyUpstream(), act())
	if outcome(err) != CodeInsuranceCoverageInsufficient+" /args/insured_amount" || !strings.Contains(reason(err), ": 9384.00 USD") {
		t.Fatalf("120 %%: %v", err)
	}
	more.Insured = "9384.00"
	if d, err := RecordInsurance(s, more, f, policyUpstream(), act()); err != nil || d.Policy().Coverage.RequiredMinor != "938400" ||
		d.Version != 2 {
		t.Fatalf("120 %% covered: %+v %v", d, err)
	}

	refused := func(want string, edit func(*InsuranceRequest, *InsuranceUpstream, *Facts)) {
		t.Helper()
		req, up, facts := policyRequest("8602.00"), policyUpstream(), sim02Facts(850, 900)
		edit(&req, &up, &facts)
		if _, err := RecordInsurance(s, req, facts, up, act()); outcome(err) != want {
			t.Errorf("want %s, got %s", want, outcome(err))
		}
	}
	refused("REFERENCE_INVALID /args/shipment_id", func(_ *InsuranceRequest, _ *InsuranceUpstream, f *Facts) { f.Shipment = nil })
	refused("INVALID_STATE /args/shipment_id", func(_ *InsuranceRequest, _ *InsuranceUpstream, f *Facts) {
		f.Shipment.Status = shipment.StatusCancelled
	})
	refused("REFERENCE_INVALID /args/evidence_ids/1", func(r *InsuranceRequest, _ *InsuranceUpstream, _ *Facts) {
		r.Evidence = []string{"evidence", "hidden"}
	})
	refused("REFERENCE_INVALID /args/insurer", func(_ *InsuranceRequest, u *InsuranceUpstream, _ *Facts) { u.Insurer = nil })
	refused("REFERENCE_STALE /args/insurer", func(_ *InsuranceRequest, u *InsuranceUpstream, _ *Facts) { u.Insurer.Code = CodeReferenceStale })
	refused("INVALID_KNOWN_VALUE /args/insurer", func(_ *InsuranceRequest, u *InsuranceUpstream, _ *Facts) { u.Insurer.Role = "forwarder" })
	refused("REFERENCE_INVALID /args/file_id", func(_ *InsuranceRequest, u *InsuranceUpstream, _ *Facts) { u.File = nil })
	refused("REFERENCE_INVALID /args/currency", func(_ *InsuranceRequest, u *InsuranceUpstream, _ *Facts) { u.Currency = nil })
	refused("INVALID_KNOWN_VALUE /args/insured_amount", func(r *InsuranceRequest, _ *InsuranceUpstream, _ *Facts) { r.Insured = "8602.001" })
	refused("INVALID_KNOWN_VALUE /args/insured_amount", func(r *InsuranceRequest, _ *InsuranceUpstream, _ *Facts) { r.Insured = "0" })
	refused("INVALID_KNOWN_VALUE /args/premium", func(r *InsuranceRequest, _ *InsuranceUpstream, _ *Facts) { r.Premium = "1.234" })
	refused("INVALID_KNOWN_VALUE /args/coverage_percent", func(r *InsuranceRequest, _ *InsuranceUpstream, _ *Facts) { r.CoveragePercent = "109.99" })
	refused("OK", func(r *InsuranceRequest, _ *InsuranceUpstream, _ *Facts) { r.CoveragePercent = "110.00" })
}

// FUL-INV-005 at issue (F-03; BT12): a CIF set without a policy has a required difference that no
// acknowledgement covers (SHIPMENT_DOCUMENTS_UNRECONCILED at /insurance_policy); an FCA set needs
// none. A policy that covered the invoice it was recorded on no longer covers a later invoice of
// more (INSURANCE_COVERAGE_INSUFFICIENT at /insurance_policy/insured_amount, decided after the
// reconciliation); a new policy does, and the issue issues it with the set.
func TestIssueNeedsACoveringPolicyOnCIF(t *testing.T) {
	s, f := sim02Set(t, "CIF", 850, 900)
	diffs := Reconcile(s, f)
	if len(diffs) != 1 || diffs[0] != (Difference{Path: "/insurance_policy", Document: KindInsurancePolicy, Item: "document",
		Expected: "an insurance policy (CIF)", Against: KindCommercialInvoice, Required: true}) {
		t.Fatalf("CIF without a policy %+v", diffs)
	}
	_, _, err := s.Issue(2, shipment.StatusPrepared, diffs, nil, act())
	if outcome(err) != CodeShipmentDocumentsUnreconciled+" /insurance_policy" {
		t.Fatalf("issue without a policy: %v", err)
	}
	_, _, err = s.Issue(2, shipment.StatusPrepared, diffs, []Ack{{Path: "/insurance_policy", Reason: "保险随后补"}}, act())
	if outcome(err) != "INVALID_KNOWN_VALUE /args/acknowledgements/0/path" || !strings.Contains(reason(err), "A missing insurance policy is never") {
		t.Fatalf("acknowledging the policy: %v", err)
	}

	fca, ff := sim02Set(t, "FCA", 850, 900)
	if d := Reconcile(fca, ff); len(d) != 0 || fca.CoverageNow() != nil {
		t.Fatalf("FCA %+v", d)
	}
	if _, _, err := fca.Issue(2, shipment.StatusPrepared, nil, nil, act()); err != nil {
		t.Fatalf("FCA issues without a policy: %v", err)
	}

	recorded(t, s, policyRequest("8602.00"), f)
	if d := Reconcile(s, f); len(d) != 0 {
		t.Fatalf("covered %+v", d)
	}
	// The shipment grows to 900 PCS before the issue: CI v2 is 8,280.00, the policy covers 8,602.00
	// of the 9,108.00 needed. The PL still says 850; its differences are acknowledged.
	g := sim02Facts(900, 900)
	d, err := Prepare(s, sim02Request(KindCommercialInvoice), g, sim02Upstream("CIF", "CI-SIM02"), act())
	if err != nil {
		t.Fatal(err)
	}
	s.Add(d)
	diffs = Reconcile(s, g)
	var acks []Ack
	for _, x := range diffs {
		acks = append(acks, Ack{Path: x.Path, Reason: "合成测试"})
	}
	_, _, err = s.Issue(4, shipment.StatusPrepared, diffs, acks, act())
	if outcome(err) != CodeInsuranceCoverageInsufficient+" /insurance_policy/insured_amount" ||
		!strings.Contains(reason(err), "8602.00 USD is below 110 % of the CIF commercial invoice CI-SIM02 v2 of 8280.00 USD: 9108.00 USD") {
		t.Fatalf("issue on the larger invoice: %v", err)
	}
	if s.CoverageNow().Sufficient || s.Current(KindCommercialInvoice).Status != Draft {
		t.Fatal("nothing moved")
	}
	recorded(t, s, policyRequest("9108.00"), g)
	iss, superseded, err := s.Issue(5, shipment.StatusPrepared, diffs, acks, act())
	if err != nil {
		t.Fatal(err)
	}
	var issued []string
	for _, x := range iss.Documents {
		issued = append(issued, x.Kind+" v"+string(rune('0'+x.Version)))
	}
	if !slices.Equal(issued, []string{"commercial_invoice v2", "packing_list v1", "insurance_policy v2"}) || len(superseded) != 0 ||
		s.Document(KindInsurancePolicy, 1).Status != Superseded || s.Current(KindInsurancePolicy).Status != Issued {
		t.Fatalf("issued %v", issued)
	}
}

// The policy must run from loading: one issued after the bill of lading's shipped-on-board date is
// a difference against the B/L record, which may be acknowledged.
func TestPolicyDatedAfterShipmentIsADifference(t *testing.T) {
	s, f := sim02Set(t, "CIF", 850, 900)
	late := policyRequest("8602.00")
	late.IssuedOn = "2026-12-21"
	recorded(t, s, late, f)
	bl := &Document{Kind: KindBillOfLading, Version: 1, Number: "BL-1", Record: &Record{BillOfLading: &BillOfLading{Number: "BL-1",
		ShippedOnBoard: "2026-12-20", Packages: 85, GrossWeightKg: "425", Consignee: PartyContent{Name: "Synthetic Importer B2 GmbH",
			Address: "Synthetic importer B2\nHamburg"}, Marks: "N/M"}}}
	s.Add(bl)
	var found *Difference
	for _, d := range Reconcile(s, f) {
		if d.Document == KindInsurancePolicy {
			found = &d
		}
	}
	if found == nil || *found != (Difference{Path: "/insurance_policy/issued_on", Document: KindInsurancePolicy, Version: 1, Item: "issued_on",
		Found: "2026-12-21", Expected: "2026-12-20", Against: KindBillOfLading}) {
		t.Fatalf("%+v", found)
	}
}

// D-13 (R78): the premium is compared with the costing's insurance component and never refused —
// equal shows no difference, another amount shows premium − component, another currency is not
// compared (FUL converts nothing), a costing without an insurance component or none CST shows says
// so.
func TestPremiumIsComparedWithTheCosting(t *testing.T) {
	c := policyUpstream().Costing
	for _, x := range []struct {
		premium kernel.Money
		edit    func(*Costing)
		want    PremiumComparison
	}{
		{usd(1200), nil, PremiumComparison{Status: PremiumEqual, SnapshotID: "snapshot.ins", ResultHash: c.ResultHash, Currency: "USD", MinorUnits: 2,
			ComponentMinor: "1200", Components: []string{"fee.2"}, PremiumCurrency: "USD", PremiumMinor: "1200", DifferenceMinor: "0"}},
		{usd(1500), nil, PremiumComparison{Status: PremiumDifferent, SnapshotID: "snapshot.ins", ResultHash: c.ResultHash, Currency: "USD", MinorUnits: 2,
			ComponentMinor: "1200", Components: []string{"fee.2"}, PremiumCurrency: "USD", PremiumMinor: "1500", DifferenceMinor: "300"}},
		{usd(1050), nil, PremiumComparison{Status: PremiumDifferent, SnapshotID: "snapshot.ins", ResultHash: c.ResultHash, Currency: "USD", MinorUnits: 2,
			ComponentMinor: "1200", Components: []string{"fee.2"}, PremiumCurrency: "USD", PremiumMinor: "1050", DifferenceMinor: "-150"}},
		{usd(1200), func(c *Costing) { c.Currency = "CNY" }, PremiumComparison{Status: PremiumNotComparable, SnapshotID: "snapshot.ins",
			ResultHash: c.ResultHash, Currency: "CNY", MinorUnits: 2, ComponentMinor: "1200", Components: []string{"fee.2"}, PremiumCurrency: "USD", PremiumMinor: "1200",
			Reason: "The costing is in CNY, the premium in USD; FUL converts no currency"}},
		{usd(1200), func(c *Costing) { c.Components, c.InsuranceMinor = nil, "0" }, PremiumComparison{Status: PremiumNoComponent,
			SnapshotID: "snapshot.ins", ResultHash: c.ResultHash, Currency: "USD", MinorUnits: 2, PremiumCurrency: "USD", PremiumMinor: "1200",
			Reason: "The costing has no insurance component"}},
		{usd(1200), func(c *Costing) { *c = Costing{Reason: "The order adopted no cost snapshot"} }, PremiumComparison{Status: PremiumUnavailable,
			PremiumCurrency: "USD", PremiumMinor: "1200", Reason: "The order adopted no cost snapshot"}},
	} {
		cc := c
		cc.Components = slices.Clone(c.Components)
		if x.edit != nil {
			x.edit(&cc)
		}
		if got := Compare(x.premium, cc); !comparisonEqual(got, x.want) {
			t.Errorf("%s: %+v, want %+v", x.premium, got, x.want)
		}
	}
	// The comparison is kept with the record and changes nothing of its acceptance.
	s, f := sim02Set(t, "CIF", 850, 900)
	req := policyRequest("8602.00")
	req.Premium = "15.00"
	if d := recorded(t, s, req, f); d.Policy().Costing.Status != PremiumDifferent || d.Policy().Costing.DifferenceMinor != "300" ||
		d.Sources[len(d.Sources)-1] != (Entry{Name: "costing", Value: "different snapshot.ins component 1200 USD difference 300"}) {
		t.Fatalf("%+v %+v", d.Policy().Costing, d.Sources)
	}
}

func reason(err error) string {
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 {
		return rej.Fields[0].Reason
	}
	return ""
}

func comparisonEqual(a, b PremiumComparison) bool {
	return slices.Equal(a.Components, b.Components) && a.Status == b.Status && a.SnapshotID == b.SnapshotID && a.ResultHash == b.ResultHash &&
		a.Currency == b.Currency && a.MinorUnits == b.MinorUnits && a.ComponentMinor == b.ComponentMinor && a.PremiumCurrency == b.PremiumCurrency &&
		a.PremiumMinor == b.PremiumMinor && a.DifferenceMinor == b.DifferenceMinor && a.Reason == b.Reason
}
