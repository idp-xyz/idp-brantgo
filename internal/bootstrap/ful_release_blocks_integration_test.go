//go:build integration

package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// A block that appears after the confirmation still stops the documents release (owner decision
// 2026-11-12 ~23:05, S5 unit C1c, a follow-up of C1b and WBS S5-07; FUL-INV-002, CNX-INV-001,
// SM-CNX-COMPLIANCE, PTY-INV-002, WF-07, R44, R56) on PostgreSQL 16 with PTY's, CNX's, STL's and
// TRD's real guard functions: the documents release of a confirmed shipment
// (commercial_release.evaluate scope documents, grant_exception, document_release.record) asks
// PTY's PartyScreeningGuard again on every party (anything but clear is PARTY_SCREENING_NOT_CLEAR)
// and CNX's TradeComplianceGuard again on the revision the goods left under and on the order's
// current revision (TRADE_COMPLIANCE_NOT_CLEAR only for a blocked conclusion; an expired conclusion,
// a newer programme list version or a check reopened but not concluded leave the confirmation's
// clear conclusion standing). Payment stays on the current revision and the other goods facts as
// confirmed (C1b). Python V0.50 has no commercial release: the tests cite the design.

// screeningList imports version of the order world's synthetic screening list cn.uel, published on
// published, holding entries (the author holds every permission).
func (w *fulWorld) screeningList(version, published string, entries ...refapi.ScreeningEntry) {
	w.t.Helper()
	sum := sha256.Sum256([]byte("synthetic order screening list " + version))
	_, err := w.c.REF.ImportScreeningList(w.author, refapi.ImportScreeningList{IdempotencyKey: w.key(), ListCode: "cn.uel",
		Jurisdiction: refapi.JurisdictionCN, Version: version, SourceName: "Synthetic list cn.uel", SourceURL: "https://lists.example.org/cn.uel",
		PublishedOn: published, FileSHA256: hex.EncodeToString(sum[:]), EntryCount: len(entries), Entries: entries})
	w.must(err)
}

// rescreened has customer.1 screened again against the current list versions under the world's
// screening_regulation and returns its screening.
func (w *fulWorld) rescreened() ptyapi.ScreeningView {
	w.t.Helper()
	s, err := w.c.PTY.GetScreening(w.author, "customer.1")
	w.must(err)
	reg := w.compliance().regulation
	_, err = w.c.PTY.RecordScreening(w.author, ptyapi.RecordScreening{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: s.RowVersion,
		RegulationID: reg.ConfigID, RegulationRevision: reg.Revision})
	w.must(err)
	s, err = w.c.PTY.GetScreening(w.author, "customer.1")
	w.must(err)
	return s
}

// wantBlocked requires release receipt r, and the latest evaluation of its release, blocked by guard
// with code, the reason containing why.
func (w *fulWorld) wantBlocked(what string, r fulapi.ReleaseReceipt, err error, guard, code, why string) fulapi.EvaluationView {
	w.t.Helper()
	if err != nil || r.Status != fulapi.ReleaseBlocked || r.Guard != guard || r.Code != code || !strings.Contains(r.Reason, why) {
		w.t.Fatalf("%s: %+v %v", what, r, err)
	}
	e := w.latestEvaluation(r.ReleaseID)
	if e.Outcome != fulapi.ReleaseBlocked || e.Code != code || e.Bindings.Rechecked == nil || e.Bindings.Confirmation == nil {
		w.t.Fatalf("%s: evaluation %+v", what, e)
	}
	return e
}

// agedCompliance asks CNX's real TradeComplianceGuard v1 at an instant by later than FUL's: a FUL
// module on it judges every trade compliance conclusion as it will stand then (expired once its
// validity ended), every other guard as now.
type agedCompliance struct {
	ful.Compliance
	by time.Duration
}

func (a agedCompliance) GuardTradeCompliance(ctx context.Context, exec bentopg.Executor, b cnxapi.OrderBinding, at time.Time) (cnxapi.TradeComplianceVerdict,
	error) {
	return a.Compliance.GuardTradeCompliance(ctx, exec, b, at.Add(a.by))
}

