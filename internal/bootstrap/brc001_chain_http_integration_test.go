//go:build integration

package bootstrap_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// WBS S3-12, BC-AT01 (Tactical DDD testCatalog: "BRC-001 合成样例 … 报价 960.00、订单 960.00、来源链完整；
// 有效订单唯一"): BRC-001's human path replayed end to end on PostgreSQL 16 over the HTTP API with
// OIDC access tokens of a local issuer, in the archive's shape (event storming ES-01 "BRC-001 正常
// 主链：询价到采购草稿", steps 1–19; white paper and M0 report: 24 + 36 + 12 + 48 = 120 PCS at 8.00 USD,
// 960.00; cost 120 × 5.00 + 60.00 = 660.00; margin 31.25 %; payments 288.00 + 672.00; purchase draft
// 600.00 USD):
//
//   - S1 front (Python brant_core/__main__.py demo, tests/test_cli.py
//     test_demo_reaches_ready_and_wont_overwrite_existing_database): source.1 with the demo's text,
//     customer.1 (with a buyer and a billing address) and contact.1, the four SKUs ST01.red.S/M and
//     ST01.blue.S/M, rfq.1 with lines L1–L4 of 24, 36, 12 and 48 PCS, confirmed ready.
//   - POL: the configurations everything cites, drafted by admin and published by an independent
//     config.reviewer (POL-INV-001); PTY: customer.1's trading profile (30 % on order confirmation,
//     70 % 30 days after shipment), the CN screening list and customer.1's clear screening.
//   - SUP (ES-01 steps 6–7): supplier RFQs to supplier.A and supplier.B recorded as issued; their
//     answering offers, four condition lines each at 5.00 and 5.30 USD with an MOQ of 100 PCS over the
//     explicit group ST01 (MOQ 100 按 ST01 款号合计); provider.1's 60.00 USD sea freight CNSHA→DEHAM,
//     allocated by quantity. The buyer drafts, cost.checker verifies (SUP-INV-003).
//   - CST (steps 8–10; Python tests/test_costing.py
//     test_actual_database_inputs_evaluate_freeze_and_read_after_restart): costing context.1 verified;
//     scenario.A (supplier.A) evaluates to 660.00 and scenario.B (supplier.B) to 696.00 — the dearer
//     candidate is kept, not dropped; scenario.A is frozen into the 660.00 USD cost snapshot.
//   - TRD quote (steps 11–14; Python test_t03_server_calculation.py
//     test_freeze_ignores_client_calculation_and_persists_server_result, test_t03_quote_review.py
//     test_draft_freeze_review_independent_approval_and_revoke, test_t03_formalization.py
//     test_activate_writes_number_file_binding_evidence_and_outbox_atomically): sales drafts quote.1,
//     freezes it with the server calculation (960.00, cost 660.00, profit 300.00, 288.00 + 672.00),
//     submits it; its own approval is INDEPENDENT_REVIEW_REQUIRED (TRD-INV-004), quote.reviewer
//     approves; sales activates it with its formal document and number.
//   - Acceptance (step 15; Python test_t04_a37_response_verification.py
//     test_full_unconditional_response_becomes_bound_acceptance_and_order): sales records the
//     customer's full unconditional reply (EVD source "mail"), acceptance.verifier verifies it into
//     acceptance.1, formed by arrival before the quote expired.
//   - Order (steps 16–18; Python test_t04_a38_order_review.py test_end_to_end_and_historical_replay):
//     sales prepares order.1 (960.00) and requests its review; its own decision is
//     REVIEWER_NOT_INDEPENDENT (TRD-INV-013), order.reviewer approves; sales activates it.
//   - Since S5 (unit B4; S5 decision D-20, ES-11) the activation needs CNX's clear trade
//     compliance conclusion of order.1's revision (TRD-INV-023) and STL's reservation of the 672.00
//     due after shipment (TRD-INV-022): until each is recorded, order.check answers
//     TRADE_COMPLIANCE_NOT_CLEAR, then CREDIT_RESERVATION_REQUIRED. Compliance imports the synthetic
//     CN and UN country programme lists, doc.1 classifies the four SKUs and doc.2 verifies them
//     (CAT-INV-002), compliance evaluates and concludes clear; POL publishes the E-2 credit policy,
//     sales requests a limit of 10,000.00 USD that two credit managers publish (above the top tier,
//     STL-INV-005), and sales reserves 672.00 within it (D-7). The commit evidence names both.
//   - PUR (step 19; Python test_t04_a46_purchase_draft.py
//     test_active_order_line_forms_reviewable_draft_from_effective_offer): purchaser prepares
//     purchase.1 from supplier.A's offer over all four lines, 600.00 USD, not committed.
//   - Read back: the quote, its active revision and formal document (quote-text/0.1.0 states the
//     amounts in minor units: "Net: 96000 | Tax: 0 | Payable: 96000", P1 28800, P2 67200), quote_ready,
//     the order, the draft bound to the order's revision and hash (TRD's ActiveOrderRef v1, as PUR
//     read it) and the order's demand allocations. quote_ready is recorded, not asserted: by its
//     contract it re-checks what activation checked, not the conversion slot.
//   - 有效订单唯一 (BC-AT15, TRD-INV-010; Python test_t03_a20_acceptance_order.py
//     test_distinct_public_actions_cannot_bypass_full_v1_slot, test_t04_a46_purchase_draft.py
//     test_competing_drafts_claim_an_order_line_once): a second order.prepare of quote.1 is
//     CONVERSION_SLOT_CONFLICT and leaves no order; a further purchase draft of one piece of L1 from
//     supplier.B is PURCHASE_ALLOCATION_EXCEEDED (Python V0.50 answers PURCHASE_LINE_ALREADY_DRAFTED,
//     before S3-16's allocation ledger).
//
// Python V0.50 never ran this chain whole: its demo stops at the ready RFQ (docs/IMPLEMENTATION.md:
// "未形成成本、报价或订单") and its T02–T04 tests run each stage on a one-line fixture of 120 PCS. The
// stages here are those tests' behaviour on the archive's four lines.
//
// What is HTTP and what is not: every business command above — EVD source.register, PTY party,
// contact, service role, trading profile and party_screening.record, CAT sku, INQ rfq, POL
// config.save_draft and config.publish, REF screening_list.import, SUP, CST, TRD and PUR — is a
// POST /api/brant/v1/{bc}/commands/{command} with the closed envelope, and every read is a GET route,
// each with the acting human's access token. Fixtures, because they have no business HTTP route:
// the tenant (newWorld: a fresh tenant ID with company.A; a tenant is a token claim), the actors'
// grants (bootstrap.Platform.Provision, as the operator-only brant-admin grant does: every
// permission, company data scope, team trade, all provisioned before the first command so no
// authority epoch moves during the chain) and the access tokens (minted by the test's issuer).
//
// With BRANT_BRC001_REPORT set to a file path the test writes the chain as JSON: the environment,
// the fixtures, what was not executed, and each step with its method, path, status, idempotency key,
// object ID, row version, receipt status or refusal code, and the IDs, hashes and amounts it read.
func TestBRC001FullChainOverHTTP(t *testing.T) {
	newBRC001(t).toPurchaseDraft()
}

// toPurchaseDraft is the S3-12 chain from the configuration evidence to the purchase draft of the
// active order, read back, and the refusals of a second order and of a purchase beyond the order.
// The S5 chain (TestBRC001OrderToSettlementOverHTTP) runs it first and continues from there.
func (b *brc001) toPurchaseDraft() {
	b.foundation()
	b.front()
	b.customerReady()
	b.sourcing()
	b.costing()
	b.quote()
	b.acceptance()
	b.order()
	b.purchase()
	b.readBack()
	b.uniqueness()
}

// BRC-001's humans (event storming ES-01 actors), each with its own access token.
const (
	brcSales         = "sales"               // 销售经办
	brcAdmin         = "admin"               // 管理员: POL drafts and the configuration evidence
	brcController    = "config.reviewer"     // publishes POL configurations and the trading profile
	brcCompliance    = "compliance"          // the CN screening list and customer.1's screening
	brcBuyer         = "buyer"               // 采购／核价经办: supplier RFQs, offers, costing drafts
	brcCostChecker   = "cost.checker"        // verifies offers and the costing context, evaluates, freezes
	brcQuoteReviewer = "quote.reviewer"      // 报价审核人
	brcVerifier      = "acceptance.verifier" // 客户确认核实人
	brcOrderReviewer = "order.reviewer"      // 订单审核人
	brcPurchaser     = "purchaser"           // 采购编制人

	// S5 B4: the humans of the order's trade compliance and credit steps, with role templates.
	brcDocumentation  = "doc.1"            // 单证: classifies the SKUs (documentation)
	brcDocVerifier    = "doc.2"            // 单证: verifies the classification (documentation)
	brcCreditManager1 = "credit.manager.1" // 信用经理 (credit_manager)
	brcCreditManager2 = "credit.manager.2" // 信用经理 (credit_manager)
)

// brcCredit is what sales and compliance hold besides allPermissions for the S5 steps: the credit
// limit request and the reservation (D-7), trade compliance and the programme lists.
var brcCredit = []string{"credit_limit.request", "credit.reserve", "stl.read"}

// brcLines are BRC-001's RFQ lines: ST01 red/blue × S/M.
var brcLines = []struct{ id, sku, qty, net, purchase string }{
	{"L1", "ST01.red.S", "24", "19200", "12000"}, {"L2", "ST01.red.M", "36", "28800", "18000"},
	{"L3", "ST01.blue.S", "12", "9600", "6000"}, {"L4", "ST01.blue.M", "48", "38400", "24000"},
}

