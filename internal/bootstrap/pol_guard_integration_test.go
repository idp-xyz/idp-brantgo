//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ConfigGuard v1 (ADR-BR-004 option B) as a downstream context uses it: a throwaway copy of what
// TRD will own (quote roots and the configuration revisions a quote adopts), with its own Bento
// transaction owner. Activating a quote locks the quote root FOR UPDATE first, then calls POL's
// guard function with the downstream's own executor (ADR-BR-011), like SC-04 B1–B3 of the M0
// spike, but against the real POL commands.

const downstreamDDL = `
CREATE TABLE {{schema}}.quote_root (
    tenant_id    text   NOT NULL,
    company_id   text   NOT NULL,
    quote_id     text   NOT NULL,
    status       text   NOT NULL CHECK (status IN ('draft', 'active')),
    root_version bigint NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant_id, company_id, quote_id)
);
CREATE TABLE {{schema}}.quote_config (
    tenant_id    text   NOT NULL,
    company_id   text   NOT NULL,
    quote_id     text   NOT NULL,
    kind         text   NOT NULL,
    config_id    text   NOT NULL,
    revision     bigint NOT NULL,
    content_hash text   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, quote_id, kind, config_id)
);
ALTER TABLE {{schema}}.quote_root ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.quote_root FORCE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.quote_config ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.quote_config FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_scope ON {{schema}}.quote_root
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY quote_config_scope ON {{schema}}.quote_config
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
GRANT SELECT, INSERT, UPDATE ON {{schema}}.quote_root, {{schema}}.quote_config TO {{app}};
`

var errGuardRefused = errors.New("downstream: configuration not in force")

type downstream struct {
	t       *testing.T
	app     *pgxpool.Pool
	db      *bentopg.DB // the downstream context's own transaction owner
	quotes  string
	adopted string
	scope   kernel.Scope
}

func newDownstream(w *world) *downstream {
	w.t.Helper()
	migrator, app := pgtest.MigratorPool(w.t), pgtest.AppPool(w.t)
	schema := pgx.Identifier{pgtest.Schema(w.t, migrator, app, "brant_t_trd_", downstreamDDL)}.Sanitize()
	db, err := bentopg.NewDB(app)
	w.must(err)
	return &downstream{t: w.t, app: app, db: db, quotes: schema + ".quote_root", adopted: schema + ".quote_config", scope: w.scope("company.A")}
}

func (d *downstream) in(scope kernel.Scope, fn func(ctx context.Context, exec bentopg.Executor) error) error {
	return bento.ScopeTransactor{DB: d.db, Scope: scope}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := d.db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
}

func (d *downstream) createQuote(id string) {
	d.t.Helper()
	err := d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, "INSERT INTO "+d.quotes+" (tenant_id, company_id, quote_id, status) VALUES ($1, $2, $3, 'draft')",
			string(d.scope.Tenant), string(d.scope.Company), id)
		return err
	})
	if err != nil {
		d.t.Fatal(err)
	}
}

// guard calls ConfigGuard v1 for ref at instant at in a transaction of scope, and returns the verdict.
func (d *downstream) guard(scope kernel.Scope, ref polapi.ConfigRef, at time.Time) (polapi.ConfigGuardVerdict, error) {
	var v polapi.ConfigGuardVerdict
	err := d.in(scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = polapi.ScanConfigGuard(ref, exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(ref, at)...).Scan)
		return err
	})
	return v, err
}

// stage lets a scenario interleave transactions: pid receives the backend pid when the transaction
// starts; hold runs after the guard passed, while the transaction holds its locks.
type stage struct {
	pid  chan<- int32
	hold func()
}

