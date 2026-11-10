package api

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// save is a save of data as kind, bounded like every exchange rate.
func save(kind string, data ConfigData) SaveDraft {
	return SaveDraft{IdempotencyKey: "k1", Kind: kind, ConfigID: "c1", ValidFrom: "2026-10-22T00:00:00Z", ValidUntil: "2026-10-23T00:00:00Z",
		Data: data}
}

var calendarRef = &ConfigRef{Kind: KindCalendar, ConfigID: "cal", Revision: 1}

// valid returns well-formed data of every implemented kind.
func valid() map[string]ConfigData {
	return map[string]ConfigData{
		KindCompanyProfile: {CompanyProfile: &CompanyProfile{LegalName: "合成企业", RegistrationNo: "SYNTHETIC", Country: "CN", Address: "合成地址",
			BaseCurrency: "CNY", TimeZone: "Asia/Shanghai", PolicyScope: "synthetic"}},
		KindCurrency: {Currency: &Currency{Code: "USD", Name: "US dollar", MinorUnits: 2, Rounding: RoundingHalfUp}},
		KindUnit:     {Unit: &Unit{Code: "PCS", Name: "件", Dimension: DimensionCount}},
		KindUnitConversion: {UnitConversion: &UnitConversion{SkuID: "sku", SpecRevision: 1, FromUnit: "DOZEN", ToUnit: "PCS", Factor: "12",
			Basis: "packaging"}},
		KindFXRate: {FXRate: &FXRate{FromCurrency: "USD", ToCurrency: "CNY", Rate: "7.123456", Purpose: PurposeCostEstimate,
			SourceName: "SYNTHETIC RATE", ObservedAt: "2026-10-22T00:00:00Z"}},
		KindCalendar: {Calendar: &Calendar{TimeZone: "Asia/Shanghai", WorkingWeekdays: []int{1, 2, 3, 4, 5}, Holidays: []string{"2026-10-01"}}},
		KindTradeTerms: {TradeTerms: &TradeTerms{Incoterm: "FCA", Edition: IncotermsEdition2020, NamedPlace: "TEST", ResponsibilityNote: "note",
			Delivery: Delivery{Kind: DeliveryLeadTime, AnchorEvent: AnchorTechnicalApproved, Days: 30, DayBasis: DayBasisWorking, Calendar: calendarRef}}},
		KindPaymentTerms: {PaymentTerms: &PaymentTerms{Name: "30/70", Stages: []PaymentStage{{StageID: "deposit", Percent: "30",
			Trigger: TriggerOrderConfirmed}, {StageID: "balance", Percent: "70", Trigger: TriggerBeforeShipment}}}},
		KindTaxProfile: {TaxProfile: &TaxProfile{Name: "SYNTHETIC TAX", Rate: "0.13", PriceBasis: PriceBasisInclusive,
			RecoveryTreatment: RecoveryNonrecoverable, PolicyBasis: "test only"}},
		KindTaxApplication: {TaxApplication: &TaxApplication{Name: "sales", Side: TaxSideSales, SubjectKind: SubjectSKU, SubjectID: "sku",
			TaxProfile: ConfigRef{Kind: KindTaxProfile, ConfigID: "tax", Revision: 1}, Jurisdiction: "CN", BasisNote: "synthetic"}},
		KindFeeCategory: {FeeCategory: &FeeCategory{Name: "包装", Category: "packaging", Side: FeeSidePurchaseCost, Inclusion: InclusionIncludedInLine,
			AllocationBasis: AllocationQuantity, PolicyBasis: "synthetic"}},
		KindPricingPolicy: {PricingPolicy: &PricingPolicy{Name: "test", CalculationCurrency: "CNY", MinimumMargin: "0.25", QuoteValidDays: 7,
			MaxFXAgeSeconds: 86400, TaxProfile: ConfigRef{Kind: KindTaxProfile, ConfigID: "tax", Revision: 1}}},
		KindBankAccount: {BankAccount: &BankAccount{OwnerKind: OwnerCompany, OwnerID: "company.A", BankName: "synthetic bank",
			AccountNumber: "1234567890", Currency: "CNY", Purpose: AccountReceipt}},
		KindNumberSequence: {NumberSequence: &NumberSequence{DocumentType: DocumentTypeQuote, Prefix: "QT-", Width: 5, Reset: ResetAnnual,
			TimeZone: "Asia/Shanghai"}},
		KindDocumentTemplate: {DocumentTemplate: &DocumentTemplate{Name: "quote", DocumentType: DocumentTypeQuote,
			TemplateText: "报价 {document.number}", FieldNames: []string{"document.number"}}},
		KindQuoteDocumentTemplate: {QuoteDocumentTemplate: &QuoteDocumentTemplate{Name: "quote", Language: LanguageZhCN,
			RendererVersion: QuoteRendererVersion, Title: "报价单", Sections: QuoteSections(), FooterNote: ""}},
		KindCreditPolicy: {CreditPolicy: &CreditPolicy{Name: "credit", Currency: "CNY", DefaultLimit: "0",
			LimitSources:         []string{LimitSourceInternalAssessment, LimitSourceSinosureBuyerLimit},
			ApprovalTiers:        []CreditApprovalTier{{Level: "credit_officer", MaxAmount: "1000"}, {Level: "credit_manager", MaxAmount: "10000.00"}},
			OverdueToleranceDays: 30}},
		KindScreeningRegulation: {ScreeningRegulation: &ScreeningRegulation{RescreenIntervalDays: 90,
			Lists: []ScreeningList{{ListCode: "cn.export_control", Jurisdiction: JurisdictionCN},
				{ListCode: "eu.consolidated", Jurisdiction: JurisdictionForeign, LegalConfirmationEvidenceID: "src.legal.1"}}}},
	}
}

