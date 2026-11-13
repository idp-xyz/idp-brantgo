//go:build integration

package bootstrap_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The CN-AT suite (S5 unit D2a; WBS S5-05 "适用 CN-AT", S5 decision E-5: every CN-AT01–CN-AT18 case
// is in S5; E-1: manual paths and synthetic data): one test per case of product baseline V1.0
// §24.3, each run end to end on PostgreSQL 16 over the HTTP API of the composed process with the
// actors the case names, on BRC-001 (D1's chain, cnat_harness_integration_test.go). The owning
// units' PG16 tests prove each rule in detail and are cited per case; these tests prove that the
// composed process carries the case through its routes, guards, consumers and reads.

// CN-AT01 单主体、自营一般贸易的普通出口 — "采购→收货→购进票→两批出运→报关结果→收汇与退税资料贯通；无虚构外部成功"
// (the two-shipment variant; D1 runs the one-shipment chain, TestBRC001OrderToSettlementOverHTTP).
// BRC-001 to its verified supplier delivery (§5 steps 1–11: the purchase commitment PO-2026-00001,
// quality releases and supplier.A's delivery of 120 PCS verified); supplier.A's VAT special invoice
// imported and verified by tax.officer, matched to the four commitment lines (CNX-INV-005) before
// anything ships; two shipments — shipment.1 of L1 + L2 (24 + 36 = 60 PCS, 480.00 USD) and
// shipment.2 of L3 + L4 (12 + 48 = 60 PCS, 480.00 USD) — each prepared from the one delivery and
// the releases, packed, booked, released, confirmed, documented (CI, PL, B/L: 0 differences, issued)
// and its documents released (FUL-INV-001..004, WF-07); an export lot per shipment (R36) declared by
// doc.1 through confirmation, the manual submission and customs' release receipt (CN-R05, CN-R07);
// a sales statement per shipment (P1's prepayment moves to the first, D-4), the customer's two
// remittances applied (CN-R10), the payable of the delivery paid; the invoice line's 120 PCS
// allocated as export_refund evidence across both lots' tax refund cases (CN-R04); each lot's
// document bundle complete with its declaration, the invoice and the fund records that paid it
// (commercial allocation, CN-R11); the order closed (shipped 120 = ordered) and the reservation
// released by STL's consumer. No external success is invented: customs' results are recorded
// receipts with their original text, FUL's departure milestone stays unrecorded, the refund cases
// state no amount and no result, and the receivables close on recorded fund records only.
func TestCNATSuite01SingleEntityExportInTwoShipments(t *testing.T) {
	c := newCNAT(t, "CN-AT01")
	c.toDelivery()
	c.observe("采购→收货: PO %s issued and confirmed; 120 PCS released by QLT; supplier.A's delivery of 120 PCS verified by purchaser", c.poNumber)
	c.invoice()
	c.observe("购进票: purchase invoice %s verified by tax.officer before the shipments, 120 PCS matched to commitment.1 L1–L4", c.invoiceID)

	c.phaseOf("two shipments")
	s1 := c.newShip("shipment.1", 1, true, cnatLine{"S1", "L1", "24"}, cnatLine{"S2", "L2", "36"})
	s2 := c.newShip("shipment.2", 2, true, cnatLine{"S1", "L3", "12"}, cnatLine{"S2", "L4", "48"})
	for _, s := range []*cnatShip{s1, s2} {
		c.releaseShip(s)
		c.confirmShip(s)
		c.shipDocuments(s)
	}
	f := c.get(brcMerchandiser, "/api/brant/v1/ful/orders/order.1/fulfilment")
	var shipped []any
	for i, l := range brcLines {
		if brcDig(f, "lines", i, "shipped") != l.qty {
			c.t.Fatalf("fulfilment after two shipments %v", f)
		}
		shipped = append(shipped, brcDig(f, "lines", i, "shipped"))
	}
	c.fact("shipped", shipped)
	var numbers []string
	for _, s := range []*cnatShip{s1, s2} {
		set := c.get(brcDocumentation, "/api/brant/v1/ful/document_sets/"+s.set)
		numbers = append(numbers, brcStr(c.issued(set, "commercial_invoice")["number"]), brcStr(c.issued(set, "packing_list")["number"]))
	}
	c.fact("documents", numbers)
	c.observe("两批出运: shipment.1 (L1 24 + L2 36) and shipment.2 (L3 12 + L4 48) released and confirmed from one delivery; shipped per line %v = ordered; documents %v issued with 0 differences, originals released", shipped, numbers)

	c.phaseOf("export lots and customs declarations")
	for _, lot := range []struct{ id, shipment, a, b string }{{"lot.1", "shipment.1", "24", "36"}, {"lot.2", "shipment.2", "12", "48"}} {
		v := c.lotOf(lot.id, lot.shipment, [3]string{"E1", "S1", lot.a}, [3]string{"E2", "S2", lot.b})
		if brcDig(v, "totals", "net_minor") != "48000" || v["reconciliation"] != "matched" {
			c.t.Fatalf("export lot %s %v", lot.id, v)
		}
	}
	o := c.get(brcSales, "/api/brant/v1/trd/orders/order.1")
	c.ok(brcSales, "trd", trdapi.Contract, "order.document.generate", map[string]any{"order_id": "order.1", "expected_order_revision": 1,
		"expected_order_hash": o["order_hash"], "document_kind": trdapi.DocumentSalesContract, "template_ref": c.scTemplate, "sequence_ref": c.scSeq})
	declarations := map[string]string{}
	for i, lot := range []string{"lot.1", "lot.2"} {
		number := fmt.Sprintf("SYN-2026-00001%d", i+1)
		id := c.declare(lot, number, fmt.Sprintf("SW-CNAT01-%04d", i+1), "E1", "E2")
		declarations[lot] = id
		v := c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+id)
		ver := brcMap(brcDig(v, "versions", 0))
		if v["status"] != "released" || v["customs_released"] != true || v["declaration_number"] != number || ver["lot_value_minor"] != "48000" ||
			brcDig(ver, "items", 0, "base_quantity") != "60" || len(brcList(ver["sales_documents"])) != 1 ||
			brcStr(brcDig(v, "receipts", 0, "original_text")) == "" || brcDig(v, "receipts", 0, "mapping_version") != "v1" {
			c.t.Fatalf("declaration of %s %v", lot, v)
		}
		c.fact("declaration_id", id, "status", v["status"], "exported_at", v["exported_at"], "sales_contract", brcDig(ver, "sales_documents", 0, "number"))
	}
	plan := c.get(brcMerchandiser, "/api/brant/v1/ful/milestone_plans/plan.order.1")
	if len(brcList(plan["actuals"])) != 1 {
		c.t.Fatalf("milestones after the customs releases %v", plan)
	}
	c.observe("报关结果: lot.1 and lot.2 (480.00 USD, 60 PCS each, one item) released by customs' recorded receipts (original text, mapping v1, sequence 1); the S/C is bound; FUL's actual_departure stays unrecorded (CN-R07)")

	c.phaseOf("sales statements and collection")
	r1 := c.statementOf("stmt.sales.1", s1)
	r2 := c.statementOf("stmt.sales.2", s2)
	if r1["amount_minor"] != "48000" || r1["applied_minor"] != "28800" || r1["open_minor"] != "19200" || r2["amount_minor"] != "48000" ||
		r2["applied_minor"] != "0" || r2["open_minor"] != "48000" {
		c.t.Fatalf("receivables %v / %v", r1, r2)
	}
	c.fact("stmt_sales_1", installmentsView(r1), "stmt_sales_2", installmentsView(r2))
	if e := c.exposure(brcCreditOfficer); e != "67200" {
		c.t.Fatalf("exposure after both statements %s", e)
	}
	c.receipt("fund.p2a", "192.00", "192.00", "MT103-CNAT01-A")
	c.applyTo("app.p2a", "fund.p2a", "stmt.sales.1", "P2", "192.00")
	c.receipt("fund.p2b", "480.00", "480.00", "MT103-CNAT01-B")
	c.applyTo("app.p2b", "fund.p2b", "stmt.sales.2", "P2", "480.00")
	for _, id := range []string{"stmt.sales.1", "stmt.sales.2"} {
		if v := c.get(brcTreasury, "/api/brant/v1/stl/receivables/"+id); v["open_minor"] != "0" || v["applied_minor"] != "48000" {
			c.t.Fatalf("receivable %s after the collection %v", id, v)
		}
	}
	if e := c.exposure(brcCreditOfficer); e != "0" {
		c.t.Fatalf("exposure after the collection %s", e)
	}
	c.payable()
	c.observe("收汇: stmt.sales.1 480.00 = P1 288.00 (prepayment moved, D-4) + P2 192.00, stmt.sales.2 480.00 = P2; fund.p2a 192.00 and fund.p2b 480.00 applied; both receivables 0, exposure 672.00 → 0; payable 600.00 paid")

	c.phaseOf("tax refund material and document bundles")
	cases := map[string]string{}
	for _, lot := range []struct{ id, a, b string }{{"lot.1", "24", "36"}, {"lot.2", "12", "48"}} {
		r := c.allocate(lot.id, cnatAlloc("alloc."+lot.id+".E1", "E1", c.invoiceID, lot.a), cnatAlloc("alloc."+lot.id+".E2", "E2", c.invoiceID, lot.b))
		cases[lot.id] = brcStr(r["object_id"])
		v := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+cases[lot.id])
		if v["status"] != cnxapi.TaxExpected || len(brcList(v["allocations"])) != 2 || len(brcList(v["results"])) != 0 ||
			brcDig(v, "amounts", "expected_minor") != nil || brcDig(v, "amounts", "approved_minor") != nil || brcDig(v, "amounts", "received_minor") != nil ||
			brcDig(v, "policy", "revision") != float64(1) {
			c.t.Fatalf("tax refund case of %s %v", lot.id, v)
		}
		c.fact("case_id", cases[lot.id], "status", v["status"], "policy_revision", brcDig(v, "policy", "revision"), "results", 0)
	}
	if b := c.balance(c.invoiceID); b != "120/120 available 0" {
		c.t.Fatalf("invoice balance %s", b)
	}
	c.source(brcDocumentation, "docs.originals", "合成单证核对记录：两批出运的商业发票、装箱单与提单与 FUL 已签发的单证一致。")
	funds := map[string][]string{"lot.1": {"fund.p1", "fund.p2a"}, "lot.2": {"fund.p2b"}}
	advices := map[string][]string{"lot.1": {"bank.p1", "advice.fund.p2a"}, "lot.2": {"advice.fund.p2b"}}
	shipments := map[string]*cnatShip{"lot.1": s1, "lot.2": s2}
	for _, lot := range []string{"lot.1", "lot.2"} {
		r := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandCheckBundle, map[string]any{"export_lot_id": lot, "expected_version": 0,
			"requirements": []any{
				cnatRequirement("ci", "commercial_invoice", "verified", []string{"docs.originals"}, nil),
				cnatRequirement("pl", "packing_list", "verified", []string{"docs.originals"}, nil),
				cnatRequirement("bl", "bill_of_lading", "verified", []string{"docs.originals", "bl.release." + shipments[lot].id}, nil),
				cnatRequirement("declaration", "customs_declaration", "verified", []string{"receipt.text.receipt." + lot},
					map[string]any{"declaration_id": declarations[lot]}),
				cnatRequirement("invoice", "purchase_invoice", "verified", []string{"invoice.received"}, nil),
				cnatRequirement("receipts", "fund_record", "verified", advices[lot], map[string]any{"fund_record_ids": funds[lot]})},
			"expect_complete": true})
		if r["status"] != "complete" {
			c.t.Fatalf("bundle of %s %v", lot, r)
		}
		v := c.get(brcDocumentation, "/api/brant/v1/cnx/document_bundles/"+brcStr(r["object_id"]))
		var labels []any
		for _, req := range brcList(v["requirements"]) {
			for _, fr := range brcList(brcDig(req, "fund_records")) {
				labels = append(labels, brcDig(fr, "label"))
			}
		}
		if v["status"] != "complete" || len(labels) != len(funds[lot]) || labels[0] != cnxapi.CommercialAllocation {
			c.t.Fatalf("bundle view of %s %v", lot, v)
		}
		c.fact("bundle_id", r["object_id"], "status", v["status"], "fund_record_labels", labels)
	}
	c.observe("退税资料: invoice line 1 (120 PCS) allocated as export_refund evidence 60 to lot.1's case and 60 to lot.2's case (balance 120/120, 0 available), both cases expected under export.tax r1 with no amount and no result; both bundles complete with declaration, invoice and the fund records labelled %s (CN-R11)", cnxapi.CommercialAllocation)

	// order.close cites the originals' release (D1's evidence ID): both shipments' B/Ls released.
	c.source(brcDocumentation, "bl.release", "合成放单汇总：shipment.1 与 shipment.2 的正本提单均已交买方指定货代 provider.1（FCA）。")
	c.closeOrder()
	c.dispatch("after order.close (CN-AT01)")
	c.reservationReleased()
	c.observe("贯通: order.1 closed (shipped 24/36/12/48 = ordered across two shipments); OrderClosed released the 672.00 reservation through STL's consumer; exposure 0")
	for _, id := range cases {
		if v := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+id); v["status"] != cnxapi.TaxExpected || len(brcList(v["results"])) != 0 {
			c.t.Fatalf("tax case at the end %v", v)
		}
	}
	res := c.get(brcTreasury, "/api/brant/v1/stl/orders/order.1/settlement")
	c.fact("order_settlement", res["net_sales_minor"])
	c.observe("无虚构外部成功: no refund declared, approved or received (both cases expected, no result); customs' outcomes only as recorded receipts; receivables closed by recorded fund records only (%s)", stlapi.FundReceipt)
}

