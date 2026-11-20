//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TSK's control commands end to end against the composed TRD on PostgreSQL 16 (WBS S4-01e;
// SM-TSK-TASK task.take_control, task.handoff, task.cancel, task.finish_stop, task.release,
// task.bind_object; SM-TRD-CONTROL; TSK-INV-002, TSK-INV-003, TSK-INV-004; Context Map R17, R18;
// owner decisions D-2, D-3, D-4, D-5). The test plays the executor of task steps
// (internal/ai/execution, S4 unit C2): it takes the task's claim, records the step's attempt in TSK
// before it sends the command with the key TSK gives, and sends it with the claim. A take-over stops
// the root at TRD first and the task records TRD's epoch; the stop barrier reconciles every open
// action with its target before TRD switches the root to manual control or binds it again; a release
// gives the root back. Q01 vectors are cited as "Q01 V0.1 tests/test_execution.py::<name>".

// newTakeoverWorld is the TRD order world with TSK's system actor, the clerk who owns the tasks and
// takes them over, and the tasks' AI agent; the clerk and the agent execute task steps (action.*).
func newTakeoverWorld(t *testing.T) *tskWorld {
	q := newOrderWorld(t)
	every := append(append(slices.Clone(allPermissions), tskPermissions...), actionPermissions...)
	return &tskWorld{trdWorld: q,
		sys:   q.provision(grantOf(q, "tsk", kernel.ActorSystem, "company", every...)),
		clerk: q.provision(grantOf(q, "clerk.1", kernel.ActorHuman, "company", every...)),
		agent: q.provision(grantOf(q, "agent.1", kernel.ActorAI, "company", append(slices.Clone(allPermissions), "tsk.read",
			"action.record_attempt", "action.reconcile")...)),
	}
}

// record has executor ctx record the attempt of intent, a step of task sending TRD's command cmd at
// claim epoch epoch, before it is sent; it returns the receipt, whose IdempotencyKey the command
// carries.
func (w *tskWorld) record(ctx context.Context, task, intent string, epoch int64, cmd trdapi.Command) tskapi.ActionReceipt {
	w.t.Helper()
	req, err := w.c.TRD.RequestOf(cmd)
	w.must(err)
	rec, err := w.c.TSK.RecordAttempt(ctx, tskapi.RecordAttempt{IdempotencyKey: w.key(), TaskID: task, BusinessIntentID: intent,
		Target: tskapi.TargetCommand{Context: tskapi.TargetTRD, Command: req.Command, RequestHash: req.RequestHash}, ControlEpoch: epoch,
		Outcome: tskapi.ActionSubmitted})
	w.must(err)
	if rec.State != tskapi.ActionSubmitted || rec.IdempotencyKey != tskapi.ActionIDOf(intent) {
		w.t.Fatalf("%s: %+v", intent, rec)
	}
	return rec
}

func (w *tskWorld) action(id string) tskapi.ActionView {
	w.t.Helper()
	v, err := w.c.TSK.GetAction(w.clerk, id)
	w.must(err)
	return v
}

func (w *tskWorld) finish(id string, v int64) tskapi.TaskReceipt {
	w.t.Helper()
	return w.task(w.c.TSK.FinishStop(w.sys, tskapi.FinishStop{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v}))
}

// lossy is a second TSK on TRD's api through which a take-over loses TRD's answer after TRD stopped
// the root (stopThenLose): TRD's step commits, TSK's is never made.
func (w *tskWorld) lossy() *tsk.Module {
	w.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	w.must(err)
	m, err := tsk.New(db, tsk.Upstream{Control: tsk.TRDControl(stopThenLose{w.c.TRD}), Readiness: tsk.TRDReadiness(w.c.TRD),
		Reconcilers: tsk.ActionReconciliation(tsk.ActionReconcilers{EVD: w.c.EVD, INQ: w.c.INQ, SUP: w.c.SUP, CST: w.c.CST, TRD: w.c.TRD})})
	w.must(err)
	return m
}

// trdAudit lists TRD's audit entries on object (all objects when "") as "command actor outcome[ code]".
func (w *tskWorld) trdAudit(object string, commands ...string) []string {
	w.t.Helper()
	entries, err := w.c.TRD.Audit(w.author)
	w.must(err)
	var out []string
	for _, e := range entries {
		if (object == "" || e.ObjectID == object) && slices.Contains(commands, e.Command) {
			s := e.Command + " " + e.Actor.ID + " " + e.Outcome
			if e.Code != "" {
				s += " " + e.Code
			}
			out = append(out, s)
		}
	}
	return out
}

