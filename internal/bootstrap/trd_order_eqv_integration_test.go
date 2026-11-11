//go:build integration

package bootstrap_test

import (
	"context"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The order cases of the S3-11 equivalence review (WBS S3-11) that the order waves left without a
// test of their own, on the order world of trd_order_integration_test.go. Each test cites the
// Python V0.50 test it ports.

// oeqvRevoke is order.approval.revoke of review of order under a fresh key.
func oeqvRevoke(q *trdWorld, order, review string) trdapi.RevokeOrderApproval {
	return trdapi.RevokeOrderApproval{IdempotencyKey: q.key(), OrderID: order, ReviewID: review, Reason: "合成测试：客户条件变化"}
}

// oeqvFact is what replays of an activation must keep: the order's state, row version, hash and
// commit evidence.
func oeqvFact(q *trdWorld, id string) string {
	q.t.Helper()
	v := q.getOrder(id)
	return jsonString([]any{v.Status, v.RowVersion, v.OrderHash, v.ActivatedReviewID, v.Commits})
}

// Python test_t04_a39_order_gate.py test_revocation_permission_human_scope_and_task_are_checked
// (PYT-0499) and test_t04_a40_public_revoke.py
// test_public_revoke_checks_permissions_human_scope_and_object_control (PYT-0510); X-INV-002,
// X-INV-003, TRD-INV-015: order.approval.revoke is refused to a reviewer without the permission
// (PERMISSION_DENIED), to an AI agent or a system actor holding every permission (human only:
// PERMISSION_DENIED) and to a reviewer whose data scope no longer covers the quote (NOT_FOUND, Python
// a generic refusal); while a task holds the quote root it is OBJECT_CONTROL_CONFLICT under the same
// key and under another, and the task's claim revokes. No refusal writes anything: the review stays
// approved and no OrderApprovalRevoked is published. Go has one command path, so Python's local and
// public halves are one test.
func TestOrderApprovalRevocationRefusals(t *testing.T) {
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	q.reviewed(o, "review.1")
	revoke := oeqvRevoke(q, o.ObjectID, "review.1")
	untouched := func(name string) {
		t.Helper()
		if r := q.getOrder(o.ObjectID).Reviews[0]; r.DecisionState != trdapi.OrderReviewApproved || len(q.eventsOf(trdapi.EventOrderApprovalRevoked)) != 0 {
			t.Fatalf("%s left residue: %+v", name, r)
		}
	}
	for name, c := range map[string]struct {
		as   func() context.Context
		code string
	}{
		"without order.approval.revoke": {func() context.Context { return q.regrant("reviewer", "company", []string{"trd.read"}) }, kernel.CodePermissionDenied},
		"an AI agent with every permission": {func() context.Context {
			return q.provision(grantOf(q, "reviewer", kernel.ActorAI, "company", allPermissions...))
		}, kernel.CodePermissionDenied},
		"a system actor with every permission": {func() context.Context {
			return q.provision(grantOf(q, "revoker.system", kernel.ActorSystem, "company", allPermissions...))
		}, kernel.CodePermissionDenied},
		"own data scope": {func() context.Context { return q.regrant("reviewer", "own", allPermissions) }, kernel.CodeNotFound},
	} {
		_, err := q.c.TRD.RevokeOrderApproval(c.as(), revoke)
		if code(err) != c.code {
			t.Errorf("%s: %s, want %s", name, code(err), c.code)
		}
		untouched(name)
	}
	q.regrant("reviewer", "company", allPermissions)

	tsk := q.actorOf(kernel.ActorSystem, "tsk.1")
	_, err := q.c.TRD.BindControl(tsk, trdapi.BindControl{IdempotencyKey: q.key(), QuoteID: "quote.1", TaskID: "task.1"})
	q.must(err)
	_, err = q.c.TRD.RevokeOrderApproval(q.reviewer, revoke)
	q.want(err, trdapi.CodeObjectControlConflict)
	bound := oeqvRevoke(q, o.ObjectID, "review.1")
	_, err = q.c.TRD.RevokeOrderApproval(q.reviewer, bound)
	q.want(err, trdapi.CodeObjectControlConflict)
	untouched("a bound root")
	rv, err := q.c.TRD.RevokeOrderApproval(trdapiClaim(t, q.reviewer), revoke)
	if err != nil || rv.Status != trdapi.OrderReviewRevoked || rv.Replayed {
		t.Fatalf("the task's revocation %+v %v", rv, err)
	}
}

// Python test_t04_a40_public_revoke.py test_historical_public_receipt_replays_after_reviewer_access_changes
// (PYT-0513); X-INV-001, X-INV-003: after the revocation commits, the reviewer is left with trd.read
// and order.approval.revoke only; the same key replays the revocation with its action ID and changes
// nothing. Go's ledger re-checks the command's permission on every replay: without
// order.approval.revoke the replay is PERMISSION_DENIED, while reconciling the key needs trd.read
// alone and answers committed.
func TestOrderRevocationReplaysAfterTheReviewerLostAccess(t *testing.T) {
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	q.reviewed(o, "review.1")
	revoke := oeqvRevoke(q, o.ObjectID, "review.1")
	first, err := q.c.TRD.RevokeOrderApproval(q.reviewer, revoke)
	q.must(err)
	before := jsonString(q.getOrder(o.ObjectID).Reviews)
	narrowed := q.regrant("reviewer", "company", []string{"trd.read", trdapi.CommandRevokeOrderApproval})
	again, err := q.c.TRD.RevokeOrderApproval(narrowed, revoke)
	if err != nil || !again.Replayed || again.ActionID != first.ActionID || again.Status != trdapi.OrderReviewRevoked {
		t.Fatalf("the replay after the reviewer's access changed %+v %v", again, err)
	}
	reader := q.regrant("reviewer", "company", []string{"trd.read"})
	_, err = q.c.TRD.RevokeOrderApproval(reader, revoke)
	q.want(err, kernel.CodePermissionDenied)
	if rec, err := q.c.TRD.Reconcile(reader, revoke); err != nil || rec.State != kernel.ReconcileCommitted || rec.ActionID != first.ActionID {
		t.Fatalf("reconciling %+v %v", rec, err)
	}
	if jsonString(q.getOrder(o.ObjectID).Reviews) != before || len(q.eventsOf(trdapi.EventOrderApprovalRevoked)) != 1 {
		t.Fatal("a replay changed the revocation")
	}
}

// Python test_t03_a20_acceptance_order.py test_commit_then_process_exit_reopens_one_active_order
// (PYT-0171); TRD-INV-016, X-INV-001: an order activation committed by one process is known to a
// fresh one (a new platform on its own connection pool, standing in for Python's child process that
// exits after the commit): it reconciles as committed with the action ID, and the same key replays
// once — one active order, one commit evidence, one OrderActivated.
func TestOrderActivationCommittedBeforeTheProcessExitReplaysOnce(t *testing.T) {
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	d := q.reviewed(o, "review.1")
	activate := q.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1")
	act, err := q.c.TRD.ActivateOrder(q.author, activate)
	q.must(err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fresh, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: pgtest.AppDSN(t)})
	q.must(err)
	defer fresh.Close()
	c, err := fresh.Contexts()
	q.must(err)
	if rec, err := c.TRD.Reconcile(q.author, activate); err != nil || rec.State != kernel.ReconcileCommitted || rec.ActionID != act.ActionID ||
		rec.ObjectID != o.ObjectID {
		t.Fatalf("a fresh process reconciles %+v %v", rec, err)
	}
	again, err := c.TRD.ActivateOrder(q.author, activate)
	if err != nil || !again.Replayed || again.ActionID != act.ActionID || again.Status != trdapi.OrderActive {
		t.Fatalf("the replay in a fresh process %+v %v", again, err)
	}
	if _, _, commits := q.orderRows(); commits != 1 || len(q.eventsOf(trdapi.EventOrderActivated)) != 1 ||
		q.getOrder(o.ObjectID).Status != trdapi.OrderActive {
		t.Fatalf("%d commit evidence, %d OrderActivated", commits, len(q.eventsOf(trdapi.EventOrderActivated)))
	}
}

