//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S5-03a on PostgreSQL 16: FUL's shipments and the order fulfilment root (tactical model
// FUL-AGG-Shipment, FUL-AGG-OrderFulfilment, SM-FUL-SHIPMENT, FUL-INV-001, FUL-INV-003; Context Map
// R26, R27, R44, R59, R70, R74; S5 decisions D-1, D-3, D-10, D-11, D-19; BT11). Python V0.50 has no
// fulfilment context: test names cite the design.
//
// The world is TRD's order world (newOrderWorld, the shared order helpers): order.quote.1 active
// for 120 PCS of sku.1 on line L1, customer.1 screened clear. QLT runs on an in-memory port to PUR
// whose commitment lines serve that order line (the QLT suite's precedent), so its releases are
// real QLT releases guarded by QualityReleaseGuard v1. PUR publishes no supplier deliveries yet (S5
// unit B3), so FUL runs on an in-memory Deliveries port that answers verified delivery lines as
// SupplierDeliveryGuard v1 will. TRD's and QLT's ports can hold FUL's transaction right after their
// guard, for the commit-order tests. "merch.1" and "merch.2" hold the role template merchandiser,
// "qc.1" quality_inspector.

// fulHold holds FUL's transaction once, right after the guard named point answered: it announces
// on held and waits until release is closed.
type fulHold struct {
	mu      sync.Mutex
	point   string
	held    chan struct{}
	release chan struct{}
}

func (h *fulHold) set(point string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.point, h.held, h.release = point, make(chan struct{}, 1), make(chan struct{})
}

func (h *fulHold) at(point string) {
	h.mu.Lock()
	if h.point != point {
		h.mu.Unlock()
		return
	}
	held, release := h.held, h.release
	h.point = ""
	h.mu.Unlock()
	held <- struct{}{}
	<-release
}

// wait waits until the hold is reached, failing the test after 30 seconds or when done answers
// first.
func (h *fulHold) wait(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-h.held:
	case err := <-done:
		t.Fatalf("finished before holding its locks: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("never reached the hold")
	}
}

func (h *fulHold) open() { close(h.release) }

type heldOrders struct {
	ful.Orders
	hold *fulHold
}

func (o heldOrders) GuardOrder(ctx context.Context, exec bentopg.Executor, b trdapi.OrderBinding) (trdapi.OrderVerdict, error) {
	v, err := o.Orders.GuardOrder(ctx, exec, b)
	if err == nil {
		o.hold.at("order")
	}
	return v, err
}

type heldReleases struct {
	ful.Releases
	hold *fulHold
}

func (r heldReleases) GuardRelease(ctx context.Context, exec bentopg.Executor, ref qltapi.QualityReleaseRef, at time.Time) (qltapi.QualityReleaseVerdict, error) {
	v, err := r.Releases.GuardRelease(ctx, exec, ref, at)
	if err == nil {
		r.hold.at("release")
	}
	return v, err
}

// fakeDeliveries answers FUL's port to PUR's supplier deliveries from memory, as
// SupplierDeliveryGuard v1 will: an unknown delivery, version or line is not known.
type fakeDeliveries struct {
	mu    sync.Mutex
	lines map[fulapi.DeliveryLineRef]ful.DeliveryLine
}

func (f *fakeDeliveries) put(l ful.DeliveryLine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l.Known = true
	f.lines[l.Ref] = l
}

func (f *fakeDeliveries) ResolveDeliveryLines(_ context.Context, refs []fulapi.DeliveryLineRef) (map[fulapi.DeliveryLineRef]ful.DeliveryLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[fulapi.DeliveryLineRef]ful.DeliveryLine{}
	for _, r := range refs {
		if l, ok := f.lines[r]; ok {
			out[r] = l
		}
	}
	return out, nil
}

func (f *fakeDeliveries) GuardDeliveryLine(_ context.Context, _ bentopg.Executor, ref fulapi.DeliveryLineRef) (ful.DeliveryLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.lines[ref]; ok {
		return l, nil
	}
	return ful.DeliveryLine{Ref: ref, Code: fulapi.CodeSupplierDeliveryUnverified}, nil
}

type fulWorld struct {
	*trdWorld
	order      trdapi.OrderReceipt
	line       trdapi.OrderLine
	subjects   *fakeSubjects
	qlt        *qlt.Module
	deliveries *fakeDeliveries
	hold       *fulHold
	ful        *ful.Module
	qc         context.Context
	merch      context.Context
	merch2     context.Context
	agent      context.Context
	releaseKit
}

