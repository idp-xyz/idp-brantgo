//go:build integration

package bootstrap_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// The deal builder of unit D2b: the S3 chain's steps (ES-01 steps 1–19, as brc001 runs them) with
// the figures a companion scenario states instead of BRC-001's — its own payment and trade terms,
// calculation currency and sales rate, SKUs, quantities, prices, purchase currency and freight — and
// more than one order in a tenant. The humans, the transport, the relay and every generic step are
// D1's and the S3 chain's (newBRC001S5; cmd, get, config, party, source); nothing of the S3 chain's
// own steps is changed. A tenant runs s5scTenant's foundation once, then each s5scDeal from its RFQ
// to its draft order, its compliance conclusion, its credit and its activation, its purchase draft,
// and the S5 steps a scenario needs (commitment, quality release, delivery, shipment).

// s5scTenant is a tenant's foundation: payment terms stages, the Incoterm and its named place, the
// pricing policy's calculation currency (and the sales rate from USD into it), the supplier offers'
// currency, the SKUs and customer.1's credit limit in USD.
type s5scTenant struct {
	terms           string // the payment terms' name
	stages          []any  // the payment terms' stages
	incoterm, place string
	calc, salesRate string // the calculation currency; the quotation rate USD → calc when calc is not USD
	purchase        string // the currency of the supplier offers and the freight
	skus            []string
	limit           string
}

// s5scLine is one line of a deal: the RFQ/order line, its SKU and quantity, the sales unit price per
// the deal's price basis, and the supplier's unit price.
type s5scLine struct{ id, sku, qty, price, cost string }

// s5scDeal is one order a scenario forms, with what its steps returned.
type s5scDeal struct {
	id         string // suffix of every ID of the deal: rfq.<id>, quote.<id>, order.<id>, …
	lines      []s5scLine
	priceBasis string // the quote lines' price_basis_quantity ("1" when empty)
	consignee  string // customer.1's consignee address the quote binds (cons.de or cons.aq)
	freight    string // provider.1's freight in the purchase currency
	supplier   string // supplier.A when empty

	rfqVersion                                     int64
	snapshotID, resultHash, quoteHash, orderHash   string
	checkID, draftHash, commitment, delivery, poNo string
	orderRevision                                  int64
}

func (d *s5scDeal) order() string { return "order." + d.id }

func (d *s5scDeal) basis() string {
	if d.priceBasis == "" {
		return "1"
	}
	return d.priceBasis
}

func (d *s5scDeal) supplierID() string {
	if d.supplier == "" {
		return "supplier.A"
	}
	return d.supplier
}

func (d *s5scDeal) lineIDs() []string {
	var out []string
	for _, l := range d.lines {
		out = append(out, l.id)
	}
	return out
}

