//go:build integration

package bootstrap_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	clmapi "go.idp.xyz/idp-brant/internal/contexts/clm/api"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	crmapi "go.idp.xyz/idp-brant/internal/contexts/crm/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	mfgapi "go.idp.xyz/idp-brant/internal/contexts/mfg/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	stkapi "go.idp.xyz/idp-brant/internal/contexts/stk/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/interop"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S6-06 on the merged tip (SIM-03, S6-D01..S6-D05). Each story is the place the wave-2
// contracts meet. Commands that have a route are HTTP on the composed process. The five CNX
// channel commands are on that route too (S6-05); this story still drives them in process.
// Material handover stays in process. Fixture setup is the existing worlds; these tests do
// not repeat a unit suite under another name.

// SIM-03: a multi_order quote takes two orders inside the cap and refuses a third.
// An AI actor cannot call order.prepare_partial.
func TestS6E2EPartialOrders(t *testing.T) {
	q := newOrderWorld(t)
	a := q.multiAccepted("e2e.sim", "1200", false)
	base, is := e2eListen(t, q.platform, false)
	human := e2eWho(t, base, q.tenant, is, "editor", "human", 0)
	ai := e2eWho(t, base, q.tenant, is, "editor", "ai", 40)
	first := map[string]any{"order_id": "order.e2e.1", "quote_id": a.QuoteID, "business_revision": a.BusinessRevision,
		"business_hash": a.BusinessHash, "acceptance_id": a.ObjectID,
		"lines": []any{map[string]any{"line_id": "L1", "quantity": "700"}}}
	postCode(t, ai, "trd", trdapi.Contract, trdapi.CommandPreparePartialOrder, first, http.StatusForbidden, kernel.CodePermissionDenied, "")
	got := human.command("trd", trdapi.Contract, trdapi.CommandPreparePartialOrder, first)
	if got["status"] != trdapi.OrderDraft || got["object_id"] != "order.e2e.1" {
		t.Fatalf("first order %v", got)
	}
	second := human.command("trd", trdapi.Contract, trdapi.CommandPreparePartialOrder, map[string]any{
		"order_id": "order.e2e.2", "quote_id": a.QuoteID, "business_revision": a.BusinessRevision,
		"business_hash": a.BusinessHash, "acceptance_id": a.ObjectID,
		"lines": []any{map[string]any{"line_id": "L1", "quantity": "500"}}})
	if second["status"] != trdapi.OrderDraft || second["object_id"] != "order.e2e.2" {
		t.Fatalf("second order %v", second)
	}
	postCode(t, human, "trd", trdapi.Contract, trdapi.CommandPreparePartialOrder, map[string]any{
		"order_id": "order.e2e.3", "quote_id": a.QuoteID, "business_revision": a.BusinessRevision,
		"business_hash": a.BusinessHash, "acceptance_id": a.ObjectID,
		"lines": []any{map[string]any{"line_id": "L1", "quantity": "1"}}},
		http.StatusConflict, trdapi.CodeQuoteRemainingInsufficient, "/args/lines/0/quantity")
	status, quote := human.do(http.MethodGet, "/api/brant/v1/trd/quotes/"+a.QuoteID, nil)
	ledger, _ := quote["multi_order_ledger"].(map[string]any)
	lines, _ := ledger["lines"].([]any)
	row, _ := lines[0].(map[string]any)
	if status != http.StatusOK || len(lines) != 1 || row["consumed"] != "1200" || row["remaining"] != "0" {
		t.Fatalf("ledger %d %v", status, ledger)
	}
}

