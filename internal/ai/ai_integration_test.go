//go:build integration

package ai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai"
	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/approvalgrant"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/ai/gateway"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The AI layer against the migrated PostgreSQL 16 database (brant_ai, brant_auth), with probe
// capabilities whose "context" records the calls it receives. Each test runs in a fresh scope.

var start = time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type probeLine struct {
	Sku      string
	Quantity string
}

// saveProbe stands for a draft command such as rfq.save_draft.
type saveProbe struct {
	IdempotencyKey  string
	ProbeID         string
	ExpectedVersion int64
	Lines           []probeLine
	Note            string
}

// prepareProbe stands for a command that needs a grant, such as order.prepare.
type prepareProbe struct {
	IdempotencyKey   string
	OrderID          string
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
}

type probeReceipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// call is what the probe context received.
type call struct {
	command string
	actor   kernel.ActorRef
	scope   kernel.Scope
	key     string
	cmd     any
}

type recorder struct {
	mu    sync.Mutex
	calls []call
}

func (r *recorder) record(ctx context.Context, command, key string, cmd any) error {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{command: command, actor: p.Actor, scope: p.Scope, key: key, cmd: cmd})
	return nil
}

func (r *recorder) all() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

const (
	cmdSave     = "probe.save_draft"
	cmdAnnotate = "probe.annotate"
	cmdPrepare  = "probe.prepare"
)

func probeRegistry(t *testing.T, rec *recorder) *capability.Registry {
	t.Helper()
	save, err := capability.Define(capability.Spec{Command: cmdSave, Context: "prb", Contract: "brant.probe/0.1.0", Version: 1,
		SideEffect: capability.Draft, Risk: capability.RiskLow, Approval: capability.ApprovalNone, Summary: "save a probe draft"},
		func(c saveProbe) capability.ObjectRef {
			return capability.ObjectRef{Type: "prb.probe", ID: c.ProbeID, Version: c.ExpectedVersion}
		},
		func(ctx context.Context, c saveProbe) (probeReceipt, error) {
			return probeReceipt{ActionID: "act-" + c.IdempotencyKey, ObjectID: c.ProbeID, RowVersion: c.ExpectedVersion + 1, Status: "draft"},
				rec.record(ctx, cmdSave, c.IdempotencyKey, c)
		})
	if err != nil {
		t.Fatal(err)
	}
	annotate, err := capability.Define(capability.Spec{Command: cmdAnnotate, Context: "prb", Contract: "brant.probe/0.1.0", Version: 1,
		SideEffect: capability.Draft, Risk: capability.RiskLow, Approval: capability.ApprovalNone, Summary: "annotate a probe"},
		func(c saveProbe) capability.ObjectRef { return capability.ObjectRef{Type: "prb.probe", ID: c.ProbeID} },
		func(ctx context.Context, c saveProbe) (probeReceipt, error) {
			return probeReceipt{ActionID: "act-" + c.IdempotencyKey, ObjectID: c.ProbeID, RowVersion: 1, Status: "draft"},
				rec.record(ctx, cmdAnnotate, c.IdempotencyKey, c)
		})
	if err != nil {
		t.Fatal(err)
	}
	prepare, err := capability.Define(capability.Spec{Command: cmdPrepare, Context: "prb", Contract: "brant.probe/0.1.0", Version: 1,
		SideEffect: capability.Submit, Risk: capability.RiskHigh, Approval: capability.ApprovalGrant, Summary: "prepare a probe order"},
		func(c prepareProbe) capability.ObjectRef {
			return capability.ObjectRef{Type: "prb.quote", ID: c.QuoteID, Version: c.BusinessRevision}
		},
		func(ctx context.Context, c prepareProbe) (probeReceipt, error) {
			return probeReceipt{ActionID: "act-" + c.IdempotencyKey, ObjectID: c.OrderID, RowVersion: 1, Status: "draft"},
				rec.record(ctx, cmdPrepare, c.IdempotencyKey, c)
		})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := capability.NewRegistry(save, annotate, prepare)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

var (
	clerk    = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk.1"}
	clerk2   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk.2"}
	approver = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "approver.1"} // company data scope
	viewer   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "viewer.1"}
	auditor  = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "auditor.1"}
	agent    = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}
	agent2   = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.2"}
	system   = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "system.tsk"}
)

var (
	humanPermissions = []string{policy.CommandGrant, policy.CommandRevoke, approvalgrant.CommandIssue, approvalgrant.CommandRevoke,
		policy.PermissionRead, cmdSave, cmdAnnotate, cmdPrepare}
	agentPermissions = []string{cmdSave, cmdAnnotate, cmdPrepare, policy.PermissionRead}
)