func controlChanges(v tskapi.TaskView) []string {
	var out []string
	for _, h := range v.History {
		if h.ControlChange != "" {
			out = append(out, h.ControlChange)
		}
	}
	return out
}

// BC-AT20 (TSK∥TRD, both commit orders forced), CT10, TSK-INV-002 and Q01 V0.1
// tests/test_execution.py::test_takeover_vs_commit_is_serialized_and_old_epoch_stops: the agent's
// recorded step on the bound root and a person's take-over meet on TRD's root lock.
//
//   - The step first: it holds the root (held at its action result) while the take-over's stop waits.
//     The step commits and is kept; the stop follows at epoch 2 and the task records it. The executor
//     never records the answer; the barrier reconciles the action — committed, with TRD's own receipt —
//     before TRD enters manual control.
//   - The take-over first: its stop holds the root (held at TSK's key for TRD's stop) while the step
//     waits. The stop commits; the step is CONTROL_EPOCH_STALE and changes nothing. The barrier
//     reconciles the action as not committed; the person sends the same intent again under the
//     manual claim with the same key and it executes once.
//
// Either way the task records TRD's epoch, the old claim and a blind resend of the old step are
// CONTROL_EPOCH_STALE, the agent gets no claim of the manual task, and the root holds at most one
// change per intent.
func TestTakeOverAndATaskStepDecideByCommitOrder(t *testing.T) {
	w := newTakeoverWorld(t)
	d := newDownstream(w.world)
	for _, stepFirst := range []bool{true, false} {
		id, quote, intent := "task.take", "quote.take", "intent.take"
		if stepFirst {
			id, quote, intent = "task.step", "quote.step", "intent.step"
		}
		w.drafted(quote)
		r := w.bound(id, quote)
		claimed, c := w.claimed(id)
		cmd := trdapi.UpdateMetadata{QuoteID: quote, ExpectedVersion: 1, Labels: []string{"ai-step"}}
		rec := w.record(w.agent, id, intent, c.ControlEpoch, cmd)
		cmd.IdempotencyKey = rec.IdempotencyKey
		take := tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, Reason: "人工接管"}
		wrote, took := make(chan error, 1), make(chan error, 1)
		var taken tskapi.TaskReceipt
		send := func() {
			_, err := w.c.TRD.UpdateMetadata(claimed, cmd)
			wrote <- err
		}
		takeOver := func() {
			var err error
			taken, err = w.c.TSK.TakeControl(w.clerk, take)
			took <- err
		}
		if stepFirst {
			blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandUpdateMetadata, cmd.IdempotencyKey)
			go send()
			if !awaitInHold(t, "the step holds the root and waits at its action result", func() bool { return d.blocks(blocker) }, wrote) {
				release()
				return
			}
			go takeOver()
			ok := awaitInHold(t, "the take-over's stop waits on the root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, took)
			release()
			if !ok {
				return
			}
		} else {
			blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandStopControl, tsk.ControlKey(id, trdapi.CommandStopControl, 1))
			go takeOver()
			if !awaitInHold(t, "the take-over's stop holds the root and waits at its action result", func() bool { return d.blocks(blocker) }, took) {
				release()
				return
			}
			go send()
			ok := awaitInHold(t, "the step waits on the root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, wrote)
			release()
			if !ok {
				return
			}
		}
		writeErr, takeErr := <-wrote, <-took
		w.must(takeErr)
		v := w.getQuote(quote)
		if taken.Status != tskapi.StateStopping || taken.ControlEpoch != 2 || v.Control.State != trdapi.ControlStopping || v.Control.ControlEpoch != 2 {
			t.Fatalf("stepFirst=%v: TRD %+v, task %+v", stepFirst, v.Control, taken)
		}
		if stepFirst {
			if writeErr != nil || v.RootVersion != 2 {
				t.Fatalf("the step holding the root: %v, root version %d", writeErr, v.RootVersion)
			}
		} else if code(writeErr) != trdapi.CodeControlEpochStale || v.RootVersion != 1 {
			t.Fatalf("the step after the stop: %s, root version %d", code(writeErr), v.RootVersion)
		}
		w.want(w.label(claimed, quote, v.RootVersion), trdapi.CodeControlEpochStale)

		r = w.finish(id, taken.RowVersion)
		a := w.action(rec.ObjectID)
		last := a.Log[len(a.Log)-1]
		if r.Status != tskapi.StatePaused || r.ControlMode != tskapi.ModeManual || r.ControlEpoch != 3 || last.Command != tskapi.CommandReconcileAction ||
			last.Actor != (kernel.ActorRef{Kind: kernel.ActorSystem, ID: "tsk"}) || last.ControlEpoch != 2 {
			t.Fatalf("stepFirst=%v: barrier %+v, action %+v", stepFirst, r, a)
		}
		if stepFirst {
			if a.State != tskapi.ActionCommitted || a.Result == nil || a.Result.ObjectID != quote || a.Result.RowVersion != 2 {
				t.Fatalf("the committed step: %+v", a)
			}
		} else if a.State != tskapi.ActionNotCommitted || a.Result != nil {
			t.Fatalf("the fenced step: %+v", a)
		}
		_, err := w.c.TSK.Claim(w.agent, id)
		w.want(err, tskapi.CodeControlModeMismatch)
		_, err = w.c.TRD.UpdateMetadata(claimed, cmd) // the old executor's blind resend under the step's key
		w.want(err, trdapi.CodeControlEpochStale)
		if !stepFirst {
			hc, err := w.c.TSK.Claim(w.clerk, id)
			w.must(err)
			human := w.claimFor(w.clerk, id, hc.ControlEpoch)
			again := w.record(w.clerk, id, intent, hc.ControlEpoch, cmd)
			if again.Attempt != 2 || again.IdempotencyKey != cmd.IdempotencyKey {
				t.Fatalf("the resend %+v", again)
			}
			_, err = w.c.TRD.UpdateMetadata(human, cmd)
			w.must(err)
			done, err := w.c.TSK.ReconcileAction(w.clerk, tskapi.ReconcileAction{IdempotencyKey: w.key(), TaskID: id, ActionID: rec.ObjectID})
			w.must(err)
			if done.State != tskapi.ActionCommitted || w.getQuote(quote).RootVersion != 2 {
				t.Fatalf("the person's resend %+v", done)
			}
		}
		if got := w.trdAudit(quote, trdapi.CommandUpdateMetadata); len(slices.DeleteFunc(slices.Clone(got),
			func(s string) bool { return !strings.HasSuffix(s, "succeeded") })) != 1 {
			t.Fatalf("stepFirst=%v: the step's commits %v", stepFirst, got)
		}
	}
}