// CN-AT02 一单分批出口、同次出口包含多销售行 — "销售、出运及报关项映射可核对，累计不超有效业务范围" (B1b's lots,
// C3a's declaration items; TestCNAT02DeclaresMultiLineLotsFromFUL, TestExportLotsMapSplitShipmentsCNAT02): order.1 ships in two
// batches from one verified delivery, sales line L2 split across them — shipment.1 L1 24 + L2 20,
// shipment.2 L2 16 + L3 12 + L4 48; a third shipment of one more L2 is SHIPMENT_QUANTITY_EXCEEDED
// (FUL-INV-001 over both). Each shipment's export lot holds several sales lines; a shipment line
// already in a lot is BUSINESS_KEY_CONFLICT (no second stock), a lot line above its shipment line
// INVALID_KNOWN_VALUE. doc.1 declares lot.1 as one item of two sales lines (one classification) and
// lot.2 as two items; a lot line left out of the items is refused, a second open declaration of a
// lot BUSINESS_KEY_CONFLICT. The declarations' lines name their shipment and sales line: per sales
// line, what the declarations declare equals what FUL shipped and what the order orders.
func TestCNATSuite02SplitShipmentsAndMultiLineExports(t *testing.T) {
	c := newCNAT(t, "CN-AT02")
	c.toDelivery()
	c.phaseOf("split shipments")
	s1 := c.newShip("shipment.1", 1, false, cnatLine{"S1", "L1", "24"}, cnatLine{"S2", "L2", "20"})
	s2 := c.newShip("shipment.2", 2, false, cnatLine{"S1", "L2", "16"}, cnatLine{"S2", "L3", "12"}, cnatLine{"S3", "L4", "48"})
	for _, s := range []*cnatShip{s1, s2} {
		c.releaseShip(s)
		c.confirmShip(s)
	}
	reason := c.no("human", brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandPrepareShipment, map[string]any{"shipment_id": "shipment.3",
		"order_id": "order.1", "route": fulapi.RouteSupplierDirect, "lines": []any{map[string]any{"line_id": "S1", "order_line_id": "L2", "quantity": "1",
			"supplier_delivery": map[string]any{"delivery_id": "delivery.1", "line_id": "L2", "version": 1}, "quality_release_id": "rel.L2"}}},
		http.StatusConflict, fulapi.CodeShipmentQuantityExceeded, "")
	f := c.get(brcMerchandiser, "/api/brant/v1/ful/orders/order.1/fulfilment")
	for i, l := range brcLines {
		if brcDig(f, "lines", i, "shipped") != l.qty {
			c.t.Fatalf("fulfilment %v", f)
		}
	}
	c.observe("分批出运: shipment.1 (L1 24, L2 20) and shipment.2 (L2 16, L3 12, L4 48) confirmed; L2 shipped 20 + 16 = 36 = ordered; one more L2 → %s (%s)",
		fulapi.CodeShipmentQuantityExceeded, reason)

	c.phaseOf("export lots of several sales lines")
	lot1 := c.lotOf("lot.1", "shipment.1", [3]string{"E1", "S1", "24"}, [3]string{"E2", "S2", "20"})
	c.no("human", brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRegisterExportLot, map[string]any{"export_lot_id": "lot.1b", "shipment_id": "shipment.1",
		"lines": []any{map[string]any{"line_id": "E1", "shipment_line_id": "S2", "quantity": "20"}}}, http.StatusConflict, fulapi.CodeBusinessKeyConflict,
		"/args/lines/0/shipment_line_id")
	c.no("human", brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRegisterExportLot, map[string]any{"export_lot_id": "lot.2x", "shipment_id": "shipment.2",
		"lines": []any{map[string]any{"line_id": "E1", "shipment_line_id": "S1", "quantity": "17"}}}, http.StatusUnprocessableEntity, kernel.CodeInvalidKnownValue,
		"/args/lines/0/quantity")
	lot2 := c.lotOf("lot.2", "shipment.2", [3]string{"E1", "S1", "16"}, [3]string{"E2", "S2", "12"}, [3]string{"E3", "S3", "48"})
	if brcDig(lot1, "totals", "net_minor") != "35200" || brcDig(lot2, "totals", "net_minor") != "60800" || brcDig(lot1, "lines", 1, "order_line_id") != "L2" ||
		brcDig(lot2, "lines", 0, "order_line_id") != "L2" {
		c.t.Fatalf("lots %v / %v", lot1, lot2)
	}
	c.observe("出口批次: lot.1 = shipment.1 S1 (L1 24) + S2 (L2 20), 352.00 USD; lot.2 = shipment.2 S1 (L2 16) + S2 (L3 12) + S3 (L4 48), 608.00 USD; a shipment line in a second lot → %s, a lot line above its shipment line (17 > 16) → %s",
		fulapi.CodeBusinessKeyConflict, kernel.CodeInvalidKnownValue)

	c.phaseOf("declaration items")
	d1, rv := c.declPrepare("lot.1", cnatItem(1, "E1", "E2"))
	c.declConfirm(d1, rv, 1)
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandPrepareDeclaration, map[string]any{"expected_version": 0, "export_lot_id": "lot.2",
		"code_table_version": codeTable, "items": []any{cnatItem(1, "E1", "E2")}}, http.StatusUnprocessableEntity, kernel.CodeInvalidKnownValue, "/args/items")
	d2, rv := c.declPrepare("lot.2", cnatItem(1, "E1", "E2"), cnatItem(2, "E3"))
	c.declConfirm(d2, rv, 1)
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandPrepareDeclaration, map[string]any{"expected_version": 0, "export_lot_id": "lot.1",
		"code_table_version": codeTable, "items": []any{cnatItem(1, "E1", "E2")}}, http.StatusConflict, cnxapi.CodeBusinessKeyConflict, "/args/export_lot_id")
	declared := map[string]kernel.Decimal{}
	var mapping []string
	for _, id := range []string{d1, d2} {
		v := c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+id)
		if v["status"] != "confirmed" {
			c.t.Fatalf("declaration %v", v)
		}
		for _, it := range brcList(brcDig(v, "versions", 0, "items")) {
			base := kernel.Decimal{}
			for _, l := range brcList(brcDig(it, "lines")) {
				q := kernel.MustDecimal(brcStr(brcDig(l, "quantity")))
				declared[brcStr(brcDig(l, "order_line_id"))] = declared[brcStr(brcDig(l, "order_line_id"))].Add(q)
				base = base.Add(q)
				mapping = append(mapping, fmt.Sprintf("%v item %v ← %v %v/%v (%v) %v PCS", v["export_lot_id"], brcDig(it, "item_no"), brcDig(l, "line_id"),
					brcDig(l, "shipment_id"), brcDig(l, "shipment_line_id"), brcDig(l, "order_line_id"), brcDig(l, "quantity")))
			}
			if !base.Equal(kernel.MustDecimal(brcStr(brcDig(it, "base_quantity")))) || brcDig(it, "commodity_code") != "6109100021" {
				c.t.Fatalf("item %v", it)
			}
		}
	}
	for _, l := range brcLines {
		if !declared[l.id].Equal(kernel.MustDecimal(l.qty)) {
			c.t.Fatalf("sales line %s declared %v, ordered and shipped %s", l.id, declared[l.id], l.qty)
		}
	}
	c.fact("mapping", mapping)
	c.observe("报关项映射: lot.1 one item of E1 (L1) + E2 (L2) = 44 PCS; lot.2 item 1 E1 (L2) + E2 (L3) = 28, item 2 E3 (L4) = 48; a lot line left out → %s; a second open declaration → %s; per sales line declared = shipped = ordered (L2 20 + 16 = 36)",
		kernel.CodeInvalidKnownValue, cnxapi.CodeBusinessKeyConflict)
	c.observe("mapping %v", mapping)
}

// CN-AT03 票据品名或单位与报关资料不同 — "需受控映射／换算及适用专业依据；无法证实时阻断申报资料确认" (C3a;
// TestCNAT03MappingWithoutAProfessionalBasisIsNotConfirmed): BRC-001 with sales line L4 (ST01.blue.M) sold,
// bought, delivered and shipped as 4 DOZEN (cnat_dozen_integration_test.go), so supplier.A's invoice and
// the export lot state 4 打 while the declaration profile declares 件 per PCS. doc.1 prepares lot.1's
// declaration — item 1 of the three PCS lines, item 2 of the DOZEN line — without a conversion: item 2's
// mapping is unverified, no base or legal quantity is guessed, and confirming the declaration is
// DECLARATION_MAPPING_UNVERIFIED. A conversion revision POL does not have is refused; with POL's
// controlled conversion of ST01.blue.M (12 PCS a DOZEN, its professional basis) the revised version 2 is
// verified (48 PCS, 48 件); the old version is REFERENCE_STALE, version 2 is confirmed, submitted and
// released. The invoice's DOZEN line, matched to the DOZEN commitment line, evidences the DOZEN export
// line for the refund; the invoice's description is not the declaration name (the profile maps it).
func TestCNATSuite03InvoiceUnitDiffersFromDeclaration(t *testing.T) {
	c := newCNAT(t, "CN-AT03")
	_, conversion := c.toDozenLot()
	c.observe("单位不同: L4 (ST01.blue.M) sold at 96.00 USD per DOZEN through POL's conversion %s (base quantity 48 PCS), bought, inspected, delivered and shipped as 4 DOZEN; lot.1 960.00 USD with line E4 4 DOZEN",
		cnatDozenConversion)

	c.phaseOf("declaration without a controlled conversion")
	id, rv := c.declPrepare("lot.1", cnatItem(1, "E1", "E2", "E3"), cnatItem(2, "E4"))
	v := c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+id)
	one, two := brcMap(brcDig(v, "versions", 0, "items", 0)), brcMap(brcDig(v, "versions", 0, "items", 1))
	line := brcMap(brcDig(two, "lines", 0))
	if brcDig(v, "versions", 0, "mapping") != cnxapi.MappingUnverified || one["mapping"] != cnxapi.MappingVerified || one["base_quantity"] != "72" ||
		two["mapping"] != cnxapi.MappingUnverified || brcStr(two["base_quantity"]) != "" || len(brcList(two["legal_quantities"])) != 0 ||
		line["reason"] != "unit_not_converted" || brcDig(line, "conversion", "source") != "none" || line["unit"] != "DOZEN" {
		c.t.Fatalf("the unverified declaration %v", v["versions"])
	}
	reason := c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandConfirmDeclaration, map[string]any{"declaration_id": id, "expected_version": rv,
		"version": 1, "reason": "试图确认未核实映射"}, http.StatusUnprocessableEntity, cnxapi.CodeDeclarationMappingUnverified, "/args/version")
	c.observe("无法证实时阻断: item 1 (E1–E3, 72 PCS) verified; item 2 (E4, 4 DOZEN) unverified — reason unit_not_converted, no base or legal quantity guessed; confirming → %s /args/version (%s)",
		cnxapi.CodeDeclarationMappingUnverified, reason)

	c.phaseOf("controlled conversion")
	revise := func(revision any) map[string]any {
		return map[string]any{"declaration_id": id, "expected_version": rv, "export_lot_id": "lot.1", "code_table_version": codeTable,
			"items": []any{cnatItem(1, "E1", "E2", "E3"), map[string]any{"item_no": 2, "lines": []any{map[string]any{"line_id": "E4",
				"unit_conversion": map[string]any{"id": cnatDozenConversion, "revision": revision}}}}}}
	}
	status, out := c.as("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandPrepareDeclaration, revise(9))
	if status < 400 || errPath(out) != "/args/items/1/lines/0/unit_conversion" {
		c.t.Fatalf("a conversion revision POL lacks: %d %v", status, out)
	}
	r := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandPrepareDeclaration, revise(conversion["revision"]))
	rv = brcNum(r["row_version"])
	v = c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+id)
	two = brcMap(brcDig(v, "versions", 1, "items", 1))
	line = brcMap(brcDig(two, "lines", 0))
	if len(brcList(v["versions"])) != 2 || brcDig(v, "versions", 1, "mapping") != cnxapi.MappingVerified || two["base_quantity"] != "48" ||
		brcDig(two, "legal_quantities", 0, "quantity") != "48" || brcDig(two, "legal_quantities", 0, "unit") != "件" ||
		brcDig(line, "conversion", "source") != "unit_conversion" || brcDig(line, "conversion", "id") != cnatDozenConversion ||
		brcDig(line, "conversion", "factor") != "12" {
		c.t.Fatalf("the verified version %v", v["versions"])
	}
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandConfirmDeclaration, map[string]any{"declaration_id": id, "expected_version": rv,
		"version": 1}, http.StatusConflict, cnxapi.CodeReferenceStale, "")
	rv = c.declConfirm(id, rv, 2)
	rv = c.declSubmit(id, rv, "SW-CNAT03-0001")
	if r = c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, c.receiptArgs(id, rv, "rcpt.released", "released", 1,
		"SYN-2026-000031")); r["status"] != "released" {
		c.t.Fatalf("released %v", r)
	}
	c.observe("受控换算: a conversion revision POL does not have → %d %v at /args/items/1/lines/0/unit_conversion; with %s r%v (factor 12, its professional basis) version 2 is verified: 4 DOZEN = 48 PCS = 48 件; confirming version 1 → %s; version 2 confirmed, submitted and released",
		status, out["code"], cnatDozenConversion, conversion["revision"], cnxapi.CodeReferenceStale)

	c.phaseOf("the invoice in DOZEN")
	facts := map[string]any{"kind": "special_vat", "colour": "blue", "number": "33002611031", "issued_on": today(),
		"supplier": map[string]any{"party_id": "supplier.A", "name": "supplier.A", "tax_id": "91330200SYNTHA0001"},
		"buyer":    map[string]any{"name": "合成企业", "tax_id": "SYNTHETIC-NOT-REAL"}, "currency": map[string]any{"id": "USD", "revision": 1},
		"lines": []any{map[string]any{"line_no": 1, "description": "针织T恤 红S/红M/蓝S", "quantity": "72", "unit": "PCS", "net_amount": "360.00",
			"tax_rate": "0.13", "tax_amount": "46.80"}, map[string]any{"line_no": 2, "description": "针织T恤 蓝M（打）", "quantity": "4", "unit": "DOZEN",
			"net_amount": "240.00", "tax_rate": "0.13", "tax_amount": "31.20"}}, "net_total": "600.00", "tax_total": "78.00", "gross_total": "678.00"}
	c.scanInvoice("invoice.scan", "33002611031", "72 + 4 打")
	inv, irv := c.importInvoice("human", brcTaxOfficer, "invoice.scan", facts)
	m4 := cnatMatch("L4", "4")
	m4["line_no"] = 2
	c.verifyInvoice(inv, irv, facts, cnatMatch("L1", "24"), cnatMatch("L2", "36"), cnatMatch("L3", "12"), m4)
	e4 := cnatAlloc("alloc.E4", "E4", inv, "4")
	e4["line_no"] = 2
	c.allocate("lot.1", cnatAlloc("alloc.E1", "E1", inv, "24"), cnatAlloc("alloc.E2", "E2", inv, "36"), cnatAlloc("alloc.E3", "E3", inv, "12"), e4)
	iv := c.get(brcTaxOfficer, "/api/brant/v1/cnx/purchase_invoices/"+inv)
	var units []string
	for _, b := range brcList(iv["balances"]) {
		if brcDig(b, "purpose") == cnxapi.PurposeExportRefund {
			units = append(units, fmt.Sprintf("line %v %v/%v %v", brcDig(b, "line_no"), brcDig(b, "allocated"), brcDig(b, "quantity"), brcDig(b, "unit")))
		}
	}
	if len(units) != 2 || units[1] != "line 2 4/4 DOZEN" || units[0] != "line 1 72/72 PCS" {
		c.t.Fatalf("invoice balances %v", units)
	}
	c.observe("票据单位: invoice 33002611031 states line 2 as 4 DOZEN (\"针织T恤 蓝M（打）\"), matched to the DOZEN commitment line L4 and allocated to export line E4 (4 DOZEN) — balances %v; the declaration declares the same goods as 48 件 through the controlled conversion; the invoice's description is not compared with the declaration name (the verified profile maps it, C3a decision 6)",
		units)
	c.deviate("the unit difference is a sales line in DOZEN carried end to end (quote, purchase, QLT, delivery, shipment, lot, invoice), adapted from the S3 chain in cnat_dozen_integration_test.go; the S3 chain's read-back and uniqueness phases are not run for this variant")
}

