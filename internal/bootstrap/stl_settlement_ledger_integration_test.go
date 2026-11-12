//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Settlement statements on stand-ins for TRD and FUL (WBS S5-04b): STL's ports Orders and Shipments
// are steered, so an order can carry baseline 4.6 S-A's plan (USD 1,000: 20 % before, 80 % after
// shipment) and goods can have left in the past. PUR's port is the composed one. The fund world
// (stl_funds_integration_test.go) and the credit world (stl_credit_integration_test.go) provide the
// people, POL configurations and the stand-in orders.

// shipmentLines is the stand-in for FUL's ShipmentLineRef v1.
type shipmentLines struct {
	mu    sync.Mutex
	lines map[stlapi.SettlementSourceRef]stl.ShipmentSource
}

func (f *shipmentLines) ResolveShipmentSources(_ context.Context, keys []stlapi.SettlementSourceRef) (map[stlapi.SettlementSourceRef]stl.ShipmentSource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[stlapi.SettlementSourceRef]stl.ShipmentSource{}
	for _, k := range keys {
		if l, ok := f.lines[k]; ok {
			out[k] = l
		}
	}
	return out, nil
}

// shipped has FUL show line S1 of confirmed shipment id: quantity of order o's line L1 (100 PCS at
// 10.00 USD, 1,000.00), valued payableMinor, the goods left at at.
func (f *shipmentLines) shipped(o stl.Order, id, quantity, payableMinor string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines[stlapi.SettlementSourceRef{ShipmentID: id, LineID: "S1"}] = stl.ShipmentSource{Line: stl.ShipmentLine{ShipmentID: id, LineID: "S1",
		ShipmentVersion: 2, ShippedAt: kernel.Instant(at), OrderID: o.Binding.OrderID, OrderRevision: o.Binding.OrderRevision,
		OrderHash: o.Binding.OrderHash, CustomerID: o.CustomerID, OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS", UnitRevision: 1,
		OrderLineQuantity: "100", Quantity: quantity, Currency: "USD", UnitPrice: "10", BasisNet: "100000", BasisTax: "0", Net: payableMinor,
		Tax: "0", Payable: payableMinor}}
}

// withShipments is an STL module of w with orders and the stand-in shipment lines.
func withShipments(w *world, orders stl.Orders) (*stl.Module, *shipmentLines) {
	w.t.Helper()
	f := &shipmentLines{lines: map[stlapi.SettlementSourceRef]stl.ShipmentSource{}}
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	w.must(err)
	m, err := stl.New(db, stl.Upstream{Orders: orders, Configs: w.c.POL, Parties: w.c.PTY, Sources: w.c.EVD, Shipments: f,
		Deliveries: stlDeliveries(w.c)})
	w.must(err)
	return m, f
}

// newLedgerWorld is the fund world with the stand-in shipment lines.
func newLedgerWorld(t *testing.T) (*fundWorld, *shipmentLines) {
	t.Helper()
	fw := newFundWorld(t)
	var f *shipmentLines
	fw.stl, f = withShipments(fw.world, fw.orders)
	return fw, f
}

func (fw *fundWorld) settle(id string, shipments ...string) stlapi.StatementReceipt {
	fw.t.Helper()
	r, err := fw.stl.ConfirmSettlement(fw.treasury, salesCmd(fw.key(), id, shipments...))
	fw.must(err)
	return r
}

func (fw *fundWorld) applyTo(id, fund, statement, stage, amount string) (stlapi.ApplicationReceipt, error) {
	return fw.stl.Apply(fw.treasury, stlapi.Apply{IdempotencyKey: fw.key(), ApplicationID: id, FundRecordID: fund,
		Target: toStatement(stlapi.TargetReceivable, statement, stage), Amount: amount})
}

func (fw *fundWorld) receivable(id string) stlapi.BalanceView {
	fw.t.Helper()
	v, err := fw.stl.GetReceivable(fw.treasury, id)
	fw.must(err)
	return v
}

