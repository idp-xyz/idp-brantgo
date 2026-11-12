//go:build integration

package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The STL settlement statement suite (WBS S5-04b; tactical model STL-AGG-SettlementStatement,
// STL-AGG-Application; SM-STL-SETTLEMENT, SM-STL-APPLICATION; STL-INV-001, STL-INV-002, STL-INV-003,
// STL-INV-004; UL-INV-22, UL-INV-23; Context Map R30 ShipmentLineRef v1, R31 PurchaseSettlementSource
// v1, R45 PaymentConditionGuard v1; S5 plan D-3, D-4, D-5, §5 D1 steps 19–21; baseline 4.2, 4.3, 4.6
// S-A) on PostgreSQL 16. Python V0.50 has no STL, so the tests cite design IDs only.
//
// The statement world is the composed process on BRC-001's plan: TRD's order (120 PCS of sku.1 at
// 8.00 = 960.00 USD; P1 30 % 288.00 on order confirmation, P2 70 % 672.00 30 days after shipment)
// activated with its 672.00 credit reservation; PUR's commitment.1 (120 at 5.00 = 600.00 USD from
// supplier.1) issued; QLT's release of 120; deliveries recorded by the merchandiser and verified by
// the purchaser; shipments prepared, released (FUL's commercial release, unit C1: P1 must be paid)
// and confirmed by the composed FUL; and the composed STL, whose ports read FUL's shipment lines and
// PUR's verified deliveries. The FUL world opens the order account and pays every stage in full
// (releasable); the statement world unapplies those prepayments, so each test pays what BRC-001
// pays. The treasurer (treasury.1) records and applies funds and confirms statements; the credit
// manager (manager.1) resolves bank charges. The ledger world (stl_settlement_ledger_integration_test.go)
// runs STL with stand-ins for TRD and FUL to give baseline 4.6 S-A's exact numbers and dates in the past.

// stlShipments and stlDeliveries are STL's ports to the composed FUL and PUR of c.
func stlShipments(c *bootstrap.Contexts) stl.Shipments {
	return stl.FULShipmentLines(func() fulapi.ShipmentLineLookups { return c.FUL })
}

func stlDeliveries(c *bootstrap.Contexts) stl.Deliveries { return stl.PURSettlementSources(c.PUR) }

// paymentCondition runs PaymentConditionGuard v1 as FUL will (unit C1): in a transaction of another
// context with its own executor, in company.A.
func (w *world) paymentCondition(order string, revision int64, scope string) stlapi.PaymentVerdict {
	w.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	w.must(err)
	q := stlapi.PaymentQuery{OrderID: order, OrderRevision: revision, Scope: scope, At: time.Now()}
	var v stlapi.PaymentVerdict
	w.must(bento.ScopeTransactor{DB: db, Scope: w.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		v, err = stlapi.ScanPaymentCondition(q, exec.QueryRow(ctx, stlapi.PaymentConditionSQL, stlapi.PaymentConditionArgs(q)...).Scan)
		return err
	}))
	return v
}

type statementWorld struct {
	*fulPurWorld
	treasury2 context.Context
	revision  int64
}

// brcPlan is BRC-001's payment terms (S5 plan §5; TRD-INV-022's positive example).
var brcPlan = &polapi.PaymentTerms{Name: "30% on order confirmation, 70% 30 days after shipment", Stages: []polapi.PaymentStage{
	{StageID: "P1", Percent: "30", Trigger: polapi.TriggerOrderConfirmed},
	{StageID: "P2", Percent: "70", Trigger: polapi.TriggerAfterShipment, Days: 30}}}

