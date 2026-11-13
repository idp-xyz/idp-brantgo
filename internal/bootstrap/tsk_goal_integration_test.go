//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// goal.verify against TRD's GoalReadiness v1 on PostgreSQL 16 (WBS S4-01d; Context Map R18; SM-TSK-TASK's
// goal.verify rows; TSK-INV-001; OutcomeEvidence; baseline 15.4 完成断言). TSK's system actor asks TRD
// for the task's goal on its quote root before TSK's transaction and records the verification with the
// task; the task completes only on TRD's satisfied assessment. Tests port the goal vectors of the Q01
// V0.1 package (docs/90_ORIGINAL_SOURCES/04_Q01_P0_契约与执行验证包_V0.1.zip, tests/test_execution.py),
// cited as "Q01 V0.1 tests/test_execution.py::<name>". Python leaves an unmet task in verifying; SM-TSK-TASK
// sends it to waiting_input (unsatisfied) or waiting_external (unknown, error), which is what is asserted.

// newGoalWorld is the TRD order world with TSK's system actor, the clerk owning the tasks and the AI
// agent, all provisioned before any binding.
func newGoalWorld(t *testing.T) *tskWorld {
	q := newOrderWorld(t)
	every := append(slices.Clone(allPermissions), tskPermissions...)
	return &tskWorld{trdWorld: q,
		sys: q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "tsk"},
			Permissions: every, DataScope: "company"}),
		clerk: q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk.1"},
			Permissions: every, DataScope: "company"}),
		agent: q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"},
			Permissions: append(slices.Clone(allPermissions), "tsk.read"), DataScope: "company"}),
	}
}

// running creates task id for goal in mode (AI: agent.1) owned by the clerk, targeting the fixture's
// customer and RFQ and quote (none when ""), and scopes it to running.
func (w *tskWorld) running(id, goal, mode, quote string) tskapi.TaskReceipt {
	w.t.Helper()
	c := tskapi.CreateTask{IdempotencyKey: w.key(), TaskID: id, GoalCode: goal, ControlMode: mode,
		Target: tskapi.Target{QuoteID: quote, RfqID: "rfq.1", CustomerID: "customer.1"}}
	if mode == tskapi.ModeAI {
		c.AgentID = "agent.1"
	}
	r := w.task(w.c.TSK.CreateTask(w.clerk, c))
	r = w.task(w.c.TSK.ScopeTask(w.sys, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion}))
	return w.task(w.c.TSK.ScopeTask(w.clerk, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, Note: "范围确认"}))
}

func (w *tskWorld) toVerifying(id string, v int64) tskapi.TaskReceipt {
	w.t.Helper()
	return w.task(w.c.TSK.VerifyTask(w.sys, tskapi.VerifyTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v}))
}

// manualVerifying is a manual task of goal on quote in verifying.
func (w *tskWorld) manualVerifying(id, goal, quote string) tskapi.TaskReceipt {
	w.t.Helper()
	return w.toVerifying(id, w.running(id, goal, tskapi.ModeManual, quote).RowVersion)
}

func (w *tskWorld) verifyGoal(ctx context.Context, id string, v int64) (tskapi.TaskReceipt, error) {
	return w.c.TSK.VerifyGoal(ctx, tskapi.VerifyGoal{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v})
}

// reverify resumes waiting task id on what arrived and moves it to verifying again.
func (w *tskWorld) reverify(id string, v int64, arrival tskapi.ObjectRef) tskapi.TaskReceipt {
	w.t.Helper()
	r := w.task(w.c.TSK.ResumeTask(w.sys, tskapi.ResumeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: v, Arrival: &arrival}))
	return w.toVerifying(id, r.RowVersion)
}

// lastVerification returns the task's latest verification and how many it has.
func (w *tskWorld) lastVerification(id string) (tskapi.GoalVerification, int) {
	w.t.Helper()
	vs, err := w.c.TSK.GoalVerifications(w.clerk, id)
	w.must(err)
	if len(vs) == 0 {
		w.t.Fatalf("%s has no verification", id)
	}
	return vs[len(vs)-1], len(vs)
}

