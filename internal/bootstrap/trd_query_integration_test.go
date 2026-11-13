//go:build integration

package bootstrap_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
)

// The read side of TRD (WBS S3-09; X-INV-003) on the TRD world: "editor" (the author) drafts,
// activates, replaces and records; "reviewer" approves and verifies; both humans with every
// permission and company data scope in team trade unless a test provisions them otherwise.
// Readiness, histories and dispatch are audited reads under trd.read; reconciliation and same-key
// replays re-check the caller's current grant, data scope and EVD sources. Test names cite the
// Python V0.50 tests they port (T03 A8–A16, A22, A23, A34, A35; T04 A45).

func (q *trdWorld) readiness(ctx context.Context, id string, rev int64) trdapi.QuoteReadiness {
	q.t.Helper()
	r, err := q.c.TRD.QuoteReady(ctx, trdapi.ReadyQuery{QuoteID: id, BusinessRevision: rev})
	q.must(err)
	return r
}

// reasonsOf are the reasons as check:code.
func reasonsOf(rs []trdapi.Reason) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Check+":"+r.Code)
	}
	return out
}

// scalar runs a query of one text value as the application role in company.A's scope.
func (q *trdWorld) scalar(sql string) string {
	q.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(q.t))
	q.must(err)
	var v string
	q.must(bento.ScopeTransactor{DB: db, Scope: q.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return exec.QueryRow(ctx, sql).Scan(&v)
	}))
	return v
}

// business is TRD's business state in company.A: every table but the audit trail, the root
// versions and control epochs, the acceptances' effectivity, and TRD's Outbox events. A read
// leaves it as it was.
func (q *trdWorld) business() string {
	var parts []string
	for _, table := range []string{"quote_root", "quote_revision", "quote_review", "quote_approval", "quote_margin_exception", "quote_document",
		"quote_commit_evidence", "quote_revision_closure", "quote_risk_hold", "quote_conversion_slot", "customer_response", "acceptance",
		"acceptance_effectivity", "action_result"} {
		parts = append(parts, fmt.Sprintf("%s=%d", table, q.count("company.A", "brant_trd."+table)))
	}
	parts = append(parts, q.scalar(`SELECT coalesce(string_agg(quote_id || ':' || root_version || ':' || control_epoch, ',' ORDER BY quote_id), '')
		FROM brant_trd.quote_root`))
	parts = append(parts, q.scalar(`SELECT coalesce(string_agg(revision::text || status, ',' ORDER BY quote_id, revision), '') FROM brant_trd.quote_revision`))
	parts = append(parts, fmt.Sprintf("events=%d", len(q.events("company.A", trdapi.Source))))
	return strings.Join(parts, " ")
}

// audited reports whether TRD's audit trail has an entry of command on object with outcome (and
// code, for a rejection).
func (q *trdWorld) audited(command, object, outcome, code string) bool {
	q.t.Helper()
	entries, err := q.c.TRD.Audit(q.actorOf(kernel.ActorHuman, "auditor"))
	q.must(err)
	return slices.ContainsFunc(entries, func(e kernel.AuditEntry) bool {
		return e.Command == command && e.ObjectID == object && e.Outcome == outcome && e.Code == code
	})
}

