//go:build integration

package bootstrap_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SIM-03, TRD-INV-024, F-01, BC-D06: an opted-in multi_order quote of 1,200 PCS takes 700 then 500
// and refuses 1 more; an uncapped quote takes 1,200 then 2,000; concurrent prepares serialize on
// the quote root; cancelling does not return quantity; replace and cancel of the quote stay locked.
// The quote quantity is also the RFQ's and the cost snapshot's (TRD-INV-006). The frozen world
// costs rfq.1 at 120 PCS, so these quotes adopt rfq.multi and its snapshot instead of changing
// that comparison basis.
func TestPartialOrdersFromOneQuote(t *testing.T) {
	q := newOrderWorld(t)
	a := q.multiAccepted("sim.cap", "1200", false)
	first := q.partial(a, "order.cap.1", "700")
	second := q.partial(a, "order.cap.2", "500")
	if first.QuoteID != a.QuoteID || second.BusinessHash != a.BusinessHash {
		t.Fatalf("orders %+v %+v", first, second)
	}
	q.wantPayable(first, expectNet("700"))
	q.wantPayable(second, expectNet("500"))
	_, err := q.c.TRD.PreparePartialOrder(q.author, q.partialCmd(a, "order.cap.3", "1"))
	if code(err) != trdapi.CodeQuoteRemainingInsufficient || path(err) != "/args/lines/0/quantity" {
		t.Fatalf("one more piece: %s %s", code(err), path(err))
	}
	if _, err = q.c.TRD.PrepareOrder(q.author, q.prepareCmd("order.cap.full", a)); code(err) != trdapi.CodeConversionSlotConflict || path(err) != "/args/quote_id" {
		t.Fatalf("order.prepare on multi_order: %s %s", code(err), path(err))
	}
	_, full := q.accepted("sim.plain")
	if _, err = q.c.TRD.PreparePartialOrder(q.author, q.partialCmd(full, "order.plain.partial", "1")); code(err) != trdapi.CodeConversionSlotUnsupported || path(err) != "/args/quote_id" {
		t.Fatalf("partial on single_full: %s %s", code(err), path(err))
	}
	if _, err = q.c.TRD.PrepareOrder(q.author, q.prepareCmd("order.plain", full)); err != nil {
		t.Fatal(err)
	}
	if view := q.getQuote("sim.plain"); view.MultiOrderLedger != nil || view.Conversion == nil || view.Conversion.Slot != "full_v1" {
		t.Fatalf("a default quote read ledger %+v conversion %+v", view.MultiOrderLedger, view.Conversion)
	}
	ledger := q.getQuote(a.QuoteID).MultiOrderLedger
	if ledger == nil || len(ledger.Lines) != 1 || ledger.Lines[0].Consumed != "1200" || ledger.Lines[0].Remaining != "0" {
		t.Fatalf("ledger %+v", ledger)
	}

	// Each partial order reviews and activates on its own.
	d1 := q.reviewed(first, "review.cap.1")
	act1, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(first.ObjectID, d1.RowVersion, "review.cap.1"))
	q.must(err)
	d2 := q.reviewed(second, "review.cap.2")
	if _, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(second.ObjectID, d2.RowVersion, "review.cap.2")); err != nil {
		t.Fatal(err)
	}
	if act1.Status != trdapi.OrderActive || q.getOrder(second.ObjectID).Status != trdapi.OrderActive {
		t.Fatal("the two orders did not activate independently")
	}

	// Draft cancel and active cancel keep the consumed quantity.
	held := q.multiAccepted("sim.hold", "1200", false)
	draft := q.partial(held, "order.hold.1", "700")
	cancelled, err := q.c.TRD.CancelOrder(q.author, trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: draft.ObjectID,
		ExpectedVersion: draft.RowVersion, Reason: "草稿撤回", Evidence: q.proof})
	q.must(err)
	if cancelled.Status != trdapi.OrderCancelled {
		t.Fatalf("draft cancel %+v", cancelled)
	}
	if _, err = q.c.TRD.PreparePartialOrder(q.author, q.partialCmd(held, "order.hold.2", "700")); code(err) != trdapi.CodeQuoteRemainingInsufficient {
		t.Fatalf("draft cancel returned quantity: %s", code(err))
	}
	live := q.partial(held, "order.hold.3", "500")
	dec := q.reviewed(live, "review.hold.3")
	gone, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(live.ObjectID, dec.RowVersion, "review.hold.3"))
	q.must(err)
	ended, err := q.c.TRD.CancelOrder(q.author, trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: live.ObjectID,
		ExpectedVersion: gone.RowVersion, Reason: "客户撤单", Evidence: q.proof})
	q.must(err)
	if ended.Status != trdapi.OrderCancelled {
		t.Fatalf("active cancel %+v", ended)
	}
	if _, err = q.c.TRD.PreparePartialOrder(q.author, q.partialCmd(held, "order.hold.4", "500")); code(err) != trdapi.CodeQuoteRemainingInsufficient {
		t.Fatalf("active cancel returned quantity: %s", code(err))
	}
	view := q.getQuote(held.QuoteID)
	_, err = q.c.TRD.Cancel(q.author, trdapi.Cancel{IdempotencyKey: q.key(), QuoteID: held.QuoteID, ExpectedVersion: view.RootVersion,
		BusinessRevision: 1, Reason: "报价取消", Evidence: q.proof})
	q.want(err, trdapi.CodeRootConversionLocked)
	n, _, version := q.revisedAndApproved(held.QuoteID, view.RootVersion, trdapi.TargetReplace, "review.hold.replace", "approval.hold.replace")
	_, err = q.c.TRD.Replace(q.author, q.replaceCmd(held.QuoteID, version, n, "approval.hold.replace"))
	q.want(err, trdapi.CodeRootConversionLocked)

	// SIM-03 repeat: 1,200 then 2,000 inside the validity window, no cap.
	open := q.multiAccepted("sim.open", "1200", true)
	one := q.partial(open, "order.open.1", "1200")
	two := q.partial(open, "order.open.2", "2000")
	q.wantPayable(one, expectNet("1200"))
	q.wantPayable(two, expectNet("2000"))
	expires := kernel.Instant(time.Now().Add(30 * time.Second))
	late := q.multiData("1200", false)
	late.ExpiresAt = kernel.FormatTimestamp(expires)
	expired := q.acceptedUntil("sim.late", late, expires)
	time.Sleep(time.Until(expires) + 200*time.Millisecond)
	if _, err := q.c.TRD.PreparePartialOrder(q.author, q.partialCmd(expired, "order.late", "1")); code(err) != trdapi.CodeQuoteExpired {
		t.Fatalf("after expiry: %s", code(err))
	}
}

