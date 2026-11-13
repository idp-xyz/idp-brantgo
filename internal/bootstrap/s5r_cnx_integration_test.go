//go:build integration

package bootstrap_test

import (
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/contexts/cnx"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// S5R unit R9 (WBS S5R-09; simulated sign-off E0 of 2026-11-13): CNX's trade compliance and export
// licences after S5R-D03, D04, D10, D15, D17, D38 and D40, on PostgreSQL 16 with the composed
// process's TRD, PTY, POL, EVD, CAT and reference data. Python V0.50 has no CNX ("—（Python 未实现）"):
// the vectors come from the register and the design (CNX-INV-001, CNX-INV-002, SM-CNX-COMPLIANCE,
// Context Map R36, R56, R58). Every programme, licence and party is synthetic.

// S5R-D10 (R58, R56; SM-CNX-COMPLIANCE blocked → evaluate → pending "守卫在新的决定之前仍答 blocked"):
// an order revision concluded blocked (an embargoed destination) is evaluated again on corrected
// facts; until compliance decides again TradeComplianceGuard v1 answers blocked, reason
// blocked_under_review, naming the blocked decision, and order.activate stays refused with
// TRADE_COMPLIANCE_NOT_CLEAR; the reads show the same state. Once concluded clear the guard answers
// clear and the activation moves on to the credit guard (CREDIT_RESERVATION_REQUIRED: no
// reservation).
func TestS5RD10BlockedUnderReviewStaysBlocked(t *testing.T) {
	q := newCreditOrderWorld(t, "")
	w := q.compliance()
	o, r := q.draftOn("quote.2")
	b := w.binding(o.ObjectID)
	ev, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, 0, syntheticEmbargoed))
	q.must(err)
	blocked, err := w.cnx.Decide(w.compliance, w.decideCmd(ev, cnxapi.ConclusionBlocked, false))
	q.must(err)
	w.wantVerdict(b, cnxapi.StatusBlocked, cnxapi.ReasonBlocked)
	_, err = q.c.TRD.ActivateOrder(q.author, q.activation(o, r))
	wantRefused(t, "blocked", err, cnxapi.CodeTradeComplianceNotClear, "/args/order_id")

	again, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, blocked.RowVersion, syntheticClear))
	q.must(err)
	if again.Status != cnxapi.StatusPending || again.Outcome != cnxapi.OutcomeNoMatch || again.DecisionID != "" {
		t.Fatalf("evaluated again %+v", again)
	}
	v := w.wantVerdict(b, cnxapi.StatusBlocked, cnxapi.ReasonBlockedUnderReview)
	if v.DecisionID != blocked.DecisionID || v.CheckID != blocked.CheckID || v.RegulationID != w.regulation.ConfigID || v.LicenseRequired ||
		!v.ValidUntil.IsZero() || v.Evidence() != "blocked/blocked_under_review/"+blocked.CheckID+"/"+blocked.DecisionID+"/false" {
		t.Fatalf("the blocked decision under review %+v", v)
	}
	_, err = q.c.TRD.ActivateOrder(q.author, q.activation(o, r))
	wantRefused(t, "blocked under review", err, cnxapi.CodeTradeComplianceNotClear, "/args/order_id")
	if v := q.getOrder(o.ObjectID); len(v.Commits) != 0 {
		t.Fatalf("the order stays a draft %+v", v)
	}
	view, err := w.cnx.GetCheck(w.compliance, blocked.CheckID)
	q.must(err)
	if view.Status != cnxapi.StatusPending || view.State != cnxapi.StatusBlocked || view.Reason != cnxapi.ReasonBlockedUnderReview ||
		view.CurrentDecisionID != "" || len(view.Evaluations) != 2 {
		t.Fatalf("the check %+v", view)
	}
	if sums, err := w.cnx.OrderChecks(w.compliance, o.ObjectID); err != nil || len(sums) != 1 || sums[0].State != cnxapi.StatusBlocked ||
		sums[0].Reason != cnxapi.ReasonBlockedUnderReview {
		t.Fatalf("order checks %+v %v", sums, err)
	}
	// A second evaluation while under review keeps the block.
	third, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, again.RowVersion, syntheticClear))
	q.must(err)
	w.wantVerdict(b, cnxapi.StatusBlocked, cnxapi.ReasonBlockedUnderReview)

	_, err = w.cnx.Decide(w.compliance, w.decideCmd(third, cnxapi.ConclusionClear, false))
	q.must(err)
	w.wantVerdict(b, cnxapi.StatusClear, cnxapi.ReasonCleared)
	_, err = q.c.TRD.ActivateOrder(q.author, q.activation(o, r))
	wantRefused(t, "cleared, without a reservation", err, stlapi.CodeCreditReservationRequired, "/args/order_id")
}

