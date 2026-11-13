//go:build integration

package tsk_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TSK on PostgreSQL 16 with its ports to TRD faked (WBS S4-01, SM-TSK-TASK, TSK-INV-001, TSK-INV-002,
// TSK-INV-004; owner decisions D-1, D-2). The fake TRD answers each control command with the next
// control epoch of the root and replays a key it has seen, as TRD's ledger does; the composed suite
// (internal/bootstrap tsk_*_integration_test.go) runs TSK against TRD itself.

// fakeTRD is TSK's ObjectControl and Readiness ports.
type fakeTRD struct {
	mu      sync.Mutex
	epoch   map[string]int64 // quote → control epoch
	keys    map[string]int64 // key → the epoch it was answered with
	calls   []call
	fail    map[string]error
	hook    func(op string) // runs once after the first, not replayed, call of an operation
	verdict tsk.Goal
	assess  func(tsk.Goal) (string, []string)
	full    func(tsk.Goal) (tsk.Verdict, error) // a whole assessment or a refusal (verification_integration_test.go)
}

type call struct {
	Op, Key, Task, Quote string
	Initiator, Agent     string
	Epoch, Answer        int64
	Claimed              bool
	Replay               bool
}

func newFake() *fakeTRD {
	return &fakeTRD{epoch: map[string]int64{}, keys: map[string]int64{}, fail: map[string]error{}}
}

func (f *fakeTRD) do(ctx context.Context, op string, r tsk.ControlRequest) (int64, error) {
	f.mu.Lock()
	if err := f.fail[op]; err != nil {
		f.mu.Unlock()
		return 0, err
	}
	c := call{Op: op, Key: r.Key, Task: r.TaskID, Quote: r.QuoteID, Epoch: r.Epoch, Initiator: r.Initiator, Agent: r.Agent}
	if e, ok := f.keys[r.Key]; ok {
		c.Answer, c.Replay = e, true
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		return e, nil
	}
	f.epoch[r.QuoteID]++
	e := f.epoch[r.QuoteID]
	f.keys[r.Key], c.Answer = e, e
	f.calls = append(f.calls, c)
	hook := f.hook
	f.hook = nil
	f.mu.Unlock()
	if hook != nil {
		hook(op)
	}
	return e, nil
}

func (f *fakeTRD) Bind(ctx context.Context, r tsk.ControlRequest) (int64, error) {
	return f.do(ctx, "bind", r)
}
func (f *fakeTRD) Stop(ctx context.Context, r tsk.ControlRequest) (int64, error) {
	return f.do(ctx, "stop", r)
}
func (f *fakeTRD) EnterManual(ctx context.Context, r tsk.ControlRequest) (int64, error) {
	return f.do(ctx, "enter_manual", r)
}
func (f *fakeTRD) Handoff(ctx context.Context, r tsk.ControlRequest) (int64, error) {
	return f.do(ctx, "handoff", r)
}
func (f *fakeTRD) Release(ctx context.Context, r tsk.ControlRequest) (int64, error) {
	return f.do(ctx, "release", r)
}

func (f *fakeTRD) Assess(_ context.Context, g tsk.Goal) (verdict tsk.Verdict, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verdict = g
	if f.full != nil {
		return f.full(g)
	}
	if f.assess == nil {
		return verdict, errors.New("no assessment scripted")
	}
	outcome, unmet := f.assess(g)
	return f.scripted(g, outcome, unmet), nil
}

func (f *fakeTRD) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		op := c.Op
		if c.Replay {
			op += "(replay)"
		}
		out = append(out, op)
	}
	return out
}

var taskPermissions = []string{"task.create", "task.scope", "task.wait", "task.verify", "goal.verify", "task.pause", "task.resume",
	"task.take_control", "task.handoff", "task.cancel", "task.finish_stop", "task.close_unfulfilled", "task.fail", "task.bind_object",
	"task.release", "tsk.read", "tsk.audit", "action.record_attempt", "action.reconcile"}

type world struct {
	t      *testing.T
	db     *bentopg.DB
	m      *tsk.Module
	trd    *fakeTRD
	target *fakeTargets // the action ledgers of EVD, INQ, SUP, CST and TRD (action_integration_test.go)
	tenant string
	serial int
	alice  context.Context // human owner, company data scope
	bob    context.Context // human, own data scope, reads only
	sys    context.Context // system:tsk, company data scope
	agent  context.Context // the tasks' AI agent: tsk.read
	rogue  context.Context // another AI actor holding every task permission
	other  context.Context // a human of company.B with every permission
}

func newWorld(t *testing.T) *world {
	t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	if err != nil {
		t.Fatal(err)
	}
	f, targets := newFake(), newTargets()
	m, err := tsk.New(db, tsk.Upstream{Control: f, Readiness: f, Reconcilers: targets.ports()})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, db: db, m: m, trd: f, target: targets, tenant: pgtest.UniqueID(t, "tenant.")}
	w.alice = w.actor("company.A", kernel.ActorHuman, "alice", authorization.DataScopeCompany, taskPermissions...)
	w.bob = w.actor("company.A", kernel.ActorHuman, "bob", "", "tsk.read")
	w.sys = w.actor("company.A", kernel.ActorSystem, "tsk", authorization.DataScopeCompany, taskPermissions...)
	w.agent = w.actor("company.A", kernel.ActorAI, "agent.1", "", "tsk.read", "action.record_attempt", "action.reconcile")
	w.rogue = w.actor("company.A", kernel.ActorAI, "agent.2", authorization.DataScopeCompany, taskPermissions...)
	w.other = w.actor("company.B", kernel.ActorHuman, "alice", authorization.DataScopeCompany, taskPermissions...)
	return w
}

