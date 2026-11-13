//go:build integration

package bootstrap_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The CN-AT suite's driver (S5 unit D2a; S5 plan "CN-AT coverage", E-1 manual paths and synthetic
// data, E-5 every CN-AT case in S5). Every case runs end to end over the HTTP API of the composed
// process (bootstrap.Platform.API behind httptest), each command by the human or AI agent the case
// names with its own OIDC access token of the test's local issuer, the events relayed to the
// composed contexts' consumers in process (brant-worker -dispatch, D1's relay). The driver builds
// on D1's BRC-001 chain (brc001_s5_chain_http_integration_test.go): the S3 chain to the active
// order, then §5's steps up to the point a case branches, then the case's own steps. The owning
// units' PG16 tests prove each rule in detail; the cases here cite them.
//
// With BRANT_CNAT_EVIDENCE set to a directory every case writes its record there as
// cn-atNN.json: the case, the baseline's requirement, the owning units and tests, what was
// executed and observed, what was not executed and why, the deviations, and every HTTP request.

// cnatMeta is a CN-AT case as the product baseline V1.0 §24.3 states it and the S5 plan assigns it.
type cnatMeta struct {
	Title       string
	Requirement string
	Units       []string
	Tests       []string
}

// cnatCatalog is baseline §24.3 CN-AT01–CN-AT18 with the owning units of the S5 plan's "CN-AT
// coverage" table and their PG16 tests.
var cnatCatalog = map[string]cnatMeta{
	"CN-AT01": {"单主体、自营一般贸易的普通出口", "采购→收货→购进票→两批出运→报关结果→收汇与退税资料贯通；无虚构外部成功。",
		[]string{"D1 (BRC-001 chain, one shipment)", "B1b", "C1", "C2", "C3a", "C3b", "B2a", "B2b"},
		[]string{"TestBRC001OrderToSettlementOverHTTP"}},
	"CN-AT02": {"一单分批出口、同次出口包含多销售行", "销售、出运及报关项映射可核对，累计不超有效业务范围。",
		[]string{"B1b (export lots)", "C3a (declaration items)"},
		[]string{"TestCNAT02DeclaresMultiLineLotsFromFUL", "TestExportLotsMapSplitShipmentsCNAT02"}},
	"CN-AT03": {"票据品名或单位与报关资料不同", "需受控映射／换算及适用专业依据；无法证实时阻断申报资料确认。",
		[]string{"C3a (declaration_case.confirm, DECLARATION_MAPPING_UNVERIFIED)"},
		[]string{"TestCNAT03MappingWithoutAProfessionalBasisIsNotConfirmed"}},
	"CN-AT04": {"同一购进凭证部分用于多批业务", "按税务用途和适用政策核对剩余；并发及重试不重复占用。",
		[]string{"C3b (purpose balances, TAX_EVIDENCE_EXCEEDED, concurrency)"},
		[]string{"TestCNAT04PurposeBalancesAcrossBatches", "TestCNAT04ConcurrentAllocationsOneWins"}},
	"CN-AT05": {"先出运后到票，原票随后红冲", "库存／交付事实保留；暂估、应付及退税资料分别处理影响。",
		[]string{"C3b (red reversal → matches and allocations to review)", "B2b (payable on the verified delivery)"},
		[]string{"TestCNAT05RedReversalPutsMatchesBackToReview", "TestRedReversalAndAllocationCommitOrders"}},
	"CN-AT06": {"改单或撤销后重复、乱序回执", "保留版本链，不用旧结果覆盖新状态，不重复恢复凭证用途。",
		[]string{"C3a (DECLARATION_RECEIPT_STALE)"},
		[]string{"TestCNAT06AmendedAndWithdrawnDeclarationsWithOutOfOrderReceipts"}},
	"CN-AT07": {"客户合并付款，银行有扣费", "原币资金与多单核销守恒，费用另有依据，未解释差额待处理。",
		[]string{"B2a (fund records, applications, FUND_DIFFERENCE_UNEXPLAINED)", "B2b (receivables)", "C10 (opening receivable)"},
		[]string{"TestCombinedPaymentForTwoOrders"}},
	"CN-AT08": {"付款方不同，或发生退汇／退款", "核实付款依据及责任；实际资金与商业调整分别记录，不自动确认合规。",
		[]string{"B2a (PARTY_ENTITY_MISMATCH pending; refund and return as fund records)"},
		[]string{"TestRefundsReturnsAndPayers"}},
	"CN-AT09": {"外币结汇，再导入人民币入账", "仅形成一次资金转换；不新增销售收入或第二次客户回款。",
		[]string{"B2a (conversion record, no second receipt)"},
		[]string{"TestBaseline46CashSideAndConversion"}},
	"CN-AT10": {"预计退税、核准与到账金额不同", "三者分开；差异与成本／资金影响按冻结政策解释。",
		[]string{"C3b (tax_refund_case.record_result)"},
		[]string{"TestCNAT10ExpectedApprovedAndReceivedStayApart"}},
	"CN-AT11": {"出口后折让或退运", "销售、库存、报关及税务影响分别形成受控案件，不因贷项自动完成退税调整。",
		[]string{"C8 (credit note)", "C10 (CNX impact via external_reconciliation.resolve)"},
		[]string{"TestCNAT11CreditNoteAfterExportIsReflectedInCNX", "TestCreditNoteAndReversalOnRealRecordsBRC001"}},
	"CN-AT12": {"企业适用政策变化且有历史未结业务", "按有效期及专业核实结果处理，旧业务不被新配置覆盖。",
		[]string{"A3a (export_tax_policy kind)", "C3b (the case binds the revision in force at the export date)"},
		[]string{"TestCNAT12CaseBindsThePolicyInForceAtTheExportDate", "TestExportTaxPolicyChangeKeepsTheRevisionOfOldExports"}},
	"CN-AT13": {"提交超时，随后收到专业结果", "先核实再重试，原业务操作唯一；无回执时不宣称办理成功。",
		[]string{"C3a (unknown → verify, EXTERNAL_RESULT_UNKNOWN)"},
		[]string{"TestCNAT13SubmissionTimeoutIsVerifiedBeforeAnythingIsSubmittedAgain"}},
	"CN-AT14": {"单证缺件或证书不适用", "列缺件、适用对象、责任人及期限；已上传文件不直接满足要求。",
		[]string{"C3a (document bundle)", "C5 (CERTIFICATE_NOT_APPLICABLE)"},
		[]string{"TestCNAT14BundleListsWhatIsMissingAndPresenceIsNotValidity", "TestCNAT14CertificateResultsApplyOnlyToTheirObjectMarketAndValidity"}},
	"CN-AT15": {"出口主体／受票或收款主体不一致", "发现差异并转获准模式复核；不能通过外部主体字段绕过公司隔离。",
		[]string{"C3b (invoice buyer)", "B2a (payee) — PARTY_ENTITY_MISMATCH"},
		[]string{"TestCNAT15BuyerEntityMismatchStaysInReview", "TestFundRecordsKeepRemittedReceivedAndChargesApart"}},
	"CN-AT16": {"业务期初、未退税资料及未达账款导入", "截止、来源余额及已办状态可核对；上线不重复生成现金、应付或用途占用。",
		[]string{"C10 (opening_balance.record, tax_refund_case.record_opening)"},
		[]string{"TestCNAT16OpeningBalancesReconcileAtTheCutoff", "TestCNAT16OpeningRefundMaterialReconciles"}},
	"CN-AT17": {"中文操作、双语单证与盖章件修订", "币种、金额、品名及条款对应；保存签署版本，草稿不能冒充已签署。",
		[]string{"C2 (zh-CN/en shipment-text, shipment_document.record_signed)", "TRD S/C signed copies (S3)"},
		[]string{"TestBilingualDocumentsCorrespondAndSignedCopiesCNAT17"}},
	"CN-AT18": {"AI读取票据／报关资料后误匹配", "显示来源与差异；候选须核实后进入相同业务命令，模型不能直接过账。",
		[]string{"C3b (AI imports candidates; verify human only)", "C3a (AI prepares declarations; confirm human only)"},
		[]string{"TestCNAT18CandidatesAreVerifiedByAHuman", "TestDeclarationHumanOnlyCommandsRefuseAnAIActor"}},
}

