package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Milestone plans (WBS S5-03b; tactical model FUL-AGG-MilestonePlan with MilestoneActual and
// EvidenceLevel; UL FUL-MilestonePlan; baseline 3.5, F25, CN-R07; SAP reference GTC-08). A plan
// follows one object — a sales order or a FUL shipment — through nodes with a type, a planned date, a
// responsible and predecessors; what happened is recorded beside it as actuals, never as a change of
// the plan ("预计进度不是实际"). An actual has an evidence level: a supplier's claim, an internal
// verification or an accepted fact, kept apart. Customs release, actual departure, delivery and
// customer acceptance are distinct node types, and an actual of one type never stands for another
// (CN-R07). Nothing runs on a timer: whether a node is overdue is judged when the plan is read.
//
//	milestone.plan            human or AI (merchandiser): with expected_version 0 plans a new object;
//	                          with the plan's row version revises its nodes. One plan per object
//	                          (BUSINESS_KEY_CONFLICT). A node that has actuals keeps its type and
//	                          stays in the plan.
//	milestone.record_actual   human or AI (merchandiser): records what happened at one node, at an
//	                          evidence level. A supplier's claim may come from an AI agent and needs
//	                          no evidence; an internal verification or an accepted fact is a human's,
//	                          with at least one EVD source (R74; external systems are manual paths
//	                          in S5). The instant it happened is never in the future.
//
// Request members in snake_case: milestone.plan {plan_id, expected_version, subject {kind, id},
// nodes [{node_id, node_type, planned_on, responsible {kind, id}, predecessors}], reason};
// milestone.record_actual {plan_id, expected_version, node_id, evidence_level, occurred_at,
// evidence_ids, note}. planned_on is a date (YYYY-MM-DD); occurred_at the contracts' UTC timestamp.

// Milestone commands, reads and events (Context Map FUL commands and facts).
const (
	CommandPlanMilestones        = "milestone.plan"
	CommandRecordMilestoneActual = "milestone.record_actual"
	CommandGetMilestonePlan      = "milestone_plan.get"

	EventMilestoneActualRecorded = "idp-brant.ful.milestone_actual_recorded.v1"

	// The objects a plan follows.
	SubjectOrder    = "order"
	SubjectShipment = "shipment"

	// Who answers for a node: one of our users (actor) or a PTY party such as a supplier or a
	// forwarder (party).
	ResponsibleActor = "actor"
	ResponsibleParty = "party"

	// Evidence levels of an actual (UL FUL-MilestonePlan: 供应商自报、内部核实与已验收).
	LevelSupplierClaim    = "supplier_claim"
	LevelInternalVerified = "internal_verified"
	LevelAccepted         = "accepted"

	// PlanActive is the state of every plan: a plan has no state machine (tactical model).
	PlanActive = "active"

	MaxNodes        = 100
	MaxPredecessors = 20
)

// Node types (CN-R07, GTC-08: 海关放行、实际离境、物流送达和客户验收分别作为事实). The production nodes
// follow the supplier's work, the shipping nodes FUL's own.
const (
	NodeMaterialReady      = "material_ready"
	NodeProductionStart    = "production_start"
	NodeProductionComplete = "production_complete"
	NodeInspection         = "inspection"
	NodePackingComplete    = "packing_complete"
	NodeSupplierHandover   = "supplier_handover"
	NodeBookingConfirmed   = "booking_confirmed"
	NodeCargoLoaded        = "cargo_loaded"
	NodeCustomsRelease     = "customs_release"
	NodeActualDeparture    = "actual_departure"
	NodeArrival            = "arrival"
	NodeDelivery           = "delivery"
	NodeCustomerAcceptance = "customer_acceptance"
)

// NodeTypes returns the node types in their usual order.
func NodeTypes() []string {
	return []string{NodeMaterialReady, NodeProductionStart, NodeProductionComplete, NodeInspection, NodePackingComplete, NodeSupplierHandover,
		NodeBookingConfirmed, NodeCargoLoaded, NodeCustomsRelease, NodeActualDeparture, NodeArrival, NodeDelivery, NodeCustomerAcceptance}
}

// NodeType reports whether s is a node type.
func NodeType(s string) bool {
	for _, t := range NodeTypes() {
		if t == s {
			return true
		}
	}
	return false
}

// Level reports whether s is an evidence level.
func Level(s string) bool {
	return s == LevelSupplierClaim || s == LevelInternalVerified || s == LevelAccepted
}

// MilestoneSubject is the object a plan follows: an order (Kind SubjectOrder, ID the TRD order) or
// a shipment (SubjectShipment, a FUL shipment).
type MilestoneSubject struct {
	Kind string
	ID   string
}

// MilestoneResponsible is who answers for a node: one of our users (ResponsibleActor) or a PTY
// party (ResponsibleParty).
type MilestoneResponsible struct {
	Kind string
	ID   string
}

// MilestoneNode is a node of a plan: its ID, type, planned date (YYYY-MM-DD), the responsible
// (optional) and the nodes of the plan it waits for.
type MilestoneNode struct {
	NodeID       string
	NodeType     string
	PlannedOn    string
	Responsible  *MilestoneResponsible
	Predecessors []string
}

// PlanMilestones plans object Subject as plan PlanID (ExpectedVersion 0) or revises the plan's nodes
// at its row version ExpectedVersion; Reason, optional, says why. Human or AI.
type PlanMilestones struct {
	IdempotencyKey  string
	PlanID          string
	ExpectedVersion int64
	Subject         MilestoneSubject
	Nodes           []MilestoneNode
	Reason          string
}

// RecordMilestoneActual records what happened at node NodeID of plan PlanID at its row version
// ExpectedVersion: at OccurredAt, on EvidenceLevel, with the EVD sources EvidenceIDs and a Note
// (optional). Human or AI; internal_verified and accepted are a human's and cite evidence.
type RecordMilestoneActual struct {
	IdempotencyKey  string
	PlanID          string
	ExpectedVersion int64
	NodeID          string
	EvidenceLevel   string
	OccurredAt      string
	EvidenceIDs     []string
	Note            string
}

func (PlanMilestones) fulCommand()        {}
func (RecordMilestoneActual) fulCommand() {}

// PlannedOn parses a planned date: YYYY-MM-DD, a real calendar day of the years 2000 to 2999.
func PlannedOn(s string) (time.Time, bool) {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil || d.Format(time.DateOnly) != s || d.Year() < 2000 || d.Year() > 2999 {
		return time.Time{}, false
	}
	return d, true
}

// Validate checks the closed contract: a plan ID, a subject, 1 to MaxNodes nodes with distinct IDs
// (DUPLICATE_LINE_ID), known node types, planned dates, complete responsibles and at most
// MaxPredecessors distinct predecessors per node; the reason, when stated, is a reason. Whether the
// predecessors name nodes of the plan without a cycle is the plan's rule.
func (c PlanMilestones) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.PlanID):
		return schema("/args/plan_id")
	case c.ExpectedVersion < 0 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case c.Subject.Kind != SubjectOrder && c.Subject.Kind != SubjectShipment:
		return schema("/args/subject/kind")
	case !kernel.ValidIdentifier(c.Subject.ID):
		return schema("/args/subject/id")
	case len(c.Nodes) < 1 || len(c.Nodes) > MaxNodes:
		return schema("/args/nodes")
	case c.Reason != "" && !Reason(c.Reason):
		return schema("/args/reason")
	}
	seen := map[string]bool{}
	for i, n := range c.Nodes {
		p := index("/args/nodes", i)
		if !kernel.ValidIdentifier(n.NodeID) {
			return schema(p + "/node_id")
		}
		if seen[n.NodeID] {
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/node_id", "")
		}
		seen[n.NodeID] = true
		if _, ok := PlannedOn(n.PlannedOn); !ok {
			return schema(p + "/planned_on")
		}
		switch {
		case !NodeType(n.NodeType):
			return schema(p + "/node_type")
		case n.Responsible != nil && n.Responsible.Kind != ResponsibleActor && n.Responsible.Kind != ResponsibleParty:
			return schema(p + "/responsible/kind")
		case n.Responsible != nil && !kernel.ValidIdentifier(n.Responsible.ID):
			return schema(p + "/responsible/id")
		case len(n.Predecessors) > MaxPredecessors:
			return schema(p + "/predecessors")
		}
		pre := map[string]bool{}
		for j, id := range n.Predecessors {
			if !kernel.ValidIdentifier(id) || pre[id] {
				return schema(index(p+"/predecessors", j))
			}
			pre[id] = true
		}
	}
	return nil
}

