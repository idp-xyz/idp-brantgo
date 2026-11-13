//go:build integration

package tsk_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.idp.xyz/idp-brant/internal/contexts/tsk"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// goal.verify on PostgreSQL 16 with TRD's GoalReadiness v1 faked (WBS S4-01d; SM-TSK-TASK's goal.verify
// rows; TSK-INV-001, TSK-INV-003; OutcomeEvidence; baseline 15.4; CT13). The fake answers the
// goal's required assertions (task.RequiredAssertions) as scripted and shows a bound root's control
// as its control calls left it; the composed suite (internal/bootstrap tsk_goal_integration_test.go)
// runs goal.verify against TRD itself.

// scripted is TRD's assessment of g: every required assertion satisfied but those in unmet, which
// answer outcome (code REFERENCE_INVALID); with a bound root's control. f.mu is held.
func (f *fakeTRD) scripted(g tsk.Goal, outcome string, unmet []string) tsk.Verdict {
	answers := map[string][2]string{}
	for _, u := range unmet {
		answers[u] = [2]string{outcome, kernel.CodeReferenceInvalid}
	}
	return f.assessment(g, answers)
}

// assessment is TRD's assessment of g with answers per assertion (answer, code); f.mu is held.
func (f *fakeTRD) assessment(g tsk.Goal, answers map[string][2]string) tsk.Verdict {
	a := tsk.Verdict{Assessor: task.AssessorGoalReadiness, Goal: g.Code, GoalVersion: g.Version, QuoteID: g.Target.QuoteID, RootVersion: 1,
		CheckedAt: time.Now()}
	var all []string
	for _, name := range task.RequiredAssertions(g.Code, g.Version) {
		c := task.Check{Check: "scripted", Answer: task.Satisfied}
		if x, ok := answers[name]; ok {
			c.Answer, c.Code, c.Path = x[0], x[1], "/data/lines/0/sku_ref"
		}
		var ev []task.Evidence
		if name == task.AssertionScope {
			ev = []task.Evidence{{Kind: task.EvidenceQuoteRoot, ID: g.Target.QuoteID, Version: 1}}
		}
		a.Assertions = append(a.Assertions, task.Assertion{Assertion: name, Answer: c.Answer, Code: c.Code, Checks: []task.Check{c}, Evidence: ev})
		all = append(all, c.Answer)
	}
	a.Outcome, a.Code = task.Outcome(all...)
	if g.Bound != "" {
		a.Control = f.control(g.Bound)
	}
	return a
}

// control is the fake root's control as its last control call left it; f.mu is held.
func (f *fakeTRD) control(quote string) *task.RootControl {
	for i := len(f.calls) - 1; i >= 0; i-- {
		c := f.calls[i]
		if c.Quote != quote {
			continue
		}
		state := map[string]string{"bind": task.RootBound, "handoff": task.RootBound, "enter_manual": task.RootManual, "stop": "stopping",
			"release": "unbound"}[c.Op]
		return &task.RootControl{State: state, TaskID: c.Task, Epoch: f.epoch[quote]}
	}
	return nil
}

func (w *world) verifications(ctx context.Context, id string) []api.GoalVerification {
	w.t.Helper()
	v, err := w.m.GoalVerifications(ctx, id)
	if err != nil {
		w.t.Fatal(err)
	}
	return v
}

// verifying binds quote to a new AI task id (order_active) and moves it to verifying.
func (w *world) verifying(id, quote string) api.TaskReceipt {
	w.t.Helper()
	r := w.bound(id, quote)
	return w.must(w.m.VerifyTask(w.sys, api.VerifyTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion}))
}

func (w *world) verifyGoal(id string, v int64) (api.TaskReceipt, error) {
	return w.m.VerifyGoal(w.sys, api.VerifyGoal{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v})
}

// again resumes waiting task id on an arrival and moves it to verifying.
func (w *world) again(id string, v int64) api.TaskReceipt {
	w.t.Helper()
	r := w.must(w.m.ResumeTask(w.sys, api.ResumeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v,
		Arrival: &api.ObjectRef{Kind: "order", ID: "order.1", Version: 1}}))
	return w.must(w.m.VerifyTask(w.sys, api.VerifyTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion}))
}

