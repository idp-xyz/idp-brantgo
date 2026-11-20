package eer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/ai/runtime"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// EER's states of an execution (Runtime API v1 Execution Record).
const (
	StateCreated    = "Created"
	StatePlanned    = "Planned"
	StateScheduled  = "Scheduled"
	StateExecuting  = "Executing"
	StateWaiting    = "Waiting"
	StateRecovering = "Recovering"
	StateFailed     = "Failed"
	StateCompleted  = "Completed"

	// TriggerApprovalRequired is the trigger of the transition into an approval wait.
	TriggerApprovalRequired = "approval_required"
	// OutcomeCancelled is the outcome of a cancelled execution.
	OutcomeCancelled = "cancelled"
)

// ExecutionID, IntentID and EventID are EER's IDs of a Brant operation.
func ExecutionID(operationID string) string { return "execution:" + operationID }

// IntentID is the intent of a Brant operation at EER.
func IntentID(operationID string) string { return "intent:" + operationID }

// EventID is the originating event of a Brant operation at EER.
func EventID(operationID string) string { return "event:" + operationID }

// Wire documents of Runtime API v1 (written from its description; only the members Brant uses).
type (
	eventRequest struct {
		TenantID      string         `json:"tenant_id"`
		EventID       string         `json:"event_id"`
		EventType     string         `json:"event_type"`
		OccurredAt    string         `json:"occurred_at"`
		Source        string         `json:"source"`
		Subject       string         `json:"subject"`
		SchemaVersion int            `json:"schema_version"`
		CorrelationID string         `json:"correlation_id"`
		Payload       map[string]any `json:"payload"`
	}
	intentDoc struct {
		TenantID          string         `json:"tenant_id"`
		IntentID          string         `json:"intent_id"`
		SchemaVersion     int            `json:"schema_version"`
		Objective         string         `json:"objective"`
		OriginatingEvents []string       `json:"originating_events"`
		CorrelationID     string         `json:"correlation_id"`
		Constraints       map[string]any `json:"constraints"`
		PolicyID          string         `json:"policy_id"`
		PolicyVersion     int            `json:"policy_version"`
		CreatedAt         string         `json:"created_at"`
	}
	intentRequest struct {
		Intent             intentDoc      `json:"intent"`
		ExecutionID        string         `json:"execution_id"`
		PlanID             string         `json:"plan_id"`
		PlanVersion        *int           `json:"plan_version,omitempty"`
		CompilationContext map[string]any `json:"compilation_context"`
	}
	intentAck struct {
		TenantID         string `json:"tenant_id"`
		ExecutionID      string `json:"execution_id"`
		IntentID         string `json:"intent_id"`
		WorkflowID       string `json:"workflow_id"`
		PlanID           string `json:"plan_id"`
		State            string `json:"state"`
		RequestStatus    string `json:"request_status"`
		IdempotentReplay bool   `json:"idempotent_replay"`
	}
	transitionDoc struct {
		Sequence  int64   `json:"sequence"`
		FromState *string `json:"from_state"`
		ToState   string  `json:"to_state"`
		Trigger   string  `json:"trigger"`
		At        string  `json:"occurred_at"`
		StepID    *string `json:"step_id"`
		ActorID   *string `json:"actor_id"`
		Attempt   *int    `json:"attempt"`
		Reason    *string `json:"reason"`
	}
	auditDoc struct {
		Sequence  int64   `json:"sequence"`
		Action    string  `json:"action"`
		ActorID   string  `json:"actor_id"`
		At        string  `json:"occurred_at"`
		Reason    string  `json:"reason"`
		PolicyRef *string `json:"policy_ref"`
	}
	executionDoc struct {
		TenantID            string          `json:"tenant_id"`
		ExecutionID         string          `json:"execution_id"`
		SchemaVersion       int             `json:"schema_version"`
		IntentID            string          `json:"intent_id"`
		CorrelationID       string          `json:"correlation_id"`
		State               string          `json:"state"`
		Outcome             *string         `json:"outcome"`
		Transitions         []transitionDoc `json:"transitions"`
		Audit               []auditDoc      `json:"audit"`
		ParticipatingActors []string        `json:"participating_actors"`
		UpdatedAt           string          `json:"updated_at"`
	}
	approvalRequest struct {
		TenantID string `json:"tenant_id"`
		Decision string `json:"decision"`
		ActorID  string `json:"actor_id"`
	}
	cancellationRequest struct {
		TenantID string `json:"tenant_id"`
		ActorID  string `json:"actor_id"`
		Reason   string `json:"reason"`
	}
	ackDoc struct {
		TenantID    string `json:"tenant_id"`
		ExecutionID string `json:"execution_id"`
		Accepted    bool   `json:"accepted"`
	}
)

