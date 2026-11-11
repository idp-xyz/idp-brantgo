package calc

import (
	"cmp"
	"fmt"
	"slices"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The costing a quote may adopt (T03 §3, §5.2): an actual context on nominal direct cost.
const (
	ContextActual          = "actual"
	ContextWhatIf          = "what_if"
	CostBasisNominalDirect = "nominal_direct_cost"
)

// ScopeBinding is what BindScope established (Python brant.quote-cost-scope/0.1.0): the RFQ
// revision, the pricing policy and calculation currency, the cost basis, the comparison basis the
// quote shares with the snapshot and the customer's trading profile, and one binding per line in
// ascending line ID order.
type ScopeBinding struct {
	Rfq           RfqRef
	PricingPolicy Ref
	Currency      string // the calculation currency code
	CostBasis     string
	TradeTerms    Ref
	PaymentTerms  Ref
	NamedPlace    Ref
	Delivery      Delivery
	Lines         []LineBinding
}

// LineBinding binds one quote line to its RFQ line and the cost line of the same ID: the quantity in
// the sales unit, the conversion used (zero when the units are the same) and the normalized
// quantity in the cost unit, which equals the RFQ's and the costing's.
type LineBinding struct {
	LineID             string
	RfqLineID          string
	SkuID              string
	SpecRevision       int64
	Quantity           kernel.Decimal
	Unit               string
	Conversion         Ref
	NormalizedQuantity kernel.Decimal
	CostUnit           string
}

// Equal reports whether two bindings are the same, so that a decision after the freeze can compare
// the binding of the current inputs with the frozen one.
func (b ScopeBinding) Equal(o ScopeBinding) bool {
	head := b.Rfq == o.Rfq && b.PricingPolicy == o.PricingPolicy && b.Currency == o.Currency && b.CostBasis == o.CostBasis &&
		b.TradeTerms == o.TradeTerms && b.PaymentTerms == o.PaymentTerms && b.NamedPlace == o.NamedPlace && b.Delivery == o.Delivery
	return head && slices.EqualFunc(b.Lines, o.Lines, func(x, y LineBinding) bool {
		return x.LineID == y.LineID && x.RfqLineID == y.RfqLineID && x.SkuID == y.SkuID && x.SpecRevision == y.SpecRevision &&
			x.Quantity.Equal(y.Quantity) && x.Unit == y.Unit && x.Conversion == y.Conversion &&
			x.NormalizedQuantity.Equal(y.NormalizedQuantity) && x.CostUnit == y.CostUnit
	})
}

// BindScope applies TRD-INV-006 (Python t03_cost_scope.bind and the profile terms of
// t03_trade_parties.bind): what a quote may include relative to the cost snapshot it adopts.
//
//   - The quote names its RFQ, customer, contact, trade and payment terms, named place, delivery
//     and lines, and f names its pricing policy (DRAFT_NOT_READY otherwise).
//   - The snapshot froze an actual costing (a what_if one is COST_SNAPSHOT_NOT_ELIGIBLE) on
//     nominal direct cost of the quote's RFQ revision, for the same pricing policy, whose
//     calculation currency is the snapshot's; its context costs the RFQ's complete line set.
//   - The customer and the contact are the RFQ's (REFERENCE_INVALID).
//   - Trade terms, payment terms, named place and delivery are exactly the costing's comparison
//     basis, and the trade and payment terms the customer's trading profile terms: a difference is
//     re-costed first, never approved away.
//   - Each line quotes one RFQ line, each RFQ line is quoted once and all are quoted; the SKU and
//     specification revision are the costing's; the quantity fits its unit's precision; a unit
//     other than the cost unit converts by a published conversion of this SKU and specification
//     (DEPENDENCY_NOT_SUPPORTED without one, REFERENCE_INVALID for a conversion that does not fit
//     or a redundant one); the converted quantity equals a stated base quantity
//     (BASE_QUANTITY_MISMATCH) and the RFQ's and the costing's quantity exactly.
//
// Every other difference is COMPARISON_BASIS_MISMATCH.
func BindScope(q Quote, f Facts) (ScopeBinding, error) {
	p := q.Path
	c := f.Cost
	switch {
	case q.Rfq.ID == "":
		return ScopeBinding{}, fail(kernel.CodeDraftNotReady, p+"/rfq_ref", "The quote binds no RFQ")
	case q.Customer == "":
		return ScopeBinding{}, fail(kernel.CodeDraftNotReady, p+"/customer_ref", "")
	case q.Contact == "":
		return ScopeBinding{}, fail(kernel.CodeDraftNotReady, p+"/contact_ref", "")
	case f.Policy.Ref.IsZero():
		return ScopeBinding{}, fail(kernel.CodeDraftNotReady, p+"/pricing_policy_ref", "")
	case q.TradeTerms.IsZero():
		return ScopeBinding{}, fail(kernel.CodeDraftNotReady, p+"/trade_terms_ref", "")
	case q.PaymentTerms.IsZero():
		return ScopeBinding{}, fail(kernel.CodeDraftNotReady, p+"/payment_terms_ref", "")
	case q.NamedPlace.IsZero():
		return ScopeBinding{}, fail(kernel.CodeDraftNotReady, p+"/named_place_ref", "")
	case q.Delivery == Delivery{}:
		return ScopeBinding{}, fail(kernel.CodeDraftNotReady, p+"/delivery", "")
	case len(q.Lines) == 0:
		return ScopeBinding{}, fail(kernel.CodeDraftNotReady, p+"/lines", "A quote has at least one line")
	case len(q.Lines) > MaxLines:
		return ScopeBinding{}, fail(CodeCalculationLimitExceeded, p+"/lines", fmt.Sprintf("A quote has at most %d lines", MaxLines))
	}

	snapshot := p + "/cost_snapshot_ref"
	switch {
	case c.ContextKind != ContextActual:
		return ScopeBinding{}, fail(CodeCostSnapshotNotEligible, snapshot, "Only a snapshot of an actual costing prices a quote; a what_if costing never does")
	case c.CostBasis != CostBasisNominalDirect:
		return ScopeBinding{}, fail(CodeComparisonBasisMismatch, snapshot, "The snapshot is not on nominal direct cost")
	case q.Rfq != c.Rfq:
		return ScopeBinding{}, fail(CodeComparisonBasisMismatch, p+"/rfq_ref", "The cost snapshot costed another RFQ revision")
	case q.Customer != f.Rfq.Customer:
		return ScopeBinding{}, fail(kernel.CodeReferenceInvalid, p+"/customer_ref", "The customer is not the RFQ's")
	case q.Contact != f.Rfq.Contact:
		return ScopeBinding{}, fail(kernel.CodeReferenceInvalid, p+"/contact_ref", "The contact is not the RFQ's")
	case f.Policy.Ref != c.PricingPolicy:
		return ScopeBinding{}, fail(CodeComparisonBasisMismatch, p+"/pricing_policy_ref", "The cost snapshot was costed under another pricing policy revision")
	case f.Policy.CalculationCurrency.IsZero() || f.Policy.CalculationCurrency != c.Total.Currency():
		return ScopeBinding{}, fail(CodeComparisonBasisMismatch, p+"/pricing_policy_ref", "The policy's calculation currency is not the snapshot's")
	}
	for _, x := range []struct {
		field         string
		quote, costed any
	}{
		{"trade_terms_ref", q.TradeTerms, c.TradeTerms},
		{"payment_terms_ref", q.PaymentTerms, c.PaymentTerms},
		{"named_place_ref", q.NamedPlace, c.NamedPlace},
		{"delivery", q.Delivery, c.Delivery},
	} {
		if x.quote != x.costed {
			return ScopeBinding{}, fail(CodeComparisonBasisMismatch, p+"/"+x.field, "The cost snapshot was compared on another "+x.field+"; re-cost first")
		}
	}
	switch {
	case q.TradeTerms != f.Profile.TradeTerms:
		return ScopeBinding{}, fail(CodeComparisonBasisMismatch, p+"/trade_terms_ref", "The trade terms are not the customer's trading profile terms")
	case q.PaymentTerms != f.Profile.PaymentTerms:
		return ScopeBinding{}, fail(CodeComparisonBasisMismatch, p+"/payment_terms_ref", "The payment terms are not the customer's trading profile terms")
	}

	demand := make(map[string]RfqLine, len(f.Rfq.Lines))
	for _, l := range f.Rfq.Lines {
		demand[l.LineID] = l
	}
	costed := make(map[string]CostLine, len(c.Lines))
	for _, l := range c.Lines {
		costed[l.LineID] = l
	}
	if len(demand) != len(f.Rfq.Lines) || len(costed) != len(c.Lines) {
		return ScopeBinding{}, fail(kernel.CodeReferenceInvalid, snapshot, "The RFQ or the costing lists a line twice")
	}
	for id := range demand {
		if _, ok := costed[id]; !ok {
			return ScopeBinding{}, fail(CodeComparisonBasisMismatch, snapshot, "The snapshot does not cost the RFQ's complete line set")
		}
	}
	if len(costed) != len(demand) {
		return ScopeBinding{}, fail(CodeComparisonBasisMismatch, snapshot, "The snapshot costs lines the RFQ does not have")
	}

	seen := make(map[string]bool, len(q.Lines))
	used := make(map[string]bool, len(q.Lines))
	lines := make([]LineBinding, 0, len(q.Lines))
	for _, l := range q.Lines {
		switch {
		case l.LineID == "":
			return ScopeBinding{}, fail(kernel.CodeDraftNotReady, l.Path+"/line_id", "")
		case seen[l.LineID]:
			return ScopeBinding{}, fail(kernel.CodeDuplicateLineID, l.Path+"/line_id", "")
		case l.RfqLineID == "":
			return ScopeBinding{}, fail(kernel.CodeDraftNotReady, l.Path+"/rfq_line_id", "The line quotes no RFQ line")
		case used[l.RfqLineID]:
			return ScopeBinding{}, fail(CodeComparisonBasisMismatch, l.Path+"/rfq_line_id", "An RFQ line is quoted by one line; it is never split")
		}
		seen[l.LineID] = true
		used[l.RfqLineID] = true
		b, err := bindLine(l, demand, costed)
		if err != nil {
			return ScopeBinding{}, err
		}
		lines = append(lines, b)
	}
	if len(used) != len(demand) {
		return ScopeBinding{}, fail(CodeComparisonBasisMismatch, p+"/lines", "The quote covers the RFQ's complete line set; partial quotes are not supported")
	}
	slices.SortFunc(lines, func(a, b LineBinding) int { return cmp.Compare(a.LineID, b.LineID) })
	return ScopeBinding{
		Rfq: q.Rfq, PricingPolicy: f.Policy.Ref, Currency: c.Total.Currency().Code(), CostBasis: c.CostBasis,
		TradeTerms: q.TradeTerms, PaymentTerms: q.PaymentTerms, NamedPlace: q.NamedPlace, Delivery: q.Delivery, Lines: lines,
	}, nil
}

func bindLine(l Line, demand map[string]RfqLine, costed map[string]CostLine) (LineBinding, error) {
	asked, ok := demand[l.RfqLineID]
	if !ok {
		return LineBinding{}, fail(CodeComparisonBasisMismatch, l.Path+"/rfq_line_id", "The RFQ has no such line")
	}
	cost := costed[l.RfqLineID]
	switch {
	case l.SkuID == "":
		return LineBinding{}, fail(kernel.CodeDraftNotReady, l.Path+"/sku_id", "")
	case l.Unit.Code == "":
		return LineBinding{}, fail(kernel.CodeDraftNotReady, l.Path+"/unit_ref", "")
	case l.SkuID != cost.SkuID || l.SpecRevision != cost.SpecRevision:
		return LineBinding{}, fail(CodeComparisonBasisMismatch, l.Path+"/sku_id", "The SKU or its specification revision is not the one costed")
	case l.SkuID != asked.SkuID:
		return LineBinding{}, fail(CodeComparisonBasisMismatch, l.Path+"/sku_id", "The SKU is not the RFQ line's")
	case cost.Unit.Code != asked.UOM:
		return LineBinding{}, fail(CodeDependencyNotSupported, l.Path+"/unit_ref", "The costing's unit is not the RFQ line's unit")
	}
	if err := positive(l.Quantity, QuantityScale, l.Path+"/quantity", "A quantity"); err != nil {
		return LineBinding{}, err
	}
	if l.Unit.Precision < 0 || !fits(l.Quantity, l.Unit.Precision) {
		return LineBinding{}, fail(kernel.CodeInvalidKnownValue, l.Path+"/quantity", "The quantity has more fractional digits than its unit allows")
	}
	normalized := l.Quantity
	var conversion Ref
	if l.Unit.Code != cost.Unit.Code {
		x := l.Conversion
		switch {
		case x == nil:
			return LineBinding{}, fail(CodeDependencyNotSupported, l.Path+"/conversion_ref",
				"A sales unit other than the cost unit needs a published conversion; a package is never guessed")
		case x.SkuID != l.SkuID || x.SpecRevision != l.SpecRevision || x.From != l.Unit.Code || x.To != cost.Unit.Code:
			return LineBinding{}, fail(kernel.CodeReferenceInvalid, l.Path+"/conversion_ref", "The conversion is for another SKU, specification or pair of units")
		case !fits(x.Factor, FactorScale) || x.Factor.Sign() <= 0 || x.Factor.Cmp(integerLimit) >= 0:
			return LineBinding{}, fail(kernel.CodeReferenceInvalid, l.Path+"/conversion_ref", "A conversion factor is positive with at most 12 fractional digits")
		}
		normalized = l.Quantity.Mul(x.Factor)
		conversion = x.Ref
	} else if l.Conversion != nil {
		return LineBinding{}, fail(kernel.CodeReferenceInvalid, l.Path+"/conversion_ref", "The line is in the cost unit; a conversion is redundant")
	}
	switch {
	case l.BaseQuantity != nil && !l.BaseQuantity.Equal(normalized):
		return LineBinding{}, fail(CodeBaseQuantityMismatch, l.Path+"/base_quantity",
			fmt.Sprintf("The quantity converts to %s %s, not the stated %s", normalized, cost.Unit.Code, *l.BaseQuantity))
	case !normalized.Equal(asked.Quantity) || !normalized.Equal(cost.Quantity):
		return LineBinding{}, fail(CodeComparisonBasisMismatch, l.Path+"/quantity",
			fmt.Sprintf("The quantity converts to %s %s; the RFQ asks %s and the snapshot costs %s", normalized, cost.Unit.Code, asked.Quantity, cost.Quantity))
	case cost.Unit.Precision < 0 || !fits(normalized, cost.Unit.Precision):
		return LineBinding{}, fail(kernel.CodeInvalidKnownValue, l.Path+"/quantity", "The converted quantity has more fractional digits than the cost unit allows")
	}
	return LineBinding{
		LineID: l.LineID, RfqLineID: l.RfqLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision, Quantity: l.Quantity,
		Unit: l.Unit.Code, Conversion: conversion, NormalizedQuantity: normalized, CostUnit: cost.Unit.Code,
	}, nil
}
