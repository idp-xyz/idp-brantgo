//go:build integration

package bootstrap_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S5-03b on PostgreSQL 16: FUL's milestone plans (tactical model FUL-AGG-MilestonePlan with
// MilestoneActual and EvidenceLevel; UL FUL-MilestonePlan; CN-R07; SAP reference GTC-08; Context Map
// R74; owner notes: external systems are manual paths). The world is B1a's FUL world (newFulWorld):
// order.quote.1 active for 120 PCS on line L1, "merch.1" and "merch.2" merchandisers. Python V0.50
// has no fulfilment context: test names cite the design.

// day is the date n days from today (UTC).
func day(n int) string { return time.Now().UTC().AddDate(0, 0, n).Format(time.DateOnly) }

// milestoneAgent provisions an AI agent with the milestone and export lot commands.
func (w *fulWorld) milestoneAgent() context.Context {
	return w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent.milestones"), DataScope: "company",
		Permissions: []string{"milestone.plan", "milestone.record_actual", "export_lot.register", "ful.read", "evd.read"}})
}

func (w *fulWorld) orderPlan(id string) fulapi.PlanMilestones {
	return fulapi.PlanMilestones{IdempotencyKey: w.key(), PlanID: id, Subject: fulapi.MilestoneSubject{Kind: fulapi.SubjectOrder, ID: w.order.ObjectID},
		Nodes: []fulapi.MilestoneNode{
			{NodeID: "prod", NodeType: fulapi.NodeProductionComplete, PlannedOn: day(-3),
				Responsible: &fulapi.MilestoneResponsible{Kind: fulapi.ResponsibleParty, ID: "supplier.1"}},
			{NodeID: "release", NodeType: fulapi.NodeCustomsRelease, PlannedOn: day(10), Predecessors: []string{"prod"}},
			{NodeID: "departure", NodeType: fulapi.NodeActualDeparture, PlannedOn: day(11), Predecessors: []string{"release"}},
			{NodeID: "delivery", NodeType: fulapi.NodeDelivery, PlannedOn: day(40), Predecessors: []string{"departure"}},
			{NodeID: "acceptance", NodeType: fulapi.NodeCustomerAcceptance, PlannedOn: day(45), Predecessors: []string{"delivery"},
				Responsible: &fulapi.MilestoneResponsible{Kind: fulapi.ResponsibleActor, ID: "merch.1"}},
		}}
}

func (w *fulWorld) actual(plan string, version int64, node, level string, at time.Time, evidence ...string) fulapi.RecordMilestoneActual {
	return fulapi.RecordMilestoneActual{IdempotencyKey: w.key(), PlanID: plan, ExpectedVersion: version, NodeID: node, EvidenceLevel: level,
		OccurredAt: kernel.FormatTimestamp(at), EvidenceIDs: evidence}
}

func planNode(v fulapi.MilestonePlanView, id string) fulapi.MilestoneNodeView {
	for _, n := range v.Nodes {
		if n.NodeID == id {
			return n
		}
	}
	return fulapi.MilestoneNodeView{}
}

