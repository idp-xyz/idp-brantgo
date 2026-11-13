//go:build integration

package bootstrap_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S5 unit D2b (S5 plan §5 "Companion tests (same harness)" and "Full-depth companions (D2)";
// plan §4, the facts line of D2): the companion scenarios of the S5 acceptance chain, each end to end
// on PostgreSQL 16 over the HTTP API with OIDC access tokens of the test's local issuer and
// brant-worker -dispatch in process, on D1's harness (newBRC001S5: the BRC-001 chain's humans, the
// relay into the composed contexts' consumers, every command a POST of the closed envelope by the
// human the step names, every read a GET route). Every unit the scenarios rest on has its own PG16
// test; these tests do not repeat them in detail: they run each scenario's commands as the archive's
// humans over HTTP and cite the unit's test for the rest.
//
// Each scenario runs in a tenant of its own. With BRANT_S5_SCENARIOS_REPORT set to a directory each
// test writes <scenario>.json there: the scenario, the test, the units it rests on and their tests,
// the figures observed, the deviations from the plan, what was not executed and why, and the chain
// record (every request with actor, route, status and outcome; the relay runs).

// s5scMeta is what a scenario's evidence record states besides the chain.
type s5scMeta struct {
	ID         string   `json:"scenario"`
	Title      string   `json:"title"`
	Design     []string `json:"design"`     // the design IDs the scenario proves
	RestsOn    []string `json:"rests_on"`   // the S5 units
	UnitTests  []string `json:"unit_tests"` // the units' own PG16 tests, cited for detail
	Deviations []string `json:"deviations,omitempty"`
	Omitted    []string `json:"omitted,omitempty"` // parts of the case not executed, and why
	// Humans are the scenario's humans besides D1's, by role template (assigned before the first command).
	Humans map[string]string `json:"humans,omitempty"`
}

// s5scEvidence is the record of one scenario (BRANT_S5_SCENARIOS_REPORT/<scenario>.json).
type s5scEvidence struct {
	s5scMeta
	Test        string            `json:"test"`
	Executed    bool              `json:"executed"`
	Result      string            `json:"result"`
	Figures     map[string]string `json:"figures"`
	NotExecuted []string          `json:"not_executed"`
	Requests    int               `json:"requests"`
	Chain       brcRecord         `json:"chain"`
}

// s5sc drives one scenario: D1's S5 chain driver and the scenario's evidence.
type s5sc struct {
	*brcS5
	meta    s5scMeta
	figures map[string]string
	order   []string // figure keys in the order they were stated

	// The deal builder's tenant (s5sc_deal_integration_test.go).
	tenant         s5scTenant
	dealCurrencies map[string]map[string]any
	creditPolicy   map[string]any
}

// newS5SC is newBRC001S5 in a tenant of its own, with the scenario's record.
func newS5SC(t *testing.T, meta s5scMeta) *s5sc {
	c := newBRC001S5(t)
	c.report = "BRANT_S5SC_CHAIN_UNUSED" // the scenario writes its own record
	c.record.Scenario, c.record.WBS, c.record.Parity = meta.ID, "S5 D2b (S5 plan §5 companions)", strings.Join(meta.Design, ", ")
	for _, actor := range s5scSortedKeys(func() map[string]any {
		m := map[string]any{}
		for k := range meta.Humans {
			m[k] = nil
		}
		return m
	}()) {
		c.w.assign(actor, meta.Humans[actor])
		c.record.Actors[actor] = meta.Humans[actor]
	}
	s := &s5sc{brcS5: c, meta: meta, figures: map[string]string{}}
	t.Cleanup(s.write)
	return s
}

// figure records an observed figure of the scenario.
func (s *s5sc) figure(key, value string) {
	if _, ok := s.figures[key]; !ok {
		s.order = append(s.order, key)
	}
	s.figures[key] = value
}

// want requires got to be want and records it as the figure key.
func (s *s5sc) want(key string, got any, want string) {
	s.t.Helper()
	g := fmt.Sprint(got)
	if g != want {
		s.t.Fatalf("%s: got %s, want %s", key, g, want)
	}
	s.figure(key, g)
}

func (s *s5sc) write() {
	dir := os.Getenv("BRANT_S5_SCENARIOS_REPORT")
	if dir == "" {
		return
	}
	s.record.Dispatches = s.runs
	s.record.FinishedAt = kernel.FormatTimestamp(time.Now())
	e := s5scEvidence{s5scMeta: s.meta, Test: s.t.Name(), Executed: true, Result: "passed", Figures: s.figures,
		NotExecuted: append(slices.Clone(s.meta.Omitted), s.record.NotExecuted...), Requests: len(s.record.Steps), Chain: s.record}
	if s.t.Failed() {
		e.Result = "failed"
	}
	e.Chain.Result = e.Result
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(e); err != nil {
		s.t.Errorf("encode the scenario record: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, s.meta.ID+".json"), out.Bytes(), 0o644); err != nil {
		s.t.Errorf("write the scenario record: %v", err)
	}
}

// release reads commercial release id as doc.1.
func (s *s5sc) release(id string) map[string]any {
	return s.get(brcDocumentation, "/api/brant/v1/ful/commercial_releases/"+id)
}

// evaluate has doc.1 evaluate the commercial release of shipment.1's scope and returns the receipt
// and the evaluation it recorded.
func (s *s5sc) evaluate(scope string) (map[string]any, map[string]any) {
	s.t.Helper()
	r := s.cmd(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandEvaluateRelease, map[string]any{"shipment_id": s.shipmentID, "scope": scope})
	return r, s.releaseEvaluation(brcStr(r["object_id"]))
}

// fund has treasury record a receipt of customer.1 on bank.usd on the advice it registers (S5R-D26).
func (s *s5sc) fund(id, received, remitted, reference, advice string) map[string]any {
	s.t.Helper()
	s.source(brcTreasury, "bank."+id, advice)
	args := map[string]any{"fund_record_id": id, "kind": stlapi.FundReceipt, "counterparty_id": "customer.1",
		"currency": cfgRef("currency", "USD", 1), "bank_account": s.bank, "received_amount": received, "value_date": now(),
		"bank_reference": reference, "evidence_ids": []string{"bank." + id}}
	if remitted != "" {
		args["remitted_amount"] = remitted
	}
	return s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, args)
}

// applyStage has treasury apply amount of fund record to order.1's payment stage.
func (s *s5sc) applyStage(id, fund, stage, amount string) map[string]any {
	return map[string]any{"application_id": id, "fund_record_id": fund,
		"target": map[string]any{"kind": stlapi.TargetOrderStage, "order_id": "order.1", "stage_id": stage}, "amount": amount}
}

// account reads order.1's STL order account as treasury.
func (s *s5sc) account() map[string]any {
	return s.get(brcTreasury, "/api/brant/v1/stl/order_accounts/order.1")
}

// activateWithoutCredit is the S3 chain's order step for terms with no stage after shipment: sales
// prepares order.1 and requests its review, order.reviewer decides it, order.check answers
// TRADE_COMPLIANCE_NOT_CLEAR until compliance concludes clear (the S3 chain's tradeCompliance), then
// nothing more — no credit reservation is required (TRD-INV-022 asks one only for a stage after
// shipment; D-20) — and sales activates it.
func (s *s5sc) activateWithoutCredit() {
	b := s.brc001
	b.phase = "TRD sales order"
	const trd = trdapi.Contract
	if r := b.cmd(brcSales, "trd", trd, "order.prepare", map[string]any{"order_id": "order.1", "quote_id": "quote.1", "business_revision": 1,
		"business_hash": b.quoteHash, "acceptance_id": "acceptance.1"}); r["status"] != trdapi.OrderDraft {
		b.t.Fatalf("order.prepare %v", r)
	}
	o := b.get(brcSales, "/api/brant/v1/trd/orders/order.1")
	b.orderHash, b.orderRevision = brcStr(o["order_hash"]), brcNum(o["order_revision"])
	b.cmd(brcSales, "trd", trd, "order.review.request", map[string]any{"order_id": "order.1", "expected_version": 1, "review_id": "review.order.1"})
	b.cmd(brcOrderReviewer, "trd", trd, "order.review.decide", map[string]any{"order_id": "order.1", "expected_version": 2,
		"review_id": "review.order.1", "order_hash": b.orderHash, "decision": trdapi.DecisionApprove,
		"expires_at": kernel.FormatTimestamp(time.Now().Add(24 * time.Hour)), "reason": "订单与已接受的报价一致"})
	b.wantCheck(cnxapi.CodeTradeComplianceNotClear)
	b.tradeCompliance()
	b.phase = "TRD sales order"
	check := b.get(brcSales, "/api/brant/v1/trd/orders/order.1/check?review_id=review.order.1")
	if check["activatable"] != true || len(brcList(check["unchecked"])) != 0 {
		b.t.Fatalf("order check %v", check)
	}
	b.fact("activatable", true, "credit", "no stage after shipment: no reservation")
	if r := b.cmd(brcSales, "trd", trd, "order.activate", map[string]any{"order_id": "order.1", "expected_version": 3,
		"review_id": "review.order.1"}); r["status"] != trdapi.OrderActive {
		b.t.Fatalf("order.activate %v", r)
	}
	o = b.get(brcSales, "/api/brant/v1/trd/orders/order.1")
	entries := brcMap(brcDig(o, "commits", 0, "entries"))
	if !strings.HasPrefix(brcStr(entries[trdapi.GuardCNXTradeCompliance]), "clear/cleared/") || entries[trdapi.EvidenceCreditReservation] != nil {
		b.t.Fatalf("the activation's commit evidence %v", entries)
	}
	b.fact("credit_guard", entries[trdapi.GuardSTLCredit], "trade_compliance", entries[trdapi.GuardCNXTradeCompliance])
}

// revise has admin draft the next revision of POL configuration kind/id with data, valid from the
// chain's start, and config.reviewer publish it on the evidence; it returns its reference.
func (s *s5sc) revise(kind, id string, data map[string]any) map[string]any {
	s.t.Helper()
	v := s.get(brcAdmin, "/api/brant/v1/pol/configs/"+kind+"/"+id)
	root, revision := brcNum(v["root_version"]), brcNum(v["active_revision"])+1
	s.cmd(brcAdmin, "pol", polapi.Contract, "config.save_draft", map[string]any{"kind": kind, "id": id, "expected_version": root,
		"valid_from": kernel.FormatTimestamp(s.from), "data": data})
	s.cmd(brcController, "pol", polapi.Contract, "config.publish", map[string]any{"kind": kind, "id": id, "expected_version": root + 1,
		"revision": revision, "evidence_ids": []string{"evidence"}, "reason": "核实合成测试配置（场景修订）"})
	return map[string]any{"kind": kind, "id": id, "revision": revision}
}

// rateConfig has admin draft an fx_rate (which POL requires to end) valid from the chain's start for
// 30 days, and config.reviewer publish it; it returns its reference.
func (s *s5sc) rateConfig(id string, data map[string]any) map[string]any {
	s.t.Helper()
	s.cmd(brcAdmin, "pol", polapi.Contract, "config.save_draft", map[string]any{"kind": polapi.KindFXRate, "id": id, "expected_version": 0,
		"valid_from": kernel.FormatTimestamp(s.from), "valid_until": kernel.FormatTimestamp(s.from.Add(30 * 24 * time.Hour)), "data": data})
	s.cmd(brcController, "pol", polapi.Contract, "config.publish", map[string]any{"kind": polapi.KindFXRate, "id": id, "expected_version": 1,
		"revision": 1, "evidence_ids": []string{"evidence"}, "reason": "核实合成汇率配置"})
	return map[string]any{"kind": polapi.KindFXRate, "id": id, "revision": 1}
}

// toPurchaseDraftOn is the S3 chain (toPurchaseDraft) with the payment terms of the scenario: the
// S3 foundation's payment.30-70 revised to them (revision 2) before the trading profile, the costing
// context and the quote bind it, and the order activated by activate.
func (s *s5sc) toPurchaseDraftOn(terms map[string]any, activate func()) {
	b := s.brc001
	b.foundation()
	b.payment = s.revise("payment_terms", "payment.30-70", terms)
	b.front()
	b.customerReady()
	b.sourcing()
	b.costing()
	b.quote()
	b.acceptance()
	activate()
	b.purchase()
	b.readBack()
	b.uniqueness()
}

// SC-08 / TD-AT08 (Context Map SC-08 "尾款未到账不放单"; ES-10, WF-07; FUL-INV-002, STL-INV-003,
// STL-INV-006; D-5, D-11, D-16, D-20): BRC-001 on the core design's terms — 30 % on order confirmation
// and 70 % before shipment (288.00 + 672.00 of 960.00), so no credit reservation. Treasury records
// the 288.00 and applies it to P1, then the customer's 672.00 of which the bank credited 670.00 with
// 2.00 unexplained: applying 672.00 is FUND_DIFFERENCE_UNEXPLAINED, 670.00 is applied to P2. The
// quality release is granted and the delivery verified. doc.1's evaluation of the shipment's
// commercial release is recorded blocked (COMMERCIAL_RELEASE_BLOCKED, the payment condition: P2 has
// 670.00 of 672.00, 2.00 short) and the merchandiser's confirmation is refused with the code; an
// ordinary note cannot release it. credit.manager.1 resolves the 2.00 as our bank charge — still
// blocked: a resolution counts only through an application —, treasury applies the 2.00, and the
// next evaluation grants the same release, binding the quality releases (120), the payment condition
// (960.00 of 960.00 at revision 1) and the screening and compliance conclusions it saw. The
// merchandiser confirms the shipment (the release consumed), doc.1 issues the documents, evaluates
// the documents release (granted: everything paid) and records the original B/L's release; the sales
// statement is 960.00 with 960.00 applied: open 0. Unit tests: C1
// TestSC08ReleaseWaitsForTheUnexplainedDifference (FUL on stand-in deliveries and an amended order),
// B2a TestFundRecordsKeepRemittedReceivedAndChargesApart.
func TestS5SC08ReleaseWaitsForTheUnexplainedDifferenceOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "SC-08", Title: "尾款未到账不放单 (TD-AT08): 670.00 of 672.00 before shipment → COMMERCIAL_RELEASE_BLOCKED → resolve 2.00 → granted",
		Design:    []string{"SC-08", "TD-AT08", "ES-10", "WF-07", "FUL-INV-002", "STL-INV-003", "STL-INV-006", "D-5", "D-11", "D-16", "D-20"},
		RestsOn:   []string{"C1", "C1b", "C1c", "B2a", "B6b", "D1"},
		UnitTests: []string{"TestSC08ReleaseWaitsForTheUnexplainedDifference", "TestFundRecordsKeepRemittedReceivedAndChargesApart (B2a)"},
		Deviations: []string{
			"The order is formed on the terms from the start (the S3 foundation's payment.30-70 revised to revision 2 before the trading profile, " +
				"costing context and quote bind it) instead of C1's amendment of an active order to them: the core design's BRC-001 terms (D-20).",
			"The 288.00 and 672.00 receipts are applied to the order's payment stages (target order_stage), as C1 did; the sales statement after " +
				"the shipment moves both applications to the receivable (D-4).",
		}})
	s.toPurchaseDraftOn(map[string]any{"name": "30% on order confirmation, 70% before shipment", "day_basis": polapi.DayBasisCalendar,
		"stages": []any{map[string]any{"stage_id": "P1", "percent": "30", "trigger": polapi.TriggerOrderConfirmed, "days": 0},
			map[string]any{"stage_id": "P2", "percent": "70", "trigger": polapi.TriggerBeforeShipment, "days": 0}}}, s.activateWithoutCredit)
	s.configurations()
	s.dispatch("after the activation (SC-08 terms)")
	s.phase = "SC-08: the order account"
	acc := s.account()
	s.want("order_account_stages", strings.Join(s5scStages(acc, "stages"), "; "), "P1 order_confirmed 28800; P2 before_shipment 67200")

	s.phase = "SC-08: receipts"
	s.fund("fund.p1", "288.00", "288.00", "MT103-SC08-P1", "合成银行收款通知：customer.1 汇入 288.00 USD（P1 订单确认 30%）。")
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, s.applyStage("app.p1", "fund.p1", "P1", "288.00"))
	s.fund("fund.p2", "670.00", "672.00", "MT103-SC08-P2", "合成银行收款通知：customer.1 汇出 672.00 USD（P2 出运前 70%），入账 670.00，差额 2.00 未说明。")
	f := s.get(brcTreasury, "/api/brant/v1/stl/fund_records/fund.p2")
	s.want("fund_p2", fmt.Sprint("received ", f["received_minor"], " remitted ", f["remitted_minor"], " available ", f["available_minor"]),
		"received 67000 remitted 67200 available 67000")
	s.refused(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, s.applyStage("app.p2.full", "fund.p2", "P2", "672.00"), http.StatusConflict,
		stlapi.CodeFundDifferenceUnexplained)
	s.figure("apply_672_00", stlapi.CodeFundDifferenceUnexplained)
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, s.applyStage("app.p2", "fund.p2", "P2", "670.00"))

	s.commitment()
	s.quality()
	s.milestones()
	s.delivery()
	s.shipment()

	s.phase = "SC-08: the release waits for the difference"
	r, e := s.evaluate(fulapi.ScopeShipment)
	releaseID := brcStr(r["object_id"])
	s.want("evaluation_1", fmt.Sprint(r["status"], " ", e["code"], " ", e["guard"]), "blocked COMMERCIAL_RELEASE_BLOCKED "+fulapi.GuardPaymentCondition)
	if !strings.Contains(brcStr(e["reason"]), "P2 (before_shipment) has 67000 of 67200 USD applied: 200 short") {
		t.Fatalf("blocked reason %v", e["reason"])
	}
	s.figure("evaluation_1_reason", brcStr(e["reason"]))
	s.refused(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": s.shipmentID,
		"expected_version": s.shipmentVersion, "evidence_ids": []string{"forwarder.receipt"}}, http.StatusConflict, fulapi.CodeCommercialReleaseBlocked)
	s.figure("confirm_while_blocked", fulapi.CodeCommercialReleaseBlocked)

	s.phase = "SC-08: STL resolves the 2.00"
	s.cmd(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandResolveDifference, map[string]any{"fund_record_id": "fund.p2",
		"expected_version": 1, "resolution": "bank_charge", "amount": "2.00", "evidence_ids": []string{"bank.fund.p2"},
		"reason": "中间行扣费 2.00 USD，经批准由我方承担（合成）"})
	r, e = s.evaluate(fulapi.ScopeShipment)
	s.want("evaluation_2_after_resolution", fmt.Sprint(r["status"], " ", e["code"]), "blocked COMMERCIAL_RELEASE_BLOCKED")
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, s.applyStage("app.p2.charge", "fund.p2", "P2", "2.00"))
	r, e = s.evaluate(fulapi.ScopeShipment)
	s.want("evaluation_3_after_application", fmt.Sprint(r["status"], " ", brcStr(r["object_id"]) == releaseID), "granted true")
	b := brcMap(e["bindings"])
	var released int64
	for _, q := range brcList(b["quality_releases"]) {
		released += kernel.MustDecimal(brcStr(brcDig(q, "released_quantity"))).Floor().Int64()
	}
	s.want("granted_binds_quality_released", released, "120")
	s.want("granted_binds_payment", fmt.Sprint(brcDig(b, "payment_condition", "status"), " ", brcDig(b, "payment_condition", "applied_minor"), "/",
		brcDig(b, "payment_condition", "required_minor"), " r", brcDig(b, "payment_condition", "order_revision")), "satisfied 96000/96000 r1")
	s.want("granted_binds_screening", fmt.Sprint(brcDig(b, "screenings", 0, "party_id"), " ", brcDig(b, "screenings", 0, "status")), "customer.1 clear")
	s.want("granted_binds_compliance", brcDig(b, "trade_compliance", "status"), "clear")
	v := s.release(releaseID)
	s.want("release_evaluations", fmt.Sprint(len(brcList(v["evaluations"]))), "3")

	s.confirm()
	v = s.release(releaseID)
	s.want("release_after_confirmation", v["status"], fulapi.ReleaseConsumed)
	s.documents()
	s.documentsRelease()
	s.figure("documents_release", "granted → consumed (original B/L)")

	s.phase = "SC-08: the sales statement"
	var sources []any
	for i := range brcLines {
		sources = append(sources, map[string]any{"shipment_id": s.shipmentID, "line_id": fmt.Sprintf("S%d", i+1)})
	}
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmSettlement, map[string]any{"statement_id": "stmt.sales.1",
		"kind": stlapi.StatementSales, "sources": sources})
	rec := s.get(brcTreasury, "/api/brant/v1/stl/receivables/stmt.sales.1")
	s.want("receivable", fmt.Sprint(rec["amount_minor"], " applied ", rec["applied_minor"], " open ", rec["open_minor"]), "96000 applied 96000 open 0")
	for _, id := range []string{"app.p1", "app.p2", "app.p2.charge"} {
		if a := s.get(brcTreasury, "/api/brant/v1/stl/applications/"+id); a["status"] != "moved" {
			t.Fatalf("%s after the statement %v", id, a)
		}
	}
	s.figure("stage_applications_after_statement", "app.p1, app.p2, app.p2.charge moved to stmt.sales.1")
	s.dispatch("at the end of SC-08")
	if !slices.ContainsFunc(s.delivered, func(e eventing.Envelope) bool { return string(e.Type) == "idp-brant.ful.commercial_release_granted.v1" }) {
		t.Fatal("no CommercialReleaseGranted delivered")
	}
}

// installmentsOf lists the stages (or installments) of an STL view as "stage trigger amount".
func s5scStages(v map[string]any, member string) []string {
	var out []string
	for _, i := range brcList(v[member]) {
		out = append(out, fmt.Sprintf("%v %v %v", brcDig(i, "stage_id"), brcDig(i, "trigger"), brcDig(i, "amount_minor")))
	}
	return out
}

