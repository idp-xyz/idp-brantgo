//go:build integration

package bootstrap_test

import (
	"strings"
	"testing"
	"time"

	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S5-07 on PostgreSQL 16: FUL's commercial release (tactical model FUL-AGG-CommercialRelease,
// SM-FUL-RELEASE, FUL-INV-002; Context Map R26, R27, R44, R45, R56, R59; WF-07, ES-10, SC-08,
// TD-AT08, TD-AT12, BT13; S5 decision D-11). Python V0.50 has no commercial release ("—（Python
// 未实现）"): the vectors are the design's. The world is the FUL world (fulWorld, the order world's
// TRD order, QLT on an in-memory PUR port, FUL on an in-memory delivery port) on the composed CNX,
// STL, PTY and TRD; the guards are their real SQL functions.

// sc08Stages are the core design's BRC-001 terms of SC-08: 30 % on order confirmation, 70 % before
// shipment (288.00 + 672.00 of 960.00).
var sc08Stages = []polapi.PaymentStage{{StageID: "P1", Percent: "30", Trigger: polapi.TriggerOrderConfirmed},
	{StageID: "P2", Percent: "70", Trigger: polapi.TriggerBeforeShipment}}

// amendTerms moves order o to payment terms stages by an amendment the author prepares and submits
// and the reviewer decides and applies (POL publishes them as the payment terms' next revision),
// and returns o at its new revision. Call it after the test's last activeOrder: later quotes would
// bind the superseded revision.
func (w *fulWorld) amendTerms(o trdapi.OrderReceipt, id string, stages []polapi.PaymentStage) trdapi.OrderReceipt {
	w.t.Helper()
	v1 := w.getOrder(o.ObjectID)
	pay := w.configure(w.author, w.reviewer, polapi.KindPaymentTerms, w.payment.ConfigID, w.rootVersion(polapi.KindPaymentTerms, w.payment.ConfigID),
		w.win.from, time.Time{}, polapi.ConfigData{PaymentTerms: &polapi.PaymentTerms{Name: "30% on order confirmation, 70% before shipment",
			Stages: stages}}, true)
	cmd := w.amendCmd(o.ObjectID, id, 0, v1.OrderRevision, v1.OrderHash, "120")
	cmd.Terms.PaymentTermsRef = tref(pay)
	p, err := w.c.TRD.PrepareAmendment(w.author, cmd)
	w.must(err)
	s, err := w.c.TRD.SubmitAmendment(w.author, trdapi.SubmitAmendment{IdempotencyKey: w.key(), OrderID: o.ObjectID, AmendmentID: id,
		ExpectedVersion: p.RowVersion})
	w.must(err)
	d, err := w.c.TRD.DecideAmendment(w.reviewer, trdapi.DecideAmendment{IdempotencyKey: w.key(), OrderID: o.ObjectID, AmendmentID: id,
		ExpectedVersion: s.RowVersion, AmendmentHash: s.AmendmentHash, Decision: trdapi.DecisionApprove, Reason: "客户书面确认新付款条件"})
	w.must(err)
	_, err = w.c.TRD.ApplyAmendment(w.reviewer, w.applyCmd(o.ObjectID, id, d.RowVersion))
	w.must(err)
	v2 := w.getOrder(o.ObjectID)
	o.OrderRevision, o.OrderHash = v2.OrderRevision, v2.OrderHash
	return o
}

// onOrder makes o the world's order (released, delivered and prepare work on it) until the
// returned function restores the one before.
func (w *fulWorld) onOrder(o trdapi.OrderReceipt) func() {
	saved := w.order
	w.order = o
	return func() { w.order = saved }
}

// sc08 is SC-08's order before the difference is resolved: order.quote.sc08 amended to 30/70
// before shipment (revision 2), cleared by compliance, its account open; 288.00 received and
// applied to P1; 672.00 remitted of which 670.00 credited with 2.00 bank charges on the advice,
// 670.00 applied to P2 (the 2.00 is unexplained and cannot be applied, STL-INV-006); shipment
// shipment.sc08 of all 120 PCS prepared on its own release and delivery line.
func (w *fulWorld) sc08() trdapi.OrderReceipt {
	w.t.Helper()
	o := w.amendTerms(w.activeOrder("quote.sc08"), "amendment.sc08", sc08Stages)
	w.cnx.cleared(o.ObjectID)
	if snap := w.account(o.ObjectID); len(snap.Stages) != 2 || snap.Stages[0].AmountMinor != "28800" || snap.Stages[1].AmountMinor != "67200" ||
		snap.Stages[1].Trigger != polapi.TriggerBeforeShipment {
		w.t.Fatalf("SC-08 terms %+v", snap.Stages)
	}
	w.receive("fund.sc08.p1", "288.00", "288.00", "")
	w.stlApply("app.sc08.p1", "fund.sc08.p1", o.ObjectID, "P1", "288.00")
	w.receive("fund.sc08.p2", "672.00", "670.00", "2.00")
	_, err := w.c.STL.Apply(w.treasury, w.stlApplyCmd("app.sc08.p2", "fund.sc08.p2", o.ObjectID, "P2", "672.00"))
	w.want(err, stlapi.CodeFundDifferenceUnexplained)
	w.stlApply("app.sc08.p2", "fund.sc08.p2", o.ObjectID, "P2", "670.00")
	restore := w.onOrder(o)
	defer restore()
	_, err = w.prepare(w.merch, "shipment.sc08", "120", w.delivered("commitment.sc08", "120"), w.released("commitment.sc08", "120"))
	w.must(err)
	return o
}

// shortOrder activates order "order.<quote>" (960.00, 100 % before shipment), clears it, opens its
// account with 959.00 applied (1.00 short) and prepares shipment id of 100 PCS on release
// rel.commitment.<quote suffix> and its delivery line.
func (w *fulWorld) shortOrder(quote, id string) trdapi.OrderReceipt {
	w.t.Helper()
	o := w.activeOrder(quote)
	w.cnx.cleared(o.ObjectID)
	stage := w.account(o.ObjectID).Stages[0].StageID
	w.receive("fund."+quote, "959.00", "959.00", "")
	w.stlApply("app."+quote, "fund."+quote, o.ObjectID, stage, "959.00")
	restore := w.onOrder(o)
	defer restore()
	c := "commitment." + strings.TrimPrefix(quote, "quote.")
	_, err := w.prepare(w.merch, id, "100", w.delivered(c, "100"), w.released(c, "100"))
	w.must(err)
	return o
}

func (w *fulWorld) fulEventsOf(kind string) []event {
	var out []event
	for _, e := range w.events("company.A", fulapi.Source) {
		if e.Type == kind {
			out = append(out, e)
		}
	}
	return out
}

// SC-08 and TD-AT08 (ES-10, WF-07; STL-INV-003, FUL-INV-002; D-5, D-11): with 670.00 applied of the
// 672.00 due before shipment and 2.00 unexplained, the documentation clerk's evaluation records the
// release blocked with COMMERCIAL_RELEASE_BLOCKED and the stage, and the confirmation is refused;
// STL's credit manager resolves the 2.00 as our bank charge (fund.resolve_difference) and the
// treasurer applies it; the next evaluation grants the release and binds the quality release, the
// payment condition and the screening it saw (SC-08 accept), with CommercialReleaseGranted. The
// merchandiser confirms the shipment, which consumes the release; the documents release is
// evaluated, an AI agent and the merchandiser may not record it, the documentation clerk records
// the telex release with evidence (DocumentsReleased), and a consumed release is terminal.
func TestSC08ReleaseWaitsForTheUnexplainedDifference(t *testing.T) {
	w := newFulWorld(t)
	o := w.sc08()

	blocked, err := w.evaluateRelease(w.ful, "shipment.sc08", fulapi.ScopeShipment)
	if err != nil || blocked.Status != fulapi.ReleaseBlocked || blocked.RowVersion != 1 || blocked.Code != fulapi.CodeCommercialReleaseBlocked ||
		blocked.Guard != fulapi.GuardPaymentCondition || !strings.Contains(blocked.Reason, "P2 (before_shipment) has 67000 of 67200 USD applied: 200 short") ||
		blocked.ShipmentID != "shipment.sc08" || blocked.Scope != fulapi.ScopeShipment || !strings.HasPrefix(blocked.ReleaseID, "rls-") {
		t.Fatalf("blocked %+v %v", blocked, err)
	}
	if len(w.fulEventsOf(fulapi.EventCommercialReleaseGranted)) != 0 {
		t.Fatal("a blocked evaluation publishes nothing")
	}
	_, err = w.confirm(w.merch, "shipment.sc08", 1)
	w.want(err, fulapi.CodeCommercialReleaseBlocked)

	_, err = w.c.STL.ResolveFundDifference(w.manager, stlapi.ResolveFundDifference{IdempotencyKey: w.key(), FundRecordID: "fund.sc08.p2",
		ExpectedVersion: 1, Resolution: stlapi.ResolutionBankCharge, Amount: "2.00", EvidenceIDs: []string{"evidence"},
		Reason: "中间行扣费，经批准由我方承担"})
	w.must(err)
	if again, err := w.evaluateRelease(w.ful, "shipment.sc08", fulapi.ScopeShipment); err != nil || again.Status != fulapi.ReleaseBlocked {
		t.Fatalf("a resolution counts only through an application (B2a): %+v %v", again, err)
	}
	w.stlApply("app.sc08.p2.charge", "fund.sc08.p2", o.ObjectID, "P2", "2.00")
	granted, err := w.evaluateRelease(w.ful, "shipment.sc08", fulapi.ScopeShipment)
	if err != nil || granted.Status != fulapi.ReleaseGranted || granted.RowVersion != 3 || granted.ReleaseID != blocked.ReleaseID || granted.Code != "" {
		t.Fatalf("granted %+v %v", granted, err)
	}
	v := w.releaseView(granted.ReleaseID)
	e := v.Evaluations[2]
	b := e.Bindings
	screening, err := w.c.PTY.GetScreening(w.author, "customer.1")
	w.must(err)
	if len(v.Evaluations) != 3 || v.Evaluations[0].Outcome != fulapi.ReleaseBlocked || v.Evaluations[0].Code != fulapi.CodeCommercialReleaseBlocked ||
		e.Outcome != fulapi.ReleaseGranted || e.EvaluatedBy.ID != "doc.1" || len(e.GuardsSHA256) != 64 || v.Order.OrderRevision != 2 ||
		b.Payment == nil || b.Payment.AppliedMinor != "96000" || b.Payment.RequiredMinor != "96000" || b.Payment.OrderRevision != 2 ||
		len(b.QualityReleases) != 1 || b.QualityReleases[0].ReleaseID != "rel.commitment.sc08" || b.QualityReleases[0].Released != "120" ||
		len(b.Screenings) == 0 || b.Screenings[0].PartyID != "customer.1" || b.Screenings[0].Status != "clear" || b.Screenings[0].ScreeningID == "" ||
		b.TradeCompliance == nil || b.TradeCompliance.CheckID == "" || b.TradeCompliance.LicenseRequired || b.ExportLicense != nil ||
		b.Order == nil || b.Order.Status != trdapi.OrderActive {
		t.Fatalf("the release binds what it saw %+v\n%+v (screening %+v)", v, b, screening)
	}
	if !strings.HasPrefix(releaseGuard(e, fulapi.GuardPaymentCondition), "satisfied//r2/USD/applied=96000") {
		t.Fatal(e.Guards)
	}
	if ev := w.fulEventsOf(fulapi.EventCommercialReleaseGranted); len(ev) != 1 || ev[0].Payload["release_id"] != granted.ReleaseID ||
		ev[0].Payload["scope"] != fulapi.ScopeShipment || ev[0].Payload["evaluation_seq"] != float64(3) || ev[0].Payload["guards_sha256"] != e.GuardsSHA256 {
		t.Fatalf("CommercialReleaseGranted %+v", ev)
	}

	c, err := w.confirm(w.merch, "shipment.sc08", 1)
	if err != nil || c.Status != fulapi.StatusConfirmed {
		t.Fatalf("confirm %+v %v", c, err)
	}
	v = w.releaseView(granted.ReleaseID)
	if v.Status != fulapi.ReleaseConsumed || v.RowVersion != 4 || v.Consumption == nil || v.Consumption.Command != fulapi.CommandConfirmShipment ||
		v.Consumption.ConsumedBy.ID != "merch.1" || v.Consumption.GuardsSHA256 != w.shipment("shipment.sc08").Confirmation.GuardsSHA256 {
		t.Fatalf("consumed %+v", v)
	}
	_, err = w.evaluateRelease(w.ful, "shipment.sc08", fulapi.ScopeShipment)
	w.want(err, kernel.CodeInvalidState)

	docs := w.grantedOn(w.ful, "shipment.sc08", fulapi.ScopeDocuments)
	record := fulapi.RecordDocumentRelease{IdempotencyKey: w.key(), ShipmentID: "shipment.sc08", ExpectedVersion: 1, ReleaseKind: fulapi.ReleaseTelexRelease,
		EvidenceIDs: []string{"evidence"}}
	_, err = w.ful.RecordDocumentRelease(w.agent, record)
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.ful.RecordDocumentRelease(w.merch, record)
	w.want(err, kernel.CodePermissionDenied)
	r, err := w.ful.RecordDocumentRelease(w.doc, record)
	if err != nil || r.Status != fulapi.ReleaseConsumed || r.ReleaseID != docs.ReleaseID || r.Scope != fulapi.ScopeDocuments || r.RowVersion != 2 {
		t.Fatalf("document release %+v %v", r, err)
	}
	if again, err := w.ful.RecordDocumentRelease(w.doc, record); err != nil || !again.Replayed {
		t.Fatalf("replay %+v %v", again, err)
	}
	record.IdempotencyKey, record.ExpectedVersion = w.key(), 2
	_, err = w.ful.RecordDocumentRelease(w.doc, record)
	w.want(err, kernel.CodeInvalidState)
	d := w.releaseView(docs.ReleaseID)
	if d.Consumption == nil || d.Consumption.Command != fulapi.CommandRecordDocumentRelease || d.Consumption.ReleaseKind != fulapi.ReleaseTelexRelease ||
		d.Consumption.EvidenceIDs[0] != "evidence" || d.Consumption.ConsumedBy.ID != "doc.1" {
		t.Fatalf("documents released %+v", d)
	}
	if ev := w.fulEventsOf(fulapi.EventDocumentsReleased); len(ev) != 1 || ev[0].Payload["release_id"] != docs.ReleaseID ||
		ev[0].Payload["release_kind"] != fulapi.ReleaseTelexRelease || ev[0].Payload["order_revision"] != float64(2) {
		t.Fatalf("DocumentsReleased %+v", ev)
	}
	list, err := w.ful.ShipmentReleases(w.merch, "shipment.sc08")
	if err != nil || len(list.Releases) != 2 || list.Releases[0].Scope != fulapi.ScopeShipment || list.Releases[1].Status != fulapi.ReleaseConsumed {
		t.Fatalf("releases %+v %v", list, err)
	}
}

// BT13, FUL-INV-002, UL-INV-28 and the role templates (commercial_release.grant_exception is the
// credit manager's alone): on SC-08's order, 2.00 short, a sales clerk's exception is
// PERMISSION_DENIED and an AI agent's too; the credit manager's, bound to the order revision, the
// shipment, the scope, stage P2 and the 200 minor units short, until a date, grants the release;
// a statement that is not what STL says is INVALID_KNOWN_VALUE. The confirmation re-checks every
// guard, finds the payment covered by the exception and records it. On a second shipment of the
// same order whose quality release QLT revoked, the evaluation is QUALITY_RELEASE_REQUIRED and the
// credit manager's exception is refused with that code; with a party no longer screened clear,
// PARTY_SCREENING_NOT_CLEAR. Nothing but the payment condition is ever excepted.
func TestExceptionCoversOnlyThePaymentCondition(t *testing.T) {
	w := newFulWorld(t)
	sales := w.assign("sales.1", "sales_clerk")
	bt13 := w.shortOrder("quote.bt13", "shipment.bt13")
	o := w.sc08()
	r, err := w.evaluateRelease(w.ful, "shipment.sc08", fulapi.ScopeShipment)
	if err != nil || r.Status != fulapi.ReleaseBlocked {
		t.Fatalf("%+v %v", r, err)
	}
	until := kernel.FormatTimestamp(time.Now().Add(72 * time.Hour))
	ex := fulapi.GrantReleaseException{IdempotencyKey: w.key(), ShipmentID: "shipment.sc08", Scope: fulapi.ScopeShipment, ExpectedVersion: 1,
		StageID: "P2", Currency: "USD", ShortfallMinor: "200", ValidUntil: until, Reason: "客户书面承诺 2.00 差额随下批货款支付", EvidenceIDs: []string{"evidence"}}
	_, err = w.ful.GrantReleaseException(sales, ex)
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.ful.GrantReleaseException(w.agent, ex)
	w.want(err, kernel.CodePermissionDenied)
	wrong := ex
	wrong.IdempotencyKey, wrong.ShortfallMinor = w.key(), "100"
	_, err = w.ful.GrantReleaseException(w.manager, wrong)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/shortfall_minor" {
		t.Fatalf("another amount: %s at %s", code(err), path(err))
	}
	g, err := w.ful.GrantReleaseException(w.manager, ex)
	if err != nil || g.Status != fulapi.ReleaseGranted || g.RowVersion != 2 {
		t.Fatalf("exception %+v %v", g, err)
	}
	v := w.releaseView(g.ReleaseID)
	if len(v.Exceptions) != 1 || v.Exceptions[0].StageID != "P2" || v.Exceptions[0].ShortfallMinor != "200" || v.Exceptions[0].Currency != "USD" ||
		v.Exceptions[0].GrantedBy.ID != "manager.1" || v.Exceptions[0].Order.OrderRevision != 2 || v.Exceptions[0].Order.OrderID != o.ObjectID ||
		v.Exceptions[0].ValidUntil.IsZero() || v.Exceptions[0].EvidenceIDs[0] != "evidence" {
		t.Fatalf("the exception %+v", v.Exceptions)
	}
	if ev := w.fulEventsOf(fulapi.EventCommercialReleaseGranted); len(ev) != 1 || ev[0].Payload["exception_seq"] != float64(1) {
		t.Fatalf("CommercialReleaseGranted %+v", ev)
	}
	if c, err := w.confirm(w.merch, "shipment.sc08", 1); err != nil || c.Status != fulapi.StatusConfirmed {
		t.Fatalf("confirm on the exception %+v %v", c, err)
	}
	s := w.shipment("shipment.sc08")
	if !strings.HasSuffix(guardValue(s, fulapi.GuardPaymentCondition), "/stage=P2/shortfall=200/exception=1") ||
		guardValue(s, fulapi.GuardCommercialRelease) != g.ReleaseID+"/v2/granted/exception=1" {
		t.Fatalf("the confirmation records the exception %+v", s.Confirmation.Guards)
	}

	// Another order, 1.00 short before shipment: once QLT revoked the quality release of its
	// shipment, and once its customer is no longer screened clear, the exception is refused with
	// the unmet guard's own code.
	r, err = w.evaluateRelease(w.ful, "shipment.bt13", fulapi.ScopeShipment)
	if err != nil || r.Code != fulapi.CodeCommercialReleaseBlocked || !strings.Contains(r.Reason, "100 short") {
		t.Fatalf("1.00 short: %+v %v", r, err)
	}
	_, err = w.qlt.RevokeRelease(w.qc, qltapi.RevokeRelease{IdempotencyKey: w.key(), ReleaseID: "rel.commitment.bt13", Reason: "复检发现色差，撤回放行"})
	w.must(err)
	r, err = w.evaluateRelease(w.ful, "shipment.bt13", fulapi.ScopeShipment)
	if err != nil || r.Status != fulapi.ReleaseBlocked || r.Code != fulapi.CodeQualityReleaseRequired || r.Guard != fulapi.GuardQualityRelease {
		t.Fatalf("revoked: %+v %v", r, err)
	}
	ex = fulapi.GrantReleaseException{IdempotencyKey: w.key(), ShipmentID: "shipment.bt13", Scope: fulapi.ScopeShipment, ExpectedVersion: 2,
		StageID: w.snapshots(w.author, bt13.ObjectID)[bt13.ObjectID].Stages[0].StageID, Currency: "USD", ShortfallMinor: "100",
		ValidUntil: kernel.FormatTimestamp(time.Now().Add(time.Hour)), Reason: "普通备注放行", EvidenceIDs: []string{"evidence"}}
	_, err = w.ful.GrantReleaseException(w.manager, ex)
	w.want(err, fulapi.CodeQualityReleaseRequired)

	screening, err := w.c.PTY.GetScreening(w.author, "customer.1")
	w.must(err)
	_, err = w.c.PTY.BlockParty(w.author, ptyapi.BlockParty{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: screening.RowVersion,
		Reason: "合成：列入负面清单"})
	w.must(err)
	r, err = w.evaluateRelease(w.ful, "shipment.bt13", fulapi.ScopeShipment)
	if err != nil || r.Code != fulapi.CodePartyScreeningNotClear || r.Guard != fulapi.GuardPartyScreening || r.RowVersion != 3 {
		t.Fatalf("screened: %+v %v", r, err)
	}
	ex.IdempotencyKey, ex.ExpectedVersion = w.key(), 3
	_, err = w.ful.GrantReleaseException(w.manager, ex)
	w.want(err, fulapi.CodePartyScreeningNotClear)
	if v := w.releaseView(r.ReleaseID); v.Status != fulapi.ReleaseBlocked || len(v.Exceptions) != 0 {
		t.Fatalf("never excepted %+v", v)
	}
}
