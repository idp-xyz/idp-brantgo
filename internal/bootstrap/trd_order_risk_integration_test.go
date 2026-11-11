//go:build integration

package bootstrap_test

import (
	"strings"
	"testing"
	"time"

	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SC-07, ES-08, BC-AT13 and TD-AT03 on PostgreSQL 16 (ADR-BR-013, TRD-INV-012, 014, 017), with
// Python test_t03_a26_order_qualification.py
// test_quote_expiration_blocks_uncommitted_order_even_if_acceptance_was_timely (PYT-0235) and
// test_approval_expiry_is_exclusive_and_does_not_change_order (PYT-0236) as deliberate divergences:
// the quote expires seconds after its activation; the customer's acceptance reached the channel
// server before then and formed the contract. The order is prepared, the quote expires, and CST then
// denies a source of the cost snapshot. The order is reviewed after the quote expired: an approval
// that does not acknowledge the denial is ORDER_RISK_UNACKNOWLEDGED, one that acknowledges it is
// approved and the order activates after the quote's expiry; the commit evidence records the
// acceptance's arrival, the risk acknowledged and the approval. A reply that reached the channel
// exactly at expires_at formed nothing (ACCEPTANCE_LATE at verification): there is no acceptance to
// prepare an order on.
func TestOrderActivatesAfterTheQuoteExpiredOnATimelyContract(t *testing.T) {
	q := newOrderWorld(t)
	f, expires := q.shortlyActivated("quote.1", 8*time.Second)
	arrived := time.Now()
	_, err := q.c.TRD.RecordResponse(q.author, q.reply("response.timely", "quote.1", 1, f.BusinessHash, trdapi.ClassificationFullUnconditional,
		arrived, trdapi.ArrivalChannelServer))
	q.must(err)
	a, err := q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.timely", "acceptance.1", 1, f.BusinessHash))
	q.must(err)
	o, err := q.c.TRD.PrepareOrder(q.author, q.prepareCmd("order.1", a))
	q.must(err)
	time.Sleep(time.Until(expires) + 100*time.Millisecond)
	late, err := q.c.TRD.RecordResponse(q.author, q.reply("response.late.2", "quote.1", 1, f.BusinessHash, trdapi.ClassificationFullUnconditional,
		expires, trdapi.ArrivalChannelServer))
	if err != nil || !late.Late {
		t.Fatalf("a reply at expires_at %+v %v", late, err)
	}
	_, err = q.c.TRD.VerifyResponse(q.reviewer, q.verification("response.late.2", "acceptance.late", 1, f.BusinessHash))
	q.want(err, trdapi.CodeAcceptanceLate)
	_, err = q.c.TRD.PrepareOrder(q.author, trdapi.PrepareOrder{IdempotencyKey: q.key(), OrderID: "order.late", QuoteID: "quote.1",
		BusinessRevision: 1, BusinessHash: f.BusinessHash, AcceptanceID: "acceptance.late"})
	q.want(err, trdapi.CodeAcceptanceScopeMismatch)

	q.deny()
	risks := q.risks("order.1")
	if len(risks) != 1 || risks[0].Code != cstapi.CodeCostSnapshotIneligible || risks[0].Path != "/data/cost_snapshot_ref" ||
		!strings.HasPrefix(risks[0].Detail, "/sources/") {
		t.Fatalf("the risks after the denial %+v", risks)
	}
	r, err := q.c.TRD.RequestOrderReview(q.author, q.requestCmd("order.1", 1, "review.1"))
	q.must(err)
	_, err = q.c.TRD.DecideOrderReview(q.reviewer, q.decideCmd("order.1", r.RowVersion, "review.1", o.OrderHash))
	q.want(err, trdapi.CodeOrderRiskUnacknowledged)
	bogus := q.decideCmd("order.1", r.RowVersion, "review.1", o.OrderHash, ack(risks[0]),
		trdapi.RiskAcknowledgement{Path: "/data/currency_ref", Code: "CONFIG_EXPIRED"})
	_, err = q.c.TRD.DecideOrderReview(q.reviewer, bogus)
	q.want(err, kernel.CodeReferenceInvalid)
	d, err := q.c.TRD.DecideOrderReview(q.reviewer, q.decideCmd("order.1", r.RowVersion, "review.1", o.OrderHash, ack(risks[0])))
	q.must(err)
	if c, err := q.c.TRD.CheckOrder(q.author, "order.1", "review.1"); err != nil || !c.Activatable || len(c.Unacknowledged) != 0 {
		t.Fatalf("checked %+v %v", c, err)
	}
	act, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd("order.1", d.RowVersion, "review.1"))
	if err != nil || act.Status != trdapi.OrderActive {
		t.Fatalf("activation after the quote expired %+v %v", act, err)
	}
	v := q.getOrder("order.1")
	e := v.Commits[0].Entries
	if !v.Commits[0].CommittedAt.After(expires) || e["arrival_at"] != kernel.FormatTimestamp(arrived) ||
		e["quote_expires_at"] != kernel.FormatTimestamp(expires) || e["risks_acknowledged"] != "1" ||
		!strings.Contains(e["risks_found"], cstapi.CodeCostSnapshotIneligible) || e["review_id"] != "review.1" ||
		e["cost_snapshot_adoptability"] == cstapi.AdoptionAdoptable {
		t.Fatalf("the commit evidence %v", e)
	}
	if got := v.Reviews[0].Acknowledged; len(got) != 1 || got[0] != risks[0] {
		t.Fatalf("acknowledged %+v", got)
	}
}

