package application

import (
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/config"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// content turns a save the contract admitted into domain content. Timestamps take their canonical
// form, so equal instants always hash equally.
func content(cmd api.SaveDraft) config.Content {
	c := config.Content{Kind: config.Kind(cmd.Kind), ValidFrom: instant(cmd.ValidFrom)}
	if cmd.ValidUntil != "" {
		c.ValidUntil = instant(cmd.ValidUntil)
	}
	d := cmd.Data
	switch {
	case d.Currency != nil:
		v := config.Currency(*d.Currency)
		c.Currency = &v
	case d.Unit != nil:
		v := config.Unit(*d.Unit)
		c.Unit = &v
	case d.FXRate != nil:
		v := config.FXRate(*d.FXRate)
		v.ObservedAt = kernel.FormatTimestamp(instant(v.ObservedAt))
		c.FXRate = &v
	case d.PaymentTerms != nil:
		v := config.PaymentTerms{Name: d.PaymentTerms.Name, Stages: make([]config.PaymentStage, len(d.PaymentTerms.Stages))}
		for i, s := range d.PaymentTerms.Stages {
			v.Stages[i] = config.PaymentStage(s)
		}
		c.PaymentTerms = &v
	case d.NumberSequence != nil:
		v := config.NumberSequence(*d.NumberSequence)
		c.NumberSequence = &v
	}
	return c
}

// instant parses a timestamp the contract admitted.
func instant(s string) time.Time {
	t, _ := kernel.ParseTimestamp(s)
	return kernel.Instant(t)
}

// data returns the data of c in the contract's shape.
func data(c config.Content) api.ConfigData {
	var d api.ConfigData
	switch {
	case c.Currency != nil:
		v := api.Currency(*c.Currency)
		d.Currency = &v
	case c.Unit != nil:
		v := api.Unit(*c.Unit)
		d.Unit = &v
	case c.FXRate != nil:
		v := api.FXRate(*c.FXRate)
		d.FXRate = &v
	case c.PaymentTerms != nil:
		v := api.PaymentTerms{Name: c.PaymentTerms.Name, Stages: make([]api.PaymentStage, len(c.PaymentTerms.Stages))}
		for i, s := range c.PaymentTerms.Stages {
			v.Stages[i] = api.PaymentStage(s)
		}
		d.PaymentTerms = &v
	case c.NumberSequence != nil:
		v := api.NumberSequence(*c.NumberSequence)
		d.NumberSequence = &v
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
