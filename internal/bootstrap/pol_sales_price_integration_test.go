//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The sales price list and the target margin of POL (S4 unit G4a; owner decision 2026-11-18: a
// quotation's unit price comes from a price list where one is in force, otherwise from cost ÷ (1 −
// target margin)) against PostgreSQL 16 and migrations/pol/000007. Python V0.50 has neither and the
// design sources do not name them yet, so the tests cite the decision.

func salesPriceData(sku, party, currency, unit, price, basis string) polapi.ConfigData {
	return polapi.ConfigData{SalesPrice: &polapi.SalesPrice{SkuID: sku, PartyID: party, Currency: currency, Unit: unit, UnitPrice: price,
		PriceBasisQuantity: basis}}
}

// priceWorld publishes what a price refers to: USD and EUR, the units PCS and CTN.
func (w *world) priceWorld(editor, reviewer context.Context, win window) {
	w.t.Helper()
	w.currency(editor, reviewer, "USD", win)
	w.currency(editor, reviewer, "EUR", win)
	for _, code := range []string{"PCS", "CTN"} {
		w.configure(editor, reviewer, polapi.KindUnit, code, 0, win.from, time.Time{},
			polapi.ConfigData{Unit: &polapi.Unit{Code: code, Name: code, Dimension: polapi.DimensionCount}}, true)
	}
}