// dealFoundation is the S3 chain's foundation, front, customer and supply parties for t, the trade
// compliance prerequisites (the CN and UN programme lists, every SKU's verified declaration profile),
// the E-2 credit policy and customer.1's limit (two credit managers above the top tier), and D1's S5
// configurations (bank account, number sequences, document templates, export tax policy).
func (s *s5sc) dealFoundation(t s5scTenant) {
	b := s.brc001
	b.phase = "deal foundation: POL"
	b.source(brcAdmin, "evidence", "合成核实依据：配置、主体、供应条件与核价的核实记录（D2b 合成场景，非真实交易）。")
	b.usd = b.config("currency", "USD", map[string]any{"code": "USD", "name": "US Dollar", "minor_units": 2, "rounding": polapi.RoundingHalfUp})
	cur := map[string]map[string]any{"USD": b.usd}
	if t.calc == "CNY" || t.purchase == "CNY" {
		cur["CNY"] = b.config("currency", "CNY", map[string]any{"code": "CNY", "name": "人民币", "minor_units": 2, "rounding": polapi.RoundingHalfUp})
	}
	b.pcs = b.config("unit", "PCS", map[string]any{"code": "PCS", "name": "件", "dimension": polapi.DimensionCount, "precision": 0})
	b.zero = b.config("tax_profile", "tax.zero", map[string]any{"name": "SYNTHETIC ZERO", "rate": "0", "price_basis": polapi.PriceBasisExclusive,
		"recovery_treatment": polapi.RecoveryNonrecoverable, "policy_basis": "test only, not a legal determination"})
	b.freight = b.config("fee_category", "freight", map[string]any{"name": "海运费", "category": "freight", "side": polapi.FeeSidePurchaseCost,
		"inclusion": polapi.InclusionSeparate, "allocation_basis": polapi.AllocationQuantity, "policy_basis": "synthetic"})
	for _, sku := range t.skus {
		for _, x := range []struct {
			side string
			apps map[string]map[string]any
		}{{polapi.TaxSidePurchase, b.goods}, {polapi.TaxSideSales, b.sale}} {
			x.apps[sku] = b.config("tax_application", x.side+"."+sku, map[string]any{"name": x.side + " " + sku, "side": x.side,
				"subject_kind": polapi.SubjectSKU, "subject_id": sku, "tax_profile": b.zero, "jurisdiction": "CN", "basis_note": "synthetic"})
		}
	}
	b.service = b.config("tax_application", "tax.service", map[string]any{"name": "service", "side": polapi.TaxSidePurchase,
		"subject_kind": polapi.SubjectFeeCategory, "subject_id": "freight", "party_id": "provider.1", "tax_profile": b.zero, "jurisdiction": "CN",
		"basis_note": "synthetic"})
	b.payment = b.config("payment_terms", "payment.deal", map[string]any{"name": t.terms, "day_basis": polapi.DayBasisCalendar, "stages": t.stages})
	b.trade = b.config("trade_terms", t.incoterm, map[string]any{"incoterm": t.incoterm, "edition": polapi.IncotermsEdition2020, "named_place": t.place,
		"responsibility_note": "synthetic evidence only", "delivery": map[string]any{"kind": polapi.DeliveryFixedDate, "date": "2027-01-31"}})
	b.policy = b.config("pricing_policy", "policy.1", map[string]any{"name": "synthetic D2b policy", "calculation_currency": t.calc,
		"minimum_margin": "0.25", "quote_valid_days": 7, "max_fx_age_seconds": 3600, "tax_profile": b.zero})
	b.company = b.config("company_profile", "company.A", map[string]any{"legal_name": "合成企业", "registration_no": "SYNTHETIC-NOT-REAL",
		"country": "CN", "address": "合成地址", "base_currency": "USD", "time_zone": "Asia/Shanghai", "policy_scope": "synthetic general trade test"})
	b.template = b.config("quote_document_template", "quote.template", map[string]any{"name": "formal", "language": polapi.LanguageEn,
		"renderer_version": polapi.QuoteRendererVersion, "title": "Quotation", "sections": polapi.QuoteSections(), "footer_note": "合成测试，不构成要约"})
	b.seq = b.config("number_sequence", "quote.seq", map[string]any{"document_type": polapi.DocumentTypeQuote, "prefix": "QT-", "width": 5,
		"reset": polapi.ResetAnnual, "time_zone": "Asia/Shanghai"})
	b.config("screening_regulation", "screening", map[string]any{"lists": []any{map[string]any{"list_code": "cn.uel", "jurisdiction": polapi.JurisdictionCN}},
		"rescreen_interval_days": 30})
	if t.calc != "USD" {
		// The quotation rate is observed a minute ago: the pricing policy allows it 3,600 s at the freeze.
		at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
		b.cmd(brcAdmin, "pol", polapi.Contract, "config.save_draft", map[string]any{"kind": polapi.KindFXRate, "id": "fx.usd." + strings.ToLower(t.calc),
			"expected_version": 0, "valid_from": kernel.FormatTimestamp(at), "valid_until": kernel.FormatTimestamp(at.Add(30 * 24 * time.Hour)),
			"data": map[string]any{"from_currency": "USD", "to_currency": t.calc, "rate": t.salesRate, "purpose": polapi.PurposeQuotation,
				"source_name": "SYNTHETIC ANALYSIS RATE", "observed_at": kernel.FormatTimestamp(at)}})
		b.cmd(brcController, "pol", polapi.Contract, "config.publish", map[string]any{"kind": polapi.KindFXRate, "id": "fx.usd." + strings.ToLower(t.calc),
			"expected_version": 1, "revision": 1, "evidence_ids": []string{"evidence"}, "reason": "核实合成报价汇率"})
	}
	s.dealCurrencies = cur
	s.tenant = t

	b.phase = "deal foundation: customer, SKUs, screening"
	b.source(brcSales, "source.1", "合成场景客户询价与采购订单（D2b，非真实交易）。")
	b.party(brcSales, "customer.1", []string{ptyapi.RoleCustomer}, "source.1",
		map[string]any{"address_id": "hq", "role": ptyapi.AddressBuyer, "address": "合成买方地址\n汉堡", "country": syntheticClear},
		map[string]any{"address_id": "cons.de", "role": ptyapi.AddressConsignee, "address": "合成收货地址\n汉堡港", "country": syntheticClear},
		map[string]any{"address_id": "cons.aq", "role": ptyapi.AddressConsignee, "address": "合成收货地址（禁运项目所列地区）", "country": syntheticEmbargoed},
		map[string]any{"address_id": "ap", "role": ptyapi.AddressBilling, "address": "合成账单地址", "country": syntheticClear})
	b.cmd(brcSales, "pty", ptyapi.Contract, "contact.save_draft", map[string]any{"id": "contact.1", "expected_version": 0, "name": "contact.1",
		"party_id": "customer.1"})
	b.cmd(brcSales, "pty", ptyapi.Contract, "contact.confirm", map[string]any{"id": "contact.1", "expected_version": 1, "evidence_ids": []string{"source.1"}})
	customer := b.get(brcSales, "/api/brant/v1/pty/parties/customer.1")
	b.customerRevision = brcNum(customer["revision"])
	for _, sku := range t.skus {
		b.cmd(brcSales, "cat", catapi.Contract, "sku.save_draft", map[string]any{"id": sku, "expected_version": 0, "name": sku, "base_uom": "PCS"})
		b.cmd(brcSales, "cat", catapi.Contract, "sku.confirm", map[string]any{"id": sku, "expected_version": 1, "evidence_ids": []string{"source.1"}})
	}
	b.cmd(brcSales, "pty", ptyapi.Contract, "trading_profile.save_draft", map[string]any{"id": "customer.1", "expected_version": 0,
		"party_revision": b.customerRevision, "payment_terms": b.payment, "trade_terms": b.trade,
		"owner": map[string]any{"actor_id": brcSales, "team_id": "trade"}, "contact_refs": []any{map[string]any{"id": "contact.1", "row_version": 2}}})
	b.cmd(brcController, "pty", ptyapi.Contract, "trading_profile.publish", map[string]any{"id": "customer.1", "expected_version": 1, "revision": 1,
		"evidence_ids": []string{"evidence"}, "reason": "核实合成交易档案"})
	sum := sha256.Sum256([]byte("synthetic D2b screening list cn.uel v1"))
	b.cmd(brcCompliance, "ref", refapi.Contract, "screening_list.import", map[string]any{"list_code": "cn.uel", "jurisdiction": refapi.JurisdictionCN,
		"version": "v1", "source_name": "Synthetic list cn.uel", "source_url": "https://lists.example.org/cn.uel", "published_on": "2026-11-01",
		"file_sha256": hex.EncodeToString(sum[:]), "entry_count": 1,
		"entries": []any{map[string]any{"entry_id": "E1", "primary_name": "Globex Corporation", "entity_type": refapi.EntityTypeEntity}}})
	if r := b.cmd(brcCompliance, "pty", ptyapi.Contract, "party_screening.record", map[string]any{"id": "customer.1", "expected_version": 0,
		"regulation_id": "screening", "regulation_revision": 1}); r["status"] != "clear" {
		b.t.Fatalf("screening %v", r)
	}

	b.phase = "deal foundation: suppliers and the forwarder"
	b.party(brcBuyer, "supplier.A", []string{ptyapi.RoleSupplier}, "evidence")
	b.party(brcBuyer, "provider.1", []string{ptyapi.RoleSupplier}, "evidence")
	b.cmd(brcBuyer, "pty", ptyapi.Contract, "service_role.verify", map[string]any{"id": "provider.1", "expected_version": 2,
		"service_role_id": "role.1", "role": ptyapi.ServiceForwarder, "service_code": "sea.fcl", "origin": "CNSHA", "destination": "DEHAM",
		"valid_from": kernel.FormatTimestamp(b.from), "valid_until": kernel.FormatTimestamp(b.until), "evidence_ids": []string{"evidence"}})

	b.phase = "deal foundation: compliance prerequisites"
	for _, code := range []string{cnProgrammeList, unProgrammeList} {
		f := syntheticCountryProgrammes(code, "v1", "2026-11-01")
		programmes := []any{}
		for _, p := range f.Programmes {
			programmes = append(programmes, map[string]any{"programme_id": p.ProgrammeID, "name": p.Name, "measure": p.Measure,
				"countries": p.Countries, "scope": p.Scope})
		}
		b.cmd(brcCompliance, "ref", refapi.Contract, "country_programme.import", map[string]any{"list_code": f.ListCode, "jurisdiction": f.Jurisdiction,
			"version": f.Version, "source_name": f.SourceName, "source_url": f.SourceURL, "published_on": f.PublishedOn, "file_sha256": f.FileSHA256,
			"programme_count": f.ProgrammeCount, "country_count": f.CountryCount, "programmes": programmes})
	}
	for _, sku := range t.skus {
		id := "decl." + sku
		b.cmd(brcDocumentation, "cat", catapi.Contract, catapi.CommandSaveDeclarationProfile, map[string]any{"id": id, "expected_version": 0,
			"sku_id": sku, "spec_revision": 1, "code_table_version": codeTable,
			"commodity":        map[string]any{"code": "6205200099", "basis": "合成：棉制梭织衬衫，按合成编码表归类"},
			"declaration_name": "棉制梭织衬衫",
			"elements":         []any{map[string]any{"name": "品名", "value": "衬衫"}, map[string]any{"name": "成分含量", "value": "100%棉"}},
			"legal_units":      []any{map[string]any{"unit": "件", "per_base_unit": "1", "basis": "一件即一 PCS"}},
			"origin":           map[string]any{"country": "CN", "criterion": catapi.OriginWhollyObtained, "basis": "合成：境内生产与缝制"},
			"export_control":   map[string]any{"status": catapi.ExportNotControlled, "basis": "合成：不在合成管制清单内"}})
		b.cmd(brcDocVerifier, "cat", catapi.Contract, catapi.CommandVerifyDeclarationProfile, map[string]any{"id": id, "expected_version": 1,
			"revision": 1, "evidence_ids": []string{"evidence"}, "reason": "单证专业核实（合成）"})
	}

	b.phase = "deal foundation: credit policy and limit"
	s.creditPolicy = b.config("credit_policy", "credit.usd", map[string]any{"name": "E-2 synthetic credit policy", "currency": "USD", "default_limit": "0",
		"limit_sources":          []string{polapi.LimitSourceInternalAssessment, polapi.LimitSourceSinosureBuyerLimit},
		"approval_tiers":         []any{map[string]any{"level": "credit_officer", "max_amount": "1000"}, map[string]any{"level": "credit_manager", "max_amount": "5000"}},
		"overdue_tolerance_days": 30, "documents_secured": true, "reservation_validity_days": 30, "decision_validity_days": 7,
		"two_approvers_above_top": true})
	b.cmd(brcSales, "stl", stlapi.Contract, stlapi.CommandRequestLimit, map[string]any{"request_id": "limit.1", "party_id": "customer.1",
		"currency": b.usd, "sources": []any{map[string]any{"source": "internal_assessment", "amount": t.limit, "evidence_ids": []string{"evidence"}}},
		"valid_from": kernel.FormatTimestamp(b.from), "valid_until": kernel.FormatTimestamp(b.from.Add(365 * 24 * time.Hour)),
		"credit_policy": s.creditPolicy, "reason": creditReason})
	for i, m := range []string{brcCreditManager1, brcCreditManager2} {
		b.cmd(m, "stl", stlapi.Contract, stlapi.CommandPublishLimit, map[string]any{"request_id": "limit.1", "expected_version": i + 1,
			"decision": stlapi.DecisionPublish, "credit_policy": s.creditPolicy, "reason": creditReason})
	}
	s.configurations()
}