// UL FUL-MilestonePlan and CN-R07: an AI agent plans the order's nodes — production, customs
// release, actual departure, delivery and customer acceptance, distinct node types — and records a
// supplier's claim without evidence; it may not record an internal verification (a person's, on
// evidence); the merchandiser verifies production and records the customs release, and accepts it.
// The customs release stands for nothing else: actual departure, delivery and acceptance stay
// open. An actual is never ahead of its recording (预计进度不是实际); a supplier's claim alone never
// makes a node done, so a node whose date passed without a verification is overdue — judged at the
// read, no timer. One plan per order; the same request under another key is the same plan; a
// revision keeps every node that has actuals with its type. MilestoneActualRecorded carries the
// node type and evidence level.
func TestMilestonePlanKeepsEvidenceLevelsApart(t *testing.T) {
	w := newFulWorld(t)
	agent := w.milestoneAgent()
	plan := w.orderPlan("plan.order")
	r, err := w.ful.PlanMilestones(agent, plan)
	if err != nil || r.Status != fulapi.PlanActive || r.RowVersion != 1 || r.ObjectID != "plan.order" {
		t.Fatalf("planned %+v %v", r, err)
	}
	plan.IdempotencyKey = w.key()
	if same, err := w.ful.PlanMilestones(agent, plan); err != nil || !same.Replayed || same.ActionID == r.ActionID {
		t.Fatalf("the same plan under another key %+v %v", same, err)
	}
	other := w.orderPlan("plan.order")
	other.Nodes = other.Nodes[:1]
	_, err = w.ful.PlanMilestones(agent, other)
	w.want(err, fulapi.CodeBusinessKeyConflict)
	_, err = w.ful.PlanMilestones(w.merch, w.orderPlan("plan.order.2"))
	if code(err) != fulapi.CodeBusinessKeyConflict || path(err) != "/args/subject" {
		t.Fatalf("one plan per order: %s at %s", code(err), path(err))
	}
	unknown := w.orderPlan("plan.unknown")
	unknown.Subject.ID = "order.none"
	_, err = w.ful.PlanMilestones(w.merch, unknown)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/subject/id" {
		t.Fatalf("unknown order: %s at %s", code(err), path(err))
	}

	ago := time.Now().Add(-time.Hour)
	if a, err := w.ful.RecordMilestoneActual(agent, w.actual("plan.order", 1, "prod", fulapi.LevelSupplierClaim, ago.Add(-time.Hour))); err != nil ||
		a.RowVersion != 2 {
		t.Fatalf("the supplier's claim %+v %v", a, err)
	}
	_, err = w.ful.RecordMilestoneActual(agent, w.actual("plan.order", 2, "prod", fulapi.LevelInternalVerified, ago, "evidence"))
	if code(err) != kernel.CodePermissionDenied || path(err) != "/args/evidence_level" {
		t.Fatalf("an agent verifying: %s at %s", code(err), path(err))
	}
	_, err = w.ful.RecordMilestoneActual(w.merch, w.actual("plan.order", 2, "prod", fulapi.LevelInternalVerified, ago))
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/evidence_ids" {
		t.Fatalf("a verification without evidence: %s at %s", code(err), path(err))
	}
	_, err = w.ful.RecordMilestoneActual(w.merch, w.actual("plan.order", 2, "prod", fulapi.LevelInternalVerified, time.Now().Add(time.Hour), "evidence"))
	w.want(err, kernel.CodeInvalidKnownValue)
	_, err = w.ful.RecordMilestoneActual(w.merch, w.actual("plan.order", 1, "prod", fulapi.LevelInternalVerified, ago, "evidence"))
	w.want(err, kernel.CodeVersionConflict)
	_, err = w.ful.RecordMilestoneActual(w.merch, w.actual("plan.order", 2, "nowhere", fulapi.LevelInternalVerified, ago, "evidence"))
	w.want(err, kernel.CodeReferenceInvalid)
	_, err = w.ful.RecordMilestoneActual(w.merch, w.actual("plan.order", 2, "prod", fulapi.LevelInternalVerified, ago, "no.such.source"))
	w.want(err, kernel.CodeReferenceInvalid)
	for i, c := range []struct{ node, level string }{{"prod", fulapi.LevelInternalVerified}, {"release", fulapi.LevelInternalVerified},
		{"release", fulapi.LevelAccepted}} {
		if a, err := w.ful.RecordMilestoneActual(w.merch, w.actual("plan.order", int64(2+i), c.node, c.level, ago, "evidence")); err != nil ||
			a.RowVersion != int64(3+i) {
			t.Fatalf("%s %s: %+v %v", c.node, c.level, a, err)
		}
	}

	v, err := w.ful.GetMilestonePlan(w.merch, "plan.order")
	w.must(err)
	prod, release, departure := planNode(v, "prod"), planNode(v, "release"), planNode(v, "departure")
	if v.RowVersion != 5 || v.PlanRevision != 1 || len(v.Actuals) != 4 || v.CreatedBy.ID != "agent.milestones" || v.Subject.ID != w.order.ObjectID ||
		prod.HighestLevel != fulapi.LevelInternalVerified || prod.SupplierClaim == nil || prod.SupplierClaim.RecordedBy.ID != "agent.milestones" ||
		prod.Verified == nil || prod.Verified.EvidenceIDs[0] != "evidence" || prod.Accepted != nil || prod.Overdue ||
		prod.Responsible == nil || prod.Responsible.ID != "supplier.1" || release.HighestLevel != fulapi.LevelAccepted || release.Verified == nil {
		t.Fatalf("the plan %+v", v)
	}
	for _, id := range []string{"departure", "delivery", "acceptance"} {
		if n := planNode(v, id); n.HighestLevel != "" || n.Verified != nil || n.Accepted != nil || n.SupplierClaim != nil || n.Overdue {
			t.Errorf("CN-R07: the customs release stands for %s: %+v", id, n)
		}
	}
	if departure.Predecessors[0] != "release" {
		t.Fatalf("departure %+v", departure)
	}

	revision := func(expected int64) fulapi.PlanMilestones {
		p := w.orderPlan("plan.order")
		p.ExpectedVersion = expected
		p.Nodes = append(p.Nodes, fulapi.MilestoneNode{NodeID: "inspect", NodeType: fulapi.NodeInspection, PlannedOn: day(-1)},
			fulapi.MilestoneNode{NodeID: "claimed", NodeType: fulapi.NodeSupplierHandover, PlannedOn: day(-2)})
		return p
	}
	revised := revision(5)
	revised.Reason = "增加中期检验节点"
	if r, err := w.ful.PlanMilestones(w.merch, revised); err != nil || r.RowVersion != 6 {
		t.Fatalf("revised %+v %v", r, err)
	}
	_, err = w.ful.RecordMilestoneActual(agent, w.actual("plan.order", 6, "claimed", fulapi.LevelSupplierClaim, ago))
	w.must(err)
	dropped := revision(7)
	dropped.Nodes = dropped.Nodes[1:]
	dropped.Nodes[0].Predecessors = nil
	_, err = w.ful.PlanMilestones(w.merch, dropped)
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/nodes" {
		t.Fatalf("dropping a node with actuals: %s at %s", code(err), path(err))
	}
	retyped := revision(7)
	retyped.Nodes[1].NodeType = fulapi.NodeActualDeparture
	_, err = w.ful.PlanMilestones(w.merch, retyped)
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/nodes/1/node_type" {
		t.Fatalf("retyping the customs release: %s at %s", code(err), path(err))
	}
	looped := revision(7)
	looped.Nodes[0].Predecessors = []string{"acceptance"}
	_, err = w.ful.PlanMilestones(w.merch, looped)
	w.want(err, kernel.CodeInvalidKnownValue)

	v, err = w.ful.GetMilestonePlan(w.merch, "plan.order")
	w.must(err)
	if v.PlanRevision != 2 || v.RevisionReason != "增加中期检验节点" || v.RevisedBy.ID != "merch.1" || !planNode(v, "inspect").Overdue ||
		!planNode(v, "claimed").Overdue || planNode(v, "claimed").HighestLevel != fulapi.LevelSupplierClaim || planNode(v, "prod").Overdue ||
		v.Actuals[4].PlanRevision != 2 || v.Actuals[0].PlanRevision != 1 {
		t.Fatalf("after the revision %+v", v)
	}

	var events []event
	for _, e := range w.events("company.A", fulapi.Source) {
		if e.Type == fulapi.EventMilestoneActualRecorded {
			events = append(events, e)
		}
	}
	if len(events) != 5 || events[0].Payload["node_type"] != fulapi.NodeProductionComplete || events[0].Payload["evidence_level"] != fulapi.LevelSupplierClaim ||
		events[2].Payload["node_type"] != fulapi.NodeCustomsRelease || events[3].Payload["evidence_level"] != fulapi.LevelAccepted ||
		events[1].Payload["subject_id"] != w.order.ObjectID || events[0].Payload["actual_seq"] != float64(1) {
		t.Fatalf("events %+v", events)
	}
}

