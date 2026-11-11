//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The active sales order suite (WBS S3-15; SM-TRD-ORDER after activation, SM-TRD-ORDER-AMENDMENT;
// TRD-INV-016, 018, 019; TD-AT01, TD-AT02, ES-09, SIM-02 step 7; Context Map R19) on the order
// world: "editor" (the author) prepares and submits amendments, "reviewer" decides and applies them;
// both are humans with every permission. Python has none of this. FUL's shipments and PUR's
// commitments are not wired in S3 (recorded as not_wired): nothing can have shipped.

// amendImpact is a complete impact assessment: purchasing affected, booking open, the rest
// unaffected.
func amendImpact() []trdapi.ImpactAssessment {
	var out []trdapi.ImpactAssessment
	for _, area := range trdapi.ImpactAreas() {
		a := trdapi.ImpactAssessment{Area: area, Status: trdapi.ImpactUnaffected}
		switch area {
		case "purchasing":
			a.Status, a.Note = trdapi.ImpactAffected, "采购草稿按新数量复核分配，余量作废"
		case "booking":
			a.Status, a.Note = trdapi.ImpactOpen, "订舱体积待货代确认"
		}
		out = append(out, a)
	}
	return out
}

// amendCmd prepares amendment id of order at revision rev and hash to quantity qty of L1, from a
// revision of the customer's PO.
func (q *trdWorld) amendCmd(order, id string, expected, rev int64, hash, qty string) trdapi.PrepareAmendment {
	return trdapi.PrepareAmendment{IdempotencyKey: q.key(), OrderID: order, AmendmentID: id, ExpectedVersion: expected,
		ExpectedOrderRevision: rev, ExpectedOrderHash: hash, Source: trdapi.AmendmentSource{Kind: trdapi.SourceCustomerPORevision, Evidence: q.proof},
		Lines: []trdapi.AmendmentLine{{LineID: "L1", Sku: trdapi.SkuRef{SkuID: "sku.1", SpecRevision: q.spec}, Quantity: qty, UnitRef: tref(q.pcs),
			UnitPrice: "8", PriceBasisQuantity: "1", DiscountRatio: "0", TaxApplicationRef: tref(q.sale)}},
		Terms:  trdapi.AmendmentTerms{TradeTermsRef: tref(q.trade), NamedPlaceRef: dehamPlace, PaymentTermsRef: tref(q.payment), Delivery: handover},
		Impact: amendImpact()}
}

// approvedAmendment has the author prepare and submit amendment id of order (at revision rev and
// hash, to qty) and the reviewer approve it; it returns the decision's receipt.
func (q *trdWorld) approvedAmendment(order, id string, rev int64, hash, qty string) trdapi.AmendmentReceipt {
	q.t.Helper()
	p, err := q.c.TRD.PrepareAmendment(q.author, q.amendCmd(order, id, 0, rev, hash, qty))
	q.must(err)
	s, err := q.c.TRD.SubmitAmendment(q.author, trdapi.SubmitAmendment{IdempotencyKey: q.key(), OrderID: order, AmendmentID: id,
		ExpectedVersion: p.RowVersion})
	q.must(err)
	d, err := q.c.TRD.DecideAmendment(q.reviewer, trdapi.DecideAmendment{IdempotencyKey: q.key(), OrderID: order, AmendmentID: id,
		ExpectedVersion: s.RowVersion, AmendmentHash: s.AmendmentHash, Decision: trdapi.DecisionApprove, Reason: "客户书面确认"})
	q.must(err)
	return d
}

func (q *trdWorld) applyCmd(order, id string, expected int64) trdapi.ApplyAmendment {
	return trdapi.ApplyAmendment{IdempotencyKey: q.key(), OrderID: order, AmendmentID: id, ExpectedVersion: expected}
}