// CN-AT04 同一购进凭证部分用于多批业务 — "按税务用途和适用政策核对剩余；并发及重试不重复占用" (C3b;
// TestCNAT04PurposeBalancesAcrossBatches, TestCNAT04ConcurrentAllocationsOneWins): shipment.1's four lines
// go into two export lots (lot.a = L1 + L2, lot.b = L3 + L4, 60 PCS each), both declared and released.
// supplier.A invoices the 120 PCS on two VAT special invoices, A for 100 PCS and B for 20 PCS, each
// verified and matched by tax.officer. Two allocations of invoice A sent at the same instant over
// HTTP, 60 + 60 > 100: one wins, the other is TAX_EVIDENCE_EXCEEDED; the winner's retry with its
// idempotency key is answered again and uses nothing twice. The loser takes A's remaining 40 and 20
// of B; one more of A is TAX_EVIDENCE_EXCEEDED. The exemption purpose keeps its own balance of A.
func TestCNATSuite04OneInvoiceAcrossBatches(t *testing.T) {
	c := newCNAT(t, "CN-AT04")
	c.toShipped()
	c.phaseOf("two export lots of one shipment, declared")
	c.lotOf("lot.a", c.shipmentID, [3]string{"E1", "S1", "24"}, [3]string{"E2", "S2", "36"})
	c.lotOf("lot.b", c.shipmentID, [3]string{"E1", "S3", "12"}, [3]string{"E2", "S4", "48"})
	c.declare("lot.a", "SYN-2026-000041", "SW-CNAT04-0001", "E1", "E2")
	c.declare("lot.b", "SYN-2026-000042", "SW-CNAT04-0002", "E1", "E2")
	c.phaseOf("two purchase invoices verified")
	c.source(brcPurchaser, "invoice.received", "合成收票记录：收到 supplier.A 增值税专用发票 33002611041（100 PCS）与 33002611042（20 PCS）原件。")
	c.scanInvoice("scan.A", "33002611041", "100")
	c.scanInvoice("scan.B", "33002611042", "20")
	factsA, factsB := cnatInvoice("33002611041", "blue", "100", ""), cnatInvoice("33002611042", "blue", "20", "")
	a, rv := c.importInvoice("human", brcTaxOfficer, "scan.A", factsA)
	c.verifyInvoice(a, rv, factsA, cnatMatch("L1", "24"), cnatMatch("L2", "36"), cnatMatch("L4", "40"))
	b, rv := c.importInvoice("human", brcTaxOfficer, "scan.B", factsB)
	c.verifyInvoice(b, rv, factsB, cnatMatch("L3", "12"), cnatMatch("L4", "8"))
	c.observe("one invoice for several batches: A 100 PCS (matched L1 24, L2 36, L4 40) and B 20 PCS (L3 12, L4 8) verified by tax.officer")

	c.phaseOf("concurrent allocations of invoice A")
	lots := map[string][][2]string{"lot.a": {{"E1", "24"}, {"E2", "36"}}, "lot.b": {{"E1", "12"}, {"E2", "48"}}}
	args := func(lot, prefix, invoice string) map[string]any {
		var as []map[string]any
		for _, l := range lots[lot] {
			as = append(as, cnatAlloc(prefix+lot+"."+l[0], l[0], invoice, l[1]))
		}
		return cnatAllocArgs(lot, as...)
	}
	race := []map[string]any{args("lot.a", "x.", a), args("lot.b", "x.", a)}
	answers := c.concurrent(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandAllocateTaxEvidence, race...)
	winner, loser := -1, -1
	for i, out := range answers {
		switch {
		case out["http_status"] == float64(http.StatusOK) && out["ok"] == true:
			winner = i
		case out["http_status"] == float64(http.StatusConflict) && out["code"] == cnxapi.CodeTaxEvidenceExceeded:
			loser = i
		}
	}
	if winner < 0 || loser < 0 {
		c.t.Fatalf("two allocations of 60 against 100: want one granted and one %s, got %v", cnxapi.CodeTaxEvidenceExceeded, answers)
	}
	if got := c.balance(a); got != "60/100 available 40" {
		c.t.Fatalf("invoice A after the race %s", got)
	}
	_, replay := c.send("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandAllocateTaxEvidence, brcStr(answers[winner]["sent_key"]), race[winner])
	if replay["ok"] != true || replay["object_id"] != answers[winner]["object_id"] || replay["action_id"] != answers[winner]["action_id"] {
		c.t.Fatalf("the winner's retry %v / %v", replay, answers[winner])
	}
	if got := c.balance(a); got != "60/100 available 40" {
		c.t.Fatalf("invoice A after the retry %s", got)
	}
	won, lost := []string{"lot.a", "lot.b"}[winner], []string{"lot.a", "lot.b"}[loser]
	c.observe("并发: allocations of 60 (%s) and 60 (%s) of invoice A sent at once: %s granted, %s refused %s; A 60/100; the winner's retry under its key answered with the same action %v, A still 60/100",
		"lot.a", "lot.b", won, lost, cnxapi.CodeTaxEvidenceExceeded, answers[winner]["action_id"])

	c.phaseOf("the loser takes A's rest and B")
	remaining := kernel.MustDecimal("40")
	var rest []map[string]any
	for _, l := range lots[lost] {
		q := kernel.MustDecimal(l[1])
		fromA := q
		if fromA.Cmp(remaining) > 0 {
			fromA = remaining
		}
		if fromA.Sign() > 0 {
			rest = append(rest, cnatAlloc("y."+lost+"."+l[0]+".A", l[0], a, fromA.String()))
			remaining = remaining.Sub(fromA)
		}
		if fromB := q.Sub(fromA); fromB.Sign() > 0 {
			rest = append(rest, cnatAlloc("y."+lost+"."+l[0]+".B", l[0], b, fromB.String()))
		}
	}
	c.allocate(lost, rest...)
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandAllocateTaxEvidence, cnatAllocArgs("lot.a", cnatAlloc("z.one", "E1", a, "1")),
		http.StatusConflict, cnxapi.CodeTaxEvidenceExceeded, "/args/allocations/0/quantity")
	exemption := cnatAlloc("w.lot.a.E1", "E1", a, "24")
	exemption["purpose"] = cnxapi.PurposeExportExemption
	c.allocate("lot.a", exemption)
	gotA, gotB, gotX := c.balance(a), c.balance(b), c.balanceOf(a, cnxapi.PurposeExportExemption)
	if gotA != "100/100 available 0" || gotB != "20/20 available 0" || gotX != "24/100 available 76" {
		c.t.Fatalf("balances A %s, B %s, A exemption %s", gotA, gotB, gotX)
	}
	cases := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases")
	if n := len(brcList(cases["tax_refund_cases"])); n != 2 {
		c.t.Fatalf("cases %v", cases)
	}
	c.observe("剩余: %s takes A's remaining 40 and 20 of B (%d allocations); one more of A → %s; export_refund A %s, B %s; export_exemption of A keeps its own balance %s; two cases, one per lot",
		lost, len(rest), cnxapi.CodeTaxEvidenceExceeded, gotA, gotB, gotX)
}

// CN-AT05 先出运后到票，原票随后红冲 — "库存／交付事实保留；暂估、应付及退税资料分别处理影响" (C3b, B2b;
// TestCNAT05RedReversalPutsMatchesBackToReview, TestRedReversalAndAllocationCommitOrders): supplier.A's goods
// are delivered (120 verified), shipped and exported, and the payable of the verified delivery is
// confirmed and paid before any invoice exists (B2b: the payable rests on the delivery). The VAT
// special invoice comes later, is verified and matched, and its line evidences lot.1's refund case.
// A red invoice then reverses it: the blue one is reversed, its matches and the case's allocations
// are under review, and planning the case is refused (INVALID_STATE). Delivery, shipment, lot and
// payable stay as they are; PurchaseInvoiceVerified of the red invoice names the reversed invoice and
// the commitment whose payable needs review — no consumer turns it into a payable change. A
// corrected invoice, verified and matched again, replaces the allocations and the case is planned.
func TestCNATSuite05ShipFirstInvoiceLaterRedReversal(t *testing.T) {
	c := newCNAT(t, "CN-AT05")
	c.toLot()
	c.declaration()
	c.payable()
	c.observe("先出运: 120 PCS delivered (verified), shipped, exported (lot.1 released by customs); the payable 600.00 of the verified delivery confirmed and paid before any invoice")
	c.invoice()
	c.taxCase()
	c.observe("后到票: invoice %s verified and matched 120; lot.1's case allocates it (4 lines) and records the expected 78.00", c.invoiceID)

	c.phaseOf("red reversal")
	c.scanInvoice("invoice.red.scan", "33002611002", "120")
	red := cnatInvoice("33002611002", "red", "120", "33002611001")
	r, rv := c.importInvoice("human", brcTaxOfficer, "invoice.red.scan", red)
	c.verifyInvoice(r, rv, red)
	blue := c.get(brcTaxOfficer, "/api/brant/v1/cnx/purchase_invoices/"+c.invoiceID)
	var matches []any
	for _, m := range brcList(blue["matches"]) {
		matches = append(matches, brcDig(m, "status"))
		if brcDig(m, "status") != "review" {
			c.t.Fatalf("the reversed invoice's matches %v", blue["matches"])
		}
	}
	if blue["reversed_by_id"] != r || len(matches) != 4 {
		c.t.Fatalf("the reversed invoice %v", blue)
	}
	tc := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+c.taxCaseID)
	for _, a := range brcList(tc["allocations"]) {
		if brcDig(a, "status") != cnxapi.EvidenceReview {
			c.t.Fatalf("allocations after the red invoice %v", tc["allocations"])
		}
	}
	planned := map[string]any{"case_id": c.taxCaseID, "expected_version": tc["row_version"], "stage": cnxapi.TaxPlanned, "currency": c.usdRef(),
		"amount": "78.00", "tax_period": "2026-11", "review": map[string]any{"reviewer": "合成税务师（测试）", "evidence_ids": []string{"tax.review"}}}
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxResult, planned, http.StatusUnprocessableEntity, kernel.CodeInvalidState, "")
	c.observe("红冲: red invoice %s verified: %s reversed (reversed_by %s), its 4 matches → review, the case's 4 allocations → review; planning the case → %s",
		r, c.invoiceID, r, kernel.CodeInvalidState)

	c.phaseOf("delivery, shipment and payable unchanged")
	deliveries := c.get(brcPurchaser, "/api/brant/v1/pur/purchase_commitments/commitment.1/deliveries")
	f := c.get(brcMerchandiser, "/api/brant/v1/ful/orders/order.1/fulfilment")
	pay := c.get(brcTreasury, "/api/brant/v1/stl/payables/stmt.purchase.1")
	lot := c.get(brcMerchandiser, "/api/brant/v1/ful/export_lots/lot.1")
	if pay["amount_minor"] != "60000" || pay["open_minor"] != "0" || brcDig(f, "lines", 3, "shipped") != "48" || brcDig(lot, "totals", "net_minor") != "96000" {
		c.t.Fatalf("facts after the red invoice: payable %v, fulfilment %v, lot %v", pay, f, lot)
	}
	c.fact("deliveries", deliveries["lines"])
	c.dispatch("after the red invoice (CN-AT05)")
	var verified []map[string]any
	for _, e := range c.delivered {
		if e.Type == "idp-brant.cnx.purchase_invoice_verified.v1" {
			var p map[string]any
			c.w.must(json.Unmarshal(e.Payload, &p))
			verified = append(verified, p)
		}
	}
	if len(verified) != 2 || verified[1]["reversed_invoice_id"] != c.invoiceID || fmt.Sprint(verified[1]["review_commitment_ids"]) != "[commitment.1]" {
		c.t.Fatalf("PurchaseInvoiceVerified events %v", verified)
	}
	for _, s := range c.subs {
		for _, typ := range s.EventTypes {
			if string(typ) == "idp-brant.cnx.purchase_invoice_verified.v1" {
				c.t.Fatalf("consumer %s subscribes PurchaseInvoiceVerified", s.Consumer)
			}
		}
	}
	c.observe("交付事实保留: PUR deliveries %v, FUL shipped 120, lot.1 960.00 unchanged; the payable 600.00 (paid) unchanged; PurchaseInvoiceVerified of the red invoice names reversed %s and review_commitment_ids %v (no consumer: B2b's payable rests on the delivery)",
		deliveries["lines"], c.invoiceID, verified[1]["review_commitment_ids"])

	c.phaseOf("corrected invoice replaces the allocations")
	c.scanInvoice("invoice.new.scan", "33002611003", "120")
	fresh := cnatInvoice("33002611003", "blue", "120", "")
	n, rv := c.importInvoice("human", brcTaxOfficer, "invoice.new.scan", fresh)
	var ms []map[string]any
	for _, l := range brcLines {
		ms = append(ms, cnatMatch(l.id, l.qty))
	}
	c.verifyInvoice(n, rv, fresh, ms...)
	var replacements []map[string]any
	for i, l := range brcLines {
		a := cnatAlloc(fmt.Sprintf("alloc.E%d.r", i+1), fmt.Sprintf("E%d", i+1), n, l.qty)
		a["replaces"] = fmt.Sprintf("alloc.E%d", i+1)
		replacements = append(replacements, a)
	}
	rr := c.allocate("lot.1", replacements...)
	tc = c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+c.taxCaseID)
	states := map[string]int{}
	for _, a := range brcList(tc["allocations"]) {
		states[brcStr(brcDig(a, "status"))]++
	}
	if states[cnxapi.EvidenceReplaced] != 4 || states[cnxapi.EvidenceStanding] != 4 {
		c.t.Fatalf("allocations after the replacement %v", states)
	}
	planned["expected_version"] = rr["row_version"]
	if p := c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxResult, planned); p["status"] != cnxapi.TaxPlanned {
		c.t.Fatalf("planned %v", p)
	}
	if got := c.balance(n); got != "120/120 available 0" {
		c.t.Fatalf("the corrected invoice %s", got)
	}
	c.observe("退税资料分别处理: corrected invoice %s verified and matched 120; 4 allocations replace the reviewed ones (replaced 4, standing 4); the case is planned; the reversed invoice and its review stay on record", n)
}

// CN-AT06 改单或撤销后重复、乱序回执 — "保留版本链，不用旧结果覆盖新状态，不重复恢复凭证用途" (C3a;
// TestCNAT06AmendedAndWithdrawnDeclarationsWithOutOfOrderReceipts): doc.1 declares lot.1 and records customs'
// receipts as they arrive, out of order: received (1), released (3) with the export date; the late
// acceptance (2) and a repeated release (3) are DECLARATION_RECEIPT_STALE and change nothing. The tax
// case binds the export date. An amendment (4) of item 1 to 110 PCS makes declaration version 2; a
// repeated amendment is stale; the case keeps the date it bound (no cascade). The withdrawal (5)
// clears the export date; a repeated withdrawal is stale and a release after it INVALID_STATE. The
// case's evidence is not restored by the withdrawal (the invoice line stays used: only an external
// tax result releases it, CN-R09); a new attempt of the lot may be prepared. Every recorded receipt
// publishes DeclarationReceiptRecorded once.
func TestCNATSuite06AmendedWithdrawnDeclarationOutOfOrderReceipts(t *testing.T) {
	c := newCNAT(t, "CN-AT06")
	c.toLot()
	c.invoice()
	c.phaseOf("receipts out of order")
	const number = "SYN-2026-000061"
	id, rv := c.declPrepare("lot.1", cnatItem(1, "E1", "E2", "E3", "E4"))
	rv = c.declConfirm(id, rv, 1)
	rv = c.declSubmit(id, rv, "SW-CNAT06-0001")
	r := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, c.receiptArgs(id, rv, "rcpt.received", "received", 1, number))
	r = c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, c.receiptArgs(id, brcNum(r["row_version"]), "rcpt.released", "released", 3, number))
	rv = brcNum(r["row_version"])
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, c.receiptArgs(id, rv, "rcpt.accepted.late", "accepted", 2, number),
		http.StatusConflict, cnxapi.CodeDeclarationReceiptStale, "/args/external/sequence")
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, c.receiptArgs(id, rv, "rcpt.released.again", "released", 3, number),
		http.StatusConflict, cnxapi.CodeDeclarationReceiptStale, "/args/external/sequence")
	v := c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+id)
	released := brcStr(v["exported_at"])
	if v["status"] != "released" || v["last_sequence"] != float64(3) || len(brcList(v["receipts"])) != 2 || released == "" {
		c.t.Fatalf("after the stale receipts %v", v)
	}
	c.observe("乱序: received (1), released (3) recorded; the late accepted (2) and a repeated released (3) → %s, nothing changed (released, last sequence 3, 2 receipts)",
		cnxapi.CodeDeclarationReceiptStale)

	c.phaseOf("tax evidence on the export date")
	var as []map[string]any
	for i, l := range brcLines {
		as = append(as, cnatAlloc(fmt.Sprintf("alloc.E%d", i+1), fmt.Sprintf("E%d", i+1), c.invoiceID, l.qty))
	}
	tc := c.allocate("lot.1", as...)
	caseID := brcStr(tc["object_id"])
	if got := brcStr(c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+caseID)["exported_at"]); got != released {
		c.t.Fatalf("the case binds %s, customs released %s", got, released)
	}

	c.phaseOf("amendment and withdrawal")
	amend := c.receiptArgs(id, rv, "rcpt.amended", "amended", 4, number)
	amend["amended_items"] = []any{map[string]any{"item_no": 1, "quantity": "110"}}
	r = c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, amend)
	rv = brcNum(r["row_version"])
	again := c.receiptArgs(id, rv, "rcpt.amended.again", "amended", 4, number)
	again["amended_items"] = []any{map[string]any{"item_no": 1, "quantity": "110"}}
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, again, http.StatusConflict, cnxapi.CodeDeclarationReceiptStale, "")
	v = c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+id)
	versions := brcList(v["versions"])
	if v["status"] != "amended" || len(versions) != 2 || brcDig(versions, 1, "kind") != "amended" || brcDig(versions, 1, "amendments", 0, "before") != "120" ||
		brcDig(versions, 1, "amendments", 0, "after") != "110" {
		c.t.Fatalf("amended %v", v)
	}
	amended := brcStr(v["exported_at"])
	if got := brcStr(c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+caseID)["exported_at"]); got != released {
		c.t.Fatalf("the case moved its export date to %s", got)
	}
	r = c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, c.receiptArgs(id, rv, "rcpt.withdrawn", "withdrawn", 5, number))
	rv = brcNum(r["row_version"])
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, c.receiptArgs(id, rv, "rcpt.withdrawn.again", "withdrawn", 5, number),
		http.StatusConflict, cnxapi.CodeDeclarationReceiptStale, "")
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, c.receiptArgs(id, rv, "rcpt.released.after", "released", 6, number),
		http.StatusUnprocessableEntity, kernel.CodeInvalidState, "")
	v = c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+id)
	if v["status"] != "withdrawn" || v["exported_at"] != nil || len(brcList(v["receipts"])) != 4 {
		c.t.Fatalf("withdrawn %v", v)
	}
	if got := c.balance(c.invoiceID); got != "120/120 available 0" {
		c.t.Fatalf("the withdrawal restored evidence: %s", got)
	}
	tcv := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+caseID)
	for _, a := range brcList(tcv["allocations"]) {
		if brcDig(a, "status") != cnxapi.EvidenceStanding {
			c.t.Fatalf("allocations after the withdrawal %v", tcv["allocations"])
		}
	}
	next, _ := c.declPrepare("lot.1", cnatItem(1, "E1", "E2", "E3", "E4"))
	nv := c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+next)
	if next == id || nv["attempt"] != float64(2) {
		c.t.Fatalf("a new attempt %v", nv)
	}
	c.dispatch("after the receipts (CN-AT06)")
	n := 0
	for _, e := range c.delivered {
		if e.Type == "idp-brant.cnx.declaration_receipt_recorded.v1" {
			n++
		}
	}
	if n != 4 {
		c.t.Fatalf("%d DeclarationReceiptRecorded events, want 4", n)
	}
	c.observe("改单: amended (4) → version 2 (item 1: 120 → 110, export date %s); a repeated amendment → %s; the tax case keeps the export date it bound (%s)",
		amended, cnxapi.CodeDeclarationReceiptStale, released)
	c.observe("撤销: withdrawn (5) clears the export date; a repeated withdrawal → %s; a release after it → %s; the version chain keeps 2 versions and 4 receipts with their original texts",
		cnxapi.CodeDeclarationReceiptStale, kernel.CodeInvalidState)
	c.observe("不重复恢复凭证用途: after the withdrawal the case's 4 allocations stay standing and the invoice line stays 120/120 used (no local restoration, CN-R09); a new attempt (2) of lot.1 is prepared; 4 DeclarationReceiptRecorded events, one per recorded receipt")
	c.deviate("the export date the case bound stays the release's after the amendment (C3a decision 3: no cascade from a later amendment to a bound case; the baseline names no path)")
}

