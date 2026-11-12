package quote

import (
	"slices"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CalculationVersion names the server calculation rules (package calc, WBS S3-02) a frozen
// revision was calculated under. A commercial document carries it as calculation_version, and an
// approval binds it with the normalization version and the pricing policy revision: a revision
// calculated under other rules never inherits an approval.
const CalculationVersion = "brant.quote-calculation/0.1.0"

// ShortfallFormat is the lexical form of a margin shortfall: an exact decimal in major units of the
// comparison currency, R × minimum − (R − C) with R and C of at most 4 minor digits and the minimum
// of at most 12, below 10^24.
var ShortfallFormat = kernel.DecimalFormat{IntDigits: 24, Scale: 16}

// Contracts of the two bindings a calculation records (Python t03_cost_scope.CONTRACT and
// t03_sales_terms.CONTRACT).
const (
	ScopeBindingContract = "brant.quote-cost-scope/0.1.0"
	TermsBindingContract = "brant.quote-sales-terms/0.1.0"
)

// Calculation is the server-owned result of the calculation a freeze seals (Python
// commercial.schema.json calculation, with the bindings Python's _server_calculation adds):
// amounts in integer minor units of the quote currency, the comparison amounts in those of the
// comparison currency (the pricing policy's calculation currency, a POL revision), as strings; the
// margin verdict with the exact minimum margin and shortfall in major units of the comparison
// currency ("0" when the margin passes, TRD-INV-007); the payment plan; the cost scope binding
// (TRD-INV-006) and the sales terms binding the amounts rest on; and the calculation version.
//
// The normalization (canonical.go) sorts lines, charges, payments and the excluded categories and
// trims payment percentages; every other list and decimal here is emitted in canonical form
// already: bindings in ascending line ID, role and charge ID order (code point order), decimals as
// their shortest exact decimal string.
type Calculation struct {
	Lines                  []AmountLine `json:"lines"`
	Charges                []AmountLine `json:"charges"`
	NetMinor               string       `json:"net_minor"`
	TaxMinor               string       `json:"tax_minor"`
	PayableMinor           string       `json:"payable_minor"`
	ComparisonCurrency     Ref          `json:"comparison_currency_ref"`
	SalesFX                *Ref         `json:"sales_fx_ref,omitempty"`
	ComparisonFXRate       string       `json:"comparison_fx_rate,omitempty"`
	ComparisonRevenueMinor string       `json:"comparison_revenue_minor"`
	CostMinor              string       `json:"cost_minor"`
	ProfitMinor            string       `json:"profit_minor"`
	MarginNumerator        string       `json:"margin_numerator"`
	MarginDenominator      string       `json:"margin_denominator"`
	MinimumMargin          string       `json:"minimum_margin"`
	MarginRule             string       `json:"margin_rule"`
	MarginShortfall        string       `json:"margin_shortfall"`
	CostBasis              string       `json:"cost_basis"`
	ExcludedCostCategories []string     `json:"excluded_cost_categories"`
	Payments               []Payment    `json:"payments"`
	CostScope              ScopeBinding `json:"cost_scope_binding"`
	SalesTerms             TermsBinding `json:"sales_terms_binding"`
	CalculationVersion     string       `json:"calculation_version"`
}

// AmountLine is the amounts of one line or charge; a charge names its obligation. An included
// charge has zero amounts: its line's price holds it.
type AmountLine struct {
	ID            string `json:"id"`
	ObligationKey string `json:"obligation_key,omitempty"`
	NetMinor      string `json:"net_minor"`
	TaxMinor      string `json:"tax_minor"`
	PayableMinor  string `json:"payable_minor"`
}

// Payment is one stage of the payment schedule with its amount: the payment terms' stage, trigger
// and days, the terms' day basis and calendar when they count working days.
type Payment struct {
	StageID     string `json:"stage_id"`
	Percent     string `json:"percent"`
	Trigger     string `json:"trigger"`
	Days        int    `json:"days"`
	DayBasis    string `json:"day_basis,omitempty"`
	Calendar    *Ref   `json:"calendar,omitempty"`
	AmountMinor string `json:"amount_minor"`
}

// ScopeBinding is the cost scope the calculation bound (calc.ScopeBinding): the RFQ revision, the
// pricing policy and calculation currency, the cost basis, the comparison basis the quote shares
// with the costing and the customer's trading profile, and one binding per line.
type ScopeBinding struct {
	Contract      string        `json:"contract"`
	Rfq           RfqRef        `json:"rfq_ref"`
	PricingPolicy Ref           `json:"pricing_policy_ref"`
	Currency      string        `json:"calculation_currency"`
	CostBasis     string        `json:"cost_basis"`
	TradeTerms    Ref           `json:"trade_terms_ref"`
	PaymentTerms  Ref           `json:"payment_terms_ref"`
	NamedPlace    BusinessRef   `json:"named_place_ref"`
	Delivery      Delivery      `json:"delivery"`
	Lines         []LineBinding `json:"line_bindings"`
}

// LineBinding binds one quote line to its RFQ line and cost line: the quantity in the sales unit,
// the conversion used (none when the units are the same) and the quantity in the cost unit.
type LineBinding struct {
	LineID             string `json:"line_id"`
	RfqLineID          string `json:"rfq_line_id"`
	SkuID              string `json:"sku_id"`
	SpecRevision       int64  `json:"spec_revision"`
	Quantity           string `json:"quantity"`
	Unit               string `json:"unit"`
	Conversion         *Ref   `json:"conversion_ref,omitempty"`
	NormalizedQuantity string `json:"normalized_quantity"`
	CostUnit           string `json:"cost_unit"`
}

// TermsBinding is the sales terms the calculation bound (calc.TermsBinding): the tax application
// and profile of every line ("line:<line_id>") and charge ("charge:<obligation_key>"), and every
// charge's obligation, fee category, amount and inclusion. A charge's evidence is in the data.
type TermsBinding struct {
	Contract string          `json:"contract"`
	Taxes    []TaxBinding    `json:"tax_bindings"`
	Charges  []ChargeBinding `json:"charge_bindings"`
}

// TaxBinding is the tax a line or charge is taxed by.
type TaxBinding struct {
	Role        string `json:"role"`
	Application Ref    `json:"tax_application_ref"`
	Profile     Ref    `json:"tax_profile_ref"`
	PriceBasis  string `json:"price_basis"`
	Rate        string `json:"rate"`
}

// ChargeBinding is one sales charge as bound.
type ChargeBinding struct {
	ChargeID       string `json:"charge_id"`
	ObligationKey  string `json:"obligation_key"`
	FeeCategory    Ref    `json:"fee_category_ref"`
	Amount         string `json:"amount"`
	Inclusion      string `json:"inclusion"`
	IncludedLineID string `json:"included_line_id,omitempty"`
}

// Stage is what payment terms say of a stage beyond its percentage, and of how its days count.
type Stage struct {
	Trigger  string
	Days     int
	DayBasis string
	Calendar *Ref
}

// NewCalculation is the document form of c, the result of calc.Calculate: comparison is the POL
// revision of the comparison currency, stages what the payment terms say of each stage ID.
func NewCalculation(c calc.Calculation, comparison Ref, stages map[string]Stage) *Calculation {
	out := &Calculation{Lines: []AmountLine{}, Charges: []AmountLine{}, NetMinor: c.Totals.Net.MinorString(),
		TaxMinor: c.Totals.Tax.MinorString(), PayableMinor: c.Totals.Payable.MinorString(), ComparisonCurrency: comparison,
		ComparisonRevenueMinor: c.Comparison.Revenue.MinorString(), CostMinor: c.Cost.MinorString(), ProfitMinor: c.Margin.Profit.MinorString(),
		MarginNumerator: c.Margin.Profit.MinorString(), MarginDenominator: c.Margin.Revenue.MinorString(), MinimumMargin: exact(c.Margin.Minimum),
		MarginRule: c.Margin.Verdict, MarginShortfall: exact(c.Margin.Shortfall), CostBasis: c.CostBasis,
		ExcludedCostCategories: slices.Clone(c.ExcludedCostCategories), Payments: []Payment{},
		CostScope: scopeBinding(c.Scope), SalesTerms: termsBinding(c.Terms), CalculationVersion: CalculationVersion}
	if out.ExcludedCostCategories == nil {
		out.ExcludedCostCategories = []string{}
	}
	if fx := c.Comparison.FX; fx != nil {
		r := ref(fx.Ref)
		out.SalesFX, out.ComparisonFXRate = &r, exact(fx.Rate)
	}
	for _, l := range c.Lines {
		out.Lines = append(out.Lines, AmountLine{ID: l.ID, NetMinor: l.Net.MinorString(), TaxMinor: l.Tax.MinorString(), PayableMinor: l.Payable.MinorString()})
	}
	for _, ch := range c.Charges {
		out.Charges = append(out.Charges, AmountLine{ID: ch.ID, ObligationKey: ch.ObligationKey, NetMinor: ch.Net.MinorString(),
			TaxMinor: ch.Tax.MinorString(), PayableMinor: ch.Payable.MinorString()})
	}
	for _, p := range c.Payments {
		s := stages[p.StageID]
		out.Payments = append(out.Payments, Payment{StageID: p.StageID, Percent: exact(p.Percent), Trigger: s.Trigger, Days: s.Days,
			DayBasis: s.DayBasis, Calendar: s.Calendar, AmountMinor: p.Amount.MinorString()})
	}
	return out
}

func scopeBinding(b calc.ScopeBinding) ScopeBinding {
	out := ScopeBinding{Contract: ScopeBindingContract, Rfq: RfqRef{RfqID: b.Rfq.ID, RowVersion: b.Rfq.RowVersion},
		PricingPolicy: ref(b.PricingPolicy), Currency: b.Currency, CostBasis: b.CostBasis, TradeTerms: ref(b.TradeTerms),
		PaymentTerms: ref(b.PaymentTerms), NamedPlace: business(b.NamedPlace), Delivery: delivery(b.Delivery), Lines: []LineBinding{}}
	for _, l := range b.Lines {
		lb := LineBinding{LineID: l.LineID, RfqLineID: l.RfqLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision, Quantity: exact(l.Quantity),
			Unit: l.Unit, NormalizedQuantity: exact(l.NormalizedQuantity), CostUnit: l.CostUnit}
		if !l.Conversion.IsZero() {
			r := ref(l.Conversion)
			lb.Conversion = &r
		}
		out.Lines = append(out.Lines, lb)
	}
	return out
}

func termsBinding(b calc.TermsBinding) TermsBinding {
	out := TermsBinding{Contract: TermsBindingContract, Taxes: []TaxBinding{}, Charges: []ChargeBinding{}}
	for _, t := range b.Taxes {
		out.Taxes = append(out.Taxes, TaxBinding{Role: t.Role, Application: ref(t.Application), Profile: ref(t.Profile),
			PriceBasis: string(t.PriceBasis), Rate: exact(t.Rate)})
	}
	for _, c := range b.Charges {
		out.Charges = append(out.Charges, ChargeBinding{ChargeID: c.ChargeID, ObligationKey: c.ObligationKey, FeeCategory: ref(c.FeeCategory),
			Amount: exact(c.Amount), Inclusion: c.Inclusion, IncludedLineID: c.IncludedLineID})
	}
	return out
}

func ref(r calc.Ref) Ref { return Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision} }

