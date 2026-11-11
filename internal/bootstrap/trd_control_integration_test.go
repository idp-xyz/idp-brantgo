//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Object control of a quote root (WBS S3-08, SM-TRD-CONTROL, TRD-INV-015) on PostgreSQL 16. Python
// tests the binding on the acceptance and order commands of its T03 A24 slice; Brant's QuoteRoot
// fence is the same root-level check, so the A24 cases are ported here on the quote writes of S3-01
// (save, revise, discard, freeze, metadata) and the order wave reuses the fence. TSK, the caller of
// the commands, is a system actor here; it binds, the stop and release are human.

type controlWorld struct {
	*trdWorld
	tsk, agent context.Context
}

// newControlWorld provisions TSK (system) and an AI agent before anything is bound, since a change of
// access in the company ends what a binding delegated, and drafts quote.1 as the author.
func newControlWorld(t *testing.T) *controlWorld {
	q := newTrdWorld(t)
	c := &controlWorld{trdWorld: q, tsk: q.actorOf(kernel.ActorSystem, "tsk.1"), agent: q.actorOf(kernel.ActorAI, "agent.1")}
	q.drafted("quote.1")
	return c
}

func (c *controlWorld) bind(id, task string, expected int64) trdapi.Receipt {
	c.t.Helper()
	r, err := c.c.TRD.BindControl(c.tsk, trdapi.BindControl{IdempotencyKey: c.key(), QuoteID: id, TaskID: task, ExpectedControlEpoch: expected})
	c.must(err)
	return r
}

func (c *controlWorld) stop(id string, expected int64) (trdapi.Receipt, error) {
	return c.c.TRD.StopControl(c.author, trdapi.StopControl{IdempotencyKey: c.key(), QuoteID: id, ExpectedControlEpoch: expected, Reason: "人工接管"})
}

func (c *controlWorld) release(id string, expected int64) (trdapi.Receipt, error) {
	return c.c.TRD.ReleaseControl(c.author, trdapi.ReleaseControl{IdempotencyKey: c.key(), QuoteID: id, ExpectedControlEpoch: expected,
		Reason: "任务结束"})
}

// claimed returns ctx carrying the claim of task at control epoch epoch, as TSK's trusted adapter
// would put it there.
func (c *controlWorld) claimed(ctx context.Context, task string, epoch int64) context.Context {
	c.t.Helper()
	ctx, err := trdapi.WithControl(ctx, trdapi.ControlClaim{TaskID: task, ControlEpoch: epoch})
	c.must(err)
	return ctx
}

// writes are the quote writes of S3-01 on quote.1 as it is after newControlWorld (root version 1,
// working revision 1), each with a fresh key.
func (c *controlWorld) writes() map[string]func(ctx context.Context) error {
	return map[string]func(ctx context.Context) error{
		trdapi.CommandSaveDraft: func(ctx context.Context) error { _, err := c.saveQuote(ctx, "quote.1", 1, c.data()); return err },
		trdapi.CommandUpdateMetadata: func(ctx context.Context) error {
			_, err := c.c.TRD.UpdateMetadata(ctx, trdapi.UpdateMetadata{IdempotencyKey: c.key(), QuoteID: "quote.1", ExpectedVersion: 1,
				Labels: []string{"x"}})
			return err
		},
		trdapi.CommandDiscardDraft: func(ctx context.Context) error {
			_, err := c.c.TRD.DiscardDraft(ctx, trdapi.DiscardDraft{IdempotencyKey: c.key(), QuoteID: "quote.1", ExpectedVersion: 1,
				WorkingRevision: 1, Reason: "放弃", Evidence: c.proof})
			return err
		},
		trdapi.CommandRevise: func(ctx context.Context) error {
			_, err := c.c.TRD.Revise(ctx, trdapi.Revise{IdempotencyKey: c.key(), QuoteID: "quote.1", ExpectedVersion: 1, BaseRevision: 1})
			return err
		},
		trdapi.CommandFreeze: func(ctx context.Context) error { _, err := c.freezeQuote(ctx, "quote.1", 1, 1); return err },
	}
}

// unchanged checks that quote.1 is still the draft newControlWorld made, bound as control says.
func (c *controlWorld) unchanged(state string, epoch int64) {
	c.t.Helper()
	v := c.getQuote("quote.1")
	if v.RootVersion != 1 || len(v.Revisions) != 1 || v.WorkingRevision != 1 || len(v.Labels) != 0 || v.Control.State != state ||
		v.Control.ControlEpoch != epoch || len(c.frozenEvents()) != 0 {
		c.t.Fatalf("quote.1 changed: %+v", v)
	}
}

