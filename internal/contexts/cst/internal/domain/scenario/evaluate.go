package scenario

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/budget"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/costing"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/delivery"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/rules"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// POL configuration kinds an evaluation reads.
const (
	polCurrency       = "currency"
	polPricingPolicy  = "pricing_policy"
	polUnit           = "unit"
	polUnitConversion = "unit_conversion"
	polFXRate         = "fx_rate"
	polCalendar       = "calendar"
	polTradeTerms     = "trade_terms"
	polPaymentTerms   = "payment_terms"
	polTaxProfile     = "tax_profile"
	polTaxApplication = "tax_application"
	polFeeCategory    = "fee_category"
)

// Roles of a delivery check and statuses of a commercial check.
const (
	RoleRequirement = "requirement"
	RoleSupply      = "supply"

	CheckMatched           = "matched"
	CheckNormalized        = "normalized"
	CheckDifferentCashflow = "different_cashflow"
	CheckCustomerBorne     = "customer_borne"
	CheckMismatch          = "mismatch"
)

// Computed is an evaluation as Evaluate computes it: the normalized input and the result, each with
// its canonical JSON and SHA-256, and the inputs it bound.
type Computed struct {
	Input       NormalizedInput
	InputBytes  []byte
	InputHash   string
	Result      Result
	ResultBytes []byte
	ResultHash  string
}

// Evaluate computes the evaluation of in (Python integrated_model._cost, T02 §13.3) or refuses it
// with the code and JSON Pointer of the first check that fails, in Python's order:
//
//  1. the scenario is complete; it draws on at most 200 supply condition revisions and names at
//     most 1000 references (resource budget);
//  2. the costing context revision is the active one (REFERENCE_STALE), its RFQ still ready at the
//     bound row version with the bound line set (REFERENCE_STALE), an actual context covers the
//     RFQ's lines (a what_if context is provisional and, with other quantities, not comparable);
//  3. calculation currency, pricing policy and requested currency in force; the sales payment and
//     trade terms; the customer's requested terms against the sales terms (a difference is not
//     comparable, never assumed equal); the policy calculates in the context's currency;
//  4. the delivery requirement resolved to a date, or unresolved (provisional, not comparable);
//  5. each demand line: its unit in force, a quantity exact at its precision, its SKU
//     specification revision current;
//  6. each allocation in order: its demand line, its supply condition revision in force and
//     answering the context's RFQ, its purchase unit in force; then rules.Purchase (MOQ, demand
//     coverage, order multiples, tiers) prices each condition line once;
//  7. the exchange rates the scenario names;
//  8. each priced condition line: its trade and payment terms (the cash flow comparison is
//     recorded, never priced), its goods tax application, its delivery against the requirement,
//     then tax, conversion and allocation (rules.Cost, PurchaseGroup.Allocate);
//  9. each logistics offer revision in force with its destination at the named place, its fees by
//     fee ID; the charge graph (rules.PlanCharges: double counts, inclusions); each fee's service
//     tax application; a payable fee measured, billed, taxed, converted and allocated;
//  10. every fee type the comparison basis requires is accounted for (COST_INPUT_NOT_READY);
//  11. the context's assumptions bound to the selected documents (provisional);
//  12. the total within the business bound; the resource budget.
//
// What does not refuse the calculation is recorded: differences make the result not comparable,
// unverified assumptions and unresolved deliveries make it provisional.
func Evaluate(in Inputs) (Computed, error) {
	e := &engine{in: in, now: kernel.Instant(in.Now), budget: budget.New(), comparable: true,
		bound: map[BoundInput]bool{}, currencies: map[string]kernel.Currency{}, units: map[string]Unit{}}
	// The minor units of every currency POL defined for the inputs, so an exchange rate names its
	// currencies exactly whichever input is costed first; the highest revision decides.
	refs := slices.SortedFunc(maps.Keys(in.Configs), compareRef)
	for _, r := range refs {
		if c := in.Configs[r].Currency; c != nil {
			if cur, err := kernel.NewCurrency(c.Code, c.MinorUnits); err == nil {
				e.currencies[cur.Code()] = cur
			}
		}
	}
	result, err := e.run()
	if err != nil {
		return Computed{}, err
	}
	inputs := make([]BoundInput, 0, len(e.bound))
	for b := range e.bound {
		inputs = append(inputs, b)
	}
	slices.SortFunc(inputs, compareInput)
	ni := NormalizedInput{NormalizationVersion: NormalizationVersion, ScenarioID: in.ScenarioID, Data: in.Data.Normalized(), Inputs: inputs}
	c := Computed{Input: ni, InputBytes: canonical(ni)}
	c.InputHash = HashBytes(c.InputBytes)
	result.EngineVersion, result.InputHash = EngineVersion, c.InputHash
	c.Result, c.ResultBytes = result, canonical(result)
	c.ResultHash = HashBytes(c.ResultBytes)
	return c, nil
}

type engine struct {
	in          Inputs
	now         time.Time
	budget      *budget.Budget
	bound       map[BoundInput]bool
	provisional bool
	comparable  bool

	ctxID      string
	rev        *costing.Revision
	ctx        costing.Data
	basis      costing.Basis
	currency   kernel.Currency
	currencies map[string]kernel.Currency // by code, from the currency configurations read
	units      map[string]Unit            // by code, from the unit configurations read
	policy     PricingPolicy
	fx         rules.FX
	salePay    []rules.PaymentStage
	required   delivery.Rule
	reqRes     delivery.Resolution

	differences []Difference
	deliveries  []DeliveryCheck
	commercial  []CommercialCheck
}

func (e *engine) bind(context, kind, id string, revision int64, hash, use string) {
	e.bound[BoundInput{Context: context, Kind: kind, ID: id, Revision: revision, ContentHash: hash, Use: use}] = true
}

func (e *engine) differ(path, reason string) {
	e.comparable = false
	e.differences = append(e.differences, Difference{Path: path, Reason: reason})
}

func (e *engine) check(path, category, status, reason string, refs ...string) {
	e.commercial = append(e.commercial, CommercialCheck{Path: path, Category: category, Status: status, Reason: reason, Refs: refs})
}

func (e *engine) mismatch(path, category, reason string, refs ...string) {
	e.differ(path, reason)
	e.check(path, category, CheckMismatch, reason, refs...)
}

// cfg resolves a POL configuration of kind at path: known (REFERENCE_INVALID), of the kind the
// field takes (REFERENCE_INVALID) and in force (POL's CONFIG_NOT_ACTIVE or CONFIG_EXPIRED).
func (e *engine) cfg(r Ref, kind, path string) (Config, error) {
	if err := e.budget.Tick(); err != nil {
		return Config{}, err
	}
	if r.Kind != kind {
		return Config{}, fail(kernel.CodeReferenceInvalid, path, fmt.Sprintf("The reference is not a %s", kind))
	}
	c, ok := e.in.Configs[r]
	if !ok || !c.holds(kind) {
		return Config{}, fail(kernel.CodeReferenceInvalid, path, "")
	}
	if c.Check != nil {
		if err := c.Check(path); err != nil {
			return Config{}, err
		}
	}
	e.bind(FromPOL, r.Kind, r.ID, r.Revision, c.ContentHash, UseInput)
	if err := e.budget.Observe(budget.Key{Kind: r.Kind, ID: r.ID, Version: r.Revision}, int64(len(canonical(c.content()))), c.refs(), 0); err != nil {
		return Config{}, err
	}
	switch {
	case c.Currency != nil:
		if cur, err := kernel.NewCurrency(c.Currency.Code, c.Currency.MinorUnits); err == nil {
			e.currencies[cur.Code()] = cur
		}
	case c.Unit != nil:
		e.units[c.Unit.Code] = *c.Unit
	}
	return c, nil
}

func (c Config) holds(kind string) bool {
	switch kind {
	case polCurrency:
		return c.Currency != nil
	case polPricingPolicy:
		return c.PricingPolicy != nil
	case polUnit:
		return c.Unit != nil
	case polUnitConversion:
		return c.Conversion != nil
	case polFXRate:
		return c.FXRate != nil
	case polCalendar:
		return c.Calendar != nil
	case polTradeTerms:
		return c.TradeTerms != nil
	case polPaymentTerms:
		return c.PaymentTerms != nil
	case polTaxProfile:
		return c.TaxProfile != nil
	case polTaxApplication:
		return c.TaxApplication != nil
	case polFeeCategory:
		return c.FeeCategory != nil
	}
	return false
}

func (c Config) content() any {
	for _, v := range []any{c.Currency, c.Unit, c.Conversion, c.FXRate, c.Calendar, c.TradeTerms, c.PaymentTerms, c.TaxProfile,
		c.TaxApplication, c.FeeCategory, c.PricingPolicy} {
		switch x := v.(type) {
		case *Currency:
			if x != nil {
				return x
			}
		case *Unit:
			if x != nil {
				return x
			}
		case *Conversion:
			if x != nil {
				return x
			}
		case *FXRate:
			if x != nil {
				return x
			}
		case *Calendar:
			if x != nil {
				return x
			}
		case *TradeTerms:
			if x != nil {
				return x
			}
		case *PaymentTerms:
			if x != nil {
				return x
			}
		case *TaxProfile:
			if x != nil {
				return x
			}
		case *TaxApplication:
			if x != nil {
				return x
			}
		case *FeeCategory:
			if x != nil {
				return x
			}
		case *PricingPolicy:
			if x != nil {
				return x
			}
		}
	}
	return nil
}

