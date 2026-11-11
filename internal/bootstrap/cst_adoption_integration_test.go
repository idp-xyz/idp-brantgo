//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// AdoptabilityGuard v1 (ADR-BR-004 option B, Context Map R11 CST→TRD, guardLock true; S2-06) as a
// downstream context uses it: a throwaway copy of what TRD will own (quote roots and the cost
// snapshot a quote adopts), with its own Bento transaction owner. Activating a quote locks its root
// FOR UPDATE first, then calls CST's guard function with the downstream's own executor
// (ADR-BR-011), against the real CST and SUP commands (Context Map SC-04: 否认先提交则拒绝).

const trdDownstreamDDL = `
CREATE TABLE {{schema}}.quote_root (
    tenant_id    text   NOT NULL,
    company_id   text   NOT NULL,
    quote_id     text   NOT NULL,
    status       text   NOT NULL CHECK (status IN ('approved', 'active')),
    root_version bigint NOT NULL DEFAULT 1,
    snapshot_id  text,
    result_hash  text,
    PRIMARY KEY (tenant_id, company_id, quote_id)
);
ALTER TABLE {{schema}}.quote_root ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.quote_root FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_scope ON {{schema}}.quote_root
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
GRANT SELECT, INSERT, UPDATE ON {{schema}}.quote_root TO {{app}};
`

var errAdoptionRefused = errors.New("downstream: cost snapshot not adoptable")

type trdDownstream struct {
	t      *testing.T
	app    *pgxpool.Pool
	db     *bentopg.DB // the downstream context's own transaction owner
	quotes string
	scope  kernel.Scope
}

func newTrdDownstream(w *scenWorld) *trdDownstream {
	w.t.Helper()
	migrator, app := pgtest.MigratorPool(w.t), pgtest.AppPool(w.t)
	schema := pgx.Identifier{pgtest.Schema(w.t, migrator, app, "brant_t_trd_", trdDownstreamDDL)}.Sanitize()
	db, err := bentopg.NewDB(app)
	w.must(err)
	return &trdDownstream{t: w.t, app: app, db: db, quotes: schema + ".quote_root", scope: w.scope("company.A")}
}

func (d *trdDownstream) in(fn func(ctx context.Context, exec bentopg.Executor) error) error {
	return bento.ScopeTransactor{DB: d.db, Scope: d.scope}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := d.db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
}

func (d *trdDownstream) approved(id string) {
	d.t.Helper()
	err := d.in(func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, "INSERT INTO "+d.quotes+" (tenant_id, company_id, quote_id, status) VALUES ($1, $2, $3, 'approved')",
			string(d.scope.Tenant), string(d.scope.Company), id)
		return err
	})
	if err != nil {
		d.t.Fatal(err)
	}
}

// activate is a downstream command: lock the quote root FOR UPDATE, guard the cost snapshot the
// quote adopts, then activate the quote and record the snapshot, in one transaction.
func (d *trdDownstream) activate(quoteID string, ref cstapi.CostSnapshotRef, s stage) (cstapi.AdoptabilityVerdict, error) {
	var v cstapi.AdoptabilityVerdict
	err := d.in(func(ctx context.Context, exec bentopg.Executor) error {
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
		if v, err = cstapi.ScanAdoptabilityGuard(ref, exec.QueryRow(ctx, cstapi.AdoptabilityGuardSQL, cstapi.AdoptabilityGuardArgs(ref, time.Now())...).Scan); err != nil {
			return err
		}
		if err = v.Check("/args/cost_snapshot_ref"); err != nil {
			return errors.Join(errAdoptionRefused, err)
		}
		if s.hold != nil {
			s.hold()
		}
		_, err = exec.Exec(ctx, "UPDATE "+d.quotes+" SET status = 'active', root_version = root_version + 1, snapshot_id = $4, result_hash = $5"+
			" WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3", t, c, quoteID, ref.SnapshotID, v.ResultHash)
		return err
	})
	return v, err
}

func (d *trdDownstream) quote(id string) string {
	d.t.Helper()
	var status string
	err := d.in(func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, "SELECT status FROM "+d.quotes+" WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3",
			string(d.scope.Tenant), string(d.scope.Company), id).Scan(&status)
	})
	if err != nil {
		d.t.Fatal(err)
	}
	return status
}

func (d *trdDownstream) blocks(pid int32) bool {
	var blocking bool
	if err := d.app.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY (pg_blocking_pids(pid)))",
		pid).Scan(&blocking); err != nil {
		d.t.Fatal(err)
	}
	return blocking
}

