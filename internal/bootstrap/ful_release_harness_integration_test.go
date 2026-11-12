//go:build integration

package bootstrap_test

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What the FUL world needs since the commercial release (S5 unit C1, WBS S5-07; D-11, D-20): a
// clear CNX conclusion of the order revision (TradeComplianceGuard v1; the order world's
// activeOrder records it through B4's readyToActivate, q.compliance()) and STL's order account with
// the order's payment stages applied (PaymentConditionGuard v1). The order of the world is 960.00
// USD, 100 % before shipment (one stage). "treasury.1" holds the treasury role template,
// "manager.1" credit_manager, doc.1 and doc.2 documentation (cnxOn).

// releaseKit is what the FUL world adds for the commercial release.
type releaseKit struct {
	treasury context.Context
	manager  context.Context
	doc      context.Context
	bankUSD  polapi.ConfigRef
}

type heldPayments struct {
	ful.Payments
	hold *fulHold
}

func (p heldPayments) GuardPaymentCondition(ctx context.Context, exec bentopg.Executor, q stlapi.PaymentQuery) (stlapi.PaymentVerdict, error) {
	v, err := p.Payments.GuardPaymentCondition(ctx, exec, q)
	if err == nil {
		p.hold.at("payment")
	}
	return v, err
}

// releasable sets up the kit once and makes the world's order releasable: compliance clear (the
// order world's activeOrder did that), the order account open, 960.00 received and applied to its
// stage.
func (w *fulWorld) releasable() {
	w.t.Helper()
	w.doc = w.compliance().doc1
	w.treasury, w.manager = w.assign("treasury.1", "treasury"), w.assign("manager.1", "credit_manager")
	w.bankUSD = w.configure(w.author, w.reviewer, polapi.KindBankAccount, "bank.usd", 0, w.win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "USD", polapi.AccountBoth), true)
	w.payable(w.order)
}

// payable makes order o releasable: a clear conclusion of its current revision (cleared unless
// CNX has a check of it already — activeOrder records one through readyToActivate, and a clear
// conclusion is not evaluated again), its account opened and every stage paid in full by one
// receipt of its own.
func (w *fulWorld) payable(o trdapi.OrderReceipt) {
	w.t.Helper()
	w.clearedOnce(o.ObjectID)
	snap := w.account(o.ObjectID)
	for _, s := range snap.Stages {
		amount := minorText(s.AmountMinor)
		fund := "fund." + o.ObjectID + "." + s.StageID
		w.receive(fund, amount, amount, "")
		w.stlApply("app."+o.ObjectID+"."+s.StageID, fund, o.ObjectID, s.StageID, amount)
	}
}

// clearedOnce has compliance clear order at its current revision unless CNX has a check of that
// revision already (as readyToActivate does).
func (w *fulWorld) clearedOnce(order string) {
	w.t.Helper()
	c := w.compliance()
	rev := w.getOrder(order).OrderRevision
	checks, err := c.cnx.OrderChecks(c.compliance, order)
	w.must(err)
	for _, k := range checks {
		if k.OrderRevision == rev {
			return
		}
	}
	c.cleared(order)
}

// minorText is a USD amount in minor units as a decimal amount (96000 → 960.00).
func minorText(minor string) string {
	for len(minor) < 3 {
		minor = "0" + minor
	}
	return minor[:len(minor)-2] + "." + minor[len(minor)-2:]
}

// account has the treasurer open the account of order id at TRD's current revision and returns
// TRD's snapshot.
func (w *fulWorld) account(id string) trdapi.OrderSnapshotFact {
	w.t.Helper()
	snap := w.snapshots(w.author, id)[id]
	_, err := w.c.STL.OpenOrderAccount(w.treasury, stlapi.OpenOrderAccount{IdempotencyKey: w.key(), OrderID: id,
		OrderRevision: snap.Binding.OrderRevision, OrderHash: snap.Binding.OrderHash})
	w.must(err)
	return snap
}

// receive has the treasurer record receipt fund of customer.1: remitted, received and the charges
// the advice states ("" for none).
func (w *fulWorld) receive(fund, remitted, received, charges string) {
	w.t.Helper()
	_, err := w.c.STL.RecordFund(w.treasury, stlapi.RecordFund{IdempotencyKey: w.key(), FundRecordID: fund, FundKind: stlapi.FundReceipt,
		CounterpartyID: "customer.1", Currency: stlRef(w.usd), BankAccount: stlRef(w.bankUSD), ReceivedAmount: received, RemittedAmount: remitted,
		BankCharges: charges, ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "MT103-" + fund, EvidenceIDs: []string{"evidence"}})
	w.must(err)
}

func (w *fulWorld) stlApplyCmd(app, fund, order, stage, amount string) stlapi.Apply {
	return stlapi.Apply{IdempotencyKey: w.key(), ApplicationID: app, FundRecordID: fund,
		Target: stlapi.ApplicationTarget{Kind: stlapi.TargetOrderStage, OrderID: order, StageID: stage}, Amount: amount}
}

// stlApply has the treasurer apply amount of fund to order's stage as application app.
func (w *fulWorld) stlApply(app, fund, order, stage, amount string) {
	w.t.Helper()
	_, err := w.c.STL.Apply(w.treasury, w.stlApplyCmd(app, fund, order, stage, amount))
	w.must(err)
}

func (w *fulWorld) releaseCmd(id, scope string) fulapi.EvaluateRelease {
	return fulapi.EvaluateRelease{IdempotencyKey: w.key(), ShipmentID: id, Scope: scope}
}

// evaluateRelease has the documentation clerk evaluate shipment id's release of scope on module m.
func (w *fulWorld) evaluateRelease(m *ful.Module, id, scope string) (fulapi.ReleaseReceipt, error) {
	return m.EvaluateRelease(w.doc, w.releaseCmd(id, scope))
}

// granted evaluates the shipment release of shipment id on the world's FUL and requires it granted.
func (w *fulWorld) granted(id string) fulapi.ReleaseReceipt {
	return w.grantedOn(w.ful, id, fulapi.ScopeShipment)
}

// grantedOn evaluates shipment id's release of scope on module m and requires it granted.
func (w *fulWorld) grantedOn(m *ful.Module, id, scope string) fulapi.ReleaseReceipt {
	w.t.Helper()
	r, err := w.evaluateRelease(m, id, scope)
	if err != nil || r.Status != fulapi.ReleaseGranted {
		w.t.Fatalf("release of %s for %s: %+v %v", id, scope, r, err)
	}
	return r
}

func (w *fulWorld) releaseView(id string) fulapi.ReleaseView {
	w.t.Helper()
	v, err := w.ful.GetRelease(w.doc, id)
	w.must(err)
	return v
}

// releaseGrant is an operator's grant of permissions to actor id of kind in company.A with data
// scope.
func releaseGrant(w *fulWorld, kind kernel.ActorKind, id string, scope authorization.DataScope, permissions ...string) bootstrap.Grant {
	return bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kind, ID: id}, Permissions: permissions, DataScope: scope}
}

func releaseGuard(e fulapi.EvaluationView, name string) string {
	for _, g := range e.Guards {
		if g.Name == name {
			return g.Value
		}
	}
	return ""
}