// TD-AT10 (模型不可用，任务 paused 且根已绑定 → 接管进入 stopping；TRD 先递增 epoch，旧执行者返回
// CONTROL_EPOCH_STALE) and SM-TSK-TASK task.take_control "第二步失败时根保持围栏，按 action_id 重试": the AI task
// is paused because the model is unavailable; its agent gets no claim while paused. A person takes it
// over: TRD stops the root at epoch 2 and the answer is lost before TSK records it (a crash between
// the two steps). The root is fenced already — the old executor's write with the claim of epoch 1 is
// CONTROL_EPOCH_STALE — while the task still shows paused at epoch 1. The take-over is sent again:
// TSK sends TRD the same key, TRD replays its stop (no second stop), and the task records stopping
// at epoch 2; the barrier then puts the root under manual control at epoch 3.
func TestTakeOverOfAPausedTaskFencesTheOldExecutorFirst(t *testing.T) {
	w := newTakeoverWorld(t)
	w.drafted("quote.1")
	r := w.bound("task.1", "quote.1")
	old, _ := w.claimed("task.1")
	r = w.task(w.c.TSK.PauseTask(w.sys, tskapi.PauseTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Cause: tskapi.PauseModelUnavailable, Reason: "模型持续不可用", ResumeCondition: "模型恢复或人工接管"}))
	_, err := w.c.TSK.Claim(w.agent, "task.1")
	w.want(err, kernel.CodeInvalidState)

	take := tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "模型不可用，人工接管"}
	if _, err = w.lossy().TakeControl(w.clerk, take); err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("%v", err)
	}
	v := w.getQuote("quote.1")
	tv, err := w.c.TSK.GetTask(w.clerk, "task.1")
	w.must(err)
	if v.Control.State != trdapi.ControlStopping || v.Control.ControlEpoch != 2 || tv.Lifecycle != tskapi.StatePaused || tv.ControlEpoch != 1 ||
		tv.TaskVersion != r.RowVersion {
		t.Fatalf("TRD %+v, task %+v", v.Control, tv)
	}
	w.want(w.label(old, "quote.1", 1), trdapi.CodeControlEpochStale)

	took := w.task(w.c.TSK.TakeControl(w.clerk, take))
	if took.Status != tskapi.StateStopping || took.ControlEpoch != 2 {
		t.Fatalf("%+v", took)
	}
	if got := w.trdAudit("quote.1", trdapi.CommandStopControl); !slices.Equal(got, []string{"object_control.stop clerk.1 succeeded",
		"object_control.stop clerk.1 replayed"}) {
		t.Fatalf("%v", got)
	}
	r = w.finish("task.1", took.RowVersion)
	if v = w.getQuote("quote.1"); r.Status != tskapi.StatePaused || r.ControlMode != tskapi.ModeManual || r.ControlEpoch != 3 ||
		v.Control.State != trdapi.ControlManual || v.Control.ControlEpoch != 3 || v.RootVersion != 1 {
		t.Fatalf("TRD %+v, task %+v", v.Control, r)
	}
	w.want(w.label(old, "quote.1", 1), trdapi.CodeControlEpochStale)
}

