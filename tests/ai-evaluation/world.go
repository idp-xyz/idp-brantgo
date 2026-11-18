package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Contracts of the command envelope, as the contexts' api packages publish them. Root tests/ import
// no internal package (AC6), so the harness states the published values (as tests/load does).
const (
	contractEVD = "brant.evd/0.1.0"
	contractPTY = "brant.pty/0.1.0"
	contractCAT = "brant.cat/0.1.0"
	contractPOL = "brant.pol/0.1.0"
	contractINQ = "brant.inq/0.1.0"
	contractSUP = "brant.sup/0.1.0"
	contractCST = "brant.costing/0.1.0"
	contractTRD = "brant.quotation/0.1.0"
	contractSTL = "brant.settlement/0.1.0"
	contractCNX = "brant.china-export/0.1.0"
	contractREF = "brant.ref/0.1.0"
	contractTSK = "brant.task/0.1.0"
	contractAI  = "brant.ai/0.1.0"

	audience = "brant-api"
	company  = "company.A"
)

// A trial's people (event storming ES-01 actors). The setup people hold BRC-001's direct grants as
// tests/load gives them; the people of the AI task act under their role templates (the owner
// sales_clerk; the reviewers quote_reviewer and order_reviewer plus reviewerExtra, D1-D10), the
// agent under ai_q01_agent.
const (
	actorClerk         = "clerk.1"
	actorAgent         = "agent.q01"
	actorSales         = "sales"
	actorAdmin         = "admin"
	actorController    = "config.reviewer"
	actorCompliance    = "compliance"
	actorBuyer         = "buyer"
	actorCostChecker   = "cost.checker"
	actorDocumentation = "doc.1"
	actorDocVerifier   = "doc.2"
	actorCreditManager = "credit.manager.1"
	actorCreditSecond  = "credit.manager.2"
	actorQuoteReviewer = "quote.reviewer"
	actorOrderReviewer = "order.reviewer"
	actorPlatform      = "eval.platform" // system: TSK's system transitions no consumer drives (D1-D06)
)

// setupPermissions is what tests/load grants BRC-001's setup people (Python's "*" for S1–S3).
var setupPermissions = []string{
	"source.register", "file.register", "evd.read",
	"party.save_draft", "party.confirm", "party.revise", "contact.save_draft", "contact.confirm", "contact.revise",
	"service_role.verify", "trading_profile.save_draft", "trading_profile.publish", "pty.read",
	"sku.save_draft", "sku.confirm", "sku.revise_spec", "sku.deactivate", "cat.read",
	"config.save_draft", "config.publish", "config.retire", "number.issue", "foundation.snapshot",
	"config.publish.credit_policy", "config.publish.export_tax_policy", "config.publish.screening_regulation",
	"pol.read", "pol.read_sensitive",
	"rfq.save_draft", "rfq.confirm_scope", "inq.read",
	"party_screening.record", "party_screening.decide", "screening_list.import", "ref.read",
	"costing_context.save_draft", "costing_context.verify", "cst.read",
	"cost_scenario.save_draft", "cost_scenario.evaluate", "cost_snapshot.freeze",
	"supplier_rfq.save_draft", "supplier_rfq.record_issued", "supplier_offer.save_draft", "supplier_offer.verify",
	"logistics_offer.save_draft", "logistics_offer.verify", "sup.read",
	"trd.read", "tsk.read",
	"declaration_profile.save_draft", "declaration_profile.verify",
	"trade_compliance.evaluate", "trade_compliance.decide", "cnx.read", "country_programme.import",
	"credit_limit.request", "stl.read",
}

// platformPermissions are the system transitions of SM-TSK-TASK the evaluation's platform actor
// drives over HTTP (S4 C3 report, "What D1 calls"): created→scoping, and resuming a waiting task
// when the input it waits for arrived where no consumer tells TSK (an RFQ confirmed, a cost
// snapshot frozen, a customer message filed), and verifying the goal of a manual task, which no
// runner serves (AN08). goal.verify of an AI task stays the runner's.
var platformPermissions = []string{"task.scope", "task.resume", "task.verify", "goal.verify", "tsk.read", "trd.read", "inq.read", "cst.read",
	"evd.read", "sup.read", "cat.read", "pol.read", "pty.read"}