// Stock route on the composed FUL: FIFO once, replay does not deduct, a direct shipment
// writes no stock movement, and the route is refused while STK is off.
func TestS6E2EStockAndDirectShip(t *testing.T) {
	w := newFulPurWorld(t)
	adminCtx := w.assign("admin.e2e", "administrator")
	_ = w.assign("buyer.e2e", "purchaser")
	base, is := e2eListen(t, w.platform, false)
	admin := e2eWho(t, base, w.tenant, is, "admin.e2e", "human", 0)
	buyer := e2eWho(t, base, w.tenant, is, "buyer.e2e", "human", 20)
	merch := e2eWho(t, base, w.tenant, is, "merch.1", "human", 40)
	sku, unit, unitRev := w.line.Sku.SkuID, w.line.UnitRef.ID, w.line.UnitRef.Revision

	postCode(t, merch, "ful", fulapi.Contract, fulapi.CommandPrepareShipment, map[string]any{
		"shipment_id": "shipment.off", "order_id": w.order.ObjectID, "route": fulapi.RouteStock,
		"lines": []any{e2eStockLine("10")}}, http.StatusUnprocessableEntity, fulapi.CodeDependencyNotSupported, "/args/route")

	enabled := admin.command("stk", stkapi.Contract, stkapi.CommandConfigure, map[string]any{"enabled": true, "expected_version": 0})
	if enabled["status"] != stkapi.StatusEnabled {
		t.Fatalf("enable %v", enabled)
	}
	buyer.command("stk", stkapi.Contract, stkapi.CommandReceive, e2eReceive("mv.e2e.old", sku, unit, unitRev, "40", "1000", "2026-01-01T00:00:00.000000Z"))
	buyer.command("stk", stkapi.Contract, stkapi.CommandReceive, e2eReceive("mv.e2e.new", sku, unit, unitRev, "60", "2500", "2026-02-01T00:00:00.000000Z"))
	if bal := w.stockBalance(adminCtx); !sameQty(t, bal.OnHand, "100") {
		t.Fatalf("received %+v", bal)
	}

	prepared := merch.command("ful", fulapi.Contract, fulapi.CommandPrepareShipment, map[string]any{
		"shipment_id": "shipment.e2e", "order_id": w.order.ObjectID, "route": fulapi.RouteStock,
		"lines": []any{e2eStockLine("50")}})
	if prepared["status"] != fulapi.StatusPrepared {
		t.Fatalf("prepare %v", prepared)
	}
	w.grantedOn(w.c.FUL, "shipment.e2e", fulapi.ScopeShipment)
	ship, err := w.c.FUL.GetShipment(w.merch, "shipment.e2e")
	w.must(err)
	confirmArgs := map[string]any{"shipment_id": "shipment.e2e", "expected_version": ship.RowVersion, "evidence_ids": []string{"evidence"}}
	moves := w.count("company.A", "brant_stk.stock_movement")
	confirmed := e2eCommand(t, merch, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, "e2e.stock.confirm", confirmArgs)
	if confirmed["status"] != fulapi.StatusConfirmed {
		t.Fatalf("confirm %v", confirmed)
	}
	after := w.count("company.A", "brant_stk.stock_movement")
	if after <= moves {
		t.Fatalf("confirm wrote no movement (%d to %d)", moves, after)
	}
	again := e2eCommand(t, merch, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, "e2e.stock.confirm", confirmArgs)
	if again["action_id"] != confirmed["action_id"] || again["row_version"] != confirmed["row_version"] {
		t.Fatalf("replay %v first %v", again, confirmed)
	}
	if got := w.count("company.A", "brant_stk.stock_movement"); got != after {
		t.Fatalf("replay wrote a movement: %d, after confirm %d", got, after)
	}
	if bal := w.stockBalance(adminCtx); !sameQty(t, bal.OnHand, "50") || !sameQty(t, bal.Available, "50") {
		t.Fatalf("balance after 50 %+v", bal)
	}
	stocked, err := w.c.FUL.GetShipment(w.merch, "shipment.e2e")
	w.must(err)
	if evidence := guardValue(stocked, "stk.stock_availability.v1:S1"); !strings.Contains(evidence, "65000") {
		t.Fatalf("stock evidence %q", evidence)
	}
	result, err := w.c.STL.GetOrderResult(w.treasury, w.order.ObjectID)
	w.must(err)
	if result.Live.Confirmed.UnsoldPurchaseCostMinor != "65000" || !hasStockCost(result, "650.00 USD") || len(result.Live.Confirmed.Gaps) != 0 {
		t.Fatalf("cost of sales %+v gaps %v", result.Live.Confirmed, result.Live.Confirmed.Gaps)
	}

	d := w.verified("delivery.e2e", "20", "20")
	directPrep := merch.command("ful", fulapi.Contract, fulapi.CommandPrepareShipment, map[string]any{
		"shipment_id": "shipment.direct", "order_id": w.order.ObjectID, "route": fulapi.RouteSupplierDirect,
		"lines": []any{map[string]any{"line_id": "S1", "order_line_id": "L1", "quantity": "10",
			"supplier_delivery":  map[string]any{"delivery_id": d.DeliveryID, "line_id": d.LineID, "version": d.Version},
			"quality_release_id": w.release}}})
	if directPrep["status"] != fulapi.StatusPrepared {
		t.Fatalf("direct prepare %v", directPrep)
	}
	w.grantedOn(w.c.FUL, "shipment.direct", fulapi.ScopeShipment)
	directShip, err := w.c.FUL.GetShipment(w.merch, "shipment.direct")
	w.must(err)
	direct := e2eCommand(t, merch, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, "e2e.direct.confirm", map[string]any{
		"shipment_id": "shipment.direct", "expected_version": directShip.RowVersion, "evidence_ids": []string{"evidence"}})
	if direct["status"] != fulapi.StatusConfirmed {
		t.Fatalf("direct confirm %v", direct)
	}
	if got := w.count("company.A", "brant_stk.stock_movement"); got != after {
		t.Fatalf("direct ship wrote a movement: %d, stock confirm left %d", got, after)
	}
	if bal := w.stockBalance(adminCtx); !sameQty(t, bal.OnHand, "50") {
		t.Fatalf("direct ship changed stock %+v", bal)
	}
	directView, err := w.c.FUL.GetShipment(w.merch, "shipment.direct")
	w.must(err)
	if guardValue(directView, "stk.stock_availability.v1:S1") != "" {
		t.Fatal("direct ship recorded a stock guard")
	}
}

