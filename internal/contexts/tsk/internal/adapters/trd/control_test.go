package trd

import (
	"context"
	"errors"
	"slices"
	"testing"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/application"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TSK's port to TRD's object control (Context Map R17; S4 C1) against a fake TRD api that keeps one
// root's control as TRD does: every change needs the bound task's claim at the current epoch
// (CONTROL_EPOCH_STALE for another epoch, OBJECT_CONTROL_CONFLICT for another task), moves the epoch
// by one and is stored under its key, whose replay is not fenced.

type fakeControl struct {
	state, task string
	epoch       int64
	keys        map[string]trdapi.Receipt
	sent        []string // command@claim epoch/expected epoch
	reads       int
	refuseRead  error
	// binds holds the binds TRD committed, by key; reconciled counts the reconciliations asked.
	binds      map[string]trdapi.BindControl
	reconciled int
	refuseRec  error
}

func (f *fakeControl) change(ctx context.Context, key, command string, expected int64, to string) (trdapi.Receipt, error) {
	c, _ := trdapi.ControlFromContext(ctx)
	f.sent = append(f.sent, command+"@"+itoa(c.ControlEpoch)+"/"+itoa(expected))
	if r, ok := f.keys[key]; ok {
		r.Replayed = true
		return r, nil
	}
	switch {
	case c.ControlEpoch != f.epoch:
		return trdapi.Receipt{}, kernel.RejectAs(trdapi.CodeControlEpochStale, kernel.CategoryConflict, "/", "")
	case f.state == trdapi.ControlUnbound || c.TaskID != f.task:
		return trdapi.Receipt{}, kernel.RejectAs(trdapi.CodeObjectControlConflict, kernel.CategoryConflict, "/", "")
	case expected != f.epoch:
		return trdapi.Receipt{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_control_epoch", "")
	}
	f.epoch++
	f.state = to
	r := trdapi.Receipt{ActionID: "act." + key, ObjectID: "quote.1", RowVersion: f.epoch, Status: to}
	f.keys[key] = r
	return r, nil
}

func itoa(n int64) string { return string(rune('0' + n)) }

func (f *fakeControl) BindControl(context.Context, trdapi.BindControl) (trdapi.Receipt, error) {
	return trdapi.Receipt{}, errors.New("not used")
}

func (f *fakeControl) StopControl(ctx context.Context, c trdapi.StopControl) (trdapi.Receipt, error) {
	return f.change(ctx, c.IdempotencyKey, "stop", c.ExpectedControlEpoch, trdapi.ControlStopping)
}

func (f *fakeControl) EnterManualControl(ctx context.Context, c trdapi.EnterManualControl) (trdapi.Receipt, error) {
	return f.change(ctx, c.IdempotencyKey, "enter_manual", c.ExpectedControlEpoch, trdapi.ControlManual)
}

func (f *fakeControl) HandoffControl(ctx context.Context, c trdapi.HandoffControl) (trdapi.Receipt, error) {
	return f.change(ctx, c.IdempotencyKey, "handoff", c.ExpectedControlEpoch, trdapi.ControlBound)
}

func (f *fakeControl) ReleaseControl(ctx context.Context, c trdapi.ReleaseControl) (trdapi.Receipt, error) {
	r, err := f.change(ctx, c.IdempotencyKey, "release", c.ExpectedControlEpoch, trdapi.ControlUnbound)
	if err == nil && !r.Replayed {
		f.task = ""
	}
	return r, err
}

func (f *fakeControl) GetQuote(context.Context, string) (trdapi.QuoteView, error) {
	f.reads++
	return trdapi.QuoteView{QuoteID: "quote.1", Control: trdapi.ControlView{State: f.state, TaskID: f.task, ControlEpoch: f.epoch}}, f.refuseRead
}

// Reconcile answers from the binds TRD committed, as TRD's ledger does: committed for the same
// request under the key (its expected epoch is outside the request hash), payload_conflict for
// another, not_committed for none.
func (f *fakeControl) Reconcile(_ context.Context, cmd trdapi.Command) (trdapi.Reconciliation, error) {
	f.reconciled++
	if f.refuseRec != nil {
		return trdapi.Reconciliation{}, f.refuseRec
	}
	c, ok := cmd.(trdapi.BindControl)
	if !ok {
		return trdapi.Reconciliation{}, errors.New("only binds are reconciled here")
	}
	stored, ok := f.binds[c.IdempotencyKey]
	if !ok {
		return trdapi.Reconciliation{State: kernel.ReconcileNotCommitted}, nil
	}
	c.ExpectedControlEpoch, stored.ExpectedControlEpoch = 0, 0
	if c != stored {
		return trdapi.Reconciliation{State: kernel.ReconcilePayloadConflict, ActionID: "act." + c.IdempotencyKey, ObjectID: stored.QuoteID}, nil
	}
	r := trdapi.Receipt{ActionID: "act." + c.IdempotencyKey, ObjectID: c.QuoteID, RowVersion: 1, Status: trdapi.ControlBound}
	return trdapi.Reconciliation{State: kernel.ReconcileCommitted, ActionID: r.ActionID, ObjectID: r.ObjectID, Receipt: &r}, nil
}

func request(key string, epoch int64) application.ControlRequest {
	return application.ControlRequest{Key: key, TaskID: "task.1", QuoteID: "quote.1", Epoch: epoch, Reason: application.ControlReason("task.1", epoch),
		Initiator: "clerk.1", Agent: "agent.1"}
}

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code
	}
	if err != nil {
		return "ERROR"
	}
	return "OK"
}