// agedFUL is the world's FUL on its own pool with CNX's guard asked by later (agedCompliance).
func (w *fulWorld) agedFUL(by time.Duration) *ful.Module {
	w.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	w.must(err)
	m, err := ful.New(db, ful.Upstream{Orders: ful.TRDOrders(w.c.TRD), Releases: ful.QLTReleases(w.qlt), Deliveries: w.deliveries,
		Screening: ful.PTYScreening(), Sources: w.c.EVD, Compliance: agedCompliance{Compliance: ful.CNXCompliance(), by: by},
		Payments: ful.STLPayments(), Documents: ful.DocumentPorts(w.c.POL, w.c.PTY, w.c.TRD).WithCertificates(w.c.CNX), Costs: w.c.CST})
	w.must(err)
	return m
}

// documentsException is the credit manager's exception for P2 of the documents release of id at
// expected, shortfall minor units short.
func (w *fulWorld) documentsException(id string, expected int64, shortfall string) fulapi.GrantReleaseException {
	return fulapi.GrantReleaseException{IdempotencyKey: w.key(), ShipmentID: id, Scope: fulapi.ScopeDocuments, ExpectedVersion: expected, StageID: "P2",
		Currency: "USD", ShortfallMinor: shortfall, ValidUntil: kernel.FormatTimestamp(time.Now().Add(48 * time.Hour)),
		Reason: "合成：客户书面承诺尾款随到港付清", EvidenceIDs: []string{"evidence"}}
}

