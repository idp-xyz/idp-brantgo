//go:build integration

package bootstrap_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/plugin"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S7-01: the plugin platform on PostgreSQL 16. A company enables industry.apparel, and apparel
// documents are refused until that activation is active. Two cost providers cannot both be active
// for one document profile. Open apparel documents keep plugin.disable at PLUGIN_IN_USE until they
// close; the rows stay readable afterwards.

func TestPluginEnableListsActiveAndIsolatesCompanies(t *testing.T) {
	w := newWorld(t)
	admin := w.actor("admin.ext", extPerms()...)
	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "admin.other"},
		Permissions: extPerms(), DataScope: "company"})

	_, err := w.c.EXT.Enable(admin, plugin.EnablePlugin{IdempotencyKey: w.key(), PluginID: plugin.ApparelPluginID})
	w.must(err)
	if status := pluginStatus(t, w, admin, plugin.ApparelPluginID); status != plugin.StatusActive {
		t.Fatalf("status %s", status)
	}
	if pluginStatus(t, w, other, plugin.ApparelPluginID) != plugin.StatusDisabled {
		t.Fatal("company.B sees company.A's activation")
	}
	if w.count("company.B", "brant_ext.plugin_activation") != 0 {
		t.Fatal("company.B reads company.A's activation row")
	}
	foreign := w.provision(bootstrap.Grant{Scope: kernel.Scope{Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: "company.A"},
		Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "admin.foreign"}, Permissions: extPerms(), DataScope: "company"})
	if pluginStatus(t, w, foreign, plugin.ApparelPluginID) != plugin.StatusDisabled {
		t.Fatal("another tenant sees company.A's activation")
	}
	if w.count("company.A", "brant_ext.plugin_activation_history") < 1 {
		t.Fatal("enable wrote no history")
	}

	bad := plugin.ApparelManifest()
	bad.ID = "industry.stale"
	bad.Version = "9.9.9"
	w.must(w.c.EXT.Registry.Register(plugin.Registration{Manifest: bad}))
	_, err = w.c.EXT.Enable(admin, plugin.EnablePlugin{IdempotencyKey: w.key(), PluginID: bad.ID})
	w.want(err, plugin.CodePluginVersionIncompatible)
}

func TestConcurrentCostProvidersOneWins(t *testing.T) {
	w := newWorld(t)
	admin := w.actor("admin.ext", extPerms()...)
	rival := plugin.ApparelManifest()
	rival.ID = "industry.rival"
	w.must(w.c.EXT.Registry.Register(plugin.Registration{Manifest: rival}))

	var wg sync.WaitGroup
	errs := make([]error, 2)
	ids := []string{plugin.ApparelPluginID, rival.ID}
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = w.c.EXT.Enable(admin, plugin.EnablePlugin{IdempotencyKey: w.key(), PluginID: ids[i]})
		}(i)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range errs {
		switch code(err) {
		case "OK":
			wins++
		case plugin.CodeCostProviderConflict:
			conflicts++
		default:
			t.Fatalf("enable: %s", code(err))
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins %d conflicts %d", wins, conflicts)
	}
}

