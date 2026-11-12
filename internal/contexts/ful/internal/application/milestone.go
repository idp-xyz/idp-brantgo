package application

import (
	"context"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/milestone"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// planCommand runs milestone.plan (FUL-AGG-MilestonePlan; UL FUL-MilestonePlan; CN-R07). Before the
// transaction it asks TRD whether the planner sees an order subject (ActiveOrderRef v1, R27). Inside
// it, it locks the plan its ID names FOR UPDATE — the plan is FUL's root of this command and the only
// row it changes; a new plan reads, without a lock, whether another plan follows the object (the
// unique key decides between two at once) and, for a shipment, the shipment as the planner sees it.
type planCommand struct {
	s        Service
	cmd      api.PlanMilestones
	req      milestone.Request
	order    milestone.SubjectFact
	existing *milestone.Plan
	taken    bool
	ship     *milestone.SubjectFact
	owner    [2]string
	replay   bool
}

func (h *planCommand) Prepare(ctx context.Context) error {
	if h.cmd.ExpectedVersion != 0 || h.req.Subject.Kind != milestone.SubjectOrder {
		return nil
	}
	orders, err := h.s.Upstream.Orders.ResolveActiveOrders(ctx, []string{h.req.Subject.ID})
	if err != nil {
		return err
	}
	if f, ok := orders[h.req.Subject.ID]; ok {
		h.order = milestone.SubjectFact{Known: true, Code: f.Code, Status: f.Status}
	}
	return nil
}

func (h *planCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.taken, h.ship, h.replay = nil, false, nil, false
	sc := scope(ctx)
	p, err := postgres.LoadPlan(ctx, exec, sc, h.req.ID, postgres.ForUpdate)
	if err != nil || p != nil || h.cmd.ExpectedVersion != 0 {
		h.existing = p
		return err
	}
	other, err := postgres.PlanOfSubject(ctx, exec, sc, h.req.Subject)
	if err != nil {
		return err
	}
	h.taken = other != ""
	if h.req.Subject.Kind == milestone.SubjectShipment {
		sh, err := postgres.LoadShipment(ctx, exec, sc, h.req.Subject.ID, postgres.NoLock)
		if err != nil {
			return err
		}
		if sh != nil {
			h.ship, h.owner = &milestone.SubjectFact{Known: true, Status: sh.Status}, [2]string{sh.OwnerActor, sh.OwnerTeam}
		}
	}
	return nil
}

func (h *planCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	act := milestone.Act{By: d.Actor, Epoch: d.Epoch, At: h.s.Clock.Now()}
	if p := h.existing; p != nil {
		if !d.Sees(p.OwnerActor, p.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/plan_id", "")
		}
		if h.cmd.ExpectedVersion == 0 {
			if err := p.Answer(h.req.RequestHash, d.Actor); err != nil {
				return action.Outcome{}, err
			}
			h.replay = true
			return action.Outcome{ObjectID: p.ID, RowVersion: p.RowVersion, Status: p.Status}, nil
		}
		from := p.RowVersion
		if err := p.Revise(h.cmd.ExpectedVersion, h.req, act); err != nil {
			return action.Outcome{}, err
		}
		if err := postgres.RevisePlan(ctx, exec, scope(ctx), p, from); err != nil {
			return action.Outcome{}, conflict(err)
		}
		return action.Outcome{ObjectID: p.ID, RowVersion: p.RowVersion, Status: p.Status}, nil
	}
	if h.cmd.ExpectedVersion != 0 {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/plan_id", "")
	}
	subject := h.order
	if h.req.Subject.Kind == milestone.SubjectShipment {
		subject = milestone.SubjectFact{}
		if h.ship != nil && d.Sees(h.owner[0], h.owner[1]) {
			subject = *h.ship
		}
	}
	p, err := milestone.New(h.req, subject, h.taken, act, d.Team)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertPlan(ctx, exec, scope(ctx), p); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: p.ID, RowVersion: p.RowVersion, Status: p.Status}, nil
}

// PlanMilestones plans an object's milestones or revises a plan (milestone.plan; UL
// FUL-MilestonePlan, CN-R07, GTC-08).
func (s Service) PlanMilestones(ctx context.Context, cmd api.PlanMilestones) (api.Receipt, error) {
	h := &planCommand{s: s, cmd: cmd}
	err := cmd.Validate()
	if err == nil {
		h.req = milestone.Request{ID: cmd.PlanID, Subject: milestone.Subject(cmd.Subject), Reason: cmd.Reason, RequestHash: requestHash(cmd)}
		for _, n := range cmd.Nodes {
			on, _ := api.PlannedOn(n.PlannedOn)
			node := milestone.Node{ID: n.NodeID, Type: n.NodeType, PlannedOn: on, Predecessors: slices.Clone(n.Predecessors)}
			if node.Predecessors == nil {
				node.Predecessors = []string{}
			}
			if n.Responsible != nil {
				node.Responsible = &milestone.Responsible{Kind: n.Responsible.Kind, ID: n.Responsible.ID}
			}
			h.req.Nodes = append(h.req.Nodes, node)
		}
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandPlanMilestones, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.PlanID,
		Reason: cmd.Reason}, cmd, err, h)
	res.Replayed = res.Replayed || (err == nil && h.replay)
	return res, err
}

