//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai"
	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/execution"
	"go.idp.xyz/idp-brant/internal/ai/runtime"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The runtime half of the stop barrier on PostgreSQL 16 (WBS S4-05; IG-10 停止屏障; SM-TSK-TASK
// task.finish_stop; baseline 8.5, 17.2): when an external runtime (EER) holds an operation of a task
// that a person takes over, the agent runner finishes the stop only once the runtime shows that
// operation ended — it asks the runtime to cancel it, and the task stays stopping until then. The
// runtime here is scripted (CI); the same flow against the live EER is TestEERLiveStopBarrierOfATask
// (make eer-live).

// scriptedRuntime is an in-memory runtime.Port: a cancellation ends an operation `lag` lookups
// later (EER takes it as a signal); unavailable makes every call fail.
type scriptedRuntime struct {
	mu          sync.Mutex
	ops         map[string]*runtime.Operation
	pending     map[string]int
	lag         int
	cancels     []string
	unavailable bool
}

func newScriptedRuntime(lag int) *scriptedRuntime {
	return &scriptedRuntime{ops: map[string]*runtime.Operation{}, pending: map[string]int{}, lag: lag}
}

func (s *scriptedRuntime) Submit(_ context.Context, sub runtime.Submission) (runtime.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := sub.Validate(); err != nil {
		return runtime.Operation{}, err
	}
	if op, ok := s.ops[sub.OperationID]; ok {
		out := *op
		out.Replayed = true
		return out, nil
	}
	op := &runtime.Operation{OperationID: sub.OperationID, State: runtime.WaitingApproval, RuntimeState: "Waiting"}
	s.ops[sub.OperationID] = op
	return *op, nil
}

func (s *scriptedRuntime) Lookup(_ context.Context, id string) (runtime.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return runtime.Operation{}, runtime.ErrUnavailable
	}
	op, ok := s.ops[id]
	if !ok {
		return runtime.Operation{OperationID: id, State: runtime.NotFound}, nil
	}
	if n, ok := s.pending[id]; ok {
		if n > 0 {
			s.pending[id] = n - 1
		} else {
			delete(s.pending, id)
			op.State, op.Outcome, op.RuntimeState = runtime.Cancelled, "cancelled", "Completed"
		}
	}
	return *op, nil
}

func (s *scriptedRuntime) Cancel(ctx context.Context, id, reason string) (runtime.Operation, error) {
	s.mu.Lock()
	if s.unavailable {
		s.mu.Unlock()
		return runtime.Operation{}, runtime.ErrUnavailable
	}
	s.cancels = append(s.cancels, id+" "+reason)
	if op, ok := s.ops[id]; ok && !op.State.Ended() {
		if _, ok := s.pending[id]; !ok {
			s.pending[id] = s.lag
		}
	}
	s.mu.Unlock()
	return s.Lookup(ctx, id)
}

func (s *scriptedRuntime) Decide(context.Context, string, bool) (runtime.Operation, error) {
	return runtime.Operation{}, runtime.ErrNotWaiting
}

func (s *scriptedRuntime) cancelled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cancels...)
}

// runnerOn assembles the agent runner of the world's company with model m and runtime rt.
func (w *agentWorld) runnerOn(m model.Port, rt runtime.Port) *execution.Runner {
	w.t.Helper()
	r, err := w.c.AI.Runner(ai.RunnerConfig{Scopes: []kernel.Scope{w.scope("company.A")}, Agents: []string{agentID}, Model: m,
		Profile: execution.ScriptedProfile("scripted-q01-v1"), Budget: execution.Q01DefaultBudget(), Sources: w.c.EVD, Runtime: rt})
	w.must(err)
	w.must(r.Init(context.Background()))
	return r
}

// takenOver creates an AI task as the clerk, submits its operation to rt (when rt is set) and has
// the clerk take it over before the runner touches it: the task is stopping.
func (w *agentWorld) takenOver(r *execution.Runner, id string, rt runtime.Port) string {
	w.t.Helper()
	rec := w.aiTask(id, tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1"}, budgetOf(10, 2), bundleRef(r), evdapi.CommandRegisterSource)
	op := runtime.OperationOf(w.scope("company.A"), id)
	if rt != nil {
		now := time.Now().UTC()
		if _, err := rt.Submit(context.Background(), runtime.Submission{OperationID: op, TaskID: id, Objective: "Q01 quote ready", Plan: "brant-q01",
			ApprovalRequired: true, RequestedAt: now, Deadline: now.Add(time.Hour)}); err != nil {
			w.t.Fatal(err)
		}
	}
	_, err := w.c.TSK.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: rec.RowVersion,
		Reason: "人工接管：外部运行时仍持有任务的执行"})
	w.must(err)
	if v := w.taskView(id); v.Lifecycle != tskapi.StateStopping {
		w.t.Fatalf("task %s is %s, not stopping", id, v.Lifecycle)
	}
	return op
}

