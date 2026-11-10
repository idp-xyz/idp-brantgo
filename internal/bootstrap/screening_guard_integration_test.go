//go:build integration

package bootstrap_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// fulDDL is a throwaway downstream of PTY in the shape FUL will have (Context Map R44 PTY->FUL,
// guardLock): a consignment root that is released only for a consignee whose screening is clear.
const fulDDL = `
CREATE TABLE {{schema}}.consignment (
    tenant_id      text NOT NULL,
    company_id     text NOT NULL,
    consignment_id text NOT NULL,
    consignee_id   text NOT NULL,
    status         text NOT NULL CHECK (status IN ('planned', 'released')),
    screening_id   text,
    PRIMARY KEY (tenant_id, company_id, consignment_id)
);
ALTER TABLE {{schema}}.consignment ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.consignment FORCE ROW LEVEL SECURITY;
CREATE POLICY consignment_scope ON {{schema}}.consignment
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
GRANT SELECT, INSERT, UPDATE ON {{schema}}.consignment TO {{app}};
`

// screeningDownstream plays the downstream context: a transaction owner of its own (rule T1) on the
// application role, in company.A.
type screeningDownstream struct {
	s     *screeningWorld
	pool  *pgxpool.Pool
	db    *bentopg.DB
	table string
}

func newScreeningDownstream(s *screeningWorld) *screeningDownstream {
	app := pgtest.AppPool(s.t)
	name := pgtest.Schema(s.t, pgtest.MigratorPool(s.t), app, "brant_t_ful_", fulDDL)
	db, err := bentopg.NewDB(app)
	s.must(err)
	return &screeningDownstream{s: s, pool: app, db: db, table: pgx.Identifier{name, "consignment"}.Sanitize()}
}

func (d *screeningDownstream) in(fn func(ctx context.Context, exec bentopg.Executor) error) error {
	return bento.ScopeTransactor{DB: d.db, Scope: d.s.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := d.db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
}

func (d *screeningDownstream) plan(id, consignee string) {
	d.s.t.Helper()
	d.s.must(d.in(func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, "INSERT INTO "+d.table+" (tenant_id, company_id, consignment_id, consignee_id, status) VALUES ($1, $2, $3, $4, 'planned')",
			d.s.tenant, "company.A", id, consignee)
		return err
	}))
}

// release locks its own consignment root FOR UPDATE first, then calls PartyScreeningGuard v1 with
// its own executor (ADR-BR-011), and releases only a clear consignee. pid receives its backend pid;
// hold runs after the guard passed, while the transaction holds the guard's locks.
func (d *screeningDownstream) release(id string, pid chan<- int32, hold func()) error {
	return d.in(func(ctx context.Context, exec bentopg.Executor) error {
		if pid != nil {
			var p int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&p); err != nil {
				return err
			}
			pid <- p
		}
		var consignee string
		err := exec.QueryRow(ctx, "SELECT consignee_id FROM "+d.table+" WHERE tenant_id = $1 AND company_id = $2 AND consignment_id = $3 FOR UPDATE",
			d.s.tenant, "company.A", id).Scan(&consignee)
		if err != nil {
			return err
		}
		var g ptyapi.ScreeningGuardResult
		if err = exec.QueryRow(ctx, ptyapi.ScreeningGuardSQL, consignee, kernel.Instant(time.Now())).Scan(g.Targets()...); err != nil {
			return err
		}
		if err = g.Check("/args/consignee_id"); err != nil {
			return err
		}
		if hold != nil {
			hold()
		}
		_, err = exec.Exec(ctx, "UPDATE "+d.table+" SET status = 'released', screening_id = $4 WHERE tenant_id = $1 AND company_id = $2 AND consignment_id = $3",
			d.s.tenant, "company.A", id, g.ScreeningID)
		return err
	})
}

func (d *screeningDownstream) status(id string) string {
	d.s.t.Helper()
	var status string
	d.s.must(d.in(func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, "SELECT status FROM "+d.table+" WHERE tenant_id = $1 AND company_id = $2 AND consignment_id = $3",
			d.s.tenant, "company.A", id).Scan(&status)
	}))
	return status
}

// waitBlockedBy returns once some backend waits for a lock pid holds; it fails if done fires first.
func waitBlockedBy(t *testing.T, pool *pgxpool.Pool, pid int32, done <-chan error) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY (pg_blocking_pids(pid))", pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("finished without waiting for backend %d: %v", pid, err)
		case <-deadline:
			t.Fatalf("nothing waited for backend %d", pid)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitWaiting returns once backend pid waits for a lock; it fails if done fires first.