func (w *world) scope(company string) kernel.Scope {
	return kernel.Scope{Tenant: kernel.TenantID(w.tenant), Company: kernel.CompanyID(company)}
}

func (w *world) actor(company string, kind kernel.ActorKind, id string, data authorization.DataScope, permissions ...string) context.Context {
	w.t.Helper()
	sc, ref := w.scope(company), kernel.ActorRef{Kind: kind, ID: id}
	err := bento.ScopeTransactor{DB: w.db, Scope: sc}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := w.db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		g := authorization.Grants{}
		if err = g.Provision(ctx, exec, sc); err != nil {
			return err
		}
		if _, err = g.Put(ctx, exec, sc, ref, append([]string{}, permissions...), time.Now()); err != nil {
			return err
		}
		if data != "" {
			_, err = g.SetDataScope(ctx, exec, sc, id, data, "", time.Now())
		}
		return err
	})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: ref, Scope: sc})
	if err != nil {
		w.t.Fatal(err)
	}
	return action.WithCorrelation(ctx, pgtest.UniqueID(w.t, "corr."))
}

func (w *world) key() string {
	w.serial++
	return "k" + strconv.Itoa(w.serial)
}

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code
	}
	if err != nil {
		return "ERROR: " + err.Error()
	}
	return "OK"
}

func (w *world) want(err error, c string) {
	w.t.Helper()
	if got := code(err); got != c {
		w.t.Fatalf("want %s, got %s", c, got)
	}
}

func (w *world) must(r api.TaskReceipt, err error) api.TaskReceipt {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

func (w *world) get(id string) api.TaskView {
	w.t.Helper()
	v, err := w.m.GetTask(w.alice, id)
	if err != nil {
		w.t.Fatal(err)
	}
	return v
}

func (w *world) create(id string, edit ...func(*api.CreateTask)) api.TaskReceipt {
	w.t.Helper()
	c := api.CreateTask{IdempotencyKey: w.key(), TaskID: id, GoalCode: api.GoalOrderActive, ControlMode: api.ModeAI, AgentID: "agent.1",
		Budget: &api.Budget{MaxModelCalls: 40, MaxRetries: 3, MaxCost: &api.CostCap{Currency: "USD", MinorUnits: 2, Amount: "3.00"}}}
	for _, e := range edit {
		e(&c)
	}
	return w.must(w.m.CreateTask(w.alice, c))
}

// running creates task id and scopes it twice: created → scoping (system) → running (human).
func (w *world) running(id string, edit ...func(*api.CreateTask)) api.TaskReceipt {
	w.t.Helper()
	r := w.create(id, edit...)
	r = w.must(w.m.ScopeTask(w.sys, api.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion}))
	return w.must(w.m.ScopeTask(w.alice, api.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, Note: "范围确认"}))
}

// bound creates task id targeting quote, scopes it and binds quote to it (the target names the root:
// SM-TSK-TASK task.bind_object; the root an action of the task created is created's case).
func (w *world) bound(id, quote string) api.TaskReceipt {
	w.t.Helper()
	r := w.running(id, func(c *api.CreateTask) { c.Target.QuoteID = quote })
	return w.must(w.m.BindObject(w.sys, api.BindObject{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, QuoteID: quote}))
}

type event struct {
	Type    string
	Payload map[string]any
}