// Python test_t03_query_contract.py test_quote_ready_query_maps_internal_reasons_without_leaking_payload
// (PYT-0426) and test_quote_root_query_is_closed_and_read_authorized (PYT-0425),
// test_t03_cost_binding.py test_activate_rechecks_and_records_current_snapshot_qualification
// (PYT-0410, the quote_ready half) and test_current_cost_denial_makes_quote_ready_false_without_rewriting_history
// (PYT-0411), test_t03_a24 (object control at quote_ready); T03 A8, TRD-INV-006, X-INV-003: a
// quote without an active revision is not ready (NO_ACTIVE_REVISION); a missing quote and another
// company's are NOT_FOUND, a reader without trd.read PERMISSION_DENIED, a malformed query
// SCHEMA_INVALID, all audited; the activated quote is ready for its author and for a reader holding
// trd.read alone, with CST's current qualification of its snapshot; a revision named that is not the
// active one is QUOTE_NOT_CURRENT; a bound root is not ready for a caller without the task's claim;
// a source denial in CST makes it not ready (COST_SNAPSHOT_INELIGIBLE) without rewriting its
// history; and no read changes TRD's business state. Also test_t03_server_calculation.py
// test_quote_get_returns_history_and_current_cost_qualification (PYT-0444): Go splits Python's
// quote_get into GetQuote (the revisions' history, for the reviewer as for the author) and
// quote_ready (CST's current qualification of the active revision's snapshot); a reader without
// trd.read gets neither (PERMISSION_DENIED).
func TestQuoteReadyReflectsTheActiveRevisionNow(t *testing.T) {
	q := newTrdWorld(t)
	q.drafted("quote.draft")
	r := q.readiness(q.author, "quote.draft", 0)
	if r.Ready || !slices.Equal(reasonsOf(r.Reasons), []string{"revision:NO_ACTIVE_REVISION"}) || r.BusinessRevision != 0 || r.RootVersion != 1 {
		t.Fatalf("a draft: %+v", r)
	}
	blind := q.regrant("blind", "company", without(allPermissions, "trd.read"))
	foreigner := q.provision(bootstrap.Grant{Scope: q.scope("company.B"), Actor: actorRef("human", "foreigner"), Permissions: allPermissions,
		DataScope: "company"})
	for name, c := range map[string]struct {
		ctx  context.Context
		id   string
		want string
	}{
		"a missing quote":       {q.author, "quote.missing", kernel.CodeNotFound},
		"a reader without read": {blind, "quote.draft", kernel.CodePermissionDenied},
		"another company":       {foreigner, "quote.draft", kernel.CodeNotFound},
		"a malformed quote ID":  {q.author, "quote draft", kernel.CodeSchemaInvalid},
	} {
		if _, err := q.c.TRD.QuoteReady(c.ctx, trdapi.ReadyQuery{QuoteID: c.id}); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
	if !q.audited(trdapi.QueryQuoteReady, "quote.draft", "read", "") || !q.audited(trdapi.QueryQuoteReady, "quote.missing", "rejected",
		kernel.CodeNotFound) || !q.audited(trdapi.QueryQuoteReady, "quote.draft", "rejected", kernel.CodePermissionDenied) {
		t.Fatal("the reads are not audited")
	}

	a := q.activated("quote.1")
	if v, err := q.c.TRD.GetQuote(q.reviewer, "quote.1"); err != nil || len(v.Revisions) != 1 || v.Revisions[0].Status != trdapi.StatusActive ||
		v.Revisions[0].BusinessHash != a.BusinessHash {
		t.Fatalf("PYT-0444: the history for the reviewer %+v, %v", v, err)
	}
	if _, err := q.c.TRD.GetQuote(blind, "quote.1"); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("PYT-0444: the history for a reader without trd.read: %v", err)
	}
	before := q.business()
	reader := q.regrant("reader", "company", []string{"trd.read"})
	for name, ctx := range map[string]context.Context{"the author": q.author, "a reader of TRD alone": reader} {
		r = q.readiness(ctx, "quote.1", 0)
		c := r.CostSnapshot
		if !r.Ready || len(r.Reasons) != 0 || r.BusinessRevision != 1 || r.BusinessHash != a.BusinessHash || r.Status != trdapi.StatusActive ||
			r.ApprovalID != "approval.quote.1" || r.ControlState != "unbound" || r.RootVersion != a.RowVersion || c == nil ||
			c.SnapshotID != q.snap.SnapshotID || c.ResultHash != q.snap.ResultHash || !c.Adoptable || r.CheckedAt.IsZero() {
			t.Fatalf("%s: the activated quote %+v %+v", name, r, c)
		}
	}
	if r = q.readiness(q.author, "quote.1", 1); !r.Ready {
		t.Fatalf("the active revision named: %+v", r)
	}
	if _, err := q.c.TRD.QuoteReady(q.author, trdapi.ReadyQuery{QuoteID: "quote.1", BusinessRevision: 9}); code(err) != kernel.CodeNotFound {
		t.Fatalf("a revision the root does not have: %s", code(err))
	}
	if q.business() != before {
		t.Fatalf("readiness changed the business state:\n%s\n%s", before, q.business())
	}

	// A revision named that is not the active one (a TSK goal set for revision 1, after revision 2
	// was frozen): QUOTE_NOT_CURRENT, and it has no commit evidence.
	_, err := q.c.TRD.Revise(q.author, trdapi.Revise{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: a.RowVersion, BaseRevision: 1})
	q.must(err)
	f, err := q.freezeQuote(q.author, "quote.1", a.RowVersion+1, 2)
	q.must(err)
	if r = q.readiness(q.author, "quote.1", 2); r.Ready || !slices.Equal(reasonsOf(r.Reasons),
		[]string{"revision:QUOTE_NOT_CURRENT", "commit_evidence:COMMIT_EVIDENCE_MISSING"}) {
		t.Fatalf("a frozen revision named: %+v", r)
	}

	// Object control: a root bound to a task is ready for the task's claim only.
	bound, err := q.bindTask(q.author, "quote.1", "task.goal", 0)
	q.must(err)
	if r = q.readiness(q.author, "quote.1", 0); r.Ready || !slices.Equal(reasonsOf(r.Reasons), []string{"object_control:OBJECT_CONTROL_CONFLICT"}) ||
		r.ControlState != "bound" {
		t.Fatalf("bound, without the claim: %+v", r)
	}
	claimed, err := trdapi.WithControl(q.author, trdapi.ControlClaim{TaskID: "task.goal", ControlEpoch: bound.RowVersion})
	q.must(err)
	if r = q.readiness(claimed, "quote.1", 0); !r.Ready {
		t.Fatalf("bound, with the task's claim: %+v", r)
	}
	_, err = q.c.TRD.ReleaseControl(claimed, trdapi.ReleaseControl{IdempotencyKey: q.key(), QuoteID: "quote.1",
		ExpectedControlEpoch: bound.RowVersion, Reason: "目标核验完成"})
	q.must(err)

	// PYT-0411: a denial of the snapshot's source makes the quote not ready; its history stays.
	_, err = q.c.CST.AttestSource(q.reviewer, q.denyCmd("src.1", q.counter()))
	q.must(err)
	r = q.readiness(q.reviewer, "quote.1", 0)
	if r.Ready || !slices.Contains(reasonsOf(r.Reasons), "upstream:COST_SNAPSHOT_INELIGIBLE") || r.CostSnapshot == nil || r.CostSnapshot.Adoptable {
		t.Fatalf("after the denial: %+v %+v", r, r.CostSnapshot)
	}
	v := q.getQuote("quote.1")
	if v.ActiveRevision != 1 || v.Revisions[0].BusinessHash != a.BusinessHash || v.Revisions[1].BusinessHash != f.BusinessHash {
		t.Fatalf("the history was rewritten: %+v", v.Revisions[0])
	}
}

