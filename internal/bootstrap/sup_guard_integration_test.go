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
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SupplyConditionGuard v1 (ADR-BR-004 option B, Context Map R09 and R20) as a downstream context
// uses it: a throwaway copy of what CST will own (cost snapshots and the supply condition
// revisions a snapshot adopts), with its own Bento transaction owner. Freezing a snapshot locks its
// root FOR UPDATE first, then calls SUP's guard function with the downstream's own executor
// (ADR-BR-011), against the real SUP commands.

const supDownstreamDDL = `
CREATE TABLE {{schema}}.snapshot_root (
    tenant_id    text   NOT NULL,
    company_id   text   NOT NULL,
    snapshot_id  text   NOT NULL,
    status       text   NOT NULL CHECK (status IN ('draft', 'frozen')),
    root_version bigint NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant_id, company_id, snapshot_id)
);
CREATE TABLE {{schema}}.snapshot_input (
    tenant_id    text   NOT NULL,
    company_id   text   NOT NULL,
    snapshot_id  text   NOT NULL,
    kind         text   NOT NULL,
    condition_id text   NOT NULL,
    revision     bigint NOT NULL,
    content_hash text   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, snapshot_id, kind, condition_id)
);
ALTER TABLE {{schema}}.snapshot_root ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.snapshot_root FORCE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.snapshot_input ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.snapshot_input FORCE ROW LEVEL SECURITY;
CREATE POLICY snapshot_scope ON {{schema}}.snapshot_root
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY snapshot_input_scope ON {{schema}}.snapshot_input
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
GRANT SELECT, INSERT, UPDATE ON {{schema}}.snapshot_root, {{schema}}.snapshot_input TO {{app}};
`

var errSupplyRefused = errors.New("downstream: supply condition not in force")

type supDownstream struct {
	t      *testing.T
	app    *pgxpool.Pool
	db     *bentopg.DB // the downstream context's own transaction owner
	roots  string
	inputs string
	scope  kernel.Scope
}

func newSupDownstream(s *supWorld) *supDownstream {
	s.t.Helper()
	migrator, app := pgtest.MigratorPool(s.t), pgtest.AppPool(s.t)
	schema := pgx.Identifier{pgtest.Schema(s.t, migrator, app, "brant_t_cst_", supDownstreamDDL)}.Sanitize()
	db, err := bentopg.NewDB(app)
	s.must(err)
	return &supDownstream{t: s.t, app: app, db: db, roots: schema + ".snapshot_root", inputs: schema + ".snapshot_input", scope: s.scope("company.A")}
}

func (d *supDownstream) in(scope kernel.Scope, fn func(ctx context.Context, exec bentopg.Executor) error) error {
	return bento.ScopeTransactor{DB: d.db, Scope: scope}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := d.db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
}

func (d *supDownstream) createSnapshot(id string) {
	d.t.Helper()
	err := d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, "INSERT INTO "+d.roots+" (tenant_id, company_id, snapshot_id, status) VALUES ($1, $2, $3, 'draft')",
			string(d.scope.Tenant), string(d.scope.Company), id)
		return err
	})
	if err != nil {
		d.t.Fatal(err)
	}
}

// guard calls SupplyConditionGuard v1 for ref at instant at in a transaction of scope.
func (d *supDownstream) guard(scope kernel.Scope, ref supapi.SupplyConditionRef, at time.Time) (supapi.SupplyConditionGuardVerdict, error) {
	var v supapi.SupplyConditionGuardVerdict
	err := d.in(scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = supapi.ScanSupplyConditionGuard(ref, exec.QueryRow(ctx, supapi.SupplyConditionGuardSQL, supapi.SupplyConditionGuardArgs(ref, at)...).Scan)
		return err
	})
	return v, err
}