func (w *world) script(answers map[string][2]string) {
	w.trd.full = func(g tsk.Goal) (tsk.Verdict, error) { return w.trd.assessment(g, answers), nil }
}

// OutcomeEvidence, SM-TSK-TASK's goal.verify rows and CT13 (必需行业检查 unknown／error：不得
// completed；展示具体原因及恢复条件): each goal.verify is recorded with the task — who judged, the
// outcome, the unmet assertions, every assertion with its checks and evidence and Q01.execution last,
// TRD's control of the bound root, the instant judged, the actor and the task version of its history
// entry. A required industry check that cannot answer leaves the task waiting_external with a reason
// naming the assertion, its code and how the task goes on; an unsatisfied order waits for input; the
// satisfied goal completes the task and TaskCompleted names the revision and the order judged. The
// verifications are read under tsk.read by those who see the task (X-INV-003), audited.
func TestGoalVerificationsAreRecorded(t *testing.T) {
	w := newWorld(t)
	r := w.verifying("task.1", "quote.1")
	w.script(map[string][2]string{task.AssertionIndustry: {task.Unknown, "DEPENDENCY_NOT_SUPPORTED"}})
	r = w.must(w.verifyGoal("task.1", r.RowVersion))
	v := w.get("task.1")
	if r.Status != api.StateWaitingExternal || v.Wait == nil || !slices.Equal(v.Wait.Items, []string{task.AssertionIndustry}) ||
		v.Wait.ResponsibleID != "alice" || !strings.Contains(v.Wait.Reason, "Q01.industry unknown DEPENDENCY_NOT_SUPPORTED") ||
		!strings.Contains(v.Wait.Reason, "once the checks can answer") || v.History[len(v.History)-1].Reason != v.Wait.Reason {
		t.Fatalf("CT13: %+v %+v", r, v.Wait)
	}
	r = w.again("task.1", r.RowVersion)
	w.script(map[string][2]string{task.AssertionOrder: {task.Unsatisfied, "ORDER_MISSING"}})
	r = w.must(w.verifyGoal("task.1", r.RowVersion))
	if v = w.get("task.1"); r.Status != api.StateWaitingInput || !strings.Contains(v.Wait.Reason, "Q01.order unsatisfied ORDER_MISSING") {
		t.Fatalf("%+v %+v", r, v.Wait)
	}
	r = w.again("task.1", r.RowVersion)
	w.trd.full = func(g tsk.Goal) (tsk.Verdict, error) {
		a := w.trd.assessment(g, nil)
		a.BusinessRevision, a.BusinessHash = 2, strings.Repeat("c", 64)
		a.OrderID, a.OrderRevision, a.OrderHash = "order.1", 1, strings.Repeat("d", 64)
		return a, nil
	}
	r = w.must(w.verifyGoal("task.1", r.RowVersion))
	if r.Status != api.StateCompleted || w.get("task.1").CompletionActor != api.CompletionAIWithRequiredReview {
		t.Fatalf("%+v", r)
	}

	vs := w.verifications(w.alice, "task.1")
	history := w.get("task.1").History
	if len(vs) != 3 {
		t.Fatalf("%d verifications", len(vs))
	}
	for i, want := range []struct{ lifecycle, outcome, code, unmet string }{
		{api.StateWaitingExternal, api.AnswerUnknown, api.CodeRequiredCheckUnknown, task.AssertionIndustry},
		{api.StateWaitingInput, api.AnswerUnsatisfied, api.CodeRequiredCheckUnsatisfied, task.AssertionOrder},
		{api.StateCompleted, api.AnswerSatisfied, "", ""},
	} {
		x := vs[i]
		h := history[x.TaskVersion-1]
		last := x.Assertions[len(x.Assertions)-1]
		if x.Lifecycle != want.lifecycle || x.Outcome != want.outcome || x.Code != want.code || strings.Join(x.Unmet, ",") != want.unmet ||
			h.Command != api.CommandVerifyGoal || h.To != x.Lifecycle || x.Actor != h.Actor || x.Actor.ID != "tsk" ||
			x.Assessor != api.AssessorGoalReadiness || x.GoalCode != api.GoalOrderActive || x.GoalVersion != 1 || x.QuoteID != "quote.1" ||
			len(x.Assertions) != 9 || last.Assertion != api.AssertionExecution || last.Answer != api.AnswerSatisfied || len(last.Checks) != 2 ||
			x.Control == nil || *x.Control != (api.RootControlView{State: "bound", TaskID: "task.1", ControlEpoch: 1}) ||
			x.CheckedAt.IsZero() || x.RecordedAt.Before(x.CheckedAt) || x.UnsettledActions != 0 {
			t.Fatalf("verification %d: %+v", i, x)
		}
	}
	industry := vs[0].Assertions[5]
	if industry.Assertion != task.AssertionIndustry || industry.Answer != api.AnswerUnknown || industry.Code != "DEPENDENCY_NOT_SUPPORTED" ||
		industry.Checks[0] != (api.GoalCheck{Check: "scripted", Answer: api.AnswerUnknown, Code: "DEPENDENCY_NOT_SUPPORTED", Path: "/data/lines/0/sku_ref"}) ||
		vs[0].Assertions[0].Evidence[0] != (api.GoalEvidence{Kind: task.EvidenceQuoteRoot, ID: "quote.1", Version: 1}) {
		t.Fatalf("%+v %+v", industry, vs[0].Assertions[0])
	}
	if c := vs[2]; c.BusinessRevision != 2 || c.OrderID != "order.1" || c.OrderRevision != 1 || c.OrderHash != strings.Repeat("d", 64) {
		t.Fatalf("%+v", c)
	}
	var completed []event
	for _, e := range w.events() {
		if e.Type == api.EventTaskCompleted {
			completed = append(completed, e)
		}
	}
	if len(completed) != 1 || completed[0].Payload["order_id"] != "order.1" || completed[0].Payload["order_revision"] != float64(1) ||
		completed[0].Payload["business_revision"] != float64(2) || completed[0].Payload["completion_actor"] != api.CompletionAIWithRequiredReview {
		t.Fatalf("%+v", completed)
	}
	// Reads: the agent sees its task; a human outside it and another company do not (NOT_FOUND).
	if got := w.verifications(w.agent, "task.1"); len(got) != 3 {
		t.Fatalf("agent read %d", len(got))
	}
	for _, ctx := range []context.Context{w.bob, w.other} {
		_, err := w.m.GoalVerifications(ctx, "task.1")
		w.want(err, kernel.CodeNotFound)
	}
	_, err := w.m.GoalVerifications(w.alice, "bad id")
	w.want(err, kernel.CodeNotFound)
	audit, err := w.m.Audit(w.alice)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	for _, e := range audit {
		if e.Command == api.CommandVerifications && e.ObjectID == "task.1" && e.Outcome == "read" {
			reads++
		}
	}
	if reads != 2 {
		t.Fatalf("%d audited reads", reads)
	}
}

