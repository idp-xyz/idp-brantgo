//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// GoalReadiness v1 (Context Map R18 TRD→TSK; WBS S4-01c; product baseline 15.4 完成断言) on the TRD
// order world: the Q01 assertions a goal requires, each satisfied, unsatisfied, unknown or error with
// its checks and evidence, judged on TRD's records now. Tests port the goal vectors of the Q01 V0.1
// package (docs/90_ORIGINAL_SOURCES/04_Q01_P0_契约与执行验证包_V0.1.zip, tests/test_execution.py),
// cited as "Q01 V0.1 tests/test_execution.py::<name>"; TRD answers the assertions, TSK's goal.verify
// (unit B2) decides the task's state from them.

var quoteReadyAssertions = []string{trdapi.AssertionScope, trdapi.AssertionQuote, trdapi.AssertionLines, trdapi.AssertionApproval,
	trdapi.AssertionDocument, trdapi.AssertionIndustry}

var orderActiveAssertions = append(slices.Clone(quoteReadyAssertions), trdapi.AssertionAcceptance, trdapi.AssertionOrder)

// goal judges goal on quote id as ctx (version 1, no target).
func (q *trdWorld) goal(ctx context.Context, goal, id string) trdapi.GoalAssessment {
	q.t.Helper()
	a, err := q.c.TRD.GoalReadiness(ctx, trdapi.GoalQuery{Goal: goal, GoalVersion: trdapi.GoalVersion, QuoteID: id})
	q.must(err)
	return a
}

// assertionsOf are a's assertions as "assertion=answer" or "assertion=answer:code", in order.
func assertionsOf(a trdapi.GoalAssessment) []string {
	out := []string{}
	for _, x := range a.Assertions {
		s := x.Assertion + "=" + x.Answer
		if x.Code != "" {
			s += ":" + x.Code
		}
		out = append(out, s)
	}
	return out
}

// goalNames are the assertions of a, in order.
func goalNames(a trdapi.GoalAssessment) []string {
	out := []string{}
	for _, x := range a.Assertions {
		out = append(out, x.Assertion)
	}
	return out
}

// satisfiedGoal fails unless a is goal satisfied with every assertion of want satisfied.
func satisfiedGoal(t *testing.T, a trdapi.GoalAssessment, goal string, want []string) {
	t.Helper()
	if a.Goal != goal || a.GoalVersion != 1 || a.Outcome != trdapi.AnswerSatisfied || a.Code != "" || len(a.Unmet()) != 0 ||
		!slices.Equal(goalNames(a), want) || a.CheckedAt.IsZero() {
		t.Fatalf("%s: %+v %v", goal, a, assertionsOf(a))
	}
	for _, x := range a.Assertions {
		if len(x.Checks) == 0 || slices.ContainsFunc(x.Checks, func(c trdapi.GoalCheck) bool { return c.Answer != trdapi.AnswerSatisfied }) {
			t.Fatalf("%s %s: checks %+v", goal, x.Assertion, x.Checks)
		}
	}
}

// checkOf returns check of assertion code of a ("" Answer when absent).
func checkOf(a trdapi.GoalAssessment, assertion, check string) trdapi.GoalCheck {
	x, _ := a.Assertion(assertion)
	for _, c := range x.Checks {
		if c.Check == check {
			return c
		}
	}
	return trdapi.GoalCheck{}
}

