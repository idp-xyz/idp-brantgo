package config

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Kind is a configuration kind. The domain knows the kinds it has rules for.
type Kind string

const (
	KindCompanyProfile        Kind = "company_profile"
	KindCurrency              Kind = "currency"
	KindUnit                  Kind = "unit"
	KindUnitConversion        Kind = "unit_conversion"
	KindFXRate                Kind = "fx_rate"
	KindCalendar              Kind = "calendar"
	KindTradeTerms            Kind = "trade_terms"
	KindPaymentTerms          Kind = "payment_terms"
	KindTaxProfile            Kind = "tax_profile"
	KindTaxApplication        Kind = "tax_application"
	KindFeeCategory           Kind = "fee_category"
	KindPricingPolicy         Kind = "pricing_policy"
	KindBankAccount           Kind = "bank_account"
	KindNumberSequence        Kind = "number_sequence"
	KindDocumentTemplate      Kind = "document_template"
	KindQuoteDocumentTemplate Kind = "quote_document_template"
	KindCreditPolicy          Kind = "credit_policy"
	KindScreeningRegulation   Kind = "screening_regulation"
)

// Closed values the rules below compare.
const (
	PriceBasisNotApplicable = "not_applicable"
	RecoveryUnknown         = "unknown"
	SubjectFeeCategory      = "fee_category"
	OwnerCompany            = "company"
	OwnerParty              = "party"
	DayBasisWorking         = "working"

	// JurisdictionForeign is the jurisdiction of a screening list a tenant enables only with its
	// legal confirmation (ADR-BR-017, SR-19).
	JurisdictionForeign = "foreign"
)

// Ref is a ConfigRef v1 inside configuration data: the exact revision of another configuration.
// Its fields match package api's ConfigRef.
type Ref struct {
	Kind     string `json:"kind"`
	ConfigID string `json:"id"`
	Revision int64  `json:"revision"`
}

// CompanyProfile is the company of the scope; its config_id is the company's ID.
type CompanyProfile struct {
	LegalName      string `json:"legal_name"`
	RegistrationNo string `json:"registration_no"`
	Country        string `json:"country"`
	Address        string `json:"address"`
	BaseCurrency   string `json:"base_currency"`
	TimeZone       string `json:"time_zone"`
	PolicyScope    string `json:"policy_scope"`
}

// Currency is an ISO 4217 currency; code, minor units and rounding are its meaning.
type Currency struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	MinorUnits int    `json:"minor_units"`
	Rounding   string `json:"rounding"`
}

// Unit is a unit of measure; code, dimension and precision are its meaning.
type Unit struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Dimension string `json:"dimension"`
	Precision int    `json:"precision"`
}

// UnitConversion is how many ToUnit make one FromUnit for one SKU at one specification revision.
// The SKU is CAT's; POL stores its ID as given.
type UnitConversion struct {
	SkuID        string `json:"sku_id"`
	SpecRevision int64  `json:"spec_revision"`
	FromUnit     string `json:"from_unit"`
	ToUnit       string `json:"to_unit"`
	Factor       string `json:"factor"`
	Basis        string `json:"basis"`
}

// FXRate is one unit of FromCurrency in ToCurrency, for one purpose. ObservedAt is a canonical
// timestamp (kernel.FormatTimestamp).
type FXRate struct {
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	Rate         string `json:"rate"`
	Purpose      string `json:"purpose"`
	SourceName   string `json:"source_name"`
	ObservedAt   string `json:"observed_at"`
}

// Calendar is a working calendar: ISO weekdays (1 Monday … 7 Sunday) and holidays, both ascending.
type Calendar struct {
	TimeZone        string   `json:"time_zone"`
	WorkingWeekdays []int    `json:"working_weekdays"`
	Holidays        []string `json:"holidays"`
}

// TradeTerms are an Incoterms rule with its named place and how delivery is dated.
type TradeTerms struct {
	Incoterm           string   `json:"incoterm"`
	Edition            string   `json:"edition"`
	NamedPlace         string   `json:"named_place"`
	ResponsibilityNote string   `json:"responsibility_note"`
	Delivery           Delivery `json:"delivery"`
}

