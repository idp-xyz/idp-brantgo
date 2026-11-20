// Package tsk assembles the TSK bounded context (business tasks and their control: the BusinessTask
// lifecycle of SM-TSK-TASK, control modes, the control epoch recorded from TRD and the claims of
// task steps; WBS S4-01). Code outside TSK uses it through package api; everything under internal/
// is private to TSK.
package tsk

import (
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/adapters/events"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/adapters/reconcile"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/adapters/trd"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/application"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
)

// Module is TSK wired to PostgreSQL through its own IDP-Bento-Go transaction owner.
type Module struct {
	application.Service
}

var (
	_ api.Commands   = (*Module)(nil)
	_ api.Queries    = (*Module)(nil)
	_ api.Auditor    = (*Module)(nil)
	_ api.Reconciler = (*Module)(nil)

	_ api.ActionCommands = (*Module)(nil)
	_ api.ActionQueries  = (*Module)(nil)
)

// The ports TSK calls TRD through (R17 ObjectControl commands v1, R18 GoalReadiness v1) and what
// passes through them.
type (
	Upstream       = application.Upstream
	ObjectControl  = application.ObjectControl
	ControlRequest = application.ControlRequest
	Readiness      = application.Readiness
	Goal           = application.Goal
	Verdict        = application.Verdict
	// TRD is what TSK calls in TRD's api: the object control commands and the quote read.
	TRD = trd.TRD
	// Reconcilers are TSK's ports to the ActionReconciliation v1 of the contexts a task step writes
	// (R18, R66–R69; S4 unit B1), and Reconciler, Answer one of them and its answer.
	Reconcilers = application.Reconcilers
	Reconciler  = application.Reconciler
	Answer      = application.Answer
	// ActionReconcilers are those contexts' published reconcilers.
	ActionReconcilers = reconcile.Contexts
)

// ActionReconciliation is TSK's ports to the ActionReconciliation v1 of r's contexts.
func ActionReconciliation(r ActionReconcilers) Reconcilers { return reconcile.Ports(r) }

// TRDControl is the port to TRD's object control over its api: bind, stop, enter_manual, handoff
// and release (R17 ObjectControl commands v1). A change TRD refuses as CONTROL_EPOCH_STALE while it
// holds the root for the same task at a later epoch — a control change of the task whose record in
// TSK was lost — is sent again at TRD's epoch (S4 C1).
func TRDControl(t TRD) ObjectControl { return trd.Control{TRD: t} }

// ControlKey is the idempotency key TSK sends TRD's object control command trdCommand with for task
// taskID at the control epoch the task recorded: every attempt of the TSK command, whatever its own
// key, sends TRD this key, so TRD replays the change it made once (SM-TSK-TASK task.take_control
// "按 action_id 重试").
func ControlKey(taskID, trdCommand string, epoch int64) string {
	return application.DeriveKey(taskID, trdCommand, epoch)
}

// ReadinessNotWired is the port to TRD's GoalReadiness v1 while TRD does not publish it: goal.verify
// answers DEPENDENCY_NOT_SUPPORTED.
func ReadinessNotWired() Readiness { return application.ReadinessNotWired{} }

// New wires TSK to db, a transaction owner that belongs to TSK alone, and to its ports to TRD.
func New(db *bentopg.DB, upstream Upstream) (*Module, error) {
	if !upstream.Complete() {
		return nil, errors.New("tsk: module needs its ports to TRD's object control and goal readiness, and to the action reconciliation of EVD, INQ, SUP, CST and TRD")
	}
	policy, err := authorization.NewPolicy(application.Rules()...)
	if err != nil {
		return nil, err
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	clock := bento.SystemClock{}
	return &Module{application.Service{Clock: clock, Upstream: upstream, Actions: application.StoredActions{}, Ledger: &action.Ledger{
		DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: outbox, Clock: clock, ReplayRead: api.CommandGetTask, Visible: application.Visible, ReconcileAudit: api.ReconcileAudit,
	}}}, nil
}

// Subscriptions are TSK's Inbox consumers on its own transaction owner (repository structure §07):
// tsk.task_progress, which has TRD's integration events v1 resume the waiting tasks they concern and
// start the verification of the tasks whose goal they make verifiable (Context Map R18; PL-04,
// PL-06; SM-TSK-TASK task.resume, task.verify).
func (m *Module) Subscriptions() []messaging.Subscription {
	return events.Subscriptions(m.Ledger.DB, m.Service)
}
