//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// QualityReleaseGuard v1 (Context Map R26 QLT→FUL, guardLock; ADR-BR-004 option B, ADR-BR-011,
// ADR-BR-014; QLT-INV-002; WBS S5-02 acceptance "放行撤回后不能出运") as FUL will use it: a throwaway
// copy of what FUL will own — shipments, each confirmed on one quality release for a quantity, FUL
// keeping its own consumption of each release — with its own Bento transaction owner. Confirming a
// shipment locks the shipment row FOR UPDATE first, then calls QLT's guard function with the
// downstream's own executor, then checks FUL's consumption of the release and confirms.

const shipmentDDL = `
CREATE TABLE {{schema}}.shipment (
    tenant_id   text          NOT NULL,
    company_id  text          NOT NULL,
    shipment_id text          NOT NULL,
    status      text          NOT NULL CHECK (status IN ('prepared', 'confirmed')),
    release_id  text          NOT NULL,
    quantity    numeric(18,6) NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (tenant_id, company_id, shipment_id)
);
ALTER TABLE {{schema}}.shipment ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.shipment FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_scope ON {{schema}}.shipment
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
GRANT SELECT, INSERT, UPDATE ON {{schema}}.shipment TO {{app}};
`

var errReleaseInsufficient = errors.New("downstream: QUALITY_RELEASE_INSUFFICIENT")

type shipments struct {
	*downstream
	table string
}

func newShipments(w *world) *shipments {
	w.t.Helper()
	d := newDownstream(w)
	schema := pgx.Identifier{pgtest.Schema(w.t, pgtest.MigratorPool(w.t), d.app, "brant_t_ful_", shipmentDDL)}.Sanitize()
	return &shipments{downstream: d, table: schema + ".shipment"}
}

func (s *shipments) prepare(id, releaseID, quantity string) {
	s.t.Helper()
	if err := s.in(s.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, "INSERT INTO "+s.table+" (tenant_id, company_id, shipment_id, status, release_id, quantity) VALUES ($1, $2, $3, 'prepared', $4, $5::numeric)",
			string(s.scope.Tenant), string(s.scope.Company), id, releaseID, quantity)
		return err
	}); err != nil {
		s.t.Fatal(err)
	}
}

func (s *shipments) guard(scope kernel.Scope, id string, at time.Time) qltapi.QualityReleaseVerdict {
	s.t.Helper()
	ref := qltapi.QualityReleaseRef{ReleaseID: id}
	var v qltapi.QualityReleaseVerdict
	if err := s.in(scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = qltapi.ScanQualityReleaseGuard(ref, exec.QueryRow(ctx, qltapi.QualityReleaseGuardSQL, qltapi.QualityReleaseGuardArgs(ref, at)...).Scan)
		return err
	}); err != nil {
		s.t.Fatal(err)
	}
	return v
}

// confirm is FUL's shipment.confirm: lock the shipment FOR UPDATE, guard its release now, check that
// FUL's confirmed shipments on the release and this one stay within the released quantity, then
// confirm, in one transaction.
func (s *shipments) confirm(id string, st stage) (qltapi.QualityReleaseVerdict, error) {
	var v qltapi.QualityReleaseVerdict
	err := s.in(s.scope, func(ctx context.Context, exec bentopg.Executor) error {
		if st.pid != nil {
			var pid int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			st.pid <- pid
		}
		t, c := string(s.scope.Tenant), string(s.scope.Company)
		var releaseID, quantity string
		if err := exec.QueryRow(ctx, "SELECT release_id, quantity::text FROM "+s.table+" WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3 FOR UPDATE",
			t, c, id).Scan(&releaseID, &quantity); err != nil {
			return err
		}
		ref := qltapi.QualityReleaseRef{ReleaseID: releaseID}
		var err error
		if v, err = qltapi.ScanQualityReleaseGuard(ref, exec.QueryRow(ctx, qltapi.QualityReleaseGuardSQL, qltapi.QualityReleaseGuardArgs(ref, time.Now())...).Scan); err != nil {
			return err
		}
		if err = v.Check("/args/lines/0/quality_release_id"); err != nil {
			return errors.Join(errReferenceRefused, err)
		}
		var within bool
		if err = exec.QueryRow(ctx, "SELECT coalesce(sum(quantity), 0) + $4::numeric <= $5::numeric FROM "+s.table+
			" WHERE tenant_id = $1 AND company_id = $2 AND release_id = $3 AND status = 'confirmed'", t, c, releaseID, quantity, v.Quantity).Scan(&within); err != nil {
			return err
		}
		if !within {
			return errReleaseInsufficient
		}
		if st.hold != nil {
			st.hold()
		}
		_, err = exec.Exec(ctx, "UPDATE "+s.table+" SET status = 'confirmed' WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3", t, c, id)
		return err
	})
	return v, err
}