func (w *world) events() []event {
	w.t.Helper()
	q, err := w.db.ReadExecutor(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	rows, err := q.Query(context.Background(), `SELECT event_type, payload FROM bento.outbox WHERE source = $1 AND scope = $2
		ORDER BY enqueued_seq`, api.Source, w.scope("company.A").String())
	if err != nil {
		w.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (event, error) {
		var e event
		return e, row.Scan(&e.Type, &e.Payload)
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

func types(evs []event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// sql runs statement as the application role in company.A's scope.
func (w *world) sql(statement string) error {
	return bento.ScopeTransactor{DB: w.db, Scope: w.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := w.db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		_, err = exec.Exec(ctx, statement)
		return err
	})
}

// A task's whole life on PostgreSQL 16 through registered commands only (SM-TSK-TASK, D-1): created →
// scoping → running, bound, waiting for the customer, resumed, verified; an unsatisfied goal sends it
// back to waiting_input with the unmet assertions (TSK-INV-001), the satisfied one completes it with
// completion actor ai_with_required_review; release gives the root back. Every change is one task
// version with its history row, and the facts are TaskCreated, TaskControlChanged and TaskCompleted.
func TestTaskLifecycleThroughRegisteredCommands(t *testing.T) {
	w := newWorld(t)
	r := w.create("task.1", func(c *api.CreateTask) {
		c.Target.RfqID, c.Target.QuoteID, c.Source = "rfq.1", "quote.1", &api.SourceRef{Kind: api.SourceRFQ, ID: "rfq.1"}
		c.Deadline = kernel.FormatTimestamp(time.Now().Add(72 * time.Hour))
		c.ExecutionBundle = &api.BundleRef{BundleID: "bundle.1", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	})
	if r.Status != api.StateCreated || r.RowVersion != 1 || r.ControlMode != api.ModeAI || r.ControlEpoch != 0 {
		t.Fatalf("%+v", r)
	}
	v := w.get("task.1")
	if v.OwnerID != "alice" || v.Initiator != (kernel.ActorRef{Kind: kernel.ActorHuman, ID: "alice"}) || v.AgentID != "agent.1" ||
		v.GoalVersion != 1 || v.Budget.MaxCost.Amount != "3.00" || v.ExecutionBundle.BundleID != "bundle.1" || v.Source.ID != "rfq.1" {
		t.Fatalf("%+v", v)
	}
	w.must(w.m.ScopeTask(w.sys, api.ScopeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: 1}))
	r = w.must(w.m.ScopeTask(w.alice, api.ScopeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: 2, Note: "范围确认"}))
	r = w.must(w.m.BindObject(w.sys, api.BindObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, QuoteID: "quote.1"}))
	if r.Status != api.StateRunning || r.ControlEpoch != 1 || r.BoundQuoteID != "quote.1" {
		t.Fatalf("bind %+v", r)
	}
	c, err := w.m.Claim(w.agent, "task.1")
	if err != nil || c.ControlEpoch != 1 || c.QuoteID != "quote.1" || c.ControlMode != api.ModeAI || c.Actor.ID != "agent.1" {
		t.Fatalf("agent claim %+v %v", c, err)
	}
	r = w.must(w.m.WaitTask(w.sys, api.WaitTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Wait: api.WaitExternal, Reason: "等待客户确认", ResponsibleID: "alice", Awaiting: &api.ObjectRef{Kind: "customer_response", ID: "quote.1"},
		DueAt: kernel.FormatTimestamp(time.Now().Add(48 * time.Hour))}))
	if v := w.get("task.1"); v.Wait == nil || v.Wait.Awaiting.Kind != "customer_response" || v.Wait.ResponsibleID != "alice" {
		t.Fatalf("%+v", v.Wait)
	}
	// While the task waits for the customer, its agent gets no claim; people do (D-2).
	_, err = w.m.Claim(w.agent, "task.1")
	w.want(err, kernel.CodeInvalidState)
	if _, err = w.m.Claim(w.alice, "task.1"); err != nil {
		t.Fatal(err)
	}
	r = w.must(w.m.ResumeTask(w.sys, api.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Arrival: &api.ObjectRef{Kind: "acceptance", ID: "acc.1", Version: 1}}))
	r = w.must(w.m.VerifyTask(w.sys, api.VerifyTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	w.trd.assess = func(tsk.Goal) (string, []string) { return "unsatisfied", []string{"Q01.order"} }
	r = w.must(w.m.VerifyGoal(w.sys, api.VerifyGoal{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	if v := w.get("task.1"); r.Status != api.StateWaitingInput || !slices.Equal(v.Wait.Items, []string{"Q01.order"}) || v.CompletionActor != "" {
		t.Fatalf("%+v %+v", r, v.Wait)
	}
	if g := w.trd.verdict; g.Code != api.GoalOrderActive || g.Version != 1 || g.Target.QuoteID != "quote.1" || g.Target.RfqID != "rfq.1" {
		t.Fatalf("readiness asked %+v", g)
	}
	r = w.must(w.m.ResumeTask(w.sys, api.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Arrival: &api.ObjectRef{Kind: "order", ID: "order.1", Version: 2}}))
	r = w.must(w.m.VerifyTask(w.sys, api.VerifyTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	w.trd.assess = func(tsk.Goal) (string, []string) { return "satisfied", nil }
	r = w.must(w.m.VerifyGoal(w.sys, api.VerifyGoal{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	if r.Status != api.StateCompleted {
		t.Fatalf("%+v", r)
	}
	// Q01 V0.1 tests/test_execution.py::test_terminal_task_rejects_new_forward_action, BC-AT25: the
	// completed task issues no claim and takes no forward command; the completion stays.
	for _, ctx := range []context.Context{w.agent, w.alice} {
		_, err = w.m.Claim(ctx, "task.1")
		w.want(err, kernel.CodeInvalidState)
	}
	_, err = w.m.PauseTask(w.alice, api.PauseTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Cause: api.PauseOperator,
		Reason: "late", ResumeCondition: "never"})
	w.want(err, kernel.CodeInvalidState)
	_, err = w.m.CancelTask(w.alice, api.CancelTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "late"})
	w.want(err, kernel.CodeInvalidState)
	r = w.must(w.m.ReleaseObject(w.alice, api.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "完成"}))
	v = w.get("task.1")
	if r.Status != api.StateCompleted || r.BoundQuoteID != "" || r.ControlEpoch != 2 || v.Binding.State != api.BindingReleased ||
		v.CompletionActor != api.CompletionAIWithRequiredReview {
		t.Fatalf("%+v %+v", r, v)
	}
	if got := w.trd.ops(); !slices.Equal(got, []string{"bind", "release"}) {
		t.Fatalf("TRD calls %v", got)
	}
	last := w.trd.calls[1]
	if last.Epoch != 1 || last.Task != "task.1" || last.Quote != "quote.1" {
		t.Fatalf("release %+v", last)
	}
	var cmds []string
	for i, h := range v.History {
		if h.TaskVersion != int64(i+1) || (i > 0 && h.From != v.History[i-1].To) {
			t.Fatalf("history %d: %+v", i, h)
		}
		cmds = append(cmds, h.Command)
	}
	if !slices.Equal(cmds, []string{"task.create", "task.scope", "task.scope", "task.bind_object", "task.wait", "task.resume", "task.verify",
		"goal.verify", "task.resume", "task.verify", "goal.verify", "task.release"}) || v.TaskVersion != 12 {
		t.Fatalf("history %v", cmds)
	}
	if got := types(w.events()); !slices.Equal(got, []string{api.EventTaskCreated, api.EventTaskControlChanged, api.EventTaskCompleted,
		api.EventTaskControlChanged}) {
		t.Fatalf("events %v", got)
	}
	done := w.events()[2].Payload
	if done["task_id"] != "task.1" || done["completion_actor"] != api.CompletionAIWithRequiredReview || done["task_version"] != float64(11) {
		t.Fatalf("%v", done)
	}
}

// No TSK command admits an AI actor, even one granted every task permission (AI-INV-004: model output
// never changes a task; X-INV-002 for the human-only ones). The refusals are audited and change
// nothing.
func TestNoCommandAdmitsAnAIActor(t *testing.T) {
	w := newWorld(t)
	r := w.running("task.1")
	v := r.RowVersion
	calls := map[string]func() error{
		api.CommandCreate: func() error {
			_, err := w.m.CreateTask(w.rogue, api.CreateTask{IdempotencyKey: w.key(), TaskID: "task.2", GoalCode: api.GoalQuoteReady,
				ControlMode: api.ModeManual, OwnerID: "alice"})
			return err
		},
		api.CommandScope: func() error {
			_, err := w.m.ScopeTask(w.rogue, api.ScopeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v})
			return err
		},
		api.CommandWait: func() error {
			_, err := w.m.WaitTask(w.rogue, api.WaitTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v, Wait: api.WaitInput,
				Reason: "r", ResponsibleID: "alice", Items: []string{"x"}})
			return err
		},
		api.CommandVerify: func() error {
			_, err := w.m.VerifyTask(w.rogue, api.VerifyTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v})
			return err
		},
		api.CommandVerifyGoal: func() error {
			_, err := w.m.VerifyGoal(w.rogue, api.VerifyGoal{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v})
			return err
		},
		api.CommandPause: func() error {
			_, err := w.m.PauseTask(w.rogue, api.PauseTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v, Cause: api.PauseOperator,
				Reason: "r", ResumeCondition: "c"})
			return err
		},
		api.CommandResume: func() error {
			_, err := w.m.ResumeTask(w.rogue, api.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v, Reason: "r"})
			return err
		},
		api.CommandTakeControl: func() error {
			_, err := w.m.TakeControl(w.rogue, api.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v, Reason: "r"})
			return err
		},
		api.CommandHandoff: func() error {
			_, err := w.m.Handoff(w.rogue, api.Handoff{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v, Reason: "r"})
			return err
		},
		api.CommandCancel: func() error {
			_, err := w.m.CancelTask(w.rogue, api.CancelTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v, Reason: "r"})
			return err
		},
		api.CommandFinishStop: func() error {
			_, err := w.m.FinishStop(w.rogue, api.FinishStop{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v})
			return err
		},
		api.CommandCloseUnfulfilled: func() error {
			_, err := w.m.CloseUnfulfilled(w.rogue, api.CloseUnfulfilled{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v,
				Cause: api.CloseNotPursued, Reason: "r"})
			return err
		},
		api.CommandFail: func() error {
			_, err := w.m.FailTask(w.rogue, api.FailTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v, Reason: "r",
				ResponsibleID: "alice"})
			return err
		},
		api.CommandBindObject: func() error {
			_, err := w.m.BindObject(w.rogue, api.BindObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v, QuoteID: "quote.1"})
			return err
		},
		api.CommandRelease: func() error {
			_, err := w.m.ReleaseObject(w.rogue, api.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: v, Reason: "r"})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); code(err) != kernel.CodePermissionDenied {
			t.Errorf("%s by an AI actor: %s", name, code(err))
		}
	}
	if got := w.get("task.1"); got.TaskVersion != v || len(w.trd.ops()) != 0 {
		t.Fatalf("the task changed: %+v", got)
	}
	audit, err := w.m.Audit(w.alice)
	if err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, e := range audit {
		if e.Actor.ID == "agent.2" && e.Outcome == "rejected" && e.Code == kernel.CodePermissionDenied {
			refused++
		}
	}
	if refused != len(calls) {
		t.Fatalf("%d audited refusals", refused)
	}
}

