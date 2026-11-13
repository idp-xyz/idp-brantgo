//go:build integration

package bootstrap_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The documents release of a confirmed shipment after an order change (owner decision 2026-11-12,
// S5 unit C1b, a follow-up of WBS S5-07; FUL-INV-002, SM-FUL-RELEASE, WF-07, BT10, D-5, D-11, R27,
// R45) on PostgreSQL 16 with TRD's, STL's, CNX's, PTY's and QLT's real guard functions: the
// documents release (commercial_release.evaluate scope documents, grant_exception,
// document_release.record) of a confirmed shipment judges STL's payment condition for the documents
// scope on the order's current revision, reuses what the shipment's confirmation bound for the goods
// and admits an order closed by order.close or order.close_remainder. The shipment scope keeps
// judging the revision the shipment binds. Python V0.50 has no commercial release: the tests cite
// the design.

// documentsPlan is 30 % on order confirmation and 70 % after shipment under document control
// (D-5: paid against the documents, required by the documents scope only, secured without a credit
// reservation): 288.00 + 672.00 of the world's 960.00.
var documentsPlan = &polapi.PaymentTerms{Name: "30% on order confirmation, 70% after shipment against documents", Stages: []polapi.PaymentStage{
	{StageID: "P1", Percent: "30", Trigger: polapi.TriggerOrderConfirmed},
	{StageID: "P2", Percent: "70", Trigger: polapi.TriggerAfterShipment, Days: 30, ReleaseControl: polapi.ReleaseControlDocuments}}}

// newDocumentsWorld is the FUL world on documentsPlan with only the deposit paid: the fixture's
// application of 672.00 to P2 is taken off again (its receipt stays, unapplied), so the customer owes
// the balance against the documents.
func newDocumentsWorld(t *testing.T) *fulWorld {
	t.Helper()
	paymentPlans.Store(t, documentsPlan)
	defer paymentPlans.Delete(t)
	w := newFulWorld(t)
	_, err := w.c.STL.Unapply(w.treasury, stlapi.Unapply{IdempotencyKey: w.key(), ApplicationID: "app." + w.order.ObjectID + ".P2", ExpectedVersion: 1,
		Reason: "合成：尾款凭单据支付，撤销夹具的核销"})
	w.must(err)
	return w
}

// amendL1 has the author prepare and submit amendment id of the world's order (L1 to qty), the
// reviewer approve and apply it.
func (w *fulWorld) amendL1(id, qty string) {
	w.t.Helper()
	order := w.order.ObjectID
	v := w.getOrder(order)
	d := w.approvedAmendment(order, id, v.OrderRevision, v.OrderHash, qty)
	_, err := w.c.TRD.ApplyAmendment(w.reviewer, w.applyCmd(order, id, d.RowVersion))
	w.must(err)
}

func (w *fulWorld) recordDocuments(id string, expected int64, kind string) (fulapi.ReleaseReceipt, error) {
	return w.ful.RecordDocumentRelease(w.doc, fulapi.RecordDocumentRelease{IdempotencyKey: w.key(), ShipmentID: id, ExpectedVersion: expected,
		ReleaseKind: kind, EvidenceIDs: []string{"evidence"}})
}

// exceptionOnRevision2 is SQL that grants blocked release id by an exception on order revision 2
// with hash, as the release commands write one (the release moved blocked → granted, then the
// exception row) — for asOwner, which rolls back.
func exceptionOnRevision2(id, hash string) string {
	return `UPDATE brant_ful.commercial_release SET status = 'granted', row_version = row_version + 1, exceptions = exceptions + 1
		WHERE release_id = '` + id + `';
		INSERT INTO brant_ful.commercial_release_exception (tenant_id, company_id, release_id, seq, release_version, order_id, order_revision,
		order_hash, stage_id, currency, shortfall_minor, valid_until, reason, evidence_ids, guard_entries, guard_entries_sha256, granted_by_kind,
		granted_by_id, grant_epoch, granted_at)
		SELECT tenant_id, company_id, release_id, exceptions, row_version, order_id, 2, '` + hash + `', 'P2', 'USD', 1, now() + interval '1 day',
		'x', '{}', '[]', encode(sha256('[]'::bytea), 'hex'), 'human', 'manager.1', 1, now() FROM brant_ful.commercial_release
		WHERE release_id = '` + id + `'`
}

// latestEvaluation is the latest evaluation of release id.
func (w *fulWorld) latestEvaluation(id string) fulapi.EvaluationView {
	w.t.Helper()
	v := w.releaseView(id)
	if len(v.Evaluations) == 0 {
		w.t.Fatalf("release %s has no evaluation", id)
	}
	return v.Evaluations[len(v.Evaluations)-1]
}

// shipmentRelease is shipment id's shipment release, consumed by its confirmation.
func (w *fulWorld) shipmentRelease(id string) fulapi.ReleaseView {
	w.t.Helper()
	list, err := w.ful.ShipmentReleases(w.doc, id)
	w.must(err)
	if len(list.Releases) == 0 || list.Releases[0].Scope != fulapi.ScopeShipment || list.Releases[0].Status != fulapi.ReleaseConsumed {
		w.t.Fatalf("the shipment release of %s %+v", id, list)
	}
	return list.Releases[0]
}

// guardNames are the names of an evaluation's or consumption's guard evidence.
func guardNames(gs []fulapi.GuardEvidence) string {
	var out []string
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return strings.Join(out, " ")
}

func guardOf(gs []fulapi.GuardEvidence, name string) string {
	for _, g := range gs {
		if g.Name == name {
			return g.Value
		}
	}
	return ""
}

// wantReused requires evaluation e to have judged confirmed shipment id's documents release by the
// owner's rules: the goods' bindings are those of the evaluation its confirmation consumed, named
// with the confirmation's evidence SHA-256; the payment condition and the order were judged on
// revision; PUR and QLT were not asked; CNX was asked on revision 1 (the goods left under it) and on
// revision when later, PTY on the customer, with no block (S5 C1c).
func (w *fulWorld) wantReused(id string, e fulapi.EvaluationView, revision int64) {
	w.t.Helper()
	conf := w.shipmentRelease(id)
	src := conf.Evaluations[len(conf.Evaluations)-1].Bindings
	b, c := e.Bindings, e.Bindings.Confirmation
	if c == nil || c.ReleaseID != conf.ReleaseID || c.ReleaseVersion != conf.RowVersion || c.GuardsSHA256 != conf.Consumption.GuardsSHA256 ||
		c.OrderRevision != 1 || c.EvaluationSeq != int64(len(conf.Evaluations)) || len(c.Reused) == 0 ||
		b.TradeCompliance == nil || b.TradeCompliance.CheckID != src.TradeCompliance.CheckID || len(b.Screenings) != len(src.Screenings) ||
		b.Screenings[0].ScreeningID != src.Screenings[0].ScreeningID || b.QualityReleases[0].ReleaseID != src.QualityReleases[0].ReleaseID ||
		b.Deliveries[0].VerifiedVersion != src.Deliveries[0].VerifiedVersion {
		w.t.Fatalf("the reused confirmation of %s: %+v\nwant from %+v", id, b, src)
	}
	cnx := fulapi.GuardTradeCompliance
	if revision > 1 {
		cnx += " " + fulapi.GuardTradeComplianceCurrent
	}
	if b.Payment == nil || b.Order == nil || b.Order.OrderRevision != revision || b.Order.OrderHash != w.getOrder(w.order.ObjectID).OrderHash ||
		guardOf(e.Guards, fulapi.GuardConfirmation) != conf.ReleaseID+"/v2/consumed/"+conf.Consumption.GuardsSHA256+"/r1" ||
		guardNames(e.Guards) != "guards_checked_at "+fulapi.GuardConfirmation+" "+cnx+" screening:customer.1 "+fulapi.GuardPaymentCondition+" "+
			fulapi.GuardOrder || !strings.HasSuffix(guardOf(e.Guards, fulapi.GuardTradeCompliance), "/r1") {
		w.t.Fatalf("the documents evaluation of %s on revision %d: %+v %+v", id, revision, b.Order, e.Guards)
	}
	r := b.Rechecked
	if r == nil || len(r.TradeCompliance) != len(strings.Fields(cnx)) || r.TradeCompliance[0].OrderRevision != 1 ||
		r.TradeCompliance[len(r.TradeCompliance)-1].OrderRevision != revision || len(r.Screenings) != 1 || r.Screenings[0].PartyID != "customer.1" {
		w.t.Fatalf("the documents evaluation of %s asked CNX and PTY again: %+v", id, r)
	}
	for _, x := range r.TradeCompliance {
		if x.Blocked {
			w.t.Fatalf("the documents evaluation of %s found a block: %+v", id, r)
		}
	}
}

// BT10 ("部分已出运后减量…保留已发生部分") with the owner's rule of 2026-11-12, on 30 % deposit and 70 % against
// documents (D-5), the deposit paid: of 120 PCS on L1, shipment.1 (40), shipment.2 (30) and
// shipment.4 (10) are confirmed on revision 1, shipment.3 (40) is prepared on it. Before the change
// the documents scope already waits for P2 (672.00). The customer reduces L1 to 110 (revision 2,
// 880.00: P1 264.00, P2 616.00).
//
//   - The shipment scope of shipment.3, prepared on revision 1, stays REFERENCE_STALE (evaluate and
//     confirm); once STL's account moved to revision 2 its exception is refused REFERENCE_STALE.
//   - shipment.1's documents release judges revision 2: COMMERCIAL_RELEASE_BLOCKED while the payment
//     plan review is pending, then for P2 of revision 2 (616.00 short) — never REFERENCE_STALE —,
//     binding the confirmation's goods; the credit manager's exception for P2 of revision 2 grants it
//     and binds revision 2; the documentation clerk records the original B/L on the issued B/L
//     record, the consumption and DocumentsReleased stating revision 2.
//   - shipment.2's documents release, without an exception of its own, is blocked until the
//     treasurer applies 616.00 to P2, then granted and its telex release recorded. The database
//     admits an exception on revision 2 for it, never for shipment.3's shipment release, which binds
//     revision 1.
//   - order.cancel is ORDER_HAS_SHIPMENTS (a cancelled order with a confirmed shipment is not reached);
//     the remainder is closed short (80 of 110): shipment.4's documents release is still granted on
//     the closed order's revision 2 and recorded.
func TestDocumentsReleaseOfAConfirmedShipmentAfterAReductionBT10(t *testing.T) {
	w := newDocumentsWorld(t)
	order := w.order.ObjectID
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "40", d, rel)
	w.confirmed("shipment.2", "30", d, rel)
	w.confirmed("shipment.4", "10", d, rel)
	_, err := w.prepare(w.merch, "shipment.3", "40", d, rel)
	w.must(err)
	for _, id := range []string{"shipment.1", "shipment.2", "shipment.4"} {
		w.issuedBillOfLading(w.ful, id) // S5-08: documents are released on the issued B/L record
	}
	r, err := w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeDocuments)
	if err != nil || r.Code != fulapi.CodeCommercialReleaseBlocked || !strings.Contains(r.Reason, "P2 (after_shipment) has 0 of 67200 USD applied") {
		t.Fatalf("revision 1's documents scope %+v %v", r, err)
	}
	docs1 := r.ReleaseID
	w.wantReused("shipment.1", w.latestEvaluation(docs1), 1)

	w.amendL1("amendment.110", "110")
	v2 := w.getOrder(order)
	if v2.OrderRevision != 2 {
		t.Fatalf("amended %+v", v2)
	}
	r, err = w.evaluateRelease(w.ful, "shipment.3", fulapi.ScopeShipment)
	if err != nil || r.Status != fulapi.ReleaseBlocked || r.Code != fulapi.CodeReferenceStale || r.Guard != fulapi.GuardOrder {
		t.Fatalf("the prepared shipment of revision 1 %+v %v", r, err)
	}
	stale := r.ReleaseID
	_, err = w.confirm(w.merch, "shipment.3", 1)
	w.want(err, fulapi.CodeReferenceStale)
	r, err = w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeDocuments)
	if err != nil || r.Code != fulapi.CodeCommercialReleaseBlocked || !strings.Contains(r.Reason, "payment plan review is pending") {
		t.Fatalf("revision 2 before STL's account moved %+v %v", r, err)
	}
	if e := w.latestEvaluation(docs1); e.Bindings.Payment.Reason != stlapi.PaymentRevisionMismatch || e.Bindings.Order.OrderRevision != 2 {
		t.Fatalf("judged on revision 2 %+v", e.Bindings)
	}

	w.amendedReleasable(order)
	r, err = w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeDocuments)
	if err != nil || r.Code != fulapi.CodeCommercialReleaseBlocked || r.RowVersion != 3 ||
		!strings.Contains(r.Reason, "P2 (after_shipment) has 0 of 61600 USD applied: 61600 short for documents") {
		t.Fatalf("revision 2's documents scope %+v %v", r, err)
	}
	e := w.latestEvaluation(docs1)
	w.wantReused("shipment.1", e, 2)
	if p := e.Bindings.Payment; p.OrderRevision != 2 || p.StageID != "P2" || p.ShortfallMinor != "61600" || p.RequiredMinor != "88000" ||
		p.AppliedMinor != "26400" || e.Bindings.Order.Status != trdapi.OrderActive {
		t.Fatalf("revision 2's payment condition %+v", p)
	}
	_, err = w.ful.GrantReleaseException(w.manager, fulapi.GrantReleaseException{IdempotencyKey: w.key(), ShipmentID: "shipment.3",
		Scope: fulapi.ScopeShipment, ExpectedVersion: 1, StageID: "P1", Currency: "USD", ShortfallMinor: "100",
		ValidUntil: kernel.FormatTimestamp(time.Now().Add(time.Hour)), Reason: "合成", EvidenceIDs: []string{"evidence"}})
	w.want(err, fulapi.CodeReferenceStale)
	if v := w.releaseView(stale); v.Status != fulapi.ReleaseBlocked || len(v.Exceptions) != 0 {
		t.Fatalf("the stale shipment release %+v", v)
	}

	// The credit manager's exception covers P2 of revision 2.
	ex := fulapi.GrantReleaseException{IdempotencyKey: w.key(), ShipmentID: "shipment.1", Scope: fulapi.ScopeDocuments, ExpectedVersion: 3, StageID: "P2",
		Currency: "USD", ShortfallMinor: "61600", ValidUntil: kernel.FormatTimestamp(time.Now().Add(48 * time.Hour)),
		Reason: "合成：客户书面承诺尾款随到港付清", EvidenceIDs: []string{"evidence"}}
	g, err := w.ful.GrantReleaseException(w.manager, ex)
	if err != nil || g.Status != fulapi.ReleaseGranted || g.RowVersion != 4 {
		t.Fatalf("the exception on revision 2 %+v %v", g, err)
	}
	if x := w.releaseView(docs1).Exceptions; len(x) != 1 || x[0].Order.OrderRevision != 2 || x[0].Order.OrderHash != v2.OrderHash {
		t.Fatalf("the exception binds revision 2 %+v", x)
	}
	if ev := w.fulEventsOf(fulapi.EventCommercialReleaseGranted); len(ev) == 0 || ev[len(ev)-1].Payload["payment_order_revision"] != float64(2) ||
		ev[len(ev)-1].Payload["order_revision"] != float64(1) {
		t.Fatalf("CommercialReleaseGranted %+v", ev)
	}
	_, err = w.recordDocuments("shipment.1", 3, fulapi.ReleaseOriginalBL)
	w.want(err, kernel.CodeVersionConflict)
	c, err := w.recordDocuments("shipment.1", 4, fulapi.ReleaseOriginalBL)
	if err != nil || c.Status != fulapi.ReleaseConsumed || c.RowVersion != 5 {
		t.Fatalf("the original B/L of shipment.1 %+v %v", c, err)
	}
	v := w.releaseView(docs1)
	cs := v.Consumption.Guards
	if v.Order.OrderRevision != 1 || !strings.Contains(guardOf(cs, fulapi.GuardPaymentCondition), "/r2/") ||
		!strings.HasSuffix(guardOf(cs, fulapi.GuardPaymentCondition), "/exception=1") || guardOf(cs, fulapi.GuardOrder) != "active/r2" ||
		!strings.HasPrefix(guardOf(cs, fulapi.GuardConfirmation), w.shipmentRelease("shipment.1").ReleaseID+"/v2/consumed/") ||
		!strings.HasPrefix(guardOf(cs, fulapi.GuardTradeCompliance), "clear/cleared/") || !strings.HasSuffix(guardOf(cs, fulapi.GuardTradeCompliance), "/r1") ||
		!strings.HasPrefix(guardOf(cs, fulapi.GuardTradeComplianceCurrent), "clear/cleared/") || v.Consumption.BillOfLading == nil {
		t.Fatalf("the consumption %+v", v.Consumption)
	}
	if ev := w.fulEventsOf(fulapi.EventDocumentsReleased); len(ev) != 1 || ev[0].Payload["order_revision"] != float64(1) ||
		ev[0].Payload["payment_order_revision"] != float64(2) || ev[0].Payload["payment_order_hash"] != v2.OrderHash {
		t.Fatalf("DocumentsReleased %+v", ev)
	}

	// shipment.2: no exception of its own; granted once the balance of revision 2 is applied.
	r, err = w.evaluateRelease(w.ful, "shipment.2", fulapi.ScopeDocuments)
	if err != nil || r.Code != fulapi.CodeCommercialReleaseBlocked || !strings.Contains(r.Reason, "61600 short") {
		t.Fatalf("shipment.2 before the balance %+v %v", r, err)
	}
	if err := w.asOwner(exceptionOnRevision2(r.ReleaseID, v2.OrderHash)); err != nil {
		t.Fatalf("the database on an exception of a confirmed shipment's documents release on revision 2: %v", err)
	}
	if err := w.asOwner(exceptionOnRevision2(stale, v2.OrderHash)); err == nil || !strings.Contains(err.Error(), "does not grant release") {
		t.Fatalf("the database on an exception of a revision-1 shipment release on revision 2: %v", err)
	}
	w.stlApply("app.balance", "fund."+order+".P2", order, "P2", "616.00")
	r = w.grantedOn(w.ful, "shipment.2", fulapi.ScopeDocuments)
	if e := w.latestEvaluation(r.ReleaseID); e.Bindings.Payment.Status != stlapi.PaymentSatisfied || e.Bindings.Payment.AppliedMinor != "88000" {
		t.Fatalf("paid %+v", e.Bindings.Payment)
	}
	w.wantReused("shipment.2", w.latestEvaluation(r.ReleaseID), 2)
	if c, err := w.recordDocuments("shipment.2", r.RowVersion, fulapi.ReleaseTelexRelease); err != nil || c.Status != fulapi.ReleaseConsumed {
		t.Fatalf("the telex release of shipment.2 %+v %v", c, err)
	}

	// An order with a confirmed shipment is never cancelled; closed short, it still releases the
	// documents of what shipped.
	_, err = w.endCancel(order)
	wantRefused(t, "cancel after 80 shipped", err, trdapi.CodeOrderHasShipments, "/args/order_id")
	_, err = w.endShort(order)
	w.must(err)
	if line, end := w.ended(order); line != "110 80 30" || end.Status != trdapi.OrderClosed || end.OrderRevision != 2 {
		t.Fatalf("closed short %s %+v", line, end)
	}
	r = w.grantedOn(w.ful, "shipment.4", fulapi.ScopeDocuments)
	e = w.latestEvaluation(r.ReleaseID)
	w.wantReused("shipment.4", e, 2)
	if e.Bindings.Order.Status != trdapi.OrderClosed || guardOf(e.Guards, fulapi.GuardOrder) != "closed/r2" {
		t.Fatalf("the closed order's documents release %+v %+v", e.Bindings.Order, e.Guards)
	}
	if c, err := w.recordDocuments("shipment.4", r.RowVersion, fulapi.ReleaseOriginalBL); err != nil || c.Status != fulapi.ReleaseConsumed {
		t.Fatalf("the original B/L of shipment.4 after the order closed short %+v %v", c, err)
	}
	// What shipped is unchanged: the confirmed shipments bind revision 1.
	if sh := w.shipment("shipment.1"); sh.Status != fulapi.StatusConfirmed || sh.Order.OrderRevision != 1 {
		t.Fatalf("shipment.1 %+v", sh)
	}
}