// money is the POL currency reference of code in the tenant.
func (s *s5sc) money(code string) map[string]any {
	s.t.Helper()
	r, ok := s.dealCurrencies[code]
	if !ok {
		s.t.Fatalf("no currency %s in the tenant", code)
	}
	return r
}

// form is ES-01 steps 1–18 for deal d: the RFQ confirmed ready, the supplier RFQ issued and its offer
// verified, provider.1's freight, the costing context verified, the scenario evaluated and frozen,
// the quote frozen, independently approved and activated, the customer's acceptance verified, and the
// order prepared and independently approved (a draft until compliance, credit and activation).
func (s *s5sc) form(d *s5scDeal) {
	b := s.brc001
	b.phase = "deal " + d.id + ": RFQ, supply and costing"
	var lines []any
	for i, l := range d.lines {
		lines = append(lines, map[string]any{"line_id": l.id, "sku_id": l.sku, "quantity": l.qty, "uom": "PCS", "source_id": "source.1",
			"locator": fmt.Sprintf("%s 第%d项", d.id, i+1)})
	}
	rfq := "rfq." + d.id
	b.cmd(brcSales, "inq", inqapi.Contract, "rfq.save_draft", map[string]any{"id": rfq, "expected_version": 0, "customer_id": "customer.1",
		"contact_id": "contact.1", "source_ids": []string{"source.1"}, "lines": lines})
	if r := b.cmd(brcSales, "inq", inqapi.Contract, "rfq.confirm_scope", map[string]any{"id": rfq, "expected_version": 1,
		"evidence_ids": []string{"source.1"}}); r["status"] != inqapi.StatusReady {
		b.t.Fatalf("confirm_scope %v", r)
	}
	d.rfqVersion = 2
	purchase := s.money(s.tenant.purchase)
	b.source(brcBuyer, "quotes."+d.id, "合成供应商与货代报价（"+d.id+"）：按行单价与运费见报价单。")
	supplier := map[string]any{"party_id": d.supplierID(), "row_version": 2}
	rfqRef := map[string]any{"rfq_id": rfq, "row_version": d.rfqVersion}
	request, offer := "request."+d.id, "offer."+d.id
	b.cmd(brcBuyer, "sup", supapi.Contract, "supplier_rfq.save_draft", map[string]any{"id": request, "expected_version": 0,
		"data": map[string]any{"supplier": supplier, "rfq": rfqRef, "rfq_line_ids": d.lineIDs(),
			"evidence": []any{map[string]any{"source_id": "source.1", "locator": d.id}}}})
	issued := b.cmd(brcBuyer, "sup", supapi.Contract, "supplier_rfq.record_issued", map[string]any{"id": request, "expected_version": 1,
		"evidence": []any{map[string]any{"source_id": "evidence", "locator": "询价发送记录 " + request}}, "reason": "人工发出供应商询价"})
	var offerLines []any
	for i, l := range d.lines {
		offerLines = append(offerLines, map[string]any{"id": fmt.Sprintf("OL%d", i+1), "sku": map[string]any{"sku_id": l.sku, "spec_revision": 1},
			"purchase_unit_ref": b.pcs, "price_unit_ref": b.pcs, "price_basis_quantity": "1", "tier_unit_ref": b.pcs,
			"price_tier_scope": supapi.TierScopeSKU, "pricing_mode": supapi.PricingFlat,
			"tiers":        []any{map[string]any{"lower_inclusive": "0", "unit_price": l.cost}},
			"moq":          map[string]any{"quantity": "1", "unit_ref": b.pcs, "scope": supapi.MOQScopeSKU},
			"currency_ref": purchase, "tax_profile_ref": b.zero, "tax_application_ref": b.goods[l.sku], "trade_terms_ref": b.trade,
			"payment_terms_ref": b.payment, "delivery": map[string]any{"kind": supapi.DeliveryFixed, "date": "2027-01-15",
				"milestone": supapi.MilestoneHandover, "place": "DEHAM", "time_zone": "Asia/Shanghai"}})
	}
	b.cmd(brcBuyer, "sup", supapi.Contract, "supplier_offer.save_draft", map[string]any{"id": offer, "expected_version": 0,
		"data": map[string]any{"supplier": supplier, "rfq": rfqRef, "rfq_line_ids": d.lineIDs(),
			"origin": map[string]any{"kind": supapi.OriginInResponseToRfq, "supplier_rfq": map[string]any{"supplier_rfq_id": request,
				"row_version": issued["row_version"]}},
			"valid_from": kernel.FormatTimestamp(b.from), "valid_until": kernel.FormatTimestamp(b.until), "lines": offerLines,
			"evidence": []any{map[string]any{"source_id": "quotes." + d.id, "locator": d.supplierID() + " 报价"}}}})
	b.cmd(brcCostChecker, "sup", supapi.Contract, "supplier_offer.verify", map[string]any{"id": offer, "expected_version": 1,
		"working_revision": 1, "evidence": []any{map[string]any{"source_id": "evidence", "locator": "核实依据 " + offer}}})
	logistics := "log." + d.id
	b.cmd(brcBuyer, "sup", supapi.Contract, "logistics_offer.save_draft", map[string]any{"id": logistics, "expected_version": 0,
		"data": map[string]any{"service_role_ref": map[string]any{"party_id": "provider.1", "service_role_id": "role.1", "revision": 1},
			"service_code": "sea.fcl", "mode": supapi.ModeSea, "origin": "CNSHA", "destination": "DEHAM",
			"valid_from": kernel.FormatTimestamp(b.from), "valid_until": kernel.FormatTimestamp(b.until),
			"fees": []any{map[string]any{"id": "F1", "obligation_id": "freight." + d.id, "fee_type_ref": b.freight, "responsible_party": supapi.PartySeller,
				"currency_ref": purchase, "tax_profile_ref": b.zero, "tax_application_ref": b.service,
				"billing": map[string]any{"kind": supapi.BillingFixed, "amount": d.freight}, "chargeable": map[string]any{"kind": supapi.ChargeableFixed},
				"allocation": map[string]any{"basis": supapi.AllocationQuantity, "target_line_ids": d.lineIDs()}}},
			"evidence": []any{map[string]any{"source_id": "quotes." + d.id, "locator": "provider.1 海运费"}}}})
	b.cmd(brcCostChecker, "sup", supapi.Contract, "logistics_offer.verify", map[string]any{"id": logistics, "expected_version": 1,
		"working_revision": 1, "evidence": []any{map[string]any{"source_id": "evidence", "locator": "核实依据 " + logistics}}})

	var costLines, allocations []any
	for i, l := range d.lines {
		costLines = append(costLines, map[string]any{"line_id": l.id, "sku_id": l.sku, "sku_spec_revision": 1, "quantity": l.qty, "unit_ref": b.pcs})
		allocations = append(allocations, map[string]any{"rfq_line_id": l.id, "purchase_quantity": l.qty, "purchase_unit_ref": b.pcs,
			"source": map[string]any{"kind": cstapi.SupplySupplierOffer, "id": offer, "revision": 1, "condition_line_id": fmt.Sprintf("OL%d", i+1)}})
	}
	context, scenario := "context."+d.id, "scenario."+d.id
	b.cmd(brcBuyer, "cst", cstapi.Contract, "costing_context.save_draft", map[string]any{"id": context, "expected_version": 0,
		"data": map[string]any{"rfq_ref": map[string]any{"id": rfq, "row_version": d.rfqVersion}, "context_kind": cstapi.ContextActual,
			"lines": costLines, "calculation_currency_ref": s.money(s.tenant.calc), "pricing_policy_ref": b.policy,
			"comparison_basis": map[string]any{"specification_basis": "TECH-" + d.id + "-r1", "named_place_ref": b.place,
				"required_fee_type_refs": []any{b.freight}, "tax_basis": "synthetic_zero_nonrecoverable", "cost_basis": cstapi.CostBasisNominalDirect,
				"sales_trade_terms_ref": b.trade, "sales_payment_terms_ref": b.payment,
				"delivery": map[string]any{"kind": cstapi.DeliveryFixed, "date": "2027-01-31", "milestone": cstapi.MilestoneHandover,
					"place_ref": b.place, "time_zone": "Asia/Shanghai"}}}})
	b.cmd(brcCostChecker, "cst", cstapi.Contract, "costing_context.verify", map[string]any{"id": context, "expected_version": 1,
		"working_revision": 1, "evidence": []any{map[string]any{"source_id": "source.1", "locator": d.id}}})
	b.cmd(brcBuyer, "cst", cstapi.Contract, "cost_scenario.save_draft", map[string]any{"id": scenario, "expected_version": 0,
		"data": map[string]any{"context_ref": map[string]any{"context_id": context, "revision": 1}, "allocations": allocations,
			"logistics_offer_refs": []any{map[string]any{"kind": cstapi.SupplyLogisticsOffer, "id": logistics, "revision": 1}},
			"selection_reason":     d.supplierID() + " 全部行，含 " + logistics + " 运费"}})
	r := b.cmd(brcCostChecker, "cst", cstapi.Contract, "cost_scenario.evaluate", map[string]any{"id": scenario, "expected_version": 1})
	e := b.get(brcCostChecker, "/api/brant/v1/cst/cost_evaluations/"+brcStr(r["object_id"]))
	f := b.cmd(brcCostChecker, "cst", cstapi.Contract, "cost_snapshot.freeze", map[string]any{"scenario_id": scenario, "expected_version": 1,
		"evaluation_id": e["evaluation_id"], "result_hash": e["result_hash"], "selection_reason": "唯一同口径方案已核实（" + d.id + "）"})
	d.snapshotID = brcStr(f["object_id"])
	snap := b.get(brcCostChecker, "/api/brant/v1/cst/cost_snapshots/"+d.snapshotID)
	d.resultHash = brcStr(snap["result_hash"])
	s.figure(d.id+".cost_snapshot", fmt.Sprint(snap["total_minor"], " ", snap["currency"]))

	b.phase = "deal " + d.id + ": quote"
	proof := []any{map[string]any{"source_id": "source.1", "locator": d.id}}
	var quoteLines []any
	for i, l := range d.lines {
		quoteLines = append(quoteLines, map[string]any{"line_id": l.id, "rfq_line_id": l.id, "sku_ref": map[string]any{"sku_id": l.sku, "spec_revision": 1},
			"quantity": l.qty, "unit_ref": b.pcs, "unit_price": l.price, "price_basis_quantity": d.basis(), "discount_ratio": "0",
			"tax_application_ref": b.sale[l.sku], "evidence": []any{map[string]any{"source_id": "source.1", "locator": fmt.Sprintf("%s 第%d项", d.id, i+1)}}})
	}
	addresses := []any{map[string]any{"party_id": "customer.1", "party_revision": b.customerRevision, "address_id": "hq"}}
	if d.consignee != "" {
		addresses = append(addresses, map[string]any{"party_id": "customer.1", "party_revision": b.customerRevision, "address_id": d.consignee})
	}
	data := map[string]any{"named_place_ref": b.place,
		"delivery": map[string]any{"kind": trdapi.DeliveryFixed, "date": "2027-01-31", "milestone": trdapi.MilestoneHandover, "place_ref": b.place,
			"time_zone": "Asia/Shanghai"},
		"issuer_ref": b.company, "customer_ref": map[string]any{"party_id": "customer.1", "revision": b.customerRevision},
		"contact_ref":           map[string]any{"contact_id": "contact.1", "row_version": 2},
		"trading_profile_ref":   map[string]any{"party_id": "customer.1", "revision": 1},
		"customer_address_refs": addresses, "rfq_ref": map[string]any{"rfq_id": rfq, "row_version": d.rfqVersion},
		"cost_snapshot_ref": map[string]any{"snapshot_id": d.snapshotID, "result_hash": d.resultHash},
		"lines":             quoteLines, "currency_ref": b.usd, "pricing_policy_ref": b.policy, "trade_terms_ref": b.trade,
		"payment_terms_ref": b.payment, "document_template_ref": b.template, "sequence_ref": b.seq,
		"expires_at":     kernel.FormatTimestamp(time.Now().Add(72 * time.Hour)),
		"customer_notes": "客户可见说明：D2b 合成场景 " + d.id + "。", "evidence": proof}
	if s.tenant.calc != "USD" {
		data["sales_fx_ref"] = cfgRef(polapi.KindFXRate, "fx.usd."+strings.ToLower(s.tenant.calc), 1)
	}
	quote := "quote." + d.id
	const trd = trdapi.Contract
	b.cmd(brcSales, "trd", trd, "quote.save_draft", map[string]any{"quote_id": quote, "expected_version": 0, "data": data})
	b.cmd(brcSales, "trd", trd, "quote.freeze", map[string]any{"quote_id": quote, "expected_version": 1, "working_revision": 1})
	q := b.get(brcSales, "/api/brant/v1/trd/quotes/"+quote)
	rev := brcMap(brcDig(q, "revisions", 0))
	d.quoteHash = brcStr(rev["business_hash"])
	calc := brcMap(rev["calculation"])
	var payments []string
	for _, p := range brcList(calc["payments"]) {
		payments = append(payments, fmt.Sprint(brcDig(p, "stage_id"), " ", brcDig(p, "amount_minor")))
	}
	s.figure(d.id+".quote", fmt.Sprint("payable ", calc["payable_minor"], " USD; cost ", calc["cost_minor"], ", profit ", calc["profit_minor"],
		" ", brcDig(calc, "comparison_currency_ref", "id"), "; margin rule ", calc["margin_rule"], "; payments ", strings.Join(payments, ", ")))
	b.cmd(brcSales, "trd", trd, "quote.submit_review", map[string]any{"quote_id": quote, "expected_version": 2, "business_revision": 1,
		"review_request_id": "review." + quote, "target_command": trdapi.TargetActivate})
	b.cmd(brcQuoteReviewer, "trd", trd, "quote.review.decide", map[string]any{"quote_id": quote, "expected_version": 3, "review_request_id": "review." + quote,
		"business_hash": d.quoteHash, "decision": trdapi.DecisionApprove, "approval_id": "approval." + quote,
		"expires_at": kernel.FormatTimestamp(time.Now().Add(24 * time.Hour)), "reason": "内容核对无误（" + d.id + "）", "evidence": proof})
	b.cmd(brcSales, "trd", trd, "quote.activate", map[string]any{"quote_id": quote, "expected_version": 4, "business_revision": 1,
		"approval_id": "approval." + quote, "expected_approval_effectivity_version": 1})

	b.phase = "deal " + d.id + ": acceptance and order"
	b.source(brcSales, "mail."+d.id, "合成客户邮件：确认接受报价 "+quote+" 全部条款与全部行。")
	b.cmd(brcSales, "trd", trd, "quote.response.record", map[string]any{"response_id": "response." + d.id, "quote_id": quote,
		"business_revision": 1, "business_hash": d.quoteHash, "classification": trdapi.ClassificationFullUnconditional, "summary": "客户回复：全部接受",
		"source_ref": map[string]any{"source_id": "mail." + d.id, "locator": "response." + d.id}, "received_at": kernel.FormatTimestamp(time.Now()),
		"received_at_basis": trdapi.ArrivalChannelServer})
	b.cmd(brcVerifier, "trd", trd, "quote.response.verify", map[string]any{"response_id": "response." + d.id, "acceptance_id": "acceptance." + d.id,
		"business_revision": 1, "business_hash": d.quoteHash, "customer_ref": map[string]any{"party_id": "customer.1", "revision": b.customerRevision},
		"contact_ref": map[string]any{"contact_id": "contact.1", "row_version": 2}, "line_ids": d.lineIDs()})
	b.cmd(brcSales, "trd", trd, "order.prepare", map[string]any{"order_id": d.order(), "quote_id": quote, "business_revision": 1,
		"business_hash": d.quoteHash, "acceptance_id": "acceptance." + d.id})
	o := b.get(brcSales, "/api/brant/v1/trd/orders/"+d.order())
	d.orderHash, d.orderRevision = brcStr(o["order_hash"]), brcNum(o["order_revision"])
	b.cmd(brcSales, "trd", trd, "order.review.request", map[string]any{"order_id": d.order(), "expected_version": 1, "review_id": "review." + d.order()})
	b.cmd(brcOrderReviewer, "trd", trd, "order.review.decide", map[string]any{"order_id": d.order(), "expected_version": 2,
		"review_id": "review." + d.order(), "order_hash": d.orderHash, "decision": trdapi.DecisionApprove,
		"expires_at": kernel.FormatTimestamp(time.Now().Add(24 * time.Hour)), "reason": "订单与已接受的报价一致"})
}