func newStatementWorld(t *testing.T) *statementWorld {
	t.Helper()
	paymentPlans.Store(t, brcPlan)
	defer paymentPlans.Delete(t)
	w := &statementWorld{fulPurWorld: newFulPurWorld(t)}
	w.treasury2 = w.assign("treasury.2", "treasury")
	snap := w.snapshots(w.author, w.order.ObjectID)[w.order.ObjectID]
	if len(snap.Stages) != 2 || snap.Stages[0].StageID != "P1" || snap.Stages[0].AmountMinor != "28800" || snap.Stages[1].AmountMinor != "67200" {
		t.Fatalf("BRC-001's plan in TRD's snapshot: %+v", snap.Stages)
	}
	w.revision = snap.Binding.OrderRevision
	for _, s := range snap.Stages {
		_, err := w.c.STL.Unapply(w.treasury, stlapi.Unapply{IdempotencyKey: w.key(), ApplicationID: "app." + w.order.ObjectID + "." + s.StageID,
			ExpectedVersion: 1, Reason: "合成：撤销夹具的全额预收，按 BRC-001 的付款重新核销"})
		w.must(err)
	}
	return w
}

// prepaid has customer.1's 288.00 recorded as fund.p1 and applied to P1 as app.p1 (BRC-001 step 7):
// what FUL's commercial release of a shipment needs.
func (w *statementWorld) prepaid() {
	w.t.Helper()
	w.fund("fund.p1", stlapi.FundReceipt, "288.00", "")
	_, err := w.c.STL.Apply(w.treasury, stlapi.Apply{IdempotencyKey: w.key(), ApplicationID: "app.p1", FundRecordID: "fund.p1",
		Target: stlapi.ApplicationTarget{Kind: stlapi.TargetOrderStage, OrderID: w.order.ObjectID, StageID: "P1"}, Amount: "288.00"})
	w.must(err)
}

// fund records fund id of kind (receipt from customer.1, or payment to supplier.1) on the company's
// USD account: received, and for a receipt remitted.
func (w *statementWorld) fund(id, kind, received, remitted string) stlapi.FundRecordReceipt {
	w.t.Helper()
	party := "customer.1"
	if kind == stlapi.FundPayment {
		party = "supplier.1"
	}
	r, err := w.c.STL.RecordFund(w.treasury, stlapi.RecordFund{IdempotencyKey: w.key(), FundRecordID: id, FundKind: kind, CounterpartyID: party,
		Currency: stlRef(w.usd), BankAccount: stlRef(w.bankUSD), ReceivedAmount: received, RemittedAmount: remitted,
		ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "ref." + id, EvidenceIDs: []string{"evidence"}})
	w.must(err)
	return r
}

// shipped has the merchandiser prepare shipment id of quantity from delivery d, the documentation
// clerk grant its commercial release (C1) and the merchandiser confirm it.
func (w *statementWorld) shipped(id, quantity string, d fulapi.DeliveryLineRef) {
	w.t.Helper()
	r, err := w.ship(w.c.FUL, w.merch, id, quantity, d)
	w.must(err)
	w.grantedOn(w.c.FUL, id, fulapi.ScopeShipment)
	if _, err = w.c.FUL.ConfirmShipment(w.merch, w.confirmCmd(id, r.RowVersion)); err != nil {
		w.t.Fatalf("confirm %s: %v", id, err)
	}
}

func salesCmd(key, id string, lines ...string) stlapi.ConfirmSettlement {
	c := stlapi.ConfirmSettlement{IdempotencyKey: key, StatementID: id, StatementKind: stlapi.StatementSales}
	for _, s := range lines {
		c.Sources = append(c.Sources, stlapi.SettlementSourceRef{ShipmentID: s, LineID: "S1"})
	}
	return c
}

func purchaseCmd(key, id string, deliveries ...string) stlapi.ConfirmSettlement {
	c := stlapi.ConfirmSettlement{IdempotencyKey: key, StatementID: id, StatementKind: stlapi.StatementPurchase, EvidenceIDs: []string{"evidence"}}
	for _, d := range deliveries {
		c.Sources = append(c.Sources, stlapi.SettlementSourceRef{DeliveryID: d, LineID: "L1", Version: 1})
	}
	return c
}

func toStatement(kind, statement, stage string) stlapi.ApplicationTarget {
	return stlapi.ApplicationTarget{Kind: kind, StatementID: statement, StageID: stage}
}

