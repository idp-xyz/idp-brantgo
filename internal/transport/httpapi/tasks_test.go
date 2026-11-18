package httpapi

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	"go.idp.xyz/idp-brant/internal/ai"
	"go.idp.xyz/idp-brant/internal/ai/approvalgrant"
	"go.idp.xyz/idp-brant/internal/ai/execution"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TSK's and the AI layer's routes and the task-step route against fakes (WBS S4-01f; S4 unit C3).

type fakeTSK struct {
	TSK
	commands []any
	audited  []string
}

func (f *fakeTSK) receipt(cmd any, id string) (tskapi.TaskReceipt, error) {
	f.commands = append(f.commands, cmd)
	return tskapi.TaskReceipt{Receipt: tskapi.Receipt{ActionID: "act-1", ObjectID: id, RowVersion: 2, Status: tskapi.StateStopping},
		ControlMode: tskapi.ModeAI, ControlEpoch: 2, BoundQuoteID: "quote.1"}, nil
}

func (f *fakeTSK) CreateTask(_ context.Context, c tskapi.CreateTask) (tskapi.TaskReceipt, error) {
	return f.receipt(c, c.TaskID)
}
func (f *fakeTSK) WaitTask(_ context.Context, c tskapi.WaitTask) (tskapi.TaskReceipt, error) {
	return f.receipt(c, c.TaskID)
}
func (f *fakeTSK) TakeControl(_ context.Context, c tskapi.TakeControl) (tskapi.TaskReceipt, error) {
	return f.receipt(c, c.TaskID)
}

func (f *fakeTSK) RecordAttempt(_ context.Context, c tskapi.RecordAttempt) (tskapi.ActionReceipt, error) {
	f.commands = append(f.commands, c)
	return tskapi.ActionReceipt{Receipt: tskapi.Receipt{ActionID: "act-2", ObjectID: tskapi.ActionIDOf(c.BusinessIntentID), RowVersion: 1,
		Status: tskapi.ActionSubmitted}, IdempotencyKey: tskapi.ActionIDOf(c.BusinessIntentID), Attempt: 1, State: tskapi.ActionSubmitted}, nil
}

func (f *fakeTSK) GetTask(_ context.Context, id string) (tskapi.TaskView, error) {
	if id != "task.1" {
		return tskapi.TaskView{}, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	return tskapi.TaskView{TaskID: id, GoalCode: tskapi.GoalQuoteReady, Lifecycle: tskapi.StateRunning, ControlMode: tskapi.ModeAI, ControlEpoch: 1,
		TaskVersion: 4, Initiator: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "alice"}, OwnerID: "alice", AgentID: "agent.1",
		Target: tskapi.Target{QuoteID: "quote.1"}, Source: &tskapi.SourceRef{Kind: tskapi.SourceFile, ID: "file.inquiry"},
		Binding: &tskapi.BindingView{QuoteID: "quote.1", State: tskapi.BindingBound, ControlEpoch: 1,
			BoundAt: time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)},
		History: []tskapi.TransitionView{{Seq: 1, Command: tskapi.CommandCreate, To: tskapi.StateCreated, TaskVersion: 1}}}, nil
}

func (f *fakeTSK) GoalVerifications(context.Context, string) ([]tskapi.GoalVerification, error) {
	return nil, nil
}

func (f *fakeTSK) TaskActions(_ context.Context, id string) ([]tskapi.ActionView, error) {
	return []tskapi.ActionView{{ActionID: "tska.1", TaskID: id, State: tskapi.ActionCommitted,
		Target: tskapi.TargetCommand{Context: tskapi.TargetTRD, Command: trdapi.CommandFreeze, RequestHash: strings.Repeat("a", 64)}}}, nil
}

func (f *fakeTSK) AuditRejected(_ context.Context, command, _, code string) error {
	f.audited = append(f.audited, command+" "+code)
	return nil
}

type fakeAI struct {
	AI
	commands []any
	steps    []execution.Step
	sent     []any
	stepErr  error
	audited  []string
}