// Delivery is a fixed date, or a lead time of Days after an anchor event, counted on a day basis
// in a calendar revision.
type Delivery struct {
	Kind        string `json:"kind"`
	Date        string `json:"date,omitempty"`
	AnchorEvent string `json:"anchor_event,omitempty"`
	Days        int    `json:"days,omitempty"`
	DayBasis    string `json:"day_basis,omitempty"`
	Calendar    *Ref   `json:"calendar,omitempty"`
}

// PaymentTerms are payment stages whose days count on DayBasis, in Calendar when one is declared.
// Payment terms without a day basis count calendar days, as published before the basis existed.
type PaymentTerms struct {
	Name     string         `json:"name"`
	Stages   []PaymentStage `json:"stages"`
	DayBasis string         `json:"day_basis,omitempty"`
	Calendar *Ref           `json:"calendar,omitempty"`
}

// PaymentStage is one stage of PaymentTerms.
type PaymentStage struct {
	StageID string `json:"stage_id"`
	Percent string `json:"percent"`
	Trigger string `json:"trigger"`
	Days    int    `json:"days"`
}

// TaxProfile is a tax rate with its price basis and whether the tax is recoverable.
type TaxProfile struct {
	Name              string `json:"name"`
	Rate              string `json:"rate"`
	PriceBasis        string `json:"price_basis"`
	RecoveryTreatment string `json:"recovery_treatment"`
	PolicyBasis       string `json:"policy_basis"`
}

// TaxApplication is the verified basis on which a tax profile applies to one side of trade and one
// subject: a SKU (CAT's, stored as given) or a fee category, optionally for one party (PTY's, stored
// as given).
type TaxApplication struct {
	Name         string `json:"name"`
	Side         string `json:"side"`
	SubjectKind  string `json:"subject_kind"`
	SubjectID    string `json:"subject_id"`
	PartyID      string `json:"party_id,omitempty"`
	TaxProfile   Ref    `json:"tax_profile"`
	Jurisdiction string `json:"jurisdiction"`
	BasisNote    string `json:"basis_note"`
}

// FeeCategory classifies a fee: its category, which side bears it, whether it is part of a line
// price and how it is allocated.
type FeeCategory struct {
	Name            string `json:"name"`
	Category        string `json:"category"`
	Side            string `json:"side"`
	Inclusion       string `json:"inclusion"`
	AllocationBasis string `json:"allocation_basis"`
	PolicyBasis     string `json:"policy_basis"`
}

// PricingPolicy is the basis of cost estimates and quotations: the calculation currency, the
// minimum margin, how long a quotation is valid, how old an exchange rate may be and the tax
// profile revision.
type PricingPolicy struct {
	Name                string `json:"name"`
	CalculationCurrency string `json:"calculation_currency"`
	MinimumMargin       string `json:"minimum_margin"`
	QuoteValidDays      int    `json:"quote_valid_days"`
	MaxFXAgeSeconds     int64  `json:"max_fx_age_seconds"`
	TaxProfile          Ref    `json:"tax_profile"`
}

// BankAccount is an account of the company or of a party (PTY's, stored as given). The account
// number is sensitive.
type BankAccount struct {
	OwnerKind     string `json:"owner_kind"`
	OwnerID       string `json:"owner_id"`
	BankName      string `json:"bank_name"`
	AccountNumber string `json:"account_number"`
	Currency      string `json:"currency"`
	Purpose       string `json:"purpose"`
}

// NumberSequence is how document numbers of one document type are formed.
type NumberSequence struct {
	DocumentType string `json:"document_type"`
	Prefix       string `json:"prefix"`
	Width        int    `json:"width"`
	Reset        string `json:"reset"`
	TimeZone     string `json:"time_zone"`
}

// DocumentTemplate is a plain-text template of one document type whose placeholders name fixed
// fields only.
type DocumentTemplate struct {
	Name         string   `json:"name"`
	DocumentType string   `json:"document_type"`
	TemplateText string   `json:"template_text"`
	FieldNames   []string `json:"field_names"`
}

