// Package milestone holds the rules of FUL's MilestonePlan aggregate (tactical model
// FUL-AGG-MilestonePlan with MilestoneActual and EvidenceLevel; UL FUL-MilestonePlan; CN-R07; SAP
// reference GTC-08; WBS S5-03b): a plan of the nodes one object goes through, revised as a whole, and
// the actuals recorded beside it at an evidence level. A plan never becomes an actual and an actual
// never changes the plan ("预计进度不是实际"); an actual of one node type never stands for another
// (customs release, actual departure, delivery and customer acceptance are distinct facts). The
// package knows no database.
package milestone

import (
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The objects a plan follows, who answers for a node and the evidence levels of an actual (as in
// package api).
const (
	SubjectOrder    = "order"
	SubjectShipment = "shipment"

	LevelSupplierClaim    = "supplier_claim"
	LevelInternalVerified = "internal_verified"
	LevelAccepted         = "accepted"

	StatusActive = "active"

	// A shipment that cancelled ends a plan's subject before any plan is made.
	shipmentCancelled = "cancelled"
)

// Codes the plan rules give besides the shared kernel codes, with the error catalog's categories.
const (
	CodeBusinessKeyConflict   = "BUSINESS_KEY_CONFLICT"
	CodeOrderNotActive        = "ORDER_NOT_ACTIVE"
	CodeOrderApprovalRequired = "ORDER_APPROVAL_REQUIRED"
	CodeOrderApprovalRevoked  = "ORDER_APPROVAL_REVOKED"
	CodeReferenceStale        = "REFERENCE_STALE"
)

// Codes maps every code of the plan rules to its category.
var Codes = map[string]kernel.Category{
	CodeBusinessKeyConflict:   kernel.CategoryConflict,
	CodeOrderNotActive:        kernel.CategoryInvalid,
	CodeOrderApprovalRequired: kernel.CategoryInvalid,
	CodeOrderApprovalRevoked:  kernel.CategoryInvalid,
	CodeReferenceStale:        kernel.CategoryConflict,
}

// Reject returns a rejection with a code of the plan rules or a shared kernel code, at path.
func Reject(code, path, reason string) *kernel.Rejection {
	if c, ok := Codes[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

// Act is who did something, under which authority epoch, when.
type Act struct {
	By    kernel.ActorRef
	Epoch int64
	At    time.Time
}

// Subject is the object a plan follows.
type Subject struct {
	Kind string
	ID   string
}

// Responsible is who answers for a node: an actor (one of our users) or a PTY party.
type Responsible struct {
	Kind string
	ID   string
}

// Node is a node of a plan revision: its ID and type, its planned date (UTC midnight), the
// responsible (nil: none named) and the nodes it waits for.
type Node struct {
	ID           string
	Type         string
	PlannedOn    time.Time
	Responsible  *Responsible
	Predecessors []string
}

// Actual is what happened at a node: Seq numbers the plan's actuals from 1; NodeType is the node's
// type when it was recorded; PlanRevision the plan revision then.
type Actual struct {
	Seq          int64
	NodeID       string
	NodeType     string
	Level        string
	OccurredAt   time.Time
	Evidence     []string
	Note         string
	PlanRevision int64
	Recorded     Act
}

// Plan is the aggregate root. RowVersion counts its commands: 1 when planned, one more for every
// revision and every actual. Revision counts the revisions of its nodes from 1.
type Plan struct {
	ID          string
	Status      string
	RowVersion  int64
	Revision    int64
	Subject     Subject
	Nodes       []Node
	Actuals     []Actual
	OwnerActor  string
	OwnerTeam   string
	RequestHash string
	Created     Act
	Revised     Act
	Reason      string
}

// SubjectFact is what the plan's subject is now as the planner sees it: Known false for an object the
// planner does not see; for an order, Code is TRD's reason it does not serve new downstream work (""
// when it does); for a shipment, Status is its state.
type SubjectFact struct {
	Known  bool
	Code   string
	Status string
}

// Request is a plan or a revision: the plan's ID, the subject, the nodes, the reason (optional) and
// the request's hash.
type Request struct {
	ID          string
	Subject     Subject
	Nodes       []Node
	Reason      string
	RequestHash string
}

func nodePath(i int, member string) string {
	p := "/args/nodes/" + strconv.Itoa(i)
	if member != "" {
		p += "/" + member
	}
	return p
}

// checkNodes refuses predecessors that name no node of the plan, the node itself, or close a cycle
// (INVALID_KNOWN_VALUE).
func checkNodes(nodes []Node) error {
	at := map[string]int{}
	for i, n := range nodes {
		at[n.ID] = i
	}
	for i, n := range nodes {
		for j, p := range n.Predecessors {
			if k, ok := at[p]; !ok || k == i {
				return kernel.Reject(kernel.CodeInvalidKnownValue, nodePath(i, "predecessors/"+strconv.Itoa(j)), "No other node of the plan has this ID")
			}
		}
	}
	// Depth-first search over the predecessor edges: 1 visiting, 2 done.
	state := make([]int, len(nodes))
	var visit func(i int) bool
	visit = func(i int) bool {
		state[i] = 1
		for _, p := range nodes[i].Predecessors {
			k := at[p]
			if state[k] == 1 || (state[k] == 0 && !visit(k)) {
				return false
			}
		}
		state[i] = 2
		return true
	}
	for i := range nodes {
		if state[i] == 0 && !visit(i) {
			return kernel.Reject(kernel.CodeInvalidKnownValue, nodePath(i, "predecessors"), "The predecessors form a cycle")
		}
	}
	return nil
}

// New decides a new plan. It refuses, in this order: a subject the planner does not see
// (REFERENCE_INVALID), an order that does not serve downstream work (TRD's code) or a cancelled
// shipment (INVALID_STATE), a subject another plan follows (BUSINESS_KEY_CONFLICT; one plan per
// object), and predecessors that name no other node or form a cycle (INVALID_KNOWN_VALUE).
func New(req Request, subject SubjectFact, taken bool, act Act, team string) (*Plan, error) {
	if !subject.Known {
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/subject/id", "No "+req.Subject.Kind+" the actor reads")
	}
	switch {
	case req.Subject.Kind == SubjectOrder && subject.Code != "":
		return nil, upstream(subject.Code, "/args/subject/id", "The order does not serve downstream work: "+subject.Code)
	case req.Subject.Kind == SubjectShipment && subject.Status == shipmentCancelled:
		return nil, Reject(kernel.CodeInvalidState, "/args/subject/id", "The shipment is cancelled")
	case taken:
		return nil, Reject(CodeBusinessKeyConflict, "/args/subject", "Another plan follows this "+req.Subject.Kind)
	}
	if err := checkNodes(req.Nodes); err != nil {
		return nil, err
	}
	act.At = kernel.Instant(act.At)
	return &Plan{ID: req.ID, Status: StatusActive, RowVersion: 1, Revision: 1, Subject: req.Subject, Nodes: req.Nodes, OwnerActor: act.By.ID,
		OwnerTeam: team, RequestHash: req.RequestHash, Created: act, Revised: act, Reason: req.Reason}, nil
}

// upstream passes TRD's verdict on with its code when the plan rules know it, and as
// REFERENCE_INVALID otherwise.
func upstream(code, path, reason string) *kernel.Rejection {
	if _, ok := Codes[code]; ok {
		return Reject(code, path, reason)
	}
	return kernel.Reject(kernel.CodeReferenceInvalid, path, reason)
}

// Answer decides what a new plan gets whose ID is taken: nil when the stored plan is the same request
// by the same planner (the command is answered from it), and BUSINESS_KEY_CONFLICT otherwise.
func (p *Plan) Answer(requestHash string, by kernel.ActorRef) error {
	if p.RequestHash != requestHash || p.Created.By != by || p.Revision != 1 {
		return Reject(CodeBusinessKeyConflict, "/args/plan_id", "The plan ID holds another plan")
	}
	return nil
}

// recorded returns the node IDs that have actuals, with the type each was recorded under.
func (p *Plan) recorded() map[string]string {
	out := map[string]string{}
	for _, a := range p.Actuals {
		out[a.NodeID] = a.NodeType
	}
	return out
}

// Revise replaces the plan's nodes (a new revision). It refuses another row version
// (VERSION_CONFLICT), another subject (INVALID_KNOWN_VALUE: a plan follows one object), predecessors
// that name no other node or form a cycle, and a revision that drops a node with actuals or changes
// its type (INVALID_STATE: what happened stays where it was recorded).
func (p *Plan) Revise(expected int64, req Request, act Act) error {
	if p.RowVersion != expected {
		return Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if req.Subject != p.Subject {
		return Reject(kernel.CodeInvalidKnownValue, "/args/subject", "A plan follows the object it was made for")
	}
	if err := checkNodes(req.Nodes); err != nil {
		return err
	}
	kept := p.recorded()
	ids := make([]string, 0, len(kept))
	for id := range kept {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		i := slices.IndexFunc(req.Nodes, func(n Node) bool { return n.ID == id })
		if i < 0 {
			return Reject(kernel.CodeInvalidState, "/args/nodes", "Node "+id+" has actuals and stays in the plan")
		}
		if req.Nodes[i].Type != kept[id] {
			return Reject(kernel.CodeInvalidState, nodePath(i, "node_type"), "Node "+id+" has actuals of type "+kept[id])
		}
	}
	act.At = kernel.Instant(act.At)
	p.Nodes, p.Reason, p.Revised = req.Nodes, req.Reason, act
	p.Revision++
	p.RowVersion++
	return nil
}

// Node returns the node id of the current revision, or false.
func (p *Plan) Node(id string) (Node, bool) {
	i := slices.IndexFunc(p.Nodes, func(n Node) bool { return n.ID == id })
	if i < 0 {
		return Node{}, false
	}
	return p.Nodes[i], true
}

// Record is an actual to record: the node, the evidence level, when it happened, the evidence and
// the note.
type Record struct {
	NodeID     string
	Level      string
	OccurredAt time.Time
	Evidence   []string
	Note       string
}

// Record appends an actual. It refuses, in this order: another row version (VERSION_CONFLICT), a
// node the plan's current revision does not have (REFERENCE_INVALID), an internal verification or
// accepted fact by an actor who is not human (PERMISSION_DENIED: it is a person's judgement on
// evidence; external systems are manual paths), evidence the actor does not read
// (REFERENCE_INVALID), and an instant after now (INVALID_KNOWN_VALUE: 预计进度不是实际).
func (p *Plan) Record(expected int64, r Record, sources map[string]bool, act Act) (*Actual, error) {
	if p.RowVersion != expected {
		return nil, Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	n, ok := p.Node(r.NodeID)
	if !ok {
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/node_id", "The plan has no such node")
	}
	if r.Level != LevelSupplierClaim && act.By.Kind != kernel.ActorHuman {
		return nil, Reject(kernel.CodePermissionDenied, "/args/evidence_level", "An internal verification or an accepted fact is recorded by a person")
	}
	for i, id := range r.Evidence {
		if !sources[id] {
			return nil, Reject(kernel.CodeReferenceInvalid, "/args/evidence_ids/"+strconv.Itoa(i), "No source the actor reads")
		}
	}
	act.At = kernel.Instant(act.At)
	at := kernel.Instant(r.OccurredAt)
	if at.After(act.At) {
		return nil, Reject(kernel.CodeInvalidKnownValue, "/args/occurred_at", "An actual cannot happen after it is recorded")
	}
	a := Actual{Seq: int64(len(p.Actuals)) + 1, NodeID: n.ID, NodeType: n.Type, Level: r.Level, OccurredAt: at, Evidence: slices.Clone(r.Evidence),
		Note: r.Note, PlanRevision: p.Revision, Recorded: act}
	if a.Evidence == nil {
		a.Evidence = []string{}
	}
	p.Actuals = append(p.Actuals, a)
	p.RowVersion++
	return &p.Actuals[len(p.Actuals)-1], nil
}

// NodeState is a node of the current revision with what was recorded of it: the latest actual of
// each level, the highest level recorded ("" none) and whether it is overdue at the instant asked.
type NodeState struct {
	Node
	Claim    *Actual
	Verified *Actual
	Accepted *Actual
	Highest  string
	Overdue  bool
}

// States returns the nodes of the current revision with their actuals, judged at asOf (no timer
// writes anything): a node is overdue when its planned date (UTC) is before asOf's date and neither
// an internal verification nor an accepted fact was recorded for it. A supplier's claim alone never
// makes a node done.
func (p *Plan) States(asOf time.Time) []NodeState {
	day := kernel.Instant(asOf).Truncate(24 * time.Hour)
	out := make([]NodeState, 0, len(p.Nodes))
	for _, n := range p.Nodes {
		s := NodeState{Node: n}
		for i := range p.Actuals {
			a := &p.Actuals[i]
			if a.NodeID != n.ID {
				continue
			}
			switch a.Level {
			case LevelSupplierClaim:
				s.Claim = a
			case LevelInternalVerified:
				s.Verified = a
			case LevelAccepted:
				s.Accepted = a
			}
		}
		switch {
		case s.Accepted != nil:
			s.Highest = LevelAccepted
		case s.Verified != nil:
			s.Highest = LevelInternalVerified
		case s.Claim != nil:
			s.Highest = LevelSupplierClaim
		}
		s.Overdue = s.Verified == nil && s.Accepted == nil && n.PlannedOn.Before(day)
		out = append(out, s)
	}
	return out
}