// hookedReadiness is TSK's port to GoalReadiness v1 that runs after once its assessment (and its read
// of the root's control) was made.
type hookedReadiness struct {
	tsk.Readiness
	after func()
}

func (h *hookedReadiness) Assess(ctx context.Context, g tsk.Goal) (tsk.Verdict, error) {
	v, err := h.Readiness.Assess(ctx, g)
	if after := h.after; after != nil {
		h.after = nil
		after()
	}
	return v, err
}

// S4 B2-D3 and A1's two-step take-over (S4 C1: "release learns TRD's current epoch for the task's own
// binding instead of failing"): a take-over's stop commits at TRD right after goal.verify read the
// root's control, and its record in TSK is lost; goal.verify completes the task at the epoch it
// recorded, so the take-over's retry is VERSION_CONFLICT (sent again at the task's new version:
// INVALID_STATE) and the root stays stopping at epoch 2, an epoch the task never recorded. task.release learns it: TRD refuses the claim of epoch 1
// (CONTROL_EPOCH_STALE), TSK reads the root, finds it held by the task at epoch 2 and releases it at
// epoch 2; the task records the release at TRD's epoch 3 and stays completed; its replay answers the
// same. The same holds when the task was closed unfulfilled between the two steps of a take-over.
func TestReleaseLearnsTRDsEpochAfterALostTakeOverStep(t *testing.T) {
	w := newTakeoverWorld(t)
	w.activated("quote.1")
	w.drafted("quote.2")
	r := w.running("task.1", tskapi.GoalQuoteReady, tskapi.ModeAI, "quote.1")
	r = w.task(w.c.TSK.BindObject(w.sys, tskapi.BindObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, QuoteID: "quote.1"}))
	r = w.toVerifying("task.1", r.RowVersion)

	lossy := w.lossy()
	take := tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "人工接管"}
	hook := &hookedReadiness{Readiness: tsk.TRDReadiness(w.c.TRD), after: func() {
		if _, err := lossy.TakeControl(w.clerk, take); err == nil || !strings.Contains(err.Error(), "connection lost") {
			t.Errorf("the take-over's first step: %v", err)
		}
	}}
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	racy, err := tsk.New(db, tsk.Upstream{Control: tsk.TRDControl(w.c.TRD), Readiness: hook,
		Reconcilers: tsk.ActionReconciliation(tsk.ActionReconcilers{EVD: w.c.EVD, INQ: w.c.INQ, SUP: w.c.SUP, CST: w.c.CST, TRD: w.c.TRD})})
	w.must(err)
	r = w.task(racy.VerifyGoal(w.sys, tskapi.VerifyGoal{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	if v := w.getQuote("quote.1"); r.Status != tskapi.StateCompleted || r.ControlEpoch != 1 || v.Control.State != trdapi.ControlStopping ||
		v.Control.ControlEpoch != 2 {
		t.Fatalf("TRD %+v, task %+v", v.Control, r)
	}
	_, err = w.c.TSK.TakeControl(w.clerk, take)
	w.want(err, kernel.CodeVersionConflict)
	take.ExpectedVersion = r.RowVersion
	_, err = w.c.TSK.TakeControl(w.clerk, take)
	w.want(err, kernel.CodeInvalidState)

	release := tskapi.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "任务已完成，交还报价根"}
	r = w.task(w.c.TSK.ReleaseObject(w.clerk, release))
	v := w.getQuote("quote.1")
	tv, err := w.c.TSK.GetTask(w.clerk, "task.1")
	w.must(err)
	if r.Status != tskapi.StateCompleted || r.ControlEpoch != 3 || r.BoundQuoteID != "" || v.Control.State != trdapi.ControlUnbound ||
		v.Control.ControlEpoch != 3 || tv.Binding.State != tskapi.BindingReleased || tv.Binding.ControlEpoch != 3 ||
		!slices.Equal(controlChanges(tv), []string{"bind", "release"}) {
		t.Fatalf("TRD %+v, task %+v %+v", v.Control, r, tv.Binding)
	}
	if got := w.trdAudit("quote.1", trdapi.CommandReleaseControl); !slices.Equal(got, []string{
		"object_control.release clerk.1 rejected CONTROL_EPOCH_STALE", "object_control.release clerk.1 succeeded"}) {
		t.Fatalf("%v", got)
	}
	if again := w.task(w.c.TSK.ReleaseObject(w.clerk, release)); !again.Replayed || again.RowVersion != r.RowVersion {
		t.Fatalf("%+v", again)
	}

	c := w.running("task.2", tskapi.GoalQuoteReady, tskapi.ModeAI, "quote.2")
	c = w.task(w.c.TSK.BindObject(w.sys, tskapi.BindObject{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: c.RowVersion, QuoteID: "quote.2"}))
	if _, err = lossy.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: c.RowVersion,
		Reason: "人工接管"}); err == nil {
		t.Fatal("the lossy take-over")
	}
	c = w.task(w.c.TSK.CloseUnfulfilled(w.sys, tskapi.CloseUnfulfilled{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: c.RowVersion,
		Cause: tskapi.CloseNotPursued, Reason: "不再跟进"}))
	c = w.task(w.c.TSK.ReleaseObject(w.clerk, tskapi.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: c.RowVersion,
		Reason: "任务已关闭"}))
	if v = w.getQuote("quote.2"); c.Status != tskapi.StateClosedUnfulfilled || c.ControlEpoch != 3 || v.Control.State != trdapi.ControlUnbound {
		t.Fatalf("TRD %+v, task %+v", v.Control, c)
	}
}