type fixture struct {
	t      *testing.T
	db     *bentopg.DB
	layer  *ai.Layer
	clock  *clock
	scope  kernel.Scope
	rec    *recorder
	serial atomic.Int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, db: db, clock: &clock{now: start}, rec: &recorder{},
		scope: kernel.Scope{Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: "company.A"}}
	if f.layer, err = ai.New(db, probeRegistry(t, f.rec), ai.WithClock(f.clock)); err != nil {
		t.Fatal(err)
	}
	f.authority(func(ctx context.Context, exec bentopg.Executor, g authorization.Grants) error {
		if err := g.Provision(ctx, exec, f.scope); err != nil {
			return err
		}
		for _, a := range []struct {
			actor kernel.ActorRef
			perms []string
		}{{clerk, humanPermissions}, {clerk2, humanPermissions}, {viewer, []string{policy.PermissionRead}},
			{auditor, []string{ai.PermissionAudit, policy.PermissionRead}}, {agent, agentPermissions}, {agent2, agentPermissions},
			{system, agentPermissions}, {approver, []string{approvalgrant.CommandIssue, policy.PermissionRead, cmdPrepare}}} {
			if _, err := g.Put(ctx, exec, f.scope, a.actor, a.perms, start); err != nil {
				return err
			}
		}
		for _, id := range []string{auditor.ID, approver.ID} {
			if _, err := g.SetDataScope(ctx, exec, f.scope, id, authorization.DataScopeCompany, "", start); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

func (f *fixture) authority(fn func(context.Context, bentopg.Executor, authorization.Grants) error) {
	f.t.Helper()
	if err := policy.WithinScope(context.Background(), f.db, f.scope, func(ctx context.Context, exec bentopg.Executor) error {
		return fn(ctx, exec, authorization.Grants{})
	}); err != nil {
		f.t.Fatal(err)
	}
}

// as returns the request context of actor in the fixture's scope.
func (f *fixture) as(actor kernel.ActorRef) context.Context {
	f.t.Helper()
	ctx, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: actor, Scope: f.scope})
	if err != nil {
		f.t.Fatal(err)
	}
	return action.WithCorrelation(ctx, pgtest.UniqueID(f.t, "corr."))
}

func (f *fixture) key() string { return fmt.Sprintf("k%04d", f.serial.Add(1)) }

func codeOf(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

func pathOf(err error) string {
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 {
		return rej.Fields[0].Path
	}
	return ""
}

func (f *fixture) want(err error, code string) {
	f.t.Helper()
	if got := codeOf(err); got != code {
		f.t.Fatalf("got %s (%v), want %s", got, err, code)
	}
}

func (f *fixture) delegate(id, agentID, task string, autonomy capability.SideEffect, caps ...string) policy.Receipt {
	f.t.Helper()
	r, err := f.layer.GrantDelegation(f.as(clerk), policy.GrantDelegation{IdempotencyKey: f.key(), DelegationID: id, AgentID: agentID,
		TaskID: task, Capabilities: caps, Autonomy: string(autonomy), ExpiresAt: start.Add(24 * time.Hour)})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func toolCall(name string, args any) model.ToolCall {
	raw, _ := json.Marshal(args)
	return model.ToolCall{ID: "call.1", Name: name, Arguments: raw}
}

func rawCall(name, args string) model.ToolCall {
	return model.ToolCall{ID: "call.1", Name: name, Arguments: json.RawMessage(args)}
}

var saveArgs = map[string]any{"ProbeID": "probe.1", "ExpectedVersion": 0, "Lines": []map[string]string{{"Sku": "sku.1", "Quantity": "960.00"}}}

// audit returns the rejected entries of the AI layer's audit trail as "command code".
func (f *fixture) rejected() []string {
	f.t.Helper()
	entries, err := f.layer.Audit(f.as(auditor))
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Outcome == "rejected" {
			out = append(out, e.Command+" "+e.Code)
		}
	}
	return out
}

func (f *fixture) outcomes(command string) []string {
	f.t.Helper()
	entries, err := f.layer.Audit(f.as(auditor))
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Command == command {
			out = append(out, e.Outcome+" "+e.Code)
		}
	}
	return out
}

// ai.delegation.grant and ai.delegation.revoke: human-only commands on the AI layer's ledger, with
// replay of the same key, the stored delegation, the reads and revocation once.
func TestDelegationGrantReplayAndRevoke(t *testing.T) {
	f := newFixture(t)
	cmd := policy.GrantDelegation{IdempotencyKey: "grant.1", DelegationID: "del.1", AgentID: agent.ID, TaskID: "task.1",
		Capabilities: []string{cmdSave, cmdAnnotate}, Autonomy: "draft", ExpiresAt: start.Add(72 * time.Hour)}
	r, err := f.layer.GrantDelegation(f.as(clerk), cmd)
	if err != nil || r.ObjectID != "del.1" || r.RowVersion != 1 || r.Status != policy.StatusActive || r.Replayed {
		t.Fatalf("grant %+v, %v", r, err)
	}
	again, err := f.layer.GrantDelegation(f.as(clerk), cmd)
	if err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("replay %+v, %v", again, err)
	}
	changed := cmd
	changed.Capabilities = []string{cmdSave}
	_, err = f.layer.GrantDelegation(f.as(clerk), changed)
	f.want(err, kernel.CodeIdempotencyPayloadConflict)
	changed.IdempotencyKey = "grant.2"
	_, err = f.layer.GrantDelegation(f.as(clerk), changed)
	f.want(err, kernel.CodeVersionConflict) // the ID is taken

	d, err := f.layer.GetDelegation(f.as(agent), "del.1")
	if err != nil || d.InitiatorID != clerk.ID || d.AgentID != agent.ID || d.TaskID != "task.1" || d.Autonomy != capability.Draft ||
		!slices.Equal(d.Capabilities, []string{cmdAnnotate, cmdSave}) || !d.ExpiresAt.Equal(start.Add(72*time.Hour)) || d.GrantedEpoch < 1 {
		t.Fatalf("the agent reads %+v, %v", d, err)
	}
	_, err = f.layer.GetDelegation(f.as(viewer), "del.1")
	f.want(err, kernel.CodeNotFound) // own data scope: another initiator's delegation
	if _, err = f.layer.GetDelegation(f.as(auditor), "del.1"); err != nil {
		t.Fatal(err)
	}

	rv := policy.RevokeDelegation{IdempotencyKey: "revoke.1", DelegationID: "del.1", ExpectedVersion: 2, Reason: "task re-planned"}
	_, err = f.layer.RevokeDelegation(f.as(clerk), rv)
	f.want(err, kernel.CodeVersionConflict)
	_, err = f.layer.RevokeDelegation(f.as(agent), policy.RevokeDelegation{IdempotencyKey: "revoke.ai", DelegationID: "del.1",
		ExpectedVersion: 1, Reason: "self"})
	f.want(err, kernel.CodePermissionDenied) // human-only, whatever the agent holds
	rv.ExpectedVersion = 1
	res, err := f.layer.RevokeDelegation(f.as(clerk), rv)
	if err != nil || res.RowVersion != 2 || res.Status != policy.StatusRevoked {
		t.Fatalf("revoke %+v, %v", res, err)
	}
	rv.IdempotencyKey, rv.ExpectedVersion = "revoke.2", 2
	_, err = f.layer.RevokeDelegation(f.as(clerk), rv)
	f.want(err, kernel.CodeInvalidState)
	d, _ = f.layer.GetDelegation(f.as(clerk), "del.1")
	if d.Status != policy.StatusRevoked || d.RevokedByID != clerk.ID || d.RevokeReason != "task re-planned" || d.RowVersion != 2 {
		t.Fatalf("revoked %+v", d)
	}
}