// activate is a downstream command: lock the quote root FOR UPDATE, guard the configuration revision
// the quote adopts, then activate the quote and record the revision, in one transaction.
func (d *downstream) activate(quoteID string, ref polapi.ConfigRef, s stage) (polapi.ConfigGuardVerdict, error) {
	var v polapi.ConfigGuardVerdict
	err := d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		if s.pid != nil {
			var pid int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			s.pid <- pid
		}
		t, c := string(d.scope.Tenant), string(d.scope.Company)
		var status string
		if err := exec.QueryRow(ctx, "SELECT status FROM "+d.quotes+" WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 FOR UPDATE",
			t, c, quoteID).Scan(&status); err != nil {
			return err
		}
		var err error
		if v, err = polapi.ScanConfigGuard(ref, exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(ref, time.Now())...).Scan); err != nil {
			return err
		}
		if err = v.Check("/args/config"); err != nil {
			return errors.Join(errGuardRefused, err)
		}
		if s.hold != nil {
			s.hold()
		}
		if _, err = exec.Exec(ctx, "UPDATE "+d.quotes+" SET status = 'active', root_version = root_version + 1 WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3",
			t, c, quoteID); err != nil {
			return err
		}
		_, err = exec.Exec(ctx, "INSERT INTO "+d.adopted+" (tenant_id, company_id, quote_id, kind, config_id, revision, content_hash) VALUES ($1, $2, $3, $4, $5, $6, $7)",
			t, c, quoteID, ref.Kind, ref.ConfigID, ref.Revision, v.ContentHash)
		return err
	})
	return v, err
}

// quote returns the quote's status and the revisions it adopted.
func (d *downstream) quote(id string) (status string, adopted int) {
	d.t.Helper()
	err := d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, "SELECT q.status, (SELECT count(*) FROM "+d.adopted+" a WHERE a.tenant_id = q.tenant_id AND a.company_id = q.company_id"+
			" AND a.quote_id = q.quote_id) FROM "+d.quotes+" q WHERE q.tenant_id = $1 AND q.company_id = $2 AND q.quote_id = $3",
			string(d.scope.Tenant), string(d.scope.Company), id).Scan(&status, &adopted)
	})
	if err != nil {
		d.t.Fatal(err)
	}
	return status, adopted
}

// await returns once cond holds; it fails the test if done fires first or 15 s pass.
func await[T any](t *testing.T, what string, cond func() bool, done <-chan T) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for !cond() {
		select {
		case v := <-done:
			t.Fatalf("finished before %s: %v", what, v)
		case <-deadline:
			t.Fatalf("timed out waiting until %s", what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// blocks reports whether backend pid holds a lock another backend waits for.
func (d *downstream) blocks(pid int32) bool {
	var blocking bool
	if err := d.app.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY (pg_blocking_pids(pid)))",
		pid).Scan(&blocking); err != nil {
		d.t.Fatal(err)
	}
	return blocking
}

// waitsForLock reports whether backend pid waits for a lock.
func (d *downstream) waitsForLock(pid int32) bool {
	var waiting bool
	if err := d.app.QueryRow(context.Background(), "SELECT coalesce(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1",
		pid).Scan(&waiting); err != nil {
		d.t.Fatal(err)
	}
	return waiting
}

// guardedConfig publishes the tax profile id for the window and returns its revision 1.
func (w *world) guardedConfig(editor, reviewer context.Context, id string, win window) polapi.ConfigRef {
	return w.tax(editor, reviewer, polapi.RecoveryNonrecoverable, id, win)
}

// The guard's verdicts, its scope, and how the database publishes it.
func TestConfigGuardVerdicts(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	d := newDownstream(w)
	ref := w.guardedConfig(editor, reviewer, "tax", win)
	resolved, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{ref}, time.Now())
	w.must(err)
	draft := w.configure(editor, reviewer, polapi.KindTaxProfile, "tax", 2, win.from, win.until, taxData(polapi.RecoveryRecoverable), false)
	for _, c := range []struct {
		ref    polapi.ConfigRef
		at     time.Time
		scope  kernel.Scope
		status string
		want   string
	}{
		{ref, time.Now(), d.scope, polapi.StatusActive, "OK"},
		{draft, time.Now(), d.scope, polapi.StatusWorking, polapi.CodeConfigNotActive},
		{ref, win.from, d.scope, polapi.StatusActive, polapi.CodeConfigNotActive}, // before its publication
		{ref, win.until, d.scope, polapi.StatusActive, polapi.CodeConfigExpired},
		{polapi.ConfigRef{Kind: polapi.KindTaxProfile, ConfigID: "tax", Revision: 9}, time.Now(), d.scope, "", kernel.CodeReferenceInvalid},
		{polapi.ConfigRef{Kind: polapi.KindCurrency, ConfigID: "tax", Revision: 1}, time.Now(), d.scope, "", kernel.CodeReferenceInvalid},
		{ref, time.Now(), w.scope("company.B"), "", kernel.CodeReferenceInvalid}, // another company never sees it
	} {
		v, err := d.guard(c.scope, c.ref, c.at)
		w.must(err)
		got := code(v.Check("/args/config"))
		if got != c.want || v.Status != c.status || v.InForce() != (c.want == "OK") {
			t.Errorf("%+v at %s in %s: %+v (%s), want %s", c.ref, c.at, c.scope, v, got, c.want)
		}
	}
	if v, _ := d.guard(d.scope, ref, time.Now()); v.ContentHash != resolved[ref].ContentHash || len(v.ContentHash) != 64 {
		t.Fatalf("the guard reports the content hash POL resolves: %+v", v)
	}
	if err = d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, polapi.ConfigGuardSQL, ref.Kind, ref.ConfigID, ref.Revision, nil)
		return err
	}); err == nil {
		t.Fatal("the guard needs an instant")
	}
	var definer, executable, public bool
	var config []string
	err = d.app.QueryRow(context.Background(), `SELECT p.prosecdef, p.proconfig,
		has_function_privilege('brant_app', p.oid, 'EXECUTE'), has_function_privilege('public', p.oid, 'EXECUTE')
		FROM pg_proc p WHERE p.oid = 'brant_pol.guard_config_in_force_v1(text, text, bigint, timestamptz)'::regprocedure`).
		Scan(&definer, &config, &executable, &public)
	w.must(err)
	if !definer || len(config) != 1 || config[0] != "search_path=pg_catalog" || !executable || public {
		t.Fatalf("security definer %v, config %v, brant_app %v, public %v", definer, config, executable, public)
	}
}

