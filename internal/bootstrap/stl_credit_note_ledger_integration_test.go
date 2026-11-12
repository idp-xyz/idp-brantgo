//go:build integration

package bootstrap_test

import (
	"context"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Credit notes, settlement reversal and cross-currency applications on stand-ins for TRD and FUL (S5 unit C8,
// WBS S5-18; tactical model STL-AGG-SettlementStatement entities CreditNote and SettlementReversal,
// STL-AGG-Application; SM-STL-CREDIT-NOTE, SM-STL-SETTLEMENT; STL-INV-007, STL-INV-008, STL-INV-009; UL-INV-22;
// Context Map R33 (POL fx_rate of purpose settlement, ConfigGuard v1); MD-10; baseline 4.5, 4.6 S-A, 4.7 S-B) on
// PostgreSQL 16. The ledger world (stl_settlement_ledger_integration_test.go) steers STL's ports Orders and
// Shipments, so an order carries S-A's and S-B's exact plans and TRD's analysis rate. Python V0.50 has no STL.
//
// People: treasury.1 confirms statements and applies (treasury); manager.1 confirms credit notes and reverses
// (credit_manager); treasury.2 holds both templates; sales.1 requests credit notes (sales_clerk);
// clerk.manager is a person granted both credit_note.request and credit.confirm (the role templates forbid
// sales_clerk with credit_manager, so only a direct grant can make a requester its own confirmer).

func (fw *fundWorld) requestCredit(ctx context.Context, id, statement, order, kind, amount string) (stlapi.CreditNoteReceipt, error) {
	return fw.stl.RequestCreditNote(ctx, stlapi.RequestCreditNote{IdempotencyKey: fw.key(), CreditNoteID: id, CreditKind: kind, OrderID: order,
		StatementID: statement, Amount: amount, Basis: "合成：到港后色差投诉，协商折让", EvidenceIDs: []string{fw.advice}})
}

func (fw *fundWorld) decideCredit(ctx context.Context, id string, expected int64, decision string) (stlapi.CreditNoteReceipt, error) {
	return fw.stl.ConfirmCredit(ctx, stlapi.ConfirmCredit{IdempotencyKey: fw.key(), CreditNoteID: id, ExpectedVersion: expected,
		Decision: decision, Reason: "信用经理核准：依据客户索赔与验货报告"})
}

func (fw *fundWorld) reverse(ctx context.Context, statement string, expected int64) (stlapi.ReversalReceipt, error) {
	return fw.stl.ReverseSettlement(ctx, stlapi.ReverseSettlement{IdempotencyKey: fw.key(), StatementID: statement, ExpectedVersion: expected,
		Reason: "结算单价格错误，冲销后按正确价格重新结算"})
}

func (fw *fundWorld) applyCredit(id, note, statement, amount string) (stlapi.ApplicationReceipt, error) {
	return fw.stl.Apply(fw.treasury, stlapi.Apply{IdempotencyKey: fw.key(), ApplicationID: id, CreditNoteID: note,
		Target: toStatement(stlapi.TargetReceivable, statement, ""), Amount: amount})
}

func (fw *fundWorld) position(order string) stlapi.OrderSettlementView {
	fw.t.Helper()
	v, err := fw.stl.GetOrderSettlement(fw.treasury, order)
	fw.must(err)
	return v
}

// Baseline 4.7 S-B and STL-INV-008 on PG16: 100 PCS at USD 10.00 are settled at 1,000.00. The sales clerk
// requests a credit note of 100.00 for the 10 PCS returned (kind return; above the receivable it is
// APPLICATION_EXCEEDS_OPEN); a person who requested a credit note may not confirm it
// (INDEPENDENT_REVIEW_REQUIRED); the credit manager confirms: net sales are 1,000.00 − 100.00 = 900.00
// (baseline 4.5, the order's settlement position) while the statement still settles 100 PCS — the credit
// note writes no quantity. Applied to the receivable (a credit note is an application source, not cash),
// the receivable is open 900.00 (the credit exposure counts open receivables: the real-world test shows
// it); the note's rest is 0 (APPLICATION_EXCEEDS_AVAILABLE). Each request and decision moves the statement's version; the
// confirmation publishes CreditConfirmed.
func TestBaseline47SBCreditNote(t *testing.T) {
	fw, ful := newLedgerWorld(t)
	sales := fw.assign("sales.1", "sales_clerk")
	self := fw.provision(bootstrap.Grant{Scope: fw.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk.manager"},
		Permissions: []string{"credit_note.request", "credit.confirm", "stl.read", "evd.read"}, DataScope: "company"})
	o := fw.activeOrder("order.sb", "customer.1", fw.usd, stl.OrderStage{StageID: "P1", Trigger: "after_shipment", Days: 30, Percent: "100",
		AmountMinor: "100000"})
	ful.shipped(o, "shipment.sb", "100", "100000", time.Now().Add(-time.Hour))
	if r := fw.settle("statement.sb", "shipment.sb"); r.AmountMinor != "100000" || r.OpenMinor != "100000" {
		t.Fatalf("statement %+v", r)
	}

	_, err := fw.requestCredit(sales, "cn.big", "statement.sb", "order.sb", stlapi.CreditReturn, "1000.01")
	wantRefused(t, "a credit note above the receivable", err, stlapi.CodeApplicationExceedsOpen, "/args/amount")
	_, err = fw.requestCredit(sales, "cn.x", "statement.sb", "order.other", stlapi.CreditReturn, "1.00")
	wantRefused(t, "a credit note of another order", err, kernel.CodeInvalidKnownValue, "/args/order_id")
	r, err := fw.requestCredit(sales, "cn.sb", "statement.sb", "order.sb", stlapi.CreditReturn, "100.00")
	fw.must(err)
	if r.Status != stlapi.CreditRequested || r.RowVersion != 1 || r.StatementVersion != 2 || r.AmountMinor != "10000" || r.CreditKind != stlapi.CreditReturn ||
		r.StatementOpenMinor != "100000" {
		t.Fatalf("requested %+v", r)
	}
	s, err := fw.requestCredit(self, "cn.self", "statement.sb", "order.sb", stlapi.CreditClaim, "1.00")
	fw.must(err)
	_, err = fw.decideCredit(self, "cn.self", s.RowVersion, stlapi.DecisionConfirm)
	wantRefused(t, "the requester confirms its own credit note", err, stlapi.CodeIndependentReviewRequired, "/args/credit_note_id")
	_, err = fw.decideCredit(sales, "cn.sb", 1, stlapi.DecisionConfirm)
	fw.want(err, kernel.CodePermissionDenied) // the sales clerk does not confirm credit (exclusive to credit_manager)
	c, err := fw.decideCredit(fw.manager, "cn.sb", 1, stlapi.DecisionConfirm)
	fw.must(err)
	if c.Status != stlapi.CreditConfirmed || c.RowVersion != 2 || c.StatementVersion != 4 {
		t.Fatalf("confirmed %+v", c)
	}
	if _, err = fw.decideCredit(fw.manager, "cn.self", 1, stlapi.DecisionReject); err != nil {
		t.Fatal(err)
	}

	pos := fw.position("order.sb")
	if pos.SettledMinor != "100000" || pos.CreditedMinor != "10000" || pos.NetSalesMinor != "90000" || pos.OpenMinor != "100000" {
		t.Fatalf("S-B position before the application %+v", pos)
	}
	a, err := fw.applyCredit("app.cn.sb", "cn.sb", "statement.sb", "100.00")
	fw.must(err)
	if a.CreditNoteID != "cn.sb" || a.FundRecordID != "" || a.StatementOpenMinor != "90000" || a.StageID != "P1" || a.AmountMinor != "10000" {
		t.Fatalf("credit note applied %+v", a)
	}
	_, err = fw.applyCredit("app.cn.more", "cn.sb", "statement.sb", "0.01")
	wantRefused(t, "the credit note applied twice", err, stlapi.CodeApplicationExceedsAvailable, "/args/amount")
	_, err = fw.applyCredit("app.cn.self", "cn.self", "statement.sb", "1.00")
	wantRefused(t, "a rejected credit note applied", err, kernel.CodeInvalidState, "/args/credit_note_id")

	st, err := fw.stl.GetStatement(fw.treasury, "statement.sb")
	fw.must(err)
	if st.Version != 5 || st.Status != stlapi.StatementConfirmed || st.Lines[0].Quantity != "100" || st.Lines[0].Status != "settled" ||
		st.OpenMinor != "90000" || st.CreditedMinor != "10000" || len(st.CreditNotes) != 2 || st.Reversal != nil {
		t.Fatalf("statement after S-B %+v", st)
	}
	n, err := fw.stl.GetCreditNote(fw.manager, "cn.sb")
	fw.must(err)
	if n.Status != stlapi.CreditConfirmed || !n.Effective || n.AppliedMinor != "10000" || n.RestMinor != "0" || n.DecidedBy.ID != "manager.1" ||
		n.RequestedBy.ID != "sales.1" || len(n.Applications) != 1 || n.Applications[0].CreditNoteID != "cn.sb" {
		t.Fatalf("credit note %+v", n)
	}
	pos = fw.position("order.sb")
	if pos.NetSalesMinor != "90000" || pos.OpenMinor != "90000" || pos.AppliedMinor != "10000" {
		t.Fatalf("S-B position %+v", pos)
	}
	var confirmed []map[string]any
	for _, e := range fw.events("company.A", stlapi.Source) {
		if e.Type == stlapi.EventCreditConfirmed {
			confirmed = append(confirmed, e.Payload)
		}
	}
	if len(confirmed) != 1 || confirmed[0]["credit_note_id"] != "cn.sb" || confirmed[0]["statement_id"] != "statement.sb" ||
		confirmed[0]["statement_version"] != float64(4) || confirmed[0]["aggregate_type"] != "SettlementStatement" {
		t.Fatalf("CreditConfirmed %v", confirmed)
	}
	// Unapplied, the receivable is open again and the note's rest back; the credit stays confirmed.
	_, err = fw.unapply("app.cn.sb", 1)
	fw.must(err)
	if n, err = fw.stl.GetCreditNote(fw.manager, "cn.sb"); err != nil || n.RestMinor != "10000" || fw.position("order.sb").OpenMinor != "100000" {
		t.Fatalf("after the unapply %+v %v", n, err)
	}
}

// fxRate publishes a POL fx_rate converting from into to for purpose.
func (fw *fundWorld) fxRate(id, from, to, rate, purpose string) polapi.ConfigRef {
	win := newWindow()
	return fw.configure(fw.editor, fw.reviewer, polapi.KindFXRate, id, 0, win.from, win.until, polapi.ConfigData{FXRate: &polapi.FXRate{
		FromCurrency: from, ToCurrency: to, Rate: rate, Purpose: purpose, SourceName: "SYNTHETIC SETTLEMENT RATE", ObservedAt: kernel.FormatTimestamp(win.from)}}, true)
}

// cnyReceipt records CNY received from customer.1 on the company's CNY account.
func (fw *fundWorld) cnyReceipt(id, received string) {
	fw.t.Helper()
	_, err := fw.stl.RecordFund(fw.treasury, stlapi.RecordFund{IdempotencyKey: fw.key(), FundRecordID: id, FundKind: stlapi.FundReceipt,
		CounterpartyID: "customer.1", Currency: stlRef(fw.cny), BankAccount: stlRef(fw.bankCNY), ReceivedAmount: received,
		ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "cny." + id, EvidenceIDs: []string{fw.advice}})
	fw.must(err)
}

func (fw *fundWorld) applyAt(id, fund string, target stlapi.ApplicationTarget, amount string, rate *polapi.ConfigRef) (stlapi.ApplicationReceipt, error) {
	cmd := stlapi.Apply{IdempotencyKey: fw.key(), ApplicationID: id, FundRecordID: fund, Target: target, Amount: amount}
	if rate != nil {
		r := stlRef(*rate)
		cmd.FXRate = &r
	}
	return fw.stl.Apply(fw.treasury, cmd)
}

// sum returns a numeric aggregate of brant_stl rows in company.A, read as the application role through RLS.
func (fw *fundWorld) sum(query string, args ...any) string {
	fw.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(fw.t))
	fw.must(err)
	var out string
	fw.must(bento.ScopeTransactor{DB: db, Scope: fw.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return exec.QueryRow(ctx, query, args...).Scan(&out)
	}))
	return out
}

