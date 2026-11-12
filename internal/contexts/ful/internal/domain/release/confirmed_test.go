package release

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The documents release of a confirmed shipment (owner decision 2026-11-12, S5 C1b; FUL-INV-002,
// SM-FUL-RELEASE, WF-07, BT10, D-5, R27, R45): money on the order's current revision, goods as the
// confirmation judged them, and an order ended by order.close or order.close_remainder still
// released. Python V0.50 has no commercial release: the vectors are BT10's (shipment.1 confirmed
// at revision 1, the order reduced at revision 2).

var current = shipment.OrderBinding{OrderID: "order.1", OrderRevision: 3, OrderHash: strings.Repeat("c", 64)}

// confirmedRelease is shipment.1's shipment release at revision 2 (order): evaluated granted with
// the goods' bindings, then consumed by shipment.confirm.
func confirmedRelease(t *testing.T) *Release {
	t.Helper()
	r := New("rls.s1", "shipment.1", ScopeShipment, order, doc)
	r.Evaluate(Verdict{Entries: []Entry{{Name: shipment.GuardQualityRelease, Value: "granted"}}, Bindings: Bindings{
		TradeCompliance: &ComplianceBinding{Status: "clear", CheckID: "chk.1", DecisionID: "dec.1"},
		Screenings:      []ScreeningBinding{{PartyID: "customer.1", Status: "clear", ScreeningID: "scr.1", PartyRevision: 3}},
		Deliveries:      []DeliveryBinding{{DeliveryID: "delivery.1", LineID: "L1", Version: 1, Status: "verified", Verified: "120"}},
		QualityReleases: []QualityBinding{{ReleaseID: "rel.1", Status: "granted", Released: "120"}},
		Payment:         &PaymentBinding{Scope: ScopeShipment, Status: "satisfied", OrderRevision: 2},
		Order:           &OrderBindingSeen{OrderID: "order.1", OrderRevision: 2, Status: OrderActive}}}, doc)
	confirm := Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "merch.1"}, Epoch: 3, At: t0.Add(time.Hour)}
	if err := r.Consume("", []string{"evidence"}, []Entry{{Name: shipment.GuardQualityRelease, Value: "granted"}}, confirm); err != nil {
		t.Fatal(err)
	}
	return r
}

// Goods as confirmed: the documents release reuses the bindings of the goods' guards of the
// evaluation the confirmation consumed, never the payment condition or the order, names the
// confirmation (release, version, evaluation, the SHA-256 of the confirming transaction's evidence,
// the revision the goods left under) and pins it in one evidence entry. Nothing else is a
// confirmation: no release, a release not consumed, a documents release.
func TestTheConfirmationIsReused(t *testing.T) {
	r := confirmedRelease(t)
	b, e, ok := r.Confirmation()
	c := b.Confirmation
	if !ok || b.Payment != nil || b.Order != nil || b.TradeCompliance.CheckID != "chk.1" || b.Screenings[0].ScreeningID != "scr.1" ||
		b.Deliveries[0].Verified != "120" || b.QualityReleases[0].ReleaseID != "rel.1" || b.ExportLicense != nil || c == nil {
		t.Fatalf("reused %+v", b)
	}
	if c.ReleaseID != "rls.s1" || c.ReleaseVersion != 2 || c.EvaluationSeq != 1 || c.GuardsSHA256 != r.Consumption.EntriesSHA256 ||
		!c.ConfirmedAt.Equal(t0.Add(time.Hour)) || c.OrderRevision != 2 || !slices.Equal(c.Reused, []string{shipment.GuardTradeCompliance,
		shipment.GuardPartyScreening, shipment.GuardSupplierDelivery, shipment.GuardQualityRelease, GuardFulfilment}) {
		t.Fatalf("confirmation %+v", c)
	}
	if e.Name != GuardConfirmation || e.Value != "rls.s1/v2/consumed/"+r.Consumption.EntriesSHA256+"/r2" {
		t.Fatalf("entry %+v", e)
	}
	doc, _ := json.Marshal(b)
	if !strings.Contains(string(doc), `"confirmation":{"release_id":"rls.s1","release_version":2,"evaluation_seq":1,`) {
		t.Fatalf("bindings JSON %s", doc)
	}

	var none *Release
	granted := evaluated(t, nil)
	documents := confirmedRelease(t)
	documents.Scope = ScopeDocuments
	for name, x := range map[string]*Release{"none": none, "granted": granted, "documents": documents} {
		if _, _, ok := x.Confirmation(); ok {
			t.Errorf("%s is no confirmation", name)
		}
	}
}