// Baseline 4.6 S-A as far as statements go, every amount exact (STL-INV-002 positive "预收 200 核销 600
// 应收后余 400；收 800 分别核销 400＋400"): sale 100 × USD 10.00 on 20 % before and 80 % after shipment.
//
//  1. USD 200.00 received before shipment is applied to P1 (a prepayment, no receivable yet).
//  2. The first shipment of 60 is settled at USD 600.00: the prepayment moves to the receivable, which
//     is open USD 400.00.
//  3. The second of 40 is settled at USD 400.00: settled 1,000.00, open 800.00.
//  4. USD 800.00 is received and sold at 6.875 for CNY 5,500.00 (a transfer, not a receipt: CN-AT09);
//     the USD receipt is applied 400.00 + 400.00 to the two receivables.
//  7. The receivables are 0; the order's position shows P1 and P2 settled and paid in full.
func TestBaseline46SAStatements(t *testing.T) {
	fw, ful := newLedgerWorld(t)
	o := fw.activeOrder("order.sa", "customer.1", fw.usd, payStage("P1", "order_confirmed", "20000"),
		stl.OrderStage{StageID: "P2", Trigger: "after_shipment", Days: 30, Percent: "80", AmountMinor: "80000"})
	fw.receipt("fund.pre", "200.00", "")
	_, err := fw.apply("app.pre", "fund.pre", "order.sa", "P1", "200.00")
	fw.must(err)
	ful.shipped(o, "shipment.60", "60", "60000", time.Now().Add(-48*time.Hour))
	ful.shipped(o, "shipment.40", "40", "40000", time.Now().Add(-24*time.Hour))

	first := fw.settle("statement.600", "shipment.60")
	if first.AmountMinor != "60000" || first.OpenMinor != "40000" || !slices.Equal(first.MovedApplications, []string{"app.pre"}) {
		t.Fatalf("first statement %+v", first)
	}
	if got := installmentsOf(fw.receivable("statement.600")); len(got) != 2 || got[0] != [4]string{"P1", "20000", "20000", "0"} ||
		got[1] != [4]string{"P2", "40000", "0", "40000"} {
		t.Fatalf("first receivable %v", got)
	}
	second := fw.settle("statement.400", "shipment.40")
	if second.AmountMinor != "40000" || second.OpenMinor != "40000" || len(second.MovedApplications) != 0 {
		t.Fatalf("second statement %+v", second)
	}
	if got := installmentsOf(fw.receivable("statement.400")); len(got) != 1 || got[0] != [4]string{"P2", "40000", "0", "40000"} {
		t.Fatalf("second receivable %v", got)
	}
	pos, err := fw.stl.GetOrderSettlement(fw.treasury, "order.sa")
	fw.must(err)
	if pos.SettledMinor != "100000" || pos.AppliedMinor != "20000" || pos.OpenMinor != "80000" || pos.PrepaidMinor != "0" ||
		pos.Stages[0].SettledMinor != "20000" || pos.Stages[0].PaidMinor != "20000" || pos.Stages[1].SettledMinor != "80000" ||
		pos.Stages[1].PaidMinor != "0" || len(pos.Receivables) != 2 {
		t.Fatalf("position after two statements %+v", pos)
	}

	fw.receipt("fund.800", "800.00", "")
	c, err := fw.stl.RecordFund(fw.treasury, stlapi.RecordFund{IdempotencyKey: fw.key(), FundRecordID: "fx.800", FundKind: stlapi.FundFXConversion,
		SourceFundRecordID: "fund.800", Currency: stlRef(fw.cny), BankAccount: stlRef(fw.bankCNY), ReceivedAmount: "5500.00",
		ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "cny.line.sa", EvidenceIDs: []string{fw.advice},
		FX: &stlapi.FXSale{SoldAmount: "800.00", BankRate: "6.875"}})
	fw.must(err)
	if c.ReceivedMinor != "550000" {
		t.Fatalf("conversion %+v", c)
	}
	_, err = fw.applyTo("app.fx", "fx.800", "statement.600", "", "1.00")
	fw.want(err, kernel.CodeInvalidState)
	for i, x := range []struct{ id, statement string }{{"app.400a", "statement.600"}, {"app.400b", "statement.400"}} {
		a, err := fw.applyTo(x.id, "fund.800", x.statement, "", "400.00")
		fw.must(err)
		if a.FundAvailableMinor != []string{"40000", "0"}[i] || a.StatementOpenMinor != "0" || a.StageID != "P2" {
			t.Fatalf("%s %+v", x.id, a)
		}
	}
	pos, err = fw.stl.GetOrderSettlement(fw.treasury, "order.sa")
	fw.must(err)
	if pos.SettledMinor != "100000" || pos.AppliedMinor != "100000" || pos.OpenMinor != "0" || pos.Stages[1].PaidMinor != "80000" {
		t.Fatalf("position at the end %+v", pos)
	}
	ref, err := fw.stl.ResolveFundRecords(fw.treasury, []string{"fund.800"})
	fw.must(err)
	if f := ref["fund.800"]; f.AppliedMinor != "80000" || f.AvailableMinor != "0" || f.CashMinor != "0" || len(f.Allocations) != 2 ||
		f.Allocations[0].TargetKind != stlapi.TargetReceivable || f.Allocations[0].StatementID != "statement.600" {
		t.Fatalf("USD receipt %+v", f)
	}
	if v := fw.paymentCondition("order.sa", 1, stlapi.ScopeShipment); !v.Satisfied() {
		t.Fatalf("shipment scope %+v", v)
	}
}

