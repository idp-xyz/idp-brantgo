//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// RfqGuard v1 (ADR-BR-004 option B, Context Map R08 and R12 with a guard lock) as a downstream
// context uses it: the throwaway quote schema of pol_guard_integration_test.go, whose activation
// locks the quote root FOR UPDATE first and then calls INQ's guard function with the downstream's
// own executor (ADR-BR-011), against the real INQ commands. The binding is recorded as an adopted
// row of kind rfq: RFQ ID, row version and line-set hash.

var errRfqRefused = errors.New("downstream: RFQ not ready as bound")

// rfqGuard calls RfqGuard v1 for b in a transaction of scope and returns the verdict.
func (d *downstream) rfqGuard(scope kernel.Scope, b inqapi.RfqBinding) (inqapi.RfqGuardVerdict, error) {
	var v inqapi.RfqGuardVerdict
	err := d.in(scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = inqapi.ScanRfqGuard(b, exec.QueryRow(ctx, inqapi.RfqGuardSQL, inqapi.RfqGuardArgs(b)...).Scan)
		return err
	})
	return v, err
}

// bindRfq is a downstream command: lock the quote root FOR UPDATE, guard the RFQ binding, then
// activate the quote and record the binding, in one transaction.
func (d *downstream) bindRfq(quoteID string, b inqapi.RfqBinding, s stage) (inqapi.RfqGuardVerdict, error) {
	var v inqapi.RfqGuardVerdict
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
		if v, err = inqapi.ScanRfqGuard(b, exec.QueryRow(ctx, inqapi.RfqGuardSQL, inqapi.RfqGuardArgs(b)...).Scan); err != nil {
			return err
		}
		if err = v.Check("/args/rfq_ref"); err != nil {
			return errors.Join(errRfqRefused, err)
		}
		if s.hold != nil {
			s.hold()
		}
		if _, err = exec.Exec(ctx, "UPDATE "+d.quotes+" SET status = 'active', root_version = root_version + 1 WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3",
			t, c, quoteID); err != nil {
			return err
		}
		_, err = exec.Exec(ctx, "INSERT INTO "+d.adopted+" (tenant_id, company_id, quote_id, kind, config_id, revision, content_hash) VALUES ($1, $2, $3, 'rfq', $4, $5, $6)",
			t, c, quoteID, b.RfqID, b.RowVersion, b.LinesHash)
		return err
	})
	return v, err
}

// readyRfq confirms RFQ id with the fixture's lines and returns its RfqRef v1 binding.
func (w *world) readyRfq(ctx context.Context, id string, lines ...inqapi.RfqLine) inqapi.RfqBinding {
	w.t.Helper()
	args := inqapi.SaveRfq{RfqID: id, CustomerID: "customer.1", ContactID: "contact.1", SourceIDs: []string{"src.1"}, UnresolvedFields: []string{},
		Lines: lines}
	_, err := w.save(ctx, args)
	w.must(err)
	_, err = w.confirm(ctx, id, 1, "src.1")
	w.must(err)
	refs, err := w.c.INQ.ResolveRfqs(ctx, []string{id})
	w.must(err)
	return refs[id].Binding()
}

func line(id, sku, quantity string) inqapi.RfqLine {
	return inqapi.RfqLine{LineID: id, SkuID: sku, Quantity: quantity, UOM: "PCS", SourceID: "src.1", Locator: "第" + id + "行"}
}