// cnatEvidence is one case's record (BRANT_CNAT_EVIDENCE).
type cnatEvidence struct {
	Case        string            `json:"case"`
	Title       string            `json:"title"`
	Requirement string            `json:"requirement"`
	Test        string            `json:"test"`
	OwningUnits []string          `json:"owning_units"`
	OwningTests []string          `json:"owning_tests"`
	Executed    string            `json:"executed"` // "executed" or "executed with deviations"
	Result      string            `json:"result"`
	StartedAt   string            `json:"started_at"`
	FinishedAt  string            `json:"finished_at"`
	Environment map[string]string `json:"environment"`
	Tenant      string            `json:"tenant"`
	Actors      map[string]string `json:"actors"`
	Chain       string            `json:"chain"` // the part of D1's BRC-001 chain the case runs on (D1's deviations apply to it)
	Observed    []string          `json:"observed"`
	NotExecuted []string          `json:"not_executed"`
	Deviations  []string          `json:"deviations"`
	Refusals    []string          `json:"refusals"`       // the case's own refusals
	ChainRefuse []string          `json:"chain_refusals"` // the refusals the chain itself asserts (S3 and D1)
	Requests    map[string]int    `json:"requests"`
	Steps       []brcStep         `json:"steps"`
	Dispatches  []brcDispatch     `json:"dispatches,omitempty"`
}

// cnat drives one CN-AT case on D1's BRC-001 driver.
type cnat struct {
	*brcS5
	id string
	ev cnatEvidence
}

// The suite's actors besides D1's (none is a fixture of the case's business: each is a grant).
const (
	cnatAgent = "agent.reader" // an AI agent (kind ai) reading invoices and declarations (CN-AT18)
)

// cnatNotExecuted is what no case executes: the external professional systems (E-1: manual paths,
// synthetic data).
var cnatNotExecuted = []string{
	"China customs (single window): submissions and receipts are recorded manually by the documentation clerk (CN-M01); the receipts' original texts are synthetic",
	"the tax authority: invoices are synthetic scans imported and verified by tax.officer; refund declarations, approvals and payments are recorded from synthetic evidence",
	"the bank: fund records are entered by treasury from synthetic bank advices",
	"the carrier's, forwarder's and inspection agency's systems: bookings, B/Ls and reports are entered from EVD evidence",
	"a real identity provider (the test's local OIDC issuer mints every token)",
	"the user interface (中文操作 is outside the API repository)",
}

// newCNAT starts case id on a fresh tenant: D1's driver (S3 humans, S5 humans, doc.1 with
// number.issue, the Inbox actors, the relay), its chain record suppressed, sales holding
// credit_note.request besides its S3 grant (the sales_clerk template's, CN-AT11) — all provisioned
// before the first command.
func newCNAT(t *testing.T, id string) *cnat {
	meta, ok := cnatCatalog[id]
	if !ok {
		t.Fatalf("no case %s", id)
	}
	s5 := newBRC001S5(t)
	s5.report = "" // the S3/S5 chain record is D1's; this case writes its own
	c := &cnat{brcS5: s5, id: id}
	c.w.provision(bootstrap.Grant{Scope: c.w.scope("company.A"), Actor: actorRef("human", brcSales),
		Permissions: append(slices.Clone(allPermissions), append(slices.Clone(brcCredit), stlapi.CommandRequestCreditNote)...),
		DataScope:   "company", Team: "trade"})
	c.ev = cnatEvidence{Case: id, Title: meta.Title, Requirement: meta.Requirement, Test: t.Name(), OwningUnits: meta.Units,
		OwningTests: meta.Tests, Executed: "executed", StartedAt: kernel.FormatTimestamp(time.Now()), Environment: c.record.Environment,
		Tenant: c.w.tenant, NotExecuted: slices.Clone(cnatNotExecuted), Requests: map[string]int{}, Observed: []string{}, Deviations: []string{},
		Refusals: []string{}, ChainRefuse: []string{}}
	t.Cleanup(c.write)
	return c
}

