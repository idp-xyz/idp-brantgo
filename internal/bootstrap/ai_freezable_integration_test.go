//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/execution"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// An agent's quote draft is one a person can freeze (S4 unit G6, WBS S4-03; the live trial of
// 2026-11-18, bin/ai-eval/live-20261118T105055Z, normal-1). TRD accepts a draft without the cost
// snapshot reference, because every member of QuoteData is optional in a draft. The live model sent
// one, was told the draft was saved, answered that it was done, and the person's quote.freeze was
// then refused COST_SNAPSHOT_MISSING with nobody to tell the agent.
//
// Now the gateway refuses the agent's draft that lacks what a freeze needs, before TRD is called, at
// the first member the freeze would miss and with the code and path the freeze would answer
// (capability.freezableDraft over trdapi.MissingForFreeze). The model is told in the call's own
// answer, the ledger and the next turn's facts keep the refusal, and nothing was saved or recorded in
// TRD or TSK. A person's save, plain or as a task step, and the freeze are unchanged.

const (
	fzTask  = "task.fz"
	fzQuote = "quote.fz"
)

var fzTools = []string{trdapi.CommandSaveDraft}

// fzDraft is the fixture's draft as the live model sent it: everything but the cost snapshot.
func fzDraft(w *agentWorld) trdapi.QuoteData {
	d := w.data()
	d.CostSnapshotRef = nil
	return d
}

func fzSave(w *agentWorld, call string, d trdapi.QuoteData) model.ToolCall {
	return tc(call, trdapi.CommandSaveDraft, trdapi.SaveDraft{QuoteID: fzQuote, ExpectedVersion: 0, Data: d})
}

// fzRefusal is the tool result of a refused call, decoded.
type fzRefusal struct {
	OK      *bool    `json:"ok"`
	Code    string   `json:"code"`
	Path    string   `json:"path"`
	Member  string   `json:"member"`
	Missing bool     `json:"missing"`
	Reason  string   `json:"reason"`
	Needs   string   `json:"needs"`
	Allowed []string `json:"allowed"`
}

// lastToolResult returns the tool message that ends a request, which is what the model learned of its
// last call.
func lastToolResult(req model.Request) (string, bool) {
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == model.RoleTool {
		return req.Messages[n-1].Content, true
	}
	return "", false
}

// checkRefusal is a script step's check that the request ends with the refusal of the draft without
// its cost snapshot, as the gateway answers it.
func checkRefusal(seen *string) func(model.Request) error {
	return func(req model.Request) error {
		content, ok := lastToolResult(req)
		if !ok {
			return fmt.Errorf("the request does not end with the tool's answer")
		}
		*seen = content
		var n fzRefusal
		if err := json.Unmarshal([]byte(content), &n); err != nil {
			return err
		}
		if n.OK == nil || *n.OK || n.Code != trdapi.CodeCostSnapshotMissing || n.Path != "/args/data/cost_snapshot_ref" ||
			n.Member != "Data.CostSnapshotRef" || !n.Missing || !strings.Contains(n.Reason, "cannot freeze") {
			return fmt.Errorf("the refusal the model was told: %s", content)
		}
		return nil
	}
}

