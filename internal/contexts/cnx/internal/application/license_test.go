package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	el "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/exportlicense"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// humanOnlyCommands name export_license.register and .release; allocate admits humans and AI
// agents (ES-12 step 3), never a system actor; each command needs its own name as permission (role
// template documentation); the licence reads need cnx.read.
func TestLicenseRules(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for command, kinds := range map[string][]kernel.ActorKind{
		api.CommandRegisterLicense: {kernel.ActorHuman},
		api.CommandAllocateLicense: {kernel.ActorHuman, kernel.ActorAI},
		api.CommandReleaseLicense:  {kernel.ActorHuman},
	} {
		r, err := policy.Rule(command)
		if err != nil || r.Permission != command || len(r.ActorKinds) != len(kinds) {
			t.Fatalf("%s: %+v %v", command, r, err)
		}
		for _, k := range kinds {
			if !r.Admits(k) {
				t.Errorf("%s does not admit %s", command, k)
			}
		}
		if r.Admits(kernel.ActorSystem) {
			t.Errorf("%s admits the system", command)
		}
	}
	for _, command := range []string{api.CommandGetLicense, api.CommandLicenses, api.CommandShipmentLicense} {
		if r, err := policy.Rule(command); err != nil || r.Permission != api.PermissionRead || !r.Admits(kernel.ActorAI) {
			t.Errorf("%s: %+v %v", command, r, err)
		}
	}
}

// The domain's codes, targets, kinds and states are the contract's; the lines hash the domain
// records is the one the published guard's Check computes.
func TestLicenseDomainIsTheContract(t *testing.T) {
	for code, category := range el.Categories {
		if got, ok := api.Category(code); !ok || got != category {
			t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, ok, category)
		}
	}
	for domain, contract := range map[string]string{el.TargetShipment: api.TargetShipment, el.TargetExportLot: api.TargetExportLot,
		el.Amended: api.ExternalAmended, el.Withdrawn: api.ExternalWithdrawn, el.Stated: api.ResultStated, el.Registered: api.LicenseStatusRegistered,
		el.Valid: api.LicenseValid, el.Expired: api.LicenseExpired, el.NotYetValid: api.LicenseNotYetValid, el.Allocated: api.AllocationAllocated,
		el.Released: api.AllocationReleased} {
		if domain != contract {
			t.Errorf("domain %q, api %q", domain, contract)
		}
	}
	lines := []el.Line{{LineID: "l2", SkuID: "sku.2", SpecRevision: 3, Quantity: kernel.MustDecimal("2.5"), Unit: "PCS"},
		{LineID: "l1", SkuID: "sku.1", SpecRevision: 1, Quantity: kernel.MustDecimal("600"), Unit: "PCS", CommodityCode: "6109100021"}}
	published, err := api.LinesHash([]api.ShipmentLine{{LineID: "l1", SkuID: "sku.1", SpecRevision: 1, Quantity: "600", Unit: "PCS"},
		{LineID: "l2", SkuID: "sku.2", SpecRevision: 3, Quantity: "2.500", Unit: "PCS"}})
	if err != nil || published != el.LinesHash(lines) {
		t.Fatalf("api %s, domain %s, %v", published, el.LinesHash(lines), err)
	}
}

// The request hash covers the business arguments, not the key or the CAS value.
func TestLicenseRequestHashIgnoresKeyAndVersion(t *testing.T) {
	r := &api.ExternalResult{Kind: api.ExternalWithdrawn}
	for _, pair := range [][2]api.Command{
		{api.RegisterLicense{IdempotencyKey: "k1", LicenseNumber: "n"}, api.RegisterLicense{IdempotencyKey: "k2", LicenseNumber: "n"}},
		{api.AllocateLicense{IdempotencyKey: "k1", LicenseID: "l", AllocationID: "a"}, api.AllocateLicense{IdempotencyKey: "k2", LicenseID: "l", AllocationID: "a"}},
		{api.ReleaseLicense{IdempotencyKey: "k1", LicenseID: "l", ExpectedVersion: 2, ExternalResult: r},
			api.ReleaseLicense{IdempotencyKey: "k2", LicenseID: "l", ExpectedVersion: 5, ExternalResult: r}},
	} {
		n1, k1, a := request(pair[0])
		n2, k2, b := request(pair[1])
		ha, errA := action.RequestHash(a)
		hb, errB := action.RequestHash(b)
		if n1 == "" || n1 != n2 || k1 == k2 || errA != nil || errB != nil || ha != hb {
			t.Errorf("%T: %v %v", pair[0], a, b)
		}
	}
	_, _, a := request(api.AllocateLicense{IdempotencyKey: "k1", LicenseID: "l", AllocationID: "a", Target: api.ExportTarget{Kind: "shipment", ID: "s1"}})
	_, _, b := request(api.AllocateLicense{IdempotencyKey: "k1", LicenseID: "l", AllocationID: "a", Target: api.ExportTarget{Kind: "shipment", ID: "s2"}})
	ha, _ := action.RequestHash(a)
	hb, _ := action.RequestHash(b)
	if ha == hb {
		t.Fatal("the request hash covers the target")
	}
}
