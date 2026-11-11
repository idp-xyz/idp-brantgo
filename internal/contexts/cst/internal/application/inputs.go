package application

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/costing"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/delivery"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/scenario"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func index(path string, i int) string { return fmt.Sprintf("%s/%d", path, i) }

// inputs reads everything the evaluation of draft d of scenario id reads, each owner in a
// transaction of its own and at instant now (Context Map R08, R09, R10, R51): CST's costing context
// revision and the costing contexts delivery anchors name; INQ's RFQ; CAT's specification
// standings; SUP's supply conditions (the allocations' sources, the logistics offers, the source
// offers of purchase terms and the revisions anchors name); POL's configurations, with those they
// depend on; and CST's measurements the charges name.
func (s Service) inputs(ctx context.Context, id string, d scenario.Data, now time.Time) (scenario.Inputs, error) {
	now = kernel.Instant(now)
	in := scenario.Inputs{ScenarioID: id, Data: d, Now: now, Specs: map[costing.Spec]scenario.Standing{}, Configs: map[scenario.Ref]scenario.Config{},
		Supply: map[scenario.Ref]scenario.Supply{}, Measurements: map[scenario.MeasurementKey]scenario.Measured{},
		Contexts: map[scenario.ContextRef]scenario.AnchorContext{}}
	var anchor *costing.Ref
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, dec authorization.Decision) error {
		cc, err := postgres.LoadContext(ctx, exec, scope(ctx), d.Context.ContextID, postgres.NoLock)
		if err != nil || cc == nil || !dec.Sees(cc.OwnerActor, cc.OwnerTeam) {
			return err
		}
		if r := cc.Revision(d.Context.Revision); r != nil {
			in.Context = &scenario.Context{ID: cc.ID, Revision: r}
			if b := r.Data.Basis; b != nil && b.Delivery.Anchor != nil {
				anchor = b.Delivery.Anchor
			}
		}
		// An anchor of another kind, or one that does not parse, is judged by the domain.
		objectID, ok := contextAnchor(anchor)
		if !ok {
			return nil
		}
		ac, err := postgres.LoadContext(ctx, exec, scope(ctx), objectID, postgres.NoLock)
		if err != nil || ac == nil || !dec.Sees(ac.OwnerActor, ac.OwnerTeam) {
			return err
		}
		if r := ac.Revision(anchor.Revision); r != nil {
			a := scenario.AnchorContext{Sealed: r.Status.Sealed(), Status: string(r.Status), Hash: r.Hash}
			if r.Seal != nil {
				a.VerifiedAt = r.Seal.At
			}
			in.Contexts[scenario.ContextRef{ContextID: objectID, Revision: anchor.Revision}] = a
		}
		return nil
	})
	if err != nil || in.Context == nil {
		return in, err
	}
	rev := in.Context.Revision
	if seal := rev.Seal; seal != nil {
		rfqs, err := s.Upstream.Rfqs.ResolveRfqs(ctx, []string{seal.Rfq.RfqID})
		if err != nil {
			return in, err
		}
		if r, ok := rfqs[seal.Rfq.RfqID]; ok {
			in.Rfq = &scenario.Rfq{RfqID: r.RfqID, RowVersion: r.RowVersion, Ready: r.Ready(), LinesHash: r.LinesHash}
		}
	}
	var specs []catapi.SpecRef
	for _, l := range rev.Data.Lines {
		specs = append(specs, catapi.SpecRef{SkuID: l.SkuID, SpecRevision: l.SkuSpecRevision})
	}
	if len(specs) > 0 {
		standings, err := s.Upstream.Skus.ResolveSpecs(ctx, specs)
		if err != nil {
			return in, err
		}
		for ref, st := range standings {
			in.Specs[costing.Spec{SkuID: ref.SkuID, Revision: ref.SpecRevision}] = scenario.Standing{Check: st.Check}
		}
	}

	supply := slices.Clone(d.Supplies())
	supply = append(supply, d.LogisticsOffers...)
	if anchor != nil {
		if kind, objectID, err := delivery.ParseAnchor("/", anchor.ID); err == nil && supapi.KindKnown(kind) {
			supply = append(supply, scenario.Ref{Kind: kind, ID: objectID, Revision: anchor.Revision})
		}
	}
	if in.Supply, err = s.resolveSupply(ctx, supply, now); err != nil {
		return in, err
	}
	var origins []scenario.Ref
	for _, sc := range in.Supply {
		if t := sc.Terms; t != nil && t.SourceOffer != nil {
			if _, ok := in.Supply[*t.SourceOffer]; !ok {
				origins = append(origins, *t.SourceOffer)
			}
		}
	}
	more, err := s.resolveSupply(ctx, origins, now)
	if err != nil {
		return in, err
	}
	maps.Copy(in.Supply, more)

	configs := configRefsOf(rev.Data, d, in.Supply)
	if in.Configs, err = s.resolveConfigs(ctx, configs, now); err != nil {
		return in, err
	}
	var dependencies []scenario.Ref
	for _, c := range in.Configs {
		switch {
		case c.TaxApplication != nil:
			dependencies = append(dependencies, c.TaxApplication.TaxProfile)
		case c.PaymentTerms != nil && c.PaymentTerms.Calendar != nil:
			dependencies = append(dependencies, *c.PaymentTerms.Calendar)
		}
	}
	more2, err := s.resolveConfigs(ctx, dependencies, now)
	if err != nil {
		return in, err
	}
	for r, c := range more2 {
		if _, ok := in.Configs[r]; !ok {
			in.Configs[r] = c
		}
	}

	var measurements []api.MeasurementRef
	for _, o := range d.LogisticsOffers {
		sc, ok := in.Supply[o]
		if !ok || sc.Logistics == nil {
			continue
		}
		for _, f := range sc.Logistics.Fees {
			if c := f.Chargeable; c != nil && c.Measurement != nil {
				measurements = append(measurements, api.MeasurementRef{MeasurementID: c.Measurement.MeasurementID, Revision: c.Measurement.Revision})
			}
			if a := f.Allocation; a != nil && a.Measurement != nil {
				measurements = append(measurements, api.MeasurementRef{MeasurementID: a.Measurement.MeasurementID, Revision: a.Measurement.Revision})
			}
		}
	}
	if len(measurements) == 0 {
		return in, nil
	}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, dec authorization.Decision) error {
		keys := make([]postgres.MeasurementRevisionKey, len(measurements))
		for i, m := range measurements {
			keys[i] = postgres.MeasurementRevisionKey{MeasurementID: m.MeasurementID, Revision: m.Revision}
		}
		found, err := postgres.MeasurementRevisions(ctx, exec, scope(ctx), keys, postgres.NoLock)
		for k, o := range found {
			if dec.Sees(o.OwnerActor, o.OwnerTeam) {
				in.Measurements[scenario.MeasurementKey{MeasurementID: k.MeasurementID, Revision: k.Revision}] = scenario.Measured{Revision: o.Revision}
			}
		}
		return err
	})
	return in, err
}

