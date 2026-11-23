package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/api"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/domain/plan"
	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/domain/subcontract"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func done(id string, version int64, status string, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: id, RowVersion: version, Status: status, Events: events}
}

func (s Service) ConfigureModule(ctx context.Context, cmd api.ConfigureModule) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandConfigure, IdempotencyKey: cmd.IdempotencyKey, ObjectID: "module"}, cmd, cmd.Validate(), &configureCmd{s: s, cmd: cmd})
}

type configureCmd struct {
	s   Service
	cmd api.ConfigureModule
	cur *postgres.Setting
}

func (h *configureCmd) Lock(ctx context.Context, exec bentopg.Executor) error {
	st, err := postgres.LoadSetting(ctx, exec, scopeOf(ctx), postgres.ForUpdate)
	h.cur = st
	return err
}

func (h *configureCmd) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	now := kernel.Instant(h.s.Clock.Now())
	next := postgres.Setting{Enabled: h.cmd.Enabled, ActorKind: string(d.Actor.Kind), ActorID: d.Actor.ID, Epoch: d.Epoch, UpdatedAt: now}
	switch {
	case h.cmd.ExpectedVersion == 0 && h.cur == nil:
		next.RowVersion = 1
		if err := postgres.InsertSetting(ctx, exec, scopeOf(ctx), next); err != nil {
			return action.Outcome{}, err
		}
	case h.cur == nil || h.cur.RowVersion != h.cmd.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	default:
		next.RowVersion = h.cur.RowVersion + 1
		if err := postgres.SaveSetting(ctx, exec, scopeOf(ctx), next, h.cur.RowVersion); err != nil {
			return action.Outcome{}, conflict(err)
		}
	}
	status := api.StatusDisabled
	if next.Enabled {
		status = api.StatusEnabled
	}
	return done("module", next.RowVersion, status), nil
}

func (s Service) PublishPlan(ctx context.Context, cmd api.PublishPlan) (api.Receipt, error) {
	h := &publishCmd{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandPublish, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.PlanID}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.Receipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return res, nil
}

type publishCmd struct {
	s      Service
	cmd    api.PublishPlan
	cur    *plan.Plan
	replay bool
}

func (h *publishCmd) Prepare(ctx context.Context) error {
	on, err := h.s.moduleOn(ctx)
	if err != nil || !on {
		if err != nil {
			return err
		}
		return api.Reject(api.CodeDependencyNotSupported, "/", "Manufacturing is not enabled")
	}
	return nil
}

func (h *publishCmd) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.cur, h.replay = nil, false
	p, err := postgres.LoadPlan(ctx, exec, scopeOf(ctx), h.cmd.PlanID, postgres.ForUpdate)
	h.cur = p
	return err
}

