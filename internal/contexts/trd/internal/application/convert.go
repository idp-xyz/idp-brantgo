package application

import (
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func ref(r api.ConfigRef) quote.Ref { return quote.Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision} }

func optRef(r *api.ConfigRef) *quote.Ref {
	if r == nil {
		return nil
	}
	q := ref(*r)
	return &q
}

func apiRef(r quote.Ref) api.ConfigRef {
	return api.ConfigRef{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func optAPIRef(r *quote.Ref) *api.ConfigRef {
	if r == nil {
		return nil
	}
	a := apiRef(*r)
	return &a
}

func business(r api.BusinessRef) quote.BusinessRef {
	return quote.BusinessRef{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func apiBusiness(r quote.BusinessRef) api.BusinessRef {
	return api.BusinessRef{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func evidence(es []api.Evidence) []quote.Evidence {
	if es == nil {
		return nil
	}
	out := make([]quote.Evidence, len(es))
	for i, e := range es {
		out[i] = quote.Evidence{SourceID: e.SourceID, Locator: e.Locator}
	}
	return out
}

func apiEvidence(es []quote.Evidence) []api.Evidence {
	if es == nil {
		return nil
	}
	out := make([]api.Evidence, len(es))
	for i, e := range es {
		out[i] = api.Evidence{SourceID: e.SourceID, Locator: e.Locator}
	}
	return out
}

// quoteData converts a validated draft; expires_at is stored in kernel.FormatTimestamp form, so two
// spellings of one instant are one draft.
func quoteData(a api.QuoteData) quote.Data {
	d := quote.Data{Issuer: optRef(a.IssuerRef), Currency: optRef(a.CurrencyRef), PricingPolicy: optRef(a.PricingPolicyRef),
		SalesFX: optRef(a.SalesFXRef), TradeTerms: optRef(a.TradeTermsRef), PaymentTerms: optRef(a.PaymentTermsRef),
		DocumentTemplate: optRef(a.DocumentTemplateRef), Sequence: optRef(a.SequenceRef), Evidence: evidence(a.Evidence),
		LegalTerms: optRef(a.LegalTermsRef)}
	if r := a.CustomerRef; r != nil {
		d.Customer = &quote.PartyRef{PartyID: r.PartyID, Revision: r.Revision}
	}
	if r := a.ContactRef; r != nil {
		d.Contact = &quote.ContactRef{ContactID: r.ContactID, RowVersion: r.RowVersion}
	}
	if r := a.TradingProfileRef; r != nil {
		d.Profile = &quote.ProfileRef{PartyID: r.PartyID, Revision: r.Revision}
	}
	for _, r := range a.CustomerAddressRefs {
		d.Addresses = append(d.Addresses, quote.AddressRef{PartyID: r.PartyID, PartyRevision: r.PartyRevision, AddressID: r.AddressID})
	}
	if r := a.RfqRef; r != nil {
		d.Rfq = &quote.RfqRef{RfqID: r.RfqID, RowVersion: r.RowVersion}
	}
	if r := a.CostSnapshotRef; r != nil {
		d.Snapshot = &quote.SnapshotRef{SnapshotID: r.SnapshotID, ResultHash: r.ResultHash}
	}
	if r := a.NamedPlaceRef; r != nil {
		p := business(*r)
		d.NamedPlace = &p
	}
	if v := a.Delivery; v != nil {
		dv := quote.Delivery{Kind: v.Kind, Date: v.Date, Days: v.Days, DayBasis: v.DayBasis, Calendar: optRef(v.CalendarRef),
			Milestone: v.Milestone, Place: business(v.PlaceRef), TimeZone: v.TimeZone}
		if v.AnchorRef != nil {
			anchor := business(*v.AnchorRef)
			dv.Anchor = &anchor
		}
		d.Delivery = &dv
	}
	for _, l := range a.Lines {
		d.Lines = append(d.Lines, quote.Line{LineID: l.LineID, RfqLineID: l.RfqLineID, Sku: quote.SkuRef{SkuID: l.Sku.SkuID,
			SpecRevision: l.Sku.SpecRevision}, Quantity: l.Quantity, Unit: ref(l.UnitRef), Conversion: optRef(l.ConversionRef),
			BaseQuantity: l.BaseQuantity, UnitPrice: l.UnitPrice, PriceBasisQuantity: l.PriceBasisQuantity, DiscountRatio: l.DiscountRatio,
			TaxApplication: ref(l.TaxApplicationRef), Evidence: evidence(l.Evidence)})
	}
	for _, c := range a.Charges {
		d.Charges = append(d.Charges, quote.Charge{ChargeID: c.ChargeID, FeeCategory: ref(c.FeeCategoryRef), ObligationKey: c.ObligationKey,
			Amount: c.Amount, Inclusion: c.Inclusion, IncludedLineID: c.IncludedLineID, TaxApplication: ref(c.TaxApplicationRef),
			Evidence: evidence(c.Evidence)})
	}
	if a.ExpiresAt != "" {
		if t, err := kernel.ParseTimestamp(a.ExpiresAt); err == nil {
			d.ExpiresAt = kernel.FormatTimestamp(t)
		} else {
			d.ExpiresAt = a.ExpiresAt // Validate refuses it before it is used
		}
	}
	if a.CustomerNotes != nil {
		n := *a.CustomerNotes
		d.CustomerNotes = &n
	}
	return d
}

func apiData(d quote.Data) api.QuoteData {
	a := api.QuoteData{IssuerRef: optAPIRef(d.Issuer), CurrencyRef: optAPIRef(d.Currency), PricingPolicyRef: optAPIRef(d.PricingPolicy),
		SalesFXRef: optAPIRef(d.SalesFX), TradeTermsRef: optAPIRef(d.TradeTerms), PaymentTermsRef: optAPIRef(d.PaymentTerms),
		DocumentTemplateRef: optAPIRef(d.DocumentTemplate), SequenceRef: optAPIRef(d.Sequence), ExpiresAt: d.ExpiresAt,
		Evidence: apiEvidence(d.Evidence), LegalTermsRef: optAPIRef(d.LegalTerms)}
	if r := d.Customer; r != nil {
		a.CustomerRef = &api.PartyRef{PartyID: r.PartyID, Revision: r.Revision}
	}
	if r := d.Contact; r != nil {
		a.ContactRef = &api.ContactRef{ContactID: r.ContactID, RowVersion: r.RowVersion}
	}
	if r := d.Profile; r != nil {
		a.TradingProfileRef = &api.TradingProfileRef{PartyID: r.PartyID, Revision: r.Revision}
	}
	for _, r := range d.Addresses {
		a.CustomerAddressRefs = append(a.CustomerAddressRefs, api.AddressRoleRef{PartyID: r.PartyID, PartyRevision: r.PartyRevision,
			AddressID: r.AddressID})
	}
	if r := d.Rfq; r != nil {
		a.RfqRef = &api.RfqRef{RfqID: r.RfqID, RowVersion: r.RowVersion}
	}
	if r := d.Snapshot; r != nil {
		a.CostSnapshotRef = &api.CostSnapshotRef{SnapshotID: r.SnapshotID, ResultHash: r.ResultHash}
	}
	if r := d.NamedPlace; r != nil {
		p := apiBusiness(*r)
		a.NamedPlaceRef = &p
	}
	if v := d.Delivery; v != nil {
		dv := api.Delivery{Kind: v.Kind, Date: v.Date, Days: v.Days, DayBasis: v.DayBasis, CalendarRef: optAPIRef(v.Calendar),
			Milestone: v.Milestone, PlaceRef: apiBusiness(v.Place), TimeZone: v.TimeZone}
		if v.Anchor != nil {
			anchor := apiBusiness(*v.Anchor)
			dv.AnchorRef = &anchor
		}
		a.Delivery = &dv
	}
	for _, l := range d.Lines {
		a.Lines = append(a.Lines, api.Line{LineID: l.LineID, RfqLineID: l.RfqLineID, Sku: api.SkuRef{SkuID: l.Sku.SkuID,
			SpecRevision: l.Sku.SpecRevision}, Quantity: l.Quantity, UnitRef: apiRef(l.Unit), ConversionRef: optAPIRef(l.Conversion),
			BaseQuantity: l.BaseQuantity, UnitPrice: l.UnitPrice, PriceBasisQuantity: l.PriceBasisQuantity, DiscountRatio: l.DiscountRatio,
			TaxApplicationRef: apiRef(l.TaxApplication), Evidence: apiEvidence(l.Evidence)})
	}
	for _, c := range d.Charges {
		a.Charges = append(a.Charges, api.Charge{ChargeID: c.ChargeID, FeeCategoryRef: apiRef(c.FeeCategory), ObligationKey: c.ObligationKey,
			Amount: c.Amount, Inclusion: c.Inclusion, IncludedLineID: c.IncludedLineID, TaxApplicationRef: apiRef(c.TaxApplication),
			Evidence: apiEvidence(c.Evidence)})
	}
	if d.CustomerNotes != nil {
		n := *d.CustomerNotes
		a.CustomerNotes = &n
	}
	return a
}

// quoteView is a quote as GetQuote shows it. A frozen revision whose commercial document does not
// hash to its business hash is refused.
func quoteView(q *quote.Quote) (api.QuoteView, error) {
	v := api.QuoteView{QuoteID: q.ID, RootVersion: q.RootVersion, LastRevision: q.LastRevision, Labels: q.Labels,
		InternalNote: q.InternalNote, Control: api.ControlView{State: string(q.Control.State), ControlEpoch: q.Control.Epoch,
			TaskID: q.Control.TaskID, ChangedBy: q.Control.By, ChangedAt: q.Control.At}}
	if w := q.Working(); w != nil {
		v.WorkingRevision = w.Number
	}
	if a := q.Active(); a != nil {
		v.ActiveRevision = a.Number
	}
	for _, r := range q.Revisions {
		rv := api.RevisionView{Revision: r.Number, Status: string(r.Status), BaseRevision: r.Base, ContentHash: r.Hash, Data: apiData(r.Data),
			Contributors: r.Contributors, SavedBy: r.SavedBy, SavedAt: r.SavedAt, AuthorityEpoch: r.SavedEpoch}
		if s := r.Seal; s != nil {
			doc, err := quote.ParseCommercial(s.Commercial, s.BusinessHash)
			if err != nil {
				return api.QuoteView{}, err
			}
			rv.BusinessHash, rv.IssuedAt, rv.ExpiresAt, rv.Commercial = s.BusinessHash, s.IssuedAt, s.ExpiresAt, string(s.Commercial)
			rv.FrozenBy, rv.FreezeEpoch, rv.FrozenAt = s.By, s.Epoch, s.At
			rv.Calculation = calculationView(doc.Calculation)
		}
		if e := r.End; e != nil {
			rv.DiscardedBy, rv.DiscardedAt, rv.DiscardReason, rv.DiscardProofs = e.By, e.At, e.Reason, apiEvidence(e.Evidence)
		}
		rv.Formal = formalView(r.Formal)
		if c := r.Closed; c != nil {
			rv.Closed = &api.ClosureView{Status: string(c.Status), ClosedBy: c.Closed.By, ClosedAt: c.Closed.At, Reason: c.Closed.Reason,
				Evidence: apiEvidence(c.Closed.Evidence), RootVersion: c.RootVersion}
		}
		v.Revisions = append(v.Revisions, rv)
	}
	for _, h := range q.Holds {
		v.Holds = append(v.Holds, api.HoldView{HoldID: h.ID, BusinessRevision: h.Revision, Cause: h.Cause, ApprovalID: h.ApprovalID,
			Code: h.Code, RegisteredBy: h.Registered.By, RegisteredAt: h.Registered.At, RootVersion: h.RootVersion})
	}
	if c := q.Slot; c != nil {
		v.Conversion = &api.ConversionView{Slot: c.Slot, BusinessRevision: c.Revision, BusinessHash: c.BusinessHash, OrderID: c.OrderID,
			TakenBy: c.Taken.By, TakenAt: c.Taken.At, RootVersion: c.RootVersion}
	}
	for _, r := range q.Reviews {
		rv := api.ReviewView{ReviewRequestID: r.ID, BusinessRevision: r.Revision, BusinessHash: r.BusinessHash, TargetCommand: r.Target,
			ExpectedActiveRevision: r.ExpectedActive, MarginExceptionRequired: r.MarginExceptionRequired, Status: string(r.Status),
			RequestedBy: r.Requested.By, RequestedAt: r.Requested.At}
		if d := r.Decided; d != nil {
			rv.DecidedBy, rv.DecidedAt, rv.Reason, rv.Evidence = d.By, d.At, d.Reason, apiEvidence(d.Evidence)
		}
		v.Reviews = append(v.Reviews, rv)
	}
	for _, a := range q.Approvals {
		av := api.ApprovalView{ApprovalID: a.ID, ReviewRequestID: a.ReviewID, BusinessRevision: a.Revision, BusinessHash: a.BusinessHash,
			TargetCommand: a.Target, ExpectedActiveRevision: a.ExpectedActive, NormalizationVersion: a.Rules.NormalizationVersion,
			CalculationVersion: a.Rules.CalculationVersion, PricingPolicyRef: apiRef(a.Rules.PricingPolicy), Reviewer: a.Approved.By,
			DecidedAt: a.Approved.At, ExpiresAt: a.ExpiresAt, Reason: a.Approved.Reason, Evidence: apiEvidence(a.Approved.Evidence),
			Effectivity: string(a.Effectivity), EffectivityVersion: a.EffectivityVersion}
		if r := a.Revocation; r != nil {
			av.RevokedBy, av.RevokedAt, av.RevokeReason, av.RevokeEvidence = r.By, r.At, r.Reason, apiEvidence(r.Evidence)
		}
		if e := a.Exception; e != nil {
			av.MarginException = &api.MarginExceptionView{ApprovedBy: e.Approved.By, ApprovedAt: e.Approved.At, BusinessHash: e.BusinessHash,
				PricingPolicyRef: apiRef(e.Rules.PricingPolicy), CalculationVersion: e.Rules.CalculationVersion, MinimumMargin: e.MinimumMargin,
				Shortfall: e.Shortfall, Currency: e.Currency, Reason: e.Approved.Reason, Evidence: apiEvidence(e.Approved.Evidence)}
		}
		v.Approvals = append(v.Approvals, av)
	}
	return v, nil
}

// calculationView is the summary of a sealed calculation, nil for none.
func calculationView(c *quote.Calculation) *api.CalculationView {
	if c == nil {
		return nil
	}
	v := &api.CalculationView{NetMinor: c.NetMinor, TaxMinor: c.TaxMinor, PayableMinor: c.PayableMinor,
		ComparisonCurrencyRef: apiRef(c.ComparisonCurrency), SalesFXRef: optAPIRef(c.SalesFX), ComparisonRevenueMinor: c.ComparisonRevenueMinor,
		CostMinor: c.CostMinor, ProfitMinor: c.ProfitMinor, MinimumMargin: c.MinimumMargin, MarginRule: c.MarginRule,
		MarginShortfall: c.MarginShortfall, PricingPolicyRef: apiRef(c.CostScope.PricingPolicy), CalculationVersion: c.CalculationVersion}
	for _, p := range c.Payments {
		v.Payments = append(v.Payments, api.PaymentView{StageID: p.StageID, Percent: p.Percent, AmountMinor: p.AmountMinor})
	}
	return v
}
