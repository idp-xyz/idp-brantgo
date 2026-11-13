package application

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/release"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// guards answers every guard of FUL's release from memory and records the order of the calls.
type guards struct {
	calls      []string
	compliance cnxapi.TradeComplianceVerdict
	license    cnxapi.ExportLicenseVerdict
	screening  ptyapi.ScreeningGuardResult
	delivery   DeliveryLine
	quality    qltapi.QualityReleaseVerdict
	payment    stlapi.PaymentVerdict
	order      trdapi.OrderVerdict
	state      trdapi.OrderState
	// complianceAt answers CNX's guard for an order revision instead of compliance (S5 C1c).
	complianceAt map[int64]cnxapi.TradeComplianceVerdict
	// what CNX's, PTY's, STL's and TRD's guards were asked
	complied []cnxapi.OrderBinding
	screened []string
	paid     []int64
	guarded  []trdapi.OrderBinding
}

func (g *guards) ResolveActiveOrders(context.Context, []string) (map[string]trdapi.ActiveOrderFact, error) {
	return nil, nil
}
func (g *guards) GuardOrder(_ context.Context, _ bentopg.Executor, b trdapi.OrderBinding) (trdapi.OrderVerdict, error) {
	g.calls, g.guarded = append(g.calls, "trd"), append(g.guarded, b)
	v := g.order
	v.Ref = b
	return v, nil
}
func (g *guards) OrderState(_ context.Context, _ bentopg.Executor, id string) (trdapi.OrderState, error) {
	g.calls = append(g.calls, "trd.state")
	v := g.state
	v.OrderID = id
	return v, nil
}
func (g *guards) ResolveReleases(context.Context, []string, time.Time) (map[qltapi.QualityReleaseRef]qltapi.ResolvedRelease, error) {
	return nil, nil
}
func (g *guards) GuardRelease(context.Context, bentopg.Executor, qltapi.QualityReleaseRef, time.Time) (qltapi.QualityReleaseVerdict, error) {
	g.calls = append(g.calls, "qlt")
	return g.quality, nil
}
func (g *guards) ResolveDeliveryLines(context.Context, []api.DeliveryLineRef) (map[api.DeliveryLineRef]DeliveryLine, error) {
	return nil, nil
}
func (g *guards) GuardDeliveryLine(context.Context, bentopg.Executor, api.DeliveryLineRef) (DeliveryLine, error) {
	g.calls = append(g.calls, "pur")
	return g.delivery, nil
}
func (g *guards) GuardScreening(_ context.Context, _ bentopg.Executor, id string, _ time.Time) (ptyapi.ScreeningGuardResult, error) {
	g.calls, g.screened = append(g.calls, "pty"), append(g.screened, id)
	return g.screening, nil
}
func (g *guards) GuardTradeCompliance(_ context.Context, _ bentopg.Executor, b cnxapi.OrderBinding, _ time.Time) (cnxapi.TradeComplianceVerdict, error) {
	g.calls, g.complied = append(g.calls, "cnx.compliance"), append(g.complied, b)
	v := g.compliance
	if x, ok := g.complianceAt[b.OrderRevision]; ok {
		v = x
	}
	v.Ref = b
	return v, nil
}
func (g *guards) GuardExportLicense(context.Context, bentopg.Executor, string, time.Time) (cnxapi.ExportLicenseVerdict, error) {
	g.calls = append(g.calls, "cnx.license")
	return g.license, nil
}
func (g *guards) GuardPaymentCondition(_ context.Context, _ bentopg.Executor, q stlapi.PaymentQuery) (stlapi.PaymentVerdict, error) {
	g.calls, g.paid = append(g.calls, "stl."+q.Scope), append(g.paid, q.OrderRevision)
	v := g.payment
	v.Query = q
	return v, nil
}

var hash = strings.Repeat("a", 64)

