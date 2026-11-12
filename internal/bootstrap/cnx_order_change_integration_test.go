//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// CNX follows order and regulation changes (S5 unit C4b-CNX, WBS S5-09b): the Inbox consumer
// cnx.trade_compliance on PostgreSQL 16, through the assembled relay with the composed process's
// dispatcher as its publisher (Platform.Relay, as brant-worker -dispatch runs it) and the assembled
// replayer, fed with the events TRD, POL and reference data wrote to their Outbox for real orders,
// amendments, publications and imports (copied unchanged into a throwaway outbox per test, so the
// relay claims only the test's events). Design: event storming PL-14; WF-06 last step and its
// idempotency ("事件 ID 去重；各下游按（订单、订单修订）只开启一次复核"); SM-CNX-COMPLIANCE clear →
// reopen → pending (actor system); CNX-INV-001 ("订单变更改动目的国、交易方或商品，或法规版本更新
// 时，结论对新修订不适用、须重新核查"); S5 D-9, D-18; MD-47; Context Map R46, R50. Python V0.50 has no
// CNX, so the tests cite design IDs only.

// followActor is the system actor cnx.trade_compliance runs as.
var followActor = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.cnx.trade_compliance"}

// cnxSubscription returns the subscription of cnx.trade_compliance to source among subs.
func cnxSubscription(t *testing.T, subs []messaging.Subscription, source string) (int, messaging.Subscription) {
	t.Helper()
	for i, s := range subs {
		if s.Consumer == "cnx.trade_compliance" && s.Source == source {
			return i, s
		}
	}
	t.Fatalf("no cnx.trade_compliance subscription to %s", source)
	return 0, messaging.Subscription{}
}

// relayed copies the events sources wrote in company.A since the last copy into ev's outbox, has
// the relay settle them all and checks each is published; it returns them.
func (w *cnxWorld) relayed(ev *orderEvents, sources ...string) []eventing.Envelope {
	w.t.Helper()
	var out []eventing.Envelope
	for _, s := range sources {
		out = append(out, ev.copyFrom(w.scope("company.A"), s)...)
	}
	ev.drain(ev.relay(5))
	ev.published(out...)
	return out
}

// envelopeOf returns the envelope of type typ about subject among envs.
func envelopeOf(t *testing.T, envs []eventing.Envelope, typ, subject string) eventing.Envelope {
	t.Helper()
	for _, e := range envs {
		if string(e.Type) == typ && e.Subject == subject {
			return e
		}
	}
	t.Fatalf("no %s about %s among %d events", typ, subject, len(envs))
	return eventing.Envelope{}
}

// reopened returns CNX's TradeComplianceReopened events of cause about order ("" for any order).
func (w *cnxWorld) reopened(cause, order string) []event {
	var out []event
	for _, e := range w.events("company.A", cnxapi.Source) {
		if e.Type == cnxapi.EventTradeComplianceReopened && e.Payload["cause"] == cause && (order == "" || e.Payload["order_id"] == order) {
			out = append(out, e)
		}
	}
	return out
}

// checkOf returns the check of order's revision rev as the compliance officer reads it, or fails.
func (w *cnxWorld) checkOf(order string, rev int64) cnxapi.CheckView {
	w.t.Helper()
	checks, err := w.cnx.OrderChecks(w.compliance, order)
	w.must(err)
	for _, c := range checks {
		if c.OrderRevision == rev {
			v, err := w.cnx.GetCheck(w.compliance, c.CheckID)
			w.must(err)
			return v
		}
	}
	w.t.Fatalf("order %s revision %d has no check: %+v", order, rev, checks)
	return cnxapi.CheckView{}
}

// revisions returns the order revisions order has checks for, newest first.
func (w *cnxWorld) revisions(order string) []int64 {
	w.t.Helper()
	checks, err := w.cnx.OrderChecks(w.compliance, order)
	w.must(err)
	var out []int64
	for _, c := range checks {
		out = append(out, c.OrderRevision)
	}
	return out
}