// QuoteDocumentTemplate is the layout of a formal quotation for the fixed quote-text renderer: its
// language, title, the order of its ten sections and a footer note.
type QuoteDocumentTemplate struct {
	Name            string   `json:"name"`
	Language        string   `json:"language"`
	RendererVersion string   `json:"renderer_version"`
	Title           string   `json:"title"`
	Sections        []string `json:"sections"`
	FooterNote      string   `json:"footer_note"`
}

// CreditPolicy is how customer credit is limited: the default limit of a new customer, the
// accepted sources of a limit, the approval levels by the largest excess each may approve, and how
// many days a payment may be overdue. Amounts are in Currency.
type CreditPolicy struct {
	Name                 string               `json:"name"`
	Currency             string               `json:"currency"`
	DefaultLimit         string               `json:"default_limit"`
	LimitSources         []string             `json:"limit_sources"`
	ApprovalTiers        []CreditApprovalTier `json:"approval_tiers"`
	OverdueToleranceDays int                  `json:"overdue_tolerance_days"`
}

// CreditApprovalTier is one approval level and the largest credit excess it may approve.
type CreditApprovalTier struct {
	Level     string `json:"level"`
	MaxAmount string `json:"max_amount"`
}

// TemplateFields are the fields a plain-text document template may name (Python FIELDS).
var TemplateFields = []string{"company.name", "customer.name", "document.number", "document.date", "lines.summary",
	"quote.total", "quote.currency"}

// zone reports whether tz is an IANA time zone; "Local" depends on the host and is not one.
func zone(tz string) bool {
	if tz == "Local" {
		return false
	}
	_, err := time.LoadLocation(tz)
	return err == nil
}

// ratio parses a FactorFormat value and reports whether it lies in [0, 1].
func ratio(s string) bool {
	d, err := kernel.ParseDecimal(s, kernel.FactorFormat)
	return err == nil && d.Sign() >= 0 && d.Cmp(kernel.DecimalFromInt(1)) <= 0
}

// ScreeningRegulation is the screening lists a company enables and its re-screening interval.
type ScreeningRegulation struct {
	Lists                []ScreeningList `json:"lists"`
	RescreenIntervalDays int             `json:"rescreen_interval_days"`
}

// ScreeningList is one enabled list; a foreign list carries the EVD source of the tenant's legal
// confirmation.
type ScreeningList struct {
	ListCode                    string `json:"list_code"`
	Jurisdiction                string `json:"jurisdiction"`
	LegalConfirmationEvidenceID string `json:"legal_confirmation_evidence_id,omitempty"`
}