// revisionLine reads quantity and payable of line L1 from an order revision document.
func revisionLine(t *testing.T, doc string) (quantity, payable string) {
	t.Helper()
	var r struct {
		Lines []struct {
			LineID   string `json:"line_id"`
			Quantity string `json:"quantity"`
		} `json:"lines"`
		Amounts struct {
			Payable string `json:"payable_minor"`
		} `json:"amounts"`
	}
	if err := json.Unmarshal([]byte(doc), &r); err != nil || len(r.Lines) != 1 {
		t.Fatalf("revision document %v: %s", err, doc)
	}
	return r.Lines[0].Quantity, r.Amounts.Payable
}

// TD-AT01's positive half, ES-09 and SIM-02 step 7 (order.amendment.prepare, decide, apply),
// TRD-INV-016, 018: the customer reduces L1 from 120 to 100. An AI agent may prepare, never submit;
// the author prepares and submits; the author cannot decide (REVIEWER_NOT_INDEPENDENT), nor can the
// reviewer on another hash (ORDER_REVIEW_MISMATCH); the reviewer approves and applies. The order
// moves to revision 2 with a new order hash, the revision document, the amendment applied, the
// commit evidence and OrderAmended in one transaction; ActiveOrderRef v1 shows 100 PCS; OrderGuard
// v1 answers REFERENCE_STALE to PUR's binding of revision 1 and admits revision 2. A replay answers
// the stored receipt; the amendment applies once.
func TestOrderAmendmentLifecycle(t *testing.T) {
	q := newOrderWorld(t)
	o := q.activeOrder("quote.1")
	v1 := q.getOrder(o.ObjectID)
	b1 := trdapi.OrderBinding{OrderID: o.ObjectID, OrderRevision: 1, OrderHash: v1.OrderHash}
	if g := q.orderGuard(b1); !g.Usable() || v1.SnapshotHash != v1.OrderHash || v1.CurrentRevision != "" {
		t.Fatalf("revision 1: %+v %+v", g, v1)
	}

	agent := q.actorOf(kernel.ActorAI, "agent.1")
	draft, err := q.c.TRD.PrepareAmendment(agent, q.amendCmd(o.ObjectID, "amendment.1", 0, 1, v1.OrderHash, "90"))
	q.must(err)
	if draft.Status != trdapi.AmendmentDraft || draft.RowVersion != 1 || draft.BaseRevision != 1 || draft.BaseHash != v1.OrderHash ||
		draft.OrderRevision != 1 || draft.OrderRowVersion != v1.RowVersion || draft.ObjectID != o.ObjectID {
		t.Fatalf("prepared by the agent: %+v", draft)
	}
	_, err = q.c.TRD.SubmitAmendment(agent, trdapi.SubmitAmendment{IdempotencyKey: q.key(), OrderID: o.ObjectID, AmendmentID: "amendment.1",
		ExpectedVersion: 1})
	q.want(err, kernel.CodePermissionDenied)

	again := q.amendCmd(o.ObjectID, "amendment.1", 1, 1, v1.OrderHash, "100.000000")
	p, err := q.c.TRD.PrepareAmendment(q.author, again)
	q.must(err)
	if p.RowVersion != 2 || p.AmendmentHash == draft.AmendmentHash {
		t.Fatalf("prepared again: %+v", p)
	}
	s, err := q.c.TRD.SubmitAmendment(q.author, trdapi.SubmitAmendment{IdempotencyKey: q.key(), OrderID: o.ObjectID, AmendmentID: "amendment.1",
		ExpectedVersion: 2})
	q.must(err)
	decide := func(ctx context.Context, hash string) error {
		_, err := q.c.TRD.DecideAmendment(ctx, trdapi.DecideAmendment{IdempotencyKey: q.key(), OrderID: o.ObjectID, AmendmentID: "amendment.1",
			ExpectedVersion: 3, AmendmentHash: hash, Decision: trdapi.DecisionApprove})
		return err
	}
	if err = decide(q.author, s.AmendmentHash); code(err) != trdapi.CodeReviewerNotIndependent {
		t.Fatalf("the preparer deciding: %s", code(err))
	}
	if err = decide(q.reviewer, draft.AmendmentHash); code(err) != trdapi.CodeOrderReviewMismatch || path(err) != "/args/amendment_hash" {
		t.Fatalf("deciding the agent's draft hash: %s at %s", code(err), path(err))
	}
	q.must(decide(q.reviewer, s.AmendmentHash))

	apply := q.applyCmd(o.ObjectID, "amendment.1", 4)
	r, err := q.c.TRD.ApplyAmendment(q.reviewer, apply)
	q.must(err)
	if r.Status != trdapi.AmendmentApplied || r.RowVersion != 5 || r.OrderRevision != 2 || r.OrderHash == v1.OrderHash ||
		r.OrderRowVersion != v1.RowVersion+1 || r.OrderStatus != trdapi.OrderActive || r.Replayed {
		t.Fatalf("applied: %+v", r)
	}
	v2 := q.getOrder(o.ObjectID)
	quantity, payable := revisionLine(t, v2.CurrentRevision)
	if v2.OrderRevision != 2 || v2.OrderHash != r.OrderHash || sha256Hex(v2.CurrentRevision) != v2.OrderHash || v2.SnapshotHash != v1.OrderHash ||
		quantity != "100" || len(v2.Revisions) != 1 || v2.Revisions[0].PreviousHash != v1.OrderHash || v2.Revisions[0].AmendmentID != "amendment.1" ||
		len(v2.Amendments) != 1 || v2.Amendments[0].Status != trdapi.AmendmentApplied || v2.Amendments[0].AppliedRevision != 2 ||
		len(v2.Amendments[0].Preparers) != 2 || v2.Amendments[0].Stale || sha256Hex(v2.Amendments[0].Content) != r.AmendmentHash ||
		len(v2.AmendmentCommits) != 1 {
		t.Fatalf("the order after the amendment: %+v", v2)
	}
	var snap struct {
		Calculation struct {
			Payable string `json:"payable_minor"`
		} `json:"calculation"`
	}
	q.must(json.Unmarshal([]byte(v1.Snapshot), &snap))
	if before, after := mustInt(t, snap.Calculation.Payable), mustInt(t, payable); before*100 != after*120 {
		t.Fatal("no payable")
	}
	e := v2.AmendmentCommits[0].Entries
	if e["command"] != trdapi.CommandApplyAmendment || e["from_order_hash"] != v1.OrderHash || e["to_order_hash"] != r.OrderHash ||
		e["fulfilment_source"] != trdapi.NotWired || e[trdapi.GuardSTLCredit] != trdapi.NotWired || e[trdapi.GuardCNXTradeCompliance] != trdapi.NotWired ||
		e["decided_by"] != "human:reviewer" || e["prepared_by"] != "ai:agent.1,human:editor" || e["open_items"] != "booking" ||
		e["idempotency_key"] != apply.IdempotencyKey {
		t.Fatalf("the commit evidence %v", e)
	}

	// PUR's binding of revision 1 is stale; revision 2 serves.
	b2 := trdapi.OrderBinding{OrderID: o.ObjectID, OrderRevision: 2, OrderHash: r.OrderHash}
	if g := q.orderGuard(b1); g.Code != trdapi.CodeReferenceStale || g.OrderRevision != 2 {
		t.Fatalf("the old binding: %+v", g)
	}
	if g := q.orderGuard(b2); !g.Usable() {
		t.Fatalf("the new binding: %+v", g)
	}
	facts, err := q.c.TRD.ResolveActiveOrders(q.author, []string{o.ObjectID})
	q.must(err)
	if f := facts[o.ObjectID]; f.Binding != b2 || len(f.Lines) != 1 || f.Lines[0].Quantity != "100" || f.Lines[0].NormalizedQuantity != "100" ||
		f.Check("/x") != nil {
		t.Fatalf("ActiveOrderRef after the amendment %+v", f)
	}

	// The events, in order, on the order's partition.
	var types []string
	var amended map[string]any
	for _, ev := range q.events("company.A", trdapi.Source) {
		if strings.Contains(ev.Type, "amend") {
			types = append(types, ev.Type)
			if ev.Type == trdapi.EventOrderAmended {
				amended = ev.Payload
			}
		}
	}
	if strings.Join(types, " ") != strings.Join([]string{trdapi.EventOrderAmendmentRequested, trdapi.EventOrderAmendmentRequested,
		trdapi.EventOrderAmendmentDecided, trdapi.EventOrderAmended}, " ") {
		t.Fatalf("events %v", types)
	}
	if amended["order_revision"] != float64(2) || amended["order_hash"] != r.OrderHash || amended["from_order_hash"] != v1.OrderHash ||
		amended["aggregate_type"] != "SalesOrder" || amended["fulfilment_source"] != trdapi.NotWired || amended["changed_terms"] != "" {
		t.Fatalf("OrderAmended %v", amended)
	}
	lines, _ := amended["changed_lines"].([]any)
	if len(lines) != 1 || lines[0].(map[string]any)["from_quantity"] != "120" || lines[0].(map[string]any)["to_quantity"] != "100" {
		t.Fatalf("OrderAmended's lines %v", amended["changed_lines"])
	}

	// A replay answers the stored receipt; the amendment applies once.
	replay, err := q.c.TRD.ApplyAmendment(q.reviewer, apply)
	q.must(err)
	if !replay.Replayed || replay.ObjectID != r.ObjectID || replay.RowVersion != r.RowVersion || replay.OrderHash != r.OrderHash {
		t.Fatalf("replay %+v", replay)
	}
	_, err = q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(o.ObjectID, "amendment.1", 5))
	q.want(err, kernel.CodeInvalidState)
	if n := q.count("company.A", "brant_trd.sales_order_revision"); n != 1 {
		t.Fatalf("%d revisions", n)
	}
	// A second amendment of revision 2 needs revision 2's hash.
	_, err = q.c.TRD.PrepareAmendment(q.author, q.amendCmd(o.ObjectID, "amendment.2", 0, 1, v1.OrderHash, "90"))
	q.want(err, trdapi.CodeOrderAmendmentStale)
	q.approvedAmendment(o.ObjectID, "amendment.2", 2, r.OrderHash, "90")
}