// amendTo has order amended from its current revision to quantity qty of L1 as amendment id and
// returns the new revision's binding.
func (w *cnxWorld) amendTo(order, id, qty string) cnxapi.OrderBinding {
	w.t.Helper()
	b := w.binding(order)
	d := w.approvedAmendment(order, id, b.OrderRevision, b.OrderHash, qty)
	r, err := w.c.TRD.ApplyAmendment(w.reviewer, w.applyCmd(order, id, d.RowVersion))
	w.must(err)
	if r.OrderRevision != b.OrderRevision+1 {
		w.t.Fatalf("amended %+v", r)
	}
	return cnxapi.OrderBinding{OrderID: order, OrderRevision: r.OrderRevision, OrderHash: r.OrderHash}
}

// PL-14, WF-06 (CNX trade_compliance.reopen 系统，为新订单修订开启待核查), D-9, CNX-INV-001: TRD's
// OrderAmended of two active orders, cleared at revision 1, is relayed to cnx.trade_compliance.
// Until the operator provisions system:inbox.cnx.trade_compliance its reopening is denied and the
// delivery retried, nothing opened. Once provisioned, each amended revision gets one pending check,
// opened by the consumer's system actor and linked to revision 1's, and TradeComplianceGuard v1
// answers TRADE_COMPLIANCE_NOT_CLEAR for it until compliance evaluates and concludes; revision 1's
// conclusion is untouched and still binds only revision 1. Two relays and a third worker deliver
// the events at once, and redeliveries follow: one check and one TradeComplianceReopened per
// revision.
func TestOrderAmendedOpensOnePendingCheckPerRevision(t *testing.T) {
	w := newCnxWorld(t)
	ctx := context.Background()
	scope := w.scope("company.A")
	subs := w.c.Subscriptions()
	ev := newOrderEvents(t, subs...)
	orders := []string{w.activeOrder("quote.1").ObjectID, w.activeOrder("quote.2").ObjectID}
	first := map[string]cnxapi.CheckView{}
	b1 := map[string]cnxapi.OrderBinding{}
	b2 := map[string]cnxapi.OrderBinding{}
	for _, o := range orders {
		first[o], b1[o] = w.checkOf(o, 1), w.binding(o)
		b2[o] = w.amendTo(o, "amendment."+o, "90")
		w.wantVerdict(b2[o], cnxapi.StatusPending, cnxapi.ReasonOrderRevised)
	}
	envs := ev.copyFrom(scope, trdapi.Source)
	amended := []eventing.Envelope{envelopeOf(t, envs, trdapi.EventOrderAmended, orders[0]), envelopeOf(t, envs, trdapi.EventOrderAmended, orders[1])}

	if err := ev.dispatcher.Publish(ctx, amended[0]); err == nil || errors.Is(err, eventing.ErrPublishRejected) {
		t.Fatalf("before provisioning: %v; want the delivery retried", err)
	}
	if got := w.revisions(orders[0]); !slices.Equal(got, []int64{1}) {
		t.Fatalf("before provisioning: checks of revisions %v", got)
	}
	if _, err := w.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	second, err := messaging.NewDispatcher(subs...)
	w.must(err)
	var wg sync.WaitGroup
	errs := make([]error, 2+len(amended))
	for i := range 2 {
		wg.Go(func() { errs[i] = drain(ev.relay(5)) })
	}
	for i, e := range amended {
		wg.Go(func() {
			// Losing the race is a retry (eventing.ErrInboxBusy, or the entry's lock), never a rejection.
			if err := second.Publish(ctx, e); errors.Is(err, eventing.ErrPublishRejected) {
				errs[2+i] = err
			}
		})
	}
	wg.Wait()
	w.must(errors.Join(errs...))
	ev.drain(ev.relay(5))
	ev.published(envs...)
	for _, e := range amended {
		for _, d := range []*messaging.Dispatcher{ev.dispatcher, second} {
			if err := d.Publish(ctx, e); err != nil {
				t.Fatalf("redelivery: %v", err)
			}
		}
	}

	for i, o := range orders {
		if got := w.revisions(o); !slices.Equal(got, []int64{2, 1}) {
			t.Fatalf("%s: checks of revisions %v", o, got)
		}
		c2 := w.checkOf(o, 2)
		if c2.Status != cnxapi.StatusPending || c2.RowVersion != 1 || c2.State != cnxapi.StatusPending || c2.Reason != cnxapi.ReasonNotEvaluated ||
			c2.PreviousCheckID != first[o].CheckID || c2.OpenedBy != followActor || c2.OrderHash != b2[o].OrderHash || len(c2.Evaluations) != 0 {
			t.Fatalf("%s revision 2: %+v", o, c2)
		}
		if rs := c2.Reopenings; len(rs) != 1 || rs[0].Cause != cnxapi.CauseOrderAmended || rs[0].ReopenedBy != followActor ||
			!strings.Contains(rs[0].Reason, string(amended[i].ID)) || rs[0].DecisionID != "" {
			t.Fatalf("%s revision 2 reopenings %+v", o, rs)
		}
		if c1 := w.checkOf(o, 1); c1.Status != cnxapi.StatusClear || c1.RowVersion != first[o].RowVersion || len(c1.Reopenings) != 0 ||
			c1.CurrentDecisionID != first[o].CurrentDecisionID {
			t.Fatalf("%s revision 1 changed: %+v", o, c1)
		}
		w.wantVerdict(b2[o], cnxapi.StatusPending, cnxapi.ReasonNotEvaluated)
		w.wantVerdict(b1[o], cnxapi.StatusClear, cnxapi.ReasonCleared)
		if n := len(w.reopened(cnxapi.CauseOrderAmended, o)); n != 1 {
			t.Fatalf("%s: %d TradeComplianceReopened", o, n)
		}
	}

	// Only compliance's evaluation and decision clear the new revision.
	o := orders[0]
	e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o, 2, 1, syntheticClear))
	w.must(err)
	w.wantVerdict(b2[o], cnxapi.StatusPending, cnxapi.ReasonAwaitingDecision)
	_, err = w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
	w.must(err)
	w.wantVerdict(b2[o], cnxapi.StatusClear, cnxapi.ReasonCleared)
}

