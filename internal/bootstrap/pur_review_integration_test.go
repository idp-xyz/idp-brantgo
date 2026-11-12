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
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// PUR follows an order change (WBS S5-09b, PUR part; tactical model PUR-AGG-DemandAllocation entity
// AllocationReview, SM-PUR-ALLOCATION-REVIEW; WF-06 step 4, its idempotency and failure mode; event
// storming PL-09, ES-09 steps 5 and 6; S5 decisions D-18, D-21; BT10; SIM-02 steps 7 and 9) on
// PostgreSQL 16: TRD's real orders of the shared order world, their own Outbox events relayed through
// the assembled relay into the dispatcher of every composed context (S4-06a), PUR's consumer
// pur.allocation_review opening the reviews as its system actor, and the purchaser ("editor", a human
// with every permission) disposing with PUR's own commands and settling. The order's line L1 is
// 120 PCS of sku.1; offer.1 sells it with an MOQ of 100. Python V0.50 has no review: the tests cite
// design IDs.

var reviewActor = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.pur.allocation_review"}

// purReview is the order world with every composed consumer behind a throwaway outbox.
type purReview struct {
	*trdWorld
	ch    *purChange
	ev    *orderEvents
	subs  []messaging.Subscription
	sc    kernel.Scope
	merch context.Context
}

// newPurReview provisions every consumer's actor; wrap, when set, replaces pur.allocation_review's
// subscription in the dispatcher.
func newPurReview(t *testing.T, wrap func(messaging.Subscription) messaging.Subscription) *purReview {
	return purReviewOn(t, newOrderWorld(t), wrap)
}

// purReviewOn is newPurReview on order world q (FUL's world, for one).
func purReviewOn(t *testing.T, q *trdWorld, wrap func(messaging.Subscription) messaging.Subscription) *purReview {
	subs := q.c.Subscriptions()
	routed := slices.Clone(subs)
	for i, s := range routed {
		if s.Consumer == "pur.allocation_review" && wrap != nil {
			routed[i] = wrap(s)
		}
	}
	r := &purReview{trdWorld: q, ch: newPurChange(q.scenWorld, func() *pur.Module { return q.c.PUR }), ev: newOrderEvents(t, routed...), subs: routed,
		sc: q.scope("company.A")}
	if _, err := q.platform.ProvisionInbox(context.Background(), r.sc, subs); err != nil {
		t.Fatal(err)
	}
	r.merch = q.actorOf(kernel.ActorHuman, "merchandiser")
	return r
}

// relayTRD relays TRD's Outbox events not relayed before and returns them.
func (r *purReview) relayTRD() []eventing.Envelope {
	r.t.Helper()
	out := r.ev.copyFrom(r.sc, trdapi.Source)
	r.ev.drain(r.ev.relay(5))
	r.ev.published(out...)
	return out
}

func eventOf(es []eventing.Envelope, typ string) eventing.Envelope {
	for _, e := range es {
		if string(e.Type) == typ {
			return e
		}
	}
	return eventing.Envelope{}
}

// amended has amendment id of order applied, L1 to qty.
func (r *purReview) amended(order, id, qty string) {
	r.t.Helper()
	v := r.getOrder(order)
	d := r.approvedAmendment(order, id, v.OrderRevision, v.OrderHash, qty)
	_, err := r.c.TRD.ApplyAmendment(r.reviewer, r.applyCmd(order, id, d.RowVersion))
	r.must(err)
}

// draft prepares draft id of order: qty of L1 under offer.1 and an overbuy when given.
func (r *purReview) draft(id, order, qty, overbuy string) purapi.DraftReceipt {
	r.t.Helper()
	l := purLine(qty, purSource("offer.1"))
	if overbuy != "" {
		l.Overbuy = &purapi.Overbuy{Quantity: overbuy, Reason: "合成：凑足 MOQ 100"}
	}
	d, err := r.c.PUR.PrepareDraft(r.author, purapi.PrepareDraft{IdempotencyKey: r.key(), DraftID: id, OrderID: order, Lines: []purapi.DraftLine{l},
		SelectionReason: purReason})
	r.must(err)
	return d
}

