package proposal

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func valid() New {
	return New{ProposalID: "agp.1", TaskID: "task.1", Delegation: policy.Delegation{DelegationID: "del.1", TaskID: "task.1", AgentID: "agent.1",
		InitiatorID: "clerk.1"}, Command: "order.prepare", RequestHash: strings.Repeat("a", 64), ObjectType: "trd.quote", ObjectID: "quote.1",
		ObjectVersion: 2, Arguments: json.RawMessage(`{"OrderID":"order.1"}`)}
}

// A proposal is stored only as a request of the task's own delegation: identifiers, a 64-hex hash, an
// object, one JSON object of arguments within the table's length and without the idempotency key
// (the key is the send's, never the model's).
func TestNewIsCheckedBeforeItIsStored(t *testing.T) {
	if err := valid().check(); err != nil {
		t.Fatalf("a valid request: %v", err)
	}
	for name, change := range map[string]func(*New){
		"no proposal ID":         func(n *New) { n.ProposalID = "" },
		"a proposal ID with a /": func(n *New) { n.ProposalID = "agp/1" },
		"no task":                func(n *New) { n.TaskID = "" },
		"another task's":         func(n *New) { n.Delegation.TaskID = "task.2" },
		"no delegation":          func(n *New) { n.Delegation.DelegationID = "" },
		"no command":             func(n *New) { n.Command = "" },
		"a short hash":           func(n *New) { n.RequestHash = "abc" },
		"no object type":         func(n *New) { n.ObjectType = "" },
		"a bad object ID":        func(n *New) { n.ObjectID = "-x" },
		"a negative version":     func(n *New) { n.ObjectVersion = -1 },
		"no arguments":           func(n *New) { n.Arguments = nil },
		"arguments not an object": func(n *New) {
			n.Arguments = json.RawMessage(`["x"]`)
		},
		"arguments null": func(n *New) { n.Arguments = json.RawMessage(`null`) },
		"a key":          func(n *New) { n.Arguments = json.RawMessage(`{"OrderID":"o","IdempotencyKey":"k"}`) },
	} {
		n := valid()
		change(&n)
		if err := n.check(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	long := valid()
	long.Arguments = json.RawMessage(`{"OrderID":"` + strings.Repeat("x", MaxArguments) + `"}`)
	if err := long.check(); !errors.Is(err, ErrTooLong) || errors.Is(err, ErrInvalid) {
		t.Errorf("too long: %v", err)
	}
	edge := valid()
	edge.Arguments = json.RawMessage(`{"OrderID":"` + strings.Repeat("x", MaxArguments-len(`{"OrderID":""}`)) + `"}`)
	if err := edge.check(); err != nil {
		t.Errorf("exactly the limit: %v", err)
	}
}

// An outcome is a sent one with its action, or a refused or void one with its code.
func TestOutcomeIsCheckedBeforeItIsRecorded(t *testing.T) {
	good := []Outcome{{Status: StatusSent, ActionID: "act.1"}, {Status: StatusRefused, Code: "CONVERSION_SLOT_CONFLICT", Path: "/args/quote_id"},
		{Status: StatusVoid, Code: "GRANT_EXPIRED"}}
	for _, o := range good {
		if err := o.check(); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
	for _, o := range []Outcome{{}, {Status: StatusSent}, {Status: StatusSent, ActionID: "act.1", Code: "X"}, {Status: StatusRefused}, {Status: StatusVoid},
		{Status: StatusPending, Code: "X"}, {Status: StatusSending, ActionID: "act.1"}, {Status: "done", Code: "X"}} {
		if err := o.check(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", o, err)
		}
	}
}

// Open is pending or being sent; a sending proposal is stale from the end of its lease.
func TestOpenAndStale(t *testing.T) {
	at := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	for status, open := range map[string]bool{StatusPending: true, StatusSending: true, StatusSent: false, StatusRefused: false, StatusVoid: false} {
		if got := (Proposal{Status: status}).Open(); got != open {
			t.Errorf("%s open %v", status, got)
		}
	}
	p := Proposal{Status: StatusSending, ClaimedAt: at}
	for _, c := range []struct {
		at    time.Time
		stale bool
	}{{at, false}, {at.Add(ClaimLease - time.Microsecond), false}, {at.Add(ClaimLease), true}, {at.Add(time.Hour), true}} {
		if got := p.Stale(c.at); got != c.stale {
			t.Errorf("stale at +%v: %v", c.at.Sub(at), got)
		}
	}
	if (Proposal{Status: StatusPending, ClaimedAt: at}).Stale(at.Add(time.Hour)) || (Proposal{Status: StatusSent, ClaimedAt: at}).Stale(at.Add(time.Hour)) {
		t.Error("only a sending proposal goes stale")
	}
}

// The proposal is seen by the agent that proposed it and by whoever sees the delegation's initiator.
func TestSeenBy(t *testing.T) {
	p := Proposal{AgentID: "agent.1", InitiatorID: "clerk.1", OwnerTeamID: "team.1"}
	as := func(id string, scope authorization.DataScope, team string) authorization.Decision {
		return authorization.Decision{Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id}, DataScope: scope, Team: team}
	}
	for name, c := range map[string]struct {
		d    authorization.Decision
		sees bool
	}{
		"the agent":                 {authorization.Decision{Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}, DataScope: authorization.DataScopeOwn}, true},
		"another agent":             {authorization.Decision{Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.2"}, DataScope: authorization.DataScopeOwn}, false},
		"the initiator":             {as("clerk.1", authorization.DataScopeOwn, ""), true},
		"another person, own scope": {as("clerk.2", authorization.DataScopeOwn, "team.1"), false},
		"the initiator's team":      {as("lead.1", authorization.DataScopeTeam, "team.1"), true},
		"another team":              {as("lead.2", authorization.DataScopeTeam, "team.2"), false},
		"the company":               {as("boss.1", authorization.DataScopeCompany, ""), true},
	} {
		if got := p.SeenBy(c.d); got != c.sees {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestRulesAreTheReadOfAiRead(t *testing.T) {
	rules := Rules()
	if len(rules) != 1 || rules[0].Command != CommandGet || rules[0].Permission != policy.PermissionRead || len(rules[0].ActorKinds) != 3 {
		t.Fatalf("rules %+v", rules)
	}
}
