//go:build integration

package tsk_test

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The stop barrier and the binding of a task's root on PostgreSQL 16 with TRD and the targets'
// ledgers faked (WBS S4-01e; SM-TSK-TASK task.finish_stop "屏障完成：submitted／unknown 已对账" and
// task.bind_object "本任务的动作已创建报价根"; TSK-INV-002, TSK-INV-003; baseline 8.5 "对已提交或结果未知的动作，
// 先查询及对账，再允许有冲突的人工或后台动作"). The composed suite (internal/bootstrap
// tsk_takeover_integration_test.go) runs them against TRD and the target contexts themselves.

func inqDraft(hash string) api.TargetCommand {
	return api.TargetCommand{Context: api.TargetINQ, Command: "rfq.save_draft", RequestHash: hash}
}

func cstDraft(hash string) api.TargetCommand {
	return api.TargetCommand{Context: api.TargetCST, Command: "costing_context.save_draft", RequestHash: hash}
}

// The barrier reconciles every open action itself before it changes the root's control: the
// agent's three steps are open when a person takes over — TRD committed the first, INQ holds
// nothing under the second's key (its answer was lost), CST holds another request under the
// third's. A target that cannot be reached leaves the barrier refused with the target's error and
// nothing changed; once it answers, one task.finish_stop records the three answers (committed with
// TRD's receipt, not_committed, conflict) as reconciliations by TSK's system actor at the task's
// control epoch, publishes three ActionReconciled before TaskControlChanged, and only then has TRD
// enter manual control. Its replay asks nobody. A cancellation's barrier reconciles too and does
// not call TRD.
func TestStopBarrierReconcilesEveryOpenAction(t *testing.T) {
	w := newWorld(t)
	r := w.bound("task.1", "quote.1")
	keys := map[string]string{}
	for intent, target := range map[string]api.TargetCommand{"intent.trd": activation(hashA), "intent.inq": inqDraft(hashA),
		"intent.cst": cstDraft(hashA)} {
		keys[intent] = api.ActionIDOf(intent)
		w.ok(w.submit(w.agent, "task.1", intent, target, r.ControlEpoch))
	}
	w.ok(w.observe(w.agent, "task.1", "intent.inq", inqDraft(hashA), api.ActionUnknown, "COMMIT_UNCERTAIN"))
	receipt := w.target.commit(api.TargetTRD, keys["intent.trd"], hashA, "order.1")
	w.target.commit(api.TargetCST, keys["intent.cst"], hashB, "context.1")
	r = w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "模型持续不可用"}))
	before := len(w.events())

	w.target.fail[api.TargetINQ] = errors.New("inq unreachable")
	finish := api.FinishStop{IdempotencyKey: "k.finish", TaskID: "task.1", ExpectedVersion: r.RowVersion}
	if _, err := w.m.FinishStop(w.sys, finish); err == nil || !strings.Contains(err.Error(), "inq unreachable") {
		t.Fatalf("an unreachable target: %v", err)
	}
	if v := w.get("task.1"); v.Lifecycle != api.StateStopping || v.TaskVersion != r.RowVersion || !slices.Equal(w.trd.ops(), []string{"bind", "stop"}) {
		t.Fatalf("%+v %v", v, w.trd.ops())
	}
	for _, key := range keys {
		if v := w.action(key); v.State != api.ActionSubmitted && v.State != api.ActionUnknown {
			t.Fatalf("a refused barrier settled %+v", v)
		}
	}
	delete(w.target.fail, api.TargetINQ)

	asked := w.target.questions()
	r = w.must(w.m.FinishStop(w.sys, finish))
	if r.Status != api.StatePaused || r.ControlMode != api.ModeManual || r.ControlEpoch != 3 ||
		!slices.Equal(w.trd.ops(), []string{"bind", "stop", "enter_manual"}) {
		t.Fatalf("%+v %v", r, w.trd.ops())
	}
	if n := w.target.questions() - asked; n != 3 {
		t.Fatalf("the barrier asked %d questions: %v", n, w.target.asked[asked:])
	}
	want := map[string]string{"intent.trd": api.ActionCommitted, "intent.inq": api.ActionNotCommitted, "intent.cst": api.ActionConflict}
	for intent, state := range want {
		v := w.action(keys[intent])
		last := v.Log[len(v.Log)-1]
		if v.State != state || last.Command != api.CommandReconcileAction || last.Actor != (kernel.ActorRef{Kind: kernel.ActorSystem, ID: "tsk"}) ||
			last.ControlEpoch != 2 {
			t.Errorf("%s: %+v, last %+v", intent, v, last)
		}
	}
	if v := w.action(keys["intent.trd"]); v.Result == nil || *v.Result != receipt {
		t.Fatalf("TRD's receipt: %+v", v.Result)
	}
	evs := w.events()[before:]
	if got := types(evs); !slices.Equal(got, []string{api.EventActionReconciled, api.EventActionReconciled, api.EventActionReconciled,
		api.EventTaskControlChanged}) {
		t.Fatalf("%v", got)
	}
	asked = w.target.questions()
	if again := w.must(w.m.FinishStop(w.sys, finish)); !again.Replayed || again.RowVersion != r.RowVersion || w.target.questions() != asked {
		t.Fatalf("the replay %+v asked %d", again, w.target.questions()-asked)
	}

	c := w.bound("task.2", "quote.2")
	w.ok(w.submit(w.agent, "task.2", "intent.c", inqDraft(hashB), c.ControlEpoch))
	c = w.must(w.m.CancelTask(w.alice, api.CancelTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: c.RowVersion, Reason: "客户撤回"}))
	c = w.must(w.m.FinishStop(w.sys, api.FinishStop{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: c.RowVersion}))
	if v := w.action(api.ActionIDOf("intent.c")); c.Status != api.StateCancelled || v.State != api.ActionNotCommitted ||
		!slices.Equal(w.trd.ops(), []string{"bind", "stop", "enter_manual", "bind", "stop"}) {
		t.Fatalf("%+v %+v %v", c, v, w.trd.ops())
	}
}

