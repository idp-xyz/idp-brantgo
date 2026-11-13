//go:build integration

package bootstrap_test

import (
	"fmt"
	"strings"
	"time"

	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CN-AT03's variant of BRC-001 (S5 unit D2a): sales line L4 (ST01.blue.M, 48 PCS) is sold, bought,
// inspected, delivered and shipped as 4 DOZEN, so its export lot line is in another unit than the
// declaration profile's base unit (PCS). Everything else is the S3 chain's and D1's (the same
// humans, IDs, 960.00 USD quote, 600.00 USD purchase): POL publishes the unit DOZEN and the
// controlled conversion of ST01.blue.M r1 (1 DOZEN = 12 PCS, with its professional basis); supplier.A
// and supplier.B offer L4 per DOZEN (60.00 / 63.60 USD) citing it; the costing buys 4 DOZEN (660.00 /
// 696.00 USD as before); the quote sells 4 DOZEN at 96.00 with the conversion (base quantity 48 PCS,
// TRD's comparison basis); PUR drafts and commits 4 DOZEN (the purchase unit is the order line's).
// The S3 steps are adapted here, not changed in their file (D1's helpers stay as they are).

const cnatDozenConversion = "dozen.ST01.blue.M"

// cnatDozens is the quantity of order line l in its sales unit: L4 in DOZEN, the others in PCS.
func cnatDozens(l string) string {
	if l == "L4" {
		return "4"
	}
	for _, x := range brcLines {
		if x.id == l {
			return x.qty
		}
	}
	return ""
}

// toDozenOrder is the S3 chain with L4 in DOZEN, to the active order and its purchase draft.
func (c *cnat) toDozenOrder() (dozen, conversion map[string]any) {
	c.foundation()
	c.front()
	c.phase = "POL unit DOZEN and its controlled conversion"
	dozen = c.config(polapi.KindUnit, "DOZEN", map[string]any{"code": "DOZEN", "name": "打", "dimension": polapi.DimensionCount, "precision": 0})
	conversion = c.config(polapi.KindUnitConversion, cnatDozenConversion, map[string]any{"sku_id": "ST01.blue.M", "spec_revision": 1, "from_unit": "DOZEN",
		"to_unit": "PCS", "factor": "12", "basis": "合成包装资料与专业复核：ST01 蓝 M 每打 12 件"})
	c.customerReady()
	c.dozenSourcing(dozen, conversion)
	c.dozenCosting(dozen)
	c.dozenQuote(dozen, conversion)
	c.acceptance()
	c.order()
	c.dozenPurchase()
	return dozen, conversion
}

// dozenOfferLines are brc001's condition lines at price with an MOQ of one per line, and L4 bought
// and priced per DOZEN (12 × price) citing the conversion.
func (c *cnat) dozenOfferLines(price string, dozen, conversion map[string]any) []any {
	perDozen := map[string]string{"5.00": "60.00", "5.30": "63.60"}[price]
	lines := c.offerLines(price)
	for i, l := range brcLines {
		line := lines[i].(map[string]any)
		line["moq"] = map[string]any{"quantity": "1", "unit_ref": c.pcs, "scope": supapi.MOQScopeSKU}
		if l.id == "L4" {
			line["purchase_unit_ref"], line["price_unit_ref"], line["tier_unit_ref"] = dozen, dozen, dozen
			line["tiers"] = []any{map[string]any{"lower_inclusive": "0", "unit_price": perDozen}}
			line["moq"] = map[string]any{"quantity": "1", "unit_ref": dozen, "scope": supapi.MOQScopeSKU}
			line["conversion_refs"] = []any{conversion}
		}
	}
	return lines
}

// dozenSourcing is brc001.sourcing with L4 offered per DOZEN.
func (c *cnat) dozenSourcing(dozen, conversion map[string]any) {
	c.phase = "SUP supply conditions (L4 per DOZEN)"
	c.source(brcBuyer, "supplier.quotes", "合成供应商报价：supplier.A 5.00 USD/PCS（蓝 M 60.00 USD/打），supplier.B 5.30 USD/PCS（蓝 M 63.60 USD/打）；"+
		"provider.1 海运 CNSHA→DEHAM 60.00 USD。")
	for _, s := range []struct{ supplier, request, offer, price string }{{"supplier.A", "request.A", "offer.A", "5.00"},
		{"supplier.B", "request.B", "offer.B", "5.30"}} {
		c.party(brcBuyer, s.supplier, []string{ptyapi.RoleSupplier}, "evidence")
		supplier := map[string]any{"party_id": s.supplier, "row_version": 2}
		rfq := map[string]any{"rfq_id": "rfq.1", "row_version": c.rfqVersion}
		c.cmd(brcBuyer, "sup", supapi.Contract, "supplier_rfq.save_draft", map[string]any{"id": s.request, "expected_version": 0,
			"data": map[string]any{"supplier": supplier, "rfq": rfq, "rfq_line_ids": brcLineIDs(),
				"evidence": []any{map[string]any{"source_id": "source.1", "locator": "第1段"}}}})
		issued := c.cmd(brcBuyer, "sup", supapi.Contract, "supplier_rfq.record_issued", map[string]any{"id": s.request, "expected_version": 1,
			"evidence": []any{map[string]any{"source_id": "evidence", "locator": "询价发送记录 " + s.request}}, "reason": "人工发出供应商询价"})
		c.cmd(brcBuyer, "sup", supapi.Contract, "supplier_offer.save_draft", map[string]any{"id": s.offer, "expected_version": 0,
			"data": map[string]any{"supplier": supplier, "rfq": rfq, "rfq_line_ids": brcLineIDs(),
				"origin": map[string]any{"kind": supapi.OriginInResponseToRfq, "supplier_rfq": map[string]any{"supplier_rfq_id": s.request,
					"row_version": issued["row_version"]}},
				"valid_from": kernel.FormatTimestamp(c.from), "valid_until": kernel.FormatTimestamp(c.until), "lines": c.dozenOfferLines(s.price, dozen, conversion),
				"evidence": []any{map[string]any{"source_id": "supplier.quotes", "locator": s.supplier + " 报价"}}}})
		c.cmd(brcCostChecker, "sup", supapi.Contract, "supplier_offer.verify", map[string]any{"id": s.offer, "expected_version": 1,
			"working_revision": 1, "evidence": []any{map[string]any{"source_id": "evidence", "locator": "核实依据 " + s.offer}}})
		if offer := c.get(brcBuyer, "/api/brant/v1/sup/supplier_offers/"+s.offer); offer["active_revision"] != float64(1) {
			c.t.Fatalf("offer %v", offer)
		}
	}
	c.party(brcBuyer, "provider.1", []string{ptyapi.RoleSupplier}, "evidence")
	c.cmd(brcBuyer, "pty", ptyapi.Contract, "service_role.verify", map[string]any{"id": "provider.1", "expected_version": 2,
		"service_role_id": "role.1", "role": ptyapi.ServiceForwarder, "service_code": "sea.fcl", "origin": "CNSHA", "destination": "DEHAM",
		"valid_from": kernel.FormatTimestamp(c.from), "valid_until": kernel.FormatTimestamp(c.until), "evidence_ids": []string{"evidence"}})
	c.cmd(brcBuyer, "sup", supapi.Contract, "logistics_offer.save_draft", map[string]any{"id": "log.1", "expected_version": 0,
		"data": map[string]any{"service_role_ref": map[string]any{"party_id": "provider.1", "service_role_id": "role.1", "revision": 1},
			"service_code": "sea.fcl", "mode": supapi.ModeSea, "origin": "CNSHA", "destination": "DEHAM",
			"valid_from": kernel.FormatTimestamp(c.from), "valid_until": kernel.FormatTimestamp(c.until),
			"fees": []any{map[string]any{"id": "F1", "obligation_id": "freight.1", "fee_type_ref": c.freight, "responsible_party": supapi.PartySeller,
				"currency_ref": c.usd, "tax_profile_ref": c.zero, "tax_application_ref": c.service,
				"billing": map[string]any{"kind": supapi.BillingFixed, "amount": "60.00"}, "chargeable": map[string]any{"kind": supapi.ChargeableFixed},
				"allocation": map[string]any{"basis": supapi.AllocationQuantity, "target_line_ids": brcLineIDs()}}},
			"evidence": []any{map[string]any{"source_id": "supplier.quotes", "locator": "provider.1 海运费"}}}})
	c.cmd(brcCostChecker, "sup", supapi.Contract, "logistics_offer.verify", map[string]any{"id": "log.1", "expected_version": 1,
		"working_revision": 1, "evidence": []any{map[string]any{"source_id": "evidence", "locator": "核实依据 log.1"}}})
}

// dozenScenario is brc001.scenario buying L4 as 4 DOZEN.
func (c *cnat) dozenScenario(offer, reason string, dozen map[string]any) map[string]any {
	s := c.scenario(offer, reason)
	a := s["allocations"].([]any)[3].(map[string]any)
	a["purchase_quantity"], a["purchase_unit_ref"] = "4", dozen
	return s
}

// dozenCosting is brc001.costing with L4 bought in DOZEN: 660.00 and 696.00 USD as before.
func (c *cnat) dozenCosting(dozen map[string]any) {
	c.phase = "CST costing (L4 in DOZEN)"
	var lines []any
	for _, l := range brcLines {
		lines = append(lines, map[string]any{"line_id": l.id, "sku_id": l.sku, "sku_spec_revision": 1, "quantity": l.qty, "unit_ref": c.pcs})
	}
	c.cmd(brcBuyer, "cst", cstapi.Contract, "costing_context.save_draft", map[string]any{"id": "context.1", "expected_version": 0,
		"data": map[string]any{"rfq_ref": map[string]any{"id": "rfq.1", "row_version": c.rfqVersion}, "context_kind": cstapi.ContextActual,
			"lines": lines, "calculation_currency_ref": c.usd, "pricing_policy_ref": c.policy,
			"comparison_basis": map[string]any{"specification_basis": "TECH-01-r1", "named_place_ref": c.place,
				"required_fee_type_refs": []any{c.freight}, "tax_basis": "synthetic_zero_nonrecoverable", "cost_basis": cstapi.CostBasisNominalDirect,
				"sales_trade_terms_ref": c.trade, "sales_payment_terms_ref": c.payment,
				"delivery": map[string]any{"kind": cstapi.DeliveryFixed, "date": "2027-01-31", "milestone": cstapi.MilestoneHandover,
					"place_ref": c.place, "time_zone": "Asia/Shanghai"}}}})
	c.cmd(brcCostChecker, "cst", cstapi.Contract, "costing_context.verify", map[string]any{"id": "context.1", "expected_version": 1,
		"working_revision": 1, "evidence": []any{map[string]any{"source_id": "source.1", "locator": "第1段：四个SKU合计120 PCS"}}})
	evaluations := map[string]map[string]any{}
	for _, s := range []struct{ scenario, offer, total string }{{"scenario.A", "offer.A", "66000"}, {"scenario.B", "offer.B", "69600"}} {
		c.cmd(brcBuyer, "cst", cstapi.Contract, "cost_scenario.save_draft", map[string]any{"id": s.scenario, "expected_version": 0,
			"data": c.dozenScenario(s.offer, "同规格同条款："+s.offer+" 全部四行（蓝 M 按打），含 log.1 海运费", dozen)})
		r := c.cmd(brcCostChecker, "cst", cstapi.Contract, "cost_scenario.evaluate", map[string]any{"id": s.scenario, "expected_version": 1})
		e := c.get(brcCostChecker, "/api/brant/v1/cst/cost_evaluations/"+brcStr(r["object_id"]))
		if brcDig(e, "result", "total_minor") != s.total {
			c.t.Fatalf("evaluation of %s: %v", s.scenario, e["result"])
		}
		evaluations[s.scenario] = e
	}
	a := evaluations["scenario.A"]
	f := c.cmd(brcCostChecker, "cst", cstapi.Contract, "cost_snapshot.freeze", map[string]any{"scenario_id": "scenario.A", "expected_version": 1,
		"evaluation_id": a["evaluation_id"], "result_hash": a["result_hash"],
		"selection_reason": "同口径成本已核实：supplier.A 660.00 USD 低于 supplier.B 696.00 USD"})
	c.snapshotID = brcStr(f["object_id"])
	snap := c.get(brcCostChecker, "/api/brant/v1/cst/cost_snapshots/"+c.snapshotID)
	if snap["total_minor"] != "66000" {
		c.t.Fatalf("snapshot %v", snap)
	}
	c.resultHash = brcStr(snap["result_hash"])
}

// dozenQuote is brc001.quote with L4 sold as 4 DOZEN at 96.00 USD per DOZEN through the conversion
// (base quantity 48 PCS): 960.00 USD, profit 300.00, payments 288.00 + 672.00 as before.
func (c *cnat) dozenQuote(dozen, conversion map[string]any) {
	c.phase = "TRD quote (L4 in DOZEN)"
	var lines []any
	for i, l := range brcLines {
		line := map[string]any{"line_id": l.id, "rfq_line_id": l.id, "sku_ref": map[string]any{"sku_id": l.sku, "spec_revision": 1},
			"quantity": l.qty, "unit_ref": c.pcs, "unit_price": "8.00", "price_basis_quantity": "1", "discount_ratio": "0",
			"tax_application_ref": c.sale[l.sku], "evidence": []any{map[string]any{"source_id": "source.1", "locator": fmt.Sprintf("第1段，第%d项", i+1)}}}
		if l.id == "L4" {
			line["quantity"], line["unit_ref"], line["unit_price"], line["conversion_ref"], line["base_quantity"] = "4", dozen, "96.00", conversion, "48"
		}
		lines = append(lines, line)
	}
	data := map[string]any{"named_place_ref": c.place,
		"delivery": map[string]any{"kind": trdapi.DeliveryFixed, "date": "2027-01-31", "milestone": trdapi.MilestoneHandover, "place_ref": c.place,
			"time_zone": "Asia/Shanghai"},
		"issuer_ref": c.company, "customer_ref": map[string]any{"party_id": "customer.1", "revision": c.customerRevision},
		"contact_ref":           map[string]any{"contact_id": "contact.1", "row_version": 2},
		"trading_profile_ref":   map[string]any{"party_id": "customer.1", "revision": 1},
		"customer_address_refs": []any{map[string]any{"party_id": "customer.1", "party_revision": c.customerRevision, "address_id": "hq"}},
		"rfq_ref":               map[string]any{"rfq_id": "rfq.1", "row_version": c.rfqVersion},
		"cost_snapshot_ref":     map[string]any{"snapshot_id": c.snapshotID, "result_hash": c.resultHash},
		"lines":                 lines, "currency_ref": c.usd, "pricing_policy_ref": c.policy, "trade_terms_ref": c.trade,
		"payment_terms_ref": c.payment, "document_template_ref": c.template, "sequence_ref": c.seq,
		"expires_at":     kernel.FormatTimestamp(time.Now().Add(72 * time.Hour)),
		"customer_notes": "客户可见说明：BRC-001 合成样例，ST01 红/蓝 × S/M，蓝 M 按打（12 件/打）。",
		"evidence":       []any{map[string]any{"source_id": "source.1", "locator": "第1段"}}}
	const trd = trdapi.Contract
	c.cmd(brcSales, "trd", trd, "quote.save_draft", map[string]any{"quote_id": "quote.1", "expected_version": 0, "data": data})
	c.cmd(brcSales, "trd", trd, "quote.freeze", map[string]any{"quote_id": "quote.1", "expected_version": 1, "working_revision": 1})
	q := c.get(brcSales, "/api/brant/v1/trd/quotes/quote.1")
	rev := brcMap(brcDig(q, "revisions", 0))
	c.quoteHash = brcStr(rev["business_hash"])
	calc := brcMap(rev["calculation"])
	if calc["net_minor"] != "96000" || calc["cost_minor"] != "66000" || calc["profit_minor"] != "30000" || brcDig(calc, "payments", 1, "amount_minor") != "67200" {
		c.t.Fatalf("frozen calculation %v", calc)
	}
	c.record.Amounts["quote_profit"] = brcMajor(c.t, brcStr(calc["profit_minor"])) + " USD"
	c.cmd(brcSales, "trd", trd, "quote.submit_review", map[string]any{"quote_id": "quote.1", "expected_version": 2, "business_revision": 1,
		"review_request_id": "review.quote.1", "target_command": trdapi.TargetActivate})
	c.cmd(brcQuoteReviewer, "trd", trd, "quote.review.decide", map[string]any{"quote_id": "quote.1", "expected_version": 3, "review_request_id": "review.quote.1",
		"business_hash": c.quoteHash, "decision": trdapi.DecisionApprove, "approval_id": "approval.quote.1",
		"expires_at": kernel.FormatTimestamp(time.Now().Add(24 * time.Hour)), "reason": "内容核对无误：毛利率 31.25% 不低于 25%",
		"evidence": []any{map[string]any{"source_id": "source.1", "locator": "第1段"}}})
	c.cmd(brcSales, "trd", trd, "quote.activate", map[string]any{"quote_id": "quote.1", "expected_version": 4, "business_revision": 1,
		"approval_id": "approval.quote.1", "expected_approval_effectivity_version": 1})
}

// dozenPurchase is brc001.purchase with L4 drafted as 4 DOZEN from supplier.A's per-DOZEN line.
func (c *cnat) dozenPurchase() {
	c.phase = "PUR purchase draft (L4 in DOZEN)"
	var lines []any
	for i, l := range brcLines {
		lines = append(lines, map[string]any{"order_line_id": l.id, "quantity": cnatDozens(l.id),
			"source": map[string]any{"kind": purapi.KindSupplierOffer, "id": "offer.A", "revision": 1, "condition_line_id": fmt.Sprintf("OL%d", i+1)}})
	}
	r := c.cmd(brcPurchaser, "pur", purapi.Contract, "purchase.draft.prepare", map[string]any{"draft_id": "purchase.1", "order_id": "order.1",
		"selection_reason": "按成本快照选定 supplier.A 报价：5.00 USD/PCS，蓝 M 60.00 USD/打", "lines": lines})
	if r["status"] != purapi.StatusPrepared {
		c.t.Fatalf("purchase.draft.prepare %v", r)
	}
	if d := c.get(brcPurchaser, "/api/brant/v1/pur/purchase_drafts/purchase.1"); d["net_minor"] != "60000" {
		c.t.Fatalf("draft %v", d)
	}
}

// dozenQuality is D1's step 9 with L4 inspected and released as 4 DOZEN.
func (c *cnat) dozenQuality() {
	c.step(9, "QLT inspection and quality release (L4 in DOZEN)")
	c.party(brcBuyer, "agency.1", []string{ptyapi.RoleSupplier}, "evidence")
	c.cmd(brcBuyer, "pty", ptyapi.Contract, "service_role.verify", map[string]any{"id": "agency.1", "expected_version": 2,
		"service_role_id": "inspection.1", "role": ptyapi.ServiceInspection, "service_code": "final.inspection", "origin": "CNNGB", "destination": "CNNGB",
		"valid_from": kernel.FormatTimestamp(c.from), "valid_until": kernel.FormatTimestamp(c.until), "evidence_ids": []string{"evidence"}})
	c.source(brcQC, "standard", "合成检验标准：AQL 2.5，一般检验水平 II，ST01 规格书 TECH-01-r1。")
	c.fileOf(brcQC, "report.final", "final-inspection-ST01.txt", "合成第三方终检报告（agency.1）：ST01 红/蓝 S/M 72 PCS 与蓝 M 4 打全检，全部合格。")
	for _, l := range brcLines {
		q := cnatDozens(l.id)
		subject := map[string]any{"kind": qltapi.SubjectCommitmentLine, "id": "commitment.1", "line_id": l.id}
		c.cmd(brcQC, "qlt", qltapi.Contract, qltapi.CommandRecordInspection, map[string]any{"inspection_id": "insp." + l.id, "subject": subject,
			"inspected_quantity": q, "conforming_quantity": q, "nonconforming_quantity": "0", "sampling": map[string]any{"method": qltapi.SamplingFull},
			"standard": map[string]any{"source_id": "standard"}, "inspector": map[string]any{"kind": qltapi.InspectorAgency,
				"agency": map[string]any{"party_id": "agency.1", "service_role_id": "inspection.1", "revision": 1}},
			"report_file_id": "report.final", "inspected_at": now()})
		c.cmd(brcQC, "qlt", qltapi.Contract, qltapi.CommandGrantRelease, map[string]any{"release_id": "rel." + l.id, "subject": subject,
			"quantity": q, "valid_until": kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))})
	}
}

