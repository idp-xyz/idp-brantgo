//go:build integration

package bootstrap_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The price list is read by its content (polapi.SalesPriceQueries): for SKUs, a currency and a
// customer, the general price of each SKU and the customer's own, in force at the instant — and a
// price is a configuration revision like every other: ConfigRef v1 resolves it, ConfigGuard v1
// holds it in a downstream transaction and ConfigAt v1 answers the one valid at an instant (S4 unit
// G4a; owner decision 2026-11-18).
func TestG4ASalesPriceLookupGuardAndConfigAt(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.priceWorld(editor, reviewer, win)
	const kind = polapi.KindSalesPrice
	before := time.Now()
	general := w.configure(editor, reviewer, kind, "price.red.S", 0, win.from, time.Time{}, salesPriceData("ST01.red.S", "", "USD", "PCS", "8.40", "1"), true)
	own := w.configure(editor, reviewer, kind, "price.red.S.c1", 0, win.from, time.Time{}, salesPriceData("ST01.red.S", "customer.1", "USD", "PCS", "7.90", "1"), true)
	w.configure(editor, reviewer, kind, "price.red.S.c2", 0, win.from, time.Time{}, salesPriceData("ST01.red.S", "customer.2", "USD", "PCS", "1.00", "1"), true)
	eur := w.configure(editor, reviewer, kind, "price.red.S.eur", 0, win.from, time.Time{}, salesPriceData("ST01.red.S", "", "EUR", "PCS", "7.70", "1"), true)
	carton := w.configure(editor, reviewer, kind, "price.red.M.ctn", 0, win.from, time.Time{}, salesPriceData("ST01.red.M", "", "USD", "CTN", "493", "100"), true)
	// Not in force: a draft, and a price whose validity has not begun.
	_, err := w.saveConfig(editor, kind, "price.blue.S", 0, win.from, time.Time{}, salesPriceData("ST01.blue.S", "", "USD", "PCS", "8.00", "1"))
	w.must(err)

	lookup := func(party, currency string, at time.Time, skus ...string) []string {
		t.Helper()
		got, err := w.c.POL.LookupSalesPrices(editor, polapi.SalesPriceQuery{SkuIDs: skus, PartyID: party, Currency: currency, At: at})
		w.must(err)
		if got.Currency.Code() != currency || got.Currency.MinorUnits() != 2 {
			t.Fatalf("currency %v", got.Currency)
		}
		var out []string
		for _, p := range got.Prices {
			out = append(out, fmt.Sprintf("%s@%d %s/%s %s per %s %s", p.Ref.ConfigID, p.Ref.Revision, p.SkuID, p.PartyID, p.UnitPrice, p.PriceBasisQuantity, p.Unit))
			if p.Ref.Kind != kind || len(p.ContentHash) != 64 || !p.ValidFrom.Equal(win.from) || !p.ValidUntil.IsZero() {
				t.Fatalf("%+v", p)
			}
		}
		return out
	}
	want := func(got []string, lines ...string) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(lines) {
			t.Fatalf("got %v, want %v", got, lines)
		}
	}
	now := time.Now()
	want(lookup("customer.1", "USD", now, "ST01.red.S", "ST01.red.M", "ST01.blue.S"),
		"price.red.M.ctn@1 ST01.red.M/ 493 per 100 CTN", "price.red.S@1 ST01.red.S/ 8.4 per 1 PCS", "price.red.S.c1@1 ST01.red.S/customer.1 7.9 per 1 PCS")
	want(lookup("", "USD", now, "ST01.red.S"), "price.red.S@1 ST01.red.S/ 8.4 per 1 PCS")
	want(lookup("customer.3", "USD", now, "ST01.red.S"), "price.red.S@1 ST01.red.S/ 8.4 per 1 PCS")
	want(lookup("customer.1", "EUR", now, "ST01.red.S", "ST01.red.M"), "price.red.S.eur@1 ST01.red.S/ 7.7 per 1 PCS")
	want(lookup("customer.1", "USD", now, "ST01.blue.S"))
	// Before they were published none was in force.
	want(lookup("customer.1", "USD", before, "ST01.red.S"))
	// A currency POL does not know is the query's refusal; a malformed query is SCHEMA_INVALID.
	if _, err = w.c.POL.LookupSalesPrices(editor, polapi.SalesPriceQuery{SkuIDs: []string{"ST01.red.S"}, Currency: "JPY", At: now}); code(err) != kernel.CodeReferenceInvalid || path(err) != "/currency" {
		t.Fatalf("an unknown currency: %v", err)
	}
	if _, err = w.c.POL.LookupSalesPrices(editor, polapi.SalesPriceQuery{Currency: "USD", At: now}); code(err) != kernel.CodeSchemaInvalid || path(err) != "/sku_ids" {
		t.Fatalf("no SKU: %v", err)
	}
	// Another company has no price list of this one.
	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: actorRef("human", "reader.B"), Permissions: allPermissions,
		DataScope: "company", Team: "trade"})
	if _, err = w.c.POL.LookupSalesPrices(other, polapi.SalesPriceQuery{SkuIDs: []string{"ST01.red.S"}, Currency: "USD", At: now}); code(err) != kernel.CodeReferenceInvalid {
		t.Fatalf("company.B: %v", err)
	}

	// ConfigRef v1, ConfigGuard v1 and ConfigAt v1 serve the kind unchanged.
	resolved, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{general, own, eur}, now)
	w.must(err)
	if r := resolved[own]; r.Code != "" || r.Check("/x") != nil || r.Data.SalesPrice == nil || r.Data.SalesPrice.PartyID != "customer.1" ||
		r.Data.SalesPrice.UnitPrice != "7.90" {
		t.Fatalf("%+v", r)
	}
	d := newDownstream(w)
	if v, err := d.guard(d.scope, general, now); err != nil || !v.InForce() || v.ContentHash != resolved[general].ContentHash {
		t.Fatalf("%+v %v", v, err)
	}
	at, err := w.c.POL.ConfigAt(editor, polapi.ConfigAtQuery{Kind: kind, ConfigID: "price.red.S", At: now})
	if err != nil || at.Code != "" || at.Ref != general || at.Data.SalesPrice.UnitPrice != "8.40" {
		t.Fatalf("%+v %v", at, err)
	}

	// The next revision of the general price supersedes the first: the lookup and the guard follow.
	next := w.configure(editor, reviewer, kind, "price.red.S", 2, win.from, time.Time{}, salesPriceData("ST01.red.S", "", "USD", "PCS", "8.60", "1"), true)
	later := time.Now()
	want(lookup("", "USD", later, "ST01.red.S"), "price.red.S@2 ST01.red.S/ 8.6 per 1 PCS")
	want(lookup("", "USD", now, "ST01.red.S"), "price.red.S@1 ST01.red.S/ 8.4 per 1 PCS") // as it was in force then
	if v, err := d.guard(d.scope, general, later); err != nil || v.InForce() || v.Code != polapi.CodeConfigNotActive {
		t.Fatalf("the superseded price: %+v %v", v, err)
	}
	if v, err := d.guard(d.scope, next, later); err != nil || !v.InForce() {
		t.Fatalf("the price in force: %+v %v", v, err)
	}
	// A retired price is out of the list; a price whose unit is no longer in force is left out.
	_, err = w.c.POL.Retire(reviewer, polapi.Retire{IdempotencyKey: w.key(), Kind: kind, ConfigID: "price.red.S.c1", ExpectedVersion: 2, Reason: "客户价停用（合成）"})
	w.must(err)
	want(lookup("customer.1", "USD", time.Now(), "ST01.red.S", "ST01.red.M"),
		"price.red.M.ctn@1 ST01.red.M/ 493 per 100 CTN", "price.red.S@2 ST01.red.S/ 8.6 per 1 PCS")
	_, err = w.c.POL.Retire(reviewer, polapi.Retire{IdempotencyKey: w.key(), Kind: polapi.KindUnit, ConfigID: "CTN", ExpectedVersion: 2, Reason: "单位停用（合成）"})
	w.must(err)
	want(lookup("customer.1", "USD", time.Now(), "ST01.red.S", "ST01.red.M"), "price.red.S@2 ST01.red.S/ 8.6 per 1 PCS")
	if v, err := d.guard(d.scope, carton, time.Now()); err != nil || !v.InForce() {
		t.Fatalf("the carton price itself stays published: %+v %v", v, err)
	}
}

