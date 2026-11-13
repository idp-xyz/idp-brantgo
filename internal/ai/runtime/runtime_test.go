package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// fake is an in-memory runtime: operations by ID, a cancellation ends an operation after `lag`
// further lookups (cooperative cancellation), and switches lose answers after the write.
type fake struct {
	ops        map[string]*Operation
	subs       map[string]Submission
	cancels    int
	submits    int
	lag        int
	pending    map[string]int
	loseSubmit bool
	loseCancel bool
	lookupErr  error
}

func newFake() *fake {
	return &fake{ops: map[string]*Operation{}, subs: map[string]Submission{}, pending: map[string]int{}}
}

func (f *fake) Submit(_ context.Context, s Submission) (Operation, error) {
	if err := s.Validate(); err != nil {
		return Operation{}, err
	}
	f.submits++
	if op, ok := f.ops[s.OperationID]; ok {
		if f.subs[s.OperationID].Objective != s.Objective {
			return Operation{}, ErrConflict
		}
		out := *op
		out.Replayed = true
		return out, nil
	}
	state := Accepted
	if s.ApprovalRequired {
		state = WaitingApproval
	}
	f.ops[s.OperationID] = &Operation{OperationID: s.OperationID, State: state}
	f.subs[s.OperationID] = s
	if f.loseSubmit {
		f.loseSubmit = false
		return Operation{}, ErrUnknown
	}
	return *f.ops[s.OperationID], nil
}

func (f *fake) Lookup(_ context.Context, id string) (Operation, error) {
	if f.lookupErr != nil {
		return Operation{}, f.lookupErr
	}
	op, ok := f.ops[id]
	if !ok {
		return Operation{OperationID: id, State: NotFound}, nil
	}
	if n, ok := f.pending[id]; ok {
		if n <= 0 {
			op.State, op.Outcome = Cancelled, "cancelled"
			delete(f.pending, id)
		} else {
			f.pending[id] = n - 1
		}
	}
	return *op, nil
}

func (f *fake) Cancel(ctx context.Context, id, reason string) (Operation, error) {
	f.cancels++
	if reason == "" {
		return Operation{}, ErrInvalid
	}
	op, ok := f.ops[id]
	if !ok || op.State.Ended() {
		return f.Lookup(ctx, id)
	}
	if _, ok := f.pending[id]; !ok {
		f.pending[id] = f.lag
	}
	if f.loseCancel {
		f.loseCancel = false
		return Operation{}, ErrUnknown
	}
	return *op, nil
}

func (f *fake) Decide(_ context.Context, id string, approve bool) (Operation, error) {
	op, ok := f.ops[id]
	if !ok || op.State != WaitingApproval {
		return Operation{}, ErrNotWaiting
	}
	if approve {
		op.State, op.Outcome = Completed, "completed"
	} else {
		op.State, op.Outcome = Completed, "rejected"
	}
	return *op, nil
}

var scopeA = kernel.Scope{Tenant: "tenant.1", Company: "company.A"}

func submission(task string) Submission {
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	return Submission{OperationID: OperationOf(scopeA, task), TaskID: task, Objective: "quote ready", Plan: "brant-q01",
		ApprovalRequired: true, RequestedAt: at, Deadline: at.Add(time.Hour)}
}