// Manufacturing on the composed process: a plan, a subcontract that cites the world's
// commitment, a QLT subject of kind mfg_subcontract_operation, and one material handover.
// Progress creates neither qualified stock nor a customer shipment.
func TestS6E2EManufacturingSubcontract(t *testing.T) {
	w := newFulPurWorld(t)
	adminCtx := w.assign("admin.mfg", "administrator")
	buyerCtx := w.assign("buyer.mfg", "purchaser")
	base, is := e2eListen(t, w.platform, false)
	admin := e2eWho(t, base, w.tenant, is, "admin.mfg", "human", 0)
	buyer := e2eWho(t, base, w.tenant, is, "buyer.mfg", "human", 20)
	merch := e2eWho(t, base, w.tenant, is, "merch.1", "human", 40)
	qc := e2eWho(t, base, w.tenant, is, "qc.1", "human", 60)
	sku, spec, unit, unitRev := w.line.Sku.SkuID, w.line.Sku.SpecRevision, w.line.UnitRef.ID, w.line.UnitRef.Revision

	admin.command("mfg", mfgapi.Contract, mfgapi.CommandConfigure, map[string]any{"enabled": true, "expected_version": 0})
	admin.command("stk", stkapi.Contract, stkapi.CommandConfigure, map[string]any{"enabled": true, "expected_version": 0})
	buyer.command("stk", stkapi.Contract, stkapi.CommandReceive, e2eReceive("mv.mfg.recv", "sku.cloth", unit, unitRev, "100", "1000", "2026-01-01T00:00:00.000000Z"))

	published := merch.command("mfg", mfgapi.Contract, mfgapi.CommandPublish, map[string]any{
		"plan_id": "plan.e2e", "expected_version": 0, "target": mfgapi.StatusPublished, "factory_party_id": "factory.1",
		"operations": []any{map[string]any{"operation_id": "op.1", "name": "sew", "sequence": 1, "sku_id": sku,
			"spec_revision": spec, "quantity": "120", "unit_id": unit, "unit_revision": unitRev}},
		"materials": []any{map[string]any{"material_id": "mat.1", "kind": mfgapi.MaterialOurSupply, "sku_id": "sku.cloth",
			"quantity": "120", "unit_id": unit, "unit_revision": unitRev}}})
	if published["status"] != mfgapi.StatusPublished || published["object_id"] != "plan.e2e" {
		t.Fatalf("plan %v", published)
	}
	issued := buyer.command("mfg", mfgapi.Contract, mfgapi.CommandIssue, map[string]any{
		"subcontract_id": "sc.e2e", "plan_id": "plan.e2e", "operation_id": "op.1", "quantity": "10",
		"unit_id": unit, "unit_revision": unitRev, "currency": "USD", "minor_units": 2, "processing_fee_minor": "0",
		"commitment_id": "commitment.1", "order_id": w.order.ObjectID, "order_line_id": "L1"})
	if issued["status"] != mfgapi.StatusIssued {
		t.Fatalf("issue %v", issued)
	}
	sc, err := w.c.MFG.GetSubcontract(adminCtx, "sc.e2e")
	w.must(err)
	if sc.CommitmentID != "commitment.1" || sc.OrderID != w.order.ObjectID || sc.Quantity != "10" {
		t.Fatalf("subcontract %+v", sc)
	}

	planBefore, err := w.c.MFG.GetPlan(adminCtx, "plan.e2e")
	w.must(err)
	moves := w.count("company.A", "brant_stk.stock_movement")
	releases := w.count("company.A", "brant_qlt.quality_release")
	shipments := w.count("company.A", "brant_ful.shipment")
	progressed := merch.command("mfg", mfgapi.Contract, mfgapi.CommandProgress, map[string]any{
		"subcontract_id": "sc.e2e", "progress_id": "pg.e2e", "expected_version": 1, "operation_id": "op.1",
		"progress_kind": mfgapi.StatusInProgress, "quantity": "4", "unit_id": unit, "reported_at": "2026-01-02T00:00:00.000000Z"})
	if progressed["status"] != mfgapi.StatusInProgress {
		t.Fatalf("progress %v", progressed)
	}
	planAfter, err := w.c.MFG.GetPlan(adminCtx, "plan.e2e")
	w.must(err)
	if planAfter.RowVersion != planBefore.RowVersion || planAfter.Status != planBefore.Status {
		t.Fatalf("progress moved the plan from %+v to %+v", planBefore.Status, planAfter.Status)
	}
	if w.count("company.A", "brant_stk.stock_movement") != moves || w.count("company.A", "brant_qlt.quality_release") != releases ||
		w.count("company.A", "brant_ful.shipment") != shipments {
		t.Fatal("progress created stock, a quality release, or a customer shipment")
	}

	inspections := w.count("company.A", "brant_qlt.inspection")
	recorded := qc.command("qlt", qltapi.Contract, qltapi.CommandRecordInspection, map[string]any{
		"inspection_id": "insp.e2e", "subject": map[string]any{"kind": qltapi.SubjectSubcontractOperation, "id": "sc.e2e", "line_id": "op.1"},
		"inspected_quantity": "10", "conforming_quantity": "10", "nonconforming_quantity": "0",
		"sampling":       map[string]any{"method": qltapi.SamplingFull},
		"standard":       map[string]any{"source_id": "standard"},
		"inspector":      map[string]any{"kind": qltapi.InspectorInternal},
		"report_file_id": "report", "inspected_at": "2026-03-01T00:00:00.000000Z"})
	if recorded["status"] != qltapi.StatusRecorded || recorded["object_id"] != "insp.e2e" {
		t.Fatalf("inspection %v", recorded)
	}
	if w.count("company.A", "brant_qlt.inspection") != inspections+1 || w.count("company.A", "brant_qlt.quality_release") != releases ||
		w.count("company.A", "brant_ful.shipment") != shipments {
		t.Fatal("the subcontract inspection granted a release or shipped to the customer")
	}

	bad := mfgHandover("sc.e2e", "mv.e2e.bad", "mv.e2e.bad.in", mfgapi.HandoverTransfer, "loc.1", "loc.factory", "20")
	bad.SkuID, bad.UnitID, bad.UnitRevision = sku, unit, unitRev
	_, err = w.c.MFG.RecordMaterialHandover(buyerCtx, bad)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/sku_id" {
		t.Fatalf("operation sku handed over: %s %s", code(err), path(err))
	}
	if w.count("company.A", "brant_mfg.material_handover") != 0 || w.count("company.A", "brant_stk.stock_movement") != moves {
		t.Fatal("a sku that is not a material was handed over")
	}
	h := mfgHandover("sc.e2e", "mv.e2e", "mv.e2e.in", mfgapi.HandoverTransfer, "loc.1", "loc.factory", "20")
	h.UnitID, h.UnitRevision = unit, unitRev
	handed, err := w.c.MFG.RecordMaterialHandover(buyerCtx, h)
	w.must(err)
	if handed.ObjectID != "mv.e2e" || handed.Status != mfgapi.HandoverTransfer {
		t.Fatalf("handover %+v", handed)
	}
	if w.count("company.A", "brant_mfg.material_handover") != 1 || w.count("company.A", "brant_stk.stock_movement") <= moves ||
		w.count("company.A", "brant_qlt.quality_release") != releases || w.count("company.A", "brant_ful.shipment") != shipments {
		t.Fatal("handover did not stay a stock movement")
	}

	// commitment.1's purchase quantity is 120. op.1 is 200, so the demand cap and the operation cap
	// are different numbers. The sum is the operation's non-cancelled issues, cited or not.
	capped := merch.command("mfg", mfgapi.Contract, mfgapi.CommandPublish, map[string]any{
		"plan_id": "plan.cap", "expected_version": 0, "target": mfgapi.StatusPublished, "factory_party_id": "factory.1",
		"operations": []any{
			map[string]any{"operation_id": "op.1", "name": "sew", "sequence": 1, "sku_id": sku,
				"spec_revision": spec, "quantity": "200", "unit_id": unit, "unit_revision": unitRev},
			map[string]any{"operation_id": "op.2", "name": "pack", "sequence": 2, "sku_id": sku,
				"spec_revision": spec, "quantity": "200", "unit_id": unit, "unit_revision": unitRev},
		},
		"materials": []any{map[string]any{"material_id": "mat.1", "kind": mfgapi.MaterialOurSupply, "sku_id": "sku.cloth",
			"quantity": "200", "unit_id": unit, "unit_revision": unitRev}}})
	if capped["status"] != mfgapi.StatusPublished {
		t.Fatalf("cap plan %v", capped)
	}
	_, err = w.c.MFG.IssueSubcontract(buyerCtx, e2eIssue(w, "sc.cap.u", "plan.cap", "op.1", "80", ""))
	w.must(err)
	_, err = w.c.MFG.IssueSubcontract(buyerCtx, e2eIssue(w, "sc.cap.mix", "plan.cap", "op.1", "50", "commitment.1"))
	if code(err) != kernel.CodeInvalidKnownValue || reasonOf(err) != "The quantity is above the subcontract demand" {
		t.Fatalf("80 uncited plus 50 cited against a demand of 120: %s %q", code(err), reasonOf(err))
	}
	_, err = w.c.MFG.IssueSubcontract(buyerCtx, e2eIssue(w, "sc.cap.c1", "plan.cap", "op.1", "40", "commitment.1"))
	w.must(err)
	_, err = w.c.MFG.IssueSubcontract(buyerCtx, e2eIssue(w, "sc.cap.full", "plan.cap", "op.1", "1", "commitment.1"))
	if code(err) != kernel.CodeInvalidKnownValue || reasonOf(err) != "The quantity is above the subcontract demand" {
		t.Fatalf("demand full: %s %q", code(err), reasonOf(err))
	}
	merch.command("mfg", mfgapi.Contract, mfgapi.CommandProgress, map[string]any{
		"subcontract_id": "sc.cap.c1", "progress_id": "pg.cap.c1", "expected_version": 1, "operation_id": "op.1",
		"progress_kind": mfgapi.StatusCancelled, "quantity": "1", "unit_id": unit, "reported_at": "2026-01-03T00:00:00.000000Z"})
	_, err = w.c.MFG.IssueSubcontract(buyerCtx, e2eIssue(w, "sc.cap.c3", "plan.cap", "op.1", "40", "commitment.1"))
	w.must(err)
	_, err = w.c.MFG.IssueSubcontract(buyerCtx, e2eIssue(w, "sc.cap.rest", "plan.cap", "op.1", "80", ""))
	w.must(err)
	_, err = w.c.MFG.IssueSubcontract(buyerCtx, e2eIssue(w, "sc.cap.over", "plan.cap", "op.1", "1", ""))
	if code(err) != kernel.CodeInvalidKnownValue || reasonOf(err) != "The quantity is above the operation" {
		t.Fatalf("operation full: %s %q", code(err), reasonOf(err))
	}
	_, err = w.c.MFG.IssueSubcontract(buyerCtx, e2eIssue(w, "sc.cap.op2.over", "plan.cap", "op.2", "121", "commitment.1"))
	if code(err) != kernel.CodeInvalidKnownValue || reasonOf(err) != "The quantity is above the subcontract demand" {
		t.Fatalf("other operation shares the sum: %s %q", code(err), reasonOf(err))
	}
	_, err = w.c.MFG.IssueSubcontract(buyerCtx, e2eIssue(w, "sc.cap.op2", "plan.cap", "op.2", "120", "commitment.1"))
	w.must(err)
}

