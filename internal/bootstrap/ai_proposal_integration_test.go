//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/ai/execution"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/ai/proposal"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The proposal of a command that needs an approval grant (S4 G7b, S4-03; owner decision 2026-11-18
// 20:15; AI-INV-002, F1, AN04, AN05, AN06): the owning context judges the request without writing
// before a person is asked, the platform keeps the request, the approver reads it, and after the grant
// the platform sends exactly it, through the path a tool call takes from the grant on, without asking
// the model. A proposal is never authority: everything a tool call checks is checked again then.

// skewClock is the runner's clock moved by a fixed offset from the real one.
type skewClock time.Duration

func (c skewClock) Now() time.Time {
	return time.Now().Add(time.Duration(c)).UTC().Truncate(time.Microsecond)
}

// proposals returns the proposals of task as the agent reads its own.
func (w *agentWorld) proposals(task string) []proposal.Proposal {
	w.t.Helper()
	db, closeDB := w.helperDB()
	defer closeDB()
	ps, err := proposal.Store{DB: db, Clock: skewClock(0)}.OfTask(w.agent, task)
	w.must(err)
	return ps
}

// onlyProposal returns the one proposal of task.
func (w *agentWorld) onlyProposal(task string) proposal.Proposal {
	w.t.Helper()
	ps := w.proposals(task)
	if len(ps) != 1 {
		w.t.Fatalf("proposals %+v", ps)
	}
	return ps[0]
}

func (w *agentWorld) noOrder(id string) {
	w.t.Helper()
	_, err := w.c.TRD.GetOrder(w.author, id)
	w.want(err, kernel.CodeNotFound)
}

func (w *agentWorld) prepareArgs(order string, acc trdapi.AcceptanceReceipt) map[string]any {
	return map[string]any{"OrderID": order, "QuoteID": acc.QuoteID, "BusinessRevision": acc.BusinessRevision,
		"BusinessHash": acc.BusinessHash, "AcceptanceID": acc.ObjectID}
}