// check reads order.check of d's order on its review.
func (s *s5sc) check(d *s5scDeal) map[string]any {
	return s.get(brcSales, "/api/brant/v1/trd/orders/"+d.order()+"/check?review_id=review."+d.order())
}

// evaluateCompliance has compliance evaluate d's order at its revision (the countries PTY records
// for customer.1's addresses, the destination the consignee's) and returns the check and its
// evaluation.
func (s *s5sc) evaluateCompliance(d *s5scDeal) (string, map[string]any) {
	s.t.Helper()
	s.phase = "deal " + d.id + ": trade compliance"
	evidence := []string{"evidence"}
	// The check of the order's revision, when compliance or CNX's consumer (an amendment, PL-14)
	// opened one.
	expected := int64(0)
	for _, c := range brcList(s.get(brcCompliance, "/api/brant/v1/cnx/orders/"+d.order()+"/trade_compliance")["checks"]) {
		if brcNum(brcDig(c, "order_revision")) == d.orderRevision {
			expected = brcNum(brcDig(c, "row_version"))
		}
	}
	e := s.cmd(brcCompliance, "cnx", cnxapi.Contract, cnxapi.CommandEvaluate, map[string]any{"order_id": d.order(), "order_revision": d.orderRevision,
		"expected_version": expected, "regulation_id": "screening", "regulation_revision": 1, "programme_lists": []string{cnProgrammeList, unProgrammeList},
		"code_table_version": codeTable, "end_use": map[string]any{"statement": "服装零售（合成）", "evidence_ids": evidence}})
	d.checkID = brcStr(e["object_id"])
	view := s.get(brcCompliance, "/api/brant/v1/cnx/trade_compliance_checks/"+d.checkID)
	evals := brcList(view["evaluations"])
	return d.checkID, brcMap(evals[len(evals)-1])
}