// brcLines are BRC-001's lines: ST01 red/blue × S/M, 120 PCS at 8.00 USD = 960.00.
var brcLines = []struct{ id, sku, item, qty string }{
	{"L1", "ST01.red.S", "knitted T-shirt red S", "24"}, {"L2", "ST01.red.M", "knitted T-shirt red M", "36"},
	{"L3", "ST01.blue.S", "knitted T-shirt blue S", "12"}, {"L4", "ST01.blue.M", "knitted T-shirt blue M", "48"},
}

func lineIDs() []string {
	ids := make([]string, len(brcLines))
	for i, l := range brcLines {
		ids[i] = l.id
	}
	return ids
}

const (
	codeTable      = "CN-2026"
	cnProgrammes   = "cn.country_programmes"
	unProgrammes   = "un.country_programmes"
	clearCountry   = "DE"
	creditReason   = "合成信用测试，非真实财务数据"
	creditLimitUSD = "50000000.00"
	unitPrice      = "8.00"
)

// world is the synthetic BRC-001 front every trial builds on: POL configurations, customer.1 with
// its trading profile and clear screening, the four SKUs with verified declaration profiles,
// provider.1's verified forwarding role, the programme lists and customer.1's credit limit.
type world struct {
	from, until                      time.Time
	usd, pcs, zero, freight, service map[string]any
	payment, trade, policy, issuer   map[string]any
	template, seq, credit, place     map[string]any
	goods, sale                      map[string]map[string]any
	customerRevision                 int64
	setupSource, evidence            string
}

// setupRun stops at the first failure and remembers where.
type setupRun struct {
	ctx  context.Context
	ops  map[string]*actor
	err  error
	step string
}

func (s *setupRun) cmd(actorID, bc, contract, command string, args map[string]any) map[string]any {
	if s.err != nil {
		return map[string]any{}
	}
	out, err := s.ops[actorID].cmd(s.ctx, bc, contract, command, args)
	if err != nil {
		s.err, s.step = err, bc+" "+command
		return map[string]any{}
	}
	return out
}

func (s *setupRun) get(actorID, path string) map[string]any {
	if s.err != nil {
		return map[string]any{}
	}
	out, err := s.ops[actorID].get(s.ctx, path)
	if err != nil {
		s.err, s.step = err, "GET "+path
		return map[string]any{}
	}
	return out
}

func (s *setupRun) fail(step string, got any) {
	if s.err == nil {
		s.err, s.step = fmt.Errorf("unexpected answer %v", got), step
	}
}

func (s *setupRun) result() error {
	if s.err != nil {
		return fmt.Errorf("%s: %w", s.step, s.err)
	}
	return nil
}

// setupWorld builds the world in the run's fresh tenant through the HTTP API, as
// TestBRC001FullChainOverHTTP and tests/load do up to the RFQ (which the trials' agents draft).
func setupWorld(ctx context.Context, ops map[string]*actor) (*world, error) {
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	w := &world{from: from, until: from.Add(14 * 24 * time.Hour), goods: map[string]map[string]any{}, sale: map[string]map[string]any{},
		place: map[string]any{"kind": "location", "id": "DEHAM", "revision": 1}, setupSource: "source.setup", evidence: "evidence"}
	s := &setupRun{ctx: ctx, ops: ops}
	s.foundation(w)
	s.front(w)
	s.customerReady(w)
	s.provider(w)
	s.compliance(w)
	s.creditLimit(w)
	return w, s.result()
}

func (s *setupRun) source(actorID, id, text string) {
	s.cmd(actorID, "evd", contractEVD, "source.register", map[string]any{"id": id, "source_kind": "manual_text", "content_text": text})
}

// config has admin draft kind/id with data and config.reviewer publish revision 1 on the evidence.
func (s *setupRun) config(w *world, kind, id string, data map[string]any) map[string]any {
	s.cmd(actorAdmin, "pol", contractPOL, "config.save_draft", map[string]any{"kind": kind, "id": id, "expected_version": 0,
		"valid_from": stamp(w.from), "data": data})
	s.cmd(actorController, "pol", contractPOL, "config.publish", map[string]any{"kind": kind, "id": id, "expected_version": 1,
		"revision": 1, "evidence_ids": []string{w.evidence}, "reason": "核实合成评测配置"})
	return map[string]any{"kind": kind, "id": id, "revision": 1}
}