// Validate judges content the contract admitted (Python foundation.validate): a validity that ends
// after it starts; a currency or unit whose config_id is its code; an exchange rate between two
// currencies with a positive rate observed no later than it becomes valid; a conversion between
// two units with a positive factor; payment stages with distinct IDs and positive percentages
// summing to 100; time zones that exist; tax rates and margins in [0, 1], no rate where tax does not
// apply; a template whose placeholders are exactly its fixed fields; credit tiers ascending.
func Validate(id string, c Content) error {
	if c.Data() == nil {
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/data", "")
	}
	if !c.Validity().Valid() {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/valid_until", "")
	}
	invalid := func(path string) error { return kernel.Reject(kernel.CodeInvalidKnownValue, path, "") }
	switch c.Kind {
	case KindCompanyProfile:
		if !zone(c.CompanyProfile.TimeZone) {
			return invalid("/args/data/time_zone")
		}
	case KindCurrency:
		if id != c.Currency.Code {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/id", "Currency id must equal its code")
		}
	case KindUnit:
		if id != c.Unit.Code {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/id", "Unit id must equal its code")
		}
	case KindUnitConversion:
		u := c.UnitConversion
		if u.FromUnit == u.ToUnit {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/to_unit", "A unit converts into another unit")
		}
		if f, err := kernel.ParseDecimal(u.Factor, kernel.FactorFormat); err != nil || f.Sign() <= 0 {
			return invalid("/args/data/factor")
		}
	case KindFXRate:
		x := c.FXRate
		if x.FromCurrency == x.ToCurrency {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/to_currency", "Same currency is an explicit 1, not a rate")
		}
		if rate, err := kernel.ParseDecimal(x.Rate, kernel.FactorFormat); err != nil || rate.Sign() <= 0 {
			return invalid("/args/data/rate")
		}
		if c.ValidUntil.IsZero() {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/valid_until", "An exchange rate expires")
		}
		observed, err := kernel.ParseTimestamp(x.ObservedAt)
		if err != nil || observed.After(c.ValidFrom) {
			return invalid("/args/data/observed_at")
		}
	case KindCalendar:
		if !zone(c.Calendar.TimeZone) {
			return invalid("/args/data/time_zone")
		}
	case KindPaymentTerms:
		return checkStages(c.PaymentTerms.Stages)
	case KindTaxProfile:
		t := c.TaxProfile
		if !ratio(t.Rate) {
			return invalid("/args/data/rate")
		}
		if t.PriceBasis == PriceBasisNotApplicable && !kernel.MustDecimal(t.Rate).IsZero() {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/rate", "No rate where tax does not apply")
		}
	case KindPricingPolicy:
		if !ratio(c.PricingPolicy.MinimumMargin) {
			return invalid("/args/data/minimum_margin")
		}
	case KindNumberSequence:
		if !zone(c.NumberSequence.TimeZone) {
			return invalid("/args/data/time_zone")
		}
	case KindDocumentTemplate:
		return checkTemplate(c.DocumentTemplate)
	case KindCreditPolicy:
		return checkCredit(c.CreditPolicy)
	case KindScreeningRegulation:
		for i, l := range c.ScreeningRegulation.Lists {
			if l.Jurisdiction == JurisdictionForeign && l.LegalConfirmationEvidenceID == "" {
				return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/lists/"+strconv.Itoa(i)+"/legal_confirmation_evidence_id",
					"A foreign list needs the tenant's legal confirmation")
			}
		}
	}
	return nil
}

// CheckLegalConfirmations requires a screening regulation's publication to cite, among its
// evidence, the legal confirmation of every list that has one, so the publisher must be able to
// read it like the rest of the evidence (ADR-BR-017, Context Map R63).
func CheckLegalConfirmations(c Content, evidence []string) error {
	if c.Kind != KindScreeningRegulation {
		return nil
	}
	for _, l := range c.ScreeningRegulation.Lists {
		if id := l.LegalConfirmationEvidenceID; id != "" && !slices.Contains(evidence, id) {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/evidence_ids",
				"Cite the legal confirmation of screening list "+l.ListCode)
		}
	}
	return nil
}

// checkStages enforces POL-INV-004: distinct stage IDs, every percentage above 0 and exactly 100 in
// total, compared exactly.
func checkStages(stages []PaymentStage) error {
	seen := map[string]bool{}
	for _, s := range stages {
		if seen[s.StageID] {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/stages", "Stage IDs must be distinct")
		}
		seen[s.StageID] = true
	}
	total := kernel.DecimalFromInt(0)
	for _, s := range stages {
		// The contract admitted PercentFormat; FactorFormat accepts every such string.
		p, err := kernel.ParseDecimal(s.Percent, kernel.FactorFormat)
		if err != nil || p.Sign() <= 0 {
			return reject(CodePaymentTotalMismatch, "/args/data/stages")
		}
		total = total.Add(p)
	}
	if !total.Equal(kernel.DecimalFromInt(100)) {
		return reject(CodePaymentTotalMismatch, "/args/data/stages")
	}
	return nil
}