// What a delegation may hold (AI-INV-001, AI-INV-002): only registered capabilities, never a
// human-only command, within its autonomy, for an active AI agent, nothing the initiator does not
// hold, for at most 30 days; only humans grant.
func TestDelegationGrantChecks(t *testing.T) {
	f := newFixture(t)
	base := policy.GrantDelegation{DelegationID: "del.x", AgentID: agent.ID, TaskID: "task.1", Capabilities: []string{cmdSave},
		Autonomy: "draft", ExpiresAt: start.Add(time.Hour)}
	cases := []struct {
		name   string
		actor  kernel.ActorRef
		change func(*policy.GrantDelegation)
		code   string
		path   string
	}{
		{"an AI grants", agent, func(*policy.GrantDelegation) {}, kernel.CodePermissionDenied, "/"},
		{"the system grants", system, func(*policy.GrantDelegation) {}, kernel.CodePermissionDenied, "/"},
		{"human-only capability", clerk, func(c *policy.GrantDelegation) { c.Capabilities = []string{cmdSave, "quote.activate"} },
			kernel.CodePermissionDenied, "/args/capabilities/1"},
		{"unregistered capability", clerk, func(c *policy.GrantDelegation) { c.Capabilities = []string{"sql.exec"} },
			kernel.CodeUnsupportedContract, "/args/capabilities/0"},
		{"above the autonomy", clerk, func(c *policy.GrantDelegation) { c.Capabilities = []string{cmdPrepare} },
			kernel.CodeInvalidKnownValue, "/args/capabilities/0"},
		{"read autonomy", clerk, func(c *policy.GrantDelegation) { c.Autonomy = "read" }, kernel.CodeInvalidKnownValue, "/args/capabilities/0"},
		{"a human agent", clerk, func(c *policy.GrantDelegation) { c.AgentID = clerk2.ID }, kernel.CodeReferenceInvalid, "/args/agent_id"},
		{"an unknown agent", clerk, func(c *policy.GrantDelegation) { c.AgentID = "agent.9" }, kernel.CodeReferenceInvalid, "/args/agent_id"},
		{"expired", clerk, func(c *policy.GrantDelegation) { c.ExpiresAt = start }, kernel.CodeInvalidKnownValue, "/args/expires_at"},
		{"too long", clerk, func(c *policy.GrantDelegation) { c.ExpiresAt = start.Add(policy.MaxLifetime + time.Second) },
			kernel.CodeInvalidKnownValue, "/args/expires_at"},
		{"no capabilities", clerk, func(c *policy.GrantDelegation) { c.Capabilities = nil }, kernel.CodeSchemaInvalid, "/args/capabilities"},
		{"duplicate capability", clerk, func(c *policy.GrantDelegation) { c.Capabilities = []string{cmdSave, cmdSave} },
			kernel.CodeSchemaInvalid, "/args/capabilities/1"},
		{"autonomy", clerk, func(c *policy.GrantDelegation) { c.Autonomy = "full" }, kernel.CodeSchemaInvalid, "/args/autonomy"},
		{"task", clerk, func(c *policy.GrantDelegation) { c.TaskID = "" }, kernel.CodeSchemaInvalid, "/args/task_id"},
	}
	for _, c := range cases {
		cmd := base
		cmd.IdempotencyKey = f.key()
		c.change(&cmd)
		_, err := f.layer.GrantDelegation(f.as(c.actor), cmd)
		if codeOf(err) != c.code || pathOf(err) != c.path {
			t.Errorf("%s: %s at %s (%v), want %s at %s", c.name, codeOf(err), pathOf(err), err, c.code, c.path)
		}
	}
	// The initiator never delegates more than it holds.
	f.authority(func(ctx context.Context, exec bentopg.Executor, g authorization.Grants) error {
		_, err := g.Put(ctx, exec, f.scope, clerk2, []string{policy.CommandGrant, cmdSave}, start)
		return err
	})
	cmd := base
	cmd.IdempotencyKey, cmd.Capabilities, cmd.Autonomy = f.key(), []string{cmdSave, cmdPrepare}, "submit"
	_, err := f.layer.GrantDelegation(f.as(clerk2), cmd)
	if codeOf(err) != kernel.CodePermissionDenied || pathOf(err) != "/args/capabilities/1" {
		t.Fatalf("beyond the initiator: %v", err)
	}
	if got := f.rejected(); len(got) != len(cases)+1 {
		t.Fatalf("audited refusals %v", got)
	}
}