func TestApparelRFQFollowsPluginLifecycle(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("admin.ext", extPerms()...)
	draft := inqapi.SaveRfq{RfqID: "rfq.apparel", Lines: []inqapi.RfqLine{{LineID: "L1", RequestedItem: "待核实成衣"}}, DocumentProfile: "apparel"}

	_, err := w.save(ctx, draft)
	w.want(err, plugin.CodePluginNotEnabled)

	_, err = w.c.EXT.Enable(ctx, plugin.EnablePlugin{IdempotencyKey: w.key(), PluginID: plugin.ApparelPluginID})
	w.must(err)
	saved, err := w.save(ctx, draft)
	w.must(err)
	if saved.Status != inqapi.StatusDraft {
		t.Fatalf("save %+v", saved)
	}
	view, err := w.c.INQ.GetRfq(ctx, "rfq.apparel")
	w.must(err)
	if view.DocumentProfile != "apparel" {
		t.Fatalf("profile %q", view.DocumentProfile)
	}

	_, err = w.c.EXT.EnterReadOnly(ctx, plugin.EnterReadOnlyPlugin{IdempotencyKey: w.key(), PluginID: plugin.ApparelPluginID})
	w.must(err)
	_, err = w.save(ctx, inqapi.SaveRfq{RfqID: "rfq.apparel.2", Lines: []inqapi.RfqLine{{LineID: "L1", RequestedItem: "新单"}}, DocumentProfile: "apparel"})
	w.want(err, plugin.CodePluginNotEnabled)
	_, err = w.c.EXT.Disable(ctx, plugin.DisablePlugin{IdempotencyKey: w.key(), PluginID: plugin.ApparelPluginID})
	w.want(err, plugin.CodePluginInUse)

	_, err = w.c.INQ.CancelRfq(ctx, inqapi.CancelRfq{IdempotencyKey: w.key(), RfqID: "rfq.apparel", ExpectedVersion: saved.RowVersion, Reason: "客户取消"})
	w.must(err)
	_, err = w.c.EXT.Disable(ctx, plugin.DisablePlugin{IdempotencyKey: w.key(), PluginID: plugin.ApparelPluginID})
	w.must(err)
	view, err = w.c.INQ.GetRfq(ctx, "rfq.apparel")
	w.must(err)
	if view.Status != inqapi.StatusCancelled || view.DocumentProfile != "apparel" {
		t.Fatalf("history %+v", view)
	}
	if pluginStatus(t, w, ctx, plugin.ApparelPluginID) != plugin.StatusDisabled {
		t.Fatal("disable did not stick")
	}
}

func TestApparelQuoteOrderAndShipmentProfiles(t *testing.T) {
	q := newOrderWorld(t)
	mixed := q.data()
	mixed.DocumentProfile = "apparel"
	_, err := q.saveQuote(q.author, "quote.mixed", 0, mixed)
	q.want(err, plugin.CodeDocumentProfileMismatch)

	order := q.activeOrder("gen")
	if order.QuoteID == "" || order.ObjectID != "order.gen" {
		t.Fatalf("order %+v", order)
	}
	cloneApparelOrder(t, q.tenant, "company.A", order.QuoteID, order.ObjectID)

	pluginAdmin := q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "plugin.admin"},
		Permissions: extPerms(), DataScope: "company", Team: "trade"})
	_, err = q.c.EXT.Enable(pluginAdmin, plugin.EnablePlugin{IdempotencyKey: q.key(), PluginID: plugin.ApparelPluginID})
	q.must(err)
	_, err = q.c.EXT.Disable(pluginAdmin, plugin.DisablePlugin{IdempotencyKey: q.key(), PluginID: plugin.ApparelPluginID})
	q.want(err, plugin.CodePluginInUse)

	shipper := q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "shipper.1"},
		Permissions: append(append([]string{}, allPermissions...), "shipment.prepare", "ful.read"), DataScope: "company", Team: "trade"})
	_, err = q.c.FUL.PrepareShipment(shipper, fulapi.PrepareShipment{IdempotencyKey: q.key(), ShipmentID: "shipment.mix",
		OrderID: order.ObjectID, OrderIDs: []string{order.ObjectID, "order.apparel"}, Route: fulapi.RouteSupplierDirect,
		Lines: []fulapi.ShipmentLine{{LineID: "S1", OrderLineID: "L1", Quantity: "1", QualityReleaseID: "rel.1",
			SupplierDelivery: fulapi.DeliveryLineRef{DeliveryID: "delivery.1", LineID: "L1", Version: 1}}}})
	q.want(err, plugin.CodeDocumentProfileMismatch)

	closeApparelClone(t, q.tenant, "company.A")
	_, err = q.c.EXT.Disable(pluginAdmin, plugin.DisablePlugin{IdempotencyKey: q.key(), PluginID: plugin.ApparelPluginID})
	q.must(err)
	if view := q.getOrder(order.ObjectID); view.QuoteID != order.QuoteID {
		t.Fatalf("generic order unreadable: %+v", view)
	}
}

