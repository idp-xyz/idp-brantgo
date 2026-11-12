//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// PUR on TRD's real sales orders (Context Map R19: ActiveOrderRef v1 before PUR's transaction,
// OrderGuard v1 inside it), on the TRD order world (newOrderWorld): quote.1 for 120 PCS at 8.00 USD,
// 960.00 USD, on the 660.00 USD snapshot (600.00 purchase from offer.1 of supplier.1); accepted,
// converted to order.quote.1, reviewed and activated, the customer screened clear.

// purHeld is PUR's port to TRD on trdapi, holding PUR's transaction after OrderGuard v1 has locked
// the order row FOR SHARE when hold is set: it announces on held and waits for hold to close.
type purHeld struct {
	pur.Orders
	mu   sync.Mutex
	hold chan struct{}
	held chan struct{}
}

func (h *purHeld) GuardOrder(ctx context.Context, exec bentopg.Executor, b pur.OrderBinding) (pur.OrderVerdict, error) {
	v, err := h.Orders.GuardOrder(ctx, exec, b)
	h.mu.Lock()
	hold, held := h.hold, h.held
	h.mu.Unlock()
	if hold != nil && err == nil {
		held <- struct{}{}
		<-hold
	}
	return v, err
}

type purOrderWorld struct {
	*trdWorld
	order trdapi.OrderReceipt
	port  *purHeld
	hook  *purHook
	pur   *pur.Module
}

func newPurOrderWorld(t *testing.T) *purOrderWorld {
	q := newOrderWorld(t)
	w := &purOrderWorld{trdWorld: q, order: q.activeOrder("quote.1"), port: &purHeld{Orders: pur.TRDOrders(q.c.TRD)},
		hook: &purHook{SnapshotLookups: q.c.CST}}
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	w.pur, err = pur.New(db, pur.Upstream{Orders: w.port, Supply: q.c.SUP, Snapshots: w.hook, Parties: q.c.PTY, Configs: q.c.POL, Numbers: q.c.POL,
		Skus: q.c.CAT, Sources: q.c.EVD})
	w.must(err)
	return w
}

func (w *purOrderWorld) cmd(id, order, q, offer string) purapi.PrepareDraft {
	return purapi.PrepareDraft{IdempotencyKey: w.key(), DraftID: id, OrderID: order, Lines: []purapi.DraftLine{purLine(q, purSource(offer))},
		SelectionReason: purReason}
}

func (w *purOrderWorld) prepare(ctx context.Context, id, q, offer string) (purapi.DraftReceipt, error) {
	return w.pur.PrepareDraft(ctx, w.cmd(id, w.order.ObjectID, q, offer))
}

func (w *purOrderWorld) revoke() error {
	_, err := w.c.TRD.RevokeOrderApproval(w.reviewer, trdapi.RevokeOrderApproval{IdempotencyKey: w.key(), OrderID: w.order.ObjectID,
		ReviewID: "review.quote.1", Reason: "客户条件变化"})
	return err
}

