//go:build integration

package bootstrap_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai"
	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/approvalgrant"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/execution"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The execution entry and the agent runner over the composed contexts on PostgreSQL 16 (WBS
// S4-03b; decision D-2; AI-INV-003, AI-INV-004, TSK-INV-003; UL AI-Receipt; baseline 17.1–17.4).
// A scripted model (the default and the only model in CI, decision E-4) drives an AI task of the Q01
// profile: every tool call goes through the gateway and the Executor — claim, intent recorded in TSK,
// command sent with the action's key (and the task's claim on its bound root), outcome reconciled
// with the owning context's ledger — and every model and tool call is reserved against the task's
// budget in brant_ai before it is made.

const agentID = "agent.q01"

type agentWorld struct {
	*trdWorld
	sys, clerk, agent context.Context
}

func newAgentWorld(t *testing.T, orders bool) *agentWorld {
	var q *trdWorld
	if orders {
		q = newOrderWorld(t)
	} else {
		q = newTrdWorld(t)
	}
	every := append(append(slices.Clone(allPermissions), tskPermissions...), "action.record_attempt", "action.reconcile")
	clerk := append(slices.Clone(every), templatePermissions(t, "sales_clerk")...)
	w := &agentWorld{trdWorld: q,
		sys:   q.provision(grantOf(q, "tsk", kernel.ActorSystem, "company", every...)),
		clerk: q.provision(grantOf(q, "clerk.ai", kernel.ActorHuman, "company", clerk...)),
		agent: q.provision(grantOf(q, agentID, kernel.ActorAI, "company", templatePermissions(t, "ai_q01_agent")...)),
	}
	if _, err := q.platform.ProvisionAgentRunner(context.Background(), q.scope("company.A")); err != nil {
		t.Fatal(err)
	}
	return w
}

// runner assembles the agent runner of the world's company with model m.
func (w *agentWorld) runner(m model.Port, accepts ...string) *execution.Runner {
	w.t.Helper()
	r, err := w.c.AI.Runner(ai.RunnerConfig{Scopes: []kernel.Scope{w.scope("company.A")}, Agents: []string{agentID}, Model: m,
		Profile: execution.ScriptedProfile("scripted-q01-v1"), Budget: execution.Q01DefaultBudget(), Accepts: accepts, Sources: w.c.EVD})
	w.must(err)
	w.must(r.Init(context.Background()))
	return r
}

// aiTask creates task id for the agent in control mode ai, settles its scope and delegates caps to
// the agent for it as the clerk.
func (w *agentWorld) aiTask(id, goal string, target tskapi.Target, budget *tskapi.Budget, bundle *tskapi.BundleRef, caps ...string) tskapi.TaskReceipt {
	w.t.Helper()
	r, err := w.c.TSK.CreateTask(w.clerk, tskapi.CreateTask{IdempotencyKey: w.key(), TaskID: id, GoalCode: goal, ControlMode: tskapi.ModeAI,
		AgentID: agentID, Target: target, Budget: budget, ExecutionBundle: bundle})
	w.must(err)
	r, err = w.c.TSK.ScopeTask(w.sys, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion})
	w.must(err)
	r, err = w.c.TSK.ScopeTask(w.clerk, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, Note: "范围确认"})
	w.must(err)
	autonomy := "draft"
	if slices.Contains(caps, trdapi.CommandPrepareOrder) || slices.Contains(caps, trdapi.CommandRequestOrderReview) {
		autonomy = "submit"
	}
	_, err = w.c.AI.GrantDelegation(w.clerk, policy.GrantDelegation{IdempotencyKey: w.key(), DelegationID: "del." + id, AgentID: agentID, TaskID: id,
		Capabilities: caps, Autonomy: autonomy, ExpiresAt: time.Now().Add(24 * time.Hour)})
	w.must(err)
	return r
}

func (w *agentWorld) taskView(id string) tskapi.TaskView {
	w.t.Helper()
	v, err := w.c.TSK.GetTask(w.clerk, id)
	w.must(err)
	return v
}

func bundleRef(r *execution.Runner) *tskapi.BundleRef {
	ref := r.Ref()
	return &tskapi.BundleRef{BundleID: ref.BundleID, SHA256: ref.SHA256}
}

func budgetOf(calls, retries int) *tskapi.Budget {
	return &tskapi.Budget{MaxModelCalls: calls, MaxRetries: retries, MaxCost: &tskapi.CostCap{Currency: "USD", MinorUnits: 2, Amount: "1.00"}}
}

// toolArgs is v as tool call arguments: the api command's fields, without null members and empty
// strings, which the gateway refuses.
func toolArgs(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var doc any
	if err = json.Unmarshal(raw, &doc); err != nil {
		panic(err)
	}
	raw, err = json.Marshal(prune(doc))
	if err != nil {
		panic(err)
	}
	return raw
}

func prune(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range t {
			if e == nil || e == "" {
				continue
			}
			out[k] = prune(e)
		}
		return out
	case []any:
		for i := range t {
			t[i] = prune(t[i])
		}
	}
	return v
}

func tc(id, name string, args any) model.ToolCall {
	return model.ToolCall{ID: id, Name: name, Arguments: toolArgs(args)}
}

func calls(cs ...model.ToolCall) model.Step {
	return model.Step{Reply: model.Message{ToolCalls: cs}, Usage: model.Usage{InputTokens: 900, OutputTokens: 120}}
}