// D-4 in parts, STL-INV-002/003 on receivables: a 50 % deposit of 500.00 meets a first statement of
// 300.00 — 300.00 moves (replacement on the receivable) and 200.00 stays on P1 as a remainder of the
// same receipt, so P1 still reads 500.00 paid and nothing more can be prepaid on it; the second
// statement of 700.00 takes the remainder (P1 200.00) and P2 500.00. P2 is due 30 days after shipment
// under document control (D-5): the documents scope is blocked until 500.00 is applied to the
// receivable's P2 installment. A moved application cannot be unapplied (its replacement can);
// unapplying an application on a receivable reopens it, returns the money to the fund and re-judges
// the payment condition; applied again, it is satisfied again.
func TestPrepaymentsMoveInPartsAndReceivablesUnapply(t *testing.T) {
	fw, ful := newLedgerWorld(t)
	o := fw.activeOrder("order.pm", "customer.1", fw.usd, payStage("P1", "order_confirmed", "50000"),
		stl.OrderStage{StageID: "P2", Trigger: "after_shipment", Days: 30, Percent: "50", AmountMinor: "50000", ReleaseControl: "documents"})
	fw.receipt("fund.500", "500.00", "")
	_, err := fw.apply("app.500", "fund.500", "order.pm", "P1", "500.00")
	fw.must(err)
	ful.shipped(o, "shipment.30", "30", "30000", time.Now().Add(-2*time.Hour))
	ful.shipped(o, "shipment.70", "70", "70000", time.Now().Add(-time.Hour))

	first := fw.settle("statement.300", "shipment.30")
	if first.OpenMinor != "0" || !slices.Equal(first.MovedApplications, []string{"app.500"}) {
		t.Fatalf("first statement %+v", first)
	}
	apps, err := fw.stl.GetOrderAccount(fw.treasury, "order.pm")
	fw.must(err)
	var remainder, replacement stlapi.AllocationView
	for _, a := range apps.Applications {
		switch {
		case a.MovedFrom == "app.500" && a.TargetKind == stlapi.TargetOrderStage:
			remainder = a
		case a.MovedFrom == "app.500":
			replacement = a
		}
	}
	if remainder.AmountMinor != "20000" || remainder.Status != stlapi.ApplicationApplied || replacement.AmountMinor != "30000" ||
		replacement.StatementID != "statement.300" || apps.Stages[0].AppliedMinor != "50000" {
		t.Fatalf("after the first statement: %+v", apps)
	}
	fw.receipt("fund.extra", "1.00", "")
	_, err = fw.apply("app.extra", "fund.extra", "order.pm", "P1", "1.00")
	fw.want(err, stlapi.CodeApplicationExceedsOpen)

	second := fw.settle("statement.700", "shipment.70")
	if second.OpenMinor != "50000" || !slices.Equal(second.MovedApplications, []string{remainder.ApplicationID}) {
		t.Fatalf("second statement %+v", second)
	}
	if got := installmentsOf(fw.receivable("statement.700")); len(got) != 2 || got[0] != [4]string{"P1", "20000", "20000", "0"} ||
		got[1] != [4]string{"P2", "50000", "0", "50000"} {
		t.Fatalf("second receivable %v", got)
	}
	if v := fw.paymentCondition("order.pm", 1, stlapi.ScopeShipment); !v.Satisfied() || v.AppliedMinor != "50000" {
		t.Fatalf("shipment scope after both moves %+v", v)
	}
	if v := fw.paymentCondition("order.pm", 1, stlapi.ScopeDocuments); v.Satisfied() || v.StageID != "P2" || v.ShortfallMinor != "50000" {
		t.Fatalf("documents scope before P2 is paid %+v", v)
	}
	fw.receipt("fund.p2", "500.00", "")
	if _, err = fw.applyTo("app.p2", "fund.p2", "statement.700", "P1", "500.00"); code(err) != stlapi.CodeApplicationExceedsOpen {
		t.Fatalf("P1's installment is paid: %v", err)
	}
	_, err = fw.applyTo("app.p2", "fund.p2", "statement.700", "P2", "500.00")
	fw.must(err)
	if v := fw.paymentCondition("order.pm", 1, stlapi.ScopeDocuments); !v.Satisfied() || v.AppliedMinor != "100000" {
		t.Fatalf("documents scope once P2 is paid on the receivable %+v", v)
	}

	_, err = fw.unapply("app.500", 2)
	fw.want(err, kernel.CodeInvalidState)
	u, err := fw.unapply("app.p2", 1)
	fw.must(err)
	if u.Status != stlapi.ApplicationUnapplied || u.StatementOpenMinor != "50000" || u.FundAvailableMinor != "50000" {
		t.Fatalf("unapplied from the receivable %+v", u)
	}
	if v := fw.paymentCondition("order.pm", 1, stlapi.ScopeDocuments); v.Satisfied() || v.StageID != "P2" {
		t.Fatalf("documents scope after the unapply %+v", v)
	}
	_, err = fw.applyTo("app.p2.again", "fund.p2", "statement.700", "P2", "500.00")
	fw.must(err)
	u, err = fw.unapply(replacement.ApplicationID, 1)
	fw.must(err)
	if u.FundAvailableMinor != "30000" || u.StatementOpenMinor != "30000" {
		t.Fatalf("the replacement unapplied %+v", u)
	}
	if v := fw.paymentCondition("order.pm", 1, stlapi.ScopeShipment); v.Satisfied() || v.StageID != "P1" || v.ShortfallMinor != "30000" {
		t.Fatalf("shipment scope once the moved prepayment is unapplied %+v", v)
	}
	_, err = fw.applyTo("app.300", "fund.500", "statement.300", "", "300.00")
	fw.must(err)
	for _, scope := range []string{stlapi.ScopeShipment, stlapi.ScopeDocuments} {
		if v := fw.paymentCondition("order.pm", 1, scope); !v.Satisfied() {
			t.Fatalf("%s scope at the end %+v", scope, v)
		}
	}
	if r := fw.receivable("statement.300"); r.OpenMinor != "0" || len(r.Applications) != 2 {
		t.Fatalf("first receivable at the end %+v", r)
	}
}

