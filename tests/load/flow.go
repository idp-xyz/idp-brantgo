package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// cycle is what one order cycle wrote; read users read it back.
type cycle struct {
	quote, order, draft, reservation, check, file string
}

// pool holds the completed cycles that read users pick from.
type pool struct {
	mu     sync.RWMutex
	cycles []cycle
}

func (p *pool) add(c cycle) {
	p.mu.Lock()
	p.cycles = append(p.cycles, c)
	p.mu.Unlock()
}

func (p *pool) pick(r *rand.Rand) (cycle, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.cycles) == 0 {
		return cycle{}, false
	}
	return p.cycles[r.IntN(len(p.cycles))], true
}

func (p *pool) size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.cycles)
}

// orderCycle runs BRC-001 steps 11–19 (event storming ES-01, with S5's ES-11 compliance and credit
// steps) for one new order on the world's RFQ and cost snapshot, under ids unique to vu and n: the
// customer's PO file and mail; quote draft, freeze, review, activation with its formal document and
// number; the customer's acceptance; order preparation, independent review, CNX's clear
// conclusion, STL's credit reservation, activation; and the purchase draft from supplier.A's offer.
// Every step is a command or read over HTTP as the acting human; a refusal ends the cycle.
func orderCycle(ctx context.Context, c *client, w *world, vu, n int) (cycle, error) {
	id := fmt.Sprintf("%d.%d", vu, n)
	cy := cycle{quote: "q." + id, order: "o." + id, draft: "p." + id, reservation: "cr." + id, file: "po." + id}
	run := &cycleRun{ctx: ctx, c: c}
	po := purchaseOrderText(id)
	run.cmd(actorSales, "evd", contractEVD, "file.register", map[string]any{"id": cy.file, "file_name": "PO-" + id + ".txt",
		"mime_type": "text/plain", "content_base64": base64.StdEncoding.EncodeToString([]byte(po))})

	proof := []any{map[string]any{"source_id": w.setupSource, "locator": "第1段"}}
	var lines []any
	for i, l := range brcLines {
		lines = append(lines, map[string]any{"line_id": l.id, "rfq_line_id": l.id, "sku_ref": map[string]any{"sku_id": l.sku, "spec_revision": 1},
			"quantity": l.qty, "unit_ref": w.pcs, "unit_price": "8.00", "price_basis_quantity": "1", "discount_ratio": "0",
			"tax_application_ref": w.sale[l.sku], "evidence": []any{map[string]any{"source_id": w.setupSource, "locator": fmt.Sprintf("第1段，第%d项", i+1)}}})
	}
	data := map[string]any{"named_place_ref": w.place,
		"delivery": map[string]any{"kind": "fixed", "date": "2027-01-31", "milestone": "handover", "place_ref": w.place,
			"time_zone": "Asia/Shanghai"},
		"issuer_ref":            w.issuer,
		"customer_ref":          map[string]any{"party_id": "customer.1", "revision": w.customerRevision},
		"contact_ref":           map[string]any{"contact_id": "contact.1", "row_version": 2},
		"trading_profile_ref":   map[string]any{"party_id": "customer.1", "revision": 1},
		"customer_address_refs": []any{map[string]any{"party_id": "customer.1", "party_revision": w.customerRevision, "address_id": "hq"}},
		"rfq_ref":               map[string]any{"rfq_id": "rfq.1", "row_version": w.rfqVersion},
		"cost_snapshot_ref":     map[string]any{"snapshot_id": w.snapshotID, "result_hash": w.resultHash},
		"lines":                 lines, "currency_ref": w.usd, "pricing_policy_ref": w.policy, "trade_terms_ref": w.trade,
		"payment_terms_ref": w.payment, "document_template_ref": w.template, "sequence_ref": w.seq,
		"expires_at":     stamp(time.Now().Add(72 * time.Hour)),
		"customer_notes": "客户可见说明：负载测试合成样例，ST01 红/蓝 × S/M。", "evidence": proof}
	run.cmd(actorSales, "trd", contractTRD, "quote.save_draft", map[string]any{"quote_id": cy.quote, "expected_version": 0, "data": data})
	run.cmd(actorSales, "trd", contractTRD, "quote.freeze", map[string]any{"quote_id": cy.quote, "expected_version": 1, "working_revision": 1})
	q := run.get(actorSales, "GET trd.quote", "/api/brant/v1/trd/quotes/"+cy.quote)
	quoteHash := str(dig(q, "revisions", 0, "business_hash"))
	if run.err == nil && dig(q, "revisions", 0, "calculation", "payable_minor") != "96000" {
		run.fail("trd quote.freeze", dig(q, "revisions", 0, "calculation"))
	}
	review := "rv." + id
	run.cmd(actorSales, "trd", contractTRD, "quote.submit_review", map[string]any{"quote_id": cy.quote, "expected_version": 2, "business_revision": 1,
		"review_request_id": review, "target_command": "quote.activate"})
	run.cmd(actorQuoteReviewer, "trd", contractTRD, "quote.review.decide", map[string]any{"quote_id": cy.quote, "expected_version": 3,
		"review_request_id": review, "business_hash": quoteHash, "decision": "approve", "approval_id": "ap." + id,
		"expires_at": stamp(time.Now().Add(24 * time.Hour)), "reason": "内容核对无误：毛利率 31.25% 不低于 25%", "evidence": proof})
	run.cmd(actorSales, "trd", contractTRD, "quote.activate", map[string]any{"quote_id": cy.quote, "expected_version": 4, "business_revision": 1,
		"approval_id": "ap." + id, "expected_approval_effectivity_version": 1})

	mail := "mail." + id
	run.cmd(actorSales, "evd", contractEVD, "source.register", map[string]any{"id": mail, "source_kind": "manual_text",
		"content_text": "合成客户邮件：确认接受报价 " + cy.quote + " 全部条款与全部四行（合计 120 PCS，960.00 USD）。"})
	run.cmd(actorSales, "trd", contractTRD, "quote.response.record", map[string]any{"response_id": "rs." + id, "quote_id": cy.quote,
		"business_revision": 1, "business_hash": quoteHash, "classification": "full_unconditional", "summary": "客户回复：全部接受",
		"source_ref": map[string]any{"source_id": mail, "locator": "response"}, "received_at": stamp(time.Now()),
		"received_at_basis": "channel_server"})
	run.cmd(actorVerifier, "trd", contractTRD, "quote.response.verify", map[string]any{"response_id": "rs." + id, "acceptance_id": "ac." + id,
		"business_revision": 1, "business_hash": quoteHash, "customer_ref": map[string]any{"party_id": "customer.1", "revision": w.customerRevision},
		"contact_ref": map[string]any{"contact_id": "contact.1", "row_version": 2}, "line_ids": lineIDs()})

	run.cmd(actorSales, "trd", contractTRD, "order.prepare", map[string]any{"order_id": cy.order, "quote_id": cy.quote, "business_revision": 1,
		"business_hash": quoteHash, "acceptance_id": "ac." + id})
	o := run.get(actorSales, "GET trd.order", "/api/brant/v1/trd/orders/"+cy.order)
	orderHash := str(o["order_hash"])
	orderReview := "ro." + id
	run.cmd(actorSales, "trd", contractTRD, "order.review.request", map[string]any{"order_id": cy.order, "expected_version": 1, "review_id": orderReview})
	run.cmd(actorOrderReviewer, "trd", contractTRD, "order.review.decide", map[string]any{"order_id": cy.order, "expected_version": 2,
		"review_id": orderReview, "order_hash": orderHash, "decision": "approve", "expires_at": stamp(time.Now().Add(24 * time.Hour)),
		"reason": "订单与已接受的报价一致"})

	evidence := []string{w.evidence}
	e := run.cmd(actorCompliance, "cnx", contractCNX, "trade_compliance.evaluate", map[string]any{"order_id": cy.order, "order_revision": 1,
		"expected_version": 0, "regulation_id": "screening", "regulation_revision": 1, "programme_lists": []string{cnProgrammes, unProgrammes},
		"code_table_version": codeTable,
		"destination":        map[string]any{"country": clearCountry, "reason": "客户 PO 载明的卸货国（合成）", "evidence_ids": evidence},
		"address_countries": []any{map[string]any{"party_id": "customer.1", "address_id": "hq", "country": clearCountry,
			"evidence_ids": evidence}},
		"end_use": map[string]any{"statement": "服装零售（合成）", "evidence_ids": evidence}})
	cy.check = str(e["object_id"])
	view := run.get(actorCompliance, "GET cnx.trade_compliance_check", "/api/brant/v1/cnx/trade_compliance_checks/"+cy.check)
	run.cmd(actorCompliance, "cnx", contractCNX, "trade_compliance.decide", map[string]any{"check_id": cy.check, "expected_version": 1,
		"evaluation_id": dig(view, "evaluations", 0, "evaluation_id"), "conclusion": "clear",
		"reason": "目的国、交易方所在国与商品核查完成（合成）", "license_required": false, "evidence_ids": evidence})
	run.cmd(actorSales, "stl", contractSTL, "credit.reserve", map[string]any{"reservation_id": cy.reservation, "order_id": cy.order,
		"order_revision": 1, "order_hash": orderHash, "credit_policy": w.credit})
	check := run.get(actorSales, "GET trd.order.check", "/api/brant/v1/trd/orders/"+cy.order+"/check?review_id="+orderReview)
	if run.err == nil && check["activatable"] != true {
		run.fail("trd order.check", check)
	}
	run.cmd(actorSales, "trd", contractTRD, "order.activate", map[string]any{"order_id": cy.order, "expected_version": 3, "review_id": orderReview})

	var purchase []any
	for i, l := range brcLines {
		purchase = append(purchase, map[string]any{"order_line_id": l.id, "quantity": l.qty,
			"source": map[string]any{"kind": "supplier_offer", "id": "offer.A", "revision": 1, "condition_line_id": fmt.Sprintf("OL%d", i+1)}})
	}
	run.cmd(actorPurchaser, "pur", contractPUR, "purchase.draft.prepare", map[string]any{"draft_id": cy.draft, "order_id": cy.order,
		"selection_reason": "按成本快照选定 supplier.A 报价：5.00 USD/PCS，MOQ 100 PCS 按 ST01 款号合计", "lines": purchase})
	return cy, run.result()
}

