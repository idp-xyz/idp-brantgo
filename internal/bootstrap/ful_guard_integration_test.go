//go:build integration

package bootstrap_test

import (
	"sync"
	"testing"
	"time"

	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
)

// FUL-INV-001 under concurrency (WBS S5-03 acceptance "两个出运争最后的数量——一个成功", PG16; S5
// decision D-3): two shipments of 80 of the 120 PCS line, each on its own release and delivery line,
// so only the order line binds them. While the first confirmation holds the order's fulfilment root
// (held right after its last guard), the second waits on that root; once the first commits, the
// second reads its line and is SHIPMENT_QUANTITY_EXCEEDED. Then three rounds of two confirmations
// racing freely end with exactly one winner each and never more than the line.
func TestTwoConfirmationsNeverExceedTheOrderLine(t *testing.T) {
	w := newFulWorld(t)
	rel1, rel2 := w.released("commitment.1", "120"), w.released("commitment.2", "120")
	d1, d2 := w.delivered("commitment.1", "120"), w.delivered("commitment.2", "120")
	_, err := w.prepare(w.merch, "shipment.a", "80", d1, rel1)
	w.must(err)
	_, err = w.prepare(w.merch2, "shipment.b", "80", d2, rel2)
	w.must(err)
	w.granted("shipment.a")
	w.granted("shipment.b")
	w.hold.set("order")
	first := make(chan error, 1)
	go func() { _, err := w.confirm(w.merch, "shipment.a", 1); first <- err }()
	w.hold.wait(t, first)
	second := make(chan error, 1)
	go func() { _, err := w.confirm(w.merch2, "shipment.b", 1); second <- err }()
	select {
	case err := <-second:
		w.hold.open()
		t.Fatalf("the second confirmation did not wait for the fulfilment root: %v (first: %v)", err, <-first)
	case <-time.After(500 * time.Millisecond):
	}
	w.hold.open()
	if err := <-first; err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := <-second; code(err) != fulapi.CodeShipmentQuantityExceeded || path(err) != "/shipment/lines/0/quantity" {
		t.Fatalf("second: %s at %s", code(err), path(err))
	}

	for round := range 3 {
		r := newRace(t, w, round)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = r.confirmAs(i)
			}()
		}
		wg.Wait()
		won, refused := 0, 0
		for _, err := range errs {
			switch code(err) {
			case "OK":
				won++
			case fulapi.CodeShipmentQuantityExceeded:
				refused++
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if won != 1 || refused != 1 {
			t.Fatalf("round %d: %d won, %d refused", round, won, refused)
		}
	}
}

// race is one round of two prepared shipments that cannot both ship.
type race struct {
	w   *fulWorld
	ids [2]string
}

// newRace prepares, on a new order of world w ("quote.r<round>", 120 PCS), two shipments of 70 on
// two releases and delivery lines of their own.
func newRace(t *testing.T, w *fulWorld, round int) *race {
	t.Helper()
	suffix := string(rune('a' + round))
	order := w.activeOrder("quote.r" + suffix)
	w.payable(order)
	saved := w.order
	w.order = order
	defer func() { w.order = saved }()
	r := &race{w: w}
	for i := range 2 {
		c := "commitment.r" + suffix + string(rune('1'+i))
		rel, d := w.released(c, "70"), w.delivered(c, "70")
		r.ids[i] = "shipment.r" + suffix + string(rune('1'+i))
		_, err := w.prepare(w.merch, r.ids[i], "70", d, rel)
		w.must(err)
		w.granted(r.ids[i])
	}
	return r
}

func (r *race) confirmAs(i int) (fulapi.ShipmentReceipt, error) {
	ctx := r.w.merch
	if i == 1 {
		ctx = r.w.merch2
	}
	return r.w.confirm(ctx, r.ids[i], 1)
}

