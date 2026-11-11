package orderrev

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Proposal is the complete commercial content an amendment proposes for the next order revision:
// every line it keeps, changes or adds (a line it leaves out is removed), every charge it keeps
// (with its amount, changed or not), and the terms. Decimals are strings in their contract formats
// (quantities kernel.QuantityFormat; prices, price basis quantities, discount ratios and charge
// amounts kernel.PriceFormat), never floats.
type Proposal struct {
	Lines   []ProposedLine   `json:"lines"`
	Charges []ProposedCharge `json:"charges"`
	Terms   Terms            `json:"terms"`
}

// ProposedLine is one line of a proposal. A line of the current revision is named by its line_id;
// another line_id adds a line. Conversion is the POL unit_conversion of the unit into the SKU's cost
// unit (none when the line counts in its cost unit); a line that keeps its SKU and unit keeps its
// conversion.
type ProposedLine struct {
	LineID             string       `json:"line_id"`
	Sku                quote.SkuRef `json:"sku_ref"`
	Quantity           string       `json:"quantity"`
	Unit               quote.Ref    `json:"unit_ref"`
	Conversion         *quote.Ref   `json:"conversion_ref,omitempty"`
	UnitPrice          string       `json:"unit_price"`
	PriceBasisQuantity string       `json:"price_basis_quantity"`
	DiscountRatio      string       `json:"discount_ratio"`
	TaxApplication     quote.Ref    `json:"tax_application_ref"`
}

// ProposedCharge is a charge of the current revision the proposal keeps, with its amount.
type ProposedCharge struct {
	ChargeID string `json:"charge_id"`
	Amount   string `json:"amount"`
}

// ProposalOf returns the proposal that restates revision r unchanged, the start of every amendment.
func ProposalOf(r *Revision) Proposal {
	p := Proposal{Lines: []ProposedLine{}, Charges: []ProposedCharge{}, Terms: r.Terms}
	for _, l := range r.Lines {
		p.Lines = append(p.Lines, ProposedLine{LineID: l.LineID, Sku: l.Sku, Quantity: l.Quantity, Unit: l.Unit, Conversion: l.Conversion,
			UnitPrice:          l.UnitPrice,
			PriceBasisQuantity: l.PriceBasisQuantity, DiscountRatio: l.DiscountRatio, TaxApplication: l.Tax.Application})
	}
	for _, c := range r.Charges {
		p.Charges = append(p.Charges, ProposedCharge{ChargeID: c.ChargeID, Amount: c.Amount})
	}
	return p
}

// Facts are what the upstream contexts say of the references a proposal names, resolved by the
// caller from the exact revisions it names before its transaction (and re-checked inside it with
// the guards): POL's order currency, units, tax applications and payment terms, and, for a SKU
// specification a line did not count in before, how CAT and POL count it in its cost unit. A fact
// Next needs and does not find is REFERENCE_INVALID at the reference: a missing fact is never taken
// as a neutral one (no tax, no conversion, no stage).
type Facts struct {
	// Currency is the order currency at its POL revision (Revision.Currency): code and minor units.
	Currency kernel.Currency
	// Units are POL units by code (a unit's config_id is its code): every unit a line of the next
	// revision counts in, its sales unit and its cost unit.
	Units map[string]calc.Unit
	// Measures say how a SKU specification counts in its cost unit when a line names a SKU
	// specification or unit its current line does not have (a changed SKU or an added line).
	Measures map[Measure]Measurement
	// Taxes are the POL sales tax applications, with their profiles, of every line whose SKU or tax
	// application is not its current line's.
	Taxes map[quote.Ref]*calc.TaxApplication
	// Stages are the stages of the proposal's payment terms when they are not the current
	// revision's.
	Stages []Stage
}

// Measure keys Facts.Measures: a SKU specification and the sales unit a line counts it in.
type Measure struct {
	Sku  quote.SkuRef
	Unit string
}

// Measurement is the cost unit of a SKU specification and the POL conversion from a sales unit
// into it; Conversion is nil when the sales unit is the cost unit.
type Measurement struct {
	CostUnit   string
	Conversion *calc.Conversion
}

// Stage is one stage of a POL payment terms revision as the payment plan prints it: its ID, its
// percentage of the payable (0–100, kernel PercentFormat), the trigger and days, and the day basis
// and calendar when the terms count working days.
type Stage struct {
	StageID  string
	Percent  string
	Trigger  string
	Days     int
	DayBasis string
	Calendar *quote.Ref
}

