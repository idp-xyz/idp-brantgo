//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
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

// The STL funds suite (WBS S5-04a and the STL half of S5-07; tactical model STL-AGG-FundRecord,
// STL-AGG-Application, STL-AGG-OrderAccount; SM-STL-FUND, SM-STL-APPLICATION; STL-INV-002,
// STL-INV-003, STL-INV-006; Context Map R45 PaymentConditionGuard v1, R38 FundRecordRef v1; S5 plan
// D-3, D-4, D-5, D-16; baseline 4.6 S-A, CN-R10, CN-AT07/08/09/15; SC-08) on PostgreSQL 16. Python
// V0.50 has no STL, so the tests cite design IDs only.
//
// The funds world runs STL with a stand-in for TRD's OrderCommercialSnapshot v1 (STL's port Orders,
// as the credit world does), so a test can give an active order any payment plan;
// stl_funds_http_integration_test.go runs the composed STL against an order TRD activated. POL holds
// USD and CNY and three bank accounts: the company's USD account (receipts and payments), its CNY
// account (receipts) and a USD account of the party agent.9. treasury.1 records funds and applies
// them (treasury); manager.1 resolves differences (credit_manager); treasury.2 holds both templates,
// to show that the recorder never resolves its own difference; agent.funds is an AI agent holding
// the fund permissions, refused because the commands are human only; system:inbox.stl opens order
// accounts.

type fundWorld struct {
	*world
	stl                                     *stl.Module
	orders                                  *creditOrders
	editor, reviewer                        context.Context
	treasury, treasury2, manager, agent     context.Context
	system                                  context.Context
	usd, cny, bankUSD, bankCNY, bankOfAgent polapi.ConfigRef
	advice                                  string
}

func bank(owner, id, currency, purpose string) polapi.ConfigData {
	return polapi.ConfigData{BankAccount: &polapi.BankAccount{OwnerKind: owner, OwnerID: id, BankName: "synthetic bank", AccountNumber: "6222000011112222",
		Currency: currency, Purpose: purpose}}
}

func newFundWorld(t *testing.T) *fundWorld {
	t.Helper()
	w := newWorld(t)
	editor, reviewer := w.polActors()
	fw := &fundWorld{world: w, orders: &creditOrders{orders: map[string]stl.Order{}}, editor: editor, reviewer: reviewer}
	win := newWindow()
	fw.usd, fw.cny = w.currency(editor, reviewer, "USD", win), w.currency(editor, reviewer, "CNY", win)
	fw.bankUSD = w.configure(editor, reviewer, polapi.KindBankAccount, "bank.usd", 0, win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "USD", polapi.AccountBoth), true)
	fw.bankCNY = w.configure(editor, reviewer, polapi.KindBankAccount, "bank.cny", 0, win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "CNY", polapi.AccountReceipt), true)
	for _, p := range []string{"customer.1", "customer.2", "agent.9"} {
		w.party(editor, p, true, "evidence")
	}
	fw.bankOfAgent = w.configure(editor, reviewer, polapi.KindBankAccount, "bank.agent", 0, win.from, time.Time{},
		bank(polapi.OwnerParty, "agent.9", "USD", polapi.AccountBoth), true)
	fw.advice = w.source(editor, "advice.1", "合成银行入账通知：汇入行扣费说明")
	fw.treasury, fw.manager = w.assign("treasury.1", "treasury"), w.assign("manager.1", "credit_manager")
	fw.treasury2 = w.assign("treasury.2", "treasury", "credit_manager")
	fw.agent = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.funds"},
		Permissions: []string{"fund.record", "fund.resolve_difference", "application.apply", "application.unapply", "stl.read", "evd.read",
			"pty.read", "pol.read"}, DataScope: "company"})
	fw.system = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "system:inbox.stl"},
		Permissions: []string{"payment_plan_review.open", "stl.read"}, DataScope: "company"})
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	fw.stl, err = stl.New(db, stl.Upstream{Orders: fw.orders, Configs: w.c.POL, Parties: w.c.PTY, Sources: w.c.EVD})
	w.must(err)
	return fw
}

// activeOrder gives TRD's stand-in an active order of customer at revision 1 in currency with
// stages, and has the system open its account.
func (fw *fundWorld) activeOrder(id, customer string, currency polapi.ConfigRef, stages ...stl.OrderStage) stl.Order {
	fw.t.Helper()
	o := stl.Order{Binding: stl.OrderBinding{OrderID: id, OrderRevision: 1, OrderHash: orderHashOf(id, 1)}, Status: stl.OrderActive,
		CustomerID: customer, Currency: stl.CurrencyRef{ID: currency.ConfigID, Revision: currency.Revision}, Stages: stages}
	fw.orders.put(o)
	_, err := fw.stl.OpenOrderAccount(fw.system, fw.openCmd(o))
	fw.must(err)
	return o
}

func (fw *fundWorld) openCmd(o stl.Order) stlapi.OpenOrderAccount {
	return stlapi.OpenOrderAccount{IdempotencyKey: fw.key(), OrderID: o.Binding.OrderID, OrderRevision: o.Binding.OrderRevision,
		OrderHash: o.Binding.OrderHash}
}

func payStage(id, trigger, amountMinor string) stl.OrderStage {
	return stl.OrderStage{StageID: id, Trigger: trigger, Percent: "0", AmountMinor: amountMinor}
}

// receiptCmd records id: a USD receipt from customer.1 on the company's USD account.
func (fw *fundWorld) receiptCmd(id, received string) stlapi.RecordFund {
	return stlapi.RecordFund{IdempotencyKey: fw.key(), FundRecordID: id, FundKind: stlapi.FundReceipt, CounterpartyID: "customer.1",
		Currency: stlRef(fw.usd), BankAccount: stlRef(fw.bankUSD), ReceivedAmount: received, ValueDate: kernel.FormatTimestamp(time.Now()),
		BankReference: "ref." + id, EvidenceIDs: []string{fw.advice}}
}