func (s *shipments) status(id string) string {
	s.t.Helper()
	var status string
	if err := s.in(s.scope, func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, "SELECT status FROM "+s.table+" WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3",
			string(s.scope.Tenant), string(s.scope.Company), id).Scan(&status)
	}); err != nil {
		s.t.Fatal(err)
	}
	return status
}

// The guard's verdicts at instants and in scopes, the lookup that answers the same before FUL's
// transaction, and how the database publishes the function.
func TestQualityReleaseGuardVerdicts(t *testing.T) {
	x := newQltWorld(t)
	s := newShipments(x.world)
	x.subjects.put(commitmentLine("commitment.1", "120"))
	_, err := x.inspect(x.qc, "insp.1", subjectOf("commitment.1"), "120", "120")
	x.must(err)
	from, until := x.now.Add(time.Hour).Truncate(time.Second), x.now.Add(2*time.Hour).Truncate(time.Second)
	cmd := x.grantCmd("rel.1", subjectOf("commitment.1"), "100.5")
	cmd.ValidFrom, cmd.ValidUntil = kernel.FormatTimestamp(from), kernel.FormatTimestamp(until)
	_, err = x.q.GrantRelease(x.qc, cmd)
	x.must(err)
	scope := x.scope("company.A")
	for _, c := range []struct {
		id     string
		at     time.Time
		scope  kernel.Scope
		known  bool
		usable bool
	}{
		{"rel.1", from.Add(-time.Microsecond), scope, true, false}, // not yet valid
		{"rel.1", from, scope, true, true},
		{"rel.1", until.Add(-time.Microsecond), scope, true, true},
		{"rel.1", until, scope, true, false}, // expired
		{"rel.none", from, scope, false, false},
		{"rel.1", from, x.scope("company.B"), false, false}, // another company never sees it
	} {
		v := s.guard(c.scope, c.id, c.at)
		if v.Known != c.known || v.Usable() != c.usable || (!c.usable && v.Code != qltapi.CodeQualityReleaseRequired) {
			t.Errorf("%s at %s in %s: %+v", c.id, c.at, c.scope, v)
		}
		if c.known && (v.Status != qltapi.StatusGranted || v.Quantity != "100.5" || v.Scope.Subject != subjectOf("commitment.1") ||
			v.Scope.OrderLineID != "OL1" || v.Scope.SupplierID != "supplier.1" || v.Scope.SkuID != "sku.1" || v.Scope.UnitID != "pcs" ||
			!v.ValidFrom.Equal(from) || !v.ValidUntil.Equal(until)) {
			t.Errorf("verdict %+v", v)
		}
	}
	got, err := x.q.ResolveReleases(x.qc, []string{"rel.1", "rel.none"}, from)
	x.must(err)
	r, ok := got[qltapi.QualityReleaseRef{ReleaseID: "rel.1"}]
	if len(got) != 1 || !ok || r.Code != "" || r.State != qltapi.StatusGranted || r.Quantity != "100.5" || r.Check("/x") != nil {
		t.Fatalf("lookup at the start %+v", got)
	}
	got, err = x.q.ResolveReleases(x.qc, []string{"rel.1"}, x.now)
	x.must(err)
	if r = got[qltapi.QualityReleaseRef{ReleaseID: "rel.1"}]; r.State != qltapi.StateNotYetValid || r.Code != qltapi.CodeQualityReleaseRequired {
		t.Fatalf("lookup now %+v", r)
	}
	ownScope := x.actor("reader.own", "qlt.read")
	if got, err = x.q.ResolveReleases(ownScope, []string{"rel.1"}, from); err != nil || len(got) != 0 {
		t.Fatalf("outside the data scope %v, %v", got, err)
	}
	if err = s.in(scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, qltapi.QualityReleaseGuardSQL, "rel.1", nil)
		return err
	}); err == nil {
		t.Fatal("the guard needs an instant")
	}
	var definer, executable, public bool
	var config []string
	err = s.app.QueryRow(context.Background(), `SELECT p.prosecdef, p.proconfig,
		has_function_privilege('brant_app', p.oid, 'EXECUTE'), has_function_privilege('public', p.oid, 'EXECUTE')
		FROM pg_proc p WHERE p.oid = 'brant_qlt.guard_quality_release_v1(text, timestamptz)'::regprocedure`).
		Scan(&definer, &config, &executable, &public)
	x.must(err)
	if !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" || !executable || public {
		t.Fatalf("security definer %v, config %v, brant_app %v, public %v", definer, config, executable, public)
	}
}

