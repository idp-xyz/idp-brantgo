//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TRD's object control under TSK (WBS S4-01b; SM-TRD-CONTROL as A0 committed it, TRD-INV-015,
// TSK-INV-004; S4 decisions D-3, D-4 and D-5) on PostgreSQL 16: the manual state, the hand-back, the
// bound task's claim on every control change, and the binding's fence on its delegation's grants.
// TSK does not exist in this tree; the tests put the claim into the context as its trusted adapter
// will (trdapi.WithControl), and TSK's system actor tsk.1 completes the barrier.

// Decision D-4 (TSK-INV-004 negative: 绕过 TSK 释放根返回 OBJECT_CONTROL_CONFLICT; SM-TRD-CONTROL: every
// change with the bound task's claim): a stop, switch, hand-back or release without the claim — what
// the HTTP command routes send — or with another task's is OBJECT_CONTROL_CONFLICT and audited; an
// earlier epoch's claim is CONTROL_EPOCH_STALE; an AI agent changes nothing (PERMISSION_DENIED). With
// the claim, the root goes through every transition, one control epoch each: bind 1, stop 2,
// enter_manual 3, stop 4, handoff 5, release 6; the root version never moves. TSK's retry of a step
// by its key after the epoch moved (SM-TSK-TASK task.take_control: 按 action_id 重试) answers the
// stored receipt.
func TestControlChangesNeedTheBoundTasksClaim(t *testing.T) {
	c := newControlWorld(t)
	c.bind("quote.1", "task.1", 0)
	for name, change := range map[string]func() error{
		"a direct stop":           func() error { _, err := c.stop(c.author, "quote.1", 1); return err },
		"a direct release":        func() error { _, err := c.release(c.author, "quote.1", 1); return err },
		"a switch without claim":  func() error { _, err := c.enterManual(c.tsk, "quote.1", 1); return err },
		"a handoff without claim": func() error { _, err := c.handoff(c.tsk, "quote.1", 1); return err },
		"another task's stop":     func() error { _, err := c.stop(c.claimed(c.author, "task.2", 1), "quote.1", 1); return err },
	} {
		if got := code(change()); got != trdapi.CodeObjectControlConflict {
			t.Errorf("%s: %s", name, got)
		}
	}
	if n := c.refusals(trdapi.CodeObjectControlConflict); n != 5 {
		t.Fatalf("%d audited refusals", n)
	}
	_, err := c.enterManual(c.claimed(c.agent, "task.1", 1), "quote.1", 1)
	c.want(err, kernel.CodePermissionDenied)
	_, err = c.handoff(c.claimed(c.agent, "task.1", 1), "quote.1", 1)
	c.want(err, kernel.CodePermissionDenied)
	c.unchanged(trdapi.ControlBound, 1)

	stop := trdapi.StopControl{IdempotencyKey: "stop.takeover", QuoteID: "quote.1", ExpectedControlEpoch: 1, Reason: "人工接管"}
	s, err := c.c.TRD.StopControl(c.claimed(c.author, "task.1", 1), stop)
	if err != nil || s.RowVersion != 2 || s.Status != trdapi.ControlStopping {
		t.Fatalf("stop %+v %v", s, err)
	}
	_, err = c.enterManual(c.claimed(c.tsk, "task.1", 1), "quote.1", 2)
	c.want(err, trdapi.CodeControlEpochStale)
	m, err := c.enterManual(c.claimed(c.tsk, "task.1", 2), "quote.1", 2)
	if err != nil || m.RowVersion != 3 || m.Status != trdapi.ControlManual {
		t.Fatalf("enter_manual %+v %v", m, err)
	}
	again, err := c.c.TRD.StopControl(c.claimed(c.author, "task.1", 1), stop)
	if err != nil || !again.Replayed || again.ActionID != s.ActionID || again.RowVersion != 2 {
		t.Fatalf("TSK's retry of the stop %+v %v", again, err)
	}
	if s, err = c.stop(c.claimed(c.author, "task.1", 3), "quote.1", 3); err != nil || s.RowVersion != 4 || s.Status != trdapi.ControlStopping {
		t.Fatalf("stop from manual %+v %v", s, err)
	}
	h, err := c.handoff(c.claimed(c.tsk, "task.1", 4), "quote.1", 4)
	if err != nil || h.RowVersion != 5 || h.Status != trdapi.ControlBound {
		t.Fatalf("handoff %+v %v", h, err)
	}
	if v := c.getQuote("quote.1"); v.Control.TaskID != "task.1" || v.Control.ChangedBy != actorRef("system", "tsk.1") ||
		v.Control.AgentID != "agent.1" {
		t.Fatalf("after the handoff %+v", v.Control)
	}
	r, err := c.release(c.claimed(c.author, "task.1", 5), "quote.1", 5)
	if err != nil || r.RowVersion != 6 || r.Status != trdapi.ControlUnbound {
		t.Fatalf("release %+v %v", r, err)
	}
	c.unchanged(trdapi.ControlUnbound, 6)
	entries, err := c.c.TRD.Audit(c.author)
	c.must(err)
	var done []string
	for _, e := range entries {
		if e.Outcome == "succeeded" && e.Command != trdapi.CommandSaveDraft {
			done = append(done, e.Command+":"+e.Reason)
		}
	}
	if want := []string{"object_control.bind:", "object_control.stop:人工接管", "object_control.enter_manual:屏障完成，转人工",
		"object_control.stop:人工接管", "object_control.handoff:屏障完成，交回 AI", "object_control.release:任务结束"}; !slices.Equal(done, want) {
		t.Fatalf("audited changes %v", done)
	}
}

