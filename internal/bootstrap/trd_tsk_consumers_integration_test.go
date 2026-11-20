//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TRD's and TSK's Inbox consumers on PostgreSQL 16 (WBS S4-06b, unit B3), through the assembled relay
// with the in-process dispatcher as its publisher (Platform.Relay, as brant-worker -dispatch runs
// it). Each test relays a Bento outbox of its own (newOrderEvents), into which it copies the events
// the producing context's command ledger wrote, unchanged.
//
// trd.cost_source_holds (Context Map R11; tactical model WF-03, event storming PL-07, Context Map
// SC-04 activation first; S4 decision D-9; S3-D67): CST's CostSourceDenied holds the active revisions
// whose commit evidence names a denied snapshot while no contract formed on them, and records a risk
// that blocks nothing on the active orders. tsk.task_progress (R18 TRD integration events v1; PL-04,
// PL-06; SM-TSK-TASK task.resume and task.verify): TRD's facts resume the waiting tasks they concern
// and start the verification of the tasks whose goal they make verifiable.

// costHoldActor and progressActor are the consumers' system actors.
var (
	costHoldActor = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.trd.cost_source_holds"}
	progressActor = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.tsk.task_progress"}
)

// outboxOf returns the events of source in scope that the platform's Outbox holds, oldest first,
// of the types given (all when none), unchanged.
func outboxOf(t *testing.T, scope kernel.Scope, source string, types ...string) []eventing.Envelope {
	t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	if err != nil {
		t.Fatal(err)
	}
	q, err := db.ReadExecutor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := q.Query(context.Background(), `SELECT event_id, event_type, event_version, spec_version, subject, partition_key, occurred_at,
		recorded_at, coalesce(correlation_id, ''), coalesce(causation_id, ''), coalesce(traceparent, ''), coalesce(tracestate, ''), content_type, payload
		FROM bento.outbox WHERE source = $1 AND scope = $2 AND (cardinality($3::text[]) = 0 OR event_type = ANY($3)) ORDER BY enqueued_seq`,
		source, scope.String(), types)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (eventing.Envelope, error) {
		env := eventing.Envelope{Source: source, Scope: scope.String()}
		var version int64
		var payload []byte
		err := row.Scan(&env.ID, &env.Type, &version, &env.SpecVersion, &env.Subject, &env.PartitionKey, &env.OccurredAt, &env.RecordedAt,
			&env.CorrelationID, &env.CausationID, &env.TraceParent, &env.TraceState, &env.ContentType, &payload)
		env.Version, env.Payload = eventing.EventVersion(version), json.RawMessage(payload)
		return env, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// holdsOf are the risk holds of quote id as "cause:code@revision".
func (q *trdWorld) holdsOf(id string) []string {
	out := []string{}
	for _, h := range q.getQuote(id).Holds {
		out = append(out, h.Cause+":"+h.Code+"@"+itoa(h.BusinessRevision))
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// auditOf are the succeeded entries of command in TRD's audit trail as "object actor".
func (q *trdWorld) auditOf(command string) []string {
	q.t.Helper()
	entries, err := q.c.TRD.Audit(q.author)
	q.must(err)
	out := []string{}
	for _, e := range entries {
		if e.Command == command && e.Outcome == "succeeded" {
			out = append(out, e.ObjectID+" "+e.Actor.ID)
		}
	}
	return out
}

// WF-03 / SC-04 activation first, S4 decision D-9, S3-D67's hold without release: CST denies the
// supplier offer source every quote of the world rests on (one cost snapshot). Until the operator
// provisions system:inbox.trd.cost_source_holds its commands are denied and the event is delivered
// again, nothing held. Then, once:
//   - quote.1, active with no contract: held, cause cost_source_denied, code COST_SOURCE_DENIED, by
//     the consumer's system actor, naming the attestation, the source, the snapshot and the event; the
//     root version moves by one, the revision stays active; QuoteReady, QuoteRevisionGuard v1 and
//     quote_ready refuse it, and so does recording the customer's response to it (forward use paused);
//   - quote.2, accepted (the contract formed): not held — its order's approval will acknowledge the
//     denial (TRD-INV-014); quote.5's draft order shows it as an upstream risk to acknowledge;
//   - quote.3, its order active: the revision is not held, the order gets a risk record that changes
//     neither its status nor its row version nor OrderGuard v1 (ADR-BR-013);
//   - quote.4, approved and pending: SC-04 denial first — its activation is refused by CST's
//     AdoptabilityGuard (COST_SNAPSHOT_INELIGIBLE), nothing to hold.
//
// A redelivery, a second dispatcher and the same denial under another event ID change nothing; the
// database refuses a cost hold on a revision a contract formed on and a risk record on a draft order.
func TestCostSourceDenialHoldsUncontractedRevisionsOnce(t *testing.T) {
	q := newOrderWorld(t)
	ctx := context.Background()
	scope := q.scope("company.A")
	open := q.activated("quote.1")
	_, accepted := q.accepted("quote.2")
	active := q.activeOrder("quote.3")
	_, pending := q.approved("quote.4")
	_, draft := q.prepared("quote.5")
	root1, root2, order3 := q.getQuote("quote.1").RootVersion, q.getQuote("quote.2").RootVersion, q.getOrder(active.ObjectID)

	q.deny()
	denials := outboxOf(t, scope, cstapi.Source, cstapi.EventCostSourceDenied)
	if len(denials) != 1 {
		t.Fatalf("%d denials in CST's Outbox", len(denials))
	}
	denial := denials[0]
	var payload struct {
		AttestationID string   `json:"attestation_id"`
		SnapshotIDs   []string `json:"snapshot_ids"`
	}
	q.must(json.Unmarshal(denial.Payload, &payload))
	if !slices.Equal(payload.SnapshotIDs, []string{q.snap.SnapshotID}) {
		t.Fatalf("the denial names %v, the world's snapshot is %s", payload.SnapshotIDs, q.snap.SnapshotID)
	}

	subs := q.c.TRD.Subscriptions()
	ev := newOrderEvents(t, subs...)
	ev.enqueue(denial)
	ev.once(ev.relay(5))
	if s := ev.status(denial.ID); s == "PUBLISHED" || len(q.holdsOf("quote.1")) != 0 || q.count("company.A", "brant_trd.sales_order_risk") != 0 {
		t.Fatalf("before provisioning: outbox %s, holds %v; want the consumer denied and the delivery retried", s, q.holdsOf("quote.1"))
	}
	if _, err := q.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	ev.drain(ev.relay(5))
	ev.published(denial)

	v1 := q.getQuote("quote.1")
	if len(v1.Holds) != 1 || v1.RootVersion != root1+1 || !slices.Equal(q.statuses("quote.1"), []string{trdapi.StatusActive}) {
		t.Fatalf("quote.1: holds %+v, root version %d (was %d), %v", v1.Holds, v1.RootVersion, root1, q.statuses("quote.1"))
	}
	h := v1.Holds[0]
	if h.HoldID != "cost_source_denied:"+payload.AttestationID || h.Cause != trdapi.HoldCostSourceDenied || h.Code != trdapi.CodeCostSourceDenied ||
		h.BusinessRevision != 1 || h.ApprovalID != "" || h.CostSnapshotID != q.snap.SnapshotID || h.CostSourceID != "src.1" ||
		h.AttestationID != payload.AttestationID || h.EventID != string(denial.ID) || h.RegisteredBy != costHoldActor || h.RootVersion != root1+1 {
		t.Fatalf("quote.1's hold %+v", h)
	}
	if r := q.readiness(q.author, "quote.1", 1); r.Ready || !slices.Contains(reasonsOf(r.Reasons), trdapi.CheckRiskHold+":"+trdapi.CodeCostSourceDenied) {
		t.Fatalf("quote.1's readiness %v %v", r.Ready, reasonsOf(r.Reasons))
	}
	if code := q.scalar(`SELECT code FROM brant_trd.guard_quote_revision_active_v1('quote.1', 1, '` + open.BusinessHash + `')`); code != trdapi.CodeCostSourceDenied {
		t.Fatalf("QuoteRevisionGuard v1 on quote.1: %s", code)
	}
	if g := q.goal(q.author, trdapi.GoalQuoteReady, "quote.1"); g.Outcome == trdapi.AnswerSatisfied {
		t.Fatalf("quote_ready on a held revision: %v", assertionsOf(g))
	}
	_, err := q.c.TRD.RecordResponse(q.author, q.full("response.quote.1", "quote.1", 1, open.BusinessHash))
	q.want(err, trdapi.CodeCostSourceDenied)

	if v := q.getQuote("quote.2"); len(v.Holds) != 0 || v.RootVersion != root2 {
		t.Fatalf("quote.2 (accepted %s): holds %+v, root version %d (was %d)", accepted.ObjectID, v.Holds, v.RootVersion, root2)
	}
	if risks := q.risks(draft.ObjectID); !slices.ContainsFunc(risks, func(r trdapi.Risk) bool { return r.Code == cstapi.CodeCostSnapshotIneligible }) ||
		len(q.holdsOf("quote.5")) != 0 {
		t.Fatalf("quote.5's draft order: risks %+v, holds %v", risks, q.holdsOf("quote.5"))
	}

	o := q.getOrder(active.ObjectID)
	if len(q.holdsOf("quote.3")) != 0 || o.Status != trdapi.OrderActive || o.RowVersion != order3.RowVersion || len(o.RiskRecords) != 1 {
		t.Fatalf("quote.3: holds %v, order %s v%d (was v%d), risks %+v", q.holdsOf("quote.3"), o.Status, o.RowVersion, order3.RowVersion,
			o.RiskRecords)
	}
	rr := o.RiskRecords[0]
	if rr.RiskID != "cost_source_denied:"+payload.AttestationID || rr.Cause != "cost_source_denied" || rr.Code != trdapi.CodeCostSourceDenied ||
		rr.OrderRevision != o.OrderRevision || rr.OrderRowVersion != o.RowVersion || rr.CostSnapshotID != q.snap.SnapshotID ||
		rr.CostSourceID != "src.1" || rr.AttestationID != payload.AttestationID || rr.EventID != string(denial.ID) || rr.RegisteredBy != costHoldActor {
		t.Fatalf("order risk record %+v", rr)
	}
	if guard := q.scalar(`SELECT coalesce(code, 'none') FROM brant_trd.guard_order_active_v1('` + active.ObjectID + `', ` + itoa(o.OrderRevision) +
		`, '` + o.OrderHash + `')`); guard != "none" {
		t.Fatalf("OrderGuard v1 after the risk record: %s", guard)
	}

	_, err = q.c.TRD.Activate(q.author, q.activateCmd("quote.4", pending.RowVersion, 1, "approval.quote.4"))
	if code(err) != cstapi.CodeCostSnapshotIneligible || path(err) != "/data/cost_snapshot_ref" {
		t.Fatalf("quote.4 activated after the denial: %s at %s", code(err), path(err))
	}
	if got := q.auditOf("quote.risk_hold.register"); !slices.Equal(got, []string{"quote.1 " + costHoldActor.ID}) {
		t.Fatalf("TRD audits the holds as %v", got)
	}
	if got := q.auditOf("order.risk.record"); !slices.Equal(got, []string{active.ObjectID + " " + costHoldActor.ID}) {
		t.Fatalf("TRD audits the order risks as %v", got)
	}

	// Once per event and per denial.
	second, err := messaging.NewDispatcher(subs...)
	q.must(err)
	again := denial
	again.ID = eventing.EventID(string(denial.ID) + ".again")
	for _, e := range []struct {
		d   *messaging.Dispatcher
		env eventing.Envelope
	}{{ev.dispatcher, denial}, {second, denial}, {second, again}} {
		if err := e.d.Publish(ctx, e.env); err != nil {
			t.Fatalf("redelivery %s: %v", e.env.ID, err)
		}
	}
	if n, r := q.count("company.A", "brant_trd.quote_risk_hold"), q.count("company.A", "brant_trd.sales_order_risk"); n != 1 || r != 1 ||
		q.getQuote("quote.1").RootVersion != root1+1 {
		t.Fatalf("after redeliveries: %d holds, %d order risks", n, r)
	}

	// The database keeps the rules: no cost hold on a revision a contract formed on, no risk record on
	// a draft order.
	err = q.exec("company.A", `INSERT INTO brant_trd.quote_risk_hold (tenant_id, company_id, quote_id, hold_id, revision, cause, code,
		registered_by_kind, registered_by_id, register_epoch, registered_at, root_version, cost_snapshot_id, cost_source_id, attestation_id, event_id)
		VALUES (current_setting('app.tenant_id'), current_setting('app.company_id'), 'quote.2', 'cost_source_denied:probe', 1,
		'cost_source_denied', 'COST_SOURCE_DENIED', 'system', 'probe', 1, now(), 99, '`+q.snap.SnapshotID+`', 'src.1', 'probe', 'evt.probe')`)
	if err == nil || !strings.Contains(err.Error(), "before a contract formed") {
		t.Fatalf("a cost hold under a contract: %v", err)
	}
	err = q.exec("company.A", `INSERT INTO brant_trd.sales_order_risk (tenant_id, company_id, order_id, risk_id, cause, code, order_revision,
		order_row_version, cost_snapshot_id, cost_source_id, attestation_id, event_id, registered_by_kind, registered_by_id, register_epoch,
		registered_at) VALUES (current_setting('app.tenant_id'), current_setting('app.company_id'), '`+draft.ObjectID+`', 'cost_source_denied:probe',
		'cost_source_denied', 'COST_SOURCE_DENIED', 1, 2, '`+q.snap.SnapshotID+`', 'src.1', 'probe', 'evt.probe', 'system', 'probe', 1, now())`)
	if err == nil || !strings.Contains(err.Error(), "sales_order_risk") {
		t.Fatalf("a risk record on a draft order: %v", err)
	}
}

// heldAfter is subscription s whose handler, once it ran to completion, reports its backend on ran
// and waits for release before its transaction commits.
func heldAfter(s messaging.Subscription, ran chan<- int32, release <-chan struct{}) messaging.Subscription {
	h := s.Handler
	s.Handler = func(ctx context.Context, e messaging.Event) error {
		if err := h(ctx, e); err != nil {
			return err
		}
		var pid int32
		if err := e.Exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			return err
		}
		ran <- pid
		<-release
		return nil
	}
	return s
}

// SC-04 activation first, decision D-9, ADR-BR-013, ADR-BR-011 lock order: the consumer of a denial
// and the customer's acceptance of the same active revision meet on the quote root, which both lock
// FOR UPDATE first. The root's lock decides, in both commit orders:
//   - the verification holds the root (it waits at its action result) while the consumer waits for
//     the root; the verification commits, the contract formed, and the consumer then holds nothing;
//   - the consumer holds the root (its transaction waits before committing) while the verification
//     waits for the root; the hold commits first, and the verification is then refused with
//     COST_SOURCE_DENIED: no contract forms on a revision whose cost source was denied.
func TestCostHoldAndContractFormationDecideByCommitOrder(t *testing.T) {
	setup := func(t *testing.T) (*trdWorld, eventing.Envelope, trdapi.VerifyResponse, []messaging.Subscription) {
		q := newOrderWorld(t)
		ctx := context.Background()
		scope := q.scope("company.A")
		a := q.activated("quote.1")
		_, err := q.c.TRD.RecordResponse(q.author, q.full("response.quote.1", "quote.1", 1, a.BusinessHash))
		q.must(err)
		q.deny()
		denials := outboxOf(t, scope, cstapi.Source, cstapi.EventCostSourceDenied)
		if len(denials) != 1 {
			t.Fatalf("%d denials", len(denials))
		}
		subs := q.c.TRD.Subscriptions()
		if _, err = q.platform.ProvisionInbox(ctx, scope, subs); err != nil {
			t.Fatal(err)
		}
		return q, denials[0], q.verification("response.quote.1", "acceptance.quote.1", 1, a.BusinessHash), subs
	}

	t.Run("the acceptance commits first", func(t *testing.T) {
		q, denial, cmd, subs := setup(t)
		d, err := messaging.NewDispatcher(subs...)
		q.must(err)
		down := newDownstream(q.world)
		blocker, release := down.keyBlocker(t, "brant_trd", trdapi.CommandVerifyResponse, cmd.IdempotencyKey)
		verified := make(chan error, 1)
		go func() {
			_, err := q.c.TRD.VerifyResponse(q.reviewer, cmd)
			verified <- err
		}()
		if !awaitInHold(t, "the verification holds the root and waits at its action result", func() bool { return down.blocks(blocker) }, verified) {
			release()
			return
		}
		consumed := make(chan error, 1)
		go func() { consumed <- d.Publish(context.Background(), denial) }()
		ok := awaitInHold(t, "the consumer waits for the root", func() bool { return lockWaiter(t, "ORDER BY quote_id FOR UPDATE") }, consumed)
		release()
		if !ok {
			return
		}
		if err := <-verified; err != nil {
			t.Fatalf("the verification: %v", err)
		}
		if err := <-consumed; err != nil {
			t.Fatalf("the consumer: %v", err)
		}
		if h := q.holdsOf("quote.1"); len(h) != 0 || q.count("company.A", "brant_trd.acceptance") != 1 {
			t.Fatalf("after the contract formed: holds %v, %d acceptances", h, q.count("company.A", "brant_trd.acceptance"))
		}
	})

	t.Run("the hold commits first", func(t *testing.T) {
		q, denial, cmd, subs := setup(t)
		ran, hold := make(chan int32, 1), make(chan struct{})
		d, err := messaging.NewDispatcher(heldAfter(subs[0], ran, hold))
		q.must(err)
		consumed := make(chan error, 1)
		go func() { consumed <- d.Publish(context.Background(), denial) }()
		var pid int32
		select {
		case pid = <-ran:
		case err := <-consumed:
			t.Fatalf("the consumer ended before it held the root: %v", err)
		case <-time.After(15 * time.Second):
			close(hold)
			t.Fatal("the consumer did not run")
		}
		verified := make(chan error, 1)
		go func() {
			_, err := q.c.TRD.VerifyResponse(q.reviewer, cmd)
			verified <- err
		}()
		ok := awaitInHold(t, "the verification waits for the consumer's root lock", func() bool { return len(blockedBy(t, pid)) > 0 }, verified)
		close(hold)
		if !ok {
			return
		}
		if err := <-consumed; err != nil {
			t.Fatalf("the consumer: %v", err)
		}
		if err := <-verified; code(err) != trdapi.CodeCostSourceDenied {
			t.Fatalf("the verification after the hold: %v", err)
		}
		if h := q.holdsOf("quote.1"); !slices.Equal(h, []string{"cost_source_denied:COST_SOURCE_DENIED@1"}) ||
			q.count("company.A", "brant_trd.acceptance") != 0 {
			t.Fatalf("after the hold: holds %v, %d acceptances", h, q.count("company.A", "brant_trd.acceptance"))
		}
	})
}

// progressWorld is the order world with TSK's actors: the system actor tsk (task.scope, task.wait),
// a sales clerk who owns the tasks and an AI agent they run with.
type progressWorld struct {
	*trdWorld
	sys, clerk context.Context
}

func newProgressWorld(t *testing.T) *progressWorld {
	q := newOrderWorld(t)
	every := append(slices.Clone(allPermissions), tskPermissions...)
	w := &progressWorld{trdWorld: q,
		sys: q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "tsk"},
			Permissions: every, DataScope: "company"}),
		clerk: q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk.1"},
			Permissions: every, DataScope: "company"}),
	}
	q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.t"},
		Permissions: []string{"tsk.read"}, DataScope: "company"})
	return w
}

