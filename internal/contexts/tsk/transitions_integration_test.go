//go:build integration

package tsk_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/tsk"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// path is what a transition needs of its task before it runs: the mode, a bound root, the state
// before a pause or a stop, and the purpose of a stop.
type path struct {
	manual     bool
	bound      bool
	pausedFrom string // paused: the state paused in (running or a waiting state)
	purpose    string // stopping: take_control or cancel
}

// pathFor returns what the transition from → to by command needs.
func pathFor(tr task.Transition) path {
	p := path{pausedFrom: api.StateRunning, purpose: api.StopTakeControl}
	switch tr.Command {
	case task.CmdHandoff:
		p.manual = true
	case task.CmdRelease:
		p.bound = true
		p.manual = tr.From == task.Paused
	}
	if tr.From == task.Paused && tr.Command == task.CmdResume {
		p.pausedFrom = string(tr.To)
	}
	if tr.From == task.Stopping && tr.To == task.Cancelled {
		p.purpose = api.StopCancel
	}
	return p
}

// reach drives a new task through registered commands to state along p and returns its version.
func (w *world) reach(id string, state task.State, p path) int64 {
	w.t.Helper()
	var target []func(*api.CreateTask)
	if state == task.Verifying || state == task.Completed {
		// goal.verify judges the task's quote root; a task without one is not met (Q01.quote, unit B2).
		target = append(target, func(c *api.CreateTask) { c.Target.QuoteID = "quote." + id })
	}
	v := w.create(id, target...).RowVersion
	if state == task.Created {
		return v
	}
	v = w.must(w.m.ScopeTask(w.sys, api.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v})).RowVersion
	if state == task.Scoping {
		return v
	}
	v = w.must(w.m.ScopeTask(w.sys, api.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v})).RowVersion
	if p.bound {
		v = w.must(w.m.BindObject(w.sys, api.BindObject{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v, QuoteID: "quote." + id})).RowVersion
	}
	if p.manual {
		v = w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v, Reason: "接管"})).RowVersion
		v = w.must(w.m.FinishStop(w.sys, api.FinishStop{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v})).RowVersion
		if state == task.Paused && p.pausedFrom == api.StateRunning {
			return v
		}
		v = w.must(w.m.ResumeTask(w.alice, api.ResumeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v, Reason: "人工继续"})).RowVersion
	}
	wait := func(v int64, kind string) int64 {
		return w.must(w.m.WaitTask(w.sys, api.WaitTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v, Wait: kind, Reason: "等待",
			ResponsibleID: "alice", Items: []string{"数量"}, Awaiting: &api.ObjectRef{Kind: "customer_response", ID: "quote.1"},
			DueAt: kernel.FormatTimestamp(time.Now().Add(time.Hour))})).RowVersion
	}
	switch state {
	case task.Running:
	case task.WaitingInput, task.WaitingApproval, task.WaitingExternal:
		v = wait(v, strings.TrimPrefix(string(state), "waiting_"))
	case task.Verifying, task.Completed:
		v = w.must(w.m.VerifyTask(w.sys, api.VerifyTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v})).RowVersion
		if state == task.Completed {
			w.trd.assess = func(tsk.Goal) (string, []string) { return "satisfied", nil }
			v = w.must(w.m.VerifyGoal(w.sys, api.VerifyGoal{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v})).RowVersion
		}
	case task.Paused:
		if p.pausedFrom != api.StateRunning {
			v = wait(v, strings.TrimPrefix(p.pausedFrom, "waiting_"))
		}
		v = w.must(w.m.PauseTask(w.sys, api.PauseTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v, Cause: api.PauseConflict,
			Reason: "冲突", ResumeCondition: "冲突解决"})).RowVersion
	case task.Stopping, task.Cancelled:
		if p.purpose == api.StopCancel || state == task.Cancelled {
			v = w.must(w.m.CancelTask(w.alice, api.CancelTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v, Reason: "撤销"})).RowVersion
		} else {
			v = w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v, Reason: "接管"})).RowVersion
		}
		if state == task.Cancelled {
			v = w.must(w.m.FinishStop(w.sys, api.FinishStop{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v})).RowVersion
		}
	case task.ClosedUnfulfilled:
		v = w.must(w.m.CloseUnfulfilled(w.sys, api.CloseUnfulfilled{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v,
			Cause: api.CloseQuoteExpired, Reason: "到期"})).RowVersion
	case task.Failed:
		v = w.must(w.m.FailTask(w.alice, api.FailTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v, Reason: "失败",
			ResponsibleID: "alice"})).RowVersion
	default:
		w.t.Fatalf("cannot reach %s", state)
	}
	return v
}

