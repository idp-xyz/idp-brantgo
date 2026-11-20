//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TSK's ActionExecution against the composed contexts on PostgreSQL 16 (WBS S4-02,
// TSK-AGG-ActionExecution, TSK-INV-003, X-INV-001, X-INV-003; Context Map R18 TRD, R66 EVD, R67 INQ,
// R68 SUP, R69 CST — ActionReconciliation v1; owner decision D-11). The test plays the executor of
// task steps (internal/ai/execution, S4 unit C2): it takes the task's claim, asks the target context
// for the request's ledger identity (RequestOf), records the attempt in TSK before it sends the
// command with the key TSK gives, sends it — with the claim on a bound root — and has TSK reconcile
// the action with the target's ledger.

var actionPermissions = []string{"action.record_attempt", "action.reconcile"}

// stepTarget is one command a task step sends: its context, its ledger identity and how to send it
// with a key.
type stepTarget struct {
	context string
	request func() (kernel.ActionRequest, error)
	send    func(ctx context.Context, key string) (actionID, objectID string, err error)
}

// step runs one task step as executor ctx at claim epoch epoch and reconciles it: TSK records the
// attempt before the command is sent, the command carries the action's key, and the reconciliation
// records the target's stored receipt, which is the one the send answered.
func step(t *testing.T, tsk taskSteps, ctx context.Context, task, intent string, epoch int64, s stepTarget) tskapi.ActionReceipt {
	t.Helper()
	req, err := s.request()
	if err != nil {
		t.Fatalf("%s: RequestOf: %v", intent, err)
	}
	rec, err := tsk.RecordAttempt(ctx, tskapi.RecordAttempt{IdempotencyKey: "k.rec." + intent, TaskID: task, BusinessIntentID: intent,
		Target: tskapi.TargetCommand{Context: s.context, Command: req.Command, RequestHash: req.RequestHash}, ControlEpoch: epoch,
		Outcome: tskapi.ActionSubmitted})
	if err != nil || rec.State != tskapi.ActionSubmitted || rec.IdempotencyKey != tskapi.ActionIDOf(intent) {
		t.Fatalf("%s: record %+v %v", intent, rec, err)
	}
	actionID, objectID, err := s.send(ctx, rec.IdempotencyKey)
	if err != nil {
		t.Fatalf("%s: send %v", intent, err)
	}
	done, err := tsk.ReconcileAction(ctx, tskapi.ReconcileAction{IdempotencyKey: "k.rcn." + intent, TaskID: task, ActionID: rec.ObjectID})
	if err != nil || done.State != tskapi.ActionCommitted || done.Result == nil || done.Result.ActionID != actionID ||
		done.Result.ObjectID != objectID {
		t.Fatalf("%s: reconcile %+v %v (the target answered %s %s)", intent, done, err, actionID, objectID)
	}
	return done
}

type taskSteps interface {
	tskapi.ActionCommands
}