func (h *publishCmd) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := enabled(ctx, exec); err != nil {
		return action.Outcome{}, err
	}
	hash := requestHash(h.cmd)
	sc := scopeOf(ctx)
	if h.cur == nil {
		if h.cmd.ExpectedVersion != 0 {
			return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
		}
		p, err := newPlan(h.cmd, hash, d)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = postgres.InsertPlan(ctx, exec, sc, p, string(d.Actor.Kind), d.Actor.ID, d.Epoch, kernel.Instant(h.s.Clock.Now())); err != nil {
			if postgres.UniqueViolation(err) {
				return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/plan_id", "The plan ID holds another plan")
			}
			return action.Outcome{}, err
		}
		return done(p.ID, p.Version, p.Status, planEvent(p)...), nil
	}
	if err := seen(d, h.cur.OwnerActor, h.cur.OwnerTeam, "/args/plan_id"); err != nil {
		return action.Outcome{}, err
	}
	if h.cmd.FactoryPartyID != "" && h.cmd.FactoryPartyID != h.cur.FactoryID {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/factory_party_id", "The factory does not change")
	}
	if h.cmd.ExpectedVersion == 0 {
		if h.cur.RequestHash == hash && h.cur.OwnerActor == d.Actor.ID && h.cur.Status == h.cmd.Target {
			h.replay = true
			return done(h.cur.ID, h.cur.Version, h.cur.Status), nil
		}
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/plan_id", "The plan ID holds another plan")
	}
	if h.cur.Version != h.cmd.ExpectedVersion {
		if h.cur.RequestHash == hash && h.cur.Status == h.cmd.Target {
			h.replay = true
			return done(h.cur.ID, h.cur.Version, h.cur.Status), nil
		}
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	from := h.cur.Version
	if h.cmd.Target == plan.StatusDraft {
		if err := fillBody(h.cur, h.cmd); err != nil {
			return action.Outcome{}, err
		}
		if err := h.cur.HoldDraft(); err != nil {
			return action.Outcome{}, err
		}
		h.cur.RequestHash = hash
		if err := postgres.ReplaceChildren(ctx, exec, sc, h.cur); err != nil {
			return action.Outcome{}, err
		}
		if err := postgres.SavePlan(ctx, exec, sc, h.cur, from); err != nil {
			return action.Outcome{}, conflict(err)
		}
		return done(h.cur.ID, h.cur.Version, h.cur.Status), nil
	}
	if h.cur.Status == plan.StatusDraft && (len(h.cmd.Operations) > 0 || len(h.cmd.Materials) > 0) {
		if err := fillBody(h.cur, h.cmd); err != nil {
			return action.Outcome{}, err
		}
		if err := postgres.ReplaceChildren(ctx, exec, sc, h.cur); err != nil {
			return action.Outcome{}, err
		}
	}
	if h.cmd.Note != "" && h.cur.Status == plan.StatusDraft {
		h.cur.Note = h.cmd.Note
	}
	if err := h.cur.Transition(h.cmd.Target); err != nil {
		return action.Outcome{}, err
	}
	h.cur.RequestHash = hash
	if err := postgres.SavePlan(ctx, exec, sc, h.cur, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	return done(h.cur.ID, h.cur.Version, h.cur.Status, planEvent(h.cur)...), nil
}

func newPlan(cmd api.PublishPlan, hash string, d authorization.Decision) (*plan.Plan, error) {
	p := &plan.Plan{ID: cmd.PlanID, FactoryID: cmd.FactoryPartyID, Note: cmd.Note, RequestHash: hash,
		OwnerActor: d.Actor.ID, OwnerTeam: d.Team}
	if err := fillBody(p, cmd); err != nil {
		return nil, err
	}
	if cmd.Target == plan.StatusDraft {
		if err := p.HoldDraft(); err != nil {
			return nil, err
		}
		return p, nil
	}
	if err := p.Transition(cmd.Target); err != nil {
		return nil, err
	}
	return p, nil
}

func fillBody(p *plan.Plan, cmd api.PublishPlan) error {
	if len(cmd.Operations) == 0 && len(cmd.Materials) == 0 {
		return nil
	}
	ops := make([]plan.Operation, 0, len(cmd.Operations))
	for _, in := range cmd.Operations {
		q, err := kernel.ParseDecimal(in.Quantity, kernel.QuantityFormat)
		if err != nil {
			return kernel.Reject(kernel.CodeSchemaInvalid, "/args/operations", "")
		}
		ops = append(ops, plan.Operation{ID: in.OperationID, Name: in.Name, Sequence: in.Sequence, SkuID: in.SkuID,
			SpecRevision: in.SpecRevision, Quantity: q, UnitID: in.UnitID, UnitRevision: in.UnitRevision})
	}
	mats := make([]plan.Material, 0, len(cmd.Materials))
	for _, in := range cmd.Materials {
		q, err := kernel.ParseDecimal(in.Quantity, kernel.QuantityFormat)
		if err != nil {
			return kernel.Reject(kernel.CodeSchemaInvalid, "/args/materials", "")
		}
		mats = append(mats, plan.Material{ID: in.MaterialID, Kind: in.Kind, SkuID: in.SkuID, Quantity: q,
			UnitID: in.UnitID, UnitRevision: in.UnitRevision, LossBasis: in.LossBasis})
	}
	if len(cmd.Operations) > 0 {
		p.Operations = ops
	}
	if len(cmd.Materials) > 0 {
		p.Materials = mats
	}
	return nil
}

func planEvent(p *plan.Plan) []action.Event {
	if p.Status == plan.StatusDraft {
		return nil
	}
	return []action.Event{{Type: api.EventPlanPublished, AggregateType: "CollaborationPlan", AggregateID: p.ID, AggregateVersion: p.Version,
		Fields: map[string]any{"plan_id": p.ID, "plan_version": p.Version, "status": p.Status,
			"material_responsibility_count": len(p.Materials), "operation_count": len(p.Operations)}}}
}

func (s Service) IssueSubcontract(ctx context.Context, cmd api.IssueSubcontract) (api.Receipt, error) {
	h := &issueCmd{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandIssue, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.SubcontractID}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.Receipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return res, nil
}

type issueCmd struct {
	s      Service
	cmd    api.IssueSubcontract
	demand *purapi.SubcontractDemand
	plan   *plan.Plan
	cur    *postgres.Subcontract
	replay bool
}

func (h *issueCmd) Prepare(ctx context.Context) error {
	on, err := h.s.moduleOn(ctx)
	if err != nil || !on {
		if err != nil {
			return err
		}
		return api.Reject(api.CodeDependencyNotSupported, "/", "Manufacturing is not enabled")
	}
	if h.cmd.CommitmentID == "" {
		return nil
	}
	if h.s.Demands == nil {
		return api.Reject(api.CodeDependencyNotSupported, "/args/commitment_id", "SubcontractDemand v1 is not wired")
	}
	d, err := h.s.Demands.ResolveSubcontractDemand(ctx, h.cmd.CommitmentID, h.cmd.OrderLineID)
	if err != nil {
		return err
	}
	h.demand = &d
	return nil
}

func (h *issueCmd) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.cur, h.replay, h.plan = nil, false, nil
	// The collaboration plan is MFG's own root for this command. Lock it before the subcontract
	// so two issues of the operation cannot both pass the quantity cap. PUR is not locked.
	p, err := postgres.LoadPlan(ctx, exec, scopeOf(ctx), h.cmd.PlanID, postgres.ForUpdate)
	if err != nil {
		return err
	}
	h.plan = p
	s, err := postgres.LoadSubcontract(ctx, exec, scopeOf(ctx), h.cmd.SubcontractID, postgres.ForUpdate)
	h.cur = s
	return err
}

