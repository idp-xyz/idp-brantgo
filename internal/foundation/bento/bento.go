// Package bento adapts IDP-Bento-Go to Brant. Every bounded context shares it, and it depends on
// no context.
package bento

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	_ application.Clock      = SystemClock{}
	_ application.Transactor = ScopeTransactor{}
)

// SystemClock reads the wall clock at the microsecond precision PostgreSQL stores.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return kernel.Instant(time.Now()) }

var (
	ErrScopeConflict = errors.New("bento: nested transaction for another scope")
	ErrNotConfigured = errors.New("bento: scope transactor not configured")
)

type scopeKey struct{}

// ScopeTransactor runs a Bento transaction bound to one tenant and company through the
// transaction-local settings app.tenant_id and app.company_id, which the FORCE RLS policies read.
// A scope marker in the Go context refuses a nested switch to another scope.
//
// Every context has a DB of its own (rule T1), so a context that calls into another context's
// transactor inside its own transaction gets application.ErrTransactionConflict instead of
// silently joining. RLS is defense in depth against a missing or wrong scope in a query, not
// protection from compromised SQL.
type ScopeTransactor struct {
	DB    *bentopg.DB
	Scope kernel.Scope
}

func (t ScopeTransactor) WithinTransaction(ctx context.Context, fn application.TxFunc) error {
	if t.DB == nil || !t.Scope.Valid() || fn == nil {
		return ErrNotConfigured
	}
	if current, ok := ScopeFromContext(ctx); ok && current != t.Scope {
		return fmt.Errorf("%w: %s inside %s", ErrScopeConflict, t.Scope, current)
	}
	return t.DB.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error {
		exec, err := t.DB.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		_, err = exec.Exec(txCtx, "SELECT pg_catalog.set_config('app.tenant_id', $1, true), pg_catalog.set_config('app.company_id', $2, true)",
			string(t.Scope.Tenant), string(t.Scope.Company))
		if err != nil {
			return fmt.Errorf("bento: bind scope %s: %w", t.Scope, err)
		}
		return fn(context.WithValue(txCtx, scopeKey{}, t.Scope))
	})
}

// ScopeFromContext returns the scope of the ScopeTransactor transaction ctx runs in.
func ScopeFromContext(ctx context.Context) (kernel.Scope, bool) {
	s, ok := ctx.Value(scopeKey{}).(kernel.Scope)
	return s, ok
}
