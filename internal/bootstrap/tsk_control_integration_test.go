//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TSK against the composed TRD on PostgreSQL 16 (WBS S4-01, SM-TSK-TASK, SM-TRD-CONTROL, TSK-INV-002,
// TSK-INV-004; Context Map R17; owner decisions D-2, D-4). TSK binds the root its task works on
// through TRD's object_control.bind and records the control epoch TRD returns; the task's agent gets
// the claim of that epoch and writes the bound root with it; a take-over stops the root at TRD first
// — the old claim is CONTROL_EPOCH_STALE from then on — and the task records TRD's new epoch; a
// cancelled task gives the root back through object_control.release. TRD stays the fence: TSK only
// records what TRD answered. A binding holds while the grants of the task's owner and agent are
// unchanged (S4 D-5).

var tskPermissions = []string{"task.create", "task.scope", "task.wait", "task.verify", "goal.verify", "task.pause", "task.resume",
	"task.take_control", "task.handoff", "task.cancel", "task.finish_stop", "task.close_unfulfilled", "task.fail", "task.bind_object",
	"task.release", "tsk.read", "tsk.audit"}

type tskWorld struct {
	*trdWorld
	sys, clerk, agent context.Context
}

func newTskWorld(t *testing.T, quotes ...string) *tskWorld {
	q := newTrdWorld(t)
	every := append(slices.Clone(allPermissions), tskPermissions...)
	w := &tskWorld{trdWorld: q,
		sys: q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "tsk"},
			Permissions: every, DataScope: "company"}),
		clerk: q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk.1"},
			Permissions: every, DataScope: "company"}),
		agent: q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"},
			Permissions: append(slices.Clone(allPermissions), "tsk.read"), DataScope: "company"}),
	}
	for _, id := range quotes {
		q.drafted(id)
	}
	return w
}

func (w *tskWorld) task(r tskapi.TaskReceipt, err error) tskapi.TaskReceipt {
	w.t.Helper()
	w.must(err)
	return r
}

// bound creates task id for the agent targeting quote, scopes it and binds quote to it through TRD
// (the task's target names the root: SM-TSK-TASK task.bind_object).
func (w *tskWorld) bound(id, quote string) tskapi.TaskReceipt {
	w.t.Helper()
	r := w.task(w.c.TSK.CreateTask(w.clerk, tskapi.CreateTask{IdempotencyKey: w.key(), TaskID: id, GoalCode: tskapi.GoalOrderActive,
		ControlMode: tskapi.ModeAI, AgentID: "agent.1", Target: tskapi.Target{RfqID: "rfq.1", CustomerID: "customer.1", QuoteID: quote}}))
	r = w.task(w.c.TSK.ScopeTask(w.sys, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion}))
	r = w.task(w.c.TSK.ScopeTask(w.clerk, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, Note: "范围确认"}))
	return w.task(w.c.TSK.BindObject(w.sys, tskapi.BindObject{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, QuoteID: quote}))
}

// claimed returns the agent's context carrying the claim TSK issues it for task.
func (w *tskWorld) claimed(task string) (context.Context, tskapi.Claim) {
	w.t.Helper()
	c, err := w.c.TSK.Claim(w.agent, task)
	w.must(err)
	ctx, err := trdapi.WithControl(w.agent, trdapi.ControlClaim{TaskID: c.TaskID, ControlEpoch: c.ControlEpoch})
	w.must(err)
	return ctx, c
}

func (w *tskWorld) label(ctx context.Context, quote string, expected int64) error {
	_, err := w.c.TRD.UpdateMetadata(ctx, trdapi.UpdateMetadata{IdempotencyKey: w.key(), QuoteID: quote, ExpectedVersion: expected,
		Labels: []string{"tsk"}})
	return err
}

