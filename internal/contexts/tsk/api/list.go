package api

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// CommandListTasks is the audited list read (S7-06 read models): a page of the tasks the caller
// sees, most recently changed first (kernel.ListCursor), under tsk.read. A task is seen as
// task.get decides it: by the owner and the owner's team (by data scope), the initiator and the
// task's AI agent. The data scope is applied in the query.
const CommandListTasks = "task.list"

// TaskStates are the lifecycle states a TaskQuery may name.
var TaskStates = []string{StateCreated, StateScoping, StateRunning, StateWaitingInput, StateWaitingApproval, StateWaitingExternal,
	StateVerifying, StatePaused, StateStopping, StateCompleted, StateClosedUnfulfilled, StateCancelled, StateFailed}

// TaskQuery asks for a page of tasks. Lifecycle and ControlMode narrow by the task's state;
// RfqID by the RFQ the task targets; QuoteID by the quote it targets or has bound. After is the
// Next of the previous page, empty for the first. Limit is 0 (kernel.DefaultListPage) to
// kernel.MaxListPage.
type TaskQuery struct {
	Lifecycle   string
	ControlMode string
	RfqID       string
	QuoteID     string
	After       string
	Limit       int
}

// Validate checks the closed contract.
func (q TaskQuery) Validate() error {
	switch {
	case q.Lifecycle != "" && !slices.Contains(TaskStates, q.Lifecycle):
		return schema("/lifecycle")
	case q.ControlMode != "" && q.ControlMode != ModeAI && q.ControlMode != ModeManual:
		return schema("/control_mode")
	case q.RfqID != "" && !kernel.ValidIdentifier(q.RfqID):
		return schema("/rfq_id")
	case q.QuoteID != "" && !kernel.ValidIdentifier(q.QuoteID):
		return schema("/quote_id")
	}
	if _, field := kernel.ListPosition(q.After, q.Limit); field != "" {
		return schema("/" + field)
	}
	return nil
}

// TaskSummary is one task of a list: what a monitor shows before the task is opened. BoundQuoteID
// is the quote root bound to the task now, empty when none is; WaitKind is what a waiting task
// waits for (input, approval or external), empty otherwise.
type TaskSummary struct {
	TaskID        string
	GoalCode      string
	Lifecycle     string
	ControlMode   string
	TaskVersion   int64
	OwnerID       string
	AgentID       string
	TargetRfqID   string
	TargetQuoteID string
	BoundQuoteID  string
	WaitKind      string
	Deadline      time.Time
	UpdatedAt     time.Time
}

// TaskPage is a page of a TaskQuery. Next is the After of the following page, empty on the last.
type TaskPage struct {
	Tasks []TaskSummary
	Next  string
}