// freeze is a downstream command: lock the snapshot root FOR UPDATE, guard the supply condition
// revision it adopts, then freeze the snapshot and record the revision, in one transaction.
func (d *supDownstream) freeze(snapshotID string, ref supapi.SupplyConditionRef, s stage) (supapi.SupplyConditionGuardVerdict, error) {
	var v supapi.SupplyConditionGuardVerdict
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
		if err := exec.QueryRow(ctx, "SELECT status FROM "+d.roots+" WHERE tenant_id = $1 AND company_id = $2 AND snapshot_id = $3 FOR UPDATE",
			t, c, snapshotID).Scan(&status); err != nil {
			return err
		}
		var err error
		if v, err = supapi.ScanSupplyConditionGuard(ref, exec.QueryRow(ctx, supapi.SupplyConditionGuardSQL, supapi.SupplyConditionGuardArgs(ref, time.Now())...).Scan); err != nil {
			return err
		}
		if err = v.Check("/args/allocations/0/source"); err != nil {
			return errors.Join(errSupplyRefused, err)
		}
		if s.hold != nil {
			s.hold()
		}
		if _, err = exec.Exec(ctx, "UPDATE "+d.roots+" SET status = 'frozen', root_version = root_version + 1 WHERE tenant_id = $1 AND company_id = $2 AND snapshot_id = $3",
			t, c, snapshotID); err != nil {
			return err
		}
		_, err = exec.Exec(ctx, "INSERT INTO "+d.inputs+" (tenant_id, company_id, snapshot_id, kind, condition_id, revision, content_hash) VALUES ($1, $2, $3, $4, $5, $6, $7)",
			t, c, snapshotID, ref.Kind, ref.ID, ref.Revision, v.ContentHash)
		return err
	})
	return v, err
}

func (d *supDownstream) snapshot(id string) (status string, adopted int) {
	d.t.Helper()
	err := d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, "SELECT q.status, (SELECT count(*) FROM "+d.inputs+" a WHERE a.tenant_id = q.tenant_id AND a.company_id = q.company_id"+
			" AND a.snapshot_id = q.snapshot_id) FROM "+d.roots+" q WHERE q.tenant_id = $1 AND q.company_id = $2 AND q.snapshot_id = $3",
			string(d.scope.Tenant), string(d.scope.Company), id).Scan(&status, &adopted)
	})
	if err != nil {
		d.t.Fatal(err)
	}
	return status, adopted
}

func (d *supDownstream) blocks(pid int32) bool {
	var blocking bool
	if err := d.app.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY (pg_blocking_pids(pid)))",
		pid).Scan(&blocking); err != nil {
		d.t.Fatal(err)
	}
	return blocking
}

func (d *supDownstream) waitsForLock(pid int32) bool {
	var waiting bool
	if err := d.app.QueryRow(context.Background(), "SELECT coalesce(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1",
		pid).Scan(&waiting); err != nil {
		d.t.Fatal(err)
	}
	return waiting
}