var brcUSD = kernel.MustCurrency("USD", 2)

// brcMajor is minor in major units of USD ("96000" → "960.00").
func brcMajor(t *testing.T, minor string) string {
	t.Helper()
	m, err := kernel.ParseMinor(brcUSD, minor)
	if err != nil {
		t.Fatalf("%q is not a minor amount: %v", minor, err)
	}
	return m.Amount()
}

// brcDig walks v by map keys (string) and array indexes (int); nil when the path is missing.
func brcDig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

// brcStr, brcNum, brcList and brcMap read a decoded JSON value as a string, an integer, an array or
// an object: the zero value when it is absent or of another type.
func brcStr(v any) string { s, _ := v.(string); return s }

func brcNum(v any) int64 { f, _ := v.(float64); return int64(f) }

func brcList(v any) []any { l, _ := v.([]any); return l }

func brcMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

// brcStep is one HTTP request of the chain as the JSON record shows it.
type brcStep struct {
	N              int            `json:"n"`
	Phase          string         `json:"phase"`
	Actor          string         `json:"actor"`
	Method         string         `json:"method"`
	Path           string         `json:"path"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	Status         int            `json:"status"`
	ObjectID       string         `json:"object_id,omitempty"`
	RowVersion     *int64         `json:"row_version,omitempty"`
	Result         string         `json:"result,omitempty"` // the receipt's status
	Code           string         `json:"code,omitempty"`   // a refusal's code
	Facts          map[string]any `json:"facts,omitempty"`  // IDs, versions, hashes and amounts it read
}

type brcFixture struct {
	What string `json:"what"`
	How  string `json:"how"`
	Why  string `json:"why"`
}

// brcRecord is the machine-readable chain record (BRANT_BRC001_REPORT).
type brcRecord struct {
	Scenario    string            `json:"scenario"`
	WBS         string            `json:"wbs"`
	Parity      string            `json:"parity"`
	Test        string            `json:"test"`
	Result      string            `json:"result"`
	StartedAt   string            `json:"started_at"`
	FinishedAt  string            `json:"finished_at"`
	Environment map[string]string `json:"environment"`
	Tenant      string            `json:"tenant"`
	Company     string            `json:"company"`
	Actors      map[string]string `json:"actors"`
	Fixtures    []brcFixture      `json:"fixtures"`
	NotExecuted []string          `json:"not_executed"`
	Amounts     map[string]string `json:"amounts"`
	Steps       []brcStep         `json:"steps"`
	Dispatches  []brcDispatch     `json:"dispatches,omitempty"` // the S5 chain's runs of the event relay
}

// brc001 drives the chain: the platform's API behind an httptest server, the local issuer, the
// POL references the chain cites ({"kind","id","revision"}) and what earlier steps returned.
type brc001 struct {
	t      *testing.T
	w      *world
	base   string
	is     *issuer
	phase  string
	keys   int
	record brcRecord
	report string // the environment variable naming the record's file

	from, until                                    time.Time
	usd, pcs, zero, freight, service               map[string]any
	payment, trade, policy, company, template, seq map[string]any
	goods, sale                                    map[string]map[string]any // by SKU
	place                                          map[string]any            // DEHAM

	customerRevision, rfqVersion      int64
	snapshotID, resultHash, quoteHash string
	number, orderHash                 string
	orderRevision                     int64
}

func newBRC001(t *testing.T) *brc001 {
	w := newWorld(t)
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	b := &brc001{t: t, w: w, base: srv.URL, is: is, report: "BRANT_BRC001_REPORT", from: from, until: from.Add(24 * time.Hour),
		goods: map[string]map[string]any{}, sale: map[string]map[string]any{},
		place: map[string]any{"kind": trdapi.PlaceKind, "id": "DEHAM", "revision": 1}}
	var version string
	if err = pgtest.AppPool(t).QueryRow(context.Background(), "SHOW server_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	b.record = brcRecord{Scenario: "BRC-001", WBS: "S3-12", Parity: "BC-AT01", Test: t.Name(), StartedAt: kernel.FormatTimestamp(time.Now()),
		Environment: map[string]string{"go": runtime.Version(), "os_arch": runtime.GOOS + "/" + runtime.GOARCH, "postgresql": version,
			"transport": "bootstrap.Platform.API behind net/http/httptest on loopback",
			"identity":  "local OIDC issuer of the test (RS256, typ at+jwt, JWKS by discovery); no real identity provider"},
		Tenant: w.tenant, Company: "company.A",
		Actors: map[string]string{brcSales: "销售经办", brcAdmin: "管理员（POL 草稿）", brcController: "配置与交易档案发布人（独立复核）",
			brcCompliance: "合规（名单与主体筛查）", brcBuyer: "采购／核价经办（起草）", brcCostChecker: "核价复核（核实、评估、冻结）",
			brcQuoteReviewer: "报价审核人", brcVerifier: "客户确认核实人", brcOrderReviewer: "订单审核人", brcPurchaser: "采购编制人",
			brcDocumentation: "单证（申报要素归类）", brcDocVerifier: "单证（归类核实）", brcCreditManager1: "信用经理", brcCreditManager2: "信用经理"},
		Fixtures: []brcFixture{
			{What: "tenant " + w.tenant + " with company.A", How: "newWorld (pgtest.UniqueID)",
				Why: "a tenant is the access token's brant_tenant claim; there is no business HTTP route for it"},
			{What: "grants of the ten humans", How: "bootstrap.Platform.Provision: every permission, company data scope, team trade",
				Why: "operator-only (cmd/brant-admin grant); no business HTTP route"},
			{What: "role templates of doc.1, doc.2 (documentation) and credit.manager.1, credit.manager.2 (credit_manager)",
				How: "bootstrap.Platform.AssignRoles with company data scope",
				Why: "operator-only; STL reads the approver's tier from the credit_manager template (S5 D-6)"},
			{What: "access tokens", How: "the test's local OIDC issuer, one token per request",
				Why: "no real identity provider is connected before the B0 pilot gate"},
		},
		NotExecuted: []string{
			"STL's credit.consume of the reservation on OrderActivated (the event consumer of unit B6): the reservation stays reserved",
			"sending the formal quote and receiving the reply through a channel: the reply is an EVD source the sales clerk registers",
			"sending supplier RFQs: supplier_rfq.record_issued records the human sending as a fact",
			"purchase commitment approval (Python A47): the draft stays not_committed",
			"the Outbox relay to an external sink: the events stay in bento.outbox",
			"a real identity provider",
		},
		Amounts: map[string]string{}}
	for _, actor := range []string{brcSales, brcAdmin, brcController, brcCompliance, brcBuyer, brcCostChecker, brcQuoteReviewer, brcVerifier,
		brcOrderReviewer, brcPurchaser} {
		permissions := allPermissions
		switch actor {
		case brcSales:
			permissions = append(slices.Clone(allPermissions), brcCredit...)
		case brcCompliance:
			permissions = append(slices.Clone(allPermissions), cnxCompliancePermissions...)
		}
		w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", actor), Permissions: permissions,
			DataScope: "company", Team: "trade"})
	}
	w.assign(brcDocumentation, "documentation")
	w.assign(brcDocVerifier, "documentation")
	w.assign(brcCreditManager1, "credit_manager")
	w.assign(brcCreditManager2, "credit_manager")
	t.Cleanup(b.writeRecord)
	return b
}

// writeRecord writes the chain record to the file the environment variable b.report names
// (BRANT_BRC001_REPORT for the S3 chain), when it is set.
func (b *brc001) writeRecord() {
	path := os.Getenv(b.report)
	if path == "" {
		return
	}
	b.record.Result = "passed"
	if b.t.Failed() {
		b.record.Result = "failed"
	}
	b.record.FinishedAt = kernel.FormatTimestamp(time.Now())
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(b.record); err != nil {
		b.t.Errorf("encode the chain record: %v", err)
		return
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		b.t.Errorf("write the chain record: %v", err)
	}
}

func (b *brc001) client(actor string) *client {
	return &client{t: b.t, base: b.base, token: b.is.token(b.w.tenant, "company.A", actor, "human")}
}

func (b *brc001) add(s brcStep) {
	s.N = len(b.record.Steps) + 1
	s.Phase = b.phase
	b.record.Steps = append(b.record.Steps, s)
}

// fact adds key-value pairs to the last step's facts.
func (b *brc001) fact(kv ...any) {
	s := &b.record.Steps[len(b.record.Steps)-1]
	if s.Facts == nil {
		s.Facts = map[string]any{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		s.Facts[kv[i].(string)] = kv[i+1]
	}
}

// post sends command of bc as actor in the closed envelope under a fresh idempotency key.
func (b *brc001) post(actor, bc, contract, command string, args map[string]any) (int, map[string]any) {
	b.t.Helper()
	b.keys++
	key := fmt.Sprintf("brc001.%03d", b.keys)
	path := "/api/brant/v1/" + bc + "/commands/" + command
	status, out := b.client(actor).do(http.MethodPost, path, map[string]any{"contract": contract, "command": command,
		"idempotency_key": key, "args": args})
	s := brcStep{Actor: actor, Method: http.MethodPost, Path: path, IdempotencyKey: key, Status: status}
	s.ObjectID, _ = out["object_id"].(string)
	if v, ok := out["row_version"].(float64); ok {
		n := int64(v)
		s.RowVersion = &n
	}
	s.Result, _ = out["status"].(string)
	s.Code, _ = out["code"].(string)
	b.add(s)
	return status, out
}

// cmd posts a command that must succeed.
func (b *brc001) cmd(actor, bc, contract, command string, args map[string]any) map[string]any {
	b.t.Helper()
	status, out := b.post(actor, bc, contract, command, args)
	if status != http.StatusOK || out["ok"] != true {
		b.t.Fatalf("%s %s as %s: %d %v", bc, command, actor, status, out)
	}
	return out
}

// refused posts a command that must be refused with status and code.
func (b *brc001) refused(actor, bc, contract, command string, args map[string]any, status int, code string) {
	b.t.Helper()
	got, out := b.post(actor, bc, contract, command, args)
	if got != status || out["ok"] != false || out["code"] != code {
		b.t.Fatalf("%s %s as %s: want %d %s, got %d %v", bc, command, actor, status, code, got, out)
	}
	b.fact("field_errors", out["field_errors"])
}

// read sends GET path as actor and requires status (and "ok": true when it is 200).
func (b *brc001) read(actor, path string, status int) map[string]any {
	b.t.Helper()
	got, out := b.client(actor).do(http.MethodGet, path, nil)
	s := brcStep{Actor: actor, Method: http.MethodGet, Path: path, Status: got}
	s.Code, _ = out["code"].(string)
	b.add(s)
	if got != status || (status == http.StatusOK && out["ok"] != true) {
		b.t.Fatalf("GET %s as %s: want %d, got %d %v", path, actor, status, got, out)
	}
	return out
}

func (b *brc001) get(actor, path string) map[string]any { return b.read(actor, path, http.StatusOK) }

// source registers EVD source id with text as actor.
func (b *brc001) source(actor, id, text string) {
	b.cmd(actor, "evd", evdapi.Contract, "source.register", map[string]any{"id": id, "source_kind": evdapi.KindManualText, "content_text": text})
}

// config has admin draft configuration kind/id with data, valid from an hour ago, and config.reviewer
// publish its revision 1 on the evidence; it returns its reference.
func (b *brc001) config(kind, id string, data map[string]any) map[string]any {
	b.t.Helper()
	b.cmd(brcAdmin, "pol", polapi.Contract, "config.save_draft", map[string]any{"kind": kind, "id": id, "expected_version": 0,
		"valid_from": kernel.FormatTimestamp(b.from), "data": data})
	b.cmd(brcController, "pol", polapi.Contract, "config.publish", map[string]any{"kind": kind, "id": id, "expected_version": 1,
		"revision": 1, "evidence_ids": []string{"evidence"}, "reason": "核实合成测试配置"})
	return map[string]any{"kind": kind, "id": id, "revision": 1}
}

// party has actor save party id with roles (and addresses) and confirm it on evidence.
func (b *brc001) party(actor, id string, roles []string, evidence string, addresses ...any) {
	b.t.Helper()
	args := map[string]any{"id": id, "expected_version": 0, "name": id, "roles": roles}
	if len(addresses) > 0 {
		args["addresses"] = addresses
	}
	b.cmd(actor, "pty", ptyapi.Contract, "party.save_draft", args)
	b.cmd(actor, "pty", ptyapi.Contract, "party.confirm", map[string]any{"id": id, "expected_version": 1, "evidence_ids": []string{evidence}})
}

func brcLineIDs() []string {
	ids := make([]string, len(brcLines))
	for i, l := range brcLines {
		ids[i] = l.id
	}
	return ids
}

// foundation registers the configuration evidence and publishes the POL configurations the chain
// cites: USD, PCS, the zero-rate tax profile, the freight fee category, purchase and sales tax
// applications of each SKU and the freight service, the 30/70 payment terms, FCA, the pricing
// policy (minimum margin 25 %, ES-01 step 12), the company profile, the quote document template,
// the quote number sequence and the CN screening regulation.
func (b *brc001) foundation() {
	b.phase = "EVD and POL foundation"
	b.source(brcAdmin, "evidence", "合成核实依据：配置、主体、供应条件与核价的核实记录（BRC-001 合成样例，非真实交易）。")
	b.usd = b.config("currency", "USD", map[string]any{"code": "USD", "name": "US Dollar", "minor_units": 2, "rounding": polapi.RoundingHalfUp})
	b.pcs = b.config("unit", "PCS", map[string]any{"code": "PCS", "name": "件", "dimension": polapi.DimensionCount, "precision": 0})
	b.zero = b.config("tax_profile", "tax.zero", map[string]any{"name": "SYNTHETIC ZERO", "rate": "0", "price_basis": polapi.PriceBasisExclusive,
		"recovery_treatment": polapi.RecoveryNonrecoverable, "policy_basis": "test only, not a legal determination"})
	b.freight = b.config("fee_category", "freight", map[string]any{"name": "海运费", "category": "freight", "side": polapi.FeeSidePurchaseCost,
		"inclusion": polapi.InclusionSeparate, "allocation_basis": polapi.AllocationQuantity, "policy_basis": "synthetic"})
	for _, l := range brcLines {
		// A slice, not a map, so the record lists the steps in the same order on every run.
		for _, s := range []struct {
			side string
			apps map[string]map[string]any
		}{{polapi.TaxSidePurchase, b.goods}, {polapi.TaxSideSales, b.sale}} {
			s.apps[l.sku] = b.config("tax_application", s.side+"."+l.sku, map[string]any{"name": s.side + " " + l.sku, "side": s.side,
				"subject_kind": polapi.SubjectSKU, "subject_id": l.sku, "tax_profile": b.zero, "jurisdiction": "CN", "basis_note": "synthetic"})
		}
	}
	b.service = b.config("tax_application", "tax.service", map[string]any{"name": "service", "side": polapi.TaxSidePurchase,
		"subject_kind": polapi.SubjectFeeCategory, "subject_id": "freight", "party_id": "provider.1", "tax_profile": b.zero, "jurisdiction": "CN",
		"basis_note": "synthetic"})
	b.payment = b.config("payment_terms", "payment.30-70", map[string]any{"name": "30% on order confirmation, 70% 30 days after shipment",
		"day_basis": polapi.DayBasisCalendar, "stages": []any{
			map[string]any{"stage_id": "P1", "percent": "30", "trigger": polapi.TriggerOrderConfirmed, "days": 0},
			map[string]any{"stage_id": "P2", "percent": "70", "trigger": polapi.TriggerAfterShipment, "days": 30}}})
	b.trade = b.config("trade_terms", "FCA", map[string]any{"incoterm": "FCA", "edition": polapi.IncotermsEdition2020, "named_place": "TEST NAMED PLACE",
		"responsibility_note": "synthetic evidence only", "delivery": map[string]any{"kind": polapi.DeliveryFixedDate, "date": "2027-01-31"}})
	b.policy = b.config("pricing_policy", "policy.1", map[string]any{"name": "synthetic BRC-001 policy", "calculation_currency": "USD",
		"minimum_margin": "0.25", "quote_valid_days": 7, "max_fx_age_seconds": 3600, "tax_profile": b.zero})
	b.company = b.config("company_profile", "company.A", map[string]any{"legal_name": "合成企业", "registration_no": "SYNTHETIC-NOT-REAL",
		"country": "CN", "address": "合成地址", "base_currency": "USD", "time_zone": "Asia/Shanghai", "policy_scope": "synthetic general trade test"})
	b.template = b.config("quote_document_template", "quote.template", map[string]any{"name": "formal", "language": polapi.LanguageEn,
		"renderer_version": polapi.QuoteRendererVersion, "title": "Quotation", "sections": polapi.QuoteSections(), "footer_note": "合成测试，不构成要约"})
	b.seq = b.config("number_sequence", "quote.seq", map[string]any{"document_type": polapi.DocumentTypeQuote, "prefix": "QT-", "width": 5,
		"reset": polapi.ResetAnnual, "time_zone": "Asia/Shanghai"})
	b.config("screening_regulation", "screening", map[string]any{"lists": []any{map[string]any{"list_code": "cn.uel", "jurisdiction": polapi.JurisdictionCN}},
		"rescreen_interval_days": 30})
	cfg := b.get(brcSales, "/api/brant/v1/pol/configs/payment_terms/payment.30-70")
	if cfg["active_revision"] != float64(1) {
		b.t.Fatalf("payment terms %v", cfg)
	}
	b.fact("active_revision", cfg["active_revision"])
}

// front is the S1 front of BRC-001 as the Python demo runs it: the source, the customer and its
// contact, the four SKUs, and rfq.1 of 120 PCS confirmed ready.
func (b *brc001) front() {
	b.phase = "S1 front"
	b.source(brcSales, "source.1", "合成样例：ST01四个SKU，24、36、12、48件，合计120 PCS。")
	b.party(brcSales, "customer.1", []string{ptyapi.RoleCustomer}, "source.1",
		map[string]any{"address_id": "hq", "role": ptyapi.AddressBuyer, "address": "合成买方地址\n汉堡"},
		map[string]any{"address_id": "ap", "role": ptyapi.AddressBilling, "address": "合成账单地址"})
	b.cmd(brcSales, "pty", ptyapi.Contract, "contact.save_draft", map[string]any{"id": "contact.1", "expected_version": 0, "name": "contact.1",
		"party_id": "customer.1"})
	if r := b.cmd(brcSales, "pty", ptyapi.Contract, "contact.confirm", map[string]any{"id": "contact.1", "expected_version": 1,
		"evidence_ids": []string{"source.1"}}); r["row_version"] != float64(2) {
		b.t.Fatalf("contact %v", r)
	}
	customer := b.get(brcSales, "/api/brant/v1/pty/parties/customer.1")
	b.customerRevision = brcNum(customer["revision"])
	b.fact("revision", b.customerRevision, "addresses", len(brcList(customer["addresses"])))
	var lines []any
	for i, l := range brcLines {
		b.cmd(brcSales, "cat", catapi.Contract, "sku.save_draft", map[string]any{"id": l.sku, "expected_version": 0, "name": l.sku, "base_uom": "PCS"})
		b.cmd(brcSales, "cat", catapi.Contract, "sku.confirm", map[string]any{"id": l.sku, "expected_version": 1, "evidence_ids": []string{"source.1"}})
		lines = append(lines, map[string]any{"line_id": l.id, "sku_id": l.sku, "quantity": l.qty, "uom": "PCS", "source_id": "source.1",
			"locator": fmt.Sprintf("第1段，第%d项", i+1)})
	}
	b.cmd(brcSales, "inq", inqapi.Contract, "rfq.save_draft", map[string]any{"id": "rfq.1", "expected_version": 0, "customer_id": "customer.1",
		"contact_id": "contact.1", "source_ids": []string{"source.1"}, "lines": lines})
	ready := b.cmd(brcSales, "inq", inqapi.Contract, "rfq.confirm_scope", map[string]any{"id": "rfq.1", "expected_version": 1,
		"evidence_ids": []string{"source.1"}})
	if ready["status"] != inqapi.StatusReady || ready["row_version"] != float64(2) {
		b.t.Fatalf("confirm_scope %v", ready)
	}
	b.rfqVersion = 2
	rfq := b.get(brcSales, "/api/brant/v1/inq/rfqs/rfq.1")
	var quantities []any
	for i, l := range brcLines {
		q := brcDig(rfq, "lines", i, "quantity")
		quantities = append(quantities, q)
		if q != l.qty || brcDig(rfq, "lines", i, "sku_id") != l.sku {
			b.t.Fatalf("rfq line %d %v", i, rfq["lines"])
		}
	}
	if rfq["status"] != inqapi.StatusReady {
		b.t.Fatalf("rfq %v", rfq)
	}
	b.fact("status", rfq["status"], "row_version", rfq["row_version"], "quantities", quantities)
}

// customerReady gives customer.1 the trading profile the quote binds, published by an independent
// reviewer, and screens it clear against the synthetic CN list, as order.activate requires.
func (b *brc001) customerReady() {
	b.phase = "PTY trading profile and screening"
	b.cmd(brcSales, "pty", ptyapi.Contract, "trading_profile.save_draft", map[string]any{"id": "customer.1", "expected_version": 0,
		"party_revision": b.customerRevision, "payment_terms": b.payment, "trade_terms": b.trade,
		"owner": map[string]any{"actor_id": brcSales, "team_id": "trade"}, "contact_refs": []any{map[string]any{"id": "contact.1", "row_version": 2}}})
	b.cmd(brcController, "pty", ptyapi.Contract, "trading_profile.publish", map[string]any{"id": "customer.1", "expected_version": 1, "revision": 1,
		"evidence_ids": []string{"evidence"}, "reason": "核实合成交易档案"})
	sum := sha256.Sum256([]byte("synthetic BRC-001 screening list cn.uel v1"))
	b.cmd(brcCompliance, "ref", refapi.Contract, "screening_list.import", map[string]any{"list_code": "cn.uel", "jurisdiction": refapi.JurisdictionCN,
		"version": "v1", "source_name": "Synthetic list cn.uel", "source_url": "https://lists.example.org/cn.uel", "published_on": "2026-11-01",
		"file_sha256": hex.EncodeToString(sum[:]), "entry_count": 1,
		"entries": []any{map[string]any{"entry_id": "E1", "primary_name": "Globex Corporation", "entity_type": refapi.EntityTypeEntity}}})
	r := b.cmd(brcCompliance, "pty", ptyapi.Contract, "party_screening.record", map[string]any{"id": "customer.1", "expected_version": 0,
		"regulation_id": "screening", "regulation_revision": 1})
	if r["status"] != "clear" {
		b.t.Fatalf("screening %v", r)
	}
	profile := b.get(brcSales, "/api/brant/v1/pty/parties/customer.1/trading_profile")
	if profile["active_revision"] != float64(1) || brcDig(profile, "revisions", 0, "payment_terms", "id") != "payment.30-70" {
		b.t.Fatalf("trading profile %v", profile)
	}
	b.fact("active_revision", profile["active_revision"])
}

// offerLines are the four condition lines of a supplier's offer at price, with the explicit ST01
// MOQ group of 100 PCS over the RFQ's lines, delivered at DEHAM on 2027-01-15.
func (b *brc001) offerLines(price string) []any {
	var out []any
	for i, l := range brcLines {
		out = append(out, map[string]any{"id": fmt.Sprintf("OL%d", i+1), "sku": map[string]any{"sku_id": l.sku, "spec_revision": 1},
			"purchase_unit_ref": b.pcs, "price_unit_ref": b.pcs, "price_basis_quantity": "1", "tier_unit_ref": b.pcs,
			"price_tier_scope": supapi.TierScopeSKU, "pricing_mode": supapi.PricingFlat,
			"tiers": []any{map[string]any{"lower_inclusive": "0", "unit_price": price}},
			"moq": map[string]any{"quantity": "100", "unit_ref": b.pcs, "scope": supapi.MOQScopeExplicitGroup, "group_id": "ST01",
				"member_line_ids": brcLineIDs()},
			"currency_ref": b.usd, "tax_profile_ref": b.zero, "tax_application_ref": b.goods[l.sku], "trade_terms_ref": b.trade,
			"payment_terms_ref": b.payment, "delivery": map[string]any{"kind": supapi.DeliveryFixed, "date": "2027-01-15",
				"milestone": supapi.MilestoneHandover, "place": "DEHAM", "time_zone": "Asia/Shanghai"}})
	}
	return out
}

// sourcing is ES-01 steps 6–7: the buyer confirms the suppliers and the forwarder, records the
// supplier RFQs as sent and drafts the answering offers and the freight; cost.checker verifies them.
func (b *brc001) sourcing() {
	b.phase = "SUP supply conditions"
	b.source(brcBuyer, "supplier.quotes", "合成供应商报价：supplier.A 5.00 USD/PCS，supplier.B 5.30 USD/PCS，ST01 四个规格，"+
		"MOQ 100 PCS 按 ST01 款号合计；provider.1 海运 CNSHA→DEHAM 60.00 USD。")
	for _, s := range []struct{ supplier, request, offer, price string }{{"supplier.A", "request.A", "offer.A", "5.00"},
		{"supplier.B", "request.B", "offer.B", "5.30"}} {
		b.party(brcBuyer, s.supplier, []string{ptyapi.RoleSupplier}, "evidence")
		supplier := map[string]any{"party_id": s.supplier, "row_version": 2}
		rfq := map[string]any{"rfq_id": "rfq.1", "row_version": b.rfqVersion}
		b.cmd(brcBuyer, "sup", supapi.Contract, "supplier_rfq.save_draft", map[string]any{"id": s.request, "expected_version": 0,
			"data": map[string]any{"supplier": supplier, "rfq": rfq, "rfq_line_ids": brcLineIDs(),
				"evidence": []any{map[string]any{"source_id": "source.1", "locator": "第1段"}}}})
		issued := b.cmd(brcBuyer, "sup", supapi.Contract, "supplier_rfq.record_issued", map[string]any{"id": s.request, "expected_version": 1,
			"evidence": []any{map[string]any{"source_id": "evidence", "locator": "询价发送记录 " + s.request}}, "reason": "人工发出供应商询价"})
		if issued["status"] != supapi.StatusIssued {
			b.t.Fatalf("issued %v", issued)
		}
		b.cmd(brcBuyer, "sup", supapi.Contract, "supplier_offer.save_draft", map[string]any{"id": s.offer, "expected_version": 0,
			"data": map[string]any{"supplier": supplier, "rfq": rfq, "rfq_line_ids": brcLineIDs(),
				"origin": map[string]any{"kind": supapi.OriginInResponseToRfq, "supplier_rfq": map[string]any{"supplier_rfq_id": s.request,
					"row_version": issued["row_version"]}},
				"valid_from": kernel.FormatTimestamp(b.from), "valid_until": kernel.FormatTimestamp(b.until), "lines": b.offerLines(s.price),
				"evidence": []any{map[string]any{"source_id": "supplier.quotes", "locator": s.supplier + " 报价"}}}})
		b.cmd(brcCostChecker, "sup", supapi.Contract, "supplier_offer.verify", map[string]any{"id": s.offer, "expected_version": 1,
			"working_revision": 1, "evidence": []any{map[string]any{"source_id": "evidence", "locator": "核实依据 " + s.offer}}})
		offer := b.get(brcBuyer, "/api/brant/v1/sup/supplier_offers/"+s.offer)
		if offer["active_revision"] != float64(1) {
			b.t.Fatalf("offer %v", offer)
		}
		b.fact("active_revision", offer["active_revision"], "unit_price", s.price)
	}

	b.party(brcBuyer, "provider.1", []string{ptyapi.RoleSupplier}, "evidence")
	b.cmd(brcBuyer, "pty", ptyapi.Contract, "service_role.verify", map[string]any{"id": "provider.1", "expected_version": 2,
		"service_role_id": "role.1", "role": ptyapi.ServiceForwarder, "service_code": "sea.fcl", "origin": "CNSHA", "destination": "DEHAM",
		"valid_from": kernel.FormatTimestamp(b.from), "valid_until": kernel.FormatTimestamp(b.until), "evidence_ids": []string{"evidence"}})
	b.cmd(brcBuyer, "sup", supapi.Contract, "logistics_offer.save_draft", map[string]any{"id": "log.1", "expected_version": 0,
		"data": map[string]any{"service_role_ref": map[string]any{"party_id": "provider.1", "service_role_id": "role.1", "revision": 1},
			"service_code": "sea.fcl", "mode": supapi.ModeSea, "origin": "CNSHA", "destination": "DEHAM",
			"valid_from": kernel.FormatTimestamp(b.from), "valid_until": kernel.FormatTimestamp(b.until),
			"fees": []any{map[string]any{"id": "F1", "obligation_id": "freight.1", "fee_type_ref": b.freight, "responsible_party": supapi.PartySeller,
				"currency_ref": b.usd, "tax_profile_ref": b.zero, "tax_application_ref": b.service,
				"billing": map[string]any{"kind": supapi.BillingFixed, "amount": "60.00"}, "chargeable": map[string]any{"kind": supapi.ChargeableFixed},
				"allocation": map[string]any{"basis": supapi.AllocationQuantity, "target_line_ids": brcLineIDs()}}},
			"evidence": []any{map[string]any{"source_id": "supplier.quotes", "locator": "provider.1 海运费"}}}})
	b.cmd(brcCostChecker, "sup", supapi.Contract, "logistics_offer.verify", map[string]any{"id": "log.1", "expected_version": 1,
		"working_revision": 1, "evidence": []any{map[string]any{"source_id": "evidence", "locator": "核实依据 log.1"}}})
}

// scenario is a cost scenario of context.1 buying every line under offer's condition lines, with
// the freight of log.1.
func (b *brc001) scenario(offer, reason string) map[string]any {
	var allocations []any
	for i, l := range brcLines {
		allocations = append(allocations, map[string]any{"rfq_line_id": l.id, "purchase_quantity": l.qty, "purchase_unit_ref": b.pcs,
			"source": map[string]any{"kind": cstapi.SupplySupplierOffer, "id": offer, "revision": 1, "condition_line_id": fmt.Sprintf("OL%d", i+1)}})
	}
	return map[string]any{"context_ref": map[string]any{"context_id": "context.1", "revision": 1}, "allocations": allocations,
		"logistics_offer_refs": []any{map[string]any{"kind": cstapi.SupplyLogisticsOffer, "id": "log.1", "revision": 1}},
		"selection_reason":     reason}
}

// costing is ES-01 steps 8–10: the buyer drafts costing context.1 and both scenarios, cost.checker
// verifies the context, evaluates both (A 660.00, B 696.00) and freezes A.
func (b *brc001) costing() {
	b.phase = "CST costing"
	var lines []any
	for _, l := range brcLines {
		lines = append(lines, map[string]any{"line_id": l.id, "sku_id": l.sku, "sku_spec_revision": 1, "quantity": l.qty, "unit_ref": b.pcs})
	}
	b.cmd(brcBuyer, "cst", cstapi.Contract, "costing_context.save_draft", map[string]any{"id": "context.1", "expected_version": 0,
		"data": map[string]any{"rfq_ref": map[string]any{"id": "rfq.1", "row_version": b.rfqVersion}, "context_kind": cstapi.ContextActual,
			"lines": lines, "calculation_currency_ref": b.usd, "pricing_policy_ref": b.policy,
			"comparison_basis": map[string]any{"specification_basis": "TECH-01-r1", "named_place_ref": b.place,
				"required_fee_type_refs": []any{b.freight}, "tax_basis": "synthetic_zero_nonrecoverable", "cost_basis": cstapi.CostBasisNominalDirect,
				"sales_trade_terms_ref": b.trade, "sales_payment_terms_ref": b.payment,
				"delivery": map[string]any{"kind": cstapi.DeliveryFixed, "date": "2027-01-31", "milestone": cstapi.MilestoneHandover,
					"place_ref": b.place, "time_zone": "Asia/Shanghai"}}}})
	b.cmd(brcCostChecker, "cst", cstapi.Contract, "costing_context.verify", map[string]any{"id": "context.1", "expected_version": 1,
		"working_revision": 1, "evidence": []any{map[string]any{"source_id": "source.1", "locator": "第1段：四个SKU合计120 PCS"}}})

	evaluations := map[string]map[string]any{}
	for _, s := range []struct{ scenario, offer, total string }{{"scenario.A", "offer.A", "66000"}, {"scenario.B", "offer.B", "69600"}} {
		b.cmd(brcBuyer, "cst", cstapi.Contract, "cost_scenario.save_draft", map[string]any{"id": s.scenario, "expected_version": 0,
			"data": b.scenario(s.offer, "同规格同条款："+s.offer+" 全部四行，含 log.1 海运费")})
		r := b.cmd(brcCostChecker, "cst", cstapi.Contract, "cost_scenario.evaluate", map[string]any{"id": s.scenario, "expected_version": 1})
		e := b.get(brcCostChecker, "/api/brant/v1/cst/cost_evaluations/"+brcStr(r["object_id"]))
		if brcDig(e, "result", "total_minor") != s.total || brcDig(e, "result", "currency") != "USD" || e["result_hash"] == "" {
			b.t.Fatalf("evaluation of %s: %v", s.scenario, e["result"])
		}
		b.fact("evaluation_id", e["evaluation_id"], "result_hash", e["result_hash"], "total_minor", s.total, "total", brcMajor(b.t, s.total)+" USD",
			"comparison_status", brcDig(e, "result", "comparison_status"))
		evaluations[s.scenario] = e
	}
	a := evaluations["scenario.A"]
	f := b.cmd(brcCostChecker, "cst", cstapi.Contract, "cost_snapshot.freeze", map[string]any{"scenario_id": "scenario.A", "expected_version": 1,
		"evaluation_id": a["evaluation_id"], "result_hash": a["result_hash"],
		"selection_reason": "同口径成本已核实：supplier.A 660.00 USD 低于 supplier.B 696.00 USD"})
	b.snapshotID = brcStr(f["object_id"])
	snap := b.get(brcCostChecker, "/api/brant/v1/cst/cost_snapshots/"+b.snapshotID)
	if snap["total_minor"] != "66000" || snap["currency"] != "USD" || snap["result_hash"] != a["result_hash"] || snap["scenario_id"] != "scenario.A" {
		b.t.Fatalf("snapshot %v", snap)
	}
	b.resultHash = brcStr(snap["result_hash"])
	b.record.Amounts["cost_snapshot_minor"] = brcStr(snap["total_minor"])
	b.record.Amounts["cost_snapshot"] = brcMajor(b.t, brcStr(snap["total_minor"])) + " USD"
	if b.record.Amounts["cost_snapshot"] != "660.00 USD" {
		b.t.Fatalf("cost snapshot %s", b.record.Amounts["cost_snapshot"])
	}
	b.record.Amounts["scenario_B_minor"] = "69600"
	b.fact("snapshot_id", b.snapshotID, "result_hash", b.resultHash, "total_minor", snap["total_minor"],
		"total", b.record.Amounts["cost_snapshot"])
	adopt := b.get(brcSales, "/api/brant/v1/cst/cost_snapshots/"+b.snapshotID+"/adoptability")
	if adopt["currently_eligible_for_quote"] != true {
		b.t.Fatalf("adoptability %v", adopt)
	}
	b.fact("currently_eligible_for_quote", true, "status", brcDig(adopt, "adoptability", "status"))
}

// quote is ES-01 steps 11–14: sales drafts quote.1 on the snapshot, freezes it with the server
// calculation and submits it; quote.reviewer approves; sales activates it.
func (b *brc001) quote() {
	b.phase = "TRD quote"
	proof := []any{map[string]any{"source_id": "source.1", "locator": "第1段"}}
	var lines []any
	for i, l := range brcLines {
		lines = append(lines, map[string]any{"line_id": l.id, "rfq_line_id": l.id, "sku_ref": map[string]any{"sku_id": l.sku, "spec_revision": 1},
			"quantity": l.qty, "unit_ref": b.pcs, "unit_price": "8.00", "price_basis_quantity": "1", "discount_ratio": "0",
			"tax_application_ref": b.sale[l.sku], "evidence": []any{map[string]any{"source_id": "source.1", "locator": fmt.Sprintf("第1段，第%d项", i+1)}}})
	}
	data := map[string]any{"named_place_ref": b.place,
		"delivery": map[string]any{"kind": trdapi.DeliveryFixed, "date": "2027-01-31", "milestone": trdapi.MilestoneHandover, "place_ref": b.place,
			"time_zone": "Asia/Shanghai"},
		"issuer_ref":            b.company,
		"customer_ref":          map[string]any{"party_id": "customer.1", "revision": b.customerRevision},
		"contact_ref":           map[string]any{"contact_id": "contact.1", "row_version": 2},
		"trading_profile_ref":   map[string]any{"party_id": "customer.1", "revision": 1},
		"customer_address_refs": []any{map[string]any{"party_id": "customer.1", "party_revision": b.customerRevision, "address_id": "hq"}},
		"rfq_ref":               map[string]any{"rfq_id": "rfq.1", "row_version": b.rfqVersion},
		"cost_snapshot_ref":     map[string]any{"snapshot_id": b.snapshotID, "result_hash": b.resultHash},
		"lines":                 lines, "currency_ref": b.usd, "pricing_policy_ref": b.policy, "trade_terms_ref": b.trade,
		"payment_terms_ref": b.payment, "document_template_ref": b.template, "sequence_ref": b.seq,
		"expires_at":     kernel.FormatTimestamp(time.Now().Add(72 * time.Hour)),
		"customer_notes": "客户可见说明：BRC-001 合成样例，ST01 红/蓝 × S/M。", "evidence": proof}
	const trd = trdapi.Contract
	if r := b.cmd(brcSales, "trd", trd, "quote.save_draft", map[string]any{"quote_id": "quote.1", "expected_version": 0, "data": data}); r["status"] != trdapi.StatusWorking {
		b.t.Fatalf("save_draft %v", r)
	}
	if r := b.cmd(brcSales, "trd", trd, "quote.freeze", map[string]any{"quote_id": "quote.1", "expected_version": 1, "working_revision": 1}); r["status"] != trdapi.StatusFrozen ||
		r["row_version"] != float64(2) {
		b.t.Fatalf("freeze %v", r)
	}
	q := b.get(brcSales, "/api/brant/v1/trd/quotes/quote.1")
	rev := brcMap(brcDig(q, "revisions", 0))
	b.quoteHash = brcStr(rev["business_hash"])
	calc := brcMap(rev["calculation"])
	if len(b.quoteHash) != 64 || calc["net_minor"] != "96000" || calc["tax_minor"] != "0" || calc["payable_minor"] != "96000" ||
		calc["cost_minor"] != "66000" || calc["profit_minor"] != "30000" || calc["margin_rule"] != "pass" ||
		brcDig(calc, "payments", 0, "amount_minor") != "28800" || brcDig(calc, "payments", 1, "amount_minor") != "67200" {
		b.t.Fatalf("frozen calculation %v", calc)
	}
	b.fact("business_revision", rev["revision"], "business_hash", b.quoteHash, "net_minor", calc["net_minor"], "payable_minor", calc["payable_minor"],
		"payable", brcMajor(b.t, brcStr(calc["payable_minor"]))+" USD", "cost_minor", calc["cost_minor"], "profit_minor", calc["profit_minor"],
		"minimum_margin", calc["minimum_margin"], "margin_rule", calc["margin_rule"],
		"payments_minor", []any{brcDig(calc, "payments", 0, "amount_minor"), brcDig(calc, "payments", 1, "amount_minor")})
	b.record.Amounts["quote_payable_minor"] = brcStr(calc["payable_minor"])
	b.record.Amounts["quote_payable"] = brcMajor(b.t, brcStr(calc["payable_minor"])) + " USD"
	if b.record.Amounts["quote_payable"] != "960.00 USD" {
		b.t.Fatalf("quote payable %s", b.record.Amounts["quote_payable"])
	}
	b.record.Amounts["quote_profit"] = brcMajor(b.t, brcStr(calc["profit_minor"])) + " USD"
	b.record.Amounts["quote_payments"] = brcMajor(b.t, "28800") + " + " + brcMajor(b.t, "67200") + " USD"

	b.cmd(brcSales, "trd", trd, "quote.submit_review", map[string]any{"quote_id": "quote.1", "expected_version": 2, "business_revision": 1,
		"review_request_id": "review.quote.1", "target_command": trdapi.TargetActivate})
	decide := map[string]any{"quote_id": "quote.1", "expected_version": 3, "review_request_id": "review.quote.1", "business_hash": b.quoteHash,
		"decision": trdapi.DecisionApprove, "approval_id": "approval.quote.1", "expires_at": kernel.FormatTimestamp(time.Now().Add(24 * time.Hour)),
		"reason": "内容核对无误：毛利率 31.25% 不低于 25%", "evidence": proof}
	// The author froze and submitted the revision: it cannot approve it (TRD-INV-004).
	b.refused(brcSales, "trd", trd, "quote.review.decide", decide, http.StatusUnprocessableEntity, trdapi.CodeIndependentReviewRequired)
	b.cmd(brcQuoteReviewer, "trd", trd, "quote.review.decide", decide)
	check := b.get(brcSales, "/api/brant/v1/trd/quotes/quote.1/approvals/approval.quote.1/check")
	if check["releasable"] != true || check["business_hash"] != b.quoteHash {
		b.t.Fatalf("approval check %v", check)
	}
	b.fact("releasable", true)
	if r := b.cmd(brcSales, "trd", trd, "quote.activate", map[string]any{"quote_id": "quote.1", "expected_version": 4, "business_revision": 1,
		"approval_id": "approval.quote.1", "expected_approval_effectivity_version": 1}); r["status"] != trdapi.StatusActive || r["row_version"] != float64(5) {
		b.t.Fatalf("activate %v", r)
	}
	ready := b.get(brcSales, "/api/brant/v1/trd/quotes/quote.1/ready")
	if ready["ready"] != true || ready["business_hash"] != b.quoteHash {
		b.t.Fatalf("ready %v", ready)
	}
	b.fact("ready", true, "business_revision", ready["business_revision"])
}

// acceptance is ES-01 step 15: sales records the customer's full unconditional reply against the
// active revision; acceptance.verifier verifies it into acceptance.1.
func (b *brc001) acceptance() {
	b.phase = "TRD customer acceptance"
	b.source(brcSales, "mail", "合成客户邮件：确认接受报价全部条款与全部四行（ST01 红/蓝 × S/M，合计 120 PCS，960.00 USD）。")
	b.cmd(brcSales, "trd", trdapi.Contract, "quote.response.record", map[string]any{"response_id": "response.1", "quote_id": "quote.1",
		"business_revision": 1, "business_hash": b.quoteHash, "classification": trdapi.ClassificationFullUnconditional, "summary": "客户回复：全部接受",
		"source_ref": map[string]any{"source_id": "mail", "locator": "response.1"}, "received_at": kernel.FormatTimestamp(time.Now()),
		"received_at_basis": trdapi.ArrivalChannelServer})
	b.cmd(brcVerifier, "trd", trdapi.Contract, "quote.response.verify", map[string]any{"response_id": "response.1", "acceptance_id": "acceptance.1",
		"business_revision": 1, "business_hash": b.quoteHash, "customer_ref": map[string]any{"party_id": "customer.1", "revision": b.customerRevision},
		"contact_ref": map[string]any{"contact_id": "contact.1", "row_version": 2}, "line_ids": brcLineIDs()})
	response := b.get(brcSales, "/api/brant/v1/trd/customer_responses/response.1")
	if response["classification"] != trdapi.ClassificationFullUnconditional {
		b.t.Fatalf("response %v", response)
	}
	b.fact("classification", response["classification"], "business_hash", response["business_hash"])
	a := b.get(brcSales, "/api/brant/v1/trd/acceptances/acceptance.1")
	if a["formed"] != true || a["effectivity"] != trdapi.AcceptanceValid || brcDig(a, "verified_by", "id") != brcVerifier ||
		a["business_hash"] != b.quoteHash || len(brcList(a["line_ids"])) != len(brcLines) {
		b.t.Fatalf("acceptance %v", a)
	}
	b.number = brcStr(a["number"])
	b.fact("formed", true, "formation_rule", a["formation_rule"], "effectivity", a["effectivity"], "number", b.number,
		"arrival_at", a["arrival_at"], "expires_at", a["expires_at"])
}

// order is ES-01 steps 16–18: sales prepares order.1 from the acceptance and requests its review;
// order.reviewer approves; sales activates it.
func (b *brc001) order() {
	b.phase = "TRD sales order"
	const trd = trdapi.Contract
	if r := b.cmd(brcSales, "trd", trd, "order.prepare", map[string]any{"order_id": "order.1", "quote_id": "quote.1", "business_revision": 1,
		"business_hash": b.quoteHash, "acceptance_id": "acceptance.1"}); r["status"] != trdapi.OrderDraft || r["row_version"] != float64(1) {
		b.t.Fatalf("order.prepare %v", r)
	}
	o := b.get(brcSales, "/api/brant/v1/trd/orders/order.1")
	b.orderHash = brcStr(o["order_hash"])
	b.orderRevision = brcNum(o["order_revision"])
	snapshot := brcStr(o["snapshot"])
	if sha256Hex(snapshot) != b.orderHash || b.orderRevision != 1 {
		b.t.Fatalf("order %v", o)
	}
	b.fact("order_hash", b.orderHash, "order_revision", b.orderRevision, "slot", o["slot"])
	b.cmd(brcSales, "trd", trd, "order.review.request", map[string]any{"order_id": "order.1", "expected_version": 1, "review_id": "review.order.1"})
	decide := map[string]any{"order_id": "order.1", "expected_version": 2, "review_id": "review.order.1", "order_hash": b.orderHash,
		"decision": trdapi.DecisionApprove, "expires_at": kernel.FormatTimestamp(time.Now().Add(24 * time.Hour)), "reason": "订单与已接受的报价一致"}
	// The preparer and requester cannot decide the order's review (TRD-INV-013; Python A38
	// test_denied_without_approval_and_independent_reviewer).
	b.refused(brcSales, "trd", trd, "order.review.decide", decide, http.StatusUnprocessableEntity, trdapi.CodeReviewerNotIndependent)
	b.cmd(brcOrderReviewer, "trd", trd, "order.review.decide", decide)
	// S5 B4: order.activate asks CNX's TradeComplianceGuard (TRD-INV-023), then STL's CreditGuard
	// for the 672.00 due after shipment (TRD-INV-022); order.check names the first that refuses.
	b.wantCheck(cnxapi.CodeTradeComplianceNotClear)
	b.tradeCompliance()
	b.wantCheck(stlapi.CodeCreditReservationRequired)
	b.creditReserved()
	b.phase = "TRD sales order"
	check := b.get(brcSales, "/api/brant/v1/trd/orders/order.1/check?review_id=review.order.1")
	if check["activatable"] != true || len(brcList(check["risks"])) != 0 || len(brcList(check["unchecked"])) != 0 {
		b.t.Fatalf("order check %v", check)
	}
	b.fact("activatable", true, "unchecked", check["unchecked"])
	if r := b.cmd(brcSales, "trd", trd, "order.activate", map[string]any{"order_id": "order.1", "expected_version": 3,
		"review_id": "review.order.1"}); r["status"] != trdapi.OrderActive || r["row_version"] != float64(4) {
		b.t.Fatalf("order.activate %v", r)
	}
	o = b.get(brcSales, "/api/brant/v1/trd/orders/order.1")
	entries := brcMap(brcDig(o, "commits", 0, "entries"))
	if entries[trdapi.GuardSTLCredit] != stlapi.CreditCovered || entries[trdapi.EvidenceCreditReservation] != "res.order.1" ||
		entries[trdapi.EvidenceCreditPortion] != "67200" || entries[trdapi.EvidenceCreditBasis] != stlapi.BasisWithinLimit ||
		!strings.HasPrefix(brcStr(entries[trdapi.GuardCNXTradeCompliance]), "clear/cleared/") ||
		entries[trdapi.EvidenceComplianceRegulation] != "screening@1" {
		b.t.Fatalf("the activation's commit evidence %v", entries)
	}
	b.fact("credit_guard", entries[trdapi.GuardSTLCredit], "credit_reservation_id", entries[trdapi.EvidenceCreditReservation],
		"credit_portion_minor", entries[trdapi.EvidenceCreditPortion], "trade_compliance", entries[trdapi.GuardCNXTradeCompliance])
}

// wantCheck requires order.check of order.1 on its review to answer code.
func (b *brc001) wantCheck(code string) {
	b.t.Helper()
	check := b.get(brcSales, "/api/brant/v1/trd/orders/order.1/check?review_id=review.order.1")
	if check["activatable"] != false || check["code"] != code {
		b.t.Fatalf("order check: want %s, got %v", code, check)
	}
	b.fact("activatable", false, "code", code)
}

// tradeCompliance is ES-11 step 5 (S5-13, CNX-INV-001; D-8, D-22): compliance imports the synthetic
// CN and UN country programme lists; documentation clerk doc.1 classifies each of the four SKUs
// under code table CN-2026 and doc.2 verifies it (CAT-INV-002); compliance evaluates order.1 at
// revision 1 — the destination and the buyer address's country entered on evidence (PTY recorded
// none), the end use stated — and concludes clear.
func (b *brc001) tradeCompliance() {
	b.phase = "CNX trade compliance"
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
	for _, l := range brcLines {
		id := "decl." + l.sku
		b.cmd(brcDocumentation, "cat", catapi.Contract, catapi.CommandSaveDeclarationProfile, map[string]any{"id": id, "expected_version": 0,
			"sku_id": l.sku, "spec_revision": 1, "code_table_version": codeTable,
			"commodity":        map[string]any{"code": "6109100021", "basis": "合成：针织棉制 T 恤，按合成编码表归类"},
			"declaration_name": "棉制针织T恤衫",
			"elements":         []any{map[string]any{"name": "品名", "value": "T恤衫"}, map[string]any{"name": "成分含量", "value": "100%棉"}},
			"legal_units":      []any{map[string]any{"unit": "件", "per_base_unit": "1", "basis": "一件即一 PCS"}},
			"origin":           map[string]any{"country": "CN", "criterion": catapi.OriginWhollyObtained, "basis": "合成：境内生产与缝制"},
			"export_control":   map[string]any{"status": catapi.ExportNotControlled, "basis": "合成：不在合成管制清单内"}})
		b.cmd(brcDocVerifier, "cat", catapi.Contract, catapi.CommandVerifyDeclarationProfile, map[string]any{"id": id, "expected_version": 1,
			"revision": 1, "evidence_ids": []string{"evidence"}, "reason": "单证专业核实（合成）"})
	}
	evidence := []string{"evidence"}
	e := b.cmd(brcCompliance, "cnx", cnxapi.Contract, cnxapi.CommandEvaluate, map[string]any{"order_id": "order.1", "order_revision": 1,
		"expected_version": 0, "regulation_id": "screening", "regulation_revision": 1, "programme_lists": []string{cnProgrammeList, unProgrammeList},
		"code_table_version": codeTable,
		"destination":        map[string]any{"country": syntheticClear, "reason": "客户 PO 载明的卸货国（合成）", "evidence_ids": evidence},
		"address_countries": []any{map[string]any{"party_id": "customer.1", "address_id": "hq", "country": syntheticClear,
			"evidence_ids": evidence}},
		"end_use": map[string]any{"statement": "服装零售（合成）", "evidence_ids": evidence}})
	checkID := brcStr(e["object_id"])
	view := b.get(brcCompliance, "/api/brant/v1/cnx/trade_compliance_checks/"+checkID)
	evaluation := brcMap(brcDig(view, "evaluations", 0))
	if evaluation["outcome"] != cnxapi.OutcomeNoMatch || len(brcList(evaluation["lines"])) != len(brcLines) {
		b.t.Fatalf("evaluation %v", evaluation)
	}
	d := b.cmd(brcCompliance, "cnx", cnxapi.Contract, cnxapi.CommandDecide, map[string]any{"check_id": checkID, "expected_version": 1,
		"evaluation_id": evaluation["evaluation_id"], "conclusion": cnxapi.ConclusionClear, "reason": "目的国、交易方所在国与商品核查完成（合成）",
		"license_required": false, "evidence_ids": evidence})
	if d["status"] != cnxapi.StatusClear {
		b.t.Fatalf("decide %v", d)
	}
	b.fact("check_id", checkID, "conclusion", d["status"], "order_revision", 1)
}

// creditReserved is ES-11's credit step (S5-12, STL-INV-004; D-6, D-7; A6 gap 7): POL publishes the
// synthetic E-2 credit policy; sales requests customer.1's limit of 10,000.00 USD on the evidence
// and two credit managers publish it (above the top tier of 5,000: two approvers); sales reserves
// the 672.00 of order.1's revision 1 due 30 days after shipment, within the limit.
func (b *brc001) creditReserved() {
	b.phase = "STL credit"
	policy := b.config("credit_policy", "credit.usd", map[string]any{"name": "E-2 synthetic credit policy", "currency": "USD", "default_limit": "0",
		"limit_sources":          []string{polapi.LimitSourceInternalAssessment, polapi.LimitSourceSinosureBuyerLimit},
		"approval_tiers":         []any{map[string]any{"level": "credit_officer", "max_amount": "1000"}, map[string]any{"level": "credit_manager", "max_amount": "5000"}},
		"overdue_tolerance_days": 30, "documents_secured": true, "reservation_validity_days": 30, "decision_validity_days": 7,
		"two_approvers_above_top": true})
	b.cmd(brcSales, "stl", stlapi.Contract, stlapi.CommandRequestLimit, map[string]any{"request_id": "limit.1", "party_id": "customer.1",
		"currency": b.usd, "sources": []any{map[string]any{"source": "internal_assessment", "amount": "10000.00", "evidence_ids": []string{"evidence"}}},
		"valid_from": kernel.FormatTimestamp(b.from), "valid_until": kernel.FormatTimestamp(b.from.Add(365 * 24 * time.Hour)),
		"credit_policy": policy, "reason": creditReason})
	for i, m := range []string{brcCreditManager1, brcCreditManager2} {
		b.cmd(m, "stl", stlapi.Contract, stlapi.CommandPublishLimit, map[string]any{"request_id": "limit.1", "expected_version": i + 1,
			"decision": stlapi.DecisionPublish, "credit_policy": policy, "reason": creditReason})
	}
	r := b.cmd(brcSales, "stl", stlapi.Contract, stlapi.CommandReserve, map[string]any{"reservation_id": "res.order.1", "order_id": "order.1",
		"order_revision": 1, "order_hash": b.orderHash, "credit_policy": policy})
	if r["status"] != stlapi.ReservationReserved {
		b.t.Fatalf("credit.reserve %v", r)
	}
	v := b.get(brcSales, "/api/brant/v1/stl/credit_reservations/res.order.1")
	if v["portion_minor"] != "67200" || v["basis"] != stlapi.BasisWithinLimit || v["order_hash"] != b.orderHash {
		b.t.Fatalf("reservation %v", v)
	}
	b.record.Amounts["credit_reserved_minor"] = "67200"
	b.record.Amounts["credit_reserved"] = brcMajor(b.t, "67200") + " USD"
	b.fact("portion_minor", v["portion_minor"], "basis", v["basis"], "exposure_minor", v["exposure_minor"])
}

// purchase is ES-01 step 19: purchaser prepares purchase.1 from supplier.A's offer over the four
// lines of the active order; the ST01 group meets its MOQ together.
func (b *brc001) purchase() {
	b.phase = "PUR purchase draft"
	var lines []any
	for i, l := range brcLines {
		lines = append(lines, map[string]any{"order_line_id": l.id, "quantity": l.qty,
			"source": map[string]any{"kind": purapi.KindSupplierOffer, "id": "offer.A", "revision": 1, "condition_line_id": fmt.Sprintf("OL%d", i+1)}})
	}
	r := b.cmd(brcPurchaser, "pur", purapi.Contract, "purchase.draft.prepare", map[string]any{"draft_id": "purchase.1", "order_id": "order.1",
		"selection_reason": "按成本快照选定 supplier.A 报价：5.00 USD/PCS，MOQ 100 PCS 按 ST01 款号合计", "lines": lines})
	if r["status"] != purapi.StatusPrepared || r["row_version"] != float64(1) {
		b.t.Fatalf("purchase.draft.prepare %v", r)
	}
}

// readBack reads the chain's results through the read routes: the quote, its active revision and
// formal document, quote_ready, the order, the draft bound to the order as ActiveOrderRef v1 showed
// it, and the order's allocations.
func (b *brc001) readBack() {
	b.phase = "read back"
	q := b.get(brcSales, "/api/brant/v1/trd/quotes/quote.1")
	if q["active_revision"] != float64(1) || brcDig(q, "conversion", "order_id") != "order.1" ||
		brcDig(q, "revisions", 0, "calculation", "payable_minor") != "96000" {
		b.t.Fatalf("quote %v", q)
	}
	b.fact("active_revision", 1, "conversion_order_id", "order.1", "conversion_slot", brcDig(q, "conversion", "slot"),
		"payable_minor", "96000")

	active := b.get(brcSales, "/api/brant/v1/trd/quotes/quote.1/active")
	if active["active"] != true || active["business_hash"] != b.quoteHash || active["number"] != b.number || active["conversion_taken"] != true ||
		!strings.HasPrefix(b.number, "QT-") {
		b.t.Fatalf("active %v", active)
	}
	b.fact("number", b.number, "document_sha256", active["document_sha256"], "conversion_taken", true)

	doc := b.get(brcSales, "/api/brant/v1/trd/quotes/quote.1/revisions/1/document")
	content, err := base64.StdEncoding.DecodeString(brcStr(doc["content"]))
	b.w.must(err)
	formal := brcMap(doc["formal"])
	if formal["document_sha256"] != sha256Hex(string(content)) || formal["document_sha256"] != active["document_sha256"] ||
		formal["number"] != b.number || !strings.Contains(string(content), "Number: "+b.number) ||
		!strings.Contains(string(content), "Net: 96000 | Tax: 0 | Payable: 96000") ||
		!strings.Contains(string(content), "P1 | 30% | 28800") || !strings.Contains(string(content), "P2 | 70% | 67200") {
		b.t.Fatalf("formal document %v\n%s", formal, content)
	}
	for _, l := range brcLines {
		if row := fmt.Sprintf("%s | %s | %s r1 | %s PCS | price 8 per 1 | discount 0 | net %s | tax 0 | payable %s", l.id, l.sku, l.sku, l.qty,
			l.net, l.net); !strings.Contains(string(content), row) {
			b.t.Fatalf("the formal document lacks %q:\n%s", row, content)
		}
	}
	// quote-text/0.1.0 states amounts in minor units (Python t03_candidate/document.py).
	b.fact("number", b.number, "document_sha256", formal["document_sha256"], "bytes", len(content), "renderer", formal["renderer_version"],
		"states", []string{"Net: 96000 | Tax: 0 | Payable: 96000", "P1 | 30% | 28800", "P2 | 70% | 67200"})

	ready := b.get(brcSales, "/api/brant/v1/trd/quotes/quote.1/ready")
	if ready["business_hash"] != b.quoteHash || ready["business_revision"] != float64(1) {
		b.t.Fatalf("ready %v", ready)
	}
	var reasons []any
	for _, r := range brcList(ready["reasons"]) {
		reasons = append(reasons, brcDig(r, "code"))
	}
	b.fact("ready", ready["ready"], "reasons", reasons)

	o := b.get(brcSales, "/api/brant/v1/trd/orders/order.1")
	var snapshot map[string]any
	b.w.must(json.Unmarshal([]byte(brcStr(o["snapshot"])), &snapshot))
	if o["status"] != trdapi.OrderActive || o["order_hash"] != b.orderHash || o["activated_review_id"] != "review.order.1" ||
		brcDig(snapshot, "calculation", "net_minor") != "96000" || brcDig(snapshot, "calculation", "payable_minor") != "96000" ||
		len(brcList(snapshot["lines"])) != len(brcLines) || brcDig(o, "commits", 0, "cost_snapshot_id") != b.snapshotID {
		b.t.Fatalf("order %v", o)
	}
	var linePayable []any
	for i, l := range brcLines {
		line := brcMap(brcDig(snapshot, "calculation", "lines", i))
		if line["id"] != l.id || line["net_minor"] != l.net || line["tax_minor"] != "0" || line["payable_minor"] != l.net {
			b.t.Fatalf("the order's line %s: %v", l.id, line)
		}
		linePayable = append(linePayable, line["payable_minor"])
	}
	b.record.Amounts["order_payable_minor"] = brcStr(brcDig(snapshot, "calculation", "payable_minor"))
	b.record.Amounts["order_payable"] = brcMajor(b.t, brcStr(brcDig(snapshot, "calculation", "payable_minor"))) + " USD"
	if b.record.Amounts["order_payable"] != "960.00 USD" {
		b.t.Fatalf("order payable %s", b.record.Amounts["order_payable"])
	}
	b.fact("status", o["status"], "row_version", o["row_version"], "order_revision", o["order_revision"], "order_hash", b.orderHash,
		"net_minor", "96000", "payable_minor", "96000", "payable", b.record.Amounts["order_payable"], "line_payable_minor", linePayable,
		"cost_snapshot_id", b.snapshotID)

	d := b.get(brcPurchaser, "/api/brant/v1/pur/purchase_drafts/purchase.1")
	if d["state"] != purapi.StatusPrepared || d["order_id"] != "order.1" || d["order_hash"] != b.orderHash ||
		d["order_revision"] != float64(b.orderRevision) || d["supplier_id"] != "supplier.A" || d["currency"] != "USD" ||
		d["net_minor"] != "60000" || d["tax_minor"] != "0" || d["gross_minor"] != "60000" ||
		d["commitment_state"] != purapi.CommitmentNotCommitted || len(brcList(d["lines"])) != len(brcLines) {
		b.t.Fatalf("draft %v", d)
	}
	for i, l := range brcLines {
		line := brcMap(brcDig(d, "lines", i))
		if line["order_line_id"] != l.id || line["net_minor"] != l.purchase || line["purchase_quantity"] != l.qty || line["same_source"] != true ||
			line["cost_variance_minor"] != "0" {
			b.t.Fatalf("draft line %d %v", i, line)
		}
	}
	var document map[string]any
	b.w.must(json.Unmarshal([]byte(brcStr(d["document"])), &document))
	if brcDig(document, "order", "quote_id") != "quote.1" || brcDig(document, "order", "order_review_id") != "review.order.1" ||
		brcDig(document, "costing_snapshot", "snapshot_id") != b.snapshotID {
		b.t.Fatalf("draft document %v", document)
	}
	b.record.Amounts["purchase_draft_net_minor"] = brcStr(d["net_minor"])
	b.record.Amounts["purchase_draft"] = brcMajor(b.t, brcStr(d["net_minor"])) + " USD"
	if b.record.Amounts["purchase_draft"] != "600.00 USD" {
		b.t.Fatalf("purchase draft %s", b.record.Amounts["purchase_draft"])
	}
	b.fact("order_id", d["order_id"], "order_revision", d["order_revision"], "order_hash", d["order_hash"], "draft_hash", d["draft_hash"],
		"supplier_id", d["supplier_id"], "net_minor", d["net_minor"], "gross_minor", d["gross_minor"], "net", b.record.Amounts["purchase_draft"],
		"commitment_state", d["commitment_state"], "line_net_minor", []string{"12000", "18000", "6000", "24000"})

	b.allocations("allocations")
}

// allocations reads order.1's demand allocations: every line fully drafted.
func (b *brc001) allocations(what string) {
	alloc := b.get(brcPurchaser, "/api/brant/v1/pur/orders/order.1/allocations")
	lines := brcList(alloc["allocations"])
	if len(lines) != len(brcLines) {
		b.t.Fatalf("%s %v", what, alloc)
	}
	var allocated []any
	for i, l := range brcLines {
		a := brcMap(lines[i])
		if a["order_line_id"] != l.id || a["demand"] != l.qty || a["allocated"] != l.qty || a["remaining"] != "0" ||
			a["order_revision"] != float64(b.orderRevision) {
			b.t.Fatalf("%s line %d %v", what, i, a)
		}
		allocated = append(allocated, a["allocated"])
	}
	b.fact("allocated", allocated, "remaining", "0")
}

// uniqueness is the second half of the acceptance (有效订单唯一): no second order of quote.1, and no
// purchase beyond an order line.
func (b *brc001) uniqueness() {
	b.phase = "有效订单唯一"
	b.refused(brcSales, "trd", trdapi.Contract, "order.prepare", map[string]any{"order_id": "order.2", "quote_id": "quote.1", "business_revision": 1,
		"business_hash": b.quoteHash, "acceptance_id": "acceptance.1"}, http.StatusConflict, trdapi.CodeConversionSlotConflict)
	b.read(brcSales, "/api/brant/v1/trd/orders/order.2", http.StatusNotFound)
	b.refused(brcPurchaser, "pur", purapi.Contract, "purchase.draft.prepare", map[string]any{"draft_id": "purchase.2", "order_id": "order.1",
		"selection_reason": "补采一件（超出订单行）", "lines": []any{map[string]any{"order_line_id": "L1", "quantity": "1",
			"source": map[string]any{"kind": purapi.KindSupplierOffer, "id": "offer.B", "revision": 1, "condition_line_id": "OL1"}}}},
		http.StatusConflict, purapi.CodePurchaseAllocationExceeded)
	b.read(brcPurchaser, "/api/brant/v1/pur/purchase_drafts/purchase.2", http.StatusNotFound)
	b.allocations("allocations after the refusals")
	q := b.get(brcSales, "/api/brant/v1/trd/quotes/quote.1")
	if brcDig(q, "conversion", "order_id") != "order.1" {
		b.t.Fatalf("conversion %v", q["conversion"])
	}
	b.fact("conversion_order_id", "order.1")
}