// checkTemplate requires the placeholders of a plain-text template to be exactly its declared
// fields (Python TEMPLATE_INVALID): no format specification, conversion, attribute or index, and no
// unbalanced brace.
func checkTemplate(t *DocumentTemplate) error {
	names, ok := TemplateFieldNames(t.TemplateText)
	if !ok {
		return reject(CodeTemplateInvalid, "/args/data/template_text")
	}
	used := slices.Compact(slices.Sorted(slices.Values(names)))
	declared := slices.Compact(slices.Sorted(slices.Values(t.FieldNames)))
	if !slices.Equal(used, declared) {
		return reject(CodeTemplateInvalid, "/args/data/field_names")
	}
	return nil
}

// TemplateFieldNames parses text as a plain-text template: literal text, "{{" and "}}" for literal
// braces and "{field}" placeholders, each exactly one of TemplateFields. It returns the fields in
// order of appearance and false for anything else, which Python's string.Formatter either rejects or
// would evaluate (format specifications, conversions, attributes, indexes).
func TemplateFieldNames(text string) ([]string, bool) {
	var names []string
	for i := 0; i < len(text); {
		switch text[i] {
		case '{':
			if i+1 < len(text) && text[i+1] == '{' {
				i += 2
				continue
			}
			end := strings.IndexByte(text[i+1:], '}')
			if end < 0 {
				return nil, false
			}
			field := text[i+1 : i+1+end]
			if !slices.Contains(TemplateFields, field) {
				return nil, false
			}
			names = append(names, field)
			i += end + 2
		case '}':
			if i+1 < len(text) && text[i+1] == '}' {
				i += 2
				continue
			}
			return nil, false
		default:
			i++
		}
	}
	return names, true
}

// checkCredit requires a non-negative default limit, positive tier amounts that strictly ascend and
// distinct tier levels. The currency's minor units are checked with the references.
func checkCredit(p *CreditPolicy) error {
	if _, err := kernel.ParseDecimal(p.DefaultLimit, amountFormat); err != nil {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/default_limit", "")
	}
	levels := map[string]bool{}
	var previous kernel.Decimal
	for i, t := range p.ApprovalTiers {
		at := "/args/data/approval_tiers/" + strconv.Itoa(i)
		if levels[t.Level] {
			return kernel.Reject(kernel.CodeInvalidKnownValue, at+"/level", "Approval levels must be distinct")
		}
		levels[t.Level] = true
		amount, err := kernel.ParseDecimal(t.MaxAmount, amountFormat)
		if err != nil || amount.Sign() <= 0 || (i > 0 && amount.Cmp(previous) <= 0) {
			return kernel.Reject(kernel.CodeInvalidKnownValue, at+"/max_amount", "Tier amounts are positive and ascend")
		}
		previous = amount
	}
	return nil
}

// amountFormat bounds an amount before its currency is known: 4 is the largest number of minor
// units a currency has, and |minor| < 10^24.
var amountFormat = kernel.DecimalFormat{IntDigits: kernel.MaxMinorDigits - 4, Scale: 4}

// Reference is one configuration a content refers to, at the path of the save request: by ID
// (Revision 0), meaning the revision in force when the content is used, or an exact revision.
type Reference struct {
	Path     string
	Kind     Kind
	ID       string
	Revision int64
}

