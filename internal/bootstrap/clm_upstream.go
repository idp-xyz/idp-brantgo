package bootstrap

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/clm"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// clmEvidence resolves the EVD sources a claim cites. CLM has no Context Map edge to EVD, so the
// adapter stays in the composition root and CLM's port does not import EVD.
type clmEvidence struct {
	Sources evdapi.Queries
}

var _ clm.Evidence = clmEvidence{}

func (e clmEvidence) Resolve(ctx context.Context, ids []string) (map[string]string, error) {
	refs, err := e.Sources.ResolveSources(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(refs))
	for id, ref := range refs {
		out[id] = ref.ContentSHA256
	}
	return out, nil
}

// clmCurrency reads the minor units of the currency revision a shipment line names. POL is not an
// upstream CLM may import.
type clmCurrency struct {
	Configs polapi.Queries
}

var _ clm.Currencies = clmCurrency{}

func (c clmCurrency) MinorUnits(ctx context.Context, code string, revision int64, at time.Time) (int, error) {
	ref := polapi.ConfigRef{Kind: polapi.KindCurrency, ConfigID: code, Revision: revision}
	got, err := c.Configs.ResolveConfigs(ctx, []polapi.ConfigRef{ref}, at)
	if err != nil {
		return 0, err
	}
	resolved, ok := got[ref]
	if !ok || resolved.Data.Currency == nil {
		return 0, kernel.Reject(kernel.CodeReferenceInvalid, "/args/shipment_line_id", "The shipment currency is not known")
	}
	if err = resolved.Check("/args/shipment_line_id"); err != nil {
		return 0, err
	}
	units := resolved.Data.Currency.MinorUnits
	if units < 0 || units > 4 || resolved.Data.Currency.Code != code {
		return 0, kernel.Reject(kernel.CodeReferenceInvalid, "/args/shipment_line_id", "The shipment currency is not usable")
	}
	return units, nil
}