// delivered records qty of commitment's L1 as the merchandiser and has the author verify it.
func (r *purReview) delivered(id, commitment, qty string) {
	r.t.Helper()
	_, err := r.ch.x.record(r.merch, id, commitment, qty)
	r.must(err)
	_, err = r.ch.x.verify(r.author, id, 1, qty)
	r.must(err)
}

// changedTo has change id of commitment at revision rev prepared (L1 to qty) by the author, approved
// by the reviewer acknowledging every deviation, and applied by the author.
func (r *purReview) changedTo(id, commitment string, rev int64, qty string) purapi.ChangeReceipt {
	r.t.Helper()
	p, err := r.ch.prepare(r.author, id, commitment, rev, changeLine(qty, "offer.1", ""))
	r.must(err)
	var acks []purapi.Acknowledgement
	for _, d := range r.ch.view(r.author, id).Deviations {
		acks = append(acks, purapi.Acknowledgement{OrderLineID: d.OrderLineID, Kind: d.Kind, Reason: "合成：随订单变更"})
	}
	_, err = r.ch.approve(r.reviewer, id, 1, p.ChangeHash, acks...)
	r.must(err)
	a, err := r.ch.apply(r.author, id, 2)
	r.must(err)
	return a
}

func (r *purReview) reviews(order string) []purapi.AllocationReviewView {
	r.t.Helper()
	v, err := r.c.PUR.GetAllocationReviews(r.author, order)
	r.must(err)
	return v
}

func (r *purReview) settle(ctx context.Context, id string, version int64, items ...purapi.ReviewItemDisposition) (purapi.Receipt, error) {
	return r.c.PUR.SettleAllocationReview(ctx, purapi.SettleAllocationReview{IdempotencyKey: r.key(), ReviewID: id, ExpectedVersion: version,
		Items: items, Reason: "合成：订单变更复核完成"})
}

func disposed(no int, kind, ref, reason string) purapi.ReviewItemDisposition {
	d := purapi.ReviewItemDisposition{ItemNo: no, Disposition: kind, Reason: reason}
	switch kind {
	case purapi.DispositionDraftVoided:
		d.DraftID = ref
	case purapi.DispositionCommitmentChanged:
		d.ChangeID = ref
	case purapi.DispositionCommitmentCancelled:
		d.CommitmentID = ref
	}
	return d
}

// itemsOf names a review's items: kind and object.
func itemsOf(v purapi.AllocationReviewView) string {
	var out []string
	for _, it := range v.Items {
		out = append(out, it.Kind+":"+it.OrderLineID+it.DraftID+"/"+it.CommitmentID)
	}
	return strings.Join(out, " ")
}

func lineOf(it purapi.ReviewItemView) string {
	if len(it.Lines) != 1 {
		return "lines " + strings.Repeat("?", len(it.Lines))
	}
	l := it.Lines[0]
	return strings.Join([]string{l.Impact, l.TargetQuantity, l.AllocatedQuantity, l.ExcessQuantity, l.PurchaseQuantity, l.VerifiedQuantity}, " ")
}

