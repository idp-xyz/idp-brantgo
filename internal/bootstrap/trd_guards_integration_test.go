//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// order.activate and order.amendment.apply under STL's CreditGuard v1 and CNX's
// TradeComplianceGuard v1 (S5 unit B4, WBS S5-12b, S5-13b; TRD-INV-022, TRD-INV-023; Context Map
// R57, R58; WF-08, ES-11, SC-09, TD-AT11, TD-AT12; S5 decisions D-2, D-5, D-9, D-20). Python V0.50
// has neither STL nor CNX, so the tests cite design IDs only.
//
// The harness (D-20): an order activates only on a clear trade compliance conclusion for its
// revision and, when its payment plan has a stage due after shipment or invoice, a credit
// reservation of that revision. readyToActivate records both for an order of the order world; the
// shared helpers reviewed and activeOrder call it, and a test that decides the order review itself
// calls it before order.activate. readyToApply reserves credit for the revision an approved
// amendment proposes, before order.amendment.apply.

// orderCredit is what credit reservations of the order world's orders need, set up once: the
// synthetic credit policy credit.orders (E-2), a credit clerk credit.clerk (template sales_clerk,
// D-7), a credit officer credit.officer and two credit managers who publish customer.1's limit of
// 10,000.00 USD (above the top tier two approvers, STL-INV-005; A6 gap 7).
type orderCredit struct {
	policy             polapi.ConfigRef
	clerk, officer     context.Context
	manager1, manager2 context.Context
	reserved           map[trdapi.OrderBinding]stlapi.ReservationReceipt
	limitRequests      int
}

// compliance returns the order world with what a clear trade compliance conclusion needs (cnxOn),
// set up on first use.
func (q *trdWorld) compliance() *cnxWorld {
	q.t.Helper()
	if q.cnx == nil {
		q.cnx = cnxOn(q)
	}
	return q.cnx
}

// orderCredit returns the order world's credit setup, made on first use.
func (q *trdWorld) orderCredit() *orderCredit {
	q.t.Helper()
	if q.credit != nil {
		return q.credit
	}
	c := &orderCredit{reserved: map[trdapi.OrderBinding]stlapi.ReservationReceipt{}}
	c.policy = q.configure(q.author, q.reviewer, polapi.KindCreditPolicy, "credit.orders", 0, q.win.from, time.Time{}, e2Policy(), true)
	c.clerk, c.officer = q.assign("credit.clerk", "sales_clerk"), q.assign("credit.officer", "credit_officer")
	c.manager1, c.manager2 = q.assign("credit.manager.1", "credit_manager"), q.assign("credit.manager.2", "credit_manager")
	q.credit = c
	q.creditLimit("10000.00")
	return c
}

// creditLimit has the credit clerk request a limit of amount USD for customer.1 on the evidence and
// the credit managers publish it (one, or both above the top tier); the published limit is in
// force from now.
func (q *trdWorld) creditLimit(amount string) {
	q.t.Helper()
	c := q.orderCredit()
	c.limitRequests++
	id := "limit.orders." + strconv.Itoa(c.limitRequests)
	_, err := q.c.STL.RequestCreditLimit(c.clerk, stlapi.RequestCreditLimit{IdempotencyKey: q.key(), RequestID: id, PartyID: "customer.1",
		Currency: stlRef(q.usd), Sources: []stlapi.LimitSource{{Source: stlapi.SourceInternalAssessment, Amount: amount,
			EvidenceIDs: []string{"evidence"}}}, ValidFrom: kernel.FormatTimestamp(time.Now().Add(-time.Minute)),
		ValidUntil: kernel.FormatTimestamp(q.win.from.Add(365 * 24 * time.Hour)), CreditPolicy: stlRef(c.policy), Reason: creditReason})
	q.must(err)
	for i, m := range []context.Context{c.manager1, c.manager2} {
		r, err := q.c.STL.PublishCreditLimit(m, stlapi.PublishCreditLimit{IdempotencyKey: q.key(), RequestID: id, ExpectedVersion: int64(i + 1),
			Decision: stlapi.DecisionPublish, CreditPolicy: stlRef(c.policy), Reason: creditReason})
		q.must(err)
		if r.Status == stlapi.RequestPublished {
			return
		}
	}
	q.t.Fatalf("limit %s of %s was not published", id, amount)
}

// reserveCmd is credit.reserve by the credit clerk of order revision b under credit.orders, as
// reservation "res.<order>.r<revision>".
func (q *trdWorld) reserveCmd(b trdapi.OrderBinding) stlapi.ReserveCredit {
	return stlapi.ReserveCredit{IdempotencyKey: q.key(), ReservationID: "res." + b.OrderID + ".r" + strconv.FormatInt(b.OrderRevision, 10),
		OrderID: b.OrderID, OrderRevision: b.OrderRevision, OrderHash: b.OrderHash, CreditPolicy: stlRef(q.orderCredit().policy)}
}

// reserveCredit has the credit clerk reserve credit for order revision b (TRD's current revision,
// or one an amendment proposes) under credit.orders, once per binding, and returns the receipt.
func (q *trdWorld) reserveCredit(b trdapi.OrderBinding) stlapi.ReservationReceipt {
	q.t.Helper()
	c := q.orderCredit()
	if r, ok := c.reserved[b]; ok {
		return r
	}
	r, err := q.c.STL.ReserveCredit(c.clerk, q.reserveCmd(b))
	q.must(err)
	if r.Status != stlapi.ReservationReserved {
		q.t.Fatalf("reservation for %+v: %+v", b, r)
	}
	c.reserved[b] = r
	return r
}