// STL-INV-007 on baseline 4.6 S-A (sale USD 1,000.00, 20 % before and 80 % after shipment, analysis rate 7.0
// CNY per USD): the customer pays the 800.00 still open on the two receivables (400.00 + 400.00) in CNY —
// 5,500.00 on the CNY account. Applying it needs a POL fx_rate of purpose settlement from CNY to USD: without
// one, with the credit rate, with the USD→CNY settlement rate (no inverse) or with a rate for a USD fund it
// is FX_SCOPE_MISMATCH at /args/fx_rate. At 0.145454545455 USD per CNY, CNY 2,750.00 pays USD 400.00 of each
// receivable (HALF_UP at the cent), the fund's 5,500.00 are taken in CNY, and each application records the
// order's analysis rate with the business settlement FX difference: 2,750.00 − 400.00 × 7.0 = −50.00 CNY;
// both −100.00 CNY, S-A's "相对订单分析收入 CNY 7,000 单列差额 -100" — not a receipt, not revenue. Unapplied, the
// CNY returns to the fund and the receivable reopens in USD.
func TestCrossCurrencyApplicationAtTheSettlementRateS_A(t *testing.T) {
	fw, ful := newLedgerWorld(t)
	settlementFX := fw.fxRate("cny.usd.settlement", "CNY", "USD", "0.145454545455", polapi.PurposeSettlement)
	creditFX := fw.fxRate("cny.usd.credit", "CNY", "USD", "0.145454545455", polapi.PurposeCredit)
	inverseFX := fw.fxRate("usd.cny.settlement", "USD", "CNY", "6.875", polapi.PurposeSettlement)
	o := fw.activeOrder("order.sa", "customer.1", fw.usd, payStage("P1", "order_confirmed", "20000"),
		stl.OrderStage{StageID: "P2", Trigger: "after_shipment", Days: 30, Percent: "80", AmountMinor: "80000"})
	o.Analysis = &stl.OrderAnalysis{OrderCurrency: "USD", Currency: "CNY", Rate: "7.0"}
	fw.orders.put(o)
	fw.receipt("fund.pre", "200.00", "")
	_, err := fw.apply("app.pre", "fund.pre", "order.sa", "P1", "200.00")
	fw.must(err)
	ful.shipped(o, "shipment.60", "60", "60000", time.Now().Add(-48*time.Hour))
	ful.shipped(o, "shipment.40", "40", "40000", time.Now().Add(-24*time.Hour))
	fw.settle("statement.600", "shipment.60")
	fw.settle("statement.400", "shipment.40")
	fw.cnyReceipt("fund.cny", "5500.00")
	fw.receipt("fund.usd", "10.00", "")

	to600 := toStatement(stlapi.TargetReceivable, "statement.600", "P2")
	for name, c := range map[string]struct {
		fund string
		rate *polapi.ConfigRef
	}{
		"no settlement rate":       {"fund.cny", nil},
		"a credit rate":            {"fund.cny", &creditFX},
		"the inverse rate":         {"fund.cny", &inverseFX},
		"a rate for a USD receipt": {"fund.usd", &settlementFX},
	} {
		amount := "2750.00"
		if c.fund == "fund.usd" {
			amount = "1.00"
		}
		_, err = fw.applyAt("app.no", c.fund, to600, amount, c.rate)
		wantRefused(t, name, err, stlapi.CodeFXScopeMismatch, "/args/fx_rate")
	}
	for i, x := range []struct{ id, statement string }{{"app.cny.600", "statement.600"}, {"app.cny.400", "statement.400"}} {
		a, err := fw.applyAt(x.id, "fund.cny", toStatement(stlapi.TargetReceivable, x.statement, "P2"), "2750.00", &settlementFX)
		fw.must(err)
		cv := a.Conversion
		if a.Currency != "USD" || a.AmountMinor != "40000" || a.StatementOpenMinor != "0" || a.FundAvailableMinor != []string{"275000", "0"}[i] ||
			cv == nil || cv.SourceCurrency != "CNY" || cv.SourceAmountMinor != "275000" || cv.FXRate.ConfigID != "cny.usd.settlement" ||
			cv.Rate != "0.145454545455" || cv.AnalysisCurrency != "CNY" || cv.AnalysisRate != "7" || cv.FXDifferenceMinor != "-5000" {
			t.Fatalf("%s %+v %+v", x.id, a, cv)
		}
	}
	pos := fw.position("order.sa")
	if pos.OpenMinor != "0" || pos.AppliedMinor != "100000" || len(pos.FXDifferences) != 2 || pos.FXDifferences[0].FXDifferenceMinor != "-5000" ||
		pos.FXDifferences[1].AmountMinor != "40000" || pos.FXDifferences[1].SourceAmountMinor != "275000" {
		t.Fatalf("S-A position %+v", pos)
	}
	if got := fw.sum(`SELECT sum(fx_difference_minor)::text FROM brant_stl.application WHERE order_id = 'order.sa' AND status = 'applied'`); got != "-10000" {
		t.Fatalf("S-A business settlement FX difference %s CNY minor, want -10000 (−100.00)", got)
	}
	ref, err := fw.stl.ResolveFundRecords(fw.treasury, []string{"fund.cny"})
	fw.must(err)
	if f := ref["fund.cny"]; f.Currency != "CNY" || f.AppliedMinor != "550000" || f.AvailableMinor != "0" || f.CashMinor != "550000" ||
		len(f.Allocations) != 2 || f.Allocations[0].Conversion == nil || f.Allocations[0].AmountMinor != "40000" {
		t.Fatalf("CNY receipt %+v", f)
	}
	if v := fw.paymentCondition("order.sa", 1, stlapi.ScopeShipment); !v.Satisfied() {
		t.Fatalf("shipment scope %+v", v)
	}
	_, err = fw.unapply("app.cny.400", 1)
	fw.must(err)
	ref, err = fw.stl.ResolveFundRecords(fw.treasury, []string{"fund.cny"})
	fw.must(err)
	if ref["fund.cny"].AvailableMinor != "275000" || fw.receivable("statement.400").OpenMinor != "40000" {
		t.Fatalf("after the unapply %+v", ref["fund.cny"])
	}
	if a, err := fw.stl.GetApplication(fw.treasury, "app.cny.400"); err != nil || a.Status != stlapi.ApplicationUnapplied || a.Conversion == nil ||
		a.Conversion.FXDifferenceMinor != "-5000" {
		t.Fatalf("unapplied conversion %+v %v", a, err)
	}
}