// Diff is what the next revision changes of the current one (AmendmentImpact's commercial part,
// OrderAmended's payload): the lines added, removed or changed and how, the charges, and the terms.
type Diff struct {
	Lines   []LineChange   `json:"lines"`
	Charges []ChargeChange `json:"charges"`
	Terms   []string       `json:"terms"`
	From    Totals         `json:"from"`
	To      Totals         `json:"to"`
}

// Changes of a line or charge.
const (
	Added   = "added"
	Removed = "removed"
	Changed = "changed"
)

// LineChange is one line the next revision adds, removes or changes: the fields that change
// (sku_ref, quantity, unit_price, price_basis_quantity, discount_ratio, tax_application_ref) and
// the quantity before and after ("" for an added line before and a removed one after).
type LineChange struct {
	LineID string   `json:"line_id"`
	Change string   `json:"change"`
	Fields []string `json:"fields"`
	From   string   `json:"from_quantity"`
	To     string   `json:"to_quantity"`
	Unit   string   `json:"unit"`
}

// ChargeChange is one charge the next revision removes or whose amount it changes.
type ChargeChange struct {
	ChargeID string `json:"charge_id"`
	Change   string `json:"change"`
	From     string `json:"from_amount"`
	To       string `json:"to_amount"`
}

// Totals are a revision's net, tax and payable in minor units.
type Totals struct {
	Net     string `json:"net_minor"`
	Tax     string `json:"tax_minor"`
	Payable string `json:"payable_minor"`
}

// Empty reports whether the diff changes nothing.
func (d *Diff) Empty() bool { return len(d.Lines) == 0 && len(d.Charges) == 0 && len(d.Terms) == 0 }