// The model is told in the same turn: its draft without the cost snapshot is refused with the code
// and the path the freeze would answer, the member it is about and that it was missing; it sends the
// draft again whole and that one is saved, so the quote that exists is one a person can freeze. The
// refused call changed nothing in TRD and left no action in TSK; it is in the ledger and the audit.
func TestAnAgentsDraftWithoutTheCostSnapshotIsRefusedInTheSameTurn(t *testing.T) {
	w := newAgentWorld(t, false)
	var told string
	second := calls(fzSave(w, "c2", w.data()))
	second.Check = checkRefusal(&told)
	m := model.NewScripted("scripted-q01-v1", calls(fzSave(w, "c1", fzDraft(w))), second, done("报价草稿已保存。"))
	r := w.runner(m)
	w.aiTask(fzTask, tskapi.GoalQuoteReady, tskapi.Target{RfqID: "rfq.1", QuoteID: fzQuote, CustomerID: "customer.1"}, nil, bundleRef(r), fzTools...)
	res := w.turnOn(r, fzTask)
	if res.ModelCalls != 3 || res.ToolCalls != 2 || fmt.Sprint(res.Refusals) != "[quote.save_draft COST_SNAPSHOT_MISSING]" || len(res.Receipts) != 1 ||
		m.Remaining() != 0 {
		t.Fatalf("the turn %+v", res)
	}
	t.Logf("the model was told: %s", told)

	// TRD: the quote exists once, from the second draft, with every member a person's freeze needs.
	q := w.getQuote(fzQuote)
	if q.RootVersion != 1 || q.WorkingRevision != 1 || len(q.Revisions) != 1 || q.Revisions[0].Status != trdapi.StatusWorking ||
		q.Revisions[0].Data.CostSnapshotRef == nil || trdapi.MissingForFreeze(q.Revisions[0].Data) != nil {
		t.Fatalf("the quote %+v", q)
	}
	// TSK: one action, the committed second draft with its first attempt. The refused call was refused
	// before its intent was recorded, as every refusal of the gateway is.
	actions, err := w.c.TSK.TaskActions(w.clerk, fzTask)
	w.must(err)
	if len(actions) != 1 || actions[0].State != tskapi.ActionCommitted || actions[0].Attempts != 1 || actions[0].Target.Command != trdapi.CommandSaveDraft {
		t.Fatalf("actions %+v", actions)
	}
	// The ledger keeps the refusal with its code, path and reason, and the committed call its action.
	var tools []executionevidence.Call
	for _, c := range w.ledger(fzTask) {
		if c.Kind == executionevidence.KindTool {
			tools = append(tools, c)
		}
	}
	if len(tools) != 2 || tools[0].State != executionevidence.StateFailed || tools[0].Code != trdapi.CodeCostSnapshotMissing ||
		tools[0].RefusalPath != "/args/data/cost_snapshot_ref" || !strings.Contains(tools[0].RefusalReason, "cannot freeze") || tools[0].ActionID != "" ||
		tools[1].State != executionevidence.StateSucceeded || tools[1].ActionID != actions[0].ActionID {
		t.Fatalf("the ledger's tool calls %+v", tools)
	}
	// The AI layer's audit trail has the refusal under the agent.
	compliance := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "compliance.1"},
		Permissions: templatePermissions(t, "compliance"), DataScope: authorization.DataScopeCompany})
	entries, err := w.c.AI.Audit(compliance)
	w.must(err)
	var audited []string
	for _, e := range entries {
		if e.Outcome == "rejected" && e.Command == trdapi.CommandSaveDraft {
			if e.Actor.ID != agentID {
				t.Errorf("refusal audited for %v", e.Actor)
			}
			audited = append(audited, e.Code)
		}
	}
	if !slices.Equal(audited, []string{trdapi.CodeCostSnapshotMissing}) {
		t.Fatalf("audited %v", audited)
	}
}

// A refused draft is in the next turn's facts, as every refusal is (S4 G3), and what was corrected
// is not there once the draft is saved: the first turn ends after the refusal; the second turn's facts
// list it with the member and the reason, and no rejected action among the actions; the draft sent
// whole is saved; the third turn's facts no longer list it.
func TestTheRefusedDraftIsInTheNextTurnsFacts(t *testing.T) {
	w := newAgentWorld(t, false)
	facts := func(req model.Request) (g3Facts, error) {
		var f g3Facts
		if len(req.Messages) < 2 {
			return f, fmt.Errorf("the request has no facts")
		}
		return f, json.Unmarshal([]byte(req.Messages[1].Content), &f)
	}
	var second, third g3Facts
	var secondRaw string
	turn2 := calls(fzSave(w, "c2", w.data()))
	turn2.Check = func(req model.Request) (err error) {
		secondRaw = req.Messages[1].Content
		second, err = facts(req)
		return err
	}
	turn3 := done("报价草稿已保存。")
	turn3.Check = func(req model.Request) (err error) {
		third, err = facts(req)
		return err
	}
	m := model.NewScripted("scripted-q01-v1", calls(fzSave(w, "c1", fzDraft(w))), turn2, turn3)
	r := w.runner(m)
	w.aiTask(fzTask, tskapi.GoalQuoteReady, tskapi.Target{RfqID: "rfq.1", QuoteID: fzQuote, CustomerID: "customer.1"}, nil, bundleRef(r), fzTools...)

	r.MaxSteps = 1
	res := w.turnOn(r, fzTask)
	if res.ModelCalls != 1 || fmt.Sprint(res.Refusals) != "[quote.save_draft COST_SNAPSHOT_MISSING]" || len(res.Receipts) != 0 || res.Lifecycle != tskapi.StateRunning {
		t.Fatalf("the first turn %+v", res)
	}
	// Nothing was saved or recorded: no quote, no action; the refusal is in the ledger.
	if _, err := w.c.TRD.GetQuote(w.clerk, fzQuote); code(err) != kernel.CodeNotFound {
		t.Fatalf("a quote exists after the refused draft: %v", err)
	}
	if actions, err := w.c.TSK.TaskActions(w.clerk, fzTask); err != nil || len(actions) != 0 {
		t.Fatalf("actions after the refused draft: %+v %v", actions, err)
	}

	res = w.turnOn(r, fzTask)
	if res.ModelCalls != 1 || len(res.Refusals) != 0 || len(res.Receipts) != 1 {
		t.Fatalf("the second turn %+v", res)
	}
	t.Logf("the second turn's facts: %s", secondRaw)
	if len(second.Refusals) != 1 || len(second.Actions) != 0 || strings.Contains(secondRaw, `"rejected"`) {
		t.Fatalf("the second turn's facts: %s", secondRaw)
	}
	n := second.Refusals[0]
	if n.Tool != trdapi.CommandSaveDraft || n.Code != trdapi.CodeCostSnapshotMissing || n.Path != "/args/data/cost_snapshot_ref" ||
		n.Member != "Data.CostSnapshotRef" || !strings.Contains(n.Reason, "cannot freeze") || n.Times != 1 {
		t.Fatalf("the refusal in the facts: %+v", n)
	}
	if q := w.getQuote(fzQuote); q.RootVersion != 1 || q.Revisions[0].Data.CostSnapshotRef == nil {
		t.Fatalf("the quote %+v", q)
	}

	res = w.turnOn(r, fzTask)
	if res.ModelCalls != 1 || !res.Verified {
		t.Fatalf("the third turn %+v", res)
	}
	if len(third.Refusals) != 0 || !third.committed(trdapi.CommandSaveDraft) {
		t.Fatalf("the third turn's facts still carry the corrected refusal: %+v", third)
	}
}

