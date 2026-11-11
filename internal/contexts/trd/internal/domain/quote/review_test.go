package quote

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The review suite (WBS S3-03, S3-14; SM-TRD-REVIEW, SM-TRD-APPROVAL; TRD-INV-003–005, 007): the
// fixture frozen by "freezer" after "author" drafted it, submitted by "submitter", decided by
// "reviewer" and, for margin exceptions, "approver". Python tests are cited by mapping ID; their
// activation half is ported onto Releasable, the check quote.activate and quote.replace make.

const day = 24 * time.Hour

// frozen returns a root whose revision 1 is the fixture at unit price price, frozen under minimum
// margin minimum at issuedAt.
func frozen(t *testing.T, price, minimum string) *Quote {
	t.Helper()
	q := New("quote.1", "author", "trade")
	d := draft()
	d.Lines[0].UnitPrice = price
	w := q.Save(d, human("author"), 1, issuedAt)
	s, err := SealDraft(q.ID, w.Number, w.Data, facts(), calculator(w.Data, minimum), issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.Freeze(1, s, human("freezer"), 1, issuedAt); err != nil {
		t.Fatal(err)
	}
	return q
}

func submitted(t *testing.T, q *Quote, id string, revision int64) *Review {
	t.Helper()
	r, err := q.SubmitReview(ReviewRequest{ID: id, Revision: revision, Target: TargetActivate}, human("submitter"), 1, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// approve is decision approve of review by decider at issuedAt + 1h, the approval lapsing a day
// later.
func approve(q *Quote, review, approval string, decider Decider, exception *ExceptionClaim) (*Approval, error) {
	r := q.Review(review)
	hash := ""
	if r != nil {
		hash = r.BusinessHash
	}
	_, a, err := q.Decide(ReviewDecision{ReviewID: review, BusinessHash: hash, Decision: DecisionApprove, ApprovalID: approval,
		ExpiresAt: issuedAt.Add(25 * time.Hour), Exception: exception}, decider, issuedAt.Add(time.Hour))
	return a, err
}

func reviewer(id string, exception bool) Decider {
	return Decider{Actor: human(id), Epoch: 2, MarginException: exception}
}

// qualified answers every actor as an active human grant with every review permission, seeing the
// root; except edits the standing of one actor.
func qualified(except map[string]Standing) Standings {
	return func(actor string) (Standing, error) {
		if s, ok := except[actor]; ok {
			return s, nil
		}
		return Standing{Human: true, SeesRoot: true, Permissions: []string{PermissionReview, PermissionRead, PermissionMarginException}}, nil
	}
}

func release(q *Quote, approval string, at time.Time, standings Standings) error {
	_, _, err := q.Releasable(Release{Revision: 1, Target: TargetActivate, ApprovalID: approval}, at, standings)
	return err
}

// Python test_t03_quote_review.py test_draft_freeze_review_independent_approval_and_revoke
// (PYT-0437, domain half) and TRD-INV-004: a frozen revision goes to review; its author, freezer and
// submitter cannot decide it; an independent reviewer approves the exact business hash; the
// approval releases until it is revoked, and a revocation at a stale effectivity version is
// VERSION_CONFLICT.
func TestReviewApprovalAndRevocation(t *testing.T) {
	q := frozen(t, "8", "0.1")
	r := submitted(t, q, "review.1", 1)
	if r.Status != ReviewOpen || r.MarginExceptionRequired || q.Revision(1).Status != PendingApproval || q.RootVersion != 3 ||
		r.BusinessHash != q.Revision(1).Seal.BusinessHash {
		t.Fatalf("submitted %+v, root %d", r, q.RootVersion)
	}
	if _, err := q.SubmitReview(ReviewRequest{ID: "review.2", Revision: 1, Target: TargetActivate}, human("submitter"), 1, issuedAt); code(err) !=
		"INVALID_STATE at /args/business_revision" {
		t.Fatalf("submit a pending revision: %s", code(err))
	}
	for _, author := range []string{"author", "freezer", "submitter"} {
		if _, err := approve(q, "review.1", "approval.1", reviewer(author, true), nil); code(err) != "INDEPENDENT_REVIEW_REQUIRED at /args/review_request_id" {
			t.Errorf("%s decides: %s", author, code(err))
		}
	}
	if _, _, err := q.Decide(ReviewDecision{ReviewID: "review.1", BusinessHash: hashOf("other"), Decision: DecisionApprove, ApprovalID: "a",
		ExpiresAt: issuedAt.Add(day)}, reviewer("reviewer", false), issuedAt); code(err) != "BUSINESS_HASH_MISMATCH at /args/business_hash" {
		t.Fatalf("another hash: %s", code(err))
	}
	if _, err := approve(q, "review.9", "approval.1", reviewer("reviewer", false), nil); code(err) != "NOT_FOUND at /args/review_request_id" {
		t.Fatalf("an unknown review: %s", code(err))
	}
	a, err := approve(q, "review.1", "approval.1", reviewer("reviewer", false), nil)
	if err != nil || a.Effectivity != Valid || a.EffectivityVersion != 1 || r.Status != ReviewApproved || q.Revision(1).Status != PendingApproval ||
		a.Rules != (RuleVersions{NormalizationVersion, CalculationVersion, policy}) || a.BusinessHash != r.BusinessHash || q.RootVersion != 4 ||
		a.Exception != nil {
		t.Fatalf("approved %+v, %v", a, err)
	}
	if _, err = approve(q, "review.1", "approval.2", reviewer("reviewer.2", false), nil); code(err) != "INVALID_STATE at /args/review_request_id" {
		t.Fatalf("a decided review: %s", code(err))
	}
	got, c, err := q.Releasable(Release{Revision: 1, Target: TargetActivate, ApprovalID: "approval.1", ExpectedEffectivity: 1},
		issuedAt.Add(2*time.Hour), qualified(nil))
	if err != nil || got != a || c.MarginRule != MarginPass || c.PayableMinor != "96000" {
		t.Fatalf("releasable %+v, %+v, %v", got, c, err)
	}
	if _, _, err = q.Revoke(Revocation{ApprovalID: "approval.1", ExpectedEffectivity: 1, Reason: "客户信用变化", Evidence: proof}, human("reviewer"), 3,
		issuedAt.Add(3*time.Hour)); err != nil || a.Effectivity != Revoked || a.EffectivityVersion != 2 || a.Revocation.Reason != "客户信用变化" ||
		q.RootVersion != 5 {
		t.Fatalf("revoke %+v, %v", a, err)
	}
	if _, _, err = q.Revoke(Revocation{ApprovalID: "approval.1", ExpectedEffectivity: 1}, human("reviewer"), 3, issuedAt); code(err) !=
		"VERSION_CONFLICT at /args/expected_effectivity_version" {
		t.Fatalf("revoke again at version 1: %s", code(err))
	}
	if _, _, err = q.Revoke(Revocation{ApprovalID: "approval.1", ExpectedEffectivity: 2}, human("reviewer"), 3, issuedAt); code(err) !=
		"INVALID_STATE at /args/approval_id" {
		t.Fatalf("revoke a revoked approval: %s", code(err))
	}
	if _, _, err = q.Revoke(Revocation{ApprovalID: "approval.9", ExpectedEffectivity: 1}, human("reviewer"), 3, issuedAt); code(err) !=
		"NOT_FOUND at /args/approval_id" {
		t.Fatalf("an unknown approval: %s", code(err))
	}
	if err = release(q, "approval.1", issuedAt.Add(4*time.Hour), qualified(nil)); code(err) != "APPROVAL_REVOKED at /args/approval_id" {
		t.Fatalf("a revoked approval releases: %s", code(err))
	}
}

// SM-TRD-REVISION pending_approval → frozen and SM-TRD-REVIEW open → returned | rejected: a return
// or rejection freezes the revision again; it is not reviewed twice, a revision revised from it is;
// one review per root is open at a time.
func TestReturnAndRejectFreezeTheRevisionAgain(t *testing.T) {
	for _, decision := range []string{DecisionReturn, DecisionReject} {
		q := frozen(t, "8", "0.1")
		r := submitted(t, q, "review.1", 1)
		if _, _, err := q.Decide(ReviewDecision{ReviewID: "review.1", BusinessHash: r.BusinessHash, Decision: decision, Reason: "价格需复核"},
			reviewer("reviewer", false), issuedAt.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		want := map[string]ReviewStatus{DecisionReturn: ReviewReturned, DecisionReject: ReviewRejected}[decision]
		if r.Status != want || r.Decided.Reason != "价格需复核" || q.Revision(1).Status != Frozen || !q.Revision(1).StatusOnly || len(q.Approvals) != 0 {
			t.Fatalf("%s: %+v", decision, r)
		}
		if _, err := q.SubmitReview(ReviewRequest{ID: "review.2", Revision: 1, Target: TargetActivate}, human("submitter"), 1, issuedAt); code(err) !=
			"INVALID_STATE at /args/business_revision" {
			t.Fatalf("%s: a revision is reviewed once: %s", decision, code(err))
		}
		w, err := q.Revise(1, human("author"), 1, issuedAt)
		if err != nil {
			t.Fatal(err)
		}
		s, err := SealDraft(q.ID, w.Number, w.Data, facts(), calculator(w.Data, "0.1"), issuedAt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = q.Freeze(w.Number, s, human("freezer"), 1, issuedAt); err != nil {
			t.Fatal(err)
		}
		if _, err = q.SubmitReview(ReviewRequest{ID: "review.1", Revision: 2, Target: TargetActivate}, human("submitter"), 1, issuedAt); code(err) !=
			"BUSINESS_KEY_CONFLICT at /args/review_request_id" {
			t.Fatalf("%s: a review ID is the quote's once: %s", decision, code(err))
		}
		if _, err = q.SubmitReview(ReviewRequest{ID: "review.2", Revision: 2, Target: TargetActivate}, human("submitter"), 1, issuedAt); err != nil {
			t.Fatalf("%s: the revised revision: %v", decision, err)
		}
	}
	// One open review per root.
	q := frozen(t, "8", "0.1")
	submitted(t, q, "review.1", 1)
	w, err := q.Revise(1, human("author"), 1, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	s, err := SealDraft(q.ID, w.Number, w.Data, facts(), calculator(w.Data, "0.1"), issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.Freeze(w.Number, s, human("freezer"), 1, issuedAt); err != nil {
		t.Fatal(err)
	}
	if _, err = q.SubmitReview(ReviewRequest{ID: "review.2", Revision: 2, Target: TargetActivate}, human("submitter"), 1, issuedAt); code(err) !=
		"INVALID_STATE at /args/quote_id" {
		t.Fatalf("a second open review: %s", code(err))
	}
}

// TRD-INV-003 (批准绑定修订、哈希、目标动作、政策与预期旧 active；不能改目标复用): the target is
// checked against the root at submission, decision and release; an approval releases only its
// revision, for its target, at its hash and rule versions.
func TestApprovalBindsRevisionTargetHashAndRules(t *testing.T) {
	q := frozen(t, "8", "0.1")
	if _, err := q.SubmitReview(ReviewRequest{ID: "r", Revision: 1, Target: TargetReplace, ExpectedActive: 1}, human("submitter"), 1, issuedAt); code(err) !=
		"NO_ACTIVE_REVISION at /args/expected_active_revision" {
		t.Fatalf("replace without an active revision: %s", code(err))
	}
	if _, err := q.SubmitReview(ReviewRequest{ID: "r", Revision: 2, Target: TargetActivate}, human("submitter"), 1, issuedAt); code(err) !=
		"REFERENCE_INVALID at /args/business_revision" {
		t.Fatalf("an unknown revision: %s", code(err))
	}
	submitted(t, q, "review.1", 1)
	a, err := approve(q, "review.1", "approval.1", reviewer("reviewer", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	at := issuedAt.Add(2 * time.Hour)
	for name, c := range map[string]struct {
		r    Release
		edit func()
		want string
	}{
		"another revision":                     {Release{Revision: 2, Target: TargetActivate, ApprovalID: "approval.1"}, nil, "REFERENCE_INVALID at /args/business_revision"},
		"no approval":                          {Release{Revision: 1, Target: TargetActivate, ApprovalID: "approval.9"}, nil, "APPROVAL_REQUIRED at /args/approval_id"},
		"another effectivity":                  {Release{Revision: 1, Target: TargetActivate, ApprovalID: "approval.1", ExpectedEffectivity: 2}, nil, "VERSION_CONFLICT at /args/expected_approval_effectivity_version"},
		"an activate approval used to replace": {Release{Revision: 1, Target: TargetReplace, ExpectedActive: 1, ApprovalID: "approval.1"}, nil, "APPROVAL_REFERENCE_MISMATCH at /args/approval_id"},
		"another calculation version": {Release{Revision: 1, Target: TargetActivate, ApprovalID: "approval.1"},
			func() { a.Rules.CalculationVersion = "brant.quote-calculation/0.0.9" }, "APPROVAL_STALE at /args/approval_id"},
		"another pricing policy": {Release{Revision: 1, Target: TargetActivate, ApprovalID: "approval.1"},
			func() { a.Rules.PricingPolicy.Revision = 2 }, "APPROVAL_STALE at /args/approval_id"},
		"another hash": {Release{Revision: 1, Target: TargetActivate, ApprovalID: "approval.1"},
			func() { a.BusinessHash = hashOf("other") }, "APPROVAL_STALE at /args/approval_id"},
		"an active revision now": {Release{Revision: 1, Target: TargetActivate, ApprovalID: "approval.1"},
			func() { q.Revisions = append(q.Revisions, &Revision{Number: 9, Status: Active}) }, "QUOTE_ACTIVE at /args/approval_id"},
	} {
		saved, revisions := *a, q.Revisions
		if c.edit != nil {
			c.edit()
		}
		if _, _, err := q.Releasable(c.r, at, qualified(nil)); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
		*a, q.Revisions = saved, revisions
	}
	// An approval ID is the quote's once; the root must be as the target expects at the decision.
	q2 := frozen(t, "8", "0.1")
	q2.Revisions = append(q2.Revisions, &Revision{Number: 9, Status: Active})
	if _, err := q2.SubmitReview(ReviewRequest{ID: "r", Revision: 1, Target: TargetActivate}, human("submitter"), 1, issuedAt); code(err) !=
		"QUOTE_ACTIVE at /args/expected_active_revision" {
		t.Fatalf("activate beside an active revision: %s", code(err))
	}
	if _, err := q2.SubmitReview(ReviewRequest{ID: "r", Revision: 1, Target: TargetReplace, ExpectedActive: 8}, human("submitter"), 1, issuedAt); code(err) !=
		"QUOTE_NOT_CURRENT at /args/expected_active_revision" {
		t.Fatalf("replace another revision: %s", code(err))
	}
	if _, err := q2.SubmitReview(ReviewRequest{ID: "r", Revision: 1, Target: TargetReplace, ExpectedActive: 9}, human("submitter"), 1, issuedAt); err != nil {
		t.Fatal(err)
	}
	if a, err := approve(q2, "r", "approval.1", reviewer("reviewer", false), nil); err != nil || a.Target != TargetReplace || a.ExpectedActive != 9 {
		t.Fatalf("approve the replacement %+v, %v", a, err)
	}
	if _, _, err := q2.Releasable(Release{Revision: 1, Target: TargetReplace, ExpectedActive: 9, ApprovalID: "approval.1"}, at, qualified(nil)); err != nil {
		t.Fatalf("release the replacement: %v", err)
	}
	q3 := frozen(t, "8", "0.1")
	submitted(t, q3, "review.1", 1)
	q3.Approvals = append(q3.Approvals, &Approval{ID: "approval.1"})
	if _, err := approve(q3, "review.1", "approval.1", reviewer("reviewer", false), nil); code(err) != "BUSINESS_KEY_CONFLICT at /args/approval_id" {
		t.Fatalf("an approval ID taken: %s", code(err))
	}
}

// TRD-INV-005 and T03 A35 (Python test_t03_a35_time_qualification.py PYT-0395 and PYT-0396, domain
// halves; test_t03_server_calculation.py test_structured_approval_cannot_outlive_quote, PYT-0445;
// BC-AT12): an approval lapses after it is given and before the quote does; the quote and the
// approval are compared as instants, as_of < expires_at, equality refused, fractions exact. Python's
// offsets (+08:00, -05:00) are the same instants in the contract's UTC form: a Go timestamp with an
// offset is SCHEMA_INVALID at the contract (api), so no stored deadline is ever compared as text.
func TestApprovalTimeQualification(t *testing.T) {
	q := frozen(t, "8", "0.1")
	r := submitted(t, q, "review.1", 1)
	quoteExpires := q.Revision(1).Seal.ExpiresAt // 2026-10-28T08:00:00Z
	decide := func(expires time.Time, now time.Time) error {
		_, _, err := q.Decide(ReviewDecision{ReviewID: "review.1", BusinessHash: r.BusinessHash, Decision: DecisionApprove, ApprovalID: "approval.1",
			ExpiresAt: expires}, reviewer("reviewer", false), now)
		return err
	}
	for name, c := range map[string]struct {
		expires, now time.Time
		want         string
	}{
		"the quote's own expiry (PYT-0445)": {quoteExpires, issuedAt, "APPROVAL_EXPIRED at /args/expires_at"},
		"after the quote":                   {quoteExpires.Add(time.Microsecond), issuedAt, "APPROVAL_EXPIRED at /args/expires_at"},
		"at the decision":                   {issuedAt, issuedAt, "APPROVAL_EXPIRED at /args/expires_at"},
		"the quote expired at the decision": {quoteExpires.Add(-time.Hour), quoteExpires, "QUOTE_EXPIRED at /args/review_request_id"},
	} {
		if got := code(decide(c.expires, c.now)); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	parse := func(s string) time.Time {
		v, err := kernel.ParseTimestamp(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	at := issuedAt // 2026-10-22T09:00:00Z, as Python's A35 clock
	for name, c := range map[string]struct {
		expires string
		want    string
	}{
		"a second earlier (16:59:59+08:00)":    {"2026-10-22T08:59:59Z", "APPROVAL_EXPIRED at /args/approval_id"},
		"the exclusive boundary (17:00+08:00)": {"2026-10-22T09:00:00Z", "APPROVAL_EXPIRED at /args/approval_id"},
		"the boundary with fractions":          {"2026-10-22T09:00:00.000000Z", "APPROVAL_EXPIRED at /args/approval_id"},
		"a second later (04:00:01-05:00)":      {"2026-10-22T09:00:01Z", "OK"},
		"a later fraction":                     {"2026-10-22T09:00:00.500000Z", "OK"},
	} {
		q := frozen(t, "8", "0.1")
		r := submitted(t, q, "review.1", 1)
		if _, _, err := q.Decide(ReviewDecision{ReviewID: "review.1", BusinessHash: r.BusinessHash, Decision: DecisionApprove, ApprovalID: "approval.1",
			ExpiresAt: parse(c.expires)}, reviewer("reviewer", false), at.Add(-time.Hour)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := code(release(q, "approval.1", at, qualified(nil))); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	// BC-AT12: the use at the quote's expiry is refused, a microsecond before it is not.
	q = frozen(t, "8", "0.1")
	r = submitted(t, q, "review.1", 1)
	if _, _, err := q.Decide(ReviewDecision{ReviewID: "review.1", BusinessHash: r.BusinessHash, Decision: DecisionApprove, ApprovalID: "approval.1",
		ExpiresAt: quoteExpires.Add(-time.Microsecond)}, reviewer("reviewer", false), issuedAt); err != nil {
		t.Fatal(err)
	}
	if got := code(release(q, "approval.1", quoteExpires.Add(-2*time.Microsecond), qualified(nil))); got != "OK" {
		t.Fatalf("before the expiry: %s", got)
	}
	if got := code(release(q, "approval.1", quoteExpires, qualified(nil))); got != "QUOTE_EXPIRED at /args/business_revision" {
		t.Fatalf("at the expiry: %s", got)
	}
	q = frozen(t, "8", "0.1")
	if _, err := q.SubmitReview(ReviewRequest{ID: "r", Revision: 1, Target: TargetActivate}, human("submitter"), 1, quoteExpires); code(err) !=
		"QUOTE_EXPIRED at /args/business_revision" {
		t.Fatalf("submit an expired quote: %s", code(err))
	}
}

// T03 A33 (Python test_t03_a33_reviewer_qualification.py PYT-0385 revoked review permission,
// PYT-0386 reviewer must remain human, PYT-0387 reviewer must retain quote scope, PYT-0388 each
// required read grant is rechecked; domain halves): the use of an approval re-reads the reviewer's
// grant; a reviewer who lost quote.review.decide or trd.read, is no longer a human grant, or no
// longer sees the quote makes the approval APPROVAL_STALE; restoring the grant restores it (A34,
// PYT-0392).
func TestReviewerQualificationAtTheUse(t *testing.T) {
	q := frozen(t, "8", "0.1")
	submitted(t, q, "review.1", 1)
	if _, err := approve(q, "review.1", "approval.1", reviewer("reviewer", false), nil); err != nil {
		t.Fatal(err)
	}
	at := issuedAt.Add(2 * time.Hour)
	for name, s := range map[string]Standing{
		"quote.review.decide revoked": {Human: true, SeesRoot: true, Permissions: []string{PermissionRead}},
		"trd.read revoked":            {Human: true, SeesRoot: true, Permissions: []string{PermissionReview}},
		"no longer human":             {Human: false, SeesRoot: true, Permissions: []string{PermissionReview, PermissionRead}},
		"own data scope":              {Human: true, SeesRoot: false, Permissions: []string{PermissionReview, PermissionRead}},
		"no grant":                    {},
	} {
		if got := code(release(q, "approval.1", at, qualified(map[string]Standing{"reviewer": s}))); got != "APPROVAL_STALE at /args/approval_id" {
			t.Errorf("%s: %s", name, got)
		}
	}
	if got := code(release(q, "approval.1", at, qualified(nil))); got != "OK" {
		t.Fatalf("restored: %s", got)
	}
	q.Revisions[0].Contributors = append(q.Revisions[0].Contributors, "reviewer")
	if got := code(release(q, "approval.1", at, qualified(nil))); got != "INDEPENDENT_REVIEW_REQUIRED at /args/approval_id" {
		t.Fatalf("a reviewer among the authors: %s", got)
	}
}

// TRD-INV-007, WBS S3-14, SC-02 and BC-AT06 (Python test_t03_server_calculation.py: Python refuses a
// quote below the minimum margin at quote.submit_review and has no exception path; Go diverges by
// design, ES-02): 7.00 USD against 660.00 at a minimum of 0.25 requires approval, short by 30 USD.
// An ordinary approval does not release it; an exception by an author or without the capability is
// MARGIN_EXCEPTION_NOT_AUTHORIZED; an exception restating other figures is
// APPROVAL_REFERENCE_MISMATCH; the explicit exception of an independent holder of
// quote.margin_exception.approve releases it, bound to the hash, the rule versions and the
// shortfall, for as long as its approver stays qualified and the approval valid.
func TestMarginExceptionApproval(t *testing.T) {
	q := frozen(t, "7.00", "0.25")
	r := submitted(t, q, "review.1", 1)
	if !r.MarginExceptionRequired {
		t.Fatal("the review knows the margin requires approval")
	}
	claim := func(edit func(c *ExceptionClaim)) *ExceptionClaim {
		c := &ExceptionClaim{PricingPolicy: policy, MinimumMargin: kernel.MustDecimal("0.25"), Shortfall: kernel.MustDecimal("30.00"),
			Reason: "战略客户首单，经营决策"}
		if edit != nil {
			edit(c)
		}
		return c
	}
	for name, c := range map[string]struct {
		decider Decider
		claim   *ExceptionClaim
		want    string
	}{
		"the author's own exception": {reviewer("author", true), claim(nil), "MARGIN_EXCEPTION_NOT_AUTHORIZED at /args/margin_exception"},
		"no capability":              {reviewer("reviewer", false), claim(nil), "MARGIN_EXCEPTION_NOT_AUTHORIZED at /args/margin_exception"},
		"another shortfall":          {reviewer("approver", true), claim(func(c *ExceptionClaim) { c.Shortfall = kernel.MustDecimal("29.99") }), "APPROVAL_REFERENCE_MISMATCH at /args/margin_exception/shortfall"},
		"another minimum":            {reviewer("approver", true), claim(func(c *ExceptionClaim) { c.MinimumMargin = kernel.MustDecimal("0.2") }), "APPROVAL_REFERENCE_MISMATCH at /args/margin_exception/minimum_margin"},
		"another pricing policy":     {reviewer("approver", true), claim(func(c *ExceptionClaim) { c.PricingPolicy.Revision = 2 }), "APPROVAL_REFERENCE_MISMATCH at /args/margin_exception/pricing_policy_ref"},
	} {
		if _, err := approve(q, "review.1", "approval.1", c.decider, c.claim); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
	if len(q.Approvals) != 0 || r.Status != ReviewOpen {
		t.Fatal("a refused decision changes nothing")
	}
	// BC-AT06: the ordinary approval does not release.
	a, err := approve(q, "review.1", "approval.1", reviewer("reviewer", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	at := issuedAt.Add(2 * time.Hour)
	if got := code(release(q, "approval.1", at, qualified(nil))); got != "MARGIN_POLICY_REQUIRES_APPROVAL at /args/approval_id" {
		t.Fatalf("an ordinary approval: %s", got)
	}
	complete := func(decider Decider, approval string, expires time.Time, c *ExceptionClaim) error {
		_, _, err := q.Decide(ReviewDecision{ReviewID: "review.1", BusinessHash: r.BusinessHash, Decision: DecisionApprove, ApprovalID: approval,
			ExpiresAt: expires, Exception: c}, decider, issuedAt.Add(90*time.Minute))
		return err
	}
	for name, c := range map[string]struct {
		decider  Decider
		approval string
		expires  time.Time
		want     string
	}{
		"by the submitter":    {reviewer("submitter", true), "approval.1", a.ExpiresAt, "MARGIN_EXCEPTION_NOT_AUTHORIZED at /args/margin_exception"},
		"without capability":  {reviewer("reviewer.2", false), "approval.1", a.ExpiresAt, "MARGIN_EXCEPTION_NOT_AUTHORIZED at /args/margin_exception"},
		"of another approval": {reviewer("approver", true), "approval.2", a.ExpiresAt, "APPROVAL_REFERENCE_MISMATCH at /args/approval_id"},
		"with another expiry": {reviewer("approver", true), "approval.1", a.ExpiresAt.Add(time.Hour), "APPROVAL_REFERENCE_MISMATCH at /args/expires_at"},
	} {
		if got := code(complete(c.decider, c.approval, c.expires, claim(nil))); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	if err = complete(reviewer("approver", true), "approval.1", a.ExpiresAt, claim(nil)); err != nil {
		t.Fatal(err)
	}
	e := a.Exception
	if e == nil || e.Shortfall != "30" || e.MinimumMargin != "0.25" || e.Currency != "USD" || e.BusinessHash != r.BusinessHash || e.Rules != a.Rules ||
		e.Approved.By != human("approver") || e.Approved.Reason != "战略客户首单，经营决策" || !e.Fresh {
		t.Fatalf("exception %+v", e)
	}
	if got := code(complete(reviewer("approver", true), "approval.1", a.ExpiresAt, claim(nil))); got != "INVALID_STATE at /args/review_request_id" {
		t.Fatalf("a second exception: %s", got)
	}
	if got := code(release(q, "approval.1", at, qualified(nil))); got != "OK" {
		t.Fatalf("with the exception: %s", got)
	}
	if got := code(release(q, "approval.1", at, qualified(map[string]Standing{"approver": {Human: true, SeesRoot: true,
		Permissions: []string{PermissionReview, PermissionRead}}}))); got != "APPROVAL_STALE at /args/approval_id" {
		t.Fatalf("an approver without the capability now: %s", got)
	}
	saved := e.Shortfall
	e.Shortfall = "29"
	if got := code(release(q, "approval.1", at, qualified(nil))); got != "APPROVAL_STALE at /args/approval_id" {
		t.Fatalf("an exception of another shortfall: %s", got)
	}
	e.Shortfall = saved
	if _, _, err = q.Revoke(Revocation{ApprovalID: "approval.1", ExpectedEffectivity: 1, Reason: "撤回例外", Evidence: proof}, human("approver"), 3, at); err != nil {
		t.Fatal(err)
	}
	if got := code(release(q, "approval.1", at, qualified(nil))); got != "APPROVAL_REVOKED at /args/approval_id" {
		t.Fatalf("the exception lapses with its approval: %s", got)
	}

	// The content approval and the exception in one decision, by one human holding both.
	q = frozen(t, "7.00", "0.25")
	submitted(t, q, "review.1", 1)
	if a, err := approve(q, "review.1", "approval.1", reviewer("approver", true), claim(nil)); err != nil || a.Exception == nil {
		t.Fatalf("both at once %+v, %v", a, err)
	}
	if got := code(release(q, "approval.1", at, qualified(nil))); got != "OK" {
		t.Fatalf("both at once: %s", got)
	}
	// A margin that passes has no exception to approve.
	q = frozen(t, "8", "0.1")
	submitted(t, q, "review.1", 1)
	if _, err := approve(q, "review.1", "approval.1", reviewer("approver", true), claim(nil)); code(err) != "INVALID_STATE at /args/margin_exception" {
		t.Fatalf("an exception of a passing margin: %s", code(err))
	}
}

// A revision sealed without the server calculation (before S3-02 was wired) is never reviewed.
func TestRevisionWithoutCalculationIsNotReviewed(t *testing.T) {
	q := frozen(t, "8", "0.1")
	rev := q.Revision(1)
	doc, err := ParseCommercial(rev.Seal.Commercial, rev.Seal.BusinessHash)
	if err != nil {
		t.Fatal(err)
	}
	doc.Calculation = nil
	tree, err := TreeOf(doc)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Seal.Commercial, rev.Seal.BusinessHash, err = Digest(tree); err != nil {
		t.Fatal(err)
	}
	if _, err = q.SubmitReview(ReviewRequest{ID: "r", Revision: 1, Target: TargetActivate}, human("submitter"), 1, issuedAt); code(err) !=
		"INVALID_STATE at /args/business_revision" {
		t.Fatalf("a revision without a calculation: %s", code(err))
	}
}
