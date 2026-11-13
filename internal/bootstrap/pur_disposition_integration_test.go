//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S5R-04 on PostgreSQL 16: PUR's dispositions of delivered goods and the S5R commitment rules
// (simulated decisions S5R-D05 and S5R-D41, E0, 2026-11-13; tactical model SM-PUR-COMMITMENT-DISPOSITION,
// SM-PUR-COMMITMENT, SM-PUR-COMMITMENT-CHANGE, PUR-INV-009, PUR-INV-011, PUR-INV-012, PUR-INV-013,
// TRD-INV-019; Context Map R71 DownstreamCommitments v1, R31 PurchaseLineRef v1). Python V0.50 has none of
// it: the tests cite the design. The worlds are S5's: newPurWorld (order.1 through PUR's port to an
// in-memory TRD; L1 is 120 PCS, offer.1 sells sku.1 at 5.00 USD, MOQ 100) and newFulPurWorld (TRD's real
// order, commitment.1 issued for all 120, FUL's real shipments).

var purDisposalEvidence = []purapi.Evidence{{SourceID: "evidence", Locator: "合成退货单与工厂签收"}}

// purDispose drives the dispositions of the module m returns, in world w.
type purDispose struct {
	w *scenWorld
	m func() *pur.Module
}

func (x purDispose) cmd(id, commitmentID, kind, quantity string) purapi.DisposeCommitment {
	c := purapi.DisposeCommitment{IdempotencyKey: x.w.key(), DispositionID: id, CommitmentID: commitmentID, OrderLineID: "L1", Disposition: kind,
		Quantity: quantity, Reason: "合成：订单取消，已到货物的去向"}
	if kind == purapi.DispositionReturnToSupplier {
		c.Evidence = purDisposalEvidence
	}
	return c
}

func (x purDispose) dispose(ctx context.Context, id, commitmentID, kind, quantity string) (purapi.DispositionReceipt, error) {
	return x.m().DisposeCommitment(ctx, x.cmd(id, commitmentID, kind, quantity))
}

func (x purDispose) decide(ctx context.Context, id string, version int64, decision, reason string) (purapi.DispositionReceipt, error) {
	return x.m().DecideDisposition(ctx, purapi.DecideDisposition{IdempotencyKey: x.w.key(), DispositionID: id, ExpectedVersion: version,
		Decision: decision, Reason: reason})
}

// approved has requester request disposition id and decider approve it.
func (x purDispose) approved(requester, decider context.Context, id, commitmentID, kind, quantity string) {
	x.w.t.Helper()
	_, err := x.dispose(requester, id, commitmentID, kind, quantity)
	x.w.must(err)
	r, err := x.decide(decider, id, 1, purapi.DecisionApprove, "")
	x.w.must(err)
	if r.Status != purapi.DispositionApproved || r.RowVersion != 2 {
		x.w.t.Fatalf("approved %+v", r)
	}
}