// contextAnchor returns the costing context an anchor names, if it names one.
func contextAnchor(anchor *costing.Ref) (string, bool) {
	if anchor == nil {
		return "", false
	}
	kind, objectID, err := delivery.ParseAnchor("/", anchor.ID)
	return objectID, err == nil && kind == scenario.KindCostingContext
}

// configRefsOf returns every POL reference the evaluation of d on context data c may read: the
// context's, the allocations' units, the exchange rates, and those of the selected supply
// conditions' lines and fees.
func configRefsOf(c costing.Data, d scenario.Data, supply map[scenario.Ref]scenario.Supply) []scenario.Ref {
	var out []scenario.Ref
	for _, r := range c.ConfigRefs() {
		out = append(out, scenario.Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision})
	}
	for _, a := range d.Allocations {
		out = append(out, a.PurchaseUnit)
	}
	out = append(out, d.FX...)
	add := func(rs ...*scenario.Ref) {
		for _, r := range rs {
			if r != nil {
				out = append(out, *r)
			}
		}
	}
	for _, r := range append(d.Supplies(), d.LogisticsOffers...) {
		sc, ok := supply[r]
		if !ok {
			continue
		}
		for _, l := range lines(sc) {
			add(l.PurchaseUnit, l.PriceUnit, l.TierUnit, l.Currency, l.TaxProfile, l.TaxApplication, l.TradeTerms, l.PaymentTerms)
			out = append(out, l.Conversions...)
			if l.MOQ != nil {
				add(l.MOQ.Unit)
			}
			if l.Delivery != nil {
				add(l.Delivery.Calendar)
			}
		}
		if lg := sc.Logistics; lg != nil {
			for _, f := range lg.Fees {
				add(f.FeeType, f.Currency, f.TaxProfile, f.TaxApplication)
				if f.Billing != nil {
					add(f.Billing.Unit)
				}
			}
		}
	}
	return out
}

