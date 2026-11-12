//go:build integration

package bootstrap_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S5-09a on PostgreSQL 16: TRD's order end and amendments read what FUL shipped and what PUR
// committed (TRD-INV-018, TRD-INV-019; SM-TRD-ORDER end transitions; WF-06; ES-09; TD-AT01, TD-AT02;
// Context Map R70 ShipmentFacts v1, R71 DownstreamCommitments v1; S5 decisions D-1, D-10, D-19).
// Python V0.50 has no fulfilment, purchasing commitment or order end: the tests cite the design.
//
// The worlds are FUL's (newFulWorld: TRD's order world with order.quote.1 active for 120 PCS on line
// L1, real FUL shipments confirmed on QLT releases and an in-memory PUR delivery port, nothing in PUR)
// and FUL's on PUR's real deliveries (newFulPurWorld: commitment.1 issued for all 120). TRD is the
// composed process's (q.c.TRD), so its reads run brant_ful.order_shipped_v1 and
// brant_pur.order_commitments_v1 with TRD's executor inside its transaction.

// confirmed has merch.1 prepare shipment id of quantity of L1 on delivery d and release rel, the
// documentation clerk have its commercial release granted (S5-07) and merch.1 confirm it, on FUL
// module w.ful.
func (w *fulWorld) confirmed(id, quantity string, d fulapi.DeliveryLineRef, rel string) {
	w.t.Helper()
	_, err := w.prepare(w.merch, id, quantity, d, rel)
	w.must(err)
	w.granted(id)
	_, err = w.confirm(w.merch, id, 1)
	w.must(err)
}

func (q *trdWorld) endCancel(id string) (trdapi.OrderReceipt, error) {
	return q.c.TRD.CancelOrder(q.author, trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: id, ExpectedVersion: q.orderVersion(id),
		Reason: "客户撤单", Evidence: q.proof})
}

func (q *trdWorld) endShort(id string) (trdapi.OrderReceipt, error) {
	return q.c.TRD.CloseOrderRemainder(q.author, trdapi.CloseOrderRemainder{IdempotencyKey: q.key(), OrderID: id,
		ExpectedVersion: q.orderVersion(id), Reason: "客户同意短交", Evidence: q.proof})
}

func (q *trdWorld) endClose(id string) (trdapi.OrderReceipt, error) {
	return q.c.TRD.CloseOrder(q.author, trdapi.CloseOrder{IdempotencyKey: q.key(), OrderID: id, ExpectedVersion: q.orderVersion(id)})
}

// ended returns the order's end line L1 as "ordered shipped remainder", with the end's sources.
func (q *trdWorld) ended(id string) (string, *trdapi.OrderEndView) {
	q.t.Helper()
	v := q.getOrder(id)
	if v.End == nil || len(v.End.Lines) != 1 {
		q.t.Fatalf("the order's end %+v", v.End)
	}
	l := v.End.Lines[0]
	return l.Ordered + " " + l.Shipped + " " + l.Remainder, v.End
}

// endEvent returns the payload of the last event of eventType.
func (q *trdWorld) endEvent(eventType string) map[string]any {
	q.t.Helper()
	evs := q.eventsOf(eventType)
	if len(evs) == 0 {
		q.t.Fatalf("no %s", eventType)
	}
	return evs[len(evs)-1].Payload
}

