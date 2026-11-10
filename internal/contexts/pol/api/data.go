package api

import (
	"slices"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Closed values of configuration data, beyond those in api.go.
const (
	DayBasisCalendar = "calendar"
	DayBasisWorking  = "working"

	DeliveryFixedDate = "fixed_date"
	DeliveryLeadTime  = "lead_time"

	AnchorOrderConfirmed    = "order_confirmed"
	AnchorDepositReceived   = "deposit_received"
	AnchorTechnicalApproved = "technical_approved"

	IncotermsEdition2020 = "2020"

	PriceBasisInclusive     = "inclusive"
	PriceBasisExclusive     = "exclusive"
	PriceBasisNotApplicable = "not_applicable"

	RecoveryUnknown        = "unknown"
	RecoveryRecoverable    = "recoverable"
	RecoveryNonrecoverable = "nonrecoverable"

	TaxSideSales    = "sales"
	TaxSidePurchase = "purchase"

	SubjectSKU         = "sku"
	SubjectFeeCategory = "fee_category"

	FeeSidePurchaseCost     = "purchase_cost"
	FeeSideSalesCharge      = "sales_charge"
	InclusionSeparate       = "separate"
	InclusionIncludedInLine = "included_in_line"
	AllocationDocument      = "document"
	AllocationQuantity      = "quantity"
	AllocationValue         = "value"

	OwnerCompany = "company"
	OwnerParty   = "party"

	AccountReceipt = "receipt"
	AccountPayment = "payment"
	AccountBoth    = "both"

	LanguageZhCN         = "zh-CN"
	LanguageEn           = "en"
	QuoteRendererVersion = "quote-text/0.1.0"

	LimitSourceInternalAssessment = "internal_assessment"
	LimitSourceSinosureBuyerLimit = "sinosure_buyer_limit"
)

// Limits of configuration data.
const (
	MaxHolidays        = 3660
	MaxCreditTiers     = 10
	maxDays            = 3650
	maxQuoteValidDays  = 365
	maxFXAgeSeconds    = 31536000
	maxTitleCharacters = 2000
	maxNoteCharacters  = 5000
	maxTemplateText    = 50000
	minAccountNumber   = 4
	maxAccountNumber   = 64
)

var (
	incoterms      = []string{"EXW", "FCA", "CPT", "CIP", "DAP", "DPU", "DDP", "FAS", "FOB", "CFR", "CIF"}
	anchors        = []string{AnchorOrderConfirmed, AnchorDepositReceived, AnchorTechnicalApproved}
	dayBases       = []string{DayBasisCalendar, DayBasisWorking}
	priceBases     = []string{PriceBasisInclusive, PriceBasisExclusive, PriceBasisNotApplicable}
	recoveries     = []string{RecoveryUnknown, RecoveryRecoverable, RecoveryNonrecoverable}
	taxSides       = []string{TaxSideSales, TaxSidePurchase}
	subjects       = []string{SubjectSKU, SubjectFeeCategory}
	feeCategories  = []string{"goods", "freight", "insurance", "packaging", "inspection", "commission", "bank", "other"}
	feeSides       = []string{FeeSidePurchaseCost, FeeSideSalesCharge}
	inclusions     = []string{InclusionSeparate, InclusionIncludedInLine}
	allocations    = []string{AllocationDocument, AllocationQuantity, AllocationValue}
	owners         = []string{OwnerCompany, OwnerParty}
	accountUses    = []string{AccountReceipt, AccountPayment, AccountBoth}
	languages      = []string{LanguageZhCN, LanguageEn}
	limitSources   = []string{LimitSourceInternalAssessment, LimitSourceSinosureBuyerLimit}
	templateFields = []string{"company.name", "customer.name", "document.number", "document.date", "lines.summary", "quote.total", "quote.currency"}
	quoteSections  = []string{"issuer", "recipient", "identity", "lines", "totals", "terms", "delivery", "payments", "validity", "notes"}
)

// AmountFormat is the lexical form of a credit amount before its currency's minor units are known:
// at most 4 fractional digits, and |minor| < 10^24.
var AmountFormat = kernel.DecimalFormat{IntDigits: kernel.MaxMinorDigits - 4, Scale: 4}

// TemplateFields returns the fields a plain-text document template may name.
func TemplateFields() []string { return slices.Clone(templateFields) }

// QuoteSections returns the ten sections of a quote document template.
func QuoteSections() []string { return slices.Clone(quoteSections) }

// ConfigData is the content of one configuration kind; exactly the member of the revision's kind
// is set. Decimals are strings in their contract format, never floats. POL stores every member as
// JSON whose names are the snake_case of the Go field names (MaxFXAgeSeconds is
// max_fx_age_seconds), except a ConfigRef inside data, which is {"kind", "id", "revision"} as
// everywhere in the contract; transports use the same names.
type ConfigData struct {
	CompanyProfile        *CompanyProfile
	Currency              *Currency
	Unit                  *Unit
	UnitConversion        *UnitConversion
	FXRate                *FXRate
	Calendar              *Calendar
	TradeTerms            *TradeTerms
	PaymentTerms          *PaymentTerms
	TaxProfile            *TaxProfile
	TaxApplication        *TaxApplication
	FeeCategory           *FeeCategory
	PricingPolicy         *PricingPolicy
	BankAccount           *BankAccount
	NumberSequence        *NumberSequence
	DocumentTemplate      *DocumentTemplate
	QuoteDocumentTemplate *QuoteDocumentTemplate
	CreditPolicy          *CreditPolicy
	ScreeningRegulation   *ScreeningRegulation
}

// CompanyProfile is the company of the scope; its config_id is the company's ID (at most
// MaxConfigIDLength characters), so a company has one profile.
type CompanyProfile struct {
	LegalName      string
	RegistrationNo string
	Country        string // ISO 3166-1 alpha-2: ^[A-Z]{2}$
	Address        string
	BaseCurrency   string // currency code, configured and in force
	TimeZone       string // IANA time zone, such as Asia/Shanghai
	PolicyScope    string
}

// Currency is an ISO 4217 currency; its config_id is its code. Code, minor units and rounding are
// its meaning, which never changes once published (POL-INV-002).
type Currency struct {
	Code       string // ^[A-Z]{3}$
	Name       string
	MinorUnits int    // 0–4
	Rounding   string // HALF_UP
}

// Unit is a unit of measure; its config_id is its code. Code, dimension and precision are its
// meaning (POL-INV-002).
type Unit struct {
	Code      string // kernel.UnitCode: 1–32 of A–Z, 0–9 and '-'
	Name      string
	Dimension string // count, mass, length or volume
	Precision int    // fractional digits, 0–6
}

// UnitConversion is how many ToUnit make one FromUnit for one SKU at one specification revision; a
// packaging conversion never applies to other SKUs. The SKU is CAT's: CAT is not upstream of POL,
// so POL stores SkuID and SpecRevision as given and the consuming context checks them against CAT.
type UnitConversion struct {
	SkuID        string // identifier
	SpecRevision int64  // the CAT specification revision the factor was verified for, 1 or more
	FromUnit     string // unit code, configured and in force
	ToUnit       string // unit code of the same dimension, configured and in force
	Factor       string // kernel.FactorFormat, positive: one FromUnit is Factor ToUnit
	Basis        string // what the factor was verified against, such as the packaging specification
}

// FXRate is an exchange rate for one direction and one purpose: one unit of FromCurrency is Rate
// units of ToCurrency (POL-INV-003). Its validity interval is the revision's and must be bounded.
type FXRate struct {
	FromCurrency string // currency code
	ToCurrency   string
	Rate         string // kernel.FactorFormat, positive
	Purpose      string // cost_estimate or quotation
	SourceName   string
	ObservedAt   string // UTC timestamp, not after the revision's ValidFrom
}

// Calendar is a working calendar. POL stores weekdays and holidays in ascending order.
type Calendar struct {
	TimeZone        string   // IANA time zone
	WorkingWeekdays []int    // ISO weekdays 1 (Monday) – 7 (Sunday), 1–7 distinct
	Holidays        []string // dates YYYY-MM-DD, 0–3660 distinct
}

// TradeTerms are an Incoterms 2020 rule with its named place and how delivery is dated.
type TradeTerms struct {
	Incoterm           string // EXW, FCA, CPT, CIP, DAP, DPU, DDP, FAS, FOB, CFR or CIF
	Edition            string // 2020
	NamedPlace         string
	ResponsibilityNote string
	Delivery           Delivery
}

// Delivery is a fixed date (Kind fixed_date with Date), or a lead time (Kind lead_time) of Days
// after AnchorEvent counted on DayBasis in the Calendar revision; the other kind's fields stay
// empty.
type Delivery struct {
	Kind        string     // fixed_date or lead_time
	Date        string     // fixed_date: YYYY-MM-DD
	AnchorEvent string     // lead_time: order_confirmed, deposit_received or technical_approved
	Days        int        // lead_time: 0–3650
	DayBasis    string     // lead_time: calendar or working
	Calendar    *ConfigRef // lead_time: a calendar revision in force
}

// PaymentTerms are payment stages whose percentages are positive and sum to exactly 100
// (POL-INV-004). Days count on DayBasis in the Calendar revision when one is declared: working
// days need a calendar; without a DayBasis they are calendar days and no calendar is declared.
type PaymentTerms struct {
	Name     string
	Stages   []PaymentStage
	DayBasis string     // "", calendar or working
	Calendar *ConfigRef // a calendar revision in force; required for working days
}

// PaymentStage is one stage of PaymentTerms.
type PaymentStage struct {
	StageID string
	Percent string // PercentFormat
	Trigger string // order_confirmed, before_shipment, after_shipment or after_invoice
	Days    int    // 0–3650
}

// TaxProfile is a tax rate, the price basis amounts are quoted on and whether the tax is
// recoverable. A calculation refuses an unknown recovery treatment (TAX_POLICY_UNCONFIRMED). Rates
// are synthetic test parameters until a human publishes a verified one.
type TaxProfile struct {
	Name              string
	Rate              string // kernel.FactorFormat, 0–1; 0 when PriceBasis is not_applicable
	PriceBasis        string // inclusive, exclusive or not_applicable
	RecoveryTreatment string // unknown, recoverable or nonrecoverable
	PolicyBasis       string
}

// TaxApplication is the verified basis on which a tax profile applies to one side of trade and one
// subject, optionally for one party. A purchase-side application never answers for a sale: the
// consuming context checks Side. A SKU (CAT) and a party (PTY) are stored as given; a fee category
// must be configured and in force.
type TaxApplication struct {
	Name         string
	Side         string    // sales or purchase
	SubjectKind  string    // sku or fee_category
	SubjectID    string    // identifier: a SKU ID or a fee_category config_id
	PartyID      string    // "" for every party, or a party ID
	TaxProfile   ConfigRef // a tax_profile revision in force
	Jurisdiction string
	BasisNote    string
}

// FeeCategory classifies a fee: its category, which side of trade it belongs to, whether it is part
// of a line price or charged separately, and how it is allocated.
type FeeCategory struct {
	Name            string
	Category        string // goods, freight, insurance, packaging, inspection, commission, bank or other
	Side            string // purchase_cost or sales_charge
	Inclusion       string // separate or included_in_line
	AllocationBasis string // document, quantity or value
	PolicyBasis     string
}

// PricingPolicy is the basis of cost estimates and quotations.
type PricingPolicy struct {
	Name                string
	CalculationCurrency string    // currency code, configured and in force
	MinimumMargin       string    // kernel.FactorFormat, 0–1
	QuoteValidDays      int       // 1–365
	MaxFXAgeSeconds     int64     // 1–31536000: how long before use an exchange rate may be observed
	TaxProfile          ConfigRef // a tax_profile revision in force
}

// BankAccount is an account of the company or of a party. The account number is sensitive: readers
// without pol.read_sensitive see only its last four characters, and only company-wide readers see
// the accounts of parties (POL cannot ask PTY who owns a party).
type BankAccount struct {
	OwnerKind     string // company or party
	OwnerID       string // the scope's company, or a party ID stored as given
	BankName      string
	AccountNumber string // 4–64 characters
	Currency      string // currency code, configured and in force
	Purpose       string // receipt, payment or both
}

// NumberSequence is how document numbers of one document type are formed: Prefix, the year in
// TimeZone, '-' and the yearly counter zero-padded to Width digits, such as QT-2026-00001.
type NumberSequence struct {
	DocumentType string // rfq, quote, order, purchase_order or invoice
	Prefix       string // ^[A-Z0-9][A-Z0-9-]{0,15}$
	Width        int    // 4–12
	Reset        string // annual
	TimeZone     string // IANA time zone the year is counted in, such as Asia/Shanghai
}

// DocumentTemplate is a plain-text template: literal text, "{{" and "}}" for braces and
// "{field}" placeholders naming exactly the fields of FieldNames, each one of TemplateFields. Any
// other placeholder is TEMPLATE_INVALID; nothing in a template is ever evaluated.
type DocumentTemplate struct {
	Name         string
	DocumentType string   // rfq, quote, order, purchase_order or invoice
	TemplateText string   // 1–50000 characters
	FieldNames   []string // 1–7 distinct TemplateFields
}

// QuoteDocumentTemplate is the layout of a formal quotation for the fixed quote-text renderer.
type QuoteDocumentTemplate struct {
	Name            string
	Language        string   // zh-CN or en
	RendererVersion string   // quote-text/0.1.0
	Title           string   // 1–2000 characters
	Sections        []string // the ten QuoteSections, each once, in rendering order
	FooterNote      string   // 0–5000 characters
}

// CreditPolicy is how customer credit is limited (ADR-BR-016): the default limit of a new customer,
// the accepted sources of a limit, the approval levels by the largest excess each may approve
// (above the highest, two approvers decide in STL), and how many days a payment may be overdue.
// Amounts are AmountFormat strings with at most the currency's minor units.
type CreditPolicy struct {
	Name                 string
	Currency             string // currency code, configured and in force
	DefaultLimit         string // 0 or more
	LimitSources         []string
	ApprovalTiers        []CreditApprovalTier // 1–10, amounts strictly ascending
	OverdueToleranceDays int                  // 0–3650
}

// CreditApprovalTier is one approval level and the largest credit excess it may approve.
type CreditApprovalTier struct {
	Level     string // identifier, such as credit_manager
	MaxAmount string // positive
}

// ScreeningRegulation is the screening lists a company enables and how often a party is screened
// again (ADR-BR-017, Context Map R60 POL->PTY). PTY screens only against the lists named here; a
// conclusion stays valid RescreenIntervalDays after the screening. Brant records the lists and the
// legal confirmations; it gives no legal opinion.
type ScreeningRegulation struct {
	Lists                []ScreeningList
	RescreenIntervalDays int // 1–3650
}

// ScreeningList is one enabled list. A foreign list needs LegalConfirmationEvidenceID: the EVD
// source of the tenant's legal confirmation, which the publication must cite among its evidence,
// so the publisher must be able to read it (R63).
type ScreeningList struct {
	ListCode                    string // the list code reference data imports it under
	Jurisdiction                string // CN, UN or foreign
	LegalConfirmationEvidenceID string // required for a foreign list
}

func (r *ScreeningRegulation) validate() error {
	if len(r.Lists) < 1 || len(r.Lists) > MaxScreeningLists {
		return schema("/args/data/lists")
	}
	seen := map[string]bool{}
	for i, l := range r.Lists {
		at := "/args/data/lists/" + strconv.Itoa(i)
		switch {
		case len(l.ListCode) > maxListCodeLength || !kernel.ValidIdentifier(l.ListCode) || seen[l.ListCode]:
			return schema(at + "/list_code")
		case !slices.Contains(jurisdictions, l.Jurisdiction):
			return schema(at + "/jurisdiction")
		case l.LegalConfirmationEvidenceID != "" && !kernel.ValidIdentifier(l.LegalConfirmationEvidenceID):
			return schema(at + "/legal_confirmation_evidence_id")
		case l.Jurisdiction == JurisdictionForeign && l.LegalConfirmationEvidenceID == "":
			// A foreign list is off unless the tenant registers its legal confirmation (SR-19).
			return schema(at + "/legal_confirmation_evidence_id")
		}
		seen[l.ListCode] = true
	}
	if r.RescreenIntervalDays < 1 || r.RescreenIntervalDays > maxRescreenDays {
		return schema("/args/data/rescreen_interval_days")
	}
	return nil
}

func (d ConfigData) validate(kind string) error {
	members := map[string]bool{
		KindCompanyProfile: d.CompanyProfile != nil, KindCurrency: d.Currency != nil, KindUnit: d.Unit != nil,
		KindUnitConversion: d.UnitConversion != nil, KindFXRate: d.FXRate != nil, KindCalendar: d.Calendar != nil,
		KindTradeTerms: d.TradeTerms != nil, KindPaymentTerms: d.PaymentTerms != nil, KindTaxProfile: d.TaxProfile != nil,
		KindTaxApplication: d.TaxApplication != nil, KindFeeCategory: d.FeeCategory != nil, KindPricingPolicy: d.PricingPolicy != nil,
		KindBankAccount: d.BankAccount != nil, KindNumberSequence: d.NumberSequence != nil, KindDocumentTemplate: d.DocumentTemplate != nil,
		KindQuoteDocumentTemplate: d.QuoteDocumentTemplate != nil, KindCreditPolicy: d.CreditPolicy != nil,
		KindScreeningRegulation: d.ScreeningRegulation != nil,
	}
	for k, present := range members {
		if present != (k == kind) {
			return schema("/args/data")
		}
	}
	switch kind {
	case KindCompanyProfile:
		return d.CompanyProfile.validate()
	case KindCurrency:
		return d.Currency.validate()
	case KindUnit:
		return d.Unit.validate()
	case KindUnitConversion:
		return d.UnitConversion.validate()
	case KindFXRate:
		return d.FXRate.validate()
	case KindCalendar:
		return d.Calendar.validate()
	case KindTradeTerms:
		return d.TradeTerms.validate()
	case KindPaymentTerms:
		return d.PaymentTerms.validate()
	case KindTaxProfile:
		return d.TaxProfile.validate()
	case KindTaxApplication:
		return d.TaxApplication.validate()
	case KindFeeCategory:
		return d.FeeCategory.validate()
	case KindPricingPolicy:
		return d.PricingPolicy.validate()
	case KindBankAccount:
		return d.BankAccount.validate()
	case KindNumberSequence:
		return d.NumberSequence.validate()
	case KindDocumentTemplate:
		return d.DocumentTemplate.validate()
	case KindQuoteDocumentTemplate:
		return d.QuoteDocumentTemplate.validate()
	case KindCreditPolicy:
		return d.CreditPolicy.validate()
	case KindScreeningRegulation:
		return d.ScreeningRegulation.validate()
	}
	return schema("/args/kind")
}

// field is one check of a closed structure: ok, or SCHEMA_INVALID at /args/data/<name>.
type field struct {
	name string
	ok   bool
}

// first returns SCHEMA_INVALID at the first field that is not ok.
func first(fields ...field) error {
	for _, f := range fields {
		if !f.ok {
			return schema("/args/data/" + f.name)
		}
	}
	return nil
}

func (c *CompanyProfile) validate() error {
	return first(field{"legal_name", name(c.LegalName)}, field{"registration_no", name(c.RegistrationNo)},
		field{"country", country(c.Country)}, field{"address", name(c.Address)}, field{"base_currency", currencyCode(c.BaseCurrency)},
		field{"time_zone", text(c.TimeZone, maxTimeZoneCharacters)}, field{"policy_scope", name(c.PolicyScope)})
}

func (c *Currency) validate() error {
	return first(field{"code", currencyCode(c.Code)}, field{"name", name(c.Name)},
		field{"minor_units", c.MinorUnits >= 0 && c.MinorUnits <= maxCurrencyMinorUnits}, field{"rounding", c.Rounding == RoundingHalfUp})
}

func (u *Unit) validate() error {
	return first(field{"code", kernel.UnitCode(u.Code).Valid()}, field{"name", name(u.Name)},
		field{"dimension", slices.Contains(dimensions, u.Dimension)}, field{"precision", u.Precision >= 0 && u.Precision <= maxUnitPrecision})
}

func (u *UnitConversion) validate() error {
	return first(field{"sku_id", kernel.ValidIdentifier(u.SkuID)}, field{"spec_revision", u.SpecRevision >= 1 && u.SpecRevision <= maxVersion},
		field{"from_unit", kernel.UnitCode(u.FromUnit).Valid()}, field{"to_unit", kernel.UnitCode(u.ToUnit).Valid()},
		field{"factor", decimal(u.Factor, kernel.FactorFormat)}, field{"basis", name(u.Basis)})
}

func (x *FXRate) validate() error {
	_, err := kernel.ParseTimestamp(x.ObservedAt)
	return first(field{"from_currency", currencyCode(x.FromCurrency)}, field{"to_currency", currencyCode(x.ToCurrency)},
		field{"rate", decimal(x.Rate, kernel.FactorFormat)}, field{"purpose", slices.Contains(purposes, x.Purpose)},
		field{"source_name", name(x.SourceName)}, field{"observed_at", err == nil})
}

func (c *Calendar) validate() error {
	if !text(c.TimeZone, maxTimeZoneCharacters) {
		return schema("/args/data/time_zone")
	}
	if len(c.WorkingWeekdays) < 1 || len(c.WorkingWeekdays) > 7 {
		return schema("/args/data/working_weekdays")
	}
	for i, d := range c.WorkingWeekdays {
		if d < 1 || d > 7 || slices.Index(c.WorkingWeekdays, d) != i {
			return schema("/args/data/working_weekdays/" + strconv.Itoa(i))
		}
	}
	if len(c.Holidays) > MaxHolidays {
		return schema("/args/data/holidays")
	}
	for i, h := range c.Holidays {
		if !date(h) || slices.Index(c.Holidays, h) != i {
			return schema("/args/data/holidays/" + strconv.Itoa(i))
		}
	}
	return nil
}

func (t *TradeTerms) validate() error {
	if err := first(field{"incoterm", slices.Contains(incoterms, t.Incoterm)}, field{"edition", t.Edition == IncotermsEdition2020},
		field{"named_place", name(t.NamedPlace)}, field{"responsibility_note", name(t.ResponsibilityNote)}); err != nil {
		return err
	}
	d := t.Delivery
	switch d.Kind {
	case DeliveryFixedDate:
		if d.AnchorEvent != "" || d.Days != 0 || d.DayBasis != "" || d.Calendar != nil {
			return schema("/args/data/delivery")
		}
		return first(field{"delivery/date", date(d.Date)})
	case DeliveryLeadTime:
		if d.Date != "" {
			return schema("/args/data/delivery/date")
		}
		if err := first(field{"delivery/anchor_event", slices.Contains(anchors, d.AnchorEvent)},
			field{"delivery/days", d.Days >= 0 && d.Days <= maxDays}, field{"delivery/day_basis", slices.Contains(dayBases, d.DayBasis)},
			field{"delivery/calendar", d.Calendar != nil}); err != nil {
			return err
		}
		return requiredRef(d.Calendar, KindCalendar, "/args/data/delivery/calendar")
	}
	return schema("/args/data/delivery/kind")
}

func (p *PaymentTerms) validate() error {
	if !name(p.Name) {
		return schema("/args/data/name")
	}
	if len(p.Stages) < 1 || len(p.Stages) > MaxPaymentStages {
		return schema("/args/data/stages")
	}
	for i, s := range p.Stages {
		at := "stages/" + strconv.Itoa(i)
		if err := first(field{at + "/stage_id", kernel.ValidIdentifier(s.StageID)}, field{at + "/percent", decimal(s.Percent, PercentFormat)},
			field{at + "/trigger", slices.Contains(triggers, s.Trigger)}, field{at + "/days", s.Days >= 0 && s.Days <= maxPaymentDays}); err != nil {
			return err
		}
	}
	switch {
	case p.DayBasis == "" && p.Calendar != nil:
		return schema("/args/data/day_basis")
	case p.DayBasis != "" && !slices.Contains(dayBases, p.DayBasis):
		return schema("/args/data/day_basis")
	case p.DayBasis == DayBasisWorking && p.Calendar == nil:
		return schema("/args/data/calendar")
	}
	return optionalRef(p.Calendar, KindCalendar, "/args/data/calendar")
}

func (t *TaxProfile) validate() error {
	return first(field{"name", name(t.Name)}, field{"rate", decimal(t.Rate, kernel.FactorFormat)},
		field{"price_basis", slices.Contains(priceBases, t.PriceBasis)}, field{"recovery_treatment", slices.Contains(recoveries, t.RecoveryTreatment)},
		field{"policy_basis", name(t.PolicyBasis)})
}

func (a *TaxApplication) validate() error {
	if err := first(field{"name", name(a.Name)}, field{"side", slices.Contains(taxSides, a.Side)},
		field{"subject_kind", slices.Contains(subjects, a.SubjectKind)}, field{"subject_id", subjectID(a.SubjectKind, a.SubjectID)},
		field{"party_id", a.PartyID == "" || kernel.ValidIdentifier(a.PartyID)}); err != nil {
		return err
	}
	if err := requiredRef(&a.TaxProfile, KindTaxProfile, "/args/data/tax_profile"); err != nil {
		return err
	}
	return first(field{"jurisdiction", name(a.Jurisdiction)}, field{"basis_note", name(a.BasisNote)})
}

// subjectID is a SKU ID or a fee_category config_id.
func subjectID(kind, id string) bool {
	if kind == SubjectFeeCategory {
		return ValidConfigID(id)
	}
	return kernel.ValidIdentifier(id)
}

func (f *FeeCategory) validate() error {
	return first(field{"name", name(f.Name)}, field{"category", slices.Contains(feeCategories, f.Category)},
		field{"side", slices.Contains(feeSides, f.Side)}, field{"inclusion", slices.Contains(inclusions, f.Inclusion)},
		field{"allocation_basis", slices.Contains(allocations, f.AllocationBasis)}, field{"policy_basis", name(f.PolicyBasis)})
}

func (p *PricingPolicy) validate() error {
	if err := first(field{"name", name(p.Name)}, field{"calculation_currency", currencyCode(p.CalculationCurrency)},
		field{"minimum_margin", decimal(p.MinimumMargin, kernel.FactorFormat)},
		field{"quote_valid_days", p.QuoteValidDays >= 1 && p.QuoteValidDays <= maxQuoteValidDays},
		field{"max_fx_age_seconds", p.MaxFXAgeSeconds >= 1 && p.MaxFXAgeSeconds <= maxFXAgeSeconds}); err != nil {
		return err
	}
	return requiredRef(&p.TaxProfile, KindTaxProfile, "/args/data/tax_profile")
}

func (b *BankAccount) validate() error {
	return first(field{"owner_kind", slices.Contains(owners, b.OwnerKind)}, field{"owner_id", kernel.ValidIdentifier(b.OwnerID)},
		field{"bank_name", name(b.BankName)}, field{"account_number", accountNumber(b.AccountNumber)},
		field{"currency", currencyCode(b.Currency)}, field{"purpose", slices.Contains(accountUses, b.Purpose)})
}

func (n *NumberSequence) validate() error {
	return first(field{"document_type", slices.Contains(documentTypes, n.DocumentType)}, field{"prefix", sequencePrefix(n.Prefix)},
		field{"width", n.Width >= minSequenceWidth && n.Width <= maxSequenceWidth}, field{"reset", n.Reset == ResetAnnual},
		field{"time_zone", text(n.TimeZone, maxTimeZoneCharacters)})
}

func (t *DocumentTemplate) validate() error {
	if err := first(field{"name", name(t.Name)}, field{"document_type", slices.Contains(documentTypes, t.DocumentType)},
		field{"template_text", text(t.TemplateText, maxTemplateText)}); err != nil {
		return err
	}
	if len(t.FieldNames) < 1 || len(t.FieldNames) > len(templateFields) {
		return schema("/args/data/field_names")
	}
	for i, f := range t.FieldNames {
		if !slices.Contains(templateFields, f) || slices.Index(t.FieldNames, f) != i {
			return schema("/args/data/field_names/" + strconv.Itoa(i))
		}
	}
	return nil
}

func (t *QuoteDocumentTemplate) validate() error {
	sections := slices.Sorted(slices.Values(t.Sections))
	return first(field{"name", name(t.Name)}, field{"language", slices.Contains(languages, t.Language)},
		field{"renderer_version", t.RendererVersion == QuoteRendererVersion},
		field{"title", nonBlank(t.Title, maxTitleCharacters)},
		field{"sections", slices.Equal(sections, slices.Sorted(slices.Values(quoteSections)))},
		field{"footer_note", utf8.ValidString(t.FooterNote) && utf8.RuneCountInString(t.FooterNote) <= maxNoteCharacters})
}

func (p *CreditPolicy) validate() error {
	if err := first(field{"name", name(p.Name)}, field{"currency", currencyCode(p.Currency)},
		field{"default_limit", decimal(p.DefaultLimit, AmountFormat)}); err != nil {
		return err
	}
	if len(p.LimitSources) < 1 || len(p.LimitSources) > len(limitSources) {
		return schema("/args/data/limit_sources")
	}
	for i, s := range p.LimitSources {
		if !slices.Contains(limitSources, s) || slices.Index(p.LimitSources, s) != i {
			return schema("/args/data/limit_sources/" + strconv.Itoa(i))
		}
	}
	if len(p.ApprovalTiers) < 1 || len(p.ApprovalTiers) > MaxCreditTiers {
		return schema("/args/data/approval_tiers")
	}
	for i, t := range p.ApprovalTiers {
		at := "approval_tiers/" + strconv.Itoa(i)
		if err := first(field{at + "/level", kernel.ValidIdentifier(t.Level)}, field{at + "/max_amount", decimal(t.MaxAmount, AmountFormat)}); err != nil {
			return err
		}
	}
	return first(field{"overdue_tolerance_days", p.OverdueToleranceDays >= 0 && p.OverdueToleranceDays <= maxDays})
}

// requiredRef checks a reference to a configuration of kind inside data.
func requiredRef(r *ConfigRef, kind, path string) error {
	if r == nil {
		return schema(path)
	}
	if err := r.Validate(path); err != nil {
		return err
	}
	if r.Kind != kind {
		return schema(path + "/kind")
	}
	return nil
}

// optionalRef is requiredRef for a reference that may be absent.
func optionalRef(r *ConfigRef, kind, path string) error {
	if r == nil {
		return nil
	}
	return requiredRef(r, kind, path)
}

// country is an ISO 3166-1 alpha-2 code.
func country(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// date is a calendar date YYYY-MM-DD that exists.
func date(s string) bool {
	d, err := time.Parse(time.DateOnly, s)
	return err == nil && d.Format(time.DateOnly) == s
}

// accountNumber is 4–64 characters without control characters and not only white space.
func accountNumber(s string) bool {
	n := utf8.RuneCountInString(s)
	return n >= minAccountNumber && n <= maxAccountNumber && utf8.ValidString(s) && name(s) &&
		!slices.ContainsFunc([]rune(s), unicode.IsControl)
}