func requestText(r model.Request) string {
	var b strings.Builder
	for _, m := range r.Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// proposed is a task whose agent proposed order.prepare and whose owner has not decided: the task
// waits for an approval grant.
type proposed struct {
	*agentWorld
	r                     *execution.Runner
	acc                   trdapi.AcceptanceReceipt
	task, order, hash, id string
}

func newProposed(t *testing.T, task, order string) *proposed {
	t.Helper()
	w := newAgentWorld(t, true)
	_, acc := w.accepted("quote.1")
	m := model.NewScripted("scripted-q01-v1", calls(tc("c1", trdapi.CommandPrepareOrder, w.prepareArgs(order, acc))))
	r := w.runner(m)
	w.aiTask(task, tskapi.GoalOrderActive, tskapi.Target{QuoteID: "quote.1", CustomerID: "customer.1"}, budgetOf(10, 2), bundleRef(r),
		trdapi.CommandPrepareOrder)
	if res := w.turnOn(r, task); res.Waiting != tskapi.WaitApproval {
		t.Fatalf("first turn %+v, ledger %+v", res, w.ledger(task))
	}
	v := w.taskView(task)
	return &proposed{agentWorld: w, r: r, acc: acc, task: task, order: order,
		hash: strings.TrimPrefix(v.Wait.Items[1], "request_hash "), id: v.Wait.Awaiting.ID}
}

// grant has the clerk issue an approval grant for exactly the proposed request.
func (p *proposed) grant(id string, expires time.Time) {
	p.t.Helper()
	g := approvalgrantIssue(p.agentWorld, id, "del."+p.task, trdapi.CommandPrepareOrder, p.hash, "quote.1", p.acc.BusinessRevision)
	g.ExpiresAt = expires
	_, err := p.c.AI.IssueApprovalGrant(p.clerk, g)
	p.must(err)
}

// grantOf reads grant id.
func (w *agentWorld) grantStatus(id string) string {
	w.t.Helper()
	g, err := w.c.AI.GetApprovalGrant(w.clerk, id)
	w.must(err)
	return g.Status
}

// What TRD refuses without writing is refused in the same turn, before anyone is asked: an order.prepare
// whose notify party is an address of another role (INVALID_KNOWN_VALUE at /args/notify_party_ref, with
// the static reason the model is told) leaves no proposal, no wait, no grant and no action; the model
// corrects it and that request is stored once. After the grant the platform sends exactly the stored
// request with no model call between the grant and the commit (a runner that took over after a restart
// does it), uses the one grant by the action's key, and the facts of the model's next turn carry the
// committed action, so it need not repeat the call. The approver reads the request as the agent
// proposed it, without the idempotency key.
func TestProposalIsJudgedBeforeAPersonIsAskedAndSentAfterTheGrant(t *testing.T) {
	w := newAgentWorld(t, true)
	_, acc := w.accepted("quote.1")
	notify := w.notifyParty()
	good := w.prepareArgs("order.ai.1", acc)
	bad := maps.Clone(good)
	bad["NotifyPartyRef"] = map[string]any{"PartyID": notify.PartyID, "PartyRevision": notify.PartyRevision, "AddressID": "dock"}
	m := model.NewScripted("scripted-q01-v1", calls(tc("c1", trdapi.CommandPrepareOrder, bad)), calls(tc("c2", trdapi.CommandPrepareOrder, good)))
	r := w.runner(m)
	w.aiTask("task.judged", tskapi.GoalOrderActive, tskapi.Target{QuoteID: "quote.1", CustomerID: "customer.1"}, budgetOf(10, 2), bundleRef(r),
		trdapi.CommandPrepareOrder)

	res := w.turnOn(r, "task.judged")
	if res.ModelCalls != 2 || len(res.Refusals) != 1 || res.Refusals[0] != trdapi.CommandPrepareOrder+" "+kernel.CodeInvalidKnownValue ||
		res.Waiting != tskapi.WaitApproval || m.Remaining() != 0 {
		t.Fatalf("turn %+v", res)
	}
	told := requestText(m.Requests()[1])
	for _, want := range []string{kernel.CodeInvalidKnownValue, "/args/notify_party_ref", "role notify"} {
		if !strings.Contains(told, want) {
			t.Fatalf("the model's second request lacks %q:\n%s", want, told)
		}
	}
	var refused, waits int
	for _, c := range w.ledger("task.judged") {
		if c.Kind != executionevidence.KindTool {
			continue
		}
		switch c.State {
		case executionevidence.StateFailed:
			refused++
			if c.Code != kernel.CodeInvalidKnownValue || c.RefusalPath != "/args/notify_party_ref" || !strings.Contains(c.RefusalReason, "role notify") ||
				c.ActionID != "" {
				t.Fatalf("the refusal %+v", c)
			}
		case executionevidence.StateWaiting:
			waits++
		}
	}
	if refused != 1 || waits != 1 {
		t.Fatalf("refused %d, waits %d", refused, waits)
	}
	if actions, err := w.c.TSK.TaskActions(w.clerk, "task.judged"); err != nil || len(actions) != 0 {
		t.Fatalf("actions %+v %v", actions, err)
	}
	if grants, err := w.c.AI.ApprovalGrantsOf(w.clerk, "del.task.judged"); err != nil || len(grants) != 0 {
		t.Fatalf("grants %+v %v", grants, err)
	}
	v := w.taskView("task.judged")
	hash := strings.TrimPrefix(v.Wait.Items[1], "request_hash ")
	p := w.onlyProposal("task.judged")
	if p.ProposalID != v.Wait.Awaiting.ID || p.Status != proposal.StatusPending || p.RequestHash != hash || p.Command != trdapi.CommandPrepareOrder ||
		p.ObjectType == "" || p.ObjectID != "quote.1" || p.ObjectVersion != acc.BusinessRevision || p.Attempts != 1 || p.Sends != 0 ||
		p.AgentID != agentID || p.DelegationID != "del.task.judged" || p.InitiatorID != "clerk.ai" {
		t.Fatalf("proposal %+v", p)
	}
	var stored map[string]any
	w.must(json.Unmarshal(p.Arguments, &stored))
	if _, ok := stored["NotifyPartyRef"]; ok || stored["OrderID"] != "order.ai.1" || stored["AcceptanceID"] != acc.ObjectID || stored["IdempotencyKey"] != nil {
		t.Fatalf("stored request %s", p.Arguments)
	}
	read, err := w.c.AI.GetProposal(w.clerk, p.ProposalID)
	w.must(err)
	if read.ProposalID != p.ProposalID || string(read.Arguments) != string(p.Arguments) {
		t.Fatalf("the owner reads %+v", read)
	}
	if _, err = w.c.AI.GetProposal(w.agent, p.ProposalID); err != nil {
		t.Fatalf("the agent reads its own proposal: %v", err)
	}
	w.noOrder("order.ai.1")

	_, err = w.c.AI.IssueApprovalGrant(w.clerk, approvalgrantIssue(w, "grant.judged", "del.task.judged", trdapi.CommandPrepareOrder, hash, "quote.1",
		acc.BusinessRevision))
	w.must(err)
	m2 := model.NewScripted("scripted-q01-v1", done("订单草稿已准备。"))
	res = w.turnOn(w.runner(m2), "task.judged")
	if res.ModelCalls != 1 || res.ToolCalls != 1 || len(res.Receipts) != 1 || res.Receipts[0].Stage != execution.Verified || res.Receipts[0].Replayed ||
		len(res.Refusals) != 0 || m2.Remaining() != 0 {
		t.Fatalf("turn %+v", res)
	}
	if o := w.getOrder("order.ai.1"); o.PreparedBy.ID != agentID || o.Status != trdapi.OrderDraft {
		t.Fatalf("order %+v", o)
	}
	g, err := w.c.AI.GetApprovalGrant(w.clerk, "grant.judged")
	w.must(err)
	if g.Status != "used" || g.UsedKey != res.Receipts[0].ActionID {
		t.Fatalf("grant %+v", g)
	}
	// Between the wait and the model's answer there is the platform's tool call and nothing else.
	rows := w.ledger("task.judged")
	at := -1
	for i, c := range rows {
		if c.Kind == executionevidence.KindTool && c.State == executionevidence.StateWaiting {
			at = i
		}
	}
	if at < 0 || len(rows) != at+3 || rows[at+1].Kind != executionevidence.KindTool || rows[at+1].State != executionevidence.StateSucceeded ||
		rows[at+1].ActionID != res.Receipts[0].ActionID || rows[at+2].Kind == executionevidence.KindTool {
		t.Fatalf("ledger after the wait: %+v", rows[at+1:])
	}
	if told = requestText(m2.Requests()[0]); !strings.Contains(told, `"command":"order.prepare","state":"committed","object_id":"order.ai.1"`) {
		t.Fatalf("the next turn's facts lack the committed action:\n%s", told)
	}
	if p = w.onlyProposal("task.judged"); p.Status != proposal.StatusSent || p.ActionID != res.Receipts[0].ActionID || p.Sends != 1 ||
		p.GrantID != "grant.judged" || p.SettledAt.IsZero() {
		t.Fatalf("proposal %+v", p)
	}
}

// A request only the transaction can judge is refused after the grant: another order took the
// quote's conversion slot between the wait and the send (CONVERSION_SLOT_CONFLICT at
// /args/quote_id: the check before the wait saw it free). The platform sent the stored request, the grant is
// used, TRD's refusal is the proposal's outcome (refused, with the code and the path) and reaches the
// model's next turn in its facts; no order of the agent exists, no second send is made and no second
// grant is asked for the same call.
func TestProposalRefusedAfterTheGrantReachesTheNextTurn(t *testing.T) {
	p := newProposed(t, "task.race", "order.ai.1")
	_, err := p.c.TRD.PrepareOrder(p.author, p.prepareCmd(p.order, p.acc))
	p.must(err)
	p.grant("grant.race", time.Now().Add(time.Hour))
	m2 := model.NewScripted("scripted-q01-v1", done("订单已被他人准备。"))
	res := p.turnOn(p.runner(m2), "task.race")
	if len(res.Refusals) != 1 || !strings.HasPrefix(res.Refusals[0], trdapi.CommandPrepareOrder+" ") || res.ModelCalls != 1 || res.ToolCalls != 1 ||
		len(res.Receipts) != 0 || m2.Remaining() != 0 {
		t.Fatalf("turn %+v", res)
	}
	code := strings.TrimPrefix(res.Refusals[0], trdapi.CommandPrepareOrder+" ")
	ps := p.onlyProposal("task.race")
	if code != trdapi.CodeConversionSlotConflict || ps.Status != proposal.StatusRefused || ps.OutcomeCode != code || ps.OutcomePath != "/args/quote_id" ||
		ps.Sends != 1 || ps.GrantID != "grant.race" || ps.ActionID != "" || ps.SettledAt.IsZero() {
		t.Fatalf("proposal %+v, refusal %s", ps, code)
	}
	var waits, failed int
	for _, c := range p.ledger("task.race") {
		if c.Kind != executionevidence.KindTool {
			continue
		}
		switch c.State {
		case executionevidence.StateWaiting:
			waits++
		case executionevidence.StateFailed:
			failed++
			if c.Code != code || c.RefusalPath != ps.OutcomePath {
				t.Fatalf("the tool call %+v, proposal %+v", c, ps)
			}
		}
	}
	if waits != 1 || failed != 1 {
		t.Fatalf("waits %d, failed %d", waits, failed)
	}
	told := requestText(m2.Requests()[0])
	if !strings.Contains(told, code) || (ps.OutcomePath != "" && !strings.Contains(told, ps.OutcomePath)) {
		t.Fatalf("the next turn's facts lack %s at %q:\n%s", code, ps.OutcomePath, told)
	}
	if o := p.getOrder(p.order); o.PreparedBy.ID == agentID {
		t.Fatalf("the agent's request prepared the order: %+v", o)
	}
	if st := p.grantStatus("grant.race"); st != "used" {
		t.Fatalf("the grant is %s", st)
	}
	actions, err := p.c.TSK.TaskActions(p.clerk, "task.race")
	p.must(err)
	if len(actions) != 1 || actions[0].State == tskapi.ActionCommitted {
		t.Fatalf("actions %+v", actions)
	}
}

// An approver reads the request over HTTP: the owner (the delegation's initiator) and the agent that
// proposed it read it; a reader who does not see the initiator gets NOT_FOUND and one without ai.read
// PERMISSION_DENIED. It shows the arguments as the agent proposed them (no idempotency key), the hash,
// the object and version, and what became of it; every read is audited.
func TestProposalReadOverHTTP(t *testing.T) {
	p := newProposed(t, "task.read", "order.ai.1")
	every := append(append(slices.Clone(allPermissions), tskPermissions...), templatePermissions(t, "sales_clerk")...)
	p.provision(grantOf(p.trdWorld, "stranger.http", kernel.ActorHuman, "own", every...))
	p.provision(grantOf(p.trdWorld, "blind.http", kernel.ActorHuman, "company"))
	is := newIssuer(t)
	handler, err := p.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	p.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	as := func(id, kind string) *client {
		return &client{t: t, base: srv.URL, token: is.token(p.tenant, "company.A", id, kind)}
	}
	owner, bot, stranger, blind := as("clerk.ai", "human"), as(agentID, "ai"), as("stranger.http", "human"), as("blind.http", "human")
	route := "/api/brant/v1/ai/proposals/" + p.id

	status, out := owner.do("GET", route, nil)
	args, _ := out["arguments"].(map[string]any)
	if status != http.StatusOK || out["ok"] != true || out["proposal_id"] != p.id || out["task_id"] != "task.read" || out["command"] != trdapi.CommandPrepareOrder ||
		out["request_hash"] != p.hash || out["object_id"] != "quote.1" || out["object_version"] != float64(p.acc.BusinessRevision) ||
		out["status"] != proposal.StatusPending || out["delegation_id"] != "del.task.read" || out["agent_id"] != agentID ||
		out["initiator_id"] != "clerk.ai" || out["attempts"] != float64(1) || out["sends"] != float64(0) || out["action_id"] != nil ||
		args["OrderID"] != "order.ai.1" || args["QuoteID"] != "quote.1" || args["AcceptanceID"] != p.acc.ObjectID || args["IdempotencyKey"] != nil {
		t.Fatalf("the owner reads: %d %v", status, out)
	}
	if status, out = bot.do("GET", route, nil); status != http.StatusOK || out["proposal_id"] != p.id {
		t.Fatalf("the agent reads: %d %v", status, out)
	}
	status, out = stranger.do("GET", route, nil)
	refused(t, "a reader who does not see the initiator", status, out, kernel.CodeNotFound)
	status, out = blind.do("GET", route, nil)
	refused(t, "a reader without ai.read", status, out, kernel.CodePermissionDenied)
	status, out = owner.do("GET", "/api/brant/v1/ai/proposals/agp.unknown", nil)
	refused(t, "an unknown proposal", status, out, kernel.CodeNotFound)
	status, out = (&client{t: t, base: srv.URL}).do("GET", route, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("no token: %d %v", status, out)
	}

	p.grant("grant.read", time.Now().Add(time.Hour))
	res := p.turnOn(p.runner(model.NewScripted("scripted-q01-v1", done("订单草稿已准备。"))), "task.read")
	if len(res.Receipts) != 1 {
		t.Fatalf("turn %+v", res)
	}
	status, out = owner.do("GET", route, nil)
	if status != http.StatusOK || out["status"] != proposal.StatusSent || out["action_id"] != res.Receipts[0].ActionID || out["grant_id"] != "grant.read" ||
		out["sends"] != float64(1) || out["settled_at"] == nil {
		t.Fatalf("the owner reads after the send: %d %v", status, out)
	}
	audit, err := p.c.AI.Audit(p.provision(grantOf(p.trdWorld, "auditor.ai", kernel.ActorHuman, "company", "ai.audit")))
	p.must(err)
	var reads int
	for _, e := range audit {
		if e.Command == proposal.CommandGet && e.Outcome == "read" && e.ObjectID == p.id {
			reads++
		}
	}
	if reads < 2 {
		t.Fatalf("audited reads of the proposal: %d", reads)
	}
}

// AN04 through the runner: the delegation is revoked while the task waits, then a grant is issued on
// it. The grant needs the delegation, so nothing is sent: the task is paused for its owner with
// PERMISSION_DENIED, no order exists, no action is recorded, the grant is not used, and the
// model is not asked.
func TestProposalIsNotSentAfterTheDelegationWasRevoked(t *testing.T) {
	p := newProposed(t, "task.revoked", "order.ai.1")
	p.grant("grant.revoked", time.Now().Add(time.Hour))
	_, err := p.c.AI.RevokeDelegation(p.clerk, policy.RevokeDelegation{IdempotencyKey: p.key(), DelegationID: "del.task.revoked", ExpectedVersion: 1,
		Reason: "发起人调岗"})
	p.must(err)
	m2 := model.NewScripted("scripted-q01-v1", done("不应被调用"))
	res := p.turnOn(p.runner(m2), "task.revoked")
	v := p.taskView("task.revoked")
	if res.Paused != tskapi.PauseConflict || res.ModelCalls != 0 || m2.Remaining() != 1 || !strings.Contains(v.Pause.Reason, "PERMISSION_DENIED") ||
		v.Lifecycle != tskapi.StatePaused || v.Pause.ResponsibleID != "clerk.ai" {
		t.Fatalf("turn %+v, task %s %+v", res, v.Lifecycle, v.Pause)
	}
	p.noOrder("order.ai.1")
	if actions, err := p.c.TSK.TaskActions(p.clerk, "task.revoked"); err != nil || len(actions) != 0 {
		t.Fatalf("actions %+v %v", actions, err)
	}
	if st := p.grantStatus("grant.revoked"); st != "issued" {
		t.Fatalf("the grant is %s", st)
	}
	if ps := p.onlyProposal("task.revoked"); ps.Status == proposal.StatusSent || ps.Sends != 0 {
		t.Fatalf("proposal %+v", ps)
	}
}

// The grant expires. A grant that has expired is not one: the task keeps waiting and nothing is sent.
// A runner whose clock is behind sees it open, resumes the task and sends; the admission re-checks the
// expiry at the grant's own ledger and refuses with GRANT_EXPIRED: nothing is sent, the grant is not
// used and no order exists. The proposal is void with that code and the refusal is in the facts of
// the model's turn; when the model asks again the same request is the same proposal, pending again
// with its next attempt (this runner, which still sees the grant as open, is refused the same way).
func TestProposalWithAnExpiredGrantIsNotSent(t *testing.T) {
	p := newProposed(t, "task.expired", "order.ai.1")
	p.grant("grant.old", time.Now().Add(2*time.Second))
	time.Sleep(2500 * time.Millisecond)
	m1 := model.NewScripted("scripted-q01-v1", done("不应被调用"))
	if res := p.turnOn(p.runner(m1), "task.expired"); res.ModelCalls != 0 || res.Skipped != "still waiting for the approval grant" {
		t.Fatalf("expired: %+v", res)
	}
	if ps := p.onlyProposal("task.expired"); ps.Status != proposal.StatusPending || ps.Sends != 0 {
		t.Fatalf("proposal %+v", ps)
	}
	p.noOrder("order.ai.1")

	behind := model.NewScripted("scripted-q01-v1", calls(tc("c2", trdapi.CommandPrepareOrder, p.prepareArgs(p.order, p.acc))), done("授权已过期。"))
	r2 := p.runner(behind)
	r2.Clock = skewClock(-time.Hour)
	res := p.turnOn(r2, "task.expired")
	refusal := trdapi.CommandPrepareOrder + " " + codes.GrantExpired
	if len(res.Refusals) != 2 || res.Refusals[0] != refusal || res.Refusals[1] != refusal || res.ModelCalls != 2 || len(res.Receipts) != 0 ||
		behind.Remaining() != 0 {
		t.Fatalf("turn with a clock behind %+v", res)
	}
	p.noOrder("order.ai.1")
	if st := p.grantStatus("grant.old"); st != "issued" {
		t.Fatalf("the expired grant is %s", st)
	}
	actions, err := p.c.TSK.TaskActions(p.clerk, "task.expired")
	p.must(err)
	for _, a := range actions {
		if a.State == tskapi.ActionCommitted {
			t.Fatalf("actions %+v", actions)
		}
	}
	if ps := p.onlyProposal("task.expired"); ps.Status != proposal.StatusVoid || ps.OutcomeCode != codes.GrantExpired || ps.Attempts != 2 ||
		ps.ProposalID != p.id || ps.ActionID != "" {
		t.Fatalf("proposal %+v", ps)
	}
	if told := requestText(behind.Requests()[0]); !strings.Contains(told, codes.GrantExpired) {
		t.Fatalf("the facts lack the refusal:\n%s", told)
	}
}

// AN06 and two runners: after the grant two runners take the waiting task at once. The proposal's
// claim is a row lock and a state check, so one of them sends: one order, one committed action, one
// grant used, one send recorded on the proposal and one succeeded tool call. The other finds the
// proposal claimed or the intent committed and sends nothing, or loses the task's resume to the first.
func TestTwoRunnersResumingAtOnceSendOnce(t *testing.T) {
	p := newProposed(t, "task.twice", "order.ai.1")
	p.grant("grant.twice", time.Now().Add(time.Hour))
	runners := []*execution.Runner{p.runner(model.NewScripted("scripted-q01-v1", done("订单草稿已准备。"))),
		p.runner(model.NewScripted("scripted-q01-v1", done("订单草稿已准备。")))}
	errs := make([]error, len(runners))
	results := make([]execution.TurnResult, len(runners))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, r := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = r.Turn(context.Background(), p.scope("company.A"), agentID, "task.twice")
		}()
	}
	close(start)
	wg.Wait()
	if errs[0] != nil && errs[1] != nil {
		t.Fatalf("both runners failed: %v, %v", errs[0], errs[1])
	}
	t.Logf("runner errors: %v, %v; results %+v, %+v", errs[0], errs[1], results[0], results[1])
	o, err := p.c.TRD.GetOrder(p.clerk, "order.ai.1")
	if err != nil {
		t.Fatalf("no order: %v; errors %v, %v; results %+v, %+v; proposal %+v", err, errs[0], errs[1], results[0], results[1], p.onlyProposal("task.twice"))
	}
	if o.PreparedBy.ID != agentID || o.RowVersion != 1 {
		t.Fatalf("order %+v", o)
	}
	actions, err := p.c.TSK.TaskActions(p.clerk, "task.twice")
	p.must(err)
	if len(actions) != 1 || actions[0].State != tskapi.ActionCommitted {
		t.Fatalf("actions %+v", actions)
	}
	if st := p.grantStatus("grant.twice"); st != "used" {
		t.Fatalf("grant %s", st)
	}
	var succeeded int
	for _, c := range p.ledger("task.twice") {
		if c.Kind == executionevidence.KindTool && c.State == executionevidence.StateSucceeded {
			succeeded++
		}
	}
	if ps := p.onlyProposal("task.twice"); ps.Status != proposal.StatusSent || ps.Sends != 1 || ps.ActionID != actions[0].ActionID || succeeded != 1 {
		t.Fatalf("proposal %+v, succeeded tool calls %d", ps, succeeded)
	}
}