// The guard's verdicts, its scope, the line-set hash it shares with RfqLinesHash, and how the
// database publishes it.
func TestRfqGuardVerdicts(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.readyArgs(ctx) // src.1, customer.1, contact.1, sku.1
	w.sku(ctx, "sku.2", true, "src.1")
	d := newDownstream(w)
	ready := w.readyRfq(ctx, "rfq.ready", line("L2", "sku.2", "30.5"), line("L1", "sku.1", "120"), line("L10", "sku.1", "1"))
	refs, err := w.c.INQ.ResolveRfqs(ctx, []string{"rfq.ready", "rfq.none"})
	w.must(err)
	ref := refs["rfq.ready"]
	if len(refs) != 1 || !ref.Ready() || ref.RowVersion != 2 || len(ref.Lines) != 3 || ref.Lines[0].LineID != "L1" || ref.Lines[1].LineID != "L10" ||
		ref.Lines[0].SkuSpecRevision != 1 || ref.LinesHash != inqapi.RfqLinesHash(ref.Lines) {
		t.Fatalf("RfqRef %+v", refs)
	}
	_, err = w.save(ctx, inqapi.SaveRfq{RfqID: "rfq.draft", Lines: []inqapi.RfqLine{{LineID: "L1", RequestedItem: "待核实"}}})
	w.must(err)
	partial := ready
	partial.LinesHash = inqapi.RfqLinesHash(ref.Lines[:2])
	stale := ready
	stale.RowVersion = 1
	for _, c := range []struct {
		name    string
		b       inqapi.RfqBinding
		scope   kernel.Scope
		status  string
		version int64
		want    string
	}{
		{"ready as bound", ready, d.scope, inqapi.StatusReady, 2, "OK"},
		{"partial line set", partial, d.scope, inqapi.StatusReady, 2, inqapi.CodeReferenceStale},
		{"another row version", stale, d.scope, inqapi.StatusReady, 2, inqapi.CodeReferenceStale},
		{"draft", inqapi.RfqBinding{RfqID: "rfq.draft", RowVersion: 1, LinesHash: ready.LinesHash}, d.scope, inqapi.StatusDraft, 1, inqapi.CodeReferenceStale},
		{"unknown", inqapi.RfqBinding{RfqID: "rfq.none", RowVersion: 1, LinesHash: ready.LinesHash}, d.scope, "", 0, kernel.CodeReferenceInvalid},
		{"another company", ready, w.scope("company.B"), "", 0, kernel.CodeReferenceInvalid},
	} {
		v, err := d.rfqGuard(c.scope, c.b)
		w.must(err)
		got := code(v.Check("/args/rfq_ref"))
		if got != c.want || v.Status != c.status || v.RowVersion != c.version || v.Ready() != (c.want == "OK") {
			t.Errorf("%s: %+v (%s), want %s", c.name, v, got, c.want)
		}
		if c.want != kernel.CodeReferenceInvalid && c.name != "draft" && v.LinesHash != ref.LinesHash {
			t.Errorf("%s: the guard's hash %s differs from RfqLinesHash %s", c.name, v.LinesHash, ref.LinesHash)
		}
	}
	if v, _ := d.rfqGuard(d.scope, ready); code(v.Check("/x")) != "OK" {
		t.Fatal("ready")
	}
	if err = d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, inqapi.RfqGuardSQL, ready.RfqID, ready.RowVersion, nil)
		return err
	}); err == nil {
		t.Fatal("the guard needs a line-set hash")
	}
	var definer, executable, public bool
	var config []string
	err = d.app.QueryRow(context.Background(), `SELECT p.prosecdef, p.proconfig,
		has_function_privilege('brant_app', p.oid, 'EXECUTE'), has_function_privilege('public', p.oid, 'EXECUTE')
		FROM pg_proc p WHERE p.oid = 'brant_inq.guard_rfq_ready_v1(text, bigint, text)'::regprocedure`).
		Scan(&definer, &config, &executable, &public)
	w.must(err)
	if !definer || len(config) != 1 || config[0] != "search_path=pg_catalog" || !executable || public {
		t.Fatalf("security definer %v, config %v, brant_app %v, public %v", definer, config, executable, public)
	}
}