// S5R-D17 (CNX-INV-001; SM-CNX-COMPLIANCE pending → clear "INVALID_STATE /args/evidence_ids"): the
// destination is under the synthetic CN embargo, a finding that applies. Clear without an
// authorization is INVALID_STATE at /args/evidence_ids — evidence alone is none — and an
// authorization that has lapsed is refused at its valid_until; clear on an export licence the
// officer cites (an EVD source among the decision's evidence, kind license, valid 10 days) is
// accepted, its validity capped at the authorization's (the evaluation allows 30 days), and the
// decision, its event and the guard carry it. The database refuses authorizations on a blocked
// decision.
func TestS5RD17EmbargoClearNeedsAnAuthorization(t *testing.T) {
	w := newCnxWorld(t)
	_, o := w.prepared("quote.1")
	b := w.binding(o.ObjectID)
	e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, 0, syntheticEmbargoed))
	w.must(err)
	if e.Outcome != cnxapi.OutcomeMatch {
		t.Fatalf("%+v", e)
	}
	_, err = w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/evidence_ids" {
		t.Fatalf("an embargo cleared without an authorization: %s %s", code(err), path(err))
	}
	w.source(w.compliance, "auth.licence", "合成：主管机关颁发的禁运例外许可（测试数据，非真实许可）")
	clear := w.decideCmd(e, cnxapi.ConclusionClear, false)
	clear.EvidenceIDs = []string{"auth.licence", "cnx.evidence"}
	clear.Authorizations = []cnxapi.Authorization{{EvidenceID: "auth.licence", Kind: cnxapi.AuthorizationLicense,
		ValidUntil: kernel.FormatTimestamp(time.Now().Add(-time.Minute))}}
	_, err = w.cnx.Decide(w.compliance, clear)
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/authorizations/0/valid_until" {
		t.Fatalf("a lapsed authorization: %s %s", code(err), path(err))
	}
	until := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	clear.IdempotencyKey, clear.Authorizations[0].ValidUntil = w.key(), kernel.FormatTimestamp(until)
	missing := clear
	missing.IdempotencyKey, missing.EvidenceIDs = w.key(), []string{"cnx.evidence"}
	_, err = w.cnx.Decide(w.compliance, missing)
	w.want(err, kernel.CodeSchemaInvalid) // an authorization names the decision's own evidence
	d, err := w.cnx.Decide(w.compliance, clear)
	w.must(err)
	if d.Status != cnxapi.StatusClear || !d.ValidUntil.Equal(until) {
		t.Fatalf("clear on the authorization %+v", d)
	}
	v := w.wantVerdict(b, cnxapi.StatusClear, cnxapi.ReasonCleared)
	if !v.ValidUntil.Equal(until) {
		t.Fatalf("the guard's validity %+v", v)
	}
	if v := w.complianceGuard("company.A", b, until, nil, nil); v.Status != cnxapi.StatusExpired {
		t.Fatalf("the conclusion lapses with its authorization: %+v", v)
	}
	view, err := w.cnx.GetCheck(w.compliance, d.CheckID)
	w.must(err)
	dec := view.Decisions[0]
	if len(dec.Authorizations) != 1 || dec.Authorizations[0].SourceID != "auth.licence" || dec.Authorizations[0].Kind != cnxapi.AuthorizationLicense ||
		!dec.Authorizations[0].ValidUntil.Equal(until) || dec.Authorizations[0].ContentSHA256 == "" || len(dec.Evidence) != 2 {
		t.Fatalf("the decision %+v", dec)
	}
	var decided map[string]any
	for _, env := range w.events("company.A", cnxapi.Source) {
		if env.Type == cnxapi.EventTradeComplianceDecided && env.Payload["check_id"] == d.CheckID {
			decided = env.Payload
		}
	}
	if decided["conclusion"] != cnxapi.ConclusionClear || decided["valid_until"] != kernel.FormatTimestamp(until) {
		t.Fatalf("TradeComplianceDecided %v", decided)
	}
	if err := w.exec("company.A", `UPDATE brant_cnx.trade_compliance_decision SET authorizations = '[{}]'`); err == nil {
		t.Fatal("decisions never change")
	}
}