// TSK-INV-002, TSK-INV-004 and Q01 V0.1
// tests/test_execution.py::test_takeover_fences_old_agent_and_records_manual_completion (its fence
// half; the manual completion needs TRD's enter_manual, S4 unit A2, and goal.verify, unit B2): the
// binding is TRD's, at the epoch TSK records; the agent writes with the task's claim; after the
// take-over the old claim is CONTROL_EPOCH_STALE and no claim is issued while the task stops.
func TestTSKBindsAndTakesOverATRDRoot(t *testing.T) {
	w := newTskWorld(t, "quote.1", "quote.2", "quote.3")
	r := w.bound("task.1", "quote.1")
	v := w.getQuote("quote.1")
	if v.Control.State != trdapi.ControlBound || v.Control.TaskID != "task.1" || v.Control.ControlEpoch != r.ControlEpoch || r.ControlEpoch != 1 ||
		r.BoundQuoteID != "quote.1" {
		t.Fatalf("TRD %+v, task %+v", v.Control, r)
	}
	claimed, c := w.claimed("task.1")
	if c.ControlEpoch != 1 || c.QuoteID != "quote.1" || c.ControlMode != tskapi.ModeAI {
		t.Fatalf("%+v", c)
	}
	w.want(w.label(w.agent, "quote.1", 1), "OBJECT_CONTROL_CONFLICT") // no claim on a bound root
	w.must(w.label(claimed, "quote.1", 1))

	r = w.task(w.c.TSK.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "模型持续不可用，人工接管"}))
	v = w.getQuote("quote.1")
	if r.Status != tskapi.StateStopping || v.Control.State != trdapi.ControlStopping || v.Control.ControlEpoch != 2 || r.ControlEpoch != 2 {
		t.Fatalf("TRD %+v, task %+v", v.Control, r)
	}
	w.want(w.label(claimed, "quote.1", 2), "CONTROL_EPOCH_STALE")
	_, err := w.c.TSK.Claim(w.agent, "task.1")
	w.want(err, kernel.CodeInvalidState)
	_, err = w.c.TSK.Claim(w.clerk, "task.1")
	w.want(err, kernel.CodeInvalidState)
	// The barrier: TRD's enter_manual with the task's claim; the task is paused under manual control.
	r = w.task(w.c.TSK.FinishStop(w.sys, tskapi.FinishStop{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	v = w.getQuote("quote.1")
	if r.Status != tskapi.StatePaused || r.ControlMode != tskapi.ModeManual || r.ControlEpoch != 3 || v.Control.State != trdapi.ControlManual ||
		v.Control.ControlEpoch != 3 || v.RootVersion != 2 {
		t.Fatalf("TRD %+v, task %+v", v.Control, r)
	}
	_, err = w.c.TSK.Claim(w.agent, "task.1")
	w.want(err, tskapi.CodeControlModeMismatch)
	hc, err := w.c.TSK.Claim(w.clerk, "task.1")
	w.must(err)
	human, err := trdapi.WithControl(w.clerk, trdapi.ControlClaim{TaskID: hc.TaskID, ControlEpoch: hc.ControlEpoch})
	w.must(err)
	w.must(w.label(human, "quote.1", 2))
	w.want(w.label(claimed, "quote.1", 3), "CONTROL_EPOCH_STALE")

	// Another task cannot bind a root bound to task.1: TRD's refusal, passed on.
	o := w.task(w.c.TSK.CreateTask(w.clerk, tskapi.CreateTask{IdempotencyKey: w.key(), TaskID: "task.9", GoalCode: tskapi.GoalQuoteReady,
		ControlMode: tskapi.ModeAI, AgentID: "agent.1", Target: tskapi.Target{QuoteID: "quote.1"}}))
	o = w.task(w.c.TSK.ScopeTask(w.sys, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: "task.9", ExpectedVersion: o.RowVersion}))
	o = w.task(w.c.TSK.ScopeTask(w.sys, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: "task.9", ExpectedVersion: o.RowVersion}))
	_, err = w.c.TSK.BindObject(w.sys, tskapi.BindObject{IdempotencyKey: w.key(), TaskID: "task.9", ExpectedVersion: o.RowVersion, QuoteID: "quote.1"})
	w.want(err, "OBJECT_CONTROL_CONFLICT")

	// Cancel: TRD stops the root, the barrier ends the task without TRD, task.release gives the root
	// back through TRD; then the root takes writes without a claim again and another task binds it at
	// the epoch TRD has reached.
	r2 := w.bound("task.2", "quote.2")
	r2 = w.task(w.c.TSK.CancelTask(w.clerk, tskapi.CancelTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r2.RowVersion,
		Reason: "客户撤回询价"}))
	r2 = w.task(w.c.TSK.FinishStop(w.sys, tskapi.FinishStop{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r2.RowVersion}))
	if r2.Status != tskapi.StateCancelled || w.getQuote("quote.2").Control.State != trdapi.ControlStopping {
		t.Fatalf("%+v", r2)
	}
	_, err = w.c.TSK.ReleaseObject(w.agent, tskapi.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r2.RowVersion,
		Reason: "AI 不能释放"})
	w.want(err, kernel.CodePermissionDenied)
	r2 = w.task(w.c.TSK.ReleaseObject(w.clerk, tskapi.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r2.RowVersion,
		Reason: "任务已取消"}))
	v = w.getQuote("quote.2")
	if r2.Status != tskapi.StateCancelled || r2.BoundQuoteID != "" || r2.ControlEpoch != 3 || v.Control.State != trdapi.ControlUnbound ||
		v.Control.ControlEpoch != 3 {
		t.Fatalf("TRD %+v, task %+v", v.Control, r2)
	}
	w.must(w.label(w.author, "quote.2", 1))
	r3 := w.bound("task.3", "quote.2")
	if r3.ControlEpoch != 4 || w.getQuote("quote.2").Control.TaskID != "task.3" {
		t.Fatalf("%+v", r3)
	}
	tv, err := w.c.TSK.GetTask(w.clerk, "task.2")
	w.must(err)
	var changes []string
	for _, h := range tv.History {
		if h.ControlChange != "" {
			changes = append(changes, h.ControlChange)
		}
	}
	if !slices.Equal(changes, []string{"bind", "cancel", "finish_stop", "release"}) || tv.Binding.State != tskapi.BindingReleased {
		t.Fatalf("%v %+v", changes, tv.Binding)
	}
	// TRD recorded the stops and releases as the humans who asked, with TSK's reason.
	var trdAudit []string
	entries, err := w.c.TRD.Audit(w.author)
	w.must(err)
	for _, e := range entries {
		if e.ObjectID == "quote.2" && e.Outcome == "succeeded" && (e.Command == trdapi.CommandStopControl || e.Command == trdapi.CommandReleaseControl) {
			trdAudit = append(trdAudit, e.Command+" "+e.Actor.ID+" "+e.Reason)
		}
	}
	if !slices.Equal(trdAudit, []string{"object_control.stop clerk.1 TSK task task.2 at control epoch 1",
		"object_control.release clerk.1 TSK task task.2 at control epoch 2"}) {
		t.Fatalf("%v", trdAudit)
	}
}