func (s *setupRun) party(actorID, id string, roles []string, evidence string, addresses ...any) {
	args := map[string]any{"id": id, "expected_version": 0, "name": id, "roles": roles}
	if len(addresses) > 0 {
		args["addresses"] = addresses
	}
	s.cmd(actorID, "pty", contractPTY, "party.save_draft", args)
	s.cmd(actorID, "pty", contractPTY, "party.confirm", map[string]any{"id": id, "expected_version": 1, "evidence_ids": []string{evidence}})
}

func (s *setupRun) foundation(w *world) {
	s.source(actorAdmin, w.evidence, "合成核实依据：配置、主体、供应条件与核价的核实记录（AI 评测合成样例，非真实交易）。")
	w.usd = s.config(w, "currency", "USD", map[string]any{"code": "USD", "name": "US Dollar", "minor_units": 2, "rounding": "HALF_UP"})
	w.pcs = s.config(w, "unit", "PCS", map[string]any{"code": "PCS", "name": "件", "dimension": "count", "precision": 0})
	w.zero = s.config(w, "tax_profile", "tax.zero", map[string]any{"name": "SYNTHETIC ZERO", "rate": "0", "price_basis": "exclusive",
		"recovery_treatment": "nonrecoverable", "policy_basis": "test only, not a legal determination"})
	w.freight = s.config(w, "fee_category", "freight", map[string]any{"name": "海运费", "category": "freight", "side": "purchase_cost",
		"inclusion": "separate", "allocation_basis": "quantity", "policy_basis": "synthetic"})
	for _, l := range brcLines {
		for _, side := range []struct {
			name string
			apps map[string]map[string]any
		}{{"purchase", w.goods}, {"sales", w.sale}} {
			side.apps[l.sku] = s.config(w, "tax_application", side.name+"."+l.sku, map[string]any{"name": side.name + " " + l.sku,
				"side": side.name, "subject_kind": "sku", "subject_id": l.sku, "tax_profile": w.zero, "jurisdiction": "CN", "basis_note": "synthetic"})
		}
	}
	w.service = s.config(w, "tax_application", "tax.service", map[string]any{"name": "service", "side": "purchase",
		"subject_kind": "fee_category", "subject_id": "freight", "party_id": "provider.1", "tax_profile": w.zero, "jurisdiction": "CN",
		"basis_note": "synthetic"})
	w.payment = s.config(w, "payment_terms", "payment.30-70", map[string]any{"name": "30% on order confirmation, 70% 30 days after shipment",
		"day_basis": "calendar", "stages": []any{
			map[string]any{"stage_id": "P1", "percent": "30", "trigger": "order_confirmed", "days": 0},
			map[string]any{"stage_id": "P2", "percent": "70", "trigger": "after_shipment", "days": 30}}})
	w.trade = s.config(w, "trade_terms", "FCA", map[string]any{"incoterm": "FCA", "edition": "2020", "named_place": "TEST NAMED PLACE",
		"responsibility_note": "synthetic evidence only", "delivery": map[string]any{"kind": "fixed_date", "date": "2027-01-31"}})
	w.policy = s.config(w, "pricing_policy", "policy.1", map[string]any{"name": "synthetic BRC-001 policy", "calculation_currency": "USD",
		"minimum_margin": "0.25", "target_margin": "0.3125", "quote_valid_days": 7, "max_fx_age_seconds": 3600, "tax_profile": w.zero})
	w.issuer = s.config(w, "company_profile", company, map[string]any{"legal_name": "合成企业", "registration_no": "SYNTHETIC-NOT-REAL",
		"country": "CN", "address": "合成地址", "base_currency": "USD", "time_zone": "Asia/Shanghai", "policy_scope": "synthetic general trade test"})
	w.template = s.config(w, "quote_document_template", "quote.template", map[string]any{"name": "formal", "language": "en",
		"renderer_version": "quote-text/0.1.0", "title": "Quotation",
		"sections":    []string{"issuer", "recipient", "identity", "lines", "totals", "terms", "delivery", "payments", "validity", "notes"},
		"footer_note": "合成测试，不构成要约"})
	w.seq = s.config(w, "number_sequence", "quote.seq", map[string]any{"document_type": "quote", "prefix": "QT-", "width": 6,
		"reset": "annual", "time_zone": "Asia/Shanghai"})
	s.config(w, "screening_regulation", "screening", map[string]any{"lists": []any{map[string]any{"list_code": "cn.uel", "jurisdiction": "CN"}},
		"rescreen_interval_days": 30, "programmes": []any{map[string]any{"list_code": cnProgrammes, "jurisdiction": "CN"},
			map[string]any{"list_code": unProgrammes, "jurisdiction": "UN"}}})
}

