// Package identity carries the verified caller of a request through the Go context.
package identity

import (
	"context"
	"errors"
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	ErrUnauthenticated  = errors.New("identity: unauthenticated")
	ErrInvalidPrincipal = errors.New("identity: invalid principal")
)

// Principal is the verified caller of a request: the actor and the tenant and company it acts
// in. Only a verified token yields a Principal; request bodies never supply actor, tenant or
// company. The OIDC adapter that verifies tokens arrives with the HTTP transport (S1).
type Principal struct {
	Actor kernel.ActorRef
	Scope kernel.Scope
}

// Validate requires a known actor kind, valid identifiers and a complete scope.
func (p Principal) Validate() error {
	if !p.Actor.Valid() || !p.Scope.Valid() {
		return fmt.Errorf("%w: actor %s in %s", ErrInvalidPrincipal, p.Actor, p.Scope)
	}
	return nil
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) (context.Context, error) {
	if err := p.Validate(); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, principalKey{}, p), nil
}

// FromContext returns the Principal of ctx, or ErrUnauthenticated.
func FromContext(ctx context.Context) (Principal, error) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}
