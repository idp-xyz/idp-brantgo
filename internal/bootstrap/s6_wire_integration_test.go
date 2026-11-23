//go:build integration

package bootstrap_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	clmapi "go.idp.xyz/idp-brant/internal/contexts/clm/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	stkapi "go.idp.xyz/idp-brant/internal/contexts/stk/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S6 wire on PostgreSQL 16 (S6-D02, S6-D03; Context Map R29, R35, R41; FUL-INV-001, STL-INV-011).
// The stock route is the order world's shipment with STK wired into that FUL. Direct-ship stays on
// the same FUL and does not call StockAvailability. STL reads CostLayerRef and CreditRequest from
// the composed process.

func (w *fulWorld) useStock() {
	w.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	w.must(err)
	w.ful, err = ful.New(db, ful.Upstream{Orders: heldOrders{Orders: ful.TRDOrders(w.c.TRD), hold: w.hold},
		Releases: heldReleases{Releases: ful.QLTReleases(w.qlt), hold: w.hold}, Deliveries: w.deliveries, Screening: ful.PTYScreening(),
		Sources: w.c.EVD, Compliance: ful.CNXCompliance(), Payments: heldPayments{Payments: ful.STLPayments(), hold: w.hold},
		Documents: ful.DocumentPorts(w.c.POL, w.c.PTY, w.c.TRD).WithCertificates(w.c.CNX), Costs: w.c.CST, Stock: w.c.STK})
	w.must(err)
}

func (w *fulWorld) stockAdmin() context.Context {
	w.t.Helper()
	return w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "stk.wire"},
		Permissions: []string{stkapi.CommandConfigure, stkapi.CommandReceive, stkapi.CommandPlace, stkapi.PermissionRead},
		DataScope:   "company"})
}

func (w *fulWorld) stockReceive(admin context.Context, id, qty, cost, at string) {
	w.t.Helper()
	cmd := receive(id, w.line.Sku.SkuID, qty, cost, "1", at)
	cmd.UnitID, cmd.UnitRevision = w.line.UnitRef.ID, w.line.UnitRef.Revision
	_, err := w.c.STK.ReceiveStock(admin, cmd)
	w.must(err)
}

func (w *fulWorld) stockBalance(admin context.Context) stkapi.BalanceView {
	w.t.Helper()
	bal, err := w.c.STK.GetBalance(admin, stkapi.BalanceKey{SkuID: w.line.Sku.SkuID, LocationID: "loc.1", BatchID: "batch.1",
		CustodyKind: stkapi.CustodyOwnWarehouse})
	w.must(err)
	return bal
}

func (w *fulWorld) prepareStockLine(id, qty, reservation string) (fulapi.ShipmentReceipt, error) {
	return w.ful.PrepareShipment(w.merch, fulapi.PrepareShipment{IdempotencyKey: w.key(), ShipmentID: id, OrderID: w.order.ObjectID,
		Route: fulapi.RouteStock, Lines: []fulapi.ShipmentLine{{LineID: "S1", OrderLineID: "L1", Quantity: qty,
			Stock: &fulapi.StockLine{LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse,
				ReservationID: reservation}}}})
}

func fieldPaths(err error) []string {
	rej, ok := kernel.AsRejection(err)
	if !ok {
		return nil
	}
	out := make([]string, len(rej.Fields))
	for i, f := range rej.Fields {
		out[i] = f.Path
	}
	return out
}

func hasPath(err error, want string) bool {
	for _, p := range fieldPaths(err) {
		if p == want {
			return true
		}
	}
	return false
}