// The transition's own actor rule and state decide after the command's rule (SM-TSK-TASK): a human
// does not resume a waiting task (system), a system actor does not resume a paused one (human), no
// command applies from a state it has no row for (INVALID_STATE), a stale version is
// VERSION_CONFLICT; each refusal leaves the task as it was.
func TestTransitionRefusalsOnPostgres(t *testing.T) {
	w := newWorld(t)
	w.create("task.1")
	_, err := w.m.VerifyTask(w.sys, api.VerifyTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: 1})
	w.want(err, kernel.CodeInvalidState)
	_, err = w.m.ScopeTask(w.alice, api.ScopeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: 1})
	w.want(err, kernel.CodePermissionDenied) // created → scoping is the system's
	r := w.running("task.2")
	r = w.must(w.m.WaitTask(w.sys, api.WaitTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion, Wait: api.WaitInput,
		Reason: "缺数量", ResponsibleID: "alice", Items: []string{"数量"}}))
	_, err = w.m.ResumeTask(w.alice, api.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion,
		Arrival: &api.ObjectRef{Kind: "input", ID: "x"}})
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.m.ResumeTask(w.sys, api.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion})
	w.want(err, kernel.CodeSchemaInvalid) // a waiting task resumes on what arrived
	r = w.must(w.m.PauseTask(w.sys, api.PauseTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion,
		Cause: api.PauseModelUnavailable, Reason: "模型不可用", ResumeCondition: "模型恢复"}))
	_, err = w.m.ResumeTask(w.sys, api.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion, Reason: "恢复"})
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.m.ResumeTask(w.alice, api.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion - 1, Reason: "恢复"})
	w.want(err, kernel.CodeVersionConflict)
	_, err = w.m.WaitTask(w.sys, api.WaitTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion, Wait: api.WaitInput,
		Reason: "x", ResponsibleID: "nobody", Items: []string{"x"}})
	w.want(err, kernel.CodeInvalidState) // paused has no task.wait
	before := w.get("task.2")
	r = w.must(w.m.ResumeTask(w.alice, api.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion, Reason: "模型已恢复"}))
	if r.Status != api.StateWaitingInput || before.PausedFrom != api.StateWaitingInput || before.Pause.Cause != api.PauseModelUnavailable ||
		before.Wait.Items[0] != "数量" {
		t.Fatalf("%+v %+v", r, before)
	}
	if after := w.get("task.2"); after.Pause != nil || after.Wait == nil || after.Wait.Items[0] != "数量" {
		t.Fatalf("the wait was not restored: %+v", after)
	}
	// A responsible person must be an active human of the company.
	_, err = w.m.FailTask(w.alice, api.FailTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion, Reason: "坏",
		ResponsibleID: "agent.1"})
	w.want(err, kernel.CodeReferenceInvalid)
	r = w.must(w.m.FailTask(w.alice, api.FailTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion, Reason: "无法恢复",
		ResponsibleID: "bob"}))
	if v := w.get("task.2"); r.Status != api.StateFailed || v.Termination.ResponsibleID != "bob" || v.Termination.By.ID != "alice" {
		t.Fatalf("%+v", v.Termination)
	}
	_, err = w.m.ResumeTask(w.alice, api.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion, Reason: "再试"})
	w.want(err, kernel.CodeInvalidState)
}

