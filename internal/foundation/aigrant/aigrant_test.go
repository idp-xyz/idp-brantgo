package aigrant

import (
	"context"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// S4 unit F1 (D-7, AI-INV-002): an AI actor's call of a command that needs an approval grant passes
// only with the proof of a grant used for exactly that call; people need none.
func TestCheckAdmitsOnlyTheCallTheGrantWasUsedFor(t *testing.T) {
	scope := kernel.Scope{Tenant: "tenant.1", Company: "company.A"}
	agent := kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.q01"}
	call := Call{Scope: scope, Actor: agent, Command: "order.prepare", RequestHash: strings.Repeat("a", 64), Key: "intent.1",
		Object: Object{Type: "trd.quote", ID: "quote.1", Version: 1}}
	proof := Proof{GrantID: "grant.1", Scope: scope, Agent: agent.ID, Command: call.Command, RequestHash: call.RequestHash,
		Object: call.Object, Key: call.Key}
	ctx := context.Background()

	if err := Check(With(ctx, proof), call); err != nil {
		t.Fatalf("the call the grant was used for: %v", err)
	}
	if got, ok := From(With(ctx, proof)); !ok || got != proof {
		t.Fatalf("From %+v %v", got, ok)
	}
	if _, ok := From(ctx); ok {
		t.Fatal("a bare context carries a proof")
	}
	if code, path := rejection(t, Check(ctx, call)); code != kernel.CodePermissionDenied || path != "/" {
		t.Fatalf("no proof: %s at %s", code, path)
	}
	for _, kind := range []kernel.ActorKind{kernel.ActorHuman, kernel.ActorSystem} {
		c := call
		c.Actor.Kind = kind
		if err := Check(ctx, c); err != nil {
			t.Fatalf("a %s needs no proof: %v", kind, err)
		}
	}
	for name, change := range map[string]func(p *Proof){
		"no grant":               func(p *Proof) { p.GrantID = "" },
		"another tenant":         func(p *Proof) { p.Scope.Tenant = "tenant.2" },
		"another company":        func(p *Proof) { p.Scope.Company = "company.B" },
		"another agent":          func(p *Proof) { p.Agent = "agent.other" },
		"another command":        func(p *Proof) { p.Command = "order.review.request" },
		"another request hash":   func(p *Proof) { p.RequestHash = strings.Repeat("b", 64) },
		"another object type":    func(p *Proof) { p.Object.Type = "trd.order" },
		"another object":         func(p *Proof) { p.Object.ID = "quote.2" },
		"another object version": func(p *Proof) { p.Object.Version = 2 },
		"another key":            func(p *Proof) { p.Key = "intent.2" },
	} {
		p := proof
		change(&p)
		code, path := rejection(t, Check(With(ctx, p), call))
		if code != CodeGrantInvalid || path != "/grant" {
			t.Errorf("%s: %s at %s, want GRANT_INVALID at /grant", name, code, path)
		}
	}
	rej, _ := kernel.AsRejection(Check(With(ctx, Proof{}), call))
	if rej == nil || rej.Category != kernel.CategoryForbidden {
		t.Fatalf("GRANT_INVALID is forbidden: %+v", rej)
	}
}

func rejection(t *testing.T, err error) (code, path string) {
	t.Helper()
	rej, ok := kernel.AsRejection(err)
	if !ok {
		t.Fatalf("not a rejection: %v", err)
	}
	return rej.Code, rej.Fields[0].Path
}