// SC-05 and ES-06 (AI 执行中的人工接管): the AI task's quote is frozen and approved, its root bound. A
// person's task step — the activation, human only, so an AI agent with the claim is PERMISSION_DENIED
// — is recorded under the claim of epoch 1 when another person takes the task over: TRD's stop
// increments the epoch and the task records stopping (TaskControlChanged). The old executor then
// sends the activation with the old epoch: CONTROL_EPOCH_STALE. The barrier reconciles that
// submitted step with TRD before TRD switches the root to manual ("外部 submitted／unknown 结果先对账再
// 切换 manual": TRD's audit shows the reconciliation first). What was committed before stays (the
// frozen revision, the review and its approval); the take-over gives no privilege to skip the
// approval: under the manual claim, an activation naming no valid approval is refused. The person
// sends the step again with the same key under the new claim and the approval: one activation, one
// QuoteActivated; the old executor's blind resend stays CONTROL_EPOCH_STALE.
func TestAnOldEpochActivationAfterATakeOverIsFenced(t *testing.T) {
	w := newTakeoverWorld(t)
	every := append(append(slices.Clone(allPermissions), tskPermissions...), actionPermissions...)
	executor := w.provision(grantOf(w.trdWorld, "exec.1", kernel.ActorHuman, "company", every...))
	_, decided := w.approved("quote.1")
	r := w.running("task.1", tskapi.GoalQuoteReady, tskapi.ModeAI, "quote.1")
	r = w.task(w.c.TSK.BindObject(w.sys, tskapi.BindObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, QuoteID: "quote.1"}))
	agentClaimed, _ := w.claimed("task.1")
	hc, err := w.c.TSK.Claim(executor, "task.1")
	w.must(err)
	old := w.claimFor(executor, "task.1", hc.ControlEpoch)
	activate := w.activateCmd("quote.1", decided.RowVersion, 1, "approval.quote.1")
	_, err = w.c.TRD.Activate(agentClaimed, activate)
	w.want(err, kernel.CodePermissionDenied)
	rec := w.record(executor, "task.1", "intent.activate", hc.ControlEpoch, activate)
	activate.IdempotencyKey = rec.IdempotencyKey

	r = w.task(w.c.TSK.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "管理员接管"}))
	_, err = w.c.TRD.Activate(old, activate)
	w.want(err, trdapi.CodeControlEpochStale)
	before := w.getQuote("quote.1")
	r = w.finish("task.1", r.RowVersion)
	if a := w.action(rec.ObjectID); r.Status != tskapi.StatePaused || r.ControlMode != tskapi.ModeManual || a.State != tskapi.ActionNotCommitted {
		t.Fatalf("%+v %+v", r, a)
	}
	var order []string
	entries, err := w.c.TRD.Audit(w.author)
	w.must(err)
	for _, e := range entries {
		if e.Actor.ID == "tsk" && (e.Command == "action.reconcile" || e.Command == trdapi.CommandEnterManualControl) {
			order = append(order, e.Command)
		}
	}
	if !slices.Equal(order, []string{"action.reconcile", trdapi.CommandEnterManualControl}) {
		t.Fatalf("TRD saw %v", order)
	}
	after := w.getQuote("quote.1")
	if after.RootVersion != before.RootVersion || after.Revisions[0].Status != before.Revisions[0].Status ||
		!w.check("quote.1", "approval.quote.1").Releasable {
		t.Fatalf("the take-over changed what was committed: %+v → %+v", before, after)
	}

	hc, err = w.c.TSK.Claim(executor, "task.1")
	w.must(err)
	manual := w.claimFor(executor, "task.1", hc.ControlEpoch)
	unapproved := w.activateCmd("quote.1", decided.RowVersion, 1, "approval.none")
	if _, err = w.c.TRD.Activate(manual, unapproved); err == nil {
		t.Fatal("a manual activation without its approval")
	}
	again := w.record(executor, "task.1", "intent.activate", hc.ControlEpoch, activate)
	if again.Attempt != 2 {
		t.Fatalf("%+v", again)
	}
	_, err = w.c.TRD.Activate(manual, activate)
	w.must(err)
	_, err = w.c.TRD.Activate(old, activate)
	w.want(err, trdapi.CodeControlEpochStale)
	if n := len(w.eventsOf(trdapi.EventQuoteActivated)); n != 1 {
		t.Fatalf("%d QuoteActivated", n)
	}
}

