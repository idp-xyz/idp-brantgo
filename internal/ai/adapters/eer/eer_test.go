package eer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/runtime"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var scopeA = kernel.Scope{Tenant: "tenant.1", Company: "company.A"}

func client(t *testing.T, base string, token string, hc *http.Client) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: base, TenantID: "pilot", ActorID: "operator", Tokens: StaticToken(token), PolicyID: "brant-policy", PolicyVersion: 1,
		EventType: "brant.task.started", HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sub(task string, approval bool) runtime.Submission {
	at := time.Date(2026, 11, 13, 9, 0, 0, 123456789, time.UTC)
	return runtime.Submission{OperationID: runtime.OperationOf(scopeA, task), TaskID: task, Objective: "quote ready (Q01)", Plan: "brant-q01",
		PlanVersion: 1, ApprovalRequired: approval, Inputs: map[string]any{"goal": "quote_ready"}, RequestedAt: at, Deadline: at.Add(time.Hour)}
}

// IG-10, baseline 17.2: an operation is submitted under its stable ID; the same submission again is
// EER's idempotent replay of the one execution; the same ID with other content is refused
// (IDEMPOTENCY_PAYLOAD_CONFLICT), not a second execution. T2 waits for approval; T1 runs.
func TestSubmitIsOneExecutionPerOperation(t *testing.T) {
	f, srv := newFakeEER(t)
	c := client(t, srv.URL, "tok-operator", nil)
	ctx := context.Background()
	s := sub("task.1", true)
	op, err := c.Submit(ctx, s)
	if err != nil || op.State != runtime.WaitingApproval || op.Replayed || op.RuntimeRef != ExecutionID(s.OperationID) ||
		op.RuntimeState != StateWaiting || op.LastTrigger() != TriggerApprovalRequired || len(op.Transitions) != 3 {
		t.Fatalf("submit %+v %v", op, err)
	}
	again, err := c.Submit(ctx, s)
	if err != nil || !again.Replayed || again.State != runtime.WaitingApproval || len(f.execs) != 1 {
		t.Fatalf("resubmit %+v %v (%d executions)", again, err, len(f.execs))
	}
	changed := s
	changed.Objective = "something else"
	_, err = c.Submit(ctx, changed)
	rej, ok := kernel.AsRejection(err)
	if !errors.Is(err, runtime.ErrConflict) || !ok || rej.Code != kernel.CodeIdempotencyPayloadConflict || len(f.execs) != 1 {
		t.Fatalf("changed content: %v", err)
	}
	// The intent carries the deadline, the tier and the instant Brant recorded (UTC, microseconds).
	req := f.intents[ExecutionID(s.OperationID)]
	for _, want := range []string{`"disposition_tier":"T2"`, `"deadline":"2026-11-13T10:00:00.123456Z"`, `"created_at":"2026-11-13T09:00:00.123456Z"`,
		`"originating_events":["` + EventID(s.OperationID) + `"]`, `"plan_version":1`, `"correlation_id":"` + s.OperationID + `"`} {
		if !strings.Contains(req, want) {
			t.Fatalf("intent %s lacks %s", req, want)
		}
	}
	if strings.Contains(req, "task.1") || strings.Contains(f.events[EventID(s.OperationID)], "task.1") {
		t.Fatal("the task ID went to EER in clear")
	}
	t1, err := c.Submit(ctx, sub("task.2", false))
	if err != nil || t1.State != runtime.Completed || t1.Outcome != "completed" {
		t.Fatalf("T1 %+v %v", t1, err)
	}
	if op, err = c.Lookup(ctx, runtime.OperationOf(scopeA, "task.none")); err != nil || op.State != runtime.NotFound {
		t.Fatalf("lookup of nothing %+v %v", op, err)
	}
}

// loseAnswers sends a request and then drops EER's answer for the routes it names: the request
// arrived, the caller never learns it.
type loseAnswers struct {
	base http.RoundTripper
	lose map[string]int
}

