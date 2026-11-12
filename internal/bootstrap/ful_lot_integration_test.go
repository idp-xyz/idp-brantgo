//go:build integration

package bootstrap_test

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S5-03b on PostgreSQL 16: FUL's export lots and the lookups ShipmentLineRef v1 (Context Map R30
// FUL→STL) and ExportLotRef v1 (R36 FUL→CNX) (tactical model FUL-AGG-ExportLot "只追加；批次行引用出运行，
// 确认数量与批次不一致时标记复核"; UL FUL-ExportLot "不复制第二份库存"; baseline 4.1, 22.1, 22.2; CN-AT02; SAP
// reference CN-05). The world is B1a's FUL world (newFulWorld): order.quote.1 active for 120 PCS at
// USD 8 on line L1.

func lotLine(id, shipmentLine, quantity string) fulapi.ExportLotLine {
	return fulapi.ExportLotLine{LineID: id, ShipmentLineID: shipmentLine, Quantity: quantity}
}

func (w *fulWorld) registerLot(ctx context.Context, id, shipment string, lines ...fulapi.ExportLotLine) (fulapi.Receipt, error) {
	return w.ful.RegisterExportLot(ctx, fulapi.RegisterExportLot{IdempotencyKey: w.key(), ExportLotID: id, ShipmentID: shipment, Lines: lines})
}

func (w *fulWorld) exportLot(id string) fulapi.ExportLotView {
	w.t.Helper()
	v, err := w.ful.GetExportLot(w.merch, id)
	w.must(err)
	return v
}

func (w *fulWorld) orderFact() trdapi.ActiveOrderFact {
	w.t.Helper()
	facts, err := w.c.TRD.ResolveActiveOrders(w.author, []string{w.order.ObjectID})
	w.must(err)
	return facts[w.order.ObjectID]
}

// shareOf is the amount in minor units of q of a line ordered whole whose amount is minor: minor × q
// / whole rounded HALF_UP (the rule of package api).
func shareOf(t *testing.T, minor string, q, whole int64) string {
	t.Helper()
	m, ok := new(big.Int).SetString(minor, 10)
	if !ok {
		t.Fatalf("amount %q", minor)
	}
	num := new(big.Int).Mul(m, big.NewInt(2*q))
	num.Add(num, big.NewInt(whole))
	return num.Quo(num, big.NewInt(2*whole)).String()
}

func sameDecimal(a, b string) bool {
	x, errA := kernel.ParseDecimal(a, kernel.PriceFormat)
	y, errB := kernel.ParseDecimal(b, kernel.PriceFormat)
	return errA == nil && errB == nil && x.Equal(y)
}

func (w *fulWorld) lotEvents() []event {
	var out []event
	for _, e := range w.events("company.A", fulapi.Source) {
		if e.Type == fulapi.EventExportLotRegistered {
			out = append(out, e)
		}
	}
	return out
}

