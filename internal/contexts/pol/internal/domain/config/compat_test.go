package config

import (
	"encoding/json"
	"testing"
	"time"
)

// Compatibility of the S5 additions (s5-plan A3a: "omitted-when-empty fields keep existing content
// hashes"). s3Contents are revisions of every kind POL had before S5, each with the members the
// kind had then; s3Stored is the data POL stored for each and its content hash, both computed by
// the code of main 481fa24 (the S3 merge) and pinned here. A revision stored then must read back,
// and hash, exactly as it was stored.
func s3Contents() []Content {
	from := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	until := from.Add(24 * time.Hour)
	cal := &Ref{Kind: "calendar", ConfigID: "cal", Revision: 2}
	taxRef := Ref{Kind: "tax_profile", ConfigID: "tax", Revision: 1}
	return []Content{
		{Kind: KindCompanyProfile, ValidFrom: from, CompanyProfile: &CompanyProfile{LegalName: "合成企业", RegistrationNo: "SYNTHETIC", Country: "CN",
			Address: "合成地址", BaseCurrency: "CNY", TimeZone: "Asia/Shanghai", PolicyScope: "synthetic",
			DefaultLegalTerms: &Ref{Kind: "legal_terms", ConfigID: "legal.default", Revision: 1}}},
		{Kind: KindCurrency, ValidFrom: from, Currency: &Currency{Code: "USD", Name: "USD", MinorUnits: 2, Rounding: "HALF_UP"}},
		{Kind: KindUnit, ValidFrom: from, Unit: &Unit{Code: "PCS", Name: "件", Dimension: "count", Precision: 0}},
		{Kind: KindUnitConversion, ValidFrom: from, ValidUntil: until, UnitConversion: &UnitConversion{SkuID: "sku", SpecRevision: 1,
			FromUnit: "DOZEN", ToUnit: "PCS", Factor: "12", Basis: "packaging"}},
		{Kind: KindFXRate, ValidFrom: from, ValidUntil: until, FXRate: &FXRate{FromCurrency: "USD", ToCurrency: "CNY", Rate: "7.123456",
			Purpose: "cost_estimate", SourceName: "SYNTHETIC RATE", ObservedAt: "2026-10-22T00:00:00.000000Z"}},
		{Kind: KindFXRate, ValidFrom: from, ValidUntil: until, FXRate: &FXRate{FromCurrency: "EUR", ToCurrency: "USD", Rate: "1.1",
			Purpose: "quotation", SourceName: "SYNTHETIC RATE", ObservedAt: "2026-10-21T23:50:00.000000Z"}},
		{Kind: KindCalendar, ValidFrom: from, Calendar: &Calendar{TimeZone: "Asia/Shanghai", WorkingWeekdays: []int{1, 2, 3, 4, 5},
			Holidays: []string{"2026-10-01"}}},
		{Kind: KindTradeTerms, ValidFrom: from, TradeTerms: &TradeTerms{Incoterm: "FCA", Edition: "2020", NamedPlace: "CNSHA",
			ResponsibilityNote: "synthetic", Delivery: Delivery{Kind: "lead_time", AnchorEvent: "order_confirmed", Days: 30, DayBasis: "working",
				Calendar: cal}}},
		// BRC-001's terms (S3 chain): 30 % at order confirmation, 70 % 30 days after shipment.
		{Kind: KindPaymentTerms, ValidFrom: from, PaymentTerms: &PaymentTerms{Name: "30/70", Stages: []PaymentStage{
			{StageID: "deposit", Percent: "30", Trigger: "order_confirmed", Days: 0},
			{StageID: "balance", Percent: "70", Trigger: "after_shipment", Days: 30}}}},
		{Kind: KindPaymentTerms, ValidFrom: from, PaymentTerms: &PaymentTerms{Name: "30/70 working", Stages: []PaymentStage{
			{StageID: "deposit", Percent: "30", Trigger: "order_confirmed"}, {StageID: "balance", Percent: "70", Trigger: "before_shipment"}},
			DayBasis: "working", Calendar: cal}},
		{Kind: KindTaxProfile, ValidFrom: from, ValidUntil: until, TaxProfile: &TaxProfile{Name: "SYNTHETIC 13%", Rate: "0.13",
			PriceBasis: "inclusive", RecoveryTreatment: "nonrecoverable", PolicyBasis: "synthetic"}},
		{Kind: KindTaxApplication, ValidFrom: from, TaxApplication: &TaxApplication{Name: "sales", Side: "sales", SubjectKind: "sku",
			SubjectID: "sku", TaxProfile: taxRef, Jurisdiction: "CN", BasisNote: "synthetic"}},
		{Kind: KindFeeCategory, ValidFrom: from, FeeCategory: &FeeCategory{Name: "包装费", Category: "packaging", Side: "purchase_cost",
			Inclusion: "included_in_line", AllocationBasis: "quantity", PolicyBasis: "synthetic"}},
		{Kind: KindPricingPolicy, ValidFrom: from, PricingPolicy: &PricingPolicy{Name: "policy", CalculationCurrency: "CNY", MinimumMargin: "0.25",
			QuoteValidDays: 7, MaxFXAgeSeconds: 86400, TaxProfile: taxRef}},
		{Kind: KindBankAccount, ValidFrom: from, BankAccount: &BankAccount{OwnerKind: "company", OwnerID: "company.A", BankName: "synthetic bank",
			AccountNumber: "TEST1234567890", Currency: "CNY", Purpose: "both"}},
		{Kind: KindNumberSequence, ValidFrom: from, NumberSequence: &NumberSequence{DocumentType: "invoice", Prefix: "CI-", Width: 5,
			Reset: "annual", TimeZone: "Asia/Shanghai"}},
		{Kind: KindDocumentTemplate, ValidFrom: from, DocumentTemplate: &DocumentTemplate{Name: "quote", DocumentType: "quote",
			TemplateText: "{document.number}", FieldNames: []string{"document.number"}}},
		{Kind: KindQuoteDocumentTemplate, ValidFrom: from, QuoteDocumentTemplate: &QuoteDocumentTemplate{Name: "quote", Language: "zh-CN",
			RendererVersion: "quote-text/0.1.0", Title: "报价单", Sections: []string{"issuer", "recipient", "identity", "lines", "totals", "terms",
				"delivery", "payments", "validity", "notes"}, FooterNote: ""}},
		// The foundation demo's credit policy before S5.
		{Kind: KindCreditPolicy, ValidFrom: from, CreditPolicy: &CreditPolicy{Name: "synthetic credit policy", Currency: "CNY", DefaultLimit: "0",
			LimitSources: []string{"internal_assessment"}, ApprovalTiers: []CreditApprovalTier{{Level: "credit_officer", MaxAmount: "10000.00"}},
			OverdueToleranceDays: 30}},
		{Kind: KindScreeningRegulation, ValidFrom: from, ScreeningRegulation: &ScreeningRegulation{Lists: []ScreeningList{
			{ListCode: "synthetic.cn", Jurisdiction: "CN"}, {ListCode: "eu.fsf", Jurisdiction: "foreign", LegalConfirmationEvidenceID: "legal.eu"}},
			RescreenIntervalDays: 90}},
		{Kind: KindLegalTerms, ValidFrom: from, LegalTerms: &LegalTerms{Name: "CISG with PRC law", GoverningLaw: "CN", CISG: "applies",
			FormationRule: "arrival", DisputeResolution: DisputeResolution{Method: "arbitration", Institution: "CIETAC", Seat: "Shanghai",
				Language: "zh-CN"}}},
		{Kind: KindOrderDocumentTemplate, ValidFrom: from, OrderDocumentTemplate: &OrderDocumentTemplate{Name: "pi", DocumentKind: "proforma_invoice",
			Language: "en", RendererVersion: "order-text/0.1.0", Title: "PROFORMA INVOICE", Sections: []string{"seller", "buyer", "identity", "lines",
				"totals", "terms", "delivery", "payments", "notes"}, Clauses: []string{}, FooterNote: ""}},
	}
}