// CN-AT07 客户合并付款，银行有扣费 — "原币资金与多单核销守恒，费用另有依据，未解释差额待处理" (B2a, B2b, C10;
// TestCombinedPaymentForTwoOrders): customer.1 owes order.1's P2 672.00 (the sales statement after the
// shipment) and 328.00 of a legacy order the cutover imported as an opening receivable (C10,
// opening_balance.record). It remits 1,000.00 for both; the bank credits 980.00 and its advice states
// 15.00 charges. Treasury records the remitted, received and stated charges apart (difference 20.00,
// unexplained); applies 672.00 to order.1; applying the legacy 328.00 is FUND_DIFFERENCE_UNEXPLAINED
// (only 308.00 is available). credit.manager.1 resolves 15.00 as our bank charge on the advice
// (fund.resolve_difference, not the recorder); treasury applies 323.00; 5.00 stays unexplained and the
// legacy receivable's 5.00 open — pending, never written off. Conservation: 1,000.00 remitted = 980.00
// received + 15.00 resolved + 5.00 unexplained; 995.00 applied = 672.00 + 323.00.
func TestCNATSuite07CombinedPaymentWithBankCharges(t *testing.T) {
	c := newCNAT(t, "CN-AT07")
	c.toLot()
	c.salesStatement() // D1 §5 step 19
	c.phaseOf("a legacy order's opening receivable")
	cutoff := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	c.source(brcAdmin, "ledger.legacy", "合成旧账截止导出：customer.1 旧订单 SO-LEGACY-0007 发票 INV-LEGACY-0007 未收 328.00 USD。")
	ob := c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordOpening, map[string]any{"opening_id": "ob.legacy.7", "kind": "receivable",
		"cutoff_at": kernel.FormatTimestamp(cutoff), "source": map[string]any{"document": "INV-LEGACY-0007",
			"issued_at": kernel.FormatTimestamp(cutoff.AddDate(0, 0, -30)), "amount": "328.00"},
		"counterparty_id": "customer.1", "order_reference": "SO-LEGACY-0007", "currency": cfgRef("currency", "USD", 1), "amount": "328.00",
		"installments": []any{map[string]any{"amount": "328.00", "due_at": kernel.FormatTimestamp(time.Now().Add(15 * 24 * time.Hour))}},
		"evidence_ids": []string{"ledger.legacy"}})
	if ob["status"] != "recorded" {
		c.t.Fatalf("opening receivable %v", ob)
	}
	if e := c.exposure(brcCreditOfficer); e != "100000" {
		c.t.Fatalf("exposure with the legacy receivable %s", e)
	}

	c.phaseOf("one remittance for two orders")
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund("fund.comb", stlapi.FundReceipt, map[string]any{"counterparty_id": "customer.1",
		"currency": cfgRef("currency", "USD", 1), "bank_account": c.bank, "received_amount": "980.00", "remitted_amount": "1000.00",
		"bank_charges": "15.00", "bank_reference": "MT103-CNAT07"}))
	f := c.fundView("fund.comb")
	if f["remitted_minor"] != "100000" || f["received_minor"] != "98000" || f["bank_charges_minor"] != "1500" || f["difference_minor"] != "2000" ||
		f["unexplained_minor"] != "2000" || f["available_minor"] != "98000" {
		c.t.Fatalf("combined receipt %v", f)
	}
	c.applyTo("app.comb.order", "fund.comb", "stmt.sales.1", "P2", "672.00")
	reason := c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, cnatApplyArgs("app.comb.legacy", "fund.comb", "ob.legacy.7", "", "328.00"),
		http.StatusConflict, stlapi.CodeFundDifferenceUnexplained, "")
	c.observe("合并付款: 1,000.00 remitted for order.1 P2 672.00 and legacy SO-LEGACY-0007 328.00 (opening receivable); received 980.00, charges 15.00 stated; difference 20.00 unexplained; 672.00 applied to order.1; the legacy 328.00 → %s (%s)",
		stlapi.CodeFundDifferenceUnexplained, reason)

	c.phaseOf("the bank charge resolved independently")
	c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandResolveDifference, map[string]any{"fund_record_id": "fund.comb", "expected_version": 1,
		"resolution": "bank_charge", "amount": "15.00", "evidence_ids": []string{"advice.fund.comb"}, "reason": "treasury tries its own resolution"},
		http.StatusForbidden, kernel.CodePermissionDenied, "")
	c.ok(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandResolveDifference, map[string]any{"fund_record_id": "fund.comb", "expected_version": 1,
		"resolution": "bank_charge", "amount": "15.00", "evidence_ids": []string{"advice.fund.comb"},
		"reason": "银行通知载明中间行与入账行扣费 15.00 USD，经批准由我方承担（合成）"})
	c.applyTo("app.comb.legacy", "fund.comb", "ob.legacy.7", "", "323.00")
	c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, cnatApplyArgs("app.comb.rest", "fund.comb", "ob.legacy.7", "", "5.00"),
		http.StatusConflict, stlapi.CodeFundDifferenceUnexplained, "")
	f = c.fundView("fund.comb")
	order, legacy := c.get(brcTreasury, "/api/brant/v1/stl/receivables/stmt.sales.1"), c.get(brcTreasury, "/api/brant/v1/stl/receivables/ob.legacy.7")
	if f["resolved_minor"] != "1500" || f["unexplained_minor"] != "500" || f["applied_minor"] != "99500" || f["available_minor"] != "0" ||
		order["open_minor"] != "0" || legacy["open_minor"] != "500" || legacy["applied_minor"] != "32300" {
		c.t.Fatalf("after the resolution: fund %v, order %v, legacy %v", f, order, legacy)
	}
	if kernel.MustDecimal("980.00").Add(kernel.MustDecimal("15.00")).Add(kernel.MustDecimal("5.00")).Cmp(kernel.MustDecimal("1000.00")) != 0 {
		c.t.Fatal("conservation")
	}
	if e := c.exposure(brcCreditOfficer); e != "500" {
		c.t.Fatalf("exposure after the combined payment %s", e)
	}
	c.observe("费用另有依据: treasury's own resolution → %s; credit.manager.1 resolves 15.00 as bank_charge on the advice; 323.00 applied to the legacy receivable; 5.00 more → %s",
		kernel.CodePermissionDenied, stlapi.CodeFundDifferenceUnexplained)
	c.observe("守恒: remitted 1,000.00 = received 980.00 + resolved 15.00 + unexplained 5.00 (pending); applied 995.00 = 672.00 (order.1, open 0) + 323.00 (legacy, open 5.00); exposure 5.00")
	c.deviate("\"多单\": the composed BRC-001 world holds one Brant order, so the second order is a legacy order imported at the cutover as an opening receivable (C10); the combined application rule is the same for both receivables")
}

// CN-AT08 付款方不同，或发生退汇／退款 — "核实付款依据及责任；实际资金与商业调整分别记录，不自动确认合规" (B2a;
// TestRefundsReturnsAndPayers): after the shipment customer.1 owes 672.00. payer.X (a third party) remits
// 672.00: treasury records it, but applying it to customer.1's receivable is PARTY_ENTITY_MISMATCH
// (pending, CN-R10); the bank returns the remittance as its own fund record drawn on it. customer.1
// then pays 680.00; 672.00 is applied, a refund of 9.00 is APPLICATION_EXCEEDS_AVAILABLE (only 8.00
// is unapplied), the refund of 8.00 is its own fund record. The receivable closes only on the
// customer's applied money; the third party's money is never applied and nothing marks it verified.
func TestCNATSuite08DifferentPayerReturnedRemittanceAndRefund(t *testing.T) {
	c := newCNAT(t, "CN-AT08")
	c.toLot()
	c.salesStatement() // D1 §5 step 19
	c.phaseOf("a third party's remittance")
	c.party(brcSales, "payer.X", []string{ptyapi.RoleCustomer}, "source.1")
	usd := cfgRef("currency", "USD", 1)
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund("fund.payer", stlapi.FundReceipt, map[string]any{"counterparty_id": "payer.X",
		"currency": usd, "bank_account": c.bank, "received_amount": "672.00", "remitted_amount": "672.00", "bank_reference": "MT103-PAYERX"}))
	reason := c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, cnatApplyArgs("app.payer", "fund.payer", "stmt.sales.1", "P2", "672.00"),
		http.StatusConflict, stlapi.CodePartyEntityMismatch, "")
	c.observe("付款方不同: payer.X's 672.00 recorded; applying it to customer.1's receivable → %s (%s): pending, the receivable stays open 672.00", stlapi.CodePartyEntityMismatch, reason)
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund("fund.payer.back", stlapi.FundReturnedRemittance,
		map[string]any{"source_fund_record_id": "fund.payer", "currency": usd, "bank_account": c.bank, "received_amount": "672.00", "bank_reference": "RET-PAYERX"}))
	p := c.fundView("fund.payer")
	if p["available_minor"] != "0" || p["applied_minor"] != "0" || p["drawn_minor"] != "67200" || len(brcList(p["draws"])) != 1 {
		c.t.Fatalf("the returned remittance %v", p)
	}
	c.observe("退汇: the bank returns payer.X's remittance as fund record fund.payer.back (returned_remittance) drawn on it: available 0, applied 0, drawn 672.00")

	c.phaseOf("the customer's payment and a refund")
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund("fund.cust", stlapi.FundReceipt, map[string]any{"counterparty_id": "customer.1",
		"currency": usd, "bank_account": c.bank, "received_amount": "680.00", "remitted_amount": "680.00", "bank_reference": "MT103-CUST-0008"}))
	c.applyTo("app.cust", "fund.cust", "stmt.sales.1", "P2", "672.00")
	refund := c.fund("fund.cust.refund", stlapi.FundRefund, map[string]any{"source_fund_record_id": "fund.cust", "currency": usd, "bank_account": c.bank,
		"received_amount": "9.00", "bank_reference": "TT-REFUND-0008"})
	c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, refund, http.StatusConflict, stlapi.CodeApplicationExceedsAvailable, "")
	refund["received_amount"] = "8.00"
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, refund)
	cust, rec := c.fundView("fund.cust"), c.get(brcTreasury, "/api/brant/v1/stl/receivables/stmt.sales.1")
	if cust["applied_minor"] != "67200" || cust["drawn_minor"] != "800" || cust["available_minor"] != "0" || rec["open_minor"] != "0" ||
		len(brcList(rec["applications"])) == 0 {
		c.t.Fatalf("after the refund: fund %v, receivable %v", cust, rec)
	}
	for _, a := range brcList(rec["applications"]) {
		if brcDig(a, "fund_record_id") == "fund.payer" {
			c.t.Fatalf("the third party's money applied %v", a)
		}
	}
	c.observe("退款: customer.1 pays 680.00, 672.00 applied (receivable 0); a refund of 9.00 → %s (8.00 unapplied); the 8.00 refund is its own fund record drawn on the receipt",
		stlapi.CodeApplicationExceedsAvailable)
	c.observe("分别记录: refunds and returns are fund records (direction out), never applications; payer.X's money is never applied and nothing marks it verified (no command verifies a third-party payer: B2a gap 6)")
	c.skipped("verifying a third-party payer's basis (\"核实付款依据及责任\") and the approved review mode: the design has no command for it (B2a decision 6); the remittance stays pending until returned")
}

// CN-AT09 外币结汇，再导入人民币入账 — "仅形成一次资金转换；不新增销售收入或第二次客户回款" (B2a;
// TestBaseline46CashSideAndConversion): order.1's P2 has been collected (670.00 received, 2.00 bank charge
// resolved, receivable 0). Treasury sells the 670.00 USD at the bank's 7.1 for CNY 4,757.00 as one fx
// conversion record (wrong arithmetic INVALID_KNOWN_VALUE). Importing the CNY bank line again as a
// customer receipt is BUSINESS_KEY_CONFLICT; a second sale of the same USD APPLICATION_EXCEEDS_AVAILABLE;
// the conversion is never applied (INVALID_STATE). The receivable, the order's net sales (960.00) and
// the USD receipt's applications are unchanged; only its cash is converted.
func TestCNATSuite09FXConversionThenCNYReceipt(t *testing.T) {
	c := newCNAT(t, "CN-AT09")
	c.toLot()
	c.salesStatement() // D1 §5 step 19
	c.collection()
	cnyRef, bankCNY := c.cny()
	c.phaseOf("one conversion")
	conv := c.fund("fx.p2", stlapi.FundFXConversion, map[string]any{"source_fund_record_id": "fund.p2", "currency": cnyRef, "bank_account": bankCNY,
		"received_amount": "4756.00", "bank_reference": "CNY-LINE-0009", "fx": map[string]any{"sold_amount": "670.00", "bank_rate": "7.1"}})
	c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, conv, http.StatusUnprocessableEntity, kernel.CodeInvalidKnownValue, "")
	conv["received_amount"] = "4757.00"
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, conv)
	x := c.fundView("fx.p2")
	if x["kind"] != stlapi.FundFXConversion || x["currency"] != "CNY" || x["received_minor"] != "475700" || brcStr(x["counterparty_id"]) != "" {
		c.t.Fatalf("conversion %v", x)
	}
	c.observe("结汇: fund.p2's 670.00 USD sold at 7.1 = CNY 4,757.00 as one fx_conversion record (4,756.00 → %s); no counterparty", kernel.CodeInvalidKnownValue)

	c.phaseOf("the CNY bank line imported again")
	c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund("fund.cny.line", stlapi.FundReceipt, map[string]any{
		"counterparty_id": "customer.1", "currency": cnyRef, "bank_account": bankCNY, "received_amount": "4757.00", "remitted_amount": "4757.00",
		"bank_reference": "CNY-LINE-0009"}), http.StatusConflict, stlapi.CodeBusinessKeyConflict, "")
	twice := c.fund("fx.p2.again", stlapi.FundFXConversion, map[string]any{"source_fund_record_id": "fund.p2", "currency": cnyRef, "bank_account": bankCNY,
		"received_amount": "4757.00", "bank_reference": "CNY-LINE-0010", "fx": map[string]any{"sold_amount": "670.00", "bank_rate": "7.1"}})
	c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, twice, http.StatusConflict, stlapi.CodeApplicationExceedsAvailable, "")
	c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, cnatApplyArgs("app.fx", "fx.p2", "stmt.sales.1", "P2", "1.00"),
		http.StatusUnprocessableEntity, kernel.CodeInvalidState, "")
	src := c.fundView("fund.p2")
	rec := c.get(brcTreasury, "/api/brant/v1/stl/receivables/stmt.sales.1")
	settlement := c.get(brcTreasury, "/api/brant/v1/stl/orders/order.1/settlement")
	if src["sold_minor"] != "67000" || src["cash_minor"] != "0" || src["applied_minor"] != "67200" || rec["applied_minor"] != "96000" || rec["open_minor"] != "0" ||
		settlement["net_sales_minor"] != "96000" {
		c.t.Fatalf("after the conversion: source %v, receivable %v, settlement %v", src, rec, settlement)
	}
	c.observe("再导入人民币入账: the CNY line as a customer receipt → %s (the conversion's bank reference on the CNY account); a second sale of the USD → %s; applying the conversion → %s",
		stlapi.CodeBusinessKeyConflict, stlapi.CodeApplicationExceedsAvailable, kernel.CodeInvalidState)
	c.observe("不新增收入或回款: fund.p2 sold 670.00 (cash 0) with its applications 672.00 unchanged; receivable 960.00 applied, open 0; order.1 net sales 960.00")
}

