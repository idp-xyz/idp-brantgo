//go:build integration

package bootstrap_test

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The price proposal (S4 unit G4a; owner decision 2026-11-18: a quotation's unit price comes from a
// sales price list where one is in force, otherwise from the formula cost ÷ (1 − target margin);
// people may change the price, and a price under the floor goes through the margin exception)
// composed on PostgreSQL 16 in the BRC-001 world of the S3-12 chain (brc001: 24 + 36 + 12 + 48 PCS,
// cost snapshot 660.00 USD, 5.50 a piece on every line; minimum margin 0.25), over the HTTP API
// with OIDC access tokens and through TRD's Go api. Python V0.50 proposes no price and the design
// sources do not name the read yet, so the tests cite the decision.

// The readers the proposal's refusals need besides the chain's humans.
const (
	g4aNoRead   = "reader.without.trd" // an active grant without trd.read
	g4aOutsider = "reader.own.scope"   // trd.read with data scope own: sees none of sales' RFQs
	g4aAgent    = "agent.q01"          // an AI actor with trd.read
)

// newPriceProposalWorld is BRC-001 up to its frozen cost snapshot — the moment a quote is drafted —
// with policy.1's target margin (none when empty).
func newPriceProposalWorld(t *testing.T, targetMargin string) *brc001 {
	b := newBRC001(t)
	b.report, b.targetMargin = "BRANT_G4A_PRICE_PROPOSAL_REPORT", targetMargin
	b.record.Scenario, b.record.WBS, b.record.Parity = "BRC-001 price proposal", "S4 G4a", "owner decision 2026-11-18"
	// Provisioned before the first command, like the chain's humans, so no authority epoch moves
	// while the quote is reviewed.
	scope := b.w.scope("company.A")
	b.w.provision(bootstrap.Grant{Scope: scope, Actor: actorRef("human", g4aNoRead), Permissions: []string{"inq.read", "pol.read", "cst.read"},
		DataScope: "company", Team: "trade"})
	b.w.provision(bootstrap.Grant{Scope: scope, Actor: actorRef("human", g4aOutsider), Permissions: []string{"trd.read", "inq.read", "pol.read", "cst.read"},
		DataScope: "own", Team: "other"})
	b.w.provision(bootstrap.Grant{Scope: scope, Actor: actorRef("ai", g4aAgent), Permissions: []string{"trd.read"}, DataScope: "company", Team: "trade"})
	b.foundation()
	b.front()
	b.customerReady()
	b.sourcing()
	b.costing()
	b.phase = "TRD price proposal"
	return b
}

// as is the request context of actor (kind human or ai) in company.A, for TRD's Go api.
func (b *brc001) as(kind, actor string) context.Context {
	ctx, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: actorRef(kind, actor), Scope: b.w.scope("company.A")})
	b.w.must(err)
	return ctx
}

// proposalRoute is the route of rfq's price proposal: in USD under policy.1 revision 1 on the
// chain's cost snapshot, unless the pairs kv say otherwise; a parameter set to "" is left out.
func (b *brc001) proposalRoute(rfq string, kv ...string) string {
	q := map[string]string{"currency": "USD", "pricing_policy_id": "policy.1", "pricing_policy_revision": "1",
		"cost_snapshot_id": b.snapshotID, "cost_snapshot_result_hash": b.resultHash}
	for i := 0; i+1 < len(kv); i += 2 {
		q[kv[i]] = kv[i+1]
	}
	v := url.Values{}
	for k, s := range q {
		if s != "" {
			v.Set(k, s)
		}
	}
	return "/api/brant/v1/trd/rfqs/" + rfq + "/price_proposal?" + v.Encode()
}