// Baseline 15.4 Q01.scope and Q01.quote: TSK judges a goal itself only to find it not met — a task
// that names no quote root is not asked of TRD (Q01.quote, REFERENCE_INVALID), a root TRD does not
// show the verifying actor (NOT_FOUND, PERMISSION_DENIED) is Q01.scope unsatisfied; both wait for
// input. Any other answer TRD gives instead of an assessment (SCHEMA_INVALID, VERSION_CONFLICT, an
// infrastructure error) and an assessment of another goal refuse goal.verify: the task stays
// verifying, nothing is recorded, a refusal is audited.
func TestGoalVerificationWithoutTRDsAssessment(t *testing.T) {
	w := newWorld(t)
	r := w.running("task.0", func(c *api.CreateTask) { c.ControlMode, c.AgentID, c.GoalCode = api.ModeManual, "", api.GoalQuoteReady })
	r = w.must(w.m.VerifyTask(w.sys, api.VerifyTask{IdempotencyKey: w.key(), TaskID: "task.0", ExpectedVersion: r.RowVersion}))
	w.trd.full = func(tsk.Goal) (tsk.Verdict, error) {
		t.Fatal("TRD asked for a task without a quote root")
		return tsk.Verdict{}, nil
	}
	r = w.must(w.verifyGoal("task.0", r.RowVersion))
	x := w.verifications(w.alice, "task.0")
	if r.Status != api.StateWaitingInput || len(x) != 1 || x[0].Assessor != api.AssessorTSK || x[0].QuoteID != "" ||
		!slices.Equal(x[0].Unmet, []string{task.AssertionQuote}) || x[0].Assertions[0].Checks[0].Code != kernel.CodeReferenceInvalid {
		t.Fatalf("%+v %+v", r, x)
	}

	for i, refusal := range []string{kernel.CodeNotFound, kernel.CodePermissionDenied} {
		id := "task.s" + string(rune('0'+i))
		r := w.verifying(id, "quote."+id)
		w.trd.full = func(tsk.Goal) (tsk.Verdict, error) { return tsk.Verdict{}, kernel.Reject(refusal, "/quote_id", "") }
		r = w.must(w.verifyGoal(id, r.RowVersion))
		x := w.verifications(w.alice, id)
		if r.Status != api.StateWaitingInput || len(x) != 1 || x[0].Assessor != api.AssessorTSK || x[0].Control != nil ||
			!slices.Equal(x[0].Unmet, []string{task.AssertionScope}) || x[0].Assertions[0].Code != refusal {
			t.Fatalf("%s: %+v %+v", refusal, r, x)
		}
	}

	r = w.verifying("task.1", "quote.1")
	for name, answer := range map[string]func(tsk.Goal) (tsk.Verdict, error){
		kernel.CodeSchemaInvalid: func(tsk.Goal) (tsk.Verdict, error) {
			return tsk.Verdict{}, kernel.Reject(kernel.CodeSchemaInvalid, "/goal", "")
		},
		kernel.CodeVersionConflict: func(tsk.Goal) (tsk.Verdict, error) {
			return tsk.Verdict{}, kernel.Reject(kernel.CodeVersionConflict, "/quote_id", "")
		},
		"ERROR: unreachable": func(tsk.Goal) (tsk.Verdict, error) { return tsk.Verdict{}, errors.New("unreachable") },
		"ERROR: tsk: goal assessment for another goal or quote, or without assertions": func(g tsk.Goal) (tsk.Verdict, error) {
			a := w.trd.assessment(g, nil)
			a.QuoteID = "quote.2"
			return a, nil
		},
		"ERROR: tsk: goal assessment without the required assertions of order_active": func(g tsk.Goal) (tsk.Verdict, error) {
			g.Code = api.GoalQuoteReady
			a := w.trd.assessment(g, nil)
			a.Goal = api.GoalOrderActive
			return a, nil
		},
	} {
		w.trd.full = answer
		_, err := w.verifyGoal("task.1", r.RowVersion)
		got := code(err)
		if failure, ok := strings.CutPrefix(name, "ERROR: "); ok {
			if !strings.Contains(got, failure) {
				t.Errorf("%s: %s", name, got)
			}
		} else if got != name {
			t.Errorf("%s: %s", name, got)
		}
	}
	if v := w.get("task.1"); v.Lifecycle != api.StateVerifying || v.TaskVersion != r.RowVersion || len(w.verifications(w.alice, "task.1")) != 0 {
		t.Fatalf("%+v", v)
	}
	audit, err := w.m.Audit(w.alice)
	if err != nil {
		t.Fatal(err)
	}
	var refused []string
	for _, e := range audit {
		if e.Command == api.CommandVerifyGoal && e.ObjectID == "task.1" && e.Outcome == "rejected" {
			refused = append(refused, e.Code)
		}
	}
	slices.Sort(refused)
	if !slices.Equal(refused, []string{kernel.CodeSchemaInvalid, kernel.CodeVersionConflict}) {
		t.Fatalf("audited refusals %v", refused)
	}
}