func (fw *fundWorld) receipt(id, received, remitted string) stlapi.FundRecordReceipt {
	fw.t.Helper()
	cmd := fw.receiptCmd(id, received)
	cmd.RemittedAmount = remitted
	r, err := fw.stl.RecordFund(fw.treasury, cmd)
	fw.must(err)
	return r
}

func (fw *fundWorld) applyCmd(id, fund, order, stageID, amount string) stlapi.Apply {
	return stlapi.Apply{IdempotencyKey: fw.key(), ApplicationID: id, FundRecordID: fund,
		Target: stlapi.ApplicationTarget{Kind: stlapi.TargetOrderStage, OrderID: order, StageID: stageID}, Amount: amount}
}

func (fw *fundWorld) apply(id, fund, order, stageID, amount string) (stlapi.ApplicationReceipt, error) {
	return fw.stl.Apply(fw.treasury, fw.applyCmd(id, fund, order, stageID, amount))
}

func (fw *fundWorld) unapply(id string, expected int64) (stlapi.ApplicationReceipt, error) {
	return fw.stl.Unapply(fw.treasury, stlapi.Unapply{IdempotencyKey: fw.key(), ApplicationID: id, ExpectedVersion: expected, Reason: "客户退汇，撤销核销"})
}

func (fw *fundWorld) resolve(ctx context.Context, fund string, expected int64, amount string) (stlapi.FundRecordReceipt, error) {
	return fw.stl.ResolveFundDifference(ctx, stlapi.ResolveFundDifference{IdempotencyKey: fw.key(), FundRecordID: fund, ExpectedVersion: expected,
		Resolution: stlapi.ResolutionBankCharge, Amount: amount, EvidenceIDs: []string{fw.advice}, Reason: "中间行扣费，经批准由我方承担"})
}

// paymentGuard calls PaymentConditionGuard v1 in a transaction of the downstream with its own executor.
func (d *downstream) paymentGuard(q stlapi.PaymentQuery) (stlapi.PaymentVerdict, error) {
	var v stlapi.PaymentVerdict
	err := d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = stlapi.ScanPaymentCondition(q, exec.QueryRow(ctx, stlapi.PaymentConditionSQL, stlapi.PaymentConditionArgs(q)...).Scan)
		return err
	})
	return v, err
}

func (fw *fundWorld) guard(d *downstream, order string, revision int64, scope string) stlapi.PaymentVerdict {
	fw.t.Helper()
	v, err := d.paymentGuard(stlapi.PaymentQuery{OrderID: order, OrderRevision: revision, Scope: scope, At: time.Now()})
	fw.must(err)
	return v
}

var errPaymentRefused = errors.New("downstream: payment condition not met")

// release is FUL's commercial release as unit C1 wires it: lock its own root FOR UPDATE, guard the
// order's payment condition for the scope, then release and record the verdict, in one transaction.
func (d *downstream) release(quoteID string, q stlapi.PaymentQuery, s stage) (stlapi.PaymentVerdict, error) {
	var v stlapi.PaymentVerdict
	err := d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		if s.pid != nil {
			var pid int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			s.pid <- pid
		}
		t, c := string(d.scope.Tenant), string(d.scope.Company)
		var status string
		if err := exec.QueryRow(ctx, "SELECT status FROM "+d.quotes+" WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 FOR UPDATE",
			t, c, quoteID).Scan(&status); err != nil {
			return err
		}
		var err error
		if v, err = stlapi.ScanPaymentCondition(q, exec.QueryRow(ctx, stlapi.PaymentConditionSQL, stlapi.PaymentConditionArgs(q)...).Scan); err != nil {
			return err
		}
		if err = v.Check("/args/shipment_id"); err != nil {
			return errors.Join(errPaymentRefused, err)
		}
		if s.hold != nil {
			s.hold()
		}
		if _, err = exec.Exec(ctx, "UPDATE "+d.quotes+" SET status = 'active', root_version = root_version + 1 WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3",
			t, c, quoteID); err != nil {
			return err
		}
		_, err = exec.Exec(ctx, "INSERT INTO "+d.adopted+" (tenant_id, company_id, quote_id, kind, config_id, revision, content_hash) VALUES ($1, $2, $3, 'payment', $4, $5, $6)",
			t, c, quoteID, q.OrderID, v.OrderRevision, v.OrderHash)
		return err
	})
	return v, err
}