// TD-AT01 with FUL's facts (TRD-INV-018, SM-TRD-ORDER amendment.apply "数量不低于已确认出运量", WF-06 step
// 3): an amendment of L1 to 36 PCS was approved before FUL confirmed 40 PCS of it; applying it now is
// QUANTITY_BELOW_SHIPPED (the revision it would propose already says so), and preparing 35 is
// refused at prepare. An amendment to 44 applies: revision 2, its commit evidence names
// ful.shipment_facts.v1 with what shipped and stk.stock_reservations.v1 not_enabled, OrderAmended
// names the source and no unchecked guard.
func TestAmendmentAgainstFULShipmentsTDAT01(t *testing.T) {
	w := newFulWorld(t)
	q := w.trdWorld
	id := w.order.ObjectID
	v := q.getOrder(id)
	below := q.approvedAmendment(id, "amendment.36", 1, v.OrderHash, "36")
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "40", d, rel)

	if p := q.snapshots(q.author, id)[id].Proposed; len(p) != 1 || p[0].AmendmentID != "amendment.36" || p[0].Code != trdapi.CodeQuantityBelowShipped {
		t.Fatalf("the revision amendment.36 proposes: %+v", p)
	}
	_, err := q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(id, "amendment.36", below.RowVersion))
	wantRefused(t, "36 of 40 shipped at apply", err, trdapi.CodeQuantityBelowShipped, "/proposal/lines/0/quantity")
	_, err = q.c.TRD.PrepareAmendment(q.author, q.amendCmd(id, "amendment.35", 0, 1, v.OrderHash, "35"))
	wantRefused(t, "35 of 40 shipped at prepare", err, trdapi.CodeQuantityBelowShipped, "/args/lines/0/quantity")

	more := q.approvedAmendment(id, "amendment.44", 1, v.OrderHash, "44")
	r, err := q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(id, "amendment.44", more.RowVersion))
	q.must(err)
	after := q.getOrder(id)
	if r.OrderRevision != 2 || len(after.AmendmentCommits) != 1 || after.Status != trdapi.OrderActive {
		t.Fatalf("applied %+v %+v", r, after)
	}
	e := after.AmendmentCommits[0].Entries
	if e["fulfilment_source"] != trdapi.GuardFULShipments || e["shipped"] != "L1=40" || e[trdapi.GuardSTKReservations] != trdapi.NotEnabled ||
		e["amendment_id"] != "amendment.44" {
		t.Fatalf("the commit evidence %v", e)
	}
	amended := q.endEvent(trdapi.EventOrderAmended)
	if amended["fulfilment_source"] != trdapi.GuardFULShipments || amended["unchecked_guards"] != "" || amended["order_revision"] != float64(2) {
		t.Fatalf("OrderAmended %v", amended)
	}
	if lines, _ := amended["changed_lines"].([]any); len(lines) != 1 || lines[0].(map[string]any)["to_quantity"] != "44" {
		t.Fatalf("OrderAmended's lines %v", amended["changed_lines"])
	}
	facts, err := q.c.TRD.ResolveActiveOrders(q.author, []string{id})
	q.must(err)
	if f := facts[id]; f.Binding.OrderRevision != 2 || f.Lines[0].Quantity != "44" {
		t.Fatalf("ActiveOrderRef %+v", f)
	}
	// FUL ships against revision 2 up to its 44 (FUL-INV-001), and TRD still counts every revision;
	// revision 2 is releasable once compliance cleared it and STL's account moved to it.
	w.amendedReleasable(id)
	_, err = w.prepare(w.merch, "shipment.2", "5", d, rel)
	w.want(err, fulapi.CodeShipmentQuantityExceeded)
	w.confirmed("shipment.3", "4", d, rel)
	if _, err := q.endClose(id); err != nil {
		t.Fatalf("close after 44 of 44 shipped over two revisions: %v", err)
	}
	if line, end := q.ended(id); line != "44 44 0" || end.FulfilmentSource != trdapi.GuardFULShipments || end.OrderRevision != 2 {
		t.Fatalf("the end %s %+v", line, end)
	}
}