func assertionOf(v tskapi.GoalVerification, name string) tskapi.GoalAssertion {
	for _, a := range v.Assertions {
		if a.Assertion == name {
			return a
		}
	}
	return tskapi.GoalAssertion{}
}

func assertionsOfVerification(v tskapi.GoalVerification) []string {
	var out []string
	for _, a := range v.Assertions {
		s := a.Assertion + "=" + a.Answer
		if a.Code != "" {
			s += ":" + a.Code
		}
		out = append(out, s)
	}
	return out
}

func (w *tskWorld) completedEvents() []event {
	var out []event
	for _, e := range w.events("company.A", tskapi.Source) {
		if e.Type == tskapi.EventTaskCompleted {
			out = append(out, e)
		}
	}
	return out
}

// AN11 (目标 order_active，但仅有有效报价或模型声称订单已建：权威订单及确认不足则保持未完成；quote_ready
// 里程碑不能替代目标) with Q01 V0.1 tests/test_execution.py::test_quote_only_cannot_complete_order_goal,
// ::test_quote_ready_is_a_different_goal, ::test_draft_order_cannot_complete and
// ::test_terminal_task_rejects_new_forward_action: on an activated and accepted quote, an order_active
// task is not met (Q01.order ORDER_MISSING) and goes to waiting_input with TRD's evidence of the
// revision; an arrival that claims an order changes nothing; a quote_ready task on the same quote
// completes (manual) and names no order, and does not complete the order_active one. A draft order
// is not active (ORDER_NOT_ACTIVE, ORDER_APPROVAL_REQUIRED); the activated order completes the task
// with the order, its revision and hash as evidence, and TaskCompleted names them. A completed task
// takes no forward action and stays completed though quote_ready is no longer satisfied once the
// order holds the conversion slot (BC-AT25).
func TestGoalVerifyJudgesTRDsRecords(t *testing.T) {
	w := newGoalWorld(t)
	f, acc := w.accepted("quote.1")
	r := w.manualVerifying("task.order", tskapi.GoalOrderActive, "quote.1")
	r = w.task(w.verifyGoal(w.sys, "task.order", r.RowVersion))
	v, n := w.lastVerification("task.order")
	order := assertionOf(v, trdapi.AssertionOrder)
	if r.Status != tskapi.StateWaitingInput || n != 1 || v.Assessor != tskapi.AssessorGoalReadiness || v.Outcome != tskapi.AnswerUnsatisfied ||
		order.Code != trdapi.CodeOrderMissing || !slices.Contains(v.Unmet, trdapi.AssertionOrder) || v.OrderID != "" ||
		v.BusinessRevision != 1 || v.BusinessHash != f.BusinessHash || v.Control != nil || len(v.Assertions) != 9 {
		t.Fatalf("quote only: %+v %v", r, assertionsOfVerification(v))
	}
	// The model's word that the order exists (an arrival naming one) is no evidence.
	r = w.reverify("task.order", r.RowVersion, tskapi.ObjectRef{Kind: "order", ID: "order.quote.1", Version: 1})
	r = w.task(w.verifyGoal(w.sys, "task.order", r.RowVersion))
	if v, _ = w.lastVerification("task.order"); r.Status != tskapi.StateWaitingInput || assertionOf(v, trdapi.AssertionOrder).Code != trdapi.CodeOrderMissing {
		t.Fatalf("claimed order: %+v %v", r, assertionsOfVerification(v))
	}

	q := w.manualVerifying("task.quote", tskapi.GoalQuoteReady, "quote.1")
	q = w.task(w.verifyGoal(w.sys, "task.quote", q.RowVersion))
	qv, _ := w.lastVerification("task.quote")
	if q.Status != tskapi.StateCompleted || qv.Outcome != tskapi.AnswerSatisfied || qv.OrderID != "" || len(qv.Assertions) != 7 ||
		len(qv.Unmet) != 0 || assertionOf(qv, trdapi.AssertionOrder).Assertion != "" {
		t.Fatalf("quote_ready: %+v %v", q, assertionsOfVerification(qv))
	}
	if tv, err := w.c.TSK.GetTask(w.clerk, "task.quote"); err != nil || tv.CompletionActor != tskapi.CompletionManual {
		t.Fatalf("%+v %v", tv, err)
	}
	if tv, err := w.c.TSK.GetTask(w.clerk, "task.order"); err != nil || tv.Lifecycle != tskapi.StateWaitingInput {
		t.Fatalf("the quote_ready milestone completed the order_active task: %+v %v", tv, err)
	}

	o, err := w.c.TRD.PrepareOrder(w.author, w.prepareCmd("order.quote.1", acc))
	w.must(err)
	r = w.reverify("task.order", r.RowVersion, tskapi.ObjectRef{Kind: "order", ID: o.ObjectID, Version: 1})
	r = w.task(w.verifyGoal(w.sys, "task.order", r.RowVersion))
	v, _ = w.lastVerification("task.order")
	if r.Status != tskapi.StateWaitingInput || v.OrderID != o.ObjectID || assertionOf(v, trdapi.AssertionOrder).Code != trdapi.CodeOrderNotActive ||
		assertionOf(v, trdapi.AssertionApproval).Code != trdapi.CodeOrderApprovalRequired {
		t.Fatalf("draft order: %+v %v", r, assertionsOfVerification(v))
	}

	d := w.reviewed(o, "review.1")
	act, err := w.c.TRD.ActivateOrder(w.author, w.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1"))
	w.must(err)
	r = w.reverify("task.order", r.RowVersion, tskapi.ObjectRef{Kind: "order", ID: o.ObjectID, Version: 1})
	r = w.task(w.verifyGoal(w.sys, "task.order", r.RowVersion))
	v, n = w.lastVerification("task.order")
	ev := assertionOf(v, trdapi.AssertionOrder).Evidence
	if r.Status != tskapi.StateCompleted || n != 4 || v.Outcome != tskapi.AnswerSatisfied || v.OrderID != o.ObjectID || v.OrderRevision != 1 ||
		v.OrderHash != act.OrderHash || v.BusinessRevision != 1 ||
		!slices.Contains(ev, tskapi.GoalEvidence{Kind: trdapi.EvidenceOrder, ID: o.ObjectID, Version: 1, Hash: act.OrderHash}) {
		t.Fatalf("active order: %+v %+v", r, v)
	}
	completed := w.completedEvents()
	if len(completed) != 2 || completed[1].Payload["order_id"] != o.ObjectID || completed[1].Payload["completion_actor"] != tskapi.CompletionManual ||
		completed[0].Payload["order_id"] != "" {
		t.Fatalf("%+v", completed)
	}

	// Terminal: no forward action, no claim; the quote_ready task stays completed though quote_ready no
	// longer holds (the conversion slot is taken).
	_, err = w.verifyGoal(w.sys, "task.order", r.RowVersion)
	w.want(err, kernel.CodeInvalidState)
	_, err = w.c.TSK.Claim(w.clerk, "task.order")
	w.want(err, kernel.CodeInvalidState)
	if a := w.goal(w.sys, trdapi.GoalQuoteReady, "quote.1"); a.Outcome != trdapi.AnswerUnsatisfied {
		t.Fatalf("quote_ready after the conversion: %v", assertionsOf(a))
	}
	if tv, err := w.c.TSK.GetTask(w.clerk, "task.quote"); err != nil || tv.Lifecycle != tskapi.StateCompleted {
		t.Fatalf("%+v %v", tv, err)
	}
}

// Q01 V0.1 tests/test_execution.py::test_takeover_fences_old_agent_and_records_manual_completion (the
// manual completion; A1 ported the fence half), TSK-INV-002, TSK-INV-004, AN08: the AI task's root is
// bound; a person takes over, the barrier puts the root under manual control at epoch 3; the old
// agent's claim is CONTROL_EPOCH_STALE and the agent gets no new one; people prepare, review and
// activate the order with the task's claim; the task resumes, is verified against TRD and completes
// with completion actor manual, its verification showing TRD's manual control at epoch 3.
func TestGoalVerifyRecordsAManualCompletionAfterATakeOver(t *testing.T) {
	w := newGoalWorld(t)
	_, acc := w.accepted("quote.1")
	r := w.running("task.1", tskapi.GoalOrderActive, tskapi.ModeAI, "")
	r = w.task(w.c.TSK.BindObject(w.sys, tskapi.BindObject{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, QuoteID: "quote.1"}))
	old, _ := w.claimed("task.1")
	r = w.task(w.c.TSK.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "模型持续不可用，人工接管"}))
	r = w.task(w.c.TSK.FinishStop(w.sys, tskapi.FinishStop{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion}))
	if r.Status != tskapi.StatePaused || r.ControlMode != tskapi.ModeManual || r.ControlEpoch != 3 {
		t.Fatalf("%+v", r)
	}
	_, err := w.c.TRD.PrepareOrder(old, w.prepareCmd("order.quote.1", acc))
	w.want(err, trdapi.CodeControlEpochStale)
	_, err = w.c.TSK.Claim(w.agent, "task.1")
	w.want(err, tskapi.CodeControlModeMismatch)
	hc, err := w.c.TSK.Claim(w.clerk, "task.1")
	w.must(err)
	author, reviewer := w.claimFor(w.author, "task.1", hc.ControlEpoch), w.claimFor(w.reviewer, "task.1", hc.ControlEpoch)
	o, err := w.c.TRD.PrepareOrder(author, w.prepareCmd("order.quote.1", acc))
	w.must(err)
	req, err := w.c.TRD.RequestOrderReview(author, w.requestCmd(o.ObjectID, w.orderVersion(o.ObjectID), "review.1"))
	w.must(err)
	d, err := w.c.TRD.DecideOrderReview(reviewer, w.decideCmd(o.ObjectID, req.RowVersion, "review.1", o.OrderHash))
	w.must(err)
	w.readyToActivate(o.ObjectID)
	_, err = w.c.TRD.ActivateOrder(author, w.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1"))
	w.must(err)

	r = w.task(w.c.TSK.ResumeTask(w.clerk, tskapi.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Reason: "订单已人工生效"}))
	r = w.toVerifying("task.1", r.RowVersion)
	r = w.task(w.verifyGoal(w.sys, "task.1", r.RowVersion))
	v, _ := w.lastVerification("task.1")
	tv, err := w.c.TSK.GetTask(w.clerk, "task.1")
	w.must(err)
	if r.Status != tskapi.StateCompleted || tv.CompletionActor != tskapi.CompletionManual || v.OrderID != o.ObjectID ||
		v.Control == nil || *v.Control != (tskapi.RootControlView{State: trdapi.ControlManual, TaskID: "task.1", ControlEpoch: 3}) {
		t.Fatalf("%+v %+v %+v", r, tv, v)
	}
}

