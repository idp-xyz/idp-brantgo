package application

import (
	"maps"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/budget"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/costing"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/delivery"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/measurement"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/rules"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The domain decides with the codes package api declares: same code, same category. The domain
// may not import api (package rules), so the comparison lives here.
func TestRuleCodesAreTheContractsCodes(t *testing.T) {
	domain := maps.Clone(rules.Categories)
	maps.Copy(domain, delivery.Categories)
	maps.Copy(domain, budget.Categories)
	maps.Copy(domain, costing.Categories)
	maps.Copy(domain, measurement.Categories)
	for code, category := range domain {
		if got, ok := api.Category(code); !ok || got != category {
			t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, ok, category)
		}
	}
}

// Verification, withdrawal, every measurement command, the freeze and the source attestation are
// human only; drafts are open to AI agents with the grant (05 stage humanOnlyCommands plus Python
// HUMAN_ACTIONS).
func TestRulesKeepVerificationHumanOnly(t *testing.T) {
	humanOnly := []string{api.CommandVerifyContext, api.CommandWithdrawContext, api.CommandRecordMeasurement,
		api.CommandSupersedeMeasurement, api.CommandWithdrawMeasurement, api.CommandFreezeSnapshot, api.CommandAttestSource}
	seen := map[string]bool{}
	for _, r := range Rules() {
		seen[r.Command] = true
		human := len(r.ActorKinds) == 1 && r.ActorKinds[0] == kernel.ActorHuman
		want := false
		for _, c := range humanOnly {
			want = want || c == r.Command
		}
		if human != want {
			t.Errorf("%s: human only %v, want %v", r.Command, human, want)
		}
	}
	for _, c := range []string{api.CommandSaveContext, api.CommandReviseContext, api.CommandDiscardContextDraft, api.CommandVerifyContext,
		api.CommandWithdrawContext, api.CommandRecordMeasurement, api.CommandSupersedeMeasurement, api.CommandWithdrawMeasurement,
		CommandGetContext, CommandGetMeasurement, CommandAudit} {
		if !seen[c] {
			t.Errorf("no rule for %s", c)
		}
	}
}

// Every command hashes its business arguments without the idempotency key and the expected
// version, so a replay with another CAS value still matches.
func TestRequestHashIgnoresKeyAndExpectedVersion(t *testing.T) {
	for _, pair := range [][2]api.Command{
		{api.SaveContext{IdempotencyKey: "k1", ContextID: "c", ExpectedVersion: 0}, api.SaveContext{IdempotencyKey: "k2", ContextID: "c", ExpectedVersion: 3}},
		{api.VerifyContext{IdempotencyKey: "k1", ContextID: "c", ExpectedVersion: 1, WorkingRevision: 1},
			api.VerifyContext{IdempotencyKey: "k2", ContextID: "c", ExpectedVersion: 2, WorkingRevision: 1}},
		{api.SupersedeMeasurement{IdempotencyKey: "k1", MeasurementID: "m", ExpectedVersion: 1, ActiveRevision: 1},
			api.SupersedeMeasurement{IdempotencyKey: "k2", MeasurementID: "m", ExpectedVersion: 5, ActiveRevision: 1}},
	} {
		n1, k1, a1 := request(pair[0])
		n2, k2, a2 := request(pair[1])
		if n1 != n2 || k1 != "k1" || k2 != "k2" {
			t.Fatalf("%s %s %s %s", n1, n2, k1, k2)
		}
		if h1, h2 := hash(t, a1), hash(t, a2); h1 != h2 {
			t.Errorf("%s: %s != %s", n1, h1, h2)
		}
	}
	_, _, a := request(api.VerifyContext{ContextID: "c", WorkingRevision: 2})
	_, _, b := request(api.VerifyContext{ContextID: "c", WorkingRevision: 1})
	if hash(t, a) == hash(t, b) {
		t.Fatal("business arguments change the hash")
	}
	if name, _, _ := request(nil); name != "" {
		t.Fatal("an unknown command has no name")
	}
}

func hash(t *testing.T, args any) string {
	t.Helper()
	h, err := action.RequestHash(args)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