// A runner that finds the proposal of its task claimed by another runner, within the claim's lease,
// neither sends nor asks the model nor ends the turn of the task under it (a task that left running
// refuses the send in flight; before this test, the runner that lost the claim finished the task and
// the runner that held it was refused with INVALID_STATE, in two of forty races). It leaves the task
// as it is; once the claim goes stale the next turn takes the proposal over and sends it.
func TestRunnerLeavesAProposalAnotherRunnerIsSending(t *testing.T) {
	p := newProposed(t, "task.held", "order.ai.1")
	p.grant("grant.held", time.Now().Add(time.Hour))
	db, closeDB := p.helperDB()
	defer closeDB()
	_, claimed, err := proposal.Store{DB: db, Clock: skewClock(0)}.Claim(p.agent, p.id, "grant.held")
	p.must(err)
	if !claimed {
		t.Fatal("the proposal was not claimed")
	}
	m := model.NewScripted("scripted-q01-v1", done("不应被调用"))
	res := p.turnOn(p.runner(m), "task.held")
	if res.ModelCalls != 0 || res.ToolCalls != 0 || len(res.Receipts) != 0 || len(res.Refusals) != 0 || m.Remaining() != 1 ||
		!strings.Contains(res.Skipped, "another runner is sending") || res.Verified {
		t.Fatalf("turn %+v", res)
	}
	if v := p.taskView("task.held"); v.Lifecycle != tskapi.StateRunning {
		t.Fatalf("the task is %s", v.Lifecycle)
	}
	p.noOrder("order.ai.1")
	if ps := p.onlyProposal("task.held"); ps.Status != proposal.StatusSending || ps.Sends != 1 || ps.Attempts != 1 {
		t.Fatalf("proposal %+v", ps)
	}
	if st := p.grantStatus("grant.held"); st != "issued" {
		t.Fatalf("the grant is %s", st)
	}

	m2 := model.NewScripted("scripted-q01-v1", done("订单草稿已准备。"))
	r2 := p.runner(m2)
	r2.Clock = skewClock(proposal.ClaimLease + time.Minute)
	res = p.turnOn(r2, "task.held")
	if res.ModelCalls != 1 || res.ToolCalls != 1 || len(res.Receipts) != 1 || res.Receipts[0].Replayed || m2.Remaining() != 0 {
		t.Fatalf("turn after the claim went stale %+v", res)
	}
	if o := p.getOrder("order.ai.1"); o.PreparedBy.ID != agentID || o.RowVersion != 1 {
		t.Fatalf("order %+v", o)
	}
	if ps := p.onlyProposal("task.held"); ps.Status != proposal.StatusSent || ps.Sends != 2 || ps.ActionID != res.Receipts[0].ActionID {
		t.Fatalf("proposal %+v", ps)
	}
	if st := p.grantStatus("grant.held"); st != "used" {
		t.Fatalf("the grant is %s", st)
	}
}