// TD-AT02 with FUL's facts (TRD-INV-019, SM-TRD-ORDER active → cancelled | closed): FUL confirmed 60
// of the 120 PCS. order.cancel is ORDER_HAS_SHIPMENTS, order.close ORDER_NOT_FULFILLED; the remainder
// closes short (PUR lists nothing against the order): the end keeps 120 ordered, 60 shipped and 60
// given up, names ful.shipment_facts.v1 and pur.downstream_commitments.v1, OrderRemainderClosed
// records STK as not_enabled, and the conversion slot stays taken. FUL can no longer ship.
func TestOrderEndsOnFULShipmentsTDAT02(t *testing.T) {
	w := newFulWorld(t)
	q := w.trdWorld
	id := w.order.ObjectID
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "60", d, rel)

	_, err := q.endCancel(id)
	wantRefused(t, "cancel after 60 shipped", err, trdapi.CodeOrderHasShipments, "/args/order_id")
	_, err = q.endClose(id)
	wantRefused(t, "close at 60 of 120", err, trdapi.CodeOrderNotFulfilled, "/args/order_id")
	r, err := q.endShort(id)
	q.must(err)
	line, end := q.ended(id)
	if r.Status != trdapi.OrderClosed || line != "120 60 60" || end.Command != trdapi.CommandCloseOrderRemainder ||
		end.FulfilmentSource != trdapi.GuardFULShipments || end.DownstreamSource != trdapi.GuardDownstream || end.Reason != "客户同意短交" {
		t.Fatalf("closed short %+v: %s %+v", r, line, end)
	}
	ev := q.endEvent(trdapi.EventOrderRemainderClosed)
	if ev["fulfilment_source"] != trdapi.GuardFULShipments || ev["downstream_source"] != trdapi.GuardDownstream ||
		ev["stock_reservations"] != trdapi.NotEnabled || ev["downstream_settled"] != "" || ev["conversion_slot"] != "full_v1" {
		t.Fatalf("OrderRemainderClosed %v", ev)
	}
	if quote := q.getQuote("quote.1"); quote.Conversion == nil {
		t.Fatal("the conversion slot after closing short")
	}
	_, err = w.prepare(w.merch, "shipment.2", "1", d, rel)
	w.want(err, fulapi.CodeOrderNotActive)
}

// ES-09 step 6 with FUL's facts: 116 of the 120 PCS shipped in two confirmed shipments. order.close
// is ORDER_NOT_FULFILLED (no tolerance is approved: zero, D-19); the last 4 PCS are closed short.
func TestES09ClosesTheLastFourPiecesShort(t *testing.T) {
	w := newFulWorld(t)
	q := w.trdWorld
	id := w.order.ObjectID
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "100", d, rel)
	w.confirmed("shipment.2", "16", d, rel)
	_, err := q.endClose(id)
	wantRefused(t, "close at 116 of 120", err, trdapi.CodeOrderNotFulfilled, "/args/order_id")
	_, err = q.endShort(id)
	q.must(err)
	if line, end := q.ended(id); line != "120 116 4" || end.Status != trdapi.OrderClosed {
		t.Fatalf("ES-09's short closing %s %+v", line, end)
	}
}

// SM-TRD-ORDER order.close with FUL's facts ("已确认出运量等于订单量"): once FUL confirmed all 120 PCS the
// remainder cannot be closed short (INVALID_STATE: nothing is left), and the order closes; order.close
// reads no downstream commitment (downstream_source not_required).
func TestOrderClosesWhenEverythingShipped(t *testing.T) {
	w := newFulWorld(t)
	q := w.trdWorld
	id := w.order.ObjectID
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "116", d, rel)
	_, err := q.endClose(id)
	wantRefused(t, "close at 116 of 120", err, trdapi.CodeOrderNotFulfilled, "/args/order_id")
	w.confirmed("shipment.2", "4", d, rel)
	_, err = q.endShort(id)
	wantRefused(t, "close the remainder of nothing", err, "INVALID_STATE", "/args/order_id")
	r, err := q.endClose(id)
	q.must(err)
	line, end := q.ended(id)
	if r.Status != trdapi.OrderClosed || line != "120 120 0" || end.Command != trdapi.CommandCloseOrder ||
		end.FulfilmentSource != trdapi.GuardFULShipments || end.DownstreamSource != trdapi.GuardNotRequired {
		t.Fatalf("closed %+v: %s %+v", r, line, end)
	}
	if ev := q.endEvent(trdapi.EventOrderClosed); ev["downstream_source"] != trdapi.GuardNotRequired || ev["end_command"] != trdapi.CommandCloseOrder {
		t.Fatalf("OrderClosed %v", ev)
	}
}

