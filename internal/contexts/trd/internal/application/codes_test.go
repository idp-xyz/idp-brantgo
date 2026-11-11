package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/document"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The codes the domain packages decide are the codes package api declares, with the same
// categories, so a refusal reaches transports with the catalog's status.
func TestDomainCodesAreDeclaredByTheAPI(t *testing.T) {
	for name, m := range map[string]map[string]kernel.Category{"quote": quote.Categories, "calc": calc.Categories, "document": document.Categories} {
		for code, category := range m {
			if got, ok := api.Category(code); !ok || got != category {
				t.Errorf("%s code %s: api says %v (declared %v), domain %v", name, code, got, ok, category)
			}
		}
	}
}

// The tactical model's humanOnlyCommands that QuoteRoot implements are human only; the rest admit
// what the command catalog says, and every command has a rule.
func TestRulesFollowTheCatalog(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]kernel.ActorKind{
		api.CommandSaveDraft:      {kernel.ActorHuman, kernel.ActorAI},
		api.CommandRevise:         {kernel.ActorHuman, kernel.ActorAI},
		api.CommandDiscardDraft:   {kernel.ActorHuman, kernel.ActorAI},
		api.CommandFreeze:         {kernel.ActorHuman},
		api.CommandUpdateMetadata: {kernel.ActorHuman, kernel.ActorAI},
		api.CommandBindControl:    {kernel.ActorHuman, kernel.ActorSystem},
		api.CommandStopControl:    {kernel.ActorHuman},
		api.CommandReleaseControl: {kernel.ActorHuman},
		api.CommandSubmitReview:   {kernel.ActorHuman},
		api.CommandDecideReview:   {kernel.ActorHuman},
		api.CommandRevokeApproval: {kernel.ActorHuman},
		CommandGetQuote:           {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
		CommandCheckApproval:      {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
	}
	for command, kinds := range want {
		r, err := policy.Rule(command)
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		for _, k := range []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem} {
			admitted := false
			for _, w := range kinds {
				admitted = admitted || w == k
			}
			if r.Admits(k) != admitted {
				t.Errorf("%s admits %s: %v", command, k, r.Admits(k))
			}
		}
		if command != CommandGetQuote && command != CommandCheckApproval && r.Permission != command {
			t.Errorf("%s needs permission %s", command, r.Permission)
		}
	}
	if r, _ := policy.Rule(CommandCheckApproval); r.Permission != PermissionRead {
		t.Errorf("%s needs %s", CommandCheckApproval, r.Permission)
	}
	// The domain's permission names are the api's command and read names.
	if quote.PermissionReview != api.CommandDecideReview || quote.PermissionMarginException != api.PermissionMarginException ||
		quote.PermissionRead != PermissionRead {
		t.Error("permission names differ")
	}
}
