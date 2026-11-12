//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// FUL follows an order change (WBS S5-09b, FUL part; tactical model FUL-AGG-OrderFulfilment entity
// ShipmentReview, SM-FUL-SHIPMENT-REVIEW; WF-06 step 5, its idempotency and failure mode; event
// storming PL-09; S5 decisions D-18, D-21; BT10) on PostgreSQL 16: TRD's real orders of the FUL world,
// their own Outbox events relayed through the assembled relay into the dispatcher of every composed
// context (S4-06a), FUL's consumer ful.shipment_review opening the reviews as its system actor, and
// the merchandiser ("merch.1", the role template that holds shipment_review.settle) disposing with
// FUL's own commands and settling. The order's line L1 is 120 PCS; FUL ships it from a delivery of
// 120 verified PCS under a quality release of 120 (in-memory PUR port, real QLT). Python V0.50 has no
// fulfilment context: the tests cite design IDs.

var shipmentReviewActor = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.ful.shipment_review"}

// fulReview is the FUL world with every composed consumer behind a throwaway outbox.
type fulReview struct {
	*fulWorld
	ev   *orderEvents
	subs []messaging.Subscription
	sc   kernel.Scope
}

// fulReviewOn provisions every consumer's actor; wrap, when set, replaces ful.shipment_review's
// subscription in the dispatcher.
func fulReviewOn(t *testing.T, w *fulWorld, wrap func(messaging.Subscription) messaging.Subscription) *fulReview {
	subs := w.c.Subscriptions()
	routed := slices.Clone(subs)
	for i, s := range routed {
		if s.Consumer == "ful.shipment_review" && wrap != nil {
			routed[i] = wrap(s)
		}
	}
	r := &fulReview{fulWorld: w, ev: newOrderEvents(t, routed...), subs: routed, sc: w.scope("company.A")}
	if _, err := w.platform.ProvisionInbox(context.Background(), r.sc, subs); err != nil {
		t.Fatal(err)
	}
	return r
}

// relayTRD relays TRD's Outbox events not relayed before and returns them.
func (r *fulReview) relayTRD() []eventing.Envelope {
	r.t.Helper()
	out := r.ev.copyFrom(r.sc, trdapi.Source)
	r.ev.drain(r.ev.relay(5))
	r.ev.published(out...)
	return out
}

// amendedTo has an amendment id of the world's order approved and applied, L1 to qty.
func (r *fulReview) amendedTo(id, qty string) {
	r.t.Helper()
	order := r.order.ObjectID
	v := r.getOrder(order)
	d := r.approvedAmendment(order, id, v.OrderRevision, v.OrderHash, qty)
	_, err := r.c.TRD.ApplyAmendment(r.reviewer, r.applyCmd(order, id, d.RowVersion))
	r.must(err)
}

func (r *fulReview) reviews() []fulapi.ShipmentReviewView {
	r.t.Helper()
	v, err := r.ful.ListShipmentReviews(r.merch, r.order.ObjectID)
	r.must(err)
	return v.ShipmentReviews
}

func (r *fulReview) settle(ctx context.Context, id string, version int64, items ...fulapi.ReviewItemDisposition) (fulapi.Receipt, error) {
	return r.ful.SettleShipmentReview(ctx, fulapi.SettleShipmentReview{IdempotencyKey: r.key(), ReviewID: id, ExpectedVersion: version, Items: items,
		Reason: "合成：订单变更出运复核完成"})
}

func disposal(no int, kind, shipment, reason string) fulapi.ReviewItemDisposition {
	return fulapi.ReviewItemDisposition{ItemNo: no, Disposition: kind, ShipmentID: shipment, Reason: reason}
}