func newFulWorld(t *testing.T) *fulWorld {
	t.Helper()
	q := newOrderWorld(t)
	w := &fulWorld{trdWorld: q, order: q.activeOrder("quote.1"), subjects: &fakeSubjects{lines: map[qltapi.SubjectRef]qlt.Subject{}},
		deliveries: &fakeDeliveries{lines: map[fulapi.DeliveryLineRef]ful.DeliveryLine{}}, hold: &fulHold{}}
	facts, err := q.c.TRD.ResolveActiveOrders(q.author, []string{w.order.ObjectID})
	w.must(err)
	if f := facts[w.order.ObjectID]; f.Code != "" || len(f.Lines) != 1 {
		t.Fatalf("order %+v", f)
	} else {
		w.line = f.Lines[0]
	}
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	w.qlt, err = qlt.New(db, qlt.Upstream{Subjects: w.subjects, Sources: q.c.EVD, Parties: q.c.PTY})
	w.must(err)
	db, err = bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	w.ful, err = ful.New(db, ful.Upstream{Orders: heldOrders{Orders: ful.TRDOrders(q.c.TRD), hold: w.hold},
		Releases: heldReleases{Releases: ful.QLTReleases(w.qlt), hold: w.hold}, Deliveries: w.deliveries, Screening: ful.PTYScreening(),
		Sources: q.c.EVD, Compliance: ful.CNXCompliance(), Payments: heldPayments{Payments: ful.STLPayments(), hold: w.hold},
		Documents: ful.DocumentPorts(q.c.POL, q.c.PTY, q.c.TRD), Costs: q.c.CST})
	w.must(err)
	w.qc, w.merch, w.merch2 = q.assign("qc.1", "quality_inspector"), q.assign("merch.1", "merchandiser"), q.assign("merch.2", "merchandiser")
	w.agent = q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: actorRef("ai", "agent.ful"), DataScope: "company",
		Permissions: []string{"shipment.prepare", "packing.plan", "packing.record_actual", "shipment.cancel", "shipment.confirm", "ful.read", "evd.read"}})
	q.source(w.qc, "standard", "合成检验标准：AQL 2.5，一般检验水平 II")
	_, err = q.file(w.qc, "report", "final-inspection.txt", evdapi.MimeText, "合成验货报告", "")
	w.must(err)
	w.releasable()
	return w
}

// released grants release "rel.<commitment>" of quantity on line L1 of purchase commitment
// commitment, which serves the order's line L1 (QLT on its in-memory port to PUR), after an internal
// inspection finding all of it conforming.
func (w *fulWorld) released(commitment, quantity string) string {
	w.t.Helper()
	ref := qltapi.SubjectRef{Kind: qltapi.SubjectCommitmentLine, ID: commitment, LineID: "L1"}
	w.subjects.put(qlt.Subject{Ref: ref, Revision: 1, Hash: hex64("commitment " + commitment), Status: "issued", SupplierID: "supplier.1",
		OrderID: w.order.ObjectID, OrderRevision: w.order.OrderRevision, OrderHash: w.order.OrderHash, OrderLineID: "L1", SkuID: w.line.Sku.SkuID,
		SpecRevision: w.line.Sku.SpecRevision, UnitID: w.line.UnitRef.ID, UnitRevision: w.line.UnitRef.Revision, Quantity: "120"})
	_, err := w.qlt.RecordInspection(w.qc, qltapi.RecordInspection{IdempotencyKey: w.key(), InspectionID: "insp." + commitment, Subject: ref,
		InspectedQuantity: quantity, ConformingQuantity: quantity, NonconformingQuantity: "0", Sampling: qltapi.Sampling{Method: qltapi.SamplingFull},
		Standard: qltapi.StandardRef{SourceID: "standard"}, Inspector: qltapi.Inspector{Kind: qltapi.InspectorInternal}, ReportFileID: "report",
		InspectedAt: kernel.FormatTimestamp(time.Now().Add(-time.Hour))})
	w.must(err)
	_, err = w.qlt.GrantRelease(w.qc, qltapi.GrantRelease{IdempotencyKey: w.key(), ReleaseID: "rel." + commitment, Subject: ref, Quantity: quantity,
		ValidUntil: kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))})
	w.must(err)
	return "rel." + commitment
}

// delivered has PUR answer delivery "delivery.<commitment>" line L1 at version 1, verified for
// quantity, against commitment's line L1.
func (w *fulWorld) delivered(commitment, verified string) fulapi.DeliveryLineRef {
	ref := fulapi.DeliveryLineRef{DeliveryID: "delivery." + commitment, LineID: "L1", Version: 1}
	w.deliveries.put(ful.DeliveryLine{Ref: ref, Status: "verified", VerifiedVersion: 1, CommitmentID: commitment, SupplierID: "supplier.1",
		OrderID: w.order.ObjectID, OrderLineID: "L1", SkuID: w.line.Sku.SkuID, SpecRevision: w.line.Sku.SpecRevision, UnitID: w.line.UnitRef.ID,
		UnitRevision: w.line.UnitRef.Revision, VerifiedQuantity: verified})
	return ref
}

func (w *fulWorld) prepareCmd(id string, quantity string, delivery fulapi.DeliveryLineRef, release string) fulapi.PrepareShipment {
	return fulapi.PrepareShipment{IdempotencyKey: w.key(), ShipmentID: id, OrderID: w.order.ObjectID, Route: fulapi.RouteSupplierDirect,
		Lines: []fulapi.ShipmentLine{{LineID: "S1", OrderLineID: "L1", Quantity: quantity, SupplierDelivery: delivery, QualityReleaseID: release}}}
}