// BRC-001's purchase draft (WBS S3-12 data path, BC-AT01; Python T04 A46 on the A38 order): the
// active 960.00 USD order's 120 PCS from supplier.1's offer at 5.00 is a 600.00 USD draft through
// the composed process (bootstrap's Contexts wires PUR to TRD's ActiveOrderRef v1 and OrderGuard
// v1), bound to the order's revision and hash, compared with the 600.00 purchase cost of the 660.00
// snapshot, holding the whole line; the line has nothing left for another draft.
func TestBRC001PurchaseDraftOf600FromTheActive960Order(t *testing.T) {
	w := newPurOrderWorld(t)
	facts, err := w.c.TRD.ResolveActiveOrders(w.author, []string{w.order.ObjectID})
	w.must(err)
	f := facts[w.order.ObjectID]
	if f.Status != trdapi.OrderActive || f.Code != "" || len(f.Lines) != 1 || f.Lines[0].NetMinor != "96000" {
		t.Fatalf("order %+v", f)
	}
	r, err := w.c.PUR.PrepareDraft(w.author, w.cmd("purchase.brc001", w.order.ObjectID, "120", "offer.1"))
	w.must(err)
	if r.NetMinor != "60000" || r.GrossMinor != "60000" || r.Currency != "USD" || r.SupplierID != "supplier.1" ||
		r.OrderHash != w.order.OrderHash || r.OrderRevision != w.order.OrderRevision || r.CommitmentState != purapi.CommitmentNotCommitted {
		t.Fatalf("receipt %+v", r)
	}
	v, err := w.c.PUR.GetDraft(w.reviewer, "purchase.brc001")
	w.must(err)
	l := v.Lines[0]
	doc := purDocument(t, v)
	order := doc["order"].(map[string]any)
	if l.NetMinor != "60000" || !l.SameSource || l.CostVarianceMinor != "0" || l.RequiredDate != "2027-01-31" || order["quote_id"] != "quote.1" ||
		order["order_review_id"] != "review.quote.1" || order["customer_id"] != "customer.1" ||
		doc["costing_snapshot"].(map[string]any)["snapshot_id"] != w.snap.SnapshotID {
		t.Fatalf("draft %+v order %v", l, order)
	}
	_, err = w.c.PUR.PrepareDraft(w.author, w.cmd("purchase.more", w.order.ObjectID, "1", "offer.1"))
	w.want(err, purapi.CodePurchaseAllocationExceeded)
	vs, err := w.c.PUR.GetAllocations(w.author, w.order.ObjectID)
	w.must(err)
	if len(vs) != 1 || vs[0].Demand != "120" || vs[0].Allocated != "120" || vs[0].OrderRevision != 1 {
		t.Fatalf("allocation %+v", vs)
	}
}

// PUR-INV-001 on TRD's orders: a draft order, a cancelled one and an order whose activating
// approval was revoked serve no purchase draft (TRD's verdicts ORDER_NOT_ACTIVE and
// ORDER_APPROVAL_REVOKED); a revocation that commits after PUR's lookups and before its transaction
// is OrderGuard v1's refusal inside it; an unknown order is NOT_FOUND.
func TestOrderNotActiveOrRevokedRefusesTheDraft(t *testing.T) {
	w := newPurOrderWorld(t)
	_, draftOrder := w.prepared("quote.2")
	_, err := w.pur.PrepareDraft(w.author, w.cmd("purchase.draft", draftOrder.ObjectID, "120", "offer.1"))
	w.want(err, purapi.CodeOrderNotActive)
	_, toCancel := w.prepared("quote.3")
	_, err = w.c.TRD.CancelOrder(w.author, trdapi.CancelOrder{IdempotencyKey: w.key(), OrderID: toCancel.ObjectID,
		ExpectedVersion: w.orderVersion(toCancel.ObjectID), Reason: "客户改用新报价", Evidence: w.proof})
	w.must(err)
	_, err = w.pur.PrepareDraft(w.author, w.cmd("purchase.cancelled", toCancel.ObjectID, "120", "offer.1"))
	w.want(err, purapi.CodeOrderNotActive)
	_, err = w.pur.PrepareDraft(w.author, w.cmd("purchase.none", "order.none", "120", "offer.1"))
	w.want(err, kernel.CodeNotFound)
	w.hook.then(func() { w.must(w.revoke()) })
	_, err = w.prepare(w.author, "purchase.revoked", "120", "offer.1")
	if code(err) != purapi.CodeOrderApprovalRevoked || path(err) != "/args/order_id" {
		t.Fatalf("revoked after the lookups: %s at %s", code(err), path(err))
	}
	_, err = w.prepare(w.author, "purchase.revoked", "120", "offer.1")
	w.want(err, purapi.CodeOrderApprovalRevoked)
	if w.count("company.A", "brant_pur.purchase_draft") != 0 || w.count("company.A", "brant_pur.demand_allocation") != 0 {
		t.Fatal("nothing stored")
	}
}