// clear is ES-11 step 5 for d: compliance evaluates and concludes clear, with a licence required or not.
func (s *s5sc) clear(d *s5scDeal, licence bool) {
	s.t.Helper()
	id, evaluation := s.evaluateCompliance(d)
	r := s.cmd(brcCompliance, "cnx", cnxapi.Contract, cnxapi.CommandDecide, map[string]any{"check_id": id,
		"expected_version": brcNum(s.get(brcCompliance, "/api/brant/v1/cnx/trade_compliance_checks/"+id)["row_version"]),
		"evaluation_id":    evaluation["evaluation_id"], "conclusion": cnxapi.ConclusionClear, "reason": "目的国、交易方所在国与商品核查完成（合成）",
		"license_required": licence, "evidence_ids": []string{"evidence"}})
	if r["status"] != cnxapi.StatusClear {
		s.t.Fatalf("decide %v", r)
	}
}

// reserve has sales reserve the credit of d's order revision as reservation id.
func (s *s5sc) reserve(d *s5scDeal, id string) map[string]any {
	s.t.Helper()
	s.phase = "deal " + d.id + ": credit"
	return s.cmd(brcSales, "stl", stlapi.Contract, stlapi.CommandReserve, map[string]any{"reservation_id": id, "order_id": d.order(),
		"order_revision": d.orderRevision, "order_hash": d.orderHash, "credit_policy": s.creditPolicy})
}