// afterShipment reports whether a payment plan has a stage due after shipment or after the invoice
// (TRD-INV-022: such a plan is put to CreditGuard v1).
func afterShipment(stages []trdapi.PaymentStage) bool {
	for _, s := range stages {
		if s.Trigger == trdapi.TriggerAfterShipment || s.Trigger == trdapi.TriggerAfterInvoice {
			return true
		}
	}
	return false
}

// readyToActivate records what order.activate's guards ask of draft order id at its current
// revision (D-20): a clear trade compliance conclusion (compliance().cleared) unless CNX has a
// check of that revision already — a test that made one decides it itself — and, when the
// revision's payment plan has a stage due after shipment or invoice, a credit reservation of it
// (reserveCredit).
func (q *trdWorld) readyToActivate(id string) {
	q.t.Helper()
	w := q.compliance()
	v := q.getOrder(id)
	checks, err := w.cnx.OrderChecks(w.compliance, id)
	q.must(err)
	checked := false
	for _, c := range checks {
		checked = checked || c.OrderRevision == v.OrderRevision
	}
	if !checked {
		w.cleared(id)
	}
	snap := q.snapshots(q.author, id)[id]
	if afterShipment(snap.Stages) {
		q.reserveCredit(snap.Binding)
	}
}

// proposed returns the revision amendment id of order proposes, as TRD's order snapshot states it.
func (q *trdWorld) proposed(order, id string) trdapi.ProposedRevision {
	q.t.Helper()
	for _, p := range q.snapshots(q.author, order)[order].Proposed {
		if p.AmendmentID == id {
			if p.Code != "" {
				q.t.Fatalf("amendment %s proposes nothing: %s", id, p.Code)
			}
			return p
		}
	}
	q.t.Fatalf("order %s states no revision amendment %s proposes", order, id)
	return trdapi.ProposedRevision{}
}

// readyToApply records what order.amendment.apply's credit guard asks of approved amendment id of
// order (D-9): a reservation of the revision it proposes when that revision's payment plan has a
// stage due after shipment or invoice. (A reservation is needed only when the revision asks more
// credit than the current one; one made when it asks no more is harmless.)
func (q *trdWorld) readyToApply(order, id string) trdapi.ProposedRevision {
	q.t.Helper()
	p := q.proposed(order, id)
	if afterShipment(p.Stages) {
		q.reserveCredit(p.Binding)
	}
	return p
}

// orderBinding is order id at its current revision and hash.
func (q *trdWorld) orderBinding(id string) trdapi.OrderBinding {
	q.t.Helper()
	v := q.getOrder(id)
	return trdapi.OrderBinding{OrderID: id, OrderRevision: v.OrderRevision, OrderHash: v.OrderHash}
}

// paymentPlans holds, per test, the payment terms "payment" its worlds publish in place of 100 %
// before shipment (settlement): the customer's trading profile, the costing and the quote all bind
// that one revision (calc.Scope: COMPARISON_BASIS_MISMATCH otherwise), so an order with a stage
// due after shipment is made from the start.
var paymentPlans sync.Map // *testing.T → *polapi.PaymentTerms

// newCreditOrderWorld is the order world on payment terms of 30 % on order confirmation and 70 %
// 30 days after shipment (BRC-001's plan: 288.00 + 672.00 of the world's 960.00), the second stage
// under release control control ("" or documents, D-5).
func newCreditOrderWorld(t *testing.T, control string) *trdWorld {
	paymentPlans.Store(t, &polapi.PaymentTerms{Name: "30% on order confirmation, 70% 30 days after shipment", Stages: []polapi.PaymentStage{
		{StageID: "P1", Percent: "30", Trigger: polapi.TriggerOrderConfirmed},
		{StageID: "P2", Percent: "70", Trigger: polapi.TriggerAfterShipment, Days: 30, ReleaseControl: control}}})
	defer paymentPlans.Delete(t)
	return newOrderWorld(t)
}

// draftOn has quote id accepted, order "order.<id>" prepared and its review "review.<id>"
// approved — and nothing recorded for order.activate's guards. It returns the order and the review
// decision's receipt.
func (q *trdWorld) draftOn(id string) (trdapi.OrderReceipt, trdapi.OrderReceipt) {
	q.t.Helper()
	_, o := q.prepared(id)
	return o, q.approvedReview(o, "review."+id)
}

// activation is order.activate of order o on its review, decided as r.
func (q *trdWorld) activation(o, r trdapi.OrderReceipt) trdapi.ActivateOrder {
	return q.activateOrderCmd(o.ObjectID, r.RowVersion, "review."+strings.TrimPrefix(o.ObjectID, "order."))
}

// submittedAmendment has the author prepare amendment id of order at revision rev and hash (L1 to
// qty) and submit it; it returns the submission's receipt.
func (q *trdWorld) submittedAmendment(order, id string, rev int64, hash, qty string) trdapi.AmendmentReceipt {
	q.t.Helper()
	p, err := q.c.TRD.PrepareAmendment(q.author, q.amendCmd(order, id, 0, rev, hash, qty))
	q.must(err)
	s, err := q.c.TRD.SubmitAmendment(q.author, trdapi.SubmitAmendment{IdempotencyKey: q.key(), OrderID: order, AmendmentID: id,
		ExpectedVersion: p.RowVersion})
	q.must(err)
	return s
}

// wantRefused requires err to be code at path.
func wantRefused(t *testing.T, what string, err error, wantCode, wantPath string) {
	t.Helper()
	if code(err) != wantCode || path(err) != wantPath {
		t.Fatalf("%s: %v, want %s at %s", what, err, wantCode, wantPath)
	}
}

