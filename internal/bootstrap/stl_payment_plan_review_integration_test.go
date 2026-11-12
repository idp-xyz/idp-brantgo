//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// STL follows an order change (unit C4b-STL, WBS S5-09b; event storming PL-09; WF-06 step 6;
// SM-STL-PAYMENT-PLAN-REVIEW; STL-INV-003; S5 plan D-4, D-9, D-18, D-21; BT10's payment-plan half)
// on PostgreSQL 16, through the assembled relay with the in-process dispatcher and the assembled
// replayer. TRD's own OrderAmended of real amendments (the credit order world, 30 % on order
// confirmation / 70 % 30 days after shipment, 120 PCS at 8.00 = 960.00) is relayed to STL's two
// consumers: stl.order_account opens the payment plan review and moves the order account to the
// new revision; stl.order_credit consumes the new revision's reservation and releases the earlier
// one. The stand-in world (TRD's answers steered, OrderAmended built field for field as TRD's
// ledger writes it) covers two workers and late replays.

// reviewTreasury gives the order world the company's USD bank account and a treasurer, who holds
// payment_plan_review.open and .settle (template treasury).
func (q *trdWorld) reviewTreasury() (context.Context, polapi.ConfigRef) {
	q.t.Helper()
	b := q.configure(q.author, q.reviewer, polapi.KindBankAccount, "bank.usd", 0, q.win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "USD", polapi.AccountBoth), true)
	return q.assign("treasury.1", "treasury"), b
}

// reviewReceipt has the treasurer record customer.1's USD receipt fund of received.
func (q *trdWorld) reviewReceipt(treasurer context.Context, b polapi.ConfigRef, fund, received string) {
	q.t.Helper()
	_, err := q.c.STL.RecordFund(treasurer, stlapi.RecordFund{IdempotencyKey: q.key(), FundRecordID: fund, FundKind: stlapi.FundReceipt,
		CounterpartyID: "customer.1", Currency: stlRef(q.usd), BankAccount: stlRef(b), ReceivedAmount: received,
		ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "ref." + fund, EvidenceIDs: []string{"evidence"}})
	q.must(err)
}

// reviewApply has the treasurer apply amount of fund to stage of order as application app.
func (q *trdWorld) reviewApply(treasurer context.Context, app, fund, order, stage, amount string) stlapi.ApplicationReceipt {
	q.t.Helper()
	r, err := q.c.STL.Apply(treasurer, stlapi.Apply{IdempotencyKey: q.key(), ApplicationID: app, FundRecordID: fund,
		Target: stlapi.ApplicationTarget{Kind: stlapi.TargetOrderStage, OrderID: order, StageID: stage}, Amount: amount})
	q.must(err)
	return r
}

// reviewAmend has amendment id of order (L1 to qty) prepared, submitted, approved and applied at
// the order's current revision; grows reserves the revision it proposes first (D-9). It returns
// the apply's receipt.
func (q *trdWorld) reviewAmend(order, id, qty string, grows bool) trdapi.AmendmentReceipt {
	q.t.Helper()
	b := q.orderBinding(order)
	d := q.approvedAmendment(order, id, b.OrderRevision, b.OrderHash, qty)
	if grows {
		q.readyToApply(order, id)
	}
	r, err := q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(order, id, d.RowVersion))
	q.must(err)
	return r
}

// relayTRD relays TRD's Outbox events not relayed before and requires each settled.
func relayTRD(ev *orderEvents, scope kernel.Scope) []eventing.Envelope {
	ev.t.Helper()
	out := ev.copyFrom(scope, trdapi.Source)
	ev.drain(ev.relay(5))
	ev.published(out...)
	return out
}

// ofType returns the envelopes of typ among envs.
func ofType(envs []eventing.Envelope, typ string) []eventing.Envelope {
	var out []eventing.Envelope
	for _, e := range envs {
		if string(e.Type) == typ {
			out = append(out, e)
		}
	}
	return out
}

// reviewGuard calls PaymentConditionGuard v1 for order at revision, scope shipment, in TRD's
// transaction with TRD's executor, as FUL's release does.
func (q *trdWorld) reviewGuard(order string, revision int64) stlapi.PaymentVerdict {
	q.t.Helper()
	var v stlapi.PaymentVerdict
	query := stlapi.PaymentQuery{OrderID: order, OrderRevision: revision, Scope: stlapi.ScopeShipment, At: time.Now()}
	q.must(q.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = stlapi.ScanPaymentCondition(query, exec.QueryRow(ctx, stlapi.PaymentConditionSQL, stlapi.PaymentConditionArgs(query)...).Scan)
		return err
	}))
	return v
}

