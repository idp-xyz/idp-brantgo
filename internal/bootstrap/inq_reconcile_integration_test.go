//go:build integration

package bootstrap_test

import (
	"context"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Every INQ command reconciles from the command as it was sent, hashed as its execution hashed it,
// without executing anything (Python test_t03_a20_acceptance_order.py
// test_action_reconciliation_classifies_committed_missing_and_conflict and
// test_t03_a23_reconciliation_access.py test_reconciliation_is_observational_and_audited; T01 has
// no reconciliation of its own).
func TestRfqCommandsReconcileWithoutExecuting(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	save := w.readyArgs(ctx)
	save.IdempotencyKey = "k.save"
	confirm := inqapi.ConfirmRfqScope{IdempotencyKey: "k.confirm", RfqID: "rfq.1", ExpectedVersion: 1, EvidenceIDs: []string{"src.1"}}
	closing := inqapi.CloseRfq{IdempotencyKey: "k.close", RfqID: "rfq.1", ExpectedVersion: 2, Reason: "客户不再推进"}
	draft := inqapi.SaveRfq{IdempotencyKey: "k.draft", RfqID: "rfq.2", Lines: []inqapi.RfqLine{{LineID: "L1", RequestedItem: "待核实商品"}}}
	cancel := inqapi.CancelRfq{IdempotencyKey: "k.cancel", RfqID: "rfq.2", ExpectedVersion: 1, Reason: "客户撤回"}
	var receipts []inqapi.Receipt
	for _, run := range []func() (inqapi.Receipt, error){
		func() (inqapi.Receipt, error) { return w.c.INQ.SaveRfq(ctx, save) },
		func() (inqapi.Receipt, error) { return w.c.INQ.ConfirmRfqScope(ctx, confirm) },
		func() (inqapi.Receipt, error) { return w.c.INQ.CloseRfq(ctx, closing) },
		func() (inqapi.Receipt, error) { return w.c.INQ.SaveRfq(ctx, draft) },
		func() (inqapi.Receipt, error) { return w.c.INQ.CancelRfq(ctx, cancel) },
	} {
		r, err := run()
		w.must(err)
		receipts = append(receipts, r)
	}
	rfqs, events, audits := w.count("company.A", "brant_inq.customer_rfq"), len(w.events("company.A", inqapi.Source)), len(w.audit(ctx))
	for i, cmd := range []inqapi.Command{save, confirm, closing, draft, cancel} {
		rec, err := w.c.INQ.Reconcile(ctx, cmd)
		w.must(err)
		if rec.State != kernel.ReconcileCommitted || rec.Receipt == nil || *rec.Receipt != receipts[i] || rec.ActionID != receipts[i].ActionID {
			t.Fatalf("command %d: %+v, receipt %+v; want committed with %+v", i, rec, rec.Receipt, receipts[i])
		}
	}
	// The expected version is not part of the request; the business fields are.
	stale := save
	stale.ExpectedVersion = 5
	if rec, err := w.c.INQ.Reconcile(ctx, stale); err != nil || rec.State != kernel.ReconcileCommitted {
		t.Fatalf("other CAS value: %+v, %v", rec, err)
	}
	changed := save
	changed.UnresolvedFields = []string{"交期"}
	if rec, err := w.c.INQ.Reconcile(ctx, changed); err != nil || rec.State != kernel.ReconcilePayloadConflict ||
		rec.Receipt != nil || rec.ObjectID != "rfq.1" {
		t.Fatalf("changed body: %+v, %v; want payload_conflict", rec, err)
	}
	never := save
	never.IdempotencyKey = "k.never"
	if rec, err := w.c.INQ.Reconcile(ctx, never); err != nil || rec != (inqapi.Reconciliation{State: kernel.ReconcileNotCommitted}) {
		t.Fatalf("unknown key: %+v, %v; want not_committed", rec, err)
	}
	if w.count("company.A", "brant_inq.customer_rfq") != rfqs || len(w.events("company.A", inqapi.Source)) != events {
		t.Fatal("a reconciliation changed RFQs or published events")
	}
	reads := 0
	for _, e := range w.audit(ctx)[audits:] {
		if e.Command == action.ReconcileQuery && e.Outcome == "read" {
			reads++
		}
	}
	if reads != 8 {
		t.Fatalf("%d reconciliations audited as reads, want 8", reads)
	}
}

// A reader recovers a receipt without the command's permission; without the read permission it
// is refused, and an RFQ outside its data scope is not found (Python
// test_t03_a23_reconciliation_access.py test_readers_can_recover_receipts_without_write_grants and
// test_own_team_company_scope_applies_to_committed_and_conflicting_receipts).
func TestRfqReconciliationNeedsReadPermissionAndDataScope(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	draft := inqapi.SaveRfq{IdempotencyKey: "k.draft", RfqID: "rfq.1", Lines: []inqapi.RfqLine{{LineID: "L1", RequestedItem: "待核实商品"}}}
	_, err := w.c.INQ.SaveRfq(ctx, draft)
	w.must(err)
	reader := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "reader.1"),
		Permissions: []string{"inq.read"}, DataScope: authorization.DataScopeCompany})
	if rec, err := w.c.INQ.Reconcile(reader, draft); err != nil || rec.State != kernel.ReconcileCommitted {
		t.Fatalf("reader without rfq.save_draft: %+v, %v; want committed", rec, err)
	}
	writer := w.actor("writer.1", "rfq.save_draft")
	_, err = w.c.INQ.Reconcile(writer, draft)
	w.want(err, kernel.CodePermissionDenied)
	outsider := w.actor("outsider.1", "inq.read")
	_, err = w.c.INQ.Reconcile(outsider, draft)
	w.want(err, kernel.CodeNotFound)
}

func (w *world) audit(ctx context.Context) []kernel.AuditEntry {
	w.t.Helper()
	entries, err := w.c.INQ.Audit(ctx)
	w.must(err)
	return entries
}
