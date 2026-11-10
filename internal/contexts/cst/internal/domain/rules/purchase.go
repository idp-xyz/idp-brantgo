package rules

import (
	"cmp"
	"fmt"
	"slices"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// SourceKey identifies the supply condition revision an allocation draws on: a verified supplier
// offer (Kind "offer") or published purchase terms (Kind "purchase_terms") at Revision.
type SourceKey struct {
	Kind     string
	ID       string
	Revision int64
}

func (k SourceKey) String() string { return fmt.Sprintf("%s:%s@%d", k.Kind, k.ID, k.Revision) }

func compareSource(a, b SourceKey) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Revision, b.Revision))
}

// DemandLine is a line of the verified costing context: what the customer asked for, in its base
// unit. SKU is the identity of the SKU profile revision; the rules only compare it.
type DemandLine struct {
	Path     string
	LineID   string
	SKU      string
	Quantity kernel.Quantity
}

// Allocation is one line of a scenario's supply selection: Quantity of demand line LineID bought
// under condition line ConditionLineID of Source, in the condition's purchase unit.
type Allocation struct {
	Path            string
	LineID          string
	Source          SourceKey
	ConditionLineID string
	Quantity        kernel.Quantity
}

// MOQScope is what a minimum order quantity counts: the SKU of the source, or an explicit group of
// demand lines the supplier lets be ordered together (T02 §5, §13.1).
type MOQScope string

const (
	MOQScopeSKU           MOQScope = "sku"
	MOQScopeExplicitGroup MOQScope = "explicit_group"
)

// MOQ is a verified minimum order quantity. An explicit group names its GroupID and its member
// demand lines; only the quantities allocated to those members under the same source count.
type MOQ struct {
	Quantity kernel.Quantity
	Scope    MOQScope
	GroupID  string
	Members  []string
}

func (m MOQ) signature() string {
	members := slices.Clone(m.Members)
	slices.Sort(members)
	return fmt.Sprintf("%s|%s|%s|%s|%q", m.Quantity.Value(), m.Quantity.Unit(), m.Scope, m.GroupID, members)
}

// UnitConversion is a published, SKU-specific conversion the condition cites: one From is Factor To,
// and To has ToPrecision fractional digits. POL publishes only conversions within one dimension.
type UnitConversion struct {
	SKU         string
	From, To    kernel.UnitCode
	Factor      kernel.Decimal
	ToPrecision int
}

// Condition is one line of a supply condition revision as the rules need it. CoveredLines are the
// demand lines the revision answers (an offer's rfq_line_ids); nil means the revision is not bound
// to lines. The first supported path prices in the purchase unit: price, tier and MOQ units equal
// the purchase unit (T02 §13.1). IncludedObligations are the charge obligations the price includes.
type Condition struct {
	Path                string
	Source              SourceKey
	LineID              string
	SKU                 string
	CoveredLines        []string
	PurchaseUnit        kernel.UnitCode
	PurchasePrecision   int // fractional digits of the purchase unit
	PriceUnit           kernel.UnitCode
	TierUnit            kernel.UnitCode
	Price               Schedule
	MOQ                 MOQ
	OrderMultiple       kernel.Decimal // zero: none
	Conversions         []UnitConversion
	IncludedObligations []string
}