// stopThenLose is TRD's api for a TSK whose take-over loses the answer of TRD's stop: TRD stops the root,
// TSK does not record it (a crash between the two steps).
type stopThenLose struct{ tsk.TRD }

func (s stopThenLose) StopControl(ctx context.Context, cmd trdapi.StopControl) (trdapi.Receipt, error) {
	if _, err := s.TRD.StopControl(ctx, cmd); err != nil {
		return trdapi.Receipt{}, err
	}
	return trdapi.Receipt{}, errors.New("connection lost after TRD's stop")
}

// SM-TSK-TASK "快照版本仍匹配且无冲突", TSK-INV-002: a bound AI task whose quote_ready goal TRD finds
// satisfied completes with ai_with_required_review, the verification showing TRD's control at the
// task's epoch. When a take-over has stopped the root at TRD and TSK has not recorded it yet (its
// second step failed), goal.verify is CONTROL_EPOCH_STALE and records nothing: the task stays
// verifying until the take-over is sent again, replays TRD's stop and records it.
func TestGoalVerifyOfABoundTaskFollowsTRDsControl(t *testing.T) {
	w := newGoalWorld(t)
	w.activated("quote.1")
	w.activated("quote.2")
	bound := func(id, quote string) tskapi.TaskReceipt {
		r := w.running(id, tskapi.GoalQuoteReady, tskapi.ModeAI, "")
		r = w.task(w.c.TSK.BindObject(w.sys, tskapi.BindObject{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, QuoteID: quote}))
		return w.toVerifying(id, r.RowVersion)
	}
	r := bound("task.1", "quote.1")
	r = w.task(w.verifyGoal(w.sys, "task.1", r.RowVersion))
	v, _ := w.lastVerification("task.1")
	tv, err := w.c.TSK.GetTask(w.clerk, "task.1")
	w.must(err)
	if r.Status != tskapi.StateCompleted || tv.CompletionActor != tskapi.CompletionAIWithRequiredReview || v.Control == nil ||
		*v.Control != (tskapi.RootControlView{State: trdapi.ControlBound, TaskID: "task.1", ControlEpoch: 1}) {
		t.Fatalf("%+v %+v", r, v)
	}

	r = bound("task.2", "quote.2")
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	lossy, err := tsk.New(db, tsk.Upstream{Control: tsk.TRDControl(stopThenLose{w.c.TRD}), Readiness: tsk.TRDReadiness(w.c.TRD),
		Reconcilers: tsk.ActionReconciliation(tsk.ActionReconcilers{EVD: w.c.EVD, INQ: w.c.INQ, SUP: w.c.SUP, CST: w.c.CST, TRD: w.c.TRD})})
	w.must(err)
	_, err = lossy.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion, Reason: "人工接管"})
	if err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("%v", err)
	}
	if q := w.getQuote("quote.2"); q.Control.State != trdapi.ControlStopping || q.Control.ControlEpoch != 2 {
		t.Fatalf("%+v", q.Control)
	}
	_, err = w.verifyGoal(w.sys, "task.2", r.RowVersion)
	w.want(err, tskapi.CodeControlEpochStale)
	if tv, err = w.c.TSK.GetTask(w.clerk, "task.2"); err != nil || tv.Lifecycle != tskapi.StateVerifying || tv.TaskVersion != r.RowVersion {
		t.Fatalf("%+v %v", tv, err)
	}
	if vs, err := w.c.TSK.GoalVerifications(w.clerk, "task.2"); err != nil || len(vs) != 0 {
		t.Fatalf("%+v %v", vs, err)
	}
	took := w.task(w.c.TSK.TakeControl(w.clerk, tskapi.TakeControl{IdempotencyKey: w.key(), TaskID: "task.2", ExpectedVersion: r.RowVersion,
		Reason: "人工接管"}))
	if took.Status != tskapi.StateStopping || took.ControlEpoch != 2 {
		t.Fatalf("%+v", took)
	}
	_, err = w.verifyGoal(w.sys, "task.2", took.RowVersion)
	w.want(err, kernel.CodeInvalidState)
}