// Python test_t03_a24_write_control.py test_bound_root_rejects_missing_task_in_both_entrypoints_without_residue
// (PYT-0189) and test_t03_persistence.py test_bound_object_requires_server_task_and_current_authority
// (PYT-0420): on a bound root every quote write without the task's claim is OBJECT_CONTROL_CONFLICT,
// is audited as refused and leaves nothing behind.
func TestBoundRootRefusesWritesWithoutTheTaskAndLeavesNoResidue(t *testing.T) {
	c := newControlWorld(t)
	if b := c.bind("quote.1", "task.1", 0); b.RowVersion != 1 || b.Status != trdapi.ControlBound {
		t.Fatalf("bind %+v", b)
	}
	for name, write := range c.writes() {
		for _, ctx := range []context.Context{c.author, c.agent} {
			if got := code(write(ctx)); got != trdapi.CodeObjectControlConflict && (name != trdapi.CommandFreeze || ctx != c.agent) {
				t.Errorf("%s without the task: %s", name, got)
			}
		}
	}
	c.unchanged(trdapi.ControlBound, 1)
	entries, err := c.c.TRD.Audit(c.author)
	c.must(err)
	refused := 0
	for _, e := range entries {
		if e.Outcome == "rejected" && e.Code == trdapi.CodeObjectControlConflict {
			refused++
		}
	}
	if refused != 9 {
		t.Fatalf("%d audited refusals", refused)
	}
	v := c.getQuote("quote.1")
	if v.Control.TaskID != "task.1" || v.Control.ChangedBy != actorRef("system", "tsk.1") {
		t.Fatalf("control %+v", v.Control)
	}
}

// Python test_t03_a24_write_control.py test_matching_server_task_allows_local_write_and_rechecks_replays
// (PYT-0190): the task's claim admits its writes, a human's too (a human-only freeze for the task);
// a replay with the claim is the stored result, without it OBJECT_CONTROL_CONFLICT.
func TestMatchingTaskWritesAndReplaysAreRechecked(t *testing.T) {
	c := newControlWorld(t)
	c.bind("quote.1", "task.1", 0)
	agent := c.claimed(c.agent, "task.1", 1)
	cmd := trdapi.SaveDraft{IdempotencyKey: "task.save", QuoteID: "quote.1", ExpectedVersion: 1, Data: c.data()}
	first, err := c.c.TRD.SaveDraft(agent, cmd)
	if err != nil || first.RowVersion != 2 {
		t.Fatalf("the task's save %+v, %v", first, err)
	}
	again, err := c.c.TRD.SaveDraft(agent, cmd)
	if err != nil || !again.Replayed || again.ActionID != first.ActionID {
		t.Fatalf("the task's replay %+v, %v", again, err)
	}
	_, err = c.c.TRD.SaveDraft(c.agent, cmd)
	c.want(err, trdapi.CodeObjectControlConflict)
	f, err := c.freezeQuote(c.claimed(c.author, "task.1", 1), "quote.1", 2, 1)
	if err != nil || f.Status != trdapi.StatusFrozen {
		t.Fatalf("a human freeze for the task %+v, %v", f, err)
	}
}

// Python test_t03_a24_write_control.py test_wrong_task_and_stale_authority_block_current_task_for_all_three_commands
// (PYT-0191): another task's claim is OBJECT_CONTROL_CONFLICT; after any change of access in the
// company the bound task's own claim is too (the binding delegated access as it was).
func TestWrongTaskAndChangedAccessBlockTheTask(t *testing.T) {
	c := newControlWorld(t)
	c.bind("quote.1", "task.1", 0)
	for name, write := range c.writes() {
		if got := code(write(c.claimed(c.author, "task.other", 1))); got != trdapi.CodeObjectControlConflict {
			t.Errorf("%s for another task: %s", name, got)
		}
	}
	c.actorOf(kernel.ActorHuman, "observer")
	for name, write := range c.writes() {
		if got := code(write(c.claimed(c.author, "task.1", 1))); got != trdapi.CodeObjectControlConflict {
			t.Errorf("%s after access changed: %s", name, got)
		}
	}
	c.unchanged(trdapi.ControlBound, 1)
}