func (w *statementWorld) apply(id, fund string, target stlapi.ApplicationTarget, amount string) (stlapi.ApplicationReceipt, error) {
	return w.c.STL.Apply(w.treasury, stlapi.Apply{IdempotencyKey: w.key(), ApplicationID: id, FundRecordID: fund, Target: target, Amount: amount})
}

func (w *statementWorld) exposure() string {
	w.t.Helper()
	v, err := w.c.STL.GetCustomerCredit(w.treasury, "customer.1")
	w.must(err)
	return v.ExposureCurrency + " " + v.ExposureMinor
}

func installmentsOf(v stlapi.BalanceView) [][4]string {
	var out [][4]string
	for _, i := range v.Installments {
		out = append(out, [4]string{i.StageID, i.AmountMinor, i.AppliedMinor, i.OpenMinor})
	}
	return out
}

// BRC-001 (S5 plan §5 D1 steps 7, 19–21) on TRD's, PUR's, QLT's and FUL's real records:
//
//   - The customer's 288.00 is applied to P1 before shipment; the 120 PCS ship from the verified
//     delivery. The credit exposure is the 672.00 reservation (STL-INV-004).
//   - settlement.confirm of the shipment line makes the receivable 960.00 (STL-INV-001 source:
//     FUL's confirmed line at the order line's price): P1 288.00 due when the goods left, P2 672.00 30
//     days later; P1's application moves to the receivable (moved, replaced on the receivable), so
//     the receivable is open 672.00 while P1 still reads paid: PaymentConditionGuard v1 is satisfied
//     for shipment and documents (D-4). The exposure stays 672.00 — the reservation and the
//     receivable that replaced it are not counted twice (D-4). The line settled again is
//     SETTLEMENT_SOURCE_DUPLICATE; another prepayment on P1, all of it settled, is
//     APPLICATION_EXCEEDS_OPEN.
//   - The customer remits 672.00, the bank credits 670.00: applying 672.00 is
//     FUND_DIFFERENCE_UNEXPLAINED until the credit manager resolves the 2.00 as our bank charge; then
//     the receivable is 0 and the exposure 0.00.
//   - The purchase statement of the verified delivery (120 at 5.00) makes the payable 600.00; our
//     payment of 600.00 to supplier.1 applied to it leaves 0. Settled again: duplicate.
//   - The order's settlement position and the SettlementConfirmed events say the same.
func TestStatementsOnRealShipmentsAndDeliveriesBRC001(t *testing.T) {
	w := newStatementWorld(t)
	order := w.order.ObjectID
	if v := w.paymentCondition(order, w.revision, stlapi.ScopeShipment); v.Satisfied() || v.StageID != "P1" {
		t.Fatalf("shipment scope with nothing paid: %+v", v)
	}
	w.prepaid()
	if v := w.paymentCondition(order, w.revision, stlapi.ScopeShipment); !v.Satisfied() || v.AppliedMinor != "28800" {
		t.Fatalf("shipment scope before shipment: %+v", v)
	}
	d1 := w.verified("delivery.1", "120", "120")
	w.shipped("shipment.1", "120", d1)
	if got := w.exposure(); got != "USD 67200" {
		t.Fatalf("exposure before the statement: %s", got)
	}

	r, err := w.c.STL.ConfirmSettlement(w.treasury, salesCmd(w.key(), "statement.sales", "shipment.1"))
	w.must(err)
	if r.Status != stlapi.StatementConfirmed || r.RowVersion != 1 || r.StatementKind != stlapi.StatementSales || r.CounterpartyID != "customer.1" ||
		r.OrderID != order || r.Currency != "USD" || r.AmountMinor != "96000" || r.OpenMinor != "67200" || r.Lines != 1 || r.Installments != 2 ||
		!slices.Equal(r.MovedApplications, []string{"app.p1"}) {
		t.Fatalf("sales statement %+v", r)
	}
	recv, err := w.c.STL.GetReceivable(w.treasury, "statement.sales")
	w.must(err)
	if got := installmentsOf(recv); len(got) != 2 || got[0] != [4]string{"P1", "28800", "28800", "0"} || got[1] != [4]string{"P2", "67200", "0", "67200"} ||
		recv.Kind != stlapi.TargetReceivable || recv.OpenMinor != "67200" || len(recv.Applications) != 1 || recv.Applications[0].MovedFrom != "app.p1" ||
		recv.Applications[0].FundRecordID != "fund.p1" || recv.Installments[1].DueAt.Sub(recv.Installments[0].DueAt) != 30*24*time.Hour {
		t.Fatalf("receivable %+v", recv)
	}
	if a, err := w.c.STL.GetApplication(w.treasury, "app.p1"); err != nil || a.Status != stlapi.ApplicationMoved || a.Version != 2 {
		t.Fatalf("moved application %+v %v", a, err)
	}
	for _, scope := range []string{stlapi.ScopeShipment, stlapi.ScopeDocuments} {
		if v := w.paymentCondition(order, w.revision, scope); !v.Satisfied() || v.AppliedMinor != "28800" {
			t.Fatalf("%s scope after the move: %+v", scope, v)
		}
	}
	acc, err := w.c.STL.GetOrderAccount(w.treasury, order)
	w.must(err)
	if acc.Stages[0].AppliedMinor != "28800" || acc.Stages[0].OpenMinor != "0" || acc.Conditions[0].Status != stlapi.PaymentSatisfied {
		t.Fatalf("order account after the move %+v", acc)
	}
	if got := w.exposure(); got != "USD 67200" {
		t.Fatalf("exposure after the statement (no double count): %s", got)
	}
	_, err = w.c.STL.ConfirmSettlement(w.treasury2, salesCmd(w.key(), "statement.again", "shipment.1"))
	wantRefused(t, "the shipment line settled again", err, stlapi.CodeSettlementSourceDuplicate, "/args/sources/0")
	w.fund("fund.more", stlapi.FundReceipt, "10.00", "")
	_, err = w.c.STL.Apply(w.treasury, stlapi.Apply{IdempotencyKey: w.key(), ApplicationID: "app.more", FundRecordID: "fund.more",
		Target: stlapi.ApplicationTarget{Kind: stlapi.TargetOrderStage, OrderID: order, StageID: "P1"}, Amount: "10.00"})
	wantRefused(t, "a prepayment on a settled stage", err, stlapi.CodeApplicationExceedsOpen, "/args/amount")

	w.fund("fund.670", stlapi.FundReceipt, "670.00", "672.00")
	_, err = w.apply("app.672", "fund.670", toStatement(stlapi.TargetReceivable, "statement.sales", "P2"), "672.00")
	wantRefused(t, "2.00 unexplained", err, stlapi.CodeFundDifferenceUnexplained, "/args/amount")
	_, err = w.c.STL.ResolveFundDifference(w.manager, stlapi.ResolveFundDifference{IdempotencyKey: w.key(), FundRecordID: "fund.670",
		ExpectedVersion: 1, Resolution: stlapi.ResolutionBankCharge, Amount: "2.00", EvidenceIDs: []string{"evidence"}, Reason: "中间行扣费，经批准由我方承担"})
	w.must(err)
	a, err := w.apply("app.672", "fund.670", toStatement(stlapi.TargetReceivable, "statement.sales", ""), "672.00")
	w.must(err)
	if a.TargetKind != stlapi.TargetReceivable || a.StatementID != "statement.sales" || a.InstallmentSeq != 2 || a.StageID != "P2" ||
		a.StatementOpenMinor != "0" || a.FundAvailableMinor != "0" {
		t.Fatalf("application to the receivable %+v", a)
	}
	if got := w.exposure(); got != "USD 0" {
		t.Fatalf("exposure once paid: %s", got)
	}

	p, err := w.c.STL.ConfirmSettlement(w.treasury, purchaseCmd(w.key(), "statement.purchase", "delivery.1"))
	w.must(err)
	if p.StatementKind != stlapi.StatementPurchase || p.CounterpartyID != "supplier.1" || p.AmountMinor != "60000" || p.OpenMinor != "60000" ||
		p.OrderID != "" || len(p.MovedApplications) != 0 {
		t.Fatalf("purchase statement %+v", p)
	}
	_, err = w.c.STL.ConfirmSettlement(w.treasury, purchaseCmd(w.key(), "statement.purchase2", "delivery.1"))
	wantRefused(t, "the delivery line settled again", err, stlapi.CodeSettlementSourceDuplicate, "/args/sources/0")
	w.fund("fund.pay", stlapi.FundPayment, "600.00", "")
	_, err = w.apply("app.pay", "fund.p1", toStatement(stlapi.TargetPayable, "statement.purchase", ""), "1.00")
	wantRefused(t, "a receipt on a payable", err, kernel.CodeInvalidState, "/args/fund_record_id")
	if a, err = w.apply("app.pay", "fund.pay", toStatement(stlapi.TargetPayable, "statement.purchase", ""), "600.00"); err != nil ||
		a.StatementOpenMinor != "0" {
		t.Fatalf("payment applied to the payable %+v %v", a, err)
	}
	st, err := w.c.STL.GetStatement(w.treasury, "statement.purchase")
	w.must(err)
	if l := st.Lines[0]; len(st.Lines) != 1 || l.SourceKind != stlapi.SourceDeliveryLine || l.SourceID != "delivery.1" || l.CommitmentID != "commitment.1" ||
		l.Quantity != "120" || l.BasisNetMinor != "60000" || l.AmountMinor != "60000" || st.OpenMinor != "0" || st.EvidenceIDs[0] != "evidence" {
		t.Fatalf("purchase statement view %+v", st)
	}

	pos, err := w.c.STL.GetOrderSettlement(w.treasury, order)
	w.must(err)
	if pos.SettledMinor != "96000" || pos.AppliedMinor != "96000" || pos.OpenMinor != "0" || pos.OverdueMinor != "0" || pos.PrepaidMinor != "0" ||
		len(pos.Stages) != 2 || pos.Stages[0].SettledMinor != "28800" || pos.Stages[0].PaidMinor != "28800" || pos.Stages[1].SettledMinor != "67200" ||
		pos.Stages[1].PaidMinor != "67200" || pos.Stages[1].UnsettledMinor != "0" || len(pos.Receivables) != 1 || len(pos.PurchaseLines) != 1 ||
		pos.PurchaseLines[0].AmountMinor != "60000" || len(pos.Payables) != 1 || pos.Payables[0].OpenMinor != "0" {
		t.Fatalf("settlement position %+v", pos)
	}
	var confirmed []map[string]any
	for _, e := range w.events("company.A", stlapi.Source) {
		if e.Type == stlapi.EventSettlementConfirmed {
			confirmed = append(confirmed, e.Payload)
		}
	}
	if len(confirmed) != 2 || confirmed[0]["statement_id"] != "statement.sales" || confirmed[0]["order_id"] != order ||
		len(confirmed[0]["moved"].([]any)) != 1 || confirmed[1]["kind"] != stlapi.StatementPurchase {
		t.Fatalf("SettlementConfirmed events %v", confirmed)
	}
}