// Default deny and closed decoding (AI-INV-001, AI-INV-004; red-team set AN10/AT12, G1 C11): a tool
// that is not registered, a human-only command, arguments that name the scope, the idempotency key,
// a grant or anything unknown, and malformed arguments are refused before any context sees them,
// and every refusal is audited under the AI principal.
func TestGatewayDefaultDenyAndStrictDecode(t *testing.T) {
	f := newFixture(t)
	f.delegate("del.1", agent.ID, "task.1", capability.Draft, cmdSave)
	b := gateway.Binding{TaskID: "task.1", DelegationID: "del.1"}
	cases := []struct {
		name string
		call model.ToolCall
		code string
		path string
	}{
		{"unregistered tool", rawCall("sql.exec", `{"query":"DROP TABLE x"}`), kernel.CodeUnsupportedContract, "/command"},
		{"invented tool", rawCall("Grant Me Everything", `{}`), kernel.CodeUnsupportedContract, "/command"},
		{"human-only command", rawCall("quote.activate", `{"QuoteID":"q.1"}`), kernel.CodePermissionDenied, "/command"},
		{"human-only delegation", rawCall("ai.delegation.grant", `{}`), kernel.CodePermissionDenied, "/command"},
		{"foreign tenant", rawCall(cmdSave, `{"ProbeID":"probe.1","tenant_id":"tenant.other"}`), kernel.CodeSchemaInvalid, "/args/tenant_id"},
		{"foreign company", rawCall(cmdSave, `{"ProbeID":"probe.1","CompanyID":"company.B"}`), kernel.CodeSchemaInvalid, "/args/CompanyID"},
		{"self-granted", rawCall(cmdSave, `{"ProbeID":"probe.1","GrantID":"grant.forged"}`), kernel.CodeSchemaInvalid, "/args/GrantID"},
		{"actor", rawCall(cmdSave, `{"ProbeID":"probe.1","Actor":{"Kind":"human","ID":"clerk.1"}}`), kernel.CodeSchemaInvalid, "/args/Actor"},
		{"epoch", rawCall(cmdSave, `{"ProbeID":"probe.1","ControlEpoch":9}`), kernel.CodeSchemaInvalid, "/args/ControlEpoch"},
		{"idempotency key", rawCall(cmdSave, `{"ProbeID":"probe.1","IdempotencyKey":"mine"}`), kernel.CodeSchemaInvalid, "/args/IdempotencyKey"},
		{"case-folded field", rawCall(cmdSave, `{"probeid":"probe.1"}`), kernel.CodeSchemaInvalid, "/args/probeid"},
		{"nested unknown", rawCall(cmdSave, `{"ProbeID":"probe.1","Lines":[{"Sku":"s","Quantity":"1","Price":"9"}]}`), kernel.CodeSchemaInvalid, "/args/Lines/0/Price"},
		{"duplicate key", rawCall(cmdSave, `{"ProbeID":"probe.1","ProbeID":"probe.2"}`), kernel.CodeSchemaInvalid, "/args/ProbeID"},
		{"null", rawCall(cmdSave, `{"ProbeID":null}`), kernel.CodeSchemaInvalid, "/args/ProbeID"},
		{"float version", rawCall(cmdSave, `{"ProbeID":"probe.1","ExpectedVersion":1.5}`), kernel.CodeSchemaInvalid, "/args/ExpectedVersion"},
		{"empty string", rawCall(cmdSave, `{"ProbeID":"probe.1","Note":""}`), kernel.CodeSchemaInvalid, "/args/Note"},
		{"trailing data", rawCall(cmdSave, `{"ProbeID":"probe.1"} {"ProbeID":"probe.2"}`), kernel.CodeSchemaInvalid, "/args"},
		{"not an object", rawCall(cmdSave, `["probe.1"]`), kernel.CodeSchemaInvalid, "/args"},
		{"no arguments", rawCall(cmdSave, ``), kernel.CodeSchemaInvalid, "/args"},
	}
	for _, c := range cases {
		_, err := f.layer.Gateway.Decode(f.as(agent), b, c.call)
		if codeOf(err) != c.code || pathOf(err) != c.path {
			t.Errorf("%s: %s at %s (%v), want %s at %s", c.name, codeOf(err), pathOf(err), err, c.code, c.path)
		}
	}
	// A human or the system does not act through the gateway.
	for _, actor := range []kernel.ActorRef{clerk, system} {
		_, err := f.layer.Gateway.Decode(f.as(actor), b, toolCall(cmdSave, saveArgs))
		f.want(err, kernel.CodePermissionDenied)
	}
	if got := f.rec.all(); len(got) != 0 {
		t.Fatalf("a context was called: %+v", got)
	}
	audited := f.rejected()
	if len(audited) != len(cases)+2 || audited[0] != "sql.exec UNSUPPORTED_CONTRACT" || audited[1] != "invalid UNSUPPORTED_CONTRACT" ||
		audited[2] != "quote.activate PERMISSION_DENIED" {
		t.Fatalf("audited %v", audited)
	}
	// The binding is the executor's; a missing one is a programming error, not a verdict.
	if _, err := f.layer.Gateway.Decode(f.as(agent), gateway.Binding{TaskID: "task.1"}, toolCall(cmdSave, saveArgs)); !errors.Is(err, gateway.ErrBinding) {
		t.Fatalf("binding: %v", err)
	}
}