// Next derives the revision after base that proposal p, of approved amendment by, yields: the
// shipments FUL confirmed (f) are kept (QUANTITY_BELOW_SHIPPED, INVALID_STATE; see the package
// documentation for what may change), every amount is recalculated with the rules of the quote
// calculation from the facts, and the revision is numbered base.Number + 1, names base and by, and is
// hashed (its order hash). Refusals point into the proposal at path (such as
// "/args/proposal/lines/0/quantity"), into the facts at /facts and into the shipments at
// /fulfilment. It also returns what the revision changes; an empty Diff is not refused here
// (amendment.Prepare refuses it).
func Next(base *Revision, by AmendmentRef, p Proposal, f Fulfilment, facts Facts, path string) (*Revision, *Diff, error) {
	if facts.Currency.IsZero() || facts.Currency.Code() != base.Currency.ID {
		return nil, nil, kernel.Reject(kernel.CodeReferenceInvalid, "/facts/currency", "The order currency's minor units are needed")
	}
	if err := f.check(base); err != nil {
		return nil, nil, err
	}
	next := &Revision{Contract: RevisionContract, Scope: base.Scope, OrderID: base.OrderID, Number: base.Number + 1,
		Previous: &Link{Revision: base.Number, Hash: base.Hash}, Amendment: &by, Source: base.Source, Parties: base.Parties,
		Customer: base.Customer, Contact: base.Contact, Currency: base.Currency, Lines: []Line{}, Charges: []Charge{},
		Amounts: Amounts{Lines: []quote.AmountLine{}, Charges: []quote.AmountLine{}, Payments: []quote.Payment{}}}
	if err := checkTerms(p.Terms, path+"/terms"); err != nil {
		return nil, nil, err
	}
	next.Terms = p.Terms

	switch {
	case len(p.Lines) == 0:
		return nil, nil, kernel.Reject(kernel.CodeInvalidKnownValue, path+"/lines", "An order keeps at least one line; cancel it instead")
	case len(p.Lines) > calc.MaxLines:
		return nil, nil, fail(calc.CodeCalculationLimitExceeded, path+"/lines", fmt.Sprintf("An order has at most %d lines", calc.MaxLines))
	}
	seen := map[string]bool{}
	var lines []calc.Amounts
	for i, pl := range p.Lines {
		at := path + "/lines/" + strconv.Itoa(i)
		switch {
		case !kernel.ValidIdentifier(pl.LineID):
			return nil, nil, kernel.Reject(kernel.CodeSchemaInvalid, at+"/line_id", "")
		case seen[pl.LineID]:
			return nil, nil, kernel.Reject(kernel.CodeDuplicateLineID, at+"/line_id", "Line "+pl.LineID+" is proposed twice")
		}
		seen[pl.LineID] = true
		line, amounts, err := nextLine(base, pl, f, facts, at)
		if err != nil {
			return nil, nil, err
		}
		next.Lines = append(next.Lines, line)
		lines = append(lines, amounts)
	}
	for _, l := range base.Lines {
		if !seen[l.LineID] && f.Shipped[l.LineID].Sign() > 0 {
			return nil, nil, fail(CodeQuantityBelowShipped, path+"/lines", "Line "+l.LineID+" has confirmed shipments of "+
				f.Shipped[l.LineID].String()+" "+l.Unit.ID+"; it is never removed")
		}
	}

	var charges []calc.ChargeAmounts
	kept := map[string]bool{}
	for i, pc := range p.Charges {
		at := path + "/charges/" + strconv.Itoa(i)
		charge, amounts, err := nextCharge(base, pc, seen, kept, facts.Currency, at)
		if err != nil {
			return nil, nil, err
		}
		next.Charges = append(next.Charges, charge)
		charges = append(charges, amounts)
	}

	totals, err := calc.Total(facts.Currency, path, lines, charges)
	if err != nil {
		return nil, nil, err
	}
	stages, err := stagesOf(base, p.Terms, facts.Stages, path+"/terms/payment_terms_ref")
	if err != nil {
		return nil, nil, err
	}
	shares := make([]calc.Stage, len(stages))
	for i, s := range stages {
		pct, err := kernel.ParseDecimal(s.Percent, wide)
		if err != nil {
			return nil, nil, kernel.Reject(kernel.CodeReferenceInvalid, path+"/terms/payment_terms_ref", "A stage percentage is a decimal")
		}
		shares[i] = calc.Stage{StageID: s.StageID, Percent: pct}
	}
	plan, err := calc.AllocatePayments(path+"/terms", totals.Payable, shares)
	if err != nil {
		return nil, nil, err
	}
	for _, l := range lines {
		next.Amounts.Lines = append(next.Amounts.Lines, quote.AmountLine{ID: l.ID, NetMinor: l.Net.MinorString(), TaxMinor: l.Tax.MinorString(),
			PayableMinor: l.Payable.MinorString()})
	}
	for _, c := range charges {
		next.Amounts.Charges = append(next.Amounts.Charges, quote.AmountLine{ID: c.ID, ObligationKey: c.ObligationKey, NetMinor: c.Net.MinorString(),
			TaxMinor: c.Tax.MinorString(), PayableMinor: c.Payable.MinorString()})
	}
	next.Amounts.Net, next.Amounts.Tax, next.Amounts.Payable = totals.Net.MinorString(), totals.Tax.MinorString(), totals.Payable.MinorString()
	for i, pay := range plan {
		s := stages[i]
		next.Amounts.Payments = append(next.Amounts.Payments, quote.Payment{StageID: s.StageID, Percent: exact(pay.Percent), Trigger: s.Trigger,
			Days: s.Days, DayBasis: s.DayBasis, Calendar: s.Calendar, AmountMinor: pay.Amount.MinorString()})
	}
	sortRevision(next)
	doc, hash, err := encode(next)
	if err != nil {
		return nil, nil, err
	}
	next.Document, next.Hash = doc, hash
	return next, diff(base, next), nil
}

// wide parses decimals a stored revision holds (normalized quantities may have up to 18
// fractional digits).
var wide = kernel.DecimalFormat{IntDigits: 24, Scale: 24}