// UL FUL-ExportLot, CN-05: a lot registered of a prepared shipment awaits its confirmation and is
// matched once the shipment confirms what it states; a lot of a confirmed shipment is matched at
// once. Quantities and amounts are at the order's prices (50 and 70 of 120 PCS at USD 8). An AI
// agent may register; the same request under another key is the same lot; a shipment line goes into
// one lot only; a lot never takes more than its shipment line, nor a line or shipment that does not
// exist. ExportLotRegistered names the shipment's state at registration.
func TestExportLotFromPreparedAndConfirmedShipments(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "120")
	d := w.delivered("commitment.1", "120")
	_, err := w.prepare(w.merch, "shipment.1", "50", d, rel)
	w.must(err)
	_, err = w.prepare(w.merch, "shipment.2", "70", d, rel)
	w.must(err)
	order := w.orderFact()
	register := fulapi.RegisterExportLot{IdempotencyKey: w.key(), ExportLotID: "lot.1", ShipmentID: "shipment.1",
		Lines: []fulapi.ExportLotLine{lotLine("E1", "S1", "50")}}
	r, err := w.ful.RegisterExportLot(w.merch, register)
	if err != nil || r.Status != fulapi.LotRegistered || r.RowVersion != 1 || r.ObjectID != "lot.1" {
		t.Fatalf("registered %+v %v", r, err)
	}
	register.IdempotencyKey = w.key()
	if same, err := w.ful.RegisterExportLot(w.merch, register); err != nil || !same.Replayed || same.ObjectID != "lot.1" {
		t.Fatalf("the same lot under another key %+v %v", same, err)
	}
	_, err = w.registerLot(w.merch, "lot.1", "shipment.2", lotLine("E1", "S1", "70"))
	if code(err) != fulapi.CodeBusinessKeyConflict || path(err) != "/args/export_lot_id" {
		t.Fatalf("another lot on the ID: %s at %s", code(err), path(err))
	}
	for _, c := range []struct {
		name, shipment string
		line           fulapi.ExportLotLine
		want, path     string
	}{
		{"a line another lot holds", "shipment.1", lotLine("E1", "S1", "1"), fulapi.CodeBusinessKeyConflict, "/args/lines/0/shipment_line_id"},
		{"above the shipment line", "shipment.2", lotLine("E1", "S1", "71"), kernel.CodeInvalidKnownValue, "/args/lines/0/quantity"},
		{"no such line", "shipment.2", lotLine("E1", "S9", "1"), kernel.CodeReferenceInvalid, "/args/lines/0/shipment_line_id"},
		{"no such shipment", "shipment.none", lotLine("E1", "S1", "1"), kernel.CodeReferenceInvalid, "/args/shipment_id"},
	} {
		if _, err := w.registerLot(w.merch, "lot.x", c.shipment, c.line); code(err) != c.want || path(err) != c.path {
			t.Errorf("%s: %s at %s", c.name, code(err), path(err))
		}
	}

	l := w.exportLot("lot.1")
	net50 := shareOf(t, w.line.NetMinor, 50, 120)
	if l.ShipmentStatus != fulapi.StatusPrepared || l.ShipmentStatusAtRegistration != fulapi.StatusPrepared || l.ShipmentVersionAtRegistration != 1 ||
		l.Reconciliation != fulapi.LotAwaitingConfirmation || len(l.Lines) != 1 || l.Lines[0].ConfirmedQuantity != "" || l.Lines[0].Quantity != "50" ||
		l.Lines[0].Amounts.NetMinor != net50 || l.Totals.NetMinor != net50 || !sameDecimal(l.Lines[0].UnitPrice, w.line.UnitPrice) ||
		l.Currency.ID != order.CurrencyRef.ID || l.Currency.Revision != order.CurrencyRef.Revision || l.Order.OrderHash != w.order.OrderHash ||
		l.CustomerID != "customer.1" || l.RegisteredBy.ID != "merch.1" || l.Lines[0].OrderLineID != "L1" || l.Lines[0].ShipmentLineQuantity != "50" {
		t.Fatalf("lot of the prepared shipment %+v", l)
	}
	_, err = w.confirm(w.merch, "shipment.1", 1)
	w.must(err)
	l = w.exportLot("lot.1")
	if l.Reconciliation != fulapi.LotMatched || l.ShipmentStatus != fulapi.StatusConfirmed || l.ShipmentVersion != 2 || l.ConfirmedAt.IsZero() ||
		l.Lines[0].ConfirmedQuantity != "50" || len(l.Differences) != 0 || l.ShipmentStatusAtRegistration != fulapi.StatusPrepared {
		t.Fatalf("lot after the confirmation %+v", l)
	}

	_, err = w.confirm(w.merch, "shipment.2", 1)
	w.must(err)
	if r, err := w.registerLot(w.milestoneAgent(), "lot.2", "shipment.2", lotLine("E1", "S1", "70")); err != nil || r.Status != fulapi.LotRegistered {
		t.Fatalf("an agent registering %+v %v", r, err)
	}
	l2 := w.exportLot("lot.2")
	if l2.Reconciliation != fulapi.LotMatched || l2.ShipmentStatusAtRegistration != fulapi.StatusConfirmed || l2.ShipmentVersionAtRegistration != 2 ||
		l2.Totals.NetMinor != shareOf(t, w.line.NetMinor, 70, 120) || l2.RegisteredBy.ID != "agent.milestones" {
		t.Fatalf("lot of the confirmed shipment %+v", l2)
	}
	events := w.lotEvents()
	if len(events) != 2 || events[0].Payload["export_lot_id"] != "lot.1" || events[0].Payload["shipment_status"] != fulapi.StatusPrepared ||
		events[1].Payload["shipment_status"] != fulapi.StatusConfirmed || events[0].Payload["net_minor"] != net50 ||
		events[0].Payload["order_hash"] != w.order.OrderHash {
		t.Fatalf("events %+v", events)
	}
}

