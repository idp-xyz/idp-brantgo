// Package runtime is the AI layer's port to an external execution runtime (RuntimePort; WBS S4-05,
// BC-D08; Context Map externalBoundaries "EER（AI 执行运行时候选）→ internal/ai, ACL / Adapter": model
// calls, step organisation and checkpoints may run in EER, business commands still go through Brant's
// application services, and EER owns no quote, order or account truth). It is the shape of Q01 V0.1
// q01/ports.py RuntimePort (submit, lookup) with the two calls Brant's stop barrier and approval wait
// need of a runtime: Cancel and Decide.
//
// What the port does not do: it never fences Brant's business writes. TRD's control epoch does
// (TSK-INV-002; baseline 17.1 "运行时本地租约不作为最终提交依据"), and every step a runtime asks Brant to
// take still goes through internal/ai/execution with the task's claim (decision D-2). The runtime half
// of the stop barrier (Barrier) is that the task's operation at the runtime has ended, so the runtime
// takes no further step of its own after a take-over (baseline 8.5, 17.2 "外部已经在途的操作按 stopping
// 规则隔离冲突").
//
// Unconnected is the runtime of a process without an external runtime: every call is refused with
// ErrNotConnected, never a fake success (Q01 UnconnectedRuntime). The in-repo agent runner
// (internal/ai/execution) is the runtime of S4; the EER adapter (internal/ai/adapters/eer) implements
// this port for live runs on the TELOS platform (decision E-2).
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Port is an external execution runtime.
type Port interface {
	// Submit hands an operation to the runtime under s.OperationID. The same submission is the same
	// operation: submitting it again answers the operation as it stands (Operation.Replayed), and the
	// same ID with other content is ErrConflict. An answer lost after the request may have reached the
	// runtime is ErrUnknown: the caller reconciles with Lookup (ReconcileSubmission), never with a
	// new ID (baseline 17.2: 超时进入 unknown 并查询／人工对账).
	Submit(ctx context.Context, s Submission) (Operation, error)
	// Lookup reads what the runtime knows of an operation; an operation the runtime does not have is
	// State NotFound, not an error.
	Lookup(ctx context.Context, operationID string) (Operation, error)
	// Cancel asks the runtime to stop an operation cooperatively, for reason. The answer is the
	// operation as the runtime then shows it; the stop takes effect when the operation has ended
	// (Barrier). Cancelling an ended operation answers it unchanged.
	Cancel(ctx context.Context, operationID, reason string) (Operation, error)
	// Decide answers an operation's approval wait (approve or reject). An operation that is not
	// waiting for approval is ErrNotWaiting.
	Decide(ctx context.Context, operationID string, approve bool) (Operation, error)
}

// State is an operation's state as Brant reads it.
type State string

const (
	// NotFound: the runtime has no operation under the ID (a submission whose answer was lost and
	// that did not arrive).
	NotFound State = "not_found"
	// Accepted: the runtime reserved or planned the operation; no step has run.
	Accepted State = "accepted"
	// Running: a step is executing, or the runtime is recovering one.
	Running State = "running"
	// WaitingApproval: the operation waits for a human approval decision (Decide).
	WaitingApproval State = "waiting_approval"
	// Waiting: the operation waits for something else (a human task, a schedule).
	Waiting State = "waiting"
	// Failed: a step failed; the runtime may still recover the operation, so it has not ended.
	Failed State = "failed"
	// Completed: the operation ended with an outcome other than cancelled.
	Completed State = "completed"
	// Cancelled: the operation ended because it was cancelled.
	Cancelled State = "cancelled"
)

// Ended reports whether the runtime takes no further step of the operation.
func (s State) Ended() bool { return s == Completed || s == Cancelled }