// clearGuards is every guard satisfied for shipmentOf's shipment: 200 PCS of order.1 L1 from
// delivery.1 L1 (800 verified) under rel.1 (600 released), customer.1 clear, 960.00 due before
// shipment fully applied.
func clearGuards() *guards {
	scope := qltapi.ReleaseScope{Subject: qltapi.SubjectRef{Kind: qltapi.SubjectCommitmentLine, ID: "commitment.1", LineID: "L1"}, SupplierID: "supplier.1",
		OrderID: "order.1", OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 2, UnitID: "pcs", UnitRevision: 1}
	return &guards{
		compliance: cnxapi.TradeComplianceVerdict{Clear: true, Status: "clear", Reason: "clear", CheckID: "chk.1", DecisionID: "dec.1",
			RegulationID: "screening", RegulationRevision: 1},
		screening: ptyapi.ScreeningGuardResult{Clear: true, Status: "clear", Reason: "no_match", ScreeningID: "scr.1", PartyRevision: 3},
		delivery: DeliveryLine{Known: true, Status: "verified", VerifiedVersion: 1, CommitmentID: "commitment.1", SupplierID: "supplier.1", OrderID: "order.1",
			OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 2, UnitID: "pcs", UnitRevision: 1, VerifiedQuantity: "800"},
		quality: qltapi.QualityReleaseVerdict{Known: true, Status: "granted", Scope: scope, Quantity: "600", ValidUntil: time.Now().Add(time.Hour)},
		payment: stlapi.PaymentVerdict{Status: stlapi.PaymentSatisfied, OrderRevision: 1, OrderHash: hash, Currency: "USD", RequiredMinor: "96000",
			AppliedMinor: "96000"},
		order: trdapi.OrderVerdict{Known: true, Status: "active", OrderRevision: 1},
	}
}

func shipmentOf(status string) *shipment.Shipment {
	return &shipment.Shipment{ID: "shipment.1", Status: status, Order: shipment.OrderBinding{OrderID: "order.1", OrderRevision: 1, OrderHash: hash},
		Lines: []shipment.Line{{ID: "S1", OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 2, UnitID: "pcs", UnitRevision: 1,
			OrderQuantity: kernel.MustDecimal("1000"), Quantity: kernel.MustDecimal("200"),
			Delivery: shipment.DeliveryRef{DeliveryID: "delivery.1", LineID: "L1"}, DeliveryVersion: 1, CommitmentID: "commitment.1",
			SupplierID: "supplier.1", ReleaseID: "rel.1"}},
		Parties: []shipment.Party{{Role: shipment.RoleCustomer, PartyID: "customer.1", PartyRevision: 3}}}
}