// StockAvailability v1 runs only on shipment.confirm of route stock, inside that FUL transaction,
// after the commercial release and not during evaluate. FIFO of 40 at 10.00 and 60 at 25.00,
// issuing 50, costs 650.00 once. The command replay does not issue again. A named reservation is
// judged on on-hand, and a direct shipment leaves the balance where confirm put it.
func TestStockRouteConfirmDeductsOnce(t *testing.T) {
	w := newFulWorld(t)
	w.useStock()
	admin := w.stockAdmin()
	_, err := w.c.STK.ConfigureModule(admin, stkapi.ConfigureModule{IdempotencyKey: w.key(), Enabled: true})
	w.must(err)
	w.stockReceive(admin, "mv.old", "40", "1000", "2026-01-01T00:00:00.000000Z")
	w.stockReceive(admin, "mv.new", "60", "2500", "2026-02-01T00:00:00.000000Z")
	if bal := w.stockBalance(admin); !sameQty(t, bal.OnHand, "100") {
		t.Fatalf("received %+v", bal)
	}

	prepared, err := w.prepareStockLine("shipment.stock", "50", "")
	w.must(err)
	if bal := w.stockBalance(admin); !sameQty(t, bal.OnHand, "100") {
		t.Fatalf("prepare wrote stock %+v", bal)
	}
	w.granted("shipment.stock")
	if bal := w.stockBalance(admin); !sameQty(t, bal.OnHand, "100") {
		t.Fatalf("evaluate wrote stock %+v", bal)
	}
	confirm := w.confirmCmd("shipment.stock", prepared.RowVersion)
	confirmed, err := w.ful.ConfirmShipment(w.merch, confirm)
	w.must(err)
	if confirmed.Status != fulapi.StatusConfirmed {
		t.Fatalf("confirm %+v", confirmed)
	}
	if bal := w.stockBalance(admin); !sameQty(t, bal.OnHand, "50") || !sameQty(t, bal.Available, "50") {
		t.Fatalf("after confirm %+v", bal)
	}
	evidence := guardValue(w.shipment("shipment.stock"), "stk.stock_availability.v1:S1")
	if evidence == "" || !strings.Contains(evidence, "65000") {
		t.Fatalf("stock evidence %q", evidence)
	}
	again, err := w.ful.ConfirmShipment(w.merch, confirm)
	w.must(err)
	if again.RowVersion != confirmed.RowVersion {
		t.Fatalf("replay %+v, first %+v", again, confirmed)
	}
	if bal := w.stockBalance(admin); !sameQty(t, bal.OnHand, "50") {
		t.Fatalf("replay deducted %+v", bal)
	}

	v, err := w.c.STL.GetOrderResult(w.treasury, w.order.ObjectID)
	w.must(err)
	if v.Live.Confirmed.UnsoldPurchaseCostMinor != "65000" || !hasStockCost(v, "650.00 USD") {
		t.Fatalf("stock cost %+v", v.Live.Confirmed)
	}

	_, err = w.c.STK.PlaceReservation(admin, stkapi.PlaceReservation{IdempotencyKey: w.key(), ReservationID: "res.wire",
		SkuID: w.line.Sku.SkuID, LocationID: "loc.1", BatchID: "batch.1", CustodyKind: stkapi.CustodyOwnWarehouse,
		Quantity: "30", UnitID: w.line.UnitRef.ID, UnitRevision: w.line.UnitRef.Revision, OrderID: w.order.ObjectID, OrderLineID: "L1"})
	w.must(err)
	_, err = w.prepareStockLine("shipment.short", "30", "")
	wantRefused(t, "unreserved stock under a hold", err, fulapi.CodeStockInsufficient, "/args/lines/0/quantity")
	held, err := w.prepareStockLine("shipment.held", "30", "res.wire")
	w.must(err)
	w.granted("shipment.held")
	_, err = w.confirm(w.merch, "shipment.held", held.RowVersion)
	w.must(err)
	if bal := w.stockBalance(admin); !sameQty(t, bal.OnHand, "20") || !sameQty(t, bal.Reserved, "0") {
		t.Fatalf("held confirm %+v", bal)
	}

	w.confirmQuantity("shipment.direct", "10")
	if bal := w.stockBalance(admin); !sameQty(t, bal.OnHand, "20") {
		t.Fatalf("direct ship touched stock %+v", bal)
	}
	if guardValue(w.shipment("shipment.direct"), "stk.stock_availability.v1:S1") != "" {
		t.Fatalf("direct ship recorded a stock guard")
	}
}

