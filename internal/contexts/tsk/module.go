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
)

// TRDControl is the port to TRD's object control over its api (bind, stop and release; enter_manual
// and handoff answer DEPENDENCY_NOT_SUPPORTED until TRD publishes them).
func TRDControl(t TRD) ObjectControl { return trd.Control{TRD: t} }

// ReadinessNotWired is the port to TRD's GoalReadiness v1 while TRD does not publish it: goal.verify
// answers DEPENDENCY_NOT_SUPPORTED.
func ReadinessNotWired() Readiness { return application.ReadinessNotWired{} }

// New wires TSK to db, a transaction owner that belongs to TSK alone, and to its ports to TRD.
func New(db *bentopg.DB, upstream Upstream) (*Module, error) {
	if !upstream.Complete() {
		return nil, errors.New("tsk: module needs its ports to TRD's object control and goal readiness")
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
	return &Module{application.Service{Clock: clock, Upstream: upstream, Actions: application.NoActions{}, Ledger: &action.Ledger{
		DB: db, Schema: postgres.Schema, Source: api.Source, Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: outbox, Clock: clock, ReplayRead: api.CommandGetTask, Visible: application.Visible,
	}}}, nil
}

// Subscriptions are TSK's Inbox consumers on its own transaction owner (repository structure §07):
// tsk.task_progress, which has TRD's integration events v1 resume the waiting tasks they concern and
// start the verification of the tasks whose goal they make verifiable (Context Map R18; PL-04,
// PL-06; SM-TSK-TASK task.resume, task.verify).
func (m *Module) Subscriptions() []messaging.Subscription {
	return events.Subscriptions(m.Ledger.DB, m.Service)
}