func judged(t *testing.T, g *guards, sh *shipment.Shipment, scope string, rel *release.Release) *judgement {
	t.Helper()
	s := Service{Upstream: Upstream{Orders: g, Releases: g, Deliveries: g, Screening: g, Compliance: g, Payments: g}}
	j, err := s.judge(context.Background(), nil, sh, shipment.Consumed{}, scope, rel, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func codeOf(r *kernel.Rejection) string {
	if r == nil {
		return "OK"
	}
	return r.Code
}

// FUL-INV-002, ADR-BR-011, D-11: the release runs every guard in context code order — CNX (the
// licence only when the clear conclusion asks for one), PTY, PUR, QLT, STL for the scope, TRD — and
// binds what each answered.
func TestJudgeRunsEveryGuardInCodeOrder(t *testing.T) {
	g := clearGuards()
	j := judged(t, g, shipmentOf(shipment.StatusPrepared), release.ScopeShipment, nil)
	if j.failure != nil || j.other != nil || !slices.Equal(g.calls, []string{"cnx.compliance", "pty", "pur", "qlt", "stl.shipment", "trd"}) {
		t.Fatalf("%v %v", j.failure, g.calls)
	}
	var names []string
	for _, e := range j.entries {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"guards_checked_at", shipment.GuardTradeCompliance, "screening:customer.1",
		"supplier_delivery:delivery.1/L1/v1", "quality_release:rel.1", shipment.GuardPaymentCondition, shipment.GuardOrder}) {
		t.Fatal(names)
	}
	b := j.bindings
	if b.TradeCompliance.CheckID != "chk.1" || b.ExportLicense != nil || b.Screenings[0].ScreeningID != "scr.1" || b.Deliveries[0].Verified != "800" ||
		b.QualityReleases[0].Released != "600" || b.Payment.AppliedMinor != "96000" || b.Order.Status != "active" {
		t.Fatalf("bindings %+v", b)
	}

	g = clearGuards()
	g.compliance.LicenseRequired = true
	g.license = cnxapi.ExportLicenseVerdict{Status: cnxapi.LicenseStatusRequired, Reason: cnxapi.LicenseReasonNotAllocated,
		Code: cnxapi.CodeExportLicenseRequired}
	j = judged(t, g, shipmentOf(shipment.StatusPrepared), release.ScopeDocuments, nil)
	if codeOf(j.failure) != api.CodeExportLicenseRequired || j.guard != shipment.GuardExportLicense ||
		!slices.Equal(g.calls, []string{"cnx.compliance", "cnx.license", "pty", "pur", "qlt", "stl.documents", "trd"}) {
		t.Fatalf("licence: %v %s %v", j.failure, j.guard, g.calls)
	}
	lines := licenseLines(shipmentOf(shipment.StatusPrepared))
	sum, _ := cnxapi.LinesHash(lines)
	g = clearGuards()
	g.compliance.LicenseRequired = true
	g.license = cnxapi.ExportLicenseVerdict{Covered: true, Status: cnxapi.LicenseStatusAllocated, Reason: cnxapi.LicenseReasonAllocated, LicenseID: "elc.1",
		AllocationID: "alloc.1", Quantity: "1000", Unit: "pcs", LinesHash: sum}
	if j = judged(t, g, shipmentOf(shipment.StatusPrepared), release.ScopeShipment, nil); j.failure != nil || j.bindings.ExportLicense.AllocationID != "alloc.1" {
		t.Fatalf("covered: %v %+v", j.failure, j.bindings.ExportLicense)
	}
}

// The first unmet guard in code order decides the outcome; the first one that is not the payment
// condition decides an exception (FUL-INV-002: compliance, screening, quality and the rest are
// never excepted).
func TestJudgeFindsTheFirstUnmetGuard(t *testing.T) {
	unpaid := func(g *guards) {
		g.payment.Status, g.payment.Reason, g.payment.AppliedMinor, g.payment.StageID, g.payment.ShortfallMinor, g.payment.Code =
			stlapi.PaymentBlocked, stlapi.PaymentStageUnpaid, "95800", "P2", "200", stlapi.CodeCommercialReleaseBlocked
	}
	for _, c := range []struct {
		name         string
		edit         func(*guards)
		failure      string
		guard, other string
	}{
		{"payment only", unpaid, api.CodeCommercialReleaseBlocked, shipment.GuardPaymentCondition, "OK"},
		{"compliance blocked", func(g *guards) {
			g.compliance = cnxapi.TradeComplianceVerdict{Status: "blocked", Reason: "blocked", Code: cnxapi.CodeTradeComplianceNotClear}
			unpaid(g)
		}, api.CodeTradeComplianceNotClear, shipment.GuardTradeCompliance, api.CodeTradeComplianceNotClear},
		{"screening match", func(g *guards) {
			g.screening = ptyapi.ScreeningGuardResult{Status: "blocked", Reason: "match"}
		}, api.CodePartyScreeningNotClear, shipment.GuardPartyScreening, api.CodePartyScreeningNotClear},
		{"quality revoked after payment", func(g *guards) {
			unpaid(g)
			g.quality.Status, g.quality.Code = "revoked", "QUALITY_RELEASE_REQUIRED"
		}, api.CodeQualityReleaseRequired, shipment.GuardQualityRelease, api.CodeQualityReleaseRequired},
		{"order revoked", func(g *guards) {
			unpaid(g)
			g.order.Code, g.order.Status = trdapi.CodeOrderApprovalRevoked, "active"
		}, api.CodeCommercialReleaseBlocked, shipment.GuardPaymentCondition, api.CodeOrderApprovalRevoked},
		{"delivery claim", func(g *guards) { g.delivery.Code = api.CodeSupplierDeliveryUnverified }, api.CodeSupplierDeliveryUnverified,
			shipment.GuardSupplierDelivery, api.CodeSupplierDeliveryUnverified},
	} {
		g := clearGuards()
		c.edit(g)
		j := judged(t, g, shipmentOf(shipment.StatusPrepared), release.ScopeShipment, nil)
		if codeOf(j.failure) != c.failure || j.guard != c.guard || codeOf(j.other) != c.other || len(g.calls) != 6 {
			t.Errorf("%s: %s %s %s %v", c.name, codeOf(j.failure), j.guard, codeOf(j.other), g.calls)
		}
	}
	g := clearGuards()
	g.order.Code = trdapi.CodeOrderApprovalRevoked
	sh := shipmentOf(shipment.StatusPrepared)
	sh.Lines[0].Quantity = kernel.MustDecimal("1001")
	if j := judged(t, g, sh, release.ScopeShipment, nil); j.guard != shipment.GuardSupplierDelivery {
		t.Fatalf("more than the delivery line verified: %s", j.guard)
	}
}

// A document release re-judges a confirmed shipment: the quantities it shipped itself are not
// counted against it.
func TestJudgeOfAConfirmedShipment(t *testing.T) {
	g := clearGuards()
	sh := shipmentOf(shipment.StatusConfirmed)
	s := Service{Upstream: Upstream{Orders: g, Releases: g, Deliveries: g, Screening: g, Compliance: g, Payments: g}}
	two := kernel.MustDecimal("200")
	consumed := shipment.Consumed{OrderLines: map[string]kernel.Decimal{"L1": two},
		Deliveries: map[shipment.DeliveryRef]kernel.Decimal{sh.Lines[0].Delivery: two}, Releases: map[string]kernel.Decimal{"rel.1": two}}
	g.quality.Quantity, g.delivery.VerifiedQuantity = "200", "200"
	j, err := s.judge(context.Background(), nil, sh, consumed, release.ScopeDocuments, nil, time.Now())
	if err != nil || j.failure != nil {
		t.Fatalf("%v %v", err, j.failure)
	}
}

var hash3 = strings.Repeat("c", 64)

// consumedShipmentRelease is the shipment release shipment.1's confirmation consumed: granted by
// an evaluation of every guard (clearGuards), then consumed.
func consumedShipmentRelease(t *testing.T) *release.Release {
	t.Helper()
	sh := shipmentOf(shipment.StatusPrepared)
	j := judged(t, clearGuards(), sh, release.ScopeShipment, nil)
	r := release.New("rls.s1", sh.ID, release.ScopeShipment, sh.Order, shipment.Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "doc.1"}, Epoch: 1,
		At: time.Now()})
	r.Evaluate(release.Verdict{Entries: j.entries, Bindings: j.bindings}, r.Opened)
	if err := r.Consume("", []string{"evidence"}, j.entries, shipment.Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "merch.1"}, Epoch: 1,
		At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return r
}