// A plan may follow a shipment the planner sees, never a cancelled one; data scope: a merchandiser
// with own data scope does not see another's plan (NOT_FOUND on the read and the commands), and
// another company sees no plan rows (FORCE RLS).
func TestMilestonePlansStayInTheirScope(t *testing.T) {
	w := newFulWorld(t)
	rel := w.released("commitment.1", "40")
	d := w.delivered("commitment.1", "40")
	_, err := w.prepare(w.merch, "shipment.1", "10", d, rel)
	w.must(err)
	_, err = w.prepare(w.merch, "shipment.2", "10", d, rel)
	w.must(err)
	_, err = w.ful.CancelShipment(w.merch, fulapi.CancelShipment{IdempotencyKey: w.key(), ShipmentID: "shipment.2", ExpectedVersion: 1, Reason: "客户推迟"})
	w.must(err)
	ship := func(plan, shipment string) fulapi.PlanMilestones {
		return fulapi.PlanMilestones{IdempotencyKey: w.key(), PlanID: plan, Subject: fulapi.MilestoneSubject{Kind: fulapi.SubjectShipment, ID: shipment},
			Nodes: []fulapi.MilestoneNode{{NodeID: "loaded", NodeType: fulapi.NodeCargoLoaded, PlannedOn: day(2)},
				{NodeID: "departure", NodeType: fulapi.NodeActualDeparture, PlannedOn: day(3), Predecessors: []string{"loaded"}}}}
	}
	_, err = w.ful.PlanMilestones(w.merch, ship("plan.ship.1", "shipment.1"))
	w.must(err)
	_, err = w.ful.PlanMilestones(w.merch, ship("plan.ship.2", "shipment.2"))
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/subject/id" {
		t.Fatalf("a cancelled shipment: %s at %s", code(err), path(err))
	}
	own := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "merch.own"), DataScope: "own",
		Permissions: []string{"milestone.plan", "milestone.record_actual", "ful.read", "evd.read"}})
	_, err = w.ful.PlanMilestones(own, ship("plan.ship.3", "shipment.1"))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/subject/id" {
		t.Fatalf("a shipment outside the data scope: %s at %s", code(err), path(err))
	}
	_, err = w.ful.GetMilestonePlan(own, "plan.ship.1")
	w.want(err, kernel.CodeNotFound)
	_, err = w.ful.RecordMilestoneActual(own, w.actual("plan.ship.1", 1, "loaded", fulapi.LevelInternalVerified, time.Now().Add(-time.Minute), "evidence"))
	w.want(err, kernel.CodeNotFound)
	_, err = w.ful.GetMilestonePlan(w.merch, "a b")
	w.want(err, kernel.CodeSchemaInvalid)
	if w.count("company.B", "brant_ful.milestone_plan") != 0 || w.count("company.B", "brant_ful.milestone_node") != 0 ||
		w.count("company.A", "brant_ful.milestone_plan") != 1 {
		t.Fatal("plans outside their company")
	}
}

