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
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R unit R8 on PostgreSQL 16 (simulated sign-off E0 of 2026-11-13): S5R-D19 (the FX difference at the actual
// conversion rate, unrealised at the application's settlement rate; charge payments in net cash: S-A 2,250), S5R-D20
// (the invoice_reversed flag from CNX's PurchaseInvoiceRef v1; payable_adjustment.request / .confirm), S5R-D22
// (fund.reverse; credit notes on opening receivables), S5R-D24 (cost components and the Incoterm), S5R-D42 (the result of
// a cancelled order). Tactical model STL-INV-002, -008, -010, -011, -013, -014; SM-STL-FUND. Python V0.50 has no STL.

// cnxInvoices is the stand-in for CNX's PurchaseInvoiceRef v1 (R55): the invoices it shows.
type cnxInvoices struct {
	mu       sync.Mutex
	invoices []stl.PurchaseInvoice
}

func (c *cnxInvoices) ResolvePurchaseInvoices(_ context.Context, q stl.PurchaseInvoiceQuery) ([]stl.PurchaseInvoice, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []stl.PurchaseInvoice
	for _, p := range c.invoices {
		matched := false
		for _, m := range p.Matches {
			for _, id := range q.CommitmentIDs {
				matched = matched || m.CommitmentID == id
			}
		}
		for _, id := range q.InvoiceIDs {
			matched = matched || p.PurchaseInvoiceID == id
		}
		if matched || p.RedOf != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

func (c *cnxInvoices) set(invoices ...stl.PurchaseInvoice) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invoices = invoices
}

// r8World is the result world (stand-ins for TRD, FUL, PUR) with a second STL module on the same database that reads
// CNX's purchase invoices through the stand-in (the first reads none: CNX not wired); a payment account, the
// forwarder's freight and the people of R8: manager.1 (credit_manager: payable_adjustment.confirm, fund.reverse).
type r8World struct {
	*resultWorld
	invoices *cnxInvoices
	withCNX  *stl.Module
}

func newR8World(t *testing.T) *r8World {
	t.Helper()
	w := &r8World{resultWorld: newResultWorld(t), invoices: &cnxInvoices{}}
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	w.withCNX, err = stl.New(db, stl.Upstream{Orders: w.orders, Configs: w.c.POL, Parties: w.c.PTY, Sources: w.c.EVD,
		Shipments: resultShipments{w.ful}, Deliveries: w.pur, Invoices: w.invoices})
	w.must(err)
	return w
}

// usdReceipt records USD received from customer.1 on the company's USD account.
func (w *r8World) usdReceipt(id, received string) {
	w.t.Helper()
	_, err := w.stl.RecordFund(w.treasury, w.receiptCmd(id, received))
	w.must(err)
}

// convert records the bank's sale of sold USD of fund at rate into CNY on the company's CNY account (an fx_conversion).
func (w *r8World) convert(id, fund, sold, rate, received string) {
	w.t.Helper()
	_, err := w.stl.RecordFund(w.treasury, stlapi.RecordFund{IdempotencyKey: w.key(), FundRecordID: id, FundKind: stlapi.FundFXConversion,
		SourceFundRecordID: fund, Currency: stlRef(w.cny), BankAccount: stlRef(w.bankCNY), ReceivedAmount: received,
		ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "fx." + id, EvidenceIDs: []string{w.advice},
		FX: &stlapi.FXSale{SoldAmount: sold, BankRate: rate}})
	w.must(err)
}

// payment records CNY paid to payee on the company's CNY payment account.
func (w *r8World) payment(id, payee, amount string) {
	w.t.Helper()
	_, err := w.stl.RecordFund(w.treasury, stlapi.RecordFund{IdempotencyKey: w.key(), FundRecordID: id, FundKind: stlapi.FundPayment,
		CounterpartyID: payee, Currency: stlRef(w.cny), BankAccount: stlRef(w.bankPay), ReceivedAmount: amount,
		ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "pay." + id, EvidenceIDs: []string{w.advice}})
	w.must(err)
}

func (w *r8World) payCharge(ctx context.Context, id, fund, charge, amount string) (stlapi.ApplicationReceipt, error) {
	return w.stl.Apply(ctx, stlapi.Apply{IdempotencyKey: w.key(), ApplicationID: id, FundRecordID: fund,
		Target: stlapi.ApplicationTarget{Kind: stlapi.TargetCharge, ChargeID: charge}, Amount: amount})
}

// saUSDSettled is S-A as baseline 4.6 tells it: USD 200 received and converted at 7.0 (CNY 1,400), prepaying P1; the
// shipments of 60 and 40 settled at USD 600 + 400 (the prepayment moves); USD 800 received and converted at 6.875 (CNY
// 5,500), paying USD 400 on each receivable; the purchase statement of CNY 4,000 paid.
func (w *r8World) saUSDSettled(o stl.Order) {
	w.t.Helper()
	w.usdReceipt("fund.200", "200.00")
	w.convert("fx.200", "fund.200", "200.00", "7.0", "1400.00")
	_, err := w.apply("app.pre", "fund.200", "order.sa", "P1", "200.00")
	w.must(err)
	w.ful.shipped(o, "shipment.60", "60", "60000", time.Now().Add(-48*time.Hour))
	w.ful.shipped(o, "shipment.40", "40", "40000", time.Now().Add(-24*time.Hour))
	w.settle("statement.600", "shipment.60")
	w.settle("statement.400", "shipment.40")
	w.usdReceipt("fund.800", "800.00")
	for _, x := range []struct{ id, statement string }{{"app.600", "statement.600"}, {"app.400", "statement.400"}} {
		a, err := w.applyAt(x.id, "fund.800", toStatement(stlapi.TargetReceivable, x.statement, "P2"), "400.00", nil)
		w.must(err)
		if a.AmountMinor != "40000" || a.Conversion != nil {
			w.t.Fatalf("%s %+v", x.id, a)
		}
	}
	w.convert("fx.800", "fund.800", "800.00", "6.875", "5500.00")
	p, err := w.stl.ConfirmSettlement(w.treasury, stlapi.ConfirmSettlement{IdempotencyKey: w.key(), StatementID: "statement.purchase.sa",
		StatementKind: stlapi.StatementPurchase, Sources: []stlapi.SettlementSourceRef{{DeliveryID: "delivery.sa", LineID: "L1", Version: 1}}})
	w.must(err)
	if p.AmountMinor != "400000" {
		w.t.Fatalf("purchase statement %+v", p)
	}
	w.payment("fund.pay", "supplier.sa", "4000.00")
	_, err = w.stl.Apply(w.treasury, stlapi.Apply{IdempotencyKey: w.key(), ApplicationID: "app.pay", FundRecordID: "fund.pay",
		Target: toStatement(stlapi.TargetPayable, "statement.purchase.sa", ""), Amount: "4000.00"})
	w.must(err)
}

// S5R-D19 / STL-INV-011 positive ("S-A：一笔美元收款加一笔结汇得出 −100.00，净现金含费用付款得出 2,250"), baseline 4.6 exactly on
// PG16: the USD receipts are applied in USD and converted by the bank (7.0 and 6.875) — the business settlement FX
// difference is −100.00, realised by the conversions; the freight of CNY 650 is paid through STL to the forwarder
// (application target charge): net cash 6,900 − 4,000 − 650 = 2,250; confirmed 2,350. Before the freight is paid the
// result lists it unpaid (net cash 2,900). Refusals: another payee (PARTY_ENTITY_MISMATCH), more than is open
// (APPLICATION_EXCEEDS_OPEN), an agent (PERMISSION_DENIED, application.apply is human only); the closed snapshot holds
// the realised figures, and a conversion after the close changes no snapshot.
func TestS5RD19SAFromUSDReceiptsConversionsAndAChargePayment(t *testing.T) {
	w := newR8World(t)
	o := w.saOrder()
	w.saEstimate()
	w.saUSDSettled(o)
	w.saFreightBilled()
	v := w.result("order.sa")
	fx, cash := v.Live.FXDifference, v.Live.NetCash
	if fx.DifferenceMinor != "-10000" || fx.RealizedMinor != "-10000" || fx.UnrealizedMinor != "0" || len(fx.Items) != 3 || len(fx.Gaps) != 0 {
		t.Fatalf("FX %+v", fx)
	}
	for _, x := range fx.Items {
		if x.Basis != "conversion" || x.Unrealized || len(x.ConversionIDs) != 1 || x.SourceCurrency != "USD" || x.TargetCurrency != "CNY" {
			t.Fatalf("FX item %+v", x)
		}
	}
	if cash.ReceiptsMinor != "690000" || cash.PaymentsMinor != "400000" || cash.NetMinor != "290000" || cash.UnpaidChargesMinor != "65000" ||
		len(cash.Unpaid) != 1 || cash.Unpaid[0].ID != "freight.sa" {
		t.Fatalf("net cash before the freight is paid %+v", cash)
	}

	w.payment("pay.freight", "forwarder.1", "650.00")
	w.payment("pay.other", "supplier.sa", "650.00")
	_, err := w.payCharge(w.treasury, "app.other", "pay.other", "freight.sa", "650.00")
	wantRefused(t, "another payee", err, stlapi.CodePartyEntityMismatch, "/args/fund_record_id")
	_, err = w.payCharge(w.treasury, "app.freight", "pay.freight", "freight.sa", "650.01")
	wantRefused(t, "more than the payment", err, stlapi.CodeApplicationExceedsAvailable, "/args/amount")
	_, err = w.payCharge(w.agentOfFunds(), "app.ai", "pay.freight", "freight.sa", "1.00")
	if code(err) != kernel.CodePermissionDenied {
		t.Fatalf("an agent pays a charge: %v", err)
	}
	a, err := w.payCharge(w.treasury, "app.freight.1", "pay.freight", "freight.sa", "400.00")
	w.must(err)
	if a.TargetKind != stlapi.TargetCharge || a.ChargeID != "freight.sa" || a.ChargeOpenMinor != "25000" || a.FundAvailableMinor != "25000" {
		t.Fatalf("charge payment %+v", a)
	}
	_, err = w.payCharge(w.treasury, "app.freight.over", "pay.freight", "freight.sa", "250.01")
	wantRefused(t, "more than is open", err, stlapi.CodeApplicationExceedsAvailable, "/args/amount")
	w.payment("pay.freight.2", "forwarder.1", "300.00")
	_, err = w.payCharge(w.treasury, "app.freight.over2", "pay.freight.2", "freight.sa", "250.01")
	wantRefused(t, "more than is open on the charge", err, stlapi.CodeApplicationExceedsOpen, "/args/amount")
	_, err = w.payCharge(w.treasury, "app.freight.2", "pay.freight", "freight.sa", "250.00")
	w.must(err)
	ch, err := w.stl.GetCharge(w.treasury, "freight.sa")
	w.must(err)
	if ch.PaidMinor != "65000" || ch.OpenMinor != "0" || ch.PaymentStatus != stlapi.ChargePaid || len(ch.Payments) != 2 ||
		ch.CostComponent != stlapi.ComponentPreCarriage || ch.Recharged {
		t.Fatalf("charge %+v", ch)
	}

	w.ended("order.sa", "order.close")
	_, err = w.review(w.treasury, "order.sa", 0)
	w.must(err)
	c, err := w.close(w.treasury, "order.sa", 1)
	w.must(err)
	if c.ConfirmedResultMinor != "235000" {
		t.Fatalf("close %+v", c)
	}
	v = w.result("order.sa")
	s := v.Snapshots[0].Figures
	if s.NetCash.ReceiptsMinor != "690000" || s.NetCash.ChargePaymentsMinor != "65000" || s.NetCash.PaymentsMinor != "465000" ||
		s.NetCash.NetMinor != "225000" || s.NetCash.UnpaidChargesMinor != "0" || s.FXDifference.DifferenceMinor != "-10000" ||
		s.Confirmed.ResultMinor != "235000" || s.Cancelled {
		t.Fatalf("snapshot: net cash %+v FX %+v", s.NetCash, s.FXDifference)
	}
	// A payment unapplied after the close is a funds matter: the snapshot stays, the live view lists the freight part
	// unpaid again.
	u, err := w.unapply("app.freight.2", 1)
	w.must(err)
	if u.Status != stlapi.ApplicationUnapplied || u.ChargeOpenMinor != "25000" {
		t.Fatalf("unapply %+v", u)
	}
	v = w.result("order.sa")
	if v.Live.NetCash.NetMinor != "250000" || v.Live.NetCash.UnpaidChargesMinor != "25000" || v.Snapshots[0].Figures.NetCash.NetMinor != "225000" {
		t.Fatalf("after the unapply: live %+v snapshot %+v", v.Live.NetCash, v.Snapshots[0].Figures.NetCash)
	}
	var charged int
	for _, e := range w.events("company.A", stlapi.Source) {
		if e.Type == stlapi.EventApplied && e.Payload["charge_id"] == "freight.sa" && e.Payload["target_kind"] == stlapi.TargetCharge {
			charged++
		}
	}
	if charged != 2 {
		t.Fatalf("applied events to the charge: %d", charged)
	}
	// The raw database keeps a charge's payments within its amount (400.00 stand: 250.01 more is too much).
	err = w.exec(`INSERT INTO brant_stl.application (tenant_id, company_id, application_id, fund_record_id, target_kind, charge_id, currency,
		minor_units, amount_minor, status, version, owner_actor_id, applied_by_kind, applied_by_id, apply_epoch, applied_at, updated_at)
		SELECT tenant_id, company_id, 'app.raw', 'pay.freight.2', 'charge', 'freight.sa', 'CNY', 2, 25001, 'applied', 1, 'treasury.1', 'human',
		'treasury.1', 1, now(), now() FROM brant_stl.charge WHERE charge_id = 'freight.sa'`)
	if err == nil || !strings.Contains(err.Error(), "beyond its amount") {
		t.Fatalf("a payment beyond the charge: %v", err)
	}
}

// agentOfFunds is the fund world's AI agent (application.apply in its grant; the rule refuses agents).
func (w *r8World) agentOfFunds() context.Context { return w.agent }

// S5R-D19 (b): "部分结汇显示 unrealized" on PG16 — order.sb (USD 800, analysed in CNY at 7.0) prepaid USD 800 that
// recorded, when applied, the POL settlement rate USD→CNY 6.9 in force; the bank converted USD 500 at 6.875: realised
// 500 × (6.875 − 7.0) = −62.50, unrealised 300 × (6.9 − 7.0) = −30.00. A settlement rate on an order analysed in its own
// currency, or a rate of another purpose, is refused at /args/settlement_fx_rate.
func TestS5RD19PartialConversionIsUnrealized(t *testing.T) {
	w := newR8World(t)
	o := w.activeOrder("order.sb", "customer.1", w.usd, payStage("P1", "order_confirmed", "80000"))
	o.Analysis = &stl.OrderAnalysis{OrderCurrency: "USD", Currency: "CNY", Rate: "7.0", CurrencyRevision: w.cny.Revision}
	o.NetMinor = "80000"
	w.orders.put(o)
	own := w.activeOrder("order.own", "customer.1", w.usd, payStage("P1", "order_confirmed", "10000"))
	own.Analysis = &stl.OrderAnalysis{OrderCurrency: "USD", Currency: "USD", Rate: "", CurrencyRevision: w.usd.Revision}
	w.orders.put(own)
	rate := w.fxRate("usd.cny.settlement", "USD", "CNY", "6.9", polapi.PurposeSettlement)
	credit := w.fxRate("usd.cny.credit", "USD", "CNY", "6.9", polapi.PurposeCredit)
	w.usdReceipt("fund.sb", "800.00")
	w.usdReceipt("fund.own", "100.00")
	settled := func(id, fund, order, amount string, r polapi.ConfigRef) (stlapi.ApplicationReceipt, error) {
		cmd := w.applyCmd(id, fund, order, "P1", amount)
		ref := stlRef(r)
		cmd.SettlementFXRate = &ref
		return w.stl.Apply(w.treasury, cmd)
	}
	_, err := settled("app.own", "fund.own", "order.own", "100.00", rate)
	wantRefused(t, "an order analysed in its own currency", err, stlapi.CodeFXScopeMismatch, "/args/settlement_fx_rate")
	_, err = settled("app.credit", "fund.sb", "order.sb", "800.00", credit)
	if r, ok := kernel.AsRejection(err); !ok || r.Fields[0].Path != "/args/settlement_fx_rate" {
		t.Fatalf("a credit-purpose rate: %v", err)
	}
	a, err := settled("app.sb", "fund.sb", "order.sb", "800.00", rate)
	w.must(err)
	if a.SettlementRate == nil || a.SettlementRate.Rate != "6.9" || a.SettlementRate.Currency != "CNY" || a.SettlementRate.FXRate.ConfigID != rate.ConfigID {
		t.Fatalf("application %+v", a)
	}
	w.convert("fx.500", "fund.sb", "500.00", "6.875", "3437.50")
	fx := w.result("order.sb").Live.FXDifference
	if fx.RealizedMinor != "-6250" || fx.UnrealizedMinor != "-3000" || fx.DifferenceMinor != "-9250" || len(fx.Items) != 2 || !fx.Items[1].Unrealized ||
		fx.Items[1].Rate != "6.9" || fx.Items[1].SourceAmountMinor != "30000" {
		t.Fatalf("FX %+v", fx)
	}
	if got := w.sum(`SELECT settlement_rate::text || ' ' || settlement_currency FROM brant_stl.application WHERE application_id = 'app.sb'`); got != "6.9 CNY" {
		t.Fatalf("stored settlement rate %q", got)
	}
}

// S5R-D22 / STL-INV-014 / SM-STL-FUND on PG16: fund.reverse is the credit manager's, independent of the recorder
// (INDEPENDENT_REVIEW_REQUIRED), refused while an application stands (FUND_RECORD_HAS_APPLICATIONS, 409) or a draw
// stands (INVALID_STATE: reverse the conversion first); once reversed (version + 1, FundRecordReversed) the record is
// kept with its reversal, takes no application, and its bank reference may be recorded again. The database refuses a
// reversal without its record or with an application standing. A reversal and an application of one fund meet at the
// fund's lock: exactly one wins.
func TestS5RD22FundReverse(t *testing.T) {
	w := newR8World(t)
	w.activeOrder("order.r", "customer.1", w.usd, payStage("P1", "order_confirmed", "100000"))
	w.usdReceipt("fund.r", "500.00")
	_, err := w.apply("app.r", "fund.r", "order.r", "P1", "500.00")
	w.must(err)
	reverse := func(ctx context.Context, id string, version int64) (stlapi.FundRecordReceipt, error) {
		return w.stl.ReverseFund(ctx, stlapi.ReverseFund{IdempotencyKey: w.key(), FundRecordID: id, ExpectedVersion: version,
			Reason: "银行退回（合成）", EvidenceIDs: []string{w.advice}})
	}
	_, err = reverse(w.treasury, "fund.r", 1)
	wantRefused(t, "the recorder (treasury holds no fund.reverse)", err, kernel.CodePermissionDenied, "/")
	_, err = reverse(w.treasury2, "fund.r", 1)
	wantRefused(t, "standing application", err, stlapi.CodeFundRecordHasApplications, "/args/fund_record_id")
	own := w.receiptCmd("fund.own", "10.00")
	_, err = w.stl.RecordFund(w.treasury2, own)
	w.must(err)
	_, err = reverse(w.treasury2, "fund.own", 1)
	wantRefused(t, "the recorder with credit_manager", err, stlapi.CodeIndependentReviewRequired, "/args/fund_record_id")
	_, err = w.unapply("app.r", 1)
	w.must(err)
	_, err = reverse(w.treasury2, "fund.r", 2)
	wantRefused(t, "stale version", err, kernel.CodeVersionConflict, "/args/expected_version")
	r, err := reverse(w.treasury2, "fund.r", 1)
	w.must(err)
	if r.Status != stlapi.FundReversed || r.RowVersion != 2 || r.AvailableMinor != "0" {
		t.Fatalf("reversed %+v", r)
	}
	f, err := w.stl.GetFundRecord(w.treasury, "fund.r")
	w.must(err)
	if f.Status != stlapi.FundReversed || f.Reversal == nil || f.Reversal.ReversedBy.ID != "treasury.2" || f.CashMinor != "0" || len(f.Allocations) != 1 {
		t.Fatalf("fund_record.get %+v", f)
	}
	_, err = w.apply("app.r2", "fund.r", "order.r", "P1", "1.00")
	wantRefused(t, "an application of a reversed fund", err, kernel.CodeInvalidState, "/args/fund_record_id")
	_, err = reverse(w.treasury2, "fund.r", 2)
	wantRefused(t, "reversed twice", err, kernel.CodeInvalidState, "/args/fund_record_id")
	again := w.receiptCmd("fund.r.again", "500.00")
	again.BankReference = "ref.fund.r"
	_, err = w.stl.RecordFund(w.treasury, again)
	w.must(err) // the bank reference is free again

	// A draw stands: reverse the conversion first.
	w.usdReceipt("fund.d", "100.00")
	w.convert("fx.d", "fund.d", "100.00", "7.0", "700.00")
	_, err = reverse(w.treasury2, "fund.d", 1)
	wantRefused(t, "a standing conversion", err, kernel.CodeInvalidState, "/args/fund_record_id")
	_, err = reverse(w.treasury2, "fx.d", 1)
	w.must(err)
	_, err = reverse(w.treasury2, "fund.d", 1)
	w.must(err)
	var reversed int
	for _, e := range w.events("company.A", stlapi.Source) {
		if e.Type == stlapi.EventFundRecordReversed {
			reversed++
			if e.Payload["fund_record_version"] != float64(2) || e.Payload["reason"] != "银行退回（合成）" || e.Payload["direction"] == nil {
				t.Fatalf("event %+v", e.Payload)
			}
		}
	}
	if reversed != 3 {
		t.Fatalf("FundRecordReversed events: %d", reversed)
	}

	// The database: no reversal without its record, none with an application standing, none by the recorder.
	w.usdReceipt("fund.raw", "50.00")
	if err := w.exec(`UPDATE brant_stl.fund_record SET status = 'reversed', version = 2 WHERE fund_record_id = 'fund.raw'`); err == nil {
		t.Fatal("a reversal without its record")
	}
	if err := w.exec(`UPDATE brant_stl.fund_record SET status = 'reversed', version = 2 WHERE fund_record_id = 'fund.raw'`,
		`INSERT INTO brant_stl.fund_reversal (tenant_id, company_id, fund_record_id, reason, evidence_ids, reversed_by_kind, reversed_by_id,
			reverse_epoch, reversed_at) SELECT tenant_id, company_id, fund_record_id, 'r', '{}', 'human', recorded_by_id, 1, now()
			FROM brant_stl.fund_record WHERE fund_record_id = 'fund.raw'`); err == nil {
		t.Fatal("a reversal by the recorder")
	}

	// A reversal and an application of one fund: exactly one wins, three rounds.
	for round := 0; round < 3; round++ {
		id := "fund.race." + string(rune('a'+round))
		w.usdReceipt(id, "10.00")
		var wg sync.WaitGroup
		var applyErr, reverseErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, applyErr = w.apply("app.race."+id, id, "order.r", "P1", "10.00") }()
		go func() { defer wg.Done(); _, reverseErr = reverse(w.treasury2, id, 1) }()
		wg.Wait()
		if (applyErr == nil) == (reverseErr == nil) {
			t.Fatalf("round %d: apply %v, reverse %v", round, applyErr, reverseErr)
		}
	}
}

// S5R-D22 on the opening world: an opening customer prepayment is reversed with its item (the source document may be
// recorded again); an opening receivable takes a credit note without an order (requested by the sales clerk, confirmed
// by a credit manager, applied: the opening receivable's open amount and the exposure fall).
func TestS5RD22OpeningCorrections(t *testing.T) {
	ow := newOpeningWorld(t)
	ow.limit() // the customer's credit root, so its exposure is read
	ow.record(ow.prepaymentItem("ob.rcv", stlapi.OpeningCustomerPrepayment, "RCV-0311", "5000.00", "2000.00", "MT103-0311"))
	r, err := ow.stl.ReverseFund(ow.manager1, stlapi.ReverseFund{IdempotencyKey: ow.key(), FundRecordID: "ob.rcv", ExpectedVersion: 1,
		Reason: "期初导入金额错误（合成）"})
	ow.must(err)
	if r.Status != stlapi.FundReversed {
		t.Fatalf("%+v", r)
	}
	if v := ow.opening("ob.rcv"); v.Status != "reversed" {
		t.Fatalf("opening item %+v", v)
	}
	ow.record(ow.prepaymentItem("ob.rcv.2", stlapi.OpeningCustomerPrepayment, "RCV-0311", "5000.00", "1800.00", "MT103-0311"))

	ow.record(ow.statementItem("ob.ar", stlapi.OpeningReceivable, "AR-2026-0107", "7000.00", "7000.00", "7000.00", 15))
	before := ow.exposure()
	_, err = ow.stl.RequestCreditNote(ow.sales, stlapi.RequestCreditNote{IdempotencyKey: ow.key(), CreditNoteID: "cn.ob", CreditKind: stlapi.CreditPriceAllowance,
		OrderID: "order.legacy", StatementID: "ob.ar", Amount: "500.00", Basis: "期初应收折让（合成）", EvidenceIDs: []string{ow.ledger}})
	wantRefused(t, "an order on an opening receivable", err, kernel.CodeInvalidKnownValue, "/args/order_id")
	n, err := ow.stl.RequestCreditNote(ow.sales, stlapi.RequestCreditNote{IdempotencyKey: ow.key(), CreditNoteID: "cn.ob",
		CreditKind: stlapi.CreditPriceAllowance, StatementID: "ob.ar", Amount: "500.00", Basis: "期初应收折让（合成）", EvidenceIDs: []string{ow.ledger}})
	ow.must(err)
	if n.OrderID != "" || n.Status != stlapi.CreditRequested {
		t.Fatalf("credit note %+v", n)
	}
	_, err = ow.stl.ConfirmCredit(ow.manager1, stlapi.ConfirmCredit{IdempotencyKey: ow.key(), CreditNoteID: "cn.ob", ExpectedVersion: 1,
		Decision: stlapi.DecisionConfirm, Reason: "核准"})
	ow.must(err)
	a, err := ow.stl.Apply(ow.treasury, stlapi.Apply{IdempotencyKey: ow.key(), ApplicationID: "app.cn.ob", CreditNoteID: "cn.ob",
		Target: toStatement(stlapi.TargetReceivable, "ob.ar", ""), Amount: "500.00"})
	ow.must(err)
	if a.StatementOpenMinor != "650000" || a.OrderID != "" {
		t.Fatalf("applied %+v", a)
	}
	ref, err := ow.stl.GetCreditNote(ow.treasury, "cn.ob")
	ow.must(err)
	if ref.NetMinor != "50000" || ref.TaxMinor != "0" || !ref.Effective || ref.OrderID != "" {
		t.Fatalf("credit note ref %+v", ref)
	}
	if after := ow.exposure(); after == before || after != "USD 650000" {
		t.Fatalf("exposure %s → %s", before, after)
	}
}

// S5R-D20 / STL-INV-013 on PG16: the invoice_reversed flag of a payable derives from CNX's invoices of the commitments
// it settles (no flag while the blue invoice stands; raised once a red invoice reverses it in part; not shown without
// CNX); treasury requests a price allowance on the red invoice (a blue one, or one CNX does not show, is
// REFERENCE_INVALID); its requester may not confirm it, the credit manager does: the payable falls by it, the event
// PayableAdjustmentConfirmed carries the designed payload and the order result's purchase cost falls by its net part; a
// statement is not reversed while an adjustment waits; the database refuses a decision by the requester.
func TestS5RD20PayableAdjustmentAndInvoiceReversed(t *testing.T) {
	w := newR8World(t)
	o := w.saOrder()
	w.ful.shipped(o, "shipment.all", "100", "100000", time.Now().Add(-24*time.Hour))
	_, err := w.stl.ConfirmSettlement(w.treasury, stlapi.ConfirmSettlement{IdempotencyKey: w.key(), StatementID: "st.p",
		StatementKind: stlapi.StatementPurchase, Sources: []stlapi.SettlementSourceRef{{DeliveryID: "delivery.sa", LineID: "L1", Version: 1}}})
	w.must(err)
	blue := stl.PurchaseInvoice{PurchaseInvoiceID: "inv.blue", Status: "verified", Colour: "blue", Number: "SYN0001", SupplierPartyID: "supplier.sa",
		CurrencyCode: "CNY", Matches: []stl.PurchaseInvoiceMatch{{LineNo: 1, CommitmentID: "commitment.sa", OrderLineID: "L1"}}}
	w.invoices.set(blue)
	p, err := w.withCNX.GetPayable(w.treasury, "st.p")
	w.must(err)
	if p.InvoiceReversed || !p.InvoicesShown || len(p.Invoices) != 0 {
		t.Fatalf("payable before the red invoice %+v", p)
	}
	if p, err = w.stl.GetPayable(w.treasury, "st.p"); err != nil || p.InvoicesShown || p.InvoiceReversed {
		t.Fatalf("without CNX %+v %v", p, err)
	}
	blue.Reversal, blue.ReversedBy = "partial", []string{"inv.red"}
	red := stl.PurchaseInvoice{PurchaseInvoiceID: "inv.red", Status: "verified", Colour: "red", Number: "SYN0002", SupplierPartyID: "supplier.sa",
		CurrencyCode: "CNY", RedOf: "inv.blue"}
	w.invoices.set(blue, red)
	st, err := w.withCNX.GetStatement(w.treasury, "st.p")
	w.must(err)
	if !st.InvoiceReversed || len(st.Invoices) != 2 || st.Invoices[0].Reversal != "partial" || st.Invoices[1].RedOf != "inv.blue" {
		t.Fatalf("statement after the red invoice %+v", st)
	}

	adjust := func(m *stl.Module, ctx context.Context, id, invoice, amount string) (stlapi.PayableAdjustmentReceipt, error) {
		return m.RequestPayableAdjustment(ctx, stlapi.RequestPayableAdjustment{IdempotencyKey: w.key(), AdjustmentID: id, StatementID: "st.p",
			AdjustmentKind: stlapi.AdjustmentPriceAllowance, Amount: amount, PurchaseInvoiceID: invoice, Basis: "工厂价格折让（合成）",
			EvidenceIDs: []string{w.bill}})
	}
	_, err = adjust(w.stl, w.treasury, "adj.0", "inv.red", "200.00")
	wantRefused(t, "CNX not wired", err, kernel.CodeReferenceInvalid, "/args/purchase_invoice_id")
	_, err = adjust(w.withCNX, w.treasury, "adj.0", "inv.blue", "200.00")
	wantRefused(t, "an allowance on a blue invoice", err, kernel.CodeReferenceInvalid, "/args/purchase_invoice_id")
	_, err = adjust(w.withCNX, w.treasury, "adj.0", "inv.red", "4000.01")
	wantRefused(t, "more than is open", err, stlapi.CodeApplicationExceedsOpen, "/args/amount")
	_, err = adjust(w.withCNX, w.agent, "adj.0", "inv.red", "200.00")
	wantRefused(t, "an agent", err, kernel.CodePermissionDenied, "/")
	r, err := adjust(w.withCNX, w.treasury, "adj.1", "inv.red", "200.00")
	w.must(err)
	if r.Status != stlapi.AdjustmentRequested || r.RowVersion != 1 || r.StatementVersion != 2 || r.StatementOpenMinor != "400000" {
		t.Fatalf("requested %+v", r)
	}
	_, err = w.stl.ReverseSettlement(w.treasury2, stlapi.ReverseSettlement{IdempotencyKey: w.key(), StatementID: "st.p", ExpectedVersion: 2,
		Reason: "错误结算"})
	wantRefused(t, "a reversal while an adjustment waits", err, kernel.CodeInvalidState, "/args/statement_id")
	decide := func(ctx context.Context, version int64) (stlapi.PayableAdjustmentReceipt, error) {
		return w.withCNX.ConfirmPayableAdjustment(ctx, stlapi.ConfirmPayableAdjustment{IdempotencyKey: w.key(), AdjustmentID: "adj.1",
			ExpectedVersion: version, Decision: stlapi.DecisionConfirm, Reason: "红票核对一致"})
	}
	_, err = decide(w.treasury, 1)
	wantRefused(t, "the requester (treasury holds no confirm)", err, kernel.CodePermissionDenied, "/")
	// treasury.2 holds treasury and credit_manager: its own request it may not confirm; a credit manager rejects it.
	_, err = adjust(w.withCNX, w.treasury2, "adj.2", "inv.red", "10.00")
	w.must(err)
	_, err = w.withCNX.ConfirmPayableAdjustment(w.treasury2, stlapi.ConfirmPayableAdjustment{IdempotencyKey: w.key(), AdjustmentID: "adj.2",
		ExpectedVersion: 1, Decision: stlapi.DecisionConfirm, Reason: "自审"})
	wantRefused(t, "the requester with credit_manager", err, stlapi.CodeIndependentReviewRequired, "/args/adjustment_id")
	rej, err := w.withCNX.ConfirmPayableAdjustment(w.manager, stlapi.ConfirmPayableAdjustment{IdempotencyKey: w.key(), AdjustmentID: "adj.2",
		ExpectedVersion: 1, Decision: stlapi.DecisionReject, Reason: "重复申请"})
	w.must(err)
	if rej.Status != stlapi.AdjustmentRejected || rej.StatementOpenMinor != "400000" {
		t.Fatalf("rejected %+v", rej)
	}
	c, err := decide(w.manager, 1)
	w.must(err)
	if c.Status != stlapi.AdjustmentConfirmed || c.RowVersion != 2 || c.StatementVersion != 5 || c.StatementOpenMinor != "380000" {
		t.Fatalf("confirmed %+v", c)
	}
	p, err = w.withCNX.GetPayable(w.treasury, "st.p")
	w.must(err)
	if p.OpenMinor != "380000" || p.AdjustedMinor != "20000" || p.AppliedMinor != "0" || len(p.Adjustments) != 2 || !p.Adjustments[0].Effective ||
		p.Adjustments[0].NetMinor != "20000" || p.Installments[0].OpenMinor != "380000" {
		t.Fatalf("payable after the adjustment %+v", p)
	}
	var confirmed []map[string]any
	for _, e := range w.events("company.A", stlapi.Source) {
		if e.Type == stlapi.EventPayableAdjustmentConfirmed {
			confirmed = append(confirmed, e.Payload)
		}
	}
	if len(confirmed) != 1 || confirmed[0]["statement_id"] != "st.p" || confirmed[0]["statement_version"] != float64(5) ||
		confirmed[0]["adjustment_id"] != "adj.1" || confirmed[0]["kind"] != stlapi.AdjustmentPriceAllowance || confirmed[0]["amount_minor"] != "20000" ||
		confirmed[0]["currency"] != "CNY" || confirmed[0]["purchase_invoice_id"] != "inv.red" {
		t.Fatalf("PayableAdjustmentConfirmed %+v", confirmed)
	}
	// The payment is limited to what remains: 3,800.00.
	w.payment("fund.pay.p", "supplier.sa", "4000.00")
	_, err = w.stl.Apply(w.treasury, stlapi.Apply{IdempotencyKey: w.key(), ApplicationID: "app.p", FundRecordID: "fund.pay.p",
		Target: toStatement(stlapi.TargetPayable, "st.p", ""), Amount: "3800.01"})
	wantRefused(t, "a payment beyond the adjusted payable", err, stlapi.CodeApplicationExceedsOpen, "/args/amount")
	if v := w.result("order.sa"); v.Live.Executing.PurchaseConfirmedMinor != "380000" {
		t.Fatalf("purchase cost after the allowance %+v", v.Live.Executing)
	}
	if err := w.exec(`UPDATE brant_stl.payable_adjustment SET status = 'rejected', version = 2, decided_by_kind = 'human',
		decided_by_id = requested_by_id, decide_epoch = 1, decided_at = now(), decision_reason = 'x', updated_at = now()`); err == nil {
		t.Fatal("a decision by the requester")
	}
}

// S5R-D24 / STL-INV-010 on PG16: an order whose buyer bears the main carriage (FCA) refuses a main_carriage charge
// (INVALID_KNOWN_VALUE /args/cost_component) unless recharged; its pre_carriage is ours; a CIF order's main carriage is
// ours. A recharged charge stays out of the order result (listed apart).
func TestS5RD24CostComponentFollowsTheIncoterm(t *testing.T) {
	w := newR8World(t)
	o := w.saOrder()
	o.Incoterm = "FCA"
	w.orders.put(o)
	cif := w.activeOrder("order.cif", "customer.1", w.usd, payStage("P1", "order_confirmed", "10000"))
	cif.Incoterm, cif.NetMinor = "CIF", "10000"
	cif.Analysis = &stl.OrderAnalysis{OrderCurrency: "USD", Currency: "CNY", Rate: "7.0", CurrencyRevision: w.cny.Revision}
	w.orders.put(cif)
	main := w.estimate("ocean.fca", "OCEAN-FCA", "1200.00", orderTarget("order.sa"))
	main.CostComponent = stlapi.ComponentMainCarriage
	_, err := w.record(w.officer, main)
	wantRefused(t, "the main carriage of an FCA order", err, kernel.CodeInvalidKnownValue, "/args/cost_component")
	main.Recharged = true
	_, err = w.record(w.officer, main)
	w.must(err)
	ocean := w.estimate("ocean.cif", "OCEAN-CIF", "800.00", orderTarget("order.cif"))
	ocean.CostComponent = stlapi.ComponentMainCarriage
	w.must2(w.record(w.officer, ocean))
	w.saEstimate()
	v := w.result("order.sa")
	if v.Live.Forecast.DirectChargesMinor != "50000" || len(v.Live.Recharged) != 1 || v.Live.Recharged[0].ID != "ocean.fca" {
		t.Fatalf("recharged in the result %+v %+v", v.Live.Forecast, v.Live.Recharged)
	}
	if got := w.sum(`SELECT string_agg(charge_id || ':' || cost_component || ':' || recharged::text, ' ' ORDER BY charge_id) FROM brant_stl.charge`); got !=
		"freight.sa:pre_carriage:false ocean.cif:main_carriage:false ocean.fca:main_carriage:true" {
		t.Fatalf("stored components %q", got)
	}
}

// S5R-D42 / STL-INV-011 on PG16 ("取消订单有运费时关闭为 cancelled"; "无费用的取消订单不生成经营结果"): order.cx bought 100 PCS
// for CNY 4,000; 60 were delivered, settled and paid, and written off (PUR's approved write_off disposition) when TRD
// cancelled the order; its freight of CNY 650 is confirmed. The result closes settled, marked cancelled: no revenue,
// write-off CNY 2,400 + freight 650 = −3,050.00; the snapshot's cancelled column is set. A cancelled order without
// charges or write-offs has no result: review is INVALID_STATE.
func TestS5RD42CancelledOrderResult(t *testing.T) {
	w := newR8World(t)
	cx := w.activeOrder("order.cx", "customer.1", w.usd, payStage("P1", "order_confirmed", "100000"))
	cx.Analysis = &stl.OrderAnalysis{OrderCurrency: "USD", Currency: "CNY", Rate: "7.0", CurrencyRevision: w.cny.Revision}
	cx.NetMinor = "100000"
	w.orders.put(cx)
	w.pur.bought(cx, "commitment.cx", "supplier.sa", "delivery.cx", "60", "240000")
	w.pur.mu.Lock()
	line := &w.pur.commitments["order.cx"][0]
	line.Status, line.NetMinor, line.PurchaseQuantity, line.VerifiedQuantity = "cancelled", "400000", "100", "60"
	line.Dispositions = []stl.OrderDisposition{{DispositionID: "disposition.1", Disposition: "write_off", Quantity: "60"}}
	w.pur.mu.Unlock()
	_, err := w.stl.ConfirmSettlement(w.treasury, stlapi.ConfirmSettlement{IdempotencyKey: w.key(), StatementID: "st.cx",
		StatementKind: stlapi.StatementPurchase, Sources: []stlapi.SettlementSourceRef{{DeliveryID: "delivery.cx", LineID: "L1", Version: 1}}})
	w.must(err)
	w.payment("fund.cx", "supplier.sa", "2400.00")
	_, err = w.stl.Apply(w.treasury, stlapi.Apply{IdempotencyKey: w.key(), ApplicationID: "app.cx", FundRecordID: "fund.cx",
		Target: toStatement(stlapi.TargetPayable, "st.cx", ""), Amount: "2400.00"})
	w.must(err)
	freight := w.estimate("freight.cx", "OFFER-CX", "650.00", orderTarget("order.cx"))
	w.must2(w.record(w.officer, freight))
	w.must2(w.confirmCharge(w.treasury, "freight.cx", 1))
	w.activeOrder("order.none", "customer.1", w.usd, payStage("P1", "order_confirmed", "100000"))
	for _, id := range []string{"order.cx", "order.none"} {
		w.orders.mu.Lock()
		o := w.orders.orders[id]
		o.Status, o.Ended = "cancelled", "order.cancel"
		w.orders.orders[id] = o
		w.orders.mu.Unlock()
	}
	_, err = w.review(w.treasury, "order.none", 0)
	wantRefused(t, "a cancelled order without sunk cost", err, kernel.CodeInvalidState, "/args/order_id")
	v := w.result("order.cx")
	if !v.Cancelled || !v.Live.Cancelled || !v.Live.SunkCost || conditionOf(v.Conditions, stlapi.DimensionFulfilment).Status != stlapi.ConditionMet {
		t.Fatalf("cancelled result %+v", v)
	}
	c := v.Live.Confirmed
	if c.NetSalesMinor != "0" || c.WriteOffCostMinor != "240000" || c.PurchaseCostMinor != "240000" || c.DirectChargesMinor != "65000" ||
		c.ResultMinor != "-305000" {
		t.Fatalf("confirmed %+v", c)
	}
	_, err = w.review(w.treasury, "order.cx", 0)
	w.must(err)
	r, err := w.close(w.treasury, "order.cx", 1)
	w.must(err)
	if r.Status != stlapi.ResultSettled || r.ConfirmedResultMinor != "-305000" {
		t.Fatalf("close %+v", r)
	}
	if got := w.sum(`SELECT cancelled::text FROM brant_stl.order_result_snapshot WHERE order_id = 'order.cx'`); got != "true" {
		t.Fatalf("snapshot cancelled %q", got)
	}
	if s := w.result("order.cx").Snapshots[0]; !s.Figures.Cancelled || s.Figures.Confirmed.ResultMinor != "-305000" {
		t.Fatalf("snapshot %+v", s.Figures.Confirmed)
	}
}

// S5R-D22 and S5R-D20 over HTTP with OIDC access tokens: fund.reverse by the credit manager (200, reconciled with the
// same action), the agent refused (403); payable_adjustment.request names a statement STL does not show (404).
func TestS5RFundReverseOverHTTP(t *testing.T) {
	q := newOrderWorld(t)
	usdBank := q.configure(q.author, q.reviewer, polapi.KindBankAccount, "bank.usd", 0, q.win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "USD", polapi.AccountBoth), true)
	q.assign("treasury.1", "treasury")
	q.assign("manager.1", "credit_manager")
	q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.funds"},
		Permissions: []string{"fund.reverse", "payable_adjustment.request", "stl.read"}, DataScope: "company"})
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	treasurer := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "treasury.1", "human"), n: 300}
	manager := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "manager.1", "human"), n: 400}
	agent := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "agent.funds", "ai"), n: 500}
	ref := func(r polapi.ConfigRef) map[string]any {
		return map[string]any{"kind": r.Kind, "id": r.ConfigID, "revision": r.Revision}
	}
	if r := treasurer.command("stl", stlapi.Contract, stlapi.CommandRecordFund, map[string]any{"fund_record_id": "fund.http", "kind": "receipt",
		"counterparty_id": "customer.1", "currency": ref(q.usd), "bank_account": ref(usdBank), "received_amount": "100.00",
		"value_date": kernel.FormatTimestamp(time.Now()), "bank_reference": "MT103-R8", "evidence_ids": []any{"evidence"}}); r["status"] != "recorded" {
		t.Fatalf("fund.record %v", r)
	}
	args := map[string]any{"fund_record_id": "fund.http", "expected_version": 1, "reason": "银行退回（合成）"}
	if status, out := agent.try("stl", stlapi.Contract, stlapi.CommandReverseFund, args); status != http.StatusForbidden ||
		out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("agent fund.reverse %d %v", status, out)
	}
	envelope := map[string]any{"contract": stlapi.Contract, "command": stlapi.CommandReverseFund, "idempotency_key": "stl.r8.reverse", "args": args}
	status, res := manager.do("POST", "/api/brant/v1/stl/commands/"+stlapi.CommandReverseFund, envelope)
	if status != http.StatusOK || res["object_id"] != "fund.http" || res["status"] != stlapi.FundReversed || res["row_version"] != float64(2) {
		t.Fatalf("fund.reverse %d %v", status, res)
	}
	if status, rec := manager.do("POST", "/api/brant/v1/stl/commands/"+stlapi.CommandReverseFund+"/reconcile", envelope); status != http.StatusOK ||
		rec["state"] != "committed" || rec["action_id"] != res["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	if status, f := treasurer.do("GET", "/api/brant/v1/stl/fund_records/fund.http", nil); status != http.StatusOK || f["status"] != stlapi.FundReversed ||
		f["reversal"] == nil {
		t.Fatalf("fund_record.get %d %v", status, f)
	}
	if status, out := treasurer.try("stl", stlapi.Contract, stlapi.CommandRequestPayableAdjustment, map[string]any{"adjustment_id": "adj.http",
		"statement_id": "st.none", "kind": "price_allowance", "amount": "1.00", "purchase_invoice_id": "inv.red", "basis": "折让",
		"evidence_ids": []any{"evidence"}}); status != http.StatusNotFound {
		t.Fatalf("payable_adjustment.request of no statement %d %v", status, out)
	}
}