// Submission is what Brant hands a runtime. Every field is fixed when Brant records the operation,
// so a resubmission after a crash or a lost answer is the identical request.
type Submission struct {
	// OperationID is the operation's stable ID at the runtime (OperationOf).
	OperationID string
	// TaskID is the Brant task the operation serves (correlation; the runtime gets no authority from
	// it).
	TaskID string
	// Objective is the operation's goal in words.
	Objective string
	// Plan and PlanVersion name what the runtime runs (its registered plan; 0: the runtime's current
	// version).
	Plan        string
	PlanVersion int
	// ApprovalRequired: the runtime waits for a human approval decision before its first step.
	ApprovalRequired bool
	// Subject is the entity the operation concerns, as "<type>:<id>" (empty: the task).
	Subject string
	// Inputs are the plan's inputs.
	Inputs map[string]any
	// RequestedAt is when Brant recorded the operation (UTC), and Deadline when the runtime stops
	// waiting for it (the task's deadline; zero: none).
	RequestedAt time.Time
	Deadline    time.Time
}

// Validate checks what every runtime needs.
func (s Submission) Validate() error {
	switch {
	case !ValidOperationID(s.OperationID):
		return fmt.Errorf("%w: operation ID %q", ErrInvalid, s.OperationID)
	case !kernel.ValidIdentifier(s.TaskID):
		return fmt.Errorf("%w: task ID", ErrInvalid)
	case s.Objective == "" || s.Plan == "":
		return fmt.Errorf("%w: an objective and a plan", ErrInvalid)
	case s.RequestedAt.IsZero():
		return fmt.Errorf("%w: the time Brant recorded the operation", ErrInvalid)
	case !s.Deadline.IsZero() && !s.Deadline.After(s.RequestedAt):
		return fmt.Errorf("%w: a deadline after the request", ErrInvalid)
	}
	return nil
}

// Operation is an operation as the runtime shows it, with the runtime's own evidence.
type Operation struct {
	OperationID string
	State       State
	// Outcome is the runtime's outcome of an ended operation (completed, cancelled, rejected, ...).
	Outcome string
	// RuntimeState is the runtime's own state name; RuntimeRef its own ID of the operation (EER's
	// execution and workflow).
	RuntimeState string
	RuntimeRef   string
	// Replayed: Submit found the operation already submitted with the same content.
	Replayed    bool
	Transitions []Transition
	Audit       []AuditEntry
	UpdatedAt   time.Time
}

// Transition is one state change the runtime recorded.
type Transition struct {
	Sequence int64
	From, To string
	Trigger  string
	At       time.Time
	Reason   string
}

// AuditEntry is one entry of the runtime's audit of the operation.
type AuditEntry struct {
	Sequence int64
	Action   string
	ActorID  string
	At       time.Time
	Reason   string
}

// LastTrigger is the trigger of the operation's latest transition ("" when none is known).
func (o Operation) LastTrigger() string {
	if len(o.Transitions) == 0 {
		return ""
	}
	return o.Transitions[len(o.Transitions)-1].Trigger
}

var (
	// ErrNotConnected is a process without an external runtime (Unconnected).
	ErrNotConnected = errors.New("runtime: no external runtime is connected")
	// ErrUnknown is a write whose answer did not arrive after the request may have reached the
	// runtime: the outcome is unknown until Lookup settles it (EXTERNAL_RESULT_UNKNOWN; baseline 17.2).
	ErrUnknown = errors.New("runtime: the outcome of the request is unknown")
	// ErrUnavailable is a runtime that cannot answer now; the request did not reach it, or a read
	// failed. Retryable.
	ErrUnavailable = errors.New("runtime: unavailable")
	// ErrConflict is an operation ID the runtime holds with other content.
	ErrConflict = errors.New("runtime: the operation ID is bound to other content")
	// ErrNotWaiting is a decision for an operation that is not waiting for approval.
	ErrNotWaiting = errors.New("runtime: the operation is not waiting for approval")
	// ErrDenied is a request the runtime refused to the caller's identity.
	ErrDenied = errors.New("runtime: the runtime refused the caller")
	// ErrInvalid is a request the port or the runtime refuses as malformed.
	ErrInvalid = errors.New("runtime: invalid request")
)

// CodeNotConnected is the rejection code of Unconnected: error catalog DEPENDENCY_NOT_SUPPORTED
// (422, not retryable, "未交付的依赖路径"); the catalog has no code of its own for a runtime.
const CodeNotConnected = "DEPENDENCY_NOT_SUPPORTED"

func notConnected() error {
	return errors.Join(ErrNotConnected,
		kernel.RejectAs(CodeNotConnected, kernel.CategoryInvalid, "/runtime", "No external execution runtime is connected"))
}

