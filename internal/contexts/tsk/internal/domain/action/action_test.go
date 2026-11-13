package action

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	agent   = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}
	target  = Target{Context: "trd", Command: "order.activate", RequestHash: strings.Repeat("a", 64)}
	receipt = &Result{ActionID: "act-1", ObjectID: "order.1", RowVersion: 3, Status: "active"}
	now     = time.Date(2026, 11, 13, 8, 0, 0, 123456789, time.UTC)

	notCommitted = Answer{State: kernel.ReconcileNotCommitted}
	committed    = Answer{State: kernel.ReconcileCommitted, Result: receipt}
	conflicting  = Answer{State: kernel.ReconcilePayloadConflict}
)

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code
	}
	if err != nil {
		return "ERROR"
	}
	return "OK"
}

func act() Act { return Act{By: agent, Epoch: 4, ControlEpoch: 1, At: now} }

func submit(intent string) Request {
	return Request{Intent: intent, TaskID: "task.1", Target: target, Outcome: Submitted, DelegationID: "del.1", GrantID: "grant.1"}
}

// The action of a business intent has one ID, which is the idempotency key of its command: a valid
// identifier, the same for every attempt and plan, another for another intent (baseline 17.1).
func TestIDIsTheIntentsKey(t *testing.T) {
	id := ID("intent.1")
	if !kernel.ValidIdentifier(id) || id != ID("intent.1") || id == ID("intent.2") || !strings.HasPrefix(id, "tska.") || len(id) != 45 {
		t.Fatalf("%q", id)
	}
	e, _, err := Record(nil, submit("intent.1"), notCommitted, act())
	if err != nil || e.ID != id || e.Key() != id {
		t.Fatalf("%+v %v", e, err)
	}
}

// TSK-INV-003: the first submission registers the action and opens attempt 1; the step records the
// delegation and grant the attempt named, the claim's control epoch, the actor and the authority
// epoch, at microseconds.
func TestFirstSubmissionRegistersTheIntent(t *testing.T) {
	e, s, err := Record(nil, submit("intent.1"), notCommitted, act())
	if err != nil {
		t.Fatal(err)
	}
	if e.State != Submitted || e.Version != 1 || e.Attempts != 1 || e.Intent != "intent.1" || e.TaskID != "task.1" || e.Target != target ||
		e.RegisteredBy != agent || !e.CreatedAt.Equal(kernel.Instant(now)) || e.Result != nil {
		t.Fatalf("%+v", e)
	}
	if s.Version != 1 || s.Attempt != 1 || s.Command != CmdRecordAttempt || s.State != Submitted || s.Answer != kernel.ReconcileNotCommitted ||
		s.DelegationID != "del.1" || s.GrantID != "grant.1" || s.ControlEpoch != 1 || s.Epoch != 4 || s.By != agent || s.At.Nanosecond()%1000 != 0 {
		t.Fatalf("%+v", s)
	}
}