// Two confirms of the same order each want the only 40 on hand. The fulfilment root serializes
// them; one commits and the other is STOCK_INSUFFICIENT. The balance is not driven negative.
func TestStockRouteConcurrentOneWins(t *testing.T) {
	w := newFulWorld(t)
	w.useStock()
	admin := w.stockAdmin()
	_, err := w.c.STK.ConfigureModule(admin, stkapi.ConfigureModule{IdempotencyKey: w.key(), Enabled: true})
	w.must(err)
	w.stockReceive(admin, "mv.one", "40", "1000", "2026-01-01T00:00:00.000000Z")
	for _, id := range []string{"shipment.a", "shipment.b"} {
		_, err := w.prepareStockLine(id, "40", "")
		w.must(err)
		w.granted(id)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i, id := range []string{"shipment.a", "shipment.b"} {
		go func(i int, id string) {
			defer wg.Done()
			<-start
			_, errs[i] = w.confirm(w.merch, id, 1)
		}(i, id)
	}
	close(start)
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch code(err) {
		case "OK":
			wins++
		case fulapi.CodeStockInsufficient:
		default:
			t.Fatalf("concurrent %v", errs)
		}
	}
	if wins != 1 {
		t.Fatalf("wins %d %v", wins, errs)
	}
	if bal := w.stockBalance(admin); !sameQty(t, bal.OnHand, "0") {
		t.Fatalf("balance %+v", bal)
	}
}

// A wired STK that the company has not enabled refuses the stock route at prepare.
func TestStockRouteOffRefuses(t *testing.T) {
	w := newFulWorld(t)
	w.useStock()
	_, err := w.prepareStockLine("shipment.off", "10", "")
	wantRefused(t, "stock route while STK is off", err, fulapi.CodeDependencyNotSupported, "/args/route")
}

func hasStockCost(v stlapi.OrderResultView, minor string) bool {
	for _, s := range v.Live.Confirmed.Sources {
		if s.Kind == "stock_cost" && strings.Contains(s.Detail, minor) {
			return true
		}
	}
	return false
}