func (f *fakeAI) GrantDelegation(_ context.Context, c policy.GrantDelegation) (policy.Receipt, error) {
	f.commands = append(f.commands, c)
	return policy.Receipt{ActionID: "act-3", ObjectID: c.DelegationID, RowVersion: 1, Status: "active"}, nil
}

func (f *fakeAI) IssueApprovalGrant(_ context.Context, c approvalgrant.IssueGrant) (approvalgrant.Receipt, error) {
	f.commands = append(f.commands, c)
	return approvalgrant.Receipt{ActionID: "act-4", ObjectID: c.GrantID, RowVersion: 1, Status: "issued"}, nil
}

func (f *fakeAI) RevokeDelegation(_ context.Context, c policy.RevokeDelegation) (policy.Receipt, error) {
	f.commands = append(f.commands, c)
	return policy.Receipt{ActionID: "act-5", ObjectID: c.DelegationID, RowVersion: 2, Status: "revoked"}, nil
}

func (f *fakeAI) ModelCalls(_ context.Context, id string) ([]executionevidence.Call, error) {
	usd, _ := kernel.NewCurrency("USD", 2)
	zero, _ := kernel.NewMoney(usd, 0)
	in := int64(120)
	return []executionevidence.Call{{CallID: "call.1", TaskID: id, Seq: 1, Kind: "model", State: "succeeded", ActorID: "agent.1",
		Bundle: executionevidence.Ref{BundleID: "q01.agent", SHA256: strings.Repeat("b", 64)}, ModelRequested: "scripted", Reserved: zero,
		ReservedAt: time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC), InputTokens: &in, Cost: &zero}}, nil
}

// RunStep plays the execution entry: it records the step, sends the command with the action's key
// and answers VERIFIED with the owning context's receipt.
func (f *fakeAI) RunStep(ctx context.Context, s execution.Step) (execution.Receipt, error) {
	f.steps = append(f.steps, s)
	if f.stepErr != nil {
		return execution.Receipt{Stage: execution.Accepted}, f.stepErr
	}
	key := tskapi.ActionIDOf(s.BusinessIntentID)
	cmd, err := execution.WithKey(s.Command, key)
	if err != nil {
		return execution.Receipt{}, err
	}
	f.sent = append(f.sent, cmd)
	res, err := s.Send(ctx, cmd)
	if err != nil {
		return execution.Receipt{}, err
	}
	return execution.Receipt{Stage: execution.Verified, TaskID: s.TaskID, BusinessIntentID: s.BusinessIntentID, ActionID: key, Attempt: 1,
		Target: tskapi.TargetCommand{Context: s.Context, Command: trdapi.CommandFreeze, RequestHash: strings.Repeat("c", 64)}, ControlEpoch: 1,
		Result: &res, Stored: &tskapi.TargetReceipt{ActionID: res.ActionID, ObjectID: res.ObjectID, RowVersion: res.RowVersion, Status: res.Status}}, nil
}

func (f *fakeAI) AuditRejected(_ context.Context, command, _, code string) error {
	f.audited = append(f.audited, command+" "+code)
	return nil
}

type stepTRD struct {
	TRD
	frozen  []trdapi.Freeze
	audited []string
}

func (f *stepTRD) Freeze(_ context.Context, c trdapi.Freeze) (trdapi.FreezeReceipt, error) {
	f.frozen = append(f.frozen, c)
	return trdapi.FreezeReceipt{Receipt: trdapi.Receipt{ActionID: "act-trd", ObjectID: c.QuoteID, RowVersion: c.ExpectedVersion + 1,
		Status: trdapi.StatusFrozen}, BusinessHash: "h"}, nil
}

func (f *stepTRD) AuditRejected(_ context.Context, command, _, code string) error {
	f.audited = append(f.audited, command+" "+code)
	return nil
}

var agentAI = identity.Principal{Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}, Scope: kernel.Scope{Tenant: "t", Company: "c"}}

