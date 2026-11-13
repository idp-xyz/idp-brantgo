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

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// STL opening balances (S5 unit C10, WBS S5-20; tactical model STL-AGG-SettlementStatement entity OpeningBalance,
// value object Cutoff; STL-INV-012; Context Map MD-50, R75; baseline 11.2 期初、防重、切换; BT17, CN-AT16 STL half;
// SC-09's "未结 7,000" seed for D2) on PostgreSQL 16. Python V0.50 has no STL, so the tests cite design IDs only.
//
// The opening world is the credit world (E-2's synthetic credit policy, customer.1 with a 10,000.00 USD limit when
// a test publishes it, the stand-in TRD orders) with the company's USD bank account, a confirmed supplier
// (supplier.1), the old books' exports as EVD sources, the treasurer treasury.1 who records opening items and
// applies funds, and agent.opening, an AI agent holding the permission: opening_balance.record is human only. The
// cutoff is twelve days ago, the source documents older.

type openingWorld struct {
	*creditWorld
	treasury, agent context.Context
	bankUSD         polapi.ConfigRef
	ledger, advice  string
	cutoff          time.Time
}

func newOpeningWorld(t *testing.T) *openingWorld {
	t.Helper()
	cw := newCreditWorld(t)
	ow := &openingWorld{creditWorld: cw, cutoff: kernel.Instant(time.Now().Add(-12 * 24 * time.Hour).Truncate(time.Second))}
	ow.treasury = cw.assign("treasury.1", "treasury")
	ow.bankUSD = cw.configure(cw.editor, cw.reviewer, polapi.KindBankAccount, "bank.usd", 0, cw.win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "USD", polapi.AccountBoth), true)
	ow.ledger = cw.source(cw.editor, "ledger.export", "合成旧账截止日导出：应收、应付、未核销预收预付明细（测试数据，非真实账簿）")
	ow.advice = cw.source(cw.editor, "advice.1", "合成银行入账通知")
	_, err := cw.c.PTY.SaveParty(cw.editor, ptyapi.SaveParty{IdempotencyKey: cw.key(), PartyID: "supplier.1", Name: "supplier.1",
		Roles: []string{ptyapi.RoleSupplier}})
	cw.must(err)
	_, err = cw.c.PTY.ConfirmParty(cw.editor, ptyapi.ConfirmParty{IdempotencyKey: cw.key(), PartyID: "supplier.1", ExpectedVersion: 1,
		EvidenceIDs: []string{"evidence"}})
	cw.must(err)
	ow.agent = cw.provision(bootstrap.Grant{Scope: cw.scope("company.A"), Actor: actorRef("ai", "agent.opening"),
		Permissions: []string{stlapi.CommandRecordOpening, "stl.read", "evd.read", "pty.read", "pol.read"}, DataScope: "company"})
	return ow
}

// statementItem is opening_balance.record of a receivable (customer.1) or payable (supplier.1) open amount at the
// cutoff from document of source amount, due in the installments given (amount, days from now).
func (ow *openingWorld) statementItem(id, kind, document, source, amount string, due ...any) stlapi.RecordOpening {
	party := "customer.1"
	if kind == stlapi.OpeningPayable {
		party = "supplier.1"
	}
	c := stlapi.RecordOpening{IdempotencyKey: ow.key(), OpeningID: id, OpeningKind: kind, CutoffAt: kernel.FormatTimestamp(ow.cutoff),
		Source:         stlapi.OpeningSource{Document: document, IssuedAt: kernel.FormatTimestamp(ow.cutoff.AddDate(0, 0, -40)), Amount: source},
		CounterpartyID: party, OrderReference: "SO-LEGACY-42", Currency: stlRef(ow.usd), Amount: amount, EvidenceIDs: []string{ow.ledger}}
	for i := 0; i+1 < len(due); i += 2 {
		c.Installments = append(c.Installments, stlapi.OpeningInstallment{Amount: due[i].(string),
			DueAt: kernel.FormatTimestamp(time.Now().Add(time.Duration(due[i+1].(int)) * 24 * time.Hour))})
	}
	return c
}

