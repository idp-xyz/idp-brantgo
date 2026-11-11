//go:build integration

package authorization_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	alice = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "alice"}
	agent = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.quote"}
	bob   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "bob"}
	rules = []authorization.Rule{
		{Command: "quote.save_draft", Permission: "quote.write", ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		authorization.HumanOnly("quote.activate", "quote.activate"),
		authorization.HumanOnly("quote.cancel", "quote.cancel"),
	}
)

type fixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	db    *bentopg.DB
	scope kernel.Scope
	auth  authorization.Authorizer
}

// newFixture provisions a fresh scope in which alice (human) and agent.quote (AI) both hold
// quote.write and quote.activate.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.AppPool(t)
	db, err := bentopg.NewDB(pool)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := authorization.NewPolicy(rules...)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, pool: pool, db: db, auth: authorization.Authorizer{Policy: policy},
		scope: kernel.Scope{Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: kernel.CompanyID(pgtest.UniqueID(t, "company."))}}
	err = f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
		g := authorization.Grants{}
		if err := g.Provision(ctx, exec, f.scope); err != nil {
			return err
		}
		for _, a := range []kernel.ActorRef{alice, agent} {
			if _, err := g.Put(ctx, exec, f.scope, a, []string{"quote.write", "quote.activate"}, time.Now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) inScope(scope kernel.Scope, fn func(context.Context, bentopg.Executor) error) error {
	return bento.ScopeTransactor{DB: f.db, Scope: scope}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := f.db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
}

func (f *fixture) as(ctx context.Context, actor kernel.ActorRef) context.Context {
	ctx, err := identity.WithPrincipal(ctx, identity.Principal{Actor: actor, Scope: f.scope})
	if err != nil {
		f.t.Fatal(err)
	}
	return ctx
}

func (f *fixture) authorize(actor kernel.ActorRef, command string) (authorization.Decision, error) {
	var d authorization.Decision
	err := f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		d, err = f.auth.Authorize(f.as(ctx, actor), exec, command)
		return err
	})
	return d, err
}

func TestAIIsRefusedHumanOnlyCommandsEvenWithTheGrant(t *testing.T) {
	f := newFixture(t)
	if _, err := f.authorize(agent, "quote.activate"); !errors.Is(err, authorization.ErrActorKindDenied) {
		t.Fatalf("AI on quote.activate: %v", err)
	}
	if d, err := f.authorize(agent, "quote.save_draft"); err != nil || d.Epoch < 1 {
		t.Fatalf("AI on quote.save_draft: %+v, %v", d, err)
	}
	if _, err := f.authorize(alice, "quote.activate"); err != nil {
		t.Fatalf("human on quote.activate: %v", err)
	}
}

func TestMissingGrantsAndUnknownCommandsAreDenied(t *testing.T) {
	f := newFixture(t)
	if _, err := f.authorize(bob, "quote.save_draft"); !errors.Is(err, authorization.ErrForbidden) {
		t.Errorf("actor without a grant: %v", err)
	}
	if _, err := f.authorize(alice, "quote.cancel"); !errors.Is(err, authorization.ErrForbidden) {
		t.Errorf("grant without the permission: %v", err)
	}
	if _, err := f.authorize(alice, "quote.unknown"); !errors.Is(err, authorization.ErrUnknownCommand) {
		t.Errorf("command without a rule: %v", err)
	}
	impostor := kernel.ActorRef{Kind: kernel.ActorHuman, ID: agent.ID}
	if _, err := f.authorize(impostor, "quote.activate"); !errors.Is(err, authorization.ErrForbidden) {
		t.Errorf("token claiming human for an AI grant: %v", err)
	}
	err := f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := f.auth.Authorize(ctx, exec, "quote.save_draft")
		return err
	})
	if !errors.Is(err, identity.ErrUnauthenticated) {
		t.Errorf("no principal: %v", err)
	}
}