// toS5 is the BRC-001 chain to the purchase draft (the S3 chain), the S5 configurations, the first
// relay run (OrderActivated: the order account opened, the reservation consumed) and §5 step 7 (P1
// received and applied): where most companions start.
func (s *s5sc) toS5() {
	s.toPurchaseDraft()
	s.configurations()
	s.dispatch("after the activation (S3 chain, §5 step 6)")
	s.activationConsumed()
	s.receiptP1()
}

// record has the merchandiser record delivery id of commitment.1 with lines (order line → quantity)
// at CNNGB on the forwarder's receipt.
func (s *s5sc) recordDelivery(id string, lines [][2]string) map[string]any {
	s.t.Helper()
	var claim []any
	for _, l := range lines {
		claim = append(claim, map[string]any{"order_line_id": l[0], "quantity": l[1]})
	}
	return s.cmd(brcMerchandiser, "pur", purapi.Contract, purapi.CommandRecordSupplierDelivery, map[string]any{"delivery_id": id,
		"expected_version": 0, "commitment_id": "commitment.1", "lines": claim, "handover_place": "CNNGB", "handed_over_at": now(),
		"evidence": []any{map[string]any{"source_id": "forwarder.receipt", "locator": id}}, "supplier_reference": "SA-DN-" + id})
}

// verifyDelivery has the purchaser verify delivery id's claim at row version 1 with lines (order line →
// verified quantity).
func (s *s5sc) verifyDelivery(id string, lines [][2]string) map[string]any {
	s.t.Helper()
	var verified []any
	for _, l := range lines {
		verified = append(verified, map[string]any{"order_line_id": l[0], "verified_quantity": l[1]})
	}
	return s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandVerifySupplierDelivery, map[string]any{"delivery_id": id,
		"expected_version": 1, "decision": "verify", "lines": verified, "evidence": []any{map[string]any{"source_id": "forwarder.receipt", "locator": id}}})
}

// s5scShipLine is one line of a shipment.prepare: shipment line, order line, quantity, delivery, delivery
// version (the quality release is rel.<order line>).
type s5scShipLine struct{ id, orderLine, qty, delivery string }

func (s *s5sc) shipmentLines(lines []s5scShipLine) []any {
	var out []any
	for _, l := range lines {
		out = append(out, map[string]any{"line_id": l.id, "order_line_id": l.orderLine, "quantity": l.qty,
			"supplier_delivery": map[string]any{"delivery_id": l.delivery, "line_id": l.orderLine, "version": 1}, "quality_release_id": "rel." + l.orderLine})
	}
	return out
}

// prepareShipment has the merchandiser prepare shipment id of order.1 on the supplier_direct route.
func (s *s5sc) prepareShipment(id string, lines []s5scShipLine) (int, map[string]any) {
	return s.post(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandPrepareShipment, map[string]any{"shipment_id": id, "order_id": "order.1",
		"route": fulapi.RouteSupplierDirect, "lines": s.shipmentLines(lines)})
}

// shipped is what FUL's fulfilment of order.1 shows shipped per order line, and in all.
func (s *s5sc) shipped() (map[string]string, int64) {
	f := s.get(brcMerchandiser, "/api/brant/v1/ful/orders/order.1/fulfilment")
	out := map[string]string{}
	var total int64
	for _, l := range brcList(f["lines"]) {
		out[brcStr(brcDig(l, "order_line_id"))] = brcStr(brcDig(l, "shipped"))
		total += kernel.MustDecimal(brcStr(brcDig(l, "shipped"))).Floor().Int64()
	}
	return out, total
}

// ES-12 / TD-AT14 (tactical TD-AT14 "直发：承诺 120 件，核实交付 100 件，另有未核实上报 20 件 → 出运 120 件
// 返回 SUPPLIER_DELIVERY_INSUFFICIENT；未核实上报不计入"; FUL-INV-003, PUR-INV-008, UL-INV-32; R59): BRC-001's
// commitment of 120 PCS, issued and confirmed, its quality released. The merchandiser records
// supplier.A's handover of 120 at CNNGB (delivery.1) and the purchaser verifies 100 of it (L4 28 of
// 48); the merchandiser records a further claim of 20 of L4 (delivery.2), left awaiting
// verification, and a claim of 5 of L1 (delivery.3) the purchaser rejects. Preparing a shipment of
// 120 from delivery.1 is SUPPLIER_DELIVERY_INSUFFICIENT on L4's quantity; one that ships L4's last
// 20 from the pending claim, and one from the rejected claim, are SUPPLIER_DELIVERY_UNVERIFIED: a
// claim never counts. The shipment of the 100 verified is prepared, released and confirmed (the
// confirmation records what SupplierDeliveryGuard v1 answered), a further piece is
// SUPPLIER_DELIVERY_INSUFFICIENT, and FUL's fulfilment shows 100 of 120 shipped. Unit tests: B3
// TestSupplierDeliveryServesShipmentsOnlyAsVerifiedTDAT14, B1a/B1b TestShipmentOnVerifiedDeliveriesTDAT14.
func TestS5SCTDAT14ShipmentOnlyOnVerifiedDeliveryOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "TD-AT14", Title: "直发交付 100 of 120 verified (ES-12): shipping 120 → SUPPLIER_DELIVERY_INSUFFICIENT; claims never count",
		Design:    []string{"ES-12", "TD-AT14", "FUL-INV-003", "PUR-INV-008", "UL-INV-32", "R59"},
		RestsOn:   []string{"B3", "B1a", "B1b", "C1", "D1"},
		UnitTests: []string{"TestSupplierDeliveryServesShipmentsOnlyAsVerifiedTDAT14 (B3)", "TestShipmentOnVerifiedDeliveriesTDAT14 (B1a/B1b)"},
		Deviations: []string{
			"BRC-001's four lines: the 20 short are on L4 (28 of 48 verified), so the refusal of 120 names /args/lines/3/quantity.",
		},
		Omitted: []string{"the concurrent confirmations of two shipments of 60 on 100 verified: a race, B1a/B1b's PG16 test (not an HTTP scenario)"}})
	s.toS5()
	s.commitment()
	s.quality()

	s.phase = "TD-AT14: deliveries"
	s.source(brcMerchandiser, "forwarder.receipt", "合成货代进仓单：provider.1 于 CNNGB 收到 supplier.A 交来 ST01，按交付编号分列。")
	all := [][2]string{{"L1", "24"}, {"L2", "36"}, {"L3", "12"}, {"L4", "48"}}
	s.recordDelivery("delivery.1", all)
	s.verifyDelivery("delivery.1", [][2]string{{"L1", "24"}, {"L2", "36"}, {"L3", "12"}, {"L4", "28"}})
	s.recordDelivery("delivery.2", [][2]string{{"L4", "20"}})
	s.recordDelivery("delivery.3", [][2]string{{"L1", "5"}})
	s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandVerifySupplierDelivery, map[string]any{"delivery_id": "delivery.3",
		"expected_version": 1, "decision": "reject", "reason": "进仓单无此 5 件（合成）"})
	d := s.get(brcPurchaser, "/api/brant/v1/pur/purchase_commitments/commitment.1/deliveries")
	s.fact("deliveries", d["lines"])
	var verified int64
	for _, l := range brcList(d["lines"]) {
		verified += kernel.MustDecimal(brcStr(brcDig(l, "verified_quantity"))).Floor().Int64()
	}
	s.want("verified_of_commitment", fmt.Sprintf("%d of 120", verified), "100 of 120")

	s.phase = "TD-AT14: shipments"
	full := []s5scShipLine{{"S1", "L1", "24", "delivery.1"}, {"S2", "L2", "36", "delivery.1"}, {"S3", "L3", "12", "delivery.1"}, {"S4", "L4", "48", "delivery.1"}}
	status, out := s.prepareShipment("shipment.120", full)
	s.want("prepare_120", fmt.Sprint(status, " ", out["code"], " ", brcDig(out, "field_errors", 0, "path")),
		"409 "+fulapi.CodeSupplierDeliveryInsufficient+" /args/lines/3/quantity")
	claims := append(slices.Clone(full[:3]), s5scShipLine{"S4", "L4", "28", "delivery.1"}, s5scShipLine{"S5", "L4", "20", "delivery.2"})
	status, out = s.prepareShipment("shipment.claims", claims)
	s.want("prepare_with_pending_claim", fmt.Sprint(status, " ", out["code"], " ", brcDig(out, "field_errors", 0, "path")),
		"409 "+fulapi.CodeSupplierDeliveryUnverified+" /args/lines/4/supplier_delivery")
	status, out = s.prepareShipment("shipment.rejected", []s5scShipLine{{"S1", "L1", "5", "delivery.3"}})
	s.want("prepare_on_rejected_claim", fmt.Sprint(status, " ", out["code"]), "409 "+fulapi.CodeSupplierDeliveryUnverified)
	s.read(brcMerchandiser, "/api/brant/v1/ful/shipments/shipment.120", http.StatusNotFound)

	s.shipmentID = "shipment.1"
	status, out = s.prepareShipment(s.shipmentID, claims[:4])
	if status != http.StatusOK {
		t.Fatalf("shipment of the 100 verified: %d %v", status, out)
	}
	s.shipmentVersion = brcNum(out["row_version"])
	r, _ := s.evaluate(fulapi.ScopeShipment)
	s.want("release_of_100", r["status"], fulapi.ReleaseGranted)
	r = s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": s.shipmentID,
		"expected_version": s.shipmentVersion, "evidence_ids": []string{"forwarder.receipt"}})
	s.want("confirm_100", r["status"], fulapi.StatusConfirmed)
	v := s.get(brcMerchandiser, "/api/brant/v1/ful/shipments/"+s.shipmentID)
	var guard string
	for _, g := range brcList(brcDig(v, "confirmation", "guards")) {
		if brcDig(g, "name") == "supplier_delivery:delivery.1/L4/v1" {
			guard = brcStr(brcDig(g, "value"))
		}
	}
	s.want("confirmation_delivery_guard_L4", guard, "verified/verified_version=1/verified=28")
	status, out = s.prepareShipment("shipment.more", []s5scShipLine{{"S1", "L4", "1", "delivery.1"}})
	s.want("prepare_one_more", fmt.Sprint(status, " ", out["code"]), "409 "+fulapi.CodeSupplierDeliveryInsufficient)
	lines, total := s.shipped()
	s.want("shipped", fmt.Sprintf("%d of 120 (L4 %s)", total, lines["L4"]), "100 of 120 (L4 28)")
}

// toShipped is toS5 and §5 steps 8–14: commitment, quality release, milestones, the verified
// delivery of 120, shipment.1 prepared, packed and booked, its release granted and its confirmation.
func (s *s5sc) toShipped() {
	s.toS5()
	s.commitment()
	s.quality()
	s.milestones()
	s.delivery()
	s.shipment()
	s.shipmentRelease()
	s.confirm()
}

// prepareDocument has doc.1 prepare a document of kind in shipment.1's set docs.shipment.1 at the
// set's row version expected; extra are the kind's members.
func (s *s5sc) prepareDocument(expected int64, kind string, extra map[string]any) map[string]any {
	s.t.Helper()
	s.documentSet = "docs.shipment.1"
	args := map[string]any{"document_set_id": s.documentSet, "shipment_id": s.shipmentID, "expected_version": expected, "document_kind": kind}
	for k, v := range extra {
		args[k] = v
	}
	return s.cmd(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandPrepareShipmentDocument, args)
}

// differences reads docs.shipment.1's reconciliation: its state and each difference as
// "path found→expected (against)".
func (s *s5sc) differences() (string, []string) {
	v := s.get(brcDocumentation, "/api/brant/v1/ful/document_sets/"+s.documentSet)
	var out []string
	for _, d := range brcList(brcDig(v, "reconciliation", "differences")) {
		out = append(out, fmt.Sprintf("%v %v→%v (%v)", brcDig(d, "path"), brcDig(d, "found"), brcDig(d, "expected"), brcDig(d, "against")))
	}
	return brcStr(brcDig(v, "reconciliation", "state")), out
}

// BT12 (baseline 9.3; tactical BT12 "商业发票、装箱单或提单草稿含版本或数量差异 → 逐项显示差异与修订；缺必需核对
// 不能签发 SHIPMENT_DOCUMENTS_UNRECONCILED"; FUL-INV-004; D-12): BRC-001 shipped (§5 steps 1–14). doc.1
// prepares the commercial invoice (CI-2026-00001, 960.00 USD); issuing without a packing list is
// SHIPMENT_DOCUMENTS_UNRECONCILED at /packing_list. doc.1 prepares the packing list as the factory
// listed it — L4 in 3 cartons of 12 (36 PCS) instead of 4: 108 PCS in 9 cartons — and the
// reconciliation shows each difference against its source (the shipment line, the CI's quantity, the
// actual packing). Issuing is refused at the first difference with the count; acknowledging one leaves
// the next; acknowledging a difference that does not exist is INVALID_KNOWN_VALUE. The PL's next
// version prints the actual packing (same number, version 2): reconciled with the B/L record, and
// the set issues CI v1, PL v2 and the B/L; PL v1 stays as the superseded draft's trace. Unit test:
// C2 TestShipmentDocumentsReconcileBeforeIssueBT12.
func TestS5SCBT12PackingListMismatchBlocksTheIssueOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "BT12", Title: "装箱单与发票数量不符 (BT12): differences item by item, SHIPMENT_DOCUMENTS_UNRECONCILED until the PL is corrected",
		Design:    []string{"BT12", "FUL-INV-004", "D-12", "INT-05"},
		RestsOn:   []string{"C2", "B1a", "D1"},
		UnitTests: []string{"TestShipmentDocumentsReconcileBeforeIssueBT12 (C2)"},
		Deviations: []string{
			"The mismatch is the factory's packing list stated in the PL's prepare (packages), the only way a PL/CI mismatch can arise (C2 decision 4): " +
				"L4 in 3 cartons instead of 4 (108 of 120 PCS).",
		}})
	s.toShipped()

	s.phase = "BT12: commercial invoice without packing list"
	ci := s.prepareDocument(0, fulapi.DocumentCommercialInvoice, map[string]any{"templates": []any{s.ciEn}, "sequence_ref": s.ciSeq,
		"seller_ref": s.company, "particulars": documentParticulars()})
	s.refused(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandIssueShipmentDocuments, map[string]any{"document_set_id": s.documentSet,
		"expected_version": ci["row_version"]}, http.StatusConflict, fulapi.CodeShipmentDocumentsUnreconciled)
	s.want("issue_without_pl", brcDig(s.record.Steps[len(s.record.Steps)-1].Facts, "field_errors", 0, "path"), "/packing_list")

	s.phase = "BT12: the factory's packing list"
	factory := []any{}
	from := 1
	for i, n := range []int{2, 3, 1, 3} {
		factory = append(factory, map[string]any{"cartons": n, "carton_from": from, "contents": []any{map[string]any{"line_id": fmt.Sprintf("S%d", i+1),
			"quantity_per_carton": "12"}}, "gross_weight_kg": "6.5", "net_weight_kg": "6", "volume_cbm": "0.05"})
		from += n
	}
	pl := s.prepareDocument(brcNum(ci["row_version"]), fulapi.DocumentPackingList, map[string]any{"templates": []any{s.plEn}, "sequence_ref": s.plSeq,
		"seller_ref": s.company, "particulars": documentParticulars(), "packages": factory})
	state, diffs := s.differences()
	s.want("reconciliation_with_factory_pl", state+": "+strings.Join(diffs, "; "), "unreconciled: "+
		"/packing_list/lines/S4/quantity 36→48 (shipment_line); /packing_list/totals/cartons 9→10 (actual_packing); "+
		"/packing_list/totals/gross_weight_kg 58.5→65 (actual_packing); /packing_list/totals/net_weight_kg 54→60 (actual_packing); "+
		"/packing_list/totals/quantity/PCS 108→120 (commercial_invoice); /packing_list/totals/volume_cbm 0.45→0.5 (actual_packing)")
	issue := func(expected any, acks ...[2]string) (int, map[string]any) {
		var a []any
		for _, x := range acks {
			a = append(a, map[string]any{"path": x[0], "reason": x[1]})
		}
		args := map[string]any{"document_set_id": s.documentSet, "expected_version": expected}
		if len(a) > 0 {
			args["acknowledgements"] = a
		}
		return s.post(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandIssueShipmentDocuments, args)
	}
	status, out := issue(pl["row_version"])
	s.want("issue_with_differences", fmt.Sprint(status, " ", out["code"], " ", brcDig(out, "field_errors", 0, "path"), " ",
		strings.Contains(brcStr(brcDig(out, "field_errors", 0, "reason")), "6 difference(s)")),
		"409 "+fulapi.CodeShipmentDocumentsUnreconciled+" /packing_list/lines/S4/quantity true")
	status, out = issue(pl["row_version"], [2]string{"/packing_list/lines/S4/quantity", "工厂装箱单待更正"})
	s.want("issue_with_one_acknowledged", fmt.Sprint(status, " ", out["code"], " ", brcDig(out, "field_errors", 0, "path")),
		"409 "+fulapi.CodeShipmentDocumentsUnreconciled+" /packing_list/totals/cartons")
	status, out = issue(pl["row_version"], [2]string{"/commercial_invoice/lines/S1/quantity", "无此差异"})
	s.want("acknowledging_no_difference", fmt.Sprint(status, " ", out["code"]), "422 "+kernel.CodeInvalidKnownValue)

	s.phase = "BT12: the corrected packing list"
	pl2 := s.prepareDocument(brcNum(pl["row_version"]), fulapi.DocumentPackingList, map[string]any{"templates": []any{s.plEn}, "sequence_ref": s.plSeq,
		"seller_ref": s.company, "particulars": documentParticulars()})
	s.fileOf(brcDocumentation, "bl.scan", "bl-SYNSTAR026E0001.txt", "合成提单扫描件：SYNTHETIC STAR 026E，CNNGB→DEHAM，10 CTNS，65 KGS，0.5 CBM。")
	bl := s.prepareDocument(brcNum(pl2["row_version"]), fulapi.DocumentBillOfLading, map[string]any{"bill_of_lading": map[string]any{
		"number": "SYNSTAR026E0001", "file_id": "bl.scan", "vessel": "SYNTHETIC STAR", "voyage": "026E", "port_of_loading": "CNNGB",
		"port_of_discharge": "DEHAM", "shipped_on_board": today(), "consignee": map[string]any{"name": "customer.1", "address": "合成买方地址\n汉堡"},
		"marks": "CUSTOMER.1\nHAMBURG\nC/NO. 1-10", "packages": 10, "gross_weight_kg": "65", "volume_cbm": "0.5", "container_numbers": []string{"SYNU1234565"}}})
	state, diffs = s.differences()
	s.want("reconciliation_after_correction", fmt.Sprint(state, " ", len(diffs)), "reconciled 0")
	status, out = issue(brcNum(bl["row_version"]))
	if status != http.StatusOK {
		t.Fatalf("issue %d %v", status, out)
	}
	set := s.get(brcDocumentation, "/api/brant/v1/ful/document_sets/"+s.documentSet)
	var docs []string
	numbers := map[string]bool{}
	for _, d := range brcList(set["documents"]) {
		docs = append(docs, fmt.Sprintf("%v v%v %v", brcDig(d, "document_kind"), brcDig(d, "version"), brcDig(d, "status")))
		if brcDig(d, "document_kind") == fulapi.DocumentPackingList {
			numbers[brcStr(brcDig(d, "number"))] = true
		}
	}
	slices.Sort(docs)
	s.want("document_set", strings.Join(docs, "; "), "bill_of_lading v1 issued; commercial_invoice v1 issued; packing_list v1 superseded; packing_list v2 issued")
	s.want("pl_versions_share_one_number", fmt.Sprint(len(numbers), " ", numbers["PL-2026-00001"]), "1 true")
}

// toReceivable is toShipped and §5 step 19: the sales statement of shipment.1's four lines (960.00,
// P1's 288.00 moved onto it, open 672.00).
func (s *s5sc) toReceivable() {
	s.toShipped()
	s.salesStatement()
}

// salesSources are the four lines of shipment.1 as a sales statement's sources.
func (s *s5sc) salesSources() []any {
	var sources []any
	for i := range brcLines {
		sources = append(sources, map[string]any{"shipment_id": s.shipmentID, "line_id": fmt.Sprintf("S%d", i+1)})
	}
	return sources
}

// receivable reads statement id's receivable as "amount applied open".
func (s *s5sc) receivable(id string) (string, map[string]any) {
	v := s.get(brcTreasury, "/api/brant/v1/stl/receivables/"+id)
	return fmt.Sprint(v["amount_minor"], " applied ", v["applied_minor"], " open ", v["open_minor"]), v
}

