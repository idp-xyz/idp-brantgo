//go:build integration

package bootstrap_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	stkmod "go.idp.xyz/idp-brant/internal/contexts/stk"
	stkapi "go.idp.xyz/idp-brant/internal/contexts/stk/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity/oidc"
	"go.idp.xyz/idp-brant/internal/kernel"
	"go.idp.xyz/idp-brant/internal/transport/httpapi"
)

// S6-01 on PostgreSQL 16: the company switch defaults to off (STK-INV-004), FIFO cost layers round
// once HALF_UP (STK-INV-003), a direct-ship delivery is not a warehouse receipt (STK-INV-002), and
// two concurrent StockAvailability v1 issues cannot drive stock negative (STK-INV-001, R29).

func TestS6StockDefaultsOffThenFIFO(t *testing.T) {
	w := newWorld(t)
	admin := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "stk.admin"},
		Permissions: []string{stkapi.CommandConfigure, stkapi.CommandReceive, stkapi.CommandIssue, stkapi.CommandAdjust,
			stkapi.CommandPlace, stkapi.CommandRelease, stkapi.PermissionRead, stkapi.PermissionAudit},
		DataScope: "company"})
	stock := w.c.STK
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)

	_, err = stock.ReceiveStock(admin, receive("mv.off", "sku.off", "10", "100", "1", "2026-01-01T00:00:00.000000Z"))
	w.want(err, stkapi.CodeDependencyNotSupported)
	_, err = stock.IssueStock(admin, stkapi.IssueStock{IdempotencyKey: w.key(), MovementID: "mv.off.issue", SkuID: "sku.off",
		LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "1", UnitID: "PCS", UnitRevision: 1})
	w.want(err, stkapi.CodeDependencyNotSupported)
	_, err = stock.AdjustStock(admin, stkapi.AdjustStock{IdempotencyKey: w.key(), MovementID: "mv.off.adj", Direction: stkapi.AdjustOut,
		SkuID: "sku.off", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "1",
		UnitID: "PCS", UnitRevision: 1, Reason: "module off"})
	w.want(err, stkapi.CodeDependencyNotSupported)
	_, err = stock.PlaceReservation(admin, stkapi.PlaceReservation{IdempotencyKey: w.key(), ReservationID: "res.off", SkuID: "sku.off",
		LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "1", UnitID: "PCS", UnitRevision: 1})
	w.want(err, stkapi.CodeDependencyNotSupported)
	_, err = stock.ReleaseReservation(admin, stkapi.ReleaseReservation{IdempotencyKey: w.key(), ReservationID: "res.off", ExpectedVersion: 1})
	w.want(err, stkapi.CodeDependencyNotSupported)
	// A cited commitment is not looked up while the switch is off (STK-INV-004).
	citedOff := receive("mv.off.pur", "sku.off", "1", "100", "1", "")
	citedOff.SourceKind, citedOff.SourceID, citedOff.SourceLineID = stkapi.SourceCommitment, "c.missing", "L1"
	_, err = stock.ReceiveStock(admin, citedOff)
	w.want(err, stkapi.CodeDependencyNotSupported)
	off := callIssue(t, db, w.scope("company.A"), stkapi.StockIssue{ShipmentID: "ship.off", ShipmentLineID: "L1", SkuID: "sku.off",
		LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "1", UnitID: "PCS", UnitRevision: 1,
		ActorKind: "human", ActorID: "stk.admin", Epoch: 1, At: time.Now()})
	if off.Status != stkapi.AvailabilityDisabled || off.Code != stkapi.CodeDependencyNotSupported {
		t.Fatalf("disabled availability %+v", off)
	}
	readOff, err := stock.ResolveAvailability(admin, stkapi.AvailabilityQuery{SkuID: "sku.off", LocationID: "loc.1", BatchID: "batch.1",
		CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "1"})
	w.must(err)
	if readOff.Enabled || readOff.Status != stkapi.AvailabilityDisabled {
		t.Fatalf("disabled read %+v", readOff)
	}

	is := newIssuer(t)
	verifier, err := oidc.New(oidc.Config{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	c := w.c
	handler, err := httpapi.New(httpapi.Config{EVD: c.EVD, PTY: c.PTY, CAT: c.CAT, INQ: c.INQ, POL: c.POL, REF: c.REF, SUP: c.SUP,
		CST: c.CST, TRD: c.TRD, PUR: c.PUR, QLT: c.QLT, STL: c.STL, CNX: c.CNX, FUL: c.FUL, TSK: c.TSK, AI: c.AI, STK: c.STK,
		Verifier: oidc.HeaderVerifier{Tokens: verifier}, Clock: bento.SystemClock{}})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	base := srv.URL
	httpAdmin := &client{t: t, base: base, token: is.token(w.tenant, "company.A", "stk.admin", "human")}
	enabled := httpAdmin.command("stk", stkapi.Contract, stkapi.CommandConfigure, map[string]any{"enabled": true, "expected_version": 0})
	if enabled["status"] != stkapi.StatusEnabled || enabled["row_version"] != float64(1) {
		t.Fatalf("enable %v", enabled)
	}
	// The same citation, once the switch is on, reaches PUR and does not invent a commitment.
	_, err = stock.ReceiveStock(admin, citedOff)
	w.want(err, kernel.CodeReferenceInvalid)
	agent := &client{t: t, base: base, token: is.token(w.tenant, "company.A", "stk.ai", "ai")}
	if status, out := agent.try("stk", stkapi.Contract, stkapi.CommandReceive, map[string]any{"movement_id": "mv.ai", "sku_id": "sku.ai",
		"location_id": "loc.1", "batch_id": "batch.1", "custody_kind": stkapi.CustodyOwnWarehouse, "quantity": "1", "unit_id": "PCS",
		"unit_revision": 1, "currency": "USD", "minor_units": 2, "unit_cost_minor": "1", "price_basis_qty": "1"}); status != 403 || out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI receive %d %v", status, out)
	}

	_, err = stock.ReceiveStock(admin, receive("mv.old", "sku.fifo", "40", "1000", "1", "2026-01-01T00:00:00.000000Z"))
	w.must(err)
	_, err = stock.ReceiveStock(admin, receive("mv.new", "sku.fifo", "60", "2500", "1", "2026-02-01T00:00:00.000000Z"))
	w.must(err)
	_, err = stock.IssueStock(admin, stkapi.IssueStock{IdempotencyKey: w.key(), MovementID: "mv.issue", SkuID: "sku.fifo", LocationID: "loc.1",
		BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "50", UnitID: "PCS", UnitRevision: 1})
	w.must(err)
	if got := allocationTotal(t, db, w.scope("company.A"), "mv.issue"); got != 65000 {
		t.Fatalf("FIFO issue cost %d, want 65000", got)
	}
	bal, err := stock.GetBalance(admin, stkapi.BalanceKey{SkuID: "sku.fifo", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse})
	w.must(err)
	if bal.OnHand != "50" || bal.Available != "50" {
		t.Fatalf("balance %+v", bal)
	}

	// The same split through StockAvailability v1, and CostLayerRef v1 reads it back.
	_, err = stock.ReceiveStock(admin, receive("mv.sql.old", "sku.sql", "40", "1000", "1", "2026-01-01T00:00:00.000000Z"))
	w.must(err)
	_, err = stock.ReceiveStock(admin, receive("mv.sql.new", "sku.sql", "60", "2500", "1", "2026-02-01T00:00:00.000000Z"))
	w.must(err)
	issued := callIssue(t, db, w.scope("company.A"), shipment("ship.sql", "L1", "sku.sql", "", "50"))
	if !issued.OK() || !issued.CostKnown || issued.TotalCostMinor != 65000 || issued.Currency != "USD" || issued.Replayed {
		t.Fatalf("sql fifo %+v", issued)
	}
	again := callIssue(t, db, w.scope("company.A"), shipment("ship.sql", "L1", "sku.sql", "", "50"))
	if !again.Replayed || again.TotalCostMinor != 65000 {
		t.Fatalf("replay %+v", again)
	}
	bal, err = stock.GetBalance(admin, stkapi.BalanceKey{SkuID: "sku.sql", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse})
	w.must(err)
	if bal.OnHand != "50" {
		t.Fatalf("replay changed on hand to %s", bal.OnHand)
	}
	costs, err := stock.ResolveShipmentStockCost(admin, []string{"ship.sql"})
	w.must(err)
	if len(costs) != 1 || costs[0].TotalCostMinor != 65000 || costs[0].ShipmentLineID != "L1" || len(costs[0].Allocations) != 2 {
		t.Fatalf("cost layer ref %+v", costs)
	}

	// 10 minor × 2 / 3 rounds HALF_UP to 7. The domain test locks the same integer.
	_, err = stock.ReceiveStock(admin, receive("mv.half", "sku.half", "3", "10", "3", "2026-01-01T00:00:00.000000Z"))
	w.must(err)
	half := callIssue(t, db, w.scope("company.A"), shipment("ship.half", "L1", "sku.half", "", "2"))
	if half.TotalCostMinor != 7 {
		t.Fatalf("half-up sql %d, want 7", half.TotalCostMinor)
	}
}