// TRD-INV-018, 两个变更并发应用只有一个成功: two approved amendments of revision 1. While the first apply
// holds the order, the second waits for the quote root, then finds the order at revision 2
// (ORDER_AMENDMENT_STALE) and stays approved; unsynchronized, exactly one of two applies wins.
func TestTwoAmendmentsOfOneRevisionHaveOneWinner(t *testing.T) {
	q := newOrderWorld(t)
	d := newDownstream(q.world)
	for _, id := range []string{"quote.1", "quote.2"} {
		o := q.activeOrder(id)
		h := q.getOrder(o.ObjectID).OrderHash
		a1 := q.approvedAmendment(o.ObjectID, "amendment.1", 1, h, "100")
		a2 := q.approvedAmendment(o.ObjectID, "amendment.2", 1, h, "110")
		first, second := q.applyCmd(o.ObjectID, "amendment.1", a1.RowVersion), q.applyCmd(o.ObjectID, "amendment.2", a2.RowVersion)
		if id == "quote.1" {
			blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandApplyAmendment, first.IdempotencyKey)
			done := make(chan error, 1)
			go func() {
				_, err := q.c.TRD.ApplyAmendment(q.reviewer, first)
				done <- err
			}()
			if !awaitInHold(t, "the first apply waits at its result", func() bool { return d.blocks(blocker) }, done) {
				release()
				return
			}
			late := make(chan error, 1)
			go func() {
				_, err := q.c.TRD.ApplyAmendment(q.author, second)
				late <- err
			}()
			ok := awaitInHold(t, "the second apply waits for the quote root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, late)
			release()
			if !ok {
				return
			}
			q.must(<-done)
			if err := <-late; code(err) != trdapi.CodeOrderAmendmentStale {
				t.Fatalf("the second apply: %v", err)
			}
		} else {
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for i, cmd := range []trdapi.ApplyAmendment{first, second} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, errs[i] = q.c.TRD.ApplyAmendment(q.reviewer, cmd)
				}()
			}
			wg.Wait()
			if (errs[0] == nil) == (errs[1] == nil) {
				t.Fatalf("two applies: %v, %v", errs[0], errs[1])
			}
			for _, err := range errs {
				if err != nil && code(err) != trdapi.CodeOrderAmendmentStale {
					t.Fatalf("the losing apply: %v", err)
				}
			}
		}
		v := q.getOrder(o.ObjectID)
		applied, approved := 0, 0
		for _, a := range v.Amendments {
			switch a.Status {
			case trdapi.AmendmentApplied:
				applied++
			case trdapi.AmendmentApproved:
				approved++
				if !a.Stale {
					t.Fatalf("the losing amendment is not stale: %+v", a)
				}
			}
		}
		if v.OrderRevision != 2 || applied != 1 || approved != 1 || len(v.Revisions) != 1 {
			t.Fatalf("%s after the race: revision %d, %d applied, %d approved", id, v.OrderRevision, applied, approved)
		}
	}
}