// activate has sales activate d's order on its review.
func (s *s5sc) activate(d *s5scDeal) map[string]any {
	s.t.Helper()
	s.phase = "deal " + d.id + ": activation"
	r := s.cmd(brcSales, "trd", trdapi.Contract, "order.activate", map[string]any{"order_id": d.order(), "expected_version": 3,
		"review_id": "review." + d.order()})
	if r["status"] != trdapi.OrderActive {
		s.t.Fatalf("order.activate %v", r)
	}
	return s.get(brcSales, "/api/brant/v1/trd/orders/"+d.order())
}

// draft is ES-01 step 19 for d: the purchaser prepares purchase.<id> from the deal's offer.
func (s *s5sc) draft(d *s5scDeal, quantities map[string]string) {
	s.t.Helper()
	s.phase = "deal " + d.id + ": purchase draft"
	var lines []any
	for i, l := range d.lines {
		qty := l.qty
		if q, ok := quantities[l.id]; ok {
			qty = q
		}
		lines = append(lines, map[string]any{"order_line_id": l.id, "quantity": qty,
			"source": map[string]any{"kind": purapi.KindSupplierOffer, "id": "offer." + d.id, "revision": 1, "condition_line_id": fmt.Sprintf("OL%d", i+1)}})
	}
	s.cmd(brcPurchaser, "pur", purapi.Contract, "purchase.draft.prepare", map[string]any{"draft_id": "purchase." + d.id, "order_id": d.order(),
		"selection_reason": "按成本快照选定 " + d.supplierID() + " 报价（" + d.id + "）", "lines": lines})
}