// write writes the case's record into the directory BRANT_CNAT_EVIDENCE names, when it is set.
func (c *cnat) write() {
	dir := os.Getenv("BRANT_CNAT_EVIDENCE")
	if dir == "" {
		return
	}
	c.ev.Result = "passed"
	if c.t.Failed() {
		c.ev.Result = "failed"
	}
	c.ev.FinishedAt = kernel.FormatTimestamp(time.Now())
	c.ev.Actors = c.record.Actors
	c.ev.Steps, c.ev.Dispatches = c.record.Steps, c.runs
	for _, s := range c.record.Steps {
		c.ev.Requests[s.Method]++
		if s.Status < 400 {
			continue
		}
		r := fmt.Sprintf("%d %s %s by %s (%s)", s.Status, s.Code, s.Path, s.Actor, s.Phase)
		if strings.HasPrefix(s.Phase, c.id+":") {
			c.ev.Refusals = append(c.ev.Refusals, r)
		} else {
			c.ev.ChainRefuse = append(c.ev.ChainRefuse, r)
		}
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c.ev); err != nil {
		c.t.Errorf("encode the case record: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, strings.ToLower(c.id)+".json"), out.Bytes(), 0o644); err != nil {
		c.t.Errorf("write the case record: %v", err)
	}
}

// observe records an observed outcome of the case.
func (c *cnat) observe(format string, args ...any) {
	c.ev.Observed = append(c.ev.Observed, fmt.Sprintf(format, args...))
}

// deviate records a deviation from the baseline's wording of the case (the code was followed,
// the case's intent kept).
func (c *cnat) deviate(format string, args ...any) {
	c.ev.Deviations = append(c.ev.Deviations, fmt.Sprintf(format, args...))
	c.ev.Executed = "executed with deviations"
}

// skipped records a part of the case that was not executed and why.
func (c *cnat) skipped(format string, args ...any) {
	c.ev.NotExecuted = append(c.ev.NotExecuted, fmt.Sprintf(format, args...))
}

// as sends command of bc as actor of kind (human or ai) in the closed envelope under a fresh
// idempotency key, recorded as a step.
func (c *cnat) as(kind, actor, bc, contract, command string, args map[string]any) (int, map[string]any) {
	c.t.Helper()
	c.keys++
	key := fmt.Sprintf("cnat.%03d", c.keys)
	return c.send(kind, actor, bc, contract, command, key, args)
}

// send sends command under key and records it.
func (c *cnat) send(kind, actor, bc, contract, command, key string, args map[string]any) (int, map[string]any) {
	c.t.Helper()
	path := "/api/brant/v1/" + bc + "/commands/" + command
	cl := &client{t: c.t, base: c.base, token: c.is.token(c.w.tenant, "company.A", actor, kind)}
	status, out := cl.do(http.MethodPost, path, map[string]any{"contract": contract, "command": command, "idempotency_key": key, "args": args})
	c.addPost(actor, kind, path, key, status, out)
	return status, out
}

func (c *cnat) addPost(actor, kind, path, key string, status int, out map[string]any) {
	who := actor
	if kind != "human" {
		who = kind + ":" + actor
	}
	s := brcStep{Actor: who, Method: http.MethodPost, Path: path, IdempotencyKey: key, Status: status}
	s.ObjectID, _ = out["object_id"].(string)
	if v, ok := out["row_version"].(float64); ok {
		n := int64(v)
		s.RowVersion = &n
	}
	s.Result, _ = out["status"].(string)
	s.Code, _ = out["code"].(string)
	c.add(s)
}

// ok posts a command as a human that must succeed.
func (c *cnat) ok(actor, bc, contract, command string, args map[string]any) map[string]any {
	c.t.Helper()
	status, out := c.as("human", actor, bc, contract, command, args)
	if status != http.StatusOK || out["ok"] != true {
		c.t.Fatalf("%s %s as %s: %d %v", bc, command, actor, status, out)
	}
	return out
}

// no posts a command as actor of kind that must be refused with status and code at path ("" for
// any path); it returns the refusal's reason.
func (c *cnat) no(kind, actor, bc, contract, command string, args map[string]any, status int, code, at string) string {
	c.t.Helper()
	got, out := c.as(kind, actor, bc, contract, command, args)
	if got != status || out["ok"] != false || out["code"] != code || (at != "" && errPath(out) != at) {
		c.t.Fatalf("%s %s as %s:%s: want %d %s at %q, got %d %v", bc, command, kind, actor, status, code, at, got, out)
	}
	c.fact("field_errors", out["field_errors"])
	reason, _ := brcDig(out, "field_errors", 0, "reason").(string)
	return reason
}

// concurrent sends the commands at once (each as human actor under its own key) and returns
// each answer; the steps are recorded afterwards in the order given.
func (c *cnat) concurrent(actor, bc, contract, command string, args ...map[string]any) []map[string]any {
	c.t.Helper()
	type answer struct {
		status int
		out    map[string]any
		err    error
	}
	path := "/api/brant/v1/" + bc + "/commands/" + command
	token := c.is.token(c.w.tenant, "company.A", actor, "human")
	keys := make([]string, len(args))
	for i := range args {
		c.keys++
		keys[i] = fmt.Sprintf("cnat.%03d", c.keys)
	}
	answers := make([]answer, len(args))
	var start, done sync.WaitGroup
	start.Add(1)
	for i, a := range args {
		done.Add(1)
		go func(i int, a map[string]any) {
			defer done.Done()
			body, _ := json.Marshal(map[string]any{"contract": contract, "command": command, "idempotency_key": keys[i], "args": a})
			req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(body))
			if err != nil {
				answers[i].err = err
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			start.Wait()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				answers[i].err = err
				return
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err == nil {
				err = json.Unmarshal(raw, &answers[i].out)
			}
			answers[i].status, answers[i].err = resp.StatusCode, err
		}(i, a)
	}
	start.Done()
	done.Wait()
	out := make([]map[string]any, len(args))
	for i, a := range answers {
		if a.err != nil {
			c.t.Fatalf("concurrent %s %d: %v", command, i, a.err)
		}
		c.addPost(actor, "human", path, keys[i], a.status, a.out)
		a.out["http_status"], a.out["sent_key"] = float64(a.status), keys[i]
		out[i] = a.out
	}
	return out
}

// provisionAI grants AI agent id the permissions (company data scope); call it before the first
// command.
func (c *cnat) provisionAI(id string, permissions ...string) {
	c.w.provision(bootstrap.Grant{Scope: c.w.scope("company.A"), Actor: actorRef("ai", id), Permissions: permissions, DataScope: "company"})
	c.record.Actors["ai:"+id] = "AI 代理（候选，只读与导入）"
}

// phaseOf names the phase of the case's own steps.
func (c *cnat) phaseOf(what string) { c.phase = c.id + ": " + what }

// toDelivery is D1's chain up to the verified supplier delivery: the S3 chain to the active order
// and its purchase draft, the S5 POL configurations, the relay after the activation, P1's receipt,
// the purchase commitment issued and confirmed (PO-2026-00001), the quality releases (120 PCS), the
// cargo-ready milestone and the delivery of 120 PCS verified (§5 steps 1–11).
func (c *cnat) toDelivery() {
	c.ev.Chain = "BRC-001: the S3 chain (S3-12) and D1 §5 steps 1–11 to the verified supplier delivery of 120 PCS"
	c.toPurchaseDraft()
	c.configurations()
	c.dispatch("after the activation (S3 chain, §5 step 6)")
	c.activationConsumed()
	c.receiptP1()
	c.commitment()
	c.quality()
	c.milestones()
	c.delivery()
}

// toShipped continues D1's chain with shipment.1 of the four lines: prepared, packed, booked,
// released and confirmed (§5 steps 12–14).
func (c *cnat) toShipped() {
	c.toDelivery()
	c.ev.Chain = "BRC-001: the S3 chain and D1 §5 steps 1–14 to shipment.1 of the four lines, released and confirmed"
	c.shipment()
	c.shipmentRelease()
	c.confirm()
}

// toLot continues with the shipment documents, their release and export lot lot.1 (§5 steps 15–17).
func (c *cnat) toLot() {
	c.toShipped()
	c.ev.Chain = "BRC-001: the S3 chain and D1 §5 steps 1–17 to export lot lot.1 (shipment.1 documented, its documents released)"
	c.documents()
	c.documentsRelease()
	c.exportLot()
}

// cnatLine is a shipment line: its ID, the order line it ships and the quantity.
type cnatLine struct{ id, orderLine, qty string }

// cnatShip is a shipment of the case and what its steps returned.
type cnatShip struct {
	id      string
	n       int // the shipment's ordinal: numbers its booking, B/L, documents and evidence
	lines   []cnatLine
	version int64 // the shipment's row version after its last packing/booking step
	set     string
	release string
}

// brcSKU is the SKU of order line id.
func cnatSKU(id string) string {
	for _, l := range brcLines {
		if l.id == id {
			return l.sku
		}
	}
	return ""
}

// perCarton is the carton size of a line: 12 PCS, else 4, else 1.
func cnatPerCarton(qty string) int64 {
	n := kernel.MustDecimal(qty).Floor().Int64()
	for _, size := range []int64{12, 4} {
		if n%size == 0 {
			return size
		}
	}
	return 1
}

// cartons is the number of cartons of the shipment's packing.
func (s *cnatShip) cartons() int64 {
	var n int64
	for _, l := range s.lines {
		n += kernel.MustDecimal(l.qty).Floor().Int64() / cnatPerCarton(l.qty)
	}
	return n
}

// packages are the shipment's cartons, numbered from 1 (actual: with weights and volume).
func (s *cnatShip) packages(actual bool) []any {
	var out []any
	from := int64(1)
	for _, l := range s.lines {
		size := cnatPerCarton(l.qty)
		n := kernel.MustDecimal(l.qty).Floor().Int64() / size
		p := map[string]any{"cartons": n, "contents": []any{map[string]any{"line_id": l.id, "quantity_per_carton": fmt.Sprint(size)}}}
		if actual {
			p["carton_from"], p["gross_weight_kg"], p["net_weight_kg"], p["volume_cbm"] = from, "6.5", "6", "0.05"
		}
		out = append(out, p)
		from += n
	}
	return out
}

func (s *cnatShip) marks() string { return fmt.Sprintf("CUSTOMER.1\nHAMBURG\nC/NO. 1-%d", s.cartons()) }

// particulars are the CI and PL particulars of the shipment in English: the consignee, the marks,
// the ports and its SKUs' descriptions.
func (s *cnatShip) particulars() map[string]any {
	var descriptions []any
	for _, l := range s.lines {
		sku := cnatSKU(l.orderLine)
		if slices.ContainsFunc(descriptions, func(d any) bool { return brcDig(d, "sku_id") == sku }) {
			continue
		}
		descriptions = append(descriptions, map[string]any{"sku_id": sku, "language": fulapi.LanguageEn, "text": "Cotton knitted T-shirt " + sku})
	}
	return map[string]any{"consignee": map[string]any{"name": "customer.1", "address": "合成买方地址\n汉堡"}, "marks": s.marks(),
		"port_of_loading": "CNNGB", "port_of_discharge": "DEHAM", "descriptions": descriptions}
}

// newShip prepares shipment id (ordinal n) of lines from the verified delivery.1 and the quality
// releases (FUL-INV-001/003: what earlier shipments shipped of a delivery line or release counts),
// and, when packed, plans and records its packing and records its booking (§5 step 12 per shipment).
func (c *cnat) newShip(id string, n int, packed bool, lines ...cnatLine) *cnatShip {
	c.t.Helper()
	s := &cnatShip{id: id, n: n, lines: lines}
	var args []any
	for _, l := range lines {
		args = append(args, map[string]any{"line_id": l.id, "order_line_id": l.orderLine, "quantity": l.qty,
			"supplier_delivery": map[string]any{"delivery_id": "delivery.1", "line_id": l.orderLine, "version": 1}, "quality_release_id": "rel." + l.orderLine})
	}
	r := c.ok(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandPrepareShipment, map[string]any{"shipment_id": id, "order_id": "order.1",
		"route": fulapi.RouteSupplierDirect, "lines": args})
	if r["status"] != fulapi.StatusPrepared {
		c.t.Fatalf("prepare %s %v", id, r)
	}
	s.version = brcNum(r["row_version"])
	if !packed {
		return s
	}
	c.ok(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandPlanPacking, map[string]any{"shipment_id": id, "expected_version": s.version,
		"packages": s.packages(false), "evidence_ids": []string{"cargo.ready"}})
	r = c.ok(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRecordPacking, map[string]any{"shipment_id": id, "expected_version": s.version + 1,
		"packages": s.packages(true), "evidence_ids": []string{"forwarder.receipt"}})
	s.version = brcNum(r["row_version"])
	booking := fmt.Sprintf("booking.%d", n)
	c.source(brcMerchandiser, booking, fmt.Sprintf("合成订舱确认（第 %d 批）：买方指定货代 provider.1（FCA），SYNTHETIC STAR 0%d6E，CNNGB→DEHAM。", n, n+1))
	c.ok(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRecordBooking, map[string]any{"shipment_id": id, "expected_version": 0,
		"service_role": map[string]any{"party_id": "provider.1", "service_role_id": "role.1", "revision": 1}, "booking_number": fmt.Sprintf("NGB26110%02d", n),
		"vessel": "SYNTHETIC STAR", "voyage": fmt.Sprintf("0%d6E", n+1), "etd": today(), "cut_off": kernel.FormatTimestamp(time.Now().UTC().Truncate(time.Hour)),
		"container_type": "40HQ", "container_count": 1, "port_of_loading": "CNNGB", "port_of_discharge": "DEHAM", "evidence_ids": []string{booking}})
	return s
}

// releaseShip evaluates the shipment's commercial release (WF-07): granted.
func (c *cnat) releaseShip(s *cnatShip) {
	c.t.Helper()
	r := c.ok(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandEvaluateRelease, map[string]any{"shipment_id": s.id, "scope": fulapi.ScopeShipment})
	if r["status"] != fulapi.ReleaseGranted {
		c.t.Fatalf("release of %s %v", s.id, r)
	}
	s.release = brcStr(r["object_id"])
}

// confirmShip confirms the shipment (FUL-INV-001).
func (c *cnat) confirmShip(s *cnatShip) {
	c.t.Helper()
	r := c.ok(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": s.id, "expected_version": s.version,
		"evidence_ids": []string{"forwarder.receipt"}})
	if r["status"] != fulapi.StatusConfirmed {
		c.t.Fatalf("confirm %s %v", s.id, r)
	}
	v := c.get(brcMerchandiser, "/api/brant/v1/ful/shipments/"+s.id)
	if v["status"] != fulapi.StatusConfirmed || len(brcList(brcDig(v, "confirmation", "unchecked_guards"))) != 0 {
		c.t.Fatalf("confirmed %s %v", s.id, v["confirmation"])
	}
}