// wantItem requires review item it to be kind on stage with from, to and applied (minor units).
func wantItem(t *testing.T, it stlapi.ReviewItemView, seq int, kind, stage, from, to, applied string) {
	t.Helper()
	if it.Seq != seq || it.Kind != kind || it.StageID != stage || it.FromMinor != from || it.ToMinor != to || it.AppliedMinor != applied {
		t.Fatalf("item %+v, want %d %s %s %s→%s applied %s", it, seq, kind, stage, from, to, applied)
	}
}

// stageApplied returns what the account shows applied on stage.
func stageApplied(v stlapi.OrderAccountView, stage string) string {
	for _, s := range v.Stages {
		if s.StageID == stage {
			return s.AppliedMinor
		}
	}
	return ""
}

// BT10 (payment-plan half), PL-09, WF-06, SM-STL-PAYMENT-PLAN-REVIEW, STL-INV-003, D-4, D-21 on TRD's
// real amendments. The order activates (672.00 reserved), its account opens at revision 1 and the
// customer's 288.00 deposit is applied. Reduced to 90 PCS (revision 2: 216.00 + 504.00; no more
// credit, so no reservation), TRD's OrderAmended moves the account to revision 2 with the new
// stages: the deposit application stays a fact at revision 1 and stands on P1 of revision 2, which
// it now over-pays — an item of the review, beside the changed credit portion; the guard answers
// for revision 2 (satisfied, not revision_mismatch). Reduced again to 80 PCS before anyone settled,
// revision 3's review supersedes revision 2's. A person of treasury settles revision 3's review:
// not a sales clerk, not with an item marked corrected that is not, and every item once; after the
// excess is unapplied and the deposit re-applied, it settles. A redelivered OrderAmended of
// revision 2 changes nothing; the credit stays on revision 1's consumed reservation throughout.
func TestAmendedOrderMovesItsAccountThroughTheRelay(t *testing.T) {
	q := newCreditOrderWorld(t, "")
	ctx := context.Background()
	scope := q.scope("company.A")
	subs := q.c.Subscriptions()
	ev := newOrderEvents(t, subs...)
	if _, err := q.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	treasurer, usdBank := q.reviewTreasury()
	id := q.activeOrder("quote.1").ObjectID
	b1 := q.orderBinding(id)
	first := q.credit.reserved[b1]
	relayTRD(ev, scope)
	q.reviewReceipt(treasurer, usdBank, "fund.r1", "288.00")
	q.reviewApply(treasurer, "app.deposit", "fund.r1", id, "P1", "288.00")
	if v := q.reviewGuard(id, 1); !v.Satisfied() || v.AppliedMinor != "28800" {
		t.Fatalf("revision 1 with its deposit: %+v", v)
	}
	exposure := func() string {
		v, err := q.c.STL.GetCustomerCredit(q.credit.officer, "customer.1")
		q.must(err)
		return v.ExposureMinor
	}
	if e := exposure(); e != "67200" {
		t.Fatalf("exposure at revision 1: %s", e)
	}

	r2 := q.reviewAmend(id, "amendment.less", "90", false)
	if v := q.reviewGuard(id, 2); v.Reason != stlapi.PaymentRevisionMismatch {
		t.Fatalf("revision 2 before STL heard of it: %+v", v)
	}
	second := relayTRD(ev, scope)
	acc, err := q.c.STL.GetOrderAccount(treasurer, id)
	q.must(err)
	if acc.OrderRevision != 2 || acc.OrderHash != r2.OrderHash || acc.Version != 2 || len(acc.Stages) != 2 || acc.Stages[0].AmountMinor != "21600" ||
		acc.Stages[1].AmountMinor != "50400" || stageApplied(acc, "P1") != "28800" || len(acc.Applications) != 1 ||
		acc.Applications[0].OrderRevision != 1 {
		t.Fatalf("account after revision 2: %+v", acc)
	}
	// P1 stands 288.00 on 216.00 due: the guard's total counts its due only (stl 000004, C1's finding).
	if v := q.reviewGuard(id, 2); !v.Satisfied() || v.OrderHash != r2.OrderHash || v.RequiredMinor != "21600" || v.AppliedMinor != "21600" {
		t.Fatalf("revision 2's guard: %+v", v)
	}
	if v := q.reviewGuard(id, 1); v.Reason != stlapi.PaymentRevisionMismatch {
		t.Fatalf("revision 1's guard after the move: %+v", v)
	}
	list, err := q.c.STL.ListPaymentPlanReviews(treasurer, id)
	q.must(err)
	if len(list.Reviews) != 1 || list.AccountVersion != 2 {
		t.Fatalf("reviews %+v", list)
	}
	rv := list.Reviews[0]
	if rv.OrderRevision != 2 || rv.OrderHash != r2.OrderHash || rv.FromRevision != 1 || rv.FromHash != b1.OrderHash || rv.Status != stlapi.ReviewOpen ||
		rv.ResponsibleRole != "treasury" || rv.OpenedBy != accountActor || len(rv.Items) != 2 {
		t.Fatalf("revision 2's review %+v", rv)
	}
	wantItem(t, rv.Items[0], 1, stlapi.ItemStageOverApplied, "P1", "28800", "21600", "28800")
	wantItem(t, rv.Items[1], 2, stlapi.ItemCreditPortion, "", "67200", "50400", "0")
	if r := q.reservationOf(first.ReservationID); r.Status != stlapi.ReservationConsumed || r.Version != 2 || exposure() != "67200" {
		t.Fatalf("revision 2 stands on revision 1's reservation: %+v, exposure %s", r, exposure())
	}

	r3 := q.reviewAmend(id, "amendment.fewer", "80", false)
	relayTRD(ev, scope)
	list, err = q.c.STL.ListPaymentPlanReviews(treasurer, id)
	q.must(err)
	if len(list.Reviews) != 2 || list.OrderRevision != 3 || list.AccountVersion != 3 {
		t.Fatalf("reviews after revision 3: %+v", list)
	}
	rv3, rv2 := list.Reviews[0], list.Reviews[1]
	if rv3.OrderRevision != 3 || rv3.OrderHash != r3.OrderHash || rv3.FromRevision != 2 || rv3.Status != stlapi.ReviewOpen || len(rv3.Items) != 2 ||
		rv2.Status != stlapi.ReviewSuperseded || rv2.Version != 2 || rv2.SupersededBy != 3 || rv2.SupersededAt.IsZero() {
		t.Fatalf("revision 3 supersedes revision 2: %+v / %+v", rv3, rv2)
	}
	wantItem(t, rv3.Items[0], 1, stlapi.ItemStageOverApplied, "P1", "21600", "19200", "28800")
	wantItem(t, rv3.Items[1], 2, stlapi.ItemCreditPortion, "", "50400", "44800", "0")

	settle := func(who context.Context, rev, version int64, items ...stlapi.ReviewDisposition) (stlapi.PaymentPlanReviewReceipt, error) {
		return q.c.STL.SettlePaymentPlanReview(who, stlapi.SettlePaymentPlanReview{IdempotencyKey: q.key(), OrderID: id, OrderRevision: rev,
			ExpectedVersion: version, Reason: "客户确认减量，定金差额退回", Items: items})
	}
	corrected := stlapi.ReviewDisposition{Seq: 1, Disposition: stlapi.DispositionCorrected, Note: "多收定金 96.00 撤销核销后重新核销 192.00"}
	accepted := stlapi.ReviewDisposition{Seq: 2, Disposition: stlapi.DispositionAccepted, Note: "信用占用仍按第 1 版 672.00 保留至订单结束"}
	_, err = settle(treasurer, 2, 3, stlapi.ReviewDisposition{Seq: 1, Disposition: stlapi.DispositionAccepted, Note: "n"},
		stlapi.ReviewDisposition{Seq: 2, Disposition: stlapi.DispositionAccepted, Note: "n"})
	wantRefused(t, "settling the superseded review", err, kernel.CodeInvalidState, "/args/order_revision")
	if _, err = settle(q.credit.clerk, 3, 3, corrected, accepted); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("a sales clerk settling: %v", err)
	}
	_, err = settle(treasurer, 3, 3, corrected)
	wantRefused(t, "one item of two", err, kernel.CodeInvalidKnownValue, "/args/items")
	_, err = settle(treasurer, 3, 3, corrected, accepted)
	wantRefused(t, "corrected while 288.00 stands on a 192.00 stage", err, kernel.CodeInvalidState, "/args/items/0/disposition")
	_, err = settle(treasurer, 3, 2, corrected, accepted)
	wantRefused(t, "an earlier account version", err, kernel.CodeVersionConflict, "/args/expected_version")

	_, err = q.c.STL.Unapply(treasurer, stlapi.Unapply{IdempotencyKey: q.key(), ApplicationID: "app.deposit", ExpectedVersion: 1,
		Reason: "订单减量，定金按第 3 版重新核销"})
	q.must(err)
	if r := q.reviewApply(treasurer, "app.deposit.r3", "fund.r1", id, "P1", "192.00"); r.OrderRevision != 3 || r.StageOpenMinor != "0" {
		t.Fatalf("the deposit applied at revision 3: %+v", r)
	}
	s, err := settle(treasurer, 3, 3, accepted, corrected)
	q.must(err)
	if s.Status != stlapi.ReviewSettled || s.RowVersion != 4 || s.OrderRevision != 3 || s.FromRevision != 2 || s.Items != 2 {
		t.Fatalf("settled %+v", s)
	}
	list, err = q.c.STL.ListPaymentPlanReviews(treasurer, id)
	q.must(err)
	if rv3 = list.Reviews[0]; rv3.Status != stlapi.ReviewSettled || rv3.Version != 2 || rv3.SettledBy.ID != "treasury.1" ||
		rv3.SettleReason != "客户确认减量，定金差额退回" || rv3.Items[0].Disposition != stlapi.DispositionCorrected || rv3.Items[1].Note != accepted.Note {
		t.Fatalf("revision 3's settled review %+v", rv3)
	}
	if _, err = settle(treasurer, 3, 4, accepted, corrected); code(err) != kernel.CodeInvalidState {
		t.Fatalf("settled twice: %v", err)
	}
	if v := q.reviewGuard(id, 3); !v.Satisfied() || v.AppliedMinor != "19200" || v.OrderHash != r3.OrderHash {
		t.Fatalf("revision 3's guard: %+v", v)
	}

	redo, err := messaging.NewDispatcher(subs...)
	q.must(err)
	for _, e := range ofType(second, trdapi.EventOrderAmended) {
		if err = redo.Publish(ctx, e); err != nil {
			t.Fatalf("redelivery of revision 2's OrderAmended: %v", err)
		}
	}
	acc, err = q.c.STL.GetOrderAccount(treasurer, id)
	q.must(err)
	if acc.OrderRevision != 3 || acc.Version != 4 || q.count("company.A", "brant_stl.payment_plan_review WHERE order_id = '"+id+"'") != 2 {
		t.Fatalf("after the redelivery: %+v", acc)
	}
	if r := q.reservationOf(first.ReservationID); r.Status != stlapi.ReservationConsumed || r.Version != 2 || exposure() != "67200" {
		t.Fatalf("the credit after three revisions: %+v, exposure %s", r, exposure())
	}
}