// Decision D-3 (SM-TRD-CONTROL stopping → manual: 此后只接受携带当前声明的人工写入（AI 返回
// CONTROL_MODE_MISMATCH）; TSK-INV-004 negative: manual 模式下 AI 写入返回 CONTROL_MODE_MISMATCH; Q01
// V0.1 tests/test_execution.py test_takeover_fences_old_agent_and_records_manual_completion, TRD's
// half): after a takeover the AI agent's writes with the task's current claim are
// CONTROL_MODE_MISMATCH (audited) and the readiness read says so; its claim from before the takeover
// is CONTROL_EPOCH_STALE; a human without the claim is OBJECT_CONTROL_CONFLICT; a human with it
// writes, a human-only freeze included, under its own grant — a change of the agent's grant does not
// fence manual steps. Handed back, the AI writes again with the new epoch's claim under the
// delegation recorded at the handoff.
func TestManualControlAdmitsHumanTaskSteps(t *testing.T) {
	c := newControlWorld(t)
	c.bind("quote.1", "task.1", 0)
	if _, err := c.c.TRD.UpdateMetadata(c.claimed(c.agent, "task.1", 1), trdapi.UpdateMetadata{IdempotencyKey: c.key(), QuoteID: "quote.1",
		ExpectedVersion: 1, Labels: []string{"ai"}}); err != nil {
		t.Fatalf("the AI's step while bound: %v", err)
	}
	_, err := c.stop(c.claimed(c.author, "task.1", 1), "quote.1", 1)
	c.must(err)
	_, err = c.enterManual(c.claimed(c.tsk, "task.1", 2), "quote.1", 2)
	c.must(err)
	for name, write := range c.writes() {
		want := trdapi.CodeControlModeMismatch
		if name == trdapi.CommandFreeze {
			want = kernel.CodePermissionDenied
		}
		if got := code(write(c.claimed(c.agent, "task.1", 3))); got != want {
			t.Errorf("%s by the AI agent with the claim: %s", name, got)
		}
		if got := code(write(c.claimed(c.agent, "task.1", 1))); got != trdapi.CodeControlEpochStale && name != trdapi.CommandFreeze {
			t.Errorf("%s by the AI agent with its old claim: %s", name, got)
		}
		if got := code(write(c.author)); got != trdapi.CodeObjectControlConflict {
			t.Errorf("%s by a human without the claim: %s", name, got)
		}
	}
	if n := c.refusals(trdapi.CodeControlModeMismatch); n != 4 {
		t.Fatalf("%d audited mode mismatches", n)
	}
	if r := c.readiness(c.claimed(c.agent, "task.1", 3), "quote.1", 0); r.ControlState != trdapi.ControlManual ||
		!slices.Contains(reasonsOf(r.Reasons), "object_control:CONTROL_MODE_MISMATCH") {
		t.Fatalf("readiness for the AI agent %+v", r)
	}

	c.provision(grantOf(c.trdWorld, "agent.1", kernel.ActorAI, "company", without(allPermissions, "pur.audit")...))
	human := c.claimed(c.author, "task.1", 3)
	if _, err = c.c.TRD.UpdateMetadata(human, trdapi.UpdateMetadata{IdempotencyKey: c.key(), QuoteID: "quote.1", ExpectedVersion: 2,
		Labels: []string{"manual"}}); err != nil {
		t.Fatalf("a human step: %v", err)
	}
	if f, err := c.freezeQuote(human, "quote.1", 3, 1); err != nil || f.Status != trdapi.StatusFrozen {
		t.Fatalf("a human-only freeze in manual control %+v %v", f, err)
	}

	_, err = c.stop(human, "quote.1", 3)
	c.must(err)
	_, err = c.handoff(c.claimed(c.tsk, "task.1", 4), "quote.1", 4)
	c.must(err)
	_, err = c.c.TRD.Revise(c.author, trdapi.Revise{IdempotencyKey: c.key(), QuoteID: "quote.1", ExpectedVersion: 4, BaseRevision: 1})
	c.want(err, trdapi.CodeObjectControlConflict)
	if r, err := c.c.TRD.Revise(c.claimed(c.agent, "task.1", 5), trdapi.Revise{IdempotencyKey: c.key(), QuoteID: "quote.1", ExpectedVersion: 4,
		BaseRevision: 1}); err != nil || r.RowVersion != 5 {
		t.Fatalf("the AI's step after the handoff %+v %v", r, err)
	}
	if v := c.getQuote("quote.1"); v.Control.State != trdapi.ControlBound || v.Control.ControlEpoch != 5 || !slices.Equal(v.Labels, []string{"manual"}) ||
		v.WorkingRevision != 2 {
		t.Fatalf("quote.1 %+v", v)
	}
}

