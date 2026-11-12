//go:build integration

package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// FUL on PUR's real supplier deliveries (Context Map R59, SupplierDeliveryGuard v1; FUL-INV-003;
// TD-AT14 FUL half; WBS S5-03a with S5-15), on the composed process: TRD's order world
// (order.quote.1 active for 120 PCS on line L1), PUR's purchase draft of all 120 from offer.1 as
// commitment.1, issued; QLT's release rel.real of 120 on that commitment line, granted by our QC on
// PUR's InspectionSubject v1; deliveries recorded by the merchandiser and verified by the editor
// (the purchaser of the fixture, independent of the recorder); shipments prepared and confirmed by
// bootstrap's FUL (w.c.FUL), whose port to PUR is ful.PURDeliveries. The commit-order test runs a FUL
// module on the same composed upstreams whose port to PUR holds its transaction after the guard.

type fulPurWorld struct {
	*fulWorld
	px      *purDeliver
	release string
}

func newFulPurWorld(t *testing.T) *fulPurWorld {
	t.Helper()
	w := &fulPurWorld{fulWorld: newFulWorld(t)}
	w.px = newPurDeliver(w.scenWorld, func() *pur.Module { return w.c.PUR })
	d, err := w.c.PUR.PrepareDraft(w.author, purapi.PrepareDraft{IdempotencyKey: w.key(), DraftID: "purchase.ful", OrderID: w.order.ObjectID,
		Lines: []purapi.DraftLine{purLine("120", purSource("offer.1"))}, SelectionReason: purReason})
	w.must(err)
	w.px.issued("commitment.1", d)
	ref := qltapi.SubjectRef{Kind: qltapi.SubjectCommitmentLine, ID: "commitment.1", LineID: "L1"}
	_, err = w.c.QLT.RecordInspection(w.qc, qltapi.RecordInspection{IdempotencyKey: w.key(), InspectionID: "insp.real", Subject: ref,
		InspectedQuantity: "120", ConformingQuantity: "120", NonconformingQuantity: "0", Sampling: qltapi.Sampling{Method: qltapi.SamplingFull},
		Standard: qltapi.StandardRef{SourceID: "standard"}, Inspector: qltapi.Inspector{Kind: qltapi.InspectorInternal}, ReportFileID: "report",
		InspectedAt: kernel.FormatTimestamp(time.Now().Add(-time.Hour))})
	w.must(err)
	_, err = w.c.QLT.GrantRelease(w.qc, qltapi.GrantRelease{IdempotencyKey: w.key(), ReleaseID: "rel.real", Subject: ref, Quantity: "120",
		ValidUntil: kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))})
	w.must(err)
	w.release = "rel.real"
	return w
}

// verified has the merchandiser record delivery id of claimed PCS against commitment.1 and the
// editor verify quantity of them: version 1 is the verified one.
func (w *fulPurWorld) verified(id, claimed, quantity string) fulapi.DeliveryLineRef {
	w.t.Helper()
	_, err := w.px.record(w.merch, id, "commitment.1", claimed)
	w.must(err)
	_, err = w.px.verify(w.author, id, 1, quantity)
	w.must(err)
	return fulapi.DeliveryLineRef{DeliveryID: id, LineID: "L1", Version: 1}
}

func (w *fulPurWorld) ship(m *ful.Module, ctx context.Context, id, quantity string, d fulapi.DeliveryLineRef) (fulapi.ShipmentReceipt, error) {
	return m.PrepareShipment(ctx, w.prepareCmd(id, quantity, d, w.release))
}

type heldDeliveries struct {
	ful.Deliveries
	hold *fulHold
}

func (h heldDeliveries) GuardDeliveryLine(ctx context.Context, exec bentopg.Executor, ref fulapi.DeliveryLineRef) (ful.DeliveryLine, error) {
	l, err := h.Deliveries.GuardDeliveryLine(ctx, exec, ref)
	if err == nil {
		h.hold.at("delivery")
	}
	return l, err
}

