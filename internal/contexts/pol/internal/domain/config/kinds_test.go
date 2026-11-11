package config

import (
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func unitFact(code, dimension string, precision int) fact {
	return fact{Target{Kind: KindUnit, ID: code}, Fact{Rev: &Revision{Number: 1, Status: Active, Content: Content{Kind: KindUnit,
		ValidFrom: start, Unit: &Unit{Code: code, Name: code, Dimension: dimension, Precision: precision}}}}}
}

func exactFact(kind Kind, id string, revision int64, refused error) fact {
	return fact{Target{Kind: kind, ID: id, Revision: revision}, Fact{Rev: &Revision{Number: revision}, Refused: refused}}
}

func company(id string) Content {
	return Content{Kind: KindCompanyProfile, ValidFrom: start, CompanyProfile: &CompanyProfile{LegalName: "合成企业", RegistrationNo: "SYNTHETIC",
		Country: "CN", Address: "合成地址", BaseCurrency: "CNY", TimeZone: "Asia/Shanghai", PolicyScope: "synthetic"}}
}

func conversion(from, to, factor string) Content {
	return Content{Kind: KindUnitConversion, ValidFrom: start, UnitConversion: &UnitConversion{SkuID: "sku", SpecRevision: 1, FromUnit: from,
		ToUnit: to, Factor: factor, Basis: "packaging"}}
}

func tax(rate, basis string) Content {
	return Content{Kind: KindTaxProfile, ValidFrom: start, TaxProfile: &TaxProfile{Name: "SYNTHETIC TAX", Rate: rate, PriceBasis: basis,
		RecoveryTreatment: "nonrecoverable", PolicyBasis: "test only"}}
}

func policy(margin string) Content {
	return Content{Kind: KindPricingPolicy, ValidFrom: start, PricingPolicy: &PricingPolicy{Name: "policy", CalculationCurrency: "CNY",
		MinimumMargin: margin, QuoteValidDays: 7, MaxFXAgeSeconds: 86400, TaxProfile: Ref{Kind: "tax_profile", ConfigID: "tax", Revision: 1}}}
}

func template(text string, fields ...string) Content {
	return Content{Kind: KindDocumentTemplate, ValidFrom: start, DocumentTemplate: &DocumentTemplate{Name: "quote test", DocumentType: "quote",
		TemplateText: text, FieldNames: fields}}
}

func credit(limit string, tiers ...CreditApprovalTier) Content {
	return Content{Kind: KindCreditPolicy, ValidFrom: start, CreditPolicy: &CreditPolicy{Name: "credit", Currency: "CNY", DefaultLimit: limit,
		LimitSources: []string{"internal_assessment"}, ApprovalTiers: tiers, OverdueToleranceDays: 30}}
}

// Python test_foundation.py test_company_identity_and_immutable_currency_semantics (company half):
// a company profile identifies the scope's company and no other.
func TestCompanyProfileIdentifiesTheCurrentCompany(t *testing.T) {
	cny := facts(currencyFact("CNY", 2))
	if got := code(CheckReferences("company.A", company("company.A"), cny)); got != "OK" {
		t.Fatal(got)
	}
	if got := code(CheckReferences("company.B", company("company.B"), cny)); got != "INVALID_KNOWN_VALUE /args/id" {
		t.Fatal(got)
	}
	if got := code(CheckReferences("company.A", company("company.A"), facts())); got != "REFERENCE_INVALID /args/data/base_currency" {
		t.Fatal(got)
	}
	mars := company("company.A")
	mars.CompanyProfile.TimeZone = "Mars/Olympus_Mons"
	if got := code(Validate("company.A", mars)); got != "INVALID_KNOWN_VALUE /args/data/time_zone" {
		t.Fatal(got)
	}
}

// Python test_foundation.py test_unit_conversion_is_sku_scoped_and_exact (definition half): a
// conversion joins two different units of one dimension with a positive factor.
func TestUnitConversionJoinsTwoUnitsOfOneDimension(t *testing.T) {
	units := facts(unitFact("DOZEN", "count", 0), unitFact("PCS", "count", 0), unitFact("KG", "mass", 3))
	for _, c := range []struct {
		content Content
		want    string
	}{
		{conversion("DOZEN", "PCS", "12"), "OK"},
		{conversion("PCS", "PCS", "1"), "INVALID_KNOWN_VALUE /args/data/to_unit"},
		{conversion("DOZEN", "PCS", "0"), "INVALID_KNOWN_VALUE /args/data/factor"},
		{conversion("DOZEN", "KG", "12"), "UNIT_SCOPE_MISMATCH /args/data/to_unit"},
		{conversion("DOZEN", "BOX", "12"), "REFERENCE_INVALID /args/data/to_unit"},
	} {
		err := Validate("dozen.pcs", c.content)
		if err == nil {
			err = CheckReferences("dozen.pcs", c.content, units)
		}
		if got := code(err); got != c.want {
			t.Errorf("%+v: %s, want %s", c.content.UnitConversion, got, c.want)
		}
	}
}

// Python test_foundation.py validate (tax_profile, pricing_policy): rates and margins lie in [0, 1]
// and a tax that does not apply has no rate.
func TestTaxRatesAndMarginsAreRatios(t *testing.T) {
	for _, c := range []struct {
		content Content
		want    string
	}{
		{tax("0.13", "inclusive"), "OK"},
		{tax("1", "exclusive"), "OK"},
		{tax("1.5", "exclusive"), "INVALID_KNOWN_VALUE /args/data/rate"},
		{tax("0", "not_applicable"), "OK"},
		{tax("0.13", "not_applicable"), "INVALID_KNOWN_VALUE /args/data/rate"},
		{policy("0.25"), "OK"},
		{policy("1.01"), "INVALID_KNOWN_VALUE /args/data/minimum_margin"},
	} {
		if got := code(Validate("x", c.content)); got != c.want {
			t.Errorf("%+v %+v: %s, want %s", c.content.TaxProfile, c.content.PricingPolicy, got, c.want)
		}
	}
}

// A pricing policy names its calculation currency by code and its tax profile by exact revision;
// both must be in force when it is saved and published.
func TestPricingPolicyReferences(t *testing.T) {
	p := policy("0.25")
	refs := p.References()
	if len(refs) != 2 || refs[0].Target() != (Target{Kind: KindCurrency, ID: "CNY"}) ||
		refs[1].Target() != (Target{Kind: KindTaxProfile, ID: "tax", Revision: 1}) || refs[1].Path != "/args/data/tax_profile" {
		t.Fatalf("references %+v", refs)
	}
	cny := currencyFact("CNY", 2)
	if got := code(CheckReferences("policy", p, facts(cny))); got != "REFERENCE_INVALID /args/data/tax_profile" {
		t.Fatal(got)
	}
	superseded := exactFact(KindTaxProfile, "tax", 1, reject(CodeConfigNotActive, "/"))
	if got := code(CheckReferences("policy", p, facts(cny, superseded))); got != "CONFIG_NOT_ACTIVE /args/data/tax_profile" {
		t.Fatal(got)
	}
	if got := code(CheckReferences("policy", p, facts(cny, exactFact(KindTaxProfile, "tax", 1, nil)))); got != "OK" {
		t.Fatal(got)
	}
	app := Content{Kind: KindTaxApplication, ValidFrom: start, TaxApplication: &TaxApplication{Name: "sales", Side: "sales",
		SubjectKind: "fee_category", SubjectID: "freight", TaxProfile: Ref{Kind: "tax_profile", ConfigID: "tax", Revision: 1},
		Jurisdiction: "CN", BasisNote: "synthetic"}}
	if got := code(CheckReferences("app", app, facts(exactFact(KindTaxProfile, "tax", 1, nil)))); got != "REFERENCE_INVALID /args/data/subject_id" {
		t.Fatal("a fee category subject is a configuration:", got)
	}
	app.TaxApplication.SubjectKind, app.TaxApplication.SubjectID = "sku", "sku.1"
	if got := code(CheckReferences("app", app, facts(exactFact(KindTaxProfile, "tax", 1, nil)))); got != "OK" {
		t.Fatal("a SKU is CAT's, not a configuration:", got)
	}
}

// Python test_foundation.py test_number_and_plaintext_template_bind_snapshot (template half): only
// the fixed fields, no format specification, conversion, attribute or unbalanced brace, and the
// declared field list is exactly what the text uses.
func TestPlainTextTemplateNamesFixedFieldsOnly(t *testing.T) {
	quote := "报价 {document.number}：{customer.name}，{quote.total} {quote.currency}"
	fields := []string{"document.number", "customer.name", "quote.total", "quote.currency"}
	for _, c := range []struct {
		content Content
		want    string
	}{
		{template(quote, fields...), "OK"},
		{template("{{literal}} {document.number} {document.number}", "document.number"), "OK"},
		{template("{customer.name.__class__}", "customer.name"), "TEMPLATE_INVALID /args/data/template_text"},
		{template("{document.number:>10}", "document.number"), "TEMPLATE_INVALID /args/data/template_text"},
		{template("{document.number!r}", "document.number"), "TEMPLATE_INVALID /args/data/template_text"},
		{template("{lines[0]}", "lines.summary"), "TEMPLATE_INVALID /args/data/template_text"},
		{template("{}", "document.number"), "TEMPLATE_INVALID /args/data/template_text"},
		{template("{document.number", "document.number"), "TEMPLATE_INVALID /args/data/template_text"},
		{template("document.number}", "document.number"), "TEMPLATE_INVALID /args/data/template_text"},
		{template(quote, "document.number", "customer.name", "quote.total"), "TEMPLATE_INVALID /args/data/field_names"},
		{template("{document.number}", "document.number", "document.date"), "TEMPLATE_INVALID /args/data/field_names"},
	} {
		if got := code(Validate("t", c.content)); got != c.want {
			t.Errorf("%q %v: %s, want %s", c.content.DocumentTemplate.TemplateText, c.content.DocumentTemplate.FieldNames, got, c.want)
		}
	}
	if names, ok := TemplateFieldNames(quote); !ok || !slices.Equal(names, fields) {
		t.Fatal(names, ok)
	}
}

func TestCreditTiersAscendAndFitTheCurrency(t *testing.T) {
	officer := CreditApprovalTier{Level: "credit_officer", MaxAmount: "1000"}
	manager := CreditApprovalTier{Level: "credit_manager", MaxAmount: "10000.00"}
	cny := facts(currencyFact("CNY", 2))
	for _, c := range []struct {
		content Content
		want    string
	}{
		{credit("0", officer, manager), "OK"},
		{credit("0", manager, officer), "INVALID_KNOWN_VALUE /args/data/approval_tiers/1/max_amount"},
		{credit("0", officer, CreditApprovalTier{Level: "credit_officer", MaxAmount: "2000"}), "INVALID_KNOWN_VALUE /args/data/approval_tiers/1/level"},
		{credit("0", CreditApprovalTier{Level: "x", MaxAmount: "0"}), "INVALID_KNOWN_VALUE /args/data/approval_tiers/0/max_amount"},
		{credit("0.001", officer), "INVALID_KNOWN_VALUE /args/data/default_limit"},
		{credit("0", CreditApprovalTier{Level: "x", MaxAmount: "1.005"}), "INVALID_KNOWN_VALUE /args/data/approval_tiers/0/max_amount"},
	} {
		err := Validate("credit", c.content)
		if err == nil {
			err = CheckReferences("credit", c.content, cny)
		}
		if got := code(err); got != c.want {
			t.Errorf("%+v: %s, want %s", c.content.CreditPolicy, got, c.want)
		}
	}
}

// Python test_foundation.py validate (bank_account): a company account belongs to the scope's
// company; a party's account names the party as given (PTY is not upstream of POL).
func TestBankAccountOwner(t *testing.T) {
	account := Content{Kind: KindBankAccount, ValidFrom: start, BankAccount: &BankAccount{OwnerKind: "company", OwnerID: "company.B",
		BankName: "synthetic bank", AccountNumber: "1234567890", Currency: "CNY", Purpose: "receipt"}}
	cny := facts(currencyFact("CNY", 2))
	if got := code(CheckReferences("bank", account, cny)); got != "REFERENCE_INVALID /args/data/owner_id" {
		t.Fatal(got)
	}
	account.BankAccount.OwnerID = "company.A"
	if got := code(CheckReferences("bank", account, cny)); got != "OK" {
		t.Fatal(got)
	}
	account.BankAccount.OwnerKind, account.BankAccount.OwnerID = "party", "customer"
	if got := code(CheckReferences("bank", account, cny)); got != "OK" {
		t.Fatal(got)
	}
}

// Payment terms published before the day basis existed keep their content hash: the basis and the
// calendar are left out of the stored data when they are not declared.
func TestPaymentTermsWithoutDayBasisKeepTheirShape(t *testing.T) {
	raw, err := payment("30", "70").DataJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"terms","stages":[{"stage_id":"s1","percent":"30","trigger":"order_confirmed","days":0},` +
		`{"stage_id":"s2","percent":"70","trigger":"order_confirmed","days":0}]}`
	if string(raw) != want {
		t.Fatalf("%s", raw)
	}
	declared := payment("100")
	declared.PaymentTerms.DayBasis, declared.PaymentTerms.Calendar = "working", &Ref{Kind: "calendar", ConfigID: "cal", Revision: 1}
	refs := declared.References()
	if len(refs) != 1 || refs[0].Target() != (Target{Kind: KindCalendar, ID: "cal", Revision: 1}) || refs[0].Path != "/args/data/calendar" {
		t.Fatalf("%+v", refs)
	}
	if got := code(CheckReferences("payment", declared, facts())); got != "REFERENCE_INVALID /args/data/calendar" {
		t.Fatal(got)
	}
}

func TestEveryKindRoundTripsThroughItsStoredData(t *testing.T) {
	cal := &Ref{Kind: "calendar", ConfigID: "cal", Revision: 2}
	contents := []Content{
		company("company.A"), currency("USD", 2, "USD"), conversion("DOZEN", "PCS", "12"), fx("USD", "CNY", "7.123456", "cost_estimate"),
		{Kind: KindUnit, Unit: &Unit{Code: "PCS", Name: "件", Dimension: "count"}},
		{Kind: KindCalendar, Calendar: &Calendar{TimeZone: "Asia/Shanghai", WorkingWeekdays: []int{1, 2, 3, 4, 5}, Holidays: []string{}}},
		{Kind: KindTradeTerms, TradeTerms: &TradeTerms{Incoterm: "FCA", Edition: "2020", NamedPlace: "TEST", ResponsibilityNote: "x",
			Delivery: Delivery{Kind: "lead_time", AnchorEvent: "technical_approved", Days: 30, DayBasis: "working", Calendar: cal}}},
		payment("100"), tax("0.13", "inclusive"), policy("0.25"),
		{Kind: KindTaxApplication, TaxApplication: &TaxApplication{Name: "n", Side: "sales", SubjectKind: "sku", SubjectID: "s",
			TaxProfile: Ref{Kind: "tax_profile", ConfigID: "tax", Revision: 1}, Jurisdiction: "CN", BasisNote: "b"}},
		{Kind: KindFeeCategory, FeeCategory: &FeeCategory{Name: "包装", Category: "packaging", Side: "purchase_cost", Inclusion: "included_in_line",
			AllocationBasis: "quantity", PolicyBasis: "p"}},
		{Kind: KindBankAccount, BankAccount: &BankAccount{OwnerKind: "company", OwnerID: "company.A", BankName: "b", AccountNumber: "1234",
			Currency: "CNY", Purpose: "both"}},
		{Kind: KindNumberSequence, NumberSequence: &NumberSequence{DocumentType: "quote", Prefix: "QT-", Width: 5, Reset: "annual", TimeZone: "UTC"}},
		template("{document.number}", "document.number"),
		{Kind: KindQuoteDocumentTemplate, QuoteDocumentTemplate: &QuoteDocumentTemplate{Name: "q", Language: "en", RendererVersion: "quote-text/0.1.0",
			Title: "Quotation", Sections: []string{"issuer"}, FooterNote: ""}},
		{Kind: KindOrderDocumentTemplate, OrderDocumentTemplate: &OrderDocumentTemplate{Name: "pi", DocumentKind: "proforma_invoice", Language: "en",
			RendererVersion: "order-text/0.1.0", Title: "Proforma invoice", Sections: []string{"seller"}, Clauses: []string{}, FooterNote: ""}},
		credit("0", CreditApprovalTier{Level: "credit_officer", MaxAmount: "1000"}),
	}
	for _, c := range contents {
		c.ValidFrom = start
		raw, err := c.DataJSON()
		if err != nil {
			t.Fatal(c.Kind, err)
		}
		back := Content{Kind: c.Kind, ValidFrom: start, ValidUntil: c.ValidUntil}
		if err = back.DecodeData(raw); err != nil {
			t.Fatal(c.Kind, err)
		}
		a, _ := c.Hash()
		b, _ := back.Hash()
		if a != b || back.Data() == nil {
			t.Errorf("%s does not round-trip: %s", c.Kind, raw)
		}
	}
	if (Content{Kind: KindCurrency}).Data() != nil {
		t.Fatal("a missing member is no data, not a typed nil")
	}
	if _, ok := kernel.AsRejection(Validate("x", Content{Kind: KindCurrency, ValidFrom: start})); !ok {
		t.Fatal("content without data is refused")
	}
}
