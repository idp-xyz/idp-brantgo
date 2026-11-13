package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"

	"go.idp.xyz/idp-brant/internal/foundation/strictjson"
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
		return reconciliationOf(r), nil
	}}
}

// hashReconciler is reconciler for a context that also publishes ActionReconciliation v1 (Context
// Map R18, R66–R69: TRD, INQ, CST, EVD, SUP): its route accepts the request hash in place of the
// arguments (reconcileHash).
func hashReconciler[K, R any](reconcile func(context.Context, K) (R, error),
	byHash func(ctx context.Context, command, idempotencyKey, requestHash string) (R, error)) *reconcileFunc {
	f := reconciler(reconcile)
	f.byHash = func(ctx context.Context, command, key, hash string) (reconciliation, error) {
		r, err := byHash(ctx, command, key, hash)
		if err != nil {
			return reconciliation{}, err
		}
		return reconciliationOf(r), nil
	}
	return f
}

// reconciliationOf is the wire answer of a context's Reconciliation r.
func reconciliationOf(r any) reconciliation {
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
	return out
}

// reconcileFunc reconciles the commands of one context whose type implements commands; byHash,
// when set, reconciles one by its ledger identity (ActionReconciliation v1).
type reconcileFunc struct {
	commands reflect.Type
	call     func(ctx context.Context, cmd any) (reconciliation, error)
	byHash   func(ctx context.Context, command, idempotencyKey, requestHash string) (reconciliation, error)
}

// reconcilable reports whether set reconciles command name.
func (set *commandSet) reconcilable(name string) bool {
	run, ok := set.commands[name]
	return ok && set.reconcile != nil && run.cmd != nil && run.cmd.Implements(set.reconcile.commands)
}

// reconcilableByHash reports whether set reconciles command name by its request hash too.
func (set *commandSet) reconcilableByHash(name string) bool {
	return set.reconcilable(name) && set.reconcile.byHash != nil
}

// hashEnvelope is the request of a reconciliation by hash (ActionReconciliation v1): the envelope of
// the command as it was sent, with the SHA-256 of its business arguments (the hash the context's
// ledger records, which the context's RequestOf returns) in place of the arguments. Presence is
// checked member by member.
type hashEnvelope struct {
	Contract       *string `json:"contract"`
	Command        *string `json:"command"`
	IdempotencyKey *string `json:"idempotency_key"`
	RequestHash    *string `json:"request_hash"`
}

// byHash reports whether a reconciliation body names a request hash (any member spelled
// request_hash, in any case: the closed decoding refuses a case variant).
func byHash(raw []byte) bool {
	var probe struct {
		RequestHash json.RawMessage `json:"request_hash"`
	}
	return json.Unmarshal(raw, &probe) == nil && probe.RequestHash != nil
}

// reconcile answers POST /api/brant/v1/<bc>/commands/<command>/reconcile. The body is the request
// the caller sent, envelope and idempotency key included, checked as the command checks it; the
// context then says what became of it (committed, payload_conflict or not_committed) without
// executing anything. For TRD, INQ, CST, EVD and SUP the body may instead be the envelope with the
// request hash in place of the arguments {contract, command, idempotency_key, request_hash}
// (ActionReconciliation v1, Context Map R18, R66–R69; the caller kept the hash, not the command).
// Reconciling needs the context's read permission and re-checks it and the caller's data scope now.
// Only commands the context reconciles have the route.
func (s *server) reconcile(w http.ResponseWriter, r *http.Request) {
	set, ok := s.contexts[r.PathValue("bc")]
	name := r.PathValue("command")
	if !ok || !set.reconcilable(name) {
		s.reject(w, r, kernel.Reject(kernel.CodeNotFound, "/", "no such route"))
		return
	}
	raw, limit, values, err := s.readBody(r, set, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var out reconciliation
	if set.reconcilableByHash(name) && byHash(raw) {
		var key, hash string
		if key, hash, err = decodeHashEnvelope(raw, set, name, limit, values); err == nil {
			out, err = set.reconcile.byHash(r.Context(), name, key, hash)
		}
	} else {
		var cmd any
		if cmd, err = decodeEnvelope(raw, set, name, set.commands[name], limit, values); err != nil {
			s.fail(w, r, unwrapExecuted(err))
			return
		}
		out, err = set.reconcile.call(r.Context(), cmd)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// decodeHashEnvelope decodes a reconciliation by hash of command name: a closed envelope of the
// context's contract and the route's command, with a valid idempotency key and a request hash of 64
// lower-case hexadecimal digits (SCHEMA_INVALID otherwise; UNSUPPORTED_CONTRACT for another contract
// or command).
func decodeHashEnvelope(raw []byte, set *commandSet, name string, limit int64, values int) (key, hash string, err error) {
	var env hashEnvelope
	if err = strictjson.Decode(bytes.NewReader(raw), &env, strictjson.Limits{MaxBytes: limit, MaxValues: values}); err != nil {
		return "", "", err
	}
	switch {
	case env.Contract == nil || *env.Contract != set.contract:
		return "", "", kernel.Reject(kernel.CodeUnsupportedContract, "/contract", "")
	case env.Command == nil || *env.Command != name:
		return "", "", kernel.Reject(kernel.CodeUnsupportedContract, "/command", "")
	case env.IdempotencyKey == nil || !kernel.ValidIdentifier(*env.IdempotencyKey):
		return "", "", kernel.Reject(kernel.CodeSchemaInvalid, "/idempotency_key", "required identifier")
	case env.RequestHash == nil || !kernel.ValidRequestHash(*env.RequestHash):
		return "", "", kernel.Reject(kernel.CodeSchemaInvalid, "/request_hash", "64 lower-case hexadecimal digits")
	}
	return *env.IdempotencyKey, *env.RequestHash, nil
}