// prepaymentItem is opening_balance.record of money customer.1 paid (customer_prepayment) or we paid supplier.1
// (supplier_prepayment) before the cutoff, amount unapplied at it, on the USD account under bank reference.
func (ow *openingWorld) prepaymentItem(id, kind, document, source, amount, reference string) stlapi.RecordOpening {
	party := "customer.1"
	if kind == stlapi.OpeningSupplierPrepayment {
		party = "supplier.1"
	}
	bank := stlRef(ow.bankUSD)
	return stlapi.RecordOpening{IdempotencyKey: ow.key(), OpeningID: id, OpeningKind: kind, CutoffAt: kernel.FormatTimestamp(ow.cutoff),
		Source:         stlapi.OpeningSource{Document: document, IssuedAt: kernel.FormatTimestamp(ow.cutoff.AddDate(0, 0, -20)), Amount: source},
		CounterpartyID: party, Currency: stlRef(ow.usd), Amount: amount, BankAccount: &bank, BankReference: reference,
		ValueDate: kernel.FormatTimestamp(ow.cutoff.AddDate(0, 0, -20)), EvidenceIDs: []string{ow.advice}}
}

func (ow *openingWorld) record(c stlapi.RecordOpening) stlapi.OpeningReceipt {
	ow.t.Helper()
	r, err := ow.stl.RecordOpening(ow.treasury, c)
	ow.must(err)
	return r
}

// fund records a fund record of kind with customer.1 (receipt) or supplier.1 (payment) of amount now.
func (ow *openingWorld) fund(id, kind, amount string) {
	ow.t.Helper()
	party := "customer.1"
	if kind == stlapi.FundPayment {
		party = "supplier.1"
	}
	_, err := ow.stl.RecordFund(ow.treasury, stlapi.RecordFund{IdempotencyKey: ow.key(), FundRecordID: id, FundKind: kind, CounterpartyID: party,
		Currency: stlRef(ow.usd), BankAccount: stlRef(ow.bankUSD), ReceivedAmount: amount, ValueDate: kernel.FormatTimestamp(time.Now()),
		BankReference: "ref." + id, EvidenceIDs: []string{ow.advice}})
	ow.must(err)
}

func (ow *openingWorld) apply(id, fund, kind, statement, amount string) (stlapi.ApplicationReceipt, error) {
	return ow.stl.Apply(ow.treasury, stlapi.Apply{IdempotencyKey: ow.key(), ApplicationID: id, FundRecordID: fund,
		Target: toStatement(kind, statement, ""), Amount: amount})
}

func (ow *openingWorld) opening(id string) stlapi.OpeningView {
	ow.t.Helper()
	v, err := ow.stl.GetOpening(ow.treasury, id)
	ow.must(err)
	return v
}

func (ow *openingWorld) totals(cutoff time.Time) map[string]stlapi.OpeningTotalView {
	ow.t.Helper()
	v, err := ow.stl.Openings(ow.treasury, kernel.FormatTimestamp(cutoff))
	ow.must(err)
	out := map[string]stlapi.OpeningTotalView{}
	for _, x := range v.Totals {
		out[x.OpeningKind] = x
	}
	return out
}

func (ow *openingWorld) exposure() string {
	ow.t.Helper()
	v, err := ow.stl.GetCustomerCredit(ow.officer, "customer.1")
	ow.must(err)
	return v.ExposureCurrency + " " + v.ExposureMinor
}