// STL-INV-001's concurrency ("两张结算单并发引用同一出运行只有一个成功"): three shipments of 40 PCS from
// three verified deliveries of 40; for each shipment line and each delivery line two treasurers
// confirm a statement at once. One wins, the other is SETTLEMENT_SOURCE_DUPLICATE — sales statements
// serialise on the order account, purchase statements on the source index — and every source line
// stands settled once. The three receivables fill BRC-001's plan in order: 288.00 + 32.00 (P1's
// prepayment moves to the first), 320.00, 320.00.
func TestConcurrentStatementsOfOneSourceLineOneWins(t *testing.T) {
	w := newStatementWorld(t)
	w.prepaid()
	for _, id := range []string{"a", "b", "c"} {
		w.shipped("shipment."+id, "40", w.verified("delivery."+id, "40", "40"))
	}
	race := func(cmds ...stlapi.ConfirmSettlement) (won string, errs []error) {
		var wg sync.WaitGroup
		errs = make([]error, len(cmds))
		for i, ctx := range []context.Context{w.treasury, w.treasury2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = w.c.STL.ConfirmSettlement(ctx, cmds[i])
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err == nil {
				won = cmds[i].StatementID
			}
		}
		return won, errs
	}
	var won []string
	for _, id := range []string{"a", "b", "c"} {
		for _, pair := range [][2]stlapi.ConfirmSettlement{
			{salesCmd(w.key(), "sales."+id+".1", "shipment."+id), salesCmd(w.key(), "sales."+id+".2", "shipment."+id)},
			{purchaseCmd(w.key(), "purchase."+id+".1", "delivery."+id), purchaseCmd(w.key(), "purchase."+id+".2", "delivery."+id)},
		} {
			winner, errs := race(pair[0], pair[1])
			if (errs[0] == nil) == (errs[1] == nil) || (code(errs[0]) != stlapi.CodeSettlementSourceDuplicate &&
				code(errs[1]) != stlapi.CodeSettlementSourceDuplicate) {
				t.Fatalf("%s: one of two statements wins: %v, %v", pair[0].StatementID, errs[0], errs[1])
			}
			won = append(won, winner)
		}
	}
	if n := w.count("company.A", "brant_stl.settlement_line WHERE status = 'settled'"); n != 6 {
		t.Fatalf("settled lines %d", n)
	}
	var due []string
	for i := 0; i < len(won); i += 2 {
		r, err := w.c.STL.GetReceivable(w.treasury, won[i])
		w.must(err)
		for _, x := range r.Installments {
			due = append(due, x.StageID+":"+x.AmountMinor)
		}
		if p, err := w.c.STL.GetPayable(w.treasury, won[i+1]); err != nil || p.AmountMinor != "20000" {
			t.Fatalf("payable %+v %v", p, err)
		}
	}
	if !slices.Equal(due, []string{"P1:28800", "P2:3200", "P2:32000", "P2:32000"}) {
		t.Fatalf("installments of the three receivables %v", due)
	}
}

