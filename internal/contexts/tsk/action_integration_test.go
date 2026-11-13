//go:build integration

package tsk_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ActionExecution on PostgreSQL 16 with the targets' action ledgers faked (WBS S4-02,
// TSK-AGG-ActionExecution, TSK-INV-003, X-INV-001; Context Map R18, R66–R69 ActionReconciliation
// v1). The fake ledger of each target context answers a reconciliation by key and request hash as
// the action ledger does (committed with the stored receipt, payload_conflict, not_committed); a
// test commits a command into it as the target would. The composed suite (internal/bootstrap
// tsk_action_integration_test.go) runs TSK against the five contexts themselves.

type ledgerEntry struct {
	hash    string
	receipt api.TargetReceipt
}

// fakeTargets is TSK's Reconcilers: an action ledger per context.
type fakeTargets struct {
	mu     sync.Mutex
	ledger map[string]map[string]ledgerEntry // context → key → entry
	fail   map[string]error                  // context → the refusal or error of its next answers
	asked  []string                          // "context command key" per question
	onAsk  func()                            // runs once, after the next answer
}

func newTargets() *fakeTargets {
	return &fakeTargets{ledger: map[string]map[string]ledgerEntry{}, fail: map[string]error{}}
}

type fakePort struct {
	f       *fakeTargets
	context string
}

func (p fakePort) ReconcileHash(_ context.Context, command, key, hash string) (tsk.Answer, error) {
	f := p.f
	f.mu.Lock()
	f.asked = append(f.asked, p.context+" "+command+" "+key)
	if err := f.fail[p.context]; err != nil {
		f.mu.Unlock()
		return tsk.Answer{}, err
	}
	e, ok := f.ledger[p.context][key]
	hook := f.onAsk
	f.onAsk = nil
	f.mu.Unlock()
	if hook != nil {
		defer hook()
	}
	switch {
	case !ok:
		return tsk.Answer{State: kernel.ReconcileNotCommitted}, nil
	case e.hash != hash:
		return tsk.Answer{State: kernel.ReconcilePayloadConflict}, nil
	}
	r := e.receipt
	return tsk.Answer{State: kernel.ReconcileCommitted, Receipt: &r}, nil
}

func (f *fakeTargets) ports() tsk.Reconcilers {
	out := tsk.Reconcilers{}
	for _, c := range []string{api.TargetEVD, api.TargetINQ, api.TargetSUP, api.TargetCST, api.TargetTRD} {
		out[c] = fakePort{f: f, context: c}
	}
	return out
}

// commit stores a committed command under key in context's ledger, as the target's transaction
// would, and returns its receipt.
func (f *fakeTargets) commit(context, key, hash, object string) api.TargetReceipt {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ledger[context] == nil {
		f.ledger[context] = map[string]ledgerEntry{}
	}
	r := api.TargetReceipt{ActionID: "act-" + key, ObjectID: object, RowVersion: 2, Status: "active"}
	f.ledger[context][key] = ledgerEntry{hash: hash, receipt: r}
	return r
}

func (f *fakeTargets) questions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

var (
	hashA = strings.Repeat("a", 64)
	hashB = strings.Repeat("b", 64)
)

func activation(hash string) api.TargetCommand {
	return api.TargetCommand{Context: api.TargetTRD, Command: "order.activate", RequestHash: hash}
}

func (w *world) submit(ctx context.Context, task, intent string, target api.TargetCommand, epoch int64) (api.ActionReceipt, error) {
	return w.m.RecordAttempt(ctx, api.RecordAttempt{IdempotencyKey: w.key(), TaskID: task, BusinessIntentID: intent, Target: target,
		ControlEpoch: epoch, Outcome: api.ActionSubmitted, DelegationID: "del.1"})
}

func (w *world) observe(ctx context.Context, task, intent string, target api.TargetCommand, outcome, code string) (api.ActionReceipt, error) {
	return w.m.RecordAttempt(ctx, api.RecordAttempt{IdempotencyKey: w.key(), TaskID: task, BusinessIntentID: intent, Target: target,
		Outcome: outcome, Code: code})
}

func (w *world) reconcile(ctx context.Context, task, actionID string) (api.ActionReceipt, error) {
	return w.m.ReconcileAction(ctx, api.ReconcileAction{IdempotencyKey: w.key(), TaskID: task, ActionID: actionID})
}