// ADR-BR-004 option B against INQ: whichever transaction locks the RFQ root row first decides.
func TestRfqGuardDecidesEveryCommitOrder(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.readyArgs(ctx)
	d := newDownstream(w)
	cancel := func(id, key string, expected int64) error {
		_, err := w.c.INQ.CancelRfq(ctx, inqapi.CancelRfq{IdempotencyKey: key, RfqID: id, ExpectedVersion: expected, Reason: "客户撤回询价"})
		return err
	}

	// INQ commits first: the RFQ is closed; the downstream guard then reports it stale and the
	// activation is refused.
	t.Run("INQ commits first", func(t *testing.T) {
		b := w.readyRfq(ctx, "rfq.b1", line("L1", "sku.1", "120"))
		_, err := w.c.INQ.CloseRfq(ctx, inqapi.CloseRfq{IdempotencyKey: w.key(), RfqID: "rfq.b1", ExpectedVersion: 2, Reason: "客户不再推进"})
		w.must(err)
		d.createQuote("q.b1")
		v, err := d.bindRfq("q.b1", b, stage{})
		if !errors.Is(err, errRfqRefused) || code(err) != inqapi.CodeReferenceStale || v.Status != inqapi.StatusClosed || v.RowVersion != 3 {
			t.Fatalf("activation %v, verdict %+v; want refused REFERENCE_STALE", err, v)
		}
		if status, adopted := d.quote("q.b1"); status != "draft" || adopted != 0 {
			t.Fatalf("quote %s with %d bindings", status, adopted)
		}
	})

	// The downstream holds the guard first: its FOR SHARE lock makes the cancellation wait until the
	// activation commits; afterwards the RFQ is cancelled and a new guard reports it stale.
	t.Run("downstream holds the guard first", func(t *testing.T) {
		b := w.readyRfq(ctx, "rfq.b2", line("L1", "sku.1", "120"))
		d.createQuote("q.b2")
		type outcome struct {
			at  time.Time
			err error
		}
		cancelled := make(chan outcome, 1)
		pids := make(chan int32, 1)
		var released time.Time
		v, err := d.bindRfq("q.b2", b, stage{pid: pids, hold: func() {
			pid, key := <-pids, w.key()
			go func() {
				err := cancel("rfq.b2", key, 2)
				cancelled <- outcome{time.Now(), err}
			}()
			await(t, "the cancellation waits on the guard", func() bool { return d.blocks(pid) }, cancelled)
			released = time.Now() // the activation writes and commits after this
		}})
		if err != nil || !v.Ready() {
			t.Fatalf("activation %v, verdict %+v", err, v)
		}
		r := <-cancelled
		if r.err != nil || !r.at.After(released) {
			t.Fatalf("cancellation %v, returned after the activation moved on: %v", r.err, r.at.After(released))
		}
		if status, adopted := d.quote("q.b2"); status != "active" || adopted != 1 {
			t.Fatalf("quote %s with %d bindings", status, adopted)
		}
		after, err := d.rfqGuard(d.scope, b)
		w.must(err)
		if after.Status != inqapi.StatusCancelled || after.Code != inqapi.CodeReferenceStale {
			t.Fatalf("after the cancellation %+v", after)
		}
	})

	// INQ holds the row first: the cancellation updated the RFQ row and waits on a transaction that
	// holds its idempotency key; the downstream guard waits on the row; when the cancellation
	// commits, the guard re-reads the row and the activation is refused.
	t.Run("INQ holds the row first", func(t *testing.T) {
		b := w.readyRfq(ctx, "rfq.b3", line("L1", "sku.1", "120"))
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
				if _, err := exec.Exec(ctx, `INSERT INTO brant_inq.action_result (tenant_id, company_id, command, idempotency_key, request_hash,
					action_id, object_id, row_version, status, actor_kind, actor_id, authority_epoch, correlation_id, recorded_at)
					VALUES ($1, $2, 'rfq.cancel', $3, repeat('0', 64), 'act.blocker', 'blocker', 1, 'cancelled', 'human', 'blocker', 1,
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
		cancelled := make(chan error, 1)
		go func() { cancelled <- cancel("rfq.b3", key, 2) }()
		await(t, "the cancellation holds the RFQ row and waits on the blocker", func() bool { return d.blocks(pid) }, cancelled)
		type outcome struct {
			v   inqapi.RfqGuardVerdict
			err error
		}
		activated := make(chan outcome, 1)
		pids := make(chan int32, 1)
		go func() {
			v, err := d.bindRfq("q.b3", b, stage{pid: pids})
			activated <- outcome{v, err}
		}()
		downstreamPID := <-pids
		await(t, "the guard waits on the cancellation", func() bool { return d.waitsForLock(downstreamPID) }, activated)
		close(hold)
		if err := <-blocker; err == nil {
			t.Fatal("the blocker must roll back")
		}
		if err := <-cancelled; err != nil {
			t.Fatalf("cancellation: %v", err)
		}
		a := <-activated
		if !errors.Is(a.err, errRfqRefused) || code(a.err) != inqapi.CodeReferenceStale || a.v.Status != inqapi.StatusCancelled {
			t.Fatalf("activation %v, verdict %+v; want refused REFERENCE_STALE after the cancellation", a.err, a.v)
		}
		if status, adopted := d.quote("q.b3"); status != "draft" || adopted != 0 {
			t.Fatalf("quote %s with %d bindings", status, adopted)
		}
	})
}