// SM-STL-FUND, STL-INV-006, D-16 and SC-08's money: the customer remitted 672.00 and the bank
// credited 670.00 with 2.00 charges stated; the difference stays unexplained (available 670.00);
// AI agents are refused (human only); the recorder may not resolve its own difference
// (INDEPENDENT_REVIEW_REQUIRED); a credit manager resolves it as our bank charge, and 672.00 is
// available. Unknown counterparty or evidence, another entity's account (CN-AT15 payee half), a
// bank reference recorded twice and a non-receipt with a remitted amount are refused.
func TestFundRecordsKeepRemittedReceivedAndChargesApart(t *testing.T) {
	fw := newFundWorld(t)
	cmd := fw.receiptCmd("fund.sc08", "670.00")
	cmd.RemittedAmount, cmd.BankCharges = "672.00", "2.00"
	_, err := fw.stl.RecordFund(fw.agent, cmd)
	fw.want(err, kernel.CodePermissionDenied)
	r, err := fw.stl.RecordFund(fw.treasury, cmd)
	fw.must(err)
	if r.Status != stlapi.FundRecorded || r.RowVersion != 1 || r.FundKind != stlapi.FundReceipt || r.ReceivedMinor != "67000" ||
		r.DifferenceMinor != "200" || r.UnexplainedMinor != "200" || r.AvailableMinor != "67000" || r.Currency != "USD" {
		t.Fatalf("recorded %+v", r)
	}
	if again, err := fw.stl.RecordFund(fw.treasury, cmd); err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("replay %+v %v", again, err)
	}
	for name, edit := range map[string]func(*stlapi.RecordFund){
		"counterparty": func(c *stlapi.RecordFund) { c.CounterpartyID = "party.none" },
		"evidence":     func(c *stlapi.RecordFund) { c.EvidenceIDs = []string{"advice.none"} },
	} {
		c := fw.receiptCmd("fund.bad."+name, "1.00")
		edit(&c)
		_, err = fw.stl.RecordFund(fw.treasury, c)
		if code(err) != kernel.CodeReferenceInvalid {
			t.Fatalf("%s: %v", name, err)
		}
	}
	other := fw.receiptCmd("fund.agent", "100.00")
	other.BankAccount = stlRef(fw.bankOfAgent)
	_, err = fw.stl.RecordFund(fw.treasury, other)
	fw.want(err, stlapi.CodePartyEntityMismatch)
	dup := fw.receiptCmd("fund.dup", "1.00")
	dup.BankReference = cmd.BankReference
	_, err = fw.stl.RecordFund(fw.treasury, dup)
	fw.want(err, stlapi.CodeBusinessKeyConflict)
	pay := fw.receiptCmd("fund.pay", "1.00")
	pay.FundKind, pay.RemittedAmount = stlapi.FundPayment, "1.00"
	_, err = fw.stl.RecordFund(fw.treasury, pay)
	fw.want(err, kernel.CodeSchemaInvalid)
	cents := fw.receiptCmd("fund.cents", "1.001")
	_, err = fw.stl.RecordFund(fw.treasury, cents)
	fw.want(err, kernel.CodeInvalidKnownValue)

	_, err = fw.resolve(fw.treasury, "fund.sc08", 1, "2.00")
	fw.want(err, kernel.CodePermissionDenied) // treasury does not hold fund.resolve_difference (D-16)
	_, err = fw.resolve(fw.agent, "fund.sc08", 1, "2.00")
	fw.want(err, kernel.CodePermissionDenied)
	own := fw.receiptCmd("fund.own", "99.00")
	own.RemittedAmount = "100.00"
	_, err = fw.stl.RecordFund(fw.treasury2, own)
	fw.must(err)
	_, err = fw.resolve(fw.treasury2, "fund.own", 1, "1.00")
	fw.want(err, stlapi.CodeIndependentReviewRequired)
	_, err = fw.resolve(fw.manager, "fund.sc08", 1, "2.01")
	fw.want(err, kernel.CodeInvalidKnownValue)
	_, err = fw.resolve(fw.manager, "fund.sc08", 2, "2.00")
	fw.want(err, kernel.CodeVersionConflict)
	res, err := fw.resolve(fw.manager, "fund.sc08", 1, "2.00")
	fw.must(err)
	if res.Status != stlapi.FundDifferenceResolved || res.RowVersion != 2 || res.UnexplainedMinor != "0" || res.AvailableMinor != "67200" {
		t.Fatalf("resolved %+v", res)
	}
	_, err = fw.resolve(fw.manager, "fund.sc08", 2, "0.01")
	fw.want(err, kernel.CodeInvalidState)

	v, err := fw.stl.GetFundRecord(fw.manager, "fund.sc08")
	fw.must(err)
	if v.RemittedMinor != "67200" || v.ReceivedMinor != "67000" || v.BankChargesMinor != "200" || v.ResolvedMinor != "200" || len(v.Resolutions) != 1 ||
		v.Resolutions[0].ResolvedBy.ID != "manager.1" || v.RecordedBy.ID != "treasury.1" || v.BankAccount.ConfigID != "bank.usd" || v.Direction != "in" {
		t.Fatalf("fund record %+v", v)
	}
	_, err = fw.stl.GetFundRecord(fw.manager, "fund.none")
	fw.want(err, kernel.CodeNotFound)
	events := fw.events("company.A", stlapi.Source)
	if !hasEvent(events, stlapi.EventFundRecorded) || !hasEvent(events, stlapi.EventFundDifferenceResolved) {
		t.Fatalf("events %+v", events)
	}
	// The database holds what the rules hold, whatever a command does: a version moved without a
	// resolution fails the deferred balance check, the amounts are not the application's to change.
	if err := fw.exec("company.A", `UPDATE brant_stl.fund_record SET version = version + 1, status = 'difference_resolved'
		WHERE fund_record_id = 'fund.sc08'`); err == nil {
		t.Fatal("a version without its resolution committed")
	}
	if err := fw.exec("company.A", `UPDATE brant_stl.fund_record SET received_minor = 67200 WHERE fund_record_id = 'fund.sc08'`); err == nil {
		t.Fatal("the received amount changed")
	}
}