func TestS6StockReservationAndConcurrency(t *testing.T) {
	w := newWorld(t)
	admin := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "stk.admin"},
		Permissions: []string{stkapi.CommandConfigure, stkapi.CommandReceive, stkapi.CommandIssue, stkapi.CommandPlace, stkapi.CommandRelease, stkapi.PermissionRead},
		DataScope:   "company"})
	w.must(enable(w, admin))
	stock := w.c.STK
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	_, err = stock.ReceiveStock(admin, receive("mv.hold", "sku.hold", "100", "100", "1", "2026-01-01T00:00:00.000000Z"))
	w.must(err)
	_, err = stock.PlaceReservation(admin, stkapi.PlaceReservation{IdempotencyKey: w.key(), ReservationID: "res.1", SkuID: "sku.hold",
		LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "60", UnitID: "PCS", UnitRevision: 1,
		OrderID: "order.1", OrderLineID: "L1"})
	w.must(err)
	// An order hold is consumed only by StockAvailability v1. stock.issue must not take it.
	_, err = stock.IssueStock(admin, stkapi.IssueStock{IdempotencyKey: w.key(), MovementID: "mv.orderhold", SkuID: "sku.hold", LocationID: "loc.1",
		BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "60", UnitID: "PCS", UnitRevision: 1, ReservationID: "res.1"})
	w.want(err, stkapi.CodeStockReservationConflict)
	_, err = stock.IssueStock(admin, stkapi.IssueStock{IdempotencyKey: w.key(), MovementID: "mv.unreserved", SkuID: "sku.hold", LocationID: "loc.1",
		BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "50", UnitID: "PCS", UnitRevision: 1})
	w.want(err, stkapi.CodeStockInsufficient)
	bal, err := stock.GetBalance(admin, stkapi.BalanceKey{SkuID: "sku.hold", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse})
	w.must(err)
	if bal.OnHand != "100" || bal.Reserved != "60" {
		t.Fatalf("order hold was consumed by stock.issue: %+v", bal)
	}
	held := callIssue(t, db, w.scope("company.A"), shipment("ship.hold", "L1", "sku.hold", "res.1", "60"))
	if !held.OK() || held.TotalCostMinor != 6000 {
		t.Fatalf("reserved issue %+v", held)
	}
	bal, err = stock.GetBalance(admin, stkapi.BalanceKey{SkuID: "sku.hold", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse})
	w.must(err)
	if bal.OnHand != "40" || bal.Reserved != "0" || bal.Available != "40" {
		t.Fatalf("after reserved issue %+v", bal)
	}
	view, err := stock.GetReservation(admin, "res.1")
	w.must(err)
	if view.Status != "consumed" || view.Remaining != "0" {
		t.Fatalf("reservation %+v", view)
	}
	_, err = stock.ReleaseReservation(admin, stkapi.ReleaseReservation{IdempotencyKey: w.key(), ReservationID: "res.1", ExpectedVersion: view.RowVersion})
	w.want(err, stkapi.CodeStockReservationConflict)

	_, err = stock.ReceiveStock(admin, receive("mv.rel", "sku.rel", "100", "100", "1", "2026-01-01T00:00:00.000000Z"))
	w.must(err)
	rel, err := stock.PlaceReservation(admin, stkapi.PlaceReservation{IdempotencyKey: w.key(), ReservationID: "res.rel", SkuID: "sku.rel",
		LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "60", UnitID: "PCS", UnitRevision: 1})
	w.must(err)
	released, err := stock.ReleaseReservation(admin, stkapi.ReleaseReservation{IdempotencyKey: w.key(), ReservationID: "res.rel", ExpectedVersion: rel.RowVersion})
	w.must(err)
	if released.Status != stkapi.StatusReleased {
		t.Fatalf("release %+v", released)
	}
	bal, err = stock.GetBalance(admin, stkapi.BalanceKey{SkuID: "sku.rel", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse})
	w.must(err)
	if bal.Reserved != "0" || bal.Available != "100" {
		t.Fatalf("released balance %+v", bal)
	}
	_, err = stock.IssueStock(admin, stkapi.IssueStock{IdempotencyKey: w.key(), MovementID: "mv.released", SkuID: "sku.rel", LocationID: "loc.1",
		BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "10", UnitID: "PCS", UnitRevision: 1, ReservationID: "res.rel"})
	w.want(err, stkapi.CodeStockReservationConflict)

	// An internal hold (no order) is consumed by stock.issue: on hand and reserved both fall.
	_, err = stock.ReceiveStock(admin, receive("mv.int", "sku.int", "100", "100", "1", "2026-01-01T00:00:00.000000Z"))
	w.must(err)
	_, err = stock.PlaceReservation(admin, stkapi.PlaceReservation{IdempotencyKey: w.key(), ReservationID: "res.int", SkuID: "sku.int",
		LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "30", UnitID: "PCS", UnitRevision: 1})
	w.must(err)
	_, err = stock.IssueStock(admin, stkapi.IssueStock{IdempotencyKey: w.key(), MovementID: "mv.int.short", SkuID: "sku.int", LocationID: "loc.1",
		BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "40", UnitID: "PCS", UnitRevision: 1, ReservationID: "res.int"})
	w.want(err, stkapi.CodeStockReservationConflict)
	_, err = stock.IssueStock(admin, stkapi.IssueStock{IdempotencyKey: w.key(), MovementID: "mv.int.out", SkuID: "sku.int", LocationID: "loc.1",
		BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "30", UnitID: "PCS", UnitRevision: 1, ReservationID: "res.int"})
	w.must(err)
	bal, err = stock.GetBalance(admin, stkapi.BalanceKey{SkuID: "sku.int", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse})
	w.must(err)
	if bal.OnHand != "70" || bal.Reserved != "0" || bal.Available != "70" {
		t.Fatalf("internal hold %+v", bal)
	}
	view, err = stock.GetReservation(admin, "res.int")
	w.must(err)
	if view.Status != "consumed" || view.Remaining != "0" {
		t.Fatalf("internal reservation %+v", view)
	}

	// Two concurrent issues of 60 against 100: one succeeds, on hand is 40, never negative.
	_, err = stock.ReceiveStock(admin, receive("mv.race", "sku.race", "100", "100", "1", "2026-01-01T00:00:00.000000Z"))
	w.must(err)
	var wg sync.WaitGroup
	got := make([]stkapi.StockAvailabilityVerdict, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got[i], errs[i] = tryIssue(db, w.scope("company.A"), shipment("ship.race", "L"+string(rune('1'+i)), "sku.race", "", "60"))
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		w.must(err)
	}
	var okN, shortN int
	for _, v := range got {
		switch v.Status {
		case stkapi.AvailabilityOK:
			okN++
		case stkapi.AvailabilityInsufficient:
			shortN++
		default:
			t.Fatalf("race status %+v", got)
		}
	}
	if okN != 1 || shortN != 1 {
		t.Fatalf("race %+v", got)
	}
	bal, err = stock.GetBalance(admin, stkapi.BalanceKey{SkuID: "sku.race", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse})
	w.must(err)
	if bal.OnHand != "40" || bal.Reserved != "0" {
		t.Fatalf("race balance %+v", bal)
	}
}