// TRD-INV-019 with PUR's facts ("有未解除承诺返回 DOWNSTREAM_COMMITMENT_OPEN", R71's risk 有未处置承诺仍取消订单),
// and WF-06 step 1: while PUR's draft of all 120 PCS is prepared, order.cancel is
// DOWNSTREAM_COMMITMENT_OPEN, and an amendment the editor prepares with only materials and
// operations assessed gets the purchasing, quality, packing, booking and payment_plan areas
// pre-filled as open items with what PUR, FUL and STL said. Once the draft is voided (its
// allocation released) nothing is open and the order is cancelled.
func TestOrderCancelWaitsForPURDraftAndPrefillsTheImpact(t *testing.T) {
	q := newOrderWorld(t)
	o := q.activeOrder("quote.1")
	id := o.ObjectID
	_, err := q.c.PUR.PrepareDraft(q.author, purapi.PrepareDraft{IdempotencyKey: q.key(), DraftID: "purchase.1", OrderID: id,
		Lines: []purapi.DraftLine{purLine("120", purSource("offer.1"))}, SelectionReason: purReason})
	q.must(err)

	_, err = q.endCancel(id)
	wantRefused(t, "cancel with a prepared draft", err, trdapi.CodeDownstreamCommitmentOpen, "/args/order_id")

	v := q.getOrder(id)
	cmd := q.amendCmd(id, "amendment.1", 0, 1, v.OrderHash, "100")
	cmd.Impact = []trdapi.ImpactAssessment{{Area: "materials", Status: trdapi.ImpactUnaffected}, {Area: "operations", Status: trdapi.ImpactUnaffected}}
	_, err = q.c.TRD.PrepareAmendment(q.author, cmd)
	q.must(err)
	a := q.getOrder(id).Amendments[0]
	var content struct {
		Impact []struct {
			Area, Status, Note string
		} `json:"impact"`
	}
	q.must(json.Unmarshal([]byte(a.Content), &content))
	got := map[string]string{}
	for _, x := range content.Impact {
		got[x.Area] = x.Status + " " + x.Note
	}
	for area, want := range map[string]string{
		"purchasing":   "open pur.downstream_commitments.v1: demand_allocation L1 120; purchase_draft purchase.1 prepared at order revision 1",
		"quality":      "open pur.downstream_commitments.v1: issued commitments: none; ful.shipment_facts.v1: nothing shipped",
		"packing":      "open ful.shipment_facts.v1: nothing shipped",
		"booking":      "open ful.shipment_facts.v1: no confirmed shipment",
		"payment_plan": "open payment terms unchanged; revision 1: stl.credit_guard.v1=not_required",
		"materials":    "unaffected ",
		"operations":   "unaffected ",
	} {
		if !strings.HasPrefix(got[area], want) {
			t.Errorf("impact %s: %q, want %q", area, got[area], want)
		}
	}
	if strings.Join(a.OpenItems, ",") != "purchasing,quality,packing,booking,payment_plan" {
		t.Fatalf("open items %q", a.OpenItems)
	}

	_, err = q.c.PUR.VoidDraft(q.author, purapi.VoidDraft{IdempotencyKey: q.key(), DraftID: "purchase.1", ExpectedVersion: 1, Reason: purVoidReason})
	q.must(err)
	if _, err = q.endCancel(id); err != nil {
		t.Fatalf("cancel after the draft was voided: %v", err)
	}
	if line, end := q.ended(id); line != "120 0 120" || end.DownstreamSource != trdapi.GuardDownstream || end.Status != trdapi.OrderCancelled {
		t.Fatalf("cancelled %s %+v", line, end)
	}
}