// Lock order of the barrier (ActionExecution's: the action row FOR UPDATE, then the task row): a
// person's action.reconcile and the barrier on the same open action, started a few milliseconds
// apart in each round so that both orders occur, never deadlock; both succeed — the barrier skips
// an action the person settled first, the person reconciles again one the barrier settled first —
// and the task ends paused under manual control.
func TestBarrierAndAReconciliationSerialize(t *testing.T) {
	w := newWorld(t)
	outcomes := map[string]int{}
	for i := range 5 {
		id, intent := "task.s"+string(rune('0'+i)), "intent.s"+string(rune('0'+i))
		r := w.bound(id, "quote.s"+string(rune('0'+i)))
		w.ok(w.submit(w.agent, id, intent, activation(hashA), r.ControlEpoch))
		r = w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, Reason: "接管"}))
		var wg sync.WaitGroup
		var finishErr, reconcileErr error
		finishKey, reconcileKey := w.key(), w.key()
		wg.Add(2)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 5 * time.Millisecond)
			_, finishErr = w.m.FinishStop(w.sys, api.FinishStop{IdempotencyKey: finishKey, TaskID: id, ExpectedVersion: r.RowVersion})
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(4-i) * 5 * time.Millisecond)
			_, reconcileErr = w.m.ReconcileAction(w.alice, api.ReconcileAction{IdempotencyKey: reconcileKey, TaskID: id, ActionID: api.ActionIDOf(intent)})
		}()
		wg.Wait()
		if finishErr != nil || reconcileErr != nil {
			t.Fatalf("round %d: barrier %v, reconciliation %v", i, finishErr, reconcileErr)
		}
		v := w.action(api.ActionIDOf(intent))
		if v.State != api.ActionNotCommitted || w.get(id).Lifecycle != api.StatePaused {
			t.Fatalf("round %d: %+v", i, v)
		}
		outcomes[v.Log[1].Actor.ID+" first"]++
	}
	t.Logf("first reconciliation of the open action: %v", outcomes)
}