// The gate is the agent's alone. The same data saved by a person is stored as before — on the plain
// route and as a task step, which is the execution entry without the gateway — and its freeze is
// refused as before, with the code and path the gate gives the agent; the contract of QuoteData and
// the freeze are unchanged.
func TestAPersonsPartialDraftIsStoredAndItsFreezeRefusedAsBefore(t *testing.T) {
	w := newAgentWorld(t, false)
	partial := fzDraft(w)
	if err := trdapi.MissingForFreeze(partial); code(err) != trdapi.CodeCostSnapshotMissing || path(err) != "/args/data/cost_snapshot_ref" {
		t.Fatalf("the rule on the draft: %v", err)
	}
	r, err := w.saveQuote(w.clerk, "quote.fz.plain", 0, partial)
	w.must(err)
	if r.RowVersion != 1 || r.Status != trdapi.StatusWorking {
		t.Fatalf("the person's save %+v", r)
	}
	if q := w.getQuote("quote.fz.plain"); q.WorkingRevision != 1 || q.Revisions[0].Data.CostSnapshotRef != nil {
		t.Fatalf("the stored draft %+v", q.Revisions[0].Data.CostSnapshotRef)
	}
	_, err = w.freezeQuote(w.clerk, "quote.fz.plain", 1, 1)
	if code(err) != trdapi.CodeCostSnapshotMissing || path(err) != "/args/data/cost_snapshot_ref" {
		t.Fatalf("the person's freeze: %v", err)
	}
	if q := w.getQuote("quote.fz.plain"); q.Revisions[0].Status != trdapi.StatusWorking || q.RootVersion != 1 {
		t.Fatalf("the refused freeze changed the quote: %+v", q)
	}

	// As a task step: the person's step goes through the execution entry and straight to TRD.
	w.aiTask("task.fz.step", tskapi.GoalQuoteReady, tskapi.Target{RfqID: "rfq.1", QuoteID: "quote.fz.step", CustomerID: "customer.1"}, nil, nil, fzTools...)
	step := execution.Step{TaskID: "task.fz.step", BusinessIntentID: "human.save.fz", Context: tskapi.TargetTRD,
		Command: trdapi.SaveDraft{QuoteID: "quote.fz.step", Data: partial},
		Send: func(ctx context.Context, cmd any) (capability.Result, error) {
			rc, err := w.c.TRD.SaveDraft(ctx, cmd.(trdapi.SaveDraft))
			return capability.Result{Receipt: capability.Receipt(rc), Detail: rc}, err
		}}
	rc, err := w.c.AI.RunStep(w.clerk, step)
	w.must(err)
	if rc.Stage != execution.Verified || rc.Result == nil || rc.Result.Status != trdapi.StatusWorking {
		t.Fatalf("the person's task step %+v", rc)
	}
	if q := w.getQuote("quote.fz.step"); q.WorkingRevision != 1 || q.Revisions[0].Data.CostSnapshotRef != nil {
		t.Fatalf("the stored draft of the task step %+v", q.Revisions[0].Data.CostSnapshotRef)
	}
}