// Settlement reversal after unapply (S5-18; STL-INV-009, SM-STL-SETTLEMENT; baseline 4.2 "冲销按批准规则恢复
// 可结算来源"; C8 decision 7 "unapply first"): BRC-001's sales statement stmt.sales.1 of 960.00 (P1's
// 288.00 moved onto it). credit.manager.1's settlement.reverse is SETTLEMENT_HAS_APPLICATIONS while
// the moved prepayment stands; treasury unapplies it (the unapplication is a new record: the fund's
// 288.00 is available again, P1 unpaid); the reversal by credit.manager.1 — not the statement's
// confirmer — commits: the statement reversed, its receivable open 0, the four shipment lines
// settleable again; the exposure stays 672.00 (the consumed reservation's portion). Treasury applies
// the 288.00 to P1 again and confirms stmt.sales.2 from the same four lines — 960.00, the new P1
// application moved, open 672.00 —, and a third statement of them is SETTLEMENT_SOURCE_DUPLICATE:
// the source settles once. SettlementReversed is published and delivered. Unit tests: C8
// TestReversalRestoresTheSourceOnce, TestCreditNoteAndReversalOnRealRecordsBRC001.
func TestS5SCSettlementReversalAfterUnapplyOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "STL-REVERSAL", Title: "结算单冲销（先撤销核销）: SETTLEMENT_HAS_APPLICATIONS → unapply → reversed → resettled once",
		Design:    []string{"STL-INV-009", "SM-STL-SETTLEMENT", "STL-INV-001", "baseline 4.2", "D-4"},
		RestsOn:   []string{"C8", "B2b", "B2a", "D1"},
		UnitTests: []string{"TestReversalRestoresTheSourceOnce (C8)", "TestCreditNoteAndReversalOnRealRecordsBRC001 (C8)"},
	})
	s.toReceivable()

	s.phase = "STL reversal: refused while an application stands"
	reverse := map[string]any{"statement_id": "stmt.sales.1", "expected_version": 1, "reason": "结算单开错（合成），冲销后按原出运重新结算",
		"evidence_ids": []string{"bank.p1"}}
	s.refused(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandReverseSettlement, reverse, http.StatusConflict, stlapi.CodeSettlementHasApplications)
	s.figure("reverse_with_moved_prepayment", stlapi.CodeSettlementHasApplications)
	_, v := s.receivable("stmt.sales.1")
	var replacement string
	for _, a := range brcList(v["applications"]) {
		if brcDig(a, "moved_from") == "app.p1" && brcDig(a, "status") == "applied" {
			replacement = brcStr(brcDig(a, "application_id"))
		}
	}
	if replacement == "" {
		t.Fatalf("no replacement of app.p1 on the receivable %v", v["applications"])
	}
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandUnapply, map[string]any{"application_id": replacement, "expected_version": 1,
		"reason": "冲销结算单前撤销核销"})
	f := s.get(brcTreasury, "/api/brant/v1/stl/fund_records/fund.p1")
	s.want("fund_p1_after_unapply", fmt.Sprint("applied ", f["applied_minor"], " available ", f["available_minor"]), "applied 0 available 28800")

	s.phase = "STL reversal"
	r := s.cmd(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandReverseSettlement, reverse)
	s.want("reversal", fmt.Sprint(r["status"], " v", r["row_version"]), "reversed v2")
	st := s.get(brcTreasury, "/api/brant/v1/stl/settlement_statements/stmt.sales.1")
	s.want("statement_after_reversal", fmt.Sprint(st["status"], " by ", brcDig(st, "reversal", "reversed_by", "id")), "reversed by credit.manager.1")
	got, _ := s.receivable("stmt.sales.1")
	s.want("receivable_after_reversal", got, "96000 applied 0 open 0")
	acc := s.account()
	s.want("p1_after_reversal", brcDig(acc, "stages", 0, "applied_minor"), "0")
	s.want("exposure_after_reversal", s.exposure(brcCreditOfficer), "67200")

	s.phase = "STL resettlement"
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, s.applyStage("app.p1.again", "fund.p1", "P1", "288.00"))
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmSettlement, map[string]any{"statement_id": "stmt.sales.2",
		"kind": stlapi.StatementSales, "sources": s.salesSources()})
	got, _ = s.receivable("stmt.sales.2")
	s.want("resettled_receivable", got, "96000 applied 28800 open 67200")
	if a := s.get(brcTreasury, "/api/brant/v1/stl/applications/app.p1.again"); a["status"] != "moved" {
		t.Fatalf("app.p1.again %v", a)
	}
	s.refused(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmSettlement, map[string]any{"statement_id": "stmt.sales.3",
		"kind": stlapi.StatementSales, "sources": s.salesSources()}, http.StatusConflict, stlapi.CodeSettlementSourceDuplicate)
	s.figure("third_statement_of_the_lines", stlapi.CodeSettlementSourceDuplicate)
	pos := s.get(brcTreasury, "/api/brant/v1/stl/orders/order.1/settlement")
	s.want("order_settlement", fmt.Sprint("settled ", pos["settled_minor"], " open ", pos["open_minor"], " net sales ", pos["net_sales_minor"]),
		"settled 96000 open 67200 net sales 96000")
	s.dispatch("after the reversal and the resettlement")
	n := 0
	for _, e := range s.delivered {
		if e.Type == "idp-brant.stl.settlement_reversed.v1" {
			n++
		}
	}
	s.want("settlement_reversed_events", n, "1")
}

// Cross-currency write-off (S5-18; STL-INV-007, MD-10, R33; baseline 4.3 "跨币种核销属于后续可选能力，未经过
// 专项规则评审和验证不得启用" — enabled by C8 with a settlement-purpose rate only): BRC-001's receivable
// stmt.sales.1, P2 open 672.00 USD. POL publishes CNY, the company's CNY account and the settlement
// rate CNY→USD 0.145454545455 (1/6.875, baseline 4.6's rate) plus a credit-purpose rate of the same
// direction. Treasury records customer.1's CNY 4,620.00 receipt. Applying it to P2 without a rate is
// FX_SCOPE_MISMATCH at /args/fx_rate, and with the credit-purpose rate too; at the settlement rate
// CNY 4,620.00 pays USD 672.00 (HALF_UP once at USD's minor units): the receivable is 0, the
// exposure 0, the fund's CNY 4,620.00 applied, and the application records the source amount, the
// rate's revision and value and the analysis comparison in the order's analysis currency (USD).
// Unit tests: C8 TestCrossCurrencyApplicationAtTheSettlementRateS_A, TestACNYReceiptPaysAUSDReceivableAtTheSettlementRate.
func TestS5SCCrossCurrencyWriteOffAtTheSettlementRateOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "STL-CROSS-CURRENCY", Title: "跨币种核销: a USD receivable written off by a CNY receipt at the settlement rate",
		Design:    []string{"STL-INV-007", "MD-10", "R33", "baseline 4.3", "baseline 4.6"},
		RestsOn:   []string{"C8", "B2a", "B2b", "D1"},
		UnitTests: []string{"TestCrossCurrencyApplicationAtTheSettlementRateS_A (C8)", "TestACNYReceiptPaysAUSDReceivableAtTheSettlementRate (C8)"},
		Deviations: []string{
			"BRC-001's own figures: P2's 672.00 USD paid by CNY 4,620.00 at 0.145454545455 (S-A's 6.875); BRC-001's analysis currency is USD, " +
				"so the analysis comparison is in USD (S-A's CNY −50.00 per receipt is C8's ledger test).",
		}})
	s.toReceivable()

	s.phase = "POL: CNY, the CNY account and the rates"
	cny := s.config("currency", "CNY", map[string]any{"code": "CNY", "name": "人民币", "minor_units": 2, "rounding": polapi.RoundingHalfUp})
	bank := s.config(polapi.KindBankAccount, "bank.cny", map[string]any{"owner_kind": polapi.OwnerCompany, "owner_id": "company.A",
		"bank_name": "synthetic bank", "account_number": "6222000033334444", "currency": "CNY", "purpose": polapi.AccountBoth})
	rate := func(id, purpose string) map[string]any {
		return s.rateConfig(id, map[string]any{"from_currency": "CNY", "to_currency": "USD", "rate": "0.145454545455",
			"purpose": purpose, "source_name": "SYNTHETIC SETTLEMENT RATE", "observed_at": kernel.FormatTimestamp(s.from)})
	}
	settle, credit := rate("fx.cny.usd", polapi.PurposeSettlement), rate("fx.cny.usd.credit", polapi.PurposeCredit)

	s.phase = "STL: the CNY receipt"
	s.source(brcTreasury, "bank.cny.p2", "合成银行收款通知：customer.1 以人民币支付 P2，入账 CNY 4,620.00，MT103-BRC-CNY。")
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, map[string]any{"fund_record_id": "fund.cny.p2", "kind": stlapi.FundReceipt,
		"counterparty_id": "customer.1", "currency": cny, "bank_account": bank, "received_amount": "4620.00", "value_date": now(),
		"bank_reference": "MT103-BRC-CNY", "evidence_ids": []string{"bank.cny.p2"}})
	apply := func(id string, fx map[string]any) map[string]any {
		a := map[string]any{"application_id": id, "fund_record_id": "fund.cny.p2", "target": map[string]any{"kind": "receivable",
			"statement_id": "stmt.sales.1", "stage_id": "P2"}, "amount": "4620.00"}
		if fx != nil {
			a["fx_rate"] = fx
		}
		return a
	}
	s.refused(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, apply("app.cny.norate", nil), http.StatusUnprocessableEntity, stlapi.CodeFXScopeMismatch)
	s.want("apply_without_rate", fmt.Sprint(stlapi.CodeFXScopeMismatch, " ", brcDig(s.record.Steps[len(s.record.Steps)-1].Facts, "field_errors", 0, "path")),
		stlapi.CodeFXScopeMismatch+" /args/fx_rate")
	s.refused(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, apply("app.cny.credit", credit), http.StatusUnprocessableEntity, stlapi.CodeFXScopeMismatch)
	s.figure("apply_at_credit_purpose_rate", stlapi.CodeFXScopeMismatch)
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, apply("app.cny.p2", settle))
	a := s.get(brcTreasury, "/api/brant/v1/stl/applications/app.cny.p2")
	conv := brcMap(a["conversion"])
	s.want("application", fmt.Sprint(a["amount_minor"], " USD from ", conv["source_currency"], " ", conv["source_amount_minor"], " at ", conv["rate"]),
		"67200 USD from CNY 462000 at 0.145454545455")
	s.want("analysis", fmt.Sprint(conv["analysis_currency"], " rate ", conv["analysis_rate"], " difference ", conv["fx_difference_minor"]), "USD rate 1 difference 0")
	got, _ := s.receivable("stmt.sales.1")
	s.want("receivable", got, "96000 applied 96000 open 0")
	f := s.get(brcTreasury, "/api/brant/v1/stl/fund_records/fund.cny.p2")
	s.want("cny_fund", fmt.Sprint(f["currency"], " applied ", f["applied_minor"], " available ", f["available_minor"]), "CNY applied 462000 available 0")
	s.want("exposure", s.exposure(brcCreditOfficer), "0")
	s.dispatch("after the cross-currency write-off")
}

// result reads order.1's operating result as treasury.
func (s *s5sc) result() map[string]any {
	return s.get(brcTreasury, "/api/brant/v1/stl/orders/order.1/result")
}

// unmet is the field paths of an ORDER_RESULT_NOT_SETTLED refusal (the last step's), joined.
func (s *s5sc) unmet() string {
	var paths []string
	for _, f := range brcList(s.record.Steps[len(s.record.Steps)-1].Facts["field_errors"]) {
		paths = append(paths, brcStr(brcDig(f, "path")))
	}
	return strings.Join(paths, " ")
}

// recordCharge has credit.officer record charge id on order.1 in USD (same currency).
func (s *s5sc) recordCharge(id, status, amount, occurred string, source map[string]any, evidence []string, basis string) map[string]any {
	s.t.Helper()
	args := map[string]any{"charge_id": id, "expected_version": 0, "status": status, "source": source,
		"beneficiaries": []any{map[string]any{"kind": "order", "order_id": "order.1"}}, "fee_category": s.freight, "currency": s.usd,
		"conversion": map[string]any{"kind": "same_currency"}, "amount": amount, "occurred_on": occurred, "analysis_on": occurred, "basis": basis}
	if len(evidence) > 0 {
		args["evidence_ids"] = evidence
	}
	return s.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandRecordCharge, args)
}

// allocateCharge has credit.officer allocate charge id at its version wholly to order.1.
func (s *s5sc) allocateCharge(id string, version int64) map[string]any {
	return map[string]any{"charge_id": id, "expected_version": version, "basis": "amount", "data_basis": "单一受益订单 order.1，全额",
		"lines": []any{map[string]any{"line_id": "A1", "target": map[string]any{"kind": "order", "order_id": "order.1"}, "weight": "1"}}}
}

// BT18 / BT24 (baseline 9.3/9.4; tactical BT18 "发货完成但有未结费用或未决事项 → 显示各维度状态与待办；不因发货完成宣告结清
// （ORDER_RESULT_NOT_SETTLED）", BT24 "订单关闭后补入次月运费，并尝试改写上期快照 → 记录发生、录入与分析日期及重新打开；历史快照不被覆盖";
// STL-INV-011, SM-STL-ORDER-RESULT; baseline 4.5): BRC-001 shipped and its sales statement confirmed
// (§5 steps 1–14, 19). credit.officer records provider.1's freight of 60.00 USD as estimated;
// treasury reviews the order result and its close is ORDER_RESULT_NOT_SETTLED, one field per unmet
// dimension (the order not ended, the receivable's 672.00 open, the purchase not settled, the freight
// estimated): the shipment's completion does not settle it. The chain goes on (documents and their
// release, collection, payable, order.close, the reservation released); the close is refused again on the freight alone. Treasury
// confirms the freight on the forwarder's bill, credit.officer allocates it, and the close settles
// the result: snapshot 1, 960.00 − 600.00 − 60.00 = 300.00 (31.25 %). Next month's freight of 20.00
// USD (BT24) is recorded pending with its bill, occurred and analysed on a date of next month: the
// result stays settled with snapshot 1 unchanged while the live figures show the charge; confirming
// or allocating it is INVALID_STATE until treasury reopens the result with its reason; then confirm,
// allocate, a close without a new review INVALID_STATE, review and close: snapshot 2 is 280.00 and
// snapshot 1 is kept with the same SHA-256, marked reopened. Unit tests: C9
// TestBaseline46SAOrderResult (BT18 part), TestBT24LateFreightReopensTheResult,
// TestOrderResultOnRealRecordsBRC001OverHTTP.
func TestS5SCBT18BT24OrderResultNotSettledAndReopenedOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "BT18-BT24", Title: "经营结果：有未结事项不结清 (BT18)；次月运费重开，历史快照不覆盖 (BT24)",
		Design:    []string{"BT18", "BT24", "STL-INV-010", "STL-INV-011", "SM-STL-ORDER-RESULT", "baseline 4.5"},
		RestsOn:   []string{"C9", "B2b", "D1"},
		UnitTests: []string{"TestBaseline46SAOrderResult (C9, BT18 part)", "TestBT24LateFreightReopensTheResult (C9)", "TestOrderResultOnRealRecordsBRC001OverHTTP (C9)"},
		Deviations: []string{
			"BRC-001's figures (USD): the late freight is 20.00 USD (C9's ledger test uses S-A's CNY 100.00).",
		},
		Omitted: []string{"§5 steps 17, 18 and 22–24 (export lot, declaration, invoice, tax case, bundle): the order result's close conditions do not include them (D1 runs them)"}})
	s.toReceivable()

	s.phase = "BT18: shipped, not settled"
	r := s.recordCharge("freight.brc", "estimated", "60.00", today(), map[string]any{"kind": "logistics_offer", "party_id": "provider.1",
		"reference": "log.1"}, nil, "provider.1 海运报价 log.1：F1 60.00 USD")
	s.want("freight_recorded", r["status"], "estimated")
	r = s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandReviewOrderResult, map[string]any{"order_id": "order.1", "expected_version": 0,
		"note": "出运完成后复核：应收、采购与运费尚未结清（合成）"})
	s.refused(brcTreasury, "stl", stlapi.Contract, stlapi.CommandCloseOrderResult, map[string]any{"order_id": "order.1",
		"expected_version": r["row_version"], "reason": "出运已完成"}, http.StatusConflict, stlapi.CodeOrderResultNotSettled)
	s.want("close_after_shipment", s.unmet(), "/conditions/fulfilment /conditions/receivables /conditions/purchase_costs /conditions/direct_charges")
	v := s.result()
	var conditions []string
	for _, c := range brcList(v["conditions"]) {
		conditions = append(conditions, fmt.Sprint(brcDig(c, "dimension"), "=", brcDig(c, "status")))
	}
	s.want("conditions_after_shipment", fmt.Sprint(v["status"], " ", strings.Join(conditions, " ")), "reviewed "+
		"fulfilment=unmet receivables=unmet payables=met purchase_costs=unmet direct_charges=unmet claims=not_enabled")

	s.documents()
	s.documentsRelease()
	s.collection()
	s.payable()
	s.closeOrder()
	s.dispatch("after order.close")
	s.reservationReleased()
	s.phase = "BT18: the freight still estimated"
	s.refused(brcTreasury, "stl", stlapi.Contract, stlapi.CommandCloseOrderResult, map[string]any{"order_id": "order.1",
		"expected_version": r["row_version"], "reason": "订单已关闭"}, http.StatusConflict, stlapi.CodeOrderResultNotSettled)
	s.want("close_with_estimated_freight", s.unmet(), "/conditions/direct_charges")
	s.source(brcTreasury, "freight.bill", "合成货代账单：provider.1 海运费 60.00 USD，order.1 / shipment.1。")
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmCharge, map[string]any{"charge_id": "freight.brc", "expected_version": 1,
		"evidence_ids": []string{"freight.bill"}})
	s.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandAllocateCharge, s.allocateCharge("freight.brc", 2))
	c := s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandCloseOrderResult, map[string]any{"order_id": "order.1",
		"expected_version": r["row_version"], "reason": "BRC-001 订单经营结果结清"})
	s.want("close", fmt.Sprint(c["status"], " v", c["row_version"]), "settled v2")
	v = s.result()
	first := brcMap(brcDig(v, "snapshots", 0))
	s.want("snapshot_1", fmt.Sprint(brcDig(first, "figures", "confirmed", "result_minor"), " ", brcDig(first, "figures", "confirmed", "margin")), "30000 31.25")
	sha := brcStr(first["content_sha256"])

	s.phase = "BT24: next month's freight"
	month := time.Now().UTC().AddDate(0, 1, 0)
	late := time.Date(month.Year(), month.Month(), 5, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	s.source(brcTreasury, "freight.bill.late", "合成货代补充账单：provider.1 次月港杂费 20.00 USD，order.1 / shipment.1，BILL-NEXT。")
	r = s.recordCharge("freight.late", "pending", "20.00", late, map[string]any{"kind": "supplier_bill", "party_id": "provider.1",
		"reference": "BILL-NEXT"}, []string{"freight.bill.late"}, "次月补入：provider.1 账单 BILL-NEXT 20.00 USD")
	s.want("late_freight", fmt.Sprint(r["status"], " occurred ", late), "pending occurred "+late)
	v = s.result()
	s.want("result_with_late_freight", fmt.Sprint(v["status"], " snapshot_1 ", brcDig(v, "snapshots", 0, "content_sha256") == sha, " executing ",
		brcDig(v, "live", "executing", "result_minor")), "settled snapshot_1 true executing 28000")
	s.refused(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmCharge, map[string]any{"charge_id": "freight.late", "expected_version": 1,
		"evidence_ids": []string{"freight.bill.late"}}, http.StatusUnprocessableEntity, kernel.CodeInvalidState)
	s.figure("confirm_while_settled", kernel.CodeInvalidState)
	s.refused(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandAllocateCharge, s.allocateCharge("freight.late", 1),
		http.StatusUnprocessableEntity, kernel.CodeInvalidState)
	s.figure("allocate_while_settled", kernel.CodeInvalidState)
	o := s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandReopenOrderResult, map[string]any{"order_id": "order.1", "expected_version": 2,
		"reason": "次月运费补录：BILL-NEXT", "evidence_ids": []string{"freight.bill.late"}})
	s.want("reopen", fmt.Sprint(o["status"], " v", o["row_version"]), "open v3")
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmCharge, map[string]any{"charge_id": "freight.late", "expected_version": 1,
		"evidence_ids": []string{"freight.bill.late"}})
	s.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandAllocateCharge, s.allocateCharge("freight.late", 2))
	s.refused(brcTreasury, "stl", stlapi.Contract, stlapi.CommandCloseOrderResult, map[string]any{"order_id": "order.1", "expected_version": 3,
		"reason": "补录后结清"}, http.StatusUnprocessableEntity, kernel.CodeInvalidState)
	s.figure("close_without_a_new_review", kernel.CodeInvalidState)
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandReviewOrderResult, map[string]any{"order_id": "order.1", "expected_version": 3,
		"note": "次月运费已确认并分配"})
	c = s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandCloseOrderResult, map[string]any{"order_id": "order.1", "expected_version": 4,
		"reason": "补录后结清"})
	s.want("second_close", fmt.Sprint(c["status"], " v", c["row_version"]), "settled v5")
	v = s.result()
	snaps := brcList(v["snapshots"])
	s.want("snapshots", fmt.Sprint(len(snaps), " #1 ", brcDig(snaps, 0, "figures", "confirmed", "result_minor"), " reopened ", brcDig(snaps, 0, "reopened"),
		" sha kept ", brcDig(snaps, 0, "content_sha256") == sha, "; #2 ", brcDig(snaps, 1, "figures", "confirmed", "result_minor"),
		" charges ", brcDig(snaps, 1, "figures", "confirmed", "direct_charges_minor")), "2 #1 30000 reopened true sha kept true; #2 28000 charges 8000")
	s.want("reopenings", fmt.Sprint(len(brcList(v["reopenings"])), " ", brcDig(v, "reopenings", 0, "reason")), "1 次月运费补录：BILL-NEXT")
	ch := s.get(brcTreasury, "/api/brant/v1/stl/charges/freight.late")
	s.want("late_charge_dates", fmt.Sprint(ch["occurred_on"], " / ", ch["analysis_on"], " recorded ", brcStr(ch["recorded_at"])[:10] == today()),
		late+" / "+late+" recorded true")
	s.dispatch("at the end of BT18/BT24")
	count := map[string]int{}
	for _, e := range s.delivered {
		count[string(e.Type)]++
	}
	s.want("result_events", fmt.Sprint(count[stlapi.EventOrderResultClosed], " closed, ", count[stlapi.EventOrderResultReopened], " reopened"),
		"2 closed, 1 reopened")
}