// ADR-BR-004 option B against POL: whichever transaction locks the revision row first decides.
func TestConfigGuardDecidesEveryCommitOrder(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	d := newDownstream(w)
	retire := func(id, key string, expected int64) error {
		_, err := w.c.POL.Retire(reviewer, polapi.Retire{IdempotencyKey: key, Kind: polapi.KindTaxProfile, ConfigID: id, ExpectedVersion: expected,
			Reason: "撤销依据"})
		return err
	}

	// B1: POL publishes the next revision, superseding the one the quote adopts, and commits; the
	// downstream guard then reports it not in force and the activation is refused.
	t.Run("POL commits first", func(t *testing.T) {
		ref := w.guardedConfig(editor, reviewer, "tax.b1", win)
		w.configure(editor, reviewer, polapi.KindTaxProfile, ref.ConfigID, 2, win.from, win.until, taxData(polapi.RecoveryRecoverable), true)
		d.createQuote("q.b1")
		v, err := d.activate("q.b1", ref, stage{})
		if !errors.Is(err, errGuardRefused) || code(err) != polapi.CodeConfigNotActive || v.Status != polapi.StatusSuperseded {
			t.Fatalf("activation %v, verdict %+v; want refused CONFIG_NOT_ACTIVE", err, v)
		}
		if status, adopted := d.quote("q.b1"); status != "draft" || adopted != 0 {
			t.Fatalf("quote %s with %d revisions", status, adopted)
		}
	})

	// B2: the downstream guard passes and holds the row FOR SHARE; drafting the next revision does
	// not wait for it, but retiring the revision does, until the downstream commits.
	t.Run("downstream holds the guard first", func(t *testing.T) {
		ref := w.guardedConfig(editor, reviewer, "tax.b2", win)
		d.createQuote("q.b2")
		type outcome struct {
			at  time.Time
			err error
		}
		retired := make(chan outcome, 1)
		pids := make(chan int32, 1)
		var released time.Time
		v, err := d.activate("q.b2", ref, stage{pid: pids, hold: func() {
			pid := <-pids
			drafted := make(chan error, 1)
			go func() {
				_, err := w.saveConfig(editor, polapi.KindTaxProfile, ref.ConfigID, 2, win.from, win.until, taxData(polapi.RecoveryRecoverable))
				drafted <- err
			}()
			select {
			case err := <-drafted:
				if err != nil {
					t.Errorf("drafting the next revision: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("drafting the next revision waited for the guard")
			}
			go func() {
				err := retire(ref.ConfigID, w.key(), 3)
				retired <- outcome{time.Now(), err}
			}()
			await(t, "the retirement waits on the guard", func() bool { return d.blocks(pid) }, retired)
			released = time.Now() // the activation writes and commits after this
		}})
		if err != nil || !v.InForce() {
			t.Fatalf("activation %v, verdict %+v", err, v)
		}
		r := <-retired
		if r.err != nil || !r.at.After(released) {
			t.Fatalf("retirement %v, returned after the activation moved on: %v", r.err, r.at.After(released))
		}
		if status, adopted := d.quote("q.b2"); status != "active" || adopted != 1 {
			t.Fatalf("quote %s with %d revisions", status, adopted)
		}
		after, err := d.guard(d.scope, ref, time.Now())
		w.must(err)
		if after.Status != polapi.StatusRetired || after.Code != polapi.CodeConfigNotActive {
			t.Fatalf("after the retirement %+v", after)
		}
	})

	// B3: POL's retirement updates the revision row and holds it (a transaction holding the same
	// idempotency key keeps it from committing); the downstream guard waits on the row; when the
	// retirement commits, the guard re-reads the row and the activation is refused.
	t.Run("POL holds the row first", func(t *testing.T) {
		ref := w.guardedConfig(editor, reviewer, "tax.b3", win)
		d.createQuote("q.b3")
		key := w.key()
		hold, blockerPID := make(chan struct{}), make(chan int32, 1)
		blocker := make(chan error, 1)
		go func() {
			blocker <- d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
				var pid int32
				if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					return err
				}
				if _, err := exec.Exec(ctx, `INSERT INTO brant_pol.action_result (tenant_id, company_id, command, idempotency_key, request_hash,
					action_id, object_id, row_version, status, actor_kind, actor_id, authority_epoch, correlation_id, recorded_at)
					VALUES ($1, $2, 'config.retire', $3, repeat('0', 64), 'act.blocker', 'blocker', 1, 'retired', 'human', 'blocker', 1,
					'corr.blocker', now())`, string(d.scope.Tenant), string(d.scope.Company), key); err != nil {
					return err
				}
				blockerPID <- pid
				<-hold
				return errors.New("blocker rolls back")
			})
		}()
		var pid int32
		select {
		case pid = <-blockerPID:
		case err := <-blocker:
			t.Fatalf("blocker: %v", err)
		}
		retired := make(chan error, 1)
		go func() { retired <- retire(ref.ConfigID, key, 2) }()
		await(t, "the retirement holds the revision row and waits on the blocker", func() bool { return d.blocks(pid) }, retired)
		type outcome struct {
			v   polapi.ConfigGuardVerdict
			err error
		}
		activated := make(chan outcome, 1)
		pids := make(chan int32, 1)
		go func() {
			v, err := d.activate("q.b3", ref, stage{pid: pids})
			activated <- outcome{v, err}
		}()
		downstreamPID := <-pids
		await(t, "the guard waits on the retirement", func() bool { return d.waitsForLock(downstreamPID) }, activated)
		close(hold)
		if err := <-blocker; err == nil {
			t.Fatal("the blocker must roll back")
		}
		if err := <-retired; err != nil {
			t.Fatalf("retirement: %v", err)
		}
		a := <-activated
		if !errors.Is(a.err, errGuardRefused) || code(a.err) != polapi.CodeConfigNotActive || a.v.Status != polapi.StatusRetired {
			t.Fatalf("activation %v, verdict %+v; want refused CONFIG_NOT_ACTIVE after the retirement", a.err, a.v)
		}
		if status, adopted := d.quote("q.b3"); status != "draft" || adopted != 0 {
			t.Fatalf("quote %s with %d revisions", status, adopted)
		}
	})
}
