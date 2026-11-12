package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/milestone"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// humanOnlyCommands names none of milestone.plan, milestone.record_actual and export_lot.register:
// each is human or AI under its own permission (role template merchandiser); a system actor runs
// none of them; the reads need ful.read.
func TestMilestoneAndLotRules(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	scope := kernel.Scope{Tenant: "tenant.1", Company: "company.A"}
	for _, command := range []string{api.CommandPlanMilestones, api.CommandRecordMilestoneActual, api.CommandRegisterExportLot} {
		for kind, ok := range map[kernel.ActorKind]bool{kernel.ActorHuman: true, kernel.ActorAI: true, kernel.ActorSystem: false} {
			p := identity.Principal{Actor: kernel.ActorRef{Kind: kind, ID: "actor.1"}, Scope: scope}
			if _, err := policy.Admit(p, command); (err == nil) != ok {
				t.Errorf("%s by %s: %v", command, kind, err)
			}
		}
		if r, err := policy.Rule(command); err != nil || r.Permission != command {
			t.Errorf("%s: %+v %v", command, r, err)
		}
	}
	for _, command := range []string{api.CommandGetMilestonePlan, api.CommandGetExportLot} {
		if r, err := policy.Rule(command); err != nil || r.Permission != api.PermissionRead {
			t.Errorf("%s: %+v %v", command, r, err)
		}
	}
}

// The request hash of the new commands covers the business arguments only, and the plan rules'
// codes are FUL's declared codes with the same categories.
func TestMilestoneAndLotRequests(t *testing.T) {
	a := api.PlanMilestones{IdempotencyKey: "k1", PlanID: "plan.1", ExpectedVersion: 0, Subject: api.MilestoneSubject{Kind: api.SubjectOrder, ID: "o"},
		Nodes: []api.MilestoneNode{{NodeID: "n", NodeType: api.NodeDelivery, PlannedOn: "2026-12-01"}}}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 4
	if requestHash(a) != requestHash(b) {
		t.Fatal("neither the key nor the expected version is part of the request")
	}
	b.Nodes = []api.MilestoneNode{{NodeID: "n", NodeType: api.NodeCustomerAcceptance, PlannedOn: "2026-12-01"}}
	if requestHash(a) == requestHash(b) {
		t.Fatal("the node type is")
	}
	for _, cmd := range []api.Command{api.RecordMilestoneActual{}, api.RegisterExportLot{}} {
		if name, _, _ := request(cmd); name == "" {
			t.Errorf("%T", cmd)
		}
	}
	for code, category := range milestone.Codes {
		if c, ok := api.Category(code); !ok || c != category {
			t.Errorf("%s: %v %v", code, c, ok)
		}
	}
}

// What FUL keeps of TRD's prices when it prepares a shipment.
func TestPriceFact(t *testing.T) {
	f := priceFact(trdapi.ActiveOrderFact{CurrencyRef: trdapi.ConfigRef{Kind: "currency", ID: "USD", Revision: 2},
		Lines: []trdapi.OrderLine{{LineID: "L1", UnitPrice: "8.0000", NetMinor: "96000", TaxMinor: "0", PayableMinor: "96000"}}})
	if f.CurrencyID != "USD" || f.CurrencyRevision != 2 || f.Lines["L1"].UnitPrice != "8.0000" || f.Lines["L1"].NetMinor != "96000" {
		t.Fatalf("%+v", f)
	}
}