// shipDocuments prepares the shipment's commercial invoice, packing list and bill of lading
// record, reads the reconciliation (0 differences), issues them and records the original B/L's
// release after the documents release (§5 steps 15–16 per shipment).
func (c *cnat) shipDocuments(s *cnatShip) {
	c.t.Helper()
	s.set = "docs." + s.id
	prepare := func(expected int64, kind string, extra map[string]any) map[string]any {
		args := map[string]any{"document_set_id": s.set, "shipment_id": s.id, "expected_version": expected, "document_kind": kind}
		for k, v := range extra {
			args[k] = v
		}
		return c.ok(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandPrepareShipmentDocument, args)
	}
	prepare(0, fulapi.DocumentCommercialInvoice, map[string]any{"templates": []any{c.ciEn}, "sequence_ref": c.ciSeq, "seller_ref": c.company,
		"particulars": s.particulars()})
	prepare(1, fulapi.DocumentPackingList, map[string]any{"templates": []any{c.plEn}, "sequence_ref": c.plSeq, "seller_ref": c.company,
		"particulars": s.particulars()})
	bl := fmt.Sprintf("SYNSTAR0%d6E%04d", s.n+1, s.n)
	file := "bl.scan." + s.id
	cartons := s.cartons()
	c.fileOf(brcDocumentation, file, "bl-"+bl+".txt", fmt.Sprintf("合成提单扫描件：%s，CNNGB→DEHAM，%d CTNS。", bl, cartons))
	r := prepare(2, fulapi.DocumentBillOfLading, map[string]any{"bill_of_lading": map[string]any{"number": bl, "file_id": file,
		"vessel": "SYNTHETIC STAR", "voyage": fmt.Sprintf("0%d6E", s.n+1), "port_of_loading": "CNNGB", "port_of_discharge": "DEHAM",
		"shipped_on_board": today(), "consignee": map[string]any{"name": "customer.1", "address": "合成买方地址\n汉堡"}, "marks": s.marks(),
		"packages": cartons, "gross_weight_kg": kernel.MustDecimal("6.5").Mul(kernel.DecimalFromInt(cartons)).String(),
		"volume_cbm": kernel.MustDecimal("0.05").Mul(kernel.DecimalFromInt(cartons)).String(), "container_numbers": []string{fmt.Sprintf("SYNU12345%02d", s.n)}}})
	set := c.get(brcDocumentation, "/api/brant/v1/ful/document_sets/"+s.set)
	if brcDig(set, "reconciliation", "state") != "reconciled" || len(brcList(brcDig(set, "reconciliation", "differences"))) != 0 {
		c.t.Fatalf("reconciliation of %s %v", s.set, set["reconciliation"])
	}
	r = c.ok(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandIssueShipmentDocuments, map[string]any{"document_set_id": s.set,
		"expected_version": r["row_version"]})
	if r["status"] != fulapi.DocumentIssued {
		c.t.Fatalf("issue %s %v", s.set, r)
	}
	r = c.ok(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandEvaluateRelease, map[string]any{"shipment_id": s.id, "scope": fulapi.ScopeDocuments})
	if r["status"] != fulapi.ReleaseGranted {
		c.t.Fatalf("documents release of %s %v", s.id, r)
	}
	release := "bl.release." + s.id
	c.source(brcDocumentation, release, fmt.Sprintf("合成正本提单放单记录：%s 三份正本交买方指定货代 provider.1（FCA）。", bl))
	r = c.ok(brcDocumentation, "ful", fulapi.Contract, fulapi.CommandRecordDocumentRelease, map[string]any{"shipment_id": s.id,
		"expected_version": r["row_version"], "release_kind": fulapi.ReleaseOriginalBL, "evidence_ids": []string{release}})
	if r["status"] != fulapi.ReleaseConsumed {
		c.t.Fatalf("document release of %s %v", s.id, r)
	}
}