// TD-AT11 and TD-AT12 (TRD halves), SC-09 as far as TRD goes, TRD-INV-022, TRD-INV-023, D-5, WF-08,
// ES-11: order.activate asks CNX's TradeComplianceGuard v1 after CAT and STL's CreditGuard v1
// after PTY, and neither refusal is a risk an order approval acknowledges.
//
//   - D-5: an order whose only stage after shipment is document-controlled is put to the guard with
//     portion 0; STL's reservation is documents_secured, and the activation records it.
//   - SC-09's first order, TD-AT11 (customer.1's limit lowered to 500.00 USD): cleared by compliance,
//     the order is refused CREDIT_RESERVATION_REQUIRED without a reservation; credit.reserve opens a
//     credit decision (CREDIT_LIMIT_EXCEEDED: 672.00 against 500.00), which is no reservation either;
//     once the credit officer releases the exception, the reservation is made under it and the
//     order activates with the evidence of both: the reservation, its basis credit_decision and the
//     decision, and CNX's check, decision and regulation revision.
//   - SC-09's second order, TD-AT12: its destination is under the synthetic CN embargo and
//     compliance concludes blocked: order.activate and order.check answer TRADE_COMPLIANCE_NOT_CLEAR
//     — before the credit it has no reservation for either — and the order stays a draft with no
//     commit evidence. Concluded clear on corrected facts, it is refused CREDIT_RESERVATION_REQUIRED.
func TestOrderActivationAsksTradeComplianceThenCredit(t *testing.T) {
	docs := newCreditOrderWorld(t, trdapi.ReleaseControlDocuments)
	o3, r3 := docs.draftOn("quote.1")
	docs.readyToActivate(o3.ObjectID)
	secured := docs.credit.reserved[docs.orderBinding(o3.ObjectID)]
	if secured.Basis != stlapi.BasisDocumentsSecured || secured.PortionMinor != "0" || secured.SecuredMinor != "67200" {
		t.Fatalf("the documents-secured reservation %+v", secured)
	}
	_, err := docs.c.TRD.ActivateOrder(docs.author, docs.activation(o3, r3))
	docs.must(err)
	e := docs.getOrder(o3.ObjectID).Commits[0].Entries
	if e[trdapi.GuardSTLCredit] != stlapi.CreditDocumentsSecured || e[trdapi.EvidenceCreditPortion] != "0" ||
		e[trdapi.EvidenceCreditPostShipment] != "67200" || e[trdapi.EvidenceCreditBasis] != stlapi.BasisDocumentsSecured ||
		e[trdapi.EvidenceCreditReservation] != secured.ReservationID {
		t.Fatalf("documents secured: the commit evidence %v", e)
	}

	// SC-09's first order under TD-AT11's exception.
	q := newCreditOrderWorld(t, "")
	w, c := q.compliance(), q.orderCredit()
	q.creditLimit("500.00")
	o1, r1 := q.draftOn("quote.1")
	cleared := w.cleared(o1.ObjectID)
	b1 := q.orderBinding(o1.ObjectID)
	activate := q.activation(o1, r1)
	_, err = q.c.TRD.ActivateOrder(q.author, activate)
	wantRefused(t, "without a reservation", err, stlapi.CodeCreditReservationRequired, "/args/order_id")
	opened, err := q.c.STL.ReserveCredit(c.clerk, q.reserveCmd(b1))
	q.must(err)
	if opened.Status != stlapi.StatusDecisionOpened || !slices.Contains(opened.Reasons, stlapi.CodeCreditLimitExceeded) ||
		opened.ExcessMinor != "17200" {
		t.Fatalf("the reservation above the limit %+v", opened)
	}
	_, err = q.c.TRD.ActivateOrder(q.author, q.activation(o1, r1))
	wantRefused(t, "on an open credit decision", err, stlapi.CodeCreditReservationRequired, "/args/order_id")
	if chk, err := q.c.TRD.CheckOrder(q.reviewer, o1.ObjectID, "review.quote.1"); err != nil || chk.Activatable ||
		chk.Code != stlapi.CodeCreditReservationRequired {
		t.Fatalf("order.check %+v %v", chk, err)
	}
	released, err := q.c.STL.DecideCreditException(c.officer, stlapi.DecideCreditException{IdempotencyKey: q.key(), DecisionID: opened.DecisionID,
		ExpectedVersion: opened.RowVersion, Decision: stlapi.DecisionRelease, CreditPolicy: stlRef(c.policy), Reason: creditReason})
	q.must(err)
	if released.Status != stlapi.DecisionReleased {
		t.Fatalf("the exception %+v", released)
	}
	reserved, err := q.c.STL.ReserveCredit(c.clerk, q.reserveCmd(b1))
	q.must(err)
	if reserved.Status != stlapi.ReservationReserved || reserved.Basis != stlapi.BasisCreditDecision || reserved.DecisionID != opened.DecisionID ||
		reserved.PortionMinor != "67200" {
		t.Fatalf("the reservation under the exception %+v", reserved)
	}
	act, err := q.c.TRD.ActivateOrder(q.author, activate)
	q.must(err)
	if act.Status != trdapi.OrderActive {
		t.Fatalf("activated %+v", act)
	}
	e = q.getOrder(o1.ObjectID).Commits[0].Entries
	for name, want := range map[string]string{trdapi.GuardSTLCredit: stlapi.CreditCovered, trdapi.EvidenceCreditReservation: reserved.ReservationID,
		trdapi.EvidenceCreditReserved: "67200", trdapi.EvidenceCreditPortion: "67200", trdapi.EvidenceCreditBasis: stlapi.BasisCreditDecision,
		trdapi.EvidenceCreditDecision: opened.DecisionID, trdapi.EvidenceCreditCurrency: "USD",
		trdapi.GuardCNXTradeCompliance:      "clear/cleared/" + cleared.CheckID + "/" + cleared.DecisionID + "/false",
		trdapi.EvidenceComplianceRegulation: w.regulation.ConfigID + "@" + strconv.FormatInt(w.regulation.Revision, 10),
		trdapi.EvidenceComplianceValidUntil: kernel.FormatTimestamp(cleared.ValidUntil)} {
		if e[name] != want {
			t.Errorf("commit evidence %s = %q, want %q", name, e[name], want)
		}
	}
	var activated map[string]any
	for _, ev := range q.eventsOf(trdapi.EventOrderActivated) {
		if ev.Payload["order_id"] == o1.ObjectID {
			activated = ev.Payload
		}
	}
	if activated["credit_guard"] != stlapi.CreditCovered || activated["credit_reservation_id"] != reserved.ReservationID ||
		activated["unchecked_guards"] != "" {
		t.Fatalf("OrderActivated %v", activated)
	}

	// SC-09's second order: an embargoed destination.
	o2, r2 := q.draftOn("quote.2")
	ev, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o2.ObjectID, 1, 0, syntheticEmbargoed))
	q.must(err)
	_, err = w.cnx.Decide(w.compliance, w.decideCmd(ev, cnxapi.ConclusionBlocked, false))
	q.must(err)
	_, err = q.c.TRD.ActivateOrder(q.author, q.activation(o2, r2))
	wantRefused(t, "an embargoed destination", err, cnxapi.CodeTradeComplianceNotClear, "/args/order_id")
	if chk, err := q.c.TRD.CheckOrder(q.reviewer, o2.ObjectID, "review.quote.2"); err != nil || chk.Activatable ||
		chk.Code != cnxapi.CodeTradeComplianceNotClear || len(chk.Unchecked) != 0 {
		t.Fatalf("order.check %+v %v", chk, err)
	}
	if v := q.getOrder(o2.ObjectID); v.Status != trdapi.OrderDraft || len(v.Commits) != 0 {
		t.Fatalf("the blocked order %+v", v)
	}
	views, err := w.cnx.OrderChecks(w.compliance, o2.ObjectID)
	q.must(err)
	again, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o2.ObjectID, 1, views[0].RowVersion, syntheticClear))
	q.must(err)
	_, err = w.cnx.Decide(w.compliance, w.decideCmd(again, cnxapi.ConclusionClear, false))
	q.must(err)
	_, err = q.c.TRD.ActivateOrder(q.author, q.activation(o2, r2))
	wantRefused(t, "cleared, without a reservation", err, stlapi.CodeCreditReservationRequired, "/args/order_id")
	if v := q.getOrder(o2.ObjectID); v.Status != trdapi.OrderDraft {
		t.Fatalf("the unreserved order %+v", v)
	}
}

