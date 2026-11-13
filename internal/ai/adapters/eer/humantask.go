package eer

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/runtime"
)

// Human tasks of EER (Runtime API v1): an assignment EER's workflow (or a workload) opens for a
// person of an execution, which a worker claims and completes or rejects. Brant's own human steps
// are TSK waits and the people's commands; an EER human task never decides a Brant fact — its result
// is input that still goes through Brant's commands (Context Map: EER owns no business truth).
//
// EER grants the actions per principal: on the TELOS platform's acceptance configuration people may
// read human tasks, only EER's workloads create them, and nobody is granted claim, complete or
// reject (see the S4 D2 report).

// Human task statuses.
const (
	TaskOpen      = "open"
	TaskClaimed   = "claimed"
	TaskCompleted = "completed"
	TaskRejected  = "rejected"
	TaskExpired   = "expired"
	TaskEscalated = "escalated"
)

// HumanTask is EER's human task.
type HumanTask struct {
	TenantID      string         `json:"tenant_id"`
	TaskID        string         `json:"task_id"`
	SchemaVersion int            `json:"schema_version"`
	ExecutionID   string         `json:"execution_id"`
	ActorID       string         `json:"actor_id"`
	Status        string         `json:"status"`
	Assignee      *string        `json:"assignee"`
	DueAt         string         `json:"due_at"`
	Payload       map[string]any `json:"payload"`
	Result        map[string]any `json:"result"`
	CreatedAt     string         `json:"created_at"`
	UpdatedAt     string         `json:"updated_at"`
}

// HumanTaskEvent is one event of a human task's history.
type HumanTaskEvent struct {
	TaskID     string         `json:"task_id"`
	Sequence   int64          `json:"sequence"`
	EventType  string         `json:"event_type"`
	ActorID    string         `json:"actor_id"`
	OccurredAt string         `json:"occurred_at"`
	Detail     map[string]any `json:"detail"`
}

// NewHumanTask opens a human task of an operation's execution for the EER actor ActorID (a human
// actor of EER's registry) until DueAt.
type NewHumanTask struct {
	TaskID      string
	OperationID string
	ActorID     string
	DueAt       time.Time
	Payload     map[string]any
}

func humanTaskPath(taskID string) (string, error) {
	if !strings.HasPrefix(taskID, "task:") || len(taskID) <= len("task:") || len(taskID) > 256 {
		return "", fmt.Errorf("%w: human task ID %q", runtime.ErrInvalid, taskID)
	}
	return "/v1/human-tasks/" + url.PathEscape(taskID), nil
}

// CreateHumanTask opens a human task; created is false when EER already had it with the same
// content (EER answers 200 then, 201 for a new one).
func (c *Client) CreateHumanTask(ctx context.Context, t NewHumanTask) (task HumanTask, created bool, err error) {
	if _, err = humanTaskPath(t.TaskID); err != nil {
		return HumanTask{}, false, err
	}
	if !runtime.ValidOperationID(t.OperationID) || strings.TrimSpace(t.ActorID) == "" || t.DueAt.IsZero() {
		return HumanTask{}, false, fmt.Errorf("%w: a human task names its operation, actor and due time", runtime.ErrInvalid)
	}
	payload := t.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	body := map[string]any{"tenant_id": c.cfg.TenantID, "task_id": t.TaskID, "schema_version": 1, "execution_id": ExecutionID(t.OperationID),
		"actor_id": t.ActorID, "due_at": stamp(t.DueAt), "payload": payload}
	status, err := c.call(ctx, http.MethodPost, "/v1/human-tasks", body, &task, true)
	return task, status == http.StatusCreated, err
}

// GetHumanTask reads a human task.
func (c *Client) GetHumanTask(ctx context.Context, taskID string) (HumanTask, error) {
	path, err := humanTaskPath(taskID)
	if err != nil {
		return HumanTask{}, err
	}
	var out HumanTask
	_, err = c.call(ctx, http.MethodGet, path, nil, &out, false)
	return out, err
}

// HumanTasks lists the human tasks the caller sees.
func (c *Client) HumanTasks(ctx context.Context) ([]HumanTask, error) {
	var doc struct {
		Items []HumanTask `json:"items"`
	}
	_, err := c.call(ctx, http.MethodGet, "/v1/human-tasks", nil, &doc, false)
	return doc.Items, err
}

// HumanTaskEvents reads a human task's history.
func (c *Client) HumanTaskEvents(ctx context.Context, taskID string) ([]HumanTaskEvent, error) {
	path, err := humanTaskPath(taskID)
	if err != nil {
		return nil, err
	}
	var doc struct {
		TaskID string           `json:"task_id"`
		Items  []HumanTaskEvent `json:"items"`
	}
	_, err = c.call(ctx, http.MethodGet, path+"/events", nil, &doc, false)
	return doc.Items, err
}

// ClaimHumanTask claims a human task for workerID.
func (c *Client) ClaimHumanTask(ctx context.Context, taskID, workerID string) (HumanTask, error) {
	return c.decideTask(ctx, taskID, "claim", map[string]any{"tenant_id": c.cfg.TenantID, "worker_id": workerID}, workerID)
}

// CompleteHumanTask completes a claimed human task with result.
func (c *Client) CompleteHumanTask(ctx context.Context, taskID, workerID string, result map[string]any) (HumanTask, error) {
	return c.decideTask(ctx, taskID, "complete", map[string]any{"tenant_id": c.cfg.TenantID, "worker_id": workerID, "result": nonNil(result)}, workerID)
}

// RejectHumanTask rejects a claimed human task with result (the reasons).
func (c *Client) RejectHumanTask(ctx context.Context, taskID, workerID string, result map[string]any) (HumanTask, error) {
	return c.decideTask(ctx, taskID, "reject", map[string]any{"tenant_id": c.cfg.TenantID, "worker_id": workerID, "result": nonNil(result)}, workerID)
}

func (c *Client) decideTask(ctx context.Context, taskID, verb string, body map[string]any, workerID string) (HumanTask, error) {
	path, err := humanTaskPath(taskID)
	if err != nil {
		return HumanTask{}, err
	}
	if strings.TrimSpace(workerID) == "" || len(workerID) > 256 {
		return HumanTask{}, fmt.Errorf("%w: worker ID", runtime.ErrInvalid)
	}
	var out HumanTask
	_, err = c.call(ctx, http.MethodPost, path+"/"+verb, body, &out, true)
	return out, err
}

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