// commitmentView reads commitment.1 as the purchaser and its lines as "line purchase/verified".
func (s *s5sc) commitmentView() (map[string]any, string) {
	v := s.get(brcPurchaser, "/api/brant/v1/pur/purchase_commitments/commitment.1")
	var lines []string
	for _, l := range brcList(v["lines"]) {
		lines = append(lines, fmt.Sprintf("%v %v/%v", brcDig(l, "order_line_id"), brcDig(l, "purchase_quantity"), brcDig(l, "verified_quantity")))
	}
	return v, strings.Join(lines, " ")
}

// allocationsView reads order.1's demand allocations as "line allocated/demand".
func (s *s5sc) allocationsView() string {
	a := s.get(brcPurchaser, "/api/brant/v1/pur/orders/order.1/allocations")
	var out []string
	for _, l := range brcList(a["allocations"]) {
		out = append(out, fmt.Sprintf("%v %v/%v", brcDig(l, "order_line_id"), brcDig(l, "allocated"), brcDig(l, "demand")))
	}
	return strings.Join(out, " ")
}

// Commitment cancellation (S5-19; A47 #7, PUR-INV-008, PUR-INV-011's cancel half; SM-PUR-COMMITMENT; D-1):
// BRC-001's commitment.1 of 120 PCS, issued as PO-2026-00001 and confirmed by supplier.A, its quality
// released. supplier.A
// hands over L1 and L2 (60 PCS), which the purchaser verifies, and then declares in writing it cannot
// make L3 and L4. The purchaser cancels the commitment on the supplier's evidence: a cancellation
// without evidence is SCHEMA_INVALID; then cancelled at revision 2 (kind cancel), each line keeping
// exactly its verified quantity (L1 24, L2 36, L3 0, L4 0); the undelivered 60 of the order's
// allocation are released (L3 0/12, L4 0/48) and PurchaseCommitmentCancelled names them; the verified
// delivery stays a shipment source (shipment.1 of L1 and L2 is prepared on it); the draft stays
// committed (void and a new request PURCHASE_DRAFT_COMMITTED); a second cancellation is
// INVALID_STATE. The purchaser prepares purchase.2 from supplier.B's offer for the 60 released, with
// 40 PCS overbuy on L4 to meet the ST01 group's MOQ of 100 (60 alone is MOQ_NOT_MET): 100 × 5.30 = 530.00.
// Unit test: C11 TestCommitmentCancelReleasesTheUndeliveredAllocationAndKeepsTheVerified.
func TestS5SCCommitmentCancellationOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "PUR-CANCEL", Title: "采购承诺取消: the undelivered allocation released, the verified kept",
		Design:    []string{"A47 #7", "PUR-INV-008", "PUR-INV-011", "SM-PUR-COMMITMENT", "D-1", "D-14"},
		RestsOn:   []string{"C11", "A4", "B3", "D1"},
		UnitTests: []string{"TestCommitmentCancelReleasesTheUndeliveredAllocationAndKeepsTheVerified (C11)"},
		Deviations: []string{
			"BRC-001's lines: 60 of 120 delivered and verified (L1, L2) before the cancellation; the replacement draft buys L3 and L4 from supplier.B " +
				"with 40 PCS overbuy (the offer's ST01 MOQ of 100; 60 alone is MOQ_NOT_MET).",
		}})
	s.toS5()
	s.commitment()
	s.quality()
	s.phase = "PUR: partial delivery"
	s.source(brcMerchandiser, "forwarder.receipt", "合成货代进仓单：provider.1 于 CNNGB 收到 supplier.A 交来 ST01 红色 S/M 60 PCS。")
	s.recordDelivery("delivery.1", [][2]string{{"L1", "24"}, {"L2", "36"}})
	s.verifyDelivery("delivery.1", [][2]string{{"L1", "24"}, {"L2", "36"}})

	s.phase = "PUR: cancellation"
	v, _ := s.commitmentView()
	version := brcNum(v["row_version"])
	s.source(brcPurchaser, "supplier.cancel", "合成供应商书面通知：supplier.A 无法生产 ST01 蓝色 S/M（L3 12、L4 48），请求取消采购订单余量。")
	cancel := map[string]any{"commitment_id": "commitment.1", "expected_version": version, "reason": "供应商书面确认无法交付蓝色两款（合成）"}
	s.refused(brcPurchaser, "pur", purapi.Contract, purapi.CommandCancelCommitment, cancel, http.StatusUnprocessableEntity, kernel.CodeSchemaInvalid)
	s.figure("cancel_without_evidence", kernel.CodeSchemaInvalid)
	cancel["evidence"] = []any{map[string]any{"source_id": "supplier.cancel", "locator": "取消通知"}}
	r := s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandCancelCommitment, cancel)
	s.want("cancel", fmt.Sprint(r["status"], " v", r["row_version"]), fmt.Sprintf("cancelled v%d", version+1))
	v, lines := s.commitmentView()
	s.want("commitment_after_cancel", fmt.Sprint(v["status"], " revision ", v["revision"], " ", brcDig(v, "revisions", 0, "kind"), "; ", lines),
		"cancelled revision 2 cancel; L1 24/24 L2 36/36 L3 0/0 L4 0/0")
	s.want("allocations_after_cancel", s.allocationsView(), "L1 24/24 L2 36/36 L3 0/12 L4 0/48")
	s.refused(brcPurchaser, "pur", purapi.Contract, purapi.CommandVoidDraft, map[string]any{"draft_id": "purchase.1", "expected_version": 1,
		"reason": "取消后作废草稿"}, http.StatusConflict, purapi.CodePurchaseDraftCommitted)
	s.refused(brcPurchaser, "pur", purapi.Contract, purapi.CommandRequestCommitment, map[string]any{"commitment_id": "commitment.again",
		"draft_id": "purchase.1", "draft_hash": s.draftHash}, http.StatusConflict, purapi.CodePurchaseDraftCommitted)
	s.figure("draft_after_cancel", "void and request "+purapi.CodePurchaseDraftCommitted)
	cancel["expected_version"] = version + 1
	s.refused(brcPurchaser, "pur", purapi.Contract, purapi.CommandCancelCommitment, cancel, http.StatusUnprocessableEntity, kernel.CodeInvalidState)
	s.figure("cancel_again", kernel.CodeInvalidState)

	s.phase = "PUR: the verified delivery stays a shipment source"
	status, out := s.prepareShipment("shipment.1", []s5scShipLine{{"S1", "L1", "24", "delivery.1"}, {"S2", "L2", "36", "delivery.1"}})
	s.want("shipment_on_verified_delivery", fmt.Sprint(status, " ", out["status"]), "200 "+fulapi.StatusPrepared)

	s.phase = "PUR: a new draft for the released quantity"
	r = s.cmd(brcPurchaser, "pur", purapi.Contract, "purchase.draft.prepare", map[string]any{"draft_id": "purchase.2", "order_id": "order.1",
		"selection_reason": "supplier.A 取消后改由 supplier.B 供货蓝色两款", "lines": []any{
			map[string]any{"order_line_id": "L3", "quantity": "12", "source": map[string]any{"kind": purapi.KindSupplierOffer, "id": "offer.B", "revision": 1,
				"condition_line_id": "OL3"}},
			map[string]any{"order_line_id": "L4", "quantity": "48", "source": map[string]any{"kind": purapi.KindSupplierOffer, "id": "offer.B", "revision": 1,
				"condition_line_id": "OL4"}, "overbuy": map[string]any{"quantity": "40", "reason": "补足 ST01 款号合计 MOQ 100 PCS（合成）"}}}})
	d := s.get(brcPurchaser, "/api/brant/v1/pur/purchase_drafts/purchase.2")
	s.want("replacement_draft", fmt.Sprint(r["status"], " ", d["supplier_id"], " ", d["net_minor"]), "prepared supplier.B 53000")
	s.want("allocations_after_new_draft", s.allocationsView(), "L1 24/24 L2 36/36 L3 12/12 L4 48/48")
	s.dispatch("after the cancellation")
	var released []string
	for _, e := range s.delivered {
		if e.Type == purapi.EventPurchaseCommitmentCancelled {
			var p map[string]any
			s.w.must(json.Unmarshal(e.Payload, &p))
			for _, l := range brcList(p["lines"]) {
				released = append(released, fmt.Sprint(brcDig(l, "order_line_id"), " kept ", brcDig(l, "verified_quantity"), " released ", brcDig(l, "released_quantity")))
			}
		}
	}
	s.want("purchase_commitment_cancelled", strings.Join(released, "; "), "L1 kept 24 released 0; L2 kept 36 released 0; L3 kept 0 released 12; L4 kept 0 released 48")
}

// BT21 (baseline 9.4; tactical BT21 "放行 200 件后 100 件已出运，再撤回放行并重放事件 → 未发部分冻结；已发部分保留；
// 重复撤回无重复扣量"; QLT-INV-002, FUL-INV-002, R26): BRC-001 with its commitment, quality releases and
// verified delivery. On L4's release of 48 PCS the merchandiser ships 24 (shipment.1, released and
// confirmed) and prepares the other 24 (shipment.2, its release evaluated granted). qc revokes the
// release (the colour re-inspection): shipment.2's confirmation is QUALITY_RELEASE_REQUIRED and a
// new evaluation is recorded blocked with it, shipment.1 stays confirmed and FUL's fulfilment keeps
// L4's 24 shipped; a further revocation is answered replayed and publishes nothing; the relay
// delivers the one QualityReleaseRevoked and its redelivery changes nothing; QLT's subject shows
// 48 conforming, 48 released (the revoked release counts in full while its frozen 24 awaits a
// re-inspection) and 0 releasable, so a new release of the frozen 24 is
// QUALITY_RELEASE_EXCEEDS_CONFORMING. S5R-D18 (R5): the revocation recorded 24 shipped and 24 frozen
// from FUL's ReleaseConsumption v1 (R79); qc re-inspects L4 after the revocation, superseding
// insp.L4 and restating all 48 conforming; the revoked release then counts its 24 shipped, the frozen
// 24 is released again (rel.L4.again) and ships, and L4 has shipped its 48. Unit tests: A8
// TestRevocationFreezesTheUnshippedRestBT21, R5 TestFrozenQuantityReleasedAgainAfterReinspectionBT21.
func TestS5SCBT21RevocationAfterPartialShipmentOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "BT21", Title: "放行后部分出运再撤回 (BT21): the unshipped rest frozen, the shipped kept, a repeated revoke deducts nothing; the frozen rest released again after a re-inspection (S5R-D18)",
		Design:  []string{"BT21", "QLT-INV-001", "QLT-INV-002", "FUL-INV-002", "R26", "R79", "S5R-D18"},
		RestsOn: []string{"A8", "B1a", "C1", "D1", "R5"},
		UnitTests: []string{"TestRevocationFreezesTheUnshippedRestBT21 (A8)", "TestQualityReleaseGuardDecidesEveryCommitOrder (A8)",
			"TestFrozenQuantityReleasedAgainAfterReinspectionBT21 (R5)", "TestFrozenQuantityReleasedAgainAfterReinspection (R5)"},
		Deviations: []string{
			"BRC-001's L4 release of 48 PCS with 24 shipped stands for the baseline's 200 released / 100 shipped (the same half).",
		}})
	s.toS5()
	s.commitment()
	s.quality()
	s.delivery()

	s.phase = "BT21: half of L4 shipped"
	s.shipmentID = "shipment.1"
	status, out := s.prepareShipment("shipment.1", []s5scShipLine{{"S1", "L4", "24", "delivery.1"}})
	if status != http.StatusOK {
		t.Fatalf("shipment.1 %d %v", status, out)
	}
	s.evaluate(fulapi.ScopeShipment)
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": "shipment.1", "expected_version": 1,
		"evidence_ids": []string{"forwarder.receipt"}})
	status, out = s.prepareShipment("shipment.2", []s5scShipLine{{"S1", "L4", "24", "delivery.1"}})
	if status != http.StatusOK {
		t.Fatalf("shipment.2 %d %v", status, out)
	}
	s.shipmentID = "shipment.2"
	r, _ := s.evaluate(fulapi.ScopeShipment)
	s.want("shipment_2_release_before_revocation", r["status"], fulapi.ReleaseGranted)
	release2 := brcStr(r["object_id"])

	s.phase = "BT21: revocation"
	s.source(brcQC, "recheck", "合成复检记录：ST01.blue.M 色差超出标样，撤回质量放行 rel.L4。")
	revoke := map[string]any{"release_id": "rel.L4", "reason": "复检发现色差，撤回放行", "evidence_ids": []string{"recheck"}}
	r = s.cmd(brcQC, "qlt", qltapi.Contract, qltapi.CommandRevokeRelease, revoke)
	s.want("revoke", fmt.Sprint(r["status"], " v", r["row_version"], " replayed ", r["replayed"]), "revoked v2 replayed false")
	r = s.cmd(brcQC, "qlt", qltapi.Contract, qltapi.CommandRevokeRelease, map[string]any{"release_id": "rel.L4", "reason": "重复撤回"})
	s.want("revoke_again", fmt.Sprint(r["status"], " v", r["row_version"], " replayed ", r["replayed"]), "revoked v2 replayed true")
	s.refused(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": "shipment.2", "expected_version": 1,
		"evidence_ids": []string{"forwarder.receipt"}}, http.StatusConflict, qltapi.CodeQualityReleaseRequired)
	s.figure("confirm_rest_after_revocation", qltapi.CodeQualityReleaseRequired)
	r, e := s.evaluate(fulapi.ScopeShipment)
	s.want("evaluation_after_revocation", fmt.Sprint(r["status"], " ", e["code"], " same release ", brcStr(r["object_id"]) == release2),
		"blocked "+qltapi.CodeQualityReleaseRequired+" same release true")
	v := s.get(brcMerchandiser, "/api/brant/v1/ful/shipments/shipment.1")
	s.want("shipment_1", v["status"], fulapi.StatusConfirmed)
	v = s.get(brcMerchandiser, "/api/brant/v1/ful/shipments/shipment.2")
	s.want("shipment_2", v["status"], fulapi.StatusPrepared)
	lines, total := s.shipped()
	s.want("shipped", fmt.Sprintf("L4 %s, %d in all", lines["L4"], total), "L4 24, 24 in all")

	s.dispatch("after the revocation")
	n := 0
	for _, e := range s.delivered {
		if e.Type == qltapi.EventQualityReleaseRevoked {
			n++
		}
	}
	s.want("quality_release_revoked_events", n, "1")
	rel := s.get(brcQC, "/api/brant/v1/qlt/quality_releases/rel.L4")
	s.want("release", fmt.Sprint(rel["state"], " by ", brcDig(rel, "revocation", "revoked_by", "id")), "revoked by qc")
	s.want("revocation_shipped_frozen", fmt.Sprint(brcDig(rel, "revocation", "shipped_quantity"), " ", brcDig(rel, "revocation", "frozen_quantity")),
		"24 24")
	subjectL4 := "/api/brant/v1/qlt/subjects/" + qltapi.SubjectCommitmentLine + "/commitment.1/lines/L4"
	st := s.get(brcQC, subjectL4)
	s.want("subject_L4", fmt.Sprint("conforming ", st["conforming"], " released ", st["released"], " frozen ", st["frozen"], " releasable ",
		st["releasable"]), "conforming 48 released 48 frozen 24 releasable 0")
	subject := map[string]any{"kind": qltapi.SubjectCommitmentLine, "id": "commitment.1", "line_id": "L4"}
	again := map[string]any{"release_id": "rel.L4.again", "subject": subject, "quantity": "24",
		"valid_until": kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))}
	s.refused(brcQC, "qlt", qltapi.Contract, qltapi.CommandGrantRelease, again, http.StatusUnprocessableEntity, qltapi.CodeQualityReleaseExceedsConforming)
	s.figure("release_the_frozen_24_again", qltapi.CodeQualityReleaseExceedsConforming)

	s.phase = "BT21: re-inspection and release of the frozen 24 (S5R-D18)"
	s.fileOf(brcQC, "report.recheck", "recheck-ST01-L4.txt", "合成复检报告：L4 ST01 蓝/M 48 件（已出运 24 件按原检验、冻结 24 件复检），色差在标样范围内，全部合格。")
	s.cmd(brcQC, "qlt", qltapi.Contract, qltapi.CommandRecordInspection, map[string]any{"inspection_id": "insp.L4.recheck", "subject": subject,
		"inspected_quantity": "48", "conforming_quantity": "48", "nonconforming_quantity": "0", "sampling": map[string]any{"method": qltapi.SamplingFull},
		"standard": map[string]any{"source_id": "standard"}, "inspector": map[string]any{"kind": qltapi.InspectorInternal},
		"report_file_id": "report.recheck", "inspected_at": now(), "supersedes_inspection_id": "insp.L4", "reason": "撤回后复检冻结的 24 件"})
	st = s.get(brcQC, subjectL4)
	s.want("subject_L4_after_recheck", fmt.Sprint("conforming ", st["conforming"], " released ", st["released"], " frozen ", st["frozen"],
		" releasable ", st["releasable"]), "conforming 48 released 24 frozen 0 releasable 24")
	r = s.cmd(brcQC, "qlt", qltapi.Contract, qltapi.CommandGrantRelease, again)
	s.want("release_the_frozen_24_after_recheck", r["status"], qltapi.StatusGranted)
	shipLines := s.shipmentLines([]s5scShipLine{{"S1", "L4", "24", "delivery.1"}})
	shipLines[0].(map[string]any)["quality_release_id"] = "rel.L4.again"
	status, out = s.post(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandPrepareShipment, map[string]any{"shipment_id": "shipment.3",
		"order_id": "order.1", "route": fulapi.RouteSupplierDirect, "lines": shipLines})
	if status != http.StatusOK {
		t.Fatalf("shipment.3 %d %v", status, out)
	}
	s.shipmentID = "shipment.3"
	r, _ = s.evaluate(fulapi.ScopeShipment)
	s.want("shipment_3_release", r["status"], fulapi.ReleaseGranted)
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": "shipment.3", "expected_version": 1,
		"evidence_ids": []string{"forwarder.receipt"}})
	lines2, total2 := s.shipped()
	s.want("shipped_after_recheck", fmt.Sprintf("L4 %s, %d in all", lines2["L4"], total2), "L4 48, 48 in all")
}

// openingReceivable has treasury record C10's opening receivable of customer.1 at the cutoff (a
// prior order's statement, open at go-live), on the legacy ledger's extract treasury registers (S5R-D26).
func (s *s5sc) openingReceivable(id, document, amount string, dueDays int) map[string]any {
	s.t.Helper()
	s.phase = "STL opening balance"
	s.source(brcTreasury, "ledger."+id, "合成旧系统应收余额表：customer.1 "+document+" 未结 "+amount+" USD（切换日）。")
	cutoff := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	return s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordOpening, map[string]any{"opening_id": id, "kind": stlapi.OpeningReceivable,
		"cutoff_at": kernel.FormatTimestamp(cutoff), "source": map[string]any{"document": document,
			"issued_at": kernel.FormatTimestamp(cutoff.AddDate(0, 0, -40)), "amount": amount},
		"counterparty_id": "customer.1", "order_reference": "SO-PRIOR-1", "currency": s.usd, "amount": amount,
		"installments": []any{map[string]any{"amount": amount, "due_at": kernel.FormatTimestamp(time.Now().Add(time.Duration(dueDays) * 24 * time.Hour))}},
		"evidence_ids": []string{"ledger." + id}})
}