// S5 D-9, TRD-INV-022, A6 gap 8, CNX-INV-001 ("订单变更后旧结论不适用"): order.amendment.apply asks STL's
// CreditGuard v1 for the revision the amendment produces when that revision asks more credit than
// the current one, and records trade compliance as not checked at apply. The order activates on
// the 672.00 reservation of revision 1. Reduced to 90 PCS (504.00 on credit) revision 2 asks less:
// it applies on what the order holds (not_increased, both portions in the evidence), CNX has no
// conclusion for it, and a person cannot release revision 1's reservation, which revision 2 stands
// on. Raised to 110 PCS (616.00) revision 3 asks more: TRD's snapshot states the revision the
// pending amendment proposes (not a draft's), STL refuses a binding TRD does not state, apply is
// CREDIT_RESERVATION_REQUIRED until STL reserves revision 3, and then produces exactly the revision
// reserved.
func TestAmendmentApplyAsksCreditForTheRevisionItProduces(t *testing.T) {
	q := newCreditOrderWorld(t, "")
	w, c := q.compliance(), q.orderCredit()
	id := q.activeOrder("quote.1").ObjectID
	b1 := q.orderBinding(id)
	first := c.reserved[b1]

	less := q.approvedAmendment(id, "amendment.less", 1, b1.OrderHash, "90")
	if p := q.proposed(id, "amendment.less"); p.CreditPortionMinor != "50400" || p.Binding.OrderRevision != 2 {
		t.Fatalf("the revision amendment.less proposes %+v", p)
	}
	r2, err := q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(id, "amendment.less", less.RowVersion))
	q.must(err)
	e := q.getOrder(id).AmendmentCommits[0].Entries
	for name, want := range map[string]string{trdapi.GuardSTLCredit: trdapi.GuardNotIncreased, trdapi.EvidenceCreditPortion: "50400",
		trdapi.EvidenceCreditPreviousPortion: "67200", trdapi.EvidenceCreditPostShipment: "50400", trdapi.EvidenceCreditPreviousPost: "67200",
		trdapi.GuardCNXTradeCompliance: trdapi.GuardNotCheckedAtApply} {
		if e[name] != want {
			t.Errorf("revision 2's commit evidence %s = %q, want %q", name, e[name], want)
		}
	}
	b2 := cnxapi.OrderBinding{OrderID: id, OrderRevision: r2.OrderRevision, OrderHash: r2.OrderHash}
	w.wantVerdict(b2, cnxapi.StatusPending, cnxapi.ReasonOrderRevised)
	_, err = q.c.STL.ReleaseCredit(c.officer, stlapi.ReleaseCredit{IdempotencyKey: q.key(), ReservationID: first.ReservationID, ExpectedVersion: 1,
		Reason: "订单已变更"})
	wantRefused(t, "releasing what revision 2 stands on", err, kernel.CodeInvalidState, "/args/reservation_id")

	pending := q.submittedAmendment(id, "amendment.more", 2, r2.OrderHash, "110")
	_, err = q.c.TRD.PrepareAmendment(q.author, q.amendCmd(id, "amendment.draft", 0, 2, r2.OrderHash, "100"))
	q.must(err)
	snap := q.snapshots(q.author, id)[id]
	if len(snap.Proposed) != 1 || snap.Proposed[0].AmendmentID != "amendment.more" ||
		snap.Proposed[0].AmendmentStatus != trdapi.AmendmentPendingApproval || snap.Proposed[0].CreditPortionMinor != "61600" ||
		snap.Proposed[0].Binding.OrderRevision != 3 || snap.Proposed[0].AmendmentHash != pending.AmendmentHash {
		t.Fatalf("the revisions TRD states as proposed %+v", snap.Proposed)
	}
	proposed := snap.Proposed[0]
	wrongHash := q.reserveCmd(trdapi.OrderBinding{OrderID: id, OrderRevision: 3, OrderHash: strings.Repeat("0", 64)})
	_, err = q.c.STL.ReserveCredit(c.clerk, wrongHash)
	wantRefused(t, "a revision 3 TRD does not state", err, stlapi.CodeReferenceStale, "/args/order_hash")
	_, err = q.c.STL.ReserveCredit(c.clerk, q.reserveCmd(trdapi.OrderBinding{OrderID: id, OrderRevision: 4, OrderHash: proposed.Binding.OrderHash}))
	wantRefused(t, "a revision nothing proposes", err, stlapi.CodeReferenceStale, "/args/order_revision")

	d, err := q.c.TRD.DecideAmendment(q.reviewer, trdapi.DecideAmendment{IdempotencyKey: q.key(), OrderID: id, AmendmentID: "amendment.more",
		ExpectedVersion: pending.RowVersion, AmendmentHash: pending.AmendmentHash, Decision: trdapi.DecisionApprove, Reason: "客户书面确认"})
	q.must(err)
	_, err = q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(id, "amendment.more", d.RowVersion))
	wantRefused(t, "revision 3 without a reservation", err, stlapi.CodeCreditReservationRequired, "/args/amendment_id")
	if v := q.getOrder(id); v.OrderRevision != 2 || len(v.AmendmentCommits) != 1 {
		t.Fatalf("the refused apply changed the order %+v", v)
	}
	third := q.readyToApply(id, "amendment.more")
	if third.Binding != proposed.Binding || c.reserved[third.Binding].PortionMinor != "61600" {
		t.Fatalf("the reservation of revision 3 %+v", c.reserved[third.Binding])
	}
	r3, err := q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(id, "amendment.more", d.RowVersion))
	q.must(err)
	if r3.OrderRevision != 3 || r3.OrderHash != proposed.Binding.OrderHash {
		t.Fatalf("applied %+v, proposed %+v", r3, proposed.Binding)
	}
	e = q.getOrder(id).AmendmentCommits[1].Entries
	if e[trdapi.GuardSTLCredit] != stlapi.CreditCovered || e[trdapi.EvidenceCreditReservation] != c.reserved[third.Binding].ReservationID ||
		e[trdapi.EvidenceCreditPreviousPortion] != "50400" || e[trdapi.EvidenceCreditPortion] != "61600" {
		t.Fatalf("revision 3's commit evidence %v", e)
	}
	var amended []map[string]any
	for _, ev := range q.eventsOf(trdapi.EventOrderAmended) {
		amended = append(amended, ev.Payload)
	}
	if len(amended) != 2 || amended[0]["credit_guard"] != trdapi.GuardNotIncreased || amended[1]["credit_guard"] != stlapi.CreditCovered ||
		amended[1]["credit_reservation_id"] != c.reserved[third.Binding].ReservationID || amended[1]["unchecked_guards"] != "" ||
		amended[1]["fulfilment_source"] != trdapi.GuardFULShipments {
		t.Fatalf("OrderAmended %v", amended)
	}
}