// lotOf registers export lot id of shipment's lines: each a [lot line, shipment line, quantity].
func (c *cnat) lotOf(id, shipment string, lines ...[3]string) map[string]any {
	c.t.Helper()
	var args []any
	for _, l := range lines {
		args = append(args, map[string]any{"line_id": l[0], "shipment_line_id": l[1], "quantity": l[2]})
	}
	c.ok(brcMerchandiser, "ful", fulapi.Contract, fulapi.CommandRegisterExportLot, map[string]any{"export_lot_id": id, "shipment_id": shipment, "lines": args})
	return c.get(brcMerchandiser, "/api/brant/v1/ful/export_lots/"+id)
}

// item is a declaration item of the lot lines.
func cnatItem(no int, lines ...string) map[string]any {
	var ls []any
	for _, l := range lines {
		ls = append(ls, map[string]any{"line_id": l})
	}
	return map[string]any{"item_no": no, "lines": ls}
}

// declPrepare has doc.1 prepare a new declaration of lot with items; it returns the case ID and row
// version.
func (c *cnat) declPrepare(lot string, items ...map[string]any) (string, int64) {
	c.t.Helper()
	var it []any
	for _, i := range items {
		it = append(it, i)
	}
	r := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandPrepareDeclaration, map[string]any{"expected_version": 0, "export_lot_id": lot,
		"code_table_version": codeTable, "items": it})
	return brcStr(r["object_id"]), brcNum(r["row_version"])
}