// Python test_t03_a34_ready_reviewer.py test_current_reviewer_qualification_is_reflected_without_business_writes
// (PYT-0392) and test_another_approval_cannot_replace_formalization_approval (PYT-0393),
// TRD-INV-003, 004: readiness follows the qualification of the reviewer of the approval the commit
// evidence names, read now — a reviewer who lost quote.review.decide or trd.read, is no longer a
// human or no longer sees the quote makes it not ready (APPROVAL_STALE) without any business write,
// and restoring the grant makes it ready again; after a replacement approved by another reviewer,
// that reviewer's loss makes the quote not ready although revision 1's approval and reviewer are
// still good; revoking the replacement's approval holds the revision (APPROVAL_REVOKED twice:
// the risk hold and the approval).
func TestQuoteReadyFollowsTheCommittedApprovalNow(t *testing.T) {
	q := newTrdWorld(t)
	q.activated("quote.1")
	before := q.business()
	for name, g := range map[string]bootstrap.Grant{
		"quote.review.decide revoked": grantOf(q, "reviewer", kernel.ActorHuman, "company", without(allPermissions, "quote.review.decide")...),
		"trd.read revoked":            grantOf(q, "reviewer", kernel.ActorHuman, "company", without(allPermissions, "trd.read")...),
		"an AI agent now":             grantOf(q, "reviewer", kernel.ActorAI, "company", allPermissions...),
		"own data scope":              grantOf(q, "reviewer", kernel.ActorHuman, "own", allPermissions...),
	} {
		q.provision(g)
		r := q.readiness(q.author, "quote.1", 0)
		if r.Ready || !slices.Equal(reasonsOf(r.Reasons), []string{"approval:APPROVAL_STALE"}) {
			t.Errorf("%s: %+v", name, r)
		}
	}
	q.regrant("reviewer", "company", allPermissions)
	if r := q.readiness(q.author, "quote.1", 0); !r.Ready {
		t.Fatalf("restored: %+v", r)
	}
	if q.business() != before {
		t.Fatal("readiness wrote business state")
	}

	// PYT-0393: revision 2 replaces revision 1 on another reviewer's approval.
	second := q.actorOf(kernel.ActorHuman, "reviewer.2")
	v := q.getQuote("quote.1")
	rv, err := q.c.TRD.Revise(q.author, trdapi.Revise{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: v.RootVersion, BaseRevision: 1})
	q.must(err)
	f, err := q.freezeQuote(q.author, "quote.1", rv.RowVersion, 2)
	q.must(err)
	_, err = q.c.TRD.SubmitReview(q.author, trdapi.SubmitReview{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: f.RowVersion,
		BusinessRevision: 2, ReviewRequestID: "review.2", TargetCommand: trdapi.TargetReplace, ExpectedActiveRevision: 1})
	q.must(err)
	d, err := q.c.TRD.DecideReview(second, q.approval("quote.1", f.RowVersion+1, "review.2", f.BusinessHash, "approval.2"))
	q.must(err)
	rp, err := q.c.TRD.Replace(q.author, q.replaceCmd("quote.1", d.RowVersion, 2, "approval.2"))
	q.must(err)
	if r := q.readiness(q.author, "quote.1", 0); !r.Ready || r.BusinessRevision != 2 || r.ApprovalID != "approval.2" {
		t.Fatalf("after the replacement: %+v", r)
	}
	q.provision(grantOf(q, "reviewer.2", kernel.ActorHuman, "company", without(allPermissions, "quote.review.decide")...))
	if r := q.readiness(q.author, "quote.1", 0); r.Ready || !slices.Equal(reasonsOf(r.Reasons), []string{"approval:APPROVAL_STALE"}) {
		t.Fatalf("revision 1's approval stands in for the replacement's: %+v", r)
	}
	if a := q.getQuote("quote.1").Approvals[0]; a.ApprovalID != "approval.quote.1" || a.Effectivity != "valid" || a.Reviewer != actorRef("human", "reviewer") {
		t.Fatalf("revision 1's approval %+v", a)
	}
	q.provision(grantOf(q, "reviewer.2", kernel.ActorHuman, "company", allPermissions...))
	_, err = q.c.TRD.RevokeApproval(second, trdapi.RevokeApproval{IdempotencyKey: q.key(), QuoteID: "quote.1", ApprovalID: "approval.2",
		ExpectedEffectivityVersion: 1, Reason: "客户信用恶化", Evidence: q.proof})
	q.must(err)
	if r := q.readiness(q.author, "quote.1", 0); r.Ready || !slices.Equal(reasonsOf(r.Reasons),
		[]string{"risk_hold:APPROVAL_REVOKED", "approval:APPROVAL_REVOKED"}) || r.RootVersion != rp.RowVersion+1 {
		t.Fatalf("held: %+v", r)
	}
}