func (c Config) refs() []budget.Key {
	var out []budget.Key
	add := func(r Ref) { out = append(out, budget.Key{Kind: r.Kind, ID: r.ID, Version: r.Revision}) }
	switch {
	case c.TaxApplication != nil:
		add(c.TaxApplication.TaxProfile)
	case c.PricingPolicy != nil:
		add(c.PricingPolicy.TaxProfile)
	case c.PaymentTerms != nil && c.PaymentTerms.Calendar != nil:
		add(*c.PaymentTerms.Calendar)
	}
	return out
}

func (e *engine) unit(r Ref, path string) (Unit, error) {
	c, err := e.cfg(r, polUnit, path)
	if err != nil {
		return Unit{}, err
	}
	return *c.Unit, nil
}

func (e *engine) currencyOf(r Ref, path string) (kernel.Currency, error) {
	c, err := e.cfg(r, polCurrency, path)
	if err != nil {
		return kernel.Currency{}, err
	}
	cur, err := kernel.NewCurrency(c.Currency.Code, c.Currency.MinorUnits)
	if err != nil {
		return kernel.Currency{}, fail(kernel.CodeReferenceInvalid, path, "The currency definition is not usable")
	}
	return cur, nil
}

func (e *engine) taxOf(r Ref, path string) (rules.TaxTreatment, error) {
	c, err := e.cfg(r, polTaxProfile, path)
	if err != nil {
		return rules.TaxTreatment{}, err
	}
	rate, err := parse(c.TaxProfile.Rate, kernel.FactorFormat, path)
	if err != nil {
		return rules.TaxTreatment{}, err
	}
	return rules.TaxTreatment{Path: path, Rate: rate, PriceBasis: rules.PriceBasis(c.TaxProfile.PriceBasis),
		Recovery: rules.Recovery(c.TaxProfile.Recovery)}, nil
}

// payment resolves payment terms and the calendar their days count on.
func (e *engine) payment(r Ref, path string) ([]rules.PaymentStage, error) {
	c, err := e.cfg(r, polPaymentTerms, path)
	if err != nil {
		return nil, err
	}
	p := c.PaymentTerms
	calendar := ""
	if p.Calendar != nil {
		if _, err := e.cfg(*p.Calendar, polCalendar, path); err != nil {
			return nil, err
		}
		calendar = p.Calendar.String()
	}
	out := make([]rules.PaymentStage, len(p.Stages))
	for i, s := range p.Stages {
		percent, err := kernel.ParseDecimal(s.Percent, kernel.FactorFormat)
		if err != nil {
			return nil, fail(kernel.CodeInvalidKnownValue, path, "A payment stage's percentage is not a decimal")
		}
		out[i] = rules.PaymentStage{Percent: percent, Trigger: s.Trigger, Days: s.Days, DayBasis: p.DayBasis, Calendar: calendar}
	}
	return out, nil
}

// supply resolves a supply condition revision at path: known (REFERENCE_INVALID) and in force
// (SUP's SUPPLY_CONDITION_NOT_CURRENT or SUPPLY_CONDITION_EXPIRED).
func (e *engine) supply(r Ref, path string) (Supply, error) {
	if err := e.budget.Tick(); err != nil {
		return Supply{}, err
	}
	s, ok := e.in.Supply[r]
	if !ok || (r.Kind == KindSupplierOffer) != (s.Offer != nil) || (r.Kind == KindPurchaseTerms) != (s.Terms != nil) ||
		(r.Kind == KindLogisticsOffer) != (s.Logistics != nil) {
		return Supply{}, fail(kernel.CodeReferenceInvalid, path, "No verified supply condition revision of this kind is visible")
	}
	if s.Check != nil {
		if err := s.Check(path); err != nil {
			return Supply{}, err
		}
	}
	e.bind(FromSUP, r.Kind, r.ID, r.Revision, s.ContentHash, UseInput)
	if t := s.Terms; t != nil && t.SourceOffer != nil {
		if o, ok := e.in.Supply[*t.SourceOffer]; ok {
			use := UseProvenance
			if t.ValidityMode == "dependent_validity" {
				use = UseInput // SUP's verdict on the terms covers the source offer's effect
			}
			e.bind(FromSUP, t.SourceOffer.Kind, t.SourceOffer.ID, t.SourceOffer.Revision, o.ContentHash, use)
		}
	}
	if err := e.budget.Observe(budget.Key{Kind: r.Kind, ID: r.ID, Version: r.Revision}, int64(len(canonical(s.content()))), nil, 0); err != nil {
		return Supply{}, err
	}
	return s, nil
}

func (s Supply) content() any {
	switch {
	case s.Offer != nil:
		return s.Offer
	case s.Terms != nil:
		return s.Terms
	}
	return s.Logistics
}

func (s Supply) lines() []ConditionLine {
	switch {
	case s.Offer != nil:
		return s.Offer.Lines
	case s.Terms != nil:
		return s.Terms.Lines
	}
	return nil
}

func (s Supply) supplier() string {
	switch {
	case s.Offer != nil:
		return s.Offer.SupplierID
	case s.Terms != nil:
		return s.Terms.SupplierID
	}
	return ""
}

func parse(s string, f kernel.DecimalFormat, path string) (kernel.Decimal, error) {
	d, err := kernel.ParseDecimal(s, f)
	if err != nil {
		return kernel.Decimal{}, fail(kernel.CodeInvalidKnownValue, path, "Not a decimal of the contract format")
	}
	return d, nil
}

// parseOptional parses s, or returns zero for an omitted value.
func parseOptional(s string, f kernel.DecimalFormat, path string) (kernel.Decimal, error) {
	if s == "" {
		return kernel.Decimal{}, nil
	}
	return parse(s, f, path)
}

func need(r *Ref, path string) (Ref, error) {
	if r == nil {
		return Ref{}, fail(CodeCostInputNotReady, path, "The input is missing; it is never assumed")
	}
	return *r, nil
}

func skuKey(id string, revision int64) string { return id + "@" + strconv.FormatInt(revision, 10) }

func ref(r costing.Ref) Ref { return Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision} }

func (e *engine) run() (Result, error) {
	d := e.in.Data
	if err := d.Complete(); err != nil {
		return Result{}, err
	}
	sources := d.Supplies()
	keys := make([]budget.Key, 0, len(sources))
	for _, s := range sources {
		keys = append(keys, budget.Key{Kind: s.Kind, ID: s.ID, Version: s.Revision})
	}
	if err := budget.CandidateSources(keys); err != nil {
		return Result{}, fail(CodeCalculationLimitExceeded, "/args/data/allocations", "A scenario draws on at most 200 supply condition revisions")
	}
	direct := append([]budget.Key{{Kind: KindCostingContext, ID: d.Context.ContextID, Version: d.Context.Revision}}, keys...)
	for _, r := range append(slices.Clone(d.LogisticsOffers), d.FX...) {
		direct = append(direct, budget.Key{Kind: r.Kind, ID: r.ID, Version: r.Revision})
	}
	for _, a := range d.Allocations {
		direct = append(direct, budget.Key{Kind: a.PurchaseUnit.Kind, ID: a.PurchaseUnit.ID, Version: a.PurchaseUnit.Revision})
	}
	if err := budget.DirectRefs(direct); err != nil {
		return Result{}, fail(CodeCalculationLimitExceeded, "/args/data", "A scenario names at most 1000 distinct references")
	}
	if err := e.budget.Observe(budget.Key{Kind: "cost_scenario", ID: e.in.ScenarioID}, int64(len(canonical(d.Normalized()))), direct, 0); err != nil {
		return Result{}, err
	}

	if err := e.context(); err != nil {
		return Result{}, err
	}
	if err := e.foundation(); err != nil {
		return Result{}, err
	}
	if err := e.requirement(); err != nil {
		return Result{}, err
	}
	demand, err := e.demand()
	if err != nil {
		return Result{}, err
	}
	groups, conditions, err := e.purchase(demand)
	if err != nil {
		return Result{}, err
	}
	if err = e.rates(); err != nil {
		return Result{}, err
	}
	r := Result{ScenarioID: e.in.ScenarioID, ContextRef: *d.Context, ContextKind: e.ctx.Kind, Currency: e.currency.Code(),
		MinorUnits: e.currency.MinorUnits(), CostBasis: e.basis.CostBasis, ExcludedCostCategories: slices.Clone(rules.ExcludedCostCategories),
		SelectionReason: d.SelectionReason, Components: []Component{}, ExcludedComponents: []Excluded{}, Assumptions: []BoundAssumption{},
		Purchases: []PurchaseQuantity{}, Charges: []ChargeObligation{}}
	seal := e.rev.Seal
	r.Rfq = RfqBinding{RfqID: seal.Rfq.RfqID, RowVersion: seal.Rfq.RowVersion, LinesHash: seal.Rfq.LinesHash}

	purchases, base, amounts, err := e.costPurchases(groups, conditions, &r)
	if err != nil {
		return Result{}, err
	}
	charges, err := e.costCharges(purchases, demand, base, &r)
	if err != nil {
		return Result{}, err
	}
	amounts = append(amounts, charges...)
	if err = e.assumptions(&r); err != nil {
		return Result{}, err
	}
	total, err := rules.Total(e.currency, "/result/total_minor", amounts...)
	if err != nil {
		return Result{}, err
	}
	report, err := e.budget.Finish()
	if err != nil {
		return Result{}, err
	}
	r.TotalMinor = total.MinorString()
	r.Budget = BudgetUsage(report)
	r.Lines = lineTotals(demand, r.Components, e.currency)
	if err = checkOutput(r); err != nil {
		return Result{}, err
	}
	r.VerificationStatus, r.ComparisonStatus = Verified, Comparable
	if e.provisional {
		r.VerificationStatus = Provisional
	}
	if !e.comparable {
		r.ComparisonStatus = NotComparable
	}
	r.Differences = sortedDifferences(e.differences)
	r.DeliveryChecks = slices.SortedFunc(slices.Values(e.deliveries), func(a, b DeliveryCheck) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Role, b.Role))
	})
	r.CommercialChecks = slices.SortedFunc(slices.Values(e.commercial), func(a, b CommercialCheck) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Category, b.Category), cmp.Compare(strings.Join(a.Refs, " "), strings.Join(b.Refs, " ")))
	})
	slices.SortFunc(r.Components, func(a, b Component) int { return cmp.Compare(a.ComponentID, b.ComponentID) })
	slices.SortFunc(r.ExcludedComponents, func(a, b Excluded) int { return cmp.Compare(a.ComponentID, b.ComponentID) })
	return r, nil
}

