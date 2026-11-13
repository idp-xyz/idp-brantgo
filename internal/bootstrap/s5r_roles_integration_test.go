//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The role templates of S5R (unit R1, WBS S5R-01; simulated sign-off E0 of 2026-11-13) against
// PostgreSQL 16, with every human holding its template the way a tenant assigns it
// (bootstrap.Platform.AssignRoles): each template change a decision makes is proven by the
// command's acceptance or refusal. The template catalog itself is pinned by
// authorization.TestS5RTemplateDecisions; the consumers' grants by
// TestS5RConsumerGrantsAreTheContextMapsConsumers. Python V0.50 has no S5 contexts: the tests cite
// the decisions.

// S5R-D02 and S5R-D26 over HTTP with OIDC access tokens, on an order TRD activated
// (OrderCommercialSnapshot v1, R32) whose account the system consumer has not opened.
//   - D26: treasury, credit_manager, credit_officer, tax_specialist and compliance each register a
//     source and a file of their own; the order reviewer, whose template has no evidence
//     registration, is refused (PERMISSION_DENIED).
//   - D02: order_account.open is refused to a sales clerk, to the Q01 AI execution identity, and to
//     an AI actor holding the permissions outright (the command takes a human or the system, never
//     an AI); the treasurer re-creates the account.
//   - D26 with D-16: the treasurer records the customer's receipt on the bank advice it registered
//     itself, the credit manager resolves the 2.00 difference on the charge note it registered —
//     the approval stays independent of the fund's recorder.
//   - D32: the Q01 AI identity is refused FUL's reads (no ful.read), while the documentation clerk
//     reads through to NOT_FOUND.
func TestS5ROrderAccountAndEvidenceUnderRoleTemplates(t *testing.T) {
	q := newOrderWorld(t)
	o := q.activeOrder("quote.1")
	snap := q.snapshots(q.author, o.ObjectID)[o.ObjectID]
	usdBank := q.configure(q.author, q.reviewer, polapi.KindBankAccount, "bank.usd", 0, q.win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "USD", polapi.AccountBoth), true)
	humans := map[string]string{"treasury.1": "treasury", "manager.1": "credit_manager", "officer.1": "credit_officer",
		"tax.1": "tax_specialist", "compliance.1": "compliance", "sales.1": "sales_clerk", "reviewer.1": "order_reviewer",
		"docs.1": "documentation"}
	for id, template := range humans {
		q.assign(id, template)
	}
	agentRef := kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.q01"}
	if _, err := q.platform.AssignRoles(context.Background(), q.scope("company.A"), agentRef, []string{"ai_q01_agent"},
		authorization.DataScopeCompany, ""); err != nil {
		t.Fatal(err)
	}
	q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.accounts"},
		Permissions: []string{stlapi.CommandOpenOrderAccount, stlapi.PermissionOpenReview, stlapi.PermissionRead}, DataScope: "company"})
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	clients, n := map[string]*client{}, 100
	for _, id := range slices.Sorted(maps.Keys(humans)) {
		clients[id] = &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", id, "human"), n: n}
		n += 100
	}
	for _, id := range []string{"agent.q01", "agent.accounts"} {
		clients[id] = &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", id, "ai"), n: n}
		n += 100
	}
	refused := func(who string, status int, out map[string]any, want int, code string) {
		t.Helper()
		if status != want || out["code"] != code {
			t.Fatalf("%s: %d %v, want %d %s", who, status, out, want, code)
		}
	}
	register := func(actor, id string) {
		t.Helper()
		c := clients[actor]
		r := c.command("evd", evdapi.Contract, "source.register", map[string]any{"id": id, "source_kind": evdapi.KindManualText,
			"content_text": "合成依据（" + actor + "，S5R-D26）：" + id})
		if r["object_id"] != id {
			t.Fatalf("%s source.register %v", actor, r)
		}
		r = c.command("evd", evdapi.Contract, "file.register", map[string]any{"id": id + ".scan", "file_name": id + ".txt",
			"mime_type": evdapi.MimeText, "content_base64": base64.StdEncoding.EncodeToString([]byte("合成扫描件：" + id))})
		if r["object_id"] != id+".scan" {
			t.Fatalf("%s file.register %v", actor, r)
		}
	}

	// S5R-D26: who decides on bank advices, bills, invoices, tax papers and compliance evidence
	// registers them.
	register("treasury.1", "advice.p1")
	register("manager.1", "charge.note")
	register("officer.1", "credit.report")
	register("tax.1", "tax.notice")
	register("compliance.1", "licence.copy")
	status, out := clients["reviewer.1"].try("evd", evdapi.Contract, "source.register", map[string]any{"id": "review.note",
		"source_kind": evdapi.KindManualText, "content_text": "合成"})
	refused("the order reviewer registering a source", status, out, http.StatusForbidden, kernel.CodePermissionDenied)

	// S5R-D02: order_account.open is treasury's and the system consumer's.
	open := map[string]any{"order_id": o.ObjectID, "order_revision": snap.Binding.OrderRevision, "order_hash": snap.Binding.OrderHash}
	for _, who := range []string{"sales.1", "agent.q01", "agent.accounts"} {
		status, out = clients[who].try("stl", stlapi.Contract, stlapi.CommandOpenOrderAccount, open)
		refused(who+" opening the order account", status, out, http.StatusForbidden, kernel.CodePermissionDenied)
	}
	r := clients["treasury.1"].command("stl", stlapi.Contract, stlapi.CommandOpenOrderAccount, open)
	if r["object_id"] != o.ObjectID || r["status"] != stlapi.AccountOpen {
		t.Fatalf("the treasurer opening the account: %v", r)
	}
	status, acc := clients["treasury.1"].do("GET", "/api/brant/v1/stl/order_accounts/"+o.ObjectID, nil)
	if status != http.StatusOK || !strings.Contains(fmt.Sprint(acc["opened_by"]), "treasury.1") {
		t.Fatalf("order account %d %v", status, acc)
	}

	// S5R-D26 with D-16: each decides on the evidence it registered.
	ref := func(r polapi.ConfigRef) map[string]any {
		return map[string]any{"kind": r.Kind, "id": r.ConfigID, "revision": r.Revision}
	}
	r = clients["treasury.1"].command("stl", stlapi.Contract, stlapi.CommandRecordFund, map[string]any{"fund_record_id": "fund.r1",
		"kind": stlapi.FundReceipt, "counterparty_id": "customer.1", "currency": ref(q.usd), "bank_account": ref(usdBank),
		"received_amount": "958.00", "remitted_amount": "960.00", "value_date": kernel.FormatTimestamp(time.Now()),
		"bank_reference": "MT103-R1", "evidence_ids": []any{"advice.p1"}})
	if r["status"] != stlapi.FundRecorded {
		t.Fatalf("fund.record on the treasurer's own advice: %v", r)
	}
	r = clients["manager.1"].command("stl", stlapi.Contract, stlapi.CommandResolveDifference, map[string]any{"fund_record_id": "fund.r1",
		"expected_version": 1, "resolution": "bank_charge", "amount": "2.00", "evidence_ids": []any{"charge.note"},
		"reason": "中间行扣费，经批准由我方承担（合成）"})
	if r["status"] != stlapi.FundDifferenceResolved {
		t.Fatalf("fund.resolve_difference on the credit manager's own note: %v", r)
	}

	// S5R-D32: no FUL read for the Q01 AI identity.
	status, out = clients["agent.q01"].do("GET", "/api/brant/v1/ful/shipments/shipment.none", nil)
	refused("the Q01 AI identity reading FUL", status, out, http.StatusForbidden, kernel.CodePermissionDenied)
	status, out = clients["docs.1"].do("GET", "/api/brant/v1/ful/shipments/shipment.none", nil)
	refused("the documentation clerk reading FUL", status, out, http.StatusNotFound, kernel.CodeNotFound)
}