func done(text string) model.Step {
	return model.Step{Reply: model.Message{Content: text}, Usage: model.Usage{InputTokens: 800, OutputTokens: 20}}
}

// An AI task of the Q01 profile under the scripted model (AI-INV-001..004, TSK-INV-003, D-2, UL
// AI-Receipt): the agent registers the inquiry and drafts a quote root — each tool call recorded in
// TSK before it is sent and VERIFIED from the owning context's ledger —, the runner binds the root
// the task's draft created (task.bind_object, TRD's epoch), the model's repeat of the same request
// is the same intent and answers its result without a second send (CT11), an injected human-only
// call is refused at the gateway, and the model's claim that the work is done only starts
// verification: goal.verify finds the quote not ready and the task waits for people. Every model and
// tool call is in brant_ai's ledger with the model ID the model returned, its tokens and cost 0; the
// ExecutionBundle is recorded and readable.
func TestAgentRunnerDraftsThroughTheExecutor(t *testing.T) {
	w := newAgentWorld(t, false)
	src := evdapi.RegisterSource{SourceID: "src.ai.1", SourceKind: evdapi.KindManualText, ContentText: "合成询价：120件 sku.1，请报价。"}
	quote := trdapi.SaveDraft{QuoteID: "quote.ai", Data: w.data()}
	m := model.NewScripted("scripted-q01-v1",
		calls(tc("c1", evdapi.CommandRegisterSource, src), tc("c2", trdapi.CommandSaveDraft, quote)),
		calls(tc("c3", evdapi.CommandRegisterSource, src), tc("c4", trdapi.CommandFreeze, map[string]any{"QuoteID": "quote.ai", "ExpectedVersion": 1})),
		done("报价已就绪，任务完成。"),
	)
	r := w.runner(m)
	w.aiTask("task.ai.1", tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1"}, budgetOf(10, 2), bundleRef(r),
		evdapi.CommandRegisterSource, trdapi.CommandSaveDraft)
	// Another person delegates the same tools for the task; its delegation sorts first, but the steps
	// run under the owner's (TSK names the owner as the initiator at TRD, D-5).
	other := w.provision(grantOf(w.trdWorld, "clerk.other", kernel.ActorHuman, "company", templatePermissions(t, "sales_clerk")...))
	_, err := w.c.AI.GrantDelegation(other, policy.GrantDelegation{IdempotencyKey: w.key(), DelegationID: "del.a.other", AgentID: agentID,
		TaskID: "task.ai.1", Capabilities: []string{evdapi.CommandRegisterSource, trdapi.CommandSaveDraft}, Autonomy: "draft",
		ExpiresAt: time.Now().Add(24 * time.Hour)})
	w.must(err)

	res, err := r.Pass(context.Background())
	w.must(err)
	if len(res) != 1 || m.Remaining() != 0 {
		t.Fatalf("pass %+v, %d steps left", res, m.Remaining())
	}
	turn := res[0]
	if turn.ModelCalls != 3 || turn.ToolCalls != 4 || !turn.Verified || turn.Lifecycle != tskapi.StateWaitingInput ||
		fmt.Sprint(turn.Refusals) != "[quote.freeze PERMISSION_DENIED]" || len(turn.Receipts) != 3 {
		t.Fatalf("turn %+v", turn)
	}
	for i, rc := range turn.Receipts {
		if rc.Stage != execution.Verified || rc.Stored == nil || rc.ActionID != tskapi.ActionIDOf(rc.BusinessIntentID) {
			t.Fatalf("receipt %d %+v", i, rc)
		}
	}
	if !turn.Receipts[2].Replayed || turn.Receipts[2].ActionID != turn.Receipts[0].ActionID || turn.Receipts[0].Replayed {
		t.Fatalf("CT11: the repeat is the first source's action: %+v", turn.Receipts)
	}
	// The root the task created is bound to it at TRD's epoch; the draft is the agent's.
	v := w.taskView("task.ai.1")
	qv := w.getQuote("quote.ai")
	if v.Binding == nil || v.Binding.QuoteID != "quote.ai" || v.Binding.State != tskapi.BindingBound || qv.Control.TaskID != "task.ai.1" ||
		qv.Control.ControlEpoch != v.ControlEpoch || v.ControlEpoch < 1 {
		t.Fatalf("binding %+v, TRD control %+v", v.Binding, qv.Control)
	}
	// TSK holds two committed actions, each with one attempt and the target's own receipt.
	actions, err := w.c.TSK.TaskActions(w.clerk, "task.ai.1")
	w.must(err)
	if len(actions) != 2 {
		t.Fatalf("actions %+v", actions)
	}
	for _, a := range actions {
		if a.State != tskapi.ActionCommitted || a.Attempts != 1 || a.RegisteredBy.ID != agentID || a.Log[0].DelegationID != "del.task.ai.1" {
			t.Fatalf("action %+v", a)
		}
	}
	// The model's word changed nothing: goal.verify judged the records and the task waits.
	if v.Lifecycle != tskapi.StateWaitingInput || v.Wait == nil || len(v.Wait.Items) == 0 {
		t.Fatalf("task %s %+v", v.Lifecycle, v.Wait)
	}
	// The ledger: 3 model calls, 4 tool calls, cost 0, the model ID as returned, the budget of the task.
	ledger, err := w.c.AI.ModelCalls(w.clerk, "task.ai.1")
	w.must(err)
	var model, tool, failed int
	for _, c := range ledger {
		switch c.Kind {
		case executionevidence.KindModel:
			model++
			if c.State != executionevidence.StateSucceeded || c.ModelReturned != "scripted-q01-v1" || c.InputTokens == nil || *c.InputTokens == 0 ||
				c.Cost == nil || !c.Cost.IsZero() || c.Bundle != r.Ref() || c.ActorID != agentID || c.ProfileID != "scripted.v1" {
				t.Fatalf("model call %+v", c)
			}
		case executionevidence.KindTool:
			tool++
			if c.State == executionevidence.StateFailed {
				failed++
				if c.ToolCommand != trdapi.CommandFreeze || c.Code != kernel.CodePermissionDenied {
					t.Fatalf("tool call %+v", c)
				}
			} else if c.ActionID == "" {
				t.Fatalf("tool call %+v", c)
			}
		}
	}
	if model != 3 || tool != 4 || failed != 1 {
		t.Fatalf("ledger %d model, %d tool, %d failed", model, tool, failed)
	}
	b, err := w.c.AI.ExecutionBundle(w.clerk, r.Ref())
	w.must(err)
	if b.BundleID != execution.Q01BundleID || b.PromptSHA256 != executionevidence.SHA256Hex([]byte(execution.Q01Prompt)) || len(b.Tools) == 0 {
		t.Fatalf("bundle %+v", b)
	}
	// The next pass leaves the waiting task alone: no model call.
	res, err = r.Pass(context.Background())
	w.must(err)
	if len(res) != 1 || res[0].ModelCalls != 0 || !strings.Contains(res[0].Skipped, tskapi.StateWaitingInput) {
		t.Fatalf("second pass %+v", res)
	}
	// A person takes the bound task over: TRD stops the root at once; the runner completes the stop
	// barrier (task.finish_stop as system:ai.runner, TRD enter_manual): the task is paused in manual
	// mode, the root manual at TRD, and the runner does nothing more with it.
	_, err = w.c.TSK.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.ai.1", ExpectedVersion: v.TaskVersion,
		Reason: "人工接管报价"})
	w.must(err)
	res, err = r.Pass(context.Background())
	w.must(err)
	v, qv = w.taskView("task.ai.1"), w.getQuote("quote.ai")
	if len(res) != 1 || !res[0].Stopped || res[0].ModelCalls != 0 || v.Lifecycle != tskapi.StatePaused || v.ControlMode != tskapi.ModeManual ||
		qv.Control.State != trdapi.ControlManual || qv.Control.ControlEpoch != v.ControlEpoch {
		t.Fatalf("pass %+v, task %s %s, TRD %+v", res, v.Lifecycle, v.ControlMode, qv.Control)
	}
	if res, err = r.Pass(context.Background()); err != nil || res[0].ModelCalls != 0 || res[0].Stopped {
		t.Fatalf("after the take-over %+v %v", res, err)
	}
}