func business(r calc.Ref) BusinessRef {
	return BusinessRef{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func delivery(d calc.Delivery) Delivery {
	out := Delivery{Kind: d.Kind, Date: d.Date, Days: d.Days, DayBasis: d.DayBasis, Milestone: d.Milestone, Place: business(d.Place), TimeZone: d.TimeZone}
	if !d.Anchor.IsZero() {
		a := business(d.Anchor)
		out.Anchor = &a
	}
	if !d.Calendar.IsZero() {
		c := ref(d.Calendar)
		out.Calendar = &c
	}
	return out
}

// exact is the shortest exact decimal string of d. Every decimal the calculation records has a
// terminating expansion (contract decimals, their products, and money in minor units); a
// non-terminating one is a programming error.
func exact(d kernel.Decimal) string {
	s, ok := d.Exact()
	if !ok {
		panic("quote: a calculation decimal has no exact decimal form: " + d.String())
	}
	return s
}

// CalcRef is r as the calculation names it.
func CalcRef(r Ref) calc.Ref { return calc.Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision} }

// CalcPlace is a place or anchor as the calculation names it.
func CalcPlace(r BusinessRef) calc.Ref { return calc.Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision} }

// CalcDelivery is d as the calculation compares it.
func CalcDelivery(d Delivery) calc.Delivery {
	out := calc.Delivery{Kind: d.Kind, Date: d.Date, Days: d.Days, DayBasis: d.DayBasis, Milestone: d.Milestone, Place: CalcPlace(d.Place),
		TimeZone: d.TimeZone}
	if d.Anchor != nil {
		out.Anchor = CalcPlace(*d.Anchor)
	}
	if d.Calendar != nil {
		out.Calendar = CalcRef(*d.Calendar)
	}
	return out
}