// SM-TRD-ORDER order.close with the owner's rule: shipment.1 shipped 40 of 120 on revision 1; the
// customer reduces L1 to the 40 shipped (revision 2) and the order closes fulfilled. The documents
// release of shipment.1, granted on revision 1 before the reduction, is evaluated again on the closed
// order's revision 2 — its 100 % before shipment covered by the 960.00 applied — and the telex
// release is recorded; the release itself stays bound to revision 1.
func TestDocumentsReleaseAfterTheOrderClosed(t *testing.T) {
	w := newFulWorld(t)
	order := w.order.ObjectID
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "40", d, rel)
	w.issuedBillOfLading(w.ful, "shipment.1")
	before := w.grantedOn(w.ful, "shipment.1", fulapi.ScopeDocuments)
	w.amendL1("amendment.40", "40")
	w.amendedReleasable(order)
	if _, err := w.endClose(order); err != nil {
		t.Fatalf("close after 40 of 40: %v", err)
	}
	r := w.grantedOn(w.ful, "shipment.1", fulapi.ScopeDocuments)
	if r.ReleaseID != before.ReleaseID || r.RowVersion != 2 {
		t.Fatalf("evaluated again %+v", r)
	}
	e := w.latestEvaluation(r.ReleaseID)
	w.wantReused("shipment.1", e, 2)
	if e.Bindings.Order.Status != trdapi.OrderClosed || e.Bindings.Payment.Status != stlapi.PaymentSatisfied || e.Bindings.Payment.OrderRevision != 2 {
		t.Fatalf("on the closed order %+v %+v", e.Bindings.Order, e.Bindings.Payment)
	}
	c, err := w.recordDocuments("shipment.1", 2, fulapi.ReleaseTelexRelease)
	if err != nil || c.Status != fulapi.ReleaseConsumed {
		t.Fatalf("the telex release after order.close %+v %v", c, err)
	}
	if v := w.releaseView(r.ReleaseID); v.Order.OrderRevision != 1 || guardOf(v.Consumption.Guards, fulapi.GuardOrder) != "closed/r2" {
		t.Fatalf("the consumed release %+v", v)
	}
}