func extPerms() []string {
	return append(append([]string{}, allPermissions...), plugin.CommandEnable, plugin.CommandEnterReadOnly, plugin.CommandDisable,
		plugin.CommandList, plugin.CommandAudit, "shipment.prepare", "ful.read")
}

func pluginStatus(t *testing.T, w *world, ctx context.Context, id string) string {
	t.Helper()
	list, err := w.c.EXT.ListPlugins(ctx)
	w.must(err)
	for _, p := range list.Plugins {
		if p.PluginID == id {
			return p.Status
		}
	}
	t.Fatalf("plugin %s is not listed", id)
	return ""
}

// cloneApparelOrder copies one active order onto a new quote and order id with document_profile
// apparel, so a shipment can be asked to consolidate the two profiles. The copy is not a second
// business command: the slot unique index allows one full_v1 order per quote, and the profile
// column cannot change after insert. session_replication_role skips the immutability triggers;
// foreign keys still hold because the parent rows are copied first.
func cloneApparelOrder(t *testing.T, tenant, company, quoteID, orderID string) {
	t.Helper()
	pool := devSuperPool(t)
	defer pool.Close()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	const newQuote, newOrder = "quote.apparel", "order.apparel"
	mustCopy(t, ctx, tx, "brant_trd.quote_root", tenant, company, "quote_id", quoteID, newQuote, "document_profile = 'apparel'")
	mustCopy(t, ctx, tx, "brant_trd.quote_revision", tenant, company, "quote_id", quoteID, newQuote, "")
	mustCopy(t, ctx, tx, "brant_trd.quote_document", tenant, company, "quote_id", quoteID, newQuote,
		"number = number || '.apparel', file_name = number || '.apparel.txt'")
	mustCopy(t, ctx, tx, "brant_trd.quote_conversion_slot", tenant, company, "quote_id", quoteID, newQuote, "order_id = 'order.apparel'")
	mustCopy(t, ctx, tx, "brant_trd.sales_order", tenant, company, "order_id", orderID, newOrder,
		"quote_id = 'quote.apparel', document_profile = 'apparel', number = number || '.apparel'")
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func closeApparelClone(t *testing.T, tenant, company string) {
	t.Helper()
	pool := devSuperPool(t)
	defer pool.Close()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE brant_trd.quote_revision SET status = 'cancelled'
		WHERE tenant_id = $1 AND company_id = $2 AND quote_id = 'quote.apparel'`, tenant, company); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE brant_trd.sales_order SET status = 'cancelled', activated_review_id = NULL
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = 'order.apparel'`, tenant, company); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// devSuperPool is the container superuser. brant_migrator cannot set session_replication_role,
// and the copy has to skip the profile immutability triggers.
func devSuperPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("IDP_BRANT_POSTGRES_DSN")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		t.Fatalf("IDP_BRANT_POSTGRES_DSN: %v", err)
	}
	u.User = url.UserPassword("postgres", "dev-superuser-only")
	pool, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func mustCopy(t *testing.T, ctx context.Context, tx pgx.Tx, table, tenant, company, column, from, to, extra string) {
	t.Helper()
	tmp := "tmp_copy_" + table[len("brant_trd."):]
	if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TEMP TABLE %s ON COMMIT DROP AS SELECT * FROM %s
		WHERE tenant_id = $1 AND company_id = $2 AND %s = $3`, tmp, table, column), tenant, company, from); err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	set := column + " = $1"
	if extra != "" {
		set += ", " + extra
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("UPDATE %s SET %s", tmp, set), to); err != nil {
		t.Fatalf("update %s: %v", table, err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s SELECT * FROM %s", table, tmp)); err != nil {
		t.Fatalf("insert %s: %v", table, err)
	}
}