// STL-INV-002, STL-INV-003, D-4, SC-08 and PL-10: the order account holds the stages; the guard
// answers per scope; applications stay within what the receipt has available (the unexplained
// 2.00 never counts: FUND_DIFFERENCE_UNEXPLAINED) and what the stage has open; after the credit
// manager resolves the 2.00 as our bank charge, the last 2.00 applies and the shipment scope is
// satisfied. The payer must be the customer (PARTY_ENTITY_MISMATCH), the currency the order's
// (FX_SCOPE_MISMATCH); unapply re-judges and a re-application restores without new cash; a moved
// order revision is REFERENCE_STALE and revision_mismatch, an order no longer active ORDER_NOT_ACTIVE.
func TestApplicationsDecideThePaymentCondition(t *testing.T) {
	fw := newFundWorld(t)
	d := newDownstream(fw.world)
	o := stl.Order{Binding: stl.OrderBinding{OrderID: "order.sc08", OrderRevision: 1, OrderHash: orderHashOf("order.sc08", 1)}, Status: stl.OrderActive,
		CustomerID: "customer.1", Currency: stl.CurrencyRef{ID: fw.usd.ConfigID, Revision: fw.usd.Revision},
		Stages: []stl.OrderStage{payStage("P1", "order_confirmed", "28800"), payStage("P2", "before_shipment", "67200")}}
	fw.orders.put(o)
	fw.receipt("fund.288", "288.00", "")
	_, err := fw.apply("app.early", "fund.288", "order.sc08", "P1", "288.00")
	fw.want(err, kernel.CodeInvalidState) // no account yet
	if v := fw.guard(d, "order.sc08", 1, stlapi.ScopeShipment); v.Status != stlapi.PaymentBlocked || v.Reason != stlapi.PaymentNoAccount ||
		v.Code != stlapi.CodeCommercialReleaseBlocked {
		t.Fatalf("no account %+v", v)
	}
	_, err = fw.stl.OpenOrderAccount(fw.agent, fw.openCmd(o))
	fw.want(err, kernel.CodePermissionDenied)
	stale := fw.openCmd(o)
	stale.OrderRevision = 2
	_, err = fw.stl.OpenOrderAccount(fw.treasury, stale)
	fw.want(err, stlapi.CodeReferenceStale)
	opened, err := fw.stl.OpenOrderAccount(fw.treasury, fw.openCmd(o))
	fw.must(err)
	if opened.Status != stlapi.AccountOpen || opened.Stages != 2 || opened.OrderRevision != 1 || opened.PartyID != "customer.1" || opened.Currency != "USD" {
		t.Fatalf("opened %+v", opened)
	}
	if again, err := fw.stl.OpenOrderAccount(fw.system, fw.openCmd(o)); err != nil || !again.Replayed {
		t.Fatalf("opened again %+v %v", again, err)
	}
	if v := fw.guard(d, "order.sc08", 1, stlapi.ScopeShipment); v.Reason != stlapi.PaymentStageUnpaid || v.StageID != "P1" || v.ShortfallMinor != "28800" ||
		v.RequiredMinor != "96000" || v.AppliedMinor != "0" || v.Check("/x") == nil {
		t.Fatalf("nothing applied %+v", v)
	}

	_, err = fw.stl.Apply(fw.agent, fw.applyCmd("app.ai", "fund.288", "order.sc08", "P1", "288.00"))
	fw.want(err, kernel.CodePermissionDenied)
	a1, err := fw.apply("app.p1", "fund.288", "order.sc08", "P1", "288.00")
	fw.must(err)
	if a1.Status != stlapi.ApplicationApplied || a1.RowVersion != 1 || a1.FundAvailableMinor != "0" || a1.StageOpenMinor != "0" || a1.AmountMinor != "28800" {
		t.Fatalf("applied %+v", a1)
	}
	_, err = fw.apply("app.p1", "fund.288", "order.sc08", "P1", "1.00")
	fw.want(err, kernel.CodeInvalidState) // the ID is taken

	r := fw.receiptCmd("fund.670", "670.00")
	r.RemittedAmount = "672.00"
	_, err = fw.stl.RecordFund(fw.treasury, r)
	fw.must(err)
	_, err = fw.apply("app.full", "fund.670", "order.sc08", "P2", "672.00")
	fw.want(err, stlapi.CodeFundDifferenceUnexplained)
	_, err = fw.apply("app.over", "fund.670", "order.sc08", "P2", "672.01")
	fw.want(err, stlapi.CodeApplicationExceedsAvailable)
	_, err = fw.apply("app.p2", "fund.670", "order.sc08", "P2", "670.00")
	fw.must(err)
	v := fw.guard(d, "order.sc08", 1, stlapi.ScopeShipment)
	if v.Status != stlapi.PaymentBlocked || v.StageID != "P2" || v.StageDueMinor != "67200" || v.StageAppliedMinor != "67000" || v.ShortfallMinor != "200" {
		t.Fatalf("2.00 unexplained %+v", v)
	}
	_, err = fw.resolve(fw.manager, "fund.670", 1, "2.00")
	fw.must(err)
	_, err = fw.apply("app.p2b", "fund.670", "order.sc08", "P2", "2.00")
	fw.must(err)
	if v = fw.guard(d, "order.sc08", 1, stlapi.ScopeShipment); !v.Satisfied() || v.AppliedMinor != "96000" || v.Check("/x") != nil {
		t.Fatalf("SC-08 after the resolution %+v", v)
	}
	if v = fw.guard(d, "order.sc08", 1, stlapi.ScopeDocuments); !v.Satisfied() {
		t.Fatalf("documents %+v", v)
	}

	fw.receipt("fund.big", "1000.00", "")
	_, err = fw.apply("app.open", "fund.big", "order.sc08", "P1", "0.01")
	fw.want(err, stlapi.CodeApplicationExceedsOpen)
	agent := fw.receiptCmd("fund.payer", "100.00")
	agent.CounterpartyID = "agent.9"
	_, err = fw.stl.RecordFund(fw.treasury, agent)
	fw.must(err)
	_, err = fw.apply("app.payer", "fund.payer", "order.sc08", "P1", "1.00")
	fw.want(err, stlapi.CodePartyEntityMismatch)
	cny := fw.receiptCmd("fund.cny", "100.00")
	cny.Currency, cny.BankAccount = stlRef(fw.cny), stlRef(fw.bankCNY)
	_, err = fw.stl.RecordFund(fw.treasury, cny)
	fw.must(err)
	_, err = fw.apply("app.cny", "fund.cny", "order.sc08", "P1", "1.00")
	fw.want(err, stlapi.CodeFXScopeMismatch)

	// Unapply re-judges (STL-INV-003); the same receipt re-applies without new cash (STL-INV-002).
	_, err = fw.unapply("app.p1", 2)
	fw.want(err, kernel.CodeVersionConflict)
	u, err := fw.unapply("app.p1", 1)
	fw.must(err)
	if u.Status != stlapi.ApplicationUnapplied || u.RowVersion != 2 || u.FundAvailableMinor != "28800" || u.StageOpenMinor != "28800" {
		t.Fatalf("unapplied %+v", u)
	}
	_, err = fw.unapply("app.p1", 2)
	fw.want(err, kernel.CodeInvalidState)
	if v = fw.guard(d, "order.sc08", 1, stlapi.ScopeShipment); v.Satisfied() || v.StageID != "P1" {
		t.Fatalf("after unapply %+v", v)
	}
	_, err = fw.apply("app.p1b", "fund.288", "order.sc08", "P1", "288.00")
	fw.must(err)
	if v = fw.guard(d, "order.sc08", 1, stlapi.ScopeShipment); !v.Satisfied() {
		t.Fatalf("re-applied %+v", v)
	}
	acc, err := fw.stl.GetOrderAccount(fw.treasury, "order.sc08")
	fw.must(err)
	if len(acc.Stages) != 2 || acc.Stages[1].AppliedMinor != "67200" || acc.Stages[1].OpenMinor != "0" || len(acc.Applications) != 4 ||
		len(acc.Conditions) != 2 || acc.Conditions[0].Status != stlapi.PaymentSatisfied || acc.Conditions[1].Scope != stlapi.ScopeDocuments {
		t.Fatalf("order account %+v", acc)
	}
	app, err := fw.stl.GetApplication(fw.treasury, "app.p1")
	fw.must(err)
	if app.Status != stlapi.ApplicationUnapplied || app.UnappliedBy.ID != "treasury.1" || app.UnapplyReason == "" || app.AmountMinor != "28800" {
		t.Fatalf("application %+v", app)
	}

	// TRD amends the order (revision 2): STL's account waits for its payment plan review (C4b).
	moved := o
	moved.Binding = stl.OrderBinding{OrderID: "order.sc08", OrderRevision: 2, OrderHash: orderHashOf("order.sc08", 2)}
	fw.orders.put(moved)
	_, err = fw.apply("app.moved", "fund.big", "order.sc08", "P1", "1.00")
	fw.want(err, stlapi.CodeReferenceStale)
	if v = fw.guard(d, "order.sc08", 2, stlapi.ScopeShipment); v.Reason != stlapi.PaymentRevisionMismatch || v.OrderRevision != 1 || v.Check("/x") == nil {
		t.Fatalf("revision 2 %+v", v)
	}
	moved2 := fw.openCmd(moved)
	_, err = fw.stl.OpenOrderAccount(fw.treasury, moved2)
	fw.want(err, kernel.CodeInvalidState)
	fw.orders.put(o)
	fw.orders.setStatus("order.sc08", "cancelled")
	_, err = fw.apply("app.cancelled", "fund.big", "order.sc08", "P1", "1.00")
	fw.want(err, stlapi.CodeOrderNotActive)
	events := fw.events("company.A", stlapi.Source)
	if !hasEvent(events, stlapi.EventApplied) || !hasEvent(events, stlapi.EventUnapplied) {
		t.Fatalf("events %+v", events)
	}
}