// TD-AT14, FUL half ("直发：承诺 120 件，核实交付 100 件，另有未核实上报 20 件；出运 120 件 → SUPPLIER_DELIVERY_INSUFFICIENT，
// 未核实上报不计入"), UL-INV-32 and FUL-INV-003: of a 120 PCS commitment the purchaser verified 100 of
// delivery.1; delivery.2's claim of 20 awaits verification and delivery.3's claim of 5 was rejected.
// Shipping 120 from delivery.1 is SUPPLIER_DELIVERY_INSUFFICIENT; the claims never count
// (SUPPLIER_DELIVERY_UNVERIFIED). Two shipments of 60 drawing on delivery.1 race to confirm: one
// wins, the other is SUPPLIER_DELIVERY_INSUFFICIENT; the last 40 ships; the confirmation records
// what SupplierDeliveryGuard v1 answered. Nothing ever exceeds the 100 verified.
func TestShipmentOnVerifiedDeliveriesTDAT14(t *testing.T) {
	w := newFulPurWorld(t)
	d1 := w.verified("delivery.1", "120", "100")
	_, err := w.px.record(w.merch, "delivery.2", "commitment.1", "20")
	w.must(err)
	_, err = w.px.record(w.merch, "delivery.3", "commitment.1", "5")
	w.must(err)
	_, err = w.px.reject(w.author, "delivery.3", 1)
	w.must(err)
	d2 := fulapi.DeliveryLineRef{DeliveryID: "delivery.2", LineID: "L1", Version: 1}
	d3 := fulapi.DeliveryLineRef{DeliveryID: "delivery.3", LineID: "L1", Version: 1}
	fl := w.c.FUL

	_, err = w.ship(fl, w.merch, "shipment.120", "120", d1)
	if code(err) != fulapi.CodeSupplierDeliveryInsufficient || path(err) != "/args/lines/0/quantity" {
		t.Fatalf("120 of 100 verified: %s at %s", code(err), path(err))
	}
	both := w.prepareCmd("shipment.claims", "100", d1, w.release)
	both.Lines = append(both.Lines, fulapi.ShipmentLine{LineID: "S2", OrderLineID: "L1", Quantity: "20", SupplierDelivery: d2, QualityReleaseID: w.release})
	_, err = fl.PrepareShipment(w.merch, both)
	if code(err) != fulapi.CodeSupplierDeliveryUnverified || path(err) != "/args/lines/1/supplier_delivery" {
		t.Fatalf("the pending claim: %s at %s", code(err), path(err))
	}
	_, err = w.ship(fl, w.merch, "shipment.rejected", "5", d3)
	if code(err) != fulapi.CodeSupplierDeliveryUnverified || path(err) != "/args/lines/0/supplier_delivery" {
		t.Fatalf("the rejected claim: %s at %s", code(err), path(err))
	}

	_, err = w.ship(fl, w.merch, "shipment.a", "60", d1)
	w.must(err)
	_, err = w.ship(fl, w.merch2, "shipment.b", "60", d1)
	w.must(err)
	w.grantedOn(fl, "shipment.a", fulapi.ScopeShipment)
	w.grantedOn(fl, "shipment.b", fulapi.ScopeShipment)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []struct {
		ctx context.Context
		id  string
	}{{w.merch, "shipment.a"}, {w.merch2, "shipment.b"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = fl.ConfirmShipment(c.ctx, w.confirmCmd(c.id, 1))
		}()
	}
	wg.Wait()
	winner := "shipment.a"
	if code(errs[0]) != "OK" {
		winner = "shipment.b"
	}
	if (code(errs[0]) == "OK") == (code(errs[1]) == "OK") || (code(errs[0]) != fulapi.CodeSupplierDeliveryInsufficient &&
		code(errs[1]) != fulapi.CodeSupplierDeliveryInsufficient) {
		t.Fatalf("one of two confirmations of 60 on 100 verified: %s, %s", code(errs[0]), code(errs[1]))
	}
	v, err := fl.GetShipment(w.merch, winner)
	w.must(err)
	if got := guardValue(v, "supplier_delivery:delivery.1/L1/v1"); got != "verified/verified_version=1/verified=100" {
		t.Fatalf("delivery guard evidence %q in %+v", got, v.Confirmation)
	}
	_, err = w.ship(fl, w.merch, "shipment.c", "40", d1)
	w.must(err)
	w.grantedOn(fl, "shipment.c", fulapi.ScopeShipment)
	if r, err := fl.ConfirmShipment(w.merch, w.confirmCmd("shipment.c", 1)); err != nil || r.Status != fulapi.StatusConfirmed {
		t.Fatalf("the last 40 %+v %v", r, err)
	}
	_, err = w.ship(fl, w.merch, "shipment.d", "1", d1)
	w.want(err, fulapi.CodeSupplierDeliveryInsufficient)
	if got, err := w.shipped("company.A", w.order.ObjectID); err != nil || len(got) != 1 || !strings.Contains(got[0], " 100 2") {
		t.Fatalf("ShipmentFacts v1 %q %v", got, err)
	}
}

