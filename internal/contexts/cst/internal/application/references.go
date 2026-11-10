package application

import (
	"context"
	"maps"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/costing"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
)

// facts asks INQ, CAT and POL about the references of d, each in a transaction of its own, before
// CST's transaction (Context Map R08, R51, R10). POL answers at the command's instant. The
// specification standings are asked for verification only: a draft may name a revision that is no
// longer current.
func (s Service) facts(ctx context.Context, d costing.Data, verify bool) (costing.Facts, error) {
	f := costing.Facts{Skus: map[string]bool{}, Specs: map[costing.Spec]func(string) error{}, Configs: map[costing.Ref]costing.Config{}}
	if d.Rfq != nil {
		rfqs, err := s.Upstream.Rfqs.ResolveRfqs(ctx, []string{d.Rfq.RfqID})
		if err != nil {
			return f, err
		}
		if r, ok := rfqs[d.Rfq.RfqID]; ok {
			f.Rfq = &costing.Rfq{RfqID: r.RfqID, RowVersion: r.RowVersion, Ready: r.Ready(), LinesHash: r.LinesHash}
			for _, l := range r.Lines {
				f.Rfq.Lines = append(f.Rfq.Lines, costing.RfqLine{LineID: l.LineID, SkuID: l.SkuID, SkuSpecRevision: l.SkuSpecRevision,
					Quantity: l.Quantity, UOM: l.UOM})
			}
		}
	}
	var skus []string
	var specs []catapi.SpecRef
	for _, l := range d.Lines {
		skus = append(skus, l.SkuID)
		specs = append(specs, catapi.SpecRef{SkuID: l.SkuID, SpecRevision: l.SkuSpecRevision})
	}
	if len(skus) > 0 {
		known, err := s.Upstream.Skus.ResolveSkus(ctx, skus)
		if err != nil {
			return f, err
		}
		for id := range known {
			f.Skus[id] = true
		}
		if verify {
			standings, err := s.Upstream.Skus.ResolveSpecs(ctx, specs)
			if err != nil {
				return f, err
			}
			for ref, standing := range standings {
				f.Specs[costing.Spec{SkuID: ref.SkuID, Revision: ref.SpecRevision}] = standing.Check
			}
		}
	}
	var refs []polapi.ConfigRef
	for _, r := range d.ConfigRefs() {
		refs = append(refs, polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ID, Revision: r.Revision})
	}
	if len(refs) == 0 {
		return f, nil
	}
	// A reference POL cannot hold (an unknown kind or an over-long ID) is simply unknown.
	var asked []polapi.ConfigRef
	for _, r := range refs {
		if r.Validate("/") == nil {
			asked = append(asked, r)
		}
	}
	resolved := map[polapi.ConfigRef]polapi.ResolvedConfig{}
	now := s.Clock.Now()
	for chunk := range slices.Chunk(asked, polapi.MaxRefs) {
		answered, err := s.Upstream.Configs.ResolveConfigs(ctx, chunk, now)
		if err != nil {
			return f, err
		}
		maps.Copy(resolved, answered)
	}
	for ref, rc := range resolved {
		c := costing.Config{Check: rc.Check, Kind: ref.Kind}
		switch data := rc.Data; {
		case data.Currency != nil:
			c.CurrencyCode = data.Currency.Code
		case data.PricingPolicy != nil:
			c.PolicyCurrency = data.PricingPolicy.CalculationCurrency
		case data.Unit != nil:
			c.Unit = &costing.Unit{Code: data.Unit.Code, Precision: data.Unit.Precision}
		case data.UnitConversion != nil:
			u := data.UnitConversion
			c.Conversion = &costing.Conversion{SkuID: u.SkuID, SpecRevision: u.SpecRevision, From: u.FromUnit, To: u.ToUnit}
		case data.Calendar != nil:
			c.TimeZone = data.Calendar.TimeZone
		}
		f.Configs[costing.Ref{Kind: ref.Kind, ID: ref.ConfigID, Revision: ref.Revision}] = c
	}
	return f, nil
}

// guard re-checks, inside CST's transaction and after the costing context root is locked FOR
// UPDATE, the upstream facts a verification relies on (ADR-BR-004 option B, ADR-BR-011): the RFQ
// binding through INQ's RfqGuard v1, then every POL configuration of d through ConfigGuard v1 at
// instant at, in context code order (INQ before POL) and within POL by kind, ID and revision. Each
// guard locks its upstream row FOR SHARE until CST's transaction ends.
func guard(ctx context.Context, exec bentopg.Executor, d costing.Data, b costing.Binding, at time.Time) error {
	rfq := inqapi.RfqBinding{RfqID: b.RfqID, RowVersion: b.RowVersion, LinesHash: b.LinesHash}
	v, err := inqapi.ScanRfqGuard(rfq, exec.QueryRow(ctx, inqapi.RfqGuardSQL, inqapi.RfqGuardArgs(rfq)...).Scan)
	if err != nil {
		return err
	}
	if err = v.Check("/args/data/rfq_ref"); err != nil {
		return err
	}
	for _, r := range d.ConfigRefs() {
		ref := polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ID, Revision: r.Revision}
		v, err := polapi.ScanConfigGuard(ref, exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(ref, at)...).Scan)
		if err != nil {
			return err
		}
		if err = v.Check(fieldPath(d, r)); err != nil {
			return err
		}
	}
	return nil
}