// "确认数量与批次不一致时标记复核": a lot taking 30 of a 40 line is review_required once the shipment
// confirms 40; a lot of a shipment that is then cancelled is review_required with nothing
// confirmed; a lot of a confirmed shipment that states less than it shipped is review_required at
// once. Nothing of a cancelled shipment is exported (INVALID_STATE). The lot itself never changes:
// CNX reconciles it (C3a).
func TestExportLotMarkedForReviewWhenTheConfirmationDiffers(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "100")
	d := w.delivered("commitment.1", "100")
	for _, s := range []struct{ id, qty string }{{"shipment.a", "40"}, {"shipment.b", "20"}, {"shipment.c", "10"}} {
		_, err := w.prepare(w.merch, s.id, s.qty, d, rel)
		w.must(err)
	}
	_, err := w.registerLot(w.merch, "lot.a", "shipment.a", lotLine("E1", "S1", "30"))
	w.must(err)
	_, err = w.confirm(w.merch, "shipment.a", 1)
	w.must(err)
	a := w.exportLot("lot.a")
	if a.Reconciliation != fulapi.LotReviewRequired || len(a.Differences) != 1 || a.Differences[0].LotQuantity != "30" ||
		a.Differences[0].ConfirmedQuantity != "40" || a.Differences[0].LineID != "E1" || a.Lines[0].ConfirmedQuantity != "40" {
		t.Fatalf("a lot below the confirmation %+v", a)
	}

	_, err = w.registerLot(w.merch, "lot.b", "shipment.b", lotLine("E1", "S1", "20"))
	w.must(err)
	_, err = w.ful.CancelShipment(w.merch, fulapi.CancelShipment{IdempotencyKey: w.key(), ShipmentID: "shipment.b", ExpectedVersion: 1, Reason: "客户取消本批"})
	w.must(err)
	b := w.exportLot("lot.b")
	if b.Reconciliation != fulapi.LotReviewRequired || b.ShipmentStatus != fulapi.StatusCancelled || len(b.Differences) != 1 ||
		b.Differences[0].ConfirmedQuantity != "0" || b.Lines[0].ConfirmedQuantity != "0" || b.ShipmentStatusAtRegistration != fulapi.StatusPrepared {
		t.Fatalf("a lot of a cancelled shipment %+v", b)
	}
	_, err = w.registerLot(w.merch, "lot.b2", "shipment.b", lotLine("E1", "S1", "20"))
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/shipment_id" {
		t.Fatalf("a lot of a cancelled shipment: %s at %s", code(err), path(err))
	}

	_, err = w.confirm(w.merch, "shipment.c", 1)
	w.must(err)
	_, err = w.registerLot(w.merch, "lot.c", "shipment.c", lotLine("E1", "S1", "5"))
	w.must(err)
	if c := w.exportLot("lot.c"); c.Reconciliation != fulapi.LotReviewRequired || c.Differences[0].ConfirmedQuantity != "10" {
		t.Fatalf("a lot stating less than its confirmed shipment %+v", c)
	}
	if w.count("company.A", "brant_ful.export_lot") != 3 {
		t.Fatal("lots")
	}
}