// D-4 with a cross-currency prepayment: CNY 3,437.50 prepays the 500.00 deposit of a USD 1,000.00 order at
// the settlement rate (3,437.50 × 0.145454545455 = 500.00; difference 3,437.50 − 500.00 × 7.0 = −62.50 CNY).
// A first statement of 300.00 moves 300.00 of it: the replacement takes CNY 2,062.50 and −37.50, the
// remainder on the stage CNY 1,375.00 and −25.00 — they add up to the original exactly (the deferred check
// of 000005); the second statement moves the remainder whole. The CNY fund stays fully applied throughout.
func TestCrossCurrencyPrepaymentMovesInProportion(t *testing.T) {
	fw, ful := newLedgerWorld(t)
	settlementFX := fw.fxRate("cny.usd.settlement", "CNY", "USD", "0.145454545455", polapi.PurposeSettlement)
	o := fw.activeOrder("order.pm", "customer.1", fw.usd, payStage("P1", "order_confirmed", "50000"),
		stl.OrderStage{StageID: "P2", Trigger: "after_shipment", Days: 30, Percent: "50", AmountMinor: "50000"})
	o.Analysis = &stl.OrderAnalysis{OrderCurrency: "USD", Currency: "CNY", Rate: "7.0"}
	fw.orders.put(o)
	fw.cnyReceipt("fund.cny", "3437.50")
	a, err := fw.applyAt("app.dep", "fund.cny", stlapi.ApplicationTarget{Kind: stlapi.TargetOrderStage, OrderID: "order.pm", StageID: "P1"},
		"3437.50", &settlementFX)
	fw.must(err)
	if a.AmountMinor != "50000" || a.StageOpenMinor != "0" || a.Conversion.FXDifferenceMinor != "-6250" || a.FundAvailableMinor != "0" {
		t.Fatalf("prepayment %+v %+v", a, a.Conversion)
	}
	ful.shipped(o, "shipment.30", "30", "30000", time.Now().Add(-2*time.Hour))
	ful.shipped(o, "shipment.70", "70", "70000", time.Now().Add(-time.Hour))
	fw.settle("statement.300", "shipment.30")
	acc, err := fw.stl.GetOrderAccount(fw.treasury, "order.pm")
	fw.must(err)
	got := map[string][3]string{}
	for _, x := range acc.Applications {
		if x.MovedFrom == "app.dep" && x.Conversion != nil {
			got[x.TargetKind] = [3]string{x.AmountMinor, x.Conversion.SourceAmountMinor, x.Conversion.FXDifferenceMinor}
		}
	}
	if got[stlapi.TargetReceivable] != [3]string{"30000", "206250", "-3750"} || got[stlapi.TargetOrderStage] != [3]string{"20000", "137500", "-2500"} {
		t.Fatalf("moved in proportion %v", got)
	}
	fw.settle("statement.700", "shipment.70")
	if n := fw.sum(`SELECT count(*)::text FROM brant_stl.application WHERE moved_from IS NOT NULL AND source_amount_minor = 137500
		AND fx_difference_minor = -2500 AND target_kind = 'receivable' AND statement_id = 'statement.700'`); n != "1" {
		t.Fatalf("the remainder moved whole: %s", n)
	}
	if v := fw.sum(`SELECT sum(source_amount_minor)::text || '/' || sum(fx_difference_minor)::text FROM brant_stl.application
		WHERE fund_record_id = 'fund.cny' AND status = 'applied'`); v != "343750/-6250" {
		t.Fatalf("what stands on the CNY fund %s", v)
	}
}

