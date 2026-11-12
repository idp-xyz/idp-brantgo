package quote

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Object control of a quote root (SM-TRD-CONTROL, TRD-INV-015; Python t03_storage guard, bind, stop
// and unbind; WBS S3-08). TRD is the only writer of the control state and its control epoch, which
// grows by one with every bind, stop and release:
//
//	unbound  → bound     object_control.bind: a task claims the root (CAS on the control epoch)
//	bound    → stopping  object_control.stop: a human fences the task out at once
//	bound, stopping → unbound  object_control.release: a human gives the root back
//
// While the root is bound, every write must claim the binding — its task and current control epoch
// — and the company's authority epoch must still be the one it was bound under (Python
// delegate_epoch: a change of access in the company ends what the binding delegated). While it is
// stopping, nothing is written. An unbound root takes writes that claim nothing, under the
// command's permissions alone. The claim reaches TRD through the trusted adapter (api.WithControl),
// never through a request body.
//
// SM-TRD-CONTROL's manual state (stopping → manual when TSK's barrier completes) and TSK's checks on
// release (the task completed, closed or cancelled; no submitted or unknown result in flight) belong
// to the TSK integration (Context Map R17, S4–S5); until then a human releases a stopping root
// directly, as Python unbind does.

// ControlState is the state of a root's object control.
type ControlState string

const (
	Unbound  ControlState = "unbound"
	Bound    ControlState = "bound"
	Stopping ControlState = "stopping"
)

// Control is the object control of a root.
type Control struct {
	State          ControlState
	Epoch          int64  // control epoch: 0 until the first bind
	TaskID         string // bound and stopping: the task holding the binding
	AuthorityEpoch int64  // bound and stopping: the company's authority epoch at the bind
	By             kernel.ActorRef
	At             time.Time
	Changed        bool
}

// Claim is the object control a write claims: the task it acts for and the control epoch of its
// binding.
type Claim struct {
	TaskID string
	Epoch  int64
}

// Admit decides whether a write may proceed on the root (TRD-INV-015). claim is what the write
// claims (nil: nothing) and authority the company's authority epoch now. A claim of another control
// epoch than the current one is CONTROL_EPOCH_STALE: its binding was stopped, released or replaced.
// Otherwise a bound root needs a claim of its task under an unchanged authority epoch, and a
// stopping root takes no write (OBJECT_CONTROL_CONFLICT).
func (c Control) Admit(claim *Claim, authority int64) error {
	const path = "/"
	if claim != nil && claim.Epoch != c.Epoch {
		return fail(CodeControlEpochStale, path, "The write claims an earlier control epoch")
	}
	switch c.State {
	case Unbound:
		if claim != nil {
			return fail(CodeControlEpochStale, path, "The root is not bound")
		}
		return nil
	case Bound:
		switch {
		case claim == nil:
			return fail(CodeObjectControlConflict, path, "The root is bound to a task")
		case claim.TaskID != c.TaskID:
			return fail(CodeObjectControlConflict, path, "The root is bound to another task")
		case authority != c.AuthorityEpoch:
			return fail(CodeObjectControlConflict, path, "Access changed since the root was bound")
		}
		return nil
	}
	return fail(CodeObjectControlConflict, path, "The root's control is stopping")
}

// Bind binds the root to task at expected control epoch (object_control.bind): VERSION_CONFLICT at
// another epoch, OBJECT_CONTROL_CONFLICT while it is bound or stopping.
func (q *Quote) Bind(task string, expected int64, by kernel.ActorRef, authority int64, now time.Time) error {
	c := &q.Control
	if c.Epoch != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_control_epoch", "")
	}
	if c.State != Unbound {
		return fail(CodeObjectControlConflict, "/args/quote_id", "The root is bound to a task")
	}
	*c = Control{State: Bound, Epoch: c.Epoch + 1, TaskID: task, AuthorityEpoch: authority, By: by, At: kernel.Instant(now), Changed: true}
	return nil
}

// Stop fences the bound task out at expected control epoch (object_control.stop): the epoch moves
// and every write is refused until the root is released.
func (q *Quote) Stop(expected int64, by kernel.ActorRef, now time.Time) error {
	c := &q.Control
	if c.Epoch != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_control_epoch", "")
	}
	if c.State != Bound {
		return kernel.Reject(kernel.CodeInvalidState, "/args/quote_id", "Only a bound root is stopped")
	}
	c.State, c.Epoch, c.By, c.At, c.Changed = Stopping, c.Epoch+1, by, kernel.Instant(now), true
	return nil
}

// Release gives a bound or stopping root back at expected control epoch (object_control.release).
func (q *Quote) Release(expected int64, by kernel.ActorRef, now time.Time) error {
	c := &q.Control
	if c.Epoch != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_control_epoch", "")
	}
	if c.State == Unbound {
		return kernel.Reject(kernel.CodeInvalidState, "/args/quote_id", "The root is not bound")
	}
	*c = Control{State: Unbound, Epoch: c.Epoch + 1, By: by, At: kernel.Instant(now), Changed: true}
	return nil
}