// Two prepares of 700 against 1,200 serialize so one commits; 500 and 600 both commit.
func TestPartialOrderConsumptionRaces(t *testing.T) {
	q := newOrderWorld(t)
	over := q.multiAccepted("race.over", "1200", false)
	cmds := []trdapi.PreparePartialOrder{q.partialCmd(over, "order.race.1", "700"), q.partialCmd(over, "order.race.2", "700")}
	got := make([]string, len(cmds))
	var wg sync.WaitGroup
	for i, c := range cmds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := q.c.TRD.PreparePartialOrder(q.author, c)
			got[i] = code(err)
		}()
	}
	wg.Wait()
	if s := slices.Sorted(slices.Values(got)); !slices.Equal(s, []string{"OK", trdapi.CodeQuoteRemainingInsufficient}) {
		t.Fatalf("overlapping prepares: %v", got)
	}
	both := q.multiAccepted("race.both", "1200", false)
	pair := []trdapi.PreparePartialOrder{q.partialCmd(both, "order.both.1", "500"), q.partialCmd(both, "order.both.2", "600")}
	got = make([]string, len(pair))
	for i, c := range pair {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := q.c.TRD.PreparePartialOrder(q.author, c)
			got[i] = code(err)
		}()
	}
	wg.Wait()
	if s := slices.Sorted(slices.Values(got)); !slices.Equal(s, []string{"OK", "OK"}) {
		t.Fatalf("non-overlapping prepares: %v", got)
	}
	if n := q.count("company.A", "brant_trd.quote_order_consumption"); n != 3 {
		t.Fatalf("%d consumption rows, want 3", n)
	}
}