// Replays and conflicts: the same key and request answers the stored receipt; the same key with
// another request is IDEMPOTENCY_PAYLOAD_CONFLICT; a task ID taken by another request is
// BUSINESS_KEY_CONFLICT, by the same request of the same initiator a replay; GOAL_UNSUPPORTED for a
// goal P0 does not support; the owner and agent must hold grants of their kinds.
func TestReplaysAndConflicts(t *testing.T) {
	w := newWorld(t)
	c := api.CreateTask{IdempotencyKey: "create.1", TaskID: "task.1", GoalCode: api.GoalQuoteReady, ControlMode: api.ModeAI, AgentID: "agent.1"}
	first := w.must(w.m.CreateTask(w.alice, c))
	again := w.must(w.m.CreateTask(w.alice, c))
	if !again.Replayed || again.ActionID != first.ActionID {
		t.Fatalf("%+v %+v", first, again)
	}
	other := c
	other.IdempotencyKey = "create.2"
	if r := w.must(w.m.CreateTask(w.alice, other)); !r.Replayed || r.ObjectID != "task.1" || r.ActionID == first.ActionID {
		t.Fatalf("same request under another key: %+v", r)
	}
	other.IdempotencyKey, other.GoalCode = "create.3", api.GoalOrderActive
	_, err := w.m.CreateTask(w.alice, other)
	w.want(err, api.CodeBusinessKeyConflict)
	changed := c
	changed.GoalCode = api.GoalOrderActive
	_, err = w.m.CreateTask(w.alice, changed)
	w.want(err, kernel.CodeIdempotencyPayloadConflict)
	bad := c
	bad.IdempotencyKey, bad.TaskID, bad.GoalCode = "create.4", "task.9", "order_shipped"
	_, err = w.m.CreateTask(w.alice, bad)
	w.want(err, api.CodeGoalUnsupported)
	for _, tc := range []struct {
		edit func(*api.CreateTask)
		code string
	}{
		{func(c *api.CreateTask) { c.AgentID = "bob" }, kernel.CodeReferenceInvalid},       // a human is no AI agent
		{func(c *api.CreateTask) { c.OwnerID = "agent.1" }, kernel.CodeReferenceInvalid},   // an AI is no owner
		{func(c *api.CreateTask) { c.OwnerID = "nobody.1" }, kernel.CodeReferenceInvalid},  // no grant at all
		{func(c *api.CreateTask) { c.AgentID = "agent.404" }, kernel.CodeReferenceInvalid}, // no grant at all
	} {
		x := c
		x.IdempotencyKey, x.TaskID = w.key(), "task.x"
		tc.edit(&x)
		_, err = w.m.CreateTask(w.alice, x)
		w.want(err, tc.code)
	}
	// A system initiator names the owner.
	sys := c
	sys.IdempotencyKey, sys.TaskID = w.key(), "task.s"
	_, err = w.m.CreateTask(w.sys, sys)
	w.want(err, kernel.CodeSchemaInvalid)
	sys.OwnerID = "bob"
	if r := w.must(w.m.CreateTask(w.sys, sys)); r.Status != api.StateCreated {
		t.Fatal(r)
	}
	// Replaying a command after the task moved on still answers its stored receipt.
	s := api.ScopeTask{IdempotencyKey: "scope.1", TaskID: "task.1", ExpectedVersion: 1}
	r1 := w.must(w.m.ScopeTask(w.sys, s))
	w.must(w.m.ScopeTask(w.alice, api.ScopeTask{IdempotencyKey: "scope.2", TaskID: "task.1", ExpectedVersion: 2}))
	s.ExpectedVersion = 7
	r2 := w.must(w.m.ScopeTask(w.sys, s))
	if !r2.Replayed || r2.ActionID != r1.ActionID || r2.RowVersion != 2 || r2.Status != api.StateScoping {
		t.Fatalf("%+v", r2)
	}
	rec, err := w.m.Reconcile(w.alice, s)
	if _, err := w.m.Reconcile(w.bob, s); code(err) != kernel.CodeNotFound {
		t.Fatalf("a reader who does not see the task: %v", err)
	}
	if err != nil || rec.State != kernel.ReconcileCommitted || rec.ActionID != r1.ActionID {
		t.Fatalf("%+v %v", rec, err)
	}
}

