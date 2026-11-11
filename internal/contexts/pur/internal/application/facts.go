package application

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"time"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// facts reads what a preparation of req is judged on, each owner in a transaction of its own and at
// instant at, before PUR's transaction: TRD's order (through Orders), SUP's supply conditions with
// the source offers of purchase terms, PTY's suppliers, CAT's specification standings, POL's
// configurations and CST's cost snapshot. References an owner cannot hold are simply absent; the
// domain refuses them. order is TRD's answer, nil when TRD has none.
func (s Service) facts(ctx context.Context, req draft.Request, at time.Time) (draft.Inputs, *Order, error) {
	in := draft.Inputs{Supply: map[draft.Ref]draft.Supply{}, Parties: map[string]draft.Party{}, Specs: map[draft.Spec]func(string) error{},
		Configs: map[draft.ConfigKey]draft.Config{}}
	orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{req.OrderID})
	if err != nil {
		return in, nil, err
	}
	o, ok := orders[req.OrderID]
	if !ok {
		return in, nil, nil
	}
	if in.Order, err = orderOf(o); err != nil {
		return in, nil, err
	}

	var refs []supapi.SupplyConditionRef
	for _, l := range req.Lines {
		r := supapi.SupplyConditionRef{Kind: l.Source.Kind, ID: l.Source.ID, Revision: l.Source.Revision}
		if !slices.Contains(refs, r) {
			refs = append(refs, r)
		}
	}
	found, err := s.resolveSupply(ctx, refs, at)
	if err != nil {
		return in, nil, err
	}
	var origins []supapi.SupplyConditionRef
	for _, r := range found {
		if t := r.Terms; t != nil && t.SourceOfferRef != nil && !slices.Contains(refs, *t.SourceOfferRef) && !slices.Contains(origins, *t.SourceOfferRef) {
			origins = append(origins, *t.SourceOfferRef)
		}
	}
	more, err := s.resolveSupply(ctx, origins, at)
	if err != nil {
		return in, nil, err
	}
	var suppliers []string
	var configs []polapi.ConfigRef
	for _, l := range in.Order.Lines {
		configs = append(configs, polapi.ConfigRef{Kind: l.Unit.Kind, ConfigID: l.Unit.ID, Revision: l.Unit.Revision})
	}
	for _, ref := range refs {
		r, ok := found[ref]
		if !ok {
			continue
		}
		sup := supplyOf(r)
		if og := sup.Origin; og != nil {
			key := supapi.SupplyConditionRef{Kind: og.Ref.Kind, ID: og.Ref.ID, Revision: og.Ref.Revision}
			src, ok := found[key]
			if !ok {
				src, ok = more[key]
			}
			if ok {
				og.ValidFrom, og.ValidUntil = kernel.Instant(src.ValidFrom), kernel.Instant(src.ValidUntil)
			}
		}
		in.Supply[sup.Ref] = sup
		if !slices.Contains(suppliers, sup.SupplierID) && sup.SupplierID != "" {
			suppliers = append(suppliers, sup.SupplierID)
		}
		for _, c := range sup.Lines {
			for _, k := range []*draft.ConfigKey{c.PurchaseUnit, c.PriceUnit, c.TierUnit, c.Currency, c.TaxProfile, c.TaxApplication, c.TradeTerms,
				c.PaymentTerms, moqUnit(c.MOQ)} {
				if k != nil {
					configs = append(configs, polapi.ConfigRef{Kind: k.Kind, ConfigID: k.ID, Revision: k.Revision})
				}
			}
		}
	}
	if len(suppliers) > 0 {
		parties, err := s.Upstream.Parties.ResolveParties(ctx, ptyapi.Resolve{PartyIDs: suppliers})
		if err != nil {
			return in, nil, err
		}
		for id, p := range parties.Parties {
			in.Parties[id] = draft.Party{Status: p.Status, RowVersion: p.RowVersion, Supplier: slices.Contains(p.Roles, ptyapi.RoleSupplier)}
		}
	}
	var specs []catapi.SpecRef
	for _, l := range req.Lines {
		if ol, ok := in.Order.Lines[l.OrderLineID]; ok {
			ref := catapi.SpecRef{SkuID: ol.SkuID, SpecRevision: ol.SpecRevision}
			if !slices.Contains(specs, ref) && ref.Validate("/") == nil {
				specs = append(specs, ref)
			}
		}
	}
	if len(specs) > 0 {
		standings, err := s.Upstream.Skus.ResolveSpecs(ctx, specs)
		if err != nil {
			return in, nil, err
		}
		for ref, st := range standings {
			in.Specs[draft.Spec{SkuID: ref.SkuID, Revision: ref.SpecRevision}] = st.Check
		}
	}
	if in.Configs, err = s.resolveConfigs(ctx, configs, at); err != nil {
		return in, nil, err
	}
	if snap := in.Order.Snapshot; snap != nil {
		ref := cstapi.CostSnapshotRef{SnapshotID: snap.SnapshotID, ResultHash: snap.ResultHash}
		if ref.SnapshotID != "" {
			resolved, err := s.Upstream.Snapshots.ResolveSnapshots(ctx, []cstapi.CostSnapshotRef{ref}, at)
			if err != nil {
				return in, nil, err
			}
			if r, ok := resolved[ref]; ok {
				if in.Costing, err = costingOf(r); err != nil {
					return in, nil, err
				}
			}
		}
	}
	return in, &o, nil
}

