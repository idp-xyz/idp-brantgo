// Package reconcile is TSK's port to the ActionReconciliation v1 of the contexts a task step writes
// (Context Map R18 TRD→TSK, R66 EVD→TSK, R67 INQ→TSK, R68 SUP→TSK, R69 CST→TSK; owner decision S4
// D-11), over each context's published api. TSK calls them only before its own transaction.
package reconcile

import (
	"context"

	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/application"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Contexts are the ActionReconciliation v1 of each context a task step writes; a nil one is not
// wired.
type Contexts struct {
	EVD evdapi.ActionReconciler
	INQ inqapi.ActionReconciler
	SUP supapi.ActionReconciler
	CST cstapi.ActionReconciler
	TRD trdapi.ActionReconciler
}

// Ports returns TSK's ports to c by context code.
func Ports(c Contexts) application.Reconcilers {
	out := application.Reconcilers{}
	if c.EVD != nil {
		out[api.TargetEVD] = port(func(ctx context.Context, command, key, hash string) (application.Answer, error) {
			r, err := c.EVD.ReconcileHash(ctx, command, key, hash)
			return answer(r.State, receiptOf(r.Receipt)), err
		})
	}
	if c.INQ != nil {
		out[api.TargetINQ] = port(func(ctx context.Context, command, key, hash string) (application.Answer, error) {
			r, err := c.INQ.ReconcileHash(ctx, command, key, hash)
			return answer(r.State, receiptOf(r.Receipt)), err
		})
	}
	if c.SUP != nil {
		out[api.TargetSUP] = port(func(ctx context.Context, command, key, hash string) (application.Answer, error) {
			r, err := c.SUP.ReconcileHash(ctx, command, key, hash)
			return answer(r.State, receiptOf(r.Receipt)), err
		})
	}
	if c.CST != nil {
		out[api.TargetCST] = port(func(ctx context.Context, command, key, hash string) (application.Answer, error) {
			r, err := c.CST.ReconcileHash(ctx, command, key, hash)
			return answer(r.State, receiptOf(r.Receipt)), err
		})
	}
	if c.TRD != nil {
		out[api.TargetTRD] = port(func(ctx context.Context, command, key, hash string) (application.Answer, error) {
			r, err := c.TRD.ReconcileHash(ctx, command, key, hash)
			return answer(r.State, receiptOf(r.Receipt)), err
		})
	}
	return out
}

// port is application.Reconciler as a function.
type port func(ctx context.Context, command, key, hash string) (application.Answer, error)

// ReconcileHash calls the function.
func (p port) ReconcileHash(ctx context.Context, command, idempotencyKey, requestHash string) (application.Answer, error) {
	return p(ctx, command, idempotencyKey, requestHash)
}

// receipt is the stored receipt every context's ActionReconciliation v1 answers with: each
// context's api declares its own Receipt type with these members.
type receipt interface {
	evdapi.Receipt | inqapi.Receipt | supapi.Receipt | cstapi.Receipt | trdapi.Receipt
}

func receiptOf[R receipt](r *R) *api.TargetReceipt {
	if r == nil {
		return nil
	}
	// The five Receipt types have the same members; converting through one of them keeps the
	// adapter free of reflection.
	c := trdapi.Receipt(*r)
	return &api.TargetReceipt{ActionID: c.ActionID, ObjectID: c.ObjectID, RowVersion: c.RowVersion, Status: c.Status}
}

func answer(state kernel.ReconcileState, r *api.TargetReceipt) application.Answer {
	return application.Answer{State: state, Receipt: r}
}