// PTY after the confirmation (owner decision ~23:05; FUL-INV-002, PTY-INV-002, R44): on 30 % deposit
// and 70 % against documents (D-5), the deposit paid, shipment.1 (40) and shipment.2 (30) are
// confirmed with customer.1 screened clear; shipment.2's documents release is granted by an
// exception for P2. Then
//
//   - a new version of the screening list cn.uel lists customer.1: PTY answers pending/list_updated,
//     and shipment.1's documents release, so far waiting for P2 only, is PARTY_SCREENING_NOT_CLEAR —
//     the first unmet guard in code order;
//   - screened again, a suspected match; compliance decides it a true match (blocked): still
//     PARTY_SCREENING_NOT_CLEAR, the credit manager's exception for P2 is refused with it, and
//     shipment.2's granted release cannot be recorded (it stays granted, not consumed). The bindings
//     keep the confirmation's clear screening and bind PTY's answer beside it;
//   - the list's next version no longer lists customer.1, screened again clear: shipment.1 waits for
//     P2 alone again, the exception grants it and its original B/L is recorded, the consumption
//     naming the new screening; shipment.2's telex release is recorded.
func TestDocumentsReleaseStopsForAPartyScreenedBlockedAfterTheConfirmation(t *testing.T) {
	w := newDocumentsWorld(t)
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "40", d, rel)
	w.confirmed("shipment.2", "30", d, rel)
	for _, id := range []string{"shipment.1", "shipment.2"} {
		w.issuedBillOfLading(w.ful, id)
	}
	conf := w.shipmentRelease("shipment.1")
	confirmed := conf.Evaluations[len(conf.Evaluations)-1].Bindings.Screenings[0]
	r, err := w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeDocuments)
	e := w.wantBlocked("P2 unpaid", r, err, fulapi.GuardPaymentCondition, fulapi.CodeCommercialReleaseBlocked, "67200")
	docs1 := r.ReleaseID
	if s := e.Bindings.Rechecked.Screenings; len(s) != 1 || s[0].PartyID != "customer.1" || s[0].Status != ptyapi.ScreeningClear ||
		s[0].ScreeningID != confirmed.ScreeningID || guardOf(e.Guards, "screening:customer.1") != "clear/screened/"+confirmed.ScreeningID {
		t.Fatalf("screened clear as confirmed %+v %+v", s, e.Guards)
	}
	if c := e.Bindings.Confirmation; strings.Join(c.Reused, " ") != fulapi.GuardTradeCompliance+" "+fulapi.GuardSupplierDelivery+" "+
		fulapi.GuardQualityRelease+" "+fulapi.GuardFulfilment || strings.Join(c.Rechecked, " ") != fulapi.GuardTradeCompliance+" "+fulapi.GuardPartyScreening {
		t.Fatalf("reused %v rechecked %v", c.Reused, c.Rechecked)
	}
	r, err = w.evaluateRelease(w.ful, "shipment.2", fulapi.ScopeDocuments)
	w.wantBlocked("shipment.2 P2 unpaid", r, err, fulapi.GuardPaymentCondition, fulapi.CodeCommercialReleaseBlocked, "67200")
	second, err := w.ful.GrantReleaseException(w.manager, w.documentsException("shipment.2", r.RowVersion, "67200"))
	if err != nil || second.Status != fulapi.ReleaseGranted {
		t.Fatalf("shipment.2's exception %+v %v", second, err)
	}

	w.screeningList("v2", "2026-11-05", listEntry("E1", "Globex Corporation"), listEntry("E9", "customer.1"))
	r, err = w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeDocuments)
	w.wantBlocked("a newer list version", r, err, fulapi.GuardPartyScreening, fulapi.CodePartyScreeningNotClear, ptyapi.ReasonListUpdated)
	if s := w.rescreened(); s.Status != ptyapi.ScreeningMatch {
		t.Fatalf("screened again %+v", s)
	}
	r, err = w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeDocuments)
	w.wantBlocked("a suspected match", r, err, fulapi.GuardPartyScreening, fulapi.CodePartyScreeningNotClear, ptyapi.ReasonSuspectedMatch)
	s, err := w.c.PTY.GetScreening(w.author, "customer.1")
	w.must(err)
	_, err = w.c.PTY.DecideScreening(w.author, ptyapi.DecideScreening{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: s.RowVersion,
		ScreeningID: s.CurrentScreeningID, Decision: ptyapi.DecisionTrueMatch, Reason: "合成：与名单条目 E9 为同一主体"})
	w.must(err)
	r, err = w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeDocuments)
	e = w.wantBlocked("a true match", r, err, fulapi.GuardPartyScreening, fulapi.CodePartyScreeningNotClear, ptyapi.ReasonTrueMatch)
	if got := e.Bindings.Rechecked.Screenings[0]; got.Status != ptyapi.ScreeningBlocked || got.Reason != ptyapi.ReasonTrueMatch ||
		got.ScreeningID != s.CurrentScreeningID || e.Bindings.Screenings[0] != confirmed ||
		guardOf(e.Guards, "screening:customer.1") != "blocked/true_match/"+s.CurrentScreeningID {
		t.Fatalf("the true match beside the confirmation's screening %+v %+v %+v", got, e.Bindings.Screenings, e.Guards)
	}
	_, err = w.ful.GrantReleaseException(w.manager, w.documentsException("shipment.1", r.RowVersion, "67200"))
	refusedAt(t, "the exception on a blocked party", err, fulapi.CodePartyScreeningNotClear, "/shipment/parties/0")
	_, err = w.recordDocuments("shipment.2", second.RowVersion, fulapi.ReleaseOriginalBL)
	refusedAt(t, "shipment.2's original B/L to a blocked party", err, fulapi.CodePartyScreeningNotClear, "/shipment/parties/0")
	if v := w.releaseView(second.ReleaseID); v.Status != fulapi.ReleaseGranted || v.RowVersion != second.RowVersion || v.Consumption != nil {
		t.Fatalf("shipment.2's release after the refusal %+v", v)
	}
	if v := w.releaseView(docs1); v.Status != fulapi.ReleaseBlocked || len(v.Exceptions) != 0 {
		t.Fatalf("never excepted %+v", v)
	}

	w.screeningList("v3", "2026-11-06", listEntry("E1", "Globex Corporation"))
	if s := w.rescreened(); s.Status != ptyapi.ScreeningClear {
		t.Fatalf("delisted and screened again %+v", s)
	}
	clear, err := w.c.PTY.GetScreening(w.author, "customer.1")
	w.must(err)
	r, err = w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeDocuments)
	w.wantBlocked("clear again: P2 alone", r, err, fulapi.GuardPaymentCondition, fulapi.CodeCommercialReleaseBlocked, "67200")
	g, err := w.ful.GrantReleaseException(w.manager, w.documentsException("shipment.1", r.RowVersion, "67200"))
	if err != nil || g.Status != fulapi.ReleaseGranted {
		t.Fatalf("the exception once screened clear %+v %v", g, err)
	}
	c, err := w.recordDocuments("shipment.1", g.RowVersion, fulapi.ReleaseOriginalBL)
	if err != nil || c.Status != fulapi.ReleaseConsumed {
		t.Fatalf("the original B/L of shipment.1 %+v %v", c, err)
	}
	cs := w.releaseView(docs1).Consumption.Guards
	if guardOf(cs, "screening:customer.1") != "clear/screened/"+clear.CurrentScreeningID || clear.CurrentScreeningID == confirmed.ScreeningID ||
		!strings.HasSuffix(guardOf(cs, fulapi.GuardTradeCompliance), "/r1") || !strings.HasSuffix(guardOf(cs, fulapi.GuardPaymentCondition), "/exception=1") {
		t.Fatalf("the consumption %+v", cs)
	}
	if c, err := w.recordDocuments("shipment.2", second.RowVersion, fulapi.ReleaseTelexRelease); err != nil || c.Status != fulapi.ReleaseConsumed {
		t.Fatalf("shipment.2's telex release once screened clear %+v %v", c, err)
	}
	if ev := w.fulEventsOf(fulapi.EventDocumentsReleased); len(ev) != 2 {
		t.Fatalf("DocumentsReleased %+v", ev)
	}
}