// order.prepare_partial over HTTP for a human, and 403 for an AI actor before the body is trusted.
func TestPartialOrderOverHTTPRefusesAI(t *testing.T) {
	q := newOrderWorld(t)
	a := q.multiAccepted("http.multi", "1200", false)
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	human := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "editor", "human")}
	ai := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "editor", "ai")}
	args := map[string]any{"order_id": "order.http.1", "quote_id": a.QuoteID, "business_revision": a.BusinessRevision,
		"business_hash": a.BusinessHash, "acceptance_id": a.ObjectID,
		"lines": []any{map[string]any{"line_id": "L1", "quantity": "700"}}}
	status, body := ai.try("trd", trdapi.Contract, trdapi.CommandPreparePartialOrder, args)
	if status != http.StatusForbidden || body["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI %d %v", status, body)
	}
	got := human.command("trd", trdapi.Contract, trdapi.CommandPreparePartialOrder, args)
	if got["status"] != trdapi.OrderDraft || got["object_id"] != "order.http.1" {
		t.Fatalf("HTTP prepare %+v", got)
	}
	st, quote := human.do("GET", "/api/brant/v1/trd/quotes/"+a.QuoteID, nil)
	ledger, _ := quote["multi_order_ledger"].(map[string]any)
	if st != http.StatusOK || ledger == nil {
		t.Fatalf("quote read %d %v", st, quote["multi_order_ledger"])
	}
}

// An amendment that grows a line of a multi_order quote must fit the remaining cap. The quantity
// this order already consumed is not counted twice. A decrease and a cancel do not return
// quantity. A quote that is not multi_order does not gain a ledger row.
func TestAmendmentStaysInsideTheQuoteCap(t *testing.T) {
	q := newOrderWorld(t)
	capQ := q.multiAccepted("sim.amend", "1200", false)
	first := q.activePartial(capQ, "order.amend.1", "700")
	q.partial(capQ, "order.amend.2", "400")
	v := q.getOrder(first.ObjectID)
	_, err := q.c.TRD.PrepareAmendment(q.author, q.amendCmd(first.ObjectID, "amendment.over", 0, v.OrderRevision, v.OrderHash, "900"))
	if code(err) != trdapi.CodeQuoteRemainingInsufficient || path(err) != "/args/lines/0/quantity" {
		t.Fatalf("prepare past the remaining cap: %s at %s", code(err), path(err))
	}
	q.applyAmend(first.ObjectID, "amendment.fit", "800")
	q.wantLedger(capQ.QuoteID, "1200", "0")
	if _, err = q.c.TRD.PreparePartialOrder(q.author, q.partialCmd(capQ, "order.amend.3", "1")); code(err) != trdapi.CodeQuoteRemainingInsufficient {
		t.Fatalf("the applied increase did not take the remainder: %s", code(err))
	}

	// Prepared while 500 remained, then another order takes 400, so apply must refuse.
	room := q.multiAccepted("sim.late", "1200", false)
	held := q.activePartial(room, "order.late.a", "700")
	hv := q.getOrder(held.ObjectID)
	approved := q.approvedAmendment(held.ObjectID, "amendment.late", hv.OrderRevision, hv.OrderHash, "1000")
	q.partial(room, "order.late.b", "400")
	_, err = q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(held.ObjectID, "amendment.late", approved.RowVersion))
	if code(err) != trdapi.CodeQuoteRemainingInsufficient || path(err) != "/proposal/lines/0/quantity" {
		t.Fatalf("apply after the remainder moved: %s at %s", code(err), path(err))
	}
	q.wantLedger(room.QuoteID, "1100", "100")

	// A decrease does not put quantity back. Rising back through what this order already
	// consumed appends nothing. Growing past that high water appends only the extra.
	down := q.multiAccepted("sim.down", "1200", false)
	one := q.activePartial(down, "order.down.1", "700")
	q.applyAmend(one.ObjectID, "amendment.down", "500")
	q.wantLedger(down.QuoteID, "700", "500")
	if _, err = q.c.TRD.PreparePartialOrder(q.author, q.partialCmd(down, "order.down.2", "501")); code(err) != trdapi.CodeQuoteRemainingInsufficient {
		t.Fatalf("a decrease returned quantity: %s", code(err))
	}
	q.applyAmend(one.ObjectID, "amendment.back", "650")
	q.wantLedger(down.QuoteID, "700", "500")
	up := q.applyAmend(one.ObjectID, "amendment.up", "900")
	q.wantLedger(down.QuoteID, "900", "300")
	qty, _ := revisionLine(t, q.getOrder(one.ObjectID).CurrentRevision)
	if qty != "900" {
		t.Fatalf("order quantity %s", qty)
	}
	ended, err := q.c.TRD.CancelOrder(q.author, trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: one.ObjectID,
		ExpectedVersion: up.OrderRowVersion, Reason: "客户撤单", Evidence: q.proof})
	q.must(err)
	if ended.Status != trdapi.OrderCancelled {
		t.Fatalf("cancel %+v", ended)
	}
	q.wantLedger(down.QuoteID, "900", "300")
	if _, err = q.c.TRD.PreparePartialOrder(q.author, q.partialCmd(down, "order.down.3", "301")); code(err) != trdapi.CodeQuoteRemainingInsufficient {
		t.Fatalf("cancel returned quantity: %s", code(err))
	}

	open := q.multiAccepted("sim.uncap", "1200", true)
	big := q.activePartial(open, "order.uncap.1", "1200")
	q.applyAmend(big.ObjectID, "amendment.uncap", "2500")
	led := q.getQuote(open.QuoteID).MultiOrderLedger
	if led == nil || len(led.Lines) != 1 || !led.Lines[0].Uncapped || led.Lines[0].Consumed != "2500" || led.Lines[0].Remaining != "" {
		t.Fatalf("uncapped ledger %+v", led)
	}

	before := q.count("company.A", "brant_trd.quote_order_consumption")
	plain := q.activeOrder("quote.plaincap")
	q.applyAmend(plain.ObjectID, "amendment.plain", "140")
	if n := q.count("company.A", "brant_trd.quote_order_consumption"); n != before {
		t.Fatalf("a full_v1 amendment wrote the ledger: %d rows, was %d", n, before)
	}
	if view := q.getQuote("quote.plaincap"); view.MultiOrderLedger != nil || view.Conversion == nil || view.Conversion.Slot != "full_v1" {
		t.Fatalf("a default quote after an increase: ledger %+v conversion %+v", view.MultiOrderLedger, view.Conversion)
	}
}