// WF-06 idempotency and S4-06a (a dead-letter replay arrives after later events of its order):
// an OrderAmended dead-lettered while the consumer was broken and replayed late opens nothing twice
// and never touches a concluded check — when compliance evaluated and concluded the revision
// meanwhile (its check stays as compliance left it), when the order was amended again (the later
// revision's OrderAmended opened that revision's check; nothing is opened for the superseded
// one), and when the order was cancelled (an ended order gets no check). A second replay is refused
// as replayed; an OrderAmended TRD contradicts is refused for good (ADR-BR-015).
func TestLateOrderAmendedOpensNothing(t *testing.T) {
	w := newCnxWorld(t)
	ctx := context.Background()
	scope := w.scope("company.A")
	subs := w.c.Subscriptions()
	var broken atomic.Bool
	i, s := cnxSubscription(t, subs, trdapi.Source)
	wrapped := slices.Clone(subs)
	wrapped[i] = breakable(s, &broken, true)
	ev := newOrderEvents(t, wrapped...)
	if _, err := w.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	operator := w.provision(bootstrap.Grant{Scope: scope, Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"},
		Permissions: []string{messaging.ReplayPermission}})
	replayer := ev.replayer()
	// deadLettered relays TRD's new events with the consumer broken: every other consumer settles
	// them, the OrderAmended of order is dead-lettered.
	deadLettered := func(order string) eventing.Envelope {
		t.Helper()
		envs := ev.copyFrom(scope, trdapi.Source)
		broken.Store(true)
		ev.drain(ev.relay(1))
		broken.Store(false)
		e := envelopeOf(t, envs, trdapi.EventOrderAmended, order)
		if st, a := ev.status(e.ID), ev.alerts.of(e.ID); st != "ABANDONED" || len(a) != 1 || a[0].Code != messaging.FailurePublishFailed {
			t.Fatalf("broken consumer: outbox %s, alerts %+v", st, a)
		}
		return e
	}
	replay := func(e eventing.Envelope) {
		t.Helper()
		if _, err := replayer.Replay(operator, e.Source, string(e.ID)); err != nil {
			t.Fatalf("replay of %s: %v", e.ID, err)
		}
	}

	// Compliance concluded the revision before the replay.
	a := w.activeOrder("quote.1").ObjectID
	a2 := w.amendTo(a, "amendment.a", "90")
	lateA := deadLettered(a)
	e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(a, 2, 0, syntheticClear))
	w.must(err)
	concluded, err := w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
	w.must(err)
	replay(lateA)
	if c := w.checkOf(a, 2); c.CheckID != concluded.CheckID || c.Status != cnxapi.StatusClear || c.RowVersion != concluded.RowVersion ||
		c.OpenedBy.ID != "officer" || len(c.Reopenings) != 0 {
		t.Fatalf("concluded revision after the replay: %+v", c)
	}
	w.wantVerdict(a2, cnxapi.StatusClear, cnxapi.ReasonCleared)
	if _, err = replayer.Replay(operator, lateA.Source, string(lateA.ID)); !errors.Is(err, messaging.ErrAlreadyReplayed) {
		t.Fatalf("second replay: %v", err)
	}

	// The order was amended again before the replay.
	b := w.activeOrder("quote.2").ObjectID
	w.amendTo(b, "amendment.b2", "90")
	lateB := deadLettered(b)
	b3 := w.amendTo(b, "amendment.b3", "80")
	w.relayed(ev, trdapi.Source)
	replay(lateB)
	if got := w.revisions(b); !slices.Equal(got, []int64{3, 1}) {
		t.Fatalf("amended twice: checks of revisions %v", got)
	}
	if c := w.checkOf(b, 3); c.Status != cnxapi.StatusPending || c.OpenedBy != followActor || c.PreviousCheckID != w.checkOf(b, 1).CheckID {
		t.Fatalf("revision 3: %+v", c)
	}
	w.wantVerdict(b3, cnxapi.StatusPending, cnxapi.ReasonNotEvaluated)

	// The order was cancelled before the replay.
	c := w.activeOrder("quote.3").ObjectID
	w.amendTo(c, "amendment.c", "90")
	lateC := deadLettered(c)
	_, err = w.c.TRD.CancelOrder(w.author, trdapi.CancelOrder{IdempotencyKey: w.key(), OrderID: c, ExpectedVersion: w.orderVersion(c),
		Reason: "客户撤单", Evidence: w.proof})
	w.must(err)
	w.relayed(ev, trdapi.Source)
	replay(lateC)
	if got := w.revisions(c); !slices.Equal(got, []int64{1}) {
		t.Fatalf("cancelled: checks of revisions %v", got)
	}
	if n := len(w.reopened(cnxapi.CauseOrderAmended, "")); n != 1 {
		t.Fatalf("%d checks opened for amended revisions, want revision 3 of %s only", n, b)
	}

	// An OrderAmended TRD contradicts (a revision TRD does not have) is refused for good: dead
	// letter and alert at once naming cnx.order_not_checkable, nothing opened, the replay refused.
	forged := salesOrderEvent(scope, trdapi.EventOrderAmended, stl.OrderBinding{OrderID: b, OrderRevision: 4, OrderHash: strings.Repeat("c", 64)},
		w.orderVersion(b)+1, "active", map[string]any{"amendment_id": "amendment.forged"})
	ev.enqueue(forged)
	ev.once(ev.relay(5))
	if st, al := ev.status(forged.ID), ev.alerts.of(forged.ID); st != "ABANDONED" || len(al) != 1 || al[0].Code != messaging.FailurePublishRejected ||
		!strings.Contains(al[0].Error, "cnx.order_not_checkable") {
		t.Fatalf("forged amendment: outbox %s, alerts %+v", st, al)
	}
	if _, err = replayer.Replay(operator, forged.Source, string(forged.ID)); !errors.Is(err, messaging.ErrReplayFailed) {
		t.Fatalf("replay of a refused amendment: %v", err)
	}
	if got := w.revisions(b); !slices.Equal(got, []int64{3, 1}) {
		t.Fatalf("after the forged amendment: checks of revisions %v", got)
	}
}

