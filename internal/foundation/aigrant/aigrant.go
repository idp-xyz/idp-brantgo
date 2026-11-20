// Package aigrant carries the AI layer's proof that a human-issued approval grant was used for one
// call of an AI actor (S4 decision D-7; AI-INV-002) from the gateway to the ledger of the owning
// context. A command whose rule needs one (authorization.Rule.AIApprovalGrant: order.prepare and
// order.review.request) is accepted from an AI actor only when the call carries a proof that names
// exactly its scope, its actor, the command, the request hash, the object and its version, and the
// idempotency key the grant was used with; foundation/action checks it before anything else is
// decided, whatever the transport. People never need one.
//
// The proof travels in the call's context.Context under a key of this package: no request body
// carries it, and no transport or context can set it. Only the AI layer's gateway mints one, from
// the grant record approvalgrant.Service.Use has just marked used for the call (gateway.Admit);
// dependency-check keeps every other package out (outside test files only internal/ai/gateway and
// internal/foundation/action import this one).
package aigrant

import (
	"context"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// CodeGrantInvalid is the AI layer's GRANT_INVALID (internal/ai/codes): the proof the call carries
// was minted for another call.
const CodeGrantInvalid = "GRANT_INVALID"

// Object is the object a grant binds, at the version the call names.
type Object struct {
	Type    string // such as "trd.quote"
	ID      string
	Version int64
}

// Proof states that approval grant GrantID was used, by AI actor Agent in Scope, for the call of
// Command with RequestHash on Object sent with idempotency key Key.
type Proof struct {
	GrantID     string
	Scope       kernel.Scope
	Agent       string
	Command     string
	RequestHash string
	Object      Object
	Key         string
}

type proofKey struct{}

// With returns ctx carrying p. Only the AI layer's gateway calls it, for a grant it has just used.
func With(ctx context.Context, p Proof) context.Context {
	return context.WithValue(ctx, proofKey{}, p)
}

// From returns the proof ctx carries.
func From(ctx context.Context) (Proof, bool) {
	p, ok := ctx.Value(proofKey{}).(Proof)
	return p, ok
}

// Call is one call of a command whose rule needs an approval grant from AI actors, as the owning
// context's ledger is asked to execute it.
type Call struct {
	Scope       kernel.Scope
	Actor       kernel.ActorRef
	Command     string
	RequestHash string // the ledger's request hash of the business arguments
	Key         string // the call's idempotency key
	Object      Object // the object and version the context says the call acts on
}

// Check admits c. A person's or the system's call needs no proof. An AI actor's call needs the
// proof of a grant used for exactly c: none is PERMISSION_DENIED (the call did not come through the
// gateway with a grant); one that names another scope, actor, command, request hash, object,
// version or key is GRANT_INVALID.
func Check(ctx context.Context, c Call) error {
	if c.Actor.Kind != kernel.ActorAI {
		return nil
	}
	p, ok := From(ctx)
	if !ok {
		return kernel.Reject(kernel.CodePermissionDenied, "/", "an AI actor issues "+c.Command+" only with an approval grant")
	}
	if p.GrantID == "" || p.Scope != c.Scope || p.Agent != c.Actor.ID || p.Command != c.Command || p.RequestHash != c.RequestHash ||
		p.Object != c.Object || p.Key != c.Key {
		return kernel.RejectAs(CodeGrantInvalid, kernel.CategoryForbidden, "/grant", "")
	}
	return nil
}