func (w *world) action(id string) api.ActionView {
	w.t.Helper()
	v, err := w.m.GetAction(w.alice, id)
	if err != nil {
		w.t.Fatal(err)
	}
	return v
}

func (w *world) ok(r api.ActionReceipt, err error) api.ActionReceipt {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}

func logStates(v api.ActionView) []string {
	var out []string
	for _, l := range v.Log {
		out = append(out, l.Command+":"+l.State)
	}
	return out
}

// TSK-INV-003, X-INV-001 and Q01 V0.1 tests/test_execution.py::test_commit_then_real_worker_exit_is_recovered_without_duplicate
// (TSK's half on a fake target): the agent records its attempt before it sends the command — the
// action is registered with the key TSK derives from the business intent, the target is asked by
// key and hash first —; while the attempt is open the task does not verify (EXTERNAL_RESULT_UNKNOWN);
// a replay of the record answers from TSK's ledger without asking the target again; the target
// commits; the reconciliation records its stored receipt and publishes ActionReconciled; the task
// verifies. The log keeps both steps; the reconciliation of TSK's own record is audited as
// tsk.reconcile, apart from the command action.reconcile.
func TestActionLifecycleThroughTheTargetsLedger(t *testing.T) {
	w := newWorld(t)
	r := w.running("task.1")
	key := api.ActionIDOf("intent.1")
	record := api.RecordAttempt{IdempotencyKey: "k.record", TaskID: "task.1", BusinessIntentID: "intent.1", Target: activation(hashA),
		Outcome: api.ActionSubmitted, DelegationID: "del.1", GrantID: "grant.1"}
	a := w.ok(w.m.RecordAttempt(w.agent, record))
	if a.ObjectID != key || a.IdempotencyKey != key || a.RowVersion != 1 || a.Status != api.ActionSubmitted || a.State != api.ActionSubmitted ||
		a.Attempt != 1 || a.Result != nil || a.Replayed {
		t.Fatalf("%+v", a)
	}
	if !slices.Equal(w.target.asked, []string{"trd order.activate " + key}) {
		t.Fatalf("asked %v", w.target.asked)
	}
	_, err := w.m.VerifyTask(w.sys, api.VerifyTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion})
	w.want(err, api.CodeExternalResultUnknown)
	again := w.ok(w.m.RecordAttempt(w.agent, record))
	if !again.Replayed || again.ActionID != a.ActionID || again.State != api.ActionSubmitted || again.Attempt != 1 || w.target.questions() != 1 {
		t.Fatalf("the replay %+v, %d questions", again, w.target.questions())
	}
	changed := record
	changed.GrantID = "grant.2"
	_, err = w.m.RecordAttempt(w.agent, changed)
	w.want(err, kernel.CodeIdempotencyPayloadConflict)

	receipt := w.target.commit(api.TargetTRD, key, hashA, "order.1")
	rec := w.ok(w.reconcile(w.agent, "task.1", key))
	if rec.State != api.ActionCommitted || rec.Status != api.ActionCommitted || rec.Result == nil || *rec.Result != receipt || rec.Attempt != 1 ||
		rec.RowVersion != 2 {
		t.Fatalf("%+v", rec)
	}
	w.must(w.m.VerifyTask(w.sys, api.VerifyTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))

	v := w.action(key)
	if v.BusinessIntentID != "intent.1" || v.TaskID != "task.1" || v.Target != activation(hashA) || v.IdempotencyKey != key || v.Version != 2 ||
		v.Attempts != 1 || v.RegisteredBy.ID != "agent.1" || *v.Result != receipt ||
		!slices.Equal(logStates(v), []string{"action.record_attempt:submitted", "action.reconcile:committed"}) {
		t.Fatalf("%+v", v)
	}
	if l := v.Log[0]; l.DelegationID != "del.1" || l.GrantID != "grant.1" || l.Answer != "not_committed" || l.Actor.ID != "agent.1" ||
		l.AuthorityEpoch < 1 || l.Result != nil {
		t.Fatalf("%+v", l)
	}
	if l := v.Log[1]; l.Answer != "committed" || *l.Result != receipt || l.Attempt != 1 {
		t.Fatalf("%+v", l)
	}
	list, err := w.m.TaskActions(w.agent, "task.1")
	if err != nil || len(list) != 1 || list[0].ActionID != key {
		t.Fatalf("%+v %v", list, err)
	}
	evs := w.events()
	last := evs[len(evs)-1]
	if last.Type != api.EventActionReconciled || last.Payload["aggregate_id"] != key || last.Payload["task_id"] != "task.1" ||
		last.Payload["business_intent_id"] != "intent.1" || last.Payload["state"] != "committed" ||
		last.Payload["reconcile_state"] != "committed" || last.Payload["target_object_id"] != "order.1" || last.Payload["attempt"] != float64(1) {
		t.Fatalf("%+v", last)
	}

	// TSK's own record reconciles as committed, audited under tsk.reconcile.
	rc, err := w.m.Reconcile(w.agent, record)
	if err != nil || rc.State != kernel.ReconcileCommitted || rc.ObjectID != key {
		t.Fatalf("%+v %v", rc, err)
	}
	audit, err := w.m.Audit(w.alice)
	if err != nil {
		t.Fatal(err)
	}
	var trail []string
	for _, e := range audit {
		if strings.HasPrefix(e.Command, "action.") || e.Command == api.ReconcileAudit {
			trail = append(trail, e.Command+" "+e.Outcome)
		}
	}
	if !slices.Equal(trail, []string{"action.record_attempt succeeded", "action.record_attempt replayed", "action.record_attempt rejected",
		"action.reconcile succeeded", "action.get read", "action.list read", "tsk.reconcile read"}) {
		t.Fatalf("%v", trail)
	}
}