func (d *trdDownstream) waitsForLock(pid int32) bool {
	var waiting bool
	if err := d.app.QueryRow(context.Background(), "SELECT coalesce(wait_event_type = 'Lock', false) FROM pg_stat_activity WHERE pid = $1",
		pid).Scan(&waiting); err != nil {
		d.t.Fatal(err)
	}
	return waiting
}

// waitingOn reports whether some backend waits for a lock while running a statement containing text.
func (d *trdDownstream) waitingOn(text string) bool {
	var found bool
	if err := d.app.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock'
		AND position($1 in query) > 0)`, text).Scan(&found); err != nil {
		d.t.Fatal(err)
	}
	return found
}

// hold opens a transaction in company.A that inserts command's action result under key in
// brant_cst, so the CST command sent with that key commits only after the transaction rolls back,
// when release is called (at the latest when t ends, so a failing test does not leave it open). It
// returns the blocker's backend pid, release and the blocker's outcome.
func (d *trdDownstream) hold(t *testing.T, command, key string) (int32, func(), <-chan error) {
	t.Helper()
	pids, done := make(chan int32, 1), make(chan error, 1)
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	go func() {
		done <- d.in(func(ctx context.Context, exec bentopg.Executor) error {
			var pid int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			if _, err := exec.Exec(ctx, `INSERT INTO brant_cst.action_result (tenant_id, company_id, command, idempotency_key, request_hash,
				action_id, object_id, row_version, status, actor_kind, actor_id, authority_epoch, correlation_id, recorded_at)
				VALUES ($1, $2, $3, $4, repeat('0', 64), 'act.blocker', 'blocker', 1, 'blocked', 'human', 'blocker', 1, 'corr.blocker', now())`,
				string(d.scope.Tenant), string(d.scope.Company), command, key); err != nil {
				return err
			}
			pids <- pid
			<-ch
			return errors.New("blocker rolls back")
		})
	}()
	select {
	case pid := <-pids:
		return pid, release, done
	case err := <-done:
		t.Fatalf("blocker: %v", err)
	}
	return 0, release, nil
}

func (w *scenWorld) denyCmd(source string, evidence string) cstapi.AttestSource {
	return cstapi.AttestSource{IdempotencyKey: w.key(), AttestationID: w.next("denial"), SourceID: source, Verdict: cstapi.VerdictDenied,
		Reason: "冻结同时到达的有效反证", Evidence: []cstapi.Evidence{{SourceID: evidence, Locator: "独立反证"}}}
}

// The guard's verdicts and how the database publishes it: an unknown snapshot, another result hash
// and another company are REFERENCE_INVALID (known false); an empty hash binds the snapshot alone;
// the instant is required; the function is SECURITY DEFINER with a fixed search_path, executable by
// brant_app and not by PUBLIC.
func TestAdoptabilityGuardVerdicts(t *testing.T) {
	w, f, _ := newFrozenWorld(t)
	for _, c := range []struct {
		name, id, hash, want string
		company              string
	}{
		{"adoptable", f.SnapshotID, f.ResultHash, "OK", "company.A"},
		{"the snapshot alone", f.SnapshotID, "", "OK", "company.A"},
		{"unknown", "no.such.snapshot", "", kernel.CodeReferenceInvalid, "company.A"},
		{"another result hash", f.SnapshotID, strings.Repeat("0", 64), kernel.CodeReferenceInvalid, "company.A"},
		{"the evaluation is no snapshot", f.EvaluationID, "", kernel.CodeReferenceInvalid, "company.A"},
		{"another company", f.SnapshotID, "", kernel.CodeReferenceInvalid, "company.B"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ref := cstapi.CostSnapshotRef{SnapshotID: c.id, ResultHash: c.hash}
			var v cstapi.AdoptabilityVerdict
			err := bento.ScopeTransactor{DB: w.cstDB(), Scope: w.scope(c.company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
				exec, err := w.cstDB().RequireExecutor(ctx)
				if err != nil {
					return err
				}
				v, err = cstapi.ScanAdoptabilityGuard(ref, exec.QueryRow(ctx, cstapi.AdoptabilityGuardSQL, cstapi.AdoptabilityGuardArgs(ref, time.Now())...).Scan)
				return err
			})
			w.must(err)
			if got := code(v.Check("/ref")); got != c.want || (c.want == "OK" && (v.EvidenceHash != f.EvidenceHash || v.ResultHash != f.ResultHash)) {
				t.Fatalf("%+v: %s, want %s", v, got, c.want)
			}
		})
	}
	if err := w.exec("company.A", "SELECT * FROM brant_cst.guard_cost_snapshot_adoptable_v1('"+f.SnapshotID+"', NULL, NULL)"); err == nil {
		t.Fatal("the guard needs an instant")
	}
	var definer, executable, public bool
	var config []string
	w.must(pgtest.AppPool(t).QueryRow(context.Background(), `SELECT p.prosecdef, p.proconfig,
		has_function_privilege('brant_app', p.oid, 'EXECUTE'), has_function_privilege('public', p.oid, 'EXECUTE')
		FROM pg_proc p WHERE p.oid = 'brant_cst.guard_cost_snapshot_adoptable_v1(text, text, timestamptz)'::regprocedure`).
		Scan(&definer, &config, &executable, &public))
	if !definer || len(config) != 1 || config[0] != "search_path=pg_catalog" || !executable || public {
		t.Fatalf("security definer %v, config %v, brant_app %v, public %v", definer, config, executable, public)
	}
}

func (w *scenWorld) cstDB() *bentopg.DB {
	if w.db == nil {
		db, err := bentopg.NewDB(pgtest.AppPool(w.t))
		w.must(err)
		w.db = db
	}
	return w.db
}

// ADR-BR-004 option B against CST (Context Map SC-04, S2-06): whichever transaction locks the
// source's standing row first decides between a denial and a quote's activation; the guard also
// holds the rows of the snapshot's SUP inputs, so a withdrawal waits for an activation in flight.
func TestAdoptabilityGuardDecidesEveryCommitOrder(t *testing.T) {
	w, f, _ := newFrozenWorld(t)
	d := newTrdDownstream(w)
	ref := cstapi.CostSnapshotRef{SnapshotID: f.SnapshotID, ResultHash: f.ResultHash}

	// C1 (SUP's guard, transitively): the downstream holds the guard; withdrawing log.1 waits until
	// the activation commits, and the snapshot is not current after.
	t.Run("downstream holds the guard first against a SUP withdrawal", func(t *testing.T) {
		d.approved("quote.c1")
		withdrawn := make(chan timed, 1)
		pids := make(chan int32, 1)
		var released time.Time
		v, err := d.activate("quote.c1", ref, stage{pid: pids, hold: func() {
			pid := <-pids
			go func() {
				_, err := w.end(w.reviewer, supapi.KindLogisticsOffer, "log.1", 2, 1)
				withdrawn <- timed{time.Now(), err}
			}()
			await(t, "the withdrawal waits on the guard", func() bool { return d.blocks(pid) }, withdrawn)
			released = time.Now()
		}})
		if err != nil || !v.Adoptable() {
			t.Fatalf("activation %v, verdict %+v", err, v)
		}
		r := <-withdrawn
		if r.err != nil || !r.at.After(released) || d.quote("quote.c1") != "active" {
			t.Fatalf("withdrawal %v after %v", r.err, r.at.After(released))
		}
		if q := w.query(f.SnapshotID); q.Adoptability.Status != cstapi.AdoptionNotCurrent || q.Adoptability.Input != "/inputs/sup/logistics_offer/log.1/1" {
			t.Fatalf("after the withdrawal %+v", q.Adoptability)
		}
	})

	w2, f2, _ := newFrozenWorld(t)
	d2 := newTrdDownstream(w2)
	ref2 := cstapi.CostSnapshotRef{SnapshotID: f2.SnapshotID, ResultHash: f2.ResultHash}
	// C2: the downstream holds the guard; the denial waits until the activation commits, then the
	// snapshot is denied for every later activation.
	t.Run("downstream holds the guard first", func(t *testing.T) {
		d2.approved("quote.c2")
		denied := make(chan timed, 1)
		pids := make(chan int32, 1)
		var released time.Time
		counter := w2.counter()
		v, err := d2.activate("quote.c2", ref2, stage{pid: pids, hold: func() {
			pid := <-pids
			go func() {
				_, err := w2.c.CST.AttestSource(w2.reviewer, w2.denyCmd("src.1", counter))
				denied <- timed{time.Now(), err}
			}()
			await(t, "the denial waits on the guard", func() bool { return d2.blocks(pid) }, denied)
			released = time.Now()
		}})
		if err != nil || !v.Adoptable() {
			t.Fatalf("activation %v, verdict %+v", err, v)
		}
		r := <-denied
		if r.err != nil || !r.at.After(released) || d2.quote("quote.c2") != "active" {
			t.Fatalf("denial %v, after the activation %v", r.err, r.at.After(released))
		}
		d2.approved("quote.c2b")
		v, err = d2.activate("quote.c2b", ref2, stage{})
		if !errors.Is(err, errAdoptionRefused) || code(err) != cstapi.CodeCostSnapshotIneligible || v.Status != cstapi.AdoptionDenied {
			t.Fatalf("a later activation %v, verdict %+v", err, v)
		}
	})

	w3, f3, _ := newFrozenWorld(t)
	d3 := newTrdDownstream(w3)
	ref3 := cstapi.CostSnapshotRef{SnapshotID: f3.SnapshotID, ResultHash: f3.ResultHash}
	// C3: CST commits the denial first; the activation is refused and the quote stays approved.
	t.Run("CST commits a denial first", func(t *testing.T) {
		_, err := w3.c.CST.AttestSource(w3.reviewer, w3.denyCmd("evidence", w3.counter()))
		w3.must(err)
		d3.approved("quote.c3")
		v, err := d3.activate("quote.c3", ref3, stage{})
		if !errors.Is(err, errAdoptionRefused) || code(err) != cstapi.CodeCostSnapshotIneligible || v.Status != cstapi.AdoptionDenied ||
			v.Input != "/sources/evidence" || d3.quote("quote.c3") != "approved" {
			t.Fatalf("activation %v, verdict %+v", err, v)
		}
	})

	w4, f4, _ := newFrozenWorld(t)
	d4 := newTrdDownstream(w4)
	ref4 := cstapi.CostSnapshotRef{SnapshotID: f4.SnapshotID, ResultHash: f4.ResultHash}
	// C4: the denial updates the standing row and holds it (a transaction holding the same
	// idempotency key keeps it from committing); the guard waits on the row; when the denial
	// commits, the guard reads it and the activation is refused.
	t.Run("CST holds the row first", func(t *testing.T) {
		cmd := w4.denyCmd("src.1", w4.counter())
		blocker, release, blocked := d4.hold(t, cstapi.CommandAttestSource, cmd.IdempotencyKey)
		denied := make(chan error, 1)
		go func() {
			_, err := w4.c.CST.AttestSource(w4.reviewer, cmd)
			denied <- err
		}()
		await(t, "the denial holds the standing row and waits on the blocker", func() bool { return d4.blocks(blocker) }, denied)
		d4.approved("quote.c4")
		type outcome struct {
			v   cstapi.AdoptabilityVerdict
			err error
		}
		activated := make(chan outcome, 1)
		pids := make(chan int32, 1)
		go func() {
			v, err := d4.activate("quote.c4", ref4, stage{pid: pids})
			activated <- outcome{v, err}
		}()
		downstream := <-pids
		await(t, "the guard waits on the denial", func() bool { return d4.waitsForLock(downstream) }, activated)
		release()
		if err := <-blocked; err == nil {
			t.Fatal("the blocker must roll back")
		}
		if err := <-denied; err != nil {
			t.Fatalf("denial: %v", err)
		}
		a := <-activated
		if !errors.Is(a.err, errAdoptionRefused) || a.v.Status != cstapi.AdoptionDenied || a.v.Input != "/sources/src.1" || d4.quote("quote.c4") != "approved" {
			t.Fatalf("activation %v, verdict %+v; want refused after the denial", a.err, a.v)
		}
	})
}

// Python test_costing_domains.py test_freeze_racing_source_denial_cannot_leave_an_eligible_snapshot
// on PostgreSQL 16: a freeze and a denial of a source its evaluation rests on, in each commit order
// and concurrently. The denial always succeeds; the freeze either is refused (REFERENCE_STALE at the
// source, the scenario a draft) or commits a snapshot that is not adoptable afterwards.
func TestFreezeRacingSourceDenialCannotLeaveAnEligibleSnapshot(t *testing.T) {
	w := newScenWorld(t)
	d := newTrdDownstream(w)
	counter := w.counter()
	// race prepares a scenario whose offer cites a source of its own and returns its evaluation and
	// that source.
	race := func() (cstapi.EvaluationReceipt, string) {
		source := w.source(w.author, w.next("race.src"), "合成报价资料")
		o := w.offerWith(w.line())
		o.Evidence = []supapi.Evidence{{SourceID: source, Locator: "报价单"}}
		offer := w.next("race.offer")
		w.verified(supapi.KindSupplierOffer, offer, o)
		r, err := w.evaluated(w.scenario(offer, "log.1"))
		w.must(err)
		return r, source
	}
	settle := func(r cstapi.EvaluationReceipt, source string, frozen cstapi.SnapshotReceipt, freezeErr, denyErr error) {
		t.Helper()
		if denyErr != nil {
			t.Fatalf("denial: %v", denyErr)
		}
		if freezeErr != nil {
			if code(freezeErr) != cstapi.CodeReferenceStale || path(freezeErr) != "/sources/"+source {
				t.Fatalf("freeze: %s at %s (%v)", code(freezeErr), path(freezeErr), freezeErr)
			}
			if status, _ := w.scenarioStatus(r.ScenarioID); status != cstapi.ScenarioDraft || w.snapshotsOf(r.ScenarioID) != 0 {
				t.Fatalf("a refused freeze left %s with %d snapshots", status, w.snapshotsOf(r.ScenarioID))
			}
			return
		}
		q := w.query(frozen.SnapshotID)
		if q.CurrentlyEligibleForQuote || q.Adoptability.Status != cstapi.AdoptionDenied || q.Adoptability.Input != "/sources/"+source ||
			q.Snapshot.TotalMinor != "66000" {
			t.Fatalf("frozen snapshot after the denial: %+v", q.Adoptability)
		}
	}

	t.Run("the freeze holds the standing rows first", func(t *testing.T) {
		r, source := race()
		cmd := w.freezeCmd(r, 1)
		blocker, release, blocked := d.hold(t, cstapi.CommandFreezeSnapshot, cmd.IdempotencyKey)
		type outcome struct {
			f   cstapi.SnapshotReceipt
			err error
		}
		frozen := make(chan outcome, 1)
		go func() {
			f, err := w.c.CST.FreezeSnapshot(w.reviewer, cmd)
			frozen <- outcome{f, err}
		}()
		await(t, "the freeze holds its locks and waits on the blocker", func() bool { return d.blocks(blocker) }, frozen)
		denied := make(chan error, 1)
		go func() {
			_, err := w.c.CST.AttestSource(w.reviewer, w.denyCmd(source, counter))
			denied <- err
		}()
		await(t, "the denial waits on the freeze", func() bool { return d.waitingOn("brant_cst.source_standing") }, denied)
		release()
		if err := <-blocked; err == nil {
			t.Fatal("the blocker must roll back")
		}
		f := <-frozen
		if f.err != nil {
			t.Fatalf("the freeze that held the rows commits: %v", f.err)
		}
		settle(r, source, f.f, f.err, <-denied)
	})

	t.Run("the denial holds the standing row first", func(t *testing.T) {
		r, source := race()
		deny := w.denyCmd(source, counter)
		blocker, release, blocked := d.hold(t, cstapi.CommandAttestSource, deny.IdempotencyKey)
		denied := make(chan error, 1)
		go func() {
			_, err := w.c.CST.AttestSource(w.reviewer, deny)
			denied <- err
		}()
		await(t, "the denial holds the standing row and waits on the blocker", func() bool { return d.blocks(blocker) }, denied)
		type outcome struct {
			f   cstapi.SnapshotReceipt
			err error
		}
		frozen := make(chan outcome, 1)
		cmd := w.freezeCmd(r, 1)
		go func() {
			f, err := w.c.CST.FreezeSnapshot(w.reviewer, cmd)
			frozen <- outcome{f, err}
		}()
		await(t, "the freeze waits on the denial", func() bool { return d.waitingOn("brant_cst.source_standing") }, frozen)
		release()
		if err := <-blocked; err == nil {
			t.Fatal("the blocker must roll back")
		}
		denyErr := <-denied
		f := <-frozen
		if f.err == nil {
			t.Fatal("a freeze that waited on a committed denial is refused")
		}
		settle(r, source, f.f, f.err, denyErr)
	})

	t.Run("concurrently", func(t *testing.T) {
		for i := range 4 {
			r, source := race()
			cmd, deny := w.freezeCmd(r, 1), w.denyCmd(source, counter)
			type outcome struct {
				f   cstapi.SnapshotReceipt
				err error
			}
			frozen, denied := make(chan outcome, 1), make(chan error, 1)
			start := make(chan struct{})
			go func() {
				<-start
				f, err := w.c.CST.FreezeSnapshot(w.reviewer, cmd)
				frozen <- outcome{f, err}
			}()
			go func() {
				<-start
				_, err := w.c.CST.AttestSource(w.reviewer, deny)
				denied <- err
			}()
			close(start)
			f := <-frozen
			t.Logf("round %d: freeze %s", i, code(f.err))
			settle(r, source, f.f, f.err, <-denied)
		}
	})
}

// Python test_costing_domains.py test_working_days_use_real_verification_event_and_reviewed_overrides
// (the freeze and the eligibility after withdrawal): a snapshot whose delivery requirement counts
// from offer.1's verification is eligible; once offer.1 is withdrawn the anchor no longer holds and
// the snapshot is not adoptable, its history unchanged.
func TestWorkingDaysFreezeAndEligibilityAfterAnchorWithdrawal(t *testing.T) {
	w := newScenWorld(t)
	verified := w.condition(supapi.KindSupplierOffer, "offer.1").Revisions[0].Verification.At
	early := w.variant(func(l *supapi.ConditionLine) { l.Delivery.Date = workingDays(verified, 0) })
	ctx := w.leadContext("supplier_offer:offer.1", 3)
	d := w.scenario(early, "log.1")
	d.ContextRef = &cstapi.ContextRef{ContextID: ctx, Revision: 1}
	f := w.frozen(d)
	before := w.snapshot(f.SnapshotID)
	if got := requirement(cstapi.EvaluationView{Result: before.Result}); got.Date != workingDays(verified, 3) || f.TotalMinor != "66000" {
		t.Fatalf("requirement %+v", got)
	}
	if q := w.query(f.SnapshotID); !q.CurrentlyEligibleForQuote {
		t.Fatalf("query %+v", q.Adoptability)
	}
	_, err := w.end(w.reviewer, supapi.KindSupplierOffer, "offer.1", 2, 1)
	w.must(err)
	w.ineligible(f.SnapshotID, before, cstapi.AdoptionNotCurrent, "/inputs/sup/supplier_offer/offer.1/1")
}

// anchored verifies an offer whose content cites source and freezes a scenario whose delivery
// requirement counts from its verification; it returns the snapshot.
func (w *scenWorld) anchored(owner context.Context, source string) cstapi.SnapshotReceipt {
	w.t.Helper()
	o := w.offerWith(w.line())
	o.Evidence = []supapi.Evidence{{SourceID: source, Locator: "锚点原始来源"}}
	anchor := w.next("anchor")
	_, err := w.save(owner, supapi.KindSupplierOffer, anchor, 0, o)
	w.must(err)
	_, err = w.seal(w.reviewer, supapi.KindSupplierOffer, anchor, 1, 1, 0)
	w.must(err)
	ctx := w.leadContext("supplier_offer:"+anchor, 3)
	d := w.scenario(w.variant(func(l *supapi.ConditionLine) { l.Delivery.Date = "2026-01-01" }), "log.1")
	d.ContextRef = &cstapi.ContextRef{ContextID: ctx, Revision: 1}
	return w.frozen(d)
}

// Python test_costing_domains.py test_anchor_original_source_denial_blocks_new_adoption: the source
// the anchor's revision was read from is denied after the freeze; the snapshot is no longer
// adoptable and its history is unchanged.
func TestAnchorOriginalSourceDenialBlocksNewAdoption(t *testing.T) {
	w := newScenWorld(t)
	source := w.source(w.author, "anchor.origin", "锚点原资料")
	f := w.anchored(w.author, source)
	before := w.snapshot(f.SnapshotID)
	_, err := w.attest(w.reviewer, source, cstapi.VerdictDenied, w.counter())
	w.must(err)
	w.ineligible(f.SnapshotID, before, cstapi.AdoptionDenied, "/sources/"+source)
}

// Python test_costing_domains.py test_anchor_original_source_permission_is_rechecked_for_history,
// on the source itself: the anchor's revision cites a source of another owner; once the author's
// data scope is narrowed to its own records it still sees its scenario, but the evaluation and the
// snapshot are PERMISSION_DENIED.
func TestAnchorOriginalSourcePermissionIsRecheckedForHistory(t *testing.T) {
	w := newScenWorld(t)
	private := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "private.owner"},
		Permissions: allPermissions, DataScope: "company", Team: "legal"})
	source := w.source(private, "private.anchor.source", "合成锚点私有来源")
	f := w.anchored(w.author, source)
	_, err := w.c.CST.GetEvaluation(w.author, f.EvaluationID)
	w.must(err)
	_, err = w.c.CST.GetSnapshot(w.author, f.SnapshotID)
	w.must(err)
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "editor"},
		Permissions: allPermissions, DataScope: "own", Team: "trade"})
	if _, err = w.c.CST.GetScenario(w.author, f.ScenarioID); err != nil {
		t.Fatalf("the author still sees its scenario: %v", err)
	}
	_, err = w.c.CST.GetEvaluation(w.author, f.EvaluationID)
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.c.CST.GetSnapshot(w.author, f.SnapshotID)
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.c.CST.QuerySnapshot(w.author, f.SnapshotID)
	w.want(err, kernel.CodePermissionDenied)
}

// Python test_costing_domains.py test_anchor_transitive_source_denial_blocks_new_adoption, on
// Brant's dependency chain: the anchor is purchase terms formed from an offer whose content cites
// a source (Python: a logistics offer whose service role cites it; PTY is not upstream of CST); the
// source is denied after the evaluation, and the freeze is refused at it.
func TestAnchorTransitiveSourceDenialBlocksNewAdoption(t *testing.T) {
	w := newScenWorld(t)
	source := w.source(w.author, "terms.origin.source", "传递依赖原资料")
	o := w.offerWith(w.line())
	o.Evidence = []supapi.Evidence{{SourceID: source, Locator: "传递角色原资料"}}
	w.verified(supapi.KindSupplierOffer, "terms.origin", o)
	w.verified(supapi.KindPurchaseTerms, "anchor.terms", w.terms("terms.origin", supapi.ValidityIndependent))
	ctx := w.leadContext("purchase_terms:anchor.terms", 3)
	d := w.scenario(w.variant(func(l *supapi.ConditionLine) { l.Delivery.Date = "2026-01-01" }), "log.1")
	d.ContextRef = &cstapi.ContextRef{ContextID: ctx, Revision: 1}
	r, err := w.evaluated(d)
	w.must(err)
	if !strings.Contains(w.evaluation(r.EvaluationID).Input, fmt.Sprintf(`"id":"%s"`, source)) {
		t.Fatal("the anchor's source offer's source is part of the evaluation's input")
	}
	_, err = w.attest(w.reviewer, source, cstapi.VerdictDenied, w.counter())
	w.must(err)
	_, err = w.freeze(w.reviewer, r, 1)
	if code(err) != cstapi.CodeReferenceStale || path(err) != "/sources/"+source {
		t.Fatalf("freeze: %s at %s", code(err), path(err))
	}
}

// Python test_costing.py test_restart_keeps_raw_input_binding_separate_from_business_normalization
// (the freeze): the selection re-saved with "120.000000" is evaluated at version 2 and frozen by a
// second platform; the snapshot is currently eligible.
func TestRestartKeepsRawInputBindingSeparateFromBusinessNormalization(t *testing.T) {
	w := newScenWorld(t)
	d := w.scenario("offer.1", "log.1")
	d.Allocations[0].PurchaseQuantity = "120.000000"
	saved, err := w.saveScenario(w.author, "scenario.1", 1, d)
	w.must(err)
	r, err := w.evaluate(w.reviewer, "scenario.1", saved.RowVersion)
	w.must(err)
	platform, err := bootstrap.Open(context.Background(), bootstrap.Config{PostgresDSN: pgtest.AppDSN(t)})
	w.must(err)
	t.Cleanup(platform.Close)
	c, err := platform.Contexts()
	w.must(err)
	f, err := c.CST.FreezeSnapshot(w.reviewer, w.freezeCmd(r, saved.RowVersion))
	w.must(err)
	if q := w.query(f.SnapshotID); !q.CurrentlyEligibleForQuote || f.InputScenarioVersion != 2 || f.FrozenScenarioVersion != 3 {
		t.Fatalf("snapshot %+v, query %+v", f, q.Adoptability)
	}
}

// The freeze and the attestation through Python test_costing_failures.py
// PersistentFailureMatrixTests (authorization, human actor, late write rollback, payload conflict,
// stale version or duplicate, invalid state), and the end of Python test_costing_lifecycle.py
// test_all_35_costing_commands_persist_and_replay: the frozen snapshot is eligible, then a denial
// recorded.
func TestSnapshotCommandsFailureMatrix(t *testing.T) {
	w := newScenWorld(t)
	blocked := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "blocked"},
		Permissions: []string{"cst.read", "evd.read", "pol.read", "sup.read", "inq.read", "cat.read"}, DataScope: "company", Team: "trade"})
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "matrix.ai"},
		Permissions: allPermissions, DataScope: "company", Team: "trade"})
	store := func() string {
		return fmt.Sprint(w.count("company.A", "brant_cst.cost_snapshot"), w.count("company.A", "brant_cst.cost_snapshot_component"),
			w.count("company.A", "brant_cst.source_attestation"), w.count("company.A", "brant_cst.action_result"),
			w.count("company.A", "brant_cst.cost_scenario WHERE status = 'frozen'"), w.count("company.A", "brant_cst.source_standing WHERE status = 'denied'"))
	}
	before := func(run func(ctx context.Context) error, event string) {
		t.Helper()
		state := store()
		for _, c := range []struct {
			ctx  context.Context
			want string
		}{{blocked, kernel.CodePermissionDenied}, {ai, kernel.CodePermissionDenied}} {
			if got := code(run(c.ctx)); got != c.want || store() != state {
				t.Fatalf("%s, want %s; store changed %v", got, c.want, store() != state)
			}
		}
		drop := w.failOutbox(event)
		err := run(w.reviewer)
		drop()
		if _, isRejection := kernel.AsRejection(err); err == nil || isRejection || store() != state {
			t.Fatalf("late write failure: %v; store changed %v", err, store() != state)
		}
	}

	r, err := w.evaluate(w.reviewer, "scenario.1", 1)
	w.must(err)
	freeze := w.freezeCmd(r, 1)
	before(func(ctx context.Context) error {
		_, err := w.c.CST.FreezeSnapshot(ctx, freeze)
		return err
	}, cstapi.EventCostSnapshotFrozen)
	f, err := w.c.CST.FreezeSnapshot(w.reviewer, freeze)
	w.must(err)
	state := store()
	changed := freeze
	changed.ScenarioID += ".changed"
	_, err = w.c.CST.FreezeSnapshot(w.reviewer, changed)
	w.want(err, kernel.CodeIdempotencyPayloadConflict)
	stale := freeze
	stale.IdempotencyKey += ".stale"
	_, err = w.c.CST.FreezeSnapshot(w.reviewer, stale)
	w.want(err, kernel.CodeVersionConflict)
	invalid := freeze
	invalid.IdempotencyKey, invalid.ExpectedVersion = freeze.IdempotencyKey+".state", f.RowVersion
	_, err = w.c.CST.FreezeSnapshot(w.reviewer, invalid)
	w.want(err, kernel.CodeInvalidState)
	if store() != state {
		t.Fatal("a refused freeze changed the store")
	}
	if q := w.query(f.SnapshotID); !q.CurrentlyEligibleForQuote {
		t.Fatalf("query %+v", q.Adoptability)
	}

	deny := w.denyCmd("evidence", w.counter())
	before(func(ctx context.Context) error {
		_, err := w.c.CST.AttestSource(ctx, deny)
		return err
	}, cstapi.EventCostSourceDenied)
	denied, err := w.c.CST.AttestSource(w.reviewer, deny)
	w.must(err)
	if denied.Status != cstapi.VerdictDenied {
		t.Fatalf("denial %+v", denied)
	}
	state = store()
	changedDeny := deny
	changedDeny.AttestationID += ".changed"
	_, err = w.c.CST.AttestSource(w.reviewer, changedDeny)
	w.want(err, kernel.CodeIdempotencyPayloadConflict)
	staleDeny := deny
	staleDeny.IdempotencyKey += ".stale"
	_, err = w.c.CST.AttestSource(w.reviewer, staleDeny)
	w.want(err, kernel.CodeVersionConflict)
	invalidDeny := deny
	invalidDeny.IdempotencyKey, invalidDeny.AttestationID = deny.IdempotencyKey+".state", deny.AttestationID+".state"
	_, err = w.c.CST.AttestSource(w.reviewer, invalidDeny)
	w.want(err, kernel.CodeInvalidState)
	if store() != state {
		t.Fatal("a refused attestation changed the store")
	}
	if q := w.query(f.SnapshotID); q.CurrentlyEligibleForQuote || q.Adoptability.Status != cstapi.AdoptionDenied {
		t.Fatalf("after the denial %+v", q.Adoptability)
	}
	// A replay re-checks visibility (Python test_source_access_revocation_blocks_successful_replay_and_history,
	// on the freeze): the reviewer narrowed to its own records no longer receives the stored success.
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"},
		Permissions: allPermissions, DataScope: "own", Team: "other"})
	_, err = w.c.CST.FreezeSnapshot(w.reviewer, freeze)
	w.want(err, kernel.CodeNotFound)
}
