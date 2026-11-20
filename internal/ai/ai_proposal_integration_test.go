//go:build integration

package ai_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/ai/proposal"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The proposal store against the migrated PostgreSQL 16 database (S4 G7b; migration ai/000005): the
// state machine the guard trigger and the store keep, the claim of two runners, the column-limited
// grant, FORCE RLS on the scope, and the audited read (ai.proposal.get; ai.read).

var proposalArgs = json.RawMessage(`{"AcceptanceID":"acc.1","BusinessHash":"` + strings.Repeat("a", 64) +
	`","BusinessRevision":2,"OrderID":"order.1","QuoteID":"quote.1"}`)

func (f *fixture) proposalStore() proposal.Store { return proposal.Store{DB: f.db, Clock: f.clock} }

// newProposal is the request of delegation d's task that proposal id stands for.
func (f *fixture) newProposal(d policy.Delegation, id string) proposal.New {
	return proposal.New{ProposalID: id, TaskID: d.TaskID, Delegation: d, Command: cmdPrepare, RequestHash: strings.Repeat("a", 64),
		ObjectType: "prb.quote", ObjectID: "quote.1", ObjectVersion: 2, Arguments: proposalArgs}
}

func (f *fixture) delegation(id string) policy.Delegation {
	f.t.Helper()
	d, err := f.layer.GetDelegation(f.as(clerk), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

// A proposal is made once, kept as it was proposed, claimed by one runner at a time, settled once,
// and a refused or void one that the agent proposes again is pending again with the next attempt and
// no sends. A claim that went stale (a runner that went away) is taken over; a request is never
// stored with the idempotency key, past its length, or under another task's delegation.
func TestProposalStateMachine(t *testing.T) {
	f := newFixture(t)
	f.delegate("del.p1", agent.ID, "task.p1", "submit", cmdPrepare)
	d := f.delegation("del.p1")
	s, ctx := f.proposalStore(), f.as(agent)

	n := f.newProposal(d, "agp.1")
	p, made, err := s.Propose(ctx, n)
	if err != nil || !made || p.Status != proposal.StatusPending || p.Attempts != 1 || p.Sends != 0 || p.RowVersion != 1 || p.AgentID != agent.ID ||
		p.InitiatorID != clerk.ID || p.DelegationID != "del.p1" || p.ObjectVersion != 2 || !p.Open() || !p.SettledAt.IsZero() {
		t.Fatalf("propose: %+v made %v, %v", p, made, err)
	}
	var stored, sent map[string]any
	if json.Unmarshal(p.Arguments, &stored) != nil || json.Unmarshal(proposalArgs, &sent) != nil || stored["OrderID"] != sent["OrderID"] ||
		len(stored) != len(sent) {
		t.Fatalf("stored %s", p.Arguments)
	}
	again, made, err := s.Propose(ctx, n)
	if err != nil || made || again.RowVersion != 1 || again.ProposalID != "agp.1" {
		t.Fatalf("the same request again: %+v made %v, %v", again, made, err)
	}
	other := n
	other.RequestHash = strings.Repeat("b", 64)
	if _, _, err = s.Propose(ctx, other); !errors.Is(err, proposal.ErrInvalid) {
		t.Fatalf("another request under the same ID: %v", err)
	}
	other = f.newProposal(d, "agp.2")
	other.Arguments = json.RawMessage(`{"OrderID":"order.1","IdempotencyKey":"k"}`)
	if _, _, err = s.Propose(ctx, other); !errors.Is(err, proposal.ErrInvalid) {
		t.Fatalf("a request with a key: %v", err)
	}
	other.Arguments = json.RawMessage(`{"OrderID":"` + strings.Repeat("x", proposal.MaxArguments) + `"}`)
	if _, _, err = s.Propose(ctx, other); !errors.Is(err, proposal.ErrTooLong) {
		t.Fatalf("a request too long: %v", err)
	}
	other = f.newProposal(d, "agp.2")
	other.TaskID = "task.other"
	if _, _, err = s.Propose(ctx, other); !errors.Is(err, proposal.ErrInvalid) {
		t.Fatalf("another task's request: %v", err)
	}

	// One runner claims; the other finds the claim held, until it goes stale.
	claimed, ok, err := s.Claim(ctx, "agp.1", "grant.1")
	if err != nil || !ok || claimed.Status != proposal.StatusSending || claimed.Sends != 1 || claimed.GrantID != "grant.1" || claimed.ClaimedAt.IsZero() {
		t.Fatalf("claim: %+v %v %v", claimed, ok, err)
	}
	if held, ok, err := s.Claim(ctx, "agp.1", "grant.1"); err != nil || ok || held.Status != proposal.StatusSending || held.Sends != 1 {
		t.Fatalf("a second claim: %+v %v %v", held, ok, err)
	}
	f.clock.advance(proposal.ClaimLease)
	taken, ok, err := s.Claim(ctx, "agp.1", "grant.2")
	if err != nil || !ok || taken.Sends != 2 || taken.GrantID != "grant.2" || !taken.ClaimedAt.After(claimed.ClaimedAt) {
		t.Fatalf("a stale claim taken over: %+v %v %v", taken, ok, err)
	}
	// The target did not answer: the proposal is pending again and sent with the same key.
	if err = s.Release(ctx, "agp.1"); err != nil {
		t.Fatal(err)
	}
	if err = s.Release(ctx, "agp.1"); !errors.Is(err, proposal.ErrSettled) {
		t.Fatalf("a release of a proposal nobody holds: %v", err)
	}
	if p = *mustGet(t, s, ctx, "agp.1"); p.Status != proposal.StatusPending || p.Sends != 2 || p.GrantID != "" || !p.ClaimedAt.IsZero() {
		t.Fatalf("released: %+v", p)
	}
	if _, ok, err = s.Claim(ctx, "agp.1", "grant.1"); err != nil || !ok {
		t.Fatalf("claim after the release: %v %v", ok, err)
	}
	for _, bad := range []proposal.Outcome{{Status: proposal.StatusSent}, {Status: proposal.StatusSent, ActionID: "act.1", Code: "X"}, {Status: proposal.StatusRefused},
		{Status: proposal.StatusVoid}, {Status: proposal.StatusPending}} {
		if err = s.Settle(ctx, "agp.1", bad); !errors.Is(err, proposal.ErrInvalid) {
			t.Fatalf("outcome %+v: %v", bad, err)
		}
	}
	if err = s.Settle(ctx, "agp.1", proposal.Outcome{Status: proposal.StatusRefused, Code: "CONVERSION_SLOT_CONFLICT", Path: "/args/quote_id"}); err != nil {
		t.Fatal(err)
	}
	if p = *mustGet(t, s, ctx, "agp.1"); p.Status != proposal.StatusRefused || p.OutcomeCode != "CONVERSION_SLOT_CONFLICT" || p.OutcomePath != "/args/quote_id" ||
		p.SettledAt.IsZero() || p.Open() {
		t.Fatalf("refused: %+v", p)
	}
	if err = s.Settle(ctx, "agp.1", proposal.Outcome{Status: proposal.StatusSent, ActionID: "act.1"}); !errors.Is(err, proposal.ErrSettled) {
		t.Fatalf("a second settlement: %v", err)
	}
	if _, ok, err = s.Claim(ctx, "agp.1", "grant.1"); err != nil || ok {
		t.Fatalf("a refused proposal is not claimed: %v %v", ok, err)
	}

	// Proposed again, and accepted by the owning context: the next attempt, pending, no sends.
	again, made, err = s.Propose(ctx, n)
	if err != nil || !made || again.Status != proposal.StatusPending || again.Attempts != 2 || again.Sends != 0 || again.OutcomeCode != "" ||
		again.OutcomePath != "" || !again.SettledAt.IsZero() || again.RowVersion != 7 || again.DelegationID != d.DelegationID {
		t.Fatalf("re-armed: %+v made %v, %v", again, made, err)
	}
	if err = s.Settle(ctx, "agp.1", proposal.Outcome{Status: proposal.StatusRefused, Code: "X_REFUSED"}); !errors.Is(err, proposal.ErrSettled) {
		t.Fatalf("a pending proposal is not refused without a claim: %v", err)
	}
	// The target's ledger held the committed request before this runner claimed it.
	if err = s.Settle(ctx, "agp.1", proposal.Outcome{Status: proposal.StatusSent, ActionID: "act.1"}); err != nil {
		t.Fatal(err)
	}
	if p = *mustGet(t, s, ctx, "agp.1"); p.Status != proposal.StatusSent || p.ActionID != "act.1" || p.OutcomeCode != "" {
		t.Fatalf("sent: %+v", p)
	}
	if _, _, err = s.Propose(ctx, n); err != nil {
		t.Fatal(err)
	}
	if p = *mustGet(t, s, ctx, "agp.1"); p.Status != proposal.StatusSent || p.Attempts != 2 {
		t.Fatalf("a sent proposal proposed again is left as it is: %+v", p)
	}

	// A proposal the admission refused is void, and proposed again pending.
	if _, _, err = s.Propose(ctx, f.newProposal(d, "agp.3")); err != nil {
		t.Fatal(err)
	}
	if err = s.Settle(ctx, "agp.3", proposal.Outcome{Status: proposal.StatusVoid, Code: "PERMISSION_DENIED", Path: "/delegation"}); err != nil {
		t.Fatal(err)
	}
	if p, made, err = s.Propose(ctx, f.newProposal(d, "agp.3")); err != nil || !made || p.Status != proposal.StatusPending || p.Attempts != 2 {
		t.Fatalf("void then proposed again: %+v %v %v", p, made, err)
	}

	ps, err := s.OfTask(ctx, "task.p1")
	if err != nil || len(ps) != 2 || ps[0].ProposalID != "agp.1" || ps[1].ProposalID != "agp.3" {
		t.Fatalf("of the task: %+v %v", ps, err)
	}
	if ps, err = s.OfTask(ctx, "task.none"); err != nil || len(ps) != 0 {
		t.Fatalf("of another task: %+v %v", ps, err)
	}
	if _, err = s.Get(context.Background(), "agp.1"); err == nil {
		t.Fatal("no principal")
	}
	if p := mustGet(t, s, ctx, "agp.none"); p != nil {
		t.Fatalf("an unknown proposal: %+v", p)
	}
	if err = s.Release(ctx, "agp.none"); !errors.Is(err, proposal.ErrNotFound) {
		t.Fatalf("release of an unknown proposal: %v", err)
	}
}

func mustGet(t *testing.T, s proposal.Store, ctx context.Context, id string) *proposal.Proposal {
	t.Helper()
	p, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Two runners claim one proposal at once: the claim is a row lock and a state check, so exactly one
// of them holds it, and the sends counter says so.
func TestProposalClaimedByOneOfTwoRunners(t *testing.T) {
	f := newFixture(t)
	f.delegate("del.p2", agent.ID, "task.p2", "submit", cmdPrepare)
	s, ctx := f.proposalStore(), f.as(agent)
	if _, _, err := s.Propose(ctx, f.newProposal(f.delegation("del.p2"), "agp.1")); err != nil {
		t.Fatal(err)
	}
	var won atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok, err := s.Claim(ctx, "agp.1", "grant.1"); err != nil {
				t.Error(err)
			} else if ok {
				won.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if p := mustGet(t, s, ctx, "agp.1"); won.Load() != 1 || p.Sends != 1 || p.Status != proposal.StatusSending {
		t.Fatalf("claims won %d: %+v", won.Load(), p)
	}
}

// What the database keeps of a proposal whatever the code does: the application role may change only
// the claim and the outcome (a column-limited grant), never delete, and the guard allows only the
// listed steps at the next row version; the request, its object and its arguments never change.
func TestProposalGuardAndGrants(t *testing.T) {
	f := newFixture(t)
	f.delegate("del.p3", agent.ID, "task.p3", "submit", cmdPrepare)
	s, ctx := f.proposalStore(), f.as(agent)
	if _, _, err := s.Propose(ctx, f.newProposal(f.delegation("del.p3"), "agp.1")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Claim(ctx, "agp.1", "grant.1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(ctx, "agp.1", proposal.Outcome{Status: proposal.StatusSent, ActionID: "act.1"}); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) error {
		return policy.WithinScope(context.Background(), f.db, f.scope, func(ctx context.Context, e bentopg.Executor) error {
			_, err := e.Exec(ctx, sql, args...)
			return err
		})
	}
	for name, sql := range map[string]string{
		"the arguments change": `UPDATE brant_ai.proposal SET arguments = '{"OrderID":"order.9"}'::jsonb, row_version = row_version + 1 WHERE proposal_id = 'agp.1'`,
		"the request changes":  `UPDATE brant_ai.proposal SET request_hash = repeat('c', 64), row_version = row_version + 1 WHERE proposal_id = 'agp.1'`,
		"the object changes":   `UPDATE brant_ai.proposal SET object_id = 'quote.9', row_version = row_version + 1 WHERE proposal_id = 'agp.1'`,
		"a delete":             `DELETE FROM brant_ai.proposal WHERE proposal_id = 'agp.1'`,
	} {
		if err := exec(sql); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %v, want permission denied", name, err)
		}
	}
	for name, sql := range map[string]string{
		"a sent proposal pending again":     `UPDATE brant_ai.proposal SET status = 'pending', row_version = row_version + 1, settled_at = NULL, action_id = NULL WHERE proposal_id = 'agp.1'`,
		"a step without the next version":   `UPDATE brant_ai.proposal SET status = 'void', outcome_code = 'X_Y' WHERE proposal_id = 'agp.1'`,
		"a sent proposal refused":           `UPDATE brant_ai.proposal SET status = 'refused', row_version = row_version + 1, outcome_code = 'X_Y', action_id = NULL WHERE proposal_id = 'agp.1'`,
		"a sent proposal claimed again":     `UPDATE brant_ai.proposal SET status = 'sending', row_version = row_version + 1, sends = sends + 1, claimed_at = now(), grant_id = 'grant.2', settled_at = NULL, action_id = NULL WHERE proposal_id = 'agp.1'`,
		"a sent proposal with another task": `UPDATE brant_ai.proposal SET task_id = 'task.other', row_version = row_version + 1 WHERE proposal_id = 'agp.1'`,
	} {
		if err := exec(sql); err == nil {
			t.Errorf("%s was allowed", name)
		}
	}
	insert := func(status string, version, attempts, sends int) error {
		return exec(`INSERT INTO brant_ai.proposal (tenant_id, company_id, proposal_id, task_id, delegation_id, agent_id, initiator_id, command,
			request_hash, object_type, object_id, object_version, arguments, status, row_version, attempts, sends, proposed_at)
			VALUES ($1, $2, 'agp.ins', 'task.p3', 'del.p3', $3, $4, 'probe.prepare', repeat('a', 64), 'prb.quote', 'quote.1', 2, '{}'::jsonb,
			$5, $6, $7, $8, now())`, string(f.scope.Tenant), string(f.scope.Company), agent.ID, clerk.ID, status, version, attempts, sends)
	}
	for name, args := range map[string][4]any{"made sent": {"sent", 1, 1, 0}, "made at version 2": {"pending", 2, 1, 0}, "made at attempt 2": {"pending", 1, 2, 0},
		"made already sent once": {"pending", 1, 1, 1}} {
		if err := insert(args[0].(string), args[1].(int), args[2].(int), args[3].(int)); err == nil {
			t.Errorf("a proposal %s was stored", name)
		}
	}
	if err := insert("pending", 1, 1, 0); err != nil {
		t.Fatalf("a well-formed proposal: %v", err)
	}
}

// A proposal belongs to its scope: another tenant's runner does not see it, FORCE RLS shows a
// connection with no scope nothing, and the read of ai.proposal.get is the delegation's initiator's
// view (the proposing agent, or a reader whose data scope covers the initiator), audited, and
// ai.read's.
func TestProposalReadIsScopedAndAudited(t *testing.T) {
	f := newFixture(t)
	f.delegate("del.p4", agent.ID, "task.p4", "submit", cmdPrepare)
	s, ctx := f.proposalStore(), f.as(agent)
	if _, _, err := s.Propose(ctx, f.newProposal(f.delegation("del.p4"), "agp.1")); err != nil {
		t.Fatal(err)
	}

	read := func(who context.Context) (proposal.Proposal, error) { return f.layer.GetProposal(who, "agp.1") }
	for name, who := range map[string]kernel.ActorRef{"the initiator": clerk, "the proposing agent": agent, "a reader of the company": approver} {
		if p, err := read(f.as(who)); err != nil || p.ProposalID != "agp.1" || p.Command != cmdPrepare || string(p.Arguments) == "" {
			t.Errorf("%s: %+v %v", name, p, err)
		}
	}
	for name, who := range map[string]kernel.ActorRef{"another person of the own scope": clerk2, "another agent": agent2, "a reader of their own records": viewer} {
		if _, err := read(f.as(who)); codeOf(err) != kernel.CodeNotFound || pathOf(err) != "/id" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := read(f.as(auditor)); err != nil {
		t.Errorf("the auditor reads with company scope and ai.read: %v", err)
	}
	if _, err := f.layer.GetProposal(f.as(clerk), "agp.none"); codeOf(err) != kernel.CodeNotFound {
		t.Errorf("an unknown proposal: %v", err)
	}
	nobody := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "nobody.1"}
	f.authority(func(ctx context.Context, exec bentopg.Executor, g authorization.Grants) error {
		_, err := g.Put(ctx, exec, f.scope, nobody, []string{cmdSave}, start)
		return err
	})
	if _, err := read(f.as(nobody)); codeOf(err) != kernel.CodePermissionDenied {
		t.Errorf("without ai.read: %v", err)
	}
	if got := f.outcomes(proposal.CommandGet); len(got) < 6 || !strings.Contains(strings.Join(got, ","), "read ") || !strings.Contains(strings.Join(got, ","), "rejected PERMISSION_DENIED") {
		t.Errorf("audited %v", got)
	}

	// Another tenant's runner has no such proposal; a connection with no scope sees no row at all.
	other := newFixture(t)
	other.delegate("del.p4", agent.ID, "task.p4", "submit", cmdPrepare)
	if ps, err := other.proposalStore().OfTask(other.as(agent), "task.p4"); err != nil || len(ps) != 0 {
		t.Errorf("another tenant: %+v %v", ps, err)
	}
	if p := mustGet(t, other.proposalStore(), other.as(agent), "agp.1"); p != nil {
		t.Errorf("another tenant's proposal: %+v", p)
	}
	var n int
	if err := pgtest.AppPool(t).QueryRow(context.Background(), `SELECT count(*) FROM brant_ai.proposal`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a connection with no scope sees %d rows, %v", n, err)
	}
}