// Q01 V0.1 tests/test_execution.py::test_same_intent_cannot_be_registered_as_new_action, CT11 and
// TSK-INV-003's concurrency: the same intent with another command or for another task is
// IDEMPOTENCY_PAYLOAD_CONFLICT; while its attempt is open, another attempt is EXTERNAL_RESULT_UNKNOWN;
// once it committed, a new plan revision recording it again is answered with the original result —
// no new attempt, nothing sent, the target not asked. Executors racing to register one intent get
// one action and one attempt.
func TestSameIntentIsOneAction(t *testing.T) {
	w := newWorld(t)
	w.running("task.1")
	w.running("task.2")
	key := api.ActionIDOf("intent.1")
	w.ok(w.submit(w.agent, "task.1", "intent.1", activation(hashA), 0))
	for name, call := range map[string]func() error{
		"another command": func() error {
			_, err := w.submit(w.agent, "task.1", "intent.1", api.TargetCommand{Context: api.TargetTRD, Command: "order.prepare", RequestHash: hashA}, 0)
			return err
		},
		"another request": func() error { _, err := w.submit(w.agent, "task.1", "intent.1", activation(hashB), 0); return err },
		"another task":    func() error { _, err := w.submit(w.agent, "task.2", "intent.1", activation(hashA), 0); return err },
	} {
		if err := call(); code(err) != kernel.CodeIdempotencyPayloadConflict {
			t.Errorf("%s: %s", name, code(err))
		}
	}
	_, err := w.submit(w.alice, "task.1", "intent.1", activation(hashA), 0)
	w.want(err, api.CodeExternalResultUnknown)
	if v := w.action(key); v.Version != 1 || v.Attempts != 1 {
		t.Fatalf("%+v", v)
	}
	receipt := w.target.commit(api.TargetTRD, key, hashA, "order.1")
	w.ok(w.reconcile(w.alice, "task.1", key))
	asked := w.target.questions()
	redo := w.ok(w.submit(w.agent, "task.1", "intent.1", activation(hashA), 0))
	if redo.State != api.ActionCommitted || *redo.Result != receipt || redo.Attempt != 1 || redo.RowVersion != 2 || w.target.questions() != asked {
		t.Fatalf("the new plan's record %+v, %d questions", redo, w.target.questions()-asked)
	}
	if v := w.action(key); v.Version != 2 || v.Attempts != 1 || len(v.Log) != 2 {
		t.Fatalf("%+v", v)
	}
	_, err = w.reconcile(w.alice, "task.1", key)
	w.want(err, kernel.CodeInvalidState)

	const n = 6
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := range n {
		executor := w.agent
		if i%2 == 1 {
			executor = w.alice
		}
		k := "k.race." + string(rune('a'+i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = w.m.RecordAttempt(executor, api.RecordAttempt{IdempotencyKey: k, TaskID: "task.2", BusinessIntentID: "intent.2",
				Target: activation(hashA), Outcome: api.ActionSubmitted})
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		switch code(err) {
		case "OK":
			won++
		case api.CodeExternalResultUnknown:
		default:
			t.Fatalf("a racing record: %v", err)
		}
	}
	list, err := w.m.TaskActions(w.alice, "task.2")
	if err != nil || won != 1 || len(list) != 1 || list[0].Attempts != 1 || list[0].Version != 1 {
		t.Fatalf("%d records won, actions %+v %v", won, list, err)
	}
}

// Q01 V0.1 tests/test_execution.py::test_unknown_external_record_blocks_takeover_barrier and
// TSK-INV-003's negative: the agent's attempt on the bound root is submitted, then its answer is
// lost (unknown); a person takes over; the stop barrier is EXTERNAL_RESULT_UNKNOWN until the action
// is reconciled — the agent cannot submit while the task stops, the executor may still record what
// it saw —; the person's reconciliation finds nothing committed, the barrier completes under manual
// control; the agent is then CONTROL_MODE_MISMATCH and the person's step needs the new claim epoch.
func TestUnknownAttemptBlocksTheTakeOverBarrier(t *testing.T) {
	w := newWorld(t)
	r := w.bound("task.1", "quote.1")
	key := api.ActionIDOf("intent.1")
	w.ok(w.submit(w.agent, "task.1", "intent.1", activation(hashA), r.ControlEpoch))
	lost := w.ok(w.observe(w.agent, "task.1", "intent.1", activation(hashA), api.ActionUnknown, "COMMIT_UNCERTAIN"))
	if lost.State != api.ActionUnknown || lost.Attempt != 1 {
		t.Fatalf("%+v", lost)
	}
	r = w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "模型持续不可用"}))
	finish := api.FinishStop{IdempotencyKey: "k.finish", TaskID: "task.1", ExpectedVersion: r.RowVersion}
	_, err := w.m.FinishStop(w.sys, finish)
	w.want(err, api.CodeExternalResultUnknown)
	_, err = w.submit(w.agent, "task.1", "intent.2", activation(hashB), r.ControlEpoch)
	w.want(err, kernel.CodeInvalidState)
	_, err = w.submit(w.alice, "task.1", "intent.2", activation(hashB), r.ControlEpoch)
	w.want(err, kernel.CodeInvalidState)
	if ops := w.trd.ops(); !slices.Equal(ops, []string{"bind", "stop"}) {
		t.Fatalf("TRD %v before the reconciliation", ops)
	}
	rec := w.ok(w.reconcile(w.alice, "task.1", key))
	if rec.State != api.ActionNotCommitted || rec.Attempt != 1 {
		t.Fatalf("%+v", rec)
	}
	r = w.must(w.m.FinishStop(w.sys, finish))
	if r.Status != api.StatePaused || r.ControlMode != api.ModeManual || r.ControlEpoch != 3 {
		t.Fatalf("%+v", r)
	}
	_, err = w.submit(w.agent, "task.1", "intent.1", activation(hashA), 3)
	w.want(err, api.CodeControlModeMismatch)
	_, err = w.submit(w.alice, "task.1", "intent.1", activation(hashA), 1)
	w.want(err, api.CodeControlEpochStale)
	resent := w.ok(w.submit(w.alice, "task.1", "intent.1", activation(hashA), 3))
	if resent.State != api.ActionSubmitted || resent.Attempt != 2 || resent.IdempotencyKey != key {
		t.Fatalf("%+v", resent)
	}
	if v := w.action(key); !slices.Equal(logStates(v), []string{"action.record_attempt:submitted", "action.record_attempt:unknown",
		"action.reconcile:not_committed", "action.record_attempt:submitted"}) || v.Log[1].Code != "COMMIT_UNCERTAIN" || v.Log[3].ControlEpoch != 3 ||
		v.Log[0].ControlEpoch != 1 || v.Log[2].Actor.ID != "alice" {
		t.Fatalf("%+v", v.Log)
	}
}

