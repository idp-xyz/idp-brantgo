//go:build integration

package bootstrap_test

import (
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The commercial release's CNX guards (R56: TradeComplianceGuard v1, ExportLicenseGuard v1;
// TD-AT12 FUL half, TD-AT13; D-9) and its two WBS races on PostgreSQL 16 (S5-07 "放行撤回与核销撤销的
// 并发测试": quality_release.revoke ∥ shipment.confirm and application.unapply ∥
// document_release.record, both commit orders), on the FUL world.

// concluded has the compliance officer evaluate order at its current revision and conclude it
// (clear with license_required license, or blocked). A clear conclusion is not evaluated again
// (CNX: reopened first), so a test calls it on a draft order, before readyToActivate.
func (w *fulWorld) concluded(order, conclusion string, license bool) {
	w.t.Helper()
	c := w.compliance()
	b := c.binding(order)
	var expected int64
	if cs, err := c.cnx.OrderChecks(c.compliance, order); err == nil {
		for _, k := range cs {
			if k.OrderRevision == b.OrderRevision {
				expected = k.RowVersion
			}
		}
	}
	e, err := c.cnx.Evaluate(c.compliance, c.evaluateCmd(order, b.OrderRevision, expected, syntheticClear))
	w.must(err)
	_, err = c.cnx.Decide(c.compliance, c.decideCmd(e, conclusion, license))
	w.must(err)
}

// embargoed turns the clear conclusion of order's current revision blocked after the order
// activated on it (SM-CNX-COMPLIANCE clear → reopen → pending → evaluate → blocked; B4's recipe):
// a new version of the synthetic CN programme list is imported, the compliance officer reopens the
// conclusion (programme_updated), evaluates the destination AQ, which the CN programme embargoes,
// and concludes blocked. Every other conclusion that used the list's first version answers pending
// (list_updated) from then on, so a test calls it last.
func (w *fulWorld) embargoed(order string) {
	w.t.Helper()
	c := w.compliance()
	rev := w.getOrder(order).OrderRevision
	list := syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-10")
	list.IdempotencyKey = w.key()
	_, err := w.c.REF.ImportCountryProgrammes(c.compliance, list)
	w.must(err)
	checks, err := c.cnx.OrderChecks(c.compliance, order)
	w.must(err)
	var check cnxapi.CheckSummary
	for _, k := range checks {
		if k.OrderRevision == rev {
			check = k
		}
	}
	r, err := c.cnx.Reopen(c.compliance, cnxapi.Reopen{IdempotencyKey: w.key(), Cause: cnxapi.CauseProgrammeUpdated, CheckID: check.CheckID,
		ExpectedVersion: check.RowVersion, Reason: "合成国别项目名单新版本"})
	w.must(err)
	e, err := c.cnx.Evaluate(c.compliance, c.evaluateCmd(order, rev, r.RowVersion, syntheticEmbargoed))
	w.must(err)
	_, err = c.cnx.Decide(c.compliance, c.decideCmd(e, cnxapi.ConclusionBlocked, false))
	w.must(err)
}

// paidOrder has order "order.<quote>" prepared, conclude (nil: none) record its compliance
// conclusion while it is a draft — readyToActivate then leaves that check alone —, reviewed and
// activated; it opens the order's account with the 960.00 applied and prepares shipment id of all
// 120 PCS.
func (w *fulWorld) paidOrder(quote, id string, conclude func(order string)) trdapi.OrderReceipt {
	w.t.Helper()
	_, draft := w.prepared(quote)
	if conclude != nil {
		conclude(draft.ObjectID)
	}
	d := w.reviewed(draft, "review."+quote)
	o, err := w.c.TRD.ActivateOrder(w.author, w.activateOrderCmd(draft.ObjectID, d.RowVersion, "review."+quote))
	w.must(err)
	stage := w.account(o.ObjectID).Stages[0].StageID
	w.receive("fund."+quote, "960.00", "960.00", "")
	w.stlApply("app."+quote, "fund."+quote, o.ObjectID, stage, "960.00")
	restore := w.onOrder(o)
	defer restore()
	c := "commitment." + strings.TrimPrefix(quote, "quote.")
	_, err = w.prepare(w.merch, id, "120", w.delivered(c, "120"), w.released(c, "120"))
	w.must(err)
	return o
}

func (w *fulWorld) exceptionCmd(id string, version int64, stage string) fulapi.GrantReleaseException {
	return fulapi.GrantReleaseException{IdempotencyKey: w.key(), ShipmentID: id, Scope: fulapi.ScopeShipment, ExpectedVersion: version, StageID: stage,
		Currency: "USD", ShortfallMinor: "100", ValidUntil: kernel.FormatTimestamp(time.Now().Add(time.Hour)), Reason: "业务例外申请",
		EvidenceIDs: []string{"evidence"}}
}

// TD-AT12, FUL half ("出运放行均返回 TRADE_COMPLIANCE_NOT_CLEAR；普通审批不能覆盖"), CNX-INV-001, FUL-INV-002, D-9:
// on the world's order, amended to 110 PCS after a release of its revision 1 was granted, the
// confirmation of that shipment is REFERENCE_STALE (OrderGuard) and the release is not consumed; a
// shipment of revision 2, which has no conclusion, is TRADE_COMPLIANCE_NOT_CLEAR (pending,
// order_revised) and no exception covers it; once compliance clears revision 2, the order account
// still holds revision 1 (STL's review pending): COMMERCIAL_RELEASE_BLOCKED, which is not an unpaid
// stage, so no exception either. Then an order activated on a clear conclusion whose destination
// compliance, reopened on a new programme list version, concludes blocked (embargoed; B4: no order
// activates on a blocked one) is evaluated blocked with TRADE_COMPLIANCE_NOT_CLEAR, and the credit
// manager's exception and the confirmation are refused with it.
func TestTradeComplianceGatesTheReleaseTDAT12(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "120")
	d := w.delivered("commitment.1", "120")
	_, err := w.prepare(w.merch, "shipment.r1", "50", d, rel)
	w.must(err)
	first := w.granted("shipment.r1")
	v1 := w.getOrder(w.order.ObjectID)
	p, err := w.c.TRD.PrepareAmendment(w.author, w.amendCmd(w.order.ObjectID, "amendment.qty", 0, v1.OrderRevision, v1.OrderHash, "110"))
	w.must(err)
	s, err := w.c.TRD.SubmitAmendment(w.author, trdapi.SubmitAmendment{IdempotencyKey: w.key(), OrderID: w.order.ObjectID, AmendmentID: "amendment.qty",
		ExpectedVersion: p.RowVersion})
	w.must(err)
	dec, err := w.c.TRD.DecideAmendment(w.reviewer, trdapi.DecideAmendment{IdempotencyKey: w.key(), OrderID: w.order.ObjectID,
		AmendmentID: "amendment.qty", ExpectedVersion: s.RowVersion, AmendmentHash: s.AmendmentHash, Decision: trdapi.DecisionApprove, Reason: "客户减量"})
	w.must(err)
	_, err = w.c.TRD.ApplyAmendment(w.reviewer, w.applyCmd(w.order.ObjectID, "amendment.qty", dec.RowVersion))
	w.must(err)
	_, err = w.confirm(w.merch, "shipment.r1", 1)
	w.want(err, fulapi.CodeReferenceStale)
	if v := w.releaseView(first.ReleaseID); v.Status != fulapi.ReleaseGranted || v.Consumption != nil {
		t.Fatalf("a release whose guards fail now is not consumed %+v", v)
	}

	v2 := w.getOrder(w.order.ObjectID)
	o2 := w.order
	o2.OrderRevision, o2.OrderHash = v2.OrderRevision, v2.OrderHash
	restore := w.onOrder(o2)
	_, err = w.prepare(w.merch, "shipment.r2", "50", w.delivered("commitment.2", "120"), w.released("commitment.2", "120"))
	restore()
	w.must(err)
	r, err := w.evaluateRelease(w.ful, "shipment.r2", fulapi.ScopeShipment)
	if err != nil || r.Code != fulapi.CodeTradeComplianceNotClear || !strings.Contains(r.Reason, cnxapi.ReasonOrderRevised) {
		t.Fatalf("revision 2 without a conclusion %+v %v", r, err)
	}
	stage := w.snapshots(w.author, w.order.ObjectID)[w.order.ObjectID].Stages[0].StageID
	_, err = w.ful.GrantReleaseException(w.manager, w.exceptionCmd("shipment.r2", 1, stage))
	w.want(err, fulapi.CodeTradeComplianceNotClear)
	w.compliance().cleared(w.order.ObjectID)
	r, err = w.evaluateRelease(w.ful, "shipment.r2", fulapi.ScopeShipment)
	if err != nil || r.Code != fulapi.CodeCommercialReleaseBlocked || !strings.Contains(r.Reason, "revision 1") {
		t.Fatalf("the account at revision 1 %+v %v", r, err)
	}
	_, err = w.ful.GrantReleaseException(w.manager, w.exceptionCmd("shipment.r2", 2, stage))
	w.want(err, fulapi.CodeCommercialReleaseBlocked)

	o := w.paidOrder("quote.embargo", "shipment.embargo", nil)
	w.embargoed(o.ObjectID)
	r, err = w.evaluateRelease(w.ful, "shipment.embargo", fulapi.ScopeShipment)
	if err != nil || r.Status != fulapi.ReleaseBlocked || r.Code != fulapi.CodeTradeComplianceNotClear || r.Guard != fulapi.GuardTradeCompliance ||
		!strings.Contains(r.Reason, "blocked") {
		t.Fatalf("blocked conclusion %+v %v", r, err)
	}
	_, err = w.ful.GrantReleaseException(w.manager, w.exceptionCmd("shipment.embargo", 1, "P1"))
	w.want(err, fulapi.CodeTradeComplianceNotClear)
	_, err = w.confirm(w.merch, "shipment.embargo", 1)
	w.want(err, fulapi.CodeTradeComplianceNotClear)
}

