//go:build integration

package bootstrap_test

import (
	"context"
	"testing"

	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TSK under the role templates V0.1 (BC-D02, S4 A0): a sales clerk runs a task's human steps —
// create, scope, take over, cancel, fail, close —; a quote reviewer only reads it (tsk.read); the Q01
// AI execution identity (ai_q01_agent) reads the task it is the agent of and gets its claim, and is
// refused every task command even where a template would grant it (AI-INV-004).
func TestTaskCommandsUnderRoleTemplates(t *testing.T) {
	w := newWorld(t)
	sales, reviewer := w.assign("sales.1", "sales_clerk"), w.assign("reviewer.1", "quote_reviewer")
	agentRef := kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.q01"}
	if _, err := w.platform.AssignRoles(context.Background(), w.scope("company.A"), agentRef, []string{"ai_q01_agent"},
		authorization.DataScopeOwn, ""); err != nil {
		t.Fatal(err)
	}
	agent, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: agentRef, Scope: w.scope("company.A")})
	w.must(err)

	r, err := w.c.TSK.CreateTask(sales, tskapi.CreateTask{IdempotencyKey: w.key(), TaskID: "task.1", GoalCode: tskapi.GoalQuoteReady,
		ControlMode: tskapi.ModeAI, AgentID: "agent.q01", Source: &tskapi.SourceRef{Kind: tskapi.SourceEVD, ID: "source.1"}})
	w.must(err)
	_, err = w.c.TSK.CreateTask(reviewer, tskapi.CreateTask{IdempotencyKey: w.key(), TaskID: "task.2", GoalCode: tskapi.GoalQuoteReady,
		ControlMode: tskapi.ModeManual})
	w.want(err, kernel.CodePermissionDenied)
	// created → scoping is the system's: a human with task.scope is refused that transition.
	_, err = w.c.TSK.ScopeTask(sales, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion})
	w.want(err, kernel.CodePermissionDenied)
	if v, err := w.c.TSK.GetTask(reviewer, "task.1"); err != nil || v.OwnerID != "sales.1" {
		t.Fatalf("%+v %v", v, err)
	}
	if v, err := w.c.TSK.GetTask(agent, "task.1"); err != nil || v.AgentID != "agent.q01" {
		t.Fatalf("the agent reads its task: %+v %v", v, err)
	}
	_, err = w.c.TSK.Claim(agent, "task.1")
	w.want(err, kernel.CodeInvalidState) // the agent acts while the task is scoping or running
	_, err = w.c.TSK.PauseTask(agent, tskapi.PauseTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Cause: tskapi.PauseOperator, Reason: "模型自述暂停", ResumeCondition: "无"})
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.c.TSK.TakeControl(reviewer, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "审核人不能接管"})
	w.want(err, kernel.CodePermissionDenied)
	r, err = w.c.TSK.TakeControl(sales, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "人工处理"})
	w.must(err)
	if r.Status != tskapi.StateStopping || r.ControlEpoch != 0 {
		t.Fatalf("%+v", r)
	}
}