// SM-TSK-TASK "快照版本仍匹配且无冲突" and TSK-INV-002: a take-over stops the root at TRD first and
// records it in TSK second. A goal.verify that runs in between — TRD's control has moved past the
// epoch the task recorded — is refused CONTROL_EPOCH_STALE and leaves the task verifying, so the
// take-over is recorded and the task does not complete under an AI that has been fenced out.
func TestGoalVerifyWaitsForATakeOverUnderWay(t *testing.T) {
	w := newWorld(t)
	r := w.verifying("task.1", "quote.1")
	w.trd.assess = func(tsk.Goal) (string, []string) { return "satisfied", nil }
	var during error
	w.trd.hook = func(op string) {
		if op == "stop" {
			_, during = w.verifyGoal("task.1", r.RowVersion)
		}
	}
	took := w.must(w.m.TakeControl(w.alice, api.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "人工接管"}))
	w.want(during, api.CodeControlEpochStale)
	if took.Status != api.StateStopping || took.ControlEpoch != 2 || len(w.verifications(w.alice, "task.1")) != 0 {
		t.Fatalf("%+v", took)
	}
	_, err := w.verifyGoal("task.1", took.RowVersion)
	w.want(err, kernel.CodeInvalidState)
}

// Concurrency (task_version CAS): five goal.verify of one verifying task at the same version — one
// verifies, the others meet VERSION_CONFLICT; one verification, one TaskCompleted. A replay of the
// one that ran answers its receipt without asking TRD again or recording a second verification.
func TestConcurrentGoalVerifications(t *testing.T) {
	w := newWorld(t)
	r := w.verifying("task.1", "quote.1")
	var asked int
	var mu sync.Mutex
	w.trd.full = func(g tsk.Goal) (tsk.Verdict, error) {
		mu.Lock()
		asked++
		mu.Unlock()
		return w.trd.assessment(g, nil), nil
	}
	var wg sync.WaitGroup
	results := make([]error, 5)
	receipts := make([]api.TaskReceipt, 5)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipts[i], results[i] = w.m.VerifyGoal(w.sys, api.VerifyGoal{IdempotencyKey: "verify." + string(rune('0'+i)), TaskID: "task.1",
				ExpectedVersion: r.RowVersion})
		}()
	}
	wg.Wait()
	won := -1
	for i, err := range results {
		switch code(err) {
		case "OK":
			if won >= 0 {
				t.Fatal("two verifications won")
			}
			won = i
		case kernel.CodeVersionConflict:
		default:
			t.Fatalf("%v", err)
		}
	}
	if won < 0 || len(w.verifications(w.alice, "task.1")) != 1 || receipts[won].Status != api.StateCompleted {
		t.Fatalf("winner %d %+v", won, receipts)
	}
	before := asked
	again, err := w.m.VerifyGoal(w.sys, api.VerifyGoal{IdempotencyKey: "verify." + string(rune('0'+won)), TaskID: "task.1", ExpectedVersion: r.RowVersion})
	if err != nil || !again.Replayed || again.RowVersion != receipts[won].RowVersion || asked != before || len(w.verifications(w.alice, "task.1")) != 1 {
		t.Fatalf("replay %+v %v asked %d→%d", again, err, before, asked)
	}
	n := 0
	for _, e := range w.events() {
		if e.Type == api.EventTaskCompleted {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d TaskCompleted", n)
	}
}

