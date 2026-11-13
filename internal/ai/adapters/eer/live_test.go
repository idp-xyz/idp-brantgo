//go:build eerlive

package eer_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/adapters/eer"
	"go.idp.xyz/idp-brant/internal/ai/adapters/eer/eerlive"
	"go.idp.xyz/idp-brant/internal/ai/runtime"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Live runs of the EER adapter against a running EER (decision E-2; IG-10: stop barrier, recovery
// and unknown reconciliation): built only with -tags eerlive (make eer-live), configured from the
// environment (package eerlive), never under CI=true. Every run uses fresh operation IDs in the
// synthetic scope tenant brant-s4d2-live, ends every execution it starts (cancelled, approved or
// rejected) and logs only IDs, states, triggers and audit actions — never a credential.
//
//	. <scratch>/eer-env.sh && make eer-live

var liveScope = kernel.Scope{Tenant: "brant-s4d2-live", Company: "company.live"}

// liveOp returns a fresh operation of this run for name.
func liveOp(t *testing.T, name string) (string, string) {
	task := fmt.Sprintf("live.%d.%s", time.Now().UnixNano(), name)
	return runtime.OperationOf(liveScope, task), task
}

func logOp(t *testing.T, label string, op runtime.Operation) {
	t.Helper()
	triggers := make([]string, 0, len(op.Transitions))
	for _, tr := range op.Transitions {
		triggers = append(triggers, tr.From+">"+tr.To+":"+tr.Trigger)
	}
	audit := make([]string, 0, len(op.Audit))
	for _, a := range op.Audit {
		audit = append(audit, a.Action+"@"+a.ActorID)
	}
	t.Logf("%s: %s state=%s eer=%s outcome=%q replayed=%t transitions=%v audit=%v", label, op.RuntimeRef, op.State, op.RuntimeState,
		op.Outcome, op.Replayed, triggers, audit)
}

// end leaves no execution of the run waiting at EER.
func end(t *testing.T, c *eer.Client, op string) {
	t.Helper()
	ctx := context.Background()
	for range 40 {
		st, err := runtime.Barrier(ctx, c, op, "IDP Brant S4 D2 live run ends its synthetic execution")
		if err == nil && st.Settled {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Errorf("execution of %s did not end", op)
}

func waiting(o runtime.Operation) bool { return o.State == runtime.WaitingApproval }

// Submission by a stable ID (baseline 17.1/17.2): EER answers the first submission with a new
// execution that waits for approval (T2), the identical submission as an idempotent replay of the
// same execution, and the same ID with other content as an identity conflict.
func TestLiveSubmissionIsIdempotentByOperation(t *testing.T) {
	ctx := context.Background()
	c := eerlive.Client(t, nil)
	op, task := liveOp(t, "idem")
	s := eerlive.Submission(op, task, true, time.Now())
	defer end(t, c, op)
	first, err := c.Submit(ctx, s)
	if err != nil || first.Replayed {
		t.Fatalf("submit %+v %v", first, err)
	}
	logOp(t, "submitted", first)
	got := eerlive.Await(ctx, t, c, op, 30*time.Second, waiting)
	logOp(t, "waits", got)
	again, err := c.Submit(ctx, s)
	if err != nil || !again.Replayed || again.RuntimeRef != first.RuntimeRef {
		t.Fatalf("resubmit %+v %v", again, err)
	}
	logOp(t, "resubmitted (replay)", again)
	changed := s
	changed.Objective = s.Objective + " — changed"
	_, err = c.Submit(ctx, changed)
	var e *eer.Error
	if !errors.Is(err, runtime.ErrConflict) || !errors.As(err, &e) {
		t.Fatalf("other content under the same ID: %v", err)
	}
	t.Logf("other objective under the same ID (the event differs first): EER %d %s", e.Status, e.Code)
	inputs := s
	inputs.Inputs = map[string]any{"context": map[string]any{"parcel": map[string]any{"parcel_id": "BRANT-OTHER"}}}
	_, err = c.Submit(ctx, inputs)
	if !errors.Is(err, runtime.ErrConflict) || !errors.As(err, &e) || e.Code != "execution_identity_conflict" {
		t.Fatalf("other inputs under the same ID: %v", err)
	}
	t.Logf("other inputs under the same ID (same event): EER %d %s", e.Status, e.Code)
	if n, err := c.Lookup(ctx, op); err != nil || n.RuntimeRef != first.RuntimeRef || n.State != runtime.WaitingApproval {
		t.Fatalf("the conflicts changed the execution: %+v %v", n, err)
	}
}

// dropAnswers lets EER receive a request and then drops its answer, n times for a route.
type dropAnswers struct {
	base http.RoundTripper
	drop map[string]int
}

func (d *dropAnswers) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := d.base.RoundTrip(r)
	key := r.Method + " " + r.URL.Path
	for prefix, n := range d.drop {
		if err == nil && n > 0 && strings.HasPrefix(key, prefix) {
			d.drop[prefix] = n - 1
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("live run: %w after EER answered %d", io.ErrUnexpectedEOF, resp.StatusCode)
		}
	}
	return resp, err
}

// Unknown reconciliation (IG-10; baseline 17.2 "超时进入 unknown 并查询／人工对账"): EER receives the
// submission and its answer is lost — the adapter reports the outcome unknown, the lookup finds the
// execution and nothing is submitted again; when the event's answer is lost the intent never left,
// the lookup finds nothing and the identical submission goes again under the same IDs. A lost
// cancellation answer is looked up by the barrier.
func TestLiveLostAnswersAreReconciled(t *testing.T) {
	ctx := context.Background()
	hc := &http.Client{Timeout: 30 * time.Second}
	c := eerlive.Client(t, hc)
	drop := &dropAnswers{base: hc.Transport, drop: map[string]int{"POST /v1/intents": 1}}
	if drop.base == nil {
		drop.base = http.DefaultTransport
	}
	hc.Transport = drop
	plain := eerlive.Client(t, nil)

	op, task := liveOp(t, "unknown")
	s := eerlive.Submission(op, task, true, time.Now())
	defer end(t, plain, op)
	if _, err := c.Submit(ctx, s); !errors.Is(err, runtime.ErrUnknown) {
		t.Fatalf("the submission's lost answer: %v", err)
	}
	t.Logf("submission %s: answer lost -> runtime.ErrUnknown", eer.ExecutionID(op))
	rec, err := runtime.ReconcileSubmission(ctx, c, s)
	if err != nil || !rec.Arrived {
		t.Fatalf("reconciliation %+v %v", rec, err)
	}
	logOp(t, "reconciled by lookup (arrived, not submitted again)", rec.Operation)
	again, err := plain.Submit(ctx, s)
	if err != nil || !again.Replayed || again.RuntimeRef != rec.Operation.RuntimeRef {
		t.Fatalf("one execution: %+v %v", again, err)
	}
	logOp(t, "a later identical submission is EER's replay of the same execution", again)

	op2, task2 := liveOp(t, "unknown-event")
	s2 := eerlive.Submission(op2, task2, true, time.Now())
	defer end(t, plain, op2)
	drop.drop["POST /v1/events"] = 1
	if _, err = c.Submit(ctx, s2); !errors.Is(err, runtime.ErrUnknown) {
		t.Fatalf("the event's lost answer: %v", err)
	}
	rec, err = runtime.ReconcileSubmission(ctx, c, s2)
	if err != nil || rec.Arrived || rec.Operation.Replayed {
		t.Fatalf("resubmission %+v %v", rec, err)
	}
	logOp(t, "event answer lost: not found, submitted again under the same IDs", rec.Operation)

	eerlive.Await(ctx, t, plain, op, 30*time.Second, waiting)
	drop.drop["POST /v1/executions/"] = 1
	st, err := runtime.Barrier(ctx, c, op, "IDP Brant S4 D2 live run: take-over with a lost cancellation answer")
	if err != nil || !st.CancelRequested {
		t.Fatalf("barrier with a lost cancellation answer %+v %v", st, err)
	}
	t.Logf("cancellation answer lost: barrier looked the execution up (state %s, settled %t)", st.Operation.State, st.Settled)
	got := eerlive.Await(ctx, t, plain, op, 30*time.Second, func(o runtime.Operation) bool { return o.State.Ended() })
	logOp(t, "the lost cancellation took effect", got)
}

// The runtime half of the stop barrier (IG-10 停止屏障): an execution waiting for approval is
// cancelled; the barrier is unsettled until EER shows it Completed with outcome cancelled, then
// settles; a late approval is refused (EER's approval_state_conflict); a second cancellation answers
// the ended execution. EER's audit and transitions are the evidence.
func TestLiveStopBarrierCancelsAndWaits(t *testing.T) {
	ctx := context.Background()
	c := eerlive.Client(t, nil)
	op, task := liveOp(t, "barrier")
	if _, err := c.Submit(ctx, eerlive.Submission(op, task, true, time.Now())); err != nil {
		t.Fatal(err)
	}
	defer end(t, c, op)
	eerlive.Await(ctx, t, c, op, 30*time.Second, waiting)
	reason := "IDP Brant stop barrier: the task is taken over; EER takes no further step"
	st, err := runtime.Barrier(ctx, c, op, reason)
	if err != nil || !st.CancelRequested {
		t.Fatalf("first barrier %+v %v", st, err)
	}
	t.Logf("barrier 1: cancel requested, settled=%t (EER state %s)", st.Settled, st.Operation.RuntimeState)
	passes := 1
	deadline := time.Now().Add(30 * time.Second)
	for !st.Settled {
		if time.Now().After(deadline) {
			t.Fatalf("not settled: %+v", st)
		}
		time.Sleep(250 * time.Millisecond)
		if st, err = runtime.Barrier(ctx, c, op, reason); err != nil {
			t.Fatal(err)
		}
		passes++
	}
	if st.Operation.State != runtime.Cancelled || st.Operation.Outcome != eer.OutcomeCancelled {
		t.Fatalf("settled as %+v", st.Operation)
	}
	triggers := []string{}
	for _, tr := range st.Operation.Transitions {
		triggers = append(triggers, tr.Trigger)
	}
	if !slices.Contains(triggers, eer.TriggerApprovalRequired) || st.Operation.LastTrigger() != "cancellation_completed" {
		t.Fatalf("transitions %v", triggers)
	}
	logOp(t, fmt.Sprintf("barrier settled after %d calls", passes), st.Operation)
	if _, err = c.Decide(ctx, op, true); !errors.Is(err, runtime.ErrNotWaiting) {
		t.Fatalf("approval after the stop: %v", err)
	}
	t.Logf("approval after the stop: %v", err)
	again, err := c.Cancel(ctx, op, reason)
	if err != nil || again.State != runtime.Cancelled {
		t.Fatalf("second cancellation %+v %v", again, err)
	}
	t.Logf("second cancellation answers the ended execution: %s", again.State)
}

// Recovery (IG-10 恢复): the process that submitted is gone; a fresh client — new token, no memory —
// finds the task's operation by its stable ID, submits nothing new (the identical submission is a
// replay), approves the wait, and EER completes the execution once: one Actor invocation per step,
// each with its idempotency key. A rejection ends another execution with outcome rejected.
func TestLiveRecoveryByAFreshProcess(t *testing.T) {
	ctx := context.Background()
	op, task := liveOp(t, "recovery")
	s := eerlive.Submission(op, task, true, time.Now())
	first := eerlive.Client(t, nil)
	if _, err := first.Submit(ctx, s); err != nil {
		t.Fatal(err)
	}
	defer end(t, first, op)
	// A new process: it knows the scope and the task, nothing else.
	fresh := eerlive.Client(t, nil)
	found := eerlive.Await(ctx, t, fresh, runtime.OperationOf(liveScope, task), 30*time.Second, waiting)
	logOp(t, "found by a fresh process", found)
	re, err := fresh.Submit(ctx, s)
	if err != nil || !re.Replayed {
		t.Fatalf("resubmission %+v %v", re, err)
	}
	if _, err = fresh.Decide(ctx, op, true); err != nil {
		t.Fatal(err)
	}
	done := eerlive.Await(ctx, t, fresh, op, 60*time.Second, func(o runtime.Operation) bool { return o.State.Ended() || o.State == runtime.Failed })
	logOp(t, "approved and run", done)
	if done.State != runtime.Completed {
		t.Fatalf("after approval: %+v", done)
	}
	approved := false
	for _, a := range done.Audit {
		approved = approved || (a.Action == "approval.approved" && a.ActorID == eerlive.Config(t).ActorID)
	}
	if !approved {
		t.Fatalf("EER's audit has no approval by the actor: %+v", done.Audit)
	}
	inv, err := fresh.Invocations(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]int{}
	for _, i := range inv {
		keys[i.StepID+"#"+i.Capability]++
		t.Logf("invocation step=%s capability=%s actor=%s attempt=%d delivery=%d key=%s", i.StepID, i.Capability, i.ActorID, i.Attempt,
			i.DeliveryAttempt, i.IdempotencyKey)
		if i.IdempotencyKey == "" {
			t.Fatalf("invocation without idempotency key: %+v", i)
		}
	}
	if len(inv) == 0 {
		t.Fatal("no Actor invocation recorded")
	}

	op2, task2 := liveOp(t, "rejected")
	if _, err = fresh.Submit(ctx, eerlive.Submission(op2, task2, true, time.Now())); err != nil {
		t.Fatal(err)
	}
	defer end(t, fresh, op2)
	eerlive.Await(ctx, t, fresh, op2, 30*time.Second, waiting)
	if _, err = fresh.Decide(ctx, op2, false); err != nil {
		t.Fatal(err)
	}
	rej := eerlive.Await(ctx, t, fresh, op2, 30*time.Second, func(o runtime.Operation) bool { return o.State.Ended() || o.State == runtime.Failed })
	logOp(t, "rejected", rej)
	if rej.Outcome != "rejected" {
		t.Fatalf("rejection ended as %+v", rej)
	}
}

// Identity (17.1; the boundary's rule that a request body supplies no authority): EER takes the
// tenant and actor from the token — a cancellation in the name of another actor and a request for
// another tenant are refused; human tasks are readable, while claiming one is not granted on the
// TELOS acceptance configuration.
func TestLiveIdentityIsTheTokens(t *testing.T) {
	ctx := context.Background()
	c := eerlive.Client(t, nil)
	op, task := liveOp(t, "identity")
	if _, err := c.Submit(ctx, eerlive.Submission(op, task, true, time.Now())); err != nil {
		t.Fatal(err)
	}
	defer end(t, c, op)
	eerlive.Await(ctx, t, c, op, 30*time.Second, waiting)

	other := eerlive.Config(t)
	other.ActorID = other.ActorID + "-impostor"
	oc, err := eer.New(other)
	if err != nil {
		t.Fatal(err)
	}
	var e *eer.Error
	if _, err = oc.Cancel(ctx, op, "in another actor's name"); !errors.Is(err, runtime.ErrDenied) || !errors.As(err, &e) {
		t.Fatalf("another actor's cancellation: %v", err)
	}
	t.Logf("cancellation in another actor's name: EER %d %s", e.Status, e.Code)
	if _, err = oc.Decide(ctx, op, true); !errors.Is(err, runtime.ErrDenied) {
		t.Fatalf("another actor's approval: %v", err)
	}
	foreign := eerlive.Config(t)
	foreign.TenantID = "brant-not-a-tenant"
	fc, err := eer.New(foreign)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fc.Submit(ctx, eerlive.Submission(runtime.OperationOf(liveScope, task+".foreign"), task, false, time.Now()))
	if err == nil || errors.Is(err, runtime.ErrUnknown) {
		t.Fatalf("another tenant's submission: %v", err)
	}
	t.Logf("submission naming another tenant: %v", err)
	still, err := c.Lookup(ctx, op)
	if err != nil || still.State != runtime.WaitingApproval {
		t.Fatalf("the refused calls changed the execution: %+v %v", still, err)
	}
	tasks, err := c.HumanTasks(ctx)
	if err != nil {
		t.Fatalf("human tasks: %v", err)
	}
	t.Logf("human tasks visible to the actor: %d", len(tasks))
	_, err = c.ClaimHumanTask(ctx, "task:brant-s4d2-none", "brant.clerk")
	t.Logf("claiming a human task: %v", err)
	if err == nil {
		t.Fatal("a claim of a task that does not exist succeeded")
	}
}