func sortedDifferences(ds []Difference) []Difference {
	out := slices.SortedFunc(slices.Values(ds), func(a, b Difference) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Reason, b.Reason))
	})
	if out == nil {
		out = []Difference{}
	}
	return slices.Compact(out)
}

// context admits the costing context revision and the RFQ it bound (Python _cost: the context
// business revision, _rfq, the line comparison).
func (e *engine) context() error {
	want := *e.in.Data.Context
	const p = "/args/data/context_ref"
	c := e.in.Context
	if c == nil || c.Revision == nil || c.ID != want.ContextID || c.Revision.Number != want.Revision {
		// Python cost_scenario.evaluate: a context revision that is not the active one is stale.
		return fail(CodeReferenceStale, p, "The costing context revision is not active")
	}
	rev := c.Revision
	if rev.Status != costing.Active || rev.Seal == nil {
		return fail(CodeReferenceStale, p, "The costing context revision is not active; evaluate on its active revision")
	}
	if err := e.budget.Tick(); err != nil {
		return err
	}
	e.ctxID, e.rev, e.ctx = c.ID, rev, rev.Data
	if e.ctx.Basis == nil || e.ctx.CalculationCurrency == nil || e.ctx.PricingPolicy == nil || e.ctx.Rfq == nil {
		return fail(CodeCostInputNotReady, p, "The costing context revision is incomplete")
	}
	e.basis = *e.ctx.Basis
	e.bind(FromCST, KindCostingContext, c.ID, rev.Number, rev.Hash, UseInput)
	var edges []budget.Key
	for _, r := range e.ctx.ConfigRefs() {
		edges = append(edges, budget.Key{Kind: r.Kind, ID: r.ID, Version: r.Revision})
	}
	if err := e.budget.Observe(budget.Key{Kind: KindCostingContext, ID: c.ID, Version: rev.Number}, int64(len(canonical(e.ctx))), edges, 0); err != nil {
		return err
	}

	b := rev.Seal.Rfq
	r := e.in.Rfq
	if r == nil || r.RfqID != b.RfqID || !r.Ready || r.RowVersion != b.RowVersion || r.LinesHash != b.LinesHash {
		return fail(CodeReferenceStale, "/context/rfq_ref", "The RFQ is no longer ready at the row version with the line set the costing context bound")
	}
	e.bind(FromINQ, "rfq", b.RfqID, b.RowVersion, b.LinesHash, UseInput)

	rfqLines := map[string]costing.RfqLine{}
	for _, l := range rev.Seal.RfqLines {
		rfqLines[l.LineID] = l
	}
	if e.ctx.Kind == costing.WhatIf {
		e.provisional = true // Python: a what_if context never yields a verified result for its RFQ
	}
	same := len(rfqLines) == len(e.ctx.Lines)
	for _, l := range e.ctx.Lines {
		rl, ok := rfqLines[l.LineID]
		if !ok || rl.SkuID != l.SkuID || rl.SkuSpecRevision != l.SkuSpecRevision {
			return fail(CodeComparisonBasisMismatch, "/context/lines", "The costing context does not cover the RFQ's lines")
		}
		a, err1 := kernel.ParseDecimal(rl.Quantity, kernel.QuantityFormat)
		q, err2 := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		same = same && err1 == nil && err2 == nil && a.Equal(q)
	}
	if !same {
		if e.ctx.Kind == costing.Actual {
			return fail(CodeComparisonBasisMismatch, "/context/lines", "An actual costing context costs the RFQ's complete line set")
		}
		e.differ("/context/lines", "What-if quantities differ from the original customer RFQ; not eligible for its formal quote")
	}
	return nil
}

// foundation admits the context's currency, policy and sales terms (Python _cost: currency,
// pricing policy, requested currency, sales trade scope and payment terms, requested terms, the
// policy's currency).
func (e *engine) foundation() error {
	c, err := e.cfg(ref(*e.ctx.CalculationCurrency), polCurrency, "/context/calculation_currency_ref")
	if err != nil {
		return err
	}
	if e.currency, err = kernel.NewCurrency(c.Currency.Code, c.Currency.MinorUnits); err != nil {
		return fail(kernel.CodeReferenceInvalid, "/context/calculation_currency_ref", "The currency definition is not usable")
	}
	p, err := e.cfg(ref(*e.ctx.PricingPolicy), polPricingPolicy, "/context/pricing_policy_ref")
	if err != nil {
		return err
	}
	e.policy = *p.PricingPolicy
	if r := e.ctx.RequestedCurrency; r != nil {
		if _, err = e.cfg(ref(*r), polCurrency, "/context/requested_currency_ref"); err != nil {
			return err
		}
	}
	if e.salePay, err = e.payment(ref(e.basis.SalesPaymentTerms), "/context/comparison_basis/sales_payment_terms_ref"); err != nil {
		return err
	}
	e.check("/context/comparison_basis/sales_payment_terms_ref", polPaymentTerms, CheckMatched,
		"Customer receipts and supplier payments are separate obligations; nominal direct cost excludes imputed financing, expected credit loss and unrealized hedging",
		ref(e.basis.SalesPaymentTerms).String())
	if _, err = e.cfg(ref(e.basis.SalesTradeTerms), polTradeTerms, "/context/comparison_basis/sales_trade_terms_ref"); err != nil {
		return err
	}
	for _, x := range []struct {
		field  string
		asked  *costing.Ref
		actual costing.Ref
		kind   string
	}{
		{"requested_trade_terms_ref", e.ctx.RequestedTradeTerms, e.basis.SalesTradeTerms, polTradeTerms},
		{"requested_payment_terms_ref", e.ctx.RequestedPaymentTerms, e.basis.SalesPaymentTerms, polPaymentTerms},
	} {
		if x.asked == nil {
			continue
		}
		path := "/context/" + x.field
		if _, err = e.cfg(ref(*x.asked), x.kind, path); err != nil {
			return err
		}
		if *x.asked != x.actual {
			e.mismatch(path, x.kind, "Customer requirement differs from the explicit sales basis; no equivalence was assumed",
				ref(*x.asked).String(), ref(x.actual).String())
		} else {
			e.check(path, x.kind, CheckMatched, "Customer requirement is bound to the same sales revision", ref(x.actual).String())
		}
	}
	if e.policy.CalculationCurrency != e.currency.Code() {
		return fail(kernel.CodeReferenceInvalid, "/context/pricing_policy_ref", "The pricing policy calculates in another currency")
	}
	if e.basis.CostBasis != rules.CostBasisNominalDirect {
		return fail(CodeDependencyNotSupported, "/context/comparison_basis/cost_basis", "Only nominal direct cost is computed")
	}
	return nil
}

