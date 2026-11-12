package application

import (
	"context"
	"slices"
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
}

func (g *guards) ResolveActiveOrders(context.Context, []string) (map[string]trdapi.ActiveOrderFact, error) {
	return nil, nil
}
func (g *guards) GuardOrder(context.Context, bentopg.Executor, trdapi.OrderBinding) (trdapi.OrderVerdict, error) {
	g.calls = append(g.calls, "trd")
	return g.order, nil
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
func (g *guards) GuardScreening(context.Context, bentopg.Executor, string, time.Time) (ptyapi.ScreeningGuardResult, error) {
	g.calls = append(g.calls, "pty")
	return g.screening, nil
}
func (g *guards) GuardTradeCompliance(_ context.Context, _ bentopg.Executor, b cnxapi.OrderBinding, _ time.Time) (cnxapi.TradeComplianceVerdict, error) {
	g.calls = append(g.calls, "cnx.compliance")
	v := g.compliance
	v.Ref = b
	return v, nil
}
func (g *guards) GuardExportLicense(context.Context, bentopg.Executor, string, time.Time) (cnxapi.ExportLicenseVerdict, error) {
	g.calls = append(g.calls, "cnx.license")
	return g.license, nil
}
func (g *guards) GuardPaymentCondition(_ context.Context, _ bentopg.Executor, q stlapi.PaymentQuery) (stlapi.PaymentVerdict, error) {
	g.calls = append(g.calls, "stl."+q.Scope)
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
