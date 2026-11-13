package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func rejected(t *testing.T, err error, code, path string) {
	t.Helper()
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != code || rej.Fields[0].Path != path {
		t.Fatalf("want %s at %s, got %v", code, path, err)
	}
}

func create() CreateTask {
	return CreateTask{IdempotencyKey: "k1", TaskID: "task.1", GoalCode: GoalOrderActive, ControlMode: ModeAI, AgentID: "agent.1",
		Target: Target{QuoteID: "quote.1"}, Source: &SourceRef{Kind: SourceEVD, ID: "src.1"}, Deadline: "2026-12-01T00:00:00Z",
		Budget:          &Budget{MaxModelCalls: 40, MaxRetries: 3, MaxCost: &CostCap{Currency: "USD", MinorUnits: 2, Amount: "3.00"}},
		ExecutionBundle: &BundleRef{BundleID: "bundle.1", SHA256: strings.Repeat("a", 64)}}
}

// task.create: GOAL_UNSUPPORTED for a goal P0 does not support (catalog 任务与 AI); the closed
// contract otherwise.
func TestCreateTaskContract(t *testing.T) {
	if err := create().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		edit func(*CreateTask)
		code string
		path string
	}{
		{func(c *CreateTask) { c.GoalCode = "order_shipped" }, CodeGoalUnsupported, "/args/goal_code"},
		{func(c *CreateTask) { c.GoalCode = "" }, kernel.CodeSchemaInvalid, "/args/goal_code"},
		{func(c *CreateTask) { c.ControlMode = "auto" }, kernel.CodeSchemaInvalid, "/args/control_mode"},
		{func(c *CreateTask) { c.AgentID = "" }, kernel.CodeSchemaInvalid, "/args/agent_id"},
		{func(c *CreateTask) { c.Target.CustomerID = "bad id" }, kernel.CodeSchemaInvalid, "/args/target/customer_id"},
		{func(c *CreateTask) { c.Source.Kind = "email" }, kernel.CodeSchemaInvalid, "/args/source/kind"},
		{func(c *CreateTask) { c.Deadline = "tomorrow" }, kernel.CodeSchemaInvalid, "/args/deadline"},
		{func(c *CreateTask) { c.Budget.MaxModelCalls = 0 }, kernel.CodeSchemaInvalid, "/args/budget/max_model_calls"},
		{func(c *CreateTask) { c.Budget.MaxCost.Amount = "3.001" }, kernel.CodeSchemaInvalid, "/args/budget/max_cost/amount"},
		{func(c *CreateTask) { c.Budget.MaxCost.Amount = "-1.00" }, kernel.CodeSchemaInvalid, "/args/budget/max_cost/amount"},
		{func(c *CreateTask) { c.Budget.MaxCost.Currency = "usd" }, kernel.CodeSchemaInvalid, "/args/budget/max_cost/currency"},
		{func(c *CreateTask) { c.ExecutionBundle.SHA256 = "x" }, kernel.CodeSchemaInvalid, "/args/execution_bundle/sha256"},
	} {
		c := create()
		b := *c.Budget
		cost := *b.MaxCost
		b.MaxCost = &cost
		c.Budget = &b
		src := *c.Source
		c.Source = &src
		bundle := *c.ExecutionBundle
		c.ExecutionBundle = &bundle
		tc.edit(&c)
		rejected(t, c.Validate(), tc.code, tc.path)
	}
	m := create()
	m.ControlMode, m.AgentID = ModeManual, ""
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	cost, err := create().Budget.MaxCost.Cost()
	if err != nil || cost.MinorString() != "300" || cost.Currency().Code() != "USD" {
		t.Fatalf("%v %v", cost, err)
	}
}

func TestCommandContracts(t *testing.T) {
	ok := []interface{ Validate() error }{
		ScopeTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1},
		WaitTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Wait: WaitInput, Reason: "缺项", ResponsibleID: "alice", Items: []string{"数量"}},
		WaitTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Wait: WaitApproval, Reason: "审核", ResponsibleID: "bob",
			Awaiting: &ObjectRef{Kind: "quote_review", ID: "rev.1", Version: 1}},
		WaitTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Wait: WaitExternal, Reason: "等待客户", ResponsibleID: "alice",
			Awaiting: &ObjectRef{Kind: "customer_response", ID: "quote.1"}, DueAt: "2026-12-01T00:00:00Z"},
		PauseTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Cause: PauseBudgetExhausted, Reason: "预算", ResumeCondition: "追加预算"},
		ResumeTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Arrival: &ObjectRef{Kind: "acceptance", ID: "acc.1", Version: 1}},
		CloseUnfulfilled{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Cause: CloseCustomerDeclined, Reason: "客户拒绝"},
		FailTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Reason: "无法恢复", ResponsibleID: "alice"},
		BindObject{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, QuoteID: "quote.1"},
		TakeControl{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Reason: "接管"},
	}
	for _, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("%T: %v", c, err)
		}
	}
	rejected(t, ScopeTask{IdempotencyKey: "k", TaskID: "t"}.Validate(), kernel.CodeSchemaInvalid, "/args/expected_version")
	rejected(t, WaitTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Wait: WaitInput, Reason: "r", ResponsibleID: "a"}.Validate(),
		kernel.CodeSchemaInvalid, "/args/items")
	rejected(t, WaitTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Wait: WaitExternal, Reason: "r", ResponsibleID: "a",
		Awaiting: &ObjectRef{Kind: "x", ID: "y"}}.Validate(), kernel.CodeSchemaInvalid, "/args/due_at")
	rejected(t, WaitTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Wait: WaitApproval, Reason: "r", ResponsibleID: "a"}.Validate(),
		kernel.CodeSchemaInvalid, "/args/awaiting")
	rejected(t, WaitTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Wait: "approval", Reason: "r", ResponsibleID: "a",
		Awaiting: &ObjectRef{Kind: "Quote", ID: "y"}}.Validate(), kernel.CodeSchemaInvalid, "/args/awaiting/kind")
	rejected(t, PauseTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Cause: "tired", Reason: "r", ResumeCondition: "c"}.Validate(),
		kernel.CodeSchemaInvalid, "/args/cause")
	rejected(t, PauseTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Cause: PauseConflict, Reason: "r"}.Validate(),
		kernel.CodeSchemaInvalid, "/args/resume_condition")
	rejected(t, TakeControl{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Reason: " "}.Validate(), kernel.CodeSchemaInvalid, "/args/reason")
	rejected(t, FailTask{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Reason: "r"}.Validate(), kernel.CodeSchemaInvalid, "/args/responsible_id")
	rejected(t, CloseUnfulfilled{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1, Cause: "done", Reason: "r"}.Validate(),
		kernel.CodeSchemaInvalid, "/args/cause")
	rejected(t, BindObject{IdempotencyKey: "k", TaskID: "t", ExpectedVersion: 1}.Validate(), kernel.CodeSchemaInvalid, "/args/quote_id")
	if v, ok := GoalVersion(GoalQuoteReady); !ok || v != 1 {
		t.Fatal("goal version")
	}
}

func TestRejectPanicsOnUndeclaredCode(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	_ = Reject("TASK_NOT_RUNNING", "/", "")
}
