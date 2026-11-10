package authorization

import (
	"errors"
	"testing"

	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var quotePolicy = []Rule{
	{Command: "quote.save_draft", Permission: "quote.write", ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
	HumanOnly("quote.activate", "quote.activate"),
}

func principal(kind kernel.ActorKind, id string) identity.Principal {
	return identity.Principal{Actor: kernel.ActorRef{Kind: kind, ID: id}, Scope: kernel.Scope{Tenant: "tenant.demo", Company: "company.demo"}}
}

func TestHumanOnlyCommandsRefuseAIAndSystemActors(t *testing.T) {
	p, err := NewPolicy(quotePolicy...)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []kernel.ActorKind{kernel.ActorAI, kernel.ActorSystem} {
		if _, err := p.Admit(principal(kind, "agent.quote"), "quote.activate"); !errors.Is(err, ErrActorKindDenied) {
			t.Errorf("%s on quote.activate: %v", kind, err)
		}
	}
	if r, err := p.Admit(principal(kernel.ActorHuman, "alice"), "quote.activate"); err != nil || r.Permission != "quote.activate" {
		t.Errorf("human on quote.activate: %+v, %v", r, err)
	}
	if _, err := p.Admit(principal(kernel.ActorAI, "agent.quote"), "quote.save_draft"); err != nil {
		t.Errorf("AI on quote.save_draft: %v", err)
	}
	if _, err := p.Admit(principal(kernel.ActorHuman, "alice"), "quote.unknown"); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("unknown command: %v", err)
	}
	if _, err := p.Admit(identity.Principal{}, "quote.activate"); !errors.Is(err, identity.ErrInvalidPrincipal) {
		t.Errorf("empty principal: %v", err)
	}
}

func TestPolicyRejectsInvalidRules(t *testing.T) {
	for name, rules := range map[string][]Rule{
		"duplicate command": {HumanOnly("a", "p"), HumanOnly("a", "q")},
		"no actor kinds":    {{Command: "a", Permission: "p"}},
		"unknown kind":      {{Command: "a", Permission: "p", ActorKinds: []kernel.ActorKind{"robot"}}},
		"bad permission":    {HumanOnly("a", "")},
		"empty command":     {HumanOnly("", "p")},
	} {
		if _, err := NewPolicy(rules...); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Python Core._access: owner, team or company data scope (BC-D02).
func TestDataScopeDecidesWhichRecordsAnActorSees(t *testing.T) {
	alice := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "alice"}
	for _, c := range []struct {
		d                Decision
		owner, ownerTeam string
		want             bool
	}{
		{Decision{Actor: alice, DataScope: DataScopeOwn}, "alice", "", true},
		{Decision{Actor: alice, DataScope: DataScopeOwn, Team: "t1"}, "bob", "t1", false},
		{Decision{Actor: alice, DataScope: DataScopeTeam, Team: "t1"}, "bob", "t1", true},
		{Decision{Actor: alice, DataScope: DataScopeTeam, Team: "t1"}, "bob", "t2", false},
		{Decision{Actor: alice, DataScope: DataScopeTeam}, "bob", "", false},
		{Decision{Actor: alice, DataScope: DataScopeCompany}, "bob", "", true},
		{Decision{Actor: alice, DataScope: DataScopeCompany}, "", "", true},
		{Decision{Actor: kernel.ActorRef{}, DataScope: DataScopeOwn}, "", "", false},
	} {
		if got := c.d.Sees(c.owner, c.ownerTeam); got != c.want {
			t.Errorf("%+v sees %s/%s = %v, want %v", c.d, c.owner, c.ownerTeam, got, c.want)
		}
	}
}
