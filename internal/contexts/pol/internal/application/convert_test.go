package application

import (
	"reflect"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/config"
)

// Every kind's contract data survives the trip into domain content and back unchanged, so a stored
// revision reads back as it was saved and hashes as it was saved.
func TestEveryKindConvertsBothWays(t *testing.T) {
	calendar := &api.ConfigRef{Kind: api.KindCalendar, ConfigID: "cal", Revision: 2}
	tax := api.ConfigRef{Kind: api.KindTaxProfile, ConfigID: "tax", Revision: 1}
	for kind, d := range map[string]api.ConfigData{
		api.KindCompanyProfile: {CompanyProfile: &api.CompanyProfile{LegalName: "合成企业", RegistrationNo: "S", Country: "CN", Address: "a",
			BaseCurrency: "CNY", TimeZone: "Asia/Shanghai", PolicyScope: "p"}},
		api.KindCurrency: {Currency: &api.Currency{Code: "USD", Name: "USD", MinorUnits: 2, Rounding: "HALF_UP"}},
		api.KindUnit:     {Unit: &api.Unit{Code: "PCS", Name: "件", Dimension: "count"}},
		api.KindUnitConversion: {UnitConversion: &api.UnitConversion{SkuID: "sku", SpecRevision: 3, FromUnit: "DOZEN", ToUnit: "PCS",
			Factor: "12", Basis: "b"}},
		api.KindFXRate: {FXRate: &api.FXRate{FromCurrency: "USD", ToCurrency: "CNY", Rate: "7.123456", Purpose: "cost_estimate",
			SourceName: "s", ObservedAt: "2026-10-22T00:00:00.000000Z"}},
		api.KindCalendar: {Calendar: &api.Calendar{TimeZone: "Asia/Shanghai", WorkingWeekdays: []int{1, 2, 3, 4, 5}, Holidays: []string{}}},
		api.KindTradeTerms: {TradeTerms: &api.TradeTerms{Incoterm: "FCA", Edition: "2020", NamedPlace: "n", ResponsibilityNote: "r",
			Delivery: api.Delivery{Kind: "lead_time", AnchorEvent: "technical_approved", Days: 30, DayBasis: "working", Calendar: calendar}}},
		api.KindPaymentTerms: {PaymentTerms: &api.PaymentTerms{Name: "n", Stages: []api.PaymentStage{{StageID: "s", Percent: "100",
			Trigger: "before_shipment"}}, DayBasis: "working", Calendar: calendar}},
		api.KindTaxProfile: {TaxProfile: &api.TaxProfile{Name: "n", Rate: "0.13", PriceBasis: "inclusive", RecoveryTreatment: "unknown",
			PolicyBasis: "p"}},
		api.KindTaxApplication: {TaxApplication: &api.TaxApplication{Name: "n", Side: "sales", SubjectKind: "fee_category", SubjectID: "freight",
			PartyID: "customer", TaxProfile: tax, Jurisdiction: "CN", BasisNote: "b"}},
		api.KindFeeCategory: {FeeCategory: &api.FeeCategory{Name: "n", Category: "freight", Side: "sales_charge", Inclusion: "separate",
			AllocationBasis: "value", PolicyBasis: "p"}},
		api.KindPricingPolicy: {PricingPolicy: &api.PricingPolicy{Name: "n", CalculationCurrency: "CNY", MinimumMargin: "0.25", QuoteValidDays: 7,
			MaxFXAgeSeconds: 86400, TaxProfile: tax}},
		api.KindBankAccount: {BankAccount: &api.BankAccount{OwnerKind: "party", OwnerID: "customer", BankName: "b", AccountNumber: "SECRET12345678",
			Currency: "CNY", Purpose: "receipt"}},
		api.KindNumberSequence: {NumberSequence: &api.NumberSequence{DocumentType: "quote", Prefix: "QT-", Width: 5, Reset: "annual", TimeZone: "UTC"}},
		api.KindDocumentTemplate: {DocumentTemplate: &api.DocumentTemplate{Name: "n", DocumentType: "quote", TemplateText: "{document.number}",
			FieldNames: []string{"document.number"}}},
		api.KindQuoteDocumentTemplate: {QuoteDocumentTemplate: &api.QuoteDocumentTemplate{Name: "n", Language: "en",
			RendererVersion: "quote-text/0.1.0", Title: "Quotation", Sections: api.QuoteSections(), FooterNote: "f"}},
		api.KindCreditPolicy: {CreditPolicy: &api.CreditPolicy{Name: "n", Currency: "CNY", DefaultLimit: "0", LimitSources: []string{"internal_assessment"},
			ApprovalTiers: []api.CreditApprovalTier{{Level: "officer", MaxAmount: "1000"}}, OverdueToleranceDays: 30}},
		api.KindLegalTerms: {LegalTerms: &api.LegalTerms{Name: "n", GoverningLaw: "DE", CISG: "applies", FormationRule: "arrival",
			DisputeResolution: api.DisputeResolution{Method: "arbitration", Institution: "i", Seat: "Hamburg", Language: "de"}}},
		"company_profile with its default legal terms": {CompanyProfile: &api.CompanyProfile{LegalName: "合成企业", RegistrationNo: "S",
			Country: "CN", Address: "a", BaseCurrency: "CNY", TimeZone: "Asia/Shanghai", PolicyScope: "p",
			DefaultLegalTerms: &api.ConfigRef{Kind: api.KindLegalTerms, ConfigID: "legal.default", Revision: 3}}},
	} {
		if d.CompanyProfile != nil {
			kind = api.KindCompanyProfile
		}
		c := content(api.SaveDraft{Kind: kind, ValidFrom: "2026-10-22T00:00:00Z", ValidUntil: "2026-10-23T00:00:00Z", Data: d})
		raw, err := c.DataJSON()
		if err != nil {
			t.Fatal(kind, err)
		}
		stored := config.Content{Kind: c.Kind, ValidFrom: c.ValidFrom, ValidUntil: c.ValidUntil}
		if err = stored.DecodeData(raw); err != nil {
			t.Fatal(kind, err)
		}
		if back := data(stored); !reflect.DeepEqual(back, d) {
			t.Errorf("%s: %+v came back as %+v", kind, d, back)
		}
	}
}

