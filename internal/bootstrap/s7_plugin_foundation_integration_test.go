//go:build integration

package bootstrap_test

import (
	"context"
	"sync"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/plugin"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S7-01: the plugin platform on PostgreSQL 16. A company enables industry.apparel, and apparel
// documents are refused until that activation is active. Two cost providers cannot both be active
// for one document profile. plugin.disable runs only from draining_readonly, and open apparel
// documents keep it at PLUGIN_IN_USE until they close; the rows stay readable afterwards.

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
	_, err = w.c.EXT.Disable(ctx, plugin.DisablePlugin{IdempotencyKey: w.key(), PluginID: plugin.ApparelPluginID})
	w.want(err, kernel.CodeInvalidState)
	if path(err) != "/args/plugin_id" || rejectionReason(err) != "Run plugin.enter_readonly before plugin.disable" {
		t.Fatalf("disable from active: %s %s", path(err), rejectionReason(err))
	}
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

	_, err = q.c.EXT.Enable(q.author, plugin.EnablePlugin{IdempotencyKey: q.key(), PluginID: plugin.ApparelPluginID})
	q.must(err)
	apparel := q.activeOrderFrom("apparel", q.apparelData())
	if apparel.QuoteID != "apparel" || apparel.ObjectID != "order.apparel" {
		t.Fatalf("apparel order %+v", apparel)
	}
	if quote := q.getQuote(apparel.QuoteID); quote.DocumentProfile != "apparel" {
		t.Fatalf("quote profile %q", quote.DocumentProfile)
	}
	guard := q.orderGuard(trdapi.OrderBinding{OrderID: apparel.ObjectID, OrderRevision: apparel.OrderRevision, OrderHash: apparel.OrderHash})
	if !guard.Usable() || guard.DocumentProfile != "apparel" {
		t.Fatalf("order guard %+v", guard)
	}
	facts, err := q.c.TRD.ResolveActiveOrders(q.author, []string{apparel.ObjectID})
	q.must(err)
	if facts[apparel.ObjectID].DocumentProfile != "apparel" {
		t.Fatalf("order fact %+v", facts[apparel.ObjectID])
	}

	generic := q.activeOrder("gen")
	shipper := q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "shipper.1"},
		Permissions: append(append([]string{}, allPermissions...), "shipment.prepare", "ful.read"), DataScope: "company", Team: "trade"})
	_, err = q.c.FUL.PrepareShipment(shipper, fulapi.PrepareShipment{IdempotencyKey: q.key(), ShipmentID: "shipment.mix",
		OrderID: generic.ObjectID, OrderIDs: []string{generic.ObjectID, apparel.ObjectID}, Route: fulapi.RouteSupplierDirect,
		Lines: []fulapi.ShipmentLine{{LineID: "S1", OrderLineID: "L1", Quantity: "1", QualityReleaseID: "rel.1",
			SupplierDelivery: fulapi.DeliveryLineRef{DeliveryID: "delivery.1", LineID: "L1", Version: 1}}}})
	q.want(err, plugin.CodeDocumentProfileMismatch)

	_, err = q.c.EXT.Disable(q.author, plugin.DisablePlugin{IdempotencyKey: q.key(), PluginID: plugin.ApparelPluginID})
	q.want(err, kernel.CodeInvalidState)
	if rejectionReason(err) != "Run plugin.enter_readonly before plugin.disable" {
		t.Fatalf("disable with open apparel work: %s", rejectionReason(err))
	}
	_, err = q.c.EXT.EnterReadOnly(q.author, plugin.EnterReadOnlyPlugin{IdempotencyKey: q.key(), PluginID: plugin.ApparelPluginID})
	q.must(err)
	_, err = q.c.EXT.Disable(q.author, plugin.DisablePlugin{IdempotencyKey: q.key(), PluginID: plugin.ApparelPluginID})
	q.want(err, plugin.CodePluginInUse)

	_, err = q.c.TRD.CancelOrder(q.author, trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: apparel.ObjectID,
		ExpectedVersion: apparel.RowVersion, Reason: "客户撤单", Evidence: q.proof})
	q.must(err)
	_, err = q.c.EXT.Disable(q.author, plugin.DisablePlugin{IdempotencyKey: q.key(), PluginID: plugin.ApparelPluginID})
	q.want(err, plugin.CodePluginInUse)
	_, err = q.c.INQ.CloseRfq(q.author, inqapi.CloseRfq{IdempotencyKey: q.key(), RfqID: "rfq.apparel",
		ExpectedVersion: q.apparelRfqVersion, Reason: "询价已转订单"})
	q.must(err)
	quote := q.getQuote(apparel.QuoteID)
	_, err = q.c.TRD.Cancel(q.author, trdapi.Cancel{IdempotencyKey: q.key(), QuoteID: apparel.QuoteID,
		ExpectedVersion: quote.RootVersion, BusinessRevision: quote.ActiveRevision, Reason: "客户取消", Evidence: q.proof})
	q.want(err, trdapi.CodeRootConversionLocked)
	_, err = q.c.EXT.Disable(q.author, plugin.DisablePlugin{IdempotencyKey: q.key(), PluginID: plugin.ApparelPluginID})
	q.must(err)

	if pluginStatus(t, q.world, q.author, plugin.ApparelPluginID) != plugin.StatusDisabled {
		t.Fatal("disable did not stick")
	}
	if view := q.getOrder(generic.ObjectID); view.QuoteID != generic.QuoteID || view.Status != trdapi.OrderActive {
		t.Fatalf("generic order unreadable: %+v", view)
	}
	ended := q.getOrder(apparel.ObjectID)
	if ended.Status != trdapi.OrderCancelled || ended.QuoteID != apparel.QuoteID {
		t.Fatalf("apparel order unreadable: %+v", ended)
	}
	after := q.orderGuard(trdapi.OrderBinding{OrderID: apparel.ObjectID, OrderRevision: apparel.OrderRevision, OrderHash: apparel.OrderHash})
	if after.DocumentProfile != "apparel" || after.Status != trdapi.OrderCancelled {
		t.Fatalf("order guard after cancel %+v", after)
	}
	if kept := q.getQuote(apparel.QuoteID); kept.DocumentProfile != "apparel" || kept.ActiveRevision == 0 {
		t.Fatalf("apparel quote unreadable: %+v", kept)
	}
	rfq, err := q.c.INQ.GetRfq(q.author, "rfq.apparel")
	q.must(err)
	if rfq.Status != inqapi.StatusClosed || rfq.DocumentProfile != "apparel" {
		t.Fatalf("apparel rfq %+v", rfq)
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

func rejectionReason(err error) string {
	rej, ok := kernel.AsRejection(err)
	if !ok || len(rej.Fields) == 0 {
		return ""
	}
	return rej.Fields[0].Reason
}