// S5R-D15 (UL PLATFORM-CountryProgrammeVersion, CNX-INV-001 "带区域的项目生成发现 region_review"): a
// new version of the synthetic CN programme list adds an embargo limited to the region UA-43; an
// order whose destination is UA is not matched by the system: the evaluation records a region_review
// finding with the region, which does not apply by itself (outcome match, for the officer), and the
// officer concludes clear with the reason every decision gives — no authorization: whether the
// consignee's address lies in UA-43 is the officer's judgement. Another country of the same list is
// still an embargo that applies.
func TestS5RD15RegionalProgrammeIsLeftToTheOfficer(t *testing.T) {
	w := newCnxWorld(t)
	list := syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-10")
	list.IdempotencyKey = w.key()
	list.Programmes = append(list.Programmes, refapi.CountryProgramme{ProgrammeID: "SYN-CN-REG-1", Name: "合成区域禁运项目（测试数据）",
		Measure: refapi.MeasureEmbargo, Countries: []string{"UA"}, Regions: []string{"UA-43"}, Scope: "该区域的全部货物（合成）"})
	list.ProgrammeCount, list.CountryCount = len(list.Programmes), list.CountryCount+1
	_, err := w.c.REF.ImportCountryProgrammes(w.compliance, list)
	w.must(err)
	_, o := w.prepared("quote.1")
	e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, 0, "UA"))
	w.must(err)
	if e.Outcome != cnxapi.OutcomeMatch {
		t.Fatalf("a region review is for the officer: %+v", e)
	}
	view, err := w.cnx.GetCheck(w.compliance, e.CheckID)
	w.must(err)
	ev := view.Evaluations[0]
	if len(ev.Findings) != 1 || ev.Findings[0].Kind != cnxapi.FindingRegionReview || ev.Findings[0].Applies ||
		strings.Join(ev.Findings[0].Regions, ",") != "UA-43" || ev.Findings[0].ProgrammeID != "SYN-CN-REG-1" ||
		strings.Join(ev.Findings[0].Roles, ",") != "destination" || ev.Programmes[0].Version != "v2" {
		t.Fatalf("findings %+v", ev.Findings)
	}
	d, err := w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
	w.must(err)
	w.wantVerdict(w.binding(o.ObjectID), cnxapi.StatusClear, cnxapi.ReasonCleared)
	if d.Status != cnxapi.StatusClear {
		t.Fatalf("%+v", d)
	}
	_, o2 := w.prepared("quote.2")
	e2, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o2.ObjectID, 1, 0, syntheticEmbargoed))
	w.must(err)
	_, err = w.cnx.Decide(w.compliance, w.decideCmd(e2, cnxapi.ConclusionClear, false))
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/evidence_ids" {
		t.Fatalf("the whole-country embargo still applies: %s %s", code(err), path(err))
	}
}

