package application

import (
	"reflect"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
)

// S4 unit G4a (owner decision 2026-11-18): a save that states no target margin, and a save of any
// other kind, has the request hash it had before the target margin and the kind sales_price existed,
// so a stored command replays after the upgrade instead of conflicting. The hashes were computed by
// the code of 3185097 (the base of G4a; scratchpad/s4g4a/golden_base.txt) and are pinned here;
// SaveDraft hashes the command without its idempotency key and expected version.
func TestG4ASaveRequestsKeepTheirHash(t *testing.T) {
	tax := api.ConfigRef{Kind: "tax_profile", ConfigID: "tax.zero", Revision: 1}
	policy := api.SaveDraft{IdempotencyKey: "k", Kind: api.KindPricingPolicy, ConfigID: "policy.1", ExpectedVersion: 3, ValidFrom: "2026-11-18T00:00:00Z",
		Data: api.ConfigData{PricingPolicy: &api.PricingPolicy{Name: "synthetic BRC-001 policy", CalculationCurrency: "USD", MinimumMargin: "0.25",
			QuoteValidDays: 7, MaxFXAgeSeconds: 3600, TaxProfile: tax}}}
	currency := api.SaveDraft{IdempotencyKey: "k", Kind: api.KindCurrency, ConfigID: "USD", ValidFrom: "2026-11-18T00:00:00Z",
		Data: api.ConfigData{Currency: &api.Currency{Code: "USD", Name: "US Dollar", MinorUnits: 2, Rounding: api.RoundingHalfUp}}}
	application := api.SaveDraft{IdempotencyKey: "k", Kind: api.KindTaxApplication, ConfigID: "sales.sku", ValidFrom: "2026-11-18T00:00:00Z",
		ValidUntil: "2027-11-18T00:00:00Z", Data: api.ConfigData{TaxApplication: &api.TaxApplication{Name: "sales", Side: "sales", SubjectKind: "sku",
			SubjectID: "sku", TaxProfile: tax, Jurisdiction: "CN", BasisNote: "synthetic"}}}
	hash := func(cmd api.SaveDraft) string {
		args := cmd
		args.IdempotencyKey, args.ExpectedVersion = "", 0
		h, err := action.RequestHash(args)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, c := range []struct {
		cmd  api.SaveDraft
		want string
	}{
		{policy, "b62890a32aec0a845f6dd39538d1d7e0b3963a40a03158ce4f52d5afed3955a7"},
		{currency, "f864bcbc82f1c1f69cd5f1e6cb72f9b9d86e7cec3f7bde34a17721ae53d7885c"},
		{application, "e6df750d03adf73185aba487afcfa4745680a0eb17469caebf0c3a55d4ac6537"},
	} {
		if got := hash(c.cmd); got != c.want {
			t.Errorf("%s: %s, want %s", c.cmd.Kind, got, c.want)
		}
	}
	// A stated target margin is a different request, and so are two different prices.
	policy.Data.PricingPolicy.TargetMargin = "0.3125"
	if hash(policy) == "b62890a32aec0a845f6dd39538d1d7e0b3963a40a03158ce4f52d5afed3955a7" {
		t.Fatal("a stated target margin changes the request")
	}
	price := api.SaveDraft{IdempotencyKey: "k", Kind: api.KindSalesPrice, ConfigID: "price.red.S", ValidFrom: "2026-11-18T00:00:00Z",
		Data: api.ConfigData{SalesPrice: &api.SalesPrice{SkuID: "ST01.red.S", Currency: "USD", Unit: "PCS", UnitPrice: "8.00", PriceBasisQuantity: "1"}}}
	general := hash(price)
	price.Data.SalesPrice.PartyID = "customer.1"
	if own := hash(price); own == general || len(general) != 64 {
		t.Fatal("the customer is part of a price's request")
	}
}

// The target margin and a sales price survive the trip into domain content and back, and no target
// margin is none.
func TestG4AMembersConvertBothWays(t *testing.T) {
	tax := api.ConfigRef{Kind: "tax_profile", ConfigID: "tax", Revision: 1}
	for name, d := range map[string]api.ConfigData{
		"target margin": {PricingPolicy: &api.PricingPolicy{Name: "n", CalculationCurrency: "USD", MinimumMargin: "0.25", QuoteValidDays: 7,
			MaxFXAgeSeconds: 3600, TaxProfile: tax, TargetMargin: "0.3125"}},
		"no target margin": {PricingPolicy: &api.PricingPolicy{Name: "n", CalculationCurrency: "USD", MinimumMargin: "0.25", QuoteValidDays: 7,
			MaxFXAgeSeconds: 3600, TaxProfile: tax}},
		"general price":    {SalesPrice: &api.SalesPrice{SkuID: "ST01.red.S", Currency: "USD", Unit: "PCS", UnitPrice: "8.00", PriceBasisQuantity: "1"}},
		"customer's price": {SalesPrice: &api.SalesPrice{SkuID: "ST01.red.S", PartyID: "customer.1", Currency: "USD", Unit: "CTN", UnitPrice: "90", PriceBasisQuantity: "1"}},
	} {
		kind := api.KindPricingPolicy
		if d.SalesPrice != nil {
			kind = api.KindSalesPrice
		}
		c := content(api.SaveDraft{Kind: kind, ValidFrom: "2026-11-18T00:00:00Z", Data: d})
		if back := data(c); !reflect.DeepEqual(back, d) {
			t.Errorf("%s: %+v back as %+v", name, d, back)
		}
		if c.Data() == nil {
			t.Errorf("%s has no domain data", name)
		}
	}
}

// The role templates (roles_gen.go) name no publisher of prices, so a sales price is published
// under the rule of the general kinds (S5R-D30): config.publish with config.retire, which the
// administrator template alone holds; a draft is saved by the templates that hold config.save_draft.
func TestG4ASalesPricesArePublishedByTheConfigurationAdministrator(t *testing.T) {
	holders := func(permission string) []string {
		var out []string
		for _, tpl := range authorization.Templates() {
			if slices.Contains(tpl.Permissions, permission) {
				out = append(out, tpl.Code)
			}
		}
		return out
	}
	if got := holders(api.PublishCapability(api.KindSalesPrice)); !slices.Equal(got, []string{"administrator"}) {
		t.Fatalf("sales prices are published by %v", got)
	}
	if got := holders(api.CommandSaveDraft); !slices.Equal(got, []string{"costing", "tax_specialist", "administrator"}) {
		t.Fatalf("configuration drafts are saved by %v", got)
	}
	for _, tpl := range authorization.Templates() {
		for _, p := range tpl.Permissions {
			if p == "config.publish.sales_price" || p == "config.publish.pricing_policy" {
				t.Fatalf("%s holds %s: the publication rule of prices must follow the templates", tpl.Code, p)
			}
		}
	}
}