// A delivery correction that PUR verified before the confirmation (FUL-INV-003, R59 commit order
// "correction first"): the shipment bound to the superseded version is REFERENCE_STALE at its
// confirmation and a new preparation on that version too; on the verified correction the shipment
// ships, and FUL counts what it shipped of the delivery line across versions.
func TestDeliveryCorrectionFirstRefusesTheConfirmation(t *testing.T) {
	w := newFulPurWorld(t)
	d1 := w.verified("delivery.1", "50", "50")
	fl := w.c.FUL
	_, err := w.ship(fl, w.merch, "shipment.1", "30", d1)
	w.must(err)
	_, err = w.px.correct(w.merch, "delivery.1", "commitment.1", "40", 2)
	w.must(err)
	w.grantedOn(fl, "shipment.1", fulapi.ScopeShipment)
	if r, err := fl.ConfirmShipment(w.merch, w.confirmCmd("shipment.1", 1)); err != nil || r.Status != fulapi.StatusConfirmed {
		// A correction awaiting verification changes nothing.
		t.Fatalf("pending correction %+v %v", r, err)
	}
	_, err = w.ship(fl, w.merch, "shipment.2", "10", d1)
	w.must(err)
	_, err = w.px.verify(w.author, "delivery.1", 3, "40")
	w.must(err)
	_, err = fl.ConfirmShipment(w.merch, w.confirmCmd("shipment.2", 1))
	if code(err) != fulapi.CodeReferenceStale || path(err) != "/shipment/lines/0/supplier_delivery" {
		t.Fatalf("superseded version at confirmation: %s at %s", code(err), path(err))
	}
	_, err = w.ship(fl, w.merch, "shipment.3", "5", d1)
	w.want(err, fulapi.CodeReferenceStale)
	d2 := fulapi.DeliveryLineRef{DeliveryID: "delivery.1", LineID: "L1", Version: 2}
	_, err = w.ship(fl, w.merch, "shipment.4", "11", d2)
	w.want(err, fulapi.CodeSupplierDeliveryInsufficient) // 30 shipped on version 1 + 11 > 40
	_, err = w.ship(fl, w.merch, "shipment.4", "10", d2)
	w.must(err)
	w.grantedOn(fl, "shipment.4", fulapi.ScopeShipment)
	if r, err := fl.ConfirmShipment(w.merch, w.confirmCmd("shipment.4", 1)); err != nil || r.Status != fulapi.StatusConfirmed {
		t.Fatalf("on the verified correction %+v %v", r, err)
	}
}

// SupplierDeliveryGuard v1 in the other commit order (R59, ADR-BR-004 option B, ADR-BR-011): while a
// confirmation holds the delivery root FOR SHARE, the verification of a correction waits until it
// commits; the shipment stays confirmed on the version it bound, the next shipment on that version
// is REFERENCE_STALE, and what shipped counts against the correction.
func TestDeliveryCorrectionWaitsForTheConfirmation(t *testing.T) {
	w := newFulPurWorld(t)
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	fl, err := ful.New(db, ful.Upstream{Orders: ful.TRDOrders(w.c.TRD), Releases: ful.QLTReleases(w.c.QLT),
		Deliveries: heldDeliveries{Deliveries: ful.PURDeliveries(w.c.PUR), hold: w.hold}, Screening: ful.PTYScreening(), Sources: w.c.EVD,
		Compliance: ful.CNXCompliance(), Payments: ful.STLPayments()})
	w.must(err)
	d1 := w.verified("delivery.1", "50", "50")
	_, err = w.ship(fl, w.merch, "shipment.1", "30", d1)
	w.must(err)
	_, err = w.ship(fl, w.merch, "shipment.2", "10", d1)
	w.must(err)
	w.grantedOn(fl, "shipment.1", fulapi.ScopeShipment)
	w.grantedOn(fl, "shipment.2", fulapi.ScopeShipment)
	_, err = w.px.correct(w.merch, "delivery.1", "commitment.1", "45", 2)
	w.must(err)
	w.hold.set("delivery")
	done := make(chan error, 1)
	go func() { _, err := fl.ConfirmShipment(w.merch, w.confirmCmd("shipment.1", 1)); done <- err }()
	w.hold.wait(t, done)
	verified := make(chan error, 1)
	go func() { _, err := w.px.verify(w.author, "delivery.1", 3, "45"); verified <- err }()
	select {
	case err := <-verified:
		w.hold.open()
		t.Fatalf("the verification did not wait: %v (confirmation: %v)", err, <-done)
	case <-time.After(500 * time.Millisecond):
	}
	w.hold.open()
	if err := <-done; err != nil {
		t.Fatalf("the confirmation holding the guard: %v", err)
	}
	if err := <-verified; err != nil {
		t.Fatalf("the verification after FUL committed: %v", err)
	}
	if v, err := fl.GetShipment(w.merch, "shipment.1"); err != nil || v.Status != fulapi.StatusConfirmed || v.Lines[0].SupplierDelivery.Version != 1 {
		t.Fatalf("the confirmed shipment stays %+v %v", v, err)
	}
	_, err = fl.ConfirmShipment(w.merch, w.confirmCmd("shipment.2", 1))
	w.want(err, fulapi.CodeReferenceStale)
	d2 := fulapi.DeliveryLineRef{DeliveryID: "delivery.1", LineID: "L1", Version: 2}
	_, err = w.ship(fl, w.merch, "shipment.3", "16", d2)
	w.want(err, fulapi.CodeSupplierDeliveryInsufficient) // 30 shipped + 16 > 45
	_, err = w.ship(fl, w.merch, "shipment.3", "15", d2)
	w.must(err)
}

