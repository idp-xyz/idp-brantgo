package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/policy"
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