// ExportLicenseGuard v1 at the release (R56, CNX-INV-002, FUL-INV-002; B5's contract): a clear
// conclusion that says the goods need a licence makes the release EXPORT_LICENSE_REQUIRED while no
// allocation covers the shipment, and the exception is refused with it. The composed process's CNX
// cannot allocate to a FUL shipment yet (cnx.ExportsNotWired, B5): the covered path runs CNX's own
// module on an in-memory FUL port that states the shipment's lines as FUL holds them; it registers
// a synthetic licence of 1,000 PCS and allocates it to the shipment, and the release, judged by
// the real guard on those rows, is granted and binds the allocation.
func TestExportLicenseGatesTheRelease(t *testing.T) {
	w := newFulWorld(t)
	o := w.paidOrder("quote.lic", "shipment.lic", func(o string) { w.concluded(o, cnxapi.ConclusionClear, true) })
	r, err := w.evaluateRelease(w.ful, "shipment.lic", fulapi.ScopeShipment)
	if err != nil || r.Code != fulapi.CodeExportLicenseRequired || r.Guard != fulapi.GuardExportLicense ||
		!strings.Contains(r.Reason, cnxapi.LicenseReasonNotAllocated) {
		t.Fatalf("not allocated %+v %v", r, err)
	}
	stage := w.snapshots(w.author, o.ObjectID)[o.ObjectID].Stages[0].StageID
	_, err = w.ful.GrantReleaseException(w.manager, w.exceptionCmd("shipment.lic", 1, stage))
	w.want(err, fulapi.CodeExportLicenseRequired)

	exports := &fakeExports{exports: map[cnx.ExportRef]cnx.Export{}}
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	cx, err := cnx.New(db, cnx.Upstream{Orders: cnx.TRDOrders(w.c.TRD), Countries: cnx.PTYCountries(w.c.PTY), Profiles: cnx.CATProfiles(w.c.CAT),
		Programmes: cnx.REFProgrammes(w.c.REF), Configs: w.c.POL, Sources: w.c.EVD, Exports: exports, Purchases: cnx.PURLines(w.c.PUR),
		Funds: cnx.STLFunds(w.c.STL), CreditNotes: cnx.STLCreditNotes(w.c.STL)})
	w.must(err)
	_, err = w.file(w.doc, "licence.1", "licence.txt", evdapi.MimeText, "合成出口许可证（测试数据）", "")
	w.must(err)
	now := time.Now().UTC()
	l, err := cx.RegisterLicense(w.doc, cnxapi.RegisterLicense{IdempotencyKey: w.key(), LicenseNumber: "SYN-EL-0001", Issuer: "合成发证机关（测试）",
		Holder: "合成出口商（测试）", Goods: cnxapi.LicensedGoods{SkuSpecs: []cnxapi.SpecRef{{SkuID: w.line.Sku.SkuID, SpecRevision: w.line.Sku.SpecRevision}}},
		LicensedQuantity: "1000", Unit: w.line.UnitRef.ID, Currency: cnxapi.CurrencyRef{ID: w.usd.ConfigID, Revision: w.usd.Revision},
		LicensedValue: "10000.00", Destination: "DE", EndUser: cnxapi.EndUser{PartyID: "customer.1", Name: "合成最终用户"},
		ValidFrom: kernel.FormatTimestamp(now.Add(-time.Hour)), ValidUntil: kernel.FormatTimestamp(now.Add(30 * 24 * time.Hour)), FileID: "licence.1",
		CustomsOffice: "2248"}) // S5R-D04: 一证一关
	w.must(err)
	value, err := kernel.RoundMoney(kernel.MustCurrency("USD", 2), kernel.MustDecimal("960"))
	w.must(err)
	exports.put(cnx.Export{Ref: cnx.ExportRef{Kind: cnxapi.TargetShipment, ID: "shipment.lic"}, Status: cnx.ShipmentPrepared, Version: 1,
		OrderID: o.ObjectID, OrderRevision: o.OrderRevision, Destination: "DE", EndUserPartyID: "customer.1", Value: value,
		Lines: []cnx.ExportLine{{LineID: "S1", SkuID: w.line.Sku.SkuID, SpecRevision: w.line.Sku.SpecRevision, Quantity: kernel.MustDecimal("120"),
			Unit: kernel.UnitCode(w.line.UnitRef.ID)}}})
	_, err = cx.AllocateLicense(w.doc, cnxapi.AllocateLicense{IdempotencyKey: w.key(), LicenseID: l.LicenseID, AllocationID: "alloc.lic",
		Target: cnxapi.ExportTarget{Kind: cnxapi.TargetShipment, ID: "shipment.lic"}})
	w.must(err)
	g := w.granted("shipment.lic")
	b := w.releaseView(g.ReleaseID).Evaluations[1].Bindings
	if b.ExportLicense == nil || b.ExportLicense.AllocationID != "alloc.lic" || b.ExportLicense.LicenseID != l.LicenseID ||
		b.ExportLicense.Status != cnxapi.LicenseStatusAllocated || b.TradeCompliance == nil || !b.TradeCompliance.LicenseRequired {
		t.Fatalf("the release binds the allocation %+v", b)
	}
	if c, err := w.confirm(w.merch, "shipment.lic", 1); err != nil || c.Status != fulapi.StatusConfirmed ||
		!strings.Contains(guardValue(w.shipment("shipment.lic"), fulapi.GuardExportLicense), "alloc.lic") {
		t.Fatalf("confirm %+v %v", c, err)
	}
}