// 来源防重 (SM-TSK-TASK created→scoping, AN06): one open task serves a goal for a source, also when
// two are created at once; once it has ended, a new task may serve the source.
func TestOneOpenTaskPerGoalAndSource(t *testing.T) {
	w := newWorld(t)
	src := func(c *api.CreateTask) { c.Source = &api.SourceRef{Kind: api.SourceEVD, ID: "inquiry.1"} }
	var wg sync.WaitGroup
	errs := make([]error, 4)
	keys := []string{"race.a", "race.b", "race.c", "race.d"}
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := api.CreateTask{IdempotencyKey: keys[i], TaskID: "task.r" + keys[i][5:], GoalCode: api.GoalOrderActive, ControlMode: api.ModeAI,
				AgentID: "agent.1"}
			src(&c)
			_, errs[i] = w.m.CreateTask(w.alice, c)
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch code(err) {
		case "OK":
			won++
		case api.CodeBusinessKeyConflict:
		default:
			t.Fatalf("%v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d tasks serve the source", won)
	}
	// Another goal for the same source is another task.
	w.create("task.q", func(c *api.CreateTask) { src(c); c.GoalCode = api.GoalQuoteReady })
	var open api.TaskView
	for _, id := range []string{"task.ra", "task.rb", "task.rc", "task.rd"} {
		if v, err := w.m.GetTask(w.alice, id); err == nil {
			open = v
		}
	}
	r := w.must(w.m.ScopeTask(w.sys, api.ScopeTask{IdempotencyKey: w.key(), TaskID: open.TaskID, ExpectedVersion: 1}))
	r = w.must(w.m.ScopeTask(w.sys, api.ScopeTask{IdempotencyKey: w.key(), TaskID: open.TaskID, ExpectedVersion: r.RowVersion}))
	w.must(w.m.CloseUnfulfilled(w.sys, api.CloseUnfulfilled{IdempotencyKey: w.key(), TaskID: open.TaskID, ExpectedVersion: r.RowVersion,
		Cause: api.CloseCustomerDeclined, Reason: "客户拒绝"}))
	w.create("task.next", src)
	if got := types(w.events()); got[len(got)-2] != api.EventTaskClosedUnfulfilled {
		t.Fatalf("events %v", got)
	}
}

// task_version CAS on PostgreSQL 16: of two commands sent at the same version, one commits and the
// other is VERSION_CONFLICT; nothing is lost or doubled in the history.
func TestConcurrentCommandsOnOneTask(t *testing.T) {
	w := newWorld(t)
	for i := range 5 {
		id := "task.c" + string(rune('0'+i))
		r := w.running(id)
		var wg sync.WaitGroup
		var pauseErr, cancelErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, pauseErr = w.m.PauseTask(w.sys, api.PauseTask{IdempotencyKey: "pause." + id, TaskID: id, ExpectedVersion: r.RowVersion,
				Cause: api.PauseToolUnavailable, Reason: "工具不可用", ResumeCondition: "工具恢复"})
		}()
		go func() {
			defer wg.Done()
			_, cancelErr = w.m.CancelTask(w.alice, api.CancelTask{IdempotencyKey: "cancel." + id, TaskID: id, ExpectedVersion: r.RowVersion,
				Reason: "客户撤销"})
		}()
		wg.Wait()
		codes := []string{code(pauseErr), code(cancelErr)}
		slices.Sort(codes)
		if !slices.Equal(codes, []string{"OK", kernel.CodeVersionConflict}) {
			t.Fatalf("%v %v", pauseErr, cancelErr)
		}
		v := w.get(id)
		if v.TaskVersion != r.RowVersion+1 || len(v.History) != int(v.TaskVersion) {
			t.Fatalf("%+v", v)
		}
	}
}