// BT10, PUR side ("部分已出运后减量…展示采购…影响并逐项复核；已发生部分保留；处置可追溯"), with SIM-02 step 7 and ES-09
// step 5: the order's L1 of 120 is bought 100 by commitment.bt10, issued, of which the supplier
// delivered 40, verified; 15 more (85 overbought for the MOQ) sit in a prepared draft. The customer
// reduces L1 to 80 (revision 2). TRD's own OrderAmended, relayed to pur.allocation_review, opens one
// review as the consumer's system actor for the purchaser: L1 allocated 115 above 80, the voidable
// draft, the commitment with what it buys and what was verified. Settling the draft as voided before
// it is, or the commitment by a change that does not exist, is refused; once the draft is voided and
// the commitment changed to 80 by a C11 change (revision 2), the review is settled by reference to
// both, the change's revision traceable from the review. The 40 verified stay; the allocation is 80
// of 80. A settled review is settled once.
func TestAllocationReviewFollowsAReducedOrderBT10(t *testing.T) {
	r := newPurReview(t, nil)
	order := r.activeOrder("quote.1").ObjectID
	d := r.draft("purchase.bt10", order, "100", "")
	r.ch.x.issued("commitment.bt10", d)
	r.delivered("delivery.40", "commitment.bt10", "40")
	r.draft("purchase.extra", order, "15", "85")
	r.relayTRD()
	if v := r.reviews(order); len(v) != 0 {
		t.Fatalf("an activation opens no review: %+v", v)
	}

	r.amended(order, "amendment.bt10", "80")
	amended := eventOf(r.relayTRD(), trdapi.EventOrderAmended)
	v := r.reviews(order)
	if amended.ID == "" || len(v) != 1 {
		t.Fatalf("after OrderAmended %s: %d reviews", amended.ID, len(v))
	}
	rv := v[0]
	if rv.Status != purapi.ReviewOpen || rv.RowVersion != 1 || rv.Cause != purapi.CauseOrderAmended || rv.OrderRevision != 2 ||
		rv.EventID != string(amended.ID) || rv.OrderVersion != r.orderVersion(order) || rv.OrderHash != r.getOrder(order).OrderHash ||
		rv.ResponsibleRole != purapi.ResponsibleRole || rv.OpenedBy != reviewActor ||
		itemsOf(rv) != "allocation:L1/ draft:purchase.extra/ commitment:purchase.bt10/commitment.bt10" {
		t.Fatalf("the review %+v: %s", rv, itemsOf(rv))
	}
	if got := lineOf(rv.Items[0]); got != "reduced 80 115 35  " || len(rv.Items[0].Holders) != 2 ||
		rv.Items[0].Holders[0].CommitmentID != "commitment.bt10" || rv.Items[0].Holders[0].CommitmentRevision != 1 {
		t.Fatalf("allocation item %s %+v", got, rv.Items[0])
	}
	if got := lineOf(rv.Items[1]); got != "reduced 80 15  100 " || rv.Items[1].Status != "prepared" {
		t.Fatalf("draft item %s %+v", got, rv.Items[1])
	}
	if c := rv.Items[2]; lineOf(c) != "reduced 80 100  100 40" || c.CommitmentRevision != 1 || c.Status != purapi.CommitmentIssued || c.PONumber == "" ||
		!strings.Contains(c.Facts, `"verified_quantity":"40"`) {
		t.Fatalf("commitment item %s %+v", lineOf(c), c)
	}

	// Nothing is done yet: the dispositions cannot be claimed.
	_, err := r.settle(r.author, rv.ReviewID, 1, disposed(1, purapi.DispositionNoAction, "", "x"),
		disposed(2, purapi.DispositionDraftVoided, "purchase.extra", "作废"), disposed(3, purapi.DispositionNoAction, "", "x"))
	refusedAt(t, "a draft not voided", err, kernel.CodeInvalidState, "/args/items/1/draft_id")
	_, err = r.settle(r.author, rv.ReviewID, 1, disposed(1, purapi.DispositionNoAction, "", "x"), disposed(2, purapi.DispositionNoAction, "", "x"),
		disposed(3, purapi.DispositionCommitmentChanged, "change.none", "x"))
	refusedAt(t, "a change that does not exist", err, kernel.CodeReferenceInvalid, "/args/items/2/change_id")
	_, err = r.settle(r.author, rv.ReviewID, 1, disposed(1, purapi.DispositionNoAction, "", "x"))
	refusedAt(t, "items left out", err, kernel.CodeDraftNotReady, "/args/items")
	_, err = r.settle(r.actorOf(kernel.ActorAI, "agent.1"), rv.ReviewID, 1, disposed(1, purapi.DispositionNoAction, "", "x"),
		disposed(2, purapi.DispositionNoAction, "", "x"), disposed(3, purapi.DispositionNoAction, "", "x"))
	if code(err) != kernel.CodePermissionDenied {
		t.Fatalf("an AI agent settling: %s", code(err))
	}

	// The dispositions, with PUR's own commands.
	_, err = r.c.PUR.VoidDraft(r.author, purapi.VoidDraft{IdempotencyKey: r.key(), DraftID: "purchase.extra", ExpectedVersion: 1, Reason: "订单减量，草稿作废"})
	r.must(err)
	if ch := r.changedTo("change.bt10", "commitment.bt10", 1, "80"); ch.AppliedRevision != 2 {
		t.Fatalf("change %+v", ch)
	}
	cmd := purapi.SettleAllocationReview{IdempotencyKey: r.key(), ReviewID: rv.ReviewID, ExpectedVersion: 1, Items: []purapi.ReviewItemDisposition{
		disposed(1, purapi.DispositionCommitmentChanged, "change.bt10", "承诺减至 80，分配降至 80"),
		disposed(2, purapi.DispositionDraftVoided, "purchase.extra", "多余草稿已作废"),
		disposed(3, purapi.DispositionCommitmentChanged, "change.bt10", "供应商确认减至 80，已交 40 保留")}}
	s, err := r.c.PUR.SettleAllocationReview(r.author, cmd)
	r.must(err)
	if s.Status != purapi.ReviewSettled || s.RowVersion != 2 || s.ObjectID != rv.ReviewID {
		t.Fatalf("settled %+v", s)
	}
	got, err := r.c.PUR.GetAllocationReview(r.author, rv.ReviewID)
	r.must(err)
	c := got.Items[2].Disposition
	if got.Status != purapi.ReviewSettled || got.SettledBy.ID != "editor" || got.SettleEpoch == 0 || c == nil || c.ChangeID != "change.bt10" ||
		c.CommitmentID != "commitment.bt10" || c.Revision != 2 || c.At.IsZero() || got.Items[1].Disposition.DraftID != "purchase.extra" ||
		got.Items[1].Disposition.At.IsZero() {
		t.Fatalf("the settled review %+v", got)
	}
	if cv := r.ch.x.p.view(r.author, "commitment.bt10"); cv.Revision != 2 || cv.Revisions[0].ChangeID != c.ChangeID {
		t.Fatalf("the change the review names is the commitment's revision 2: %+v", cv)
	}
	if st := r.ch.x.standing(r.author, "commitment.bt10"); st != "80 40 40" {
		t.Fatalf("the verified 40 stay: %s", st)
	}
	al, err := r.c.PUR.GetAllocations(r.author, order)
	r.must(err)
	if len(al) != 1 || al[0].Demand != "80" || al[0].Allocated != "80" || al[0].OrderRevision != 2 {
		t.Fatalf("allocation %+v", al)
	}
	if again, err := r.c.PUR.SettleAllocationReview(r.author, cmd); err != nil || !again.Replayed {
		t.Fatalf("replay %+v %v", again, err)
	}
	cmd.IdempotencyKey = r.key()
	_, err = r.c.PUR.SettleAllocationReview(r.author, cmd)
	r.want(err, kernel.CodeVersionConflict)
	cmd.IdempotencyKey, cmd.ExpectedVersion = r.key(), 2
	_, err = r.c.PUR.SettleAllocationReview(r.author, cmd)
	refusedAt(t, "settled twice", err, kernel.CodeInvalidState, "/args/review_id")

	// A redelivery of the amendment, by this dispatcher and another, reopens nothing.
	second, err := messaging.NewDispatcher(r.subs...)
	r.must(err)
	for _, dp := range []*messaging.Dispatcher{r.ev.dispatcher, second} {
		r.must(dp.Publish(context.Background(), amended))
	}
	if v := r.reviews(order); len(v) != 1 || v[0].Status != purapi.ReviewSettled {
		t.Fatalf("after redeliveries %+v", v)
	}
}

