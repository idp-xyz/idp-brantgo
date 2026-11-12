package quote

import (
	"slices"
	"testing"
	"time"
)

// The formalization suite (WBS S3-04, S3-05; SM-TRD-REVISION; TRD-INV-001, 009, 010, 016): domain
// halves of Python test_t03_formalization.py, cited by mapping ID; the transaction, the number and
// the rendered bytes are the PostgreSQL suite's (internal/bootstrap/trd_formal_integration_test.go).

var at = issuedAt.Add(2 * time.Hour)

// approvedFor submits frozen revision n for target and has "reviewer" approve it as approval.
func approvedFor(t *testing.T, q *Quote, n int64, review, approval, target string, expectedActive int64) *Approval {
	t.Helper()
	if _, err := q.SubmitReview(ReviewRequest{ID: review, Revision: n, Target: target, ExpectedActive: expectedActive}, human("submitter"), 1,
		issuedAt); err != nil {
		t.Fatal(err)
	}
	a, err := approve(q, review, approval, reviewer("reviewer", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// frozenNext revises revision 1 into the next revision and freezes it.
func frozenNext(t *testing.T, q *Quote) int64 {
	t.Helper()
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
	return w.Number
}

// documentOf is a formal document of revision n with content, as the renderer would bind it.
func documentOf(q *Quote, n int64, number, content string) Document {
	b := []byte(content)
	return Document{Number: number, Sequence: Ref{Kind: KindNumberSequence, ID: "quote.seq", Revision: 1}, BusinessRef: "quote.1:1",
		BusinessHash: q.Revision(n).Seal.BusinessHash, Template: Ref{Kind: KindQuoteDocumentTemplate, ID: "quote.template", Revision: 1},
		TemplateHash: hashOf("template"), RendererVersion: QuoteRendererV010, FileName: number + ".txt", MimeType: "text/plain",
		ContentSHA256: Hash(b), SizeBytes: len(b), Content: b}
}

// formalize releases revision n with approval for target (replacing expectedActive) and formalizes
// it with document d at instant at.
func formalize(q *Quote, n int64, approval, target string, expectedActive int64, d Document) (*Revision, *Revision, error) {
	r := Release{Revision: n, Target: target, ExpectedActive: expectedActive, ApprovalID: approval, ExpectedEffectivity: 1}
	a, _, err := q.Releasable(r, at, qualified(nil))
	if err != nil {
		return nil, nil, err
	}
	return q.Formalize(Formalize{Release: r, IdempotencyKey: "key." + approval, Document: d,
		Binding:  []Entry{{"quote_id", q.ID}, {"number", d.Number}, {"content_sha256", d.ContentSHA256}},
		Upstream: []Entry{{"cost_snapshot_id", "snapshot.1"}}, CostSnapshotID: "snapshot.1", ControlEpoch: 0}, a, human("activator"), 4, at)
}

// Python test_t03_formalization.py test_activate_writes_number_file_binding_evidence_and_outbox_atomically
// (PYT-0413, domain half) and test_duplicate_formalization_is_rejected_without_second_number
// (PYT-0415, domain half), TRD-INV-009 and 016: an approved revision becomes active once, with its
// formal document and commit evidence bound; the root version moves once; a second formalization of
// it is refused; a document whose bytes are not its binding's is refused.
func TestActivationBindsTheFormalDocumentAndCommitEvidence(t *testing.T) {
	q := frozen(t, "8", "0.1")
	approvedFor(t, q, 1, "review.1", "approval.1", TargetActivate, 0)
	d := documentOf(q, 1, "QT-2026-00001", "Quotation\n\nNumber: QT-2026-00001\n")
	broken := d
	broken.Content = []byte("other bytes")
	if _, _, err := formalize(q, 1, "approval.1", TargetActivate, 0, broken); code(err) != "INVALID_KNOWN_VALUE at /document" {
		t.Fatalf("bytes that are not the bound ones: %s", code(err))
	}
	before := q.RootVersion
	rev, replaced, err := formalize(q, 1, "approval.1", TargetActivate, 0, d)
	if err != nil || replaced != nil || rev.Status != Active || q.Active() != rev || q.RootVersion != before+1 || rev.Formal == nil ||
		!rev.Formal.Fresh || !rev.StatusOnly {
		t.Fatalf("activate %+v, %v", rev, err)
	}
	c := rev.Formal.Commit
	if c.Command != TargetActivate || c.ApprovalID != "approval.1" || c.EffectivityVersion != 1 || c.Replaced != 0 || c.RootVersion != q.RootVersion ||
		c.AuthorityEpoch != 4 || c.By != human("activator") || !c.At.Equal(at) || c.IdempotencyKey != "key.approval.1" || c.CostSnapshotID != "snapshot.1" {
		t.Fatalf("commit %+v", c)
	}
	var names []string
	for _, e := range c.Entries {
		names = append(names, e.Name)
	}
	want := []string{"quote_id", "number", "content_sha256", "command", "idempotency_key", "approval_id", "approval_effectivity_version",
		"review_request_id", "authority_epoch", "control_epoch", "root_version", "committed_by", "committed_at", "cost_snapshot_id"}
	if !slices.Equal(names, want) {
		t.Fatalf("entries %v", names)
	}
	if doc, err := c.EntriesJSON(); err != nil || string(doc[:10]) != `[{"name":"` {
		t.Fatalf("entries JSON %s, %v", doc, err)
	}
	if _, _, err = q.Formalize(Formalize{Release: Release{Revision: 1, Target: TargetActivate, ApprovalID: "approval.1"}, Document: d},
		q.Approval("approval.1"), human("activator"), 4, at); code(err) != "BUSINESS_KEY_CONFLICT at /args/business_revision" {
		t.Fatalf("a second formalization: %s", code(err))
	}
	if q.FormalizedAt(q.RootVersion) != rev || q.FormalizedAt(before) != nil {
		t.Fatal("FormalizedAt")
	}
}

// TRD-INV-001, domain half: two revisions approved for a first activation; once one is active, the
// other's approval no longer releases (QUOTE_ACTIVE), and Formalize refuses it too.
func TestOneFirstActivationPerRoot(t *testing.T) {
	q := frozen(t, "8", "0.1")
	n := frozenNext(t, q)
	approvedFor(t, q, 1, "review.1", "approval.1", TargetActivate, 0)
	approvedFor(t, q, n, "review.2", "approval.2", TargetActivate, 0)
	if _, _, err := formalize(q, 1, "approval.1", TargetActivate, 0, documentOf(q, 1, "QT-1", "one")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := formalize(q, n, "approval.2", TargetActivate, 0, documentOf(q, n, "QT-2", "two")); code(err) != "QUOTE_ACTIVE at /args/approval_id" {
		t.Fatalf("a second first activation: %s", code(err))
	}
	if _, _, err := q.Formalize(Formalize{Release: Release{Revision: n, Target: TargetActivate, ApprovalID: "approval.2"},
		Document: documentOf(q, n, "QT-2", "two")}, q.Approval("approval.2"), human("activator"), 4, at); code(err) != "QUOTE_ACTIVE at /args/approval_id" {
		t.Fatalf("Formalize beside an active revision: %s", code(err))
	}
	if q.Revision(n).Status != PendingApproval || q.Revision(n).Formal != nil {
		t.Fatalf("revision %d %+v", n, q.Revision(n))
	}
}

// Python test_t03_formalization.py test_replace_supersedes_active_revision_and_writes_new_formal_artifacts
// (PYT-0416, domain half) and TRD-INV-010: an approval for quote.replace of the active revision
// makes the new revision active and the old one superseded in one step, with the replaced revision
// in the commit evidence; another expected revision is QUOTE_NOT_CURRENT; while the conversion slot
// is taken a replacement is ROOT_CONVERSION_LOCKED.
func TestReplacementSupersedesTheActiveRevision(t *testing.T) {
	q := frozen(t, "8", "0.1")
	approvedFor(t, q, 1, "review.1", "approval.1", TargetActivate, 0)
	if _, _, err := formalize(q, 1, "approval.1", TargetActivate, 0, documentOf(q, 1, "QT-1", "one")); err != nil {
		t.Fatal(err)
	}
	n := frozenNext(t, q)
	approvedFor(t, q, n, "review.2", "approval.2", TargetReplace, 1)
	if _, _, err := formalize(q, n, "approval.2", TargetReplace, 9, documentOf(q, n, "QT-2", "two")); code(err) !=
		"APPROVAL_REFERENCE_MISMATCH at /args/approval_id" {
		t.Fatalf("another expected active revision: %s", code(err))
	}
	slotted := *q
	slotted.Slot = &Slot{Slot: SlotFullV1, Revision: 1}
	if err := slotted.CheckConversion(TargetReplace); code(err) != "ROOT_CONVERSION_LOCKED at /args/quote_id" {
		t.Fatalf("replace a converted quote: %s", code(err))
	}
	if _, _, err := slotted.Formalize(Formalize{Release: Release{Revision: n, Target: TargetReplace, ExpectedActive: 1, ApprovalID: "approval.2"},
		Document: documentOf(q, n, "QT-2", "two")}, q.Approval("approval.2"), human("activator"), 4, at); code(err) !=
		"ROOT_CONVERSION_LOCKED at /args/quote_id" {
		t.Fatalf("Formalize a converted quote: %s", code(err))
	}
	if q.CheckConversion(TargetReplace) != nil || slotted.CheckConversion(TargetActivate) != nil {
		t.Fatal("only a taken slot locks, and only a replacement")
	}
	rev, replaced, err := formalize(q, n, "approval.2", TargetReplace, 1, documentOf(q, n, "QT-2", "two"))
	if err != nil || rev.Status != Active || replaced == nil || replaced.Number != 1 || replaced.Status != Superseded || !replaced.StatusOnly ||
		rev.Formal.Commit.Replaced != 1 || rev.Formal.Commit.Command != TargetReplace || q.Active() != rev {
		t.Fatalf("replace %+v %+v, %v", rev, replaced, err)
	}
	if replaced.Formal == nil || replaced.Formal.Document.Number != "QT-1" {
		t.Fatal("the superseded revision keeps its formal document")
	}
	if !slices.ContainsFunc(rev.Formal.Commit.Entries, func(e Entry) bool { return e.Name == "replaced_revision" && e.Value == "1" }) {
		t.Fatalf("entries %v", rev.Formal.Commit.Entries)
	}
}

// Python test_t03_quote_review.py test_withdraw_closes_open_review_and_invalidates_revision
// (PYT-0441, domain half) and test_t03_formalization.py
// test_cancel_clears_current_pointer_without_deleting_formal_history (PYT-0417, domain half):
// withdrawing a pending revision cancels its open review and its approval never releases it; a
// frozen revision is withdrawn too, an active or working one is not; the active revision is
// cancelled with its formal document kept, another revision is QUOTE_NOT_CURRENT, a converted root
// ROOT_CONVERSION_LOCKED.
func TestWithdrawAndCancel(t *testing.T) {
	q := frozen(t, "8", "0.1")
	if _, err := q.SubmitReview(ReviewRequest{ID: "review.1", Revision: 1, Target: TargetActivate}, human("submitter"), 1, issuedAt); err != nil {
		t.Fatal(err)
	}
	before := q.RootVersion
	rev, review, err := q.Withdraw(1, human("withdrawer"), 5, at, "客户撤回询价", proof)
	if err != nil || rev.Status != Withdrawn || review == nil || review.Status != ReviewCancelled || review.Decided.By != human("withdrawer") ||
		review.Decided.Reason != "客户撤回询价" || rev.Closed == nil || rev.Closed.Status != Withdrawn || rev.Closed.RootVersion != before+1 ||
		q.RootVersion != before+1 || q.ClosedAt(q.RootVersion) != rev {
		t.Fatalf("withdraw %+v %+v, %v", rev, review, err)
	}
	if _, _, err = q.Withdraw(1, human("withdrawer"), 5, at, "again", proof); code(err) != "INVALID_STATE at /args/business_revision" {
		t.Fatalf("withdraw twice: %s", code(err))
	}
	if _, _, err = q.Withdraw(7, human("withdrawer"), 5, at, "r", proof); code(err) != "REFERENCE_INVALID at /args/business_revision" {
		t.Fatalf("an unknown revision: %s", code(err))
	}

	q = frozen(t, "8", "0.1")
	n := frozenNext(t, q)
	approvedFor(t, q, 1, "review.1", "approval.1", TargetActivate, 0)
	if _, _, err = q.Withdraw(n, human("withdrawer"), 5, at, "不再需要", proof); err != nil || q.Revision(n).Status != Withdrawn {
		t.Fatalf("withdraw a frozen revision: %v", err)
	}
	if _, err = q.Revise(1, human("author"), 1, issuedAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err = q.Withdraw(q.Working().Number, human("withdrawer"), 5, at, "r", proof); code(err) != "INVALID_STATE at /args/business_revision" {
		t.Fatalf("withdraw a working revision: %s", code(err))
	}
	if _, _, err = formalize(q, 1, "approval.1", TargetActivate, 0, documentOf(q, 1, "QT-1", "one")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = q.Withdraw(1, human("withdrawer"), 5, at, "r", proof); code(err) != "INVALID_STATE at /args/business_revision" {
		t.Fatalf("withdraw the active revision: %s", code(err))
	}
	if _, err = q.Cancel(n, human("canceller"), 5, at, "r", proof); code(err) != "QUOTE_NOT_CURRENT at /args/business_revision" {
		t.Fatalf("cancel a revision that is not active: %s", code(err))
	}
	slotted := *q
	slotted.Slot = &Slot{Slot: SlotFullV1, Revision: 1}
	if _, err = slotted.Cancel(1, human("canceller"), 5, at, "r", proof); code(err) != "ROOT_CONVERSION_LOCKED at /args/quote_id" {
		t.Fatalf("cancel a converted quote: %s", code(err))
	}
	before = q.RootVersion
	rev, err = q.Cancel(1, human("canceller"), 5, at, "客户取消", proof)
	if err != nil || rev.Status != Cancelled || q.Active() != nil || rev.Formal == nil || rev.Closed.Status != Cancelled ||
		rev.Closed.Closed.Reason != "客户取消" || q.RootVersion != before+1 {
		t.Fatalf("cancel %+v, %v", rev, err)
	}
	if err = q.Usable(1, "", "/x"); code(err) != "QUOTE_NOT_CURRENT at /x" {
		t.Fatalf("a cancelled revision: %s", code(err))
	}
}

// SM-TRD-APPROVAL 生效后撤销登记风险暂停 and UL TRD-RiskHold: revoking the approval the active revision was
// formalized with registers a risk hold in the same command; the revision stays active, its history
// unchanged, but no longer serves new business (APPROVAL_REVOKED) and its conversion slot cannot be
// taken; nothing lifts the hold, a replacement ends it. Revoking another approval (of a pending
// revision, or a replacement's after the revision it formalized was superseded) registers none.
func TestRevokingTheFormalizationApprovalRegistersARiskHold(t *testing.T) {
	q := frozen(t, "8", "0.1")
	approvedFor(t, q, 1, "review.1", "approval.1", TargetActivate, 0)
	rev, _, err := formalize(q, 1, "approval.1", TargetActivate, 0, documentOf(q, 1, "QT-1", "one"))
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Usable(1, rev.Seal.BusinessHash, "/x"); err != nil {
		t.Fatalf("usable before: %v", err)
	}
	if err = q.Usable(1, hashOf("other"), "/x"); code(err) != "BUSINESS_HASH_MISMATCH at /x" {
		t.Fatalf("another hash: %s", code(err))
	}
	n := frozenNext(t, q)
	approvedFor(t, q, n, "review.2", "approval.2", TargetReplace, 1)
	if _, hold, err := q.Revoke(Revocation{ApprovalID: "approval.2", ExpectedEffectivity: 1, Reason: "r", Evidence: proof}, human("reviewer"), 3,
		at); err != nil || hold != nil || len(q.Holds) != 0 {
		t.Fatalf("revoking a pending revision's approval: %+v, %v", hold, err)
	}
	before := q.RootVersion
	a, hold, err := q.Revoke(Revocation{ApprovalID: "approval.1", ExpectedEffectivity: 1, Reason: "客户信用变化", Evidence: proof},
		human("reviewer"), 3, at)
	if err != nil || a.Effectivity != Revoked || hold == nil || hold.ID != "approval_revoked:approval.1" || hold.Revision != 1 ||
		hold.Code != CodeApprovalRevoked || hold.Cause != HoldApprovalRevoked || hold.RootVersion != before+1 || !hold.Fresh ||
		q.Hold(1) != hold || q.RootVersion != before+1 {
		t.Fatalf("revoke %+v, %+v, %v", a, hold, err)
	}
	if rev.Status != Active || q.Active() != rev {
		t.Fatal("the held revision stays active")
	}
	if err = q.Usable(1, "", "/x"); code(err) != "APPROVAL_REVOKED at /x" {
		t.Fatalf("a held revision: %s", code(err))
	}
	if _, err = q.TakeSlot(1, rev.Seal.BusinessHash, "order.1", human("clerk"), 3, at); code(err) != "APPROVAL_REVOKED at /args/business_revision" {
		t.Fatalf("convert a held revision: %s", code(err))
	}
	// A replacement on a new approval ends the held revision.
	q2 := frozen(t, "8", "0.1")
	approvedFor(t, q2, 1, "review.1", "approval.1", TargetActivate, 0)
	if _, _, err = formalize(q2, 1, "approval.1", TargetActivate, 0, documentOf(q2, 1, "QT-1", "one")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = q2.Revoke(Revocation{ApprovalID: "approval.1", ExpectedEffectivity: 1, Reason: "r", Evidence: proof}, human("reviewer"), 3, at); err != nil {
		t.Fatal(err)
	}
	m := frozenNext(t, q2)
	approvedFor(t, q2, m, "review.2", "approval.2", TargetReplace, 1)
	if _, _, err = formalize(q2, m, "approval.2", TargetReplace, 1, documentOf(q2, m, "QT-2", "two")); err != nil {
		t.Fatalf("replace a held revision: %v", err)
	}
	if err = q2.Usable(m, "", "/x"); err != nil || q2.Usable(1, "", "/x") == nil {
		t.Fatalf("after the replacement: %v", err)
	}
}

// TRD-INV-010 (BC-AT15 domain half): the order step takes the conversion slot once for the active,
// unheld revision at its hash; a second conversion is CONVERSION_SLOT_CONFLICT, a revision that is
// not active QUOTE_NOT_CURRENT, another hash BUSINESS_HASH_MISMATCH; the root version moves.
func TestConversionSlotIsTakenOnce(t *testing.T) {
	q := frozen(t, "8", "0.1")
	n := frozenNext(t, q)
	approvedFor(t, q, 1, "review.1", "approval.1", TargetActivate, 0)
	if _, err := q.TakeSlot(1, q.Revision(1).Seal.BusinessHash, "order.1", human("clerk"), 3, at); code(err) !=
		"REFERENCE_INVALID at /args/business_revision" {
		t.Fatalf("convert a revision never formalized: %s", code(err))
	}
	rev, _, err := formalize(q, 1, "approval.1", TargetActivate, 0, documentOf(q, 1, "QT-1", "one"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.TakeSlot(1, hashOf("other"), "order.1", human("clerk"), 3, at); code(err) != "BUSINESS_HASH_MISMATCH at /args/business_revision" {
		t.Fatalf("another hash: %s", code(err))
	}
	if _, err = q.TakeSlot(n, q.Revision(n).Seal.BusinessHash, "order.1", human("clerk"), 3, at); code(err) !=
		"REFERENCE_INVALID at /args/business_revision" {
		t.Fatalf("a frozen revision: %s", code(err))
	}
	before := q.RootVersion
	s, err := q.TakeSlot(1, rev.Seal.BusinessHash, "order.1", human("clerk"), 3, at)
	if err != nil || s.Slot != SlotFullV1 || s.OrderID != "order.1" || s.RootVersion != before+1 || q.Slot != s || !s.Fresh {
		t.Fatalf("take %+v, %v", s, err)
	}
	if _, err = q.TakeSlot(1, rev.Seal.BusinessHash, "order.2", human("clerk"), 3, at); code(err) != "CONVERSION_SLOT_CONFLICT at /args/quote_id" {
		t.Fatalf("a second conversion: %s", code(err))
	}
	if err = q.Usable(1, rev.Seal.BusinessHash, "/x"); err != nil {
		t.Fatalf("a converted revision stays active and usable: %v", err)
	}
}

// Python _current_sales_fx (T03 A32, PYT-0383 at the formalization): the sales exchange rate is
// at most the pricing policy's maximum age old at the formalization's instant, the boundary
// included, and never observed after it.
func TestSalesFXAgeAtFormalization(t *testing.T) {
	observed := issuedAt
	for name, c := range map[string]struct {
		at     time.Time
		maxAge time.Duration
		want   string
	}{
		"at the maximum age":        {issuedAt.Add(time.Hour), time.Hour, "OK"},
		"a microsecond beyond it":   {issuedAt.Add(time.Hour + time.Microsecond), time.Hour, "FX_STALE at /data/sales_fx_ref"},
		"observed after the use":    {issuedAt.Add(-time.Second), time.Hour, "FX_STALE at /data/sales_fx_ref"},
		"no maximum in the policy":  {issuedAt, 0, "INVALID_KNOWN_VALUE at /data/sales_fx_ref"},
		"observed at the same time": {issuedAt, time.Second, "OK"},
	} {
		if got := code(FreshFX(observed, c.maxAge, c.at, "/data/sales_fx_ref")); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	if got := code(FreshFX(time.Time{}, time.Hour, issuedAt, "/p")); got != "FX_STALE at /p" {
		t.Errorf("no observation: %s", got)
	}
}

// The owner's decision of 2026-11-11: a revision a live (valid or disputed) acceptance accepts is
// neither replaced (CheckConversion, Formalize), cancelled nor withdrawn — ACCEPTANCE_ALREADY_EXISTS —
// while the conversion slot, when taken, refuses first (ROOT_CONVERSION_LOCKED); once no live
// acceptance accepts it, the revision ends as before.
func TestAcceptedRevisionIsNotEnded(t *testing.T) {
	q := frozen(t, "8", "0.1")
	approvedFor(t, q, 1, "review.1", "approval.1", TargetActivate, 0)
	if _, _, err := formalize(q, 1, "approval.1", TargetActivate, 0, documentOf(q, 1, "QT-1", "one")); err != nil {
		t.Fatal(err)
	}
	n := frozenNext(t, q)
	approvedFor(t, q, n, "review.2", "approval.2", TargetReplace, 1)
	q.Contracts = map[int64][]string{1: {"acceptance.1"}}
	if err := q.CheckConversion(TargetReplace); code(err) != "ACCEPTANCE_ALREADY_EXISTS at /args/expected_active_revision" {
		t.Fatalf("replace an accepted revision: %s", code(err))
	}
	if _, _, err := formalize(q, n, "approval.2", TargetReplace, 1, documentOf(q, n, "QT-2", "two")); code(err) !=
		"ACCEPTANCE_ALREADY_EXISTS at /args/expected_active_revision" {
		t.Fatalf("Formalize a replacement of an accepted revision: %s", code(err))
	}
	if _, err := q.Cancel(1, human("canceller"), 5, at, "r", proof); code(err) != "ACCEPTANCE_ALREADY_EXISTS at /args/business_revision" {
		t.Fatalf("cancel an accepted revision: %s", code(err))
	}
	if err := q.CheckContract(n, "/x"); err != nil {
		t.Fatalf("the pending revision has no contract: %v", err)
	}
	slotted := *q
	slotted.Slot = &Slot{Slot: SlotFullV1, Revision: 1}
	if err := slotted.CheckConversion(TargetReplace); code(err) != "ROOT_CONVERSION_LOCKED at /args/quote_id" {
		t.Fatalf("the slot first: %s", code(err))
	}
	if q.Active().Number != 1 || q.Revision(n).Status != PendingApproval {
		t.Fatal("refusals changed the revisions")
	}
	q.Contracts = map[int64][]string{}
	if _, replaced, err := formalize(q, n, "approval.2", TargetReplace, 1, documentOf(q, n, "QT-2", "two")); err != nil || replaced.Number != 1 {
		t.Fatalf("replace once the acceptance is invalidated: %v", err)
	}
}

// TRD-INV-014 (正式化批准未撤销、审核人仍合格) and Python test_t03_a26 PYT-0232–0234, 0239, 0241, 0242,
// and test_reviewer_independence_is_rechecked (PYT-0252): an order's activation re-checks the
// approval the revision's commit evidence names — not revoked, its reviewer still independent
// (INDEPENDENT_REVIEW_REQUIRED when the reviewer is one of the revision's authors or the review's
// requester; Python rewrites the stored reviewer, which Go's immutable approval rows refuse, so the
// domain test sets it) and still qualified — but neither the approval's nor the quote's expiry (the
// contract formed before; ADR-BR-013).
func TestFormalApprovalAtAnOrdersActivation(t *testing.T) {
	q := frozen(t, "8", "0.1")
	if _, err := q.FormalApproval(1, qualified(nil), "/x"); code(err) != "REFERENCE_INVALID at /x" {
		t.Fatalf("a revision never formalized: %s", code(err))
	}
	approvedFor(t, q, 1, "review.1", "approval.1", TargetActivate, 0)
	if _, _, err := formalize(q, 1, "approval.1", TargetActivate, 0, documentOf(q, 1, "QT-1", "one")); err != nil {
		t.Fatal(err)
	}
	reviewer := q.Approval("approval.1").Approved.By.ID
	if a, err := q.FormalApproval(1, qualified(nil), "/x"); err != nil || a.ID != "approval.1" {
		t.Fatalf("the formalization approval %+v %v", a, err)
	}
	approval := q.Approval("approval.1")
	named := authors(q.Revision(1), q.Review(approval.ReviewID))
	if len(named) < 2 {
		t.Fatalf("the revision's authors and requester %v", named)
	}
	for _, author := range named {
		approval.Approved.By = human(author)
		if _, err := q.FormalApproval(1, qualified(nil), "/x"); code(err) != "INDEPENDENT_REVIEW_REQUIRED at /x" {
			t.Errorf("the reviewer is an author, %s (PYT-0252): %s", author, code(err))
		}
	}
	approval.Approved.By = human(reviewer)
	for name, s := range map[string]Standing{
		"without quote.review.decide (PYT-0232)": {Human: true, SeesRoot: true, Permissions: []string{PermissionRead}},
		"no longer human (PYT-0233)":             {SeesRoot: true, Permissions: []string{PermissionReview, PermissionRead}},
		"no longer seeing the quote (PYT-0234)":  {Human: true, Permissions: []string{PermissionReview, PermissionRead}},
	} {
		if _, err := q.FormalApproval(1, qualified(map[string]Standing{reviewer: s}), "/x"); code(err) != "APPROVAL_STALE at /x" {
			t.Errorf("a reviewer %s: %s", name, code(err))
		}
	}
	if _, _, err := q.Revoke(Revocation{ApprovalID: "approval.1", ExpectedEffectivity: 1, Reason: "r", Evidence: proof}, human("revoker"), 5, at); err != nil {
		t.Fatal(err)
	}
	if _, err := q.FormalApproval(1, qualified(nil), "/x"); code(err) != "APPROVAL_REVOKED at /x" {
		t.Fatalf("a revoked formalization approval: %s", code(err))
	}
}