// The guard's verdicts, its scope, dependent purchase terms, and how the database publishes it.
func TestSupplyConditionGuardVerdicts(t *testing.T) {
	s := newSupWorld(t)
	d := newSupDownstream(s)
	offer := s.verified(supapi.KindSupplierOffer, "offer.1", s.offer())
	terms := s.verified(supapi.KindPurchaseTerms, "terms.dep", s.terms("offer.1", supapi.ValidityDependent))
	independent := s.verified(supapi.KindPurchaseTerms, "terms.ind", s.terms("offer.1", supapi.ValidityIndependent))
	logistics := s.verified(supapi.KindLogisticsOffer, "log.1", s.logistics())
	_, err := s.save(s.author, supapi.KindSupplierOffer, "offer.draft", 0, s.offer())
	s.must(err)
	draft := supapi.SupplyConditionRef{Kind: supapi.KindSupplierOffer, ID: "offer.draft", Revision: 1}
	resolved, err := s.c.SUP.ResolveSupplyConditions(s.author, []supapi.SupplyConditionRef{offer}, time.Now())
	s.must(err)
	v := s.condition(supapi.KindSupplierOffer, "offer.1")
	verifiedAt := v.Revisions[0].Verification.At
	for _, c := range []struct {
		ref    supapi.SupplyConditionRef
		at     time.Time
		scope  kernel.Scope
		status string
		want   string
	}{
		{offer, time.Now(), d.scope, supapi.StatusActive, "OK"},
		{terms, time.Now(), d.scope, supapi.StatusActive, "OK"},
		{independent, time.Now(), d.scope, supapi.StatusActive, "OK"},
		{logistics, time.Now(), d.scope, supapi.StatusActive, "OK"},
		{draft, time.Now(), d.scope, supapi.StatusDraft, supapi.CodeSupplyConditionNotCurrent},
		{offer, verifiedAt.Add(-time.Second), d.scope, supapi.StatusActive, supapi.CodeSupplyConditionNotCurrent}, // before its verification
		{offer, s.win.until, d.scope, supapi.StatusActive, supapi.CodeSupplyConditionExpired},                     // the end is exclusive
		{terms, s.win.until, d.scope, supapi.StatusActive, supapi.CodeSupplyConditionExpired},
		{supapi.SupplyConditionRef{Kind: supapi.KindSupplierOffer, ID: "offer.1", Revision: 9}, time.Now(), d.scope, "", kernel.CodeReferenceInvalid},
		{supapi.SupplyConditionRef{Kind: supapi.KindLogisticsOffer, ID: "offer.1", Revision: 1}, time.Now(), d.scope, "", kernel.CodeReferenceInvalid},
		{offer, time.Now(), s.scope("company.B"), "", kernel.CodeReferenceInvalid}, // another company never sees it
	} {
		v, err := d.guard(c.scope, c.ref, c.at)
		s.must(err)
		got := code(v.Check("/args/source"))
		if got != c.want || v.Status != c.status || v.InForce() != (c.want == "OK") {
			t.Errorf("%+v at %s in %s: %+v (%s), want %s", c.ref, c.at, c.scope, v, got, c.want)
		}
	}
	if v, _ := d.guard(d.scope, offer, time.Now()); v.ContentHash != resolved[offer].ContentHash || len(v.ContentHash) != 64 {
		t.Fatalf("the guard reports the content hash SUP resolves: %+v", v)
	}

	// Withdrawing the source offer ends dependent purchase terms with it; independent terms stay in
	// force on their own validity (T02 §13.8, A46).
	_, err = s.end(s.reviewer, supapi.KindSupplierOffer, "offer.1", 2, 1)
	s.must(err)
	for ref, want := range map[supapi.SupplyConditionRef]string{offer: supapi.CodeSupplyConditionNotCurrent,
		terms: supapi.CodeSupplyConditionNotCurrent, independent: ""} {
		v, err := d.guard(d.scope, ref, time.Now())
		s.must(err)
		if v.Code != want || !v.Known {
			t.Errorf("after the withdrawal of offer.1, %+v: %+v, want %q", ref, v, want)
		}
	}

	if err = d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, supapi.SupplyConditionGuardSQL, offer.Kind, offer.ID, offer.Revision, nil)
		return err
	}); err == nil {
		t.Fatal("the guard needs an instant")
	}
	var definer, executable, public bool
	var config []string
	err = d.app.QueryRow(context.Background(), `SELECT p.prosecdef, p.proconfig,
		has_function_privilege('brant_app', p.oid, 'EXECUTE'), has_function_privilege('public', p.oid, 'EXECUTE')
		FROM pg_proc p WHERE p.oid = 'brant_sup.guard_supply_condition_v1(text, text, bigint, timestamptz)'::regprocedure`).
		Scan(&definer, &config, &executable, &public)
	s.must(err)
	if !definer || len(config) != 1 || config[0] != "search_path=pg_catalog" || !executable || public {
		t.Fatalf("security definer %v, config %v, brant_app %v, public %v", definer, config, executable, public)
	}
}