// Q01 V0.1 tests/test_execution.py::test_same_intent_cannot_be_registered_as_new_action and CT11:
// the same intent for another task, context, command or request is IDEMPOTENCY_PAYLOAD_CONFLICT
// (Python BUSINESS_INTENT_CONFLICT, not a catalog code); while its attempt is open, another
// submission is EXTERNAL_RESULT_UNKNOWN; once committed, a submission of the same intent — a new
// plan revision redoing it — answers with the original result and appends nothing.
func TestOneIntentIsOneAction(t *testing.T) {
	e, _, err := Record(nil, submit("intent.1"), notCommitted, act())
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*Request){
		"another task":    func(r *Request) { r.TaskID = "task.2" },
		"another context": func(r *Request) { r.Target.Context = "inq" },
		"another command": func(r *Request) { r.Target.Command = "order.prepare" },
		"another request": func(r *Request) { r.Target.RequestHash = strings.Repeat("b", 64) },
	} {
		r := submit("intent.1")
		edit(&r)
		if _, _, err := Record(e, r, notCommitted, act()); code(err) != kernel.CodeIdempotencyPayloadConflict {
			t.Errorf("%s: %s", name, code(err))
		}
	}
	if _, _, err := Record(e, submit("intent.1"), notCommitted, act()); code(err) != CodeExternalResultUnknown {
		t.Fatalf("an open attempt: %s", code(err))
	}
	s, err := e.Reconcile(committed, act())
	if err != nil || e.State != Committed || *e.Result != *receipt || s.State != Committed || s.Attempt != 1 || e.Version != 2 {
		t.Fatalf("%+v %+v %v", e, s, err)
	}
	before := *e
	again, step, err := Record(e, submit("intent.1"), notCommitted, act())
	if err != nil || step != nil || again != e || *e != before {
		t.Fatalf("a committed intent: %+v %+v %v", again, step, err)
	}
	if ask, err := Precheck(e, submit("intent.1")); ask || err != nil {
		t.Fatalf("a committed intent asks the target: %v %v", ask, err)
	}
	r := submit("intent.1")
	r.Target.Command = "order.prepare"
	if _, _, err := Record(e, r, notCommitted, act()); code(err) != kernel.CodeIdempotencyPayloadConflict {
		t.Fatalf("another command of a committed intent: %s", code(err))
	}
}

// The executor's observation of its submitted attempt is recorded only while the target holds
// nothing under the key: rejected with the target's code, unknown when it lost the answer; the
// target's committed or conflicting answer wins over what the executor saw. A rejected attempt is
// settled: the next submission opens attempt 2 under the same key.
func TestObservationsFollowTheTargetsAnswer(t *testing.T) {
	observe := func(outcome State, c string) Request {
		r := submit("intent.1")
		r.Outcome, r.Code, r.DelegationID, r.GrantID = outcome, c, "", ""
		return r
	}
	e, _, _ := Record(nil, submit("intent.1"), notCommitted, act())
	e, s, err := Record(e, observe(Rejected, "VERSION_CONFLICT"), notCommitted, act())
	if err != nil || e.State != Rejected || s.Code != "VERSION_CONFLICT" || s.Attempt != 1 || e.State.Open() {
		t.Fatalf("%+v %+v %v", e, s, err)
	}
	if _, _, err = Record(e, observe(Unknown, ""), notCommitted, act()); code(err) != kernel.CodeInvalidState {
		t.Fatalf("an observation without a submitted attempt: %s", code(err))
	}
	e, s, err = Record(e, submit("intent.1"), notCommitted, act())
	if err != nil || e.State != Submitted || e.Attempts != 2 || s.Attempt != 2 || e.Key() != ID("intent.1") {
		t.Fatalf("the resend %+v %+v %v", e, s, err)
	}
	e, s, err = Record(e, observe(Unknown, "COMMIT_UNCERTAIN"), notCommitted, act())
	if err != nil || e.State != Unknown || !e.State.Open() || s.Code != "COMMIT_UNCERTAIN" {
		t.Fatalf("%+v %+v %v", e, s, err)
	}
	if _, _, err = Record(e, observe(Rejected, "X_Y"), notCommitted, act()); code(err) != kernel.CodeInvalidState {
		t.Fatalf("a rejection of an unknown attempt: %s", code(err))
	}
	if _, _, err = Record(nil, observe(Unknown, ""), notCommitted, act()); code(err) != kernel.CodeInvalidState {
		t.Fatalf("an observation of an intent without an action: %s", code(err))
	}

	// The target committed what the executor reports as lost or refused: committed.
	for _, outcome := range []State{Rejected, Unknown} {
		e, _, _ := Record(nil, submit("intent.2"), notCommitted, act())
		e, s, err := Record(e, observe(outcome, "TOOL_UNAVAILABLE"), committed, act())
		if err != nil || e.State != Committed || s.Code != "" || *s.Result != *receipt || s.Answer != kernel.ReconcileCommitted {
			t.Fatalf("%s: %+v %+v %v", outcome, e, s, err)
		}
	}
	// The target's ledger holds another request under the key: conflict, final.
	e, _, _ = Record(nil, submit("intent.3"), notCommitted, act())
	e, s, err = Record(e, observe(Unknown, ""), conflicting, act())
	if err != nil || e.State != Conflict || !e.State.Final() || s.State != Conflict {
		t.Fatalf("%+v %+v %v", e, s, err)
	}
	if err = e.CheckReconcile(); code(err) != kernel.CodeInvalidState {
		t.Fatalf("reconciling a conflict: %s", code(err))
	}
}