// PUR-INV-013 and SM-PUR-COMMITMENT-DISPOSITION (S5R-D05) on commitment.1, issued for 120 PCS, confirmed,
// 60 verified delivered: a purchaser requests the disposition of delivered goods — an AI agent is
// PERMISSION_DENIED; a return to the supplier cites its evidence; the line's dispositions requested or
// approved stay within the 60 verified (INVALID_KNOWN_VALUE) — and an order reviewer other than the
// requester decides it once (INDEPENDENT_REVIEW_REQUIRED for the requester). The commitment's status and
// version never move. An approval publishes PurchaseCommitmentDisposed with the design's payload and shows
// in DownstreamCommitments v1 (the allocation net of it, a commitment_disposition row), in the commitment
// read and in PurchaseLineRef v1 (R31, for STL's sunk cost, S5R-D42). A delivery correction that would
// leave the line verified below its approved dispositions is refused. The database refuses a decision by
// the requester, a second decision and approvals beyond the verified deliveries.
func TestCommitmentDispositionIsRequestedAndDecidedIndependently(t *testing.T) {
	w := newPurWorld(t)
	c := newPurChange(w.scenWorld, func() *pur.Module { return w.pur })
	x := purDispose{w: w.scenWorld, m: func() *pur.Module { return w.pur }}
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	c.x.issued("commitment.1", d)
	_, err = c.x.p.confirm(w.author, "commitment.1", 3, "")
	w.must(err)
	merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
	_, err = c.x.record(merch, "delivery.1", "commitment.1", "60")
	w.must(err)
	_, err = c.x.verify(w.author, "delivery.1", 1, "60")
	w.must(err)

	agent := w.other("dispose.agent", kernel.ActorAI, allPermissions...)
	_, err = x.dispose(agent, "disp.1", "commitment.1", purapi.DispositionReturnToSupplier, "40")
	w.want(err, kernel.CodePermissionDenied)
	bare := x.cmd("disp.1", "commitment.1", purapi.DispositionReturnToSupplier, "40")
	bare.Evidence = nil
	_, err = w.pur.DisposeCommitment(w.author, bare)
	refusedAt(t, "a return to the supplier without evidence", err, kernel.CodeSchemaInvalid, "/args/evidence")
	_, err = x.dispose(w.author, "disp.1", "commitment.1", purapi.DispositionWriteOff, "61")
	refusedAt(t, "beyond the 60 verified", err, kernel.CodeInvalidKnownValue, "/args/quantity")
	other := x.cmd("disp.1", "commitment.1", purapi.DispositionWriteOff, "1")
	other.OrderLineID = "L9"
	_, err = w.pur.DisposeCommitment(w.author, other)
	refusedAt(t, "a line the commitment does not buy", err, kernel.CodeReferenceInvalid, "/args/order_line_id")
	unread := x.cmd("disp.1", "commitment.1", purapi.DispositionReturnToSupplier, "40")
	unread.Evidence = []purapi.Evidence{{SourceID: "no.such.source", Locator: "x"}}
	_, err = w.pur.DisposeCommitment(w.author, unread)
	refusedAt(t, "evidence nobody reads", err, kernel.CodeReferenceInvalid, "/args/evidence/0/source_id")

	req := x.cmd("disp.1", "commitment.1", purapi.DispositionReturnToSupplier, "40")
	r, err := w.pur.DisposeCommitment(w.author, req)
	w.must(err)
	if r.Status != purapi.DispositionRequested || r.RowVersion != 1 || r.CommitmentID != "commitment.1" || r.OrderID != "order.1" ||
		r.OrderLineID != "L1" || r.Disposition != purapi.DispositionReturnToSupplier || r.Quantity != "40" {
		t.Fatalf("requested %+v", r)
	}
	again, err := w.module().DisposeCommitment(w.author, req)
	if err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("replay %+v %v", again, err)
	}
	taken := x.cmd("disp.1", "commitment.1", purapi.DispositionReturnToSupplier, "39")
	_, err = w.pur.DisposeCommitment(w.author, taken)
	w.want(err, kernel.CodeVersionConflict)
	if status, version := c.x.p.state("commitment.1"); status != purapi.CommitmentConfirmed || version != 4 {
		t.Fatalf("a disposition never moves the commitment: %s %d", status, version)
	}
	if got := c.x.p.downstream("company.A", "order.1"); !slices.Equal(got, []string{"demand_allocation L1  L1 1 allocated 120 ",
		"purchase_commitment commitment.1 purchase.a46  1 confirmed  "}) {
		t.Fatalf("a requested disposition settles nothing %q", got)
	}
	_, err = x.dispose(w.author, "disp.2", "commitment.1", purapi.DispositionWriteOff, "21")
	refusedAt(t, "40 requested and 21 more beyond the 60 verified", err, kernel.CodeInvalidKnownValue, "/args/quantity")

	_, err = x.decide(w.author, "disp.1", 1, purapi.DecisionApprove, "")
	refusedAt(t, "the requester deciding", err, purapi.CodeIndependentReviewRequired, "/args/disposition_id")
	_, err = x.decide(agent, "disp.1", 1, purapi.DecisionApprove, "")
	w.want(err, kernel.CodePermissionDenied)
	_, err = x.decide(w.reviewer, "disp.1", 1, purapi.DecisionReject, "")
	refusedAt(t, "a rejection without its reason", err, kernel.CodeSchemaInvalid, "/args/reason")
	_, err = x.decide(w.reviewer, "disp.1", 2, purapi.DecisionApprove, "")
	w.want(err, kernel.CodeVersionConflict)
	_, err = x.decide(w.reviewer, "disp.none", 1, purapi.DecisionApprove, "")
	w.want(err, kernel.CodeNotFound)
	dec, err := x.decide(w.reviewer, "disp.1", 1, purapi.DecisionApprove, "合成：工厂签收退货")
	w.must(err)
	if dec.Status != purapi.DispositionApproved || dec.RowVersion != 2 {
		t.Fatalf("approved %+v", dec)
	}
	_, err = x.decide(w.reviewer, "disp.1", 2, purapi.DecisionReject, "late")
	w.want(err, kernel.CodeInvalidState)
	if status, version := c.x.p.state("commitment.1"); status != purapi.CommitmentConfirmed || version != 4 {
		t.Fatalf("an approval never moves the commitment: %s %d", status, version)
	}
	events := c.payloads(purapi.EventPurchaseCommitmentDisposed)
	if len(events) != 1 {
		t.Fatalf("disposed events %v", events)
	}
	e := events[0]
	for k, want := range map[string]any{"commitment_id": "commitment.1", "disposition_id": "disp.1", "order_id": "order.1", "order_line_id": "L1",
		"kind": purapi.DispositionReturnToSupplier, "quantity": "40", "unit_id": "PCS", "decided_by": "reviewer",
		"aggregate_type": "PurchaseCommitment", "aggregate_id": "commitment.1", "aggregate_version": float64(4)} {
		if e[k] != want {
			t.Errorf("event %s = %v, want %v", k, e[k], want)
		}
	}
	for _, k := range []string{"draft_id", "supplier_id", "order_revision"} {
		if _, ok := e[k]; ok {
			t.Errorf("the payload is the tactical model's; %s is not in it", k)
		}
	}
	if got := c.x.p.downstream("company.A", "order.1"); !slices.Equal(got, []string{
		"commitment_disposition disp.1 purchase.a46 L1 1 approved 40 return_to_supplier",
		"demand_allocation L1  L1 1 allocated 80 ",
		"purchase_commitment commitment.1 purchase.a46  1 confirmed  "}) {
		t.Fatalf("downstream after the approval %q", got)
	}

	// A second disposition of the same line, rejected: it settles nothing and publishes nothing.
	_, err = x.dispose(w.author, "disp.2", "commitment.1", purapi.DispositionWriteOff, "20")
	w.must(err)
	rej, err := x.decide(w.reviewer, "disp.2", 1, purapi.DecisionReject, "合成：工厂同意折价收回")
	w.must(err)
	if rej.Status != purapi.DispositionRejected || rej.RowVersion != 2 || len(c.payloads(purapi.EventPurchaseCommitmentDisposed)) != 1 {
		t.Fatalf("rejected %+v", rej)
	}
	cv := c.x.p.view(w.reviewer, "commitment.1")
	if len(cv.Dispositions) != 2 || cv.Lines[0].DisposedQuantity != "40" || cv.Lines[0].VerifiedQuantity != "60" {
		t.Fatalf("commitment read %+v %+v", cv.Lines, cv.Dispositions)
	}
	if v := cv.Dispositions[0]; v.DispositionID != "disp.1" || v.Status != purapi.DispositionApproved || v.Disposition != purapi.DispositionReturnToSupplier ||
		v.Quantity != "40" || v.UnitID != "PCS" || v.RequestedBy.ID != "editor" || v.DecidedBy.ID != "reviewer" || v.Decision != purapi.DecisionApprove ||
		len(v.Evidence) != 1 || v.RowVersion != 2 {
		t.Fatalf("approved disposition read %+v", v)
	}
	if v := cv.Dispositions[1]; v.Status != purapi.DispositionRejected || v.Decision != purapi.DecisionReject || v.DecisionReason == "" {
		t.Fatalf("rejected disposition read %+v", v)
	}
	lines, err := w.pur.ResolvePurchaseLines(w.author, []purapi.CommitmentLineRef{{CommitmentID: "commitment.1", OrderLineID: "L1"}})
	w.must(err)
	if l := lines[purapi.CommitmentLineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}]; l.DisposedQuantity != "40" || len(l.Dispositions) != 1 ||
		l.Dispositions[0].DispositionID != "disp.1" || l.Dispositions[0].Disposition != purapi.DispositionReturnToSupplier || l.Dispositions[0].DecidedAt.IsZero() {
		t.Fatalf("PurchaseLineRef v1 %+v", l)
	}

	// PUR-INV-013: a correction of delivery.1 to 30 would leave the 40 disposed of above what is verified.
	_, err = c.x.correct(merch, "delivery.1", "commitment.1", "30", 2)
	w.must(err)
	_, err = c.x.verify(w.author, "delivery.1", 3, "30")
	refusedAt(t, "a correction below the approved dispositions", err, kernel.CodeInvalidState, "/args/lines/0/verified_quantity")
	_, err = c.x.reject(w.author, "delivery.1", 3)
	w.must(err)

	// The database: a requester deciding, a second decision, a deletion, and approvals beyond the verified.
	if err = w.purOwner(`UPDATE brant_pur.purchase_commitment_disposition SET status = 'rejected', row_version = 3, decided_at = now()
		WHERE disposition_id = 'disp.2'`); sqlCode(err) != "23000" {
		t.Fatalf("decided once: %v", err)
	}
	if err = w.purOwner(`DELETE FROM brant_pur.purchase_commitment_disposition WHERE disposition_id = 'disp.2'`); sqlCode(err) != "23000" {
		t.Fatalf("never deleted: %v", err)
	}
	insert := `INSERT INTO brant_pur.purchase_commitment_disposition (tenant_id, company_id, disposition_id, commitment_id, status, row_version, draft_id,
		order_id, order_line_id, kind, quantity, unit_id, unit_revision, reason, evidence, requested_by_kind, requested_by_id, request_epoch,
		requested_at, updated_at) SELECT current_setting('app.tenant_id'), 'company.A', 'disp.db', 'commitment.1', 'requested', 1, 'purchase.a46',
		'order.1', 'L1', 'write_off', 21, unit_id, unit_revision, 'r', '[]', 'human', 'editor', 1, now(), now()
		FROM brant_pur.demand_allocation WHERE order_line_id = 'L1'`
	if err = w.purOwner(insert, `UPDATE brant_pur.purchase_commitment_disposition SET status = 'approved', row_version = 2, decided_by_kind = 'human',
		decided_by_id = 'editor', decide_epoch = 1, decided_at = now(), decision = 'approve' WHERE disposition_id = 'disp.db'`); sqlCode(err) != "23514" {
		t.Fatalf("the requester never decides: %v", err)
	}
	if err = w.purOwner(insert, `UPDATE brant_pur.purchase_commitment_disposition SET status = 'approved', row_version = 2, decided_by_kind = 'human',
		decided_by_id = 'reviewer', decide_epoch = 1, decided_at = now(), decision = 'approve' WHERE disposition_id = 'disp.db'`); sqlCode(err) != "23000" ||
		!strings.Contains(err.Error(), "PUR-INV-013") {
		t.Fatalf("40 and 21 approved exceed the 60 verified: %v", err)
	}
	if err = w.purOwner(strings.Replace(insert, "21, unit_id, unit_revision", "21, 'KG', unit_revision", 1)); sqlCode(err) != "23000" ||
		!strings.Contains(err.Error(), "PUR-INV-012") {
		t.Fatalf("a disposition is in the line's unit: %v", err)
	}
}