func lines(s scenario.Supply) []scenario.ConditionLine {
	switch {
	case s.Offer != nil:
		return s.Offer.Lines
	case s.Terms != nil:
		return s.Terms.Lines
	}
	return nil
}

// resolveSupply asks SUP about refs at instant at, in batches of supapi.MaxLookup; references SUP
// cannot hold are simply unknown.
func (s Service) resolveSupply(ctx context.Context, refs []scenario.Ref, at time.Time) (map[scenario.Ref]scenario.Supply, error) {
	out := map[scenario.Ref]scenario.Supply{}
	var asked []supapi.SupplyConditionRef
	seen := map[supapi.SupplyConditionRef]bool{}
	for _, r := range refs {
		q := supapi.SupplyConditionRef{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
		if q.Validate("/") == nil && !seen[q] {
			seen[q] = true
			asked = append(asked, q)
		}
	}
	for chunk := range slices.Chunk(asked, supapi.MaxLookup) {
		answered, err := s.Upstream.Supply.ResolveSupplyConditions(ctx, chunk, at)
		if err != nil {
			return nil, err
		}
		for ref, rc := range answered {
			out[scenario.Ref{Kind: ref.Kind, ID: ref.ID, Revision: ref.Revision}] = supplyInput(rc)
		}
	}
	return out, nil
}

// resolveConfigs asks POL about refs at instant at, in batches of polapi.MaxRefs; references POL
// cannot hold are simply unknown.
func (s Service) resolveConfigs(ctx context.Context, refs []scenario.Ref, at time.Time) (map[scenario.Ref]scenario.Config, error) {
	out := map[scenario.Ref]scenario.Config{}
	var asked []polapi.ConfigRef
	seen := map[polapi.ConfigRef]bool{}
	for _, r := range refs {
		q := polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ID, Revision: r.Revision}
		if q.Validate("/") == nil && !seen[q] {
			seen[q] = true
			asked = append(asked, q)
		}
	}
	for chunk := range slices.Chunk(asked, polapi.MaxRefs) {
		answered, err := s.Upstream.Configs.ResolveConfigs(ctx, chunk, at)
		if err != nil {
			return nil, err
		}
		for ref, rc := range answered {
			out[scenario.Ref{Kind: ref.Kind, ID: ref.ConfigID, Revision: ref.Revision}] = configInput(rc)
		}
	}
	return out, nil
}