// SC-04 and SC-07's failure for the order (ADR-BR-004; TRD-INV-014) with Python test_t03_a26
// test_current_cost_denial_blocks_new_activation_without_residue (PYT-0228) and
// test_cost_denial_committing_first_blocks_waiting_activation (PYT-0254) as deliberate divergences
// (Python refuses; the design makes the denial a risk the approval acknowledges),
// test_retired_template_blocks_new_activation_without_residue (PYT-0229, likewise, with the sales tax
// application), test_retired_cost_pricing_policy_blocks_new_activation (PYT-0256, likewise),
// test_advisory_quote_ready_is_not_used_as_a_write_token (PYT-0244) and
// test_source_checks_use_the_exact_order_write_connection (PYT-0245: the upstream guards run inside
// the activation's own transaction and hold their rows, so a CST denial waits for it; Python checks
// the connection object, which Go has no counterpart of): the approval
// came before the upstream change. An activation holding its guards makes a CST denial wait until it
// commits, and its order stays active; the denial committed first is found by the next activation
// (ORDER_RISK_UNACKNOWLEDGED, nothing written). A new review acknowledging the risk activates that
// order; the acknowledgement must match the risk exactly. A party being revised is a risk on each PTY
// reference, but its screening is no longer clear (a refusal); a retired configuration is a risk
// the approval acknowledges.
func TestUpstreamChangesAfterApprovalAreRisksToAcknowledge(t *testing.T) {
	q := newOrderWorld(t)
	d := newDownstream(q.world)
	_, o1 := q.prepared("quote.1")
	_, o2 := q.prepared("quote.2")
	d1, d2 := q.reviewed(o1, "review.1"), q.reviewed(o2, "review.2")

	// The activation of order.1 holds its guards (it waits at its action result): the denial and
	// the retirement of the sales tax application wait for it.
	cmd := q.activateOrderCmd(o1.ObjectID, d1.RowVersion, "review.1")
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandActivateOrder, cmd.IdempotencyKey)
	done := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.ActivateOrder(q.author, cmd)
		done <- err
	}()
	if !awaitInHold(t, "the activation holds its guards", func() bool { return d.blocks(blocker) }, done) {
		release()
		return
	}
	activation := blockedBy(t, blocker)[0]
	changed := make(chan timed, 2)
	go func() {
		_, err := q.c.CST.AttestSource(q.reviewer, q.denyCmd("src.1", q.counter()))
		changed <- timed{time.Now(), err}
	}()
	ok := awaitInHold(t, "the denial waits on the activation", func() bool { return len(blockedBy(t, activation)) == 1 }, changed)
	released := time.Now()
	release()
	if !ok {
		return
	}
	if err := <-done; err != nil {
		t.Fatalf("the activation holding the guards: %v", err)
	}
	if r := <-changed; r.err != nil || !r.at.After(released) {
		t.Fatalf("the denial %v, after the activation %v", r.err, r.at.After(released))
	}
	if v := q.getOrder(o1.ObjectID); v.Status != trdapi.OrderActive || v.Commits[0].Entries["risks_found"] != "[]" {
		t.Fatalf("order.1 %+v", v)
	}

	// The denial committed first: order.2's activation finds it and is refused, writing nothing.
	_, _, before := q.orderRows()
	_, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o2.ObjectID, d2.RowVersion, "review.2"))
	if code(err) != trdapi.CodeOrderRiskUnacknowledged || path(err) != "/args/review_id" {
		t.Fatalf("after the denial: %s at %s", code(err), path(err))
	}
	if _, _, commits := q.orderRows(); commits != before || q.getOrder(o2.ObjectID).Status != trdapi.OrderDraft {
		t.Fatal("a refused activation wrote")
	}
	c, err := q.c.TRD.CheckOrder(q.author, o2.ObjectID, "review.2")
	if err != nil || c.Activatable || c.Code != trdapi.CodeOrderRiskUnacknowledged || len(c.Unacknowledged) != 1 {
		t.Fatalf("checked %+v %v", c, err)
	}
	risk := c.Unacknowledged[0]
	// A new review after revoking the approval acknowledges the risk; a near miss does not count.
	rv, err := q.c.TRD.RevokeOrderApproval(q.reviewer, trdapi.RevokeOrderApproval{IdempotencyKey: q.key(), OrderID: o2.ObjectID, ReviewID: "review.2",
		Reason: "上游成本来源被否认"})
	q.must(err)
	r, err := q.c.TRD.RequestOrderReview(q.author, q.requestCmd(o2.ObjectID, rv.RowVersion, "review.2b"))
	q.must(err)
	near := ack(risk)
	near.Detail = "/sources/other"
	_, err = q.c.TRD.DecideOrderReview(q.reviewer, q.decideCmd(o2.ObjectID, r.RowVersion, "review.2b", o2.OrderHash, near))
	q.want(err, kernel.CodeReferenceInvalid)
	d2b, err := q.c.TRD.DecideOrderReview(q.reviewer, q.decideCmd(o2.ObjectID, r.RowVersion, "review.2b", o2.OrderHash, ack(risk)))
	q.must(err)
	if _, err = q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o2.ObjectID, d2b.RowVersion, "review.2b")); err != nil {
		t.Fatalf("activation on the acknowledging approval: %v", err)
	}

	// A party being revised is a risk on every PTY reference the revision binds — its trading
	// profile and its address — each acknowledged by itself; its screening, though, is no longer
	// clear for the revision being edited, which no approval overrides.
	w := newOrderWorld(t)
	_, o := w.prepared("quote.1")
	w.must(w.reviseParty(w.author, "customer.1", 5, w.key()))
	found := w.risks(o.ObjectID)
	if len(found) != 2 || found[0] != (trdapi.Risk{Path: "/data/trading_profile_ref", Code: trdapi.CodeReferenceStale, Detail: ptyapi.StalePartyUnavailable}) ||
		found[1] != (trdapi.Risk{Path: "/data/customer_address_refs/0", Code: trdapi.CodeReferenceStale, Detail: ptyapi.StalePartyUnavailable}) {
		t.Fatalf("after the party revision %+v", found)
	}
	r1, err := w.c.TRD.RequestOrderReview(w.author, w.requestCmd(o.ObjectID, 1, "review.1"))
	w.must(err)
	_, err = w.c.TRD.DecideOrderReview(w.reviewer, w.decideCmd(o.ObjectID, r1.RowVersion, "review.1", o.OrderHash, ack(found[0])))
	if code(err) != trdapi.CodeOrderRiskUnacknowledged {
		t.Fatalf("one of two risks acknowledged: %s", code(err))
	}
	dw, err := w.c.TRD.DecideOrderReview(w.reviewer, w.decideCmd(o.ObjectID, r1.RowVersion, "review.1", o.OrderHash, ack(found[0]), ack(found[1])))
	w.must(err)
	_, err = w.c.TRD.ActivateOrder(w.author, w.activateOrderCmd(o.ObjectID, dw.RowVersion, "review.1"))
	w.want(err, ptyapi.CodePartyScreeningNotClear)

	// A configuration retired after the contract formed (Python PYT-0229 refuses): a risk the
	// approval acknowledges, and the order activates.
	x := newOrderWorld(t)
	_, ox := x.prepared("quote.1")
	_, err = x.c.POL.Retire(x.reviewer, polapi.Retire{IdempotencyKey: x.key(), Kind: x.sale.Kind, ConfigID: x.sale.ConfigID, ExpectedVersion: 2,
		Reason: "停用税适用"})
	x.must(err)
	found = x.risks(ox.ObjectID)
	if len(found) != 1 || found[0] != (trdapi.Risk{Path: "/data/lines/0/tax_application_ref", Code: polapi.CodeConfigNotActive}) {
		t.Fatalf("after the retirement %+v", found)
	}
	dx := x.reviewed(ox, "review.1", ack(found[0]))
	if _, err = x.c.TRD.ActivateOrder(x.author, x.activateOrderCmd(ox.ObjectID, dx.RowVersion, "review.1")); err != nil {
		t.Fatalf("activation acknowledging the retirement: %v", err)
	}

	// The cost's pricing policy retired after the contract formed (Python
	// test_retired_cost_pricing_policy_blocks_new_activation, PYT-0256, refuses): the risks it raises
	// are acknowledged and the order activates; an approval acknowledging none is refused.
	y := newOrderWorld(t)
	_, oy := y.prepared("quote.1")
	_, err = y.c.POL.Retire(y.reviewer, polapi.Retire{IdempotencyKey: y.key(), Kind: polapi.KindPricingPolicy, ConfigID: y.policy.ConfigID,
		ExpectedVersion: 2, Reason: "规则停止采用"})
	y.must(err)
	found = y.risks(oy.ObjectID)
	if len(found) == 0 {
		t.Fatal("a retired pricing policy raised no risk")
	}
	acks := make([]trdapi.RiskAcknowledgement, len(found))
	for i, r := range found {
		acks[i] = ack(r)
	}
	ry, err := y.c.TRD.RequestOrderReview(y.author, y.requestCmd(oy.ObjectID, 1, "review.1"))
	y.must(err)
	_, err = y.c.TRD.DecideOrderReview(y.reviewer, y.decideCmd(oy.ObjectID, ry.RowVersion, "review.1", oy.OrderHash))
	y.want(err, trdapi.CodeOrderRiskUnacknowledged)
	dy, err := y.c.TRD.DecideOrderReview(y.reviewer, y.decideCmd(oy.ObjectID, ry.RowVersion, "review.1", oy.OrderHash, acks...))
	y.must(err)
	if _, err = y.c.TRD.ActivateOrder(y.author, y.activateOrderCmd(oy.ObjectID, dy.RowVersion, "review.1")); err != nil {
		t.Fatalf("activation acknowledging the retired pricing policy %+v: %v", found, err)
	}
}