func (h *issueCmd) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := enabled(ctx, exec); err != nil {
		return action.Outcome{}, err
	}
	hash := requestHash(h.cmd)
	if h.cur != nil {
		if err := seen(d, h.cur.Order.OwnerActor, h.cur.OwnerTeam, "/args/subcontract_id"); err != nil {
			return action.Outcome{}, err
		}
		if err := h.cur.Order.Answer(hash, d.Actor.ID); err != nil {
			return action.Outcome{}, err
		}
		h.replay = true
		return done(h.cur.Order.ID, h.cur.Order.Version, h.cur.Order.Status), nil
	}
	sc := scopeOf(ctx)
	p := h.plan
	if p == nil || !d.Sees(p.OwnerActor, p.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/plan_id", "The collaboration plan is not known")
	}
	switch p.Status {
	case plan.StatusPublished, plan.StatusInProgress, plan.StatusPaused:
	default:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/plan_id", "The plan is "+p.Status)
	}
	op, ok := p.OperationByID(h.cmd.OperationID)
	if !ok {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/operation_id", "The plan has no such operation")
	}
	qty, err := kernel.ParseDecimal(h.cmd.Quantity, kernel.QuantityFormat)
	if err != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/quantity", "")
	}
	if h.cmd.UnitID != op.UnitID || h.cmd.UnitRevision != op.UnitRevision {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/unit_id", "The unit is the operation's unit")
	}
	// The plan lock is held. open is the non-cancelled quantity already issued for this operation.
	// This issue is added to that sum. A cited demand caps the same sum, not a second ledger.
	open, err := postgres.SubcontractedQuantity(ctx, exec, sc, p.ID, op.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	if !subcontract.WithinQuantity(open, qty, op.Quantity) {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/quantity", "The quantity is above the operation")
	}
	orderRevision := int64(1)
	orderHash := digest("mfg-order", h.cmd.OrderID, h.cmd.OrderLineID)
	if h.demand != nil {
		if h.demand.Code != "" {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/commitment_id", "The commitment is "+h.demand.Status)
		}
		if h.demand.OrderID != h.cmd.OrderID {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "The demand belongs to order "+h.demand.OrderID)
		}
		dq, err := kernel.ParseDecimal(h.demand.OperationQuantity, kernel.QuantityFormat)
		if err != nil || !subcontract.WithinQuantity(open, qty, dq) {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/quantity", "The quantity is above the subcontract demand")
		}
		if h.cmd.UnitID != h.demand.UnitID || h.cmd.UnitRevision != h.demand.UnitRevision {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/unit_id", "The unit is the demand's unit")
		}
		if h.demand.SkuID != op.SkuID || h.demand.SpecRevision != op.SpecRevision {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/operation_id", "The operation is not the demand's specification")
		}
		if h.demand.OrderLineID != h.cmd.OrderLineID {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_line_id", "The demand is another order line")
		}
		if h.demand.OrderRevision > 0 {
			orderRevision = h.demand.OrderRevision
		}
		if len(h.demand.OrderHash) == 64 {
			orderHash = h.demand.OrderHash
		}
	}
	cur, err := kernel.NewCurrency(h.cmd.Currency, h.cmd.MinorUnits)
	if err != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/currency", "")
	}
	fee, err := kernel.ParseMinor(cur, h.cmd.ProcessingFeeMinor)
	if err != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/processing_fee_minor", "")
	}
	now := kernel.Instant(h.s.Clock.Now())
	subjectHash := digest("mfg-subject", h.cmd.SubcontractID, h.cmd.PlanID, h.cmd.OperationID, postgres.Text(qty), h.cmd.UnitID, fee.MinorString(), h.cmd.Currency, h.cmd.CommitmentID)
	row := &postgres.Subcontract{Order: subcontract.Order{ID: h.cmd.SubcontractID, Status: subcontract.StatusIssued, Version: 1,
		PlanID: h.cmd.PlanID, OperationID: h.cmd.OperationID, Quantity: qty, UnitID: h.cmd.UnitID, IssueHash: hash, RequestHash: hash,
		OwnerActor: d.Actor.ID}, UnitRevision: h.cmd.UnitRevision, Currency: h.cmd.Currency, MinorUnits: h.cmd.MinorUnits,
		FeeMinor: fee.MinorString(), FactoryID: p.FactoryID, SkuID: op.SkuID, SpecRevision: op.SpecRevision, CommitmentID: h.cmd.CommitmentID,
		OrderID: h.cmd.OrderID, OrderLineID: h.cmd.OrderLineID, OrderRevision: orderRevision, OrderHash: orderHash, SubjectHash: subjectHash,
		OwnerTeam: d.Team}
	if err = postgres.InsertSubcontract(ctx, exec, sc, row, d.Actor.ID, d.Epoch, now); err != nil {
		if postgres.UniqueViolation(err) {
			return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/subcontract_id", "The subcontract ID holds another subcontract")
		}
		return action.Outcome{}, err
	}
	ev := action.Event{Type: api.EventSubcontractIssued, AggregateType: "SubcontractOrder", AggregateID: row.Order.ID, AggregateVersion: row.Order.Version,
		Fields: map[string]any{"subcontract_id": row.Order.ID, "plan_id": row.Order.PlanID, "demand_ref": h.cmd.CommitmentID,
			"quantity": postgres.Text(qty), "unit_id": row.Order.UnitID, "processing_fee_minor": fee.MinorString(), "currency": h.cmd.Currency}}
	return done(row.Order.ID, row.Order.Version, row.Order.Status, ev), nil
}

