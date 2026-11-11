//go:build integration

package bootstrap_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Action reconciliation and authorization recovery of TRD's commands (contract 06 动作对账 and §07;
// Python T03 A22, A23 and T04 A45; X-INV-003, TRD-INV-016) on the TRD world. Reconciling needs
// trd.read now, never the command's own permission, follows the caller's data scope for a matching
// and a conflicting request hash alike, never reads another company's ledger, executes nothing and
// is audited; a replay of the same key and a reconciliation hand a stored result only to a caller
// who still reads the EVD sources the command cited. Python's A23 receipts are orders and
// acceptances; the order commands are the order wave's, so the quote, response and acceptance
// commands stand in for them here.

// recorded is the TRD world with quote.1 activated, the customer's full acceptance recorded and
// verified, and the commands that did it, as sent.
type recorded struct {
	*trdWorld
	activate trdapi.Activate
	record   trdapi.RecordResponse
	verify   trdapi.VerifyResponse
	save     trdapi.SaveDraft
}

func newRecorded(t *testing.T) *recorded {
	q := newTrdWorld(t)
	q.mail()
	r := &recorded{trdWorld: q}
	_, d := q.approved("quote.1")
	r.activate = q.activateCmd("quote.1", d.RowVersion, 1, "approval.quote.1")
	a, err := q.c.TRD.Activate(q.author, r.activate)
	q.must(err)
	r.record = q.full("response.1", "quote.1", 1, a.BusinessHash)
	_, err = q.c.TRD.RecordResponse(q.author, r.record)
	q.must(err)
	r.verify = q.verification("response.1", "acceptance.1", 1, a.BusinessHash)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, r.verify)
	q.must(err)
	r.save = trdapi.SaveDraft{IdempotencyKey: q.key(), QuoteID: "quote.metadata", Data: q.data()}
	_, err = q.c.TRD.SaveDraft(q.author, r.save)
	q.must(err)
	return r
}

// commands are the recorded commands with, for each, a request of the same key and another hash.
func (r *recorded) commands() map[string][2]trdapi.Command {
	activate, record, verify, save := r.activate, r.record, r.verify, r.save
	activate.ApprovalID = "approval.other"
	record.Summary = "另一份回复"
	verify.LineIDs = []string{"L2"}
	save.Data.ExpiresAt = kernel.FormatTimestamp(time.Now().Add(96 * time.Hour))
	return map[string][2]trdapi.Command{
		trdapi.CommandActivate:       {r.activate, activate},
		trdapi.CommandRecordResponse: {r.record, record},
		trdapi.CommandVerifyResponse: {r.verify, verify},
		trdapi.CommandSaveDraft:      {r.save, save},
	}
}

// reconciled reconciles cmd as ctx and returns the state, or the refusal's code.
func (r *recorded) reconciled(ctx context.Context, cmd trdapi.Command) string {
	r.t.Helper()
	rec, err := r.c.TRD.Reconcile(ctx, cmd)
	if err != nil {
		return code(err)
	}
	return string(rec.State)
}