// SC-09 (Context Map SC-09 "赊销订单的信用占用与禁运目的国"; ES-11; STL-INV-004/005, CNX-INV-001, TRD-INV-022/023;
// D-6, D-7, D-8, D-20; plan §4 facts D2: seeded by an opening receivable of 7,000.00 through C10):
// customer.1 pays 30 % on order confirmation and 70 % 30 days after shipment; its limit is
// 10,000.00 USD (two credit managers above the top tier); treasury records the opening receivable of
// a prior settled order, 7,000.00 open at the cutoff (C10, STL-INV-012): the exposure is 7,000.00.
// Order A (100 PCS of ST01.red.S at 5,714.29 USD per 100: P1 1,714.29 + P2 4,000.00, consignee in DE)
// is approved and concluded clear by compliance; order.check answers CREDIT_RESERVATION_REQUIRED; sales'
// reservation of the 4,000.00 opens a credit decision (CREDIT_LIMIT_EXCEEDED: 7,000.00 + 4,000.00 over
// 10,000.00, 1,000.00 in excess) and activation is refused; credit.manager.1 releases the exception
// within its tier; sales reserves again on the decision (basis credit_decision) and activates order
// A: the commit evidence records the reservation and its basis. Order B (consignee in AQ, the
// synthetic CN embargo programme's territory) is approved by the order reviewer too, but compliance's
// evaluation finds the embargo and concludes blocked: order.check and order.activate answer
// TRADE_COMPLIANCE_NOT_CLEAR — the order review's approval does not override it — and order B stays
// a draft. Unit tests: C10 TestSC09AnOpeningReceivableCountsInTheCreditExposure, A6
// TestCreditReservationWithinTheLimitOrByDecision, B4 TestOrderActivationAsksTradeComplianceThenCredit,
// A7 TestTDAT12EmbargoedDestinationIsBlocked.
func TestS5SCSC09CreditDecisionAndEmbargoedOrderOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "SC-09", Title: "赊销订单的信用占用与禁运目的国: opening 7,000 + portion 4,000 > 10,000 → decision → activate; embargoed order stays draft",
		Design:    []string{"SC-09", "ES-11", "STL-INV-004", "STL-INV-005", "STL-INV-012", "CNX-INV-001", "TRD-INV-022", "TRD-INV-023", "D-6", "D-7", "D-8", "D-20"},
		RestsOn:   []string{"A6", "C10", "A7", "B4", "A3b"},
		UnitTests: []string{"TestSC09AnOpeningReceivableCountsInTheCreditExposure (C10)", "TestCreditReservationWithinTheLimitOrByDecision (A6)", "TestTDAT12EmbargoedDestinationIsBlocked (A7)"},
		Deviations: []string{
			"Order A's portion of 4,000.00 is 70 % of 5,714.29 (100 PCS at 5,714.29 USD per 100, HALF_UP to cents): the design's 4,000 with " +
				"its 30/70 terms; P1 is 1,714.29.",
			"The two orders' destinations come from customer.1's consignee addresses with the countries PTY records (DE; AQ, the synthetic " +
				"embargo territory) rather than entered by compliance: PTY refuses no country and CNX takes the consignee's (A7 decision 3).",
		}})
	s.dealFoundation(s5scTenant{terms: "30% on order confirmation, 70% 30 days after shipment", stages: []any{
		map[string]any{"stage_id": "P1", "percent": "30", "trigger": polapi.TriggerOrderConfirmed, "days": 0},
		map[string]any{"stage_id": "P2", "percent": "70", "trigger": polapi.TriggerAfterShipment, "days": 30}},
		incoterm: "FCA", place: "TEST NAMED PLACE", calc: "USD", purchase: "USD", skus: []string{"ST01.red.S"}, limit: "10000.00"})
	s.openingReceivable("ob.prior", "AR-PRIOR-001", "7000.00", 20)
	s.want("exposure_with_opening_receivable", s.exposure(brcCreditOfficer), "700000")

	a := &s5scDeal{id: "sc09a", lines: []s5scLine{{"L1", "ST01.red.S", "100", "5714.29", "5.00"}}, priceBasis: "100", consignee: "cons.de", freight: "60.00"}
	s.form(a)
	s.clear(a, false)
	c := s.check(a)
	s.want("order_a_check_before_credit", fmt.Sprint(c["activatable"], " ", c["code"]), "false "+stlapi.CodeCreditReservationRequired)
	r := s.reserve(a, "res.sc09a")
	s.want("reserve_4000", r["status"], stlapi.StatusDecisionOpened)
	decision := brcStr(r["object_id"])
	v := s.get(brcSales, "/api/brant/v1/stl/credit_decisions/"+decision)
	s.want("credit_decision", fmt.Sprint(v["reasons"], " portion ", v["portion_minor"], " exposure ", v["exposure_minor"], " limit ", v["limit_minor"],
		" excess ", v["excess_minor"], " ", v["status"]), "["+stlapi.CodeCreditLimitExceeded+"] portion 400000 exposure 700000 limit 1000000 excess 100000 open")
	s.refused(brcSales, "trd", trdapi.Contract, "order.activate", map[string]any{"order_id": a.order(), "expected_version": 3,
		"review_id": "review." + a.order()}, http.StatusConflict, stlapi.CodeCreditReservationRequired)
	s.figure("activate_before_the_exception", stlapi.CodeCreditReservationRequired)
	d := s.cmd(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandDecideException, map[string]any{"decision_id": decision, "expected_version": 1,
		"decision": stlapi.DecisionRelease, "credit_policy": s.creditPolicy, "reason": "客户历史付款良好，批准超额 1,000.00 USD（合成）"})
	s.want("credit_exception", d["status"], stlapi.DecisionReleased)
	r = s.reserve(a, "res.sc09a.2")
	res := s.get(brcSales, "/api/brant/v1/stl/credit_reservations/res.sc09a.2")
	s.want("reservation_on_the_decision", fmt.Sprint(r["status"], " ", res["portion_minor"], " ", res["basis"]), "reserved 400000 "+stlapi.BasisCreditDecision)
	o := s.activate(a)
	entries := brcMap(brcDig(o, "commits", 0, "entries"))
	s.want("order_a_commit_evidence", fmt.Sprint(o["status"], " ", entries[trdapi.GuardSTLCredit], " ", entries[trdapi.EvidenceCreditReservation], " ",
		entries[trdapi.EvidenceCreditPortion], " ", entries[trdapi.EvidenceCreditBasis]), "active covered res.sc09a.2 400000 "+stlapi.BasisCreditDecision)
	s.figure("order_a_commit_trade_compliance", brcStr(entries[trdapi.GuardCNXTradeCompliance]))

	b := &s5scDeal{id: "sc09b", lines: []s5scLine{{"L1", "ST01.red.S", "10", "8.00", "5.00"}}, consignee: "cons.aq", freight: "10.00"}
	s.form(b)
	id, evaluation := s.evaluateCompliance(b)
	var findings []string
	for _, f := range brcList(evaluation["findings"]) {
		findings = append(findings, fmt.Sprint(brcDig(f, "programme_id"), " ", brcDig(f, "country")))
	}
	s.want("order_b_evaluation", fmt.Sprint(evaluation["outcome"], " ", strings.Join(findings, ", ")), cnxapi.OutcomeMatch+" SYN-CN-EMB-1 AQ, SYN-CN-EXP-1 AQ")
	s.cmd(brcCompliance, "cnx", cnxapi.Contract, cnxapi.CommandDecide, map[string]any{"check_id": id, "expected_version": 1,
		"evaluation_id": evaluation["evaluation_id"], "conclusion": cnxapi.ConclusionBlocked, "reason": "收货地属合成禁运项目所列地区（SYN-CN-EMB-1）",
		"evidence_ids": []string{"evidence"}})
	c = s.check(b)
	s.want("order_b_check", fmt.Sprint(c["activatable"], " ", c["code"]), "false "+cnxapi.CodeTradeComplianceNotClear)
	s.refused(brcSales, "trd", trdapi.Contract, "order.activate", map[string]any{"order_id": b.order(), "expected_version": 3,
		"review_id": "review." + b.order()}, http.StatusConflict, cnxapi.CodeTradeComplianceNotClear)
	s.figure("order_b_activate", cnxapi.CodeTradeComplianceNotClear)
	ob := s.get(brcSales, "/api/brant/v1/trd/orders/"+b.order())
	s.want("order_b_status", fmt.Sprint(ob["status"], " review ", brcDig(ob, "reviews", 0, "decision_state"), " by ", brcDig(ob, "reviews", 0, "decided_by", "id")),
		trdapi.OrderDraft+" review approved by "+brcOrderReviewer)
	s.dispatch("after SC-09's two orders")
	s.want("exposure_after_activation", s.exposure(brcCreditOfficer), "1100000")
}

// s5scUSDTenant is a tenant on BRC-001's terms (30 % on order confirmation, 70 % 30 days after
// shipment, FCA, USD) with skus and a limit of 10,000.00 USD.
func s5scUSDTenant(skus ...string) s5scTenant {
	return s5scTenant{terms: "30% on order confirmation, 70% 30 days after shipment", stages: []any{
		map[string]any{"stage_id": "P1", "percent": "30", "trigger": polapi.TriggerOrderConfirmed, "days": 0},
		map[string]any{"stage_id": "P2", "percent": "70", "trigger": polapi.TriggerAfterShipment, "days": 30}},
		incoterm: "FCA", place: "TEST NAMED PLACE", calc: "USD", purchase: "USD", skus: skus, limit: "10000.00"}
}

// toShippedDeal forms d, concludes it clear, reserves its credit, activates it, drafts and commits its
// purchase, releases its quality, verifies its delivery in full, relays OrderActivated and has the P1
// share received and applied (stage P1, amount p1) and the whole order shipped as shipment.<id>.
func (s *s5sc) toShippedDeal(d *s5scDeal, p1 string) {
	s.t.Helper()
	s.form(d)
	s.clear(d, false)
	if r := s.reserve(d, "res."+d.id); r["status"] != stlapi.ReservationReserved {
		s.t.Fatalf("reserve %v", r)
	}
	s.activate(d)
	s.draft(d, nil)
	s.dispatch("after the activation of " + d.order())
	s.fundOf("fund."+d.id+".p1", "USD", s.bank, p1, "MT103-"+strings.ToUpper(d.id)+"-P1")
	s.applyOn("app."+d.id+".p1", "fund."+d.id+".p1", s5scStage(d.order(), "P1"), p1, nil)
	s.commit(d, "purchase."+d.id)
	s.inspect(d, nil)
	var all [][2]string
	for _, l := range d.lines {
		all = append(all, [2]string{l.id, l.qty})
	}
	s.deliver(d, "delivery."+d.id, all, all)
	s.ship(d, "shipment."+d.id, all)
}

// payPurchase has treasury confirm the purchase statement of d's verified delivery, record our
// payment of amount to its supplier and apply it.
func (s *s5sc) payPurchase(d *s5scDeal, currency string, account map[string]any, amount string) map[string]any {
	s.t.Helper()
	s.phase = "deal " + d.id + ": purchase statement and payment"
	var sources []any
	for _, l := range d.lines {
		sources = append(sources, map[string]any{"delivery_id": d.delivery, "line_id": l.id, "version": 1})
	}
	id := "stmt.purchase." + d.id
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmSettlement, map[string]any{"statement_id": id, "kind": stlapi.StatementPurchase,
		"sources": sources, "evidence_ids": []string{"supplier.ack." + d.id}})
	v := s.get(brcTreasury, "/api/brant/v1/stl/payables/"+id)
	s.source(brcTreasury, "bank.pay."+d.id, "合成银行付款回单：向 "+d.supplierID()+" 付款 "+amount+" "+currency+"。")
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, map[string]any{"fund_record_id": "fund.pay." + d.id, "kind": stlapi.FundPayment,
		"counterparty_id": d.supplierID(), "currency": s.money(currency), "bank_account": account, "received_amount": amount, "value_date": now(),
		"bank_reference": "TT-PAY-" + strings.ToUpper(d.id), "evidence_ids": []string{"bank.pay." + d.id}})
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, map[string]any{"application_id": "app.pay." + d.id, "fund_record_id": "fund.pay." + d.id,
		"target": map[string]any{"kind": "payable", "statement_id": id}, "amount": amount})
	return v
}

// freightOf has credit.officer record provider.1's freight of d as estimated (its logistics offer),
// treasury confirm it on the forwarder's bill and credit.officer allocate it wholly to the order.
func (s *s5sc) freightOf(d *s5scDeal, currency, amount string) {
	s.t.Helper()
	s.phase = "deal " + d.id + ": freight"
	s.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandRecordCharge, map[string]any{"charge_id": "freight." + d.id, "expected_version": 0,
		"status": "estimated", "source": map[string]any{"kind": "logistics_offer", "party_id": "provider.1", "reference": "log." + d.id},
		"beneficiaries": []any{map[string]any{"kind": "order", "order_id": d.order()}}, "fee_category": s.freight, "currency": s.money(currency),
		"conversion": map[string]any{"kind": "same_currency"}, "amount": amount, "occurred_on": today(), "analysis_on": today(),
		"basis": "provider.1 海运报价 log." + d.id})
	s.source(brcTreasury, "freight.bill."+d.id, "合成货代账单：provider.1 海运费 "+amount+" "+currency+"，"+d.order()+"。")
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmCharge, map[string]any{"charge_id": "freight." + d.id, "expected_version": 1,
		"evidence_ids": []string{"freight.bill." + d.id}})
	s.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandAllocateCharge, map[string]any{"charge_id": "freight." + d.id, "expected_version": 2,
		"basis": "amount", "data_basis": "单一受益订单，全额", "lines": []any{map[string]any{"line_id": "A1",
			"target": map[string]any{"kind": "order", "order_id": d.order()}, "weight": "1"}}})
}

// settleResult has treasury review and close d's order result and returns the result view.
func (s *s5sc) settleResult(d *s5scDeal) map[string]any {
	s.t.Helper()
	s.phase = "deal " + d.id + ": order result"
	r := s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandReviewOrderResult, map[string]any{"order_id": d.order(), "expected_version": 0,
		"note": "履约、账款、采购与费用核对完毕（合成）"})
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandCloseOrderResult, map[string]any{"order_id": d.order(), "expected_version": r["row_version"],
		"reason": "订单经营结果结清"})
	return s.get(brcTreasury, "/api/brant/v1/stl/orders/"+d.order()+"/result")
}

// Baseline 4.7 S-B (credit note; S5-18, STL-INV-008, CN-AT11 STL half; baseline 4.5 net sales): an
// independent USD case — 100 PCS of ST01.red.S at 10.00 USD = 1,000.00, bought at 4.00 (400.00), freight
// 50.00 confirmed: the original result is 550.00. The order runs end to end (order, compliance,
// credit, commitment, quality, delivery, shipment, P1 300.00 received before shipment); the sales
// statement is 1,000.00. The customer returns 10 PCS of the batch and the sales clerk requests a
// return credit note of 100.00 USD; the clerk cannot confirm it (no permission), credit.manager.1
// confirms it (CreditConfirmed); applied to P2 it leaves 600.00 open, which the customer pays. The
// statement still lists 100 PCS (the shipment is not rewritten); net sales are 900.00. The purchase
// statement (400.00) is paid, the freight confirmed and allocated, the order closed, and the order
// result settles at 900.00 − 400.00 − 50.00 = 450.00 (50 %). Unit tests: C8 TestBaseline47SBCreditNote,
// C9 TestSBCreditNoteInTheConfirmedResult.
func TestS5SCSBCreditNoteOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "S-B", Title: "基线 4.7 S-B 退货与贷项: USD 100 credit note on 1,000 → net 900, result 450",
		Design:    []string{"baseline 4.7 S-B", "baseline 4.5", "STL-INV-008", "SM-STL-CREDIT-NOTE", "STL-INV-011", "CN-AT11 (STL half)"},
		RestsOn:   []string{"C8", "C9", "B2b", "D1"},
		UnitTests: []string{"TestBaseline47SBCreditNote (C8)", "TestSBCreditNoteInTheConfirmedResult (C9)"},
		Humans:    map[string]string{"sales.clerk": "sales_clerk"},
		Deviations: []string{
			"The confirmed result is 450.00, not the baseline's 490.00: the return of the 10 PCS to stock at their cost of 40.00 is STK's " +
				"(S6-01), not in S5 (C8 decision 13, C9); the credit note and the receivable are STL's.",
			"Payment terms 30/70 after shipment (S-B states none); the credit note is applied to P2.",
		},
		Omitted: []string{"the return of the 10 PCS into stock at their cost (40.00): STK is S6-01; the customer's refund is not part of S-B's figures"}})
	s.dealFoundation(s5scUSDTenant("ST01.red.S"))
	d := &s5scDeal{id: "sb", lines: []s5scLine{{"L1", "ST01.red.S", "100", "10.00", "4.00"}}, consignee: "cons.de", freight: "50.00"}
	s.toShippedDeal(d, "300.00")
	s.settle("stmt.sb", "shipment.sb", 1)
	got, _ := s.receivable("stmt.sb")
	s.want("receivable", got, "100000 applied 30000 open 70000")

	s.phase = "S-B: the credit note"
	s.source(brcSales, "return.sb", "合成退货记录：customer.1 退回原批次 ST01.red.S 10 PCS，经确认给予贷项 100.00 USD。")
	r := s.cmd("sales.clerk", "stl", stlapi.Contract, stlapi.CommandRequestCreditNote, map[string]any{"credit_note_id": "cn.sb", "kind": stlapi.CreditReturn,
		"order_id": d.order(), "statement_id": "stmt.sb", "amount": "100.00", "basis": "收回原批次 10 件（S-B）", "evidence_ids": []string{"return.sb"}})
	s.want("credit_note_requested", fmt.Sprint(r["status"], " v", r["row_version"]), stlapi.CreditRequested+" v1")
	confirm := map[string]any{"credit_note_id": "cn.sb", "expected_version": 1, "decision": "confirm", "reason": "退货已核实（合成）"}
	s.refused("sales.clerk", "stl", stlapi.Contract, stlapi.CommandConfirmCredit, confirm, http.StatusForbidden, kernel.CodePermissionDenied)
	s.figure("requester_confirms", kernel.CodePermissionDenied)
	r = s.cmd(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandConfirmCredit, confirm)
	s.want("credit_note_confirmed", fmt.Sprint(r["status"], " v", r["row_version"]), stlapi.CreditConfirmed+" v2")
	n := s.get(brcTreasury, "/api/brant/v1/stl/credit_notes/cn.sb")
	s.figure("credit_note", fmt.Sprint(n["kind"], " ", n["amount_minor"], " effective ", n["effective"], " rest ", n["rest_minor"]))
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, map[string]any{"application_id": "app.cn.sb", "credit_note_id": "cn.sb",
		"target": s5scInstallment("stmt.sb", "P2"), "amount": "100.00"})
	got, _ = s.receivable("stmt.sb")
	s.want("receivable_after_credit_note", got, "100000 applied 40000 open 60000")
	st := s.get(brcTreasury, "/api/brant/v1/stl/settlement_statements/stmt.sb")
	s.want("statement_quantity_kept", fmt.Sprint(brcDig(st, "lines", 0, "quantity"), " credited ", st["credited_minor"]), "100 credited 10000")
	s.fundOf("fund.sb.p2", "USD", s.bank, "600.00", "MT103-SB-P2")
	s.applyOn("app.sb.p2", "fund.sb.p2", s5scInstallment("stmt.sb", "P2"), "600.00", nil)
	got, _ = s.receivable("stmt.sb")
	s.want("receivable_paid", got, "100000 applied 100000 open 0")
	pos := s.get(brcTreasury, "/api/brant/v1/stl/orders/"+d.order()+"/settlement")
	s.want("order_settlement", fmt.Sprint("settled ", pos["settled_minor"], " credited ", pos["credited_minor"], " net sales ", pos["net_sales_minor"]),
		"settled 100000 credited 10000 net sales 90000")

	pay := s.payPurchase(d, "USD", s.bank, "400.00")
	s.want("payable", pay["amount_minor"], "40000")
	s.freightOf(d, "USD", "50.00")
	s.closeDeal(d, "receipt."+d.delivery)
	s.dispatch("after the close of order.sb")
	v := s.settleResult(d)
	confirmed := brcMap(brcDig(v, "live", "confirmed"))
	s.want("order_result", fmt.Sprint(v["status"], " net ", confirmed["net_sales_minor"], " credits ", confirmed["credits_minor"], " cost ",
		brcDig(confirmed, "batches", 0, "cost_of_sales_minor"), " charges ", confirmed["direct_charges_minor"], " result ", confirmed["result_minor"],
		" margin ", confirmed["margin"]), "settled net 100000 credits 10000 cost 40000 charges 5000 result 45000 margin 50.00")
	count := 0
	for _, e := range s.delivered {
		if e.Type == stlapi.EventCreditConfirmed {
			count++
		}
	}
	s.want("credit_confirmed_events", count, "1")
}

// registerLicence has doc.1 register an export licence of the tenant's SKUs for destination DE and
// end user customer.1 (the consignee), valid over [from, until), on a scan doc.1 registers.
func (s *s5sc) registerLicence(number, quantity, value string, skus []string, from, until time.Time) string {
	s.t.Helper()
	s.fileOf(brcDocumentation, "licence."+number, "licence-"+number+".txt", "合成出口许可证扫描件："+number+"（测试数据，非真实许可证）。")
	var specs []any
	for _, sku := range skus {
		specs = append(specs, map[string]any{"sku_id": sku, "spec_revision": 1})
	}
	r := s.cmd(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRegisterLicense, map[string]any{"license_number": number,
		"issuer": "合成发证机关（测试）", "holder": "合成企业", "goods": map[string]any{"sku_specs": specs}, "licensed_quantity": quantity,
		"unit": "PCS", "currency": map[string]any{"id": "USD", "revision": 1}, "licensed_value": value, "destination": syntheticClear,
		"end_user": map[string]any{"party_id": "customer.1", "name": "customer.1"}, "valid_from": kernel.FormatTimestamp(from),
		"valid_until": kernel.FormatTimestamp(until), "file_id": "licence." + number})
	return brcStr(r["object_id"])
}

// allocateLicence has doc.1 allocate licence to shipment and returns the status and answer.
func (s *s5sc) allocateLicence(licence, allocation, shipment string) (int, map[string]any) {
	return s.post(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandAllocateLicense, map[string]any{"license_id": licence,
		"allocation_id": allocation, "target": map[string]any{"kind": "shipment", "id": shipment}})
}