// cycleRun stops a cycle at its first refusal.
type cycleRun struct {
	ctx  context.Context
	c    *client
	err  error
	step string
}

func (r *cycleRun) cmd(actor, bc, contract, command string, args map[string]any) map[string]any {
	if r.err != nil {
		return map[string]any{}
	}
	out, err := r.c.command(r.ctx, classCommand, actor, bc, contract, command, args)
	if err != nil {
		r.err, r.step = err, bc+" "+command
		return map[string]any{}
	}
	return out
}

func (r *cycleRun) get(actor, name, path string) map[string]any {
	if r.err != nil {
		return map[string]any{}
	}
	out, err := r.c.get(r.ctx, classRead, name, actor, path)
	if err != nil {
		r.err, r.step = err, name
		return map[string]any{}
	}
	return out
}

func (r *cycleRun) fail(step string, got any) {
	if r.err == nil {
		r.err, r.step = fmt.Errorf("unexpected answer %v", got), step
	}
}

func (r *cycleRun) result() error {
	if r.err == nil {
		return nil
	}
	return &stepError{step: r.step, err: r.err}
}

type stepError struct {
	step string
	err  error
}

func (e *stepError) Error() string { return e.step + ": " + e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// purchaseOrderText is the customer's synthetic purchase order, about 3 KiB of text.
func purchaseOrderText(id string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SYNTHETIC PURCHASE ORDER %s (load test data, not a real transaction)\n", id)
	for i := 0; i < 40; i++ {
		l := brcLines[i%len(brcLines)]
		fmt.Fprintf(&b, "%03d %s %s PCS at 8.00 USD, delivery FCA 2027-01-31, payment 30%% on confirmation, 70%% 30 days after shipment\n",
			i+1, l.sku, l.qty)
	}
	return b.String()
}

// readOnce issues one ordinary read, chosen uniformly among the routes a clerk opens around an
// order: the quote, its active revision, formal document and history; the order; the purchase
// draft and the order's allocations; the PO file; the credit reservation; the trade compliance
// check; and the shared master data (customer, RFQ, payment terms, cost snapshot).
func readOnce(ctx context.Context, c *client, w *world, p *pool, r *rand.Rand) error {
	cy, ok := p.pick(r)
	if !ok {
		return nil
	}
	type route struct{ name, actor, path string }
	routes := []route{
		{"GET trd.quote", actorSales, "/api/brant/v1/trd/quotes/" + cy.quote},
		{"GET trd.quote.active", actorSales, "/api/brant/v1/trd/quotes/" + cy.quote + "/active"},
		{"GET trd.quote.document", actorSales, "/api/brant/v1/trd/quotes/" + cy.quote + "/revisions/1/document"},
		{"GET trd.quote.history", actorSales, "/api/brant/v1/trd/quotes/" + cy.quote + "/history?limit=20"},
		{"GET trd.order", actorSales, "/api/brant/v1/trd/orders/" + cy.order},
		{"GET pur.purchase_draft", actorPurchaser, "/api/brant/v1/pur/purchase_drafts/" + cy.draft},
		{"GET pur.demand_allocation", actorPurchaser, "/api/brant/v1/pur/orders/" + cy.order + "/allocations"},
		{"GET evd.file", actorSales, "/api/brant/v1/evd/files/" + cy.file},
		{"GET stl.credit_reservation", actorSales, "/api/brant/v1/stl/credit_reservations/" + cy.reservation},
		{"GET cnx.trade_compliance_check", actorCompliance, "/api/brant/v1/cnx/trade_compliance_checks/" + cy.check},
		{"GET pty.party", actorSales, "/api/brant/v1/pty/parties/customer.1"},
		{"GET inq.rfq", actorSales, "/api/brant/v1/inq/rfqs/rfq.1"},
		{"GET pol.config", actorSales, "/api/brant/v1/pol/configs/payment_terms/payment.30-70"},
		{"GET cst.cost_snapshot", actorSales, "/api/brant/v1/cst/cost_snapshots/" + w.snapshotID},
	}
	rt := routes[r.IntN(len(routes))]
	_, err := c.get(ctx, classRead, rt.name, rt.actor, rt.path)
	return err
}