// STL-INV-004 with receivables (D-4; SIM-02 step 11 "最早逾期 45 天超过容忍 30 天"): customer.1 (limit
// 10,000.00 USD, tolerance 30 days) owes 1,000.00 on an order settled when its goods left 75 days ago,
// due 30 days after shipment: 45 days overdue. The receivable shows it; the exposure counts the open
// 1,000.00; credit.reserve for a new order within the limit opens a decision with
// CREDIT_OVERDUE_EXCEEDED and reserves nothing. Once the receivable is paid, the reservation is made
// within the limit.
func TestOverdueReceivablesFeedTheCreditTolerance(t *testing.T) {
	cw := newCreditWorld(t)
	var ful *shipmentLines
	cw.stl, ful = withShipments(cw.world, cw.orders)
	treasury := cw.assign("treasury.1", "treasury")
	bankUSD := cw.configure(cw.editor, cw.reviewer, polapi.KindBankAccount, "bank.usd", 0, cw.win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "USD", polapi.AccountBoth), true)
	advice := cw.source(cw.editor, "advice.1", "合成银行入账通知")
	cw.limit()
	old := stl.Order{Binding: stl.OrderBinding{OrderID: "order.old", OrderRevision: 1, OrderHash: orderHashOf("order.old", 1)}, Status: stl.OrderActive,
		CustomerID: "customer.1", Currency: stl.CurrencyRef{ID: cw.usd.ConfigID, Revision: cw.usd.Revision}, Stages: []stl.OrderStage{{StageID: "P2",
			Trigger: "after_shipment", Days: 30, Percent: "100", AmountMinor: "100000", OnCredit: true}}, CreditPortionMinor: "100000"}
	cw.orders.put(old)
	_, err := cw.stl.OpenOrderAccount(treasury, stlapi.OpenOrderAccount{IdempotencyKey: cw.key(), OrderID: "order.old", OrderRevision: 1,
		OrderHash: old.Binding.OrderHash})
	cw.must(err)
	ful.shipped(old, "shipment.old", "100", "100000", time.Now().Add(-75*24*time.Hour))
	_, err = cw.stl.ConfirmSettlement(treasury, salesCmd(cw.key(), "statement.old", "shipment.old"))
	cw.must(err)
	r, err := cw.stl.GetReceivable(treasury, "statement.old")
	cw.must(err)
	if len(r.Installments) != 1 || r.Installments[0].OverdueDays != 45 || r.OpenMinor != "100000" {
		t.Fatalf("overdue receivable %+v", r)
	}
	pos, err := cw.stl.GetOrderSettlement(treasury, "order.old")
	cw.must(err)
	if pos.OverdueMinor != "100000" {
		t.Fatalf("position %+v", pos)
	}
	if v, err := cw.stl.GetCustomerCredit(cw.officer, "customer.1"); err != nil || v.ExposureMinor != "100000" || v.ExposureCurrency != "USD" {
		t.Fatalf("exposure %+v %v", v, err)
	}
	o := cw.onCredit("order.new", "50000")
	opened, err := cw.reserve(cw.sales, "res.new", o)
	cw.must(err)
	if opened.Status != stlapi.StatusDecisionOpened || !slices.Contains(opened.Reasons, stlapi.CodeCreditOverdueExceeded) ||
		slices.Contains(opened.Reasons, stlapi.CodeCreditLimitExceeded) || opened.ExposureMinor != "100000" {
		t.Fatalf("reservation with a receivable 45 days overdue %+v", opened)
	}
	_, err = cw.stl.RecordFund(treasury, stlapi.RecordFund{IdempotencyKey: cw.key(), FundRecordID: "fund.old", FundKind: stlapi.FundReceipt,
		CounterpartyID: "customer.1", Currency: stlRef(cw.usd), BankAccount: stlRef(bankUSD), ReceivedAmount: "1000.00",
		ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "ref.old", EvidenceIDs: []string{advice}})
	cw.must(err)
	_, err = cw.stl.Apply(treasury, stlapi.Apply{IdempotencyKey: cw.key(), ApplicationID: "app.old", FundRecordID: "fund.old",
		Target: toStatement(stlapi.TargetReceivable, "statement.old", ""), Amount: "1000.00"})
	cw.must(err)
	reserved, err := cw.reserve(cw.sales, "res.new.2", o)
	cw.must(err)
	if reserved.Status != stlapi.ReservationReserved || reserved.Basis != stlapi.BasisWithinLimit || reserved.ExposureMinor != "0" {
		t.Fatalf("reservation once paid %+v", reserved)
	}
}