// front confirms customer.1, its contact and the four SKUs (the RFQ is the trials' own).
func (s *setupRun) front(w *world) {
	s.source(actorSales, w.setupSource, "合成样例：customer.1 主数据与 ST01 四个 SKU（AI 评测合成数据）。")
	s.party(actorSales, "customer.1", []string{"customer"}, w.setupSource,
		map[string]any{"address_id": "hq", "role": "buyer", "address": "合成买方地址\n汉堡"},
		map[string]any{"address_id": "ap", "role": "billing", "address": "合成账单地址"})
	s.cmd(actorSales, "pty", contractPTY, "contact.save_draft", map[string]any{"id": "contact.1", "expected_version": 0, "name": "contact.1",
		"party_id": "customer.1"})
	s.cmd(actorSales, "pty", contractPTY, "contact.confirm", map[string]any{"id": "contact.1", "expected_version": 1,
		"evidence_ids": []string{w.setupSource}})
	w.customerRevision = num(s.get(actorSales, "/api/brant/v1/pty/parties/customer.1")["revision"])
	for _, l := range brcLines {
		s.sku(w, l.sku)
	}
}

func (s *setupRun) sku(w *world, id string) {
	s.cmd(actorSales, "cat", contractCAT, "sku.save_draft", map[string]any{"id": id, "expected_version": 0, "name": id, "base_uom": "PCS"})
	s.cmd(actorSales, "cat", contractCAT, "sku.confirm", map[string]any{"id": id, "expected_version": 1, "evidence_ids": []string{w.setupSource}})
}

func (s *setupRun) customerReady(w *world) {
	s.cmd(actorSales, "pty", contractPTY, "trading_profile.save_draft", map[string]any{"id": "customer.1", "expected_version": 0,
		"party_revision": w.customerRevision, "payment_terms": w.payment, "trade_terms": w.trade,
		"owner": map[string]any{"actor_id": actorSales, "team_id": "trade"}, "contact_refs": []any{map[string]any{"id": "contact.1", "row_version": 2}}})
	s.cmd(actorController, "pty", contractPTY, "trading_profile.publish", map[string]any{"id": "customer.1", "expected_version": 1, "revision": 1,
		"evidence_ids": []string{w.evidence}, "reason": "核实合成交易档案"})
	sum := sha256.Sum256([]byte("synthetic BRC-001 screening list cn.uel v1"))
	s.cmd(actorCompliance, "ref", contractREF, "screening_list.import", map[string]any{"list_code": "cn.uel", "jurisdiction": "CN",
		"version": "v1", "source_name": "Synthetic list cn.uel", "source_url": "https://lists.example.org/cn.uel", "published_on": "2026-11-01",
		"file_sha256": hex.EncodeToString(sum[:]), "entry_count": 1,
		"entries": []any{map[string]any{"entry_id": "E1", "primary_name": "Globex Corporation", "entity_type": "entity"}}})
	r := s.cmd(actorCompliance, "pty", contractPTY, "party_screening.record", map[string]any{"id": "customer.1", "expected_version": 0,
		"regulation_id": "screening", "regulation_revision": 1})
	if s.err == nil && r["status"] != "clear" {
		s.fail("pty party_screening.record", r)
	}
}

// provider confirms supplier.A and provider.1 with its verified forwarding role.
func (s *setupRun) provider(w *world) {
	s.source(actorBuyer, "supplier.quotes", "合成供应商报价：supplier.A 5.00 USD/PCS，ST01 四个规格，MOQ 100 PCS 按 ST01 款号合计；"+
		"provider.1 海运 CNSHA→DEHAM 60.00 USD。")
	s.party(actorBuyer, "supplier.A", []string{"supplier"}, w.evidence)
	s.party(actorBuyer, "provider.1", []string{"supplier"}, w.evidence)
	s.cmd(actorBuyer, "pty", contractPTY, "service_role.verify", map[string]any{"id": "provider.1", "expected_version": 2,
		"service_role_id": "role.1", "role": "forwarder", "service_code": "sea.fcl", "origin": "CNSHA", "destination": "DEHAM",
		"valid_from": stamp(w.from), "valid_until": stamp(w.until), "evidence_ids": []string{w.evidence}})
}