// CN-AT02, the lot side ("一单分批出口、同次出口包含多销售行": 销售、出运及报关项映射可核对，累计不超有效业务范围):
// the order's 120 PCS leave in two shipments, each of two lines from two supplier deliveries; each
// shipment becomes one export lot of both its lines. Every lot line names its shipment line, every
// shipment line its lot (ShipmentLineRef v1), the lots of each shipment are found by shipment
// (ExportLotRef v1), and over both lots the order line is exported exactly once: 120 of 120, with
// amounts that add up to what the shipment lines are worth.
func TestExportLotsMapSplitShipmentsCNAT02(t *testing.T) {
	w := newFulWorld(t)
	rel1, rel2 := w.released("commitment.1", "60"), w.released("commitment.2", "60")
	d1, d2 := w.delivered("commitment.1", "60"), w.delivered("commitment.2", "60")
	for _, id := range []string{"shipment.1", "shipment.2"} {
		_, err := w.ful.PrepareShipment(w.merch, fulapi.PrepareShipment{IdempotencyKey: w.key(), ShipmentID: id, OrderID: w.order.ObjectID,
			Route: fulapi.RouteSupplierDirect, Lines: []fulapi.ShipmentLine{
				{LineID: "S1", OrderLineID: "L1", Quantity: "30", SupplierDelivery: d1, QualityReleaseID: rel1},
				{LineID: "S2", OrderLineID: "L1", Quantity: "30", SupplierDelivery: d2, QualityReleaseID: rel2}}})
		w.must(err)
		_, err = w.confirm(w.merch, id, 1)
		w.must(err)
	}
	_, err := w.registerLot(w.merch, "lot.1", "shipment.1", lotLine("E1", "S1", "30"), lotLine("E2", "S2", "30"))
	w.must(err)
	_, err = w.registerLot(w.merch, "lot.2", "shipment.2", lotLine("E1", "S2", "30"), lotLine("E2", "S1", "30"))
	w.must(err)

	lots, err := w.ful.ResolveShipmentExportLots(w.merch, []string{"shipment.1", "shipment.2", "shipment.none"})
	w.must(err)
	if len(lots["shipment.1"]) != 1 || lots["shipment.1"][0].ExportLotID != "lot.1" || len(lots["shipment.2"]) != 1 ||
		lots["shipment.none"] == nil || len(lots["shipment.none"]) != 0 {
		t.Fatalf("lots by shipment %+v", lots)
	}
	lines, err := w.ful.ResolveOrderShipmentLines(w.merch, []string{w.order.ObjectID})
	w.must(err)
	refs := lines[w.order.ObjectID]
	if len(refs) != 4 {
		t.Fatalf("shipment lines %+v", refs)
	}
	lineNet := map[fulapi.ShipmentLineKey]string{}
	shipped := kernel.Decimal{}
	for _, r := range refs {
		want := map[string]string{"shipment.1": "lot.1", "shipment.2": "lot.2"}[r.Key.ShipmentID]
		if !r.Settleable() || r.ExportLotID != want || r.OrderLineID != "L1" {
			t.Errorf("shipment line %+v", r)
		}
		q, _ := fulapi.Quantity(r.Quantity)
		shipped = shipped.Add(q)
		lineNet[r.Key] = r.Amounts.NetMinor
	}
	exported, exportedNet, linesNet := kernel.Decimal{}, new(big.Int), new(big.Int)
	for _, l := range []fulapi.ExportLotRef{lots["shipment.1"][0], lots["shipment.2"][0]} {
		if l.Reconciliation != fulapi.LotMatched || len(l.Lines) != 2 {
			t.Fatalf("lot %+v", l)
		}
		for _, x := range l.Lines {
			q, _ := fulapi.Quantity(x.Quantity)
			exported = exported.Add(q)
			n, _ := new(big.Int).SetString(x.Amounts.NetMinor, 10)
			exportedNet.Add(exportedNet, n)
			m, _ := new(big.Int).SetString(lineNet[fulapi.ShipmentLineKey{ShipmentID: l.ShipmentID, LineID: x.ShipmentLineID}], 10)
			linesNet.Add(linesNet, m)
		}
	}
	if lot2 := lots["shipment.2"][0]; lot2.Lines[0].ShipmentLineID != "S2" || lot2.Lines[1].ShipmentLineID != "S1" {
		t.Fatalf("lot.2 maps E1 to S2 and E2 to S1: %+v", lot2.Lines)
	}
	if exported.String() != "120" || shipped.String() != "120" || exportedNet.Cmp(linesNet) != 0 {
		t.Fatalf("exported %s shipped %s net %s of %s", exported, shipped, exportedNet, linesNet)
	}
}

