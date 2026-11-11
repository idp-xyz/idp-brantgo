//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Python test_t04_a38_order_review.py test_denied_without_approval_and_independent_reviewer
// (PYT-0480), test_expiry_and_reviewer_current_authority (PYT-0481, the decision half: the
// order approval's validity is no longer bounded by the quote's, ADR-BR-013) and
// test_access_task_and_human_controls (PYT-0482), test_t04_a41_public_review.py
// test_request_and_decision_are_closed_and_payload_conflicts_are_rejected (PYT-0515),
// test_request_requires_draft_and_decision_requires_current_independent_human (PYT-0516),
// test_rejected_decision_blocks_activation_and_atomic_audit_failure_rolls_back (PYT-0517, the
// decision half) and test_public_decision_expiry_and_quote_bound_are_checked (PYT-0518, as a
// deliberate divergence), test_t04_a39_order_gate.py test_revocation_is_idempotent_and_immutable
// (PYT-0496) and test_revocation_needs_approved_decision (PYT-0498), and test_t04_a40_public_revoke.py
// test_public_revoke_commits_audited_idempotent_receipt (PYT-0508: the revocation commits, the same
// key replays its action ID, the review reads revoked and the activation is ORDER_APPROVAL_REVOKED;
// its audit entry is in TestOrderReadsAndReconciliation); TRD-INV-013,
// SM-TRD-ORDER-REVIEW: one open review per order; the reviewer is a human independent of the
// preparer and the requester who reads TRD now; the decision restates the order hash; an approval
// is valid after its decision and may outlive the quote; a rejection blocks activation and a new
// review may follow it; a decided review never changes; only an approval is revoked, once.
func TestOrderReviewRulesAndIndependence(t *testing.T) {
	q := newOrderWorld(t)
	agent := q.actorOf(kernel.ActorAI, "agent.1")
	_, o := q.prepared("quote.1")
	id, hash := o.ObjectID, o.OrderHash
	r, err := q.c.TRD.RequestOrderReview(agent, q.requestCmd(id, 1, "review.1"))
	q.must(err)
	_, err = q.c.TRD.RequestOrderReview(q.author, q.requestCmd(id, r.RowVersion, "review.2"))
	q.want(err, kernel.CodeInvalidState)
	_, err = q.c.TRD.RequestOrderReview(q.author, q.requestCmd(id, 1, "review.2"))
	q.want(err, kernel.CodeVersionConflict)

	v := r.RowVersion
	for name, c := range map[string]struct {
		ctx  context.Context
		cmd  trdapi.DecideOrderReview
		code string
	}{
		"the preparer and requester":   {q.author, q.decideCmd(id, v, "review.1", hash), trdapi.CodeReviewerNotIndependent},
		"an AI agent":                  {agent, q.decideCmd(id, v, "review.1", hash), kernel.CodePermissionDenied},
		"another order hash":           {q.reviewer, q.decideCmd(id, v, "review.1", sha256Hex("other")), trdapi.CodeOrderReviewMismatch},
		"an unknown review":            {q.reviewer, q.decideCmd(id, v, "review.none", hash), kernel.CodeNotFound},
		"an earlier order row version": {q.reviewer, q.decideCmd(id, 1, "review.1", hash), kernel.CodeVersionConflict},
	} {
		_, err := q.c.TRD.DecideOrderReview(c.ctx, c.cmd)
		if code(err) != c.code {
			t.Errorf("%s: %s, want %s", name, code(err), c.code)
		}
	}
	lapsed := q.decideCmd(id, v, "review.1", hash)
	lapsed.ExpiresAt = kernel.FormatTimestamp(time.Now().Add(-time.Second))
	_, err = q.c.TRD.DecideOrderReview(q.reviewer, lapsed)
	q.want(err, trdapi.CodeOrderReviewExpiryInvalid)
	// The decider's current qualification: without trd.read now, PERMISSION_DENIED (T04 A41).
	reader := q.regrant("order.reviewer", "company", without(allPermissions, "trd.read"))
	_, err = q.c.TRD.DecideOrderReview(reader, q.decideCmd(id, v, "review.1", hash))
	q.want(err, kernel.CodePermissionDenied)

	reject := trdapi.DecideOrderReview{IdempotencyKey: q.key(), OrderID: id, ExpectedVersion: v, ReviewID: "review.1", OrderHash: hash,
		Decision: trdapi.DecisionReject, Reason: "客户条款待确认"}
	d, err := q.c.TRD.DecideOrderReview(q.reviewer, reject)
	if err != nil || d.Status != trdapi.OrderReviewRejected || d.OrderStatus != trdapi.OrderDraft {
		t.Fatalf("rejected %+v %v", d, err)
	}
	_, err = q.c.TRD.DecideOrderReview(q.reviewer, q.decideCmd(id, d.RowVersion, "review.1", hash))
	q.want(err, trdapi.CodeOrderReviewImmutable)
	_, err = q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(id, d.RowVersion, "review.1"))
	q.want(err, trdapi.CodeOrderApprovalRequired)
	_, err = q.c.TRD.RevokeOrderApproval(q.reviewer, trdapi.RevokeOrderApproval{IdempotencyKey: q.key(), OrderID: id, ReviewID: "review.1",
		Reason: "无"})
	q.want(err, trdapi.CodeOrderApprovalRequired)

	// A new review after the rejection; its approval may outlive the quote (ADR-BR-013: the
	// approval's validity is the order approval's own).
	_, err = q.c.TRD.RequestOrderReview(q.author, q.requestCmd(id, d.RowVersion, "review.1"))
	q.want(err, trdapi.CodeOrderReviewImmutable)
	r2, err := q.c.TRD.RequestOrderReview(q.author, q.requestCmd(id, d.RowVersion, "review.2"))
	q.must(err)
	long := q.decideCmd(id, r2.RowVersion, "review.2", hash)
	long.ExpiresAt = kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))
	approved, err := q.c.TRD.DecideOrderReview(q.reviewer, long)
	if err != nil || approved.ReviewStatus != trdapi.OrderReviewApproved {
		t.Fatalf("an approval outliving the quote %+v %v", approved, err)
	}
	revoke := trdapi.RevokeOrderApproval{IdempotencyKey: q.key(), OrderID: id, ReviewID: "review.2", Reason: "合成测试：客户条件变化"}
	rv, err := q.c.TRD.RevokeOrderApproval(q.reviewer, revoke)
	if err != nil || rv.Status != trdapi.OrderReviewRevoked || rv.OrderStatus != trdapi.OrderDraft {
		t.Fatalf("revoked %+v %v", rv, err)
	}
	if again, err := q.c.TRD.RevokeOrderApproval(q.reviewer, revoke); err != nil || !again.Replayed || again.ActionID != rv.ActionID {
		t.Fatalf("the same key %+v %v", again, err)
	}
	revoke.IdempotencyKey = q.key()
	_, err = q.c.TRD.RevokeOrderApproval(q.reviewer, revoke)
	q.want(err, trdapi.CodeOrderReviewImmutable)
	_, err = q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(id, rv.RowVersion, "review.2"))
	q.want(err, trdapi.CodeOrderApprovalRevoked)
	views := q.getOrder(id).Reviews
	if len(views) != 2 || views[0].DecisionState != trdapi.OrderReviewRejected || views[1].DecisionState != trdapi.OrderReviewRevoked ||
		views[1].RevokeReason != revoke.Reason || views[1].RevokedBy.ID != "reviewer" {
		t.Fatalf("the reviews %+v", views)
	}
	decided := q.eventsOf(trdapi.EventOrderReviewDecided)
	if len(decided) != 2 || decided[0].Payload["review_status"] != trdapi.OrderReviewRejected || len(q.eventsOf(trdapi.EventOrderApprovalRevoked)) != 1 {
		t.Fatalf("events %v", decided)
	}
}

