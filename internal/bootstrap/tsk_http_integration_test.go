//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai"
	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TSK, the AI layer and the task-step route over HTTP with OIDC access tokens (WBS S4-01f; S4 unit
// C3; owner decision D-2; SM-TSK-TASK; TSK-INV-002, TSK-INV-003, TSK-INV-004; TRD-INV-015; AI-INV-002,
// AI-INV-004; X-INV-002). The people act under their role templates: the sales clerk (sales_clerk)
// owns and steers the task, the compliance officer (compliance) reads the audit trails; the task's
// agent holds ai_q01_agent; TSK's system actor runs the system's transitions.

// httpWorld is the agent world behind the composed HTTP API.
type httpWorld struct {
	*agentWorld
	clerk, agent, sys, auditor *client
}

func newHTTPTaskWorld(t *testing.T) *httpWorld {
	w := newAgentWorld(t, false)
	w.provision(grantOf(w.trdWorld, "clerk.http", kernel.ActorHuman, "company", templatePermissions(t, "sales_clerk")...))
	w.provision(grantOf(w.trdWorld, "auditor.http", kernel.ActorHuman, "company", templatePermissions(t, "compliance")...))
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	as := func(id, kind string) *client {
		return &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", id, kind)}
	}
	return &httpWorld{agentWorld: w, clerk: as("clerk.http", "human"), agent: as(agentID, "ai"), sys: as("tsk", "system"),
		auditor: as("auditor.http", "human")}
}

// send posts command of bc with a key of its own.
func (w *httpWorld) send(c *client, bc, contract, command string, args map[string]any) (int, map[string]any) {
	w.t.Helper()
	return c.do("POST", "/api/brant/v1/"+bc+"/commands/"+command, map[string]any{"contract": contract, "command": command,
		"idempotency_key": w.key(), "args": args})
}

// task sends a TSK command that must succeed and returns the task's version after it.
func (w *httpWorld) task(c *client, command string, args map[string]any) int64 {
	w.t.Helper()
	status, out := w.send(c, "tsk", tskapi.Contract, command, args)
	if status != http.StatusOK || out["ok"] != true {
		w.t.Fatalf("%s: %d %v", command, status, out)
	}
	return int64(out["row_version"].(float64))
}

// step sends a person's step on task through the task-step route.
func (w *httpWorld) step(c *client, task, bc, contract, command, intent string, args map[string]any) (int, map[string]any) {
	w.t.Helper()
	return c.do("POST", "/api/brant/v1/tsk/tasks/"+task+"/steps/"+bc+"/"+command, map[string]any{"contract": contract, "command": command,
		"business_intent_id": intent, "args": args})
}

func (w *httpWorld) get(c *client, path string) map[string]any {
	w.t.Helper()
	status, out := c.do("GET", path, nil)
	if status != http.StatusOK || out["ok"] != true {
		w.t.Fatalf("GET %s: %d %v", path, status, out)
	}
	return out
}

func refused(t *testing.T, what string, status int, out map[string]any, code string) {
	t.Helper()
	if out["code"] != code || status < 400 {
		t.Fatalf("%s: %d %v, want %s", what, status, out, code)
	}
}

// A person's AI task over HTTP from creation to release. The clerk creates it, TSK's system actor and
// the clerk scope it, the system binds its quote root; the agent may issue no task command. A step
// sent on the plain command route meets TRD's fence (OBJECT_CONTROL_CONFLICT); through the task-step
// route it carries the task's claim, is recorded before it is sent and VERIFIED from TRD's ledger — a
// refused attempt is recorded with TRD's code and the corrected one is the same intent's next attempt
// with the same key; the intent sent again answers its result without a send (CT11). The agent cannot
// take a step there, nor can anyone send an object control command. The system verifies the goal
// (the quote is frozen, not active: waiting_input, the verification readable); the clerk takes the
// task over (TRD stops the root first), the system completes the barrier (manual at TRD's epoch 3),
// the clerk's next step carries the manual claim; the clerk closes the task and releases the root.
// The release's own reconcile route answers committed; the audit trail shows the commands.
func TestTaskLifecycleOverHTTP(t *testing.T) {
	w := newHTTPTaskWorld(t)
	w.drafted("quote.h")
	root := w.getQuote("quote.h").RootVersion

	create := map[string]any{"task_id": "task.h", "goal_code": tskapi.GoalQuoteReady, "control_mode": tskapi.ModeAI, "agent_id": agentID,
		"target": map[string]any{"quote_id": "quote.h", "customer_id": "customer.1"}}
	status, out := w.send(w.agent, "tsk", tskapi.Contract, tskapi.CommandCreate, create)
	refused(t, "the agent creates a task", status, out, kernel.CodePermissionDenied)
	v := w.task(w.clerk, tskapi.CommandCreate, create)
	v = w.task(w.sys, tskapi.CommandScope, map[string]any{"task_id": "task.h", "expected_version": v})
	status, out = w.send(w.agent, "tsk", tskapi.Contract, tskapi.CommandScope, map[string]any{"task_id": "task.h", "expected_version": v})
	refused(t, "the agent scopes", status, out, kernel.CodePermissionDenied)
	v = w.task(w.clerk, tskapi.CommandScope, map[string]any{"task_id": "task.h", "expected_version": v, "note": "范围确认"})
	status, out = w.send(w.clerk, "tsk", tskapi.Contract, tskapi.CommandBindObject, map[string]any{"task_id": "task.h", "expected_version": v,
		"quote_id": "quote.h"})
	refused(t, "a person binds", status, out, kernel.CodePermissionDenied)
	v = w.task(w.sys, tskapi.CommandBindObject, map[string]any{"task_id": "task.h", "expected_version": v, "quote_id": "quote.h"})

	freeze := func(expected int64) map[string]any {
		return map[string]any{"quote_id": "quote.h", "expected_version": expected, "working_revision": 1}
	}
	status, out = w.send(w.clerk, "trd", trdapi.Contract, trdapi.CommandFreeze, freeze(root))
	refused(t, "the plain route on the bound root", status, out, trdapi.CodeObjectControlConflict)
	status, out = w.step(w.clerk, "task.h", "trd", trdapi.Contract, trdapi.CommandFreeze, "human.freeze.h", freeze(root+5))
	refused(t, "a stale version", status, out, kernel.CodeVersionConflict)
	status, out = w.step(w.clerk, "task.h", "trd", trdapi.Contract, trdapi.CommandFreeze, "human.freeze.h", freeze(root))
	result, _ := out["result"].(map[string]any)
	stored, _ := out["stored"].(map[string]any)
	if status != http.StatusOK || out["stage"] != "VERIFIED" || out["attempt"] != float64(2) || out["control_epoch"] != float64(1) ||
		out["action_id"] != tskapi.ActionIDOf("human.freeze.h") || result["status"] != trdapi.StatusFrozen || stored["action_id"] != result["action_id"] ||
		out["target"].(map[string]any)["command"] != trdapi.CommandFreeze || out["replayed"] != false {
		t.Fatalf("the step: %d %v", status, out)
	}
	status, again := w.step(w.clerk, "task.h", "trd", trdapi.Contract, trdapi.CommandFreeze, "human.freeze.h", freeze(root))
	if status != http.StatusOK || again["stage"] != "VERIFIED" || again["replayed"] != true || again["result"] != nil ||
		again["stored"].(map[string]any)["action_id"] != stored["action_id"] {
		t.Fatalf("the intent again: %d %v", status, again)
	}
	if q := w.getQuote("quote.h"); q.RootVersion != root+1 {
		t.Fatalf("one freeze: root %d", q.RootVersion)
	}
	status, out = w.step(w.agent, "task.h", "trd", trdapi.Contract, trdapi.CommandUpdateMetadata, "agent.label",
		map[string]any{"quote_id": "quote.h", "expected_version": root + 1, "labels": []string{"ai"}})
	refused(t, "the agent's step", status, out, kernel.CodePermissionDenied)
	status, out = w.step(w.clerk, "task.h", "trd", trdapi.Contract, trdapi.CommandReleaseControl, "release.root",
		map[string]any{"quote_id": "quote.h", "expected_control_epoch": 1, "reason": "绕过任务"})
	refused(t, "object control as a step", status, out, kernel.CodePermissionDenied)

	actions, _ := w.get(w.clerk, "/api/brant/v1/tsk/tasks/task.h/actions")["actions"].([]any)
	a := actions[0].(map[string]any)
	log, _ := a["log"].([]any)
	if len(actions) != 1 || a["state"] != tskapi.ActionCommitted || a["business_intent_id"] != "human.freeze.h" || len(log) < 4 {
		t.Fatalf("actions %v", actions)
	}
	if one := w.get(w.clerk, "/api/brant/v1/tsk/actions/"+a["action_id"].(string)); one["attempts"] != float64(2) {
		t.Fatalf("action %v", one)
	}

	v = w.task(w.sys, tskapi.CommandVerify, map[string]any{"task_id": "task.h", "expected_version": v})
	v = w.task(w.sys, tskapi.CommandVerifyGoal, map[string]any{"task_id": "task.h", "expected_version": v})
	vs, _ := w.get(w.clerk, "/api/brant/v1/tsk/tasks/task.h/verifications")["verifications"].([]any)
	if len(vs) != 1 || vs[0].(map[string]any)["outcome"] != tskapi.AnswerUnsatisfied || vs[0].(map[string]any)["lifecycle"] != tskapi.StateWaitingInput {
		t.Fatalf("verifications %v", vs)
	}

	v = w.task(w.clerk, tskapi.CommandTakeControl, map[string]any{"task_id": "task.h", "expected_version": v, "reason": "人工接管"})
	if q := w.getQuote("quote.h"); q.Control.State != trdapi.ControlStopping || q.Control.ControlEpoch != 2 {
		t.Fatalf("TRD stops first: %+v", q.Control)
	}
	status, out = w.send(w.clerk, "tsk", tskapi.Contract, tskapi.CommandFinishStop, map[string]any{"task_id": "task.h", "expected_version": v})
	refused(t, "a person completes the barrier", status, out, kernel.CodePermissionDenied)
	v = w.task(w.sys, tskapi.CommandFinishStop, map[string]any{"task_id": "task.h", "expected_version": v})
	tv := w.get(w.clerk, "/api/brant/v1/tsk/tasks/task.h")
	if tv["lifecycle"] != tskapi.StatePaused || tv["control_mode"] != tskapi.ModeManual || tv["control_epoch"] != float64(3) ||
		tv["binding"].(map[string]any)["quote_id"] != "quote.h" {
		t.Fatalf("after the barrier %v", tv)
	}
	status, out = w.step(w.clerk, "task.h", "trd", trdapi.Contract, trdapi.CommandUpdateMetadata, "human.label.h",
		map[string]any{"quote_id": "quote.h", "expected_version": root + 1, "labels": []string{"人工"}})
	if status != http.StatusOK || out["stage"] != "VERIFIED" || out["control_epoch"] != float64(3) {
		t.Fatalf("the manual step: %d %v", status, out)
	}

	// Q01 V0.1 tests/test_execution.py::test_human_confirmation_identity_cannot_be_forged, the HTTP half
	// (A5 did the gateway's): the verifier of a customer's acceptance is the token's person. A request
	// naming one is SCHEMA_INVALID (a closed contract), and the agent's verification is
	// PERMISSION_DENIED (human only), on the plain route and as a task step alike.
	verify := map[string]any{"response_id": "response.none", "acceptance_id": "acceptance.none", "business_revision": 1,
		"business_hash": strings.Repeat("a", 64), "customer_ref": map[string]any{"party_id": "customer.1", "revision": 2},
		"contact_ref": map[string]any{"contact_id": "contact.1", "row_version": 2}, "line_ids": []string{"L1"}}
	forged := map[string]any{"verified_by": "clerk.http"}
	for k, x := range verify {
		forged[k] = x
	}
	status, out = w.send(w.agent, "trd", trdapi.Contract, trdapi.CommandVerifyResponse, verify)
	refused(t, "the agent verifies an acceptance", status, out, kernel.CodePermissionDenied)
	status, out = w.step(w.agent, "task.h", "trd", trdapi.Contract, trdapi.CommandVerifyResponse, "agent.verify", verify)
	refused(t, "the agent verifies as a step", status, out, kernel.CodePermissionDenied)
	status, out = w.send(w.clerk, "trd", trdapi.Contract, trdapi.CommandVerifyResponse, forged)
	refused(t, "a verifier in the request", status, out, kernel.CodeSchemaInvalid)
	status, out = w.step(w.clerk, "task.h", "trd", trdapi.Contract, trdapi.CommandVerifyResponse, "human.verify", forged)
	refused(t, "a verifier in the step", status, out, kernel.CodeSchemaInvalid)

	v = w.task(w.clerk, tskapi.CommandCloseUnfulfilled, map[string]any{"task_id": "task.h", "expected_version": v,
		"cause": tskapi.CloseNotPursued, "reason": "客户不再跟进"})
	release := map[string]any{"contract": tskapi.Contract, "command": tskapi.CommandRelease, "idempotency_key": "release.h",
		"args": map[string]any{"task_id": "task.h", "expected_version": v, "reason": "交还报价根"}}
	status, out = w.clerk.do("POST", "/api/brant/v1/tsk/commands/task.release", release)
	if status != http.StatusOK || out["status"] != tskapi.StateClosedUnfulfilled {
		t.Fatalf("release: %d %v", status, out)
	}
	if q := w.getQuote("quote.h"); q.Control.State != trdapi.ControlUnbound || q.Control.ControlEpoch != 4 {
		t.Fatalf("released: %+v", q.Control)
	}
	status, rec := w.clerk.do("POST", "/api/brant/v1/tsk/commands/task.release/reconcile", release)
	if status != http.StatusOK || rec["state"] != string(kernel.ReconcileCommitted) || rec["action_id"] != out["action_id"] {
		t.Fatalf("reconcile: %d %v", status, rec)
	}
	entries, _ := w.get(w.auditor, "/api/brant/v1/tsk/audit")["entries"].([]any)
	var seen []string
	for _, e := range entries {
		m := e.(map[string]any)
		seen = append(seen, m["command"].(string)+" "+m["outcome"].(string))
	}
	for _, want := range []string{"task.create rejected", "task.create succeeded", "task.take_control succeeded", "task.release succeeded",
		"action.record_attempt succeeded"} {
		if !slices.Contains(seen, want) {
			t.Errorf("TSK audit lacks %q: %v", want, seen)
		}
	}
	if status, out = w.clerk.do("GET", "/api/brant/v1/tsk/audit", nil); status != http.StatusForbidden {
		t.Fatalf("the clerk holds no tsk.audit: %d %v", status, out)
	}
}

// The AI layer over HTTP (Context Map platformCommands AI; D-7; AI-INV-002; X-INV-002): the clerk
// delegates to the agent and issues, reads and revokes an approval grant; every one of the four
// commands is human-only — the agent's are PERMISSION_DENIED and audited. After one turn of the agent
// runner under the scripted model, the task's model and tool calls (model ID as returned, tokens, no
// content) and the ExecutionBundle the task runs with are readable; the compliance officer reads the
// AI layer's audit trail.
func TestAIRoutesOverHTTP(t *testing.T) {
	w := newHTTPTaskWorld(t)
	src := evdapi.RegisterSource{SourceID: "src.http", SourceKind: evdapi.KindManualText, ContentText: "合成询价：HTTP"}
	m := model.NewScripted("scripted-q01-v1", calls(tc("c1", evdapi.CommandRegisterSource, src)), done("x"))
	r := w.runner(m)
	r.MaxSteps = 1
	w.aiTask("task.ai", tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1"}, nil, bundleRef(r), evdapi.CommandRegisterSource)
	if res := w.turnOn(r, "task.ai"); len(res.Receipts) != 1 {
		t.Fatalf("turn %+v", res)
	}

	expires := kernel.FormatTimestamp(time.Now().Add(24 * time.Hour))
	grant := map[string]any{"id": "del.http", "agent_id": agentID, "task_id": "task.ai", "capabilities": []string{evdapi.CommandRegisterSource},
		"autonomy": "draft", "expires_at": expires}
	status, out := w.send(w.agent, "ai", ai.Contract, "ai.delegation.grant", grant)
	refused(t, "the agent delegates", status, out, kernel.CodePermissionDenied)
	status, out = w.send(w.clerk, "ai", ai.Contract, "ai.delegation.grant", grant)
	if status != http.StatusOK || out["status"] != "active" || out["object_id"] != "del.http" {
		t.Fatalf("grant: %d %v", status, out)
	}
	d := w.get(w.clerk, "/api/brant/v1/ai/delegations/del.http")
	if d["agent_id"] != agentID || d["initiator_id"] != "clerk.http" || d["autonomy"] != "draft" || d["expires_at"] != expires {
		t.Fatalf("delegation %v", d)
	}
	status, out = w.send(w.clerk, "ai", ai.Contract, "ai.delegation.grant", map[string]any{"id": "del.order", "agent_id": agentID,
		"task_id": "task.ai", "capabilities": []string{trdapi.CommandPrepareOrder}, "autonomy": "submit", "expires_at": expires})
	if status != http.StatusOK {
		t.Fatalf("grant order.prepare: %d %v", status, out)
	}
	issue := map[string]any{"id": "grant.http", "delegation_id": "del.order", "command": trdapi.CommandPrepareOrder,
		"request_hash": strings.Repeat("a", 64), "object_id": "quote.x", "object_version": 3, "expires_at": expires}
	status, out = w.send(w.agent, "ai", ai.Contract, "ai.approval_grant.issue", issue)
	refused(t, "the agent issues a grant", status, out, kernel.CodePermissionDenied)
	status, out = w.send(w.clerk, "ai", ai.Contract, "ai.approval_grant.issue", issue)
	if status != http.StatusOK || out["status"] != "issued" {
		t.Fatalf("issue: %d %v", status, out)
	}
	g := w.get(w.clerk, "/api/brant/v1/ai/approval_grants/grant.http")
	if g["command"] != trdapi.CommandPrepareOrder || g["object_version"] != float64(3) || g["status"] != "issued" || g["issuer_id"] != "clerk.http" {
		t.Fatalf("grant %v", g)
	}
	if gs, _ := w.get(w.clerk, "/api/brant/v1/ai/delegations/del.order/approval_grants")["approval_grants"].([]any); len(gs) != 1 {
		t.Fatalf("grants %v", gs)
	}
	revoke := map[string]any{"id": "grant.http", "expected_version": 1, "reason": "不再需要"}
	status, out = w.send(w.agent, "ai", ai.Contract, "ai.approval_grant.revoke", revoke)
	refused(t, "the agent revokes a grant", status, out, kernel.CodePermissionDenied)
	if status, out = w.send(w.clerk, "ai", ai.Contract, "ai.approval_grant.revoke", revoke); status != http.StatusOK || out["status"] != "revoked" {
		t.Fatalf("revoke grant: %d %v", status, out)
	}
	status, out = w.send(w.agent, "ai", ai.Contract, "ai.delegation.revoke", map[string]any{"id": "del.http", "expected_version": 1, "reason": "x"})
	refused(t, "the agent revokes a delegation", status, out, kernel.CodePermissionDenied)
	if status, out = w.send(w.clerk, "ai", ai.Contract, "ai.delegation.revoke", map[string]any{"id": "del.http", "expected_version": 1,
		"reason": "调岗"}); status != http.StatusOK || out["status"] != "revoked" || out["row_version"] != float64(2) {
		t.Fatalf("revoke delegation: %d %v", status, out)
	}

	callsOut, _ := w.get(w.clerk, "/api/brant/v1/ai/tasks/task.ai/model_calls")["calls"].([]any)
	if len(callsOut) < 2 {
		t.Fatalf("calls %v", callsOut)
	}
	first, tool := callsOut[0].(map[string]any), callsOut[1].(map[string]any)
	if first["kind"] != "model" || first["model_returned"] != "scripted-q01-v1" || first["input_tokens"] != float64(900) || first["prompt_sha256"] == nil ||
		first["cost"].(map[string]any)["amount"] == nil || tool["kind"] != "tool" || tool["tool_command"] != evdapi.CommandRegisterSource || tool["action_id"] == nil {
		t.Fatalf("model call %v, tool call %v", first, tool)
	}
	ref := r.Ref()
	b := w.get(w.clerk, "/api/brant/v1/ai/execution_bundles/"+ref.BundleID+"/"+ref.SHA256)
	if b["sha256"] != ref.SHA256 || b["bundle"].(map[string]any)["bundle_id"] != ref.BundleID {
		t.Fatalf("bundle %v", b)
	}
	if status, out = w.clerk.do("GET", "/api/brant/v1/ai/execution_bundles/"+ref.BundleID+"/"+strings.Repeat("0", 64), nil); status != http.StatusNotFound {
		t.Fatalf("another hash: %d %v", status, out)
	}

	entries, _ := w.get(w.auditor, "/api/brant/v1/ai/audit")["entries"].([]any)
	var seen []string
	for _, e := range entries {
		m := e.(map[string]any)
		seen = append(seen, m["command"].(string)+" "+m["actor"].(map[string]any)["id"].(string)+" "+m["outcome"].(string))
	}
	for _, want := range []string{"ai.delegation.grant " + agentID + " rejected", "ai.approval_grant.issue " + agentID + " rejected",
		"ai.delegation.grant clerk.http succeeded", "ai.approval_grant.revoke clerk.http succeeded"} {
		if !slices.Contains(seen, want) {
			t.Errorf("AI audit lacks %q: %v", want, seen)
		}
	}
}