// ShipmentLineRef v1 for STL (R30) and ExportLotRef v1 for CNX (R36) under data scope: a confirmed
// line is a settlement source with the order line's price at the revision its shipment binds and
// the amount of its quantity; a prepared one is INVALID_STATE; unknown shipments and lines are
// absent; an actor with own data scope, and another company, see nothing. After an amendment moves
// the order to revision 2 at another price, the shipment line and the lot keep revision 1's price
// (baseline 4.1: 出运保存价格、币种).
func TestShipmentLineAndExportLotLookups(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "100")
	d := w.delivered("commitment.1", "100")
	_, err := w.prepare(w.merch, "shipment.1", "50", d, rel)
	w.must(err)
	_, err = w.confirm(w.merch, "shipment.1", 1)
	w.must(err)
	_, err = w.prepare(w.merch2, "shipment.2", "20", d, rel)
	w.must(err)
	_, err = w.registerLot(w.merch, "lot.1", "shipment.1", lotLine("E1", "S1", "50"))
	w.must(err)
	order := w.orderFact()

	k1, k2 := fulapi.ShipmentLineKey{ShipmentID: "shipment.1", LineID: "S1"}, fulapi.ShipmentLineKey{ShipmentID: "shipment.2", LineID: "S1"}
	keys := []fulapi.ShipmentLineKey{k1, k2, {ShipmentID: "shipment.none", LineID: "S1"}, {ShipmentID: "shipment.1", LineID: "S9"}}
	refs, err := w.ful.ResolveShipmentLines(w.merch, keys)
	w.must(err)
	r1, r2 := refs[k1], refs[k2]
	net50 := shareOf(t, w.line.NetMinor, 50, 120)
	if len(refs) != 2 || !r1.Settleable() || r1.Check("/x") != nil || r1.ShipmentStatus != fulapi.StatusConfirmed || r1.ConfirmedAt.IsZero() ||
		r1.Price == nil || !sameDecimal(r1.Price.UnitPrice, w.line.UnitPrice) || r1.Price.NetMinor != w.line.NetMinor ||
		r1.Price.Currency.ID != order.CurrencyRef.ID || r1.Amounts.NetMinor != net50 || r1.Quantity != "50" || r1.OrderLineQuantity != "120" ||
		r1.ExportLotID != "lot.1" || r1.SupplierDelivery != d || r1.QualityReleaseID != rel || r1.CommitmentID != "commitment.1" ||
		r1.CustomerID != "customer.1" || r1.Order.OrderID != w.order.ObjectID || r1.Order.OrderRevision != 1 || r1.UnitID != w.line.UnitRef.ID {
		t.Fatalf("the confirmed line %+v", r1)
	}
	if r2.Code != kernel.CodeInvalidState || code(r2.Check("/lines/1")) != kernel.CodeInvalidState || r2.ShipmentStatus != fulapi.StatusPrepared ||
		r2.ExportLotID != "" || r2.Amounts.NetMinor != shareOf(t, w.line.NetMinor, 20, 120) {
		t.Fatalf("the prepared line %+v", r2)
	}
	if byOrder, err := w.ful.ResolveOrderShipmentLines(w.merch, []string{w.order.ObjectID, "order.none"}); err != nil ||
		len(byOrder[w.order.ObjectID]) != 2 || byOrder["order.none"] == nil || len(byOrder["order.none"]) != 0 {
		t.Fatalf("by order %+v %v", byOrder, err)
	}
	lots, err := w.ful.ResolveExportLots(w.merch, []string{"lot.1", "lot.none"})
	if err != nil || len(lots) != 1 || lots["lot.1"].Reconciliation != fulapi.LotMatched || lots["lot.1"].Totals.NetMinor != net50 {
		t.Fatalf("lots %+v %v", lots, err)
	}
	many := make([]fulapi.ShipmentLineKey, fulapi.MaxLineLookups+1)
	for i := range many {
		many[i] = k1
	}
	_, err = w.ful.ResolveShipmentLines(w.merch, many)
	w.want(err, kernel.CodeSchemaInvalid)

	own := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "settle.own"), DataScope: "own",
		Permissions: []string{"ful.read"}})
	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: actorRef("human", "settle.b"), DataScope: "company",
		Permissions: []string{"ful.read"}})
	for name, ctx := range map[string]context.Context{"own data scope": own, "company.B": other} {
		refs, err := w.ful.ResolveShipmentLines(ctx, keys)
		lots, lerr := w.ful.ResolveExportLots(ctx, []string{"lot.1"})
		byShipment, serr := w.ful.ResolveShipmentExportLots(ctx, []string{"shipment.1"})
		byOrder, oerr := w.ful.ResolveOrderShipmentLines(ctx, []string{w.order.ObjectID})
		if err != nil || lerr != nil || serr != nil || oerr != nil || len(refs) != 0 || len(lots) != 0 || len(byShipment["shipment.1"]) != 0 ||
			len(byOrder[w.order.ObjectID]) != 0 {
			t.Errorf("%s sees %+v %+v %+v %+v (%v %v %v %v)", name, refs, lots, byShipment, byOrder, err, lerr, serr, oerr)
		}
	}

	amend := w.amendCmd(w.order.ObjectID, "amendment.price", 0, w.order.OrderRevision, w.order.OrderHash, "120")
	amend.Lines[0].UnitPrice = "9"
	p, err := w.c.TRD.PrepareAmendment(w.author, amend)
	w.must(err)
	s, err := w.c.TRD.SubmitAmendment(w.author, trdapi.SubmitAmendment{IdempotencyKey: w.key(), OrderID: w.order.ObjectID, AmendmentID: "amendment.price",
		ExpectedVersion: p.RowVersion})
	w.must(err)
	dec, err := w.c.TRD.DecideAmendment(w.reviewer, trdapi.DecideAmendment{IdempotencyKey: w.key(), OrderID: w.order.ObjectID,
		AmendmentID: "amendment.price", ExpectedVersion: s.RowVersion, AmendmentHash: s.AmendmentHash, Decision: trdapi.DecisionApprove,
		Reason: "客户书面确认新单价"})
	w.must(err)
	applied, err := w.c.TRD.ApplyAmendment(w.reviewer, w.applyCmd(w.order.ObjectID, "amendment.price", dec.RowVersion))
	w.must(err)
	if now := w.orderFact(); applied.OrderRevision != 2 || len(now.Lines) != 1 || sameDecimal(now.Lines[0].UnitPrice, w.line.UnitPrice) {
		t.Fatalf("revision 2 at another price %+v %+v", applied, now.Lines)
	}
	refs, err = w.ful.ResolveShipmentLines(w.merch, []fulapi.ShipmentLineKey{k1})
	w.must(err)
	if r := refs[k1]; r.Price == nil || !sameDecimal(r.Price.UnitPrice, w.line.UnitPrice) || r.Amounts.NetMinor != net50 || r.Order.OrderRevision != 1 {
		t.Fatalf("the line keeps revision 1's price %+v", r)
	}
	if l := w.exportLot("lot.1"); l.Totals.NetMinor != net50 || !sameDecimal(l.Lines[0].UnitPrice, w.line.UnitPrice) {
		t.Fatalf("the lot keeps revision 1's price %+v", l)
	}
}