// requirement resolves the customer's delivery requirement (Python delivery(..., "requirement")).
func (e *engine) requirement() error {
	const p = "/context/comparison_basis/delivery"
	dv := e.basis.Delivery
	rule := delivery.Rule{Path: p, Kind: dv.Kind, Days: dv.Days, DayBasis: dv.DayBasis, Milestone: dv.Milestone, Place: dv.Place.ID,
		TimeZone: dv.TimeZone}
	var anchor *delivery.Anchor
	var cal *delivery.Calendar
	switch dv.Kind {
	case delivery.KindFixed:
		date, err := delivery.ParseDate(dv.Date)
		if err != nil {
			return fail(kernel.CodeSchemaInvalid, p+"/date", "")
		}
		rule.Date = date
	case delivery.KindLeadTime:
		var err error
		if dv.Anchor == nil || dv.Calendar == nil {
			return fail(CodeCostInputNotReady, p, "A lead time names its anchor and calendar")
		}
		if anchor, err = e.anchor(*dv.Anchor, p+"/anchor_ref"); err != nil {
			return err
		}
		if cal, err = e.calendar(ref(*dv.Calendar), p+"/calendar_ref"); err != nil {
			return err
		}
	}
	res, err := e.resolve(rule, anchor, cal, RoleRequirement, "")
	if err != nil {
		return err
	}
	e.required, e.reqRes = rule, res
	if dv.Place != e.basis.NamedPlace {
		e.differ(p+"/place_ref", "Delivery requirement and comparison place do not refer to the same location")
	}
	return nil
}

func (e *engine) resolve(rule delivery.Rule, anchor *delivery.Anchor, cal *delivery.Calendar, role, pending string) (delivery.Resolution, error) {
	res, err := delivery.Resolve(rule, anchor, cal, e.now)
	if err != nil {
		return delivery.Resolution{}, err
	}
	row := DeliveryCheck{Path: rule.Path, Role: role, Milestone: rule.Milestone, Place: rule.Place, TimeZone: rule.TimeZone, Status: "resolved"}
	if res.Resolved {
		row.Date = res.Date.String()
	} else {
		row.Status, row.Reason = "unresolved", res.Reason
		if pending != "" {
			row.Reason = pending
		}
		e.provisional = true
		e.differ(rule.Path, row.Reason)
	}
	e.deliveries = append(e.deliveries, row)
	return res, nil
}

// anchor resolves a delivery anchor: the verification event of a business revision (T02 §15.3).
// A revision CST or SUP holds that is not sealed has no occurrence yet: the delivery stays
// unresolved and no date is assumed. A sealed revision that is no longer the active one is stale.
func (e *engine) anchor(a costing.Ref, path string) (*delivery.Anchor, error) {
	kind, id, err := delivery.ParseAnchor(path+"/id", a.ID)
	if err != nil {
		return nil, err
	}
	if err = e.budget.Tick(); err != nil {
		return nil, err
	}
	switch kind {
	case KindCostingContext:
		c, ok := e.in.Contexts[ContextRef{ContextID: id, Revision: a.Revision}]
		if !ok {
			return nil, fail(kernel.CodeReferenceInvalid, path, "The anchor names no costing context revision")
		}
		e.bind(FromCST, kind, id, a.Revision, c.Hash, UseAnchor)
		switch {
		case !c.Sealed:
			return &delivery.Anchor{Path: path}, nil
		case c.Status != string(costing.Active):
			return nil, fail(CodeReferenceStale, path, "The anchor's revision is no longer the active one")
		}
		return &delivery.Anchor{Path: path, Verified: true, OccurredAt: c.VerifiedAt}, nil
	case KindSupplierOffer, KindPurchaseTerms, KindLogisticsOffer:
		s, ok := e.in.Supply[Ref{Kind: kind, ID: id, Revision: a.Revision}]
		if !ok {
			// SUP answers sealed revisions only: a draft has not been verified yet.
			return &delivery.Anchor{Path: path}, nil
		}
		e.bind(FromSUP, kind, id, a.Revision, s.ContentHash, UseAnchor)
		if s.Status != "active" {
			return nil, fail(CodeReferenceStale, path, "The anchor's revision is no longer the active one")
		}
		return &delivery.Anchor{Path: path, Verified: true, OccurredAt: s.VerifiedAt}, nil
	}
	return nil, fail(CodeDependencyNotSupported, path, fmt.Sprintf("Anchors on %s revisions are not mapped: PTY is not upstream of CST", kind))
}

// calendar resolves a POL calendar into a delivery calendar. A POL calendar revision covers its
// validity interval in its time zone; holidays outside it do not apply.
func (e *engine) calendar(r Ref, path string) (*delivery.Calendar, error) {
	c, err := e.cfg(r, polCalendar, path)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(c.Calendar.TimeZone)
	if err != nil || c.Calendar.TimeZone == "" || c.Calendar.TimeZone == "Local" {
		return nil, fail(kernel.CodeInvalidKnownValue, path, "Unknown IANA time zone")
	}
	cal := &delivery.Calendar{Path: path, TimeZone: c.Calendar.TimeZone, WorkingWeekdays: slices.Clone(c.Calendar.WorkingWeekdays),
		CoverageFrom: delivery.LocalDate(c.ValidFrom, loc), CoverageUntil: delivery.MustDate("9999-12-31")}
	if !c.ValidUntil.IsZero() {
		cal.CoverageUntil = delivery.LocalDate(c.ValidUntil.Add(-time.Microsecond), loc)
	}
	for _, h := range c.Calendar.Holidays {
		day, err := delivery.ParseDate(h)
		if err != nil {
			return nil, fail(kernel.CodeInvalidKnownValue, path, "A holiday is not a date")
		}
		if day.Compare(cal.CoverageFrom) >= 0 && day.Compare(cal.CoverageUntil) <= 0 {
			cal.Holidays = append(cal.Holidays, day)
		}
	}
	return cal, nil
}