// A pricing policy stored before the target margin existed keeps its stored data and its content
// hash on PostgreSQL (the golden vector of the code of 3185097, pinned in the domain test
// TestG4APricingPoliciesStoredBeforeTheTargetMarginKeepTheirHash); a target margin is content of the
// next revision, at least the minimum margin and below 1, and reads back through every read.
func TestG4APricingPolicyTargetMargin(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.currency(editor, reviewer, "USD", win)
	tax := w.tax(editor, reviewer, polapi.RecoveryNonrecoverable, "tax.zero", win)
	from := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	policy := func(target string) polapi.ConfigData {
		return polapi.ConfigData{PricingPolicy: &polapi.PricingPolicy{Name: "synthetic BRC-001 policy", CalculationCurrency: "USD", MinimumMargin: "0.25",
			QuoteValidDays: 7, MaxFXAgeSeconds: 3600, TaxProfile: tax, TargetMargin: target}}
	}
	first := w.configure(editor, reviewer, polapi.KindPricingPolicy, "policy.1", 0, from, time.Time{}, policy(""), true)
	v, err := w.c.POL.GetConfig(editor, polapi.KindPricingPolicy, "policy.1")
	w.must(err)
	if r := v.Revisions[0]; r.ContentHash != "fcd89eff609505943a06c60b71d8be036664ee0545fe177c7d2a43cc99f2d070" || r.Data.PricingPolicy.TargetMargin != "" {
		t.Fatalf("a policy without a target margin: %+v", r)
	}
	// The stored data has no target_margin member.
	var stored string
	db, done := w.helperDB()
	defer done()
	w.must(bento.ScopeTransactor{DB: db, Scope: w.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return exec.QueryRow(ctx, `SELECT data::text FROM brant_pol.config_revision WHERE kind = 'pricing_policy' AND revision = 1`).Scan(&stored)
	}))
	if !strings.Contains(stored, `"minimum_margin"`) || strings.Contains(stored, "target_margin") {
		t.Fatalf("stored %s", stored)
	}

	for _, c := range []struct{ target, code, path string }{
		{"0.2", kernel.CodeInvalidKnownValue, "/args/data/target_margin"},
		{"1", kernel.CodeInvalidKnownValue, "/args/data/target_margin"},
		{"31.25%", kernel.CodeSchemaInvalid, "/args/data/target_margin"},
	} {
		if _, err = w.saveConfig(editor, polapi.KindPricingPolicy, "policy.1", 2, from, time.Time{}, policy(c.target)); code(err) != c.code || path(err) != c.path {
			t.Fatalf("target %s: %v", c.target, err)
		}
	}
	second := w.configure(editor, reviewer, polapi.KindPricingPolicy, "policy.1", 2, from, time.Time{}, policy("0.3125"), true)
	now := time.Now()
	resolved, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{first, second}, now)
	w.must(err)
	if r := resolved[second]; r.Code != "" || r.Data.PricingPolicy.TargetMargin != "0.3125" || r.Data.PricingPolicy.MinimumMargin != "0.25" ||
		r.ContentHash != "028cbf02f6e93bb9847aeb14a37729c0a0b059c928f735ec9d0cb8997bbc1d45" {
		t.Fatalf("%+v", r)
	}
	if r := resolved[first]; r.Code != polapi.CodeConfigNotActive || r.Data.PricingPolicy.TargetMargin != "" ||
		r.ContentHash != "fcd89eff609505943a06c60b71d8be036664ee0545fe177c7d2a43cc99f2d070" {
		t.Fatalf("the superseded revision: %+v", r)
	}
	at, err := w.c.POL.ConfigAt(editor, polapi.ConfigAtQuery{Kind: polapi.KindPricingPolicy, ConfigID: "policy.1", At: now})
	if err != nil || at.Ref != second || at.Data.PricingPolicy.TargetMargin != "0.3125" {
		t.Fatalf("%+v %v", at, err)
	}
}