// holdReservation locks reservation id FOR UPDATE in a transaction of company.A, as credit.release
// does, until release; it returns the backend's pid.
func (d *downstream) holdReservation(t *testing.T, id string) (pid int32, release func()) {
	t.Helper()
	hold, pids, done := make(chan struct{}), make(chan int32, 1), make(chan error, 1)
	go func() {
		done <- d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
			var p int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&p); err != nil {
				return err
			}
			if _, err := exec.Exec(ctx, `SELECT 1 FROM brant_stl.credit_reservation WHERE tenant_id = $1 AND company_id = $2 AND reservation_id = $3
				FOR UPDATE`, string(d.scope.Tenant), string(d.scope.Company), id); err != nil {
				return err
			}
			pids <- p
			<-hold
			return errBlockerRollsBack
		})
	}()
	select {
	case pid = <-pids:
	case err := <-done:
		t.Fatalf("holder: %v", err)
	}
	var closed bool
	release = func() {
		if closed {
			return
		}
		closed = true
		close(hold)
		if err := <-done; !errors.Is(err, errBlockerRollsBack) {
			t.Errorf("the holder must roll back: %v", err)
		}
	}
	t.Cleanup(release)
	return pid, release
}

// finished is what a command run in a goroutine returned, and when.
type finished struct {
	at  time.Time
	err error
}