// References returns what c refers to. Currencies and units are named by code: their meaning never
// changes (POL-INV-002). Tax profiles and calendars are named by exact revision. A fee category a
// tax application applies to is named by ID; a SKU, a party or a company is not a configuration.
func (c Content) References() []Reference {
	var out []Reference
	byID := func(path string, kind Kind, id string) {
		out = append(out, Reference{Path: path, Kind: kind, ID: id})
	}
	exact := func(path string, r *Ref) {
		if r != nil {
			out = append(out, Reference{Path: path, Kind: Kind(r.Kind), ID: r.ConfigID, Revision: r.Revision})
		}
	}
	switch c.Kind {
	case KindCompanyProfile:
		byID("/args/data/base_currency", KindCurrency, c.CompanyProfile.BaseCurrency)
	case KindUnitConversion:
		byID("/args/data/from_unit", KindUnit, c.UnitConversion.FromUnit)
		byID("/args/data/to_unit", KindUnit, c.UnitConversion.ToUnit)
	case KindFXRate:
		byID("/args/data/from_currency", KindCurrency, c.FXRate.FromCurrency)
		byID("/args/data/to_currency", KindCurrency, c.FXRate.ToCurrency)
	case KindTradeTerms:
		exact("/args/data/delivery/calendar", c.TradeTerms.Delivery.Calendar)
	case KindPaymentTerms:
		exact("/args/data/calendar", c.PaymentTerms.Calendar)
	case KindTaxApplication:
		a := c.TaxApplication
		if a.SubjectKind == SubjectFeeCategory {
			byID("/args/data/subject_id", KindFeeCategory, a.SubjectID)
		}
		exact("/args/data/tax_profile", &a.TaxProfile)
	case KindPricingPolicy:
		byID("/args/data/calculation_currency", KindCurrency, c.PricingPolicy.CalculationCurrency)
		exact("/args/data/tax_profile", &c.PricingPolicy.TaxProfile)
	case KindBankAccount:
		byID("/args/data/currency", KindCurrency, c.BankAccount.Currency)
	case KindCreditPolicy:
		byID("/args/data/currency", KindCurrency, c.CreditPolicy.Currency)
	}
	return out
}

// Target names a referenced configuration: by ID when Revision is 0, otherwise exactly.
type Target struct {
	Kind     Kind
	ID       string
	Revision int64
}

// Target returns what r names.
func (r Reference) Target() Target { return Target{Kind: r.Kind, ID: r.ID, Revision: r.Revision} }

// Fact is the state of one referenced configuration at an instant: the revision in force (by ID)
// or the revision named (exact), and nil Refused when it is in force, or what InForce says against
// it. Rev is nil when no revision is published at the instant.
type Fact struct {
	Rev     *Revision
	Refused error
}

// Facts are the states of the configurations a content refers to, at the instant of a save or a
// publication, and the company of the scope. A target without an entry is not configured.
type Facts struct {
	Company string
	Configs map[Target]Fact
}

// CheckReferences judges what content c of configuration id refers to (Python: every child reference
// is active at save and publication). A company profile is the profile of the scope's company; a
// company's bank account belongs to the scope's company; every reference is configured and in force;
// the units of a conversion measure the same dimension; credit amounts fit their currency's minor
// units.
func CheckReferences(id string, c Content, f Facts) error {
	switch c.Kind {
	case KindCompanyProfile:
		if id != f.Company {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/id", "Company profile must identify the current company")
		}
	case KindBankAccount:
		if b := c.BankAccount; b.OwnerKind == OwnerCompany && b.OwnerID != f.Company {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/data/owner_id", "")
		}
	}
	for _, ref := range c.References() {
		fact, known := f.Configs[ref.Target()]
		if !known {
			return kernel.Reject(kernel.CodeReferenceInvalid, ref.Path, "")
		}
		if fact.Refused != nil {
			return Repath(fact.Refused, ref.Path)
		}
	}
	switch c.Kind {
	case KindUnitConversion:
		u := c.UnitConversion
		from := f.Configs[Target{Kind: KindUnit, ID: u.FromUnit}].Rev.Content.Unit
		to := f.Configs[Target{Kind: KindUnit, ID: u.ToUnit}].Rev.Content.Unit
		if from.Dimension != to.Dimension {
			return reject(CodeUnitScopeMismatch, "/args/data/to_unit")
		}
	case KindCreditPolicy:
		p := c.CreditPolicy
		cur := f.Configs[Target{Kind: KindCurrency, ID: p.Currency}].Rev.Content.Currency
		currency, err := kernel.NewCurrency(cur.Code, cur.MinorUnits)
		if err != nil {
			return err
		}
		if _, err = kernel.ParseAmount(currency, p.DefaultLimit); err != nil {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/default_limit", "More digits than the currency's minor units")
		}
		for i, t := range p.ApprovalTiers {
			if _, err = kernel.ParseAmount(currency, t.MaxAmount); err != nil {
				return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/approval_tiers/"+strconv.Itoa(i)+"/max_amount",
					"More digits than the currency's minor units")
			}
		}
	}
	return nil
}