// Python test_authority_epoch_changes_only_when_access_changes.
func TestEpochChangesOnlyWhenAccessChanges(t *testing.T) {
	f := newFixture(t)
	g := authorization.Grants{}
	var epochs []int64
	step := func(fn func(context.Context, bentopg.Executor) (int64, error)) {
		t.Helper()
		if err := f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
			e, err := fn(ctx, exec)
			epochs = append(epochs, e)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	put := func(perms ...string) func(context.Context, bentopg.Executor) (int64, error) {
		return func(ctx context.Context, exec bentopg.Executor) (int64, error) {
			return g.Put(ctx, exec, f.scope, alice, perms, time.Now())
		}
	}
	revoke := func(ctx context.Context, exec bentopg.Executor) (int64, error) {
		return g.Revoke(ctx, exec, f.scope, alice.ID, time.Now())
	}
	step(put("quote.activate", "quote.write"))                 // same set, other order
	step(put("quote.write", "quote.activate", "quote.cancel")) // adds a permission
	step(revoke)
	step(revoke) // already inactive
	step(put("quote.write"))
	base := epochs[0]
	if want := []int64{base, base + 1, base + 2, base + 2, base + 3}; !equal(epochs, want) {
		t.Fatalf("epochs = %v, want %v", epochs, want)
	}
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestScopesAreIsolatedByRowLevelSecurity(t *testing.T) {
	f := newFixture(t)
	others := map[string]kernel.Scope{
		"other company": {Tenant: f.scope.Tenant, Company: kernel.CompanyID(pgtest.UniqueID(t, "company."))},
		"other tenant":  {Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: f.scope.Company},
	}
	for name, s := range others {
		err := f.inScope(s, func(ctx context.Context, exec bentopg.Executor) error {
			var n int
			if err := exec.QueryRow(ctx, `SELECT count(*) FROM brant_auth.actor_grant WHERE tenant_id = $1 AND company_id = $2`,
				string(f.scope.Tenant), string(f.scope.Company)).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("%s: sees %d grants of %s", name, n, f.scope)
			}
			ctx, err := identity.WithPrincipal(ctx, identity.Principal{Actor: alice, Scope: s})
			if err != nil {
				return err
			}
			if _, err := f.auth.Authorize(ctx, exec, "quote.activate"); !errors.Is(err, authorization.ErrForbidden) {
				t.Errorf("%s: alice of %s authorized in %s: %v", name, f.scope, s, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		err = f.inScope(s, func(ctx context.Context, exec bentopg.Executor) error {
			_, err := exec.Exec(ctx, `INSERT INTO brant_auth.authority_scope (tenant_id, company_id) VALUES ($1, $2)`,
				string(f.scope.Tenant), pgtest.UniqueID(t, "company."))
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: writing a row of another scope: %v, want SQLSTATE 42501", name, err)
		}
	}
	err := f.inScope(others["other company"], func(ctx context.Context, exec bentopg.Executor) error {
		_, err := f.auth.Authorize(f.as(ctx, alice), exec, "quote.activate")
		return err
	})
	if !errors.Is(err, authorization.ErrScopeMismatch) {
		t.Errorf("principal of another scope than the transaction: %v", err)
	}
	q, err := f.db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var visible int
	if err = q.QueryRow(context.Background(), "SELECT count(*) FROM brant_auth.authority_scope").Scan(&visible); err != nil || visible != 0 {
		t.Errorf("outside a scope transaction %d rows are visible, %v", visible, err)
	}
}

// waitForLock returns once the backend whose pid arrives on pids waits for a lock, and fails if
// done fires first.
func waitForLock[T any](t *testing.T, pool *pgxpool.Pool, pids <-chan int32, done <-chan T) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	var pid int32
	select {
	case pid = <-pids:
	case v := <-done:
		t.Fatalf("finished before it started: %v", v)
	case <-deadline:
		t.Fatal("backend never started")
	}
	for {
		var waiting bool
		if err := pool.QueryRow(context.Background(),
			"SELECT coalesce(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1", pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case v := <-done:
			t.Fatalf("finished without waiting for the lock: %v", v)
		case <-deadline:
			t.Fatal("backend never waited for the lock")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type outcome struct {
	epoch int64
	err   error
}

// A command that holds the guard row commits first; the revocation waits for it and then takes
// effect, so the next command is denied.
func TestCommandHoldingTheGuardCommitsBeforeARevocation(t *testing.T) {
	f := newFixture(t)
	revoked := make(chan outcome, 1)
	var commandEpoch int64
	err := f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
		d, err := f.auth.Authorize(f.as(ctx, alice), exec, "quote.activate")
		if err != nil {
			return err
		}
		commandEpoch = d.Epoch
		pids := make(chan int32, 1)
		go func() {
			var o outcome
			o.err = f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
				var pid int32
				if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					return err
				}
				pids <- pid
				var err error
				o.epoch, err = authorization.Grants{}.Revoke(ctx, exec, f.scope, alice.ID, time.Now())
				return err
			})
			revoked <- o
		}()
		waitForLock(t, f.pool, pids, revoked)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r := <-revoked
	if r.err != nil || r.epoch != commandEpoch+1 {
		t.Fatalf("revocation = %+v, want epoch %d", r, commandEpoch+1)
	}
	if _, err := f.authorize(alice, "quote.activate"); !errors.Is(err, authorization.ErrForbidden) {
		t.Fatalf("command after the revocation: %v", err)
	}
}

// A revocation that holds the guard row commits first; the command waits for it and is denied.
func TestRevocationHoldingTheGuardDeniesTheWaitingCommand(t *testing.T) {
	f := newFixture(t)
	decided := make(chan error, 1)
	err := f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
		if _, err := (authorization.Grants{}).Revoke(ctx, exec, f.scope, alice.ID, time.Now()); err != nil {
			return err
		}
		pids := make(chan int32, 1)
		go func() {
			decided <- f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
				var pid int32
				if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					return err
				}
				pids <- pid
				_, err := f.auth.Authorize(f.as(ctx, alice), exec, "quote.activate")
				return err
			})
		}()
		waitForLock(t, f.pool, pids, decided)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-decided; !errors.Is(err, authorization.ErrForbidden) {
		t.Fatalf("command that waited for the revocation: %v, want ErrForbidden", err)
	}
}

func TestDataScopeChangesBumpTheEpochAndReachTheDecision(t *testing.T) {
	f := newFixture(t)
	before, err := f.authorize(alice, "quote.save_draft")
	if err != nil || before.DataScope != authorization.DataScopeOwn || before.Actor != alice {
		t.Fatalf("default decision %+v, %v", before, err)
	}
	set := func(scope authorization.DataScope, team string) (int64, error) {
		var epoch int64
		err := f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
			var err error
			epoch, err = authorization.Grants{}.SetDataScope(ctx, exec, f.scope, alice.ID, scope, team, time.Now())
			return err
		})
		return epoch, err
	}
	epoch, err := set(authorization.DataScopeTeam, "team.east")
	if err != nil || epoch != before.Epoch+1 {
		t.Fatalf("team scope: epoch %d, %v", epoch, err)
	}
	if again, err := set(authorization.DataScopeTeam, "team.east"); err != nil || again != epoch {
		t.Fatalf("an unchanged data scope must keep the epoch: %d, %v", again, err)
	}
	if _, err = set(authorization.DataScopeTeam, ""); !errors.Is(err, authorization.ErrInvalidPolicy) {
		t.Fatalf("team scope without a team: %v", err)
	}
	if _, err = set(authorization.DataScopeOwn, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = set(authorization.DataScopeTeam, "team.east"); err != nil {
		t.Fatal(err)
	}
	d, err := f.authorize(alice, "quote.save_draft")
	if err != nil || d.DataScope != authorization.DataScopeTeam || d.Team != "team.east" {
		t.Fatalf("decision after the change %+v, %v", d, err)
	}
}

func TestAccessNeedsAnActiveGrantButNoPermission(t *testing.T) {
	f := newFixture(t)
	access := func(actor kernel.ActorRef) (authorization.Decision, error) {
		var d authorization.Decision
		err := f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
			var err error
			d, err = f.auth.Access(f.as(ctx, actor), exec)
			return err
		})
		return d, err
	}
	if d, err := access(agent); err != nil || d.Actor != agent || d.Epoch < 1 {
		t.Fatalf("AI with a grant: %+v, %v", d, err)
	}
	if _, err := access(bob); !errors.Is(err, authorization.ErrForbidden) {
		t.Fatalf("actor without a grant: %v", err)
	}
	err := f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := authorization.Grants{}.Revoke(ctx, exec, f.scope, alice.ID, time.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access(alice); !errors.Is(err, authorization.ErrForbidden) {
		t.Fatalf("revoked actor: %v", err)
	}
}

