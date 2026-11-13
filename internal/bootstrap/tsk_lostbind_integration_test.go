//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
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

// bindThenLose is TRD's api for a TSK whose task.bind_object loses the answer of TRD's bind: TRD binds
// the root, TSK does not record it (a crash between the two steps).
type bindThenLose struct{ tsk.TRD }

func (b bindThenLose) BindControl(ctx context.Context, cmd trdapi.BindControl) (trdapi.Receipt, error) {
	if _, err := b.TRD.BindControl(ctx, cmd); err != nil {
		return trdapi.Receipt{}, err
	}
	return trdapi.Receipt{}, errors.New("connection lost after TRD's bind")
}

// lossyBind is a second TSK on TRD's api whose binds lose TRD's answer.
func (w *tskWorld) lossyBind() *tsk.Module {
	w.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	w.must(err)
	m, err := tsk.New(db, tsk.Upstream{Control: tsk.TRDControl(bindThenLose{w.c.TRD}), Readiness: tsk.TRDReadiness(w.c.TRD),
		Reconcilers: tsk.ActionReconciliation(tsk.ActionReconcilers{EVD: w.c.EVD, INQ: w.c.INQ, SUP: w.c.SUP, CST: w.c.CST, TRD: w.c.TRD})})
	w.must(err)
	return m
}