// S5 D-18, SM-CNX-COMPLIANCE clear → reopen → pending (新的 screening_regulation 修订发布, actor
// system), CNX-INV-001 (法规版本更新时须重新核查), R50: POL's ConfigPublished of screening_regulation
// revision 2 reopens, as the consumer's system actor, every clear conclusion that follows revision
// 1 — a draft order's and an active order's — naming the revision; a blocked conclusion and a
// pending check stay as they are. The publication of revision 1, delivered after revision 2
// superseded it, and POL's publications of other kinds reopen nothing; a redelivery reopens
// nothing twice. The follow-up read shows the clear conclusions of revision 1 before (also over
// HTTP) and none after, and the conclusion compliance gives under revision 2 afterwards.
func TestRegulationPublicationReopensTheConclusionsOfEarlierRevisions(t *testing.T) {
	w := newCnxWorld(t)
	ctx := context.Background()
	scope := w.scope("company.A")
	subs := w.c.Subscriptions()
	ev := newOrderEvents(t, subs...)
	if _, err := w.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	_, draft := w.prepared("quote.1")
	cleared := w.cleared(draft.ObjectID)
	active := w.activeOrder("quote.2").ObjectID
	activeCheck := w.checkOf(active, 1)
	_, embargoed := w.prepared("quote.3")
	e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(embargoed.ObjectID, 1, 0, syntheticEmbargoed))
	w.must(err)
	blocked, err := w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionBlocked, false))
	w.must(err)
	_, waiting := w.prepared("quote.4")
	pending, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(waiting.ObjectID, 1, 0, syntheticClear))
	w.must(err)
	w.relayed(ev, polapi.Source, trdapi.Source)

	rev1 := w.regulation.Revision
	byRegulation := func(revision int64) []string {
		t.Helper()
		cs, err := w.cnx.ChecksByRegulation(w.compliance, "screening", revision)
		w.must(err)
		var out []string
		for _, c := range cs {
			out = append(out, c.CheckID)
		}
		return out
	}
	want := []string{cleared.CheckID, activeCheck.CheckID}
	slices.Sort(want)
	if got := byRegulation(rev1); !slices.Equal(got, want) || !slices.Equal(byRegulation(0), want) {
		t.Fatalf("clear conclusions of revision %d: %v, want %v", rev1, got, want)
	}
	cs, err := w.cnx.ChecksByRegulation(w.compliance, "screening", rev1)
	w.must(err)
	if c := cs[slices.IndexFunc(cs, func(c cnxapi.ClearCheck) bool { return c.CheckID == cleared.CheckID })]; c.OrderID != draft.ObjectID ||
		c.RegulationRevision != rev1 || c.State != cnxapi.StatusClear || c.Reason != cnxapi.ReasonCleared || len(c.Programmes) != 2 ||
		c.DecisionID != cleared.DecisionID {
		t.Fatalf("clear conclusion %+v", c)
	}

	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	officer := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "officer", "human"), n: 400}
	status, body := officer.do("GET", "/api/brant/v1/cnx/regulations/screening/trade_compliance?revision=1", nil)
	if checks, _ := body["checks"].([]any); status != 200 || len(checks) != 2 || checks[0].(map[string]any)["regulation_revision"] != float64(rev1) {
		t.Fatalf("over HTTP: %d %v", status, body)
	}
	if status, body = officer.do("GET", "/api/brant/v1/cnx/regulations/screening/trade_compliance?revision=one", nil); status != 422 ||
		body["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("malformed revision: %d %v", status, body)
	}

	cfg, err := w.c.POL.GetConfig(w.author, polapi.KindScreeningRegulation, "screening")
	w.must(err)
	reg2 := w.configure(w.author, w.reviewer, polapi.KindScreeningRegulation, "screening", cfg.RootVersion, w.win.from, time.Time{},
		polapi.ConfigData{ScreeningRegulation: &polapi.ScreeningRegulation{Lists: []polapi.ScreeningList{cnList}, RescreenIntervalDays: 14}}, true)
	envs := w.relayed(ev, polapi.Source)
	var publication eventing.Envelope
	for _, env := range envs {
		var p struct {
			Kind     string `json:"kind"`
			Revision int64  `json:"revision"`
		}
		w.must(json.Unmarshal(env.Payload, &p))
		if env.Type == polapi.EventConfigPublished && p.Kind == polapi.KindScreeningRegulation && p.Revision == reg2.Revision {
			publication = env
		}
	}
	if publication.ID == "" {
		t.Fatalf("no publication of revision %d among %d POL events", reg2.Revision, len(envs))
	}
	w.must(ev.dispatcher.Publish(ctx, publication))

	for _, c := range []struct {
		order, check, decision string
	}{{draft.ObjectID, cleared.CheckID, cleared.DecisionID}, {active, activeCheck.CheckID, activeCheck.CurrentDecisionID}} {
		v, err := w.cnx.GetCheck(w.compliance, c.check)
		w.must(err)
		if v.Status != cnxapi.StatusPending || v.Reason != cnxapi.ReasonNotEvaluated || len(v.Reopenings) != 1 {
			t.Fatalf("%s after revision %d: %+v", c.order, reg2.Revision, v)
		}
		r := v.Reopenings[0]
		if r.Cause != cnxapi.CauseRegulationRevised || r.RegulationID != "screening" || r.RegulationRevision != reg2.Revision ||
			r.DecisionID != c.decision || r.ReopenedBy != followActor || !strings.Contains(r.Reason, string(publication.ID)) {
			t.Fatalf("%s reopening %+v", c.order, r)
		}
		w.wantVerdict(w.binding(c.order), cnxapi.StatusPending, cnxapi.ReasonNotEvaluated)
	}
	if v, err := w.cnx.GetCheck(w.compliance, blocked.CheckID); err != nil || v.Status != cnxapi.StatusBlocked || v.RowVersion != blocked.RowVersion {
		t.Fatalf("blocked conclusion: %+v %v", v, err)
	}
	if v, err := w.cnx.GetCheck(w.compliance, pending.CheckID); err != nil || v.Status != cnxapi.StatusPending || v.RowVersion != pending.RowVersion ||
		v.CurrentEvaluationID != pending.EvaluationID {
		t.Fatalf("pending check: %+v %v", v, err)
	}
	if n := len(w.reopened(cnxapi.CauseRegulationRevised, "")); n != 2 {
		t.Fatalf("%d reopened for the regulation, want 2", n)
	}
	if got := byRegulation(rev1); len(got) != 0 {
		t.Fatalf("clear conclusions of revision %d after revision %d: %v", rev1, reg2.Revision, got)
	}

	w.regulation = reg2
	e, err = w.cnx.Evaluate(w.compliance, w.evaluateCmd(draft.ObjectID, 1, w.checkOf(draft.ObjectID, 1).RowVersion, syntheticClear))
	w.must(err)
	_, err = w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
	w.must(err)
	if got := byRegulation(reg2.Revision); !slices.Equal(got, []string{cleared.CheckID}) {
		t.Fatalf("clear conclusions of revision %d: %v", reg2.Revision, got)
	}
}