// reviewItems names a review's items: kind, shipment and object.
func reviewItems(v fulapi.ShipmentReviewView) string {
	var out []string
	for _, it := range v.Items {
		s := it.Kind + ":" + it.ShipmentID
		switch it.Kind {
		case fulapi.ReviewItemPacking:
			s += "/" + it.PackingKind
		case fulapi.ReviewItemBooking:
			s += "/" + strconvI(it.BookingVersion)
		case fulapi.ReviewItemDocument:
			s += "/" + it.DocumentKind
		case fulapi.ReviewItemRelease:
			s += "/" + it.ReleaseScope
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

// reviewLine is a shipment item's only line: impact, quantity, target, shipped, remaining.
func reviewLine(it fulapi.ShipmentReviewItemView) string {
	if len(it.Lines) != 1 {
		return "lines " + strings.Repeat("?", len(it.Lines))
	}
	l := it.Lines[0]
	return strings.Join([]string{l.Impact, l.Quantity, l.TargetQuantity, l.ShippedQuantity, l.RemainingQuantity}, " ")
}

// planned records the packing plan of shipment id at expected: cartons of 10 PCS of line S1.
func (r *fulReview) planned(id string, expected, cartons int64) {
	r.t.Helper()
	_, err := r.ful.PlanPacking(r.merch, fulapi.PlanPacking{IdempotencyKey: r.key(), ShipmentID: id, ExpectedVersion: expected,
		Packages: []fulapi.Package{{Cartons: cartons, Contents: []fulapi.PackageContent{{LineID: "S1", QuantityPerCarton: "10"}}}}})
	r.must(err)
}

// cancelled has merch.1 cancel shipment id at expected.
func (r *fulReview) cancelled(id string, expected int64) {
	r.t.Helper()
	_, err := r.ful.CancelShipment(r.merch, fulapi.CancelShipment{IdempotencyKey: r.key(), ShipmentID: id, ExpectedVersion: expected,
		Reason: "合成：订单变更后按新修订重排"})
	r.must(err)
}

// BT10, FUL side ("部分已出运后减量或改色：展示…包装及交期影响；保留已发生部分和批准处理"), with SM-FUL-SHIPMENT-REVIEW and
// WF-06 step 5: of the order's 120 PCS on L1, shipment.1 shipped 40 (confirmed at revision 1, its
// documents release granted and not yet used); shipment.2 is prepared for the other 80 with its
// packing plan, a booking, a draft commercial invoice and packing list, and its shipment release
// granted. The customer reduces L1 to 110 (revision 2). TRD's own OrderAmended, relayed to
// ful.shipment_review, opens one review as the consumer's system actor for the merchandiser: the
// confirmed shipment.1 stays a fact and only its granted documents release is an item; shipment.2
// (80 of the 70 left), its packing plan, booking, both drafts and its release are items. Claiming a
// disposition that did not happen is refused, as is an AI agent settling. The merchandiser cancels
// shipment.2 and re-plans the 70 on revision 2 as shipment.3 — packing plan, booking correction,
// a new commercial invoice, its release evaluated — and the documentation clerk evaluates
// shipment.1's documents release again; the review is settled by reference to all of it, each
// disposition traceable. A settled review is settled once and a redelivered amendment reopens
// nothing.
func TestShipmentReviewFollowsAReducedOrderBT10(t *testing.T) {
	w := newFulDocWorld(t)
	r := fulReviewOn(t, w.fulWorld, nil)
	order := w.order.ObjectID
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "40", d, rel)
	w.grantedOn(w.ful, "shipment.1", fulapi.ScopeDocuments)
	_, err := w.prepare(w.merch, "shipment.2", "80", d, rel)
	w.must(err)
	r.planned("shipment.2", 1, 8)
	_, err = w.ful.RecordBooking(w.merch, w.bookingCmd("shipment.2", 0, w.forwarder, "DEHAM"))
	w.must(err)
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.2", "shipment.2", 0))
	w.must(err)
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.plCmd("docs.2", "shipment.2", 1, fulapi.Package{Cartons: 8, CartonFrom: 1,
		Contents: []fulapi.PackageContent{{LineID: "S1", QuantityPerCarton: "10"}}, GrossWeightKg: "10.5", NetWeightKg: "9.5", VolumeCbm: "0.06"}))
	w.must(err)
	w.granted("shipment.2")
	r.relayTRD()
	if v := r.reviews(); len(v) != 0 {
		t.Fatalf("an activation opens no review: %+v", v)
	}

	r.amendedTo("amendment.110", "110")
	amended := eventOf(r.relayTRD(), trdapi.EventOrderAmended)
	v := r.reviews()
	if amended.ID == "" || len(v) != 1 {
		t.Fatalf("after OrderAmended %s: %d reviews", amended.ID, len(v))
	}
	rv := v[0]
	if rv.Status != fulapi.ReviewOpen || rv.RowVersion != 1 || rv.Cause != fulapi.CauseOrderAmended || rv.OrderRevision != 2 ||
		rv.EventID != string(amended.ID) || rv.OrderVersion != w.orderVersion(order) || rv.OrderHash != w.getOrder(order).OrderHash ||
		rv.ResponsibleRole != fulapi.ReviewResponsibleRole || rv.OpenedBy != shipmentReviewActor ||
		reviewItems(rv) != "release:shipment.1/documents shipment:shipment.2 packing:shipment.2/plan booking:shipment.2/1 "+
			"document:shipment.2/commercial_invoice document:shipment.2/packing_list release:shipment.2/shipment" {
		t.Fatalf("the review %+v: %s", rv, reviewItems(rv))
	}
	it := rv.Items
	if it[0].ShipmentStatus != fulapi.StatusConfirmed || it[0].ShipmentRevision != 1 || it[0].ReleaseStatus != fulapi.ReleaseGranted ||
		it[0].ReleaseEvaluations != 1 || it[0].Impact != fulapi.ImpactRevisionChanged {
		t.Fatalf("the confirmed shipment's documents release %+v", it[0])
	}
	if got := reviewLine(it[1]); got != "reduced 80 110 40 70" || it[1].ShipmentStatus != fulapi.StatusPrepared || it[1].ShipmentRevision != 1 ||
		it[1].ShipmentVersion != 2 {
		t.Fatalf("shipment item %s %+v", got, it[1])
	}
	if it[2].PackingKind != fulapi.PackingPlan || it[2].PackingSeq != 1 || it[2].Cartons != 8 || it[3].BookingNumber != "SHA2611001" ||
		it[4].DocumentSetID != "docs.2" || it[4].DocumentVersion != 1 || it[4].DocumentNumber == "" || it[5].DocumentKind != fulapi.DocumentPackingList ||
		it[6].ReleaseScope != fulapi.ScopeShipment || !strings.Contains(it[1].Facts, `"remaining_quantity":"70"`) {
		t.Fatalf("items %+v", it)
	}

	// Nothing is done yet: the dispositions cannot be claimed.
	all := func(first fulapi.ReviewItemDisposition) []fulapi.ReviewItemDisposition {
		out := []fulapi.ReviewItemDisposition{disposal(1, fulapi.DispositionNoAction, "", "x")}
		for no := 2; no <= 7; no++ {
			out = append(out, disposal(no, fulapi.DispositionShipmentCancelled, "", "x"))
		}
		out[first.ItemNo-1] = first
		return out
	}
	_, err = r.settle(w.merch, rv.ReviewID, 1, all(disposal(2, fulapi.DispositionShipmentCancelled, "", "x"))...)
	refusedAt(t, "a shipment not cancelled", err, kernel.CodeInvalidState, "/args/items/1/disposition")
	_, err = r.settle(w.merch, rv.ReviewID, 1, all(disposal(2, fulapi.DispositionNoAction, "", "x"))...)
	refusedAt(t, "no_action on a prepared shipment", err, kernel.CodeInvalidKnownValue, "/args/items/1/disposition")
	_, err = r.settle(w.merch, rv.ReviewID, 1, all(disposal(1, fulapi.DispositionShipmentCancelled, "", "x"))...)
	refusedAt(t, "a confirmed shipment cancelled", err, kernel.CodeInvalidKnownValue, "/args/items/0/disposition")
	_, err = r.settle(w.merch, rv.ReviewID, 1, all(disposal(1, fulapi.DispositionReevaluated, "", "x"))...)
	refusedAt(t, "a release not evaluated again", err, kernel.CodeInvalidState, "/args/items/0/disposition")
	_, err = r.settle(w.merch, rv.ReviewID, 1, disposal(1, fulapi.DispositionNoAction, "", "x"))
	refusedAt(t, "items left out", err, kernel.CodeInvalidKnownValue, "/args/items")
	_, err = r.settle(w.agent, rv.ReviewID, 1, all(disposal(1, fulapi.DispositionNoAction, "", "x"))...)
	if code(err) != kernel.CodePermissionDenied {
		t.Fatalf("an AI agent settling: %s", code(err))
	}

	// The dispositions, with FUL's own commands: shipment.2 cancelled, the 70 left re-planned on
	// revision 2 (releasable once compliance cleared it and STL's account moved to it).
	r.cancelled("shipment.2", 2)
	w.amendedReleasable(order)
	_, err = w.prepare(w.merch, "shipment.3", "70", d, rel)
	w.must(err)
	_, err = r.settle(w.merch, rv.ReviewID, 1, all(disposal(3, fulapi.DispositionReplaced, "shipment.3", "x"))...)
	refusedAt(t, "a replacement without a packing plan", err, kernel.CodeInvalidState, "/args/items/2/shipment_id")
	_, err = r.settle(w.merch, rv.ReviewID, 1, all(disposal(2, fulapi.DispositionReplaced, "shipment.1", "x"))...)
	refusedAt(t, "a replacement of revision 1", err, kernel.CodeInvalidState, "/args/items/1/shipment_id")
	_, err = r.settle(w.merch, rv.ReviewID, 1, all(disposal(2, fulapi.DispositionReplaced, "shipment.none", "x"))...)
	refusedAt(t, "a replacement that does not exist", err, kernel.CodeReferenceInvalid, "/args/items/1/shipment_id")
	r.planned("shipment.3", 1, 7)
	_, err = w.ful.RecordBooking(w.merch, w.bookingCmd("shipment.3", 0, w.forwarder, "DEHAM"))
	w.must(err)
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.3", "shipment.3", 0))
	w.must(err)
	granted := w.granted("shipment.3")
	again, err := w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeDocuments)
	w.must(err)
	cmd := fulapi.SettleShipmentReview{IdempotencyKey: w.key(), ReviewID: rv.ReviewID, ExpectedVersion: 1, Reason: "合成：减量后按修订 2 重排",
		Items: []fulapi.ReviewItemDisposition{
			disposal(1, fulapi.DispositionReevaluated, "", "已出运部分的放单按新修订重新评估"),
			disposal(2, fulapi.DispositionReplaced, "shipment.3", "剩余 70 件按修订 2 重新准备"),
			disposal(3, fulapi.DispositionReplaced, "shipment.3", "按 7 箱重排装箱计划"),
			disposal(4, fulapi.DispositionReplaced, "shipment.3", "货代确认订舱改到 shipment.3"),
			disposal(5, fulapi.DispositionReplaced, "shipment.3", "商业发票按修订 2 重新编制"),
			disposal(6, fulapi.DispositionShipmentCancelled, "", "装箱单待实际装箱后编制"),
			disposal(7, fulapi.DispositionReplaced, "shipment.3", "放行按 shipment.3 重新评估")}}
	s, err := w.ful.SettleShipmentReview(w.merch, cmd)
	w.must(err)
	if s.Status != fulapi.ReviewSettled || s.RowVersion != 2 || s.ObjectID != rv.ReviewID {
		t.Fatalf("settled %+v", s)
	}
	got, err := w.ful.GetShipmentReview(w.merch, rv.ReviewID)
	w.must(err)
	ds := make([]string, 0, len(got.Items))
	for _, it := range got.Items {
		x := it.Disposition
		if x == nil || x.Reason == "" {
			t.Fatalf("item %d without its disposition %+v", it.ItemNo, it)
		}
		ds = append(ds, strings.Join([]string{x.Disposition, x.ShipmentID, strconvI(x.Revision), x.Ref, x.Status}, ","))
		if x.Disposition != fulapi.DispositionNoAction && x.At.IsZero() {
			t.Fatalf("item %d disposition without its instant %+v", it.ItemNo, x)
		}
	}
	want := []string{"reevaluated,,0," + again.ReleaseID + "," + again.Status, "replaced,shipment.3,2,,prepared", "replaced,shipment.3,2,plan/1,prepared",
		"replaced,shipment.3,2,booking/1,prepared", "replaced,shipment.3,2,commercial_invoice/1,prepared", "shipment_cancelled,,0,,",
		"replaced,shipment.3,2," + granted.ReleaseID + ",granted"}
	if got.Status != fulapi.ReviewSettled || got.SettledBy.ID != "merch.1" || got.SettleEpoch == 0 || got.SettleReason == "" ||
		strings.Join(ds, " ") != strings.Join(want, " ") {
		t.Fatalf("the settled review %+v:\n%s\nwant\n%s", got, strings.Join(ds, "\n"), strings.Join(want, "\n"))
	}
	// The confirmed shipment stays as it was: what shipped is a fact.
	if sh := w.fulWorld.shipment("shipment.1"); sh.Status != fulapi.StatusConfirmed || sh.RowVersion != 2 || sh.Order.OrderRevision != 1 {
		t.Fatalf("shipment.1 after the review %+v", sh)
	}
	if facts, err := w.shipped("company.A", order); err != nil || strings.Join(facts, "|") != "L1 PCS 1 40 1" {
		t.Fatalf("ShipmentFacts v1 after the review %v %v", facts, err)
	}
	if replay, err := w.ful.SettleShipmentReview(w.merch, cmd); err != nil || !replay.Replayed {
		t.Fatalf("replay %+v %v", replay, err)
	}
	cmd.IdempotencyKey = w.key()
	_, err = w.ful.SettleShipmentReview(w.merch, cmd)
	w.want(err, kernel.CodeVersionConflict)
	cmd.IdempotencyKey, cmd.ExpectedVersion = w.key(), 2
	_, err = w.ful.SettleShipmentReview(w.merch, cmd)
	refusedAt(t, "settled twice", err, kernel.CodeInvalidState, "/args/review_id")

	// A redelivery of the amendment, by this dispatcher and another, reopens nothing.
	second, err := messaging.NewDispatcher(r.subs...)
	w.must(err)
	for _, dp := range []*messaging.Dispatcher{r.ev.dispatcher, second} {
		w.must(dp.Publish(context.Background(), amended))
	}
	if v := r.reviews(); len(v) != 1 || v[0].Status != fulapi.ReviewSettled {
		t.Fatalf("after redeliveries %+v", v)
	}
}