// Validate checks the closed contract: a known evidence level, a timestamp, evidence (required for
// internal_verified and accepted) and a note, when stated, of a reason's form (Reason).
func (c RecordMilestoneActual) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.PlanID):
		return schema("/args/plan_id")
	case version(c.ExpectedVersion) != nil:
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.NodeID):
		return schema("/args/node_id")
	case !Level(c.EvidenceLevel):
		return schema("/args/evidence_level")
	}
	if _, err := kernel.ParseTimestamp(c.OccurredAt); err != nil {
		return schema("/args/occurred_at")
	}
	if c.EvidenceLevel != LevelSupplierClaim && len(c.EvidenceIDs) == 0 {
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/evidence_ids", "An internal verification or an accepted fact cites evidence")
	}
	if c.Note != "" && !Reason(c.Note) {
		return schema("/args/note")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// MilestoneActualView is one actual of a plan: its number in the plan, the node and its type, the
// evidence level, when it happened, the evidence and note, who recorded it when and under which
// plan revision.
type MilestoneActualView struct {
	Seq            int64
	NodeID         string
	NodeType       string
	EvidenceLevel  string
	OccurredAt     time.Time
	EvidenceIDs    []string
	Note           string
	PlanRevision   int64
	RecordedBy     kernel.ActorRef
	RecordedAt     time.Time
	AuthorityEpoch int64
}

// MilestoneNodeView is a node of the plan's current revision with what was recorded of it: the
// latest actual of each evidence level, the highest level recorded ("" for none) and whether it is
// overdue — its planned date passed (UTC) without an internal verification or an accepted fact. A
// supplier's claim alone never makes a node done.
type MilestoneNodeView struct {
	NodeID        string
	NodeType      string
	PlannedOn     string
	Responsible   *MilestoneResponsible
	Predecessors  []string
	HighestLevel  string
	SupplierClaim *MilestoneActualView
	Verified      *MilestoneActualView
	Accepted      *MilestoneActualView
	Overdue       bool
}

// MilestonePlanView is a plan as the read shows it: the object it follows, its row version and
// revision, the nodes of the current revision, every actual in the order recorded, who planned it
// first and who revised it last, and the instant overdue was judged at.
type MilestonePlanView struct {
	PlanID         string
	Status         string
	RowVersion     int64
	PlanRevision   int64
	Subject        MilestoneSubject
	Nodes          []MilestoneNodeView
	Actuals        []MilestoneActualView
	CreatedBy      kernel.ActorRef
	CreatedAt      time.Time
	RevisedBy      kernel.ActorRef
	RevisedAt      time.Time
	RevisionReason string
	AuthorityEpoch int64
	CheckedAt      time.Time
}

// MilestoneCommands is FUL's command entry point for milestone plans. Errors are *kernel.Rejection
// for refusals.
type MilestoneCommands interface {
	PlanMilestones(ctx context.Context, cmd PlanMilestones) (Receipt, error)
	RecordMilestoneActual(ctx context.Context, cmd RecordMilestoneActual) (Receipt, error)
}

// MilestoneQueries is the audited read of milestone plans.
type MilestoneQueries interface {
	// GetMilestonePlan returns a plan the caller sees, audited as milestone_plan.get (ful.read):
	// NOT_FOUND otherwise.
	GetMilestonePlan(ctx context.Context, planID string) (MilestonePlanView, error)
}