// CN-AT10 预计退税、核准与到账金额不同 — "三者分开；差异与成本／资金影响按冻结政策解释" (C3b;
// TestCNAT10ExpectedApprovedAndReceivedStayApart): lot.1's tax refund case (the verified invoice's 120 PCS
// allocated for export_refund) goes through every stage tax.officer records from the tax authority's
// synthetic papers: expected CNY 507.00 (a stated estimate, not computed), planned 507.00 for tax period
// 2026-11 with the professional reviewer, submitted (external reference), approved 480.00 refund plus
// 20.00 offset (免抵, not cash), and received 480.00 on treasury's CNY fund record from the tax bureau.
// A stage out of order is INVALID_STATE; a fund record in USD or for less than the amount is
// INVALID_KNOWN_VALUE. The case keeps each amount apart and shows the differences; nothing is
// computed, and the refund is a non-goods fund record, never a customer receipt.
func TestCNATSuite10ExpectedApprovedAndReceivedRefundDiffer(t *testing.T) {
	c := newCNAT(t, "CN-AT10")
	c.toLot()
	c.declaration()
	c.invoice()
	cnyRef, bankCNY := c.cny()
	cny := map[string]any{"id": "CNY", "revision": 1}
	var as []map[string]any
	for i, l := range brcLines {
		as = append(as, cnatAlloc(fmt.Sprintf("alloc.E%d", i+1), fmt.Sprintf("E%d", i+1), c.invoiceID, l.qty))
	}
	r := c.allocate("lot.1", as...)
	caseID, rv := brcStr(r["object_id"]), brcNum(r["row_version"])
	c.party(brcBuyer, "tax.bureau", []string{ptyapi.RoleSupplier}, "evidence")
	c.source(brcAdmin, "tax.submission", "合成退税申报回执：申报号 SB-CNAT10-0001（测试数据）。")
	c.source(brcAdmin, "tax.approval", "合成退税核准通知：核准退税 480.00 元，免抵 20.00 元（测试数据）。")
	c.phaseOf("stages recorded apart")
	result := func(stage string, extra map[string]any) map[string]any {
		a := map[string]any{"case_id": caseID, "expected_version": rv, "stage": stage}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	step := func(stage string, extra map[string]any) {
		c.t.Helper()
		out := c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxResult, result(stage, extra))
		rv = brcNum(out["row_version"])
	}
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxResult, result(cnxapi.TaxSubmitted, map[string]any{
		"external_reference": "SB-CNAT10-0001", "evidence_ids": []string{"tax.submission"}}), http.StatusUnprocessableEntity, kernel.CodeInvalidState, "")
	step(cnxapi.TaxExpected, map[string]any{"currency": cny, "amount": "507.00", "reason": "按合成口径估计（非税务结论）"})
	step(cnxapi.TaxPlanned, map[string]any{"currency": cny, "amount": "507.00", "tax_period": "2026-11",
		"review": map[string]any{"reviewer": "合成税务师（测试）", "evidence_ids": []string{"tax.review"}}})
	step(cnxapi.TaxSubmitted, map[string]any{"external_reference": "SB-CNAT10-0001", "evidence_ids": []string{"tax.submission"}})
	step(cnxapi.TaxApproved, map[string]any{"currency": cny, "amount": "480.00", "offset_amount": "20.00",
		"result_text": "合成核准通知：核准退税 480.00 元，免抵 20.00 元（测试数据）", "evidence_ids": []string{"tax.approval"}})
	c.phaseOf("the refund received on a fund record")
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund("fund.refund.usd", stlapi.FundNonGoods, map[string]any{"counterparty_id": "tax.bureau",
		"currency": cfgRef("currency", "USD", 1), "bank_account": c.bank, "received_amount": "480.00", "bank_reference": "TAX-USD-0010"}))
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund("fund.refund.small", stlapi.FundNonGoods, map[string]any{"counterparty_id": "tax.bureau",
		"currency": cnyRef, "bank_account": bankCNY, "received_amount": "470.00", "bank_reference": "TAX-CNY-0009"}))
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund("fund.refund", stlapi.FundNonGoods, map[string]any{"counterparty_id": "tax.bureau",
		"currency": cnyRef, "bank_account": bankCNY, "received_amount": "480.00", "bank_reference": "TAX-CNY-0010"}))
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxResult, result(cnxapi.TaxReceived, map[string]any{"currency": cny,
		"amount": "480.00", "fund_record_id": "fund.refund.usd"}), http.StatusUnprocessableEntity, kernel.CodeInvalidKnownValue, "/args/currency")
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxResult, result(cnxapi.TaxReceived, map[string]any{"currency": cny,
		"amount": "480.00", "fund_record_id": "fund.refund.small"}), http.StatusUnprocessableEntity, kernel.CodeInvalidKnownValue, "/args/amount")
	step(cnxapi.TaxReceived, map[string]any{"currency": cny, "amount": "480.00", "fund_record_id": "fund.refund"})
	v := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+caseID)
	a := brcMap(v["amounts"])
	if v["status"] != cnxapi.TaxReceived || a["currency_code"] != "CNY" || a["expected_minor"] != "50700" || a["planned_minor"] != "50700" ||
		a["approved_minor"] != "48000" || a["offset_minor"] != "2000" || a["received_minor"] != "48000" || a["approved_less_expected_minor"] != "-2700" ||
		a["received_less_approved_minor"] != "0" || len(brcList(v["results"])) != 5 {
		c.t.Fatalf("case %v", v)
	}
	if fr := c.fundView("fund.refund"); fr["kind"] != stlapi.FundNonGoods || fr["counterparty_id"] != "tax.bureau" || fr["applied_minor"] != "0" {
		c.t.Fatalf("the refund's fund record %v", fr)
	}
	c.observe("三者分开: expected 507.00, planned 507.00 (2026-11, reviewer), submitted SB-CNAT10-0001, approved 480.00 + offset 20.00 (免抵), received 480.00 on fund.refund (non_goods from tax.bureau); approved − expected = −27.00, received − approved = 0; 5 results")
	c.observe("refusals: submitted before planned → %s; received on a USD fund record → %s /args/currency; received on a 470.00 record → %s /args/amount",
		kernel.CodeInvalidState, kernel.CodeInvalidKnownValue, kernel.CodeInvalidKnownValue)
	c.skipped("the cost and fund impact of the differences \"按冻结政策解释\": CN-D02/CN-D07 are not frozen; Brant records the stated amounts and their differences and computes nothing (CN-E01)")
	c.deviate("the tax bureau is a synthetic PTY party of role supplier (PTY has customer and supplier roles only); the submission and approval papers are EVD sources registered by admin (the tax_specialist template holds no source.register: D1's gap)")
}

// CN-AT11 出口后折让或退运 — "销售、库存、报关及税务影响分别形成受控案件，不因贷项自动完成退税调整" (C8, C10;
// TestCNAT11CreditNoteAfterExportIsReflectedInCNX): after export, declaration, the sales statement, the
// invoice, the refund case (expected 78.00) and the complete document bundle, customer.1 claims a price
// allowance of 50.00. sales requests the credit note; reflecting it in CNX before its decision is
// INVALID_STATE; credit.manager.1 confirms it; treasury applies it to the receivable (open 622.00);
// tax.officer records its impact on lot.1's bundle as an external reconciliation of subject
// credit_note: customs and tax show 0.00, internal 50.00, difference −50.00, no adjustment. The
// declaration, the export lot, the shipment, the refund case and its allocations are unchanged.
func TestCNATSuite11AllowanceAfterExport(t *testing.T) {
	c := newCNAT(t, "CN-AT11")
	c.toLot()
	c.declaration()
	c.salesStatement() // D1 §5 step 19
	c.invoice()
	c.taxCase()
	c.bundle()
	c.phaseOf("credit note")
	c.source(brcSales, "claim.allowance", "合成客户索赔函：色差，协商折让 50.00 USD（合成）。")
	c.ok(brcSales, "stl", stlapi.Contract, stlapi.CommandRequestCreditNote, map[string]any{"credit_note_id": "cn.allowance", "kind": "price_allowance",
		"order_id": "order.1", "statement_id": "stmt.sales.1", "amount": "50.00", "basis": "色差折让，客户索赔函（合成）", "evidence_ids": []string{"claim.allowance"}})
	bundle := c.get(brcTaxOfficer, "/api/brant/v1/cnx/document_bundles/"+c.bundleID)
	resolve := map[string]any{"bundle_id": c.bundleID, "expected_version": bundle["row_version"], "reconciliation_id": "recon.allowance",
		"subject": map[string]any{"kind": "credit_note", "id": "cn.allowance"}, "measure": "amount", "currency": c.usdRef(),
		"external":       map[string]any{"value": "0.00", "source": "海关未改单、退税未调整（合成）", "evidence_ids": []string{"customs.receipt"}},
		"internal":       map[string]any{"value": "50.00", "source": "STL 贷项 cn.allowance"},
		"responsibility": map[string]any{"kind": "customer", "note": "客户索赔折让"}, "adjustment": map[string]any{"kind": "none"},
		"reason": "出口后折让：报关与退税资料未自动调整，差异待专业评估"}
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandResolveReconciliation, resolve, http.StatusUnprocessableEntity, kernel.CodeInvalidState, "")
	c.ok(brcCreditManager1, "stl", stlapi.Contract, stlapi.CommandConfirmCredit, map[string]any{"credit_note_id": "cn.allowance", "expected_version": 1,
		"decision": "confirm", "reason": "折让依据成立（合成）"})
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, map[string]any{"application_id": "app.allowance", "credit_note_id": "cn.allowance",
		"target": map[string]any{"kind": "receivable", "statement_id": "stmt.sales.1", "stage_id": "P2"}, "amount": "50.00"})
	rec := c.get(brcTreasury, "/api/brant/v1/stl/receivables/stmt.sales.1")
	settlement := c.get(brcTreasury, "/api/brant/v1/stl/orders/order.1/settlement")
	if rec["open_minor"] != "62200" || settlement["credited_minor"] != "5000" || settlement["net_sales_minor"] != "91000" {
		c.t.Fatalf("after the credit note: receivable %v, settlement %v", rec, settlement)
	}
	c.observe("贷项: sales requests cn.allowance 50.00 (price_allowance); reflecting it in CNX before its decision → %s; credit.manager.1 confirms; applied to the receivable: open 672.00 → 622.00; order.1 credited 50.00, net sales 910.00",
		kernel.CodeInvalidState)

	c.phaseOf("CNX impact as a controlled reconciliation")
	r := c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandResolveReconciliation, resolve)
	if r["status"] != "complete" {
		c.t.Fatalf("resolve %v", r)
	}
	bundle = c.get(brcTaxOfficer, "/api/brant/v1/cnx/document_bundles/"+c.bundleID)
	var link map[string]any
	var difference any
	for _, rc := range brcList(bundle["reconciliations"]) {
		if brcDig(rc, "reconciliation_id") == "recon.allowance" {
			link, difference = brcMap(brcDig(rc, "credit_note")), brcDig(rc, "difference")
		}
	}
	if link == nil || link["kind"] != "price_allowance" || link["amount_minor"] != "5000" || link["statement_id"] != "stmt.sales.1" || difference != "-50" {
		c.t.Fatalf("the credit note's reconciliation %v", bundle["reconciliations"])
	}
	decl := c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+c.declarationID)
	tc := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+c.taxCaseID)
	lot := c.get(brcMerchandiser, "/api/brant/v1/ful/export_lots/lot.1")
	f := c.get(brcMerchandiser, "/api/brant/v1/ful/orders/order.1/fulfilment")
	standing := 0
	for _, a := range brcList(tc["allocations"]) {
		if brcDig(a, "status") == cnxapi.EvidenceStanding {
			standing++
		}
	}
	if decl["status"] != "released" || len(brcList(decl["versions"])) != 1 || tc["status"] != cnxapi.TaxExpected || brcDig(tc, "amounts", "expected_minor") != "7800" ||
		standing != 4 || brcDig(lot, "totals", "net_minor") != "96000" || brcDig(f, "lines", 0, "shipped") != "24" {
		c.t.Fatalf("separate cases changed: declaration %v, case %v, lot %v", decl["status"], tc, lot["totals"])
	}
	c.observe("分别形成受控案件: tax.officer records the credit note on lot.1's bundle (subject credit_note, external 0.00, internal 50.00, difference −50.00, responsibility customer, no adjustment); the declaration stays released (1 version), the refund case expected 78.00 with 4 standing allocations, lot.1 960.00 and the shipment's 120 PCS unchanged — no automatic refund adjustment")
	c.skipped("退运 (a return of the goods): the stock route stays with STK (S6-01, decision D-10) and FUL records no return; a credit note of kind return would be commercial only")
}

// CN-AT12 企业适用政策变化且有历史未结业务 — "按有效期及专业核实结果处理，旧业务不被新配置覆盖" (A3a, C3b;
// TestCNAT12CaseBindsThePolicyInForceAtTheExportDate, TestExportTaxPolicyChangeKeepsTheRevisionOfOldExports): shipment.1
// goes into two lots. lot.a is released by customs and its case binds export.tax revision 1
// (exempt_credit_refund) with E1. The professional review changes the company's method: admin drafts
// revision 2 (exempt, exemption only; S5R-D14) and config.reviewer publishes it on the new review. lot.b,
// released after that, binds revision 2: export_refund is refused (INVALID_KNOWN_VALUE), the exemption
// is allocated. lot.a's open case keeps revision 1 and takes E2 for export_refund — the old business
// is not overwritten by the new configuration.
func TestCNATSuite12PolicyChangeWithOpenHistoricalBusiness(t *testing.T) {
	c := newCNAT(t, "CN-AT12")
	c.toShipped()
	c.invoice()
	c.phaseOf("lot.a under revision 1")
	c.lotOf("lot.a", c.shipmentID, [3]string{"E1", "S1", "24"}, [3]string{"E2", "S2", "36"})
	c.lotOf("lot.b", c.shipmentID, [3]string{"E1", "S3", "12"}, [3]string{"E2", "S4", "48"})
	c.declare("lot.a", "SYN-2026-000121", "SW-CNAT12-0001", "E1", "E2")
	ra := c.allocate("lot.a", cnatAlloc("alloc.a.E1", "E1", c.invoiceID, "24"))
	caseA := brcStr(ra["object_id"])
	c.phaseOf("the policy changes")
	cfg := c.get(brcAdmin, "/api/brant/v1/pol/configs/export_tax_policy/export.tax")
	// S5R-D14: the method without a refund is exempt (免税); POL refuses not_applicable on a new revision.
	c.source(brcAdmin, "tax.review.2", "合成专业复核记录（第二次）：自本日起企业适用方法改为“免税（不退税）”，仅供测试。")
	c.ok(brcAdmin, "pol", polapi.Contract, "config.save_draft", map[string]any{"kind": polapi.KindExportTaxPolicy, "id": "export.tax",
		"expected_version": cfg["root_version"], "valid_from": now(), "data": map[string]any{"name": "synthetic export tax policy (changed)",
			"refund_method": polapi.RefundExempt, "policy_basis": "合成政策依据（变更，测试用）", "professional_review_evidence_id": "tax.review.2"}})
	cfg = c.get(brcAdmin, "/api/brant/v1/pol/configs/export_tax_policy/export.tax")
	c.ok(brcController, "pol", polapi.Contract, "config.publish", map[string]any{"kind": polapi.KindExportTaxPolicy, "id": "export.tax",
		"expected_version": cfg["root_version"], "revision": 2, "evidence_ids": []string{"evidence", "tax.review.2"}, "reason": "按第二次专业复核变更适用方法"})
	if cfg = c.get(brcAdmin, "/api/brant/v1/pol/configs/export_tax_policy/export.tax"); cfg["active_revision"] != float64(2) {
		c.t.Fatalf("policy %v", cfg)
	}
	c.phaseOf("lot.b under revision 2, lot.a still revision 1")
	c.declare("lot.b", "SYN-2026-000122", "SW-CNAT12-0002", "E1", "E2")
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandAllocateTaxEvidence, cnatAllocArgs("lot.b", cnatAlloc("alloc.b.E1", "E1", c.invoiceID, "12")),
		http.StatusUnprocessableEntity, kernel.CodeInvalidKnownValue, "")
	exempt := cnatAlloc("alloc.b.E1x", "E1", c.invoiceID, "12")
	exempt["purpose"] = cnxapi.PurposeExportExemption
	rb := c.allocate("lot.b", exempt)
	c.allocate("lot.a", cnatAlloc("alloc.a.E2", "E2", c.invoiceID, "36"))
	a, b := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+caseA), c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+brcStr(rb["object_id"]))
	if brcDig(a, "policy", "revision") != float64(1) || brcDig(a, "policy", "refund_method") != polapi.RefundExemptCreditRefund || len(brcList(a["allocations"])) != 2 ||
		brcDig(b, "policy", "revision") != float64(2) || brcDig(b, "policy", "refund_method") != polapi.RefundExempt {
		c.t.Fatalf("cases: lot.a %v, lot.b %v", a["policy"], b["policy"])
	}
	c.observe("政策变化: export.tax revision 2 (exempt) published on its professional review while lot.a's case is open under revision 1 (exempt_credit_refund)")
	c.observe("旧业务不被覆盖: lot.a's case keeps revision 1 and takes E2 for export_refund after the change; lot.b, exported after it, binds revision 2: export_refund → %s, export_exemption allocated",
		kernel.CodeInvalidKnownValue)
}