// SM-FUL-SHIPMENT-REVIEW open → superseded ("更晚的订单修订开启新复核") and WF-06's idempotency ("更晚修订的复核取代未
// 处置的旧复核"), then an order's end: shipment.2 is prepared for 80 of L1 at revision 1 with its packing
// plan and its shipment release granted. The order is amended to 110 (revision 2): review A lists
// them. Before anyone settles A it is amended again to 100 (revision 3): review B lists the same
// stale objects and supersedes A, which can no longer be settled (REFERENCE_STALE). Nothing has
// shipped, so the customer's cancellation passes (TD-AT02): TRD's OrderCancelled opens review C for
// the cancellation, which supersedes B; an ended order has no later revision to re-plan on, so
// replaced is refused there; once shipment.2 is cancelled, C is settled. At most one review of the
// order is open at a time.
func TestShipmentReviewIsSupersededByALaterChange(t *testing.T) {
	w := newFulWorld(t)
	r := fulReviewOn(t, w, nil)
	order := w.order.ObjectID
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	_, err := w.prepare(w.merch, "shipment.2", "80", d, rel)
	w.must(err)
	r.planned("shipment.2", 1, 8)
	w.granted("shipment.2")

	r.amendedTo("amendment.110", "110")
	r.relayTRD()
	v := r.reviews()
	if len(v) != 1 || v[0].Status != fulapi.ReviewOpen || reviewItems(v[0]) != "shipment:shipment.2 packing:shipment.2/plan release:shipment.2/shipment" ||
		reviewLine(v[0].Items[0]) != "unchanged 80 110 0 110" {
		t.Fatalf("review A %+v", v)
	}
	a := v[0]
	r.amendedTo("amendment.100", "100")
	r.relayTRD()
	v = r.reviews()
	if len(v) != 2 || v[0].ReviewID != a.ReviewID || v[0].Status != fulapi.ReviewSuperseded || v[0].RowVersion != 2 || v[0].SupersededAt.IsZero() ||
		v[1].Status != fulapi.ReviewOpen || v[1].OrderRevision != 3 || v[0].SupersededBy != v[1].ReviewID || reviewItems(v[1]) != reviewItems(a) ||
		reviewLine(v[1].Items[0]) != "unchanged 80 100 0 100" {
		t.Fatalf("review B superseding A %+v", v)
	}
	b := v[1]
	_, err = r.settle(w.merch, a.ReviewID, 2, disposal(1, fulapi.DispositionShipmentCancelled, "", "x"),
		disposal(2, fulapi.DispositionShipmentCancelled, "", "x"), disposal(3, fulapi.DispositionShipmentCancelled, "", "x"))
	refusedAt(t, "a superseded review", err, fulapi.CodeReferenceStale, "/args/review_id")

	_, err = r.endCancel(order)
	w.must(err)
	cancelled := eventOf(r.relayTRD(), trdapi.EventOrderCancelled)
	v = r.reviews()
	if cancelled.ID == "" || len(v) != 3 || v[1].Status != fulapi.ReviewSuperseded || v[1].SupersededBy != v[2].ReviewID ||
		v[2].Cause != fulapi.CauseOrderCancelled || v[2].OrderRevision != 3 || v[2].Status != fulapi.ReviewOpen || reviewItems(v[2]) != reviewItems(a) ||
		v[2].Items[0].Impact != fulapi.ImpactOrderCancelled || reviewLine(v[2].Items[0]) != "order_cancelled 80  0 0" {
		t.Fatalf("review C of the cancellation %+v", v)
	}
	c := v[2]
	every := func(kind, shipment string) []fulapi.ReviewItemDisposition {
		return []fulapi.ReviewItemDisposition{disposal(1, kind, shipment, "合成：订单已取消"), disposal(2, kind, shipment, "合成：订单已取消"),
			disposal(3, kind, shipment, "合成：订单已取消")}
	}
	_, err = r.settle(w.merch, c.ReviewID, 1, every(fulapi.DispositionReplaced, "shipment.9")...)
	refusedAt(t, "re-planning an ended order", err, kernel.CodeInvalidKnownValue, "/args/items/0/disposition")
	_, err = r.settle(w.merch, b.ReviewID, 1, every(fulapi.DispositionShipmentCancelled, "")...)
	w.want(err, kernel.CodeVersionConflict)
	r.cancelled("shipment.2", 2)
	_, err = r.settle(w.merch, b.ReviewID, 2, every(fulapi.DispositionShipmentCancelled, "")...)
	refusedAt(t, "review B superseded by the cancellation's", err, fulapi.CodeReferenceStale, "/args/review_id")
	s, err := r.settle(w.merch, c.ReviewID, 1, every(fulapi.DispositionShipmentCancelled, "")...)
	w.must(err)
	got, err := w.ful.GetShipmentReview(w.merch, c.ReviewID)
	w.must(err)
	if s.Status != fulapi.ReviewSettled || got.Status != fulapi.ReviewSettled || got.Items[0].Disposition == nil ||
		got.Items[0].Disposition.At.IsZero() || got.Items[2].Disposition.Disposition != fulapi.DispositionShipmentCancelled {
		t.Fatalf("review C settled %+v %+v", s, got)
	}
}