// ADR-BR-004 option B against QLT: whichever transaction locks the release row first decides
// (QLT-INV-002 "撤回与出运确认并发按锁序裁决").
func TestQualityReleaseGuardDecidesEveryCommitOrder(t *testing.T) {
	x := newQltWorld(t)
	s := newShipments(x.world)

	// The revocation commits first: the shipment's guard then refuses and nothing ships.
	t.Run("QLT commits first", func(t *testing.T) {
		x.released("commitment.b1", "120")
		s.prepare("ship.b1", "rel.commitment.b1", "120")
		_, err := x.revoke(x.qc, "rel.commitment.b1", x.key())
		x.must(err)
		v, err := s.confirm("ship.b1", stage{})
		if !errors.Is(err, errReferenceRefused) || code(err) != qltapi.CodeQualityReleaseRequired || v.Status != qltapi.StatusRevoked {
			t.Fatalf("confirmation %v, verdict %+v; want refused QUALITY_RELEASE_REQUIRED", err, v)
		}
		if s.status("ship.b1") != "prepared" {
			t.Fatal("shipped on a revoked release")
		}
	})

	// The shipment's guard passes and holds the release row FOR SHARE; the revocation waits until the
	// shipment commits, then revokes: the shipped quantity stays shipped.
	t.Run("downstream holds the guard first", func(t *testing.T) {
		x.released("commitment.b2", "120")
		s.prepare("ship.b2", "rel.commitment.b2", "120")
		type outcome struct {
			at  time.Time
			err error
		}
		revoked := make(chan outcome, 1)
		pids := make(chan int32, 1)
		var released time.Time
		v, err := s.confirm("ship.b2", stage{pid: pids, hold: func() {
			pid := <-pids
			go func() {
				_, err := x.revoke(x.qc, "rel.commitment.b2", x.key())
				revoked <- outcome{time.Now(), err}
			}()
			await(t, "the revocation waits on the guard", func() bool { return s.blocks(pid) }, revoked)
			released = time.Now() // the shipment confirms and commits after this
		}})
		if err != nil || !v.Usable() {
			t.Fatalf("confirmation %v, verdict %+v", err, v)
		}
		r := <-revoked
		if r.err != nil || !r.at.After(released) {
			t.Fatalf("revocation %v, returned after the shipment moved on: %v", r.err, r.at.After(released))
		}
		if s.status("ship.b2") != "confirmed" || s.guard(s.scope, "rel.commitment.b2", time.Now()).Status != qltapi.StatusRevoked {
			t.Fatal("after the revocation")
		}
	})

	// The revocation updates the release row and holds it (a transaction holding its idempotency key
	// keeps it from committing); the shipment's guard waits on the row and, when the revocation
	// commits, re-reads it and refuses.
	t.Run("QLT holds the row first", func(t *testing.T) {
		x.released("commitment.b3", "120")
		s.prepare("ship.b3", "rel.commitment.b3", "120")
		key := x.key()
		blockerPID, release := s.keyBlocker(t, "brant_qlt", qltapi.CommandRevokeRelease, key)
		revoked := make(chan error, 1)
		go func() {
			_, err := x.revoke(x.qc, "rel.commitment.b3", key)
			revoked <- err
		}()
		if !awaitInHold(t, "the revocation holds the release row and waits on the blocker", func() bool { return s.blocks(blockerPID) }, revoked) {
			release()
			return
		}
		type outcome struct {
			v   qltapi.QualityReleaseVerdict
			err error
		}
		confirmed := make(chan outcome, 1)
		pids := make(chan int32, 1)
		go func() {
			v, err := s.confirm("ship.b3", stage{pid: pids})
			confirmed <- outcome{v, err}
		}()
		downstreamPID := <-pids
		ok := awaitInHold(t, "the guard waits on the revocation", func() bool { return s.waitsForLock(downstreamPID) }, confirmed)
		release()
		if !ok {
			return
		}
		if err := <-revoked; err != nil {
			t.Fatalf("revocation: %v", err)
		}
		c := <-confirmed
		if !errors.Is(c.err, errReferenceRefused) || code(c.err) != qltapi.CodeQualityReleaseRequired || c.v.Status != qltapi.StatusRevoked {
			t.Fatalf("confirmation %v, verdict %+v; want refused QUALITY_RELEASE_REQUIRED after the revocation", c.err, c.v)
		}
		if s.status("ship.b3") != "prepared" {
			t.Fatal("shipped on a revoked release")
		}
	})
}