// declConfirm has doc.1 confirm version of declaration id at row version rv.
func (c *cnat) declConfirm(id string, rv, version int64) int64 {
	c.t.Helper()
	r := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandConfirmDeclaration, map[string]any{"declaration_id": id, "expected_version": rv,
		"version": version, "reason": "申报要素与已核实的申报档案一致（合成）"})
	return brcNum(r["row_version"])
}

// declSubmit records doc.1's manual submission of declaration id under operation reference ref.
func (c *cnat) declSubmit(id string, rv int64, ref string) int64 {
	c.t.Helper()
	evidence := "sent." + ref
	c.source(brcDocumentation, evidence, "合成报关单人工申报记录：单一窗口录入并提交，操作编号 "+ref+"。")
	r := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordSubmission, map[string]any{"declaration_id": id, "expected_version": rv,
		"operation_reference": ref, "submitter": map[string]any{"kind": "own_clerk", "name": brcDocumentation}, "submitted_at": now(),
		"evidence_ids": []string{evidence}})
	return brcNum(r["row_version"])
}

// receiptArgs is customs' receipt receiptID of declaration id at rv: outcome at external sequence
// seq with the synthetic original text, mapping v1 and declaration number; released and amended
// receipts state the export date (the receipt's instant less a millisecond).
func (c *cnat) receiptArgs(id string, rv int64, receiptID, outcome string, seq int, number string) map[string]any {
	evidence := "receipt.text." + receiptID
	c.source(brcDocumentation, evidence, fmt.Sprintf("合成海关回执 %s：%s（序号 %d，合成原文）。", number, outcome, seq))
	received := time.Now()
	args := map[string]any{"declaration_id": id, "expected_version": rv, "receipt_id": receiptID, "outcome": outcome,
		"external": map[string]any{"sequence": seq, "status_code": "SYN-" + strings.ToUpper(outcome),
			"original_text": fmt.Sprintf("【合成】海关回执 %s：%s / customs %s (synthetic)", number, outcome, outcome), "received_at": kernel.FormatTimestamp(received)},
		"mapping_version": "v1", "declaration_number": number, "evidence_ids": []string{evidence}}
	if outcome == "released" || outcome == "amended" {
		args["exported_at"] = kernel.FormatTimestamp(received.Add(-time.Millisecond))
	}
	return args
}

// declare runs lot's declaration of one item of the lines through confirmation, manual submission
// and customs' release receipt; it returns the case ID.
func (c *cnat) declare(lot, number, ref string, lines ...string) string {
	c.t.Helper()
	id, rv := c.declPrepare(lot, cnatItem(1, lines...))
	rv = c.declConfirm(id, rv, 1)
	rv = c.declSubmit(id, rv, ref)
	r := c.ok(brcDocumentation, "cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, c.receiptArgs(id, rv, "receipt."+lot, "released", 1, number))
	if r["status"] != "released" {
		c.t.Fatalf("receipt of %s %v", lot, r)
	}
	return id
}

// alloc is a tax evidence allocation of qty of line 1 of invoice to export line.
func cnatAlloc(id, line, invoice, qty string) map[string]any {
	return map[string]any{"allocation_id": id, "export_line_id": line, "purchase_invoice_id": invoice, "line_no": 1, "purpose": cnxapi.PurposeExportRefund,
		"quantity": qty}
}

// allocate has tax.officer allocate evidence to lot under export.tax; it returns the answer.
func (c *cnat) allocate(lot string, allocations ...map[string]any) map[string]any {
	c.t.Helper()
	var as []any
	for _, a := range allocations {
		as = append(as, a)
	}
	return c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandAllocateTaxEvidence, map[string]any{"export_lot_id": lot, "policy_id": "export.tax",
		"allocations": as})
}