// S5R-D38 (Context Map R50; UL POL-ScreeningRegulationProfile; CNX-INV-001 "项目名单取自绑定的
// screening_regulation 修订的 programmes"): the evaluation reads the programme lists from the bound
// regulation revision — without programme_lists, and programme_lists is accepted only naming exactly
// those (INVALID_KNOWN_VALUE otherwise); a regulation that enables only the UN list evaluates against
// the UN list alone; one that enables none leaves the gap programmes_missing, never a clear one
// (TRADE_COMPLIANCE_EVALUATION_INCOMPLETE). A list the regulation enables but no one imported is the
// gap programme_list_missing.
func TestS5RD38ProgrammeListsComeFromTheRegulation(t *testing.T) {
	w := newCnxWorld(t)
	_, o := w.prepared("quote.1")
	cmd := w.evaluateCmd(o.ObjectID, 1, 0, syntheticClear)
	cmd.ProgrammeLists = nil
	e, err := w.cnx.Evaluate(w.compliance, cmd)
	w.must(err)
	view, err := w.cnx.GetCheck(w.compliance, e.CheckID)
	w.must(err)
	if e.Outcome != cnxapi.OutcomeNoMatch || strings.Join(view.Evaluations[0].ProgrammeLists, ",") != cnProgrammeList+","+unProgrammeList ||
		len(view.Evaluations[0].Programmes) != 2 {
		t.Fatalf("lists from the regulation %+v %+v", e, view.Evaluations[0])
	}
	other := w.evaluateCmd(o.ObjectID, 1, e.RowVersion, syntheticClear)
	other.ProgrammeLists = []string{unProgrammeList}
	_, err = w.cnx.Evaluate(w.compliance, other)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/programme_lists" {
		t.Fatalf("programme_lists other than the regulation's: %s %s", code(err), path(err))
	}
	same := w.evaluateCmd(o.ObjectID, 1, e.RowVersion, syntheticClear)
	same.ProgrammeLists = []string{unProgrammeList, cnProgrammeList}
	e, err = w.cnx.Evaluate(w.compliance, same)
	w.must(err)

	unOnly := w.configure(w.author, w.reviewer, polapi.KindScreeningRegulation, "screening.un", 0, w.win.from, time.Time{},
		polapi.ConfigData{ScreeningRegulation: &polapi.ScreeningRegulation{Lists: []polapi.ScreeningList{cnList}, RescreenIntervalDays: 30,
			Programmes: []polapi.ScreeningList{{ListCode: unProgrammeList, Jurisdiction: polapi.JurisdictionUN}}}}, true)
	_, o2 := w.prepared("quote.2")
	un := w.evaluateCmd(o2.ObjectID, 1, 0, syntheticEmbargoed)
	un.RegulationID, un.RegulationRevision, un.ProgrammeLists = unOnly.ConfigID, unOnly.Revision, nil
	e2, err := w.cnx.Evaluate(w.compliance, un)
	w.must(err)
	view, err = w.cnx.GetCheck(w.compliance, e2.CheckID)
	w.must(err)
	ev := view.Evaluations[0]
	if strings.Join(ev.ProgrammeLists, ",") != unProgrammeList || ev.Outcome != cnxapi.OutcomeNoMatch || len(ev.Findings) != 0 {
		t.Fatalf("the CN embargo is not enabled by the UN-only regulation: %+v", ev)
	}

	none := w.configure(w.author, w.reviewer, polapi.KindScreeningRegulation, "screening.none", 0, w.win.from, time.Time{},
		polapi.ConfigData{ScreeningRegulation: &polapi.ScreeningRegulation{Lists: []polapi.ScreeningList{cnList}, RescreenIntervalDays: 30}}, true)
	_, o3 := w.prepared("quote.3")
	bare := w.evaluateCmd(o3.ObjectID, 1, 0, syntheticClear)
	bare.RegulationID, bare.RegulationRevision, bare.ProgrammeLists = none.ConfigID, none.Revision, nil
	e3, err := w.cnx.Evaluate(w.compliance, bare)
	w.must(err)
	view, err = w.cnx.GetCheck(w.compliance, e3.CheckID)
	w.must(err)
	if e3.Outcome != cnxapi.OutcomeIncomplete || len(view.Evaluations[0].Gaps) != 1 || view.Evaluations[0].Gaps[0].Kind != "programmes_missing" {
		t.Fatalf("a regulation enabling no list: %+v", view.Evaluations[0])
	}
	_, err = w.cnx.Decide(w.compliance, w.decideCmd(e3, cnxapi.ConclusionClear, false))
	w.want(err, cnxapi.CodeTradeComplianceEvaluationIncomplete)
	bare.ProgrammeLists = []string{cnProgrammeList}
	bare.IdempotencyKey, bare.ExpectedVersion = w.key(), e3.RowVersion
	_, err = w.cnx.Evaluate(w.compliance, bare)
	w.want(err, kernel.CodeInvalidKnownValue)
}

// licensedOrder is the FUL world's paidOrder without a shipment: order "order.<quote>" of 120 PCS
// of sku.1, concluded clear with a licence required, activated and paid in full (960.00 USD).
func (w *declWorld) licensedOrder(quote string) trdapi.OrderReceipt {
	w.t.Helper()
	_, draft := w.prepared(quote)
	w.concluded(draft.ObjectID, cnxapi.ConclusionClear, true)
	d := w.reviewed(draft, "review."+quote)
	o, err := w.c.TRD.ActivateOrder(w.author, w.activateOrderCmd(draft.ObjectID, d.RowVersion, "review."+quote))
	w.must(err)
	stage := w.account(o.ObjectID).Stages[0].StageID
	w.receive("fund."+quote, "960.00", "960.00", "")
	w.stlApply("app."+quote, "fund."+quote, o.ObjectID, stage, "960.00")
	return o
}

// licence has the documentation clerk doc.1 register a synthetic licence of 1,000 PCS of sku.1 at
// USD 10,000.00 for DE and customer.1, valid 30 days, at customs office 2248, in usage mode mode
// (multi_use on the licence face's evidence), through the CNX that reads the real FUL.
func (w *declWorld) licence(number, mode string) string {
	w.t.Helper()
	doc := w.cx.doc1
	_, err := w.file(doc, "licence."+number, "licence.txt", evdapi.MimeText, "合成出口许可证 "+number+"（测试数据）", "")
	w.must(err)
	now := time.Now().UTC()
	cmd := cnxapi.RegisterLicense{IdempotencyKey: w.key(), LicenseNumber: number, Issuer: "合成发证机关（测试）", Holder: "合成出口商（测试）",
		Goods: cnxapi.LicensedGoods{SkuSpecs: []cnxapi.SpecRef{{SkuID: "sku.1", SpecRevision: w.spec}}}, LicensedQuantity: "1000", Unit: "PCS",
		Currency: cnxapi.CurrencyRef{ID: w.usd.ConfigID, Revision: w.usd.Revision}, LicensedValue: "10000.00", Destination: "DE",
		EndUser: cnxapi.EndUser{PartyID: "customer.1", Name: "合成客户"}, ValidFrom: kernel.FormatTimestamp(now.Add(-time.Hour)),
		ValidUntil: kernel.FormatTimestamp(now.Add(30 * 24 * time.Hour)), FileID: "licence." + number, UsageMode: mode, CustomsOffice: "2248"}
	if mode == cnxapi.UsageMultiUse {
		cmd.UsageEvidenceIDs = []string{w.source(doc, "face."+number, "合成许可证证面：非一批一证（测试数据）")}
	}
	r, err := w.cnx.RegisterLicense(doc, cmd)
	w.must(err)
	return r.LicenseID
}