// CN-AT13 提交超时，随后收到专业结果 — "先核实再重试，原业务操作唯一；无回执时不宣称办理成功" (C3a;
// TestCNAT13SubmissionTimeoutIsVerifiedBeforeAnythingIsSubmittedAgain): doc.1 submits lot.1's declaration
// manually under operation SW-CNAT13-0001; the single window times out and doc.1 records the result
// unknown with the reason. Submitting again is EXTERNAL_RESULT_UNKNOWN; the original submission's
// retry under its idempotency key is answered from its record (still one submission); no tax evidence
// can be allocated (no export date: INVALID_STATE) and nothing shows the goods released. The result
// found by verification — received, then released with the export date — is recorded on the same
// declaration and the case can then take its evidence.
func TestCNATSuite13SubmissionTimeoutThenVerifiedResult(t *testing.T) {
	c := newCNAT(t, "CN-AT13")
	c.toLot()
	c.invoice()
	c.phaseOf("submission and timeout")
	const number = "SYN-2026-000131"
	id, rv := c.declPrepare("lot.1", cnatItem(1, "E1", "E2", "E3", "E4"))
	rv = c.declConfirm(id, rv, 1)
	c.source(brcDocumentation, "sent.SW-CNAT13-0001", "合成报关单人工申报记录：单一窗口提交 lot.1，操作编号 SW-CNAT13-0001，提交后超时无回执。")
	submission := map[string]any{"declaration_id": id, "expected_version": rv, "operation_reference": "SW-CNAT13-0001",
		"submitter": map[string]any{"kind": "own_clerk", "name": brcDocumentation}, "submitted_at": now(), "evidence_ids": []string{"sent.SW-CNAT13-0001"}}
	c.keys++
	key := fmt.Sprintf("cnat.%03d", c.keys)
	_, s := c.send("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordSubmission, key, submission)
	if s["ok"] != true || s["status"] != "submitted" {
		c.t.Fatalf("submission %v", s)
	}
	c.source(brcDocumentation, "timeout.log", "合成操作日志：单一窗口提交后 30 分钟未返回回执，状态未知（合成）。")
	u := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, map[string]any{"declaration_id": id, "expected_version": s["row_version"],
		"receipt_id": "rcpt.unknown", "outcome": "unknown", "reason": "单一窗口提交超时，未收到回执", "evidence_ids": []string{"timeout.log"}})
	if u["status"] != "unknown" {
		c.t.Fatalf("unknown %v", u)
	}
	rv = brcNum(u["row_version"])
	c.source(brcDocumentation, "sent.SW-CNAT13-0002", "合成：拟重新提交（未核实前）。")
	resubmit := map[string]any{"declaration_id": id, "expected_version": rv, "operation_reference": "SW-CNAT13-0002",
		"submitter": map[string]any{"kind": "own_clerk", "name": brcDocumentation}, "submitted_at": now(), "evidence_ids": []string{"sent.SW-CNAT13-0002"}}
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordSubmission, resubmit, http.StatusConflict, cnxapi.CodeExternalResultUnknown,
		"/args/declaration_id")
	_, again := c.send("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordSubmission, key, submission)
	if again["ok"] != true || again["action_id"] != s["action_id"] {
		c.t.Fatalf("the original submission's retry %v / %v", again, s)
	}
	var as []map[string]any
	for i, l := range brcLines {
		as = append(as, cnatAlloc(fmt.Sprintf("alloc.E%d", i+1), fmt.Sprintf("E%d", i+1), c.invoiceID, l.qty))
	}
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandAllocateTaxEvidence, cnatAllocArgs("lot.1", as...), http.StatusUnprocessableEntity,
		kernel.CodeInvalidState, "/args/export_lot_id")
	v := c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+id)
	if v["status"] != "unknown" || v["customs_released"] != false || v["exported_at"] != nil || len(brcList(v["submissions"])) != 1 {
		c.t.Fatalf("while unknown %v", v)
	}
	c.observe("超时: submission SW-CNAT13-0001 recorded, the result recorded unknown with its reason; resubmission → %s /args/declaration_id; the original submission's retry under its key answered with the same action (one submission); tax allocation → %s (no export date); customs_released false",
		cnxapi.CodeExternalResultUnknown, kernel.CodeInvalidState)

	c.phaseOf("the verified result")
	r := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, c.receiptArgs(id, rv, "rcpt.received", "received", 1, number))
	r = c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, c.receiptArgs(id, brcNum(r["row_version"]), "rcpt.released", "released", 2, number))
	if r["status"] != "released" {
		c.t.Fatalf("released %v", r)
	}
	v = c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+id)
	receipts := brcList(v["receipts"])
	if len(brcList(v["submissions"])) != 1 || len(receipts) != 3 || brcDig(receipts, 0, "outcome") != "unknown" || brcDig(receipts, 0, "reason") == nil ||
		brcDig(receipts, 1, "from") != "unknown" || v["customs_released"] != true {
		c.t.Fatalf("after the verified result %v", v)
	}
	tc := c.allocate("lot.1", as...)
	c.observe("核实后: received (1) and released (2) recorded on the same declaration from the unknown state; one submission, three receipts; the case %v then takes its evidence", tc["object_id"])
}

// CN-AT14 单证缺件或证书不适用 — "列缺件、适用对象、责任人及期限；已上传文件不直接满足要求" (C3a, C5;
// TestCNAT14BundleListsWhatIsMissingAndPresenceIsNotValidity, TestCNAT14CertificateResultsApplyOnlyToTheirObjectMarketAndValidity):
// lot.1 (released by customs) needs a certificate of origin for Germany. doc.1's first check lists it
// missing with its owner (the documentation role) and due date: incomplete. The uploaded copy makes it
// present, not met: expecting completeness is DOCUMENT_REQUIREMENT_UNVERIFIED, and so is stating it
// verified without an official certificate result. A certificate registered for the US is
// CERTIFICATE_NOT_APPLICABLE (market), one of a single SKU leaves lot lines uncovered
// (CERTIFICATE_NOT_APPLICABLE); the certificate of the shipment for DE makes the bundle complete.
// CNX's applicability read answers the same.
func TestCNATSuite14MissingDocumentAndCertificateNotApplicable(t *testing.T) {
	c := newCNAT(t, "CN-AT14")
	c.toLot()
	c.declaration()
	c.phaseOf("the bundle lists what is missing")
	c.source(brcDocumentation, "docs.originals", "合成单证核对记录：商业发票、装箱单与提单与 FUL 已签发的单证一致。")
	base := []any{
		cnatRequirement("ci", "commercial_invoice", "verified", []string{"docs.originals"}, nil),
		cnatRequirement("pl", "packing_list", "verified", []string{"docs.originals"}, nil),
		cnatRequirement("bl", "bill_of_lading", "verified", []string{"docs.originals", "bl.release"}, nil),
		cnatRequirement("declaration", "customs_declaration", "verified", []string{"customs.receipt"}, map[string]any{"declaration_id": c.declarationID})}
	with := func(co map[string]any) []any { return append(append([]any{}, base...), co) }
	check := func(rv any, requirements []any, complete bool) map[string]any {
		return map[string]any{"export_lot_id": "lot.1", "expected_version": rv, "requirements": requirements, "expect_complete": complete}
	}
	co := cnatRequirement("co", "certificate_of_origin", "missing", nil, nil)
	r := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandCheckBundle, check(0, with(co), false))
	bundle, rv := brcStr(r["object_id"]), r["row_version"]
	if r["status"] != "incomplete" {
		c.t.Fatalf("first check %v", r)
	}
	v := c.get(brcDocumentation, "/api/brant/v1/cnx/document_bundles/"+bundle)
	missing := brcMap(brcDig(v, "requirements", 4))
	if missing["status"] != "missing" || brcDig(missing, "owner", "id") != "documentation" || missing["due_on"] == nil {
		c.t.Fatalf("the missing certificate %v", missing)
	}
	c.observe("缺件: certificate_of_origin listed missing with owner role documentation and due date %v; bundle incomplete", missing["due_on"])
	c.fileOf(brcDocumentation, "co.scan", "co-SYN-CO-0014.txt", "合成原产地证书扫描件：SYN-CO-0014，CCPIT（合成），货物 ST01，进口国 DE。")
	c.source(brcDocumentation, "co.copy", "合成：客户邮件转来的原产地证书副本（未核验）。")
	present := cnatRequirement("co", "certificate_of_origin", "present", []string{"co.copy"}, nil)
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandCheckBundle, check(rv, with(present), true), http.StatusConflict,
		cnxapi.CodeDocumentRequirementUnverified, "/args/requirements/4/status")
	attached := cnatRequirement("co", "certificate_of_origin", "verified", []string{"co.copy"}, nil)
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandCheckBundle, check(rv, with(attached), true), http.StatusConflict,
		cnxapi.CodeDocumentRequirementUnverified, "")
	c.observe("已上传文件不直接满足要求: the copy makes it present, expecting completeness → %s; verified on the attachment alone → %s", cnxapi.CodeDocumentRequirementUnverified,
		cnxapi.CodeDocumentRequirementUnverified)

	c.phaseOf("certificates and their applicability")
	c.source(brcDocumentation, "co.verify", "合成：贸促会网站核验页截图，证书真实（合成）。")
	register := func(number, market string, object map[string]any) string {
		at := time.Now().UTC()
		out := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRegisterCertificate, map[string]any{"expected_version": 0,
			"certificate_kind": "certificate_of_origin", "number": number, "issuer": "合成贸促会（测试）", "issued_on": at.Format(time.DateOnly), "object": object,
			"market": market, "valid_from": kernel.FormatTimestamp(at.Add(-time.Hour)), "valid_until": kernel.FormatTimestamp(at.AddDate(1, 0, 0)), "file_id": "co.scan",
			"basis": "原件核验（合成）", "evidence_ids": []string{"co.verify"}})
		return brcStr(out["object_id"])
	}
	us := register("SYN-CO-0014-US", "US", map[string]any{"kind": "shipment", "id": c.shipmentID})
	red := register("SYN-CO-0014-RS", "DE", map[string]any{"kind": "sku_spec", "sku_specs": []any{map[string]any{"sku_id": "ST01.red.S", "spec_revision": 1}}})
	de := register("SYN-CO-0014", "DE", map[string]any{"kind": "shipment", "id": c.shipmentID})
	byCertificate := func(ids ...string) map[string]any {
		return cnatRequirement("co", "certificate_of_origin", "verified", []string{"co.verify"}, map[string]any{"certificate_ids": ids})
	}
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandCheckBundle, check(rv, with(byCertificate(us)), true), http.StatusConflict,
		cnxapi.CodeCertificateNotApplicable, "/args/requirements/4/certificate_ids/0")
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandCheckBundle, check(rv, with(byCertificate(red)), true), http.StatusConflict,
		cnxapi.CodeCertificateNotApplicable, "/args/requirements/4/certificate_ids")
	usOn := c.get(brcDocumentation, "/api/brant/v1/cnx/certificates/"+us+"/applicability?target_kind=export_lot&target_id=lot.1")
	deOn := c.get(brcDocumentation, "/api/brant/v1/cnx/certificates/"+de+"/applicability?target_kind=export_lot&target_id=lot.1")
	if usOn["code"] != cnxapi.CodeCertificateNotApplicable || usOn["reason"] != "market" || deOn["applicable"] != true || deOn["reason"] != "applicable" {
		c.t.Fatalf("applicability: US %v, DE %v", usOn, deOn)
	}
	done := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandCheckBundle, check(rv, with(byCertificate(de)), true))
	if done["status"] != "complete" {
		c.t.Fatalf("complete %v", done)
	}
	v = c.get(brcDocumentation, "/api/brant/v1/cnx/document_bundles/"+bundle)
	link := brcMap(brcDig(v, "requirements", 4, "certificates", 0))
	if v["status"] != "complete" || link["certificate_id"] != de || link["market"] != "DE" || len(brcList(v["checks"])) != 2 {
		c.t.Fatalf("bundle %v", v)
	}
	c.observe("证书不适用: the US certificate → %s (market; applicability read: %v); the certificate of one SKU spec leaves lot lines uncovered → %s; the shipment's DE certificate %s → bundle complete (2 checks), the requirement linking certificate revision %v",
		cnxapi.CodeCertificateNotApplicable, usOn["reason"], cnxapi.CodeCertificateNotApplicable, de, link["revision"])
}

// CN-AT15 出口主体／受票或收款主体不一致 — "发现差异并转获准模式复核；不能通过外部主体字段绕过公司隔离" (C3b, B2a;
// TestCNAT15BuyerEntityMismatchStaysInReview, TestFundRecordsKeepRemittedReceivedAndChargesApart): supplier.A's
// invoice names another buyer entity: tax.officer's verification is PARTY_ENTITY_MISMATCH and the invoice
// stays imported (not verified, nothing matched); tax.officer rejects it with the reason and the
// supplier's invoice to the company is verified. A customer receipt credited to an account POL records
// as provider.1's, not the company's, is PARTY_ENTITY_MISMATCH and nothing is recorded. No request body
// can name another entity: a buyer party ID in the candidate and an exporter member in a declaration are
// SCHEMA_INVALID (the company is the token's), and company.B's tax officer does not see company.A's
// invoice (NOT_FOUND).
func TestCNATSuite15EntityMismatchExporterBuyerPayee(t *testing.T) {
	c := newCNAT(t, "CN-AT15")
	c.w.provision(bootstrap.Grant{Scope: c.w.scope("company.B"), Actor: actorRef("human", "tax.officer.B"), Permissions: []string{"cnx.read"}, DataScope: "company"})
	c.record.Actors["tax.officer.B"] = "税务专员（company.B）"
	c.toLot()
	c.phaseOf("invoice buyer entity")
	c.scanInvoice("scan.other", "33002611151", "120")
	other := cnatInvoice("33002611151", "blue", "120", "")
	other["buyer"] = map[string]any{"name": "合成关联企业（另一主体）", "tax_id": "OTHER-ENTITY-0001"}
	sneaky := cnatInvoice("33002611151", "blue", "120", "")
	sneaky["buyer"] = map[string]any{"party_id": "company.B", "name": "合成企业", "tax_id": "SYNTHETIC-NOT-REAL"}
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandImportInvoice, map[string]any{"file_id": "scan.other", "candidate": sneaky},
		http.StatusUnprocessableEntity, kernel.CodeSchemaInvalid, "/args/candidate/buyer/party_id")
	id, rv := c.importInvoice("human", brcTaxOfficer, "scan.other", other)
	var ms []map[string]any
	for _, l := range brcLines {
		ms = append(ms, cnatMatch(l.id, l.qty))
	}
	var ma []any
	for _, m := range ms {
		ma = append(ma, m)
	}
	reason := c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandVerifyInvoice, map[string]any{"purchase_invoice_id": id, "expected_version": rv,
		"decision": "verify", "facts": other, "matches": ma}, http.StatusConflict, stlapi.CodePartyEntityMismatch, "")
	if v := c.get(brcTaxOfficer, "/api/brant/v1/cnx/purchase_invoices/"+id); v["status"] != "imported" || len(brcList(v["matches"])) != 0 {
		c.t.Fatalf("the mismatched invoice %v", v)
	}
	c.readIn("company.B", "tax.officer.B", "/api/brant/v1/cnx/purchase_invoices/"+id, http.StatusNotFound)
	c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandVerifyInvoice, map[string]any{"purchase_invoice_id": id, "expected_version": rv,
		"decision": "reject", "reason": "受票主体非本公司（OTHER-ENTITY-0001），退回供应商重开（合成）"})
	c.scanInvoice("scan.ours", "33002611152", "120")
	ours := cnatInvoice("33002611152", "blue", "120", "")
	oid, orv := c.importInvoice("human", brcTaxOfficer, "scan.ours", ours)
	c.verifyInvoice(oid, orv, ours, ms...)
	c.observe("受票主体不一致: the invoice to OTHER-ENTITY-0001 → %s (%s) and stays imported, nothing matched; rejected with the reason; the reissued invoice to the company verified and matched 120",
		stlapi.CodePartyEntityMismatch, reason)
	c.observe("公司隔离: a buyer party_id in the candidate → %s /args/candidate/buyer/party_id; company.B's tax officer reads company.A's invoice → 404 NOT_FOUND", kernel.CodeSchemaInvalid)

	c.phaseOf("payee entity")
	providerBank := c.config(polapi.KindBankAccount, "bank.provider", map[string]any{"owner_kind": polapi.OwnerParty, "owner_id": "provider.1",
		"bank_name": "synthetic bank", "account_number": "6222000055556666", "currency": "USD", "purpose": polapi.AccountBoth})
	c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund("fund.provider", stlapi.FundReceipt, map[string]any{
		"counterparty_id": "customer.1", "currency": cfgRef("currency", "USD", 1), "bank_account": providerBank, "received_amount": "672.00",
		"remitted_amount": "672.00", "bank_reference": "MT103-PROVIDER"}), http.StatusConflict, stlapi.CodePartyEntityMismatch, "")
	c.read(brcTreasury, "/api/brant/v1/stl/fund_records/fund.provider", http.StatusNotFound)
	c.observe("收款主体不一致: a receipt credited to provider.1's account (POL owner party) → %s; nothing recorded (fund record 404)", stlapi.CodePartyEntityMismatch)

	c.phaseOf("exporter entity")
	c.no("human", brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandPrepareDeclaration, map[string]any{"expected_version": 0, "export_lot_id": "lot.1",
		"code_table_version": codeTable, "exporter": map[string]any{"company_id": "company.B"}, "items": []any{cnatItem(1, "E1", "E2", "E3", "E4")}},
		http.StatusUnprocessableEntity, kernel.CodeSchemaInvalid, "")
	c.observe("出口主体: a declaration naming an exporter → %s: the declaring entity is the token's company, never a body member", kernel.CodeSchemaInvalid)
	c.skipped("转获准模式复核 (review in an approved mode): no command exists (C3b decision 9, B2a decision 6); the mismatched invoice stays imported until rejected and the receipt is not recorded")
	c.skipped("an exporter other than the company (CN-M03 委托出口): out of CN-M01; the declaration has no exporter member to differ")
}