func TestEveryImplementedKindHasAContract(t *testing.T) {
	data := valid()
	for _, kind := range kinds {
		d, ok := data[kind]
		if !ok || !KindImplemented(kind) {
			t.Fatalf("%s has no example or is not implemented", kind)
		}
		if got := code(save(kind, d).Validate()); got != "OK" {
			t.Errorf("%s: %s", kind, got)
		}
		// The member of another kind is never accepted.
		for other, od := range data {
			if other != kind {
				if got := code(save(kind, od).Validate()); got != "SCHEMA_INVALID /args/data" {
					t.Fatalf("%s with %s data: %s", kind, other, got)
				}
				break
			}
		}
	}
}

func TestSecondBatchContracts(t *testing.T) {
	for _, c := range []struct {
		kind string
		edit func(*ConfigData)
		want string
	}{
		{KindCompanyProfile, func(d *ConfigData) { d.CompanyProfile.Country = "cn" }, "SCHEMA_INVALID /args/data/country"},
		{KindUnitConversion, func(d *ConfigData) { d.UnitConversion.SpecRevision = 0 }, "SCHEMA_INVALID /args/data/spec_revision"},
		{KindUnitConversion, func(d *ConfigData) { d.UnitConversion.Factor = "1e1" }, "SCHEMA_INVALID /args/data/factor"},
		{KindCalendar, func(d *ConfigData) { d.Calendar.WorkingWeekdays = []int{1, 1} }, "SCHEMA_INVALID /args/data/working_weekdays/1"},
		{KindCalendar, func(d *ConfigData) { d.Calendar.WorkingWeekdays = []int{8} }, "SCHEMA_INVALID /args/data/working_weekdays/0"},
		{KindCalendar, func(d *ConfigData) { d.Calendar.Holidays = []string{"2026-02-30"} }, "SCHEMA_INVALID /args/data/holidays/0"},
		{KindCalendar, func(d *ConfigData) { d.Calendar.Holidays = nil }, "OK"},
		{KindTradeTerms, func(d *ConfigData) { d.TradeTerms.Incoterm = "FOB2020" }, "SCHEMA_INVALID /args/data/incoterm"},
		{KindTradeTerms, func(d *ConfigData) { d.TradeTerms.Delivery.Calendar = nil }, "SCHEMA_INVALID /args/data/delivery/calendar"},
		{KindTradeTerms, func(d *ConfigData) {
			d.TradeTerms.Delivery.Calendar = &ConfigRef{Kind: KindUnit, ConfigID: "PCS", Revision: 1}
		},
			"SCHEMA_INVALID /args/data/delivery/calendar/kind"},
		{KindTradeTerms, func(d *ConfigData) { d.TradeTerms.Delivery = Delivery{Kind: DeliveryFixedDate, Date: "2026-12-01"} }, "OK"},
		{KindTradeTerms, func(d *ConfigData) {
			d.TradeTerms.Delivery = Delivery{Kind: DeliveryFixedDate, Date: "2026-12-01", Days: 3}
		},
			"SCHEMA_INVALID /args/data/delivery"},
		{KindPaymentTerms, func(d *ConfigData) { d.PaymentTerms.DayBasis = DayBasisWorking }, "SCHEMA_INVALID /args/data/calendar"},
		{KindPaymentTerms, func(d *ConfigData) { d.PaymentTerms.Calendar = calendarRef }, "SCHEMA_INVALID /args/data/day_basis"},
		{KindPaymentTerms, func(d *ConfigData) { d.PaymentTerms.DayBasis, d.PaymentTerms.Calendar = DayBasisWorking, calendarRef }, "OK"},
		{KindTaxProfile, func(d *ConfigData) { d.TaxProfile.RecoveryTreatment = "maybe" }, "SCHEMA_INVALID /args/data/recovery_treatment"},
		{KindTaxApplication, func(d *ConfigData) { d.TaxApplication.TaxProfile.Kind = KindCalendar }, "SCHEMA_INVALID /args/data/tax_profile/kind"},
		{KindTaxApplication, func(d *ConfigData) { d.TaxApplication.PartyID = "customer.1" }, "OK"},
		{KindFeeCategory, func(d *ConfigData) { d.FeeCategory.Inclusion = "both" }, "SCHEMA_INVALID /args/data/inclusion"},
		{KindPricingPolicy, func(d *ConfigData) { d.PricingPolicy.MaxFXAgeSeconds = 0 }, "SCHEMA_INVALID /args/data/max_fx_age_seconds"},
		{KindPricingPolicy, func(d *ConfigData) { d.PricingPolicy.TaxProfile.Revision = 0 }, "SCHEMA_INVALID /args/data/tax_profile/revision"},
		{KindBankAccount, func(d *ConfigData) { d.BankAccount.AccountNumber = "123" }, "SCHEMA_INVALID /args/data/account_number"},
		{KindBankAccount, func(d *ConfigData) { d.BankAccount.AccountNumber = "1234\n5678" }, "SCHEMA_INVALID /args/data/account_number"},
		{KindDocumentTemplate, func(d *ConfigData) { d.DocumentTemplate.FieldNames = []string{"customer.secret"} },
			"SCHEMA_INVALID /args/data/field_names/0"},
		{KindQuoteDocumentTemplate, func(d *ConfigData) { d.QuoteDocumentTemplate.Sections = d.QuoteDocumentTemplate.Sections[1:] },
			"SCHEMA_INVALID /args/data/sections"},
		{KindQuoteDocumentTemplate, func(d *ConfigData) { slices.Reverse(d.QuoteDocumentTemplate.Sections) }, "OK"},
		{KindQuoteDocumentTemplate, func(d *ConfigData) { d.QuoteDocumentTemplate.FooterNote = strings.Repeat("注", 5001) },
			"SCHEMA_INVALID /args/data/footer_note"},
		{KindCreditPolicy, func(d *ConfigData) { d.CreditPolicy.LimitSources = []string{"guess"} }, "SCHEMA_INVALID /args/data/limit_sources/0"},
		{KindCreditPolicy, func(d *ConfigData) { d.CreditPolicy.ApprovalTiers = nil }, "SCHEMA_INVALID /args/data/approval_tiers"},
		{KindCreditPolicy, func(d *ConfigData) { d.CreditPolicy.DefaultLimit = "-1" }, "SCHEMA_INVALID /args/data/default_limit"},
	} {
		d := valid()[c.kind]
		c.edit(&d)
		if got := code(save(c.kind, d).Validate()); got != c.want {
			t.Errorf("%s: %s, want %s", c.kind, got, c.want)
		}
	}
	if !slices.Equal(TemplateFields(), templateFields) || len(QuoteSections()) != 10 {
		t.Fatal("fixed lists")
	}
}

