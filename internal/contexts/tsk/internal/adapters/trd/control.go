// Package trd is TSK's port to TRD's object control (Context Map R17 ObjectControl commands v1)
// over TRD's published api. TSK calls TRD only through it, before its own transaction.
package trd

import (
	"context"
	"fmt"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/application"
)

// TRD is what TSK calls in TRD: the object control commands and the quote read that tells the
// control epoch a root has before TSK binds it.
type TRD interface {
	BindControl(ctx context.Context, cmd trdapi.BindControl) (trdapi.Receipt, error)
	StopControl(ctx context.Context, cmd trdapi.StopControl) (trdapi.Receipt, error)
	ReleaseControl(ctx context.Context, cmd trdapi.ReleaseControl) (trdapi.Receipt, error)
	GetQuote(ctx context.Context, quoteID string) (trdapi.QuoteView, error)
}

// Control is application.ObjectControl over TRD. Stop and release carry the task's claim (TaskID
// and the control epoch the task recorded) in their context, as TRD requires of the bound task (S4
// D-4); the bind carries none, the root being unbound. object_control.enter_manual and
// object_control.handoff are not published by TRD before S4 unit A2: they answer
// DEPENDENCY_NOT_SUPPORTED until they are bound to A2's commands, and the bind names the delegation
// (ControlRequest.Initiator and Agent) from A2's BindControl on (D-5).
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
		ExpectedControlEpoch: q.Control.ControlEpoch})
	return epoch(rec, err, trdapi.ControlBound)
}

// Stop fences the task out of its root (bound → stopping).
func (c Control) Stop(ctx context.Context, r application.ControlRequest) (int64, error) {
	ctx, err := claim(ctx, r)
	if err != nil {
		return 0, err
	}
	rec, err := c.TRD.StopControl(ctx, trdapi.StopControl{IdempotencyKey: r.Key, QuoteID: r.QuoteID, ExpectedControlEpoch: r.Epoch,
		Reason: r.Reason})
	return epoch(rec, err, trdapi.ControlStopping)
}

// Release gives the root back (→ unbound).
func (c Control) Release(ctx context.Context, r application.ControlRequest) (int64, error) {
	ctx, err := claim(ctx, r)
	if err != nil {
		return 0, err
	}
	rec, err := c.TRD.ReleaseControl(ctx, trdapi.ReleaseControl{IdempotencyKey: r.Key, QuoteID: r.QuoteID, ExpectedControlEpoch: r.Epoch,
		Reason: r.Reason})
	return epoch(rec, err, trdapi.ControlUnbound)
}

// EnterManual is object_control.enter_manual (stopping → manual), which TRD does not publish yet.
func (Control) EnterManual(context.Context, application.ControlRequest) (int64, error) {
	return 0, api.Reject(api.CodeDependencyNotSupported, "/", "TRD object_control.enter_manual is not wired")
}

// Handoff is object_control.handoff (stopping → bound), which TRD does not publish yet.
func (Control) Handoff(context.Context, application.ControlRequest) (int64, error) {
	return 0, api.Reject(api.CodeDependencyNotSupported, "/", "TRD object_control.handoff is not wired")
}

func claim(ctx context.Context, r application.ControlRequest) (context.Context, error) {
	return trdapi.WithControl(ctx, trdapi.ControlClaim{TaskID: r.TaskID, ControlEpoch: r.Epoch})
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