// A6 gap 1, ADR-BR-004 option B, ADR-BR-011, SM-STL-CREDIT-RESERVATION (订单未生效即放弃): a person's
// credit.release and order.activate on PostgreSQL 16, in both commit orders.
//
//   - The activation holds CreditGuard v1 on the reservation (it waits at its action result): the
//     release waits for its lock on the reservation row; once the order is active the release reads
//     TRD's order state (brant_trd.order_state_v1, after its locks) and refuses (INVALID_STATE); the
//     reservation stays reserved.
//   - The release holds the reservation row (it waits at its action result): the activation's
//     guard waits; the release commits, the guard sees no live reservation and the activation is
//     refused (CREDIT_RESERVATION_REQUIRED); the order stays a draft.
func TestCreditReleaseAndActivationInBothCommitOrders(t *testing.T) {
	q := newCreditOrderWorld(t, "")
	c := q.orderCredit()
	d := newDownstream(q.world)
	release := func(id, key string) stlapi.ReleaseCredit {
		return stlapi.ReleaseCredit{IdempotencyKey: key, ReservationID: id, ExpectedVersion: 1, Reason: "订单未生效，放弃占用"}
	}

	t.Run("the activation holds the guard", func(t *testing.T) {
		o, r := q.draftOn("quote.1")
		q.readyToActivate(o.ObjectID)
		res := c.reserved[q.orderBinding(o.ObjectID)]
		cmd := q.activation(o, r)
		blocker, unblock := d.keyBlocker(t, "brant_trd", trdapi.CommandActivateOrder, cmd.IdempotencyKey)
		activated := make(chan finished, 1)
		go func() {
			_, err := q.c.TRD.ActivateOrder(q.author, cmd)
			activated <- finished{time.Now(), err}
		}()
		if !awaitInHold(t, "the activation holds its guards", func() bool { return d.blocks(blocker) }, activated) {
			unblock()
			return
		}
		activation := blockedBy(t, blocker)[0]
		released := make(chan finished, 1)
		go func() {
			_, err := q.c.STL.ReleaseCredit(c.officer, release(res.ReservationID, q.key()))
			released <- finished{time.Now(), err}
		}()
		ok := awaitInHold(t, "the release waits on the activation", func() bool { return len(blockedBy(t, activation)) == 1 }, released)
		unblock()
		if !ok {
			return
		}
		a := <-activated
		if a.err != nil {
			t.Fatalf("activation: %v", a.err)
		}
		rel := <-released
		wantRefused(t, "the release after the activation", rel.err, kernel.CodeInvalidState, "/args/reservation_id")
		if v, err := q.c.STL.GetReservation(c.officer, res.ReservationID); err != nil || v.State != stlapi.ReservationReserved {
			t.Fatalf("the reservation %+v %v", v, err)
		}
		if v := q.getOrder(o.ObjectID); v.Status != trdapi.OrderActive {
			t.Fatalf("the order %+v", v)
		}
	})

	t.Run("the release holds the reservation", func(t *testing.T) {
		o, r := q.draftOn("quote.2")
		q.readyToActivate(o.ObjectID)
		res := c.reserved[q.orderBinding(o.ObjectID)]
		key := q.key()
		blocker, unblock := d.keyBlocker(t, "brant_stl", stlapi.CommandRelease, key)
		released := make(chan finished, 1)
		go func() {
			_, err := q.c.STL.ReleaseCredit(c.officer, release(res.ReservationID, key))
			released <- finished{time.Now(), err}
		}()
		if !awaitInHold(t, "the release holds the reservation", func() bool { return d.blocks(blocker) }, released) {
			unblock()
			return
		}
		releasing := blockedBy(t, blocker)[0]
		activated := make(chan finished, 1)
		go func() {
			_, err := q.c.TRD.ActivateOrder(q.author, q.activation(o, r))
			activated <- finished{time.Now(), err}
		}()
		ok := awaitInHold(t, "the activation's guard waits on the release", func() bool { return len(blockedBy(t, releasing)) == 1 }, activated)
		unblock()
		if !ok {
			return
		}
		if rel := <-released; rel.err != nil {
			t.Fatalf("release: %v", rel.err)
		}
		a := <-activated
		wantRefused(t, "the activation after the release", a.err, stlapi.CodeCreditReservationRequired, "/args/order_id")
		if v := q.getOrder(o.ObjectID); v.Status != trdapi.OrderDraft || len(v.Commits) != 0 {
			t.Fatalf("the order %+v", v)
		}
	})
}

