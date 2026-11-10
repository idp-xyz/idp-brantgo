package application

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/config"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// content turns a save the contract admitted into domain content. Timestamps take their canonical
// form and a calendar's weekdays and holidays their ascending order, so equal content always hashes
// equally.
func content(cmd api.SaveDraft) config.Content {
	c := config.Content{Kind: config.Kind(cmd.Kind), ValidFrom: instant(cmd.ValidFrom)}
	if cmd.ValidUntil != "" {
		c.ValidUntil = instant(cmd.ValidUntil)
	}
	d := cmd.Data
	switch {
	case d.CompanyProfile != nil:
		c.CompanyProfile = ptr(config.CompanyProfile(*d.CompanyProfile))
	case d.Currency != nil:
		c.Currency = ptr(config.Currency(*d.Currency))
	case d.Unit != nil:
		c.Unit = ptr(config.Unit(*d.Unit))
	case d.UnitConversion != nil:
		c.UnitConversion = ptr(config.UnitConversion(*d.UnitConversion))
	case d.FXRate != nil:
		v := config.FXRate(*d.FXRate)
		v.ObservedAt = kernel.FormatTimestamp(instant(v.ObservedAt))
		c.FXRate = &v
	case d.Calendar != nil:
		c.Calendar = &config.Calendar{TimeZone: d.Calendar.TimeZone, WorkingWeekdays: slices.Sorted(slices.Values(d.Calendar.WorkingWeekdays)),
			Holidays: append([]string{}, slices.Sorted(slices.Values(d.Calendar.Holidays))...)}
	case d.TradeTerms != nil:
		t, l := d.TradeTerms, d.TradeTerms.Delivery
		c.TradeTerms = &config.TradeTerms{Incoterm: t.Incoterm, Edition: t.Edition, NamedPlace: t.NamedPlace, ResponsibilityNote: t.ResponsibilityNote,
			Delivery: config.Delivery{Kind: l.Kind, Date: l.Date, AnchorEvent: l.AnchorEvent, Days: l.Days, DayBasis: l.DayBasis, Calendar: toRef(l.Calendar)}}
	case d.PaymentTerms != nil:
		p := d.PaymentTerms
		v := config.PaymentTerms{Name: p.Name, Stages: make([]config.PaymentStage, len(p.Stages)), DayBasis: p.DayBasis, Calendar: toRef(p.Calendar)}
		for i, s := range p.Stages {
			v.Stages[i] = config.PaymentStage(s)
		}
		c.PaymentTerms = &v
	case d.TaxProfile != nil:
		c.TaxProfile = ptr(config.TaxProfile(*d.TaxProfile))
	case d.TaxApplication != nil:
		a := d.TaxApplication
		c.TaxApplication = &config.TaxApplication{Name: a.Name, Side: a.Side, SubjectKind: a.SubjectKind, SubjectID: a.SubjectID, PartyID: a.PartyID,
			TaxProfile: config.Ref(a.TaxProfile), Jurisdiction: a.Jurisdiction, BasisNote: a.BasisNote}
	case d.FeeCategory != nil:
		c.FeeCategory = ptr(config.FeeCategory(*d.FeeCategory))
	case d.PricingPolicy != nil:
		p := d.PricingPolicy
		c.PricingPolicy = &config.PricingPolicy{Name: p.Name, CalculationCurrency: p.CalculationCurrency, MinimumMargin: p.MinimumMargin,
			QuoteValidDays: p.QuoteValidDays, MaxFXAgeSeconds: p.MaxFXAgeSeconds, TaxProfile: config.Ref(p.TaxProfile)}
	case d.BankAccount != nil:
		c.BankAccount = ptr(config.BankAccount(*d.BankAccount))
	case d.NumberSequence != nil:
		c.NumberSequence = ptr(config.NumberSequence(*d.NumberSequence))
	case d.DocumentTemplate != nil:
		v := config.DocumentTemplate(*d.DocumentTemplate)
		v.FieldNames = slices.Clone(v.FieldNames)
		c.DocumentTemplate = &v
	case d.QuoteDocumentTemplate != nil:
		v := config.QuoteDocumentTemplate(*d.QuoteDocumentTemplate)
		v.Sections = slices.Clone(v.Sections)
		c.QuoteDocumentTemplate = &v
	case d.CreditPolicy != nil:
		p := d.CreditPolicy
		v := config.CreditPolicy{Name: p.Name, Currency: p.Currency, DefaultLimit: p.DefaultLimit, LimitSources: slices.Clone(p.LimitSources),
			ApprovalTiers: make([]config.CreditApprovalTier, len(p.ApprovalTiers)), OverdueToleranceDays: p.OverdueToleranceDays}
		for i, t := range p.ApprovalTiers {
			v.ApprovalTiers[i] = config.CreditApprovalTier(t)
		}
		c.CreditPolicy = &v
	case d.ScreeningRegulation != nil:
		v := config.ScreeningRegulation{Lists: make([]config.ScreeningList, len(d.ScreeningRegulation.Lists)),
			RescreenIntervalDays: d.ScreeningRegulation.RescreenIntervalDays}
		for i, l := range d.ScreeningRegulation.Lists {
			v.Lists[i] = config.ScreeningList(l)
		}
		c.ScreeningRegulation = &v
	}
	return c
}

func ptr[T any](v T) *T { return &v }

func toRef(r *api.ConfigRef) *config.Ref {
	if r == nil {
		return nil
	}
	return ptr(config.Ref(*r))
}

func fromRef(r *config.Ref) *api.ConfigRef {
	if r == nil {
		return nil
	}
	return ptr(api.ConfigRef(*r))
}