func e2eIssue(w *fulPurWorld, id, planID, operationID, qty, commitment string) mfgapi.IssueSubcontract {
	return mfgapi.IssueSubcontract{IdempotencyKey: "cap." + id, SubcontractID: id, PlanID: planID, OperationID: operationID,
		Quantity: qty, UnitID: w.line.UnitRef.ID, UnitRevision: w.line.UnitRef.Revision, Currency: "USD", MinorUnits: 2,
		ProcessingFeeMinor: "0", CommitmentID: commitment, OrderID: w.order.ObjectID, OrderLineID: "L1"}
}

// A claim on a confirmed shipment line settles one credit. A second settlement of that
// remedy is refused. An open claim blocks order_result.close on /conditions/claims.
// Closing the claim makes that dimension met and leaves the other dimensions as they were.
func TestS6E2EClaimCloseGate(t *testing.T) {
	w := newStatementWorld(t)
	_ = w.assign("reviewer.e2e", "order_reviewer")
	_ = w.assign("clerk.e2e", "sales_clerk")
	w.prepaid()
	w.shipped("shipment.e2e", "120", w.verified("delivery.e2e", "120", "120"))
	_, err := w.c.STL.ConfirmSettlement(w.treasury, salesCmd(w.key(), "statement.e2e", "shipment.e2e"))
	w.must(err)
	_, err = w.c.STL.ReviewOrderResult(w.treasury, stlapi.ReviewOrderResult{IdempotencyKey: w.key(), OrderID: w.order.ObjectID,
		Note: "出运后复核，尚无客诉"})
	w.must(err)
	before, err := w.c.STL.GetOrderResult(w.treasury, w.order.ObjectID)
	w.must(err)
	claims := conditionOf(before.Conditions, stlapi.DimensionClaims)
	if claims.Status != stlapi.ConditionMet || len(claims.Items) != 1 || claims.Items[0] != "no open claim" {
		t.Fatalf("before the claim %+v", claims)
	}
	documents := storyDims(before)

	base, is := e2eListen(t, w.platform, false)
	merch := e2eWho(t, base, w.tenant, is, "merch.1", "human", 0)
	closer := e2eWho(t, base, w.tenant, is, "merch.2", "human", 20)
	review := e2eWho(t, base, w.tenant, is, "reviewer.e2e", "human", 40)
	clerk := e2eWho(t, base, w.tenant, is, "clerk.e2e", "human", 60)
	treasury := e2eWho(t, base, w.tenant, is, "treasury.1", "human", 80)

	opened := merch.command("clm", clmapi.Contract, clmapi.CommandOpenClaim, map[string]any{
		"claim_id": "claim.e2e", "order_id": w.order.ObjectID, "shipment_id": "shipment.e2e",
		"shipment_line_id": "S1", "reason": "客户投诉色差"})
	if opened["status"] != clmapi.StatusOpen {
		t.Fatalf("open %v", opened)
	}
	decided := review.command("clm", clmapi.Contract, clmapi.CommandDecideRemedy, map[string]any{
		"claim_id": "claim.e2e", "remedy_id": "rem.e2e", "kind": clmapi.KindCreditNote, "amount_minor": "10000",
		"reason": "色差折让", "expected_version": 1})
	if decided["status"] != clmapi.StatusOpen {
		t.Fatalf("remedy %v", decided)
	}
	openView, err := w.c.STL.GetOrderResult(w.treasury, w.order.ObjectID)
	w.must(err)
	claims = conditionOf(openView.Conditions, stlapi.DimensionClaims)
	if claims.Status != stlapi.ConditionUnmet || len(claims.Items) != 1 || claims.Items[0] != "claim claim.e2e is open" {
		t.Fatalf("open claim %+v", claims)
	}
	status, out := treasury.try("stl", stlapi.Contract, stlapi.CommandCloseOrderResult, map[string]any{
		"order_id": w.order.ObjectID, "expected_version": openView.Version, "reason": "客诉未结"})
	if status != http.StatusConflict || out["code"] != stlapi.CodeOrderResultNotSettled || !httpHasPath(out, "/conditions/claims") {
		t.Fatalf("close while open %d %v", status, out)
	}

	note := clerk.command("stl", stlapi.Contract, stlapi.CommandRequestCreditNote, map[string]any{
		"credit_note_id": "cn.e2e", "kind": stlapi.CreditPriceAllowance, "order_id": w.order.ObjectID,
		"statement_id": "statement.e2e", "amount": "100.00", "basis": "色差折让", "evidence_ids": []string{"evidence"},
		"claim_id": "claim.e2e", "remedy_id": "rem.e2e"})
	if note["status"] != stlapi.CreditRequested {
		t.Fatalf("credit %v", note)
	}
	postCode(t, clerk, "stl", stlapi.Contract, stlapi.CommandRequestCreditNote, map[string]any{
		"credit_note_id": "cn.e2e.2", "kind": stlapi.CreditPriceAllowance, "order_id": w.order.ObjectID,
		"statement_id": "statement.e2e", "amount": "40.00", "basis": "再次折让", "evidence_ids": []string{"evidence"},
		"claim_id": "claim.e2e", "remedy_id": "rem.e2e"},
		http.StatusConflict, stlapi.CodeClaimRemedyAlreadySettled, "/args/remedy_id")

	withNote, err := w.c.STL.GetOrderResult(w.treasury, w.order.ObjectID)
	w.must(err)
	closed := closer.command("clm", clmapi.Contract, clmapi.CommandCloseClaim, map[string]any{
		"claim_id": "claim.e2e", "expected_version": 2, "summary": "折让已决，结案"})
	if closed["status"] != clmapi.StatusClosed {
		t.Fatalf("claim close %v", closed)
	}
	after, err := w.c.STL.GetOrderResult(w.treasury, w.order.ObjectID)
	w.must(err)
	claims = conditionOf(after.Conditions, stlapi.DimensionClaims)
	if claims.Status != stlapi.ConditionMet || len(claims.Items) != 1 || claims.Items[0] != "no open claim" {
		t.Fatalf("after close %+v", claims)
	}
	if storyDims(after) != storyDims(withNote) {
		t.Fatalf("claim close moved documents from %s to %s", storyDims(withNote), storyDims(after))
	}
	if storyDims(after) != documents {
		t.Fatalf("documents changed from %s to %s", documents, storyDims(after))
	}
}

