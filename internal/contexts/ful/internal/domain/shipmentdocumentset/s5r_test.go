package shipmentdocumentset

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
)

// The S5R decisions on the shipment document set (simulated sign-off E0 of 2026-11-13; unit R6, WBS
// S5R-06): S5R-D06 (CIF/CIP insurance: missing policy, contract percentage, clauses, premium share),
// S5R-D12 (the certificate of origin at issue), S5R-D35 (the documents print the order revision the
// shipment binds) and S5R-D40 (the order's notify party is printed and checked). Python V0.50 has no
// fulfilment context: the vectors are the register's and SIM-02's.

// S5R-D06 (FUL-INV-005): a missing policy on a CIF invoice is a missing required document —
// SHIPMENT_DOCUMENTS_UNRECONCILED at /insurance_policy, never acknowledged — not
// INSURANCE_COVERAGE_INSUFFICIENT, which is for a cover below the percentage only.
func TestS5RD06AMissingPolicyIsAMissingRequiredDocument(t *testing.T) {
	s, f := sim02Set(t, "CIF", 850, 900)
	diffs := Reconcile(s, f)
	_, _, err := s.Issue(2, shipment.StatusPrepared, diffs, []Ack{{Path: "/insurance_policy", Reason: "客户自保"}}, act())
	if outcome(err) != "INVALID_KNOWN_VALUE /args/acknowledgements/0/path" || !strings.Contains(reason(err), "A missing insurance policy is never acknowledged") {
		t.Fatalf("acknowledged: %v", err)
	}
	if _, _, err = s.Issue(2, shipment.StatusPrepared, diffs, nil, act()); outcome(err) != CodeShipmentDocumentsUnreconciled+" /insurance_policy" {
		t.Fatalf("issued: %v", err)
	}
}

// S5R-D06 (FUL-INV-005, R49): the contract percentage is the insurance_percent of the trade terms
// the bound order revision names. With a contract of 120 % on the CIF invoice of 7,820.00 the cover
// is 9,384.00: a policy of 115 % (8,993.00) is INSURANCE_COVERAGE_INSUFFICIENT; the clerk may state a
// higher percentage (125 %: 9,775.00) but not a lower one (115 %: INVALID_KNOWN_VALUE); without a
// percentage the contract's is the policy's. No contract FUL reads is REFERENCE_INVALID (fail closed).
func TestS5RD06TheContractPercentageComesFromTheTradeTerms(t *testing.T) {
	s, f := sim02Set(t, "CIF", 850, 900)
	up := policyUpstream()
	up.Contract = &Contract{Percent: "120", Source: "trade_terms:terms.cif120@2"}
	_, err := RecordInsurance(s, policyRequest("8993.00"), f, up, act())
	if outcome(err) != CodeInsuranceCoverageInsufficient+" /args/insured_amount" ||
		!strings.Contains(reason(err), "8993.00 USD is below 120 % of the CIF commercial invoice CO-SIM02 v1 of 7820.00 USD: 9384.00 USD") {
		t.Fatalf("115 %% of the invoice under a 120 %% contract: %v", err)
	}
	stated := policyRequest("9384.00")
	stated.CoveragePercent = "115"
	if _, err = RecordInsurance(s, stated, f, up, act()); outcome(err) != "INVALID_KNOWN_VALUE /args/coverage_percent" ||
		!strings.Contains(reason(err), "The contract insures 120 % (trade_terms:terms.cif120@2)") {
		t.Fatalf("a stated percentage below the contract's: %v", err)
	}
	d, err := RecordInsurance(s, policyRequest("9384.00"), f, up, act())
	if err != nil || d.Policy().CoveragePercent != "120" || d.Policy().ContractPercent != "120" || d.Policy().ContractSource != "trade_terms:terms.cif120@2" ||
		d.Policy().Coverage.RequiredMinor != "938400" || !strings.Contains(d.ContentJSON, `"contract_percent":"120","contract_source":"trade_terms:terms.cif120@2"`) {
		t.Fatalf("the contract's percentage %+v %v", d.Policy(), err)
	}
	if src := sourceOf(d, "contract"); src != "120% trade_terms:terms.cif120@2" {
		t.Fatalf("source %q", src)
	}
	higher := policyRequest("9775.00")
	higher.CoveragePercent = "125"
	if d, err := RecordInsurance(s, higher, f, up, act()); err != nil || d.Policy().CoveragePercent != "125" || d.Policy().ContractPercent != "120" ||
		d.Policy().Coverage.RequiredMinor != "977500" {
		t.Fatalf("a higher percentage %+v %v", d, err)
	}
	up.Contract = nil
	if _, err = RecordInsurance(s, policyRequest("9384.00"), f, up, act()); outcome(err) != "REFERENCE_INVALID /args/shipment_id" {
		t.Fatalf("no contract: %v", err)
	}
	up.Contract = &Contract{Percent: "100", Source: "trade_terms:bad@1"}
	if _, err = RecordInsurance(s, policyRequest("9384.00"), f, up, act()); outcome(err) != "REFERENCE_INVALID /args/shipment_id" {
		t.Fatalf("a contract below 110: %v", err)
	}
}