// D-5: document-controlled post-shipment stages count for scope documents only; an O/A stage for
// neither (SIM-01's 30 % deposit and 70 % against B/L copy with the original B/L held).
func TestDocumentControlledStagesGateDocumentsOnly(t *testing.T) {
	fw := newFundWorld(t)
	d := newDownstream(fw.world)
	docs := payStage("P2", "after_shipment", "54600")
	docs.ReleaseControl = "documents"
	fw.activeOrder("order.sim01", "customer.1", fw.usd, payStage("P1", "order_confirmed", "23400"), docs, payStage("P3", "after_invoice", "0"))
	fw.receipt("fund.dep", "234.00", "")
	_, err := fw.apply("app.dep", "fund.dep", "order.sim01", "P1", "234.00")
	fw.must(err)
	if v := fw.guard(d, "order.sim01", 1, stlapi.ScopeShipment); !v.Satisfied() || v.RequiredMinor != "23400" {
		t.Fatalf("shipment %+v", v)
	}
	if v := fw.guard(d, "order.sim01", 1, stlapi.ScopeDocuments); v.Satisfied() || v.StageID != "P2" || v.StageTrigger != "after_shipment" ||
		v.ShortfallMinor != "54600" {
		t.Fatalf("documents before the balance %+v", v)
	}
	fw.receipt("fund.bal", "546.00", "")
	_, err = fw.apply("app.bal", "fund.bal", "order.sim01", "P2", "546.00")
	fw.must(err)
	if v := fw.guard(d, "order.sim01", 1, stlapi.ScopeDocuments); !v.Satisfied() || v.RequiredMinor != "78000" {
		t.Fatalf("documents %+v", v)
	}
	fw.activeOrder("order.oa", "customer.1", fw.usd, payStage("P1", "after_shipment", "96000"))
	if v := fw.guard(d, "order.oa", 1, stlapi.ScopeDocuments); !v.Satisfied() || v.RequiredMinor != "0" {
		t.Fatalf("O/A %+v", v)
	}
	if _, err := d.paymentGuard(stlapi.PaymentQuery{OrderID: "order.oa", OrderRevision: 1, Scope: "delivery", At: time.Now()}); err == nil {
		t.Fatal("an unknown scope")
	}
}

