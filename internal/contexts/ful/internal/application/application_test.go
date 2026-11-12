package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// humanOnlyCommands names shipment.confirm; the other shipment commands are human or AI
// (SM-FUL-SHIPMENT); a system actor runs none of them.
func TestRules(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	scope := kernel.Scope{Tenant: "tenant.1", Company: "company.A"}
	for _, c := range []struct {
		command string
		kind    kernel.ActorKind
		ok      bool
	}{
		{api.CommandConfirmShipment, kernel.ActorHuman, true},
		{api.CommandConfirmShipment, kernel.ActorAI, false},
		{api.CommandConfirmShipment, kernel.ActorSystem, false},
		{api.CommandPrepareShipment, kernel.ActorAI, true},
		{api.CommandPrepareShipment, kernel.ActorSystem, false},
		{api.CommandCancelShipment, kernel.ActorAI, true},
		{api.CommandRecordPacking, kernel.ActorAI, true},
		{api.CommandPlanPacking, kernel.ActorHuman, true},
		{api.CommandGetShipment, kernel.ActorSystem, true},
	} {
		p := identity.Principal{Actor: kernel.ActorRef{Kind: c.kind, ID: "actor.1"}, Scope: scope}
		if _, err := policy.Admit(p, c.command); (err == nil) != c.ok {
			t.Errorf("%s by %s: %v", c.command, c.kind, err)
		}
	}
	for _, c := range []struct{ command, permission string }{
		{api.CommandPrepareShipment, "shipment.prepare"}, {api.CommandConfirmShipment, "shipment.confirm"},
		{api.CommandRecordPacking, "packing.record_actual"}, {api.CommandGetFulfilment, api.PermissionRead}, {api.CommandAudit, api.PermissionAudit},
	} {
		if r, err := policy.Rule(c.command); err != nil || r.Permission != c.permission {
			t.Errorf("%s: %+v %v", c.command, r, err)
		}
	}
}

// A request hash covers the business arguments only: not the idempotency key, not the expected
// version (BT11: the same request under another key is the same shipment).
func TestRequestHash(t *testing.T) {
	a := api.PrepareShipment{IdempotencyKey: "k1", ShipmentID: "shipment.1", OrderID: "order.1", Route: api.RouteSupplierDirect,
		Lines: []api.ShipmentLine{{LineID: "S1", OrderLineID: "L1", Quantity: "50", SupplierDelivery: api.DeliveryLineRef{DeliveryID: "d", LineID: "L1",
			Version: 1}, QualityReleaseID: "rel.1"}}}
	b := a
	b.IdempotencyKey = "k2"
	if requestHash(a) != requestHash(b) || len(requestHash(a)) != 64 {
		t.Fatal("the key is not part of the request")
	}
	b.Lines = []api.ShipmentLine{a.Lines[0]}
	b.Lines[0].SupplierDelivery.Version = 2
	if requestHash(a) == requestHash(b) {
		t.Fatal("the delivery version is")
	}
	c1 := api.ConfirmShipment{IdempotencyKey: "k1", ShipmentID: "shipment.1", ExpectedVersion: 1}
	c2 := c1
	c2.IdempotencyKey, c2.ExpectedVersion = "k2", 3
	if requestHash(c1) != requestHash(c2) {
		t.Fatal("the expected version is not part of the request")
	}
	for _, cmd := range []api.Command{api.PlanPacking{}, api.RecordPacking{}, api.CancelShipment{}} {
		if name, _, _ := request(cmd); name == "" {
			t.Errorf("%T", cmd)
		}
	}
}

// What the domain is told of TRD's, QLT's and PUR's answers.
func TestUpstreamFacts(t *testing.T) {
	f := trdapi.ActiveOrderFact{Binding: trdapi.OrderBinding{OrderID: "order.1", OrderRevision: 2, OrderHash: "h"}, CustomerRef: trdapi.PartyRef{
		PartyID: "customer.1", Revision: 3}, Lines: []trdapi.OrderLine{{LineID: "L1", Sku: trdapi.SkuRef{SkuID: "sku.1", SpecRevision: 1}, Quantity: "120.000000",
		UnitRef: trdapi.ConfigRef{Kind: "unit", ID: "pcs", Revision: 1}}}, Terms: trdapi.OrderTerms{Addresses: []trdapi.OrderAddress{{Role: "consignee",
		PartyID: "consignee.1", PartyRevision: 1, AddressID: "dock"}}}}
	o := orderFact(f)
	if o.Code != "" || o.Customer != 3 || o.Lines[0].Quantity.String() != "120" || o.Lines[0].UnitID != "pcs" || o.Addresses[0].Role != "consignee" {
		t.Fatalf("%+v", o)
	}
	f.Lines[0].Quantity = "lots"
	if orderFact(f).Code != kernel.CodeReferenceInvalid {
		t.Fatal("an unreadable line makes the order not intact for FUL")
	}
	r := releaseFact(qltapi.ResolvedRelease{Ref: qltapi.QualityReleaseRef{ReleaseID: "rel.1"}, State: "granted", Quantity: "50",
		Scope: qltapi.ReleaseScope{Subject: qltapi.SubjectRef{Kind: qltapi.SubjectCommitmentLine, ID: "commitment.1", LineID: "L1"}, SupplierID: "s"}})
	if r.ID != "rel.1" || r.Scope.CommitmentID != "commitment.1" || r.SubjectKind != shipment.SubjectCommitmentLine || r.Quantity.String() != "50" {
		t.Fatalf("%+v", r)
	}
	d := deliveryFact(DeliveryLine{Ref: api.DeliveryLineRef{DeliveryID: "d", LineID: "L1", Version: 2}, VerifiedQuantity: "60", CommitmentID: "c"})
	if d.Version != 2 || d.Ref != (shipment.DeliveryRef{DeliveryID: "d", LineID: "L1"}) || d.Verified.String() != "60" || d.Scope.CommitmentID != "c" {
		t.Fatalf("%+v", d)
	}
}