// compliance imports the synthetic programme lists and has doc.1 classify each SKU and doc.2 verify it.
func (s *setupRun) compliance(w *world) {
	lists := []struct {
		code, jurisdiction string
		programmes         []any
		countries          int
	}{
		{cnProgrammes, "CN", []any{
			map[string]any{"programme_id": "SYN-CN-EMB-1", "name": "合成全面禁运项目（测试数据）", "measure": "embargo", "countries": []string{"AQ"},
				"scope": "全部货物与服务（合成）"},
			map[string]any{"programme_id": "SYN-CN-EXP-1", "name": "合成出口管制项目（测试数据）", "measure": "export_control",
				"countries": []string{"BV", "AQ"}, "scope": "合成两用物项清单所列物项"}}, 2},
		{unProgrammes, "UN", []any{
			map[string]any{"programme_id": "SYN-UN-ARM-1", "name": "Synthetic arms embargo (test data)", "measure": "arms_embargo",
				"countries": []string{"HM", "BV"}, "scope": "Arms and related materiel (synthetic)"}}, 2},
	}
	for _, l := range lists {
		sum := sha256.Sum256([]byte("synthetic programme file " + l.code + " v1"))
		s.cmd(actorCompliance, "ref", contractREF, "country_programme.import", map[string]any{"list_code": l.code, "jurisdiction": l.jurisdiction,
			"version": "v1", "source_name": "Synthetic country programmes " + l.code + " (test data, not a legal list)",
			"source_url": "https://lists.example.org/" + l.code, "published_on": "2026-11-01", "file_sha256": hex.EncodeToString(sum[:]),
			"programme_count": len(l.programmes), "country_count": l.countries, "programmes": l.programmes})
	}
	for _, l := range brcLines {
		s.declaration(w, l.sku)
	}
}

func (s *setupRun) declaration(w *world, sku string) {
	id := "decl." + sku
	s.cmd(actorDocumentation, "cat", contractCAT, "declaration_profile.save_draft", map[string]any{"id": id, "expected_version": 0,
		"sku_id": sku, "spec_revision": 1, "code_table_version": codeTable,
		"commodity":        map[string]any{"code": "6109100021", "basis": "合成：针织棉制 T 恤，按合成编码表归类"},
		"declaration_name": "棉制针织T恤衫",
		"elements":         []any{map[string]any{"name": "品名", "value": "T恤衫"}, map[string]any{"name": "成分含量", "value": "100%棉"}},
		"legal_units":      []any{map[string]any{"unit": "件", "per_base_unit": "1", "basis": "一件即一 PCS"}},
		"origin":           map[string]any{"country": "CN", "criterion": "wholly_obtained", "basis": "合成：境内生产与缝制"},
		"export_control":   map[string]any{"status": "not_controlled", "basis": "合成：不在合成管制清单内"}})
	s.cmd(actorDocVerifier, "cat", contractCAT, "declaration_profile.verify", map[string]any{"id": id, "expected_version": 1,
		"revision": 1, "evidence_ids": []string{w.evidence}, "reason": "单证专业核实（合成）"})
}

// creditLimit publishes the synthetic credit policy and customer.1's limit (two credit managers).
func (s *setupRun) creditLimit(w *world) {
	w.credit = s.config(w, "credit_policy", "credit.usd", map[string]any{"name": "E-2 synthetic credit policy", "currency": "USD", "default_limit": "0",
		"limit_sources":          []string{"internal_assessment", "sinosure_buyer_limit"},
		"approval_tiers":         []any{map[string]any{"level": "credit_officer", "max_amount": "1000"}, map[string]any{"level": "credit_manager", "max_amount": "5000"}},
		"overdue_tolerance_days": 30, "documents_secured": true, "reservation_validity_days": 30, "decision_validity_days": 7,
		"two_approvers_above_top": true})
	s.cmd(actorSales, "stl", contractSTL, "credit_limit.request", map[string]any{"request_id": "limit.1", "party_id": "customer.1",
		"currency": w.usd, "sources": []any{map[string]any{"source": "internal_assessment", "amount": creditLimitUSD, "evidence_ids": []string{w.evidence}}},
		"valid_from": stamp(w.from), "valid_until": stamp(w.from.Add(365 * 24 * time.Hour)), "credit_policy": w.credit, "reason": creditReason})
	for i, m := range []string{actorCreditManager, actorCreditSecond} {
		s.cmd(m, "stl", contractSTL, "credit_limit.publish", map[string]any{"request_id": "limit.1", "expected_version": i + 1,
			"decision": "publish", "credit_policy": w.credit, "reason": creditReason})
	}
}