// D-2 and TSK-INV-004 on a submission: the actor holds the task's claim — an AI actor that is not
// the task's agent is PERMISSION_DENIED, the agent of a task that is not scoping or running
// INVALID_STATE, nobody on an ended task —, at the claim's control epoch (CONTROL_EPOCH_STALE
// otherwise); a system actor executes no task step; a task the caller does not see is NOT_FOUND
// (another company, own data scope). Refusals record nothing and are audited.
func TestAttemptNeedsTheTasksClaim(t *testing.T) {
	w := newWorld(t)
	r := w.bound("task.1", "quote.1")
	w.create("task.2")
	carol := w.actor("company.A", kernel.ActorHuman, "carol", authorization.DataScopeOwn, "tsk.read", "action.record_attempt", "action.reconcile")
	for name, c := range map[string]struct {
		ctx   context.Context
		task  string
		epoch int64
		code  string
	}{
		"another AI actor":        {w.rogue, "task.1", 1, kernel.CodePermissionDenied},
		"a system actor":          {w.sys, "task.1", 1, kernel.CodePermissionDenied},
		"another company":         {w.other, "task.1", 1, kernel.CodeNotFound},
		"own data scope":          {carol, "task.1", 1, kernel.CodeNotFound},
		"a stale claim":           {w.agent, "task.1", 0, api.CodeControlEpochStale},
		"a claim from the future": {w.alice, "task.1", 2, api.CodeControlEpochStale},
		"the agent while created": {w.agent, "task.2", 0, kernel.CodeInvalidState},
		"an unknown task":         {w.alice, "task.9", 0, kernel.CodeNotFound},
	} {
		if _, err := w.submit(c.ctx, c.task, "intent."+strings.ReplaceAll(name, " ", "."), activation(hashA), c.epoch); code(err) != c.code {
			t.Errorf("%s: %s, want %s", name, code(err), c.code)
		}
	}
	if w.target.questions() != 0 {
		t.Fatalf("refused records asked the target %v", w.target.asked)
	}
	w.ok(w.submit(w.alice, "task.2", "intent.human", activation(hashA), 0)) // a person on a created task
	w.ok(w.submit(w.agent, "task.1", "intent.agent", activation(hashA), r.ControlEpoch))
	for _, task := range []string{"task.1", "task.2"} {
		list, err := w.m.TaskActions(w.alice, task)
		if err != nil || len(list) != 1 {
			t.Fatalf("%s: %+v %v", task, list, err)
		}
	}
	_, err := w.m.TaskActions(carol, "task.1")
	w.want(err, kernel.CodeNotFound)
	_, err = w.m.GetAction(w.other, api.ActionIDOf("intent.agent"))
	w.want(err, kernel.CodeNotFound)

	// An ended task takes no new attempt.
	u := w.running("task.3")
	u = w.must(w.m.CloseUnfulfilled(w.alice, api.CloseUnfulfilled{IdempotencyKey: w.key(), TaskID: "task.3", ExpectedVersion: u.RowVersion,
		Cause: api.CloseNotPursued, Reason: "不再推进"}))
	for _, ctx := range []context.Context{w.alice, w.agent} {
		_, err = w.submit(ctx, "task.3", "intent.late", activation(hashA), 0)
		w.want(err, kernel.CodeInvalidState)
	}
	audit, err := w.m.Audit(w.alice)
	if err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, e := range audit {
		if e.Command == api.CommandRecordAttempt && e.Outcome == "rejected" {
			refused++
		}
	}
	if refused != 9 { // company.A's trail; the company.B actor's refusal is in company.B's
		t.Fatalf("%d audited refusals", refused)
	}
}