func TestCalendarIsStoredInAscendingOrder(t *testing.T) {
	c := content(api.SaveDraft{Kind: api.KindCalendar, ValidFrom: "2026-10-22T00:00:00Z", Data: api.ConfigData{Calendar: &api.Calendar{
		TimeZone: "Asia/Shanghai", WorkingWeekdays: []int{5, 1, 3}, Holidays: []string{"2026-10-02", "2026-10-01"}}}})
	if !slices.Equal(c.Calendar.WorkingWeekdays, []int{1, 3, 5}) || !slices.Equal(c.Calendar.Holidays, []string{"2026-10-01", "2026-10-02"}) {
		t.Fatalf("%+v", c.Calendar)
	}
	empty := content(api.SaveDraft{Kind: api.KindCalendar, ValidFrom: "2026-10-22T00:00:00Z", Data: api.ConfigData{Calendar: &api.Calendar{
		TimeZone: "UTC", WorkingWeekdays: []int{1}}}})
	if raw, _ := empty.DataJSON(); string(raw) != `{"time_zone":"UTC","working_weekdays":[1],"holidays":[]}` {
		t.Fatalf("%s", raw)
	}
}

// The domain judges templates by the same fixed fields the contract admits.
func TestTemplateFieldsAreTheContractsFields(t *testing.T) {
	if !slices.Equal(api.TemplateFields(), config.TemplateFields) {
		t.Fatalf("api %v, domain %v", api.TemplateFields(), config.TemplateFields)
	}
}

// Python test_foundation.py test_sensitive_account_field_requires_separate_grant: readers without
// the separate grant see the last four characters only.
func TestMaskKeepsTheLastFourCharacters(t *testing.T) {
	for in, want := range map[string]string{"1234567890": "******7890", "SECRET12345678": "**********5678", "1234": "1234", "账户号码1234": "****1234"} {
		if got := mask(in); got != want {
			t.Errorf("mask(%q) = %q, want %q", in, got, want)
		}
	}
}