// nextLine derives line pl of the next revision from its current line (none for an added line).
func nextLine(base *Revision, pl ProposedLine, f Fulfilment, facts Facts, at string) (Line, calc.Amounts, error) {
	none := func(err error) (Line, calc.Amounts, error) { return Line{}, calc.Amounts{}, err }
	q, err := contractDecimal(pl.Quantity, kernel.QuantityFormat, at+"/quantity")
	if err != nil {
		return none(err)
	}
	price, err := contractDecimal(pl.UnitPrice, kernel.PriceFormat, at+"/unit_price")
	if err != nil {
		return none(err)
	}
	basis, err := contractDecimal(pl.PriceBasisQuantity, kernel.PriceFormat, at+"/price_basis_quantity")
	if err != nil {
		return none(err)
	}
	discount, err := contractDecimal(pl.DiscountRatio, kernel.PriceFormat, at+"/discount_ratio")
	if err != nil {
		return none(err)
	}
	switch {
	case !kernel.ValidIdentifier(pl.Sku.SkuID) || pl.Sku.SpecRevision <= 0:
		return none(kernel.Reject(kernel.CodeSchemaInvalid, at+"/sku_ref", ""))
	case !validRef(pl.Unit, quote.KindUnit):
		return none(kernel.Reject(kernel.CodeSchemaInvalid, at+"/unit_ref", ""))
	case !validRef(pl.TaxApplication, quote.KindTaxApplication):
		return none(kernel.Reject(kernel.CodeSchemaInvalid, at+"/tax_application_ref", ""))
	}
	cur := base.Line(pl.LineID)
	line := Line{LineID: pl.LineID, Sku: pl.Sku, Quantity: exact(q), Unit: pl.Unit, UnitPrice: exact(price), PriceBasisQuantity: exact(basis),
		DiscountRatio: exact(discount)}
	if cur != nil {
		line.RfqLineID = cur.RfqLineID
		if pl.Unit != cur.Unit {
			return none(kernel.Reject(kernel.CodeInvalidKnownValue, at+"/unit_ref", "A line keeps its unit; remove it and add another line"))
		}
		if shipped := f.Shipped[pl.LineID]; shipped.Sign() > 0 {
			if pl.Sku != cur.Sku || !same(price, cur.UnitPrice) || !same(basis, cur.PriceBasisQuantity) || !same(discount, cur.DiscountRatio) ||
				pl.TaxApplication != cur.Tax.Application {
				return none(kernel.Reject(kernel.CodeInvalidState, at, "Line "+pl.LineID+" has confirmed shipments; it keeps its SKU, price and tax. "+
					"Reduce it to the shipped quantity and add a line for the rest"))
			}
			if q.Cmp(shipped) < 0 {
				return none(fail(CodeQuantityBelowShipped, at+"/quantity", "Line "+pl.LineID+" has confirmed shipments of "+shipped.String()+" "+
					pl.Unit.ID))
			}
		}
	}

	// The quantity in its unit and in the SKU's cost unit. A line that keeps its SKU and quantity
	// keeps them as they are; any other needs the units' precision and the conversion.
	if cur != nil && cur.Sku == pl.Sku && !sameRef(pl.Conversion, cur.Conversion) {
		return none(kernel.Reject(kernel.CodeInvalidKnownValue, at+"/conversion_ref", "A line that keeps its SKU and unit keeps its conversion"))
	}
	if cur != nil && cur.Sku == pl.Sku && same(q, cur.Quantity) {
		line.CostUnit, line.Conversion, line.NormalizedQuantity = cur.CostUnit, cur.Conversion, cur.NormalizedQuantity
	} else if err := measure(&line, cur, pl, q, facts, at); err != nil {
		return none(err)
	}
	return priced(base, line, cur, pl, q, price, basis, discount, facts, at)
}

