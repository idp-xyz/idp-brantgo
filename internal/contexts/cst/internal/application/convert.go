package application

import (
	"slices"

	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/costing"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/measurement"
)

func ref(r api.ConfigRef) costing.Ref {
	return costing.Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func refPtr(r *api.ConfigRef) *costing.Ref {
	if r == nil {
		return nil
	}
	c := ref(*r)
	return &c
}

func refs(rs []api.ConfigRef) []costing.Ref {
	if rs == nil {
		return nil
	}
	out := make([]costing.Ref, len(rs))
	for i, r := range rs {
		out[i] = ref(r)
	}
	return out
}

func business(r api.BusinessRef) costing.Ref {
	return costing.Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func evidence(es []api.Evidence) []costing.Evidence {
	out := make([]costing.Evidence, len(es))
	for i, e := range es {
		out[i] = costing.Evidence{SourceID: e.SourceID, Locator: e.Locator}
	}
	return out
}

// contextData is the domain content of a request's data.
func contextData(d api.ContextData) costing.Data {
	out := costing.Data{Kind: d.Kind, CalculationCurrency: refPtr(d.CalculationCurrencyRef), PricingPolicy: refPtr(d.PricingPolicyRef),
		RequestedCurrency: refPtr(d.RequestedCurrencyRef), RequestedTradeTerms: refPtr(d.RequestedTradeTermsRef),
		RequestedPaymentTerms: refPtr(d.RequestedPaymentTermsRef)}
	if r := d.RfqRef; r != nil {
		out.Rfq = &costing.RfqRef{RfqID: r.RfqID, RowVersion: r.RowVersion}
	}
	for _, l := range d.Lines {
		out.Lines = append(out.Lines, costing.Line{LineID: l.LineID, SkuID: l.SkuID, SkuSpecRevision: l.SkuSpecRevision, Quantity: l.Quantity,
			Unit: ref(l.UnitRef), Conversions: refs(l.ConversionRefs)})
	}
	if b := d.ComparisonBasis; b != nil {
		dv := b.Delivery
		out.Basis = &costing.Basis{SpecificationBasis: b.SpecificationBasis, NamedPlace: business(b.NamedPlaceRef),
			RequiredFeeTypes: refs(b.RequiredFeeTypeRefs), TaxBasis: b.TaxBasis, CostBasis: b.CostBasis,
			SalesTradeTerms: ref(b.SalesTradeTermsRef), SalesPaymentTerms: ref(b.SalesPaymentTermsRef),
			Delivery: costing.Delivery{Kind: dv.Kind, Date: dv.Date, Days: dv.Days, DayBasis: dv.DayBasis, Calendar: refPtr(dv.CalendarRef),
				Milestone: dv.Milestone, Place: business(dv.PlaceRef), TimeZone: dv.TimeZone}}
		if a := dv.AnchorRef; a != nil {
			anchor := business(*a)
			out.Basis.Delivery.Anchor = &anchor
		}
	}
	for _, a := range d.Assumptions {
		out.Assumptions = append(out.Assumptions, costing.Assumption{ID: a.AssumptionID, Path: a.Path, ValueText: a.ValueText,
			Reason: a.Reason, Evidence: evidence(a.Evidence)})
	}
	return out
}

func configRef(r costing.Ref) api.ConfigRef {
	return api.ConfigRef{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func configRefPtr(r *costing.Ref) *api.ConfigRef {
	if r == nil {
		return nil
	}
	c := configRef(*r)
	return &c
}

func configRefs(rs []costing.Ref) []api.ConfigRef {
	out := make([]api.ConfigRef, len(rs))
	for i, r := range rs {
		out[i] = configRef(r)
	}
	return out
}

func businessRef(r costing.Ref) api.BusinessRef {
	return api.BusinessRef{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func evidenceView(es []costing.Evidence) []api.Evidence {
	out := make([]api.Evidence, len(es))
	for i, e := range es {
		out[i] = api.Evidence{SourceID: e.SourceID, Locator: e.Locator}
	}
	return out
}

// contextDataView is the stored content as the contract shows it; lists are never nil.
func contextDataView(d costing.Data) api.ContextData {
	out := api.ContextData{Kind: d.Kind, CalculationCurrencyRef: configRefPtr(d.CalculationCurrency), PricingPolicyRef: configRefPtr(d.PricingPolicy),
		RequestedCurrencyRef: configRefPtr(d.RequestedCurrency), RequestedTradeTermsRef: configRefPtr(d.RequestedTradeTerms),
		RequestedPaymentTermsRef: configRefPtr(d.RequestedPaymentTerms), Lines: []api.ContextLine{}, Assumptions: []api.Assumption{}}
	if r := d.Rfq; r != nil {
		out.RfqRef = &api.RfqRef{RfqID: r.RfqID, RowVersion: r.RowVersion}
	}
	for _, l := range d.Lines {
		out.Lines = append(out.Lines, api.ContextLine{LineID: l.LineID, SkuID: l.SkuID, SkuSpecRevision: l.SkuSpecRevision, Quantity: l.Quantity,
			UnitRef: configRef(l.Unit), ConversionRefs: configRefs(l.Conversions)})
	}
	if b := d.Basis; b != nil {
		out.ComparisonBasis = basisView(*b)
	}
	for _, a := range d.Assumptions {
		out.Assumptions = append(out.Assumptions, assumptionView(a))
	}
	return out
}

func basisView(b costing.Basis) *api.ComparisonBasis {
	dv := b.Delivery
	out := &api.ComparisonBasis{SpecificationBasis: b.SpecificationBasis, NamedPlaceRef: businessRef(b.NamedPlace),
		RequiredFeeTypeRefs: configRefs(b.RequiredFeeTypes), TaxBasis: b.TaxBasis, CostBasis: b.CostBasis,
		SalesTradeTermsRef: configRef(b.SalesTradeTerms), SalesPaymentTermsRef: configRef(b.SalesPaymentTerms),
		Delivery: api.Delivery{Kind: dv.Kind, Date: dv.Date, Days: dv.Days, DayBasis: dv.DayBasis, CalendarRef: configRefPtr(dv.Calendar),
			Milestone: dv.Milestone, PlaceRef: businessRef(dv.Place), TimeZone: dv.TimeZone}}
	if a := dv.Anchor; a != nil {
		anchor := businessRef(*a)
		out.Delivery.AnchorRef = &anchor
	}
	return out
}

func assumptionView(a costing.Assumption) api.Assumption {
	return api.Assumption{AssumptionID: a.ID, Path: a.Path, ValueText: a.ValueText, Reason: a.Reason, Evidence: evidenceView(a.Evidence)}
}

func boundLines(ls []costing.RfqLine) []api.BoundLine {
	out := make([]api.BoundLine, len(ls))
	for i, l := range ls {
		out[i] = api.BoundLine(l)
	}
	return out
}

func binding(b costing.Binding) api.RfqBinding {
	return api.RfqBinding{RfqID: b.RfqID, RowVersion: b.RowVersion, LinesHash: b.LinesHash}
}

// contextView is a costing context as its reader sees it.
func contextView(cc *costing.Context) api.ContextView {
	v := api.ContextView{ContextID: cc.ID, RootVersion: cc.RootVersion, LastRevision: cc.LastRevision, Revisions: []api.ContextRevisionView{}}
	if w := cc.Working(); w != nil {
		v.WorkingRevision = w.Number
	}
	if a := cc.Active(); a != nil {
		v.ActiveRevision = a.Number
	}
	for _, r := range cc.Revisions {
		rv := api.ContextRevisionView{Revision: r.Number, Status: string(r.Status), BaseRevision: r.Base, ContentHash: r.Hash,
			Data: contextDataView(r.Data), Contributors: slices.Clone(r.Contributors), SavedBy: r.SavedBy, SavedAt: r.SavedAt,
			Proposals: proposals(r), SupersededAt: r.SupersededAt, VerificationEvidence: []api.Evidence{}, RfqLines: []api.BoundLine{},
			EndEvidence: []api.Evidence{}}
		if s := r.Seal; s != nil {
			rv.VerifiedBy, rv.VerifiedAt, rv.AuthorityEpoch, rv.Rfq = s.By, s.At, s.Epoch, binding(s.Rfq)
			rv.VerificationEvidence, rv.RfqLines = evidenceView(s.Evidence), boundLines(s.RfqLines)
		}
		if e := r.End; e != nil {
			rv.EndedBy, rv.EndedAt, rv.EndReason, rv.EndEvidence = e.By, e.At, e.Reason, evidenceView(e.Evidence)
		}
		v.Revisions = append(v.Revisions, rv)
	}
	return v
}

// proposals returns the proposers of the revision's assumptions in assumption order.
func proposals(r *costing.Revision) []api.AssumptionProposal {
	out := []api.AssumptionProposal{}
	for _, a := range r.Data.Assumptions {
		if p, ok := r.Proposals[a.ID]; ok {
			out = append(out, api.AssumptionProposal{AssumptionID: a.ID, ProposedBy: p.By, ProposedAt: p.At})
		}
	}
	return out
}

// verifiedContext is a sealed revision as the lookups give it.
func verifiedContext(id string, r *costing.Revision) api.VerifiedContext {
	d := r.Data
	v := api.VerifiedContext{Ref: api.ContextRef{ContextID: id, Revision: r.Number}, Status: string(r.Status), Kind: d.Kind,
		ContentHash: r.Hash, RfqLines: []api.BoundLine{}, Lines: contextDataView(d).Lines, ConfigRefs: configRefs(d.ConfigRefs()),
		RequestedCurrencyRef: configRefPtr(d.RequestedCurrency), RequestedTradeTermsRef: configRefPtr(d.RequestedTradeTerms),
		RequestedPaymentTermsRef: configRefPtr(d.RequestedPaymentTerms), Assumptions: []api.BoundAssumption{}}
	if d.CalculationCurrency != nil {
		v.CalculationCurrencyRef = configRef(*d.CalculationCurrency)
	}
	if d.PricingPolicy != nil {
		v.PricingPolicyRef = configRef(*d.PricingPolicy)
	}
	if d.Basis != nil {
		v.ComparisonBasis = *basisView(*d.Basis)
	}
	if s := r.Seal; s != nil {
		v.Rfq, v.VerifiedBy, v.VerifiedAt, v.AuthorityEpoch, v.Evidence = binding(s.Rfq), s.By, s.At, s.Epoch, evidenceView(s.Evidence)
		v.RfqLines = boundLines(s.RfqLines)
	}
	for _, a := range d.Assumptions {
		p := r.Proposals[a.ID]
		v.Assumptions = append(v.Assumptions, api.BoundAssumption{Assumption: assumptionView(a), ProposedBy: p.By, ProposedAt: p.At,
			VerificationStatus: api.AssumptionUnverified})
	}
	return v
}

func measurementData(d api.MeasurementData) measurement.Data {
	out := measurement.Data{Kind: d.Kind, ObservedAt: d.ObservedAt, DeclaredVolumeM3: d.DeclaredVolumeM3, EstimateReason: d.EstimateReason,
		Packages: []measurement.Package{}, Evidence: []measurement.Evidence{}}
	for _, p := range d.Packages {
		out.Packages = append(out.Packages, measurement.Package{ID: p.PackageID, Count: p.Count, LengthM: p.LengthM, WidthM: p.WidthM,
			HeightM: p.HeightM, NetKG: p.NetKG, GrossKG: p.GrossKG, LineIDs: slices.Clone(p.LineIDs)})
	}
	for _, e := range d.Evidence {
		out.Evidence = append(out.Evidence, measurement.Evidence{SourceID: e.SourceID, Locator: e.Locator})
	}
	return out
}

func measurementEvidence(es []api.Evidence) []measurement.Evidence {
	out := make([]measurement.Evidence, len(es))
	for i, e := range es {
		out[i] = measurement.Evidence{SourceID: e.SourceID, Locator: e.Locator}
	}
	return out
}

func measurementEvidenceView(es []measurement.Evidence) []api.Evidence {
	out := make([]api.Evidence, len(es))
	for i, e := range es {
		out[i] = api.Evidence{SourceID: e.SourceID, Locator: e.Locator}
	}
	return out
}

func measurementDataView(d measurement.Data) api.MeasurementData {
	out := api.MeasurementData{Kind: d.Kind, ObservedAt: d.ObservedAt, DeclaredVolumeM3: d.DeclaredVolumeM3, EstimateReason: d.EstimateReason,
		Packages: []api.Package{}, Evidence: measurementEvidenceView(d.Evidence)}
	for _, p := range d.Packages {
		out.Packages = append(out.Packages, api.Package{PackageID: p.ID, Count: p.Count, LengthM: p.LengthM, WidthM: p.WidthM,
			HeightM: p.HeightM, NetKG: p.NetKG, GrossKG: p.GrossKG, LineIDs: slices.Clone(p.LineIDs)})
	}
	return out
}

func measurementRevisionView(r *measurement.Revision) api.MeasurementRevisionView {
	v := api.MeasurementRevisionView{Revision: r.Number, Status: string(r.Status), ContentHash: r.Hash, Data: measurementDataView(r.Data),
		LineIDs: r.Data.LineIDs(), Reason: r.Reason, RecordedBy: r.RecordedBy, RecordedAt: r.RecordedAt, AuthorityEpoch: r.Epoch,
		SupersededAt: r.SupersededAt, WithdrawEvidence: []api.Evidence{}}
	if w := r.Withdrawal; w != nil {
		v.WithdrawnBy, v.WithdrawnAt, v.WithdrawReason, v.WithdrawEvidence = w.By, w.At, w.Reason, measurementEvidenceView(w.Evidence)
	}
	return v
}

func measurementView(s *measurement.Snapshot) api.MeasurementView {
	v := api.MeasurementView{MeasurementID: s.ID, RootVersion: s.RootVersion, LastRevision: s.LastRevision, Revisions: []api.MeasurementRevisionView{}}
	if a := s.Active(); a != nil {
		v.ActiveRevision = a.Number
	}
	for _, r := range s.Revisions {
		v.Revisions = append(v.Revisions, measurementRevisionView(r))
	}
	return v
}

func resolvedMeasurement(id string, r *measurement.Revision) api.ResolvedMeasurement {
	return api.ResolvedMeasurement{Ref: api.MeasurementRef{MeasurementID: id, Revision: r.Number}, Status: string(r.Status),
		ContentHash: r.Hash, Data: measurementDataView(r.Data), LineIDs: r.Data.LineIDs(), RecordedBy: r.RecordedBy,
		RecordedAt: r.RecordedAt, AuthorityEpoch: r.Epoch}
}

// path of the first field of d that names r, for a guard's rejection.
func fieldPath(d costing.Data, r costing.Ref) string {
	for _, f := range d.ConfigFields() {
		if f.Ref == r {
			return f.Path
		}
	}
	return "/args/data"
}