// Python test_t03_a23_reconciliation_access.py
// test_business_read_permission_is_required_for_order_and_acceptance_receipts (PYT-0174),
// test_readers_can_recover_receipts_without_write_grants (PYT-0175),
// test_own_team_company_scope_applies_to_committed_and_conflicting_receipts (PYT-0176),
// test_revoked_grants_and_team_changes_apply_after_reopening (PYT-0177),
// test_company_and_tenant_boundaries_do_not_reveal_another_ledger (PYT-0178),
// test_unknown_command_is_rejected_by_both_request_schemas (PYT-0179) and
// test_quote_metadata_receipts_keep_quote_read_scope_without_business_write_grants (PYT-0187);
// test_t04_a45 test_revoked_write_grants_keep_read_only_recovery_available (PYT-0554, the
// reconciliation half); X-INV-003: a caller without trd.read is PERMISSION_DENIED for both hashes;
// a reader holding trd.read and evd.read but no write grant recovers each receipt (committed, the
// stored receipt) and learns of a conflict (payload_conflict, no receipt); the owner, a teammate
// and a company reader see both, an own-scope outsider and another team get NOT_FOUND for both; a
// grant revoked or a team changed applies at the next reconciliation; another company or tenant
// reads its own ledger only (not_committed, nothing revealed); a command TRD does not know is
// SCHEMA_INVALID.
func TestReconciliationReadsWithTheCurrentGrantAndScope(t *testing.T) {
	r := newRecorded(t)
	blind := r.regrant("blind", "company", without(allPermissions, "trd.read"))
	reader := r.regrant("reader", "company", []string{"trd.read", "evd.read"})
	for name, pair := range r.commands() {
		for i, cmd := range pair {
			if got := r.reconciled(blind, cmd); got != kernel.CodePermissionDenied {
				t.Errorf("%s (hash %d) without trd.read: %s", name, i, got)
			}
		}
		rec, err := r.c.TRD.Reconcile(reader, pair[0])
		if err != nil || rec.State != kernel.ReconcileCommitted || rec.Receipt == nil || rec.Receipt.ActionID != rec.ActionID || rec.ActionID == "" {
			t.Errorf("%s: a reader recovers %+v %v", name, rec, err)
		}
		rec, err = r.c.TRD.Reconcile(reader, pair[1])
		if err != nil || rec.State != kernel.ReconcilePayloadConflict || rec.Receipt != nil || rec.ActionID == "" {
			t.Errorf("%s: a conflict %+v %v", name, rec, err)
		}
	}

	scoped := func(id, dataScope, team string) context.Context {
		return r.provision(bootstrap.Grant{Scope: r.scope("company.A"), Actor: actorRef("human", id), Permissions: []string{"trd.read", "evd.read"},
			DataScope: authorizationScope(dataScope), Team: team})
	}
	allowed := map[string]context.Context{"a teammate": scoped("mate", "team", "trade"), "a company reader": scoped("company.reader", "company", "trade")}
	denied := map[string]context.Context{"an own-scope outsider": scoped("outsider", "own", "trade"), "another team": scoped("other.team", "team", "other")}
	for name, pair := range r.commands() {
		for i, cmd := range pair {
			for who, ctx := range allowed {
				if got := r.reconciled(ctx, cmd); got != string(kernel.ReconcileCommitted) && got != string(kernel.ReconcilePayloadConflict) {
					t.Errorf("%s (hash %d), %s: %s", name, i, who, got)
				}
			}
			for who, ctx := range denied {
				if got := r.reconciled(ctx, cmd); got != kernel.CodeNotFound {
					t.Errorf("%s (hash %d), %s: %s", name, i, who, got)
				}
			}
		}
	}

	// The grant changes apply at once.
	revocable := scoped("revocable", "team", "trade")
	if got := r.reconciled(revocable, r.activate); got != string(kernel.ReconcileCommitted) {
		t.Fatalf("before the change: %s", got)
	}
	revocable = r.provision(bootstrap.Grant{Scope: r.scope("company.A"), Actor: actorRef("human", "revocable"), Permissions: []string{"evd.read"},
		DataScope: "team", Team: "trade"})
	if got := r.reconciled(revocable, r.activate); got != kernel.CodePermissionDenied {
		t.Fatalf("trd.read revoked: %s", got)
	}
	revocable = scoped("revocable", "team", "other")
	if got := r.reconciled(revocable, r.commands()[trdapi.CommandActivate][1]); got != kernel.CodeNotFound {
		t.Fatalf("moved to another team: %s", got)
	}

	// Another company, another tenant: their own ledgers, where nothing was committed.
	other := pgtest.UniqueID(t, "tenant.")
	for name, s := range map[string]kernel.Scope{"another company": r.scope("company.B"),
		"another tenant": {Tenant: kernel.TenantID(other), Company: "company.A"}} {
		ctx := r.provision(bootstrap.Grant{Scope: s, Actor: actorRef("human", "editor"), Permissions: allPermissions, DataScope: "company"})
		for _, pair := range r.commands() {
			for _, cmd := range pair {
				rec, err := r.c.TRD.Reconcile(ctx, cmd)
				if err != nil || rec.State != kernel.ReconcileNotCommitted || rec.ActionID != "" || rec.ObjectID != "" || rec.Receipt != nil {
					t.Errorf("%s: %+v %v", name, rec, err)
				}
			}
		}
	}
	if _, err := r.c.TRD.Reconcile(r.author, nil); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("an unknown command: %s", code(err))
	}
}