// measure sets the line's cost unit, conversion and quantity in the cost unit: the current line's
// conversion while the SKU stays, the facts' measures otherwise; both units' precision from the
// facts.
func measure(line *Line, cur *Line, pl ProposedLine, q kernel.Decimal, facts Facts, at string) error {
	unit, ok := facts.Units[pl.Unit.ID]
	if !ok || unit.Code != pl.Unit.ID {
		return kernel.Reject(kernel.CodeReferenceInvalid, at+"/unit_ref", "The unit's precision is needed")
	}
	if q.Sign() > 0 && (unit.Precision < 0 || !fits(q, unit.Precision)) {
		return kernel.Reject(kernel.CodeInvalidKnownValue, at+"/quantity", "The quantity has more fractional digits than its unit allows")
	}
	var factor kernel.Decimal
	if cur != nil && cur.Sku == pl.Sku {
		curQ, err1 := kernel.ParseDecimal(cur.Quantity, wide)
		curN, err2 := kernel.ParseDecimal(cur.NormalizedQuantity, wide)
		if err1 != nil || err2 != nil || curQ.Sign() <= 0 {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/revision/lines", "The current line "+cur.LineID+" has no readable quantities")
		}
		factor, _ = curN.Quo(curQ)
		line.CostUnit, line.Conversion = cur.CostUnit, cur.Conversion
	} else {
		m, ok := facts.Measures[Measure{Sku: pl.Sku, Unit: pl.Unit.ID}]
		if !ok || m.CostUnit == "" {
			return kernel.Reject(kernel.CodeReferenceInvalid, at+"/sku_ref", "How the SKU specification counts in its cost unit is needed")
		}
		line.CostUnit = m.CostUnit
		switch x := m.Conversion; {
		case !sameRef(pl.Conversion, conversionRef(x)):
			return kernel.Reject(kernel.CodeReferenceInvalid, at+"/conversion_ref", "The measures are of another conversion")
		case x == nil && m.CostUnit != pl.Unit.ID:
			return fail(calc.CodeDependencyNotSupported, at+"/unit_ref",
				"A sales unit other than the cost unit needs a published conversion; a package is never guessed")
		case x == nil:
			factor = kernel.DecimalFromInt(1)
		case x.SkuID != pl.Sku.SkuID || x.SpecRevision != pl.Sku.SpecRevision || x.From != pl.Unit.ID || x.To != m.CostUnit:
			return kernel.Reject(kernel.CodeReferenceInvalid, at+"/unit_ref", "The conversion is for another SKU, specification or pair of units")
		case !fits(x.Factor, calc.FactorScale) || x.Factor.Sign() <= 0 || x.Factor.Cmp(kernel.DecimalFromInt(1_000_000_000_000)) >= 0:
			return kernel.Reject(kernel.CodeReferenceInvalid, at+"/unit_ref", "A conversion factor is positive with at most 12 fractional digits")
		default:
			factor = x.Factor
			r := quote.Ref{Kind: x.Ref.Kind, ID: x.Ref.ID, Revision: x.Ref.Revision}
			line.Conversion = &r
		}
	}
	normalized := q.Mul(factor)
	cost, ok := facts.Units[line.CostUnit]
	if !ok || cost.Code != line.CostUnit {
		return kernel.Reject(kernel.CodeReferenceInvalid, at+"/sku_ref", "The cost unit's precision is needed")
	}
	if cost.Precision < 0 || !fits(normalized, cost.Precision) {
		return kernel.Reject(kernel.CodeInvalidKnownValue, at+"/quantity", "The converted quantity has more fractional digits than the cost unit allows")
	}
	n, ok := normalized.Exact()
	if !ok {
		return kernel.Reject(kernel.CodeInvalidKnownValue, at+"/quantity", "The converted quantity has no exact decimal form")
	}
	line.NormalizedQuantity = n
	return nil
}

// priced binds the line's tax and prices it.
func priced(base *Revision, line Line, cur *Line, pl ProposedLine, q, unitPrice, basis, discount kernel.Decimal, facts Facts,
	at string) (Line, calc.Amounts, error) {
	none := func(err error) (Line, calc.Amounts, error) { return Line{}, calc.Amounts{}, err }
	// The tax: the current binding while the SKU and the application stay, else the fact.
	if cur != nil && cur.Sku == pl.Sku && cur.Tax.Application == pl.TaxApplication {
		line.Tax = cur.Tax
	} else {
		a, ok := facts.Taxes[pl.TaxApplication]
		if !ok || a == nil || a.Ref != quote.CalcRef(pl.TaxApplication) {
			return none(kernel.Reject(kernel.CodeReferenceInvalid, at+"/tax_application_ref", "The tax application's profile is needed"))
		}
		if err := a.Applies(calc.SubjectSku, pl.Sku.SkuID, base.Customer.PartyID, at+"/tax_application_ref"); err != nil {
			return none(err)
		}
		line.Tax = Tax{Application: pl.TaxApplication, Profile: quote.Ref{Kind: a.Profile.Ref.Kind, ID: a.Profile.Ref.ID, Revision: a.Profile.Ref.Revision},
			PriceBasis: string(a.Profile.PriceBasis), Rate: exact(a.Profile.Rate)}
	}

	b, err := calc.Line{Path: at, LineID: pl.LineID, Quantity: q, UnitPrice: unitPrice, PriceBasisQuantity: basis, Discount: discount}.Base()
	if err != nil {
		return none(err)
	}
	s, err := split(facts.Currency, b, line.Tax, at+"/tax_application_ref")
	if err != nil {
		if r, ok := kernel.AsRejection(err); ok && r.Code == calc.CodeCalculationLimitExceeded {
			return none(fail(calc.CodeCalculationLimitExceeded, at, "The line amount is 10^24 minor units or more"))
		}
		return none(err)
	}
	return line, calc.Amounts{ID: pl.LineID, Net: s.Net, Tax: s.Tax, Payable: s.Gross}, nil
}