// Check applies the verify-time rules of one condition line (supplier_offer.verify,
// purchase_terms.publish): its price schedule, units, quantities exact at the purchase unit's
// precision, its MOQ rule, and included obligations named once. demandLines are the line IDs of
// the request the revision answers; an explicit MOQ group's members must be among them.
func (c Condition) Check(demandLines []string) error {
	if err := c.Price.Check(); err != nil {
		return err
	}
	for _, u := range []struct {
		unit kernel.UnitCode
		path string
	}{{c.PriceUnit, "/price_unit_ref"}, {c.TierUnit, "/tier_unit_ref"}} {
		if u.unit != c.PurchaseUnit {
			return fail(CodeUnsupportedPricingMode, c.Path+u.path, "Prices and tiers in another unit than the purchase unit are not supported")
		}
	}
	if c.MOQ.Quantity.Unit() != c.PurchaseUnit {
		return fail(kernel.CodeReferenceInvalid, c.Path+"/moq/unit_ref", "The MOQ is stated in the purchase unit")
	}
	if err := checkPrecision(c.Price.BasisQuantity, c.PurchasePrecision, c.Path+"/price_basis_quantity"); err != nil {
		return err
	}
	if err := checkQuantity(c.MOQ.Quantity.Value(), c.Path+"/moq/quantity"); err != nil {
		return err
	}
	if err := checkPrecision(c.MOQ.Quantity.Value(), c.PurchasePrecision, c.Path+"/moq/quantity"); err != nil {
		return err
	}
	if !c.OrderMultiple.IsZero() {
		if err := checkQuantity(c.OrderMultiple, c.Path+"/order_multiple"); err != nil {
			return err
		}
		if err := checkPrecision(c.OrderMultiple, c.PurchasePrecision, c.Path+"/order_multiple"); err != nil {
			return err
		}
	}
	switch c.MOQ.Scope {
	case MOQScopeSKU:
	case MOQScopeExplicitGroup:
		if c.MOQ.GroupID == "" || len(c.MOQ.Members) == 0 {
			return fail(kernel.CodeReferenceInvalid, c.Path+"/moq", "An explicit MOQ group names its group and members")
		}
		if err := unique(c.MOQ.Members, c.Path+"/moq/member_line_ids"); err != nil {
			return err
		}
		for i, m := range c.MOQ.Members {
			if !slices.Contains(demandLines, m) {
				return fail(kernel.CodeReferenceInvalid, index(c.Path, "moq/member_line_ids", i), "The member is not a line of the request")
			}
		}
	default:
		return fail(kernel.CodeSchemaInvalid, c.Path+"/moq/scope", "")
	}
	return unique(c.IncludedObligations, c.Path+"/included_obligation_ids")
}

// CheckConditions checks the condition lines of one supply condition revision: line IDs once, each
// line by Check, and one rule per explicit MOQ group (REFERENCE_INVALID when lines of the group
// state it differently).
func CheckConditions(lines []Condition, demandLines []string) error {
	ids := make([]string, len(lines))
	groups := map[string]string{}
	for i, c := range lines {
		ids[i] = c.LineID
		if err := c.Check(demandLines); err != nil {
			return err
		}
		if c.MOQ.Scope == MOQScopeExplicitGroup {
			sig := c.MOQ.signature()
			if prev, ok := groups[c.MOQ.GroupID]; ok && prev != sig {
				return fail(kernel.CodeReferenceInvalid, c.Path+"/moq", "Lines of one MOQ group state different rules")
			}
			groups[c.MOQ.GroupID] = sig
		}
	}
	if i := repeated(ids); i >= 0 {
		return fail(kernel.CodeDuplicateLineID, lines[i].Path+"/id", "")
	}
	return nil
}

// Member is one demand line's share of a purchase group, in the purchase unit.
type Member struct {
	LineID   string
	Quantity kernel.Decimal
	Paths    []string // the allocations that contribute it
}

// PurchaseGroup is one condition line as the scenario buys it: the purchase quantity of all its
// allocations aggregated, so the line is priced once (T02 §13.3), the demand lines it serves and
// the exact amount in the condition's currency before tax. Condition points into the slice passed
// to Purchase.
type PurchaseGroup struct {
	Condition *Condition
	Quantity  kernel.Quantity
	Members   []Member // ascending LineID
	Amount    kernel.Decimal
}

