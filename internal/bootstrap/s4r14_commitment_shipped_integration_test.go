//go:build integration

package bootstrap_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S4 unit R14 on PostgreSQL 16 (PUR-INV-013, the "减已出运" half of S5R-D05; Context Map R82
// CommitmentShipped v1, OHS（事务内无锁读取）, guardLock false). FUL publishes the confirmed shipped
// quantity of one commitment, by order line; PUR's dispose and decide read it after the commitment
// root is locked. Prepared and cancelled shipments do not count. The grain is the commitment line.

func (w *fulWorld) commitmentShippedCount(company, commitmentID string) int {
	w.t.Helper()
	var n int
	w.must(w.execScan(company, `SELECT count(*)::int FROM brant_ful.commitment_shipped_v1('`+commitmentID+`')`, &n))
	return n
}

func (w *fulWorld) commitmentShippedRow(company, commitmentID string) (line, unit, quantity string, revision, shipments int64) {
	w.t.Helper()
	w.must(w.execScan(company, `SELECT order_line_id, unit_id, unit_revision, shipped_quantity, shipments
		FROM brant_ful.commitment_shipped_v1('`+commitmentID+`')`, &line, &unit, &revision, &quantity, &shipments))
	return line, unit, quantity, revision, shipments
}

// CommitmentShipped v1 as the database publishes it (R82): the same security attributes as
// order_shipped_v1, an index on the commitment, and a NULL commitment refused. Prepared and cancelled
// shipments add nothing. Two commitments of one order line are two quantities: what one shipped is
// not the other's, and company.B sees neither.
func TestCommitmentShippedFunction(t *testing.T) {
	w := newFulWorld(t)
	var definer, publicExec, appExec bool
	var config []string
	var index string
	w.must(w.execScan("company.A", `SELECT p.prosecdef, p.proconfig, has_function_privilege('public', p.oid, 'EXECUTE'),
		has_function_privilege('brant_app', p.oid, 'EXECUTE') FROM pg_proc p
		WHERE p.oid = 'brant_ful.commitment_shipped_v1(text)'::regprocedure`, &definer, &config, &publicExec, &appExec))
	if !definer || publicExec || !appExec || !slices.Contains(config, "search_path=pg_catalog, pg_temp") {
		t.Fatalf("definer %v config %v public %v app %v", definer, config, publicExec, appExec)
	}
	w.must(w.execScan("company.A", `SELECT indexdef FROM pg_indexes WHERE schemaname = 'brant_ful' AND indexname = 'shipment_line_commitment'`, &index))
	if !strings.Contains(index, "tenant_id") || !strings.Contains(index, "company_id") || !strings.Contains(index, "commitment_id") {
		t.Fatalf("index %s", index)
	}
	if err := w.exec("company.A", "SELECT * FROM brant_ful.commitment_shipped_v1(NULL)"); sqlCode(err) != "22004" {
		t.Fatalf("NULL: %s", sqlCode(err))
	}

	rel1, rel2 := w.released("commitment.1", "80"), w.released("commitment.2", "70")
	d1, d2 := w.delivered("commitment.1", "80"), w.delivered("commitment.2", "70")
	_, err := w.prepare(w.merch, "shipment.prep", "10", d1, rel1)
	w.must(err)
	if n := w.commitmentShippedCount("company.A", "commitment.1"); n != 0 {
		t.Fatalf("a prepared shipment counts %d", n)
	}
	_, err = w.ful.CancelShipment(w.agent, fulapi.CancelShipment{IdempotencyKey: w.key(), ShipmentID: "shipment.prep", ExpectedVersion: 1,
		Reason: "合成：此票取消，不出运"})
	w.must(err)
	if n := w.commitmentShippedCount("company.A", "commitment.1"); n != 0 {
		t.Fatalf("a cancelled shipment counts %d", n)
	}
	_, err = w.prepare(w.merch, "shipment.1", "50", d1, rel1)
	w.must(err)
	w.granted("shipment.1")
	_, err = w.confirm(w.merch, "shipment.1", 1)
	w.must(err)
	_, err = w.prepare(w.merch, "shipment.2", "70", d2, rel2)
	w.must(err)
	w.granted("shipment.2")
	_, err = w.confirm(w.merch, "shipment.2", 1)
	w.must(err)

	line, unit, quantity, revision, shipments := w.commitmentShippedRow("company.A", "commitment.1")
	if line != "L1" || unit != w.line.UnitRef.ID || revision != w.line.UnitRef.Revision || quantity != "50" || shipments != 1 {
		t.Fatalf("commitment.1 shipped %s %s %d %s %d", line, unit, revision, quantity, shipments)
	}
	_, _, quantity, _, shipments = w.commitmentShippedRow("company.A", "commitment.2")
	if quantity != "70" || shipments != 1 {
		t.Fatalf("commitment.2 shipped %s in %d shipments", quantity, shipments)
	}
	got, err := w.shipped("company.A", w.order.ObjectID)
	want := "L1 " + w.line.UnitRef.ID + " " + strconv.FormatInt(w.line.UnitRef.Revision, 10) + " 120 2"
	if err != nil || len(got) != 1 || got[0] != want {
		t.Fatalf("the order line is both commitments %q, want %s (%v)", got, want, err)
	}
	if n := w.commitmentShippedCount("company.B", "commitment.1"); n != 0 {
		t.Fatalf("company.B sees company.A's shipments %d", n)
	}
}

// A prepared shipment and a cancelled one do not reduce the room. After 60 of 100 are confirmed,
// a request of 41 is refused and a request of 40 is taken and approved; one more is refused.
func TestDispositionRoomIsVerifiedLessConfirmedShipped(t *testing.T) {
	w := newFulPurWorld(t)
	x := w.disposals()
	fl := w.c.FUL
	d := w.verified("delivery.1", "100", "100")
	_, err := w.ship(fl, w.merch, "shipment.prep", "10", d)
	w.must(err)
	_, err = x.dispose(w.author, "disp.prepared", "commitment.1", purapi.DispositionWriteOff, "100")
	w.must(err)
	_, err = x.decide(w.reviewer, "disp.prepared", 1, purapi.DecisionReject, "合成：按出运后再定")
	w.must(err)
	_, err = fl.CancelShipment(w.merch, fulapi.CancelShipment{IdempotencyKey: w.key(), ShipmentID: "shipment.prep", ExpectedVersion: 1,
		Reason: "合成：此票取消，不出运"})
	w.must(err)
	_, err = w.ship(fl, w.merch, "shipment.can", "20", d)
	w.must(err)
	_, err = fl.CancelShipment(w.merch, fulapi.CancelShipment{IdempotencyKey: w.key(), ShipmentID: "shipment.can", ExpectedVersion: 1,
		Reason: "合成：此票也取消"})
	w.must(err)
	_, err = x.dispose(w.author, "disp.cancelled", "commitment.1", purapi.DispositionWriteOff, "100")
	w.must(err)
	_, err = x.decide(w.reviewer, "disp.cancelled", 1, purapi.DecisionReject, "合成：取消的出运不占余量")
	w.must(err)

	w.confirmed(fl, "shipment.1", "60", d)
	_, err = x.dispose(w.author, "disp.over", "commitment.1", purapi.DispositionWriteOff, "41")
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/quantity" || !strings.Contains(reasonOf(err), "verified delivered 100") ||
		!strings.Contains(reasonOf(err), "60 of it shipped") || !strings.Contains(reasonOf(err), "0 of it approved or requested") {
		t.Fatalf("one over the room: %s at %s (%s)", code(err), path(err), reasonOf(err))
	}
	r, err := x.dispose(w.author, "disp.edge", "commitment.1", purapi.DispositionWriteOff, "40")
	w.must(err)
	if r.Status != purapi.DispositionRequested {
		t.Fatalf("40 equals the room %+v", r)
	}
	r, err = x.decide(w.reviewer, "disp.edge", 1, purapi.DecisionApprove, "")
	w.must(err)
	if r.Status != purapi.DispositionApproved {
		t.Fatalf("the decision allows the same 40 %+v", r)
	}
	_, err = x.dispose(w.author, "disp.more", "commitment.1", purapi.DispositionWriteOff, "1")
	refusedAt(t, "nothing left after the 40", err, kernel.CodeInvalidKnownValue, "/args/quantity")
}

// A request that fitted before the shipment is refused at the decision, which is the authority:
// 50 was within the 100 verified, and after 60 shipped it is not within the 40 left.
func TestDispositionRequestedBeforeTheShipmentIsRefusedAtTheDecision(t *testing.T) {
	w := newFulPurWorld(t)
	x := w.disposals()
	fl := w.c.FUL
	d := w.verified("delivery.1", "100", "100")
	_, err := x.dispose(w.author, "disp.early", "commitment.1", purapi.DispositionWriteOff, "50")
	w.must(err)
	_, err = x.dispose(w.author, "disp.one", "commitment.1", purapi.DispositionWriteOff, "41")
	w.must(err)
	w.confirmed(fl, "shipment.1", "60", d)

	_, err = x.decide(w.reviewer, "disp.early", 1, purapi.DecisionApprove, "")
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/disposition/quantity" || !strings.Contains(reasonOf(err), "60 of it shipped") {
		t.Fatalf("the decision after the shipment: %s at %s (%s)", code(err), path(err), reasonOf(err))
	}
	cv, err := w.c.PUR.GetCommitment(w.author, "commitment.1")
	w.must(err)
	if dispositionStatus(cv, "disp.early") != purapi.DispositionRequested {
		t.Fatal("a refused decision leaves the disposition requested")
	}
	_, err = x.decide(w.reviewer, "disp.one", 1, purapi.DecisionApprove, "")
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/disposition/quantity" {
		t.Fatalf("41 is one over the 40 left: %s at %s", code(err), path(err))
	}
	line := purapi.CommitmentLineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}
	lines, err := w.c.PUR.ResolvePurchaseLines(w.author, []purapi.CommitmentLineRef{line})
	w.must(err)
	if lines[line].DisposedQuantity != "0" {
		t.Fatalf("nothing was approved %+v", lines[line])
	}
}