// nextCharge derives charge pc of the next revision from its current charge.
func nextCharge(base *Revision, pc ProposedCharge, lines, kept map[string]bool, c kernel.Currency, at string) (Charge, calc.ChargeAmounts, error) {
	none := func(err error) (Charge, calc.ChargeAmounts, error) { return Charge{}, calc.ChargeAmounts{}, err }
	cur := base.Charge(pc.ChargeID)
	switch {
	case !kernel.ValidIdentifier(pc.ChargeID):
		return none(kernel.Reject(kernel.CodeSchemaInvalid, at+"/charge_id", ""))
	case kept[pc.ChargeID]:
		return none(kernel.Reject(kernel.CodeInvalidKnownValue, at+"/charge_id", "Charge "+pc.ChargeID+" is proposed twice"))
	case cur == nil:
		return none(fail(calc.CodeDependencyNotSupported, at+"/charge_id", "An amendment keeps, changes the amount of or removes the order's charges; "+
			"a new charge is not supported yet"))
	}
	kept[pc.ChargeID] = true
	amount, err := contractDecimal(pc.Amount, kernel.PriceFormat, at+"/amount")
	if err != nil {
		return none(err)
	}
	switch {
	case amount.Sign() <= 0:
		return none(kernel.Reject(kernel.CodeInvalidKnownValue, at+"/amount", "A sales charge is positive; remove it instead"))
	case amount.Cmp(kernel.DecimalFromInt(1_000_000_000_000)) >= 0:
		return none(fail(calc.CodeCalculationLimitExceeded, at+"/amount", "A charge amount has at most 12 integer digits"))
	case cur.Inclusion == calc.InclusionIncluded && !lines[cur.IncludedLineID]:
		return none(kernel.Reject(kernel.CodeReferenceInvalid, at, "The charge is included in line "+cur.IncludedLineID+", which the proposal removes"))
	}
	charge := *cur
	charge.Amount = exact(amount)
	out := calc.ChargeAmounts{Amounts: calc.Amounts{ID: cur.ChargeID}, ObligationKey: cur.ObligationKey, Included: cur.Inclusion == calc.InclusionIncluded}
	if out.Included {
		zero, _ := kernel.NewMoney(c, 0)
		out.Net, out.Tax, out.Payable = zero, zero, zero
		return charge, out, nil
	}
	s, err := split(c, amount, cur.Tax, at+"/amount")
	if err != nil {
		return none(err)
	}
	out.Net, out.Tax, out.Payable = s.Net, s.Tax, s.Gross
	return charge, out, nil
}

// split applies a bound tax to base, an exact amount of currency c, as calc.TaxProfile.Split does
// once the profile was checked (when it was bound): an inclusive amount is rounded as the payable,
// any other as the net (T03 §5.1).
func split(c kernel.Currency, base kernel.Decimal, t Tax, path string) (kernel.TaxSplit, error) {
	rate, err := kernel.ParseDecimal(t.Rate, kernel.FactorFormat)
	known := t.PriceBasis == string(calc.TaxInclusive) || t.PriceBasis == string(calc.TaxExclusive) || t.PriceBasis == string(calc.TaxNotApplicable)
	if err != nil || !known || rate.Cmp(kernel.DecimalFromInt(1)) > 0 {
		return kernel.TaxSplit{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "The bound tax has no readable rate and price basis")
	}
	by := kernel.TaxExclusive
	if t.PriceBasis == string(calc.TaxInclusive) {
		by = kernel.TaxInclusive
	}
	s, err := by(c, base, rate)
	switch {
	case errors.Is(err, kernel.ErrAmountOutOfRange):
		return kernel.TaxSplit{}, fail(calc.CodeCalculationLimitExceeded, path, "The amount is 10^24 minor units or more")
	case err != nil:
		return kernel.TaxSplit{}, kernel.Reject(kernel.CodeInvalidKnownValue, path, err.Error())
	}
	return s, nil
}