// CNX on the revision the goods left under (owner decision ~23:05; CNX-INV-001, SM-CNX-COMPLIANCE,
// MD-47, R56): shipment.1 (40) and shipment.2 (30) are confirmed on the order's only revision under a
// clear conclusion, both documents releases granted. What is no block leaves the documents release
// granted, CNX's answer bound beside the confirmation's: the conclusion seen after its validity (a
// FUL whose CNX instant is 31 days later: expired), a new version of the CN programme list the
// conclusion used (pending/list_updated), the conclusion reopened on it (pending/not_evaluated) and
// evaluated again for the embargoed destination AQ (pending/awaiting_decision). Concluded blocked,
// it is TRADE_COMPLIANCE_NOT_CLEAR: evaluate, the exception and shipment.2's document_release.record
// are refused with it, shipment.2's release stays granted. Compliance evaluates the facts again
// (destination DE) and concludes clear: both are released.
func TestDocumentsReleaseStopsForATradeComplianceBlockAfterTheConfirmation(t *testing.T) {
	w := newFulWorld(t)
	order := w.order.ObjectID
	c := w.compliance()
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "40", d, rel)
	w.confirmed("shipment.2", "30", d, rel)
	for _, id := range []string{"shipment.1", "shipment.2"} {
		w.issuedBillOfLading(w.ful, id)
	}
	first := w.grantedOn(w.ful, "shipment.1", fulapi.ScopeDocuments)
	second := w.grantedOn(w.ful, "shipment.2", fulapi.ScopeDocuments)
	checks, err := c.cnx.OrderChecks(c.compliance, order)
	w.must(err)
	if len(checks) != 1 || checks[0].OrderRevision != 1 {
		t.Fatalf("checks %+v", checks)
	}
	check := checks[0]
	granted := func(what string, m *ful.Module, status, reason string) fulapi.EvaluationView {
		t.Helper()
		r := w.grantedOn(m, "shipment.1", fulapi.ScopeDocuments)
		e := w.latestEvaluation(r.ReleaseID)
		tc := e.Bindings.Rechecked.TradeCompliance
		if len(tc) != 1 || tc[0].OrderRevision != 1 || tc[0].Status != status || tc[0].Reason != reason || tc[0].Blocked || tc[0].CheckID != check.CheckID ||
			e.Bindings.TradeCompliance.Status != cnxapi.StatusClear ||
			!strings.HasPrefix(guardOf(e.Guards, fulapi.GuardTradeCompliance), status+"/"+reason+"/"+check.CheckID+"/") ||
			guardOf(e.Guards, fulapi.GuardTradeComplianceCurrent) != "" {
			t.Fatalf("%s: %+v %+v", what, tc, e.Guards)
		}
		return e
	}
	granted("cleared", w.ful, cnxapi.StatusClear, cnxapi.ReasonCleared)
	granted("expired", w.agedFUL(31*24*time.Hour), cnxapi.StatusExpired, cnxapi.ReasonExpired)

	list := syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-10")
	list.IdempotencyKey = w.key()
	_, err = w.c.REF.ImportCountryProgrammes(c.compliance, list)
	w.must(err)
	granted("a newer programme list version", w.ful, cnxapi.StatusPending, cnxapi.ReasonListUpdated)
	reopened, err := c.cnx.Reopen(c.compliance, cnxapi.Reopen{IdempotencyKey: w.key(), Cause: cnxapi.CauseProgrammeUpdated, CheckID: check.CheckID,
		ExpectedVersion: check.RowVersion, Reason: "合成国别项目名单新版本"})
	w.must(err)
	granted("reopened", w.ful, cnxapi.StatusPending, cnxapi.ReasonNotEvaluated)
	evaluated, err := c.cnx.Evaluate(c.compliance, c.evaluateCmd(order, 1, reopened.RowVersion, syntheticEmbargoed))
	w.must(err)
	granted("evaluated again, not concluded", w.ful, cnxapi.StatusPending, cnxapi.ReasonAwaitingDecision)

	blocked, err := c.cnx.Decide(c.compliance, c.decideCmd(evaluated, cnxapi.ConclusionBlocked, false))
	w.must(err)
	r, err := w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeDocuments)
	e := w.wantBlocked("concluded blocked", r, err, fulapi.GuardTradeCompliance, fulapi.CodeTradeComplianceNotClear, "order revision 1 the goods left under")
	if tc := e.Bindings.Rechecked.TradeCompliance; len(tc) != 1 || !tc[0].Blocked || tc[0].DecisionID != blocked.DecisionID ||
		e.Bindings.TradeCompliance.DecisionID == blocked.DecisionID || e.Bindings.Rechecked.Screenings[0].Status != ptyapi.ScreeningClear ||
		guardOf(e.Guards, fulapi.GuardTradeCompliance) != "blocked/blocked/"+check.CheckID+"/"+blocked.DecisionID+"/false/r1" {
		t.Fatalf("the block beside the confirmation's clear conclusion %+v %+v", e.Bindings.Rechecked, e.Guards)
	}
	_, err = w.ful.GrantReleaseException(w.manager, w.documentsException("shipment.1", r.RowVersion, "100"))
	refusedAt(t, "the exception on a blocked conclusion", err, fulapi.CodeTradeComplianceNotClear, "/args/shipment_id")
	_, err = w.recordDocuments("shipment.2", second.RowVersion, fulapi.ReleaseOriginalBL)
	refusedAt(t, "shipment.2's original B/L", err, fulapi.CodeTradeComplianceNotClear, "/args/shipment_id")
	if v := w.releaseView(second.ReleaseID); v.Status != fulapi.ReleaseGranted || v.RowVersion != second.RowVersion || v.Consumption != nil {
		t.Fatalf("shipment.2's release after the refusal %+v", v)
	}

	again, err := c.cnx.Evaluate(c.compliance, c.evaluateCmd(order, 1, blocked.RowVersion, syntheticClear))
	w.must(err)
	_, err = c.cnx.Decide(c.compliance, c.decideCmd(again, cnxapi.ConclusionClear, false))
	w.must(err)
	r = w.grantedOn(w.ful, "shipment.1", fulapi.ScopeDocuments)
	if r.ReleaseID != first.ReleaseID {
		t.Fatalf("the same release %+v", r)
	}
	if c, err := w.recordDocuments("shipment.1", r.RowVersion, fulapi.ReleaseOriginalBL); err != nil || c.Status != fulapi.ReleaseConsumed {
		t.Fatalf("shipment.1's original B/L once cleared %+v %v", c, err)
	}
	if c, err := w.recordDocuments("shipment.2", second.RowVersion, fulapi.ReleaseTelexRelease); err != nil || c.Status != fulapi.ReleaseConsumed {
		t.Fatalf("shipment.2's telex release once cleared %+v %v", c, err)
	}
	if g := guardOf(w.releaseView(second.ReleaseID).Consumption.Guards, fulapi.GuardTradeCompliance); !strings.HasPrefix(g, "clear/cleared/"+check.CheckID+"/") {
		t.Fatalf("the consumption states the clear conclusion: %s", g)
	}
}