// QualityReleaseGuard v1 in both commit orders (R26, ADR-BR-004 option B, ADR-BR-011, QLT-INV-002):
// a revocation that committed first refuses the confirmation (TestConfirmationRechecksEveryGuard);
// a confirmation that holds the release FOR SHARE makes quality_release.revoke wait until it
// commits, and the next shipment on the revoked release is QUALITY_RELEASE_REQUIRED. The shipment
// that committed keeps what it shipped.
func TestQualityRevocationWaitsForTheConfirmation(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "100")
	d := w.delivered("commitment.1", "100")
	_, err := w.prepare(w.merch, "shipment.1", "40", d, rel)
	w.must(err)
	_, err = w.prepare(w.merch, "shipment.2", "40", d, rel)
	w.must(err)
	w.granted("shipment.1")
	w.granted("shipment.2")
	w.hold.set("release")
	done := make(chan error, 1)
	go func() { _, err := w.confirm(w.merch, "shipment.1", 1); done <- err }()
	w.hold.wait(t, done)
	revoked := make(chan error, 1)
	go func() {
		_, err := w.qlt.RevokeRelease(w.qc, qltapi.RevokeRelease{IdempotencyKey: w.key(), ReleaseID: rel, Reason: "复检发现色差，撤回放行"})
		revoked <- err
	}()
	select {
	case err := <-revoked:
		w.hold.open()
		t.Fatalf("the revocation did not wait: %v (confirmation: %v)", err, <-done)
	case <-time.After(500 * time.Millisecond):
	}
	w.hold.open()
	if err := <-done; err != nil {
		t.Fatalf("the confirmation holding the guard: %v", err)
	}
	if err := <-revoked; err != nil {
		t.Fatalf("the revocation after FUL committed: %v", err)
	}
	if v := w.shipment("shipment.1"); v.Status != fulapi.StatusConfirmed {
		t.Fatalf("the confirmed shipment stays %+v", v)
	}
	_, err = w.confirm(w.merch, "shipment.2", 1)
	w.want(err, fulapi.CodeQualityReleaseRequired)
	if got, err := w.shipped("company.A", w.order.ObjectID); err != nil || len(got) != 1 {
		t.Fatalf("what shipped stays %q %v", got, err)
	}
}

// OrderGuard v1 and ShipmentFacts v1 (R27, R70; S5 decision D-1): while a confirmation holds the
// order row FOR SHARE, TRD's order.approval.revoke waits for it, and ShipmentFacts v1 — read without
// locks, as TRD reads it after its own order lock — answers at once with what committed before.
// After the confirmation commits the revocation goes through, ShipmentFacts v1 counts the
// shipment, and the next confirmation is ORDER_APPROVAL_REVOKED.
func TestOrderRevocationWaitsForTheConfirmation(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "100")
	d := w.delivered("commitment.1", "100")
	_, err := w.prepare(w.merch, "shipment.1", "40", d, rel)
	w.must(err)
	_, err = w.prepare(w.merch, "shipment.2", "40", d, rel)
	w.must(err)
	w.granted("shipment.1")
	w.granted("shipment.2")
	w.hold.set("order")
	done := make(chan error, 1)
	go func() { _, err := w.confirm(w.merch, "shipment.1", 1); done <- err }()
	w.hold.wait(t, done)
	type answer struct {
		rows []string
		err  error
	}
	read := make(chan answer, 1)
	go func() { rows, err := w.shipped("company.A", w.order.ObjectID); read <- answer{rows, err} }()
	var during answer
	select {
	case during = <-read:
	case <-time.After(5 * time.Second):
		w.hold.open()
		t.Fatalf("ShipmentFacts v1 waited on the confirmation's locks (confirmation: %v)", <-done)
	}
	revoked := make(chan error, 1)
	go func() {
		_, err := w.c.TRD.RevokeOrderApproval(w.reviewer, trdapi.RevokeOrderApproval{IdempotencyKey: w.key(), OrderID: w.order.ObjectID,
			ReviewID: "review.quote.1", Reason: "客户条件变化"})
		revoked <- err
	}()
	select {
	case err := <-revoked:
		w.hold.open()
		t.Fatalf("the revocation did not wait: %v (confirmation: %v)", err, <-done)
	case <-time.After(500 * time.Millisecond):
	}
	w.hold.open()
	if err := <-done; err != nil {
		t.Fatalf("the confirmation holding the order: %v", err)
	}
	if err := <-revoked; err != nil {
		t.Fatalf("the revocation after FUL committed: %v", err)
	}
	if during.err != nil || len(during.rows) != 0 {
		t.Fatalf("lock-free read during the confirmation %q %v", during.rows, during.err)
	}
	if got, err := w.shipped("company.A", w.order.ObjectID); err != nil || len(got) != 1 {
		t.Fatalf("after the commit %q %v", got, err)
	}
	_, err = w.confirm(w.merch, "shipment.2", 1)
	w.want(err, fulapi.CodeOrderApprovalRevoked)
}