func TestS6StockDirectShipAndIsolation(t *testing.T) {
	w := newWorld(t)
	admin := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "stk.admin"},
		Permissions: []string{stkapi.CommandConfigure, stkapi.CommandReceive, stkapi.CommandAdjust, stkapi.PermissionRead}, DataScope: "company"})
	w.must(enable(w, admin))
	stock := w.c.STK
	bad := receive("mv.ship", "sku.direct", "1", "100", "1", "")
	bad.SourceKind, bad.SourceID, bad.SourceLineID = stkapi.SourceDelivery, "delivery.1", "L1"
	_, err := stock.ReceiveStock(admin, bad)
	w.want(err, kernel.CodeInvalidKnownValue)
	if _, err = stock.GetBalance(admin, stkapi.BalanceKey{SkuID: "sku.direct", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse}); code(err) != kernel.CodeNotFound {
		t.Fatalf("direct ship created a balance: %v", err)
	}
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	mod, err := stkmod.New(db, fakeReceipts{req: purapi.ReceiptRequest{CommitmentID: "c.1", OrderLineID: "L1", Status: "issued", SkuID: "sku.fact",
		UnitID: "PCS", UnitRevision: 1, OrderedQuantity: "10", Currency: "USD", MinorUnits: 2, DirectShip: true}})
	w.must(err)
	cited := receive("mv.fact", "sku.fact", "1", "100", "1", "")
	cited.SourceKind, cited.SourceID, cited.SourceLineID = stkapi.SourceCommitment, "c.1", "L1"
	_, err = mod.ReceiveStock(admin, cited)
	w.want(err, kernel.CodeInvalidKnownValue)
	hold := cited
	hold.MovementID, hold.SourceKind = "mv.holdfact", stkapi.SourceHoldForReuse
	_, err = mod.ReceiveStock(admin, hold)
	w.want(err, kernel.CodeInvalidState)

	_, err = stock.ReceiveStock(admin, receive("mv.iso", "sku.iso", "5", "100", "1", "2026-01-01T00:00:00.000000Z"))
	w.must(err)
	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "stk.other"},
		Permissions: []string{stkapi.CommandConfigure, stkapi.PermissionRead}, DataScope: "company"})
	w.must(enable(w, other))
	if _, err = stock.GetBalance(other, stkapi.BalanceKey{SkuID: "sku.iso", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse}); code(err) != kernel.CodeNotFound {
		t.Fatalf("company B sees company A: %v", err)
	}
	isolated := callIssue(t, db, w.scope("company.B"), shipment("ship.iso", "L1", "sku.iso", "", "5"))
	if isolated.Status != stkapi.AvailabilityInsufficient {
		t.Fatalf("company B issue %+v", isolated)
	}
	bal, err := stock.GetBalance(admin, stkapi.BalanceKey{SkuID: "sku.iso", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse})
	w.must(err)
	if bal.OnHand != "5" {
		t.Fatalf("company A on hand changed to %s", bal.OnHand)
	}
	foreignScope, err := kernel.NewScope(pgtest.UniqueID(t, "tn."), "company.A")
	w.must(err)
	foreign := w.provision(bootstrap.Grant{Scope: foreignScope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "stk.foreign"},
		Permissions: []string{stkapi.PermissionRead}, DataScope: "company"})
	if _, err = stock.GetBalance(foreign, stkapi.BalanceKey{SkuID: "sku.iso", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse}); code(err) != kernel.CodeNotFound {
		t.Fatalf("other tenant sees the balance: %v", err)
	}

	_, err = stock.AdjustStock(admin, stkapi.AdjustStock{IdempotencyKey: w.key(), MovementID: "mv.down", Direction: stkapi.AdjustOut, SkuID: "sku.iso",
		LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "9", UnitID: "PCS", UnitRevision: 1, Reason: "count short"})
	w.want(err, stkapi.CodeStockInsufficient)
	_, err = stock.AdjustStock(admin, stkapi.AdjustStock{IdempotencyKey: w.key(), MovementID: "mv.up", Direction: stkapi.AdjustIn, SkuID: "sku.iso",
		LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: "1", UnitID: "PCS", UnitRevision: 1,
		Currency: "USD", MinorUnits: 2, UnitCostMinor: "100", PriceBasisQty: "1", Reason: "count long"})
	w.must(err)
	bal, err = stock.GetBalance(admin, stkapi.BalanceKey{SkuID: "sku.iso", LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse})
	w.must(err)
	if bal.OnHand != "6" {
		t.Fatalf("adjust %+v", bal)
	}
}

