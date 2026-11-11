package quote

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The read side's judgments (WBS S3-09; ready.go): domain halves of Python T03 A8 quote_ready
// (t03_storage.quote_ready), A34 (test_t03_a34_ready_reviewer.py), A35 and A16, cited by mapping ID;
// the PostgreSQL suite is internal/bootstrap/trd_query_integration_test.go.

// active is the fixture frozen at price under minimum, approved by "reviewer" (and, below the
// minimum, its margin exception by "approver") and activated with approval.1.
func active(t *testing.T, price, minimum string) *Quote {
	t.Helper()
	q := frozen(t, price, minimum)
	if _, err := q.SubmitReview(ReviewRequest{ID: "review.1", Revision: 1, Target: TargetActivate}, human("submitter"), 1, issuedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := approve(q, "review.1", "approval.1", reviewer("reviewer", false), nil); err != nil {
		t.Fatal(err)
	}
	if q.Review("review.1").MarginExceptionRequired {
		c := &ExceptionClaim{PricingPolicy: policy, MinimumMargin: kernel.MustDecimal(minimum), Reason: "经营决策"}
		calc, _ := q.Revision(1).Calculation()
		c.Shortfall = kernel.MustDecimal(calc.MarginShortfall)
		a := q.Approval("approval.1")
		if _, _, err := q.Decide(ReviewDecision{ReviewID: "review.1", BusinessHash: a.BusinessHash, Decision: DecisionApprove, ApprovalID: a.ID,
			ExpiresAt: a.ExpiresAt, Exception: c}, reviewer("approver", true), issuedAt.Add(90*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := formalize(q, 1, "approval.1", TargetActivate, 0, documentOf(q, 1, "QT-1", "one")); err != nil {
		t.Fatal(err)
	}
	return q
}

// Python test_t03_a34_ready_reviewer.py test_current_reviewer_qualification_is_reflected_without_business_writes
// (PYT-0392, domain half), test_t03_a35_time_qualification.py
// test_ready_compares_quote_and_approval_offsets_as_instants (PYT-0397, the approval half) and
// TRD-INV-004: the approval that formalized the active revision stands while its reviewer is an
// active human grant holding quote.review.decide and trd.read that sees the quote; each loss is
// APPROVAL_STALE, and restoring the grant restores it; the approval of a revision not active lapses
// at its expires_at (equality lapsed), an active revision's is spent; a reviewer among the authors is INDEPENDENT_REVIEW_REQUIRED; a margin
// exception's approver must stay qualified with the capability.
func TestFormalApprovalStandsOnCurrentQualification(t *testing.T) {
	q := active(t, "8", "0.1")
	rev := q.Active()
	stands := func(at time.Time, s Standings) string {
		a, err := q.ReadyApproval(rev, at, s)
		if a == nil || a.ID != "approval.1" {
			t.Fatalf("the approval judged: %+v", a)
		}
		return code(err)
	}
	if got := stands(at, qualified(nil)); got != "OK" {
		t.Fatalf("a qualified reviewer: %s", got)
	}
	for name, s := range map[string]Standing{
		"quote.review.decide revoked": {Human: true, SeesRoot: true, Permissions: []string{PermissionRead}},
		"trd.read revoked":            {Human: true, SeesRoot: true, Permissions: []string{PermissionReview}},
		"no longer human":             {SeesRoot: true, Permissions: []string{PermissionReview, PermissionRead}},
		"own data scope":              {Human: true, Permissions: []string{PermissionReview, PermissionRead}},
	} {
		if got := stands(at, qualified(map[string]Standing{"reviewer": s})); got != "APPROVAL_STALE at /commit/approval_id" {
			t.Errorf("%s: %s", name, got)
		}
	}
	expires := q.Approval("approval.1").ExpiresAt
	if got := stands(expires.Add(-time.Microsecond), qualified(nil)); got != "OK" {
		t.Fatalf("a microsecond before the approval lapses: %s", got)
	}
	// Once the revision is active its approval is spent: its expiry no longer counts (owner decision
	// 2026-11-11; Python reports APPROVAL_EXPIRED). Of a revision that is not active, it does.
	if got := stands(expires, qualified(nil)); got != "OK" {
		t.Fatalf("an active revision at the approval's expires_at: %s", got)
	}
	rev.Status = Superseded
	if got := stands(expires, qualified(nil)); got != "APPROVAL_EXPIRED at /commit/approval_id" {
		t.Fatalf("a superseded revision at the approval's expires_at: %s", got)
	}
	rev.Status = Active
	rev.Contributors = append(rev.Contributors, "reviewer")
	if got := stands(at, qualified(nil)); got != "INDEPENDENT_REVIEW_REQUIRED at /commit/approval_id" {
		t.Fatalf("a reviewer among the authors: %s", got)
	}

	m := active(t, "7.00", "0.25")
	if _, err := m.ReadyApproval(m.Active(), at, qualified(nil)); err != nil {
		t.Fatalf("an exception approver still qualified: %v", err)
	}
	lost := qualified(map[string]Standing{"approver": {Human: true, SeesRoot: true, Permissions: []string{PermissionReview, PermissionRead}}})
	if _, err := m.ReadyApproval(m.Active(), at, lost); code(err) != "APPROVAL_STALE at /commit/approval_id" {
		t.Fatalf("an exception approver without the capability now: %s", code(err))
	}
}

// Python test_t03_a34_ready_reviewer.py test_another_approval_cannot_replace_formalization_approval
// (PYT-0393, domain half) and test_missing_commit_evidence_cannot_report_ready (PYT-0394, domain
// half), TRD-INV-003: readiness judges the approval the commit evidence names and no other — after a
// replacement, the replacement's approval, while revision 1's approval, still valid and qualified,
// never stands in for it; a revoked approval is APPROVAL_REVOKED, a missing one APPROVAL_MISSING,
// one of other content APPROVAL_REFERENCE_MISMATCH, a revision without commit evidence
// COMMIT_EVIDENCE_MISSING.
func TestReadinessJudgesTheCommittedApprovalOnly(t *testing.T) {
	q := active(t, "8", "0.1")
	n := frozenNext(t, q)
	approvedFor(t, q, n, "review.2", "approval.2", TargetReplace, 1)
	if _, _, err := formalize(q, n, "approval.2", TargetReplace, 1, documentOf(q, n, "QT-2", "two")); err != nil {
		t.Fatal(err)
	}
	rev := q.Active()
	if a, err := q.CommittedApproval(rev); err != nil || a.ID != "approval.2" {
		t.Fatalf("the committed approval %+v, %v", a, err)
	}
	// approval.1 stays valid and its reviewer qualified; approval.2's reviewer lost the review grant.
	lost := qualified(map[string]Standing{"reviewer": {Human: true, SeesRoot: true, Permissions: []string{PermissionRead}}})
	if _, err := q.ReadyApproval(rev, at, lost); code(err) != "APPROVAL_STALE at /commit/approval_id" {
		t.Fatalf("another approval stands in: %s", code(err))
	}
	if q.Approval("approval.1").Effectivity != Valid {
		t.Fatal("approval.1 is valid")
	}

	if _, _, err := q.Revoke(Revocation{ApprovalID: "approval.2", ExpectedEffectivity: 1, Reason: "撤销"}, human("reviewer"), 3, at); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CommittedApproval(rev); code(err) != "APPROVAL_REVOKED at /commit/approval_id" {
		t.Fatalf("revoked: %s", code(err))
	}
	missing := *rev.Formal
	missing.Commit.ApprovalID = "approval.missing"
	tampered := *rev
	tampered.Formal = &missing
	if _, err := q.CommittedApproval(&tampered); code(err) != "APPROVAL_MISSING at /commit/approval_id" {
		t.Fatalf("missing: %s", code(err))
	}
	missing.Commit.ApprovalID = "approval.1"
	if _, err := q.CommittedApproval(&tampered); code(err) != "APPROVAL_REFERENCE_MISMATCH at /commit/approval_id" {
		t.Fatalf("another revision's approval: %s", code(err))
	}
	tampered.Formal = nil
	if a, err := q.ReadyApproval(&tampered, at, qualified(nil)); a != nil || code(err) != "COMMIT_EVIDENCE_MISSING at /commit" {
		t.Fatalf("no commit evidence: %+v %s", a, code(err))
	}
	if c, ok := ReadCategories[CodeCommitEvidenceMissing]; !ok || c != kernel.CategoryInvalid {
		t.Fatal("the read side declares its codes")
	}
}

// The frozen bindings hold on the current inputs (Python quote_ready's t03_cost_scope.bind and
// t03_sales_terms.bind, PYT-0410/0411 domain half, and test_t03_a27_cost_scope.py
// test_current_scope_failure_does_not_rewrite_committed_quote_history, PYT-0296, the rule; the
// PostgreSQL half is TestQuoteReadyAfterTheRfqEndsKeepsTheHistory; TRD-INV-006): binding the fixture's inputs again
// gives the frozen bindings; a changed quantity rebinds the cost scope otherwise and a changed tax
// rate the sales terms (REFERENCE_STALE at the binding); a revision without a calculation is
// REFERENCE_INVALID.
func TestBoundComparesTheFrozenBindings(t *testing.T) {
	var cq calc.Quote
	var cf calc.Facts
	capture := func(q *calc.Quote, f *calc.Facts) { cq, cf = *q, *f }
	d := draft()
	c, err := calculator(d, "0.1", capture)()
	if err != nil {
		t.Fatal(err)
	}
	bind := func(q calc.Quote, f calc.Facts) error {
		scope, err := calc.BindScope(q, f)
		if err != nil {
			t.Fatal(err)
		}
		terms, err := calc.BindTerms(q)
		if err != nil {
			t.Fatal(err)
		}
		return c.Bound(scope, terms)
	}
	if err = bind(cq, cf); err != nil {
		t.Fatalf("the same inputs: %v", err)
	}
	// The stored form round-trips: bindings parsed back from the sealed document still compare.
	s, err := sealed(d, "0.1")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseCommercial(s.Commercial, s.BusinessHash)
	if err != nil {
		t.Fatal(err)
	}
	c = doc.Calculation
	if err = bind(cq, cf); err != nil {
		t.Fatalf("the sealed bindings: %v", err)
	}

	changed := cq
	changed.Lines = append([]calc.Line(nil), cq.Lines...)
	changed.Lines[0].SpecRevision = 2
	changedFacts := cf
	changedFacts.Cost.Lines = append([]calc.CostLine(nil), cf.Cost.Lines...)
	changedFacts.Cost.Lines[0].SpecRevision = 2
	if code(bind(changed, changedFacts)) != "REFERENCE_STALE at /calculation/cost_scope_binding" {
		t.Fatalf("another specification: %s", code(bind(changed, changedFacts)))
	}
	taxed := cq
	taxed.Lines = append([]calc.Line(nil), cq.Lines...)
	tax := *cq.Lines[0].Tax
	tax.Profile.Rate = kernel.MustDecimal("0.13")
	taxed.Lines[0].Tax = &tax
	if code(bind(taxed, cf)) != "REFERENCE_STALE at /calculation/sales_terms_binding" {
		t.Fatalf("another tax rate: %s", code(bind(taxed, cf)))
	}
	var none *Calculation
	if err := none.Bound(calc.ScopeBinding{}, calc.TermsBinding{}); code(err) != "REFERENCE_INVALID at /calculation" {
		t.Fatalf("no calculation: %s", code(err))
	}
}

// Python test_t03_query_contract.py test_dispatch_target_reads_immutable_formalization_snapshot
// (PYT-0434, domain half) and T03 A16: the recipient of a formal revision is the customer, contact
// and addresses its sealed commercial document binds, with a stable SHA-256; a commercial document
// that does not hash to its business hash is RECIPIENT_SNAPSHOT_INTEGRITY_FAILURE, and so is a
// revision never sealed.
func TestRecipientIsTheSealedSnapshot(t *testing.T) {
	q := active(t, "8", "0.1")
	rev := q.Active()
	r, sum, err := RecipientOf(rev)
	if err != nil || r.Customer != (PartyRef{PartyID: "customer.1", Revision: 2}) || r.Contact != (ContactRef{ContactID: "contact.1", RowVersion: 2}) ||
		len(r.Addresses) != 1 || r.Addresses[0].Ref != hq || r.Addresses[0].Role != "buyer" || r.Addresses[0].Address != "Synthetic buyer address\n上海" ||
		len(sum) != 64 {
		t.Fatalf("recipient %+v %s, %v", r, sum, err)
	}
	if _, again, _ := RecipientOf(rev); again != sum {
		t.Fatal("the snapshot hash is stable")
	}
	broken := *rev.Seal
	broken.Commercial = append([]byte(nil), rev.Seal.Commercial...)
	broken.Commercial[len(broken.Commercial)-2] ^= 1
	tampered := *rev
	tampered.Seal = &broken
	if _, _, err := RecipientOf(&tampered); code(err) != "RECIPIENT_SNAPSHOT_INTEGRITY_FAILURE at /party_snapshots" {
		t.Fatalf("a corrupt document: %s", code(err))
	}
	tampered.Seal = nil
	if _, _, err := RecipientOf(&tampered); code(err) != "RECIPIENT_SNAPSHOT_INTEGRITY_FAILURE at /" {
		t.Fatalf("never sealed: %s", code(err))
	}
}