func (q *trdWorld) multiData(quantity string, uncapped bool) trdapi.QuoteData {
	q.ensureMultiBasis(quantity)
	d := q.data()
	d.ConversionMode = "multi_order"
	d.Lines[0].Quantity = quantity
	// TRD-INV-006: the sealed line quantity is the RFQ's and the cost snapshot's, exactly.
	// The world's rfq.1 and its snapshot are 120 PCS; quoting another quantity on them is
	// COMPARISON_BASIS_MISMATCH at /args/data/lines/0/quantity.
	d.RfqRef = &trdapi.RfqRef{RfqID: "rfq.multi", RowVersion: q.multiRfqVersion}
	d.CostSnapshotRef = &trdapi.CostSnapshotRef{SnapshotID: q.multiSnap.SnapshotID, ResultHash: q.multiSnap.ResultHash}
	if uncapped {
		yes := true
		d.Lines[0].Uncapped = &yes
	}
	return d
}

// ensureMultiBasis freezes one actual costing of quantity PCS on a new RFQ, with the world's
// comparison basis (trade terms, payment terms, DEHAM, the handover). Every multi_order quote of
// this world adopts that snapshot. The supplier offer answers this RFQ: offer.1 answers rfq.1 only.
func (q *trdWorld) ensureMultiBasis(quantity string) {
	q.t.Helper()
	if q.multiRfqVersion != 0 {
		if q.multiQty != quantity {
			q.t.Fatalf("comparison basis is %s PCS, not %s", q.multiQty, quantity)
		}
		return
	}
	version, snap := q.freezeBasis("multi", quantity, "")
	q.multiQty = quantity
	q.multiRfqVersion = version
	q.multiSnap = snap
}