// S5-03a with S5-15 over HTTP with OIDC access tokens, on the composed process: the merchandiser
// records the supplier's claim, the editor verifies it, the merchandiser prepares and confirms a
// shipment on the verified delivery line, everything in its wire form; the shipment reads back with
// the delivery guard's answer in its confirmation.
func TestShipmentOnDeliveryVerifiedOverHTTP(t *testing.T) {
	w := newFulPurWorld(t)
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	merch := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "merch.1", "human")}
	ed := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "editor", "human"), n: 300}

	r := merch.command("pur", purapi.Contract, purapi.CommandRecordSupplierDelivery, map[string]any{"delivery_id": "delivery.http",
		"expected_version": 0, "commitment_id": "commitment.1", "lines": []any{map[string]any{"order_line_id": "L1", "quantity": "50"}},
		"handover_place": "CNNGB", "handed_over_at": kernel.FormatTimestamp(time.Now().Add(-time.Minute)),
		"evidence": []any{map[string]any{"source_id": "evidence", "locator": "合成货代收货单"}}})
	if r["status"] != "recorded" {
		t.Fatalf("record %v", r)
	}
	v := ed.command("pur", purapi.Contract, purapi.CommandVerifySupplierDelivery, map[string]any{"delivery_id": "delivery.http",
		"expected_version": 1, "decision": "verify", "lines": []any{map[string]any{"order_line_id": "L1", "verified_quantity": "50"}}})
	if v["status"] != "verified" {
		t.Fatalf("verify %v", v)
	}
	p := merch.command("ful", fulapi.Contract, fulapi.CommandPrepareShipment, map[string]any{"shipment_id": "shipment.http",
		"order_id": w.order.ObjectID, "route": fulapi.RouteSupplierDirect, "lines": []any{map[string]any{"line_id": "S1", "order_line_id": "L1",
			"quantity": "50", "supplier_delivery": map[string]any{"delivery_id": "delivery.http", "line_id": "L1", "version": 1},
			"quality_release_id": w.release}}})
	if p["status"] != fulapi.StatusPrepared || p["row_version"] != float64(1) {
		t.Fatalf("prepare %v", p)
	}
	doc := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "doc.1", "human"), n: 500}
	if e := doc.command("ful", fulapi.Contract, fulapi.CommandEvaluateRelease, map[string]any{"shipment_id": "shipment.http",
		"scope": fulapi.ScopeShipment}); e["status"] != fulapi.ReleaseGranted || e["row_version"] != float64(1) {
		t.Fatalf("evaluate %v", e)
	}
	c := merch.command("ful", fulapi.Contract, fulapi.CommandConfirmShipment, map[string]any{"shipment_id": "shipment.http", "expected_version": 1,
		"evidence_ids": []any{"evidence"}})
	if c["status"] != fulapi.StatusConfirmed || c["row_version"] != float64(2) {
		t.Fatalf("confirm %v", c)
	}
	status, s := merch.do("GET", "/api/brant/v1/ful/shipments/shipment.http", nil)
	guards, _ := s["confirmation"].(map[string]any)["guards"].([]any)
	found := ""
	for _, g := range guards {
		if g.(map[string]any)["name"] == "supplier_delivery:delivery.http/L1/v1" {
			found, _ = g.(map[string]any)["value"].(string)
		}
	}
	if status != http.StatusOK || found != "verified/verified_version=1/verified=50" {
		t.Fatalf("shipment %d %v", status, s)
	}
	status, out := merch.try("ful", fulapi.Contract, fulapi.CommandPrepareShipment, map[string]any{"shipment_id": "shipment.more",
		"order_id": w.order.ObjectID, "route": fulapi.RouteSupplierDirect, "lines": []any{map[string]any{"line_id": "S1", "order_line_id": "L1",
			"quantity": "1", "supplier_delivery": map[string]any{"delivery_id": "delivery.http", "line_id": "L1", "version": 1},
			"quality_release_id": w.release}}})
	if status != http.StatusConflict || out["code"] != fulapi.CodeSupplierDeliveryInsufficient {
		t.Fatalf("beyond the verified delivery %d %v", status, out)
	}
}