// STL-INV-003 with receivables ("撤销核销与 FUL 放单并发按锁序裁决"): an application on a receivable
// counts for its stage, so its unapply locks the order account FOR UPDATE (with the fund record and
// the statement, in ID order) and waits for a FUL transaction that holds PaymentConditionGuard v1's
// FOR SHARE on the account; the guard judged by the application, and only after FUL committed is the
// stage unpaid. Unapplied first, the guard is blocked.
func TestReceivableUnapplyWaitsForTheGuard(t *testing.T) {
	fw, ful := newLedgerWorld(t)
	o := fw.activeOrder("order.g", "customer.1", fw.usd, payStage("P1", "order_confirmed", "30000"),
		stl.OrderStage{StageID: "P2", Trigger: "after_shipment", Days: 30, Percent: "70", AmountMinor: "70000", ReleaseControl: "documents"})
	ful.shipped(o, "shipment.g", "100", "100000", time.Now().Add(-time.Hour))
	fw.settle("statement.g", "shipment.g")
	fw.receipt("fund.g", "1000.00", "")
	_, err := fw.applyTo("app.g1", "fund.g", "statement.g", "P1", "300.00")
	fw.must(err)
	_, err = fw.applyTo("app.g2", "fund.g", "statement.g", "P2", "700.00")
	fw.must(err)

	db, err := bentopg.NewDB(pgtest.AppPool(t))
	fw.must(err)
	held, release, judged := make(chan struct{}), make(chan struct{}), make(chan stlapi.PaymentVerdict, 1)
	go func() {
		q := stlapi.PaymentQuery{OrderID: "order.g", OrderRevision: 1, Scope: stlapi.ScopeDocuments, At: time.Now()}
		_ = bento.ScopeTransactor{DB: db, Scope: fw.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
			exec, err := db.RequireExecutor(ctx)
			if err != nil {
				close(held)
				return err
			}
			v, err := stlapi.ScanPaymentCondition(q, exec.QueryRow(ctx, stlapi.PaymentConditionSQL, stlapi.PaymentConditionArgs(q)...).Scan)
			judged <- v
			close(held)
			<-release
			return err
		})
	}()
	<-held
	done := make(chan error, 1)
	go func() {
		_, err := fw.unapply("app.g2", 1)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the unapply did not wait for the guard: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if v := <-judged; !v.Satisfied() {
		t.Fatalf("the guard judged by the application %+v", v)
	}
	close(release)
	fw.must(<-done)
	if v := fw.paymentCondition("order.g", 1, stlapi.ScopeDocuments); v.Satisfied() || v.StageID != "P2" || v.ShortfallMinor != "70000" {
		t.Fatalf("documents scope after the unapply %+v", v)
	}
	if v := fw.paymentCondition("order.g", 1, stlapi.ScopeShipment); !v.Satisfied() {
		t.Fatalf("shipment scope stays satisfied %+v", v)
	}
}