// The lost bind record (S4 C3; C1's gap 1) against TRD itself on PostgreSQL 16 (SM-TSK-TASK
// task.bind_object, task.take_control, task.finish_stop, task.release; SM-TRD-CONTROL; TSK-INV-002,
// TSK-INV-004, TRD-INV-015; MD-31; Context Map R17, R18).
//
// The AI task's agent drafts the quote as a recorded step; TSK binds the root that action created, but
// the answer of TRD's bind is lost: TRD holds the root for the task at epoch 1, the task records no
// binding. The system pauses the task before the bind is sent again. The task's claim names no root,
// so TRD refuses every write (OBJECT_CONTROL_CONFLICT) and the root would stay bound for good. A
// person takes the task over: TSK reads TRD's ledger for the bind it sent (TRD's audit shows the
// person's reconciliation read), adopts the binding at epoch 1 and stops the root with the key a
// recorded bind would have given the stop — a take-over whose own answer is lost is sent again and
// TRD replays that stop. The barrier switches the root to manual control, the person writes it under
// the manual claim and releases it.
//
// A task whose target names the root, closed before its lost bind was sent again, gives the root back
// through task.release, which learns the bind the same way; the release replays.
func TestALostBindRecordIsLearnedFromTRD(t *testing.T) {
	w := newTakeoverWorld(t)
	r := w.running("task.1", tskapi.GoalQuoteReady, tskapi.ModeAI, "")
	draft := trdapi.SaveDraft{QuoteID: "quote.1", Data: w.data()}
	rec := w.record(w.agent, "task.1", "intent.draft", 0, draft)
	draft.IdempotencyKey = rec.IdempotencyKey
	_, err := w.c.TRD.SaveDraft(w.agent, draft)
	w.must(err)
	_, err = w.c.TSK.ReconcileAction(w.agent, tskapi.ReconcileAction{IdempotencyKey: w.key(), TaskID: "task.1", ActionID: rec.ObjectID})
	w.must(err)

	lossy := w.lossyBind()
	bind := tskapi.BindObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, QuoteID: "quote.1"}
	if _, err = lossy.BindObject(w.sys, bind); err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("the bind's answer: %v", err)
	}
	q := w.getQuote("quote.1")
	tv, err := w.c.TSK.GetTask(w.clerk, "task.1")
	w.must(err)
	if q.Control.State != trdapi.ControlBound || q.Control.TaskID != "task.1" || q.Control.ControlEpoch != 1 || tv.Binding != nil ||
		tv.ControlEpoch != 0 {
		t.Fatalf("TRD %+v, task %+v", q.Control, tv.Binding)
	}
	r = w.task(w.c.TSK.PauseTask(w.sys, tskapi.PauseTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Cause: tskapi.PauseModelUnavailable, Reason: "模型持续不可用", ResumeCondition: "人工接管"}))
	// Without the binding the person's claim names no root, and TRD refuses the write.
	c, err := w.c.TSK.Claim(w.clerk, "task.1")
	w.must(err)
	if c.QuoteID != "" || c.ControlEpoch != 0 {
		t.Fatalf("claim %+v", c)
	}
	w.want(w.label(w.clerk, "quote.1", 1), trdapi.CodeObjectControlConflict)

	// The take-over learns the bind; its first attempt loses TRD's answer, the second replays TRD's stop.
	take := tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "模型持续不可用，人工接管"}
	if _, err = w.lossy().TakeControl(w.clerk, take); err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("the take-over's answer: %v", err)
	}
	take.IdempotencyKey = w.key()
	r = w.task(w.c.TSK.TakeControl(w.clerk, take))
	q = w.getQuote("quote.1")
	if r.Status != tskapi.StateStopping || r.ControlEpoch != 2 || r.BoundQuoteID != "quote.1" || q.Control.State != trdapi.ControlStopping ||
		q.Control.ControlEpoch != 2 {
		t.Fatalf("TRD %+v, task %+v", q.Control, r)
	}
	if got := w.trdAudit("quote.1", trdapi.CommandBindControl, trdapi.CommandStopControl); !slices.Equal(got, []string{
		"object_control.bind tsk succeeded", "object_control.stop clerk.1 succeeded", "object_control.stop clerk.1 replayed"}) {
		t.Fatalf("TRD audit %v", got)
	}
	if got := w.trdAudit("", "action.reconcile"); len(got) < 2 || !slices.Contains(got, "action.reconcile clerk.1 read") {
		t.Fatalf("TRD reconciliations %v", got)
	}

	r = w.finish("task.1", r.RowVersion)
	if r.Status != tskapi.StatePaused || r.ControlMode != tskapi.ModeManual || r.ControlEpoch != 3 || w.getQuote("quote.1").Control.State != trdapi.ControlManual {
		t.Fatalf("%+v", r)
	}
	hc, err := w.c.TSK.Claim(w.clerk, "task.1")
	w.must(err)
	if hc.QuoteID != "quote.1" || hc.ControlEpoch != 3 {
		t.Fatalf("manual claim %+v", hc)
	}
	w.must(w.label(w.claimFor(w.clerk, "task.1", hc.ControlEpoch), "quote.1", 1))
	r = w.task(w.c.TSK.ReleaseObject(w.clerk, tskapi.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "人工处理完毕，交还报价根"}))
	tv, err = w.c.TSK.GetTask(w.clerk, "task.1")
	w.must(err)
	if q = w.getQuote("quote.1"); r.ControlEpoch != 4 || q.Control.State != trdapi.ControlUnbound || tv.Target.QuoteID != "quote.1" ||
		tv.Binding.State != tskapi.BindingReleased || !slices.Equal(controlChanges(tv), []string{"take_control", "finish_stop", "release"}) {
		t.Fatalf("TRD %+v, task %+v %v", q.Control, tv.Binding, controlChanges(tv))
	}

	// The target names the root; the task is closed before its lost bind is sent again.
	w.drafted("quote.2")
	c2 := w.running("task.2", tskapi.GoalQuoteReady, tskapi.ModeAI, "quote.2")
	if _, err = lossy.BindObject(w.sys, tskapi.BindObject{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: c2.RowVersion,
		QuoteID: "quote.2"}); err == nil {
		t.Fatal("the lossy bind")
	}
	c2 = w.task(w.c.TSK.CloseUnfulfilled(w.sys, tskapi.CloseUnfulfilled{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: c2.RowVersion,
		Cause: tskapi.CloseNotPursued, Reason: "不再跟进"}))
	release := tskapi.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: c2.RowVersion, Reason: "任务已关闭，交还报价根"}
	c2 = w.task(w.c.TSK.ReleaseObject(w.clerk, release))
	if q = w.getQuote("quote.2"); c2.Status != tskapi.StateClosedUnfulfilled || c2.ControlEpoch != 2 || q.Control.State != trdapi.ControlUnbound ||
		q.Control.ControlEpoch != 2 {
		t.Fatalf("TRD %+v, task %+v", q.Control, c2)
	}
	if got := w.trdAudit("quote.2", trdapi.CommandReleaseControl); !slices.Equal(got, []string{"object_control.release clerk.1 succeeded"}) {
		t.Fatalf("TRD audit %v", got)
	}
	if again := w.task(w.c.TSK.ReleaseObject(w.clerk, release)); !again.Replayed || again.RowVersion != c2.RowVersion {
		t.Fatalf("replay %+v", again)
	}
	_, err = w.c.TSK.ReleaseObject(w.clerk, tskapi.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: c2.RowVersion,
		Reason: "再交还一次"})
	w.want(err, kernel.CodeInvalidState)
}