// Qualify reads another actor's grant as it is now, in the caller's transaction (T03 A33: the
// reviewer an approval relies on): kind, activity, permissions and data scope; a missing grant is an
// inactive holding; another scope's grant is never read.
func TestQualifyReadsAnotherActorsCurrentGrant(t *testing.T) {
	f := newFixture(t)
	qualify := func(scope kernel.Scope, actor string) authorization.Holding {
		var h authorization.Holding
		err := f.inScope(scope, func(ctx context.Context, exec bentopg.Executor) error {
			var err error
			h, err = f.auth.Qualify(ctx, exec, actor)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := qualify(f.scope, alice.ID)
	if h.Actor != alice || !h.Active || !h.Holds("quote.activate") || h.Holds("quote.cancel") || h.DataScope != authorization.DataScopeOwn ||
		!h.Sees("alice", "") || h.Sees("bob", "") {
		t.Fatalf("alice %+v", h)
	}
	if h := qualify(f.scope, agent.ID); h.Actor != agent || !h.Holds("quote.write") {
		t.Fatalf("agent %+v", h)
	}
	if h := qualify(f.scope, bob.ID); h.Active || h.Holds("quote.write") || h.Sees("bob", "") {
		t.Fatalf("no grant %+v", h)
	}
	other := kernel.Scope{Tenant: f.scope.Tenant, Company: kernel.CompanyID(pgtest.UniqueID(t, "company."))}
	if h := qualify(other, alice.ID); h.Active {
		t.Fatalf("another company %+v", h)
	}
	err := f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
		g := authorization.Grants{}
		if _, err := g.SetDataScope(ctx, exec, f.scope, alice.ID, authorization.DataScopeTeam, "team.east", time.Now()); err != nil {
			return err
		}
		_, err := g.Put(ctx, exec, f.scope, alice, []string{"quote.write"}, time.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if h := qualify(f.scope, alice.ID); h.Holds("quote.activate") || !h.Sees("carol", "team.east") || h.Sees("carol", "team.west") {
		t.Fatalf("after the change %+v", h)
	}
	err = f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := authorization.Grants{}.Revoke(ctx, exec, f.scope, alice.ID, time.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if h := qualify(f.scope, alice.ID); h.Active || h.Holds("quote.write") || h.Sees("alice", "") {
		t.Fatalf("revoked %+v", h)
	}
}

// BC-D02: roles derive permissions; tenant roles merge templates; forbidden combinations are
// refused at definition and at assignment; assignments keep their history.
func TestRolesDerivePermissionsAndKeepSeparationOfDuties(t *testing.T) {
	f := newFixture(t)
	g := authorization.Grants{}
	var epoch int64
	run := func(fn func(context.Context, bentopg.Executor) (int64, error)) error {
		return f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
			var err error
			epoch, err = fn(ctx, exec)
			return err
		})
	}
	err := run(func(ctx context.Context, exec bentopg.Executor) (int64, error) {
		return g.AssignRoles(ctx, exec, f.scope, bob, []string{"sales_clerk"}, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	salesEpoch := epoch
	err = run(func(ctx context.Context, exec bentopg.Executor) (int64, error) {
		return g.AssignRoles(ctx, exec, f.scope, bob, []string{"sales_clerk", "credit_manager"}, time.Now())
	})
	if !errors.Is(err, authorization.ErrSeparationOfDuties) {
		t.Fatalf("sales with credit decisions: %v", err)
	}
	err = run(func(ctx context.Context, exec bentopg.Executor) (int64, error) {
		return g.DefineRole(ctx, exec, f.scope, "self.review", "自审", []string{"sales_clerk", "quote_reviewer"}, time.Now())
	})
	if !errors.Is(err, authorization.ErrSeparationOfDuties) {
		t.Fatalf("a role that reviews its own quotes: %v", err)
	}
	err = run(func(ctx context.Context, exec bentopg.Executor) (int64, error) {
		return g.DefineRole(ctx, exec, f.scope, "trade.clerk", "外贸业务员", []string{"sales_clerk", "merchandiser"}, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	err = run(func(ctx context.Context, exec bentopg.Executor) (int64, error) {
		return g.AssignRoles(ctx, exec, f.scope, bob, []string{"trade.clerk"}, time.Now())
	})
	if err != nil || epoch <= salesEpoch {
		t.Fatalf("merged role: epoch %d after %d, %v", epoch, salesEpoch, err)
	}
	err = run(func(ctx context.Context, exec bentopg.Executor) (int64, error) {
		return g.AssignRoles(ctx, exec, f.scope, bob, []string{"trade.clerk", "credit_officer"}, time.Now())
	})
	if !errors.Is(err, authorization.ErrSeparationOfDuties) {
		t.Fatalf("a merged role still keeps sales from credit: %v", err)
	}
	// Redefining the role re-derives the holder's permissions.
	err = run(func(ctx context.Context, exec bentopg.Executor) (int64, error) {
		return g.DefineRole(ctx, exec, f.scope, "trade.clerk", "外贸业务员", []string{"sales_clerk"}, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	var history, current int
	err = f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE removed_epoch IS NULL) FROM brant_auth.actor_role
			WHERE actor_id = $1`, bob.ID).Scan(&history, &current)
	})
	if err != nil || history != 2 || current != 1 {
		t.Fatalf("assignment history %d, current %d, %v", history, current, err)
	}
	var holds bool
	err = f.inScope(f.scope, func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, `SELECT 'shipment.confirm' = ANY (permissions) FROM brant_auth.actor_grant WHERE actor_id = $1`, bob.ID).Scan(&holds)
	})
	if err != nil || holds {
		t.Fatalf("the redefined role no longer grants merchandiser permissions: %v %v", holds, err)
	}
}