func (q *trdWorld) reservationOf(id string) stlapi.ReservationView {
	q.t.Helper()
	v, err := q.c.STL.GetReservation(q.credit.officer, id)
	q.must(err)
	return v
}

// D-9, WF-06 ("信用部分增加时须新的占用"), D-4, PL-12, SM-STL-CREDIT-RESERVATION on TRD's real
// amendment: raised to 130 PCS (revision 2: 312.00 + 728.00), the order asks more credit. STL
// reserves revision 2 before apply; from then on the exposure counts revision 2's 728.00 in place
// of revision 1's 672.00 (D-4: an order's latest revision), and it stays 728.00 through the apply,
// the consume and the release. TRD's OrderAmended names the reservation; stl.order_credit consumes
// it at the amendment's applied_at and releases revision 1's consumed reservation, naming the
// event; the review lists the grown credit portion. Redelivered by two dispatchers at once, the
// event changes nothing more. When the order is cancelled, only revision 2's reservation is left
// to release.
func TestAmendmentThatGrowsTheCreditMovesTheReservation(t *testing.T) {
	q := newCreditOrderWorld(t, "")
	ctx := context.Background()
	scope := q.scope("company.A")
	subs := q.c.Subscriptions()
	ev := newOrderEvents(t, subs...)
	if _, err := q.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	id := q.activeOrder("quote.1").ObjectID
	b1 := q.orderBinding(id)
	first := q.credit.reserved[b1]
	relayTRD(ev, scope)
	exposure := func() string {
		v, err := q.c.STL.GetCustomerCredit(q.credit.officer, "customer.1")
		q.must(err)
		return v.ExposureMinor
	}
	if r := q.reservationOf(first.ReservationID); r.Status != stlapi.ReservationConsumed || exposure() != "67200" {
		t.Fatalf("revision 1 activated: %+v, exposure %s", r, exposure())
	}

	d := q.approvedAmendment(id, "amendment.more", 1, b1.OrderHash, "130")
	p := q.readyToApply(id, "amendment.more")
	next := q.credit.reserved[p.Binding]
	if next.PortionMinor != "72800" || p.Binding.OrderRevision != 2 || exposure() != "72800" {
		t.Fatalf("revision 2 reserved: %+v, exposure %s", next, exposure())
	}
	r2, err := q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(id, "amendment.more", d.RowVersion))
	q.must(err)
	amendedEvents := q.eventsOf(trdapi.EventOrderAmended)
	if len(amendedEvents) != 1 || amendedEvents[0].Payload["credit_guard"] != stlapi.CreditCovered ||
		amendedEvents[0].Payload["credit_reservation_id"] != next.ReservationID || exposure() != "72800" {
		t.Fatalf("OrderAmended %v, exposure %s", amendedEvents, exposure())
	}
	appliedAt, err := kernel.ParseTimestamp(fmt.Sprint(amendedEvents[0].Payload["applied_at"]))
	q.must(err)
	relayed := relayTRD(ev, scope)
	v2 := q.reservationOf(next.ReservationID)
	if v2.Status != stlapi.ReservationConsumed || v2.Version != 2 || v2.ConsumedBy != consumerActor || !v2.ActivatedAt.Equal(appliedAt) ||
		v2.OrderRevision != 2 || v2.OrderHash != r2.OrderHash {
		t.Fatalf("revision 2's reservation: %+v (applied at %v)", v2, appliedAt)
	}
	v1 := q.reservationOf(first.ReservationID)
	if v1.Status != stlapi.ReservationReleased || v1.Version != 3 || v1.ReleasedBy != consumerActor ||
		!strings.Contains(v1.ReleaseReason, "superseded by reservation "+next.ReservationID) || exposure() != "72800" {
		t.Fatalf("revision 1's reservation: %+v, exposure %s", v1, exposure())
	}
	list, err := q.c.STL.ListPaymentPlanReviews(q.credit.officer, id)
	q.must(err)
	if len(list.Reviews) != 1 || len(list.Reviews[0].Items) != 1 {
		t.Fatalf("reviews %+v", list)
	}
	wantItem(t, list.Reviews[0].Items[0], 1, stlapi.ItemCreditPortion, "", "67200", "72800", "0")

	amended := ofType(relayed, trdapi.EventOrderAmended)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		dd, err := messaging.NewDispatcher(subs...)
		q.must(err)
		wg.Go(func() { errs[i] = dd.Publish(ctx, amended[0]) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil && errors.Is(err, eventing.ErrPublishRejected) {
			t.Fatalf("redelivery rejected: %v", err)
		}
	}
	if a, b := q.reservationOf(next.ReservationID), q.reservationOf(first.ReservationID); a.Version != 2 || b.Version != 3 ||
		q.stlEvents(stlapi.EventCreditReservationConsumed, next.ReservationID) != 1 ||
		q.stlEvents(stlapi.EventCreditReservationReleased, first.ReservationID) != 1 {
		t.Fatalf("after redeliveries: %+v %+v", a, b)
	}

	_, err = q.c.TRD.CancelOrder(q.author, trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: id, ExpectedVersion: q.orderVersion(id),
		Reason: "客户撤单", Evidence: q.proof})
	q.must(err)
	relayTRD(ev, scope)
	if a, b := q.reservationOf(next.ReservationID), q.reservationOf(first.ReservationID); a.Status != stlapi.ReservationReleased || a.Version != 3 ||
		b.Version != 3 || q.stlEvents(stlapi.EventCreditReservationReleased, first.ReservationID) != 1 {
		t.Fatalf("after the cancellation: %+v %+v", a, b)
	}
}