func cnatAllocArgs(lot string, allocations ...map[string]any) map[string]any {
	var as []any
	for _, a := range allocations {
		as = append(as, a)
	}
	return map[string]any{"export_lot_id": lot, "policy_id": "export.tax", "allocations": as}
}

// balance is invoice line 1's export_refund balance as "allocated/quantity available".
func (c *cnat) balance(invoice string) string {
	return c.balanceOf(invoice, cnxapi.PurposeExportRefund)
}

// balanceOf is invoice line 1's balance for purpose as "allocated/quantity available".
func (c *cnat) balanceOf(invoice, purpose string) string {
	c.t.Helper()
	v := c.get(brcTaxOfficer, "/api/brant/v1/cnx/purchase_invoices/"+invoice)
	for _, b := range brcList(v["balances"]) {
		if brcDig(b, "line_no") == float64(1) && brcDig(b, "purpose") == purpose {
			out := fmt.Sprintf("%v/%v available %v", brcDig(b, "allocated"), brcDig(b, "quantity"), brcDig(b, "available"))
			c.fact("balance", out)
			return out
		}
	}
	c.t.Fatalf("no %s balance of line 1 in %v", purpose, v["balances"])
	return ""
}

// requirement is a bundle requirement owned by the documentation role, due in 30 days.
func cnatRequirement(id, kind, status string, evidence []string, extra map[string]any) map[string]any {
	r := map[string]any{"requirement_id": id, "kind": kind, "version": "1", "status": status, "owner": map[string]any{"kind": "role", "id": "documentation"}}
	if status == "verified" {
		r["basis"] = "单证核对原件一致（合成）"
	} else {
		r["due_on"] = time.Now().UTC().AddDate(0, 0, 30).Format(time.DateOnly)
	}
	if len(evidence) > 0 {
		r["evidence_ids"] = evidence
	}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

// statementOf has treasury confirm the sales statement id of the shipment's lines; it returns the
// receivable.
func (c *cnat) statementOf(id string, s *cnatShip) map[string]any {
	c.t.Helper()
	var sources []any
	for _, l := range s.lines {
		sources = append(sources, map[string]any{"shipment_id": s.id, "line_id": l.id})
	}
	c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandConfirmSettlement, map[string]any{"statement_id": id, "kind": stlapi.StatementSales,
		"sources": sources})
	return c.get(brcTreasury, "/api/brant/v1/stl/receivables/"+id)
}

// receipt has treasury record customer.1's receipt id of amount on the USD account on bank advice
// evidence (registered by doc.1, as in D1); remitted is what the customer sent.
func (c *cnat) receipt(id, amount, remitted, reference string) map[string]any {
	c.t.Helper()
	advice := "advice." + id
	c.source(brcDocumentation, advice, fmt.Sprintf("合成银行收款通知：customer.1 汇出 %s USD，入账 %s USD，%s。", remitted, amount, reference))
	return c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandRecordFund, map[string]any{"fund_record_id": id, "kind": stlapi.FundReceipt,
		"counterparty_id": "customer.1", "currency": cfgRef("currency", "USD", 1), "bank_account": c.bank, "received_amount": amount,
		"remitted_amount": remitted, "value_date": now(), "bank_reference": reference, "evidence_ids": []string{advice}})
}

// applyTo has treasury apply amount of fund to the statement's installment of stage.
func (c *cnat) applyTo(id, fund, statement, stage, amount string) map[string]any {
	return c.ok(brcTreasury, "stl", stlapi.Contract, stlapi.CommandApply, cnatApplyArgs(id, fund, statement, stage, amount))
}

func cnatApplyArgs(id, fund, statement, stage, amount string) map[string]any {
	target := map[string]any{"kind": "receivable", "statement_id": statement}
	if stage != "" {
		target["stage_id"] = stage
	}
	return map[string]any{"application_id": id, "fund_record_id": fund, "target": target, "amount": amount}
}

// cnatInvoice is a synthetic VAT special invoice of supplier.A to the company (the company
// profile's legal name and registration number, as D1's): one line of qty PCS at 5.00 USD net, the
// 13 % tax as printed (HALF_UP to the cent), colour blue or red (red: red_of names the blue one).
func cnatInvoice(number, colour, qty string, redOf string) map[string]any {
	usd := kernel.MustCurrency("USD", 2)
	net, err := kernel.RoundMoney(usd, kernel.MustDecimal(qty).Mul(kernel.MustDecimal("5")))
	if err != nil {
		panic(err)
	}
	tax, err := kernel.RoundMoney(usd, net.Decimal().Mul(kernel.MustDecimal("0.13")))
	if err != nil {
		panic(err)
	}
	gross, err := net.Add(tax)
	if err != nil {
		panic(err)
	}
	f := map[string]any{"kind": "special_vat", "colour": colour, "number": number, "issued_on": today(),
		"supplier": map[string]any{"party_id": "supplier.A", "name": "supplier.A", "tax_id": "91330200SYNTHA0001"},
		"buyer":    map[string]any{"name": "合成企业", "tax_id": "SYNTHETIC-NOT-REAL"}, "currency": map[string]any{"id": "USD", "revision": 1},
		"lines": []any{map[string]any{"line_no": 1, "description": "棉制针织T恤衫 ST01", "quantity": qty, "unit": "PCS", "net_amount": net.Amount(),
			"tax_rate": "0.13", "tax_amount": tax.Amount()}}, "net_total": net.Amount(), "tax_total": tax.Amount(), "gross_total": gross.Amount()}
	if redOf != "" {
		f["red_of"] = map[string]any{"kind": "special_vat", "number": redOf}
	}
	return f
}