// CN-AT16 业务期初、未退税资料及未达账款导入 — "截止、来源余额及已办状态可核对；上线不重复生成现金、应付或用途占用"
// (C10; TestCNAT16OpeningBalancesReconcileAtTheCutoff, TestCNAT16OpeningRefundMaterialReconciles): at the
// cutover treasury records the old books' open items at the cutoff — a receivable of customer.1 (10,000.00
// issued, 7,000.00 open, two installments), a payable to supplier.A (2,400.00, 1,800.00 open) and a
// customer prepayment (5,000.00 received, 2,000.00 unapplied); importing the receivable's document again,
// even at a later cutoff, is OPENING_BALANCE_DUPLICATE, the prepayment's bank reference again
// BUSINESS_KEY_CONFLICT. The list at the cutoff reconciles source, settled before, open at the cutoff,
// applied since and open now; the credit exposure includes the opening receivable; a receipt after
// go-live is applied to it; only the prepayment is cash. tax.officer records the old tax register's
// refund material: invoice SYNOPEN1 with 960 PCS consumed before the cutoff (40 left: a post-cutoff
// allocation of 41 to lot.1 is TAX_EVIDENCE_EXCEEDED, 40 is allocated) and an export approved at the
// cutoff (CNY 390.00 on 300 PCS of SYNOPEN2, under the historic policy); duplicates are refused.
func TestCNATSuite16OpeningBalancesAndRefundMaterial(t *testing.T) {
	c := newCNAT(t, "CN-AT16")
	c.toLot()
	c.declaration()
	c.cny()
	c.phaseOf("STL opening balances at the cutoff")
	cutoff := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	at := kernel.FormatTimestamp(cutoff)
	c.source(brcAdmin, "ledger.cutover", "合成旧账截止导出：应收、应付与预收余额（截止日，测试数据）。")
	opening := func(id, kind, document, issuedAmount, amount string, extra map[string]any) map[string]any {
		a := map[string]any{"opening_id": id, "kind": kind, "cutoff_at": at, "source": map[string]any{"document": document,
			"issued_at": kernel.FormatTimestamp(cutoff.AddDate(0, 0, -40)), "amount": issuedAmount}, "currency": cfgRef("currency", "USD", 1), "amount": amount,
			"evidence_ids": []string{"ledger.cutover"}}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	due := func(days int) string {
		return kernel.FormatTimestamp(time.Now().Add(time.Duration(days) * 24 * time.Hour))
	}
	before := c.exposure(brcCreditOfficer)
	ar := opening("ob.ar.1", "receivable", "OB-AR-0016", "10000.00", "7000.00", map[string]any{"counterparty_id": "customer.1",
		"order_reference": "SO-LEGACY-0016", "installments": []any{map[string]any{"amount": "4000.00", "due_at": due(15)},
			map[string]any{"amount": "3000.00", "due_at": due(45)}}})
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordOpening, ar)
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordOpening, opening("ob.ap.1", "payable", "OB-AP-0016", "2400.00", "1800.00",
		map[string]any{"counterparty_id": "supplier.A", "installments": []any{map[string]any{"amount": "1800.00", "due_at": due(30)}}}))
	pre := opening("ob.pre.1", "customer_prepayment", "OB-PRE-0016", "5000.00", "2000.00", map[string]any{"counterparty_id": "customer.1",
		"bank_account": c.bank, "bank_reference": "OB-BANK-0016", "value_date": kernel.FormatTimestamp(cutoff.AddDate(0, 0, -5))})
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordOpening, pre)
	ar["opening_id"] = "ob.ar.2"
	c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordOpening, ar, http.StatusConflict, stlapi.CodeOpeningBalanceDuplicate, "/args/source/document")
	ar["opening_id"], ar["cutoff_at"] = "ob.ar.3", now()
	c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordOpening, ar, http.StatusConflict, stlapi.CodeOpeningBalanceDuplicate, "/args/source/document")
	pre["opening_id"], pre["source"] = "ob.pre.2", map[string]any{"document": "OB-PRE-0017", "issued_at": kernel.FormatTimestamp(cutoff.AddDate(0, 0, -40)), "amount": "100.00"}
	pre["amount"] = "100.00"
	c.no("human", brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordOpening, pre, http.StatusConflict, stlapi.CodeBusinessKeyConflict, "/args/bank_reference")
	after := c.exposure(brcCreditOfficer)
	if kernel.MustDecimal(after).Sub(kernel.MustDecimal(before)).String() != "700000" {
		c.t.Fatalf("exposure %s → %s", before, after)
	}
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, c.fund("fund.golive", stlapi.FundReceipt, map[string]any{"counterparty_id": "customer.1",
		"currency": cfgRef("currency", "USD", 1), "bank_account": c.bank, "received_amount": "1000.00", "remitted_amount": "1000.00",
		"bank_reference": "MT103-GOLIVE-0016"}))
	c.applyTo("app.golive", "fund.golive", "ob.ar.1", "", "1000.00")
	list := c.get(brcTreasury, "/api/brant/v1/stl/opening_balances?cutoff_at="+at)
	totals := map[string]map[string]any{}
	for _, row := range brcList(list["totals"]) {
		totals[brcStr(brcDig(row, "kind"))] = brcMap(row)
	}
	if len(brcList(list["items"])) != 3 || totals["receivable"]["source_amount_minor"] != "1000000" || totals["receivable"]["settled_before_minor"] != "300000" ||
		totals["receivable"]["amount_minor"] != "700000" || totals["receivable"]["applied_since_minor"] != "100000" || totals["receivable"]["open_minor"] != "600000" ||
		totals["payable"]["amount_minor"] != "180000" || totals["customer_prepayment"]["amount_minor"] != "200000" {
		c.t.Fatalf("opening list %v", list)
	}
	c.read(brcTreasury, "/api/brant/v1/stl/fund_records/ob.ar.1", http.StatusNotFound)
	if p := c.fundView("ob.pre.1"); p["received_minor"] != "200000" || p["available_minor"] != "200000" {
		c.t.Fatalf("the opening prepayment %v", p)
	}
	c.observe("期初余额: receivable OB-AR-0016 10,000.00 → 7,000.00 open (2 installments), payable OB-AP-0016 2,400.00 → 1,800.00, prepayment OB-PRE-0016 5,000.00 → 2,000.00 unapplied, cutoff %s", at)
	c.observe("防重: the receivable's document again → %s, at a later cutoff → %s; the prepayment's bank reference again → %s", stlapi.CodeOpeningBalanceDuplicate,
		stlapi.CodeOpeningBalanceDuplicate, stlapi.CodeBusinessKeyConflict)
	c.observe("可核对: list at the cutoff: receivable source 10,000.00, settled before 3,000.00, at cutoff 7,000.00, applied since 1,000.00 (go-live receipt), open 6,000.00; payable 1,800.00; prepayment 2,000.00; exposure +7,000.00 (%s → %s)",
		before, after)
	c.observe("不重复生成现金: the receivable and payable openings are statements without fund records (fund record ob.ar.1 404); the prepayment is the only opening cash (2,000.00 available)")

	c.phaseOf("CNX opening refund material")
	c.source(brcAdmin, "tax.register", "合成退税台账截止导出：进项发票剩余用途余额与在途退税申报（测试数据，非税务结论）。")
	c.source(brcAdmin, "approval.notice", "合成退税核准通知（截止前，测试数据）：核准退税 390.00 元。")
	c.cmd(brcAdmin, "pol", polapi.Contract, "config.save_draft", map[string]any{"kind": polapi.KindExportTaxPolicy, "id": "export.tax.hist", "expected_version": 0,
		"valid_from": kernel.FormatTimestamp(time.Now().AddDate(0, 0, -400)), "data": map[string]any{"name": "synthetic historic export tax policy",
			"refund_method": polapi.RefundExemptRefund, "policy_basis": "切换前适用的合成政策（测试用）", "professional_review_evidence_id": "tax.review"}})
	c.cmd(brcController, "pol", polapi.Contract, "config.publish", map[string]any{"kind": polapi.KindExportTaxPolicy, "id": "export.tax.hist", "expected_version": 1,
		"revision": 1, "evidence_ids": []string{"evidence", "tax.review"}, "reason": "切换时登记截止前适用的合成退税政策"})
	invoice := func(number string) string {
		c.scanInvoice("scan."+number, number, "1000")
		f := cnatInvoice(number, "blue", "1000", "")
		id, rv := c.importInvoice("human", brcTaxOfficer, "scan."+number, f)
		c.verifyInvoice(id, rv, f)
		return id
	}
	a := invoice("SYNOPEN1")
	balance := map[string]any{"opening_id": "op.a", "kind": "invoice_balance", "cutoff_at": at, "source_document": "TR-REG/0016",
		"evidence_ids": []string{"tax.register"}, "purchase_invoice_id": a, "balances": []any{map[string]any{"line_no": 1, "purpose": cnxapi.PurposeExportRefund,
			"consumed": "960"}}}
	c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxOpening, balance)
	if got := c.balance(a); got != "0/1000 available 40" {
		c.t.Fatalf("SYNOPEN1 after its opening %s", got)
	}
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandAllocateTaxEvidence, cnatAllocArgs("lot.1", cnatAlloc("alloc.E4.41", "E4", a, "41")),
		http.StatusConflict, cnxapi.CodeTaxEvidenceExceeded, "")
	c.allocate("lot.1", cnatAlloc("alloc.E4.40", "E4", a, "40"))
	balance["opening_id"] = "op.a2"
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxOpening, balance, http.StatusConflict, cnxapi.CodeOpeningBalanceDuplicate, "/args/source_document")
	balance["opening_id"], balance["source_document"] = "op.a3", "TR-REG/0017"
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxOpening, balance, http.StatusConflict, cnxapi.CodeOpeningBalanceDuplicate, "/args/purchase_invoice_id")
	b := invoice("SYNOPEN2")
	refundCase := map[string]any{"opening_id": "op.b", "kind": "refund_case", "cutoff_at": at, "source_document": "TR-REG/0018", "evidence_ids": []string{"tax.register"},
		"export": map[string]any{"reference": "223320260001234516", "exported_at": kernel.FormatTimestamp(cutoff.AddDate(0, 0, -30)), "order_reference": "SO-LEGACY-0016",
			"lines": []any{map[string]any{"line_id": "1", "quantity": "300", "unit": "PCS"}}},
		"policy_id": "export.tax.hist", "allocations": []any{map[string]any{"allocation_id": "oa.b", "export_line_id": "1", "purchase_invoice_id": b, "line_no": 1,
			"purpose": cnxapi.PurposeExportRefund, "quantity": "300"}},
		"position": map[string]any{"stage": cnxapi.TaxApproved, "currency": map[string]any{"id": "CNY", "revision": 1}, "amount": "390.00",
			"result_text": "合成核准通知：核准退税 390.00 元（测试数据）", "evidence_ids": []string{"approval.notice"}}}
	oc := c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxOpening, refundCase)
	if oc["status"] != cnxapi.TaxApproved {
		c.t.Fatalf("opening case %v", oc)
	}
	refundCase["opening_id"], refundCase["source_document"] = "op.b2", "TR-REG/0019"
	c.no("human", brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandRecordTaxOpening, refundCase, http.StatusConflict, cnxapi.CodeOpeningBalanceDuplicate,
		"/args/export/reference")
	if got := c.balance(b); got != "300/1000 available 700" {
		c.t.Fatalf("SYNOPEN2 %s", got)
	}
	openings := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_openings")
	if n := len(brcList(openings["tax_openings"])); n != 2 {
		c.t.Fatalf("tax openings %v", openings)
	}
	cases := c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases")
	var opened map[string]any
	for _, tc := range brcList(cases["tax_refund_cases"]) {
		if brcDig(tc, "export_lot_id") == "223320260001234516" {
			opened = c.get(brcTaxOfficer, "/api/brant/v1/cnx/tax_refund_cases/"+brcStr(brcDig(tc, "case_id")))
		}
	}
	if opened == nil || opened["status"] != cnxapi.TaxApproved || brcDig(opened, "opening", "opening_id") != "op.b" || brcDig(opened, "policy", "policy_id") != "export.tax.hist" ||
		brcDig(opened, "amounts", "approved_minor") != "39000" {
		c.t.Fatalf("the opening case %v", opened)
	}
	c.observe("未退税资料: SYNOPEN1 1,000 PCS with 960 consumed before the cutoff (op.a): available 40; after go-live 41 to lot.1 → %s, 40 allocated; the same register entry again → %s /args/source_document, another entry for the invoice → /args/purchase_invoice_id",
		cnxapi.CodeTaxEvidenceExceeded, cnxapi.CodeOpeningBalanceDuplicate)
	c.observe("已办状态: export 223320260001234516 approved at the cutoff (CNY 390.00, 300 PCS of SYNOPEN2) opens its case approved under export.tax.hist with its opening; SYNOPEN2 300/1000; a second opening of the export → %s /args/export/reference; 2 tax openings listed",
		cnxapi.CodeOpeningBalanceDuplicate)
}