// An approved credit_note remedy settles once on credit_note.request, and an approved refund
// remedy settles once on fund.record. A second settlement is CLAIM_REMEDY_ALREADY_SETTLED. An
// open claim makes the claims dimension unmet and order_result.close names it. No claim is met.
func TestClaimCreditRefundAndOpenClaimBlocksClose(t *testing.T) {
	w := newStatementWorld(t)
	order := w.order.ObjectID
	clerk := w.assign("clerk.wire", "sales_clerk")
	reviewer := w.assign("reviewer.wire", "order_reviewer")
	w.prepaid()
	w.shipped("shipment.1", "120", w.verified("delivery.1", "120", "120"))
	_, err := w.c.STL.ConfirmSettlement(w.treasury, salesCmd(w.key(), "statement.sales", "shipment.1"))
	w.must(err)
	reviewed, err := w.c.STL.ReviewOrderResult(w.treasury, stlapi.ReviewOrderResult{IdempotencyKey: w.key(), OrderID: order,
		Note: "出运后复核，尚无客诉"})
	w.must(err)
	before, err := w.c.STL.GetOrderResult(w.treasury, order)
	w.must(err)
	claims := conditionOf(before.Conditions, stlapi.DimensionClaims)
	if claims.Status != stlapi.ConditionMet || len(claims.Items) != 1 || claims.Items[0] != "no open claim" {
		t.Fatalf("no claim %+v", claims)
	}

	_, err = w.c.CLM.OpenClaim(w.merch, clmapi.OpenClaim{IdempotencyKey: w.key(), ClaimID: "claim.wire", OrderID: order,
		ShipmentID: "shipment.1", ShipmentLineID: "S1", Reason: "客户投诉色差"})
	w.must(err)
	_, err = w.c.CLM.DecideRemedy(reviewer, clmapi.DecideRemedy{IdempotencyKey: w.key(), ClaimID: "claim.wire", RemedyID: "rem.wire",
		RemedyKind: clmapi.KindCreditNote, AmountMinor: "10000", Reason: "色差折让", ExpectedVersion: 1})
	w.must(err)
	open, err := w.c.STL.GetOrderResult(w.treasury, order)
	w.must(err)
	claims = conditionOf(open.Conditions, stlapi.DimensionClaims)
	if claims.Status != stlapi.ConditionUnmet || len(claims.Items) != 1 || claims.Items[0] != "claim claim.wire is open" {
		t.Fatalf("open claim %+v", claims)
	}
	_, err = w.c.STL.CloseOrderResult(w.treasury, stlapi.CloseOrderResult{IdempotencyKey: w.key(), OrderID: order,
		ExpectedVersion: reviewed.RowVersion, Reason: "客诉未结"})
	if code(err) != stlapi.CodeOrderResultNotSettled || !hasPath(err, "/conditions/claims") {
		t.Fatalf("close %v paths %v", err, fieldPaths(err))
	}

	high := w.creditNoteCmd("cn.high", "statement.sales", "150.00")
	high.ClaimID, high.RemedyID = "claim.wire", "rem.wire"
	_, err = w.c.STL.RequestCreditNote(clerk, high)
	wantRefused(t, "above the approved remedy", err, kernel.CodeInvalidKnownValue, "/args/amount")
	note := w.creditNoteCmd("cn.wire", "statement.sales", "100.00")
	note.ClaimID, note.RemedyID = "claim.wire", "rem.wire"
	got, err := w.c.STL.RequestCreditNote(clerk, note)
	w.must(err)
	if got.Status != stlapi.CreditRequested {
		t.Fatalf("credit %+v", got)
	}
	again := w.creditNoteCmd("cn.wire.2", "statement.sales", "50.00")
	again.ClaimID, again.RemedyID = "claim.wire", "rem.wire"
	_, err = w.c.STL.RequestCreditNote(clerk, again)
	wantRefused(t, "the remedy settled by the credit note", err, stlapi.CodeClaimRemedyAlreadySettled, "/args/remedy_id")

	_, err = w.c.CLM.OpenClaim(w.merch2, clmapi.OpenClaim{IdempotencyKey: w.key(), ClaimID: "claim.refund", OrderID: order,
		ShipmentID: "shipment.1", ShipmentLineID: "S1", Reason: "客户要求退款"})
	w.must(err)
	_, err = w.c.CLM.DecideRemedy(reviewer, clmapi.DecideRemedy{IdempotencyKey: w.key(), ClaimID: "claim.refund", RemedyID: "rem.refund",
		RemedyKind: clmapi.KindRefund, AmountMinor: "10000", Reason: "退回部分货款", ExpectedVersion: 1})
	w.must(err)
	w.fund("fund.spare", stlapi.FundReceipt, "200.00", "200.00")
	refund := stlapi.RecordFund{IdempotencyKey: w.key(), FundRecordID: "refund.wire", FundKind: stlapi.FundRefund,
		SourceFundRecordID: "fund.spare", Currency: stlRef(w.usd), BankAccount: stlRef(w.bankUSD), ReceivedAmount: "40.00",
		ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "ref.refund.wire", EvidenceIDs: []string{"evidence"},
		ClaimID: "claim.refund", RemedyID: "rem.refund", OrderID: order}
	if _, err = w.c.STL.RecordFund(w.treasury, refund); err != nil {
		t.Fatalf("refund: %v", err)
	}
	refund.IdempotencyKey, refund.FundRecordID, refund.BankReference, refund.ReceivedAmount = w.key(), "refund.wire.2", "ref.refund.wire.2", "10.00"
	_, err = w.c.STL.RecordFund(w.treasury, refund)
	wantRefused(t, "the remedy settled by the refund", err, stlapi.CodeClaimRemedyAlreadySettled, "/args/remedy_id")
}