func (w *fulWorld) prepare(ctx context.Context, id, quantity string, delivery fulapi.DeliveryLineRef, release string) (fulapi.ShipmentReceipt, error) {
	return w.ful.PrepareShipment(ctx, w.prepareCmd(id, quantity, delivery, release))
}

func (w *fulWorld) confirmCmd(id string, version int64) fulapi.ConfirmShipment {
	return fulapi.ConfirmShipment{IdempotencyKey: w.key(), ShipmentID: id, ExpectedVersion: version, EvidenceIDs: []string{"evidence"}}
}

func (w *fulWorld) confirm(ctx context.Context, id string, version int64) (fulapi.ShipmentReceipt, error) {
	return w.ful.ConfirmShipment(ctx, w.confirmCmd(id, version))
}

func (w *fulWorld) shipment(id string) fulapi.ShipmentView {
	w.t.Helper()
	v, err := w.ful.GetShipment(w.merch, id)
	w.must(err)
	return v
}

// shipped runs ShipmentFacts v1 (R70) as TRD does, as the application role in company's scope.
func (w *fulWorld) shipped(company, orderID string) ([]string, error) {
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	if err != nil {
		return nil, err
	}
	var out []string
	err = bento.ScopeTransactor{DB: db, Scope: w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		rows, err := exec.Query(ctx, fulapi.OrderShippedSQL, fulapi.OrderShippedArgs(orderID)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			l, err := fulapi.ScanOrderShipped(rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, strings.Join([]string{l.OrderLineID, l.UnitID, strconv.FormatInt(l.UnitRevision, 10), l.Quantity,
				strconv.FormatInt(l.Shipments, 10)}, " "))
		}
		return rows.Err()
	})
	return out, err
}

func (w *fulWorld) fulEvents() []event {
	var out []event
	for _, e := range w.events("company.A", fulapi.Source) {
		if e.Type == fulapi.EventShipmentConfirmed {
			out = append(out, e)
		}
	}
	return out
}

func guardValue(v fulapi.ShipmentView, name string) string {
	if v.Confirmation == nil {
		return ""
	}
	for _, g := range v.Confirmation.Guards {
		if g.Name == name {
			return g.Value
		}
	}
	return ""
}