// TSK-INV-002 and SM-TSK-TASK take_control: the bound root is stopped at TRD first; when TSK's own
// step then fails, the root stays fenced, and the take-over sent again sends TRD the same key, which
// TRD answers from its first result: one stop, its epoch recorded once.
func TestTakeOverAfterAFailedSecondStepReplaysTRDsStop(t *testing.T) {
	w := newWorld(t)
	r := w.bound("task.1", "quote.1")
	// Between TRD's stop and TSK's record, the system pauses the task: TSK's step meets another version.
	w.trd.hook = func(op string) {
		if op != "stop" {
			return
		}
		w.must(w.m.PauseTask(w.sys, api.PauseTask{IdempotencyKey: "pause.1", TaskID: "task.1", ExpectedVersion: r.RowVersion,
			Cause: api.PauseModelUnavailable, Reason: "模型不可用", ResumeCondition: "模型恢复"}))
	}
	_, err := w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: "take.1", TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "人工接管"})
	w.want(err, kernel.CodeVersionConflict)
	v := w.get("task.1")
	if v.Lifecycle != api.StatePaused || v.ControlEpoch != 1 || w.trd.epoch["quote.1"] != 2 {
		t.Fatalf("after the failed step: %+v, TRD epoch %d", v, w.trd.epoch["quote.1"])
	}
	// The paused task's claim still names epoch 1, which TRD has fenced out.
	if c, err := w.m.Claim(w.alice, "task.1"); err != nil || c.ControlEpoch != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	r = w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: "take.2", TaskID: "task.1", ExpectedVersion: v.TaskVersion, Reason: "人工接管"}))
	if r.Status != api.StateStopping || r.ControlEpoch != 2 || w.trd.epoch["quote.1"] != 2 {
		t.Fatalf("%+v", r)
	}
	if got := w.trd.ops(); !slices.Equal(got, []string{"bind", "stop", "stop(replay)"}) || w.trd.calls[1].Key != w.trd.calls[2].Key {
		t.Fatalf("TRD calls %v", w.trd.calls)
	}
	v = w.get("task.1")
	if v.Stop.Purpose != api.StopTakeControl || v.Stop.From != api.StateRunning || v.Stop.RequestedBy.ID != "alice" {
		t.Fatalf("stop %+v", v.Stop)
	}
}