// A delegated draft runs at its context as the AI agent, in the agent's scope, with the key of the
// persisted intent, never one from the model; the tools offered are the delegation's.
func TestGatewayRunsADelegatedDraft(t *testing.T) {
	f := newFixture(t)
	f.delegate("del.1", agent.ID, "task.1", capability.Draft, cmdSave)
	b := gateway.Binding{TaskID: "task.1", DelegationID: "del.1"}
	c, err := f.layer.Gateway.Decode(f.as(agent), b, toolCall(cmdSave, saveArgs))
	if err != nil {
		t.Fatal(err)
	}
	if c.NeedsGrant() || c.Object != (capability.ObjectRef{Type: "prb.probe", ID: "probe.1"}) || len(c.RequestHash) != 64 ||
		c.Delegation.DelegationID != "del.1" {
		t.Fatalf("call %+v", c)
	}
	a, err := f.layer.Gateway.Admit(f.as(agent), b, c, "intent.1")
	if err != nil || a.Epoch < 1 || a.Grant != nil {
		t.Fatalf("admit %+v, %v", a, err)
	}
	res, err := a.Run(f.as(agent))
	if err != nil || res.ObjectID != "probe.1" || res.Status != "draft" || res.ActionID != "act-intent.1" {
		t.Fatalf("run %+v, %v", res, err)
	}
	calls := f.rec.all()
	if len(calls) != 1 || calls[0].actor != agent || calls[0].scope != f.scope || calls[0].key != "intent.1" {
		t.Fatalf("calls %+v", calls)
	}
	if got := calls[0].cmd.(saveProbe); got.ProbeID != "probe.1" || len(got.Lines) != 1 || got.Lines[0].Quantity != "960.00" {
		t.Fatalf("command %+v", got)
	}
	tools, err := f.layer.Gateway.Tools(f.as(agent), c.Delegation)
	if err != nil || len(tools) != 1 || tools[0].Name != cmdSave || len(tools[0].Parameters) == 0 {
		t.Fatalf("tools %+v, %v", tools, err)
	}
	if _, err = f.layer.Gateway.Admit(f.as(agent), b, c, "not a key!"); err == nil {
		t.Fatal("an invalid key must not be admitted")
	}
}

