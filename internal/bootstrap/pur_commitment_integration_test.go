//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// PUR purchase commitments (WBS S5-01; tactical model PUR-AGG-PurchaseCommitment, SM-PUR-COMMITMENT,
// PUR-INV-009, PUR-INV-010; S5 decision D-14) on PostgreSQL 16, on the PUR worlds of S3: newPurWorld
// (order.1 through PUR's port to an in-memory TRD, every other upstream real) and newPurOrderWorld
// (TRD's real sales orders). Python V0.50 has no test to port: A47 is design only
// (docs/T04_A46_PURCHASE_DRAFT_REPORT_V0.1.md, "独立采购承诺审批设计（建议 A47）" points #1–#8 and "A47
// 最低验收"); the tests cite the points and the acceptance row A47-01.

// purClock is PUR's clock moved by shift, for validities that end within a test.
type purClock struct{ shift time.Duration }

func (c purClock) Now() time.Time { return time.Now().Add(c.shift) }

var (
	purSent     = []purapi.Evidence{{SourceID: "evidence", Locator: "合成采购单发出邮件"}}
	purReceived = []purapi.Evidence{{SourceID: "evidence", Locator: "合成供应商回签"}}
)

// purCommit drives the purchase commitments of the module m returns, in world w, with the
// purchase_order number sequence "po.seq" it publishes.
type purCommit struct {
	w   *scenWorld
	m   func() *pur.Module
	seq purapi.SequenceRef
}

func newPurCommit(w *scenWorld, m func() *pur.Module) *purCommit {
	ref := w.configure(w.author, w.reviewer, polapi.KindNumberSequence, "po.seq", 0, w.win.from, time.Time{}, polapi.ConfigData{
		NumberSequence: &polapi.NumberSequence{DocumentType: polapi.DocumentTypePurchaseOrder, Prefix: "PO-", Width: 5, Reset: polapi.ResetAnnual,
			TimeZone: "Asia/Shanghai"}}, true)
	return &purCommit{w: w, m: m, seq: purapi.SequenceRef{ID: ref.ConfigID, Revision: ref.Revision}}
}

func (p *purCommit) request(ctx context.Context, id, draftID, hash string) (purapi.CommitmentReceipt, error) {
	return p.m().RequestCommitment(ctx, purapi.RequestCommitment{IdempotencyKey: p.w.key(), CommitmentID: id, DraftID: draftID, DraftHash: hash})
}

func (p *purCommit) approveCmd(id string, version int64, hash string, acks ...purapi.Acknowledgement) purapi.DecideCommitment {
	return purapi.DecideCommitment{IdempotencyKey: p.w.key(), CommitmentID: id, ExpectedVersion: version, DraftHash: hash,
		Decision: purapi.DecisionApprove, ValidUntil: kernel.FormatTimestamp(time.Now().Add(2 * time.Hour)), Acknowledgements: acks}
}

func (p *purCommit) approve(ctx context.Context, id string, version int64, hash string, acks ...purapi.Acknowledgement) (purapi.CommitmentReceipt, error) {
	return p.m().DecideCommitment(ctx, p.approveCmd(id, version, hash, acks...))
}

func (p *purCommit) reject(ctx context.Context, id string, version int64, hash string) (purapi.CommitmentReceipt, error) {
	return p.m().DecideCommitment(ctx, purapi.DecideCommitment{IdempotencyKey: p.w.key(), CommitmentID: id, ExpectedVersion: version,
		DraftHash: hash, Decision: purapi.DecisionReject, Reason: "合成：价格高于核价"})
}

func (p *purCommit) revoke(ctx context.Context, id string, version int64) (purapi.CommitmentReceipt, error) {
	return p.m().RevokeCommitment(ctx, purapi.RevokeCommitment{IdempotencyKey: p.w.key(), CommitmentID: id, ExpectedVersion: version,
		Reason: "合成：供应商条件变化"})
}

func (p *purCommit) issueCmd(id string, version int64) purapi.IssueCommitment {
	return purapi.IssueCommitment{IdempotencyKey: p.w.key(), CommitmentID: id, ExpectedVersion: version, SequenceRef: p.seq,
		SentAt: kernel.FormatTimestamp(time.Now()), Evidence: purSent}
}

func (p *purCommit) issue(ctx context.Context, id string, version int64) (purapi.CommitmentReceipt, error) {
	return p.m().IssueCommitment(ctx, p.issueCmd(id, version))
}

func (p *purCommit) confirm(ctx context.Context, id string, version int64, deviation string) (purapi.CommitmentReceipt, error) {
	cmd := purapi.RecordSupplierConfirmation{IdempotencyKey: p.w.key(), CommitmentID: id, ExpectedVersion: version,
		Result: purapi.ConfirmationAsIssued, ReceivedAt: kernel.FormatTimestamp(time.Now()), Evidence: purReceived, SupplierReference: "SO-2026-88"}
	if deviation != "" {
		cmd.Result, cmd.Deviation = purapi.ConfirmationWithDeviations, deviation
	}
	return p.m().RecordSupplierConfirmation(ctx, cmd)
}

func (p *purCommit) view(ctx context.Context, id string) purapi.CommitmentView {
	p.w.t.Helper()
	v, err := p.m().GetCommitment(ctx, id)
	p.w.must(err)
	return v
}

// downstream runs DownstreamCommitments v1 (R71) as the application role in company's scope.
func (p *purCommit) downstream(company, orderID string) []string {
	p.w.t.Helper()
	out, err := p.downstreamRows(company, orderID)
	p.w.must(err)
	return out
}

func (p *purCommit) downstreamRows(company, orderID string) ([]string, error) {
	db, err := bentopg.NewDB(pgtest.AppPool(p.w.t))
	if err != nil {
		return nil, err
	}
	var out []string
	err = bento.ScopeTransactor{DB: db, Scope: p.w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		rows, err := exec.Query(ctx, purapi.DownstreamCommitmentsSQL, purapi.DownstreamCommitmentsArgs(orderID)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			x, err := purapi.ScanDownstreamCommitment(rows.Scan)
			if err != nil {
				return err
			}
			out = append(out, strings.Join([]string{x.Kind, x.ObjectID, x.DraftID, x.OrderLineID, strconv.FormatInt(x.OrderRevision, 10), x.State,
				x.Quantity, x.Disposition}, " "))
		}
		return rows.Err()
	})
	return out, err
}

func (p *purCommit) state(id string) (string, int64) {
	p.w.t.Helper()
	var status string
	var version int64
	p.w.must(p.w.execScan("company.A", "SELECT status, row_version FROM brant_pur.purchase_commitment WHERE commitment_id = '"+id+"'",
		&status, &version))
	return status, version
}