// stamp writes an instant as EER reads it (RFC 3339, UTC, microseconds as Brant stores them).
func stamp(t time.Time) string { return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) }

// Submit publishes the operation's originating event, then submits its intent and execution
// (runtime.Port). Both requests derive from s alone, so a resubmission is an idempotent replay at
// EER (Operation.Replayed).
func (c *Client) Submit(ctx context.Context, s runtime.Submission) (runtime.Operation, error) {
	if err := s.Validate(); err != nil {
		return runtime.Operation{}, err
	}
	subject := s.Subject
	if subject == "" {
		subject = "brant_task:" + s.OperationID
	}
	if !entityPattern(subject) {
		return runtime.Operation{}, fmt.Errorf("%w: subject %q", runtime.ErrInvalid, subject)
	}
	event := eventRequest{TenantID: c.cfg.TenantID, EventID: EventID(s.OperationID), EventType: c.cfg.EventType, OccurredAt: stamp(s.RequestedAt),
		Source: c.cfg.EventSource, Subject: subject, SchemaVersion: 1, CorrelationID: s.OperationID,
		Payload: map[string]any{"operation_id": s.OperationID, "objective": s.Objective}}
	if _, err := c.call(ctx, http.MethodPost, "/v1/events", event, nil, true); err != nil {
		return runtime.Operation{}, conflictRejection(err)
	}
	tier := "T1"
	if s.ApprovalRequired {
		tier = "T2"
	}
	constraints := map[string]any{"disposition_tier": tier}
	if !s.Deadline.IsZero() {
		constraints["deadline"] = stamp(s.Deadline)
	}
	inputs := s.Inputs
	if inputs == nil {
		inputs = map[string]any{}
	}
	req := intentRequest{Intent: intentDoc{TenantID: c.cfg.TenantID, IntentID: IntentID(s.OperationID), SchemaVersion: 1, Objective: s.Objective,
		OriginatingEvents: []string{event.EventID}, CorrelationID: s.OperationID, Constraints: constraints, PolicyID: c.cfg.PolicyID,
		PolicyVersion: c.cfg.PolicyVersion, CreatedAt: stamp(s.RequestedAt)},
		ExecutionID: ExecutionID(s.OperationID), PlanID: s.Plan, CompilationContext: inputs}
	if s.PlanVersion > 0 {
		v := s.PlanVersion
		req.PlanVersion = &v
	}
	var ack intentAck
	if _, err := c.call(ctx, http.MethodPost, "/v1/intents", req, &ack, true); err != nil {
		return runtime.Operation{}, conflictRejection(err)
	}
	if ack.ExecutionID != req.ExecutionID || ack.TenantID != c.cfg.TenantID {
		return runtime.Operation{}, fmt.Errorf("%w: EER accepted %s in %s, not %s in %s", runtime.ErrInvalid, ack.ExecutionID, ack.TenantID,
			req.ExecutionID, c.cfg.TenantID)
	}
	op, err := c.Lookup(ctx, s.OperationID)
	if err != nil || op.State == runtime.NotFound {
		// The submission stands; only the read after it failed or does not show it yet.
		state, merr := stateOf(ack.State, "", "")
		if merr != nil {
			return runtime.Operation{}, merr
		}
		op = runtime.Operation{OperationID: s.OperationID, State: state, RuntimeState: ack.State, RuntimeRef: ack.ExecutionID}
	}
	op.Replayed = ack.IdempotentReplay
	return op, nil
}

