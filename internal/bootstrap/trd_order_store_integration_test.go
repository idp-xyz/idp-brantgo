//go:build integration

package bootstrap_test

import (
	"strings"
	"testing"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TRD-INV-016, BC-AT19 and Python test_t03_a20 test_order_prepare_failure_rolls_back_order_and_event
// (PYT-0170), test_t03_a25 test_prepare_failure_rolls_back_snapshot_events_and_success_ledger
// (PYT-0216), test_t03_a26 test_post_activation_failure_rolls_back_qualification_event_and_ledger
// (PYT-0247), test_t04_a38 test_original_quote_approval_still_required_and_atomic_failure (PYT-0483,
// the failure half) and test_t04_a39 test_public_activation_audit_failure_rolls_back_link_event_order_and_receipt
// (PYT-0501), test_revocation_audit_failure_rolls_back_every_business_record (PYT-0500) and
// test_t04_a40_public_revoke.py test_rejected_public_revoke_has_no_success_ledger_and_audit_failure_rolls_back
// (PYT-0511: Go has one command path; the failed revocation leaves no action result, so the retry
// under the same key executes rather than replays): a storage
// failure inside a preparation leaves no order, no slot and no event, the root unmoved; inside an
// activation (the commit evidence, or the Outbox) the order stays a draft with no evidence and no
// event; inside a revocation the review stays approved. Each retry with the same key succeeds.
func TestOrderWritesFailWithoutResidue(t *testing.T) {
	q := newOrderWorld(t)
	_, a := q.accepted("quote.1")
	before := q.getQuote("quote.1").RootVersion
	prepare := q.prepareCmd("order.1", a)
	drop := q.failInsert("brant_trd.sales_order")
	_, err := q.c.TRD.PrepareOrder(q.author, prepare)
	drop()
	if _, rejected := kernel.AsRejection(err); err == nil || rejected || !strings.Contains(err.Error(), "test storage failure") {
		t.Fatalf("a failing order insert: %v", err)
	}
	if orders, _, _ := q.orderRows(); orders != 0 || q.getQuote("quote.1").RootVersion != before || q.getQuote("quote.1").Conversion != nil ||
		len(q.eventsOf(trdapi.EventOrderPrepared)) != 0 {
		t.Fatal("a failed preparation left residue")
	}
	o, err := q.c.TRD.PrepareOrder(q.author, prepare)
	q.must(err)
	d := q.reviewed(o, "review.1")
	activate := q.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1")
	for _, fault := range []struct {
		name string
		arm  func() func()
	}{
		{"the commit evidence", func() func() { return q.failInsert("brant_trd.sales_order_commit_evidence") }},
		{"the Outbox", func() func() { return q.failOutbox(trdapi.EventOrderActivated) }},
	} {
		drop := fault.arm()
		_, err = q.c.TRD.ActivateOrder(q.author, activate)
		drop()
		if _, rejected := kernel.AsRejection(err); err == nil || rejected || !strings.Contains(err.Error(), "test storage failure") {
			t.Fatalf("%s failing: %v", fault.name, err)
		}
		if _, _, commits := q.orderRows(); commits != 0 || q.getOrder(o.ObjectID).Status != trdapi.OrderDraft ||
			len(q.eventsOf(trdapi.EventOrderActivated)) != 0 {
			t.Fatalf("%s failing left residue", fault.name)
		}
	}
	act, err := q.c.TRD.ActivateOrder(q.author, activate)
	if err != nil || act.Status != trdapi.OrderActive || act.Replayed {
		t.Fatalf("the retry %+v %v", act, err)
	}
	revoke := trdapi.RevokeOrderApproval{IdempotencyKey: q.key(), OrderID: o.ObjectID, ReviewID: "review.1", Reason: "客户条件变化"}
	drop = q.failOutbox(trdapi.EventOrderApprovalRevoked)
	_, err = q.c.TRD.RevokeOrderApproval(q.reviewer, revoke)
	drop()
	if err == nil {
		t.Fatal("a failing revocation succeeded")
	}
	if r := q.getOrder(o.ObjectID).Reviews[0]; r.Status != trdapi.OrderReviewApproved {
		t.Fatalf("after the failed revocation %+v", r)
	}
	if r, err := q.c.TRD.RevokeOrderApproval(q.reviewer, revoke); err != nil || r.Replayed || r.Status != trdapi.OrderReviewRevoked {
		t.Fatalf("the retry %+v %v", r, err)
	}
}

// Python test_t03_a25 test_database_rejects_commercial_column_changes_and_delete (PYT-0205),
// test_database_rejects_replace_by_primary_key_and_root_slot (PYT-0223, the PostgreSQL analogue) and
// test_t04_a38 test_records_immutable_and_legacy_activation_requires_order_approval (PYT-0484, the
// storage half): the database keeps an order's identity, binding and snapshot (no update of them, no
// second order of the root, no delete, no row version that does not move by one, no activation
// without its commit evidence); a review is decided once and never by the order's preparer; commit
// evidence and closures never change.
func TestDatabaseRefusesIllegalOrderChanges(t *testing.T) {
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	d := q.reviewed(o, "review.1")
	where := ` WHERE tenant_id = '` + q.tenant + `' AND order_id = '` + o.ObjectID + `'`
	for name, sql := range map[string]string{
		"the snapshot":              `UPDATE brant_trd.sales_order SET snapshot = snapshot || ' ', order_hash = encode(sha256(convert_to(snapshot || ' ', 'UTF8')), 'hex'), row_version = row_version + 1` + where,
		"the binding":               `UPDATE brant_trd.sales_order SET acceptance_id = 'other', row_version = row_version + 1` + where,
		"the row version by two":    `UPDATE brant_trd.sales_order SET row_version = row_version + 2` + where,
		"active without evidence":   `UPDATE brant_trd.sales_order SET status = 'active', activated_review_id = 'review.1', row_version = row_version + 1` + where,
		"cancelled without closure": `UPDATE brant_trd.sales_order SET status = 'cancelled', row_version = row_version + 1` + where,
		"a delete":                  `DELETE FROM brant_trd.sales_order` + where,
		"the decision": `UPDATE brant_trd.sales_order_review SET status = 'rejected', decision_reason = 'x', expires_at = NULL,
			acknowledged_risks = NULL` + where,
		"a second order of the root": `INSERT INTO brant_trd.sales_order SELECT tenant_id, company_id, 'order.second', quote_id, slot, revision, business_hash, number, document_sha256, acceptance_id, acceptance_version, arrival_at, arrival_basis, expires_at, order_revision, snapshot, order_hash, 'draft', 1, prepared_by_kind, prepared_by_id, prepare_epoch, prepared_at, NULL, updated_at FROM brant_trd.sales_order` + where,
	} {
		if err := q.exec("company.A", sql); err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
	}
	// An open review decided by the order's preparer never stands.
	_, o2 := q.prepared("quote.2")
	_, err := q.c.TRD.RequestOrderReview(q.reviewer, q.requestCmd(o2.ObjectID, 1, "review.2"))
	q.must(err)
	if err = q.exec("company.A", `UPDATE brant_trd.sales_order_review SET status = 'rejected', decided_by_kind = 'human', decided_by_id = 'editor',
		decide_epoch = 1, decided_at = now(), decision_reason = 'x', decision_evidence = '[]' WHERE tenant_id = '`+q.tenant+`' AND order_id = '`+
		o2.ObjectID+`'`); err == nil {
		t.Error("a review decided by the preparer: the database accepted it")
	}
	act, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1"))
	q.must(err)
	for name, sql := range map[string]string{
		"the commit evidence": `UPDATE brant_trd.sales_order_commit_evidence SET review_id = 'other'` + where,
		"an active order back to draft": `UPDATE brant_trd.sales_order SET status = 'draft', activated_review_id = NULL, row_version = row_version + 1` +
			where,
	} {
		if err := q.exec("company.A", sql); err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
	}
	if v := q.getOrder(o.ObjectID); v.RowVersion != act.RowVersion || v.Status != trdapi.OrderActive {
		t.Fatalf("after the refusals %+v", v)
	}
}

// Python test_t03_a20 test_acceptance_and_order_queries_are_scoped (PYT-0169, the order half) and
// test_action_reconciliation_classifies_committed_missing_and_conflict (PYT-0173),
// test_t04_a42_public_review_query.py test_query_requires_read_scope_and_does_not_leak_unknown_review
// (PYT-0520), test_query_rejects_extra_fields_and_cross_scope_review (PYT-0521, the scope half) and
// test_query_current_access_is_rechecked_after_review_was_submitted (PYT-0522), and
// test_t04_a43_order_reconciliation.py test_request_decide_and_revoke_receipts_are_committed_and_conflict_safe
// (PYT-0523), test_reconciliation_requires_business_read_and_does_not_repeat_write_permission
// (PYT-0524) and test_missing_receipt_is_explicit_and_unknown_order_review_command_is_closed
// (PYT-0525, the receipt half): reading an order needs trd.read and its quote in the reader's scope
// (NOT_FOUND otherwise, nothing leaked); the owner of the quote reads with own scope; reconciliation
// needs trd.read only, reports each order command committed, not committed or a payload conflict, and
// is the order's object for review commands.
func TestOrderReadsAndReconciliation(t *testing.T) {
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	request := q.requestCmd(o.ObjectID, 1, "review.1")
	r, err := q.c.TRD.RequestOrderReview(q.author, request)
	q.must(err)
	decide := q.decideCmd(o.ObjectID, r.RowVersion, "review.1", o.OrderHash)
	_, err = q.c.TRD.DecideOrderReview(q.reviewer, decide)
	q.must(err)
	revoke := trdapi.RevokeOrderApproval{IdempotencyKey: q.key(), OrderID: o.ObjectID, ReviewID: "review.1", Reason: "Synthetic revocation"}
	_, err = q.c.TRD.RevokeOrderApproval(q.reviewer, revoke)
	q.must(err)
	noRead := q.regrant("reader.none", "company", without(allPermissions, "trd.read"))
	outsider := q.regrant("outsider", "own", allPermissions)
	owner := q.regrant("editor", "own", []string{"trd.read"})
	if _, err = q.c.TRD.GetOrder(noRead, o.ObjectID); code(err) != kernel.CodePermissionDenied {
		t.Errorf("without trd.read: %s", code(err))
	}
	for _, id := range []string{o.ObjectID, "order.none"} {
		if _, err = q.c.TRD.GetOrder(outsider, id); code(err) != kernel.CodeNotFound {
			t.Errorf("an outsider reads %s: %s", id, code(err))
		}
	}
	if v, err := q.c.TRD.GetOrder(owner, o.ObjectID); err != nil || v.Reviews[0].DecisionState != trdapi.OrderReviewRevoked ||
		v.Reviews[0].RevokeReason != "Synthetic revocation" {
		t.Fatalf("the quote's owner reads %+v %v", v, err)
	}
	if _, err = q.c.TRD.CheckOrder(outsider, o.ObjectID, ""); code(err) != kernel.CodeNotFound {
		t.Errorf("an outsider checks: %s", code(err))
	}
	for _, c := range []trdapi.Command{request, decide, revoke} {
		rec, err := q.c.TRD.Reconcile(owner, c)
		if err != nil || rec.State != kernel.ReconcileCommitted || rec.ObjectID != o.ObjectID {
			t.Fatalf("reconcile %T %+v %v", c, rec, err)
		}
	}
	other := request
	other.ReviewID = "review.other"
	if rec, err := q.c.TRD.Reconcile(owner, other); err != nil || rec.State != kernel.ReconcilePayloadConflict {
		t.Fatalf("another payload %+v %v", rec, err)
	}
	missing := request
	missing.IdempotencyKey = q.key()
	if rec, err := q.c.TRD.Reconcile(owner, missing); err != nil || rec.State != kernel.ReconcileNotCommitted {
		t.Fatalf("a key never sent %+v %v", rec, err)
	}
	if _, err = q.c.TRD.Reconcile(noRead, request); code(err) != kernel.CodePermissionDenied {
		t.Errorf("reconcile without trd.read: %s", code(err))
	}
	if audit, err := q.c.TRD.Audit(q.reviewer); err != nil || !hasAudit(audit, "order.get") || !hasAudit(audit, "order.check") ||
		!hasAudit(audit, trdapi.CommandRevokeOrderApproval) {
		t.Fatalf("the audit trail %v", err)
	}
}

func hasAudit(entries []kernel.AuditEntry, command string) bool {
	for _, e := range entries {
		if e.Command == command {
			return true
		}
	}
	return false
}