// SM-STL-SETTLEMENT and STL-INV-009 on PG16: a statement confirmed by treasury.2 (treasury and credit
// manager) is not reversed by treasury.2 (INDEPENDENT_REVIEW_REQUIRED); while a payment stands on it the
// credit manager's reversal is SETTLEMENT_HAS_APPLICATIONS, while a credit note waits for its decision
// INVALID_STATE. Unapplied and decided, the statement is reversed: it stays with its lines (reversed) and
// the reversal record; nothing is open on it; the shipment line settles again on a new statement — once
// (SETTLEMENT_SOURCE_DUPLICATE); the order's position counts the new statement only; SettlementReversed names
// the freed source.
func TestReversalRestoresTheSourceOnce(t *testing.T) {
	fw, ful := newLedgerWorld(t)
	sales := fw.assign("sales.1", "sales_clerk")
	o := fw.activeOrder("order.rv", "customer.1", fw.usd, stl.OrderStage{StageID: "P1", Trigger: "after_shipment", Days: 30, Percent: "100",
		AmountMinor: "100000"})
	ful.shipped(o, "shipment.rv", "100", "100000", time.Now().Add(-time.Hour))
	_, err := fw.stl.ConfirmSettlement(fw.treasury2, salesCmd(fw.key(), "statement.wrong", "shipment.rv"))
	fw.must(err)
	_, err = fw.reverse(fw.treasury2, "statement.wrong", 1)
	wantRefused(t, "the confirmer reverses", err, stlapi.CodeIndependentReviewRequired, "/args/statement_id")
	fw.receipt("fund.rv", "100.00", "")
	_, err = fw.applyTo("app.rv", "fund.rv", "statement.wrong", "", "100.00")
	fw.must(err)
	_, err = fw.reverse(fw.manager, "statement.wrong", 1)
	wantRefused(t, "a reversal with a payment applied", err, stlapi.CodeSettlementHasApplications, "/args/statement_id")
	_, err = fw.requestCredit(sales, "cn.rv", "statement.wrong", "order.rv", stlapi.CreditPriceAllowance, "10.00")
	fw.must(err)
	_, err = fw.unapply("app.rv", 1)
	fw.must(err)
	_, err = fw.reverse(fw.manager, "statement.wrong", 2)
	wantRefused(t, "a reversal with a credit note pending", err, kernel.CodeInvalidState, "/args/statement_id")
	_, err = fw.decideCredit(fw.manager, "cn.rv", 1, stlapi.DecisionReject)
	fw.must(err)
	_, err = fw.reverse(fw.manager, "statement.wrong", 2)
	fw.want(err, kernel.CodeVersionConflict)
	r, err := fw.reverse(fw.manager, "statement.wrong", 3)
	fw.must(err)
	if r.Status != stlapi.StatementReversed || r.RowVersion != 4 || r.StatementKind != stlapi.StatementSales || len(r.Sources) != 1 ||
		r.Sources[0].ShipmentID != "shipment.rv" || r.OrderID != "order.rv" {
		t.Fatalf("reversal %+v", r)
	}
	_, err = fw.applyTo("app.late", "fund.rv", "statement.wrong", "", "1.00")
	wantRefused(t, "an application to a reversed statement", err, kernel.CodeInvalidState, "/args/target/statement_id")
	st, err := fw.stl.GetStatement(fw.manager, "statement.wrong")
	fw.must(err)
	if st.Status != stlapi.StatementReversed || st.Version != 4 || st.Lines[0].Status != stlapi.LineReversed || st.OpenMinor != "0" ||
		st.Reversal == nil || st.Reversal.ReversedBy.ID != "manager.1" || st.AmountMinor != "100000" {
		t.Fatalf("reversed statement %+v", st)
	}
	again, err := fw.stl.ConfirmSettlement(fw.treasury, salesCmd(fw.key(), "statement.right", "shipment.rv"))
	fw.must(err)
	if again.AmountMinor != "100000" || again.OpenMinor != "100000" {
		t.Fatalf("settled again %+v", again)
	}
	_, err = fw.stl.ConfirmSettlement(fw.treasury, salesCmd(fw.key(), "statement.twice", "shipment.rv"))
	wantRefused(t, "the line settled a third time", err, stlapi.CodeSettlementSourceDuplicate, "/args/sources/0")
	if pos := fw.position("order.rv"); pos.SettledMinor != "100000" || pos.OpenMinor != "100000" || len(pos.Receivables) != 2 {
		t.Fatalf("position after the reversal %+v", pos)
	}
	var reversed []map[string]any
	for _, e := range fw.events("company.A", stlapi.Source) {
		if e.Type == stlapi.EventSettlementReversed {
			reversed = append(reversed, e.Payload)
		}
	}
	if len(reversed) != 1 || reversed[0]["statement_id"] != "statement.wrong" || reversed[0]["statement_version"] != float64(4) ||
		len(reversed[0]["sources"].([]any)) != 1 {
		t.Fatalf("SettlementReversed %v", reversed)
	}
}

