//go:build integration

package bootstrap_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai"
	"go.idp.xyz/idp-brant/internal/ai/approvalgrant"
	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/ai/gateway"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/aigrant"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S4 unit F1 (decision D-7, AI-INV-002): an AI actor issues a command that needs an approval grant
// — order.prepare and order.review.request, the D-7 list — only with the AI layer's proof that a
// grant for exactly that call was used (gateway Admit). The owning context's ledger checks the
// proof, so the refusal holds whatever the transport; people are unaffected.

// The reproduction of C3's gap over HTTP with OIDC access tokens: the Q01 AI execution identity
// (role template ai_q01_agent, which holds order.prepare and order.review.request) calls both
// commands on the plain command route POST /api/brant/v1/trd/commands/... on a root no task binds,
// without a grant. Both are PERMISSION_DENIED and audited in TRD; nothing is written. The agent
// fares no better through TRD's Go api, nor does an AI actor holding the permissions outright. The
// sales clerk then prepares the order and requests its review over the same routes.
func TestAIAgentCannotSkipTheApprovalGrantOnThePlainRoutes(t *testing.T) {
	q := newOrderWorld(t)
	_, acc := q.accepted("quote.1")
	_, draft := q.prepared("quote.2")
	scope := q.scope("company.A")
	agentRef := kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.q01"}
	clerkRef := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk.f1"}
	for ref, template := range map[kernel.ActorRef]string{agentRef: "ai_q01_agent", clerkRef: "sales_clerk"} {
		if _, err := q.platform.AssignRoles(context.Background(), scope, ref, []string{template}, authorization.DataScopeCompany, ""); err != nil {
			t.Fatal(err)
		}
	}
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	agent := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", agentRef.ID, "ai"), n: 100}
	clerk := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", clerkRef.ID, "human"), n: 200}

	prepare := map[string]any{"order_id": "order.f1", "quote_id": acc.QuoteID, "business_revision": acc.BusinessRevision,
		"business_hash": acc.BusinessHash, "acceptance_id": acc.ObjectID}
	request := map[string]any{"order_id": draft.ObjectID, "expected_version": 1, "review_id": "review.f1"}
	for _, c := range []struct {
		command, object string
		args            map[string]any
	}{
		{trdapi.CommandPrepareOrder, "order.f1", prepare},
		{trdapi.CommandRequestOrderReview, draft.ObjectID, request},
	} {
		status, out := agent.try("trd", trdapi.Contract, c.command, c.args)
		t.Logf("ai_q01_agent %s on the plain route without a grant: %d %v", c.command, status, out)
		if status != http.StatusForbidden || out["code"] != kernel.CodePermissionDenied {
			t.Errorf("%s: %d %v, want 403 PERMISSION_DENIED", c.command, status, out)
		}
		if !q.audited(c.command, c.object, "rejected", kernel.CodePermissionDenied) {
			t.Errorf("%s: TRD did not audit the refusal", c.command)
		}
	}
	if _, err = q.c.TRD.GetOrder(q.author, "order.f1"); code(err) != kernel.CodeNotFound {
		t.Errorf("the agent's order exists: %v", err)
	}
	if v := q.getOrder(draft.ObjectID); v.RowVersion != 1 || len(v.Reviews) != 0 {
		t.Errorf("the draft order changed: version %d, reviews %+v", v.RowVersion, v.Reviews)
	}

	// Whatever the transport: TRD's Go api as the template's agent and as an AI actor holding the
	// permissions outright.
	agentCtx, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: agentRef, Scope: scope})
	q.must(err)
	outright := q.actorOf(kernel.ActorAI, "agent.outright")
	for name, ctx := range map[string]context.Context{"ai_q01_agent": agentCtx, "AI holding the permissions": outright} {
		_, err = q.c.TRD.PrepareOrder(ctx, q.prepareCmd("order.f1", acc))
		if code(err) != kernel.CodePermissionDenied {
			t.Errorf("%s: order.prepare through the api: %v", name, err)
		}
		_, err = q.c.TRD.RequestOrderReview(ctx, q.requestCmd(draft.ObjectID, 1, "review.f1"))
		if code(err) != kernel.CodePermissionDenied {
			t.Errorf("%s: order.review.request through the api: %v", name, err)
		}
	}

	// People are unaffected: the sales clerk prepares the order and requests its review.
	o := clerk.command("trd", trdapi.Contract, trdapi.CommandPrepareOrder, prepare)
	if o["object_id"] != "order.f1" || o["status"] != trdapi.OrderDraft {
		t.Fatalf("the clerk's order.prepare: %v", o)
	}
	r := clerk.command("trd", trdapi.Contract, trdapi.CommandRequestOrderReview, request)
	if r["object_id"] != draft.ObjectID || r["row_version"] != float64(2) {
		t.Fatalf("the clerk's order.review.request: %v", r)
	}
	if v := q.getOrder("order.f1"); v.PreparedBy != clerkRef {
		t.Fatalf("prepared by %v", v.PreparedBy)
	}
}

