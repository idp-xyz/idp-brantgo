package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/scenario"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The scenario domain decides with the codes package api declares: same code, same category.
func TestScenarioCodesAreTheContractsCodes(t *testing.T) {
	for code, category := range scenario.Categories {
		if got, ok := api.Category(code); !ok || got != category {
			t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, ok, category)
		}
	}
	for _, pair := range [][2]string{{scenario.Verified, api.VerificationVerified}, {scenario.Provisional, api.VerificationProvisional},
		{scenario.Comparable, api.ComparisonComparable}, {scenario.NotComparable, api.ComparisonNotComparable},
		{scenario.FromCST, api.InputFromCST}, {scenario.FromINQ, api.InputFromINQ}, {scenario.FromCAT, api.InputFromCAT},
		{scenario.FromPOL, api.InputFromPOL}, {scenario.FromSUP, api.InputFromSUP}, {scenario.UseInput, api.UseInput},
		{scenario.UseAnchor, api.UseAnchor}, {scenario.UseProvenance, api.UseProvenance}, {string(scenario.Draft), api.ScenarioDraft},
		{string(scenario.Frozen), api.ScenarioFrozen}} {
		if pair[0] != pair[1] {
			t.Errorf("domain %q, api %q", pair[0], pair[1])
		}
	}
}

// Saving and evaluating a scenario are open to humans and AI agents with the grant (command
// catalog: 人工或 AI); the reads need cst.read.
func TestScenarioRules(t *testing.T) {
	rules := map[string]bool{}
	for _, r := range Rules() {
		human := len(r.ActorKinds) == 1 && r.ActorKinds[0] == kernel.ActorHuman
		switch r.Command {
		case api.CommandSaveScenario, api.CommandEvaluateScenario:
			if human || r.Permission != r.Command {
				t.Errorf("%s: %+v", r.Command, r)
			}
		case CommandGetScenario, CommandGetEvaluation:
			if r.Permission != PermissionRead {
				t.Errorf("%s: %+v", r.Command, r)
			}
		}
		rules[r.Command] = true
	}
	for _, c := range []string{api.CommandSaveScenario, api.CommandEvaluateScenario, CommandGetScenario, CommandGetEvaluation} {
		if !rules[c] {
			t.Errorf("no rule for %s", c)
		}
	}
}

// The scenario commands hash their business arguments without the idempotency key and the expected
// version, as the other CST commands do.
func TestScenarioRequestHash(t *testing.T) {
	for _, pair := range [][2]api.Command{
		{api.SaveScenario{IdempotencyKey: "k1", ScenarioID: "s", ExpectedVersion: 0}, api.SaveScenario{IdempotencyKey: "k2", ScenarioID: "s", ExpectedVersion: 4}},
		{api.EvaluateScenario{IdempotencyKey: "k1", ScenarioID: "s", ExpectedVersion: 1}, api.EvaluateScenario{IdempotencyKey: "k2", ScenarioID: "s", ExpectedVersion: 2}},
	} {
		n1, k1, a1 := request(pair[0])
		n2, k2, a2 := request(pair[1])
		if n1 != n2 || n1 == "" || k1 != "k1" || k2 != "k2" || hash(t, a1) != hash(t, a2) {
			t.Errorf("%s: %s %s %s %s", n1, n2, k1, k2, hash(t, a1))
		}
	}
	_, _, a := request(api.SaveScenario{ScenarioID: "s", Data: api.ScenarioData{SelectionReason: "a"}})
	_, _, b := request(api.SaveScenario{ScenarioID: "s", Data: api.ScenarioData{SelectionReason: "b"}})
	if hash(t, a) == hash(t, b) {
		t.Fatal("business arguments change the hash")
	}
}