// amendment is TRD's OrderAmended of order o (at its revision and hash, amended from the revision
// before) at row version, applied now; reservation is the one TRD's CreditGuard found ("": none
// asked, not_increased).
func amendment(scope kernel.Scope, o stl.Order, version int64, reservation string) eventing.Envelope {
	guard := trdapi.GuardNotIncreased
	if reservation != "" {
		guard = stlapi.CreditCovered
	}
	return salesOrderEvent(scope, trdapi.EventOrderAmended, o.Binding, version, "active", map[string]any{"amendment_id": "amendment.1",
		"from_order_revision": o.Binding.OrderRevision - 1, "from_order_hash": orderHashOf(o.Binding.OrderID, o.Binding.OrderRevision-1),
		"applied_at": kernel.FormatTimestamp(kernel.Instant(time.Now())), "credit_guard": guard, "credit_reservation_id": reservation,
		"unchecked_guards": trdapi.GuardFULShipments})
}

// openReviews counts the committed payment_plan_review.open results of order in company.A.
func (w *world) openReviews(order string) int {
	return w.count("company.A", "brant_stl.action_result WHERE command = 'payment_plan_review.open' AND object_id = '"+order+"'")
}

// S4-06a, WF-06 idempotency ("各下游按（订单、订单修订）只开启一次复核"), D-18 with TRD's answers steered:
// OrderAmended of four orders relayed by two relays at once while a third worker delivers each
// directly moves each account once, with one review each; an order without an account gets one at
// TRD's current revision with a review that has nothing to dispose of. A dead-lettered OrderAmended
// replayed after a later amendment moved the account, or after the order ended, changes nothing;
// an OrderAmended of an order TRD does not show is refused for good.
func TestOrderAmendedOpensOneReviewPerRevision(t *testing.T) {
	cw := newCreditWorld(t)
	ctx := context.Background()
	scope := cw.scope("company.A")
	treasurer := cw.assign("treasury.1", "treasury")
	var broken atomic.Bool
	sub := breakable(cw.accountSubscription(), &broken, true)
	ev := newOrderEvents(t, sub)
	if _, err := cw.platform.ProvisionInbox(ctx, scope, []messaging.Subscription{sub}); err != nil {
		t.Fatal(err)
	}
	opened := func(o stl.Order) {
		t.Helper()
		_, err := cw.stl.OpenOrderAccount(treasurer, stlapi.OpenOrderAccount{IdempotencyKey: cw.key(), OrderID: o.Binding.OrderID,
			OrderRevision: o.Binding.OrderRevision, OrderHash: o.Binding.OrderHash})
		cw.must(err)
	}
	reviews := func(id string) stlapi.PaymentPlanReviewsView {
		t.Helper()
		v, err := cw.stl.ListPaymentPlanReviews(cw.officer, id)
		cw.must(err)
		return v
	}

	second, err := messaging.NewDispatcher(sub)
	cw.must(err)
	var events []eventing.Envelope
	var orders []stl.Order
	for i := range 4 {
		id := fmt.Sprintf("order.w%d", i)
		opened(cw.activeAt(id, 1, prepaid(100000)...))
		o := cw.activeAt(id, 2, prepaid(80000)...)
		events, orders = append(events, amendment(scope, o, 6, "")), append(orders, o)
	}
	none := cw.activeAt("order.none", 2, prepaid(50000)...)
	events = append(events, amendment(scope, none, 6, ""))
	ev.enqueue(events...)
	var wg sync.WaitGroup
	errs := make([]error, 2+len(events))
	for i := range 2 {
		wg.Go(func() { errs[i] = drain(ev.relay(5)) })
	}
	for i, e := range events {
		wg.Go(func() {
			if err := second.Publish(ctx, e); errors.Is(err, eventing.ErrPublishRejected) {
				errs[2+i] = err
			}
		})
	}
	wg.Wait()
	cw.must(errors.Join(errs...))
	ev.drain(ev.relay(5))
	ev.published(events...)
	for _, o := range orders {
		id := o.Binding.OrderID
		v := reviews(id)
		if v.OrderRevision != 2 || v.AccountVersion != 2 || len(v.Reviews) != 1 || v.Reviews[0].FromRevision != 1 || v.Reviews[0].OpenedBy != accountActor ||
			cw.openReviews(id) != 1 {
			t.Fatalf("%s with two workers: %+v, %d opens", id, v, cw.openReviews(id))
		}
		wantItem(t, v.Reviews[0].Items[0], 1, stlapi.ItemCreditPortion, "", "70000", "56000", "0")
	}
	acc, err := cw.account("order.none")
	cw.must(err)
	if v := reviews("order.none"); acc.OrderRevision != 2 || acc.Version != 1 || acc.OpenedBy != accountActor || !sameStages(acc, none.Stages) ||
		len(v.Reviews) != 1 || v.Reviews[0].FromRevision != 0 || v.Reviews[0].FromHash != "" || len(v.Reviews[0].Items) != 0 ||
		v.Reviews[0].Status != stlapi.ReviewOpen {
		t.Fatalf("an order without an account: %+v %+v", acc, v)
	}

	operator := cw.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"},
		Permissions: []string{messaging.ReplayPermission}})
	replayer := ev.replayer()
	deadLetter := func(e eventing.Envelope) {
		t.Helper()
		ev.enqueue(e)
		broken.Store(true)
		ev.once(ev.relay(1))
		broken.Store(false)
		if s := ev.status(e.ID); s != "ABANDONED" {
			t.Fatalf("broken consumer: outbox %s", s)
		}
	}
	replay := func(e eventing.Envelope) {
		t.Helper()
		if _, err := replayer.Replay(operator, e.Source, string(e.ID)); err != nil {
			t.Fatalf("replay %s: %v", e.Subject, err)
		}
	}

	opened(cw.activeAt("order.later", 1, prepaid(100000)...))
	late := amendment(scope, cw.activeAt("order.later", 2, prepaid(90000)...), 6, "")
	deadLetter(late)
	third := amendment(scope, cw.activeAt("order.later", 3, prepaid(80000)...), 8, "")
	ev.enqueue(third)
	ev.drain(ev.relay(5))
	ev.published(third)
	replay(late)
	if v := reviews("order.later"); v.OrderRevision != 3 || v.AccountVersion != 2 || len(v.Reviews) != 1 || v.Reviews[0].FromRevision != 1 {
		t.Fatalf("revision 2's OrderAmended replayed after revision 3's: %+v", v)
	}

	opened(cw.activeAt("order.ended", 1, prepaid(100000)...))
	endedEvent := amendment(scope, cw.activeAt("order.ended", 2, prepaid(90000)...), 6, "")
	deadLetter(endedEvent)
	cw.orders.setStatus("order.ended", "cancelled")
	replay(endedEvent)
	if v := reviews("order.ended"); v.OrderRevision != 1 || v.AccountVersion != 1 || len(v.Reviews) != 0 {
		t.Fatalf("OrderAmended replayed after the order ended: %+v", v)
	}

	unknown := stl.Order{Binding: stl.OrderBinding{OrderID: "order.unknown", OrderRevision: 2, OrderHash: orderHashOf("order.unknown", 2)}}
	e := amendment(scope, unknown, 6, "")
	ev.enqueue(e)
	ev.once(ev.relay(5))
	if alerts := ev.alerts.of(e.ID); ev.status(e.ID) != "ABANDONED" || len(alerts) != 1 ||
		!strings.Contains(alerts[0].Error, "stl.payment_plan_review_not_openable") {
		t.Fatalf("an order TRD does not show: outbox %s, alerts %+v", ev.status(e.ID), alerts)
	}
}