// CN-AT17 中文操作、双语单证与盖章件修订 — "币种、金额、品名及条款对应；保存签署版本，草稿不能冒充已签署" (C2, TRD;
// TestBilingualDocumentsCorrespondAndSignedCopiesCNAT17): the API half — the UI ("中文操作") is outside the
// API repository. POL publishes zh-CN shipment document templates beside the English ones; doc.1 prepares
// shipment.1's commercial invoice and packing list in en and zh-CN from one content (goods named per
// language): number, currency, amounts, quantities, Incoterm and payment stages correspond, the labels
// are the language's. A signed copy of the draft is INVALID_STATE; after the issue the generated bytes
// are not a signed copy (INVALID_KNOWN_VALUE), an AI agent is refused (PERMISSION_DENIED), the stamped
// zh-CN scan is recorded once, another copy for the language is INVALID_STATE. sales records the
// countersigned copy of the order's sales contract (TRD). zh-CN data (company, addresses, goods,
// reasons) is kept as written; every refusal is a catalogued code whose Chinese text the client shows.
func TestCNATSuite17BilingualDocumentsAndSignedCopies(t *testing.T) {
	c := newCNAT(t, "CN-AT17")
	c.provisionAI(cnatAgent, fulapi.CommandRecordSignedShipmentDocument, "ful.read", "evd.read")
	c.toShipped()
	c.phaseOf("zh-CN templates")
	template := func(id, kind, language, title string) map[string]any {
		sections, _ := polapi.ShipmentDocumentSections(kind)
		for _, s := range []string{"consignee", "marks"} {
			if !slices.Contains(sections, s) {
				sections = append(slices.Clone(sections), s)
			}
		}
		return c.config(polapi.KindShipmentDocumentTemplate, id, map[string]any{"name": id, "document_kind": kind, "language": language,
			"renderer_version": polapi.ShipmentRendererVersion, "title": title, "sections": sections, "footer_note": "合成测试单证，非真实贸易"})
	}
	ciZh := template("ship.ci.zh", polapi.ShipmentDocumentCommercialInvoice, polapi.LanguageZhCN, "商业发票")
	plZh := template("ship.pl.zh", polapi.ShipmentDocumentPackingList, polapi.LanguageZhCN, "装箱单")
	var descriptions []any
	for _, l := range brcLines {
		descriptions = append(descriptions, map[string]any{"sku_id": l.sku, "language": fulapi.LanguageEn, "text": "Cotton knitted T-shirt " + l.sku},
			map[string]any{"sku_id": l.sku, "language": fulapi.LanguageZhCN, "text": "棉制针织T恤衫 " + l.sku})
	}
	particulars := map[string]any{"consignee": map[string]any{"name": "customer.1", "address": "合成买方地址\n汉堡"}, "marks": "CUSTOMER.1\nHAMBURG\nC/NO. 1-10",
		"port_of_loading": "CNNGB", "port_of_discharge": "DEHAM", "descriptions": descriptions}
	const set = "docs.shipment.1"
	prepare := func(expected int64, kind string, templates []any, sequence map[string]any) map[string]any {
		return c.ok(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandPrepareShipmentDocument, map[string]any{"document_set_id": set,
			"shipment_id": c.shipmentID, "expected_version": expected, "document_kind": kind, "templates": templates, "sequence_ref": sequence,
			"seller_ref": c.company, "particulars": particulars})
	}
	prepare(0, fulapi.DocumentCommercialInvoice, []any{c.ciEn, ciZh}, c.ciSeq)
	r := prepare(1, fulapi.DocumentPackingList, []any{c.plEn, plZh}, c.plSeq)
	rendition := func(kind, language string) string {
		v := c.get(brcDocumentation, fmt.Sprintf("/api/brant/v1/ful/document_sets/%s/documents/%s/versions/1/%s", set, kind, language))
		b, err := base64.StdEncoding.DecodeString(brcStr(v["content"]))
		c.w.must(err)
		return string(b)
	}
	en, zh := rendition(fulapi.DocumentCommercialInvoice, fulapi.LanguageEn), rendition(fulapi.DocumentCommercialInvoice, fulapi.LanguageZhCN)
	pen, pzh := rendition(fulapi.DocumentPackingList, fulapi.LanguageEn), rendition(fulapi.DocumentPackingList, fulapi.LanguageZhCN)
	number := brcStr(c.issuedOrDraft(set, fulapi.DocumentCommercialInvoice)["number"])
	for _, w := range []struct{ text, want string }{
		{en, "COMMERCIAL INVOICE\n"}, {zh, "商业发票\n"}, {en, "Seller: 合成企业\n"}, {zh, "卖方：合成企业\n"},
		{en, "Number: " + number + "\n"}, {zh, "编号：" + number + "\n"}, {en, "Currency: USD\n"}, {zh, "币种：USD\n"},
		{en, "Quantity: 120 PCS\n"}, {zh, "数量：120 PCS\n"}, {en, "Net: 960.00 | Tax: 0.00 | Total: 960.00"}, {zh, "净额：960.00 | 税额：0.00 | 合计：960.00"},
		{en, "Terms: FCA 2020"}, {zh, "贸易术语：FCA 2020"}, {en, "Cotton knitted T-shirt ST01.red.S"}, {zh, "棉制针织T恤衫 ST01.red.S"},
		{zh, "合成买方地址\n汉堡"}, {pen, "PACKING LIST\n"}, {pzh, "装箱单\n"}, {pzh, "箱数：10\n"},
	} {
		if !strings.Contains(w.text, w.want) {
			c.t.Fatalf("%q not in\n%s", w.want, w.text)
		}
	}
	for _, p := range [][2]*regexp.Regexp{{amountsEn, amountsZh}, {totalsEn, totalsZh}, {stagesEn, stagesZh}} {
		x, y := p[0].FindAllStringSubmatch(en, -1), p[1].FindAllStringSubmatch(zh, -1)
		if len(x) == 0 || len(x) != len(y) {
			c.t.Fatalf("%s: %v / %v", p[0], x, y)
		}
		for i := range x {
			if !slices.Equal(x[i][1:], y[i][1:]) {
				c.t.Fatalf("en %v, zh-CN %v", x[i], y[i])
			}
		}
	}
	c.observe("双语单证: CI %s and the PL rendered in en and zh-CN from one content: number, currency USD, quantity 120 PCS, amounts (per line, totals 960.00), FCA 2020 and the payment stages correspond; labels 商业发票/卖方/币种/净额 and goods 棉制针织T恤衫 per language",
		number)

	c.phaseOf("signed copies")
	c.fileOf(brcDocumentation, "ci.stamped.zh", "ci-stamped-zh.txt", "合成盖章件扫描：商业发票 "+number+" 中文版，加盖公司公章（合成）。")
	sign := func(file string) map[string]any {
		return map[string]any{"document_set_id": set, "document_kind": fulapi.DocumentCommercialInvoice, "version": 1, "language": fulapi.LanguageZhCN,
			"file_id": file, "evidence_ids": []string{"docs.check"}}
	}
	c.source(brcDocumentation, "docs.check", "合成：盖章件与系统生成版本逐项核对一致。")
	c.no("human", brcDocumentation, "ful", fulapi.Contract, fulapi.CommandRecordSignedShipmentDocument, sign("ci.stamped.zh"), http.StatusUnprocessableEntity,
		kernel.CodeInvalidState, "/args/version")
	c.ok(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandIssueShipmentDocuments, map[string]any{"document_set_id": set, "expected_version": r["row_version"]})
	c.fileOf(brcDocumentation, "ci.generated.zh", "ci-generated-zh.txt", zh)
	c.no("human", brcDocumentation, "ful", fulapi.Contract, fulapi.CommandRecordSignedShipmentDocument, sign("ci.generated.zh"), http.StatusUnprocessableEntity,
		kernel.CodeInvalidKnownValue, "/args/file_id")
	c.no("ai", cnatAgent, "ful", fulapi.Contract, fulapi.CommandRecordSignedShipmentDocument, sign("ci.stamped.zh"), http.StatusForbidden, kernel.CodePermissionDenied, "")
	if s := c.ok(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandRecordSignedShipmentDocument, sign("ci.stamped.zh")); s["status"] != fulapi.StatusSigned {
		c.t.Fatalf("signed %v", s)
	}
	c.ok(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandRecordSignedShipmentDocument, sign("ci.stamped.zh"))
	c.fileOf(brcDocumentation, "ci.stamped.zh.2", "ci-stamped-zh-2.txt", "另一份盖章件（合成）。")
	c.no("human", brcDocumentation, "ful", fulapi.Contract, fulapi.CommandRecordSignedShipmentDocument, sign("ci.stamped.zh.2"), http.StatusUnprocessableEntity,
		kernel.CodeInvalidState, "/args/language")
	ci := c.issuedOrDraft(set, fulapi.DocumentCommercialInvoice)
	copies := brcList(ci["signed_copies"])
	if len(copies) != 1 || brcDig(copies, 0, "language") != fulapi.LanguageZhCN || brcDig(copies, 0, "file_id") != "ci.stamped.zh" ||
		brcDig(copies, 0, "document_sha256") != sha256Hex(zh) {
		c.t.Fatalf("signed copies %v", ci)
	}
	c.observe("盖章件: a signed copy of the draft → %s /args/version; after the issue the generated zh-CN bytes as the signed copy → %s /args/file_id; the AI agent → %s; the stamped zh-CN scan recorded once (bound to the zh-CN rendition's SHA-256), again answered without a second copy; another copy for zh-CN → %s /args/language",
		kernel.CodeInvalidState, kernel.CodeInvalidKnownValue, kernel.CodePermissionDenied, kernel.CodeInvalidState)

	c.phaseOf("the sales contract's countersigned copy (TRD)")
	o := c.get(brcSales, "/api/brant/v1/trd/orders/order.1")
	c.ok(brcSales, "trd", trdapi.Contract, "order.document.generate", map[string]any{"order_id": "order.1", "expected_order_revision": 1,
		"expected_order_hash": o["order_hash"], "document_kind": trdapi.DocumentSalesContract, "template_ref": c.scTemplate, "sequence_ref": c.scSeq})
	c.fileOf(brcSales, "sc.countersigned", "sc-countersigned.txt", "合成销售合同回签件扫描：customer.1 签字、我司盖章（合成）。")
	c.source(brcSales, "sc.return", "合成：客户邮件寄回回签合同（合成）。")
	c.ok(brcSales, "trd", trdapi.Contract, "order.document.record_signed", map[string]any{"order_id": "order.1", "order_revision": 1,
		"document_kind": trdapi.DocumentSalesContract, "signature_kind": trdapi.SignatureCountersigned, "file_id": "sc.countersigned",
		"evidence": []any{map[string]any{"source_id": "sc.return", "locator": "回签邮件"}}})
	sc := c.get(brcSales, "/api/brant/v1/trd/orders/order.1/revisions/1/documents/"+trdapi.DocumentSalesContract)
	if brcDig(sc, "document", "signatures", 0, "signature_kind") != trdapi.SignatureCountersigned || brcDig(sc, "document", "signatures", 0, "file_id") != "sc.countersigned" {
		c.t.Fatalf("the sales contract %v", sc["document"])
	}
	c.observe("签署版本 (TRD): the sales contract %v of order revision 1 carries the countersigned scan sc.countersigned (S3's order documents)", brcDig(sc, "document", "number"))

	c.phaseOf("zh-CN data and error texts")
	party := c.get(brcSales, "/api/brant/v1/pty/parties/customer.1")
	if !strings.Contains(fmt.Sprint(party["addresses"]), "合成买方地址") || !cnatHan(brcStr(brcDig(c.get(brcSales, "/api/brant/v1/pol/configs/company_profile/company.A"),
		"revisions", 0, "data", "legal_name"))) {
		c.t.Fatalf("zh-CN data %v", party["addresses"])
	}
	catalog := cnatErrorCatalog(c.t)
	var texts []string
	for _, s := range c.record.Steps {
		if s.Status < 400 || s.Phase == "" || !strings.HasPrefix(s.Phase, c.id) {
			continue
		}
		e, ok := catalog[s.Code]
		if !ok || e.HTTP != s.Status || !cnatHan(e.Note) {
			c.t.Fatalf("refusal %s (%d) is not a catalogued code with a Chinese text: %+v", s.Code, s.Status, e)
		}
		texts = append(texts, s.Code+"："+e.Note)
	}
	c.fact("zh_cn_error_texts", texts)
	c.observe("中文数据与错误文本: company 合成企业, customer address 合成买方地址/汉堡 and goods names stored and printed as written; each refusal of the case is a catalogued code with its HTTP status and Chinese text (%d: %s)",
		len(texts), strings.Join(slices.Compact(slices.Sorted(slices.Values(texts))), "; "))
	c.skipped("中文操作 (the Chinese user interface): outside the API repository; the API answers stable codes and English reasons, the client shows the catalogue's Chinese text")
}

// CN-AT18 AI读取票据／报关资料后误匹配 — "显示来源与差异；候选须核实后进入相同业务命令，模型不能直接过账" (C3b, C3a;
// TestCNAT18CandidatesAreVerifiedByAHuman, TestDeclarationHumanOnlyCommandsRefuseAnAIActor): an AI agent
// holding every invoice, declaration and allocation permission reads supplier.A's invoice scan and imports
// a candidate with misread figures (12 PCS, 60.00 net). It cannot verify it (PERMISSION_DENIED: human only)
// nor allocate the unverified candidate (INVALID_STATE). tax.officer verifies the true facts through the
// same command: the invoice view shows each difference with its source (the AI's import, the human's
// verification). The agent prepares lot.1's declaration (a candidate draft); confirming it and recording a
// customs receipt are PERMISSION_DENIED; doc.1 confirms the same version. Nothing the model produced
// is posted without a human's verification.
func TestCNATSuite18AIReadsInvoicesAndDeclarations(t *testing.T) {
	c := newCNAT(t, "CN-AT18")
	c.provisionAI(cnatAgent, cnxapi.CommandImportInvoice, cnxapi.CommandVerifyInvoice, cnxapi.CommandAllocateTaxEvidence, cnxapi.CommandPrepareDeclaration,
		cnxapi.CommandConfirmDeclaration, cnxapi.CommandRecordReceipt, "cnx.read", "evd.read", "ful.read", "trd.read", "cat.read", "pur.read", "pol.read", "stl.read")
	c.toLot()
	c.phaseOf("the AI's invoice candidate")
	c.scanInvoice("invoice.scan", "33002611001", "120")
	misread := cnatInvoice("33002611001", "blue", "12", "")
	id, rv := c.importInvoice("ai", cnatAgent, "invoice.scan", misread)
	truth := cnatInvoice("33002611001", "blue", "120", "")
	var ms []any
	for _, l := range brcLines {
		ms = append(ms, cnatMatch(l.id, l.qty))
	}
	verify := map[string]any{"purchase_invoice_id": id, "expected_version": rv, "decision": "verify", "facts": truth, "matches": ms}
	c.no("ai", cnatAgent, "cnx", cnxapi.Contract, cnxapi.CommandVerifyInvoice, verify, http.StatusForbidden, kernel.CodePermissionDenied, "")
	c.no("ai", cnatAgent, "cnx", cnxapi.Contract, cnxapi.CommandAllocateTaxEvidence, cnatAllocArgs("lot.1", cnatAlloc("alloc.ai", "E1", id, "12")),
		http.StatusUnprocessableEntity, kernel.CodeInvalidState, "")
	c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandVerifyInvoice, verify)
	v := c.get(brcTaxOfficer, "/api/brant/v1/cnx/purchase_invoices/"+id)
	var fields []string
	for _, d := range brcList(v["differences"]) {
		if brcDig(d, "candidate_source", "actor", "kind") != "ai" || brcDig(d, "candidate_source", "step") != "import" ||
			brcDig(d, "verified_source", "actor", "id") != brcTaxOfficer {
			c.t.Fatalf("difference %v", d)
		}
		fields = append(fields, fmt.Sprintf("%v: %v → %v", brcDig(d, "field"), brcDig(d, "candidate"), brcDig(d, "verified")))
	}
	if v["status"] != "verified" || len(fields) == 0 || brcDig(v, "imported_by", "kind") != "ai" || len(brcList(v["matches"])) != 4 {
		c.t.Fatalf("invoice %v", v)
	}
	c.fact("differences", fields)
	c.observe("AI 误读: the agent imports a candidate of 12 PCS / 60.00; its verification → %s, its allocation of the candidate → %s; tax.officer verifies the true 120 PCS / 600.00 and the matches through the same command; the view shows %d differences, each with the AI's import and the human's verification as sources: %v",
		kernel.CodePermissionDenied, kernel.CodeInvalidState, len(fields), fields)

	c.phaseOf("the AI's declaration candidate")
	status, p := c.as("ai", cnatAgent, "cnx", cnxapi.Contract, cnxapi.CommandPrepareDeclaration, map[string]any{"expected_version": 0, "export_lot_id": "lot.1",
		"code_table_version": codeTable, "items": []any{cnatItem(1, "E1", "E2", "E3", "E4")}})
	if status != http.StatusOK || p["status"] != "draft" {
		c.t.Fatalf("the AI prepares %d %v", status, p)
	}
	decl := brcStr(p["object_id"])
	c.no("ai", cnatAgent, "cnx", cnxapi.Contract, cnxapi.CommandConfirmDeclaration, map[string]any{"declaration_id": decl, "expected_version": p["row_version"],
		"version": 1}, http.StatusForbidden, kernel.CodePermissionDenied, "")
	rv = c.declConfirm(decl, brcNum(p["row_version"]), 1)
	rv = c.declSubmit(decl, rv, "SW-CNAT18-0001")
	c.no("ai", cnatAgent, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, c.receiptArgs(decl, rv, "rcpt.ai", "released", 1, "SYN-2026-000181"),
		http.StatusForbidden, kernel.CodePermissionDenied, "")
	dv := c.get(brcDocumentation, "/api/brant/v1/cnx/declaration_cases/"+decl)
	if dv["status"] != "submitted" || brcDig(dv, "opened_by", "kind") != "ai" || brcDig(dv, "confirmations", 0, "confirmed_by", "id") != brcDocumentation {
		c.t.Fatalf("declaration %v", dv)
	}
	c.observe("AI 报关候选: the agent prepares lot.1's declaration (opened by ai); its confirmation and a customs receipt → %s; doc.1 confirms the same version and submits it",
		kernel.CodePermissionDenied)
}