// SM-FUL-SHIPMENT, BT11 and FUL-INV-001 on the 120 PCS order line (BT11's 200 and 800 of 1000 as 50
// and 70 of 120): an AI agent prepares the first shipment, plans and records its actual packing
// (cartons accounting for every line, UL FUL-ActualPackage, R74); only a human merchandiser
// confirms it (humanOnlyCommands), and only on a granted shipment release (D-11,
// COMMERCIAL_RELEASE_BLOCKED before); the confirmation re-checks CNX's trade compliance, PTY's
// screening of the order's parties, the delivery line, QLT's release, STL's payment condition and
// TRD's order, records each answer and the release it consumed with its SHA-256, and publishes
// ShipmentConfirmed. A replayed submission ships nothing more; the second shipment ships
// the rest; one more piece is SHIPMENT_QUANTITY_EXCEEDED. ShipmentFacts v1 (R70) and the order's
// fulfilment show what shipped.
func TestShipmentPreparePackConfirmBT11(t *testing.T) {
	w := newFulWorld(t)
	rel1, rel2 := w.released("commitment.1", "50"), w.released("commitment.2", "70")
	d1, d2 := w.delivered("commitment.1", "60"), w.delivered("commitment.2", "70")
	if got, err := w.shipped("company.A", w.order.ObjectID); err != nil || len(got) != 0 {
		t.Fatalf("nothing shipped yet %q %v", got, err)
	}
	prepare := w.prepareCmd("shipment.1", "50", d1, rel1)
	p, err := w.ful.PrepareShipment(w.agent, prepare)
	if err != nil || p.Status != fulapi.StatusPrepared || p.RowVersion != 1 || p.Order.OrderID != w.order.ObjectID ||
		p.Order.OrderRevision != w.order.OrderRevision || p.Order.OrderHash != w.order.OrderHash {
		t.Fatalf("prepared %+v %v", p, err)
	}
	again, err := w.ful.PrepareShipment(w.agent, prepare)
	if err != nil || !again.Replayed || again.ActionID != p.ActionID {
		t.Fatalf("the same key %+v %v", again, err)
	}
	prepare.IdempotencyKey = w.key()
	if same, err := w.ful.PrepareShipment(w.agent, prepare); err != nil || !same.Replayed || same.ActionID == p.ActionID {
		t.Fatalf("the same request under another key is answered from the shipment (BT11) %+v %v", same, err)
	}
	other := w.prepareCmd("shipment.1", "49", d1, rel1)
	_, err = w.ful.PrepareShipment(w.agent, other)
	w.want(err, fulapi.CodeBusinessKeyConflict)

	plan := fulapi.PlanPacking{IdempotencyKey: w.key(), ShipmentID: "shipment.1", ExpectedVersion: 1, Packages: []fulapi.Package{{Cartons: 4,
		Contents: []fulapi.PackageContent{{LineID: "S1", QuantityPerCarton: "12"}}}}}
	_, err = w.ful.PlanPacking(w.agent, plan)
	w.want(err, kernel.CodeInvalidKnownValue) // 48 of 50
	plan.IdempotencyKey, plan.Packages[0].Cartons, plan.Packages[0].Contents[0].QuantityPerCarton = w.key(), 5, "10"
	if r, err := w.ful.PlanPacking(w.agent, plan); err != nil || r.RowVersion != 2 {
		t.Fatalf("plan %+v %v", r, err)
	}
	actual := fulapi.RecordPacking{IdempotencyKey: w.key(), ShipmentID: "shipment.1", ExpectedVersion: 2, EvidenceIDs: []string{"evidence"},
		Packages: []fulapi.Package{
			{Cartons: 4, CartonFrom: 1, Contents: []fulapi.PackageContent{{LineID: "S1", QuantityPerCarton: "10"}}, GrossWeightKg: "8.5", NetWeightKg: "7.5", VolumeCbm: "0.04"},
			{Cartons: 1, CartonFrom: 5, Contents: []fulapi.PackageContent{{LineID: "S1", QuantityPerCarton: "10"}}, GrossWeightKg: "8.25", NetWeightKg: "7.5", VolumeCbm: "0.04"},
		}}
	if r, err := w.ful.RecordPacking(w.agent, actual); err != nil || r.RowVersion != 3 {
		t.Fatalf("actual %+v %v", r, err)
	}
	_, err = w.confirm(w.agent, "shipment.1", 3)
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.confirm(w.merch, "shipment.1", 2)
	w.want(err, kernel.CodeVersionConflict)
	_, err = w.confirm(w.merch, "shipment.1", 3)
	w.want(err, fulapi.CodeCommercialReleaseBlocked) // no release evaluated yet (D-11)
	granted := w.granted("shipment.1")
	confirm := w.confirmCmd("shipment.1", 3)
	c, err := w.ful.ConfirmShipment(w.merch, confirm)
	if err != nil || c.Status != fulapi.StatusConfirmed || c.RowVersion != 4 || len(c.UncheckedGuards) != 0 {
		t.Fatalf("confirmed %+v %v", c, err)
	}
	if again, err := w.ful.ConfirmShipment(w.merch, confirm); err != nil || !again.Replayed || again.ActionID != c.ActionID {
		t.Fatalf("a replayed submission ships nothing more (BT11) %+v %v", again, err)
	}
	_, err = w.confirm(w.merch, "shipment.1", 4)
	w.want(err, kernel.CodeInvalidState)

	v := w.shipment("shipment.1")
	cv := v.Confirmation
	if v.Status != fulapi.StatusConfirmed || cv == nil || cv.ConfirmedBy.ID != "merch.1" || len(cv.GuardsSHA256) != 64 || cv.EvidenceIDs[0] != "evidence" ||
		v.PreparedBy.ID != "agent.ful" || v.ActualPacking == nil || v.ActualPacking.CartonCount != 5 || v.ActualPacking.GrossWeightKg != "42.25" ||
		v.ActualPacking.NetWeightKg != "37.5" || v.ActualPacking.VolumeCbm != "0.2" || v.PackingPlan == nil || v.PackingPlan.CartonCount != 5 ||
		v.PackingPlan.GrossWeightKg != "" || len(v.Lines) != 1 || v.Lines[0].Quantity != "50" || v.Lines[0].OrderLineQuantity != "120" ||
		v.Lines[0].SupplierDelivery != d1 || v.Lines[0].CommitmentID != "commitment.1" {
		t.Fatalf("shipment %+v", v)
	}
	if len(v.Parties) != 2 || v.Parties[0].Role != "customer" || v.Parties[1].Role != ptyapi.AddressBuyer || v.Parties[1].AddressID != "hq" {
		t.Fatalf("parties %+v", v.Parties)
	}
	if !strings.HasPrefix(guardValue(v, fulapi.GuardTradeCompliance), "clear/") || guardValue(v, fulapi.GuardExportLicense) != "" ||
		guardValue(v, fulapi.GuardPaymentCondition) != "satisfied//r1/USD/applied=96000/required=96000" ||
		guardValue(v, fulapi.GuardCommercialRelease) != granted.ReleaseID+"/v1/granted" {
		t.Fatalf("CNX, STL and the release in %+v", cv.Guards)
	}
	if !strings.HasPrefix(guardValue(v, "screening:customer.1"), "clear/") || guardValue(v, fulapi.GuardOrder) != "active/r1" ||
		!strings.HasPrefix(guardValue(v, "quality_release:"+rel1), "granted/released=50/") ||
		guardValue(v, "supplier_delivery:delivery.commitment.1/L1/v1") != "verified/verified_version=1/verified=60" {
		t.Fatalf("guards %+v", cv.Guards)
	}
	events := w.fulEvents()
	if len(events) != 1 || events[0].Payload["shipment_id"] != "shipment.1" || events[0].Payload["guards_sha256"] != cv.GuardsSHA256 ||
		events[0].Payload["order_hash"] != w.order.OrderHash || len(events[0].Payload["unchecked_guards"].([]any)) != 0 ||
		events[0].Payload["release_id"] != granted.ReleaseID {
		t.Fatalf("events %+v", events)
	}

	_, err = w.prepare(w.merch2, "shipment.2", "71", d2, rel2)
	w.want(err, fulapi.CodeShipmentQuantityExceeded)
	if path(err) != "/args/lines/0/quantity" {
		t.Fatal(path(err))
	}
	_, err = w.prepare(w.merch2, "shipment.2", "70", d2, rel2)
	w.must(err)
	w.granted("shipment.2")
	if r, err := w.confirm(w.merch2, "shipment.2", 1); err != nil || r.Status != fulapi.StatusConfirmed {
		t.Fatalf("the second shipment ships the rest without a packing record %+v %v", r, err)
	}
	w.released("commitment.3", "10")
	d3 := w.delivered("commitment.3", "10")
	_, err = w.prepare(w.merch, "shipment.3", "1", d3, "rel.commitment.3")
	w.want(err, fulapi.CodeShipmentQuantityExceeded)

	got, err := w.shipped("company.A", w.order.ObjectID)
	if err != nil || len(got) != 1 || got[0] != "L1 "+w.line.UnitRef.ID+" "+strconv.FormatInt(w.line.UnitRef.Revision, 10)+" 120 2" {
		t.Fatalf("ShipmentFacts v1 %q %v", got, err)
	}
	if other, err := w.shipped("company.B", w.order.ObjectID); err != nil || len(other) != 0 {
		t.Fatalf("another company %q %v", other, err)
	}
	facts, err := w.ful.ResolveOrderShipped(w.author, []string{w.order.ObjectID, "order.none"})
	if err != nil || len(facts[w.order.ObjectID]) != 1 || facts[w.order.ObjectID][0].Quantity != "120" || facts["order.none"] == nil ||
		len(facts["order.none"]) != 0 {
		t.Fatalf("ResolveOrderShipped %+v %v", facts, err)
	}
	f, err := w.ful.GetFulfilment(w.merch, w.order.ObjectID)
	if err != nil || f.OrderStatus != trdapi.OrderActive || len(f.Lines) != 1 || f.Lines[0].OrderQuantity != "120" || f.Lines[0].Shipped != "120" ||
		f.Lines[0].Remaining != "0" || f.Lines[0].Prepared != "0" || len(f.Shipments) != 2 || f.Shipments[0].ShipmentID != "shipment.1" ||
		f.Shipments[1].Status != fulapi.StatusConfirmed {
		t.Fatalf("fulfilment %+v %v", f, err)
	}
	if w.count("company.A", "brant_ful.order_fulfilment") != 1 || w.count("company.A", "brant_ful.shipment") != 2 {
		t.Fatal("one fulfilment root for the order")
	}
}