// S4-06a and PL-13 against D-9 with TRD's answers steered: an OrderAmended whose reservation the
// order's end released before the event arrived (a dead-letter replay) consumes nothing, and one
// replayed after a later amendment superseded its reservation consumes nothing either; the later
// amendment consumed its own reservation and released every earlier one.
func TestReplayedAmendmentNeverConsumesASupersededReservation(t *testing.T) {
	cw := newCreditWorld(t)
	ctx := context.Background()
	scope := cw.scope("company.A")
	cw.limit()
	var broken atomic.Bool
	var sub messaging.Subscription
	for _, s := range cw.stl.Subscriptions() {
		if s.Consumer == "stl.order_credit" {
			sub = breakable(s, &broken, true)
		}
	}
	ev := newOrderEvents(t, sub)
	if _, err := cw.platform.ProvisionInbox(ctx, scope, []messaging.Subscription{sub}); err != nil {
		t.Fatal(err)
	}
	operator := cw.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"},
		Permissions: []string{messaging.ReplayPermission}})
	replayer := ev.replayer()
	relay := func(es ...eventing.Envelope) {
		t.Helper()
		ev.enqueue(es...)
		ev.drain(ev.relay(5))
		ev.published(es...)
	}
	deadLetter := func(e eventing.Envelope) {
		t.Helper()
		ev.enqueue(e)
		broken.Store(true)
		ev.once(ev.relay(1))
		broken.Store(false)
		if s := ev.status(e.ID); s != "ABANDONED" {
			t.Fatalf("broken consumer: outbox %s", s)
		}
	}
	// activated reserves revision 1 of a draft on 30/70 terms and relays its activation (consumed).
	activated := func(id string) {
		t.Helper()
		o := cw.draftOrder(id, cw.usd, prepaid(100000)...)
		_, err := cw.reserve(cw.sales, "res."+id+".r1", o)
		cw.must(err)
		cw.orders.setStatus(id, stl.OrderActive)
		o.Status = stl.OrderActive
		a, _ := activation(scope, o, 5)
		relay(a)
	}
	// revised moves TRD's stand-in to revision rev (more on credit) and reserves it.
	revised := func(id string, rev int64) stl.Order {
		t.Helper()
		o := cw.activeAt(id, rev, prepaid(100000+rev*10000)...)
		_, err := cw.reserve(cw.sales, fmt.Sprintf("res.%s.r%d", id, rev), o)
		cw.must(err)
		return o
	}

	activated("order.end")
	r2 := amendment(scope, revised("order.end", 2), 6, "res.order.end.r2")
	deadLetter(r2)
	end := salesOrderEvent(scope, trdapi.EventOrderCancelled, stl.OrderBinding{OrderID: "order.end", OrderRevision: 2,
		OrderHash: orderHashOf("order.end", 2)}, 7, "cancelled", nil)
	relay(end)
	if _, err := replayer.Replay(operator, r2.Source, string(r2.ID)); err != nil {
		t.Fatal(err)
	}
	if a, b := cw.reservation("res.order.end.r1"), cw.reservation("res.order.end.r2"); a.Status != stlapi.ReservationReleased || a.Version != 3 ||
		b.Status != stlapi.ReservationReleased || b.Version != 2 || !b.ConsumedAt.IsZero() || b.ReleasedBy != consumerActor {
		t.Fatalf("OrderAmended replayed after the end: %+v / %+v", a, b)
	}

	activated("order.chain")
	late := amendment(scope, revised("order.chain", 2), 6, "res.order.chain.r2")
	deadLetter(late)
	relay(amendment(scope, revised("order.chain", 3), 8, "res.order.chain.r3"))
	r1, r2v, r3 := cw.reservation("res.order.chain.r1"), cw.reservation("res.order.chain.r2"), cw.reservation("res.order.chain.r3")
	if r3.Status != stlapi.ReservationConsumed || r3.OrderRevision != 3 || r1.Status != stlapi.ReservationReleased || r1.Version != 3 ||
		r2v.Status != stlapi.ReservationReleased || r2v.Version != 2 || !strings.Contains(r2v.ReleaseReason, "res.order.chain.r3") {
		t.Fatalf("revision 3's OrderAmended: %+v / %+v / %+v", r1, r2v, r3)
	}
	if _, err := replayer.Replay(operator, late.Source, string(late.ID)); err != nil {
		t.Fatal(err)
	}
	if r2v = cw.reservation("res.order.chain.r2"); r2v.Status != stlapi.ReservationReleased || r2v.Version != 2 || !r2v.ConsumedAt.IsZero() ||
		cw.reservation("res.order.chain.r3").Version != 2 {
		t.Fatalf("revision 2's OrderAmended replayed after revision 3's: %+v", r2v)
	}
	if v, err := cw.stl.GetCustomerCredit(cw.officer, "customer.1"); err != nil || v.ExposureMinor != "91000" {
		t.Fatalf("exposure: revision 3 of order.chain alone %+v %v", v.ExposureMinor, err)
	}
}