// lockWaiters counts the backends waiting for a row lock of table.
func lockWaiters(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(DISTINCT l.pid) FROM pg_locks l WHERE l.locktype = 'tuple'
		AND l.relation = $1::regclass AND EXISTS (SELECT 1 FROM pg_locks x WHERE x.pid = l.pid AND NOT x.granted)`, table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// awaitWaiters waits until n backends wait for a row lock of table.
func awaitWaiters(t *testing.T, pool *pgxpool.Pool, table string, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for lockWaiters(t, pool, table) < n {
		if time.Now().After(deadline) {
			t.Fatalf("never %d waiting on %s", n, table)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// holdAuthority locks company's authority guard row FOR NO KEY UPDATE, as an authority change
// does, in a transaction of the schema owner: every command of the company then waits at its
// authority guard. release rolls the transaction back.
func (w *fulWorld) holdAuthority(pool *pgxpool.Pool, company string) (release func()) {
	w.t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	w.must(err)
	s := w.scope(company)
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', $2, true)`, string(s.Tenant), string(s.Company))
	w.must(err)
	_, err = tx.Exec(ctx, `SELECT authority_epoch FROM brant_auth.authority_scope WHERE tenant_id = $1 AND company_id = $2 FOR NO KEY UPDATE`,
		string(s.Tenant), string(s.Company))
	w.must(err)
	return func() { _ = tx.Rollback(ctx) }
}