// Python test_t03_a24_write_control.py test_stopping_blocks_matching_task_until_cas_unbind
// (PYT-0192) and test_t03_persistence.py test_bound_foundation_object_is_blocked_until_unbound
// (PYT-0421): a stop moves the control epoch, so the task's claim is CONTROL_EPOCH_STALE and every
// other write OBJECT_CONTROL_CONFLICT; a release at the old epoch is VERSION_CONFLICT; after the
// release at the current epoch, writes without a claim pass and the old claim stays stale.
func TestStoppingFencesTheTaskUntilRelease(t *testing.T) {
	c := newControlWorld(t)
	c.bind("quote.1", "task.1", 0)
	s, err := c.stop("quote.1", 1)
	if err != nil || s.RowVersion != 2 || s.Status != trdapi.ControlStopping {
		t.Fatalf("stop %+v, %v", s, err)
	}
	for name, write := range c.writes() {
		if got := code(write(c.claimed(c.agent, "task.1", 1))); got != trdapi.CodeControlEpochStale && name != trdapi.CommandFreeze {
			t.Errorf("%s by the stopped task: %s", name, got)
		}
		if got := code(write(c.author)); got != trdapi.CodeObjectControlConflict {
			t.Errorf("%s while stopping: %s", name, got)
		}
	}
	_, err = c.release("quote.1", 1)
	c.want(err, kernel.CodeVersionConflict)
	_, err = c.stop("quote.1", 2)
	c.want(err, kernel.CodeInvalidState)
	r, err := c.release("quote.1", 2)
	if err != nil || r.RowVersion != 3 || r.Status != trdapi.ControlUnbound {
		t.Fatalf("release %+v, %v", r, err)
	}
	c.unchanged(trdapi.ControlUnbound, 3)
	_, err = c.saveQuote(c.claimed(c.agent, "task.1", 1), "quote.1", 1, c.data())
	c.want(err, trdapi.CodeControlEpochStale)
	_, err = c.saveQuote(c.agent, "quote.1", 1, c.data())
	c.must(err)
	_, err = c.release("quote.1", 3)
	c.want(err, kernel.CodeInvalidState)
}

// Python test_t03_a24_write_control.py test_committed_public_replay_and_local_replay_cannot_bypass_new_binding
// (PYT-0193): a write committed before the bind is not handed out again to a caller the binding now
// fences out; reconciliation, a read, still reports it committed; the task's own replay returns it
// and changes nothing.
func TestCommittedReplayCannotBypassANewBinding(t *testing.T) {
	c := newControlWorld(t)
	cmd := trdapi.UpdateMetadata{IdempotencyKey: "labels.1", QuoteID: "quote.1", ExpectedVersion: 1, Labels: []string{"a"}}
	first, err := c.c.TRD.UpdateMetadata(c.agent, cmd)
	c.must(err)
	c.bind("quote.1", "task.1", 0)
	_, err = c.c.TRD.UpdateMetadata(c.agent, cmd)
	c.want(err, trdapi.CodeObjectControlConflict)
	rec, err := c.c.TRD.Reconcile(c.agent, cmd)
	if err != nil || rec.State != kernel.ReconcileCommitted || rec.ActionID != first.ActionID {
		t.Fatalf("reconcile while bound %+v, %v", rec, err)
	}
	again, err := c.c.TRD.UpdateMetadata(c.claimed(c.agent, "task.1", 1), cmd)
	if err != nil || !again.Replayed || again.ActionID != first.ActionID {
		t.Fatalf("the task's replay %+v, %v", again, err)
	}
	if v := c.getQuote("quote.1"); v.RootVersion != 2 || !slices.Equal(v.Labels, []string{"a"}) {
		t.Fatalf("after the replays %+v", v)
	}
}