// D-10: the stock route waits for STK. The composed process asks PUR itself (ful.PURDeliveries): a
// delivery line PUR does not have, such as this world's in-memory one, is REFERENCE_INVALID. Nothing
// is stored either way.
func TestStockRouteAndUnknownDeliveries(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "50")
	d := w.delivered("commitment.1", "50")
	stock := w.prepareCmd("shipment.stock", "10", d, rel)
	stock.Route = fulapi.RouteStock
	_, err := w.ful.PrepareShipment(w.merch, stock)
	if code(err) != fulapi.CodeDependencyNotSupported || path(err) != "/args/route" {
		t.Fatalf("stock route %s at %s", code(err), path(err))
	}
	composed := w.c.FUL.PrepareShipment
	_, err = composed(w.merch, w.prepareCmd("shipment.composed", "10", d, rel))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/lines/0/supplier_delivery" {
		t.Fatalf("a delivery line PUR does not have: %s at %s", code(err), path(err))
	}
	if w.count("company.A", "brant_ful.shipment") != 0 || w.count("company.A", "brant_ful.order_fulfilment") != 0 {
		t.Fatal("nothing stored")
	}
	audit, err := w.c.FUL.Audit(w.assign("admin.1", "administrator"))
	w.must(err)
	if len(audit) != 2 || audit[0].Outcome != "rejected" || audit[0].Code != fulapi.CodeDependencyNotSupported ||
		audit[1].Code != kernel.CodeReferenceInvalid {
		t.Fatalf("audit %+v", audit)
	}
}