// ES-09 steps 4 to 6 with TD-AT02's close_remainder and SIM-02 step 9, on FUL's real shipment (S5
// C4a: TRD's end reads FUL's ShipmentFacts v1 and PUR's DownstreamCommitments v1): commitment.es09
// bought 100 of L1, all delivered and verified; QLT released them and bootstrap's FUL shipped them on
// PUR's verified delivery (its commercial release granted, S5 C1). A prepared draft holds 15 more (85
// overbought for the MOQ). The customer reduces L1 to 110 (revision 2): OrderAmended opens the review
// of L1 (115 above 110), the draft and the fulfilled commitment. The remainder cannot be closed while
// the draft is prepared (DOWNSTREAM_COMMITMENT_OPEN, TRD-INV-019). The purchaser voids the draft and
// settles the review by reference to the void, the fulfilled commitment needing nothing (ES-09 step
// 5); then the remainder of 10 closes short (step 6). TRD's OrderRemainderClosed finds nothing above
// what shipped and no open review: it opens nothing.
func TestAllocationReviewOfAnOrderClosedShortES09(t *testing.T) {
	w := newFulWorld(t)
	r := purReviewOn(t, w.trdWorld, nil)
	order := w.order.ObjectID
	d := r.draft("purchase.es09", order, "100", "")
	r.ch.x.issued("commitment.es09", d)
	r.delivered("delivery.100", "commitment.es09", "100")
	r.draft("purchase.rest", order, "15", "85")
	ref := qltapi.SubjectRef{Kind: qltapi.SubjectCommitmentLine, ID: "commitment.es09", LineID: "L1"}
	_, err := w.c.QLT.RecordInspection(w.qc, qltapi.RecordInspection{IdempotencyKey: w.key(), InspectionID: "insp.es09", Subject: ref,
		InspectedQuantity: "100", ConformingQuantity: "100", NonconformingQuantity: "0", Sampling: qltapi.Sampling{Method: qltapi.SamplingFull},
		Standard: qltapi.StandardRef{SourceID: "standard"}, Inspector: qltapi.Inspector{Kind: qltapi.InspectorInternal}, ReportFileID: "report",
		InspectedAt: kernel.FormatTimestamp(time.Now().Add(-time.Hour))})
	w.must(err)
	_, err = w.c.QLT.GrantRelease(w.qc, qltapi.GrantRelease{IdempotencyKey: w.key(), ReleaseID: "rel.es09", Subject: ref, Quantity: "100",
		ValidUntil: kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))})
	w.must(err)
	_, err = w.c.FUL.PrepareShipment(w.merch, w.prepareCmd("shipment.es09", "100", fulapi.DeliveryLineRef{DeliveryID: "delivery.100", LineID: "L1",
		Version: 1}, "rel.es09"))
	w.must(err)
	w.grantedOn(w.c.FUL, "shipment.es09", fulapi.ScopeShipment)
	_, err = w.c.FUL.ConfirmShipment(w.merch, w.confirmCmd("shipment.es09", 1))
	w.must(err)
	r.relayTRD()

	r.amended(order, "amendment.es09", "110")
	r.relayTRD()
	reviews := r.reviews(order)
	if len(reviews) != 1 {
		t.Fatalf("%d reviews", len(reviews))
	}
	rv := reviews[0]
	if rv.Cause != purapi.CauseOrderAmended || rv.OrderRevision != 2 ||
		itemsOf(rv) != "allocation:L1/ draft:purchase.rest/ commitment:purchase.es09/commitment.es09" ||
		lineOf(rv.Items[0]) != "reduced 110 115 5  " || !rv.Items[2].Fulfilled || lineOf(rv.Items[2]) != "reduced 110 100  100 100" {
		t.Fatalf("review %+v: %s", rv, itemsOf(rv))
	}
	_, err = r.endShort(order)
	wantRefused(t, "close the remainder while the draft is prepared", err, trdapi.CodeDownstreamCommitmentOpen, "/args/order_id")
	if why := reasonOf(err); !strings.Contains(why, "purchase_draft purchase.rest is prepared") ||
		!strings.Contains(why, "demand_allocation L1 allocates 115 of line L1, which shipped 100") {
		t.Fatalf("the refusal names the draft and the allocation: %s", why)
	}

	_, err = r.c.PUR.VoidDraft(r.author, purapi.VoidDraft{IdempotencyKey: r.key(), DraftID: "purchase.rest", ExpectedVersion: 1, Reason: "余量短交关闭"})
	r.must(err)
	_, err = r.settle(r.author, rv.ReviewID, 1, disposed(1, purapi.DispositionDraftVoided, "purchase.rest", "余量草稿作废"),
		disposed(2, purapi.DispositionDraftVoided, "purchase.rest", "余量草稿作废"), disposed(3, purapi.DispositionNoAction, "", "工厂已全部交付，已出运 100"))
	r.must(err)
	if got := r.reviews(order)[0]; got.Status != purapi.ReviewSettled || got.Items[2].Disposition.Disposition != purapi.DispositionNoAction {
		t.Fatalf("settled %+v", got)
	}

	_, err = r.endShort(order)
	r.must(err)
	if line, end := r.ended(order); line != "110 100 10" || end.DownstreamSource != trdapi.GuardDownstream {
		t.Fatalf("closed short %s %+v", line, end)
	}
	if closed := eventOf(r.relayTRD(), trdapi.EventOrderRemainderClosed); closed.ID == "" {
		t.Fatal("no OrderRemainderClosed relayed")
	}
	if v := r.reviews(order); len(v) != 1 || v[0].ReviewID != rv.ReviewID || v[0].Status != purapi.ReviewSettled {
		t.Fatalf("after OrderRemainderClosed %+v", v)
	}
}