func (l *loseAnswers) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := l.base.RoundTrip(r)
	key := r.Method + " " + r.URL.Path
	for prefix, n := range l.lose {
		if strings.HasPrefix(key, prefix) && n > 0 && err == nil {
			l.lose[prefix] = n - 1
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return nil, io.ErrUnexpectedEOF
		}
	}
	return resp, err
}

// Unknown reconciliation (IG-10, baseline 17.2 超时进入 unknown 并查询／人工对账): a submission whose
// answer is lost is ErrUnknown, never a failure or a success; looking it up finds the execution, and
// it is not submitted again. A lost cancellation is looked up by the barrier. A request that never
// left (dial failure) is ErrUnavailable, and a read that fails is never "unknown".
func TestLostAnswersAreReconciledByLookup(t *testing.T) {
	f, srv := newFakeEER(t)
	lose := &loseAnswers{base: http.DefaultTransport, lose: map[string]int{"POST /v1/intents": 1}}
	c := client(t, srv.URL, "tok-operator", &http.Client{Transport: lose})
	ctx := context.Background()
	s := sub("task.1", true)
	if _, err := c.Submit(ctx, s); !errors.Is(err, runtime.ErrUnknown) {
		t.Fatalf("lost answer: %v", err)
	}
	rec, err := runtime.ReconcileSubmission(ctx, c, s)
	if err != nil || !rec.Arrived || rec.Operation.State != runtime.WaitingApproval || f.count("POST /v1/intents") != 1 {
		t.Fatalf("reconciled %+v %v (%d intents)", rec, err, f.count("POST /v1/intents"))
	}
	// The event's answer lost: the intent never went; the reconciliation submits again under the
	// same IDs, and EER takes the identical event again.
	lose.lose["POST /v1/events"] = 1
	s2 := sub("task.2", true)
	if _, err = c.Submit(ctx, s2); !errors.Is(err, runtime.ErrUnknown) {
		t.Fatalf("lost event answer: %v", err)
	}
	rec, err = runtime.ReconcileSubmission(ctx, c, s2)
	if err != nil || rec.Arrived || rec.Operation.State != runtime.WaitingApproval || len(f.execs) != 2 {
		t.Fatalf("resubmitted %+v %v", rec, err)
	}
	// A lost cancellation: the barrier looks the execution up and stays unsettled until it ended.
	lose.lose["POST /v1/executions/"] = 1
	st, err := runtime.Barrier(ctx, c, s.OperationID, "take-over of the task")
	if err != nil || st.Settled || !st.CancelRequested {
		t.Fatalf("lost cancel %+v %v", st, err)
	}
	if st, err = runtime.Barrier(ctx, c, s.OperationID, "take-over of the task"); err != nil || !st.Settled || st.Operation.State != runtime.Cancelled {
		t.Fatalf("after the lost cancel %+v %v", st, err)
	}
	// EER answers 5xx on a write: maybe recorded, so unknown; on a read: unavailable.
	f.failNext["POST /v1/intents"] = http.StatusServiceUnavailable
	if _, err = c.Submit(ctx, sub("task.3", true)); !errors.Is(err, runtime.ErrUnknown) {
		t.Fatalf("5xx write: %v", err)
	}
	f.failNext["GET /v1/executions"] = http.StatusServiceUnavailable
	if _, err = c.Lookup(ctx, s.OperationID); !errors.Is(err, runtime.ErrUnavailable) || errors.Is(err, runtime.ErrUnknown) {
		t.Fatalf("5xx read: %v", err)
	}
	// Nothing listens: the request never left.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	if _, err = client(t, deadURL, "tok-operator", nil).Submit(ctx, sub("task.4", true)); !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("dial failure: %v", err)
	}
}