func async(f func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	return ch
}

// A lot registration meets a shipment confirmation or cancellation (FUL-AGG-ExportLot; lock order:
// the registration holds the shipment FOR SHARE, the confirmation and the cancellation hold it FOR
// UPDATE), in both commit orders on PG16. Confirmation first: the registration waits for it and
// records the shipment confirmed (matched). Registration first: the confirmation waits for it; the
// lot records the shipment prepared and is matched once the confirmation commits. Cancellation
// first: the registration waits and is refused (INVALID_STATE). Registration first, cancellation
// after: the lot records the shipment prepared and is review_required once it is cancelled.
func TestExportLotRegistrationMeetsConfirmationAndCancellation(t *testing.T) {
	w := newFulWorld(t)
	pool := pgtest.MigratorPool(t)
	rel := w.released("commitment.1", "120")
	d := w.delivered("commitment.1", "120")
	for _, id := range []string{"shipment.a", "shipment.b", "shipment.c", "shipment.d"} {
		_, err := w.prepare(w.merch, id, "10", d, rel)
		w.must(err)
	}
	register := func(lot, shipment string) func() error {
		return func() error {
			_, err := w.registerLot(w.merch, lot, shipment, lotLine("E1", "S1", "10"))
			return err
		}
	}
	confirm := func(id string) func() error {
		return func() error {
			_, err := w.confirm(w.merch, id, 1)
			return err
		}
	}
	cancel := func(id string) func() error {
		return func() error {
			_, err := w.ful.CancelShipment(w.merch, fulapi.CancelShipment{IdempotencyKey: w.key(), ShipmentID: id, ExpectedVersion: 1, Reason: "并发取消"})
			return err
		}
	}

	w.hold.set("release")
	c := async(confirm("shipment.a"))
	w.hold.wait(t, c)
	r := async(register("lot.a", "shipment.a"))
	awaitWaiters(t, pool, "brant_ful.shipment", 1)
	w.hold.open()
	w.must(<-c)
	w.must(<-r)
	if l := w.exportLot("lot.a"); l.ShipmentStatusAtRegistration != fulapi.StatusConfirmed || l.ShipmentVersionAtRegistration != 2 ||
		l.Reconciliation != fulapi.LotMatched {
		t.Fatalf("confirmation first %+v", l)
	}

	release := w.holdAuthority(pool, "company.A")
	r = async(register("lot.b", "shipment.b"))
	awaitWaiters(t, pool, "brant_auth.authority_scope", 1)
	c = async(confirm("shipment.b"))
	awaitWaiters(t, pool, "brant_auth.authority_scope", 2)
	release()
	w.must(<-r)
	w.must(<-c)
	if l := w.exportLot("lot.b"); l.ShipmentStatusAtRegistration != fulapi.StatusPrepared || l.ShipmentVersionAtRegistration != 1 ||
		l.ShipmentStatus != fulapi.StatusConfirmed || l.Reconciliation != fulapi.LotMatched {
		t.Fatalf("registration first %+v", l)
	}

	release = w.holdAuthority(pool, "company.A")
	x := async(cancel("shipment.c"))
	awaitWaiters(t, pool, "brant_auth.authority_scope", 1)
	r = async(register("lot.c", "shipment.c"))
	awaitWaiters(t, pool, "brant_ful.shipment", 1)
	release()
	w.must(<-x)
	if err := <-r; code(err) != kernel.CodeInvalidState {
		t.Fatalf("cancellation first: %v", err)
	}

	release = w.holdAuthority(pool, "company.A")
	r = async(register("lot.d", "shipment.d"))
	awaitWaiters(t, pool, "brant_auth.authority_scope", 1)
	x = async(cancel("shipment.d"))
	awaitWaiters(t, pool, "brant_ful.shipment", 1)
	release()
	w.must(<-r)
	w.must(<-x)
	if l := w.exportLot("lot.d"); l.ShipmentStatusAtRegistration != fulapi.StatusPrepared || l.ShipmentStatus != fulapi.StatusCancelled ||
		l.Reconciliation != fulapi.LotReviewRequired || l.Differences[0].ConfirmedQuantity != "0" {
		t.Fatalf("registration first, cancellation after %+v", l)
	}
	if w.count("company.A", "brant_ful.export_lot") != 3 {
		t.Fatal("three lots")
	}
}