// AN08 (Q01 未完成，模型持续不可用 → 人工接管并通过专业页面处理；未执行后台动作停止，无并行重复提交，completion_actor 为
// manual), SM-TSK-TASK task.bind_object "本任务的动作已创建报价根", and Q01 V0.1
// tests/test_execution.py::test_takeover_fences_old_agent_and_records_manual_completion end to end:
// the AI task's agent drafts the quote as a recorded step, and TSK binds the root that action created.
// Its next step is recorded but never sent: the model stops answering and the system pauses the task.
// A person takes over; the barrier reconciles the unsent step (not committed) and the root goes to
// manual control. The agent's late send is CONTROL_EPOCH_STALE and it gets no claim. The person
// resumes the task, sends the same step again — same intent, same key, executed once — and finishes
// the quote with colleagues under the manual claim (freeze, review, activation); the task is verified
// against TRD and completes with completion actor manual.
func TestAModelUnavailableTaskIsTakenOverAndCompletedManually(t *testing.T) {
	w := newTakeoverWorld(t)
	r := w.running("task.1", tskapi.GoalQuoteReady, tskapi.ModeAI, "")
	draft := trdapi.SaveDraft{QuoteID: "quote.1", Data: w.data()}
	rec := w.record(w.agent, "task.1", "intent.draft", 0, draft)
	draft.IdempotencyKey = rec.IdempotencyKey
	_, err := w.c.TRD.SaveDraft(w.agent, draft)
	w.must(err)
	done, err := w.c.TSK.ReconcileAction(w.agent, tskapi.ReconcileAction{IdempotencyKey: w.key(), TaskID: "task.1", ActionID: rec.ObjectID})
	w.must(err)
	if done.State != tskapi.ActionCommitted || done.Result.ObjectID != "quote.1" || done.Result.RowVersion != 1 {
		t.Fatalf("%+v", done)
	}
	_, err = w.c.TSK.BindObject(w.sys, tskapi.BindObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, QuoteID: "quote.2"})
	w.want(err, kernel.CodeReferenceInvalid)
	r = w.task(w.c.TSK.BindObject(w.sys, tskapi.BindObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, QuoteID: "quote.1"}))

	claimed, c := w.claimed("task.1")
	label := trdapi.UpdateMetadata{QuoteID: "quote.1", ExpectedVersion: 1, Labels: []string{"q01"}}
	step := w.record(w.agent, "task.1", "intent.label", c.ControlEpoch, label)
	label.IdempotencyKey = step.IdempotencyKey
	r = w.task(w.c.TSK.PauseTask(w.sys, tskapi.PauseTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Cause: tskapi.PauseModelUnavailable, Reason: "模型持续不可用", ResumeCondition: "人工接管"}))
	r = w.task(w.c.TSK.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "模型持续不可用，人工接管"}))
	r = w.finish("task.1", r.RowVersion)
	if a := w.action(step.ObjectID); r.Status != tskapi.StatePaused || r.ControlMode != tskapi.ModeManual || r.ControlEpoch != 3 ||
		a.State != tskapi.ActionNotCommitted {
		t.Fatalf("%+v %+v", r, a)
	}
	_, err = w.c.TRD.UpdateMetadata(claimed, label)
	w.want(err, trdapi.CodeControlEpochStale)
	_, err = w.c.TSK.Claim(w.agent, "task.1")
	w.want(err, tskapi.CodeControlModeMismatch)

	r = w.task(w.c.TSK.ResumeTask(w.clerk, tskapi.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "人工继续处理"}))
	hc, err := w.c.TSK.Claim(w.clerk, "task.1")
	w.must(err)
	clerk, author, reviewer := w.claimFor(w.clerk, "task.1", hc.ControlEpoch), w.claimFor(w.author, "task.1", hc.ControlEpoch),
		w.claimFor(w.reviewer, "task.1", hc.ControlEpoch)
	if again := w.record(w.clerk, "task.1", "intent.label", hc.ControlEpoch, label); again.Attempt != 2 || again.IdempotencyKey != label.IdempotencyKey {
		t.Fatalf("%+v", again)
	}
	_, err = w.c.TRD.UpdateMetadata(clerk, label)
	w.must(err)
	_, err = w.c.TRD.UpdateMetadata(clerk, label) // the same key again: TRD's replay, no second change
	w.must(err)
	if done, err = w.c.TSK.ReconcileAction(w.clerk, tskapi.ReconcileAction{IdempotencyKey: w.key(), TaskID: "task.1",
		ActionID: step.ObjectID}); err != nil || done.State != tskapi.ActionCommitted || done.Result.RowVersion != 2 {
		t.Fatalf("%+v %v", done, err)
	}
	f, err := w.freezeQuote(author, "quote.1", 2, 1)
	w.must(err)
	_, err = w.submit(author, "quote.1", f.RowVersion, 1, "review.1")
	w.must(err)
	dec, err := w.c.TRD.DecideReview(reviewer, w.approval("quote.1", f.RowVersion+1, "review.1", f.BusinessHash, "approval.1"))
	w.must(err)
	_, err = w.c.TRD.Activate(author, w.activateCmd("quote.1", dec.RowVersion, 1, "approval.1"))
	w.must(err)
	if v := w.getQuote("quote.1"); v.RootVersion != dec.RowVersion+1 {
		t.Fatalf("%+v", v)
	}

	r = w.toVerifying("task.1", r.RowVersion)
	r = w.task(w.verifyGoal(w.sys, "task.1", r.RowVersion))
	tv, err := w.c.TSK.GetTask(w.clerk, "task.1")
	w.must(err)
	if r.Status != tskapi.StateCompleted || tv.CompletionActor != tskapi.CompletionManual ||
		!slices.Equal(controlChanges(tv), []string{"bind", "take_control", "finish_stop"}) {
		t.Fatalf("%+v %+v", r, tv)
	}
	completed := w.completedEvents()
	if len(completed) != 1 || completed[0].Payload["completion_actor"] != tskapi.CompletionManual {
		t.Fatalf("%+v", completed)
	}
}