// IG-10 stop barrier (runtime half), TSK-INV-002/003: the take-over of a task whose operation the
// runtime still holds asks the runtime to cancel it, and the task stays stopping while the runtime
// shows the operation running — no task.finish_stop, no model call; once the runtime shows it ended
// (cancelled), the next pass finishes the stop and the task is paused in manual mode. A task the
// runtime never had stops at once. A runtime that cannot be read keeps the barrier closed (the
// pass reports the error) until it answers again; an unconnected runtime never settles a barrier
// (never a fake success).
func TestStopBarrierWaitsForTheRuntimesOperation(t *testing.T) {
	w := newAgentWorld(t, false)
	rt := newScriptedRuntime(1)
	m := model.NewScripted("scripted-q01-v1")
	r := w.runnerOn(m, rt)
	ctx := context.Background()

	op := w.takenOver(r, "task.rt.1", rt)
	res := w.turnOn(r, "task.rt.1")
	v := w.taskView("task.rt.1")
	if res.Stopped || !strings.Contains(res.Skipped, "waits for the runtime") || res.ModelCalls != 0 || v.Lifecycle != tskapi.StateStopping ||
		len(rt.cancelled()) != 1 || !strings.HasPrefix(rt.cancelled()[0], op+" "+execution.RuntimeStopReason) {
		t.Fatalf("first pass %+v, task %s, cancels %v", res, v.Lifecycle, rt.cancelled())
	}
	res = w.turnOn(r, "task.rt.1")
	v = w.taskView("task.rt.1")
	if !res.Stopped || v.Lifecycle != tskapi.StatePaused || v.ControlMode != tskapi.ModeManual || m.Remaining() != 0 {
		t.Fatalf("second pass %+v, task %s %s", res, v.Lifecycle, v.ControlMode)
	}
	if got, _ := rt.Lookup(ctx, op); got.State != runtime.Cancelled {
		t.Fatalf("runtime operation %+v", got)
	}

	// The runtime never had the task's operation: the barrier settles at once.
	w.takenOver(r, "task.rt.2", nil)
	if res = w.turnOn(r, "task.rt.2"); !res.Stopped {
		t.Fatalf("no runtime operation: %+v", res)
	}

	// The runtime cannot be read: the barrier stays closed and the pass says why; then it answers.
	w.takenOver(r, "task.rt.3", rt)
	rt.mu.Lock()
	rt.unavailable = true
	rt.mu.Unlock()
	res, err := r.Turn(ctx, w.scope("company.A"), agentID, "task.rt.3")
	if !errors.Is(err, runtime.ErrUnavailable) || res.Stopped || w.taskView("task.rt.3").Lifecycle != tskapi.StateStopping {
		t.Fatalf("unreadable runtime: %+v %v", res, err)
	}
	rt.mu.Lock()
	rt.unavailable = false
	rt.mu.Unlock()
	for i := 0; i < 3 && !res.Stopped; i++ {
		res = w.turnOn(r, "task.rt.3")
	}
	if !res.Stopped || w.taskView("task.rt.3").ControlMode != tskapi.ModeManual {
		t.Fatalf("after the outage: %+v", res)
	}

	// An unconnected runtime refuses: the barrier never settles on a fake success.
	ru := w.runnerOn(model.NewScripted("scripted-q01-v1"), runtime.Unconnected{})
	w.takenOver(ru, "task.rt.4", nil)
	res, err = ru.Turn(ctx, w.scope("company.A"), agentID, "task.rt.4")
	if !errors.Is(err, runtime.ErrNotConnected) || res.Stopped || w.taskView("task.rt.4").Lifecycle != tskapi.StateStopping {
		t.Fatalf("unconnected runtime: %+v %v", res, err)
	}
}