// A sales price has the life cycle of every kind and is published under the rule of the general
// kinds (S5R-D30: config.publish with config.retire, the administrator template; the role templates
// name nobody for prices): a costing clerk drafts it, an administrator other than the editor
// publishes it, a specialist of another kind does not. A subject — SKU, customer, currency — has one
// price in force (BUSINESS_KEY_CONFLICT, and the unique index under two publications that race) and
// a configuration keeps its subject (IMMUTABLE_DEFINITION).
func TestG4ASalesPricePublicationAndSubject(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.priceWorld(editor, reviewer, win)
	costing, admin := w.assign("costing.1", "costing"), w.assign("admin.1", "administrator")
	creditManager, taxSpecialist := w.assign("credit.manager.1", "credit_manager"), w.assign("tax.1", "tax_specialist")
	const kind = polapi.KindSalesPrice

	// The contract's rules, on the stored references.
	for _, c := range []struct {
		data       polapi.ConfigData
		code, path string
	}{
		{salesPriceData("ST01.red.S", "", "USD", "PCS", "0", "1"), kernel.CodeInvalidKnownValue, "/args/data/unit_price"},
		{salesPriceData("ST01.red.S", "", "USD", "PCS", "8.40", "0.5"), kernel.CodeInvalidKnownValue, "/args/data/price_basis_quantity"},
		{salesPriceData("ST01.red.S", "", "JPY", "PCS", "8.40", "1"), kernel.CodeReferenceInvalid, "/args/data/currency"},
		{salesPriceData("ST01.red.S", "", "USD", "BOX", "8.40", "1"), kernel.CodeReferenceInvalid, "/args/data/unit"},
		{salesPriceData("ST01.red.S", "", "USD", "PCS", "8.4000000001", "1"), kernel.CodeSchemaInvalid, "/args/data/unit_price"},
	} {
		if _, err := w.saveConfig(costing, kind, "price.bad", 0, win.from, time.Time{}, c.data); code(err) != c.code || path(err) != c.path {
			t.Fatalf("%+v: %v", *c.data.SalesPrice, err)
		}
	}

	// A costing clerk drafts; the editor does not publish its own draft, a specialist of another
	// kind does not publish prices, the administrator does.
	_, err := w.saveConfig(costing, kind, "price.red.S", 0, win.from, time.Time{}, salesPriceData("ST01.red.S", "", "USD", "PCS", "8.40", "1"))
	w.must(err)
	if _, err = w.publish(costing, kind, "price.red.S", 1, 1); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("a costing clerk publishing: %v", err)
	}
	for who, ctx := range map[string]context.Context{"credit manager": creditManager, "tax specialist": taxSpecialist} {
		if _, err = w.publish(ctx, kind, "price.red.S", 1, 1); code(err) != kernel.CodePermissionDenied || path(err) != "/args/kind" {
			t.Fatalf("a %s publishing a price: %v", who, err)
		}
	}
	r, err := w.publish(admin, kind, "price.red.S", 1, 1)
	if err != nil || r.Status != polapi.StatusActive || r.ObjectID != "sales_price:price.red.S" {
		t.Fatalf("%+v %v", r, err)
	}
	if events := w.events("company.A", polapi.Source); events[len(events)-1].Type != polapi.EventConfigPublished ||
		events[len(events)-1].Payload["kind"] != kind {
		t.Fatalf("%+v", events[len(events)-1])
	}

	// One price in force per subject: another configuration of the same SKU, customer and currency
	// is refused at publication; the customer's own price and the EUR price are other subjects.
	_, err = w.saveConfig(editor, kind, "price.red.S.again", 0, win.from, time.Time{}, salesPriceData("ST01.red.S", "", "USD", "PCS", "8.50", "1"))
	w.must(err)
	if _, err = w.publish(reviewer, kind, "price.red.S.again", 1, 1); code(err) != polapi.CodeBusinessKeyConflict || path(err) != "/args/id" {
		t.Fatalf("a second price of one subject: %v", err)
	}
	w.configure(editor, reviewer, kind, "price.red.S.c1", 0, win.from, time.Time{}, salesPriceData("ST01.red.S", "customer.1", "USD", "PCS", "7.90", "1"), true)
	w.configure(editor, reviewer, kind, "price.red.S.eur", 0, win.from, time.Time{}, salesPriceData("ST01.red.S", "", "EUR", "PCS", "7.70", "1"), true)

	// Two publications of one new subject under two IDs at once: one is in force, the other is
	// refused as a conflict.
	for _, id := range []string{"race.a", "race.b"} {
		_, err = w.saveConfig(editor, kind, id, 0, win.from, time.Time{}, salesPriceData("ST01.blue.S", "", "USD", "PCS", "8.00", "1"))
		w.must(err)
	}
	results := make([]error, 2)
	var wg sync.WaitGroup
	for i, id := range []string{"race.a", "race.b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = w.c.POL.Publish(reviewer, polapi.Publish{IdempotencyKey: "race." + id, Kind: kind, ConfigID: id, ExpectedVersion: 1,
				Revision: 1, EvidenceIDs: []string{"evidence"}, Reason: "并发发布同一价格主题"})
		}()
	}
	wg.Wait()
	codes := []string{code(results[0]), code(results[1])}
	slices.Sort(codes)
	if !slices.Equal(codes, []string{polapi.CodeBusinessKeyConflict, "OK"}) {
		t.Fatalf("racing publications: %v", codes)
	}
	active := 0
	for _, id := range []string{"race.a", "race.b"} {
		v, err := w.c.POL.GetConfig(editor, kind, id)
		w.must(err)
		if v.ActiveRevision == 1 {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("%d prices of ST01.blue.S are active", active)
	}

	// A configuration keeps its subject: its next revision is the same SKU's price, at another
	// amount, and supersedes the first.
	_, err = w.saveConfig(editor, kind, "price.red.S", 2, win.from, time.Time{}, salesPriceData("ST01.red.M", "", "USD", "PCS", "8.60", "1"))
	w.must(err)
	if _, err = w.publish(reviewer, kind, "price.red.S", 3, 2); code(err) != polapi.CodeImmutableDefinition {
		t.Fatalf("another SKU under the same configuration: %v", err)
	}
	_, err = w.saveConfig(editor, kind, "price.red.S", 3, win.from, time.Time{}, salesPriceData("ST01.red.S", "", "USD", "PCS", "8.60", "1"))
	w.must(err)
	_, err = w.publish(reviewer, kind, "price.red.S", 4, 2)
	w.must(err)
	v, err := w.c.POL.GetConfig(costing, kind, "price.red.S")
	w.must(err)
	if v.ActiveRevision != 2 || len(v.Revisions) != 2 || v.Revisions[0].Status != polapi.StatusSuperseded ||
		*v.Revisions[1].Data.SalesPrice != (polapi.SalesPrice{SkuID: "ST01.red.S", Currency: "USD", Unit: "PCS", UnitPrice: "8.60", PriceBasisQuantity: "1"}) ||
		v.Revisions[1].PublishedBy.ID != "reviewer" || v.Revisions[0].PublishedBy.ID != "admin.1" || v.Revisions[0].SavedBy.ID != "costing.1" {
		t.Fatalf("%+v", v)
	}
	// Once the price in force is retired its subject is free for another configuration.
	_, err = w.c.POL.Retire(admin, polapi.Retire{IdempotencyKey: w.key(), Kind: kind, ConfigID: "price.red.S", ExpectedVersion: 5, Reason: "价格停用（合成）"})
	w.must(err)
	_, err = w.publish(reviewer, kind, "price.red.S.again", 1, 1)
	w.must(err)
}