// The owner's rule over HTTP with OIDC, on the composed process (bootstrap's FUL on PUR's real
// supplier deliveries): shipment.http of 40 is confirmed on revision 1; the order is reduced to 110
// and STL's account moved to revision 2. The documentation clerk's evaluation of the documents scope
// is granted on revision 2; the release reads back with the confirmation it reused and revision 2's
// payment condition and order; the original B/L is recorded over HTTP and reconciles; the
// consumption reads back stating revision 2.
func TestDocumentsReleaseOnTheCurrentRevisionOverHTTP(t *testing.T) {
	w := newFulPurWorld(t)
	dl := w.verified("delivery.http", "120", "120")
	_, err := w.ship(w.c.FUL, w.merch, "shipment.http", "40", dl)
	w.must(err)
	w.grantedOn(w.c.FUL, "shipment.http", fulapi.ScopeShipment)
	_, err = w.c.FUL.ConfirmShipment(w.merch, w.confirmCmd("shipment.http", 1))
	w.must(err)
	w.issuedBillOfLading(w.c.FUL, "shipment.http")
	w.amendL1("amendment.http", "110")
	w.amendedReleasable(w.order.ObjectID)
	v2 := w.getOrder(w.order.ObjectID)

	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	doc := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "doc.1", "human")}

	e := doc.command("ful", fulapi.Contract, fulapi.CommandEvaluateRelease, map[string]any{"shipment_id": "shipment.http", "scope": "documents"})
	id, _ := e["object_id"].(string)
	if e["status"] != fulapi.ReleaseGranted || e["row_version"] != float64(1) || id == "" {
		t.Fatalf("evaluate on revision 2 %v", e)
	}
	status, v := doc.do("GET", "/api/brant/v1/ful/commercial_releases/"+id, nil)
	evaluations, _ := v["evaluations"].([]any)
	if status != http.StatusOK || len(evaluations) != 1 || v["order"].(map[string]any)["order_revision"] != float64(1) {
		t.Fatalf("release %d %v", status, v)
	}
	b := evaluations[0].(map[string]any)["bindings"].(map[string]any)
	conf, _ := b["confirmation"].(map[string]any)
	payment, _ := b["payment_condition"].(map[string]any)
	ord, _ := b["order"].(map[string]any)
	if conf == nil || conf["order_revision"] != float64(1) || len(conf["guards_sha256"].(string)) != 64 || len(conf["reused"].([]any)) == 0 ||
		payment["order_revision"] != float64(2) || payment["scope"] != "documents" || payment["status"] != stlapi.PaymentSatisfied ||
		ord["order_revision"] != float64(2) || ord["order_hash"] != v2.OrderHash || b["trade_compliance"] == nil {
		t.Fatalf("bindings %v", b)
	}
	record := map[string]any{"contract": fulapi.Contract, "command": fulapi.CommandRecordDocumentRelease, "idempotency_key": "ful.http.c1b.record",
		"args": map[string]any{"shipment_id": "shipment.http", "expected_version": 1, "release_kind": fulapi.ReleaseOriginalBL,
			"evidence_ids": []any{"evidence"}}}
	status, r := doc.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandRecordDocumentRelease, record)
	if status != http.StatusOK || r["status"] != fulapi.ReleaseConsumed || r["row_version"] != float64(2) {
		t.Fatalf("record %d %v", status, r)
	}
	status, rec := doc.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandRecordDocumentRelease+"/reconcile", record)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != r["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	_, v = doc.do("GET", "/api/brant/v1/ful/commercial_releases/"+id, nil)
	consumption, _ := v["consumption"].(map[string]any)
	var trd, stl string
	for _, g := range consumption["guards"].([]any) {
		m := g.(map[string]any)
		switch m["name"] {
		case fulapi.GuardOrder:
			trd, _ = m["value"].(string)
		case fulapi.GuardPaymentCondition:
			stl, _ = m["value"].(string)
		}
	}
	if v["status"] != fulapi.ReleaseConsumed || trd != "active/r2" || !strings.Contains(stl, "satisfied//r2/") {
		t.Fatalf("the consumption %v", consumption)
	}
}