// TRD-INV-014 and 017 in commit order (the activation's source re-checks), with Python
// test_t03_a26 test_revocation_event_is_a_durable_revision_hold_even_if_effectivity_is_corrupted
// (PYT-0241, the hold half): an acceptance invalidated before the activation commits refuses it
// (ACCEPTANCE_NOT_VALID); an invalidation while the activation holds the quote root waits, and the
// activated order stays active after it; the formalization approval revoked (a risk hold) refuses it
// (APPROVAL_REVOKED). None of these is a risk an approval acknowledges.
func TestActivationRechecksTheContractAndTheSourceRevision(t *testing.T) {
	q := newOrderWorld(t)
	d := newDownstream(q.world)
	a1, o1 := q.prepared("quote.1")
	a2, o2 := q.prepared("quote.2")
	_, o3 := q.prepared("quote.3")
	d1, d2, d3 := q.reviewed(o1, "review.1"), q.reviewed(o2, "review.2"), q.reviewed(o3, "review.3")

	_, err := q.c.TRD.InvalidateAcceptance(q.reviewer, q.invalidation(a1.ObjectID, 1))
	q.must(err)
	_, err = q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o1.ObjectID, d1.RowVersion, "review.1"))
	q.want(err, trdapi.CodeAcceptanceNotValid)

	cmd := q.activateOrderCmd(o2.ObjectID, d2.RowVersion, "review.2")
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandActivateOrder, cmd.IdempotencyKey)
	done := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.ActivateOrder(q.author, cmd)
		done <- err
	}()
	if !awaitInHold(t, "the activation holds the quote root", func() bool { return d.blocks(blocker) }, done) {
		release()
		return
	}
	invalidated := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.InvalidateAcceptance(q.reviewer, q.invalidation(a2.ObjectID, 1))
		invalidated <- err
	}()
	ok := awaitInHold(t, "the invalidation waits for the activating root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, invalidated)
	release()
	if !ok {
		return
	}
	if err = <-done; err != nil {
		t.Fatalf("the activation: %v", err)
	}
	if err = <-invalidated; err != nil {
		t.Fatalf("the invalidation after it: %v", err)
	}
	if v := q.getOrder(o2.ObjectID); v.Status != trdapi.OrderActive {
		t.Fatalf("order.2 %s", v.Status)
	}
	if f := q.fact(a2.ObjectID); f.Effectivity != trdapi.AcceptanceInvalidated {
		t.Fatalf("acceptance %+v", f)
	}

	_, err = q.c.TRD.RevokeApproval(q.reviewer, trdapi.RevokeApproval{IdempotencyKey: q.key(), QuoteID: "quote.3", ApprovalID: "approval.quote.3",
		ExpectedEffectivityVersion: 1, Reason: "批准依据失效", Evidence: q.proof})
	q.must(err)
	_, err = q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o3.ObjectID, d3.RowVersion, "review.3"))
	q.want(err, trdapi.CodeApprovalRevoked)

}