// CN-AT07: one remittance of 1,000.00 for two orders, the bank credits 980.00 and states 15.00
// charges; the credit manager resolves 15.00 as our bank charge with its basis; 600.00 and 395.00
// apply; the remaining 5.00 stays unexplained and cannot be applied (FUND_DIFFERENCE_UNEXPLAINED);
// applications and the original-currency fund are conserved (980.00 + 15.00 = 995.00 applied).
func TestCombinedPaymentForTwoOrders(t *testing.T) {
	fw := newFundWorld(t)
	fw.activeOrder("order.a", "customer.1", fw.usd, payStage("P1", "before_shipment", "60000"))
	fw.activeOrder("order.b", "customer.1", fw.usd, payStage("P1", "before_shipment", "40000"))
	cmd := fw.receiptCmd("fund.combined", "980.00")
	cmd.RemittedAmount, cmd.BankCharges = "1000.00", "15.00"
	_, err := fw.stl.RecordFund(fw.treasury, cmd)
	fw.must(err)
	r, err := fw.resolve(fw.manager, "fund.combined", 1, "15.00")
	fw.must(err)
	if r.UnexplainedMinor != "500" || r.AvailableMinor != "99500" {
		t.Fatalf("after the charge %+v", r)
	}
	_, err = fw.apply("app.a", "fund.combined", "order.a", "P1", "600.00")
	fw.must(err)
	_, err = fw.apply("app.b", "fund.combined", "order.b", "P1", "400.00")
	fw.want(err, stlapi.CodeFundDifferenceUnexplained)
	b, err := fw.apply("app.b", "fund.combined", "order.b", "P1", "395.00")
	fw.must(err)
	if b.FundAvailableMinor != "0" || b.StageOpenMinor != "500" {
		t.Fatalf("second order %+v", b)
	}
	refs, err := fw.stl.ResolveFundRecords(fw.manager, []string{"fund.combined", "fund.none"})
	fw.must(err)
	ref, ok := refs["fund.combined"]
	if !ok || len(refs) != 1 || ref.AppliedMinor != "99500" || ref.UnexplainedMinor != "500" || ref.ResolvedMinor != "1500" || len(ref.Allocations) != 2 ||
		ref.Allocations[0].OrderID != "order.a" || ref.AvailableMinor != "0" || ref.CashMinor != "98000" {
		t.Fatalf("FundRecordRef v1 %+v", refs)
	}
}

// CN-AT08 and CN-R10: a payer other than the customer stays pending (PARTY_ENTITY_MISMATCH); a
// refund is its own fund record drawn on the receipt, never more than is unapplied
// (APPLICATION_EXCEEDS_AVAILABLE until the application is unapplied, which re-judges the payment
// condition); a returned remittance likewise; a non-goods receipt is never applied.
func TestRefundsReturnsAndPayers(t *testing.T) {
	fw := newFundWorld(t)
	d := newDownstream(fw.world)
	fw.activeOrder("order.r", "customer.1", fw.usd, payStage("P1", "before_shipment", "50000"))
	fw.receipt("fund.r", "500.00", "")
	_, err := fw.apply("app.r", "fund.r", "order.r", "P1", "500.00")
	fw.must(err)
	refund := stlapi.RecordFund{IdempotencyKey: fw.key(), FundRecordID: "refund.r", FundKind: stlapi.FundRefund, SourceFundRecordID: "fund.r",
		Currency: stlRef(fw.usd), BankAccount: stlRef(fw.bankUSD), ReceivedAmount: "100.00", ValueDate: kernel.FormatTimestamp(time.Now()),
		BankReference: "ref.refund.r", EvidenceIDs: []string{fw.advice}}
	_, err = fw.stl.RecordFund(fw.treasury, refund)
	fw.want(err, stlapi.CodeApplicationExceedsAvailable)
	_, err = fw.unapply("app.r", 1)
	fw.must(err)
	if v := fw.guard(d, "order.r", 1, stlapi.ScopeShipment); v.Satisfied() {
		t.Fatalf("after unapply %+v", v)
	}
	refund.IdempotencyKey = fw.key()
	rr, err := fw.stl.RecordFund(fw.treasury, refund)
	fw.must(err)
	if rr.FundKind != stlapi.FundRefund || rr.AvailableMinor != "10000" {
		t.Fatalf("refund %+v", rr)
	}
	_, err = fw.apply("app.refund", "refund.r", "order.r", "P1", "1.00")
	fw.want(err, kernel.CodeInvalidState)
	_, err = fw.apply("app.r2", "fund.r", "order.r", "P1", "400.01")
	fw.want(err, stlapi.CodeApplicationExceedsAvailable)
	back := refund
	back.IdempotencyKey, back.FundRecordID, back.FundKind, back.BankReference, back.ReceivedAmount = fw.key(), "return.r", stlapi.FundReturnedRemittance,
		"ref.return.r", "400.00"
	if _, err = fw.stl.RecordFund(fw.treasury, back); err != nil {
		t.Fatalf("returned remittance: %v", err)
	}
	v, err := fw.stl.GetFundRecord(fw.treasury, "fund.r")
	fw.must(err)
	if v.AvailableMinor != "0" || v.CashMinor != "0" || v.DrawnMinor != "50000" || len(v.Draws) != 2 || len(v.Allocations) != 1 ||
		v.Allocations[0].Status != stlapi.ApplicationUnapplied {
		t.Fatalf("drawn receipt %+v", v)
	}
	ng := fw.receiptCmd("fund.ng", "50.00")
	ng.FundKind = stlapi.FundNonGoods
	_, err = fw.stl.RecordFund(fw.treasury, ng)
	fw.must(err)
	_, err = fw.apply("app.ng", "fund.ng", "order.r", "P1", "1.00")
	fw.want(err, kernel.CodeInvalidState)
	payer := fw.receiptCmd("fund.agent9", "500.00")
	payer.CounterpartyID = "agent.9"
	_, err = fw.stl.RecordFund(fw.treasury, payer)
	fw.must(err)
	_, err = fw.apply("app.agent9", "fund.agent9", "order.r", "P1", "500.00")
	fw.want(err, stlapi.CodePartyEntityMismatch)
}