// Hand-back (SM-TSK-TASK task.handoff, task.finish_stop; SM-TRD-CONTROL manual → stopping → bound) and
// the delegation fence of a task with several delegations (S4 D-5; lead decision on A5's gap 1; S4
// C1): TRD's binding names the task's owner — the initiator of the delegation whose steps write the
// root — and its agent. A change of another person's grant, such as the costing clerk who delegated
// the costing drafts, does not fence the root: the agent writes on. A change of the owner's grant
// does (OBJECT_CONTROL_CONFLICT). The way on: a person takes the task over and hands it back; the
// barrier binds the root again (object_control.handoff), naming the delegation as its grants are now,
// the task resumes in mode ai and the agent writes with the claim of epoch 5.
func TestHandBackAfterTheOwnersGrantChanged(t *testing.T) {
	w := newTakeoverWorld(t)
	w.drafted("quote.1")
	r := w.bound("task.1", "quote.1")
	claimed, _ := w.claimed("task.1")
	every := append(append(slices.Clone(allPermissions), tskPermissions...), actionPermissions...)
	w.provision(grantOf(w.trdWorld, "costing.1", kernel.ActorHuman, "company", "cst.read", "costing_context.save_draft"))
	w.must(w.label(claimed, "quote.1", 1))
	w.regrant("clerk.1", "company", append(slices.Clone(every), "ai.read"))
	w.want(w.label(claimed, "quote.1", 2), trdapi.CodeObjectControlConflict)

	r = w.task(w.c.TSK.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "委托人权限变更，人工接管"}))
	r = w.finish("task.1", r.RowVersion)
	r = w.task(w.c.TSK.Handoff(w.clerk, tskapi.Handoff{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "核对完毕，交回 AI"}))
	if v := w.getQuote("quote.1"); r.Status != tskapi.StateStopping || r.ControlEpoch != 4 || v.Control.State != trdapi.ControlStopping {
		t.Fatalf("TRD %+v, task %+v", v.Control, r)
	}
	r = w.finish("task.1", r.RowVersion)
	v := w.getQuote("quote.1")
	if r.Status != tskapi.StatePaused || r.ControlMode != tskapi.ModeAI || r.ControlEpoch != 5 || v.Control.State != trdapi.ControlBound ||
		v.Control.ControlEpoch != 5 || v.Control.TaskID != "task.1" || v.Control.InitiatorID != "clerk.1" || v.Control.AgentID != "agent.1" {
		t.Fatalf("TRD %+v, task %+v", v.Control, r)
	}
	r = w.task(w.c.TSK.ResumeTask(w.clerk, tskapi.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "交回 AI 继续"}))
	w.want(w.label(claimed, "quote.1", 2), trdapi.CodeControlEpochStale)
	again, c := w.claimed("task.1")
	if c.ControlEpoch != 5 || c.ControlMode != tskapi.ModeAI {
		t.Fatalf("%+v", c)
	}
	w.must(w.label(again, "quote.1", 2))
	tv, err := w.c.TSK.GetTask(w.clerk, "task.1")
	w.must(err)
	if tv.Lifecycle != tskapi.StateRunning || !slices.Equal(controlChanges(tv), []string{"bind", "take_control", "finish_stop", "handoff",
		"finish_stop"}) {
		t.Fatalf("%+v", tv)
	}
	if got := w.trdAudit("quote.1", trdapi.CommandStopControl, trdapi.CommandEnterManualControl, trdapi.CommandHandoffControl); !slices.Equal(got,
		[]string{"object_control.stop clerk.1 succeeded", "object_control.enter_manual tsk succeeded", "object_control.stop clerk.1 succeeded",
			"object_control.handoff tsk succeeded"}) {
		t.Fatalf("%v", got)
	}
}