// BC-AT20 (TSK half) and CT10: the agent's claimed write and a take-over meet on the root. TRD's root
// lock orders them: a write that commits before TRD's stop is kept; one after it is
// CONTROL_EPOCH_STALE; either way the take-over commits, TRD's root is stopping and the task records
// TRD's epoch.
func TestTakeOverAndAClaimedWriteDecideByCommitOrder(t *testing.T) {
	quotes := []string{"quote.r0", "quote.r1", "quote.r2", "quote.r3", "quote.r4"}
	w := newTskWorld(t, quotes...)
	outcomes := map[string]int{}
	for i, quote := range quotes {
		task := "task.r" + string(rune('0'+i))
		r := w.bound(task, quote)
		claimed, _ := w.claimed(task)
		var wg sync.WaitGroup
		var writeErr, takeErr error
		var took tskapi.TaskReceipt
		wg.Add(2)
		go func() {
			defer wg.Done()
			// Later starts in later rounds, so both commit orders occur; the assertions hold for either.
			time.Sleep(time.Duration(i) * 15 * time.Millisecond)
			writeErr = w.label(claimed, quote, 1)
		}()
		go func() {
			defer wg.Done()
			took, takeErr = w.c.TSK.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: "take." + task, TaskID: task, ExpectedVersion: r.RowVersion,
				Reason: "接管"})
		}()
		wg.Wait()
		w.must(takeErr)
		v := w.getQuote(quote)
		switch code(writeErr) {
		case "OK":
			if v.RootVersion != 2 {
				t.Fatalf("a kept write left root version %d", v.RootVersion)
			}
		case "CONTROL_EPOCH_STALE", "OBJECT_CONTROL_CONFLICT":
			if v.RootVersion != 1 {
				t.Fatalf("a refused write changed the root: %+v", v)
			}
		default:
			t.Fatalf("%v", writeErr)
		}
		outcomes[code(writeErr)]++
		if v.Control.State != trdapi.ControlStopping || v.Control.ControlEpoch != took.ControlEpoch || took.Status != tskapi.StateStopping {
			t.Fatalf("TRD %+v, task %+v", v.Control, took)
		}
		w.want(w.label(claimed, quote, v.RootVersion), "CONTROL_EPOCH_STALE")
	}
	t.Logf("claimed writes against take-overs: %v", outcomes)
}