// Baseline 4.6 S-A, cash side, and CN-AT09: USD 200 received in advance applies to the order's
// prepayment stage; USD 800 received later is sold at the bank's 6.875 for CNY 5,500.00 — one
// conversion, never a second receipt: the CNY bank line of the conversion cannot be recorded again
// as a customer receipt (BUSINESS_KEY_CONFLICT), the conversion is not applied, and the USD 800
// stays available for its applications of 400 and 400 (in S5-04a to the order's stage; B2b moves
// them to the receivables of the two statements).
func TestBaseline46CashSideAndConversion(t *testing.T) {
	fw := newFundWorld(t)
	fw.activeOrder("order.sa", "customer.1", fw.usd, payStage("P1", "order_confirmed", "20000"), payStage("P2", "after_shipment", "80000"))
	fw.receipt("fund.pre", "200.00", "")
	_, err := fw.apply("app.pre", "fund.pre", "order.sa", "P1", "200.00")
	fw.must(err)
	fw.receipt("fund.800", "800.00", "")
	conv := stlapi.RecordFund{IdempotencyKey: fw.key(), FundRecordID: "fx.800", FundKind: stlapi.FundFXConversion, SourceFundRecordID: "fund.800",
		Currency: stlRef(fw.cny), BankAccount: stlRef(fw.bankCNY), ReceivedAmount: "5500.00", ValueDate: kernel.FormatTimestamp(time.Now()),
		BankReference: "cny.line.1", EvidenceIDs: []string{fw.advice}, FX: &stlapi.FXSale{SoldAmount: "800.00", BankRate: "6.875"}}
	wrong := conv
	wrong.ReceivedAmount = "5499.00"
	_, err = fw.stl.RecordFund(fw.treasury, wrong)
	fw.want(err, kernel.CodeInvalidKnownValue)
	c, err := fw.stl.RecordFund(fw.treasury, conv)
	fw.must(err)
	if c.FundKind != stlapi.FundFXConversion || c.Currency != "CNY" || c.ReceivedMinor != "550000" {
		t.Fatalf("conversion %+v", c)
	}
	again := conv
	again.IdempotencyKey, again.FundRecordID = fw.key(), "fx.again"
	_, err = fw.stl.RecordFund(fw.treasury, again)
	fw.want(err, stlapi.CodeBusinessKeyConflict)
	again.BankReference = "cny.line.2"
	_, err = fw.stl.RecordFund(fw.treasury, again)
	fw.want(err, stlapi.CodeApplicationExceedsAvailable) // nothing left to sell
	cnyLine := fw.receiptCmd("fund.cny.line", "5500.00")
	cnyLine.Currency, cnyLine.BankAccount, cnyLine.BankReference = stlRef(fw.cny), stlRef(fw.bankCNY), "cny.line.1"
	_, err = fw.stl.RecordFund(fw.treasury, cnyLine)
	fw.want(err, stlapi.CodeBusinessKeyConflict)
	_, err = fw.apply("app.fx", "fx.800", "order.sa", "P2", "1.00")
	fw.want(err, kernel.CodeInvalidState)
	for i, id := range []string{"app.400a", "app.400b"} {
		a, err := fw.apply(id, "fund.800", "order.sa", "P2", "400.00")
		fw.must(err)
		if want := []string{"40000", "0"}[i]; a.FundAvailableMinor != want {
			t.Fatalf("%s %+v", id, a)
		}
	}
	ref, err := fw.stl.ResolveFundRecords(fw.treasury, []string{"fund.800", "fx.800"})
	fw.must(err)
	if f := ref["fund.800"]; f.SoldMinor != "80000" || f.CashMinor != "0" || f.AppliedMinor != "80000" || len(f.Draws) != 1 {
		t.Fatalf("USD receipt %+v", f)
	}
	if x := ref["fx.800"]; x.FX == nil || x.FX.BankRate != "6.875" || x.FX.SoldCurrency != "USD" || x.CounterpartyID != "" || x.Kind != "fx_conversion" {
		t.Fatalf("conversion ref %+v", x)
	}
}