// ADR-BR-004 option B, ADR-BR-011, TRD-INV-023, SM-CNX-COMPLIANCE ("decide(blocked) ∥ activate"):
// compliance's decisions and order.activate on PostgreSQL 16, in both commit orders.
//
//   - A blocking decision committed first: the activation is refused (TRADE_COMPLIANCE_NOT_CLEAR).
//   - The activation holds TradeComplianceGuard v1 on a check awaiting its decision (it waits at
//     STL's guard, whose reservation row a holder locks): the blocking decision waits; the
//     activation saw pending and is refused; the decision then commits blocked.
//   - The activation holds the guard on a clear conclusion (it waits at its action result): a new
//     version of a programme list the conclusion used waits (MD-47); the order activates on the
//     conclusion that committed first; then the guard answers list_updated, compliance reopens,
//     evaluates and blocks the revision — FUL's release gate is the next to ask (D-9).
func TestTradeComplianceDecisionAndActivationInBothCommitOrders(t *testing.T) {
	q := newCreditOrderWorld(t, "")
	w := q.compliance()
	d := newDownstream(q.world)

	t.Run("the blocking decision commits first", func(t *testing.T) {
		o, r := q.draftOn("quote.1")
		e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, 0, syntheticEmbargoed))
		q.must(err)
		_, err = w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionBlocked, false))
		q.must(err)
		q.readyToActivate(o.ObjectID) // the credit only: the order has a check
		_, err = q.c.TRD.ActivateOrder(q.author, q.activation(o, r))
		wantRefused(t, "after the blocking decision", err, cnxapi.CodeTradeComplianceNotClear, "/args/order_id")
	})

	t.Run("the activation holds the pending check", func(t *testing.T) {
		o, r := q.draftOn("quote.2")
		e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, 0, syntheticEmbargoed))
		q.must(err)
		q.readyToActivate(o.ObjectID)
		holder, unhold := d.holdReservation(t, q.credit.reserved[q.orderBinding(o.ObjectID)].ReservationID)
		activated := make(chan finished, 1)
		go func() {
			_, err := q.c.TRD.ActivateOrder(q.author, q.activation(o, r))
			activated <- finished{time.Now(), err}
		}()
		if !awaitInHold(t, "the activation waits at STL's guard", func() bool { return d.blocks(holder) }, activated) {
			unhold()
			return
		}
		activation := blockedBy(t, holder)[0]
		decided := make(chan finished, 1)
		go func() {
			_, err := w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionBlocked, false))
			decided <- finished{time.Now(), err}
		}()
		ok := awaitInHold(t, "the decision waits on the activation", func() bool { return len(blockedBy(t, activation)) == 1 }, decided)
		unhold()
		if !ok {
			return
		}
		a := <-activated
		wantRefused(t, "the activation that saw pending", a.err, cnxapi.CodeTradeComplianceNotClear, "/args/order_id")
		if dec := <-decided; dec.err != nil {
			t.Fatalf("decision %v", dec.err)
		}
		w.wantVerdict(w.binding(o.ObjectID), cnxapi.StatusBlocked, cnxapi.ReasonBlocked)
		if v := q.getOrder(o.ObjectID); v.Status != trdapi.OrderDraft {
			t.Fatalf("the order %+v", v)
		}
	})

	t.Run("the activation holds the clear conclusion", func(t *testing.T) {
		o, r := q.draftOn("quote.3")
		q.readyToActivate(o.ObjectID)
		b := w.binding(o.ObjectID)
		checks, err := w.cnx.OrderChecks(w.compliance, o.ObjectID)
		q.must(err)
		cmd := q.activation(o, r)
		blocker, unblock := d.keyBlocker(t, "brant_trd", trdapi.CommandActivateOrder, cmd.IdempotencyKey)
		activated := make(chan finished, 1)
		go func() {
			_, err := q.c.TRD.ActivateOrder(q.author, cmd)
			activated <- finished{time.Now(), err}
		}()
		if !awaitInHold(t, "the activation holds its guards", func() bool { return d.blocks(blocker) }, activated) {
			unblock()
			return
		}
		activation := blockedBy(t, blocker)[0]
		imported := make(chan finished, 1)
		go func() {
			list := syntheticCountryProgrammes(unProgrammeList, "v2", "2026-11-10")
			list.IdempotencyKey = q.key()
			_, err := q.c.REF.ImportCountryProgrammes(w.compliance, list)
			imported <- finished{time.Now(), err}
		}()
		ok := awaitInHold(t, "the programme list's new version waits on the activation", func() bool { return len(blockedBy(t, activation)) == 1 },
			imported)
		unblock()
		if !ok {
			return
		}
		if a := <-activated; a.err != nil {
			t.Fatalf("activation: %v", a.err)
		}
		if im := <-imported; im.err != nil {
			t.Fatalf("import: %v", im.err)
		}
		w.wantVerdict(b, cnxapi.StatusPending, cnxapi.ReasonListUpdated)
		reopen, err := w.cnx.Reopen(w.system, cnxapi.Reopen{IdempotencyKey: q.key(), Cause: cnxapi.CauseProgrammeUpdated, CheckID: checks[0].CheckID,
			ExpectedVersion: checks[0].RowVersion, Reason: "名单新版本"})
		q.must(err)
		e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, reopen.RowVersion, syntheticEmbargoed))
		q.must(err)
		_, err = w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionBlocked, false))
		q.must(err)
		w.wantVerdict(b, cnxapi.StatusBlocked, cnxapi.ReasonBlocked)
		if v := q.getOrder(o.ObjectID); v.Status != trdapi.OrderActive || !strings.HasPrefix(v.Commits[0].Entries[trdapi.GuardCNXTradeCompliance], "clear/") {
			t.Fatalf("the order activated on the clear conclusion %+v", v.Commits)
		}
	})
}