// S5-04b over HTTP with OIDC access tokens on the composed process (BRC-001 shape): the treasurer
// confirms the sales statement of FUL's real shipment line and the purchase statement of PUR's real
// verified delivery; an AI agent holding settlement.confirm is refused (humanOnlyCommands); the reads
// show the statement, the receivable with its installments, the payable and the order's settlement
// position; P1's 288.00 was prepaid and moved, the customer's 672.00 is applied to the receivable's
// P2 over HTTP and reconciles: the receivable is 0; the same shipment line settled again is 409
// SETTLEMENT_SOURCE_DUPLICATE.
func TestSettlementStatementsOverHTTP(t *testing.T) {
	w := newStatementWorld(t)
	order := w.order.ObjectID
	w.prepaid()
	w.shipped("shipment.1", "120", w.verified("delivery.1", "120", "120"))
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.settle"},
		Permissions: []string{"settlement.confirm", "stl.read"}, DataScope: "company"})
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	treasurer := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "treasury.1", "human"), n: 600}
	agent := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "agent.settle", "ai"), n: 700}

	sales := map[string]any{"statement_id": "statement.http", "kind": "sales", "sources": []any{map[string]any{"shipment_id": "shipment.1",
		"line_id": "S1"}}}
	if status, out := agent.try("stl", stlapi.Contract, stlapi.CommandConfirmSettlement, sales); status != http.StatusForbidden ||
		out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI agent: %d %v", status, out)
	}
	if r := treasurer.command("stl", stlapi.Contract, stlapi.CommandConfirmSettlement, sales); r["object_id"] != "statement.http" ||
		r["status"] != "confirmed" || r["row_version"] != float64(1) {
		t.Fatalf("sales statement %v", r)
	}
	again := map[string]any{"statement_id": "statement.http2", "kind": "sales", "sources": sales["sources"]}
	if status, out := treasurer.try("stl", stlapi.Contract, stlapi.CommandConfirmSettlement, again); status != http.StatusConflict ||
		out["code"] != stlapi.CodeSettlementSourceDuplicate {
		t.Fatalf("settled again: %d %v", status, out)
	}
	purchase := map[string]any{"statement_id": "statement.http.p", "kind": "purchase", "sources": []any{map[string]any{"delivery_id": "delivery.1",
		"line_id": "L1", "version": 1}}, "evidence_ids": []any{"evidence"}}
	treasurer.command("stl", stlapi.Contract, stlapi.CommandConfirmSettlement, purchase)

	status, st := treasurer.do("GET", "/api/brant/v1/stl/settlement_statements/statement.http", nil)
	lines, _ := st["lines"].([]any)
	if status != http.StatusOK || st["kind"] != "sales" || st["amount_minor"] != "96000" || st["open_minor"] != "67200" || len(lines) != 1 ||
		lines[0].(map[string]any)["source_id"] != "shipment.1" {
		t.Fatalf("statement %d %v", status, st)
	}
	status, recv := treasurer.do("GET", "/api/brant/v1/stl/receivables/statement.http", nil)
	inst, _ := recv["installments"].([]any)
	if status != http.StatusOK || recv["kind"] != "receivable" || len(inst) != 2 || inst[1].(map[string]any)["stage_id"] != "P2" ||
		inst[1].(map[string]any)["open_minor"] != "67200" {
		t.Fatalf("receivable %d %v", status, recv)
	}
	if status, out := treasurer.do("GET", "/api/brant/v1/stl/payables/statement.http", nil); status != http.StatusNotFound {
		t.Fatalf("a receivable read as a payable: %d %v", status, out)
	}
	status, pay := treasurer.do("GET", "/api/brant/v1/stl/payables/statement.http.p", nil)
	if status != http.StatusOK || pay["kind"] != "payable" || pay["amount_minor"] != "60000" || pay["counterparty_id"] != "supplier.1" {
		t.Fatalf("payable %d %v", status, pay)
	}

	w.fund("fund.672", stlapi.FundReceipt, "672.00", "")
	apply := map[string]any{"contract": stlapi.Contract, "command": stlapi.CommandApply, "idempotency_key": "stl.http.receivable",
		"args": map[string]any{"application_id": "app.http.r", "fund_record_id": "fund.672",
			"target": map[string]any{"kind": "receivable", "statement_id": "statement.http", "stage_id": "P2"}, "amount": "672.00"}}
	status, res := treasurer.do("POST", "/api/brant/v1/stl/commands/application.apply", apply)
	if status != http.StatusOK || res["object_id"] != "app.http.r" || res["status"] != "applied" {
		t.Fatalf("apply %d %v", status, res)
	}
	status, rec := treasurer.do("POST", "/api/brant/v1/stl/commands/application.apply/reconcile", apply)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != res["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	status, pos := treasurer.do("GET", "/api/brant/v1/stl/orders/"+order+"/settlement", nil)
	stages, _ := pos["stages"].([]any)
	if status != http.StatusOK || pos["settled_minor"] != "96000" || pos["open_minor"] != "0" || len(stages) != 2 ||
		stages[0].(map[string]any)["paid_minor"] != "28800" || stages[1].(map[string]any)["paid_minor"] != "67200" {
		t.Fatalf("settlement position %d %v", status, pos)
	}
	if status, app := treasurer.do("GET", "/api/brant/v1/stl/applications/app.http.r", nil); status != http.StatusOK ||
		app["target_kind"] != "receivable" || app["statement_id"] != "statement.http" || app["installment_seq"] != float64(2) {
		t.Fatalf("application %d %v", status, app)
	}
}
