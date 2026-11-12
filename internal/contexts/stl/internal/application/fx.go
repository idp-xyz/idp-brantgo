package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Cross-currency applications (S5 unit C8, STL-INV-007; Context Map R33 guard lock; MD-10). Before the
// transaction application.apply asks POL for the settlement rate it names (LookupFX: exactly that direction
// and purpose, in force — never the inverse or a cross rate) and TRD for the order's analysis rate; inside it,
// after STL's roots and the authority guard, POL's ConfigGuard v1 re-checks the rate revision and holds it FOR
// SHARE until STL commits.

// settlementRate is what application.apply read of the rate it names: POL's answer, or POL's refusal, which
// Apply returns in its place among the refusals (Prepare only fetches).
type settlementRate struct {
	rate    *polapi.ExchangeRate
	refusal error
}

// fundCurrency returns the currency code of fund record id, "" when STL has none in the caller's scope.
func (s Service) fundCurrency(ctx context.Context, id string) (string, error) {
	var code string
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		f, err := postgres.LoadFund(ctx, exec, scope(ctx), id, false)
		if err == nil && f != nil {
			code = f.Currency().Code()
		}
		return err
	})
	return code, err
}

// lookupRate asks POL for fx_rate ref converting from into to for settlement now. POL's refusals are kept for
// Apply, at /args/fx_rate.
func (s Service) lookupRate(ctx context.Context, ref *api.ConfigRef, from, to string) (settlementRate, error) {
	if ref == nil || from == "" || to == "" {
		return settlementRate{}, nil
	}
	r := polRef(*ref)
	x, err := s.Upstream.Configs.LookupFX(ctx, polapi.FXQuery{FromCurrency: from, ToCurrency: to, Purpose: polapi.PurposeSettlement,
		At: s.Clock.Now(), Rate: &r})
	if rej, ok := kernel.AsRejection(err); ok {
		reason := rej.Code
		if len(rej.Fields) > 0 {
			reason = rej.Fields[0].Reason
		}
		return settlementRate{refusal: kernel.RejectAs(rej.Code, rej.Category, "/args/fx_rate", reason)}, nil
	}
	if err != nil {
		return settlementRate{}, err
	}
	return settlementRate{rate: &x}, nil
}

// input re-checks the rate with POL's ConfigGuard v1 in STL's transaction (R33 guard lock) and returns it
// for the rules with the order's analysis basis; nil without a rate.
func (r settlementRate) input(ctx context.Context, exec bentopg.Executor, at time.Time, basis *fa.AnalysisBasis) (*fa.FXInput, error) {
	if r.refusal != nil {
		return nil, r.refusal
	}
	if r.rate == nil {
		return nil, nil
	}
	if err := guardConfig(ctx, exec, r.rate.Rate, r.rate.ContentHash, at, "/args/fx_rate"); err != nil {
		return nil, err
	}
	return &fa.FXInput{Rate: r.rate.Value, Use: fa.RateUse{ID: r.rate.Rate.ConfigID, Revision: r.rate.Rate.Revision, Hash: r.rate.ContentHash},
		Basis: basis}, nil
}

// basisOf is order o's analysis basis for a conversion from source into target (the order's currency):
// known to STL when the analysis currency is one of the two (a rate of exactly 1 when it is the target's);
// nil otherwise, or when TRD states none.
func basisOf(o *Order, source, target kernel.Currency) *fa.AnalysisBasis {
	if o == nil || o.Analysis == nil || o.Analysis.OrderCurrency != target.Code() {
		return nil
	}
	a := o.Analysis
	b := &fa.AnalysisBasis{OrderCurrency: a.OrderCurrency, Rate: kernel.DecimalFromInt(1)}
	switch a.Currency {
	case target.Code():
		b.Currency = target
		return b
	case source.Code():
		b.Currency = source
	default:
		return nil
	}
	rate, err := kernel.ParseDecimal(a.Rate, kernel.FactorFormat)
	if err != nil || rate.Sign() <= 0 {
		return nil
	}
	b.Rate = rate
	return b
}

func conversionView(c *fa.Conversion) *api.ConversionView {
	if c == nil {
		return nil
	}
	v := &api.ConversionView{SourceCurrency: c.Source.Currency().Code(), SourceAmountMinor: c.Source.MinorString(),
		FXRate: api.ConfigRef{Kind: api.KindFXRate, ConfigID: c.Rate.ID, Revision: c.Rate.Revision}, FXRateHash: c.Rate.Hash, Rate: c.Value.String()}
	if a := c.Analysis; a != nil {
		v.AnalysisCurrency, v.AnalysisRate = a.Currency.Code(), a.Rate.String()
		if a.Difference != nil {
			v.FXDifferenceMinor = a.Difference.MinorString()
		}
	}
	return v
}