// proposedLines is every line of proposal p as one line of text: the RFQ line, the source with the
// reason or the list price used, the unit price per basis quantity, what the formula used, and the
// net amount on the snapshot's cost with the margin it gives.
func proposedLines(p map[string]any) []string {
	var out []string
	for _, l := range brcList(p["lines"]) {
		m := brcMap(l)
		s := brcStr(m["rfq_line_id"]) + " " + brcStr(m["source"])
		if r := brcStr(m["reason"]); r != "" {
			s += " " + r
		}
		if scope := brcStr(m["price_list_scope"]); scope != "" {
			s += fmt.Sprintf(" %s %s@%d", scope, brcStr(brcDig(m, "price_list_ref", "id")), brcNum(brcDig(m, "price_list_ref", "revision")))
		}
		if price := brcStr(m["unit_price"]); price != "" {
			s += " " + price + "/" + brcStr(m["price_basis_quantity"])
		}
		if c := brcStr(m["unit_cost"]); c != "" {
			s += " cost " + c + " target " + brcStr(m["target_margin"])
		}
		if n := brcStr(m["net_minor"]); n != "" {
			s += " net " + n + " on " + brcStr(m["cost_minor"]) + " margin " + brcStr(m["margin"])
		}
		if m["under_floor"] == true {
			s += " UNDER FLOOR"
		}
		out = append(out, s)
	}
	return out
}