// Python test_t03_a26_order_qualification.py
// test_later_cost_denial_does_not_retroactively_reject_committed_recovery (PYT-0249),
// test_later_revocation_and_configuration_retirement_keep_original_activation_fact (PYT-0250) and
// test_t03_a28_sales_terms.py test_committed_order_replay_keeps_original_sales_terms_after_retirement
// (PYT-0345); X-INV-001, TRD-INV-016: a committed order activation replays from its stored result,
// with its action ID and the order's facts unchanged, after each later upstream change — CST denies a
// source of the cost snapshot; POL retires the sales tax application the lines bind; POL retires the
// quote template, the formalization approval is revoked (a risk hold on the revision) and its
// reviewer keeps trd.read only. Reconciling the key answers committed throughout.
func TestOrderActivationReplaysKeepTheirFactAfterUpstreamChanges(t *testing.T) {
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	d := q.reviewed(o, "review.1")
	activate := q.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1")
	act, err := q.c.TRD.ActivateOrder(q.author, activate)
	q.must(err)
	fact := oeqvFact(q, o.ObjectID)
	replays := func(after string) {
		t.Helper()
		again, err := q.c.TRD.ActivateOrder(q.author, activate)
		if err != nil || !again.Replayed || again.ActionID != act.ActionID || again.Status != trdapi.OrderActive {
			t.Fatalf("the replay after %s %+v %v", after, again, err)
		}
		if rec, err := q.c.TRD.Reconcile(q.author, activate); err != nil || rec.State != kernel.ReconcileCommitted || rec.ActionID != act.ActionID {
			t.Fatalf("reconciling after %s %+v %v", after, rec, err)
		}
		if oeqvFact(q, o.ObjectID) != fact || len(q.eventsOf(trdapi.EventOrderActivated)) != 1 {
			t.Fatalf("the order changed after %s", after)
		}
	}

	q.deny()
	replays("the cost denial (PYT-0249)")

	_, err = q.c.POL.Retire(q.reviewer, polapi.Retire{IdempotencyKey: q.key(), Kind: q.sale.Kind, ConfigID: q.sale.ConfigID, ExpectedVersion: 2,
		Reason: "停用销售税适用"})
	q.must(err)
	replays("the sales tax application's retirement (PYT-0345)")

	_, err = q.c.POL.Retire(q.reviewer, polapi.Retire{IdempotencyKey: q.key(), Kind: q.template.Kind, ConfigID: q.template.ConfigID,
		ExpectedVersion: 2, Reason: "停用报价模板"})
	q.must(err)
	_, err = q.c.TRD.RevokeApproval(q.reviewer, trdapi.RevokeApproval{IdempotencyKey: q.key(), QuoteID: "quote.1", ApprovalID: "approval.quote.1",
		ExpectedEffectivityVersion: 1, Reason: "批准依据失效", Evidence: q.proof})
	q.must(err)
	q.regrant("reviewer", "company", []string{"trd.read"})
	replays("the template's retirement, the approval's revocation and the reviewer's loss (PYT-0250)")
}