func (s Service) RecordProgress(ctx context.Context, cmd api.RecordProgress) (api.Receipt, error) {
	h := &progressCmd{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandProgress, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.SubcontractID}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.Receipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return res, nil
}

type progressCmd struct {
	s      Service
	cmd    api.RecordProgress
	cur    *postgres.Subcontract
	replay bool
}

func (h *progressCmd) Prepare(ctx context.Context) error {
	on, err := h.s.moduleOn(ctx)
	if err != nil || !on {
		if err != nil {
			return err
		}
		return api.Reject(api.CodeDependencyNotSupported, "/", "Manufacturing is not enabled")
	}
	return nil
}

func (h *progressCmd) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.cur, h.replay = nil, false
	s, err := postgres.LoadSubcontract(ctx, exec, scopeOf(ctx), h.cmd.SubcontractID, postgres.ForUpdate)
	h.cur = s
	return err
}

func (h *progressCmd) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if err := enabled(ctx, exec); err != nil {
		return action.Outcome{}, err
	}
	if h.cur == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/subcontract_id", "")
	}
	if err := seen(d, h.cur.Order.OwnerActor, h.cur.OwnerTeam, "/args/subcontract_id"); err != nil {
		return action.Outcome{}, err
	}
	hash := requestHash(h.cmd)
	if h.cur.Order.Replay(h.cmd.ProgressID, hash, d.Actor.ID) {
		h.replay = true
		return done(h.cur.Order.ID, h.cur.Order.Version, h.cur.Order.Status), nil
	}
	if h.cur.Order.Version != h.cmd.ExpectedVersion {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	qty, err := kernel.ParseDecimal(h.cmd.Quantity, kernel.QuantityFormat)
	if err != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/quantity", "")
	}
	at, err := kernel.ParseTimestamp(h.cmd.ReportedAt)
	if err != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/reported_at", "")
	}
	from := h.cur.Order.Version
	claim := subcontract.Progress{ID: h.cmd.ProgressID, Operation: h.cmd.OperationID, Kind: h.cmd.ProgressKind, Quantity: qty,
		UnitID: h.cmd.UnitID, Hash: hash, ActorID: d.Actor.ID}
	if err = h.cur.Order.Advance(claim); err != nil {
		return action.Outcome{}, err
	}
	h.cur.Order.RequestHash = hash
	sc := scopeOf(ctx)
	if err = postgres.InsertProgress(ctx, exec, sc, h.cur.Order.ID, claim, string(d.Actor.Kind), d.Epoch, kernel.Instant(at)); err != nil {
		if postgres.UniqueViolation(err) {
			return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/progress_id", "The progress ID holds another claim")
		}
		return action.Outcome{}, err
	}
	if err = postgres.SaveSubcontract(ctx, exec, sc, h.cur, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	ev := action.Event{Type: api.EventProgressRecorded, AggregateType: "SubcontractOrder", AggregateID: h.cur.Order.ID, AggregateVersion: h.cur.Order.Version,
		Fields: map[string]any{"subcontract_id": h.cur.Order.ID, "operation_id": h.cmd.OperationID, "progress_kind": h.cmd.ProgressKind,
			"quantity": postgres.Text(qty), "unit_id": h.cmd.UnitID, "reported_at": kernel.FormatTimestamp(kernel.Instant(at))}}
	return done(h.cur.Order.ID, h.cur.Order.Version, h.cur.Order.Status, ev), nil
}