// The payment plan review over HTTP (contract brant.settlement/0.1.0; WF-06; D-21) on TRD's real
// amendment: the treasurer reads the open review of revision 2 at
// GET /api/brant/v1/stl/order_accounts/{id}/payment_plan_reviews, sends payment_plan_review.open
// for the revision the account holds (answered as it is, replayed), and settles it with
// payment_plan_review.settle {order_id, order_revision, expected_version, reason, items [{seq,
// disposition, note}]}; an order without an account has no reviews (404).
func TestPaymentPlanReviewOverHTTP(t *testing.T) {
	q := newCreditOrderWorld(t, "")
	ctx := context.Background()
	scope := q.scope("company.A")
	subs := q.c.Subscriptions()
	ev := newOrderEvents(t, subs...)
	if _, err := q.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	q.reviewTreasury()
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	treasurer := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "treasury.1", "human"), n: 700}

	id := q.activeOrder("quote.1").ObjectID
	relayTRD(ev, scope)
	r2 := q.reviewAmend(id, "amendment.less", "90", false)
	relayTRD(ev, scope)

	path := "/api/brant/v1/stl/order_accounts/" + id + "/payment_plan_reviews"
	status, out := treasurer.do("GET", path, nil)
	reviews, _ := out["reviews"].([]any)
	if status != http.StatusOK || out["order_revision"] != float64(2) || out["account_version"] != float64(2) || len(reviews) != 1 {
		t.Fatalf("GET reviews: %d %v", status, out)
	}
	rv := reviews[0].(map[string]any)
	items, _ := rv["items"].([]any)
	if rv["status"] != stlapi.ReviewOpen || rv["responsible_role"] != "treasury" || len(items) != 1 ||
		items[0].(map[string]any)["kind"] != stlapi.ItemCreditPortion || items[0].(map[string]any)["to_minor"] != "50400" {
		t.Fatalf("review over HTTP %v", rv)
	}
	if r := treasurer.command("stl", stlapi.Contract, stlapi.CommandOpenReview, map[string]any{"order_id": id, "order_revision": 2,
		"order_hash": r2.OrderHash}); r["status"] != stlapi.ReviewOpen || r["replayed"] != true {
		t.Fatalf("payment_plan_review.open of the revision the account holds: %v", r)
	}
	settle := map[string]any{"order_id": id, "order_revision": 2, "expected_version": 2, "reason": "减量后信用部分降低，占用保留至订单结束",
		"items": []any{map[string]any{"seq": 1, "disposition": stlapi.DispositionAccepted, "note": "第 1 版占用 672.00 保留"}}}
	r := treasurer.command("stl", stlapi.Contract, stlapi.CommandSettleReview, settle)
	if r["status"] != stlapi.ReviewSettled || r["row_version"] != float64(3) {
		t.Fatalf("payment_plan_review.settle %v", r)
	}
	status, out = treasurer.do("GET", path, nil)
	rv = out["reviews"].([]any)[0].(map[string]any)
	items = rv["items"].([]any)
	if status != http.StatusOK || rv["status"] != stlapi.ReviewSettled || items[0].(map[string]any)["disposition"] != stlapi.DispositionAccepted ||
		!strings.Contains(fmt.Sprint(rv["settled_by"]), "treasury.1") {
		t.Fatalf("settled review over HTTP: %d %v", status, rv)
	}
	if status, out = treasurer.do("GET", "/api/brant/v1/stl/order_accounts/order.nobody/payment_plan_reviews", nil); status != http.StatusNotFound {
		t.Fatalf("reviews of an order without an account: %d %v", status, out)
	}
}
