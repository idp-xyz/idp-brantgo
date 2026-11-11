//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The review suite (WBS S3-03, S3-14; TRD-INV-003, 004, 005, 007) on the TRD world: "editor" drafts,
// freezes and submits; "reviewer" decides; "approver" approves margin exceptions; all humans with
// every permission and company data scope unless a test provisions them otherwise. Whether an
// approval releases its revision is read through CheckApproval, which runs the check quote.activate
// and quote.replace will run in their transaction. Test names cite the Python V0.50 test they port.

func (q *trdWorld) submit(ctx context.Context, id string, expected, revision int64, review string) (trdapi.ReviewReceipt, error) {
	return q.c.TRD.SubmitReview(ctx, trdapi.SubmitReview{IdempotencyKey: q.key(), QuoteID: id, ExpectedVersion: expected,
		BusinessRevision: revision, ReviewRequestID: review, TargetCommand: trdapi.TargetActivate})
}

// approval is decision approve of review at hash, lapsing a day from now.
func (q *trdWorld) approval(id string, expected int64, review, hash, approval string) trdapi.DecideReview {
	return trdapi.DecideReview{IdempotencyKey: q.key(), QuoteID: id, ExpectedVersion: expected, ReviewRequestID: review, BusinessHash: hash,
		Decision: trdapi.DecisionApprove, ApprovalID: approval, ExpiresAt: kernel.FormatTimestamp(time.Now().Add(24 * time.Hour)),
		Reason: "内容核对无误", Evidence: q.proof}
}

func (q *trdWorld) check(id, approval string) trdapi.ApprovalCheck {
	q.t.Helper()
	c, err := q.c.TRD.CheckApproval(q.author, id, approval)
	q.must(err)
	return c
}

// regrant gives the human id in company.A exactly permissions with data scope dataScope.
func (q *trdWorld) regrant(id string, dataScope string, permissions []string) context.Context {
	return q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: actorRef("human", id), Permissions: permissions,
		DataScope: authorizationScope(dataScope), Team: "trade"})
}

func without(permissions []string, drop ...string) []string {
	return slices.DeleteFunc(slices.Clone(permissions), func(p string) bool { return slices.Contains(drop, p) })
}