// CRM on stores an opportunity cited by an RFQ. CRM off still saves an RFQ that omits one,
// and refuses an RFQ that supplies one. This does not rebuild BRC-001.
func TestS6E2ECRMOnAndOff(t *testing.T) {
	t.Run("on", func(t *testing.T) {
		w := newWorld(t)
		e2eSales(w, "sales.e2e")
		base, is := e2eListen(t, w.platform, false)
		sales := e2eWho(t, base, w.tenant, is, "sales.e2e", "human", 0)
		created := sales.command("crm", crmapi.Contract, crmapi.CommandAdvanceOpportunity, map[string]any{
			"opportunity_id": "opp.e2e", "expected_version": 0, "title": "E2E shirts"})
		if created["status"] != crmapi.StageQualified || created["object_id"] != "opp.e2e" {
			t.Fatalf("opportunity %v", created)
		}
		sales.command("inq", inqapi.Contract, inqapi.CommandSaveRfq, map[string]any{
			"id": "rfq.e2e", "expected_version": 0, "opportunity_id": "opp.e2e"})
		status, rfq := sales.do(http.MethodGet, "/api/brant/v1/inq/rfqs/rfq.e2e", nil)
		if status != http.StatusOK || rfq["opportunity_id"] != "opp.e2e" {
			t.Fatalf("rfq %d %v", status, rfq)
		}
	})

	t.Run("off", func(t *testing.T) {
		w := newWorldWith(t, bootstrap.PlatformConfig{DisableCRM: true})
		if w.c.CRM != nil {
			t.Fatal("CRM was constructed")
		}
		e2eSales(w, "sales.off")
		base, is := e2eListen(t, w.platform, true)
		sales := e2eWho(t, base, w.tenant, is, "sales.off", "human", 0)
		sales.command("inq", inqapi.Contract, inqapi.CommandSaveRfq, map[string]any{"id": "rfq.off", "expected_version": 0})
		status, plain := sales.do(http.MethodGet, "/api/brant/v1/inq/rfqs/rfq.off", nil)
		if status != http.StatusOK || plain["opportunity_id"] != nil {
			t.Fatalf("plain rfq %d %v", status, plain)
		}
		postCode(t, sales, "inq", inqapi.Contract, inqapi.CommandSaveRfq, map[string]any{
			"id": "rfq.off.cite", "expected_version": 0, "opportunity_id": "opp.e2e"},
			http.StatusUnprocessableEntity, inqapi.CodeDependencyNotSupported, "/args/opportunity_id")
	})
}

