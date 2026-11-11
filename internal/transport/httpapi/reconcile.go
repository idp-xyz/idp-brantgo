package httpapi

import (
	"context"
	"net/http"
	"reflect"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// reconciliation is the answer of a reconciliation (contract 06, 动作对账): what became of a command
// whose answer the caller lost, without executing it again. Receipt is the stored receipt of a
// committed command, as the command answered it; null otherwise.
type reconciliation struct {
	OK       bool     `json:"ok"`
	State    string   `json:"state"`
	ActionID *string  `json:"action_id"`
	ObjectID *string  `json:"object_id"`
	Receipt  *receipt `json:"receipt"`
}

// reconciler adapts a context's Reconciler, whose commands are of type K. A command that is not a K
// cannot be reconciled through this context.
func reconciler[K, R any](reconcile func(context.Context, K) (R, error)) *reconcileFunc {
	return &reconcileFunc{commands: reflect.TypeFor[K](), call: func(ctx context.Context, cmd any) (reconciliation, error) {
		k, ok := cmd.(K)
		if !ok {
			return reconciliation{}, kernel.Reject(kernel.CodeUnsupportedContract, "/command", "not reconcilable")
		}
		r, err := reconcile(ctx, k)
		if err != nil {
			return reconciliation{}, err
		}
		v := reflect.ValueOf(r)
		out := reconciliation{OK: true, State: v.FieldByName("State").String()}
		if id := v.FieldByName("ActionID").String(); id != "" {
			out.ActionID = &id
		}
		if id := v.FieldByName("ObjectID").String(); id != "" {
			out.ObjectID = &id
		}
		if p := v.FieldByName("Receipt"); !p.IsNil() {
			e := p.Elem()
			out.Receipt = &receipt{OK: true, ActionID: e.FieldByName("ActionID").String(), ObjectID: e.FieldByName("ObjectID").String(),
				RowVersion: e.FieldByName("RowVersion").Int(), Status: e.FieldByName("Status").String(), Replayed: e.FieldByName("Replayed").Bool()}
		}
		return out, nil
	}}
}

// reconcileFunc reconciles the commands of one context whose type implements commands.
type reconcileFunc struct {
	commands reflect.Type
	call     func(ctx context.Context, cmd any) (reconciliation, error)
}

// reconcilable reports whether set reconciles command name.
func (set *commandSet) reconcilable(name string) bool {
	run, ok := set.commands[name]
	return ok && set.reconcile != nil && run.cmd != nil && run.cmd.Implements(set.reconcile.commands)
}

// reconcile answers POST /api/brant/v1/<bc>/commands/<command>/reconcile. The body is the request
// the caller sent, envelope and idempotency key included, checked as the command checks it; the
// context then says what became of it (committed, payload_conflict or not_committed) without
// executing anything. Reconciling needs the context's read permission and re-checks it and the
// caller's data scope now. Only commands the context reconciles have the route.
func (s *server) reconcile(w http.ResponseWriter, r *http.Request) {
	set, ok := s.contexts[r.PathValue("bc")]
	name := r.PathValue("command")
	if !ok || !set.reconcilable(name) {
		s.reject(w, r, kernel.Reject(kernel.CodeNotFound, "/", "no such route"))
		return
	}
	cmd, err := s.decodeCommand(r, set, name, set.commands[name])
	if err != nil {
		s.fail(w, r, unwrapExecuted(err))
		return
	}
	out, err := set.reconcile.call(r.Context(), cmd)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