// S5R-D25 (S5-D34's rule for the purchaser, extended to documentation): a documentation clerk who
// holds the documentation template alone, assigned as a tenant assigns it, prepares the commercial
// invoice and the packing list of a shipment, and POL's NumberIssue v1 numbers them as that clerk
// (CI-…, PL-…). The same job without number.issue — S5's template — is refused PERMISSION_DENIED and
// no number is issued: the number is an internal effect of the formalizing command, and its
// operator holds both.
func TestS5RDocumentationTemplateNumbersItsDocuments(t *testing.T) {
	w := newFulDocWorld(t)
	w.shipment("shipment.1", "commitment.1", "50")
	issued := func() int {
		n := 0
		for _, e := range w.events("company.A", polapi.Source) {
			if e.Type == polapi.EventNumberIssued {
				n++
			}
		}
		return n
	}
	before := issued()
	tpl, ok := authorization.TemplateByCode("documentation")
	if !ok {
		t.Fatal("no documentation role template")
	}
	s5 := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "docs.s5"), DataScope: "company",
		Permissions: without(tpl.Permissions, "number.issue")})
	_, err := w.ful.PrepareShipmentDocument(s5, w.ciCmd("docs.r1", "shipment.1", 0))
	if code(err) != kernel.CodePermissionDenied || issued() != before {
		t.Fatalf("documentation without number.issue: %v, %d numbers issued", err, issued()-before)
	}
	clerk := w.assign("docs.r1", "documentation")
	ci, err := w.ful.PrepareShipmentDocument(clerk, w.ciCmd("docs.r1", "shipment.1", 0))
	w.must(err)
	pl, err := w.ful.PrepareShipmentDocument(clerk, w.plCmd("docs.r1", "shipment.1", ci.RowVersion))
	w.must(err)
	if !strings.HasPrefix(ci.Number, "CI-") || !strings.HasPrefix(pl.Number, "PL-") || issued() != before+2 {
		t.Fatalf("numbers %q %q, %d issued", ci.Number, pl.Number, issued()-before)
	}
	set := w.set("docs.r1")
	if len(set.Documents) != 2 {
		t.Fatalf("document set %+v", set)
	}
	for _, d := range set.Documents {
		if d.PreparedBy != (kernel.ActorRef{Kind: kernel.ActorHuman, ID: "docs.r1"}) || (d.DocumentKind != fulapi.DocumentCommercialInvoice &&
			d.DocumentKind != fulapi.DocumentPackingList) {
			t.Errorf("%s prepared by %s", d.DocumentKind, d.PreparedBy)
		}
	}
}