// instant parses a timestamp the contract admitted.
func instant(s string) time.Time {
	t, _ := kernel.ParseTimestamp(s)
	return kernel.Instant(t)
}

// data returns the data of c in the contract's shape. Bank account numbers come back whole; queries
// mask them for readers without the sensitive permission.
func data(c config.Content) api.ConfigData {
	var d api.ConfigData
	switch {
	case c.CompanyProfile != nil:
		d.CompanyProfile = ptr(api.CompanyProfile(*c.CompanyProfile))
	case c.Currency != nil:
		d.Currency = ptr(api.Currency(*c.Currency))
	case c.Unit != nil:
		d.Unit = ptr(api.Unit(*c.Unit))
	case c.UnitConversion != nil:
		d.UnitConversion = ptr(api.UnitConversion(*c.UnitConversion))
	case c.FXRate != nil:
		d.FXRate = ptr(api.FXRate(*c.FXRate))
	case c.Calendar != nil:
		d.Calendar = &api.Calendar{TimeZone: c.Calendar.TimeZone, WorkingWeekdays: slices.Clone(c.Calendar.WorkingWeekdays),
			Holidays: slices.Clone(c.Calendar.Holidays)}
	case c.TradeTerms != nil:
		t, l := c.TradeTerms, c.TradeTerms.Delivery
		d.TradeTerms = &api.TradeTerms{Incoterm: t.Incoterm, Edition: t.Edition, NamedPlace: t.NamedPlace, ResponsibilityNote: t.ResponsibilityNote,
			Delivery: api.Delivery{Kind: l.Kind, Date: l.Date, AnchorEvent: l.AnchorEvent, Days: l.Days, DayBasis: l.DayBasis, Calendar: fromRef(l.Calendar)}}
	case c.PaymentTerms != nil:
		p := c.PaymentTerms
		v := api.PaymentTerms{Name: p.Name, Stages: make([]api.PaymentStage, len(p.Stages)), DayBasis: p.DayBasis, Calendar: fromRef(p.Calendar)}
		for i, s := range p.Stages {
			v.Stages[i] = api.PaymentStage(s)
		}
		d.PaymentTerms = &v
	case c.TaxProfile != nil:
		d.TaxProfile = ptr(api.TaxProfile(*c.TaxProfile))
	case c.TaxApplication != nil:
		a := c.TaxApplication
		d.TaxApplication = &api.TaxApplication{Name: a.Name, Side: a.Side, SubjectKind: a.SubjectKind, SubjectID: a.SubjectID, PartyID: a.PartyID,
			TaxProfile: api.ConfigRef(a.TaxProfile), Jurisdiction: a.Jurisdiction, BasisNote: a.BasisNote}
	case c.FeeCategory != nil:
		d.FeeCategory = ptr(api.FeeCategory(*c.FeeCategory))
	case c.PricingPolicy != nil:
		p := c.PricingPolicy
		d.PricingPolicy = &api.PricingPolicy{Name: p.Name, CalculationCurrency: p.CalculationCurrency, MinimumMargin: p.MinimumMargin,
			QuoteValidDays: p.QuoteValidDays, MaxFXAgeSeconds: p.MaxFXAgeSeconds, TaxProfile: api.ConfigRef(p.TaxProfile)}
	case c.BankAccount != nil:
		d.BankAccount = ptr(api.BankAccount(*c.BankAccount))
	case c.NumberSequence != nil:
		d.NumberSequence = ptr(api.NumberSequence(*c.NumberSequence))
	case c.DocumentTemplate != nil:
		v := api.DocumentTemplate(*c.DocumentTemplate)
		v.FieldNames = slices.Clone(v.FieldNames)
		d.DocumentTemplate = &v
	case c.QuoteDocumentTemplate != nil:
		v := api.QuoteDocumentTemplate(*c.QuoteDocumentTemplate)
		v.Sections = slices.Clone(v.Sections)
		d.QuoteDocumentTemplate = &v
	case c.CreditPolicy != nil:
		p := c.CreditPolicy
		v := api.CreditPolicy{Name: p.Name, Currency: p.Currency, DefaultLimit: p.DefaultLimit, LimitSources: slices.Clone(p.LimitSources),
			ApprovalTiers: make([]api.CreditApprovalTier, len(p.ApprovalTiers)), OverdueToleranceDays: p.OverdueToleranceDays}
		for i, t := range p.ApprovalTiers {
			v.ApprovalTiers[i] = api.CreditApprovalTier(t)
		}
		d.CreditPolicy = &v
	case c.ScreeningRegulation != nil:
		v := api.ScreeningRegulation{Lists: make([]api.ScreeningList, len(c.ScreeningRegulation.Lists)),
			RescreenIntervalDays: c.ScreeningRegulation.RescreenIntervalDays}
		for i, l := range c.ScreeningRegulation.Lists {
			v.Lists[i] = api.ScreeningList(l)
		}
		d.ScreeningRegulation = &v
	}
	return d
}

func revisionView(r *config.Revision) api.RevisionView {
	v := api.RevisionView{Revision: r.Number, Status: string(r.Status), ContentHash: r.Hash, ValidFrom: r.Content.ValidFrom,
		ValidUntil: r.Content.ValidUntil, Data: data(r.Content), SavedBy: r.SavedBy, SavedAt: r.SavedAt, EndedAt: r.EndedAt}
	if p := r.Publication; p != nil {
		v.PublishedBy, v.PublishedAt, v.EvidenceIDs = p.By, p.At, p.EvidenceIDs
	}
	return v
}