// Python test_t03_a35_time_qualification.py test_ready_compares_quote_and_approval_offsets_as_instants
// (PYT-0397), TRD-INV-005: readiness compares instants — before the expiry the quote is ready; once
// it expired, readiness reports the quote's expiry. Python also reports the approval's; an active
// revision's approval is spent (owner decision 2026-11-11).
func TestQuoteReadyComparesInstants(t *testing.T) {
	q := newTrdWorld(t)
	_, expires := q.shortlyActivated("quote.1", 10*time.Second)
	if r := q.readiness(q.author, "quote.1", 0); !r.Ready || !r.ExpiresAt.Equal(expires) {
		t.Fatalf("before the expiry: %+v", r)
	}
	time.Sleep(time.Until(expires) + 100*time.Millisecond)
	r := q.readiness(q.author, "quote.1", 0)
	// The approval lapsed with the quote, but it is spent once the revision is active (owner
	// decision 2026-11-11; Python also reports APPROVAL_EXPIRED).
	if r.Ready || !slices.Equal(reasonsOf(r.Reasons), []string{"validity:QUOTE_EXPIRED"}) {
		t.Fatalf("after the expiry: %+v", r)
	}
}

// Python test_t03_query_contract.py test_quote_history_returns_compact_revision_metadata_without_raw_data
// (PYT-0428), test_quote_history_requires_a_specific_revision_and_current_dependencies (PYT-0429),
// test_approval_history_is_payload_free_and_tracks_effectivity_events (PYT-0435),
// test_document_history_returns_binding_metadata_and_integrity_without_content (PYT-0427),
// test_commit_history_returns_server_evidence_without_result_payload (PYT-0431) and
// test_outbox_history_reports_queued_metadata_without_dispatch_claim (PYT-0430); T03 A9–A13: a quote
// activated, replaced and answered by the customer; each history is ordered, paged, payload-free and
// scoped: the revisions with their steps and amounts, the reviews with their approvals'
// effectivity (approved, then revoked at version 2), the formal documents intact, the commit
// evidence naming the actions that committed it, and the Outbox events of the root, its response
// and its acceptance in enqueue order with the relay's state and no customer delivery. A reader
// without trd.read is PERMISSION_DENIED, another company NOT_FOUND, a bad page SCHEMA_INVALID.
func TestQuoteHistoriesArePagedOrderedAndPayloadFree(t *testing.T) {
	q := newTrdWorld(t)
	q.mail()
	act := q.activated("quote.1")
	n, hash, version := q.revisedAndApproved("quote.1", act.RowVersion, trdapi.TargetReplace, "review.2", "approval.2")
	rep, err := q.c.TRD.Replace(q.author, q.replaceCmd("quote.1", version, n, "approval.2"))
	q.must(err)
	_, err = q.c.TRD.RevokeApproval(q.reviewer, trdapi.RevokeApproval{IdempotencyKey: q.key(), QuoteID: "quote.1", ApprovalID: "approval.quote.1",
		ExpectedEffectivityVersion: 1, Reason: "已被替代", Evidence: q.proof})
	q.must(err)
	_, err = q.c.TRD.RecordResponse(q.author, q.full("response.1", "quote.1", n, hash))
	q.must(err)
	_, err = q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.1", "acceptance.1", n, hash))
	q.must(err)
	before := q.business()

	// Revisions, one a page.
	p1, err := q.c.TRD.QuoteHistory(q.author, trdapi.HistoryQuery{QuoteID: "quote.1", Page: trdapi.Page{Limit: 1}})
	q.must(err)
	p2, err := q.c.TRD.QuoteHistory(q.author, trdapi.HistoryQuery{QuoteID: "quote.1", Page: trdapi.Page{After: p1.Next, Limit: 1}})
	q.must(err)
	if len(p1.Revisions) != 1 || p1.Next != 1 || len(p2.Revisions) != 1 || p2.Next != 0 || p2.ActiveRevision != 2 || p2.LastRevision != 2 ||
		p2.WorkingRevision != 0 {
		t.Fatalf("pages %+v %+v", p1, p2)
	}
	events := func(r trdapi.RevisionHistory) []string {
		var out []string
		for _, e := range r.Events {
			out = append(out, e.Event)
		}
		return out
	}
	r1, r2 := p1.Revisions[0], p2.Revisions[0]
	if r1.Status != trdapi.StatusSuperseded || r1.BusinessHash != act.BusinessHash || r1.Number != act.Number || r1.Calculation == nil ||
		r1.Calculation.NetMinor != "96000" || r1.Calculation.CostMinor != "66000" || r1.Calculation.MarginRule != "pass" ||
		!slices.Equal(events(r1), []string{"saved", "frozen", "review_requested", "review_approved", "activated", "superseded", "approval_revoked"}) {
		t.Fatalf("revision 1 %+v %v", r1, events(r1))
	}
	if r2.Status != trdapi.StatusActive || r2.BaseRevision != 1 || r2.Number != rep.Number ||
		!slices.Equal(events(r2), []string{"saved", "frozen", "review_requested", "review_approved", "replaced"}) {
		t.Fatalf("revision 2 %+v %v", r2, events(r2))
	}
	if e := r1.Events[4]; e.By != actorRef("human", "editor") || e.RootVersion != act.RowVersion {
		t.Fatalf("the activation step %+v", e)
	}

	// Reviews and approvals.
	ah, err := q.c.TRD.ApprovalHistory(q.author, trdapi.HistoryQuery{QuoteID: "quote.1"})
	q.must(err)
	if len(ah.Reviews) != 2 || ah.Next != 0 {
		t.Fatalf("approval history %+v", ah)
	}
	first := ah.Reviews[0]
	var steps []string
	for _, e := range first.Events {
		steps = append(steps, fmt.Sprintf("%s@%d", e.Event, e.EffectivityVersion))
	}
	if first.Seq != 1 || first.ApprovalID != "approval.quote.1" || first.Effectivity != "revoked" || first.EffectivityVersion != 2 ||
		first.ReviewStatus != "approved" || first.TargetCommand != trdapi.TargetActivate ||
		!slices.Equal(steps, []string{"review_requested@0", "review_approved@1", "approval_revoked@2"}) {
		t.Fatalf("the first review %+v %v", first, steps)
	}
	if s := ah.Reviews[1]; s.Seq != 2 || s.ApprovalID != "approval.2" || s.Effectivity != "valid" || s.ExpectedActiveRevision != 1 {
		t.Fatalf("the second review %+v", s)
	}
	if page, err := q.c.TRD.ApprovalHistory(q.author, trdapi.HistoryQuery{QuoteID: "quote.1", Page: trdapi.Page{After: 1}}); err != nil ||
		len(page.Reviews) != 1 || page.Reviews[0].Seq != 2 {
		t.Fatalf("after the first review %+v %v", page, err)
	}

	// Formal documents and commit evidence.
	dh, err := q.c.TRD.DocumentHistory(q.author, trdapi.HistoryQuery{QuoteID: "quote.1"})
	q.must(err)
	if len(dh.Documents) != 2 || dh.Documents[0].Integrity != trdapi.DocumentIntact || dh.Documents[1].Integrity != trdapi.DocumentIntact ||
		dh.Documents[0].Number != act.Number || dh.Documents[0].DocumentSHA256 != act.DocumentSHA256 || dh.Documents[0].RevisionStatus != "superseded" ||
		dh.Documents[1].FileName != rep.FileName || dh.Documents[1].FormalizedBy != actorRef("human", "editor") {
		t.Fatalf("document history %+v", dh)
	}
	ch, err := q.c.TRD.CommitHistory(q.author, trdapi.HistoryQuery{QuoteID: "quote.1"})
	q.must(err)
	if len(ch.Commits) != 2 {
		t.Fatalf("commit history %+v", ch)
	}
	c1, c2 := ch.Commits[0], ch.Commits[1]
	if c1.ActionID != act.ActionID || c1.Command != trdapi.CommandActivate || c1.ApprovalID != "approval.quote.1" || !c1.HasCostQualification ||
		c1.CostSnapshotID != q.snap.SnapshotID || c1.Number != act.Number || len(c1.EntriesSHA256) != 64 || c1.RootVersion != act.RowVersion ||
		c2.ActionID != rep.ActionID || c2.Command != trdapi.CommandReplace || c2.ReplacedRevision != 1 || c2.ApprovalID != "approval.2" {
		t.Fatalf("commits %+v %+v", c1, c2)
	}

	// The Outbox: QuoteRoot, CustomerResponse and Acceptance events in enqueue order, three a page.
	var all []trdapi.OutboxEntry
	var after int64
	for pages := 0; ; pages++ {
		p, err := q.c.TRD.OutboxHistory(q.author, trdapi.HistoryQuery{QuoteID: "quote.1", Page: trdapi.Page{After: after, Limit: 3}})
		q.must(err)
		all = append(all, p.Events...)
		if after = p.Next; after == 0 {
			break
		}
		if pages > 10 {
			t.Fatal("the pages do not end")
		}
	}
	var types []string
	for i, e := range all {
		types = append(types, strings.TrimSuffix(strings.TrimPrefix(e.EventType, "idp-brant.trd."), ".v1"))
		if e.RelayStatus != trdapi.RelayPending || e.ExternalDelivery != trdapi.ExternalDeliveryNotStarted || len(e.PayloadSHA256) != 64 ||
			(i > 0 && e.Seq <= all[i-1].Seq) || e.ActionID == "" {
			t.Fatalf("event %d %+v", i, e)
		}
	}
	want := []string{"quote_frozen", "quote_review_requested", "quote_review_decided", "quote_activated", "quote_frozen", "quote_review_requested",
		"quote_review_decided", "quote_replaced", "quote_approval_revoked", "customer_response_recorded", "acceptance_verified"}
	if !slices.Equal(types, want) {
		t.Fatalf("events %v", types)
	}
	if e := all[3]; e.AggregateType != "QuoteRoot" || e.AggregateID != "quote.1" || e.BusinessRevision != 1 || e.ActionID != act.ActionID {
		t.Fatalf("QuoteActivated %+v", e)
	}
	if e := all[10]; e.AggregateType != "Acceptance" || e.AggregateID != "acceptance.1" || e.BusinessRevision != 2 {
		t.Fatalf("AcceptanceVerified %+v", e)
	}

	// Scope and contract.
	blind := q.regrant("blind", "company", without(allPermissions, "trd.read"))
	foreigner := q.provision(bootstrap.Grant{Scope: q.scope("company.B"), Actor: actorRef("human", "foreigner"), Permissions: allPermissions,
		DataScope: "company"})
	reads := map[string]func(ctx context.Context, h trdapi.HistoryQuery) error{
		"quote": func(ctx context.Context, h trdapi.HistoryQuery) error {
			_, err := q.c.TRD.QuoteHistory(ctx, h)
			return err
		},
		"approval": func(ctx context.Context, h trdapi.HistoryQuery) error {
			_, err := q.c.TRD.ApprovalHistory(ctx, h)
			return err
		},
		"document": func(ctx context.Context, h trdapi.HistoryQuery) error {
			_, err := q.c.TRD.DocumentHistory(ctx, h)
			return err
		},
		"commit": func(ctx context.Context, h trdapi.HistoryQuery) error {
			_, err := q.c.TRD.CommitHistory(ctx, h)
			return err
		},
		"outbox": func(ctx context.Context, h trdapi.HistoryQuery) error {
			_, err := q.c.TRD.OutboxHistory(ctx, h)
			return err
		},
	}
	for name, read := range reads {
		for who, c := range map[string]struct {
			ctx  context.Context
			h    trdapi.HistoryQuery
			want string
		}{
			"without trd.read": {blind, trdapi.HistoryQuery{QuoteID: "quote.1"}, kernel.CodePermissionDenied},
			"another company":  {foreigner, trdapi.HistoryQuery{QuoteID: "quote.1"}, kernel.CodeNotFound},
			"a missing quote":  {q.author, trdapi.HistoryQuery{QuoteID: "quote.missing"}, kernel.CodeNotFound},
			"a page too large": {q.author, trdapi.HistoryQuery{QuoteID: "quote.1", Page: trdapi.Page{Limit: trdapi.MaxPage + 1}}, kernel.CodeSchemaInvalid},
		} {
			if got := code(read(c.ctx, c.h)); got != c.want {
				t.Errorf("%s history %s: %s, want %s", name, who, got, c.want)
			}
		}
	}
	if q.business() != before {
		t.Fatal("the histories changed the business state")
	}
	for _, read := range []string{trdapi.QueryQuoteHistory, trdapi.QueryApprovalHistory, trdapi.QueryDocumentHistory, trdapi.QueryCommitHistory,
		trdapi.QueryOutboxHistory} {
		if !q.audited(read, "quote.1", "read", "") || !q.audited(read, "quote.1", "rejected", kernel.CodeSchemaInvalid) {
			t.Errorf("%s is not audited", read)
		}
	}
}