func TestLookupContracts(t *testing.T) {
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	tax := ConfigRef{Kind: KindTaxProfile, ConfigID: "tax", Revision: 1}
	if got := code(TaxQuery{Profile: tax, Currency: "CNY", At: at}.Validate()); got != "OK" {
		t.Fatal(got)
	}
	if got := code(TaxQuery{Profile: ConfigRef{Kind: KindCurrency, ConfigID: "CNY", Revision: 1}, Currency: "CNY", At: at}.Validate()); got != "SCHEMA_INVALID /profile/kind" {
		t.Fatal(got)
	}
	conversion := ConfigRef{Kind: KindUnitConversion, ConfigID: "dozen.pcs", Revision: 1}
	if got := code(UnitQuery{Conversion: conversion, SkuID: "sku", SpecRevision: 0, At: at}.Validate()); got != "SCHEMA_INVALID /spec_revision" {
		t.Fatal(got)
	}
	policy := ConfigRef{Kind: KindTaxProfile, ConfigID: "tax", Revision: 1}
	q := FXQuery{FromCurrency: "USD", ToCurrency: "CNY", Purpose: PurposeCostEstimate, At: at, Policy: &policy}
	if got := code(q.Validate()); got != "SCHEMA_INVALID /policy/kind" {
		t.Fatal(got)
	}
}