// Python test_t04_a39_order_gate.py test_new_review_can_replace_revoked_approval_for_draft_only
// (PYT-0504), test_new_activation_requires_explicit_order_review_in_both_paths (PYT-0487),
// test_unknown_and_cross_scope_review_cannot_authorize_order (PYT-0488),
// test_pending_and_rejected_decisions_block_both_paths (PYT-0489),
// test_expired_order_approval_blocks_both_paths_before_quote_expires (PYT-0491),
// test_current_order_reviewer_grant_and_human_identity_are_required (PYT-0492, ORDER_APPROVAL_REQUIRED
// where Python says PERMISSION_DENIED), test_current_order_reviewer_record_access_is_required
// (PYT-0493, likewise), test_revoking_active_order_preserves_original_fact_and_receipt (PYT-0503),
// test_t03_a26_order_qualification.py test_revoked_reviewer_grant_blocks_new_activation (PYT-0232),
// test_reviewer_changed_to_ai_blocks_new_activation (PYT-0233) and
// test_reviewer_losing_source_quote_scope_blocks_new_activation (PYT-0234), and test_t03_a24
// test_order_activation_does_not_bypass_source_quote_scope (PYT-0196) and
// test_order_activation_rechecks_binding_after_core_restart (PYT-0197), test_t03_a20
// test_wrong_version_and_unapproved_activation_are_blocked (PYT-0168, the activation half: an order
// whose review is not approved is ORDER_APPROVAL_REQUIRED where Python says APPROVAL_REQUIRED, the
// order step naming no quote approval) and test_t03_a26
// test_matching_trusted_task_keeps_quote_control_and_qualification_checks (PYT-0255: the task's
// claim passes the quote's control and the qualification checks still run; the commit evidence
// records the control epoch); TRD-INV-013, 014, 015: the order gate admits an approved, unrevoked, unexpired review by a reviewer still qualified, and the
// formalization approval's reviewer must still be qualified too; an outsider is NOT_FOUND, a bound
// root needs the task; a review after a revocation activates the draft, and nothing is requested of
// an active order; revoking the approval that activated an order keeps it active and its receipt.
func TestOrderActivationGate(t *testing.T) {
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	id := o.ObjectID
	r, err := q.c.TRD.RequestOrderReview(q.author, q.requestCmd(id, 1, "review.1"))
	q.must(err)
	_, err = q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(id, r.RowVersion, "review.1"))
	q.want(err, trdapi.CodeOrderApprovalRequired)
	_, err = q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(id, r.RowVersion, "review.none"))
	q.want(err, kernel.CodeNotFound)
	short := q.decideCmd(id, r.RowVersion, "review.1", o.OrderHash)
	expires := time.Now().Add(3 * time.Second)
	short.ExpiresAt = kernel.FormatTimestamp(expires)
	d, err := q.c.TRD.DecideOrderReview(q.reviewer, short)
	q.must(err)
	outsider := q.regrant("outsider", "own", allPermissions)
	_, err = q.c.TRD.ActivateOrder(outsider, q.activateOrderCmd(id, d.RowVersion, "review.1"))
	q.want(err, kernel.CodeNotFound)
	time.Sleep(time.Until(expires) + 100*time.Millisecond)
	_, err = q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(id, d.RowVersion, "review.1"))
	q.want(err, trdapi.CodeOrderApprovalExpired)
	if v := q.getOrder(id); v.Status != trdapi.OrderDraft || v.Reviews[0].DecisionState != "expired" {
		t.Fatalf("after the approval lapsed %+v", v.Reviews)
	}

	// The order reviewer's current standing.
	r2, err := q.c.TRD.RequestOrderReview(q.author, q.requestCmd(id, d.RowVersion, "review.2"))
	q.must(err)
	orderReviewer := q.regrant("order.reviewer", "company", allPermissions)
	d2, err := q.c.TRD.DecideOrderReview(orderReviewer, q.decideCmd(id, r2.RowVersion, "review.2", o.OrderHash))
	q.must(err)
	activate := q.activateOrderCmd(id, d2.RowVersion, "review.2")
	for name, grant := range map[string]func(){
		"without order.review.decide (PYT-0492)": func() { q.regrant("order.reviewer", "company", without(allPermissions, "order.review.decide")) },
		"an AI agent now (PYT-0492)":             func() { q.provision(grantOf(q, "order.reviewer", kernel.ActorAI, "company", allPermissions...)) },
		"own data scope (PYT-0493)":              func() { q.regrant("order.reviewer", "own", allPermissions) },
	} {
		grant()
		activate.IdempotencyKey = q.key()
		_, err = q.c.TRD.ActivateOrder(q.author, activate)
		if code(err) != trdapi.CodeOrderApprovalRequired {
			t.Errorf("an order reviewer %s: %s", name, code(err))
		}
	}
	q.regrant("order.reviewer", "company", allPermissions)
	// The formalization approval's reviewer (the quote's) must still be qualified (T03 A26).
	for name, grant := range map[string]func(){
		"without quote.review.decide (PYT-0232)": func() { q.regrant("reviewer", "company", without(allPermissions, "quote.review.decide")) },
		"an AI agent now (PYT-0233)":             func() { q.provision(grantOf(q, "reviewer", kernel.ActorAI, "company", allPermissions...)) },
		"own data scope (PYT-0234)":              func() { q.regrant("reviewer", "own", allPermissions) },
	} {
		grant()
		activate.IdempotencyKey = q.key()
		_, err = q.c.TRD.ActivateOrder(q.author, activate)
		if code(err) != trdapi.CodeApprovalStale {
			t.Errorf("a quote reviewer %s: %s", name, code(err))
		}
	}
	q.regrant("reviewer", "company", allPermissions)

	// A task bound to the quote root fences the activation (PYT-0197).
	tsk := q.actorOf(kernel.ActorSystem, "tsk.1")
	_, err = q.c.TRD.BindControl(tsk, trdapi.BindControl{IdempotencyKey: q.key(), QuoteID: "quote.1", TaskID: "task.1"})
	q.must(err)
	activate.IdempotencyKey = q.key()
	_, err = q.c.TRD.ActivateOrder(q.author, activate)
	q.want(err, trdapi.CodeObjectControlConflict)
	claimed, err := trdapi.WithControl(q.author, trdapi.ControlClaim{TaskID: "task.1", ControlEpoch: 1})
	q.must(err)
	act, err := q.c.TRD.ActivateOrder(claimed, activate)
	if err != nil || act.Status != trdapi.OrderActive || act.ReviewID != "review.2" {
		t.Fatalf("the task's activation %+v %v", act, err)
	}
	if e := q.getOrder(id).Commits[0].Entries; e["control_epoch"] != "1" || e["reviewer"] != "human:order.reviewer" {
		t.Fatalf("commit evidence %v", e)
	}
	_, err = q.c.TRD.RequestOrderReview(claimed, q.requestCmd(id, act.RowVersion, "review.3"))
	q.want(err, trdapi.CodeOrderNotDraft)

	// Revoking the approval that activated the order keeps the order active and its receipt (PYT-0503).
	rv, err := q.c.TRD.RevokeOrderApproval(trdapiClaim(t, q.reviewer), trdapi.RevokeOrderApproval{IdempotencyKey: q.key(), OrderID: id,
		ReviewID: "review.2", Reason: "合成测试：客户条件变化"})
	if err != nil || rv.OrderStatus != trdapi.OrderActive || rv.Status != trdapi.OrderReviewRevoked {
		t.Fatalf("revoked after activation %+v %v", rv, err)
	}
	if again, err := q.c.TRD.ActivateOrder(claimed, activate); err != nil || !again.Replayed || again.ActionID != act.ActionID {
		t.Fatalf("the activation replays %+v %v", again, err)
	}
	if ev := q.eventsOf(trdapi.EventOrderApprovalRevoked); len(ev) != 1 || ev[0].Payload["activated_order"] != true {
		t.Fatalf("OrderApprovalRevoked %v", ev)
	}
}