// Python test_t03_a24_write_control.py test_current_permissions_human_identity_and_scope_remain_required_with_task
// (PYT-0194) and test_matching_task_does_not_bypass_scope_and_team_changes (PYT-0201): the task's
// claim adds nothing to the actor's permissions, actor kind or data scope — a human without
// quote.freeze and an AI agent are PERMISSION_DENIED, an actor who does not see the root NOT_FOUND
// (visibility is judged before the control, so nothing about it is revealed).
func TestTaskClaimAddsNoPermissionOrScope(t *testing.T) {
	q := newTrdWorld(t)
	narrow := q.provision(grantOf(q, "narrow", kernel.ActorHuman, "company", trdapi.CommandSaveDraft, "trd.read", "evd.read"))
	outsider := q.provision(grantOf(q, "outsider", kernel.ActorHuman, "own", allPermissions...))
	c := &controlWorld{trdWorld: q, tsk: q.actorOf(kernel.ActorSystem, "tsk.1"), agent: q.actorOf(kernel.ActorAI, "agent.1")}
	q.drafted("quote.1")
	c.bind("quote.1", "task.1", 0)
	_, err := c.freezeQuote(c.claimed(narrow, "task.1", 1), "quote.1", 1, 1)
	c.want(err, kernel.CodePermissionDenied)
	_, err = c.freezeQuote(c.claimed(c.agent, "task.1", 1), "quote.1", 1, 1)
	c.want(err, kernel.CodePermissionDenied)
	for name, write := range c.writes() {
		if got := code(write(c.claimed(outsider, "task.1", 1))); got != kernel.CodeNotFound {
			t.Errorf("%s by an actor who does not see the root: %s", name, got)
		}
	}
	_, err = c.saveQuote(c.claimed(narrow, "task.1", 1), "quote.1", 1, c.data())
	c.must(err)
}

func grantOf(q *trdWorld, id string, kind kernel.ActorKind, scope string, permissions ...string) bootstrap.Grant {
	return bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kind, ID: id}, Permissions: permissions,
		DataScope: authorizationScope(scope), Team: "trade"}
}

// Python test_t03_a24_write_control.py test_task_rebinding_requires_new_server_identity_for_all_three_actions
// (PYT-0200): after a release and a new bind, the first task's claim is stale, a claim of the new
// epoch for another task conflicts, and only the new task writes.
func TestRebindingRequiresTheNewTask(t *testing.T) {
	c := newControlWorld(t)
	c.bind("quote.1", "task.1", 0)
	_, err := c.release("quote.1", 1)
	c.must(err)
	if b := c.bind("quote.1", "task.2", 2); b.RowVersion != 3 {
		t.Fatalf("rebind %+v", b)
	}
	_, err = c.saveQuote(c.claimed(c.agent, "task.1", 1), "quote.1", 1, c.data())
	c.want(err, trdapi.CodeControlEpochStale)
	_, err = c.saveQuote(c.claimed(c.agent, "task.1", 3), "quote.1", 1, c.data())
	c.want(err, trdapi.CodeObjectControlConflict)
	_, err = c.saveQuote(c.claimed(c.agent, "task.2", 3), "quote.1", 1, c.data())
	c.must(err)
	if v := c.getQuote("quote.1"); v.Control.TaskID != "task.2" || v.Control.ControlEpoch != 3 || v.RootVersion != 2 {
		t.Fatalf("control %+v", v.Control)
	}
}

// The rules of the control commands themselves: a root that does not exist or that the caller does
// not see is NOT_FOUND (a task claims an existing root); a bind at another epoch is VERSION_CONFLICT;
// a second binding OBJECT_CONTROL_CONFLICT; the stop's and release's reasons are audited; a
// stop or release keeps the root version.
func TestControlCommandRules(t *testing.T) {
	c := newControlWorld(t)
	_, err := c.c.TRD.BindControl(c.tsk, trdapi.BindControl{IdempotencyKey: c.key(), QuoteID: "quote.none", TaskID: "task.1"})
	c.want(err, kernel.CodeNotFound)
	_, err = c.c.TRD.BindControl(c.tsk, trdapi.BindControl{IdempotencyKey: c.key(), QuoteID: "quote.1", TaskID: "task.1", ExpectedControlEpoch: 1})
	c.want(err, kernel.CodeVersionConflict)
	c.bind("quote.1", "task.1", 0)
	_, err = c.c.TRD.BindControl(c.tsk, trdapi.BindControl{IdempotencyKey: c.key(), QuoteID: "quote.1", TaskID: "task.2", ExpectedControlEpoch: 1})
	c.want(err, trdapi.CodeObjectControlConflict)
	_, err = c.stop("quote.1", 1)
	c.must(err)
	_, err = c.release("quote.1", 2)
	c.must(err)
	c.unchanged(trdapi.ControlUnbound, 3)
	entries, err := c.c.TRD.Audit(c.author)
	c.must(err)
	var reasons []string
	for _, e := range entries {
		if e.Outcome == "succeeded" && (e.Command == trdapi.CommandStopControl || e.Command == trdapi.CommandReleaseControl) {
			reasons = append(reasons, e.Reason)
		}
	}
	if !slices.Equal(reasons, []string{"人工接管", "任务结束"}) {
		t.Fatalf("audited reasons %v", reasons)
	}
}