// STL-INV-002 concurrency: two applications racing for one receipt's balance commit one after the
// other under the fund record's lock; the second sees the first and is refused. Five rounds.
func TestApplicationsInParallelNeverExceedTheFund(t *testing.T) {
	fw := newFundWorld(t)
	for round := range 5 {
		r := string(rune('a' + round))
		fund, orders := "fund.race."+r, []string{"order.x." + r, "order.y." + r}
		for _, o := range orders {
			fw.activeOrder(o, "customer.1", fw.usd, payStage("P1", "before_shipment", "100000"))
		}
		fw.receipt(fund, "1000.00", "")
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, order := range orders {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = fw.apply(fund+"."+order, fund, order, "P1", "150.00")
				if errs[i] == nil {
					_, errs[i] = fw.apply(fund+".big."+order, fund, order, "P1", "600.00")
				}
			}()
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch code(err) {
			case "OK":
				ok++
			case stlapi.CodeApplicationExceedsAvailable:
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		v, err := fw.stl.GetFundRecord(fw.treasury, fund)
		fw.must(err)
		if ok != 1 || v.AppliedMinor != "90000" || v.AvailableMinor != "10000" {
			t.Fatalf("round %d: %d winners, fund %+v", round, ok, v)
		}
	}
}

// ADR-BR-004 option B against STL (WBS "放行撤回与核销撤销的并发测试", STL half): application.unapply ∥ FUL's
// release (unit C1; here a downstream stand-in locks its root FOR UPDATE and calls
// PaymentConditionGuard v1). An unapply that committed first refuses the release; a release holding
// the guard makes the unapply wait until it commits (the fact stays); an unapply holding the account
// makes the guard wait and then answer blocked.
func TestPaymentConditionGuardDecidesEveryCommitOrder(t *testing.T) {
	fw := newFundWorld(t)
	d := newDownstream(fw.world)
	type outcome struct {
		at  time.Time
		err error
	}
	paid := func(order string) stlapi.PaymentQuery {
		fw.activeOrder(order, "customer.1", fw.usd, payStage("P1", "before_shipment", "10000"))
		fw.receipt("fund."+order, "100.00", "")
		_, err := fw.apply("app."+order, "fund."+order, order, "P1", "100.00")
		fw.must(err)
		return stlapi.PaymentQuery{OrderID: order, OrderRevision: 1, Scope: stlapi.ScopeShipment, At: time.Now()}
	}

	t.Run("STL commits first", func(t *testing.T) {
		q := paid("order.u1")
		_, err := fw.unapply("app.order.u1", 1)
		fw.must(err)
		d.createQuote("q.u1")
		v, err := d.release("q.u1", q, stage{})
		if !errors.Is(err, errPaymentRefused) || code(err) != stlapi.CodeCommercialReleaseBlocked || v.Reason != stlapi.PaymentStageUnpaid {
			t.Fatalf("release %v, verdict %+v", err, v)
		}
		if status, adopted := d.quote("q.u1"); status != "draft" || adopted != 0 {
			t.Fatalf("shipment %s with %d bindings", status, adopted)
		}
	})

	t.Run("FUL holds the guard first", func(t *testing.T) {
		q := paid("order.u2")
		d.createQuote("q.u2")
		unapplied := make(chan outcome, 1)
		pids := make(chan int32, 1)
		var moved time.Time
		v, err := d.release("q.u2", q, stage{pid: pids, hold: func() {
			pid := <-pids
			go func() {
				_, err := fw.unapply("app.order.u2", 1)
				unapplied <- outcome{time.Now(), err}
			}()
			await(t, "the unapply waits on the guard", func() bool { return d.blocks(pid) }, unapplied)
			moved = time.Now()
		}})
		if err != nil || !v.Satisfied() {
			t.Fatalf("release %v, verdict %+v", err, v)
		}
		u := <-unapplied
		if u.err != nil || !u.at.After(moved) {
			t.Fatalf("unapply %v, after the release: %v", u.err, u.at.After(moved))
		}
		if status, adopted := d.quote("q.u2"); status != "active" || adopted != 1 {
			t.Fatalf("shipment %s with %d bindings", status, adopted)
		}
		if v := fw.guard(d, "order.u2", 1, stlapi.ScopeShipment); v.Satisfied() {
			t.Fatalf("after the unapply %+v", v)
		}
	})

	t.Run("STL holds the account first", func(t *testing.T) {
		q := paid("order.u3")
		d.createQuote("q.u3")
		key := fw.key()
		hold, blockerPID := make(chan struct{}), make(chan int32, 1)
		blocker := make(chan error, 1)
		go func() {
			blocker <- d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
				var pid int32
				if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					return err
				}
				if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.action_result (tenant_id, company_id, command, idempotency_key, request_hash,
					action_id, object_id, row_version, status, actor_kind, actor_id, authority_epoch, correlation_id, recorded_at)
					VALUES ($1, $2, 'application.unapply', $3, repeat('0', 64), 'act.blocker', 'blocker', 1, 'unapplied', 'human', 'blocker', 1,
					'corr.blocker', now())`, string(d.scope.Tenant), string(d.scope.Company), key); err != nil {
					return err
				}
				blockerPID <- pid
				<-hold
				return errors.New("blocker rolls back")
			})
		}()
		var pid int32
		select {
		case pid = <-blockerPID:
		case err := <-blocker:
			t.Fatalf("blocker: %v", err)
		}
		unapplied := make(chan error, 1)
		go func() {
			_, err := fw.stl.Unapply(fw.treasury, stlapi.Unapply{IdempotencyKey: key, ApplicationID: "app.order.u3", ExpectedVersion: 1,
				Reason: "客户退汇，撤销核销"})
			unapplied <- err
		}()
		await(t, "the unapply holds the account and waits on the blocker", func() bool { return d.blocks(pid) }, unapplied)
		type release struct {
			v   stlapi.PaymentVerdict
			err error
		}
		released := make(chan release, 1)
		pids := make(chan int32, 1)
		go func() {
			v, err := d.release("q.u3", q, stage{pid: pids})
			released <- release{v, err}
		}()
		guardPID := <-pids
		await(t, "the guard waits on the unapply", func() bool { return d.waitsForLock(guardPID) }, released)
		close(hold)
		if err := <-blocker; err == nil {
			t.Fatal("the blocker committed")
		}
		if err := <-unapplied; err != nil {
			t.Fatalf("unapply: %v", err)
		}
		r := <-released
		if !errors.Is(r.err, errPaymentRefused) || r.v.Reason != stlapi.PaymentStageUnpaid || r.v.ShortfallMinor != "10000" {
			t.Fatalf("release after the unapply %v %+v", r.err, r.v)
		}
		if status, adopted := d.quote("q.u3"); status != "draft" || adopted != 0 {
			t.Fatalf("shipment %s with %d bindings", status, adopted)
		}
	})
}
