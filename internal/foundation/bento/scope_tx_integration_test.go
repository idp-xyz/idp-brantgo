//go:build integration

package bento_test

import (
	"context"
	"errors"
	"testing"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func newDB(t *testing.T) *bentopg.DB {
	t.Helper()
	pool := pgtest.AppPool(t)
	db, err := bentopg.NewDB(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CheckSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

var scope = kernel.Scope{Tenant: "tenant.m0", Company: "company.m0"}

// AC4: with one DB per context, a context that calls into another context's transaction owner
// inside its own transaction gets ErrTransactionConflict instead of silently joining it.
func TestContextsNeverJoinEachOthersTransactions(t *testing.T) {
	trd := newDB(t)
	cst, err := bentopg.NewDB(pgtest.AppPool(t))
	if err != nil {
		t.Fatal(err)
	}
	nop := func(context.Context) error { return nil }
	err = bento.ScopeTransactor{DB: trd, Scope: scope}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		if _, err := cst.RequireExecutor(ctx); !errors.Is(err, application.ErrTransactionConflict) {
			t.Errorf("RequireExecutor of another owner: %v", err)
		}
		if _, err := cst.ReadExecutor(ctx); !errors.Is(err, application.ErrTransactionConflict) {
			t.Errorf("ReadExecutor of another owner: %v", err)
		}
		if err := cst.Transactor().WithinTransaction(ctx, nop); !errors.Is(err, application.ErrTransactionConflict) {
			t.Errorf("Transactor of another owner: %v", err)
		}
		if err := (bento.ScopeTransactor{DB: cst, Scope: scope}).WithinTransaction(ctx, nop); !errors.Is(err, application.ErrTransactionConflict) {
			t.Errorf("ScopeTransactor of another owner: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The scope is bound with transaction-local settings, a nested call of the same owner joins the
// transaction, and a nested switch to another scope is refused.
func TestScopeBindsTheTransaction(t *testing.T) {
	db := newDB(t)
	tx := bento.ScopeTransactor{DB: db, Scope: scope}
	read := func(ctx context.Context) (tenant, company string, xid int64) {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = exec.QueryRow(ctx, "SELECT current_setting('app.tenant_id'), current_setting('app.company_id'), txid_current()").
			Scan(&tenant, &company, &xid)
		if err != nil {
			t.Fatal(err)
		}
		return tenant, company, xid
	}
	err := tx.WithinTransaction(context.Background(), func(ctx context.Context) error {
		tenant, company, outer := read(ctx)
		if tenant != "tenant.m0" || company != "company.m0" {
			t.Errorf("settings = %s/%s", tenant, company)
		}
		if got, ok := bento.ScopeFromContext(ctx); !ok || got != scope {
			t.Errorf("ScopeFromContext = %v, %v", got, ok)
		}
		if err := tx.WithinTransaction(ctx, func(inner context.Context) error {
			if _, _, xid := read(inner); xid != outer {
				t.Errorf("nested call of the same owner ran in transaction %d, outer %d", xid, outer)
			}
			return nil
		}); err != nil {
			t.Error(err)
		}
		other := bento.ScopeTransactor{DB: db, Scope: kernel.Scope{Tenant: "tenant.m0", Company: "company.other"}}
		if err := other.WithinTransaction(ctx, func(context.Context) error { return nil }); !errors.Is(err, bento.ErrScopeConflict) {
			t.Errorf("nested switch of company: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := (bento.ScopeTransactor{DB: db}).WithinTransaction(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, bento.ErrNotConfigured) {
		t.Fatalf("missing scope: %v", err)
	}
}