// ADR-BR-004 option B, ADR-BR-011 on R19: while PUR's draft transaction holds the order row FOR
// SHARE (OrderGuard v1, after PUR's own allocation root FOR UPDATE), TRD's order.approval.revoke
// waits; the draft commits on the order as it was, and stays history after the revocation, which
// refuses the next draft.
func TestTRDRevocationWaitsForTheDraftHoldingTheOrder(t *testing.T) {
	w := newPurOrderWorld(t)
	src := w.moq40()
	w.port.hold, w.port.held = make(chan struct{}), make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := w.prepare(w.author, "purchase.first", "80", src)
		done <- err
	}()
	select {
	case <-w.port.held:
	case err := <-done:
		t.Fatalf("PUR finished before holding the order: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("PUR never reached OrderGuard")
	}
	revoked := make(chan error, 1)
	go func() { revoked <- w.revoke() }()
	select {
	case err := <-revoked:
		close(w.port.hold)
		t.Fatalf("the revocation did not wait: %v (PUR: %v)", err, <-done)
	case <-time.After(500 * time.Millisecond):
	}
	w.port.mu.Lock()
	hold := w.port.hold
	w.port.hold = nil
	w.port.mu.Unlock()
	close(hold)
	if err := <-done; err != nil {
		t.Fatalf("PUR: %v", err)
	}
	if err := <-revoked; err != nil {
		t.Fatalf("the revocation after PUR committed: %v", err)
	}
	if v, err := w.pur.GetDraft(w.author, "purchase.first"); err != nil || v.State != purapi.StatusPrepared {
		t.Fatalf("the draft stays %+v, %v", v, err)
	}
	_, err := w.prepare(w.author, "purchase.second", "40", "offer.1")
	w.want(err, purapi.CodeOrderApprovalRevoked)
}

// PUR-INV-002 and TD-AT05 on the order line's real quantity: 121 of 120 is refused, 80 and 40 of
// two suppliers fill it, one more piece is refused; a line the order does not have is
// ORDER_LINE_NOT_FOUND. Voiding releases (TD-AT06) on the real order too.
func TestAllocationLimitsFollowTheOrderLine(t *testing.T) {
	w := newPurOrderWorld(t)
	a, b := w.moq40(), w.secondSupplier()
	_, err := w.prepare(w.author, "draft.over", "121", a)
	w.want(err, purapi.CodePurchaseAllocationExceeded)
	missing := w.cmd("draft.missing", w.order.ObjectID, "1", a)
	missing.Lines[0].OrderLineID = "L9"
	_, err = w.pur.PrepareDraft(w.author, missing)
	w.want(err, purapi.CodeOrderLineNotFound)
	_, err = w.prepare(w.author, "draft.A", "80", a)
	w.must(err)
	_, err = w.prepare(w.author, "draft.B", "40", b)
	w.must(err)
	_, err = w.prepare(w.author, "draft.C", "1", a)
	w.want(err, purapi.CodePurchaseAllocationExceeded)
	_, err = w.pur.VoidDraft(w.author, purapi.VoidDraft{IdempotencyKey: w.key(), DraftID: "draft.A", ExpectedVersion: 1, Reason: purVoidReason})
	w.must(err)
	r, err := w.prepare(w.author, "draft.D", "80", a)
	if err != nil || r.NetMinor != "40000" {
		t.Fatalf("the released 80: %+v, %v", r, err)
	}
	if v := w.condition(supapi.KindSupplierOffer, b); v.ActiveRevision != 1 {
		t.Fatal("offers unchanged")
	}
}

var errPurBlockerRollsBack = errors.New("the blocker rolls back")