func (s Service) RecordMaterialHandover(ctx context.Context, cmd api.RecordMaterialHandover) (api.Receipt, error) {
	h := &handoverCmd{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandHandover, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.MovementID, Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.Receipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return res, nil
}

type handoverCmd struct {
	s      Service
	cmd    api.RecordMaterialHandover
	cur    *postgres.Subcontract
	plan   *plan.Plan
	replay bool
}

func (h *handoverCmd) Prepare(ctx context.Context) error {
	on, err := h.s.moduleOn(ctx)
	if err != nil || !on {
		if err != nil {
			return err
		}
		return api.Reject(api.CodeDependencyNotSupported, "/", "Manufacturing is not enabled")
	}
	var existing *postgres.Handover
	err = h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		s, err := postgres.LoadSubcontract(ctx, exec, scopeOf(ctx), h.cmd.SubcontractID, postgres.NoLock)
		if err != nil || s == nil {
			h.cur = s
			return err
		}
		h.cur = s
		h.plan, err = postgres.LoadPlan(ctx, exec, scopeOf(ctx), s.Order.PlanID, postgres.NoLock)
		if err != nil {
			return err
		}
		existing, err = postgres.LoadHandover(ctx, exec, scopeOf(ctx), h.cmd.MovementID)
		return err
	})
	if err != nil {
		return err
	}
	if h.cur == nil {
		return kernel.Reject(kernel.CodeNotFound, "/args/subcontract_id", "")
	}
	if h.cur.Order.Status == subcontract.StatusCancelled {
		return kernel.Reject(kernel.CodeInvalidState, "/args/subcontract_id", "The subcontract is cancelled")
	}
	if h.cmd.UnitID != h.cur.Order.UnitID || h.cmd.UnitRevision != h.cur.UnitRevision {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/unit_id", "The handover is in the subcontract unit")
	}
	hash := requestHash(h.cmd)
	if existing != nil {
		if existing.Hash == hash && existing.Kind == h.cmd.Kind {
			return nil
		}
		return api.Reject(api.CodeBusinessKeyConflict, "/args/movement_id", "The movement ID holds another handover")
	}
	if err = h.materialSKU(); err != nil {
		return err
	}
	if h.s.Handovers == nil {
		return api.Reject(api.CodeDependencyNotSupported, "/args/kind", "MaterialHandover v1 is not wired")
	}
	stock, err := h.s.Handovers.Enabled(ctx)
	if err != nil {
		return err
	}
	if !stock {
		return api.Reject(api.CodeDependencyNotSupported, "/args/kind", "Stock is not enabled")
	}
	_, err = h.s.Handovers.Record(ctx, api.HandoverRequest{IdempotencyKey: h.cmd.IdempotencyKey, MovementID: h.cmd.MovementID,
		CounterMovementID: h.cmd.CounterMovementID, Kind: h.cmd.Kind, SkuID: h.cmd.SkuID, LocationID: h.cmd.LocationID,
		BatchID: h.cmd.BatchID, CustodyKind: h.cmd.CustodyKind, ToLocationID: h.cmd.ToLocationID, ToBatchID: h.cmd.ToBatchID,
		ToCustodyKind: h.cmd.ToCustodyKind, Quantity: h.cmd.Quantity, UnitID: h.cmd.UnitID, UnitRevision: h.cmd.UnitRevision,
		Reason: h.cmd.Reason})
	return err
}