// BC-AT25 (订单完成后报价自然到期：历史完成事实保留；不能继续前向动作), baseline 15.4 (completed 是核验时刻
// 目标已达成的历史事实) and TRD-INV-017: an order_active task completes on an order whose quote expires
// seconds later. After the expiry the task is still completed with its verification as it was, takes no
// forward command and issues no claim; TRD still judges order_active satisfied (the contract formed in
// time) while quote_ready has lapsed; one TaskCompleted.
func TestGoalVerifyCompletionOutlivesTheQuotesExpiry(t *testing.T) {
	w := newGoalWorld(t)
	w.compliance() // set up before the clock starts: order.activate's guards
	f, expires := w.shortlyActivated("quote.1", 8*time.Second)
	_, err := w.c.TRD.RecordResponse(w.author, w.full("response.1", "quote.1", 1, f.BusinessHash))
	w.must(err)
	acc, err := w.c.TRD.VerifyResponse(w.reviewer, w.verification("response.1", "acceptance.1", 1, f.BusinessHash))
	w.must(err)
	o, err := w.c.TRD.PrepareOrder(w.author, w.prepareCmd("order.1", acc))
	w.must(err)
	d := w.reviewed(o, "review.1")
	_, err = w.c.TRD.ActivateOrder(w.author, w.activateOrderCmd(o.ObjectID, d.RowVersion, "review.1"))
	w.must(err)
	r := w.manualVerifying("task.1", tskapi.GoalOrderActive, "quote.1")
	r = w.task(w.verifyGoal(w.sys, "task.1", r.RowVersion))
	before, _ := w.lastVerification("task.1")
	if r.Status != tskapi.StateCompleted || !before.CheckedAt.Before(expires) {
		t.Fatalf("not completed before the quote expired at %s (the host is too slow for this window): %+v %s", expires, r, before.CheckedAt)
	}
	time.Sleep(time.Until(expires) + 200*time.Millisecond)

	after, n := w.lastVerification("task.1")
	tv, err := w.c.TSK.GetTask(w.clerk, "task.1")
	w.must(err)
	if n != 1 || after.TaskVersion != before.TaskVersion || after.Outcome != tskapi.AnswerSatisfied || tv.Lifecycle != tskapi.StateCompleted ||
		tv.CompletionActor != tskapi.CompletionManual || tv.TaskVersion != r.RowVersion {
		t.Fatalf("%+v %+v", tv, after)
	}
	_, err = w.verifyGoal(w.sys, "task.1", r.RowVersion)
	w.want(err, kernel.CodeInvalidState)
	_, err = w.c.TSK.PauseTask(w.clerk, tskapi.PauseTask{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Cause: tskapi.PauseOperator, Reason: "报价到期", ResumeCondition: "无"})
	w.want(err, kernel.CodeInvalidState)
	_, err = w.c.TSK.CloseUnfulfilled(w.clerk, tskapi.CloseUnfulfilled{IdempotencyKey: w.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion,
		Cause: tskapi.CloseQuoteExpired, Reason: "报价到期"})
	w.want(err, kernel.CodeInvalidState)
	_, err = w.c.TSK.Claim(w.clerk, "task.1")
	w.want(err, kernel.CodeInvalidState)
	satisfiedGoal(t, w.goal(w.sys, trdapi.GoalOrderActive, "quote.1"), trdapi.GoalOrderActive, orderActiveAssertions)
	if a := w.goal(w.sys, trdapi.GoalQuoteReady, "quote.1"); checkOf(a, trdapi.AssertionQuote, trdapi.CheckValidity).Code != trdapi.CodeQuoteExpired {
		t.Fatalf("%v", assertionsOf(a))
	}
	if n := len(w.completedEvents()); n != 1 {
		t.Fatalf("%d TaskCompleted", n)
	}
}

