package capability

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// tacticalHumanOnly reads humanOnlyCommands from the archive's tactical model.
func tacticalHumanOnly(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "05_TACTICAL_DDD", "02_IDP_Brant_Tactical_DDD_Candidate_V0.1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		HumanOnly []string `json:"humanOnlyCommands"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.HumanOnly
}

// The Go list is the tactical model's humanOnlyCommands, entry for entry (UL-INV-17: the tactical
// model is the single source).
func TestHumanOnlyListIsTheTacticalModels(t *testing.T) {
	want := tacticalHumanOnly(t)
	slices.Sort(want)
	if len(want) < 100 || !slices.Equal(HumanOnlyCommands(), want) {
		t.Fatalf("humanOnly differs from the tactical model (%d entries there, %d here)", len(want), len(humanOnly))
	}
	for _, c := range []string{"ai.delegation.grant", "ai.delegation.revoke", "ai.approval_grant.issue", "ai.approval_grant.revoke",
		"quote.activate", "rfq.confirm_scope", "task.take_control"} {
		if !IsHumanOnly(c) {
			t.Errorf("%s must be human-only", c)
		}
	}
}

type probe struct {
	IdempotencyKey  string
	ID              string
	ExpectedVersion int64
	Lines           []string
}

func probeCapability(t *testing.T, spec Spec) Capability {
	t.Helper()
	c, err := Define(spec, func(p probe) ObjectRef { return ObjectRef{Type: "x.probe", ID: p.ID, Version: p.ExpectedVersion} },
		func(_ context.Context, p probe) (trdapi.Receipt, error) {
			return trdapi.Receipt{ActionID: "act-1", ObjectID: p.ID, RowVersion: 1, Status: "draft"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func draftSpec(command string) Spec {
	return Spec{Command: command, Context: "xx", Contract: "brant.x/0.1.0", Version: 1, SideEffect: Draft, Risk: RiskLow,
		Approval: ApprovalNone, Summary: "probe"}
}

// AI-INV-001, X-INV-002: no command of the full humanOnlyCommands list can be defined or registered
// as a capability, and a registry answers each of them PERMISSION_DENIED (quote.activate), whatever
// else it holds.
func TestHumanOnlyCommandsAreNeverCapabilities(t *testing.T) {
	ok := probeCapability(t, draftSpec("probe.save_draft"))
	reg, err := NewRegistry(ok)
	if err != nil {
		t.Fatal(err)
	}
	all := tacticalHumanOnly(t)
	for _, command := range all {
		spec := draftSpec(command)
		if _, err := Define(spec, func(probe) ObjectRef { return ObjectRef{} },
			func(context.Context, probe) (trdapi.Receipt, error) { return trdapi.Receipt{}, nil }); !errors.Is(err, ErrInvalid) {
			t.Errorf("Define(%s) = %v, want ErrInvalid", command, err)
		}
		forged := ok
		forged.Command = command // a capability built for another command and renamed
		if _, err := NewRegistry(forged); !errors.Is(err, ErrInvalid) {
			t.Errorf("NewRegistry(%s) = %v, want ErrInvalid", command, err)
		}
		if _, err := reg.Lookup("tenant.1", command, "/command"); code(err) != kernel.CodePermissionDenied {
			t.Errorf("Lookup(%s) = %v, want PERMISSION_DENIED", command, err)
		}
	}
	if len(all) != len(humanOnly) {
		t.Fatalf("checked %d human-only commands", len(all))
	}
}

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

// Default deny (AI-INV-001): an empty or missing registry and an unregistered command answer
// UNSUPPORTED_CONTRACT; a capability limited to other tenants is not there for this one.
func TestDefaultDeny(t *testing.T) {
	empty, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	var none *Registry
	for _, r := range []*Registry{empty, none} {
		for _, command := range []string{"rfq.save_draft", "sql.exec", "ai.tool.add", ""} {
			if _, err := r.Lookup("tenant.1", command, "/command"); code(err) != kernel.CodeUnsupportedContract {
				t.Errorf("Lookup(%q) = %v", command, err)
			}
		}
	}
	spec := draftSpec("probe.save_draft")
	spec.Tenants = []kernel.TenantID{"tenant.1"}
	reg, err := NewRegistry(probeCapability(t, spec))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Lookup("tenant.1", "probe.save_draft", "/command"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Lookup("tenant.2", "probe.save_draft", "/command"); code(err) != kernel.CodeUnsupportedContract {
		t.Fatalf("other tenant: %v", err)
	}
	if _, err := NewRegistry(probeCapability(t, draftSpec("probe.save_draft")), probeCapability(t, draftSpec("probe.save_draft"))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := NewRegistry(Capability{Spec: draftSpec("probe.save_draft")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a capability not built with Define: %v", err)
	}
}

// AI-INV-004: an api type with a field only the platform may set (scope, actor, task, control
// epoch, delegation, grant) is not a capability, nor one with floats or open types, nor one
// without the idempotency key the execution entry sets.
func TestInputTypesMustBeClosedAndCarryNoPlatformFields(t *testing.T) {
	type withTenant struct {
		IdempotencyKey string
		TenantID       string
	}
	type nested struct{ GrantRef string }
	type withNestedGrant struct {
		IdempotencyKey string
		Lines          []nested
	}
	type withEpoch struct {
		IdempotencyKey string
		Control        struct{ ControlEpoch int64 }
	}
	type withTask struct {
		IdempotencyKey string
		Task           string `json:"task_id"`
	}
	type withFloat struct {
		IdempotencyKey string
		Price          float64
	}
	type withAny struct {
		IdempotencyKey string
		Extra          map[string]any
	}
	type withoutKey struct{ ID string }
	spec := draftSpec("probe.save_draft")
	cases := map[string]error{
		"tenant":       define[withTenant](spec),
		"nested grant": define[withNestedGrant](spec),
		"epoch":        define[withEpoch](spec),
		"tagged task":  define[withTask](spec),
		"float":        define[withFloat](spec),
		"open map":     define[withAny](spec),
		"no key":       define[withoutKey](spec),
	}
	for name, err := range cases {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

func define[C any](spec Spec) error {
	_, err := Define(spec, func(C) ObjectRef { return ObjectRef{} },
		func(context.Context, C) (trdapi.Receipt, error) { return trdapi.Receipt{}, nil })
	return err
}

func TestSpecChecks(t *testing.T) {
	good := draftSpec("probe.save_draft")
	bad := map[string]func(*Spec){
		"command":        func(s *Spec) { s.Command = "Probe" },
		"context":        func(s *Spec) { s.Context = "X" },
		"contract":       func(s *Spec) { s.Contract = "" },
		"version":        func(s *Spec) { s.Version = 0 },
		"class":          func(s *Spec) { s.SideEffect = "write" },
		"risk":           func(s *Spec) { s.Risk = "none" },
		"approval":       func(s *Spec) { s.Approval = "maybe" },
		"submit no gate": func(s *Spec) { s.SideEffect = Submit },
		"summary":        func(s *Spec) { s.Summary = " " },
		"tenant":         func(s *Spec) { s.Tenants = []kernel.TenantID{"bad tenant"} },
	}
	for name, change := range bad {
		s := good
		change(&s)
		if err := define[probe](s); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	submit := good
	submit.SideEffect, submit.Approval = Submit, ApprovalGrant
	if err := define[probe](submit); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		s, autonomy SideEffect
		within      bool
	}{{Read, Read, true}, {Draft, Read, false}, {Draft, Draft, true}, {Submit, Draft, false}, {Submit, Submit, true},
		{Read, Submit, true}, {"x", Submit, false}, {Read, "x", false}} {
		if c.s.Within(c.autonomy) != c.within {
			t.Errorf("%s within %s = %v", c.s, c.autonomy, !c.within)
		}
	}
}

// The request hash leaves out the idempotency key and the expected versions, as the target
// ledgers do, and covers every business field.
func TestRequestHashAndKey(t *testing.T) {
	c := probeCapability(t, draftSpec("probe.save_draft"))
	a := probe{IdempotencyKey: "k1", ID: "p.1", ExpectedVersion: 3, Lines: []string{"a"}}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 7
	ha, err := c.RequestHash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := c.RequestHash(b)
	want, _ := action.RequestHash(probe{ID: "p.1", Lines: []string{"a"}})
	if ha != hb || ha != want {
		t.Fatalf("hashes %s %s, want %s", ha, hb, want)
	}
	b.Lines = []string{"b"}
	if hc, _ := c.RequestHash(b); hc == ha {
		t.Fatal("a business change must change the hash")
	}
	keyed := c.WithKey(probe{ID: "p.1"}, "key.1")
	if keyed.(probe).IdempotencyKey != "key.1" || c.Key(keyed) != "key.1" {
		t.Fatalf("keyed %+v", keyed)
	}
	if o := c.Object(a); o != (ObjectRef{Type: "x.probe", ID: "p.1", Version: 3}) {
		t.Fatalf("object %+v", o)
	}
	res, err := c.Run(context.Background(), keyed)
	if err != nil || res.ActionID != "act-1" || res.ObjectID != "p.1" || res.RowVersion != 1 || res.Status != "draft" {
		t.Fatalf("run %+v, %v", res, err)
	}
	if _, ok := res.Detail.(trdapi.Receipt); !ok {
		t.Fatalf("detail %T", res.Detail)
	}
	if _, err := c.Run(context.Background(), "not a probe"); err == nil {
		t.Fatal("a command of another type must not run")
	}
}

// D-7 and the role template ai_q01_agent are one list: every Q01 capability is a permission of
// the template, every write permission of the template is a Q01 capability, and only order.prepare
// and order.review.request need an approval grant.
func TestQ01IsTheAgentTemplate(t *testing.T) {
	caps, err := Q01(Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := NewRegistry(caps...)
	if err != nil {
		t.Fatal(err)
	}
	tpl, ok := authorization.TemplateByCode("ai_q01_agent")
	if !ok {
		t.Fatal("no template ai_q01_agent")
	}
	var writes []string
	for _, p := range tpl.Permissions {
		if strings.HasSuffix(p, ".read") || strings.HasPrefix(p, "action.") {
			continue
		}
		writes = append(writes, p)
	}
	if got := reg.Commands(); !slices.Equal(got, writes) {
		t.Fatalf("Q01 capabilities %v, template writes %v", got, writes)
	}
	var granted []string
	for _, c := range reg.Capabilities() {
		if c.Approval == ApprovalGrant {
			granted = append(granted, c.Command)
		}
		if !slices.Contains(tpl.Permissions, c.Permission()) || IsHumanOnly(c.Command) {
			t.Errorf("%s: permission %s", c.Command, c.Permission())
		}
	}
	if !slices.Equal(granted, []string{"order.prepare", "order.review.request"}) {
		t.Fatalf("grant-gated %v", granted)
	}
	for _, p := range tpl.Permissions {
		if IsHumanOnly(p) {
			t.Errorf("the agent template holds human-only %s", p)
		}
	}
}

// A catalogue built without its contexts refuses to run instead of panicking.
func TestQ01WithoutTargets(t *testing.T) {
	reg, err := Q01Registry(Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range reg.Capabilities() {
		cmd := c.WithKey(reflectZero(c), "k1")
		if _, err := c.Run(context.Background(), cmd); !errors.Is(err, errNoTarget) {
			t.Errorf("%s: %v", c.Command, err)
		}
	}
}

// The tool schema of each Q01 capability is closed at every level and leaves out the idempotency
// key; its top-level members are the api command's fields.
func TestQ01Schemas(t *testing.T) {
	reg, err := Q01Registry(Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range reg.Capabilities() {
		raw, err := c.Schema()
		if err != nil {
			t.Fatalf("%s: %v", c.Command, err)
		}
		var s struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Additional *bool                      `json:"additionalProperties"`
		}
		if err = json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		if s.Type != "object" || s.Additional == nil || *s.Additional || len(s.Properties) == 0 {
			t.Errorf("%s: schema %s", c.Command, raw)
		}
		if _, ok := s.Properties[KeyField]; ok {
			t.Errorf("%s: the schema offers the idempotency key", c.Command)
		}
		for i := range c.Input().NumField() {
			f := c.Input().Field(i)
			if _, ok := s.Properties[f.Name]; f.Name != KeyField && !ok {
				t.Errorf("%s: the schema lacks %s", c.Command, f.Name)
			}
		}
		if strings.Contains(string(raw), `"additionalProperties":true`) {
			t.Errorf("%s: an open object", c.Command)
		}
	}
}

func reflectZero(c Capability) any { return reflect.Zero(c.Input()).Interface() }