// CNX channel, in process, on the fake port. A timeout is unknown and is not success.
// A second submit while unknown does not file again. A stale receipt stays on the channel
// chain and does not move the case. not_received allows a new attempt.
func TestS6E2EChannelUnknownAndStale(t *testing.T) {
	w := newInvoiceWorld(t)
	doc := w.declare.doc
	past := time.Now().Add(-2 * time.Hour).UTC()
	evidence := []string{w.declare.evidence}
	clerk := "合成报关员（测试）"
	receivedAt := kernel.FormatTimestamp(time.Now().Add(-time.Minute))
	exportedAt := kernel.FormatTimestamp(time.Now().Add(-2 * time.Hour))

	w.dlot("lot.e2e.unk", "u1", "10", "PCS")
	heldCase := w.declare.confirmed("lot.e2e.unk", items("u1"))
	fake := &interop.Fake{Now: func() time.Time { return past }, Script: []interop.Step{{
		Behaviour: interop.BehaviourTimeoutHeld, Outcome: cnxapi.DeclarationAccepted, Sequence: 2, StatusCode: "SYN-ACCEPTED",
		OriginalText: "合成海关回执：受理（测试数据）", MappingVersion: "syn-customs-status.v1", DeclarationNumber: "SYN-DECL-E2E"}}}
	w.cnx.Interop = fake
	unknown, err := w.cnx.SubmitDeclarationChannel(doc, cnxapi.SubmitDeclarationChannel{IdempotencyKey: w.key(), DeclarationID: heldCase.DeclarationID,
		ExpectedVersion: heldCase.RowVersion, SubmitterName: clerk, EvidenceIDs: evidence})
	w.must(err)
	if unknown.Status != cnxapi.DeclarationUnknown || unknown.ConnectorState != string(interop.Unknown) || unknown.OperationID == "" {
		t.Fatalf("timeout was not unknown %+v", unknown)
	}
	if fake.Submits != 1 {
		t.Fatalf("timeout filed %d times", fake.Submits)
	}
	log, err := w.cnx.ChannelLog(doc, interop.ChannelCustomsDeclaration, heldCase.DeclarationID)
	w.must(err)
	heldVersion := log.Attempt.RowVersion
	_, err = w.cnx.SubmitDeclarationChannel(doc, cnxapi.SubmitDeclarationChannel{IdempotencyKey: w.key(), DeclarationID: heldCase.DeclarationID,
		ExpectedVersion: unknown.RowVersion, SubmitterName: clerk, EvidenceIDs: evidence})
	if code(err) != cnxapi.CodeExternalResultUnknown {
		t.Fatalf("second submit %s %s", code(err), path(err))
	}
	if fake.Submits != 1 {
		t.Fatalf("second submit filed again: %d", fake.Submits)
	}
	log, err = w.cnx.ChannelLog(doc, interop.ChannelCustomsDeclaration, heldCase.DeclarationID)
	w.must(err)
	if log.Attempt.State != string(interop.Unknown) || log.Attempt.RowVersion != heldVersion || log.Attempt.OperationID != unknown.OperationID {
		t.Fatalf("attempt moved %+v", log.Attempt)
	}

	w.dlot("lot.e2e.lost", "l1", "10", "PCS")
	lost := w.declare.confirmed("lot.e2e.lost", items("l1"))
	w.cnx.Interop = &interop.Fake{Now: func() time.Time { return past }, Script: []interop.Step{{Behaviour: interop.BehaviourTimeoutLost}}}
	lostAns, err := w.cnx.SubmitDeclarationChannel(doc, cnxapi.SubmitDeclarationChannel{IdempotencyKey: w.key(), DeclarationID: lost.DeclarationID,
		ExpectedVersion: lost.RowVersion, SubmitterName: clerk, EvidenceIDs: evidence})
	w.must(err)
	if lostAns.Status != cnxapi.DeclarationUnknown || lostAns.ConnectorState != string(interop.Unknown) {
		t.Fatalf("lost %+v", lostAns)
	}
	miss, err := w.cnx.ReconcileDeclarationChannel(doc, cnxapi.ReconcileDeclarationChannel{IdempotencyKey: w.key(), DeclarationID: lost.DeclarationID,
		ExpectedVersion: lostAns.RowVersion, EvidenceIDs: evidence, Reason: "海关查询无此申报（测试）"})
	w.must(err)
	if miss.Status != cnxapi.DeclarationNotReceived || miss.ConnectorState != string(interop.NotReceived) || miss.OperationID != lostAns.OperationID {
		t.Fatalf("not received %+v", miss)
	}
	next := w.declare.confirmed("lot.e2e.lost", items("l1"))
	if next.DeclarationID == lost.DeclarationID || next.Attempt != 2 {
		t.Fatalf("new attempt %+v", next)
	}
	w.cnx.Interop = &interop.Fake{Now: func() time.Time { return past }, Script: []interop.Step{{
		Behaviour: interop.BehaviourAccept, Outcome: cnxapi.DeclarationAccepted, Sequence: 1, StatusCode: "SYN-ACCEPTED",
		OriginalText: "合成海关回执：受理（测试数据）", MappingVersion: "syn-customs-status.v1", DeclarationNumber: "SYN-DECL-E2E-2"}}}
	again, err := w.cnx.SubmitDeclarationChannel(doc, cnxapi.SubmitDeclarationChannel{IdempotencyKey: w.key(), DeclarationID: next.DeclarationID,
		ExpectedVersion: next.RowVersion, SubmitterName: clerk, EvidenceIDs: evidence})
	w.must(err)
	if again.Status != cnxapi.DeclarationAccepted || again.ConnectorState != string(interop.Succeeded) || again.OperationID == miss.OperationID {
		t.Fatalf("retry %+v", again)
	}
	old, err := w.cnx.ChannelLog(doc, interop.ChannelCustomsDeclaration, lost.DeclarationID)
	w.must(err)
	if old.Attempt.State != string(interop.NotReceived) || old.Attempt.OperationID != miss.OperationID {
		t.Fatalf("old attempt overwritten %+v", old.Attempt)
	}

	w.dlot("lot.e2e.stale", "s1", "10", "PCS")
	sub := w.declare.submitted("lot.e2e.stale", items("s1"))
	receipts := w.count("company.A", "brant_cnx.declaration_receipt")
	release := cnxapi.ChannelReceiptIn{Outcome: cnxapi.DeclarationReleased, Sequence: 2, StatusCode: "SYN-released",
		OriginalText: "合成海关回执：released（测试数据）", MappingVersion: "syn-customs-status.v1", DeclarationNumber: "SYN-DECL-E2E-S",
		ReceivedAt: receivedAt, ExportedAt: exportedAt}
	stale := cnxapi.ChannelReceiptIn{Outcome: cnxapi.DeclarationAccepted, Sequence: 1, StatusCode: "SYN-accepted",
		OriginalText: "合成海关回执：accepted（测试数据）", MappingVersion: "syn-customs-status.v1", DeclarationNumber: "SYN-DECL-E2E-S",
		ReceivedAt: receivedAt}
	ing, err := w.cnx.IngestDeclarationReceipts(doc, cnxapi.IngestDeclarationReceipts{IdempotencyKey: w.key(), DeclarationID: sub.DeclarationID,
		ExpectedVersion: sub.RowVersion, Receipts: []cnxapi.ChannelReceiptIn{release, stale}})
	w.must(err)
	if ing.Status != cnxapi.DeclarationReleased || ing.LastSequence != 2 || len(ing.Chain) != 2 {
		t.Fatalf("ingest %+v chain %d", ing.Status, len(ing.Chain))
	}
	if !ing.Chain[0].Authoritative || ing.Chain[0].Sequence != 2 || ing.Chain[0].RefusalCode != "" {
		t.Fatalf("release %+v", ing.Chain[0])
	}
	if ing.Chain[1].Authoritative || ing.Chain[1].RefusalCode != cnxapi.CodeDeclarationReceiptStale || ing.Chain[1].Sequence != 1 {
		t.Fatalf("stale %+v", ing.Chain[1])
	}
	if got := w.count("company.A", "brant_cnx.declaration_receipt"); got != receipts+1 {
		t.Fatalf("declaration receipts %d, before %d", got, receipts)
	}
	view := w.declaration(sub.DeclarationID)
	if view.Status != cnxapi.DeclarationReleased || view.LastSequence != 2 {
		t.Fatalf("case moved to %+v sequence %d", view.Status, view.LastSequence)
	}
}

