//go:build integration

package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S4 F2: an AI token that holds ai_q01_agent and a live delegation for its task is still refused
// on the plain command route (PERMISSION_DENIED) before the body is read. Nothing is written: no
// RFQ, no idempotency row, no outbox event. The same agent's step through the gateway in process
// still saves a draft: TestAIGatewayScriptedAgentDrafts. A person then saves the same body, which
// is the proof the refused request would have committed.
func TestAITokenWithADelegationIsRefusedOnThePlainRoute(t *testing.T) {
	w := newWorld(t)
	scope := w.scope("company.A")
	agentRef := kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.q01"}
	clerkRef := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk.f2"}
	for ref, template := range map[kernel.ActorRef]string{agentRef: "ai_q01_agent", clerkRef: "sales_clerk"} {
		if _, err := w.platform.AssignRoles(context.Background(), scope, ref, []string{template}, authorization.DataScopeCompany, ""); err != nil {
			t.Fatal(err)
		}
	}
	clerk, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: clerkRef, Scope: scope})
	w.must(err)
	clerk = action.WithCorrelation(clerk, pgtest.UniqueID(t, "corr."))
	w.source(clerk, "src.mail.1", "合成询价：120件 sku.1，请报价。")
	layer, _ := aiLayer(t, w.c)
	if _, err = layer.GrantDelegation(clerk, policy.GrantDelegation{IdempotencyKey: w.key(), DelegationID: "del.f2", AgentID: agentRef.ID,
		TaskID: "task.f2", Capabilities: []string{inqapi.CommandSaveRfq}, Autonomy: "draft", ExpiresAt: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	agent := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", agentRef.ID, "ai"), n: 0}
	human := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", clerkRef.ID, "human"), n: 100}
	args := map[string]any{"id": "rfq.plain", "expected_version": 0, "source_ids": []string{"src.mail.1"}, "unresolved_fields": []string{"customer"},
		"lines": []any{map[string]any{"line_id": "L1", "requested_item": "sku.1", "quantity": "120", "source_id": "src.mail.1", "locator": "第1行"}}}
	status, out := agent.try("inq", inqapi.Contract, inqapi.CommandSaveRfq, args)
	if status != http.StatusForbidden || out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI token with a live delegation: %d %v", status, out)
	}
	if n := w.count("company.A", "brant_inq.customer_rfq"); n != 0 {
		t.Fatalf("%d RFQs after the refusal", n)
	}
	if n := w.count("company.A", "brant_inq.action_result"); n != 0 {
		t.Fatalf("%d idempotency rows after the refusal", n)
	}
	if ev := w.events("company.A", inqapi.Source); len(ev) != 0 {
		t.Fatalf("%d INQ outbox events after the refusal", len(ev))
	}
	auditor := w.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "auditor.f2"},
		Permissions: []string{"inq.audit"}, DataScope: authorization.DataScopeCompany})
	entries, err := w.c.INQ.Audit(auditor)
	w.must(err)
	var found bool
	for _, e := range entries {
		if e.Command == inqapi.CommandSaveRfq && e.ObjectID == "" && e.Outcome == "rejected" && e.Code == kernel.CodePermissionDenied &&
			e.Actor == agentRef {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit %v", entries)
	}
	if r := human.command("inq", inqapi.Contract, inqapi.CommandSaveRfq, args); r["object_id"] != "rfq.plain" || r["status"] != "draft" {
		t.Fatalf("the same body as a person: %v", r)
	}
	if n := w.count("company.A", "brant_inq.customer_rfq"); n != 1 {
		t.Fatalf("%d RFQs after the person's save", n)
	}
}