// Baseline 17.2 (not_committed ⇒ send the same key again) and the executor's observations: a
// rejection is recorded with the target's code while the target holds nothing; the action is
// settled, a second observation is INVALID_STATE; the next submission is attempt 2 under the same
// key; when the target committed what the executor reports as lost, TSK records the commit.
func TestRejectedAttemptIsSentAgainWithTheSameKey(t *testing.T) {
	w := newWorld(t)
	r := w.running("task.1")
	key := api.ActionIDOf("intent.1")
	inq := api.TargetCommand{Context: api.TargetINQ, Command: "rfq.save_draft", RequestHash: hashA}
	w.ok(w.submit(w.agent, "task.1", "intent.1", inq, 0))
	rej := w.ok(w.observe(w.agent, "task.1", "intent.1", inq, api.ActionRejected, "VERSION_CONFLICT"))
	if rej.State != api.ActionRejected || rej.Attempt != 1 {
		t.Fatalf("%+v", rej)
	}
	// Settled: the task verifies; the executor's second observation has no submitted attempt.
	r = w.must(w.m.VerifyTask(w.sys, api.VerifyTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	_, err := w.observe(w.agent, "task.1", "intent.1", inq, api.ActionRejected, "VERSION_CONFLICT")
	w.want(err, kernel.CodeInvalidState)
	// The agent acts while the task is scoping or running; a person sends the step again.
	_, err = w.submit(w.agent, "task.1", "intent.1", inq, 0)
	w.want(err, kernel.CodeInvalidState)
	again := w.ok(w.submit(w.alice, "task.1", "intent.1", inq, 0))
	if again.Attempt != 2 || again.IdempotencyKey != key || again.State != api.ActionSubmitted {
		t.Fatalf("%+v", again)
	}
	receipt := w.target.commit(api.TargetINQ, key, hashA, "rfq.1")
	got := w.ok(w.observe(w.agent, "task.1", "intent.1", inq, api.ActionUnknown, ""))
	if got.State != api.ActionCommitted || *got.Result != receipt || got.Attempt != 2 {
		t.Fatalf("%+v", got)
	}
	if v := w.action(key); !slices.Equal(logStates(v), []string{"action.record_attempt:submitted", "action.record_attempt:rejected",
		"action.record_attempt:submitted", "action.record_attempt:committed"}) || v.Log[1].Code != "VERSION_CONFLICT" || v.Log[3].Code != "" {
		t.Fatalf("%v", logStates(v))
	}
}

// The target's ledger is the truth: a key it holds before TSK registered the intent registers the
// action committed (attempt 0) or in conflict, which is final — nothing is reconciled or sent again;
// the target's refusal of the request's form lands at TSK's member (an unknown command at
// /args/target/command), its PERMISSION_DENIED and its errors pass on, and none records anything.
func TestTargetAnswersAreTheTruth(t *testing.T) {
	w := newWorld(t)
	w.running("task.1")
	committed := api.ActionIDOf("intent.committed")
	receipt := w.target.commit(api.TargetEVD, committed, hashA, "src.1")
	src := api.TargetCommand{Context: api.TargetEVD, Command: "source.register", RequestHash: hashA}
	a := w.ok(w.submit(w.agent, "task.1", "intent.committed", src, 0))
	if a.State != api.ActionCommitted || a.Attempt != 0 || *a.Result != receipt {
		t.Fatalf("%+v", a)
	}
	conflict := api.ActionIDOf("intent.conflict")
	w.target.commit(api.TargetCST, conflict, hashB, "context.1")
	cst := api.TargetCommand{Context: api.TargetCST, Command: "costing_context.save_draft", RequestHash: hashA}
	c := w.ok(w.submit(w.agent, "task.1", "intent.conflict", cst, 0))
	if c.State != api.ActionConflict || c.Result != nil {
		t.Fatalf("%+v", c)
	}
	_, err := w.reconcile(w.agent, "task.1", conflict)
	w.want(err, kernel.CodeInvalidState)
	asked := w.target.questions()
	if again := w.ok(w.submit(w.agent, "task.1", "intent.conflict", cst, 0)); again.State != api.ActionConflict || w.target.questions() != asked {
		t.Fatalf("%+v", again)
	}
	reconciled := 0
	for _, e := range w.events() {
		if e.Type == api.EventActionReconciled {
			reconciled++
		}
	}
	if reconciled != 2 {
		t.Fatalf("%d ActionReconciled", reconciled)
	}

	sup := api.TargetCommand{Context: api.TargetSUP, Command: "supplier_offer.save_draft", RequestHash: hashA}
	for name, c := range map[string]struct {
		fail error
		code string
		path string
	}{
		"an unknown command":     {kernel.Reject(kernel.CodeSchemaInvalid, "/args/command", ""), kernel.CodeSchemaInvalid, "/args/target/command"},
		"no read permission":     {kernel.Reject(kernel.CodePermissionDenied, "/", ""), kernel.CodePermissionDenied, "/"},
		"outside the data scope": {kernel.Reject(kernel.CodeNotFound, "/args/idempotency_key", ""), kernel.CodeNotFound, "/args/idempotency_key"},
	} {
		w.target.fail[api.TargetSUP] = c.fail
		_, err := w.submit(w.agent, "task.1", "intent.sup", sup, 0)
		if rej, ok := kernel.AsRejection(err); !ok || rej.Code != c.code || rej.Fields[0].Path != c.path {
			t.Errorf("%s: %v", name, err)
		}
	}
	w.target.fail[api.TargetSUP] = errors.New("connection refused")
	if _, err = w.submit(w.agent, "task.1", "intent.sup", sup, 0); err == nil || code(err) != "ERROR: connection refused" {
		t.Fatalf("an infrastructure error: %v", err)
	}
	if _, err = w.m.GetAction(w.alice, api.ActionIDOf("intent.sup")); code(err) != kernel.CodeNotFound {
		t.Fatalf("a refused record left an action: %s", code(err))
	}
}

// TSK's own record survives a lost COMMIT (BC-AT18/BC-AT19 for TSK's step, pgtest.CommitFault): the
// answer of a committed record is lost — TSK reconciles its own command as committed and the same
// key replays —; a record whose COMMIT never reached the server is not_committed and the same key
// records it once.
func TestRecordSurvivesALostCommit(t *testing.T) {
	w := newWorld(t)
	w.running("task.1")
	fault := &pgtest.CommitFault{}
	db, err := bentopg.NewDB(pgtest.FaultyAppPool(t, fault))
	if err != nil {
		t.Fatal(err)
	}
	m, err := tsk.New(db, tsk.Upstream{Control: w.trd, Readiness: w.trd, Reconcilers: w.target.ports()})
	if err != nil {
		t.Fatal(err)
	}
	for i, mode := range []pgtest.FaultMode{pgtest.LoseAnswer, pgtest.LoseCommit} {
		intent := "intent.lost." + string(rune('1'+i))
		record := api.RecordAttempt{IdempotencyKey: "k.lost." + string(rune('1'+i)), TaskID: "task.1", BusinessIntentID: intent,
			Target: activation(hashA), Outcome: api.ActionSubmitted}
		// The fault is armed when the target answered, so the next COMMIT is the record's own.
		w.target.onAsk = func() { fault.Arm(mode) }
		_, err := m.RecordAttempt(w.agent, record)
		if !errors.Is(err, application.ErrCommitUncertain) || !fault.Fired() {
			t.Fatalf("%v: %v (fired %v)", mode, err, fault.Fired())
		}
		rec, err := m.Reconcile(w.agent, record)
		want := kernel.ReconcileCommitted
		if mode == pgtest.LoseCommit {
			want = kernel.ReconcileNotCommitted
		}
		if err != nil || rec.State != want || fault.Committed() != (mode == pgtest.LoseAnswer) {
			t.Fatalf("%v: %+v %v", mode, rec, err)
		}
		again, err := m.RecordAttempt(w.agent, record)
		if err != nil || again.Replayed != (mode == pgtest.LoseAnswer) || again.State != api.ActionSubmitted || again.Attempt != 1 {
			t.Fatalf("%v: the same key %+v %v", mode, again, err)
		}
		if v := w.action(api.ActionIDOf(intent)); v.Version != 1 || v.Attempts != 1 {
			t.Fatalf("%v: %+v", mode, v)
		}
	}
}

// A record and a take-over meet on the task row (the record holds it FOR SHARE, the take-over FOR
// UPDATE): either the attempt is recorded first and the barrier waits for it, or the take-over is
// first and the attempt is refused; no attempt is recorded behind a completed barrier.
func TestAttemptAndTakeOverSerializeOnTheTask(t *testing.T) {
	w := newWorld(t)
	outcomes := map[string]int{}
	for i := range 5 {
		task := "task.r" + string(rune('0'+i))
		r := w.bound(task, "quote."+task)
		var wg sync.WaitGroup
		var recordErr, takeErr error
		var took api.TaskReceipt
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, recordErr = w.m.RecordAttempt(w.agent, api.RecordAttempt{IdempotencyKey: "k.rec." + task, TaskID: task,
				BusinessIntentID: "intent." + task, Target: activation(hashA), ControlEpoch: r.ControlEpoch, Outcome: api.ActionSubmitted})
		}()
		go func() {
			defer wg.Done()
			// Later starts in later rounds, so both orders occur; the assertions hold for either.
			time.Sleep(time.Duration(i) * 4 * time.Millisecond)
			took, takeErr = w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: "k.take." + task, TaskID: task,
				ExpectedVersion: r.RowVersion, Reason: "接管"})
		}()
		wg.Wait()
		if takeErr != nil {
			t.Fatal(takeErr)
		}
		_, finishErr := w.m.FinishStop(w.sys, api.FinishStop{IdempotencyKey: "k.finish." + task, TaskID: task, ExpectedVersion: took.RowVersion})
		switch code(recordErr) {
		case "OK":
			w.want(finishErr, api.CodeExternalResultUnknown)
		case kernel.CodeInvalidState:
			w.want(finishErr, "OK")
		default:
			t.Fatalf("the record: %v", recordErr)
		}
		outcomes[code(recordErr)]++
	}
	t.Logf("records against take-overs: %v", outcomes)
}