// WF-06's idempotency ("事件 ID 去重；各下游按（订单、订单修订）只开启一次复核") and failure mode, with ES-09 step 6:
// an amendment that leaves nothing stale opens nothing (shipment.1's 40 are confirmed, a fact); a
// prepared shipment.2 of revision 2 makes revision 3's amendment open a review. Revision 3's
// OrderAmended, dead-lettered by a broken consumer, is replayed after revision 4's was relayed: it
// opens nothing. Revision 4's OrderAmended published again by two workers at once, besides the
// relay, stays one review. The order then closes its remainder short (ES-09 step 6): its
// OrderRemainderClosed opens the end's review, superseding revision 4's. The database refuses a
// second review of one order revision and cause, a second open review of an order, a disposition that
// did not happen, a changed item and a deleted review.
func TestShipmentReviewOpensOncePerOrderRevision(t *testing.T) {
	var broken atomic.Bool
	w := newFulWorld(t)
	r := fulReviewOn(t, w, func(s messaging.Subscription) messaging.Subscription { return breakable(s, &broken, true) })
	order := w.order.ObjectID
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "40", d, rel)
	r.amendedTo("amendment.110", "110")
	if e := eventOf(r.relayTRD(), trdapi.EventOrderAmended); e.ID == "" || len(r.reviews()) != 0 {
		t.Fatalf("an amendment that leaves nothing stale: %+v", r.reviews())
	}

	w.amendedReleasable(order)
	_, err := w.prepare(w.merch, "shipment.2", "70", d, rel)
	w.must(err)
	r.amendedTo("amendment.100", "100")
	lost := r.ev.copyFrom(r.sc, trdapi.Source)
	dead := eventOf(lost, trdapi.EventOrderAmended)
	broken.Store(true)
	r.ev.drain(r.ev.relay(1))
	broken.Store(false)
	if s := r.ev.status(dead.ID); s != "ABANDONED" || len(r.reviews()) != 0 {
		t.Fatalf("broken consumer: outbox %s, reviews %+v", s, r.reviews())
	}
	r.amendedTo("amendment.90", "90")
	later := eventOf(r.relayTRD(), trdapi.EventOrderAmended)
	v := r.reviews()
	if len(v) != 1 || v[0].OrderRevision != 4 || v[0].EventID != string(later.ID) || reviewItems(v[0]) != "shipment:shipment.2" ||
		reviewLine(v[0].Items[0]) != "reduced 70 90 40 50" || v[0].Items[0].ShipmentRevision != 2 {
		t.Fatalf("revision 4's review %+v", v)
	}
	operator := r.provision(bootstrap.Grant{Scope: r.sc, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"},
		Permissions: []string{messaging.ReplayPermission}})
	if _, err := r.ev.replayer().Replay(operator, dead.Source, string(dead.ID)); err != nil {
		t.Fatalf("replay of revision 3: %v", err)
	}
	if v := r.reviews(); len(v) != 1 || v[0].OrderRevision != 4 || v[0].Status != fulapi.ReviewOpen {
		t.Fatalf("after the late replay %+v", v)
	}

	second, err := messaging.NewDispatcher(r.subs...)
	w.must(err)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, dp := range []*messaging.Dispatcher{r.ev.dispatcher, second} {
		wg.Go(func() {
			if err := dp.Publish(context.Background(), later); errors.Is(err, eventing.ErrPublishRejected) {
				errs[i] = err
			}
		})
	}
	wg.Wait()
	w.must(errors.Join(errs...))
	if v := r.reviews(); len(v) != 1 || len(v[0].Items) != 1 {
		t.Fatalf("after two workers %+v", v)
	}
	id := v[0].ReviewID

	_, err = r.endShort(order)
	w.must(err)
	closed := eventOf(r.relayTRD(), trdapi.EventOrderRemainderClosed)
	v = r.reviews()
	if closed.ID == "" || len(v) != 2 || v[0].Status != fulapi.ReviewSuperseded || v[1].Cause != fulapi.CauseOrderRemainderClosed ||
		reviewItems(v[1]) != "shipment:shipment.2" || reviewLine(v[1].Items[0]) != "remainder_closed 70  40 0" {
		t.Fatalf("the end's review %+v", v)
	}
	end := v[1].ReviewID

	insert := func(review, revision, cause, event, status string) string {
		return `INSERT INTO brant_ful.shipment_review (tenant_id, company_id, review_id, order_id, order_revision, order_hash, order_version, cause,
			event_id, status, row_version, responsible_role, item_count, opened_by_kind, opened_by_id, open_epoch, opened_at)
			SELECT tenant_id, company_id, '` + review + `', order_id, ` + revision + `, order_hash, order_version + 9, '` + cause + `', '` + event +
			`', '` + status + `', 1, responsible_role, 0, opened_by_kind, opened_by_id, open_epoch, opened_at FROM brant_ful.shipment_review
			WHERE review_id = '` + end + `'; SET CONSTRAINTS ALL IMMEDIATE`
	}
	for _, c := range []struct{ what, sql, refusal string }{
		{"a second review of one order revision and cause", insert("shr.twice", "order_revision", "order_remainder_closed", "evt.twice", "open"),
			"duplicate key value"},
		{"a review inserted settled", insert("shr.settled", "9", "order_amended", "evt.settled", "settled"), "is inserted open"},
		{"a second open review of the order", insert("shr.open", "9", "order_amended", "evt.open", "open"), "exclusion constraint"},
		{"a disposition that did not happen", `UPDATE brant_ful.shipment_review_item SET disposition = 'shipment_cancelled', disposition_at = now(),
			disposition_reason = 'x' WHERE review_id = '` + end + `' AND item_no = 1`, "which did not happen"},
		{"an item's facts changed", `UPDATE brant_ful.shipment_review_item SET facts = '{}', facts_sha256 = encode(sha256('{}'::bytea), 'hex')
			WHERE review_id = '` + end + `' AND item_no = 1`, "changes only by its disposition"},
		{"a superseded review reopened", `UPDATE brant_ful.shipment_review SET status = 'open', row_version = 1, superseded_by = NULL,
			superseded_at = NULL WHERE review_id = '` + id + `'`, "changes only once"},
		{"an item deleted", `DELETE FROM brant_ful.shipment_review_item WHERE review_id = '` + id + `'`, "never deleted"},
		{"a review deleted", `DELETE FROM brant_ful.shipment_review WHERE review_id = '` + id + `'`, "never deleted"},
	} {
		if err := r.asOwner(c.sql); err == nil || !strings.Contains(err.Error(), c.refusal) {
			t.Fatalf("the database on %s: %v, want %q", c.what, err, c.refusal)
		}
	}
}