type fakeReceipts struct{ req purapi.ReceiptRequest }

func (f fakeReceipts) ResolveReceiptRequest(context.Context, string, string) (purapi.ReceiptRequest, error) {
	return f.req, nil
}

func enable(w *world, ctx context.Context) error {
	_, err := w.c.STK.ConfigureModule(ctx, stkapi.ConfigureModule{IdempotencyKey: w.key(), Enabled: true})
	return err
}

func receive(id, sku, qty, cost, basis, at string) stkapi.ReceiveStock {
	return stkapi.ReceiveStock{IdempotencyKey: id, MovementID: id, SkuID: sku, LocationID: "loc.1", BatchID: "batch.1",
		CustodyKind: stkapi.CustodyOwnWarehouse, Quantity: qty, UnitID: "PCS", UnitRevision: 1, Currency: "USD", MinorUnits: 2,
		UnitCostMinor: cost, PriceBasisQty: basis, ReceivedAt: at}
}

func shipment(id, line, sku, reservation, qty string) stkapi.StockIssue {
	return stkapi.StockIssue{ShipmentID: id, ShipmentLineID: line, SkuID: sku, LocationID: "loc.1", BatchID: "batch.1",
		CustodyKind: stkapi.CustodyOwnWarehouse, ReservationID: reservation, Quantity: qty, UnitID: "PCS", UnitRevision: 1,
		ActorKind: "human", ActorID: "stk.admin", Epoch: 1, At: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
}

func callIssue(t *testing.T, db *bentopg.DB, scope kernel.Scope, in stkapi.StockIssue) stkapi.StockAvailabilityVerdict {
	t.Helper()
	v, err := tryIssue(db, scope, in)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func tryIssue(db *bentopg.DB, scope kernel.Scope, in stkapi.StockIssue) (stkapi.StockAvailabilityVerdict, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var v stkapi.StockAvailabilityVerdict
	err := bento.ScopeTransactor{DB: db, Scope: scope}.WithinTransaction(ctx, func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		v, err = stkapi.ScanStockAvailability(exec.QueryRow(ctx, stkapi.StockAvailabilitySQL, stkapi.StockAvailabilityArgs(in)...).Scan)
		return err
	})
	return v, err
}

func allocationTotal(t *testing.T, db *bentopg.DB, scope kernel.Scope, movementID string) int64 {
	t.Helper()
	var total int64
	err := bento.ScopeTransactor{DB: db, Scope: scope}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return exec.QueryRow(ctx, `SELECT coalesce(sum(amount_minor), 0) FROM brant_stk.cost_allocation
			WHERE tenant_id = current_setting('app.tenant_id') AND company_id = current_setting('app.company_id') AND issue_movement_id = $1`, movementID).Scan(&total)
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}