// The gateway path at the target (D-7; AI-INV-002 positive and negative): the agent's order.prepare
// and order.review.request run at TRD when the gateway used a grant for exactly the call. TRD itself
// refuses, GRANT_INVALID and audited, an admitted call changed after its admission (another order,
// so another request hash; another key; another expected version of the order, which the request
// hash leaves out) and the agent's proof carried by another AI actor. A resend with the same key is
// the same use and TRD replays; a grant for another request hash is refused by the gateway; the
// plain api with the same key is PERMISSION_DENIED, the stored result being no exception.
func TestTRDAcceptsAnAgentsCallOnlyWithTheProofOfItsGrant(t *testing.T) {
	q := newOrderWorld(t)
	_, acc := q.accepted("quote.1")
	scope := q.scope("company.A")
	agentRef := kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.q01"}
	agent := q.provision(grantOf(q, agentRef.ID, kernel.ActorAI, "company", templatePermissions(t, "ai_q01_agent")...))
	other := q.provision(grantOf(q, "agent.other", kernel.ActorAI, "company", templatePermissions(t, "ai_q01_agent")...))
	g := newGrantedAI(q)

	prepare := q.prepareCmd("order.ai.1", acc)
	a := g.admit(agent, prepare)
	changed := a
	cmd := changed.Command.(trdapi.PrepareOrder)
	cmd.OrderID = "order.ai.other"
	changed.Command = cmd
	_, err := changed.Run(agent)
	q.want(err, codes.GrantInvalid)
	rekeyed := a
	rekeyed.Key = q.key()
	_, err = rekeyed.Run(agent)
	q.want(err, codes.GrantInvalid)
	proof := aigrant.Proof{GrantID: a.Grant.GrantID, Scope: scope, Agent: agentRef.ID, Command: a.Grant.Command, RequestHash: a.Grant.RequestHash,
		Object: aigrant.Object{Type: a.Grant.ObjectType, ID: a.Grant.ObjectID, Version: a.Grant.ObjectVersion}, Key: a.Key}
	_, err = q.c.TRD.PrepareOrder(aigrant.With(other, proof), prepare)
	q.want(err, codes.GrantInvalid)
	if _, err = q.c.TRD.GetOrder(q.author, "order.ai.1"); code(err) != kernel.CodeNotFound {
		t.Fatalf("a refused call prepared the order: %v", err)
	}

	res, err := a.Run(agent)
	q.must(err)
	o := res.Detail.(trdapi.OrderReceipt)
	if o.ObjectID != "order.ai.1" || o.Status != trdapi.OrderDraft || o.Replayed {
		t.Fatalf("prepared %+v", o)
	}
	again, err := g.prepare(agent, prepare)
	if err != nil || !again.Replayed || again.ActionID != o.ActionID {
		t.Fatalf("the same key through the gateway %+v, %v", again, err)
	}
	_, err = q.c.TRD.PrepareOrder(agent, prepare)
	q.want(err, kernel.CodePermissionDenied)
	// A grant for another request hash: the gateway refuses to use it.
	second := q.prepareCmd("order.ai.2", acc)
	c, b, key := g.decode(agent, second)
	b.GrantID = g.grants[prepare.IdempotencyKey]
	_, err = g.layer.Gateway.Admit(agent, b, c, key)
	q.want(err, codes.GrantInvalid)

	// order.review.request binds the order at the version the call expects.
	request := q.requestCmd("order.ai.1", 1, "review.ai")
	ar := g.admit(agent, request)
	moved := ar
	rc := moved.Command.(trdapi.RequestOrderReview)
	rc.ExpectedVersion = 2
	moved.Command = rc
	_, err = moved.Run(agent)
	q.want(err, codes.GrantInvalid)
	res, err = ar.Run(agent)
	q.must(err)
	if r := res.Detail.(trdapi.OrderReceipt); r.Status != trdapi.OrderReviewOpen || r.RowVersion != 2 {
		t.Fatalf("requested %+v", r)
	}
	v := q.getOrder("order.ai.1")
	if v.PreparedBy != agentRef || len(v.Reviews) != 1 || v.Reviews[0].RequestedBy != agentRef {
		t.Fatalf("the order %+v", v)
	}
	for _, e := range []struct{ command, object, code string }{
		{trdapi.CommandPrepareOrder, "order.ai.other", codes.GrantInvalid},
		{trdapi.CommandPrepareOrder, "order.ai.1", codes.GrantInvalid},
		{trdapi.CommandPrepareOrder, "order.ai.1", kernel.CodePermissionDenied},
		{trdapi.CommandRequestOrderReview, "order.ai.1", codes.GrantInvalid},
	} {
		if !q.audited(e.command, e.object, "rejected", e.code) {
			t.Errorf("TRD did not audit %s %s %s", e.command, e.object, e.code)
		}
	}
}

