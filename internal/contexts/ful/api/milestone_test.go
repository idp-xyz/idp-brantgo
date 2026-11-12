package api

import (
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func validPlan() PlanMilestones {
	return PlanMilestones{IdempotencyKey: "k1", PlanID: "plan.1", Subject: MilestoneSubject{Kind: SubjectOrder, ID: "order.1"},
		Nodes: []MilestoneNode{{NodeID: "release", NodeType: NodeCustomsRelease, PlannedOn: "2026-12-01",
			Responsible: &MilestoneResponsible{Kind: ResponsibleParty, ID: "broker.1"}},
			{NodeID: "departure", NodeType: NodeActualDeparture, PlannedOn: "2026-12-02", Predecessors: []string{"release"}}}}
}

// The closed contract of milestone.plan (UL FUL-MilestonePlan: 对象、责任人、前置项与计划日期).
func TestPlanMilestonesContract(t *testing.T) {
	if err := validPlan().Validate(); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(*PlanMilestones){
		"SCHEMA_INVALID /idempotency_key":               func(c *PlanMilestones) { c.IdempotencyKey = "" },
		"SCHEMA_INVALID /args/plan_id":                  func(c *PlanMilestones) { c.PlanID = "a b" },
		"SCHEMA_INVALID /args/expected_version":         func(c *PlanMilestones) { c.ExpectedVersion = -1 },
		"SCHEMA_INVALID /args/subject/kind":             func(c *PlanMilestones) { c.Subject.Kind = "quote" },
		"SCHEMA_INVALID /args/subject/id":               func(c *PlanMilestones) { c.Subject.ID = "" },
		"SCHEMA_INVALID /args/nodes":                    func(c *PlanMilestones) { c.Nodes = nil },
		"SCHEMA_INVALID /args/reason":                   func(c *PlanMilestones) { c.Reason = " " },
		"DUPLICATE_LINE_ID /args/nodes/1/node_id":       func(c *PlanMilestones) { c.Nodes[1].NodeID = "release" },
		"SCHEMA_INVALID /args/nodes/0/node_type":        func(c *PlanMilestones) { c.Nodes[0].NodeType = "shipped" },
		"SCHEMA_INVALID /args/nodes/0/planned_on":       func(c *PlanMilestones) { c.Nodes[0].PlannedOn = "2026-02-30" },
		"SCHEMA_INVALID /args/nodes/0/responsible/kind": func(c *PlanMilestones) { c.Nodes[0].Responsible.Kind = "team" },
		"SCHEMA_INVALID /args/nodes/0/responsible/id":   func(c *PlanMilestones) { c.Nodes[0].Responsible.ID = "" },
		"SCHEMA_INVALID /args/nodes/1/predecessors/1":   func(c *PlanMilestones) { c.Nodes[1].Predecessors = []string{"release", "release"} },
	} {
		c := validPlan()
		edit(&c)
		rej, ok := kernel.AsRejection(c.Validate())
		if !ok || rej.Code+" "+rej.Fields[0].Path != want {
			t.Errorf("%s: %v", want, c.Validate())
		}
	}
}

// CN-R07 and GTC-08: customs release, actual departure, delivery and customer acceptance are
// distinct node types; the evidence levels keep a supplier's claim, an internal verification and an
// accepted fact apart.
func TestNodeTypesAndLevels(t *testing.T) {
	types := NodeTypes()
	for _, n := range []string{NodeCustomsRelease, NodeActualDeparture, NodeDelivery, NodeCustomerAcceptance} {
		if !NodeType(n) {
			t.Errorf("%s", n)
		}
	}
	sorted := slices.Clone(types)
	slices.Sort(sorted)
	if len(slices.Compact(sorted)) != len(types) || NodeType("shipped") {
		t.Fatal("distinct node types")
	}
	if !Level(LevelSupplierClaim) || !Level(LevelInternalVerified) || !Level(LevelAccepted) || Level("verified") {
		t.Fatal("levels")
	}
	if _, ok := PlannedOn("2026-1-01"); ok {
		t.Fatal("a date is YYYY-MM-DD")
	}
}

// The closed contract of milestone.record_actual: an internal verification or an accepted fact
// cites evidence; a supplier's claim may not.
func TestRecordMilestoneActualContract(t *testing.T) {
	valid := RecordMilestoneActual{IdempotencyKey: "k1", PlanID: "plan.1", ExpectedVersion: 1, NodeID: "release", EvidenceLevel: LevelSupplierClaim,
		OccurredAt: "2026-11-12T00:00:00Z"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(*RecordMilestoneActual){
		"SCHEMA_INVALID /args/expected_version": func(c *RecordMilestoneActual) { c.ExpectedVersion = 0 },
		"SCHEMA_INVALID /args/node_id":          func(c *RecordMilestoneActual) { c.NodeID = "" },
		"SCHEMA_INVALID /args/evidence_level":   func(c *RecordMilestoneActual) { c.EvidenceLevel = "rumoured" },
		"SCHEMA_INVALID /args/occurred_at":      func(c *RecordMilestoneActual) { c.OccurredAt = "2026-11-12 00:00" },
		"SCHEMA_INVALID /args/evidence_ids":     func(c *RecordMilestoneActual) { c.EvidenceLevel = LevelAccepted },
		"SCHEMA_INVALID /args/evidence_ids/1":   func(c *RecordMilestoneActual) { c.EvidenceIDs = []string{"e", "e"} },
		"SCHEMA_INVALID /args/note":             func(c *RecordMilestoneActual) { c.Note = "\x00" },
	} {
		c := valid
		edit(&c)
		rej, ok := kernel.AsRejection(c.Validate())
		if !ok || rej.Code+" "+rej.Fields[0].Path != want {
			t.Errorf("%s: %v", want, c.Validate())
		}
	}
	verified := valid
	verified.EvidenceLevel, verified.EvidenceIDs = LevelInternalVerified, []string{"e"}
	if err := verified.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The closed contract of export_lot.register: distinct lot lines and shipment lines, positive
// quantities.
func TestRegisterExportLotContract(t *testing.T) {
	valid := RegisterExportLot{IdempotencyKey: "k1", ExportLotID: "lot.1", ShipmentID: "shipment.1",
		Lines: []ExportLotLine{{LineID: "E1", ShipmentLineID: "S1", Quantity: "30"}, {LineID: "E2", ShipmentLineID: "S2", Quantity: "0.5"}}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(*RegisterExportLot){
		"SCHEMA_INVALID /args/export_lot_id":               func(c *RegisterExportLot) { c.ExportLotID = "" },
		"SCHEMA_INVALID /args/shipment_id":                 func(c *RegisterExportLot) { c.ShipmentID = "a b" },
		"SCHEMA_INVALID /args/lines":                       func(c *RegisterExportLot) { c.Lines = nil },
		"DUPLICATE_LINE_ID /args/lines/1/line_id":          func(c *RegisterExportLot) { c.Lines[1].LineID = "E1" },
		"DUPLICATE_LINE_ID /args/lines/1/shipment_line_id": func(c *RegisterExportLot) { c.Lines[1].ShipmentLineID = "S1" },
		"SCHEMA_INVALID /args/lines/0/quantity":            func(c *RegisterExportLot) { c.Lines[0].Quantity = "0" },
	} {
		c := valid
		c.Lines = slices.Clone(valid.Lines)
		edit(&c)
		rej, ok := kernel.AsRejection(c.Validate())
		if !ok || rej.Code+" "+rej.Fields[0].Path != want {
			t.Errorf("%s: %v", want, c.Validate())
		}
	}
}

// ShipmentLineRef v1's verdicts and the lookups' bounds.
func TestShipmentLineRefCheck(t *testing.T) {
	if (ShipmentLineRef{}).Check("/x") != nil || !(ShipmentLineRef{}).Settleable() {
		t.Fatal("a confirmed, priced line is a settlement source")
	}
	for code, want := range map[string]string{kernel.CodeInvalidState: kernel.CodeInvalidState, kernel.CodeReferenceInvalid: kernel.CodeReferenceInvalid} {
		rej, ok := kernel.AsRejection(ShipmentLineRef{Code: code, ShipmentStatus: StatusPrepared}.Check("/lines/0"))
		if !ok || rej.Code != want || rej.Fields[0].Path != "/lines/0" {
			t.Errorf("%s: %v", code, rej)
		}
	}
	if ValidateShipmentLineKeys([]ShipmentLineKey{{ShipmentID: "s", LineID: ""}}) == nil || ValidateIDs([]string{"a", "b"}, 1) == nil ||
		ValidateIDs([]string{"a b"}, 5) == nil || ValidateIDs([]string{"a"}, 1) != nil {
		t.Fatal("lookup bounds")
	}
}