// Python test_t03_a26_order_qualification.py test_restored_reviewer_permission_allows_a_previous_rejected_key
// (PYT-0251); X-INV-001: an order activation refused because the formalization approval's reviewer
// lost quote.review.decide (APPROVAL_STALE) writes nothing, the command ledger included; once the
// grant is restored the same key activates the order — executed, not replayed.
func TestRefusedOrderActivationDoesNotConsumeItsKey(t *testing.T) {
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	r, err := q.c.TRD.RequestOrderReview(q.author, q.requestCmd(o.ObjectID, 1, "review.1"))
	q.must(err)
	orderReviewer := q.regrant("order.reviewer", "company", allPermissions)
	d, err := q.c.TRD.DecideOrderReview(orderReviewer, q.decideCmd(o.ObjectID, r.RowVersion, "review.1", o.OrderHash))
	q.must(err)
	activate := q.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1")
	q.regrant("reviewer", "company", without(allPermissions, "quote.review.decide"))
	_, err = q.c.TRD.ActivateOrder(q.author, activate)
	q.want(err, trdapi.CodeApprovalStale)
	if rec, err := q.c.TRD.Reconcile(q.author, activate); err != nil || rec.State != kernel.ReconcileNotCommitted {
		t.Fatalf("the refused key %+v %v", rec, err)
	}
	if _, _, commits := q.orderRows(); commits != 0 || q.getOrder(o.ObjectID).Status != trdapi.OrderDraft {
		t.Fatal("a refused activation wrote")
	}
	q.regrant("reviewer", "company", allPermissions)
	act, err := q.c.TRD.ActivateOrder(q.author, activate)
	if err != nil || act.Replayed || act.Status != trdapi.OrderActive {
		t.Fatalf("the same key after the grant was restored %+v %v", act, err)
	}
}

