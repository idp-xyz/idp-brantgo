//go:build integration && eerlive

package bootstrap_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/adapters/eer"
	"go.idp.xyz/idp-brant/internal/ai/adapters/eer/eerlive"
	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/execution"
	"go.idp.xyz/idp-brant/internal/ai/runtime"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
)

// The stop barrier of a Brant task against the live EER on PostgreSQL 16 (WBS S4-05; decision E-2;
// IG-10 停止屏障 and 恢复): a task's operation is submitted to EER and waits there for approval; a
// person takes the task over in TSK; the agent runner — with the EER adapter as its runtime — asks
// EER to cancel the execution and keeps the task stopping until EER shows it Completed with outcome
// cancelled, then finishes the stop (task paused, manual). EER refuses the late approval. A fresh
// process (new client, new runner) finds the operation by its stable ID and the barrier settled,
// and submitting it again is EER's replay of the same execution. Built only with -tags
// 'integration eerlive' (make eer-live), never under CI=true.
func TestEERLiveStopBarrierOfATask(t *testing.T) {
	w := newAgentWorld(t, false)
	ctx := context.Background()
	c := eerlive.Client(t, nil)
	m := model.NewScripted("scripted-q01-v1")
	r := w.runnerOn(m, c)
	task := fmt.Sprintf("task.eer.%d", time.Now().UnixNano())
	sub := eerlive.Submission(runtime.OperationOf(w.scope("company.A"), task), task, true, time.Now())

	rec := w.aiTask(task, tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1"}, budgetOf(10, 2), bundleRef(r), "source.register")
	first, err := c.Submit(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Whatever happens, no execution of this run is left waiting at EER.
		for range 40 {
			if st, err := runtime.Barrier(ctx, c, sub.OperationID, "IDP Brant S4 D2 live run ends"); err == nil && st.Settled {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()
	waiting := eerlive.Await(ctx, t, c, sub.OperationID, 30*time.Second, func(o runtime.Operation) bool { return o.State == runtime.WaitingApproval })
	t.Logf("task %s: EER %s submitted (replayed=%t), waits: %s %s", task, first.RuntimeRef, first.Replayed, waiting.RuntimeState, waiting.LastTrigger())

	_, err = w.c.TSK.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: task, ExpectedVersion: rec.RowVersion,
		Reason: "人工接管：EER 仍持有任务的执行"})
	w.must(err)
	t.Logf("take-over: task %s", w.taskView(task).Lifecycle)

	passes, stopped := 0, false
	var log []string
	deadline := time.Now().Add(45 * time.Second)
	for !stopped {
		if time.Now().After(deadline) {
			t.Fatalf("the barrier did not settle: %v", log)
		}
		res := w.turnOn(r, task)
		passes++
		v := w.taskView(task)
		log = append(log, fmt.Sprintf("pass %d: stopped=%t skipped=%q task=%s/%s", passes, res.Stopped, res.Skipped, v.Lifecycle, v.ControlMode))
		if res.ModelCalls != 0 {
			t.Fatalf("a model call during the stop: %+v", res)
		}
		if !res.Stopped && v.Lifecycle != tskapi.StateStopping {
			t.Fatalf("the task left stopping without the barrier: %v", log)
		}
		stopped = res.Stopped
		if !stopped {
			time.Sleep(250 * time.Millisecond)
		}
	}
	for _, l := range log {
		t.Log(l)
	}
	if passes < 2 || !strings.Contains(log[0], "waits for the runtime") {
		t.Fatalf("the first pass should have waited for EER: %v", log)
	}
	v := w.taskView(task)
	if v.Lifecycle != tskapi.StatePaused || v.ControlMode != tskapi.ModeManual {
		t.Fatalf("task %s %s", v.Lifecycle, v.ControlMode)
	}
	ended, err := c.Lookup(ctx, sub.OperationID)
	if err != nil || ended.State != runtime.Cancelled || ended.Outcome != eer.OutcomeCancelled {
		t.Fatalf("EER execution %+v %v", ended, err)
	}
	reasonSeen := false
	for _, tr := range ended.Transitions {
		reasonSeen = reasonSeen || (tr.Trigger == "cancellation_completed" && tr.Reason == execution.RuntimeStopReason)
	}
	actions := []string{}
	for _, a := range ended.Audit {
		actions = append(actions, a.Action+"@"+a.ActorID)
	}
	if !reasonSeen {
		t.Fatalf("EER's transitions do not carry Brant's stop reason: %+v", ended.Transitions)
	}
	t.Logf("EER %s: state=%s outcome=%s last=%s audit=%v", ended.RuntimeRef, ended.RuntimeState, ended.Outcome, ended.LastTrigger(), actions)
	if _, err = c.Decide(ctx, sub.OperationID, true); !errors.Is(err, runtime.ErrNotWaiting) {
		t.Fatalf("a late approval at EER: %v", err)
	}

	// A fresh process: new EER client, new runner. It finds the operation by its stable ID, ended;
	// the identical submission is EER's replay; the paused manual task is not touched.
	fresh := eerlive.Client(t, nil)
	again, err := fresh.Submit(ctx, sub)
	if err != nil || !again.Replayed || again.State != runtime.Cancelled {
		t.Fatalf("resubmission from a fresh process %+v %v", again, err)
	}
	st, err := runtime.Barrier(ctx, fresh, runtime.OperationOf(w.scope("company.A"), task), execution.RuntimeStopReason)
	if err != nil || !st.Settled || st.CancelRequested {
		t.Fatalf("fresh barrier %+v %v", st, err)
	}
	r2 := w.runnerOn(model.NewScripted("scripted-q01-v1"), fresh)
	if res := w.turnOn(r2, task); res.Stopped || res.ModelCalls != 0 {
		t.Fatalf("the fresh runner acted on the manual task: %+v", res)
	}
	t.Logf("fresh process: replayed=%t state=%s; barrier settled without a new cancellation; manual task left alone", again.Replayed, again.State)
}