// STL-INV-009's concurrency ("冲销与核销并发按结算单根锁裁决"): in each of five rounds the credit manager reverses a
// statement while the treasurer applies a receipt to it. Both lock the statement root FOR UPDATE, so one
// commits first and the other sees it: either the reversal wins (the application is INVALID_STATE, the
// statement is reversed) or the application wins (the reversal is SETTLEMENT_HAS_APPLICATIONS). Never both.
func TestReversalAndApplicationOfOneStatementOneWins(t *testing.T) {
	fw, ful := newLedgerWorld(t)
	wins := map[string]int{}
	for i := range 5 {
		n := string(rune('a' + i))
		o := fw.activeOrder("order.c"+n, "customer.1", fw.usd, stl.OrderStage{StageID: "P1", Trigger: "after_shipment", Days: 30, Percent: "100",
			AmountMinor: "10000"})
		ful.shipped(o, "shipment.c"+n, "10", "10000", time.Now().Add(-time.Hour))
		fw.settle("statement.c"+n, "shipment.c"+n)
		fw.receipt("fund.c"+n, "100.00", "")
		var wg sync.WaitGroup
		var reverseErr, applyErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, reverseErr = fw.reverse(fw.manager, "statement.c"+n, 1) }()
		go func() {
			defer wg.Done()
			_, applyErr = fw.applyTo("app.c"+n, "fund.c"+n, "statement.c"+n, "", "100.00")
		}()
		wg.Wait()
		st, err := fw.stl.GetStatement(fw.treasury, "statement.c"+n)
		fw.must(err)
		switch {
		case reverseErr == nil && code(applyErr) == kernel.CodeInvalidState && st.Status == stlapi.StatementReversed && len(st.Applications) == 0:
			wins["reversal"]++
		case applyErr == nil && code(reverseErr) == stlapi.CodeSettlementHasApplications && st.Status == stlapi.StatementConfirmed && st.OpenMinor == "0":
			wins["application"]++
		default:
			t.Fatalf("round %s: reversal %v, application %v, statement %s open %s", n, reverseErr, applyErr, st.Status, st.OpenMinor)
		}
	}
	t.Logf("winners over five rounds: %v", wins)
}

