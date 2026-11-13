package api

import (
	"context"
	"errors"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Object control of a quote root (SM-TRD-CONTROL, TRD-INV-015, TSK-INV-004; WBS S3-08, S4-01b;
// Context Map R17 TSK→TRD "ObjectControl commands v1"). TRD is the only writer of a root's control
// state and control epoch; TSK is the caller of the five commands. Every change increments the
// control epoch, which the commands compare (ExpectedControlEpoch, VERSION_CONFLICT otherwise):
//
//	object_control.bind          unbound → bound: the root is claimed for TaskID under the task's
//	                             delegation (InitiatorID, AgentID); another binding is
//	                             OBJECT_CONTROL_CONFLICT. Human or system, never AI.
//	object_control.stop          bound or manual → stopping: every write is refused from now on.
//	                             Human only.
//	object_control.enter_manual  stopping → manual: TSK's barrier completed on a takeover. Human or
//	                             system (TSK, as the acting human or itself), never AI.
//	object_control.handoff       stopping → bound: TSK's barrier completed on a hand-back to the same
//	                             task's AI; the delegation is named again. Human or system, never AI.
//	object_control.release       bound, manual or stopping → unbound. Human only.
//
// Every command but the bind needs the bound task's ControlClaim at the current control epoch in
// its context (S4 decision D-4): without one, or with another task's, it is OBJECT_CONTROL_CONFLICT;
// with an earlier epoch's, CONTROL_EPOCH_STALE. So only TSK, which holds the preconditions (a
// terminal task, no submitted or unknown result in flight, a completed barrier), stops, switches,
// hands back or releases a bound root. Their replays are not fenced: TSK retries a step by its key.
//
// Writes on a root (quote.save_draft, quote.revise, quote.discard_draft, quote.freeze,
// quote.metadata.update, the review, formalization, acceptance and order commands, and the replay of
// any of them) meet the fence as the root's control state says (decision D-3):
//
//	unbound   no claim: the command's permissions alone; a claim is CONTROL_EPOCH_STALE
//	bound     the bound task's claim, from its AI agent or a human, while the grants of the task's
//	          initiator and agent are as they were at the bind or handoff (decision D-5:
//	          authorization.Holding.ChangedEpoch; replaces S3-D12's company authority epoch);
//	          OBJECT_CONTROL_CONFLICT otherwise
//	manual    the bound task's claim (OBJECT_CONTROL_CONFLICT otherwise) from a human: an AI agent
//	          or the system is CONTROL_MODE_MISMATCH
//	stopping  nothing: OBJECT_CONTROL_CONFLICT (CONTROL_EPOCH_STALE for a claim of an earlier epoch)
//
// A claim of an earlier control epoch is CONTROL_EPOCH_STALE in every state. Reads and
// reconciliation are never fenced.
//
// The claim is put into the context by the trusted adapter that runs the task (TSK through
// internal/ai/execution, R17), never taken from a request body: no command struct has a task member
// but the bind's own task, so a transport cannot pass one on (Python
// test_task_identity_cannot_be_supplied_through_public_json).
const (
	CommandBindControl        = "object_control.bind"
	CommandStopControl        = "object_control.stop"
	CommandEnterManualControl = "object_control.enter_manual"
	CommandHandoffControl     = "object_control.handoff"
	CommandReleaseControl     = "object_control.release"

	ControlUnbound  = "unbound"
	ControlBound    = "bound"
	ControlStopping = "stopping"
	ControlManual   = "manual"
)

// BindControl binds quote QuoteID to task TaskID at control epoch ExpectedControlEpoch (0 for a
// root never bound). InitiatorID is the task's initiator, an actor with an active human grant, and
// AgentID its AI agent, an actor with an active AI grant (REFERENCE_INVALID otherwise): the binding
// holds while both grants stay as they are (decision D-5).
type BindControl struct {
	IdempotencyKey       string
	QuoteID              string
	TaskID               string
	InitiatorID          string
	AgentID              string
	ExpectedControlEpoch int64
}

// StopControl fences the bound task out of quote QuoteID at ExpectedControlEpoch, for Reason.
type StopControl struct {
	IdempotencyKey       string
	QuoteID              string
	ExpectedControlEpoch int64
	Reason               string
}

// EnterManualControl puts the stopping quote QuoteID under manual control at ExpectedControlEpoch,
// for Reason.
type EnterManualControl struct {
	IdempotencyKey       string
	QuoteID              string
	ExpectedControlEpoch int64
	Reason               string
}

// HandoffControl binds the stopping quote QuoteID to its task again at ExpectedControlEpoch, for
// Reason, under the delegation InitiatorID and AgentID name (as BindControl's).
type HandoffControl struct {
	IdempotencyKey       string
	QuoteID              string
	ExpectedControlEpoch int64
	InitiatorID          string
	AgentID              string
	Reason               string
}

// ReleaseControl gives quote QuoteID back at ExpectedControlEpoch, for Reason.
type ReleaseControl struct {
	IdempotencyKey       string
	QuoteID              string
	ExpectedControlEpoch int64
	Reason               string
}

// Validate checks the closed contract.
func (c BindControl) Validate() error {
	if err := controlHead(c.IdempotencyKey, c.QuoteID, c.ExpectedControlEpoch); err != nil {
		return err
	}
	if !kernel.ValidIdentifier(c.TaskID) {
		return schema("/args/task_id")
	}
	return delegation(c.InitiatorID, c.AgentID)
}

// Validate checks the closed contract.
func (c StopControl) Validate() error {
	return controlEnd(c.IdempotencyKey, c.QuoteID, c.ExpectedControlEpoch, c.Reason)
}

// Validate checks the closed contract.
func (c EnterManualControl) Validate() error {
	return controlEnd(c.IdempotencyKey, c.QuoteID, c.ExpectedControlEpoch, c.Reason)
}

// Validate checks the closed contract.
func (c HandoffControl) Validate() error {
	if err := controlHead(c.IdempotencyKey, c.QuoteID, c.ExpectedControlEpoch); err != nil {
		return err
	}
	if c.ExpectedControlEpoch < 1 {
		return schema("/args/expected_control_epoch")
	}
	if err := delegation(c.InitiatorID, c.AgentID); err != nil {
		return err
	}
	if !text(c.Reason, maxText) {
		return schema("/args/reason")
	}
	return nil
}

// Validate checks the closed contract.
func (c ReleaseControl) Validate() error {
	return controlEnd(c.IdempotencyKey, c.QuoteID, c.ExpectedControlEpoch, c.Reason)
}

func controlHead(key, id string, expected int64) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(id):
		return schema("/args/quote_id")
	case expected < 0 || expected > maxVersion:
		return schema("/args/expected_control_epoch")
	}
	return nil
}