// CT13 (必需检查 unknown／error：不得 completed；展示具体原因及恢复条件) and baseline 15.4 (资料不足和核验不可用
// 均不能当作 pass; Q01.scope 执行身份有权读取) against TRD itself: formal document bytes rewritten under the
// active order are an integrity error (Q01.document error FORMAL_DOCUMENT_INTEGRITY_FAILURE) — the task
// waits for an external answer with that reason, and completes once the bytes are restored; a revised
// SKU specification fails the required industry check (Q01.industry unsatisfied) — the task waits for
// input; a verifying system actor that may not read TRD finds Q01.scope unsatisfied (PERMISSION_DENIED,
// judged by TSK). The industry check's own unknown and error answers are run against a scripted TRD in
// the TSK package (TestGoalVerificationsAreRecorded) and the domain.
func TestGoalVerifyNeverPassesAnUndecidedRequiredCheck(t *testing.T) {
	w := newGoalWorld(t)
	w.activeOrder("quote.1")
	r := w.manualVerifying("task.doc", tskapi.GoalOrderActive, "quote.1")
	where := ` WHERE tenant_id = '` + w.tenant + `' AND quote_id = 'quote.1' AND revision = 1`
	off := "ALTER TABLE brant_trd.quote_document DISABLE TRIGGER quote_document_guard"
	on := "ALTER TABLE brant_trd.quote_document ENABLE TRIGGER quote_document_guard"
	w.must(oeqvOwner(w.trdWorld, off, `UPDATE brant_trd.quote_document SET content = content || '\x0a'::bytea, size_bytes = size_bytes + 1,
		content_sha256 = encode(sha256(content || '\x0a'::bytea), 'hex')`+where, on))
	r = w.task(w.verifyGoal(w.sys, "task.doc", r.RowVersion))
	v, _ := w.lastVerification("task.doc")
	tv, err := w.c.TSK.GetTask(w.clerk, "task.doc")
	w.must(err)
	doc := assertionOf(v, trdapi.AssertionDocument)
	if r.Status != tskapi.StateWaitingExternal || v.Outcome != tskapi.AnswerError || v.Code != tskapi.CodeRequiredCheckUnknown ||
		doc.Answer != tskapi.AnswerError || doc.Code != trdapi.CodeFormalDocumentIntegrityFailure ||
		!slices.Equal(tv.Wait.Items, []string{trdapi.AssertionDocument}) ||
		!strings.Contains(tv.Wait.Reason, "Q01.document error "+trdapi.CodeFormalDocumentIntegrityFailure) {
		t.Fatalf("%+v %v %+v", r, assertionsOfVerification(v), tv.Wait)
	}
	w.must(oeqvOwner(w.trdWorld, off, `UPDATE brant_trd.quote_document SET content = substring(content from 1 for size_bytes - 1),
		size_bytes = size_bytes - 1, content_sha256 = encode(sha256(substring(content from 1 for size_bytes - 1)), 'hex')`+where, on))
	r = w.reverify("task.doc", r.RowVersion, tskapi.ObjectRef{Kind: "formal_document", ID: "quote.1", Version: 1})
	if r = w.task(w.verifyGoal(w.sys, "task.doc", r.RowVersion)); r.Status != tskapi.StateCompleted {
		t.Fatalf("restored: %+v", r)
	}

	s := w.manualVerifying("task.sku", tskapi.GoalOrderActive, "quote.1")
	sku, err := w.c.CAT.GetSku(w.author, "sku.1")
	w.must(err)
	_, err = w.c.CAT.ReviseSpec(w.author, catapi.ReviseSpec{IdempotencyKey: w.key(), SkuID: "sku.1", ExpectedVersion: sku.RowVersion,
		Reason: "合成技术变更"})
	w.must(err)
	s = w.task(w.verifyGoal(w.sys, "task.sku", s.RowVersion))
	v, _ = w.lastVerification("task.sku")
	if s.Status != tskapi.StateWaitingInput || assertionOf(v, trdapi.AssertionIndustry).Answer != tskapi.AnswerUnsatisfied ||
		!slices.Contains(v.Unmet, trdapi.AssertionIndustry) {
		t.Fatalf("%+v %v", s, assertionsOfVerification(v))
	}

	blind := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "tsk.blind"},
		Permissions: tskPermissions, DataScope: "company"})
	b := w.manualVerifying("task.blind", tskapi.GoalQuoteReady, "quote.1")
	b = w.task(w.verifyGoal(blind, "task.blind", b.RowVersion))
	v, _ = w.lastVerification("task.blind")
	if b.Status != tskapi.StateWaitingInput || v.Assessor != tskapi.AssessorTSK || !slices.Equal(v.Unmet, []string{trdapi.AssertionScope}) ||
		v.Assertions[0].Code != kernel.CodePermissionDenied || v.Actor.ID != "tsk.blind" {
		t.Fatalf("%+v %+v", b, v)
	}
}