// ADR-BR-004 option B against SUP: whichever transaction locks the revision row first decides.
func TestSupplyConditionGuardDecidesEveryCommitOrder(t *testing.T) {
	s := newSupWorld(t)
	d := newSupDownstream(s)

	// B1: SUP verifies the next revision, superseding the one the snapshot adopts, and commits; the
	// downstream guard then reports it not current and the freeze is refused.
	t.Run("SUP commits first", func(t *testing.T) {
		ref := s.verified(supapi.KindSupplierOffer, "offer.b1", s.offer())
		_, err := s.revise(s.author, ref.Kind, ref.ID, 2, 1)
		s.must(err)
		_, err = s.seal(s.reviewer, ref.Kind, ref.ID, 3, 2, 1)
		s.must(err)
		d.createSnapshot("snap.b1")
		v, err := d.freeze("snap.b1", ref, stage{})
		if !errors.Is(err, errSupplyRefused) || code(err) != supapi.CodeSupplyConditionNotCurrent || v.Status != supapi.StatusSuperseded {
			t.Fatalf("freeze %v, verdict %+v; want refused SUPPLY_CONDITION_NOT_CURRENT", err, v)
		}
		if status, adopted := d.snapshot("snap.b1"); status != "draft" || adopted != 0 {
			t.Fatalf("snapshot %s with %d inputs", status, adopted)
		}
	})

	// B2: the downstream guard passes and holds the revision row FOR SHARE; revising the offer does
	// not wait for it, but withdrawing the revision does, until the downstream commits.
	t.Run("downstream holds the guard first", func(t *testing.T) {
		ref := s.verified(supapi.KindSupplierOffer, "offer.b2", s.offer())
		d.createSnapshot("snap.b2")
		type outcome struct {
			at  time.Time
			err error
		}
		withdrawn := make(chan outcome, 1)
		pids := make(chan int32, 1)
		var released time.Time
		v, err := d.freeze("snap.b2", ref, stage{pid: pids, hold: func() {
			pid := <-pids
			revised := make(chan error, 1)
			go func() {
				_, err := s.revise(s.author, ref.Kind, ref.ID, 2, 1)
				revised <- err
			}()
			select {
			case err := <-revised:
				if err != nil {
					t.Errorf("revising the offer: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("revising the offer waited for the guard")
			}
			go func() {
				_, err := s.end(s.reviewer, ref.Kind, ref.ID, 3, 1)
				withdrawn <- outcome{time.Now(), err}
			}()
			await(t, "the withdrawal waits on the guard", func() bool { return d.blocks(pid) }, withdrawn)
			released = time.Now() // the freeze writes and commits after this
		}})
		if err != nil || !v.InForce() {
			t.Fatalf("freeze %v, verdict %+v", err, v)
		}
		r := <-withdrawn
		if r.err != nil || !r.at.After(released) {
			t.Fatalf("withdrawal %v, returned after the freeze moved on: %v", r.err, r.at.After(released))
		}
		if status, adopted := d.snapshot("snap.b2"); status != "frozen" || adopted != 1 {
			t.Fatalf("snapshot %s with %d inputs", status, adopted)
		}
		after, err := d.guard(d.scope, ref, time.Now())
		s.must(err)
		if after.Status != supapi.StatusWithdrawn || after.Code != supapi.CodeSupplyConditionNotCurrent {
			t.Fatalf("after the withdrawal %+v", after)
		}
	})

	// B3: SUP's withdrawal updates the revision row and holds it (a transaction holding the same
	// idempotency key keeps it from committing); the downstream guard waits on the row; when the
	// withdrawal commits, the guard re-reads the row and the freeze is refused.
	t.Run("SUP holds the row first", func(t *testing.T) {
		ref := s.verified(supapi.KindSupplierOffer, "offer.b3", s.offer())
		d.createSnapshot("snap.b3")
		key := s.key()
		hold, blockerPID := make(chan struct{}), make(chan int32, 1)
		blocker := make(chan error, 1)
		go func() {
			blocker <- d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
				var pid int32
				if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					return err
				}
				if _, err := exec.Exec(ctx, `INSERT INTO brant_sup.action_result (tenant_id, company_id, command, idempotency_key, request_hash,
					action_id, object_id, row_version, status, actor_kind, actor_id, authority_epoch, correlation_id, recorded_at)
					VALUES ($1, $2, 'supplier_offer.withdraw', $3, repeat('0', 64), 'act.blocker', 'blocker', 1, 'withdrawn', 'human', 'blocker', 1,
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
		withdrawn := make(chan error, 1)
		go func() {
			_, err := s.c.SUP.End(s.reviewer, supapi.End{IdempotencyKey: key, Kind: ref.Kind, ID: ref.ID, ExpectedVersion: 2, TargetRevision: 1,
				Reason: "撤销", Evidence: evidenceOf("撤销依据")})
			withdrawn <- err
		}()
		await(t, "the withdrawal holds the revision row and waits on the blocker", func() bool { return d.blocks(pid) }, withdrawn)
		type outcome struct {
			v   supapi.SupplyConditionGuardVerdict
			err error
		}
		frozen := make(chan outcome, 1)
		pids := make(chan int32, 1)
		go func() {
			v, err := d.freeze("snap.b3", ref, stage{pid: pids})
			frozen <- outcome{v, err}
		}()
		downstreamPID := <-pids
		await(t, "the guard waits on the withdrawal", func() bool { return d.waitsForLock(downstreamPID) }, frozen)
		close(hold)
		if err := <-blocker; err == nil {
			t.Fatal("the blocker must roll back")
		}
		if err := <-withdrawn; err != nil {
			t.Fatalf("withdrawal: %v", err)
		}
		f := <-frozen
		if !errors.Is(f.err, errSupplyRefused) || code(f.err) != supapi.CodeSupplyConditionNotCurrent || f.v.Status != supapi.StatusWithdrawn {
			t.Fatalf("freeze %v, verdict %+v; want refused SUPPLY_CONDITION_NOT_CURRENT after the withdrawal", f.err, f.v)
		}
		if status, adopted := d.snapshot("snap.b3"); status != "draft" || adopted != 0 {
			t.Fatalf("snapshot %s with %d inputs", status, adopted)
		}
	})
}
