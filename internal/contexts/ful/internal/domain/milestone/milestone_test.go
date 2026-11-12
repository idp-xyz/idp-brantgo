package milestone

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	now    = time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	human  = Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "merch.1"}, Epoch: 1, At: now}
	agent  = Act{By: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}, Epoch: 1, At: now}
	order  = Subject{Kind: SubjectOrder, ID: "order.1"}
	active = SubjectFact{Known: true}
)

func date(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s)
	return d
}

func nodes() []Node {
	return []Node{
		{ID: "release", Type: "customs_release", PlannedOn: date("2026-11-10"), Predecessors: []string{}},
		{ID: "departure", Type: "actual_departure", PlannedOn: date("2026-11-11"), Predecessors: []string{"release"}},
		{ID: "delivery", Type: "delivery", PlannedOn: date("2026-12-20"), Predecessors: []string{"departure"}},
		{ID: "acceptance", Type: "customer_acceptance", PlannedOn: date("2026-12-30"), Predecessors: []string{"delivery"}},
	}
}

func plan(t *testing.T) *Plan {
	t.Helper()
	p, err := New(Request{ID: "plan.1", Subject: order, Nodes: nodes(), RequestHash: "h"}, active, false, agent, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func codeAt(err error) string {
	rej, ok := kernel.AsRejection(err)
	if !ok {
		return ""
	}
	return rej.Code + " " + rej.Fields[0].Path
}

// New refuses, in order: an unseen subject, an order that does not serve downstream work, a
// cancelled shipment, a subject another plan follows, predecessors outside the plan or in a cycle.
func TestNewPlanRefusals(t *testing.T) {
	ship := Subject{Kind: SubjectShipment, ID: "shipment.1"}
	cyclic := nodes()
	cyclic[0].Predecessors = []string{"acceptance"}
	dangling := nodes()
	dangling[1].Predecessors = []string{"nowhere"}
	self := nodes()
	self[1].Predecessors = []string{"departure"}
	for _, c := range []struct {
		want    string
		subject Subject
		fact    SubjectFact
		taken   bool
		nodes   []Node
	}{
		{"REFERENCE_INVALID /args/subject/id", order, SubjectFact{}, false, nodes()},
		{"ORDER_NOT_ACTIVE /args/subject/id", order, SubjectFact{Known: true, Code: CodeOrderNotActive}, false, nodes()},
		{"REFERENCE_INVALID /args/subject/id", order, SubjectFact{Known: true, Code: "SOMETHING_ELSE"}, false, nodes()},
		{"INVALID_STATE /args/subject/id", ship, SubjectFact{Known: true, Status: "cancelled"}, false, nodes()},
		{"BUSINESS_KEY_CONFLICT /args/subject", order, active, true, nodes()},
		{"INVALID_KNOWN_VALUE /args/nodes/0/predecessors", order, active, false, cyclic},
		{"INVALID_KNOWN_VALUE /args/nodes/1/predecessors/0", order, active, false, dangling},
	} {
		_, err := New(Request{ID: "plan.1", Subject: c.subject, Nodes: c.nodes}, c.fact, c.taken, human, "")
		if got := codeAt(err); got != c.want {
			t.Errorf("%s: %s", c.want, got)
		}
	}
	if _, err := New(Request{ID: "plan.1", Subject: order, Nodes: self}, active, false, human, ""); codeAt(err) != "INVALID_KNOWN_VALUE /args/nodes/1/predecessors/0" {
		t.Errorf("a node waiting for itself: %s", codeAt(err))
	}
	if p, err := New(Request{ID: "plan.1", Subject: ship, Nodes: nodes()}, SubjectFact{Known: true, Status: "confirmed"}, false, human, "team.1"); err != nil ||
		p.RowVersion != 1 || p.Revision != 1 || p.OwnerActor != "merch.1" || p.OwnerTeam != "team.1" || p.Status != StatusActive {
		t.Fatalf("a plan of a confirmed shipment %+v %v", p, err)
	}
}

// CN-R07 and UL FUL-MilestonePlan: the evidence levels stay apart, an AI agent records only a
// supplier's claim, an actual is never ahead of its recording, an actual of the customs release
// stands for no other node, and a node is overdue until it is verified or accepted (judged at the
// instant asked, no timer).
func TestActualsKeepLevelsAndTypesApart(t *testing.T) {
	p := plan(t)
	claim := Record{NodeID: "release", Level: LevelSupplierClaim, OccurredAt: now.Add(-2 * time.Hour)}
	if _, err := p.Record(1, claim, nil, agent); err != nil || p.RowVersion != 2 {
		t.Fatalf("an agent's supplier claim: %v", err)
	}
	verify := Record{NodeID: "release", Level: LevelInternalVerified, OccurredAt: now.Add(-time.Hour), Evidence: []string{"receipt"}}
	for want, c := range map[string]struct {
		expected int64
		rec      Record
		sources  map[string]bool
		act      Act
	}{
		"VERSION_CONFLICT /args/expected_version": {1, verify, map[string]bool{"receipt": true}, human},
		"REFERENCE_INVALID /args/node_id":         {2, Record{NodeID: "nowhere", Level: LevelSupplierClaim, OccurredAt: now}, nil, human},
		"PERMISSION_DENIED /args/evidence_level":  {2, verify, map[string]bool{"receipt": true}, agent},
		"REFERENCE_INVALID /args/evidence_ids/0":  {2, verify, map[string]bool{}, human},
		"INVALID_KNOWN_VALUE /args/occurred_at":   {2, Record{NodeID: "release", Level: LevelInternalVerified, OccurredAt: now.Add(time.Minute), Evidence: []string{"receipt"}}, map[string]bool{"receipt": true}, human},
	} {
		if _, err := p.Record(c.expected, c.rec, c.sources, c.act); codeAt(err) != want {
			t.Errorf("%s: %s", want, codeAt(err))
		}
	}
	a, err := p.Record(2, verify, map[string]bool{"receipt": true}, human)
	if err != nil || a.Seq != 2 || a.NodeType != "customs_release" || a.PlanRevision != 1 || p.RowVersion != 3 {
		t.Fatalf("verified %+v %v", a, err)
	}
	states := p.States(now)
	if s := states[0]; s.Highest != LevelInternalVerified || s.Claim == nil || s.Verified == nil || s.Accepted != nil || s.Overdue {
		t.Fatalf("customs release %+v", s)
	}
	if s := states[1]; s.Highest != "" || s.Verified != nil || !s.Overdue {
		t.Fatalf("CN-R07: actual departure is not the customs release %+v", s)
	}
	if s := states[2]; s.Overdue {
		t.Fatalf("delivery is not due %+v", s)
	}
	if !p.States(date("2026-12-21"))[2].Overdue {
		t.Fatal("delivery is overdue the day after its date")
	}
	if _, err = p.Record(3, Record{NodeID: "departure", Level: LevelSupplierClaim, OccurredAt: now}, nil, agent); err != nil {
		t.Fatal(err)
	}
	if s := p.States(now)[1]; s.Highest != LevelSupplierClaim || !s.Overdue {
		t.Fatalf("a supplier's claim alone never makes a node done %+v", s)
	}
}

// A revision replaces the nodes and keeps every node that has actuals with its type; a plan
// follows the object it was made for; the same request by the same planner is answered from it.
func TestRevisions(t *testing.T) {
	p := plan(t)
	if _, err := p.Record(1, Record{NodeID: "release", Level: LevelSupplierClaim, OccurredAt: now}, nil, agent); err != nil {
		t.Fatal(err)
	}
	if err := p.Answer("h", agent.By); err != nil {
		t.Fatal(err)
	}
	if codeAt(p.Answer("other", agent.By)) != "BUSINESS_KEY_CONFLICT /args/plan_id" || codeAt(p.Answer("h", human.By)) != "BUSINESS_KEY_CONFLICT /args/plan_id" {
		t.Fatal("another request on the plan ID")
	}
	dropped := nodes()[1:]
	dropped[0].Predecessors = nil
	retyped := nodes()
	retyped[0].Type = "delivery"
	for want, c := range map[string]struct {
		expected int64
		req      Request
	}{
		"VERSION_CONFLICT /args/expected_version": {1, Request{Subject: order, Nodes: nodes()}},
		"INVALID_KNOWN_VALUE /args/subject":       {2, Request{Subject: Subject{Kind: SubjectOrder, ID: "order.2"}, Nodes: nodes()}},
		"INVALID_STATE /args/nodes":               {2, Request{Subject: order, Nodes: dropped}},
		"INVALID_STATE /args/nodes/0/node_type":   {2, Request{Subject: order, Nodes: retyped}},
	} {
		if err := p.Revise(c.expected, c.req, human); codeAt(err) != want {
			t.Errorf("%s: %s", want, codeAt(err))
		}
	}
	more := append(nodes(), Node{ID: "inspect", Type: "inspection", PlannedOn: date("2026-11-01"), Predecessors: []string{}})
	if err := p.Revise(2, Request{Subject: order, Nodes: more, Reason: "加检验"}, human); err != nil || p.Revision != 2 || p.RowVersion != 3 ||
		p.Revised.By != human.By || p.Created.By != agent.By || len(p.Nodes) != 5 || p.Reason != "加检验" {
		t.Fatalf("revised %+v %v", p, err)
	}
	if codeAt(p.Answer("h", agent.By)) != "BUSINESS_KEY_CONFLICT /args/plan_id" {
		t.Fatal("a revised plan is no longer the first request")
	}
}