// Python test_t03_a23_reconciliation_access.py test_reconciliation_is_observational_and_audited
// (PYT-0182), test_commit_then_process_exit_recovers_public_action_and_replays_once (PYT-0183),
// test_public_receipt_insert_failure_rolls_back_activation_then_same_key_recovers (PYT-0184),
// test_bound_root_allows_observation_but_blocks_another_write_replay (PYT-0186) and
// test_t04_a45 test_task_control_blocks_writes_but_allows_historical_observation (PYT-0539),
// TRD-INV-016: reconciling changes nothing and is audited (action.reconcile, read, the object);
// a failure storing the action result rolls the activation back whole and reconciles as
// not_committed, and the same key then activates; a command committed by one process is
// reconciled as committed by a fresh one (a new connection pool) and replays once with its action
// ID; while a task holds the root, reconciling and reading still answer and the replay of a write
// does not.
func TestReconciliationObservesRecoversAndStaysAvailable(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	_, d := q.approved("quote.1")
	activate := q.activateCmd("quote.1", d.RowVersion, 1, "approval.quote.1")

	drop := q.failInsert("brant_trd.action_result")
	_, err := q.c.TRD.Activate(q.author, activate)
	if _, isRejection := kernel.AsRejection(err); err == nil || isRejection || !strings.Contains(err.Error(), "test storage failure") {
		t.Fatalf("the action result failing: %v", err)
	}
	if rec, err := q.c.TRD.Reconcile(q.author, activate); err != nil || rec.State != kernel.ReconcileNotCommitted {
		t.Fatalf("after the rollback %+v %v", rec, err)
	}
	if documents, commits, _ := q.formalRows(); documents != 0 || commits != 0 || len(q.eventsOf(trdapi.EventQuoteActivated)) != 0 {
		t.Fatalf("the rolled back activation left %d documents, %d commit evidence", documents, commits)
	}
	drop()
	a, err := q.c.TRD.Activate(q.author, activate)
	if err != nil || a.Replayed {
		t.Fatalf("the same key after the failure %+v %v", a, err)
	}

	// A fresh process: a new platform on its own pool.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fresh, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: pgtest.AppDSN(t)})
	q.must(err)
	defer fresh.Close()
	c, err := fresh.Contexts()
	q.must(err)
	rec, err := c.TRD.Reconcile(q.author, activate)
	if err != nil || rec.State != kernel.ReconcileCommitted || rec.ActionID != a.ActionID || rec.ObjectID != "quote.1" {
		t.Fatalf("a fresh process reconciles %+v %v", rec, err)
	}
	again, err := c.TRD.Activate(q.author, activate)
	if err != nil || !again.Replayed || again.ActionID != a.ActionID || len(q.eventsOf(trdapi.EventQuoteActivated)) != 1 {
		t.Fatalf("the replay %+v %v", again, err)
	}

	// Observational and audited.
	record := q.full("response.1", "quote.1", 1, a.BusinessHash)
	_, err = q.c.TRD.RecordResponse(q.author, record)
	q.must(err)
	conflict := record
	conflict.Summary = "另一份回复"
	before := q.business()
	entries, err := q.c.TRD.Audit(q.actorOf(kernel.ActorHuman, "auditor"))
	q.must(err)
	for _, cmd := range []trdapi.Command{activate, record, conflict} {
		if _, err = q.c.TRD.Reconcile(q.author, cmd); err != nil {
			t.Fatal(err)
		}
	}
	if q.business() != before {
		t.Fatal("reconciling changed the business state")
	}
	after, err := q.c.TRD.Audit(q.actorOf(kernel.ActorHuman, "auditor"))
	q.must(err)
	var reads []string
	for _, e := range after[len(entries):] {
		if e.Command == "action.reconcile" {
			reads = append(reads, e.Outcome+":"+e.ObjectID)
		}
	}
	if strings.Join(reads, ",") != "read:quote.1,read:response.1,read:response.1" {
		t.Fatalf("the reconciliations' audit %v", reads)
	}

	// A task holds the root.
	bound, err := q.c.TRD.BindControl(q.author, trdapi.BindControl{IdempotencyKey: q.key(), QuoteID: "quote.1", TaskID: "task.a23",
		ExpectedControlEpoch: 0})
	q.must(err)
	for name, cmd := range map[string]trdapi.Command{"activate": activate, "record": record} {
		if rec, err := q.c.TRD.Reconcile(q.author, cmd); err != nil || rec.State != kernel.ReconcileCommitted {
			t.Errorf("%s: reconciling a bound root %+v %v", name, rec, err)
		}
	}
	if rec, err := q.c.TRD.Reconcile(q.author, conflict); err != nil || rec.State != kernel.ReconcilePayloadConflict {
		t.Fatalf("a conflict on a bound root %+v %v", rec, err)
	}
	if _, err = q.c.TRD.GetCustomerResponse(q.author, "response.1"); err != nil {
		t.Fatalf("reading under a binding: %v", err)
	}
	if _, err = q.c.TRD.Activate(q.author, activate); code(err) != trdapi.CodeObjectControlConflict {
		t.Fatalf("the activation's replay under a binding: %s", code(err))
	}
	if _, err = q.c.TRD.RecordResponse(q.author, record); code(err) != trdapi.CodeObjectControlConflict {
		t.Fatalf("the record's replay under a binding: %s", code(err))
	}
	claimed, err := trdapi.WithControl(q.author, trdapi.ControlClaim{TaskID: "task.a23", ControlEpoch: bound.RowVersion})
	q.must(err)
	if again, err = q.c.TRD.Activate(claimed, activate); err != nil || !again.Replayed {
		t.Fatalf("the task's own replay %+v %v", again, err)
	}
	if n := q.count("company.A", "brant_trd.quote_document"); n != 1 {
		t.Fatalf("%d formal documents", n)
	}
}

