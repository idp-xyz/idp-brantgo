package policy

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/capability"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func codeOf(err error) (string, string) {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code, r.Fields[0].Path
	}
	if err != nil {
		return "ERROR", err.Error()
	}
	return "OK", ""
}

func TestGrantDelegationValidate(t *testing.T) {
	good := GrantDelegation{IdempotencyKey: "k1", DelegationID: "del.1", AgentID: "agent.1", TaskID: "task.1",
		Capabilities: []string{"rfq.save_draft"}, Autonomy: "draft", ExpiresAt: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	many := make([]string, MaxCapabilities+1)
	for i := range many {
		many[i] = "c.x" + strings.Repeat("y", i)
	}
	for path, change := range map[string]func(*GrantDelegation){
		"/idempotency_key":      func(c *GrantDelegation) { c.IdempotencyKey = "" },
		"/args/id":              func(c *GrantDelegation) { c.DelegationID = "bad id" },
		"/args/agent_id":        func(c *GrantDelegation) { c.AgentID = "" },
		"/args/task_id":         func(c *GrantDelegation) { c.TaskID = "-" },
		"/args/capabilities":    func(c *GrantDelegation) { c.Capabilities = many },
		"/args/capabilities/0":  func(c *GrantDelegation) { c.Capabilities = []string{"bad name"} },
		"/args/autonomy":        func(c *GrantDelegation) { c.Autonomy = "autonomous" },
		"/args/expires_at":      func(c *GrantDelegation) { c.ExpiresAt = time.Time{} },
		"/args/capabilities/1 ": func(c *GrantDelegation) { c.Capabilities = []string{"a.b", "a.b"} },
	} {
		c := good
		change(&c)
		code, got := codeOf(c.Validate())
		if code != kernel.CodeSchemaInvalid || got != strings.TrimSpace(path) {
			t.Errorf("%s: %s at %s", path, code, got)
		}
	}
	rv := RevokeDelegation{IdempotencyKey: "k1", DelegationID: "del.1", ExpectedVersion: 1, Reason: "停止"}
	if err := rv.Validate(); err != nil {
		t.Fatal(err)
	}
	for path, change := range map[string]func(*RevokeDelegation){
		"/args/expected_version": func(c *RevokeDelegation) { c.ExpectedVersion = 0 },
		"/args/reason":           func(c *RevokeDelegation) { c.Reason = strings.Repeat("理", MaxReasonCharacters+1) },
		"/args/id":               func(c *RevokeDelegation) { c.DelegationID = "" },
	} {
		c := rv
		change(&c)
		if code, got := codeOf(c.Validate()); code != kernel.CodeSchemaInvalid || got != path {
			t.Errorf("%s: %s at %s", path, code, got)
		}
	}
	if Reason("") || Reason("\xff") || !Reason("ok") {
		t.Fatal("reason rules")
	}
}

func capabilityOf(t *testing.T, command string, class capability.SideEffect) capability.Capability {
	t.Helper()
	approval := capability.ApprovalNone
	if class == capability.Submit {
		approval = capability.ApprovalGrant
	}
	c, err := capability.Define(capability.Spec{Command: command, Context: "trd", Contract: trdapi.Contract, Version: 1, SideEffect: class,
		Risk: capability.RiskLow, Approval: approval, Summary: "probe"},
		func(trdapi.Revise) capability.ObjectRef { return capability.ObjectRef{} },
		func(context.Context, trdapi.Revise) (trdapi.Receipt, error) { return trdapi.Receipt{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A delegation allows what it lists within its autonomy, until it expires; its agent and readers
// whose data scope covers the initiator see it.
func TestDelegationAllowsExpiresAndIsSeen(t *testing.T) {
	at := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	d := Delegation{InitiatorID: "clerk.1", AgentID: "agent.1", Autonomy: capability.Draft, Capabilities: []string{"quote.revise", "order.prepare"},
		ExpiresAt: at, OwnerTeamID: "trade"}
	if !d.Allows(capabilityOf(t, "quote.revise", capability.Draft)) || d.Allows(capabilityOf(t, "order.prepare", capability.Submit)) ||
		d.Allows(capabilityOf(t, "quote.save_draft", capability.Draft)) {
		t.Fatal("allows")
	}
	if !d.Expired(at) || d.Expired(at.Add(-time.Microsecond)) {
		t.Fatal("expiry")
	}
	for who, want := range map[authorization.Decision]bool{
		{Actor: kernel.ActorRef{ID: "agent.1"}, DataScope: authorization.DataScopeOwn}:                   true,
		{Actor: kernel.ActorRef{ID: "clerk.1"}, DataScope: authorization.DataScopeOwn}:                   true,
		{Actor: kernel.ActorRef{ID: "clerk.2"}, DataScope: authorization.DataScopeOwn}:                   false,
		{Actor: kernel.ActorRef{ID: "clerk.2"}, DataScope: authorization.DataScopeTeam, Team: "trade"}:   true,
		{Actor: kernel.ActorRef{ID: "clerk.2"}, DataScope: authorization.DataScopeCompany}:               true,
		{Actor: kernel.ActorRef{ID: "agent.2"}, DataScope: authorization.DataScopeTeam, Team: "finance"}: false,
	} {
		if d.SeenBy(who) != want {
			t.Errorf("%+v sees: %v", who, !want)
		}
	}
}

// The delegation commands are human-only (platformCommands AI; humanOnlyCommands); the read admits
// every actor kind holding ai.read.
func TestRules(t *testing.T) {
	p, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{CommandGrant, CommandRevoke} {
		r, err := p.Rule(c)
		if err != nil || r.Permission != c || r.Admits(kernel.ActorAI) || r.Admits(kernel.ActorSystem) || !capability.IsHumanOnly(c) {
			t.Errorf("%s: %+v %v", c, r, err)
		}
	}
	if r, _ := p.Rule(CommandGet); r.Permission != PermissionRead || !r.Admits(kernel.ActorAI) {
		t.Errorf("read rule %+v", r)
	}
}