// stagesOf returns the stages of terms' payment terms: the current revision's while they stay, the
// facts' otherwise.
func stagesOf(base *Revision, terms Terms, facts []Stage, path string) ([]Stage, error) {
	if terms.PaymentTerms == base.Terms.PaymentTerms {
		out := make([]Stage, len(base.Amounts.Payments))
		for i, p := range base.Amounts.Payments {
			out[i] = Stage{StageID: p.StageID, Percent: p.Percent, Trigger: p.Trigger, Days: p.Days, DayBasis: p.DayBasis, Calendar: p.Calendar}
		}
		return out, nil
	}
	if len(facts) == 0 {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "The stages of the new payment terms are needed")
	}
	for _, s := range facts {
		if !kernel.ValidIdentifier(s.StageID) || strings.TrimSpace(s.Trigger) == "" || s.Days < 0 || s.Days > 3650 {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "A payment stage has an ID, a trigger and 0 to 3650 days")
		}
	}
	return slices.Clone(facts), nil
}

// checkTerms checks the shape of proposed terms: references of their kinds and a delivery of one
// of the two forms a quote binds (fixed date or lead time) at a place in a time zone.
func checkTerms(t Terms, path string) error {
	switch {
	case !validRef(t.TradeTerms, quote.KindTradeTerms):
		return kernel.Reject(kernel.CodeSchemaInvalid, path+"/trade_terms_ref", "")
	case !validRef(t.PaymentTerms, quote.KindPaymentTerms):
		return kernel.Reject(kernel.CodeSchemaInvalid, path+"/payment_terms_ref", "")
	case t.NamedPlace.Kind != "location" || !kernel.ValidIdentifier(t.NamedPlace.ID) || t.NamedPlace.Revision <= 0:
		return kernel.Reject(kernel.CodeSchemaInvalid, path+"/named_place_ref", "")
	}
	d, at := t.Delivery, path+"/delivery"
	invalid := func(why string) error { return kernel.Reject(kernel.CodeInvalidKnownValue, at, why) }
	switch d.Kind {
	case "fixed":
		if day, err := time.Parse(time.DateOnly, d.Date); err != nil || day.Format(time.DateOnly) != d.Date || d.Anchor != nil || d.Calendar != nil ||
			d.Days != 0 || d.DayBasis != "" {
			return invalid("A fixed delivery has a date and nothing of a lead time")
		}
	case "lead_time":
		if d.Date != "" || d.Anchor == nil || d.Calendar == nil || strings.TrimSpace(d.DayBasis) == "" || d.Days < 0 || d.Days > 3650 {
			return invalid("A lead time has an anchor, 0 to 3650 days, a day basis and a calendar")
		}
	default:
		return invalid("The delivery is a fixed date or a lead time")
	}
	if strings.TrimSpace(d.Milestone) == "" || strings.TrimSpace(d.TimeZone) == "" || d.Place.Kind != "location" || !kernel.ValidIdentifier(d.Place.ID) {
		return invalid("A delivery has a milestone, a place and a time zone")
	}
	if _, err := time.LoadLocation(d.TimeZone); err != nil || d.TimeZone == "Local" {
		return invalid("The delivery's time zone is an IANA time zone")
	}
	return nil
}