// CN-AT16 STL half (业务期初及未达账款导入: 截止、来源余额及已办状态可核对；上线不重复生成现金、应付; STL-INV-012, MD-50):
//
//   - At the cutoff the old books hold: AR-2026-0107, 10,000.00 billed, 7,000.00 open (4,000.00 due in 15 days,
//     3,000.00 in 45); AP-SUP1-88, 2,400.00 billed, 1,800.00 open; RCV-0311, 5,000.00 received from customer.1,
//     2,000.00 unapplied; PAY-0920, 900.00 paid to supplier.1, all unapplied. Each becomes one opening item: the
//     receivable and the payable opening statements without a line or an order, the prepayments opening fund
//     records on the USD account under their bank references; one opening_balance_recorded.v1 each.
//   - The exposure of customer.1 counts the 7,000.00 (SC-09's seed); the prepayment does not lower it until
//     applied.
//   - Imported twice — the same documents under new IDs, or the same ID again, or the receivable at a later
//     cutoff — every item is OPENING_BALANCE_DUPLICATE (409) and nothing is written; the bank movement of an
//     opening prepayment recorded again by fund.record is BUSINESS_KEY_CONFLICT: no second receivable, payable or
//     cash.
//   - Applications work as for any statement: the 2,000.00 prepayment and a new receipt of 5,000.00 settle the
//     receivable (open 0, exposure 0.00), the 900.00 supplier prepayment and a payment of 900.00 the payable.
//   - opening_balance.list at the cutoff reconciles every kind: source amounts, settled before the cutoff, open at
//     it, applied since, open now.
func TestCNAT16OpeningBalancesReconcileAtTheCutoff(t *testing.T) {
	ow := newOpeningWorld(t)
	ow.limit()
	ar := ow.record(ow.statementItem("ob.ar", stlapi.OpeningReceivable, "AR-2026-0107", "10000.00", "7000.00", "4000.00", 15, "3000.00", 45))
	if ar.Status != stlapi.OpeningRecorded || ar.RowVersion != 1 || ar.StatementID != "ob.ar" || ar.FundRecordID != "" || ar.AmountMinor != "700000" ||
		!ar.CutoffAt.Equal(ow.cutoff) || ar.SourceDocument != "AR-2026-0107" || ar.OpeningKind != stlapi.OpeningReceivable {
		t.Fatalf("receivable %+v", ar)
	}
	ow.record(ow.statementItem("ob.ap", stlapi.OpeningPayable, "AP-SUP1-88", "2400.00", "1800.00", "1800.00", 20))
	pre := ow.record(ow.prepaymentItem("ob.pre", stlapi.OpeningCustomerPrepayment, "RCV-0311", "5000.00", "2000.00", "BOC-0311"))
	if pre.FundRecordID != "ob.pre" || pre.StatementID != "" {
		t.Fatalf("prepayment %+v", pre)
	}
	ow.record(ow.prepaymentItem("ob.sup", stlapi.OpeningSupplierPrepayment, "PAY-0920", "900.00", "900.00", "BOC-0920"))

	st, err := ow.stl.GetStatement(ow.treasury, "ob.ar")
	ow.must(err)
	if st.OpeningID != "ob.ar" || st.Kind != stlapi.StatementSales || st.OrderID != "" || len(st.Lines) != 0 || st.NetMinor != "700000" ||
		st.TaxMinor != "0" || len(st.Installments) != 2 || st.Installments[0].AmountMinor != "400000" || st.Installments[0].StageID != "" ||
		st.OpenMinor != "700000" {
		t.Fatalf("opening statement %+v", st)
	}
	if f, err := ow.stl.GetFundRecord(ow.treasury, "ob.pre"); err != nil || f.Kind != stlapi.FundReceipt || f.ReceivedMinor != "200000" ||
		f.AvailableMinor != "200000" || f.BankReference != "BOC-0311" {
		t.Fatalf("opening fund record %+v %v", f, err)
	}
	if got := ow.exposure(); got != "USD 700000" {
		t.Fatalf("exposure with the opening receivable: %s", got)
	}
	if n := len(slices.DeleteFunc(ow.events("company.A", stlapi.Source), func(e event) bool { return e.Type != stlapi.EventOpeningBalanceRecorded })); n != 4 {
		t.Fatalf("%d opening_balance_recorded events", n)
	}

	for name, again := range map[string]stlapi.RecordOpening{
		"receivable again":        ow.statementItem("ob.ar.2", stlapi.OpeningReceivable, "AR-2026-0107", "10000.00", "7000.00", "7000.00", 15),
		"receivable, same ID":     ow.statementItem("ob.ar", stlapi.OpeningReceivable, "AR-2026-0107", "10000.00", "7000.00", "7000.00", 15),
		"payable again":           ow.statementItem("ob.ap.2", stlapi.OpeningPayable, "AP-SUP1-88", "2400.00", "1800.00", "1800.00", 20),
		"prepayment again":        ow.prepaymentItem("ob.pre.2", stlapi.OpeningCustomerPrepayment, "RCV-0311", "5000.00", "2000.00", "BOC-0311-B"),
		"supplier prepayment too": ow.prepaymentItem("ob.sup.2", stlapi.OpeningSupplierPrepayment, "PAY-0920", "900.00", "900.00", "BOC-0920"),
	} {
		_, err := ow.stl.RecordOpening(ow.treasury, again)
		if code(err) != stlapi.CodeOpeningBalanceDuplicate || path(err) != "/args/source/document" {
			t.Fatalf("%s: %s %s", name, code(err), path(err))
		}
	}
	later := ow.statementItem("ob.ar.later", stlapi.OpeningReceivable, "AR-2026-0107", "10000.00", "7000.00", "7000.00", 15)
	later.CutoffAt = kernel.FormatTimestamp(ow.cutoff.Add(24 * time.Hour))
	if _, err = ow.stl.RecordOpening(ow.treasury, later); code(err) != stlapi.CodeOpeningBalanceDuplicate {
		t.Fatalf("the same receivable at a later cutoff: %s", code(err))
	}
	_, err = ow.stl.RecordFund(ow.treasury, stlapi.RecordFund{IdempotencyKey: ow.key(), FundRecordID: "fund.dup", FundKind: stlapi.FundReceipt,
		CounterpartyID: "customer.1", Currency: stlRef(ow.usd), BankAccount: stlRef(ow.bankUSD), ReceivedAmount: "5000.00",
		ValueDate: kernel.FormatTimestamp(ow.cutoff.AddDate(0, 0, -20)), BankReference: "BOC-0311", EvidenceIDs: []string{ow.advice}})
	if code(err) != stlapi.CodeBusinessKeyConflict || path(err) != "/args/bank_reference" {
		t.Fatalf("the opening prepayment's bank movement recorded again: %s %s", code(err), path(err))
	}
	ow.fund("fund.later", stlapi.FundReceipt, "5000.00")
	_, err = ow.stl.RecordOpening(ow.treasury, ow.prepaymentItem("ob.pre.3", stlapi.OpeningCustomerPrepayment, "RCV-0999", "5000.00", "5000.00",
		"ref.fund.later"))
	if code(err) != stlapi.CodeBusinessKeyConflict {
		t.Fatalf("a bank movement recorded after go-live, again as opening: %s", code(err))
	}

	// Applications against opening items work as for any statement.
	_, err = ow.apply("app.pre", "ob.pre", stlapi.TargetReceivable, "ob.ar", "2000.00")
	ow.must(err)
	_, err = ow.apply("app.later", "fund.later", stlapi.TargetReceivable, "ob.ar", "5000.01")
	if code(err) != stlapi.CodeApplicationExceedsAvailable && code(err) != stlapi.CodeApplicationExceedsOpen {
		t.Fatalf("over the receivable: %s", code(err))
	}
	for i, amount := range []string{"2000.00", "3000.00"} { // each to the first installment still open
		_, err = ow.apply("app.later."+string(rune('1'+i)), "fund.later", stlapi.TargetReceivable, "ob.ar", amount)
		ow.must(err)
	}
	_, err = ow.apply("app.sup", "ob.sup", stlapi.TargetPayable, "ob.ap", "900.00")
	ow.must(err)
	ow.fund("pay.ap", stlapi.FundPayment, "900.00")
	_, err = ow.apply("app.pay", "pay.ap", stlapi.TargetPayable, "ob.ap", "900.00")
	ow.must(err)
	if r, err := ow.stl.GetReceivable(ow.treasury, "ob.ar"); err != nil || r.OpenMinor != "0" || r.AppliedMinor != "700000" || r.OrderID != "" {
		t.Fatalf("receivable settled after go-live %+v %v", r, err)
	}
	if got := ow.exposure(); got != "USD 0" {
		t.Fatalf("exposure once paid: %s", got)
	}

	totals := ow.totals(ow.cutoff)
	for kind, want := range map[string][5]string{
		stlapi.OpeningReceivable:         {"1000000", "300000", "700000", "700000", "0"},
		stlapi.OpeningPayable:            {"240000", "60000", "180000", "180000", "0"},
		stlapi.OpeningCustomerPrepayment: {"500000", "300000", "200000", "200000", "0"},
		stlapi.OpeningSupplierPrepayment: {"90000", "0", "90000", "90000", "0"},
	} {
		x := totals[kind]
		if got := [5]string{x.SourceAmountMinor, x.SettledBeforeMinor, x.AmountMinor, x.AppliedSinceMinor, x.OpenMinor}; got != want || x.Items != 1 ||
			x.Currency != "USD" || !x.CutoffAt.Equal(ow.cutoff) {
			t.Errorf("%s totals %+v, want %v", kind, x, want)
		}
	}
	if v := ow.opening("ob.ar"); v.Balance == nil || v.Balance.OpenMinor != "0" || len(v.Balance.Applications) != 3 || v.SettledBeforeMinor != "300000" ||
		v.OrderReference != "SO-LEGACY-42" || v.Status != stlapi.OpeningRecorded {
		t.Fatalf("opening_balance.get %+v", v)
	}
	if v := ow.opening("ob.sup"); v.FundRecord == nil || v.FundRecord.AvailableMinor != "0" || v.AppliedSinceMinor != "90000" {
		t.Fatalf("opening_balance.get of a prepayment %+v", v)
	}
}