func (q *trdWorld) eventsOf(eventType string) []event {
	var out []event
	for _, e := range q.events("company.A", trdapi.Source) {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

// Python test_t03_quote_review.py test_draft_freeze_review_independent_approval_and_revoke
// (PYT-0437) and TRD-INV-003, 004: the editor freezes and submits; the editor cannot decide its own
// quote (INDEPENDENT_REVIEW_REQUIRED); the reviewer approves the exact business hash and the approval
// releases the revision; the decision replays; a revocation at the stale effectivity version is
// VERSION_CONFLICT; a revoked approval no longer releases. Every step is in the Outbox and the read.
func TestQuoteReviewApprovalAndRevocation(t *testing.T) {
	q := newTrdWorld(t)
	f := q.frozenQuote("quote.1")
	s, err := q.submit(q.author, "quote.1", f.RowVersion, 1, "review.1")
	if err != nil || s.Status != trdapi.StatusPendingApproval || s.RowVersion != 3 || s.BusinessRevision != 1 || s.BusinessHash != f.BusinessHash ||
		s.ReviewRequestID != "review.1" || s.MarginExceptionRequired {
		t.Fatalf("submit %+v, %v", s, err)
	}
	_, err = q.c.TRD.DecideReview(q.author, q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.1"))
	q.want(err, trdapi.CodeIndependentReviewRequired)
	wrong := q.approval("quote.1", 3, "review.1", sha256Hex("another document"), "approval.1")
	_, err = q.c.TRD.DecideReview(q.reviewer, wrong)
	q.want(err, trdapi.CodeBusinessHashMismatch)
	cmd := q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.1")
	d, err := q.c.TRD.DecideReview(q.reviewer, cmd)
	if err != nil || d.Status != trdapi.ReviewApproved || d.RowVersion != 4 || d.ApprovalID != "approval.1" || d.EffectivityVersion != 1 ||
		d.BusinessHash != f.BusinessHash {
		t.Fatalf("approve %+v, %v", d, err)
	}
	again, err := q.c.TRD.DecideReview(q.reviewer, cmd)
	if err != nil || !again.Replayed || again.ActionID != d.ActionID || again.ApprovalID != "approval.1" {
		t.Fatalf("replay %+v, %v", again, err)
	}
	if c := q.check("quote.1", "approval.1"); !c.Releasable || c.Code != "" || c.MarginRule != "pass" || c.BusinessRevision != 1 {
		t.Fatalf("check %+v", c)
	}
	revoke := trdapi.RevokeApproval{IdempotencyKey: q.key(), QuoteID: "quote.1", ApprovalID: "approval.1", ExpectedEffectivityVersion: 1,
		Reason: "客户信用变化", Evidence: q.proof}
	r, err := q.c.TRD.RevokeApproval(q.reviewer, revoke)
	if err != nil || r.Status != trdapi.ApprovalRevoked || r.EffectivityVersion != 2 || r.RowVersion != 5 || r.ReviewRequestID != "review.1" {
		t.Fatalf("revoke %+v, %v", r, err)
	}
	revoke.IdempotencyKey = q.key()
	_, err = q.c.TRD.RevokeApproval(q.reviewer, revoke)
	q.want(err, kernel.CodeVersionConflict)
	if c := q.check("quote.1", "approval.1"); c.Releasable || c.Code != trdapi.CodeApprovalRevoked {
		t.Fatalf("check after the revocation %+v", c)
	}
	v := q.getQuote("quote.1")
	if v.RootVersion != 5 || v.Revisions[0].Status != trdapi.StatusPendingApproval || len(v.Reviews) != 1 || v.Reviews[0].Status != trdapi.ReviewApproved ||
		v.Reviews[0].DecidedBy != actorRef("human", "reviewer") || len(v.Approvals) != 1 || v.Approvals[0].Effectivity != trdapi.ApprovalRevoked ||
		v.Approvals[0].EffectivityVersion != 2 || v.Approvals[0].RevokeReason != "客户信用变化" ||
		v.Approvals[0].CalculationVersion != "brant.quote-calculation/0.1.0" || v.Approvals[0].PricingPolicyRef != tref(q.policy) ||
		v.Revisions[0].Calculation == nil || v.Revisions[0].Calculation.PayableMinor != "96000" {
		t.Fatalf("quote %+v", v)
	}
	requested, decided, revoked := q.eventsOf(trdapi.EventQuoteReviewRequested), q.eventsOf(trdapi.EventQuoteReviewDecided),
		q.eventsOf(trdapi.EventQuoteApprovalRevoked)
	if len(requested) != 1 || len(decided) != 1 || len(revoked) != 1 || decided[0].Payload["approval_id"] != "approval.1" ||
		decided[0].Payload["business_hash"] != f.BusinessHash || revoked[0].Payload["effectivity_version"] != float64(2) {
		t.Fatalf("events %+v %+v %+v", requested, decided, revoked)
	}
}

// Python test_t03_quote_review.py test_ai_cannot_decide_review_and_rollback_leaves_no_event
// (PYT-0439): an AI agent or the system cannot submit, decide or revoke whatever it holds; nothing is
// written.
func TestAICannotReviewAndLeavesNothing(t *testing.T) {
	q := newTrdWorld(t)
	f := q.frozenQuote("quote.1")
	agent, system := q.actorOf(kernel.ActorAI, "agent.1"), q.actorOf(kernel.ActorSystem, "tsk.1")
	for _, ctx := range []context.Context{agent, system} {
		_, err := q.submit(ctx, "quote.1", f.RowVersion, 1, "review.1")
		q.want(err, kernel.CodePermissionDenied)
	}
	_, err := q.submit(q.author, "quote.1", f.RowVersion, 1, "review.1")
	q.must(err)
	for _, ctx := range []context.Context{agent, system} {
		_, err = q.c.TRD.DecideReview(ctx, q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.1"))
		q.want(err, kernel.CodePermissionDenied)
	}
	if n := q.count("company.A", "brant_trd.quote_approval"); n != 0 || len(q.eventsOf(trdapi.EventQuoteReviewDecided)) != 0 {
		t.Fatalf("%d approvals after the refusals", n)
	}
	_, err = q.c.TRD.DecideReview(q.reviewer, q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.1"))
	q.must(err)
	_, err = q.c.TRD.RevokeApproval(agent, trdapi.RevokeApproval{IdempotencyKey: q.key(), QuoteID: "quote.1", ApprovalID: "approval.1",
		ExpectedEffectivityVersion: 1, Reason: "r", Evidence: q.proof})
	q.want(err, kernel.CodePermissionDenied)
}

// TRD-INV-004 at decision time (the S3-03 brief: a permission revoked between submission and
// decision is refused): the reviewer's grant is read in the decision's transaction — without
// quote.review.decide or trd.read it is PERMISSION_DENIED, with an own data scope the quote is
// NOT_FOUND; restored, the decision passes.
func TestReviewerQualificationIsCheckedAtDecisionTime(t *testing.T) {
	q := newTrdWorld(t)
	f := q.frozenQuote("quote.1")
	_, err := q.submit(q.author, "quote.1", f.RowVersion, 1, "review.1")
	q.must(err)
	for name, c := range map[string]struct {
		scope string
		perms []string
		want  string
	}{
		"quote.review.decide revoked": {"company", without(allPermissions, "quote.review.decide"), kernel.CodePermissionDenied},
		"trd.read revoked":            {"company", without(allPermissions, "trd.read"), kernel.CodePermissionDenied},
		"own data scope":              {"own", allPermissions, kernel.CodeNotFound},
	} {
		ctx := q.regrant("reviewer", c.scope, c.perms)
		_, err = q.c.TRD.DecideReview(ctx, q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.1"))
		if code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
	if n := q.count("company.A", "brant_trd.quote_approval"); n != 0 {
		t.Fatalf("%d approvals after the refusals", n)
	}
	ctx := q.regrant("reviewer", "company", allPermissions)
	if _, err = q.c.TRD.DecideReview(ctx, q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.1")); err != nil {
		t.Fatalf("restored: %v", err)
	}
}

// T03 A33 and A34 (Python test_t03_a33_reviewer_qualification.py PYT-0385 revoked review permission,
// PYT-0386 reviewer must remain human, PYT-0387 reviewer must retain quote scope, PYT-0388 each
// required read grant is rechecked; test_t03_a34_ready_reviewer.py PYT-0392 current qualification
// is reflected without business writes), on the check activation will run: after the approval, a
// reviewer who lost a permission, became an AI agent or narrowed its data scope makes it
// APPROVAL_STALE; restored, it releases again; the check writes nothing.
func TestApprovalFollowsTheReviewersCurrentQualification(t *testing.T) {
	q := newTrdWorld(t)
	f := q.frozenQuote("quote.1")
	_, err := q.submit(q.author, "quote.1", f.RowVersion, 1, "review.1")
	q.must(err)
	_, err = q.c.TRD.DecideReview(q.reviewer, q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.1"))
	q.must(err)
	before := q.getQuote("quote.1")
	for name, change := range map[string]func(){
		"quote.review.decide revoked (PYT-0385)": func() { q.regrant("reviewer", "company", without(allPermissions, "quote.review.decide")) },
		"trd.read revoked (PYT-0388)":            func() { q.regrant("reviewer", "company", without(allPermissions, "trd.read")) },
		"own data scope (PYT-0387)":              func() { q.regrant("reviewer", "own", allPermissions) },
		"no longer human (PYT-0386)": func() {
			q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: actorRef("ai", "reviewer"), Permissions: allPermissions,
				DataScope: "company", Team: "trade"})
		},
	} {
		change()
		if c := q.check("quote.1", "approval.1"); c.Releasable || c.Code != trdapi.CodeApprovalStale {
			t.Errorf("%s: %+v", name, c)
		}
		q.regrant("reviewer", "company", allPermissions)
		if c := q.check("quote.1", "approval.1"); !c.Releasable {
			t.Errorf("%s restored: %+v", name, c)
		}
	}
	if after := q.getQuote("quote.1"); after.RootVersion != before.RootVersion || len(after.Approvals) != 1 ||
		after.Approvals[0].EffectivityVersion != 1 {
		t.Fatalf("the check wrote %+v", after)
	}
}

// SM-TRD-REVIEW returned and rejected (Python decide_review: the revision is frozen again): a
// returned revision is not submitted twice; a revision revised from it is, and a rejection
// freezes that one again too.
func TestReturnAndRejectFreezeTheRevisionAgain(t *testing.T) {
	q := newTrdWorld(t)
	f := q.frozenQuote("quote.1")
	_, err := q.submit(q.author, "quote.1", f.RowVersion, 1, "review.1")
	q.must(err)
	ret := trdapi.DecideReview{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: 3, ReviewRequestID: "review.1",
		BusinessHash: f.BusinessHash, Decision: trdapi.DecisionReturn, Reason: "交期需与客户再确认"}
	d, err := q.c.TRD.DecideReview(q.reviewer, ret)
	if err != nil || d.Status != trdapi.ReviewReturned || d.RowVersion != 4 || d.ApprovalID != "" {
		t.Fatalf("return %+v, %v", d, err)
	}
	if v := q.getQuote("quote.1"); v.Revisions[0].Status != trdapi.StatusFrozen || v.Reviews[0].Reason != "交期需与客户再确认" {
		t.Fatalf("returned %+v", v)
	}
	_, err = q.submit(q.author, "quote.1", 4, 1, "review.2")
	q.want(err, kernel.CodeInvalidState)
	_, err = q.c.TRD.Revise(q.author, trdapi.Revise{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: 4, BaseRevision: 1})
	q.must(err)
	f2, err := q.freezeQuote(q.author, "quote.1", 5, 2)
	q.must(err)
	s, err := q.submit(q.author, "quote.1", f2.RowVersion, 2, "review.2")
	if err != nil || s.BusinessRevision != 2 || s.BusinessHash != f2.BusinessHash {
		t.Fatalf("submit the revised revision %+v, %v", s, err)
	}
	rej := trdapi.DecideReview{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: s.RowVersion, ReviewRequestID: "review.2",
		BusinessHash: f2.BusinessHash, Decision: trdapi.DecisionReject, Reason: "价格不可接受"}
	if d, err = q.c.TRD.DecideReview(q.reviewer, rej); err != nil || d.Status != trdapi.ReviewRejected {
		t.Fatalf("reject %+v, %v", d, err)
	}
	v := q.getQuote("quote.1")
	if v.Revisions[0].Status != trdapi.StatusFrozen || v.Revisions[1].Status != trdapi.StatusFrozen || len(v.Reviews) != 2 || len(v.Approvals) != 0 {
		t.Fatalf("after the rejection %+v", v)
	}
}

// TRD-INV-005 (Python test_t03_server_calculation.py test_structured_approval_cannot_outlive_quote,
// PYT-0445): an approval lapsing with the quote, or already lapsed, is APPROVAL_EXPIRED; a timestamp
// with an offset is not the contract's (SCHEMA_INVALID).
func TestApprovalCannotOutliveTheQuote(t *testing.T) {
	q := newTrdWorld(t)
	f := q.frozenQuote("quote.1")
	_, err := q.submit(q.author, "quote.1", f.RowVersion, 1, "review.1")
	q.must(err)
	for _, expires := range []string{f.ExpiresAt, kernel.FormatTimestamp(time.Now().Add(-time.Minute))} {
		cmd := q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.1")
		cmd.ExpiresAt = expires
		_, err = q.c.TRD.DecideReview(q.reviewer, cmd)
		if code(err) != trdapi.CodeApprovalExpired || path(err) != "/args/expires_at" {
			t.Errorf("expires %s: %s at %s", expires, code(err), path(err))
		}
	}
	cmd := q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.1")
	cmd.ExpiresAt = "2026-10-22T17:00:00+08:00"
	_, err = q.c.TRD.DecideReview(q.reviewer, cmd)
	q.want(err, kernel.CodeSchemaInvalid)
}

// TRD-INV-007, WBS S3-14, SC-02 and BC-AT06 on PostgreSQL 16 (Python test_t03_server_calculation.py
// has no exception path: Go diverges by design). The world's costing binds policy.1 at a minimum
// margin of 0.1, so SC-02's 7.00 at 0.25 (180/840) becomes 6.00 at 0.1 here: 720.00 net against
// 660.00 is a profit of 60.00 below 72.00, short by 12 USD (the exact SC-02 figures are the domain
// suite's). The review knows the margin requires approval; an ordinary approval does not release
// (MARGIN_POLICY_REQUIRES_APPROVAL); the editor's own exception and one by a reviewer without
// quote.margin_exception.approve are MARGIN_EXCEPTION_NOT_AUTHORIZED; an exception of another
// shortfall is APPROVAL_REFERENCE_MISMATCH; the approver's explicit exception releases the
// revision, bound to the hash, the rule versions and the shortfall, until the approver loses the
// capability.
func TestMarginExceptionApprovalSC02(t *testing.T) {
	q := newTrdWorld(t)
	d := q.data()
	d.Lines[0].UnitPrice = "6.00"
	_, err := q.saveQuote(q.author, "quote.1", 0, d)
	q.must(err)
	f, err := q.freezeQuote(q.author, "quote.1", 1, 1)
	q.must(err)
	if c := q.getQuote("quote.1").Revisions[0].Calculation; c.MarginRule != "requires_approval" || c.MarginShortfall != "12" ||
		c.MinimumMargin != "0.1" || c.ProfitMinor != "6000" {
		t.Fatalf("calculation %+v", c)
	}
	s, err := q.submit(q.author, "quote.1", f.RowVersion, 1, "review.1")
	if err != nil || !s.MarginExceptionRequired {
		t.Fatalf("submit %+v, %v", s, err)
	}
	exception := func(shortfall string) *trdapi.MarginException {
		return &trdapi.MarginException{PricingPolicyRef: tref(q.policy), MinimumMargin: "0.1", Shortfall: shortfall,
			Reason: "战略客户首单，经营决策", Evidence: q.proof}
	}
	plain := q.regrant("plain.reviewer", "company", without(allPermissions, trdapi.PermissionMarginException))
	approver := q.regrant("approver", "company", allPermissions)
	for name, c := range map[string]struct {
		ctx       context.Context
		shortfall string
		want      string
	}{
		"the editor's own exception": {q.author, "12", trdapi.CodeMarginExceptionNotAuthorized},
		"without the capability":     {plain, "12", trdapi.CodeMarginExceptionNotAuthorized},
		"another shortfall":          {approver, "12.01", trdapi.CodeApprovalReferenceMismatch},
	} {
		cmd := q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.1")
		cmd.MarginException = exception(c.shortfall)
		if _, err = q.c.TRD.DecideReview(c.ctx, cmd); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
	ordinary := q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.1")
	a, err := q.c.TRD.DecideReview(plain, ordinary)
	if err != nil || a.MarginExceptionApproved || !a.MarginExceptionRequired {
		t.Fatalf("the ordinary approval %+v, %v", a, err)
	}
	if c := q.check("quote.1", "approval.1"); c.Releasable || c.Code != trdapi.CodeMarginPolicyRequiresApproval || !c.MarginExceptionRequired {
		t.Fatalf("BC-AT06: an ordinary approval %+v", c)
	}
	complete := trdapi.DecideReview{IdempotencyKey: q.key(), QuoteID: "quote.1", ExpectedVersion: a.RowVersion, ReviewRequestID: "review.1",
		BusinessHash: f.BusinessHash, Decision: trdapi.DecisionApprove, ApprovalID: "approval.1", ExpiresAt: ordinary.ExpiresAt,
		MarginException: exception("12.000")}
	e, err := q.c.TRD.DecideReview(approver, complete)
	if err != nil || e.Status != trdapi.ReviewApproved || !e.MarginExceptionApproved || e.ApprovalID != "approval.1" {
		t.Fatalf("the exception %+v, %v", e, err)
	}
	if c := q.check("quote.1", "approval.1"); !c.Releasable || !c.MarginExceptionApproved {
		t.Fatalf("with the exception %+v", c)
	}
	v := q.getQuote("quote.1")
	x := v.Approvals[0].MarginException
	if x == nil || x.Shortfall != "12" || x.MinimumMargin != "0.1" || x.Currency != "USD" || x.BusinessHash != f.BusinessHash ||
		x.ApprovedBy != actorRef("human", "approver") || x.PricingPolicyRef != tref(q.policy) || v.Approvals[0].Reviewer != actorRef("human", "plain.reviewer") {
		t.Fatalf("exception %+v", x)
	}
	if decided := q.eventsOf(trdapi.EventQuoteReviewDecided); len(decided) != 2 || decided[1].Payload["margin_exception_approved"] != true {
		t.Fatalf("events %+v", decided)
	}
	q.regrant("approver", "company", without(allPermissions, trdapi.PermissionMarginException))
	if c := q.check("quote.1", "approval.1"); c.Releasable || c.Code != trdapi.CodeApprovalStale {
		t.Fatalf("an approver without the capability now %+v", c)
	}
}

// Concurrency on one review (the root's FOR UPDATE lock and version CAS): two reviewers deciding the
// same open review at once have one winner; the other is VERSION_CONFLICT or, having waited for the
// winner, INVALID_STATE; one approval and one QuoteReviewDecided are written.
func TestConcurrentDecisionsHaveOneWinner(t *testing.T) {
	q := newTrdWorld(t)
	f := q.frozenQuote("quote.1")
	_, err := q.submit(q.author, "quote.1", f.RowVersion, 1, "review.1")
	q.must(err)
	second := q.regrant("reviewer.2", "company", allPermissions)
	contexts := []context.Context{q.reviewer, second}
	cmds := []trdapi.DecideReview{q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.a"),
		q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.b")}
	cmds[1].Decision, cmds[1].ApprovalID, cmds[1].ExpiresAt, cmds[1].Reason = trdapi.DecisionReject, "", "", "不同意"
	var wg sync.WaitGroup
	got := make([]string, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := q.c.TRD.DecideReview(contexts[i], cmds[i])
			got[i] = code(err)
		}()
	}
	wg.Wait()
	slices.Sort(got)
	if got[0] != "OK" || (got[1] != kernel.CodeVersionConflict && got[1] != kernel.CodeInvalidState) {
		t.Fatalf("two decisions: %v", got)
	}
	v := q.getQuote("quote.1")
	if len(q.eventsOf(trdapi.EventQuoteReviewDecided)) != 1 || v.RootVersion != 4 || len(v.Reviews) != 1 || v.Reviews[0].Status == trdapi.ReviewOpen ||
		len(v.Approvals) > 1 {
		t.Fatalf("after the race %+v", v)
	}
}

// The review commands pass the root's object control like every quote write (TRD-INV-015): on a
// root bound to a task a submission without the task's claim is OBJECT_CONTROL_CONFLICT.
func TestReviewCommandsPassTheObjectControl(t *testing.T) {
	q := newTrdWorld(t)
	f := q.frozenQuote("quote.1")
	_, err := q.c.TRD.BindControl(q.author, trdapi.BindControl{IdempotencyKey: q.key(), QuoteID: "quote.1", TaskID: "task.1"})
	q.must(err)
	_, err = q.submit(q.author, "quote.1", f.RowVersion, 1, "review.1")
	q.want(err, trdapi.CodeObjectControlConflict)
	claimed, err := trdapi.WithControl(q.author, trdapi.ControlClaim{TaskID: "task.1", ControlEpoch: 1})
	q.must(err)
	if _, err = q.submit(claimed, "quote.1", f.RowVersion, 1, "review.1"); err != nil {
		t.Fatalf("the task's submission: %v", err)
	}
}

// TRD-INV-003 storage: the application role cannot move a pending revision anywhere but back to
// frozen, change a sealed revision's content while it moves, reopen or rewrite a review, rewrite,
// delete or unrevoke an approval, or change a margin exception.
func TestDatabaseRefusesIllegalReviewChanges(t *testing.T) {
	q := newTrdWorld(t)
	d := q.data()
	d.Lines[0].UnitPrice = "6"
	_, err := q.saveQuote(q.author, "quote.1", 0, d)
	q.must(err)
	f, err := q.freezeQuote(q.author, "quote.1", 1, 1)
	q.must(err)
	_, err = q.submit(q.author, "quote.1", f.RowVersion, 1, "review.1")
	q.must(err)
	cmd := q.approval("quote.1", 3, "review.1", f.BusinessHash, "approval.1")
	cmd.MarginException = &trdapi.MarginException{PricingPolicyRef: tref(q.policy), MinimumMargin: "0.1", Shortfall: "12", Reason: "例外"}
	approver := q.regrant("approver", "company", allPermissions)
	_, err = q.c.TRD.DecideReview(approver, cmd)
	q.must(err)
	for _, sql := range []string{
		`UPDATE brant_trd.quote_revision SET status = 'active' WHERE revision = 1`,
		`UPDATE brant_trd.quote_revision SET status = 'working' WHERE revision = 1`,
		`UPDATE brant_trd.quote_revision SET status = 'frozen', commercial = commercial || ' ' WHERE revision = 1`,
		`UPDATE brant_trd.quote_review SET status = 'open', decided_by_kind = NULL, decided_by_id = NULL, decided_epoch = NULL,
			decided_at = NULL, decision_evidence = NULL`,
		`UPDATE brant_trd.quote_review SET decision_reason = 'rewritten'`,
		`DELETE FROM brant_trd.quote_review`,
		`UPDATE brant_trd.quote_approval SET approved_by_id = 'someone'`,
		`UPDATE brant_trd.quote_approval SET expires_at = expires_at + interval '1 day'`,
		`DELETE FROM brant_trd.quote_approval`,
		`UPDATE brant_trd.quote_margin_exception SET shortfall = '1'`,
		`DELETE FROM brant_trd.quote_margin_exception`,
		`INSERT INTO brant_trd.quote_review SELECT tenant_id, company_id, quote_id, 'review.x', revision, business_hash, target_command,
			expected_active_revision, margin_exception_required, 'approved', requested_by_kind, requested_by_id, requested_epoch, requested_at,
			decided_by_kind, decided_by_id, decided_epoch, decided_at, decision_reason, decision_evidence FROM brant_trd.quote_review`,
	} {
		if err := q.exec("company.A", sql); err == nil {
			t.Errorf("the database accepted %s", sql)
		}
	}
	// The one change an approval takes: revoked once, at the next effectivity version.
	if err := q.exec("company.A", `UPDATE brant_trd.quote_approval SET effectivity = 'revoked', effectivity_version = 2,
		revoked_by_kind = 'human', revoked_by_id = 'r', revoke_epoch = 1, revoked_at = now(), revoke_reason = 'r', revoke_evidence = '[]'`); err != nil {
		t.Fatalf("a revocation: %v", err)
	}
	if err := q.exec("company.A", `UPDATE brant_trd.quote_approval SET effectivity = 'valid', effectivity_version = 3, revoked_by_kind = NULL,
		revoked_by_id = NULL, revoke_epoch = NULL, revoked_at = NULL, revoke_reason = NULL, revoke_evidence = NULL`); err == nil {
		t.Fatal("the database brought a revoked approval back")
	}
	var doc map[string]any
	q.must(json.Unmarshal([]byte(q.getQuote("quote.1").Revisions[0].Commercial), &doc))
	if doc["calculation"].(map[string]any)["margin_rule"] != "requires_approval" {
		t.Fatalf("calculation %v", doc["calculation"])
	}
}