// dozenDelivery is D1's step 11 with L4 delivered and verified as 4 DOZEN.
func (c *cnat) dozenDelivery() {
	c.step(11, "PUR supplier delivery (L4 in DOZEN)")
	c.source(brcMerchandiser, "forwarder.receipt", "合成货代进仓单：provider.1 于 CNNGB 收到 supplier.A 交来 ST01 72 PCS 与蓝 M 4 打。")
	var lines, verified []any
	for _, l := range brcLines {
		lines = append(lines, map[string]any{"order_line_id": l.id, "quantity": cnatDozens(l.id)})
		verified = append(verified, map[string]any{"order_line_id": l.id, "verified_quantity": cnatDozens(l.id)})
	}
	c.cmd(brcMerchandiser, "pur", purapi.Contract, purapi.CommandRecordSupplierDelivery, map[string]any{"delivery_id": "delivery.1", "expected_version": 0,
		"commitment_id": "commitment.1", "lines": lines, "handover_place": "CNNGB", "handed_over_at": now(),
		"evidence": []any{map[string]any{"source_id": "forwarder.receipt", "locator": "进仓单"}}, "supplier_reference": "SA-DN-001"})
	if r := c.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandVerifySupplierDelivery, map[string]any{"delivery_id": "delivery.1",
		"expected_version": 1, "decision": "verify", "lines": verified, "evidence": []any{map[string]any{"source_id": "forwarder.receipt", "locator": "进仓单"}}}); r["status"] != purapi.DeliveryVerified {
		c.t.Fatalf("verify %v", r)
	}
}