// The runtime half of the stop barrier against EER's cancellation (IG-10 停止屏障): EER takes the
// cancellation as a signal, so the first barrier asks and stays unsettled; the next one finds the
// execution Completed with outcome cancelled and settles; a late approval is refused (the old
// decision path is closed); cancelling again answers the ended execution. Cancelling only works as
// the token's mapped actor.
func TestBarrierCancelsTheExecutionAndWaitsForItsEnd(t *testing.T) {
	f, srv := newFakeEER(t)
	c := client(t, srv.URL, "tok-operator", nil)
	ctx := context.Background()
	s := sub("task.1", true)
	if _, err := c.Submit(ctx, s); err != nil {
		t.Fatal(err)
	}
	st, err := runtime.Barrier(ctx, c, s.OperationID, "Brant take-over")
	if err != nil || st.Settled || !st.CancelRequested || f.count("POST cancellation") != 1 {
		t.Fatalf("first barrier %+v %v", st, err)
	}
	st, err = runtime.Barrier(ctx, c, s.OperationID, "Brant take-over")
	if err != nil || !st.Settled || st.Operation.State != runtime.Cancelled || st.Operation.Outcome != OutcomeCancelled ||
		st.Operation.LastTrigger() != "cancellation_completed" || f.count("POST cancellation") != 1 {
		t.Fatalf("second barrier %+v %v", st, err)
	}
	if _, err = c.Decide(ctx, s.OperationID, true); !errors.Is(err, runtime.ErrNotWaiting) {
		t.Fatalf("approval after the stop: %v", err)
	}
	if op, err := c.Cancel(ctx, s.OperationID, "again"); err != nil || op.State != runtime.Cancelled {
		t.Fatalf("cancel an ended execution %+v %v", op, err)
	}
	if op, err := c.Cancel(ctx, runtime.OperationOf(scopeA, "task.none"), "nothing"); err != nil || op.State != runtime.NotFound {
		t.Fatalf("cancel nothing %+v %v", op, err)
	}
	// bob's token maps to actor bob: cancelling as operator is refused.
	s2 := sub("task.2", true)
	if _, err = c.Submit(ctx, s2); err != nil {
		t.Fatal(err)
	}
	if _, err = client(t, srv.URL, "tok-bob", nil).Cancel(ctx, s2.OperationID, "not mine"); !errors.Is(err, runtime.ErrDenied) {
		t.Fatalf("foreign actor: %v", err)
	}
	if _, err = c.Cancel(ctx, s2.OperationID, ""); !errors.Is(err, runtime.ErrInvalid) {
		t.Fatalf("empty reason: %v", err)
	}
	if _, err = c.Cancel(ctx, s2.OperationID, strings.Repeat("x", MaxReason+1)); !errors.Is(err, runtime.ErrInvalid) {
		t.Fatalf("long reason: %v", err)
	}
}