// Python test_t03_a24_write_control.py test_stop_commits_before_waiting_order_activation_is_rejected
// (PYT-0198) and BC-AT20 (AI 提交与接管竞争：已提交保留；旧 epoch 新提交拒绝), both commit orders on the
// root's row lock: a stop holding the root makes the task's waiting write fail with
// CONTROL_EPOCH_STALE once it commits; a task's write holding the root commits and is kept, and the
// stop waiting behind it commits after.
func TestStopAndTaskWriteDecideByCommitOrder(t *testing.T) {
	c := newControlWorld(t)
	d := newDownstream(c.world)
	c.bind("quote.1", "task.1", 0)
	stop := trdapi.StopControl{IdempotencyKey: "stop.held", QuoteID: "quote.1", ExpectedControlEpoch: 1, Reason: "人工接管"}
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandStopControl, stop.IdempotencyKey)
	stopped := make(chan error, 1)
	go func() {
		_, err := c.c.TRD.StopControl(c.author, stop)
		stopped <- err
	}()
	if !awaitInHold(t, "the stop holds the root and waits at its action result", func() bool { return d.blocks(blocker) }, stopped) {
		release()
		return
	}
	wrote := make(chan error, 1)
	task := c.claimed(c.agent, "task.1", 1)
	go func() {
		_, err := c.saveQuote(task, "quote.1", 1, c.data())
		wrote <- err
	}()
	ok := awaitInHold(t, "the task's write waits on the root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, wrote)
	release()
	if !ok {
		return
	}
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := <-wrote; code(err) != trdapi.CodeControlEpochStale {
		t.Fatalf("the waiting write after the stop: %s", code(err))
	}
	c.unchanged(trdapi.ControlStopping, 2)

	_, err := c.release("quote.1", 2)
	c.must(err)
	c.bind("quote.1", "task.2", 3)
	task = c.claimed(c.agent, "task.2", 4)
	save := trdapi.SaveDraft{IdempotencyKey: "save.held", QuoteID: "quote.1", ExpectedVersion: 1, Data: c.data()}
	blocker, release = d.keyBlocker(t, "brant_trd", trdapi.CommandSaveDraft, save.IdempotencyKey)
	go func() {
		_, err := c.c.TRD.SaveDraft(task, save)
		wrote <- err
	}()
	if !awaitInHold(t, "the task's write holds the root and waits at its action result", func() bool { return d.blocks(blocker) }, wrote) {
		release()
		return
	}
	go func() {
		_, err := c.stop("quote.1", 4)
		stopped <- err
	}()
	ok = awaitInHold(t, "the stop waits on the root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, stopped)
	release()
	if !ok {
		return
	}
	if err := <-wrote; err != nil {
		t.Fatalf("the task's write holding the root: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("the stop after the write: %v", err)
	}
	if v := c.getQuote("quote.1"); v.RootVersion != 2 || v.Control.State != trdapi.ControlStopping || v.Control.ControlEpoch != 5 {
		t.Fatalf("after both %+v", v)
	}
}

// Python test_t03_a24_write_control.py test_two_local_activations_with_current_task_keep_one_event
// (PYT-0202), on the freeze: two freezes of the bound task with one key commit one action and one
// QuoteFrozen event; the other is its replay.
func TestTwoClaimedFreezesKeepOneEvent(t *testing.T) {
	c := newControlWorld(t)
	c.bind("quote.1", "task.1", 0)
	ctx := c.claimed(c.author, "task.1", 1)
	cmd := trdapi.Freeze{IdempotencyKey: "freeze.task", QuoteID: "quote.1", ExpectedVersion: 1, WorkingRevision: 1}
	var wg sync.WaitGroup
	receipts, errs := make([]trdapi.FreezeReceipt, 2), make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipts[i], errs[i] = c.c.TRD.Freeze(ctx, cmd)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || receipts[0].ActionID != receipts[1].ActionID || receipts[0].Replayed == receipts[1].Replayed {
		t.Fatalf("two claimed freezes %+v %v", receipts, errs)
	}
	if n := len(c.frozenEvents()); n != 1 {
		t.Fatalf("%d QuoteFrozen events", n)
	}
	if v := c.getQuote("quote.1"); v.RootVersion != 2 || v.Control.State != trdapi.ControlBound {
		t.Fatalf("after %+v", v)
	}
}