// Milestone plans only grow (FUL-AGG-MilestonePlan "row_version CAS"; UL "预计进度不是实际"): the
// application role and the schema owner may not change or delete a node or an actual, move a plan
// to another object, record an actual ahead of its recording or an internal verification without
// evidence, or drop a node that has actuals.
func TestMilestoneRecordsAreImmutable(t *testing.T) {
	w := newFulWorld(t)
	_, err := w.ful.PlanMilestones(w.merch, w.orderPlan("plan.order"))
	w.must(err)
	_, err = w.ful.RecordMilestoneActual(w.merch, w.actual("plan.order", 1, "prod", fulapi.LevelInternalVerified, time.Now().Add(-time.Hour), "evidence"))
	w.must(err)
	for _, sql := range []string{
		`UPDATE brant_ful.milestone_node SET planned_on = planned_on + 1`,
		`DELETE FROM brant_ful.milestone_actual`,
		`UPDATE brant_ful.milestone_actual SET evidence_level = 'accepted'`,
		`UPDATE brant_ful.milestone_plan SET subject_id = 'order.other', row_version = row_version + 1, plan_revision = plan_revision + 1`,
		`DELETE FROM brant_ful.milestone_plan`,
		`UPDATE brant_ful.milestone_plan SET row_version = row_version + 1, actual_count = actual_count + 1`,
	} {
		if err := w.exec("company.A", sql); err == nil || !strings.Contains(err.Error(), "brant_ful") && !strings.Contains(err.Error(), "42501") {
			t.Errorf("as brant_app %s: %v", sql, err)
		}
		if err := w.world.asOwner("company.A", sql); err == nil || !strings.Contains(err.Error(), "brant_ful") {
			t.Errorf("as the owner %s: %v", sql, err)
		}
	}
	for _, sql := range []string{
		`INSERT INTO brant_ful.milestone_actual (tenant_id, company_id, plan_id, seq, plan_revision, node_id, node_type, evidence_level, occurred_at,
			evidence_ids, plan_version, recorded_by_kind, recorded_by_id, record_epoch, recorded_at)
			SELECT tenant_id, company_id, plan_id, 2, 1, 'release', 'customs_release', 'accepted', now(), '{}', 3, 'human', 'x', 1, now()
			FROM brant_ful.milestone_plan`,
		`INSERT INTO brant_ful.milestone_actual (tenant_id, company_id, plan_id, seq, plan_revision, node_id, node_type, evidence_level, occurred_at,
			evidence_ids, plan_version, recorded_by_kind, recorded_by_id, record_epoch, recorded_at)
			SELECT tenant_id, company_id, plan_id, 2, 1, 'release', 'customs_release', 'supplier_claim', now() + interval '1 day', '{}', 3, 'ai', 'x', 1,
			now() FROM brant_ful.milestone_plan`,
		`INSERT INTO brant_ful.milestone_actual (tenant_id, company_id, plan_id, seq, plan_revision, node_id, node_type, evidence_level, occurred_at,
			evidence_ids, plan_version, recorded_by_kind, recorded_by_id, record_epoch, recorded_at)
			SELECT tenant_id, company_id, plan_id, 2, 1, 'release', 'actual_departure', 'supplier_claim', now(), '{}', 3, 'ai', 'x', 1,
			now() FROM brant_ful.milestone_plan`,
	} {
		if err := w.world.asOwner("company.A", sql); err == nil {
			t.Errorf("as the owner %s", sql)
		}
	}
	if v, err := w.ful.GetMilestonePlan(w.merch, "plan.order"); err != nil || v.RowVersion != 2 || len(v.Actuals) != 1 {
		t.Fatalf("after the refused edits %+v %v", v, err)
	}
}