// freezeBasis freezes one actual costing of quantity PCS on rfq.<suffix>. profile empty is generic.
// An apparel profile is refused until industry.apparel is active.
func (q *trdWorld) freezeBasis(suffix, quantity, profile string) (int64, cstapi.SnapshotReceipt) {
	q.t.Helper()
	rfqID := "rfq." + suffix
	saved, err := q.c.INQ.SaveRfq(q.author, inqapi.SaveRfq{IdempotencyKey: q.key(), RfqID: rfqID,
		CustomerID: "customer.1", ContactID: "contact.1", SourceIDs: []string{"src.1"}, UnresolvedFields: []string{},
		DocumentProfile: profile,
		Lines:           []inqapi.RfqLine{{LineID: "L1", SkuID: "sku.1", Quantity: quantity, UOM: "PCS", SourceID: "src.1", Locator: "第1段"}}})
	q.must(err)
	ready, err := q.c.INQ.ConfirmRfqScope(q.author, inqapi.ConfirmRfqScope{IdempotencyKey: q.key(), RfqID: rfqID,
		ExpectedVersion: saved.RowVersion, EvidenceIDs: []string{"src.1"}})
	q.must(err)
	offer := q.offer()
	offer.Lines = []supapi.ConditionLine{q.line()}
	offer.Rfq = &supapi.RfqRef{RfqID: rfqID, RowVersion: ready.RowVersion}
	q.verified(supapi.KindSupplierOffer, "offer."+suffix, offer)
	ctxData := q.contextData()
	ctxData.RfqRef = &cstapi.RfqRef{RfqID: rfqID, RowVersion: ready.RowVersion}
	ctxData.Lines[0].Quantity = quantity
	q.verifiedContext("context."+suffix, ctxData)
	scenario := q.scenario("offer."+suffix, "log.1")
	scenario.ContextRef = &cstapi.ContextRef{ContextID: "context." + suffix, Revision: 1}
	scenario.Allocations[0].PurchaseQuantity = quantity
	_, err = q.saveScenario(q.author, "scenario."+suffix, 0, scenario)
	q.must(err)
	ev, err := q.evaluate(q.reviewer, "scenario."+suffix, 1)
	q.must(err)
	snap, err := q.freeze(q.reviewer, ev, 1)
	q.must(err)
	return ready.RowVersion, snap
}

// apparelData is the fixture quote on an apparel RFQ and the cost snapshot of that RFQ.
func (q *trdWorld) apparelData() trdapi.QuoteData {
	q.t.Helper()
	if q.apparelRfqVersion == 0 {
		q.apparelRfqVersion, q.apparelSnap = q.freezeBasis("apparel", "120", "apparel")
	}
	d := q.data()
	d.DocumentProfile = "apparel"
	d.RfqRef = &trdapi.RfqRef{RfqID: "rfq.apparel", RowVersion: q.apparelRfqVersion}
	d.CostSnapshotRef = &trdapi.CostSnapshotRef{SnapshotID: q.apparelSnap.SnapshotID, ResultHash: q.apparelSnap.ResultHash}
	return d
}

func (q *trdWorld) multiAccepted(id, quantity string, uncapped bool) trdapi.AcceptanceReceipt {
	q.t.Helper()
	return q.acceptedOn(id, q.multiData(quantity, uncapped))
}

// acceptedUntil is acceptedOn with the approval lapsing one second before the quote, so a short
// validity still activates (the default approval lasts a day, past the quote).
func (q *trdWorld) acceptedUntil(id string, d trdapi.QuoteData, expires time.Time) trdapi.AcceptanceReceipt {
	q.t.Helper()
	_, err := q.saveQuote(q.author, id, 0, d)
	q.must(err)
	f, err := q.freezeQuote(q.author, id, 1, 1)
	q.must(err)
	_, err = q.submit(q.author, id, f.RowVersion, 1, "review."+id)
	q.must(err)
	decide := q.approval(id, f.RowVersion+1, "review."+id, f.BusinessHash, "approval."+id)
	decide.ExpiresAt = kernel.FormatTimestamp(expires.Add(-time.Second))
	dec, err := q.c.TRD.DecideReview(q.reviewer, decide)
	q.must(err)
	act, err := q.c.TRD.Activate(q.author, q.activateCmd(id, dec.RowVersion, 1, "approval."+id))
	q.must(err)
	_, err = q.c.TRD.RecordResponse(q.author, q.full("response."+id, id, 1, act.BusinessHash))
	q.must(err)
	verify := q.verification("response."+id, "acceptance."+id, 1, act.BusinessHash)
	verify.CustomerRef = *d.CustomerRef
	v, err := q.c.TRD.VerifyResponse(q.reviewer, verify)
	q.must(err)
	return v
}