// Two purchasers ask for the last 40 of the 60 verified at once: the requests serialize on the commitment
// root (PUR-INV-013 "领域守卫＋承诺根 FOR UPDATE") and one is INVALID_KNOWN_VALUE.
func TestCommitmentDispositionRequestsRaceOnce(t *testing.T) {
	w := newPurWorld(t)
	c := newPurChange(w.scenWorld, func() *pur.Module { return w.pur })
	x := purDispose{w: w.scenWorld, m: func() *pur.Module { return w.pur }}
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	c.x.issued("commitment.1", d)
	merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
	_, err = c.x.record(merch, "delivery.1", "commitment.1", "60")
	w.must(err)
	_, err = c.x.verify(w.author, "delivery.1", 1, "60")
	w.must(err)
	_, err = x.dispose(w.author, "disp.0", "commitment.1", purapi.DispositionWriteOff, "20")
	w.must(err)
	second := w.other("purchaser.2", kernel.ActorHuman, allPermissions...)
	for round := range 3 {
		a, b := "race.a."+strconv.Itoa(round), "race.b."+strconv.Itoa(round)
		got := raceTwo(func() error {
			_, err := x.dispose(w.author, a, "commitment.1", purapi.DispositionHoldForReuse, "40")
			return err
		}, func() error {
			_, err := x.dispose(second, b, "commitment.1", purapi.DispositionHoldForReuse, "40")
			return err
		})
		slices.Sort(got)
		if !slices.Equal(got, []string{kernel.CodeInvalidKnownValue, "OK"}) {
			t.Fatalf("round %d: %v", round, got)
		}
		// Free the 40 again for the next round: the reviewer rejects whichever was requested.
		for _, id := range []string{a, b} {
			if _, err := x.decide(w.reviewer, id, 1, purapi.DecisionReject, "合成：轮次结束"); err != nil && code(err) != kernel.CodeNotFound {
				t.Fatalf("reject %s: %v", id, err)
			}
		}
	}
}