// MD-47 (新版本触发复筛), CNX-INV-001: reference data's CountryProgrammesImported of version v2 of
// the CN programme list reopens, as the consumer's system actor, the clear conclusion whose
// evaluation used v1 (programme_updated, the list named); a clear conclusion evaluated against the
// UN list alone stays clear. Between the import and its delivery TradeComplianceGuard v1 already
// refuses the outdated conclusion (pending, list_updated) and the follow-up read shows it so; the
// earlier imports, delivered after v2, and a redelivery reopen nothing more.
func TestProgrammeImportReopensTheConclusionsThatUsedAnotherVersion(t *testing.T) {
	w := newCnxWorld(t)
	ctx := context.Background()
	scope := w.scope("company.A")
	subs := w.c.Subscriptions()
	ev := newOrderEvents(t, subs...)
	if _, err := w.platform.ProvisionInbox(ctx, scope, subs); err != nil {
		t.Fatal(err)
	}
	_, both := w.prepared("quote.1")
	withCN := w.cleared(both.ObjectID)
	_, un := w.prepared("quote.2")
	cmd := w.evaluateCmd(un.ObjectID, 1, 0, syntheticClear)
	cmd.ProgrammeLists = []string{unProgrammeList}
	e, err := w.cnx.Evaluate(w.compliance, cmd)
	w.must(err)
	unOnly, err := w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
	w.must(err)

	imp := syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-10")
	imp.IdempotencyKey = w.key()
	_, err = w.c.REF.ImportCountryProgrammes(w.compliance, imp)
	w.must(err)
	w.wantVerdict(w.binding(both.ObjectID), cnxapi.StatusPending, cnxapi.ReasonListUpdated)
	byList := func(code, version string) []cnxapi.ClearCheck {
		t.Helper()
		cs, err := w.cnx.ChecksByList(w.compliance, code, version)
		w.must(err)
		return cs
	}
	if cs := byList(cnProgrammeList, "v1"); len(cs) != 1 || cs[0].CheckID != withCN.CheckID || cs[0].State != cnxapi.StatusPending ||
		cs[0].Reason != cnxapi.ReasonListUpdated {
		t.Fatalf("clear conclusions on %s v1: %+v", cnProgrammeList, cs)
	}
	if cs := byList(unProgrammeList, ""); len(cs) != 2 {
		t.Fatalf("clear conclusions on %s: %+v", unProgrammeList, cs)
	}

	var v2 eventing.Envelope
	imports := 0
	for _, env := range w.relayed(ev, refapi.Source) {
		if env.Type == refapi.EventCountryProgrammesImported {
			imports++
			var p struct {
				ListCode string `json:"list_code"`
				Version  string `json:"version"`
			}
			w.must(json.Unmarshal(env.Payload, &p))
			if p.ListCode == cnProgrammeList && p.Version == "v2" {
				v2 = env
			}
		}
	}
	if v2.ID == "" || imports != 3 {
		t.Fatalf("%d imports relayed, v2 %q", imports, v2.ID)
	}
	w.must(ev.dispatcher.Publish(ctx, v2))

	v, err := w.cnx.GetCheck(w.compliance, withCN.CheckID)
	w.must(err)
	if v.Status != cnxapi.StatusPending || v.Reason != cnxapi.ReasonNotEvaluated || len(v.Reopenings) != 1 ||
		v.Reopenings[0].Cause != cnxapi.CauseProgrammeUpdated || strings.Join(v.Reopenings[0].UpdatedLists, ",") != cnProgrammeList ||
		v.Reopenings[0].ReopenedBy != followActor || v.Reopenings[0].DecisionID != withCN.DecisionID {
		t.Fatalf("conclusion on v1 after v2: %+v", v)
	}
	if v, err := w.cnx.GetCheck(w.compliance, unOnly.CheckID); err != nil || v.Status != cnxapi.StatusClear || v.RowVersion != unOnly.RowVersion {
		t.Fatalf("conclusion on the UN list: %+v %v", v, err)
	}
	w.wantVerdict(w.binding(un.ObjectID), cnxapi.StatusClear, cnxapi.ReasonCleared)
	if n := len(w.reopened(cnxapi.CauseProgrammeUpdated, "")); n != 1 {
		t.Fatalf("%d reopened for the list, want 1", n)
	}
	if cs := byList(cnProgrammeList, ""); len(cs) != 0 {
		t.Fatalf("clear conclusions on %s after the reopening: %+v", cnProgrammeList, cs)
	}
}