// Python test_t04_a45_response_query_recovery.py test_source_revocation_blocks_query_both_replays_and_both_hashes
// (PYT-0537) and test_source_scope_change_is_rechecked_independently_of_quote (PYT-0538), and
// contract 06 §07 (来源权限撤回后，查询、同键写重放与对账均不能返回成功事实) for every TRD command
// that cites EVD sources; Context Map R16: the recorder cites a message of another team, the
// author withdraws a frozen quote and discards a draft on that evidence. Once the author's data
// scope no longer shows that source (it still sees the quotes), the read of the response, the
// replays of the record, the withdrawal and the discard, and their reconciliations with a matching
// and a conflicting hash are REFERENCE_INVALID, and so are the verifier's replay and
// reconciliations of the verification; without evd.read they are PERMISSION_DENIED, as the read is.
// Nothing is written. With the source readable again, every replay and reconciliation answers.
func TestSourceRevocationBlocksReplaysAndReconciliation(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	outsider := q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: actorRef("human", "outsider"), Permissions: allPermissions,
		DataScope: "team", Team: "other"})
	q.source(outsider, "foreign", "其他团队登记的客户邮件")
	foreign := []trdapi.Evidence{{SourceID: "foreign", Locator: "附件"}}
	a := q.activated("quote.1")
	record := q.full("response.1", "quote.1", 1, a.BusinessHash)
	record.Evidence = foreign
	_, err := q.c.TRD.RecordResponse(q.author, record)
	q.must(err)
	verify := q.verification("response.1", "acceptance.1", 1, a.BusinessHash)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, verify)
	q.must(err)
	f := q.frozenQuote("quote.2")
	withdraw := trdapi.Withdraw{IdempotencyKey: q.key(), QuoteID: "quote.2", ExpectedVersion: f.RowVersion, BusinessRevision: 1,
		Reason: "客户撤回询价", Evidence: foreign}
	_, err = q.c.TRD.Withdraw(q.author, withdraw)
	q.must(err)
	q.drafted("quote.3")
	discard := trdapi.DiscardDraft{IdempotencyKey: q.key(), QuoteID: "quote.3", ExpectedVersion: 1, WorkingRevision: 1, Reason: "重复草稿",
		Evidence: foreign}
	_, err = q.c.TRD.DiscardDraft(q.author, discard)
	q.must(err)

	conflicting := func(cmd trdapi.Command) trdapi.Command {
		switch c := cmd.(type) {
		case trdapi.RecordResponse:
			c.Summary = "另一份回复"
			return c
		case trdapi.VerifyResponse:
			c.LineIDs = []string{"L2"}
			return c
		case trdapi.Withdraw:
			c.Reason = "另一理由"
			return c
		case trdapi.DiscardDraft:
			c.Reason = "另一理由"
			return c
		}
		return cmd
	}
	replay := func(ctx context.Context, cmd trdapi.Command) error {
		switch c := cmd.(type) {
		case trdapi.RecordResponse:
			_, err := q.c.TRD.RecordResponse(ctx, c)
			return err
		case trdapi.VerifyResponse:
			_, err := q.c.TRD.VerifyResponse(ctx, c)
			return err
		case trdapi.Withdraw:
			_, err := q.c.TRD.Withdraw(ctx, c)
			return err
		case trdapi.DiscardDraft:
			_, err := q.c.TRD.DiscardDraft(ctx, c)
			return err
		}
		return nil
	}
	// expect checks, for the author's commands and the reviewer's verification, the read, both
	// replays (the stored key, and the key under the caller's current grant) and both hashes.
	expect := func(phase string, author, reviewer context.Context, want string) {
		t.Helper()
		got := func(err error) string {
			if want == "OK" && err == nil {
				return "OK"
			}
			return code(err)
		}
		_, err := q.c.TRD.GetCustomerResponse(author, "response.1")
		if g := got(err); g != want {
			t.Errorf("%s: the read: %s, want %s", phase, g, want)
		}
		for ctx, cmds := range map[context.Context][]trdapi.Command{author: {record, withdraw, discard}, reviewer: {verify}} {
			for _, cmd := range cmds {
				if g := got(replay(ctx, cmd)); g != want {
					t.Errorf("%s: the replay of %T: %s, want %s", phase, cmd, g, want)
				}
				for i, c := range []trdapi.Command{cmd, conflicting(cmd)} {
					_, err := q.c.TRD.Reconcile(ctx, c)
					if g := got(err); g != want {
						t.Errorf("%s: reconciling %T (hash %d): %s, want %s", phase, cmd, i, g, want)
					}
				}
			}
		}
	}
	before := q.business()
	teamOnly := func(id string, permissions []string) context.Context {
		return q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: actorRef("human", id), Permissions: permissions, DataScope: "team",
			Team: "trade"})
	}
	expect("the source out of the data scope", teamOnly("editor", allPermissions), teamOnly("reviewer", allPermissions), kernel.CodeReferenceInvalid)
	noEVD := without(allPermissions, "evd.read")
	expect("without evd.read", q.regrant("editor", "company", noEVD), q.regrant("reviewer", "company", noEVD), kernel.CodePermissionDenied)
	if q.business() != before {
		t.Fatal("the refused replays and reconciliations wrote business state")
	}
	expect("the source readable again", q.regrant("editor", "company", allPermissions), q.regrant("reviewer", "company", allPermissions), "OK")

	// PYT-0538: a reader who sees the quote but not the source.
	reader := teamOnly("source.reader", []string{"trd.read", "evd.read"})
	if _, err = q.c.TRD.GetCustomerResponse(reader, "response.1"); code(err) != kernel.CodeReferenceInvalid {
		t.Fatalf("a reader without the source: %s", code(err))
	}
	if _, err = q.c.TRD.Reconcile(reader, record); code(err) != kernel.CodeReferenceInvalid {
		t.Fatalf("a reader without the source reconciles: %s", code(err))
	}
	if _, err = q.c.TRD.GetQuote(reader, "quote.1"); err != nil {
		t.Fatalf("the reader sees the quote: %v", err)
	}
}

// Python test_t04_a45 test_receipt_wrong_command_and_action_id_fail_closed (PYT-0541),
// test_receipt_cannot_be_swapped_to_other_valid_response (PYT-0543) and
// test_reconciliation_recomputes_ledger_request_binding (PYT-0553), and test_t03_a23
// test_invalid_stored_receipts_fail_closed_instead_of_appearing_committed (PYT-0181), in Go's
// terms: TRD keeps no receipt JSON (a receipt is rebuilt from the aggregate it names) and the
// application role may only insert into the command ledger, so neither a stored result nor its
// request hash can be swapped or edited through the application; the database refuses it.
func TestTheCommandLedgerCannotBeRewritten(t *testing.T) {
	q := newTrdWorld(t)
	q.drafted("quote.1")
	for name, sql := range map[string]string{
		"another object":       `UPDATE brant_trd.action_result SET object_id = 'quote.other'`,
		"another request hash": `UPDATE brant_trd.action_result SET request_hash = repeat('f', 64)`,
		"another action":       `UPDATE brant_trd.action_result SET action_id = 'act.other'`,
		"a deletion":           `DELETE FROM brant_trd.action_result`,
	} {
		if err := q.exec("company.A", sql); err == nil || !strings.Contains(err.Error(), "42501") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
