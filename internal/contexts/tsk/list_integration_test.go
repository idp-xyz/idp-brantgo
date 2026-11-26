//go:build integration

package tsk_test

import (
	"context"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// task.list (S7-06 read models): a page of the tasks the caller sees, most recently changed first.
// A task is seen as task.get decides it: the owner and, by data scope, the owner's team, the
// initiator and the task's AI agent. Filters narrow and pages do not overlap.
func TestTaskListFollowsTheTaskVisibility(t *testing.T) {
	w := newWorld(t)
	w.create("task.ai", func(c *api.CreateTask) {
		c.Target.RfqID, c.Target.QuoteID, c.Source = "rfq.1", "quote.1", &api.SourceRef{Kind: api.SourceRFQ, ID: "rfq.1"}
	})
	w.create("task.manual", func(c *api.CreateTask) {
		c.ControlMode, c.AgentID, c.Budget = api.ModeManual, "", nil
		c.Target.RfqID, c.Source = "rfq.2", &api.SourceRef{Kind: api.SourceRFQ, ID: "rfq.2"}
	})
	w.create("task.other.agent", func(c *api.CreateTask) {
		c.AgentID = "agent.2"
		c.Target.RfqID, c.Source = "rfq.3", &api.SourceRef{Kind: api.SourceRFQ, ID: "rfq.3"}
	})

	list := func(ctx context.Context, q api.TaskQuery) api.TaskPage {
		t.Helper()
		p, err := w.m.ListTasks(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	ids := func(p api.TaskPage) []string {
		out := []string{}
		for _, r := range p.Tasks {
			out = append(out, r.TaskID)
		}
		return out
	}

	all := list(w.alice, api.TaskQuery{})
	if got := ids(all); !slices.Equal(got, []string{"task.other.agent", "task.manual", "task.ai"}) || all.Next != "" {
		t.Fatalf("the owner sees %v next %q", got, all.Next)
	}
	if r := all.Tasks[2]; r.Lifecycle != api.StateCreated || r.ControlMode != api.ModeAI || r.TaskVersion != 1 || r.OwnerID != "alice" ||
		r.AgentID != "agent.1" || r.TargetRfqID != "rfq.1" || r.TargetQuoteID != "quote.1" || r.BoundQuoteID != "" || r.UpdatedAt.IsZero() {
		t.Fatalf("summary %+v", r)
	}
	// bob has tsk.read in the own scope and owns nothing; the agent sees the task it runs; the
	// other company's alice sees none.
	if got := ids(list(w.bob, api.TaskQuery{})); len(got) != 0 {
		t.Fatalf("bob sees %v", got)
	}
	if got := ids(list(w.agent, api.TaskQuery{})); !slices.Equal(got, []string{"task.ai"}) {
		t.Fatalf("the agent sees %v", got)
	}
	if got := ids(list(w.other, api.TaskQuery{})); len(got) != 0 {
		t.Fatalf("the other company sees %v", got)
	}

	// Filters.
	if got := ids(list(w.alice, api.TaskQuery{ControlMode: api.ModeManual})); !slices.Equal(got, []string{"task.manual"}) {
		t.Fatalf("control mode %v", got)
	}
	if got := ids(list(w.alice, api.TaskQuery{RfqID: "rfq.3"})); !slices.Equal(got, []string{"task.other.agent"}) {
		t.Fatalf("rfq %v", got)
	}
	if got := ids(list(w.alice, api.TaskQuery{QuoteID: "quote.1", Lifecycle: api.StateCreated})); !slices.Equal(got, []string{"task.ai"}) {
		t.Fatalf("quote %v", got)
	}
	if got := ids(list(w.alice, api.TaskQuery{Lifecycle: api.StateRunning})); len(got) != 0 {
		t.Fatalf("running %v", got)
	}

	// A write moves the task to the front; pages of two cover the list once.
	w.must(w.m.ScopeTask(w.sys, api.ScopeTask{IdempotencyKey: w.key(), TaskID: "task.ai", ExpectedVersion: 1}))
	first := list(w.alice, api.TaskQuery{Limit: 2})
	rest := list(w.alice, api.TaskQuery{Limit: 2, After: first.Next})
	if got := append(ids(first), ids(rest)...); first.Next == "" || rest.Next != "" ||
		!slices.Equal(got, []string{"task.ai", "task.other.agent", "task.manual"}) {
		t.Fatalf("pages %v, next %q then %q", got, first.Next, rest.Next)
	}
	if first.Tasks[0].Lifecycle != api.StateScoping {
		t.Fatalf("scoped %+v", first.Tasks[0])
	}

	for _, bad := range []api.TaskQuery{{Lifecycle: "open"}, {ControlMode: "auto"}, {After: "not-a-cursor"}, {Limit: kernel.MaxListPage + 1}} {
		if _, err := w.m.ListTasks(w.alice, bad); code(err) != kernel.CodeSchemaInvalid {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
}