// SM-TSK-TASK task.bind_object "本任务的动作已创建报价根" (S4 C1, B1-D09): a running AI task whose target
// names no root binds only a root one of its own actions created — TRD committed the task's
// quote.save_draft at root version 1. A root nobody gave the task, a root another task's action
// created, a root the task's action only saved again (version 2) and a creating save still open are
// REFERENCE_INVALID before TRD is asked; the root the task created binds and becomes its target. A
// task whose target names a root binds that one without having created it.
func TestBindNeedsTheTargetOrARootTheTaskCreated(t *testing.T) {
	w := newWorld(t)
	r := w.running("task.1")
	w.running("task.2")
	bind := func(quote string) error {
		_, err := w.m.BindObject(w.sys, api.BindObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, QuoteID: quote})
		return err
	}
	w.want(bind("quote.1"), kernel.CodeReferenceInvalid)
	w.created(w.alice, "task.2", "draft.other", "quote.2")
	w.want(bind("quote.2"), kernel.CodeReferenceInvalid)
	saved := w.target.commit(api.TargetTRD, api.ActionIDOf("save.again"), hashB, "quote.3") // a save at root version 2
	if v := w.ok(w.submit(w.agent, "task.1", "save.again", saveDraft(hashB), 0)); v.State != api.ActionCommitted || v.Result.RowVersion != saved.RowVersion {
		t.Fatalf("%+v", v)
	}
	w.want(bind("quote.3"), kernel.CodeReferenceInvalid)
	w.ok(w.submit(w.agent, "task.1", "draft.open", saveDraft(hashA), 0))
	w.want(bind("quote.4"), kernel.CodeReferenceInvalid)
	if ops := w.trd.ops(); len(ops) != 0 || w.get("task.1").TaskVersion != r.RowVersion {
		t.Fatalf("refused bindings asked TRD %v", ops)
	}

	w.created(w.agent, "task.1", "draft.mine", "quote.5")
	b := w.must(w.m.BindObject(w.sys, api.BindObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, QuoteID: "quote.5"}))
	if v := w.get("task.1"); b.BoundQuoteID != "quote.5" || v.Target.QuoteID != "quote.5" || !slices.Equal(w.trd.ops(), []string{"bind"}) {
		t.Fatalf("%+v %+v", b, v.Target)
	}
	named := w.bound("task.3", "quote.6")
	if named.BoundQuoteID != "quote.6" {
		t.Fatalf("%+v", named)
	}
}

// The action log admits TSK's system actor only for the barrier's reconciliations (migration tsk
// 000004): a system actor's attempt row is refused by the database.
func TestActionLogAdmitsTheSystemOnlyForReconciliations(t *testing.T) {
	w := newWorld(t)
	r := w.bound("task.1", "quote.1")
	w.ok(w.submit(w.agent, "task.1", "intent.1", activation(hashA), r.ControlEpoch))
	r = w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "接管"}))
	w.must(w.m.FinishStop(w.sys, api.FinishStop{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	where := ` WHERE action_id = '` + api.ActionIDOf("intent.1") + `'`
	err := w.sql(`INSERT INTO brant_tsk.action_attempt (tenant_id, company_id, action_id, action_version, attempt, command, state, answer,
		control_epoch, actor_kind, actor_id, authority_epoch, recorded_at)
		SELECT tenant_id, company_id, action_id, action_version + 1, attempt + 1, 'action.record_attempt', 'submitted', 'not_committed',
		control_epoch, 'system', 'tsk', authority_epoch, recorded_at FROM brant_tsk.action_attempt` + where + ` AND action_version = 2`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("a system actor's attempt: %v", err)
	}
	if v := w.action(api.ActionIDOf("intent.1")); v.State != api.ActionNotCommitted || len(v.Log) != 2 || v.Log[1].Actor.Kind != kernel.ActorSystem {
		t.Fatalf("%+v", v)
	}
}