func (q *trdWorld) partialCmd(a trdapi.AcceptanceReceipt, order, quantity string) trdapi.PreparePartialOrder {
	return trdapi.PreparePartialOrder{IdempotencyKey: q.key(), OrderID: order, QuoteID: a.QuoteID, BusinessRevision: a.BusinessRevision,
		BusinessHash: a.BusinessHash, AcceptanceID: a.ObjectID, Lines: []trdapi.PartialLine{{LineID: "L1", Quantity: quantity}}}
}

func (q *trdWorld) partial(a trdapi.AcceptanceReceipt, order, quantity string) trdapi.OrderReceipt {
	q.t.Helper()
	o, err := q.c.TRD.PreparePartialOrder(q.author, q.partialCmd(a, order, quantity))
	q.must(err)
	return o
}

func (q *trdWorld) activePartial(a trdapi.AcceptanceReceipt, order, quantity string) trdapi.OrderReceipt {
	q.t.Helper()
	o := q.partial(a, order, quantity)
	d := q.reviewed(o, "review."+order)
	act, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o.ObjectID, d.RowVersion, "review."+order))
	q.must(err)
	return act
}

func (q *trdWorld) applyAmend(order, id, quantity string) trdapi.AmendmentReceipt {
	q.t.Helper()
	v := q.getOrder(order)
	d := q.approvedAmendment(order, id, v.OrderRevision, v.OrderHash, quantity)
	got, err := q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(order, id, d.RowVersion))
	q.must(err)
	return got
}

func (q *trdWorld) wantLedger(id, consumed, remaining string) {
	q.t.Helper()
	ledger := q.getQuote(id).MultiOrderLedger
	if ledger == nil || len(ledger.Lines) != 1 || ledger.Lines[0].Consumed != consumed || ledger.Lines[0].Remaining != remaining {
		q.t.Fatalf("ledger of %s: %+v", id, ledger)
	}
}

func (q *trdWorld) wantPayable(o trdapi.OrderReceipt, want string) {
	q.t.Helper()
	v := q.getOrder(o.ObjectID)
	var body map[string]any
	if err := json.Unmarshal([]byte(v.Snapshot), &body); err != nil {
		q.t.Fatal(err)
	}
	calc := body["calculation"].(map[string]any)
	if calc["payable_minor"] != want {
		q.t.Fatalf("payable %v, want %s", calc["payable_minor"], want)
	}
	var sum int64
	for _, raw := range calc["payments"].([]any) {
		sum += mustInt(q.t, raw.(map[string]any)["amount_minor"])
	}
	if strconv.FormatInt(sum, 10) != want {
		q.t.Fatalf("payments sum %d, payable %s", sum, want)
	}
}

// expectNet is quantity × the fixture's 8 USD unit price, tax exclusive at rate 0 (tax.zero),
// HALF_UP to USD minor units.
func expectNet(qty string) string {
	q, err := kernel.ParseDecimal(qty, kernel.QuantityFormat)
	if err != nil {
		panic(err)
	}
	price, err := kernel.ParseDecimal("8", kernel.PriceFormat)
	if err != nil {
		panic(err)
	}
	rate, err := kernel.ParseDecimal("0", kernel.DecimalFormat{IntDigits: 1, Scale: 12})
	if err != nil {
		panic(err)
	}
	split, err := kernel.TaxExclusive(kernel.MustCurrency("USD", 2), q.Mul(price), rate)
	if err != nil {
		panic(err)
	}
	return split.Gross.MinorString()
}