// The database keeps the action's rules too (defense in depth): a state without its receipt, a
// version that skips, a move the action never makes, a changed registration, a key that is not the
// action's ID, a change without its log row, a deletion and a change of the log are refused.
func TestDatabaseKeepsTheActionLog(t *testing.T) {
	w := newWorld(t)
	w.running("task.1")
	id := api.ActionIDOf("intent.1")
	w.ok(w.submit(w.agent, "task.1", "intent.1", activation(hashA), 0))
	w.ok(w.observe(w.agent, "task.1", "intent.1", activation(hashA), api.ActionRejected, "VERSION_CONFLICT"))
	where := ` WHERE action_id = '` + id + `'`
	for _, stmt := range []string{
		`UPDATE brant_tsk.action_execution SET state = 'committed', action_version = action_version + 1` + where,
		`UPDATE brant_tsk.action_execution SET state = 'not_committed', action_version = action_version + 2` + where,
		`UPDATE brant_tsk.action_execution SET state = 'unknown', action_version = action_version + 1` + where,
		`UPDATE brant_tsk.action_execution SET state = 'submitted', action_version = action_version + 1` + where,
		`UPDATE brant_tsk.action_execution SET request_hash = '` + hashB + `', state = 'not_committed', action_version = action_version + 1` + where,
		`UPDATE brant_tsk.action_execution SET state = 'not_committed', action_version = action_version + 1` + where,
		`DELETE FROM brant_tsk.action_execution` + where,
		`UPDATE brant_tsk.action_attempt SET code = 'X_Y'` + where,
		`DELETE FROM brant_tsk.action_attempt` + where,
		`INSERT INTO brant_tsk.action_execution (tenant_id, company_id, action_id, business_intent_id, task_id, target_context, target_command,
			idempotency_key, request_hash, state, action_version, attempts, registered_by_kind, registered_by_id, created_at, updated_at)
			SELECT tenant_id, company_id, 'tska.other', 'intent.other', task_id, target_context, target_command, 'tska.another', request_hash,
			'submitted', 1, 1, registered_by_kind, registered_by_id, created_at, updated_at FROM brant_tsk.action_execution` + where,
	} {
		err := w.sql(stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code[:2] != "23" && pgErr.Code != "42501") {
			t.Errorf("%s: %v", stmt, err)
		}
	}
	if v := w.action(id); v.State != api.ActionRejected || v.Version != 2 || len(v.Log) != 2 {
		t.Fatalf("%+v", v)
	}
}