func (h *handoverCmd) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.replay = false
	s, err := postgres.LoadSubcontract(ctx, exec, scopeOf(ctx), h.cmd.SubcontractID, postgres.ForUpdate)
	h.cur = s
	return err
}

func (h *handoverCmd) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if h.cur == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/subcontract_id", "")
	}
	if err := seen(d, h.cur.Order.OwnerActor, h.cur.OwnerTeam, "/args/subcontract_id"); err != nil {
		return action.Outcome{}, err
	}
	hash := requestHash(h.cmd)
	existing, err := postgres.LoadHandover(ctx, exec, scopeOf(ctx), h.cmd.MovementID)
	if err != nil {
		return action.Outcome{}, err
	}
	if existing != nil {
		if existing.Hash == hash && existing.Kind == h.cmd.Kind {
			h.replay = true
			return done(h.cmd.MovementID, h.cur.Order.Version, h.cmd.Kind), nil
		}
		return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/movement_id", "The movement ID holds another handover")
	}
	if err = h.materialSKU(); err != nil {
		return action.Outcome{}, err
	}
	row := postgres.Handover{MovementID: h.cmd.MovementID, Kind: h.cmd.Kind, Hash: hash, OwnerActor: d.Actor.ID, OwnerTeam: d.Team}
	if err = postgres.InsertHandover(ctx, exec, scopeOf(ctx), h.cur.Order.ID, row, h.cmd.Kind, h.cmd.CounterMovementID, h.cmd.SkuID,
		h.cmd.LocationID, h.cmd.BatchID, h.cmd.CustodyKind, h.cmd.ToLocationID, h.cmd.ToBatchID, h.cmd.ToCustodyKind, h.cmd.Quantity,
		h.cmd.UnitID, h.cmd.UnitRevision, h.cmd.Reason, kernel.Instant(h.s.Clock.Now())); err != nil {
		if postgres.UniqueViolation(err) {
			return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/movement_id", "The movement ID holds another handover")
		}
		return action.Outcome{}, err
	}
	return done(h.cmd.MovementID, h.cur.Order.Version, h.cmd.Kind), nil
}

func (h *handoverCmd) materialSKU() error {
	if h.plan == nil || !h.plan.HasMaterialSKU(h.cmd.SkuID) {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/sku_id", "The sku is not a material on the plan")
	}
	return nil
}