// Decision D-5 (replaces S3-D12), BC-AT20's pattern for the delegation: a change of the initiator's
// grant and a task's write decide by commit order on the authority guard rows. The write holds the
// root and, through its own authorization and the delegation's (Authorizer.Qualify), the scope's
// guard row and the grant rows FOR SHARE: a grant change waiting behind it lets it commit, and the
// task's next write is OBJECT_CONTROL_CONFLICT. Stopped and handed back under the grants as they are,
// the task writes again; a grant change holding the guard row first makes the waiting write
// OBJECT_CONTROL_CONFLICT once it commits. A handoff to an agent whose grant was revoked is
// REFERENCE_INVALID and leaves the root stopping.
func TestDelegationChangeAndTaskWriteDecideByCommitOrder(t *testing.T) {
	c := newControlWorld(t)
	d := newDownstream(c.world)
	c.bind("quote.1", "task.1", 0)
	initiator := actorID(t, c.author)
	changed := func(i int) []string {
		return without(allPermissions, []string{"pur.audit", "stl.audit", "cnx.audit"}[i])
	}
	metadata := func(ctx context.Context, key string, expected int64, label string) error {
		_, err := c.c.TRD.UpdateMetadata(ctx, trdapi.UpdateMetadata{IdempotencyKey: key, QuoteID: "quote.1", ExpectedVersion: expected,
			Labels: []string{label}})
		return err
	}

	// The task's write first.
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandUpdateMetadata, "meta.held")
	wrote := make(chan error, 1)
	go func() { wrote <- metadata(c.claimed(c.agent, "task.1", 1), "meta.held", 1, "first") }()
	if !awaitInHold(t, "the task's write holds the root and waits at its action result", func() bool { return d.blocks(blocker) }, wrote) {
		release()
		return
	}
	regranted := make(chan error, 1)
	go func() {
		_, err := c.platform.Provision(context.Background(), grantOf(c.trdWorld, initiator, kernel.ActorHuman, "company", changed(0)...))
		regranted <- err
	}()
	ok := awaitInHold(t, "the initiator's grant change waits on the guard row", func() bool { return lockWaiter(t, "brant_auth.authority_scope") },
		regranted)
	release()
	if !ok {
		return
	}
	if err := <-wrote; err != nil {
		t.Fatalf("the task's write holding the guard rows: %v", err)
	}
	if err := <-regranted; err != nil {
		t.Fatalf("the grant change after the write: %v", err)
	}
	c.want(metadata(c.claimed(c.agent, "task.1", 1), c.key(), 2, "second"), trdapi.CodeObjectControlConflict)

	// Stopped and handed back under the grants as they are now.
	_, err := c.stop(c.claimed(c.author, "task.1", 1), "quote.1", 1)
	c.must(err)
	_, err = c.handoff(c.claimed(c.tsk, "task.1", 2), "quote.1", 2)
	c.must(err)
	c.must(metadata(c.claimed(c.agent, "task.1", 3), c.key(), 2, "handed back"))

	// The grant change first.
	hold, held, changeDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		changeDone <- d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
			if _, err := (authorization.Grants{}).Put(ctx, exec, d.scope, kernel.ActorRef{Kind: kernel.ActorHuman, ID: initiator}, changed(1),
				time.Now()); err != nil {
				return err
			}
			close(held)
			<-hold
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-changeDone:
		t.Fatalf("the grant change: %v", err)
	}
	go func() { wrote <- metadata(c.claimed(c.agent, "task.1", 3), c.key(), 3, "third") }()
	ok = awaitInHold(t, "the task's write waits on the guard row", func() bool { return lockWaiter(t, "brant_auth.authority_scope") }, wrote)
	close(hold)
	if err := <-changeDone; err != nil {
		t.Fatalf("the grant change holding the guard row: %v", err)
	}
	if !ok {
		return
	}
	if err := <-wrote; code(err) != trdapi.CodeObjectControlConflict {
		t.Fatalf("the waiting write after the grant change: %s", code(err))
	}

	// A handoff names agents with active AI grants.
	_, err = c.stop(c.claimed(c.author, "task.1", 3), "quote.1", 3)
	c.must(err)
	err = d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := authorization.Grants{}.Revoke(ctx, exec, d.scope, "agent.1", time.Now())
		return err
	})
	c.must(err)
	_, err = c.handoff(c.claimed(c.tsk, "task.1", 4), "quote.1", 4)
	c.want(err, kernel.CodeReferenceInvalid)
	if v := c.getQuote("quote.1"); v.Control.State != trdapi.ControlStopping || v.Control.ControlEpoch != 4 || v.RootVersion != 3 ||
		!slices.Equal(v.Labels, []string{"handed back"}) {
		t.Fatalf("quote.1 %+v", v)
	}
}