// What shipment.prepare refuses on the lookups (R26, R27, R59) and what shipment.cancel does
// (SM-FUL-SHIPMENT prepared → cancelled, human or AI): nothing is reserved by a preparation, and a
// cancelled shipment counts for nothing.
func TestPrepareRefusalsAndCancel(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "100")
	d := w.delivered("commitment.1", "120")
	for _, c := range []struct {
		name string
		edit func(*fulapi.PrepareShipment)
		want string
		path string
	}{
		{"unknown order", func(c *fulapi.PrepareShipment) { c.OrderID = "order.none" }, kernel.CodeReferenceInvalid, "/args/order_id"},
		{"line not in the order", func(c *fulapi.PrepareShipment) { c.Lines[0].OrderLineID = "L9" }, kernel.CodeReferenceInvalid, "/args/lines/0/order_line_id"},
		{"unknown release", func(c *fulapi.PrepareShipment) { c.Lines[0].QualityReleaseID = "rel.none" }, kernel.CodeReferenceInvalid,
			"/args/lines/0/quality_release_id"},
		{"unknown delivery version", func(c *fulapi.PrepareShipment) { c.Lines[0].SupplierDelivery.Version = 2 }, kernel.CodeReferenceInvalid,
			"/args/lines/0/supplier_delivery"},
		{"above the release", func(c *fulapi.PrepareShipment) { c.Lines[0].Quantity = "101" }, fulapi.CodeQualityReleaseInsufficient, "/args/lines/0/quantity"},
		{"above the order line", func(c *fulapi.PrepareShipment) { c.Lines[0].Quantity = "121" }, fulapi.CodeShipmentQuantityExceeded, "/args/lines/0/quantity"},
	} {
		cmd := w.prepareCmd("shipment.x", "10", d, rel)
		c.edit(&cmd)
		if _, err := w.ful.PrepareShipment(w.merch, cmd); code(err) != c.want || path(err) != c.path {
			t.Errorf("%s: %s at %s", c.name, code(err), path(err))
		}
	}
	w.deliveries.put(ful.DeliveryLine{Ref: fulapi.DeliveryLineRef{DeliveryID: "delivery.claim", LineID: "L1", Version: 1}, Status: "recorded",
		CommitmentID: "commitment.1", SupplierID: "supplier.1", OrderID: w.order.ObjectID, OrderLineID: "L1", SkuID: w.line.Sku.SkuID,
		SpecRevision: w.line.Sku.SpecRevision, UnitID: w.line.UnitRef.ID, UnitRevision: w.line.UnitRef.Revision, VerifiedQuantity: "0",
		Code: fulapi.CodeSupplierDeliveryUnverified})
	_, err := w.prepare(w.merch, "shipment.claim", "10", fulapi.DeliveryLineRef{DeliveryID: "delivery.claim", LineID: "L1", Version: 1}, rel)
	w.want(err, fulapi.CodeSupplierDeliveryUnverified) // a supplier's claim never counts (TD-AT14)

	_, err = w.prepare(w.merch, "shipment.a", "100", d, rel)
	w.must(err)
	_, err = w.prepare(w.merch, "shipment.b", "100", d, rel)
	w.must(err) // nothing is reserved by preparing
	_, err = w.ful.CancelShipment(w.agent, fulapi.CancelShipment{IdempotencyKey: w.key(), ShipmentID: "shipment.a", ExpectedVersion: 1, Reason: "客户推迟出运"})
	w.must(err)
	_, err = w.confirm(w.merch, "shipment.a", 2)
	w.want(err, kernel.CodeInvalidState)
	if v := w.shipment("shipment.a"); v.Status != fulapi.StatusCancelled || v.Cancellation == nil || v.Cancellation.CancelledBy.ID != "agent.ful" {
		t.Fatalf("cancelled %+v", v)
	}
	w.granted("shipment.b")
	if r, err := w.confirm(w.merch, "shipment.b", 1); err != nil || r.Status != fulapi.StatusConfirmed {
		t.Fatalf("the cancelled shipment counts for nothing %+v %v", r, err)
	}
	_, err = w.ful.CancelShipment(w.merch, fulapi.CancelShipment{IdempotencyKey: w.key(), ShipmentID: "shipment.b", ExpectedVersion: 2, Reason: "x"})
	w.want(err, kernel.CodeInvalidState)
	f, err := w.ful.GetFulfilment(w.merch, w.order.ObjectID)
	if err != nil || f.Lines[0].Shipped != "100" || f.Lines[0].Remaining != "20" || len(f.Shipments) != 2 {
		t.Fatalf("fulfilment %+v %v", f, err)
	}
	_, err = w.ful.GetFulfilment(w.merch, "order.none")
	w.want(err, kernel.CodeNotFound)
}

// FUL-INV-003 and the release consumption at confirmation (TD-AT14 FUL half; QLT-INV-002): two
// shipments, each within its supplier delivery line and release when prepared, together exceed
// them; the second confirmation is SUPPLIER_DELIVERY_INSUFFICIENT, or QUALITY_RELEASE_INSUFFICIENT.
func TestConfirmationJudgesWhatWasConsumed(t *testing.T) {
	w := newFulWorld(t)
	rel1, rel2 := w.released("commitment.1", "60"), w.released("commitment.2", "50")
	d1, d2 := w.delivered("commitment.1", "50"), w.delivered("commitment.2", "60")
	_, err := w.prepare(w.merch, "shipment.d1", "30", d1, rel1)
	w.must(err)
	_, err = w.prepare(w.merch, "shipment.d2", "30", d1, rel1)
	w.must(err)
	w.granted("shipment.d1")
	w.granted("shipment.d2")
	_, err = w.confirm(w.merch, "shipment.d1", 1)
	w.must(err)
	_, err = w.confirm(w.merch, "shipment.d2", 1)
	if code(err) != fulapi.CodeSupplierDeliveryInsufficient || path(err) != "/shipment/lines/0/quantity" {
		t.Fatalf("delivery: %s at %s", code(err), path(err))
	}
	_, err = w.prepare(w.merch, "shipment.r1", "30", d2, rel2)
	w.must(err)
	_, err = w.prepare(w.merch, "shipment.r2", "30", d2, rel2)
	w.must(err)
	w.granted("shipment.r1")
	w.granted("shipment.r2")
	_, err = w.confirm(w.merch, "shipment.r1", 1)
	w.must(err)
	_, err = w.confirm(w.merch, "shipment.r2", 1)
	w.want(err, fulapi.CodeQualityReleaseInsufficient)
	if v := w.shipment("shipment.r2"); v.Status != fulapi.StatusPrepared || v.RowVersion != 1 {
		t.Fatalf("a refused confirmation changes nothing %+v", v)
	}
	if r, err := w.ful.ShipmentReleases(w.doc, "shipment.r2"); err != nil || len(r.Releases) != 1 || r.Releases[0].Status != fulapi.ReleaseGranted {
		t.Fatalf("a refused confirmation does not consume the release %+v %v", r, err)
	}
}