// Every context a Q01 task step writes reconciles through its ActionReconciliation v1 (R18, R66–R69):
// the task's AI agent drafts a source at EVD, an RFQ at INQ, a supplier offer at SUP, a costing
// context at CST and a quote root at TRD, each recorded before it is sent with TSK's key and
// reconciled as committed with the target's own receipt; while one is open the task does not verify,
// once all are committed it does, and TSK binds the root its action created. A step whose actor
// lacks the target's read permission is refused before anything is recorded or sent (X-INV-003:
// the target's PERMISSION_DENIED); a command the target does not have is SCHEMA_INVALID at
// /args/target/command.
func TestTaskStepsReconcileOnEveryTarget(t *testing.T) {
	q := newTrdWorld(t)
	every := append(append(slices.Clone(allPermissions), tskPermissions...), actionPermissions...)
	sys := q.provision(grantOf(q, "tsk", kernel.ActorSystem, "company", every...))
	clerk := q.provision(grantOf(q, "clerk.1", kernel.ActorHuman, "company", every...))
	agent := q.provision(grantOf(q, "agent.1", kernel.ActorAI, "company", append(slices.Clone(allPermissions), "tsk.read", "action.record_attempt",
		"action.reconcile")...))
	blind := q.provision(grantOf(q, "clerk.blind", kernel.ActorHuman, "company", without(every, "inq.read")...))
	tsk := q.c.TSK
	r, err := tsk.CreateTask(clerk, tskapi.CreateTask{IdempotencyKey: q.key(), TaskID: "task.1", GoalCode: tskapi.GoalQuoteReady,
		ControlMode: tskapi.ModeAI, AgentID: "agent.1", Target: tskapi.Target{RfqID: "rfq.1", CustomerID: "customer.1"}})
	q.must(err)
	r, err = tsk.ScopeTask(sys, tskapi.ScopeTask{IdempotencyKey: q.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion})
	q.must(err)
	r, err = tsk.ScopeTask(clerk, tskapi.ScopeTask{IdempotencyKey: q.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Note: "范围确认"})
	q.must(err)
	claim, err := tsk.Claim(agent, "task.1")
	q.must(err)

	source := evdapi.RegisterSource{SourceID: "src.task", SourceKind: evdapi.KindManualText, ContentText: "合成询价来源：任务步骤"}
	rfq := inqapi.SaveRfq{RfqID: "rfq.task", Lines: []inqapi.RfqLine{{LineID: "L1", RequestedItem: "待核实商品"}}}
	offer := supapi.SaveSupplierOffer{OfferID: "offer.task", Data: q.offer()}
	costing := cstapi.SaveContext{ContextID: "context.task", Data: q.contextData()}
	quote := trdapi.SaveDraft{QuoteID: "quote.task", Data: q.data()}
	targets := map[string]stepTarget{
		"intent.evd": {tskapi.TargetEVD, func() (kernel.ActionRequest, error) { return q.c.EVD.RequestOf(source) },
			func(ctx context.Context, key string) (string, string, error) {
				c := source
				c.IdempotencyKey = key
				res, err := q.c.EVD.RegisterSource(ctx, c)
				return res.ActionID, res.ObjectID, err
			}},
		"intent.inq": {tskapi.TargetINQ, func() (kernel.ActionRequest, error) { return q.c.INQ.RequestOf(rfq) },
			func(ctx context.Context, key string) (string, string, error) {
				c := rfq
				c.IdempotencyKey = key
				res, err := q.c.INQ.SaveRfq(ctx, c)
				return res.ActionID, res.ObjectID, err
			}},
		"intent.sup": {tskapi.TargetSUP, func() (kernel.ActionRequest, error) { return q.c.SUP.RequestOf(offer) },
			func(ctx context.Context, key string) (string, string, error) {
				c := offer
				c.IdempotencyKey = key
				res, err := q.c.SUP.SaveSupplierOffer(ctx, c)
				return res.ActionID, res.ObjectID, err
			}},
		"intent.cst": {tskapi.TargetCST, func() (kernel.ActionRequest, error) { return q.c.CST.RequestOf(costing) },
			func(ctx context.Context, key string) (string, string, error) {
				c := costing
				c.IdempotencyKey = key
				res, err := q.c.CST.SaveContext(ctx, c)
				return res.ActionID, res.ObjectID, err
			}},
		"intent.trd": {tskapi.TargetTRD, func() (kernel.ActionRequest, error) { return q.c.TRD.RequestOf(quote) },
			func(ctx context.Context, key string) (string, string, error) {
				c := quote
				c.IdempotencyKey = key
				res, err := q.c.TRD.SaveDraft(ctx, c)
				return res.ActionID, res.ObjectID, err
			}},
	}

	// X-INV-003: the target re-checks the executor's read permission before anything is recorded.
	inq := targets["intent.inq"]
	req, err := inq.request()
	q.must(err)
	_, err = tsk.RecordAttempt(blind, tskapi.RecordAttempt{IdempotencyKey: q.key(), TaskID: "task.1", BusinessIntentID: "intent.blind",
		Target: tskapi.TargetCommand{Context: tskapi.TargetINQ, Command: req.Command, RequestHash: req.RequestHash}, Outcome: tskapi.ActionSubmitted})
	q.want(err, kernel.CodePermissionDenied)
	_, err = tsk.RecordAttempt(agent, tskapi.RecordAttempt{IdempotencyKey: q.key(), TaskID: "task.1", BusinessIntentID: "intent.wrong",
		Target: tskapi.TargetCommand{Context: tskapi.TargetEVD, Command: req.Command, RequestHash: req.RequestHash}, Outcome: tskapi.ActionSubmitted})
	if q.want(err, kernel.CodeSchemaInvalid); path(err) != "/args/target/command" {
		t.Fatalf("an INQ command at EVD: %s", path(err))
	}

	// An open attempt holds the task back from verification.
	open, err := tsk.RecordAttempt(agent, tskapi.RecordAttempt{IdempotencyKey: "k.rec.intent.inq", TaskID: "task.1", BusinessIntentID: "intent.inq",
		Target: tskapi.TargetCommand{Context: tskapi.TargetINQ, Command: req.Command, RequestHash: req.RequestHash}, Outcome: tskapi.ActionSubmitted})
	q.must(err)
	_, err = tsk.VerifyTask(sys, tskapi.VerifyTask{IdempotencyKey: q.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion})
	q.want(err, tskapi.CodeExternalResultUnknown)
	if open.IdempotencyKey != tskapi.ActionIDOf("intent.inq") {
		t.Fatalf("%+v", open)
	}
	receipts := map[string]tskapi.ActionReceipt{}
	for _, intent := range []string{"intent.evd", "intent.inq", "intent.sup", "intent.cst", "intent.trd"} {
		receipts[intent] = step(t, tsk, agent, "task.1", intent, claim.ControlEpoch, targets[intent])
	}
	list, err := tsk.TaskActions(clerk, "task.1")
	q.must(err)
	var got []string
	for _, a := range list {
		got = append(got, a.Target.Context+":"+a.Target.Command+":"+a.State)
		if a.RegisteredBy.ID != "agent.1" || a.Attempts != 1 || *a.Result != *receipts[a.BusinessIntentID].Result {
			t.Fatalf("%+v", a)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"cst:costing_context.save_draft:committed", "evd:source.register:committed", "inq:rfq.save_draft:committed",
		"sup:supplier_offer.save_draft:committed", "trd:quote.save_draft:committed"}) {
		t.Fatalf("%v", got)
	}
	// The target's ledger and TSK agree: the key TSK gave is the one the target stored, for the
	// request TSK recorded.
	for _, a := range list {
		if a.Target.Context == tskapi.TargetTRD {
			if h := ledgerHash(t, q.world, "brant_trd", a.Target.Command, a.IdempotencyKey); h != a.Target.RequestHash {
				t.Fatalf("TRD stored %s for %+v", h, a.Target)
			}
		}
	}
	r, err = tsk.VerifyTask(sys, tskapi.VerifyTask{IdempotencyKey: q.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion})
	if err != nil || r.Status != tskapi.StateVerifying {
		t.Fatalf("%+v %v", r, err)
	}
	reconciled := 0
	for _, e := range q.events("company.A", tskapi.Source) {
		if e.Type == tskapi.EventActionReconciled {
			reconciled++
		}
	}
	if reconciled != 5 {
		t.Fatalf("%d ActionReconciled", reconciled)
	}
}