// WF-06 "更晚修订的复核取代未处置的旧复核" and an order's end (TD-AT02 cancel of an unshipped order): a
// prepared draft of 100 on L1; the order amended to 90 opens review r2 (L1 and the draft). While the
// draft is prepared the order cannot be cancelled (DOWNSTREAM_COMMITMENT_OPEN, TRD-INV-019; S5 C4a).
// The purchaser voids the draft — its allocation released — but leaves r2 open; the order is
// cancelled. TRD's OrderCancelled finds nothing PUR still holds above what the order needs, but r2 is
// open: the cancellation's review opens without items and supersedes it (settling r2 is
// REFERENCE_STALE). A review without items is settled with a reason only.
func TestAllocationReviewOfACancelledOrderSupersedesTheAmendedOne(t *testing.T) {
	r := newPurReview(t, nil)
	order := r.activeOrder("quote.1").ObjectID
	r.draft("purchase.c", order, "100", "")
	r.amended(order, "amendment.90", "90")
	r.relayTRD()
	first := r.reviews(order)
	if len(first) != 1 || itemsOf(first[0]) != "allocation:L1/ draft:purchase.c/" || lineOf(first[0].Items[0]) != "reduced 90 100 10  " {
		t.Fatalf("review of revision 2 %+v", first)
	}
	_, err := r.endCancel(order)
	wantRefused(t, "cancel with the draft prepared", err, trdapi.CodeDownstreamCommitmentOpen, "/args/order_id")
	_, err = r.c.PUR.VoidDraft(r.author, purapi.VoidDraft{IdempotencyKey: r.key(), DraftID: "purchase.c", ExpectedVersion: 1, Reason: "订单将取消"})
	r.must(err)
	_, err = r.endCancel(order)
	r.must(err)
	cancelled := eventOf(r.relayTRD(), trdapi.EventOrderCancelled)
	all := r.reviews(order)
	if cancelled.ID == "" || len(all) != 2 {
		t.Fatalf("after OrderCancelled %s: %+v", cancelled.ID, all)
	}
	old, rv := all[0], all[1]
	if old.Status != purapi.ReviewSuperseded || old.SupersededBy != rv.ReviewID || old.RowVersion != 2 || old.SupersededAt.IsZero() ||
		rv.Status != purapi.ReviewOpen || rv.Cause != purapi.CauseOrderCancelled || rv.OrderRevision != 2 || rv.OrderVersion <= old.OrderVersion ||
		rv.EventID != string(cancelled.ID) || len(rv.Items) != 0 {
		t.Fatalf("superseded %+v by %+v", old, rv)
	}
	_, err = r.settle(r.author, old.ReviewID, 2, disposed(1, purapi.DispositionDraftVoided, "purchase.c", "x"),
		disposed(2, purapi.DispositionDraftVoided, "purchase.c", "x"))
	refusedAt(t, "a superseded review", err, purapi.CodeReferenceStale, "/args/review_id")
	_, err = r.c.PUR.SettleAllocationReview(r.author, purapi.SettleAllocationReview{IdempotencyKey: r.key(), ReviewID: rv.ReviewID, ExpectedVersion: 1})
	refusedAt(t, "a review without items settled without a reason", err, kernel.CodeSchemaInvalid, "/args/reason")
	s, err := r.settle(r.author, rv.ReviewID, 1)
	r.must(err)
	if got, err := r.c.PUR.GetAllocationReview(r.author, rv.ReviewID); err != nil || s.Status != purapi.ReviewSettled || got.Status != purapi.ReviewSettled ||
		got.SettleReason == "" {
		t.Fatalf("settled %+v %+v %v", s, got, err)
	}
	if n := r.count("company.A", "brant_pur.allocation_review"); n != 2 {
		t.Fatalf("%d reviews", n)
	}
}