// TRD-INV-021 and TD-AT09 (PTY PartyScreeningGuard v1 at order.activate): the customer's screening
// is a suspected match; activation is refused with PARTY_SCREENING_NOT_CLEAR whatever the order
// approval acknowledges (a screening is not a risk an approval overrides), and writes nothing; after
// compliance decides the hit a false positive, the same order activates.
func TestTDAT09ScreeningNotClearBlocksActivation(t *testing.T) {
	q := newOrderWorld(t, listEntry("E1", "customer.1"))
	if s, err := q.c.PTY.GetScreening(q.author, "customer.1"); err != nil || s.Status != ptyapi.ScreeningMatch {
		t.Fatalf("the screening %+v %v", s, err)
	}
	_, o := q.prepared("quote.1")
	d := q.reviewed(o, "review.1")
	_, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1"))
	if code(err) != ptyapi.CodePartyScreeningNotClear || path(err) != "/data/customer_ref" {
		t.Fatalf("a match: %s at %s", code(err), path(err))
	}
	if _, _, commits := q.orderRows(); commits != 0 {
		t.Fatal("a refused activation wrote")
	}
	s, err := q.c.PTY.GetScreening(q.author, "customer.1")
	q.must(err)
	_, err = q.c.PTY.DecideScreening(q.author, ptyapi.DecideScreening{IdempotencyKey: q.key(), PartyID: "customer.1", ExpectedVersion: s.RowVersion,
		ScreeningID: s.CurrentScreeningID, Decision: ptyapi.DecisionFalsePositive, Reason: "注册信息与名单条目不同"})
	q.must(err)
	if _, err = q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1")); err != nil {
		t.Fatalf("after compliance cleared it: %v", err)
	}
}