// workerEnv names the environment variable that turns a run of
// TestTaskStepCommittedBeforeTheWorkerExitIsReconciledOnce into the worker process.
const workerEnv = "BRANT_TSK_STEP_WORKER"

// workerStep is what the worker process executes: an order activation as a task step.
type workerStep struct {
	Tenant, Company, Actor, Task, Intent, Order, Review string
	Expected                                            int64
}

// runWorker is the worker process: it opens its own platform, takes the task's claim, records the
// attempt, sends the activation with the claim and exits with 73 right after the commit — before it
// can record anything else (Python's child calls os._exit(73)).
func runWorker(t *testing.T, payload string) {
	var s workerStep
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		t.Fatal(err)
	}
	fail := func(what string, err error) {
		fmt.Fprintf(os.Stderr, "worker: %s: %v\n", what, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	platform, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: pgtest.AppDSN(t)})
	if err != nil {
		fail("open", err)
	}
	c, err := platform.Contexts()
	if err != nil {
		fail("contexts", err)
	}
	as, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: s.Actor},
		Scope: kernel.Scope{Tenant: kernel.TenantID(s.Tenant), Company: kernel.CompanyID(s.Company)}})
	if err != nil {
		fail("principal", err)
	}
	as = action.WithCorrelation(as, "corr.worker")
	claim, err := c.TSK.Claim(as, s.Task)
	if err != nil {
		fail("claim", err)
	}
	activate := trdapi.ActivateOrder{OrderID: s.Order, ExpectedVersion: s.Expected, ReviewID: s.Review}
	req, err := c.TRD.RequestOf(activate)
	if err != nil {
		fail("request", err)
	}
	rec, err := c.TSK.RecordAttempt(as, tskapi.RecordAttempt{IdempotencyKey: "k.worker.record", TaskID: s.Task, BusinessIntentID: s.Intent,
		Target: tskapi.TargetCommand{Context: tskapi.TargetTRD, Command: req.Command, RequestHash: req.RequestHash}, ControlEpoch: claim.ControlEpoch,
		Outcome: tskapi.ActionSubmitted})
	if err != nil {
		fail("record", err)
	}
	claimed, err := trdapi.WithControl(as, trdapi.ControlClaim{TaskID: claim.TaskID, ControlEpoch: claim.ControlEpoch})
	if err != nil {
		fail("claim context", err)
	}
	activate.IdempotencyKey = rec.IdempotencyKey
	if _, err = c.TRD.ActivateOrder(claimed, activate); err != nil {
		fail("activate", err)
	}
	os.Exit(73) // after the commit, before the executor records or acknowledges anything
}