// BT17 (期初与切换: 上线后不重建同一账款): the opening receivable AR-2026-0042 (7,000.00) is settled after go-live by a
// receipt of 7,000.00. Recording it again — the same cutoff or a later one — is OPENING_BALANCE_DUPLICATE: no second
// receivable arises for it, and the exposure stays 0.00. Only once it is reversed (settlement.reverse by a credit
// manager, after its application is unapplied: STL-INV-009) is its item reversed and may a corrected one be
// recorded — and then exactly one stands.
func TestBT17NoSecondReceivableForAnOpeningItemSettledAfterGoLive(t *testing.T) {
	ow := newOpeningWorld(t)
	ow.limit()
	ow.record(ow.statementItem("ob.42", stlapi.OpeningReceivable, "AR-2026-0042", "7000.00", "7000.00", "7000.00", 10))
	ow.fund("fund.42", stlapi.FundReceipt, "7000.00")
	_, err := ow.apply("app.42", "fund.42", stlapi.TargetReceivable, "ob.42", "7000.00")
	ow.must(err)
	for _, at := range []time.Time{ow.cutoff, ow.cutoff.Add(48 * time.Hour)} {
		again := ow.statementItem("ob.42.again", stlapi.OpeningReceivable, "AR-2026-0042", "7000.00", "7000.00", "7000.00", 10)
		again.CutoffAt = kernel.FormatTimestamp(at)
		if _, err = ow.stl.RecordOpening(ow.treasury, again); code(err) != stlapi.CodeOpeningBalanceDuplicate {
			t.Fatalf("at %s: %s", at, code(err))
		}
	}
	if got := ow.exposure(); got != "USD 0" {
		t.Fatalf("exposure after the settlement and the refused import: %s", got)
	}
	if st, err := ow.stl.GetStatement(ow.treasury, "ob.42.again"); code(err) != kernel.CodeNotFound {
		t.Fatalf("a second receivable: %+v %v", st, err)
	}

	_, err = ow.stl.ReverseSettlement(ow.manager1, stlapi.ReverseSettlement{IdempotencyKey: ow.key(), StatementID: "ob.42", ExpectedVersion: 1,
		Reason: "期初金额录入错误，冲销后重新登记"})
	if code(err) != stlapi.CodeSettlementHasApplications {
		t.Fatalf("reversal with the receipt applied: %s", code(err))
	}
	_, err = ow.stl.Unapply(ow.treasury, stlapi.Unapply{IdempotencyKey: ow.key(), ApplicationID: "app.42", ExpectedVersion: 1, Reason: "冲销前撤销核销"})
	ow.must(err)
	r, err := ow.stl.ReverseSettlement(ow.manager1, stlapi.ReverseSettlement{IdempotencyKey: ow.key(), StatementID: "ob.42", ExpectedVersion: 1,
		Reason: "期初金额录入错误，冲销后重新登记"})
	if err != nil || r.Status != stlapi.StatementReversed || len(r.Sources) != 0 {
		t.Fatalf("reversal %+v %v", r, err)
	}
	if v := ow.opening("ob.42"); v.Status != stlapi.OpeningReversed || v.OpenMinor != "0" {
		t.Fatalf("reversed item %+v", v)
	}
	ow.record(ow.statementItem("ob.42.fixed", stlapi.OpeningReceivable, "AR-2026-0042", "7000.00", "6500.00", "6500.00", 10))
	if totals := ow.totals(ow.cutoff); totals[stlapi.OpeningReceivable].Items != 1 || totals[stlapi.OpeningReceivable].AmountMinor != "650000" {
		t.Fatalf("standing receivables %+v", totals)
	}
	if got := ow.exposure(); got != "USD 650000" {
		t.Fatalf("exposure with the corrected item: %s", got)
	}
}