// The approval wait: an approval as the mapped actor completes the execution and EER's audit names
// the actor; a rejection ends it with outcome rejected; deciding what does not wait is ErrNotWaiting.
// The Actor invocations are EER's evidence of each step (idempotency key, attempt, delivery attempt).
func TestDecideAnswersTheApprovalWait(t *testing.T) {
	_, srv := newFakeEER(t)
	c := client(t, srv.URL, "tok-operator", nil)
	ctx := context.Background()
	a, r := sub("task.a", true), sub("task.r", true)
	for _, s := range []runtime.Submission{a, r} {
		if _, err := c.Submit(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	op, err := c.Decide(ctx, a.OperationID, true)
	if err != nil || op.State != runtime.Completed || op.Outcome != "completed" || len(op.Audit) != 1 || op.Audit[0].Action != "approval.approved" ||
		op.Audit[0].ActorID != "operator" {
		t.Fatalf("approved %+v %v", op, err)
	}
	if op, err = c.Decide(ctx, r.OperationID, false); err != nil || op.State != runtime.Completed || op.Outcome != "rejected" {
		t.Fatalf("rejected %+v %v", op, err)
	}
	if _, err = c.Decide(ctx, a.OperationID, true); !errors.Is(err, runtime.ErrNotWaiting) {
		t.Fatalf("decided twice: %v", err)
	}
	if _, err = c.Decide(ctx, runtime.OperationOf(scopeA, "task.none"), true); !errors.Is(err, runtime.ErrNotWaiting) {
		t.Fatalf("decide nothing: %v", err)
	}
	inv, err := c.Invocations(ctx, a.OperationID)
	if err != nil || len(inv) != 1 || inv[0].IdempotencyKey == "" || inv[0].Attempt != 1 || inv[0].DeliveryAttempt != 1 {
		t.Fatalf("invocations %+v %v", inv, err)
	}
}

// Refusals and malformed answers: a bad token is ErrDenied; another tenant's answer, a state Brant
// does not know and an oversized answer are refused, never guessed.
func TestAnswersBrantCannotTrustAreRefused(t *testing.T) {
	f, srv := newFakeEER(t)
	ctx := context.Background()
	if _, err := client(t, srv.URL, "wrong", nil).Lookup(ctx, "brant-x"); !errors.Is(err, runtime.ErrDenied) {
		t.Fatalf("bad token: %v", err)
	}
	c := client(t, srv.URL, "tok-operator", nil)
	s := sub("task.1", true)
	if _, err := c.Submit(ctx, s); err != nil {
		t.Fatal(err)
	}
	f.stateOver[ExecutionID(s.OperationID)] = "Paused"
	if _, err := c.Lookup(ctx, s.OperationID); !errors.Is(err, runtime.ErrInvalid) {
		t.Fatalf("unknown state: %v", err)
	}
	delete(f.stateOver, ExecutionID(s.OperationID))
	other, err := New(Config{BaseURL: srv.URL, TenantID: "other", ActorID: "operator", Tokens: StaticToken("tok-operator"), PolicyID: "p", PolicyVersion: 1,
		EventType: "brant.task.started"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Lookup(ctx, s.OperationID); !errors.Is(err, runtime.ErrInvalid) {
		t.Fatalf("another tenant's execution: %v", err)
	}
	small, err := New(Config{BaseURL: srv.URL, TenantID: "pilot", ActorID: "operator", Tokens: StaticToken("tok-operator"), PolicyID: "p", PolicyVersion: 1,
		EventType: "brant.task.started", MaxResponseBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = small.Lookup(ctx, s.OperationID); !errors.Is(err, runtime.ErrInvalid) {
		t.Fatalf("oversized: %v", err)
	}
	var e *Error
	_, err = client(t, srv.URL, "tok-bob", nil).Cancel(ctx, s.OperationID, "x")
	if !errors.As(err, &e) || e.Status != http.StatusForbidden || e.Code != "principal_actor_mismatch" {
		t.Fatalf("EER's error %v", err)
	}
	for _, bad := range []Config{
		{BaseURL: "ftp://x", TenantID: "pilot", ActorID: "a", Tokens: StaticToken("t"), PolicyID: "p", PolicyVersion: 1, EventType: "a.b"},
		{BaseURL: "http://x/v1", TenantID: "pilot", ActorID: "a", Tokens: StaticToken("t"), PolicyID: "p", PolicyVersion: 1, EventType: "a.b"},
		{BaseURL: "http://x", TenantID: "Pilot", ActorID: "a", Tokens: StaticToken("t"), PolicyID: "p", PolicyVersion: 1, EventType: "a.b"},
		{BaseURL: "http://x", TenantID: "pilot", Tokens: StaticToken("t"), PolicyID: "p", PolicyVersion: 1, EventType: "a.b"},
		{BaseURL: "http://x", TenantID: "pilot", ActorID: "a", PolicyID: "p", PolicyVersion: 1, EventType: "a.b"},
		{BaseURL: "http://x", TenantID: "pilot", ActorID: "a", Tokens: StaticToken("t"), PolicyVersion: 1, EventType: "a.b"},
		{BaseURL: "http://x", TenantID: "pilot", ActorID: "a", Tokens: StaticToken("t"), PolicyID: "p", PolicyVersion: 1, EventType: "single"},
		{BaseURL: "http://x", TenantID: "pilot", ActorID: "a", Tokens: StaticToken("t"), PolicyID: "p", PolicyVersion: 1, EventType: "a.b", EventSource: "nocolon"},
	} {
		if _, err := New(bad); !errors.Is(err, runtime.ErrInvalid) {
			t.Fatalf("config %+v accepted", bad)
		}
	}
	if _, err = c.Submit(ctx, runtime.Submission{OperationID: "Bad"}); !errors.Is(err, runtime.ErrInvalid) {
		t.Fatalf("invalid submission: %v", err)
	}
	bad := sub("task.9", true)
	bad.Subject = "no-colon"
	if _, err = c.Submit(ctx, bad); !errors.Is(err, runtime.ErrInvalid) {
		t.Fatalf("bad subject: %v", err)
	}
}

// Credentials stay out of errors, strings and logs: the static token, the client secret, and the
// token endpoint's answer. The client-credentials source caches its token until shortly before it
// expires.
func TestCredentialsNeverAppear(t *testing.T) {
	const secret, token = "s3cr3t-client", "tok-operator"
	var grants int
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("client_secret") != secret || r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("audience") != "eer-runtime" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"error":"unauthorized_client","echo":"`+r.PostForm.Get("client_secret")+`"}`)
			return
		}
		grants++
		_, _ = fmt.Fprint(w, `{"access_token":"`+token+`","token_type":"Bearer","expires_in":300}`)
	}))
	defer idp.Close()
	_, srv := newFakeEER(t)
	cc := &ClientCredentials{TokenURL: idp.URL, ClientID: "brant", ClientSecret: secret, Audience: "eer-runtime"}
	c, err := New(Config{BaseURL: srv.URL, TenantID: "pilot", ActorID: "operator", Tokens: cc, PolicyID: "p", PolicyVersion: 1, EventType: "a.b"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 3 {
		if _, err = c.Lookup(ctx, "brant-x"); err != nil {
			t.Fatal(err)
		}
	}
	if grants != 1 {
		t.Fatalf("%d grants for 3 calls", grants)
	}
	wrong := &ClientCredentials{TokenURL: idp.URL, ClientID: "brant", ClientSecret: "wrong-" + secret}
	cw, _ := New(Config{BaseURL: srv.URL, TenantID: "pilot", ActorID: "operator", Tokens: wrong, PolicyID: "p", PolicyVersion: 1, EventType: "a.b"})
	_, err = cw.Lookup(ctx, "brant-x")
	if !errors.Is(err, runtime.ErrDenied) {
		t.Fatalf("refused grant: %v", err)
	}
	cs, _ := New(Config{BaseURL: srv.URL, TenantID: "pilot", ActorID: "operator", Tokens: StaticToken("leaky-token"), PolicyID: "p", PolicyVersion: 1, EventType: "a.b"})
	_, lerr := cs.Lookup(ctx, "brant-x")
	for _, s := range []string{err.Error(), lerr.Error(), fmt.Sprint(c), fmt.Sprintf("%#v", c), fmt.Sprint(cc), fmt.Sprintf("%#v", cc),
		fmt.Sprint(StaticToken(token)), fmt.Sprintf("%#v", StaticToken(token))} {
		if strings.Contains(s, secret) || strings.Contains(s, token) || strings.Contains(s, "leaky-token") {
			t.Fatalf("a credential leaked: %s", s)
		}
	}
	if _, _, err = FetchToken(ctx, nil, "not a url", url.Values{}); err == nil {
		t.Fatal("bad token endpoint accepted")
	}
	if _, err = StaticToken("").Token(ctx); err == nil {
		t.Fatal("empty token accepted")
	}
}

// Human tasks of an execution (create, list, get, claim, complete, reject, history): created is
// false for the same task again; completing a task another worker claimed is refused.
func TestHumanTasksOfAnExecution(t *testing.T) {
	_, srv := newFakeEER(t)
	c := client(t, srv.URL, "tok-operator", nil)
	ctx := context.Background()
	s := sub("task.1", true)
	if _, err := c.Submit(ctx, s); err != nil {
		t.Fatal(err)
	}
	nt := NewHumanTask{TaskID: "task:brant-ht-1", OperationID: s.OperationID, ActorID: "cs-agent-pool", DueAt: s.Deadline,
		Payload: map[string]any{"question": "manual send confirmation"}}
	ht, created, err := c.CreateHumanTask(ctx, nt)
	if err != nil || !created || ht.Status != TaskOpen || ht.ExecutionID != ExecutionID(s.OperationID) {
		t.Fatalf("create %+v %t %v", ht, created, err)
	}
	if _, created, err = c.CreateHumanTask(ctx, nt); err != nil || created {
		t.Fatalf("create again %t %v", created, err)
	}
	list, err := c.HumanTasks(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	if ht, err = c.ClaimHumanTask(ctx, nt.TaskID, "clerk.1"); err != nil || ht.Status != TaskClaimed {
		t.Fatalf("claim %+v %v", ht, err)
	}
	if _, err = c.CompleteHumanTask(ctx, nt.TaskID, "clerk.2", map[string]any{"sent": true}); err == nil {
		t.Fatal("another worker completed the task")
	}
	if ht, err = c.CompleteHumanTask(ctx, nt.TaskID, "clerk.1", map[string]any{"sent": true}); err != nil || ht.Status != TaskCompleted {
		t.Fatalf("complete %+v %v", ht, err)
	}
	if ht, err = c.GetHumanTask(ctx, nt.TaskID); err != nil || ht.Result["sent"] != true {
		t.Fatalf("get %+v %v", ht, err)
	}
	ev, err := c.HumanTaskEvents(ctx, nt.TaskID)
	if err != nil || len(ev) != 3 || ev[2].EventType != "task.completed" {
		t.Fatalf("events %+v %v", ev, err)
	}
	nt2 := nt
	nt2.TaskID = "task:brant-ht-2"
	if _, _, err = c.CreateHumanTask(ctx, nt2); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ClaimHumanTask(ctx, nt2.TaskID, "clerk.1"); err != nil {
		t.Fatal(err)
	}
	if ht, err = c.RejectHumanTask(ctx, nt2.TaskID, "clerk.1", nil); err != nil || ht.Status != TaskRejected {
		t.Fatalf("reject %+v %v", ht, err)
	}
	if _, err = c.GetHumanTask(ctx, "task:none"); !IsNotFound(err) {
		t.Fatalf("missing task: %v", err)
	}
	for _, bad := range []string{"", "nope", "task:"} {
		if _, err = c.GetHumanTask(ctx, bad); !errors.Is(err, runtime.ErrInvalid) {
			t.Fatalf("task ID %q: %v", bad, err)
		}
	}
	if _, _, err = c.CreateHumanTask(ctx, NewHumanTask{TaskID: "task:x", OperationID: s.OperationID}); !errors.Is(err, runtime.ErrInvalid) {
		t.Fatalf("incomplete task: %v", err)
	}
	if _, err = c.ClaimHumanTask(ctx, nt.TaskID, ""); !errors.Is(err, runtime.ErrInvalid) {
		t.Fatalf("no worker: %v", err)
	}
}

// EER states map onto Brant's, and only those.
func TestStatesMapOntoBrants(t *testing.T) {
	for _, c := range []struct{ state, outcome, trigger string }{
		{StateCreated, "", ""}, {StatePlanned, "", ""}, {StateScheduled, "", ""}, {StateExecuting, "", ""}, {StateRecovering, "", ""},
		{StateWaiting, "", TriggerApprovalRequired}, {StateWaiting, "", "human_task_opened"}, {StateFailed, "", ""},
		{StateCompleted, "completed", ""}, {StateCompleted, OutcomeCancelled, ""}, {StateCompleted, "rejected", ""},
	} {
		got, err := stateOf(c.state, c.outcome, c.trigger)
		want := map[string]runtime.State{StateCreated: runtime.Accepted, StatePlanned: runtime.Accepted, StateScheduled: runtime.Accepted,
			StateExecuting: runtime.Running, StateRecovering: runtime.Running, StateFailed: runtime.Failed}[c.state]
		switch {
		case c.state == StateWaiting && c.trigger == TriggerApprovalRequired:
			want = runtime.WaitingApproval
		case c.state == StateWaiting:
			want = runtime.Waiting
		case c.state == StateCompleted && c.outcome == OutcomeCancelled:
			want = runtime.Cancelled
		case c.state == StateCompleted:
			want = runtime.Completed
		}
		if err != nil || got != want {
			t.Fatalf("%+v -> %s %v", c, got, err)
		}
	}
	if _, err := stateOf("Paused", "", ""); !errors.Is(err, runtime.ErrInvalid) {
		t.Fatal("an unknown state was mapped")
	}
}