func (w *declWorld) allocateTo(licence, allocation, kind, target string) (cnxapi.LicenseReceipt, error) {
	return w.cnx.AllocateLicense(w.cx.doc1, cnxapi.AllocateLicense{IdempotencyKey: w.key(), LicenseID: licence, AllocationID: allocation,
		Target: cnxapi.ExportTarget{Kind: kind, ID: target}})
}

// S5R-D04 on real FUL records (CNX-INV-002 positive "两证两批次的出运可确认", negative "缺一个批次的分配
// 返回 EXPORT_LICENSE_REQUIRED", "single_use 许可证第二次分配返回 INVALID_STATE", "申报海关不符返回
// INVALID_KNOWN_VALUE"; R56 ExportLicenseGuard v1 "出运的每一行由出运本身的分配或包含该行的出口批次的分配
// 覆盖"): an order whose goods need a licence ships 60 + 60 PCS in one shipment of two lines, split into
// two export lots, one per licence (一份报关单一张许可证). While no allocation covers it the commercial
// release is EXPORT_LICENSE_REQUIRED (not_allocated); with lot.a's allocation only, still refused
// (lines_not_covered) though the guard sees lot.a's allocation; the single_use licence X is not
// allocated a second time, and the shipment is not allocated licence Y beside its lots'. Once lot.b
// holds Y, FUL's release is granted on both (lots_allocated) and the shipment confirms. Each lot's
// declaration names the licence's customs office (一证一关): another is refused at preparation.
func TestS5RD04TwoLicencesThroughTwoExportLotsReleaseTheShipment(t *testing.T) {
	w := newDeclWorld(t)
	o := w.licensedOrder("quote.lic")
	restore := w.onOrder(o)
	defer restore()
	w.shipment("shipment.two", "60", "commitment.lic.a", "commitment.lic.b")
	r, err := w.evaluateRelease(w.ful, "shipment.two", fulapi.ScopeShipment)
	if err != nil || r.Code != fulapi.CodeExportLicenseRequired || !strings.Contains(r.Reason, cnxapi.LicenseReasonNotAllocated) {
		t.Fatalf("not allocated %+v %v", r, err)
	}
	_, err = w.registerLot(w.merch, "lot.a", "shipment.two", lotLine("E1", "S1", "60"))
	w.must(err)
	_, err = w.registerLot(w.merch, "lot.b", "shipment.two", lotLine("E2", "S2", "60"))
	w.must(err)
	x, y := w.licence("SYN-EL-D04-X", cnxapi.UsageSingleUse), w.licence("SYN-EL-D04-Y", "")
	a, err := w.allocateTo(x, "alloc.a", cnxapi.TargetExportLot, "lot.a")
	if err != nil || a.Usage.AllocatedQuantity != "60" {
		t.Fatalf("lot.a %+v %v", a, err)
	}
	r, err = w.evaluateRelease(w.ful, "shipment.two", fulapi.ScopeShipment)
	if err != nil || r.Code != fulapi.CodeExportLicenseRequired || !strings.Contains(r.Reason, cnxapi.LicenseReasonLinesNotCovered) {
		t.Fatalf("lot.b without an allocation %+v %v", r, err)
	}
	sv, err := w.cnx.ShipmentLicense(w.cx.doc1, "shipment.two")
	w.must(err)
	if !sv.Covered || sv.Reason != cnxapi.LicenseReasonLotsAllocated || sv.LicenseID != x || sv.AllocationID != "alloc.a" || sv.Quantity != "60" {
		t.Fatalf("the guard sees lot.a's allocation %+v", sv)
	}
	_, err = w.allocateTo(x, "alloc.b.x", cnxapi.TargetExportLot, "lot.b")
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/license_id" {
		t.Fatalf("a single_use licence a second time: %s %s", code(err), path(err))
	}
	_, err = w.allocateTo(y, "alloc.whole", cnxapi.TargetShipment, "shipment.two")
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/target/id" {
		t.Fatalf("the shipment beside its lots: %s %s", code(err), path(err))
	}
	_, err = w.allocateTo(y, "alloc.b", cnxapi.TargetExportLot, "lot.b")
	w.must(err)
	g := w.granted("shipment.two")
	b := w.releaseView(g.ReleaseID).Evaluations[len(w.releaseView(g.ReleaseID).Evaluations)-1].Bindings
	ids := []string{x, y}
	slices.Sort(ids)
	if b.ExportLicense == nil || b.ExportLicense.Status != cnxapi.LicenseStatusAllocated || b.ExportLicense.LicenseID != strings.Join(ids, ",") ||
		b.ExportLicense.AllocationID != "alloc.a,alloc.b" || b.ExportLicense.Quantity != "120" || b.ExportLicense.Unit != "PCS" {
		t.Fatalf("the release binds both allocations %+v", b.ExportLicense)
	}
	if c, err := w.confirm(w.merch, "shipment.two", 1); err != nil || c.Status != fulapi.StatusConfirmed ||
		!strings.Contains(guardValue(w.fulWorld.shipment("shipment.two"), fulapi.GuardExportLicense), cnxapi.LicenseReasonLotsAllocated) {
		t.Fatalf("two licences, two lots: the shipment confirms %+v %v", c, err)
	}
	l, err := w.cnx.GetLicense(w.cx.doc1, x)
	w.must(err)
	if al := l.Allocations[0]; l.UsageMode != cnxapi.UsageSingleUse || l.CustomsOffice != "2248" || al.ShipmentID != "shipment.two" ||
		al.EndUserSource != cnxapi.EndUserCustomer || al.Lines[0].ShipmentLineID != "S1" || al.Lines[0].LineID != "E1" {
		t.Fatalf("licence X %+v", l)
	}
	if l, err := w.cnx.GetLicense(w.cx.doc1, y); err != nil || l.UsageMode != cnxapi.UsageSingleUse {
		t.Fatalf("usage_mode defaults to single_use: %+v %v", l, err)
	}

	_, err = w.cnx.PrepareDeclaration(w.cx.doc1, cnxapi.PrepareDeclaration{IdempotencyKey: w.key(), ExportLotID: "lot.a", CodeTableVersion: codeTable,
		Items: items("E1"), CustomsOffice: "2249"})
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/customs_office" {
		t.Fatalf("another customs office: %s %s", code(err), path(err))
	}
	_, err = w.cnx.PrepareDeclaration(w.cx.doc1, cnxapi.PrepareDeclaration{IdempotencyKey: w.key(), ExportLotID: "lot.a", CodeTableVersion: codeTable,
		Items: items("E1")})
	w.want(err, kernel.CodeInvalidKnownValue)
	w.d.office = "2248"
	c := w.d.confirmed("lot.a", items("E1"))
	v, err := w.cnx.GetDeclaration(w.cx.doc1, c.DeclarationID)
	w.must(err)
	if v.Versions[0].CustomsOffice != "2248" || v.Status != cnxapi.DeclarationConfirmed {
		t.Fatalf("the declaration names the licence's office %+v", v.Versions[0])
	}
}