// Q01 V0.1 q01/ports.py UnconnectedRuntime: an unconnected runtime answers every call with a refusal
// (DEPENDENCY_NOT_SUPPORTED, ErrNotConnected), never a fake success — not even "not found".
func TestUnconnectedRuntimeRefusesEverything(t *testing.T) {
	var p Port = Unconnected{}
	ctx := context.Background()
	_, e1 := p.Submit(ctx, submission("task.1"))
	_, e2 := p.Lookup(ctx, "brant-x")
	_, e3 := p.Cancel(ctx, "brant-x", "take-over")
	_, e4 := p.Decide(ctx, "brant-x", true)
	for i, err := range []error{e1, e2, e3, e4} {
		rej, ok := kernel.AsRejection(err)
		if !errors.Is(err, ErrNotConnected) || !ok || rej.Code != CodeNotConnected || rej.Category != kernel.CategoryInvalid {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := Barrier(ctx, p, "brant-x", "take-over"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("an unconnected runtime settles no barrier: %v", err)
	}
}

// The operation of a task is stable per tenant, company and task (recovery finds it, a resubmission
// is the same request) and names none of them in clear.
func TestOperationOfIsStableAndOpaque(t *testing.T) {
	a := OperationOf(scopeA, "task.1")
	if a != OperationOf(scopeA, "task.1") || !ValidOperationID(a) {
		t.Fatalf("operation %q", a)
	}
	for _, other := range []string{OperationOf(scopeA, "task.2"), OperationOf(kernel.Scope{Tenant: "tenant.1", Company: "company.B"}, "task.1"),
		OperationOf(kernel.Scope{Tenant: "tenant.2", Company: "company.A"}, "task.1")} {
		if other == a {
			t.Fatal("two tasks share an operation")
		}
	}
	if strings.Contains(a, "task") || strings.Contains(a, "tenant") || strings.Contains(a, "company") {
		t.Fatalf("operation %q names the task or scope", a)
	}
	for _, bad := range []string{"", "Brant", "1abc", "a b", "a/b", strings.Repeat("a", 121)} {
		if ValidOperationID(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
	s := submission("task.1")
	s.Deadline = s.RequestedAt
	if err := s.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("deadline at the request: %v", err)
	}
	s = submission("task.1")
	s.RequestedAt = time.Time{}
	if err := s.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no recorded time: %v", err)
	}
}

// The runtime half of the stop barrier (IG-10 停止屏障; SM-TSK-TASK finish_stop; baseline 8.5): an
// operation still running is cancelled and the barrier stays unsettled until the runtime shows it
// ended; asking again does not cancel twice in effect; an operation the runtime never had, or one
// that has ended, settles at once; a cancellation whose answer was lost is looked up.
func TestBarrierSettlesOnlyWhenTheRuntimeOperationEnded(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.lag = 1
	s := submission("task.1")
	if _, err := f.Submit(ctx, s); err != nil {
		t.Fatal(err)
	}
	st, err := Barrier(ctx, f, s.OperationID, "take-over of task.1")
	if err != nil || st.Settled || !st.CancelRequested || st.Operation.State != WaitingApproval {
		t.Fatalf("first barrier %+v %v", st, err)
	}
	st, err = Barrier(ctx, f, s.OperationID, "take-over of task.1")
	if err != nil || st.Settled {
		t.Fatalf("second barrier %+v %v", st, err)
	}
	st, err = Barrier(ctx, f, s.OperationID, "take-over of task.1")
	if err != nil || !st.Settled || st.Operation.State != Cancelled || st.CancelRequested {
		t.Fatalf("third barrier %+v %v", st, err)
	}
	// An ended operation: no cancellation is asked for.
	before := f.cancels
	if st, err = Barrier(ctx, f, s.OperationID, "again"); err != nil || !st.Settled || f.cancels != before {
		t.Fatalf("ended %+v %v (%d cancels)", st, err, f.cancels-before)
	}
	// Nothing at the runtime: settled.
	if st, err = Barrier(ctx, f, OperationOf(scopeA, "task.none"), "take-over"); err != nil || !st.Settled || st.Operation.State != NotFound {
		t.Fatalf("not found %+v %v", st, err)
	}
	// A lost cancellation answer is looked up; the operation is still running, so unsettled.
	s2 := submission("task.2")
	if _, err = f.Submit(ctx, s2); err != nil {
		t.Fatal(err)
	}
	f.loseCancel, f.lag = true, 1
	if st, err = Barrier(ctx, f, s2.OperationID, "take-over"); err != nil || st.Settled || !st.CancelRequested {
		t.Fatalf("lost cancel %+v %v", st, err)
	}
	// The runtime cannot be read: the barrier is not settled and says why.
	f.lookupErr = ErrUnavailable
	if _, err = Barrier(ctx, f, s2.OperationID, "take-over"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreadable runtime: %v", err)
	}
}

// Unknown reconciliation (IG-10, baseline 17.2): a submission whose answer was lost is looked up,
// found, and not submitted again; one that never arrived is submitted again under the same ID; a
// failed lookup leaves the outcome unknown.
func TestReconcileSubmissionNeverSubmitsTwice(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	s := submission("task.1")
	f.loseSubmit = true
	if _, err := f.Submit(ctx, s); !errors.Is(err, ErrUnknown) {
		t.Fatalf("lost answer: %v", err)
	}
	rec, err := ReconcileSubmission(ctx, f, s)
	if err != nil || !rec.Arrived || rec.Operation.State != WaitingApproval || f.submits != 1 {
		t.Fatalf("reconciled %+v %v (%d submits)", rec, err, f.submits)
	}
	s2 := submission("task.2")
	rec, err = ReconcileSubmission(ctx, f, s2)
	if err != nil || rec.Arrived || rec.Operation.State != WaitingApproval || f.submits != 2 {
		t.Fatalf("resubmitted %+v %v", rec, err)
	}
	f.lookupErr = errors.New("connection reset")
	if _, err = ReconcileSubmission(ctx, f, submission("task.3")); !errors.Is(err, ErrUnknown) {
		t.Fatalf("failed lookup: %v", err)
	}
	if !Cancelled.Ended() || !Completed.Ended() || Failed.Ended() || WaitingApproval.Ended() || NotFound.Ended() {
		t.Fatal("ended states")
	}
}
