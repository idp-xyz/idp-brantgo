package approvalgrant

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func pathOf(err error) string {
	if r, ok := kernel.AsRejection(err); ok && r.Code == kernel.CodeSchemaInvalid {
		return r.Fields[0].Path
	}
	return "not SCHEMA_INVALID"
}

func TestIssueAndRevokeValidate(t *testing.T) {
	good := IssueGrant{IdempotencyKey: "k1", GrantID: "grant.1", DelegationID: "del.1", Command: "order.prepare",
		RequestHash: strings.Repeat("ab", 32), ObjectID: "quote.1", ObjectVersion: 2, ExpiresAt: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for path, change := range map[string]func(*IssueGrant){
		"/idempotency_key":     func(c *IssueGrant) { c.IdempotencyKey = "" },
		"/args/id":             func(c *IssueGrant) { c.GrantID = "" },
		"/args/delegation_id":  func(c *IssueGrant) { c.DelegationID = "a b" },
		"/args/command":        func(c *IssueGrant) { c.Command = "" },
		"/args/request_hash":   func(c *IssueGrant) { c.RequestHash = strings.Repeat("AB", 32) },
		"/args/object_id":      func(c *IssueGrant) { c.ObjectID = "" },
		"/args/object_version": func(c *IssueGrant) { c.ObjectVersion = -1 },
		"/args/expires_at":     func(c *IssueGrant) { c.ExpiresAt = time.Time{} },
	} {
		c := good
		change(&c)
		if got := pathOf(c.Validate()); got != path {
			t.Errorf("%s: %s", path, got)
		}
	}
	if hash(strings.Repeat("a", 63)) || hash(strings.Repeat("g", 64)) || !hash(strings.Repeat("0f", 32)) {
		t.Fatal("hash rule")
	}
	rv := RevokeGrant{IdempotencyKey: "k1", GrantID: "grant.1", ExpectedVersion: 1, Reason: "withdrawn"}
	if err := rv.Validate(); err != nil {
		t.Fatal(err)
	}
	for path, change := range map[string]func(*RevokeGrant){
		"/args/id":               func(c *RevokeGrant) { c.GrantID = "" },
		"/args/expected_version": func(c *RevokeGrant) { c.ExpectedVersion = 0 },
		"/args/reason":           func(c *RevokeGrant) { c.Reason = "" },
	} {
		c := rv
		change(&c)
		if got := pathOf(c.Validate()); got != path {
			t.Errorf("%s: %s", path, got)
		}
	}
}

// The grant commands are human-only (platformCommands AI; humanOnlyCommands); its agent and readers
// whose data scope covers the issuer see a grant.
func TestRulesAndVisibility(t *testing.T) {
	p, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{CommandIssue, CommandRevoke} {
		r, err := p.Rule(c)
		if err != nil || r.Admits(kernel.ActorAI) || r.Admits(kernel.ActorSystem) || !capability.IsHumanOnly(c) {
			t.Errorf("%s: %+v %v", c, r, err)
		}
	}
	g := Grant{IssuerID: "clerk.1", AgentID: "agent.1"}
	if !g.SeenBy(authorization.Decision{Actor: kernel.ActorRef{ID: "agent.1"}}) || !g.SeenBy(authorization.Decision{Actor: kernel.ActorRef{ID: "clerk.1"}}) ||
		g.SeenBy(authorization.Decision{Actor: kernel.ActorRef{ID: "clerk.2"}, DataScope: authorization.DataScopeOwn}) {
		t.Fatal("visibility")
	}
}