func moqUnit(m *draft.MOQ) *draft.ConfigKey {
	if m == nil {
		return nil
	}
	return m.Unit
}

// resolveSupply asks SUP about refs at instant at, in batches of supapi.MaxLookup.
func (s Service) resolveSupply(ctx context.Context, refs []supapi.SupplyConditionRef, at time.Time) (map[supapi.SupplyConditionRef]supapi.ResolvedSupplyCondition, error) {
	out := map[supapi.SupplyConditionRef]supapi.ResolvedSupplyCondition{}
	var asked []supapi.SupplyConditionRef
	for _, r := range refs {
		if r.Validate("/") == nil {
			asked = append(asked, r)
		}
	}
	for chunk := range slices.Chunk(asked, supapi.MaxLookup) {
		answered, err := s.Upstream.Supply.ResolveSupplyConditions(ctx, chunk, at)
		if err != nil {
			return nil, err
		}
		for ref, r := range answered {
			out[ref] = r
		}
	}
	return out, nil
}

// resolveConfigs asks POL about refs at instant at, in batches of polapi.MaxRefs.
func (s Service) resolveConfigs(ctx context.Context, refs []polapi.ConfigRef, at time.Time) (map[draft.ConfigKey]draft.Config, error) {
	out := map[draft.ConfigKey]draft.Config{}
	var asked []polapi.ConfigRef
	for _, r := range refs {
		if r.Validate("/") == nil && !slices.Contains(asked, r) {
			asked = append(asked, r)
		}
	}
	for chunk := range slices.Chunk(asked, polapi.MaxRefs) {
		answered, err := s.Upstream.Configs.ResolveConfigs(ctx, chunk, at)
		if err != nil {
			return nil, err
		}
		for ref, r := range answered {
			out[draft.ConfigKey{Kind: ref.Kind, ID: ref.ConfigID, Revision: ref.Revision}] = configOf(r)
		}
	}
	return out, nil
}

// orderOf is TRD's order as the rules read it.
func orderOf(o Order) (*draft.Order, error) {
	out := &draft.Order{OrderID: o.Binding.OrderID, OrderRevision: o.Binding.OrderRevision, OrderHash: o.Binding.OrderHash, Status: o.Status,
		Code: o.Code, QuoteID: o.QuoteID, QuoteRevision: o.BusinessRevision, QuoteHash: o.BusinessHash, ReviewID: o.ReviewID,
		CustomerID: o.CustomerID, Lines: map[string]draft.OrderLine{}}
	if r := o.Rfq; r != nil {
		out.Rfq = &draft.RfqRef{RfqID: r.RfqID, RowVersion: r.RowVersion}
	}
	if s := o.CostSnapshot; s != nil {
		out.Snapshot = &draft.SnapshotRef{SnapshotID: s.SnapshotID, ResultHash: s.ResultHash}
	}
	if d := o.Delivery; d != nil {
		out.Delivery = &draft.RequiredDelivery{Kind: d.Kind, Date: d.Date, Milestone: d.Milestone, Place: d.PlaceID, TimeZone: d.TimeZone}
	}
	for _, l := range o.Lines {
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil || q.Sign() <= 0 {
			return nil, fmt.Errorf("pur: TRD's order %s line %s has quantity %q", o.Binding.OrderID, l.LineID, l.Quantity)
		}
		out.Lines[l.LineID] = draft.OrderLine{LineID: l.LineID, RfqLineID: l.RfqLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision,
			Quantity: q, Unit: draft.ConfigKey{Kind: polapi.KindUnit, ID: l.Unit.ID, Revision: l.Unit.Revision}}
	}
	return out, nil
}

func cfgKey(r *supapi.ConfigRef) *draft.ConfigKey {
	if r == nil {
		return nil
	}
	return &draft.ConfigKey{Kind: r.Kind, ID: r.ConfigID, Revision: r.Revision}
}