// S5R-D06 (3): the premium is compared with the whole costing component, never prorated; the
// shipment's share of the order revision is shown beside it — SIM-02's 850 of 900 PCS is 94.44 %.
func TestS5RD06ThePremiumComparisonShowsTheShipmentsShare(t *testing.T) {
	s, f := sim02Set(t, "CIF", 850, 900)
	up := policyUpstream()
	up.Costing.ShipmentNetMinor, up.Costing.OrderNetMinor = "782000", "828000"
	d, err := RecordInsurance(s, policyRequest("8602.00"), f, up, act())
	c := d.Policy().Costing
	if err != nil || c.Status != PremiumEqual || c.ComponentMinor != "1200" || c.ShipmentPercent != "94.44" || c.ShipmentNetMinor != "782000" ||
		c.OrderNetMinor != "828000" {
		t.Fatalf("%+v %v", c, err)
	}
	up.Costing.OrderNetMinor = ""
	if d, err := RecordInsurance(s, policyRequest("8602.00"), f, up, act()); err != nil || d.Policy().Costing.ShipmentPercent != "" {
		t.Fatalf("no order total, no share %+v %v", d, err)
	}
	var share PremiumComparison
	share.shipmentShare("1", "3")
	if share.ShipmentPercent != "33.33" {
		t.Fatal(share)
	}
	share.shipmentShare("2", "3")
	if share.ShipmentPercent != "66.67" {
		t.Fatal(share)
	}
}

// S5R-D06 (4): CIP needs ICC (A) or the equivalent, CIF at least ICC (C) (Incoterms 2020 A5): a
// policy below is a difference /insurance_policy/clauses against the invoice that can be
// acknowledged when the contract agreed otherwise. The China Insurance Clauses' All Risks, WPA and
// FPA count as (A), (B) and (C).
func TestS5RD06TheClausesFollowTheIncoterm(t *testing.T) {
	for _, c := range []struct {
		clauses []string
		want    int
	}{
		{[]string{"Institute Cargo Clauses (A)"}, CoverA}, {[]string{"ICC(B)", "Institute War Clauses (Cargo)"}, CoverB},
		{[]string{"icc (c)"}, CoverC}, {[]string{"All Risks as per CIC 1/1/1981"}, CoverA}, {[]string{"W.P.A."}, CoverB},
		{[]string{"F.P.A."}, CoverC}, {[]string{"一切险"}, CoverA}, {[]string{"水渍险"}, CoverB}, {[]string{"平安险"}, CoverC},
		{[]string{"Institute War Clauses (Cargo)", "Institute Strikes Clauses (Cargo)"}, CoverNone},
		{[]string{"ICC (C)", "ICC (A)"}, CoverA}, {[]string{"Institute Cargo Clauses (Air)"}, CoverNone},
	} {
		if got := ClauseCover(c.clauses); got != c.want {
			t.Errorf("%v: %d", c.clauses, got)
		}
	}
	for _, c := range []struct {
		incoterm string
		clauses  []string
		found    string
		expected string
	}{
		{"CIP", []string{"ICC (C)"}, "ICC (C)", "ICC (A) or better (CIP)"},
		{"CIP", []string{"ICC (B)"}, "ICC (B)", "ICC (A) or better (CIP)"},
		{"CIF", []string{"Institute War Clauses (Cargo)"}, "no Institute Cargo Clauses or equivalent", "ICC (C) or better (CIF)"},
		{"CIP", []string{"ICC (A)"}, "", ""},
		{"CIF", []string{"ICC (C)"}, "", ""},
	} {
		s, f := sim02Set(t, c.incoterm, 850, 900)
		req := policyRequest("8602.00")
		req.Clauses = c.clauses
		recorded(t, s, req, f)
		diffs := Reconcile(s, f)
		if c.found == "" {
			if len(diffs) != 0 {
				t.Errorf("%s %v: %+v", c.incoterm, c.clauses, diffs)
			}
			continue
		}
		if len(diffs) != 1 || diffs[0] != (Difference{Path: "/insurance_policy/clauses", Document: KindInsurancePolicy, Version: 1, Item: "clauses",
			Found: c.found, Expected: c.expected, Against: KindCommercialInvoice}) {
			t.Errorf("%s %v: %+v", c.incoterm, c.clauses, diffs)
			continue
		}
		if _, _, err := s.Issue(3, shipment.StatusPrepared, diffs, []Ack{{Path: diffs[0].Path, Reason: "合同约定 ICC (C) 即可"}}, act()); err != nil {
			t.Errorf("%s %v: the contract's own clauses are acknowledged: %v", c.incoterm, c.clauses, err)
		}
	}
}