func waitWaiting(t *testing.T, pool *pgxpool.Pool, pid int32, done <-chan error) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(context.Background(), "SELECT coalesce(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1", pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("finished without waiting: %v", err)
		case <-deadline:
			t.Fatal("never waited for a lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// PartyScreeningGuard v1 (PTY-INV-002, ADR-BR-004 option B, ADR-BR-011): a downstream calls the
// guard with its own executor in its own transaction after locking its own root. A block, a new
// match or a new list version that commits first makes the downstream refuse; one that starts
// while the downstream holds the guard waits until the downstream commits; a block that holds the
// screening root first makes the guard wait and then see the block.
func TestPartyScreeningGuardDecidesEveryCommitOrder(t *testing.T) {
	s := newScreeningWorld(t)
	d := newScreeningDownstream(s)
	s.mustImport("cn.uel", refapi.JurisdictionCN, "v1", "2026-11-01", listEntry("E2", "Globex Corporation"))
	reg, err := s.regulation("screening", 0, 30, cnList)
	s.must(err)
	clear := func(id, name string) {
		t.Helper()
		s.customer(id, name)
		r, err := s.record(s.compliance, id, 0, reg)
		s.must(err)
		if r.Status != ptyapi.ScreeningClear {
			t.Fatalf("%s: %+v", id, r)
		}
		d.plan("c."+id, id)
	}
	block := func(id string, expected int64) error {
		_, err := s.c.PTY.BlockParty(s.compliance, ptyapi.BlockParty{IdempotencyKey: s.key(), PartyID: id, ExpectedVersion: expected, Reason: "制裁关联"})
		return err
	}

	t.Run("block and new match committed first", func(t *testing.T) {
		clear("p1", "Alpha Freight")
		s.must(block("p1", 1))
		if err := d.release("c.p1", nil, nil); code(err) != ptyapi.CodePartyScreeningNotClear || d.status("c.p1") != "planned" {
			t.Fatalf("release after a block: %v", err)
		}
		clear("p5", "Hooli Exports")
		s.mustImport("cn.uel", refapi.JurisdictionCN, "v2", "2026-11-02", listEntry("E2", "Globex Corporation"), listEntry("E9", "HOOLI EXPORTS"))
		r, err := s.record(s.compliance, "p5", 1, reg)
		s.must(err)
		if r.Status != ptyapi.ScreeningMatch {
			t.Fatalf("new match %+v", r)
		}
		err = d.release("c.p5", nil, nil)
		if rej, ok := kernel.AsRejection(err); !ok || rej.Code != ptyapi.CodePartyScreeningNotClear || rej.Fields[0].Reason != "Party screening is match: suspected_match" {
			t.Fatalf("release after a new match: %v", err)
		}
	})

	t.Run("release holds the guard; the block waits", func(t *testing.T) {
		clear("p2", "Bravo Logistics")
		pids := make(chan int32, 1)
		blocked := make(chan error, 1)
		var released, blockReturned time.Time
		err := d.release("c.p2", pids, func() {
			pid := <-pids
			go func() {
				err := block("p2", 1)
				blockReturned = time.Now()
				blocked <- err
			}()
			waitBlockedBy(t, d.pool, pid, blocked)
			released = time.Now() // the release writes and commits after this
		})
		s.must(err)
		s.must(<-blocked)
		if d.status("c.p2") != "released" || !blockReturned.After(released) {
			t.Fatalf("release %s, block returned after the release went on: %v", d.status("c.p2"), blockReturned.After(released))
		}
		s.wantGuard("p2", ptyapi.ScreeningBlocked, ptyapi.ReasonNegativeList)
	})

	t.Run("release holds the guard; a new list version waits", func(t *testing.T) {
		clear("p3", "Charlie Cargo")
		pids := make(chan int32, 1)
		imported := make(chan error, 1)
		err := d.release("c.p3", pids, func() {
			pid := <-pids
			go func() {
				_, err := s.importList(s.compliance, "cn.uel", refapi.JurisdictionCN, "v3", "2026-11-03", listEntry("E2", "Globex Corporation"))
				imported <- err
			}()
			waitBlockedBy(t, d.pool, pid, imported)
		})
		s.must(err)
		s.must(<-imported)
		if d.status("c.p3") != "released" {
			t.Fatal("the release committed first")
		}
		s.wantGuard("p3", ptyapi.ScreeningPending, ptyapi.ReasonListUpdated)
	})

	t.Run("block holds the screening root; the guard waits and sees it", func(t *testing.T) {
		clear("p4", "Delta Shipping")
		// Hold the scope's authority guard row, so the block stops after it locked the screening
		// root FOR UPDATE and before it commits (the ledger locks the root, then the guard rows).
		conn, err := d.pool.Acquire(context.Background())
		s.must(err)
		defer conn.Release()
		authority, err := conn.Begin(context.Background())
		s.must(err)
		defer func() { _ = authority.Rollback(context.Background()) }()
		var holder int32
		s.must(authority.QueryRow(context.Background(), `SELECT pg_backend_pid(), set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`,
			s.tenant).Scan(&holder, new(string), new(string)))
		_, err = authority.Exec(context.Background(), `SELECT authority_epoch FROM brant_auth.authority_scope WHERE tenant_id = $1 AND company_id = 'company.A'
			FOR NO KEY UPDATE`, s.tenant)
		s.must(err)
		blocked := make(chan error, 1)
		go func() { blocked <- block("p4", 1) }()
		waitBlockedBy(t, d.pool, holder, blocked)

		pids := make(chan int32, 1)
		released := make(chan error, 1)
		go func() { released <- d.release("c.p4", pids, nil) }()
		var pid int32
		select {
		case pid = <-pids:
		case err := <-released:
			t.Fatalf("release finished before it started: %v", err)
		}
		waitWaiting(t, d.pool, pid, released)
		s.must(authority.Rollback(context.Background()))
		s.must(<-blocked)
		err = <-released
		if rej, ok := kernel.AsRejection(err); !ok || rej.Code != ptyapi.CodePartyScreeningNotClear || d.status("c.p4") != "planned" {
			t.Fatalf("the waiting release must see the block: %v", err)
		}
		s.wantGuard("p4", ptyapi.ScreeningBlocked, ptyapi.ReasonNegativeList)
	})
}