// cnatMatch matches qty of invoice line 1 to commitment.1's line.
func cnatMatch(line, qty string) map[string]any {
	return map[string]any{"line_no": 1, "commitment_id": "commitment.1", "order_line_id": line, "quantity": qty}
}

// scanInvoice has purchaser register the scan of invoice number as EVD file id.
func (c *cnat) scanInvoice(id, number, qty string) {
	c.fileOf(brcPurchaser, id, "vat-"+number+".txt", fmt.Sprintf("合成增值税专用发票扫描件：supplier.A，No. %s，ST01 %s PCS（合成，非真实发票）。", number, qty))
}

// importInvoice has actor of kind import facts from file; it returns the invoice ID and row version.
func (c *cnat) importInvoice(kind, actor, file string, facts map[string]any) (string, int64) {
	c.t.Helper()
	status, out := c.as(kind, actor, "cnx", cnxapi.Contract, cnxapi.CommandImportInvoice, map[string]any{"file_id": file, "candidate": facts})
	if status != http.StatusOK || out["status"] != "imported" {
		c.t.Fatalf("import as %s:%s: %d %v", kind, actor, status, out)
	}
	return brcStr(out["object_id"]), brcNum(out["row_version"])
}

// verifyInvoice has tax.officer verify invoice id at rv with facts and matches.
func (c *cnat) verifyInvoice(id string, rv int64, facts map[string]any, matches ...map[string]any) map[string]any {
	c.t.Helper()
	args := map[string]any{"purchase_invoice_id": id, "expected_version": rv, "decision": "verify", "facts": facts}
	if len(matches) > 0 {
		var ms []any
		for _, m := range matches {
			ms = append(ms, m)
		}
		args["matches"] = ms
	}
	r := c.ok(brcTaxOfficer, "cnx", cnxapi.Contract, cnxapi.CommandVerifyInvoice, args)
	if r["status"] != "verified" {
		c.t.Fatalf("verify %s %v", id, r)
	}
	return r
}

// cny publishes POL's CNY currency and the company's CNY bank account (admin drafts,
// config.reviewer publishes), as a refund or a conversion needs them.
func (c *cnat) cny() (currency, bank map[string]any) {
	c.t.Helper()
	currency = c.config("currency", "CNY", map[string]any{"code": "CNY", "name": "人民币", "minor_units": 2, "rounding": polapi.RoundingHalfUp})
	bank = c.config("bank_account", "bank.cny", map[string]any{"owner_kind": "company", "owner_id": "company.A", "bank_name": "synthetic bank",
		"account_number": "6222000033334444", "currency": "CNY", "purpose": "both"})
	return currency, bank
}

// fund has treasury record a fund record of kind with the members given, on advice evidence doc.1
// registers (D1's deviation 14: treasury holds no source.register).
func (c *cnat) fund(id, kind string, args map[string]any) map[string]any {
	c.t.Helper()
	advice := "advice." + id
	c.source(brcDocumentation, advice, "合成银行通知："+id+"（"+kind+"，合成，非真实流水）。")
	a := map[string]any{"fund_record_id": id, "kind": kind, "value_date": now(), "evidence_ids": []string{advice}}
	for k, v := range args {
		a[k] = v
	}
	return a
}

// fundView reads fund record id.
func (c *cnat) fundView(id string) map[string]any {
	return c.get(brcTreasury, "/api/brant/v1/stl/fund_records/"+id)
}

// cnatCatalogEntry is a row of the archive's error catalog (docs/00_START_HERE/tools/data/
// error_catalog.json): the code, its category and HTTP status, and its Chinese note — the text a
// zh-CN client shows for the code the API answers.
type cnatCatalogEntry struct {
	Category, Note string
	HTTP           int
}

// cnatErrorCatalog reads the error catalog's codes.
func cnatErrorCatalog(t *testing.T) map[string]cnatCatalogEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "00_START_HERE", "tools", "data", "error_catalog.json"))
	if err != nil {
		t.Fatalf("the error catalog: %v", err)
	}
	var doc struct {
		Codes [][]any `json:"codes"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]cnatCatalogEntry{}
	for _, row := range doc.Codes {
		if len(row) < 6 {
			continue
		}
		code, _ := row[0].(string)
		category, _ := row[1].(string)
		status, _ := row[2].(float64)
		note, _ := row[5].(string)
		out[code] = cnatCatalogEntry{Category: category, Note: note, HTTP: int(status)}
	}
	return out
}

// cnatHan reports whether s holds a Chinese character.
func cnatHan(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// issuedOrDraft is the current version of kind in document set id: the issued one, else the draft.
func (c *cnat) issuedOrDraft(id, kind string) map[string]any {
	c.t.Helper()
	set := c.get(brcDocumentation, "/api/brant/v1/ful/document_sets/"+id)
	var draft map[string]any
	for _, d := range brcList(set["documents"]) {
		if brcDig(d, "document_kind") != kind {
			continue
		}
		switch brcDig(d, "status") {
		case fulapi.DocumentIssued:
			return brcMap(d)
		case "draft":
			draft = brcMap(d)
		}
	}
	if draft == nil {
		c.t.Fatalf("no %s in %v", kind, set)
	}
	return draft
}

// readIn sends GET path as human actor of company (a token of that company) and requires status.
func (c *cnat) readIn(company, actor, path string, status int) map[string]any {
	c.t.Helper()
	cl := &client{t: c.t, base: c.base, token: c.is.token(c.w.tenant, company, actor, "human")}
	got, out := cl.do(http.MethodGet, path, nil)
	s := brcStep{Actor: actor + "@" + company, Method: http.MethodGet, Path: path, Status: got}
	s.Code, _ = out["code"].(string)
	c.add(s)
	if got != status {
		c.t.Fatalf("GET %s as %s of %s: want %d, got %d %v", path, actor, company, status, got, out)
	}
	return out
}