// TRD-INV-019 and SM-TRD-ORDER close_remainder ("相关采购与占用已处理") on PUR's real commitment and
// deliveries: commitment.1 is issued for 120 PCS, 60 were verified delivered and FUL shipped 50 of
// them. The order cannot be cancelled (ORDER_HAS_SHIPMENTS) and its remainder cannot be closed while
// the commitment is issued (DOWNSTREAM_COMMITMENT_OPEN). The purchaser cancels the commitment with
// the supplier's evidence: PUR keeps the 60 verified and releases the rest, but 10 delivered PCS are
// still allocated to the order and not shipped — still open. FUL ships them; now the allocation is
// what shipped, the cancelled commitment is no longer listed, and the remainder of 60 closes short.
func TestCloseRemainderWaitsForPURCommitment(t *testing.T) {
	w := newFulPurWorld(t)
	q := w.trdWorld
	id := w.order.ObjectID
	d1 := w.verified("delivery.1", "60", "60")
	shipOn := func(sid, quantity string) {
		t.Helper()
		_, err := w.ship(w.c.FUL, w.merch, sid, quantity, d1)
		w.must(err)
		w.grantedOn(w.c.FUL, sid, fulapi.ScopeShipment)
		_, err = w.c.FUL.ConfirmShipment(w.merch, w.confirmCmd(sid, 1))
		w.must(err)
	}
	shipOn("shipment.1", "50")

	_, err := q.endCancel(id)
	wantRefused(t, "cancel after 50 shipped", err, trdapi.CodeOrderHasShipments, "/args/order_id")
	_, err = q.endShort(id)
	wantRefused(t, "close the remainder while the commitment is issued", err, trdapi.CodeDownstreamCommitmentOpen, "/args/order_id")
	if !strings.Contains(reasonOf(err), "purchase_commitment commitment.1 is issued") {
		t.Fatalf("the refusal names the commitment: %v %s", err, reasonOf(err))
	}

	cv, err := w.c.PUR.GetCommitment(w.author, "commitment.1")
	w.must(err)
	_, err = w.c.PUR.CancelCommitment(w.author, purapi.CancelCommitment{IdempotencyKey: w.key(), CommitmentID: "commitment.1",
		ExpectedVersion: cv.RowVersion, Reason: "合成：工厂停产，双方确认取消余量", Evidence: purChangeEvidence})
	w.must(err)
	_, err = q.endShort(id)
	wantRefused(t, "close the remainder with delivered goods not shipped", err, trdapi.CodeDownstreamCommitmentOpen, "/args/order_id")
	if !strings.Contains(reasonOf(err), "demand_allocation L1 allocates 60 of line L1, which shipped 50") {
		t.Fatalf("the refusal names the allocation: %s", reasonOf(err))
	}

	shipOn("shipment.2", "10")
	if _, err = q.endShort(id); err != nil {
		t.Fatalf("close the remainder once the delivered goods shipped: %v", err)
	}
	if line, end := q.ended(id); line != "120 60 60" || end.DownstreamSource != trdapi.GuardDownstream {
		t.Fatalf("closed short %s %+v", line, end)
	}
}

// reasonOf returns the reason of a rejection's first field, "" for none.
func reasonOf(err error) string {
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 {
		return rej.Fields[0].Reason
	}
	return ""
}

