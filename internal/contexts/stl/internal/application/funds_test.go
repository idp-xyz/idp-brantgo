package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// humanOnlyCommands name fund.record, fund.resolve_difference, application.apply and
// application.unapply (D-16, D-17); order_account.open is a person's or the system's, with the
// permission payment_plan_review.open (SM-STL-PAYMENT-PLAN-REVIEW); the reads need stl.read.
func TestActorKindsOfTheFundCommands(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for c, want := range map[string]struct {
		permission string
		kinds      []kernel.ActorKind
	}{
		api.CommandRecordFund:        {api.CommandRecordFund, []kernel.ActorKind{kernel.ActorHuman}},
		api.CommandResolveDifference: {api.CommandResolveDifference, []kernel.ActorKind{kernel.ActorHuman}},
		api.CommandApply:             {api.CommandApply, []kernel.ActorKind{kernel.ActorHuman}},
		api.CommandUnapply:           {api.CommandUnapply, []kernel.ActorKind{kernel.ActorHuman}},
		api.CommandOpenOrderAccount:  {api.PermissionOpenOrderAccount, []kernel.ActorKind{kernel.ActorHuman, kernel.ActorSystem}},
		api.CommandGetFundRecord:     {api.PermissionRead, []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}},
		api.CommandGetOrderAccount:   {api.PermissionRead, []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}},
		api.CommandGetApplication:    {api.PermissionRead, []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}},
	} {
		r, err := policy.Rule(c)
		if err != nil || r.Permission != want.permission || len(r.ActorKinds) != len(want.kinds) {
			t.Errorf("%s: %+v, %v", c, r, err)
			continue
		}
		for _, k := range want.kinds {
			if !r.Admits(k) {
				t.Errorf("%s does not admit %s", c, k)
			}
		}
	}
	for _, c := range []api.Command{api.RecordFund{}, api.ResolveFundDifference{}, api.OpenOrderAccount{}, api.Apply{}, api.Unapply{}} {
		if name, _, _ := request(c); name == "" {
			t.Errorf("%T has no name", c)
		}
	}
	a := api.Unapply{IdempotencyKey: "k1", ApplicationID: "a", ExpectedVersion: 1, Reason: "r"}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 2
	_, _, x := request(a)
	_, _, y := request(b)
	if x != y {
		t.Fatal("the unapply hash covers the key or the version")
	}
}