// turnOn takes one turn of r on task in company.A.
func (w *agentWorld) turnOn(r *execution.Runner, task string) execution.TurnResult {
	w.t.Helper()
	res, err := r.Turn(context.Background(), w.scope("company.A"), agentID, task)
	w.must(err)
	return res
}

func (w *agentWorld) ledger(task string) []executionevidence.Call {
	w.t.Helper()
	calls, err := w.c.AI.ModelCalls(w.clerk, task)
	w.must(err)
	return calls
}

// AI-INV-003, AN09, BC-AT26: budget and retry limits are enforced at the execution entry before every
// model call, not by the prompt. A task whose two model calls are used is paused with the code, the
// spend and the condition to resume before a third call is made; a model that keeps answering
// unavailable is retried within max_retries and the task then pauses (model_unavailable,
// TOOL_UNAVAILABLE). A person takes the task over (AN08): it is manual from then on and the runner
// leaves it alone — nothing claims an AI success.
func TestAgentRunnerPausesAtItsBudget(t *testing.T) {
	w := newAgentWorld(t, false)
	src := func(id string) evdapi.RegisterSource {
		return evdapi.RegisterSource{SourceID: id, SourceKind: evdapi.KindManualText, ContentText: "合成询价 " + id}
	}
	m := model.NewScripted("scripted-q01-v1", calls(tc("c1", evdapi.CommandRegisterSource, src("src.b1"))),
		calls(tc("c2", evdapi.CommandRegisterSource, src("src.b2"))), calls(tc("c3", evdapi.CommandRegisterSource, src("src.b3"))))
	r := w.runner(m)
	w.aiTask("task.budget", tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1"}, budgetOf(2, 1), bundleRef(r), evdapi.CommandRegisterSource)
	res := w.turnOn(r, "task.budget")
	if res.Paused != tskapi.PauseBudgetExhausted || res.ModelCalls != 2 || m.Remaining() != 1 || res.Lifecycle != tskapi.StatePaused {
		t.Fatalf("turn %+v, %d steps left", res, m.Remaining())
	}
	v := w.taskView("task.budget")
	if v.Pause == nil || v.Pause.Cause != tskapi.PauseBudgetExhausted || !strings.Contains(v.Pause.Reason, "BUDGET_EXHAUSTED at /budget/max_model_calls") ||
		!strings.Contains(v.Pause.Reason, "spent 2 model calls, 2 tool calls, 0 retries") || !strings.Contains(v.Pause.Reason, "of 1.00 USD") ||
		v.Pause.ResumeCondition == "" || v.Pause.ResponsibleID != "clerk.ai" {
		t.Fatalf("pause %+v", v.Pause)
	}
	if n := len(w.ledger("task.budget")); n != 4 {
		t.Fatalf("ledger has %d calls", n)
	}
	if res = w.turnOn(r, "task.budget"); res.ModelCalls != 0 || res.Skipped == "" {
		t.Fatalf("a paused task got a turn: %+v", res)
	}

	down := model.NewScripted("scripted-q01-v1", model.Step{Err: model.ErrUnavailable}, model.Step{Err: model.ErrUnavailable},
		model.Step{Err: model.ErrUnavailable}, model.Step{Err: model.ErrUnavailable})
	r = w.runner(down)
	w.aiTask("task.down", tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1"}, budgetOf(10, 2), bundleRef(r), evdapi.CommandRegisterSource)
	res = w.turnOn(r, "task.down")
	if res.Paused != tskapi.PauseModelUnavailable || res.ModelCalls != 3 || down.Remaining() != 1 {
		t.Fatalf("turn %+v, %d steps left", res, down.Remaining())
	}
	v = w.taskView("task.down")
	if !strings.Contains(v.Pause.Reason, "TOOL_UNAVAILABLE") || !strings.Contains(v.Pause.Reason, "2 retries") {
		t.Fatalf("pause %+v", v.Pause)
	}
	var retries []bool
	for _, c := range w.ledger("task.down") {
		if c.State != executionevidence.StateUnavailable || c.Code != "TOOL_UNAVAILABLE" || c.Cost == nil || !c.Cost.IsZero() {
			t.Fatalf("call %+v", c)
		}
		retries = append(retries, c.Retry)
	}
	if fmt.Sprint(retries) != "[false true true]" {
		t.Fatalf("retries %v", retries)
	}
	// AN08 / BC-AT26: a person takes over; the runner completes the stop barrier; the task is manual
	// and the runner does nothing more.
	_, err := w.c.TSK.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.down", ExpectedVersion: v.TaskVersion,
		Reason: "模型持续不可用，人工接管"})
	w.must(err)
	if res = w.turnOn(r, "task.down"); !res.Stopped || res.ModelCalls != 0 {
		t.Fatalf("turn on the stopping task %+v", res)
	}
	if v = w.taskView("task.down"); v.ControlMode != tskapi.ModeManual || v.Lifecycle != tskapi.StatePaused {
		t.Fatalf("task %s %s", v.ControlMode, v.Lifecycle)
	}
	if res = w.turnOn(r, "task.down"); res.ModelCalls != 0 || res.Skipped != "not an AI task of this agent" || down.Remaining() != 1 {
		t.Fatalf("turn on a manual task %+v", res)
	}
}