// recordCommand runs milestone.record_actual: before the transaction it asks EVD about the evidence;
// inside it, it locks the plan FOR UPDATE and appends the actual.
type recordCommand struct {
	s        Service
	cmd      api.RecordMilestoneActual
	rec      milestone.Record
	sources  map[string]bool
	existing *milestone.Plan
}

func (h *recordCommand) Prepare(ctx context.Context) error {
	var err error
	h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *recordCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.existing, err = postgres.LoadPlan(ctx, exec, scope(ctx), h.cmd.PlanID, postgres.ForUpdate)
	return err
}

func (h *recordCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	p := h.existing
	if p == nil || !d.Sees(p.OwnerActor, p.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/plan_id", "")
	}
	from := p.RowVersion
	a, err := p.Record(h.cmd.ExpectedVersion, h.rec, h.sources, milestone.Act{By: d.Actor, Epoch: d.Epoch, At: h.s.Clock.Now()})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertActual(ctx, exec, scope(ctx), p, a, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	event := action.Event{Type: api.EventMilestoneActualRecorded, AggregateType: "MilestonePlan", AggregateID: p.ID, AggregateVersion: p.RowVersion,
		Fields: map[string]any{"plan_id": p.ID, "plan_revision": a.PlanRevision, "subject_kind": p.Subject.Kind, "subject_id": p.Subject.ID,
			"actual_seq": a.Seq, "node_id": a.NodeID, "node_type": a.NodeType, "evidence_level": a.Level,
			"occurred_at": kernel.FormatTimestamp(a.OccurredAt), "evidence_ids": a.Evidence}}
	return action.Outcome{ObjectID: p.ID, RowVersion: p.RowVersion, Status: p.Status, Events: []action.Event{event}}, nil
}

// RecordMilestoneActual records what happened at a node of a plan (milestone.record_actual; UL
// FUL-MilestonePlan, CN-R07, R74). It publishes MilestoneActualRecorded.
func (s Service) RecordMilestoneActual(ctx context.Context, cmd api.RecordMilestoneActual) (api.Receipt, error) {
	h := &recordCommand{s: s, cmd: cmd}
	err := cmd.Validate()
	if err == nil {
		at, _ := kernel.ParseTimestamp(cmd.OccurredAt)
		h.rec = milestone.Record{NodeID: cmd.NodeID, Level: cmd.EvidenceLevel, OccurredAt: at, Evidence: slices.Clone(cmd.EvidenceIDs), Note: cmd.Note}
	}
	return s.run(ctx, action.Command{Name: api.CommandRecordMilestoneActual, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.PlanID}, cmd, err, h)
}

// GetMilestonePlan returns a plan the caller sees, audited as milestone_plan.get (ful.read), with
// its nodes judged now: NOT_FOUND otherwise.
func (s Service) GetMilestonePlan(ctx context.Context, id string) (api.MilestonePlanView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.MilestonePlanView{}, invalidID(s, ctx, api.CommandGetMilestonePlan)
	}
	var v api.MilestonePlanView
	err := s.Ledger.Read(ctx, api.CommandGetMilestonePlan, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		p, err := postgres.LoadPlan(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if p == nil || !d.Sees(p.OwnerActor, p.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = planView(p, kernel.Instant(s.Clock.Now()))
		return nil
	})
	return v, err
}

func actualView(a *milestone.Actual) api.MilestoneActualView {
	return api.MilestoneActualView{Seq: a.Seq, NodeID: a.NodeID, NodeType: a.NodeType, EvidenceLevel: a.Level, OccurredAt: a.OccurredAt,
		EvidenceIDs: slices.Clone(a.Evidence), Note: a.Note, PlanRevision: a.PlanRevision, RecordedBy: a.Recorded.By, RecordedAt: a.Recorded.At,
		AuthorityEpoch: a.Recorded.Epoch}
}

func planView(p *milestone.Plan, now time.Time) api.MilestonePlanView {
	v := api.MilestonePlanView{PlanID: p.ID, Status: p.Status, RowVersion: p.RowVersion, PlanRevision: p.Revision,
		Subject: api.MilestoneSubject(p.Subject), Nodes: []api.MilestoneNodeView{}, Actuals: []api.MilestoneActualView{}, CreatedBy: p.Created.By,
		CreatedAt: p.Created.At, RevisedBy: p.Revised.By, RevisedAt: p.Revised.At, RevisionReason: p.Reason, AuthorityEpoch: p.Revised.Epoch,
		CheckedAt: now}
	opt := func(a *milestone.Actual) *api.MilestoneActualView {
		if a == nil {
			return nil
		}
		x := actualView(a)
		return &x
	}
	for _, s := range p.States(now) {
		n := api.MilestoneNodeView{NodeID: s.ID, NodeType: s.Type, PlannedOn: s.PlannedOn.Format(time.DateOnly), Predecessors: slices.Clone(s.Predecessors),
			HighestLevel: s.Highest, SupplierClaim: opt(s.Claim), Verified: opt(s.Verified), Accepted: opt(s.Accepted), Overdue: s.Overdue}
		if s.Responsible != nil {
			n.Responsible = &api.MilestoneResponsible{Kind: s.Responsible.Kind, ID: s.Responsible.ID}
		}
		v.Nodes = append(v.Nodes, n)
	}
	for i := range p.Actuals {
		v.Actuals = append(v.Actuals, actualView(&p.Actuals[i]))
	}
	return v
}