// commit is §5 step 8 for d's draft draftID: request, independent decision, issue (PO number) and the
// supplier's confirmation as issued.
func (s *s5sc) commit(d *s5scDeal, draftID string) {
	s.t.Helper()
	s.phase = "deal " + d.id + ": purchase commitment"
	v := s.get(brcPurchaser, "/api/brant/v1/pur/purchase_drafts/"+draftID)
	d.draftHash, d.commitment = brcStr(v["draft_hash"]), "commitment."+d.id
	s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandRequestCommitment, map[string]any{"commitment_id": d.commitment, "draft_id": draftID,
		"draft_hash": d.draftHash})
	s.cmd(brcOrderReviewer, "pur", purapi.Contract, purapi.CommandDecideCommitment, map[string]any{"commitment_id": d.commitment,
		"expected_version": 1, "draft_hash": d.draftHash, "decision": purapi.DecisionApprove, "valid_until": kernel.FormatTimestamp(s.until)})
	s.source(brcPurchaser, "po.sent."+d.id, "合成采购订单发送记录："+d.commitment+" 经邮件发给 "+d.supplierID()+"。")
	s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandIssueCommitment, map[string]any{"commitment_id": d.commitment, "expected_version": 2,
		"sequence_ref": map[string]any{"id": "po.seq", "revision": 1}, "sent_at": now(),
		"evidence": []any{map[string]any{"source_id": "po.sent." + d.id, "locator": "发送记录"}}})
	s.source(brcPurchaser, "supplier.ack."+d.id, "合成供应商确认："+d.supplierID()+" 按采购订单原样确认。")
	s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandRecordSupplierConfirmation, map[string]any{"commitment_id": d.commitment,
		"expected_version": 3, "result": purapi.ConfirmationAsIssued, "received_at": now(),
		"evidence": []any{map[string]any{"source_id": "supplier.ack." + d.id, "locator": "确认函"}}, "supplier_reference": "OC-" + d.id})
	c := s.get(brcPurchaser, "/api/brant/v1/pur/purchase_commitments/"+d.commitment)
	d.poNo = brcStr(c["po_number"])
	s.figure(d.id+".po_number", d.poNo)
}

// inspect is §5 step 9 for d: an internal final inspection of each commitment line (all conforming)
// and quality releases rel.<id>.<line> of quantities (the line's quantity when absent).
func (s *s5sc) inspect(d *s5scDeal, quantities map[string]string) {
	s.t.Helper()
	s.phase = "deal " + d.id + ": quality"
	s.source(brcQC, "standard."+d.id, "合成检验标准：AQL 2.5，一般检验水平 II（"+d.id+"）。")
	s.fileOf(brcQC, "report."+d.id, "final-inspection-"+d.id+".txt", "合成终检报告（"+d.id+"）：全部合格。")
	v := s.get(brcPurchaser, "/api/brant/v1/pur/purchase_commitments/"+d.commitment)
	for _, l := range brcList(v["lines"]) {
		line, qty := brcStr(brcDig(l, "order_line_id")), brcStr(brcDig(l, "purchase_quantity"))
		if q, ok := quantities[line]; ok {
			qty = q
		}
		subject := map[string]any{"kind": qltapi.SubjectCommitmentLine, "id": d.commitment, "line_id": line}
		s.cmd(brcQC, "qlt", qltapi.Contract, qltapi.CommandRecordInspection, map[string]any{"inspection_id": "insp." + d.id + "." + line, "subject": subject,
			"inspected_quantity": qty, "conforming_quantity": qty, "nonconforming_quantity": "0", "sampling": map[string]any{"method": qltapi.SamplingFull},
			"standard": map[string]any{"source_id": "standard." + d.id}, "inspector": map[string]any{"kind": qltapi.InspectorInternal},
			"report_file_id": "report." + d.id, "inspected_at": now()})
		s.cmd(brcQC, "qlt", qltapi.Contract, qltapi.CommandGrantRelease, map[string]any{"release_id": "rel." + d.id + "." + line, "subject": subject,
			"quantity": qty, "valid_until": kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))})
	}
}