// quality_release.revoke ∥ shipment.confirm, both commit orders (R26, QLT-INV-002, FUL-INV-002,
// D-11): with granted releases, a revocation that commits first refuses the confirmation
// (QUALITY_RELEASE_REQUIRED) and leaves the release granted, and the next evaluation blocks it; a
// confirmation that holds the quality release FOR SHARE (held after QLT's guard) makes the
// revocation wait until it commits — the shipment and the consumed release stay, the revocation
// then goes through and the next release on that quality release is blocked.
func TestQualityRevocationAndTheReleasedConfirmation(t *testing.T) {
	w := newFulWorld(t)
	rel1, rel2 := w.released("commitment.1", "50"), w.released("commitment.2", "50")
	d1, d2 := w.delivered("commitment.1", "50"), w.delivered("commitment.2", "50")
	for _, s := range []struct{ id, rel string }{{"shipment.1", rel1}, {"shipment.2", rel2}, {"shipment.3", rel2}} {
		d := d1
		if s.rel == rel2 {
			d = d2
		}
		_, err := w.prepare(w.merch, s.id, "20", d, s.rel)
		w.must(err)
	}
	g1, g2 := w.granted("shipment.1"), w.granted("shipment.2")

	_, err := w.qlt.RevokeRelease(w.qc, qltapi.RevokeRelease{IdempotencyKey: w.key(), ReleaseID: rel1, Reason: "复检发现色差，撤回放行"})
	w.must(err)
	_, err = w.confirm(w.merch, "shipment.1", 1)
	w.want(err, fulapi.CodeQualityReleaseRequired)
	if v := w.releaseView(g1.ReleaseID); v.Status != fulapi.ReleaseGranted || v.Consumption != nil {
		t.Fatalf("revocation first: the release stays granted, unconsumed %+v", v)
	}
	if r, err := w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeShipment); err != nil || r.Status != fulapi.ReleaseBlocked ||
		r.Code != fulapi.CodeQualityReleaseRequired {
		t.Fatalf("re-evaluated %+v %v", r, err)
	}

	w.hold.set("release")
	done := make(chan error, 1)
	go func() { _, err := w.confirm(w.merch, "shipment.2", 1); done <- err }()
	w.hold.wait(t, done)
	revoked := make(chan error, 1)
	go func() {
		_, err := w.qlt.RevokeRelease(w.qc, qltapi.RevokeRelease{IdempotencyKey: w.key(), ReleaseID: rel2, Reason: "复检发现色差，撤回放行"})
		revoked <- err
	}()
	select {
	case err := <-revoked:
		w.hold.open()
		t.Fatalf("the revocation did not wait: %v (confirmation: %v)", err, <-done)
	case <-time.After(500 * time.Millisecond):
	}
	w.hold.open()
	if err := <-done; err != nil {
		t.Fatalf("the confirmation holding the guard: %v", err)
	}
	if err := <-revoked; err != nil {
		t.Fatalf("the revocation after FUL committed: %v", err)
	}
	if v := w.releaseView(g2.ReleaseID); v.Status != fulapi.ReleaseConsumed || w.shipment("shipment.2").Status != fulapi.StatusConfirmed {
		t.Fatalf("FUL first: the shipment and the consumed release stay %+v", v)
	}
	if r, err := w.evaluateRelease(w.ful, "shipment.3", fulapi.ScopeShipment); err != nil || r.Code != fulapi.CodeQualityReleaseRequired {
		t.Fatalf("after the revocation %+v %v", r, err)
	}
}