// A first submission whose key the target already holds records the target's answer instead of
// opening an attempt: committed (attempt 0, the receipt) or conflict.
func TestFirstSubmissionAnsweredByTheTarget(t *testing.T) {
	e, s, err := Record(nil, submit("intent.1"), committed, act())
	if err != nil || e.State != Committed || e.Attempts != 0 || e.Version != 1 || s.Attempt != 0 || s.DelegationID != "" {
		t.Fatalf("%+v %+v %v", e, s, err)
	}
	e, s, err = Record(nil, submit("intent.2"), conflicting, act())
	if err != nil || e.State != Conflict || e.Attempts != 0 || s.State != Conflict {
		t.Fatalf("%+v %+v %v", e, s, err)
	}
}

// action.reconcile: the open attempt is settled by the target's answer — not_committed (the next
// attempt sends the same key), committed with the original result, or conflict —; a settled, not
// committed action may be reconciled again; a final one is INVALID_STATE; an incomplete answer is
// refused.
func TestReconcile(t *testing.T) {
	e, _, _ := Record(nil, submit("intent.1"), notCommitted, act())
	s, err := e.Reconcile(notCommitted, act())
	if err != nil || e.State != NotCommitted || s.Command != CmdReconcile || s.State != NotCommitted || s.Attempt != 1 || e.Version != 2 {
		t.Fatalf("%+v %+v %v", e, s, err)
	}
	if s, err = e.Reconcile(notCommitted, act()); err != nil || e.State != NotCommitted || e.Version != 3 || s.Version != 3 {
		t.Fatalf("again %+v %+v %v", e, s, err)
	}
	if _, err = e.Reconcile(Answer{State: kernel.ReconcileCommitted}, act()); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("committed without a receipt: %s", code(err))
	}
	if _, err = e.Reconcile(Answer{State: kernel.ReconcileNotCommitted, Result: receipt}, act()); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("not_committed with a receipt: %s", code(err))
	}
	if _, err = e.Reconcile(Answer{State: "maybe"}, act()); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("an unknown answer: %s", code(err))
	}
	if s, err = e.Reconcile(committed, act()); err != nil || e.State != Committed || e.Result.ObjectID != "order.1" || s.Result == nil {
		t.Fatalf("%+v %+v %v", e, s, err)
	}
	if _, err = e.Reconcile(committed, act()); code(err) != kernel.CodeInvalidState {
		t.Fatalf("reconciling a committed action: %s", code(err))
	}
	if _, _, err = Record(nil, Request{Intent: "i", TaskID: "t", Target: target, Outcome: "accepted"}, notCommitted, act()); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("an unknown outcome: %s", code(err))
	}
}