// TRD-INV-019 "取消与出运确认并发：出运先提交则取消拒绝", both commit orders on PostgreSQL 16 (D-1: FUL's
// shipment.confirm holds the order row FOR SHARE through OrderGuard v1 until it commits; TRD's cancel
// locks it FOR UPDATE and then reads ShipmentFacts v1 without a lock):
//
//   - The confirmation holds the order (it waits at its action result): order.cancel waits for the
//     order row; the confirmation commits, the cancel reads the shipment and is ORDER_HAS_SHIPMENTS;
//     the order stays active.
//   - The cancel holds the order (it waits at its action result): the confirmation's OrderGuard
//     waits; the cancel commits, the guard finds the order cancelled and the confirmation is
//     ORDER_NOT_ACTIVE; nothing shipped.
func TestOrderCancelAndShipmentConfirmationInBothCommitOrders(t *testing.T) {
	t.Run("the confirmation holds the order", func(t *testing.T) {
		w := newFulWorld(t)
		q := w.trdWorld
		d := newDownstream(q.world)
		id := w.order.ObjectID
		rel, dl := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
		_, err := w.prepare(w.merch, "shipment.1", "40", dl, rel)
		w.must(err)
		w.granted("shipment.1")
		cmd := w.confirmCmd("shipment.1", 1)
		blocker, unblock := d.keyBlocker(t, "brant_ful", fulapi.CommandConfirmShipment, cmd.IdempotencyKey)
		confirmed := make(chan finished, 1)
		go func() {
			_, err := w.ful.ConfirmShipment(w.merch, cmd)
			confirmed <- finished{time.Now(), err}
		}()
		if !awaitInHold(t, "the confirmation holds the order", func() bool { return d.blocks(blocker) }, confirmed) {
			unblock()
			return
		}
		confirming := blockedBy(t, blocker)[0]
		version := q.orderVersion(id)
		cancelled := make(chan finished, 1)
		go func() {
			_, err := q.c.TRD.CancelOrder(q.author, trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: id, ExpectedVersion: version,
				Reason: "客户撤单", Evidence: q.proof})
			cancelled <- finished{time.Now(), err}
		}()
		ok := awaitInHold(t, "the cancel waits on the confirmation", func() bool { return len(blockedBy(t, confirming)) == 1 }, cancelled)
		unblock()
		if !ok {
			return
		}
		if c := <-confirmed; c.err != nil {
			t.Fatalf("the confirmation: %v", c.err)
		}
		c := <-cancelled
		wantRefused(t, "the cancel after the confirmation", c.err, trdapi.CodeOrderHasShipments, "/args/order_id")
		if v := q.getOrder(id); v.Status != trdapi.OrderActive || v.End != nil {
			t.Fatalf("the order %+v", v)
		}
	})

	t.Run("the cancel holds the order", func(t *testing.T) {
		w := newFulWorld(t)
		q := w.trdWorld
		d := newDownstream(q.world)
		id := w.order.ObjectID
		rel, dl := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
		_, err := w.prepare(w.merch, "shipment.1", "40", dl, rel)
		w.must(err)
		w.granted("shipment.1")
		cmd := trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: id, ExpectedVersion: q.orderVersion(id), Reason: "客户撤单", Evidence: q.proof}
		blocker, unblock := d.keyBlocker(t, "brant_trd", trdapi.CommandCancelOrder, cmd.IdempotencyKey)
		cancelled := make(chan finished, 1)
		go func() {
			_, err := q.c.TRD.CancelOrder(q.author, cmd)
			cancelled <- finished{time.Now(), err}
		}()
		if !awaitInHold(t, "the cancel holds the order", func() bool { return d.blocks(blocker) }, cancelled) {
			unblock()
			return
		}
		cancelling := blockedBy(t, blocker)[0]
		confirmed := make(chan finished, 1)
		go func() {
			_, err := w.ful.ConfirmShipment(w.merch, w.confirmCmd("shipment.1", 1))
			confirmed <- finished{time.Now(), err}
		}()
		ok := awaitInHold(t, "the confirmation waits on the cancel", func() bool { return len(blockedBy(t, cancelling)) == 1 }, confirmed)
		unblock()
		if !ok {
			return
		}
		if c := <-cancelled; c.err != nil {
			t.Fatalf("the cancel: %v", c.err)
		}
		c := <-confirmed
		if code(c.err) != fulapi.CodeOrderNotActive {
			t.Fatalf("the confirmation after the cancel: %v", c.err)
		}
		if got, err := w.shipped("company.A", id); err != nil || len(got) != 0 {
			t.Fatalf("ShipmentFacts v1 after the cancel %q %v", got, err)
		}
		if line, end := q.ended(id); line != "120 0 120" || end.Status != trdapi.OrderCancelled {
			t.Fatalf("cancelled %s %+v", line, end)
		}
	})
}