// offerLines are the four condition lines of supplier.A's offer at 5.00 USD with the explicit ST01
// MOQ group of 100 PCS, delivered at DEHAM on 2027-01-15.
func (w *world) offerLines() []any {
	var out []any
	for i, l := range brcLines {
		out = append(out, map[string]any{"id": fmt.Sprintf("OL%d", i+1), "sku": map[string]any{"sku_id": l.sku, "spec_revision": 1},
			"purchase_unit_ref": w.pcs, "price_unit_ref": w.pcs, "price_basis_quantity": "1", "tier_unit_ref": w.pcs,
			"price_tier_scope": "sku", "pricing_mode": "flat",
			"tiers": []any{map[string]any{"lower_inclusive": "0", "unit_price": "5.00"}},
			"moq": map[string]any{"quantity": "100", "unit_ref": w.pcs, "scope": "explicit_group", "group_id": "ST01",
				"member_line_ids": lineIDs()},
			"currency_ref": w.usd, "tax_profile_ref": w.zero, "tax_application_ref": w.goods[l.sku], "trade_terms_ref": w.trade,
			"payment_terms_ref": w.payment, "delivery": map[string]any{"kind": "fixed", "date": "2027-01-15",
				"milestone": "handover", "place": "DEHAM", "time_zone": "Asia/Shanghai"}})
	}
	return out
}

// costed is a trial's cost snapshot.
type costed struct {
	snapshotID, resultHash string
}