// trdapiClaim claims task.1 at control epoch 1 for ctx.
func trdapiClaim(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	c, err := trdapi.WithControl(ctx, trdapi.ControlClaim{TaskID: "task.1", ControlEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Python test_t04_a38 test_competing_approved_reviews_produce_one_activation (PYT-0486) and the A24
// note on concurrent activations of one order: two approved reviews of one order activated at once
// under different keys have one winner; the other is refused (the order is no longer a draft, or its
// row version moved); one commit evidence, one OrderActivated.
func TestConcurrentOrderActivationsHaveOneWinner(t *testing.T) {
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	d1 := q.reviewed(o, "review.1")
	d2 := q.reviewed(o, "review.2")
	got := make([]string, 2)
	var wg sync.WaitGroup
	for i, c := range []trdapi.ActivateOrder{q.activateOrderCmd(o.ObjectID, d2.RowVersion, "review.1"),
		q.activateOrderCmd(o.ObjectID, d2.RowVersion, "review.2")} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := q.c.TRD.ActivateOrder(q.author, c)
			got[i] = code(err)
		}()
	}
	wg.Wait()
	ok := slices.Index(got, "OK")
	if ok < 0 || (got[1-ok] != trdapi.CodeOrderNotDraft && got[1-ok] != kernel.CodeVersionConflict) {
		t.Fatalf("two activations: %v (first decision at %d)", got, d1.RowVersion)
	}
	if _, _, commits := q.orderRows(); commits != 1 || len(q.eventsOf(trdapi.EventOrderActivated)) != 1 {
		t.Fatalf("%d commit evidence", commits)
	}
}

// Draft cancellation and unfulfilled close (SM-TRD-ORDER draft → cancelled, draft →
// closed_unfulfilled: 授权负责人与理由；名额保留): a human with a reason and evidence they read ends a draft;
// the conversion slot stays taken (the quote converts no second order and is neither replaced nor
// cancelled); nothing activates, reviews or ends it again; an AI agent ends nothing.
func TestDraftOrderCancelAndCloseUnfulfilled(t *testing.T) {
	q := newOrderWorld(t)
	agent := q.actorOf(kernel.ActorAI, "agent.1")
	a, o := q.prepared("quote.1")
	d := q.reviewed(o, "review.1")
	cancel := trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: o.ObjectID, ExpectedVersion: d.RowVersion, Reason: "客户改用新报价",
		Evidence: q.proof}
	_, err := q.c.TRD.CancelOrder(agent, cancel)
	q.want(err, kernel.CodePermissionDenied)
	unread := cancel
	unread.Evidence = []trdapi.Evidence{{SourceID: "source.none", Locator: "x"}}
	_, err = q.c.TRD.CancelOrder(q.author, unread)
	q.want(err, kernel.CodeReferenceInvalid)
	c, err := q.c.TRD.CancelOrder(q.author, cancel)
	if err != nil || c.Status != trdapi.OrderCancelled || c.RowVersion != d.RowVersion+1 {
		t.Fatalf("cancelled %+v %v", c, err)
	}
	_, err = q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o.ObjectID, c.RowVersion, "review.1"))
	q.want(err, trdapi.CodeOrderNotDraft)
	_, err = q.c.TRD.CloseOrderUnfulfilled(q.author, trdapi.CloseOrderUnfulfilled{IdempotencyKey: q.key(), OrderID: o.ObjectID,
		ExpectedVersion: c.RowVersion, Reason: "x", Evidence: q.proof})
	q.want(err, trdapi.CodeOrderNotDraft)
	_, err = q.c.TRD.PrepareOrder(q.author, q.prepareCmd("order.again", a))
	q.want(err, trdapi.CodeConversionSlotConflict)
	v := q.getQuote("quote.1")
	_, err = q.c.TRD.Cancel(q.author, trdapi.Cancel{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: v.RootVersion, BusinessRevision: 1,
		Reason: "r", Evidence: q.proof})
	q.want(err, trdapi.CodeRootConversionLocked)
	if got := q.getOrder(o.ObjectID); got.Closure == nil || got.Closure.Status != trdapi.OrderCancelled || got.Closure.Reason != cancel.Reason ||
		got.Closure.ClosedBy.ID != "editor" {
		t.Fatalf("the closure %+v", got.Closure)
	}

	// The acceptance invalidated after the preparation: the draft is closed unfulfilled.
	b, o2 := q.prepared("quote.2")
	_, err = q.c.TRD.InvalidateAcceptance(q.reviewer, q.invalidation(b.ObjectID, 1))
	q.must(err)
	_, err = q.c.TRD.RequestOrderReview(q.author, q.requestCmd(o2.ObjectID, 1, "review.2"))
	q.must(err)
	_, err = q.c.TRD.DecideOrderReview(q.reviewer, q.decideCmd(o2.ObjectID, 2, "review.2", o2.OrderHash))
	q.want(err, trdapi.CodeAcceptanceNotValid)
	closed, err := q.c.TRD.CloseOrderUnfulfilled(q.author, trdapi.CloseOrderUnfulfilled{IdempotencyKey: q.key(), OrderID: o2.ObjectID,
		ExpectedVersion: 2, Reason: "接受被作废", Evidence: q.proof})
	if err != nil || closed.Status != trdapi.OrderClosedUnfulfilled {
		t.Fatalf("closed unfulfilled %+v %v", closed, err)
	}
	if len(q.eventsOf(trdapi.EventOrderCancelled)) != 1 || len(q.eventsOf(trdapi.EventOrderClosedUnfulfilled)) != 1 {
		t.Fatal("the closing events")
	}
}

