package quote

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Object control of a quote root (SM-TRD-CONTROL, TRD-INV-015, TSK-INV-004; Python t03_storage guard,
// bind, stop and unbind; Q01 V0.1 store.py take_control and finish_control; WBS S3-08, S4-01b). TRD
// is the only writer of the control state and its control epoch, which grows by one with every
// change:
//
//	unbound          → bound     object_control.bind: TSK claims the root for a task (CAS on the
//	                             control epoch) and names the task's delegation
//	bound, manual    → stopping  object_control.stop: a human takes control or hands it back; the
//	                             fence is effective at once
//	stopping         → manual    object_control.enter_manual: the barrier completed on a takeover
//	stopping         → bound     object_control.handoff: the barrier completed on a hand-back to the
//	                             same task's AI; the delegation is named again
//	bound, manual, stopping → unbound  object_control.release: TSK gives the root back
//
// Every change but the bind needs the bound task's claim at the current control epoch (S4 decision
// D-4): only TSK, which holds the preconditions (a terminal task, no submitted or unknown result in
// flight, a completed barrier), can stop, switch, hand back or release a bound root; a direct call
// is OBJECT_CONTROL_CONFLICT.
//
// What a write needs (Admit; decision D-3): on an unbound root nothing but its permissions; on a
// bound root the bound task's claim, from its AI agent or a human, while the binding's delegation
// holds — the grants of the task's initiator and agent unchanged since the bind or handoff (decision
// D-5, which replaces S3-D12's company authority epoch); on a manual root the bound task's claim
// from a human (CONTROL_MODE_MISMATCH for an AI agent); on a stopping root nothing. The claim reaches
// TRD through the trusted adapter (api.WithControl), never through a request body.

// ControlState is the state of a root's object control.
type ControlState string

const (
	Unbound  ControlState = "unbound"
	Bound    ControlState = "bound"
	Stopping ControlState = "stopping"
	Manual   ControlState = "manual"
)

// Control is the object control of a root.
type Control struct {
	State ControlState
	Epoch int64 // control epoch: 0 until the first bind
	// TaskID is the task holding the binding while the root is not unbound.
	TaskID string
	// Delegation is what the binding delegates (bound, stopping and manual roots bound from TRD
	// 000009 on); the last bind or handoff recorded it.
	Delegation Delegation
	// AuthorityEpoch is the company's authority epoch the last bind or handoff was decided under:
	// recorded, no longer compared (decision D-5 replaced S3-D12's fence on it).
	AuthorityEpoch int64
	By             kernel.ActorRef
	At             time.Time
	Changed        bool
}

// Delegation is the delegation a binding holds under (decision D-5): the task's initiator, the
// human it acts for, and its AI agent, each with the authority epoch at which its grant last
// changed (authorization.Holding.ChangedEpoch) when the binding was made.
type Delegation struct {
	Initiator      string
	InitiatorEpoch int64
	Agent          string
	AgentEpoch     int64
}

// Grant is an actor's grant as it is now: its kind, whether it is active, and the authority epoch
// of its last change.
type Grant struct {
	Kind         kernel.ActorKind
	Active       bool
	ChangedEpoch int64
}

// Grants reads an actor's grant now (in TRD's transaction, through the authority guard rows).
type Grants func(actorID string) (Grant, error)

// Claim is the object control a write claims: the task it acts for and the control epoch of its
// binding.
type Claim struct {
	TaskID string
	Epoch  int64
}

// Write is a write as the fence meets it: the claim it carries (nil: none), the kind of its actor,
// and the grants as they are now, read only when a bound root needs its delegation checked.
type Write struct {
	Claim  *Claim
	Actor  kernel.ActorKind
	Grants Grants
}

// Admit decides whether write w may proceed on the root (TRD-INV-015, decision D-3). A claim of
// another control epoch than the current one is CONTROL_EPOCH_STALE: its binding was stopped,
// switched, handed back, released or replaced. Otherwise a bound root needs the bound task's claim
// under an unchanged delegation (OBJECT_CONTROL_CONFLICT), a manual root the bound task's claim
// (OBJECT_CONTROL_CONFLICT) from a human (CONTROL_MODE_MISMATCH), and a stopping root takes no
// write (OBJECT_CONTROL_CONFLICT).
func (c Control) Admit(w Write) error {
	const path = "/"
	if w.Claim != nil && w.Claim.Epoch != c.Epoch {
		return fail(CodeControlEpochStale, path, "The write claims an earlier control epoch")
	}
	switch c.State {
	case Unbound:
		if w.Claim != nil {
			return fail(CodeControlEpochStale, path, "The root is not bound")
		}
		return nil
	case Bound, Manual:
		if w.Claim == nil {
			return fail(CodeObjectControlConflict, path, "The root is bound to a task")
		}
		if w.Claim.TaskID != c.TaskID {
			return fail(CodeObjectControlConflict, path, "The root is bound to another task")
		}
		if c.State == Manual {
			if w.Actor != kernel.ActorHuman {
				return fail(CodeControlModeMismatch, path, "The root is under manual control: only humans write")
			}
			return nil
		}
		holds, err := c.Delegation.holds(w.Grants)
		if err != nil {
			return err
		}
		if !holds {
			return fail(CodeObjectControlConflict, path, "Access changed since the root was bound")
		}
		return nil
	}
	return fail(CodeObjectControlConflict, path, "The root's control is stopping")
}

// holds reports whether the delegation still holds: the initiator's and the agent's grants are
// active and unchanged. A binding that names no delegation (made before TRD 000009) never holds.
func (d Delegation) holds(grants Grants) (bool, error) {
	if d.Initiator == "" || grants == nil {
		return false, nil
	}
	for _, x := range []struct {
		id    string
		epoch int64
	}{{d.Initiator, d.InitiatorEpoch}, {d.Agent, d.AgentEpoch}} {
		g, err := grants(x.id)
		if err != nil || !g.Active || g.ChangedEpoch != x.epoch {
			return false, err
		}
	}
	return true, nil
}

// delegate reads the grants of initiator and agent now: the initiator must hold an active human
// grant and the agent an active AI grant (REFERENCE_INVALID at their fields otherwise).
func delegate(initiator, agent string, grants Grants) (Delegation, error) {
	d := Delegation{Initiator: initiator, Agent: agent}
	for _, x := range []struct {
		id, path string
		kind     kernel.ActorKind
		epoch    *int64
	}{{initiator, "/args/initiator_id", kernel.ActorHuman, &d.InitiatorEpoch}, {agent, "/args/agent_id", kernel.ActorAI, &d.AgentEpoch}} {
		g, err := grants(x.id)
		if err != nil {
			return Delegation{}, err
		}
		if !g.Active || g.Kind != x.kind {
			return Delegation{}, kernel.Reject(kernel.CodeReferenceInvalid, x.path, "Not an active "+string(x.kind)+" grant")
		}
		*x.epoch = g.ChangedEpoch
	}
	return d, nil
}

// Binding is what object_control.bind and object_control.handoff name: the task's initiator and AI
// agent, by actor ID.
type Binding struct {
	Initiator string
	Agent     string
}

// Bind binds the root to task at expected control epoch (object_control.bind) under the delegation
// b names, whose grants it reads now; authority is the company's authority epoch, recorded.
// VERSION_CONFLICT at another epoch, OBJECT_CONTROL_CONFLICT while the root is not unbound. No claim
// is needed: nothing is bound yet.
func (q *Quote) Bind(task string, b Binding, expected int64, by kernel.ActorRef, authority int64, grants Grants, now time.Time) error {
	c := &q.Control
	if c.Epoch != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_control_epoch", "")
	}
	if c.State != Unbound {
		return fail(CodeObjectControlConflict, "/args/quote_id", "The root is bound to a task")
	}
	d, err := delegate(b.Initiator, b.Agent, grants)
	if err != nil {
		return err
	}
	*c = Control{State: Bound, Epoch: c.Epoch + 1, TaskID: task, Delegation: d, AuthorityEpoch: authority, By: by,
		At: kernel.Instant(now), Changed: true}
	return nil
}