// The database's own guard (brant_tsk.guard_action_execution, migration 000002) allows exactly the
// moves the domain makes: every move reachable through Record and Reconcile is in its list, and
// every listed move is reachable. Each move opens an attempt exactly when it enters submitted.
func TestDatabaseGuardMovesAreTheDomains(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "..", "migrations", "tsk", "000002_tsk_action_executions.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	start := strings.Index(sql, "v_moves text[] := ARRAY[")
	end := strings.Index(sql[start:], "];")
	if start < 0 || end < 0 {
		t.Fatal("v_moves not found")
	}
	var listed []string
	for _, part := range strings.Split(sql[start+len("v_moves text[] := ARRAY["):start+end], ",") {
		listed = append(listed, strings.Trim(strings.TrimSpace(part), "'"))
	}
	answers := []Answer{notCommitted, committed, conflicting}
	var reached []string
	seen := map[State]bool{}
	var walk func(e *Execution)
	walk = func(e *Execution) {
		if seen[e.State] {
			return
		}
		seen[e.State] = true
		next := func(from State, attempts int, n *Execution) {
			m := string(from) + ">" + string(n.State)
			if !slices.Contains(reached, m) {
				reached = append(reached, m)
			}
			if (n.State == Submitted) != (n.Attempts == attempts+1) || (n.State != Submitted && n.Attempts != attempts) {
				t.Errorf("%s: attempts %d -> %d", m, attempts, n.Attempts)
			}
			walk(n)
		}
		for _, outcome := range []State{Submitted, Rejected, Unknown} {
			for _, a := range answers {
				c := *e
				r := submit("intent.1")
				r.Outcome = outcome
				if outcome != Submitted {
					r.Code, r.DelegationID, r.GrantID = "X_CODE", "", ""
				}
				if n, s, err := Record(&c, r, a, act()); err == nil && s != nil {
					next(e.State, e.Attempts, n)
				}
			}
		}
		for _, a := range answers {
			c := *e
			if _, err := c.Reconcile(a, act()); err == nil {
				next(e.State, e.Attempts, &c)
			}
		}
	}
	for _, a := range answers {
		e, _, err := Record(nil, submit("intent.1"), a, act())
		if err != nil || e.Version != 1 || !slices.Contains([]State{Submitted, Committed, Conflict}, e.State) {
			t.Fatalf("insert %+v %v", e, err)
		}
		walk(e)
	}
	slices.Sort(reached)
	slices.Sort(listed)
	if !slices.Equal(reached, listed) {
		t.Fatalf("domain moves %v\nguard moves %v", reached, listed)
	}
	for _, s := range States {
		if !seen[s] {
			t.Errorf("state %s not reached", s)
		}
	}
}

func TestKnownContexts(t *testing.T) {
	for _, c := range []string{"evd", "inq", "sup", "cst", "trd"} {
		if !KnownContext(c) {
			t.Error(c)
		}
	}
	if KnownContext("pur") || KnownContext("") {
		t.Fatal("pur")
	}
}

// SM-TSK-TASK task.bind_object "本任务的动作已创建报价根" (S4 C1, B1-D09): a task created a root when one of
// its actions committed the root's creating save — the target's receipt names the root at row
// version 1. A later save of the root, another command or context, another root or an action that
// did not commit is not its creation.
func TestCreatedRootIsTheCommittedSaveThatMadeIt(t *testing.T) {
	save := Target{Context: "trd", Command: "quote.save_draft", RequestHash: strings.Repeat("b", 64)}
	made := &Execution{ID: ID("intent.save"), Target: save, State: Committed,
		Result: &Result{ActionID: "act-9", ObjectID: "quote.1", RowVersion: 1, Status: "working"}}
	if !CreatedRoot([]*Execution{made}, "trd", "quote.save_draft", "quote.1") {
		t.Fatal("the committed creating save")
	}
	updated := *made
	updated.Result = &Result{ActionID: "act-10", ObjectID: "quote.1", RowVersion: 2, Status: "working"}
	open := *made
	open.State, open.Result = Submitted, nil
	other := *made
	other.Target.Command = "quote.revise"
	elsewhere := *made
	elsewhere.Target.Context = "inq"
	for name, list := range map[string][]*Execution{
		"none": nil, "a later save": {&updated}, "an open save": {&open}, "another command": {&other}, "another context": {&elsewhere},
	} {
		if CreatedRoot(list, "trd", "quote.save_draft", "quote.1") {
			t.Errorf("%s counts as the creation", name)
		}
	}
	if CreatedRoot([]*Execution{made}, "trd", "quote.save_draft", "quote.2") {
		t.Error("another root")
	}
}