// C4b-FUL's shipment review under the owner's rule (S5 C1b; SM-FUL-SHIPMENT-REVIEW, WF-06 step 5,
// BT10), with TRD's own events relayed to ful.shipment_review: shipment.1 shipped 40 on revision 1
// with its documents release granted on revision 1's payment condition. The reduction to 110 opens
// a review whose only item is that release; no_action no longer settles it (the database refuses it
// too). The documentation clerk records the original B/L on revision 2 without evaluating again —
// document_release.record judges the current revision as an evaluation would — and the review is
// settled as reevaluated by that consumption. shipment.2 then ships 30 on revision 2 with its
// documents release granted on revision 2; closing the remainder short leaves nothing stale (the
// end still releases the documents of what shipped), so no review opens, and its telex release is
// recorded on the closed order.
func TestShipmentReviewOfAConfirmedShipmentsDocumentsRelease(t *testing.T) {
	w := newFulWorld(t)
	r := fulReviewOn(t, w, nil)
	order := w.order.ObjectID
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "40", d, rel)
	w.issuedBillOfLading(w.ful, "shipment.1")
	docs := w.grantedOn(w.ful, "shipment.1", fulapi.ScopeDocuments)
	r.relayTRD()
	r.amendedTo("amendment.110", "110")
	r.relayTRD()
	v := r.reviews()
	if len(v) != 1 || v[0].Status != fulapi.ReviewOpen || reviewItems(v[0]) != "release:shipment.1/documents" ||
		v[0].Items[0].ReleaseID != docs.ReleaseID || v[0].Items[0].Impact != fulapi.ImpactRevisionChanged {
		t.Fatalf("the review of revision 2 %+v", v)
	}
	rv := v[0]
	_, err := r.settle(w.merch, rv.ReviewID, 1, disposal(1, fulapi.DispositionNoAction, "", "x"))
	refusedAt(t, "no_action", err, kernel.CodeInvalidKnownValue, "/args/items/0/disposition")
	if err := r.asOwner(`UPDATE brant_ful.shipment_review_item SET disposition = 'no_action', disposition_reason = 'x'
		WHERE review_id = '` + rv.ReviewID + `' AND item_no = 1`); err == nil || !strings.Contains(err.Error(), "which did not happen") {
		t.Fatalf("the database on no_action: %v", err)
	}
	_, err = r.settle(w.merch, rv.ReviewID, 1, disposal(1, fulapi.DispositionReevaluated, "", "x"))
	refusedAt(t, "nothing done yet", err, kernel.CodeInvalidState, "/args/items/0/disposition")

	w.amendedReleasable(order)
	if c, err := w.recordDocuments("shipment.1", docs.RowVersion, fulapi.ReleaseOriginalBL); err != nil || c.Status != fulapi.ReleaseConsumed {
		t.Fatalf("the original B/L of shipment.1 on revision 2 %+v %v", c, err)
	}
	if g := guardOf(w.releaseView(docs.ReleaseID).Consumption.Guards, fulapi.GuardOrder); g != "active/r2" {
		t.Fatalf("judged on revision 2: %s", g)
	}
	s, err := r.settle(w.merch, rv.ReviewID, 1, disposal(1, fulapi.DispositionReevaluated, "", "已按修订 2 放单"))
	w.must(err)
	got, err := w.ful.GetShipmentReview(w.merch, rv.ReviewID)
	w.must(err)
	x := got.Items[0].Disposition
	if s.Status != fulapi.ReviewSettled || x == nil || x.Disposition != fulapi.DispositionReevaluated || x.Ref != docs.ReleaseID ||
		x.Status != fulapi.ReleaseConsumed || x.At.IsZero() {
		t.Fatalf("settled by the consumption %+v %+v", s, x)
	}

	w.confirmed("shipment.2", "30", d, rel)
	w.issuedBillOfLading(w.ful, "shipment.2")
	second := w.grantedOn(w.ful, "shipment.2", fulapi.ScopeDocuments)
	_, err = r.endShort(order)
	w.must(err)
	if closed := eventOf(r.relayTRD(), trdapi.EventOrderRemainderClosed); closed.ID == "" {
		t.Fatal("no OrderRemainderClosed")
	}
	if v := r.reviews(); len(v) != 1 || v[0].ReviewID != rv.ReviewID {
		t.Fatalf("closing short with the documents release judged on its revision opens nothing: %+v", v)
	}
	if c, err := w.recordDocuments("shipment.2", second.RowVersion, fulapi.ReleaseTelexRelease); err != nil || c.Status != fulapi.ReleaseConsumed {
		t.Fatalf("the telex release of shipment.2 after the order closed short %+v %v", c, err)
	}
}