// TD-AT13 (tactical TD-AT13 "许可 1,000 件；已分配 600，再分配 500；另一许可证已过期 → EXPORT_LICENSE_EXCEEDED；过期许可证返回
// EXPORT_LICENSE_EXPIRED"; CNX-INV-002, R56, FUL-INV-002; S5-14): an order of 600 PCS of ST01.red.S and 500 of
// ST01.blue.S (8.00 USD, 8,800.00) whose destination (the consignee's country, DE) compliance concludes
// clear with a licence required. Its goods are committed, released by QC, delivered and verified;
// P1 (2,640.00) is received. shipment.600's commercial release is blocked EXPORT_LICENSE_REQUIRED
// until doc.1 allocates licence A (1,000 PCS, 8,000.00 USD, DE, customer.1, the two SKUs): then
// granted, binding the allocation, and confirmed. shipment.500 on licence A is EXPORT_LICENSE_EXCEEDED
// (600 + 500 > 1,000); on licence B, which expired yesterday, EXPORT_LICENSE_EXPIRED; its release is
// blocked EXPORT_LICENSE_REQUIRED (not_allocated). Licence C (500 PCS) covers it: granted and
// confirmed. Licence A shows 600 allocated, 400 available. Unit tests: B5
// TestTDAT13ExportLicenseRegisterAllocateAndGuard, TestTDAT13ExpiredLicenceAndMismatchesAreRefused;
// C1 TestExportLicenseGatesTheRelease.
func TestS5SCTDAT13ExportLicenceOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "TD-AT13", Title: "出口许可证: 1,000 licensed, 600 allocated, 500 more → EXPORT_LICENSE_EXCEEDED; expired → EXPORT_LICENSE_EXPIRED",
		Design:    []string{"TD-AT13", "CNX-INV-002", "R56", "FUL-INV-002", "S5-14"},
		RestsOn:   []string{"B5", "C1", "A7", "D1"},
		UnitTests: []string{"TestTDAT13ExportLicenseRegisterAllocateAndGuard (B5)", "TestTDAT13ExpiredLicenceAndMismatchesAreRefused (B5)", "TestExportLicenseGatesTheRelease (C1)"},
		Deviations: []string{
			"The order's two lines are shipped as two shipments of 600 and 500 PCS (the design's allocations); licence C, covering the second, " +
				"is added so that the order ships whole.",
		}})
	skus := []string{"ST01.red.S", "ST01.blue.S"}
	s.dealFoundation(s5scUSDTenant(skus...))
	d := &s5scDeal{id: "lic", lines: []s5scLine{{"L1", "ST01.red.S", "600", "8.00", "5.00"}, {"L2", "ST01.blue.S", "500", "8.00", "5.00"}},
		consignee: "cons.de", freight: "60.00"}
	s.form(d)
	s.clear(d, true)
	s.reserve(d, "res.lic")
	o := s.activate(d)
	s.figure("commit_trade_compliance", brcStr(brcDig(o, "commits", 0, "entries", trdapi.GuardCNXTradeCompliance)))
	s.draft(d, nil)
	s.dispatch("after the activation")
	s.fundOf("fund.lic.p1", "USD", s.bank, "2640.00", "MT103-LIC-P1")
	s.applyOn("app.lic.p1", "fund.lic.p1", s5scStage(d.order(), "P1"), "2640.00", nil)
	s.commit(d, "purchase.lic")
	s.inspect(d, nil)
	s.deliver(d, "delivery.lic", [][2]string{{"L1", "600"}, {"L2", "500"}}, [][2]string{{"L1", "600"}, {"L2", "500"}})

	s.phase = "TD-AT13: licences"
	now := time.Now().UTC().Truncate(time.Second)
	a := s.registerLicence("SYN-EL-2026-A", "1000", "8000.00", skus, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	b := s.registerLicence("SYN-EL-2025-B", "1000", "8000.00", skus, now.Add(-60*24*time.Hour), now.Add(-24*time.Hour))
	s.want("licence_b_state", s.get(brcDocumentation, "/api/brant/v1/cnx/export_licenses/"+b)["state"], "expired")

	s.phase = "TD-AT13: shipment of 600"
	status, out := s.prepareOn(d, "shipment.600", [][2]string{{"L1", "600"}})
	if status != http.StatusOK {
		t.Fatalf("shipment.600 %d %v", status, out)
	}
	s.shipmentID = "shipment.600"
	version := out["row_version"]
	r, e := s.evaluate(fulapi.ScopeShipment)
	s.want("release_600_without_allocation", fmt.Sprint(r["status"], " ", e["code"]), "blocked "+cnxapi.CodeExportLicenseRequired)
	status, _ = s.allocateLicence(a, "alloc.600", "shipment.600")
	s.want("allocate_600_on_A", status, "200")
	g := s.get(brcDocumentation, "/api/brant/v1/cnx/shipments/shipment.600/export_license")
	s.want("guard_600", fmt.Sprint(g["covered"], " ", g["status"], " ", g["quantity"]), "true allocated 600")
	r, e = s.evaluate(fulapi.ScopeShipment)
	s.want("release_600", fmt.Sprint(r["status"], " licence ", brcDig(e, "bindings", "export_license", "license_id") == a), "granted licence true")
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": "shipment.600",
		"expected_version": version, "evidence_ids": []string{"receipt.delivery.lic"}})

	s.phase = "TD-AT13: shipment of 500"
	status, out = s.prepareOn(d, "shipment.500", [][2]string{{"L2", "500"}})
	if status != http.StatusOK {
		t.Fatalf("shipment.500 %d %v", status, out)
	}
	s.shipmentID = "shipment.500"
	version = out["row_version"]
	status, out = s.allocateLicence(a, "alloc.500", "shipment.500")
	s.want("allocate_500_on_A", fmt.Sprint(status, " ", out["code"]), "409 "+cnxapi.CodeExportLicenseExceeded)
	status, out = s.allocateLicence(b, "alloc.500.b", "shipment.500")
	s.want("allocate_500_on_expired_B", fmt.Sprint(out["code"], " ", brcDig(out, "field_errors", 0, "path")), cnxapi.CodeExportLicenseExpired+" /args/license_id")
	s.figure("allocate_500_on_expired_B_status", fmt.Sprint(status))
	r, e = s.evaluate(fulapi.ScopeShipment)
	s.want("release_500_unallocated", fmt.Sprint(r["status"], " ", e["code"]), "blocked "+cnxapi.CodeExportLicenseRequired)
	c := s.registerLicence("SYN-EL-2026-C", "500", "4000.00", skus, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	status, _ = s.allocateLicence(c, "alloc.500.c", "shipment.500")
	s.want("allocate_500_on_C", status, "200")
	r, _ = s.evaluate(fulapi.ScopeShipment)
	s.want("release_500", r["status"], fulapi.ReleaseGranted)
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": "shipment.500",
		"expected_version": version, "evidence_ids": []string{"receipt.delivery.lic"}})
	lic := s.get(brcDocumentation, "/api/brant/v1/cnx/export_licenses/"+a)
	s.want("licence_a_usage", fmt.Sprint(brcDig(lic, "usage", "allocated_quantity"), " allocated, ", brcDig(lic, "usage", "available_quantity"), " available"),
		"600 allocated, 400 available")
	f := s.get(brcMerchandiser, "/api/brant/v1/ful/orders/"+d.order()+"/fulfilment")
	var shipped []string
	for _, l := range brcList(f["lines"]) {
		shipped = append(shipped, fmt.Sprint(brcDig(l, "order_line_id"), " ", brcDig(l, "shipped")))
	}
	s.want("shipped", strings.Join(shipped, ", "), "L1 600, L2 500")
	s.dispatch("at the end of TD-AT13")
}

// Baseline 4.6 S-A (S5-17, S5-18; STL-INV-002/007/010/011, MD-10, R33; baseline 4.5 definitions): 100 PCS
// of ST01.red.S at 10.00 USD = 1,000.00, bought at CNY 40.00 (CNY 4,000.00), analysed in CNY at 7.0 (the
// pricing policy's calculation currency and the quote's sales rate), estimated freight CNY 500.00;
// 20 % on order confirmation, 80 % 30 days after shipment. The order runs end to end (quote CNY
// 7,000 − 4,500 = 2,500 forecast, compliance, credit, commitment in CNY, quality, delivery). Treasury
// records the customer's prepayment, credited as CNY 1,400.00, and applies it to P1 at the settlement
// rate 1/7: USD 200.00, no difference (step 1). shipment.60 and shipment.40 are confirmed and settled
// as USD 600.00 and 400.00, the prepayment moving onto the first (steps 2–3: open 800.00). The
// customer's USD 800.00, credited as CNY 5,500.00 (6.875), pays 400.00 + 400.00 at the settlement rate
// 1/6.875, −50.00 each against the analysis rate (step 4). The purchase statement CNY 4,000.00 is paid;
// provider.1's freight estimate of CNY 500.00 is billed at 650.00, confirmed and allocated by
// quantity 60/40: 390.00 + 260.00 (step 5); the order is closed and its result settles: confirmed CNY
// 2,350.00 = 1,410.00 + 940.00 (4,200 − 2,400 − 390, 2,800 − 1,600 − 260), forecast 2,500.00, business
// settlement FX difference −100.00, receivables 0 (step 7). Unit tests: C9 TestBaseline46SAOrderResult,
// TestSAFreightIsAllocatedByQuantity; C8 TestCrossCurrencyApplicationAtTheSettlementRateS_A.
func TestS5SCSABaseline46OverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "S-A", Title: "基线 4.6 S-A: prepay 200, settle 600 + 400, receive 800 at 6.875, freight 650 → 390 + 260, result CNY 2,350 = 1,410 + 940",
		Design:    []string{"baseline 4.6 S-A", "baseline 4.5", "STL-INV-002", "STL-INV-007", "STL-INV-010", "STL-INV-011", "MD-10", "R33", "D-16"},
		RestsOn:   []string{"C9", "C8", "B2a", "B2b", "D1"},
		UnitTests: []string{"TestBaseline46SAOrderResult (C9)", "TestSAFreightIsAllocatedByQuantity (C9)", "TestCrossCurrencyApplicationAtTheSettlementRateS_A (C8)"},
		Deviations: []string{
			"The plan's \"receive 800 at 6.875 via an FX conversion record\": STL's fx_conversion fund record cannot be applied (receipts only) and a " +
				"same-currency receipt converted later is not attributable to the order's FX difference (C9's rule); so, as C8/C9 do, the bank's " +
				"CNY credits (1,400.00 at 7.0; 5,500.00 at 6.875) are recorded as CNY receipts and applied at the settlement-purpose rates 0.142857142857 " +
				"and 0.145454545455, which gives the baseline's −100.00.",
			"Payment terms 20 % on order confirmation / 80 % 30 days after shipment (C9's; the baseline states only the 200 prepayment).",
			"Net cash is CNY 2,900.00 (receipts 6,900 − purchase payments 4,000), not the baseline's 2,250.00: charges are not paid through STL " +
				"(C9's reported gap); the freight's 650.00 payment is outside S5.",
		}})
	s.dealFoundation(s5scTenant{terms: "20% on order confirmation, 80% 30 days after shipment", stages: []any{
		map[string]any{"stage_id": "P1", "percent": "20", "trigger": polapi.TriggerOrderConfirmed, "days": 0},
		map[string]any{"stage_id": "P2", "percent": "80", "trigger": polapi.TriggerAfterShipment, "days": 30}},
		incoterm: "FCA", place: "TEST NAMED PLACE", calc: "CNY", salesRate: "7.0", purchase: "CNY", skus: []string{"ST01.red.S"}, limit: "10000.00"})
	s.phase = "S-A: CNY account and settlement rates"
	bank := s.config(polapi.KindBankAccount, "bank.cny", map[string]any{"owner_kind": polapi.OwnerCompany, "owner_id": "company.A",
		"bank_name": "synthetic bank", "account_number": "6222000033334444", "currency": "CNY", "purpose": polapi.AccountBoth})
	rate := func(id, value string) map[string]any {
		return s.rateConfig(id, map[string]any{"from_currency": "CNY", "to_currency": "USD", "rate": value, "purpose": polapi.PurposeSettlement,
			"source_name": "SYNTHETIC SETTLEMENT RATE", "observed_at": kernel.FormatTimestamp(s.from)})
	}
	pre, sa := rate("cny.usd.pre", "0.142857142857"), rate("cny.usd.sa", "0.145454545455")

	d := &s5scDeal{id: "sa", lines: []s5scLine{{"L1", "ST01.red.S", "100", "10.00", "40.00"}}, consignee: "cons.de", freight: "500.00"}
	s.form(d)
	s.want("quote", s.figures["sa.quote"], "payable 100000 USD; cost 450000, profit 250000 CNY; margin rule pass; payments P1 20000, P2 80000")
	s.clear(d, false)
	s.reserve(d, "res.sa")
	s.activate(d)
	s.draft(d, nil)
	s.dispatch("after the activation")

	s.phase = "S-A step 1: prepayment"
	s.fundOf("fund.sa.pre", "CNY", bank, "1400.00", "CNY-SA-PRE")
	s.applyOn("app.sa.pre", "fund.sa.pre", s5scStage(d.order(), "P1"), "1400.00", pre)
	a := s.get(brcTreasury, "/api/brant/v1/stl/applications/app.sa.pre")
	s.want("step1_prepayment", fmt.Sprint(a["amount_minor"], " USD from CNY ", brcDig(a, "conversion", "source_amount_minor"), " difference ",
		brcDig(a, "conversion", "fx_difference_minor")), "20000 USD from CNY 140000 difference 0")

	s.commit(d, "purchase.sa")
	s.inspect(d, nil)
	s.deliver(d, "delivery.sa", [][2]string{{"L1", "100"}}, [][2]string{{"L1", "100"}})
	s.ship(d, "shipment.60", [][2]string{{"L1", "60"}})
	s.ship(d, "shipment.40", [][2]string{{"L1", "40"}})

	s.phase = "S-A steps 2–3: two statements"
	s.settle("stmt.600", "shipment.60", 1)
	s.want("step2_statement_600", s.installments("stmt.600"), "P1 20000/20000/0 P2 40000/0/40000")
	s.settle("stmt.400", "shipment.40", 1)
	s.want("step3_statement_400", s.installments("stmt.400"), "P2 40000/0/40000")
	pos := s.get(brcTreasury, "/api/brant/v1/stl/orders/"+d.order()+"/settlement")
	s.want("step3_open", fmt.Sprint("settled ", pos["settled_minor"], " open ", pos["open_minor"]), "settled 100000 open 80000")

	s.phase = "S-A step 4: CNY 5,500.00 for USD 800.00"
	s.fundOf("fund.sa.cny", "CNY", bank, "5500.00", "CNY-SA-800")
	s.applyOn("app.600", "fund.sa.cny", s5scInstallment("stmt.600", "P2"), "2750.00", sa)
	s.applyOn("app.400", "fund.sa.cny", s5scInstallment("stmt.400", "P2"), "2750.00", sa)
	var step4 []string
	for _, id := range []string{"app.600", "app.400"} {
		a := s.get(brcTreasury, "/api/brant/v1/stl/applications/"+id)
		step4 = append(step4, fmt.Sprint(id, " ", a["amount_minor"], " (", brcDig(a, "conversion", "fx_difference_minor"), ")"))
	}
	s.want("step4_applications", strings.Join(step4, ", "), "app.600 40000 (-5000), app.400 40000 (-5000)")
	pos = s.get(brcTreasury, "/api/brant/v1/stl/orders/"+d.order()+"/settlement")
	s.want("step4_open", pos["open_minor"], "0")
	f := s.get(brcTreasury, "/api/brant/v1/stl/fund_records/fund.sa.cny")
	s.want("step4_cny_fund", fmt.Sprint("applied ", f["applied_minor"], " available ", f["available_minor"]), "applied 550000 available 0")

	pay := s.payPurchase(d, "CNY", bank, "4000.00")
	s.want("purchase_statement", fmt.Sprint(pay["amount_minor"], " ", pay["currency"]), "400000 CNY")

	s.phase = "S-A step 5: freight"
	s.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandRecordCharge, map[string]any{"charge_id": "freight.sa", "expected_version": 0,
		"status": "estimated", "source": map[string]any{"kind": "logistics_offer", "party_id": "provider.1", "reference": "log.sa"},
		"beneficiaries": []any{map[string]any{"kind": "order", "order_id": d.order()}}, "fee_category": s.freight, "currency": s.money("CNY"),
		"conversion": map[string]any{"kind": "same_currency"}, "amount": "500.00", "occurred_on": today(), "analysis_on": today(),
		"basis": "provider.1 海运报价 log.sa：预计 CNY 500.00"})
	s.source(brcTreasury, "freight.bill.sa", "合成货代账单 BILL-SA：provider.1 海运费 CNY 650.00，shipment.60 与 shipment.40。")
	r := s.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandRecordCharge, map[string]any{"charge_id": "freight.sa", "expected_version": 1,
		"status": "pending", "source": map[string]any{"kind": "supplier_bill", "party_id": "provider.1", "reference": "BILL-SA"}, "amount": "650.00",
		"occurred_on": today(), "analysis_on": today(), "basis": "货代账单 BILL-SA", "evidence_ids": []string{"freight.bill.sa"}})
	s.want("freight_billed", r["status"], "pending")
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmCharge, map[string]any{"charge_id": "freight.sa", "expected_version": 2,
		"evidence_ids": []string{"freight.bill.sa"}})
	s.cmd(brcCreditOfficer, "stl", stlapi.Contract, stlapi.CommandAllocateCharge, map[string]any{"charge_id": "freight.sa", "expected_version": 3,
		"basis": "quantity", "data_basis": "FUL 实际出运数量 60 / 40", "lines": []any{
			map[string]any{"line_id": "A", "target": map[string]any{"kind": "shipment", "order_id": d.order(), "shipment_id": "shipment.60"}, "weight": "60"},
			map[string]any{"line_id": "B", "target": map[string]any{"kind": "shipment", "order_id": d.order(), "shipment_id": "shipment.40"}, "weight": "40"}}})
	ch := s.get(brcTreasury, "/api/brant/v1/stl/charges/freight.sa")
	s.want("step5_freight", fmt.Sprint("estimate ", ch["estimate_minor"], " amount ", ch["amount_minor"], " → ", brcDig(ch, "allocations", 0, "lines", 0, "amount_minor"),
		" + ", brcDig(ch, "allocations", 0, "lines", 1, "amount_minor")), "estimate 50000 amount 65000 → 39000 + 26000")

	s.closeDeal(d, "receipt."+d.delivery)
	s.dispatch("after the close")
	v := s.settleResult(d)
	confirmed := brcMap(brcDig(v, "live", "confirmed"))
	var batches []string
	for _, b := range brcList(confirmed["batches"]) {
		batches = append(batches, fmt.Sprint(brcDig(b, "shipment_id"), " ", brcDig(b, "net_sales_minor"), "−", brcDig(b, "cost_of_sales_minor"), "−",
			brcDig(b, "direct_charges_minor"), "=", brcDig(b, "result_minor")))
	}
	s.want("step7_result", fmt.Sprint(v["status"], " ", brcDig(v, "live", "currency"), " confirmed ", confirmed["result_minor"], " (", strings.Join(batches, "; "),
		") forecast ", brcDig(v, "live", "forecast", "result_minor"), " fx ", brcDig(v, "live", "fx_difference", "difference_minor")),
		"settled CNY confirmed 235000 (shipment.40 280000−160000−26000=94000; shipment.60 420000−240000−39000=141000) forecast 250000 fx -10000")
	s.want("net_cash", fmt.Sprint("receipts ", brcDig(v, "live", "net_cash", "receipts_minor"), " payments ", brcDig(v, "live", "net_cash", "payments_minor"),
		" net ", brcDig(v, "live", "net_cash", "net_minor")), "receipts 690000 payments 400000 net 290000")
}

// amendmentLines are order.1's lines at the quantities given (BRC-001's L1–L4 at 8.00).
func (s *s5sc) amendmentLines(quantities map[string]string) []any {
	var out []any
	for _, l := range brcLines {
		q := l.qty
		if x, ok := quantities[l.id]; ok {
			q = x
		}
		out = append(out, map[string]any{"line_id": l.id, "sku_ref": map[string]any{"sku_id": l.sku, "spec_revision": 1}, "quantity": q,
			"unit_ref": s.pcs, "unit_price": "8.00", "price_basis_quantity": "1", "discount_ratio": "0", "tax_application_ref": s.sale[l.sku]})
	}
	return out
}

// amendment is order.amendment.prepare's args for order.1 at revision rev and hash, to quantities.
func (s *s5sc) amendment(id string, rev int64, hash string, quantities map[string]string) map[string]any {
	var impact []any
	for _, area := range trdapi.ImpactAreas() {
		a := map[string]any{"area": area, "status": trdapi.ImpactUnaffected}
		switch area {
		case "purchasing":
			a["status"], a["note"] = trdapi.ImpactAffected, "采购承诺 L4 减 12 PCS，按变更单复核"
		case "packing", "booking":
			a["status"], a["note"] = trdapi.ImpactOpen, "待出运的装箱与订舱按新修订复核"
		case "payment_plan":
			a["status"], a["note"] = trdapi.ImpactAffected, "订单金额 960.00 → 864.00，付款节点按新修订复核"
		}
		impact = append(impact, a)
	}
	return map[string]any{"order_id": "order.1", "amendment_id": id, "expected_version": 0, "expected_order_revision": rev, "expected_order_hash": hash,
		"source": map[string]any{"kind": trdapi.SourceCustomerPORevision, "evidence": []any{map[string]any{"source_id": "po.rev2", "locator": "PO 第 2 版"}}},
		"lines":  s.amendmentLines(quantities), "terms": map[string]any{"trade_terms_ref": s.trade, "named_place_ref": s.place,
			"payment_terms_ref": s.payment, "delivery": map[string]any{"kind": trdapi.DeliveryFixed, "date": "2027-01-31", "milestone": trdapi.MilestoneHandover,
				"place_ref": s.place, "time_zone": "Asia/Shanghai"}},
		"impact": impact}
}