// demand admits the context's lines (Python _cost: each line's SKU profile, unit, quantity and
// SKU master) and the context's other configurations.
func (e *engine) demand() ([]rules.DemandLine, error) {
	out := make([]rules.DemandLine, len(e.ctx.Lines))
	for i, l := range e.ctx.Lines {
		p := index("/context/lines", i)
		u, err := e.unit(ref(l.Unit), p+"/unit_ref")
		if err != nil {
			return nil, err
		}
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil || q.Sign() <= 0 || q.Cmp(kernel.DecimalFromInt(1_000_000)) > 0 || !q.RoundHalfUp(min(6, u.Precision)).Equal(q) {
			return nil, fail(kernel.CodeInvalidKnownValue, p+"/quantity", "The quantity is positive, at most 1000000, within its unit's precision")
		}
		spec := costing.Spec{SkuID: l.SkuID, Revision: l.SkuSpecRevision}
		standing, ok := e.in.Specs[spec]
		if !ok {
			return nil, fail(kernel.CodeReferenceInvalid, p+"/sku_spec_revision", "")
		}
		if standing.Check != nil {
			if err = standing.Check(p + "/sku_spec_revision"); err != nil {
				return nil, err
			}
		}
		e.bind(FromCAT, "sku_spec", l.SkuID, l.SkuSpecRevision, "", UseInput)
		for j, c := range l.Conversions {
			if _, err = e.cfg(ref(c), polUnitConversion, index(p+"/conversion_refs", j)); err != nil {
				return nil, err
			}
		}
		quantity, err := kernel.NewQuantity(q, kernel.UnitCode(u.Code))
		if err != nil {
			return nil, fail(kernel.CodeInvalidKnownValue, p+"/quantity", "")
		}
		out[i] = rules.DemandLine{Path: p, LineID: l.LineID, SKU: skuKey(l.SkuID, l.SkuSpecRevision), Quantity: quantity}
	}
	for i, f := range e.basis.RequiredFeeTypes {
		if _, err := e.cfg(ref(f), polFeeCategory, index("/context/comparison_basis/required_fee_type_refs", i)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// condition is a priced condition line with the document it belongs to.
type condition struct {
	ref    Ref
	supply Supply
	line   ConditionLine
	path   string
}

// purchase admits each allocation's source in order and prices the condition lines once
// (rules.Purchase, CST-INV-005).
func (e *engine) purchase(demand []rules.DemandLine) ([]rules.PurchaseGroup, map[string]condition, error) {
	lines := map[string]bool{}
	for _, l := range demand {
		lines[l.LineID] = true
	}
	b := e.rev.Seal.Rfq
	allocations := make([]rules.Allocation, len(e.in.Data.Allocations))
	conditions := map[string]condition{}
	var built []rules.Condition
	seen := map[LineRef]bool{}
	for i, a := range e.in.Data.Allocations {
		p := index("/args/data/allocations", i)
		if !lines[a.RfqLineID] {
			return nil, nil, fail(kernel.CodeReferenceInvalid, p+"/rfq_line_id", "No demand line of the costing context has this ID")
		}
		src := a.Source.Supply()
		if src.Kind != KindSupplierOffer && src.Kind != KindPurchaseTerms {
			return nil, nil, fail(kernel.CodeReferenceInvalid, p+"/source/kind", "Goods are bought under a supplier offer or purchase terms")
		}
		s, err := e.supply(src, p+"/source")
		if err != nil {
			return nil, nil, err
		}
		if o := s.Offer; o != nil && (o.RfqID != b.RfqID || o.RfqRowVersion != b.RowVersion) {
			return nil, nil, fail(kernel.CodeReferenceInvalid, p+"/source", "The supplier offer answers another RFQ or RFQ version than the costing context")
		}
		u, err := e.unit(a.PurchaseUnit, p+"/purchase_unit_ref")
		if err != nil {
			return nil, nil, err
		}
		q, err := kernel.ParseDecimal(a.PurchaseQuantity, kernel.QuantityFormat)
		if err != nil {
			return nil, nil, fail(kernel.CodeInvalidKnownValue, p+"/purchase_quantity", "")
		}
		quantity, err := kernel.NewQuantity(q, kernel.UnitCode(u.Code))
		if err != nil {
			return nil, nil, fail(kernel.CodeInvalidKnownValue, p+"/purchase_quantity", "")
		}
		allocations[i] = rules.Allocation{Path: p, LineID: a.RfqLineID, Source: rules.SourceKey{Kind: src.Kind, ID: src.ID, Revision: src.Revision},
			ConditionLineID: a.Source.ConditionLineID, Quantity: quantity}
		if seen[a.Source] {
			continue
		}
		seen[a.Source] = true
		for j, l := range s.lines() {
			if l.ID != a.Source.ConditionLineID {
				continue
			}
			var covered []string
			if s.Offer != nil {
				covered = slices.Clone(s.Offer.RfqLineIDs)
				if covered == nil {
					covered = []string{}
				}
			}
			c, err := e.condition(src, j, l, covered)
			if err != nil {
				return nil, nil, err
			}
			built = append(built, c)
			conditions[c.Path] = condition{ref: src, supply: s, line: l, path: c.Path}
			break
		}
	}
	groups, err := rules.Purchase(demand, allocations, built)
	if err != nil {
		return nil, nil, err
	}
	if err = e.budget.ClaimComponents(len(groups)); err != nil {
		return nil, nil, err
	}
	return groups, conditions, nil
}

// condition builds the rules' view of condition line j of source.
func (e *engine) condition(src Ref, j int, l ConditionLine, covered []string) (rules.Condition, error) {
	cp := fmt.Sprintf("/%s/%s/lines/%d", src.Kind, src.ID, j)
	unit := func(r *Ref, field string) (Unit, error) {
		x, err := need(r, cp+field)
		if err != nil {
			return Unit{}, err
		}
		return e.unit(x, cp+field)
	}
	purchase, err := unit(l.PurchaseUnit, "/purchase_unit_ref")
	if err != nil {
		return rules.Condition{}, err
	}
	price, err := unit(l.PriceUnit, "/price_unit_ref")
	if err != nil {
		return rules.Condition{}, err
	}
	tier, err := unit(l.TierUnit, "/tier_unit_ref")
	if err != nil {
		return rules.Condition{}, err
	}
	basis, err := parse(l.PriceBasisQuantity, kernel.QuantityFormat, cp+"/price_basis_quantity")
	if err != nil {
		return rules.Condition{}, err
	}
	tiers, err := schedule(l.Tiers, cp)
	if err != nil {
		return rules.Condition{}, err
	}
	if l.MOQ == nil {
		return rules.Condition{}, fail(CodeCostInputNotReady, cp+"/moq", "The condition states no MOQ")
	}
	moqUnit, err := unit(l.MOQ.Unit, "/moq/unit_ref")
	if err != nil {
		return rules.Condition{}, err
	}
	mq, err := parse(l.MOQ.Quantity, kernel.QuantityFormat, cp+"/moq/quantity")
	if err != nil {
		return rules.Condition{}, err
	}
	moq, err := kernel.NewQuantity(mq, kernel.UnitCode(moqUnit.Code))
	if err != nil {
		return rules.Condition{}, fail(kernel.CodeInvalidKnownValue, cp+"/moq/quantity", "")
	}
	multiple, err := parseOptional(l.OrderMultiple, kernel.QuantityFormat, cp+"/order_multiple")
	if err != nil {
		return rules.Condition{}, err
	}
	var conversions []rules.UnitConversion
	for k, r := range l.Conversions {
		p := index(cp+"/conversion_refs", k)
		c, err := e.cfg(r, polUnitConversion, p)
		if err != nil {
			return rules.Condition{}, err
		}
		factor, err := parse(c.Conversion.Factor, kernel.FactorFormat, p)
		if err != nil {
			return rules.Condition{}, err
		}
		precision := 0
		if u, ok := e.units[c.Conversion.To]; ok {
			precision = u.Precision
		}
		conversions = append(conversions, rules.UnitConversion{SKU: skuKey(c.Conversion.SkuID, c.Conversion.SpecRevision),
			From: kernel.UnitCode(c.Conversion.From), To: kernel.UnitCode(c.Conversion.To), Factor: factor, ToPrecision: precision})
	}
	return rules.Condition{Path: cp, Source: rules.SourceKey{Kind: src.Kind, ID: src.ID, Revision: src.Revision}, LineID: l.ID,
		SKU: skuKey(l.SkuID, l.SpecRevision), CoveredLines: covered, PurchaseUnit: kernel.UnitCode(purchase.Code),
		PurchasePrecision: purchase.Precision, PriceUnit: kernel.UnitCode(price.Code), TierUnit: kernel.UnitCode(tier.Code),
		Price: rules.Schedule{Path: cp, Mode: rules.PricingMode(l.PricingMode), Scope: rules.TierScope(l.PriceTierScope), BasisQuantity: basis,
			Tiers: tiers},
		MOQ:           rules.MOQ{Quantity: moq, Scope: rules.MOQScope(l.MOQ.Scope), GroupID: l.MOQ.GroupID, Members: slices.Clone(l.MOQ.Members)},
		OrderMultiple: multiple, Conversions: conversions, IncludedObligations: slices.Clone(l.IncludedObligation)}, nil
}

func schedule(ts []Tier, path string) ([]rules.Tier, error) {
	out := make([]rules.Tier, len(ts))
	for i, t := range ts {
		p := index(path+"/tiers", i)
		lower, err := parse(t.Lower, kernel.QuantityFormat, p+"/lower_inclusive")
		if err != nil {
			return nil, err
		}
		upper, err := parseOptional(t.Upper, kernel.QuantityFormat, p+"/upper_exclusive")
		if err != nil {
			return nil, err
		}
		price, err := parse(t.UnitPrice, kernel.PriceFormat, p+"/unit_price")
		if err != nil {
			return nil, err
		}
		out[i] = rules.Tier{Lower: lower, Upper: upper, Bounded: t.Upper != "", UnitPrice: price, ZeroPriceReason: t.ZeroPriceReason}
	}
	return out, nil
}

// rates reads the exchange rates the scenario names (Python _cost: fx_refs).
func (e *engine) rates() error {
	var rates []rules.FXRate
	for i, r := range e.in.Data.FX {
		p := index("/args/data/fx_refs", i)
		c, err := e.cfg(r, polFXRate, p)
		if err != nil {
			return err
		}
		x := c.FXRate
		rate, err := parse(x.Rate, kernel.FactorFormat, p)
		if err != nil {
			return err
		}
		observed, err := kernel.ParseTimestamp(x.ObservedAt)
		if err != nil {
			return fail(kernel.CodeInvalidKnownValue, p, "The observation time is not a timestamp")
		}
		validity, err := kernel.NewValidityInterval(c.ValidFrom, c.ValidUntil)
		if err != nil {
			return fail(CodeCostInputNotReady, p, "The exchange rate has no valid interval")
		}
		from, err := e.rateCurrency(x.From, p)
		if err != nil {
			return err
		}
		to, err := e.rateCurrency(x.To, p)
		if err != nil {
			return err
		}
		rates = append(rates, rules.FXRate{Path: p, Ref: r.String(), From: from, To: to, Rate: rate, Purpose: x.Purpose,
			ObservedAt: observed, Validity: validity})
	}
	e.fx = rules.FX{Path: "/args/data/fx_refs", Target: e.currency, Rates: rates, At: e.now,
		MaxAge: time.Duration(e.policy.MaxFXAgeSeconds) * time.Second}
	return nil
}

// rateCurrency returns the currency of a rate's code with the minor units of the currency
// configurations the evaluation reads; a code none of them defines only matters if it is used, and
// then it does not match the cost's currency.
func (e *engine) rateCurrency(code, path string) (kernel.Currency, error) {
	if c, ok := e.currencies[code]; ok {
		return c, nil
	}
	c, err := kernel.NewCurrency(code, 0)
	if err != nil {
		return kernel.Currency{}, fail(kernel.CodeReferenceInvalid, path, "The exchange rate names no currency")
	}
	return c, nil
}

// costPurchases costs each priced condition line (Python _cost: the purchase groups loop) and
// returns the purchase components of the charge graph, the lines' purchase amounts and the
// component amounts.
func (e *engine) costPurchases(groups []rules.PurchaseGroup, conditions map[string]condition, r *Result) ([]rules.PurchaseCharge, map[string]kernel.Money, []kernel.Money, error) {
	base := map[string]kernel.Money{}
	var purchases []rules.PurchaseCharge
	var amounts []kernel.Money
	salePay := ref(e.basis.SalesPaymentTerms).String()
	saleTrade := ref(e.basis.SalesTradeTerms).String()
	for i, g := range groups {
		c := conditions[g.Condition.Path]
		cp, l := c.path, c.line
		trade, err := need(l.TradeTerms, cp+"/trade_terms_ref")
		if err != nil {
			return nil, nil, nil, err
		}
		if _, err = e.cfg(trade, polTradeTerms, cp+"/trade_terms_ref"); err != nil {
			return nil, nil, nil, err
		}
		pay, err := need(l.PaymentTerms, cp+"/payment_terms_ref")
		if err != nil {
			return nil, nil, nil, err
		}
		stages, err := e.payment(pay, cp+"/payment_terms_ref")
		if err != nil {
			return nil, nil, nil, err
		}
		e.check(cp+"/trade_terms_ref", polTradeTerms, CheckNormalized,
			"Supplier contract and export sales contract retain their own parties; explicit price inclusions and exporter charges define the cost bridge",
			trade.String(), saleTrade)
		status := rules.CompareCashflow(e.salePay, stages)
		e.check(cp+"/payment_terms_ref", polPaymentTerms, status,
			"Separate receipt/payment schedules retained; no automatic financing adjustment to nominal direct cost", salePay, pay.String())
		profile, err := need(l.TaxProfile, cp+"/tax_profile_ref")
		if err != nil {
			return nil, nil, nil, err
		}
		if err = e.taxApplication(l.TaxApplication, profile, "sku", l.SkuID, c.supply.supplier(), cp); err != nil {
			return nil, nil, nil, err
		}
		e.check(cp+"/tax_application_ref", polTaxApplication, CheckMatched,
			"Reviewed purchase-side goods application matches counterparty, SKU and tax revision; no tax entitlement inferred",
			l.TaxApplication.String(), profile.String())
		if err = e.supplyDelivery(l.Delivery, cp+"/delivery"); err != nil {
			return nil, nil, nil, err
		}
		cur, err := need(l.Currency, cp+"/currency_ref")
		if err != nil {
			return nil, nil, nil, err
		}
		currency, err := e.currencyOf(cur, cp+"/currency_ref")
		if err != nil {
			return nil, nil, nil, err
		}
		tax, err := e.taxOf(profile, cp+"/tax_profile_ref")
		if err != nil {
			return nil, nil, nil, err
		}
		cost, err := rules.Cost(cp, g.Amount, currency, tax, e.fx)
		if err != nil {
			return nil, nil, nil, err
		}
		shares, err := g.Allocate(cost.Amount)
		if err != nil {
			return nil, nil, nil, err
		}
		id := "purchase." + strconv.Itoa(i+1)
		obligation := "goods." + strconv.Itoa(i+1)
		purchases = append(purchases, rules.PurchaseCharge{Path: cp, ComponentID: id, ObligationID: obligation,
			Includes: slices.Clone(l.IncludedObligation)})
		for _, s := range shares {
			prev, ok := base[s.LineID]
			if !ok {
				prev, _ = kernel.NewMoney(e.currency, 0)
			}
			base[s.LineID], _ = prev.Add(s.Amount)
		}
		r.Components = append(r.Components, component(id, "purchase", c.ref, l.ID, obligation, g.Quantity.Value().String(),
			string(g.Quantity.Unit()), currency, g.Amount, profile, cost, shares))
		r.Purchases = append(r.Purchases, PurchaseQuantity{ComponentID: id, Source: c.ref, LineID: l.ID, Quantity: g.Quantity.Value().String(),
			Unit: string(g.Quantity.Unit())})
		amounts = append(amounts, cost.Amount)
	}
	return purchases, base, amounts, nil
}

func component(id, kind string, src Ref, lineID, obligation, quantity, unit string, c kernel.Currency, raw kernel.Decimal, profile Ref,
	cost rules.ComponentCost, shares []rules.Share) Component {
	out := Component{ComponentID: id, Kind: kind, Source: src, SourceLineID: lineID, ObligationID: obligation, Quantity: quantity,
		Unit: unit, Currency: c.Code(), RawAmount: raw.String(), NetMinor: cost.Tax.Net.MinorString(), TaxMinor: cost.Tax.Tax.MinorString(),
		GrossMinor: cost.Tax.Gross.MinorString(), RecoveredMinor: cost.Tax.Recovered.MinorString(), CostMinor: cost.Tax.Cost.MinorString(),
		TaxProfile: profile, FXRate: cost.FXRate, AmountMinor: cost.Amount.MinorString(), Allocations: []LineAmount{}}
	for _, s := range shares {
		out.Allocations = append(out.Allocations, LineAmount{LineID: s.LineID, Basis: string(s.Basis), BaseInteger: s.Base.String(),
			AmountMinor: s.Amount.MinorString()})
	}
	return out
}

// taxApplication checks a POL tax application against the cost it applies to: purchase side, the
// subject kind (sku for goods, fee_category for a service charge), the subject, the counterparty
// (an application for every party, or this one) and the exact tax profile revision. One kind never
// stands in for the other (Python tax_application).
func (e *engine) taxApplication(app *Ref, profile Ref, subjectKind, subjectID, party, path string) error {
	p := path + "/tax_application_ref"
	r, err := need(app, p)
	if err != nil {
		return err
	}
	c, err := e.cfg(r, polTaxApplication, p)
	if err != nil {
		return err
	}
	a := c.TaxApplication
	switch {
	case a.Side != "purchase":
		return fail(kernel.CodeReferenceInvalid, p, "A sales-side tax application does not apply to a purchase cost")
	case a.SubjectKind != subjectKind:
		return fail(kernel.CodeReferenceInvalid, p, fmt.Sprintf("A %s tax application does not apply to this cost", a.SubjectKind))
	case a.SubjectID != subjectID:
		return fail(kernel.CodeReferenceInvalid, p, "The tax application is for another subject")
	case a.PartyID != "" && a.PartyID != party:
		return fail(kernel.CodeReferenceInvalid, p, "The tax application is for another counterparty")
	case a.TaxProfile != profile:
		return fail(kernel.CodeReferenceInvalid, p, "The tax application binds another tax profile revision")
	}
	return nil
}

// supplyDelivery resolves a supplier's delivery and compares it with the requirement (Python
// delivery(..., "supply") and the comparison that follows). A lead time counts from an order
// event that has not occurred while costing, so it stays unresolved.
func (e *engine) supplyDelivery(dv *Delivery, path string) error {
	if dv == nil {
		return fail(CodeCostInputNotReady, path, "The supply condition states no delivery")
	}
	rule := delivery.Rule{Path: path, Kind: dv.Kind, Days: dv.Days, DayBasis: dv.DayBasis, Milestone: dv.Milestone, Place: dv.Place,
		TimeZone: dv.TimeZone}
	var cal *delivery.Calendar
	pending := ""
	switch dv.Kind {
	case delivery.KindFixed:
		date, err := delivery.ParseDate(dv.Date)
		if err != nil {
			return fail(kernel.CodeSchemaInvalid, path+"/date", "")
		}
		rule.Date = date
	case delivery.KindLeadTime:
		if dv.Calendar != nil {
			var err error
			if cal, err = e.calendar(*dv.Calendar, path+"/calendar_ref"); err != nil {
				return err
			}
		}
		pending = fmt.Sprintf("The anchor event %s has not occurred while costing; no date was assumed", dv.AnchorEvent)
	}
	res, err := e.resolve(rule, nil, cal, RoleSupply, pending)
	if err != nil {
		return err
	}
	for _, d := range delivery.Compare(e.required, rule, e.reqRes, res) {
		e.differ(d.Path, d.Reason)
	}
	return nil
}

// fee is a logistics charge with the document it belongs to.
type fee struct {
	offer    Ref
	provider string
	doc      Fee
	path     string
	feeType  Ref
}

// costCharges adds the charges of the selected logistics offers (Python _cost: the logistics loop,
// the fee graph, each fee's costing, the required obligations and fee types) and returns their
// amounts.
func (e *engine) costCharges(purchases []rules.PurchaseCharge, demand []rules.DemandLine, base map[string]kernel.Money, r *Result) ([]kernel.Money, error) {
	type indexed struct {
		ref Ref
		at  int
	}
	offers := make([]indexed, len(e.in.Data.LogisticsOffers))
	for i, o := range e.in.Data.LogisticsOffers {
		offers[i] = indexed{o, i}
	}
	slices.SortStableFunc(offers, func(a, b indexed) int { return compareRef(a.ref, b.ref) })
	var fees []rules.Fee
	var metas []fee
	for _, o := range offers {
		p := index("/args/data/logistics_offer_refs", o.at)
		if o.ref.Kind != KindLogisticsOffer {
			return nil, fail(kernel.CodeReferenceInvalid, p+"/kind", "Charges come from logistics offers")
		}
		s, err := e.supply(o.ref, p)
		if err != nil {
			return nil, err
		}
		lg := s.Logistics
		docp := "/logistics_offer/" + o.ref.ID
		if lg.Destination != e.basis.NamedPlace.ID {
			return nil, fail(CodeComparisonBasisMismatch, docp+"/destination", "The logistics offer does not deliver to the comparison place")
		}
		order := make([]int, len(lg.Fees))
		for j := range order {
			order[j] = j
		}
		slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(lg.Fees[a].ID, lg.Fees[b].ID) })
		for _, j := range order {
			f := lg.Fees[j]
			fp := index(docp+"/fees", j)
			if err = e.budget.ClaimComponents(1); err != nil {
				return nil, err
			}
			ft, err := need(f.FeeType, fp+"/fee_type_ref")
			if err != nil {
				return nil, err
			}
			if _, err = e.cfg(ft, polFeeCategory, fp+"/fee_type_ref"); err != nil {
				return nil, err
			}
			rf, err := e.fee(o.ref, f, fp, "fee."+strconv.Itoa(len(fees)+1), ft)
			if err != nil {
				return nil, err
			}
			fees = append(fees, rf)
			metas = append(metas, fee{offer: o.ref, provider: lg.ProviderID, doc: f, path: fp, feeType: ft})
		}
	}
	plan, err := rules.PlanCharges(purchases, fees, nil, nil)
	if err != nil {
		return nil, err
	}
	if err = e.budget.Inclusions(plan.InclusionEdges); err != nil {
		return nil, err
	}
	for _, d := range plan.Differences {
		e.differ(d.Path, d.Reason)
	}
	for _, d := range plan.Decisions {
		r.Charges = append(r.Charges, ChargeObligation{ComponentID: d.ComponentID, ObligationID: d.ObligationID, Status: string(d.Status),
			IncludedIn: d.IncludedIn})
	}
	lines := make([]rules.BaseLine, len(demand))
	for i, l := range demand {
		purchase, ok := base[l.LineID]
		if !ok {
			purchase, _ = kernel.NewMoney(e.currency, 0)
		}
		lines[i] = rules.BaseLine{LineID: l.LineID, Quantity: l.Quantity.Value(), Purchase: purchase}
	}
	var amounts []kernel.Money
	for i := range fees {
		f, m := &fees[i], metas[i]
		cur, err := need(m.doc.Currency, m.path+"/currency_ref")
		if err != nil {
			return nil, err
		}
		currency, err := e.currencyOf(cur, m.path+"/currency_ref")
		if err != nil {
			return nil, err
		}
		profile, err := need(m.doc.TaxProfile, m.path+"/tax_profile_ref")
		if err != nil {
			return nil, err
		}
		tax, err := e.taxOf(profile, m.path+"/tax_profile_ref")
		if err != nil {
			return nil, err
		}
		if err = e.taxApplication(m.doc.TaxApplication, profile, "fee_category", m.feeType.ID, m.provider, m.path); err != nil {
			return nil, err
		}
		e.check(m.path+"/tax_application_ref", polTaxApplication, CheckMatched,
			"Reviewed purchase-side service application matches provider, fee type and tax revision", m.doc.TaxApplication.String(), profile.String())
		d, _ := plan.Decision(f.ComponentID)
		switch d.Status {
		case rules.CustomerBorne:
			r.ExcludedComponents = append(r.ExcludedComponents, Excluded{ComponentID: f.ComponentID, Source: m.offer, SourceLineID: f.ID,
				ObligationID: f.ObligationID, IncludedIn: d.IncludedIn, ReasonCode: CheckCustomerBorne,
				Reason: "Customer-borne obligation is excluded from exporter nominal direct cost; no zero-priced service was fabricated"})
			e.check(m.path, "obligation", CheckCustomerBorne, "Exporter cost excludes the explicitly customer-borne obligation", m.offer.String())
			continue
		case rules.Included:
			r.ExcludedComponents = append(r.ExcludedComponents, Excluded{ComponentID: f.ComponentID, Source: m.offer, SourceLineID: f.ID,
				ObligationID: f.ObligationID, IncludedIn: d.IncludedIn, ReasonCode: "included",
				Reason: "Included in referenced payable component; not counted again"})
			continue
		}
		var measured *rules.Measurement
		if f.Chargeable.Kind != rules.ChargeFixed && m.doc.Chargeable != nil && m.doc.Chargeable.Measurement != nil {
			if measured, err = e.measurement(*m.doc.Chargeable.Measurement, m.path+"/chargeable/measurement_ref", lines); err != nil {
				return nil, err
			}
		}
		q, err := f.Chargeable.Quantity(measured)
		if err != nil {
			return nil, err
		}
		raw, err := f.Billing.Charge(f.Chargeable, q)
		if err != nil {
			return nil, err
		}
		cost, err := rules.Cost(m.path, raw, currency, tax, e.fx)
		if err != nil {
			return nil, err
		}
		if a := m.doc.Allocation; a != nil && a.Measurement != nil && (f.Allocation.Basis == rules.BasisWeight || f.Allocation.Basis == rules.BasisVolume) {
			if f.Allocation.Measurement, err = e.measurement(*a.Measurement, m.path+"/allocation/measurement_ref", lines); err != nil {
				return nil, err
			}
		}
		shares, err := f.Allocation.Distribute(cost.Amount, lines)
		if err != nil {
			return nil, err
		}
		unit := ""
		if f.Chargeable.Kind != rules.ChargeFixed {
			unit = string(f.Billing.Unit)
		}
		r.Components = append(r.Components, component(f.ComponentID, "charge", m.offer, f.ID, f.ObligationID, q.String(), unit, currency,
			raw, profile, cost, shares))
		amounts = append(amounts, cost.Amount)
	}
	required := make([]rules.RequiredFee, len(e.basis.RequiredFeeTypes))
	for i, t := range e.basis.RequiredFeeTypes {
		required[i] = rules.RequiredFee{Path: index("/context/comparison_basis/required_fee_type_refs", i), FeeType: ref(t).String()}
	}
	if _, err = rules.PlanCharges(purchases, fees, nil, required); err != nil {
		return nil, err
	}
	return amounts, nil
}

// fee builds the rules' view of one logistics fee.
func (e *engine) fee(offer Ref, f Fee, fp, componentID string, feeType Ref) (rules.Fee, error) {
	out := rules.Fee{Path: fp, Offer: offer.String(), ID: f.ID, ComponentID: componentID, ObligationID: f.ObligationID,
		FeeType: feeType.String(), Party: rules.Party(f.Party), IncludedIn: f.IncludedIn}
	if f.Chargeable == nil || f.Billing == nil || f.Allocation == nil {
		return rules.Fee{}, fail(CodeCostInputNotReady, fp, "The charge states no tariff, chargeable basis or allocation")
	}
	ch := f.Chargeable
	increment, err := parseOptional(ch.BillingIncrement, kernel.QuantityFormat, fp+"/chargeable/billing_increment")
	if err != nil {
		return rules.Fee{}, err
	}
	divisor, err := parseOptional(ch.Divisor, kernel.FactorFormat, fp+"/chargeable/divisor_m3_per_kg")
	if err != nil {
		return rules.Fee{}, err
	}
	out.Chargeable = rules.ChargeableRule{Path: fp + "/chargeable", Kind: rules.ChargeableKind(ch.Kind), BillingIncrement: increment,
		Rounding: rules.Rounding(ch.RoundingMode), RoundingScope: rules.RoundingScope(ch.RoundingScope), MaxScope: rules.RoundingScope(ch.MaxScope),
		DivisorM3PerKG: divisor}
	bl := f.Billing
	bp := fp + "/billing"
	amount, err := parseOptional(bl.Amount, kernel.PriceFormat, bp+"/amount")
	if err != nil {
		return rules.Fee{}, err
	}
	rate, err := parseOptional(bl.Rate, kernel.PriceFormat, bp+"/rate")
	if err != nil {
		return rules.Fee{}, err
	}
	basis, err := parseOptional(bl.BasisQuantity, kernel.QuantityFormat, bp+"/basis_quantity")
	if err != nil {
		return rules.Fee{}, err
	}
	minimum, err := parseOptional(bl.Minimum, kernel.PriceFormat, bp+"/minimum_amount")
	if err != nil {
		return rules.Fee{}, err
	}
	tiers, err := schedule(bl.Tiers, bp)
	if err != nil {
		return rules.Fee{}, err
	}
	unit := kernel.UnitCode("")
	if bl.Unit != nil {
		u, err := e.unit(*bl.Unit, bp+"/unit_ref")
		if err != nil {
			return rules.Fee{}, err
		}
		unit = kernel.UnitCode(u.Code)
	}
	out.Billing = rules.Billing{Path: bp, Kind: rules.BillingKind(bl.Kind), Amount: amount, Rate: rate, BasisQuantity: basis, Minimum: minimum,
		Unit: unit, Tiers: rules.Schedule{Mode: rules.PricingMode(bl.PricingMode), Tiers: tiers}, ZeroPriceReason: f.ZeroPriceReason}
	a := f.Allocation
	weights := make([]rules.ExplicitWeight, len(a.Weights))
	for i, w := range a.Weights {
		n, err := strconv.ParseInt(w.Weight, 10, 64)
		if err != nil {
			return rules.Fee{}, fail(rules.CodeAllocationInvalid, index(fp+"/allocation/weights", i)+"/weight", "An explicit weight is an integer")
		}
		weights[i] = rules.ExplicitWeight{LineID: w.LineID, Weight: n}
	}
	out.Allocation = rules.AllocationRule{Path: fp + "/allocation", Basis: rules.Basis(a.Basis), Targets: slices.Clone(a.Targets), Weights: weights}
	return out, nil
}

// measurement admits a measurement revision a charge is measured or allocated on: known, active,
// observed by now, a measurement the pricing policy allows (no verified estimates: POL's policy has
// no allow_verified_estimates), and holding only lines the scenario buys.
func (e *engine) measurement(k MeasurementKey, path string, lines []rules.BaseLine) (*rules.Measurement, error) {
	if err := e.budget.Tick(); err != nil {
		return nil, err
	}
	m, ok := e.in.Measurements[k]
	if !ok || m.Revision == nil {
		return nil, fail(kernel.CodeReferenceInvalid, path, "No measurement revision of this ID is visible")
	}
	if m.Revision.Status != "active" {
		return nil, fail(CodeReferenceStale, path, "The measurement revision is no longer the active one")
	}
	rm, err := m.Revision.Data.Rules(path)
	if err != nil {
		return nil, err
	}
	if err = rm.Check(e.now); err != nil {
		return nil, err
	}
	if err = rm.Usable(false); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, l := range lines {
		known[l.LineID] = true
	}
	for _, p := range rm.Packages {
		for i, id := range p.LineIDs {
			if !known[id] {
				return nil, fail(kernel.CodeReferenceInvalid, index(p.Path+"/line_ids", i), "The package holds a line the scenario does not purchase")
			}
		}
	}
	e.bind(FromCST, KindMeasurement, k.MeasurementID, k.Revision, m.Revision.Hash, UseInput)
	if err = e.budget.Observe(budget.Key{Kind: KindMeasurement, ID: k.MeasurementID, Version: k.Revision}, int64(len(canonical(m.Revision.Data))),
		nil, len(rm.Packages)); err != nil {
		return nil, err
	}
	return &rm, nil
}

// assumptions binds the context's assumptions to the documents the scenario selects (Python
// bind_assumptions): each names an input of the context or of a selected document and states its
// explicit value; any assumption keeps the result provisional.
func (e *engine) assumptions(r *Result) error {
	order := make([]int, len(e.ctx.Assumptions))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(e.ctx.Assumptions[a].ID, e.ctx.Assumptions[b].ID) })
	for _, i := range order {
		a := e.ctx.Assumptions[i]
		p := index("/context/assumptions", i)
		value, found := e.assumed(a.Path)
		if !found {
			return fail(kernel.CodeReferenceInvalid, p+"/path", "The assumption names no input of the costing context or the selected documents")
		}
		if value != a.ValueText {
			return fail(kernel.CodeInvalidKnownValue, p+"/value_text", "The assumption states the input's explicit value")
		}
		proposal, ok := e.rev.Proposals[a.ID]
		if !ok {
			return fail(CodeCostInputNotReady, p, "The assumption has no recorded proposer")
		}
		e.provisional = true
		e.differences = append(e.differences, Difference{Path: a.Path, Reason: "Unverified assumption: " + a.Reason})
		r.Assumptions = append(r.Assumptions, BoundAssumption{ID: a.ID, Path: a.Path, ValueText: a.ValueText, Reason: a.Reason,
			ProposedByKind: string(proposal.By.Kind), ProposedByID: proposal.By.ID, ProposedAt: kernel.FormatTimestamp(proposal.At),
			VerificationStatus: "unverified"})
	}
	return nil
}