// The confirmation re-checks every guard in its own transaction (D-11): a superseded or unverified
// delivery version (R59), a revoked release (R26, QLT-INV-002: "放行撤回后不能出运"), a party no
// longer screened clear (R44, FUL-INV-002) and an order whose approval was revoked (R27) refuse it.
func TestConfirmationRechecksEveryGuard(t *testing.T) {
	w := newFulWorld(t)
	rel1, rel2 := w.released("commitment.1", "40"), w.released("commitment.2", "40")
	d1, d2 := w.delivered("commitment.1", "40"), w.delivered("commitment.2", "40")
	for _, id := range []string{"shipment.stale", "shipment.revoked", "shipment.screened", "shipment.order"} {
		rel, d := rel2, d2
		if id == "shipment.revoked" {
			rel, d = rel1, d1
		}
		_, err := w.prepare(w.merch, id, "10", d, rel)
		w.must(err)
	}
	stale := d2
	w.deliveries.mu.Lock()
	l := w.deliveries.lines[stale]
	l.Code, l.VerifiedVersion = fulapi.CodeReferenceStale, 2
	w.deliveries.lines[stale] = l
	w.deliveries.mu.Unlock()
	_, err := w.confirm(w.merch, "shipment.stale", 1)
	if code(err) != fulapi.CodeReferenceStale || path(err) != "/shipment/lines/0/supplier_delivery" {
		t.Fatalf("superseded delivery: %s at %s", code(err), path(err))
	}
	w.delivered("commitment.2", "40")

	_, err = w.qlt.RevokeRelease(w.qc, qltapi.RevokeRelease{IdempotencyKey: w.key(), ReleaseID: rel1, Reason: "复检发现色差，撤回放行"})
	w.must(err)
	_, err = w.confirm(w.merch, "shipment.revoked", 1)
	if code(err) != fulapi.CodeQualityReleaseRequired || path(err) != "/shipment/lines/0/quality_release_id" {
		t.Fatalf("revoked release: %s at %s", code(err), path(err))
	}

	screening, err := w.c.PTY.GetScreening(w.author, "customer.1")
	w.must(err)
	_, err = w.c.PTY.BlockParty(w.author, ptyapi.BlockParty{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: screening.RowVersion,
		Reason: "合成：列入负面清单"})
	w.must(err)
	_, err = w.confirm(w.merch, "shipment.screened", 1)
	if code(err) != fulapi.CodePartyScreeningNotClear || path(err) != "/shipment/parties/0" {
		t.Fatalf("screening: %s at %s", code(err), path(err))
	}
	if w.count("company.A", "brant_ful.shipment WHERE status = 'confirmed'") != 0 || len(w.fulEvents()) != 0 {
		t.Fatal("nothing confirmed")
	}
}

// R27 inside the confirmation: an order whose activating approval was revoked after the shipment
// was prepared refuses it (ORDER_APPROVAL_REVOKED), and so it does the next preparation.
func TestConfirmationRechecksTheOrder(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "40")
	d := w.delivered("commitment.1", "40")
	_, err := w.prepare(w.merch, "shipment.1", "10", d, rel)
	w.must(err)
	_, err = w.c.TRD.RevokeOrderApproval(w.reviewer, trdapi.RevokeOrderApproval{IdempotencyKey: w.key(), OrderID: w.order.ObjectID,
		ReviewID: "review.quote.1", Reason: "客户条件变化"})
	w.must(err)
	_, err = w.confirm(w.merch, "shipment.1", 1)
	if code(err) != fulapi.CodeOrderApprovalRevoked || path(err) != "/args/shipment_id" {
		t.Fatalf("confirm: %s at %s", code(err), path(err))
	}
	_, err = w.prepare(w.merch, "shipment.2", "10", d, rel)
	w.want(err, fulapi.CodeOrderApprovalRevoked)
}