// orderTask provisions TSK's system actor, a sales clerk who executes task steps and the task's AI
// agent, and binds quote.1 to task.1 (order_active) through TSK.
func orderTask(q *trdWorld) (sys, clerk context.Context, bound tskapi.TaskReceipt) {
	q.t.Helper()
	every := append(append(slices.Clone(allPermissions), tskPermissions...), actionPermissions...)
	sys = q.provision(grantOf(q, "tsk", kernel.ActorSystem, "company", every...))
	clerk = q.provision(grantOf(q, "clerk.1", kernel.ActorHuman, "company", every...))
	q.provision(grantOf(q, "agent.1", kernel.ActorAI, "company", "tsk.read", "trd.read"))
	tsk := q.c.TSK
	r, err := tsk.CreateTask(clerk, tskapi.CreateTask{IdempotencyKey: q.key(), TaskID: "task.1", GoalCode: tskapi.GoalOrderActive,
		ControlMode: tskapi.ModeAI, AgentID: "agent.1", Target: tskapi.Target{QuoteID: "quote.1", CustomerID: "customer.1"}})
	q.must(err)
	r, err = tsk.ScopeTask(sys, tskapi.ScopeTask{IdempotencyKey: q.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion})
	q.must(err)
	r, err = tsk.ScopeTask(clerk, tskapi.ScopeTask{IdempotencyKey: q.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, Note: "范围确认"})
	q.must(err)
	bound, err = tsk.BindObject(sys, tskapi.BindObject{IdempotencyKey: q.key(), TaskID: "task.1", ExpectedVersion: r.RowVersion, QuoteID: "quote.1"})
	q.must(err)
	return sys, clerk, bound
}