func sourceOf(d *Document, name string) string {
	for _, e := range d.Sources {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// coSet is the fixture's set with a certificate of origin record naming certificate id ("": none).
func coSet(t *testing.T, id string) *Set {
	t.Helper()
	s := NewSet("docs.1", fixtureShipment(), "", act())
	f := fixtureFacts()
	prepared(t, s, KindCommercialInvoice, "CI-2026-00001", f)
	prepared(t, s, KindPackingList, "PL-2026-00001", f)
	req := Request{Kind: KindCertificateOfOrigin, FileID: "file.co", Certificate: &Certificate{Number: "CO-1", Issuer: "合成贸促会", IssuedOn: "2026-12-01",
		CertificateID: id, CertificateVersion: 1}, IdempotencyKey: "k.co"}
	up := Upstream{File: &File{ID: "file.co", SHA256: strings.Repeat("c", 64), MimeType: "application/pdf"}}
	if id != "" {
		up.Certificate = &CertificateResult{Number: "CO-1", Reason: "applicable", Binding: id + "/v1/applicable"}
	}
	d, err := Prepare(s, req, fixtureFacts(), up, act())
	if err != nil {
		t.Fatal(err)
	}
	s.Add(d)
	return s
}

// S5R-D12 (FUL-INV-004, MD-40): a certificate of origin record that names no CNX certificate result
// is the difference co_not_registered, which the clerk acknowledges with a reason; one that names a
// result is asked of CNX again at issue: still applicable, nothing; expired, no longer covering or
// superseded since the draft — or not asked — a required difference
// /certificate_of_origin/certificate_ref (SHIPMENT_DOCUMENTS_UNRECONCILED, never acknowledged); an
// issued record is not asked again; a reconciliation without CNX's answers (a prepare) does not judge
// it.
func TestS5RD12TheCertificateOfOriginAtIssue(t *testing.T) {
	plain := coSet(t, "")
	f := fixtureFacts()
	f.Rechecked = map[RecheckKey]*CertificateResult{}
	diffs := Reconcile(plain, f)
	want := Difference{Path: "/certificate_of_origin/certificate_ref", Document: KindCertificateOfOrigin, Version: 1, Item: ItemCONotRegistered,
		Expected: "a certificate result registered in CNX", Against: AgainstCertificateResult}
	if len(diffs) != 1 || diffs[0] != want {
		t.Fatalf("co_not_registered %+v", diffs)
	}
	if _, ok := plain.ToRecheck(); ok {
		t.Fatal("a record without a certificate result is not asked of CNX")
	}
	if _, _, err := plain.Issue(3, shipment.StatusPrepared, diffs, nil, act()); outcome(err) != CodeShipmentDocumentsUnreconciled+
		" /certificate_of_origin/certificate_ref" {
		t.Fatalf("issued without acknowledging: %v", err)
	}
	if iss, _, err := plain.Issue(3, shipment.StatusPrepared, diffs, []Ack{{Path: want.Path, Reason: "客户只要复印件，原产地证由客户自办"}}, act()); err != nil ||
		len(iss.Acknowledged) != 1 || iss.Acknowledged[0].Difference != want {
		t.Fatalf("acknowledged: %+v %v", iss, err)
	}

	for _, c := range []struct {
		name   string
		answer *CertificateResult
		asked  bool
		found  string
	}{
		{"expired since the draft", &CertificateResult{Code: CodeCertificateNotApplicable, Reason: "expired 2026-12-10"}, true,
			"CERTIFICATE_NOT_APPLICABLE: expired 2026-12-10"},
		{"superseded since the draft", &CertificateResult{Code: CodeReferenceStale, Reason: "superseded by revision 2"}, true,
			"REFERENCE_STALE: superseded by revision 2"},
		{"not asked", nil, false, "not asked of CNX again at issue"},
		{"still applicable", &CertificateResult{Number: "CO-1", Reason: "applicable"}, true, ""},
	} {
		s := coSet(t, "crt-1")
		k, ok := s.ToRecheck()
		if !ok || k != (RecheckKey{Version: 1, CertificateID: "crt-1", CertificateVersion: 1}) {
			t.Fatalf("%s: %+v", c.name, k)
		}
		f := fixtureFacts()
		if Reconcile(s, f) != nil {
			t.Fatalf("%s: a prepare's reconciliation does not ask CNX", c.name)
		}
		f.Rechecked = map[RecheckKey]*CertificateResult{}
		if c.asked {
			f.Rechecked[k] = c.answer
		}
		diffs := Reconcile(s, f)
		if c.found == "" {
			if len(diffs) != 0 {
				t.Errorf("%s: %+v", c.name, diffs)
			}
			if _, _, err := s.Issue(3, shipment.StatusPrepared, diffs, nil, act()); err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			if _, ok := s.ToRecheck(); ok {
				t.Errorf("%s: an issued record is not asked again", c.name)
			}
			continue
		}
		if len(diffs) != 1 || diffs[0] != (Difference{Path: "/certificate_of_origin/certificate_ref", Document: KindCertificateOfOrigin, Version: 1,
			Item: ItemCertificateApplicability, Found: c.found, Expected: "a certificate result that applies to the shipment at issue",
			Against: AgainstCertificateResult, Required: true}) {
			t.Errorf("%s: %+v", c.name, diffs)
			continue
		}
		_, _, err := s.Issue(3, shipment.StatusPrepared, diffs, []Ack{{Path: diffs[0].Path, Reason: "x"}}, act())
		if outcome(err) != "INVALID_KNOWN_VALUE /args/acknowledgements/0/path" || !strings.Contains(reason(err), "resolved, never acknowledged") {
			t.Errorf("%s: acknowledged %v", c.name, err)
		}
		if _, _, err := s.Issue(3, shipment.StatusPrepared, diffs, nil, act()); outcome(err) != CodeShipmentDocumentsUnreconciled+
			" /certificate_of_origin/certificate_ref" {
			t.Errorf("%s: issued %v", c.name, err)
		}
	}
}

// S5R-D35 (OrderRevisionRef v1): a CI prints the order revision the shipment binds — its terms and
// its line prices, with a price basis quantity and a discount ratio when they are not 1 and 0 — and
// is refused when TRD shows another revision or hash; FUL's own prices stay the reconciliation value.
func TestS5RD35TheInvoicePrintsTheBoundRevision(t *testing.T) {
	up := fixtureUpstream("CI-2026-00009")
	up.Order.Lines["L1"] = LinePrice{UnitPrice: "800", PriceBasisQuantity: "100", DiscountRatio: "0", NetMinor: "96000", TaxMinor: "0",
		PayableMinor: "96000"}
	up.Order.Lines["L2"] = LinePrice{UnitPrice: "13.157894", PriceBasisQuantity: "1", DiscountRatio: "0.05", NetMinor: "30000", TaxMinor: "3900",
		PayableMinor: "33900"}
	s := NewSet("docs.1", fixtureShipment(), "", act())
	d, err := Prepare(s, request(KindCommercialInvoice), fixtureFacts(), up, act())
	if err != nil {
		t.Fatal(err)
	}
	l1, l2 := d.Content.Lines[0], d.Content.Lines[1]
	if l1.UnitPrice != "800" || l1.PriceBasisQuantity != "100" || l1.DiscountRatio != "" || l1.NetMinor != "40000" ||
		l2.PriceBasisQuantity != "" || l2.DiscountRatio != "0.05" || l2.NetMinor != "30000" || d.Content.Terms.OrderRevision != 1 {
		t.Fatalf("%+v %+v", l1, l2)
	}
	en := string(d.Renditions[0].Content)
	if !strings.Contains(en, "S1 | sku.1 r1 | Cotton shirt, red | 50 PCS | price 800 per 100 PCS | net 400.00") ||
		!strings.Contains(en, "| price 13.157894 | discount 0.05 | net 300.00") {
		t.Fatal(en)
	}
	if zh := string(d.Renditions[1].Content); !strings.Contains(zh, "单价 800 每 100 PCS") || !strings.Contains(zh, "折扣 0.05") {
		t.Fatal(zh)
	}
	s.Add(d)
	diffs := Reconcile(s, fixtureFacts())
	var prices []string
	for _, x := range diffs {
		if x.Item == "unit_price" {
			prices = append(prices, x.Path+" "+x.Found+" "+x.Expected+" "+x.Against)
		}
	}
	if strings.Join(prices, "; ") != "/commercial_invoice/lines/S1/unit_price 800 8 order_line; /commercial_invoice/lines/S2/unit_price 13.157894 12.5 order_line" {
		t.Fatalf("FUL's own prices are the reconciliation value: %v", prices)
	}
	for _, edit := range []func(*OrderFacts){
		func(o *OrderFacts) { o.Binding.OrderRevision = 2 },
		func(o *OrderFacts) { o.Binding.OrderHash = strings.Repeat("b", 64) },
		func(o *OrderFacts) { delete(o.Lines, "L2") },
	} {
		up := fixtureUpstream("CI-2026-00010")
		edit(up.Order)
		if _, err := Prepare(NewSet("docs.1", fixtureShipment(), "", act()), request(KindCommercialInvoice), fixtureFacts(), up, act()); outcome(err) !=
			"REFERENCE_INVALID /args/shipment_id" {
			t.Errorf("%v", err)
		}
	}
}

// S5R-D40 (FUL-INV-004, R44): the order's notify party (address role notify) is the documents'
// notify party unless the clerk states one (notify_source order or stated); the CI's notify party
// is reconciled against the order's; a notify party without a name the actor reads is refused.
func TestS5RD40TheOrdersNotifyPartyIsPrintedAndChecked(t *testing.T) {
	notify := &Address{Role: "notify", PartyID: "notify.1", PartyRevision: 2, Text: "Synthetic notify office\nRotterdam"}
	up := fixtureUpstream("CI-2026-00011")
	up.Order.Notify = notify
	up.Names[PartyKey{PartyID: "notify.1", Revision: 2}] = "Synthetic Notify BV"
	req := request(KindCommercialInvoice)
	req.Particulars.Notify = nil
	s := NewSet("docs.1", fixtureShipment(), "", act())
	d, err := Prepare(s, req, fixtureFacts(), up, act())
	if err != nil {
		t.Fatal(err)
	}
	c := d.Content
	order := PartyContent{PartyID: "notify.1", PartyRevision: 2, Name: "Synthetic Notify BV", Address: "Synthetic notify office\nRotterdam"}
	if c.NotifySource != "order" || c.OrderNotify == nil || *c.OrderNotify != order || c.Notify == nil || *c.Notify != order {
		t.Fatalf("%+v %+v %s", c.Notify, c.OrderNotify, c.NotifySource)
	}
	if en := string(d.Renditions[0].Content); !strings.Contains(en, "Notify: Synthetic Notify BV\nSynthetic notify office\nRotterdam") {
		t.Fatal(en)
	}
	s.Add(d)
	for _, x := range Reconcile(s, fixtureFacts()) {
		if x.Item == "notify" {
			t.Fatalf("the order's own notify party %+v", x)
		}
	}
	stated := request(KindCommercialInvoice) // states the buyer as notify party
	other := NewSet("docs.2", fixtureShipment(), "", act())
	d, err = Prepare(other, stated, fixtureFacts(), up, act())
	if err != nil || d.Content.NotifySource != "stated" {
		t.Fatalf("%+v %v", d, err)
	}
	other.Add(d)
	var found []Difference
	for _, x := range Reconcile(other, fixtureFacts()) {
		if x.Item == "notify" {
			found = append(found, x)
		}
	}
	if len(found) != 1 || found[0].Path != "/commercial_invoice/notify" || found[0].Against != AgainstOrder ||
		found[0].Expected != "Synthetic Notify BV / Synthetic notify office Rotterdam" {
		t.Fatalf("a stated notify party other than the order's %+v", found)
	}
	delete(up.Names, PartyKey{PartyID: "notify.1", Revision: 2})
	if _, err := Prepare(NewSet("docs.3", fixtureShipment(), "", act()), req, fixtureFacts(), up, act()); outcome(err) != "REFERENCE_INVALID /args/shipment_id" ||
		!strings.Contains(reason(err), "notify party") {
		t.Fatalf("no name: %v", err)
	}
}