// application.unapply ∥ document_release.record, both commit orders (R45, STL-INV-003, MD-37
// "撤销核销或退汇后重新判定", FUL-INV-002, D-11): with the shipment confirmed and its documents release
// granted, an unapply that commits first makes the document release COMMERCIAL_RELEASE_BLOCKED and
// leaves the release granted; once the treasurer applies the money again, a document release that
// holds the order account FOR SHARE (held after STL's guard) makes the unapply wait until it commits
// — the documents stay released (DocumentsReleased), the unapply then goes through, and a new
// evaluation of the documents scope would be blocked.
func TestUnapplyAndDocumentReleaseInBothCommitOrders(t *testing.T) {
	w := newFulWorld(t)
	rel, d := w.released("commitment.1", "100"), w.delivered("commitment.1", "100")
	_, err := w.prepare(w.merch, "shipment.1", "60", d, rel)
	w.must(err)
	_, err = w.prepare(w.merch, "shipment.2", "40", d, rel)
	w.must(err)
	w.granted("shipment.1")
	_, err = w.confirm(w.merch, "shipment.1", 1)
	w.must(err)
	w.issuedBillOfLading(w.ful, "shipment.1") // S5-08: documents are released on the issued B/L record
	docs := w.grantedOn(w.ful, "shipment.1", fulapi.ScopeDocuments)
	stage := w.snapshots(w.author, w.order.ObjectID)[w.order.ObjectID].Stages[0].StageID
	app, fund := "app."+w.order.ObjectID+"."+stage, "fund."+w.order.ObjectID+"."+stage
	record := func(key string) (fulapi.ReleaseReceipt, error) {
		return w.ful.RecordDocumentRelease(w.doc, fulapi.RecordDocumentRelease{IdempotencyKey: key, ShipmentID: "shipment.1", ExpectedVersion: 1,
			ReleaseKind: fulapi.ReleaseOriginalBL, EvidenceIDs: []string{"evidence"}})
	}

	_, err = w.c.STL.Unapply(w.treasury, stlapi.Unapply{IdempotencyKey: w.key(), ApplicationID: app, ExpectedVersion: 1, Reason: "银行退回，撤销核销"})
	w.must(err)
	_, err = record(w.key())
	w.want(err, fulapi.CodeCommercialReleaseBlocked)
	if v := w.releaseView(docs.ReleaseID); v.Status != fulapi.ReleaseGranted || v.RowVersion != 1 || v.Consumption != nil {
		t.Fatalf("unapply first: the release stays granted %+v", v)
	}

	w.stlApply("app.again", fund, w.order.ObjectID, stage, "960.00")
	w.hold.set("payment")
	done := make(chan error, 1)
	go func() { _, err := record(w.key()); done <- err }()
	w.hold.wait(t, done)
	unapplied := make(chan error, 1)
	go func() {
		_, err := w.c.STL.Unapply(w.treasury, stlapi.Unapply{IdempotencyKey: w.key(), ApplicationID: "app.again", ExpectedVersion: 1,
			Reason: "客户退汇，撤销核销"})
		unapplied <- err
	}()
	select {
	case err := <-unapplied:
		w.hold.open()
		t.Fatalf("the unapply did not wait: %v (document release: %v)", err, <-done)
	case <-time.After(500 * time.Millisecond):
	}
	w.hold.open()
	if err := <-done; err != nil {
		t.Fatalf("the document release holding the guard: %v", err)
	}
	if err := <-unapplied; err != nil {
		t.Fatalf("the unapply after FUL committed: %v", err)
	}
	if v := w.releaseView(docs.ReleaseID); v.Status != fulapi.ReleaseConsumed || v.Consumption.ReleaseKind != fulapi.ReleaseOriginalBL ||
		len(w.fulEventsOf(fulapi.EventDocumentsReleased)) != 1 {
		t.Fatalf("FUL first: the documents stay released %+v", v)
	}
	if r, err := w.evaluateRelease(w.ful, "shipment.2", fulapi.ScopeShipment); err != nil || r.Code != fulapi.CodeCommercialReleaseBlocked {
		t.Fatalf("the money is no longer applied %+v %v", r, err)
	}
}

