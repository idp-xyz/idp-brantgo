package application

import (
	"context"
	"slices"
	"time"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
)

// refs are what a document and a command refer to in SUP's upstream contexts.
type refs struct {
	parties []string
	rfqs    []string
	specs   []catapi.SpecRef
	codes   []catapi.ItemCode
	configs []polapi.ConfigRef
	roles   []ptyapi.ServiceRoleRef
	sources []string
}

// facts are what the upstream contexts said about refs at instant at, each in a transaction of its
// own before SUP's: references that do not exist, or that the caller does not see, are absent.
type facts struct {
	at      time.Time
	parties map[string]ptyapi.PartyRef
	rfqs    map[string]inqapi.ResolvedRfq
	specs   map[catapi.SpecRef]catapi.SpecStanding
	codes   map[catapi.ItemCode]catapi.ResolvedItemCode
	configs map[polapi.ConfigRef]polapi.ResolvedConfig
	roles   map[ptyapi.ServiceRoleRef]ptyapi.ResolvedServiceRole
	sources map[string]evdapi.SourceRef
}

func polRef(r *api.ConfigRef) polapi.ConfigRef {
	return polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ConfigID, Revision: r.Revision}
}

func (r *refs) config(c *api.ConfigRef) {
	if c != nil {
		r.configs = append(r.configs, polRef(c))
	}
}

func (r *refs) evidence(ev []api.Evidence) {
	for _, e := range ev {
		r.sources = append(r.sources, e.SourceID)
	}
}

func (r *refs) party(p *api.PartyRef) {
	if p != nil {
		r.parties = append(r.parties, p.PartyID)
	}
}

func (r *refs) rfq(q *api.RfqRef) {
	if q != nil {
		r.rfqs = append(r.rfqs, q.RfqID)
	}
}

// lines collects the references of condition lines; supplier is the party whose item codes they
// cite ("" when unknown).
func (r *refs) lines(ls []api.ConditionLine, supplier string) {
	for _, l := range ls {
		if l.Sku != nil {
			r.specs = append(r.specs, catapi.SpecRef{SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision})
		}
		if l.ItemCodeMapping != nil && supplier != "" {
			r.codes = append(r.codes, catapi.ItemCode{PartyID: supplier, PartyRole: catapi.RoleSupplier, ExternalCode: l.SupplierItemCode})
		}
		for _, c := range []*api.ConfigRef{l.PurchaseUnitRef, l.PriceUnitRef, l.TierUnitRef, l.CurrencyRef, l.TaxProfileRef,
			l.TaxApplicationRef, l.TradeTermsRef, l.PaymentTermsRef} {
			r.config(c)
		}
		if l.MOQ != nil {
			r.config(l.MOQ.UnitRef)
		}
		for i := range l.ConversionRefs {
			r.config(&l.ConversionRefs[i])
		}
		if l.Delivery != nil {
			r.config(l.Delivery.CalendarRef)
		}
	}
}

// document collects the references of a supply condition's content.
func (r *refs) document(d document) {
	switch d.kind {
	case api.KindSupplierOffer:
		o := d.offer
		r.party(o.Supplier)
		r.rfq(o.Rfq)
		r.evidence(o.Evidence)
		supplier := ""
		if o.Supplier != nil {
			supplier = o.Supplier.PartyID
		}
		r.lines(o.Lines, supplier)
	case api.KindPurchaseTerms:
		t := d.terms
		r.party(t.Supplier)
		r.evidence(t.Evidence)
		supplier := ""
		if t.Supplier != nil {
			supplier = t.Supplier.PartyID
		}
		r.lines(t.Lines, supplier)
	case api.KindLogisticsOffer:
		l := d.logistics
		if s := l.ServiceRoleRef; s != nil {
			r.roles = append(r.roles, ptyapi.ServiceRoleRef{PartyID: s.PartyID, ServiceRoleID: s.ServiceRoleID, Revision: s.Revision})
		}
		r.evidence(l.Evidence)
		for _, f := range l.Fees {
			for _, c := range []*api.ConfigRef{f.FeeTypeRef, f.CurrencyRef, f.TaxProfileRef, f.TaxApplicationRef} {
				r.config(c)
			}
			if f.Billing != nil {
				r.config(f.Billing.UnitRef)
			}
		}
	}
}

// supplierRfq collects the references of a supplier RFQ's content.
func (r *refs) supplierRfq(d api.SupplierRfq) {
	r.party(d.Supplier)
	r.rfq(d.Rfq)
	r.evidence(d.Evidence)
}

// lookupBatch is the size of one lookup: the smallest limit of the upstream contexts.
const lookupBatch = 200

func batches[T comparable](all []T) [][]T {
	var unique []T
	for _, v := range all {
		if !slices.Contains(unique, v) {
			unique = append(unique, v)
		}
	}
	var out [][]T
	for len(unique) > 0 {
		n := min(lookupBatch, len(unique))
		out = append(out, unique[:n])
		unique = unique[n:]
	}
	return out
}

// gather asks the upstream contexts about r at instant at. A context SUP has nothing to ask is not
// asked, so a command without sources needs no EVD read permission.
func (s Service) gather(ctx context.Context, r refs, at time.Time) (facts, error) {
	f := facts{at: at, parties: map[string]ptyapi.PartyRef{}, rfqs: map[string]inqapi.ResolvedRfq{},
		specs: map[catapi.SpecRef]catapi.SpecStanding{}, codes: map[catapi.ItemCode]catapi.ResolvedItemCode{},
		configs: map[polapi.ConfigRef]polapi.ResolvedConfig{}, roles: map[ptyapi.ServiceRoleRef]ptyapi.ResolvedServiceRole{},
		sources: map[string]evdapi.SourceRef{}}
	u := s.Upstream
	for _, ids := range batches(r.parties) {
		got, err := u.Parties.ResolveParties(ctx, ptyapi.Resolve{PartyIDs: ids})
		if err != nil {
			return f, err
		}
		for id, p := range got.Parties {
			f.parties[id] = p
		}
	}
	for _, roles := range batches(r.roles) {
		got, err := u.Parties.ResolveServiceRoles(ctx, roles, at)
		if err != nil {
			return f, err
		}
		for k, v := range got {
			f.roles[k] = v
		}
	}
	for _, ids := range batches(r.rfqs) {
		got, err := u.Rfqs.ResolveRfqs(ctx, ids)
		if err != nil {
			return f, err
		}
		for k, v := range got {
			f.rfqs[k] = v
		}
	}
	for _, specs := range batches(r.specs) {
		got, err := u.Skus.ResolveSpecs(ctx, specs)
		if err != nil {
			return f, err
		}
		for k, v := range got {
			f.specs[k] = v
		}
	}
	for _, codes := range batches(r.codes) {
		got, err := u.Skus.ResolveItemCodes(ctx, codes)
		if err != nil {
			return f, err
		}
		for k, v := range got {
			f.codes[k] = v
		}
	}
	for _, configs := range batches(r.configs) {
		got, err := u.Configs.ResolveConfigs(ctx, configs, at)
		if err != nil {
			return f, err
		}
		for k, v := range got {
			f.configs[k] = v
		}
	}
	for _, ids := range batches(r.sources) {
		got, err := u.Sources.ResolveSources(ctx, ids)
		if err != nil {
			return f, err
		}
		for k, v := range got {
			f.sources[k] = v
		}
	}
	return f, nil
}