// BC-AT18 (TSK half), AN05, TRD-INV-016, TSK-INV-003 and Q01 V0.1
// tests/test_execution.py::test_commit_then_real_worker_exit_is_recovered_without_duplicate: a
// worker process records the order activation as a step of the task bound to the quote, sends it
// with the task's claim, and exits right after TRD committed it — before it could record anything.
// The action is left submitted: the task does not verify. A fresh platform (this process, a new
// connection pool) reconciles the action by its key and hash: committed, with TRD's original action
// and the active order. A new plan recording the same intent is answered with that result and sends
// nothing; TRD replays the same key; one active order, one commit evidence, one OrderActivated; the
// task verifies.
func TestTaskStepCommittedBeforeTheWorkerExitIsReconciledOnce(t *testing.T) {
	if payload := os.Getenv(workerEnv); payload != "" {
		runWorker(t, payload)
		return
	}
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	d := q.reviewed(o, "review.1")
	sys, clerk, bound := orderTask(q)
	intent := "task.1.order.activate." + o.ObjectID
	payload, err := json.Marshal(workerStep{Tenant: q.tenant, Company: "company.A", Actor: "clerk.1", Task: "task.1", Intent: intent,
		Order: o.ObjectID, Review: "review.1", Expected: d.RowVersion})
	q.must(err)
	worker := exec.Command(os.Args[0], "-test.run=^TestTaskStepCommittedBeforeTheWorkerExitIsReconciledOnce$", "-test.count=1",
		"-test.timeout=120s")
	worker.Env = append(os.Environ(), workerEnv+"="+string(payload))
	out, err := worker.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 73 {
		t.Fatalf("the worker: %v\n%s", err, out)
	}

	key := tskapi.ActionIDOf(intent)
	before, err := q.c.TSK.GetAction(clerk, key)
	q.must(err)
	if before.State != tskapi.ActionSubmitted || before.Attempts != 1 || before.RegisteredBy.ID != "clerk.1" || len(before.Log) != 1 ||
		before.Log[0].ControlEpoch != bound.ControlEpoch {
		t.Fatalf("the worker's record %+v", before)
	}
	if q.getOrder(o.ObjectID).Status != trdapi.OrderActive || len(q.eventsOf(trdapi.EventOrderActivated)) != 1 {
		t.Fatal("the worker's activation did not commit")
	}
	_, err = q.c.TSK.VerifyTask(sys, tskapi.VerifyTask{IdempotencyKey: q.key(), TaskID: "task.1", ExpectedVersion: bound.RowVersion})
	q.want(err, tskapi.CodeExternalResultUnknown)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fresh, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: pgtest.AppDSN(t)})
	q.must(err)
	defer fresh.Close()
	c, err := fresh.Contexts()
	q.must(err)
	rec, err := c.TSK.ReconcileAction(clerk, tskapi.ReconcileAction{IdempotencyKey: q.key(), TaskID: "task.1", ActionID: key})
	q.must(err)
	trdRec, err := c.TRD.ReconcileHash(clerk, trdapi.CommandActivateOrder, key, before.Target.RequestHash)
	q.must(err)
	if rec.State != tskapi.ActionCommitted || rec.Result == nil || rec.Result.ActionID != trdRec.ActionID || rec.Result.ObjectID != o.ObjectID ||
		rec.Result.Status != trdapi.OrderActive || rec.Attempt != 1 {
		t.Fatalf("the reconciliation %+v, TRD %+v", rec, trdRec)
	}
	// CT11: a new plan revision redoing the step is answered with the original result.
	redo, err := c.TSK.RecordAttempt(clerk, tskapi.RecordAttempt{IdempotencyKey: q.key(), TaskID: "task.1", BusinessIntentID: intent,
		Target: before.Target, ControlEpoch: bound.ControlEpoch, Outcome: tskapi.ActionSubmitted})
	if err != nil || redo.State != tskapi.ActionCommitted || *redo.Result != *rec.Result {
		t.Fatalf("the new plan's record %+v %v", redo, err)
	}
	// The same key at TRD replays (Python's reopened.execute of the same request).
	claimed, err := trdapi.WithControl(clerk, trdapi.ControlClaim{TaskID: "task.1", ControlEpoch: bound.ControlEpoch})
	q.must(err)
	again, err := c.TRD.ActivateOrder(claimed, trdapi.ActivateOrder{IdempotencyKey: key, OrderID: o.ObjectID, ExpectedVersion: d.RowVersion,
		ReviewID: "review.1"})
	if err != nil || !again.Replayed || again.ActionID != rec.Result.ActionID || again.Status != trdapi.OrderActive {
		t.Fatalf("the replay %+v %v", again, err)
	}
	if _, _, commits := q.orderRows(); commits != 1 || len(q.eventsOf(trdapi.EventOrderActivated)) != 1 {
		t.Fatalf("%d commit evidence, %d OrderActivated", commits, len(q.eventsOf(trdapi.EventOrderActivated)))
	}
	after, err := c.TSK.GetAction(clerk, key)
	q.must(err)
	if after.Attempts != 1 || after.Version != 2 || !slices.Equal([]string{after.Log[0].State, after.Log[1].State}, []string{"submitted", "committed"}) {
		t.Fatalf("%+v", after)
	}
	v, err := c.TSK.VerifyTask(sys, tskapi.VerifyTask{IdempotencyKey: q.key(), TaskID: "task.1", ExpectedVersion: bound.RowVersion})
	if err != nil || v.Status != tskapi.StateVerifying {
		t.Fatalf("%+v %v", v, err)
	}
}