// CNX on the order's current revision (owner decision ~23:05; CNX-INV-001, PL-14, BT10): shipment.1
// (40) is confirmed on revision 1; the order is reduced to 110 (revision 2) and compliance clears
// revision 2: the documents release asks CNX on both revisions and is granted. Compliance then
// reopens revision 2's conclusion on a new CN programme list version and concludes it blocked
// (destination AQ): revision 1's conclusion only answers pending/list_updated, but the block on the
// current revision is TRADE_COMPLIANCE_NOT_CLEAR, and the original B/L is not recorded.
func TestDocumentsReleaseStopsForABlockOnTheCurrentRevision(t *testing.T) {
	w := newFulWorld(t)
	order := w.order.ObjectID
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "40", d, rel)
	w.issuedBillOfLading(w.ful, "shipment.1")
	w.amendL1("amendment.110", "110")
	w.amendedReleasable(order)
	v2 := w.getOrder(order)
	r := w.grantedOn(w.ful, "shipment.1", fulapi.ScopeDocuments)
	e := w.latestEvaluation(r.ReleaseID)
	w.wantReused("shipment.1", e, 2)
	if tc := e.Bindings.Rechecked.TradeCompliance; len(tc) != 2 || tc[0].OrderRevision != 1 || tc[0].Status != cnxapi.StatusClear ||
		tc[1].OrderRevision != 2 || tc[1].OrderHash != v2.OrderHash || tc[1].Status != cnxapi.StatusClear || tc[0].CheckID == tc[1].CheckID ||
		!strings.HasSuffix(guardOf(e.Guards, fulapi.GuardTradeComplianceCurrent), "/r2") {
		t.Fatalf("both revisions asked %+v %+v", tc, e.Guards)
	}

	w.embargoed(order)
	r, err := w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeDocuments)
	e = w.wantBlocked("revision 2 concluded blocked", r, err, fulapi.GuardTradeCompliance, fulapi.CodeTradeComplianceNotClear,
		"order's current revision 2")
	if tc := e.Bindings.Rechecked.TradeCompliance; len(tc) != 2 || tc[0].Blocked || tc[0].Reason != cnxapi.ReasonListUpdated || !tc[1].Blocked ||
		!strings.HasPrefix(guardOf(e.Guards, fulapi.GuardTradeComplianceCurrent), "blocked/blocked/") {
		t.Fatalf("the current revision's block %+v %+v", tc, e.Guards)
	}
	_, err = w.recordDocuments("shipment.1", r.RowVersion, fulapi.ReleaseOriginalBL)
	refusedAt(t, "the original B/L under the current revision's block", err, fulapi.CodeTradeComplianceNotClear, "/args/shipment_id")
	if v := w.releaseView(r.ReleaseID); v.Status != fulapi.ReleaseBlocked || v.Consumption != nil {
		t.Fatalf("not released %+v", v)
	}
}