// Allocate splits the group's component amount over its demand lines in proportion to their
// purchase quantities (Allocate's largest remainder, line IDs ascending on ties).
func (g PurchaseGroup) Allocate(total kernel.Money) ([]Share, error) {
	values := make([]LineValue, len(g.Members))
	for i, m := range g.Members {
		values[i] = LineValue{LineID: m.LineID, Value: m.Quantity}
	}
	weights, err := IntegerWeights(g.Condition.Path, values)
	if err != nil {
		return nil, err
	}
	return Allocate(g.Condition.Path, total, BasisQuantity, weights)
}

type moqKey struct {
	source SourceKey
	scope  MOQScope
	id     string
}

type moqGroup struct {
	key      moqKey
	rule     MOQ
	path     string
	quantity kernel.Decimal
	paths    []string
}

// Purchase applies CST-INV-005 and the purchase rules of T02 §5, §7 and §13.1 to a scenario's
// allocations and returns one priced group per condition line, ordered by source and line:
//
//   - every allocation names a demand line, a condition line of the same SKU that answers it, a
//     positive quantity in the condition's purchase unit that is exact at the unit's precision
//     (a fractional piece is INVALID_KNOWN_VALUE) and at most 10^6;
//   - a quantity in another unit than the demand line's converts by exactly one conversion the
//     condition cites for that SKU from the purchase unit to the demand unit; a conversion is
//     never inferred from unit names nor inverted (REFERENCE_INVALID), and the result must be exact;
//   - MOQ counts the purchase quantity of its scope after aggregation: the source's SKU, or the
//     members of an explicit group under the same source, never other suppliers' or unrelated
//     lines; MOQ_NOT_MET is never resolved by buying more;
//   - the converted purchase quantity of each demand line equals its demand exactly
//     (COMPARISON_BASIS_MISMATCH): the base scenario neither under-buys nor over-buys;
//   - the aggregated quantity of a condition line is a whole multiple of its order multiple
//     (ORDER_MULTIPLE_INVALID).
func Purchase(demand []DemandLine, allocations []Allocation, conditions []Condition) ([]PurchaseGroup, error) {
	if len(demand) > MaxDemandLines {
		return nil, fail(CodeCalculationLimitExceeded, "/", "A costing context has at most 200 lines")
	}
	lines := map[string]*DemandLine{}
	for i := range demand {
		d := &demand[i]
		if _, dup := lines[d.LineID]; dup {
			return nil, fail(kernel.CodeDuplicateLineID, d.Path+"/line_id", "")
		}
		if err := checkQuantity(d.Quantity.Value(), d.Path+"/quantity"); err != nil {
			return nil, err
		}
		lines[d.LineID] = d
	}
	type conditionKey struct {
		source SourceKey
		line   string
	}
	conds := map[conditionKey]*Condition{}
	sources := map[SourceKey]bool{}
	for i := range conditions {
		c := &conditions[i]
		k := conditionKey{c.Source, c.LineID}
		if _, dup := conds[k]; dup {
			return nil, fail(kernel.CodeDuplicateLineID, c.Path+"/id", "")
		}
		conds[k] = c
	}

	type priceGroup struct {
		cond     *Condition
		quantity kernel.Decimal
		members  map[string]*Member
	}
	groups := map[conditionKey]*priceGroup{}
	moqs := map[moqKey]*moqGroup{}
	given := map[string]kernel.Decimal{}
	byLine := map[string][]string{}
	for _, a := range allocations {
		line, ok := lines[a.LineID]
		if !ok {
			return nil, fail(kernel.CodeReferenceInvalid, a.Path+"/rfq_line_id", "No demand line of the costing context has this ID")
		}
		ck := conditionKey{a.Source, a.ConditionLineID}
		c, ok := conds[ck]
		if !ok {
			return nil, fail(kernel.CodeReferenceInvalid, a.Path+"/source", "The source has no such condition line")
		}
		if c.CoveredLines != nil && !slices.Contains(c.CoveredLines, a.LineID) {
			return nil, fail(kernel.CodeReferenceInvalid, a.Path+"/rfq_line_id", "The source does not answer this demand line")
		}
		if c.SKU != line.SKU {
			return nil, fail(kernel.CodeReferenceInvalid, a.Path+"/source", "The condition line is for another SKU")
		}
		if a.Quantity.Unit() != c.PurchaseUnit {
			return nil, fail(CodeDependencyNotSupported, a.Path+"/purchase_unit_ref", "Only the condition's purchase unit is supported")
		}
		if c.PriceUnit != c.PurchaseUnit || c.TierUnit != c.PurchaseUnit {
			return nil, fail(CodeDependencyNotSupported, c.Path+"/price_unit_ref", "Prices and tiers in another unit than the purchase unit are not supported")
		}
		q := a.Quantity.Value()
		if err := checkQuantity(q, a.Path+"/purchase_quantity"); err != nil {
			return nil, err
		}
		if err := checkPrecision(q, c.PurchasePrecision, a.Path+"/purchase_quantity"); err != nil {
			return nil, err
		}
		base, err := toDemandUnit(q, c, line.Quantity.Unit(), a.Path)
		if err != nil {
			return nil, err
		}
		given[line.LineID] = given[line.LineID].Add(base)
		byLine[line.LineID] = append(byLine[line.LineID], a.Path+"/purchase_quantity")
		sources[a.Source] = true
		if len(sources) > MaxCandidateSources {
			return nil, fail(CodeCalculationLimitExceeded, a.Path+"/source", "A scenario draws on at most 200 supply condition revisions")
		}

		g := groups[ck]
		if g == nil {
			g = &priceGroup{cond: c, members: map[string]*Member{}}
			groups[ck] = g
		}
		g.quantity = g.quantity.Add(q)
		m := g.members[line.LineID]
		if m == nil {
			m = &Member{LineID: line.LineID}
			g.members[line.LineID] = m
		}
		m.Quantity = m.Quantity.Add(q)
		m.Paths = append(m.Paths, a.Path)

		if c.MOQ.Quantity.Unit() != c.PurchaseUnit {
			return nil, fail(kernel.CodeReferenceInvalid, c.Path+"/moq/unit_ref", "The MOQ is stated in the purchase unit")
		}
		mk := moqKey{source: a.Source, scope: c.MOQ.Scope, id: c.SKU}
		switch c.MOQ.Scope {
		case MOQScopeExplicitGroup:
			if !slices.Contains(c.MOQ.Members, line.LineID) {
				return nil, fail(kernel.CodeReferenceInvalid, c.Path+"/moq/member_line_ids", "The demand line is not a member of the condition's MOQ group", a.Path)
			}
			mk.id = c.MOQ.GroupID
		case MOQScopeSKU:
		default:
			return nil, fail(kernel.CodeSchemaInvalid, c.Path+"/moq/scope", "")
		}
		mg := moqs[mk]
		if mg == nil {
			mg = &moqGroup{key: mk, rule: c.MOQ, path: c.Path + "/moq/quantity"}
			moqs[mk] = mg
		} else if mg.rule.signature() != c.MOQ.signature() {
			return nil, fail(kernel.CodeReferenceInvalid, c.Path+"/moq", "Lines of one MOQ group state different rules")
		}
		mg.quantity = mg.quantity.Add(q)
		mg.paths = append(mg.paths, a.Path+"/purchase_quantity")
	}

	orderedMOQ := make([]*moqGroup, 0, len(moqs))
	for _, mg := range moqs {
		orderedMOQ = append(orderedMOQ, mg)
	}
	slices.SortFunc(orderedMOQ, func(a, b *moqGroup) int {
		return cmp.Or(compareSource(a.key.source, b.key.source), cmp.Compare(a.key.scope, b.key.scope), cmp.Compare(a.key.id, b.key.id))
	})
	for _, mg := range orderedMOQ {
		if mg.quantity.Cmp(mg.rule.Quantity.Value()) < 0 {
			return nil, fail(CodeMOQNotMet, mg.path,
				fmt.Sprintf("Purchase quantity %s is below the MOQ %s %s; quantities are never raised automatically", mg.quantity, mg.rule.Quantity.Value(), mg.rule.Quantity.Unit()),
				mg.paths...)
		}
	}
	for _, d := range demand {
		if got := given[d.LineID]; !got.Equal(d.Quantity.Value()) {
			return nil, fail(CodeComparisonBasisMismatch, d.Path+"/quantity",
				fmt.Sprintf("Purchase quantity %s does not equal the demand %s %s", got, d.Quantity.Value(), d.Quantity.Unit()),
				byLine[d.LineID]...)
		}
	}

	keys := make([]conditionKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b conditionKey) int {
		return cmp.Or(compareSource(a.source, b.source), cmp.Compare(a.line, b.line))
	})
	out := make([]PurchaseGroup, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		c := g.cond
		if !c.OrderMultiple.IsZero() {
			times, err := g.quantity.Quo(c.OrderMultiple)
			if err != nil || !times.IsInteger() {
				return nil, fail(CodeOrderMultipleInvalid, c.Path+"/order_multiple",
					fmt.Sprintf("Purchase quantity %s is not a multiple of %s; quantities are never raised automatically", g.quantity, c.OrderMultiple))
			}
		}
		amount, err := c.Price.Amount(g.quantity, c.Path+"/tiers")
		if err != nil {
			return nil, err
		}
		quantity, err := kernel.NewQuantity(g.quantity, c.PurchaseUnit)
		if err != nil {
			return nil, fail(kernel.CodeInvalidKnownValue, c.Path, "")
		}
		members := make([]Member, 0, len(g.members))
		for _, m := range g.members {
			members = append(members, *m)
		}
		slices.SortFunc(members, func(a, b Member) int { return cmp.Compare(a.LineID, b.LineID) })
		out = append(out, PurchaseGroup{Condition: c, Quantity: quantity, Members: members, Amount: amount})
	}
	return out, nil
}