// CT14 and Q01 V0.1 tests/test_execution.py::test_incompatible_persisted_bundle_stops_actions
// (baseline 17.3): a task whose recorded ExecutionBundle is not the runner's — another hash, or none
// — is paused before any model or tool call (EXECUTION_BUNDLE_INCOMPATIBLE); a runner that lists the
// task's bundle as compatible runs it.
func TestAgentRunnerRunsOnlyACompatibleBundle(t *testing.T) {
	w := newAgentWorld(t, false)
	m := model.NewScripted("scripted-q01-v1", done("没有可做的事。"))
	r := w.runner(m)
	if err := r.CheckBundle(tskapi.TaskView{ExecutionBundle: bundleRef(r)}); err != nil {
		t.Fatal(err)
	}
	old := strings.Repeat("ab", 32)
	w.aiTask("task.old", tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1"}, nil,
		&tskapi.BundleRef{BundleID: execution.Q01BundleID, SHA256: old}, evdapi.CommandRegisterSource)
	w.aiTask("task.none", tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1"}, nil, nil, evdapi.CommandRegisterSource)
	for _, task := range []string{"task.old", "task.none"} {
		res := w.turnOn(r, task)
		v := w.taskView(task)
		if res.Paused != tskapi.PauseConflict || res.ModelCalls != 0 || v.Pause == nil ||
			!strings.Contains(v.Pause.Reason, "EXECUTION_BUNDLE_INCOMPATIBLE") || len(w.ledger(task)) != 0 {
			t.Fatalf("%s: turn %+v, pause %+v", task, res, v.Pause)
		}
	}
	if m.Remaining() != 1 {
		t.Fatal("the model was asked")
	}
	w.aiTask("task.compatible", tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1"}, nil,
		&tskapi.BundleRef{BundleID: execution.Q01BundleID, SHA256: old}, evdapi.CommandRegisterSource)
	res := w.turnOn(w.runner(m, old), "task.compatible")
	if res.ModelCalls != 1 || !res.Verified || res.Lifecycle != tskapi.StateWaitingInput {
		t.Fatalf("turn %+v", res)
	}
	// A task without its own budget runs under the bundle's default one, recorded as the runner's.
	calls := w.ledger("task.compatible")
	if len(calls) != 1 || calls[0].Bundle.SHA256 != r.Ref().SHA256 {
		t.Fatalf("ledger %+v", calls)
	}
}

// D-7 and AI-INV-002 through the runner: the agent's order.prepare needs a person's approval grant
// for exactly that request. Without one the task waits for approval (task.wait approval, awaiting the
// runner's proposal, with the command, request hash and object as items) and nothing is recorded or
// sent; the runner leaves it waiting until the clerk issues the grant, then resumes the task (arrival:
// the grant) and the agent's next call runs with it: one order, prepared by the agent, the grant used
// by the action's key, the delegation and grant recorded on the attempt.
func TestAgentRunnerWaitsForAnApprovalGrant(t *testing.T) {
	w := newAgentWorld(t, true)
	_, acc := w.accepted("quote.1")
	prepare := map[string]any{"OrderID": "order.ai.1", "QuoteID": acc.QuoteID, "BusinessRevision": acc.BusinessRevision,
		"BusinessHash": acc.BusinessHash, "AcceptanceID": acc.ObjectID}
	m := model.NewScripted("scripted-q01-v1", calls(tc("c1", trdapi.CommandPrepareOrder, prepare)), calls(tc("c2", trdapi.CommandPrepareOrder, prepare)),
		done("订单已生效。"))
	r := w.runner(m)
	w.aiTask("task.order", tskapi.GoalOrderActive, tskapi.Target{QuoteID: "quote.1", CustomerID: "customer.1"}, budgetOf(10, 2), bundleRef(r),
		trdapi.CommandPrepareOrder)
	res := w.turnOn(r, "task.order")
	v := w.taskView("task.order")
	if res.Waiting != tskapi.WaitApproval || v.Lifecycle != tskapi.StateWaitingApproval || v.Wait.Awaiting == nil ||
		v.Wait.Awaiting.Kind != execution.ArrivalApprovalGrant || v.Wait.Awaiting.Version != acc.BusinessRevision || len(v.Wait.Items) != 3 ||
		v.Wait.ResponsibleID != "clerk.ai" {
		t.Fatalf("turn %+v, task %s %+v", res, v.Lifecycle, v.Wait)
	}
	hash := strings.TrimPrefix(v.Wait.Items[1], "request_hash ")
	if v.Wait.Awaiting.ID != execution.ProposalID("task.order", trdapi.CommandPrepareOrder, hash) {
		t.Fatalf("awaiting %+v", v.Wait.Awaiting)
	}
	if actions, err := w.c.TSK.TaskActions(w.clerk, "task.order"); err != nil || len(actions) != 0 {
		t.Fatalf("actions %+v %v", actions, err)
	}
	if res = w.turnOn(r, "task.order"); res.ModelCalls != 0 || res.Skipped != "still waiting for the approval grant" {
		t.Fatalf("turn %+v", res)
	}
	_, err := w.c.AI.IssueApprovalGrant(w.clerk, approvalgrantIssue(w, "grant.order", "del.task.order", trdapi.CommandPrepareOrder, hash, "quote.1",
		acc.BusinessRevision))
	w.must(err)
	res = w.turnOn(r, "task.order")
	if res.ModelCalls != 2 || len(res.Receipts) != 1 || res.Receipts[0].Stage != execution.Verified || !res.Verified || m.Remaining() != 0 {
		t.Fatalf("turn %+v", res)
	}
	if o := w.getOrder("order.ai.1"); o.PreparedBy.ID != agentID || o.Status != trdapi.OrderDraft {
		t.Fatalf("order %+v", o)
	}
	g, err := w.c.AI.GetApprovalGrant(w.clerk, "grant.order")
	w.must(err)
	if g.Status != "used" || g.UsedKey != res.Receipts[0].ActionID {
		t.Fatalf("grant %+v", g)
	}
	a, err := w.c.TSK.GetAction(w.clerk, res.Receipts[0].ActionID)
	w.must(err)
	if a.State != tskapi.ActionCommitted || a.Log[0].GrantID != "grant.order" || a.Log[0].DelegationID != "del.task.order" {
		t.Fatalf("action %+v", a)
	}
	v = w.taskView("task.order")
	var resumed bool
	for _, h := range v.History {
		resumed = resumed || (h.Command == tskapi.CommandResume && strings.Contains(h.Reason, "grant.order"))
	}
	if !resumed || v.Lifecycle != tskapi.StateWaitingInput {
		t.Fatalf("history %+v, %s", v.History, v.Lifecycle)
	}
}

func approvalgrantIssue(w *agentWorld, id, delegation, command, hash, object string, version int64) approvalgrant.IssueGrant {
	return approvalgrant.IssueGrant{IdempotencyKey: w.key(), GrantID: id, DelegationID: delegation, Command: command, RequestHash: hash,
		ObjectID: object, ObjectVersion: version, ExpiresAt: time.Now().Add(time.Hour)}
}

func freezeStep(w *agentWorld, task, intent string, expected int64, sends *int) execution.Step {
	return execution.Step{TaskID: task, BusinessIntentID: intent, Context: tskapi.TargetTRD,
		Command: trdapi.Freeze{QuoteID: "quote.h", ExpectedVersion: expected, WorkingRevision: 1},
		Send: func(ctx context.Context, cmd any) (capability.Result, error) {
			*sends++
			r, err := w.c.TRD.Freeze(ctx, cmd.(trdapi.Freeze))
			return capability.Result{Receipt: capability.Receipt(r.Receipt), Detail: r}, err
		}}
}

// D-2: a person's step on an AI task's bound root goes through the execution entry (the path of unit
// C3's task-step route): directly TRD refuses it (TRD-INV-015, OBJECT_CONTROL_CONFLICT); through
// RunStep it carries the task's claim, is recorded before it is sent and VERIFIED from TRD's ledger.
// A refused attempt is recorded with TRD's code (ACCEPTED, not VERIFIED) and the corrected one is the
// same intent's next attempt with the same key; the intent run again returns its result without a
// send (CT11).
func TestHumanTaskStepsGoThroughTheExecutor(t *testing.T) {
	w := newAgentWorld(t, false)
	w.drafted("quote.h")
	r := w.aiTask("task.h", tskapi.GoalQuoteReady, tskapi.Target{QuoteID: "quote.h", CustomerID: "customer.1"}, nil, nil, evdapi.CommandRegisterSource)
	_, err := w.c.TSK.BindObject(w.sys, tskapi.BindObject{IdempotencyKey: w.key(), TaskID: "task.h", ExpectedVersion: r.RowVersion, QuoteID: "quote.h"})
	w.must(err)
	root := w.getQuote("quote.h").RootVersion
	_, err = w.c.TRD.Freeze(w.clerk, trdapi.Freeze{IdempotencyKey: w.key(), QuoteID: "quote.h", ExpectedVersion: root, WorkingRevision: 1})
	w.want(err, "OBJECT_CONTROL_CONFLICT")

	sends := 0
	rc, err := w.c.AI.RunStep(w.clerk, freezeStep(w, "task.h", "human.freeze.h", root+5, &sends))
	w.want(err, kernel.CodeVersionConflict)
	if rc.Stage != execution.Accepted || rc.Attempt != 1 || sends != 1 {
		t.Fatalf("refused step %+v", rc)
	}
	a, err := w.c.TSK.GetAction(w.clerk, rc.ActionID)
	w.must(err)
	if a.State != tskapi.ActionRejected || a.Log[len(a.Log)-1].Code != kernel.CodeVersionConflict {
		t.Fatalf("action %+v", a)
	}
	rc, err = w.c.AI.RunStep(w.clerk, freezeStep(w, "task.h", "human.freeze.h", root, &sends))
	w.must(err)
	if rc.Stage != execution.Verified || rc.Attempt != 2 || rc.Result == nil || rc.Stored == nil || rc.Stored.ActionID != rc.Result.ActionID ||
		rc.ControlEpoch < 1 || sends != 2 {
		t.Fatalf("step %+v", rc)
	}
	if q := w.getQuote("quote.h"); q.RootVersion != root+1 || q.WorkingRevision != 0 {
		t.Fatalf("quote v%d working %d", q.RootVersion, q.WorkingRevision)
	}
	again, err := w.c.AI.RunStep(w.clerk, freezeStep(w, "task.h", "human.freeze.h", root, &sends))
	w.must(err)
	if again.Stage != execution.Verified || !again.Replayed || sends != 2 || *again.Stored != *rc.Stored {
		t.Fatalf("replay %+v", again)
	}
}

// AN04 and Q01 V0.1 tests/test_execution.py::test_revoked_actor_cannot_execute_or_read_old_result: a
// revoked delegation stops the agent at its next turn — the running task is paused for its owner
// before any model call; once the agent's own grant is withdrawn, the same step run again as the
// agent is PERMISSION_DENIED, not the old result, and the agent can no longer read it, while the
// committed action stays in the task's history for the clerk (CT16).
func TestRevokedAuthorityStopsTheAgent(t *testing.T) {
	w := newAgentWorld(t, false)
	src := evdapi.RegisterSource{SourceID: "src.rev", SourceKind: evdapi.KindManualText, ContentText: "合成询价：撤销前"}
	m := model.NewScripted("scripted-q01-v1", calls(tc("c1", evdapi.CommandRegisterSource, src)), done("x"))
	r := w.runner(m)
	r.MaxSteps = 1
	w.aiTask("task.rev", tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1"}, nil, bundleRef(r), evdapi.CommandRegisterSource)
	res := w.turnOn(r, "task.rev")
	if len(res.Receipts) != 1 || res.Lifecycle != tskapi.StateRunning {
		t.Fatalf("turn %+v", res)
	}
	_, err := w.c.AI.RevokeDelegation(w.clerk, policy.RevokeDelegation{IdempotencyKey: w.key(), DelegationID: "del.task.rev", ExpectedVersion: 1,
		Reason: "发起人调岗"})
	w.must(err)
	res = w.turnOn(r, "task.rev")
	v := w.taskView("task.rev")
	if res.Paused != tskapi.PauseConflict || res.ModelCalls != 0 || m.Remaining() != 1 || !strings.Contains(v.Pause.Reason, "PERMISSION_DENIED") {
		t.Fatalf("turn %+v, pause %+v", res, v.Pause)
	}
	actions, err := w.c.TSK.TaskActions(w.clerk, "task.rev")
	w.must(err)
	if len(actions) != 1 || actions[0].State != tskapi.ActionCommitted {
		t.Fatalf("actions %+v", actions)
	}
	actionID := actions[0].ActionID
	// The agent's own grant is withdrawn.
	none := w.provision(grantOf(w.trdWorld, agentID, kernel.ActorAI, "company"))
	sends := 0
	_, err = w.c.AI.RunStep(none, execution.Step{TaskID: "task.rev", BusinessIntentID: actions[0].BusinessIntentID, Context: tskapi.TargetEVD,
		Command: src, Send: func(ctx context.Context, cmd any) (capability.Result, error) {
			sends++
			return capability.Result{}, nil
		}})
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.c.TSK.GetAction(none, actionID)
	w.want(err, kernel.CodePermissionDenied)
	if sends != 0 {
		t.Fatal("sent")
	}
	if a, err := w.c.TSK.GetAction(w.clerk, actionID); err != nil || a.State != tskapi.ActionCommitted {
		t.Fatalf("history %+v %v", a, err)
	}
}

// Baseline 17.2 and AI-INV-003 (恢复前核查已有副作用): what an earlier run left in flight is settled from
// the owning context's ledger. An answer lost after EVD committed is reported to TSK as unknown, which
// TSK checks against EVD's ledger and records committed: VERIFIED, one source. An attempt recorded and sent by an executor that died before it
// recorded the outcome is reconciled by the runner before the model is asked: the model's first
// request already shows the action committed.
func TestExecutorSettlesWhatWasInFlight(t *testing.T) {
	w := newAgentWorld(t, false)
	m := model.NewScripted("scripted-q01-v1", model.Step{Reply: model.Message{Content: "已登记。"}, Check: func(req model.Request) error {
		if !strings.Contains(req.Messages[1].Content, `"state":"committed"`) {
			return fmt.Errorf("the open action was not settled before the model call: %s", req.Messages[1].Content)
		}
		return nil
	}})
	r := w.runner(m)
	w.aiTask("task.flight", tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1"}, nil, bundleRef(r), evdapi.CommandRegisterSource)

	lost := evdapi.RegisterSource{SourceID: "src.lost", SourceKind: evdapi.KindManualText, ContentText: "合成询价：回执丢失"}
	rc, err := w.c.AI.RunStep(w.agent, execution.Step{TaskID: "task.flight", BusinessIntentID: "intent.lost", Context: tskapi.TargetEVD, Command: lost,
		Send: func(ctx context.Context, cmd any) (capability.Result, error) {
			if _, err := w.c.EVD.RegisterSource(ctx, cmd.(evdapi.RegisterSource)); err != nil {
				return capability.Result{}, err
			}
			return capability.Result{}, fmt.Errorf("connection reset after the commit")
		}})
	w.must(err)
	if rc.Stage != execution.Verified || rc.Stored == nil || rc.Stored.ObjectID != "src.lost" || rc.Result != nil {
		t.Fatalf("lost answer %+v", rc)
	}
	a, err := w.c.TSK.GetAction(w.clerk, rc.ActionID)
	w.must(err)
	var states []string
	for _, l := range a.Log {
		states = append(states, l.State)
	}
	// The executor's observation "unknown" is checked against EVD's ledger first, which holds the
	// command: TSK records it committed (B1-D05).
	if fmt.Sprint(states) != "[submitted committed]" {
		t.Fatalf("log %v", states)
	}

	// An executor recorded and sent, then died.
	died := evdapi.RegisterSource{SourceID: "src.died", SourceKind: evdapi.KindManualText, ContentText: "合成询价：进程退出"}
	req, err := w.c.EVD.RequestOf(died)
	w.must(err)
	rec, err := w.c.TSK.RecordAttempt(w.agent, tskapi.RecordAttempt{IdempotencyKey: w.key(), TaskID: "task.flight", BusinessIntentID: "intent.died",
		Target: tskapi.TargetCommand{Context: tskapi.TargetEVD, Command: req.Command, RequestHash: req.RequestHash}, Outcome: tskapi.ActionSubmitted})
	w.must(err)
	died.IdempotencyKey = rec.IdempotencyKey
	_, err = w.c.EVD.RegisterSource(w.agent, died)
	w.must(err)
	res := w.turnOn(r, "task.flight")
	if res.ModelCalls != 1 || m.Remaining() != 0 || !res.Verified {
		t.Fatalf("turn %+v", res)
	}
	if a, err = w.c.TSK.GetAction(w.clerk, rec.ObjectID); err != nil || a.State != tskapi.ActionCommitted || a.Attempts != 1 {
		t.Fatalf("action %+v %v", a, err)
	}
}

// AI-INV-003 "并发步骤的额度保留不超出预算": eight concurrent reservations against a budget of three
// model calls — three are granted, five BUDGET_EXHAUSTED; a priced profile's reservations count
// against the cost cap until settled with the actual cost (HALF_UP once, to the cap's minor units),
// which releases the difference; brant_ai keeps the ledger as recorded.
func TestBudgetReservationsSerialize(t *testing.T) {
	w := newAgentWorld(t, false)
	r := w.runner(model.NewScripted("scripted-q01-v1"))
	ev := w.c.AI.Evidence
	profile := execution.ScriptedProfile("scripted-q01-v1")
	usd := kernel.MustCurrency("USD", 2)
	limits := executionevidence.Limits{MaxModelCalls: 3, MaxRetries: 0, Currency: usd, Source: "task"}
	reservation := func(task string, p *executionevidence.ModelProfile) executionevidence.Reservation {
		return executionevidence.Reservation{TaskID: task, TaskVersion: 1, Kind: executionevidence.KindModel, Bundle: r.Ref(), Profile: p,
			PromptSHA: r.Bundle.PromptSHA256}
	}
	results := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := ev.Reserve(w.agent, limits, reservation("task.conc", &profile))
			results <- err
		}()
	}
	granted, refused := 0, 0
	for range 8 {
		switch err := <-results; code(err) {
		case "OK":
			granted++
		case "BUDGET_EXHAUSTED":
			refused++
		default:
			t.Fatal(err)
		}
	}
	if granted != 3 || refused != 5 {
		t.Fatalf("granted %d, refused %d", granted, refused)
	}

	priced := profile
	priced.InputPerMTok, priced.OutputPerMTok, priced.ReservePerCall = "10", "30", "0.01"
	capped := executionevidence.Limits{MaxModelCalls: 10, Currency: usd, Source: "task"}
	two, _ := kernel.NewMoney(usd, 2)
	capped.MaxCost = &two
	c1, err := ev.Reserve(w.agent, capped, reservation("task.cost", &priced))
	w.must(err)
	_, err = ev.Reserve(w.agent, capped, reservation("task.cost", &priced))
	w.must(err)
	_, err = ev.Reserve(w.agent, capped, reservation("task.cost", &priced))
	w.want(err, "BUDGET_EXHAUSTED")
	in, out := int64(100), int64(100)
	cost, err := priced.Cost(usd, in, out) // 0.004 USD → 0.00
	w.must(err)
	_, err = ev.Settle(w.agent, c1.CallID, executionevidence.Outcome{State: executionevidence.StateSucceeded, ModelReturned: "m",
		InputTokens: &in, OutputTokens: &out, Cost: &cost})
	w.must(err)
	_, err = ev.Reserve(w.agent, capped, reservation("task.cost", &priced))
	w.must(err)
	spend, ok, err := ev.Spend(w.agent, "task.cost")
	w.must(err)
	if !ok || spend.ModelCalls != 3 || spend.Settled.MinorString() != "0" || spend.Open.MinorString() != "2" || spend.Tokens != 200 {
		t.Fatalf("spend %+v", spend)
	}
	_, err = ev.Settle(w.agent, c1.CallID, executionevidence.Outcome{State: executionevidence.StateFailed, Code: "TOOL_UNAVAILABLE", Cost: &cost})
	if err == nil {
		t.Fatal("a call was settled twice")
	}
	for _, stmt := range []string{
		`UPDATE brant_ai.model_call SET reserved_minor = 0 WHERE call_id = '` + c1.CallID + `'`,
		`DELETE FROM brant_ai.model_call WHERE call_id = '` + c1.CallID + `'`,
		`UPDATE brant_ai.task_budget SET source = 'runner' WHERE task_id = 'task.cost'`,
		`DELETE FROM brant_ai.execution_bundle`,
	} {
		err := policy.WithinScope(context.Background(), ev.DB, w.scope("company.A"), func(ctx context.Context, exec bentopg.Executor) error {
			_, err := exec.Exec(ctx, stmt)
			return err
		})
		if err == nil {
			t.Errorf("brant_ai accepted %s", stmt)
		}
	}
}

// brant-worker -agent runs the agent runner as a process: it refuses to start without its agents or
// without a script for the scripted model, logs the bundle tasks must name, takes its turn on the
// world's AI task with the scripted model of a script file (the model has nothing to do; goal.verify
// then sends the task back to people) and stops on SIGTERM. No key appears in its logs.
func TestWorkerAgentModeRunsTheRunner(t *testing.T) {
	w := newAgentWorld(t, false)
	bundle, err := w.c.AI.Bundle(ai.RunnerConfig{Profile: execution.ScriptedProfile("scripted-q01-v1"), Budget: execution.Q01DefaultBudget()})
	w.must(err)
	ref, err := bundle.Ref()
	w.must(err)
	w.aiTask("task.worker", tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1"}, nil,
		&tskapi.BundleRef{BundleID: ref.BundleID, SHA256: ref.SHA256}, evdapi.CommandRegisterSource)
	dir := t.TempDir()
	script := filepath.Join(dir, "script.json")
	w.must(os.WriteFile(script, []byte(`[{"content":"没有可做的事。","usage":{"InputTokens":5,"OutputTokens":3}}]`), 0o600))
	bin := filepath.Join(dir, "brant-worker")
	if out, err := exec.Command("go", "build", "-o", bin, "go.idp.xyz/idp-brant/cmd/brant-worker").CombinedOutput(); err != nil {
		t.Fatalf("build brant-worker: %v\n%s", err, out)
	}
	scope := w.scope("company.A")
	where := string(scope.Tenant) + "/" + string(scope.Company)
	for _, args := range [][]string{{"-agent", "-agent-scope", where}, {"-agent", "-agent-scope", where, "-agent-id", agentID}} {
		cmd := exec.Command(bin, args...)
		cmd.Env = os.Environ()
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("%v started:\n%s", args, out)
		}
	}
	var logs bytes.Buffer
	worker := exec.Command(bin, "-agent", "-agent-scope", where, "-agent-id", agentID, "-agent-poll", "200ms", "-model-script", script)
	worker.Env = append(os.Environ(), "IDP_BRANT_MODEL_API_KEY=never-logged-key")
	worker.Stdout, worker.Stderr = &logs, &logs
	w.must(worker.Start())
	deadline := time.Now().Add(60 * time.Second)
	for w.taskView("task.worker").Lifecycle != tskapi.StateWaitingInput {
		if time.Now().After(deadline) {
			_ = worker.Process.Kill()
			t.Fatalf("the runner did not take its turn:\n%s", logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	w.must(worker.Process.Signal(syscall.SIGTERM))
	if err := worker.Wait(); err != nil {
		t.Fatalf("graceful shutdown: %v\n%s", err, logs.String())
	}
	out := logs.String()
	if !strings.Contains(out, "running agents") || !strings.Contains(out, ref.SHA256) || strings.Contains(out, "never-logged-key") {
		t.Fatalf("logs:\n%s", out)
	}
	calls := w.ledger("task.worker")
	if len(calls) != 1 || calls[0].ModelReturned != "scripted-q01-v1" || *calls[0].OutputTokens != 3 {
		t.Fatalf("ledger %+v", calls)
	}
}
