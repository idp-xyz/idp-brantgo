package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/approvalgrant"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/aigrant"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

type recordingAuditor struct{ entries []string }

func (a *recordingAuditor) AuditRejected(_ context.Context, command, _, code string) error {
	a.entries = append(a.entries, command+" "+code)
	return nil
}

func agentContext(t *testing.T, kind kernel.ActorKind) context.Context {
	t.Helper()
	ctx, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: kernel.ActorRef{Kind: kind, ID: "actor.1"},
		Scope: kernel.Scope{Tenant: "tenant.1", Company: "company.A"}})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func q01(t *testing.T) *capability.Registry {
	t.Helper()
	reg, err := capability.Q01Registry(capability.Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// Before any delegation is looked up, the gateway refuses non-AI actors, unregistered and human-only
// tools and arguments that are not the api command's closed structure, and audits each refusal.
func TestDecodeRefusesBeforeTheDelegation(t *testing.T) {
	audit := &recordingAuditor{}
	g := Gateway{Registry: q01(t), Auditor: audit}
	b := Binding{TaskID: "task.1", DelegationID: "del.1"}
	cases := []struct {
		kind kernel.ActorKind
		call model.ToolCall
		code string
		path string
	}{
		{kernel.ActorHuman, model.ToolCall{Name: "rfq.save_draft", Arguments: json.RawMessage(`{"RfqID":"r"}`)}, kernel.CodePermissionDenied, "/"},
		{kernel.ActorAI, model.ToolCall{Name: "quote.activate", Arguments: json.RawMessage(`{}`)}, kernel.CodePermissionDenied, "/command"},
		{kernel.ActorAI, model.ToolCall{Name: "rm -rf /", Arguments: json.RawMessage(`{}`)}, kernel.CodeUnsupportedContract, "/command"},
		{kernel.ActorAI, model.ToolCall{Name: "rfq.save_draft", Arguments: json.RawMessage(`{"RfqID":"r","Scope":"x"}`)}, kernel.CodeSchemaInvalid, "/args/Scope"},
		{kernel.ActorAI, model.ToolCall{Name: "rfq.save_draft", Arguments: json.RawMessage(`{"RfqID":"r","IdempotencyKey":"k"}`)}, kernel.CodeSchemaInvalid, "/args/IdempotencyKey"},
		{kernel.ActorAI, model.ToolCall{Name: "rfq.save_draft", Arguments: json.RawMessage(`{"RfqID":"r","Lines":[{"LineID":""}]}`)}, kernel.CodeSchemaInvalid, "/args/Lines/0/LineID"},
	}
	for _, c := range cases {
		_, err := g.Decode(agentContext(t, c.kind), b, c.call)
		rej, ok := kernel.AsRejection(err)
		if !ok || rej.Code != c.code || rej.Fields[0].Path != c.path {
			t.Errorf("%s: %v, want %s at %s", c.call.Name, err, c.code, c.path)
		}
	}
	want := []string{"rfq.save_draft PERMISSION_DENIED", "quote.activate PERMISSION_DENIED", "invalid UNSUPPORTED_CONTRACT",
		"rfq.save_draft SCHEMA_INVALID", "rfq.save_draft SCHEMA_INVALID", "rfq.save_draft SCHEMA_INVALID"}
	if len(audit.entries) != len(want) {
		t.Fatalf("audited %v", audit.entries)
	}
	for i := range want {
		if audit.entries[i] != want[i] {
			t.Fatalf("audited %v", audit.entries)
		}
	}
	if _, err := g.Decode(agentContext(t, kernel.ActorAI), Binding{DelegationID: "del.1"}, cases[0].call); !errors.Is(err, ErrBinding) {
		t.Fatalf("binding %v", err)
	}
	if _, err := g.Decode(context.Background(), b, cases[0].call); err == nil {
		t.Fatal("no principal")
	}
}

// The tools offered to the model are the capabilities the delegation allows, with their schemas.
func TestToolsFollowTheDelegation(t *testing.T) {
	g := Gateway{Registry: q01(t)}
	tools, err := g.Tools(agentContext(t, kernel.ActorAI), policy.Delegation{Autonomy: capability.Draft,
		Capabilities: []string{"rfq.save_draft", "order.prepare", "source.register", "quote.activate"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "rfq.save_draft" || tools[1].Name != "source.register" || tools[0].Description == "" ||
		!json.Valid(tools[0].Parameters) {
		t.Fatalf("tools %+v", tools)
	}
	if err = (model.Request{Messages: []model.Message{{Role: model.RoleUser}}, Tools: tools}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// grantProbe stands for a command that needs an approval grant, such as order.prepare.
type grantProbe struct {
	IdempotencyKey string
	ProbeID        string
}

type grantProbeReceipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// S4 unit F1 (D-7): the proof of an approval grant's use reaches the owning context only from an
// admission (Admit after approvalgrant.Service.Use, covered by the composed tests); an Admitted
// built elsewhere — naming a grant or not — runs without one, so its target refuses an AI actor.
func TestOnlyAnAdmissionCarriesTheGrantProof(t *testing.T) {
	var seen []bool
	c, err := capability.Define(capability.Spec{Command: "probe.prepare", Context: "prb", Contract: "brant.probe/0.1.0", Version: 1,
		SideEffect: capability.Submit, Risk: capability.RiskHigh, Approval: capability.ApprovalGrant, Summary: "prepare a probe"},
		func(p grantProbe) capability.ObjectRef { return capability.ObjectRef{Type: "prb.probe", ID: p.ProbeID} },
		func(ctx context.Context, p grantProbe) (grantProbeReceipt, error) {
			_, ok := aigrant.From(ctx)
			seen = append(seen, ok)
			return grantProbeReceipt{ActionID: "act-1", ObjectID: p.ProbeID, RowVersion: 1, Status: "draft"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	call := Call{Capability: c, Command: grantProbe{ProbeID: "probe.1"}, Object: capability.ObjectRef{Type: "prb.probe", ID: "probe.1"}}
	forged := &approvalgrant.Grant{GrantID: "grant.1", Command: "probe.prepare", ObjectType: "prb.probe", ObjectID: "probe.1",
		Status: approvalgrant.StatusUsed, UsedKey: "k1", UsedByID: "actor.1"}
	for _, a := range []Admitted{{Call: call, Key: "k1"}, {Call: call, Key: "k1", Grant: forged}} {
		if _, err = a.Run(agentContext(t, kernel.ActorAI)); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0] || seen[1] {
		t.Fatalf("a hand-built admission carried a proof: %v", seen)
	}
}

type refusingChecks struct {
	err   error
	asked int
}

func (r *refusingChecks) CheckPrepareOrder(context.Context, trdapi.PrepareOrder) error {
	r.asked++
	return r.err
}

func (r *refusingChecks) CheckRequestOrderReview(context.Context, trdapi.RequestOrderReview) error {
	r.asked++
	return r.err
}

const prepareArgs = `{"OrderID":"order.1","QuoteID":"quote.1","BusinessRevision":2,"BusinessHash":"` +
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `","AcceptanceID":"acc.1"}`

// S4 G7b: Judge asks the owning context before a person is asked and returns its rejection as the
// call's, audited in brant_ai like any refusal of the gateway; a request it accepts and a command
// that needs no judgement pass, and nothing is audited for them.
func TestJudgeIsTheOwningContextsRefusal(t *testing.T) {
	checks := &refusingChecks{err: kernel.Reject(kernel.CodeInvalidKnownValue, "/args/notify_party_ref", "")}
	reg, err := capability.Q01Registry(capability.Q01Targets{TRDOrderChecks: checks})
	if err != nil {
		t.Fatal(err)
	}
	audit := &recordingAuditor{}
	g := Gateway{Registry: reg, Auditor: audit}
	ctx := agentContext(t, kernel.ActorAI)
	c, err := reg.Lookup("tenant.1", trdapi.CommandPrepareOrder, "/command")
	if err != nil {
		t.Fatal(err)
	}
	cmd := trdapi.PrepareOrder{OrderID: "order.1", QuoteID: "quote.1", BusinessRevision: 2, BusinessHash: strings.Repeat("a", 64), AcceptanceID: "acc.1"}
	hash, err := c.RequestHash(cmd)
	if err != nil {
		t.Fatal(err)
	}
	prepare := Call{Capability: c, Command: cmd, RequestHash: hash, Object: c.Object(cmd)}
	err = g.Judge(ctx, prepare)
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != kernel.CodeInvalidKnownValue || rej.Fields[0].Path != "/args/notify_party_ref" || checks.asked != 1 {
		t.Fatalf("judge %v, asked %d", err, checks.asked)
	}
	if len(audit.entries) != 1 || audit.entries[0] != "order.prepare INVALID_KNOWN_VALUE" {
		t.Fatalf("audited %v", audit.entries)
	}
	checks.err = nil
	if err = g.Judge(ctx, prepare); err != nil || checks.asked != 2 || len(audit.entries) != 1 {
		t.Fatalf("an accepted request: %v, asked %d, audited %v", err, checks.asked, audit.entries)
	}
	plain, err := reg.Lookup("tenant.1", "source.register", "/command")
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Judge(ctx, Call{Capability: plain, Command: plain.WithKey(evdapi.RegisterSource{SourceID: "s"}, "")}); err != nil || checks.asked != 2 {
		t.Fatalf("a command without a judgement: %v, asked %d", err, checks.asked)
	}
}

// S4 G7b: Reload gives back the call a stored proposal was only if it is that request still: arguments
// that decode strictly, hash and object the proposal recorded. A stored request that changed under the
// proposal (a schema that moved, a row altered) is ErrProposalChanged and sends nothing; the actor,
// the command and the binding are checked as for a model's call.
func TestReloadRefusesAProposalThatIsNotTheRequestProposed(t *testing.T) {
	reg, err := capability.Q01Registry(capability.Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := reg.Lookup("tenant.1", trdapi.CommandPrepareOrder, "/command")
	if err != nil {
		t.Fatal(err)
	}
	cmd := trdapi.PrepareOrder{OrderID: "order.1", QuoteID: "quote.1", BusinessRevision: 2, BusinessHash: strings.Repeat("a", 64), AcceptanceID: "acc.1"}
	hash, _ := c.RequestHash(cmd)
	object := c.Object(cmd)
	audit := &recordingAuditor{}
	g := Gateway{Registry: reg, Auditor: audit}
	ctx := agentContext(t, kernel.ActorAI)
	b := Binding{TaskID: "task.1", DelegationID: "del.1"}
	args := json.RawMessage(prepareArgs)

	if _, err = g.Reload(ctx, b, trdapi.CommandPrepareOrder, args, strings.Repeat("b", 64), object); !errors.Is(err, ErrProposalChanged) {
		t.Errorf("another hash: %v", err)
	}
	if _, err = g.Reload(ctx, b, trdapi.CommandPrepareOrder, args, hash, capability.ObjectRef{Type: object.Type, ID: object.ID, Version: object.Version + 1}); !errors.Is(err, ErrProposalChanged) {
		t.Errorf("another object version: %v", err)
	}
	if _, err = g.Reload(ctx, b, trdapi.CommandPrepareOrder, args, hash, capability.ObjectRef{Type: object.Type, ID: "quote.2", Version: object.Version}); !errors.Is(err, ErrProposalChanged) {
		t.Errorf("another object: %v", err)
	}
	for name, stored := range map[string]string{"an unknown member": `{"OrderID":"order.1","Scope":"x"}`, "the key": `{"OrderID":"order.1","IdempotencyKey":"k"}`,
		"not json": `{`, "an empty string": `{"OrderID":""}`} {
		_, err = g.Reload(ctx, b, trdapi.CommandPrepareOrder, json.RawMessage(stored), hash, object)
		if !errors.Is(err, ErrProposalChanged) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err = g.Reload(agentContext(t, kernel.ActorHuman), b, trdapi.CommandPrepareOrder, args, hash, object); err == nil {
		t.Error("a person's context")
	} else if rej, ok := kernel.AsRejection(err); !ok || rej.Code != kernel.CodePermissionDenied {
		t.Errorf("a person's context: %v", err)
	}
	if _, err = g.Reload(ctx, b, "quote.activate", args, hash, object); err == nil {
		t.Error("a human-only command")
	} else if rej, ok := kernel.AsRejection(err); !ok || rej.Code != kernel.CodePermissionDenied {
		t.Errorf("a human-only command: %v", err)
	}
	if _, err = g.Reload(ctx, Binding{DelegationID: "del.1"}, trdapi.CommandPrepareOrder, args, hash, object); !errors.Is(err, ErrBinding) {
		t.Errorf("binding: %v", err)
	}
	if _, err = g.Reload(context.Background(), b, trdapi.CommandPrepareOrder, args, hash, object); err == nil {
		t.Error("no principal")
	}
}

func TestAuditNameAndEmptyStrings(t *testing.T) {
	for in, want := range map[string]string{"rfq.save_draft": "rfq.save_draft", "DROP TABLE": "invalid", "": "invalid",
		"a." + string(make([]byte, 70)): "invalid"} {
		if got := auditName(in); got != want {
			t.Errorf("auditName(%q) = %s", in, got)
		}
	}
	for doc, want := range map[string]string{`{"a":"x","b":[{"c":""}]}`: "/b/0/c", `{"a/b":""}`: "/a~1b", `{"a":1}`: "", `{`: ""} {
		if got := emptyString([]byte(doc)); got != want {
			t.Errorf("emptyString(%s) = %q", doc, got)
		}
	}
}