// The review's reads and settlement over HTTP with OIDC: the merchandiser lists the order's reviews,
// reads one, settles it — a disposition not made is 422, a stale version 409 — and reconciles the
// settlement; a sales clerk (no shipment_review.settle) settling is 403, an unknown review 404.
func TestShipmentReviewOverHTTP(t *testing.T) {
	w := newFulWorld(t)
	r := fulReviewOn(t, w, nil)
	order := w.order.ObjectID
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	_, err := w.prepare(w.merch, "shipment.http", "60", d, rel)
	w.must(err)
	r.amendedTo("amendment.http", "60")
	r.relayTRD()
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	merch := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "merch.1", "human")}
	sales := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "sales.http", "human"), n: 300}
	w.assign("sales.http", "sales_clerk")

	status, list := merch.do("GET", "/api/brant/v1/ful/orders/"+order+"/shipment_reviews", nil)
	reviews, _ := list["shipment_reviews"].([]any)
	if status != http.StatusOK || list["order_id"] != order || len(reviews) != 1 {
		t.Fatalf("list %d %v", status, list)
	}
	id := reviews[0].(map[string]any)["review_id"].(string)
	status, v := merch.do("GET", "/api/brant/v1/ful/shipment_reviews/"+id, nil)
	items, _ := v["items"].([]any)
	if status != http.StatusOK || v["status"] != "open" || v["cause"] != "order_amended" || v["responsible_role"] != "merchandiser" || len(items) != 1 ||
		items[0].(map[string]any)["kind"] != "shipment" || items[0].(map[string]any)["shipment_id"] != "shipment.http" {
		t.Fatalf("review %d %v", status, v)
	}
	args := map[string]any{"review_id": id, "expected_version": 1, "items": []any{
		map[string]any{"item_no": 1, "disposition": "shipment_cancelled", "reason": "合成：按修订 2 取消"}}}
	if status, out := sales.try("ful", fulapi.Contract, fulapi.CommandSettleShipmentReview, args); status != http.StatusForbidden ||
		out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("a sales clerk settling %d %v", status, out)
	}
	status, out := merch.try("ful", fulapi.Contract, fulapi.CommandSettleShipmentReview, args)
	if status != http.StatusUnprocessableEntity || out["code"] != kernel.CodeInvalidState {
		t.Fatalf("a shipment not cancelled %d %v", status, out)
	}
	merch.command("ful", fulapi.Contract, fulapi.CommandCancelShipment, map[string]any{"shipment_id": "shipment.http", "expected_version": 1,
		"reason": "合成：订单减量"})
	stale := map[string]any{"review_id": id, "expected_version": 2, "items": args["items"]}
	if status, out = merch.try("ful", fulapi.Contract, fulapi.CommandSettleShipmentReview, stale); status != http.StatusConflict ||
		out["code"] != kernel.CodeVersionConflict {
		t.Fatalf("stale %d %v", status, out)
	}
	settle := map[string]any{"contract": fulapi.Contract, "command": fulapi.CommandSettleShipmentReview, "idempotency_key": "ful.http.settle",
		"args": args}
	status, s := merch.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandSettleShipmentReview, settle)
	if status != http.StatusOK || s["status"] != "settled" || s["row_version"] != float64(2) || s["object_id"] != id {
		t.Fatalf("settle %d %v", status, s)
	}
	status, rec := merch.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandSettleShipmentReview+"/reconcile", settle)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != s["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	status, v = merch.do("GET", "/api/brant/v1/ful/shipment_reviews/"+id, nil)
	items, _ = v["items"].([]any)
	if status != http.StatusOK || v["status"] != "settled" || len(items) != 1 ||
		items[0].(map[string]any)["disposition"].(map[string]any)["disposition"] != "shipment_cancelled" {
		t.Fatalf("settled review %d %v", status, v)
	}
	if status, out = merch.do("GET", "/api/brant/v1/ful/shipment_reviews/shr.none", nil); status != http.StatusNotFound || out["code"] != kernel.CodeNotFound {
		t.Fatalf("no such review %d %v", status, out)
	}
}
