//go:build integration

package tsk_test

import (
	"errors"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/tsk"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The lost bind record (S4 C3; C1's gap 1; SM-TSK-TASK task.bind_object, task.take_control,
// task.cancel, task.release; TSK-INV-002; MD-31). TRD's object_control.bind commits and its answer is
// lost, so task.bind_object records nothing; the task then leaves running before the bind is sent
// again. The control changes that act on the root learn the bind from TRD's ledger (the fake answers
// Bound from the binds it committed, as TRD's reconciliation does) and proceed exactly as on the
// normal path: the same keys, so a retry of a change whose answer was lost replays TRD's result. A task
// TRD never bound asks once and calls TRD for nothing; a task without an AI agent asks nothing.
func TestALostBindIsLearnedByTheControlChanges(t *testing.T) {
	w := newWorld(t)
	lost := errors.New("connection lost after TRD's bind")
	lostBind := func(id, quote string) api.TaskReceipt {
		t.Helper()
		r := w.running(id, func(c *api.CreateTask) { c.Target.QuoteID = quote })
		w.trd.lose["bind"] = lost
		if _, err := w.m.BindObject(w.sys, api.BindObject{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, QuoteID: quote}); !errors.Is(err, lost) {
			t.Fatalf("the bind's answer: %v", err)
		}
		if v := w.get(id); v.Binding != nil || v.ControlEpoch != 0 || v.TaskVersion != r.RowVersion || w.trd.epoch[quote] != 1 {
			t.Fatalf("after the lost bind: %+v, TRD epoch %d", v, w.trd.epoch[quote])
		}
		return r
	}

	// Paused, then taken over: the take-over learns the bind (epoch 1), stops the root at epoch 2 with
	// the key a recorded bind would have given it, and the barrier enters manual control; the release
	// gives the root back. The history shows no bind: the task records what TRD returned for its
	// changes.
	r := lostBind("task.1", "quote.1")
	r = w.must(w.m.PauseTask(w.alice, api.PauseTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Cause: api.PauseOperator, Reason: "暂停核对", ResumeCondition: "核对完成"}))
	asked := w.trd.asked
	r = w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "人工接管"}))
	if r.Status != api.StateStopping || r.ControlEpoch != 2 || r.BoundQuoteID != "quote.1" || w.trd.asked != asked+1 {
		t.Fatalf("take-over %+v, asked %d", r, w.trd.asked-asked)
	}
	stop := w.trd.calls[len(w.trd.calls)-1]
	if stop.Op != "stop" || stop.Epoch != 1 || stop.Key != tsk.ControlKey("task.1", "object_control.stop", 1) {
		t.Fatalf("stop %+v", stop)
	}
	r = w.must(w.m.FinishStop(w.sys, api.FinishStop{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	if r.Status != api.StatePaused || r.ControlMode != api.ModeManual || r.ControlEpoch != 3 {
		t.Fatalf("barrier %+v", r)
	}
	if c, err := w.m.Claim(w.alice, "task.1"); err != nil || c.QuoteID != "quote.1" || c.ControlEpoch != 3 {
		t.Fatalf("the manual claim names the root: %+v %v", c, err)
	}
	r = w.must(w.m.ReleaseObject(w.alice, api.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "交还"}))
	v := w.get("task.1")
	if r.ControlEpoch != 4 || v.Binding.State != api.BindingReleased || !slices.Equal(changes(v), []string{"take_control", "finish_stop", "release"}) {
		t.Fatalf("release %+v, binding %+v, history %v", r, v.Binding, changes(v))
	}
	if got := w.trd.ops(); !slices.Equal(got, []string{"bind", "stop", "enter_manual", "release"}) {
		t.Fatalf("TRD %v", got)
	}

	// Closed before the bind was sent again: the release learns it and gives the root back at once.
	w.trd.calls = nil
	c := lostBind("task.2", "quote.2")
	c = w.must(w.m.CloseUnfulfilled(w.sys, api.CloseUnfulfilled{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: c.RowVersion,
		Cause: api.CloseNotPursued, Reason: "不再跟进"}))
	c = w.must(w.m.ReleaseObject(w.alice, api.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: c.RowVersion, Reason: "任务已关闭"}))
	if c.Status != api.StateClosedUnfulfilled || c.ControlEpoch != 2 || w.get("task.2").Binding.State != api.BindingReleased ||
		!slices.Equal(w.trd.ops(), []string{"bind", "release"}) || w.trd.calls[1].Key != tsk.ControlKey("task.2", "object_control.release", 1) {
		t.Fatalf("release of a closed task %+v, TRD %v", c, w.trd.calls)
	}

	// Cancelled: the cancellation learns it and stops the root; the barrier ends the task without TRD
	// and the release gives the root back.
	w.trd.calls = nil
	x := lostBind("task.3", "quote.3")
	x = w.must(w.m.CancelTask(w.alice, api.CancelTask{IdempotencyKey: w.key(), TaskID: "task.3", ExpectedVersion: x.RowVersion, Reason: "客户撤回"}))
	x = w.must(w.m.FinishStop(w.sys, api.FinishStop{IdempotencyKey: w.key(), TaskID: "task.3", ExpectedVersion: x.RowVersion}))
	x = w.must(w.m.ReleaseObject(w.alice, api.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.3", ExpectedVersion: x.RowVersion, Reason: "交还"}))
	if x.Status != api.StateCancelled || x.ControlEpoch != 3 || !slices.Equal(w.trd.ops(), []string{"bind", "stop", "release"}) {
		t.Fatalf("cancelled %+v, TRD %v", x, w.trd.ops())
	}

	// The learning take-over loses TRD's answer too: sent again, it learns the same bind and sends the
	// same stop, which TRD replays.
	w.trd.calls = nil
	y := lostBind("task.4", "quote.4")
	w.trd.lose["stop"] = errors.New("connection lost after TRD's stop")
	if _, err := w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: "task.4", ExpectedVersion: y.RowVersion, Reason: "接管"}); err == nil {
		t.Fatal("the lost stop")
	}
	if v := w.get("task.4"); v.Binding != nil || v.Lifecycle != api.StateRunning {
		t.Fatalf("nothing recorded: %+v", v)
	}
	y = w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: "task.4", ExpectedVersion: y.RowVersion, Reason: "接管"}))
	if y.ControlEpoch != 2 || !slices.Equal(w.trd.ops(), []string{"bind", "stop", "stop(replay)"}) {
		t.Fatalf("retry %+v, TRD %v", y, w.trd.ops())
	}

	// TRD never bound the task: the take-over asks once and calls TRD for nothing. A refused question
	// refuses the change, which then changes nothing.
	w.trd.calls = nil
	z := w.running("task.5", func(c *api.CreateTask) { c.Target.QuoteID = "quote.5" })
	w.trd.fail["bound"] = kernel.Reject(kernel.CodePermissionDenied, "/", "")
	_, err := w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: "task.5", ExpectedVersion: z.RowVersion, Reason: "接管"})
	w.want(err, kernel.CodePermissionDenied)
	delete(w.trd.fail, "bound")
	asked = w.trd.asked
	z = w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: "task.5", ExpectedVersion: z.RowVersion, Reason: "接管"}))
	if z.ControlEpoch != 0 || z.BoundQuoteID != "" || w.trd.asked != asked+1 || len(w.trd.ops()) != 0 {
		t.Fatalf("unbound take-over %+v, TRD %v", z, w.trd.ops())
	}

	// A manual task without an AI agent never bound a root: its cancellation asks nothing.
	m := w.running("task.6", func(c *api.CreateTask) { c.ControlMode, c.AgentID, c.Target.QuoteID = api.ModeManual, "", "quote.6" })
	asked = w.trd.asked
	w.must(w.m.CancelTask(w.alice, api.CancelTask{IdempotencyKey: w.key(), TaskID: "task.6", ExpectedVersion: m.RowVersion, Reason: "取消"}))
	if w.trd.asked != asked || len(w.trd.ops()) != 0 {
		t.Fatalf("asked %d, TRD %v", w.trd.asked-asked, w.trd.ops())
	}
}

func changes(v api.TaskView) []string {
	var out []string
	for _, h := range v.History {
		if h.ControlChange != "" {
			out = append(out, h.ControlChange)
		}
	}
	return out
}