// conflictRejection adds IDEMPOTENCY_PAYLOAD_CONFLICT to an identity conflict.
func conflictRejection(err error) error {
	if errors.Is(err, runtime.ErrConflict) {
		return errors.Join(err, kernel.Reject(kernel.CodeIdempotencyPayloadConflict, "/operation_id",
			"The runtime holds the operation ID with other content"))
	}
	return err
}

// Lookup reads the operation's execution (runtime.Port); one EER does not have is runtime.NotFound.
func (c *Client) Lookup(ctx context.Context, operationID string) (runtime.Operation, error) {
	doc, err := c.Execution(ctx, operationID)
	if IsNotFound(err) {
		return runtime.Operation{OperationID: operationID, State: runtime.NotFound}, nil
	}
	if err != nil {
		return runtime.Operation{}, err
	}
	return c.operationOf(operationID, doc)
}

// Execution reads the raw execution record of an operation.
func (c *Client) Execution(ctx context.Context, operationID string) (ExecutionRecord, error) {
	if !runtime.ValidOperationID(operationID) {
		return ExecutionRecord{}, fmt.Errorf("%w: operation ID %q", runtime.ErrInvalid, operationID)
	}
	var doc executionDoc
	if _, err := c.call(ctx, http.MethodGet, "/v1/executions/"+url.PathEscape(ExecutionID(operationID)), nil, &doc, false); err != nil {
		return ExecutionRecord{}, err
	}
	return ExecutionRecord(doc), nil
}

// ExecutionRecord is EER's execution record of an operation.
type ExecutionRecord executionDoc

func (c *Client) operationOf(operationID string, doc ExecutionRecord) (runtime.Operation, error) {
	if doc.ExecutionID != ExecutionID(operationID) || doc.TenantID != c.cfg.TenantID {
		return runtime.Operation{}, fmt.Errorf("%w: EER answered %s in %s for %s", runtime.ErrInvalid, doc.ExecutionID, doc.TenantID, operationID)
	}
	outcome := ""
	if doc.Outcome != nil {
		outcome = *doc.Outcome
	}
	last := ""
	if n := len(doc.Transitions); n > 0 {
		last = doc.Transitions[n-1].Trigger
	}
	state, err := stateOf(doc.State, outcome, last)
	if err != nil {
		return runtime.Operation{}, err
	}
	op := runtime.Operation{OperationID: operationID, State: state, Outcome: outcome, RuntimeState: doc.State, RuntimeRef: doc.ExecutionID,
		UpdatedAt: parseInstant(doc.UpdatedAt)}
	for _, t := range doc.Transitions {
		tr := runtime.Transition{Sequence: t.Sequence, To: t.ToState, Trigger: t.Trigger, At: parseInstant(t.At)}
		if t.FromState != nil {
			tr.From = *t.FromState
		}
		if t.Reason != nil {
			tr.Reason = *t.Reason
		}
		op.Transitions = append(op.Transitions, tr)
	}
	for _, a := range doc.Audit {
		op.Audit = append(op.Audit, runtime.AuditEntry{Sequence: a.Sequence, Action: a.Action, ActorID: a.ActorID, At: parseInstant(a.At), Reason: a.Reason})
	}
	return op, nil
}

// stateOf translates EER's state (with the outcome and the last transition's trigger) into Brant's.
func stateOf(state, outcome, lastTrigger string) (runtime.State, error) {
	switch state {
	case StateCreated, StatePlanned, StateScheduled:
		return runtime.Accepted, nil
	case StateExecuting, StateRecovering:
		return runtime.Running, nil
	case StateWaiting:
		if lastTrigger == TriggerApprovalRequired {
			return runtime.WaitingApproval, nil
		}
		return runtime.Waiting, nil
	case StateFailed:
		return runtime.Failed, nil
	case StateCompleted:
		if outcome == OutcomeCancelled {
			return runtime.Cancelled, nil
		}
		return runtime.Completed, nil
	}
	return "", fmt.Errorf("%w: EER state %q is not one Brant knows", runtime.ErrInvalid, state)
}