// SM-TRD-ORDER: an amendment and the order's cancellation have one winner. The apply holding the
// order first: the cancellation waits, then finds another row version (VERSION_CONFLICT). The
// cancellation first: the apply waits, then finds the order cancelled (ORDER_NOT_ACTIVE), and
// OrderGuard v1 answers ORDER_NOT_ACTIVE.
func TestAmendmentAndCancellationHaveOneWinner(t *testing.T) {
	q := newOrderWorld(t)
	d := newDownstream(q.world)
	cancelCmd := func(order string, expected int64) trdapi.CancelOrder {
		return trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: order, ExpectedVersion: expected, Reason: "客户撤单", Evidence: q.proof}
	}
	for _, applyFirst := range []bool{true, false} {
		id := "quote.cancel"
		if applyFirst {
			id = "quote.apply"
		}
		o := q.activeOrder(id)
		v := q.getOrder(o.ObjectID)
		a := q.approvedAmendment(o.ObjectID, "amendment.1", 1, v.OrderHash, "100")
		apply, cancel := q.applyCmd(o.ObjectID, "amendment.1", a.RowVersion), cancelCmd(o.ObjectID, v.RowVersion)
		firstCmd, firstKey := trdapi.CommandCancelOrder, cancel.IdempotencyKey
		if applyFirst {
			firstCmd, firstKey = trdapi.CommandApplyAmendment, apply.IdempotencyKey
		}
		blocker, release := d.keyBlocker(t, "brant_trd", firstCmd, firstKey)
		runApply := func(out chan<- error) {
			_, err := q.c.TRD.ApplyAmendment(q.reviewer, apply)
			out <- err
		}
		runCancel := func(out chan<- error) {
			_, err := q.c.TRD.CancelOrder(q.author, cancel)
			out <- err
		}
		first, second := runApply, runCancel
		if !applyFirst {
			first, second = runCancel, runApply
		}
		done, late := make(chan error, 1), make(chan error, 1)
		go first(done)
		if !awaitInHold(t, "the first command waits at its result", func() bool { return d.blocks(blocker) }, done) {
			release()
			return
		}
		go second(late)
		ok := awaitInHold(t, "the second command waits for the quote root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, late)
		release()
		if !ok {
			return
		}
		q.must(<-done)
		after := q.getOrder(o.ObjectID)
		b1 := trdapi.OrderBinding{OrderID: o.ObjectID, OrderRevision: 1, OrderHash: v.OrderHash}
		if applyFirst {
			if err := <-late; code(err) != kernel.CodeVersionConflict {
				t.Fatalf("the cancellation after the apply: %v", err)
			}
			if after.Status != trdapi.OrderActive || after.OrderRevision != 2 {
				t.Fatalf("after the apply: %+v", after)
			}
		} else {
			if err := <-late; code(err) != trdapi.CodeOrderNotActive {
				t.Fatalf("the apply after the cancellation: %v", err)
			}
			if after.Status != trdapi.OrderCancelled || after.OrderRevision != 1 || after.End == nil || after.End.Command != trdapi.CommandCancelOrder {
				t.Fatalf("after the cancellation: %+v", after)
			}
			if g := q.orderGuard(b1); g.Code != trdapi.CodeOrderNotActive {
				t.Fatalf("the guard after the cancellation %+v", g)
			}
		}
	}
}

