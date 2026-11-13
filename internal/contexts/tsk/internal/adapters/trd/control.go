// Package trd is TSK's port to TRD's object control (Context Map R17 ObjectControl commands v1)
// over TRD's published api. TSK calls TRD only through it, before its own transaction.
package trd

import (
	"context"
	"fmt"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/application"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TRD is what TSK calls in TRD: the object control commands and the quote read that tells the
// control epoch a root has before TSK binds it.
type TRD interface {
	BindControl(ctx context.Context, cmd trdapi.BindControl) (trdapi.Receipt, error)
	StopControl(ctx context.Context, cmd trdapi.StopControl) (trdapi.Receipt, error)
	EnterManualControl(ctx context.Context, cmd trdapi.EnterManualControl) (trdapi.Receipt, error)
	HandoffControl(ctx context.Context, cmd trdapi.HandoffControl) (trdapi.Receipt, error)
	ReleaseControl(ctx context.Context, cmd trdapi.ReleaseControl) (trdapi.Receipt, error)
	GetQuote(ctx context.Context, quoteID string) (trdapi.QuoteView, error)
}

// Control is application.ObjectControl over TRD. Stop, enter_manual, handoff and release carry the
// task's claim (TaskID and the control epoch the task recorded) in their context, as TRD requires of
// the bound task (S4 D-4); the bind carries none, the root being unbound. The bind and the hand-back
// name the task's delegation (ControlRequest.Initiator and Agent; D-5). A change TRD refuses as
// CONTROL_EPOCH_STALE while it holds the root for the same task at a later epoch is sent again at
// that epoch (send).
type Control struct {
	TRD TRD
}

var _ application.ObjectControl = Control{}

// Bind binds r.QuoteID to r.TaskID at the root's current control epoch, read first; a retry of the
// same task at the same recorded epoch sends the same key, which TRD answers from its first result
// (the expected epoch is not part of TRD's request hash).
func (c Control) Bind(ctx context.Context, r application.ControlRequest) (int64, error) {
	q, err := c.TRD.GetQuote(ctx, r.QuoteID)
	if err != nil {
		return 0, err
	}
	rec, err := c.TRD.BindControl(ctx, trdapi.BindControl{IdempotencyKey: r.Key, QuoteID: r.QuoteID, TaskID: r.TaskID,
		InitiatorID: r.Initiator, AgentID: r.Agent, ExpectedControlEpoch: q.Control.ControlEpoch})
	return epoch(rec, err, trdapi.ControlBound)
}

// Stop fences the task out of its root (bound → stopping).
func (c Control) Stop(ctx context.Context, r application.ControlRequest) (int64, error) {
	return c.send(ctx, r, trdapi.ControlStopping, func(ctx context.Context, expected int64) (trdapi.Receipt, error) {
		return c.TRD.StopControl(ctx, trdapi.StopControl{IdempotencyKey: r.Key, QuoteID: r.QuoteID, ExpectedControlEpoch: expected, Reason: r.Reason})
	})
}

// Release gives the root back (→ unbound).
func (c Control) Release(ctx context.Context, r application.ControlRequest) (int64, error) {
	return c.send(ctx, r, trdapi.ControlUnbound, func(ctx context.Context, expected int64) (trdapi.Receipt, error) {
		return c.TRD.ReleaseControl(ctx, trdapi.ReleaseControl{IdempotencyKey: r.Key, QuoteID: r.QuoteID, ExpectedControlEpoch: expected,
			Reason: r.Reason})
	})
}

// EnterManual puts the stopping root under manual control (stopping → manual).
func (c Control) EnterManual(ctx context.Context, r application.ControlRequest) (int64, error) {
	return c.send(ctx, r, trdapi.ControlManual, func(ctx context.Context, expected int64) (trdapi.Receipt, error) {
		return c.TRD.EnterManualControl(ctx, trdapi.EnterManualControl{IdempotencyKey: r.Key, QuoteID: r.QuoteID, ExpectedControlEpoch: expected,
			Reason: r.Reason})
	})
}

// Handoff binds the stopping root to its task again under the task's delegation (stopping → bound).
func (c Control) Handoff(ctx context.Context, r application.ControlRequest) (int64, error) {
	return c.send(ctx, r, trdapi.ControlBound, func(ctx context.Context, expected int64) (trdapi.Receipt, error) {
		return c.TRD.HandoffControl(ctx, trdapi.HandoffControl{IdempotencyKey: r.Key, QuoteID: r.QuoteID, ExpectedControlEpoch: expected,
			InitiatorID: r.Initiator, AgentID: r.Agent, Reason: r.Reason})
	})
}

// send sends a control change of r's task's root with the task's claim at r.Epoch, the epoch the task
// recorded, as the expected epoch too. TRD may have moved the task's own binding past that epoch: a
// control change TSK sent earlier for the task committed at TRD and its record in TSK was lost — a
// take-over's stop whose second step failed and that a goal verification, a pause or a close then
// overtook (S4 B2-D3, A1's two-step take-over). TRD then answers CONTROL_EPOCH_STALE; send reads the
// root's control and, when TRD shows the root still held by the same task at a later epoch, sends
// the change again at TRD's epoch — learning it from the fence authority, as TSK-INV-002 asks, never
// inventing one —, under the same key and reason, which stay those of the recorded epoch so that a
// retry after a lost answer replays TRD's result. A root that is unbound or held by another task is
// not the task's to change: the refusal passes on. The claims TSK issues to the executors of task
// steps never learn: they carry the recorded epoch, and TRD keeps refusing them.
func (c Control) send(ctx context.Context, r application.ControlRequest, state string,
	call func(ctx context.Context, expected int64) (trdapi.Receipt, error)) (int64, error) {
	claimed, err := claim(ctx, r.TaskID, r.Epoch)
	if err != nil {
		return 0, err
	}
	rec, err := call(claimed, r.Epoch)
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == trdapi.CodeControlEpochStale {
		if q, qerr := c.TRD.GetQuote(ctx, r.QuoteID); qerr == nil && q.Control.State != trdapi.ControlUnbound &&
			q.Control.TaskID == r.TaskID && q.Control.ControlEpoch > r.Epoch {
			if claimed, err = claim(ctx, r.TaskID, q.Control.ControlEpoch); err != nil {
				return 0, err
			}
			rec, err = call(claimed, q.Control.ControlEpoch)
		}
	}
	return epoch(rec, err, state)
}

func claim(ctx context.Context, taskID string, epoch int64) (context.Context, error) {
	return trdapi.WithControl(ctx, trdapi.ControlClaim{TaskID: taskID, ControlEpoch: epoch})
}

// epoch returns the control epoch of TRD's receipt, which must show the control state the command
// leads to.
func epoch(rec trdapi.Receipt, err error, state string) (int64, error) {
	if err != nil {
		return 0, err
	}
	if rec.Status != state || rec.RowVersion < 1 {
		return 0, fmt.Errorf("tsk: TRD answered control %s at epoch %d, want %s", rec.Status, rec.RowVersion, state)
	}
	return rec.RowVersion, nil
}
