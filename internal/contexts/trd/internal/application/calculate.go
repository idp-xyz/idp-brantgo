package application

import (
	"fmt"
	"time"

	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Calculator is the server calculation of WBS S3-02 (sales charges, tax, sales FX, margin, payment
// allocation): the one place a freeze takes amounts from, never from the client. It computes on
// the draft and what INQ, PTY, POL and CST said before the transaction; the freeze runs it once the
// domain found every reference known and in effect (quote.SealDraft).
type Calculator interface {
	Calculate(in CalculationInput) (*quote.Calculation, error)
}

// CalculationInput is what the calculation of one freeze rests on.
type CalculationInput struct {
	Data     quote.Data
	Configs  map[polapi.ConfigRef]polapi.ResolvedConfig // every POL revision the draft adopts, direct, through the costing and nested
	Rfq      inqapi.RfqRef                              // the RFQ: its customer, contact and complete line set
	Profile  quote.Profile                              // the customer's trading profile terms
	Snapshot cstapi.ResolvedSnapshot                    // the adopted cost snapshot
	Context  *cstapi.VerifiedContext                    // the costing context it froze; nil when CST did not answer it
	At       time.Time                                  // the freeze's instant
}

// excludedCostCategories are what CST's only cost basis, nominal direct cost, does not estimate
// (T02 §16.1; CST rules.ExcludedCostCategories): CST's ResolvedSnapshot does not carry them, so
// the calculation names them for the cost basis it adopts.
var excludedCostCategories = map[string][]string{
	calc.CostBasisNominalDirect: {"imputed_financing_cost", "expected_credit_loss", "unrealized_fx_hedge"},
}

// ServerCalculator is the Calculator of package calc: it resolves the draft's references into
// calc's inputs, runs calc.Calculate and returns its result in the commercial document's form.
type ServerCalculator struct{}

// Calculate implements Calculator.
func (ServerCalculator) Calculate(in CalculationInput) (*quote.Calculation, error) {
	b := builder{in: in}
	q, f, err := b.inputs()
	if err != nil {
		return nil, err
	}
	c, err := calc.Calculate(q, f)
	if err != nil {
		return nil, err
	}
	return quote.NewCalculation(c, b.comparisonRef, b.stages), nil
}

const dataPath = "/args/data"

// builder resolves one freeze's references into calc's inputs.
type builder struct {
	in            CalculationInput
	quoteCurrency kernel.Currency
	comparison    kernel.Currency
	comparisonRef quote.Ref
	stages        map[string]quote.Stage
}

func (b *builder) config(r quote.Ref, path string) (polapi.ConfigData, error) {
	rc, ok := b.in.Configs[polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ID, Revision: r.Revision}]
	if !ok {
		return polapi.ConfigData{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	return rc.Data, nil
}

func (b *builder) currency(r quote.Ref, path string) (kernel.Currency, error) {
	d, err := b.config(r, path)
	if err != nil || d.Currency == nil {
		return kernel.Currency{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	c, err := kernel.NewCurrency(d.Currency.Code, d.Currency.MinorUnits)
	if err != nil {
		return kernel.Currency{}, kernel.Reject(kernel.CodeReferenceInvalid, path, err.Error())
	}
	return c, nil
}

// currencyOf is the quote or comparison currency of code; another code has no definition here,
// which the calculation refuses before it needs one.
func (b *builder) currencyOf(code string) kernel.Currency {
	switch code {
	case b.quoteCurrency.Code():
		return b.quoteCurrency
	case b.comparison.Code():
		return b.comparison
	}
	c, _ := kernel.NewCurrency(code, 0)
	return c
}

func (b *builder) unit(r quote.Ref, path string) (calc.Unit, error) {
	d, err := b.config(r, path)
	if err != nil || d.Unit == nil {
		return calc.Unit{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	return calc.Unit{Code: d.Unit.Code, Precision: d.Unit.Precision}, nil
}

func (b *builder) tax(r quote.Ref, path string) (*calc.TaxApplication, error) {
	d, err := b.config(r, path)
	if err != nil || d.TaxApplication == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	t := d.TaxApplication
	profileRef := quote.Ref{Kind: t.TaxProfile.Kind, ID: t.TaxProfile.ConfigID, Revision: t.TaxProfile.Revision}
	pd, err := b.config(profileRef, path)
	if err != nil || pd.TaxProfile == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "The tax profile it applies is unknown")
	}
	rate, err := kernel.ParseDecimal(pd.TaxProfile.Rate, kernel.FactorFormat)
	if err != nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	return &calc.TaxApplication{Ref: quote.CalcRef(r), Side: t.Side, SubjectKind: t.SubjectKind, SubjectID: t.SubjectID, PartyID: t.PartyID,
		Profile: calc.TaxProfile{Ref: quote.CalcRef(profileRef), Rate: rate, PriceBasis: calc.PriceBasis(pd.TaxProfile.PriceBasis),
			Recovery: pd.TaxProfile.RecoveryTreatment}}, nil
}

func parse(s string, f kernel.DecimalFormat, path string) (kernel.Decimal, error) {
	d, err := kernel.ParseDecimal(s, f)
	if err != nil {
		return kernel.Decimal{}, kernel.Reject(kernel.CodeSchemaInvalid, path, "")
	}
	return d, nil
}

// inputs resolves the draft, which SealDraft found complete with every reference known and in
// effect, into calc's Quote and Facts.
func (b *builder) inputs() (calc.Quote, calc.Facts, error) {
	in, d, p := b.in, b.in.Data, dataPath
	ctx := in.Context
	if ctx == nil {
		return calc.Quote{}, calc.Facts{}, kernel.Reject(kernel.CodeReferenceInvalid, p+"/cost_snapshot_ref",
			"The costing context of the cost snapshot is not visible")
	}
	var err error
	if b.quoteCurrency, err = b.currency(*d.Currency, p+"/currency_ref"); err != nil {
		return calc.Quote{}, calc.Facts{}, err
	}
	b.comparisonRef = quote.Ref{Kind: ctx.CalculationCurrencyRef.Kind, ID: ctx.CalculationCurrencyRef.ID, Revision: ctx.CalculationCurrencyRef.Revision}
	if b.comparison, err = b.currency(b.comparisonRef, p+"/cost_snapshot_ref"); err != nil {
		return calc.Quote{}, calc.Facts{}, err
	}
	pd, err := b.config(*d.PricingPolicy, p+"/pricing_policy_ref")
	if err != nil || pd.PricingPolicy == nil {
		return calc.Quote{}, calc.Facts{}, kernel.Reject(kernel.CodeReferenceInvalid, p+"/pricing_policy_ref", "")
	}
	minimum, err := parse(pd.PricingPolicy.MinimumMargin, kernel.FactorFormat, p+"/pricing_policy_ref")
	if err != nil {
		return calc.Quote{}, calc.Facts{}, err
	}
	policyCurrency := b.currencyOf(pd.PricingPolicy.CalculationCurrency)
	if policyCurrency.Code() != b.comparison.Code() {
		policyCurrency, _ = kernel.NewCurrency(pd.PricingPolicy.CalculationCurrency, b.comparison.MinorUnits())
	}
	f := calc.Facts{At: in.At, Profile: calc.Profile{TradeTerms: quote.CalcRef(in.Profile.TradeTerms), PaymentTerms: quote.CalcRef(in.Profile.PaymentTerms)},
		Policy: calc.Policy{Ref: quote.CalcRef(*d.PricingPolicy), CalculationCurrency: policyCurrency, MinimumMargin: minimum,
			MaxFXAge: time.Duration(pd.PricingPolicy.MaxFXAgeSeconds) * time.Second}}

	f.Rfq = calc.Rfq{Customer: in.Rfq.CustomerID, Contact: in.Rfq.ContactID}
	for _, l := range in.Rfq.Lines {
		q, err := parse(l.Quantity, kernel.QuantityFormat, p+"/rfq_ref")
		if err != nil {
			return calc.Quote{}, calc.Facts{}, kernel.Reject(kernel.CodeReferenceInvalid, p+"/rfq_ref", "An RFQ line has no quantity")
		}
		f.Rfq.Lines = append(f.Rfq.Lines, calc.RfqLine{LineID: l.LineID, SkuID: l.SkuID, Quantity: q, UOM: l.UOM})
	}

	if f.Cost, err = b.cost(); err != nil {
		return calc.Quote{}, calc.Facts{}, err
	}
	if f.Stages, err = b.payments(*d.PaymentTerms); err != nil {
		return calc.Quote{}, calc.Facts{}, err
	}
	if d.SalesFX != nil {
		path := p + "/sales_fx_ref"
		fd, err := b.config(*d.SalesFX, path)
		if err != nil || fd.FXRate == nil {
			return calc.Quote{}, calc.Facts{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
		}
		rate, err := kernel.ParseDecimal(fd.FXRate.Rate, kernel.FactorFormat)
		if err != nil {
			return calc.Quote{}, calc.Facts{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
		}
		observed, err := kernel.ParseTimestamp(fd.FXRate.ObservedAt)
		if err != nil {
			return calc.Quote{}, calc.Facts{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
		}
		f.FX = &calc.FXRate{Ref: quote.CalcRef(*d.SalesFX), From: b.currencyOf(fd.FXRate.FromCurrency), To: b.currencyOf(fd.FXRate.ToCurrency),
			Rate: rate, Purpose: fd.FXRate.Purpose, ObservedAt: observed}
	}

	q := calc.Quote{Path: p, Rfq: calc.RfqRef{ID: d.Rfq.RfqID, RowVersion: d.Rfq.RowVersion}, Customer: d.Customer.PartyID,
		Contact: d.Contact.ContactID, Currency: b.quoteCurrency, TradeTerms: quote.CalcRef(*d.TradeTerms),
		PaymentTerms: quote.CalcRef(*d.PaymentTerms), NamedPlace: quote.CalcPlace(*d.NamedPlace), Delivery: quote.CalcDelivery(*d.Delivery)}
	for i, l := range d.Lines {
		line, err := b.line(l, fmt.Sprintf("%s/lines/%d", p, i))
		if err != nil {
			return calc.Quote{}, calc.Facts{}, err
		}
		q.Lines = append(q.Lines, line)
	}
	for i, c := range d.Charges {
		charge, err := b.charge(c, fmt.Sprintf("%s/charges/%d", p, i))
		if err != nil {
			return calc.Quote{}, calc.Facts{}, err
		}
		q.Charges = append(q.Charges, charge)
	}
	return q, f, nil
}

// cost is the adopted snapshot with the costing context it froze: its kind, comparison basis, lines
// and total in the comparison currency.
func (b *builder) cost() (calc.Cost, error) {
	path := dataPath + "/cost_snapshot_ref"
	ctx, s := b.in.Context, b.in.Snapshot
	basis := ctx.ComparisonBasis
	total, err := kernel.ParseMinor(b.currencyOf(s.Currency), s.TotalMinor)
	if err != nil {
		return calc.Cost{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "The cost snapshot has no total")
	}
	c := calc.Cost{ContextKind: ctx.Kind, CostBasis: basis.CostBasis, Rfq: calc.RfqRef{ID: s.Rfq.RfqID, RowVersion: s.Rfq.RowVersion},
		PricingPolicy: cstRef(ctx.PricingPolicyRef), TradeTerms: cstRef(basis.SalesTradeTermsRef), PaymentTerms: cstRef(basis.SalesPaymentTermsRef),
		NamedPlace: placeRef(basis.NamedPlaceRef), Delivery: cstDelivery(basis.Delivery), Total: total,
		ExcludedCostCategories: excludedCostCategories[basis.CostBasis]}
	for _, l := range ctx.Lines {
		q, err := parse(l.Quantity, kernel.QuantityFormat, path)
		if err != nil {
			return calc.Cost{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "A costing line has no quantity")
		}
		u, err := b.unit(quote.Ref{Kind: l.UnitRef.Kind, ID: l.UnitRef.ID, Revision: l.UnitRef.Revision}, path)
		if err != nil {
			return calc.Cost{}, err
		}
		c.Lines = append(c.Lines, calc.CostLine{LineID: l.LineID, SkuID: l.SkuID, SpecRevision: l.SkuSpecRevision, Quantity: q, Unit: u})
	}
	return c, nil
}

// payments are the stages of the payment terms, with what the document records of each.
func (b *builder) payments(r quote.Ref) ([]calc.Stage, error) {
	path := dataPath + "/payment_terms_ref"
	d, err := b.config(r, path)
	if err != nil || d.PaymentTerms == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	terms := d.PaymentTerms
	var calendar *quote.Ref
	if c := terms.Calendar; c != nil {
		calendar = &quote.Ref{Kind: c.Kind, ID: c.ConfigID, Revision: c.Revision}
	}
	b.stages = map[string]quote.Stage{}
	var out []calc.Stage
	for _, s := range terms.Stages {
		percent, err := parse(s.Percent, polapi.PercentFormat, path)
		if err != nil {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
		}
		out = append(out, calc.Stage{StageID: s.StageID, Percent: percent})
		b.stages[s.StageID] = quote.Stage{Trigger: s.Trigger, Days: s.Days, DayBasis: terms.DayBasis, Calendar: calendar,
			ReleaseControl: s.ReleaseControl}
	}
	return out, nil
}

func (b *builder) line(l quote.Line, path string) (calc.Line, error) {
	out := calc.Line{Path: path, LineID: l.LineID, RfqLineID: l.RfqLineID, SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision}
	var err error
	if out.Quantity, err = parse(l.Quantity, kernel.QuantityFormat, path+"/quantity"); err != nil {
		return calc.Line{}, err
	}
	if out.Unit, err = b.unit(l.Unit, path+"/unit_ref"); err != nil {
		return calc.Line{}, err
	}
	if l.BaseQuantity != "" {
		base, err := parse(l.BaseQuantity, kernel.QuantityFormat, path+"/base_quantity")
		if err != nil {
			return calc.Line{}, err
		}
		out.BaseQuantity = &base
	}
	if r := l.Conversion; r != nil {
		cp := path + "/conversion_ref"
		d, err := b.config(*r, cp)
		if err != nil || d.UnitConversion == nil {
			return calc.Line{}, kernel.Reject(kernel.CodeReferenceInvalid, cp, "")
		}
		u := d.UnitConversion
		factor, err := kernel.ParseDecimal(u.Factor, kernel.FactorFormat)
		if err != nil {
			return calc.Line{}, kernel.Reject(kernel.CodeReferenceInvalid, cp, "")
		}
		out.Conversion = &calc.Conversion{Ref: quote.CalcRef(*r), SkuID: u.SkuID, SpecRevision: u.SpecRevision, From: u.FromUnit, To: u.ToUnit,
			Factor: factor}
	}
	if out.UnitPrice, err = parse(l.UnitPrice, kernel.PriceFormat, path+"/unit_price"); err != nil {
		return calc.Line{}, err
	}
	if out.PriceBasisQuantity, err = parse(l.PriceBasisQuantity, kernel.PriceFormat, path+"/price_basis_quantity"); err != nil {
		return calc.Line{}, err
	}
	if out.Discount, err = parse(l.DiscountRatio, kernel.PriceFormat, path+"/discount_ratio"); err != nil {
		return calc.Line{}, err
	}
	if out.Tax, err = b.tax(l.TaxApplication, path+"/tax_application_ref"); err != nil {
		return calc.Line{}, err
	}
	return out, nil
}

func (b *builder) charge(c quote.Charge, path string) (calc.Charge, error) {
	out := calc.Charge{Path: path, ChargeID: c.ChargeID, ObligationKey: c.ObligationKey, Inclusion: c.Inclusion, IncludedLineID: c.IncludedLineID}
	fp := path + "/fee_category_ref"
	d, err := b.config(c.FeeCategory, fp)
	if err != nil || d.FeeCategory == nil {
		return calc.Charge{}, kernel.Reject(kernel.CodeReferenceInvalid, fp, "")
	}
	out.Fee = &calc.FeeCategory{Ref: quote.CalcRef(c.FeeCategory), Side: d.FeeCategory.Side, Inclusion: d.FeeCategory.Inclusion}
	if out.Amount, err = parse(c.Amount, kernel.PriceFormat, path+"/amount"); err != nil {
		return calc.Charge{}, err
	}
	for _, e := range c.Evidence {
		out.Evidence = append(out.Evidence, calc.Evidence{SourceID: e.SourceID, Locator: e.Locator})
	}
	if out.Tax, err = b.tax(c.TaxApplication, path+"/tax_application_ref"); err != nil {
		return calc.Charge{}, err
	}
	return out, nil
}

func cstRef(r cstapi.ConfigRef) calc.Ref {
	return calc.Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func placeRef(r cstapi.BusinessRef) calc.Ref {
	return calc.Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func cstDelivery(d cstapi.Delivery) calc.Delivery {
	out := calc.Delivery{Kind: d.Kind, Date: d.Date, Days: d.Days, DayBasis: d.DayBasis, Milestone: d.Milestone, Place: placeRef(d.PlaceRef),
		TimeZone: d.TimeZone}
	if d.AnchorRef != nil {
		out.Anchor = placeRef(*d.AnchorRef)
	}
	if d.CalendarRef != nil {
		out.Calendar = cstRef(*d.CalendarRef)
	}
	return out
}

// adoptedRefs are the POL revisions the calculation reads of the costing context beyond the draft's
// references: the comparison currency and the cost units.
func adoptedRefs(ctx *cstapi.VerifiedContext) []quote.Ref {
	if ctx == nil {
		return nil
	}
	out := []quote.Ref{{Kind: ctx.CalculationCurrencyRef.Kind, ID: ctx.CalculationCurrencyRef.ID, Revision: ctx.CalculationCurrencyRef.Revision}}
	for _, l := range ctx.Lines {
		out = append(out, quote.Ref{Kind: l.UnitRef.Kind, ID: l.UnitRef.ID, Revision: l.UnitRef.Revision})
	}
	return out
}