// change checks what every control change but the bind needs (decision D-4): claim is the bound
// task's claim at the current control epoch (CONTROL_EPOCH_STALE for another epoch or an unbound
// root, OBJECT_CONTROL_CONFLICT for none or another task's; INVALID_STATE on an unbound root without
// a claim), then expected is the current epoch (VERSION_CONFLICT), then the root is in one of from
// (INVALID_STATE).
func (c Control) change(claim *Claim, expected int64, from ...ControlState) error {
	const path = "/"
	if claim != nil && claim.Epoch != c.Epoch {
		return fail(CodeControlEpochStale, path, "The change claims an earlier control epoch")
	}
	switch {
	case c.State == Unbound && claim != nil:
		return fail(CodeControlEpochStale, path, "The root is not bound")
	case c.State == Unbound:
		return kernel.Reject(kernel.CodeInvalidState, "/args/quote_id", "The root is not bound")
	case claim == nil:
		return fail(CodeObjectControlConflict, path, "Only the bound task changes the root's control")
	case claim.TaskID != c.TaskID:
		return fail(CodeObjectControlConflict, path, "The root is bound to another task")
	case c.Epoch != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_control_epoch", "")
	}
	for _, s := range from {
		if c.State == s {
			return nil
		}
	}
	return kernel.Reject(kernel.CodeInvalidState, "/args/quote_id", "The control is "+string(c.State))
}