// Data scope: a merchandiser with own data scope does not see another's shipment (NOT_FOUND on the
// read and on every command), and FUL's tables keep their rows to their company (FORCE RLS).
func TestShipmentsStayInTheirScope(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "40")
	d := w.delivered("commitment.1", "40")
	_, err := w.prepare(w.merch, "shipment.1", "10", d, rel)
	w.must(err)
	own := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "merch.own"), DataScope: "own",
		Permissions: []string{"shipment.prepare", "packing.plan", "packing.record_actual", "shipment.cancel", "shipment.confirm", "ful.read", "evd.read"}})
	_, err = w.ful.GetShipment(own, "shipment.1")
	w.want(err, kernel.CodeNotFound)
	_, err = w.confirm(own, "shipment.1", 1)
	w.want(err, kernel.CodeNotFound)
	_, err = w.ful.CancelShipment(own, fulapi.CancelShipment{IdempotencyKey: w.key(), ShipmentID: "shipment.1", ExpectedVersion: 1, Reason: "x"})
	w.want(err, kernel.CodeNotFound)
	_, err = w.ful.GetShipment(w.merch, "a b")
	w.want(err, kernel.CodeSchemaInvalid)
	if w.count("company.B", "brant_ful.shipment") != 0 || w.count("company.B", "brant_ful.shipment_line") != 0 ||
		w.count("company.B", "brant_ful.order_fulfilment") != 0 {
		t.Fatal("company.B sees company.A's shipments")
	}
}

// The shipment tables only grow (SM-FUL-SHIPMENT; D-3 "derive balances from immutable entries"):
// the application role may not change a line, a party, a packing record or a fulfilment root, nor
// move a shipment but by its commands; the schema owner is refused by the triggers too; and the
// database refuses a confirmation that would ship more of an order line than it orders even when
// written around the application (FUL-INV-001).
func TestShipmentRecordsAreImmutable(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "100")
	d := w.delivered("commitment.1", "100")
	_, err := w.prepare(w.merch, "shipment.1", "100", d, rel)
	w.must(err)
	_, err = w.prepare(w.merch, "shipment.2", "100", d, rel)
	w.must(err)
	w.granted("shipment.1")
	_, err = w.confirm(w.merch, "shipment.1", 1)
	w.must(err)
	for _, sql := range []string{
		`UPDATE brant_ful.shipment_line SET quantity = 1 WHERE shipment_id = 'shipment.1'`,
		`DELETE FROM brant_ful.shipment_line WHERE shipment_id = 'shipment.2'`,
		`UPDATE brant_ful.shipment_party SET party_id = 'x' WHERE shipment_id = 'shipment.1'`,
		`DELETE FROM brant_ful.shipment WHERE shipment_id = 'shipment.2'`,
		`UPDATE brant_ful.order_fulfilment SET order_id = order_id`,
		`DELETE FROM brant_ful.order_fulfilment`,
		`UPDATE brant_ful.shipment SET status = 'prepared', row_version = 1, confirmed_by_kind = NULL, confirmed_by_id = NULL, confirm_epoch = NULL,
			confirmed_at = NULL, confirm_evidence_ids = NULL, guard_entries = NULL, guard_entries_sha256 = NULL, unchecked_guards = NULL
			WHERE shipment_id = 'shipment.1'`,
		`UPDATE brant_ful.shipment SET status = 'confirmed', row_version = 2, confirmed_by_kind = 'human', confirmed_by_id = 'x', confirm_epoch = 1,
			confirmed_at = now(), confirm_evidence_ids = '{}', guard_entries = '[]',
			guard_entries_sha256 = encode(sha256(convert_to('[]', 'UTF8')), 'hex'), unchecked_guards = '{}' WHERE shipment_id = 'shipment.2'`,
	} {
		if err := w.exec("company.A", sql); err == nil || !strings.Contains(err.Error(), "brant_ful") && !strings.Contains(err.Error(), "42501") {
			t.Errorf("as brant_app %s: %v", sql, err)
		}
		if err := w.world.asOwner("company.A", sql); err == nil || !strings.Contains(err.Error(), "brant_ful") {
			t.Errorf("as the owner %s: %v", sql, err)
		}
	}
	if v := w.shipment("shipment.2"); v.Status != fulapi.StatusPrepared {
		t.Fatalf("after the refused edits %+v", v)
	}
}

// ShipmentFacts v1 as the database publishes it (R70): a SECURITY DEFINER function with a fixed
// search_path that only the application role may execute, refusing a NULL order.
func TestShipmentFactsFunction(t *testing.T) {
	w := newFulWorld(t)
	var definer, publicExec, appExec bool
	var config []string
	w.must(w.execScan("company.A", `SELECT p.prosecdef, p.proconfig, has_function_privilege('public', p.oid, 'EXECUTE'),
		has_function_privilege('brant_app', p.oid, 'EXECUTE') FROM pg_proc p WHERE p.oid = 'brant_ful.order_shipped_v1(text)'::regprocedure`,
		&definer, &config, &publicExec, &appExec))
	if !definer || publicExec || !appExec || !slices.Contains(config, "search_path=pg_catalog, pg_temp") {
		t.Fatalf("definer %v config %v public %v app %v", definer, config, publicExec, appExec)
	}
	if err := w.exec("company.A", "SELECT * FROM brant_ful.order_shipped_v1(NULL)"); sqlCode(err) != "22004" {
		t.Fatalf("NULL: %s", sqlCode(err))
	}
}