// BT10 (baseline 9.3; tactical BT10 "部分已出运后减量或改色 → 展示采购、装箱与付款计划影响并逐项复核；已发生部分保留；处置可追溯";
// WF-06, PL-09, PL-14, D-9, D-18, D-21; TRD-INV-016/018, PUR-INV-011): BRC-001 with its commitment and
// quality releases; supplier.A has delivered L1–L3 (72, verified). shipment.1 of L1 and L2 (60 PCS) is
// confirmed; shipment.2 of L3 (12) is prepared, packed and released. The customer's PO revision
// reduces L4 from 48 to 36: an amendment below what shipped (L1 to 20) is QUANTITY_BELOW_SHIPPED; the
// reduction is prepared and submitted by sales, approved and applied by the order reviewer (960.00 →
// 864.00; the credit portion falls, nothing new to reserve). The relay delivers OrderAmended to
// every consumer, once: PUR opens an allocation review (L4: 48 held by commitment.1, target 36), FUL
// a shipment review (shipment.2, bound to revision 1: its shipment, packing and release items), STL a
// payment plan review (the account moved to revision 2: P1 259.20 with 288.00 applied, P2 604.80) and
// CNX a pending check for revision 2; STL's credit consumer changes nothing. Each responsible role
// settles its review on what it did: the purchaser changes commitment.1 (L4 → 36; the order reviewer
// decides it independently, the purchaser applies it) and settles commitment_changed; compliance
// concludes revision 2 clear; the merchandiser cancels shipment.2, prepares shipment.3 of L3 at
// revision 2 with its packing and release, and settles each item replaced by it; treasury settles
// both stage items accepted with notes. shipment.3 ships; the 60 shipped before stay shipped and the
// reviews show their dispositions. Unit tests: C4b-PUR TestAllocationReviewFollowsAReducedOrderBT10,
// C4b-FUL TestShipmentReviewFollowsAReducedOrderBT10, C4b-STL TestAmendedOrderMovesItsAccountThroughTheRelay,
// C4b-CNX TestOrderAmendedOpensOnePendingCheckPerRevision, C4a TestAmendmentAgainstFULShipmentsTDAT01,
// C11 TestCommitmentChangeIsDecidedIndependentlyAndApplied.
func TestS5SCBT10OrderChangeAfterPartialShipmentOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "BT10", Title: "部分出运后减量 (BT10): PUR, FUL, STL and CNX each review the amendment; what shipped stays",
		Design: []string{"BT10", "WF-06", "PL-09", "PL-14", "D-9", "D-18", "D-21", "TRD-INV-016", "TRD-INV-018", "PUR-INV-011", "SM-PUR-ALLOCATION-REVIEW",
			"SM-FUL-SHIPMENT-REVIEW", "SM-STL-PAYMENT-PLAN-REVIEW"},
		RestsOn: []string{"C4a", "C4b-PUR", "C4b-FUL", "C4b-STL", "C4b-CNX", "C11", "D1"},
		UnitTests: []string{"TestAllocationReviewFollowsAReducedOrderBT10 (C4b-PUR)", "TestShipmentReviewFollowsAReducedOrderBT10 (C4b-FUL)",
			"TestAmendedOrderMovesItsAccountThroughTheRelay (C4b-STL)", "TestOrderAmendedOpensOnePendingCheckPerRevision (C4b-CNX)",
			"TestAmendmentAgainstFULShipmentsTDAT01 (C4a)"},
		Deviations: []string{
			"The reduced line L4 is not yet delivered: a commitment change below a verified delivery is SUPPLIER_DELIVERY_EXCEEDED (C11).",
		},
		Omitted: []string{"the colour change (改色, specification_changed): C4b's unit tests; over HTTP it needs a new SKU spec with its own supply condition and costing"}})
	s.toS5()
	s.commitment()
	s.quality()
	s.phase = "BT10: delivery of L1–L3"
	s.source(brcMerchandiser, "forwarder.receipt", "合成货代进仓单：provider.1 于 CNNGB 收到 supplier.A 交来 ST01 红 S/M、蓝 S 共 72 PCS。")
	s.recordDelivery("delivery.1", [][2]string{{"L1", "24"}, {"L2", "36"}, {"L3", "12"}})
	s.verifyDelivery("delivery.1", [][2]string{{"L1", "24"}, {"L2", "36"}, {"L3", "12"}})
	s.phase = "BT10: shipment.1 shipped, shipment.2 prepared"
	s.shipmentID = "shipment.1"
	if status, out := s.prepareShipment("shipment.1", []s5scShipLine{{"S1", "L1", "24", "delivery.1"}, {"S2", "L2", "36", "delivery.1"}}); status != http.StatusOK {
		t.Fatalf("shipment.1 %d %v", status, out)
	}
	s.evaluate(fulapi.ScopeShipment)
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": "shipment.1", "expected_version": 1,
		"evidence_ids": []string{"forwarder.receipt"}})
	if status, out := s.prepareShipment("shipment.2", []s5scShipLine{{"S1", "L3", "12", "delivery.1"}}); status != http.StatusOK {
		t.Fatalf("shipment.2 %d %v", status, out)
	}
	packing := []any{map[string]any{"cartons": 1, "contents": []any{map[string]any{"line_id": "S1", "quantity_per_carton": "12"}}}}
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandPlanPacking, map[string]any{"shipment_id": "shipment.2", "expected_version": 1,
		"packages": packing, "evidence_ids": []string{"forwarder.receipt"}})
	s.shipmentID = "shipment.2"
	s.evaluate(fulapi.ScopeShipment)

	s.phase = "BT10: the amendment"
	o := s.get(brcSales, "/api/brant/v1/trd/orders/order.1")
	hash := brcStr(o["order_hash"])
	s.source(brcSales, "po.rev2", "合成客户 PO 第 2 版：ST01.blue.M 由 48 PCS 减为 36 PCS，其余不变。")
	s.refused(brcSales, "trd", trdapi.Contract, "order.amendment.prepare", s.amendment("amendment.below", 1, hash, map[string]string{"L1": "20", "L4": "36"}),
		http.StatusUnprocessableEntity, trdapi.CodeQuantityBelowShipped)
	s.want("amend_below_shipped", fmt.Sprint(trdapi.CodeQuantityBelowShipped, " ", brcDig(s.record.Steps[len(s.record.Steps)-1].Facts, "field_errors", 0, "path")),
		trdapi.CodeQuantityBelowShipped+" /args/lines/0/quantity")
	p := s.cmd(brcSales, "trd", trdapi.Contract, "order.amendment.prepare", s.amendment("amendment.1", 1, hash, map[string]string{"L4": "36"}))
	sub := s.cmd(brcSales, "trd", trdapi.Contract, "order.amendment.submit", map[string]any{"order_id": "order.1", "amendment_id": "amendment.1",
		"expected_version": p["row_version"]})
	var amendmentHash string
	for _, a := range brcList(s.get(brcSales, "/api/brant/v1/trd/orders/order.1")["amendments"]) {
		if brcDig(a, "amendment_id") == "amendment.1" {
			amendmentHash = brcStr(brcDig(a, "amendment_hash"))
		}
	}
	dec := s.cmd(brcOrderReviewer, "trd", trdapi.Contract, "order.amendment.decide", map[string]any{"order_id": "order.1", "amendment_id": "amendment.1",
		"expected_version": sub["row_version"], "amendment_hash": amendmentHash, "decision": trdapi.DecisionApprove, "reason": "客户 PO 第 2 版书面确认"})
	s.cmd(brcOrderReviewer, "trd", trdapi.Contract, "order.amendment.apply", map[string]any{"order_id": "order.1", "amendment_id": "amendment.1",
		"expected_version": dec["row_version"]})
	o = s.get(brcSales, "/api/brant/v1/trd/orders/order.1")
	snap := s.get(brcSales, "/api/brant/v1/trd/orders/order.1/snapshot")
	var stages []string
	for _, st := range brcList(snap["stages"]) {
		stages = append(stages, fmt.Sprint(brcDig(st, "stage_id"), " ", brcDig(st, "amount_minor")))
	}
	s.want("order_revision_2", fmt.Sprint(o["order_revision"], " payable ", snap["payable_minor"], " stages ", strings.Join(stages, ", "), " credit portion ",
		snap["credit_portion_minor"]), "2 payable 86400 stages P1 25920, P2 60480 credit portion 60480")
	s.orderHash, s.orderRevision = brcStr(o["order_hash"]), 2

	s.dispatch("after the amendment")
	processed := s.runs[len(s.runs)-1].Processed
	var consumers []string
	for k := range processed {
		if strings.HasSuffix(k, trdapi.EventOrderAmended) {
			consumers = append(consumers, k)
		}
	}
	slices.Sort(consumers)
	s.want("order_amended_consumers", strings.Join(consumers, "; "), "cnx.trade_compliance ← "+trdapi.EventOrderAmended+"; ful.shipment_review ← "+
		trdapi.EventOrderAmended+"; pur.allocation_review ← "+trdapi.EventOrderAmended+"; stl.order_account ← "+trdapi.EventOrderAmended+
		"; stl.order_credit ← "+trdapi.EventOrderAmended)
	s.want("reservation_after_amendment", s.reservation()["status"], stlapi.ReservationConsumed)

	s.phase = "BT10: the reviews"
	pr := brcMap(brcDig(s.get(brcPurchaser, "/api/brant/v1/pur/orders/order.1/allocation_reviews"), "allocation_reviews", 0))
	var purItems []string
	for _, it := range brcList(pr["items"]) {
		purItems = append(purItems, fmt.Sprint(brcDig(it, "item_no"), " ", brcDig(it, "kind"), " ", brcStr(brcDig(it, "order_line_id"))+brcStr(brcDig(it, "commitment_id"))))
	}
	s.want("pur_review", fmt.Sprint(pr["status"], " ", pr["responsible_role"], ": ", strings.Join(purItems, "; ")),
		"open purchaser: 1 allocation L4; 2 commitment commitment.1")
	fr := brcMap(brcDig(s.get(brcMerchandiser, "/api/brant/v1/ful/orders/order.1/shipment_reviews"), "shipment_reviews", 0))
	var fulItems []string
	for _, it := range brcList(fr["items"]) {
		fulItems = append(fulItems, fmt.Sprint(brcDig(it, "item_no"), " ", brcDig(it, "kind"), " ", brcDig(it, "shipment_id")))
	}
	s.want("ful_review", fmt.Sprint(fr["status"], " ", fr["responsible_role"], ": ", strings.Join(fulItems, "; ")),
		"open merchandiser: 1 shipment shipment.2; 2 packing shipment.2; 3 release shipment.2")
	sr := brcMap(brcDig(s.get(brcTreasury, "/api/brant/v1/stl/order_accounts/order.1/payment_plan_reviews"), "reviews", 0))
	var stlItems []string
	for _, it := range brcList(sr["items"]) {
		stlItems = append(stlItems, fmt.Sprint(brcDig(it, "seq"), " ", brcDig(it, "kind"), " ", brcStr(brcDig(it, "stage_id")), " ", brcDig(it, "from_minor"), "→",
			brcDig(it, "to_minor"), " applied ", brcDig(it, "applied_minor")))
	}
	s.want("stl_review", fmt.Sprint(sr["status"], " ", sr["responsible_role"], ": ", strings.Join(stlItems, "; ")),
		"open treasury: 1 stage_over_applied P1 28800→25920 applied 28800; 2 credit_portion_changed  67200→60480 applied 0")
	cc := s.get(brcCompliance, "/api/brant/v1/cnx/orders/order.1/trade_compliance")
	var checks []string
	for _, c := range brcList(cc["checks"]) {
		checks = append(checks, fmt.Sprint("r", brcDig(c, "order_revision"), " ", brcDig(c, "state"), "/", brcDig(c, "reason")))
	}
	slices.Sort(checks)
	s.want("cnx_checks", strings.Join(checks, ", "), "r1 clear/cleared, r2 pending/not_evaluated")

	s.phase = "BT10: PUR changes the commitment"
	s.source(brcPurchaser, "change.l4", "合成供应商确认：supplier.A 同意 ST01.blue.M 由 48 减为 36 PCS（PO-2026-00001 第 2 版）。")
	var changeLines []any
	for i, l := range brcLines {
		q := l.qty
		if l.id == "L4" {
			q = "36"
		}
		changeLines = append(changeLines, map[string]any{"order_line_id": l.id, "quantity": q,
			"source": map[string]any{"kind": purapi.KindSupplierOffer, "id": "offer.A", "revision": 1, "condition_line_id": fmt.Sprintf("OL%d", i+1)}})
	}
	s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandPrepareCommitmentChange, map[string]any{"change_id": "change.1", "commitment_id": "commitment.1",
		"revision": 1, "lines": changeLines, "reason": "订单修订 2：L4 减 12 PCS", "evidence": []any{map[string]any{"source_id": "change.l4", "locator": "确认函"}}})
	cv := s.get(brcPurchaser, "/api/brant/v1/pur/purchase_commitment_changes/change.1")
	var acks []any
	for _, dv := range brcList(cv["deviations"]) {
		acks = append(acks, map[string]any{"order_line_id": brcDig(dv, "order_line_id"), "kind": brcDig(dv, "kind"), "reason": "订单修订所致，已核对（合成）"})
	}
	decide := map[string]any{"change_id": "change.1", "expected_version": 1, "change_hash": cv["change_hash"], "decision": purapi.DecisionApprove}
	if len(acks) > 0 {
		decide["acknowledgements"] = acks
	}
	s.cmd(brcOrderReviewer, "pur", purapi.Contract, purapi.CommandDecideCommitmentChange, decide)
	s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandApplyCommitmentChange, map[string]any{"change_id": "change.1", "expected_version": 2})
	_, lines := s.commitmentView()
	s.want("commitment_after_change", lines, "L1 24/24 L2 36/36 L3 12/12 L4 36/0")
	var purDispositions []any
	for _, it := range brcList(pr["items"]) {
		purDispositions = append(purDispositions, map[string]any{"item_no": brcDig(it, "item_no"), "disposition": purapi.DispositionCommitmentChanged,
			"change_id": "change.1", "reason": "承诺按订单修订 2 变更：L4 36 PCS"})
	}
	r := s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandSettleAllocationReview, map[string]any{"review_id": pr["review_id"],
		"expected_version": pr["row_version"], "items": purDispositions, "reason": "采购影响已处置"})
	s.want("pur_review_settled", r["status"], "settled")
	s.want("allocations_after_change", s.allocationsView(), "L1 24/24 L2 36/36 L3 12/12 L4 36/36")

	s.phase = "BT10: compliance concludes revision 2"
	checkID := ""
	for _, c := range brcList(cc["checks"]) {
		if brcDig(c, "order_revision") == float64(2) {
			checkID = brcStr(brcDig(c, "check_id"))
		}
	}
	if checkID == "" {
		t.Fatalf("no check of revision 2 %v", cc)
	}
	ck := s.get(brcCompliance, "/api/brant/v1/cnx/trade_compliance_checks/"+checkID)
	evidence := []string{"evidence"}
	s.cmd(brcCompliance, "cnx", cnxapi.Contract, cnxapi.CommandEvaluate, map[string]any{"order_id": "order.1", "order_revision": 2,
		"expected_version": ck["row_version"], "regulation_id": "screening", "regulation_revision": 1, "programme_lists": []string{cnProgrammeList, unProgrammeList},
		"code_table_version": codeTable, "destination": map[string]any{"country": syntheticClear, "reason": "客户 PO 载明的卸货国（合成）", "evidence_ids": evidence},
		"address_countries": []any{map[string]any{"party_id": "customer.1", "address_id": "hq", "country": syntheticClear, "evidence_ids": evidence}},
		"end_use":           map[string]any{"statement": "服装零售（合成）", "evidence_ids": evidence}})
	ck = s.get(brcCompliance, "/api/brant/v1/cnx/trade_compliance_checks/"+checkID)
	evals := brcList(ck["evaluations"])
	s.cmd(brcCompliance, "cnx", cnxapi.Contract, cnxapi.CommandDecide, map[string]any{"check_id": checkID, "expected_version": ck["row_version"],
		"evaluation_id": brcDig(evals, len(evals)-1, "evaluation_id"), "conclusion": cnxapi.ConclusionClear, "reason": "修订 2 仅减量，核查完成（合成）",
		"license_required": false, "evidence_ids": evidence})

	s.phase = "BT10: FUL re-plans the stale shipment"
	v := s.get(brcMerchandiser, "/api/brant/v1/ful/shipments/shipment.2")
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandCancelShipment, map[string]any{"shipment_id": "shipment.2", "expected_version": v["row_version"],
		"reason": "订单修订 2：按新修订重新编制出运"})
	status, out := s.prepareShipment("shipment.3", []s5scShipLine{{"S1", "L3", "12", "delivery.1"}})
	if status != http.StatusOK {
		t.Fatalf("shipment.3 %d %v", status, out)
	}
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandPlanPacking, map[string]any{"shipment_id": "shipment.3", "expected_version": 1,
		"packages": packing, "evidence_ids": []string{"forwarder.receipt"}})
	s.shipmentID = "shipment.3"
	r, _ = s.evaluate(fulapi.ScopeShipment)
	s.want("shipment_3_release", r["status"], fulapi.ReleaseGranted)
	var fulDispositions []any
	for _, it := range brcList(fr["items"]) {
		fulDispositions = append(fulDispositions, map[string]any{"item_no": brcDig(it, "item_no"), "disposition": "replaced", "shipment_id": "shipment.3",
			"reason": "按修订 2 重新编制：shipment.3"})
	}
	r = s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandSettleShipmentReview, map[string]any{"review_id": fr["review_id"],
		"expected_version": fr["row_version"], "items": fulDispositions, "reason": "出运影响已处置"})
	s.want("ful_review_settled", r["status"], "settled")

	s.phase = "BT10: STL settles the payment plan review"
	var stlDispositions []any
	for _, it := range brcList(sr["items"]) {
		stlDispositions = append(stlDispositions, map[string]any{"seq": brcDig(it, "seq"), "disposition": "accepted",
			"note": "按修订 2 接受：P1 多收 28.80 留待尾款核销，信用占用按订单结束释放"})
	}
	acc := s.account()
	r = s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandSettleReview, map[string]any{"order_id": "order.1", "order_revision": 2,
		"expected_version": acc["version"], "reason": "付款计划影响已复核", "items": stlDispositions})
	s.want("stl_review_settled", r["status"], "settled")
	acc = s.account()
	s.want("order_account", fmt.Sprint("r", acc["order_revision"], " ", strings.Join(s5scStages(acc, "stages"), "; ")),
		"r2 P1 order_confirmed 25920; P2 after_shipment 60480")

	s.phase = "BT10: shipment.3 ships"
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": "shipment.3", "expected_version": 2,
		"evidence_ids": []string{"forwarder.receipt"}})
	shipped, total := s.shipped()
	s.want("shipped", fmt.Sprintf("%d (L1 %s, L2 %s, L3 %s)", total, shipped["L1"], shipped["L2"], shipped["L3"]), "72 (L1 24, L2 36, L3 12)")
	pr = brcMap(brcDig(s.get(brcPurchaser, "/api/brant/v1/pur/orders/order.1/allocation_reviews"), "allocation_reviews", 0))
	fr = brcMap(brcDig(s.get(brcMerchandiser, "/api/brant/v1/ful/orders/order.1/shipment_reviews"), "shipment_reviews", 0))
	s.want("dispositions_traced", fmt.Sprint(brcDig(pr, "items", 0, "disposition", "disposition"), " r", brcDig(pr, "items", 0, "disposition", "revision"), "; ",
		brcDig(fr, "items", 0, "disposition", "disposition"), " ", brcDig(fr, "items", 0, "disposition", "shipment_id")), "commitment_changed r2; replaced shipment.3")
	s.dispatch("at the end of BT10")
}

// decideException has credit.manager.1 release credit decision id, and credit.manager.2 too when the
// first approval leaves it open (two approvers above the top tier, STL-INV-005).
func (s *s5sc) decideException(id, reason string) map[string]any {
	s.t.Helper()
	d := s.cmd(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandDecideException, map[string]any{"decision_id": id, "expected_version": 1,
		"decision": stlapi.DecisionRelease, "credit_policy": s.creditPolicy, "reason": reason})
	if d["status"] != stlapi.DecisionReleased {
		d = s.cmd(brcCreditManager2, "stl", stlapi.Contract, stlapi.CommandDecideException, map[string]any{"decision_id": id,
			"expected_version": d["row_version"], "decision": stlapi.DecisionRelease, "credit_policy": s.creditPolicy, "reason": reason})
	}
	return d
}