// toDozenLot is CN-AT03's chain to export lot lot.1 of shipment.1 (L1–L3 in PCS, L4 4 DOZEN).
func (c *cnat) toDozenLot() (dozen, conversion map[string]any) {
	dozen, conversion = c.toDozenOrder()
	c.ev.Chain = "BRC-001 with L4 in DOZEN: the S3 chain adapted (cnat_dozen_integration_test.go) and D1 §5 steps 1–14 and 17 adapted to export lot lot.1"
	c.configurations()
	c.dispatch("after the activation (CN-AT03 chain)")
	c.activationConsumed()
	c.receiptP1()
	c.commitment()
	c.dozenQuality()
	c.milestones()
	c.dozenDelivery()
	c.phaseOf("shipment and export lot (L4 in DOZEN)")
	var lines []cnatLine
	var lot [][3]string
	for i, l := range brcLines {
		lines = append(lines, cnatLine{fmt.Sprintf("S%d", i+1), l.id, cnatDozens(l.id)})
		lot = append(lot, [3]string{fmt.Sprintf("E%d", i+1), fmt.Sprintf("S%d", i+1), cnatDozens(l.id)})
	}
	s := c.newShip("shipment.1", 1, false, lines...)
	c.shipmentID = s.id
	c.releaseShip(s)
	c.confirmShip(s)
	v := c.lotOf("lot.1", s.id, lot...)
	if brcDig(v, "totals", "net_minor") != "96000" || !strings.EqualFold(brcStr(brcDig(v, "lines", 3, "unit_id")), "DOZEN") {
		c.t.Fatalf("export lot %v", v)
	}
	return dozen, conversion
}