func parseInstant(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// MaxReason bounds a cancellation reason (EER: 2048 characters).
const MaxReason = 2048

// Cancel asks EER to cancel the operation's execution (runtime.Port) and answers the execution as
// EER then shows it: cancellation is a signal EER's workflow takes at its next safe point, so the
// answer is usually not yet ended (runtime.Barrier waits). An execution that has completed (EER's
// cancellation_state_conflict) or that EER does not have answers as it stands.
func (c *Client) Cancel(ctx context.Context, operationID, reason string) (runtime.Operation, error) {
	if !runtime.ValidOperationID(operationID) {
		return runtime.Operation{}, fmt.Errorf("%w: operation ID %q", runtime.ErrInvalid, operationID)
	}
	if strings.TrimSpace(reason) == "" || utf8.RuneCountInString(reason) > MaxReason {
		return runtime.Operation{}, fmt.Errorf("%w: a cancellation reason of 1 to %d characters", runtime.ErrInvalid, MaxReason)
	}
	id := ExecutionID(operationID)
	var ack ackDoc
	_, err := c.call(ctx, http.MethodPost, "/v1/executions/"+url.PathEscape(id)+"/cancellation",
		cancellationRequest{TenantID: c.cfg.TenantID, ActorID: c.cfg.ActorID, Reason: reason}, &ack, true)
	switch {
	case err == nil && (!ack.Accepted || ack.ExecutionID != id):
		return runtime.Operation{}, fmt.Errorf("%w: EER's cancellation answer %+v", runtime.ErrInvalid, ack)
	case err != nil && !IsNotFound(err) && !errors.Is(err, errStateConflict):
		return runtime.Operation{}, err
	}
	return c.Lookup(ctx, operationID)
}

// Decide approves or rejects the operation's approval wait (runtime.Port) as Config.ActorID;
// EER's approval_state_conflict is runtime.ErrNotWaiting.
func (c *Client) Decide(ctx context.Context, operationID string, approve bool) (runtime.Operation, error) {
	if !runtime.ValidOperationID(operationID) {
		return runtime.Operation{}, fmt.Errorf("%w: operation ID %q", runtime.ErrInvalid, operationID)
	}
	decision := "rejected"
	if approve {
		decision = "approved"
	}
	id := ExecutionID(operationID)
	var ack struct {
		ackDoc
		Decision string `json:"decision"`
	}
	if _, err := c.call(ctx, http.MethodPost, "/v1/executions/"+url.PathEscape(id)+"/approval",
		approvalRequest{TenantID: c.cfg.TenantID, Decision: decision, ActorID: c.cfg.ActorID}, &ack, true); err != nil {
		if IsNotFound(err) {
			return runtime.Operation{}, errors.Join(runtime.ErrNotWaiting, err)
		}
		return runtime.Operation{}, err
	}
	if !ack.Accepted || ack.ExecutionID != id || ack.Decision != decision {
		return runtime.Operation{}, fmt.Errorf("%w: EER's approval answer %+v", runtime.ErrInvalid, ack)
	}
	return c.Lookup(ctx, operationID)
}

// Invocation is EER's evidence of one Actor invocation of an execution step.
type Invocation struct {
	IdempotencyKey  string         `json:"idempotency_key"`
	ExecutionID     string         `json:"execution_id"`
	StepID          string         `json:"step_id"`
	Attempt         int            `json:"attempt"`
	DeliveryAttempt int            `json:"delivery_attempt"`
	ActorID         string         `json:"actor_id"`
	Capability      string         `json:"capability"`
	Result          map[string]any `json:"result"`
}

// Invocations reads the Actor invocations of the operation's execution.
func (c *Client) Invocations(ctx context.Context, operationID string) ([]Invocation, error) {
	if !runtime.ValidOperationID(operationID) {
		return nil, fmt.Errorf("%w: operation ID %q", runtime.ErrInvalid, operationID)
	}
	var doc struct {
		ExecutionID string       `json:"execution_id"`
		Items       []Invocation `json:"items"`
	}
	if _, err := c.call(ctx, http.MethodGet, "/v1/executions/"+url.PathEscape(ExecutionID(operationID))+"/invocations", nil, &doc, false); err != nil {
		return nil, err
	}
	return doc.Items, nil
}
