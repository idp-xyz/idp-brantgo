package application

import (
	"maps"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/supplierdelivery"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The api declares exactly the codes the rules give, with the same categories.
func TestAPICodesAreTheRulesCodes(t *testing.T) {
	rules := maps.Clone(draft.Codes)
	maps.Copy(rules, allocation.Codes)
	for _, codes := range []map[string]kernel.Category{commitment.Codes, supplierdelivery.Codes} {
		for code, category := range codes {
			if c, ok := rules[code]; ok && c != category {
				t.Fatalf("%s is %s and %s", code, c, category)
			}
			rules[code] = category
		}
	}
	if !maps.Equal(rules, api.Codes()) {
		t.Fatalf("rules %v\napi %v", rules, api.Codes())
	}
}

// The tactical model's humanOnlyCommands name purchase.draft.prepare, purchase.draft.void,
// purchase.commitment.decide, purchase.commitment.revoke, purchase.commitment.issue (A47 #2:
// AI is PERMISSION_DENIED) and supplier_delivery.verify (PUR-INV-008: 经授权人工核实); the command
// catalog, SM-PUR-COMMITMENT and ES-12 let a human or an AI request a commitment, record a supplier
// confirmation and record a supplier delivery, never a system actor.
func TestHumanOnlyCommands(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{api.CommandPrepareDraft, api.CommandVoidDraft, api.CommandDecideCommitment, api.CommandRevokeCommitment,
		api.CommandIssueCommitment, api.CommandVerifySupplierDelivery} {
		r, err := policy.Rule(c)
		if err != nil || len(r.ActorKinds) != 1 || r.ActorKinds[0] != kernel.ActorHuman || r.Permission != c {
			t.Errorf("%s: %+v, %v", c, r, err)
		}
	}
	for _, c := range []string{api.CommandRequestCommitment, api.CommandRecordSupplierConfirmation, api.CommandRecordSupplierDelivery} {
		r, err := policy.Rule(c)
		if err != nil || !r.Admits(kernel.ActorHuman) || !r.Admits(kernel.ActorAI) || r.Admits(kernel.ActorSystem) || r.Permission != c {
			t.Errorf("%s: %+v, %v", c, r, err)
		}
	}
	for _, c := range []string{api.CommandGetDraft, api.CommandGetAllocations, api.CommandGetCommitment, api.CommandGetOrderCommitments,
		api.CommandGetDelivery, api.CommandGetCommitmentDeliveries} {
		if r, err := policy.Rule(c); err != nil || r.Permission != api.PermissionRead {
			t.Errorf("%s: %+v, %v", c, r, err)
		}
	}
}

// The request hash covers the business arguments, not the key or the CAS value, for every command.
func TestRequestHashIgnoresKeyAndVersion(t *testing.T) {
	a := api.VoidDraft{IdempotencyKey: "k1", DraftID: "d", ExpectedVersion: 1, Reason: "r"}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 2
	_, _, x := request(a)
	_, _, y := request(b)
	if x != y {
		t.Fatal("hash args differ")
	}
	for _, c := range []struct{ a, b api.Command }{
		{api.RequestCommitment{IdempotencyKey: "k1", CommitmentID: "c"}, api.RequestCommitment{IdempotencyKey: "k2", CommitmentID: "c"}},
		{api.DecideCommitment{IdempotencyKey: "k1", ExpectedVersion: 1, Decision: "approve"}, api.DecideCommitment{IdempotencyKey: "k2", ExpectedVersion: 2, Decision: "approve"}},
		{api.RevokeCommitment{IdempotencyKey: "k1", ExpectedVersion: 1}, api.RevokeCommitment{IdempotencyKey: "k2", ExpectedVersion: 2}},
		{api.IssueCommitment{IdempotencyKey: "k1", ExpectedVersion: 1, SentAt: "s"}, api.IssueCommitment{IdempotencyKey: "k2", ExpectedVersion: 3, SentAt: "s"}},
		{api.RecordSupplierConfirmation{IdempotencyKey: "k1", ExpectedVersion: 1}, api.RecordSupplierConfirmation{IdempotencyKey: "k2", ExpectedVersion: 4}},
		{api.RecordSupplierDelivery{IdempotencyKey: "k1", DeliveryID: "d", HandoverPlace: "CNNGB"},
			api.RecordSupplierDelivery{IdempotencyKey: "k2", DeliveryID: "d", ExpectedVersion: 2, HandoverPlace: "CNNGB"}},
		{api.VerifySupplierDelivery{IdempotencyKey: "k1", ExpectedVersion: 1, Decision: "verify"},
			api.VerifySupplierDelivery{IdempotencyKey: "k2", ExpectedVersion: 3, Decision: "verify"}},
	} {
		na, ka, x := request(c.a)
		nb, kb, y := request(c.b)
		hx, errx := action.RequestHash(x)
		hy, erry := action.RequestHash(y)
		if na == "" || na != nb || ka != "k1" || kb != "k2" || errx != nil || erry != nil || hx != hy {
			t.Errorf("%T: %s %s %s %s %s %s", c.a, na, nb, ka, kb, hx, hy)
		}
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