// TD-AT02 and TRD-INV-019 in S3, where FUL is not built: an active order shipped nothing, so it is
// not closed (ORDER_NOT_FULFILLED) nor closed short (INVALID_STATE: cancel it), and it is
// cancelled, with every line's ordered and given-up quantity and the sources not wired; the
// conversion slot stays taken, OrderGuard v1 answers ORDER_NOT_ACTIVE, and the amendments end with
// the order (ORDER_NOT_ACTIVE). An AI agent ends nothing.
func TestActiveOrderEnds(t *testing.T) {
	q := newOrderWorld(t)
	o := q.activeOrder("quote.1")
	v := q.getOrder(o.ObjectID)
	a, err := q.c.TRD.PrepareAmendment(q.author, q.amendCmd(o.ObjectID, "amendment.1", 0, 1, v.OrderHash, "100"))
	q.must(err)
	_, err = q.c.TRD.CloseOrder(q.author, trdapi.CloseOrder{IdempotencyKey: q.key(), OrderID: o.ObjectID, ExpectedVersion: v.RowVersion})
	q.want(err, trdapi.CodeOrderNotFulfilled)
	_, err = q.c.TRD.CloseOrderRemainder(q.author, trdapi.CloseOrderRemainder{IdempotencyKey: q.key(), OrderID: o.ObjectID,
		ExpectedVersion: v.RowVersion, Reason: "客户同意短交"})
	q.want(err, kernel.CodeInvalidState)
	agent := q.actorOf(kernel.ActorAI, "agent.1")
	_, err = q.c.TRD.CancelOrder(agent, trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: o.ObjectID, ExpectedVersion: v.RowVersion,
		Reason: "客户撤单", Evidence: q.proof})
	q.want(err, kernel.CodePermissionDenied)
	c, err := q.c.TRD.CancelOrder(q.author, trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: o.ObjectID, ExpectedVersion: v.RowVersion,
		Reason: "客户撤单", Evidence: q.proof})
	q.must(err)
	after := q.getOrder(o.ObjectID)
	if c.Status != trdapi.OrderCancelled || c.RowVersion != v.RowVersion+1 || after.Closure != nil || after.End == nil ||
		after.End.Status != trdapi.OrderCancelled || after.End.FulfilmentSource != trdapi.NotWired || after.End.DownstreamSource != trdapi.NotWired ||
		len(after.End.Lines) != 1 || after.End.Lines[0].Ordered != "120" || after.End.Lines[0].Shipped != "0" || after.End.Lines[0].Remainder != "120" ||
		after.End.OrderRevision != 1 || after.End.Reason != "客户撤单" {
		t.Fatalf("cancelled: %+v %+v", c, after.End)
	}
	if g := q.orderGuard(trdapi.OrderBinding{OrderID: o.ObjectID, OrderRevision: 1, OrderHash: v.OrderHash}); g.Code != trdapi.CodeOrderNotActive {
		t.Fatalf("the guard after the cancellation %+v", g)
	}
	_, err = q.c.TRD.SubmitAmendment(q.author, trdapi.SubmitAmendment{IdempotencyKey: q.key(), OrderID: o.ObjectID, AmendmentID: "amendment.1",
		ExpectedVersion: a.RowVersion})
	q.want(err, trdapi.CodeOrderNotActive)
	var cancelled map[string]any
	for _, ev := range q.events("company.A", trdapi.Source) {
		if ev.Type == trdapi.EventOrderCancelled {
			cancelled = ev.Payload
		}
	}
	if cancelled["ended_from"] != trdapi.OrderActive || cancelled["fulfilment_source"] != trdapi.NotWired || cancelled["conversion_slot"] != "full_v1" {
		t.Fatalf("OrderCancelled %v", cancelled)
	}
	// The slot stays taken: the quote converts no second order.
	if quote := q.getQuote("quote.1"); quote.Conversion == nil {
		t.Fatalf("the slot after the cancellation: %+v", quote.Conversion)
	}
	_, err = q.c.TRD.CloseOrder(q.author, trdapi.CloseOrder{IdempotencyKey: q.key(), OrderID: o.ObjectID, ExpectedVersion: c.RowVersion})
	q.want(err, trdapi.CodeOrderNotActive)
}