// C1's finding, fixed in stl 000004 (PaymentConditionGuard v1; C4b-STL's carry-over by stage ID):
// order.am's revision 1 asks P1 500.00, P2 200.00, P3 200.00 before shipment; 500.00, 100.00 and
// 100.00 are applied. Amended to 300.00 each, the payment plan review moves the account to revision
// 2: P1 is over-applied by 200.00 (a review item), P2 and P3 are short 200.00 each. The guard is
// blocked on P2 with the scope's totals required 900.00, applied 500.00 — P1 counts its due only —,
// so the scope's shortfall (400.00) differs from P2's (200.00) and FUL's exception rule sees two
// unpaid stages. Counted in full, applied would be 700.00 and the shortfalls equal (200.00).
func TestAnOverAppliedStageDoesNotHideAnotherShortfall(t *testing.T) {
	fw, _ := newLedgerWorld(t)
	before := func(id, amount string) stl.OrderStage {
		return stl.OrderStage{StageID: id, Trigger: "before_shipment", Percent: "0", AmountMinor: amount}
	}
	o := fw.activeOrder("order.am", "customer.1", fw.usd, payStage("P1", "order_confirmed", "50000"), before("P2", "20000"), before("P3", "20000"))
	fw.receipt("fund.am", "700.00", "")
	for _, x := range [][2]string{{"P1", "500.00"}, {"P2", "100.00"}, {"P3", "100.00"}} {
		_, err := fw.apply("app.am."+x[0], "fund.am", "order.am", x[0], x[1])
		fw.must(err)
	}
	o.Binding = stl.OrderBinding{OrderID: "order.am", OrderRevision: 2, OrderHash: orderHashOf("order.am", 2)}
	o.Stages = []stl.OrderStage{payStage("P1", "order_confirmed", "30000"), before("P2", "30000"), before("P3", "30000")}
	fw.orders.put(o)
	_, err := fw.stl.OpenPaymentPlanReview(fw.system, stlapi.OpenPaymentPlanReview{IdempotencyKey: fw.key(), OrderID: "order.am", OrderRevision: 2,
		OrderHash: o.Binding.OrderHash})
	fw.must(err)
	v := fw.paymentCondition("order.am", 2, stlapi.ScopeShipment)
	if v.Satisfied() || v.Reason != stlapi.PaymentStageUnpaid || v.StageID != "P2" || v.StageDueMinor != "30000" || v.StageAppliedMinor != "10000" ||
		v.ShortfallMinor != "20000" || v.RequiredMinor != "90000" || v.AppliedMinor != "50000" {
		t.Fatalf("the guard after the review %+v", v)
	}
	acc, err := fw.stl.GetOrderAccount(fw.treasury, "order.am")
	fw.must(err)
	if c := acc.Conditions[0]; c.Status != stlapi.PaymentBlocked || c.StageID != "P2" || c.RequiredMinor != "90000" || c.AppliedMinor != "50000" ||
		acc.Stages[0].AppliedMinor != "50000" {
		t.Fatalf("the order account after the review %+v", acc)
	}
}