// costing has the buyer source supplier.A and provider.1 for the trial's ready RFQ and the cost
// checker verify, evaluate and freeze the 660.00 USD snapshot (BRC-001 ES-01 steps 3–10), as
// people do on their own pages (plain routes: the RFQ, offers and costing are not task roots).
func costing(ctx context.Context, ops map[string]*actor, w *world, t string, rfqVersion int64, source string) (costed, error) {
	s := &setupRun{ctx: ctx, ops: ops}
	supplier := map[string]any{"party_id": "supplier.A", "row_version": 2}
	rfqID := "rfq." + t
	rfq := map[string]any{"rfq_id": rfqID, "row_version": rfqVersion}
	request, offer, logistics := "request."+t, "offer."+t, "log."+t
	s.cmd(actorBuyer, "sup", contractSUP, "supplier_rfq.save_draft", map[string]any{"id": request, "expected_version": 0,
		"data": map[string]any{"supplier": supplier, "rfq": rfq, "rfq_line_ids": lineIDs(),
			"evidence": []any{map[string]any{"source_id": source, "locator": "inquiry"}}}})
	issued := s.cmd(actorBuyer, "sup", contractSUP, "supplier_rfq.record_issued", map[string]any{"id": request, "expected_version": 1,
		"evidence": []any{map[string]any{"source_id": w.evidence, "locator": "询价发送记录 " + request}}, "reason": "人工发出供应商询价"})
	s.cmd(actorBuyer, "sup", contractSUP, "supplier_offer.save_draft", map[string]any{"id": offer, "expected_version": 0,
		"data": map[string]any{"supplier": supplier, "rfq": rfq, "rfq_line_ids": lineIDs(),
			"origin": map[string]any{"kind": "in_response_to_rfq", "supplier_rfq": map[string]any{"supplier_rfq_id": request,
				"row_version": issued["row_version"]}},
			"valid_from": stamp(w.from), "valid_until": stamp(w.until), "lines": w.offerLines(),
			"evidence": []any{map[string]any{"source_id": "supplier.quotes", "locator": "supplier.A 报价"}}}})
	s.cmd(actorCostChecker, "sup", contractSUP, "supplier_offer.verify", map[string]any{"id": offer, "expected_version": 1,
		"working_revision": 1, "evidence": []any{map[string]any{"source_id": w.evidence, "locator": "核实依据 " + offer}}})
	s.cmd(actorBuyer, "sup", contractSUP, "logistics_offer.save_draft", map[string]any{"id": logistics, "expected_version": 0,
		"data": map[string]any{"service_role_ref": map[string]any{"party_id": "provider.1", "service_role_id": "role.1", "revision": 1},
			"service_code": "sea.fcl", "mode": "sea", "origin": "CNSHA", "destination": "DEHAM",
			"valid_from": stamp(w.from), "valid_until": stamp(w.until),
			"fees": []any{map[string]any{"id": "F1", "obligation_id": "freight.1", "fee_type_ref": w.freight, "responsible_party": "seller",
				"currency_ref": w.usd, "tax_profile_ref": w.zero, "tax_application_ref": w.service,
				"billing": map[string]any{"kind": "fixed", "amount": "60.00"}, "chargeable": map[string]any{"kind": "fixed"},
				"allocation": map[string]any{"basis": "quantity", "target_line_ids": lineIDs()}}},
			"evidence": []any{map[string]any{"source_id": "supplier.quotes", "locator": "provider.1 海运费"}}}})
	s.cmd(actorCostChecker, "sup", contractSUP, "logistics_offer.verify", map[string]any{"id": logistics, "expected_version": 1,
		"working_revision": 1, "evidence": []any{map[string]any{"source_id": w.evidence, "locator": "核实依据 " + logistics}}})

	var lines, allocations []any
	for i, l := range brcLines {
		lines = append(lines, map[string]any{"line_id": l.id, "sku_id": l.sku, "sku_spec_revision": 1, "quantity": l.qty, "unit_ref": w.pcs})
		allocations = append(allocations, map[string]any{"rfq_line_id": l.id, "purchase_quantity": l.qty, "purchase_unit_ref": w.pcs,
			"source": map[string]any{"kind": "supplier_offer", "id": offer, "revision": 1, "condition_line_id": fmt.Sprintf("OL%d", i+1)}})
	}
	ctxID, scenario := "context."+t, "scenario."+t
	s.cmd(actorBuyer, "cst", contractCST, "costing_context.save_draft", map[string]any{"id": ctxID, "expected_version": 0,
		"data": map[string]any{"rfq_ref": map[string]any{"id": rfqID, "row_version": rfqVersion}, "context_kind": "actual",
			"lines": lines, "calculation_currency_ref": w.usd, "pricing_policy_ref": w.policy,
			"comparison_basis": map[string]any{"specification_basis": "TECH-01-r1", "named_place_ref": w.place,
				"required_fee_type_refs": []any{w.freight}, "tax_basis": "synthetic_zero_nonrecoverable", "cost_basis": "nominal_direct_cost",
				"sales_trade_terms_ref": w.trade, "sales_payment_terms_ref": w.payment,
				"delivery": map[string]any{"kind": "fixed", "date": "2027-01-31", "milestone": "handover",
					"place_ref": w.place, "time_zone": "Asia/Shanghai"}}}})
	s.cmd(actorCostChecker, "cst", contractCST, "costing_context.verify", map[string]any{"id": ctxID, "expected_version": 1,
		"working_revision": 1, "evidence": []any{map[string]any{"source_id": source, "locator": "inquiry: four SKUs, 120 PCS"}}})
	s.cmd(actorBuyer, "cst", contractCST, "cost_scenario.save_draft", map[string]any{"id": scenario, "expected_version": 0,
		"data": map[string]any{"context_ref": map[string]any{"context_id": ctxID, "revision": 1}, "allocations": allocations,
			"logistics_offer_refs": []any{map[string]any{"kind": "logistics_offer", "id": logistics, "revision": 1}},
			"selection_reason":     "同规格同条款：" + offer + " 全部四行，含 " + logistics + " 海运费"}})
	r := s.cmd(actorCostChecker, "cst", contractCST, "cost_scenario.evaluate", map[string]any{"id": scenario, "expected_version": 1})
	e := s.get(actorCostChecker, "/api/brant/v1/cst/cost_evaluations/"+str(r["object_id"]))
	if s.err == nil && dig(e, "result", "total_minor") != "66000" {
		s.fail("cst cost_scenario.evaluate", e["result"])
	}
	f := s.cmd(actorCostChecker, "cst", contractCST, "cost_snapshot.freeze", map[string]any{"scenario_id": scenario, "expected_version": 1,
		"evaluation_id": e["evaluation_id"], "result_hash": e["result_hash"], "selection_reason": "同口径成本已核实：supplier.A 660.00 USD"})
	return costed{snapshotID: str(f["object_id"]), resultHash: str(e["result_hash"])}, s.result()
}

// stamp formats t as the API's timestamps (kernel.FormatTimestamp: UTC, six fractional digits).
func stamp(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
}