// The owner's rule over HTTP with OIDC on the composed process (bootstrap's FUL on PUR's real
// supplier deliveries): shipment.http of 40 is confirmed; compliance puts customer.1 on the negative
// list. The documentation clerk's evaluation of the documents scope commits blocked; the release
// reads back with PARTY_SCREENING_NOT_CLEAR, the confirmation's clear screening and PTY's answer
// beside it; document_release.record answers 409 PARTY_SCREENING_NOT_CLEAR.
func TestDocumentsReleaseRefusedForABlockedPartyOverHTTP(t *testing.T) {
	w := newFulPurWorld(t)
	dl := w.verified("delivery.http", "120", "120")
	_, err := w.ship(w.c.FUL, w.merch, "shipment.http", "40", dl)
	w.must(err)
	w.grantedOn(w.c.FUL, "shipment.http", fulapi.ScopeShipment)
	_, err = w.c.FUL.ConfirmShipment(w.merch, w.confirmCmd("shipment.http", 1))
	w.must(err)
	w.issuedBillOfLading(w.c.FUL, "shipment.http")
	s, err := w.c.PTY.GetScreening(w.author, "customer.1")
	w.must(err)
	_, err = w.c.PTY.BlockParty(w.author, ptyapi.BlockParty{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: s.RowVersion,
		Reason: "合成：列入负面清单"})
	w.must(err)

	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	doc := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "doc.1", "human")}

	ev := doc.command("ful", fulapi.Contract, fulapi.CommandEvaluateRelease, map[string]any{"shipment_id": "shipment.http", "scope": "documents"})
	id, _ := ev["object_id"].(string)
	if ev["status"] != fulapi.ReleaseBlocked || ev["row_version"] != float64(1) || id == "" {
		t.Fatalf("evaluate %v", ev)
	}
	status, v := doc.do("GET", "/api/brant/v1/ful/commercial_releases/"+id, nil)
	evaluations, _ := v["evaluations"].([]any)
	if status != http.StatusOK || len(evaluations) != 1 {
		t.Fatalf("release %d %v", status, v)
	}
	first := evaluations[0].(map[string]any)
	b := first["bindings"].(map[string]any)
	confirmed, _ := b["screenings"].([]any)
	rechecked, _ := b["rechecked"].(map[string]any)
	now, _ := rechecked["screenings"].([]any)
	tc, _ := rechecked["trade_compliance"].([]any)
	if first["code"] != fulapi.CodePartyScreeningNotClear || first["guard"] != fulapi.GuardPartyScreening || len(confirmed) != 1 ||
		confirmed[0].(map[string]any)["status"] != ptyapi.ScreeningClear || len(now) != 1 ||
		now[0].(map[string]any)["status"] != ptyapi.ScreeningBlocked || now[0].(map[string]any)["reason"] != ptyapi.ReasonNegativeList ||
		len(tc) != 1 || tc[0].(map[string]any)["blocked"] != false {
		t.Fatalf("evaluation %v", first)
	}
	status, out := doc.try("ful", fulapi.Contract, fulapi.CommandRecordDocumentRelease, map[string]any{"shipment_id": "shipment.http",
		"expected_version": 1, "release_kind": fulapi.ReleaseOriginalBL, "evidence_ids": []any{"evidence"}})
	if status != http.StatusConflict || out["code"] != fulapi.CodePartyScreeningNotClear {
		t.Fatalf("record %d %v", status, out)
	}
}