// The stop barrier (task.finish_stop, SM-TSK-TASK, baseline 16.1): a take-over pauses the task under
// manual control after TRD's enter_manual — the agent then gets CONTROL_MODE_MISMATCH, people get the
// claim of TRD's new epoch —; a hand-back returns it to its agent after TRD's handoff; a cancellation
// ends it without TRD, and task.release then gives the root back. An unbound task never calls TRD.
func TestStopBarrierPaths(t *testing.T) {
	w := newWorld(t)
	r := w.bound("task.1", "quote.1")
	r = w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "模型持续不可用"}))
	_, err := w.m.Claim(w.alice, "task.1")
	w.want(err, kernel.CodeInvalidState)
	_, err = w.m.FinishStop(w.alice, api.FinishStop{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion})
	w.want(err, kernel.CodePermissionDenied) // the barrier is the system's
	r = w.must(w.m.FinishStop(w.sys, api.FinishStop{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	if r.Status != api.StatePaused || r.ControlMode != api.ModeManual || r.ControlEpoch != 3 {
		t.Fatalf("%+v", r)
	}
	_, err = w.m.Claim(w.agent, "task.1")
	w.want(err, api.CodeControlModeMismatch)
	if c, err := w.m.Claim(w.alice, "task.1"); err != nil || c.ControlEpoch != 3 || c.ControlMode != api.ModeManual {
		t.Fatalf("%+v %v", c, err)
	}
	r = w.must(w.m.Handoff(w.alice, api.Handoff{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "模型已恢复"}))
	r = w.must(w.m.FinishStop(w.sys, api.FinishStop{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	if r.Status != api.StatePaused || r.ControlMode != api.ModeAI || r.ControlEpoch != 5 {
		t.Fatalf("%+v", r)
	}
	r = w.must(w.m.ResumeTask(w.alice, api.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "交回 AI"}))
	if c, err := w.m.Claim(w.agent, "task.1"); err != nil || c.ControlEpoch != 5 || r.Status != api.StateRunning {
		t.Fatalf("%+v %v", c, err)
	}
	r = w.must(w.m.CancelTask(w.alice, api.CancelTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "客户撤销"}))
	r = w.must(w.m.FinishStop(w.sys, api.FinishStop{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	if r.Status != api.StateCancelled || r.ControlEpoch != 6 || r.BoundQuoteID != "quote.1" {
		t.Fatalf("%+v", r)
	}
	r = w.must(w.m.ReleaseObject(w.alice, api.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Reason: "已取消"}))
	if r.Status != api.StateCancelled || r.ControlEpoch != 7 || r.BoundQuoteID != "" {
		t.Fatalf("%+v", r)
	}
	if got := w.trd.ops(); !slices.Equal(got, []string{"bind", "stop", "enter_manual", "stop", "handoff", "stop", "release"}) {
		t.Fatalf("TRD calls %v", got)
	}
	// The bind and the hand-back name the task's delegation (D-5): its owner and its agent.
	for _, i := range []int{0, 4} {
		if c := w.trd.calls[i]; c.Initiator != "alice" || c.Agent != "agent.1" {
			t.Fatalf("%s names %s and %s", c.Op, c.Initiator, c.Agent)
		}
	}
	for _, c := range w.trd.calls[1:] {
		if c.Epoch != c.Answer-1 || c.Task != "task.1" {
			t.Fatalf("call %+v does not claim the epoch the task recorded", c)
		}
	}
	var changes []string
	for _, h := range w.get("task.1").History {
		if h.ControlChange != "" {
			changes = append(changes, h.ControlChange+"@"+string(rune('0'+h.ControlEpoch)))
		}
	}
	if !slices.Equal(changes, []string{"bind@1", "take_control@2", "finish_stop@3", "handoff@4", "finish_stop@5", "cancel@6", "finish_stop@6",
		"release@7"}) {
		t.Fatalf("control history %v", changes)
	}

	// Unbound: take-over and barrier without TRD.
	n := len(w.trd.ops())
	u := w.running("task.2")
	u = w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: u.RowVersion, Reason: "接管"}))
	u = w.must(w.m.FinishStop(w.sys, api.FinishStop{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: u.RowVersion}))
	if u.ControlMode != api.ModeManual || u.ControlEpoch != 0 || len(w.trd.ops()) != n {
		t.Fatalf("%+v %v", u, w.trd.ops())
	}
	_, err = w.m.ReleaseObject(w.alice, api.ReleaseObject{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: u.RowVersion, Reason: "x"})
	w.want(err, kernel.CodeInvalidState)
}

// A TRD refusal is passed on unchanged, audited, and leaves the task as it was; goal.verify without
// TRD's GoalReadiness v1 answers DEPENDENCY_NOT_SUPPORTED.
func TestTRDRefusalsLeaveTheTaskUnchanged(t *testing.T) {
	w := newWorld(t)
	r := w.running("task.1", func(c *api.CreateTask) { c.Target.QuoteID = "quote.1" })
	w.trd.fail["bind"] = kernel.RejectAs("OBJECT_CONTROL_CONFLICT", kernel.CategoryConflict, "/args/quote_id", "bound to another task")
	_, err := w.m.BindObject(w.sys, api.BindObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, QuoteID: "quote.1"})
	w.want(err, "OBJECT_CONTROL_CONFLICT")
	if v := w.get("task.1"); v.TaskVersion != r.RowVersion || v.Binding != nil {
		t.Fatalf("%+v", v)
	}
	r = w.must(w.m.VerifyTask(w.sys, api.VerifyTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := tsk.New(db, tsk.Upstream{Control: w.trd, Readiness: tsk.ReadinessNotWired(), Reconcilers: w.target.ports()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = plain.VerifyGoal(w.sys, api.VerifyGoal{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion})
	w.want(err, api.CodeDependencyNotSupported)
	if v := w.get("task.1"); v.Lifecycle != api.StateVerifying || v.TaskVersion != r.RowVersion {
		t.Fatalf("%+v", v)
	}
	// A verdict TSK does not know is no pass.
	w.trd.assess = func(tsk.Goal) (string, []string) { return "unknown", []string{"Q01.document"} }
	r = w.must(w.m.VerifyGoal(w.sys, api.VerifyGoal{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	if r.Status != api.StateWaitingExternal {
		t.Fatalf("%+v", r)
	}
}

// Visibility and scope (X-INV-003, BC-AT21): a human with own data scope who is neither owner,
// initiator nor agent does not see the task; the task's agent does; a principal of another company
// never does — its own task of the same ID is another task.
func TestVisibilityAndScope(t *testing.T) {
	w := newWorld(t)
	w.running("task.1")
	_, err := w.m.GetTask(w.bob, "task.1")
	w.want(err, kernel.CodeNotFound)
	_, err = w.m.Claim(w.bob, "task.1")
	w.want(err, kernel.CodeNotFound)
	if v, err := w.m.GetTask(w.agent, "task.1"); err != nil || v.TaskID != "task.1" {
		t.Fatalf("%v", err)
	}
	_, err = w.m.GetTask(w.other, "task.1")
	w.want(err, kernel.CodeNotFound)
	_, err = w.m.ScopeTask(w.other, api.ScopeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: 3})
	w.want(err, kernel.CodeNotFound)
	if r := w.must(w.m.CreateTask(w.other, api.CreateTask{IdempotencyKey: w.key(), TaskID: "task.1", GoalCode: api.GoalQuoteReady,
		ControlMode: api.ModeManual})); r.RowVersion != 1 {
		t.Fatal(r)
	}
	// A system actor gets no claim; nor does an AI actor that is not the task's agent.
	_, err = w.m.Claim(w.sys, "task.1")
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.m.Claim(w.rogue, "task.1")
	w.want(err, kernel.CodePermissionDenied)
}

// The database keeps SM-TSK-TASK too (defense in depth): a direct update to an unlisted state, a
// version that skips, a changed creation field, a change without its history row, a deletion and a
// change of the history are refused.
func TestDatabaseKeepsTheStateMachine(t *testing.T) {
	w := newWorld(t)
	w.running("task.1")
	for _, stmt := range []string{
		`UPDATE brant_tsk.business_task SET lifecycle = 'completed', completion_actor = 'manual', task_version = task_version + 1 WHERE task_id = 'task.1'`,
		`UPDATE brant_tsk.business_task SET lifecycle = 'verifying', task_version = task_version + 2 WHERE task_id = 'task.1'`,
		`UPDATE brant_tsk.business_task SET goal_code = 'quote_ready', task_version = task_version + 1, lifecycle = 'verifying' WHERE task_id = 'task.1'`,
		`UPDATE brant_tsk.business_task SET lifecycle = 'verifying', task_version = task_version + 1 WHERE task_id = 'task.1'`,
		`UPDATE brant_tsk.business_task SET completion_actor = 'ai_unassisted' WHERE task_id = 'task.1'`,
		`DELETE FROM brant_tsk.business_task WHERE task_id = 'task.1'`,
		`UPDATE brant_tsk.task_transition SET reason = 'x' WHERE task_id = 'task.1'`,
		`DELETE FROM brant_tsk.task_transition WHERE task_id = 'task.1'`,
	} {
		err := w.sql(stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code[:2] != "23" && pgErr.Code != "42501") {
			t.Errorf("%s: %v", stmt, err)
		}
	}
	if v := w.get("task.1"); v.Lifecycle != api.StateRunning || v.TaskVersion != 3 {
		t.Fatalf("%+v", v)
	}
}