// exec runs statements in one transaction of company.A as the application role (RLS) and returns the commit's
// error.
func (fw *fundWorld) exec(statements ...string) error {
	fw.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(fw.t))
	fw.must(err)
	return bento.ScopeTransactor{DB: db, Scope: fw.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		for _, s := range statements {
			if _, err = exec.Exec(ctx, s); err != nil {
				return err
			}
		}
		return nil
	})
}

// The database holds STL-INV-008 and STL-INV-009 whatever the code does (migrations/stl/000005), as the
// application role: a credit note decided by its requester, a decision that does not move the statement root, a
// statement reversed without its reversal record, a line reversed while its statement stands, a reversal with a
// payment standing or by the statement's confirmer, and a credit note applied beyond its amount are refused; the
// same reversal by another person with nothing standing commits.
func TestTheDatabaseHoldsCreditNotesAndReversals(t *testing.T) {
	fw, ful := newLedgerWorld(t)
	sales := fw.assign("sales.1", "sales_clerk")
	o := fw.activeOrder("order.db", "customer.1", fw.usd, stl.OrderStage{StageID: "P1", Trigger: "after_shipment", Days: 30, Percent: "100",
		AmountMinor: "10000"})
	ful.shipped(o, "shipment.db", "10", "10000", time.Now().Add(-time.Hour))
	fw.settle("statement.db", "shipment.db")
	for _, n := range []struct{ id, amount string }{{"cn.db", "60.00"}, {"cn.db2", "1.00"}} {
		_, err := fw.requestCredit(sales, n.id, "statement.db", "order.db", stlapi.CreditClaim, n.amount)
		fw.must(err)
	}
	_, err := fw.decideCredit(fw.manager, "cn.db", 1, stlapi.DecisionConfirm)
	fw.must(err)
	refused := func(name string, sql ...string) {
		t.Helper()
		if err := fw.exec(sql...); err == nil {
			t.Errorf("%s: committed", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
	decide := func(by string) string {
		return `UPDATE brant_stl.credit_note SET status = 'rejected', version = 2, decided_by_kind = 'human', decided_by_id = '` + by + `',
			decide_epoch = 1, decided_at = now(), decision_reason = 'no', updated_at = now() WHERE credit_note_id = 'cn.db2'`
	}
	const where = ` WHERE statement_id = 'statement.db'`
	bump := `UPDATE brant_stl.settlement_statement SET version = version + 1` + where
	refused("decided by its requester", decide("sales.1"), bump)
	refused("a decision without the root", decide("manager.1"))
	_, err = fw.decideCredit(fw.manager, "cn.db2", 1, stlapi.DecisionReject)
	fw.must(err)
	refused("a credit note applied beyond its amount", `INSERT INTO brant_stl.application (tenant_id, company_id, application_id, credit_note_id,
		target_kind, order_id, order_revision, stage_id, currency, minor_units, amount_minor, status, version, owner_actor_id, applied_by_kind,
		applied_by_id, apply_epoch, applied_at, updated_at, statement_id, installment_seq) SELECT tenant_id, company_id, 'app.db.over', 'cn.db',
		'receivable', order_id, order_revision, 'P1', currency, minor_units, 6001, 'applied', 1, 'treasury.1', 'human', 'treasury.1', 1, now(), now(),
		statement_id, 1 FROM brant_stl.settlement_statement`+where)
	reverse := func(by string) []string {
		return []string{`UPDATE brant_stl.settlement_statement SET status = 'reversed', version = version + 1` + where,
			`UPDATE brant_stl.settlement_line SET status = 'reversed'` + where,
			`INSERT INTO brant_stl.settlement_reversal (tenant_id, company_id, statement_id, reason, evidence_ids, reversed_by_kind, reversed_by_id,
				reverse_epoch, reversed_at) SELECT tenant_id, company_id, statement_id, 'x', '{}', 'human', ` + by + `, 1, now()
				FROM brant_stl.settlement_statement` + where}
	}
	refused("reversed without its record", `UPDATE brant_stl.settlement_statement SET status = 'reversed', version = version + 1`+where)
	refused("a line reversed alone", `UPDATE brant_stl.settlement_line SET status = 'reversed'`+where)
	fw.receipt("fund.db", "100.00", "")
	_, err = fw.applyTo("app.db", "fund.db", "statement.db", "", "10.00")
	fw.must(err)
	refused("reversed with a payment standing", reverse("'manager.1'")...)
	_, err = fw.unapply("app.db", 1)
	fw.must(err)
	refused("reversed by its confirmer", reverse("confirmed_by_id")...)
	if err = fw.exec(reverse("'manager.1'")...); err != nil {
		t.Fatalf("the same reversal by another person with nothing standing: %v", err)
	}
}