// S5R-D05 end to end on TRD's real order (TRD-INV-019 "为已交付未出运货物保留的需求分配视为未处置，除非 PUR 记录了获批处置";
// positive "已交付未出运的承诺行获批处置后取消"; negative "未获批时取消订单返回 DOWNSTREAM_COMMITMENT_OPEN"): commitment.1 of
// 120 PCS, 60 verified delivered, nothing shipped. The order cannot be cancelled while the commitment is
// issued, nor once it is cancelled (the 60 delivered stay allocated to the order); the purchaser disposes
// of 40 by returning them (over HTTP, the reviewer approving there) — still open for the other 20 — and of
// 20 by writing them off; then order.cancel passes and OrderCancelled names both dispositions in
// downstream_settled. TRD's code is C4a's, unchanged: PUR's answer carries the dispositions.
func TestOrderCancelsOnceItsDeliveredGoodsAreDisposedOf(t *testing.T) {
	w := newFulPurWorld(t)
	q := w.trdWorld
	id := w.order.ObjectID
	x := purDispose{w: w.scenWorld, m: func() *pur.Module { return w.c.PUR }}
	w.verified("delivery.1", "60", "60")
	_, err := q.endCancel(id)
	wantRefused(t, "cancel while the commitment is issued", err, trdapi.CodeDownstreamCommitmentOpen, "/args/order_id")

	cv, err := w.c.PUR.GetCommitment(w.author, "commitment.1")
	w.must(err)
	_, err = w.c.PUR.CancelCommitment(w.author, purapi.CancelCommitment{IdempotencyKey: w.key(), CommitmentID: "commitment.1",
		ExpectedVersion: cv.RowVersion, Reason: "合成：客户撤单，工厂停止生产", Evidence: purChangeEvidence})
	w.must(err)
	_, err = q.endCancel(id)
	wantRefused(t, "cancel with 60 delivered goods allocated", err, trdapi.CodeDownstreamCommitmentOpen, "/args/order_id")
	if !strings.Contains(reasonOf(err), "demand_allocation L1 allocates 60 of line L1, which shipped 0") {
		t.Fatalf("the refusal names the allocation: %s", reasonOf(err))
	}

	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ed := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "editor", "human")}
	rv := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "reviewer", "human"), n: 300}
	evidence := []any{map[string]any{"source_id": "evidence", "locator": "合成退货单与工厂签收"}}
	disp := ed.command("pur", purapi.Contract, purapi.CommandDisposeCommitment, map[string]any{"disposition_id": "disp.return",
		"commitment_id": "commitment.1", "order_line_id": "L1", "disposition": "return_to_supplier", "quantity": "40",
		"reason": "合成：退回工厂", "evidence": evidence})
	if disp["status"] != "requested" || disp["row_version"] != float64(1) || disp["object_id"] != "disp.return" {
		t.Fatalf("dispose over HTTP %v", disp)
	}
	status, out := ed.try("pur", purapi.Contract, purapi.CommandDisposeCommitment, map[string]any{"disposition_id": "disp.more",
		"commitment_id": "commitment.1", "order_line_id": "L1", "disposition": "write_off", "quantity": "21", "reason": "合成"})
	if status != http.StatusUnprocessableEntity || out["code"] != kernel.CodeInvalidKnownValue {
		t.Fatalf("beyond what is verified over HTTP %d %v", status, out)
	}
	status, out = ed.try("pur", purapi.Contract, purapi.CommandDecideDisposition, map[string]any{"disposition_id": "disp.return",
		"expected_version": 1, "decision": "approve"})
	if status != http.StatusUnprocessableEntity || out["code"] != purapi.CodeIndependentReviewRequired {
		t.Fatalf("the requester deciding over HTTP %d %v", status, out)
	}
	decide := map[string]any{"contract": purapi.Contract, "command": purapi.CommandDecideDisposition, "idempotency_key": "pur.http.disposition",
		"args": map[string]any{"disposition_id": "disp.return", "expected_version": 1, "decision": "approve"}}
	status, dec := rv.do("POST", "/api/brant/v1/pur/commands/"+purapi.CommandDecideDisposition, decide)
	if status != http.StatusOK || dec["status"] != "approved" || dec["row_version"] != float64(2) {
		t.Fatalf("decide over HTTP %d %v", status, dec)
	}
	status, rec := rv.do("POST", "/api/brant/v1/pur/commands/"+purapi.CommandDecideDisposition+"/reconcile", decide)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != dec["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	status, cview := ed.do("GET", "/api/brant/v1/pur/purchase_commitments/commitment.1", nil)
	ds, _ := cview["dispositions"].([]any)
	if status != http.StatusOK || len(ds) != 1 || ds[0].(map[string]any)["disposition"] != "return_to_supplier" ||
		ds[0].(map[string]any)["status"] != "approved" || cview["lines"].([]any)[0].(map[string]any)["disposed_quantity"] != "40" {
		t.Fatalf("commitment read over HTTP %d %v", status, cview)
	}

	_, err = q.endCancel(id)
	wantRefused(t, "cancel with 20 delivered goods not disposed of", err, trdapi.CodeDownstreamCommitmentOpen, "/args/order_id")
	if !strings.Contains(reasonOf(err), "demand_allocation L1 allocates 20 of line L1, which shipped 0") {
		t.Fatalf("the refusal names what is left: %s", reasonOf(err))
	}
	x.approved(w.author, w.reviewer, "disp.writeoff", "commitment.1", purapi.DispositionWriteOff, "20")
	if _, err = q.endCancel(id); err != nil {
		t.Fatalf("cancel once the delivered goods are disposed of: %v", err)
	}
	if line, end := q.ended(id); line != "120 0 120" || end.Status != trdapi.OrderCancelled || end.DownstreamSource != trdapi.GuardDownstream {
		t.Fatalf("cancelled %s %+v", line, end)
	}
	if got := q.endEvent(trdapi.EventOrderCancelled)["downstream_settled"]; got !=
		"commitment_disposition:disp.return=return_to_supplier,commitment_disposition:disp.writeoff=write_off" {
		t.Fatalf("OrderCancelled names the dispositions: %v", got)
	}
}

