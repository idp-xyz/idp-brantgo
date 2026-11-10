package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/profile"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The trading profile decides with the codes package api declares: same code, same category; its
// reasons for a stale revision are the api's.
func TestDomainCodesAreTheContractsCodes(t *testing.T) {
	for code, category := range profile.Categories {
		if got, ok := api.Category(code); !ok || got != category {
			t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, ok, category)
		}
	}
	for use, stale := range map[profile.Use]string{profile.UseSuperseded: api.StaleSuperseded, profile.UsePartyChanged: api.StalePartyChanged,
		profile.UseContactChanged: api.StaleContactChanged, profile.UsePartyUnavailable: api.StalePartyUnavailable} {
		if string(use) != stale {
			t.Errorf("%q is %q in the api", use, stale)
		}
	}
}

// 05 stage humanOnlyCommands: verifying a service role and publishing a trading profile are human
// only; saving a trading profile and revising a contact are open to agents with the grant.
func TestRulesKeepVerificationAndPublicationHumanOnly(t *testing.T) {
	humanOnly := map[string]bool{api.CommandConfirmParty: true, api.CommandDeactivateParty: true, api.CommandConfirmContact: true,
		api.CommandVerifyServiceRole: true, api.CommandPublishTradingProfile: true, api.CommandDecideScreening: true, api.CommandBlockParty: true}
	seen := map[string]bool{}
	for _, r := range Rules() {
		seen[r.Command] = true
		human := len(r.ActorKinds) == 1 && r.ActorKinds[0] == kernel.ActorHuman
		if human != humanOnly[r.Command] {
			t.Errorf("%s: human only %v", r.Command, human)
		}
	}
	for _, c := range []string{api.CommandReviseContact, api.CommandSaveTradingProfile, api.CommandVerifyServiceRole, api.CommandPublishTradingProfile,
		CommandGetTradingProfile} {
		if !seen[c] {
			t.Errorf("%s has no rule", c)
		}
	}
}