// S5R-D03 on real FUL records (CNX-INV-002 "分配的目标在 FUL 中已取消、且 CNX 中没有覆盖该目标的已提交或之后
// 状态的报关案件时，单证岗位人工可附理由以 basis local_cancellation 释放全部未消耗用量"; MD-44): a single_use
// licence allocated to export lot lot.c is not released on a local cancellation while FUL has not
// cancelled it (INVALID_STATE /args/allocation_id); once lot.c's declaration is submitted and the
// shipment cancelled, not while the declaration stands (INVALID_STATE /args/basis: declared usage is
// released on its external result); customs rejects the declaration, and the release on the local
// cancellation then frees everything outstanding — recorded with its basis, the kind cancelled, the
// lot's version and no receipt — and the single_use licence, never declared, is allocated again. An
// AI agent is refused (human only).
func TestS5RD03LocalCancellationReleasesAnUndeclaredAllocation(t *testing.T) {
	w := newDeclWorld(t)
	w.shipment("shipment.c", "50", "commitment.1")
	_, err := w.registerLot(w.merch, "lot.c", "shipment.c", lotLine("E1", "S1", "50"))
	w.must(err)
	x := w.licence("SYN-EL-D03", cnxapi.UsageSingleUse)
	a, err := w.allocateTo(x, "alloc.c", cnxapi.TargetExportLot, "lot.c")
	w.must(err)
	release := func(version int64, id string) (cnxapi.LicenseReceipt, error) {
		return w.cnx.ReleaseLicense(w.cx.doc1, cnxapi.ReleaseLicense{IdempotencyKey: w.key(), LicenseID: x, ExpectedVersion: version,
			AllocationID: "alloc.c", ReleaseID: id, Basis: cnxapi.BasisLocalCancellation, Reason: "出运在报关前取消（合成）"})
	}
	_, err = release(a.RowVersion, "rel.c.1")
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/allocation_id" {
		t.Fatalf("a lot FUL has not cancelled: %s %s", code(err), path(err))
	}
	w.d.office = "2248"
	submitted := w.d.submitted("lot.c", items("E1"))
	_, err = w.ful.CancelShipment(w.merch, fulapi.CancelShipment{IdempotencyKey: w.key(), ShipmentID: "shipment.c",
		ExpectedVersion: w.fulWorld.shipment("shipment.c").RowVersion, Reason: "客户取消（合成）"})
	w.must(err)
	_, err = release(a.RowVersion, "rel.c.2")
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/basis" {
		t.Fatalf("a submitted declaration: %s %s", code(err), path(err))
	}
	_, err = w.cnx.RecordReceipt(w.cx.doc1, w.d.receipt(submitted, cnxapi.DeclarationRejected, 1))
	w.must(err)
	agent := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent.lic"), DataScope: "company",
		Permissions: []string{"export_license.release", "cnx.read", "evd.read"}})
	_, err = w.cnx.ReleaseLicense(agent, cnxapi.ReleaseLicense{IdempotencyKey: w.key(), LicenseID: x, ExpectedVersion: a.RowVersion,
		AllocationID: "alloc.c", ReleaseID: "rel.c.ai", Basis: cnxapi.BasisLocalCancellation, Reason: "AI 试图释放"})
	w.want(err, kernel.CodePermissionDenied)
	r, err := release(a.RowVersion, "rel.c.3")
	if err != nil || r.Usage.AllocatedQuantity != "0" || r.Usage.AvailableQuantity != "1000" || r.ReleaseID != "rel.c.3" {
		t.Fatalf("released on the local cancellation %+v %v", r, err)
	}
	l, err := w.cnx.GetLicense(w.cx.doc1, x)
	w.must(err)
	rel := l.Allocations[0].Releases[0]
	if l.Allocations[0].Status != cnxapi.AllocationReleased || rel.Basis != cnxapi.BasisLocalCancellation || rel.Kind != cnxapi.ReleaseCancelled ||
		rel.Source != cnxapi.ResultLocalCancellation || rel.DeclarationNumber != "" || !rel.ReceivedAt.IsZero() || rel.Quantity != "50" ||
		rel.TargetVersion != w.exportLot("lot.c").RowVersion || len(rel.Evidence) != 0 {
		t.Fatalf("the release %+v", rel)
	}
	_, err = release(r.RowVersion, "rel.c.4")
	w.want(err, kernel.CodeInvalidState) // nothing outstanding
	w.shipment("shipment.d", "10", "commitment.2")
	if _, err := w.allocateTo(x, "alloc.d", cnxapi.TargetShipment, "shipment.d"); err != nil {
		t.Fatalf("a single_use licence never declared is allocated again: %v", err)
	}

	// S5R-D04 (一证一关): a lot whose confirmed declaration names another customs office is not
	// allocated the licence.
	w.shipment("shipment.e", "10", "commitment.3")
	_, err = w.registerLot(w.merch, "lot.e", "shipment.e", lotLine("E1", "S1", "10"))
	w.must(err)
	w.d.office = "2249"
	w.d.confirmed("lot.e", items("E1"))
	e := w.licence("SYN-EL-D03-E", cnxapi.UsageSingleUse)
	if _, err := w.allocateTo(e, "alloc.e", cnxapi.TargetExportLot, "lot.e"); code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/target/id" {
		t.Fatalf("a declaration at another customs office: %s %s", code(err), path(err))
	}
}