// readableUpstream asks SUP, INQ, CAT and POL which of the upstream inputs of an evaluation the
// caller still reads: each lookup answers only what the caller's company and data scope hold.
func (s Service) readableUpstream(ctx context.Context, bound []scenario.BoundInput) (map[scenario.BoundInput]bool, error) {
	out := map[scenario.BoundInput]bool{}
	var supply, configs []scenario.Ref
	var rfqs, skus []string
	for _, b := range bound {
		switch b.Context {
		case scenario.FromSUP:
			supply = append(supply, scenario.Ref{Kind: b.Kind, ID: b.ID, Revision: b.Revision})
		case scenario.FromPOL:
			configs = append(configs, scenario.Ref{Kind: b.Kind, ID: b.ID, Revision: b.Revision})
		case scenario.FromINQ:
			rfqs = append(rfqs, b.ID)
		case scenario.FromCAT:
			skus = append(skus, b.ID)
		}
	}
	now := s.Clock.Now()
	foundSupply, err := s.resolveSupply(ctx, supply, now)
	if err != nil {
		return nil, err
	}
	foundConfigs, err := s.resolveConfigs(ctx, configs, now)
	if err != nil {
		return nil, err
	}
	foundRfqs := map[string]inqapi.RfqRef{}
	if len(rfqs) > 0 {
		if foundRfqs, err = s.Upstream.Rfqs.ResolveRfqs(ctx, slices.Compact(slices.Sorted(slices.Values(rfqs)))); err != nil {
			return nil, err
		}
	}
	foundSkus := map[string]catapi.SkuRef{}
	if len(skus) > 0 {
		if foundSkus, err = s.Upstream.Skus.ResolveSkus(ctx, slices.Compact(slices.Sorted(slices.Values(skus)))); err != nil {
			return nil, err
		}
	}
	for _, b := range bound {
		r := scenario.Ref{Kind: b.Kind, ID: b.ID, Revision: b.Revision}
		switch b.Context {
		case scenario.FromSUP:
			_, out[b] = foundSupply[r]
		case scenario.FromPOL:
			_, out[b] = foundConfigs[r]
		case scenario.FromINQ:
			_, out[b] = foundRfqs[b.ID]
		case scenario.FromCAT:
			_, out[b] = foundSkus[b.ID]
		}
	}
	return out, nil
}

// GuardInputs re-checks, inside CST's transaction and after the scenario root is locked FOR
// UPDATE, that the inputs an evaluation bound are still what it read (ADR-BR-004 option B,
// ADR-BR-011, lock order): CST's costing context and measurement revisions re-read FOR SHARE, then
// the upstream guards in context code order, each locking its row FOR SHARE until the transaction
// ends: RfqGuard v1 for the RFQ, ConfigGuard v1 for every POL input and SupplyConditionGuard v1 for
// every SUP input in force (anchors and provenance are history: their standing is their
// verification). A revision that is no longer in force is refused with its owner's code at
// /inputs/<context>/<kind>/<id>/<revision>; one whose content hash changed is REFERENCE_STALE there.
// The freeze calls it again at its own instant.
func GuardInputs(ctx context.Context, exec bentopg.Executor, d authorization.Decision, bound []scenario.BoundInput, at time.Time) error {
	path := func(b scenario.BoundInput) string {
		return fmt.Sprintf("/inputs/%s/%s/%s/%d", b.Context, b.Kind, b.ID, b.Revision)
	}
	stale := func(b scenario.BoundInput) error {
		return api.Reject(api.CodeReferenceStale, path(b))
	}
	var contexts []postgres.ContextRevisionKey
	var measurements []postgres.MeasurementRevisionKey
	for _, b := range bound {
		switch {
		case b.Context == scenario.FromCST && b.Kind == scenario.KindCostingContext && b.Use == scenario.UseInput:
			contexts = append(contexts, postgres.ContextRevisionKey{ContextID: b.ID, Revision: b.Revision})
		case b.Context == scenario.FromCST && b.Kind == scenario.KindMeasurement:
			measurements = append(measurements, postgres.MeasurementRevisionKey{MeasurementID: b.ID, Revision: b.Revision})
		}
	}
	foundContexts, err := postgres.ContextRevisions(ctx, exec, scope(ctx), contexts, postgres.ForShare)
	if err != nil {
		return err
	}
	foundMeasurements, err := postgres.MeasurementRevisions(ctx, exec, scope(ctx), measurements, postgres.ForShare)
	if err != nil {
		return err
	}
	for _, b := range bound {
		if b.Context != scenario.FromCST || b.Use != scenario.UseInput {
			continue
		}
		switch b.Kind {
		case scenario.KindCostingContext:
			o, ok := foundContexts[postgres.ContextRevisionKey{ContextID: b.ID, Revision: b.Revision}]
			if !ok || !d.Sees(o.OwnerActor, o.OwnerTeam) || o.Revision.Status != costing.Active || o.Revision.Hash != b.ContentHash {
				return stale(b)
			}
		case scenario.KindMeasurement:
			o, ok := foundMeasurements[postgres.MeasurementRevisionKey{MeasurementID: b.ID, Revision: b.Revision}]
			if !ok || !d.Sees(o.OwnerActor, o.OwnerTeam) || string(o.Revision.Status) != api.StatusActive || o.Revision.Hash != b.ContentHash {
				return stale(b)
			}
		}
	}
	for _, b := range bound {
		if b.Context != scenario.FromINQ {
			continue
		}
		binding := inqapi.RfqBinding{RfqID: b.ID, RowVersion: b.Revision, LinesHash: b.ContentHash}
		v, err := inqapi.ScanRfqGuard(binding, exec.QueryRow(ctx, inqapi.RfqGuardSQL, inqapi.RfqGuardArgs(binding)...).Scan)
		if err != nil {
			return err
		}
		if err = v.Check(path(b)); err != nil {
			return err
		}
	}
	for _, b := range bound {
		if b.Context != scenario.FromPOL {
			continue
		}
		ref := polapi.ConfigRef{Kind: b.Kind, ConfigID: b.ID, Revision: b.Revision}
		v, err := polapi.ScanConfigGuard(ref, exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(ref, at)...).Scan)
		if err != nil {
			return err
		}
		if err = v.Check(path(b)); err != nil {
			return err
		}
		if v.ContentHash != b.ContentHash {
			return stale(b)
		}
	}
	for _, b := range bound {
		if b.Context != scenario.FromSUP || b.Use != scenario.UseInput {
			continue
		}
		ref := supapi.SupplyConditionRef{Kind: b.Kind, ID: b.ID, Revision: b.Revision}
		v, err := supapi.ScanSupplyConditionGuard(ref, exec.QueryRow(ctx, supapi.SupplyConditionGuardSQL, supapi.SupplyConditionGuardArgs(ref, at)...).Scan)
		if err != nil {
			return err
		}
		if err = v.Check(path(b)); err != nil {
			return err
		}
		if v.ContentHash != b.ContentHash {
			return stale(b)
		}
	}
	return nil
}