// Python test_foundation.py test_tax_split_is_exact_and_unknown_recovery_blocks (split half):
// 113 inclusive of 13 % is 100.00 + 13.00.
func TestTaxSplitIsExact(t *testing.T) {
	cny := kernel.MustCurrency("CNY", 2)
	inclusive := TaxRate{Currency: cny, Rate: kernel.MustDecimal("0.13"), PriceBasis: PriceBasisInclusive}
	s, err := inclusive.Split(kernel.MustDecimal("113"))
	if err != nil || s.Net.Amount() != "100.00" || s.Tax.Amount() != "13.00" || s.Gross.Amount() != "113.00" {
		t.Fatalf("%+v %v", s, err)
	}
	exclusive := inclusive
	exclusive.PriceBasis = PriceBasisExclusive
	if s, _ = exclusive.Split(kernel.MustDecimal("100")); s.Gross.Amount() != "113.00" {
		t.Fatal(s.Gross)
	}
	none := TaxRate{Currency: cny, Rate: kernel.DecimalFromInt(0), PriceBasis: PriceBasisNotApplicable}
	if s, _ = none.Split(kernel.MustDecimal("10.005")); s.Net.Amount() != "10.01" || !s.Tax.IsZero() {
		t.Fatal(s)
	}
	if _, err = inclusive.Split(kernel.MustDecimal("-1")); code(err) != "INVALID_KNOWN_VALUE /amount" {
		t.Fatal(code(err))
	}
}

// Python test_foundation.py test_unit_conversion_is_sku_scoped_and_exact (conversion half): 2 DOZEN
// are 24 PCS; 0.1 DOZEN would be 1.2 PCS, which a unit of precision 0 cannot hold.
func TestQuantityConversionIsExact(t *testing.T) {
	c := QuantityConversion{From: "DOZEN", To: "PCS", Factor: kernel.DecimalFromInt(12), ToPrecision: 0}
	two, _ := kernel.ParseQuantity("2", "DOZEN")
	got, err := c.Convert(two)
	if err != nil || got.String() != "24 PCS" {
		t.Fatal(got, err)
	}
	tenth, _ := kernel.ParseQuantity("0.1", "DOZEN")
	if _, err = c.Convert(tenth); code(err) != "INVALID_KNOWN_VALUE /quantity" {
		t.Fatal(code(err))
	}
	pcs, _ := kernel.ParseQuantity("2", "PCS")
	if _, err = c.Convert(pcs); code(err) != "UNIT_SCOPE_MISMATCH /unit" {
		t.Fatal(code(err))
	}
	zero, _ := kernel.ParseQuantity("0", "DOZEN")
	if _, err = c.Convert(zero); code(err) != "INVALID_KNOWN_VALUE /quantity" {
		t.Fatal(code(err))
	}
}

func TestConfigGuardVerdict(t *testing.T) {
	ref := ConfigRef{Kind: KindFXRate, ConfigID: "usd.cny", Revision: 1}
	args := ConfigGuardArgs(ref, time.Date(2026, 10, 22, 9, 0, 0, 1500, time.FixedZone("CST", 8*3600)))
	if len(args) != 4 || args[0] != KindFXRate || args[2] != int64(1) || !args[3].(time.Time).Equal(time.Date(2026, 10, 22, 1, 0, 0, 1000, time.UTC)) {
		t.Fatalf("args %v", args)
	}
	row := func(values ...any) func(dest ...any) error {
		return func(dest ...any) error {
			*dest[0].(*bool) = values[0].(bool)
			for i := 1; i < 4; i++ {
				if s, ok := values[i].(string); ok {
					*dest[i].(**string) = &s
				}
			}
			return nil
		}
	}
	for _, c := range []struct {
		scan func(dest ...any) error
		want string
	}{
		{row(true, "active", nil, "hash"), "OK"},
		{row(true, "retired", CodeConfigNotActive, "hash"), "CONFIG_NOT_ACTIVE /args/rate"},
		{row(true, "active", CodeConfigExpired, "hash"), "CONFIG_EXPIRED /args/rate"},
		{row(false, nil, kernel.CodeReferenceInvalid, nil), "REFERENCE_INVALID /args/rate"},
	} {
		v, err := ScanConfigGuard(ref, c.scan)
		if err != nil {
			t.Fatal(err)
		}
		if got := code(v.Check("/args/rate")); got != c.want || v.InForce() != (c.want == "OK") || v.Ref != ref {
			t.Errorf("%+v: %s, want %s", v, got, c.want)
		}
	}
	broken := errors.New("connection lost")
	if _, err := ScanConfigGuard(ref, func(...any) error { return broken }); !errors.Is(err, broken) {
		t.Fatal(err)
	}
}