// The SIM-02 variant (S5 plan §5 companions; simulated G0 walkthrough SIM-02 "变更与异常：CIF 汉堡，O/A 60 天，客户改单、
// 工厂短交" steps 4, 6–9; WF-06, ES-09, F-03; FUL-INV-005, PUR-INV-008/011, STL-INV-004; D-9, D-13, D-14,
// D-18, D-19): customer.1 (limit 10,000.00 USD, 7,000.00 open — C10's opening receivable) orders
// 1,000 woven shirts, two styles of 500, CIF Hamburg at 9.20 USD = 9,200.00, open account 60 days
// after shipment. Compliance concludes clear; the reservation of 9,200.00 opens a credit decision
// (CREDIT_LIMIT_EXCEEDED: 7,000.00 + 9,200.00 over 10,000.00) that the credit managers release; the
// order is activated on the reservation and the purchaser drafts 1,000 PCS. The customer reduces
// both styles by 10 % (450 + 450 = 8,280.00): the amendment is applied (credit not increased); the
// relay opens PUR's allocation review (the draft and both lines held 50 above the target), STL's
// payment plan review (credit portion 9,200.00 → 8,280.00) and CNX's pending check of revision 2.
// The purchaser voids the draft (PurchaseDraftVoided), drafts 900 and settles the review
// draft_voided; treasury accepts the portion change; compliance concludes revision 2 clear. The 900
// are committed, inspected and released; the factory delivers 850 (450 + 400), verified, and the
// purchaser's short-receipt change to 850 (order reviewer's decision) makes the commitment fulfilled.
// The merchandiser packs, books and ships 850; doc.1 prepares the CIF commercial invoice (7,820.00)
// and the packing list: issuing without the insurance policy is SHIPMENT_DOCUMENTS_UNRECONCILED at
// /insurance_policy, a policy insuring 8,601.99 is INSURANCE_COVERAGE_INSUFFICIENT (110 % of 7,820.00
// = 8,602.00), the policy of 8,602.00 is recorded and the set issued. Treasury confirms the
// receivable of 7,820.00; the merchandiser closes the remainder of 50 (order.close_remainder,
// OrderRemainderClosed), and STL's consumer releases the reservation: the exposure is the opening
// 7,000.00 plus the receivable 7,820.00. Unit tests: C11 TestCommitmentShortReceiptSIM02, C6
// TestInsuranceOnTheCIFInvoiceSIM02Step8, C4b-PUR TestAllocationReviewFollowsAReducedOrderBT10, C4a
// TestCloseRemainderWaitsForPURCommitment, B6 TestOrderEndReleasesEveryReservationOfTheOrder.
func TestS5SCSIM02VariantOverHTTP(t *testing.T) {
	s := newS5SC(t, s5scMeta{ID: "SIM-02", Title: "SIM-02 变体: CIF Hamburg 1,000 × 9.20, O/A 60, −10 %, insurance 110 %, 850 of 900, close_remainder 50",
		Design: []string{"SIM-02", "WF-06", "ES-09", "F-03", "FUL-INV-005", "PUR-INV-008", "PUR-INV-011", "STL-INV-004", "D-9", "D-13", "D-14",
			"D-18", "D-19"},
		RestsOn: []string{"C6", "C11", "C4a", "C4b-PUR", "C4b-STL", "C4b-CNX", "C10", "A6", "B6", "D1"},
		UnitTests: []string{"TestCommitmentShortReceiptSIM02 (C11)", "TestInsuranceOnTheCIFInvoiceSIM02Step8 (C6)", "TestAllocationReviewFollowsAReducedOrderBT10 (C4b-PUR)",
			"TestAmendedOrderMovesItsAccountThroughTheRelay (C4b-STL)"},
		Deviations: []string{
			"The 10 % reduction is applied before anything is committed, so PUR's disposition is draft_voided and a new draft of 900 (SIM-02 step 7's " +
				"\"作废多余的采购分配\"); the short receipt is C11's commitment change to the 850 verified.",
			"The insurer is a PTY party with a verified insurer service role; the premium comparison with the costing has no insurance component " +
				"(the CIF costing names freight only): FUL records no_component, never a refusal (C6).",
		},
		Omitted: []string{"SIM-02 steps 1–3 (counter-offer by colour, German law: F-02), 5 (screening false positive), 10 (arrival claim and credit: " +
			"CLM is S6, F-05) and 11 (partial payment, overdue next order): not part of the plan's variant"}})
	skus := []string{"WS01.navy.M", "WS02.white.M"}
	s.dealFoundation(s5scTenant{terms: "O/A 60 days after shipment", stages: []any{
		map[string]any{"stage_id": "P1", "percent": "100", "trigger": polapi.TriggerAfterShipment, "days": 60}},
		incoterm: "CIF", place: "HAMBURG", calc: "USD", purchase: "USD", skus: skus, limit: "10000.00"})
	s.openingReceivable("ob.prior", "AR-PRIOR-B2", "7000.00", 20)
	d := &s5scDeal{id: "sim", lines: []s5scLine{{"L1", "WS01.navy.M", "500", "9.20", "6.00"}, {"L2", "WS02.white.M", "500", "9.20", "6.00"}},
		consignee: "cons.de", freight: "400.00"}
	s.form(d)
	s.want("quote", s.figures["sim.quote"], "payable 920000 USD; cost 640000, profit 280000 USD; margin rule pass; payments P1 920000")
	s.clear(d, false)

	s.phase = "SIM-02 step 4: credit decision"
	r := s.reserve(d, "res.sim")
	decision := brcStr(r["object_id"])
	v := s.get(brcSales, "/api/brant/v1/stl/credit_decisions/"+decision)
	s.want("credit_decision", fmt.Sprint(r["status"], " ", v["reasons"], " exposure ", v["exposure_minor"], " portion ", v["portion_minor"], " excess ", v["excess_minor"]),
		stlapi.StatusDecisionOpened+" ["+stlapi.CodeCreditLimitExceeded+"] exposure 700000 portion 920000 excess 620000")
	dec := s.decideException(decision, "B2 历史付款良好，O/A 60 天超额经批准（合成）")
	s.want("credit_exception", dec["status"], stlapi.DecisionReleased)
	r = s.reserve(d, "res.sim.2")
	s.want("reservation", r["status"], stlapi.ReservationReserved)
	o := s.activate(d)
	s.want("activation", fmt.Sprint(o["status"], " ", brcDig(o, "commits", 0, "entries", trdapi.EvidenceCreditBasis)), "active "+stlapi.BasisCreditDecision)
	s.draft(d, nil)
	s.dispatch("after the activation")

	s.phase = "SIM-02 step 7: the customer reduces 10 %"
	o = s.get(brcSales, "/api/brant/v1/trd/orders/"+d.order())
	s.source(brcSales, "po.rev2", "合成客户 PO 第 2 版：两款各减 10%，各 450 PCS。")
	var lines []any
	for _, l := range d.lines {
		lines = append(lines, map[string]any{"line_id": l.id, "sku_ref": map[string]any{"sku_id": l.sku, "spec_revision": 1}, "quantity": "450",
			"unit_ref": s.pcs, "unit_price": "9.20", "price_basis_quantity": "1", "discount_ratio": "0", "tax_application_ref": s.sale[l.sku]})
	}
	var impact []any
	for _, area := range trdapi.ImpactAreas() {
		a := map[string]any{"area": area, "status": trdapi.ImpactUnaffected}
		if area == "purchasing" || area == "payment_plan" {
			a["status"], a["note"] = trdapi.ImpactAffected, "按 900 PCS 复核（合成）"
		}
		impact = append(impact, a)
	}
	p := s.cmd(brcSales, "trd", trdapi.Contract, "order.amendment.prepare", map[string]any{"order_id": d.order(), "amendment_id": "amendment.sim",
		"expected_version": 0, "expected_order_revision": 1, "expected_order_hash": o["order_hash"],
		"source": map[string]any{"kind": trdapi.SourceCustomerPORevision, "evidence": []any{map[string]any{"source_id": "po.rev2", "locator": "PO 第 2 版"}}},
		"lines":  lines, "terms": map[string]any{"trade_terms_ref": s.trade, "named_place_ref": s.place, "payment_terms_ref": s.payment,
			"delivery": map[string]any{"kind": trdapi.DeliveryFixed, "date": "2027-01-31", "milestone": trdapi.MilestoneHandover, "place_ref": s.place,
				"time_zone": "Asia/Shanghai"}}, "impact": impact})
	sub := s.cmd(brcSales, "trd", trdapi.Contract, "order.amendment.submit", map[string]any{"order_id": d.order(), "amendment_id": "amendment.sim",
		"expected_version": p["row_version"]})
	var amendmentHash string
	for _, a := range brcList(s.get(brcSales, "/api/brant/v1/trd/orders/"+d.order())["amendments"]) {
		amendmentHash = brcStr(brcDig(a, "amendment_hash"))
	}
	dd := s.cmd(brcOrderReviewer, "trd", trdapi.Contract, "order.amendment.decide", map[string]any{"order_id": d.order(), "amendment_id": "amendment.sim",
		"expected_version": sub["row_version"], "amendment_hash": amendmentHash, "decision": trdapi.DecisionApprove, "reason": "客户书面确认减量 10%"})
	s.cmd(brcOrderReviewer, "trd", trdapi.Contract, "order.amendment.apply", map[string]any{"order_id": d.order(), "amendment_id": "amendment.sim",
		"expected_version": dd["row_version"]})
	snap := s.get(brcSales, "/api/brant/v1/trd/orders/"+d.order()+"/snapshot")
	s.want("revision_2", fmt.Sprint(brcDig(snap, "binding", "order_revision"), " payable ", snap["payable_minor"], " credit portion ", snap["credit_portion_minor"]),
		"2 payable 828000 credit portion 828000")
	d.orderRevision, d.orderHash = 2, brcStr(brcDig(snap, "binding", "order_hash"))
	s.dispatch("after the amendment")
	pr := brcMap(brcDig(s.get(brcPurchaser, "/api/brant/v1/pur/orders/"+d.order()+"/allocation_reviews"), "allocation_reviews", 0))
	var purItems []string
	for _, it := range brcList(pr["items"]) {
		purItems = append(purItems, fmt.Sprint(brcDig(it, "kind"), " ", brcStr(brcDig(it, "order_line_id"))+brcStr(brcDig(it, "draft_id"))))
	}
	s.want("pur_review", fmt.Sprint(pr["status"], ": ", strings.Join(purItems, "; ")), "open: allocation L1; allocation L2; draft purchase.sim")
	r = s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandVoidDraft, map[string]any{"draft_id": "purchase.sim", "expected_version": 1,
		"reason": "订单减量 10%：按 900 PCS 重新编制"})
	s.want("void_draft", r["status"], "voided")
	var draftLines []any
	for i, l := range d.lines {
		draftLines = append(draftLines, map[string]any{"order_line_id": l.id, "quantity": "450",
			"source": map[string]any{"kind": purapi.KindSupplierOffer, "id": "offer.sim", "revision": 1, "condition_line_id": fmt.Sprintf("OL%d", i+1)}})
	}
	s.cmd(brcPurchaser, "pur", purapi.Contract, "purchase.draft.prepare", map[string]any{"draft_id": "purchase.sim.2", "order_id": d.order(),
		"selection_reason": "订单修订 2：两款各 450 PCS", "lines": draftLines})
	var purDispositions []any
	for _, it := range brcList(pr["items"]) {
		purDispositions = append(purDispositions, map[string]any{"item_no": brcDig(it, "item_no"), "disposition": purapi.DispositionDraftVoided,
			"draft_id": "purchase.sim", "reason": "原 1,000 PCS 草稿作废，按 900 PCS 重新编制 purchase.sim.2"})
	}
	r = s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandSettleAllocationReview, map[string]any{"review_id": pr["review_id"],
		"expected_version": pr["row_version"], "items": purDispositions, "reason": "采购影响已处置"})
	s.want("pur_review_settled", r["status"], "settled")
	s.want("allocations", s.allocationsOf(d), "L1 450/450 L2 450/450")
	sr := brcMap(brcDig(s.get(brcTreasury, "/api/brant/v1/stl/order_accounts/"+d.order()+"/payment_plan_reviews"), "reviews", 0))
	var stlItems, stlDispositions []any
	for _, it := range brcList(sr["items"]) {
		stlItems = append(stlItems, fmt.Sprint(brcDig(it, "kind"), " ", brcDig(it, "from_minor"), "→", brcDig(it, "to_minor")))
		stlDispositions = append(stlDispositions, map[string]any{"seq": brcDig(it, "seq"), "disposition": "accepted", "note": "减量后信用占用随订单结束释放"})
	}
	s.want("stl_review", fmt.Sprint(stlItems), "[credit_portion_changed 920000→828000]")
	acc := s.get(brcTreasury, "/api/brant/v1/stl/order_accounts/"+d.order())
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandSettleReview, map[string]any{"order_id": d.order(), "order_revision": 2,
		"expected_version": acc["version"], "reason": "付款计划影响已复核", "items": stlDispositions})
	s.clear(d, false)
	s.want("exposure_after_amendment", s.exposure(brcCreditOfficer), "1620000")

	s.phase = "SIM-02 step 9: 850 of 900"
	s.commit(d, "purchase.sim.2")
	s.inspect(d, nil)
	s.deliver(d, "delivery.sim", [][2]string{{"L1", "450"}, {"L2", "400"}}, [][2]string{{"L1", "450"}, {"L2", "400"}})
	s.source(brcPurchaser, "short.sim", "合成工厂通知：WS02.white.M 只能交 400 PCS，余 50 PCS 不再生产（短交）。")
	var changeLines []any
	for i, l := range d.lines {
		q := "450"
		if l.id == "L2" {
			q = "400"
		}
		changeLines = append(changeLines, map[string]any{"order_line_id": l.id, "quantity": q,
			"source": map[string]any{"kind": purapi.KindSupplierOffer, "id": "offer.sim", "revision": 1, "condition_line_id": fmt.Sprintf("OL%d", i+1)}})
	}
	s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandPrepareCommitmentChange, map[string]any{"change_id": "change.short", "commitment_id": d.commitment,
		"revision": 1, "lines": changeLines, "reason": "工厂短交 50 PCS：按已核实交付 850 PCS 收货", "evidence": []any{map[string]any{"source_id": "short.sim", "locator": "短交通知"}}})
	cv := s.get(brcPurchaser, "/api/brant/v1/pur/purchase_commitment_changes/change.short")
	decide := map[string]any{"change_id": "change.short", "expected_version": 1, "change_hash": cv["change_hash"], "decision": purapi.DecisionApprove}
	var acks []any
	for _, dv := range brcList(cv["deviations"]) {
		acks = append(acks, map[string]any{"order_line_id": brcDig(dv, "order_line_id"), "kind": brcDig(dv, "kind"), "reason": "短交已核实（合成）"})
	}
	if len(acks) > 0 {
		decide["acknowledgements"] = acks
	}
	s.cmd(brcOrderReviewer, "pur", purapi.Contract, purapi.CommandDecideCommitmentChange, decide)
	s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandApplyCommitmentChange, map[string]any{"change_id": "change.short", "expected_version": 2})
	c := s.get(brcPurchaser, "/api/brant/v1/pur/purchase_commitments/"+d.commitment)
	var cl []string
	for _, l := range brcList(c["lines"]) {
		cl = append(cl, fmt.Sprintf("%v %v/%v", brcDig(l, "order_line_id"), brcDig(l, "purchase_quantity"), brcDig(l, "verified_quantity")))
	}
	s.want("short_receipt", fmt.Sprint("revision ", c["revision"], " fulfilled ", c["fulfilled"], " ", strings.Join(cl, " ")), "revision 2 fulfilled true L1 450/450 L2 400/400")

	s.phase = "SIM-02 step 9: shipment of 850"
	status, out := s.prepareOn(d, "shipment.sim", [][2]string{{"L1", "450"}, {"L2", "400"}})
	if status != http.StatusOK {
		t.Fatalf("shipment %d %v", status, out)
	}
	s.shipmentID = "shipment.sim"
	pkg := func(actual bool) []any {
		var out []any
		from := 1
		for i, n := range []int{45, 40} {
			p := map[string]any{"cartons": n, "contents": []any{map[string]any{"line_id": fmt.Sprintf("S%d", i+1), "quantity_per_carton": "10"}}}
			if actual {
				p["carton_from"], p["gross_weight_kg"], p["net_weight_kg"], p["volume_cbm"] = from, "5.5", "5", "0.04"
			}
			out = append(out, p)
			from += n
		}
		return out
	}
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandPlanPacking, map[string]any{"shipment_id": s.shipmentID, "expected_version": 1,
		"packages": pkg(false), "evidence_ids": []string{"receipt.delivery.sim"}})
	r = s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRecordPacking, map[string]any{"shipment_id": s.shipmentID, "expected_version": 2,
		"packages": pkg(true), "evidence_ids": []string{"receipt.delivery.sim"}})
	version := r["row_version"]
	s.source(brcMerchandiser, "booking.sim", "合成订舱确认：provider.1，CNNGB→DEHAM，1×40HQ（CIF，我方订舱）。")
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRecordBooking, map[string]any{"shipment_id": s.shipmentID, "expected_version": 0,
		"service_role": map[string]any{"party_id": "provider.1", "service_role_id": "role.1", "revision": 1}, "booking_number": "NGB2611SIM",
		"vessel": "SYNTHETIC STAR", "voyage": "027E", "etd": today(), "cut_off": kernel.FormatTimestamp(time.Now().UTC().Truncate(time.Hour)),
		"container_type": "40HQ", "container_count": 1, "port_of_loading": "CNNGB", "port_of_discharge": "DEHAM", "evidence_ids": []string{"booking.sim"}})
	r, e := s.evaluate(fulapi.ScopeShipment)
	s.want("release", fmt.Sprint(r["status"], " ", brcDig(e, "bindings", "payment_condition", "status")), "granted satisfied")
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": s.shipmentID, "expected_version": version,
		"evidence_ids": []string{"receipt.delivery.sim"}})

	s.phase = "SIM-02 step 8: CIF documents and insurance"
	var descriptions []any
	for _, l := range d.lines {
		descriptions = append(descriptions, map[string]any{"sku_id": l.sku, "language": fulapi.LanguageEn, "text": "Cotton woven shirt " + l.sku})
	}
	particulars := map[string]any{"consignee": map[string]any{"name": "customer.1", "address": "合成收货地址\n汉堡港"}, "marks": "B2\nHAMBURG\nC/NO. 1-85",
		"port_of_loading": "CNNGB", "port_of_discharge": "DEHAM", "descriptions": descriptions}
	ci := s.prepareDocument(0, fulapi.DocumentCommercialInvoice, map[string]any{"templates": []any{s.ciEn}, "sequence_ref": s.ciSeq,
		"seller_ref": s.company, "particulars": particulars})
	pl := s.prepareDocument(brcNum(ci["row_version"]), fulapi.DocumentPackingList, map[string]any{"templates": []any{s.plEn}, "sequence_ref": s.plSeq,
		"seller_ref": s.company, "particulars": particulars})
	state, diffs := s.differences()
	s.want("reconciliation_without_policy", state+": "+strings.Join(diffs, "; "), "unreconciled: /insurance_policy <nil>→an insurance policy (CIF) (commercial_invoice)")
	status, out = s.post(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandIssueShipmentDocuments, map[string]any{"document_set_id": s.documentSet,
		"expected_version": pl["row_version"]})
	s.want("issue_without_policy", fmt.Sprint(status, " ", out["code"], " ", brcDig(out, "field_errors", 0, "path")),
		"409 "+fulapi.CodeShipmentDocumentsUnreconciled+" /insurance_policy")
	s.party(brcBuyer, "insurer.1", []string{ptyapi.RoleSupplier}, "evidence")
	s.cmd(brcBuyer, "pty", ptyapi.Contract, "service_role.verify", map[string]any{"id": "insurer.1", "expected_version": 2,
		"service_role_id": "cargo.1", "role": ptyapi.ServiceInsurer, "service_code": "cargo.insurance", "origin": "CNNGB", "destination": "DEHAM",
		"valid_from": kernel.FormatTimestamp(s.from), "valid_until": kernel.FormatTimestamp(s.until), "evidence_ids": []string{"evidence"}})
	s.fileOf(brcDocumentation, "policy.sim", "policy-SYN-CARGO-0001.txt", "合成货运险保单：ICC (A)，保额 8,602.00 USD（发票金额 7,820.00 × 110%）。")
	insurance := func(expected any, amount string) (int, map[string]any) {
		return s.post(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandRecordInsurance, map[string]any{"document_set_id": s.documentSet,
			"shipment_id": s.shipmentID, "expected_version": expected, "insurer": map[string]any{"party_id": "insurer.1", "service_role_id": "cargo.1", "revision": 1},
			"policy_number": "SYN-CARGO-0001", "clauses": []string{"Institute Cargo Clauses (A)"}, "issued_on": today(), "currency": s.usd,
			"insured_amount": amount, "premium": "15.00", "coverage_percent": "110", "file_id": "policy.sim"})
	}
	status, out = insurance(pl["row_version"], "8601.99")
	s.want("insured_8601_99", fmt.Sprint(status, " ", out["code"], " ", brcDig(out, "field_errors", 0, "path")),
		"422 "+fulapi.CodeInsuranceCoverageInsufficient+" /args/insured_amount")
	status, out = insurance(pl["row_version"], "8602.00")
	if status != http.StatusOK {
		t.Fatalf("insurance.record %d %v", status, out)
	}
	cov := s.get(brcDocumentation, "/api/brant/v1/ful/document_sets/"+s.documentSet+"/insurance")
	s.want("insurance_coverage", fmt.Sprint(brcDig(cov, "coverage", "incoterm"), " invoice ", brcDig(cov, "coverage", "invoice_amount"), " × ",
		brcDig(cov, "coverage", "percent"), "% = ", brcDig(cov, "coverage", "required_amount"), "; insured ", brcDig(cov, "coverage", "insured_amount"),
		" sufficient ", brcDig(cov, "coverage", "sufficient")), "CIF invoice 7820.00 × 110% = 8602.00; insured 8602.00 sufficient true")
	s.figure("insurance_costing", fmt.Sprint(brcDig(cov, "versions", 0, "costing", "status")))
	status, out = s.post(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandIssueShipmentDocuments, map[string]any{"document_set_id": s.documentSet,
		"expected_version": out["row_version"]})
	s.want("issue", fmt.Sprint(status, " ", out["status"]), "200 "+fulapi.DocumentIssued)

	s.phase = "SIM-02: receivable and the remainder"
	s.settle("stmt.sim", s.shipmentID, 2)
	got, _ := s.receivable("stmt.sim")
	s.want("receivable", got, "782000 applied 0 open 782000")
	o = s.get(brcMerchandiser, "/api/brant/v1/trd/orders/"+d.order())
	s.source(brcMerchandiser, "remainder.sim", "合成短交关闭记录：WS02.white.M 余 50 PCS 经客户同意不再交付。")
	r = s.cmd(brcMerchandiser, "trd", trdapi.Contract, "order.close_remainder", map[string]any{"order_id": d.order(), "expected_version": o["row_version"],
		"reason": "工厂短交 50 PCS，客户同意短交关闭", "evidence": []any{map[string]any{"source_id": "remainder.sim", "locator": "短交关闭"}}})
	s.want("close_remainder_receipt", r["status"], trdapi.OrderClosed)
	o = s.get(brcMerchandiser, "/api/brant/v1/trd/orders/"+d.order())
	var end []string
	for _, l := range brcList(brcDig(o, "end", "lines")) {
		end = append(end, fmt.Sprint(brcDig(l, "line_id"), " ", brcDig(l, "shipped"), "/", brcDig(l, "ordered"), " remainder ", brcDig(l, "remainder")))
	}
	s.want("close_remainder", fmt.Sprint(o["status"], " ", brcDig(o, "end", "command"), "; ", strings.Join(end, "; ")),
		trdapi.OrderClosed+" order.close_remainder; L1 450/450 remainder 0; L2 400/450 remainder 50")
	s.dispatch("after the remainder's close")
	res := s.get(brcSales, "/api/brant/v1/stl/credit_reservations/res.sim.2")
	s.want("reservation_released", fmt.Sprint(res["status"], " by ", brcDig(res, "released_by", "id")), stlapi.ReservationReleased+" by inbox.stl.order_credit")
	s.want("exposure_at_the_end", s.exposure(brcCreditOfficer), "1482000")
}