func e2eListen(t *testing.T, platform *bootstrap.Platform, disableCRM bool) (string, *issuer) {
	t.Helper()
	is := newIssuer(t)
	handler, err := platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true, DisableCRM: disableCRM})
	if err != nil {
		t.Fatal(err)
	}
	return newHTTP(t, handler), is
}

func e2eWho(t *testing.T, base, tenant string, is *issuer, actor, kind string, n int) *client {
	t.Helper()
	return &client{t: t, base: base, token: is.token(tenant, "company.A", actor, kind), n: n}
}

func e2eCommand(t *testing.T, c *client, bc, contract, command, key string, args map[string]any) map[string]any {
	t.Helper()
	status, out := c.do(http.MethodPost, "/api/brant/v1/"+bc+"/commands/"+command, map[string]any{
		"contract": contract, "command": command, "idempotency_key": key, "args": args})
	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("%s %s: %d %v", bc, command, status, out)
	}
	return out
}

func e2eSales(w *world, id string) {
	w.t.Helper()
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id},
		Permissions: []string{crmapi.CommandAdvanceOpportunity, "crm.read", inqapi.CommandSaveRfq, "inq.read"},
		DataScope:   "company", Team: "trade"})
}

func e2eReceive(id, sku, unit string, unitRev int64, qty, cost, at string) map[string]any {
	return map[string]any{"movement_id": id, "sku_id": sku, "location_id": "loc.1", "batch_id": "batch.1",
		"custody_kind": stkapi.CustodyOwnWarehouse, "quantity": qty, "unit_id": unit, "unit_revision": unitRev,
		"currency": "USD", "minor_units": 2, "unit_cost_minor": cost, "price_basis_qty": "1", "received_at": at}
}

func e2eStockLine(qty string) map[string]any {
	return map[string]any{"line_id": "S1", "order_line_id": "L1", "quantity": qty,
		"stock": map[string]any{"location_id": "loc.1", "batch_id": "batch.1", "custody_kind": stkapi.CustodyOwnWarehouse}}
}

func storyDims(v stlapi.OrderResultView) string {
	out := ""
	for _, dim := range []string{stlapi.DimensionFulfilment, stlapi.DimensionReceivables, stlapi.DimensionDirectCharges} {
		c := conditionOf(v.Conditions, dim)
		out += dim + "=" + c.Status + "[" + strings.Join(c.Items, ",") + "];"
	}
	return out
}

func httpHasPath(body map[string]any, want string) bool {
	errs, _ := body["field_errors"].([]any)
	for _, raw := range errs {
		field, _ := raw.(map[string]any)
		if field["path"] == want {
			return true
		}
	}
	return false
}