// The lock order of the documents path (S5 C1c; ADR-BR-011, R44, R56): document_release.record of a
// confirmed shipment holds CNX's check and PTY's screening FOR SHARE from its guards until it commits,
// so a block either committed before FUL read it — refused — or waits for FUL's commit. Four
// shipments of 30 are confirmed with their documents releases granted. Compliance has reopened the
// conclusion on a new programme list version and evaluated the destination AQ (no block yet); FUL
// records shipment.1's original B/L and is held after STL's guard: trade_compliance.decide (blocked)
// waits more than 500 ms and commits once FUL did; shipment.2's B/L is then refused
// TRADE_COMPLIANCE_NOT_CLEAR. Once compliance concluded clear again, FUL records shipment.3's B/L and
// is held: party_screening.block of customer.1 waits likewise; shipment.4's B/L is then refused
// PARTY_SCREENING_NOT_CLEAR.
func TestABlockAndTheDocumentReleaseInBothCommitOrders(t *testing.T) {
	w := newFulWorld(t)
	order := w.order.ObjectID
	c := w.compliance()
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	ids := []string{"shipment.1", "shipment.2", "shipment.3", "shipment.4"}
	for _, id := range ids {
		w.confirmed(id, "30", d, rel)
		w.issuedBillOfLading(w.ful, id)
	}
	granted := map[string]fulapi.ReleaseReceipt{}
	for _, id := range ids {
		granted[id] = w.grantedOn(w.ful, id, fulapi.ScopeDocuments)
	}
	record := func(id string) error {
		_, err := w.recordDocuments(id, granted[id].RowVersion, fulapi.ReleaseOriginalBL)
		return err
	}
	heldWhile := func(id, what string, block func() error) {
		t.Helper()
		w.hold.set("payment")
		done := make(chan error, 1)
		go func() { done <- record(id) }()
		w.hold.wait(t, done)
		blocked := make(chan error, 1)
		go func() { blocked <- block() }()
		select {
		case err := <-blocked:
			w.hold.open()
			t.Fatalf("%s did not wait: %v (document release: %v)", what, err, <-done)
		case <-time.After(500 * time.Millisecond):
		}
		w.hold.open()
		if err := <-done; err != nil {
			t.Fatalf("the document release of %s holding the guards: %v", id, err)
		}
		if err := <-blocked; err != nil {
			t.Fatalf("%s after FUL committed: %v", what, err)
		}
		if v := w.releaseView(granted[id].ReleaseID); v.Status != fulapi.ReleaseConsumed {
			t.Fatalf("FUL first: %s's documents stay released %+v", id, v)
		}
	}

	checks, err := c.cnx.OrderChecks(c.compliance, order)
	w.must(err)
	list := syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-10")
	list.IdempotencyKey = w.key()
	_, err = w.c.REF.ImportCountryProgrammes(c.compliance, list)
	w.must(err)
	reopened, err := c.cnx.Reopen(c.compliance, cnxapi.Reopen{IdempotencyKey: w.key(), Cause: cnxapi.CauseProgrammeUpdated, CheckID: checks[0].CheckID,
		ExpectedVersion: checks[0].RowVersion, Reason: "合成国别项目名单新版本"})
	w.must(err)
	evaluated, err := c.cnx.Evaluate(c.compliance, c.evaluateCmd(order, 1, reopened.RowVersion, syntheticEmbargoed))
	w.must(err)
	var decided cnxapi.CheckReceipt
	heldWhile("shipment.1", "trade_compliance.decide", func() error {
		var err error
		decided, err = c.cnx.Decide(c.compliance, c.decideCmd(evaluated, cnxapi.ConclusionBlocked, false))
		return err
	})
	refusedAt(t, "shipment.2 after the block committed", record("shipment.2"), fulapi.CodeTradeComplianceNotClear, "/args/shipment_id")

	again, err := c.cnx.Evaluate(c.compliance, c.evaluateCmd(order, 1, decided.RowVersion, syntheticClear))
	w.must(err)
	_, err = c.cnx.Decide(c.compliance, c.decideCmd(again, cnxapi.ConclusionClear, false))
	w.must(err)
	s, err := w.c.PTY.GetScreening(w.author, "customer.1")
	w.must(err)
	heldWhile("shipment.3", "party_screening.block", func() error {
		_, err := w.c.PTY.BlockParty(w.author, ptyapi.BlockParty{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: s.RowVersion,
			Reason: "合成：列入负面清单"})
		return err
	})
	refusedAt(t, "shipment.4 after the block committed", record("shipment.4"), fulapi.CodePartyScreeningNotClear, "/shipment/parties/0")
	if ev := w.fulEventsOf(fulapi.EventDocumentsReleased); len(ev) != 2 {
		t.Fatalf("DocumentsReleased %+v", ev)
	}
}
