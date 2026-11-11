package api

import (
	"context"
	"errors"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Object control of a quote root (SM-TRD-CONTROL, TRD-INV-015, WBS S3-08; Context Map R17 TSK→TRD
// "ObjectControl commands v1"). TRD is the only writer of a root's control state and control epoch;
// TSK will be the caller of the three commands. Every bind, stop and release increments the control
// epoch, which the commands compare (ExpectedControlEpoch, VERSION_CONFLICT otherwise):
//
//	object_control.bind     unbound → bound: the root is claimed for TaskID; another binding is
//	                        OBJECT_CONTROL_CONFLICT. Human or system, never AI.
//	object_control.stop     bound → stopping: every write is refused from now on. Human only.
//	object_control.release  bound or stopping → unbound. Human only.
//
// While a root is bound, every write on it (quote.save_draft, quote.revise, quote.discard_draft,
// quote.freeze, quote.metadata.update and the replay of any of them) must carry a ControlClaim of
// the binding's task and current control epoch in its context, and the company's authority epoch
// must be the one the binding was made under: no claim, another task or changed access is
// OBJECT_CONTROL_CONFLICT, an earlier epoch CONTROL_EPOCH_STALE. A stopping root takes no write
// (OBJECT_CONTROL_CONFLICT; CONTROL_EPOCH_STALE for the stopped task's claim). An unbound root
// takes writes without a claim under their permissions alone; a claim on it is CONTROL_EPOCH_STALE.
// Reads and reconciliation are never fenced.
//
// The claim is put into the context by the trusted adapter that runs the task (TSK), never taken
// from a request body: no command struct has a task member, so a transport cannot pass one on
// (Python test_task_identity_cannot_be_supplied_through_public_json).
const (
	CommandBindControl    = "object_control.bind"
	CommandStopControl    = "object_control.stop"
	CommandReleaseControl = "object_control.release"

	ControlUnbound  = "unbound"
	ControlBound    = "bound"
	ControlStopping = "stopping"
)

// BindControl binds quote QuoteID to task TaskID at control epoch ExpectedControlEpoch (0 for a
// root never bound).
type BindControl struct {
	IdempotencyKey       string
	QuoteID              string
	TaskID               string
	ExpectedControlEpoch int64
}

// StopControl fences the bound task out of quote QuoteID at ExpectedControlEpoch, for Reason.
type StopControl struct {
	IdempotencyKey       string
	QuoteID              string
	ExpectedControlEpoch int64
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
	return nil
}

// Validate checks the closed contract.
func (c StopControl) Validate() error {
	return controlEnd(c.IdempotencyKey, c.QuoteID, c.ExpectedControlEpoch, c.Reason)
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

// ControlClaim is the object control a write claims: the task it acts for and the control epoch
// the task's bind (or the receipt TSK keeps of it) returned.
type ControlClaim struct {
	TaskID       string
	ControlEpoch int64
}

// ErrInvalidClaim means a control claim names no task or no positive epoch.
var ErrInvalidClaim = errors.New("trd: invalid control claim")

type claimKey struct{}

// WithControl returns ctx carrying claim, for the trusted adapter that runs a task's writes.
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