// Unconnected is the runtime of a process without an external runtime.
type Unconnected struct{}

var _ Port = Unconnected{}

// Submit refuses: nothing is submitted.
func (Unconnected) Submit(context.Context, Submission) (Operation, error) {
	return Operation{}, notConnected()
}

// Lookup refuses: an unconnected runtime knows nothing, which is not "not found".
func (Unconnected) Lookup(context.Context, string) (Operation, error) {
	return Operation{}, notConnected()
}

// Cancel refuses.
func (Unconnected) Cancel(context.Context, string, string) (Operation, error) {
	return Operation{}, notConnected()
}

// Decide refuses.
func (Unconnected) Decide(context.Context, string, bool) (Operation, error) {
	return Operation{}, notConnected()
}

// OperationOf is the stable operation ID of a task's operation at a runtime: the same task in the
// same tenant and company is always the same operation, so a restarted process finds it (recovery)
// and a resubmission is the same request. The ID names no tenant, company or task in clear.
func OperationOf(scope kernel.Scope, taskID string) string {
	sum := sha256.Sum256([]byte("brant.runtime.operation\x1f" + string(scope.Tenant) + "\x1f" + string(scope.Company) + "\x1f" + taskID))
	return "brant-" + hex.EncodeToString(sum[:20])
}

// ValidOperationID reports whether id can name an operation: 1 to 120 characters of [a-z0-9._-],
// starting with a letter.
func ValidOperationID(id string) bool {
	if len(id) == 0 || len(id) > 120 || id[0] < 'a' || id[0] > 'z' {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// Settlement is where the runtime half of a stop barrier stands.
type Settlement struct {
	Operation Operation
	// Settled: the runtime takes no further step of the operation (it has ended, or the runtime
	// never had it).
	Settled bool
	// CancelRequested: this call asked the runtime to cancel the operation.
	CancelRequested bool
}

// Barrier is the runtime half of a task's stop barrier (SM-TSK-TASK task.finish_stop "屏障完成";
// baseline 8.5, 17.2): the operation is settled when the runtime does not have it or it has ended;
// otherwise Barrier asks the runtime to cancel it (for reason) and reports it unsettled — the caller
// tries again later and finishes the stop only once it is settled. A cancellation whose answer was
// lost is looked up once; the next call asks again (Cancel is idempotent at the runtime).
func Barrier(ctx context.Context, p Port, operationID, reason string) (Settlement, error) {
	op, err := p.Lookup(ctx, operationID)
	if err != nil {
		return Settlement{}, err
	}
	if op.State == NotFound || op.State.Ended() {
		return Settlement{Operation: op, Settled: true}, nil
	}
	after, err := p.Cancel(ctx, operationID, reason)
	switch {
	case errors.Is(err, ErrUnknown):
		var lerr error
		if after, lerr = p.Lookup(ctx, operationID); lerr != nil {
			return Settlement{Operation: op, CancelRequested: true}, errors.Join(err, lerr)
		}
	case err != nil:
		return Settlement{Operation: op}, err
	}
	return Settlement{Operation: after, Settled: after.State == NotFound || after.State.Ended(), CancelRequested: true}, nil
}

// Reconciliation is what ReconcileSubmission found.
type Reconciliation struct {
	Operation Operation
	// Arrived: the runtime had the operation (the lost answer's request had arrived);
	// otherwise it was submitted again under the same ID.
	Arrived bool
}

// ReconcileSubmission settles a submission whose answer was lost (ErrUnknown): Lookup first — the
// operation is there, so it is not submitted again (one operation, X-INV-001 in the runtime's terms);
// not found, so the identical submission is sent again under the same ID, which the runtime
// deduplicates if the first one arrives late. A lookup that fails leaves the outcome unknown.
func ReconcileSubmission(ctx context.Context, p Port, s Submission) (Reconciliation, error) {
	op, err := p.Lookup(ctx, s.OperationID)
	if err != nil {
		return Reconciliation{}, errors.Join(ErrUnknown, err)
	}
	if op.State != NotFound {
		return Reconciliation{Operation: op, Arrived: true}, nil
	}
	op, err = p.Submit(ctx, s)
	return Reconciliation{Operation: op}, err
}
