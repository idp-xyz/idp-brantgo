package api

import (
	"strings"
	"testing"
	"time"
)

// The sales price list and the target margin (S4 unit G4a; owner decision 2026-11-18): the contract
// of the kind sales_price, of a pricing policy's target_margin, and of the lookup of the list.
func TestG4ASalesPriceAndTargetMarginContracts(t *testing.T) {
	for _, c := range []struct {
		kind string
		edit func(*ConfigData)
		want string
	}{
		{KindSalesPrice, func(*ConfigData) {}, "OK"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.PartyID = "customer.1" }, "OK"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.UnitPrice, d.SalesPrice.PriceBasisQuantity = "4.93", "1000" }, "OK"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.UnitPrice = "0.00493123" }, "OK"},
		// A price of zero is refused by POL's rule (INVALID_KNOWN_VALUE); the contract checks the form.
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.UnitPrice = "0" }, "OK"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.SkuID = "" }, "SCHEMA_INVALID /args/data/sku_id"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.SkuID = "ST01 red" }, "SCHEMA_INVALID /args/data/sku_id"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.PartyID = "customer 1" }, "SCHEMA_INVALID /args/data/party_id"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.Currency = "usd" }, "SCHEMA_INVALID /args/data/currency"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.Unit = "pcs" }, "SCHEMA_INVALID /args/data/unit"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.UnitPrice = "8.000000001" }, "SCHEMA_INVALID /args/data/unit_price"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.UnitPrice = "8e0" }, "SCHEMA_INVALID /args/data/unit_price"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.UnitPrice = "-8" }, "SCHEMA_INVALID /args/data/unit_price"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.UnitPrice = "" }, "SCHEMA_INVALID /args/data/unit_price"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.PriceBasisQuantity = "1.0000001" }, "SCHEMA_INVALID /args/data/price_basis_quantity"},
		{KindSalesPrice, func(d *ConfigData) { d.SalesPrice.PriceBasisQuantity = "" }, "SCHEMA_INVALID /args/data/price_basis_quantity"},
		{KindPricingPolicy, func(d *ConfigData) { d.PricingPolicy.TargetMargin = "" }, "OK"},
		{KindPricingPolicy, func(d *ConfigData) { d.PricingPolicy.TargetMargin = "0.3125" }, "OK"},
		// Below the minimum margin or 1 and above is POL's rule (INVALID_KNOWN_VALUE).
		{KindPricingPolicy, func(d *ConfigData) { d.PricingPolicy.TargetMargin = "0.1" }, "OK"},
		{KindPricingPolicy, func(d *ConfigData) { d.PricingPolicy.TargetMargin = "31.25%" }, "SCHEMA_INVALID /args/data/target_margin"},
		{KindPricingPolicy, func(d *ConfigData) { d.PricingPolicy.TargetMargin = ".3125" }, "SCHEMA_INVALID /args/data/target_margin"},
		{KindPricingPolicy, func(d *ConfigData) { d.PricingPolicy.TargetMargin = "0.3125000000001" }, "SCHEMA_INVALID /args/data/target_margin"},
	} {
		d := valid()[c.kind]
		c.edit(&d)
		if got := code(save(c.kind, d).Validate()); got != c.want {
			t.Errorf("%s %+v%+v: %s, want %s", c.kind, d.SalesPrice, d.PricingPolicy, got, c.want)
		}
	}
	if !KindKnown(KindSalesPrice) || !KindImplemented(KindSalesPrice) || len(kinds) != 23 || kinds[22] != KindSalesPrice {
		t.Fatal("sales_price is the twenty-third kind")
	}
	// The role templates name no publisher of prices: the rule of the general kinds applies (S5R-D30).
	if PublishCapability(KindSalesPrice) != PermissionAdministerConfigs {
		t.Fatal(PublishCapability(KindSalesPrice))
	}
	// A price is a reference like every configuration revision.
	if err := (ConfigRef{Kind: KindSalesPrice, ConfigID: "price.red.S", Revision: 2}).Validate("/ref"); err != nil {
		t.Fatal(err)
	}
}

func TestG4ASalesPriceQuery(t *testing.T) {
	at := time.Date(2026, 11, 18, 5, 0, 0, 0, time.UTC)
	q := SalesPriceQuery{SkuIDs: []string{"ST01.red.S", "ST01.red.M"}, PartyID: "customer.1", Currency: "USD", At: at}
	many := make([]string, MaxRefs+1)
	for i := range many {
		many[i] = "sku." + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26%26))
	}
	for _, c := range []struct {
		edit func(*SalesPriceQuery)
		want string
	}{
		{func(*SalesPriceQuery) {}, "OK"},
		{func(q *SalesPriceQuery) { q.PartyID = "" }, "OK"},
		{func(q *SalesPriceQuery) { q.SkuIDs = nil }, "SCHEMA_INVALID /sku_ids"},
		{func(q *SalesPriceQuery) { q.SkuIDs = many }, "SCHEMA_INVALID /sku_ids"},
		{func(q *SalesPriceQuery) { q.SkuIDs = []string{"a", "a"} }, "SCHEMA_INVALID /sku_ids/1"},
		{func(q *SalesPriceQuery) { q.SkuIDs = []string{"a", "b c"} }, "SCHEMA_INVALID /sku_ids/1"},
		{func(q *SalesPriceQuery) { q.PartyID = "customer 1" }, "SCHEMA_INVALID /party_id"},
		{func(q *SalesPriceQuery) { q.Currency = "US" }, "SCHEMA_INVALID /currency"},
		{func(q *SalesPriceQuery) { q.At = time.Time{} }, "SCHEMA_INVALID /at"},
	} {
		v := q
		c.edit(&v)
		if got := code(v.Validate()); got != c.want {
			t.Errorf("%+v: %s, want %s", v, got, c.want)
		}
	}
}