// The order-state read (migrations/trd/000008, A6 gap 1): one row in the caller's company for any
// order — draft, active at its current revision and hash — and known false for an unknown one or
// another company's; it takes no lock, so it does not wait for an order command holding the order
// row; a SECURITY DEFINER function brant_app executes and PUBLIC does not.
func TestOrderStateReadIsLockFree(t *testing.T) {
	q := newOrderWorld(t)
	d := newDownstream(q.world)
	_, draft := q.prepared("quote.1")
	active := q.activeOrder("quote.2")
	read := func(scope kernel.Scope, id string) trdapi.OrderState {
		t.Helper()
		var s trdapi.OrderState
		q.must(d.in(scope, func(ctx context.Context, exec bentopg.Executor) error {
			var err error
			s, err = trdapi.ScanOrderState(id, exec.QueryRow(ctx, trdapi.OrderStateSQL, trdapi.OrderStateArgs(id)...).Scan)
			return err
		}))
		return s
	}
	if s := read(d.scope, draft.ObjectID); !s.Known || s.Status != trdapi.OrderDraft || s.OrderRevision != 1 || s.OrderHash != draft.OrderHash ||
		s.ActiveFrom(1) {
		t.Fatalf("the draft %+v", s)
	}
	v := q.getOrder(active.ObjectID)
	if s := read(d.scope, active.ObjectID); !s.Known || s.Status != trdapi.OrderActive || s.OrderHash != v.OrderHash || s.RowVersion != v.RowVersion ||
		!s.ActiveFrom(1) || s.ActiveFrom(2) {
		t.Fatalf("the active order %+v", s)
	}
	if s := read(d.scope, "order.none"); s.Known || s.Status != "" {
		t.Fatalf("an unknown order %+v", s)
	}
	if s := read(q.scope("company.B"), active.ObjectID); s.Known {
		t.Fatalf("another company's order %+v", s)
	}
	// An order command holding the order row FOR UPDATE does not delay the read.
	held := make(chan struct{})
	locked := make(chan error, 1)
	go func() {
		locked <- d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
			if _, err := exec.Exec(ctx, `SELECT 1 FROM brant_trd.sales_order WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 FOR UPDATE`,
				string(d.scope.Tenant), string(d.scope.Company), active.ObjectID); err != nil {
				return err
			}
			held <- struct{}{}
			<-held
			return errBlockerRollsBack
		})
	}()
	select {
	case <-held:
	case err := <-locked:
		t.Fatalf("lock: %v", err)
	}
	start := time.Now()
	if s := read(d.scope, active.ObjectID); !s.Known || time.Since(start) > 5*time.Second {
		t.Fatalf("read under the order's lock %+v after %v", s, time.Since(start))
	}
	held <- struct{}{}
	if err := <-locked; !errors.Is(err, errBlockerRollsBack) {
		t.Fatalf("lock: %v", err)
	}
	var definer, executable, public bool
	var config []string
	q.must(pgtest.AppPool(t).QueryRow(context.Background(), `SELECT p.prosecdef, p.proconfig,
		has_function_privilege('brant_app', p.oid, 'EXECUTE'), has_function_privilege('public', p.oid, 'EXECUTE')
		FROM pg_proc p WHERE p.oid = 'brant_trd.order_state_v1(text)'::regprocedure`).Scan(&definer, &config, &executable, &public))
	if !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" || !executable || public {
		t.Fatalf("security definer %v, config %v, brant_app %v, public %v", definer, config, executable, public)
	}
}

// PL-12 and PL-13 end to end on TRD's real order (S5 B6's consumer under B4's wiring): an order on
// 30/70 terms activates on its 672.00 reservation (CreditGuard v1 in TRD's transaction); TRD's own
// OrderActivated, relayed from TRD's Outbox through the dispatcher, has the consumer
// stl.order_credit consume that reservation for the activation's binding and instant, and the
// consumer stl.order_account (S5 B6b) open the order account at the activated revision; TRD's
// OrderCancelled then has stl.order_credit release the consumed reservation. Every event is settled.
func TestActivatedOrderCreditIsConsumedAndReleasedThroughTheRelay(t *testing.T) {
	q := newCreditOrderWorld(t, "")
	ctx := context.Background()
	scope := q.scope("company.A")
	subs := q.c.Subscriptions()
	ev := newOrderEvents(t, subs...)
	if _, err := q.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	o := q.activeOrder("quote.1")
	b := q.orderBinding(o.ObjectID)
	res := q.credit.reserved[b]
	if res.ReservationID == "" || res.PortionMinor != "67200" {
		t.Fatalf("the reservation activeOrder made %+v", res)
	}
	activatedAt := q.getOrder(o.ObjectID).Commits[0].CommittedAt
	first := ev.copyFrom(scope, trdapi.Source)
	ev.drain(ev.relay(5))
	ev.published(first...)
	v, err := q.c.STL.GetReservation(q.credit.officer, res.ReservationID)
	q.must(err)
	if v.Status != stlapi.ReservationConsumed || v.Version != 2 || v.ConsumedBy != consumerActor || !v.ActivatedAt.Equal(activatedAt) ||
		v.OrderRevision != 1 || v.OrderHash != b.OrderHash || q.stlEvents(stlapi.EventCreditReservationConsumed, res.ReservationID) != 1 {
		t.Fatalf("after TRD's OrderActivated: %+v (activated at %v)", v, activatedAt)
	}
	acc, err := q.c.STL.GetOrderAccount(q.credit.officer, o.ObjectID)
	q.must(err)
	if acc.Status != "open" || acc.Version != 1 || acc.OpenedBy != accountActor || acc.OrderRevision != 1 || acc.OrderHash != b.OrderHash ||
		len(acc.Stages) != 2 {
		t.Fatalf("the order account stl.order_account opened: %+v", acc)
	}

	_, err = q.c.TRD.CancelOrder(q.author, trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: o.ObjectID, ExpectedVersion: q.orderVersion(o.ObjectID),
		Reason: "客户撤单", Evidence: q.proof})
	q.must(err)
	later := ev.copyFrom(scope, trdapi.Source)
	ev.drain(ev.relay(5))
	ev.published(later...)
	v, err = q.c.STL.GetReservation(q.credit.officer, res.ReservationID)
	q.must(err)
	if v.Status != stlapi.ReservationReleased || v.Version != 3 || v.ReleasedBy != consumerActor ||
		q.stlEvents(stlapi.EventCreditReservationReleased, res.ReservationID) != 1 {
		t.Fatalf("after TRD's OrderCancelled: %+v", v)
	}
}