// BC-AT19 (TSK half), TRD-INV-016 and TSK-INV-003: the order activation fails inside TRD's
// transaction before it commits (an injected storage failure in the commit evidence); the executor
// has no answer and records the attempt unknown — TRD holds nothing under the key —; the
// reconciliation finds it not committed; the next attempt sends the same key, which executes once:
// one active order, one commit evidence, one OrderActivated; the action's log shows both attempts.
func TestTaskStepFailedBeforeCommitIsSentAgainOnce(t *testing.T) {
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	d := q.reviewed(o, "review.1")
	sys, clerk, bound := orderTask(q)
	tsk := q.c.TSK
	intent := "task.1.order.activate." + o.ObjectID
	activate := trdapi.ActivateOrder{OrderID: o.ObjectID, ExpectedVersion: d.RowVersion, ReviewID: "review.1"}
	req, err := q.c.TRD.RequestOf(activate)
	q.must(err)
	target := tskapi.TargetCommand{Context: tskapi.TargetTRD, Command: req.Command, RequestHash: req.RequestHash}
	claimed, err := trdapi.WithControl(clerk, trdapi.ControlClaim{TaskID: "task.1", ControlEpoch: bound.ControlEpoch})
	q.must(err)
	record := func(outcome string) tskapi.ActionReceipt {
		t.Helper()
		r, err := tsk.RecordAttempt(clerk, tskapi.RecordAttempt{IdempotencyKey: q.key(), TaskID: "task.1", BusinessIntentID: intent, Target: target,
			ControlEpoch: bound.ControlEpoch, Outcome: outcome})
		q.must(err)
		return r
	}
	first := record(tskapi.ActionSubmitted)
	activate.IdempotencyKey = first.IdempotencyKey
	drop := q.failInsert("brant_trd.sales_order_commit_evidence")
	_, err = q.c.TRD.ActivateOrder(claimed, activate)
	drop()
	if _, rejected := kernel.AsRejection(err); err == nil || rejected || !strings.Contains(err.Error(), "test storage failure") {
		t.Fatalf("the failing activation: %v", err)
	}
	if lost := record(tskapi.ActionUnknown); lost.State != tskapi.ActionUnknown || lost.Attempt != 1 {
		t.Fatalf("%+v", lost)
	}
	_, err = tsk.VerifyTask(sys, tskapi.VerifyTask{IdempotencyKey: q.key(), TaskID: "task.1", ExpectedVersion: bound.RowVersion})
	q.want(err, tskapi.CodeExternalResultUnknown)
	rec, err := tsk.ReconcileAction(clerk, tskapi.ReconcileAction{IdempotencyKey: q.key(), TaskID: "task.1", ActionID: first.ObjectID})
	if err != nil || rec.State != tskapi.ActionNotCommitted {
		t.Fatalf("%+v %v", rec, err)
	}
	if _, _, commits := q.orderRows(); commits != 0 || q.getOrder(o.ObjectID).Status != trdapi.OrderDraft ||
		len(q.eventsOf(trdapi.EventOrderActivated)) != 0 {
		t.Fatal("the failed activation left residue")
	}
	second := record(tskapi.ActionSubmitted)
	if second.Attempt != 2 || second.IdempotencyKey != first.IdempotencyKey {
		t.Fatalf("%+v", second)
	}
	act, err := q.c.TRD.ActivateOrder(claimed, activate)
	if err != nil || act.Replayed || act.Status != trdapi.OrderActive {
		t.Fatalf("the second attempt %+v %v", act, err)
	}
	done, err := tsk.ReconcileAction(clerk, tskapi.ReconcileAction{IdempotencyKey: q.key(), TaskID: "task.1", ActionID: first.ObjectID})
	if err != nil || done.State != tskapi.ActionCommitted || done.Result.ActionID != act.ActionID || done.Attempt != 2 {
		t.Fatalf("%+v %v", done, err)
	}
	if _, _, commits := q.orderRows(); commits != 1 || len(q.eventsOf(trdapi.EventOrderActivated)) != 1 {
		t.Fatalf("%d commit evidence, %d OrderActivated", commits, len(q.eventsOf(trdapi.EventOrderActivated)))
	}
	v, err := tsk.GetAction(clerk, first.ObjectID)
	q.must(err)
	var log []string
	for _, l := range v.Log {
		log = append(log, l.State)
	}
	if !slices.Equal(log, []string{"submitted", "unknown", "not_committed", "submitted", "committed"}) {
		t.Fatalf("%v", log)
	}
}