// purActionBlocker holds idempotency key of command in PUR's ledger in an open transaction of
// company.A: the PUR command with that key does all its writes, holding its row locks, and then waits
// to insert its result until release; the blocker then rolls back and the command commits.
func purActionBlocker(w *world, command, key string) (pid int32, release func()) {
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
			if _, err = exec.Exec(ctx, `INSERT INTO brant_pur.action_result (tenant_id, company_id, command, idempotency_key, request_hash,
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

// blockedOn counts the backends waiting on pid (depth 1) or on a backend that waits on pid (depth 2).
func blockedOn(w *world, pid int32, depth int) int {
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

// heldUntil runs first until it holds PUR's transaction at the fake TRD's OrderGuard (hold), then
// second, which must still be waiting after half a second; then lets first commit and returns both
// outcomes.
func (w *purWorld) heldUntil(t *testing.T, first, second func() error) (error, error) {
	t.Helper()
	w.orders.mu.Lock()
	w.orders.hold, w.orders.held = make(chan struct{}), make(chan struct{}, 1)
	hold := w.orders.hold
	w.orders.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- first() }()
	select {
	case <-w.orders.held:
	case err := <-done:
		t.Fatalf("finished before holding the order: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("never reached OrderGuard")
	}
	w.orders.mu.Lock()
	w.orders.hold = nil
	w.orders.mu.Unlock()
	waited := make(chan error, 1)
	go func() { waited <- second() }()
	select {
	case err := <-waited:
		close(hold)
		t.Fatalf("the second command did not wait: %v (first: %v)", err, <-done)
	case <-time.After(500 * time.Millisecond):
	}
	close(hold)
	return <-done, <-waited
}

// blockedBehind runs first with its PUR ledger key held by a blocker until first holds its row locks
// and waits, then second until it waits on first; then releases the blocker and returns both
// outcomes.
func (w *purWorld) blockedBehind(t *testing.T, command, key string, first, second func() error) (error, error) {
	t.Helper()
	pid, release := purActionBlocker(w.world, command, key)
	done := make(chan error, 1)
	go func() { done <- first() }()
	if !awaitInHold(t, "the first command holds its locks", func() bool { return blockedOn(w.world, pid, 1) > 0 }, done) {
		t.FailNow()
	}
	waited := make(chan error, 1)
	go func() { waited <- second() }()
	if !awaitInHold(t, "the second command waits on the first", func() bool { return blockedOn(w.world, pid, 2) > 0 }, waited) {
		t.FailNow()
	}
	release()
	return <-done, <-waited
}

// A47-01, positive path (SIM-01 step 9 向工厂下采购单并取得确认; PUR-INV-009 and PUR-INV-010 positive;
// A47 #1, #2, #4, #7): an AI agent requests the commitment of BRC-001's 600.00 draft, an independent
// human approves it, the purchaser issues it with POL's PO number and the evidence of sending, and the
// supplier's answers are recorded — with deviations (still issued), then as issued (confirmed). The
// draft is committed and is never voided (TD-AT06, PURCHASE_DRAFT_COMMITTED, also for the owner's
// database role); a second commitment of it is refused; the events are in the Outbox;
// DownstreamCommitments v1 and InspectionSubject v1 show the commitment.
func TestCommitmentRequestedApprovedIssuedAndConfirmed(t *testing.T) {
	w := newPurWorld(t)
	p := newPurCommit(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	agent := w.other("a47.agent", kernel.ActorAI, allPermissions...)
	req := purapi.RequestCommitment{IdempotencyKey: w.key(), CommitmentID: "commitment.1", DraftID: d.ObjectID, DraftHash: d.DraftHash}
	r, err := w.pur.RequestCommitment(agent, req)
	w.must(err)
	if r.Status != purapi.CommitmentRequested || r.RowVersion != 1 || r.Replayed || r.DraftHash != d.DraftHash || r.OrderHash != purOrderHash ||
		r.SupplierID != "supplier.1" || r.Currency != "USD" || !r.ValidUntil.IsZero() || r.PONumber != "" {
		t.Fatalf("request %+v", r)
	}
	again, err := w.pur.RequestCommitment(agent, req)
	if err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("replay %+v, %v", again, err)
	}
	req.IdempotencyKey = w.key()
	if answered, err := w.pur.RequestCommitment(agent, req); err != nil || !answered.Replayed || answered.ActionID == r.ActionID {
		t.Fatalf("answered from the stored commitment %+v, %v", answered, err)
	}
	if got := p.downstream("company.A", "order.1"); !slices.Equal(got, []string{"demand_allocation L1  L1 1 allocated 120 ",
		"purchase_commitment commitment.1 purchase.a46  1 requested  ", "purchase_draft purchase.a46 purchase.a46  1 prepared  "}) {
		t.Fatalf("downstream requested %q", got)
	}
	a, err := p.approve(w.reviewer, "commitment.1", 1, d.DraftHash)
	w.must(err)
	if a.Status != purapi.CommitmentApproved || a.RowVersion != 2 || a.ValidUntil.IsZero() {
		t.Fatalf("approve %+v", a)
	}
	i, err := p.issue(w.author, "commitment.1", 2)
	w.must(err)
	if i.Status != purapi.CommitmentIssued || i.RowVersion != 3 || !strings.HasPrefix(i.PONumber, "PO-") {
		t.Fatalf("issue %+v", i)
	}
	_, err = w.void(w.author, "purchase.a46", 1)
	w.want(err, purapi.CodePurchaseDraftCommitted)
	if got := sqlCode(w.purOwner("UPDATE brant_pur.purchase_draft SET status = 'voided', row_version = 2, voided_by_kind = 'human', " +
		"voided_by_id = 'x', void_epoch = 1, voided_at = prepared_at, void_reason = 'x'")); got != "23000" {
		t.Fatalf("the database refuses voiding a committed draft: %s", got)
	}
	c, err := p.confirm(agent, "commitment.1", 3, "交期延后 5 天")
	w.must(err)
	if c.Status != purapi.CommitmentIssued || c.RowVersion != 4 {
		t.Fatalf("with deviations %+v", c)
	}
	c, err = p.confirm(w.author, "commitment.1", 4, "")
	w.must(err)
	if c.Status != purapi.CommitmentConfirmed || c.RowVersion != 5 || c.PONumber != i.PONumber {
		t.Fatalf("as issued %+v", c)
	}
	_, err = p.confirm(w.author, "commitment.1", 5, "")
	w.want(err, kernel.CodeInvalidState)
	v := p.view(w.reviewer, "commitment.1")
	if v.Status != purapi.CommitmentConfirmed || v.State != purapi.CommitmentConfirmed || v.RequestedBy.ID != "a47.agent" ||
		v.RequestedBy.Kind != kernel.ActorAI || v.Decision != purapi.DecisionApprove || v.DecidedBy.ID != "reviewer" || v.IssuedBy.ID != "editor" ||
		v.PONumber != i.PONumber || v.SequenceRef != p.seq || len(v.IssueEvidence) != 1 || len(v.Confirmations) != 2 ||
		v.Confirmations[0].Result != purapi.ConfirmationWithDeviations || v.Confirmations[0].CommitmentVersion != 4 ||
		v.Confirmations[1].Result != purapi.ConfirmationAsIssued || v.Confirmations[1].SupplierReference != "SO-2026-88" || len(v.Acknowledgements) != 0 {
		t.Fatalf("view %+v", v)
	}
	dv := w.draft(w.author, "purchase.a46")
	if dv.CommitmentState != purapi.CommitmentCommitted || !slices.Equal(dv.CommitmentIDs, []string{"commitment.1"}) || dv.Status != purapi.StatusPrepared {
		t.Fatalf("draft %+v", dv)
	}
	_, err = p.request(w.author, "commitment.2", "purchase.a46", d.DraftHash)
	w.want(err, purapi.CodePurchaseDraftCommitted)
	list, err := w.pur.GetOrderCommitments(w.author, "order.1")
	if err != nil || len(list) != 1 || list[0].CommitmentID != "commitment.1" {
		t.Fatalf("order commitments %+v, %v", list, err)
	}
	var types []string
	for _, e := range w.events("company.A", purapi.Source) {
		types = append(types, e.Type)
		if e.Type == purapi.EventPurchaseCommitmentIssued && (e.Payload["po_number"] != i.PONumber || e.Payload["commitment_id"] != "commitment.1") {
			t.Fatalf("issued %v", e.Payload)
		}
	}
	if !slices.Equal(types, []string{purapi.EventPurchaseDraftPrepared, purapi.EventPurchaseCommitmentApproved, purapi.EventPurchaseCommitmentIssued,
		purapi.EventSupplierConfirmationRecorded, purapi.EventSupplierConfirmationRecorded}) {
		t.Fatalf("events %v", types)
	}
	if got := p.downstream("company.A", "order.1"); !slices.Equal(got, []string{"demand_allocation L1  L1 1 allocated 120 ",
		"purchase_commitment commitment.1 purchase.a46  1 confirmed  "}) {
		t.Fatalf("downstream confirmed %q", got)
	}
	lines, err := w.pur.ResolveCommitmentLines(w.reviewer, []purapi.CommitmentLineRef{{CommitmentID: "commitment.1", OrderLineID: "L1"},
		{CommitmentID: "commitment.1", OrderLineID: "L9"}})
	l := lines[purapi.CommitmentLineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}]
	if err != nil || len(lines) != 1 || l.Check("/x") != nil || l.PONumber != i.PONumber || l.PurchaseQuantity != "120" || l.SkuID != "sku.1" ||
		l.UnitID != "PCS" || l.SupplierDate != "2027-01-15" || l.Status != purapi.CommitmentConfirmed {
		t.Fatalf("inspection subjects %+v, %v", lines, err)
	}
	for _, c := range []struct{ sql, want string }{
		{"UPDATE brant_pur.purchase_commitment SET po_number = 'PO-X', row_version = row_version + 1", "23000"},
		{"UPDATE brant_pur.purchase_commitment SET status = 'approved', row_version = row_version + 1", "23000"},
		{"DELETE FROM brant_pur.purchase_commitment", "42501"},
		{"UPDATE brant_pur.supplier_confirmation SET result = 'as_issued'", "42501"},
		{"DELETE FROM brant_pur.supplier_confirmation", "42501"},
	} {
		if got := sqlCode(w.exec("company.A", c.sql)); got != c.want {
			t.Errorf("%s: %s, want %s", c.sql, got, c.want)
		}
	}
}

// A47 #2 and A47-01 (编制人或申请人自审被拒，AI 身份和缺少权限被拒): the draft's preparer and the requester
// do not decide (INDEPENDENT_REVIEW_REQUIRED); an AI agent with every permission neither decides,
// revokes nor issues, and a human without the permission does not decide (PERMISSION_DENIED); a
// rejection needs a reason, is final and stops the issue; the draft then takes a new request.
func TestCommitmentDecisionIsIndependentAndHuman(t *testing.T) {
	w := newPurWorld(t)
	p := newPurCommit(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	buyer := w.other("a47.buyer", kernel.ActorHuman, allPermissions...)
	_, err = p.request(buyer, "commitment.1", d.ObjectID, d.DraftHash)
	w.must(err)
	for _, ctx := range []context.Context{w.author, buyer} {
		_, err = p.approve(ctx, "commitment.1", 1, d.DraftHash)
		if code(err) != purapi.CodeIndependentReviewRequired || path(err) != "/args/commitment_id" {
			t.Fatalf("self-decision: %s at %s", code(err), path(err))
		}
	}
	ai := w.other("a47.ai", kernel.ActorAI, allPermissions...)
	_, err = p.approve(ai, "commitment.1", 1, d.DraftHash)
	w.want(err, kernel.CodePermissionDenied)
	_, err = p.revoke(ai, "commitment.1", 1)
	w.want(err, kernel.CodePermissionDenied)
	_, err = p.issue(ai, "commitment.1", 1)
	w.want(err, kernel.CodePermissionDenied)
	limited := w.provision(bootstrapGrant(w.supWorld, "a47.limited", "company",
		slices.DeleteFunc(slices.Clone(allPermissions), func(s string) bool { return s == purapi.CommandDecideCommitment })...))
	_, err = p.approve(limited, "commitment.1", 1, d.DraftHash)
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.pur.DecideCommitment(w.reviewer, purapi.DecideCommitment{IdempotencyKey: w.key(), CommitmentID: "commitment.1", ExpectedVersion: 1,
		DraftHash: d.DraftHash, Decision: purapi.DecisionReject})
	w.want(err, kernel.CodeSchemaInvalid)
	_, err = p.approve(w.reviewer, "commitment.1", 1, strings.Repeat("0", 64))
	w.want(err, purapi.CodeReferenceStale)
	r, err := p.reject(w.reviewer, "commitment.1", 1, d.DraftHash)
	w.must(err)
	if r.Status != purapi.CommitmentRejected || r.RowVersion != 2 {
		t.Fatalf("reject %+v", r)
	}
	_, err = p.approve(w.reviewer, "commitment.1", 1, d.DraftHash)
	w.want(err, kernel.CodeVersionConflict)
	_, err = p.approve(w.reviewer, "commitment.1", 2, d.DraftHash)
	w.want(err, kernel.CodeInvalidState)
	_, err = p.issue(w.author, "commitment.1", 2)
	w.want(err, purapi.CodePurchaseCommitmentNotApproved)
	if v := p.view(w.author, "commitment.1"); v.Decision != purapi.DecisionReject || v.DecisionReason == "" || v.State != purapi.CommitmentRejected {
		t.Fatalf("rejected %+v", v)
	}
	_, err = p.request(w.author, "commitment.2", d.ObjectID, d.DraftHash)
	w.must(err)
	_, err = p.request(w.author, "commitment.3", d.ObjectID, d.DraftHash)
	w.want(err, kernel.CodeInvalidState)
	audit, err := w.pur.Audit(w.reviewer)
	w.must(err)
	refused := 0
	for _, e := range audit {
		if e.Command == purapi.CommandDecideCommitment && e.Outcome == "rejected" {
			refused++
		}
	}
	if refused != 8 {
		t.Fatalf("every refused decision is audited: %d", refused)
	}
}

// A47 #3 (A47-01: 供应条件被替代、过期、撤回或订单审批撤销后不能批准；重算结果不一致时失败关闭): the decision
// prepares the draft's request again on the facts as they are now and approves only an equal draft.
// Refused, with the commitment left requested: an order whose activating approval was revoked
// (ORDER_APPROVAL_REVOKED, from TRD's lookup and, committed between the lookup and the transaction,
// from OrderGuard v1), an order at another revision (PURCHASE_DRAFT_STALE), the same order binding
// with another delivery requirement (PURCHASE_DRAFT_STALE naming the field), an expired supply
// (SUPPLY_CONDITION_EXPIRED), a withdrawn offer (from SUP's lookup and, withdrawn after it, from
// SupplyConditionGuard) and a superseded one (SUPPLY_CONDITION_NOT_CURRENT). Refusals point into the
// draft.
func TestCommitmentDecisionRecomputesTheDraft(t *testing.T) {
	setup := func(t *testing.T) (*purWorld, *purCommit, purapi.DraftReceipt) {
		w := newPurWorld(t)
		p := newPurCommit(w.scenWorld, func() *pur.Module { return w.pur })
		d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
		w.must(err)
		_, err = p.request(w.author, "commitment.1", d.ObjectID, d.DraftHash)
		w.must(err)
		return w, p, d
	}
	refused := func(t *testing.T, w *purWorld, p *purCommit, d purapi.DraftReceipt, want, at string) {
		t.Helper()
		_, err := p.approve(w.reviewer, "commitment.1", 1, d.DraftHash)
		if code(err) != want || path(err) != at {
			t.Fatalf("%s at %s, want %s at %s (%v)", code(err), path(err), want, at, err)
		}
		if s, v := p.state("commitment.1"); s != purapi.CommitmentRequested || v != 1 {
			t.Fatalf("the commitment stays requested: %s %d", s, v)
		}
	}
	t.Run("order and clock", func(t *testing.T) {
		w, p, d := setup(t)
		w.orders.set("order.1", func(o *pur.Order) { o.Code = pur.OrderApprovalRevoked })
		refused(t, w, p, d, purapi.CodeOrderApprovalRevoked, "/draft/order_id")
		w.orders.set("order.1", func(o *pur.Order) { o.Code, o.Binding.OrderRevision = "", 2 })
		refused(t, w, p, d, purapi.CodePurchaseDraftStale, "/draft/order_id")
		w.orders.set("order.1", func(o *pur.Order) { o.Binding.OrderRevision, o.Delivery.Date = 1, "2027-01-30" })
		refused(t, w, p, d, purapi.CodePurchaseDraftStale, "/args/commitment_id")
		_, err := p.approve(w.reviewer, "commitment.1", 1, d.DraftHash)
		if r, _ := kernel.AsRejection(err); r == nil || !strings.Contains(r.Fields[0].Reason, "/lines/0/delivery/required_date") {
			t.Fatalf("the field that differs is named: %v", err)
		}
		w.orders.set("order.1", func(o *pur.Order) { o.Delivery.Date = "2027-01-31" })
		w.pur.Clock = purClock{shift: 25 * time.Hour}
		refused(t, w, p, d, supapi.CodeSupplyConditionExpired, "/draft/lines/0/source")
		w.pur.Clock = purClock{}
		w.hook.then(func() {
			w.orders.verdict("order.1", pur.OrderVerdict{Known: true, Status: "active", Code: pur.OrderApprovalRevoked})
		})
		refused(t, w, p, d, purapi.CodeOrderApprovalRevoked, "/draft/order_id")
		w.orders.mu.Lock()
		w.orders.verdicts = map[string]pur.OrderVerdict{}
		w.orders.mu.Unlock()
		a, err := p.approve(w.reviewer, "commitment.1", 1, d.DraftHash)
		if err != nil || a.Status != purapi.CommitmentApproved {
			t.Fatalf("equal again: %+v, %v", a, err)
		}
	})
	t.Run("offer withdrawn", func(t *testing.T) {
		w, p, d := setup(t)
		w.hook.then(func() {
			_, err := w.end(w.reviewer, supapi.KindSupplierOffer, "offer.1", 2, 1)
			w.must(err)
		})
		refused(t, w, p, d, supapi.CodeSupplyConditionNotCurrent, "/draft/lines/0/source")
		refused(t, w, p, d, supapi.CodeSupplyConditionNotCurrent, "/draft/lines/0/source")
	})
	t.Run("offer superseded", func(t *testing.T) {
		w, p, d := setup(t)
		_, err := w.revise(w.author, supapi.KindSupplierOffer, "offer.1", 2, 1)
		w.must(err)
		_, err = w.seal(w.reviewer, supapi.KindSupplierOffer, "offer.1", 3, 2, 1)
		w.must(err)
		refused(t, w, p, d, supapi.CodeSupplyConditionNotCurrent, "/draft/lines/0/source")
	})
}

// A47 #5 and S5 decision D-14 (closing S3-D111): a draft from another supplier than the costing's, at
// a cost above it, needs both deviations acknowledged with reasons (PURCHASE_DEVIATION_UNACKNOWLEDGED;
// an acknowledgement of a deviation the draft does not have is REFERENCE_INVALID); a draft that buys
// beyond its allocation for the MOQ is approved by the decision acknowledging the overbuy, and then
// issued (OVERBUY_NOT_APPROVED is no longer the preparer's).
func TestCommitmentDeviationsAndOverbuyAreAcknowledged(t *testing.T) {
	w := newPurWorld(t)
	p := newPurCommit(w.scenWorld, func() *pur.Module { return w.pur })
	b := w.secondSupplier()
	db, err := w.prepare(w.author, "draft.B", "40", b)
	w.must(err)
	_, err = p.request(w.author, "commitment.B", db.ObjectID, db.DraftHash)
	w.must(err)
	_, err = p.approve(w.reviewer, "commitment.B", 1, db.DraftHash)
	w.want(err, purapi.CodePurchaseDeviationUnacknowledged)
	source := purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationSourceDiffers, Reason: "合成：第二供应商分担交期风险"}
	cost := purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationCostAbove, Reason: "合成：高 12.00 美元，经采购经理同意"}
	_, err = p.approve(w.reviewer, "commitment.B", 1, db.DraftHash, source)
	w.want(err, purapi.CodePurchaseDeviationUnacknowledged)
	_, err = p.approve(w.reviewer, "commitment.B", 1, db.DraftHash, source, cost,
		purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationOverbuy, Reason: "none"})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/acknowledgements/2" {
		t.Fatalf("no such deviation: %s at %s", code(err), path(err))
	}
	_, err = p.approve(w.reviewer, "commitment.B", 1, db.DraftHash, cost, source)
	w.must(err)
	if v := p.view(w.author, "commitment.B"); len(v.Acknowledgements) != 2 || v.Acknowledgements[0].Kind != purapi.DeviationCostAbove {
		t.Fatalf("acknowledgements %+v", v.Acknowledgements)
	}
	_, err = p.issue(w.author, "commitment.B", 2)
	w.must(err)

	moq := w.variant(func(l *supapi.ConditionLine) { l.MOQ.Quantity = "150" })
	line := purLine("80", purSource(moq))
	line.Overbuy = &purapi.Overbuy{Quantity: "70", Reason: "MOQ 150; the remainder goes to stock"}
	dm, err := w.pur.PrepareDraft(w.author, w.cmd("draft.moq", line))
	w.must(err)
	_, err = p.request(w.author, "commitment.moq", dm.ObjectID, dm.DraftHash)
	w.must(err)
	moqSource := purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationSourceDiffers, Reason: "合成：MOQ 150 的报价"}
	_, err = p.approve(w.reviewer, "commitment.moq", 1, dm.DraftHash, moqSource)
	if r, _ := kernel.AsRejection(err); r == nil || r.Code != purapi.CodePurchaseDeviationUnacknowledged || !strings.Contains(r.Fields[0].Reason, "overbuy") {
		t.Fatalf("the overbuy is approved at the decision: %v", err)
	}
	_, err = p.approve(w.reviewer, "commitment.moq", 1, dm.DraftHash, moqSource,
		purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationOverbuy, Reason: "合成：余量入库，经采购经理批准"})
	w.must(err)
	i, err := p.issue(w.author, "commitment.moq", 2)
	if err != nil || i.Status != purapi.CommitmentIssued {
		t.Fatalf("issue %+v, %v", i, err)
	}
	if al := w.allocation(); al.Allocated != "120" || al.Remaining != "0" {
		t.Fatalf("the commitments hold their drafts' allocations: %+v", al)
	}
}

// A47 #4, #6 and PUR-INV-010: an approval is valid after its decision and not after the draft's
// supply (INVALID_KNOWN_VALUE); a revoked one stops the issue (PURCHASE_COMMITMENT_NOT_APPROVED) and is
// not revoked again; an expired one is APPROVAL_EXPIRED (state expired); one whose decider lost the
// permission is no approval any more; the sequence must be a purchase_order one, the evidence read
// and the sending not in the future; the issue is once and its key replays after a restart with the
// same number; an issued commitment is not revoked.
func TestCommitmentValidityRevocationAndIssue(t *testing.T) {
	w := newPurWorld(t)
	p := newPurCommit(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	_, err = p.request(w.author, "commitment.1", d.ObjectID, d.DraftHash)
	w.must(err)
	for _, until := range []time.Time{time.Now().Add(-time.Minute), w.win.until.Add(time.Second)} {
		cmd := p.approveCmd("commitment.1", 1, d.DraftHash)
		cmd.ValidUntil = kernel.FormatTimestamp(until)
		_, err = w.pur.DecideCommitment(w.reviewer, cmd)
		if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/valid_until" {
			t.Fatalf("valid until %s: %s at %s", until, code(err), path(err))
		}
	}
	_, err = p.approve(w.reviewer, "commitment.1", 1, d.DraftHash)
	w.must(err)
	_, err = p.revoke(w.reviewer, "commitment.1", 2)
	w.must(err)
	_, err = p.issue(w.author, "commitment.1", 3)
	w.want(err, purapi.CodePurchaseCommitmentNotApproved)
	_, err = p.revoke(w.reviewer, "commitment.1", 3)
	w.want(err, kernel.CodeInvalidState)

	_, err = p.request(w.author, "commitment.2", d.ObjectID, d.DraftHash)
	w.must(err)
	_, err = p.approve(w.reviewer, "commitment.2", 1, d.DraftHash)
	w.must(err)
	w.pur.Clock = purClock{shift: 3 * time.Hour}
	_, err = p.issue(w.author, "commitment.2", 2)
	w.want(err, purapi.CodeApprovalExpired)
	if v := p.view(w.author, "commitment.2"); v.State != purapi.CommitmentStateExpired || v.Status != purapi.CommitmentApproved {
		t.Fatalf("expired %+v", v)
	}
	w.pur.Clock = purClock{}
	_, err = p.revoke(w.reviewer, "commitment.2", 2)
	w.must(err)

	_, err = p.request(w.author, "commitment.3", d.ObjectID, d.DraftHash)
	w.must(err)
	_, err = p.approve(w.reviewer, "commitment.3", 1, d.DraftHash)
	w.must(err)
	w.provision(bootstrapGrantTeam(w.supWorld, "reviewer", slices.DeleteFunc(slices.Clone(allPermissions),
		func(s string) bool { return s == purapi.CommandDecideCommitment })...))
	_, err = p.issue(w.author, "commitment.3", 2)
	w.want(err, purapi.CodePurchaseCommitmentNotApproved)
	w.provision(bootstrapGrantTeam(w.supWorld, "reviewer", allPermissions...))
	bad := p.issueCmd("commitment.3", 2)
	bad.SequenceRef = purapi.SequenceRef{ID: "missing.seq", Revision: 1}
	_, err = w.pur.IssueCommitment(w.author, bad)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/sequence_ref" {
		t.Fatalf("sequence: %s at %s", code(err), path(err))
	}
	quoteSeq := w.configure(w.author, w.reviewer, polapi.KindNumberSequence, "quote.seq", 0, w.win.from, time.Time{}, polapi.ConfigData{
		NumberSequence: &polapi.NumberSequence{DocumentType: polapi.DocumentTypeQuote, Prefix: "Q-", Width: 5, Reset: polapi.ResetAnnual,
			TimeZone: "Asia/Shanghai"}}, true)
	bad.IdempotencyKey, bad.SequenceRef = w.key(), purapi.SequenceRef{ID: quoteSeq.ConfigID, Revision: quoteSeq.Revision}
	_, err = w.pur.IssueCommitment(w.author, bad)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/sequence_ref" {
		t.Fatalf("a quote sequence: %s at %s", code(err), path(err))
	}
	bad = p.issueCmd("commitment.3", 2)
	bad.Evidence = []purapi.Evidence{{SourceID: "missing.source", Locator: "x"}}
	_, err = w.pur.IssueCommitment(w.author, bad)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence/0/source_id" {
		t.Fatalf("evidence: %s at %s", code(err), path(err))
	}
	bad = p.issueCmd("commitment.3", 2)
	bad.SentAt = kernel.FormatTimestamp(time.Now().Add(time.Hour))
	_, err = w.pur.IssueCommitment(w.author, bad)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/sent_at" {
		t.Fatalf("sent at: %s at %s", code(err), path(err))
	}
	cmd := p.issueCmd("commitment.3", 2)
	i, err := w.pur.IssueCommitment(w.author, cmd)
	w.must(err)
	again, err := w.module().IssueCommitment(w.author, cmd)
	if err != nil || !again.Replayed || again.ActionID != i.ActionID || again.PONumber != i.PONumber {
		t.Fatalf("replay after restart %+v, %v", again, err)
	}
	_, err = p.issue(w.author, "commitment.3", 3)
	w.want(err, kernel.CodeInvalidState)
	_, err = p.revoke(w.reviewer, "commitment.3", 3)
	w.want(err, kernel.CodeInvalidState)
}

func bootstrapGrantTeam(s *supWorld, id string, permissions ...string) bootstrap.Grant {
	g := bootstrapGrant(s, id, "company", permissions...)
	g.Team = "trade"
	return g
}

// A47-01 (审批过期、撤销或竞争时只有一个结果生效) on PostgreSQL 16: two requests for one draft commit once
// (the live-commitment index); two decisions of one commitment commit once (the commitment root); a
// decision and a void, and an issue and a void, serialize on the draft's allocation roots in either
// commit order: a void after an approval leaves it unissuable, a void after the issue is
// PURCHASE_DRAFT_COMMITTED (TD-AT06), and a decision or an issue after the void finds the draft voided.
func TestCommitmentRacesHaveOneOutcome(t *testing.T) {
	w := newPurWorld(t)
	p := newPurCommit(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.race", "120", "offer.1")
	w.must(err)
	race := func(a, b func() error) []string {
		out := make([]string, 2)
		var wg sync.WaitGroup
		for i, f := range []func() error{a, b} {
			wg.Add(1)
			go func() { defer wg.Done(); out[i] = code(f()) }()
		}
		wg.Wait()
		return out
	}
	got := race(func() error { _, err := p.request(w.author, "request.a", d.ObjectID, d.DraftHash); return err },
		func() error { _, err := p.request(w.author, "request.b", d.ObjectID, d.DraftHash); return err })
	if !slices.Contains(got, "OK") || !slices.Contains(got, kernel.CodeInvalidState) {
		t.Fatalf("two requests: %v", got)
	}
	live := "request.a"
	if got[0] != "OK" {
		live = "request.b"
	}
	_, err = p.reject(w.reviewer, live, 1, d.DraftHash)
	w.must(err)
	for round := range 3 {
		id := "decide." + string(rune('a'+round))
		_, err = p.request(w.author, id, d.ObjectID, d.DraftHash)
		w.must(err)
		got = race(func() error { _, err := p.approve(w.reviewer, id, 1, d.DraftHash); return err },
			func() error { _, err := p.reject(w.reviewer, id, 1, d.DraftHash); return err })
		if !slices.Contains(got, "OK") || !slices.Contains(got, kernel.CodeVersionConflict) {
			t.Fatalf("round %d, two decisions: %v", round, got)
		}
		if s, v := p.state(id); v != 2 || (s == purapi.CommitmentApproved) != (got[0] == "OK") {
			t.Fatalf("round %d: %s %d after %v", round, s, v, got)
		}
		if got[0] == "OK" {
			_, err = p.revoke(w.reviewer, id, 2)
			w.must(err)
		}
	}
	_, err = w.void(w.author, d.ObjectID, 1)
	w.must(err)

	next := func(id string) purapi.DraftReceipt {
		t.Helper()
		r, err := w.prepare(w.author, id, "120", "offer.1")
		w.must(err)
		_, err = p.request(w.author, "c."+id, r.ObjectID, r.DraftHash)
		w.must(err)
		return r
	}
	t.Run("void first, then the decision", func(t *testing.T) {
		r := next("void.decide")
		key := w.key()
		voided, decided := w.blockedBehind(t, purapi.CommandVoidDraft, key,
			func() error {
				_, err := w.pur.VoidDraft(w.author, purapi.VoidDraft{IdempotencyKey: key, DraftID: r.ObjectID, ExpectedVersion: 1, Reason: purVoidReason})
				return err
			},
			func() error { _, err := p.approve(w.reviewer, "c.void.decide", 1, r.DraftHash); return err })
		if voided != nil || code(decided) != kernel.CodeInvalidState {
			t.Fatalf("void %v, decision %s", voided, code(decided))
		}
	})
	t.Run("decision first, then the void", func(t *testing.T) {
		r := next("decide.void")
		decided, voided := w.heldUntil(t, func() error { _, err := p.approve(w.reviewer, "c.decide.void", 1, r.DraftHash); return err },
			func() error { _, err := w.void(w.author, r.ObjectID, 1); return err })
		if decided != nil || voided != nil {
			t.Fatalf("decision %v, void %v", decided, voided)
		}
		_, err := p.issue(w.author, "c.decide.void", 2)
		w.want(err, kernel.CodeInvalidState)
		if got := p.downstream("company.A", "order.1"); len(got) != 0 {
			t.Fatalf("nothing open: %q", got)
		}
	})
	t.Run("void first, then the issue", func(t *testing.T) {
		r := next("void.issue")
		_, err := p.approve(w.reviewer, "c.void.issue", 1, r.DraftHash)
		w.must(err)
		key := w.key()
		voided, issued := w.blockedBehind(t, purapi.CommandVoidDraft, key,
			func() error {
				_, err := w.pur.VoidDraft(w.author, purapi.VoidDraft{IdempotencyKey: key, DraftID: r.ObjectID, ExpectedVersion: 1, Reason: purVoidReason})
				return err
			},
			func() error { _, err := p.issue(w.author, "c.void.issue", 2); return err })
		if voided != nil || code(issued) != kernel.CodeInvalidState {
			t.Fatalf("void %v, issue %s", voided, code(issued))
		}
	})
	t.Run("issue first, then the void", func(t *testing.T) {
		r := next("issue.void")
		_, err := p.approve(w.reviewer, "c.issue.void", 1, r.DraftHash)
		w.must(err)
		issued, voided := w.heldUntil(t, func() error { _, err := p.issue(w.author, "c.issue.void", 2); return err },
			func() error { _, err := w.void(w.author, r.ObjectID, 1); return err })
		if issued != nil || code(voided) != purapi.CodePurchaseDraftCommitted {
			t.Fatalf("issue %v, void %s", issued, code(voided))
		}
	})
}

// A47 #6 (撤销与登记竞争时，以先提交者为准) and PUR-INV-010 (撤销与发出并发只有一个成功): a revocation and an
// issue of one approved commitment race; exactly one commits and the other sees it.
func TestCommitmentRevokeAndIssueRaceOnce(t *testing.T) {
	w := newPurWorld(t)
	p := newPurCommit(w.scenWorld, func() *pur.Module { return w.pur })
	src := w.moq40()
	ack := purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationSourceDiffers, Reason: "合成：MOQ 40 的报价"}
	for round := range 3 {
		id := "race." + string(rune('a'+round))
		d, err := w.prepare(w.author, "draft."+id, "40", src)
		w.must(err)
		_, err = p.request(w.author, id, d.ObjectID, d.DraftHash)
		w.must(err)
		_, err = p.approve(w.reviewer, id, 1, d.DraftHash, ack)
		w.must(err)
		out := make([]string, 2)
		var wg sync.WaitGroup
		for i, f := range []func() error{
			func() error { _, err := p.revoke(w.reviewer, id, 2); return err },
			func() error { _, err := p.issue(w.author, id, 2); return err },
		} {
			wg.Add(1)
			go func() { defer wg.Done(); out[i] = code(f()) }()
		}
		wg.Wait()
		s, v := p.state(id)
		switch {
		case out[0] == "OK" && out[1] == kernel.CodeVersionConflict && s == purapi.CommitmentRevoked && v == 3:
		case out[1] == "OK" && out[0] == kernel.CodeVersionConflict && s == purapi.CommitmentIssued && v == 3:
		default:
			t.Fatalf("round %d: %v, %s %d", round, out, s, v)
		}
	}
}

// ADR-BR-004 option B and ADR-BR-011 on R19 for the decision, on TRD's real orders: while the
// decision's transaction holds the order row FOR SHARE (OrderGuard v1, after the commitment and the
// allocation roots), order.approval.revoke waits; the approval commits on the order as it was, and
// the issue afterwards is ORDER_APPROVAL_REVOKED. The other commit order: a decision whose OrderGuard
// waits behind a revocation in flight reads the order as the revocation left it and is refused,
// leaving the commitment requested. BRC-001's 600.00 draft of the active 960.00 order.
func TestCommitmentGuardsOnTRDOrders(t *testing.T) {
	setup := func(t *testing.T) (*purOrderWorld, *purCommit, purapi.DraftReceipt) {
		w := newPurOrderWorld(t)
		p := newPurCommit(w.scenWorld, func() *pur.Module { return w.pur })
		d, err := w.prepare(w.author, "purchase.brc001", "120", "offer.1")
		w.must(err)
		if d.NetMinor != "60000" {
			t.Fatalf("draft %+v", d)
		}
		_, err = p.request(w.author, "commitment.brc001", d.ObjectID, d.DraftHash)
		w.must(err)
		return w, p, d
	}
	t.Run("PUR first", func(t *testing.T) {
		w, p, d := setup(t)
		w.port.mu.Lock()
		w.port.hold, w.port.held = make(chan struct{}), make(chan struct{}, 1)
		hold := w.port.hold
		w.port.mu.Unlock()
		done := make(chan error, 1)
		go func() { _, err := p.approve(w.reviewer, "commitment.brc001", 1, d.DraftHash); done <- err }()
		select {
		case <-w.port.held:
		case err := <-done:
			t.Fatalf("the decision finished before holding the order: %v", err)
		case <-time.After(30 * time.Second):
			t.Fatal("the decision never reached OrderGuard")
		}
		w.port.mu.Lock()
		w.port.hold = nil
		w.port.mu.Unlock()
		revoked := make(chan error, 1)
		go func() { revoked <- w.revoke() }()
		select {
		case err := <-revoked:
			close(hold)
			t.Fatalf("the revocation did not wait: %v (decision: %v)", err, <-done)
		case <-time.After(500 * time.Millisecond):
		}
		close(hold)
		if err := <-done; err != nil {
			t.Fatalf("decision: %v", err)
		}
		if err := <-revoked; err != nil {
			t.Fatalf("revocation: %v", err)
		}
		if got := p.downstream("company.A", w.order.ObjectID); len(got) != 3 || !strings.HasPrefix(got[1], "purchase_commitment commitment.brc001 ") ||
			!strings.Contains(got[1], " approved ") {
			t.Fatalf("downstream %q", got)
		}
		_, err := p.issue(w.author, "commitment.brc001", 2)
		if code(err) != purapi.CodeOrderApprovalRevoked || path(err) != "/draft/order_id" {
			t.Fatalf("issue: %s at %s", code(err), path(err))
		}
	})
	t.Run("TRD first", func(t *testing.T) {
		w, p, d := setup(t)
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
		go func() { _, err := p.approve(w.reviewer, "commitment.brc001", 1, d.DraftHash); done <- err }()
		if !awaitInHold(t, "the decision waits on the order", func() bool { return w.blocked(pid, 2) > 0 }, done) {
			return
		}
		release()
		if err := <-revoked; err != nil {
			t.Fatalf("revocation: %v", err)
		}
		err := <-done
		if code(err) != purapi.CodeOrderApprovalRevoked || path(err) != "/draft/order_id" {
			t.Fatalf("decision: %s at %s", code(err), path(err))
		}
		if s, v := p.state("commitment.brc001"); s != purapi.CommitmentRequested || v != 1 {
			t.Fatalf("the commitment stays requested: %s %d", s, v)
		}
	})
}

// A47-01 (失败回滚，重启后重放一致): a failing Outbox write rolls the approval back whole (the commitment
// stays requested, no action result); the same key approves once the store accepts the write; a
// restarted process reads the same commitment and replays the key.
func TestCommitmentFailureRollsBackAndReplaysAfterRestart(t *testing.T) {
	w := newPurWorld(t)
	p := newPurCommit(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	_, err = p.request(w.author, "commitment.1", d.ObjectID, d.DraftHash)
	w.must(err)
	restore := w.failOutbox(purapi.EventPurchaseCommitmentApproved)
	cmd := p.approveCmd("commitment.1", 1, d.DraftHash)
	_, err = w.pur.DecideCommitment(w.reviewer, cmd)
	if _, ok := kernel.AsRejection(err); err == nil || ok {
		t.Fatalf("an infrastructure failure, no rejection: %v", err)
	}
	if s, v := p.state("commitment.1"); s != purapi.CommitmentRequested || v != 1 {
		t.Fatalf("rolled back: %s %d", s, v)
	}
	restore()
	r, err := w.pur.DecideCommitment(w.reviewer, cmd)
	if err != nil || r.Replayed || r.Status != purapi.CommitmentApproved {
		t.Fatalf("retry %+v, %v", r, err)
	}
	m := w.module()
	v, err := m.GetCommitment(w.author, "commitment.1")
	if err != nil || v.RowVersion != 2 || !v.ValidUntil.Equal(r.ValidUntil) {
		t.Fatalf("restart %+v, %v", v, err)
	}
	again, err := m.DecideCommitment(w.reviewer, cmd)
	if err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("replay after restart %+v, %v", again, err)
	}
}

// DownstreamCommitments v1 (R71) and InspectionSubject v1 (R24): the function is SECURITY DEFINER with
// a fixed search_path, executable by brant_app and not by PUBLIC, refuses a NULL order, answers in
// the caller's company only, follows the draft's and the commitment's states, and takes no lock: it
// answers while a decision holds the commitment and allocation roots FOR UPDATE. A commitment line is
// an inspection subject once issued; a reader outside the commitment's data scope does not see it.
func TestDownstreamCommitmentsAndInspectionSubjects(t *testing.T) {
	w := newPurWorld(t)
	p := newPurCommit(w.scenWorld, func() *pur.Module { return w.pur })
	var definer, publicExec, appExec bool
	var config []string
	w.must(w.execScan("company.A", `SELECT p.prosecdef, p.proconfig, has_function_privilege('public', p.oid, 'EXECUTE'),
		has_function_privilege('brant_app', p.oid, 'EXECUTE') FROM pg_proc p WHERE p.oid = 'brant_pur.order_commitments_v1(text)'::regprocedure`,
		&definer, &config, &publicExec, &appExec))
	if !definer || publicExec || !appExec || !slices.Contains(config, "search_path=pg_catalog, pg_temp") {
		t.Fatalf("definer %v config %v public %v app %v", definer, config, publicExec, appExec)
	}
	if err := w.exec("company.A", "SELECT * FROM brant_pur.order_commitments_v1(NULL)"); sqlCode(err) != "22004" {
		t.Fatalf("NULL: %s", sqlCode(err))
	}
	if got := p.downstream("company.A", "order.1"); len(got) != 0 {
		t.Fatalf("nothing yet %q", got)
	}
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	_, err = p.request(w.author, "commitment.1", d.ObjectID, d.DraftHash)
	w.must(err)
	lines, err := w.pur.ResolveCommitmentLines(w.reviewer, []purapi.CommitmentLineRef{{CommitmentID: "commitment.1", OrderLineID: "L1"}})
	l := lines[purapi.CommitmentLineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}]
	if err != nil || code(l.Check("/x")) != kernel.CodeInvalidState || l.Status != purapi.CommitmentRequested {
		t.Fatalf("a requested commitment is no subject: %+v, %v", l, err)
	}
	own := w.provision(bootstrapGrant(w.supWorld, "a47.own", "own", allPermissions...))
	if lines, err = w.pur.ResolveCommitmentLines(own, []purapi.CommitmentLineRef{{CommitmentID: "commitment.1", OrderLineID: "L1"}}); err != nil ||
		len(lines) != 0 {
		t.Fatalf("outside the data scope %+v, %v", lines, err)
	}
	w.orders.mu.Lock()
	w.orders.hold, w.orders.held = make(chan struct{}), make(chan struct{}, 1)
	hold := w.orders.hold
	w.orders.mu.Unlock()
	decided := make(chan error, 1)
	go func() { _, err := p.approve(w.reviewer, "commitment.1", 1, d.DraftHash); decided <- err }()
	select {
	case <-w.orders.held:
	case err := <-decided:
		t.Fatalf("the decision finished before holding its locks: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the decision never reached OrderGuard")
	}
	w.orders.mu.Lock()
	w.orders.hold = nil
	w.orders.mu.Unlock()
	type answer struct {
		rows []string
		err  error
	}
	read := make(chan answer, 1)
	go func() { rows, err := p.downstreamRows("company.A", "order.1"); read <- answer{rows, err} }()
	var during answer
	select {
	case during = <-read:
	case <-time.After(5 * time.Second):
		close(hold)
		t.Fatalf("DownstreamCommitments v1 waited on the decision's locks (decision: %v)", <-decided)
	}
	close(hold)
	if err := <-decided; err != nil || during.err != nil || len(during.rows) != 3 || !strings.Contains(during.rows[1], " requested ") {
		t.Fatalf("lock-free read during the decision %q, %v (decision %v)", during.rows, during.err, err)
	}
	if got := p.downstream("company.B", "order.1"); len(got) != 0 {
		t.Fatalf("another company %q", got)
	}
	_, err = p.issue(w.author, "commitment.1", 2)
	w.must(err)
	if lines, err = w.pur.ResolveCommitmentLines(w.reviewer, []purapi.CommitmentLineRef{{CommitmentID: "commitment.1", OrderLineID: "L1"}}); err != nil ||
		lines[purapi.CommitmentLineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}].Check("/x") != nil {
		t.Fatalf("an issued commitment's line is a subject %+v, %v", lines, err)
	}
	if _, err = w.pur.ResolveCommitmentLines(w.reviewer, []purapi.CommitmentLineRef{{CommitmentID: "c 1", OrderLineID: "L1"}}); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("refs: %v", err)
	}
}

// S5-01 over HTTP with OIDC access tokens, on TRD's real order: the editor prepares and requests, the
// reviewer approves, the editor issues and records the supplier's confirmation, everything in its
// wire form; the commitment and the order's commitments read back; the decision reconciles; the
// committed draft's void is 409 PURCHASE_DRAFT_COMMITTED.
func TestPurchaseCommitmentOverHTTP(t *testing.T) {
	q := newOrderWorld(t)
	order := q.activeOrder("quote.1")
	p := newPurCommit(q.scenWorld, func() *pur.Module { return q.c.PUR })
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ed := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "editor", "human")}
	rv := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "reviewer", "human"), n: 300}

	ed.command("pur", purapi.Contract, purapi.CommandPrepareDraft, map[string]any{"draft_id": "purchase.http", "order_id": order.ObjectID,
		"selection_reason": purReason, "lines": []any{map[string]any{"order_line_id": "L1", "quantity": "120",
			"source": map[string]any{"kind": purapi.KindSupplierOffer, "id": "offer.1", "revision": 1, "condition_line_id": "OL1"}}}})
	_, draft := ed.do("GET", "/api/brant/v1/pur/purchase_drafts/purchase.http", nil)
	hash, _ := draft["draft_hash"].(string)
	r := ed.command("pur", purapi.Contract, purapi.CommandRequestCommitment, map[string]any{"commitment_id": "commitment.http",
		"draft_id": "purchase.http", "draft_hash": hash})
	if r["status"] != "requested" || r["row_version"] != float64(1) {
		t.Fatalf("request %v", r)
	}
	decide := map[string]any{"contract": purapi.Contract, "command": purapi.CommandDecideCommitment, "idempotency_key": "pur.http.decide",
		"args": map[string]any{"commitment_id": "commitment.http", "expected_version": 1, "draft_hash": hash, "decision": "approve",
			"valid_until": kernel.FormatTimestamp(time.Now().Add(2 * time.Hour))}}
	status, a := rv.do("POST", "/api/brant/v1/pur/commands/"+purapi.CommandDecideCommitment, decide)
	if status != http.StatusOK || a["status"] != "approved" || a["row_version"] != float64(2) {
		t.Fatalf("decide %d %v", status, a)
	}
	status, rec := rv.do("POST", "/api/brant/v1/pur/commands/"+purapi.CommandDecideCommitment+"/reconcile", decide)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != a["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	ed.command("pur", purapi.Contract, purapi.CommandIssueCommitment, map[string]any{"commitment_id": "commitment.http", "expected_version": 2,
		"sequence_ref": map[string]any{"id": p.seq.ID, "revision": p.seq.Revision}, "sent_at": kernel.FormatTimestamp(time.Now()),
		"evidence": []any{map[string]any{"source_id": "evidence", "locator": "合成采购单发出邮件"}}})
	c := ed.command("pur", purapi.Contract, purapi.CommandRecordSupplierConfirmation, map[string]any{"commitment_id": "commitment.http",
		"expected_version": 3, "result": "as_issued", "received_at": kernel.FormatTimestamp(time.Now()),
		"evidence": []any{map[string]any{"source_id": "evidence", "locator": "合成供应商回签"}}})
	if c["status"] != "confirmed" || c["row_version"] != float64(4) {
		t.Fatalf("confirm %v", c)
	}
	status, v := ed.do("GET", "/api/brant/v1/pur/purchase_commitments/commitment.http", nil)
	if status != http.StatusOK || v["state"] != "confirmed" || !strings.HasPrefix(v["po_number"].(string), "PO-") || v["decided_by"].(map[string]any)["id"] != "reviewer" ||
		len(v["confirmations"].([]any)) != 1 {
		t.Fatalf("commitment %d %v", status, v)
	}
	status, list := ed.do("GET", "/api/brant/v1/pur/orders/"+order.ObjectID+"/commitments", nil)
	if status != http.StatusOK || len(list["commitments"].([]any)) != 1 {
		t.Fatalf("order commitments %d %v", status, list)
	}
	status, out := ed.try("pur", purapi.Contract, purapi.CommandVoidDraft, map[string]any{"draft_id": "purchase.http", "expected_version": 1,
		"reason": purVoidReason})
	if status != http.StatusConflict || out["code"] != purapi.CodePurchaseDraftCommitted {
		t.Fatalf("void %d %v", status, out)
	}
}