// TRD-INV-019 for an order closed short (S5R-D05, SM-TRD-ORDER close_remainder): 60 verified delivered, 50
// shipped, the commitment cancelled; the 10 delivered and not shipped hold the remainder open until the
// reviewer approves the purchaser's hold_for_reuse of them — instead of shipping them as S5's C4a test did.
func TestCloseRemainderOnceTheUnshippedGoodsAreHeldForReuse(t *testing.T) {
	w := newFulPurWorld(t)
	q := w.trdWorld
	id := w.order.ObjectID
	x := purDispose{w: w.scenWorld, m: func() *pur.Module { return w.c.PUR }}
	d1 := w.verified("delivery.1", "60", "60")
	_, err := w.ship(w.c.FUL, w.merch, "shipment.1", "50", d1)
	w.must(err)
	w.grantedOn(w.c.FUL, "shipment.1", fulapi.ScopeShipment)
	_, err = w.c.FUL.ConfirmShipment(w.merch, w.confirmCmd("shipment.1", 1))
	w.must(err)
	cv, err := w.c.PUR.GetCommitment(w.author, "commitment.1")
	w.must(err)
	_, err = w.c.PUR.CancelCommitment(w.author, purapi.CancelCommitment{IdempotencyKey: w.key(), CommitmentID: "commitment.1",
		ExpectedVersion: cv.RowVersion, Reason: "合成：工厂停产，双方确认取消余量", Evidence: purChangeEvidence})
	w.must(err)
	_, err = q.endShort(id)
	wantRefused(t, "close the remainder with 10 delivered goods not shipped", err, trdapi.CodeDownstreamCommitmentOpen, "/args/order_id")
	x.approved(w.author, w.reviewer, "disp.hold", "commitment.1", purapi.DispositionHoldForReuse, "10")
	if _, err = q.endShort(id); err != nil {
		t.Fatalf("close the remainder once the unshipped goods are held for reuse: %v", err)
	}
	if line, _ := q.ended(id); line != "120 50 70" {
		t.Fatalf("closed short %s", line)
	}
	if got := q.endEvent(trdapi.EventOrderRemainderClosed)["downstream_settled"]; got != "commitment_disposition:disp.hold=hold_for_reuse" {
		t.Fatalf("OrderRemainderClosed names the disposition: %v", got)
	}
}

