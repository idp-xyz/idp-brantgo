//go:build integration

package bootstrap_test

import (
	"context"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
)

// S5R unit R13 on PostgreSQL 16: goods an approved purchase disposition took out of the order never ship while
// the order is active (simulated decision S5R-D05, E0, 2026-11-13; Context Map R59 SupplierDeliveryGuard v2 beside
// v1; tactical model FUL-INV-003, PUR-INV-013; the follow-up of the R4 and R6 reports). Python V0.50 has none of it:
// the tests cite the design. The world is newFulPurWorld: TRD's real order (L1 120 PCS), commitment.1 issued for all
// 120, QLT's rel.real of 120, FUL's real shipments on PUR's real deliveries; the editor records and approves nothing
// it verified (it is the purchaser), the reviewer is the order reviewer who decides dispositions.
//
// Commit order (decided by R13): a disposition's approval and a shipment's confirmation meet at the commitment root
// — the decision holds it FOR UPDATE, SupplierDeliveryGuard v2 FOR SHARE.
//   - approval first: the confirmation reads, after the lock, what the approval took out and is refused
//     SUPPLIER_DELIVERY_INSUFFICIENT when the shipment needs it;
//   - confirmation first: the approval waits until the shipment committed and is then judged by PUR on what is verified
//     only (PUR sees no shipment: no FUL→PUR relationship); what it took out is then missing from every later shipment.

func (w *fulPurWorld) disposals() purDispose {
	return purDispose{w: w.scenWorld, m: func() *pur.Module { return w.c.PUR }}
}

// confirmed prepares shipment id of quantity from d, grants its shipment release and confirms it on module m.
func (w *fulPurWorld) confirmed(m *ful.Module, id, quantity string, d fulapi.DeliveryLineRef) fulapi.ShipmentReceipt {
	w.t.Helper()
	_, err := w.ship(m, w.merch, id, quantity, d)
	w.must(err)
	w.grantedOn(m, id, fulapi.ScopeShipment)
	r, err := m.ConfirmShipment(w.merch, w.confirmCmd(id, 1))
	w.must(err)
	return r
}

// heldFUL is a FUL module on the composed upstreams whose port to PUR holds its transaction right after the
// first SupplierDeliveryGuard v2 answer while the hold is set.
func (w *fulPurWorld) heldFUL() *ful.Module {
	w.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	w.must(err)
	fl, err := ful.New(db, ful.Upstream{Orders: ful.TRDOrders(w.c.TRD), Releases: ful.QLTReleases(w.c.QLT),
		Deliveries: heldDeliveries{Deliveries: ful.PURDeliveries(w.c.PUR), hold: w.hold}, Screening: ful.PTYScreening(), Sources: w.c.EVD,
		Compliance: ful.CNXCompliance(), Payments: ful.STLPayments(), Documents: ful.DocumentPorts(w.c.POL, w.c.PTY, w.c.TRD).WithCertificates(w.c.CNX),
		Costs: w.c.CST})
	w.must(err)
	return fl
}

// FUL-INV-003 with S5R-D05: commitment.1 line L1 has 100 verified over delivery.1 (60) and delivery.2 (40); 30 of
// delivery.1 shipped; an approved write_off takes 50 out of the order, so 20 may still ship. Delivery.2 alone has 40,
// yet a preparation of 21 from it is SUPPLIER_DELIVERY_INSUFFICIENT naming the disposition; 20 are prepared and their
// release granted; then a hold_for_reuse of 10 is approved, and the confirmation of the 20 — judged again on
// SupplierDeliveryGuard v2 — is refused; 10 ship, with the commitment line's totals in the guard evidence, and nothing
// more. PUR's lookup (R59) states the same totals.
func TestS5RD05DisposedGoodsNeverShipWhileTheOrderIsActive(t *testing.T) {
	w := newFulPurWorld(t)
	x := w.disposals()
	fl := w.c.FUL
	d1 := w.verified("delivery.1", "60", "60")
	d2 := w.verified("delivery.2", "40", "40")
	w.confirmed(fl, "shipment.1", "30", d1)
	x.approved(w.author, w.reviewer, "disp.writeoff", "commitment.1", purapi.DispositionWriteOff, "50")

	ref2 := purapi.DeliveryLineRef{DeliveryID: "delivery.2", LineID: "L1", Version: 1}
	got, err := w.c.PUR.ResolveDeliveryLines(w.merch, []purapi.DeliveryLineRef{ref2})
	w.must(err)
	if l := got[ref2]; l.VerifiedQuantity != "40" || l.CommitmentLineVerified != "100" || l.CommitmentLineDisposed != "50" {
		t.Fatalf("DeliveryLookups %+v", l)
	}

	_, err = w.ship(fl, w.merch, "shipment.2", "21", d2)
	if code(err) != fulapi.CodeSupplierDeliveryInsufficient || path(err) != "/args/lines/0/quantity" ||
		!strings.Contains(reasonOf(err), "100 verified and 50 disposed of") {
		t.Fatalf("21 of the 20 left: %s at %s (%s)", code(err), path(err), reasonOf(err))
	}
	_, err = w.ship(fl, w.merch, "shipment.2", "20", d2)
	w.must(err)
	w.grantedOn(fl, "shipment.2", fulapi.ScopeShipment)
	x.approved(w.author, w.reviewer, "disp.hold", "commitment.1", purapi.DispositionHoldForReuse, "10")
	_, err = fl.ConfirmShipment(w.merch, w.confirmCmd("shipment.2", 1))
	if code(err) != fulapi.CodeSupplierDeliveryInsufficient || path(err) != "/shipment/lines/0/quantity" {
		t.Fatalf("approved before the confirmation: %s at %s", code(err), path(err))
	}

	w.confirmed(fl, "shipment.3", "10", d2)
	v, err := fl.GetShipment(w.merch, "shipment.3")
	w.must(err)
	if got := guardValue(v, "supplier_commitment_line:commitment.1/L1"); got != "verified=100/disposed=60" {
		t.Fatalf("commitment line evidence %q", got)
	}
	if got := guardValue(v, "supplier_delivery:delivery.2/L1/v1"); got != "verified/verified_version=1/verified=40" {
		t.Fatalf("delivery line evidence %q", got)
	}
	_, err = w.ship(fl, w.merch, "shipment.4", "1", d1)
	w.want(err, fulapi.CodeSupplierDeliveryInsufficient)
}