func controlEnd(key, id string, expected int64, reason string) error {
	if err := controlHead(key, id, expected); err != nil {
		return err
	}
	if expected < 1 {
		return schema("/args/expected_control_epoch")
	}
	if !text(reason, maxText) {
		return schema("/args/reason")
	}
	return nil
}

func delegation(initiator, agent string) error {
	switch {
	case !kernel.ValidIdentifier(initiator):
		return schema("/args/initiator_id")
	case !kernel.ValidIdentifier(agent) || agent == initiator:
		return schema("/args/agent_id")
	}
	return nil
}

// ControlClaim is the object control a write or a control change claims: the task it acts for and
// the control epoch the task's last control change returned (the receipt's RowVersion, which TSK
// records).
type ControlClaim struct {
	TaskID       string
	ControlEpoch int64
}

// ErrInvalidClaim means a control claim names no task or no positive epoch.
var ErrInvalidClaim = errors.New("trd: invalid control claim")

type claimKey struct{}

// WithControl returns ctx carrying claim, for the trusted adapter that runs a task's writes and
// control changes.
func WithControl(ctx context.Context, claim ControlClaim) (context.Context, error) {
	if !kernel.ValidIdentifier(claim.TaskID) || claim.ControlEpoch < 1 || claim.ControlEpoch > maxVersion {
		return nil, ErrInvalidClaim
	}
	return context.WithValue(ctx, claimKey{}, claim), nil
}

// ControlFromContext returns the claim of ctx, if any.
func ControlFromContext(ctx context.Context) (ControlClaim, bool) {
	c, ok := ctx.Value(claimKey{}).(ControlClaim)
	return c, ok
}