// moved records a control change to state by by at now: the epoch grows by one.
func (c *Control) moved(state ControlState, by kernel.ActorRef, now time.Time) {
	c.State, c.Epoch, c.By, c.At, c.Changed = state, c.Epoch+1, by, kernel.Instant(now), true
}

// Stop fences the bound task out of a bound or manual root for the bound task's claim at expected
// control epoch (object_control.stop): the epoch moves and every write is refused until the barrier
// completes.
func (q *Quote) Stop(claim *Claim, expected int64, by kernel.ActorRef, now time.Time) error {
	if err := q.Control.change(claim, expected, Bound, Manual); err != nil {
		return err
	}
	q.Control.moved(Stopping, by, now)
	return nil
}

// EnterManual puts a stopping root under manual control for the bound task's claim at expected
// control epoch (object_control.enter_manual): from now on only humans write, with the task's claim
// of the new epoch.
func (q *Quote) EnterManual(claim *Claim, expected int64, by kernel.ActorRef, now time.Time) error {
	if err := q.Control.change(claim, expected, Stopping); err != nil {
		return err
	}
	q.Control.moved(Manual, by, now)
	return nil
}

// Handoff binds a stopping root to the same task again for the bound task's claim at expected
// control epoch (object_control.handoff), under the delegation b names, whose grants it reads now;
// authority is the company's authority epoch, recorded.
func (q *Quote) Handoff(claim *Claim, b Binding, expected int64, by kernel.ActorRef, authority int64, grants Grants, now time.Time) error {
	c := &q.Control
	if err := c.change(claim, expected, Stopping); err != nil {
		return err
	}
	d, err := delegate(b.Initiator, b.Agent, grants)
	if err != nil {
		return err
	}
	c.Delegation, c.AuthorityEpoch = d, authority
	c.moved(Bound, by, now)
	return nil
}

// Release gives a bound, manual or stopping root back for the bound task's claim at expected
// control epoch (object_control.release).
func (q *Quote) Release(claim *Claim, expected int64, by kernel.ActorRef, now time.Time) error {
	c := &q.Control
	if err := c.change(claim, expected, Bound, Manual, Stopping); err != nil {
		return err
	}
	*c = Control{State: Unbound, Epoch: c.Epoch + 1, By: by, At: kernel.Instant(now), Changed: true}
	return nil
}