// s3Stored is, for each of s3Contents, the content hash and the stored data of main 481fa24.
var s3Stored = []struct{ hash, data string }{
	{"fcbf92c1e3eff53556af4791d124716c20c043a647ca2e22603f68dd7e2b3760", "{\"legal_name\":\"合成企业\",\"registration_no\":\"SYNTHETIC\",\"country\":\"CN\",\"address\":\"合成地址\",\"base_currency\":\"CNY\",\"time_zone\":\"Asia/Shanghai\",\"policy_scope\":\"synthetic\",\"default_legal_terms\":{\"kind\":\"legal_terms\",\"id\":\"legal.default\",\"revision\":1}}"},
	{"7fedb736bd7867fa6ca2bf0b643345b172302f3c14b5102dd63b2e2bc099e7b8", "{\"code\":\"USD\",\"name\":\"USD\",\"minor_units\":2,\"rounding\":\"HALF_UP\"}"},
	{"ee55211b0098bd8cddd7a840b31071514497870e62af598211b1e3f72c249ca7", "{\"code\":\"PCS\",\"name\":\"件\",\"dimension\":\"count\",\"precision\":0}"},
	{"724d2f707011a443fc4760e2dba640a88736e7dff6a3437e056c113f36b4e4f1", "{\"sku_id\":\"sku\",\"spec_revision\":1,\"from_unit\":\"DOZEN\",\"to_unit\":\"PCS\",\"factor\":\"12\",\"basis\":\"packaging\"}"},
	{"44186c3ebbb84048d0042efa2182e6b8ace16897a1d1c68c99c0a3fcff854488", "{\"from_currency\":\"USD\",\"to_currency\":\"CNY\",\"rate\":\"7.123456\",\"purpose\":\"cost_estimate\",\"source_name\":\"SYNTHETIC RATE\",\"observed_at\":\"2026-10-22T00:00:00.000000Z\"}"},
	{"c7eeb885d923466afcb6cbce6f2734f27fc5a8fc09ad25ba3f3aa69250b732d3", "{\"from_currency\":\"EUR\",\"to_currency\":\"USD\",\"rate\":\"1.1\",\"purpose\":\"quotation\",\"source_name\":\"SYNTHETIC RATE\",\"observed_at\":\"2026-10-21T23:50:00.000000Z\"}"},
	{"2841bb03ab7b1bb465225ed892e3cf1f17a88adc32e0e62b4a839008025f7919", "{\"time_zone\":\"Asia/Shanghai\",\"working_weekdays\":[1,2,3,4,5],\"holidays\":[\"2026-10-01\"]}"},
	{"243dd59b92d2b6ae9c0c5d2f34cc6bd64719f9c618a574d4721aea084aca22fc", "{\"incoterm\":\"FCA\",\"edition\":\"2020\",\"named_place\":\"CNSHA\",\"responsibility_note\":\"synthetic\",\"delivery\":{\"kind\":\"lead_time\",\"anchor_event\":\"order_confirmed\",\"days\":30,\"day_basis\":\"working\",\"calendar\":{\"kind\":\"calendar\",\"id\":\"cal\",\"revision\":2}}}"},
	{"d6c11b03fa478faa2d9b8da29c1d6fcca2e6cf3bcb6198574a582ae9b8f30048", "{\"name\":\"30/70\",\"stages\":[{\"stage_id\":\"deposit\",\"percent\":\"30\",\"trigger\":\"order_confirmed\",\"days\":0},{\"stage_id\":\"balance\",\"percent\":\"70\",\"trigger\":\"after_shipment\",\"days\":30}]}"},
	{"5ce7e11be3deb37647af1846bcf35809c4c2d1c25d7f84f0ae828302d68de066", "{\"name\":\"30/70 working\",\"stages\":[{\"stage_id\":\"deposit\",\"percent\":\"30\",\"trigger\":\"order_confirmed\",\"days\":0},{\"stage_id\":\"balance\",\"percent\":\"70\",\"trigger\":\"before_shipment\",\"days\":0}],\"day_basis\":\"working\",\"calendar\":{\"kind\":\"calendar\",\"id\":\"cal\",\"revision\":2}}"},
	{"d247787e498ce3bf54d3c5f31701c408870fa64907180f4fcf70578d32f033a1", "{\"name\":\"SYNTHETIC 13%\",\"rate\":\"0.13\",\"price_basis\":\"inclusive\",\"recovery_treatment\":\"nonrecoverable\",\"policy_basis\":\"synthetic\"}"},
	{"ae2b8b9e36e253acaa75cd818c1271f4c14b95f06b2ac868c2fe3829d7a01f69", "{\"name\":\"sales\",\"side\":\"sales\",\"subject_kind\":\"sku\",\"subject_id\":\"sku\",\"tax_profile\":{\"kind\":\"tax_profile\",\"id\":\"tax\",\"revision\":1},\"jurisdiction\":\"CN\",\"basis_note\":\"synthetic\"}"},
	{"6a472e809dd40901d8325fea7d8205b5b8c854d84c5550d272d2e758b033d32a", "{\"name\":\"包装费\",\"category\":\"packaging\",\"side\":\"purchase_cost\",\"inclusion\":\"included_in_line\",\"allocation_basis\":\"quantity\",\"policy_basis\":\"synthetic\"}"},
	{"b14aa96cdc34ae8d909738226ec5364e6655b3abdc902e47ea45d949d54ed6f1", "{\"name\":\"policy\",\"calculation_currency\":\"CNY\",\"minimum_margin\":\"0.25\",\"quote_valid_days\":7,\"max_fx_age_seconds\":86400,\"tax_profile\":{\"kind\":\"tax_profile\",\"id\":\"tax\",\"revision\":1}}"},
	{"1ed2beafbe1023e86aeafe01349968c1d71a4f2bc5f0ce4396772a50a6cfa5a5", "{\"owner_kind\":\"company\",\"owner_id\":\"company.A\",\"bank_name\":\"synthetic bank\",\"account_number\":\"TEST1234567890\",\"currency\":\"CNY\",\"purpose\":\"both\"}"},
	{"29d45f19a1bd6c03a364f252ddd30ef39442d096d6fa73de2ab8991604d0ddf5", "{\"document_type\":\"invoice\",\"prefix\":\"CI-\",\"width\":5,\"reset\":\"annual\",\"time_zone\":\"Asia/Shanghai\"}"},
	{"9e5d15aa73730f1dcdc900971bd56a8f7e77c28c0b774a47ad4c7bc028b73c63", "{\"name\":\"quote\",\"document_type\":\"quote\",\"template_text\":\"{document.number}\",\"field_names\":[\"document.number\"]}"},
	{"6e14860da780d4fda67c5d976bbb949991a4ab47c8177b46caf55eff6bf5564e", "{\"name\":\"quote\",\"language\":\"zh-CN\",\"renderer_version\":\"quote-text/0.1.0\",\"title\":\"报价单\",\"sections\":[\"issuer\",\"recipient\",\"identity\",\"lines\",\"totals\",\"terms\",\"delivery\",\"payments\",\"validity\",\"notes\"],\"footer_note\":\"\"}"},
	{"5b5c0de3d8b6d361a31b5a9535c05049e63417ef05418dab5ec325eedf93e764", "{\"name\":\"synthetic credit policy\",\"currency\":\"CNY\",\"default_limit\":\"0\",\"limit_sources\":[\"internal_assessment\"],\"approval_tiers\":[{\"level\":\"credit_officer\",\"max_amount\":\"10000.00\"}],\"overdue_tolerance_days\":30}"},
	{"58239702954df52ecf422c4e33b3f9c1895b82c9e082692a7b61350955ad7c19", "{\"lists\":[{\"list_code\":\"synthetic.cn\",\"jurisdiction\":\"CN\"},{\"list_code\":\"eu.fsf\",\"jurisdiction\":\"foreign\",\"legal_confirmation_evidence_id\":\"legal.eu\"}],\"rescreen_interval_days\":90}"},
	{"f13dfc04f8470adcb3610b688600191aa8bc21cbcb4821781439934db6cf29be", "{\"name\":\"CISG with PRC law\",\"governing_law\":\"CN\",\"cisg\":\"applies\",\"formation_rule\":\"arrival\",\"dispute_resolution\":{\"method\":\"arbitration\",\"institution\":\"CIETAC\",\"seat\":\"Shanghai\",\"language\":\"zh-CN\"}}"},
	{"e22d064ce502e23cfe9acdd212e8fecb234177b7a9dbe33f7e8e128dfb5cc5e0", "{\"name\":\"pi\",\"document_kind\":\"proforma_invoice\",\"language\":\"en\",\"renderer_version\":\"order-text/0.1.0\",\"title\":\"PROFORMA INVOICE\",\"sections\":[\"seller\",\"buyer\",\"identity\",\"lines\",\"totals\",\"terms\",\"delivery\",\"payments\",\"notes\"],\"clauses\":[],\"footer_note\":\"\"}"},
}