// The database refuses what SM-TRD-ORDER-AMENDMENT and the revision chain do not allow, whatever a
// command checked: moving an order's revision without its revision row and commit evidence; a
// revision of an amendment that is not approved; changing an applied amendment or a revision;
// deleting an amendment; an amendment decided by its preparer.
func TestDatabaseRefusesIllegalAmendmentWrites(t *testing.T) {
	q := newOrderWorld(t)
	o := q.activeOrder("quote.1")
	v := q.getOrder(o.ObjectID)
	p, err := q.c.TRD.PrepareAmendment(q.author, q.amendCmd(o.ObjectID, "amendment.draft", 0, 1, v.OrderHash, "100"))
	q.must(err)
	a := q.approvedAmendment(o.ObjectID, "amendment.1", 1, v.OrderHash, "110")
	r, err := q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(o.ObjectID, "amendment.1", a.RowVersion))
	q.must(err)
	s := q.scope("company.A")
	t2, c2 := string(s.Tenant), string(s.Company)
	for name, sql := range map[string]string{
		"an order revision without its row": `UPDATE brant_trd.sales_order SET order_revision = 3, order_hash = repeat('a', 64), row_version = row_version + 1
			WHERE tenant_id = $1 AND company_id = $2 AND order_id = '` + o.ObjectID + `'`,
		"a revision of a draft amendment": `INSERT INTO brant_trd.sales_order_revision (tenant_id, company_id, order_id, order_revision, document,
			order_hash, previous_revision, previous_hash, amendment_id, amendment_hash, row_version, created_at)
			VALUES ($1, $2, '` + o.ObjectID + `', 3, '{}', encode(sha256(convert_to('{}', 'UTF8')), 'hex'), 2, '` + r.OrderHash + `',
			'amendment.draft', '` + p.AmendmentHash + `', 99, now())`,
		"an applied amendment's content": `UPDATE brant_trd.sales_order_amendment SET content = '{"x":1}',
			amendment_hash = encode(sha256(convert_to('{"x":1}', 'UTF8')), 'hex'), version = version + 1
			WHERE tenant_id = $1 AND company_id = $2 AND order_id = '` + o.ObjectID + `' AND amendment_id = 'amendment.1'`,
		"a revision's document": `UPDATE brant_trd.sales_order_revision SET created_at = now()
			WHERE tenant_id = $1 AND company_id = $2 AND order_id = '` + o.ObjectID + `'`,
		"deleting an amendment": `DELETE FROM brant_trd.sales_order_amendment WHERE tenant_id = $1 AND company_id = $2
			AND order_id = '` + o.ObjectID + `' AND amendment_id = 'amendment.draft'`,
		"an end of an order at an old revision": `INSERT INTO brant_trd.sales_order_end (tenant_id, company_id, order_id, command, status,
			order_revision, order_hash, lines, fulfilment_source, downstream_source, ended_by_kind, ended_by_id, end_epoch, ended_at, reason,
			evidence, row_version) VALUES ($1, $2, '` + o.ObjectID + `', 'order.cancel', 'cancelled', 1, '` + v.OrderHash + `', '[]', 'x', 'x',
			'human', 'editor', 1, now(), 'r', '[]', 99)`,
	} {
		err := q.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
			_, err := exec.Exec(ctx, sql, t2, c2)
			return err
		})
		if err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
	}
	// The draft submitted, then decided by its preparer in SQL.
	_, err = q.c.TRD.PrepareAmendment(q.author, q.amendCmd(o.ObjectID, "amendment.draft", p.RowVersion, 2, r.OrderHash, "100"))
	q.must(err)
	_, err = q.c.TRD.SubmitAmendment(q.reviewer, trdapi.SubmitAmendment{IdempotencyKey: q.key(), OrderID: o.ObjectID, AmendmentID: "amendment.draft",
		ExpectedVersion: 2})
	q.must(err)
	err = q.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, `UPDATE brant_trd.sales_order_amendment SET status = 'approved', version = version + 1, decided_by_kind = 'human',
			decided_by_id = 'editor', decide_epoch = 1, decided_at = now(), decision = 'approve', decision_evidence = '[]'
			WHERE tenant_id = $1 AND company_id = $2 AND order_id = '`+o.ObjectID+`' AND amendment_id = 'amendment.draft'`, t2, c2)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "SM-TRD-ORDER-AMENDMENT") {
		t.Fatalf("decided by its preparer: %v", err)
	}
}