// The release tables only grow (SM-FUL-RELEASE; D-3): neither the application role nor the schema
// owner may change an evaluation or an exception, move a release but by its commands, delete one, or
// confirm a shipment around its release (FUL-INV-002, checked at commit); evaluations by an AI agent
// and by the system are commands like a human's (SM-FUL-RELEASE "human|ai|system") and replay; a
// merchandiser with own data scope does not see another's release; company.B sees no row.
func TestReleaseRecordsAreImmutable(t *testing.T) {
	w := newFulWorld(t)
	rel, d := w.released("commitment.1", "100"), w.delivered("commitment.1", "100")
	for _, id := range []string{"shipment.1", "shipment.2"} {
		_, err := w.prepare(w.merch, id, "40", d, rel)
		w.must(err)
	}
	ai := w.provision(releaseGrant(w, kernel.ActorAI, "agent.release", "company", "commercial_release.evaluate", "ful.read"))
	sys := w.provision(releaseGrant(w, kernel.ActorSystem, "system.release", "company", "commercial_release.evaluate", "ful.read"))
	cmd := w.releaseCmd("shipment.1", fulapi.ScopeShipment)
	first, err := w.ful.EvaluateRelease(ai, cmd)
	if err != nil || first.Status != fulapi.ReleaseGranted {
		t.Fatalf("AI %+v %v", first, err)
	}
	if again, err := w.ful.EvaluateRelease(ai, cmd); err != nil || !again.Replayed || again.ActionID != first.ActionID {
		t.Fatalf("replay %+v %v", again, err)
	}
	if r, err := w.ful.EvaluateRelease(sys, w.releaseCmd("shipment.1", fulapi.ScopeShipment)); err != nil || r.RowVersion != 2 {
		t.Fatalf("system %+v %v", r, err)
	}
	_, err = w.confirm(w.merch, "shipment.1", 1)
	w.must(err)
	w.granted("shipment.2")
	own := w.provision(releaseGrant(w, kernel.ActorHuman, "merch.own", "own", "ful.read"))
	_, err = w.ful.GetRelease(own, first.ReleaseID)
	w.want(err, kernel.CodeNotFound)
	for _, sql := range []string{
		`UPDATE brant_ful.commercial_release SET status = 'granted', row_version = row_version + 1, evaluations = evaluations + 1
			WHERE shipment_id = 'shipment.1'`,
		`UPDATE brant_ful.commercial_release SET scope = 'documents' WHERE shipment_id = 'shipment.2'`,
		`DELETE FROM brant_ful.commercial_release WHERE shipment_id = 'shipment.2'`,
		`UPDATE brant_ful.commercial_release_evaluation SET outcome = 'blocked'`,
		`DELETE FROM brant_ful.commercial_release_evaluation`,
		`UPDATE brant_ful.shipment SET status = 'confirmed', row_version = 2, confirmed_by_kind = 'human', confirmed_by_id = 'x', confirm_epoch = 1,
			confirmed_at = now(), confirm_evidence_ids = '{}', guard_entries = '[]',
			guard_entries_sha256 = encode(sha256(convert_to('[]', 'UTF8')), 'hex'), unchecked_guards = '{}' WHERE shipment_id = 'shipment.2'`,
	} {
		if err := w.exec("company.A", sql); err == nil || !strings.Contains(err.Error(), "brant_ful") && !strings.Contains(err.Error(), "42501") {
			t.Errorf("as brant_app %s: %v", sql, err)
		}
		if err := w.world.asOwner("company.A", sql); err == nil || !strings.Contains(err.Error(), "brant_ful") {
			t.Errorf("as the owner %s: %v", sql, err)
		}
	}
	if w.count("company.A", "brant_ful.commercial_release") != 2 || w.count("company.A", "brant_ful.commercial_release_evaluation") != 3 ||
		w.count("company.B", "brant_ful.commercial_release") != 0 || w.count("company.B", "brant_ful.commercial_release_evaluation") != 0 {
		t.Fatal("rows")
	}
	if v := w.releaseView(first.ReleaseID); v.Status != fulapi.ReleaseConsumed || v.Evaluations[0].EvaluatedBy.Kind != kernel.ActorAI ||
		v.Evaluations[1].EvaluatedBy.Kind != kernel.ActorSystem {
		t.Fatalf("after the refused edits %+v", v)
	}
}