// The commit order "approval first", concurrently: a transaction holding commitment.1's root FOR UPDATE, as
// purchase.commitment.disposition.decide does, approves the requested write_off of 50 of the 100 verified; the
// confirmation of a shipment of 60 waits in SupplierDeliveryGuard v2 until it commits, then reads the approval and is
// SUPPLIER_DELIVERY_INSUFFICIENT; the 50 left ship.
func TestS5RD05ApprovalFirstRefusesTheWaitingConfirmation(t *testing.T) {
	w := newFulPurWorld(t)
	x := w.disposals()
	fl := w.c.FUL
	d1 := w.verified("delivery.1", "100", "100")
	_, err := w.ship(fl, w.merch, "shipment.1", "60", d1)
	w.must(err)
	w.grantedOn(fl, "shipment.1", fulapi.ScopeShipment)
	_, err = x.dispose(w.author, "disp.1", "commitment.1", purapi.DispositionWriteOff, "50")
	w.must(err)

	ctx := context.Background()
	tx, err := pgtest.AppPool(t).Begin(ctx)
	w.must(err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`, w.tenant)
	w.must(err)
	_, err = tx.Exec(ctx, `SELECT 1 FROM brant_pur.purchase_commitment WHERE commitment_id = 'commitment.1' FOR UPDATE`)
	w.must(err)
	_, err = tx.Exec(ctx, `UPDATE brant_pur.purchase_commitment_disposition SET status = 'approved', row_version = 2, decided_by_kind = 'human',
		decided_by_id = 'reviewer', decide_epoch = 1, decided_at = now(), decision = 'approve', updated_at = now() WHERE disposition_id = 'disp.1'`)
	w.must(err)

	done := make(chan error, 1)
	go func() { _, err := fl.ConfirmShipment(w.merch, w.confirmCmd("shipment.1", 1)); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("the confirmation did not wait for the commitment root: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	w.must(tx.Commit(ctx))
	err = <-done
	if code(err) != fulapi.CodeSupplierDeliveryInsufficient || path(err) != "/shipment/lines/0/quantity" {
		t.Fatalf("the confirmation after the approval: %s at %s", code(err), path(err))
	}
	w.confirmed(fl, "shipment.2", "50", d1)
	_, err = w.ship(fl, w.merch, "shipment.3", "1", d1)
	w.want(err, fulapi.CodeSupplierDeliveryInsufficient)
}

// The commit order "confirmation first": while shipment.1's confirmation of 60 of the 100 verified holds
// commitment.1's root FOR SHARE (SupplierDeliveryGuard v2), the reviewer's approval of a requested write_off of 50
// waits; the shipment commits confirmed, then PUR approves the 50 on what is verified (PUR-INV-013's "已出运" half is
// not PUR's to see: the gap R13 reports). Nothing more ships of the line: 60 shipped and 50 disposed of exceed the 100
// verified, and PurchaseLineRef v1 shows both for STL's cap of the write-off (S5R-D42).
func TestS5RD05ConfirmationFirstMakesTheApprovalWait(t *testing.T) {
	w := newFulPurWorld(t)
	x := w.disposals()
	fl := w.heldFUL()
	d1 := w.verified("delivery.1", "100", "100")
	_, err := w.ship(fl, w.merch, "shipment.1", "60", d1)
	w.must(err)
	w.grantedOn(fl, "shipment.1", fulapi.ScopeShipment)
	_, err = x.dispose(w.author, "disp.1", "commitment.1", purapi.DispositionWriteOff, "50")
	w.must(err)

	w.hold.set("delivery")
	done := make(chan error, 1)
	go func() { _, err := fl.ConfirmShipment(w.merch, w.confirmCmd("shipment.1", 1)); done <- err }()
	w.hold.wait(t, done)
	decided := make(chan error, 1)
	go func() { _, err := x.decide(w.reviewer, "disp.1", 1, purapi.DecisionApprove, ""); decided <- err }()
	select {
	case err := <-decided:
		w.hold.open()
		t.Fatalf("the approval did not wait: %v (confirmation: %v)", err, <-done)
	case <-time.After(500 * time.Millisecond):
	}
	w.hold.open()
	if err := <-done; err != nil {
		t.Fatalf("the confirmation holding the commitment root: %v", err)
	}
	if err := <-decided; err != nil {
		t.Fatalf("the approval after FUL committed: %v", err)
	}
	if v, err := fl.GetShipment(w.merch, "shipment.1"); err != nil || v.Status != fulapi.StatusConfirmed {
		t.Fatalf("the shipment stays confirmed %+v %v", v, err)
	}
	_, err = w.ship(fl, w.merch, "shipment.2", "1", d1)
	if code(err) != fulapi.CodeSupplierDeliveryInsufficient || !strings.Contains(reasonOf(err), "60 shipped before") {
		t.Fatalf("after the approval: %s (%s)", code(err), reasonOf(err))
	}
	line := purapi.CommitmentLineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}
	lines, err := w.c.PUR.ResolvePurchaseLines(w.author, []purapi.CommitmentLineRef{line})
	w.must(err)
	if l := lines[line]; l.VerifiedQuantity != "100" || l.DisposedQuantity != "50" {
		t.Fatalf("PurchaseLineRef v1 %+v", l)
	}
}

// SupplierDeliveryGuard v2 holds the commitment root and no delivery root: one confirmation of a shipment from
// delivery.1 and delivery.2 of the same commitment line, held after its first guard answer, and the verification of
// a correction of delivery.2 (which locks delivery.2, then the commitment root) never deadlock — the verification
// waits for the confirmation, which reads delivery.2 as it was, then commits; the correction is verified after it.
// A guard that also held the delivery roots would cross the verification here.
func TestS5RD05OneShipmentOfTwoDeliveriesAndAVerificationNeverDeadlock(t *testing.T) {
	w := newFulPurWorld(t)
	fl := w.heldFUL()
	d1 := w.verified("delivery.1", "50", "50")
	d2 := w.verified("delivery.2", "40", "40")
	both := w.prepareCmd("shipment.1", "30", d1, w.release)
	both.Lines = append(both.Lines, fulapi.ShipmentLine{LineID: "S2", OrderLineID: "L1", Quantity: "20", SupplierDelivery: d2,
		QualityReleaseID: w.release})
	_, err := fl.PrepareShipment(w.merch, both)
	w.must(err)
	w.grantedOn(fl, "shipment.1", fulapi.ScopeShipment)
	_, err = w.px.correct(w.merch, "delivery.2", "commitment.1", "35", 2)
	w.must(err)

	w.hold.set("delivery")
	done := make(chan error, 1)
	go func() { _, err := fl.ConfirmShipment(w.merch, w.confirmCmd("shipment.1", 1)); done <- err }()
	w.hold.wait(t, done)
	verified := make(chan error, 1)
	go func() { _, err := w.px.verify(w.author, "delivery.2", 3, "35"); verified <- err }()
	select {
	case err := <-verified:
		w.hold.open()
		t.Fatalf("the verification did not wait for the commitment root: %v (confirmation: %v)", err, <-done)
	case <-time.After(500 * time.Millisecond):
	}
	w.hold.open()
	if err := <-done; err != nil {
		t.Fatalf("the confirmation: %v", err)
	}
	if err := <-verified; err != nil {
		t.Fatalf("the verification after FUL committed: %v", err)
	}
	v, err := fl.GetShipment(w.merch, "shipment.1")
	w.must(err)
	if v.Status != fulapi.StatusConfirmed || guardValue(v, "supplier_commitment_line:commitment.1/L1") != "verified=90/disposed=0" {
		t.Fatalf("confirmed on the totals before the correction %+v", v.Confirmation)
	}
	// 20 shipped on delivery.2's version 1 count against its verified correction of 35 (FUL-INV-003 across versions).
	_, err = w.ship(fl, w.merch, "shipment.2", "16", fulapi.DeliveryLineRef{DeliveryID: "delivery.2", LineID: "L1", Version: 2})
	w.want(err, fulapi.CodeSupplierDeliveryInsufficient)
}