// toDemandUnit converts purchase quantity q of condition c into the demand line's unit.
func toDemandUnit(q kernel.Decimal, c *Condition, to kernel.UnitCode, path string) (kernel.Decimal, error) {
	if c.PurchaseUnit == to {
		return q, nil
	}
	var found []UnitConversion
	for i, x := range c.Conversions {
		if x.SKU != c.SKU {
			return kernel.Decimal{}, fail(kernel.CodeReferenceInvalid, index(c.Path, "conversion_refs", i), "The conversion is for another SKU")
		}
		if x.From == c.PurchaseUnit && x.To == to {
			found = append(found, x)
		}
	}
	if len(found) != 1 {
		return kernel.Decimal{}, fail(kernel.CodeReferenceInvalid, c.Path+"/conversion_refs",
			fmt.Sprintf("The condition cites no single conversion from %s to %s; none is inferred from unit names or inverted", c.PurchaseUnit, to))
	}
	x := found[0]
	if x.Factor.Sign() <= 0 {
		return kernel.Decimal{}, fail(kernel.CodeInvalidKnownValue, c.Path+"/conversion_refs", "A conversion factor is positive")
	}
	v := q.Mul(x.Factor)
	if v.Cmp(maxQuantity) > 0 {
		return kernel.Decimal{}, fail(CodeCalculationLimitExceeded, path+"/purchase_quantity", "The converted quantity is above 1000000")
	}
	if err := checkPrecision(v, x.ToPrecision, path+"/purchase_quantity"); err != nil {
		return kernel.Decimal{}, err
	}
	return v, nil
}

// repeated returns the index of the first ID that repeats an earlier one, or -1.
func repeated(ids []string) int {
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if seen[id] {
			return i
		}
		seen[id] = true
	}
	return -1
}

// unique refuses a repeated ID in ids (DUPLICATE_LINE_ID at path/i).
func unique(ids []string, path string) error {
	if i := repeated(ids); i >= 0 {
		return fail(kernel.CodeDuplicateLineID, fmt.Sprintf("%s/%d", path, i), "")
	}
	return nil
}