// SC-09 (D2's seed; STL-INV-004 with STL-INV-012's positive): customer.1's limit is 10,000.00 USD and the books
// hold an opening receivable of 7,000.00 from a prior order. A new order's credit portion of 4,000.00 takes the
// exposure to 11,000.00: credit.reserve commits a credit decision (CREDIT_LIMIT_EXCEEDED, exposure 7,000.00, excess
// 1,000.00) and reserves nothing; a credit manager releases it and the next reservation is made on the decision.
// An opening receivable 45 days overdue at the next reservation exceeds the 30 days' tolerance
// (CREDIT_OVERDUE_EXCEEDED): its installments' due instants count like any receivable's.
func TestSC09AnOpeningReceivableCountsInTheCreditExposure(t *testing.T) {
	ow := newOpeningWorld(t)
	ow.limit()
	ow.record(ow.statementItem("ob.sc09", stlapi.OpeningReceivable, "AR-SC09-PRIOR", "7000.00", "7000.00", "7000.00", 20))
	o := ow.onCredit("order.sc09", "400000")
	opened, err := ow.reserve(ow.sales, "res.sc09", o)
	ow.must(err)
	if opened.Status != stlapi.StatusDecisionOpened || !slices.Equal(opened.Reasons, []string{stlapi.CodeCreditLimitExceeded}) ||
		opened.ExposureMinor != "700000" || opened.ExcessMinor != "100000" || opened.LimitMinor != "1000000" || opened.DecisionID == "" {
		t.Fatalf("SC-09 reservation %+v", opened)
	}
	if d, err := ow.decide(ow.manager1, opened.DecisionID, 1, stlapi.DecisionRelease); err != nil || d.Status != stlapi.DecisionReleased {
		t.Fatalf("decision %+v %v", d, err)
	}
	reserved, err := ow.reserve(ow.sales, "res.sc09.2", o)
	if err != nil || reserved.Status != stlapi.ReservationReserved || reserved.Basis != stlapi.BasisCreditDecision {
		t.Fatalf("reservation on the decision %+v %v", reserved, err)
	}
	overdue := ow.statementItem("ob.late", stlapi.OpeningReceivable, "AR-SC09-LATE", "100.00", "100.00", "100.00", -45)
	ow.record(overdue)
	late, err := ow.reserve(ow.sales, "res.late", ow.onCredit("order.late", "100"))
	ow.must(err)
	if late.Status != stlapi.StatusDecisionOpened || !slices.Contains(late.Reasons, stlapi.CodeCreditOverdueExceeded) {
		t.Fatalf("with an opening receivable 45 days overdue %+v", late)
	}
}