// goalEvidence returns the evidence of kind of assertion code of a.
func goalEvidence(a trdapi.GoalAssessment, assertion, kind string) []trdapi.EvidenceRef {
	x, _ := a.Assertion(assertion)
	var out []trdapi.EvidenceRef
	for _, e := range x.Evidence {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// Q01 V0.1 tests/test_execution.py::test_quote_ready_is_a_different_goal and
// ::test_quote_only_cannot_complete_order_goal; baseline 15.4; X-INV-003; R18's risk (never the
// model's word): the activated quote satisfies quote_ready — scope, quote, lines, approval, document
// and industry, each with its evidence (the revision at its business hash, the commit evidence, the
// cost snapshot and RFQ, the approval, the formal document, the SKU specification) — and names no
// order; the same quote does not satisfy order_active (ORDER_MISSING on the order, and no
// acceptance, approval or lines of an order). The goal is the same for a reader holding trd.read
// alone, an AI agent and TSK's system actor; it is not fenced by the root's object control (bound,
// stopping and manual alike, without a claim); the target customer and RFQ must be the revision's;
// a malformed query, a revision the root does not have, a missing quote and a caller without
// trd.read are refused and audited; no read changes TRD's business state.
func TestGoalReadinessJudgesQuoteReady(t *testing.T) {
	q := newOrderWorld(t)
	act := q.activated("quote.1")
	before := q.business()
	a := q.goal(q.author, trdapi.GoalQuoteReady, "quote.1")
	satisfiedGoal(t, a, trdapi.GoalQuoteReady, quoteReadyAssertions)
	if a.QuoteID != "quote.1" || a.BusinessRevision != 1 || a.BusinessHash == "" || a.OrderID != "" || a.OrderRevision != 0 {
		t.Fatalf("judged records %+v", a)
	}
	rev := goalEvidence(a, trdapi.AssertionQuote, trdapi.EvidenceQuoteRevision)
	doc := goalEvidence(a, trdapi.AssertionDocument, trdapi.EvidenceDocument)
	spec := goalEvidence(a, trdapi.AssertionIndustry, trdapi.EvidenceSkuSpec)
	approval := goalEvidence(a, trdapi.AssertionApproval, trdapi.EvidenceApproval)
	snap := goalEvidence(a, trdapi.AssertionLines, trdapi.EvidenceCostSnapshot)
	if len(rev) != 1 || rev[0] != (trdapi.EvidenceRef{Kind: trdapi.EvidenceQuoteRevision, ID: "quote.1", Version: 1, Hash: a.BusinessHash}) ||
		len(doc) != 1 || doc[0].ID != act.Number || doc[0].Hash != act.DocumentSHA256 || len(spec) != 1 || spec[0].ID != "sku.1" ||
		spec[0].Version != q.spec || len(approval) != 1 || approval[0].ID != "approval.quote.1" || approval[0].Version != 1 ||
		len(snap) != 1 || snap[0].ID != q.snap.SnapshotID || snap[0].Hash != q.snap.ResultHash ||
		len(goalEvidence(a, trdapi.AssertionQuote, trdapi.EvidenceCommit)) != 1 || len(goalEvidence(a, trdapi.AssertionQuote, trdapi.EvidenceConversionSlot)) != 0 {
		t.Fatalf("evidence %+v", a.Assertions)
	}
	if c := checkOf(a, trdapi.AssertionQuote, trdapi.CheckConversion); c.Answer != trdapi.AnswerSatisfied {
		t.Fatalf("a free conversion slot: %+v", c)
	}

	// order_active is another goal: the quote alone does not satisfy it.
	o := q.goal(q.author, trdapi.GoalOrderActive, "quote.1")
	if o.Outcome != trdapi.AnswerUnsatisfied || o.Code != trdapi.CodeRequiredCheckUnsatisfied || o.OrderID != "" ||
		!slices.Equal(goalNames(o), orderActiveAssertions) ||
		!slices.Equal(o.Unmet(), []string{trdapi.AssertionLines, trdapi.AssertionApproval, trdapi.AssertionAcceptance, trdapi.AssertionOrder}) {
		t.Fatalf("order_active on a quote: %+v %v", o, assertionsOf(o))
	}
	if x, _ := o.Assertion(trdapi.AssertionOrder); x.Code != trdapi.CodeOrderMissing || x.Answer != trdapi.AnswerUnsatisfied {
		t.Fatalf("Q01.order %+v", x)
	}

	// Every reader holding trd.read sees the same goal: TSK's system actor verifies it, an AI agent
	// reads it.
	reader := q.provision(grantOf(q, "reader.1", kernel.ActorHuman, "company", "trd.read"))
	for name, ctx := range map[string]context.Context{"trd.read alone": reader, "an AI agent": q.actorOf(kernel.ActorAI, "agent.goal"),
		"TSK's system actor": q.actorOf(kernel.ActorSystem, "tsk.goal")} {
		if got := q.goal(ctx, trdapi.GoalQuoteReady, "quote.1"); got.Outcome != trdapi.AnswerSatisfied {
			t.Errorf("%s: %v", name, assertionsOf(got))
		}
	}

	// The target the goal names must be the revision's.
	for _, c := range []struct {
		query trdapi.GoalQuery
		want  string
	}{
		{trdapi.GoalQuery{CustomerID: "customer.1", RfqID: "rfq.1", BusinessRevision: 1}, ""},
		{trdapi.GoalQuery{CustomerID: "customer.9"}, "/customer_id"},
		{trdapi.GoalQuery{RfqID: "rfq.9"}, "/rfq_id"},
	} {
		c.query.Goal, c.query.GoalVersion, c.query.QuoteID = trdapi.GoalQuoteReady, 1, "quote.1"
		got, err := q.c.TRD.GoalReadiness(q.author, c.query)
		q.must(err)
		scope, _ := got.Assertion(trdapi.AssertionScope)
		switch {
		case c.want == "" && got.Outcome != trdapi.AnswerSatisfied:
			t.Errorf("%+v: %v", c.query, assertionsOf(got))
		case c.want != "" && (got.Outcome != trdapi.AnswerUnsatisfied || scope.Code != kernel.CodeReferenceInvalid ||
			!slices.ContainsFunc(scope.Checks, func(k trdapi.GoalCheck) bool { return k.Path == c.want })):
			t.Errorf("%+v: %v %+v", c.query, assertionsOf(got), scope.Checks)
		}
	}

	// Refusals, audited (X-INV-003).
	for _, c := range []struct {
		ctx   context.Context
		query trdapi.GoalQuery
		want  string
	}{
		{q.author, trdapi.GoalQuery{Goal: "quote_sent", GoalVersion: 1, QuoteID: "quote.1"}, kernel.CodeSchemaInvalid},
		{q.author, trdapi.GoalQuery{Goal: trdapi.GoalQuoteReady, GoalVersion: 2, QuoteID: "quote.1"}, kernel.CodeSchemaInvalid},
		{q.author, trdapi.GoalQuery{Goal: trdapi.GoalOrderActive, GoalVersion: 1, QuoteID: "quote.1", BusinessRevision: 9}, kernel.CodeNotFound},
		{q.author, trdapi.GoalQuery{Goal: trdapi.GoalQuoteReady, GoalVersion: 1, QuoteID: "quote.missing"}, kernel.CodeNotFound},
		{q.provision(grantOf(q, "writer.1", kernel.ActorHuman, "company", "quote.save_draft")),
			trdapi.GoalQuery{Goal: trdapi.GoalQuoteReady, GoalVersion: 1, QuoteID: "quote.1"}, kernel.CodePermissionDenied},
		{q.provision(grantOf(q, "outsider.1", kernel.ActorHuman, "own", "trd.read")),
			trdapi.GoalQuery{Goal: trdapi.GoalQuoteReady, GoalVersion: 1, QuoteID: "quote.1"}, kernel.CodeNotFound},
	} {
		if _, err := q.c.TRD.GoalReadiness(c.ctx, c.query); code(err) != c.want {
			t.Errorf("%+v: %s, want %s", c.query, code(err), c.want)
		}
	}
	for _, c := range []struct{ object, outcome, code string }{{"quote.1", "read", ""}, {"quote.1", "rejected", kernel.CodeSchemaInvalid},
		{"quote.1", "rejected", kernel.CodeNotFound}, {"quote.missing", "rejected", kernel.CodeNotFound},
		{"quote.1", "rejected", kernel.CodePermissionDenied}} {
		if !q.audited(trdapi.QueryGoalReadiness, c.object, c.outcome, c.code) {
			t.Errorf("no audit entry %+v", c)
		}
	}

	// The root's object control does not fence the read (bound, stopping, manual; no claim).
	_, err := q.bindTask(q.author, "quote.1", "task.1", 0)
	q.must(err)
	if r := q.readiness(q.author, "quote.1", 0); r.Ready {
		t.Fatal("quote_ready (the write-side readiness) ready on a bound root without the claim")
	}
	check := func(state string) {
		t.Helper()
		if got := q.goal(q.author, trdapi.GoalQuoteReady, "quote.1"); got.Outcome != trdapi.AnswerSatisfied {
			t.Fatalf("%s root: %v", state, assertionsOf(got))
		}
	}
	check("bound")
	_, err = q.c.TRD.StopControl(q.claimFor(q.author, "task.1", 1), trdapi.StopControl{IdempotencyKey: q.key(), QuoteID: "quote.1",
		ExpectedControlEpoch: 1, Reason: "人工接管"})
	q.must(err)
	check("stopping")
	_, err = q.c.TRD.EnterManualControl(q.claimFor(q.author, "task.1", 2), trdapi.EnterManualControl{IdempotencyKey: q.key(),
		QuoteID: "quote.1", ExpectedControlEpoch: 2, Reason: "屏障完成，转人工"})
	q.must(err)
	check("manual")
	if q.business() == before {
		t.Fatal("the control changes did not change the business state")
	}
	before = q.business()

	// The revision's upstream facts and approval are judged now: a CST source denial lapses the cost
	// snapshot (Q01.lines), a revoked approval holds the revision (Q01.quote's risk hold, Q01.approval),
	// a revised SKU specification is no longer the confirmed technical data (Q01.industry).
	q.deny()
	got := q.goal(q.author, trdapi.GoalQuoteReady, "quote.1")
	if got.Outcome != trdapi.AnswerUnsatisfied || !slices.Equal(got.Unmet(), []string{trdapi.AssertionLines}) ||
		checkOf(got, trdapi.AssertionLines, trdapi.CheckUpstream).Code != cstapi.CodeCostSnapshotIneligible {
		t.Fatalf("after the denial: %v %+v", assertionsOf(got), got.Assertions[2].Checks)
	}
	sku, err := q.c.CAT.GetSku(q.author, "sku.1")
	q.must(err)
	_, err = q.c.CAT.ReviseSpec(q.author, catapi.ReviseSpec{IdempotencyKey: q.key(), SkuID: "sku.1", ExpectedVersion: sku.RowVersion, Reason: "合成技术变更"})
	q.must(err)
	got = q.goal(q.author, trdapi.GoalQuoteReady, "quote.1")
	if c := checkOf(got, trdapi.AssertionIndustry, trdapi.CheckIndustry); c.Answer != trdapi.AnswerUnsatisfied || c.Path != "/data/lines/0/sku_ref" {
		t.Fatalf("after the SKU revision: %v %+v", assertionsOf(got), c)
	}
	_, err = q.c.TRD.RevokeApproval(q.claimFor(q.reviewer, "task.1", 3), trdapi.RevokeApproval{IdempotencyKey: q.key(), QuoteID: "quote.1",
		ApprovalID: "approval.quote.1", ExpectedEffectivityVersion: 1, Reason: "客户信用恶化", Evidence: q.proof})
	q.must(err)
	got = q.goal(q.author, trdapi.GoalQuoteReady, "quote.1")
	if checkOf(got, trdapi.AssertionQuote, trdapi.CheckRiskHold).Code != trdapi.CodeApprovalRevoked ||
		checkOf(got, trdapi.AssertionApproval, trdapi.CheckApproval).Code != trdapi.CodeApprovalRevoked ||
		!slices.Equal(got.Unmet(), []string{trdapi.AssertionQuote, trdapi.AssertionLines, trdapi.AssertionApproval, trdapi.AssertionIndustry}) {
		t.Fatalf("after the revocation: %v", assertionsOf(got))
	}
	if after := q.business(); after == before {
		t.Fatal("the revocation did not change the business state")
	}
	snapshot := q.business()
	q.goal(q.author, trdapi.GoalOrderActive, "quote.1")
	q.goal(q.author, trdapi.GoalQuoteReady, "quote.1")
	if q.business() != snapshot {
		t.Fatal("a goal read changed TRD's business state")
	}
}

// Q01 V0.1 tests/test_execution.py::test_draft_order_cannot_complete; baseline 15.4 (Q01.order 仅有草稿
// 不满足; full_v1 已占用时返回已有订单或明确冲突); BRC-001 observation 4 and the S3 gap "quote_ready
// still ready after the conversion slot is taken": once order.prepare took the root's full_v1 slot,
// quote_ready is no longer satisfied (Q01.quote conversion: CONVERSION_SLOT_CONFLICT with the order
// as evidence) and QuoteReady reports it too; order_active names the draft order and is not
// satisfied (ORDER_NOT_ACTIVE, ORDER_APPROVAL_REQUIRED) while the contract is (Q01.acceptance). The
// activated order satisfies order_active with the order, its review, the acceptance and the revision
// as evidence, for the author, an AI agent and TSK's system actor; a revoked order approval makes
// Q01.approval unsatisfied (ORDER_APPROVAL_REVOKED, as OrderGuard v1 reports it to PUR).
func TestGoalReadinessJudgesOrderActive(t *testing.T) {
	q := newOrderWorld(t)
	acc, o := q.prepared("quote.1")
	ready := q.goal(q.author, trdapi.GoalQuoteReady, "quote.1")
	conv := checkOf(ready, trdapi.AssertionQuote, trdapi.CheckConversion)
	slot := goalEvidence(ready, trdapi.AssertionQuote, trdapi.EvidenceConversionSlot)
	if ready.Outcome != trdapi.AnswerUnsatisfied || !slices.Equal(ready.Unmet(), []string{trdapi.AssertionQuote}) ||
		conv.Answer != trdapi.AnswerUnsatisfied || conv.Code != trdapi.CodeConversionSlotConflict || len(slot) != 1 ||
		slot[0].ID != o.ObjectID || slot[0].Version != 1 || slot[0].Hash != o.BusinessHash {
		t.Fatalf("quote_ready after the conversion: %v %+v %+v", assertionsOf(ready), conv, slot)
	}
	if r := q.readiness(q.author, "quote.1", 0); r.Ready || !slices.Contains(reasonsOf(r.Reasons), "conversion:CONVERSION_SLOT_CONFLICT") {
		t.Fatalf("QuoteReady after the conversion: %+v", r)
	}
	draft := q.goal(q.author, trdapi.GoalOrderActive, "quote.1")
	if draft.Outcome != trdapi.AnswerUnsatisfied || draft.OrderID != o.ObjectID || draft.OrderHash != o.OrderHash ||
		!slices.Equal(draft.Unmet(), []string{trdapi.AssertionApproval, trdapi.AssertionOrder}) ||
		checkOf(draft, trdapi.AssertionOrder, trdapi.CheckOrder).Code != trdapi.CodeOrderNotActive ||
		checkOf(draft, trdapi.AssertionApproval, trdapi.CheckOrderApproval).Code != trdapi.CodeOrderApprovalRequired {
		t.Fatalf("a draft order: %v", assertionsOf(draft))
	}

	d := q.reviewed(o, "review.1")
	act, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1"))
	q.must(err)
	for name, ctx := range map[string]context.Context{"author": q.author, "an AI agent": q.actorOf(kernel.ActorAI, "agent.goal"),
		"TSK's system actor": q.actorOf(kernel.ActorSystem, "tsk.goal")} {
		a := q.goal(ctx, trdapi.GoalOrderActive, "quote.1")
		satisfiedGoal(t, a, trdapi.GoalOrderActive, orderActiveAssertions)
		order := goalEvidence(a, trdapi.AssertionOrder, trdapi.EvidenceOrder)
		review := goalEvidence(a, trdapi.AssertionApproval, trdapi.EvidenceOrderReview)
		accepted := goalEvidence(a, trdapi.AssertionAcceptance, trdapi.EvidenceAcceptance)
		if a.OrderID != o.ObjectID || a.OrderRevision != 1 || a.OrderHash != act.OrderHash || a.BusinessRevision != 1 ||
			a.BusinessHash != acc.BusinessHash || len(order) != 1 || order[0] != (trdapi.EvidenceRef{Kind: trdapi.EvidenceOrder, ID: o.ObjectID,
			Version: 1, Hash: act.OrderHash}) || len(review) != 1 || review[0].ID != "review.1" || len(accepted) != 1 ||
			accepted[0].ID != acc.ObjectID || accepted[0].Hash != acc.BusinessHash {
			t.Fatalf("%s: %+v", name, a)
		}
	}
	_, err = q.c.TRD.RevokeOrderApproval(q.reviewer, trdapi.RevokeOrderApproval{IdempotencyKey: q.key(), OrderID: o.ObjectID,
		ReviewID: "review.1", Reason: "订单审批撤销", Evidence: q.proof})
	q.must(err)
	revoked := q.goal(q.author, trdapi.GoalOrderActive, "quote.1")
	if revoked.Outcome != trdapi.AnswerUnsatisfied || !slices.Equal(revoked.Unmet(), []string{trdapi.AssertionApproval}) ||
		checkOf(revoked, trdapi.AssertionApproval, trdapi.CheckOrderApproval).Code != trdapi.CodeOrderApprovalRevoked {
		t.Fatalf("a revoked order approval: %v", assertionsOf(revoked))
	}
}

// TRD-INV-017, BC-AT13, SC-07 and baseline 15.4 (Q01.quote for order_active: 承诺到达时同左（合同成立）
// …此后报价到期不影响转单): an acceptance that arrived before the quote expired formed the contract;
// the order activated after the expiry satisfies order_active, while quote_ready is no longer
// satisfied (the validity has passed and the conversion slot is taken).
func TestGoalReadinessKeepsATimelyContractAfterTheQuoteExpired(t *testing.T) {
	q := newOrderWorld(t)
	f, expires := q.shortlyActivated("quote.1", 10*time.Second) // 4 s lapsed before activation on a loaded host (sharded gate, 2026-11-13)
	_, err := q.c.TRD.RecordResponse(q.author, q.full("response.1", "quote.1", 1, f.BusinessHash))
	q.must(err)
	acc, err := q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.1", "acceptance.1", 1, f.BusinessHash))
	q.must(err)
	o, err := q.c.TRD.PrepareOrder(q.author, q.prepareCmd("order.1", acc))
	q.must(err)
	time.Sleep(time.Until(expires) + 100*time.Millisecond)
	d := q.reviewed(o, "review.1")
	_, err = q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1"))
	q.must(err)
	satisfiedGoal(t, q.goal(q.author, trdapi.GoalOrderActive, "quote.1"), trdapi.GoalOrderActive, orderActiveAssertions)
	ready := q.goal(q.author, trdapi.GoalQuoteReady, "quote.1")
	if checkOf(ready, trdapi.AssertionQuote, trdapi.CheckValidity).Code != trdapi.CodeQuoteExpired ||
		checkOf(ready, trdapi.AssertionQuote, trdapi.CheckConversion).Code != trdapi.CodeConversionSlotConflict {
		t.Fatalf("quote_ready after the expiry: %v", assertionsOf(ready))
	}
}

// Q01 V0.1 tests/test_execution.py::test_missing_document_blocks_completion and
// ::test_corrupt_order_customer_is_not_success; baseline 15.4 (资料不足和核验不可用均不能当作 pass);
// TRD-INV-009: an integrity failure of the records a goal rests on is an error, never a pass. Formal
// document bytes rewritten with their hash and size (the owner, the document's guard off) are no
// longer the document the order bound (Q01.document error FORMAL_DOCUMENT_INTEGRITY_FAILURE; Python
// DOCUMENT_BINDING_MISMATCH); an order snapshot rewritten to name another customer is not the copy
// of its source (Q01.lines error REFERENCE_INVALID; Python ORDER_QUOTE_MISMATCH). Either way the
// outcome is error with REQUIRED_CHECK_UNKNOWN, so the task does not complete; restored, the goal is
// satisfied again.
func TestGoalReadinessReportsBrokenRecordsAsErrors(t *testing.T) {
	q := newOrderWorld(t)
	q.activeOrder("quote.1")
	satisfiedGoal(t, q.goal(q.author, trdapi.GoalOrderActive, "quote.1"), trdapi.GoalOrderActive, orderActiveAssertions)
	where := ` WHERE tenant_id = '` + q.tenant + `' AND quote_id = 'quote.1' AND revision = 1`
	off := "ALTER TABLE brant_trd.quote_document DISABLE TRIGGER quote_document_guard"
	on := "ALTER TABLE brant_trd.quote_document ENABLE TRIGGER quote_document_guard"
	q.must(oeqvOwner(q, off, `UPDATE brant_trd.quote_document SET content = content || '\x0a'::bytea, size_bytes = size_bytes + 1,
		content_sha256 = encode(sha256(content || '\x0a'::bytea), 'hex')`+where, on))
	broken := q.goal(q.author, trdapi.GoalOrderActive, "quote.1")
	doc, _ := broken.Assertion(trdapi.AssertionDocument)
	if broken.Outcome != trdapi.AnswerError || broken.Code != trdapi.CodeRequiredCheckUnknown ||
		!slices.Equal(broken.Unmet(), []string{trdapi.AssertionDocument}) || doc.Answer != trdapi.AnswerError ||
		doc.Code != trdapi.CodeFormalDocumentIntegrityFailure {
		t.Fatalf("rewritten document bytes: %v %+v", assertionsOf(broken), doc.Checks)
	}
	q.must(oeqvOwner(q, off, `UPDATE brant_trd.quote_document SET content = substring(content from 1 for size_bytes - 1),
		size_bytes = size_bytes - 1, content_sha256 = encode(sha256(substring(content from 1 for size_bytes - 1)), 'hex')`+where, on))
	satisfiedGoal(t, q.goal(q.author, trdapi.GoalOrderActive, "quote.1"), trdapi.GoalOrderActive, orderActiveAssertions)

	orderWhere := ` WHERE tenant_id = '` + q.tenant + `' AND order_id = 'order.quote.1'`
	orderOff := "ALTER TABLE brant_trd.sales_order DISABLE TRIGGER sales_order_guard"
	orderOn := "ALTER TABLE brant_trd.sales_order ENABLE TRIGGER sales_order_guard"
	original := q.scalar(`SELECT snapshot FROM brant_trd.sales_order` + orderWhere)
	if !strings.Contains(original, `"party_id":"customer.1"`) {
		t.Fatalf("the snapshot does not name the customer as expected: %.200s", original)
	}
	corrupt := strings.ReplaceAll(original, `"party_id":"customer.1"`, `"party_id":"wrong.customer"`)
	set := func(snapshot string) {
		t.Helper()
		lit := "'" + strings.ReplaceAll(snapshot, "'", "''") + "'"
		hash := `encode(sha256(convert_to(` + lit + `, 'UTF8')), 'hex')`
		q.must(oeqvOwner(q, orderOff, `UPDATE brant_trd.sales_order SET snapshot = `+lit+`, snapshot_hash = `+hash+`, order_hash = `+hash+
			orderWhere, orderOn))
	}
	set(corrupt)
	broken = q.goal(q.author, trdapi.GoalOrderActive, "quote.1")
	lines, _ := broken.Assertion(trdapi.AssertionLines)
	if broken.Outcome != trdapi.AnswerError || !slices.Contains(broken.Unmet(), trdapi.AssertionLines) || lines.Answer != trdapi.AnswerError ||
		lines.Code != kernel.CodeReferenceInvalid || checkOf(broken, trdapi.AssertionLines, trdapi.CheckOrderSnapshot).Path != "/order/snapshot" {
		t.Fatalf("a snapshot naming another customer: %v %+v", assertionsOf(broken), lines.Checks)
	}
	set(original)
	satisfiedGoal(t, q.goal(q.author, trdapi.GoalOrderActive, "quote.1"), trdapi.GoalOrderActive, orderActiveAssertions)
}
