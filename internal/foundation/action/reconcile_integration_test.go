//go:build integration

package action_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func noteHash(t *testing.T, id, body string) string {
	t.Helper()
	hash, err := action.RequestHash(struct{ ID, Body string }{id, body})
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func (r *rig) reconcile(ctx context.Context, key, id, body string) (action.Reconciliation, error) {
	return r.ledger.Reconcile(ctx, "note.save", key, noteHash(r.t, id, body))
}

var conflictingHash = strings.Repeat("0", 64)

// Python test_t03_a20_acceptance_order.py test_action_reconciliation_classifies_committed_missing_and_conflict.
func TestReconcileClassifiesCommittedConflictAndMissing(t *testing.T) {
	r := newRig(t)
	ctx := r.as(human)
	saved, err := r.save(ctx, "k1", "n1", "hello", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := r.reconcile(ctx, "k1", "n1", "hello")
	if err != nil || rec.State != kernel.ReconcileCommitted || rec.Result == nil || *rec.Result != saved ||
		rec.ActionID != saved.ActionID || rec.ObjectID != "n1" {
		t.Fatalf("committed: %+v %+v, %v; want the stored result %+v", rec, rec.Result, err, saved)
	}
	rec, err = r.ledger.Reconcile(ctx, "note.save", "k1", conflictingHash)
	if err != nil || rec.State != kernel.ReconcilePayloadConflict || rec.Result != nil || rec.ActionID != saved.ActionID || rec.ObjectID != "n1" {
		t.Fatalf("payload conflict: %+v, %v", rec, err)
	}
	rec, err = r.reconcile(ctx, "k.missing", "n1", "hello")
	if err != nil || rec != (action.Reconciliation{State: kernel.ReconcileNotCommitted}) {
		t.Fatalf("not committed: %+v, %v", rec, err)
	}
}

// Python test_t03_a23_reconciliation_access.py test_reconciliation_is_observational_and_audited.
func TestReconcileIsObservationalAndAudited(t *testing.T) {
	r := newRig(t)
	ctx := r.as(human)
	if _, err := r.save(ctx, "k1", "n1", "hello", 0); err != nil {
		t.Fatal(err)
	}
	notes, results, events, audits := r.count("note"), r.count("action_result"), r.events(), len(r.audit())
	for _, hash := range []string{noteHash(t, "n1", "hello"), conflictingHash} {
		if _, err := r.ledger.Reconcile(ctx, "note.save", "k1", hash); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.reconcile(ctx, "k.missing", "n1", "hello"); err != nil {
		t.Fatal(err)
	}
	if r.count("note") != notes || r.count("action_result") != results || r.events() != events {
		t.Fatal("a reconciliation changed business data, the ledger or the Outbox")
	}
	a := r.audit()[audits:]
	if len(a) != 3 {
		t.Fatalf("want three read entries, got %+v", a)
	}
	for i, wantObject := range []string{"n1", "n1", ""} {
		if a[i].Command != action.ReconcileQuery || a[i].Outcome != "read" || a[i].ObjectID != wantObject || a[i].AuthorityEpoch < 1 {
			t.Fatalf("audit %d: %+v", i, a[i])
		}
	}
}

// Python test_t03_a23_reconciliation_access.py test_readers_can_recover_receipts_without_write_grants,
// test_business_read_permission_is_required_for_order_and_acceptance_receipts and
// test_t04_a43_order_reconciliation.py test_reconciliation_requires_business_read_and_does_not_repeat_write_permission.
func TestReconcileNeedsReadPermissionNotWritePermission(t *testing.T) {
	r := newRig(t)
	reader := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reader.1"}
	writer := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "writer.1"}
	r.grant(reader, "note.read")
	r.grant(writer, "note.save")
	r.admin(func(ctx context.Context, exec bentopg.Executor) error {
		_, err := authorization.Grants{}.SetDataScope(ctx, exec, r.scope, reader.ID, authorization.DataScopeCompany, "", time.Now())
		return err
	})
	if _, err := r.save(r.as(human), "k1", "n1", "hello", 0); err != nil {
		t.Fatal(err)
	}
	if rec, err := r.reconcile(r.as(reader), "k1", "n1", "hello"); err != nil || rec.State != kernel.ReconcileCommitted {
		t.Fatalf("reader without the write grant: %+v, %v; want committed", rec, err)
	}
	if rec, err := r.ledger.Reconcile(r.as(reader), "note.save", "k1", conflictingHash); err != nil || rec.State != kernel.ReconcilePayloadConflict {
		t.Fatalf("reader, conflicting hash: %+v, %v", rec, err)
	}
	audits := len(r.audit())
	for _, hash := range []string{noteHash(t, "n1", "hello"), conflictingHash} {
		for _, key := range []string{"k1", "k.missing"} {
			if _, err := r.ledger.Reconcile(r.as(writer), "note.save", key, hash); code(err) != kernel.CodePermissionDenied {
				t.Fatalf("writer without the read grant, %s: want PERMISSION_DENIED, got %v", key, err)
			}
		}
	}
	a := r.audit()[audits:]
	if len(a) != 4 || a[0].Outcome != "rejected" || a[0].Code != kernel.CodePermissionDenied || a[0].Command != action.ReconcileQuery || a[0].Actor.ID != writer.ID {
		t.Fatalf("rejections are audited: %+v", a)
	}
}

// Python test_t03_a23_reconciliation_access.py test_revoked_grants_and_team_changes_apply_after_reopening and
// test_own_team_company_scope_applies_to_committed_and_conflicting_receipts: like a replay, a
// reconciliation re-checks the current grant and data scope.
func TestReconcileRechecksRevocationAndDataScope(t *testing.T) {
	r := newRig(t)
	r.grant(other, "note.save", "note.read")
	if _, err := r.save(r.as(human), "k1", "n1", "hello", 0); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{noteHash(t, "n1", "hello"), conflictingHash} {
		if _, err := r.ledger.Reconcile(r.as(other), "note.save", "k1", hash); code(err) != kernel.CodeNotFound {
			t.Fatalf("object outside the data scope: want NOT_FOUND for both hashes, got %v", err)
		}
	}
	// A replay of the same key is NOT_FOUND too: reconciliation reveals no more than a replay.
	if _, err := r.save(r.as(other), "k1", "n1", "hello", 0); code(err) != kernel.CodeNotFound {
		t.Fatalf("replay outside the data scope: %v", err)
	}
	r.admin(func(ctx context.Context, exec bentopg.Executor) error {
		_, err := authorization.Grants{}.SetDataScope(ctx, exec, r.scope, other.ID, authorization.DataScopeCompany, "", time.Now())
		return err
	})
	if rec, err := r.reconcile(r.as(other), "k1", "n1", "hello"); err != nil || rec.State != kernel.ReconcileCommitted {
		t.Fatalf("company data scope: %+v, %v", rec, err)
	}
	r.admin(func(ctx context.Context, exec bentopg.Executor) error {
		_, err := authorization.Grants{}.Revoke(ctx, exec, r.scope, human.ID, time.Now())
		return err
	})
	for _, key := range []string{"k1", "k.missing"} {
		if _, err := r.reconcile(r.as(human), key, "n1", "hello"); code(err) != kernel.CodePermissionDenied {
			t.Fatalf("revoked actor, %s: want PERMISSION_DENIED, got %v", key, err)
		}
	}
}

// Python test_t03_a23_reconciliation_access.py test_company_and_tenant_boundaries_do_not_reveal_another_ledger.
func TestReconcileNeverReadsAnotherScope(t *testing.T) {
	r := newRig(t)
	if _, err := r.save(r.as(human), "k1", "n1", "hello", 0); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []kernel.Scope{
		{Tenant: r.scope.Tenant, Company: "other.company"},
		{Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: r.scope.Company},
	} {
		err := bento.ScopeTransactor{DB: r.db, Scope: scope}.WithinTransaction(context.Background(), func(ctx context.Context) error {
			exec, err := r.db.RequireExecutor(ctx)
			if err != nil {
				return err
			}
			g := authorization.Grants{}
			if err = g.Provision(ctx, exec, scope); err != nil {
				return err
			}
			if _, err = g.Put(ctx, exec, scope, human, []string{"note.save", "note.read"}, time.Now()); err != nil {
				return err
			}
			_, err = g.SetDataScope(ctx, exec, scope, human.ID, authorization.DataScopeCompany, "", time.Now())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: human, Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		for _, hash := range []string{noteHash(t, "n1", "hello"), conflictingHash} {
			rec, err := r.ledger.Reconcile(ctx, "note.save", "k1", hash)
			if err != nil || rec != (action.Reconciliation{State: kernel.ReconcileNotCommitted}) {
				t.Fatalf("%s: %+v, %v; want not_committed and nothing else", scope, rec, err)
			}
		}
	}
}

// Python test_t03_a23_reconciliation_access.py test_unknown_command_is_rejected_by_both_request_schemas.
func TestReconcileRejectsMalformedQueries(t *testing.T) {
	r := newRig(t)
	ctx := r.as(human)
	hash := noteHash(t, "n1", "hello")
	for _, c := range []struct{ command, key, hash, path string }{
		{"note.unknown", "k1", hash, "/args/command"},
		{"note.save\n", "k1", hash, "/args/command"},
		{"note.save", "k 1", hash, "/args/idempotency_key"},
		{"note.save", "", hash, "/args/idempotency_key"},
		{"note.save", "k1", strings.ToUpper(hash), "/args/request_hash"},
		{"note.save", "k1", hash[:63], "/args/request_hash"},
	} {
		_, err := r.ledger.Reconcile(ctx, c.command, c.key, c.hash)
		rej, ok := kernel.AsRejection(err)
		if !ok || rej.Code != kernel.CodeSchemaInvalid || rej.Fields[0].Path != c.path {
			t.Errorf("%q %q %q: %v; want SCHEMA_INVALID at %s", c.command, c.key, c.hash, err, c.path)
		}
	}
}

// faultyLedger is r's ledger on a pool whose connections lose the COMMIT f is armed for, with the
// Outbox store on the same transaction owner.
func (r *rig) faultyLedger(f *pgtest.CommitFault) *action.Ledger {
	r.t.Helper()
	db, err := bentopg.NewDB(pgtest.FaultyAppPool(r.t, f), bentopg.WithSchema(r.db.Schema()))
	if err != nil {
		r.t.Fatal(err)
	}
	store, err := bentooutbox.NewStore(db)
	if err != nil {
		r.t.Fatal(err)
	}
	l := *r.ledger
	l.DB, l.Outbox = db, store
	return &l
}

func saveVia(t *testing.T, l *action.Ledger, r *rig, ctx context.Context, key, id, body string) (action.Result, error) {
	t.Helper()
	return l.Execute(ctx, action.Command{Name: "note.save", IdempotencyKey: key, RequestHash: noteHash(t, id, body), ObjectID: id},
		&saveNote{r: r, id: id, body: body})
}

func requireCommitUncertain(t *testing.T, res action.Result, err error) {
	t.Helper()
	if !errors.Is(err, application.ErrCommitUncertain) {
		t.Fatalf("want application.ErrCommitUncertain, got %v", err)
	}
	if _, ok := kernel.AsRejection(err); ok || res != (action.Result{}) {
		t.Fatalf("an uncertain commit must be neither a rejection nor a result: %+v, %v", res, err)
	}
}

// Python test_t03_a23_reconciliation_access.py test_commit_then_process_exit_recovers_public_action_and_replays_once:
// the server commits and the answer is lost. Reconcile reports committed, and a replay of the
// same key returns the stored result with the same action ID; nothing executes twice.
func TestCommitUncertainAfterCommitIsReconciledAndReplayed(t *testing.T) {
	r := newRig(t)
	ctx := r.as(human)
	fault := &pgtest.CommitFault{}
	fault.Arm(pgtest.LoseAnswer)
	res, err := saveVia(t, r.faultyLedger(fault), r, ctx, "k.lost", "n1", "hello")
	requireCommitUncertain(t, res, err)
	if !fault.Fired() || !fault.Committed() {
		t.Fatalf("the fault did not lose a committed answer (fired %v, committed %v)", fault.Fired(), fault.Committed())
	}
	for _, e := range r.audit() {
		if e.Outcome == "rejected" {
			t.Fatalf("an uncertain commit was audited as a rejection: %+v", e)
		}
	}
	rec, err := r.reconcile(ctx, "k.lost", "n1", "hello")
	if err != nil || rec.State != kernel.ReconcileCommitted || rec.Result == nil || rec.Result.RowVersion != 1 {
		t.Fatalf("reconcile: %+v, %v; want committed", rec, err)
	}
	replay, err := r.save(ctx, "k.lost", "n1", "hello", 0)
	if err != nil || !replay.Replayed || replay.ActionID != rec.ActionID || replay.RowVersion != 1 {
		t.Fatalf("replay: %+v, %v; want the stored result of %s", replay, err, rec.ActionID)
	}
	if r.count("note") != 1 || r.count("action_result") != 1 || r.events() != 1 {
		t.Fatalf("notes %d, results %d, events %d; want the command once", r.count("note"), r.count("action_result"), r.events())
	}
	var outcomes []string
	for _, e := range r.audit() {
		outcomes = append(outcomes, e.Outcome)
	}
	if strings.Join(outcomes, ",") != "succeeded,read,replayed" {
		t.Fatalf("audit outcomes %v", outcomes)
	}
}

// Python test_t03_a23_reconciliation_access.py test_public_receipt_insert_failure_rolls_back_activation_then_same_key_recovers:
// the COMMIT never reaches the server. Reconcile reports not_committed, and the command sent again
// with the same key executes once.
func TestCommitUncertainBeforeCommitIsReconciledAndRetriedOnce(t *testing.T) {
	r := newRig(t)
	ctx := r.as(human)
	fault := &pgtest.CommitFault{}
	fault.Arm(pgtest.LoseCommit)
	res, err := saveVia(t, r.faultyLedger(fault), r, ctx, "k.lost", "n1", "hello")
	requireCommitUncertain(t, res, err)
	if !fault.Fired() {
		t.Fatal("the fault did not fire")
	}
	rec, err := r.reconcile(ctx, "k.lost", "n1", "hello")
	if err != nil || rec != (action.Reconciliation{State: kernel.ReconcileNotCommitted}) {
		t.Fatalf("reconcile: %+v, %v; want not_committed", rec, err)
	}
	if r.count("note") != 0 || r.count("action_result") != 0 || r.events() != 0 {
		t.Fatal("an uncommitted command left data behind")
	}
	retried, err := r.save(ctx, "k.lost", "n1", "hello", 0)
	if err != nil || retried.Replayed || retried.RowVersion != 1 {
		t.Fatalf("retry: %+v, %v; want a first execution", retried, err)
	}
	if rec, err = r.reconcile(ctx, "k.lost", "n1", "hello"); err != nil || rec.State != kernel.ReconcileCommitted || rec.ActionID != retried.ActionID {
		t.Fatalf("reconcile after the retry: %+v, %v", rec, err)
	}
	if r.count("note") != 1 || r.count("action_result") != 1 || r.events() != 1 {
		t.Fatalf("notes %d, results %d, events %d; want the command once", r.count("note"), r.count("action_result"), r.events())
	}
}