// assumed returns the explicit value of the input an assumption path names.
func (e *engine) assumed(path string) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	number := func(s string, n int) (int, bool) {
		i, err := strconv.Atoi(s)
		return i, err == nil && i >= 0 && i < n && strconv.Itoa(i) == s
	}
	switch parts[0] {
	case "context":
		if len(parts) == 4 && parts[1] == "lines" && parts[3] == "quantity" {
			if i, ok := number(parts[2], len(e.ctx.Lines)); ok {
				return e.ctx.Lines[i].Quantity, true
			}
			return "", false
		}
		dv := e.basis.Delivery
		switch parts[len(parts)-1] {
		case "date":
			return dv.Date, dv.Kind == delivery.KindFixed
		case "days":
			return strconv.Itoa(dv.Days), dv.Kind == delivery.KindLeadTime
		}
		return "", false
	case KindSupplierOffer, KindPurchaseTerms:
		if len(parts) < 5 || parts[2] != "lines" {
			return "", false
		}
		for _, src := range e.in.Data.Supplies() {
			s, ok := e.in.Supply[src]
			if src.Kind != parts[0] || src.ID != parts[1] || !ok {
				continue
			}
			ls := s.lines()
			i, ok := number(parts[3], len(ls))
			if !ok {
				return "", false
			}
			l := ls[i]
			switch {
			case len(parts) == 7 && parts[4] == "tiers" && parts[6] == "unit_price":
				if j, ok := number(parts[5], len(l.Tiers)); ok {
					return l.Tiers[j].UnitPrice, true
				}
			case len(parts) == 6 && parts[4] == "delivery" && l.Delivery != nil:
				switch parts[5] {
				case "date":
					return l.Delivery.Date, l.Delivery.Kind == delivery.KindFixed
				case "days":
					return strconv.Itoa(l.Delivery.Days), l.Delivery.Kind == delivery.KindLeadTime
				}
			}
			return "", false
		}
	case KindLogisticsOffer:
		if len(parts) != 6 || parts[2] != "fees" || parts[4] != "billing" {
			return "", false
		}
		for _, o := range e.in.Data.LogisticsOffers {
			s, ok := e.in.Supply[o]
			if o.ID != parts[1] || !ok || s.Logistics == nil {
				continue
			}
			i, ok := number(parts[3], len(s.Logistics.Fees))
			if !ok || s.Logistics.Fees[i].Billing == nil {
				return "", false
			}
			b := s.Logistics.Fees[i].Billing
			switch parts[5] {
			case "amount":
				return b.Amount, b.Amount != ""
			case "rate":
				return b.Rate, b.Rate != ""
			case "minimum_amount":
				return b.Minimum, b.Minimum != ""
			}
			return "", false
		}
	}
	return "", false
}