// WF-06 "事件 ID 去重；各下游按（订单、订单修订）只开启一次复核", ADR-BR-015 and S4-06a (a dead-letter replay
// arrives after the later events of its aggregate): with 110 of L1 drafted, a reduction to 115
// (revision 2) touches nothing and opens nothing; the reduction to 100 (revision 3) is dead-lettered
// while the consumer is broken; the reduction to 90
// (revision 4) opens its review; the replayed revision 3 opens nothing (TRD is at revision 4). That
// review's OrderAmended published again by two workers at once, besides the relay, stays one review;
// the database refuses a second review of the revision and a settlement whose disposition did not
// happen.
func TestAllocationReviewOpensOncePerOrderRevision(t *testing.T) {
	var broken atomic.Bool
	r := newPurReview(t, func(s messaging.Subscription) messaging.Subscription { return breakable(s, &broken, true) })
	order := r.activeOrder("quote.1").ObjectID
	r.draft("purchase.once", order, "110", "")
	r.amended(order, "amendment.115", "115")
	if e := eventOf(r.relayTRD(), trdapi.EventOrderAmended); e.ID == "" || len(r.reviews(order)) != 0 {
		t.Fatalf("an amendment that touches nothing: %+v", r.reviews(order))
	}

	r.amended(order, "amendment.100", "100")
	lost := r.ev.copyFrom(r.sc, trdapi.Source)
	dead := eventOf(lost, trdapi.EventOrderAmended)
	broken.Store(true)
	r.ev.drain(r.ev.relay(1))
	broken.Store(false)
	if s := r.ev.status(dead.ID); s != "ABANDONED" || len(r.reviews(order)) != 0 {
		t.Fatalf("broken consumer: outbox %s, reviews %+v", s, r.reviews(order))
	}
	r.amended(order, "amendment.90", "90")
	later := eventOf(r.relayTRD(), trdapi.EventOrderAmended)
	v := r.reviews(order)
	if len(v) != 1 || v[0].OrderRevision != 4 || v[0].EventID != string(later.ID) || lineOf(v[0].Items[0]) != "reduced 90 110 20  " {
		t.Fatalf("revision 4's review %+v", v)
	}
	operator := r.provision(bootstrap.Grant{Scope: r.sc, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"},
		Permissions: []string{messaging.ReplayPermission}})
	if _, err := r.ev.replayer().Replay(operator, dead.Source, string(dead.ID)); err != nil {
		t.Fatalf("replay of revision 3: %v", err)
	}
	if v := r.reviews(order); len(v) != 1 || v[0].OrderRevision != 4 || v[0].Status != purapi.ReviewOpen {
		t.Fatalf("after the late replay %+v", v)
	}

	second, err := messaging.NewDispatcher(r.subs...)
	r.must(err)
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
	r.must(errors.Join(errs...))
	if v := r.reviews(order); len(v) != 1 || v[0].ReviewID == "" || len(v[0].Items) != 2 {
		t.Fatalf("after two workers %+v", v)
	}
	id := v[0].ReviewID
	if err := r.asOwner(`INSERT INTO brant_pur.allocation_review (tenant_id, company_id, review_id, order_id, order_revision, order_hash, order_version, cause,
		event_id, status, row_version, responsible_role, item_count, opened_by_kind, opened_by_id, open_epoch, opened_at)
		SELECT tenant_id, company_id, 'alr.twice', order_id, order_revision, order_hash, order_version + 1, cause, 'evt.twice', 'open', 1,
		responsible_role, 0, opened_by_kind, opened_by_id, open_epoch, opened_at FROM brant_pur.allocation_review WHERE review_id = '` + id + `'`); err == nil {
		t.Fatal("a second review of one order revision and cause")
	}
	if err := r.asOwner(`UPDATE brant_pur.allocation_review_item SET disposition = 'draft_voided', disposition_draft_id = 'purchase.once', disposition_at = now(),
		disposition_reason = 'x' WHERE review_id = '` + id + `' AND item_no = 2`); err == nil {
		t.Fatal("the database disposed of a draft that is not voided")
	}
}