// Every step re-checks the delegation and the current grants (AI-INV-002, AN04, baseline 8.7): an
// unknown delegation, another agent's, another task's, an unlisted capability or a revoked
// delegation is GRANT_INVALID; an expired one GRANT_EXPIRED; an initiator or agent whose grant was
// revoked or narrowed is PERMISSION_DENIED — also between Decode and Admit.
func TestEveryStepRechecksTheDelegation(t *testing.T) {
	f := newFixture(t)
	f.delegate("del.1", agent.ID, "task.1", capability.Draft, cmdSave)
	f.delegate("del.2", agent2.ID, "task.2", capability.Draft, cmdSave)
	save := toolCall(cmdSave, saveArgs)
	for name, b := range map[string]gateway.Binding{
		"unknown delegation":   {TaskID: "task.1", DelegationID: "del.9"},
		"another agent's":      {TaskID: "task.2", DelegationID: "del.2"},
		"another task":         {TaskID: "task.9", DelegationID: "del.1"},
		"unlisted capability ": {TaskID: "task.1", DelegationID: "del.1"},
	} {
		call := save
		if name == "unlisted capability " {
			call = toolCall(cmdAnnotate, saveArgs)
		}
		_, err := f.layer.Gateway.Decode(f.as(agent), b, call)
		if codeOf(err) != codes.GrantInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	b := gateway.Binding{TaskID: "task.1", DelegationID: "del.1"}
	c, err := f.layer.Gateway.Decode(f.as(agent), b, save)
	if err != nil {
		t.Fatal(err)
	}
	// The initiator loses the permission between Decode and Admit.
	f.authority(func(ctx context.Context, exec bentopg.Executor, g authorization.Grants) error {
		_, err := g.Put(ctx, exec, f.scope, clerk, []string{policy.CommandGrant, policy.CommandRevoke}, start)
		return err
	})
	_, err = f.layer.Gateway.Admit(f.as(agent), b, c, "intent.1")
	f.want(err, kernel.CodePermissionDenied)
	// ... and its grant is revoked.
	f.authority(func(ctx context.Context, exec bentopg.Executor, g authorization.Grants) error {
		if _, err := g.Put(ctx, exec, f.scope, clerk, humanPermissions, start); err != nil {
			return err
		}
		_, err := g.Revoke(ctx, exec, f.scope, clerk.ID, start)
		return err
	})
	_, err = f.layer.Gateway.Decode(f.as(agent), b, save)
	f.want(err, kernel.CodePermissionDenied)
	f.authority(func(ctx context.Context, exec bentopg.Executor, g authorization.Grants) error {
		_, err := g.Put(ctx, exec, f.scope, clerk, humanPermissions, start)
		return err
	})
	// The agent loses the permission.
	f.authority(func(ctx context.Context, exec bentopg.Executor, g authorization.Grants) error {
		_, err := g.Put(ctx, exec, f.scope, agent, []string{policy.PermissionRead}, start)
		return err
	})
	_, err = f.layer.Gateway.Decode(f.as(agent), b, save)
	f.want(err, kernel.CodePermissionDenied)
	f.authority(func(ctx context.Context, exec bentopg.Executor, g authorization.Grants) error {
		_, err := g.Put(ctx, exec, f.scope, agent, agentPermissions, start)
		return err
	})
	if _, err = f.layer.Gateway.Admit(f.as(agent), b, c, "intent.1"); err != nil {
		t.Fatalf("restored grants: %v", err)
	}
	// The delegation expires.
	f.clock.advance(24 * time.Hour)
	_, err = f.layer.Gateway.Admit(f.as(agent), b, c, "intent.1")
	f.want(err, codes.GrantExpired)
	f.clock.advance(-time.Hour)
	// The delegation is revoked.
	if _, err = f.layer.RevokeDelegation(f.as(clerk), policy.RevokeDelegation{IdempotencyKey: f.key(), DelegationID: "del.1",
		ExpectedVersion: 1, Reason: "stop"}); err != nil {
		t.Fatal(err)
	}
	_, err = f.layer.Gateway.Admit(f.as(agent), b, c, "intent.1")
	f.want(err, codes.GrantInvalid)
	if len(f.rec.all()) != 0 {
		t.Fatal("no call may have run")
	}
	if got := f.rejected(); len(got) != 9 {
		t.Fatalf("audited %v", got)
	}
}

// prepare is the decoded probe.prepare call of the fixture's delegation.
func (f *fixture) prepare(b gateway.Binding, args map[string]any) gateway.Call {
	f.t.Helper()
	c, err := f.layer.Gateway.Decode(f.as(agent), b, toolCall(cmdPrepare, args))
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) issue(actor kernel.ActorRef, id string, c gateway.Call, delegation string, expires time.Duration) error {
	_, err := f.layer.IssueApprovalGrant(f.as(actor), approvalgrant.IssueGrant{IdempotencyKey: f.key(), GrantID: id,
		DelegationID: delegation, Command: c.Capability.Command, RequestHash: c.RequestHash, ObjectID: c.Object.ID,
		ObjectVersion: c.Object.Version, ExpiresAt: start.Add(expires)})
	return err
}

var prepareArgs = map[string]any{"OrderID": "order.1", "QuoteID": "quote.1", "BusinessRevision": 2, "BusinessHash": "hash.1"}

// AI-INV-002 and Q01 V0.1 tests/test_execution.py::test_grant_reference_is_checked: a call that
// needs a grant runs only with a human-issued grant of the agent's delegation that binds exactly
// its command, request hash, object and version, before it expires, once; a missing, forged,
// mismatched, revoked, expired or reused grant is refused, and so is a grant whose issuer no longer
// holds the command's permission.
func TestApprovalGrantBindsOneCall(t *testing.T) {
	f := newFixture(t)
	f.delegate("del.1", agent.ID, "task.1", capability.Submit, cmdSave, cmdPrepare)
	f.delegate("del.2", agent2.ID, "task.1", capability.Submit, cmdPrepare)
	b := gateway.Binding{TaskID: "task.1", DelegationID: "del.1"}
	c := f.prepare(b, prepareArgs)
	if !c.NeedsGrant() || c.Object != (capability.ObjectRef{Type: "prb.quote", ID: "quote.1", Version: 2}) {
		t.Fatalf("call %+v", c)
	}
	_, err := f.layer.Gateway.Admit(f.as(agent), b, c, "intent.1")
	f.want(err, codes.GrantInvalid) // no grant: the task waits for approval
	b.GrantID = "forged.grant"
	_, err = f.layer.Gateway.Admit(f.as(agent), b, c, "intent.1")
	f.want(err, codes.GrantInvalid)

	if err = f.issue(clerk, "grant.1", c, "del.1", time.Hour); err != nil {
		t.Fatal(err)
	}
	other := map[string]any{"OrderID": "order.1", "QuoteID": "quote.1", "BusinessRevision": 2, "BusinessHash": "hash.2"}
	moved := map[string]any{"OrderID": "order.1", "QuoteID": "quote.1", "BusinessRevision": 3, "BusinessHash": "hash.1"}
	b.GrantID = "grant.1"
	for name, args := range map[string]map[string]any{"another request": other, "another version": moved} {
		_, err = f.layer.Gateway.Admit(f.as(agent), b, f.prepare(b, args), "intent.x")
		if codeOf(err) != codes.GrantInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Another agent's delegation cannot use it.
	b2 := gateway.Binding{TaskID: "task.1", DelegationID: "del.2", GrantID: "grant.1"}
	c2, err := f.layer.Gateway.Decode(f.as(agent2), b2, toolCall(cmdPrepare, prepareArgs))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.layer.Gateway.Admit(f.as(agent2), b2, c2, "intent.1")
	f.want(err, codes.GrantInvalid)

	a, err := f.layer.Gateway.Admit(f.as(agent), b, c, "intent.1")
	if err != nil || a.Grant == nil || a.Grant.Status != approvalgrant.StatusUsed || a.Grant.UsedKey != "intent.1" || a.Grant.UsedByID != agent.ID {
		t.Fatalf("admit %+v, %v", a, err)
	}
	if res, err := a.Run(f.as(agent)); err != nil || res.ObjectID != "order.1" {
		t.Fatalf("run %+v, %v", res, err)
	}
	// The same call again (a resend after an uncertain outcome) is the same use; any other is not.
	again, err := f.layer.Gateway.Admit(f.as(agent), b, c, "intent.1")
	if err != nil || again.Grant.RowVersion != 2 {
		t.Fatalf("same key %+v, %v", again, err)
	}
	_, err = f.layer.Gateway.Admit(f.as(agent), b, c, "intent.2")
	f.want(err, codes.GrantInvalid)
	g, err := f.layer.GetApprovalGrant(f.as(clerk), "grant.1")
	if err != nil || g.Status != approvalgrant.StatusUsed || g.RequestHash != c.RequestHash || g.ObjectType != "prb.quote" ||
		g.AgentID != agent.ID || g.TaskID != "task.1" || g.IssuerID != clerk.ID {
		t.Fatalf("grant %+v, %v", g, err)
	}
	_, err = f.layer.RevokeApprovalGrant(f.as(clerk), approvalgrant.RevokeGrant{IdempotencyKey: f.key(), GrantID: "grant.1", ExpectedVersion: 2, Reason: "late"})
	f.want(err, kernel.CodeInvalidState) // used

	// Expired and revoked grants.
	if err = f.issue(clerk, "grant.2", c, "del.1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err = f.issue(clerk, "grant.3", c, "del.1", 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err = f.layer.RevokeApprovalGrant(f.as(clerk), approvalgrant.RevokeGrant{IdempotencyKey: f.key(), GrantID: "grant.3",
		ExpectedVersion: 1, Reason: "withdrawn"}); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(time.Hour)
	b.GrantID = "grant.2"
	_, err = f.layer.Gateway.Admit(f.as(agent), b, c, "intent.3")
	f.want(err, codes.GrantExpired)
	b.GrantID = "grant.3"
	_, err = f.layer.Gateway.Admit(f.as(agent), b, c, "intent.3")
	f.want(err, codes.GrantInvalid)

	// The issuer loses the command's permission after issuing.
	if err = f.issue(approver, "grant.4", c, "del.1", 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	f.authority(func(ctx context.Context, exec bentopg.Executor, g authorization.Grants) error {
		_, err := g.Put(ctx, exec, f.scope, approver, []string{approvalgrant.CommandIssue, policy.PermissionRead}, start)
		return err
	})
	b.GrantID = "grant.4"
	_, err = f.layer.Gateway.Admit(f.as(agent), b, c, "intent.4")
	f.want(err, kernel.CodePermissionDenied)

	if calls := f.rec.all(); len(calls) != 1 || calls[0].command != cmdPrepare || calls[0].key != "intent.1" {
		t.Fatalf("calls %+v", calls)
	}
	if got := f.outcomes(approvalgrant.CommandUse); !slices.Equal(got, []string{"succeeded ", "replayed "}) {
		t.Fatalf("uses %v", got)
	}
	grants, err := f.layer.ApprovalGrantsOf(f.as(agent), "del.1")
	if err != nil || len(grants) != 4 {
		t.Fatalf("grants of del.1: %d, %v", len(grants), err)
	}
}

// What an approval grant may be issued for: only by a human who sees an active delegation, for a
// registered capability that needs a grant and that the delegation allows, which the issuer could
// call itself, for at most 7 days.
func TestApprovalGrantIssueChecks(t *testing.T) {
	f := newFixture(t)
	f.delegate("del.1", agent.ID, "task.1", capability.Submit, cmdSave, cmdPrepare)
	f.delegate("del.2", agent.ID, "task.2", capability.Draft, cmdSave)
	f.delegate("del.3", agent.ID, "task.3", capability.Submit, cmdPrepare)
	if _, err := f.layer.RevokeDelegation(f.as(clerk), policy.RevokeDelegation{IdempotencyKey: f.key(), DelegationID: "del.3",
		ExpectedVersion: 1, Reason: "ended"}); err != nil {
		t.Fatal(err)
	}
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	base := approvalgrant.IssueGrant{GrantID: "grant.x", DelegationID: "del.1", Command: cmdPrepare, RequestHash: hash,
		ObjectID: "quote.1", ObjectVersion: 2, ExpiresAt: start.Add(time.Hour)}
	f.authority(func(ctx context.Context, exec bentopg.Executor, g authorization.Grants) error {
		_, err := g.Put(ctx, exec, f.scope, approver, []string{approvalgrant.CommandIssue, policy.PermissionRead}, start)
		return err
	})
	cases := []struct {
		name   string
		actor  kernel.ActorRef
		change func(*approvalgrant.IssueGrant)
		code   string
		path   string
	}{
		{"an AI issues", agent, func(*approvalgrant.IssueGrant) {}, kernel.CodePermissionDenied, "/"},
		{"unknown delegation", clerk, func(c *approvalgrant.IssueGrant) { c.DelegationID = "del.9" }, kernel.CodeReferenceInvalid, "/args/delegation_id"},
		{"no issue permission", viewer, func(c *approvalgrant.IssueGrant) {}, kernel.CodePermissionDenied, "/"},
		{"unseen delegation", clerk2, func(c *approvalgrant.IssueGrant) {}, kernel.CodeReferenceInvalid, "/args/delegation_id"},
		{"revoked delegation", clerk, func(c *approvalgrant.IssueGrant) { c.DelegationID = "del.3" }, kernel.CodeInvalidState, "/args/delegation_id"},
		{"human-only command", clerk, func(c *approvalgrant.IssueGrant) { c.Command = "order.activate" }, kernel.CodePermissionDenied, "/args/command"},
		{"unregistered command", clerk, func(c *approvalgrant.IssueGrant) { c.Command = "sql.exec" }, kernel.CodeUnsupportedContract, "/args/command"},
		{"needs no grant", clerk, func(c *approvalgrant.IssueGrant) { c.Command = cmdSave }, kernel.CodeInvalidKnownValue, "/args/command"},
		{"not delegated", clerk, func(c *approvalgrant.IssueGrant) { c.DelegationID = "del.2" }, kernel.CodeInvalidKnownValue, "/args/command"},
		{"expired", clerk, func(c *approvalgrant.IssueGrant) { c.ExpiresAt = start }, kernel.CodeInvalidKnownValue, "/args/expires_at"},
		{"too long", clerk, func(c *approvalgrant.IssueGrant) { c.ExpiresAt = start.Add(approvalgrant.MaxLifetime + time.Second) },
			kernel.CodeInvalidKnownValue, "/args/expires_at"},
		{"issuer cannot prepare", approver, func(c *approvalgrant.IssueGrant) {}, kernel.CodePermissionDenied, "/args/command"},
		{"hash", clerk, func(c *approvalgrant.IssueGrant) { c.RequestHash = "ABC" }, kernel.CodeSchemaInvalid, "/args/request_hash"},
		{"version", clerk, func(c *approvalgrant.IssueGrant) { c.ObjectVersion = -1 }, kernel.CodeSchemaInvalid, "/args/object_version"},
	}
	for _, c := range cases {
		cmd := base
		cmd.IdempotencyKey = f.key()
		c.change(&cmd)
		_, err := f.layer.IssueApprovalGrant(f.as(c.actor), cmd)
		if codeOf(err) != c.code || pathOf(err) != c.path {
			t.Errorf("%s: %s at %s (%v), want %s at %s", c.name, codeOf(err), pathOf(err), err, c.code, c.path)
		}
	}
	cmd := base
	cmd.IdempotencyKey = f.key()
	r, err := f.layer.IssueApprovalGrant(f.as(clerk), cmd)
	if err != nil || r.Status != approvalgrant.StatusIssued || r.ObjectID != "grant.x" {
		t.Fatalf("issue %+v, %v", r, err)
	}
	cmd.IdempotencyKey = f.key()
	_, err = f.layer.IssueApprovalGrant(f.as(clerk), cmd)
	f.want(err, kernel.CodeVersionConflict)
}

// Of two concurrent uses of one grant by different calls, exactly one succeeds (AI-INV-002
// concurrency).
func TestConcurrentUsesOfOneGrant(t *testing.T) {
	f := newFixture(t)
	f.delegate("del.1", agent.ID, "task.1", capability.Submit, cmdPrepare)
	b := gateway.Binding{TaskID: "task.1", DelegationID: "del.1", GrantID: "grant.1"}
	c := f.prepare(b, prepareArgs)
	if err := f.issue(clerk, "grant.1", c, "del.1", time.Hour); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	results := make(chan string, n)
	for i := range n {
		wg.Go(func() {
			_, err := f.layer.Gateway.Admit(f.as(agent), b, c, fmt.Sprintf("intent.%d", i))
			results <- codeOf(err)
		})
	}
	wg.Wait()
	close(results)
	var got []string
	for r := range results {
		got = append(got, r)
	}
	slices.Sort(got)
	want := append(slices.Repeat([]string{codes.GrantInvalid}, n-1), "OK")
	if !slices.Equal(got, want) {
		t.Fatalf("outcomes %v", got)
	}
}

// brant_ai keeps its records as the triggers and grants say: the application role cannot change a
// grant's binding, revive a used grant or delete anything.
func TestStorageKeepsTheBinding(t *testing.T) {
	f := newFixture(t)
	f.delegate("del.1", agent.ID, "task.1", capability.Submit, cmdPrepare)
	b := gateway.Binding{TaskID: "task.1", DelegationID: "del.1", GrantID: "grant.1"}
	c := f.prepare(b, prepareArgs)
	if err := f.issue(clerk, "grant.1", c, "del.1", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := f.layer.Gateway.Admit(f.as(agent), b, c, "intent.1"); err != nil {
		t.Fatal(err)
	}
	for name, stmt := range map[string]string{
		"rebind":        `UPDATE brant_ai.approval_grant SET request_hash = repeat('0', 64) WHERE grant_id = 'grant.1'`,
		"revive":        `UPDATE brant_ai.approval_grant SET status = 'issued', row_version = 3, used_key = NULL, used_by_id = NULL, used_epoch = NULL, used_at = NULL WHERE grant_id = 'grant.1'`,
		"delete grant":  `DELETE FROM brant_ai.approval_grant WHERE grant_id = 'grant.1'`,
		"widen":         `UPDATE brant_ai.delegation SET capabilities = ARRAY['probe.prepare', 'quote.activate'] WHERE delegation_id = 'del.1'`,
		"delete ledger": `DELETE FROM brant_ai.audit_event`,
	} {
		err := policy.WithinScope(context.Background(), f.db, f.scope, func(ctx context.Context, exec bentopg.Executor) error {
			_, err := exec.Exec(ctx, stmt)
			return err
		})
		if err == nil {
			t.Errorf("%s was allowed", name)
		}
	}
}