// grantedAI runs an AI agent's calls of the commands that need an approval grant (D-7:
// order.prepare, order.review.request) the way the AI layer runs them, for tests about what the
// target does with such a call: a sales clerk delegates both commands to the agent for a task and
// approves exactly the call the gateway decoded (ai.approval_grant.issue); the gateway uses the
// grant with the call's key (Admit) and runs the call as the agent with the proof TRD checks. A call
// sent again with the same key reuses its grant: the same use.
type grantedAI struct {
	q      *trdWorld
	layer  *ai.Layer
	clerk  context.Context
	grants map[string]string // idempotency key → grant ID
}

func newGrantedAI(q *trdWorld) *grantedAI {
	q.t.Helper()
	clerk := q.provision(grantOf(q, "clerk.grants", kernel.ActorHuman, "company", templatePermissions(q.t, "sales_clerk")...))
	layer, _ := aiLayer(q.t, q.c)
	return &grantedAI{q: q, layer: layer, clerk: clerk, grants: map[string]string{}}
}

// decode delegates both commands to the agent of ctx on its first call and decodes cmd, an api
// command with its idempotency key, as the agent's tool call; it returns the key apart.
func (g *grantedAI) decode(agent context.Context, cmd any) (gateway.Call, gateway.Binding, string) {
	g.q.t.Helper()
	p, err := identity.FromContext(agent)
	g.q.must(err)
	b := gateway.Binding{TaskID: "task." + p.Actor.ID, DelegationID: "del." + p.Actor.ID}
	if _, err = g.layer.GetDelegation(g.clerk, b.DelegationID); code(err) == kernel.CodeNotFound {
		_, err = g.layer.GrantDelegation(g.clerk, policy.GrantDelegation{IdempotencyKey: g.q.key(), DelegationID: b.DelegationID,
			AgentID: p.Actor.ID, TaskID: b.TaskID, Capabilities: []string{trdapi.CommandPrepareOrder, trdapi.CommandRequestOrderReview},
			Autonomy: "submit", ExpiresAt: time.Now().Add(24 * time.Hour)})
	}
	g.q.must(err)
	var name, key string
	switch c := cmd.(type) {
	case trdapi.PrepareOrder:
		name, key = trdapi.CommandPrepareOrder, c.IdempotencyKey
	case trdapi.RequestOrderReview:
		name, key = trdapi.CommandRequestOrderReview, c.IdempotencyKey
	default:
		g.q.t.Fatalf("%T needs no approval grant", cmd)
	}
	raw, err := json.Marshal(cmd)
	g.q.must(err)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var args map[string]any
	g.q.must(dec.Decode(&args))
	delete(args, "IdempotencyKey") // the platform's, never the model's
	c, err := g.layer.Gateway.Decode(agent, b, scriptedCall("call."+key, name, args))
	g.q.must(err)
	return c, b, key
}

// admit decodes the agent's call cmd, has the clerk approve exactly it (once per key) and admits it
// with the call's key.
func (g *grantedAI) admit(agent context.Context, cmd any) gateway.Admitted {
	g.q.t.Helper()
	c, b, key := g.decode(agent, cmd)
	grant, ok := g.grants[key]
	if !ok {
		grant = "grant." + key
		_, err := g.layer.IssueApprovalGrant(g.clerk, approvalgrant.IssueGrant{IdempotencyKey: g.q.key(), GrantID: grant,
			DelegationID: b.DelegationID, Command: c.Capability.Command, RequestHash: c.RequestHash, ObjectID: c.Object.ID,
			ObjectVersion: c.Object.Version, ExpiresAt: time.Now().Add(time.Hour)})
		g.q.must(err)
		g.grants[key] = grant
	}
	b.GrantID = grant
	a, err := g.layer.Gateway.Admit(agent, b, c, key)
	g.q.must(err)
	return a
}

// prepare runs the agent's order.prepare through the gateway with a grant.
func (g *grantedAI) prepare(agent context.Context, cmd trdapi.PrepareOrder) (trdapi.OrderReceipt, error) {
	res, err := g.admit(agent, cmd).Run(agent)
	if err != nil {
		return trdapi.OrderReceipt{}, err
	}
	return res.Detail.(trdapi.OrderReceipt), nil
}

// requestReview runs the agent's order.review.request through the gateway with a grant.
func (g *grantedAI) requestReview(agent context.Context, cmd trdapi.RequestOrderReview) (trdapi.OrderReceipt, error) {
	res, err := g.admit(agent, cmd).Run(agent)
	if err != nil {
		return trdapi.OrderReceipt{}, err
	}
	return res.Detail.(trdapi.OrderReceipt), nil
}