// Refusals before the ledger: a record refused by its closed contract is audited; reads of a
// malformed ID are NOT_FOUND.
func TestActionContractRefusals(t *testing.T) {
	w := newWorld(t)
	w.running("task.1")
	_, err := w.m.RecordAttempt(w.agent, api.RecordAttempt{IdempotencyKey: w.key(), TaskID: "task.1", BusinessIntentID: "intent.1",
		Target: api.TargetCommand{Context: "pur", Command: "purchase.draft.prepare", RequestHash: hashA}, Outcome: api.ActionSubmitted})
	w.want(err, kernel.CodeSchemaInvalid)
	_, err = w.m.ReconcileAction(w.agent, api.ReconcileAction{IdempotencyKey: w.key(), TaskID: "task.1"})
	w.want(err, kernel.CodeSchemaInvalid)
	_, err = w.reconcile(w.agent, "task.1", api.ActionIDOf("intent.none"))
	w.want(err, kernel.CodeNotFound)
	_, err = w.m.GetAction(w.alice, "bad id")
	w.want(err, kernel.CodeNotFound)
	_, err = w.m.TaskActions(w.alice, "")
	w.want(err, kernel.CodeNotFound)
	if w.target.questions() != 0 {
		t.Fatal("a refused command asked a target")
	}
	var actionErr error
	_, actionErr = w.m.RecordAttempt(context.Background(), api.RecordAttempt{})
	if actionErr == nil {
		t.Fatal("a command without a principal")
	}
}
