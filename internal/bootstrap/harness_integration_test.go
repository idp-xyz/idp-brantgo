//go:build integration

package bootstrap_test

import (
	"context"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The S1 suite drives the composed contexts through their api packages against the migrated
// PostgreSQL 16 database. Each test runs in a fresh tenant with company.A and company.B, so tests
// never see each other's data. Test names cite the Python V0.50 test they port.

// every S1 permission, Python's "*".
var allPermissions = []string{
	"source.register", "evd.read", "evd.audit",
}

type world struct {
	t        *testing.T
	platform *bootstrap.Platform
	c        *bootstrap.Contexts
	tenant   string
	serial   int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	platform, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: pgtest.AppDSN(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	c, err := platform.Contexts()
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, platform: platform, c: c, tenant: pgtest.UniqueID(t, "tenant.")}
}

func (w *world) scope(company string) kernel.Scope {
	return kernel.Scope{Tenant: kernel.TenantID(w.tenant), Company: kernel.CompanyID(company)}
}

// actor provisions a human actor in company.A with permissions and returns its request context.
func (w *world) actor(id string, permissions ...string) context.Context {
	return w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id}, Permissions: permissions})
}

func (w *world) provision(g bootstrap.Grant) context.Context {
	w.t.Helper()
	if g.Permissions == nil {
		g.Permissions = []string{}
	}
	if _, err := w.platform.Provision(context.Background(), g); err != nil {
		w.t.Fatal(err)
	}
	ctx, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: g.Actor, Scope: g.Scope})
	if err != nil {
		w.t.Fatal(err)
	}
	return action.WithCorrelation(ctx, pgtest.UniqueID(w.t, "corr."))
}

// key returns a fresh idempotency key, like the Python tests' k1, k2, ...
func (w *world) key() string {
	w.serial++
	return "k" + string(rune('0'+w.serial/100%10)) + string(rune('0'+w.serial/10%10)) + string(rune('0'+w.serial%10))
}

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code
	}
	if err != nil {
		return "ERROR: " + err.Error()
	}
	return "OK"
}

func path(err error) string {
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 {
		return rej.Fields[0].Path
	}
	return ""
}

func (w *world) must(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) want(err error, wantCode string) {
	w.t.Helper()
	if got := code(err); got != wantCode {
		w.t.Fatalf("want %s, got %s", wantCode, got)
	}
}

// count returns the rows of table in company, read as the application role through the scope's
// RLS.
func (w *world) count(company, table string) int {
	w.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	if err != nil {
		w.t.Fatal(err)
	}
	var n int
	err = bento.ScopeTransactor{DB: db, Scope: w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return exec.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n)
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return n
}