// S4 B2-D3 and A1's two-step take-over (S4 C1): a take-over stopped the root at TRD (epoch 2) and its
// record in TSK was lost; the task then ended at epoch 1. task.release learns TRD's epoch for the
// task's own binding: TRD refuses the claim of epoch 1 (CONTROL_EPOCH_STALE), the port reads the root,
// finds it held by the task at epoch 2 and sends the release again at epoch 2 under the same key.
// Retried after a lost answer, the release replays TRD's result under that key.
func TestControlLearnsTRDsEpochForTheTasksOwnBinding(t *testing.T) {
	f := &fakeControl{state: trdapi.ControlStopping, task: "task.1", epoch: 2, keys: map[string]trdapi.Receipt{}}
	c := Control{TRD: f}
	key := application.DeriveKey("task.1", trdapi.CommandReleaseControl, 1)
	got, err := c.Release(context.Background(), request(key, 1))
	if err != nil || got != 3 || f.state != trdapi.ControlUnbound || f.reads != 1 || !slices.Equal(f.sent, []string{"release@1/1", "release@2/2"}) {
		t.Fatalf("%d %v %+v", got, err, f)
	}
	again, err := c.Release(context.Background(), request(key, 1))
	if err != nil || again != 3 || f.epoch != 3 || f.reads != 1 {
		t.Fatalf("the retry: %d %v %+v", again, err, f)
	}
}