// The database keeps SM-TRD-CONTROL (TRD 000009's trigger and checks) against the application role:
// no transition the machine lacks, no skipped epoch, no other task while bound, no bind or handoff
// without the delegation, no new delegation on a stop or a switch to manual, no half delegation and
// none on an unbound root. A transition the machine has is accepted.
func TestDatabaseRefusesIllegalControlChanges(t *testing.T) {
	c := newControlWorld(t)
	c.bind("quote.1", "task.1", 0)
	set := func(sql string) error {
		return c.exec("company.A", `UPDATE brant_trd.quote_root SET `+sql+`, control_changed_at = now() WHERE quote_id = 'quote.1'`)
	}
	for _, sql := range []string{
		`control_state = 'manual', control_epoch = 2`,
		`control_state = 'stopping', control_epoch = 3`,
		`control_state = 'unbound', control_epoch = 2`,
		`control_epoch = 2, control_task_id = 'task.2'`,
		`control_epoch = 2`,
		`control_state = 'stopping', control_epoch = 2, control_initiator_epoch = control_initiator_epoch + 1`,
		`control_state = 'stopping', control_epoch = 2, control_agent_id = NULL`,
		`control_state = 'stopping', control_epoch = 2, control_agent_id = control_initiator_id`,
		`control_state = 'paused', control_epoch = 2`,
	} {
		if err := set(sql); err == nil {
			t.Errorf("the database accepted %s", sql)
		}
	}
	c.unchanged(trdapi.ControlBound, 1)
	if err := set(`control_state = 'stopping', control_epoch = 2`); err != nil {
		t.Fatalf("a stop: %v", err)
	}
	for _, sql := range []string{
		`control_state = 'bound', control_epoch = 3, control_initiator_id = NULL, control_initiator_epoch = NULL, control_agent_id = NULL,
			control_agent_epoch = NULL`,
		`control_state = 'manual', control_epoch = 3, control_agent_epoch = control_agent_epoch + 1`,
	} {
		if err := set(sql); err == nil {
			t.Errorf("the database accepted %s", sql)
		}
	}
	if err := set(`control_state = 'manual', control_epoch = 3`); err != nil {
		t.Fatalf("a switch to manual: %v", err)
	}
	if v := c.getQuote("quote.1"); v.Control.State != trdapi.ControlManual || v.Control.ControlEpoch != 3 || v.Control.TaskID != "task.1" {
		t.Fatalf("control %+v", v.Control)
	}
	if err := set(`control_state = 'unbound', control_epoch = 4, control_task_id = NULL, control_authority_epoch = NULL`); err == nil {
		t.Error("the database accepted an unbound root with a delegation")
	}
	err := set(`control_state = 'unbound', control_epoch = 4, control_task_id = NULL, control_authority_epoch = NULL,
		control_initiator_id = NULL, control_initiator_epoch = NULL, control_agent_id = NULL, control_agent_epoch = NULL`)
	if err != nil {
		t.Fatalf("a release: %v", err)
	}
}