// purKeyBlocker holds idempotency key of command in TRD's ledger in an open transaction of
// company.A: the TRD command with that key does all its writes, holding its row locks, and then
// waits to insert its result until release; the blocker then rolls back and the command commits.
func (w *purOrderWorld) purKeyBlocker(command, key string) (pid int32, release func()) {
	w.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	w.must(err)
	hold, pids, done := make(chan struct{}), make(chan int32, 1), make(chan error, 1)
	go func() {
		done <- bento.ScopeTransactor{DB: db, Scope: w.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
			exec, err := db.RequireExecutor(ctx)
			if err != nil {
				return err
			}
			var pid int32
			if err = exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			if _, err = exec.Exec(ctx, `INSERT INTO brant_trd.action_result (tenant_id, company_id, command, idempotency_key, request_hash,
				action_id, object_id, row_version, status, actor_kind, actor_id, authority_epoch, correlation_id, recorded_at)
				VALUES ($1, 'company.A', $2, $3, repeat('0', 64), 'act.blocker', 'blocker', 1, 'blocked', 'human', 'blocker', 1, 'corr.blocker', now())`,
				w.tenant, command, key); err != nil {
				return err
			}
			pids <- pid
			<-hold
			return errPurBlockerRollsBack
		})
	}()
	select {
	case pid = <-pids:
	case err := <-done:
		w.t.Fatalf("blocker: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(hold)
			if err := <-done; !errors.Is(err, errPurBlockerRollsBack) {
				w.t.Errorf("the blocker must roll back: %v", err)
			}
		})
	}
	w.t.Cleanup(release)
	return pid, release
}

// blocked counts the backends waiting on pid (depth 1) or on a backend that waits on pid (depth 2).
func (w *purOrderWorld) blocked(pid int32, depth int) int {
	w.t.Helper()
	var n int
	sql := `SELECT count(*) FROM pg_stat_activity a WHERE $1::int = ANY(pg_blocking_pids(a.pid))`
	if depth == 2 {
		sql = `SELECT count(*) FROM pg_stat_activity a WHERE EXISTS (SELECT 1 FROM pg_stat_activity b
			WHERE b.pid = ANY(pg_blocking_pids(a.pid)) AND $1::int = ANY(pg_blocking_pids(b.pid)))`
	}
	w.must(pgtest.AppPool(w.t).QueryRow(context.Background(), sql, pid).Scan(&n))
	return n
}

// ADR-BR-004 option B on R19, the other commit order: while TRD's order.approval.revoke holds the
// order row FOR UPDATE, PUR's OrderGuard v1 waits inside PUR's transaction; once the revocation
// commits, the guard reads the order as it left it and the draft is ORDER_APPROVAL_REVOKED, with
// nothing stored.
func TestDraftWaitsForARevocationInFlightAndIsRefused(t *testing.T) {
	w := newPurOrderWorld(t)
	src := w.moq40()
	key := w.key()
	pid, release := w.purKeyBlocker(trdapi.CommandRevokeOrderApproval, key)
	revoked := make(chan error, 1)
	go func() {
		_, err := w.c.TRD.RevokeOrderApproval(w.reviewer, trdapi.RevokeOrderApproval{IdempotencyKey: key, OrderID: w.order.ObjectID,
			ReviewID: "review.quote.1", Reason: "客户条件变化"})
		revoked <- err
	}()
	if !awaitInHold(t, "the revocation holds the order", func() bool { return w.blocked(pid, 1) > 0 }, revoked) {
		return
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.prepare(w.author, "purchase.late", "80", src)
		done <- err
	}()
	if !awaitInHold(t, "the draft waits on the order", func() bool { return w.blocked(pid, 2) > 0 }, done) {
		return
	}
	release()
	if err := <-revoked; err != nil {
		t.Fatalf("revocation: %v", err)
	}
	err := <-done
	if code(err) != purapi.CodeOrderApprovalRevoked || path(err) != "/args/order_id" {
		t.Fatalf("draft: %s at %s", code(err), path(err))
	}
	if w.count("company.A", "brant_pur.purchase_draft") != 0 || w.count("company.A", "brant_pur.demand_allocation") != 0 {
		t.Fatal("nothing stored")
	}
}
