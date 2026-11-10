//go:build integration

package bento_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

// A COMMIT whose answer never arrives is application.ErrCommitUncertain, whether the server
// committed or not. pgx v5.10.0 reports the lost answer as pgconn.ErrConnClosed, which Bento
// v0.1.0-rc.2 would pass on as a certain failure. A callback's own error is never reclassified.
func TestLostCommitIsUncertain(t *testing.T) {
	fault := &pgtest.CommitFault{}
	db, err := bentopg.NewDB(pgtest.FaultyAppPool(t, fault))
	if err != nil {
		t.Fatal(err)
	}
	observer := newDB(t)
	ctx := context.Background()
	tx := bento.ScopeTransactor{DB: db, Scope: scope}
	for _, c := range []struct {
		mode pgtest.FaultMode
		want string // txid_status of the transaction as the server decided it
	}{{pgtest.LoseAnswer, "committed"}, {pgtest.LoseCommit, "aborted"}} {
		fault.Arm(c.mode)
		var xid int64
		err := tx.WithinTransaction(ctx, func(ctx context.Context) error {
			exec, err := db.RequireExecutor(ctx)
			if err != nil {
				return err
			}
			return exec.QueryRow(ctx, "SELECT txid_current()").Scan(&xid)
		})
		if !errors.Is(err, application.ErrCommitUncertain) || !fault.Fired() {
			t.Fatalf("mode %d: %v (fired %v); want ErrCommitUncertain", c.mode, err, fault.Fired())
		}
		if fault.Committed() != (c.mode == pgtest.LoseAnswer) {
			t.Fatalf("mode %d: the server answered CommandComplete: %v", c.mode, fault.Committed())
		}
		q, err := observer.ReadExecutor(ctx)
		if err != nil {
			t.Fatal(err)
		}
		status := "in progress"
		for deadline := time.Now().Add(10 * time.Second); status == "in progress" && time.Now().Before(deadline); {
			if err = q.QueryRow(ctx, "SELECT txid_status($1)", xid).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status == "in progress" {
				time.Sleep(20 * time.Millisecond) // the backend rolls back once it sees the connection close
			}
		}
		if status != c.want {
			t.Fatalf("mode %d: transaction %d is %s on the server, want %s", c.mode, xid, status, c.want)
		}
	}
	fault.Arm(0)
	own := fmt.Errorf("callback failed: %w", pgconn.ErrConnClosed)
	if err := tx.WithinTransaction(ctx, func(context.Context) error { return own }); !errors.Is(err, own) || errors.Is(err, application.ErrCommitUncertain) {
		t.Fatalf("callback error: %v; want it unchanged", err)
	}
}