// amendedGuards is clearGuards after the order was amended to revision 3: TRD's order state and
// OrderGuard v1 on the current binding, STL's documents scope paid on revision 3.
func amendedGuards() *guards {
	g := clearGuards()
	g.state = trdapi.OrderState{Known: true, Status: "active", OrderRevision: 3, OrderHash: hash3}
	g.order = trdapi.OrderVerdict{Known: true, Status: "active", OrderRevision: 3, OrderHash: hash3}
	g.payment.OrderRevision, g.payment.OrderHash = 3, hash3
	return g
}

func judgedRelease(t *testing.T, g *guards, roots *releaseRootsOf, scope string) *judgement {
	t.Helper()
	s := Service{Upstream: Upstream{Orders: g, Releases: g, Deliveries: g, Screening: g, Compliance: g, Payments: g}}
	j, err := s.judgeRelease(context.Background(), nil, roots, scope, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// documentsCalls is what the documents release of a confirmed shipment asks (S5 C1b, C1c): TRD's
// order state, CNX on the revision the goods left under and on the later current revision, PTY on
// the customer, STL's documents scope, TRD's OrderGuard.
var documentsCalls = []string{"trd.state", "cnx.compliance", "cnx.compliance", "pty", "stl.documents", "trd"}

func entryNames(j *judgement) []string {
	var names []string
	for _, e := range j.entries {
		names = append(names, e.Name)
	}
	return names
}

// Owner decisions 2026-11-12 (S5 C1b and C1c; WF-07, BT10, D-5, R27, R44, R45, R56): the documents
// release of a confirmed shipment asks TRD's order state for the current revision, then in context
// code order CNX's TradeComplianceGuard on the revision the goods left under and on the current
// revision, PTY's PartyScreeningGuard on every party, STL's PaymentConditionGuard for the documents
// scope on the current revision and TRD's OrderGuard on the current binding; it asks PUR and QLT
// nothing: it binds what the confirmation's release bound, names the confirmation and binds CNX's and
// PTY's answers beside it. A confirmed shipment whose confirmation consumed no shipment release, the
// shipment scope and a prepared shipment's documents release are judged as before.
func TestJudgeDocumentsOfAConfirmedShipmentOnTheCurrentRevision(t *testing.T) {
	conf := consumedShipmentRelease(t)
	g := amendedGuards()
	roots := &releaseRootsOf{existing: shipmentOf(shipment.StatusConfirmed), confirmation: conf}
	j := judgedRelease(t, g, roots, release.ScopeDocuments)
	if j.failure != nil || !slices.Equal(g.calls, documentsCalls) || !slices.Equal(g.paid, []int64{3}) ||
		len(g.guarded) != 1 || g.guarded[0] != (trdapi.OrderBinding{OrderID: "order.1", OrderRevision: 3, OrderHash: hash3}) ||
		!slices.Equal(g.complied, []cnxapi.OrderBinding{{OrderID: "order.1", OrderRevision: 1, OrderHash: hash}, {OrderID: "order.1",
			OrderRevision: 3, OrderHash: hash3}}) || !slices.Equal(g.screened, []string{"customer.1"}) {
		t.Fatalf("%v %v paid %v guarded %v complied %v", j.failure, g.calls, g.paid, g.guarded, g.complied)
	}
	if j.order != (shipment.OrderBinding{OrderID: "order.1", OrderRevision: 3, OrderHash: hash3}) {
		t.Fatalf("judged %+v", j.order)
	}
	if !slices.Equal(entryNames(j), []string{"guards_checked_at", release.GuardConfirmation, shipment.GuardTradeCompliance,
		release.GuardTradeComplianceCurrent, "screening:customer.1", shipment.GuardPaymentCondition, shipment.GuardOrder}) ||
		j.entries[1].Value != "rls.s1/v2/consumed/"+conf.Consumption.EntriesSHA256+"/r1" || j.entries[2].Value != "clear/clear/chk.1/dec.1/false/r1" ||
		j.entries[3].Value != "clear/clear/chk.1/dec.1/false/r3" || j.entries[4].Value != "clear/no_match/scr.1" ||
		!strings.Contains(j.entries[5].Value, "/r3/") || j.entries[6].Value != "active/r3" {
		t.Fatalf("entries %+v", j.entries)
	}
	b := j.bindings
	if b.Confirmation == nil || b.Confirmation.ReleaseID != "rls.s1" || b.TradeCompliance.CheckID != "chk.1" || b.Screenings[0].ScreeningID != "scr.1" ||
		b.Deliveries[0].Verified != "800" || b.QualityReleases[0].Released != "600" || b.Payment.Scope != release.ScopeDocuments ||
		b.Payment.OrderRevision != 3 || b.Order.OrderRevision != 3 || b.Order.OrderHash != hash3 || b.Order.Status != "active" {
		t.Fatalf("bindings %+v", b)
	}
	if !slices.Equal(b.Confirmation.Reused, []string{shipment.GuardTradeCompliance, shipment.GuardSupplierDelivery, shipment.GuardQualityRelease,
		release.GuardFulfilment}) || !slices.Equal(b.Confirmation.Rechecked, release.Rechecked) {
		t.Fatalf("reused %v rechecked %v", b.Confirmation.Reused, b.Confirmation.Rechecked)
	}
	if r := b.Rechecked; r == nil || len(r.TradeCompliance) != 2 || r.TradeCompliance[0].OrderRevision != 1 || r.TradeCompliance[0].OrderHash != hash ||
		r.TradeCompliance[1].OrderRevision != 3 || r.TradeCompliance[1].Blocked || len(r.Screenings) != 1 ||
		r.Screenings[0] != (release.RecheckedScreening{PartyID: "customer.1", Status: "clear", Reason: "no_match", ScreeningID: "scr.1", PartyRevision: 3}) {
		t.Fatalf("rechecked %+v", b.Rechecked)
	}

	// Ended orders, an unpaid stage of the current revision, TRD moving on, an unknown state.
	unpaid := func(g *guards) {
		g.payment.Status, g.payment.Reason, g.payment.AppliedMinor, g.payment.StageID, g.payment.ShortfallMinor, g.payment.Code =
			stlapi.PaymentBlocked, stlapi.PaymentStageUnpaid, "28800", "P2", "61600", stlapi.CodeCommercialReleaseBlocked
	}
	for _, c := range []struct {
		name           string
		edit           func(*guards)
		failure, other string
		paid           int64
	}{
		{"closed short", func(g *guards) { g.order.Status, g.order.Code = "closed", trdapi.CodeOrderNotActive }, "OK", "OK", 3},
		{"cancelled", func(g *guards) { g.order.Status, g.order.Code = "cancelled", trdapi.CodeOrderNotActive }, api.CodeOrderNotActive,
			api.CodeOrderNotActive, 3},
		{"the current revision's stage unpaid", unpaid, api.CodeCommercialReleaseBlocked, "OK", 3},
		{"revoked", func(g *guards) { g.order.ApprovalRevoked, g.order.Code = true, trdapi.CodeOrderApprovalRevoked }, api.CodeOrderApprovalRevoked,
			api.CodeOrderApprovalRevoked, 3},
		{"moved on", func(g *guards) { g.order.OrderRevision, g.order.Code = 4, trdapi.CodeReferenceStale }, api.CodeReferenceStale,
			api.CodeReferenceStale, 3},
		{"no later revision", func(g *guards) {
			g.state = trdapi.OrderState{Known: true, Status: "active", OrderRevision: 1, OrderHash: hash}
			g.order.OrderRevision, g.payment.OrderRevision = 1, 1
		}, "OK", "OK", 1},
	} {
		g := amendedGuards()
		c.edit(g)
		j := judgedRelease(t, g, &releaseRootsOf{existing: shipmentOf(shipment.StatusConfirmed), confirmation: conf}, release.ScopeDocuments)
		calls := documentsCalls
		if c.paid == 1 {
			calls = slices.Delete(slices.Clone(documentsCalls), 2, 3) // no later revision: CNX asked once
		}
		if codeOf(j.failure) != c.failure || codeOf(j.other) != c.other || !slices.Equal(g.paid, []int64{c.paid}) || !slices.Equal(g.calls, calls) {
			t.Errorf("%s: %s %s paid %v %v", c.name, codeOf(j.failure), codeOf(j.other), g.paid, g.calls)
		}
	}

	// Judged as before: no consumed shipment release, the shipment scope, a prepared shipment.
	for _, c := range []struct {
		name  string
		roots *releaseRootsOf
		scope string
		stl   string
	}{
		{"no confirmation", &releaseRootsOf{existing: shipmentOf(shipment.StatusConfirmed)}, release.ScopeDocuments, "stl.documents"},
		{"a granted release is no confirmation", &releaseRootsOf{existing: shipmentOf(shipment.StatusConfirmed),
			confirmation: release.New("rls.x", "shipment.1", release.ScopeShipment, shipmentOf("").Order, shipment.Act{})}, release.ScopeDocuments,
			"stl.documents"},
		{"a prepared shipment", &releaseRootsOf{existing: shipmentOf(shipment.StatusPrepared), confirmation: conf}, release.ScopeDocuments,
			"stl.documents"},
		{"the shipment scope", &releaseRootsOf{existing: shipmentOf(shipment.StatusPrepared)}, release.ScopeShipment, "stl.shipment"},
	} {
		g := clearGuards()
		j := judgedRelease(t, g, c.roots, c.scope)
		if !slices.Equal(g.calls, []string{"cnx.compliance", "pty", "pur", "qlt", c.stl, "trd"}) || !slices.Equal(g.paid, []int64{1}) ||
			j.bindings.Confirmation != nil || j.order.OrderRevision != 1 {
			t.Errorf("%s: %v paid %v %+v", c.name, g.calls, g.paid, j.bindings.Confirmation)
		}
	}
}

// Owner decision 2026-11-12 ~23:05 (S5 C1c; FUL-INV-002, CNX-INV-001, SM-CNX-COMPLIANCE, PTY-INV-002,
// R44, R56): a block that appears after the confirmation still stops the documents release of a
// confirmed shipment. CNX refuses it only for a blocked conclusion of the revision the goods left
// under or of the order's current revision; an expired conclusion, a newer programme list version, a
// check reopened, re-evaluated or not yet opened for the current revision leave the confirmation's
// clear conclusion standing. PTY refuses anything but clear, as at the confirmation. Both are
// noted before the payment condition and neither is excepted (j.other); every guard is still asked.
func TestJudgeDocumentsAsksForABlockAfterTheConfirmation(t *testing.T) {
	conf := consumedShipmentRelease(t)
	cnx := func(rev int64, status, reason string) func(*guards) {
		return func(g *guards) {
			if g.complianceAt == nil {
				g.complianceAt = map[int64]cnxapi.TradeComplianceVerdict{}
			}
			v := cnxapi.TradeComplianceVerdict{Status: status, Reason: reason, CheckID: "chk.r" + strconv.FormatInt(rev, 10)}
			if status == cnxapi.StatusBlocked {
				v.DecisionID = "dec.blocked"
			}
			if status != cnxapi.StatusClear {
				v.Code = cnxapi.CodeTradeComplianceNotClear
			}
			g.complianceAt[rev] = v
		}
	}
	pty := func(status, reason string) func(*guards) {
		return func(g *guards) {
			g.screening = ptyapi.ScreeningGuardResult{Status: status, Reason: reason, ScreeningID: "scr.2", PartyRevision: 3}
		}
	}
	unpaid := func(g *guards) {
		g.payment.Status, g.payment.Reason, g.payment.AppliedMinor, g.payment.StageID, g.payment.ShortfallMinor, g.payment.Code =
			stlapi.PaymentBlocked, stlapi.PaymentStageUnpaid, "28800", "P2", "61600", stlapi.CodeCommercialReleaseBlocked
	}
	both := func(fs ...func(*guards)) func(*guards) {
		return func(g *guards) {
			for _, f := range fs {
				f(g)
			}
		}
	}
	for _, c := range []struct {
		name           string
		edit           func(*guards)
		failure, guard string
		other          string
		blocked        []bool
	}{
		{"revision 1 reopened and concluded blocked", cnx(1, cnxapi.StatusBlocked, cnxapi.ReasonBlocked), api.CodeTradeComplianceNotClear,
			shipment.GuardTradeCompliance, api.CodeTradeComplianceNotClear, []bool{true, false}},
		{"the current revision concluded blocked", cnx(3, cnxapi.StatusBlocked, cnxapi.ReasonBlocked), api.CodeTradeComplianceNotClear,
			shipment.GuardTradeCompliance, api.CodeTradeComplianceNotClear, []bool{false, true}},
		{"blocked and unpaid: the block first, never excepted", both(cnx(1, cnxapi.StatusBlocked, cnxapi.ReasonBlocked), unpaid),
			api.CodeTradeComplianceNotClear, shipment.GuardTradeCompliance, api.CodeTradeComplianceNotClear, []bool{true, false}},
		{"blocked under another hash", cnx(1, cnxapi.StatusBlocked, cnxapi.ReasonOrderChanged), api.CodeTradeComplianceNotClear,
			shipment.GuardTradeCompliance, api.CodeTradeComplianceNotClear, []bool{true, false}},
		{"revision 1 expired", cnx(1, cnxapi.StatusExpired, cnxapi.ReasonExpired), "OK", "", "OK", []bool{false, false}},
		{"a newer programme list version", cnx(1, cnxapi.StatusPending, cnxapi.ReasonListUpdated), "OK", "", "OK", []bool{false, false}},
		{"revision 1 reopened", cnx(1, cnxapi.StatusPending, cnxapi.ReasonNotEvaluated), "OK", "", "OK", []bool{false, false}},
		{"revision 1 evaluated again, not concluded", cnx(1, cnxapi.StatusPending, cnxapi.ReasonAwaitingDecision), "OK", "", "OK", []bool{false, false}},
		{"no check of the current revision yet", cnx(3, cnxapi.StatusPending, cnxapi.ReasonOrderRevised), "OK", "", "OK", []bool{false, false}},
		{"expired and unpaid: the payment condition alone, excepted", both(cnx(1, cnxapi.StatusExpired, cnxapi.ReasonExpired), unpaid),
			api.CodeCommercialReleaseBlocked, shipment.GuardPaymentCondition, "OK", []bool{false, false}},
		{"a decided true match", pty(ptyapi.ScreeningBlocked, ptyapi.ReasonTrueMatch), api.CodePartyScreeningNotClear, shipment.GuardPartyScreening,
			api.CodePartyScreeningNotClear, []bool{false, false}},
		{"the negative list", pty(ptyapi.ScreeningBlocked, ptyapi.ReasonNegativeList), api.CodePartyScreeningNotClear,
			shipment.GuardPartyScreening, api.CodePartyScreeningNotClear, []bool{false, false}},
		{"a suspected match", pty(ptyapi.ScreeningMatch, ptyapi.ReasonSuspectedMatch), api.CodePartyScreeningNotClear,
			shipment.GuardPartyScreening, api.CodePartyScreeningNotClear, []bool{false, false}},
		{"a screening list with a newer version", pty(ptyapi.ScreeningPending, ptyapi.ReasonListUpdated), api.CodePartyScreeningNotClear,
			shipment.GuardPartyScreening, api.CodePartyScreeningNotClear, []bool{false, false}},
		{"the screening expired", pty(ptyapi.ScreeningExpired, ptyapi.ReasonExpired), api.CodePartyScreeningNotClear,
			shipment.GuardPartyScreening, api.CodePartyScreeningNotClear, []bool{false, false}},
		{"the party changed", pty(ptyapi.ScreeningPending, ptyapi.ReasonPartyChanged), api.CodePartyScreeningNotClear,
			shipment.GuardPartyScreening, api.CodePartyScreeningNotClear, []bool{false, false}},
		{"screened and unpaid: screening first", both(pty(ptyapi.ScreeningBlocked, ptyapi.ReasonTrueMatch), unpaid),
			api.CodePartyScreeningNotClear, shipment.GuardPartyScreening, api.CodePartyScreeningNotClear, []bool{false, false}},
		{"compliance before screening", both(cnx(3, cnxapi.StatusBlocked, cnxapi.ReasonBlocked), pty(ptyapi.ScreeningBlocked, ptyapi.ReasonTrueMatch)),
			api.CodeTradeComplianceNotClear, shipment.GuardTradeCompliance, api.CodeTradeComplianceNotClear, []bool{false, true}},
	} {
		g := amendedGuards()
		c.edit(g)
		j := judgedRelease(t, g, &releaseRootsOf{existing: shipmentOf(shipment.StatusConfirmed), confirmation: conf}, release.ScopeDocuments)
		if codeOf(j.failure) != c.failure || j.guard != c.guard || codeOf(j.other) != c.other || !slices.Equal(g.calls, documentsCalls) {
			t.Errorf("%s: %s %s %s %v", c.name, codeOf(j.failure), j.guard, codeOf(j.other), g.calls)
			continue
		}
		r := j.bindings.Rechecked
		if len(r.TradeCompliance) != 2 || r.TradeCompliance[0].Blocked != c.blocked[0] || r.TradeCompliance[1].Blocked != c.blocked[1] ||
			r.Screenings[0].Status != g.screening.Status || r.Screenings[0].Reason != g.screening.Reason || j.bindings.Screenings[0].ScreeningID != "scr.1" {
			t.Errorf("%s: rechecked %+v", c.name, r)
		}
		if c.guard == shipment.GuardPartyScreening && path0(j.failure) != "/shipment/parties/0" ||
			c.guard == shipment.GuardTradeCompliance && path0(j.failure) != "/args/shipment_id" {
			t.Errorf("%s: at %s", c.name, path0(j.failure))
		}
	}
}

func path0(r *kernel.Rejection) string {
	if r == nil || len(r.Fields) == 0 {
		return ""
	}
	return r.Fields[0].Path
}