// S5R-D41 (PUR-INV-009 "只减少数量（含整行降为零）的变更不查供应条件有效期，单价沿用当前修订；增加数量或改价仍须有效的已核实供应条件";
// SM-PUR-COMMITMENT-CHANGE decide guard; closes S5 C11's gap 5): commitment.1 buys 120 PCS under offer.1
// revision 1, 100 of them verified delivered. Once the offer is superseded (SUPPLY_CONDITION_NOT_CURRENT)
// — and in a second world once its validity ended (SUPPLY_CONDITION_EXPIRED, PUR's clock 25 hours on) — a
// change of the supplier date is refused on the lapsed condition, while the short receipt to 100 is
// prepared, decided and applied: its revision document says decrease_only and keeps the unit price, and
// the commitment is fulfilled.
func TestDecreaseOnlyChangeIgnoresTheLapsedSupplyCondition(t *testing.T) {
	for _, lapse := range []struct {
		name string
		code string
		do   func(w *purWorld)
	}{
		{"superseded", supapi.CodeSupplyConditionNotCurrent, func(w *purWorld) {
			_, err := w.revise(w.author, supapi.KindSupplierOffer, "offer.1", 2, 1)
			w.must(err)
			_, err = w.seal(w.reviewer, supapi.KindSupplierOffer, "offer.1", 3, 2, 1)
			w.must(err)
		}},
		{"expired", supapi.CodeSupplyConditionExpired, func(w *purWorld) { w.pur.Clock = purClock{shift: 25 * time.Hour} }},
	} {
		t.Run(lapse.name, func(t *testing.T) {
			w := newPurWorld(t)
			c := newPurChange(w.scenWorld, func() *pur.Module { return w.pur })
			d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
			w.must(err)
			c.x.issued("commitment.1", d)
			merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
			_, err = c.x.record(merch, "delivery.1", "commitment.1", "100")
			w.must(err)
			_, err = c.x.verify(w.author, "delivery.1", 1, "100")
			w.must(err)
			lapse.do(w)

			_, err = c.prepare(w.author, "change.date", "commitment.1", 1, changeLine("120", "offer.1", "2027-01-20"))
			refusedAt(t, "a change of the date on a lapsed condition", err, lapse.code, "/args/lines/0/source")
			_, err = c.prepare(w.author, "change.more", "commitment.1", 1, changeLine("100", "offer.1", "2027-01-20"))
			refusedAt(t, "a decrease with another date", err, lapse.code, "/args/lines/0/source")
			r := c.changed("change.short", "commitment.1", 1, changeLine("100", "offer.1", ""))
			if r.AppliedRevision != 2 {
				t.Fatalf("short receipt %+v", r)
			}
			v := c.view(w.author, "change.short")
			var doc struct {
				Revision struct {
					DecreaseOnly bool `json:"decrease_only"`
				} `json:"revision"`
				PriceGroups []struct {
					PriceKept    bool `json:"price_kept"`
					AppliedTiers []struct {
						UnitPrice string `json:"unit_price"`
					} `json:"applied_tiers"`
					NetMinor string `json:"net_minor"`
				} `json:"price_groups"`
			}
			w.must(json.Unmarshal([]byte(v.Document), &doc))
			if !doc.Revision.DecreaseOnly || len(doc.PriceGroups) != 1 || !doc.PriceGroups[0].PriceKept ||
				doc.PriceGroups[0].AppliedTiers[0].UnitPrice != "5" || doc.PriceGroups[0].NetMinor != "50000" {
				t.Fatalf("decrease-only revision %s", v.Document)
			}
			if cv := c.x.p.view(w.author, "commitment.1"); !cv.Fulfilled || cv.Revision != 2 {
				t.Fatalf("fulfilled after the short receipt %+v", cv)
			}
		})
	}
}

// PUR-INV-012 (S5R-D41 "承诺行单位 = 订单行单位"; negative "单位不同的草稿返回 UNIT_SCOPE_MISMATCH"; S5 refused it with
// DEPENDENCY_NOT_SUPPORTED): a draft whose condition line buys in KG for an order line in PCS is refused.
func TestDraftInAnotherUnitIsRefused(t *testing.T) {
	w := newPurWorld(t)
	kg := w.variant(func(l *supapi.ConditionLine) {
		l.PurchaseUnitRef, l.PriceUnitRef, l.TierUnitRef, l.MOQ.UnitRef = cref(w.kg), cref(w.kg), cref(w.kg), cref(w.kg)
	})
	_, err := w.pur.PrepareDraft(w.author, w.cmd("purchase.kg", purLine("120", purSource(kg))))
	refusedAt(t, "a draft in another unit", err, purapi.CodeUnitScopeMismatch, "/args/lines/0/source")
}