func lineTotals(demand []rules.DemandLine, components []Component, c kernel.Currency) []LineTotal {
	sums := map[string]kernel.Money{}
	for _, comp := range components {
		for _, a := range comp.Allocations {
			m, err := kernel.ParseMinor(c, a.AmountMinor)
			if err != nil {
				continue
			}
			prev, ok := sums[a.LineID]
			if !ok {
				prev, _ = kernel.NewMoney(c, 0)
			}
			sums[a.LineID], _ = prev.Add(m)
		}
	}
	out := make([]LineTotal, len(demand))
	for i, l := range demand {
		m, ok := sums[l.LineID]
		if !ok {
			m, _ = kernel.NewMoney(c, 0)
		}
		out[i] = LineTotal{LineID: l.LineID, Quantity: l.Quantity.Value().String(), Unit: string(l.Quantity.Unit()), AmountMinor: m.MinorString()}
	}
	slices.SortFunc(out, func(a, b LineTotal) int { return cmp.Compare(a.LineID, b.LineID) })
	return out
}

// checkOutput is Python check_output: component IDs once, the components sum to the total, each
// component's allocations sum to it with each line once.
func checkOutput(r Result) error {
	ids := map[string]bool{}
	total, _ := kernel.ParseMinor(kernel.MustCurrency("XXX", 0), "0")
	for _, c := range r.Components {
		if ids[c.ComponentID] {
			return fail(kernel.CodeDuplicateLineID, "/result/components", "")
		}
		ids[c.ComponentID] = true
		amount, err := kernel.ParseMinor(kernel.MustCurrency("XXX", 0), c.AmountMinor)
		if err != nil {
			return fail(rules.CodeAllocationInvalid, "/result/components", "")
		}
		sum, _ := kernel.ParseMinor(kernel.MustCurrency("XXX", 0), "0")
		lines := map[string]bool{}
		for _, a := range c.Allocations {
			if lines[a.LineID] {
				return fail(kernel.CodeDuplicateLineID, "/result/components/allocations", "")
			}
			lines[a.LineID] = true
			m, err := kernel.ParseMinor(kernel.MustCurrency("XXX", 0), a.AmountMinor)
			if err != nil {
				return fail(rules.CodeAllocationInvalid, "/result/components/allocations", "")
			}
			sum, _ = sum.Add(m)
		}
		if !sum.Equal(amount) {
			return fail(rules.CodeAllocationInvalid, "/result/components/allocations", "A component's allocations do not sum to it")
		}
		total, _ = total.Add(amount)
	}
	if total.MinorString() != r.TotalMinor {
		return fail(rules.CodeAllocationInvalid, "/result/total_minor", "The components do not sum to the total")
	}
	return nil
}
