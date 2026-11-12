package application

import (
	"maps"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The api declares exactly the codes the rules give, with the same categories.
func TestAPICodesAreTheRulesCodes(t *testing.T) {
	rules := maps.Clone(draft.Codes)
	maps.Copy(rules, allocation.Codes)
	if !maps.Equal(rules, api.Codes()) {
		t.Fatalf("rules %v\napi %v", rules, api.Codes())
	}
}

// The tactical model's humanOnlyCommands name purchase.draft.prepare and purchase.draft.void.
func TestHumanOnlyCommands(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{api.CommandPrepareDraft, api.CommandVoidDraft} {
		r, err := policy.Rule(c)
		if err != nil || len(r.ActorKinds) != 1 || r.ActorKinds[0] != kernel.ActorHuman || r.Permission != c {
			t.Errorf("%s: %+v, %v", c, r, err)
		}
	}
	for _, c := range []string{api.CommandGetDraft, api.CommandGetAllocations} {
		if r, err := policy.Rule(c); err != nil || r.Permission != api.PermissionRead {
			t.Errorf("%s: %+v, %v", c, r, err)
		}
	}
}

// The request hash covers the business arguments, not the key or the CAS value.
func TestRequestHashIgnoresKeyAndVersion(t *testing.T) {
	a := api.VoidDraft{IdempotencyKey: "k1", DraftID: "d", ExpectedVersion: 1, Reason: "r"}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 2
	_, _, x := request(a)
	_, _, y := request(b)
	if x != y {
		t.Fatal("hash args differ")
	}
	if name, _, _ := request(nil); name != "" {
		t.Fatal("unknown command")
	}
}

// OrderGuard v1's verdicts as PUR passes them on.
func TestOrderCheck(t *testing.T) {
	for _, c := range []struct {
		v    OrderVerdict
		want string
	}{
		{OrderVerdict{Known: true, Status: "active"}, ""},
		{OrderVerdict{}, kernel.CodeReferenceInvalid},
		{OrderVerdict{Known: true, Code: OrderNotActive}, api.CodeOrderNotActive},
		{OrderVerdict{Known: true, Code: OrderApprovalRevoked}, api.CodeOrderApprovalRevoked},
		{OrderVerdict{Known: true, Code: OrderStale}, api.CodeReferenceStale},
		{OrderVerdict{Known: true, Code: "SURPRISE"}, kernel.CodeReferenceInvalid},
	} {
		err := orderCheck(c.v, "/args/order_id")
		r, _ := kernel.AsRejection(err)
		if (c.want == "" && err != nil) || (c.want != "" && (r == nil || r.Code != c.want)) {
			t.Errorf("%+v: %v", c.v, err)
		}
	}
}
