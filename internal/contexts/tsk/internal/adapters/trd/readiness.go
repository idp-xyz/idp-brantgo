package trd

import (
	"context"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/application"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
)

// Goals is what TSK reads in TRD to verify a goal (Context Map R18): GoalReadiness v1, and the quote
// read that shows a bound root's object control.
type Goals interface {
	GoalReadiness(ctx context.Context, q trdapi.GoalQuery) (trdapi.GoalAssessment, error)
	GetQuote(ctx context.Context, quoteID string) (trdapi.QuoteView, error)
}

// Readiness is application.Readiness over TRD's GoalReadiness v1 (WBS S4-01d). It asks for the
// task's goal at its version on its quote root with the target customer and RFQ the revision must
// bind, as the principal of ctx (TSK's system actor), and translates the assessment into TSK's
// terms. For a task with a bound root it then reads the root's object control (quote.get), so the
// verification sees whether a control change is under way. TRD's refusals pass unchanged.
type Readiness struct {
	TRD Goals
}

var _ application.Readiness = Readiness{}

// Assess judges g on TRD's records.
func (r Readiness) Assess(ctx context.Context, g application.Goal) (application.Verdict, error) {
	a, err := r.TRD.GoalReadiness(ctx, trdapi.GoalQuery{Goal: g.Code, GoalVersion: g.Version, QuoteID: g.Target.QuoteID,
		CustomerID: g.Target.CustomerID, RfqID: g.Target.RfqID})
	if err != nil {
		return application.Verdict{}, err
	}
	out := Assessment(a)
	if g.Bound != "" {
		v, err := r.TRD.GetQuote(ctx, g.Bound)
		if err != nil {
			return application.Verdict{}, err
		}
		out.Control = &task.RootControl{State: v.Control.State, TaskID: v.Control.TaskID, Epoch: v.Control.ControlEpoch}
	}
	return out, nil
}

// Assessment translates TRD's assessment into TSK's, field by field (assessor
// trd.goal_readiness.v1).
func Assessment(a trdapi.GoalAssessment) task.Assessment {
	out := task.Assessment{Assessor: task.AssessorGoalReadiness, Goal: a.Goal, GoalVersion: a.GoalVersion, QuoteID: a.QuoteID,
		RootVersion: a.RootVersion, BusinessRevision: a.BusinessRevision, BusinessHash: a.BusinessHash, OrderID: a.OrderID,
		OrderRevision: a.OrderRevision, OrderHash: a.OrderHash, Outcome: a.Outcome, Code: a.Code, CheckedAt: a.CheckedAt,
		Assertions: make([]task.Assertion, len(a.Assertions))}
	for i, x := range a.Assertions {
		y := task.Assertion{Assertion: x.Assertion, Answer: x.Answer, Code: x.Code, Checks: make([]task.Check, len(x.Checks)),
			Evidence: make([]task.Evidence, len(x.Evidence))}
		for j, c := range x.Checks {
			y.Checks[j] = task.Check{Check: c.Check, Answer: c.Answer, Code: c.Code, Path: c.Path}
		}
		for j, e := range x.Evidence {
			y.Evidence[j] = task.Evidence{Kind: e.Kind, ID: e.ID, Version: e.Version, Hash: e.Hash}
		}
		out.Assertions[i] = y
	}
	return out
}