// Export lots and line prices only grow (FUL-AGG-ExportLot "只追加"): the application role and the
// schema owner may not change or delete a lot, a lot line or a price; a second lot line on a
// shipment line is refused by its unique key, and a lot line above its shipment line by the
// trigger, even when written around the application.
func TestExportLotRecordsAreImmutable(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "100")
	d := w.delivered("commitment.1", "100")
	_, err := w.prepare(w.merch, "shipment.1", "50", d, rel)
	w.must(err)
	_, err = w.registerLot(w.merch, "lot.1", "shipment.1", lotLine("E1", "S1", "40"))
	w.must(err)
	if w.count("company.A", "brant_ful.shipment_line_price") != 1 {
		t.Fatal("one price per shipment line")
	}
	for _, sql := range []string{
		`UPDATE brant_ful.export_lot_line SET quantity = 1`,
		`DELETE FROM brant_ful.export_lot_line`,
		`UPDATE brant_ful.export_lot SET net_minor = 0, payable_minor = tax_minor`,
		`DELETE FROM brant_ful.export_lot`,
		`UPDATE brant_ful.shipment_line_price SET unit_price = 1`,
		`DELETE FROM brant_ful.shipment_line_price`,
	} {
		if err := w.exec("company.A", sql); err == nil || !strings.Contains(err.Error(), "brant_ful") && !strings.Contains(err.Error(), "42501") {
			t.Errorf("as brant_app %s: %v", sql, err)
		}
		if err := w.world.asOwner("company.A", sql); err == nil || !strings.Contains(err.Error(), "brant_ful") {
			t.Errorf("as the owner %s: %v", sql, err)
		}
	}
	insert := `INSERT INTO brant_ful.export_lot_line (tenant_id, company_id, export_lot_id, line_id, line_no, shipment_id, shipment_line_id, quantity,
		net_minor, tax_minor, payable_minor) SELECT tenant_id, company_id, export_lot_id, 'E9', 2, shipment_id, 'S1', %s, 0, 0, 0
		FROM brant_ful.export_lot`
	if err := w.world.asOwner("company.A", strings.Replace(insert, "%s", "5", 1)); sqlCode(err) != "23505" {
		t.Errorf("a shipment line in a second lot line: %v", err)
	}
	if err := w.world.asOwner("company.A", strings.Replace(insert, "%s", "51", 1)); err == nil || !strings.Contains(err.Error(), "takes more") {
		t.Errorf("a lot line above its shipment line: %v", err)
	}
	if l := w.exportLot("lot.1"); l.Lines[0].Quantity != "40" || len(l.Lines) != 1 {
		t.Fatalf("after the refused edits %+v", l)
	}
}