// propose reads the price proposal of path as sales and requires its lines.
func (b *brc001) propose(path string, want ...string) map[string]any {
	b.t.Helper()
	p := b.get(brcSales, path)
	got := proposedLines(p)
	b.fact("lines", got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		b.t.Fatalf("GET %s\ngot:\n  %s\nwant:\n  %s", path, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	return p
}

// proposalRefused requires the read of path as actor to be refused with status and code at field.
func (b *brc001) proposalRefused(actor, path string, status int, code, field string) {
	b.t.Helper()
	out := b.read(actor, path, status)
	if out["ok"] != false || out["code"] != code || brcDig(out, "field_errors", 0, "path") != field {
		b.t.Fatalf("GET %s as %s: want %s at %s, got %v", path, actor, code, field, out)
	}
}

// salesPrice has admin draft and config.reviewer publish the sales price id of sku at amount per
// piece in currency, for party or, with "", for every customer.
func (b *brc001) salesPrice(id, sku, party, currency, amount string) {
	b.t.Helper()
	data := map[string]any{"sku_id": sku, "currency": currency, "unit": "PCS", "unit_price": amount, "price_basis_quantity": "1"}
	if party != "" {
		data["party_id"] = party
	}
	b.config(polapi.KindSalesPrice, id, data)
}

var g4aFormulaLines = []string{
	"L1 formula 8.00/1 cost 5.50 target 0.3125 net 19200 on 13200 margin 0.3125",
	"L2 formula 8.00/1 cost 5.50 target 0.3125 net 28800 on 19800 margin 0.3125",
	"L3 formula 8.00/1 cost 5.50 target 0.3125 net 9600 on 6600 margin 0.3125",
	"L4 formula 8.00/1 cost 5.50 target 0.3125 net 38400 on 26400 margin 0.3125",
}

// With a target margin of 0.3125 on policy.1 the formula proposes BRC-001's four lines at 8.00 a
// piece on their unit cost of 5.50 — 960.00 in all on a cost of 660.00 — to the sales clerk and to
// an AI agent alike, and writes nothing. A person stays in charge: a draft at another price is
// taken and judged by the margin rule that exists, and the draft at the proposed prices is
// BRC-001's quote. A general list price then wins over the formula, the customer's own over the
// general one, a list price under the floor is flagged; another currency, a missing or unadoptable
// snapshot and the refusals follow; every read and refusal is in TRD's audit trail.
func TestPriceProposalOnBRC001(t *testing.T) {
	b := newPriceProposalWorld(t, "0.3125")
	w := b.w

	// 1. The formula.
	p := b.propose(b.proposalRoute("rfq.1"), g4aFormulaLines...)
	if p["rfq_id"] != "rfq.1" || brcNum(p["rfq_row_version"]) != b.rfqVersion || p["customer_id"] != "customer.1" || p["currency"] != "USD" ||
		brcNum(p["price_precision"]) != 2 || p["minimum_margin"] != "0.25" || p["target_margin"] != "0.3125" ||
		brcDig(p, "pricing_policy_ref", "kind") != "pricing_policy" || brcDig(p, "pricing_policy_ref", "id") != "policy.1" ||
		brcDig(p, "cost_snapshot", "snapshot_id") != b.snapshotID || brcDig(p, "cost_snapshot", "result_hash") != b.resultHash ||
		brcDig(p, "cost_snapshot", "status") != "adoptable" || brcDig(p, "cost_snapshot", "adoptable") != true || brcStr(p["checked_at"]) == "" {
		t.Fatalf("proposal %v", p)
	}
	total := new(big.Int)
	for _, l := range brcList(p["lines"]) {
		n, _ := new(big.Int).SetString(brcStr(brcMap(l)["net_minor"]), 10)
		total.Add(total, n)
	}
	if total.String() != "96000" {
		t.Fatalf("the four lines are %s, want 96000", total)
	}
	b.fact("total_minor", total.String(), "total", brcMajor(t, total.String())+" USD")
	// The customer may be named, and is the RFQ's.
	b.propose(b.proposalRoute("rfq.1", "customer_id", "customer.1"), g4aFormulaLines...)

	// The same answer through TRD's Go api, to the sales clerk and to an AI agent holding trd.read.
	query := trdapi.PriceProposalQuery{RfqID: "rfq.1", Currency: "USD",
		CostSnapshotRef:  &trdapi.CostSnapshotRef{SnapshotID: b.snapshotID, ResultHash: b.resultHash},
		PricingPolicyRef: trdapi.ConfigRef{Kind: trdapi.PricingPolicyKind, ID: "policy.1", Revision: 1}}
	for who, ctx := range map[string]context.Context{"sales": b.as("human", brcSales), "the agent": b.as("ai", g4aAgent)} {
		got, err := w.c.TRD.PriceProposal(ctx, query)
		w.must(err)
		if len(got.Lines) != 4 || got.PricePrecision != 2 || got.TargetMargin != "0.3125" || got.CostSnapshot == nil || !got.CostSnapshot.Adoptable ||
			got.CheckedAt.IsZero() {
			t.Fatalf("%s: %+v", who, got)
		}
		for i, l := range got.Lines {
			if l.RfqLineID != brcLines[i].id || l.SkuID != brcLines[i].sku || l.Quantity != brcLines[i].qty || l.Unit != "PCS" ||
				l.Source != trdapi.PriceSourceFormula || l.UnitPrice != "8.00" || l.PriceBasisQuantity != "1" || l.UnitCost != "5.50" ||
				l.NetMinor != brcLines[i].net || l.Margin != "0.3125" || l.UnderFloor || l.PriceListRef != nil {
				t.Fatalf("%s, line %d: %+v", who, i, l)
			}
		}
	}
	// A proposal writes nothing in TRD but its audit entries: no quote, no command result.
	if roots, results := w.count("company.A", "brant_trd.quote_root"), w.count("company.A", "brant_trd.action_result"); roots != 0 || results != 0 {
		t.Fatalf("%d quote roots and %d command results after reads", roots, results)
	}

	// 2. People stay in charge. The draft is unchanged: the sales clerk drafts 7.00 where the
	// proposal says 8.00, TRD takes it, and the freeze judges it as before the proposal existed
	// (840.00 on 660.00 is 21.43 %, under the floor: BC-AT06's requires_approval, shortfall 30).
	b.phase = "TRD quote at another price"
	b.unitPrices = map[string]string{"L1": "7.00", "L2": "7.00", "L3": "7.00", "L4": "7.00"}
	low, _ := b.quoteData()
	if r := b.cmd(brcSales, "trd", trdapi.Contract, "quote.save_draft", map[string]any{"quote_id": "quote.low", "expected_version": 0, "data": low}); r["status"] != trdapi.StatusWorking {
		t.Fatalf("a draft at another price: %v", r)
	}
	b.cmd(brcSales, "trd", trdapi.Contract, "quote.freeze", map[string]any{"quote_id": "quote.low", "expected_version": 1, "working_revision": 1})
	calc := brcMap(brcDig(b.get(brcSales, "/api/brant/v1/trd/quotes/quote.low"), "revisions", 0, "calculation"))
	if calc["net_minor"] != "84000" || calc["cost_minor"] != "66000" || calc["margin_rule"] != "requires_approval" || calc["margin_shortfall"] != "30" ||
		calc["minimum_margin"] != "0.25" {
		t.Fatalf("the draft at 7.00: %v", calc)
	}
	b.fact("net_minor", calc["net_minor"], "margin_rule", calc["margin_rule"], "margin_shortfall", calc["margin_shortfall"])
	// The draft at the proposed prices is BRC-001's quote: 960.00, cost 660.00, margin pass,
	// reviewed and activated (b.quote asserts each).
	b.unitPrices = map[string]string{}
	for _, l := range brcList(p["lines"]) {
		b.unitPrices[brcStr(brcMap(l)["rfq_line_id"])] = brcStr(brcMap(l)["unit_price"])
	}
	b.quote()
	if b.record.Amounts["quote_payable"] != "960.00 USD" {
		t.Fatalf("the quote at the proposed prices: %v", b.record.Amounts)
	}

	// 3. The price list. A general price of ST01.red.S wins over the formula for its line.
	b.phase = "TRD price proposal with a price list"
	b.salesPrice("price.red.S", "ST01.red.S", "", "USD", "8.40")
	general := "L1 price_list general price.red.S@1 8.40/1 net 20160 on 13200 margin 0.345238095238"
	b.propose(b.proposalRoute("rfq.1"), general, g4aFormulaLines[1], g4aFormulaLines[2], g4aFormulaLines[3])
	// The customer's own price wins over the general one; another customer's price is not its.
	b.salesPrice("price.red.S.c2", "ST01.red.S", "customer.2", "USD", "1.00")
	b.propose(b.proposalRoute("rfq.1"), general, g4aFormulaLines[1], g4aFormulaLines[2], g4aFormulaLines[3])
	b.salesPrice("price.red.S.c1", "ST01.red.S", "customer.1", "USD", "7.90")
	own := "L1 price_list customer price.red.S.c1@1 7.90/1 net 18960 on 13200 margin 0.303797468354"
	b.propose(b.proposalRoute("rfq.1"), own, g4aFormulaLines[1], g4aFormulaLines[2], g4aFormulaLines[3])
	// A list price under the floor is proposed as published, and flagged: 48 × 7.00 = 336.00 on
	// 264.00 is 3/14, below 0.25.
	b.salesPrice("price.blue.M", "ST01.blue.M", "", "USD", "7.00")
	under := "L4 price_list general price.blue.M@1 7.00/1 net 33600 on 26400 margin 0.214285714286 UNDER FLOOR"
	b.propose(b.proposalRoute("rfq.1"), own, g4aFormulaLines[1], g4aFormulaLines[2], under)

	// 4. Another currency: the cost is in USD, so the formula proposes nothing in EUR; a EUR list
	// price is proposed and not judged against a cost in another currency.
	b.phase = "TRD price proposal in another currency"
	b.config("currency", "EUR", map[string]any{"code": "EUR", "name": "Euro", "minor_units": 2, "rounding": polapi.RoundingHalfUp})
	p = b.propose(b.proposalRoute("rfq.1", "currency", "EUR"), "L1 none another_currency", "L2 none another_currency",
		"L3 none another_currency", "L4 none another_currency")
	if p["currency"] != "EUR" || brcDig(p, "cost_snapshot", "adoptable") != true {
		t.Fatalf("%v", p)
	}
	b.salesPrice("price.red.M.eur", "ST01.red.M", "", "EUR", "7.70")
	b.propose(b.proposalRoute("rfq.1", "currency", "EUR"), "L1 none another_currency", "L2 price_list general price.red.M.eur@1 7.70/1",
		"L3 none another_currency", "L4 none another_currency")

	// 5. No adoptable snapshot: none named, or one CST does not know under that result hash. List
	// prices are proposed without a judgement; the formula has no cost.
	b.phase = "TRD price proposal without a snapshot"
	unjudged := []string{"L1 price_list customer price.red.S.c1@1 7.90/1", "L2 none no_adoptable_snapshot", "L3 none no_adoptable_snapshot",
		"L4 price_list general price.blue.M@1 7.00/1"}
	p = b.propose(b.proposalRoute("rfq.1", "cost_snapshot_id", "", "cost_snapshot_result_hash", ""), unjudged...)
	if _, named := p["cost_snapshot"]; named {
		t.Fatalf("no snapshot was named: %v", p["cost_snapshot"])
	}
	p = b.propose(b.proposalRoute("rfq.1", "cost_snapshot_result_hash", strings.Repeat("0", 64)), unjudged...)
	if _, known := p["cost_snapshot"]; known {
		t.Fatalf("another result hash: %v", p["cost_snapshot"])
	}

	// 6. Refusals. An RFQ that is unknown, or outside the reader's data scope, is NOT_FOUND; a
	// reader without trd.read learns nothing; a draft RFQ has no proposal yet; the customer is the
	// RFQ's; a snapshot costs this RFQ; the policy and the currency are POL's, in force.
	b.phase = "TRD price proposal refusals"
	const invalid = http.StatusUnprocessableEntity
	b.proposalRefused(brcSales, b.proposalRoute("rfq.none"), http.StatusNotFound, kernel.CodeNotFound, "/rfq_id")
	b.proposalRefused(g4aOutsider, b.proposalRoute("rfq.1"), http.StatusNotFound, kernel.CodeNotFound, "/rfq_id")
	b.proposalRefused(g4aNoRead, b.proposalRoute("rfq.1"), http.StatusForbidden, kernel.CodePermissionDenied, "/")
	b.proposalRefused(g4aNoRead, b.proposalRoute("rfq.none"), http.StatusForbidden, kernel.CodePermissionDenied, "/")
	b.proposalRefused(brcSales, b.proposalRoute("rfq.1", "customer_id", "customer.2"), invalid, kernel.CodeReferenceInvalid, "/customer_id")
	b.proposalRefused(brcSales, b.proposalRoute("rfq.1", "pricing_policy_id", "policy.none"), invalid, kernel.CodeReferenceInvalid, "/pricing_policy_ref")
	b.proposalRefused(brcSales, b.proposalRoute("rfq.1", "pricing_policy_revision", "9"), invalid, kernel.CodeReferenceInvalid, "/pricing_policy_ref")
	b.proposalRefused(brcSales, b.proposalRoute("rfq.1", "currency", "JPY"), invalid, kernel.CodeReferenceInvalid, "/currency")
	b.proposalRefused(brcSales, b.proposalRoute("rfq.1", "currency", "usd"), invalid, kernel.CodeSchemaInvalid, "/currency")
	b.proposalRefused(brcSales, b.proposalRoute("rfq.1", "cost_snapshot_result_hash", ""), invalid, kernel.CodeSchemaInvalid, "/cost_snapshot_ref/result_hash")
	b.proposalRefused(brcSales, b.proposalRoute("rfq.1", "at", "2026-11-18T00:00:00Z"), invalid, kernel.CodeSchemaInvalid, "/at")
	line := map[string]any{"line_id": "L1", "sku_id": "ST01.red.S", "quantity": "24", "uom": "PCS", "source_id": "source.1", "locator": "第1段，第1项"}
	b.cmd(brcSales, "inq", inqapi.Contract, "rfq.save_draft", map[string]any{"id": "rfq.2", "expected_version": 0, "customer_id": "customer.1",
		"contact_id": "contact.1", "source_ids": []string{"source.1"}, "lines": []any{line}})
	b.proposalRefused(brcSales, b.proposalRoute("rfq.2"), invalid, kernel.CodeInvalidState, "/rfq_id")
	b.cmd(brcSales, "inq", inqapi.Contract, "rfq.confirm_scope", map[string]any{"id": "rfq.2", "expected_version": 1, "evidence_ids": []string{"source.1"}})
	b.proposalRefused(brcSales, b.proposalRoute("rfq.2"), invalid, kernel.CodeReferenceInvalid, "/cost_snapshot_ref")
	// A ready RFQ that nobody costed yet has its list prices.
	b.propose(b.proposalRoute("rfq.2", "cost_snapshot_id", "", "cost_snapshot_result_hash", ""), "L1 price_list customer price.red.S.c1@1 7.90/1")

	// 7. The pricing policy moves on: revision 2 raises the target margin. Revision 1 is no longer
	// in force, and the snapshot costed under it is no longer adoptable, so the formula has no cost
	// until the RFQ is costed again; the snapshot's standing is shown.
	b.phase = "TRD price proposal after the pricing policy changed"
	b.cmd(brcAdmin, "pol", polapi.Contract, "config.save_draft", map[string]any{"kind": "pricing_policy", "id": "policy.1", "expected_version": 2,
		"valid_from": kernel.FormatTimestamp(b.from), "data": map[string]any{"name": "synthetic BRC-001 policy", "calculation_currency": "USD",
			"minimum_margin": "0.25", "quote_valid_days": 7, "max_fx_age_seconds": 3600, "tax_profile": b.zero, "target_margin": "0.35"}})
	b.cmd(brcController, "pol", polapi.Contract, "config.publish", map[string]any{"kind": "pricing_policy", "id": "policy.1", "expected_version": 3,
		"revision": 2, "evidence_ids": []string{"evidence"}, "reason": "提高目标毛利率（合成）"})
	b.proposalRefused(brcSales, b.proposalRoute("rfq.1"), invalid, polapi.CodeConfigNotActive, "/pricing_policy_ref")
	p = b.propose(b.proposalRoute("rfq.1", "pricing_policy_revision", "2"), unjudged...)
	if p["target_margin"] != "0.35" || brcDig(p, "cost_snapshot", "adoptable") != false || brcDig(p, "cost_snapshot", "status") != "not_current" ||
		brcDig(p, "cost_snapshot", "snapshot_id") != b.snapshotID {
		t.Fatalf("after the policy changed: %v", p)
	}
	b.fact("cost_snapshot_status", brcDig(p, "cost_snapshot", "status"))

	// 8. Audited like TRD's other reads: every proposal as a read of its RFQ, every refusal with its code.
	b.phase = "TRD audit"
	reads, refusals := map[string]int{}, map[string]int{}
	for _, e := range brcList(b.get(brcSales, "/api/brant/v1/trd/audit")["entries"]) {
		m := brcMap(e)
		if m["command"] != trdapi.QueryPriceProposal {
			continue
		}
		actor := brcStr(brcDig(m, "actor", "id"))
		switch m["outcome"] {
		case "read":
			reads[actor+" "+brcStr(m["object_id"])]++
		case "rejected":
			refusals[actor+" "+brcStr(m["code"])]++
		default:
			t.Fatalf("audit entry %v", m)
		}
	}
	wantReads := map[string]int{brcSales + " rfq.1": 12, g4aAgent + " rfq.1": 1, brcSales + " rfq.2": 1}
	wantRefusals := map[string]int{brcSales + " NOT_FOUND": 1, g4aOutsider + " NOT_FOUND": 1, g4aNoRead + " PERMISSION_DENIED": 2,
		brcSales + " REFERENCE_INVALID": 5, brcSales + " SCHEMA_INVALID": 2, brcSales + " INVALID_STATE": 1, brcSales + " CONFIG_NOT_ACTIVE": 1}
	if fmt.Sprint(reads) != fmt.Sprint(wantReads) || fmt.Sprint(refusals) != fmt.Sprint(wantRefusals) {
		t.Fatalf("audit: reads %v, refusals %v", reads, refusals)
	}
	b.fact("reads", reads, "refusals", refusals)
}

// Without a target margin — BRC-001's policy as it was before the owner's decision — the formula
// proposes nothing and says why; a list price is proposed all the same, judged on the snapshot's cost.
func TestPriceProposalWithoutATargetMargin(t *testing.T) {
	b := newPriceProposalWorld(t, "")
	p := b.propose(b.proposalRoute("rfq.1"), "L1 none no_target_margin", "L2 none no_target_margin", "L3 none no_target_margin",
		"L4 none no_target_margin")
	if _, stated := p["target_margin"]; stated || p["minimum_margin"] != "0.25" || brcDig(p, "cost_snapshot", "adoptable") != true {
		t.Fatalf("%v", p)
	}
	b.salesPrice("price.blue.S", "ST01.blue.S", "", "USD", "8.00")
	b.propose(b.proposalRoute("rfq.1"), "L1 none no_target_margin", "L2 none no_target_margin",
		"L3 price_list general price.blue.S@1 8.00/1 net 9600 on 6600 margin 0.3125", "L4 none no_target_margin")
	// Without a snapshot the first thing the formula lacks is the cost.
	b.propose(b.proposalRoute("rfq.1", "cost_snapshot_id", "", "cost_snapshot_result_hash", ""), "L1 none no_adoptable_snapshot",
		"L2 none no_adoptable_snapshot", "L3 price_list general price.blue.S@1 8.00/1", "L4 none no_adoptable_snapshot")
}