// Python test_t03_a26_order_qualification.py test_formalized_replacement_uses_its_own_bound_approval
// (PYT-0259); TRD-INV-003, 014: revision 2 replaces active revision 1 on its own approval; the
// customer accepts revision 2 and its order activates on the approval revision 2's commit evidence
// names — the commit evidence records revision 2's business hash and that approval, not revision
// 1's.
func TestOrderOnAReplacementUsesTheReplacementsApproval(t *testing.T) {
	q := newOrderWorld(t)
	q.activated("quote.1")
	rev, hash, v := q.revisedAndApproved("quote.1", q.getQuote("quote.1").RootVersion, trdapi.TargetReplace, "review.r2", "approval.r2")
	_, err := q.c.TRD.Replace(q.author, q.replaceCmd("quote.1", v, rev, "approval.r2"))
	q.must(err)
	_, err = q.c.TRD.RecordResponse(q.author, q.full("response.r2", "quote.1", rev, hash))
	q.must(err)
	a, err := q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.r2", "acceptance.r2", rev, hash))
	q.must(err)
	o, err := q.c.TRD.PrepareOrder(q.author, q.prepareCmd("order.r2", a))
	q.must(err)
	d := q.reviewed(o, "review.order.r2")
	act, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o.ObjectID, d.RowVersion, "review.order.r2"))
	if err != nil || act.Status != trdapi.OrderActive || act.BusinessRevision != rev || act.BusinessHash != hash {
		t.Fatalf("the order on the replacement %+v %v", act, err)
	}
	e := q.getOrder(o.ObjectID).Commits[0].Entries
	if e["formalization_approval_id"] != "approval.r2" || e["business_hash"] != hash {
		t.Fatalf("the commit evidence %v", e)
	}
}

// oeqvOwner runs statements in one transaction as the schema owner in company.A's scope.
func oeqvOwner(q *trdWorld, statements ...string) error {
	tx, err := pgtest.MigratorPool(q.t).Begin(context.Background())
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`,
		q.tenant); err != nil {
		return err
	}
	for _, s := range statements {
		if _, err = tx.Exec(context.Background(), s); err != nil {
			return err
		}
	}
	return tx.Commit(context.Background())
}

// Python test_t03_a26_order_qualification.py test_formal_file_bytes_are_rehashed_not_only_metadata_checked
// (PYT-0240); TRD-INV-009, 014: the database refuses formal document bytes that do not hash to the
// stored hash or size even for the owner with the document's guard trigger off (CHECK, 23514), so
// Python's "bytes changed, metadata kept" cannot be stored; bytes rewritten together with their hash
// and size are caught by the order's activation, which re-hashes the stored bytes against the
// document the order bound (FORMAL_DOCUMENT_INTEGRITY_FAILURE) and writes nothing. With the bytes
// restored the same order activates.
func TestOrderActivationRehashesTheFormalDocument(t *testing.T) {
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	d := q.reviewed(o, "review.1")
	where := ` WHERE tenant_id = '` + q.tenant + `' AND quote_id = 'quote.1' AND revision = 1`
	off := "ALTER TABLE brant_trd.quote_document DISABLE TRIGGER quote_document_guard"
	on := "ALTER TABLE brant_trd.quote_document ENABLE TRIGGER quote_document_guard"
	if got := sqlCode(oeqvOwner(q, off, `UPDATE brant_trd.quote_document SET content = content || '\x0a'::bytea`+where, on)); got != "23514" {
		t.Fatalf("bytes that do not hash to the stored hash: %s", got)
	}
	q.must(oeqvOwner(q, off, `UPDATE brant_trd.quote_document SET content = content || '\x0a'::bytea, size_bytes = size_bytes + 1,
		content_sha256 = encode(sha256(content || '\x0a'::bytea), 'hex')`+where, on))
	activate := q.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1")
	_, err := q.c.TRD.ActivateOrder(q.author, activate)
	q.want(err, trdapi.CodeFormalDocumentIntegrityFailure)
	if _, _, commits := q.orderRows(); commits != 0 || q.getOrder(o.ObjectID).Status != trdapi.OrderDraft ||
		len(q.eventsOf(trdapi.EventOrderActivated)) != 0 {
		t.Fatal("an activation on rewritten bytes wrote")
	}
	q.must(oeqvOwner(q, off, `UPDATE brant_trd.quote_document SET content = substring(content from 1 for size_bytes - 1),
		size_bytes = size_bytes - 1, content_sha256 = encode(sha256(substring(content from 1 for size_bytes - 1)), 'hex')`+where, on))
	if act, err := q.c.TRD.ActivateOrder(q.author, activate); err != nil || act.Status != trdapi.OrderActive {
		t.Fatalf("the bound bytes restored %+v %v", act, err)
	}
}