// diff compares two revisions of one order.
func diff(from, to *Revision) *Diff {
	d := &Diff{Lines: []LineChange{}, Charges: []ChargeChange{}, Terms: []string{},
		From: Totals{Net: from.Amounts.Net, Tax: from.Amounts.Tax, Payable: from.Amounts.Payable},
		To:   Totals{Net: to.Amounts.Net, Tax: to.Amounts.Tax, Payable: to.Amounts.Payable}}
	ids := map[string]bool{}
	for _, l := range from.Lines {
		ids[l.LineID] = true
	}
	for _, l := range to.Lines {
		ids[l.LineID] = true
	}
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		a, b := from.Line(id), to.Line(id)
		switch {
		case a == nil:
			d.Lines = append(d.Lines, LineChange{LineID: id, Change: Added, Fields: []string{}, To: b.Quantity, Unit: b.Unit.ID})
		case b == nil:
			d.Lines = append(d.Lines, LineChange{LineID: id, Change: Removed, Fields: []string{}, From: a.Quantity, Unit: a.Unit.ID})
		default:
			var fields []string
			for _, c := range []struct {
				name string
				diff bool
			}{{"sku_ref", a.Sku != b.Sku}, {"quantity", !sameText(a.Quantity, b.Quantity)}, {"unit_price", !sameText(a.UnitPrice, b.UnitPrice)},
				{"price_basis_quantity", !sameText(a.PriceBasisQuantity, b.PriceBasisQuantity)},
				{"discount_ratio", !sameText(a.DiscountRatio, b.DiscountRatio)}, {"tax_application_ref", a.Tax.Application != b.Tax.Application}} {
				if c.diff {
					fields = append(fields, c.name)
				}
			}
			if len(fields) > 0 {
				d.Lines = append(d.Lines, LineChange{LineID: id, Change: Changed, Fields: fields, From: a.Quantity, To: b.Quantity, Unit: b.Unit.ID})
			}
		}
	}
	for _, c := range from.Charges {
		switch n := to.Charge(c.ChargeID); {
		case n == nil:
			d.Charges = append(d.Charges, ChargeChange{ChargeID: c.ChargeID, Change: Removed, From: c.Amount})
		case !sameText(c.Amount, n.Amount):
			d.Charges = append(d.Charges, ChargeChange{ChargeID: c.ChargeID, Change: Changed, From: c.Amount, To: n.Amount})
		}
	}
	if from.Terms.TradeTerms != to.Terms.TradeTerms {
		d.Terms = append(d.Terms, "trade_terms_ref")
	}
	if from.Terms.NamedPlace != to.Terms.NamedPlace {
		d.Terms = append(d.Terms, "named_place_ref")
	}
	if from.Terms.PaymentTerms != to.Terms.PaymentTerms {
		d.Terms = append(d.Terms, "payment_terms_ref")
	}
	if !sameDelivery(from.Terms.Delivery, to.Terms.Delivery) {
		d.Terms = append(d.Terms, "delivery")
	}
	return d
}

func sameDelivery(a, b quote.Delivery) bool {
	samePtr := func(x, y *quote.BusinessRef) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	sameRef := func(x, y *quote.Ref) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.Kind == b.Kind && a.Date == b.Date && samePtr(a.Anchor, b.Anchor) && a.Days == b.Days && a.DayBasis == b.DayBasis &&
		sameRef(a.Calendar, b.Calendar) && a.Milestone == b.Milestone && a.Place == b.Place && a.TimeZone == b.TimeZone
}

// sameText compares two stored decimals by value.
func sameText(a, b string) bool {
	x, err1 := kernel.ParseDecimal(a, wide)
	y, err2 := kernel.ParseDecimal(b, wide)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return x.Equal(y)
}

// same compares a parsed decimal with a stored one.
func same(d kernel.Decimal, stored string) bool {
	s, err := kernel.ParseDecimal(stored, wide)
	return err == nil && d.Equal(s)
}

// contractDecimal parses s in format f, SCHEMA_INVALID at path otherwise.
func contractDecimal(s string, f kernel.DecimalFormat, path string) (kernel.Decimal, error) {
	d, err := kernel.ParseDecimal(s, f)
	if err != nil {
		return kernel.Decimal{}, kernel.Reject(kernel.CodeSchemaInvalid, path, fmt.Sprintf("A decimal of at most %d integer and %d fractional digits",
			f.IntDigits, f.Scale))
	}
	return d, nil
}

// fits reports whether d has at most scale fractional digits.
func fits(d kernel.Decimal, scale int) bool { return d.RoundHalfUp(scale).Equal(d) }

// exact is the shortest exact decimal string of d, which has a terminating expansion (a contract
// decimal or a product of them).
func exact(d kernel.Decimal) string {
	s, ok := d.Exact()
	if !ok {
		panic("orderrev: a contract decimal has no exact decimal form: " + d.String())
	}
	return s
}

func sameRef(a, b *quote.Ref) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func conversionRef(x *calc.Conversion) *quote.Ref {
	if x == nil {
		return nil
	}
	return &quote.Ref{Kind: x.Ref.Kind, ID: x.Ref.ID, Revision: x.Ref.Revision}
}

func validRef(r quote.Ref, kind string) bool {
	return r.Kind == kind && kernel.ValidIdentifier(r.ID) && r.Revision > 0
}
