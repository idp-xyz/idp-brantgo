package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/config"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The sales price list (owner decision 2026-11-18; api/sales_price.go).

// singleSalesPrice refuses the publication that made root's working revision active while another
// configuration is the active sales price of the same subject (SKU, customer, currency): a subject
// has one price in force, so a lookup never chooses between two. It runs in POL's transaction after
// root is locked; two publications of one subject under different IDs that race past it meet the
// unique index config_revision_sales_price_subject, and the ledger's second attempt of the loser is
// refused here. The other configuration is retired first, or the price is revised there.
func singleSalesPrice(ctx context.Context, exec bentopg.Executor, root *config.Root) error {
	if root.Kind != config.KindSalesPrice || root.Active == nil {
		return nil
	}
	sku, party, currency := root.Active.Content.SalesPrice.Subject()
	holder, err := postgres.SalesPriceHolder(ctx, exec, scope(ctx), sku, party, currency, root.ID)
	if err != nil || holder == "" {
		return err
	}
	return kernel.RejectAs(api.CodeBusinessKeyConflict, kernel.CategoryConflict, "/args/id",
		"Sales price "+holder+" is in force for this SKU, customer and currency; retire it or revise the price there")
}

// LookupSalesPrices answers the sales prices in force at an instant for SKUs, a currency and a
// customer (api.SalesPriceQueries), in a transaction of POL's own without locks. A price whose unit
// is not in force at the instant is left out: a price is used with its unit.
func (s Service) LookupSalesPrices(ctx context.Context, q api.SalesPriceQuery) (api.SalesPrices, error) {
	if err := q.Validate(); err != nil {
		return api.SalesPrices{}, err
	}
	at := kernel.Instant(q.At)
	var out api.SalesPrices
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		cur, err := inForce(ctx, exec, config.Target{Kind: config.KindCurrency, ID: q.Currency}, at, "/currency")
		if err != nil {
			return err
		}
		if out.Currency, err = kernel.NewCurrency(cur.Content.Currency.Code, cur.Content.Currency.MinorUnits); err != nil {
			return err
		}
		stored, err := postgres.SalesPricesInForce(ctx, exec, scope(ctx), q.SkuIDs, q.PartyID, q.Currency, at)
		if err != nil {
			return err
		}
		units := map[string]bool{}
		for _, st := range stored {
			p := st.Rev.Content.SalesPrice
			if p == nil || config.InForce(st.Rev, at) != nil {
				continue
			}
			usable, asked := units[p.Unit]
			if !asked {
				_, unitErr := inForce(ctx, exec, config.Target{Kind: config.KindUnit, ID: p.Unit}, at, "/")
				if _, refused := kernel.AsRejection(unitErr); unitErr != nil && !refused {
					return unitErr
				}
				usable = unitErr == nil
				units[p.Unit] = usable
			}
			if !usable {
				continue
			}
			price, err := kernel.ParseDecimal(p.UnitPrice, kernel.PriceFormat)
			if err != nil {
				return err
			}
			basis, err := kernel.ParseDecimal(p.PriceBasisQuantity, kernel.QuantityFormat)
			if err != nil {
				return err
			}
			out.Prices = append(out.Prices, api.SalesPriceInForce{
				Ref:         api.ConfigRef{Kind: api.KindSalesPrice, ConfigID: st.Key.ConfigID, Revision: st.Key.Revision},
				ContentHash: st.Rev.Hash, SkuID: p.SkuID, PartyID: p.PartyID, Unit: p.Unit, UnitPrice: price, PriceBasisQuantity: basis,
				ValidFrom: st.Rev.Content.ValidFrom, ValidUntil: st.Rev.Content.ValidUntil})
		}
		return nil
	})
	if err != nil {
		return api.SalesPrices{}, err
	}
	return out, nil
}