// order.review.request is judged the same way: the owning context refuses a stale expected version
// (VERSION_CONFLICT at /args/expected_version) in the same turn, before a person is asked, and the
// corrected request waits once, is stored with the order's version, and after the grant the platform
// opens the review with no model call between the grant and the commit.
func TestReviewRequestIsJudgedBeforeAPersonIsAskedAndSentAfterTheGrant(t *testing.T) {
	w := newAgentWorld(t, true)
	_, order := w.prepared("quote.1")
	version := w.orderVersion(order.ObjectID)
	request := func(expected int64) map[string]any {
		return map[string]any{"OrderID": order.ObjectID, "ExpectedVersion": expected, "ReviewID": "review.ai.1"}
	}
	m := model.NewScripted("scripted-q01-v1", calls(tc("c1", trdapi.CommandRequestOrderReview, request(version+3))),
		calls(tc("c2", trdapi.CommandRequestOrderReview, request(version))))
	r := w.runner(m)
	w.aiTask("task.review", tskapi.GoalOrderActive, tskapi.Target{QuoteID: "quote.1", CustomerID: "customer.1"}, budgetOf(10, 2), bundleRef(r),
		trdapi.CommandRequestOrderReview)
	res := w.turnOn(r, "task.review")
	if res.ModelCalls != 2 || len(res.Refusals) != 1 || res.Refusals[0] != trdapi.CommandRequestOrderReview+" "+kernel.CodeVersionConflict ||
		res.Waiting != tskapi.WaitApproval {
		t.Fatalf("turn %+v", res)
	}
	var refused int
	for _, c := range w.ledger("task.review") {
		if c.Kind == executionevidence.KindTool && c.State == executionevidence.StateFailed {
			refused++
			if c.Code != kernel.CodeVersionConflict || c.RefusalPath != "/args/expected_version" {
				t.Fatalf("the refusal %+v", c)
			}
		}
	}
	if actions, err := w.c.TSK.TaskActions(w.clerk, "task.review"); err != nil || refused != 1 || len(actions) != 0 {
		t.Fatalf("refused %d, actions %+v %v", refused, actions, err)
	}
	if w.orderVersion(order.ObjectID) != version {
		t.Fatalf("the order moved before the grant")
	}
	v := w.taskView("task.review")
	p := w.onlyProposal("task.review")
	hash := strings.TrimPrefix(v.Wait.Items[1], "request_hash ")
	if p.Command != trdapi.CommandRequestOrderReview || p.Status != proposal.StatusPending || p.ObjectID != order.ObjectID || p.ObjectVersion != version ||
		p.RequestHash != hash || p.ProposalID != v.Wait.Awaiting.ID {
		t.Fatalf("proposal %+v", p)
	}
	_, err := w.c.AI.IssueApprovalGrant(w.clerk, approvalgrantIssue(w, "grant.review", "del.task.review", trdapi.CommandRequestOrderReview, hash,
		order.ObjectID, version))
	w.must(err)
	m2 := model.NewScripted("scripted-q01-v1", done("审核已发起。"))
	res = w.turnOn(w.runner(m2), "task.review")
	if res.ModelCalls != 1 || res.ToolCalls != 1 || len(res.Receipts) != 1 || res.Receipts[0].Replayed || len(res.Refusals) != 0 || m2.Remaining() != 0 {
		t.Fatalf("turn %+v", res)
	}
	if w.orderVersion(order.ObjectID) != version+1 {
		t.Fatalf("the review was not opened")
	}
	if st := w.grantStatus("grant.review"); st != "used" {
		t.Fatalf("the grant is %s", st)
	}
	if p = w.onlyProposal("task.review"); p.Status != proposal.StatusSent || p.ActionID != res.Receipts[0].ActionID || p.Sends != 1 {
		t.Fatalf("proposal %+v", p)
	}
}