// The database keeps the verifications too (defense in depth; TSK-INV-001): a verification is never
// changed or deleted, exists only for a goal.verify from verifying to the state it names, once per
// version; a goal.verify history entry without its verification is refused at commit; only TRD's
// assessment completes a task.
func TestDatabaseKeepsGoalVerifications(t *testing.T) {
	w := newWorld(t)
	r := w.verifying("task.1", "quote.1")
	w.trd.assess = func(tsk.Goal) (string, []string) { return "unsatisfied", []string{task.AssertionOrder} }
	r = w.must(w.verifyGoal("task.1", r.RowVersion))
	w.verifying("task.2", "quote.2")
	insert := func(task string, version int64, lifecycle, assessor, outcome string) string {
		code := map[string]string{"satisfied": "NULL", "unsatisfied": "'REQUIRED_CHECK_UNSATISFIED'"}[outcome]
		if code == "" {
			code = "'REQUIRED_CHECK_UNKNOWN'"
		}
		unmet := "'{Q01.order}'"
		if outcome == "satisfied" {
			unmet = "'{}'"
		}
		return `INSERT INTO brant_tsk.goal_verification (tenant_id, company_id, task_id, task_version, lifecycle, goal_code, goal_version,
			assessor, outcome, code, unmet, quote_id, root_version, unsettled_actions, assertions, checked_at, recorded_at)
			SELECT tenant_id, company_id, task_id, ` + strconv.FormatInt(version, 10) + `, '` + lifecycle + `', goal_code, goal_version, '` +
			assessor + `', '` + outcome + `', ` + code + `, ` + unmet + `, target_quote_id, 1, 0, '[{"assertion":"Q01.order"}]', now(), now()
			FROM brant_tsk.business_task WHERE task_id = '` + task + `'`
	}
	// task.2 (verifying at version 5) moved by goal.verify in SQL to version 6, with its history entry.
	move := func(to, extra string) string {
		return `UPDATE brant_tsk.business_task SET lifecycle = '` + to + `', task_version = task_version + 1` + extra + ` WHERE task_id = 'task.2';
			INSERT INTO brant_tsk.task_transition SELECT tenant_id, company_id, task_id, task_version, 'goal.verify', 'verifying', lifecycle,
				control_mode, control_epoch, NULL, 'system', 'tsk', 1, now(), 'x' FROM brant_tsk.business_task WHERE task_id = 'task.2'`
	}
	waiting := `, wait_kind = 'input', wait_reason = 'x', wait_responsible_id = 'alice', wait_since = now()`
	done := `, completion_actor = 'manual'`
	for name, stmt := range map[string]string{
		"update":                `UPDATE brant_tsk.goal_verification SET outcome = 'satisfied' WHERE task_id = 'task.1'`,
		"delete":                `DELETE FROM brant_tsk.goal_verification WHERE task_id = 'task.1'`,
		"a second one":          insert("task.1", r.RowVersion, "waiting_input", "trd.goal_readiness.v1", "unsatisfied"),
		"not a goal.verify":     insert("task.1", 5, "waiting_input", "trd.goal_readiness.v1", "unsatisfied"),
		"no verification":       move("waiting_input", waiting),
		"another state":         move("waiting_input", waiting) + `; ` + insert("task.2", 6, "waiting_external", "trd.goal_readiness.v1", "unknown"),
		"TSK satisfied":         move("completed", done) + `; ` + insert("task.2", 6, "completed", "tsk", "satisfied"),
		"completed unsatisfied": move("completed", done) + `; ` + insert("task.2", 6, "completed", "trd.goal_readiness.v1", "unsatisfied"),
	} {
		err := w.sql(stmt)
		var pgErr *pgconn.PgError
		t.Logf("%s: %v", name, err)
		// UPDATE and DELETE are not even granted to the application role (42501); the trigger refuses
		// them to the owner too.
		if !errors.As(err, &pgErr) || (pgErr.Code[:2] != "23" && pgErr.Code != "42501") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if v := w.get("task.2"); v.Lifecycle != api.StateVerifying || v.TaskVersion != 5 {
		t.Fatalf("%+v", v)
	}
	// The same SQL with its verification is accepted: the checks above refuse what is missing or wrong.
	if err := w.sql(move("waiting_input", waiting) + `; ` + insert("task.2", 6, "waiting_input", "trd.goal_readiness.v1", "unsatisfied")); err != nil {
		t.Fatal(err)
	}
}