// deliver is §5 step 11 for d: the merchandiser records supplier delivery id of claimed (line →
// quantity) at CNNGB and the purchaser verifies verified (line → quantity).
func (s *s5sc) deliver(d *s5scDeal, id string, claimed, verified [][2]string) {
	s.t.Helper()
	s.phase = "deal " + d.id + ": supplier delivery"
	s.source(brcMerchandiser, "receipt."+id, "合成货代进仓单："+id+"，provider.1 于 CNNGB 收货。")
	var claim, check []any
	for _, l := range claimed {
		claim = append(claim, map[string]any{"order_line_id": l[0], "quantity": l[1]})
	}
	for _, l := range verified {
		check = append(check, map[string]any{"order_line_id": l[0], "verified_quantity": l[1]})
	}
	s.cmd(brcMerchandiser, "pur", purapi.Contract, purapi.CommandRecordSupplierDelivery, map[string]any{"delivery_id": id, "expected_version": 0,
		"commitment_id": d.commitment, "lines": claim, "handover_place": "CNNGB", "handed_over_at": now(),
		"evidence": []any{map[string]any{"source_id": "receipt." + id, "locator": "进仓单"}}, "supplier_reference": "DN-" + id})
	s.cmd(brcPurchaser, "pur", purapi.Contract, purapi.CommandVerifySupplierDelivery, map[string]any{"delivery_id": id, "expected_version": 1,
		"decision": "verify", "lines": check, "evidence": []any{map[string]any{"source_id": "receipt." + id, "locator": "进仓单"}}})
	d.delivery = id
}

// prepareOn has the merchandiser prepare shipment id of d's order from d's delivery (line → quantity,
// shipment lines S1…).
func (s *s5sc) prepareOn(d *s5scDeal, id string, lines [][2]string) (int, map[string]any) {
	var out []any
	for i, l := range lines {
		out = append(out, map[string]any{"line_id": fmt.Sprintf("S%d", i+1), "order_line_id": l[0], "quantity": l[1],
			"supplier_delivery": map[string]any{"delivery_id": d.delivery, "line_id": l[0], "version": 1}, "quality_release_id": "rel." + d.id + "." + l[0]})
	}
	return s.post(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandPrepareShipment, map[string]any{"shipment_id": id, "order_id": d.order(),
		"route": fulapi.RouteSupplierDirect, "lines": out})
}

// ship is prepareOn, the shipment release evaluated (granted) and the confirmation.
func (s *s5sc) ship(d *s5scDeal, id string, lines [][2]string) {
	s.t.Helper()
	s.phase = "deal " + d.id + ": shipment " + id
	status, out := s.prepareOn(d, id, lines)
	if status != http.StatusOK {
		s.t.Fatalf("prepare %s: %d %v", id, status, out)
	}
	s.shipmentID = id
	if r, e := s.evaluate(fulapi.ScopeShipment); r["status"] != fulapi.ReleaseGranted {
		s.t.Fatalf("release of %s %v", id, e)
	}
	s.cmd(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": id, "expected_version": out["row_version"],
		"evidence_ids": []string{"receipt." + d.delivery}})
}

// fundOf has treasury record a receipt of customer.1 in currency on account, its advice registered by doc.1.
func (s *s5sc) fundOf(id, currency string, account map[string]any, received, reference string) {
	s.t.Helper()
	s.source(brcDocumentation, "bank."+id, "合成银行收款通知："+id+"，customer.1 汇入 "+received+" "+currency+"，"+reference+"。")
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, map[string]any{"fund_record_id": id, "kind": stlapi.FundReceipt,
		"counterparty_id": "customer.1", "currency": s.money(currency), "bank_account": account, "received_amount": received, "value_date": now(),
		"bank_reference": reference, "evidence_ids": []string{"bank." + id}})
}

// applyOn has treasury apply amount of fund to target (an order stage or a receivable installment),
// at fx when the fund is in another currency.
func (s *s5sc) applyOn(id, fund string, target map[string]any, amount string, fx map[string]any) map[string]any {
	s.t.Helper()
	args := map[string]any{"application_id": id, "fund_record_id": fund, "target": target, "amount": amount}
	if fx != nil {
		args["fx_rate"] = fx
	}
	return s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, args)
}

func s5scStage(order, stage string) map[string]any {
	return map[string]any{"kind": stlapi.TargetOrderStage, "order_id": order, "stage_id": stage}
}

func s5scInstallment(statement, stage string) map[string]any {
	return map[string]any{"kind": "receivable", "statement_id": statement, "stage_id": stage}
}

// settle has treasury confirm sales statement id of shipment's lines.
func (s *s5sc) settle(id, shipment string, lines int) {
	s.t.Helper()
	var sources []any
	for i := range lines {
		sources = append(sources, map[string]any{"shipment_id": shipment, "line_id": fmt.Sprintf("S%d", i+1)})
	}
	s.cmd(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmSettlement, map[string]any{"statement_id": id, "kind": stlapi.StatementSales,
		"sources": sources})
}

// installments lists a receivable's installments as "stage amount/applied/open".
func (s *s5sc) installments(statement string) string {
	v := s.get(brcTreasury, "/api/brant/v1/stl/receivables/"+statement)
	var out []string
	for _, i := range brcList(v["installments"]) {
		out = append(out, fmt.Sprintf("%v %v/%v/%v", brcDig(i, "stage_id"), brcDig(i, "amount_minor"), brcDig(i, "applied_minor"), brcDig(i, "open_minor")))
	}
	return strings.Join(out, " ")
}

// closeDeal has the merchandiser close d's order on evidence.
func (s *s5sc) closeDeal(d *s5scDeal, evidence string) map[string]any {
	s.t.Helper()
	s.phase = "deal " + d.id + ": order close"
	o := s.get(brcMerchandiser, "/api/brant/v1/trd/orders/"+d.order())
	return s.cmd(brcMerchandiser, "trd", trdapi.Contract, "order.close", map[string]any{"order_id": d.order(), "expected_version": o["row_version"],
		"reason": "全部出运，货款结清（合成）", "evidence": []any{map[string]any{"source_id": evidence, "locator": "出运与收款记录"}}})
}

// s5scSortedKeys is the sorted keys of m.
func s5scSortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// allocationsOf reads d's order's demand allocations as "line allocated/demand".
func (s *s5sc) allocationsOf(d *s5scDeal) string {
	a := s.get(brcPurchaser, "/api/brant/v1/pur/orders/"+d.order()+"/allocations")
	var out []string
	for _, l := range brcList(a["allocations"]) {
		out = append(out, fmt.Sprintf("%v %v/%v", brcDig(l, "order_line_id"), brcDig(l, "allocated"), brcDig(l, "demand")))
	}
	return strings.Join(out, " ")
}