func cfgRef(r *supapi.ConfigRef) *scenario.Ref {
	if r == nil {
		return nil
	}
	return &scenario.Ref{Kind: r.Kind, ID: r.ConfigID, Revision: r.Revision}
}

func cfgRefs(rs []supapi.ConfigRef) []scenario.Ref {
	var out []scenario.Ref
	for _, r := range rs {
		out = append(out, scenario.Ref{Kind: r.Kind, ID: r.ConfigID, Revision: r.Revision})
	}
	return out
}

func tiersOf(ts []supapi.Tier) []scenario.Tier {
	out := make([]scenario.Tier, len(ts))
	for i, t := range ts {
		out[i] = scenario.Tier{Lower: t.LowerInclusive, Upper: t.UpperExclusive, UnitPrice: t.UnitPrice, ZeroPriceReason: t.ZeroPriceReason}
	}
	return out
}

func conditionLines(ls []supapi.ConditionLine) []scenario.ConditionLine {
	out := make([]scenario.ConditionLine, len(ls))
	for i, l := range ls {
		c := scenario.ConditionLine{ID: l.ID, PurchaseUnit: cfgRef(l.PurchaseUnitRef), PriceUnit: cfgRef(l.PriceUnitRef),
			PriceBasisQuantity: l.PriceBasisQuantity, Conversions: cfgRefs(l.ConversionRefs), TierUnit: cfgRef(l.TierUnitRef),
			PriceTierScope: l.PriceTierScope, PricingMode: l.PricingMode, Tiers: tiersOf(l.Tiers), OrderMultiple: l.OrderMultiple,
			Currency: cfgRef(l.CurrencyRef), TaxProfile: cfgRef(l.TaxProfileRef), TaxApplication: cfgRef(l.TaxApplicationRef),
			TradeTerms: cfgRef(l.TradeTermsRef), PaymentTerms: cfgRef(l.PaymentTermsRef), IncludedObligation: slices.Clone(l.IncludedObligationIDs)}
		if l.Sku != nil {
			c.SkuID, c.SpecRevision = l.Sku.SkuID, l.Sku.SpecRevision
		}
		if m := l.MOQ; m != nil {
			c.MOQ = &scenario.MOQ{Quantity: m.Quantity, Unit: cfgRef(m.UnitRef), Scope: m.Scope, GroupID: m.GroupID, Members: slices.Clone(m.MemberLineIDs)}
		}
		if dv := l.Delivery; dv != nil {
			c.Delivery = &scenario.Delivery{Kind: dv.Kind, Date: dv.Date, AnchorEvent: dv.AnchorEvent, Days: dv.Days, DayBasis: dv.DayBasis,
				Calendar: cfgRef(dv.CalendarRef), Milestone: dv.Milestone, Place: dv.Place, TimeZone: dv.TimeZone}
		}
		out[i] = c
	}
	return out
}