// BT21 ("放行200件后100件已出运，再撤回放行并重放事件 → 未发部分冻结；已发部分保留；重复撤回无重复扣量") and
// QLT-INV-002: 200 released, FUL ships 100 on the release, our QC revokes it; the other 100 can no
// longer ship and the shipped 100 stay shipped; revoking again changes nothing and publishes
// nothing; the revoked release still counts, so nothing becomes releasable again.
func TestRevocationFreezesTheUnshippedRestBT21(t *testing.T) {
	x := newQltWorld(t)
	s := newShipments(x.world)
	x.released("commitment.bt21", "200")
	s.prepare("ship.1", "rel.commitment.bt21", "100")
	if _, err := s.confirm("ship.1", stage{}); err != nil {
		t.Fatal(err)
	}
	s.prepare("ship.big", "rel.commitment.bt21", "100.000001")
	if _, err := s.confirm("ship.big", stage{}); !errors.Is(err, errReleaseInsufficient) {
		t.Fatalf("FUL's own consumption: %v", err)
	}
	key := x.key()
	_, err := x.revoke(x.agent, "rel.commitment.bt21", key)
	x.want(err, kernel.CodePermissionDenied)
	r, err := x.revoke(x.qc, "rel.commitment.bt21", key)
	if err != nil || r.Status != qltapi.StatusRevoked || r.RowVersion != 2 || r.Replayed {
		t.Fatalf("revoke %+v, %v", r, err)
	}
	s.prepare("ship.2", "rel.commitment.bt21", "100")
	if _, err = s.confirm("ship.2", stage{}); code(err) != qltapi.CodeQualityReleaseRequired {
		t.Fatalf("the unshipped 100 after the revocation: %v", err)
	}
	if s.status("ship.1") != "confirmed" || s.status("ship.2") != "prepared" {
		t.Fatal("the shipped part stays, the rest is frozen")
	}
	again, err := x.q.RevokeRelease(x.qc, qltapi.RevokeRelease{IdempotencyKey: x.key(), ReleaseID: "rel.commitment.bt21", Reason: "重复撤回"})
	if err != nil || !again.Replayed || again.RowVersion != 2 || again.Status != qltapi.StatusRevoked {
		t.Fatalf("second revoke %+v, %v", again, err)
	}
	replay, err := x.revoke(x.qc, "rel.commitment.bt21", key)
	if err != nil || !replay.Replayed || replay.ActionID != r.ActionID {
		t.Fatalf("replay %+v, %v", replay, err)
	}
	if e := x.qltEvents(qltapi.EventQualityReleaseRevoked); len(e) != 1 || e[0].Payload["release_id"] != "rel.commitment.bt21" ||
		e[0].Payload["released_quantity"] != "200" {
		t.Fatalf("revocation events %v", e)
	}
	v, err := x.q.GetRelease(x.qc, "rel.commitment.bt21")
	x.must(err)
	if v.State != qltapi.StatusRevoked || v.Revocation == nil || v.Revocation.Reason != "复检发现色差，撤回放行" || v.Revocation.RevokedBy != actorRef("human", "qc.1") {
		t.Fatalf("release %+v", v)
	}
	if st := x.standing("commitment.bt21"); st.Conforming != "200" || st.Released != "200" || st.Releasable != "0" ||
		len(st.Releases) != 1 || st.Releases[0].State != qltapi.StatusRevoked {
		t.Fatalf("standing %+v", st)
	}
	_, err = x.grant(x.qc, "rel.again", subjectOf("commitment.bt21"), "100")
	x.want(err, qltapi.CodeQualityReleaseExceedsConforming)
}