// apply runs the transition's command on task id at version v as ctx.
func (w *world) apply(ctx context.Context, id string, v int64, tr task.Transition) (api.TaskReceipt, error) {
	k := w.key()
	switch tr.Command {
	case task.CmdScope:
		return w.m.ScopeTask(ctx, api.ScopeTask{IdempotencyKey: k, TaskID: id, ExpectedVersion: v})
	case task.CmdWait:
		return w.m.WaitTask(ctx, api.WaitTask{IdempotencyKey: k, TaskID: id, ExpectedVersion: v, Wait: strings.TrimPrefix(string(tr.To), "waiting_"),
			Reason: "等待", ResponsibleID: "alice", Items: []string{"数量"}, Awaiting: &api.ObjectRef{Kind: "quote_review", ID: "rev.1", Version: 1},
			DueAt: kernel.FormatTimestamp(time.Now().Add(time.Hour))})
	case task.CmdResume:
		r := api.ResumeTask{IdempotencyKey: k, TaskID: id, ExpectedVersion: v, Reason: "已处理"}
		if tr.From != task.Paused {
			r.Reason, r.Arrival = "", &api.ObjectRef{Kind: "acceptance", ID: "acc.1", Version: 1}
		}
		return w.m.ResumeTask(ctx, r)
	case task.CmdVerify:
		return w.m.VerifyTask(ctx, api.VerifyTask{IdempotencyKey: k, TaskID: id, ExpectedVersion: v})
	case task.CmdVerifyGoal:
		outcome := map[task.State]string{task.Completed: "satisfied", task.WaitingInput: "unsatisfied", task.WaitingExternal: "unknown"}[tr.To]
		w.trd.assess = func(tsk.Goal) (string, []string) {
			if outcome == "satisfied" {
				return outcome, nil
			}
			return outcome, []string{"Q01.order"}
		}
		return w.m.VerifyGoal(ctx, api.VerifyGoal{IdempotencyKey: k, TaskID: id, ExpectedVersion: v})
	case task.CmdPause:
		return w.m.PauseTask(ctx, api.PauseTask{IdempotencyKey: k, TaskID: id, ExpectedVersion: v, Cause: api.PauseBudgetExhausted, Reason: "预算",
			ResumeCondition: "追加预算"})
	case task.CmdTakeControl:
		return w.m.TakeControl(ctx, api.TakeControl{IdempotencyKey: k, TaskID: id, ExpectedVersion: v, Reason: "接管"})
	case task.CmdHandoff:
		return w.m.Handoff(ctx, api.Handoff{IdempotencyKey: k, TaskID: id, ExpectedVersion: v, Reason: "交回"})
	case task.CmdCancel:
		return w.m.CancelTask(ctx, api.CancelTask{IdempotencyKey: k, TaskID: id, ExpectedVersion: v, Reason: "撤销"})
	case task.CmdFinishStop:
		return w.m.FinishStop(ctx, api.FinishStop{IdempotencyKey: k, TaskID: id, ExpectedVersion: v})
	case task.CmdCloseUnfulfilled:
		return w.m.CloseUnfulfilled(ctx, api.CloseUnfulfilled{IdempotencyKey: k, TaskID: id, ExpectedVersion: v, Cause: api.CloseCustomerDeclined,
			Reason: "拒绝"})
	case task.CmdFail:
		return w.m.FailTask(ctx, api.FailTask{IdempotencyKey: k, TaskID: id, ExpectedVersion: v, Reason: "失败", ResponsibleID: "alice"})
	case task.CmdBindObject:
		return w.m.BindObject(ctx, api.BindObject{IdempotencyKey: k, TaskID: id, ExpectedVersion: v, QuoteID: "quote.b." + id})
	case task.CmdRelease:
		return w.m.ReleaseObject(ctx, api.ReleaseObject{IdempotencyKey: k, TaskID: id, ExpectedVersion: v, Reason: "释放"})
	}
	w.t.Fatalf("no command %s", tr.Command)
	return api.TaskReceipt{}, nil
}

// Every one of SM-TSK-TASK's 67 transitions on PostgreSQL 16, by each actor kind its row names,
// through the registered command only: it lands in the row's state at the next task version, the
// database's guard and checks accept it, the history row shows it and the other kind, where the row
// names one, is refused PERMISSION_DENIED with the task left as it was.
func TestEveryTransitionOnPostgres(t *testing.T) {
	w := newWorld(t)
	actors := map[kernel.ActorKind]context.Context{kernel.ActorHuman: w.alice, kernel.ActorSystem: w.sys}
	n, want := 0, 0
	for i, tr := range task.Transitions() {
		want += len(tr.Actors)
		for _, kind := range []kernel.ActorKind{kernel.ActorHuman, kernel.ActorSystem} {
			id := "task.t" + strconv.Itoa(i) + string(kind[0])
			v := w.reach(id, tr.From, pathFor(tr))
			if got := w.get(id); got.Lifecycle != string(tr.From) {
				t.Fatalf("%s: reached %s, want %s", id, got.Lifecycle, tr.From)
			}
			r, err := w.apply(actors[kind], id, v, tr)
			admitted := false
			for _, k := range tr.Actors {
				admitted = admitted || k == kind
			}
			if !admitted {
				if code(err) != kernel.CodePermissionDenied || w.get(id).TaskVersion != v {
					t.Errorf("%s %s → %s by %s: %s", tr.From, tr.Command, tr.To, kind, code(err))
				}
				continue
			}
			if err != nil || r.Status != string(tr.To) || r.RowVersion != v+1 {
				t.Errorf("%s %s → %s by %s: %+v %v", tr.From, tr.Command, tr.To, kind, r, err)
				continue
			}
			got := w.get(id)
			last := got.History[len(got.History)-1]
			if last.Command != string(tr.Command) || last.From != string(tr.From) || last.To != string(tr.To) || last.Actor.Kind != kind ||
				last.TaskVersion != v+1 || len(got.History) != int(v+1) {
				t.Errorf("%s: history %+v", id, last)
			}
			n++
		}
	}
	if n != want || want != 81 {
		t.Fatalf("%d of %d transitions by admitted actors ran; SM-TSK-TASK has 67 rows, 14 of them human or system", n, want)
	}
}