// opening_balance.record is human only: an AI agent holding the permission is PERMISSION_DENIED and the refusal is
// audited. Two treasurers recording one source document at once: exactly one item stands, the other answer is
// OPENING_BALANCE_DUPLICATE (the partial unique index; the ledger's retry finds the item). The database refuses an
// opening item that names a statement its command did not write, and a reversal of an item whose statement stands.
func TestOpeningBalancesAreHumanOnlyAndRecordedOnce(t *testing.T) {
	ow := newOpeningWorld(t)
	_, err := ow.stl.RecordOpening(ow.agent, ow.statementItem("ob.ai", stlapi.OpeningReceivable, "AR-AI-1", "100.00", "100.00", "100.00", 10))
	if code(err) != kernel.CodePermissionDenied {
		t.Fatalf("AI agent: %s", code(err))
	}
	auditor := ow.provision(bootstrap.Grant{Scope: ow.scope("company.A"), Actor: actorRef("human", "auditor.1"), Permissions: []string{"stl.audit"},
		DataScope: "company"})
	audit, err := ow.stl.Audit(auditor)
	ow.must(err)
	if !slices.ContainsFunc(audit, func(e kernel.AuditEntry) bool {
		return e.Command == stlapi.CommandRecordOpening && e.Outcome == "rejected" && e.Code == kernel.CodePermissionDenied
	}) {
		t.Fatalf("the refusal is not audited: %+v", audit)
	}
	treasury2 := ow.assign("treasury.2", "treasury")
	for round := range 3 {
		document := "AR-RACE-" + string(rune('A'+round))
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, ctx := range []context.Context{ow.treasury, treasury2} {
			c := ow.statementItem("ob.race."+document+"."+string(rune('1'+i)), stlapi.OpeningReceivable, document, "50.00", "50.00", "50.00", 5)
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = ow.stl.RecordOpening(ctx, c)
			}()
		}
		wg.Wait()
		codes := []string{code(errs[0]), code(errs[1])}
		slices.Sort(codes)
		if !slices.Equal(codes, []string{"OK", stlapi.CodeOpeningBalanceDuplicate}) {
			t.Fatalf("round %d: %v", round, codes)
		}
	}
	if v, err := ow.stl.Openings(ow.treasury, ""); err != nil || len(v.Items) != 3 {
		t.Fatalf("three items stand: %+v %v", v, err)
	}
	ow.record(ow.statementItem("ob.db", stlapi.OpeningReceivable, "AR-DB-1", "10.00", "10.00", "10.00", 5))
	for name, sql := range map[string]string{
		"an item of a statement its command did not write": `INSERT INTO brant_stl.opening_balance (tenant_id, company_id, opening_id, kind, status,
			cutoff_at, source_document, source_issued_at, source_amount_minor, amount_minor, currency, minor_units, currency_revision, counterparty_id,
			statement_id, evidence_ids, owner_actor_id, recorded_by_kind, recorded_by_id, record_epoch, recorded_at, updated_at)
			SELECT tenant_id, company_id, 'ob.db', 'receivable', 'recorded', now() - interval '1 day', 'AR-DB-2', now() - interval '2 days', 1000, 1000,
			'USD', 2, 1, 'customer.1', 'ob.db', ARRAY['x'], 'treasury.1', 'human', 'treasury.1', 1, now(), now() FROM brant_stl.settlement_statement
			WHERE statement_id = 'ob.db'`,
		"a reversal of an item whose statement stands": `UPDATE brant_stl.opening_balance SET status = 'reversed' WHERE opening_id = 'ob.db'`,
		"a line on an opening statement": `INSERT INTO brant_stl.settlement_line SELECT tenant_id, company_id, 'ob.db', 1, 'shipment_line', 'ship.x', 'S1',
			1, 'order.x', 1, repeat('a', 64), 'L1', NULL, NULL, 'sku.1', 1, 'PCS', 1, 1, 1, NULL, 1000, 0, 1000, 0, 1000, now(), 'settled'
			FROM brant_stl.settlement_statement WHERE statement_id = 'ob.db'`,
		"an item deleted": `DELETE FROM brant_stl.opening_balance WHERE opening_id = 'ob.db'`,
	} {
		if err := ow.exec("company.A", sql); err == nil {
			t.Errorf("%s: committed", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
}

// The composed process over HTTP with OIDC access tokens: the treasurer records an opening receivable through
// STL's command route and reconciles it; the same document again is 409 OPENING_BALANCE_DUPLICATE; an AI agent is
// 403; a member of the other kind is a schema refusal naming it; GET opening_balances/{id} and the list of the
// cutoff answer the item with its totals.
func TestOpeningBalancesOverHTTP(t *testing.T) {
	ow := newOpeningWorld(t)
	is := newIssuer(t)
	handler, err := ow.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	ow.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	treasurer := &client{t: t, base: srv.URL, token: is.token(ow.tenant, "company.A", "treasury.1", "human"), n: 300}
	agent := &client{t: t, base: srv.URL, token: is.token(ow.tenant, "company.A", "agent.opening", "ai"), n: 500}
	cutoff := kernel.FormatTimestamp(ow.cutoff)
	args := map[string]any{"opening_id": "ob.http", "kind": "receivable", "cutoff_at": cutoff,
		"source":          map[string]any{"document": "AR-HTTP-7000", "issued_at": kernel.FormatTimestamp(ow.cutoff.AddDate(0, -1, 0)), "amount": "10000.00"},
		"counterparty_id": "customer.1", "order_reference": "SO-LEGACY-42",
		"currency": map[string]any{"kind": "currency", "id": ow.usd.ConfigID, "revision": ow.usd.Revision}, "amount": "7000.00",
		"installments": []any{map[string]any{"amount": "7000.00", "due_at": kernel.FormatTimestamp(time.Now().Add(15 * 24 * time.Hour))}},
		"evidence_ids": []any{ow.ledger}}
	body := map[string]any{"contract": stlapi.Contract, "command": stlapi.CommandRecordOpening, "idempotency_key": "stl.http.opening", "args": args}
	if status, out := agent.do("POST", "/api/brant/v1/stl/commands/opening_balance.record", body); status != http.StatusForbidden ||
		out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI agent: %d %v", status, out)
	}
	status, res := treasurer.do("POST", "/api/brant/v1/stl/commands/opening_balance.record", body)
	if status != http.StatusOK || res["object_id"] != "ob.http" || res["status"] != "recorded" || res["row_version"] != float64(1) {
		t.Fatalf("record %d %v", status, res)
	}
	status, rec := treasurer.do("POST", "/api/brant/v1/stl/commands/opening_balance.record/reconcile", body)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != res["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	again := map[string]any{"contract": stlapi.Contract, "command": stlapi.CommandRecordOpening, "idempotency_key": "stl.http.opening.2", "args": args}
	if status, out := treasurer.do("POST", "/api/brant/v1/stl/commands/opening_balance.record", again); status != http.StatusConflict ||
		out["code"] != stlapi.CodeOpeningBalanceDuplicate {
		t.Fatalf("again: %d %v", status, out)
	}
	args["bank_reference"] = "BOC-1"
	bad := map[string]any{"contract": stlapi.Contract, "command": stlapi.CommandRecordOpening, "idempotency_key": "stl.http.opening.3", "args": args}
	if status, out := treasurer.do("POST", "/api/brant/v1/stl/commands/opening_balance.record", bad); status != http.StatusUnprocessableEntity ||
		out["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("a receivable with a bank reference: %d %v", status, out)
	}
	status, v := treasurer.do("GET", "/api/brant/v1/stl/opening_balances/ob.http", nil)
	if status != http.StatusOK || v["kind"] != "receivable" || v["settled_before_minor"] != "300000" || v["open_minor"] != "700000" ||
		v["statement_id"] != "ob.http" || v["source_document"] != "AR-HTTP-7000" {
		t.Fatalf("opening_balance.get %d %v", status, v)
	}
	status, list := treasurer.do("GET", "/api/brant/v1/stl/opening_balances?cutoff_at="+cutoff, nil)
	items, _ := list["items"].([]any)
	totals, _ := list["totals"].([]any)
	if status != http.StatusOK || len(items) != 1 || len(totals) != 1 || totals[0].(map[string]any)["amount_minor"] != "700000" {
		t.Fatalf("opening_balance.list %d %v", status, list)
	}
	if status, out := treasurer.do("GET", "/api/brant/v1/stl/opening_balances?cutoff_at=yesterday", nil); status != http.StatusUnprocessableEntity ||
		out["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("a cutoff that is no timestamp: %d %v", status, out)
	}
}