// Python test_t03_query_contract.py test_dispatch_preflight_connects_local_facts_without_claiming_delivery
// (PYT-0432), test_dispatch_target_exposes_typed_refs_but_blocks_without_recipient_snapshot_channel
// (PYT-0433) and test_dispatch_target_reads_immutable_formalization_snapshot (PYT-0434); T03
// A14–A16: the activated revision's local chain is closed — commit evidence, active, unheld,
// unexpired, its document intact, its approval valid, QuoteActivated in the Outbox, the customer,
// contact and address as bound — and nothing claims a delivery; its target is the recipient its
// sealed document binds, with a stable snapshot hash the database never lets change, and is not
// ready while there is no channel. A frozen revision has no commit evidence; a contact revised
// since the freeze is stale; a revoked approval holds the revision; a cancelled one is not current.
func TestDispatchPreflightAndTarget(t *testing.T) {
	q := newTrdWorld(t)
	a := q.activated("quote.1")
	before := q.business()
	p, err := q.c.TRD.DispatchPreflight(q.author, trdapi.DispatchQuery{QuoteID: "quote.1", BusinessRevision: 1})
	q.must(err)
	oh, err := q.c.TRD.OutboxHistory(q.author, trdapi.HistoryQuery{QuoteID: "quote.1"})
	q.must(err)
	activated := oh.Events[len(oh.Events)-1]
	if !p.Ready || len(p.Reasons) != 0 || p.ActivationEventID != activated.EventID || activated.EventType != trdapi.EventQuoteActivated ||
		p.ActionID != a.ActionID || p.Number != a.Number || p.DocumentSHA256 != a.DocumentSHA256 || p.ApprovalID != "approval.quote.1" ||
		p.BusinessHash != a.BusinessHash || p.ExternalDelivery != trdapi.ExternalDeliveryNotStarted {
		t.Fatalf("preflight %+v", p)
	}
	target, err := q.c.TRD.DispatchTarget(q.author, trdapi.DispatchQuery{QuoteID: "quote.1", BusinessRevision: 1})
	q.must(err)
	if target.CustomerRef != (trdapi.PartyRef{PartyID: "customer.1", Revision: 2}) || target.ContactRef != (trdapi.ContactRef{ContactID: "contact.1",
		RowVersion: 2}) || len(target.Addresses) != 1 || target.Addresses[0].Role != "buyer" || target.Addresses[0].Address != "合成买方地址\n上海" ||
		len(target.RecipientSnapshotSHA256) != 64 || !target.PreflightReady || target.TargetReady || target.ChannelStatus != trdapi.ChannelNotConfigured ||
		!slices.Equal(reasonsOf(target.Reasons), []string{"channel:EXTERNAL_CHANNEL_NOT_CONFIGURED"}) ||
		target.ExternalDelivery != trdapi.ExternalDeliveryNotStarted {
		t.Fatalf("target %+v", target)
	}
	if again, _ := q.c.TRD.DispatchTarget(q.reviewer, trdapi.DispatchQuery{QuoteID: "quote.1", BusinessRevision: 1}); again.RecipientSnapshotSHA256 !=
		target.RecipientSnapshotSHA256 {
		t.Fatal("the snapshot hash is not stable")
	}
	if err = q.exec("company.A", `UPDATE brant_trd.quote_revision SET commercial = commercial || ' ' WHERE quote_id = 'quote.1'`); err == nil {
		t.Fatal("the database lets the sealed recipient change")
	}
	if q.business() != before {
		t.Fatal("dispatch changed the business state")
	}
	blind := q.regrant("blind", "company", without(allPermissions, "trd.read"))
	if _, err = q.c.TRD.DispatchPreflight(blind, trdapi.DispatchQuery{QuoteID: "quote.1", BusinessRevision: 1}); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("a reader without trd.read: %s", code(err))
	}
	if _, err = q.c.TRD.DispatchTarget(blind, trdapi.DispatchQuery{QuoteID: "quote.1", BusinessRevision: 1}); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("a reader without trd.read: %s", code(err))
	}
	if _, err = q.c.TRD.DispatchPreflight(q.author, trdapi.DispatchQuery{QuoteID: "quote.1", BusinessRevision: 9}); code(err) != kernel.CodeNotFound {
		t.Fatalf("a revision the root does not have: %s", code(err))
	}
	q.frozenQuote("quote.2")
	if p, err = q.c.TRD.DispatchPreflight(q.author, trdapi.DispatchQuery{QuoteID: "quote.2", BusinessRevision: 1}); err != nil || p.Ready ||
		!slices.Equal(reasonsOf(p.Reasons), []string{"commit_evidence:COMMIT_EVIDENCE_MISSING"}) {
		t.Fatalf("a frozen revision %+v %v", p, err)
	}

	// The contact moved on after the freeze.
	q.must(q.reviseContact(q.author, "contact.1", 2, q.key()))
	if p, err = q.c.TRD.DispatchPreflight(q.author, trdapi.DispatchQuery{QuoteID: "quote.1", BusinessRevision: 1}); err != nil || p.Ready ||
		!slices.Equal(reasonsOf(p.Reasons), []string{"contact:REFERENCE_STALE"}) || p.Reasons[0].Path != "/data/contact_ref" {
		t.Fatalf("a revised contact %+v %v", p, err)
	}
	if r := q.readiness(q.author, "quote.1", 0); r.Ready || !slices.Contains(reasonsOf(r.Reasons), "upstream:REFERENCE_STALE") {
		t.Fatalf("readiness with a revised contact %+v", r)
	}
	// A revoked approval holds the revision; a cancellation ends it.
	r, err := q.c.TRD.RevokeApproval(q.reviewer, trdapi.RevokeApproval{IdempotencyKey: q.key(), QuoteID: "quote.1", ApprovalID: "approval.quote.1",
		ExpectedEffectivityVersion: 1, Reason: "客户信用恶化", Evidence: q.proof})
	q.must(err)
	if p, err = q.c.TRD.DispatchPreflight(q.author, trdapi.DispatchQuery{QuoteID: "quote.1", BusinessRevision: 1}); err != nil ||
		!slices.Equal(reasonsOf(p.Reasons), []string{"risk_hold:APPROVAL_REVOKED", "approval:APPROVAL_REVOKED", "contact:REFERENCE_STALE"}) {
		t.Fatalf("held %+v %v", p, err)
	}
	_, err = q.c.TRD.Cancel(q.author, trdapi.Cancel{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: r.RowVersion, BusinessRevision: 1,
		Reason: "客户取消", Evidence: q.proof})
	q.must(err)
	if p, err = q.c.TRD.DispatchPreflight(q.author, trdapi.DispatchQuery{QuoteID: "quote.1", BusinessRevision: 1}); err != nil ||
		!slices.Contains(reasonsOf(p.Reasons), "revision:QUOTE_NOT_CURRENT") || p.Status != trdapi.StatusCancelled {
		t.Fatalf("cancelled %+v %v", p, err)
	}
}