// Two commitments share order line L1. What commitment.a shipped does not reduce commitment.b's room.
func TestOneCommitmentsShipmentDoesNotReduceAnothersRoom(t *testing.T) {
	w := newFulWorld(t)
	px := newPurDeliver(w.scenWorld, func() *pur.Module { return w.c.PUR })
	x := purDispose{w: w.scenWorld, m: func() *pur.Module { return w.c.PUR }}
	src := w.moq40()
	da, err := w.c.PUR.PrepareDraft(w.author, purapi.PrepareDraft{IdempotencyKey: w.key(), DraftID: "purchase.a", OrderID: w.order.ObjectID,
		Lines: []purapi.DraftLine{purLine("80", purSource(src))}, SelectionReason: purReason})
	w.must(err)
	px.issued("commitment.a", da, purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationSourceDiffers, Reason: "合成：MOQ 40 的报价"})
	offerB := w.secondSupplier()
	db, err := w.c.PUR.PrepareDraft(w.author, purapi.PrepareDraft{IdempotencyKey: w.key(), DraftID: "purchase.b", OrderID: w.order.ObjectID,
		Lines: []purapi.DraftLine{purLine("40", purSource(offerB))}, SelectionReason: purReason})
	w.must(err)
	px.issued("commitment.b", db,
		purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationSourceDiffers, Reason: "合成：第二供应商分担交期风险"},
		purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationCostAbove, Reason: "合成：高 12.00 美元，经采购经理同意"})

	release := func(commitment, quantity string) {
		t.Helper()
		ref := qltapi.SubjectRef{Kind: qltapi.SubjectCommitmentLine, ID: commitment, LineID: "L1"}
		_, err := w.c.QLT.RecordInspection(w.qc, qltapi.RecordInspection{IdempotencyKey: w.key(), InspectionID: "insp." + commitment, Subject: ref,
			InspectedQuantity: quantity, ConformingQuantity: quantity, NonconformingQuantity: "0", Sampling: qltapi.Sampling{Method: qltapi.SamplingFull},
			Standard: qltapi.StandardRef{SourceID: "standard"}, Inspector: qltapi.Inspector{Kind: qltapi.InspectorInternal}, ReportFileID: "report",
			InspectedAt: kernel.FormatTimestamp(time.Now().Add(-time.Hour))})
		w.must(err)
		_, err = w.c.QLT.GrantRelease(w.qc, qltapi.GrantRelease{IdempotencyKey: w.key(), ReleaseID: "rel." + commitment, Subject: ref, Quantity: quantity,
			ValidUntil: kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))})
		w.must(err)
	}
	release("commitment.a", "80")
	_, err = px.record(w.merch, "delivery.a", "commitment.a", "80")
	w.must(err)
	_, err = px.verify(w.author, "delivery.a", 1, "80")
	w.must(err)
	_, err = px.record(w.merch, "delivery.b", "commitment.b", "40")
	w.must(err)
	_, err = px.verify(w.author, "delivery.b", 1, "40")
	w.must(err)

	daLine := fulapi.DeliveryLineRef{DeliveryID: "delivery.a", LineID: "L1", Version: 1}
	_, err = w.c.FUL.PrepareShipment(w.merch, fulapi.PrepareShipment{IdempotencyKey: w.key(), ShipmentID: "shipment.a", OrderID: w.order.ObjectID,
		Route: fulapi.RouteSupplierDirect, Lines: []fulapi.ShipmentLine{{LineID: "S1", OrderLineID: "L1", Quantity: "50", SupplierDelivery: daLine,
			QualityReleaseID: "rel.commitment.a"}}})
	w.must(err)
	w.grantedOn(w.c.FUL, "shipment.a", fulapi.ScopeShipment)
	_, err = w.c.FUL.ConfirmShipment(w.merch, w.confirmCmd("shipment.a", 1))
	w.must(err)

	r, err := x.dispose(w.author, "disp.b", "commitment.b", purapi.DispositionWriteOff, "40")
	w.must(err)
	if r.Status != purapi.DispositionRequested {
		t.Fatalf("commitment.b still has its 40 %+v", r)
	}
	_, err = x.dispose(w.author, "disp.a.over", "commitment.a", purapi.DispositionWriteOff, "31")
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/quantity" {
		t.Fatalf("commitment.a's 31 after 50 shipped: %s at %s (%s)", code(err), path(err), reasonOf(err))
	}
	r, err = x.dispose(w.author, "disp.a", "commitment.a", purapi.DispositionWriteOff, "30")
	w.must(err)
	if r.Status != purapi.DispositionRequested {
		t.Fatalf("30 equals 80 verified less 50 shipped %+v", r)
	}
}