func (w *progressWorld) task(r tskapi.TaskReceipt, err error) tskapi.TaskReceipt {
	w.t.Helper()
	w.must(err)
	return r
}

// running creates task id for goal on quote in mode and scopes it to running.
func (w *progressWorld) running(id, goal, quote, mode string) tskapi.TaskReceipt {
	w.t.Helper()
	c := tskapi.CreateTask{IdempotencyKey: w.key(), TaskID: id, GoalCode: goal, ControlMode: mode,
		Target: tskapi.Target{RfqID: "rfq.1", QuoteID: quote, CustomerID: "customer.1"}}
	if mode == tskapi.ModeAI {
		c.AgentID = "agent.t"
	}
	r := w.task(w.c.TSK.CreateTask(w.clerk, c))
	r = w.task(w.c.TSK.ScopeTask(w.sys, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion}))
	return w.task(w.c.TSK.ScopeTask(w.clerk, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, Note: "范围确认"}))
}

// waiting is running, then waits for kind: awaiting the object named (approval, external) or the
// missing items (input).
func (w *progressWorld) waiting(id, goal, quote, kind string, awaiting *tskapi.ObjectRef, items ...string) tskapi.TaskReceipt {
	w.t.Helper()
	r := w.running(id, goal, quote, tskapi.ModeAI)
	cmd := tskapi.WaitTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, Wait: kind, Reason: "等待 " + kind,
		ResponsibleID: "clerk.1", Items: items, Awaiting: awaiting}
	if kind == tskapi.WaitExternal {
		cmd.DueAt = kernel.FormatTimestamp(time.Now().Add(48 * time.Hour))
	}
	return w.task(w.c.TSK.WaitTask(w.sys, cmd))
}