// supplyOf is SUP's supply condition revision as the rules read it.
func supplyOf(r supapi.ResolvedSupplyCondition) draft.Supply {
	out := draft.Supply{Ref: draft.Ref{Kind: r.Ref.Kind, ID: r.Ref.ID, Revision: r.Ref.Revision}, Check: r.Check, ContentHash: r.ContentHash,
		ValidFrom: kernel.Instant(r.ValidFrom), ValidUntil: kernel.Instant(r.ValidUntil), VerifiedBy: r.Verification.By,
		VerifiedAt: kernel.Instant(r.Verification.At), Lines: map[string]draft.Condition{}}
	var lines []supapi.ConditionLine
	var supplier *supapi.PartyRef
	switch {
	case r.Offer != nil:
		o := r.Offer
		supplier, lines = o.Supplier, o.Lines
		if o.Rfq != nil {
			out.Rfq = &draft.RfqRef{RfqID: o.Rfq.RfqID, RowVersion: o.Rfq.RowVersion}
		}
		out.RfqLineIDs = append([]string{}, o.RfqLineIDs...) // an offer answers exactly these lines
	case r.Terms != nil:
		t := r.Terms
		supplier, lines = t.Supplier, t.Lines
		if s := t.SourceOfferRef; s != nil {
			out.Origin = &draft.Origin{Ref: draft.Ref{Kind: s.Kind, ID: s.ID, Revision: s.Revision}, ValidityMode: t.ValidityMode}
		}
	}
	if supplier != nil {
		out.SupplierID, out.SupplierRowVersion = supplier.PartyID, supplier.RowVersion
	}
	for _, l := range lines {
		c := draft.Condition{ID: l.ID, SupplierItemCode: l.SupplierItemCode, PurchaseUnit: cfgKey(l.PurchaseUnitRef), PriceUnit: cfgKey(l.PriceUnitRef),
			TierUnit: cfgKey(l.TierUnitRef), PriceBasisQuantity: l.PriceBasisQuantity, PriceTierScope: l.PriceTierScope, PricingMode: l.PricingMode,
			OrderMultiple: l.OrderMultiple, Currency: cfgKey(l.CurrencyRef), TaxProfile: cfgKey(l.TaxProfileRef),
			TaxApplication: cfgKey(l.TaxApplicationRef), TradeTerms: cfgKey(l.TradeTermsRef), PaymentTerms: cfgKey(l.PaymentTermsRef)}
		if l.Sku != nil {
			c.SkuID, c.SpecRevision = l.Sku.SkuID, l.Sku.SpecRevision
		}
		for _, t := range l.Tiers {
			c.Tiers = append(c.Tiers, draft.Tier{Lower: t.LowerInclusive, Upper: t.UpperExclusive, UnitPrice: t.UnitPrice, ZeroPriceReason: t.ZeroPriceReason})
		}
		if m := l.MOQ; m != nil {
			c.MOQ = &draft.MOQ{Quantity: m.Quantity, Unit: cfgKey(m.UnitRef), Scope: m.Scope, GroupID: m.GroupID, Members: slices.Clone(m.MemberLineIDs)}
		}
		if d := l.Delivery; d != nil {
			c.Delivery = &draft.SupplyDelivery{Kind: d.Kind, Date: d.Date, Milestone: d.Milestone, Place: d.Place, TimeZone: d.TimeZone}
		}
		out.Lines[l.ID] = c
	}
	return out
}

// configOf is POL's configuration revision as the rules read it.
func configOf(r polapi.ResolvedConfig) draft.Config {
	out := draft.Config{Check: r.Check, ContentHash: r.ContentHash}
	d := r.Data
	switch {
	case d.Currency != nil:
		out.Currency = &draft.Currency{Code: d.Currency.Code, MinorUnits: d.Currency.MinorUnits}
	case d.Unit != nil:
		out.Unit = &draft.Unit{Code: d.Unit.Code, Precision: d.Unit.Precision}
	case d.TaxProfile != nil:
		out.TaxProfile = &draft.TaxProfile{Rate: d.TaxProfile.Rate, PriceBasis: d.TaxProfile.PriceBasis, Recovery: d.TaxProfile.RecoveryTreatment}
	}
	return out
}

// costingOf is CST's cost snapshot as the comparison reads it: per RFQ line, the condition lines
// its purchase components bought it under and the sum of their allocations to it.
func costingOf(r cstapi.ResolvedSnapshot) (*draft.Costing, error) {
	out := &draft.Costing{Ref: draft.SnapshotRef{SnapshotID: r.Ref.SnapshotID, ResultHash: r.Ref.ResultHash}, Currency: r.Currency,
		Lines: map[string]draft.CostedLine{}}
	sums := map[string]*big.Int{}
	for _, c := range r.Components {
		if c.Kind != "purchase" {
			continue
		}
		src := draft.Source{Ref: draft.Ref{Kind: c.Source.Kind, ID: c.Source.ID, Revision: c.Source.Revision}, ConditionLineID: c.SourceLineID}
		for _, a := range c.Allocations {
			amount, ok := new(big.Int).SetString(a.AmountMinor, 10)
			if !ok {
				return nil, fmt.Errorf("pur: CST's snapshot %s allocates %q", r.Ref.SnapshotID, a.AmountMinor)
			}
			l := out.Lines[a.LineID]
			if !slices.Contains(l.Sources, src) {
				l.Sources = append(l.Sources, src)
			}
			out.Lines[a.LineID] = l
			if sums[a.LineID] == nil {
				sums[a.LineID] = new(big.Int)
			}
			sums[a.LineID].Add(sums[a.LineID], amount)
		}
	}
	for id, sum := range sums {
		l := out.Lines[id]
		v := kernel.DecimalFromBigInt(sum)
		l.PurchaseMinor = &v
		out.Lines[id] = l
	}
	return out, nil
}