// R19's guard lock: a downstream (PUR) transaction holding OrderGuard v1 on revision 1 makes the
// apply wait until it commits; the apply then moves the order, and the next downstream draft on
// revision 1 is refused (REFERENCE_STALE).
func TestDownstreamGuardHoldsTheApply(t *testing.T) {
	q := newOrderWorld(t)
	d := newDownstream(q.world)
	o := q.activeOrder("quote.1")
	v := q.getOrder(o.ObjectID)
	b1 := trdapi.OrderBinding{OrderID: o.ObjectID, OrderRevision: 1, OrderHash: v.OrderHash}
	a := q.approvedAmendment(o.ObjectID, "amendment.1", 1, v.OrderHash, "100")
	d.createQuote("pur.1")
	holding, hold := make(chan struct{}), make(chan struct{})
	var g trdapi.OrderVerdict
	drafted := make(chan error, 1)
	go func() {
		drafted <- d.activateWith("pur.1", stage{hold: func() { close(holding); <-hold }}, orderStep(b1, &g))
	}()
	<-holding
	applied := make(chan timed, 1)
	go func() {
		_, err := q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(o.ObjectID, "amendment.1", a.RowVersion))
		applied <- timed{at: time.Now(), err: err}
	}()
	ok := awaitInHold(t, "the apply waits for the downstream's guard", func() bool { return lockWaiter(t, "brant_trd.sales_order") }, applied)
	released := time.Now()
	close(hold)
	if !ok {
		return
	}
	q.must(<-drafted)
	if r := <-applied; r.err != nil || !r.at.After(released) {
		t.Fatalf("the apply %v, after the downstream %v", r.err, r.at.After(released))
	}
	d.createQuote("pur.2")
	if err := d.activateWith("pur.2", stage{}, orderStep(b1, &g)); code(err) != trdapi.CodeReferenceStale {
		t.Fatalf("a downstream draft on revision 1 after the amendment: %v", err)
	}
}