func measurementKey(r *supapi.MeasurementRef) *scenario.MeasurementKey {
	if r == nil {
		return nil
	}
	return &scenario.MeasurementKey{MeasurementID: r.MeasurementID, Revision: r.Revision}
}

// supplyInput is what SUP said about one supply condition revision, as the evaluation reads it.
func supplyInput(r supapi.ResolvedSupplyCondition) scenario.Supply {
	out := scenario.Supply{Status: r.Status, Check: r.Check, ContentHash: r.ContentHash, VerifiedAt: kernel.Instant(r.Verification.At)}
	switch {
	case r.Offer != nil:
		o := r.Offer
		x := &scenario.Offer{RfqLineIDs: slices.Clone(o.RfqLineIDs), Lines: conditionLines(o.Lines)}
		if o.Supplier != nil {
			x.SupplierID = o.Supplier.PartyID
		}
		if o.Rfq != nil {
			x.RfqID, x.RfqRowVersion = o.Rfq.RfqID, o.Rfq.RowVersion
		}
		out.Offer = x
	case r.Terms != nil:
		t := r.Terms
		x := &scenario.Terms{ValidityMode: t.ValidityMode, Lines: conditionLines(t.Lines)}
		if t.Supplier != nil {
			x.SupplierID = t.Supplier.PartyID
		}
		if o := t.SourceOfferRef; o != nil {
			x.SourceOffer = &scenario.Ref{Kind: o.Kind, ID: o.ID, Revision: o.Revision}
		}
		out.Terms = x
	case r.Logistics != nil:
		g := r.Logistics
		x := &scenario.Logistics{ServiceCode: g.ServiceCode, Mode: g.Mode, Origin: g.Origin, Destination: g.Destination}
		if g.ServiceRoleRef != nil {
			x.ProviderID = g.ServiceRoleRef.PartyID
		}
		for _, f := range g.Fees {
			fee := scenario.Fee{ID: f.ID, ObligationID: f.ObligationID, FeeType: cfgRef(f.FeeTypeRef), Party: f.ResponsibleParty,
				Currency: cfgRef(f.CurrencyRef), TaxProfile: cfgRef(f.TaxProfileRef), TaxApplication: cfgRef(f.TaxApplicationRef),
				IncludedIn: f.IncludedInComponentRef, ZeroPriceReason: f.ZeroPriceReason}
			if b := f.Billing; b != nil {
				fee.Billing = &scenario.Billing{Kind: b.Kind, Amount: b.Amount, Rate: b.Rate, BasisQuantity: b.BasisQuantity, Unit: cfgRef(b.UnitRef),
					Minimum: b.MinimumAmount, PricingMode: b.PricingMode, Tiers: tiersOf(b.Tiers)}
			}
			if c := f.Chargeable; c != nil {
				fee.Chargeable = &scenario.Chargeable{Kind: c.Kind, Measurement: measurementKey(c.MeasurementRef), BillingIncrement: c.BillingIncrement,
					RoundingMode: c.RoundingMode, RoundingScope: c.RoundingScope, MaxScope: c.MaxScope, Divisor: c.DivisorM3PerKG}
			}
			if a := f.Allocation; a != nil {
				alloc := &scenario.FeeAllocation{Basis: a.Basis, Targets: slices.Clone(a.TargetLineIDs), Measurement: measurementKey(a.MeasurementRef)}
				for _, w := range a.Weights {
					alloc.Weights = append(alloc.Weights, scenario.Weight{LineID: w.LineID, Weight: w.Weight})
				}
				fee.Allocation = alloc
			}
			x.Fees = append(x.Fees, fee)
		}
		out.Logistics = x
	}
	return out
}