// The review's reads and settlement over HTTP with OIDC: the purchaser lists the order's reviews,
// reads one, settles it with a no_action and a void, and reconciles the settlement; a stale version
// is 409, a disposition not made 422, an unknown review 404.
func TestAllocationReviewOverHTTP(t *testing.T) {
	r := newPurReview(t, nil)
	order := r.activeOrder("quote.1").ObjectID
	r.draft("purchase.http", order, "100", "")
	r.amended(order, "amendment.http", "60")
	r.relayTRD()
	is := newIssuer(t)
	handler, err := r.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	r.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ed := &client{t: t, base: srv.URL, token: is.token(r.tenant, "company.A", "editor", "human")}

	status, list := ed.do("GET", "/api/brant/v1/pur/orders/"+order+"/allocation_reviews", nil)
	reviews, _ := list["allocation_reviews"].([]any)
	if status != http.StatusOK || list["order_id"] != order || len(reviews) != 1 {
		t.Fatalf("list %d %v", status, list)
	}
	id := reviews[0].(map[string]any)["review_id"].(string)
	status, v := ed.do("GET", "/api/brant/v1/pur/allocation_reviews/"+id, nil)
	items, _ := v["items"].([]any)
	if status != http.StatusOK || v["status"] != "open" || v["cause"] != "order_amended" || v["responsible_role"] != "purchaser" || len(items) != 2 ||
		items[0].(map[string]any)["kind"] != "allocation" || items[1].(map[string]any)["draft_id"] != "purchase.http" {
		t.Fatalf("review %d %v", status, v)
	}
	args := map[string]any{"review_id": id, "expected_version": 1, "items": []any{
		map[string]any{"item_no": 1, "disposition": "no_action", "reason": "合成：等待作废"},
		map[string]any{"item_no": 2, "disposition": "draft_voided", "draft_id": "purchase.http", "reason": "合成：作废"}}}
	status, out := ed.try("pur", purapi.Contract, purapi.CommandSettleAllocationReview, args)
	if status != http.StatusUnprocessableEntity || out["code"] != kernel.CodeInvalidState {
		t.Fatalf("a draft not voided %d %v", status, out)
	}
	ed.command("pur", purapi.Contract, purapi.CommandVoidDraft, map[string]any{"draft_id": "purchase.http", "expected_version": 1, "reason": "合成：订单减量"})
	stale := map[string]any{"review_id": id, "expected_version": 2, "items": args["items"]}
	if status, out = ed.try("pur", purapi.Contract, purapi.CommandSettleAllocationReview, stale); status != http.StatusConflict ||
		out["code"] != kernel.CodeVersionConflict {
		t.Fatalf("stale %d %v", status, out)
	}
	settle := map[string]any{"contract": purapi.Contract, "command": purapi.CommandSettleAllocationReview, "idempotency_key": "pur.http.settle",
		"args": args}
	status, s := ed.do("POST", "/api/brant/v1/pur/commands/"+purapi.CommandSettleAllocationReview, settle)
	if status != http.StatusOK || s["status"] != "settled" || s["row_version"] != float64(2) || s["object_id"] != id {
		t.Fatalf("settle %d %v", status, s)
	}
	status, rec := ed.do("POST", "/api/brant/v1/pur/commands/"+purapi.CommandSettleAllocationReview+"/reconcile", settle)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != s["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	status, v = ed.do("GET", "/api/brant/v1/pur/allocation_reviews/"+id, nil)
	items, _ = v["items"].([]any)
	if status != http.StatusOK || v["status"] != "settled" || len(items) != 2 ||
		items[1].(map[string]any)["disposition"].(map[string]any)["draft_id"] != "purchase.http" {
		t.Fatalf("settled review %d %v", status, v)
	}
	if status, out = ed.do("GET", "/api/brant/v1/pur/allocation_reviews/alr.none", nil); status != http.StatusNotFound || out["code"] != kernel.CodeNotFound {
		t.Fatalf("no such review %d %v", status, out)
	}
}