func TestRevisionsStoredBeforeS5KeepTheirDataAndHash(t *testing.T) {
	contents := s3Contents()
	if len(contents) != len(s3Stored) {
		t.Fatalf("%d contents, %d stored", len(contents), len(s3Stored))
	}
	kinds := map[Kind]bool{}
	for i, c := range contents {
		kinds[c.Kind] = true
		want := s3Stored[i]
		raw, err := c.DataJSON()
		if err != nil {
			t.Fatal(c.Kind, err)
		}
		if hash, _ := c.Hash(); string(raw) != want.data || hash != want.hash {
			t.Errorf("%s now stores %s with hash %s", c.Kind, raw, hash)
		}
		// The stored data decodes into the S5 structures and hashes as it was stored.
		back := Content{Kind: c.Kind, ValidFrom: c.ValidFrom, ValidUntil: c.ValidUntil}
		if err = back.DecodeData([]byte(want.data)); err != nil {
			t.Fatal(c.Kind, err)
		}
		if hash, _ := back.Hash(); hash != want.hash {
			t.Errorf("%s stored before S5 hashes as %s", c.Kind, hash)
		}
		// PostgreSQL keeps the data as jsonb, which reorders members; a revision read back from it
		// still hashes as stored.
		var generic map[string]any
		if err = json.Unmarshal([]byte(want.data), &generic); err != nil {
			t.Fatal(err)
		}
		reordered, _ := json.Marshal(generic)
		jsonb := Content{Kind: c.Kind, ValidFrom: c.ValidFrom, ValidUntil: c.ValidUntil}
		if err = jsonb.DecodeData(reordered); err != nil {
			t.Fatal(c.Kind, err)
		}
		if hash, _ := jsonb.Hash(); hash != want.hash {
			t.Errorf("%s read back from jsonb hashes as %s", c.Kind, hash)
		}
		// Nothing of S5 appears in a revision stored before it.
		switch {
		case back.PaymentTerms != nil:
			for _, s := range back.PaymentTerms.Stages {
				if s.ReleaseControl != "" {
					t.Errorf("%+v", s)
				}
			}
		case back.CreditPolicy != nil:
			p := back.CreditPolicy
			if p.DocumentsSecured || p.TwoApproversAboveTop || p.ReservationValidityDays != 0 || p.DecisionValidityDays != 0 {
				t.Errorf("%+v", p)
			}
		}
	}
	if len(kinds) != 20 || kinds[KindShipmentDocumentTemplate] || kinds[KindExportTaxPolicy] {
		t.Fatalf("every kind before S5, and only those: %v", kinds)
	}
}