// Money on the current revision: the documents release judges the revision and hash TRD's order
// state shows when it is later than the shipment's; the shipment's own binding otherwise (TRD does not
// know the order, or shows no later revision, or the bound revision under another hash).
func TestTheCurrentRevisionIsJudged(t *testing.T) {
	for _, c := range []struct {
		name     string
		known    bool
		revision int64
		hash     string
		want     shipment.OrderBinding
	}{
		{"amended", true, 3, current.OrderHash, current},
		{"the same revision", true, 2, order.OrderHash, order},
		{"unknown", false, 0, "", order},
		{"an earlier revision", true, 1, current.OrderHash, order},
		{"the bound revision under another hash", true, 2, current.OrderHash, order},
	} {
		if got := Current(order, c.known, c.revision, c.hash); got != c.want {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

// Ended orders: OrderGuard v1 on the current binding. An active order and one ended by order.close
// or order.close_remainder (status closed, TRD's ORDER_NOT_ACTIVE) serve; a cancelled one (not
// reachable with a confirmed shipment since C4a) and a draft are ORDER_NOT_ACTIVE; a revoked
// activating approval is ORDER_APPROVAL_REVOKED whatever the state; an order TRD moved on during the
// transaction is REFERENCE_STALE; an unknown one REFERENCE_INVALID.
func TestEndedOrdersStillReleaseTheirDocuments(t *testing.T) {
	for _, c := range []struct {
		name string
		v    shipment.OrderVerdict
		want string
	}{
		{"active", shipment.OrderVerdict{Known: true, Status: OrderActive, OrderRevision: 3}, "OK"},
		{"closed", shipment.OrderVerdict{Known: true, Status: OrderClosed, OrderRevision: 3, Code: shipment.CodeOrderNotActive}, "OK"},
		{"cancelled", shipment.OrderVerdict{Known: true, Status: OrderCancelled, OrderRevision: 3, Code: shipment.CodeOrderNotActive},
			shipment.CodeOrderNotActive},
		{"draft", shipment.OrderVerdict{Known: true, Status: "draft", OrderRevision: 3, Code: shipment.CodeOrderNotActive}, shipment.CodeOrderNotActive},
		{"revoked", shipment.OrderVerdict{Known: true, Status: OrderActive, OrderRevision: 3, ApprovalRevoked: true,
			Code: shipment.CodeOrderApprovalRevoked}, shipment.CodeOrderApprovalRevoked},
		{"closed and revoked", shipment.OrderVerdict{Known: true, Status: OrderClosed, OrderRevision: 3, ApprovalRevoked: true,
			Code: shipment.CodeOrderNotActive}, shipment.CodeOrderApprovalRevoked},
		{"moved on", shipment.OrderVerdict{Known: true, Status: OrderActive, OrderRevision: 4, Code: shipment.CodeReferenceStale},
			shipment.CodeReferenceStale},
		{"unknown", shipment.OrderVerdict{Code: kernel.CodeReferenceInvalid}, kernel.CodeReferenceInvalid},
		{"another code", shipment.OrderVerdict{Known: true, Status: OrderActive, OrderRevision: 3, Code: "SOMETHING_ELSE"}, kernel.CodeReferenceInvalid},
	} {
		e, err := CheckDocumentsOrder(current, c.v, "/args/shipment_id")
		if code(err) != c.want || e.Name != shipment.GuardOrder || e.Value != c.v.Status+"/r"+kernel.DecimalFromInt(c.v.OrderRevision).String() {
			t.Errorf("%s: %v %+v", c.name, err, e)
		}
	}
	_, err := CheckDocumentsOrder(current, shipment.OrderVerdict{Known: true, Status: OrderActive, OrderRevision: 4, Code: shipment.CodeReferenceStale}, "/x")
	if r, _ := kernel.AsRejection(err); r == nil || !strings.Contains(r.Fields[0].Reason, "moved to revision 4 while its revision 3 was judged") {
		t.Fatalf("stale: %v", err)
	}
}

// An exception of the documents release of a confirmed shipment is bound to the revision the payment
// condition was judged on — the order's current one — and covers that revision only; one granted on
// the shipment's revision before the amendment covers nothing after it.
func TestAnExceptionBindsTheJudgedRevision(t *testing.T) {
	r := New("rls.d1", "shipment.1", ScopeDocuments, order, doc)
	r.Evaluate(Verdict{Failure: blocked(shipment.CodeCommercialReleaseBlocked), Guard: shipment.GuardPaymentCondition}, doc)
	before := unpaid()
	before.Scope = ScopeDocuments
	if _, err := r.GrantException(exceptionRequest(r), nil, before, nil, manager); err != nil || r.Exceptions[0].Order != order {
		t.Fatalf("on the bound revision: %v %+v", err, r.Exceptions)
	}
	after := before
	after.OrderRevision, after.OrderHash = current.OrderRevision, current.OrderHash
	if _, _, err := r.CheckPayment(after, t0.Add(time.Hour), "/x"); code(err) != shipment.CodeCommercialReleaseBlocked {
		t.Fatalf("revision 2's exception after the amendment: %v", err)
	}
	r.Evaluate(Verdict{Failure: blocked(shipment.CodeCommercialReleaseBlocked), Guard: shipment.GuardPaymentCondition}, doc)
	req := exceptionRequest(r)
	req.Order = current
	x, err := r.GrantException(req, nil, after, nil, manager)
	if err != nil || x.Order != current || x.Seq != 2 || r.Status != StatusGranted {
		t.Fatalf("on the current revision: %v %+v", err, x)
	}
	if _, seq, err := r.CheckPayment(after, t0.Add(time.Hour), "/x"); err != nil || seq != 2 {
		t.Fatalf("covered on the current revision: %d %v", seq, err)
	}
}