func (w *progressWorld) taskView(id string) tskapi.TaskView {
	w.t.Helper()
	v, err := w.c.TSK.GetTask(w.clerk, id)
	w.must(err)
	return v
}

// states are the tasks' lifecycles as "id=state".
func (w *progressWorld) states(ids ...string) []string {
	out := []string{}
	for _, id := range ids {
		out = append(out, id+"="+w.taskView(id).Lifecycle)
	}
	return out
}

// last is the last transition of task id.
func (w *progressWorld) last(id string) tskapi.TransitionView {
	h := w.taskView(id).History
	return h[len(h)-1]
}

// relayTRD copies TRD's new events into the test outbox and relays them all.
func (w *progressWorld) relayTRD(ev *orderEvents) []eventing.Envelope {
	w.t.Helper()
	out := ev.copyFrom(w.scope("company.A"), trdapi.Source)
	ev.drain(ev.relay(5))
	ev.published(out...)
	return out
}

// R18 TRD integration events v1, PL-04, PL-06, SM-TSK-TASK (task.resume waiting_* → running on what
// arrived, system; task.verify running → verifying, system; BC-AT25: an ended task takes no forward
// action): TRD's facts reach tsk.task_progress through the relay. Until the operator provisions
// system:inbox.tsk.task_progress the consumer's task.resume is denied and nothing moves. Then:
//   - QuoteReviewDecided resumes the task awaiting that quote review, recording the arrival; a
//     running task, a task waiting for input and one awaiting an order stay;
//   - QuoteActivated (PL-04) starts the verification of the running quote_ready task and of the one
//     just resumed, and resumes the quote_ready task waiting for input and verifies it; the order
//     task, the paused task, the cancelled task and the task of another quote stay;
//   - OrderReviewDecided resumes the task awaiting the order; OrderActivated (PL-06) starts its
//     verification and that of the running manual order_active task;
//   - a revocation of a quote's approval resumes the task waiting on that quote.
//
// Every move is the consumer's system actor's; a redelivery and a second dispatcher change nothing.
func TestTRDFactsResumeWaitingTasksAndStartVerification(t *testing.T) {
	w := newProgressWorld(t)
	ctx := context.Background()
	scope := w.scope("company.A")
	subs := w.c.TSK.Subscriptions()
	ev := newOrderEvents(t, subs...)

	w.running("task.run", tskapi.GoalQuoteReady, "quote.1", tskapi.ModeAI)
	w.waiting("task.review", tskapi.GoalQuoteReady, "quote.1", tskapi.WaitApproval,
		&tskapi.ObjectRef{Kind: tskapi.ArrivalQuoteReview, ID: "review.quote.1"})
	w.waiting("task.input", tskapi.GoalQuoteReady, "quote.1", tskapi.WaitInput, nil, "Q01.quote")
	w.waiting("task.order", tskapi.GoalOrderActive, "quote.1", tskapi.WaitExternal,
		&tskapi.ObjectRef{Kind: tskapi.ArrivalOrder, ID: "order.quote.1"})
	w.running("task.manual", tskapi.GoalOrderActive, "quote.1", tskapi.ModeManual)
	p := w.running("task.paused", tskapi.GoalQuoteReady, "quote.1", tskapi.ModeAI)
	w.task(w.c.TSK.PauseTask(w.clerk, tskapi.PauseTask{IdempotencyKey: w.key(), TaskID: "task.paused", ExpectedVersion: p.RowVersion,
		Cause: tskapi.PauseOperator, Reason: "人工暂停", ResumeCondition: "经办确认"}))
	c := w.running("task.cancelled", tskapi.GoalQuoteReady, "quote.1", tskapi.ModeAI)
	c = w.task(w.c.TSK.CancelTask(w.clerk, tskapi.CancelTask{IdempotencyKey: w.key(), TaskID: "task.cancelled", ExpectedVersion: c.RowVersion,
		Reason: "客户撤回"}))
	w.task(w.c.TSK.FinishStop(w.sys, tskapi.FinishStop{IdempotencyKey: w.key(), TaskID: "task.cancelled", ExpectedVersion: c.RowVersion}))
	w.waiting("task.other", tskapi.GoalQuoteReady, "quote.9", tskapi.WaitInput, nil, "Q01.quote")
	all := []string{"task.run", "task.review", "task.input", "task.order", "task.manual", "task.paused", "task.cancelled", "task.other"}
	versions := func() map[string]int64 {
		out := map[string]int64{}
		for _, id := range all {
			out[id] = w.taskView(id).TaskVersion
		}
		return out
	}

	// The quote review decided: until provisioned the consumer is denied.
	_, d := w.approved("quote.1")
	decided := outboxOf(t, scope, trdapi.Source, trdapi.EventQuoteReviewDecided)
	if len(decided) != 1 {
		t.Fatalf("%d review decisions", len(decided))
	}
	before := versions()
	if err := ev.dispatcher.Publish(ctx, decided[0]); err == nil || errors.Is(err, eventing.ErrPublishRejected) {
		t.Fatalf("before provisioning: %v, want a retryable denial", err)
	}
	if after := versions(); !maps.Equal(before, after) {
		t.Fatalf("before provisioning the tasks moved: %v → %v", before, after)
	}
	if _, err := w.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	w.relayTRD(ev)
	if got := w.states(all...); !slices.Equal(got, []string{"task.run=running", "task.review=running", "task.input=waiting_input",
		"task.order=waiting_external", "task.manual=running", "task.paused=paused", "task.cancelled=cancelled", "task.other=waiting_input"}) {
		t.Fatalf("after the review decision: %v", got)
	}
	if l := w.last("task.review"); l.Command != tskapi.CommandResume || l.From != tskapi.StateWaitingApproval || l.Actor != progressActor ||
		!strings.HasPrefix(l.Reason, "arrived: quote_review review.quote.1 at version 1 (TRD quote_review_decided.v1 event ") {
		t.Fatalf("task.review resumed by %+v", l)
	}

	// PL-04: QuoteActivated.
	a, err := w.c.TRD.Activate(w.author, w.activateCmd("quote.1", d.RowVersion, 1, "approval.quote.1"))
	w.must(err)
	w.relayTRD(ev)
	if got := w.states(all...); !slices.Equal(got, []string{"task.run=verifying", "task.review=verifying", "task.input=verifying",
		"task.order=waiting_external", "task.manual=running", "task.paused=paused", "task.cancelled=cancelled", "task.other=waiting_input"}) {
		t.Fatalf("after the activation: %v", got)
	}
	h := w.taskView("task.input").History
	if r, v := h[len(h)-2], h[len(h)-1]; r.Command != tskapi.CommandResume || r.Actor != progressActor ||
		!strings.HasPrefix(r.Reason, "arrived: quote quote.1 at version 1 (TRD quote_activated.v1 event ") ||
		v.Command != tskapi.CommandVerify || v.From != tskapi.StateRunning || v.Actor != progressActor {
		t.Fatalf("task.input: %+v then %+v", r, v)
	}

	// PL-06: the customer accepts, the order is prepared, approved and activated.
	_, err = w.c.TRD.RecordResponse(w.author, w.full("response.quote.1", "quote.1", 1, a.BusinessHash))
	w.must(err)
	acc, err := w.c.TRD.VerifyResponse(w.reviewer, w.verification("response.quote.1", "acceptance.quote.1", 1, a.BusinessHash))
	w.must(err)
	o, err := w.c.TRD.PrepareOrder(w.author, w.prepareCmd("order.quote.1", acc))
	w.must(err)
	od := w.reviewed(o, "order.review.1")
	w.relayTRD(ev)
	if got := w.states("task.order", "task.manual"); !slices.Equal(got, []string{"task.order=running", "task.manual=running"}) {
		t.Fatalf("after the order review: %v", got)
	}
	if l := w.last("task.order"); !strings.HasPrefix(l.Reason, "arrived: order order.quote.1 at version 1 (TRD order_review_decided.v1 event ") {
		t.Fatalf("task.order resumed by %+v", l)
	}
	_, err = w.c.TRD.ActivateOrder(w.author, w.activateOrderCmd(o.ObjectID, od.RowVersion, "order.review.1"))
	w.must(err)
	w.relayTRD(ev)
	if got := w.states(all...); !slices.Equal(got, []string{"task.run=verifying", "task.review=verifying", "task.input=verifying",
		"task.order=verifying", "task.manual=verifying", "task.paused=paused", "task.cancelled=cancelled", "task.other=waiting_input"}) {
		t.Fatalf("after the order's activation: %v", got)
	}
	if l := w.last("task.manual"); l.Command != tskapi.CommandVerify || l.Actor != progressActor {
		t.Fatalf("task.manual verified by %+v", l)
	}

	// An approval revoked wakes the task waiting on its quote.
	w.activated("quote.2")
	w.relayTRD(ev)
	w.waiting("task.reply", tskapi.GoalQuoteReady, "quote.2", tskapi.WaitExternal,
		&tskapi.ObjectRef{Kind: tskapi.ArrivalCustomerResponse, ID: "response.later"})
	_, err = w.c.TRD.RevokeApproval(w.reviewer, trdapi.RevokeApproval{IdempotencyKey: w.key(), QuoteID: "quote.2", ApprovalID: "approval.quote.2",
		ExpectedEffectivityVersion: 1, Reason: "客户信用变化", Evidence: w.proof})
	w.must(err)
	w.relayTRD(ev)
	if l := w.last("task.reply"); w.taskView("task.reply").Lifecycle != tskapi.StateRunning || l.Command != tskapi.CommandResume ||
		!strings.HasPrefix(l.Reason, "arrived: quote_approval approval.quote.2 at version 2 (TRD quote_approval_revoked.v1 event ") {
		t.Fatalf("task.reply after the revocation: %+v", l)
	}

	// Redelivery of every TRD event, by the relay's dispatcher and a second one: nothing moves.
	all = append(all, "task.reply")
	before = versions()
	second, err := messaging.NewDispatcher(subs...)
	w.must(err)
	for _, e := range outboxOf(t, scope, trdapi.Source) {
		for _, d := range []*messaging.Dispatcher{ev.dispatcher, second} {
			if err := d.Publish(ctx, e); err != nil {
				t.Fatalf("redelivery of %s: %v", e.Type, err)
			}
		}
	}
	if after := versions(); !maps.Equal(before, after) {
		t.Fatalf("after redeliveries: %v → %v", before, after)
	}
	// TSK's audit trail names the consumer for every move it made.
	entries, err := w.c.TSK.Audit(w.clerk)
	w.must(err)
	moves := map[string]int{}
	for _, e := range entries {
		if e.Actor == progressActor && e.Outcome == "succeeded" {
			moves[e.Command]++
		}
	}
	if moves[tskapi.CommandResume] != 4 || moves[tskapi.CommandVerify] != 5 {
		t.Fatalf("the consumer's moves in TSK's audit: %v", moves)
	}
}