// configInput is what POL said about one configuration revision, as the evaluation reads it.
func configInput(r polapi.ResolvedConfig) scenario.Config {
	out := scenario.Config{Check: r.Check, ContentHash: r.ContentHash, ValidFrom: kernel.Instant(r.ValidFrom)}
	if !r.ValidUntil.IsZero() {
		out.ValidUntil = kernel.Instant(r.ValidUntil)
	}
	ref := func(c polapi.ConfigRef) scenario.Ref {
		return scenario.Ref{Kind: c.Kind, ID: c.ConfigID, Revision: c.Revision}
	}
	d := r.Data
	switch {
	case d.Currency != nil:
		out.Currency = &scenario.Currency{Code: d.Currency.Code, MinorUnits: d.Currency.MinorUnits}
	case d.Unit != nil:
		out.Unit = &scenario.Unit{Code: d.Unit.Code, Dimension: d.Unit.Dimension, Precision: d.Unit.Precision}
	case d.UnitConversion != nil:
		u := d.UnitConversion
		out.Conversion = &scenario.Conversion{SkuID: u.SkuID, SpecRevision: u.SpecRevision, From: u.FromUnit, To: u.ToUnit, Factor: u.Factor}
	case d.FXRate != nil:
		x := d.FXRate
		out.FXRate = &scenario.FXRate{From: x.FromCurrency, To: x.ToCurrency, Rate: x.Rate, Purpose: x.Purpose, ObservedAt: x.ObservedAt}
	case d.Calendar != nil:
		out.Calendar = &scenario.Calendar{TimeZone: d.Calendar.TimeZone, WorkingWeekdays: slices.Clone(d.Calendar.WorkingWeekdays),
			Holidays: slices.Clone(d.Calendar.Holidays)}
	case d.TradeTerms != nil:
		out.TradeTerms = &scenario.TradeTerms{Incoterm: d.TradeTerms.Incoterm, NamedPlace: d.TradeTerms.NamedPlace}
	case d.PaymentTerms != nil:
		p := &scenario.PaymentTerms{DayBasis: d.PaymentTerms.DayBasis}
		if c := d.PaymentTerms.Calendar; c != nil {
			r := ref(*c)
			p.Calendar = &r
		}
		for _, s := range d.PaymentTerms.Stages {
			p.Stages = append(p.Stages, scenario.PaymentStage{StageID: s.StageID, Percent: s.Percent, Trigger: s.Trigger, Days: s.Days})
		}
		out.PaymentTerms = p
	case d.TaxProfile != nil:
		out.TaxProfile = &scenario.TaxProfile{Rate: d.TaxProfile.Rate, PriceBasis: d.TaxProfile.PriceBasis, Recovery: d.TaxProfile.RecoveryTreatment}
	case d.TaxApplication != nil:
		a := d.TaxApplication
		out.TaxApplication = &scenario.TaxApplication{Side: a.Side, SubjectKind: a.SubjectKind, SubjectID: a.SubjectID, PartyID: a.PartyID,
			TaxProfile: ref(a.TaxProfile)}
	case d.FeeCategory != nil:
		out.FeeCategory = &scenario.FeeCategory{Category: d.FeeCategory.Category, Side: d.FeeCategory.Side}
	case d.PricingPolicy != nil:
		p := d.PricingPolicy
		out.PricingPolicy = &scenario.PricingPolicy{CalculationCurrency: p.CalculationCurrency, MaxFXAgeSeconds: p.MaxFXAgeSeconds,
			TaxProfile: ref(p.TaxProfile)}
	}
	return out
}