// TRD-INV-013 (撤销与激活共享锁，先提交者胜) and Python test_t04_a39 test_committed_revocation_wins_against_waiting_activation
// (PYT-0505): a revocation holding the order makes the activation on that approval wait, which is
// then refused (ORDER_APPROVAL_REVOKED, or VERSION_CONFLICT for its stale row version) and writes
// nothing; an activation holding it makes the revocation wait, which then revokes the approval of an
// active order without rewriting it.
func TestOrderRevocationAndActivationDecideByCommitOrder(t *testing.T) {
	q := newOrderWorld(t)
	d := newDownstream(q.world)
	_, o1 := q.prepared("quote.1")
	_, o2 := q.prepared("quote.2")
	d1, d2 := q.reviewed(o1, "review.1"), q.reviewed(o2, "review.2")

	revoke := trdapi.RevokeOrderApproval{IdempotencyKey: q.key(), OrderID: o1.ObjectID, ReviewID: "review.1", Reason: "客户条件变化"}
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandRevokeOrderApproval, revoke.IdempotencyKey)
	done := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.RevokeOrderApproval(q.reviewer, revoke)
		done <- err
	}()
	if !awaitInHold(t, "the revocation holds the order", func() bool { return d.blocks(blocker) }, done) {
		release()
		return
	}
	activated := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o1.ObjectID, d1.RowVersion, "review.1"))
		activated <- err
	}()
	ok := awaitInHold(t, "the activation waits for the revoking root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, activated)
	release()
	if !ok {
		return
	}
	if err := <-done; err != nil {
		t.Fatalf("the revocation: %v", err)
	}
	if err := <-activated; code(err) != kernel.CodeVersionConflict && code(err) != trdapi.CodeOrderApprovalRevoked {
		t.Fatalf("an activation behind the revocation: %s", code(err))
	}
	if v := q.getOrder(o1.ObjectID); v.Status != trdapi.OrderDraft || len(v.Commits) != 0 {
		t.Fatalf("order.1 %+v", v)
	}
	_, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o1.ObjectID, q.orderVersion(o1.ObjectID), "review.1"))
	q.want(err, trdapi.CodeOrderApprovalRevoked)

	activate := q.activateOrderCmd(o2.ObjectID, d2.RowVersion, "review.2")
	blocker, release = d.keyBlocker(t, "brant_trd", trdapi.CommandActivateOrder, activate.IdempotencyKey)
	go func() {
		_, err := q.c.TRD.ActivateOrder(q.author, activate)
		done <- err
	}()
	if !awaitInHold(t, "the activation holds the order", func() bool { return d.blocks(blocker) }, done) {
		release()
		return
	}
	revoked := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.RevokeOrderApproval(q.reviewer, trdapi.RevokeOrderApproval{IdempotencyKey: q.key(), OrderID: o2.ObjectID,
			ReviewID: "review.2", Reason: "客户条件变化"})
		revoked <- err
	}()
	ok = awaitInHold(t, "the revocation waits for the activating root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, revoked)
	release()
	if !ok {
		return
	}
	if err := <-done; err != nil {
		t.Fatalf("the activation: %v", err)
	}
	if err := <-revoked; err != nil {
		t.Fatalf("the revocation after the activation: %v", err)
	}
	if v := q.getOrder(o2.ObjectID); v.Status != trdapi.OrderActive || v.Reviews[0].Status != trdapi.OrderReviewRevoked {
		t.Fatalf("order.2 %+v", v)
	}
}