// S5R-D04 and D40 on ExportLicenseGuard v1's rows (R56 guardLock): export lots lot.1 and lot.2 of
// ship.L (FUL's port states their shipment lines and the end user the shipment states) hold
// licences X and Y; the guard covers ship.L's two lines together (lots_allocated) and a downstream
// holding it makes a release of Y wait until it commits — it acted on both allocations; once Y's
// allocation is released in part (an amended receipt) the guard answers required/quantity_exceeded,
// and past X's validity expired. An allocation records where its end user came from; a stated end
// user other than the licence's is EXPORT_LICENSE_EXPIRED.
func TestS5RD04LotAllocationsCoverTheShipmentInTheGuard(t *testing.T) {
	w := newLicenseWorld(t)
	x, y := w.register("SYN-EL-LOT-X"), w.register("SYN-EL-LOT-Y")
	lot := func(id, line, quantity string, valueMinor int64) {
		v, err := kernel.MoneyFromMinor(kernel.MustCurrency("USD", 2), big.NewInt(valueMinor))
		w.must(err)
		w.ful.put(cnx.Export{Ref: cnx.ExportRef{Kind: cnxapi.TargetExportLot, ID: id}, Status: cnx.ExportLotRegistered, Version: 1, OrderID: "order.1",
			OrderRevision: 1, Destination: "DE", EndUserPartyID: "customer.1", EndUserSource: cnxapi.EndUserStated, Value: v,
			Lines: []cnx.ExportLine{{LineID: id + ".e1", SkuID: "sku.1", SpecRevision: 1, Quantity: kernel.MustDecimal(quantity), Unit: "PCS",
				ShipmentID: "ship.L", ShipmentLineID: line}}})
	}
	lot("lot.1", "ship.L.l1", "600", 600000)
	lot("lot.2", "ship.L.l2", "400", 400000)
	allocate := func(licence, allocation, target string) (cnxapi.LicenseReceipt, error) {
		return w.cnx.AllocateLicense(w.doc1, cnxapi.AllocateLicense{IdempotencyKey: w.key(), LicenseID: licence, AllocationID: allocation,
			Target: cnxapi.ExportTarget{Kind: cnxapi.TargetExportLot, ID: target}})
	}
	_, err := allocate(x.LicenseID, "alloc.1", "lot.1")
	w.must(err)
	ya, err := allocate(y.LicenseID, "alloc.2", "lot.2")
	w.must(err)
	lines := []cnxapi.ShipmentLine{{LineID: "ship.L.l1", SkuID: "sku.1", SpecRevision: 1, Quantity: "600", Unit: "PCS"},
		{LineID: "ship.L.l2", SkuID: "sku.1", SpecRevision: 1, Quantity: "400", Unit: "PCS"}}
	v := w.wantLicenseVerdict("ship.L", cnxapi.LicenseStatusAllocated, cnxapi.LicenseReasonLotsAllocated)
	ids := []string{x.LicenseID, y.LicenseID}
	slices.Sort(ids)
	if err := v.Check("/args/shipment_id", lines); err != nil || v.LicenseID != strings.Join(ids, ",") || v.AllocationID != "alloc.1,alloc.2" ||
		v.Quantity != "1000" || v.ValueMinor != "1000000" || v.Currency != "USD" {
		t.Fatalf("lots_allocated %+v %v", v, err)
	}
	if err := v.Check("/args/shipment_id", lines[:1]); code(err) != cnxapi.CodeExportLicenseRequired {
		t.Fatalf("a line the lots do not hold: %v", err)
	}
	l, err := w.cnx.GetLicense(w.doc1, x.LicenseID)
	w.must(err)
	if al := l.Allocations[0]; al.EndUserSource != cnxapi.EndUserStated || al.ShipmentID != "ship.L" || al.Lines[0].ShipmentLineID != "ship.L.l1" {
		t.Fatalf("the allocation %+v", al)
	}

	pid, held, release := make(chan int32, 1), make(chan struct{}), make(chan struct{})
	seen := make(chan cnxapi.ExportLicenseVerdict, 1)
	go func() {
		seen <- w.licenseGuard("company.A", "ship.L", time.Now(), pid, func() { close(held); <-release })
	}()
	p := <-pid
	<-held
	done := make(chan error, 1)
	go func() {
		cmd := w.releaseCmd(y.LicenseID, ya.RowVersion, "alloc.2", "rel.2", cnxapi.ExternalAmended)
		_, err := w.cnx.ReleaseLicense(w.doc2, cmd)
		done <- err
	}()
	waitBlockedBy(t, w.pool, p, done)
	close(release)
	if s := <-seen; s.Reason != cnxapi.LicenseReasonLotsAllocated || !s.Covered {
		t.Fatalf("the downstream saw %+v", s)
	}
	w.must(<-done)
	w.wantLicenseVerdict("ship.L", cnxapi.LicenseStatusRequired, cnxapi.LicenseReasonQuantityExceeded)
	if v := w.licenseGuard("company.A", "ship.L", time.Now().Add(31*24*time.Hour), nil, nil); v.Covered || v.Status != cnxapi.LicenseStatusRequired {
		t.Fatalf("released in part, then expired: %+v", v)
	}

	// S5R-D40: the shipment states another end user than the licence's.
	other := w.register("SYN-EL-LOT-Z")
	lot("lot.3", "ship.M.l1", "10", 10000)
	z := w.ful.exports[cnx.ExportRef{Kind: cnxapi.TargetExportLot, ID: "lot.3"}]
	z.EndUserPartyID = "end.user.9"
	w.ful.put(z)
	if _, err := allocate(other.LicenseID, "alloc.3", "lot.3"); code(err) != cnxapi.CodeExportLicenseExpired || path(err) != "/args/target/id" {
		t.Fatalf("a stated end user other than the licence's: %s %s", code(err), path(err))
	}
}