// The port learns only what TRD shows of the task's own binding: a root unbound or held by another
// task, or at the recorded epoch, keeps TRD's refusal; a refused read keeps it too; other refusals are
// passed on without a read; a change at the recorded epoch needs no read.
func TestControlKeepsTRDsRefusalUnlessTheBindingIsTheTasks(t *testing.T) {
	for name, f := range map[string]*fakeControl{
		"unbound":       {state: trdapi.ControlUnbound, epoch: 3},
		"another task":  {state: trdapi.ControlStopping, task: "task.2", epoch: 3},
		"refused read":  {state: trdapi.ControlStopping, task: "task.1", epoch: 3, refuseRead: kernel.Reject(kernel.CodePermissionDenied, "/", "")},
		"earlier epoch": {state: trdapi.ControlManual, task: "task.1", epoch: 1},
	} {
		f.keys = map[string]trdapi.Receipt{}
		c := Control{TRD: f}
		r := request(application.DeriveKey("task.1", trdapi.CommandStopControl, 2), 2)
		if _, err := c.Stop(context.Background(), r); code(err) != trdapi.CodeControlEpochStale || f.reads != 1 || len(f.sent) != 1 {
			t.Errorf("%s: %s, %d reads, sent %v", name, code(err), f.reads, f.sent)
		}
	}
	f := &fakeControl{state: trdapi.ControlBound, task: "task.2", epoch: 2, keys: map[string]trdapi.Receipt{}}
	if _, err := (Control{TRD: f}).Stop(context.Background(), request("tsk.k1", 2)); code(err) != trdapi.CodeObjectControlConflict || f.reads != 0 {
		t.Fatalf("another task's root: %s, %d reads", code(err), f.reads)
	}
	f = &fakeControl{state: trdapi.ControlBound, task: "task.1", epoch: 2, keys: map[string]trdapi.Receipt{}}
	got, err := (Control{TRD: f}).Stop(context.Background(), request("tsk.k2", 2))
	if err != nil || got != 3 || f.reads != 0 || !slices.Equal(f.sent, []string{"stop@2/2"}) {
		t.Fatalf("at the recorded epoch: %d %v %+v", got, err, f)
	}
	for _, step := range []func(Control, context.Context, application.ControlRequest) (int64, error){
		Control.EnterManual, Control.Handoff,
	} {
		f = &fakeControl{state: trdapi.ControlStopping, task: "task.1", epoch: 4, keys: map[string]trdapi.Receipt{}}
		if got, err := step(Control{TRD: f}, context.Background(), request("tsk.k3", 3)); err != nil || got != 5 || f.reads != 1 {
			t.Errorf("%d %v %+v", got, err, f)
		}
	}
}

// S4 C3 (C1's gap "lost bind record"): TRD's object_control.bind committed and TSK's record of it was
// lost. Bound learns it from TRD's ledger by the bind TSK sent — the same key, root, task and
// delegation, whatever expected epoch — and answers the epoch TRD bound the root at; nothing under
// the key, another request under it (another root) or another delegation answer 0; TRD's refusal of
// the read passes on. Nothing is executed at TRD.
func TestBoundLearnsALostBindFromTRDsLedger(t *testing.T) {
	key := application.DeriveKey("task.1", trdapi.CommandBindControl, 0)
	f := &fakeControl{state: trdapi.ControlBound, task: "task.1", epoch: 1, keys: map[string]trdapi.Receipt{},
		binds: map[string]trdapi.BindControl{key: {IdempotencyKey: key, QuoteID: "quote.1", TaskID: "task.1", InitiatorID: "clerk.1",
			AgentID: "agent.1", ExpectedControlEpoch: 0}}}
	c := Control{TRD: f}
	ask := func(key, quote, initiator string) (int64, error) {
		r := request(key, 0)
		r.QuoteID, r.Initiator = quote, initiator
		return c.Bound(context.Background(), r)
	}
	if e, err := ask(key, "quote.1", "clerk.1"); err != nil || e != 1 {
		t.Fatalf("bound at %d: %v", e, err)
	}
	for name, got := range map[string][2]any{
		"another key":       pair(ask(application.DeriveKey("task.1", trdapi.CommandBindControl, 3), "quote.1", "clerk.1")),
		"another root":      pair(ask(key, "quote.2", "clerk.1")),
		"another initiator": pair(ask(key, "quote.1", "clerk.2")),
	} {
		if got[0] != int64(0) || got[1] != nil {
			t.Errorf("%s: %v", name, got)
		}
	}
	f.refuseRec = kernel.Reject(kernel.CodePermissionDenied, "/", "")
	if _, err := ask(key, "quote.1", "clerk.1"); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("refused read: %v", err)
	}
	if len(f.sent) != 0 || f.reconciled != 5 {
		t.Fatalf("sent %v, reconciled %d", f.sent, f.reconciled)
	}
}

func pair(e int64, err error) [2]any { return [2]any{e, err} }