func newTaskServer(t *testing.T, tsk *fakeTSK, ai *fakeAI, trd *stepTRD) http.Handler {
	t.Helper()
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: struct{ REF }{},
		SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: trd, PUR: struct{ PUR }{}, TSK: tsk, AI: ai,
		Verifier: tokens{"Bearer good": alice, "Bearer agent": agentAI}, Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Every command the Context Map lists for TSK (the 15 of SM-TSK-TASK and ActionExecution's two) is
// routed, and every one answers a receipt that embeds tskapi.Receipt.
func TestEveryTSKCommandIsRouted(t *testing.T) {
	set := tskCommands(struct{ TSK }{})
	var want []string
	for _, i := range []reflect.Type{reflect.TypeFor[tskapi.Commands](), reflect.TypeFor[tskapi.ActionCommands]()} {
		for m := range i.Methods() {
			f, ok := m.Type.Out(0).FieldByName("Receipt")
			if !ok || !f.Anonymous || f.Type != reflect.TypeFor[tskapi.Receipt]() {
				t.Errorf("%s answers %s", m.Name, m.Type.Out(0))
			}
			want = append(want, m.Name)
		}
	}
	if len(set.commands) != len(want) || len(want) != 17 {
		t.Fatalf("%d routed, %d commands", len(set.commands), len(want))
	}
	for name := range set.commands {
		if !set.reconcilable(name) {
			t.Errorf("%s has no reconcile route", name)
		}
	}
}

// TSK's commands decode by reflection into tskapi's structures with A1's and B1's member names; the
// answer is the closed receipt.
func TestTSKCommandsDecodeTheirMembers(t *testing.T) {
	tsk := &fakeTSK{}
	h := newTaskServer(t, tsk, &fakeAI{}, &stepTRD{})
	post := func(command, args string) answer {
		return call(t, h, "POST", "/api/brant/v1/tsk/commands/"+command, "Bearer good",
			`{"contract":"`+tskapi.Contract+`","command":"`+command+`","idempotency_key":"k1","args":`+args+`}`)
	}
	a := post(tskapi.CommandCreate, `{"task_id":"task.1","goal_code":"quote_ready","control_mode":"ai","agent_id":"agent.1","owner_id":"alice",`+
		`"target":{"rfq_id":"rfq.1","quote_id":"quote.1","customer_id":"customer.1"},"source":{"kind":"evd_source","id":"src.1"},`+
		`"deadline":"2026-11-20T00:00:00Z","budget":{"max_model_calls":40,"max_retries":3,"max_cost":{"currency":"USD","minor_units":2,"amount":"3.00"}},`+
		`"execution_bundle":{"bundle_id":"q01.agent","sha256":"`+strings.Repeat("d", 64)+`"}}`)
	c := tsk.commands[0].(tskapi.CreateTask)
	if a.status != 200 || len(a.body) != 6 || a.body["object_id"] != "task.1" || c.IdempotencyKey != "k1" || c.Target.QuoteID != "quote.1" ||
		c.Source.ID != "src.1" || c.Budget.MaxCost.Amount != "3.00" || c.ExecutionBundle.SHA256 != strings.Repeat("d", 64) || c.OwnerID != "alice" {
		t.Fatalf("%d %v %+v", a.status, a.body, c)
	}
	a = post(tskapi.CommandWait, `{"task_id":"task.1","expected_version":3,"wait":"approval","reason":"等审批","responsible_id":"alice",`+
		`"awaiting":{"kind":"ai_approval_grant","id":"p.1","version":2},"items":["command order.prepare"]}`)
	wt := tsk.commands[1].(tskapi.WaitTask)
	if a.status != 200 || wt.ExpectedVersion != 3 || wt.Awaiting.Kind != "ai_approval_grant" || wt.Awaiting.Version != 2 || wt.Items[0] != "command order.prepare" {
		t.Fatalf("%d %v %+v", a.status, a.body, wt)
	}
	a = post(tskapi.CommandTakeControl, `{"task_id":"task.1","reason":"人工接管"}`)
	if a.status != 422 || a.body["code"] != kernel.CodeSchemaInvalid || !slices.Equal(tsk.audited, []string{"task.take_control SCHEMA_INVALID"}) {
		t.Fatalf("expected_version is required: %d %v %v", a.status, a.body, tsk.audited)
	}
	a = post(tskapi.CommandRecordAttempt, `{"task_id":"task.1","business_intent_id":"intent.1","target":{"context":"trd","command":"quote.freeze",`+
		`"request_hash":"`+strings.Repeat("e", 64)+`"},"control_epoch":1,"outcome":"submitted","delegation_id":"del.1"}`)
	ra := tsk.commands[2].(tskapi.RecordAttempt)
	if a.status != 200 || a.body["object_id"] != tskapi.ActionIDOf("intent.1") || a.body["status"] != tskapi.ActionSubmitted ||
		ra.Target.RequestHash != strings.Repeat("e", 64) || ra.ControlEpoch != 1 || ra.DelegationID != "del.1" {
		t.Fatalf("%d %v %+v", a.status, a.body, ra)
	}
	// A task that starts from an uploaded file names the file as its source (S4 G2): the same
	// members, kind evd_file.
	a = post(tskapi.CommandCreate, `{"task_id":"task.2","goal_code":"order_active","control_mode":"ai","agent_id":"agent.1",`+
		`"target":{"rfq_id":"rfq.2","quote_id":"quote.2","customer_id":"customer.1"},"source":{"kind":"evd_file","id":"file.inquiry"}}`)
	fc := tsk.commands[3].(tskapi.CreateTask)
	if a.status != 200 || a.body["object_id"] != "task.2" || fc.Source == nil || *fc.Source != (tskapi.SourceRef{Kind: tskapi.SourceFile, ID: "file.inquiry"}) {
		t.Fatalf("%d %v %+v", a.status, a.body, fc.Source)
	}
}

// TSK's reads: a task with its binding and history, the verifications (none: []), the actions.
func TestTSKReads(t *testing.T) {
	h := newTaskServer(t, &fakeTSK{}, &fakeAI{}, &stepTRD{})
	a := call(t, h, "GET", "/api/brant/v1/tsk/tasks/task.1", "Bearer good", "")
	b, _ := a.body["binding"].(map[string]any)
	if a.status != 200 || a.body["ok"] != true || a.body["control_epoch"] != float64(1) || b["quote_id"] != "quote.1" ||
		b["bound_at"] != "2026-11-13T08:00:00.000000Z" || a.body["initiator"].(map[string]any)["kind"] != "human" {
		t.Fatalf("%d %v", a.status, a.body)
	}
	// The source a task serves, here an uploaded file (S4 G2): kind and id.
	if src, _ := a.body["source"].(map[string]any); len(src) != 2 || src["kind"] != "evd_file" || src["id"] != "file.inquiry" {
		t.Fatalf("source %v", a.body["source"])
	}
	if a = call(t, h, "GET", "/api/brant/v1/tsk/tasks/task.9", "Bearer good", ""); a.status != 404 {
		t.Fatalf("%d %v", a.status, a.body)
	}
	a = call(t, h, "GET", "/api/brant/v1/tsk/tasks/task.1/verifications", "Bearer good", "")
	if v, ok := a.body["verifications"].([]any); a.status != 200 || !ok || len(v) != 0 || a.body["task_id"] != "task.1" {
		t.Fatalf("%d %v", a.status, a.body)
	}
	a = call(t, h, "GET", "/api/brant/v1/tsk/tasks/task.1/actions", "Bearer good", "")
	if v, _ := a.body["actions"].([]any); a.status != 200 || len(v) != 1 || v[0].(map[string]any)["target"].(map[string]any)["command"] != "quote.freeze" {
		t.Fatalf("%d %v", a.status, a.body)
	}
}

// The AI layer's commands: hand-written members, expires_at a contract timestamp (SCHEMA_INVALID
// otherwise), object_version required; refusals before the layer are audited in the AI layer. The
// model and tool calls show money as currency, minor units and amount.
func TestAICommandsAndReads(t *testing.T) {
	f := &fakeAI{}
	h := newTaskServer(t, &fakeTSK{}, f, &stepTRD{})
	post := func(command, args string) answer {
		return call(t, h, "POST", "/api/brant/v1/ai/commands/"+command, "Bearer good",
			`{"contract":"`+ai.Contract+`","command":"`+command+`","idempotency_key":"k1","args":`+args+`}`)
	}
	a := post(policy.CommandGrant, `{"id":"del.1","agent_id":"agent.1","task_id":"task.1","capabilities":["quote.save_draft"],"autonomy":"draft",`+
		`"expires_at":"2026-11-20T00:00:00Z"}`)
	g := f.commands[0].(policy.GrantDelegation)
	if a.status != 200 || a.body["status"] != "active" || g.DelegationID != "del.1" || g.Capabilities[0] != "quote.save_draft" ||
		!g.ExpiresAt.Equal(time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)) || g.IdempotencyKey != "k1" {
		t.Fatalf("%d %v %+v", a.status, a.body, g)
	}
	a = post(policy.CommandGrant, `{"id":"del.2","agent_id":"agent.1","task_id":"task.1","capabilities":["quote.save_draft"],"autonomy":"draft",`+
		`"expires_at":"2026-11-20"}`)
	if a.status != 422 || a.body["code"] != kernel.CodeSchemaInvalid || a.body["field_errors"].([]any)[0].(map[string]any)["path"] != "/args/expires_at" {
		t.Fatalf("%d %v", a.status, a.body)
	}
	issue := `{"id":"grant.1","delegation_id":"del.1","command":"order.prepare","request_hash":"` + strings.Repeat("f", 64) +
		`","object_id":"quote.1","expires_at":"2026-11-14T00:00:00Z"`
	if a = post(approvalgrant.CommandIssue, issue+`}`); a.status != 422 || a.body["field_errors"].([]any)[0].(map[string]any)["path"] != "/args/object_version" {
		t.Fatalf("object_version is required: %d %v", a.status, a.body)
	}
	a = post(approvalgrant.CommandIssue, issue+`,"object_version":0}`)
	ig := f.commands[1].(approvalgrant.IssueGrant)
	if a.status != 200 || a.body["status"] != "issued" || ig.GrantID != "grant.1" || ig.ObjectVersion != 0 || ig.Command != "order.prepare" {
		t.Fatalf("%d %v %+v", a.status, a.body, ig)
	}
	a = post(policy.CommandRevoke, `{"id":"del.1","expected_version":1,"reason":"调岗"}`)
	if rv := f.commands[2].(policy.RevokeDelegation); a.status != 200 || rv.ExpectedVersion != 1 || rv.Reason != "调岗" {
		t.Fatalf("%d %v %+v", a.status, a.body, rv)
	}
	if !slices.Equal(f.audited, []string{"ai.delegation.grant SCHEMA_INVALID", "ai.approval_grant.issue SCHEMA_INVALID"}) {
		t.Fatalf("audited %v", f.audited)
	}
	a = call(t, h, "GET", "/api/brant/v1/ai/tasks/task.1/model_calls", "Bearer good", "")
	calls, _ := a.body["calls"].([]any)
	if a.status != 200 || len(calls) != 1 {
		t.Fatalf("%d %v", a.status, a.body)
	}
	c := calls[0].(map[string]any)
	if c["reserved"].(map[string]any)["amount"] != "0.00" || c["input_tokens"] != float64(120) || c["bundle_id"] != "q01.agent" ||
		c["reserved_at"] != "2026-11-13T08:00:00.000000Z" {
		t.Fatalf("%v", c)
	}
}

// The task-step route (D-2): a person's command of an owning context, decoded as its own route
// decodes it, runs through the execution entry with the business intent; the command's key is the
// intent's action. An AI actor, an object control command, a missing intent, an idempotency key in
// the envelope, another contract and a context a task does not write are refused before the
// execution entry, the refusals audited in the owning context.
func TestTaskStepRunsThroughTheExecutionEntry(t *testing.T) {
	f, trd := &fakeAI{}, &stepTRD{}
	h := newTaskServer(t, &fakeTSK{}, f, trd)
	route := "/api/brant/v1/tsk/tasks/task.1/steps/trd/quote.freeze"
	body := func(command, extra string) string {
		return `{"contract":"` + trdapi.Contract + `","command":"` + command + `",` + extra + `"args":{"quote_id":"quote.1","expected_version":4,"working_revision":1}}`
	}
	a := call(t, h, "POST", route, "Bearer good", body("quote.freeze", `"business_intent_id":"human.freeze.1",`))
	key := tskapi.ActionIDOf("human.freeze.1")
	st := f.steps[0]
	if a.status != 200 || a.body["stage"] != "VERIFIED" || a.body["action_id"] != key || a.body["task_id"] != "task.1" ||
		a.body["result"].(map[string]any)["object_id"] != "quote.1" || a.body["stored"].(map[string]any)["row_version"] != float64(5) ||
		a.body["target"].(map[string]any)["context"] != "trd" || st.TaskID != "task.1" || st.Context != tskapi.TargetTRD || st.Admit != nil ||
		len(trd.frozen) != 1 || trd.frozen[0].IdempotencyKey != key || trd.frozen[0].ExpectedVersion != 4 {
		t.Fatalf("%d %v %+v %+v", a.status, a.body, st, trd.frozen)
	}
	if _, ok := st.Command.(trdapi.Freeze); !ok {
		t.Fatalf("command %T", st.Command)
	}
	for _, c := range []struct {
		name, auth, route, body, code string
		status                        int
	}{
		{"an AI actor", "Bearer agent", route, body("quote.freeze", `"business_intent_id":"i.1",`), kernel.CodePermissionDenied, 403},
		{"object control", "Bearer good", "/api/brant/v1/tsk/tasks/task.1/steps/trd/object_control.release",
			`{"contract":"` + trdapi.Contract + `","command":"object_control.release","business_intent_id":"i.1","args":{}}`, kernel.CodePermissionDenied, 403},
		{"no intent", "Bearer good", route, body("quote.freeze", ""), kernel.CodeSchemaInvalid, 422},
		{"an idempotency key", "Bearer good", route, body("quote.freeze", `"business_intent_id":"i.1","idempotency_key":"k1",`), kernel.CodeSchemaInvalid, 422},
		{"another command", "Bearer good", route, body("quote.activate", `"business_intent_id":"i.1",`), kernel.CodeUnsupportedContract, 400},
		{"another contract", "Bearer good", route, strings.Replace(body("quote.freeze", `"business_intent_id":"i.1",`), trdapi.Contract, "brant.inq/0.1.0", 1),
			kernel.CodeUnsupportedContract, 400},
	} {
		a = call(t, h, "POST", c.route, c.auth, c.body)
		if a.status != c.status || a.body["code"] != c.code {
			t.Errorf("%s: %d %v", c.name, a.status, a.body)
		}
	}
	if len(f.steps) != 1 || len(trd.audited) != 6 {
		t.Fatalf("steps %d, TRD audited %v", len(f.steps), trd.audited)
	}
	if a = call(t, h, "POST", "/api/brant/v1/tsk/tasks/task.1/steps/pur/purchase.draft", "Bearer good", "{}"); a.status != 404 {
		t.Fatalf("a context a task does not write: %d %v", a.status, a.body)
	}
	// The execution entry's refusal is its own (audited where it happened) and keeps its category.
	f.stepErr = kernel.RejectAs(trdapi.CodeControlEpochStale, kernel.CategoryConflict, "/", "")
	a = call(t, h, "POST", route, "Bearer good", body("quote.freeze", `"business_intent_id":"i.2",`))
	if a.status != 409 || a.body["code"] != trdapi.CodeControlEpochStale || len(trd.audited) != 6 {
		t.Fatalf("%d %v %v", a.status, a.body, trd.audited)
	}
	// A lost answer is a transport error: the client sends the same intent again.
	f.stepErr = errors.Join(errors.New("model: unavailable"), errors.New("connection reset"))
	if a = call(t, h, "POST", route, "Bearer good", body("quote.freeze", `"business_intent_id":"i.3",`)); a.status != 503 || a.body["transport_error"] != "UNAVAILABLE" {
		t.Fatalf("%d %v", a.status, a.body)
	}
}
